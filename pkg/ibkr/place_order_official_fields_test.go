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
