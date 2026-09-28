package risk

import (
	"math"
	"strings"
)

const (
	// OptionExitActionLoss selects the event-driven full-close loss proposal.
	OptionExitActionLoss = "loss_exit"
	// OptionExitActionProfitTrail selects the broker-managed profit trail.
	OptionExitActionProfitTrail = "profit_trail"
	// OptionExitActionProfitTake selects a full close now: the fresh bid has
	// already fallen to the stop of the profit trail measured from its carried
	// high water, so a broker trail placed now would trigger on arrival.
	OptionExitActionProfitTake = "profit_take"
	// OptionExitActionExpiryClose selects a full close of an in-the-money long
	// option inside the Rulebook's expiry window, so nothing is exercised by
	// accident (owner decision 2026-09-28).
	OptionExitActionExpiryClose = "expiry_close"
)

// OptionExitPolicy is the approved, unit-explicit policy for one directional
// long option. It governs advisory exit candidates only; it grants no broker
// write authority.
type OptionExitPolicy struct {
	// MinDTE is the profit trail's floor in calendar days to expiry. The loss
	// exit and the expiry close apply below it, until expiry.
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
	// ExpiryCloseDTE is the Rulebook's expiry act level (runway_act_dte): at
	// this many calendar days to expiry or fewer an in-the-money long option
	// is closed. A negative value matches no expiry.
	ExpiryCloseDTE int
}

// OptionExitHighWater is a profit trail's high-water mark carried from the
// last published row of the same position: the highest per-share value since
// the trail armed, with the per-share cost basis and the quantity it was
// recorded against.
type OptionExitHighWater struct {
	PerShare    float64
	CostPremium float64
	Quantity    float64
}

// Current returns the carried high water that still applies to a position
// with this per-share cost basis and quantity. It is zero when no mark was
// carried, or when the cost basis or quantity changed since the mark was
// recorded: the trail then starts again from the fresh price.
func (h OptionExitHighWater) Current(costPremium, quantity float64) float64 {
	values := []float64{h.PerShare, h.CostPremium, h.Quantity, costPremium, quantity}
	for _, value := range values {
		if !optionExitFinite(value) {
			return 0
		}
	}
	if h.PerShare <= 0 || math.Abs(h.CostPremium-costPremium) > 1e-9*math.Max(1, math.Abs(costPremium)) ||
		math.Abs(h.Quantity-quantity) > 1e-9 {
		return 0
	}
	return h.PerShare
}

// OptionInTheMoney reports whether a long option is in the money at this
// underlying price: a call with the underlying above its strike, a put with
// the underlying below it. known is false when the right, the strike or the
// underlying price cannot decide it.
func OptionInTheMoney(right string, underlying, strike float64) (itm, known bool) {
	if !optionExitFinite(underlying) || !optionExitFinite(strike) || underlying <= 0 || strike <= 0 {
		return false, false
	}
	right = strings.ToUpper(strings.TrimSpace(right))
	switch {
	case isCall(right):
		return underlying > strike, true
	case isPut(right):
		return underlying < strike, true
	default:
		return false, false
	}
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
	// QuoteSkipped records that the caller deliberately requested no broker
	// quote, because the contract cannot qualify for an exit yet (its purpose is
	// unconfirmed). The valuation stays unavailable, but the absence of a quote
	// is then the caller's decision, not a quote failure, and no quote blocker
	// is reported for it.
	QuoteSkipped bool
	// Right, Strike and Underlying decide moneyness for the expiry close.
	// Underlying is the fresh underlying price from the refresh's
	// exact-contract evidence; zero means unavailable.
	Right      string
	Strike     float64
	Underlying float64
	// CarriedHighWater is the profit trail's high-water mark carried from the
	// leg's last published row. It keeps the trail armed after the gain dips
	// below the arming line; a changed cost basis or quantity resets it.
	CarriedHighWater OptionExitHighWater
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
	// Measured is true when every eligibility, data and session gate passed:
	// ReturnPct is then a measurement, also where a later rule holds the
	// action back.
	Measured       bool
	SpreadAbs      float64
	SpreadPctOfMid float64
	// Underlying is the fresh underlying price the expiry window judged
	// moneyness on; zero outside that window.
	Underlying float64
	// HighWater is the measured profit trail's high-water mark: the fresh bid,
	// or the higher carried mark. TrailAmount, TrailPct, InitialStop and
	// InitialLockPct are measured from it. Zero when no trail was measured.
	HighWater      float64
	TrailAmount    float64
	TrailPct       float64
	InitialStop    float64
	InitialLockPct float64
	Blockers       []string
}

