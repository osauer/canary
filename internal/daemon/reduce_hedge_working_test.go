package daemon

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/ibkr"
)

// The hedge plan counts working hedge sales as sold, as placement does:
// 3,000 index shares lose 15,000 in the fall and one contract offsets 2,700,
// so with two contracts already working three more reach neutral, not five.
// Sized on the held book alone, the plan offered a sale placement refuses
// past neutral (review of 2026-10-29).
func TestReduceHedgePlanCountsWorkingHedgeSales(t *testing.T) {
	working := func(s *Server, qty float64) {
		s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
			scope := s.currentBrokerStateScope()
			return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 77, ClientIDPresent: true,
				Account: scope.Account, Symbol: "MES", SecType: "FUT", ConID: hedgeTestConID, Currency: "USD", Multiplier: 5, Action: "SELL", OrderType: "LMT", LimitPrice: 6000,
				TotalQuantity: qty, Remaining: qty, Status: "Submitted"}}}, scope, nil
		}
	}
	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-6 }

	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	working(s, 2)
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || res.Legs[0].ReduceQuantity != 3 {
		t.Fatalf("plan = %+v, want SELL 3 beside 2 working", res)
	}
	if !near(res.IndexLossBeforeBase, 15000-2*2700) || !near(res.IndexLossAfterBase, 1500) || !strings.Contains(res.Message, "2 already working") {
		t.Fatalf("fall %s, message %q; want 9,600 → 1,500 with the working sales named", beforeAfter(res.IndexLossBeforeBase, res.IndexLossAfterBase), res.Message)
	}

	s = hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	working(s, 6)
	res = hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 0 || len(res.Blockers) != 1 || res.Blockers[0].Code != "nothing_to_hedge" || !strings.Contains(res.Blockers[0].Message, "6 working hedge contracts") {
		t.Fatalf("plan = %+v, want nothing to hedge with six working", res)
	}
}
