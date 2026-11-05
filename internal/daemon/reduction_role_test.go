package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func reductionRoleBook(stockQuantity float64) *rpc.PositionsResult {
	row := budgetOptionLeg("SPY", 990001, "P", 40, 500, 0)
	row.Currency, row.Delta, row.Underlying, row.FXRate = "USD", new(-0.5), new(100.0), new(1.0)
	pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"}, Options: []rpc.PositionView{row}, Stocks: []rpc.PositionView{{Symbol: "SYNTH", ConID: 990002, SecType: "STK", Currency: "USD", Quantity: stockQuantity, Mark: 100, MarketValue: stockQuantity * 100, MarketValueBase: new(stockQuantity * 100)}}}
	pos.ByUnderlying = groupByUnderlying(pos.Stocks, pos.Options, "USD", nil)
	return pos
}

func TestReductionUsesRulebookIndexPutClassification(t *testing.T) {
	pol := risk.DefaultRulebookPolicy()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	policy.Buckets.TrailingStop.Options.DefaultIndexPutsProtection = true
	engine := &proposalEngine{}
	for _, closed := range []bool{false, true} {
		now := optionExitTestTime()
		if closed {
			now = now.Add(-12 * time.Hour)
		}
		for _, protective := range []bool{false, true} {
			stockQuantity := 100.0
			if protective {
				stockQuantity = 10000
			}
			pos := reductionRoleBook(stockQuantity)
			in := risk.RuleInputs{BaseCurrency: "USD", NLVBase: new(100000.0), Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true}, Names: mapRuleNames(pos, pol, "USD")}
			evaluation := risk.EvaluateRulebook(in, pol)
			var premium risk.RuleRow
			for _, row := range evaluation.Rows {
				if row.ID == risk.RuleOptionLinePremium {
					premium = row
				}
			}
			input := budgetGovernorInput{Rulebook: pol, NLVBase: in.NLVBase, AccountBaseCurrency: "USD"}
			rows, _ := engine.budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, now)
			if !protective && (premium.Status != risk.RuleStatusAct || len(rows) == 0) {
				t.Errorf("closed=%t: directional size breach %s has %d reduction proposals", closed, premium.Status, len(rows))
			}
			if protective && len(rows) != 0 {
				t.Errorf("closed=%t: actual protection offered for reduction", closed)
			}
			candidateInput := mandateReductionInput{Input: &mandate.Input{Legs: []mandate.Leg{{ID: mandateConLegID(990001), Underlying: "SPY", Kind: mandate.KindOption, Quantity: 40, Multiplier: 100, Right: "P"}}}}
			candidates, _, _ := engine.mandateCandidates(policy, candidateInput, pos, now)
			if len(candidates) != 0 {
				t.Errorf("closed=%t protective=%t: mandate may sell the index put: %d candidates", closed, protective, len(candidates))
			}
		}
	}
}

func TestReductionIndexPutRequiresFreshEconomicsBeforeAdmission(t *testing.T) {
	for _, bucket := range []string{rpc.TradeProposalBucketBudgetReduction, rpc.TradeProposalBucketMandateReduction} {
		p := rpc.TradeProposal{Symbol: "SPY", Bucket: bucket, Contract: rpc.ContractParams{Right: "P"}}
		for _, bind := range []bool{false, true} {
			if blockers := (&proposalEngine{}).checkOptionExitEconomics(t.Context(), p, nil, bind); len(blockers) == 0 {
				t.Errorf("%s bind=%t admitted without fresh complete-book evidence", bucket, bind)
			}
		}
	}
}

func TestReductionUnknownIndexPutIsHeldWithoutCallingItProtection(t *testing.T) {
	pos := reductionRoleBook(100)
	pos.Options[0].Delta = nil
	pos.ByUnderlying = groupByUnderlying(pos.Stocks, pos.Options, "USD", nil)
	pol := risk.DefaultRulebookPolicy()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	policy.Buckets.BudgetReduction.Basis = rpc.BudgetBasisRulebook
	input := budgetGovernorInput{Rulebook: pol, NLVBase: new(100000.0), AccountBaseCurrency: "USD"}
	rows, st := (&proposalEngine{}).budgetReductionProposals(policy, rpc.ProtectionPolicyStatus{}, input, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, brokerStateScope{}, optionExitTestTime())
	if len(rows) != 0 || st.ProtectionLegs != 0 || st.ExcludedLegs != 1 {
		t.Fatalf("unknown role lost: rows=%d status=%+v", len(rows), st)
	}
}

