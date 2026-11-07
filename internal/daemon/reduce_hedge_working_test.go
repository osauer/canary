package daemon

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
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

// workingOrdersAt makes the broker's open orders these rows.
func workingOrdersAt(s *Server, rows ...ibkr.OrderLifecycleEvent) {
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		scope := s.currentBrokerStateScope()
		for i := range rows {
			rows[i].Account = scope.Account
		}
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: rows}, scope, nil
	}
}

// workingSale is a limit sale of a line working at the broker, untouched.
func workingSale(symbol, secType string, conID int, qty float64) ibkr.OrderLifecycleEvent {
	return ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 2000 + conID, ClientID: 77, ClientIDPresent: true, Symbol: symbol, SecType: secType, ConID: conID,
		Currency: "EUR", Action: "SELL", OrderType: "LMT", LimitPrice: 1, TotalQuantity: qty, Remaining: qty, Status: "Submitted"}
}

// hedgeTestCalls is a line of bought index calls with a long time to run, so
// they gain more in the rise than they lose in the fall.
func hedgeTestCalls(conID int, contracts float64) rpc.PositionView {
	return mandateReductionCall("SPY", conID, contracts, 50, 50, mandate.OptionPrice(true, 50, 50, 1.1, 0.4), "20271217")
}

// hedgeFallWith is the book's loss in the 10% fall with held units of the
// named lines sold, read the way the plan reads it.
func hedgeFallWith(t *testing.T, s *Server, sold map[int]float64) float64 {
	t.Helper()
	in, why := s.reduceMandateInput(t.Context())
	if in == nil {
		t.Fatal(why)
	}
	book := *in
	for conID, qty := range sold {
		for _, l := range in.Legs {
			if l.ID == mandateConLegID(conID) {
				l.Quantity = -qty
				book = mandateInputWithLegs(book, []mandate.Leg{l})
				break
			}
		}
	}
	v := mandate.Evaluate(book)
	if v.Stresses == nil {
		t.Fatalf("the fixture book is unmeasured: %v", v.Reasons)
	}
	return v.Stresses.IndexLossBase
}

