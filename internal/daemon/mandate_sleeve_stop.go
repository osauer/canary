package daemon

import (
	"fmt"
	"math"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
)

// The owner sleeve's drawdown stop (internal-docs/design/mandate-risk-budget.md,
// "Risk allocations" and "The forced reduction"). The owner delegated the
// setting on 2026-10-18 21:41 CEST ("Need your judgement, be on the
// aggressive side") and the agent set: the stop is 25% of the owner sleeve's
// own high-water mark, acting reduce-only to half the sleeve's risk, with the
// whole book's 30% maximum loss unchanged as the backstop; reviewed when the
// scorecard holds 100 entry weeks. The key is [mandate.sleeves]
// owner_stop_pct; absent, nothing here runs.
//
// Engaged, held, released. The stop engages on the reading at which the
// drawdown reaches the stop and pins the sleeve's use on that verdict, half
// of it the target. It then holds that pin while the drawdown stays within
// the release band under the stop (mandate.SleeveStopReleaseBand: five
// points, 20% for a 25% stop) and releases only on a reading below the band;
// a later reading past the stop is a new engagement and pins the use of its
// own verdict. Added 2026-10-18 22:00 WEST after the quality loop showed the
// stop ratcheting: releasing on the first reading under the stop cleared the
// pin, and a re-engagement on the next pinned half of the already halved
// use, so a drawdown oscillating around the stop cut the sleeve to a quarter,
// an eighth and on, each notch a new reduction batch.
//
// The sleeve's measure. The owner's sleeve has no account of its own, so its
// equity-like measure is derived from the two series the capital state
// already keeps: the book's cash-flow-adjusted equity (the whole-book
// high-water mark's series, observed with every account read) less the
// machine sleeves' marked P&L since the start, measured from the verdict's
// legs: for every leg placed in a machine sleeve, the units held at the last
// reading times the move of its unit value in base currency (the evaluator's
// own pricing: spot and FX for a stock, Black-Scholes at the position's IV
// for an option) since that reading. The tax reserve is subtracted at read
// time, as the book's is, so a changed reserve never creates a drawdown. With
// the machine holding nothing, as it does today, the sleeve's measure is the
// book's risk base and its high-water mark the book's: the 25% stop then sits
// inside the 30% maximum loss exactly as the owner's words place it. A new
// high counts once a second reading at least five minutes later confirms it,
// at the lower of the two, the book's own glitch rule. The series counts from
// mandate.started_at and seeds from the book's high-water mark, so a key
// written after the start inherits the history the book already has; the
// machine's P&L is tracked from the first reading with the key written.
//
// Why not the statement or the broker's P&L: the statement arrives a day
// late and carries no sleeve, and the broker's daily P&L is a whole-account
// figure; both would need the same split by placement the verdict already
// gives, from the legs it prices anyway.

const mandateOwnerSleeveStateVer = 1

// mandateOwnerSleeveStateV1 is the owner sleeve's part of
// riskCapitalStateFileV1, persisted beside the book's high-water mark.
type mandateOwnerSleeveStateV1 struct {
	Version int `json:"version"`
	// StartedAt is the mandate's started_at this series counts from; a
	// different value starts it again.
	StartedAt string `json:"started_at"`
	Seeded    bool   `json:"seeded"`
	// HWMAdjusted is the high-water mark of the sleeve's cash-flow-adjusted
	// equity (the book's adjusted equity less the machine's P&L); HWMAsOf the
	// reading that set it. PendingHigh and PendingHighAt confirm a new high as
	// the book's are confirmed (mandate_capital.go).
	HWMAdjusted   float64   `json:"hwm_adjusted_base"`
	HWMAsOf       time.Time `json:"hwm_as_of,omitzero"`
	PendingHigh   float64   `json:"pending_high_adjusted_base,omitempty"`
	PendingHighAt time.Time `json:"pending_high_at,omitzero"`
	// MachinePnL is the machine sleeves' marked P&L since the series started;
	// Marks the machine legs' units and unit value at the last reading.
	MachinePnL float64                      `json:"machine_pnl_base"`
	Marks      map[string]mandateSleeveMark `json:"machine_marks,omitempty"`
	LastAt     time.Time                    `json:"last_at,omitzero"`
	// EngagedAt is the reading at which the drawdown reached the stop, zero
	// while the stop is not engaged; UseAtStop the sleeve's use on that
	// verdict and TargetUse half of it, the risk the reduction cuts to. Held
	// is true once the engaged stop's drawdown has been back under the stop,
	// inside the release band, so the hold is journaled once an engagement.
	EngagedAt time.Time `json:"engaged_at,omitzero"`
	UseAtStop float64   `json:"use_at_stop_base,omitempty"`
	TargetUse float64   `json:"target_use_base,omitempty"`
	Held      bool      `json:"held,omitempty"`
}

