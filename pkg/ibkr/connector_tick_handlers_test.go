package ibkr

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTickFixture is a Connector with one stock line (request 7) and one
// option line (request 8), the shape the gateway's tick frames are keyed by.
func newTickFixture(t *testing.T) (*Connector, *Subscription, *Subscription) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(c.conn.rateLimiter.Stop)
	stock := &Subscription{Symbol: "SYNX", ReqID: 7}
	option := &Subscription{Symbol: "SYNX_261120C100", ReqID: 8, Right: "C"}
	c.subscriptions["SYNX"] = stock
	c.subscriptions["SYNX_261120C100"] = option
	c.reqIDMap[7] = "SYNX"
	c.reqIDMap[8] = "SYNX_261120C100"
	c.optReqIDs[8] = "SYNX_261120C100"
	return c, stock, option
}

func tickPrice(reqID, tick int, price string) []string {
	return []string{"1", "6", strconv.Itoa(reqID), strconv.Itoa(tick), price, "0", "1"}
}

// Every tick-price type the gateway sends for a stock line lands in its own
// field; a non-positive price records the receipt but never overwrites a
// quote; statistics (week ranges) never count as a quote.
func TestTickPriceRoutesEachTickTypeToItsField(t *testing.T) {
	c, stock, _ := newTickFixture(t)
	for tick, want := range map[int]func() float64{
		1: func() float64 { return stock.Bid }, 2: func() float64 { return stock.Ask }, 4: func() float64 { return stock.LastPrice },
		6: func() float64 { return stock.High }, 7: func() float64 { return stock.Low }, 9: func() float64 { return stock.PrevClose },
		14: func() float64 { return stock.Open }, 15: func() float64 { return stock.Week13Low }, 16: func() float64 { return stock.Week13High },
		17: func() float64 { return stock.Week26Low }, 18: func() float64 { return stock.Week26High }, 19: func() float64 { return stock.Week52Low },
		20: func() float64 { return stock.Week52High }, 37: func() float64 { return stock.MarkPrice },
	} {
		price := 100 + float64(tick)
		c.handleTickPrice(tickPrice(7, tick, strconv.FormatFloat(price, 'f', -1, 64)))
		if got := want(); got != price {
			t.Fatalf("tick %d stored %v, want %v", tick, got, price)
		}
	}
	if stock.LastPriceTickAt.IsZero() || stock.LastTickAt.IsZero() || !stock.Observed || stock.BidAt.IsZero() || stock.MarkAt.IsZero() {
		t.Fatalf("quote ticks did not stamp the line: %+v", stock)
	}
	// Delayed types feed the same fields and mark the line delayed.
	for tick, want := range map[int]func() float64{
		66: func() float64 { return stock.Bid }, 67: func() float64 { return stock.Ask }, 68: func() float64 { return stock.LastPrice },
		72: func() float64 { return stock.High }, 73: func() float64 { return stock.Low }, 75: func() float64 { return stock.PrevClose }, 76: func() float64 { return stock.Open },
	} {
		c.handleTickPrice(tickPrice(7, tick, "55.5"))
		if got := want(); got != 55.5 {
			t.Fatalf("delayed tick %d stored %v", tick, got)
		}
	}
	if !stock.delayedTicks || c.MarketDataTypeForSymbol("synx") != 3 {
		t.Fatalf("delayed ticks did not mark the line delayed: delayed=%t type=%d", stock.delayedTicks, c.MarketDataTypeForSymbol("synx"))
	}

	before := stock.LastPriceTickAt
	bid := stock.Bid
	time.Sleep(time.Millisecond)
	for _, raw := range []string{"0", "-1", "NaN", "Inf"} {
		c.handleTickPrice(tickPrice(7, 1, raw))
	}
	if stock.Bid != bid || stock.LastPriceTickAt != before {
		t.Fatalf("a no-quote price overwrote the bid: %v (was %v)", stock.Bid, bid)
	}
	if stock.LastTickAt.Equal(before) {
		t.Fatal("a no-quote tick was not recorded as a receipt")
	}
	// A blank price is a stale-tick marker: a receipt, no quote.
	last := stock.LastTickAt
	time.Sleep(time.Millisecond)
	c.handleTickPrice(tickPrice(7, 1, ""))
	if !stock.LastTickAt.After(last) || stock.Bid != bid {
		t.Fatal("blank price tick mishandled")
	}
	// Unknown tick types are recorded without a field; auction data is informational.
	c.handleTickPrice(tickPrice(7, 225, "12"))
	c.handleTickPrice(tickPrice(7, 221, "12"))
	c.handleTickPrice(tickPrice(7, 232, "12"))
	c.handleTickPrice(tickPrice(7, 233, "12"))
	c.handleTickPrice(tickPrice(7, 500, "12"))
	if stock.Bid != bid {
		t.Fatal("an unmapped tick type changed the bid")
	}
}

