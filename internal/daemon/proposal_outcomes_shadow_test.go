package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A shadow row's daily mark keeps its baseline for the retrospective but is
// never an offer: the brief's offered-versus-acted count leaves it out.
func TestProposalOutcomesShadowMarksAreNotOffers(t *testing.T) {
	store := &proposalOutcomeStore{Path: filepath.Join(t.TempDir(), "outcomes.jsonl")}
	now := time.Date(2026, time.September, 21, 14, 0, 0, 0, time.UTC)
	price := 20.0
	offered := rpc.TradeProposal{Key: "trailing_stop:1", Bucket: rpc.TradeProposalBucketTrailingStop, Symbol: "TEST", LimitPrice: &price}
	shadow := rpc.TradeProposal{Key: "budget_reduction:1", Bucket: rpc.TradeProposalBucketBudgetReduction, Symbol: "AAA", LimitPrice: &price, Shadow: true}
	for _, prop := range []rpc.TradeProposal{offered, shadow} {
		mark := proposalOutcomeMarked(prop, now)
		if mark.Shadow != prop.Shadow {
			t.Fatalf("mark shadow = %v for %s", mark.Shadow, prop.Key)
		}
		if err := store.AppendMark(mark); err != nil {
			t.Fatal(err)
		}
	}
	got, acted, day, ok, err := store.SessionSummary()
	if err != nil || !ok || day != "2026-09-21" || got != 1 || acted != 0 {
		t.Fatalf("summary = offered %d acted %d day %s ok %v err %v; want one offer", got, acted, day, ok, err)
	}
}
