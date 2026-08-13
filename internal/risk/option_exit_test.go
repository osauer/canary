package risk

import (
	"math"
	"slices"
	"testing"
)

func approvedOptionExitPolicy() OptionExitPolicy {
	return OptionExitPolicy{
		MinDTE: 14, LossExitPct: 60, ProfitArmGainPct: 50,
		ProfitTrailPct: 30, LockedGainPct: 5, MinTrailPct: 20,
		MaxTrailPct: 50, MaxSpreadPctOfMid: 25, MinTrailAbs: 0.10,
		SpreadMultiple: 2,
	}
}

func eligibleOptionExitInput() OptionExitInput {
	return OptionExitInput{
		ConID: 42, Quantity: 1, Multiplier: 100, AvgCost: 100,
		Bid: 1.50, Ask: 1.55, DTE: 30, DirectionalIntent: true,
		Standalone: true, EconomicRoleAllowed: true, QuoteLive: true,
		QuoteFresh: true, SessionOpen: true,
	}
}

func TestEvaluateOptionExitApprovedThresholds(t *testing.T) {
	pol := approvedOptionExitPolicy()

	loss := eligibleOptionExitInput()
	loss.Bid, loss.Ask = 0.40, 0.42
	got := EvaluateOptionExit(loss, pol)
	if got.Action != OptionExitActionLoss || len(got.Blockers) != 0 {
		t.Fatalf("loss decision = %+v", got)
	}

	profit := eligibleOptionExitInput()
	got = EvaluateOptionExit(profit, pol)
	if got.Action != OptionExitActionProfitTrail || math.Abs(got.TrailAmount-0.45) > 1e-9 || got.InitialLockPct < 5 || len(got.Blockers) != 0 {
		t.Fatalf("profit decision = %+v", got)
	}

	none := eligibleOptionExitInput()
	none.Bid, none.Ask = 1.20, 1.25
	got = EvaluateOptionExit(none, pol)
	if got.Action != "" || len(got.Blockers) != 0 {
		t.Fatalf("no-action decision = %+v", got)
	}
}

func TestEvaluateOptionExitFailsClosed(t *testing.T) {
	pol := approvedOptionExitPolicy()
	for _, tc := range []struct {
		name string
		edit func(*OptionExitInput)
		code string
	}{
		{"missing intent", func(in *OptionExitInput) { in.DirectionalIntent = false }, "directional_intent_required"},
		{"strategy", func(in *OptionExitInput) { in.Standalone = false }, "standalone_option_required"},
		{"hedge conflict", func(in *OptionExitInput) { in.EconomicRoleAllowed = false }, "directional_role_not_confirmed"},
		{"fractional", func(in *OptionExitInput) { in.Quantity = 1.5 }, "whole_contract_quantity_required"},
		{"near expiry", func(in *OptionExitInput) { in.DTE = 13 }, "option_exit_min_dte"},
		{"stale", func(in *OptionExitInput) { in.QuoteFresh = false }, "fresh_option_quote_required"},
		{"delayed", func(in *OptionExitInput) { in.QuoteLive = false }, "live_option_quote_required"},
		{"closed", func(in *OptionExitInput) { in.SessionOpen = false }, "option_rth_closed"},
		{"cost missing", func(in *OptionExitInput) { in.AvgCost = 0 }, "option_cost_basis_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := eligibleOptionExitInput()
			tc.edit(&in)
			got := EvaluateOptionExit(in, pol)
			if !containsOptionExitBlocker(got.Blockers, tc.code) {
				t.Fatalf("decision = %+v, want blocker %q", got, tc.code)
			}
			if got.Action != "" {
				t.Fatalf("blocked evidence selected action %q: %+v", got.Action, got)
			}
		})
	}
}

func TestEvaluateOptionExitDoesNotInferThresholdFromInvalidMeasurement(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*OptionExitInput)
		code string
	}{
		{"delayed", func(in *OptionExitInput) { in.QuoteLive = false }, "live_option_quote_required"},
		{"stale", func(in *OptionExitInput) { in.QuoteFresh = false }, "fresh_option_quote_required"},
		{"closed", func(in *OptionExitInput) { in.SessionOpen = false }, "option_rth_closed"},
		{"wide", func(in *OptionExitInput) { in.Ask = 2.00 }, "option_spread_too_wide"},
		{"role unknown", func(in *OptionExitInput) { in.EconomicRoleAllowed = false }, "directional_role_not_confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := eligibleOptionExitInput()
			in.Bid = 0.40
			in.Ask = 0.42
			tc.edit(&in)
			got := EvaluateOptionExit(in, approvedOptionExitPolicy())
			if got.Action != "" || !containsOptionExitBlocker(got.Blockers, tc.code) {
				t.Fatalf("decision = %+v", got)
			}
		})
	}
}

func TestEvaluateOptionExitBlocksNoiseFloorThatLosesLockedGain(t *testing.T) {
	in := eligibleOptionExitInput()
	in.Ask = 1.75
	got := EvaluateOptionExit(in, approvedOptionExitPolicy())
	if got.Action != OptionExitActionProfitTrail || !containsOptionExitBlocker(got.Blockers, "option_trail_locked_gain_not_met") {
		t.Fatalf("decision = %+v", got)
	}
}

func TestEvaluateOptionExitRejectsNonFiniteBrokerInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*OptionExitInput)
	}{
		{"quantity nan", func(in *OptionExitInput) { in.Quantity = math.NaN() }},
		{"cost positive infinity", func(in *OptionExitInput) { in.AvgCost = math.Inf(1) }},
		{"bid negative infinity", func(in *OptionExitInput) { in.Bid = math.Inf(-1) }},
		{"ask nan", func(in *OptionExitInput) { in.Ask = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := eligibleOptionExitInput()
			tc.edit(&in)
			got := EvaluateOptionExit(in, approvedOptionExitPolicy())
			if got.Action != "" || !containsOptionExitBlocker(got.Blockers, "option_numeric_input_invalid") {
				t.Fatalf("decision = %+v", got)
			}
		})
	}
}

func TestEvaluateOptionExitRejectsNonFinitePolicy(t *testing.T) {
	pol := approvedOptionExitPolicy()
	pol.ProfitArmGainPct = math.NaN()
	got := EvaluateOptionExit(eligibleOptionExitInput(), pol)
	if got.Action != "" || !containsOptionExitBlocker(got.Blockers, "option_exit_policy_invalid") {
		t.Fatalf("decision = %+v", got)
	}
}

func TestOptionExitTrailPctWithinBoundsRejectsRoundedAmountAboveMaximum(t *testing.T) {
	if OptionExitTrailPctWithinBounds(0.21, 0.11, 20, 50) {
		t.Fatal("rounded 52.38% trail must exceed the approved maximum")
	}
	if !OptionExitTrailPctWithinBounds(1.50, 0.45, 20, 50) {
		t.Fatal("30% trail should remain inside the approved range")
	}
}

func containsOptionExitBlocker(blockers []string, want string) bool {
	return slices.Contains(blockers, want)
}
