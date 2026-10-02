package risk

import (
	"slices"
	"testing"
	"time"
)

func TestIndependentRepairedLiveValidity(t *testing.T) {
	base := liveSweepCalibrationFixture()
	for _, s := range StudyCashSweepFiniteHorizons(base) {
		if s.NAVAfterEUR == nil {
			t.Fatal("positive live control unavailable", s)
		}
	}
	for _, source := range []string{"cash", "valuation", "price", "exit"} {
		in := liveSweepCalibrationFixture()
		gap := ""
		switch source {
		case "cash":
			in.CashValidUntil = in.AsOf
			gap = "cash_source_validity_unavailable"
		case "valuation":
			in.ValuationValidUntil = in.AsOf
			gap = "valuation_source_validity_unavailable"
		case "price":
			in.Lines[0].PriceValidUntil = in.AsOf
			gap = "price_source_validity_unavailable"
		case "exit":
			in.Lines[0].ExitValidUntil = in.AsOf
			gap = "exit_source_validity_unavailable"
		}
		for _, s := range StudyCashSweepFiniteHorizons(in) {
			if s.NAVAfterEUR != nil || s.WorstLossEUR != nil || !slices.Contains(s.Gaps, gap) {
				t.Fatalf("%s expired evidence retained numeric study: %+v", source, s)
			}
		}
	}
}

func TestIndependentUncreditedCallFundingIsFullAndTruthful(t *testing.T) {
	in := sweepObservationFixture()
	d := *in.Positions[0].Deliverable
	d.Right = "C"
	call := in.Positions[0]
	call.Quantity = -2
	call.Right = "C"
	call.Deliverable = &d
	in.CoverageCreditMode = CashSweepCoverageUncredited
	// Settled shares are absent. Reserving full cover must never report them as0.
	stock := CashSweepFundingPosition{ConID: 101, Symbol: "SYNTH", Currency: "USD", SecType: "STK", Quantity: 200, Price: new(100.), PriceAt: in.AsOf.Add(-time.Second)}
	in.Positions = []CashSweepFundingPosition{stock, call}
	out := ObserveCashSweepOperationalFunding(in)
	if len(out.Obligations) != 1 {
		t.Fatal(out)
	}
	o := out.Obligations[0]
	if o.GrossPrincipal == nil || *o.GrossPrincipal != 40000 || o.CoveredShares != nil || o.CoverageTreatment != CashSweepCoverageUncredited || slices.Contains(o.Gaps, "settled_covered_shares_unavailable") {
		t.Fatalf("conservative full cover: %+v", o)
	}
	call.Deliverable = nil
	in.Positions = []CashSweepFundingPosition{stock, call}
	out = ObserveCashSweepOperationalFunding(in)
	if out.Obligations[0].GrossPrincipal != nil {
		t.Fatal("no-credit mode bypassed missing contract terms", out)
	}
}

func TestIndependentBillHoldingDoesNotCreateFundingCredit(t *testing.T) {
	for _, kind := range []string{"BOND", "BILL"} {
		in := sweepObservationFixture()
		in.Positions = []CashSweepFundingPosition{{ConID: 301, Symbol: "SYNTH_BILL", Currency: "USD", SecType: kind, Quantity: 10}}
		out := ObserveCashSweepOperationalFunding(in)
		if len(out.Obligations) != 0 {
			t.Fatalf("positive %s generated a funding obligation: %+v", kind, out)
		}
		for _, c := range out.Currencies {
			if c.GrossPrincipal == nil || *c.GrossPrincipal != 0 {
				t.Fatal("positive bill generated sale funding or unknown principal", c)
			}
		}
		in.Positions[0].Quantity = -10
		out = ObserveCashSweepOperationalFunding(in)
		if len(out.Obligations) != 1 || out.Obligations[0].Kind != "unsupported_funding_exposure" || !slices.Contains(out.Obligations[0].Gaps, "instrument_funding_model_unavailable") || out.Obligations[0].GrossPrincipal != nil {
			t.Fatal("short bill borrowing escaped hold", out)
		}
	}
}
