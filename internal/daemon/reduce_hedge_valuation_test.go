package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
)

func TestHedgeExistingContractCannotCrossNeutral(t *testing.T) {
	for _, held := range []float64{-1, 1} {
		// The held and quoted future are the same contract, but the held book
		// was measured at6100 and the fresh quote is6000. The after-score must
		// agree with the valuation used to round the hedge down toward neutral.
		s := hedgeServer(t, mandateReductionStock("SPY", 9101, 2730, 50), mandateReductionFuture(750001, held, 6100))
		res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
		if len(res.Blockers) > 0 || res.IndexLossAfterBase == nil || len(res.Legs) != 1 {
			t.Fatalf("held=%g plan=%+v", held, res)
		}
		if *res.IndexLossAfterBase < 0 {
			t.Fatalf("held=%g quantity=%d fall_before=%g fall_after=%g: sold past neutral", held, res.Legs[0].ReduceQuantity, *res.IndexLossBeforeBase, *res.IndexLossAfterBase)
		}
		want := 3
		if held > 0 {
			want = 5
		}
		if res.Legs[0].ReduceQuantity != want {
			t.Fatalf("held=%g quantity=%d want%d", held, res.Legs[0].ReduceQuantity, want)
		}
		if res.TradeCostBase == nil || *res.TradeCostBase != 0.25*float64(want)*5*0.9 {
			t.Fatalf("fresh quoted cost lost: %+v", res.TradeCostBase)
		}
	}
}
