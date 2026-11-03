package ibkr

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"testing"
)

// The protobuf placeOrder encoder admits one narrow order slice and refuses
// everything else before a byte is encoded. These tests drive every refusal
// through encodePlaceOrderProtoBody (the body must be nil) and pin the
// encoder's output only by decoding it with the package's own summary
// reader. The refusals are untyped errors; the tests assert the field each
// one names, never the sentence.

// ordTStockLimit is the smallest order the encoder admits.
func ordTStockLimit() *IBKROrder {
	return &IBKROrder{OrderID: 77, ClientID: 15, ConID: 4242, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 10, OrderType: "LMT", LmtPrice: 100.5, TIF: "DAY", Account: "DU1234567", OrderRef: "ref-1", Transmit: true}
}

func ordTTrailLimit() *IBKROrder {
	o := ordTStockLimit()
	o.Action, o.OrderType, o.TIF, o.LmtPrice = "SELL", "TRAIL LIMIT", "GTC", 0
	o.TrailStopPrice, o.TrailingPercent, o.LmtPriceOffset, o.TriggerMethod = 95, 5, 0.5, 2
	return o
}

func ordTAttachedStop(parent int) *IBKROrder {
	o := ordTStockLimit()
	o.OrderID, o.ParentID, o.Action, o.OrderType, o.TIF, o.LmtPrice, o.AuxPrice, o.TriggerMethod, o.OpenClose = parent+1, parent, "SELL", "STP", "GTC", 0, 95, 2, "C"
	return o
}

func ordTRefused(t *testing.T, name string, order *IBKROrder, wantSubstr string) {
	t.Helper()
	body, err := encodePlaceOrderProtoBody(order)
	if err == nil {
		t.Errorf("%s: admitted", name)
		return
	}
	if body != nil {
		t.Errorf("%s: refused but encoded %d bytes", name, len(body))
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Errorf("%s: error %q does not name %q", name, err, wantSubstr)
	}
}

