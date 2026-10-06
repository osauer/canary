package daemon

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepTraceDurableDedupeAndReturnTransition(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	engine := &proposalEngine{store: &proposalStore{core: core}}
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 100000)
	policy.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityUSDFirst
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	now := time.Now().UTC()
	status := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"EUR": 10000, "USD": 20000}), now).status
	appendTrace := func() {
		t.Helper()
		if err := engine.persistCashSweepTrace(t.Context(), policy, rpc.TradeProposalSourceFingerprints{Account: &rpc.Fingerprint{Key: "synthetic-current"}}, scope, now, &status); err != nil {
			t.Fatal(err)
		}
	}
	appendTrace()
	if status.TraceState != "recorded" || len(status.DecisionTrace) != 1 {
		t.Fatal(status)
	}
	now = now.Add(time.Second)
	appendTrace()
	if len(status.DecisionTrace) != 1 {
		t.Fatal("unchanged poll was logged")
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core, err = corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	engine.store.core = core
	appendTrace()
	if len(status.DecisionTrace) != 1 {
		t.Fatal("restart duplicated unchanged decision")
	}
	policy.PolicyVersion++ // preserve owner edits even with identical cash/state
	appendTrace()
	if len(status.DecisionTrace) != 2 {
		t.Fatal("owner policy change disappeared")
	}
	status.CurrencyPriority = rpc.CashSweepPriorityEURFirst
	appendTrace()
	status.CurrencyPriority = rpc.CashSweepPriorityUSDFirst
	appendTrace()
	if len(status.DecisionTrace) != 4 {
		t.Fatal("return to prior state was deduped")
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{Type: cashSweepTraceEventType})
	if err != nil || len(events) != 4 {
		t.Fatal(len(events), err)
	}
	var trace rpc.CashSweepDecisionTrace
	if err := json.Unmarshal(events[3].PayloadJSON, &trace); err != nil || trace.PolicyVersion != policy.PolicyVersion || len(trace.Currencies) != 2 {
		t.Fatal(trace, err)
	}
	// Responses must never let a consumer mutate the retained audit value.
	*status.DecisionTrace[0].Currencies[0].Cash = -999
	appendTrace()
	if *status.DecisionTrace[0].Currencies[0].Cash < 0 {
		t.Fatal("caller mutated persisted trace")
	}
}

func TestCashSweepTraceRetainsAllEventsBeyondRecentWindowAndSeparatesScopes(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	e := &proposalEngine{store: &proposalStore{core: core}}
	p := cashSweepTestPolicy(rpc.CashSweepModeShadow, 100000)
	st := rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeShadow, Currencies: []rpc.TradeProposalCashSweepCurrency{{Currency: "USD", State: "hold", Cash: new(1000.)}}}
	for i := range 25 {
		st.Currencies[0].Cash = new(float64(i))
		if err := e.persistCashSweepTrace(t.Context(), p, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{Account: "UTEST", Mode: "paper"}, time.Now().UTC(), &st); err != nil {
			t.Fatal(err)
		}
	}
	if len(st.DecisionTrace) != 20 {
		t.Fatal(len(st.DecisionTrace))
	}
	if err := e.persistCashSweepTrace(t.Context(), p, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{Account: "UTEST", Mode: "live"}, time.Now().UTC(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.DecisionTrace) != 1 {
		t.Fatal("paper trace leaked into live")
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{Type: cashSweepTraceEventType})
	if err != nil || len(events) != 26 {
		t.Fatal("old audit events removed", len(events), err)
	}
}

func TestCashSweepTraceUnavailableDoesNotPretendRecorded(t *testing.T) {
	e := &proposalEngine{store: &proposalStore{}}
	st := rpc.TradeProposalCashSweepStatus{}
	if err := e.persistCashSweepTrace(t.Context(), cashSweepTestPolicy("shadow", 100000), rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, time.Now(), &st); err == nil || st.TraceState == "recorded" {
		t.Fatal("missing DB accepted")
	}
}

func TestCashSweepTraceOriginalClocksAndScenarioProvenance(t *testing.T) {
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	e := &proposalEngine{store: &proposalStore{core: core}}
	p := cashSweepTestPolicy(rpc.CashSweepModeShadow, 100000)
	now := time.Now().UTC()
	st := rpc.TradeProposalCashSweepStatus{Mode: "shadow", AccountReceiptAt: now.Add(-time.Second), PositionsReceiptAt: now.Add(-2 * time.Second),
		FundingAsOf: now.Add(-3 * time.Second), FundingValidUntil: now.Add(time.Minute), ScenarioFingerprint: "synthetic-scenario-v1", PlanningSessionEpoch: 7,
		Currencies: []rpc.TradeProposalCashSweepCurrency{{Currency: "EUR", State: "hold", WebCashOriginalAsOf: now.Add(-10 * time.Second), SettledSourceKind: "native_web_ledger"}}}
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	if err := e.persistCashSweepTrace(t.Context(), p, rpc.TradeProposalSourceFingerprints{}, scope, now, &st); err != nil {
		t.Fatal(err)
	}
	trace := st.DecisionTrace[0]
	if trace.AccountReceiptAt != st.AccountReceiptAt || trace.FundingAsOf != st.FundingAsOf || trace.Currencies[0].WebCashOriginalAsOf != st.Currencies[0].WebCashOriginalAsOf || trace.PlanningSessionEpoch != 7 {
		t.Fatal("source clocks lost or replaced with planning time", trace)
	}
	st.AccountReceiptAt, st.FundingAsOf, st.Currencies[0].WebCashOriginalAsOf = now, now, now
	if err := e.persistCashSweepTrace(t.Context(), p, rpc.TradeProposalSourceFingerprints{}, scope, now.Add(time.Second), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.DecisionTrace) != 1 || st.DecisionTrace[0].Currencies[0].WebCashOriginalAsOf != trace.Currencies[0].WebCashOriginalAsOf {
		t.Fatal("unchanged poll refreshed original recorded evidence")
	}
	st.ScenarioFingerprint = "synthetic-scenario-v2"
	if err := e.persistCashSweepTrace(t.Context(), p, rpc.TradeProposalSourceFingerprints{}, scope, now.Add(2*time.Second), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.DecisionTrace) != 2 || st.DecisionTrace[0].ScenarioFingerprint != st.ScenarioFingerprint {
		t.Fatal("changed scenario provenance was deduped")
	}
}
