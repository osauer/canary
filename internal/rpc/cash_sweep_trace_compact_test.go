package rpc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

func TestCashSweepHistoryProjectionPreservesUnknownsAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC)
	funding := &risk.CashSweepOperationalObservation{Source: "frozen_synthetic", State: "partial", AsOf: now, AccountReceiptAt: now.Add(-time.Second), PositionsReceiptAt: now.Add(-2 * time.Second), Gaps: []string{"funding_unavailable"}, Currencies: []risk.CashSweepOperationalCurrency{{Currency: "USD", GrossPrincipal: nil, Gaps: []string{"assignment_unknown"}}}, Obligations: []risk.CashSweepFundingObligation{{ConID: 30000, Currency: "USD", GrossPrincipal: nil}}}
	studies := []risk.CashSweepCalibrationStudy{{Sessions: 1, Source: "frozen_synthetic", State: "partial", AsOf: now}}
	snap := TradeProposalSnapshot{CashSweep: &TradeProposalCashSweepStatus{OperationalFunding: funding, CalibrationStudies: studies, DecisionTrace: []CashSweepDecisionTrace{{At: now, OperationalFunding: funding, CalibrationStudies: studies, Currencies: []CashSweepDecisionCurrency{{Currency: "USD", Cash: new(0.), FundingNeed: nil, Free: new(-25.), WebCashOriginalAsOf: now.Add(-3 * time.Second), SettledSourceKind: "native_web_ledger"}}}, {At: now.Add(-time.Minute)}}}}
	original, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	retained := snap
	CompactCashSweepDecisionHistory(&snap)
	row := snap.CashSweep.DecisionTrace[0]
	if row.DetailAvailability == nil || row.DetailAvailability.State != CashSweepTraceDetailsRetained || row.DetailAvailability.FundingObligationCount == nil || *row.DetailAvailability.FundingObligationCount != 1 || row.DetailAvailability.CalibrationStudyCount == nil || *row.DetailAvailability.CalibrationStudyCount != 1 {
		t.Fatal("omitted details were not explicitly counted", row.DetailAvailability)
	}
	if len(row.OperationalFunding.Obligations) != 0 || len(row.CalibrationStudies) != 0 {
		t.Fatal("repeated detail survived projection")
	}
	if row.OperationalFunding.Source != funding.Source || row.OperationalFunding.State != funding.State || row.OperationalFunding.AccountReceiptAt != funding.AccountReceiptAt || row.OperationalFunding.PositionsReceiptAt != funding.PositionsReceiptAt || row.OperationalFunding.Gaps[0] != funding.Gaps[0] || row.OperationalFunding.Currencies[0].GrossPrincipal != nil || row.OperationalFunding.Currencies[0].Gaps[0] != "assignment_unknown" {
		t.Fatal("source gaps/clocks/native funding unknown lost")
	}
	c := row.Currencies[0]
	if c.Cash == nil || *c.Cash != 0 || c.FundingNeed != nil || c.Free == nil || *c.Free != -25 || c.WebCashOriginalAsOf != now.Add(-3*time.Second) || c.SettledSourceKind != "native_web_ledger" {
		t.Fatal("native cash, zero/unknown distinction or original clock changed", c)
	}
	missing := snap.CashSweep.DecisionTrace[1].DetailAvailability
	if missing == nil || missing.FundingObligationCount != nil || missing.CalibrationStudyCount != nil || snap.CashSweep.DecisionTrace[1].OperationalFunding != nil {
		t.Fatal("unavailable history invented an empty observation", missing)
	}
	if len(snap.CashSweep.OperationalFunding.Obligations) != 1 || len(snap.CashSweep.CalibrationStudies) != 1 {
		t.Fatal("current evidence compacted")
	}
	first, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	CompactCashSweepDecisionHistory(&snap)
	second, err := json.Marshal(snap)
	if err != nil || string(first) != string(second) {
		t.Fatal("projection is not idempotent", err)
	}
	*row.DetailAvailability.FundingObligationCount = 99
	row.OperationalFunding.Gaps[0] = "mutated"
	*row.Currencies[0].Free = 0
	unchanged, err := json.Marshal(retained)
	if err != nil || string(unchanged) != string(original) {
		t.Fatal("projection mutated retained source", err)
	}
	if *snap.CashSweep.DecisionTrace[0].DetailAvailability.FundingObligationCount != 1 {
		t.Fatal("second projection aliases prior metadata")
	}
}

func TestCashSweepHistoryProjectionKeepsKnownEmptyCounts(t *testing.T) {
	snap := TradeProposalSnapshot{CashSweep: &TradeProposalCashSweepStatus{DecisionTrace: []CashSweepDecisionTrace{{OperationalFunding: &risk.CashSweepOperationalObservation{Obligations: []risk.CashSweepFundingObligation{}}, CalibrationStudies: []risk.CashSweepCalibrationStudy{}}}}}
	CompactCashSweepDecisionHistory(&snap)
	detail := snap.CashSweep.DecisionTrace[0].DetailAvailability
	if detail.FundingObligationCount == nil || *detail.FundingObligationCount != 0 || detail.CalibrationStudyCount == nil || *detail.CalibrationStudyCount != 0 {
		t.Fatal("known empty detail collapsed into unavailable", detail)
	}
}
