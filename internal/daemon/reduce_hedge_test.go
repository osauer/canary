package daemon

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// hedgeServer keeps a mandate evaluation of rows (a EUR book) with a USD
// rate and history beside it, so a USD future can be priced, and reads a
// front Micro S&P future at 6,000 with a half-point spread.
func hedgeServer(t *testing.T, rows ...rpc.PositionView) *Server {
	t.Helper()
	s := &Server{}
	in := keepMandateEvaluation(t, s, mandateReductionBook(rows...))
	in.FX["USD"] = mandateSeriesInput(mandateTestSeries("fx:USD", 0.08, 9, 0), mandateTestNow)
	if in.FXNow == nil {
		in.FXNow = map[string]float64{}
	}
	in.FXNow["USD"] = 0.9
	s.reduceHedgeFutureForTest = func(context.Context) (rpc.ContractParams, *rpc.Quote, error) {
		return rpc.ContractParams{ConID: 750001, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", TradingClass: "MES", Multiplier: 5},
			&rpc.Quote{Bid: new(5999.75), Ask: new(6000.25), Mark: new(6000.0)}, nil
	}
	return s
}

func hedgePlan(t *testing.T, s *Server, p rpc.TradeProposalReducePortfolioParams) *rpc.TradeProposalReducePortfolioResult {
	t.Helper()
	p.Mode = rpc.ReduceModeHedge
	res, err := s.reducePortfolioPreview(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The index hedge sells whole Micro S&P contracts until a 10% fall leaves
// the book about even, rounded toward less hedge (owner decisions
// 2026-10-28 13:17 CET): 3,000 index shares lose 15,000 in the fall, one
// contract at 6,000 × 5 × 0.9 offsets 2,700, so five leave 1,500. The plan
// is scored as a sweep is: the rise gains less by as much, the unstopped
// future adds nothing to the loss measure, and the cost is half the spread.
func TestReduceHedgePlanSellsToNeutral(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50))
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || res.Mode != rpc.ReduceModeHedge || !res.PlanOnly || len(res.Legs) != 1 {
		t.Fatalf("plan = %+v, want one planned leg", res)
	}
	leg := res.Legs[0]
	if leg.Action != rpc.OrderActionSell || leg.ReduceQuantity != 5 || leg.ConID != 750001 || leg.SecType != "FUT" || leg.Contract == nil || leg.Contract.LocalSymbol != "MESZ6" {
		t.Fatalf("leg = %+v, want SELL 5 MESZ6", leg)
	}
	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-6 }
	if !near(res.IndexLossBeforeBase, 15000) || !near(res.IndexLossAfterBase, 1500) || !near(res.IndexRiseLossBeforeBase, -15000) || !near(res.IndexRiseLossAfterBase, -1500) {
		t.Fatalf("fall %s, rise %s; want 15,000 → 1,500 and -15,000 → -1,500",
			beforeAfter(res.IndexLossBeforeBase, res.IndexLossAfterBase), beforeAfter(res.IndexRiseLossBeforeBase, res.IndexRiseLossAfterBase))
	}
	if res.LossMeasureBeforeBase == nil || !near(res.LossMeasureAfterBase, *res.LossMeasureBeforeBase) || res.LossRoomBase == nil {
		t.Fatalf("loss measure %s, want unchanged by an unstopped hedge", beforeAfter(res.LossMeasureBeforeBase, res.LossMeasureAfterBase))
	}
	if !near(res.TradeCostBase, 0.25*5*5*0.9) || !near(leg.NotionalBase, 5*6000*5*0.9) || res.BaseCurrency != "EUR" {
		t.Fatalf("cost %v notional %v ccy %s", res.TradeCostBase, leg.NotionalBase, res.BaseCurrency)
	}
}

// Nothing is sold when the fall already leaves the book even or ahead, or
// when the loss is under one contract's offset; the plan says why and
// shows the book's figures unchanged.
func TestReduceHedgePlanSellsNothingPastNeutral(t *testing.T) {
	put := mandateReductionCall("SPY", 9103, 10, 50, 50, 2.5, "20261218")
	put.Right = "P"
	for _, tc := range []struct {
		name string
		rows []rpc.PositionView
		code string
	}{
		{"already ahead in the fall", []rpc.PositionView{mandateReductionStock("SPY", 9101, 100, 50), put}, "nothing_to_hedge"},
		{"under one contract", []rpc.PositionView{mandateReductionStock("SPY", 9101, 100, 50)}, "below_one_contract"},
	} {
		res := hedgePlan(t, hedgeServer(t, tc.rows...), rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
		if len(res.Legs) != 0 || len(res.Blockers) != 1 || res.Blockers[0].Code != tc.code {
			t.Fatalf("%s: plan = %+v, want no leg and %s", tc.name, res, tc.code)
		}
		if res.IndexLossBeforeBase == nil || res.IndexLossAfterBase == nil || *res.IndexLossAfterBase != *res.IndexLossBeforeBase || res.TradeCostBase == nil || *res.TradeCostBase != 0 {
			t.Fatalf("%s: figures %+v cost %v, want the book unchanged", tc.name, res.IndexLoss, res.TradeCostBase)
		}
	}
}

// A hedge is a plan only, sized to neutral on the whole book, and a future
// it cannot read leaves it unsized with the reason.
func TestReduceHedgePlanRefusals(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50))
	if res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{}); len(res.Blockers) == 0 || res.Blockers[0].Code != "hedge_plan_only" {
		t.Fatalf("preview = %+v, want plan only", res.Blockers)
	}
	if res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true, Percent: 50}); len(res.Blockers) == 0 || res.Blockers[0].Code != "bad_request" {
		t.Fatalf("percent = %+v, want refused", res.Blockers)
	}
	s.reduceHedgeFutureForTest = func(context.Context) (rpc.ContractParams, *rpc.Quote, error) {
		return rpc.ContractParams{}, nil, errors.New("front future contract unavailable")
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 0 || len(res.Blockers) == 0 || res.Blockers[0].Code != "hedge_contract_unavailable" || !strings.Contains(res.Blockers[0].Message, "front future") {
		t.Fatalf("no contract = %+v, want unsized and why", res.Blockers)
	}
}
