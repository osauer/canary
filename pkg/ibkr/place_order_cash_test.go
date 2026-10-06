package ibkr

import "testing"

// A currency conversion encodes only as LMT DAY on IDEALPRO in regular
// hours; every other CASH shape is refused before it reaches the wire.
func TestValidatePlaceOrderProtoCashConversion(t *testing.T) {
	conversion := func() IBKROrder {
		return IBKROrder{ConID: 12087792, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD",
			Action: "SELL", TotalQty: 23116, OrderType: "LMT", LmtPrice: 1.1698, LmtPriceSet: true, TIF: "DAY"}
	}
	base := conversion()
	if err := validatePlaceOrderProtoSupported(&base); err != nil {
		t.Fatalf("LMT DAY on IDEALPRO refused: %v", err)
	}
	for name, edit := range map[string]func(*IBKROrder){
		"trail":         func(o *IBKROrder) { o.OrderType = "TRAIL" },
		"GTC":           func(o *IBKROrder) { o.TIF = "GTC" },
		"SMART":         func(o *IBKROrder) { o.Exchange = "SMART" },
		"outside RTH":   func(o *IBKROrder) { o.OutsideRth = true },
		"multiplier":    func(o *IBKROrder) { o.Multiplier = "100" },
		"other secType": func(o *IBKROrder) { o.SecType = "FUT" },
	} {
		o := conversion()
		edit(&o)
		if err := validatePlaceOrderProtoSupported(&o); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
}
