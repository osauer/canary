package ibkr

import (
	"math"
	"testing"
)

func TestEncodePlaceOrderProtoGuaranteedCombo(t *testing.T) {
	order := &IBKROrder{
		OrderID: 17, ClientID: 9, Symbol: "SNOW", SecType: "BAG", Exchange: "SMART", Currency: "USD",
		Action: "SELL", TotalQty: 2, OrderType: "LMT", LmtPrice: -1.25, LmtPriceSet: true, TIF: "DAY", OpenClose: "C",
		ComboLegs: []ComboLeg{
			{ConID: 101, Ratio: 1, Action: "BUY", Exchange: "SMART", OpenClose: 2},
			{ConID: 102, Ratio: 2, Action: "SELL", Exchange: "SMART", OpenClose: 2},
		},
	}
	body, err := encodePlaceOrderProtoBody(order)
	if err != nil {
		t.Fatal(err)
	}

	var contractBody, orderBody []byte
	if err := forEachProtoField(body, func(fieldNumber, _ int, value []byte) error {
		switch fieldNumber {
		case 2:
			contractBody = append([]byte(nil), value...)
		case 3:
			orderBody = append([]byte(nil), value...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var legs [][]byte
	if err := forEachProtoField(contractBody, func(fieldNumber, _ int, value []byte) error {
		if fieldNumber == 20 {
			legs = append(legs, append([]byte(nil), value...))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(legs) != 2 {
		t.Fatalf("combo legs=%d want 2", len(legs))
	}
	assertComboLegProto(t, legs[0], 101, 1, "BUY", "SMART", 2)
	assertComboLegProto(t, legs[1], 102, 2, "SELL", "SMART", 2)

	limit := math.NaN()
	if err := forEachProtoField(orderBody, func(fieldNumber, wireType int, value []byte) error {
		if fieldNumber == 9 {
			bits, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			limit = math.Float64frombits(bits)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if limit != -1.25 {
		t.Fatalf("limit=%v want -1.25", limit)
	}
}

func TestEncodePlaceOrderProtoOptionNativePercentTrail(t *testing.T) {
	order := &IBKROrder{
		OrderID: 18, ClientID: 9, Symbol: "TEST", SecType: "OPT", ConID: 42, Exchange: "SMART", Currency: "USD",
		Expiry: "20260918", Strike: 100, Right: "C", Multiplier: "100", LocalSymbol: "TEST  260918C00100000", TradingClass: "TEST",
		Action: "SELL", TotalQty: 2, OrderType: "TRAIL LIMIT", TIF: "DAY", OpenClose: "C",
		TrailingPercent: 35, TrailStopPrice: 1.30, LmtPriceOffset: 0.05, WhatIf: true, Transmit: true,
	}
	body, err := encodePlaceOrderProtoBody(order)
	if err != nil {
		t.Fatal(err)
	}

	var orderBody []byte
	if err := forEachProtoField(body, func(fieldNumber, _ int, value []byte) error {
		if fieldNumber == 3 {
			orderBody = append([]byte(nil), value...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(orderBody) == 0 {
		t.Fatal("protobuf placeOrder omitted order body")
	}

	prices := map[int]float64{}
	if err := forEachProtoField(orderBody, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 9, 10, 22, 23, 99:
			bits, err := protoFixed64Value(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			prices[fieldNumber] = math.Float64frombits(bits)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := prices[9]; ok {
		t.Fatalf("native TRAIL LIMIT unexpectedly populated lmtPrice: %+v", prices)
	}
	if _, ok := prices[10]; ok {
		t.Fatalf("native percentage trail unexpectedly populated auxPrice: %+v", prices)
	}
	if prices[22] != 35 || prices[23] != 1.30 || prices[99] != 0.05 {
		t.Fatalf("native trail protobuf prices = %+v, want fields 22=35 23=1.30 99=0.05", prices)
	}
}

func TestValidatePlaceOrderProtoComboRequiresExactDistinctLegs(t *testing.T) {
	base := IBKROrder{
		Symbol: "SPY", SecType: "BAG", Exchange: "SMART", Currency: "USD", Action: "SELL", TotalQty: 1,
		OrderType: "LMT", LmtPriceSet: true, TIF: "DAY",

		ComboLegs: []ComboLeg{{ConID: 1, Ratio: 1, Action: "BUY", Exchange: "SMART"}}}
	if err := validatePlaceOrderProtoSupported(&base); err == nil {
		t.Fatal("one-leg BAG unexpectedly accepted")
	}
	base.ComboLegs = []ComboLeg{
		{ConID: 1, Ratio: 1, Action: "BUY", Exchange: "SMART"},
		{ConID: 1, Ratio: 1, Action: "SELL", Exchange: "SMART"},
	}
	if err := validatePlaceOrderProtoSupported(&base); err == nil {
		t.Fatal("duplicate combo ConID unexpectedly accepted")
	}
}

func assertComboLegProto(t *testing.T, body []byte, conID, ratio int, action, exchange string, openClose int) {
	t.Helper()
	gotConID, gotRatio, gotOpenClose := 0, 0, 0
	gotAction, gotExchange := "", ""
	if err := forEachProtoField(body, func(fieldNumber, wireType int, value []byte) error {
		switch fieldNumber {
		case 1, 2, 5:
			v, err := protoVarintValue(fieldNumber, wireType, value)
			if err != nil {
				return err
			}
			switch fieldNumber {
			case 1:
				gotConID = int(v)
			case 2:
				gotRatio = int(v)
			case 5:
				gotOpenClose = int(v)
			}
		case 3:
			gotAction = string(value)
		case 4:
			gotExchange = string(value)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if gotConID != conID || gotRatio != ratio || gotAction != action || gotExchange != exchange || gotOpenClose != openClose {
		t.Fatalf("leg=(%d,%d,%s,%s,%d) want (%d,%d,%s,%s,%d)", gotConID, gotRatio, gotAction, gotExchange, gotOpenClose, conID, ratio, action, exchange, openClose)
	}
}
