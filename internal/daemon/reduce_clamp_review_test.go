package daemon

import (
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
)

func TestReduceWorkingCommitmentScalesReportedRelief(t *testing.T) {
	row := reduceTestOptionRow(5)
	srv, _ := newReducePreviewTestServer(t, row)
	appendReduceComboReservation(t, srv, 4, 4, []rpc.StrategyOrderLeg{{Contract: rpc.ContractParams{ConID: row.ConID}, Ratio: -1, Action: rpc.OrderActionSell}})
	leg, preview := srv.reduceLegPrepare(t.Context(), reduceSweepCandidate{row: row, qty: 2, dollarDelta: 6250, allocatedDollars: 2500}, 0, "")
	if preview == nil || leg.ReduceQuantity != 1 {
		t.Fatalf("fixture: preview=%v leg=%+v", preview != nil, leg)
	}
	if leg.RiskContributionCut != 1250 {
		t.Fatalf("relief=%g, want1250 for1 contract", leg.RiskContributionCut)
	}
	res := &rpc.TradeProposalReducePortfolioResult{Legs: []rpc.TradeProposalReduceLeg{leg}, TargetDollarDelta: 2500}
	aggregateBasket(res, false)
	if res.AchievedDollarDelta != 1250 || res.AchievedPctOfTarget == nil || *res.AchievedPctOfTarget != 50 {
		t.Fatalf("basket relief=%g percent=%v", res.AchievedDollarDelta, res.AchievedPctOfTarget)
	}
}
