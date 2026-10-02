package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// This reproduces the complete proposal-snapshot transport failure with
// synthetic funding detail; private account data never belongs in this fixture.
func TestCashSweepHistorySnapshotFitsObserverWithoutLosingAudit(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	e := &proposalEngine{store: &proposalStore{core: core}}
	now := time.Date(2026, 10, 2, 7, 0, 0, 0, time.UTC)
	st := rpc.TradeProposalCashSweepStatus{Mode: "shadow", TraceState: "recorded",
		OperationalFunding: &risk.CashSweepOperationalObservation{AsOf: now, AccountReceiptAt: now.Add(-time.Second), PositionsReceiptAt: now.Add(-2 * time.Second), Source: "frozen_synthetic", State: "partial", PolicyFingerprint: "synthetic-policy",
			Gaps:       []string{"finite_portfolio_stress_uncommissioned", "commitment_overlap_unreconciled"},
			Currencies: []risk.CashSweepOperationalCurrency{{Currency: "EUR", KnownGrossComponents: 40000, GrossPrincipal: nil, Gaps: []string{"assignment_exit_cash_timing_unreconciled"}}, {Currency: "USD", KnownGrossComponents: 80000, GrossPrincipal: new(80000.)}}},
		Currencies: []rpc.TradeProposalCashSweepCurrency{{Currency: "EUR", State: "hold", Cash: new(20000.), FundingNeed: nil, EffectiveReserve: nil}, {Currency: "USD", State: "hold", Cash: new(40000.), FundingNeed: new(80000.), EffectiveReserve: new(80000.), Free: new(-40000.)}}}
	for i := range 144 {
		st.OperationalFunding.Obligations = append(st.OperationalFunding.Obligations, risk.CashSweepFundingObligation{ConID: 30000 + i, Currency: "USD", Kind: "synthetic_assignment", GrossPrincipal: nil, IndicativePrincipal: new(5000.), CoveredShares: new(0.), EarliestSettlement: now.Add(24 * time.Hour), QuoteOriginalAt: now.Add(-time.Second), DeliverableOriginalAt: now.Add(-time.Minute), Gaps: []string{"automatic_exercise_policy_unavailable", "settled_cover_shares_unavailable", "cash_settlement_timing_unavailable"}})
	}
	for _, sessions := range []int{1, 2, 5} {
		st.CalibrationStudies = append(st.CalibrationStudies, risk.CashSweepCalibrationStudy{Sessions: sessions, Source: "frozen_synthetic", State: "partial", PolicyFingerprint: "synthetic-policy", AsOf: now, HorizonEnd: now.Add(time.Duration(sessions) * 24 * time.Hour), WorstLossEUR: new(10000.), NAVAfterEUR: new(250000.), ProtectedFloor: new(200000.), ExitComplete: new(false), ExitFrictionEUR: new(100.), Gaps: []string{"stressed_broker_margin_unavailable", "scenario_acceptance_uncommissioned", "assignment_exit_cash_timing_unreconciled"}})
	}
	policy := cashSweepTestPolicy("shadow", 100000)
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	for i := range 20 {
		st.Currencies[0].Cash = new(20000. + float64(i))
		if err := e.persistCashSweepTrace(t.Context(), policy, rpc.TradeProposalSourceFingerprints{}, scope, now.Add(time.Duration(i)*time.Minute), &st); err != nil {
			t.Fatal(err)
		}
	}
	e.scope = func() brokerStateScope { return scope }
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: now, Revision: "synthetic-revision", AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{}, CashSweep: &st}
	full, err := json.Marshal(e.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) <= 128<<10 {
		t.Fatalf("fixture no longer reproduces payload overflow: %d bytes", len(full))
	}
	s := &Server{tradeProposals: e}
	got := s.handleTradeProposalsSnapshot(&rpc.Request{})
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("complete proposal snapshot: full=%d compact=%d observer_limit=%d", len(full), len(raw), 128<<10)
	if len(raw) > 128<<10 {
		t.Fatalf("read-only proposal snapshot exceeds Desk observer limit: %d bytes", len(raw))
	}
	if len(got.CashSweep.DecisionTrace) != 20 || len(got.CashSweep.OperationalFunding.Obligations) != 144 || len(got.CashSweep.CalibrationStudies) != 3 {
		t.Fatal("current evidence or decision window lost")
	}
	if got.CashSweep.DecisionTrace[0].Currencies[0].FundingNeed != nil || *got.CashSweep.DecisionTrace[0].Currencies[1].Free != -40000 {
		t.Fatal("native money or unknown funding was changed")
	}
	retained, err := json.Marshal(e.snapshot)
	if err != nil || string(retained) != string(full) {
		t.Fatal("read-only compaction mutated engine current", err)
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{Type: cashSweepTraceEventType})
	if err != nil || len(events) != 20 {
		t.Fatal("audit events lost", len(events), err)
	}
	for _, event := range events {
		var trace rpc.CashSweepDecisionTrace
		if err := json.Unmarshal(event.PayloadJSON, &trace); err != nil {
			t.Fatal(err)
		}
		if len(trace.OperationalFunding.Obligations) != 144 || len(trace.CalibrationStudies) != 3 {
			t.Fatal("full SQLite audit detail was compacted")
		}
	}
	// The deduplicated current SQLite trace state also stays full, including
	// when it is loaded again after a read-only snapshot was served.
	if err := e.persistCashSweepTrace(t.Context(), policy, rpc.TradeProposalSourceFingerprints{}, scope, now.Add(time.Hour), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.DecisionTrace[0].OperationalFunding.Obligations) != 144 || len(st.DecisionTrace[0].CalibrationStudies) != 3 {
		t.Fatal("current SQLite trace state was compacted")
	}
}
