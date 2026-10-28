package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestNamedProposalPlanMarksUnavailableMeasurementUnpriced(t *testing.T) {
	for _, state := range []string{"missing", "unmeasurable", "unpriceable row scenario"} {
		t.Run(state, func(t *testing.T) {
			s := &Server{}
			if state != "missing" {
				s = indexLossServer(t, mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50)))
				if state == "unmeasurable" {
					result, input, at, fp := s.currentMandateVerdict()
					input.Reasons = []string{"synthetic missing measurement evidence"}
					v := mandate.Evaluate(*input)
					result.Verdict = &v
					s.mandateVerdicts.set(result, input, at, fp, false, s.captureMandateBookBinding())
				}
			}
			scope := brokerStateScope{Account: "synthetic", Mode: rpc.AccountModePaper}
			s.tradeProposals = &proposalEngine{server: s, scope: func() brokerStateScope { return scope }, snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{{Key: "rr-SPY", Revision: "r1", State: "generated", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: 40, Symbol: "SPY", SecType: "STK", OrderType: rpc.OrderTypeLMT, Contract: rpc.ContractParams{ConID: 9101, Symbol: "SPY", SecType: "STK", Currency: "EUR"}}}}}
			if state == "unpriceable row scenario" {
				row := &s.tradeProposals.snapshot.Proposals[0]
				row.Symbol, row.Contract.Symbol, row.Contract.ConID, row.LimitPrice = "SYNNEW", "SYNNEW", 9991, new(20.0)
			}
			plan := s.reduceProposalsPlan(t.Context(), rpc.TradeProposalReducePortfolioParams{PlanOnly: true, ProposalKeys: []string{"rr-SPY"}})
			if plan.IndexLossUnknown == "" || plan.IndexLossBeforeBase != nil || plan.IndexLossAfterBase != nil {
				t.Fatal("unavailable measurement must carry no scenario figures")
			}
			if plan.BlockedCount != 1 || len(plan.Legs) != 1 || len(plan.Legs[0].Blockers) != 1 || plan.Legs[0].Blockers[0].Code != "index_loss_unpriced" {
				t.Fatalf("unmeasured plan counts priced legs: blocked=%d message=%q legs=%+v", plan.BlockedCount, plan.Message, plan.Legs)
			}
		})
	}
}
