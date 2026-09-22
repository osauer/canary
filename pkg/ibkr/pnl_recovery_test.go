package ibkr

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"
)

func pnlRecoveryFixture(t *testing.T) (*Connector, *Connection, *safeBuffer, *time.Time) {
	t.Helper()
	conn, out := newReadyWireTestConnection(t)
	conn.observeNextValidOrderID(100)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.conn, c.running, c.ready = conn, true, true
	conn.evidenceBarrier, conn.publicationBarrier = &c.evidenceBarrier, &c.publicationBarrier
	now := time.Now().UTC()
	c.pnlResubNow = func() time.Time { return now }
	t.Cleanup(c.stopBackendEpisodeTimer)
	if err := c.SubscribeAccountPnL("TEST"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribePositionDailyPnL("TEST", 123); err != nil {
		t.Fatal(err)
	}
	return c, conn, out, &now
}

func pnlRequestID(c *Connector) int {
	c.pnl.mu.RLock()
	defer c.pnl.mu.RUnlock()
	return c.pnl.accountReqID
}

func TestPnLRebuildAfterBackendRestore(t *testing.T) {
	for _, code := range []int{1101, 1102} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			c, conn, out, now := pnlRecoveryFixture(t)
			c.acctUpdatesAccount, c.acctUpdatesLastAt = "TEST", *now
			oldID := pnlRequestID(c)
			binding, ok := c.CaptureSession()
			if !ok {
				t.Fatal("no session")
			}
			c.handleBackendConnectivityNotice(binding, &systemNotification{code: 1100, timestamp: *now})
			if !c.BackendLink().Down {
				t.Fatal("loss not latched")
			}
			post := c.handleBackendConnectivityNotice(binding, &systemNotification{code: code, timestamp: now.Add(5 * time.Second)})
			if c.BackendLink().Down {
				t.Fatal("restore not latched")
			}
			if code == 1101 {
				if post == nil {
					t.Fatal("lost subscriptions were not scheduled for replay")
				}
				// Exercise the same post-action worker synchronously to keep this witness deterministic.
				c.recoverFromBackendDataLoss(binding)
			} else {
				if post != nil {
					t.Fatal("maintained subscriptions replayed immediately")
				}
				for i := 1; i <= 5; i++ {
					*now = now.Add(15 * time.Second)
					if c.MaybeResubscribeStaleDailyPnL(true) {
						t.Fatal("rebuild before90s")
					}
				}
				*now = now.Add(15 * time.Second)
				if !c.MaybeResubscribeStaleDailyPnL(true) {
					t.Fatal("silent maintained subscription never rebuilt")
				}
				if c.MaybeResubscribeStaleDailyPnL(true) {
					t.Fatal("same-tick rebuild duplicated")
				}
			}
			newID := pnlRequestID(c)
			if newID == 0 || newID == oldID {
				t.Fatal("no fresh subscription identity")
			}
			c.handlePnL([]string{"94", strconv.Itoa(oldID), "42", "0", "0"})
			if _, ok := c.AccountDailyPnL(); ok {
				t.Fatal("old frame accepted")
			}
			c.handlePnL([]string{"94", strconv.Itoa(newID), "42", "0", "0"})
			snap, ok := c.AccountDailyPnL()
			if !ok || snap.DailyPnLStatus != DailyPnLFrameAvailable {
				t.Fatal("current valid frame absent")
			}
			frames := decodeOutboundFrames(t, conn, out.Bytes())
			n := 0
			for _, f := range frames {
				if f[0] == strconv.Itoa(reqPnL) {
					n++
				}
			}
			if n != 2 {
				t.Fatalf("account subscription requests=%d, want2", n)
			}
			if c.ActiveDailyPnLSubscriptions() != 1 {
				t.Fatal("position subscription lost")
			}
		})
	}
}

func TestRetiredBackendReplayCannotRebuildSuccessorPnL(t *testing.T) {
	c, conn, out, _ := pnlRecoveryFixture(t)
	origin, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	conn.brokerSessionEpoch.Add(1)
	before, id := out.Len(), pnlRequestID(c)
	c.recoverFromBackendDataLoss(origin)
	if out.Len() != before || pnlRequestID(c) != id {
		t.Fatal("retired1101 replay mutated successor subscriptions")
	}
}

func TestQueuedPnLRepairCannotCrossSocketGeneration(t *testing.T) {
	c, conn, out, _ := pnlRecoveryFixture(t)
	origin, _ := c.CaptureSession()
	before := out.Len()
	conn.pauseTransport()
	t.Cleanup(conn.resumeTransport)
	dispatches := conn.rateLimiter.GetMetrics().TotalRequests
	done := make(chan struct{})
	go func() { c.forceResubscribeDailyPnLForSession(origin); close(done) }()
	waitForProtectedDispatch(t, conn, dispatches)
	conn.resetOrderIDReadiness()
	conn.observeNextValidOrderID(100)
	successor, _ := c.CaptureSession()
	c.mutatePnLForSession(successor, func() {
		c.resetPnLSessionLocked(successor)
		c.pnl.accountReqID, c.pnl.accountAcct = 100, "TEST"
	})
	nextSocket := &safeBuffer{}
	conn.writer = bufio.NewWriter(nextSocket)
	conn.resumeTransport()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old repair did not finish")
	}
	if out.Len() != before || nextSocket.Len() != 0 || pnlRequestID(c) != 100 {
		t.Fatal("queued old repair touched successor")
	}
	c.receivePnLForSession(origin, func() { c.handlePnL([]string{"94", "100", "42", "0", "0"}) })
	if _, ok := c.AccountDailyPnL(); ok {
		t.Fatal("retired receipt with reused id accepted")
	}
	c.receivePnLForSession(successor, func() { c.handlePnL([]string{"94", "100", "42", "0", "0"}) })
	if _, ok := c.AccountDailyPnL(); !ok {
		t.Fatal("current receipt rejected")
	}
}

