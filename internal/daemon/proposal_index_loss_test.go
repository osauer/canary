package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// indexLossServer is a daemon whose kept mandate evaluation measures pos
// under the current policy, so served proposals and plans read it.
func indexLossServer(t *testing.T, pos *rpc.PositionsResult) *Server {
	t.Helper()
	s := &Server{}
	keepMandateEvaluation(t, s, pos)
	return s
}

// keepMandateEvaluation keeps a mandate evaluation of pos on s under the
// current policy, the loss measure half the loss room, and returns its input.
func keepMandateEvaluation(t *testing.T, s *Server, pos *rpc.PositionsResult) *mandate.Input {
	t.Helper()
	s.now = func() time.Time { return mandateTestNow }
	if s.riskPolicies == nil {
		s.riskPolicies = newRiskPolicyManager("", time.Minute, nil)
	}
	s.riskPolicies.active = testMandateConstitution()
	fixture := mandateReductionFixture(t, pos, 0.5, 1000000, 0)
	s.mandateVerdicts.set(fixture.Result, fixture.Input, mandateTestNow, s.riskPolicies.snapshot().policy.FingerprintKey(), false, s.captureMandateBookBinding())
	return fixture.Input
}

// Canary's own proposals are priced for the index fall exactly as the
// reduce plan prices its legs (owner, 2026-10-28 06:41 CET): a sell of 40
// of 100 index shares reads the same before and after as the plan cutting those
// 40, a trailing stop reads unknown because it fills only when reached, a
// cash sweep row carries no figure, and the snapshot's figure fills every
// actionable row together.
func TestProposalIndexLossMatchesTheReducePlan(t *testing.T) {
	// The index line's beta is 1 by construction, so selling it must lessen
	// the fall's loss; the synthetic names' betas are whatever their series
	// give.
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50), mandateReductionStock("SYNB", 9102, 200, 30))
	s := indexLossServer(t, pos)

	plan := &rpc.TradeProposalReducePortfolioResult{}
	s.reduceMandateScores(t.Context(), plan, map[int]float64{9101: -40})
	if plan.IndexLossBeforeBase == nil || plan.IndexLossAfterBase == nil {
		t.Fatalf("precondition: the plan prices the cut: %+v", plan.IndexLoss)
	}
	both := &rpc.TradeProposalReducePortfolioResult{}
	s.reduceMandateScores(t.Context(), both, map[int]float64{9101: -40, 9102: -50})

	sell := func(key, symbol string, conID, qty int) rpc.TradeProposal {
		return rpc.TradeProposal{Key: key, Revision: "r1", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: qty,
			Symbol: symbol, SecType: "STK", OrderType: rpc.OrderTypeLMT, Contract: rpc.ContractParams{ConID: conID, Symbol: symbol, SecType: "STK", Currency: "EUR"}}
	}
	trail := sell("ts-SYNB", "SYNB", 9102, 200)
	trail.Bucket, trail.OrderType, trail.Trail = rpc.TradeProposalBucketTrailingStop, rpc.OrderTypeTRAIL, &rpc.OrderTrailSpec{OffsetType: "percent", TrailingPercent: new(8.0)}
	blocked := sell("rr-SYNB-blocked", "SYNB", 9102, 50)
	blocked.Blockers = []rpc.TradingBlocker{{Code: "synthetic_blocker"}}
	sweep := rpc.TradeProposal{Key: "cs-1", Revision: "r1", Bucket: rpc.TradeProposalBucketCashSweep, Action: rpc.OrderActionBuy, Quantity: 1, Symbol: "BILL"}
	snap := rpc.TradeProposalSnapshot{Revision: "snap-1", Proposals: []rpc.TradeProposal{sell("rr-SPY", "SPY", 9101, 40), trail, blocked, sweep}}
	e := &proposalEngine{server: s}
	e.decorateIndexLoss(&snap)

	near := func(a, b *float64) bool { return a != nil && b != nil && math.Abs(*a-*b) < 1e-6 }
	row := snap.Proposals[0].IndexLoss
	if row == nil || !near(row.IndexLossBeforeBase, plan.IndexLossBeforeBase) || !near(row.IndexLossAfterBase, plan.IndexLossAfterBase) || row.IndexShockPct != plan.IndexShockPct {
		t.Fatalf("proposal figure = %+v, plan figure = %+v; want the same", row, plan.IndexLoss)
	}
	if *row.IndexLossAfterBase >= *row.IndexLossBeforeBase {
		t.Fatalf("selling a long must lessen the fall's loss: %+v", row)
	}
	if got := snap.Proposals[1].IndexLoss; got == nil || got.IndexLossAfterBase != nil || !strings.Contains(got.IndexLossUnknown, "stop or trail") {
		t.Fatalf("trailing stop figure = %+v, want unknown and why", got)
	}
	if got := snap.Proposals[2].IndexLoss; got == nil || got.IndexLossAfterBase == nil {
		t.Fatalf("a blocked row is still priced on its own: %+v", got)
	}
	if snap.Proposals[3].IndexLoss != nil {
		t.Fatal("a cash sweep row carries no index-fall figure")
	}
	// Together: only the actionable sell; the blocked row and the stop stay out.
	if snap.IndexLoss == nil || !near(snap.IndexLoss.IndexLossAfterBase, plan.IndexLossAfterBase) || snap.BaseCurrency != "EUR" {
		t.Fatalf("snapshot figure = %+v %q, want the actionable rows filled together", snap.IndexLoss, snap.BaseCurrency)
	}
	snap.Proposals[2].Blockers = nil
	e.decorateIndexLoss(&snap)
	if !near(snap.IndexLoss.IndexLossAfterBase, both.IndexLossAfterBase) {
		t.Fatalf("snapshot figure after the blocker cleared = %+v, want both sells = %+v", snap.IndexLoss, both.IndexLoss)
	}

	// One JSON shape for the plan and the proposals.
	planJSON, _ := json.Marshal(plan.IndexLoss)
	rowJSON, _ := json.Marshal(row)
	var planKeys, rowKeys map[string]any
	_ = json.Unmarshal(planJSON, &planKeys)
	_ = json.Unmarshal(rowJSON, &rowKeys)
	for _, k := range []string{"index_shock_pct", "index_loss_before_base", "index_loss_after_base"} {
		if _, ok := planKeys[k]; !ok {
			t.Errorf("plan JSON lacks %s: %s", k, planJSON)
		}
		if _, ok := rowKeys[k]; !ok {
			t.Errorf("proposal JSON lacks %s: %s", k, rowJSON)
		}
	}
	var wire map[string]json.RawMessage
	raw, _ := json.Marshal(snap.Proposals[0])
	if err := json.Unmarshal(raw, &wire); err != nil || wire["index_loss"] == nil {
		t.Fatalf("proposal wire lacks index_loss: %s", raw)
	}
}

