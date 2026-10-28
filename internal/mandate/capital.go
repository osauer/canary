package mandate

import (
	"fmt"
	"math"
	"time"
)

// Policy is the [mandate] table as numbers the evaluation reads: every
// field is the owner's, written into the constitution (risk.ConstitutionMandate).
type Policy struct {
	// MaxLossPct is D, the largest tolerated loss of the budget base.
	MaxLossPct float64
	// VolTargetPct is the annualised volatility budget at full room.
	VolTargetPct float64
	// GapShockPct is the one-name gap each underlying is stressed by.
	GapShockPct float64
	// IndexGapShockPct is the gap a broad index (IsBroadIndex) is stressed
	// by; zero, while the owner has not written it, stresses every
	// underlying by GapShockPct.
	IndexGapShockPct float64
	// GapRoomPct is the share of the remaining loss room one name may lose
	// in that gap.
	GapRoomPct float64
	// IVFallback is the owner's [mandate.iv_fallback]; nil while unwritten,
	// when an option without its own volatility leaves the book unknown.
	IVFallback *IVFallback
}

// CapitalInput is the daemon's capital evidence for the mandate: the last
// equity observation, the owner's reserve, the flows since the start and the
// persisted high-water mark of the cash-flow-adjusted equity.
type CapitalInput struct {
	BaseCurrency string
	// Seeded is false until the first observation on or after the start.
	Seeded bool
	// StartedAt is the start date the high-water mark counts from.
	StartedAt  string
	EquityBase float64
	EquityAsOf time.Time
	// EquityAgeLimit bounds trust in the observation; zero means the owner
	// has not written capital.max_equity_age_minutes, which is unknown.
	EquityAgeLimit time.Duration
	TaxReserveBase float64
	// FlowsSinceStart is the net external flow since the start (deposits
	// positive), in base currency.
	FlowsSinceStart float64
	// HWMAdjustedEquity is the high-water mark of (equity − flows since
	// start): the reserve is subtracted at read time, so a changed reserve
	// moves the high-water mark and today's risk base alike.
	HWMAdjustedEquity float64
	HWMAsOf           time.Time
	Now               time.Time
}