// mandateSleeveMark is one machine leg as last marked.
type mandateSleeveMark struct {
	Quantity float64 `json:"quantity"`
	UnitBase float64 `json:"unit_base"`
}

// mandateMachineMarks reads the machine legs' marks off a measured verdict.
func mandateMachineMarks(v *mandate.Verdict, plan *mandate.AllocationPlan) map[string]mandateSleeveMark {
	out := map[string]mandateSleeveMark{}
	if v == nil {
		return out
	}
	for _, l := range v.Legs {
		if l.Unknown != "" || l.Quantity == 0 || plan.SleeveOf(l.ID) == mandate.SleeveOwner {
			continue
		}
		out[l.ID] = mandateSleeveMark{Quantity: l.Quantity, UnitBase: l.ValueBase / l.Quantity}
	}
	return out
}

// mandateMachineStep is the machine legs' marked P&L between two readings:
// the units held at the last reading times each unit's move in base currency.
func mandateMachineStep(last, now map[string]mandateSleeveMark) float64 {
	step := 0.0
	for id, prev := range last {
		if cur, ok := now[id]; ok {
			step += prev.Quantity * (cur.UnitBase - prev.UnitBase)
		}
	}
	return step
}

// mandateOwnerUse is the owner sleeve's use on a measured verdict: its
// allocation's use, or the book's risk while the verdict carries no
// allocations (every leg is then the owner's).
func mandateOwnerUse(v *mandate.Verdict) float64 {
	if v == nil || v.Risk == nil {
		return 0
	}
	if a, ok := v.Allocation(mandate.SleeveOwner); ok {
		return a.UseBase
	}
	return v.Risk.SDAnnualBase
}