// A cut still working when the hedge is sized (review of Desk 70929f97,
// 2026-11-06): 100 calls sized alone take several contracts, and with 75 of
// them sold those contracts stand past neutral until the buy-back of a breach.
// The plan sizes on the book as it reads once the working sale fills.
func TestReduceHedgePlanCountsAWorkingSaleOfCalls(t *testing.T) {
	const perContract = 2700.0
	s := hedgeOrderServer(t, 25, hedgeTestCalls(9201, 100))
	fallHeld, fallCut := hedgeFallWith(t, s, nil), hedgeFallWith(t, s, map[int]float64{9201: 75})
	neutralHeld, neutralCut := math.Floor(fallHeld/perContract), math.Floor(fallCut/perContract)
	if neutralCut < 1 || fallCut-neutralHeld*perContract >= 0 {
		t.Fatalf("fixture: the held size must pass neutral once the sale fills: fall %g then %g, neutral %g then %g", fallHeld, fallCut, neutralHeld, neutralCut)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 1 || float64(res.Legs[0].ReduceQuantity) != neutralHeld {
		t.Fatalf("control, no working sale: plan %+v, want SELL %g", res, neutralHeld)
	}

	workingOrdersAt(s, workingSale("SPY", "OPT", 9201, 75))
	res = hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || float64(res.Legs[0].ReduceQuantity) != neutralCut {
		t.Fatalf("plan = %+v, want SELL %g with 75 calls working, not the %g the held book takes", res, neutralCut, neutralHeld)
	}
	if res.IndexLossBeforeBase == nil || math.Abs(*res.IndexLossBeforeBase-fallCut) > 1e-6 || res.IndexLossAfterBase == nil || *res.IndexLossAfterBase < -1e-6 {
		t.Fatalf("fall %s; want from %g, not past neutral", beforeAfter(res.IndexLossBeforeBase, res.IndexLossAfterBase), fallCut)
	}
	if res.IndexRiseLossAfterBase == nil || *res.IndexRiseLossAfterBase > 1e-6 {
		t.Fatalf("the sale makes a 10%% rise lose %v once the calls have gone", res.IndexRiseLossAfterBase)
	}
	if !strings.Contains(res.Message, "working sales") {
		t.Fatalf("message %q does not say the working sale is counted", res.Message)
	}
}

// How each kind of working order on a line reads for the hedge's size.
func TestReduceHedgePlanReadsWorkingOrdersOnTheBook(t *testing.T) {
	const perContract = 2700.0
	shares := func() rpc.PositionView { return mandateReductionStock("SPY", 9101, 3000, 50) }
	put := hedgeTestCalls(9202, 10)
	put.Right = "P"
	short := mandateReductionCall("SPY", 9203, -10, 60, 50, 0.5, "20271217")
	sale := func(mutate func(*ibkr.OrderLifecycleEvent)) ibkr.OrderLifecycleEvent {
		o := workingSale("SPY", "OPT", 9201, 75)
		mutate(&o)
		return o
	}
	for _, tc := range []struct {
		name   string
		rows   []rpc.PositionView
		orders []ibkr.OrderLifecycleEvent
		sold   map[int]float64 // the book the size is measured on
		says   string
		unread bool
	}{
		// The fills are in the held quantity, so only what is left counts.
		{name: "a partly filled sale counts what remains", rows: []rpc.PositionView{hedgeTestCalls(9201, 60)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.TotalQuantity, o.Filled, o.Remaining = 100, 40, 45 })},
			sold:   map[int]float64{9201: 45}, says: "working sales on 1 line"},
		{name: "two sales of one line add up", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9201, 40), func() ibkr.OrderLifecycleEvent { o := workingSale("SPY", "OPT", 9201, 35); o.OrderID = 4000; return o }()},
			sold:   map[int]float64{9201: 75}, says: "working sales on 1 line"},
		// More than the line holds closes it and no further: the rest is no cut.
		{name: "a sale takes the line to flat at most", rows: []rpc.PositionView{hedgeTestCalls(9201, 100), shares()},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9201, 500)}, sold: map[int]float64{9201: 100}, says: "working sales on 1 line"},
		{name: "two lines", rows: []rpc.PositionView{hedgeTestCalls(9201, 100), shares()},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9201, 75), workingSale("SPY", "STK", 9101, 1000)},
			sold:   map[int]float64{9201: 75, 9101: 1000}, says: "working sales on 2 lines"},
		// A stop sells only when the market reaches it, and every protected line
		// carries one: counted as sold, it would leave such a book unhedged.
		{name: "a stop is not a sale yet", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.OrderType = "STP" })}},
		{name: "a stop limit is not a sale yet", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.OrderType = "STP LMT" })}},
		{name: "a trail is not a sale yet", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.OrderType = "TRAIL" })}},
		{name: "a buy is no cut", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.Action = "BUY" })}},
		// A bought put pays in the fall. Sized as if it had gone, the hedge would
		// stand past neutral for as long as it stays.
		{name: "selling a bought put does not raise the size", rows: []rpc.PositionView{put, shares()},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9202, 10)}},
		{name: "a sale adding to a short line is no cut", rows: []rpc.PositionView{short, shares()},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9203, 10)}},
		{name: "a sale of a line not held is no cut", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{workingSale("SPY", "OPT", 9299, 75)}},
		{name: "an unreadable sale of another line does not matter", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 3, Symbol: "SPY", SecType: "OPT", ConID: 9299, Action: "SELL", OrderType: "LMT", Status: "Submitted"}}},
		{name: "a sale naming no contract, of another name, does not matter", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 3, Symbol: "ZZZ", SecType: "STK", Action: "SELL", OrderType: "LMT", TotalQuantity: 5, Remaining: 5, Status: "Submitted"}}},
		// Unread: the plan leaves it out and says so, and the preview refuses.
		{name: "an unknown remaining quantity is unread", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.TotalQuantity, o.Remaining = 0, 0 })}, says: "no remaining quantity", unread: true},
		{name: "an unknown direction is unread", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.Action = "" })}, says: "no direction", unread: true},
		{name: "an unknown order type is unread", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.OrderType = "" })}, says: "no order type", unread: true},
		{name: "a sale of the same name naming no contract is unread", rows: []rpc.PositionView{hedgeTestCalls(9201, 100)},
			orders: []ibkr.OrderLifecycleEvent{sale(func(o *ibkr.OrderLifecycleEvent) { o.ConID = 0 })}, says: "does not name its contract", unread: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := hedgeOrderServer(t, 25, tc.rows...)
			want := math.Floor(hedgeFallWith(t, s, tc.sold) / perContract)
			if want < 1 {
				t.Fatalf("fixture: the book must take a hedge, fall %g", hedgeFallWith(t, s, tc.sold))
			}
			workingOrdersAt(s, tc.orders...)
			res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
			if len(res.Legs) != 1 || float64(res.Legs[0].ReduceQuantity) != want {
				t.Fatalf("plan = %+v (message %q), want SELL %g", res.Legs, res.Message, want)
			}
			if !strings.Contains(res.Message, tc.says) {
				t.Fatalf("message %q does not say %q", res.Message, tc.says)
			}
			if tc.says == "" && strings.Contains(res.Message, "working sales") {
				t.Fatalf("message %q counts a sale the size left out", res.Message)
			}
			preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
			if len(preview.Legs) != 1 {
				t.Fatalf("preview = %+v", preview)
			}
			blocked := slices.ContainsFunc(preview.Legs[0].Blockers, func(b rpc.TradingBlocker) bool { return b.Code == "index_hedge_unmeasured" })
			if blocked != tc.unread || (tc.unread && preview.Legs[0].PreviewToken != "") {
				t.Fatalf("preview blockers %+v token %q, want unread %t", preview.Legs[0].Blockers, preview.Legs[0].PreviewToken, tc.unread)
			}
		})
	}
}

