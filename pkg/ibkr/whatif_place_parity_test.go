package ibkr

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// officialOrderFields is Order.proto (API 10.37) by field number: I a varint
// (int32, int64 or bool; the values sent here fit int32), D a double, S a
// string, M a message or map entry.
var officialOrderFields = func() map[int]officialProtoType {
	const table = "1I 2I 3I 4I 5S 6S 7I 8S 9D 10D 11S 12S 13S 14S 15S 16I 17I 18I 19I 20I 21D 22D 23D 24I 25S 26S 27S 28S 29S 30I " +
		"31I 32S 33S 34S 35S 36S 37D 38I 39I 40I 41S 42D 43I 44S 45I 46I 47S 48I 49I 50D 51D 52I 53D 54I 55I 56I 57I 58S " +
		"59S 60S 61S 62M 63S 64M 65I 66I 67I 68S 69I 70I 71S 72I 73S 74S 75S 76D 77I 78D 79D 80D 81D 82D 83I 84M 85I " +
		"86I 87I 88I 89D 90I 91D 92S 93S 94D 95D 96D 97D 98I 99D 100M 101I 102I 103S 104S 105M 106D 107S 108S 109S 110S " +
		"111I 112I 113I 114S 115S 116I 117I 118S 119I 120I 121I 122I 123I 124I 125S 126S 127I 128I 129D 130D 131D 132S " +
		"133I 134S 135I 136I 137S"
	out := map[int]officialProtoType{}
	for entry := range strings.FieldsSeq(table) {
		n, _ := strconv.Atoi(entry[:len(entry)-1])
		out[n] = map[byte]officialProtoType{'I': officialInt32, 'D': officialDouble, 'S': officialString, 'M': officialMessage}[entry[len(entry)-1]]
	}
	return out
}()

type officialFrame struct {
	request, contract, order map[int][]officialField
	legs                     []map[int][]officialField
}

func decodeOfficialFrame(t *testing.T, order *IBKROrder) officialFrame {
	t.Helper()
	body, err := encodePlaceOrderProtoBody(order)
	if err != nil {
		t.Fatalf("encode %s %s: %v", order.SecType, order.OrderType, err)
	}
	var f officialFrame
	f.request = decodeOfficial(t, "PlaceOrderRequest", body, officialPlaceOrderRequestFields)
	if len(f.request[2]) != 1 || len(f.request[3]) != 1 {
		t.Fatalf("request = %+v", f.request)
	}
	f.contract = decodeOfficial(t, "Contract", f.request[2][0].bytesValue, officialContractFields)
	f.order = decodeOfficial(t, "Order", f.request[3][0].bytesValue, officialOrderFields)
	for _, raw := range f.contract[20] {
		f.legs = append(f.legs, decodeOfficial(t, "ComboLeg", raw.bytesValue, officialComboLegFields))
	}
	return f
}

func whatIfTestCombo() (*Contract, *RawOrder) {
	contract := &Contract{Symbol: "SYNA", SecType: "BAG", Exchange: "SMART", Currency: "USD", ComboLegs: []ComboLeg{
		{ConID: 7501, Ratio: 1, Action: "BUY", Exchange: "SMART"},
		{ConID: 7502, Ratio: 1, Action: "SELL", Exchange: "SMART"},
	}}
	order := &RawOrder{ClientID: 9, Account: "DU1234567", Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1.10, LmtPriceSet: true, TIF: "DAY", OpenClose: "O", OrderRef: "ref-combo"}
	return contract, order
}

// A combination's WhatIf asks about the combination: both legs with the
// official leg defaults, the explicit limit (a credit or zero limit
// included, which is what LmtPriceSet exists for), and whatIf set.
func TestWhatIfOfACombinationCarriesItsLegsAndLimit(t *testing.T) {
	for _, limit := range []float64{1.10, 0, -0.45} {
		contract, order := whatIfTestCombo()
		order.LmtPrice = limit
		f := decodeOfficialFrame(t, whatIfIBKROrder(contract, order, 0))
		if len(f.contract[3]) != 1 || string(f.contract[3][0].bytesValue) != "BAG" || len(f.legs) != 2 {
			t.Fatalf("limit %v: contract %+v legs %d, want BAG with two legs", limit, f.contract, len(f.legs))
		}
		for i, leg := range f.legs {
			want := contract.ComboLegs[i]
			if len(leg[1]) != 1 || int(leg[1][0].int32Value) != want.ConID || leg[2][0].int32Value != 1 || string(leg[3][0].bytesValue) != want.Action ||
				string(leg[4][0].bytesValue) != "SMART" || len(leg[5]) != 1 || leg[5][0].int32Value != 0 || len(leg[6]) != 1 || leg[6][0].int32Value != 0 ||
				len(leg[8]) != 1 || leg[8][0].int32Value != -1 {
				t.Fatalf("limit %v: leg %d = %+v", limit, i, leg)
			}
		}
		if len(f.order[9]) != 1 || f.order[9][0].doubleValue != limit {
			t.Fatalf("limit %v: Order.9 lmtPrice = %+v", limit, f.order[9])
		}
		if len(f.order[65]) != 1 || f.order[65][0].int32Value != 1 || len(f.order[66]) != 0 {
			t.Fatalf("limit %v: whatIf %+v transmit %+v, want whatIf only", limit, f.order[65], f.order[66])
		}
	}
}