// Without a kept evaluation under the current policy every equity row and
// the snapshot say the figure is unknown and why; a read never evaluates.
func TestProposalIndexLossUnknownWithoutAnEvaluation(t *testing.T) {
	s := &Server{}
	snap := rpc.TradeProposalSnapshot{Proposals: []rpc.TradeProposal{{Key: "rr-1", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: 1, Symbol: "SYNA"}}}
	(&proposalEngine{server: s}).decorateIndexLoss(&snap)
	if snap.IndexLoss == nil || snap.IndexLoss.IndexLossUnknown == "" || snap.Proposals[0].IndexLoss == nil || snap.Proposals[0].IndexLoss.IndexLossBeforeBase != nil {
		t.Fatalf("snapshot = %+v row = %+v, want unknown with a reason", snap.IndexLoss, snap.Proposals[0].IndexLoss)
	}
}

// A plan of named proposals prices exactly the named rows (owner,
// 2026-10-28 06:41 CET): the figure equals the snapshot's for the same
// rows, a row it cannot price leaves the whole figure unknown, an unknown
// key is a blocked leg, and nothing from it can be submitted.
func TestReducePlanPricesNamedProposals(t *testing.T) {
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50), mandateReductionStock("SYNB", 9102, 200, 30))
	s := indexLossServer(t, pos)
	sell := func(key, symbol string, conID, qty int) rpc.TradeProposal {
		return rpc.TradeProposal{Key: key, Revision: "r1", State: "generated", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: qty,
			Symbol: symbol, SecType: "STK", OrderType: rpc.OrderTypeLMT, Contract: rpc.ContractParams{ConID: conID, Symbol: symbol, SecType: "STK", Currency: "EUR"}}
	}
	trail := sell("ts-SYNB", "SYNB", 9102, 200)
	trail.Bucket, trail.OrderType = rpc.TradeProposalBucketTrailingStop, rpc.OrderTypeTRAIL
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	s.tradeProposals = &proposalEngine{server: s, scope: func() brokerStateScope { return scope },
		snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "snap-1", AccountID: scope.Account, AccountMode: scope.Mode,
			Proposals: []rpc.TradeProposal{sell("rr-SPY", "SPY", 9101, 40), sell("rr-SYNB", "SYNB", 9102, 50), trail}}}

	plan, err := s.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{PlanOnly: true, ProposalKeys: []string{"rr-SPY", "rr-SYNB", "rr-SPY"}})
	if err != nil {
		t.Fatal(err)
	}
	served := s.tradeProposals.snapshotForRead(false, false)
	if plan.Mode != rpc.ReduceModeProposals || !plan.PlanOnly || plan.LegCount != 2 || plan.BlockedCount != 0 || plan.BaseCurrency != "EUR" {
		t.Fatalf("plan = %+v, want two priced legs in EUR", plan)
	}
	cuts := &rpc.TradeProposalReducePortfolioResult{}
	s.reduceMandateScores(t.Context(), cuts, map[int]float64{9101: -40, 9102: -50})
	near := func(a, b *float64) bool { return a != nil && b != nil && math.Abs(*a-*b) < 1e-6 }
	if !near(plan.IndexLossAfterBase, cuts.IndexLossAfterBase) || !near(plan.IndexLossBeforeBase, cuts.IndexLossBeforeBase) || !near(plan.IndexLossAfterBase, served.IndexLoss.IndexLossAfterBase) {
		t.Fatalf("named plan %+v, sweep plan %+v, snapshot %+v; want one figure", plan.IndexLoss, cuts.IndexLoss, served.IndexLoss)
	}
	if plan.Legs[0].ConID != 9101 || plan.Legs[0].Action != rpc.OrderActionSell || plan.Legs[0].ReduceQuantity != 40 {
		t.Fatalf("leg = %+v, want the row's own size", plan.Legs[0])
	}

	withStop, _ := s.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{PlanOnly: true, ProposalKeys: []string{"rr-SPY", "ts-SYNB", "gone"}})
	if withStop.IndexLossAfterBase != nil || !strings.Contains(withStop.IndexLossUnknown, "2 of 3") || withStop.BlockedCount != 2 {
		t.Fatalf("plan with a stop and an unknown key = %+v, want the figure unknown and two blocked legs", withStop)
	}
	if code := withStop.Legs[2].Blockers[0].Code; code != "proposal_unknown" {
		t.Fatalf("unknown key blocker = %s", code)
	}
	refused, _ := s.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{ProposalKeys: []string{"rr-SPY"}})
	if len(refused.Blockers) == 0 || refused.IndexLossAfterBase != nil {
		t.Fatalf("named proposals without plan_only = %+v, want refused", refused)
	}
}

