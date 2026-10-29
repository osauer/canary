package ibkr

import (
	"strings"
	"testing"
)

// The index hedge sells Micro E-mini S&P 500 futures, and Canary checks the
// futures permission with a WhatIf of one contract (owner decisions
// 2026-10-28 and 2026-10-29). The encoder refused every future, so both
// failed inside Canary before reaching IBKR (live 2026-10-29 07:53 CET:
// "protobuf placeOrder does not support secType=\"FUT\""). A future now
// encodes as a limit for the day in regular hours on its exact contract;
// every other futures shape is refused before the wire.
func TestPlaceOrderProtoIndexFuture(t *testing.T) {
	future := func() IBKROrder {
		return IBKROrder{ConID: 990001, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218",
			Multiplier: "5", LocalSymbol: "MESZ6", TradingClass: "MES",
			Action: "SELL", TotalQty: 2, OrderType: "LMT", LmtPrice: 6800.25, LmtPriceSet: true, TIF: "DAY"}
	}
	base := future()
	if err := validatePlaceOrderProtoSupported(&base); err != nil {
		t.Fatalf("a Micro S&P limit for the day was refused: %v", err)
	}
	frame, err := encodePlaceOrderProtoBody(&base)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	summary, err := parsePlaceOrderProtoSummary(frame)
	if err != nil {
		t.Fatal(err)
	}
	if summary.secType != "FUT" {
		t.Fatalf("encoded secType %q", summary.secType)
	}
	for _, want := range []string{"20261218", "MESZ6", "CME"} {
		if !strings.Contains(string(frame), want) {
			t.Errorf("frame lacks %q", want)
		}
	}
	for name, edit := range map[string]func(*IBKROrder){
		"trail":        func(o *IBKROrder) { o.OrderType = "TRAIL"; o.LmtPriceSet = false; o.AuxPrice = 5 },
		"GTC":          func(o *IBKROrder) { o.TIF = "GTC" },
		"outside RTH":  func(o *IBKROrder) { o.OutsideRth = true },
		"no contract":  func(o *IBKROrder) { o.ConID = 0 },
		"no expiry":    func(o *IBKROrder) { o.Expiry = "" },
		"no multipler": func(o *IBKROrder) { o.Multiplier = "" },
		"right":        func(o *IBKROrder) { o.Right = "C" },
		"strike":       func(o *IBKROrder) { o.Strike = 6000 },
		"no exchange":  func(o *IBKROrder) { o.Exchange = "" },
	} {
		o := future()
		edit(&o)
		if err := validatePlaceOrderProtoSupported(&o); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}

// The daemon hands the connector a Contract and a RawOrder; the future's
// multiplier is an int there. Both together must reach the wire, since the
// hedge's own tests stand in for the broker call and never got this far.
func TestPlaceOrderProtoIndexFutureFromContractAndRawOrder(t *testing.T) {
	contract := &Contract{ConID: 990001, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218",
		LocalSymbol: "MESZ6", TradingClass: "MES", Multiplier: 5}
	for _, action := range []string{"SELL", "BUY"} {
		raw := &RawOrder{Action: action, TotalQty: 1, OrderType: "LMT", LmtPrice: 3400, TIF: "DAY", OrderRef: "canary-permission-check"}
		order := rawIBKROrder(contract, raw)
		if err := ValidateOrder(order); err != nil {
			t.Fatalf("%s: ValidateOrder: %v", action, err)
		}
		if _, err := encodePlaceOrderProtoBody(order); err != nil {
			t.Fatalf("%s: encode: %v", action, err)
		}
		if order.Multiplier != "5" {
			t.Fatalf("multiplier %q", order.Multiplier)
		}
	}
}
