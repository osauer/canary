package ibkr

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// syntheticBondContract is a synthetic bill line's order contract.
func syntheticBondContract() Contract {
	return Contract{ConID: 880001, Symbol: "SYNTHB", SecType: "BOND", Exchange: "SMART", Currency: "USD"}
}

// The grid comes from a complete frame; a frame without size rules or a
// minimum tick cannot be ordered, and sizes must be whole order units.
func TestBondOrderRulesFromDetails(t *testing.T) {
	frame := syntheticBondFrame(7, "880001", "912797ZZ3", "US912797ZZ37", "USD", "20261126", "20260827")
	d, _, err := decodeBond(frame, 7)
	if err != nil {
		t.Fatalf("frame refused: %v", err)
	}
	rules, err := BondOrderRulesFrom(d)
	if err != nil || rules.MinTick != 0.0001 || rules.MinSize != 1000 || rules.SizeIncrement != 1000 || rules.Step() != 1000 || rules.Minimum() != 1000 {
		t.Fatalf("rules = %+v err %v", rules, err)
	}
	d.Complete = false
	if _, err := BondOrderRulesFrom(d); err == nil || !strings.Contains(err.Error(), "minimum size") {
		t.Fatalf("an incomplete frame gave rules: %v", err)
	}
	for name, r := range map[string]BondOrderRules{
		"no tick":            {MinSize: 1, SizeIncrement: 1},
		"no minimum":         {MinTick: 0.01, SizeIncrement: 1},
		"fractional minimum": {MinTick: 0.01, MinSize: 0.5},
		"fractional step":    {MinTick: 0.01, MinSize: 1, SizeIncrement: 0.25},
		"negative step":      {MinTick: 0.01, MinSize: 1, SizeIncrement: -1},
	} {
		if r.Validate() == nil {
			t.Errorf("%s: rules accepted", name)
		}
	}
	// No increment: the minimum size is the step.
	if r := (BondOrderRules{MinTick: 0.01, MinSize: 5}); r.Step() != 5 || r.CheckQuantity(10) != nil || r.CheckQuantity(12) == nil {
		t.Fatal("the minimum size is not the step when no increment is named")
	}
}

// Quantity must reach the minimum and sit on the size step; price must be
// positive and on the minimum tick, float noise aside.
func TestBondOrderRulesCheckQuantityAndPrice(t *testing.T) {
	r := BondOrderRules{MinTick: 0.0001, MinSize: 1000, SizeIncrement: 1000}
	for q, ok := range map[int]bool{1000: true, 60000: true, 999: false, 1500: false, 0: false, -1000: false} {
		if (r.CheckQuantity(q) == nil) != ok {
			t.Errorf("quantity %d accepted = %v", q, !ok)
		}
	}
	for p, ok := range map[float64]bool{99.1234: true, 99.12: true, 100: true, 99.12345: false, 0: false, -99.5: false} {
		if (r.CheckPrice(p) == nil) != ok {
			t.Errorf("price %v accepted = %v", p, !ok)
		}
	}
	// A 1/256 tick keeps its binary-exact grid.
	fine := BondOrderRules{MinTick: 1.0 / 256, MinSize: 1, SizeIncrement: 1}
	if fine.CheckPrice(99+5.0/256) != nil || fine.CheckPrice(99.01) == nil {
		t.Fatal("1/256 tick grid")
	}
	if got := BondTickFloor(99.12345, 0.0001); got != 99.1234 {
		t.Fatalf("floor = %v", got)
	}
	if got := BondTickCeil(99.12341, 0.0001); got != 99.1235 {
		t.Fatalf("ceil = %v", got)
	}
	// A wire value a hair under the grid is on it, not a tick lower.
	if got := BondTickFloor(99.12999999999, 0.01); got != 99.13 {
		t.Fatalf("floor of float noise = %v", got)
	}
	if got := BondTickNearest(99+5.0/256+1e-9, 1.0/256); got != 99+5.0/256 {
		t.Fatalf("nearest = %v", got)
	}
}