func TestPlaceOrderProtoRefusesOrdersOutsideTheSlice(t *testing.T) {
	mutate := func(f func(o *IBKROrder)) *IBKROrder { o := ordTStockLimit(); f(o); return o }
	cases := []struct {
		name  string
		order *IBKROrder
		want  string
	}{
		{"nil order", nil, "nil"},
		{"orderID beyond int32", mutate(func(o *IBKROrder) { o.OrderID = math.MaxInt32 + 1 }), "orderID"},
		{"secType IND", mutate(func(o *IBKROrder) { o.SecType = "IND" }), "secType"},
		{"orderType MKT", mutate(func(o *IBKROrder) { o.OrderType = "MKT" }), "orderType"},
		{"parentID on a LMT", mutate(func(o *IBKROrder) { o.ParentID = 70 }), "parentID"},
		{"tif GTC on a LMT", mutate(func(o *IBKROrder) { o.TIF = "GTC" }), "tif"},
		{"tif IOC", mutate(func(o *IBKROrder) { o.TIF = "IOC" }), "tif"},
		{"trigger method on a LMT", mutate(func(o *IBKROrder) { o.TriggerMethod = 2 }), "triggerMethod"},
		{"LMT without a price", mutate(func(o *IBKROrder) { o.LmtPrice = 0 }), "lmtPrice"},
		{"LMT with an aux price", mutate(func(o *IBKROrder) { o.AuxPrice = 1 }), "auxiliary"},
		{"stock with an expiry", mutate(func(o *IBKROrder) { o.Expiry = "20261218" }), "expiry"},
		{"stock with a right", mutate(func(o *IBKROrder) { o.Right = "C" }), "right"},
		{"stock with a multiplier", mutate(func(o *IBKROrder) { o.Multiplier = "100" }), "multiplier"},
		{"stock with a strike", mutate(func(o *IBKROrder) { o.Strike = 1 }), "strike"},
		{"stock with combo legs", mutate(func(o *IBKROrder) { o.ComboLegs = []ComboLeg{{ConID: 1, Ratio: 1}} }), "comboLegs"},
		{"ocaType without a group", mutate(func(o *IBKROrder) { o.OcaType = 1 }), "ocaGroup"},
		{"ocaType 4", mutate(func(o *IBKROrder) { o.OcaGroup, o.OcaType = "g", 4 }), "ocaType"},
		{"good till date", mutate(func(o *IBKROrder) { o.GoodTillDate = "20261231" }), "goodTillDate"},
		{"hedge type", mutate(func(o *IBKROrder) { o.HedgeType = "D" }), "hedgeType"},
		{"display size", mutate(func(o *IBKROrder) { o.DisplaySize = 5 }), "displaySize"},
		{"scale levels", mutate(func(o *IBKROrder) { o.ScaleInitLevelSize = 2 }), "scaleInitLevelSize"},
		{"exempt code", mutate(func(o *IBKROrder) { o.ExemptCode = 2 }), "exemptCode"},
		{"openClose X", mutate(func(o *IBKROrder) { o.OpenClose = "X" }), "openClose"},
		{"discretionary amount", mutate(func(o *IBKROrder) { o.DiscretionaryAmt = 0.1 }), "discretionaryAmt"},
		{"volatility", mutate(func(o *IBKROrder) { o.Volatility = 0.2 }), "volatility"},
		{"hidden", mutate(func(o *IBKROrder) { o.Hidden = true }), "hidden"},
		{"not held", mutate(func(o *IBKROrder) { o.NotHeld = true }), "notHeld"},
		{"adaptive on a GTC trail", mutate(func(o *IBKROrder) { o.AdaptivePriority = "Normal"; o.TIF = "GTC" }), "Adaptive"},
		{"adaptive unknown priority", mutate(func(o *IBKROrder) { o.AdaptivePriority = "Fast" }), "Adaptive priority"},
	}
	for _, tc := range cases {
		ordTRefused(t, tc.name, tc.order, tc.want)
	}
	// A non-positive quantity is refused by ValidateOrder, which every send
	// path (placeOrder, the WhatIf preview, the stock entry and stop pair)
	// runs before the encoder; the encoder itself only bounds the quantity
	// to int32. Pin the layer that holds the line.
	negative := ordTStockLimit()
	negative.TotalQty = -1
	if err := ValidateOrder(negative); err == nil || !strings.Contains(err.Error(), "quantity") {
		t.Fatalf("negative quantity passed ValidateOrder: %v", err)
	}
	ordTRefused(t, "quantity beyond int32", mutate(func(o *IBKROrder) { o.TotalQty = math.MaxInt32 + 1 }), "totalQty")
	// exemptCode -1 is IBKR's own default and never a populated field.
	o := ordTStockLimit()
	o.ExemptCode = -1
	if _, err := encodePlaceOrderProtoBody(o); err != nil {
		t.Fatalf("exemptCode -1 refused: %v", err)
	}
	for _, oc := range []string{"", "O", "C", " c "} {
		o := ordTStockLimit()
		o.OpenClose = oc
		if _, err := encodePlaceOrderProtoBody(o); err != nil {
			t.Fatalf("openClose %q refused: %v", oc, err)
		}
	}
}

// A fixed stop is admitted only as the attached, regular-session, GTC,
// SELL stock child the stock-entry-stop path builds.
func TestPlaceOrderProtoAttachedStopShape(t *testing.T) {
	if _, err := encodePlaceOrderProtoBody(ordTAttachedStop(77)); err != nil {
		t.Fatalf("attached stop refused: %v", err)
	}
	whatIfOnly := ordTAttachedStop(77)
	whatIfOnly.Transmit, whatIfOnly.WhatIf = false, true
	if _, err := encodePlaceOrderProtoBody(whatIfOnly); err != nil {
		t.Fatalf("attached stop preview refused: %v", err)
	}
	for name, f := range map[string]func(o *IBKROrder){
		"option":                 func(o *IBKROrder) { o.SecType = "OPT" },
		"no parent":              func(o *IBKROrder) { o.ParentID = 0 },
		"buy":                    func(o *IBKROrder) { o.Action = "BUY" },
		"day":                    func(o *IBKROrder) { o.TIF = "DAY" },
		"trigger default":        func(o *IBKROrder) { o.TriggerMethod = 0 },
		"trigger last":           func(o *IBKROrder) { o.TriggerMethod = 1 },
		"outside rth":            func(o *IBKROrder) { o.OutsideRth = true },
		"held, not previewed":    func(o *IBKROrder) { o.Transmit = false },
		"aux price zero":         func(o *IBKROrder) { o.AuxPrice = 0 },
		"aux price infinite":     func(o *IBKROrder) { o.AuxPrice = math.Inf(1) },
		"limit price too":        func(o *IBKROrder) { o.LmtPrice = 90 },
		"trailing percent too":   func(o *IBKROrder) { o.TrailingPercent = 1 },
		"trail stop price too":   func(o *IBKROrder) { o.TrailStopPrice = 1 },
		"limit price offset too": func(o *IBKROrder) { o.LmtPriceOffset = 1 },
	} {
		o := ordTAttachedStop(77)
		f(o)
		ordTRefused(t, name, o, "stop")
	}
}

