//go:build trading

package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A reviewed basket never sells a line the owner locked after the review:
// the leg is refused at submit and nothing reaches the broker.
func TestReduceBasketRefusesALineLockedAfterReview(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview, err := r.srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
		t.Fatalf("preview = %+v, want one tokened leg", preview)
	}
	if _, err = replaceHoldingLocks(t, r.srv, []rpc.HoldingLock{{ConID: preview.Legs[0].ConID, Symbol: "SYN", Role: rpc.HoldingLockRoleCore}}, 0, "lock-after-review"); err != nil {
		t.Fatal(err)
	}
	res, err := r.srv.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{PreviewTokens: []string{preview.Legs[0].PreviewToken}, RequestRef: "basket-locked"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || len(res.Legs) != 1 || firstBlockerCode(res.Legs[0].Blockers) != "holding_locked" || len(r.sends()) != 0 {
		t.Fatalf("submit = %+v sends %d, want holding_locked and nothing sent", res, len(r.sends()))
	}
	again, err := r.srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Legs) != 0 || len(again.Kept) != 1 || again.Kept[0].Reason != reduceKeptLocked {
		t.Fatalf("preview after locking = %+v, want no legs and the line kept as locked", again)
	}
}
