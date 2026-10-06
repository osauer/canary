package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A refreshed snapshot persists with rows at their own revisions. On
// 2026-10-06 the installed daemon could not save any refresh for 25 minutes
// because the store still demanded every row equal the list revision, and
// kept serving the pre-restart snapshot.
func TestProposalStorePersistsRowsAtTheirOwnRevisions(t *testing.T) {
	now := cashSweepTestNow()
	snap := rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: now,
		Revision: "sha256:list", AccountID: "DU1234567", AccountMode: "paper",
		Proposals: []rpc.TradeProposal{
			{Key: "trailing_stop:1", Revision: "sha256:row", State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketTrailingStop, Quantity: 100, PositionEffect: rpc.OrderPositionEffectClose},
			{Key: "cash_sweep:1", Revision: "sha256:list", State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketCashSweep, Quantity: 1000, PositionEffect: rpc.OrderPositionEffectOpen},
		}}
	if !proposalAuthoritySnapshotValid(snap) {
		t.Fatal("a snapshot with rows at their own revisions reads as malformed")
	}
	blank := snap
	blank.Proposals = []rpc.TradeProposal{{Key: "trailing_stop:1", Bucket: rpc.TradeProposalBucketTrailingStop}}
	if proposalAuthoritySnapshotValid(blank) {
		t.Fatal("a row without a revision reads as well-formed")
	}
	sweep := snap
	sweep.Proposals = []rpc.TradeProposal{{Key: "cash_sweep:1", Revision: "sha256:row", Bucket: rpc.TradeProposalBucketCashSweep}}
	if proposalAuthoritySnapshotValid(sweep) {
		t.Fatal("a sweep row off the list revision reads as well-formed")
	}
	ctx := context.Background()
	core, err := corestore.Open(ctx, corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatalf("open corestore: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := initializeCleanProposalOpportunityAuthority(ctx, core); err != nil {
		t.Fatalf("initialize clean authority: %v", err)
	}
	store := &proposalStore{}
	if _, _, err := store.bindCore(ctx, core); err != nil {
		t.Fatalf("bind core: %v", err)
	}
	if err := store.SaveCurrentWithEvents(ctx, snap, nil); err != nil {
		t.Fatalf("persist a refreshed snapshot: %v", err)
	}
	loaded, err := store.LoadCurrent()
	if err != nil {
		t.Fatalf("load current: %v", err)
	}
	if loaded.Revision != snap.Revision || len(loaded.Proposals) != 2 || loaded.Proposals[0].Revision != "sha256:row" || loaded.Proposals[1].Revision != "sha256:list" {
		t.Fatalf("loaded snapshot = %+v", loaded)
	}
}