func TestPlaceOrderProtoTrailPriceRules(t *testing.T) {
	if _, err := encodePlaceOrderProtoBody(ordTTrailLimit()); err != nil {
		t.Fatalf("trail limit refused: %v", err)
	}
	trail := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTTrailLimit()
		o.OrderType, o.LmtPriceOffset = "TRAIL", 0
		f(o)
		return o
	}
	trailLimit := func(f func(o *IBKROrder)) *IBKROrder { o := ordTTrailLimit(); f(o); return o }
	if _, err := encodePlaceOrderProtoBody(trail(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("trail refused: %v", err)
	}
	amount := trail(func(o *IBKROrder) { o.TrailingPercent, o.AuxPrice = 0, 2 })
	if _, err := encodePlaceOrderProtoBody(amount); err != nil {
		t.Fatalf("trail by amount refused: %v", err)
	}
	day := trail(func(o *IBKROrder) { o.TIF = "DAY" })
	if _, err := encodePlaceOrderProtoBody(day); err != nil {
		t.Fatalf("trail DAY refused: %v", err)
	}
	cases := []struct {
		name  string
		order *IBKROrder
		want  string
	}{
		{"TRAIL with a limit price", trail(func(o *IBKROrder) { o.LmtPrice = 1 }), "lmtPrice"},
		{"TRAIL without a stop", trail(func(o *IBKROrder) { o.TrailStopPrice = 0 }), "trailStopPrice"},
		{"TRAIL amount and percent", trail(func(o *IBKROrder) { o.AuxPrice = 1 }), "exactly one"},
		{"TRAIL neither amount nor percent", trail(func(o *IBKROrder) { o.TrailingPercent = 0 }), "exactly one"},
		{"TRAIL with an offset", trail(func(o *IBKROrder) { o.LmtPriceOffset = 1 }), "lmtPriceOffset"},
		{"TRAIL LIMIT with a limit price", trailLimit(func(o *IBKROrder) { o.LmtPrice = 1 }), "lmtPriceOffset"},
		{"TRAIL LIMIT without a stop", trailLimit(func(o *IBKROrder) { o.TrailStopPrice = 0 }), "trailStopPrice"},
		{"TRAIL LIMIT amount and percent", trailLimit(func(o *IBKROrder) { o.AuxPrice = 1 }), "exactly one"},
		{"TRAIL LIMIT without an offset", trailLimit(func(o *IBKROrder) { o.LmtPriceOffset = 0 }), "lmtPriceOffset"},
		{"TRAIL LIMIT trigger method 5", trailLimit(func(o *IBKROrder) { o.TriggerMethod = 5 }), "triggerMethod"},
		{"TRAIL LIMIT tif IOC", trailLimit(func(o *IBKROrder) { o.TIF = "IOC" }), "tif"},
	}
	for _, tc := range cases {
		ordTRefused(t, tc.name, tc.order, tc.want)
	}
	for _, m := range []int{1, 2, 3, 4, 7, 8} {
		o := ordTTrailLimit()
		o.TriggerMethod = m
		if _, err := encodePlaceOrderProtoBody(o); err != nil {
			t.Errorf("trigger method %d refused: %v", m, err)
		}
	}
}

