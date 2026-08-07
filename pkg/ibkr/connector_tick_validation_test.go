package ibkr

import (
	"testing"
	"time"
)

// TestHandleTickPrice_ZeroValueRejection verifies that zero prices do not overwrite valid prices.
// This test addresses the issue where IBKR sends tick updates with price=0.0 (indicating
// no quote available) which would incorrectly overwrite previously received valid prices.
func TestHandleTickPrice_ZeroValueRejection(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Simulate IBKR sending a zero bid price (no quote available)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	// Zero price should NOT overwrite the previous valid bid
	if sub.Bid != 580.50 {
		t.Errorf("zero bid price overwrote valid price: expected 580.50, got %.2f", sub.Bid)
	}

	// Simulate zero ask price
	connector.handleTickPrice([]string{"1", "2", "42", "2", "0"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Ask != 580.52 {
		t.Errorf("zero ask price overwrote valid price: expected 580.52, got %.2f", sub.Ask)
	}

	// Simulate zero last price
	connector.handleTickPrice([]string{"1", "2", "42", "4", "0.00"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.LastPrice != 580.51 {
		t.Errorf("zero last price overwrote valid price: expected 580.51, got %.2f", sub.LastPrice)
	}
}

// TestHandleTickPrice_ValidPriceUpdate verifies that valid non-zero prices ARE updated correctly.
func TestHandleTickPrice_ValidPriceUpdate(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Update bid with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "1", "581.00"})
	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 581.00 {
		t.Errorf("valid bid price not updated: expected 581.00, got %.2f", sub.Bid)
	}

	// Update ask with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "2", "581.02"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Ask != 581.02 {
		t.Errorf("valid ask price not updated: expected 581.02, got %.2f", sub.Ask)
	}

	// Update last with valid price
	connector.handleTickPrice([]string{"1", "2", "42", "4", "581.01"})
	connector.subMu.RLock()
	sub = connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.LastPrice != 581.01 {
		t.Errorf("valid last price not updated: expected 581.01, got %.2f", sub.LastPrice)
	}
}

func TestHandleTickPrice_DelayedPriceUpdate(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "MBG|STK|IBIS||EUR||"
	connector.subscriptions["MBG|STK|IBIS||EUR||"] = &Subscription{Symbol: "MBG|STK|IBIS||EUR||"}
	connector.subMu.Unlock()

	connector.handleTickPrice([]string{"1", "2", "42", "66", "56.10"})
	connector.handleTickPrice([]string{"1", "2", "42", "67", "56.12"})
	connector.handleTickPrice([]string{"1", "2", "42", "68", "56.11"})
	connector.handleTickPrice([]string{"1", "2", "42", "75", "55.50"})

	connector.subMu.RLock()
	sub := connector.subscriptions["MBG|STK|IBIS||EUR||"]
	connector.subMu.RUnlock()
	if sub.Bid != 56.10 {
		t.Errorf("delayed bid: want 56.10, got %.2f", sub.Bid)
	}
	if sub.Ask != 56.12 {
		t.Errorf("delayed ask: want 56.12, got %.2f", sub.Ask)
	}
	if sub.LastPrice != 56.11 {
		t.Errorf("delayed last: want 56.11, got %.2f", sub.LastPrice)
	}
	if sub.PrevClose != 55.50 {
		t.Errorf("delayed close: want 55.50, got %.2f", sub.PrevClose)
	}
}

func TestHandleTickPrice_OptionReqUpdatesQuoteCacheAndSubscription(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	key := "SPY_260601C758"
	connector.subMu.Lock()
	connector.reqIDMap[42] = key
	connector.subscriptions[key] = &Subscription{Symbol: key}
	connector.subMu.Unlock()
	connector.optMu.Lock()
	connector.optReqIDs[42] = key
	connector.optMu.Unlock()

	connector.handleTickPrice([]string{"1", "2", "42", "1", "1.46"})
	connector.handleTickPrice([]string{"1", "2", "42", "2", "1.47"})
	connector.handleTickPrice([]string{"1", "2", "42", "4", "1.46"})
	connector.handleTickPrice([]string{"1", "2", "42", "9", "1.45"})

	bid, ask, ok := connector.OptionQuoteBidAsk(key)
	if !ok || bid != 1.46 || ask != 1.47 {
		t.Fatalf("option quote cache = %.2f x %.2f ok=%v, want 1.46 x 1.47 ok=true", bid, ask, ok)
	}
	if prev, ok := connector.OptionPrevClose(key); !ok || prev != 1.45 {
		t.Fatalf("option prev close = %.2f ok=%v, want 1.45 ok=true", prev, ok)
	}

	md := connector.MarketDataSnapshot()[key]
	if md == nil {
		t.Fatalf("market data missing for option key %s", key)
	}
	if md.Bid != 1.46 || md.Ask != 1.47 || md.Last != 1.46 || md.Close != 1.45 {
		t.Fatalf("option subscription quote = bid %.2f ask %.2f last %.2f close %.2f, want 1.46 1.47 1.46 1.45",
			md.Bid, md.Ask, md.Last, md.Close)
	}
}

func TestHandleTickPrice_OptionDelayedTicksUpdateQuoteCacheAndSubscription(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	key := "SPY_260601P758"
	connector.subMu.Lock()
	connector.reqIDMap[42] = key
	connector.subscriptions[key] = &Subscription{Symbol: key}
	connector.subMu.Unlock()
	connector.optMu.Lock()
	connector.optReqIDs[42] = key
	connector.optMu.Unlock()

	connector.handleTickPrice([]string{"1", "2", "42", "66", "2.84"})
	connector.handleTickPrice([]string{"1", "2", "42", "67", "2.86"})
	connector.handleTickPrice([]string{"1", "2", "42", "68", "2.85"})
	connector.handleTickPrice([]string{"1", "2", "42", "75", "2.80"})

	bid, ask, ok := connector.OptionQuoteBidAsk(key)
	if !ok || bid != 2.84 || ask != 2.86 {
		t.Fatalf("delayed option quote cache = %.2f x %.2f ok=%v, want 2.84 x 2.86 ok=true", bid, ask, ok)
	}
	md := connector.MarketDataSnapshot()[key]
	if md == nil {
		t.Fatalf("market data missing for option key %s", key)
	}
	if md.Bid != 2.84 || md.Ask != 2.86 || md.Last != 2.85 || md.Close != 2.80 {
		t.Fatalf("delayed option subscription quote = bid %.2f ask %.2f last %.2f close %.2f, want 2.84 2.86 2.85 2.80",
			md.Bid, md.Ask, md.Last, md.Close)
	}
}

// TestHandleTickPrice_InvalidParsing tests that malformed price strings are handled gracefully.
func TestHandleTickPrice_InvalidParsing(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	testCases := []struct {
		name      string
		fields    []string
		checkFunc func(*Subscription) error
	}{
		{
			name:   "invalid price string",
			fields: []string{"1", "2", "42", "1", "not-a-number"},
			checkFunc: func(sub *Subscription) error {
				// Invalid parse should result in 0.0, which should be rejected
				if sub.Bid != 580.50 {
					t.Errorf("invalid price string changed bid: expected 580.50, got %.2f", sub.Bid)
				}
				return nil
			},
		},
		{
			name:   "empty price field",
			fields: []string{"1", "2", "42", "2", ""},
			checkFunc: func(sub *Subscription) error {
				if sub.Ask != 580.52 {
					t.Errorf("empty price field changed ask: expected 580.52, got %.2f", sub.Ask)
				}
				return nil
			},
		},
		{
			name:   "invalid reqID",
			fields: []string{"1", "2", "not-a-number", "1", "590.00"},
			checkFunc: func(sub *Subscription) error {
				// Invalid reqID should prevent any update
				if sub.Bid != 580.50 {
					t.Errorf("invalid reqID caused update: expected 580.50, got %.2f", sub.Bid)
				}
				return nil
			},
		},
		{
			name:   "invalid tickType",
			fields: []string{"1", "2", "42", "invalid", "590.00"},
			checkFunc: func(sub *Subscription) error {
				// Invalid tickType should prevent any update (tickType would be 0, not 1/2/4)
				if sub.Bid != 580.50 || sub.Ask != 580.52 || sub.LastPrice != 580.51 {
					t.Errorf("invalid tickType caused updates")
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			connector.handleTickPrice(tc.fields)
			connector.subMu.RLock()
			sub := connector.subscriptions["SPY"]
			connector.subMu.RUnlock()

			if err := tc.checkFunc(sub); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestHandleTickPrice_ObservedFlag verifies that the Observed flag is set appropriately.
func TestHandleTickPrice_ObservedFlag(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		Observed: false,
	}
	connector.subMu.Unlock()

	// Send valid price
	connector.handleTickPrice([]string{"1", "2", "42", "1", "580.50"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if !sub.Observed {
		t.Error("Observed flag not set after receiving valid tick")
	}
}

// TestHandleTickPrice_LastTimeUpdate verifies LastTime is updated even with rejected prices.
func TestHandleTickPrice_LastTimeUpdate(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	initialTime := time.Now().Add(-1 * time.Hour)
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		Bid:      580.50,
		LastTime: initialTime,
	}
	connector.subMu.Unlock()

	// Send zero price (should be rejected but LastTime should update)
	time.Sleep(10 * time.Millisecond)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	// Price should not change
	if sub.Bid != 580.50 {
		t.Errorf("bid changed unexpectedly: %.2f", sub.Bid)
	}

	// But LastTime should be updated to show we received a tick
	if !sub.LastTime.After(initialTime) {
		t.Error("LastTime not updated after receiving tick")
	}
}

// A subscription's LastTime is seeded when the subscription is created and
// advanced by bookkeeping, so it cannot answer "when did a tick last arrive".
// LastTickAt exists to answer exactly that: zero until a tick lands, and then
// the arrival instant. Without the distinction a live subscription that has
// gone quiet is indistinguishable from one that is ticking.
func TestMarketDataSnapshot_LastTickAtIsTickArrivalNotSubscribeTime(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	subscribedAt := time.Now().Add(-time.Hour)
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:   "SPY",
		ReqID:    42,
		LastTime: subscribedAt,
	}
	connector.subMu.Unlock()

	if got := connector.MarketDataSnapshot()["SPY"]; !got.LastTickAt.IsZero() {
		t.Fatalf("LastTickAt = %s before any tick; want zero so absence cannot read as a fresh observation", got.LastTickAt)
	}

	before := time.Now()
	connector.handleTickPrice([]string{"1", "2", "42", "1", "580.50"})

	got := connector.MarketDataSnapshot()["SPY"]
	if got.LastTickAt.Before(before) {
		t.Fatalf("LastTickAt = %s; want the tick's arrival instant, at or after %s", got.LastTickAt, before)
	}
	if !got.LastTickAt.After(subscribedAt) {
		t.Fatalf("LastTickAt = %s; want it to advance past the subscribe-time seed %s", got.LastTickAt, subscribedAt)
	}
}

// A size tick with no price still advances LastTickAt: it is an "any tick"
// arrival instant, so a subscription delivering only size ticks looks alive
// while its price is frozen. Pinned because every reader has to know it.
func TestMarketDataSnapshot_LastTickAtAdvancesOnNonPriceTicks(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{Symbol: "SPY", ReqID: 42, LastPrice: 580.51}
	connector.subMu.Unlock()

	connector.handleTickSize([]string{"2", "2", "42", "0", "300"})

	if got := connector.MarketDataSnapshot()["SPY"]; got.LastTickAt.IsZero() {
		t.Fatal("LastTickAt still zero after a bid-size tick; it tracks any tick arrival, not price arrival")
	}
}

// Price freshness must not inherit the any-tick liveness clock. A liquid
// subscription can keep receiving size and volume while its bid/ask/last are
// frozen, which is exactly the failure mode LastPriceTickAt needs to expose.
func TestMarketDataSnapshot_LastPriceTickAtAdvancesOnlyOnAcceptedPrice(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{Symbol: "SPY", ReqID: 42, LastPrice: 580.51}
	connector.subMu.Unlock()

	connector.handleTickSize([]string{"2", "2", "42", "0", "300"})
	connector.handleTickPrice([]string{"1", "2", "42", "1", ""})
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0"})

	got := connector.MarketDataSnapshot()["SPY"]
	if got.LastTickAt.IsZero() {
		t.Fatal("LastTickAt stayed zero after non-price traffic")
	}
	if !got.LastPriceTickAt.IsZero() {
		t.Fatalf("LastPriceTickAt = %s before an accepted price; non-price and rejected traffic must not advance it", got.LastPriceTickAt)
	}

	before := time.Now()
	connector.handleTickPrice([]string{"1", "2", "42", "1", "580.50"})
	got = connector.MarketDataSnapshot()["SPY"]
	if got.LastPriceTickAt.Before(before) {
		t.Fatalf("LastPriceTickAt = %s; want accepted-price arrival at or after %s", got.LastPriceTickAt, before)
	}
	if !got.LastPriceTickAt.Equal(got.LastTickAt) {
		t.Fatalf("accepted price clocks differ: LastPriceTickAt=%s LastTickAt=%s", got.LastPriceTickAt, got.LastTickAt)
	}
}

// TestHandleTickPrice_NegativePrice verifies negative prices are also validated.
func TestHandleTickPrice_NegativePrice(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "SPY"
	connector.subscriptions["SPY"] = &Subscription{
		Symbol:    "SPY",
		Bid:       580.50,
		Ask:       580.52,
		LastPrice: 580.51,
	}
	connector.subMu.Unlock()

	// Negative prices should be rejected
	connector.handleTickPrice([]string{"1", "2", "42", "1", "-1.0"})
	connector.handleTickPrice([]string{"1", "2", "42", "2", "-0.5"})
	connector.handleTickPrice([]string{"1", "2", "42", "4", "-100"})

	connector.subMu.RLock()
	sub := connector.subscriptions["SPY"]
	connector.subMu.RUnlock()

	if sub.Bid != 580.50 {
		t.Errorf("negative bid overwrote price: %.2f", sub.Bid)
	}
	if sub.Ask != 580.52 {
		t.Errorf("negative ask overwrote price: %.2f", sub.Ask)
	}
	if sub.LastPrice != 580.51 {
		t.Errorf("negative last overwrote price: %.2f", sub.LastPrice)
	}
}

// TestHandleTickPrice_VerySmallPrice tests handling of very small but valid prices (like some penny stocks).
func TestHandleTickPrice_VerySmallPrice(t *testing.T) {
	connector := NewConnector(&ConnectorConfig{})
	connector.subMu.Lock()
	connector.reqIDMap[42] = "PENNY"
	connector.subscriptions["PENNY"] = &Subscription{Symbol: "PENNY"}
	connector.subMu.Unlock()

	// Very small but valid price (0.0001 for penny stocks)
	connector.handleTickPrice([]string{"1", "2", "42", "1", "0.0001"})

	connector.subMu.RLock()
	sub := connector.subscriptions["PENNY"]
	connector.subMu.RUnlock()

	if sub.Bid != 0.0001 {
		t.Errorf("small valid price rejected: expected 0.0001, got %.4f", sub.Bid)
	}
}

// TestShortableAndUnderlyingIVArriveOnWireTickIDs pins the wire-id vs
// request-id distinction for the generic-tick bundle: requesting generic
// tick 236 delivers wire tick 89 (shortable share count, a tickSize) and
// wire tick 46 (difficulty level, ignored); requesting generic tick 106
// delivers wire tick 24 (chain-averaged underlying IV, a tickGeneric).
// An earlier dispatch matched the request ids (236/106), which never
// appear on the wire — borrow inventory reported "unknown" for every
// symbol and quote IV stayed null (observed 2026-06-11). The request-id
// cases must stay dead: tick 46's 0–3 float stored as a share count
// would fire false Borrow-scarce flags.
func TestShortableAndUnderlyingIVArriveOnWireTickIDs(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AMD"
	c.subscriptions["AMD"] = &Subscription{Symbol: "AMD"}
	c.subMu.Unlock()

	// Request-id lookalikes must be ignored.
	c.handleTickGeneric([]string{"45", "6", "7", "236", "2.9"}) // request id, not a wire tick
	c.handleTickGeneric([]string{"45", "6", "7", "46", "2.9"})  // difficulty level — ignored
	c.handleTickGeneric([]string{"45", "6", "7", "106", "0.35"})

	c.subMu.RLock()
	sub := c.subscriptions["AMD"]
	c.subMu.RUnlock()
	if sub.ShortableObserved || sub.ShortableShares != 0 {
		t.Fatalf("request-id/difficulty ticks must not set shortable state: shares=%d observed=%v",
			sub.ShortableShares, sub.ShortableObserved)
	}
	if sub.IV != 0 {
		t.Fatalf("request-id tick 106 must not set IV, got %v", sub.IV)
	}

	// The real wire ticks land.
	c.handleTickSize([]string{"2", "6", "7", "89", "8500"})     // shortable share count
	c.handleTickGeneric([]string{"45", "6", "7", "24", "0.35"}) // underlying IV

	c.subMu.RLock()
	sub = c.subscriptions["AMD"]
	c.subMu.RUnlock()
	if !sub.ShortableObserved || sub.ShortableShares != 8500 {
		t.Fatalf("wire tick 89 should set shortable shares: shares=%d observed=%v",
			sub.ShortableShares, sub.ShortableObserved)
	}
	if sub.ShortableTickAt.IsZero() {
		t.Fatal("wire tick 89 should stamp the shortable-share observation")
	}
	if sub.IV != 0.35 {
		t.Fatalf("wire tick 24 should set underlying IV, got %v", sub.IV)
	}
}

// TestHandleTickSize_DispatchesByTickType verifies bid_size (0), ask_size (3),
// volume (8), and average volume (21) ticks land on the right Subscription
// field. Other tick types (e.g. 5=last_size) are intentionally dropped.
func TestHandleTickSize_DispatchesByTickType(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	// Fields format: [msgID, version, reqID, tickType, size]
	c.handleTickSize([]string{"2", "6", "7", "0", "1500"})      // bid_size
	c.handleTickSize([]string{"2", "6", "7", "3", "2200"})      // ask_size
	c.handleTickSize([]string{"2", "6", "7", "8", "9876543"})   // volume
	c.handleTickSize([]string{"2", "6", "7", "69", "1600"})     // delayed_bid_size
	c.handleTickSize([]string{"2", "6", "7", "70", "2300"})     // delayed_ask_size
	c.handleTickSize([]string{"2", "6", "7", "74", "9876544"})  // delayed_volume
	c.handleTickSize([]string{"2", "6", "7", "21", "58900000"}) // avg_volume
	c.handleTickSize([]string{"2", "6", "7", "5", "999"})       // last_size — ignored

	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()

	if sub.BidSize != 1600 {
		t.Errorf("BidSize: want 1600, got %d", sub.BidSize)
	}
	if sub.AskSize != 2300 {
		t.Errorf("AskSize: want 2300, got %d", sub.AskSize)
	}
	if sub.Volume != 9876544 {
		t.Errorf("Volume: want 9876544, got %d", sub.Volume)
	}
	if sub.AvgVolume != 58900000 {
		t.Errorf("AvgVolume: want 58900000, got %d", sub.AvgVolume)
	}
}

func TestHandleTickString_LastTradeTime(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	want := time.Unix(1770000062, 0)
	c.handleTickString([]string{"46", "1", "7", "45", "1770000062"})

	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()

	if !sub.LastTradeTime.Equal(want) {
		t.Fatalf("LastTradeTime = %s, want %s", sub.LastTradeTime, want)
	}
	if !sub.Observed {
		t.Fatal("Observed not set after last-timestamp tick")
	}
	if sub.LastTime.IsZero() {
		t.Fatal("LastTime not updated after last-timestamp tick")
	}
	if !sub.LastPriceTickAt.IsZero() {
		t.Fatalf("LastPriceTickAt = %s after a last-trade-time tick; no price was observed", sub.LastPriceTickAt)
	}
}

func TestHandleTickString_RTVolumeUpdatesVolume(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	wantTime := time.UnixMilli(1770000062000)
	c.handleTickString([]string{"46", "1", "7", "233", "203.14;100;1770000062000;41762007;202.98;true"})

	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()

	if sub.LastPrice != 203.14 {
		t.Fatalf("LastPrice = %.2f, want 203.14", sub.LastPrice)
	}
	if sub.Volume != 41762007 {
		t.Fatalf("Volume = %d, want 41762007", sub.Volume)
	}
	if !sub.LastTradeTime.Equal(wantTime) {
		t.Fatalf("LastTradeTime = %s, want %s", sub.LastTradeTime, wantTime)
	}
	if !sub.Observed {
		t.Fatal("Observed not set after RTVolume tick")
	}
	if sub.LastPriceTickAt.IsZero() {
		t.Fatal("LastPriceTickAt not set after RTVolume supplied a positive last price")
	}
}

func TestParseRTVolumeTickNormalizesDecimalVolume(t *testing.T) {
	last, volume, ts, ok := parseRTVolumeTick("203.14;100;1770000062000;41762007966821;202.98;true", minServerVerSizeRules)
	if !ok {
		t.Fatal("parseRTVolumeTick returned ok=false")
	}
	if last != 203.14 {
		t.Fatalf("last = %.2f, want 203.14", last)
	}
	if volume != 41762007 {
		t.Fatalf("volume = %d, want 41762007", volume)
	}
	if want := time.UnixMilli(1770000062000); !ts.Equal(want) {
		t.Fatalf("time = %s, want %s", ts, want)
	}
}

func TestParseTickSize_NormalizesDecimalEncodedVolume(t *testing.T) {
	t.Parallel()

	got, ok := parseTickSize(minServerVerSizeRules, 8, "41762007966821")
	if !ok {
		t.Fatal("parseTickSize returned !ok")
	}
	if got != 41762007 {
		t.Fatalf("decimal-encoded volume = %d, want 41762007", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules, 74, "41762007966821")
	if !ok {
		t.Fatal("parseTickSize delayed returned !ok")
	}
	if got != 41762007 {
		t.Fatalf("decimal-encoded delayed volume = %d, want 41762007", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules, 8, "166")
	if !ok {
		t.Fatal("parseTickSize small returned !ok")
	}
	if got != 166 {
		t.Fatalf("small integer volume = %d, want 166", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules, 8, "30362805.851506")
	if !ok {
		t.Fatal("parseTickSize dotted volume returned !ok")
	}
	if got != 30362805 {
		t.Fatalf("dotted Decimal volume = %d, want 30362805", got)
	}

	got, ok = parseTickSize(minServerVerSizeRules-1, 8, "9876543")
	if !ok {
		t.Fatal("parseTickSize legacy returned !ok")
	}
	if got != 9876543 {
		t.Fatalf("legacy volume = %d, want 9876543", got)
	}
}

// TestHandleTickSize_OpenInterest pins tick types 27 (callOpenInterest) and
// 28 (putOpenInterest) into Subscription.OpenInt only when the tick matches
// the subscribed option right. The gateway also emits a zero-valued companion
// tick for the opposite right, which must not overwrite the real observation.
//
// The zero-gamma RPC depends on this — without the parser, the field stays
// at zero and the dealer GEX integral collapses silently.
func TestHandleTickSize_OpenInterest(t *testing.T) {
	for _, tt := range []struct {
		name     string
		right    string
		ticks    [][2]string
		want     int64
		observed bool
	}{
		{
			name:     "call_companion_zero_does_not_overwrite",
			right:    "C",
			ticks:    [][2]string{{"27", "6991"}, {"28", "0"}},
			want:     6991,
			observed: true,
		},
		{
			name:     "put_companion_zero_does_not_overwrite",
			right:    "P",
			ticks:    [][2]string{{"28", "6049"}, {"27", "0"}},
			want:     6049,
			observed: true,
		},
		{
			name:  "call_leg_ignores_put_tick_alone",
			right: "C",
			ticks: [][2]string{{"28", "777"}},
		},
		{
			name:  "put_leg_ignores_call_tick_alone",
			right: "P",
			ticks: [][2]string{{"27", "888"}},
		},
		{
			name:  "right_less_subscription_drops_aggregate_ticks",
			ticks: [][2]string{{"27", "999"}, {"28", "1000"}},
		},
		{
			name:     "matching_zero_is_observed",
			right:    "C",
			ticks:    [][2]string{{"27", "0"}},
			observed: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConnector(&ConnectorConfig{})
			key := "SPX 20260619 5000 " + tt.right
			c.subMu.Lock()
			c.reqIDMap[11] = key
			c.subscriptions[key] = &Subscription{Symbol: key, Right: tt.right}
			c.subMu.Unlock()

			for _, tick := range tt.ticks {
				c.handleTickSize([]string{"2", "6", "11", tick[0], tick[1]})
			}

			c.subMu.RLock()
			sub := c.subscriptions[key]
			c.subMu.RUnlock()

			if sub.OpenInt != tt.want {
				t.Errorf("OpenInt: want %d, got %d", tt.want, sub.OpenInt)
			}
			if sub.OpenIntObserved != tt.observed {
				t.Errorf("OpenIntObserved: want %t, got %t", tt.observed, sub.OpenIntObserved)
			}
			if !sub.Observed {
				t.Errorf("Observed flag not set after OI ticks")
			}

			// Round-trip: prove the OI also surfaces via MarketDataSnapshot,
			// which is the path Phase 2 (zero-gamma) reads from. Without
			// this, a regression on the connector→MarketData copy would
			// silently zero out every leg's OI in the GEX integral.
			md := c.MarketDataSnapshot()
			if md[key] == nil {
				t.Fatalf("MarketDataSnapshot missing entry for %q", key)
			}
			if md[key].OpenInt != tt.want {
				t.Errorf("MarketData.OpenInt: want %d, got %d", tt.want, md[key].OpenInt)
			}
			if md[key].OpenIntObserved != tt.observed {
				t.Errorf("MarketData.OpenIntObserved: want %t, got %t", tt.observed, md[key].OpenIntObserved)
			}
		})
	}
}

func TestHandleTickSize_OpenInterestAcceptsDecimalPayload(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	key := "SPY 20260602 758 P"
	c.subMu.Lock()
	c.reqIDMap[11] = key
	c.subscriptions[key] = &Subscription{Symbol: key, Right: "P"}
	c.subMu.Unlock()

	c.handleTickSize([]string{"2", "6", "11", "28", "2180.0"})

	md := c.MarketDataSnapshot()
	if md[key] == nil {
		t.Fatalf("MarketDataSnapshot missing entry for %q", key)
	}
	if md[key].OpenInt != 2180 {
		t.Fatalf("decimal OpenInt = %d, want 2180", md[key].OpenInt)
	}
	if !md[key].OpenIntObserved {
		t.Fatalf("decimal OpenInt should be observed")
	}
}

// TestHandleTickPrice_WeekRangeCapture pins the new tick types added in
// v0.12 for scan-row enrichment. 13W/26W/52W highs/lows arrive as
// standard tickPrice messages (msg ID 1) with tick types 15-20; capture
// is load-bearing for the scanner's 52w column. A previous build silently
// dropped these into the default branch, so an absent test made the
// regression invisible.
func TestHandleTickPrice_WeekRangeCapture(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[42] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	cases := []struct {
		tickType string
		price    string
		field    func(*Subscription) float64
		want     float64
		name     string
	}{
		{"15", "150.10", func(s *Subscription) float64 { return s.Week13Low }, 150.10, "13w low"},
		{"16", "240.55", func(s *Subscription) float64 { return s.Week13High }, 240.55, "13w high"},
		{"17", "140.00", func(s *Subscription) float64 { return s.Week26Low }, 140.00, "26w low"},
		{"18", "245.00", func(s *Subscription) float64 { return s.Week26High }, 245.00, "26w high"},
		{"19", "120.00", func(s *Subscription) float64 { return s.Week52Low }, 120.00, "52w low"},
		{"20", "260.50", func(s *Subscription) float64 { return s.Week52High }, 260.50, "52w high"},
	}
	for _, tc := range cases {
		c.handleTickPrice([]string{"1", "2", "42", tc.tickType, tc.price})
	}
	c.subMu.RLock()
	sub := c.subscriptions["AAPL"]
	c.subMu.RUnlock()
	for _, tc := range cases {
		if got := tc.field(sub); got != tc.want {
			t.Errorf("%s: got %.2f, want %.2f", tc.name, got, tc.want)
		}
	}
}

// TestHandleTickGeneric_IVRoutesToSubscription pins the v0.12 change that
// also writes the averaged option implied vol (wire tick 24, requested
// via generic tick 106) into the per-symbol subscription struct — not
// just into the optIV map. Without this, the scan-row IV column would
// stay blank even when the gateway delivers the tick, because the
// enrichment path reads from MarketDataSnapshot() which is derived from
// subscriptions.
func TestHandleTickGeneric_IVRoutesToSubscription(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[7] = "AAPL"
	c.subscriptions["AAPL"] = &Subscription{Symbol: "AAPL"}
	c.subMu.Unlock()

	// Fraction-form: 0.234 → IV = 0.234 (23.4%)
	c.handleTickGeneric([]string{"45", "1", "7", "24", "0.234"})
	c.subMu.RLock()
	if got := c.subscriptions["AAPL"].IV; got != 0.234 {
		t.Errorf("fraction-form IV: got %.4f, want 0.234", got)
	}
	c.subMu.RUnlock()

	// Percent-form: 23.4 should normalize to 0.234.
	c.subMu.Lock()
	c.reqIDMap[8] = "MSFT"
	c.subscriptions["MSFT"] = &Subscription{Symbol: "MSFT"}
	c.subMu.Unlock()
	c.handleTickGeneric([]string{"45", "1", "8", "24", "23.4"})
	c.subMu.RLock()
	got := c.subscriptions["MSFT"].IV
	c.subMu.RUnlock()
	if got < 0.233 || got > 0.235 {
		t.Errorf("percent-form IV normalization: got %.4f, want ~0.234", got)
	}
}

func TestHandleTickSizeShortableSharesRoutesToMarketData(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.reqIDMap[9] = "CRWV"
	c.subscriptions["CRWV"] = &Subscription{Symbol: "CRWV"}
	c.subMu.Unlock()

	// Wire tick 89 (tickSize) carries the share count for the
	// generic-tick-236 request.
	c.handleTickSize([]string{"2", "1", "9", "89", "750"})
	md := c.MarketDataSnapshot()["CRWV"]
	if md == nil {
		t.Fatal("market data missing for CRWV")
	}
	if !md.ShortableObserved || md.ShortableShares != 750 {
		t.Fatalf("shortable tick = observed %v shares %d, want true/750", md.ShortableObserved, md.ShortableShares)
	}
	if md.ShortableTickAt.IsZero() {
		t.Fatal("shortable tick observation time missing from MarketDataSnapshot")
	}
	shortableAt := md.ShortableTickAt
	c.handleTickSize([]string{"2", "1", "9", "0", "1200"})
	if got := c.MarketDataSnapshot()["CRWV"].ShortableTickAt; !got.Equal(shortableAt) {
		t.Fatalf("ShortableTickAt advanced on a bid-size tick: got %s want %s", got, shortableAt)
	}
}

// TestMarketDataSnapshot_SurfacesWeekRangeAndIV pins the daemon-facing read
// path: scan-row enrichment polls MarketDataSnapshot() and copies into the
// row. If a future refactor accidentally drops the new fields from the
// MarketData materialisation, this test catches it before the column
// silently goes blank.
func TestMarketDataSnapshot_SurfacesWeekRangeAndIV(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.subMu.Lock()
	c.subscriptions["AAPL"] = &Subscription{
		Symbol:        "AAPL",
		Week52Low:     120.00,
		Week52High:    260.50,
		Week26Low:     140.00,
		Week26High:    245.00,
		Week13Low:     150.10,
		Week13High:    240.55,
		AvgVolume:     58900000,
		LastTradeTime: time.Unix(1770000062, 0),
		IV:            0.234,
	}
	c.subMu.Unlock()

	md := c.MarketDataSnapshot()
	got, ok := md["AAPL"]
	if !ok {
		t.Fatal("AAPL not in MarketDataSnapshot() output")
	}
	if got.Week52Low != 120.00 || got.Week52High != 260.50 {
		t.Errorf("52w range: got %.2f..%.2f, want 120.00..260.50", got.Week52Low, got.Week52High)
	}
	if got.Week26Low != 140.00 || got.Week26High != 245.00 {
		t.Errorf("26w range: got %.2f..%.2f, want 140.00..245.00", got.Week26Low, got.Week26High)
	}
	if got.Week13Low != 150.10 || got.Week13High != 240.55 {
		t.Errorf("13w range: got %.2f..%.2f, want 150.10..240.55", got.Week13Low, got.Week13High)
	}
	if got.IV != 0.234 {
		t.Errorf("IV: got %.4f, want 0.234", got.IV)
	}
	if got.AvgVolume != 58900000 {
		t.Errorf("AvgVolume: got %d, want 58900000", got.AvgVolume)
	}
	if want := time.Unix(1770000062, 0); !got.LastTradeTime.Equal(want) {
		t.Errorf("LastTradeTime: got %s, want %s", got.LastTradeTime, want)
	}
}
