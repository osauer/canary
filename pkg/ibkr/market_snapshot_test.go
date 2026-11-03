package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func snapshotFields(msgID, reqID, tick int, value string) []string {
	return []string{strconv.Itoa(msgID), "6", strconv.Itoa(reqID), strconv.Itoa(tick), value, "0", "1"}
}

// The snapshot handlers accept only frames for the session's request id,
// record bid, ask and last from positive prices, implied volatility from
// generic tick 106, the data type, and the end marker once.
func TestSnapshotHandlersKeyOnTheSessionRequestID(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	s := newSnapshotSession("SYNX")
	// Before the request id is known every frame is dropped.
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, "10"))
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 106, "0.3"))
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "7", "3"})
	c.handleSnapshotEnd(s, []string{"57", "1", "7"})
	if s.snap.Bid != nil || s.snap.IV != nil || s.dataType != 0 || len(s.gotEndCh) != 0 {
		t.Fatalf("frames accepted before the request id was known: %+v", s.snap)
	}
	s.targetReqID.Store(7)
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, "10.5"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 2, "10.7"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 4, "10.6"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 9, "10.1"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 8, 1, "99"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, "0"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, "-1"))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, ""))
	c.handleSnapshotTickPrice(s, snapshotFields(msgTickPrice, 7, 1, "x"))
	c.handleSnapshotTickPrice(s, []string{"1", "6", "7", "x", "5"})
	c.handleSnapshotTickPrice(s, []string{"1", "6", "x", "1", "5"})
	c.handleSnapshotTickPrice(s, []string{"1", "6", "7"})
	if s.snap.Bid == nil || *s.snap.Bid != 10.5 || s.snap.Ask == nil || *s.snap.Ask != 10.7 || s.snap.Last == nil || *s.snap.Last != 10.6 {
		t.Fatalf("snapshot = bid %v ask %v last %v", s.snap.Bid, s.snap.Ask, s.snap.Last)
	}
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 24, "0.3"))
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 8, 106, "0.3"))
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 106, "0"))
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 106, "x"))
	c.handleSnapshotTickGeneric(s, []string{"45", "6", "7", "x", "1"})
	c.handleSnapshotTickGeneric(s, []string{"45", "6", "x", "106", "1"})
	c.handleSnapshotTickGeneric(s, []string{"45", "6", "7"})
	if s.snap.IV != nil || s.snap.IVStatus != "unavailable" {
		t.Fatalf("iv set by a non-106 or foreign tick: %v %s", s.snap.IV, s.snap.IVStatus)
	}
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 106, "28"))
	if s.snap.IV == nil || *s.snap.IV != 0.28 || s.snap.IVStatus != "real" {
		t.Fatalf("percent iv: %v %s", s.snap.IV, s.snap.IVStatus)
	}
	c.handleSnapshotTickGeneric(s, snapshotFields(msgTickGeneric, 7, 106, "0.31"))
	if *s.snap.IV != 0.31 {
		t.Fatalf("fraction iv: %v", *s.snap.IV)
	}
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "8", "3"})
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "7", "x"})
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "x", "3"})
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "7"})
	if s.dataType != 0 {
		t.Fatalf("data type from a foreign or malformed frame: %d", s.dataType)
	}
	c.handleSnapshotMarketDataType(s, []string{"58", "1", "7", "3"})
	if s.dataType != 3 {
		t.Fatalf("data type = %d", s.dataType)
	}
	c.handleSnapshotEnd(s, []string{"57", "1", "8"})
	c.handleSnapshotEnd(s, []string{"57", "1", "x"})
	c.handleSnapshotEnd(s, []string{"57", "1"})
	if len(s.gotEndCh) != 0 {
		t.Fatal("end accepted for another request")
	}
	c.handleSnapshotEnd(s, []string{"57", "1", "7"})
	c.handleSnapshotEnd(s, []string{"57", "1", "7"})
	if len(s.gotEndCh) != 1 {
		t.Fatalf("end signals = %d, want one (duplicates must not block)", len(s.gotEndCh))
	}
}

func newSnapshotFixture(t *testing.T) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	conn, c, out, _, _ := newQueuedInstructionReconnectFixture(t)
	c.contractCache["SYNX"] = ContractDetailsLite{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD", LocalSymbol: "SYNX", TradingClass: "NMS"}
	return c, conn, out
}