type rejectedPnLWriter struct{}

func (rejectedPnLWriter) Write([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic transport failure")
}

func TestPnLFailedRequestsLeaveRetryableSlots(t *testing.T) {
	c, conn, _, _ := pnlRecoveryFixture(t)
	conn.writer = bufio.NewWriter(rejectedPnLWriter{})
	c.forceResubscribeDailyPnL()
	if pnlRequestID(c) != 0 || c.ActiveDailyPnLSubscriptions() != 0 {
		t.Fatal("failed request retained active identity")
	}
	conn.writer = bufio.NewWriter(&safeBuffer{})
	if err := c.SubscribeAccountPnL("TEST"); err != nil {
		t.Fatal(err)
	}
	if err := c.SubscribePositionDailyPnL("TEST", 123); err != nil {
		t.Fatal(err)
	}
	if pnlRequestID(c) == 0 || c.ActiveDailyPnLSubscriptions() != 1 {
		t.Fatal("retry did not recover subscriptions")
	}
}

func TestPnLSilentRepairWaitsForBackendAndDoesNotReplaceNewerRepair(t *testing.T) {
	c, _, out, now := pnlRecoveryFixture(t)
	origin, _ := c.CaptureSession()
	oldID := pnlRequestID(c)
	c.setBackendConnectivityDown(true, *now)
	*now = now.Add(2 * time.Minute)
	before := out.Len()
	if c.MaybeResubscribeStaleDailyPnL(true) || out.Len() != before {
		t.Fatal("repair attempted while backend unavailable")
	}
	c.setBackendConnectivityDown(false, *now)
	c.forceResubscribeDailyPnLForSession(origin)
	before = out.Len()
	if c.rebuildPnLForSession(origin, oldID) || out.Len() != before {
		t.Fatal("stale monitor decision replaced newer repair")
	}
}

func TestPnLReceiptDisplayAndPublicationDoNotDeadlock(t *testing.T) {
	c, conn, _, _ := pnlRecoveryFixture(t)
	c.registerHandlers(conn)
	origin, _ := c.CaptureSession()
	msg := conn.encodeMsg(msgPnL, pnlRequestID(c), 42, 0, 0)
	// encodeMsg includes the wire frame length; dispatch consumes the payload.
	payload := msg
	if len(msg) > 4 && int(binary.BigEndian.Uint32(msg[:4])) == len(msg)-4 {
		payload = msg[4:]
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 500 {
			conn.processMessageAtEpoch(payload, origin.epoch)
		}
	})
	wg.Go(func() {
		for range 500 {
			c.captureDisplayAccount(origin)
		}
	})
	wg.Go(func() {
		for range 500 {
			c.WithBrokerEvidenceMutation(func() {})
		}
	})
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("P&L receipt/display/publication lock inversion")
	}
	if _, ok := c.AccountDailyPnL(); !ok {
		t.Fatal("receipt dispatch did not populate P&L")
	}
}

func TestPnLCacheCommitExcludesEpochRetirement(t *testing.T) {
	c, conn, _, _ := pnlRecoveryFixture(t)
	origin, _ := c.CaptureSession()
	c.pnl.mu.Lock()
	mutation := make(chan bool, 1)
	go func() { mutation <- c.mutatePnLForSession(origin, func() { c.pnl.accountReqID = 200 }) }()
	deadline := time.Now().Add(time.Second)
	exclusive := false
	for time.Now().Before(deadline) {
		if !c.evidenceBarrier.TryRLock() {
			exclusive = true
			break
		}
		c.evidenceBarrier.RUnlock()
		runtime.Gosched()
	}
	if !exclusive {
		c.pnl.mu.Unlock()
		t.Fatal("cache commit does not exclude epoch retirement")
	}
	retired := make(chan struct{})
	go func() { conn.resetOrderIDReadiness(); close(retired) }()
	c.pnl.mu.Unlock()
	if !<-mutation {
		t.Fatal("current commit refused")
	}
	<-retired
	if _, ok := c.AccountDailyPnL(); ok {
		t.Fatal("retired cache served as current")
	}
	if c.mutatePnLForSession(origin, func() { t.Error("retired mutation ran") }) {
		t.Fatal("retired commit admitted")
	}
}

func BenchmarkPnLCachedRead(b *testing.B) {
	c := NewConnector(&ConnectorConfig{})
	b.Cleanup(c.conn.rateLimiter.Stop)
	c.ready = true
	c.conn.status = StatusConnected
	origin, _ := c.CaptureSession()
	value := 0.0
	c.pnl.session, c.pnl.accountReqID = origin, 100
	c.pnl.account = AccountDailyPnL{AsOf: time.Now(), DailyPnL: &value, DailyPnLStatus: DailyPnLFrameAvailable}
	b.ReportAllocs()
	for b.Loop() {
		c.AccountDailyPnL()
	}
}
