package daemon

import (
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
)

func TestDeskExecutionReplayUnknownBlocksLaterAddition(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.tradeProposals = notFoundEngine(s)
	auth := installDeskMandate(t, s, scope, "full", true)
	earlier := deskExecutionTestItem("unknown-reduction", "reduction-episode", "r1")
	if _, refusal, err := s.recordDeskExecutionRequest(t.Context(), scope, earlier, "automatic:"+auth.ID); err != nil || refusal != nil {
		t.Fatalf("setup %v %v", refusal, err)
	}
	next := deskExecutionStrategyItem("next-addition", "core:synthetic:2026-10:2026-11-02", rpc.DeskClassAdd)
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &auth, Items: []rpc.DeskExecutionItem{earlier, next}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("receipts=%+v", got.Receipts)
	if got.Receipts[0].Outcome != rpc.DeskOutcomeUnknown {
		t.Fatal("setup not unknown")
	}
	if len(got.Receipts[1].Blockers) == 0 || got.Receipts[1].Blockers[0].Code != rpc.DeskBlockerPriorUnknown {
		t.Fatal("addition reached engine despite earlier replayed unknown")
	}
}
func TestDeskExecutionReplayKeepsDurableOutcomeWhenBrokerDown(t *testing.T) {
	for _, outcome := range []string{rpc.DeskOutcomeAccepted, rpc.DeskOutcomeUnknown} {
		t.Run(outcome, func(t *testing.T) {
			s, scope := deskExecutionTestServer(t)
			e := notFoundEngine(s)
			s.tradeProposals = e
			row := rpc.TradeProposal{Key: "core_rebalance:syn", Revision: "r1", Bucket: rpc.TradeProposalBucketCoreRebalance, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, Core: &rpc.TradeProposalCore{Session: "2026-10-01", Sleeve: "core", Terms: rpc.CoreRowTerms{Episode: "core:DU1234567:paper:2026-09"}}}
			e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "r1", AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{row}}
			auth := installDeskMandate(t, s, scope, "full", true)
			item := deskExecutionStrategyItem("existing-request", "core:DU1234567:paper:2026-09:2026-10-01", rpc.DeskClassAdd)
			if _, refusal, err := s.recordDeskExecutionRequest(t.Context(), scope, item, "automatic:"+auth.ID); err != nil || refusal != nil {
				t.Fatalf("setup %v %v", refusal, err)
			}
			if outcome == rpc.DeskOutcomeAccepted {
				if err := s.recordDeskExecutionOutcome(t.Context(), scope, item.RequestID, deskExecutionOutcomeRow{Outcome: outcome, OrderRef: "synthetic-order"}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &auth, Items: []rpc.DeskExecutionItem{item}})
			if err != nil {
				t.Fatal(err)
			}
			lookup := deskExecutionLookup(t, s, item.RequestID)[0]
			t.Logf("submit=%+v lookup=%+v", got.Receipts[0], lookup)
			if got.Receipts[0].Outcome != lookup.Outcome {
				t.Fatal("retry contradicts durable outcome")
			}
		})
	}
}

// Replay preserves request identity even while readiness would refuse a new
// attempt. A prior unknown fences only new additions, not durable receipts.
func TestDeskExecutionReplayPreservesIdentityAndRecordedAdditions(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.tradeProposals = notFoundEngine(s)
	auth := installDeskMandate(t, s, scope, "full", true)
	prior := deskExecutionTestItem("unknown-first", "ep-unknown", "r1")
	accepted := deskExecutionStrategyItem("accepted-add", "ep-accepted", rpc.DeskClassAdd)
	for _, item := range []rpc.DeskExecutionItem{prior, accepted} {
		if _, refusal, err := s.recordDeskExecutionRequest(t.Context(), scope, item, "automatic:"+auth.ID); err != nil || refusal != nil {
			t.Fatalf("setup %v %v", refusal, err)
		}
	}
	if err := s.recordDeskExecutionOutcome(t.Context(), scope, accepted.RequestID, deskExecutionOutcomeRow{Outcome: rpc.DeskOutcomeAccepted, OrderRef: "synthetic-order"}); err != nil {
		t.Fatal(err)
	}
	reduction := deskExecutionTestItem("next-reduction", "ep-next-reduction", "r1")
	got, err := deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &auth, Items: []rpc.DeskExecutionItem{prior, reduction, accepted}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Receipts[0].Outcome != rpc.DeskOutcomeUnknown || got.Receipts[1].Blockers[0].Code != "proposal_not_found" || got.Receipts[2].Outcome != rpc.DeskOutcomeAccepted {
		t.Fatalf("replay or reduce continuation changed: %+v", got.Receipts)
	}
	changed := accepted
	proposal := *accepted.Intent.Proposal
	proposal.Revision = "r2"
	changed.Intent.Proposal = &proposal
	got, err = deskExecutionSubmit(t, s, rpc.DeskExecutionSubmitParams{Automatic: &auth, Items: []rpc.DeskExecutionItem{changed}})
	if err != nil || got.Receipts[0].Blockers[0].Code != rpc.DeskBlockerRequestConflict {
		t.Fatalf("changed identity accepted: %+v %v", got, err)
	}
}
func TestDeskExecutionUnreadableReplayIsUnknown(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	s.tradeProposals = notFoundEngine(s)
	auth := installDeskMandate(t, s, scope, "full", true)
	authority, blocker, err := s.deskAutomaticAuthority(t.Context(), auth, scope)
	if err != nil || blocker != nil {
		t.Fatalf("authority %v %v", blocker, err)
	}
	store := s.coreStore
	s.coreStore = nil
	defer func() { s.coreStore = store }()
	unknown := false
	item := deskExecutionTestItem("unreadable", "ep-unreadable", "r1")
	got := s.deskExecutionSubmitItem(t.Context(), scope, authority, item, &unknown, 0)
	if got.Outcome != rpc.DeskOutcomeUnknown || !unknown {
		t.Fatalf("unreadable journal gave false certainty: %+v fence=%v", got, unknown)
	}
}
