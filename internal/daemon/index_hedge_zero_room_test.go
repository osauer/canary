package daemon

import (
	"maps"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The owner, 2026-11-06 14:23 CET: the one hedge Canary trades must stay open
// at the maximum loss, when the book needs it most. 3,000 index shares lose
// 15,000 in a 10% fall and one contract offsets 2,700, so five reach neutral
// with no loss room left; six would pass it.
func TestIndexHedgeSellsToNeutralWithNoLossRoom(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50))
	in, _ := s.reduceMandateInput(t.Context())
	in.Capital.Room = 0
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || res.Legs[0].ReduceQuantity != 5 {
		t.Fatalf("plan = %d legs %+v, message %q; want SELL 5 to neutral at zero room", len(res.Legs), res.Blockers, res.Message)
	}

	open := rpc.OrderPositionImpact{Before: 0, After: -5, Effect: rpc.OrderPositionEffectOpenShort}
	gate := &Server{}
	keepMandateEvaluationAt(t, gate, mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50)), 0.5)
	kept, _, why := gate.mandateCurrentInput()
	if kept == nil {
		t.Fatal(why)
	}
	kept.Capital.Room = 0
	if gate.riskAddingOrder(hedgeSale(5), open) {
		t.Error("a sale to neutral at zero room read as an addition")
	}
	if !gate.riskAddingOrder(hedgeSale(6), open) {
		t.Error("a sale past neutral read as a reduction")
	}
	if blocker := indexHedgeProjectedRiskRefusal(kept, hedgeSale(5), nil); blocker != nil {
		t.Errorf("placement refused a sale to neutral at zero room: %+v", blocker)
	}
	if blocker := indexHedgeProjectedRiskRefusal(kept, hedgeSale(6), nil); blocker == nil || blocker.Code != "index_hedge_past_neutral" {
		t.Errorf("placement of a sale past neutral = %+v", blocker)
	}
}

// What takes the place of the room test: the hedge may not turn a rise into a
// loss, because then nothing the book holds pays for it.
func TestIndexHedgeSaleRefusalIsTheFallAndTheRise(t *testing.T) {
	verdict := func(fall, rise float64) *mandate.Verdict {
		return &mandate.Verdict{Stresses: &mandate.Stresses{IndexLossBase: fall, IndexRiseLossBase: rise}}
	}
	for _, tc := range []struct {
		name          string
		before, after *mandate.Verdict
		code          string
	}{
		{"toward neutral, the rise still gains", verdict(15000, -15000), verdict(1500, -1500), ""},
		{"to exactly neutral", verdict(2700, -5000), verdict(0, -2300), ""},
		{"past neutral", verdict(15000, -15000), verdict(-1200, 1200), "index_hedge_past_neutral"},
		{"not lower", verdict(15000, -15000), verdict(15000, -15000), "index_hedge_past_neutral"},
		{"turns the rise into a loss", verdict(15000, -2000), verdict(1500, 11500), "index_hedge_rise_loss"},
		{"deepens a rise loss", verdict(15000, 2000), verdict(1500, 15500), "index_hedge_rise_loss"},
		{"a rise loss it did not make and does not deepen", verdict(15000, 2000), verdict(1500, 2000), ""},
	} {
		bar := indexHedgeSaleRefusal(tc.before, *tc.after, "EUR")
		switch {
		case bar == nil && tc.code != "":
			t.Errorf("%s: passed, want %s", tc.name, tc.code)
		case bar != nil && bar.code != tc.code:
			t.Errorf("%s: %s (%s), want %q", tc.name, bar.code, bar.leaves, tc.code)
		}
	}
}

