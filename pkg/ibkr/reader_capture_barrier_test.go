package ibkr

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const captureTestAccount = "DU1234567"

// newCaptureBarrierConnector is a ready Connector whose reader path runs the
// real handlers, with one quote line for tick frames.
func newCaptureBarrierConnector(t *testing.T) (*Connector, []byte) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { c.conn.rateLimiter.Stop() })
	c.conn.setStatus(StatusConnected)
	c.conn.resetOrderIDReadiness()
	setServerVersionReady(c.conn, minServerVerProtoBufPlaceOrder)
	c.ensureHandlersRegistered(c.conn)
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	const reqID, key = 4101, "SYNTH"
	c.subMu.Lock()
	c.reqIDMap[reqID] = key
	c.subscriptions[key] = &Subscription{Symbol: key, ReqID: reqID, LastTime: time.Now(), RejectCh: make(chan SubscriptionRejection, 1)}
	c.subMu.Unlock()
	return c, twsGatewayFrame(msgTickPrice, "1", strconv.Itoa(reqID), "1", "101.5", "0")
}

var inboundCaptures = map[string]func(*Connector, ConnectorSessionBinding){
	"account stream observation": func(c *Connector, b ConnectorSessionBinding) {
		c.CaptureAccountStreamObservationForSession(b, captureTestAccount)
	},
	"settled cash schedules": func(c *Connector, b ConnectorSessionBinding) {
		c.CaptureSettledCashSchedulesForSession(b, captureTestAccount)
	},
	"display": func(c *Connector, _ ConnectorSessionBinding) { c.CaptureDisplay() },
}

// A capture that wants the inbound lease and the evidence write side must not
// hold the lease while it waits for evidence: every inbound frame needs that
// lease, ticks included, so the reader stopped for as long as the evidence
// holder ran. The account captures run on every account summary. The
// tick-led reader stall at the 2026-11-19 US open fits this, by inference: the
// log names neither the frames the reader waited in nor the evidence holder.
// Order, error, notice and account-summary frames still wait out a long
// commit by design.
func TestTickFrameIsNotHeldBehindCaptureWaitingForEvidence(t *testing.T) {
	for name, capture := range inboundCaptures {
		t.Run(name, func(t *testing.T) {
			c, tick := newCaptureBarrierConnector(t)
			evidence, ok := c.CaptureBrokerEvidence()
			if !ok {
				t.Fatal("ready connector gave no evidence binding")
			}
			session, _ := c.CaptureSession()

			// An evidence holder that takes a while: a derived-state commit.
			const hold = 1500 * time.Millisecond
			committing, released := make(chan struct{}), make(chan time.Time, 1)
			go func() {
				c.WithStableBrokerEvidence(evidence, func() bool {
					close(committing)
					time.Sleep(hold)
					released <- time.Now()
					return true
				})
			}()
			<-committing

			captured := make(chan time.Time, 1)
			go func() {
				capture(c, session)
				captured <- time.Now()
			}()
			time.Sleep(100 * time.Millisecond) // let the capture reach its locks

			handled := make(chan time.Duration, 1)
			go func() {
				began := time.Now()
				c.conn.processMessage(tick)
				handled <- time.Since(began)
			}()
			select {
			case took := <-handled:
				if took > 250*time.Millisecond {
					t.Fatalf("tick frame took %s behind a capture waiting for evidence", took)
				}
			case <-time.After(hold):
				t.Fatalf("tick frame still blocked after %s: a capture holds the inbound lease while it waits for evidence", hold)
			}
			if bid := c.MarketDataSnapshot()["SYNTH"].Bid; bid != 101.5 {
				t.Fatalf("tick not applied: bid %v", bid)
			}

			// The capture still linearizes against the evidence holder.
			if at, done := <-captured, <-released; at.Before(done) {
				t.Fatalf("capture returned %s before the evidence holder released", done.Sub(at))
			}
		})
	}
}

// The capture must finish while evidence readers overlap without a gap and
// the reader is busy with frames: it may not wait for a moment when nobody
// holds either lock.
func TestInboundCaptureFinishesUnderOverlappingEvidenceReadersAndTickFlood(t *testing.T) {
	for name, capture := range inboundCaptures {
		t.Run(name, func(t *testing.T) {
			c, tick := newCaptureBarrierConnector(t)
			session, _ := c.CaptureSession()
			ctx, stop := context.WithCancel(t.Context())
			var load sync.WaitGroup
			for range 32 {
				load.Go(func() {
					for ctx.Err() == nil {
						c.CaptureBrokerEvidence()
					}
				})
			}
			load.Go(func() {
				for ctx.Err() == nil {
					c.conn.processMessage(tick)
				}
			})
			defer func() { stop(); load.Wait() }()

			for range 20 {
				done := make(chan struct{})
				go func() { capture(c, session); close(done) }()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("capture did not finish under overlapping evidence readers and a tick flood")
				}
			}
		})
	}
}

func TestLongEvidenceCommitIsNamedInTheLog(t *testing.T) {
	logs := captureConnectorLogs(t)
	c, _ := newCaptureBarrierConnector(t)
	evidence, ok := c.CaptureBrokerEvidence()
	if !ok {
		t.Fatal("ready connector gave no evidence binding")
	}
	c.WithStableBrokerEvidence(evidence, func() bool { return true })
	if lines := logLines(logs, "held broker evidence"); len(lines) != 0 {
		t.Fatalf("a quick commit was reported: %v", lines)
	}
	c.WithStableBrokerEvidence(evidence, func() bool { time.Sleep(evidenceHoldWarnAfter + 50*time.Millisecond); return true })
	lines := logLines(logs, "held broker evidence")
	if len(lines) != 1 || !strings.Contains(lines[0], "TestLongEvidenceCommitIsNamedInTheLog") {
		t.Fatalf("long commit not named once with its caller: %v", lines)
	}
}
