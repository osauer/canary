package daemon

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// hedgeServer keeps a mandate evaluation of rows (a EUR book) with a USD
// rate and history beside it, so a USD future can be priced, and reads a
// front Micro S&P future at 6,000 with a half-point spread.
func hedgeServer(t *testing.T, rows ...rpc.PositionView) *Server {
	t.Helper()
	return hedgeServerOn(t, &Server{}, rows...)
}

// hedgeServerOn is hedgeServer on a server the caller built.
func hedgeServerOn(t *testing.T, s *Server, rows ...rpc.PositionView) *Server {
	t.Helper()
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
// is scored as a sweep is: the rise gains less by as much, the hedge's loss
// in the rise (5 × 10% × 6,000 × 5 × 0.9 = 13,500) spends loss room (owner
// decision 2026-10-28 20:07 CET), and the cost is half the spread.
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
	if res.LossMeasureBeforeBase == nil || !near(res.LossMeasureAfterBase, *res.LossMeasureBeforeBase+13500) || res.LossRoomBase == nil {
		t.Fatalf("loss measure %s, want the hedge's 13,500 rise loss added", beforeAfter(res.LossMeasureBeforeBase, res.LossMeasureAfterBase))
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

// A hedge is sized to neutral on the whole book; without the owner's
// [mandate.index_hedge] table a preview prices it and refuses to place it,
// and a future it cannot read leaves it unsized with the reason.
func TestReduceHedgePlanRefusals(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50))
	if res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{}); len(res.Legs) != 1 || len(res.Legs[0].Blockers) == 0 || res.Legs[0].Blockers[0].Code != "index_hedge_unplaceable" ||
		!strings.Contains(res.Legs[0].Blockers[0].Message, "[mandate.index_hedge]") || res.Legs[0].PreviewToken != "" || res.Accepted || res.IndexLossAfterBase == nil {
		t.Fatalf("preview without the table = %+v, want the priced leg refused naming the table", res)
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

// On the held book the plan never offers a sale its placement refuses: a
// sale that leaves a gap cap breached or the loss measure over the loss
// room is refused there
// (indexHedgeProjectedRiskRefusal). Each contract's 2,700 rise loss spends
// room, so with room for three and a half more the plan sells three of the
// five to neutral and says why. With SPY's gap cap already breached the
// hedge nets against SPY in the index gap (owner decision "1. A",
// 2026-10-30 09:41 CET), so the plan sells all five to neutral and cures
// the breach.
func TestReduceHedgePlanStaysWithinWhatPlacementAdmits(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50))
	in, _ := s.reduceMandateInput(t.Context())
	in.Capital.Room = (150000 + 3.5*2700) / 300000
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || res.Legs[0].ReduceQuantity != 3 ||
		!strings.Contains(res.Message, "5 would reach neutral") || !strings.Contains(res.Message, "above the loss room") {
		t.Fatalf("plan = %d legs %+v, message %q; want SELL 3 held below neutral by the loss room", len(res.Legs), res.Blockers, res.Message)
	}
	if hedgeSaleRefusal(*in, mandate.Leg{ID: mandateConLegID(750001), Symbol: "MESZ6", Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock, Multiplier: 5, Currency: "USD", Spot: 6000}, 3) != "" {
		t.Fatal("the planned sale would be refused at placement")
	}

	spy := mandateReductionStock("SPY", 9101, 3000, 50)
	spy.Currency, spy.FXRate = "USD", new(0.9) // SPY is a broad index in USD only
	s = hedgeServer(t, spy)
	in, _ = s.reduceMandateInput(t.Context())
	in.Capital.Room = 1
	in.Policy.GapRoomPct = 10
	if before := mandate.Evaluate(*in); !slices.Equal(before.Stresses.GapBreaches, []string{"SPY"}) {
		t.Fatalf("fixture: breaches %v, want SPY's", before.Stresses.GapBreaches)
	}
	res = hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || res.Legs[0].ReduceQuantity != 5 {
		t.Fatalf("plan = %d legs %+v, message %q; want SELL 5 to neutral curing SPY's breach", len(res.Legs), res.Blockers, res.Message)
	}
}
