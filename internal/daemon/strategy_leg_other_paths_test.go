package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/strategy"
)

// The bulk reduction sells long options one contract at a time. A long call
// held against a sold call is one leg of a spread: selling it alone leaves
// the sold call uncovered, so every sweep mode holds it and names the
// strategy route. A long option with nothing sold against it stays in scope.
func TestBulkReductionNeverSellsTheLongLegOfASpread(t *testing.T) {
	dl, ds, spot := 0.6, 0.4, 100.0
	opts := []rpc.PositionView{
		{Symbol: "SYNTHB", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 10, Right: "C", Strike: 100, Expiry: "20270115", Delta: &dl, Underlying: &spot, Multiplier: 100, Mark: 5},
		{Symbol: "SYNTHB", SecType: "OPTION", ConID: 3, Currency: "USD", Quantity: -10, Right: "C", Strike: 110, Expiry: "20270115", Delta: &ds, Underlying: &spot, Multiplier: 100, Mark: 2},
		{Symbol: "SYNTHC", SecType: "OPTION", ConID: 4, Currency: "USD", Quantity: 10, Right: "C", Strike: 100, Expiry: "20270115", Delta: &dl, Underlying: &spot, Multiplier: 100, Mark: 5},
	}
	pos := reduceTestPortfolio([]rpc.PositionView{{Symbol: "OTHER", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 50}}, opts)
	pos.Strategies, pos.StrategyIssues = strategy.InferStrategies(pos.Stocks, pos.Options)
	check := func(mode string, cands []reduceSweepCandidate) {
		t.Helper()
		leg, ok := reduceFindCandidate(cands, 2)
		if !ok || leg.qty != 0 || !hasBlocker(leg.blockers, strategyWorkflowRequiredCode) {
			t.Errorf("%s: spread's long call = %+v (found %v), want held with %s", mode, leg, ok, strategyWorkflowRequiredCode)
		}
		if alone, ok := reduceFindCandidate(cands, 4); !ok || alone.qty < 1 || len(alone.blockers) > 0 {
			t.Errorf("%s: a long call with nothing sold against it = %+v (found %v), want a sale", mode, alone, ok)
		}
	}
	net, _, _, _, _ := reduceSweepCandidates(pos, pos, 50)
	check("net", net)
	for _, mode := range []string{rpc.ReduceModeLongs, rpc.ReduceModeAll} {
		cands, _ := reduceScopedCandidates(pos, rpc.TradeProposalReducePortfolioParams{Mode: mode, Percent: 50}, reduceScopeFacts{})
		check(mode, cands)
	}
	if b := soldAgainstBlocker(pos, opts[0]); b == nil || b.Code != strategyWorkflowRequiredCode {
		t.Errorf("single reduction of the spread's long call = %+v, want %s", b, strategyWorkflowRequiredCode)
	}
	if b := soldAgainstBlocker(pos, opts[2]); b != nil {
		t.Errorf("single reduction of a long call held alone was held: %+v", b)
	}
}

// Exercising a long put that caps a sold put, or that sells the shares a
// sold call is written against, leaves the sold option uncovered. The
// opportunity is listed blocked, with the strategy route.
func TestExerciseOfAStrategyLegIsHeld(t *testing.T) {
	now := time.Date(2026, 6, 12, 15, 0, 0, 0, time.UTC)
	bid, ask, obid, spot := 79.9, 80.0, 19.5, 80.0
	stock := rpc.PositionView{Symbol: "SYNTHB", SecType: "STK", ConID: 1, Currency: "USD", Quantity: 200, Mark: 80, Bid: &bid, Ask: &ask, PriceAt: now}
	put := rpc.PositionView{Symbol: "SYNTHB", SecType: "OPT", ConID: 2, Currency: "USD", Quantity: 2, Right: "P", Strike: 100, Expiry: "20260619", Multiplier: 100, OptionBid: &obid, Underlying: &spot, PriceAt: now}
	call := rpc.PositionView{Symbol: "SYNTHB", SecType: "OPT", ConID: 3, Currency: "USD", Quantity: -2, Right: "C", Strike: 110, Expiry: "20260619", Multiplier: 100, PriceAt: now}
	shortPut := call
	shortPut.Right, shortPut.Strike = "P", 90
	generate := func(options ...rpc.PositionView) []rpc.Opportunity {
		s, issues := strategy.InferStrategies([]rpc.PositionView{stock}, options)
		pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{stock}, Options: options, Strategies: s, StrategyIssues: issues}
		return (&opportunityEngine{}).generate(defaultOpportunityPolicy(), rpc.OpportunityPolicyStatus{}, pos, rpc.OpportunitySourceFingerprints{}, brokerStateScope{}, now)
	}
	alone := generate(put)
	if len(alone) != 1 || alone[0].State == rpc.OpportunityStateBlocked {
		t.Fatalf("fixture: the put held alone = %+v, want one open exercise", alone)
	}
	for name, other := range map[string]rpc.PositionView{"collar": call, "put spread": shortPut} {
		opps := generate(put, other)
		if len(opps) != 1 || opps[0].State != rpc.OpportunityStateBlocked || !hasBlocker(opps[0].Blockers, strategyWorkflowRequiredCode) {
			t.Errorf("%s: opportunities = %+v, want the exercise listed blocked with %s", name, opps, strategyWorkflowRequiredCode)
		}
	}
}