// ObserveOwnerSleeveStop folds one measured verdict into the owner sleeve's
// series and returns the stop's state for the evaluation: the sleeve's
// measure against its high-water mark, whether the stop is engaged and the
// risk it cuts the sleeve to. While the capital state is unknown or the book
// unmeasured the series holds and the state is returned as last persisted,
// Known false, so the reduction sizes nothing new on it. The stop releases
// with hysteresis: engaged at stopPct, it keeps its pinned target until the
// drawdown is back under the release (the stop less the release band).
func (st *riskCapitalStore) ObserveOwnerSleeveStop(c *risk.Constitution, scope brokerStateScope, stopPct float64, capital mandate.Capital, v *mandate.Verdict, plan *mandate.AllocationPlan, now time.Time) mandate.SleeveStop {
	out := mandate.SleeveStop{Sleeve: mandate.SleeveOwner, StopPct: stopPct, ReleasePct: mandate.SleeveStopRelease(stopPct)}
	if st == nil || c == nil || c.Mandate == nil {
		out.Reasons = append(out.Reasons, "no capital state")
		return out
	}
	out.StartedAt = c.Mandate.StartedAt
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if !brokerScopeConcrete(scope) {
			out.Reasons = append(out.Reasons, "capital state requires one selected broker account")
			return out
		}
		if err := st.selectScopeLocked(scope); err != nil {
			out.Reasons = append(out.Reasons, riskCapitalPublicScopeReason(err))
			return out
		}
	}
	reserve := policyCheckDeref(c.Mandate.TaxReserveBase)
	m := st.state.OwnerSleeve
	held := func(reasons ...string) mandate.SleeveStop {
		out.Reasons = append(out.Reasons, reasons...)
		if m != nil && m.StartedAt == c.Mandate.StartedAt && m.Seeded {
			out.HWMRiskBase, out.HWMAsOf, out.MachinePnLBase = m.HWMAdjusted-reserve, m.HWMAsOf, m.MachinePnL
			out.Engaged, out.EngagedAt, out.UseAtStopBase, out.TargetUseBase = !m.EngagedAt.IsZero(), m.EngagedAt, m.UseAtStop, m.TargetUse
		}
		return out
	}
	book := st.state.Mandate
	switch {
	case !capital.Known:
		return held(capital.Reasons...)
	case v == nil || v.Risk == nil:
		return held("the book is not measured")
	case book == nil || !book.Seeded || book.StartedAt != c.Mandate.StartedAt:
		return held("the book's high-water mark is not seeded")
	}
	force := false
	marks := mandateMachineMarks(v, plan)
	if m == nil || m.StartedAt != c.Mandate.StartedAt {
		// Seeded from the book's own mark: with the machine holding nothing
		// the sleeve's series is the book's, and the machine's P&L counts
		// from here.
		m = &mandateOwnerSleeveStateV1{Version: mandateOwnerSleeveStateVer, StartedAt: c.Mandate.StartedAt, Seeded: true,
			HWMAdjusted: book.HWMAdjustedEquity, HWMAsOf: book.HWMAsOf, Marks: marks, LastAt: now}
		st.state.OwnerSleeve = m
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "mandate_owner_stop_started", "started_at": m.StartedAt, "stop_pct": stopPct,
			"hwm_adjusted_base": m.HWMAdjusted, "machine_legs": len(marks), "policy_fingerprint": constitutionFingerprint(c),
		})
		force = true
	} else if !now.Before(m.LastAt) {
		m.MachinePnL += mandateMachineStep(m.Marks, marks)
		m.Marks, m.LastAt = marks, now
	}
	adjusted := capital.EquityBase - capital.FlowsSinceStart - m.MachinePnL
	switch {
	case adjusted <= m.HWMAdjusted:
		m.PendingHigh, m.PendingHighAt = 0, time.Time{}
	case m.PendingHighAt.IsZero():
		m.PendingHigh, m.PendingHighAt = adjusted, now
	case now.Sub(m.PendingHighAt) < mandateHighConfirm:
		m.PendingHigh = math.Min(m.PendingHigh, adjusted)
	default:
		to := math.Min(m.PendingHigh, adjusted)
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "mandate_owner_hwm_advanced", "from_base": m.HWMAdjusted, "to_base": to,
			"machine_pnl_base": m.MachinePnL, "policy_fingerprint": constitutionFingerprint(c),
		})
		m.HWMAdjusted, m.HWMAsOf = to, now
		m.PendingHigh, m.PendingHighAt = 0, time.Time{}
		if adjusted > to {
			m.PendingHigh, m.PendingHighAt = adjusted, now
		}
		force = true
	}
	hwmRisk := m.HWMAdjusted - reserve
	if hwmRisk <= 0 {
		_ = st.persistLocked(force)
		return held(fmt.Sprintf("the tax reserve %.0f %s leaves no sleeve risk base above it", reserve, capital.BaseCurrency))
	}
	out.Known = true
	out.RiskBase, out.HWMRiskBase, out.HWMAsOf, out.MachinePnLBase = adjusted-reserve, hwmRisk, m.HWMAsOf, m.MachinePnL
	out.DrawdownBase = math.Max(0, m.HWMAdjusted-adjusted)
	out.DrawdownPct = out.DrawdownBase / hwmRisk * 100
	// Engage at the stop, hold the pin through the release band, release
	// under it: a drawdown oscillating around the stop keeps the first
	// engagement's target, so a sleeve cut to half is never cut to a quarter.
	switch engaged := !m.EngagedAt.IsZero(); {
	case !engaged && out.DrawdownPct >= stopPct:
		use := mandateOwnerUse(v)
		m.EngagedAt, m.UseAtStop, m.TargetUse, m.Held = now, use, mandate.SleeveStopHalf*use, false
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "mandate_owner_stop_engaged", "drawdown_pct": out.DrawdownPct, "stop_pct": stopPct, "release_pct": out.ReleasePct,
			"hwm_risk_base": hwmRisk, "risk_base": out.RiskBase, "use_base": use, "target_use_base": m.TargetUse, "policy_fingerprint": constitutionFingerprint(c),
		})
		force = true
	case engaged && out.DrawdownPct < out.ReleasePct:
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "mandate_owner_stop_released", "drawdown_pct": out.DrawdownPct, "stop_pct": stopPct, "release_pct": out.ReleasePct,
			"engaged_at": m.EngagedAt.UTC(), "use_at_stop_base": m.UseAtStop, "target_use_base": m.TargetUse, "held": m.Held, "policy_fingerprint": constitutionFingerprint(c),
		})
		m.EngagedAt, m.UseAtStop, m.TargetUse, m.Held = time.Time{}, 0, 0, false
		force = true
	case engaged && out.DrawdownPct < stopPct && !m.Held:
		m.Held = true
		st.appendRiskPolicyJournal(map[string]any{
			"version": 1, "at": now.UTC(), "kind": "mandate_owner_stop_held", "drawdown_pct": out.DrawdownPct, "stop_pct": stopPct, "release_pct": out.ReleasePct,
			"engaged_at": m.EngagedAt.UTC(), "use_at_stop_base": m.UseAtStop, "target_use_base": m.TargetUse, "policy_fingerprint": constitutionFingerprint(c),
		})
		force = true
	}
	out.Engaged, out.EngagedAt, out.UseAtStopBase, out.TargetUseBase = !m.EngagedAt.IsZero(), m.EngagedAt, m.UseAtStop, m.TargetUse
	_ = st.persistLocked(force)
	return out
}