func TestPlaceOrderProtoInstrumentShapes(t *testing.T) {
	option := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTStockLimit()
		o.SecType, o.Expiry, o.Right, o.Strike, o.Multiplier, o.LmtPrice = "OPT", "20261218", "C", 100, "100", 3.5
		f(o)
		return o
	}
	if _, err := encodePlaceOrderProtoBody(option(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("option refused: %v", err)
	}
	future := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTStockLimit()
		o.SecType, o.Exchange, o.Expiry, o.Multiplier, o.LocalSymbol = "FUT", "CME", "20261218", "5", "ZETAZ6"
		f(o)
		return o
	}
	if _, err := encodePlaceOrderProtoBody(future(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("future refused: %v", err)
	}
	cash := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTStockLimit()
		o.SecType, o.Exchange, o.Symbol, o.Currency, o.LmtPrice = "CASH", "IDEALPRO", "EUR", "USD", 1.1
		f(o)
		return o
	}
	if _, err := encodePlaceOrderProtoBody(cash(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("cash refused: %v", err)
	}
	bond := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTStockLimit()
		o.SecType, o.Symbol, o.TotalQty, o.LmtPrice = "BOND", "", 1000, 99.5
		o.BondRules = &BondOrderRules{MinTick: 0.001, MinSize: 1000, SizeIncrement: 1000}
		f(o)
		return o
	}
	if _, err := encodePlaceOrderProtoBody(bond(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("bond refused: %v", err)
	}
	bag := func(f func(o *IBKROrder)) *IBKROrder {
		o := ordTStockLimit()
		o.SecType, o.LmtPrice, o.LmtPriceSet = "BAG", 0, true
		o.ComboLegs = []ComboLeg{{ConID: 1, Ratio: 1, Action: "BUY", Exchange: "SMART"}, {ConID: 2, Ratio: 1, Action: "SELL", Exchange: "SMART"}}
		f(o)
		return o
	}
	if _, err := encodePlaceOrderProtoBody(bag(func(o *IBKROrder) {})); err != nil {
		t.Fatalf("combo at an explicit zero refused: %v", err)
	}
	cases := []struct {
		name  string
		order *IBKROrder
		want  string
	}{
		{"option without expiry", option(func(o *IBKROrder) { o.Expiry = "" }), "expiry"},
		{"option right X", option(func(o *IBKROrder) { o.Right = "X" }), "right"},
		{"option strike zero", option(func(o *IBKROrder) { o.Strike = 0 }), "strike"},
		{"option without multiplier", option(func(o *IBKROrder) { o.Multiplier = "" }), "multiplier"},
		{"future TRAIL", future(func(o *IBKROrder) {
			o.OrderType, o.LmtPrice, o.TrailStopPrice, o.AuxPrice, o.TIF = "TRAIL", 0, 90, 1, "GTC"
		}), "orderType"},
		{"future GTC", future(func(o *IBKROrder) { o.TIF = "GTC" }), "tif"},
		{"future outside rth", future(func(o *IBKROrder) { o.OutsideRth = true }), "outsideRth"},
		{"future without expiry", future(func(o *IBKROrder) { o.Expiry = "" }), "expiry"},
		{"future without exchange", future(func(o *IBKROrder) { o.Exchange = "" }), "exchange"},
		{"future with a strike", future(func(o *IBKROrder) { o.Strike = 1 }), "strike"},
		{"cash TRAIL", cash(func(o *IBKROrder) {
			o.OrderType, o.LmtPrice, o.TrailStopPrice, o.AuxPrice, o.TIF = "TRAIL", 0, 1, 0.01, "GTC"
		}), "orderType"},
		{"cash GTC", cash(func(o *IBKROrder) { o.TIF = "GTC" }), "tif"},
		{"cash on SMART", cash(func(o *IBKROrder) { o.Exchange = "SMART" }), "exchange"},
		{"cash outside rth", cash(func(o *IBKROrder) { o.OutsideRth = true }), "outsideRth"},
		{"bond TRAIL", bond(func(o *IBKROrder) {
			o.OrderType, o.LmtPrice, o.TrailStopPrice, o.AuxPrice, o.TIF = "TRAIL", 0, 90, 1, "GTC"
		}), "orderType"},
		{"bond GTC", bond(func(o *IBKROrder) { o.TIF = "GTC" }), "tif"},
		{"bond off the size grid", bond(func(o *IBKROrder) { o.TotalQty = 1500 }), "BOND"},
		{"bond without rules", bond(func(o *IBKROrder) { o.BondRules = nil }), "BOND"},
		{"combo without an explicit price", bag(func(o *IBKROrder) { o.LmtPriceSet = false }), "lmtPrice"},
		{"combo with NaN price", bag(func(o *IBKROrder) { o.LmtPrice = math.NaN() }), "lmtPrice"},
		{"combo with one leg", bag(func(o *IBKROrder) { o.ComboLegs = o.ComboLegs[:1] }), "two combo legs"},
		{"combo leg ratio zero", bag(func(o *IBKROrder) { o.ComboLegs[0].Ratio = 0 }), "ratio"},
		{"combo leg conId zero", bag(func(o *IBKROrder) { o.ComboLegs[0].ConID = 0 }), "ConID"},
		{"combo leg conId beyond int32", bag(func(o *IBKROrder) { o.ComboLegs[0].ConID = math.MaxInt32 + 1 }), "int32"},
		{"combo leg action HOLD", bag(func(o *IBKROrder) { o.ComboLegs[0].Action = "HOLD" }), "action"},
		{"combo leg without exchange", bag(func(o *IBKROrder) { o.ComboLegs[0].Exchange = "" }), "exchange"},
		{"combo leg openClose 4", bag(func(o *IBKROrder) { o.ComboLegs[0].OpenClose = 4 }), "openClose"},
		{"combo repeats a leg", bag(func(o *IBKROrder) { o.ComboLegs[1].ConID = 1 }), "repeats"},
		{"combo with an expiry", bag(func(o *IBKROrder) { o.Expiry = "20261218" }), "expiry"},
		{"combo with a strike", bag(func(o *IBKROrder) { o.Strike = 1 }), "strike"},
	}
	for _, tc := range cases {
		ordTRefused(t, tc.name, tc.order, tc.want)
	}
}

