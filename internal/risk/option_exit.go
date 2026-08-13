package risk

import "math"

const (
	// OptionExitActionLoss selects the event-driven full-close loss proposal.
	OptionExitActionLoss = "loss_exit"
	// OptionExitActionProfitTrail selects the broker-managed profit trail.
	OptionExitActionProfitTrail = "profit_trail"
)

// OptionExitPolicy is the approved, unit-explicit policy for one directional
// long option. It governs advisory exit candidates only; it grants no broker
// write authority.
type OptionExitPolicy struct {
	MinDTE            int
	LossExitPct       float64
	ProfitArmGainPct  float64
	ProfitTrailPct    float64
	LockedGainPct     float64
	MinTrailPct       float64
	MaxTrailPct       float64
	MaxSpreadPctOfMid float64
	MinTrailAbs       float64
	SpreadMultiple    float64
}

// OptionExitInput contains only decision inputs for a single exact contract.
// AvgCost is IBKR's multiplier-inclusive average cost; Bid/Ask are per-share
// option premium quotes.
type OptionExitInput struct {
	ConID               int
	Quantity            float64
	Multiplier          int
	AvgCost             float64
	Bid                 float64
	Ask                 float64
	DTE                 int
	DirectionalIntent   bool
	Standalone          bool
	EconomicRoleAllowed bool
	QuoteLive           bool
	QuoteFresh          bool
	SessionOpen         bool
}

// OptionExitDecision is a pure candidate decision. TrailAmount is the
// unrounded premium-distance sizing evidence; the daemon applies the
// exact-contract tick, converts the result to IBKR's native percentage trail,
// and then rechecks the locked-gain invariant.
type OptionExitDecision struct {
	Action         string
	CostPremium    float64
	ReferencePrice float64
	ReturnPct      float64
	SpreadAbs      float64
	SpreadPctOfMid float64
	TrailAmount    float64
	TrailPct       float64
	InitialStop    float64
	InitialLockPct float64
	Blockers       []string
}

// EvaluateOptionExit applies the approved loss-exit/profit-trail split. An
// empty Action with no blockers means the exact contract is eligible but no
// action threshold has been reached.
func EvaluateOptionExit(in OptionExitInput, pol OptionExitPolicy) OptionExitDecision {
	var out OptionExitDecision
	add := func(code string) { out.Blockers = append(out.Blockers, code) }
	if !optionExitPolicyFinite(pol) {
		add("option_exit_policy_invalid")
		return out
	}
	if !optionExitFinite(in.Quantity) || !optionExitFinite(in.AvgCost) ||
		!optionExitFinite(in.Bid) || !optionExitFinite(in.Ask) {
		add("option_numeric_input_invalid")
		return out
	}
	valuationOK := true
	if in.ConID <= 0 {
		add("exact_contract_required")
	}
	if !in.DirectionalIntent {
		add("directional_intent_required")
	}
	if !in.Standalone {
		add("standalone_option_required")
	}
	if !in.EconomicRoleAllowed {
		add("directional_role_not_confirmed")
	}
	if in.Quantity <= 0 {
		add("long_option_required")
	} else if math.Abs(in.Quantity-math.Round(in.Quantity)) > 1e-9 {
		add("whole_contract_quantity_required")
	}
	if in.DTE < pol.MinDTE {
		add("option_exit_min_dte")
	}
	if in.Multiplier <= 0 || in.AvgCost <= 0 {
		add("option_cost_basis_unavailable")
		valuationOK = false
	} else {
		out.CostPremium = in.AvgCost / float64(in.Multiplier)
	}
	if !in.QuoteLive {
		add("live_option_quote_required")
	}
	if !in.QuoteFresh {
		add("fresh_option_quote_required")
	}
	if !in.SessionOpen {
		add("option_rth_closed")
	}
	if in.Bid <= 0 || in.Ask <= 0 || in.Ask < in.Bid {
		add("two_sided_option_quote_required")
		valuationOK = false
	} else {
		out.ReferencePrice = in.Bid
		out.SpreadAbs = in.Ask - in.Bid
		mid := (in.Ask + in.Bid) / 2
		out.SpreadPctOfMid = out.SpreadAbs / mid * 100
		if out.SpreadPctOfMid > pol.MaxSpreadPctOfMid {
			add("option_spread_too_wide")
		}
	}
	if !valuationOK {
		return out
	}
	// Eligibility and measurement blockers make the threshold unknown. Do not
	// label a stale, delayed, closed-session, wide, hedge-conflicted, or
	// otherwise ineligible row as a loss exit or profit trail merely because
	// its retained numbers cross a line.
	if len(out.Blockers) > 0 {
		return out
	}

	out.ReturnPct = (out.ReferencePrice/out.CostPremium - 1) * 100
	if out.ReturnPct <= -pol.LossExitPct {
		out.Action = OptionExitActionLoss
		return out
	}
	if out.ReturnPct < pol.ProfitArmGainPct {
		return out
	}

	out.Action = OptionExitActionProfitTrail
	out.TrailAmount = math.Max(out.ReferencePrice*pol.ProfitTrailPct/100,
		math.Max(pol.MinTrailAbs, pol.SpreadMultiple*out.SpreadAbs))
	out.TrailPct = out.TrailAmount / out.ReferencePrice * 100
	if out.TrailPct < pol.MinTrailPct || out.TrailPct > pol.MaxTrailPct {
		out.Blockers = append(out.Blockers, "option_trail_outside_policy_bounds")
	}
	out.InitialStop = out.ReferencePrice - out.TrailAmount
	out.InitialLockPct = (out.InitialStop/out.CostPremium - 1) * 100
	if out.InitialLockPct+1e-9 < pol.LockedGainPct {
		out.Blockers = append(out.Blockers, "option_trail_locked_gain_not_met")
	}
	return out
}

// OptionExitLockedGainMet rechecks the policy invariant after daemon-side
// exact tick rounding. Rounding may widen the trail, so the pre-rounding
// evaluation alone is not sufficient.
func OptionExitLockedGainMet(costPremium, initialStop, lockedGainPct float64) bool {
	if !optionExitFinite(costPremium) || !optionExitFinite(initialStop) || !optionExitFinite(lockedGainPct) ||
		costPremium <= 0 || initialStop <= 0 {
		return false
	}
	return (initialStop/costPremium-1)*100+1e-9 >= lockedGainPct
}

// OptionExitTrailPctWithinBounds rechecks the actual broker amount after
// exact-tick rounding. Rounding upward can otherwise move a valid pure-policy
// amount beyond the approved maximum.
func OptionExitTrailPctWithinBounds(referencePrice, trailAmount, minPct, maxPct float64) bool {
	if !optionExitFinite(referencePrice) || !optionExitFinite(trailAmount) ||
		!optionExitFinite(minPct) || !optionExitFinite(maxPct) || referencePrice <= 0 || trailAmount <= 0 {
		return false
	}
	pct := trailAmount / referencePrice * 100
	return pct+1e-9 >= minPct && pct <= maxPct+1e-9
}

func optionExitPolicyFinite(pol OptionExitPolicy) bool {
	values := []float64{
		pol.LossExitPct, pol.ProfitArmGainPct, pol.ProfitTrailPct, pol.LockedGainPct,
		pol.MinTrailPct, pol.MaxTrailPct, pol.MaxSpreadPctOfMid, pol.MinTrailAbs, pol.SpreadMultiple,
	}
	for _, value := range values {
		if !optionExitFinite(value) {
			return false
		}
	}
	return true
}

func optionExitFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