// Frames the gateway would never send for a live line are dropped without
// touching any subscription: a zero or non-numeric request id, a request id
// of another session, a non-numeric tick type or price, and a frame too
// short to carry a price.
func TestTickPriceIgnoresMalformedAndForeignFrames(t *testing.T) {
	c, stock, _ := newTickFixture(t)
	for _, fields := range [][]string{
		{"1", "6", "7"},
		{"1", "6", "7", "1"},
		tickPrice(0, 1, "10"),
		{"1", "6", "x", "1", "10"},
		{"1", "6", "7", "x", "10"},
		{"1", "6", "9", "x", "10"},
		{"1", "6", "7", "1", "ten"},
		{"1", "6", "9", "1", "ten"},
		tickPrice(6, 1, "10"),
		tickPrice(99, 1, "10"),
	} {
		c.handleTickPrice(fields)
	}
	if stock.Bid != 0 || !stock.LastTickAt.IsZero() {
		t.Fatalf("a malformed or foreign frame reached the line: %+v", stock)
	}
	// A request id that was remapped to another symbol since the line was
	// created no longer feeds it.
	c.reqIDMap[7] = "OTHER"
	c.handleTickPrice(tickPrice(7, 1, "10"))
	if stock.Bid != 0 {
		t.Fatal("a remapped request id fed the old line")
	}
}

// An option line keeps its own quote, previous close and mark, and feeds the
// option quote maps the risk reads use; a bond yield tick is a yield, never a
// price.
func TestTickPriceFeedsOptionAndBondLines(t *testing.T) {
	c, _, option := newTickFixture(t)
	c.handleTickPrice(tickPrice(8, 1, "2.5"))
	c.handleTickPrice(tickPrice(8, 2, "2.7"))
	c.handleTickPrice(tickPrice(8, 4, "2.6"))
	c.handleTickPrice(tickPrice(8, 9, "2.4"))
	c.handleTickPrice(tickPrice(8, 37, "2.61"))
	c.handleTickPrice(tickPrice(8, 66, "0"))
	if option.Bid != 2.5 || option.Ask != 2.7 || option.LastPrice != 2.6 || option.PrevClose != 2.4 || option.MarkPrice != 2.61 || !option.Observed {
		t.Fatalf("option line = %+v", option)
	}
	c.optMu.RLock()
	bid, ask, prev := c.optQuoteBid["SYNX_261120C100"], c.optQuoteAsk["SYNX_261120C100"], c.optPrevClose["SYNX_261120C100"]
	c.optMu.RUnlock()
	if bid != 2.5 || ask != 2.7 || prev != 2.4 {
		t.Fatalf("option quote maps: bid=%v ask=%v prev=%v", bid, ask, prev)
	}
	// An option request id without a subscription row still fills the maps.
	c.optReqIDs[9] = "ORPHAN"
	c.reqIDMap[9] = "ORPHAN"
	c.handleTickPrice(tickPrice(9, 2, "1.1"))
	c.optMu.RLock()
	orphan := c.optQuoteAsk["ORPHAN"]
	c.optMu.RUnlock()
	if orphan != 1.1 {
		t.Fatalf("orphan option ask = %v", orphan)
	}

	bond := &Subscription{Symbol: "SYNBOND", ReqID: 11}
	c.subscriptions["SYNBOND"] = bond
	c.reqIDMap[11] = "SYNBOND"
	c.handleTickPrice(tickPrice(11, tickBidYield, "4.25"))
	c.handleTickPrice(tickPrice(11, tickAskYield, "4.15"))
	c.handleTickPrice(tickPrice(11, tickLastYield, "-1"))
	if bond.bidYield == nil || *bond.bidYield != 4.25 || bond.askYield == nil || *bond.askYield != 4.15 || bond.lastYield != nil {
		t.Fatalf("bond yields: bid=%v ask=%v last=%v", bond.bidYield, bond.askYield, bond.lastYield)
	}
	if bond.Bid != 0 || bond.LastPrice != 0 || bond.LastTickAt.IsZero() {
		t.Fatalf("a yield tick was stored as a price: %+v", bond)
	}
}

