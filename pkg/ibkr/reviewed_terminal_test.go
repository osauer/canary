package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// Membership is symbol-level for bare and default-route keys, STK-only for
// route keys, and narrowed by an explicit ConID; removal restores ordinary
// behaviour without touching heuristic marks.
func TestSetReviewedTerminalCoversExactStockKeysOnly(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.inactiveMu.Lock()
	c.inactiveSymbols = map[string]inactiveSymbolState{"SYNTHMARK": {reason: "heuristic", markedAt: time.Now()}}
	c.inactiveMu.Unlock()

	c.SetReviewedTerminal(map[string]ReviewedTerminalStock{"synthdead ": {ConID: 900201}})

	stk := Contract{Symbol: "SYNTHDEAD", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	exact, other, opt := stk, stk, stk
	exact.ConID, other.ConID = 900201, 900202
	opt.SecType, opt.ConID = "OPT", 900203
	for _, key := range []string{"SYNTHDEAD", "synthdead", DefaultMarketDataKeyForSymbol("SYNTHDEAD"), MarketDataKeyForContract(stk), MarketDataKeyForContract(exact)} {
		reason, inactive := c.InactiveReason(key)
		if !inactive || reason != defaultReviewedTerminalReason {
			t.Fatalf("%q not covered: inactive=%t reason=%q", key, inactive, reason)
		}
	}
	for _, key := range []string{MarketDataKeyForContract(other), MarketDataKeyForContract(opt), "SYNTHOTHER", "SYNTHDEADX"} {
		if c.IsSymbolInactive(key) {
			t.Fatalf("%q covered without exact stock identity", key)
		}
	}

	// Connection loss clears heuristic session marks, never reviewed evidence.
	c.invalidateUnstampedConnectorObservations(c.conn)
	if !c.IsSymbolInactive("SYNTHDEAD") {
		t.Fatal("reviewed terminal membership did not survive connection loss")
	}

	c.inactiveMu.Lock()
	c.inactiveSymbols = map[string]inactiveSymbolState{"SYNTHMARK": {reason: "heuristic", markedAt: time.Now()}}
	c.inactiveMu.Unlock()
	c.SetReviewedTerminal(nil)
	if c.IsSymbolInactive("SYNTHDEAD") || c.IsSymbolInactive(MarketDataKeyForContract(exact)) {
		t.Fatal("removal did not restore ordinary behaviour")
	}
	if !c.IsSymbolInactive("SYNTHMARK") {
		t.Fatal("clearing the reviewed set dropped a heuristic mark")
	}
}

// The witnessed loop: a held terminal stock's routed quote fell back to
// delayed data and its live-retry timer re-probed the broker every thirty
// minutes. Adding it to the reviewed set must tear that line down (timer
// stopped, broker request cancelled), refuse later subscribes and chart reads
// without wire traffic, and leave unrelated lines alone. Removal lifts the
// reviewed gate again.
func TestSetReviewedTerminalDetachesLingeringSubscription(t *testing.T) {
	c, conn, out, _ := newQuoteFallbackFixture(t)
	dead := Contract{ConID: 900201, Symbol: "SYNTHDEAD", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	live := Contract{ConID: 900301, Symbol: "SYNTHLIVE", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	deadKey, err := c.SubscribeMarketDataWithContract(t.Context(), dead, nil)
	if err != nil {
		t.Fatal(err)
	}
	liveKey, err := c.SubscribeMarketDataWithContract(t.Context(), live, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, deadKey, 354)
	c.subMu.RLock()
	sub := c.subscriptions[deadKey]
	timer := sub.liveRetryTimer
	reqID := sub.ReqID
	c.subMu.RUnlock()
	if timer == nil {
		t.Fatal("fixture did not arm the live re-probe")
	}

	before := len(decodeOutboundFrames(t, conn, out.Bytes()))
	c.SetReviewedTerminal(map[string]ReviewedTerminalStock{"SYNTHDEAD": {ConID: 900201, Reason: "reviewed terminal evidence"}})

	c.subMu.RLock()
	_, deadStill := c.subscriptions[deadKey]
	_, liveStill := c.subscriptions[liveKey]
	_, reqStill := c.reqIDMap[reqID]
	c.subMu.RUnlock()
	if deadStill || reqStill {
		t.Fatal("reviewed terminal stock kept its subscription")
	}
	if !liveStill {
		t.Fatal("an unrelated subscription was detached")
	}
	if timer.Stop() {
		t.Fatal("the live re-probe timer was still armed")
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != before+1 || frames[before][0] != strconv.Itoa(cancelMktData) || frames[before][2] != strconv.Itoa(reqID) {
		t.Fatalf("want exactly one cancel of reqID %d, got %v", reqID, frames[before:])
	}

	// Re-publishing the same set detaches nothing further.
	c.SetReviewedTerminal(map[string]ReviewedTerminalStock{"SYNTHDEAD": {ConID: 900201, Reason: "reviewed terminal evidence"}})
	wire := out.Len()
	if _, err := c.SubscribeMarketDataWithContract(t.Context(), dead, nil); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("routed subscribe = %v, want ErrSymbolInactive", err)
	}
	if err := c.SubscribeMarketData(t.Context(), "SYNTHDEAD", nil); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("symbol subscribe = %v, want ErrSymbolInactive", err)
	}
	if _, err := c.FetchContractDetails("SYNTHDEAD", time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("contract details = %v, want ErrSymbolInactive", err)
	}
	if _, err := c.FetchChartBars(t.Context(), dead, 30, "1 day", time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("chart bars = %v, want ErrSymbolInactive", err)
	}
	if out.Len() != wire {
		t.Fatal("a refused request still wrote wire frames")
	}

	// Removal lifts only the reviewed gate; the route's own 354 absence
	// memory still answers, so the refusal is no longer ErrSymbolInactive.
	c.SetReviewedTerminal(map[string]ReviewedTerminalStock{})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := c.SubscribeMarketDataWithContract(ctx, dead, nil); errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("subscribe after removal still refused as inactive: %v", err)
	}
	if c.IsSymbolInactive("SYNTHDEAD") {
		t.Fatal("removal left the stock inactive")
	}
}
