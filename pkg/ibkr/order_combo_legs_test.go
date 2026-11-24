package ibkr

import "testing"

// A combination's openOrder carries its legs (Contract.proto comboLegs,
// field 20), so an order placed in TWS reads as the combination it is.
func TestOpenOrderCarriesComboLegs(t *testing.T) {
	var contract []byte
	contract = protoAppendInt32(contract, 1, 28812380)
	contract = protoAppendString(contract, 2, "SYNC")
	contract = protoAppendString(contract, 3, "BAG")
	contract = protoAppendString(contract, 10, "USD")
	contract = protoAppendMessage(contract, 20, encodeComboLegProto(ComboLeg{ConID: 930604, Ratio: 1, Action: "BUY", Exchange: "SMART"}))
	contract = protoAppendMessage(contract, 20, encodeComboLegProto(ComboLeg{ConID: 930608, Ratio: 1, Action: "SELL", Exchange: "SMART"}))
	var order []byte
	order = protoAppendInt32(order, 2, 41)
	order = protoAppendString(order, 5, "BUY")
	var body []byte
	body = protoAppendInt32(body, 1, 41)
	body = protoAppendMessage(body, 2, contract)
	body = protoAppendMessage(body, 3, order)
	fields := summarizeOpenOrderProtoCallback(body)
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok || ev.SecType != "BAG" || len(ev.ComboLegs) != 2 || ev.ComboLegs[0] != (ComboLeg{ConID: 930604, Ratio: 1, Action: "BUY", Exchange: "SMART"}) || ev.ComboLegs[1].ConID != 930608 || ev.ComboLegs[1].Action != "SELL" {
		t.Fatalf("event %+v from %v", ev.ComboLegs, fields)
	}
	if got := parseComboLegSummary("1:1:BUY:SMART,x"); len(got) != 1 {
		t.Fatalf("a malformed leg: %+v", got)
	}
}
