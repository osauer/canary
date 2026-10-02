package rpc

import (
	"errors"
	"math"
	"time"
)

// MethodFX reads cached completed-day valuation attribution.
const MethodFX = "reporting.fx"

// MethodFXBackfill resumes owner-triggered statement acquisition.
const MethodFXBackfill = "reporting.fx.backfill"

// FXSchemaVersion identifies the missing-aware attribution contract.
const FXSchemaVersion = "canary-fx-v1"

// FXResult contains completed reporting-day valuation attribution, never
// intraday P&L, realised currency lots or asset performance. Nil money means
// missing evidence; zero money is a reconciled observation.
type FXResult struct {
	SchemaVersion string     `json:"schema_version"`
	AsOf          time.Time  `json:"as_of"`
	BaseCurrency  string     `json:"base_currency,omitempty"`
	Through       string     `json:"through,omitempty"`
	State         string     `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	Method        string     `json:"method"`
	Days          []FXDay    `json:"days"`
	Periods       []FXPeriod `json:"periods"`
	Backfill      FXBackfill `json:"backfill"`
}

// FXDay carries one reporting-day effect or its evidence failure.
type FXDay struct {
	Day          string   `json:"day"`
	PreviousDay  string   `json:"previous_day,omitempty"`
	Contribution *float64 `json:"contribution,omitempty"`
	Foreign      bool     `json:"foreign_exposure"`
	Reason       string   `json:"reason,omitempty"`
	// Residual checks FX + other movement against NAV less flows. It does
	// not independently certify conversion or external-event classification.
	ReconciliationResidual *float64 `json:"reconciliation_residual,omitempty"`
}

// FXPeriod certifies a total only when all expected days reconcile.
type FXPeriod struct {
	Key          string   `json:"key"`
	From         string   `json:"from"`
	Through      string   `json:"through"`
	State        string   `json:"state"`
	Contribution *float64 `json:"contribution,omitempty"`
	ObservedDays int      `json:"observed_days"`
	ExpectedDays int      `json:"expected_days"`
	MissingDays  []string `json:"missing_days"`
}

// FXBackfill reports acquisition progress without broker credentials.
type FXBackfill struct {
	Running           bool   `json:"running"`
	SnapshotDays      int    `json:"snapshot_days"`
	ExpectedSnapshots int    `json:"expected_snapshots"`
	Reason            string `json:"reason,omitempty"`
}

// ValidateFXResult rejects malformed or uncertified attribution envelopes.
func ValidateFXResult(r FXResult) error {
	if r.SchemaVersion != FXSchemaVersion || r.AsOf.IsZero() || r.Method != "closing_native_book_v1" || r.Days == nil || r.Periods == nil {
		return errors.New("invalid FX envelope")
	}
	if r.State != "available" && r.State != "no_exposure" && r.State != "partial" && r.State != "unavailable" {
		return errors.New("invalid FX result state")
	}
	if r.Backfill.SnapshotDays < 0 || r.Backfill.ExpectedSnapshots < r.Backfill.SnapshotDays {
		return errors.New("invalid FX backfill coverage")
	}
	if r.Through != "" {
		if _, err := time.Parse("2006-01-02", r.Through); err != nil {
			return errors.New("invalid FX cutoff")
		}
	}
	prev := ""
	for _, d := range r.Days {
		if _, err := time.Parse("2006-01-02", d.Day); err != nil || d.Day <= prev {
			return errors.New("invalid FX reporting days")
		}
		prev = d.Day
		for _, v := range []*float64{d.Contribution, d.ReconciliationResidual} {
			if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0)) {
				return errors.New("non-finite FX evidence")
			}
		}
		if d.Contribution == nil && d.Reason == "" {
			return errors.New("missing FX evidence reason")
		}
		if d.Contribution != nil {
			if _, err := time.Parse("2006-01-02", d.PreviousDay); err != nil || d.PreviousDay >= d.Day || d.Reason != "" || d.ReconciliationResidual == nil || math.Abs(*d.ReconciliationResidual) > .03 {
				return errors.New("uncertified daily FX evidence")
			}
		}
	}
	keys := map[string]bool{}
	if len(r.Periods) != 0 && len(r.Periods) != 4 {
		return errors.New("invalid FX period selection")
	}
	for _, p := range r.Periods {
		if keys[p.Key] || (p.Key != "day" && p.Key != "week" && p.Key != "month" && p.Key != "ytd") {
			return errors.New("invalid FX period key")
		}
		keys[p.Key] = true
		if _, err := time.Parse("2006-01-02", p.From); err != nil || p.From > p.Through || p.Through != r.Through {
			return errors.New("invalid FX period boundaries")
		}
		if p.MissingDays == nil || p.ObservedDays < 0 || p.ExpectedDays < 0 || p.ObservedDays > p.ExpectedDays || len(p.MissingDays) != p.ExpectedDays-p.ObservedDays {
			return errors.New("invalid FX period coverage")
		}
		switch p.State {
		case "available", "no_exposure":
			if p.Contribution == nil || len(p.MissingDays) != 0 || p.ExpectedDays == 0 || p.ObservedDays != p.ExpectedDays {
				return errors.New("uncertified FX total")
			}
		case "unavailable", "partial":
			if p.Contribution != nil {
				return errors.New("incomplete FX total")
			}
		default:
			return errors.New("invalid FX period state")
		}
		if p.Contribution != nil && (math.IsNaN(*p.Contribution) || math.IsInf(*p.Contribution, 0)) {
			return errors.New("invalid FX total")
		}
		expected, observed, total := 0, 0, 0.
		missing := []string{}
		for _, d := range r.Days {
			if d.Day < p.From || d.Day > p.Through {
				continue
			}
			expected++
			if d.Contribution == nil {
				missing = append(missing, d.Day)
			} else {
				observed++
				total += *d.Contribution
			}
		}
		if expected != p.ExpectedDays || observed != p.ObservedDays {
			return errors.New("FX coverage differs from daily evidence")
		}
		for i, day := range missing {
			if p.MissingDays[i] != day {
				return errors.New("FX gaps differ from daily evidence")
			}
		}
		if p.Contribution != nil && math.Abs(*p.Contribution-total) > .000001 {
			return errors.New("FX total differs from daily evidence")
		}
		if p.State == "no_exposure" {
			if *p.Contribution != 0 {
				return errors.New("nonzero FX without exposure")
			}
			for _, d := range r.Days {
				if d.Day >= p.From && d.Day <= p.Through && (d.Foreign || d.Contribution == nil || *d.Contribution != 0) {
					return errors.New("foreign FX without exposure")
				}
			}
		}
	}
	if len(r.Periods) == 0 && r.State != "unavailable" {
		return errors.New("FX state without periods")
	}
	return nil
}