func tickSize(reqID, tick int, size string) []string {
	return []string{"2", "6", strconv.Itoa(reqID), strconv.Itoa(tick), size}
}

// Sizes route by tick type; open interest commits only for the line's own
// right, since the gateway sends both 27 (call) and 28 (put) on one line.
func TestTickSizeRoutesByTypeAndRight(t *testing.T) {
	c, stock, option := newTickFixture(t)
	c.handleTickSize(tickSize(7, 0, "100"))
	c.handleTickSize(tickSize(7, 3, "200"))
	c.handleTickSize(tickSize(7, 8, "3000"))
	c.handleTickSize(tickSize(7, 21, "4000"))
	c.handleTickSize(tickSize(7, 89, "5000000"))
	if stock.BidSize != 100 || stock.AskSize != 200 || stock.Volume != 3000 || !stock.VolumeObserved || stock.AvgVolume != 4000 || stock.ShortableShares != 5000000 || !stock.ShortableObserved || stock.ShortableTickAt.IsZero() {
		t.Fatalf("stock sizes = %+v", stock)
	}
	c.handleTickSize(tickSize(7, 69, "110"))
	c.handleTickSize(tickSize(7, 70, "210"))
	c.handleTickSize(tickSize(7, 74, "3100"))
	if stock.BidSize != 110 || stock.AskSize != 210 || stock.Volume != 3100 {
		t.Fatalf("delayed sizes = %+v", stock)
	}
	c.handleTickSize(tickSize(8, 28, "999"))
	if option.OpenIntObserved || option.OpenInt != 0 {
		t.Fatal("put open interest committed to a call line")
	}
	c.handleTickSize(tickSize(8, 27, "777"))
	if !option.OpenIntObserved || option.OpenInt != 777 {
		t.Fatalf("call open interest = %d observed=%t", option.OpenInt, option.OpenIntObserved)
	}
	option.Right = "P"
	c.handleTickSize(tickSize(8, 28, "888"))
	if option.OpenInt != 888 {
		t.Fatalf("put open interest = %d", option.OpenInt)
	}
	for _, fields := range [][]string{{"2", "6", "7"}, {"2", "6", "7", "0"}, tickSize(7, 0, "-5"), tickSize(7, 0, "x"), tickSize(99, 0, "5"), tickSize(7, 500, "5")} {
		c.handleTickSize(fields)
	}
	if stock.BidSize != 110 {
		t.Fatal("a malformed size frame changed the bid size")
	}
	c.reqIDMap[12] = "GONE"
	c.handleTickSize(tickSize(12, 0, "5"))
}

func tickString(reqID, tick int, value string) []string {
	return []string{"46", "6", strconv.Itoa(reqID), strconv.Itoa(tick), value}
}

