//go:build trading

package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// No order path carries a future: a hedge submit is refused past the write
// gate, and nothing is sent.
func TestReduceHedgeSubmitIsRefused(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	res, err := r.srv.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge})
	if err != nil || res.Accepted || len(res.Blockers) != 1 || res.Blockers[0].Code != "bad_request" || len(res.Legs) != 0 {
		t.Fatalf("hedge submit = %+v err %v, want refused as a plan", res, err)
	}
}
