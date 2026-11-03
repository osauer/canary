package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// mdTFixture is a ready Connector on a captured wire with SYNX (conId 4242)
// already resolved, so every subscribe path below reaches the wire without
// a contract-details round trip.
func mdTFixture(t *testing.T) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	conn, c, out, _, _ := newQueuedInstructionReconnectFixture(t)
	conn.brokerSessionEpoch.Store(1)
	c.contractCache["SYNX"] = ContractDetailsLite{ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD", LocalSymbol: "SYNX", TradingClass: "NMS"}
	return c, conn, out
}

func mdTFrameCount(t *testing.T, conn *Connection, out *safeBuffer) int {
	t.Helper()
	return len(decodeOutboundFrames(t, conn, out.Bytes()))
}

// mdTFramesAfter returns the outbound frames written after the first n.
func mdTFramesAfter(t *testing.T, conn *Connection, out *safeBuffer, n int) [][]string {
	t.Helper()
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) < n {
		t.Fatalf("outbound shrank: %d < %d", len(frames), n)
	}
	return frames[n:]
}

func mdTSlotHeld(conn *Connection, reqID int) bool {
	conn.marketDataSlotsMu.Lock()
	defer conn.marketDataSlotsMu.Unlock()
	_, held := conn.marketDataSlots[reqID]
	return held
}

func mdTRejection(sub *Subscription) (SubscriptionRejection, bool) {
	select {
	case r := <-sub.RejectCh:
		return r, true
	default:
		return SubscriptionRejection{}, false
	}
}

func mdTSub(c *Connector, key string) *Subscription {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	return c.subscriptions[key]
}

func mdTRouted(c *Connector, reqID int) (string, bool) {
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	key, ok := c.reqIDMap[reqID]
	return key, ok
}

