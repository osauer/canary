package ibkr

import (
	"slices"
	"testing"

	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// Canary's placeOrder encoder read back by the broker's own field table
// (twssim decodes by IB Gateway 10.50's PlaceOrderRequest, Contract and
// Order). A stock limit arrives whole. An option's multiplier does not:
// Contract.7 is a double at the broker, and the encoder writes it as a
// string, so the broker's parser drops it (frame audit 2026-11-22). This test
// records what the broker receives today; the encoder fix on branch
// frame-fixes turns its option expectation around.
func TestPlaceOrderFramesAsTheBrokerReadsThem(t *testing.T) {
	stock := &IBKROrder{OrderID: 21, ClientID: 15, ConID: 900001, Symbol: "SYNA", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD",
		Action: "BUY", TotalQty: 10, OrderType: "LMT", LmtPrice: 50.1, TIF: "DAY", Account: "DU1234567", Transmit: true, WhatIf: true}
	body, err := encodePlaceOrderProtoBody(stock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := twssim.DecodePlaceOrder(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.IgnoredByBroker) != 0 || got.ConID != 900001 || got.Action != "BUY" || got.Quantity != 10 || got.LimitPrice != 50.1 || got.TIF != "DAY" || !got.WhatIf || !got.Transmit || got.Account != "DU1234567" {
		t.Fatalf("stock limit as the broker reads it: %+v", got)
	}

	option := &IBKROrder{OrderID: 22, ClientID: 15, ConID: 900501, Symbol: "SYNA", SecType: "OPT", Expiry: "20261218", Strike: 55, Right: "C", Multiplier: "100", Exchange: "SMART", Currency: "USD",
		Action: "SELL", TotalQty: 1, OrderType: "LMT", LmtPrice: 1.25, TIF: "DAY", Account: "DU1234567", Transmit: true}
	if body, err = encodePlaceOrderProtoBody(option); err != nil {
		t.Fatal(err)
	}
	if got, err = twssim.DecodePlaceOrder(body); err != nil {
		t.Fatal(err)
	}
	if got.Multiplier != 0 || !slices.Contains(got.IgnoredByBroker, "contract.7") {
		t.Fatalf("option multiplier at the broker: %v, dropped %v", got.Multiplier, got.IgnoredByBroker)
	}
	if got.Strike != 55 || got.Right != "C" || got.Expiry != "20261218" {
		t.Fatalf("option identity as the broker reads it: %+v", got)
	}
}