// Tick 45/88 carry the last trade time as Unix seconds; 233 (RTVolume)
// carries price, size, time and day volume; other string ticks are ignored.
func TestTickStringRecordsTradeTimesAndRTVolume(t *testing.T) {
	c, stock, _ := newTickFixture(t)
	c.handleTickString(tickString(7, 45, "1700000000"))
	if !stock.LastTradeTime.Equal(time.Unix(1700000000, 0)) || !stock.Observed || stock.LastPriceTickAt.IsZero() == false {
		t.Fatalf("tick 45: %+v", stock)
	}
	c.handleTickString(tickString(7, 88, "1700000100"))
	if !stock.LastTradeTime.Equal(time.Unix(1700000100, 0)) {
		t.Fatalf("tick 88: %v", stock.LastTradeTime)
	}
	c.handleTickString(tickString(7, 233, "101.25;100;1700000200000;123456;101.2;true"))
	if stock.LastPrice != 101.25 || stock.Volume != 123456 || !stock.LastTradeTime.Equal(time.UnixMilli(1700000200000)) || stock.LastPriceTickAt.IsZero() {
		t.Fatalf("RTVolume: %+v", stock)
	}
	// A struck price of zero (no trade yet) keeps the last price.
	c.handleTickString(tickString(7, 233, "0;0;1700000300000;123457;101.2;true"))
	if stock.LastPrice != 101.25 || stock.Volume != 123457 {
		t.Fatalf("RTVolume without a trade: %+v", stock)
	}
	last := stock.LastTradeTime
	for _, fields := range [][]string{
		{"46", "6", "7", "45"},
		tickString(7, 45, ""),
		tickString(7, 45, "0"),
		tickString(7, 45, "x"),
		tickString(7, 233, "1;2"),
		tickString(7, 999, "1700000400"),
		tickString(99, 45, "1700000400"),
		{"46", "6", "x", "45", "1700000400"},
		{"46", "6", "7", "x", "1700000400"},
	} {
		c.handleTickString(fields)
	}
	if stock.LastTradeTime != last {
		t.Fatalf("a malformed string tick changed the trade time: %v", stock.LastTradeTime)
	}
	c.reqIDMap[7] = "SYNX"
	stock.ReqID = 70
	c.handleTickString(tickString(7, 45, "1700000500"))
	if stock.LastTradeTime != last {
		t.Fatal("a tick for a retired request id reached the line")
	}
	if _, _, _, ok := parseRTVolumeTick("1;2;3", 203); ok {
		t.Fatal("three-part RTVolume accepted")
	}
}

// Generic tick 24 is the chain-averaged implied volatility of the
// underlying; percent inputs are normalized and the per-contract model type
// is cleared because this tick is not a model computation.
func TestTickGenericStoresChainIV(t *testing.T) {
	c, stock, _ := newTickFixture(t)
	c.optIVDataType["SYNX"] = OptionModelDataTypeLive
	c.handleTickGeneric([]string{"45", "6", "7", "24", "32.5"})
	c.optMu.RLock()
	iv, typed := c.optIV["SYNX"], c.optIVDataType["SYNX"]
	_, hasType := c.optIVDataType["SYNX"]
	c.optMu.RUnlock()
	if iv != 0.325 || stock.IV != 0.325 || hasType || typed != 0 || stock.LastTickAt.IsZero() {
		t.Fatalf("iv=%v sub.iv=%v type present=%t", iv, stock.IV, hasType)
	}
	c.handleTickGeneric([]string{"45", "6", "7", "24", "0.41"})
	if stock.IV != 0.41 {
		t.Fatalf("fractional iv = %v", stock.IV)
	}
	for _, fields := range [][]string{{"45", "6", "7", "24"}, {"45", "6", "7", "24", "0"}, {"45", "6", "7", "46", "2"}, {"45", "6", "99", "24", "0.3"}} {
		c.handleTickGeneric(fields)
	}
	if stock.IV != 0.41 {
		t.Fatalf("a malformed generic tick changed iv: %v", stock.IV)
	}
}