// scoreBook is a long index book with two-sided quotes: shares under a
// stop and bought calls. The index's beta is 1 by construction, so the
// book loses in the fall and gains in the rise.
func scoreBook() []rpc.PositionView {
	spy := mandateReductionStock("SPY", 9101, 100, 50)
	call := mandateReductionCall("SPY", 9103, 10, 50, 50, 2.5, "20261218")
	call.Delta = new(0.5)
	spy.Bid, spy.Ask = new(49.9), new(50.1)
	call.Bid, call.Ask = new(2.4), new(2.6)
	return []rpc.PositionView{spy, call}
}

// The reduce plan is scored on both sides against the loss limit (owner,
// 2026-10-28 13:13 CET), in every sweep mode: the loss measure now and after
// against the room (each cut line gives up its own premium or distance to
// the stop), the index rise beside the fall (a long book gains less in the
// rise and loses less in the fall once cut), and the trading cost, half the
// spread times units times multiplier. A leg without a two-sided quote
// makes the cost unknown, never partial.
func TestReducePlanScoresBothSidesAgainstTheLossLimit(t *testing.T) {
	srv, pos := newReducePreviewTestServer(t, scoreBook()...)
	pos.Portfolio.BaseCurrency = "EUR"
	in := keepMandateEvaluation(t, srv, pos)
	perUnit, quote := map[int]float64{}, map[int]float64{}
	for _, l := range in.Legs {
		var conID int
		if _, err := fmt.Sscanf(l.ID, "con:%d", &conID); err == nil {
			perUnit[conID] = l.LossPerUnitBase
		}
	}
	for _, row := range slices.Concat(pos.Stocks, pos.Options) {
		quote[row.ConID] = (*row.Ask - *row.Bid) / 2 * float64(max(row.Multiplier, 1))
	}
	nowMeasure, _ := mandate.MeasureLoss(*in)
	nowStress := mandate.Evaluate(*in).Stresses
	room := mandate.LossRoomBase(in.Policy, in.Capital)
	near := func(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-6 }

	for _, mode := range []string{rpc.ReduceModeNet, rpc.ReduceModeLongs, rpc.ReduceModeAll} {
		plan, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: mode, Percent: 50, PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		cut, cost, planned := 0.0, 0.0, 0
		for _, leg := range plan.Legs {
			if len(leg.Blockers) > 0 {
				continue
			}
			planned++
			cut += float64(leg.ReduceQuantity) * perUnit[leg.ConID]
			cost += float64(leg.ReduceQuantity) * quote[leg.ConID]
		}
		if planned < 2 {
			t.Fatalf("%s: precondition: the plan cuts lines: %+v", mode, plan.Legs)
		}
		if !near(plan.LossMeasureBeforeBase, nowMeasure.TotalBase) || !near(plan.LossMeasureAfterBase, nowMeasure.TotalBase-cut) || !near(plan.LossRoomBase, room) || plan.LossMeasureUnknown != "" {
			t.Fatalf("%s: loss measure %+v, want %.2f now, %.2f after, room %.2f", mode, plan.PlanLossMeasure, nowMeasure.TotalBase, nowMeasure.TotalBase-cut, room)
		}
		if plan.IndexRisePct != mandate.IndexShockPct || !near(plan.IndexRiseLossBeforeBase, nowStress.IndexRiseLossBase) || plan.IndexRiseLossAfterBase == nil ||
			*plan.IndexRiseLossBeforeBase >= 0 || *plan.IndexRiseLossAfterBase <= *plan.IndexRiseLossBeforeBase || *plan.IndexRiseLossAfterBase >= 0 ||
			*plan.IndexLossBeforeBase <= 0 || *plan.IndexLossAfterBase >= *plan.IndexLossBeforeBase {
			t.Fatalf("%s: index fall %s and rise %s, want a long book that gains less in the rise and loses less in the fall once cut", mode,
				beforeAfter(plan.IndexLossBeforeBase, plan.IndexLossAfterBase), beforeAfter(plan.IndexRiseLossBeforeBase, plan.IndexRiseLossAfterBase))
		}
		if !near(plan.TradeCostBase, cost) || plan.TradeCostUnknown != "" {
			t.Fatalf("%s: trade cost %v %q, want %.2f", mode, plan.TradeCostBase, plan.TradeCostUnknown, cost)
		}
	}

	pos.Stocks[0].Ask = nil
	plan, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50, PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TradeCostBase != nil || !strings.Contains(plan.TradeCostUnknown, "SPY") || plan.LossMeasureAfterBase == nil {
		t.Fatalf("one-sided quote: cost %v %q, measure %+v; want the cost unknown naming the line and the scores kept", plan.TradeCostBase, plan.TradeCostUnknown, plan.PlanLossMeasure)
	}

	// The wire names, beside the index fall's.
	raw, _ := json.Marshal(plan)
	for _, k := range []string{"loss_measure_before_base", "loss_measure_after_base", "loss_room_base", "index_rise_pct", "index_rise_loss_before_base", "index_rise_loss_after_base", "trade_cost_unknown"} {
		if !strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("plan JSON lacks %s", k)
		}
	}
}

