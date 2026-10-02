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
	if r.SchemaVersion != FXSchemaVersion || r.Days == nil || r.Periods == nil {
		return errors.New("invalid FX envelope")
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
	}
	for _, p := range r.Periods {
		if p.MissingDays == nil || p.ObservedDays > p.ExpectedDays {
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
	}
	return nil
}
