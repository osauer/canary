package rpc

import (
	"github.com/osauer/canary/v2/internal/risk"
	"time"
)

// CashSweepDecisionTrace is a planning transition, not an execution receipt.
// SQLite keeps every event; the read-only proposal snapshot carries recent rows.
type CashSweepDecisionTrace struct {
	OperationalFunding *CashSweepOperationalObservation  `json:"operational_funding,omitempty"`
	CalibrationStudies []CashSweepCalibrationStudy       `json:"calibration_studies,omitempty"`
	DetailAvailability *CashSweepTraceDetailAvailability `json:"detail_availability,omitempty"`
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

// CashSweepTraceDetailAvailability distinguishes compact transport history
// from an observation with no obligations or studies. Full details remain in
// Canary's SQLite audit. A nil count means the original list was unavailable.
type CashSweepTraceDetailAvailability struct {
	State                  string `json:"state"`
	FundingObligationCount *int   `json:"funding_obligation_count,omitempty"`
	CalibrationStudyCount  *int   `json:"calibration_study_count,omitempty"`
}

// CashSweepTraceDetailsRetained identifies omitted transport detail preserved
// in Canary's full SQLite audit; it never states that the detail is absent.
const CashSweepTraceDetailsRetained = "retained_in_canary_audit"

// CompactCashSweepDecisionHistory makes a transport-only copy. Current
// funding/studies stay full, and callers must persist the original snapshot.
func CompactCashSweepDecisionHistory(snap *TradeProposalSnapshot) {
	if snap == nil || snap.CashSweep == nil || len(snap.CashSweep.DecisionTrace) == 0 {
		return
	}
	snap.CashSweep = CloneCashSweepStatus(snap.CashSweep)
	for i := range snap.CashSweep.DecisionTrace {
		trace := &snap.CashSweep.DecisionTrace[i]
		// Preserve original counts when a compact snapshot passes through
		// another adapter; omission must never be rewritten as zero.
		if trace.DetailAvailability == nil {
			detail := CashSweepTraceDetailAvailability{State: CashSweepTraceDetailsRetained}
			if trace.OperationalFunding != nil && trace.OperationalFunding.Obligations != nil {
				detail.FundingObligationCount = new(len(trace.OperationalFunding.Obligations))
			}
			if trace.CalibrationStudies != nil {
				detail.CalibrationStudyCount = new(len(trace.CalibrationStudies))
			}
			trace.DetailAvailability = &detail
		}
		if trace.OperationalFunding != nil {
			trace.OperationalFunding.Obligations = nil
		}
		trace.CalibrationStudies = nil
	}
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
		if t.DetailAvailability != nil {
			detail := *t.DetailAvailability
			if detail.FundingObligationCount != nil {
				detail.FundingObligationCount = new(*detail.FundingObligationCount)
			}
			if detail.CalibrationStudyCount != nil {
				detail.CalibrationStudyCount = new(*detail.CalibrationStudyCount)
			}
			out[i].DetailAvailability = &detail
		}
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