// A short call caps what the shares gain in a rise, so the book has less to
// pay the hedge with than the fall's neutral asks for: the plan stops where a
// rise would start to lose and says why.
func TestReduceHedgePlanStopsWhereARiseWouldLose(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionCall("SPY", 9201, -30, 50, 50, 2, "20261218"))
	in, _ := s.reduceMandateInput(t.Context())
	in.Capital.Room = 0
	now := mandate.Evaluate(*in)
	if now.Stresses == nil || now.Stresses.IndexLossBase <= 2700 || now.Stresses.IndexRiseLossBase >= now.Stresses.IndexLossBase {
		t.Fatalf("fixture must lose more in the fall than it gains in the rise: %+v", now.Stresses)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Legs) == 1 && res.IndexLoss.IndexRiseLossAfterBase != nil && *res.IndexLoss.IndexRiseLossAfterBase > 1e-6 {
		t.Fatalf("plan sells %d and leaves a rise losing %g", res.Legs[0].ReduceQuantity, *res.IndexLoss.IndexRiseLossAfterBase)
	}
	if len(res.Legs) == 0 && !strings.Contains(res.Blockers[0].Message, "rise") {
		t.Fatalf("refusal does not name the rise: %+v", res.Blockers)
	}
	if len(res.Legs) == 1 && !strings.Contains(res.Message, "rise") {
		t.Fatalf("plan held below neutral does not say why: %q", res.Message)
	}
}

// The other half of option A (owner, 2026-11-07 07:33 CET): with an option on
// its mark's volatility the hedge offsets the edge that loses less, so no end of
// the band can leave it past neutral, at the plan and at placement alike.
func TestIndexHedgeSizesOnTheSmallerFallWhileAnOptionHasNoVolatility(t *testing.T) {
	s := hedgeServer(t, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionCall("SPY", 9201, 200, 50, 50, 2, "20261218"))
	in, _ := s.reduceMandateInput(t.Context())
	in.Capital.Room = 0
	in.Policy.IVFallback = &mandate.IVFallback{ShortBandPct: 65, MonthBandPct: 65, LongBandPct: 65, MeasuredPct: 5, RangePct: 20}
	marked := false
	for i := range in.Legs {
		if in.Legs[i].Kind == mandate.KindOption {
			in.Legs[i].IVSource, marked = mandate.IVSourceMark, true
		}
	}
	if !marked {
		t.Fatal("fixture has no option leg")
	}
	now := mandate.Evaluate(*in)
	if now.Status != mandate.StatusReduce || now.Stresses == nil || now.IVFallback == nil {
		t.Fatalf("verdict %s %v: want it to stand at zero room", now.Status, now.Reasons)
	}
	const perContract = 2700
	atBase, atEdge := int(now.Stresses.IndexLossBase/perContract), int(now.HedgeFallBase()/perContract)
	if atEdge < 1 || atBase <= atEdge {
		t.Fatalf("fixture must separate the sizes: base %d, edge %d", atBase, atEdge)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) > 0 || len(res.Legs) != 1 || res.Legs[0].ReduceQuantity != atEdge {
		t.Fatalf("plan = %d legs %+v, message %q; want SELL %d, the smaller fall", len(res.Legs), res.Blockers, res.Message, atEdge)
	}
	if !strings.Contains(res.Message, "priced on its mark") {
		t.Fatalf("plan does not say why it stops short of the base figure: %q", res.Message)
	}
	if blocker := indexHedgeProjectedRiskRefusal(in, hedgeSale(atEdge), nil); blocker != nil {
		t.Errorf("placement refused the sale the plan offered: %+v", blocker)
	}
	if blocker := indexHedgeProjectedRiskRefusal(in, hedgeSale(atBase), nil); blocker == nil || blocker.Code != "index_hedge_past_neutral" {
		t.Errorf("placement at the base figure = %+v, want past neutral", blocker)
	}
}

// A hedge held past the low edge but not past the base figure: the plan must not
// call that nothing to hedge, because Desk would then say the book needs none.
func TestReduceHedgePlanWaitsWhenOnlyTheBandSaysNeutral(t *testing.T) {
	mark := func(s *Server) *mandate.Input {
		in, _ := s.reduceMandateInput(t.Context())
		in.Capital.Room = 0
		in.Policy.IVFallback = &mandate.IVFallback{ShortBandPct: 65, MonthBandPct: 65, LongBandPct: 65, MeasuredPct: 5, RangePct: 20}
		for i := range in.Legs {
			if in.Legs[i].Kind == mandate.KindOption {
				in.Legs[i].IVSource = mandate.IVSourceMark
			}
		}
		return in
	}
	stock, call := mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionCall("SPY", 9201, 200, 50, 50, 2, "20261218")
	before := mandate.Evaluate(*mark(hedgeServer(t, stock, call)))
	if before.Stresses == nil {
		t.Fatalf("unmeasured: %v", before.Reasons)
	}
	held := int(before.HedgeFallBase()/2700) + 1
	s := hedgeServer(t, stock, call, mandateReductionFuture(750001, -float64(held), 6000))
	now := mandate.Evaluate(*mark(s))
	if now.Stresses == nil || now.Stresses.IndexLossBase <= 0 || now.HedgeFallBase() > 0 {
		t.Fatalf("fixture must lose at the base figure and not at the low edge: %v %+v", now.Reasons, now.IVFallback)
	}
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{PlanOnly: true})
	if len(res.Blockers) == 0 || res.Blockers[0].Code != "hedge_unsized" || !strings.Contains(res.Blockers[0].Message, "low end") {
		t.Fatalf("blockers %+v, want the wait named, not nothing to hedge", res.Blockers)
	}
}

