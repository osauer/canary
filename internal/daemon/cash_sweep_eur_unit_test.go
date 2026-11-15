package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// TestCashSweepEURBillUnitMatchesBrokerWhatIf pins the EUR bill unit to the
// broker's own figures (proof step 7, 2026-10-23): a German Bubill line read
// min_size 1, and WhatIf previews of 50, 5,000 and 50,000 units each moved
// the initial margin by 2.5% of units × 1,000 EUR, with the equity change of a
// 1,000-fold order. One unit is 1,000 EUR of face, as for US bills; a sweep of
// 50,000 EUR face is 50 units. Figures here are synthetic at those ratios.
func TestCashSweepEURBillUnitMatchesBrokerWhatIf(t *testing.T) {
	const brokerFacePerUnit, margin, face, price = 1000.0, 0.025, 50000.0, 99.7
	for _, instrument := range []string{cashSweepInstrumentDEBubill, cashSweepInstrumentFRBTF} {
		conv := cashSweepInstrumentConventions[instrument]
		if conv.FacePerUnit <= 0 {
			t.Fatalf("%s has no unit", instrument)
		}
		units := face / conv.FacePerUnit
		notional := units * conv.FacePerUnit * price / 100
		change := units * brokerFacePerUnit * price / 100 * margin
		whatIf := rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true,
			Margin: &rpc.OrderMarginImpact{Currency: "EUR", InitialMarginBefore: new(10000.0), InitialMarginAfter: new(10000 + change)}}
		terms := &rpc.OrderBondTerms{Instrument: instrument, QuantityUnit: conv.QuantityUnit, FacePerUnit: conv.FacePerUnit}
		if message, ok := bondWhatIfUnitCheck(whatIf, terms, notional, notional, "EUR", "EUR"); !ok {
			t.Fatalf("%s: %s", instrument, message)
		}
	}
	// A working EUR bill order or fill in the journal is valued at the same
	// unit; a EUR BOND line keeps its own, unproven by this evidence.
	if conv, ok := cashSweepBondConvention("BILL", "EUR"); !ok || conv.FacePerUnit != brokerFacePerUnit || conv.QuantityUnit != rpc.BondQuantityUnitFace1000 {
		t.Fatalf("EUR bill journal convention = %+v, %v", conv, ok)
	}
	if conv, ok := cashSweepBondConvention("BOND", "EUR"); !ok || conv.FacePerUnit != 1 || conv.QuantityUnit != rpc.BondQuantityUnitFace1 {
		t.Fatalf("EUR bond convention = %+v, %v", conv, ok)
	}
}
