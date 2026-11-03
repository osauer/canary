package ibkr

import (
	"strconv"
	"testing"
	"time"
)

// mdTDelayedLine opens a shared quote line, refuses it with 354 so it moves
// to the delayed feed, serves one delayed close and disarms the half-hour
// live probe so the test owns the retry.
func mdTDelayedLine(t *testing.T) (*Connector, *Connection, *safeBuffer, string, *Subscription) {
	t.Helper()
	c, conn, out, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	quoteType(c, key, 4)
	quoteTick(c, key, 75, 100)
	sub := mdTSub(c, key)
	if sub == nil || !sub.delayedFallback || sub.liveRetryTimer == nil {
		t.Fatalf("line not on the delayed feed with a probe armed: %+v", sub)
	}
	sub.liveRetryTimer.Stop()
	t.Cleanup(func() {
		c.subMu.Lock()
		if sub.liveRetryTimer != nil {
			sub.liveRetryTimer.Stop()
		}
		c.subMu.Unlock()
	})
	return c, conn, out, key, sub
}

func mdTAwaitReplacement(t *testing.T, c *Connector, sub *Subscription, old int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.subMu.RLock()
		id := sub.ReqID
		c.subMu.RUnlock()
		if id != old {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("request %d was never replaced", old)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The live probe re-requests the line in frozen-aware mode, keeps the delayed
// close visible until a live price arrives, then reports the live feed, clears
// the refusal and arms the next half-hour probe.
func TestRetrySharedQuoteLiveProbesLiveAndRestoresTheLineOnATick(t *testing.T) {
	c, conn, out, key, sub := mdTDelayedLine(t)
	origin, _ := c.CaptureSession()
	old := sub.ReqID
	before := c.MarketDataSnapshot()[key]
	n := mdTFrameCount(t, conn, out)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.retrySharedQuoteLive(origin, key, sub)
	}()
	probe := mdTAwaitReplacement(t, c, sub, old)
	// The probe cancels the delayed request, then asks for the line in
	// frozen-aware mode (type 2) around the request so no other request
	// can inherit the override.
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 4 {
		t.Fatalf("live probe wrote %d frames: %v", len(frames), frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(cancelMktData), "msgID"}, {2, strconv.Itoa(old), "cancelled delayed reqID"}})
	assertFields(t, frames[1], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgID"}, {2, "2", "frozen-aware probe mode"}})
	assertFields(t, frames[2], []fieldAssertion{{0, strconv.Itoa(reqMktData), "msgID"}, {2, strconv.Itoa(probe), "probe reqID"}, {3, "90123", "conID"}, {16, sharedGenericTicks, "live generic ticks"}})
	assertFields(t, frames[3], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgID"}, {2, "2", "restored mode"}})
	pending := c.MarketDataSnapshot()[key]
	if pending.Close != before.Close || pending.FeedType != before.FeedType || !pending.CloseAt.Equal(before.CloseAt) {
		t.Fatalf("probe hid the delayed close: %+v vs %+v", pending, before)
	}
	select {
	case <-done:
		t.Fatal("probe returned before any price")
	default:
	}
	quoteType(c, key, 1)
	quoteTick(c, key, 1, 102)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not finish after a live tick")
	}
	live := c.MarketDataSnapshot()[key]
	if live.FeedType != 1 || live.Bid != 102 || live.Close != 0 {
		t.Fatalf("live line = %+v", live)
	}
	if abs := c.MarketDataAbsences(); len(abs) != 0 {
		t.Fatalf("live recovery kept the refusal: %+v", abs)
	}
	c.subMu.RLock()
	delayed, next, armed := sub.delayedFallback, sub.liveRetryAt, sub.liveRetryTimer != nil
	c.subMu.RUnlock()
	if delayed || !armed || time.Until(next) < marketDataAbsenceRetry-time.Second || time.Until(next) > marketDataAbsenceRetry {
		t.Fatalf("after recovery delayed=%t armed=%t next=%s", delayed, armed, time.Until(next))
	}
	if mdTSlotHeld(conn, old) || !mdTSlotHeld(conn, probe) {
		t.Fatal("slot accounting did not follow the probe")
	}
}

// A live probe the gateway refuses again returns the line to the delayed
// feed instead of leaving it empty.
func TestRetrySharedQuoteLiveReturnsToDelayedWhenTheProbeIsRefused(t *testing.T) {
	c, conn, out, key, sub := mdTDelayedLine(t)
	origin, _ := c.CaptureSession()
	old := sub.ReqID
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.retrySharedQuoteLive(origin, key, sub)
	}()
	probe := mdTAwaitReplacement(t, c, sub, old)
	n := mdTFrameCount(t, conn, out)
	rejectQuote(t, c, key, 354)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not finish after its refusal")
	}
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 3 || frames[0][2] != "4" || frames[1][0] != strconv.Itoa(reqMktData) || frames[1][16] != delayedQuoteGenericTicks || frames[2][2] != "2" {
		t.Fatalf("refused probe did not fall back to delayed: %v", frames)
	}
	c.subMu.RLock()
	delayed, id := sub.delayedFallback, sub.ReqID
	c.subMu.RUnlock()
	if !delayed || id == probe || id == old {
		t.Fatalf("line after refused probe: delayed=%t reqID=%d (old %d, probe %d)", delayed, id, old, probe)
	}
	if md := c.MarketDataSnapshot()[key]; md.Close != 100 {
		t.Fatalf("refused probe lost the delayed close: %+v", md)
	}
	if abs := c.MarketDataAbsences(); len(abs) != 1 || abs[0].Code != 354 {
		t.Fatalf("refusal not disclosed after the probe: %+v", abs)
	}
}

// A probe for a retired session or a replaced line writes nothing.
func TestRetrySharedQuoteLiveLeavesRetiredSessionsAndReplacedLinesAlone(t *testing.T) {
	c, conn, out, key, sub := mdTDelayedLine(t)
	origin, _ := c.CaptureSession()
	n := mdTFrameCount(t, conn, out)
	retired := origin
	retired.epoch++
	c.retrySharedQuoteLive(retired, key, sub)
	c.retrySharedQuoteLive(origin, key, &Subscription{Symbol: key, ReqID: sub.ReqID})
	if mdTFrameCount(t, conn, out) != n {
		t.Fatalf("stray probe wrote %v", mdTFramesAfter(t, conn, out, n))
	}
	c.subMu.RLock()
	id, delayed := sub.ReqID, sub.delayedFallback
	c.subMu.RUnlock()
	if mdTSub(c, key) != sub || !delayed || id == 0 {
		t.Fatal("stray probe touched the line")
	}
}

// A shared quote whose wire request is refused before it is sent leaves no
// line behind, so the next subscribe starts clean.
func TestSubscribeSharedQuoteRemovesTheLineWhenTheRequestFails(t *testing.T) {
	c, conn, out, _ := newQuoteFallbackFixture(t)
	n := mdTFrameCount(t, conn, out)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), Contract{ConID: 90124, Symbol: "SYNÉ", SecType: "IND", Exchange: "NASDAQ"}, nil)
	if err == nil {
		t.Fatal("non-ASCII symbol accepted")
	}
	if key == "" {
		t.Fatal("route key missing from the refusal")
	}
	if mdTSub(c, key) != nil || mdTFrameCount(t, conn, out) != n {
		t.Fatal("refused request left a line or wrote to the wire")
	}
	c.subMu.RLock()
	routes := len(c.reqIDMap)
	c.subMu.RUnlock()
	if routes != 0 {
		t.Fatalf("refused request left %d routes", routes)
	}
}
