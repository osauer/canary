package rpc

import (
	"github.com/osauer/canary/v2/internal/risk"
	"time"
)

// CashSweepDecisionTrace is a planning transition, not an execution receipt.
// SQLite keeps every event; the read-only proposal snapshot carries recent rows.
type CashSweepDecisionTrace struct {
	OperationalFunding *CashSweepOperationalObservation `json:"operational_funding,omitempty"`
	CalibrationStudies []CashSweepCalibrationStudy      `json:"calibration_studies,omitempty"`
	// Receipt clocks describe request/stream observation, never original TWS
	// field timestamps. Original Web cash time is retained on each currency.
	AccountReceiptAt        time.Time                   `json:"account_receipt_at,omitzero"`
	PositionsReceiptAt      time.Time                   `json:"positions_receipt_at,omitzero"`
	FundingAsOf             time.Time                   `json:"funding_as_of,omitzero"`
	FundingValidUntil       time.Time                   `json:"funding_valid_until,omitzero"`
	ScenarioFingerprint     string                      `json:"scenario_fingerprint,omitempty"`
	PlanningSessionEpoch    uint64                      `json:"planning_session_epoch,omitempty"`
	PlanningDaemonStartedAt time.Time                   `json:"planning_daemon_started_at,omitzero"`
	At                      time.Time                   `json:"at"`
	PolicyID                string                      `json:"policy_id"`
	PolicyVersion           int                         `json:"policy_version"`
	PolicyFingerprint       string                      `json:"policy_fingerprint"`
	Mode                    string                      `json:"mode"`
	CurrencyPriority        string                      `json:"currency_priority,omitempty"`
	ReserveCushionEUR       *float64                    `json:"reserve_cushion_eur,omitempty"`
	ReserveState            string                      `json:"reserve_state,omitempty"`
	ReserveReason           string                      `json:"reserve_reason,omitempty"`
	AccountFingerprint      string                      `json:"account_fingerprint,omitempty"`
	PositionsFingerprint    string                      `json:"positions_fingerprint,omitempty"`
	Currencies              []CashSweepDecisionCurrency `json:"currencies"`
}

// CashSweepDecisionCurrency preserves native money and unavailable values.
type CashSweepDecisionCurrency struct {
	WebCashOriginalAsOf time.Time `json:"web_cash_original_as_of,omitzero"`
	SettledSourceKind   string    `json:"settled_source_kind,omitempty"`
	Currency            string    `json:"currency"`
	Action              string    `json:"action"`
	Reason              string    `json:"reason,omitempty"`
	PriorityRank        int       `json:"priority_rank,omitempty"`
	Cash                *float64  `json:"cash,omitempty"`
	Committed           *float64  `json:"committed,omitempty"`
	FundingNeed         *float64  `json:"funding_need,omitempty"`
	BufferAllocation    *float64  `json:"buffer_allocation,omitempty"`
	EffectiveReserve    *float64  `json:"effective_reserve,omitempty"`
	Free                *float64  `json:"free,omitempty"`
}

// CloneCashSweepDecisionTrace isolates snapshot consumers from retained audit rows.
func CloneCashSweepDecisionTrace(in []CashSweepDecisionTrace) []CashSweepDecisionTrace {
	if in == nil {
		return nil
	}
	out := make([]CashSweepDecisionTrace, len(in))
	for i, t := range in {
		out[i] = t
		out[i].OperationalFunding = risk.CloneCashSweepOperationalObservation(t.OperationalFunding)
		out[i].CalibrationStudies = risk.CloneCashSweepCalibrationStudies(t.CalibrationStudies)
		out[i].ReserveCushionEUR = cloneCashSweepFloat(t.ReserveCushionEUR)
		out[i].Currencies = make([]CashSweepDecisionCurrency, len(t.Currencies))
		for j, c := range t.Currencies {
			out[i].Currencies[j] = c
			v := &out[i].Currencies[j]
			v.Cash, v.Committed, v.FundingNeed = cloneCashSweepFloat(c.Cash), cloneCashSweepFloat(c.Committed), cloneCashSweepFloat(c.FundingNeed)
			v.BufferAllocation, v.EffectiveReserve, v.Free = cloneCashSweepFloat(c.BufferAllocation), cloneCashSweepFloat(c.EffectiveReserve), cloneCashSweepFloat(c.Free)
		}
	}
	return out
}