// The WhatIf and the placed order are the same order: for every shape Canary
// sends, the two frames decode to the same fields, except the request's
// orderId and the order's whatIf (65) and transmit (66) flags.
func TestWhatIfAndPlacedOrderAreTheSameOrder(t *testing.T) {
	stock := &Contract{ConID: 7601, Symbol: "SYNB", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	option := &Contract{ConID: 7602, Symbol: "SYNC", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20270115", Strike: 40, Right: "C", Multiplier: 100, TradingClass: "SYNC", LocalSymbol: "SYNC  270115C00040000"}
	bond := &Contract{ConID: 7603, SecType: "BOND", Exchange: "SMART", Currency: "USD", BondRules: &BondOrderRules{MinSize: 1000, SizeIncrement: 1000, MinTick: 0.01}}
	combo, comboOrder := whatIfTestCombo()
	base := RawOrder{ClientID: 9, Account: "DU1234567", OrderRef: "ref-parity", TotalQty: 10, TIF: "DAY"}
	with := func(edit func(*RawOrder)) *RawOrder {
		o := base
		edit(&o)
		return &o
	}
	for _, tc := range []struct {
		name     string
		contract *Contract
		order    *RawOrder
	}{
		{"stock limit with Adaptive", stock, with(func(o *RawOrder) {
			o.Action, o.OrderType, o.LmtPrice, o.LmtPriceSet, o.AdaptivePriority = "BUY", "LMT", 20, true, "Normal"
		})},
		{"option limit", option, with(func(o *RawOrder) {
			o.Action, o.OrderType, o.LmtPrice, o.LmtPriceSet, o.TotalQty, o.OpenClose = "BUY", "LMT", 2.5, true, 1, "O"
		})},
		{"combination limit", combo, comboOrder},
		{"attached stop child", stock, with(func(o *RawOrder) {
			o.Action, o.OrderType, o.AuxPrice, o.TIF, o.TriggerMethod, o.ParentID, o.OpenClose = "SELL", "STP", 18, "GTC", 2, 41, "C"
		})},
		{"percent trail", stock, with(func(o *RawOrder) {
			o.Action, o.OrderType, o.TrailingPercent, o.TrailStopPrice, o.TIF, o.OpenClose = "SELL", "TRAIL", 5, 19, "GTC", "C"
		})},
		{"bond limit", bond, with(func(o *RawOrder) {
			o.Action, o.OrderType, o.LmtPrice, o.LmtPriceSet, o.TotalQty = "BUY", "LMT", 99.5, true, 2000
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whatIf := decodeOfficialFrame(t, whatIfIBKROrder(tc.contract, tc.order, 0))
			placed := decodeOfficialFrame(t, rawIBKROrder(tc.contract, tc.order))
			compare := func(part string, a, b map[int][]officialField, skip ...int) {
				t.Helper()
				for n := range 200 {
					if slices.Contains(skip, n) {
						continue
					}
					if !sameOfficialField(a[n], b[n]) {
						t.Errorf("%s.%d: WhatIf %+v, placed %+v", part, n, a[n], b[n])
					}
				}
			}
			compare("PlaceOrderRequest", whatIf.request, placed.request, 1, 2, 3)
			compare("Contract", whatIf.contract, placed.contract, 20)
			compare("Order", whatIf.order, placed.order, 65, 66)
			if len(whatIf.legs) != len(placed.legs) {
				t.Fatalf("legs: WhatIf %d, placed %d", len(whatIf.legs), len(placed.legs))
			}
			for i := range whatIf.legs {
				compare("ComboLeg"+strconv.Itoa(i), whatIf.legs[i], placed.legs[i])
			}
		})
	}
}

func sameOfficialField(a, b []officialField) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].int32Value != b[i].int32Value || a[i].doubleValue != b[i].doubleValue || !bytes.Equal(a[i].bytesValue, b[i].bytesValue) {
			return false
		}
	}
	return true
}
