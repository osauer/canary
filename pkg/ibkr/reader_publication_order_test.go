package ibkr

import (
	"sync"
	"testing"
	"time"
)

// The lock order is publication, then the inbound lease, then evidence
// (docs/docs/internals/display-stream.md). On 2026-11-17 the live daemon hung
// for four and a half hours because the reader took them the other way round
// for order frames: it held the inbound lease while its order-status handler
// asked for the publication read side, an account read held that read side
// while it waited for the inbound lease, and the reconnect queued for the
// write side between them, which turns away every new reader.

func readerOrderFixture(t *testing.T) (*Connector, *Connection, ConnectorSessionBinding) {
	t.Helper()
	c, conn, _, _ := pnlRecoveryFixture(t)
	c.registerHandlers(conn)
	origin, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	return c, conn, origin
}

func readerOrderStatusFrame() []byte {
	body := protoAppendInt32(nil, 1, 40)
	body = protoAppendString(body, 2, "Submitted")
	body = protoAppendString(body, 3, "0")
	body = protoAppendString(body, 4, "100")
	body = protoAppendInt64(body, 6, 700000)
	body = protoAppendInt32(body, 9, 41)
	return encodeProtoCallbackFrameForTest(protoOrderStatusMsgID, body)
}

func readerPublicationFrames() map[string][]byte {
	return map[string][]byte{
		"order status":       readerOrderStatusFrame(),
		"error":              wireTErrorFrame(9, 200, "No security definition has been found for the request"),
		"system notice":      inboundNotice(9, 2104, "Market data farm connection is OK"),
		"option computation": twsGatewayFrame(msgTickOptionComputation, "9", "13", "0", "0.2", "0.5", "1", "0", "0.1", "0.1", "-0.1", "100"),
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The incident, replayed in order: the reader is mid-frame on an order
// status, an account read arrives, then the reconnect's publication.
func TestOrderStatusFrameAccountReadAndPublicationDoNotDeadlock(t *testing.T) {
	c, conn, origin := readerOrderFixture(t)
	inFrame, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(conn.tapInboundFrames(func([]string, uint64) {
		once.Do(func() { close(inFrame); <-resume })
	}))

	var wg sync.WaitGroup
	wg.Go(func() { conn.processMessageAtEpoch(readerOrderStatusFrame(), origin.epoch) })
	<-inFrame
	wg.Go(func() { c.CaptureAccountStreamObservationForSession(origin, "TEST") })
	waitUntil(t, "the account read queues for the inbound lease", func() bool {
		if conn.inboundEpochMu.TryRLock() {
			conn.inboundEpochMu.RUnlock()
			return false
		}
		return true
	})
	wg.Go(func() { c.WithBrokerEvidenceMutation(func() {}) })
	waitUntil(t, "the publication queues for the write side", func() bool {
		if c.publicationBarrier.TryRLock() {
			c.publicationBarrier.RUnlock()
			return false
		}
		return true
	})
	close(resume)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reader, account read and publication deadlocked: the reader asked for publication while holding the inbound lease")
	}
}

// While a publication is in progress, a frame whose handler needs the
// publication read side must wait for it before taking the inbound lease,
// current or stale.
func TestReaderWaitsForPublicationBeforeInboundLease(t *testing.T) {
	for name, frame := range readerPublicationFrames() {
		for _, age := range []string{"current", "stale"} {
			t.Run(name+"/"+age, func(t *testing.T) {
				c, conn, origin := readerOrderFixture(t)
				epoch := origin.epoch
				if age == "stale" {
					epoch--
				}
				c.publicationBarrier.Lock()
				done := make(chan struct{})
				go func() { conn.processMessageAtEpoch(frame, epoch); close(done) }()
				deadline := time.Now().Add(150 * time.Millisecond)
				for time.Now().Before(deadline) {
					if !conn.inboundEpochMu.TryLock() {
						c.publicationBarrier.Unlock()
						<-done
						t.Fatal("reader holds the inbound lease while it waits for publication")
					}
					conn.inboundEpochMu.Unlock()
					time.Sleep(time.Millisecond)
				}
				select {
				case <-done:
					// A stale option computation is dropped unhandled.
					if age == "current" {
						c.publicationBarrier.Unlock()
						t.Fatal("frame was handled during a publication")
					}
				default:
				}
				c.publicationBarrier.Unlock()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("frame did not finish after the publication")
				}
			})
		}
	}
}