// EvaluateOptionExit applies the approved exits to one long option, in this
// order: the loss exit at the Rulebook loss line, from expiry day (DTE 0)
// onwards; the expiry close of an in-the-money option inside the Rulebook's
// expiry window; and the profit trail from the minimum DTE only, armed at the
// arming gain or by a carried high water and measured from that high water.
// An empty Action with no blockers means the exact contract is eligible but no
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
	// The loss exit and the expiry close apply from expiry day (DTE 0)
	// onwards; the profit trail's floor is checked below, where it would
	// otherwise apply. An unknown or passed expiry supports no exit.
	if in.DTE < 0 {
		add("option_exit_min_dte")
	}
	if in.Multiplier <= 0 || in.AvgCost <= 0 {
		add("option_cost_basis_unavailable")
		valuationOK = false
	} else {
		out.CostPremium = in.AvgCost / float64(in.Multiplier)
	}
	if !in.SessionOpen {
		add("option_rth_closed")
	}
	if in.QuoteSkipped {
		valuationOK = false
	} else if in.Bid <= 0 || in.Ask <= 0 || in.Ask < in.Bid || !in.QuoteLive || !in.QuoteFresh {
		if !in.QuoteLive {
			add("live_option_quote_required")
		}
		if !in.QuoteFresh {
			add("fresh_option_quote_required")
		}
		if in.Bid <= 0 || in.Ask <= 0 || in.Ask < in.Bid {
			add("two_sided_option_quote_required")
		}
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

	out.Measured = true
	out.ReturnPct = (out.ReferencePrice/out.CostPremium - 1) * 100
	if out.ReturnPct <= -pol.LossExitPct {
		out.Action = OptionExitActionLoss
		return out
	}
	if in.DTE <= pol.ExpiryCloseDTE {
		itm, known := OptionInTheMoney(in.Right, in.Underlying, in.Strike)
		if !known {
			// Moneyness decides between closing and holding into expiry; an
			// unknown underlying price supports neither.
			add("option_expiry_underlying_unavailable")
			return out
		}
		out.Underlying = in.Underlying
		if itm {
			out.Action = OptionExitActionExpiryClose
			return out
		}
	}
	carried := in.CarriedHighWater.Current(out.CostPremium, in.Quantity)
	if out.ReturnPct < pol.ProfitArmGainPct && carried <= 0 {
		return out
	}
	if in.DTE < pol.MinDTE {
		// The profit trail keeps its floor; the loss exit still applies.
		add("option_exit_min_dte")
		return out
	}

	// An armed trail stays armed from its peak: the high water carried from
	// the last published row keeps it live after the bid retraces below the
	// arming line, and the trail distance and stop are measured from it.
	out.HighWater = math.Max(out.ReferencePrice, carried)
	out.TrailAmount = math.Max(out.HighWater*pol.ProfitTrailPct/100,
		math.Max(pol.MinTrailAbs, pol.SpreadMultiple*out.SpreadAbs))
	out.TrailPct = out.TrailAmount / out.HighWater * 100
	out.InitialStop = out.HighWater - out.TrailAmount
	out.InitialLockPct = (out.InitialStop/out.CostPremium - 1) * 100
	lockedGainMet := out.InitialLockPct+1e-9 >= pol.LockedGainPct
	if out.ReferencePrice <= out.InitialStop+1e-9 && lockedGainMet {
		out.Action = OptionExitActionProfitTake
		return out
	}
	out.Action = OptionExitActionProfitTrail
	if out.TrailPct < pol.MinTrailPct || out.TrailPct > pol.MaxTrailPct {
		out.Blockers = append(out.Blockers, "option_trail_outside_policy_bounds")
	}
	if !lockedGainMet {
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
