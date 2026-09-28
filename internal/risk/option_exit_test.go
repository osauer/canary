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

// A caller that deliberately requested no quote (the leg's purpose is not yet
// confirmed) gets the blockers it can act on, not quote failures for a quote
// nobody asked for. The valuation stays unavailable either way.
func TestEvaluateOptionExitSkippedQuoteReportsNoQuoteBlocker(t *testing.T) {
	pol := OptionExitPolicy{MinDTE: 14, LossExitPct: 60, ProfitArmGainPct: 50, ProfitTrailPct: 30, LockedGainPct: 5, MinTrailPct: 20, MaxTrailPct: 50, MaxSpreadPctOfMid: 25, MinTrailAbs: 0.1, SpreadMultiple: 2}
	in := OptionExitInput{ConID: 42, Quantity: 1, Multiplier: 100, AvgCost: 350, DTE: 30, DirectionalIntent: false, Standalone: true, EconomicRoleAllowed: true, SessionOpen: true, QuoteSkipped: true}
	out := EvaluateOptionExit(in, pol)
	if out.Action != "" || out.ReferencePrice != 0 {
		t.Fatalf("a skipped quote produced a valuation or an action: %+v", out)
	}
	want := []string{"directional_intent_required"}
	if !slices.Equal(out.Blockers, want) {
		t.Fatalf("blockers %v, want %v", out.Blockers, want)
	}
	// The same leg with a quote requested but not received reports the quote failures.
	in.QuoteSkipped = false
	out = EvaluateOptionExit(in, pol)
	for _, code := range []string{"live_option_quote_required", "fresh_option_quote_required", "two_sided_option_quote_required"} {
		if !slices.Contains(out.Blockers, code) {
			t.Fatalf("requested quote absent but %q not reported: %v", code, out.Blockers)
		}
	}
}

// expiryWindowPolicy is the approved policy with the Rulebook's standard
// expiry act level (runway_act_dte = 7).
func expiryWindowPolicy() OptionExitPolicy {
	pol := approvedOptionExitPolicy()
	pol.ExpiryCloseDTE = 7
	return pol
}

// Owner decision 2026-09-28: the loss exit keeps working until expiry. Below
// the 14-day floor a long option at or below the loss line still gets its
// loss exit, up to and including expiry day, and it needs no underlying price.
// Only an expired contract has no exit left.
func TestEvaluateOptionExitLossExitRunsUntilExpiry(t *testing.T) {
	for _, dte := range []int{13, 3, 0} {
		in := eligibleOptionExitInput()
		in.Bid, in.Ask, in.DTE = 0.35, 0.36, dte
		got := EvaluateOptionExit(in, expiryWindowPolicy())
		if got.Action != OptionExitActionLoss || len(got.Blockers) != 0 || !got.Measured {
			t.Fatalf("DTE %d: loss decision = %+v", dte, got)
		}
	}
	expired := eligibleOptionExitInput()
	expired.Bid, expired.Ask, expired.DTE = 0.35, 0.36, -1
	got := EvaluateOptionExit(expired, expiryWindowPolicy())
	if got.Action != "" || !slices.Equal(got.Blockers, []string{"option_exit_min_dte"}) || got.Measured {
		t.Fatalf("expired contract decision = %+v", got)
	}
}

// The profit trail keeps its 14-day floor, and the floor applies only where the
// trail would: a measured gain at the arming line, or a carried high water. A
// mid-range option below 14 DTE has no row at all.
func TestEvaluateOptionExitProfitTrailKeepsItsMinimumDTE(t *testing.T) {
	armed := eligibleOptionExitInput()
	armed.DTE = 13
	got := EvaluateOptionExit(armed, expiryWindowPolicy())
	if got.Action != "" || !slices.Equal(got.Blockers, []string{"option_exit_min_dte"}) || !got.Measured ||
		math.Abs(got.ReturnPct-50) > 1e-9 || got.HighWater != 0 {
		t.Fatalf("armed trail below the floor = %+v", got)
	}
	carried := eligibleOptionExitInput()
	carried.DTE, carried.Bid, carried.Ask = 13, 1.30, 1.35
	carried.CarriedHighWater = OptionExitHighWater{PerShare: 1.60, CostPremium: 1, Quantity: 1}
	got = EvaluateOptionExit(carried, expiryWindowPolicy())
	if got.Action != "" || !slices.Equal(got.Blockers, []string{"option_exit_min_dte"}) {
		t.Fatalf("carried trail below the floor = %+v", got)
	}
	carried.Bid, carried.Ask = 1.10, 1.15 // below the carried stop: no take below the floor either
	got = EvaluateOptionExit(carried, expiryWindowPolicy())
	if got.Action != "" || !slices.Equal(got.Blockers, []string{"option_exit_min_dte"}) {
		t.Fatalf("carried stop hit below the floor = %+v", got)
	}
	quiet := eligibleOptionExitInput()
	quiet.DTE, quiet.Bid, quiet.Ask = 13, 1.20, 1.25
	got = EvaluateOptionExit(quiet, expiryWindowPolicy())
	if got.Action != "" || len(got.Blockers) != 0 {
		t.Fatalf("mid-range option below the floor = %+v", got)
	}
}

