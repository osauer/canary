package ibkr

import (
	"encoding/binary"
	"math"
	"testing"
)

// The placeOrder frame read back by the official field numbers and types of
// the IB API 10.37 protobuf definitions (source/proto/PlaceOrderRequest.proto,
// Contract.proto, ComboLeg.proto), with a decoder that shares nothing with
// the encoder. A field the definition does not name, or one sent with a wire
// type its declared type does not use, fails the decode: that is how the
// broker's generated parser treats it (an unknown field is skipped, so the
// value never arrives).

type officialProtoType int

const (
	officialInt32 officialProtoType = iota
	officialDouble
	officialString
	officialMessage
)

var (
	officialPlaceOrderRequestFields = map[int]officialProtoType{1: officialInt32, 2: officialMessage, 3: officialMessage}
	officialContractFields          = map[int]officialProtoType{
		1: officialInt32, 2: officialString, 3: officialString, 4: officialString, 5: officialDouble, 6: officialString,
		7: officialDouble, 8: officialString, 9: officialString, 10: officialString, 11: officialString, 12: officialString,
		13: officialString, 14: officialString, 15: officialString, 16: officialString, 17: officialMessage,
		18: officialInt32, // bool includeExpired: a varint, as int32
		19: officialString, 20: officialMessage,
	}
	officialComboLegFields = map[int]officialProtoType{
		1: officialInt32, 2: officialInt32, 3: officialString, 4: officialString, 5: officialInt32,
		6: officialInt32, 7: officialString, 8: officialInt32, 9: officialDouble,
	}
)

type officialField struct {
	int32Value  int32
	doubleValue float64
	bytesValue  []byte
}

// decodeOfficial returns every occurrence of each field, in order.
func decodeOfficial(t *testing.T, name string, buf []byte, table map[int]officialProtoType) map[int][]officialField {
	t.Helper()
	out := map[int][]officialField{}
	for len(buf) > 0 {
		tag, n := binary.Uvarint(buf)
		if n <= 0 {
			t.Fatalf("%s: bad tag", name)
		}
		buf = buf[n:]
		number, wire := int(tag>>3), int(tag&7)
		typ, known := table[number]
		if !known {
			t.Fatalf("%s: field %d is not in the official definition (wire %d)", name, number, wire)
		}
		var f officialField
		switch typ {
		case officialInt32:
			if wire != 0 {
				t.Fatalf("%s: field %d is int32, sent with wire type %d", name, number, wire)
			}
			v, n := binary.Uvarint(buf)
			if n <= 0 {
				t.Fatalf("%s: field %d bad varint", name, number)
			}
			buf = buf[n:]
			signed := int64(v)
			if signed < math.MinInt32 || signed > math.MaxInt32 {
				t.Fatalf("%s: field %d varint %d outside int32", name, number, signed)
			}
			f.int32Value = int32(signed)
		case officialDouble:
			if wire != 1 {
				t.Fatalf("%s: field %d is double, sent with wire type %d", name, number, wire)
			}
			if len(buf) < 8 {
				t.Fatalf("%s: field %d truncated double", name, number)
			}
			f.doubleValue = math.Float64frombits(binary.LittleEndian.Uint64(buf[:8]))
			buf = buf[8:]
		case officialString, officialMessage:
			if wire != 2 {
				t.Fatalf("%s: field %d is length-delimited, sent with wire type %d", name, number, wire)
			}
			l, n := binary.Uvarint(buf)
			if n <= 0 || uint64(len(buf)-n) < l {
				t.Fatalf("%s: field %d bad length", name, number)
			}
			f.bytesValue = buf[n : n+int(l)]
			buf = buf[n+int(l):]
		}
		out[number] = append(out[number], f)
	}
	return out
}

func officialPlaceOrderContract(t *testing.T, order *IBKROrder) map[int][]officialField {
	t.Helper()
	body, err := encodePlaceOrderProtoBody(order)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	request := decodeOfficial(t, "PlaceOrderRequest", body, officialPlaceOrderRequestFields)
	if len(request[2]) != 1 {
		t.Fatalf("PlaceOrderRequest carries %d contracts", len(request[2]))
	}
	return decodeOfficial(t, "Contract", request[2][0].bytesValue, officialContractFields)
}

// PlaceOrderRequest in the 10.37 API has orderId, contract and order only;
// the official client never sends field 4 (attachedOrders, TWS 10.45+), whose
// presence alone routes the order through the server's 10354 check.
func TestPlaceOrderFrameSendsOnlyOfficialRequestFields(t *testing.T) {
	stock := &IBKROrder{OrderID: 34, ClientID: 9, ConID: 7004, Symbol: "SYNC", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 3, OrderType: "LMT", LmtPrice: 12.5, LmtPriceSet: true, TIF: "DAY", Account: "DU1234567"}
	body, err := encodePlaceOrderProtoBody(stock)
	if err != nil {
		t.Fatal(err)
	}
	request := decodeOfficial(t, "PlaceOrderRequest", body, officialPlaceOrderRequestFields)
	if len(request[1]) != 1 || request[1][0].int32Value != 34 || len(request[2]) != 1 || len(request[3]) != 1 {
		t.Fatalf("PlaceOrderRequest = %+v", request)
	}
}