// A plan of named proposals scores as the sweep cutting the same lines: a
// proposal selling held units takes them off the line at the line's own
// loss per unit, the rise and the loss measure match, and the cost reads
// the held lines' quotes. A row it cannot price leaves every figure unknown.
func TestReducePlanOfProposalsScoresAsTheSweep(t *testing.T) {
	rows := scoreBook()
	pos := mandateReductionBook(rows...)
	s := indexLossServer(t, pos)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	sell := func(key string, row rpc.PositionView, qty int) rpc.TradeProposal {
		return rpc.TradeProposal{Key: key, Revision: "r1", State: "generated", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: qty,
			Symbol: row.Symbol, SecType: positionWireSecType(row.SecType), OrderType: rpc.OrderTypeLMT,
			Contract: rpc.ContractParams{ConID: row.ConID, Symbol: row.Symbol, SecType: positionWireSecType(row.SecType), Currency: "EUR"}}
	}
	trail := sell("ts-SPY", rows[0], 100)
	trail.Bucket, trail.OrderType = rpc.TradeProposalBucketTrailingStop, rpc.OrderTypeTRAIL
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	s.tradeProposals = &proposalEngine{server: s, scope: func() brokerStateScope { return scope },
		snapshot: rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, Revision: "snap-1", AccountID: scope.Account, AccountMode: scope.Mode,
			Proposals: []rpc.TradeProposal{sell("rr-SPY", rows[0], 40), sell("ox-SPY", rows[1], 4), trail}}}

	plan, err := s.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{PlanOnly: true, ProposalKeys: []string{"rr-SPY", "ox-SPY"}})
	if err != nil {
		t.Fatal(err)
	}
	sweep := &rpc.TradeProposalReducePortfolioResult{}
	s.reduceMandateScores(t.Context(), sweep, map[int]float64{9101: -40, 9103: -4})
	near := func(a, b *float64) bool { return a != nil && b != nil && math.Abs(*a-*b) < 1e-6 }
	if !near(plan.LossMeasureAfterBase, sweep.LossMeasureAfterBase) || !near(plan.LossMeasureBeforeBase, sweep.LossMeasureBeforeBase) || !near(plan.LossRoomBase, sweep.LossRoomBase) {
		t.Fatalf("named plan measure %s, sweep %s; want one figure", beforeAfter(plan.LossMeasureBeforeBase, plan.LossMeasureAfterBase), beforeAfter(sweep.LossMeasureBeforeBase, sweep.LossMeasureAfterBase))
	}
	if *plan.LossMeasureAfterBase >= *plan.LossMeasureBeforeBase {
		t.Fatalf("selling stopped shares and bought calls must lessen the loss measure: %+v", plan.PlanLossMeasure)
	}
	if !near(plan.IndexRiseLossAfterBase, sweep.IndexRiseLossAfterBase) || !near(plan.IndexRiseLossBeforeBase, sweep.IndexRiseLossBeforeBase) {
		t.Fatalf("named plan rise %s, sweep %s; want one figure", beforeAfter(plan.IndexRiseLossBeforeBase, plan.IndexRiseLossAfterBase), beforeAfter(sweep.IndexRiseLossBeforeBase, sweep.IndexRiseLossAfterBase))
	}
	if want := 40*0.1 + 4*0.1*100; !near(plan.TradeCostBase, new(want)) {
		t.Fatalf("trade cost %v %q, want %.2f", plan.TradeCostBase, plan.TradeCostUnknown, want)
	}

	withStop, _ := s.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{PlanOnly: true, ProposalKeys: []string{"ox-SPY", "ts-SPY"}})
	if withStop.LossMeasureAfterBase != nil || withStop.IndexRiseLossAfterBase != nil || withStop.TradeCostBase != nil ||
		withStop.LossMeasureUnknown == "" || withStop.IndexRiseLossUnknown == "" || withStop.TradeCostUnknown == "" {
		t.Fatalf("plan with a stop = %+v, want every figure unknown and why", withStop)
	}
}