// A cut that lapses leaves the hedge short of neutral, never past it, and the
// next plan sells the rest.
func TestReduceHedgePlanSellsTheRestWhenACutLapses(t *testing.T) {
	s := hedgeOrderServer(t, 25, hedgeTestCalls(9201, 100))
	workingOrdersAt(s, workingSale("SPY", "OPT", 9201, 75))
	sized := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(sized.Legs) != 1 {
		t.Fatalf("plan = %+v", sized)
	}
	workingOrdersAt(s)
	whole := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(whole.Legs) != 1 || whole.Legs[0].ReduceQuantity <= sized.Legs[0].ReduceQuantity {
		t.Fatalf("with the cut gone the plan sells %+v, after %d beside it", whole.Legs, sized.Legs[0].ReduceQuantity)
	}
}

// Hedge contracts already working and a working cut count together, and a book
// the working sales leave with nothing to hedge is said to be that.
func TestReduceHedgePlanCountsBothKindsOfWorkingSale(t *testing.T) {
	const perContract = 2700.0
	mes := ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 77, ClientIDPresent: true, Symbol: "MES", SecType: "FUT", ConID: hedgeTestConID,
		Currency: "USD", Multiplier: 5, Action: "SELL", OrderType: "LMT", LimitPrice: 6000, TotalQuantity: 1, Remaining: 1, Status: "Submitted"}
	s := hedgeOrderServer(t, 25, hedgeTestCalls(9201, 100))
	want := math.Floor(hedgeFallWith(t, s, map[int]float64{9201: 60})/perContract) - 1
	if want < 1 {
		t.Fatalf("fixture: %g contracts", want)
	}
	workingOrdersAt(s, mes, workingSale("SPY", "OPT", 9201, 60))
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 1 || float64(res.Legs[0].ReduceQuantity) != want {
		t.Fatalf("plan = %+v, want SELL %g", res.Legs, want)
	}
	if !strings.Contains(res.Message, "1 already working") || !strings.Contains(res.Message, "working sales on 1 line") {
		t.Fatalf("message %q does not name both", res.Message)
	}

	workingOrdersAt(s, workingSale("SPY", "OPT", 9201, 100))
	res = hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 0 || len(res.Blockers) != 1 || res.Blockers[0].Code != "nothing_to_hedge" || !strings.Contains(res.Blockers[0].Message, "working sales on 1 line") {
		t.Fatalf("plan = %+v, want nothing to hedge once the calls are sold", res)
	}
}

// A working combination's selling leg counts by its own direction and ratio.
// A combination's buying leg is not counted: that leg's short stays in the size
// until positions prove it closed, which errs toward less hedge.
func TestReduceHedgePlanCountsACombinationsSellingLeg(t *testing.T) {
	const perContract = 2700.0
	s := hedgeOrderServer(t, 25, hedgeTestCalls(9201, 100))
	want := math.Floor(hedgeFallWith(t, s, map[int]float64{9201: 70}) / perContract)
	if want < 1 {
		t.Fatalf("fixture: %g contracts", want)
	}
	scope := s.currentBrokerStateScope()
	leg := func(conID int, ratio int, action string) rpc.StrategyOrderLeg {
		return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: conID, Symbol: "SPY", SecType: "OPT"}, Ratio: ratio, Action: action}
	}
	if err := s.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-combo", PreviewTokenID: "synthetic-combo-token", ReservedOrderID: 301,
		Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", Symbol: "SPY", SecType: "BAG", Currency: "EUR", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, LimitPrice: 1,
		Quantity: 35, Remaining: 35, Status: "Submitted", StrategyGroup: &rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{leg(9201, 2, rpc.OrderActionSell), leg(9299, 1, rpc.OrderActionBuy)}}}); err != nil {
		t.Fatal(err)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) != 1 || float64(res.Legs[0].ReduceQuantity) != want {
		t.Fatalf("plan = %+v (message %q), want SELL %g with 70 calls in the combination's selling leg", res.Legs, res.Message, want)
	}
}