// The encoder's output is pinned by decoding it: every admitted field is
// read back by the outbound summary and an unset double is absent, never
// a zero or a sentinel on the wire.
func TestPlaceOrderProtoEncoderRoundTripsThroughTheSummary(t *testing.T) {
	stock := ordTStockLimit()
	stock.OutsideRth, stock.OpenClose, stock.OcaGroup, stock.OcaType = true, "O", "oca-1", OcaTypeReduceNonBlock
	frame, err := encodePlaceOrderProtoFrame(stock)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint32(frame[:4]); got != uint32(protoPlaceOrderMsgID) {
		t.Fatalf("wire id %d", got)
	}
	summary, err := parsePlaceOrderProtoSummary(frame[4:])
	if err != nil {
		t.Fatal(err)
	}
	want := placeOrderProtoSummary{orderID: 77, conID: 4242, symbol: "SYNX", secType: "STK", clientID: 15, action: "BUY", quantity: "10",
		orderType: "LMT", lmtPrice: 100.5, tif: "DAY", account: "DU1234567", orderRef: "ref-1", outsideRth: true, transmit: true, openClose: "O"}
	if summary != want {
		t.Fatalf("stock summary\n got %+v\nwant %+v", summary, want)
	}
	fields := summarizePlaceOrderProtoFrame(frame)
	if fields[0] != strconv.Itoa(protoPlaceOrderMsgID) || fields[1] != "protobuf" || summaryFieldValue(fields, "orderId=") != "77" || summaryFieldValue(fields, "auxPrice=") != "0" {
		t.Fatalf("outbound summary: %#v", fields)
	}
	if got := summarizePlaceOrderProtoFrame(frame[:3]); summaryFieldValue(got, "decode_error=") != "truncated" {
		t.Fatalf("short frame: %#v", got)
	}

	trail := ordTTrailLimit()
	trail.WhatIf = true
	frame, err = encodePlaceOrderProtoFrame(trail)
	if err != nil {
		t.Fatal(err)
	}
	summary, err = parsePlaceOrderProtoSummary(frame[4:])
	if err != nil {
		t.Fatal(err)
	}
	if summary.orderType != "TRAIL LIMIT" || summary.tif != "GTC" || summary.trailStopPrice != 95 || summary.trailingPercent != 5 || summary.lmtPriceOffset != 0.5 || summary.triggerMethod != 2 || !summary.whatIf || summary.lmtPrice != 0 || summary.auxPrice != 0 {
		t.Fatalf("trail limit summary: %+v", summary)
	}

	option := ordTStockLimit()
	option.SecType, option.Expiry, option.Right, option.Strike, option.Multiplier, option.LmtPrice = "opt", "20261218", "p", 100.5, "100", 3.5
	frame, err = encodePlaceOrderProtoFrame(option)
	if err != nil {
		t.Fatal(err)
	}
	summary, err = parsePlaceOrderProtoSummary(frame[4:])
	if err != nil || summary.secType != "OPT" || summary.right != "P" || summary.expiry != "20261218" || summary.strike != 100.5 || summary.multiplier != "100" {
		t.Fatalf("option summary: %+v err %v", summary, err)
	}

	child := ordTAttachedStop(77)
	frame, err = encodePlaceOrderProtoFrame(child)
	if err != nil {
		t.Fatal(err)
	}
	summary, err = parsePlaceOrderProtoSummary(frame[4:])
	if err != nil || summary.parentID != 77 || summary.orderID != 78 || summary.orderType != "STP" || summary.auxPrice != 95 || summary.lmtPrice != 0 || summary.openClose != "C" {
		t.Fatalf("attached stop summary: %+v err %v", summary, err)
	}

	// No double sentinel ever reaches the wire for an unset price.
	sentinel := binary.LittleEndian.AppendUint64(nil, math.Float64bits(math.MaxFloat64))
	for _, o := range []*IBKROrder{stock, trail, option, child} {
		frame, _ := encodePlaceOrderProtoFrame(o)
		if strings.Contains(string(frame), string(sentinel)) {
			t.Fatalf("max-float sentinel on the wire for %s", o.OrderType)
		}
	}
	// The outbound summary reader refuses the wrong wire type field by field.
	for _, field := range []int{1, 4, 19, 31, 65, 66} {
		var s placeOrderProtoSummary
		if err := parseOrderProtoSummary(protoAppendString(nil, field, "x"), &s); err == nil {
			t.Errorf("order summary field %d as string accepted", field)
		}
	}
	for _, field := range []int{9, 10, 22, 23, 99} {
		var s placeOrderProtoSummary
		if err := parseOrderProtoSummary(protoAppendInt32(nil, field, 1), &s); err == nil {
			t.Errorf("order summary field %d as varint accepted", field)
		}
	}
	var s placeOrderProtoSummary
	if err := parseContractProtoSummary(protoAppendInt32(nil, 5, 1), &s); err == nil {
		t.Error("contract strike as varint accepted")
	}
	if err := parseContractProtoSummary(protoAppendString(nil, 1, "x"), &s); err == nil {
		t.Error("contract conId as string accepted")
	}
	if _, err := parsePlaceOrderProtoSummary(protoAppendString(nil, 1, "x")); err == nil {
		t.Error("orderId as string accepted")
	}
	if _, err := parsePlaceOrderProtoSummary(protoAppendMessage(nil, 3, []byte{0x08, 0x80})); err == nil {
		t.Error("truncated order message accepted")
	}
}