// mdTMarkInactive places an unexpired inactive mark on symbol.
func mdTMarkInactive(c *Connector, symbol string) {
	c.inactiveMu.Lock()
	if c.inactiveSymbols == nil {
		c.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	c.inactiveSymbols[symbol] = inactiveSymbolState{reason: "delisted", markedAt: time.Now()}
	c.inactiveMu.Unlock()
}

func mdTErrorFrame(reqID, code int, message string) []string {
	return []string{strconv.Itoa(msgErrMsg), "2", strconv.Itoa(reqID), strconv.Itoa(code), message}
}

// mdTAssertSharedRequest checks one reqMktData frame carries the resolved
// SYNX contract, the shared generic tick set and no snapshot flags.
func mdTAssertSharedRequest(t *testing.T, frame []string, reqID int) {
	t.Helper()
	assertFields(t, frame, []fieldAssertion{
		{0, strconv.Itoa(reqMktData), "msgID"}, {2, strconv.Itoa(reqID), "reqID"}, {3, "4242", "conID"}, {4, "SYNX", "symbol"},
		{5, "STK", "secType"}, {10, "SMART", "exchange"}, {11, "ARCA", "primary"}, {12, "USD", "currency"},
		{13, "SYNX", "localSymbol"}, {14, "NMS", "tradingClass"}, {16, sharedGenericTicks, "genericTicks"},
		{17, "0", "snapshot"}, {18, "0", "regulatorySnapshot"},
	})
}

// EnsureMarketDataSubscription sends one reqMktData for the resolved
// contract and remembers it; repeating the call is a no-op until the line is
// stale, when the old request is cancelled (or, if the gateway already killed
// it, only its slot released) before a replacement is requested.
func TestEnsureMarketDataSubscriptionRequestsOnceAndRefreshesWhenStale(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	created, err := c.EnsureMarketDataSubscription(ctx, "synx", []string{"last"}, 0)
	if err != nil || !created {
		t.Fatalf("first ensure = %t %v", created, err)
	}
	requests := outboundFramesByID(t, conn, out, reqMktData)
	if len(requests) != 1 {
		t.Fatalf("reqMktData frames = %d", len(requests))
	}
	sub := mdTSub(c, "SYNX")
	if sub == nil || sub.ReqID == 0 || sub.RejectCh == nil || len(sub.Fields) != 1 {
		t.Fatalf("subscription = %+v", sub)
	}
	first := sub.ReqID
	mdTAssertSharedRequest(t, requests[0], first)
	if key, ok := mdTRouted(c, first); !ok || key != "SYNX" || !mdTSlotHeld(conn, first) {
		t.Fatalf("request %d routed to %q (%t), slot held %t", first, key, ok, mdTSlotHeld(conn, first))
	}

	n := mdTFrameCount(t, conn, out)
	for _, stale := range []time.Duration{0, time.Hour} {
		if created, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, stale); err != nil || created {
			t.Fatalf("fresh line re-requested (staleAfter=%s): %t %v", stale, created, err)
		}
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("fresh line wrote to the wire")
	}

	// Stale: the gateway still holds the old line, so it is cancelled first.
	c.handleTickPrice(tickPrice(first, 4, "10.5"))
	c.subMu.Lock()
	sub.LastTime = time.Now().Add(-2 * time.Hour)
	c.subMu.Unlock()
	if created, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, time.Hour); err != nil || !created {
		t.Fatalf("stale refresh = %t %v", created, err)
	}
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 2 {
		t.Fatalf("stale refresh wrote %d frames: %v", len(frames), frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(cancelMktData), "msgID"}, {2, strconv.Itoa(first), "cancelled reqID"}})
	second := sub.ReqID
	if second == first || second == 0 {
		t.Fatalf("refresh kept reqID %d", second)
	}
	mdTAssertSharedRequest(t, frames[1], second)
	if mdTSlotHeld(conn, first) || !mdTSlotHeld(conn, second) {
		t.Fatal("slot accounting did not follow the refresh")
	}
	if key, ok := mdTRouted(c, second); !ok || key != "SYNX" {
		t.Fatal("replacement reqID not routed")
	}
	if sub.LastPrice != 10.5 {
		t.Fatal("a stale refresh must keep the last known price until the new line ticks")
	}

	// Stale and already killed by the gateway: no futile wire cancel.
	n = mdTFrameCount(t, conn, out)
	c.subMu.Lock()
	sub.LastTime = time.Now().Add(-2 * time.Hour)
	sub.rejectedReqID = second
	c.subMu.Unlock()
	if created, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, time.Hour); err != nil || !created {
		t.Fatalf("rejected refresh = %t %v", created, err)
	}
	frames = mdTFramesAfter(t, conn, out, n)
	if len(frames) != 1 || frames[0][0] != strconv.Itoa(reqMktData) {
		t.Fatalf("rejected-line refresh wrote %v, want only the new request", frames)
	}
	if mdTSlotHeld(conn, second) {
		t.Fatal("killed request kept its slot")
	}
	third := sub.ReqID
	if third == second || !mdTSlotHeld(conn, third) {
		t.Fatalf("replacement after rejection = %d", third)
	}

	// A forced reset re-requests a fresh line and clears its observations.
	c.handleTickPrice(tickPrice(third, 1, "9.5"))
	n = mdTFrameCount(t, conn, out)
	if created, err := c.ensureMarketDataSubscription(ctx, "SYNX", nil, 0, true); err != nil || !created {
		t.Fatalf("reset = %t %v", created, err)
	}
	if sub.ReqID == third || sub.Bid != 0 || sub.LastPrice != 0 || sub.Observed {
		t.Fatalf("reset kept observations: %+v", sub)
	}
	if frames = mdTFramesAfter(t, conn, out, n); len(frames) != 2 || frames[0][0] != strconv.Itoa(cancelMktData) {
		t.Fatalf("reset wrote %v", frames)
	}
}

// Regression: the stale refresh cancelled the old request but left its id
// mapped to the symbol, so a late 200 or 354 for the cancelled request aborted
// the replacement's poller and recorded a 354 absence for the live symbol.
//
// After a refresh the cancelled request ID belongs to nothing: a tick still
// in flight for it is ignored, and a terminal notice the gateway emits for it
// (a 354 or 200 queued before the cancel was processed) is a verdict on the
// dead request, not on the replacement line that is now serving the symbol.
func TestEnsureMarketDataSubscriptionRefreshDropsTheCancelledRequestID(t *testing.T) {
	c, _, _ := mdTFixture(t)
	ctx := t.Context()
	if _, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, 0); err != nil {
		t.Fatal(err)
	}
	sub := mdTSub(c, "SYNX")
	old := sub.ReqID
	if _, err := c.ensureMarketDataSubscription(ctx, "SYNX", nil, 0, true); err != nil {
		t.Fatal(err)
	}
	if sub.ReqID == old {
		t.Fatal("reset did not replace the request")
	}
	c.handleTickPrice(tickPrice(old, 4, "77"))
	if sub.LastPrice != 0 || sub.Observed {
		t.Fatalf("tick on cancelled reqID %d reached the refreshed line: %+v", old, sub)
	}
	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(old, 200, "No security definition has been found for the request"))
	if rej, aborted := mdTRejection(sub); aborted {
		t.Errorf("notice %+v for cancelled reqID %d aborted the replacement line (reqID %d)", rej, old, sub.ReqID)
	}
	if post := c.recoverFromSystemNotice(binding, reqAliasEntry{symbol: "SYNX", secType: "STK"}, &systemNotification{tickerID: int64(old), code: 354, message: "late refusal for the cancelled request"}); post != nil {
		post()
	}
	if rej, aborted := mdTRejection(sub); aborted {
		t.Errorf("late 354 for cancelled reqID %d aborted the replacement line: %+v", old, rej)
	}
	if absent := c.marketDataAbsenceFor("SYNX"); absent != nil {
		t.Errorf("late 354 for cancelled reqID %d recorded an absence for the live line: %+v", old, absent)
	}
	if sub.rejectedReqID == sub.ReqID {
		t.Errorf("late notice marked the live replacement as killed")
	}
	// Current gateways deliver the definition verdict as a system notice.
	if post := c.recoverFromSystemNotice(binding, reqAliasEntry{symbol: "SYNX", secType: "STK"}, &systemNotification{tickerID: int64(old), code: 200, message: "No security definition has been found for the request"}); post != nil {
		post()
	}
	if rej, aborted := mdTRejection(sub); aborted {
		t.Errorf("late 200 notice for cancelled reqID %d aborted the replacement line: %+v", old, rej)
	}
	if miss := c.contractResolutionMissFor("SYNX"); miss != nil {
		t.Errorf("late 200 notice for cancelled reqID %d gated the live symbol: %v", old, miss)
	}
}