func optionComputation(reqID, tick int, iv, delta, price, pvDiv, gamma, vega, theta, under string) []string {
	return []string{"21", strconv.Itoa(reqID), strconv.Itoa(tick), "0", iv, delta, price, pvDiv, gamma, vega, theta, under, ""}
}

// The model computation (13 live, 83 delayed) is the source of record for
// greeks and model IV; bid/ask computations carry only the option price.
// IBKR's sentinel rows (NaN, MaxFloat) never commit a greek.
func TestOptionComputationCommitsOnlySaneGreeks(t *testing.T) {
	c, _, _ := newTickFixture(t)
	key := "SYNX_261120C100"
	c.handleOptionComputation(optionComputation(8, 13, "0.35", "0.52", "2.6", "0", "0.03", "0.12", "-0.05", "101.5"))
	c.optMu.RLock()
	iv, typ, g, under := c.optIV[key], c.optIVDataType[key], c.optGreeks[key], c.optUnderlyingPx[key]
	c.optMu.RUnlock()
	if iv != 0.35 || typ != OptionModelDataTypeLive || g != (Greeks{Delta: 0.52, Gamma: 0.03, Theta: -0.05, Vega: 0.12}) || under != 101.5 {
		t.Fatalf("live model: iv=%v type=%d greeks=%+v under=%v", iv, typ, g, under)
	}
	c.handleOptionComputation(optionComputation(8, 83, "36", "NaN", "2.6", "0", "1.7976931348623157e308", "0.13", "NaN", "0"))
	c.optMu.RLock()
	iv, typ, g, under = c.optIV[key], c.optIVDataType[key], c.optGreeks[key], c.optUnderlyingPx[key]
	c.optMu.RUnlock()
	if iv != 0.36 || typ != OptionModelDataTypeDelayed || g.Delta != 0.52 || g.Gamma != 0.03 || g.Theta != -0.05 || g.Vega != 0.13 || under != 101.5 {
		t.Fatalf("delayed model with sentinels: iv=%v type=%d greeks=%+v under=%v", iv, typ, g, under)
	}
	c.handleOptionComputation(optionComputation(8, 10, "0", "0", "2.55", "0", "0", "0", "0", "0"))
	c.handleOptionComputation(optionComputation(8, 81, "0", "0", "2.75", "0", "0", "0", "0", "0"))
	c.handleOptionComputation(optionComputation(8, 11, "0", "0", "-1", "0", "0", "0", "0", "0"))
	c.optMu.RLock()
	bid, ask := c.optQuoteBid[key], c.optQuoteAsk[key]
	c.optMu.RUnlock()
	if bid != 2.55 || ask != 2.75 {
		t.Fatalf("computed quote: bid=%v ask=%v", bid, ask)
	}
	// A row of pure sentinels for a new contract commits nothing.
	c.optReqIDs[9] = "SYNX_261120P100"
	c.handleOptionComputation(optionComputation(9, 13, "0", "NaN", "0", "0", "NaN", "NaN", "NaN", "NaN"))
	c.optMu.RLock()
	_, hasGreeks := c.optGreeks["SYNX_261120P100"]
	_, hasIV := c.optIV["SYNX_261120P100"]
	c.optMu.RUnlock()
	if hasGreeks || hasIV {
		t.Fatal("sentinel row committed greeks or iv")
	}
	for _, fields := range [][]string{
		{"21", "8", "13"},
		optionComputation(99, 13, "0.3", "0.5", "2", "0", "0", "0", "0", "0"),
		{"21", "x", "13", "0", "0.3", "0.5", "2", "0", "0", "0", "0", "100"},
		{"21", "8", "x", "0", "0.3", "0.5", "2", "0", "0", "0", "0", "100"},
	} {
		c.handleOptionComputation(fields)
	}
	if !saneGreek(0.5, 1.05) || saneGreek(math.NaN(), 1) || saneGreek(math.Inf(1), 1) || saneGreek(2, 1) {
		t.Fatal("saneGreek bounds")
	}
}