// NewBondLimitOrder builds one DAY limit order with the grid attached and
// refuses anything off it at construction.
func TestNewBondLimitOrder(t *testing.T) {
	rules := BondOrderRules{MinTick: 0.0001, MinSize: 1, SizeIncrement: 1}
	contract, order, err := NewBondLimitOrder(syntheticBondContract(), rules, "buy", 60, 99.6)
	if err != nil {
		t.Fatal(err)
	}
	if contract.SecType != "BOND" || contract.ConID != 880001 || contract.BondRules == nil || *contract.BondRules != rules ||
		order.Action != "BUY" || order.TotalQty != 60 || order.OrderType != "LMT" || order.TIF != "DAY" || !order.LmtPriceSet || order.LmtPrice != 99.6 {
		t.Fatalf("contract %+v order %+v", contract, order)
	}
	for name, build := range map[string]func() error{
		"off the tick": func() error {
			_, _, err := NewBondLimitOrder(syntheticBondContract(), rules, "BUY", 60, 99.60005)
			return err
		},
		"below the minimum": func() error {
			_, _, err := NewBondLimitOrder(syntheticBondContract(), BondOrderRules{MinTick: 0.01, MinSize: 1000, SizeIncrement: 1000}, "BUY", 500, 99.6)
			return err
		},
		"off the size step": func() error {
			_, _, err := NewBondLimitOrder(syntheticBondContract(), BondOrderRules{MinTick: 0.01, MinSize: 1000, SizeIncrement: 1000}, "BUY", 1500, 99.6)
			return err
		},
		"no contract id": func() error {
			c := syntheticBondContract()
			c.ConID = 0
			_, _, err := NewBondLimitOrder(c, rules, "BUY", 60, 99.6)
			return err
		},
		"no grid": func() error {
			_, _, err := NewBondLimitOrder(syntheticBondContract(), BondOrderRules{}, "BUY", 60, 99.6)
			return err
		},
		"no side": func() error {
			_, _, err := NewBondLimitOrder(syntheticBondContract(), rules, "HOLD", 60, 99.6)
			return err
		},
	} {
		if build() == nil {
			t.Errorf("%s: built", name)
		}
	}
}

// ValidateOrder refuses every BOND order that is not a LMT DAY order on a
// contract id carrying its grid, whichever encoder would send it; the
// protobuf encoder admits the one shape and writes it by contract id.
func TestBondOrderValidationAndProtoEncoding(t *testing.T) {
	rules := BondOrderRules{MinTick: 0.0001, MinSize: 1, SizeIncrement: 1}
	good := func() *IBKROrder {
		return &IBKROrder{OrderID: 21, ClientID: 9, ConID: 880001, Symbol: "SYNTHB", SecType: "BOND", Exchange: "SMART", Currency: "USD",
			Action: "BUY", TotalQty: 60, OrderType: "LMT", LmtPrice: 99.6, LmtPriceSet: true, TIF: "DAY", OpenClose: "O", BondRules: &rules}
	}
	if err := ValidateOrder(good()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*IBKROrder){
		"no grid":        func(o *IBKROrder) { o.BondRules = nil },
		"GTC":            func(o *IBKROrder) { o.TIF = "GTC" },
		"a trail":        func(o *IBKROrder) { o.OrderType, o.LmtPrice, o.TrailStopPrice, o.TrailingPercent = "TRAIL", 0, 99, 1 },
		"off the tick":   func(o *IBKROrder) { o.LmtPrice = 99.60005 },
		"no contract id": func(o *IBKROrder) { o.ConID = 0 },
		"a multiplier":   func(o *IBKROrder) { o.Multiplier = "100" },
		"no currency":    func(o *IBKROrder) { o.Currency = "" },
	} {
		o := good()
		mutate(o)
		if err := ValidateOrder(o); err == nil {
			t.Errorf("%s: ValidateOrder accepted", name)
		}
		if _, err := encodePlaceOrderProtoBody(o); err == nil {
			t.Errorf("%s: the protobuf encoder accepted", name)
		}
	}
	body, err := encodePlaceOrderProtoBody(good())
	if err != nil {
		t.Fatal(err)
	}
	summary, err := parsePlaceOrderProtoSummary(body)
	if err != nil || summary.conID != 880001 || summary.secType != "BOND" || summary.quantity != "60" || summary.orderType != "LMT" ||
		summary.tif != "DAY" || summary.lmtPrice != 99.6 || summary.action != "BUY" {
		t.Fatalf("summary = %+v err %v", summary, err)
	}
	// A bill goes as BILL: the encoder admits it on the same terms.
	bill := good()
	bill.SecType = "BILL"
	if body, err = encodePlaceOrderProtoBody(bill); err != nil {
		t.Fatal(err)
	}
	if summary, err = parsePlaceOrderProtoSummary(body); err != nil || summary.secType != "BILL" {
		t.Fatalf("bill summary = %+v err %v", summary, err)
	}
	bill.BondRules = nil
	if err := ValidateOrder(bill); err == nil {
		t.Fatal("a BILL order without its grid was accepted")
	}
	c, _, err := NewBondLimitOrder(Contract{ConID: 880001, Symbol: "SYNTHB", SecType: "bill", Currency: "USD"}, rules, "BUY", 1, 99.6)
	if err != nil || c.SecType != "BILL" {
		t.Fatalf("bill order contract = %+v err %v", c, err)
	}
	if c, _, err = NewBondLimitOrder(Contract{ConID: 880001, Symbol: "SYNTHB", SecType: "STK", Currency: "USD"}, rules, "BUY", 1, 99.6); err != nil || c.SecType != "BOND" {
		t.Fatalf("bond order contract = %+v err %v", c, err)
	}
	// The WhatIf and submit builders carry the grid from the contract.
	if cloneBondOrderRules(nil) != nil || *cloneBondOrderRules(&rules) != rules {
		t.Fatal("clone")
	}
}