// One snapshot is one reqMktData with snapshot=1 and no generic ticks (the
// gateway refuses generic ticks on a snapshot with error 321), answered by
// ticks and tickSnapshotEnd for that request id; the handlers are gone and
// the slot released afterwards.
func TestFetchMarketSnapshotCompletesOnTheEndMarker(t *testing.T) {
	c, conn, out := newSnapshotFixture(t)
	type result struct {
		snap *MarketSnapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, err := c.FetchMarketSnapshot(context.Background(), "synx", 3*time.Second)
		done <- result{snap, err}
	}()
	reqID := waitForHandlerReqID(t, conn, msgTickSnapshotEnd)
	deadline := time.Now().Add(3 * time.Second)
	for len(outboundFramesByID(t, conn, out, reqMktData)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no market data request was sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	id := strconv.Itoa(reqID)
	var r result
	// The caller stores the request id microseconds after the send returns;
	// ticks are repeated until the end marker is taken.
	for round := 0; ; round++ {
		conn.processMessage(twsGatewayFrame(msgTickPrice, "6", id, "1", "10.5", "0", "1"))
		conn.processMessage(twsGatewayFrame(msgTickPrice, "6", id, "2", "10.7", "0", "1"))
		conn.processMessage(twsGatewayFrame(msgTickPrice, "6", id, "4", "10.6", "0", "1"))
		conn.processMessage(twsGatewayFrame(msgMarketDataType, "1", id, "1"))
		if round >= 3 {
			conn.processMessage(twsGatewayFrame(msgTickSnapshotEnd, "1", id))
		}
		select {
		case r = <-done:
		case <-time.After(5 * time.Millisecond):
			if round > 400 {
				t.Fatal("snapshot never completed")
			}
			continue
		}
		break
	}
	if r.err != nil || r.snap == nil || r.snap.Bid == nil || *r.snap.Bid != 10.5 || *r.snap.Ask != 10.7 || *r.snap.Last != 10.6 || r.snap.MarketDataType != 1 || r.snap.IV != nil || r.snap.IVStatus != "unavailable" || r.snap.AsOf.IsZero() || r.snap.Symbol != "SYNX" {
		t.Fatalf("snapshot = %+v err=%v", r.snap, r.err)
	}
	requests := outboundFramesByID(t, conn, out, reqMktData)
	if len(requests) != 1 {
		t.Fatalf("market data requests = %d", len(requests))
	}
	assertFields(t, requests[0], []fieldAssertion{{2, id, "reqId"}, {3, "4242", "conId"}, {4, "SYNX", "symbol"}, {5, "STK", "secType"}, {16, "", "genericTicks"}, {17, "1", "snapshot"}, {18, "0", "regulatorySnapshot"}})
	cancels := outboundFramesByID(t, conn, out, cancelMktData)
	if len(cancels) != 1 || cancels[0][2] != id {
		t.Fatalf("cancel frames = %q, want one for %s", cancels, id)
	}
	conn.handlersMu.RLock()
	remaining := len(conn.msgHandlers[msgTickSnapshotEnd]) + len(conn.msgHandlers[msgTickPrice])
	conn.handlersMu.RUnlock()
	if remaining != 0 {
		t.Fatalf("%d snapshot handlers left registered", remaining)
	}
}

// A snapshot the gateway never completes ends at the caller's timeout with
// context.DeadlineExceeded and a cancel on the wire; a caller cancellation
// ends it with the caller's error.
func TestFetchMarketSnapshotTimesOutAndCancelsOnTheWire(t *testing.T) {
	c, conn, out := newSnapshotFixture(t)
	_, err := c.FetchMarketSnapshot(context.Background(), "SYNX", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout err = %v", err)
	}
	if n := len(outboundFramesByID(t, conn, out, cancelMktData)); n != 1 {
		t.Fatalf("cancel frames after timeout = %d", n)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = c.FetchMarketSnapshot(ctx, "SYNX", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel err = %v", err)
	}
	if n := len(outboundFramesByID(t, conn, out, cancelMktData)); n != 2 {
		t.Fatalf("cancel frames after cancellation = %d", n)
	}
	if _, err := c.FetchMarketSnapshot(context.Background(), "  ", time.Second); err == nil {
		t.Fatal("empty symbol accepted")
	}
	for range inactiveConfirmations {
		if _, post := c.registerInactiveCandidatePostAction("DEAD", "No security definition has been found"); post != nil {
			post()
		}
	}
	if _, err := c.FetchMarketSnapshot(context.Background(), "dead", time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive symbol err = %v", err)
	}
	conn.setStatus(StatusDisconnected)
	if _, err := c.FetchMarketSnapshot(context.Background(), "SYNX", time.Second); !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("disconnected err = %v", err)
	}
}
