package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A shadow row is observation, never an order. Preview and Submit refuse it
// with shadow_mode before any broker call, even when the row reaches them
// with no blockers of its own, and the scheduler's predicate says no.
func TestShadowProposalsAreRefusedByPreviewAndSubmit(t *testing.T) {
	now := optionExitTestTime()
	shadow := rpc.TradeProposal{Key: "budget_reduction:abc", Revision: "rev-1", Bucket: rpc.TradeProposalBucketBudgetReduction, Shadow: true, NeverSkipVeto: true,
		Action: rpc.OrderActionSell, Quantity: 1, PositionEffect: rpc.OrderPositionEffectReduce, SecType: "OPT", Contract: rpc.ContractParams{ConID: 601, SecType: "OPT"}}
	engine := &proposalEngine{
		server: &Server{cfg: &config.Resolved{}},
		now:    func() time.Time { return now },
		resolve: func(_ context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			if key != shadow.Key || revision != shadow.Revision {
				t.Fatalf("resolver asked for %s@%s", key, revision)
			}
			return shadow, nil, nil
		},
	}
	preview, err := engine.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: shadow.Key, Revision: shadow.Revision})
	if err != nil || preview.Accepted || preview.PreviewTokenID != "" || len(preview.Blockers) != 1 || preview.Blockers[0].Code != "shadow_mode" || preview.Blockers[0].Action == "" || !preview.Proposal.Shadow {
		t.Fatalf("preview = %+v err %v", preview, err)
	}
	for _, fastPath := range []bool{false, true} {
		submit, err := engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: shadow.Key, Revision: shadow.Revision, FastPath: fastPath})
		if err != nil || submit.Accepted || submit.OrderRef != "" || submit.PreviewTokenID != "" || len(submit.Blockers) != 1 || submit.Blockers[0].Code != "shadow_mode" {
			t.Fatalf("submit(fast_path=%v) = %+v err %v", fastPath, submit, err)
		}
	}
	if shadow.AutomaticEligible() {
		t.Fatal("a shadow row must never be automatically eligible")
	}
	if blockers := shadowProposalBlockers(shadow); len(blockers) != 1 || blockers[0].Code != "shadow_mode" {
		t.Fatalf("shadow blockers = %+v", blockers)
	}

	// The same row in active mode passes the shadow gate: the next thing
	// Preview does is ask the broker, which this bare engine cannot, so the
	// gate is proven by the predicate and by the blocker set staying empty.
	active := shadow
	active.Shadow = false
	if !active.AutomaticEligible() || shadowProposalBlockers(active) != nil {
		t.Fatal("an active row was treated as shadow")
	}
	active.Blockers = []rpc.TradingBlocker{{Code: "fresh_option_quote_required"}}
	if active.AutomaticEligible() {
		t.Fatal("a blocked row was automatically eligible")
	}
}
