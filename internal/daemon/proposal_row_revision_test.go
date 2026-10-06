package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A cash sweep sized from free cash moves on every refresh while net
// liquidation value moves. That must move the list and the sweep row, never
// a stop the owner is confirming: 2026-10-05 a trailing stop was refused
// stale_revision because the sweep re-sized between prepare and submit.
func TestRowRevisionStandsWhenAnotherRowMoves(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	fp := rpc.Fingerprint{Key: "p"}
	sources := rpc.TradeProposalSourceFingerprints{Positions: &rpc.Fingerprint{Version: "positions-fp-v1", Key: "sha256:a"}}
	rows := func(cash float64) []rpc.TradeProposal {
		plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": cash}), now)
		cashSweepResolveBills(context.Background(), usBillSource(now), policy.Cash.Sweep, &plan, now)
		out := []rpc.TradeProposal{{Key: "trailing_stop:1", Bucket: rpc.TradeProposalBucketTrailingStop, Quantity: 100, PositionEffect: rpc.OrderPositionEffectClose}}
		for _, cp := range plan.currencies {
			if cp.side != "" {
				out = append(out, cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, sources, now, plan, cp))
			}
		}
		return out
	}
	before, after := rows(60000), rows(61000)
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("rows = %d and %d, want a stop and a sweep each", len(before), len(after))
	}
	if proposalRevision(fp, sources, scope, before) == proposalRevision(fp, sources, scope, after) {
		t.Fatal("a changed sweep order did not move the list revision")
	}
	stop := proposalRowRevision(fp, sources, scope, before[0])
	if stop != proposalRowRevision(fp, sources, scope, after[0]) {
		t.Fatal("the sweep's new size moved the stop's revision")
	}
	if proposalRowRevision(fp, sources, scope, before[1]) == proposalRowRevision(fp, sources, scope, after[1]) {
		t.Fatal("the sweep's new size left its own row revision")
	}
	if stop == proposalRevision(fp, sources, scope, before[:1]) {
		t.Fatal("a row revision collides with the one-row list revision")
	}
	// The row still binds what the list binds: its own size, the policy and
	// the position evidence.
	smaller := before[0]
	smaller.Quantity = 50
	if proposalRowRevision(fp, sources, scope, smaller) == stop {
		t.Fatal("a changed stop quantity did not move its revision")
	}
	if proposalRowRevision(rpc.Fingerprint{Key: "q"}, sources, scope, before[0]) == stop {
		t.Fatal("a changed policy did not move the row revision")
	}
	moved := rpc.TradeProposalSourceFingerprints{Positions: &rpc.Fingerprint{Version: "positions-fp-v1", Key: "sha256:b"}}
	if proposalRowRevision(fp, moved, scope, before[0]) == stop {
		t.Fatal("changed positions did not move the row revision")
	}
}

// Preview and submit resolve a key against its own row revision: the list
// revision is not a row's, and a missing key is reported as missing.
func TestCachedProposalResolvesByRowRevision(t *testing.T) {
	now := cashSweepTestNow()
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	e := &proposalEngine{now: func() time.Time { return now }}
	e.scope = func() brokerStateScope { return scope }
	e.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AsOf: now, Revision: "sha256:list", AccountID: scope.Account, AccountMode: scope.Mode,
		Proposals: []rpc.TradeProposal{{Key: "trailing_stop:1", Revision: "sha256:row", State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketTrailingStop, Quantity: 100, PositionEffect: rpc.OrderPositionEffectClose}}}
	cases := []struct{ key, revision, want string }{
		{"trailing_stop:1", "sha256:row", ""},
		{"trailing_stop:1", "sha256:list", "stale_revision"},
		{"trailing_stop:2", "sha256:row", "proposal_not_found"},
	}
	for _, c := range cases {
		prop, blockers, handled := e.fastPathCachedProposal(c.key, c.revision)
		if !handled {
			t.Fatalf("%s@%s: the cached stop was not served", c.key, c.revision)
		}
		got := ""
		if len(blockers) > 0 {
			got = blockers[0].Code
		}
		if got != c.want {
			t.Fatalf("%s@%s: blocker = %q, want %q", c.key, c.revision, got, c.want)
		}
		if c.want == "" && prop.Key != c.key {
			t.Fatalf("%s@%s: served %q", c.key, c.revision, prop.Key)
		}
	}
	if !proposalUnblocked(e.snapshot, e.snapshot.Proposals[0]) {
		t.Fatal("a served row at its own revision reads as blocked")
	}
	foreign := e.snapshot.Proposals[0]
	foreign.Revision = "sha256:list"
	if proposalUnblocked(e.snapshot, foreign) {
		t.Fatal("a row at the list revision reads as served")
	}
}
