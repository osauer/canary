package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A held bill or bond carries no equity delta. Its quantity is face (in the
// line's order unit) and its mark is a price per 100 of face, so quantity ×
// mark is neither shares nor value: a EUR bond of 10,000 face at 98.5 read as
// 985,000 of dollar delta.
func TestPortfolioAggregatesLeaveBondsOutOfEquityDelta(t *testing.T) {
	stock := rpc.PositionView{Symbol: "SYNTH", SecType: "STK", Currency: "USD", Quantity: 100, Mark: 50}
	bond := rpc.PositionView{Symbol: "SYNB", SecType: "BOND", Currency: "EUR", Quantity: 10000, Mark: 98.5, MarketValue: 9850}
	bill := rpc.PositionView{Symbol: "SYNT", SecType: "BILL", Currency: "USD", Quantity: 10, Mark: 99.2, MarketValue: 9920}

	got := buildPortfolioAggregatesWithBase([]rpc.PositionView{stock, bond, bill}, nil, "USD")
	if got.EffectiveDelta == nil || *got.EffectiveDelta != 100 {
		t.Fatalf("effective delta = %v, want 100 (the stock's shares only)", ptrValue(got.EffectiveDelta))
	}
	if got.DollarDelta == nil || *got.DollarDelta != 5000 {
		t.Fatalf("dollar delta = %v, want 5000 (the stock's value only)", ptrValue(got.DollarDelta))
	}
	if got.DollarDeltaCurrency != "USD" {
		t.Fatalf("dollar delta currency = %q, want USD: a EUR bond must not mix the currency", got.DollarDeltaCurrency)
	}

	onlyBonds := buildPortfolioAggregatesWithBase([]rpc.PositionView{bond, bill}, nil, "USD")
	if onlyBonds.EffectiveDelta != nil || onlyBonds.DollarDelta != nil {
		t.Fatalf("a bonds-only book reports effective delta %v and dollar delta %v, want none", ptrValue(onlyBonds.EffectiveDelta), ptrValue(onlyBonds.DollarDelta))
	}
}

func ptrValue(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