// Capital is the mandate's capital state: the risk base, the budget base,
// the drawdown from the high-water mark and the room left under D. Known is
// false, with Reasons, whenever the evidence cannot establish them; the
// numbers are then zero and must not be read.
type Capital struct {
	Known        bool      `json:"known"`
	Reasons      []string  `json:"reasons,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	StartedAt    string    `json:"started_at,omitempty"`
	EquityBase   float64   `json:"equity_base,omitempty"`
	EquityAsOf   time.Time `json:"equity_as_of,omitzero"`
	EquityStale  bool      `json:"equity_stale,omitempty"`
	// TaxReserveBase is the owner's reserve; RiskBase is equity after it.
	TaxReserveBase float64 `json:"tax_reserve_base,omitempty"`
	RiskBase       float64 `json:"risk_base,omitempty"`
	// FlowsSinceStart is the net external flow since the start.
	FlowsSinceStart float64 `json:"flows_since_start_base,omitempty"`
	// HWMRiskBase is the high-water mark of the cash-flow-adjusted risk
	// base; HWMAsOf is when it was set.
	HWMRiskBase float64   `json:"hwm_risk_base,omitempty"`
	HWMAsOf     time.Time `json:"hwm_as_of,omitzero"`
	// BudgetBase is the high-water mark in today's capital terms (the
	// high-water mark plus the flows since the start): the base the budget
	// and the gap caps are percentages of.
	BudgetBase   float64 `json:"budget_base,omitempty"`
	DrawdownBase float64 `json:"drawdown_base,omitempty"`
	DrawdownPct  float64 `json:"drawdown_pct,omitempty"`
	// Room is 1 − drawdown / D, clamped to [0, 1].
	Room float64 `json:"room"`
	// BudgetAnnualSD is the volatility budget: vol target × budget base ×
	// room, in base currency a year.
	BudgetAnnualSD float64 `json:"budget_annual_sd_base,omitempty"`
}

// Room is the share of the loss budget still open: 1 at a new high, 0 once
// the drawdown reaches D, clamped to [0, 1].
func Room(drawdownPct, maxLossPct float64) float64 {
	if maxLossPct <= 0 || math.IsNaN(drawdownPct) || math.IsInf(drawdownPct, 0) {
		return 0
	}
	return math.Max(0, math.Min(1, 1-drawdownPct/maxLossPct))
}

// EvaluateCapital derives the mandate's capital state from the evidence.
// Missing, stale or inconsistent evidence is unknown, never ok.
func EvaluateCapital(p Policy, in CapitalInput) Capital {
	out := Capital{BaseCurrency: in.BaseCurrency, StartedAt: in.StartedAt, TaxReserveBase: in.TaxReserveBase, FlowsSinceStart: in.FlowsSinceStart}
	if p.MaxLossPct <= 0 || p.VolTargetPct <= 0 {
		out.Reasons = append(out.Reasons, "mandate.max_loss_pct and mandate.vol_target_pct must be written")
	}
	if !in.Seeded {
		out.Reasons = append(out.Reasons, "no equity observation since the mandate start; the high-water mark is not seeded")
	}
	usable := in.EquityBase > 0 && !math.IsNaN(in.EquityBase) && !math.IsInf(in.EquityBase, 0) && !in.EquityAsOf.IsZero() && !in.EquityAsOf.After(in.Now)
	if !usable {
		out.Reasons = append(out.Reasons, "no usable equity observation")
	} else {
		out.EquityBase, out.EquityAsOf = in.EquityBase, in.EquityAsOf
		switch {
		case in.EquityAgeLimit <= 0:
			out.EquityStale = true
			out.Reasons = append(out.Reasons, "capital.max_equity_age_minutes is unapproved, so the equity observation cannot be shown current")
		case in.Now.Sub(in.EquityAsOf) > in.EquityAgeLimit:
			out.EquityStale = true
			out.Reasons = append(out.Reasons, "equity observation is older than capital.max_equity_age_minutes")
		}
	}
	if len(out.Reasons) > 0 {
		return out
	}
	out.RiskBase = in.EquityBase - in.TaxReserveBase
	hwmRiskBase := in.HWMAdjustedEquity - in.TaxReserveBase
	if hwmRiskBase <= 0 || out.RiskBase <= 0 {
		out.Reasons = append(out.Reasons, fmt.Sprintf("the tax reserve %.0f %s leaves no risk base above it", in.TaxReserveBase, in.BaseCurrency))
		return out
	}
	adjustedNow := in.EquityBase - in.FlowsSinceStart
	out.HWMRiskBase, out.HWMAsOf = hwmRiskBase, in.HWMAsOf
	out.DrawdownBase = math.Max(0, in.HWMAdjustedEquity-adjustedNow)
	out.DrawdownPct = out.DrawdownBase / hwmRiskBase * 100
	out.Room = Room(out.DrawdownPct, p.MaxLossPct)
	out.BudgetBase = hwmRiskBase + in.FlowsSinceStart
	if out.BudgetBase <= 0 {
		out.Reasons = append(out.Reasons, "withdrawals since the start exceed the high-water mark of the risk base")
		return out
	}
	out.BudgetAnnualSD = p.VolTargetPct / 100 * out.BudgetBase * out.Room
	out.Known = true
	return out
}

// GapCapBase is the loss one underlying may suffer in a gap: the share
// gap_room_pct of the remaining loss room (room × D × budget base).
func GapCapBase(p Policy, c Capital) float64 {
	if !c.Known {
		return 0
	}
	return c.Room * p.MaxLossPct / 100 * c.BudgetBase * p.GapRoomPct / 100
}