// Ensure refuses before any wire request when the connector is not ready,
// the symbol carries an inactive mark, or a terminal market-data refusal is
// still inside its window; a symbol resolved on demand goes out with the
// identity the resolver returned.
func TestEnsureMarketDataSubscriptionRefusesGatedSymbols(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if created, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, 0); err == nil || created {
		t.Fatalf("not-ready connector subscribed: %t %v", created, err)
	}
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()

	mdTMarkInactive(c, "ZETA")
	if _, err := c.EnsureMarketDataSubscription(ctx, "zeta", nil, 0); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive symbol error = %v", err)
	}

	c.rememberMarketDataAbsence("SYNX", 354, "synthetic entitlement refusal")
	_, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, 0)
	absent, ok := errors.AsType[*MarketDataAbsenceError](err)
	if !ok || absent.Code != 354 {
		t.Fatalf("absence error = %v", err)
	}
	if mdTFrameCount(t, conn, out) != 0 || mdTSub(c, "SYNX") != nil || mdTSub(c, "ZETA") != nil {
		t.Fatal("a refused ensure reached the wire or left a subscription")
	}

	// Resolution on demand: the resolver's identity goes on the wire and is cached.
	c.fetchContractDetails = func(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
		if symbol != "OMEGA" {
			return nil, errors.New("unexpected symbol " + symbol)
		}
		return []ContractDetailsLite{{Symbol: "OMEGA", ConID: 4343, Exchange: "SMART", PrimaryExch: "NASDAQ", LocalSymbol: "OMEGA", TradingClass: "NMS"}}, nil
	}
	created, err := c.EnsureMarketDataSubscription(ctx, "omega", nil, 0)
	if err != nil || !created {
		t.Fatalf("resolved ensure = %t %v", created, err)
	}
	requests := outboundFramesByID(t, conn, out, reqMktData)
	if len(requests) != 1 {
		t.Fatalf("frames = %v", requests)
	}
	assertFields(t, requests[0], []fieldAssertion{{3, "4343", "conID"}, {4, "OMEGA", "symbol"}, {11, "NASDAQ", "primary"}})
	if !c.hasActiveContract("OMEGA") {
		t.Fatal("resolved identity not cached")
	}
}

// SubscribeMarketData is idempotent per symbol, records the wire form so the
// line can be replayed, and refuses without a wire request while a
// definition miss or an inactive mark stands; without a connection it keeps
// a local line with no live request.
func TestSubscribeMarketDataIsIdempotentAndHonoursGates(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	if err := c.SubscribeMarketData(ctx, "synx", []string{"bid", "ask"}); err != nil {
		t.Fatal(err)
	}
	sub := mdTSub(c, "SYNX")
	if sub == nil || sub.ReqID == 0 || sub.replaySpec == nil || sub.replaySpec.contract.ConID != 4242 || sub.replaySpec.genericTicks != sharedGenericTicks {
		t.Fatalf("subscription = %+v", sub)
	}
	requests := outboundFramesByID(t, conn, out, reqMktData)
	if len(requests) != 1 {
		t.Fatalf("frames = %v", requests)
	}
	mdTAssertSharedRequest(t, requests[0], sub.ReqID)
	if err := c.SubscribeMarketData(ctx, "SYNX", nil); err != nil {
		t.Fatal(err)
	}
	if mdTSub(c, "SYNX") != sub || mdTFrameCount(t, conn, out) != 1 {
		t.Fatal("repeat subscribe replaced the line or wrote to the wire")
	}

	c.recordContractResolutionMiss("ZETA")
	err := c.SubscribeMarketData(ctx, "zeta", nil)
	if miss, ok := errors.AsType[*ContractResolutionMissError](err); !ok || miss.Symbol != "ZETA" {
		t.Fatalf("miss-gated subscribe = %v", err)
	}
	mdTMarkInactive(c, "OMEGA")
	if err := c.SubscribeMarketData(ctx, "omega", nil); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive subscribe = %v", err)
	}
	if mdTSub(c, "ZETA") != nil || mdTSub(c, "OMEGA") != nil || mdTFrameCount(t, conn, out) != 1 {
		t.Fatal("refused subscribes left a line or a wire request")
	}

	offline := NewConnector(&ConnectorConfig{})
	t.Cleanup(offline.conn.rateLimiter.Stop)
	if err := offline.SubscribeMarketData(ctx, "synx", nil); err != nil {
		t.Fatalf("offline subscribe = %v", err)
	}
	if local := mdTSub(offline, "SYNX"); local == nil || local.ReqID != 0 || local.replaySpec == nil || local.replaySpec.symbol != "SYNX" {
		t.Fatalf("offline line = %+v", local)
	}
}