// mandateStopOver reports a verdict whose owner sleeve stop is engaged with
// risk still to cut.
func mandateStopOver(v mandate.Verdict) bool {
	return v.OwnerStop != nil && v.OwnerStop.Over
}

// mandateOwnerStopHolds says why the owner sleeve's stop holds a risk-adding
// order of sleeve, or "" when it does not: while the stop is engaged the
// sleeve is reduce-only, whatever the book reads, through the release band.
func mandateOwnerStopHolds(v *mandate.Verdict, sleeve, ccy string) string {
	if v == nil || v.OwnerStop == nil || !v.OwnerStop.Engaged || sleeve != v.OwnerStop.Sleeve {
		return ""
	}
	return mandateOwnerStopSentence(v.OwnerStop, ccy) + "; the sleeve is reduce-only " + mandateOwnerStopReleaseClause(v.OwnerStop)
}

// mandateOwnerStopSentence states the stop in one clause: "your sleeve is
// 27% below its high-water mark 240,000 EUR, past its 25% stop", or, held
// inside the release band, "your sleeve is 23% below its high-water mark
// 240,000 EUR, under its 25% stop but not yet under the 20% that releases it".
func mandateOwnerStopSentence(s *mandate.SleeveStop, ccy string) string {
	if s.Engaged && s.DrawdownPct < s.StopPct {
		return fmt.Sprintf("%s is %.0f%% below its high-water mark %s, under its %g%% stop but not yet under the %g%% that releases it", mandateSleeveName(s.Sleeve), s.DrawdownPct, mandateMoney(s.HWMRiskBase, ccy), s.StopPct, s.ReleasePct)
	}
	return fmt.Sprintf("%s is %.0f%% below its high-water mark %s, past its %g%% stop", mandateSleeveName(s.Sleeve), s.DrawdownPct, mandateMoney(s.HWMRiskBase, ccy), s.StopPct)
}

// mandateOwnerStopReleaseClause says when an engaged stop lets go: "until its
// drawdown is back under 20%, 5 points under the stop".
func mandateOwnerStopReleaseClause(s *mandate.SleeveStop) string {
	return fmt.Sprintf("until its drawdown is back under %g%%, %g points under the stop", s.ReleasePct, s.StopPct-s.ReleasePct)
}