// Owner decision 2026-09-28: a new proposal closes in-the-money long options
// before expiry, so nothing is exercised by accident. It reads moneyness from
// the fresh underlying price inside the Rulebook's expiry window; the loss exit
// wins where both apply.
func TestEvaluateOptionExitExpiryClose(t *testing.T) {
	for _, tc := range []struct {
		name               string
		right              string
		dte                int
		underlying, bid    float64
		action             string
		blockers           []string
		recordedUnderlying float64
	}{
		{"ITM call at the act level", "C", 7, 105, 1.20, OptionExitActionExpiryClose, nil, 105},
		{"ITM put on expiry day", "P", 0, 95, 1.20, OptionExitActionExpiryClose, nil, 95},
		{"ITM call with a large gain", "C", 5, 105, 1.60, OptionExitActionExpiryClose, nil, 105},
		{"OTM call at the act level", "C", 7, 95, 1.20, "", nil, 95},
		{"at the money is not in the money", "C", 7, 100, 1.20, "", nil, 100},
		{"ITM call outside the window", "C", 8, 105, 1.20, "", nil, 0},
		{"underlying unavailable", "C", 7, 0, 1.20, "", []string{"option_expiry_underlying_unavailable"}, 0},
		{"unknown right", "X", 7, 105, 1.20, "", []string{"option_expiry_underlying_unavailable"}, 0},
		{"loss exit wins", "C", 3, 105, 0.35, OptionExitActionLoss, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := eligibleOptionExitInput()
			in.Right, in.Strike, in.Underlying, in.DTE = tc.right, 100, tc.underlying, tc.dte
			in.Bid, in.Ask = tc.bid, tc.bid+0.05
			got := EvaluateOptionExit(in, expiryWindowPolicy())
			if got.Action != tc.action || !slices.Equal(got.Blockers, tc.blockers) || got.Underlying != tc.recordedUnderlying || !got.Measured {
				t.Fatalf("decision = %+v", got)
			}
		})
	}
}

// Owner decision 2026-09-28: the single-option profit trail stays armed from
// its peak, carrying a high-water mark the way units do. The trail and its
// stop are measured from the high water; a bid already through the stop is a
// close now; a changed cost basis or quantity starts the trail again.
func TestEvaluateOptionExitCarriedHighWater(t *testing.T) {
	carried := OptionExitHighWater{PerShare: 1.60, CostPremium: 1, Quantity: 1}
	for _, tc := range []struct {
		name      string
		bid       float64
		carried   OptionExitHighWater
		action    string
		blockers  []string
		highWater float64
		stop      float64
	}{
		// +30% is below the arming line; the carried 1.60 keeps the trail armed.
		{"armed below the arming line", 1.30, carried, OptionExitActionProfitTrail, nil, 1.60, 1.12},
		{"fresh peak above the carried mark", 1.70, carried, OptionExitActionProfitTrail, nil, 1.70, 1.19},
		{"stop hit", 1.10, carried, OptionExitActionProfitTake, nil, 1.60, 1.12},
		{"stop exactly reached", 1.12, carried, OptionExitActionProfitTake, nil, 1.60, 1.12},
		// A stop that cannot keep the minimum gain is no take, as for units.
		{"stop below the locked gain", 0.95, OptionExitHighWater{PerShare: 1.40, CostPremium: 1, Quantity: 1}, OptionExitActionProfitTrail, []string{"option_trail_locked_gain_not_met"}, 1.40, 0.98},
		{"reset by a changed cost basis", 1.30, OptionExitHighWater{PerShare: 1.60, CostPremium: 0.90, Quantity: 1}, "", nil, 0, 0},
		{"reset by a changed quantity", 1.30, OptionExitHighWater{PerShare: 1.60, CostPremium: 1, Quantity: 2}, "", nil, 0, 0},
		{"non-finite mark is no mark", 1.30, OptionExitHighWater{PerShare: math.NaN(), CostPremium: 1, Quantity: 1}, "", nil, 0, 0},
		{"no mark: disarmed below the arming line", 1.30, OptionExitHighWater{}, "", nil, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := eligibleOptionExitInput()
			in.Bid, in.Ask, in.CarriedHighWater = tc.bid, tc.bid+0.05, tc.carried
			got := EvaluateOptionExit(in, expiryWindowPolicy())
			if got.Action != tc.action || !slices.Equal(got.Blockers, tc.blockers) || math.Abs(got.HighWater-tc.highWater) > 1e-9 ||
				math.Abs(got.InitialStop-tc.stop) > 1e-9 {
				t.Fatalf("decision = %+v", got)
			}
			if tc.action == OptionExitActionProfitTrail && math.Abs(got.TrailPct-30) > 1e-9 {
				t.Fatalf("trail distance not measured from the high water: %+v", got)
			}
		})
	}
}

func TestOptionInTheMoney(t *testing.T) {
	for _, tc := range []struct {
		right             string
		underlying        float64
		itm, known        bool
		strikeUnavailable bool
	}{
		{"C", 101, true, true, false}, {"CALL", 99, false, true, false}, {"P", 99, true, true, false},
		{"put", 101, false, true, false}, {"P", 100, false, true, false}, {"C", 0, false, false, false},
		{"C", math.Inf(1), false, false, false}, {"", 101, false, false, false}, {"C", 101, false, false, true},
	} {
		strike := 100.0
		if tc.strikeUnavailable {
			strike = math.NaN()
		}
		if itm, known := OptionInTheMoney(tc.right, tc.underlying, strike); itm != tc.itm || known != tc.known {
			t.Fatalf("OptionInTheMoney(%q, %v, %v) = %t, %t", tc.right, tc.underlying, strike, itm, known)
		}
	}
}