// An option-IV line goes out as the cached option contract with the option
// generic ticks, is routed by its request ID to the chosen key, and
// CancelOptionIV sends exactly one cancel, forgets the routing and releases
// the slot; unknown, zero and stock request IDs are left alone.
func TestSubscribeOptionIVRequestsResolvedContractAndCancelReleasesIt(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	expiry := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	conn.optionContractMu.Lock()
	conn.optionContractCache[optionContractKey("SYNX", "SYNX", "20261120", 100, "C")] = ContractDetailsLite{
		Symbol: "SYNX", SecType: "OPT", Expiry: "20261120", Strike: 100, Right: "C", Multiplier: 100, ConID: 7001,
		Exchange: "SMART", Currency: "USD", LocalSymbol: "SYNX  261120C00100000", TradingClass: "SYNX",
	}
	conn.optionContractMu.Unlock()

	reqID, err := c.SubscribeOptionIV(ctx, "synx", expiry, 100, "c")
	if err != nil || reqID == 0 {
		t.Fatalf("SubscribeOptionIV = %d %v", reqID, err)
	}
	requests := outboundFramesByID(t, conn, out, reqMktData)
	if len(requests) != 1 {
		t.Fatalf("frames = %v", requests)
	}
	assertFields(t, requests[0], []fieldAssertion{
		{2, strconv.Itoa(reqID), "reqID"}, {3, "7001", "conID"}, {4, "SYNX", "symbol"}, {5, "OPT", "secType"},
		{6, "20261120", "expiry"}, {7, "100", "strike"}, {8, "C", "right"}, {9, "100", "multiplier"},
		{16, "100,101,104,106,221,236", "genericTicks"}, {17, "0", "snapshot"},
	})
	if key, ok := mdTRouted(c, reqID); !ok || key != "SYNX" {
		t.Fatalf("option reqID routed to %q", key)
	}
	c.optMu.RLock()
	optKey := c.optReqIDs[reqID]
	c.optMu.RUnlock()
	if optKey != "SYNX" || !mdTSlotHeld(conn, reqID) {
		t.Fatalf("option bookkeeping: key %q slot %t", optKey, mdTSlotHeld(conn, reqID))
	}

	keyedID, key, err := c.SubscribeOptionIVKeyed(ctx, "SYNX", expiry, 100, "C")
	if err != nil || keyedID == reqID || key != OptionMarketDataKey("SYNX", "20261120", "C", 100) {
		t.Fatalf("keyed = %d %q %v", keyedID, key, err)
	}
	if routed, ok := mdTRouted(c, keyedID); !ok || routed != key {
		t.Fatalf("keyed reqID routed to %q", routed)
	}

	if err := c.SubscribeMarketData(ctx, "SYNX", nil); err != nil {
		t.Fatal(err)
	}
	stockID := mdTSub(c, "SYNX").ReqID

	n := mdTFrameCount(t, conn, out)
	c.CancelOptionIV(reqID)
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 1 {
		t.Fatalf("cancel wrote %v", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(cancelMktData), "msgID"}, {2, strconv.Itoa(reqID), "reqID"}})
	if _, ok := mdTRouted(c, reqID); ok || mdTSlotHeld(conn, reqID) {
		t.Fatal("cancelled option line kept its routing or slot")
	}
	c.optMu.RLock()
	_, stillOption := c.optReqIDs[reqID]
	c.optMu.RUnlock()
	if stillOption {
		t.Fatal("cancelled option reqID still marked as option")
	}
	n = mdTFrameCount(t, conn, out)
	c.CancelOptionIV(reqID)
	c.CancelOptionIV(0)
	c.CancelOptionIV(stockID)
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("repeat, zero or stock cancel reached the wire")
	}
	if routed, ok := mdTRouted(c, stockID); !ok || routed != "SYNX" || !mdTSlotHeld(conn, stockID) {
		t.Fatal("CancelOptionIV touched the stock line")
	}

	mdTMarkInactive(c, "ZETA")
	if _, err := c.SubscribeOptionIV(ctx, "ZETA", expiry, 100, "C"); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive option subscribe = %v", err)
	}
	conn.setStatus(StatusDisconnected)
	if _, err := c.SubscribeOptionIV(ctx, "SYNX", expiry, 100, "C"); err == nil {
		t.Fatal("disconnected option subscribe succeeded")
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("refused option subscribes reached the wire")
	}
}