// mapIBOrderStatus is the broker-status to package-status mapping the order
// tracker depends on: partial fills are detected from the quantities for
// working statuses, a working order whose remaining quantity hit zero is
// filled, and unknown statuses pass through uppercased.
func TestMapIBOrderStatusUsesQuantitiesForWorkingOrders(t *testing.T) {
	cases := []struct {
		status            string
		filled, remaining float64
		want              OrderStatus
	}{
		{"PendingSubmit", 0, 10, OrderStatusPending}, {"ApiPending", 0, 10, OrderStatusPending},
		{"PreSubmitted", 0, 10, OrderStatusSubmitted}, {"PreSubmitted", 3, 7, OrderStatusPartial},
		{"Submitted", 0, 10, OrderStatusSubmitted}, {"Submitted", 3, 7, OrderStatusPartial}, {"Submitted", 10, 0, OrderStatusFilled},
		{"PendingCancel", 3, 7, OrderStatusPartial},
		{"PartiallyFilled", 1, 1, OrderStatusPartial}, {"Filled", 10, 0, OrderStatusFilled}, {"Completed", 10, 0, OrderStatusFilled},
		{"Cancelled", 0, 10, OrderStatusCancelled}, {"ApiCancelled", 0, 10, OrderStatusCancelled},
		{"Inactive", 0, 10, OrderStatusRejected}, {"Rejected", 0, 10, OrderStatusRejected}, {"Error", 0, 10, OrderStatusRejected},
		{"Expired", 0, 10, OrderStatusExpired},
		{"Unknown", 10, 0, OrderStatusFilled}, {"Unknown", 4, 6, OrderStatusPartial}, {"Unknown", 0, 10, OrderStatus("UNKNOWN")},
	}
	for _, tc := range cases {
		if got := mapIBOrderStatus(tc.status, tc.filled, tc.remaining); got != tc.want {
			t.Errorf("%s %v/%v = %s, want %s", tc.status, tc.filled, tc.remaining, got, tc.want)
		}
	}
	for _, s := range []OrderStatus{OrderStatusFilled, OrderStatusCancelled, OrderStatusRejected, OrderStatusExpired} {
		if !isTerminalOrderStatus(s) {
			t.Errorf("%s not terminal", s)
		}
	}
	for _, s := range []OrderStatus{OrderStatusNew, OrderStatusPending, OrderStatusSubmitted, OrderStatusPartial, OrderStatusAccepted} {
		if isTerminalOrderStatus(s) {
			t.Errorf("%s terminal", s)
		}
	}
}