func beforeAfter(before, after *float64) string {
	if before == nil || after == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.2f now, %.2f after", *before, *after)
}

// The broker's portfolio stream carries a mark for an option and never a
// bid or ask (live book 2026-10-28 17:19 CET: none of eight option rows had
// one), so a cost read off the rows alone was unknown on every plan that
// touched an option. A leg without a two-sided row quote is quoted by the
// exact contract, as the hedge's future is; the row's own quote is kept
// when it has one, and a quote that does not arrive leaves the cost
// unknown, naming the line.
func TestReducePlanQuotesAnOptionLegWithoutARowQuote(t *testing.T) {
	book := scoreBook()
	book[1].Bid, book[1].Ask = nil, nil
	srv, pos := newReducePreviewTestServer(t, book...)
	pos.Portfolio.BaseCurrency = "EUR"
	keepMandateEvaluation(t, srv, pos)
	var asked []rpc.ContractParams
	srv.reduceLegQuoteForTest = func(_ context.Context, c rpc.ContractParams) (*rpc.Quote, error) {
		asked = append(asked, c)
		return &rpc.Quote{Bid: new(2.0), Ask: new(3.0)}, nil
	}
	plan, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeAll, Percent: 100, PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || asked[0].ConID != 9103 || asked[0].SecType != "OPT" || asked[0].Right == "" {
		t.Fatalf("exact quotes asked = %+v, want the one option leg by contract", asked)
	}
	want := 100*0.1 + 10*0.5*100
	if plan.TradeCostBase == nil || math.Abs(*plan.TradeCostBase-want) > 1e-6 || plan.TradeCostUnknown != "" {
		t.Fatalf("cost %v %q, want %.2f from the row quote and the exact option quote", plan.TradeCostBase, plan.TradeCostUnknown, want)
	}

	srv.reduceLegQuoteForTest = func(context.Context, rpc.ContractParams) (*rpc.Quote, error) {
		return nil, errors.New("no quote within 2s")
	}
	plan, err = srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeAll, Percent: 100, PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TradeCostBase != nil || !strings.Contains(plan.TradeCostUnknown, "SPY") || plan.LossMeasureAfterBase == nil {
		t.Fatalf("no quote: cost %v %q, measure %+v; want unknown naming the line and the scores kept", plan.TradeCostBase, plan.TradeCostUnknown, plan.PlanLossMeasure)
	}
}