// The delayed fallback switches the connection to delayed mode, re-requests
// the symbol's line with its observations cleared, and its lease restores
// the frozen-aware default exactly once, unless a competing live session has
// since been reported. A failed fallback releases the lease and puts back the
// 354 refusal it had rearmed.
func TestBeginDelayedMarketDataFallbackSwitchesModeRefreshesAndRestores(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	if _, err := c.EnsureMarketDataSubscription(ctx, "SYNX", nil, 0); err != nil {
		t.Fatal(err)
	}
	sub := mdTSub(c, "SYNX")
	old := sub.ReqID
	c.handleTickPrice(tickPrice(old, 4, "10.5"))

	n := mdTFrameCount(t, conn, out)
	release, err := c.BeginDelayedMarketDataFallback(ctx, " synx ")
	if err != nil {
		t.Fatal(err)
	}
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 3 {
		t.Fatalf("fallback wrote %d frames: %v", len(frames), frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgID"}, {2, "3", "delayed"}})
	assertFields(t, frames[1], []fieldAssertion{{0, strconv.Itoa(cancelMktData), "msgID"}, {2, strconv.Itoa(old), "old reqID"}})
	if sub.ReqID == old || sub.LastPrice != 0 || sub.Observed {
		t.Fatalf("fallback did not replace and reset the line: %+v", sub)
	}
	mdTAssertSharedRequest(t, frames[2], sub.ReqID)
	n = mdTFrameCount(t, conn, out)
	release()
	release()
	frames = mdTFramesAfter(t, conn, out, n)
	if len(frames) != 1 {
		t.Fatalf("release wrote %v, want one frozen-mode restore", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgID"}, {2, "2", "frozen"}})

	// A competing live session keeps the connection delayed after release.
	conn.markCompetingLiveSession("synthetic")
	release, err = c.BeginDelayedMarketDataFallback(ctx, "SYNX")
	if err != nil {
		t.Fatal(err)
	}
	n = mdTFrameCount(t, conn, out)
	release()
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("release restored frozen mode under a competing live session")
	}
	conn.competingMu.Lock()
	conn.competingLiveSession = false
	conn.competingMu.Unlock()

	// A recorded 354 is the verdict on the rejected request, not on the
	// delayed one: it is lifted for the fallback and stays lifted on success.
	c.rememberMarketDataAbsence("SYNX", 354, "synthetic refusal")
	release, err = c.BeginDelayedMarketDataFallback(ctx, "SYNX")
	if err != nil {
		t.Fatalf("fallback under 354 = %v", err)
	}
	release()
	if absent := c.marketDataAbsenceFor("SYNX"); absent != nil {
		t.Fatalf("successful fallback put the 354 back: %+v", absent)
	}
	// On failure the rearmed refusal is restored and the lease released.
	c.rememberMarketDataAbsence("SYNX", 354, "synthetic refusal")
	mdTMarkInactive(c, "SYNX")
	n = mdTFrameCount(t, conn, out)
	if _, err := c.BeginDelayedMarketDataFallback(ctx, "SYNX"); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("fallback on an inactive symbol = %v", err)
	}
	frames = mdTFramesAfter(t, conn, out, n)
	if len(frames) != 2 || frames[0][2] != "3" || frames[1][2] != "2" {
		t.Fatalf("failed fallback left the mode lease: %v", frames)
	}
	if absent := c.marketDataAbsenceFor("SYNX"); absent == nil || absent.Code != 354 {
		t.Fatalf("failed fallback lost the 354 it rearmed: %+v", absent)
	}
	c.inactiveMu.Lock()
	delete(c.inactiveSymbols, "SYNX")
	c.inactiveMu.Unlock()
	if !c.marketDataModeMu.TryLock() {
		t.Fatal("failed fallback kept the mode lock")
	}
	c.marketDataModeMu.Unlock()

	// Any other refusal is not rearmed and stops the fallback.
	c.absenceMu.Lock()
	delete(c.mktDataAbsent, "SYNX")
	c.absenceMu.Unlock()
	c.rememberMarketDataAbsence("SYNX", 200, "synthetic definition refusal")
	_, err = c.BeginDelayedMarketDataFallback(ctx, "SYNX")
	if absent, ok := errors.AsType[*MarketDataAbsenceError](err); !ok || absent.Code != 200 {
		t.Fatalf("fallback under a 200 refusal = %v", err)
	}
	if absent := c.marketDataAbsenceFor("SYNX"); absent == nil || absent.Code != 200 {
		t.Fatal("non-354 refusal was lifted")
	}
	c.absenceMu.Lock()
	delete(c.mktDataAbsent, "SYNX")
	c.absenceMu.Unlock()

	if _, err := c.BeginDelayedMarketDataFallback(ctx, "  "); err == nil {
		t.Fatal("empty symbol accepted")
	}
	var none *Connector
	if _, err := none.BeginDelayedMarketDataFallback(ctx, "SYNX"); err == nil {
		t.Fatal("nil connector accepted")
	}
	c.marketDataModeMu.Lock()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	n = mdTFrameCount(t, conn, out)
	if _, err := c.BeginDelayedMarketDataFallback(bounded, "SYNX"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held mode lock -> %v", err)
	}
	c.marketDataModeMu.Unlock()
	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if _, err := c.BeginDelayedMarketDataFallback(ctx, "SYNX"); err == nil {
		t.Fatal("not-ready connector began a fallback")
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("refused fallbacks reached the wire")
	}
	if !c.marketDataModeMu.TryLock() {
		t.Fatal("refused fallback kept the mode lock")
	}
	c.marketDataModeMu.Unlock()
}

// The first 10197 on a shared line forces delayed mode once and replays that
// line under a new request ID without aborting its poller; a later 10197 on
// the replacement, or one on a line with no replay form, is a terminal
// rejection that releases the slot and signals the poller.
func TestRecoverFromSystemNotice10197ForcesDelayedAndReplaysTheSharedLine(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	if err := c.SubscribeMarketData(ctx, "SYNX", nil); err != nil {
		t.Fatal(err)
	}
	sub := mdTSub(c, "SYNX")
	old := sub.ReqID
	c.handleTickPrice(tickPrice(old, 4, "10.5"))
	origin, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	alias := reqAliasEntry{symbol: "SYNX", secType: "STK", conID: 4242}

	n := mdTFrameCount(t, conn, out)
	post := c.recoverFromSystemNotice(origin, alias, &systemNotification{tickerID: int64(old), code: 10197, message: "competing live session"})
	if post == nil {
		t.Fatal("10197 produced no post-barrier work")
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("recovery wrote to the wire under the inbound lease")
	}
	post()
	frames := mdTFramesAfter(t, conn, out, n)
	if len(frames) != 2 {
		t.Fatalf("recovery wrote %v", frames)
	}
	assertFields(t, frames[0], []fieldAssertion{{0, strconv.Itoa(reqMarketDataType), "msgID"}, {2, "3", "delayed"}})
	replacement := sub.ReqID
	if replacement == old || replacement == 0 {
		t.Fatalf("line not replayed: reqID %d", replacement)
	}
	mdTAssertSharedRequest(t, frames[1], replacement)
	if _, aborted := mdTRejection(sub); aborted {
		t.Fatal("poller aborted although the line was replayed")
	}
	if key, ok := mdTRouted(c, replacement); !ok || key != "SYNX" {
		t.Fatal("replacement not routed")
	}
	if _, ok := mdTRouted(c, old); ok || mdTSlotHeld(conn, old) || !mdTSlotHeld(conn, replacement) {
		t.Fatal("killed request kept routing or slot")
	}
	if sub.LastPrice != 0 || !conn.HasCompetingLiveSession() {
		t.Fatalf("replay kept stale live observations (%v) or forgot the competing session", sub.LastPrice)
	}
	c.handleTickPrice(tickPrice(replacement, 68, "10.1"))
	if sub.LastPrice != 10.1 {
		t.Fatal("replacement line does not tick")
	}

	// The replacement is not replayed again.
	n = mdTFrameCount(t, conn, out)
	post = c.recoverFromSystemNotice(origin, alias, &systemNotification{tickerID: int64(replacement), code: 10197, message: "competing live session"})
	if post != nil {
		post()
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatalf("second 10197 wrote %v", mdTFramesAfter(t, conn, out, n))
	}
	if rej, aborted := mdTRejection(sub); !aborted || rej.Code != 10197 {
		t.Fatalf("second 10197 did not abort the poller: %+v %t", rej, aborted)
	}
	if sub.rejectedReqID != replacement || mdTSlotHeld(conn, replacement) {
		t.Fatal("second 10197 left the slot or the kill unrecorded")
	}

	// A line without a replay form (Ensure-created) is rejected outright.
	c.contractCache["ZETA"] = ContractDetailsLite{ConID: 4343, Symbol: "ZETA", Exchange: "SMART", PrimaryExch: "NASDAQ", LocalSymbol: "ZETA", TradingClass: "NMS"}
	if _, err := c.EnsureMarketDataSubscription(ctx, "ZETA", nil, 0); err != nil {
		t.Fatal(err)
	}
	zeta := mdTSub(c, "ZETA")
	n = mdTFrameCount(t, conn, out)
	post = c.recoverFromSystemNotice(origin, reqAliasEntry{symbol: "ZETA", secType: "STK"}, &systemNotification{tickerID: int64(zeta.ReqID), code: 10197, message: "competing live session"})
	if post != nil {
		post()
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatal("unreplayable line was re-requested")
	}
	if rej, aborted := mdTRejection(zeta); !aborted || rej.Code != 10197 || zeta.rejectedReqID != zeta.ReqID || mdTSlotHeld(conn, zeta.ReqID) {
		t.Fatalf("unreplayable line not rejected: %+v", zeta)
	}

	// A socket that retired between the notice and its post-barrier work
	// gets no replay on the successor: the poller is told instead.
	c.contractCache["OMEGA"] = ContractDetailsLite{ConID: 4444, Symbol: "OMEGA", Exchange: "SMART", PrimaryExch: "NASDAQ", LocalSymbol: "OMEGA", TradingClass: "NMS"}
	if err := c.SubscribeMarketData(ctx, "OMEGA", nil); err != nil {
		t.Fatal(err)
	}
	omega := mdTSub(c, "OMEGA")
	retired := origin
	retired.epoch++
	n = mdTFrameCount(t, conn, out)
	if !c.recoverSharedMarketDataAfter10197(retired, omega.ReqID, "competing live session") {
		t.Fatal("shared line not claimed for recovery")
	}
	if mdTFrameCount(t, conn, out) != n {
		t.Fatalf("retired socket replayed: %v", mdTFramesAfter(t, conn, out, n))
	}
	if rej, aborted := mdTRejection(omega); !aborted || rej.Code != 10197 || omega.rejectedReqID != omega.ReqID {
		t.Fatalf("retired-socket recovery did not reject the line: %+v %t", rej, aborted)
	}
	if c.recoverSharedMarketDataAfter10197(origin, omega.ReqID, "competing live session") {
		t.Fatal("a line is replayed at most once after 10197")
	}
}

// Error frames are routed by request ID: a pending history read fails with
// the code's own retry semantics and a dead-definition verdict counts toward
// the symbol's inactive mark; a live quote line is aborted only by terminal
// codes; a market-data farm break marks every line unobserved; frames for a
// retired session and short frames change nothing.
func TestHandleIBKRErrorFromRoutesCodesPerRequest(t *testing.T) {
	c, conn, out := mdTFixture(t)
	ctx := t.Context()
	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no session")
	}
	pending := func(reqID int, symbol string) chan historicalResult {
		ch := make(chan historicalResult, 1)
		c.historicalMu.Lock()
		c.historicalReqs[reqID] = &historicalRequest{symbol: symbol, result: ch}
		c.historicalMu.Unlock()
		return ch
	}
	failed := func(t *testing.T, ch chan historicalResult) *HistoricalRequestError {
		t.Helper()
		select {
		case res := <-ch:
			hErr, ok := errors.AsType[*HistoricalRequestError](res.err)
			if !ok {
				t.Fatalf("history failed with %T %v", res.err, res.err)
			}
			return hErr
		default:
			t.Fatal("history read still pending")
			return nil
		}
	}

	noData := pending(9001, "ZETA")
	post := c.handleIBKRErrorFrom(binding, mdTErrorFrame(9001, 162, "Historical Market Data Service error message:HMDS query returned no data"))
	if hErr := failed(t, noData); hErr.Code != 162 || hErr.RetryAfter != 30*time.Second {
		t.Fatalf("162 no-data -> %+v", hErr)
	}
	if post != nil {
		t.Fatal("a first no-data verdict already produced a mark")
	}
	if c.IsSymbolInactive("ZETA") {
		t.Fatal("one verdict marked the symbol inactive")
	}
	unknown := pending(9002, "ZETA")
	post = c.handleIBKRErrorFrom(binding, mdTErrorFrame(9002, 366, "No historical data query found for ticker id:9002"))
	if hErr := failed(t, unknown); hErr.Code != 366 || hErr.RetryAfter != 0 {
		t.Fatalf("366 -> %+v", hErr)
	}
	if post != nil {
		post()
	}
	if reason, inactive := c.InactiveReason("ZETA"); !inactive || reason == "" {
		t.Fatal("two dead-definition verdicts did not mark the symbol inactive")
	}
	if err := c.SubscribeMarketData(ctx, "ZETA", nil); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("inactive symbol still subscribes: %v", err)
	}

	pacing := pending(9003, "SYNX")
	if post := c.handleIBKRErrorFrom(binding, mdTErrorFrame(9003, 162, "Historical Market Data Service error message:pacing violation")); post != nil {
		t.Fatal("pacing violation counted as a definition verdict")
	}
	if hErr := failed(t, pacing); hErr.Code != 162 || hErr.RetryAfter != 30*time.Second {
		t.Fatalf("162 pacing -> %+v", hErr)
	}
	again := pending(9004, "SYNX")
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(9004, 162, ""))
	if hErr := failed(t, again); hErr.RetryAfter != time.Minute || hErr.Message == "" {
		t.Fatalf("repeated pacing -> %+v", hErr)
	}
	validation := pending(9005, "SYNX")
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(9005, 321, ""))
	if hErr := failed(t, validation); hErr.Code != 321 || hErr.RetryAfter != 0 || hErr.Message == "" {
		t.Fatalf("321 -> %+v", hErr)
	}
	reset := pending(9006, "SYNX")
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(9006, 162, "pacing"))
	if hErr := failed(t, reset); hErr.RetryAfter != 30*time.Second {
		t.Fatalf("backoff not reset by 321: %+v", hErr)
	}
	informational := pending(9007, "SYNX")
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(9007, 2106, "HMDS data farm connection is OK"))
	select {
	case res := <-informational:
		t.Fatalf("informational notice failed the read: %v", res.err)
	default:
	}
	c.historicalMu.Lock()
	_, stillPending := c.historicalReqs[9007]
	delete(c.historicalReqs, 9007)
	c.historicalMu.Unlock()
	if !stillPending {
		t.Fatal("informational notice removed the pending read")
	}

	if err := c.SubscribeMarketData(ctx, "SYNX", nil); err != nil {
		t.Fatal(err)
	}
	sub := mdTSub(c, "SYNX")
	n := mdTFrameCount(t, conn, out)
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(sub.ReqID, 10090, "Part of requested market data is not subscribed"))
	c.handleIBKRErrorFrom(binding, mdTErrorFrame(sub.ReqID, 10091, "Part of requested market data requires additional subscription"))
	if _, aborted := mdTRejection(sub); aborted {
		t.Fatal("a partial-subscription notice aborted the poller")
	}
	if mdTSub(c, "SYNX") != sub || mdTFrameCount(t, conn, out) != n {
		t.Fatal("partial-subscription notice changed the line or the wire")
	}

	sub.Observed = true
	if post := c.handleIBKRErrorFrom(binding, mdTErrorFrame(-1, 2108, "Market data farm connection is broken")); post != nil {
		t.Fatal("farm break produced post-barrier work")
	}
	if sub.Observed {
		t.Fatal("farm break left the line observed")
	}
	if _, aborted := mdTRejection(sub); aborted {
		t.Fatal("farm break aborted the poller")
	}

	retired := binding
	retired.epoch++
	c.handleIBKRErrorFrom(retired, mdTErrorFrame(sub.ReqID, 200, "No security definition has been found for the request"))
	c.handleIBKRErrorFrom(binding, []string{"4", "2", strconv.Itoa(sub.ReqID)})
	if _, aborted := mdTRejection(sub); aborted {
		t.Fatal("a retired session's or short frame reached the live line")
	}
	if c.IsSymbolInactive("SYNX") {
		t.Fatal("retired-session notice counted toward an inactive mark")
	}

	post = c.handleIBKRErrorFrom(binding, mdTErrorFrame(sub.ReqID, 200, "No security definition has been found for the request"))
	if rej, aborted := mdTRejection(sub); !aborted || rej.Code != 200 {
		t.Fatalf("200 did not abort the poller: %+v %t", rej, aborted)
	}
	if post != nil {
		post()
	}
	if c.IsSymbolInactive("SYNX") {
		t.Fatal("a single 200 marked the symbol inactive")
	}
	post = c.handleIBKRErrorFrom(binding, mdTErrorFrame(sub.ReqID, 200, "No security definition has been found for the request"))
	if post != nil {
		post()
	}
	if !c.IsSymbolInactive("SYNX") {
		t.Fatal("two 200 verdicts did not mark the symbol inactive")
	}
}