// The 2026-11-04 book: a large long position whose options carry a delta but
// no underlying price drops out of the long book, and the index puts then read
// as over-hedged. A book Canary cannot measure says nothing about the hedge:
// neither reduction may treat the puts as directional on it.
func TestReductionIncompleteLongBookLeavesTheIndexPutsAlone(t *testing.T) {
	put := budgetOptionLeg("SPY", 990001, "P", 710, 500, 0)
	put.Currency, put.Delta, put.Underlying, put.FXRate, put.Quantity, put.Multiplier = "USD", new(-0.3), new(768.0), new(1.0), 10, 100
	long := budgetOptionLeg("SYNB", 990002, "C", 9, 30000, 0)
	long.Currency, long.Delta, long.FXRate, long.Quantity, long.Multiplier = "USD", new(0.5), new(1.0), 375, 100
	pos := &rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"}, Options: []rpc.PositionView{put, long}, Stocks: []rpc.PositionView{{Symbol: "SYNS", ConID: 990003, SecType: "STK", Quantity: 40, Mark: 250, Currency: "USD", FXRate: new(1.0), Delta: new(1.0)}}}
	pos.ByUnderlying = groupByUnderlying(pos.Stocks, pos.Options, "USD", nil)
	pol := risk.DefaultRulebookPolicy()
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	policy.Buckets.TrailingStop.Options.DefaultIndexPutsProtection = true
	now := optionExitTestTime()
	unitLegs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(policy.Buckets.TrailingStop.Options), now)
	if got := budgetLegRole(policy.Buckets.TrailingStop.Options, put, pos, unitLegs, ambiguous, pol, now); got != risk.IndexPutRoleUnclassified {
		t.Fatalf("role on an incomplete long book = %q, want unclassified", got)
	}
}

// A batch sized before that rule stands over a restart while no verdict can be
// trusted: holding it must not keep naming the put as a sale (2026-11-05, the
// first start on the new build still listed both puts of the 4 November batch).
func TestMandateHeldBatchDropsAHedgeListedPut(t *testing.T) {
	pos := reductionRoleBook(100)
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	put, stock := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketMandateReduction, Action: "SELL", Quantity: 5, PositionQuantity: 40, Mandate: &rpc.TradeProposalMandate{Rank: 1}}, rpc.TradeProposal{Bucket: rpc.TradeProposalBucketMandateReduction, Action: "SELL", Quantity: 10, PositionQuantity: 100, Mandate: &rpc.TradeProposalMandate{Rank: 2}}
	put.Contract.ConID, stock.Contract.ConID = 990001, 990002
	st := &rpc.TradeProposalMandateStatus{}
	rows, st := (&proposalEngine{}).mandateHeldRows(policy, rpc.ProtectionPolicyStatus{}, []rpc.TradeProposal{put, stock}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, optionExitTestTime(), st, "no verdict")
	if len(rows) != 1 || rows[0].Contract.ConID != 990002 || st.Rows != 1 || len(st.Plan) != 1 {
		t.Fatalf("held rows = %+v, status %+v, want only the stock row", rows, st)
	}
	if len(st.LeftOut) != 1 || !strings.Contains(st.LeftOut[0], "the reduction never sells it") {
		t.Fatalf("left out = %v, want the put named as protection", st.LeftOut)
	}
}

// The owner's rule (2026-11-04 21:04 CET): the forced reduction never sells a
// hedge-listed long put, measured as a hedge or not. The mandate flattens to
// its hedges. A budget row may still name one, and that row waits for a tap.
func TestMandateReductionNeverSellsAHedgeListedPut(t *testing.T) {
	pos := reductionRoleBook(100) // a small long book: the put measures directional
	policy := budgetTestPolicy(rpc.BudgetReductionModeActive, 0, 0)
	policy.Buckets.TrailingStop.Options.DefaultIndexPutsProtection = true
	in := mandateReductionInput{Input: &mandate.Input{Legs: []mandate.Leg{{ID: mandateConLegID(990001), Underlying: "SPY", Kind: mandate.KindOption, Quantity: 40, Multiplier: 100, Right: "P"}}}}
	candidates, leftOut, _ := (&proposalEngine{}).mandateCandidates(policy, in, pos, optionExitTestTime())
	if len(candidates) != 0 {
		t.Fatalf("the reduction may sell the index put: %+v", candidates)
	}
	if len(leftOut) != 1 || !strings.Contains(leftOut[0], "the reduction never sells it") {
		t.Fatalf("left out = %v, want the put named as protection", leftOut)
	}
}
