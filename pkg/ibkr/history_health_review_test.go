package ibkr

import (
	"bufio"
	"errors"
	"sync"
	"testing"
	"time"
)

type historyReviewWriter func([]byte) (int, error)

func (write historyReviewWriter) Write(p []byte) (int, error) { return write(p) }

func TestHistoricalQueuedOldSessionAnswerCannotClearCurrentStall(t *testing.T) {
	c, gateway := newRetiredIdentityConnector(t, "", "756733", 0)
	conn := gateway.conn
	oldEpoch := conn.BrokerSessionEpoch()
	queued := make(chan struct{})
	resume := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(resume) }) })
	var once sync.Once
	conn.writer = bufio.NewWriter(historyReviewWriter(func(p []byte) (int, error) {
		n, err := gateway.Write(p)
		once.Do(func() { close(queued) })
		<-resume
		return n, err
	}))

	// Pause the fake writer after the real request has received its bars,
	// before its fetch goroutine can consume the completed result.
	// Session teardown cannot fail this request: completion already removed
	// it from historicalReqs and queued the successful result.
	result := make(chan error, 1)
	go func() {
		_, err := c.fetchHistoricalWithContract(t.Context(), "SPY", Contract{ConID: 756733, Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"}, 365, 30*time.Second, "TRADES")
		result <- err
	}()
	select {
	case <-queued:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic gateway did not queue its history answer")
	}
	c.historicalMu.Lock()
	pending := len(c.historicalReqs)
	c.historicalMu.Unlock()
	if pending != 0 {
		t.Fatalf("completed history request still pending: %d", pending)
	}
	c.onConnectionLost(conn)
	conn.resetOrderIDReadiness()
	c.onConnectionEstablished(conn)
	newEpoch := conn.BrokerSessionEpoch()
	if newEpoch == oldEpoch {
		t.Fatal("session did not advance")
	}

	// Model the successor's independently established stall without making
	// the test spend seventy-five wall-clock seconds on unanswered reads.
	now := time.Now()
	since := now.Add(-time.Second)
	session := HistoricalSessionBinding{connector: c, connection: conn, epoch: newEpoch}
	c.historicalStall.mu.Lock()
	c.historicalStall.session = session
	c.historicalStall.unanswered = 3
	c.historicalStall.firstSent = now.Add(-75 * time.Second)
	c.historicalStall.since, c.historicalStall.probeAt = since, since
	c.historicalStall.mu.Unlock()
	release.Do(func() { close(resume) })
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("original queued result was replaced: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("original history reader did not finish")
	}
	if got, stalled := c.HistoricalServiceStalled(); !stalled || !got.Equal(since) {
		t.Errorf("old-session answer cleared the successor's stall: since=%s stalled=%t", got, stalled)
	}
	if err := c.admitHistoricalRequest(session, now); !errors.Is(err, ErrHistoricalServiceStalled) {
		t.Errorf("old-session answer reopened successor history admission: %v", err)
	}
	if at, _ := c.answers.read(now); !at.IsZero() {
		t.Errorf("old-session answer was published as current answer-path evidence: %s", at)
	}
}

func TestHistoricalStallRejectsRetiredSessionOutcomes(t *testing.T) {
	for _, replaceConnection := range []bool{false, true} {
		name := "new socket epoch"
		if replaceConnection {
			name = "new connection with same epoch"
		}
		t.Run(name, func(t *testing.T) {
			c, old := newHistoricalStallFixture()
			now := time.Now()
			for i := 1; i <= 3; i++ {
				c.noteHistoricalTimeout(old, 25*time.Second, now.Add(time.Duration(i)*25*time.Second))
			}
			if _, stalled := c.HistoricalServiceStalled(); !stalled {
				t.Fatal("initial session did not establish a stall")
			}
			if replaceConnection {
				c.conn = &Connection{}
				c.conn.brokerSessionEpoch.Store(old.epoch)
			} else {
				c.conn.resetOrderIDReadiness()
			}
			current := HistoricalSessionBinding{connector: c, connection: c.conn, epoch: c.conn.BrokerSessionEpoch()}
			now = now.Add(80 * time.Second)
			if _, stalled := c.HistoricalServiceStalled(); stalled {
				t.Fatal("new session inherited the old stall")
			}
			if err := c.admitHistoricalRequest(current, now); err != nil {
				t.Fatalf("new session must admit a fresh request: %v", err)
			}
			beforeAnswer, beforeTimeouts := c.answers.read(now)
			for range 3 {
				c.noteHistoricalTimeout(old, 25*time.Second, now.Add(75*time.Second))
			}
			c.noteHistoricalAnswer(old, now.Add(76*time.Second))
			if afterAnswer, afterTimeouts := c.answers.read(now); !afterAnswer.Equal(beforeAnswer) || afterTimeouts != beforeTimeouts {
				t.Fatalf("retired outcomes changed answer-path diagnostics: answer=%s timeouts=%d; before=%s/%d", afterAnswer, afterTimeouts, beforeAnswer, beforeTimeouts)
			}
			if err := c.admitHistoricalRequest(old, now); !errors.Is(err, ErrIBKRUnavailable) {
				t.Fatalf("retired admission must be refused without disturbing the new session: %v", err)
			}
			for i := 1; i <= 3; i++ {
				c.noteHistoricalTimeout(current, 25*time.Second, now.Add(time.Duration(i)*25*time.Second))
			}
			since, stalled := c.HistoricalServiceStalled()
			if !stalled {
				t.Fatal("new session must still detect its own sustained silence")
			}
			c.noteHistoricalAnswer(old, now.Add(76*time.Second))
			c.noteHistoricalTimeout(old, 25*time.Second, now.Add(77*time.Second))
			if got, stalled := c.HistoricalServiceStalled(); !stalled || !got.Equal(since) {
				t.Fatalf("retired outcome changed the new stall: since=%s stalled=%t", got, stalled)
			}
			c.noteHistoricalAnswer(current, now.Add(78*time.Second))
			if _, stalled := c.HistoricalServiceStalled(); stalled {
				t.Fatal("current-session answer must recover the stall")
			}
		})
	}
}
