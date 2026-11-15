package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A holding the broker reports as terminal and values at zero carries no
// exposure (defunctHolding): it neither blanks the book's delta as "unknown"
// nor appears as a row (owner review 2026-10-23 22:15 CEST); a stock with no
// mark and no such verdict still reads unknown.
func TestMandateExposureSkipsADefunctHolding(t *testing.T) {
	defunct := rpc.PositionView{Symbol: "SYND", SecType: positionSecType("STK"), ConID: 202, Currency: "USD", Quantity: 20000, Stale: true,
		QuoteExpectation: rpc.QuoteExpectationNone, QuoteExpectationReason: rpc.QuoteExpectationReasonTerminal, MarketValueBase: new(0.)}
	row, delta, _, _, _, _ := mandateExposureName(rpc.PositionGroup{Underlying: "SYND", Stock: &defunct}, "USD", nil, nil)
	if row.Legs != 0 || !delta.known || len(row.Unknown) != 0 {
		t.Fatalf("a defunct holding counted as unknown exposure: legs %d delta known %v unknown %v", row.Legs, delta.known, row.Unknown)
	}
	unpriced := defunct
	unpriced.QuoteExpectation, unpriced.QuoteExpectationReason = "", ""
	if _, delta, _, _, _, _ := mandateExposureName(rpc.PositionGroup{Underlying: "SYND", Stock: &unpriced}, "USD", nil, nil); delta.known {
		t.Fatal("a stock with no mark and no terminal verdict read as zero delta")
	}
}