// Contract.7 is `optional double multiplier`; the official client sends
// Double.parseDouble(contract.multiplier()) (EClientUtils.java:371).
func TestPlaceOrderFrameMultiplierIsOfficialDouble(t *testing.T) {
	option := &IBKROrder{
		OrderID: 31, ClientID: 9, ConID: 7001, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD",
		Expiry: "20270115", Strike: 45, Right: "P", Multiplier: "100", TradingClass: "SYNA",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1.25, LmtPriceSet: true, TIF: "DAY", Account: "DU1234567",
	}
	contract := officialPlaceOrderContract(t, option)
	if len(contract[7]) != 1 || contract[7][0].doubleValue != 100 {
		t.Fatalf("Contract.7 multiplier = %+v, want one double 100", contract[7])
	}
	future := &IBKROrder{
		OrderID: 32, ClientID: 9, ConID: 7002, Symbol: "SYNF", SecType: "FUT", Exchange: "CME", Currency: "USD",
		Expiry: "20261218", Multiplier: "5", Action: "SELL", TotalQty: 1, OrderType: "LMT", LmtPrice: 10.5, LmtPriceSet: true, TIF: "DAY",
	}
	if contract := officialPlaceOrderContract(t, future); len(contract[7]) != 1 || contract[7][0].doubleValue != 5 {
		t.Fatalf("future Contract.7 multiplier = %+v, want one double 5", contract[7])
	}
	stock := &IBKROrder{OrderID: 33, ClientID: 9, ConID: 7003, Symbol: "SYNB", SecType: "STK", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 10, OrderType: "LMT", LmtPrice: 20, LmtPriceSet: true, TIF: "DAY"}
	if contract := officialPlaceOrderContract(t, stock); len(contract[7]) != 0 {
		t.Fatalf("stock sent a multiplier: %+v", contract[7])
	}

	// The wire log reads the multiplier back as the double it now is.
	body, err := encodePlaceOrderProtoBody(option)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := parsePlaceOrderProtoSummary(body)
	if err != nil || summary.multiplier != "100" {
		t.Fatalf("outbound summary multiplier = %q, %v", summary.multiplier, err)
	}

	// A multiplier that is not a finite positive number cannot be sent as
	// the reviewed order meant it: the order is refused before the wire.
	for _, bad := range []string{"abc", "NaN", "Inf", "-100", "0", "0x64"} {
		o := *option
		o.Multiplier = bad
		if _, err := encodePlaceOrderProtoBody(&o); err == nil {
			t.Errorf("multiplier %q was encoded", bad)
		}
	}
}

// Every combo leg as the official encoder writes it (EClientUtils.java
// createComboLegProto, ComboLeg() defaults): conId, ratio, action, exchange,
// openClose (0, Same), shortSalesSlot 0 and exemptCode -1 on every leg;
// designatedLocation only when set, perLegPrice never without order combo
// leg prices.
func TestPlaceOrderFrameComboLegsMatchOfficialDefaults(t *testing.T) {
	bag := &IBKROrder{
		OrderID: 35, ClientID: 9, Symbol: "SYNA", SecType: "BAG", Exchange: "SMART", Currency: "USD",
		Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1.10, LmtPriceSet: true, TIF: "DAY", OpenClose: "O",
		ComboLegs: []ComboLeg{
			{ConID: 7101, Ratio: 1, Action: "BUY", Exchange: "SMART"},
			{ConID: 7102, Ratio: 1, Action: "SELL", Exchange: "SMART"},
		},
	}
	contract := officialPlaceOrderContract(t, bag)
	if len(contract[20]) != 2 {
		t.Fatalf("combo legs = %d, want 2", len(contract[20]))
	}
	for i, raw := range contract[20] {
		leg := decodeOfficial(t, "ComboLeg", raw.bytesValue, officialComboLegFields)
		want := bag.ComboLegs[i]
		if len(leg[1]) != 1 || int(leg[1][0].int32Value) != want.ConID || len(leg[2]) != 1 || leg[2][0].int32Value != 1 ||
			len(leg[3]) != 1 || string(leg[3][0].bytesValue) != want.Action || len(leg[4]) != 1 || string(leg[4][0].bytesValue) != "SMART" {
			t.Fatalf("leg %d identity = %+v", i, leg)
		}
		for _, f := range []struct {
			number int
			name   string
			want   int32
		}{{5, "openClose", 0}, {6, "shortSalesSlot", 0}, {8, "exemptCode", -1}} {
			if len(leg[f.number]) != 1 || leg[f.number][0].int32Value != f.want {
				t.Errorf("leg %d %s (field %d) = %+v, want present %d", i, f.name, f.number, leg[f.number], f.want)
			}
		}
		if len(leg[7]) != 0 || len(leg[9]) != 0 {
			t.Errorf("leg %d sent designatedLocation or perLegPrice: %+v", i, leg)
		}
	}
}