// cancelOrder takes the protobuf frame on a current server and the legacy
// field frame below it, in the layout the server version negotiated.
func TestCancelOrderMessageFollowsServerVersion(t *testing.T) {
	conn, _ := newReadyWireTestConnection(t)
	msg, err := conn.encodeCancelOrderMessage(42)
	if err != nil {
		t.Fatal(err)
	}
	fields := conn.decodeOutboundMessage(msg)
	if fields[0] != strconv.Itoa(protoCancelOrderMsgID) || summaryFieldValue(fields, "orderId=") != "42" {
		t.Fatalf("proto cancel: %#v", fields)
	}
	if _, err := conn.encodeCancelOrderMessage(math.MaxInt32 + 1); err == nil {
		t.Fatal("orderID beyond int32 accepted")
	}
	for _, tc := range []struct {
		version int
		orderAt int
		fields  int
	}{
		{minServerVerProtoBufPlaceOrder - 1, 1, 5}, // id, "" (manual order time), "", MAX_INT (CME tagging layout)
		{minServerVerCMETaggingFields - 1, 2, 4},   // version 1, id, "" (manual order time); RFQ fields undone at 190
		{minServerVerRFQFields, 2, 7},              // version 1, id, "", then "", "", MAX_INT between 187 and 190
		{minServerVerManualOrderTime - 1, 2, 3},    // version 1, id
	} {
		conn.serverVersion = tc.version
		msg, err := conn.encodeCancelOrderMessage(42)
		if err != nil {
			t.Fatal(err)
		}
		fields := conn.decodeMessage(msg)
		// The field splitter leaves one empty field after the closing separator.
		if len(fields) > 0 && fields[len(fields)-1] == "" {
			fields = fields[:len(fields)-1]
		}
		if fields[0] != strconv.Itoa(cancelOrder) || fields[tc.orderAt] != "42" || len(fields) != tc.fields {
			t.Fatalf("server %d legacy cancel: %#v (want order at %d, %d fields)", tc.version, fields, tc.orderAt, tc.fields)
		}
	}
}