// The harm the zero-room fix removes: one option on its mark's volatility held
// the whole reduction at the maximum loss (owner, 2026-11-07 07:33 CET).
func TestMandateReductionRunsAtZeroRoomWithAnOptionOnItsMark(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionCall("SPY", 9201, 20, 50, 50, 2, "20261218"))
	in := mandateReductionFixture(t, pos, 3, 1e6, 2)
	in.Input.Capital.Room = 0
	in.Input.Policy.IVFallback = &mandate.IVFallback{ShortBandPct: 65, MonthBandPct: 15, LongBandPct: 8, MeasuredPct: 5, RangePct: 20}
	for i := range in.Input.Legs {
		if in.Input.Legs[i].Kind == mandate.KindOption {
			in.Input.Legs[i].IVSource = mandate.IVSourceMark
		}
	}
	rows, st := mandateReductionRun(t, mandateReductionVerdict(*in.Input, 2), pos)
	if st.State == rpc.MandateReductionUnknown || len(rows) == 0 {
		t.Fatalf("state %s, %d rows (%s): the reduction must run on a verdict both edges give", st.State, len(rows), st.Reason)
	}
}

func hedgeCloseRow(t *testing.T, rows []rpc.TradeProposal) (rpc.TradeProposal, bool) {
	t.Helper()
	for _, row := range rows {
		if strings.EqualFold(row.Contract.SecType, "FUT") {
			return row, true
		}
	}
	return rpc.TradeProposal{}, false
}

// The other half of the owner's decision: as the calls go, the short goes with
// them. 600 index shares lose 3,000 in the fall against five contracts'
// 13,500, and shares are linear, so any contract past neutral also makes the
// rise lose: four go, and the one left leaves the book 300 short of neutral.
func TestMandateReductionBuysBackTheHedgePastNeutral(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	row, ok := hedgeCloseRow(t, rows)
	if !ok {
		t.Fatalf("no hedge close among %d rows (%+v)", len(rows), st)
	}
	if row.Action != rpc.OrderActionBuy || row.Quantity != 4 || row.PositionEffect != rpc.OrderPositionEffectReduce {
		t.Fatalf("close = %s %d (%s), want BUY 4 reducing", row.Action, row.Quantity, row.PositionEffect)
	}
	if row.Bucket != rpc.TradeProposalBucketMandateReduction || !row.NeverSkipVeto || row.Mandate == nil || row.Mandate.Phase != rpc.MandatePhaseHedge {
		t.Fatalf("close is not a vetoable mandate row of its own phase: %+v", row.Mandate)
	}
	if row.Mandate.Rank != 0 {
		t.Fatalf("rank %d: the close keeps its own clock, neither waiting behind the cuts nor holding them", row.Mandate.Rank)
	}
	if !strings.Contains(row.Reason, "past neutral") {
		t.Fatalf("reason %q does not say why", row.Reason)
	}
	if st.State != rpc.MandateReductionReducing || st.Rows != len(rows) {
		t.Fatalf("state %s rows %d, want reducing with %d", st.State, st.Rows, len(rows))
	}
}