// orderStatus frames reach the tracked order through the legacy layout
// (with and without the version field) and through the protobuf summary.
// Fills record the average price (or the last fill price when the average
// is absent) and a terminal status removes the order from the open set.
func TestOrderStatusUpdatesTrackedOrdersFromBothFrameShapes(t *testing.T) {
	c, _, _ := newTickFixture(t)
	track := func(id, brokerID string) *trackedOrder {
		o := &trackedOrder{ID: id, Status: OrderStatusSubmitted, Quantity: 10}
		c.openOrders[id] = o
		if brokerID != "" {
			c.brokerOrderIndex[brokerID] = id
		}
		return o
	}
	a := track("local-a", "501")
	// Legacy frame with version: [3, version, orderId, status, filled, remaining, avgFillPrice, permId, parentId, lastFillPrice, clientId, whyHeld, mktCapPrice]
	c.handleOrderStatus([]string{"3", "1", "501", "Submitted", "4", "6", "100.5", "9001", "0", "100.4", "15", "", "0"})
	if a.Status != OrderStatusPartial || a.FilledQty != 4 || a.FilledPrice != 100.5 || a.BrokerID != "501" || a.UpdatedAt.IsZero() {
		t.Fatalf("partial: %+v", a)
	}
	// Repeated frame: still tracked, nothing lost.
	c.handleOrderStatus([]string{"3", "1", "501", "Submitted", "4", "6", "100.5", "9001", "0", "100.4", "15", "", "0"})
	// Held reason is kept; a last-fill price stands in for a missing average.
	c.handleOrderStatus([]string{"3", "1", "501", "Submitted", "5", "5", "0", "9001", "0", "100.6", "15", "locate", "0"})
	if a.Reason != "locate" {
		t.Fatalf("held partial: %+v", a)
	}
	c.handleOrderStatus([]string{"3", "1", "501", "Filled", "10", "0", "100.7", "9001", "0", "100.7", "15", "", "0"})
	if a.Status != OrderStatusFilled || a.FilledAt == nil || a.FilledPrice != 100.7 {
		t.Fatalf("filled: %+v", a)
	}
	if _, open := c.openOrders["local-a"]; open {
		t.Fatal("filled order still open")
	}
	if _, indexed := c.brokerOrderIndex["501"]; indexed {
		t.Fatal("filled order still indexed")
	}

	// Frame without the version field, keyed directly by broker id.
	b := track("502", "")
	c.handleOrderStatus([]string{"3", "502", "Cancelled", "0", "10"})
	if b.Status != OrderStatusCancelled || b.CancelledAt == nil {
		t.Fatalf("cancelled: %+v", b)
	}
	if _, open := c.openOrders["502"]; open {
		t.Fatal("cancelled order still open")
	}

	// Protobuf summary, as the current gateway sends it.
	d := track("local-d", "503")
	proto := twsGatewayOrderStatus(twsGatewayOrder{Bound: 503, PermID: 9003, Quantity: 10, Filled: 10, Status: "Filled"})
	fields := c.conn.decodeMessage(proto)
	if len(fields) < 2 || fields[1] != "protobuf" {
		c.conn.serverVersion = minServerVerProtoBufPlaceOrder
		fields = c.conn.decodeMessage(proto)
	}
	c.handleOrderStatus(fields)
	if d.Status != OrderStatusFilled || d.FilledQty != 10 {
		t.Fatalf("proto filled: %+v", d)
	}

	// Frames that name no tracked order, or are too short, change nothing.
	e := track("local-e", "504")
	for _, fields := range [][]string{
		{"3", "1"},
		{"3", "1", "504"},
		{"3", "1", "999", "Filled", "10", "0"},
		{"3", "1", "504", "", "10", "0"},
		{"3", "1", "", "Filled", "10", "0"},
	} {
		c.handleOrderStatus(fields)
	}
	if e.Status != OrderStatusSubmitted {
		t.Fatalf("untouched order changed: %+v", e)
	}
	// The log dedupe forgets terminal orders so a reused id logs fresh.
	if !c.orderStatusChanged("504", "x") || c.orderStatusChanged("504", "x") {
		t.Fatal("order status log signature dedupe")
	}
	c.forgetOrderStatusLog("504")
	if !c.orderStatusChanged("504", "x") {
		t.Fatal("forgotten signature still deduped")
	}
	if !strings.Contains(orderStatusLogSignature("Filled", 1, 0), "Filled|1.0000|0.0000") {
		t.Fatal("signature shape")
	}
}

// The connector-level position and portfolio handlers only narrate; a short
// frame is ignored, a full one is accepted. They sit behind the Connection's
// own decoders, which hold the state.
func TestConnectorPositionAndPortfolioHandlersAcceptFullFrames(t *testing.T) {
	c, _, _ := newTickFixture(t)
	c.handlePosition([]string{"61", "3", "DU1234567"})
	c.handlePosition([]string{"61", "3", "DU1234567", "SYNX", "STK", "", "0", "", "1", "SMART", "USD", "SYNX", "NMS", "100", "50.5"})
	c.handlePortfolioValue([]string{"7", "SYNX"})
	c.handlePortfolioValue([]string{"7", "SYNX", "1", "2", "3", "4", "100", "51", "5100", "50", "100", "0", "0", "0", "0", "0", "0", "0", "DU1234567"})
}
