package daemon

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepRefuterMissingConfiguredNativeCashCannotDisappear(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
	policy.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityEURFirst
	in := observedSweepFundingInput(map[string]float64{"EUR": 50000})
	in.FundingEvidence = sweepFundingFixture(now)
	in.FundingEvidence.FundingNative["USD"] = 0
	plan := cashSweepPlanFor(policy, in, now)
	if got := cashSweepCurrencyOf(t, plan, "EUR"); got.side != "" {
		t.Fatalf("missing configured USD cash/floor silently omitted, EUR investment admitted: %+v", got.status)
	}
}

func TestCashSweepRefuterEveryNativeReserveMustBeCertifiedAndFunded(t *testing.T) {
	for _, kind := range []string{"missing-settled", "unobserved", "working-order-deficit", "unknown-commitment", "nonfinite-settled", "pending-sale", "stale-ledger-scope"} {
		t.Run(kind, func(t *testing.T) {
			now := cashSweepTestNow()
			policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 100000)
			policy.Cash.Sweep.CurrencyPriority = rpc.CashSweepPriorityEURFirst
			in := observedSweepFundingInput(map[string]float64{"EUR": 50000, "USD": 50000})
			in.FundingEvidence = sweepFundingFixture(now)
			row := in.Ledger["USD"]
			switch kind {
			case "missing-settled":
				row.Settled = nil
			case "unobserved":
				row.Observed = false
			case "working-order-deficit":
				in.Commitments.ByCurrency["USD"] = 45000
			case "unknown-commitment":
				in.Commitments.Unknown["USD"] = "synthetic unresolved order receipt"
			case "nonfinite-settled":
				row.Settled = new(math.NaN())
			case "pending-sale":
				row.TradeDate = 50000
				row.Settled = new(1000.)
				in.Settlement.EquivalentSales["USD"] = 49000
			case "stale-ledger-scope":
				in.LedgerReason = "synthetic stale or wrong-session cash authority"
			}
			in.Ledger["USD"] = row
			plan := cashSweepPlanFor(policy, in, now)
			if got := cashSweepCurrencyOf(t, plan, "EUR"); got.side != "" {
				t.Fatalf("uncertified USD reserve permitted EUR spend (%s): %+v", kind, got.status)
			}
		})
	}
}
func TestCashSweepRefuterTraceConcurrentPollsAndRestartProvenanceDeduplicate(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	e := &proposalEngine{store: &proposalStore{core: core}}
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 100000)
	scope := brokerStateScope{Account: "UTEST", Mode: "paper"}
	now := time.Now().UTC()
	template := rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeShadow, PlanningSessionEpoch: 7, PlanningDaemonStartedAt: now.Add(-time.Hour), Currencies: []rpc.TradeProposalCashSweepCurrency{{Currency: "EUR", State: "hold", Cash: new(1000.)}}}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			st := template
			// Separate proposal-store locks force the SQLite CAS contract to elect a writer.
			concurrent := &proposalEngine{store: &proposalStore{core: core}}
			failures <- concurrent.persistCashSweepTrace(context.Background(), policy, rpc.TradeProposalSourceFingerprints{}, scope, now, &st)
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := core.LoadEvents(t.Context(), corestore.EventQuery{Type: cashSweepTraceEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("same concurrent decision appended %d events: %v", len(events), err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	core, err = corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	e.store.core = core
	restarted := template
	restarted.PlanningSessionEpoch = 99
	restarted.PlanningDaemonStartedAt = now
	if err := e.persistCashSweepTrace(t.Context(), policy, rpc.TradeProposalSourceFingerprints{}, scope, now.Add(time.Second), &restarted); err != nil {
		t.Fatal(err)
	}
	if len(restarted.DecisionTrace) != 1 || restarted.DecisionTrace[0].PlanningSessionEpoch != 7 || !restarted.DecisionTrace[0].PlanningDaemonStartedAt.Equal(template.PlanningDaemonStartedAt) {
		t.Fatalf("provenance-only restart rewrote or duplicated original decision: %+v", restarted.DecisionTrace)
	}
}