// Everything the hedge covered gone: the batch has no cut left, and the whole
// short closes, so the book is never left a bare short of the index.
func TestMandateReductionClosesTheHedgeWhenNothingIsLeftToHedge(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionFuture(750001, -5, 6000))
	book := *mandateReductionFixture(t, pos, 3, 1e7, 2).Input
	book.Capital.Room = 0 // the hedge's own rise loss keeps the book over no room at all
	in := mandateReductionVerdict(book, 2)
	rows, st := mandateReductionRun(t, in, pos)
	row, ok := hedgeCloseRow(t, rows)
	if !ok {
		t.Fatalf("no hedge close (%+v)", st)
	}
	if row.Quantity != 5 || row.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("close = %d (%s), want all 5", row.Quantity, row.PositionEffect)
	}
	if st.State != rpc.MandateReductionReducing {
		t.Fatalf("state %s (%s), want reducing", st.State, st.Reason)
	}
}

// At neutral nothing is bought back: the sale the plan just made must not be
// undone by the next pass.
func TestMandateReductionLeavesAHedgeAtNeutral(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	rows, _ := mandateReductionRun(t, in, pos)
	if row, ok := hedgeCloseRow(t, rows); ok {
		t.Fatalf("a hedge at neutral got %s %d", row.Action, row.Quantity)
	}
	// Calls gain far more in a rise than they lose in a fall, so a hedge less
	// than one contract past neutral makes nothing lose: a small move in the
	// calls does not trade it.
	calls := mandateReductionCall("SPY", 9301, 40, 52, 50, 1.5, "20261218")
	calls.Currency, calls.FXRate = "EUR", new(1.0)
	fall := mandate.Evaluate(*mandateReductionFixture(t, mandateReductionBook(calls), 3, 1e7, 2).Input).Stresses.IndexLossBase
	over := math.Ceil(fall / 2700)
	if fall <= 0 || over < 1 {
		t.Fatalf("fixture: calls lose %g in the fall", fall)
	}
	pos = mandateReductionBook(calls, mandateReductionFuture(750001, -over, 6000))
	in = mandateReductionFixture(t, pos, 3, 1e7, 2)
	stresses := mandate.Evaluate(*in.Input).Stresses
	if stresses.IndexLossBase >= 0 || stresses.IndexRiseLossBase >= 0 {
		t.Fatalf("fixture must be past neutral with the rise still gaining: %+v", stresses)
	}
	rows, _ = mandateReductionRun(t, in, pos)
	if row, ok := hedgeCloseRow(t, rows); ok {
		t.Fatalf("a hedge under one contract past neutral got %s %d", row.Action, row.Quantity)
	}
}

// The owner's lock and ignore keep their meaning: a locked hedge is left out
// and said to be.
func TestMandateReductionLeavesALockedHedge(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	in.Locked = func(conID int) bool { return conID == 750001 }
	rows, st := mandateReductionRun(t, in, pos)
	if row, ok := hedgeCloseRow(t, rows); ok {
		t.Fatalf("a locked hedge got %s %d", row.Action, row.Quantity)
	}
	if !strings.Contains(strings.Join(st.LeftOut, "; "), "locked") {
		t.Fatalf("left out %v does not name the locked hedge", st.LeftOut)
	}
}

// A buy-back already working at the broker is not bought a second time.
func TestMandateReductionDoesNotDoubleAWorkingHedgeClose(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	in.WorkingOrder = func(row rpc.PositionView, action string) (bool, bool) {
		return row.ConID == 750001 && action == rpc.OrderActionBuy, false
	}
	rows, _ := mandateReductionRun(t, in, pos)
	if row, ok := hedgeCloseRow(t, rows); ok {
		t.Fatalf("a working close got another row: %s %d", row.Action, row.Quantity)
	}
}

// Within the room nothing acts: the measure already counts the hedge's rise
// loss, so it fits the mandate, and hedge_not_needed tells the owner.
func TestMandateReductionWithinTheRoomLeavesTheHedgeToTheOwner(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 0.5, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	if len(rows) != 0 || st.State != rpc.MandateReductionWithin {
		t.Fatalf("%d rows, state %s; want none within the room", len(rows), st.State)
	}
}