// IBKR's hours list: dated and undated closes, CLOSED days, several spans a
// day and an overnight span; anything malformed refuses the whole list.
func TestParseTradingHours(t *testing.T) {
	windows, err := ParseTradingHours("US/Eastern", "20261001:0800-20261001:1700;20261002:0800-1200,1300-1700;20261003:CLOSED;20261004:1800-0400")
	if err != nil {
		t.Fatal(err)
	}
	ny, _ := time.LoadLocation("America/New_York")
	want := []TradingWindow{
		{time.Date(2026, 10, 1, 8, 0, 0, 0, ny).UTC(), time.Date(2026, 10, 1, 17, 0, 0, 0, ny).UTC()},
		{time.Date(2026, 10, 2, 8, 0, 0, 0, ny).UTC(), time.Date(2026, 10, 2, 12, 0, 0, 0, ny).UTC()},
		{time.Date(2026, 10, 2, 13, 0, 0, 0, ny).UTC(), time.Date(2026, 10, 2, 17, 0, 0, 0, ny).UTC()},
		{time.Date(2026, 10, 4, 18, 0, 0, 0, ny).UTC(), time.Date(2026, 10, 5, 4, 0, 0, 0, ny).UTC()},
	}
	if !slices.Equal(windows, want) {
		t.Fatalf("windows = %v", windows)
	}
	for _, bad := range []string{"20261001", "2026100:0800-1700", "20261001:0800", "20261001:8-17", "20261001:0800-20261001:0700"} {
		if _, err := ParseTradingHours("US/Eastern", bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := ParseTradingHours("Not/AZone", "20261001:0800-1700"); err == nil {
		t.Fatal("an unknown zone parsed")
	}
	// Liquid hours first, trading hours next, nothing from an incomplete
	// frame.
	d := BondContractDetails{Complete: true, TimeZoneID: "Europe/Berlin", TradingHours: "20261001:0800-2200", LiquidHours: "20261001:0900-1730"}
	got, source, ok := d.SessionWindows()
	if !ok || source != BondHoursLiquid || len(got) != 1 || got[0].Open.Hour() != 7 {
		t.Fatalf("liquid = %v %s %v", got, source, ok)
	}
	d.LiquidHours = ""
	if _, source, ok := d.SessionWindows(); !ok || source != BondHoursTrading {
		t.Fatal("trading hours fallback")
	}
	d.Complete = false
	if _, _, ok := d.SessionWindows(); ok {
		t.Fatal("an incomplete frame gave hours")
	}
}

// A bond quote on the exact order session asks for no generic ticks: the
// stock and option lists are not defined for bonds.
func TestExactSessionBondQuoteAsksForNoGenericTicks(t *testing.T) {
	conn, c, socket, _, _ := newQueuedInstructionReconnectFixture(t)
	binding, _ := c.CaptureSession()
	if _, err := c.SubscribeMarketDataWithContractForSession(context.Background(), binding, syntheticBondContract(), nil); err != nil {
		t.Fatal(err)
	}
	frames := decodeOutboundFrames(t, conn, socket.Bytes())
	var sent []string
	for _, f := range frames {
		if slices.Contains(f, "BOND") {
			sent = f
		}
	}
	if sent == nil || slices.ContainsFunc(sent, func(field string) bool { return strings.Contains(field, OptionSubscriptionGenericTicks) }) {
		t.Fatalf("bond request frame = %v", sent)
	}
	if _, err := c.BondContractDetailsForSession(context.Background(), ConnectorSessionBinding{}, BondContractRequest{ConID: 880001, Currency: "USD"}, time.Second); err == nil {
		t.Fatal("a stale session binding read contract details")
	}
}