// The close reaches the broker as a hedge's close: a limit at the ask, good for
// the day, in regular hours, under the proposal source the automatic path
// checks, and never given a bounded limit the future path refuses.
func TestHedgeCloseRowPreviewsThroughTheFuturePath(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(hedgeTestConID, -5, 6000))
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(hedgeTestConID, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	row, ok := hedgeCloseRow(t, rows)
	if !ok {
		t.Fatalf("no close (%+v)", st)
	}
	engine := &proposalEngine{server: s}
	if bound := engine.mandateReductionGive(row); bound != nil {
		t.Fatalf("the close was given a bounded limit: %+v", bound)
	}
	params := proposalOrderPreviewParams(row, row.Quantity, 0)
	preview, err := s.previewOrder(t.Context(), params)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !preview.SubmitEligible || preview.Draft.Action != rpc.OrderActionBuy || preview.Draft.Quantity != 4 || preview.Draft.Source != proposalOrderSource {
		t.Fatalf("preview = %+v", preview.Draft)
	}
	if blockers := proposalPreviewSafetyBlockers(row, preview); len(blockers) > 0 {
		t.Fatalf("safety blockers %+v", blockers)
	}
	// A proposal never sells the future: the sale stays the device-confirmed
	// hedge alone.
	sell := params
	sell.Action = rpc.OrderActionSell
	if _, err := s.previewOrder(t.Context(), sell); err == nil {
		t.Fatal("a proposal sold the future")
	}
}

// The future's preview refuses outside US regular hours, and that refusal is
// final for a record, so the close waits for the open as a cut does rather
// than fall due overnight and fail.
func TestHedgeCloseRowWaitsForTheUSSession(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionFuture(hedgeTestConID, -5, 6000))
	next := mandateTestNow.Add(5 * time.Hour)
	s.previewSessionAt = func(m marketcal.Market, _ time.Time) (marketcal.Session, error) {
		if m != marketcal.MarketUSEquity {
			t.Errorf("the future's session was read on %s", m)
		}
		return marketcal.Session{Market: m, State: marketcal.StateClosed, IsOpen: false, NextOpen: &next}, nil
	}
	engine := &proposalEngine{server: s}
	row := rpc.TradeProposal{Key: "k", Bucket: rpc.TradeProposalBucketMandateReduction, Action: rpc.OrderActionBuy, Quantity: 3, SecType: "FUT",
		Contract: rpc.ContractParams{ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", Multiplier: 5}}
	if due := engine.automaticSessionDue(row, mandateTestNow); due.Before(next) {
		t.Fatalf("due %s, before the open %s", due, next)
	}
}

// hedgeReviewInput is a book at a stated loss room, with the index gap shocked.
func hedgeReviewInput(t *testing.T, pos *rpc.PositionsResult, room float64) mandateReductionInput {
	t.Helper()
	legs, notModelled, fxNow, reasons := mandateLegsFromPositions(pos, "EUR", mandateTestNow)
	if len(reasons) > 0 {
		t.Fatal(reasons)
	}
	in := mandate.Input{Now: mandateTestNow, BaseCurrency: "EUR", Policy: mandate.Policy{MaxLossPct: 30, VolTargetPct: 15, GapShockPct: 25, GapRoomPct: 33.3, IndexGapShockPct: 12},
		Capital: mandate.Capital{Known: true, BaseCurrency: "EUR", BudgetBase: room / 0.3, Room: 1, BudgetAnnualSD: 1e9},
		Legs:    legs, NotModelled: notModelled, FXNow: fxNow, Series: map[string]mandate.Series{"SPY": mandateSeriesInput(mandateTestSeries("u:SPY", 0.16, 1, 0), mandateTestNow)}, FX: map[string]mandate.Series{}}
	for _, name := range hedgeReviewNames {
		in.Series[name] = mandateSeriesInput(mandateTestSeries("u:SPY", 0.22, 1, 0), mandateTestNow)
	}
	if _, ok := fxNow["USD"]; ok {
		in.FX["USD"] = mandateSeriesInput(mandateTestSeries("fx:USD", 0.08, 9, 0), mandateTestNow)
	}
	return mandateReductionVerdict(in, 2)
}

var hedgeReviewNames = []string{"NAA", "NBB", "NCC", "NDD"}

func hedgeReviewCalls() []rpc.PositionView {
	var out []rpc.PositionView
	mark := mandate.OptionPrice(true, 50, 52, 1.1, 0.4)
	for i, name := range hedgeReviewNames {
		out = append(out, mandateReductionCall(name, 9400+i, 100, 52, 50, mark, "20271217"))
	}
	return out
}

func hedgeReviewSold(rows []rpc.TradeProposal) map[string]int {
	sold := map[string]int{}
	for _, row := range rows {
		if row.Action == rpc.OrderActionSell {
			sold[row.Symbol] += row.Mandate.Units
		}
	}
	return sold
}

// Found by review: the measure counts a hedge's rise loss, and selling calls
// was the sizer's only lever, so a neutral hedge sold extra calls to offset its
// own figure and then stood past neutral. A hedge never makes the reduction
// sell: the cuts are what the same book would cut without it.
func TestMandateReductionAHedgeSellsNoExtraCalls(t *testing.T) {
	t.Parallel()
	calls := hedgeReviewCalls()
	bare := hedgeReviewInput(t, mandateReductionBook(calls...), 1e9).Result.Verdict
	premium, fall := bare.Loss.TotalBase, bare.Stresses.IndexLossBase
	neutral := math.Floor(fall / 2700)
	if neutral < 1 {
		t.Fatalf("fixture: fall %g", fall)
	}
	hedged := append(slices.Clone(calls), mandateReductionFuture(750001, -neutral, 6000))
	for _, tc := range []struct {
		name  string
		ratio float64
		sells bool
	}{
		{"over its room without the hedge", 0.9, true},
		{"inside its room without the hedge", 1.2, false},
	} {
		room := premium * tc.ratio
		pos := mandateReductionBook(calls...)
		without, _ := mandateReductionRun(t, hedgeReviewInput(t, pos, room), pos)
		pos = mandateReductionBook(hedged...)
		with, st := mandateReductionRun(t, hedgeReviewInput(t, pos, room), pos)
		if got, want := hedgeReviewSold(with), hedgeReviewSold(without); !maps.Equal(got, want) {
			t.Errorf("%s: hedged sells %v, unhedged %v", tc.name, got, want)
		}
		if tc.sells != (len(hedgeReviewSold(with)) > 0) {
			t.Errorf("%s: sold %v (%s)", tc.name, hedgeReviewSold(with), st.Reason)
		}
		if st.LossBase <= premium*0.999 && tc.ratio > 1 {
			t.Errorf("%s: the status hides the hedge's rise loss: measure %g", tc.name, st.LossBase)
		}
	}
}

// The close holds with the cuts and keeps its clock, like every standing row.
func TestMandateReductionHoldKeepsTheHedgeClose(t *testing.T) {
	t.Parallel()
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 600, 50), mandateReductionFuture(750001, -5, 6000))
	in := mandateReductionFixture(t, pos, 3, 1e7, 2)
	rows, st := mandateReductionRun(t, in, pos)
	if _, ok := hedgeCloseRow(t, rows); !ok {
		t.Fatalf("fixture: no close (%+v)", st)
	}
	in.Previous, in.LocksUnknown = rows, "the keep list cannot be read"
	held, st := mandateReductionRun(t, in, pos)
	row, ok := hedgeCloseRow(t, held)
	if !ok || row.Mandate == nil || !row.Mandate.Held || row.Quantity != 4 {
		t.Fatalf("held rows %+v (%+v), want the close held at 4", held, st)
	}
	if st.Held != true {
		t.Fatalf("status %+v", st)
	}
}

// A close the rise triggers says so: the book is not past neutral.
func TestHedgeCloseSaysWhyWhenTheRiseTriggersIt(t *testing.T) {
	t.Parallel()
	mark := mandate.OptionPrice(true, 50, 50, 0.1, 0.4)
	pos := mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionCall("SPY", 9201, -30, 50, 50, mark, "20261218"), mandateReductionFuture(750001, -3, 6000))
	in := hedgeReviewInput(t, pos, 1000)
	rows, st := mandateReductionRun(t, in, pos)
	row, ok := hedgeCloseRow(t, rows)
	if !ok {
		t.Fatalf("no close (%+v)", st)
	}
	if strings.Contains(row.Reason, "past neutral") || !strings.Contains(row.Reason, "rise") {
		t.Fatalf("reason %q", row.Reason)
	}
}

// A close refused for the session waits; it is not a final failure.
func TestHedgeCloseOutsideHoursIsTemporary(t *testing.T) {
	res := rpc.TradeProposalSubmitResult{Blockers: []rpc.TradingBlocker{{Code: indexFutureOutsideHoursCode}}}
	if !automaticTransientRefusal(res, nil, nil) {
		t.Fatal("the future's session refusal ended the record")
	}
}
