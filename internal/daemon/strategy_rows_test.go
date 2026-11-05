package daemon

import (
	"context"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Rows for any live register entry (Desk docs/strategy-engine.md, build 2;
// strategy-register.md section 2), witnessed on the synthetic trend fund
// written live in a sleeve of its own beside the live core: its month's
// decision is one strategy_rebalance row with the core's terms on its own
// line, keyed by its id and sleeve, through the same gates, and its line is
// reserved to its sleeve once it holds it alone.

const trendLiveSleeve = "trend"

// testLiveTrendConstitution is the core's constitution with the trend fund
// written live in its own sleeve, at a 4% risk allocation.
func testLiveTrendConstitution() *risk.Constitution {
	c := testTrendFundConstitution()
	e := c.Mandate.Strategies[trendFundID]
	e.Stage, e.Sleeve, e.RiskAllocationPct = risk.StageLive, trendLiveSleeve, new(4.0)
	if e.KillDrawdownK == nil {
		e.KillDrawdownK = new(0.35) // a live entry needs its kill bound written (2026-10-19)
	}
	return c
}

// liveTrendServer is the core test server at a clock with the live trend
// fund: IBKR's answer for both ISINs, the fund's London series under its
// own key, an order journal, the proposal engine, both lines resolved and
// the mandate measured on them.
func liveTrendServer(t *testing.T, c *risk.Constitution, pos *rpc.PositionsResult, now time.Time) (*Server, *proposalEngine) {
	t.Helper()
	srv := coreTestServer(t, c, pos, 0.25)
	srv.now = func() time.Time { return now }
	srv.coreListingsForTest = (&byISINListings{listings: []ibkrlib.Listing{coreTestListing(), trendFundListing()}}).lookup
	series := coreTestSeriesOn(t, trendFundListing(), marketcal.MarketUKLSE, coreTestDatesOn(t, marketcal.MarketUKLSE), 0.25)
	series.Key = strategySeriesKey(trendFundID, trendFundConID)
	if err := srv.saveMandateSeries(t.Context(), series); err != nil {
		t.Fatal(err)
	}
	srv.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders"))
	engine := &proposalEngine{server: srv, now: srv.now, queued: &queuedAuthStore{}, store: &proposalStore{}}
	srv.tradeProposals = engine
	measure(t, srv)
	return srv, engine
}

// measure resolves every entry's line and measures the mandate on it, as
// the background lane does.
func measure(t *testing.T, srv *Server) {
	t.Helper()
	srv.refreshStrategies(t.Context())
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict == nil {
		t.Fatal("no mandate verdict")
	}
}

func liveTrendTarget(t *testing.T, srv *Server) *rpc.StrategyTargetResult {
	t.Helper()
	r, ok := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
	if !ok {
		t.Fatal("no trend-fund entry")
	}
	return srv.evaluateStrategyTarget(t.Context(), srv.nowUTC(), r)
}

// strategyGeneration generates one proposal set and returns its rows by
// bucket and every other entry's status.
func strategyGeneration(t *testing.T, srv *Server, engine *proposalEngine, pos *rpc.PositionsResult) (coreRows, entryRows []rpc.TradeProposal, st []rpc.TradeProposalCoreStatus) {
	t.Helper()
	rows, _, _, _, _, _, st := engine.generateBookWithStrategies(t.Context(), defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, srv.currentBrokerStateScope(), srv.now())
	for _, p := range rows {
		switch p.Bucket {
		case rpc.TradeProposalBucketCoreRebalance:
			coreRows = append(coreRows, p)
		case rpc.TradeProposalBucketStrategyRebalance:
			entryRows = append(entryRows, p)
		}
	}
	return coreRows, entryRows, st
}

// recordEntryFill journals one of the entry's own fills the way the write
// path leaves it: the proposal journal's submitted event of its row and the
// order journal's execution of that order.
func recordEntryFill(t *testing.T, srv *Server, ref, execID, side string, shares, price float64, at time.Time) {
	t.Helper()
	scope := srv.currentBrokerStateScope()
	st := srv.tradeProposals.store
	st.mu.Lock()
	st.events = append(st.events, proposalEvent{At: at, Type: "submitted", Key: "strategy_rebalance:synthetic", Bucket: rpc.TradeProposalBucketStrategyRebalance,
		AccountID: scope.Account, AccountMode: scope.Mode, OrderRef: ref, Strategy: trendFundID, Sleeve: trendLiveSleeve, ConID: trendFundConID})
	st.mu.Unlock()
	ev := originTestOrder(ref, 900+len(execID), at)
	ev.Type, ev.Status, ev.ConID, ev.Symbol, ev.Action = orderJournalEventStatusUpdated, "Filled", trendFundConID, "SYNF", side
	ev.ExecID, ev.ExecShares, ev.ExecSide, ev.LastFillPrice, ev.Filled = execID, shares, map[string]string{rpc.OrderActionBuy: "BOT", rpc.OrderActionSell: "SLD"}[side], price, shares
	if err := srv.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
}

// A live entry's decision day creates exactly one strategy_rebalance row
// beside the core's own row: the entry's buy of its line by contract id, the
// core's terms (a DAY limit at the live mid, the window, the due time) under
// the entry's own episode, keyed by its id and sleeve; it passes the typed
// exception within its sized quantity only, is classified a strategy
// addition for Desk, is never pre-authorised and never queued, and its
// decision, row view, status line and kill mark are recorded once. The core's row is unchanged.
func TestStrategyRowOnTheDecisionDayBesideTheCore(t *testing.T) {
	pos := mandateTestPositions(100)
	srv, engine := liveTrendServer(t, testLiveTrendConstitution(), pos, coreDecisionNow)
	res := liveTrendTarget(t, srv)
	if res.Status != rpc.CoreStatusReady || res.Action == nil || res.Action.Kind != core.ActionBuy || res.ReadOnly || res.Decision.Decision != "2026-09-30" {
		t.Fatalf("verdict %s %v action %+v read-only %v", res.Status, res.Reasons, res.Action, res.ReadOnly)
	}
	coreRows, entryRows, statuses := strategyGeneration(t, srv, engine, pos)
	if len(coreRows) != 1 || len(entryRows) != 1 {
		t.Fatalf("core rows %d, entry rows %d; want one each", len(coreRows), len(entryRows))
	}
	p := entryRows[0]
	if p.Action != rpc.OrderActionBuy || p.Quantity != res.Action.Quantity || p.MaxQuantity != p.Quantity || p.PositionEffect != rpc.OrderPositionEffectOpen || p.Key == coreRows[0].Key ||
		p.OrderType != rpc.OrderTypeLMT || p.TIF != rpc.OrderTIFDay || p.Contract.ConID != trendFundConID || p.Contract.Exchange != "SMART" || p.Contract.PrimaryExch != "LSEETF" ||
		p.Contract.Market != "uk" || p.Contract.SecType != "STK" || p.State != rpc.TradeProposalStateGenerated || len(p.Blockers) != 0 || p.Shadow || !strings.HasPrefix(p.Key, rpc.TradeProposalBucketStrategyRebalance+":") {
		t.Fatalf("row %+v", p)
	}
	c := p.Core
	if c == nil || c.Strategy != trendFundID || c.Sleeve != trendLiveSleeve || c.Rule != register.TimedHoldingRuleID || c.Stage != "live" || c.Session != "2026-10-01" ||
		c.Kind != core.ActionBuy || c.ConID != trendFundConID || c.Target != res.Sizing.Target || c.DecisionClose != res.Decision.Close {
		t.Fatalf("figures %+v", c)
	}
	if !strings.HasPrefix(c.Terms.Episode, "strategy:"+trendFundID+":") || !strings.HasSuffix(c.Terms.Episode, ":2026-09") || c.Terms.PrimaryExchange != "LSEETF" || len(c.Terms.Window) != core.WindowSessions || c.Terms.Window[0] != "2026-10-01" {
		t.Fatalf("terms %+v", c.Terms)
	}
	if !strings.Contains(p.Reason, "trend-fund strategy (sleeve trend), decision 2026-09-30") || !strings.Contains(strings.Join(p.Details, " "), "standing mandate") {
		t.Fatalf("reason %q details %v", p.Reason, p.Details)
	}
	if k := coreRows[0].Core; k == nil || k.Strategy != "" || k.Rule != "" || k.Sleeve != "core" || !strings.HasPrefix(k.Terms.Episode, "core:") {
		t.Fatalf("the core's row %+v", k)
	}
	if len(statuses) != 1 || statuses[0].Strategy != trendFundID || statuses[0].Sleeve != trendLiveSleeve || !statuses[0].Served || statuses[0].RowKey != p.Key || statuses[0].State != "live" {
		t.Fatalf("statuses %+v", statuses)
	}
	// The typed exception admits the row's own buy within its sized
	// quantity and nothing more; the Desk contract classifies it as a
	// strategy addition bound to its session's episode; it is due fifteen
	// minutes after the US cash open, never pre-authorised and never queued.
	x, ok := coreOpenExceptionFor(p)
	if !ok || x.Strategy != trendFundID || x.ConID != trendFundConID || x.MaxQuantity != p.Quantity {
		t.Fatalf("exception %+v %v", x, ok)
	}
	preview := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: p.Contract, Quantity: p.Quantity, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: c.DecisionClose},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpen}}
	if b := x.previewBlockers(preview); len(b) != 0 {
		t.Fatalf("the row's own buy refused: %+v", b)
	}
	preview.Draft.Quantity++
	if b := x.previewBlockers(preview); len(b) != 1 || !strings.Contains(b[0].Message, "the trend-fund row's DAY limit buy") {
		t.Fatalf("one share more: %+v", b)
	}
	stray := p
	stray.Core = &rpc.TradeProposalCore{Kind: core.ActionBuy, ConID: trendFundConID, Currency: "EUR", Target: c.Target}
	if _, ok := coreOpenExceptionFor(stray); ok {
		t.Fatal("a strategy_rebalance buy that names no entry passed the exception")
	}
	if deskExecutionClass(p) != rpc.DeskClassAdd || deskExecutionStrategyEpisode(p) != c.Terms.Episode+":2026-10-01" || !strings.Contains(deskExecutionStrategyName(p), "strategy trend-fund (timed-holding-v1), sleeve trend") {
		t.Fatalf("desk class %q episode %q name %q", deskExecutionClass(p), deskExecutionStrategyEpisode(p), deskExecutionStrategyName(p))
	}
	if due, ok := coreRowDueAt(p, srv.now()); !ok || !due.Equal(time.Date(2026, 10, 1, 13, 45, 0, 0, time.UTC)) {
		t.Fatalf("due %s %v", due, ok)
	}
	if b := coreDueBlockers(p, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)); len(b) != 1 || b[0].Code != coreBeforeDueCode || !strings.Contains(b[0].Message, "the trend-fund row is sent from") {
		t.Fatalf("before the due time %+v", b)
	}
	if validPreAuthorisedBucket(rpc.TradeProposalBucketStrategyRebalance) || automaticBucketFor(p) != "" || engine.automaticBucket(p) != "" {
		t.Fatal("a strategy row is pre-authorisable")
	}
	if engine.coreLine(p) {
		// Nobody holds the line yet; once the entry's own fills hold it alone
		// it leaves protection's automatic sales (TestStrategyLineIsPlacedByLineage).
		t.Fatal("a line nobody holds left protection's automatic sales")
	}
	// The decision is recorded once with the row's key, the row is served on
	// the entry's verdict and on `canary status`, and the kill rule's first
	// mark is kept from its own fills (none yet).
	for range 2 {
		srv.refreshStrategies(t.Context())
	}
	scope := frozenScopeFor(func() register.Resolved {
		r, _ := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
		return r
	}(), srv.currentBrokerStateScope(), trendFundConID)
	if f := srv.coreFrozen(t.Context(), scope); f == nil || f.Row == nil || !f.Row.Served || f.Row.RowKey != p.Key || f.Row.Kind != core.ActionBuy || f.Row.Stage != "live" {
		t.Fatalf("recorded decision %+v", f)
	}
	engine.snapshot = rpc.TradeProposalSnapshot{Kind: "trade-proposals", Proposals: append(coreRows, entryRows...), StrategyRebalance: statuses,
		AccountID: srv.currentBrokerStateScope().Account, AccountMode: srv.currentBrokerStateScope().Mode}
	again := liveTrendTarget(t, srv)
	if again.Row == nil || again.Row.Key != p.Key || again.Row.Session != "2026-10-01" || again.Row.Terms.Episode != c.Terms.Episode {
		t.Fatalf("row view %+v", again.Row)
	}
	if d := again.LastDecision; d == nil || d.Decision != "2026-09-30" || !d.Outcome.Served || d.RowKey != p.Key {
		t.Fatalf("last decision %+v", again.LastDecision)
	}
	health := srv.strategyHealth()
	if len(health) != 2 || health[0].ID != risk.CoreStrategyID || health[1].ID != trendFundID || health[1].Row == nil || health[1].Row.Key != p.Key || health[1].Row.Session != "2026-10-01" || health[1].State != "live" {
		t.Fatalf("status lines %+v", health)
	}
	marks, err := testLiveMarks(t, srv, trendFundID, trendFundConID)
	if err != nil || len(marks) != 1 || marks[0].Session != "2026-09-30" || marks[0].Units != 0 || marks[0].Cash != 0 || !(marks[0].SleeveBudget > 0) {
		t.Fatalf("kill marks %+v %v", marks, err)
	}
	if records, _ := srv.shadowRecords(t.Context(), trendFundID); len(records) != 0 {
		t.Fatalf("a live entry wrote shadow records: %+v", records)
	}
	if again.Kill == nil || again.Kill.Source != rpc.KillSourceOwnFills || !again.Kill.Measured || again.Kill.Breached {
		t.Fatalf("kill %+v", again.Kill)
	}
}

// withPositions swaps the book the mandate reads and measures it again.
func withPositions(t *testing.T, srv *Server, pos *rpc.PositionsResult) *mandate.Input {
	t.Helper()
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	measure(t, srv)
	_, input, _, _ := srv.mandateVerdicts.current()
	return input
}

// Placement by lineage (mandate-risk-budget.md, "Placement"; refuter
// findings F1, F2 and F4, 2026-10-19): units of the line the owner bought
// stay the owner's, the entry's are what its own fills left, a line both
// hold is split into two legs, a hand sale beyond the owner's units takes
// the entry's, and the entry holds while the owner holds any of its line.
// The first build reserved the whole line on the first fill and adopted the
// owner's units.
func TestStrategyLineIsPlacedByLineage(t *testing.T) {
	base := mandateTestPositions(100)
	srv, engine := liveTrendServer(t, testLiveTrendConstitution(), withSYNF(base, 500, 90), coreDecisionNow)
	own := *srv.mandateHistorySeries(t.Context(), strategySeriesKey(trendFundID, trendFundConID))
	own.Key = "u:SYNF"
	if err := srv.saveMandateSeries(t.Context(), &own); err != nil {
		t.Fatal(err)
	}
	line := mandateConLegID(trendFundConID)
	split := strategyLegID(trendLiveSleeve, trendFundConID)

	// The owner's 500 units, the entry with no fill: the owner's line; the
	// entry holds, names them and serves no row; protection keeps the line.
	input := withPositions(t, srv, withSYNF(base, 500, 90))
	if input.Allocations.SleeveOf(line) != mandate.SleeveOwner {
		t.Fatalf("the owner's units placed in %s", input.Allocations.SleeveOf(line))
	}
	res := liveTrendTarget(t, srv)
	if res.Status != rpc.CoreStatusHold || res.Holding.Shares != 0 || !slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "500 units of SYNF are yours") }) {
		t.Fatalf("status %s reasons %v holding %+v", res.Status, res.Reasons, res.Holding)
	}
	if _, rows, _ := strategyGeneration(t, srv, engine, withSYNF(base, 500, 90)); len(rows) != 0 {
		t.Fatalf("rows over the owner's units: %+v", rows)
	}
	ownerRow := rpc.TradeProposal{Contract: rpc.ContractParams{ConID: trendFundConID}}
	if engine.coreLine(ownerRow) {
		t.Fatal("F2: the owner's holding lost protection's automatic sales")
	}

	// The entry's own row fills 159 beside the owner's 500: the line splits,
	// the entry holds its 159 and still holds while the owner's units stand.
	recordEntryFill(t, srv, "synthetic-trend-1", "exec-trend-1", rpc.OrderActionBuy, 159, 90, coreDecisionNow.Add(-time.Hour))
	if f := srv.strategyOwnFills(t.Context(), trendFundID, trendFundConID); f.Units != 159 || f.Cash != -14310 || f.Execs != 1 {
		t.Fatalf("own fills %+v", f)
	}
	input = withPositions(t, srv, withSYNF(base, 659, 90))
	if input.Allocations.SleeveOf(line) != mandate.SleeveOwner || input.Allocations.SleeveOf(split) != trendLiveSleeve ||
		strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 159 || strategyOwnerUnitsInBook(input, trendFundConID) != 500 {
		t.Fatalf("F4: split legs %+v placed %+v", input.Legs, input.Allocations.Placed)
	}
	if res := liveTrendTarget(t, srv); res.Holding.Shares != 159 || res.Status != rpc.CoreStatusHold {
		t.Fatalf("beside the owner's units: holding %+v status %s", res.Holding, res.Status)
	}
	// The forced reduction sizes a split line as one line on the broker's
	// position, so the entry's units are within its reach (refuter R2).
	merged := mandateMergedLegs(input.Legs)
	if i := slices.IndexFunc(merged, func(l mandate.Leg) bool { return l.ID == line }); i < 0 || merged[i].Quantity != 659 ||
		slices.ContainsFunc(merged, func(l mandate.Leg) bool { return strings.HasPrefix(l.ID, "sleeve:") }) {
		t.Fatalf("R2: the reduction's legs %+v", merged)
	}

	// The owner sells their 500: the line is the entry's alone, placed whole,
	// out of protection's automatic sales; the entry trades again.
	input = withPositions(t, srv, withSYNF(base, 159, 90))
	if input.Allocations.SleeveOf(line) != trendLiveSleeve || strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 159 {
		t.Fatalf("the entry's line alone: %+v", input.Allocations.Placed)
	}
	if res := liveTrendTarget(t, srv); res.Holding.Shares != 159 || slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "are yours") }) {
		t.Fatalf("holding %+v reasons %v", res.Holding, res.Reasons)
	}
	if !engine.coreLine(ownerRow) {
		t.Fatal("the entry's own line is inside protection's automatic sales")
	}
	// A hand sale beyond the owner's units takes the entry's (rule 2).
	if input := withPositions(t, srv, withSYNF(base, 100, 90)); strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 100 {
		t.Fatalf("after a hand sale of the entry's units: %+v", input.Legs)
	}
	// While the entry is live a hand buy of its line is refused, so the
	// owner's units never mix into it again through Canary (refuter round
	// 2, R1); the entry's own row buys it, and its line stays its own.
	hand := rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{ConID: trendFundConID, Symbol: "SYNF", SecType: "STK", Currency: "EUR"}, Quantity: 10, OrderType: rpc.OrderTypeLMT, LimitPrice: 90}
	if b := srv.mandateBudgetGate(hand, rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectIncrease}); b == nil || !strings.Contains(b.Message, "this fund line is the trend-fund strategy's while it is live") {
		t.Fatalf("R1: a hand buy of a live entry's line passed: %+v", b)
	}
	handSale := hand
	handSale.Action = rpc.OrderActionSell
	if b := srv.mandateBudgetGate(handSale, rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectReduce}); b != nil {
		t.Fatalf("a hand sale of the entry's line was refused: %+v", b)
	}
	recordEntryFill(t, srv, "synthetic-trend-4", "exec-trend-4", rpc.OrderActionBuy, 100, 90, coreDecisionNow.Add(-30*time.Minute))
	if input := withPositions(t, srv, withSYNF(base, 200, 90)); input.Allocations.SleeveOf(line) != trendLiveSleeve || strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 200 {
		t.Fatalf("the entry's own buy after the hand sale: %+v placed %+v", input.Legs, input.Allocations.Placed)
	}

	// Orders are placed by origin (F1): a hand buy of the entry's line is
	// the owner's, on a leg of its own; the entry's own row is the entry's;
	// a strategy order that is not a stock order of its own line, a
	// combination or a hedge put included, is refused before any pass.
	_, input, _, _ = srv.mandateVerdicts.current()
	if sleeve, leg := mandateOrderPlacement(input.Allocations, hand, ""); sleeve != mandate.SleeveOwner || leg != line+":owner" {
		t.Fatalf("F1: a hand buy of the entry's line placed %s on %q", sleeve, leg)
	}
	entryBuy := hand
	entryBuy.RegisterEntry = trendFundID
	if sleeve, leg := mandateOrderPlacement(input.Allocations, entryBuy, trendLiveSleeve); sleeve != trendLiveSleeve || leg != "" {
		t.Fatalf("the entry's own buy placed %s on %q", sleeve, leg)
	}
	if b := srv.mandateBudgetGate(entryBuy, rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectIncrease}); b != nil && strings.Contains(b.Message, "trades only its own fund line") {
		t.Fatalf("the entry's own stock buy refused as foreign: %+v", b)
	}
	combo := entryBuy
	combo.Contract.SecType = "BAG"
	put := rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{ConID: 777001, Symbol: "SPY", SecType: "OPT", Right: "P", Currency: "USD"}, Quantity: 1, OrderType: rpc.OrderTypeLMT, LimitPrice: 2, RegisterEntry: trendFundID}
	other := entryBuy
	other.Contract.ConID = 4242
	for name, d := range map[string]rpc.OrderDraft{"a combination": combo, "a hedge put": put, "another contract": other} {
		if b := srv.mandateBudgetGate(d, rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpen}); b == nil || !strings.Contains(b.Message, "trades only its own fund line") {
			t.Fatalf("F8: %s named as the entry's own passed: %+v", name, b)
		}
	}
	resolver := scorecardSleeveResolver([]orderJournalEvent{{ExecID: "exec-trend-1", OrderRef: "synthetic-trend-1"}}, srv.tradeProposals.store.eventsSince(time.Time{}))
	if got := resolver(edgecore.PositionChange{ExecutionIDs: []string{"exec-trend-1"}}); got.Name != trendLiveSleeve || got.StrategyRef != trendFundID || got.Origin != "proposal:strategy_rebalance" {
		t.Fatalf("scorecard sleeve %+v", got)
	}
}

// The entry's units are read afresh at every verdict (no state to drift):
// an execution journaled before the position updates, or the reverse, reads
// right on the next verdict; an order journal that cannot be read holds the
// entry rather than let it trade on a guess (refuter round 3, L1 and L2).
func TestStrategyUnitsReadAfreshEachVerdict(t *testing.T) {
	base := mandateTestPositions(100)
	srv, _ := liveTrendServer(t, testLiveTrendConstitution(), base, coreDecisionNow)
	// Journaled first: the position still reads flat, the entry holds none.
	recordEntryFill(t, srv, "synthetic-trend-2", "exec-trend-2", rpc.OrderActionBuy, 159, 90, coreDecisionNow)
	if input := withPositions(t, srv, base); strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 0 {
		t.Fatalf("before the position updates: %+v", input.Legs)
	}
	// The position catches up: the line is the entry's, whole.
	if input := withPositions(t, srv, withSYNF(base, 159, 90)); input.Allocations.SleeveOf(mandateConLegID(trendFundConID)) != trendLiveSleeve || strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 159 {
		t.Fatalf("after the position updates: %+v", input.Allocations.Placed)
	}
	// The journal cannot be read: the entry holds and names it.
	if err := srv.orderJournal.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv); res.Status != rpc.CoreStatusHold || !slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "the order journal could not be read") }) {
		t.Fatalf("with the journal unreadable: %s %v", res.Status, res.Reasons)
	}
	// ...and the verdict keeps the line the entry's, so protection's
	// automatic sales and the owner's sleeve never take its units (P1).
	if input := withPositions(t, srv, withSYNF(base, 159, 90)); input.Allocations.SleeveOf(mandateConLegID(trendFundConID)) != trendLiveSleeve {
		t.Fatalf("P1: with the journal unreadable the line is placed %+v", input.Allocations.Placed)
	}
}

// A standing mandate armed before the register changed sends no strategy
// row until the owner confirms again (refuter finding F6; strategy-register
// section 2): the first build sent a newly written live entry's row under an
// arming that never named it.
func TestDeskExecutionRefusesStrategyRowsAfterARegisterEdit(t *testing.T) {
	srv, engine := liveTrendServer(t, testLiveTrendConstitution(), mandateTestPositions(100), coreDecisionNow)
	_, rows, _ := strategyGeneration(t, srv, engine, mandateTestPositions(100))
	if len(rows) != 1 {
		t.Fatalf("rows %+v", rows)
	}
	engine.snapshot = rpc.TradeProposalSnapshot{Kind: "trade-proposals", Proposals: rows, AccountID: srv.currentBrokerStateScope().Account, AccountMode: srv.currentBrokerStateScope().Mode}
	armed := deskAuthorityRecord{RegisterDigest: srv.deskRegisterDigest()}
	if armed.RegisterDigest == "" || srv.deskExecutionRegisterChanged(armed, rows[0].Key) != nil {
		t.Fatalf("the armed register refused its own row (digest %q)", armed.RegisterDigest)
	}
	c := testLiveTrendConstitution()
	c.Mandate.Strategies[trendFundID].ScalePct = new(50.0)
	srv.riskPolicies.active = c
	if b := srv.deskExecutionRegisterChanged(armed, rows[0].Key); b == nil || b.Code != rpc.DeskBlockerAuthorityChanged || !strings.Contains(b.Message, "the strategy register changed since the mandate was armed") {
		t.Fatalf("a re-sized entry's row under the old arming: %+v", b)
	}
	// Any key the evaluator reads binds the arming (R5): a raised value cap.
	moved := testLiveTrendConstitution()
	moved.Mandate.Strategies[trendFundID].MaxValuePct = new(80.0)
	srv.riskPolicies.active = moved
	if srv.deskRegisterBlocker(armed.RegisterDigest, rows[0]) == nil {
		t.Fatal("R5: a raised value cap kept the arming")
	}
	// A strategy row's sale still goes under a changed register (R6): the
	// edit stops buys, never the mandate's de-risking.
	sale := rows[0]
	sale.Action, sale.PositionEffect = rpc.OrderActionSell, rpc.OrderPositionEffectReduce
	if srv.deskRegisterBlocker(armed.RegisterDigest, sale) != nil {
		t.Fatal("R6: a strategy sale was refused for a register edit")
	}
	srv.riskPolicies.active = c
	if b := srv.deskExecutionRegisterChanged(deskAuthorityRecord{}, rows[0].Key); b == nil {
		t.Fatal("an arming that names no register sent a strategy row")
	}
	if srv.deskExecutionRegisterChanged(deskAuthorityRecord{}, "trailing_stop:none") != nil {
		t.Fatal("a row that is not a strategy row was refused for the register")
	}
}

// The kill rule of a live entry is measured on its own fills at each
// decision close: past the written bound the entry holds before the month
// can be frozen, naming its own fills, and serves no row.
func TestStrategyKillOnOwnFillsHoldsPastTheBound(t *testing.T) {
	c := testLiveTrendConstitution()
	c.Mandate.Strategies[trendFundID].KillDrawdownK = new(0.01)
	pos := mandateTestPositions(100)
	srv, engine := liveTrendServer(t, c, pos, coreDecisionNow)
	// Two kept marks: bought 100 at 100, then marked at 80, a fall of 2,000
	// from the peak, 0.033 K of a 9,200 sleeve budget.
	for _, m := range []liveMark{{Session: "2026-07-31", Close: 100, Units: 100, Cash: -10000, SleeveBudget: 9200}, {Session: "2026-08-31", Close: 80, Units: 100, Cash: -10000, SleeveBudget: 9200}} {
		m.Version, m.Strategy, m.RecordedAt = 1, trendFundID, coreDecisionNow
		appendTestLiveMark(t, srv, m)
	}
	res := liveTrendTarget(t, srv)
	if res.Kill == nil || !res.Kill.Breached || res.Kill.Source != rpc.KillSourceOwnFills || res.Kill.Decisions < 2 {
		t.Fatalf("kill %+v", res.Kill)
	}
	if res.Status != rpc.CoreStatusHold || !slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "kill rule: the drawdown on its own fills is") }) {
		t.Fatalf("status %s reasons %v", res.Status, res.Reasons)
	}
	if _, rows, _ := strategyGeneration(t, srv, engine, pos); len(rows) != 0 {
		t.Fatalf("rows past the kill bound: %+v", rows)
	}
}

func appendTestLiveMark(t *testing.T, srv *Server, m liveMark) {
	t.Helper()
	res := &rpc.StrategyTargetResult{Decision: &rpc.CoreDecision{Decision: m.Session, Close: m.Close, FrozenAt: m.RecordedAt},
		Sizing: &rpc.CoreSizingView{Current: core.Size{SleeveBudget: m.SleeveBudget}}, Instrument: &rpc.CoreInstrument{ConID: trendFundConID}}
	r, _ := srv.resolvedStrategy(srv.riskPolicies.snapshot().policy, trendFundID)
	// The writer reads the entry's own fills: journal them so the kept mark
	// carries the units and cash under test.
	srv.liveMarkFillsForTest = func() strategyFills { return strategyFills{Units: m.Units, Cash: m.Cash, Execs: 1} }
	defer func() { srv.liveMarkFillsForTest = nil }()
	srv.recordLiveMark(t.Context(), res, r, srv.currentBrokerStateScope(), m.RecordedAt)
}

// A held line on Xetra's second segment (IBIS2, the trend fund's EUR line)
// is a German line on every row built from the position: market de, its
// venue kept as the primary exchange, routed SMART; before 2026-10-19 only
// IBIS was, so a reduction of an IBIS2 line quoted on the US calendar.
func TestProposalContractFromAnIBIS2Position(t *testing.T) {
	c := proposalContractFromPosition(rpc.PositionView{Symbol: "SYNX", SecType: rpc.SecTypeStock, ConID: 9041, Exchange: "IBIS2", Currency: "EUR", Quantity: 10, Multiplier: 1}, "STK")
	if c.Market != "de" || c.PrimaryExch != "IBIS2" || c.Exchange != "SMART" || c.Currency != "EUR" || quoteMarketForStockContract(c) != marketcal.MarketDEXetra {
		t.Fatalf("contract %+v", c)
	}
}

const (
	signalISIN  = "ZZ0000000002"
	signalConID = 9050
)

// signalListing is IBKR's synthetic answer for the fund's older share class:
// one GBP line on London.
func signalListing() ibkrlib.Listing {
	return ibkrlib.Listing{ConID: signalConID, Symbol: "SYNS", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "GBP", TradingClass: "SYNS",
		ISIN: signalISIN, ValidExchanges: "SMART,LSEETF", Forms: []string{"STK by ISIN on SMART"}}
}

// signalTrendServer is the live trend fund whose own line holds only its
// last sessions, with the older class written as its signal series, read in
// USD from its GBP line at a GBP.USD rate.
func signalTrendServer(t *testing.T, ownSessions int, withRate bool) *Server {
	t.Helper()
	c := testLiveTrendConstitution()
	e := c.Mandate.Strategies[trendFundID]
	e.SignalISIN, e.SignalCurrency, e.SignalIn = signalISIN, "GBP", "USD"
	pos := mandateTestPositions(100)
	srv := coreTestServer(t, c, pos, 0.25)
	srv.now = func() time.Time { return coreDecisionNow }
	srv.coreListingsForTest = (&byISINListings{listings: []ibkrlib.Listing{coreTestListing(), trendFundListing(), signalListing()}}).lookup
	dates := coreTestDatesOn(t, marketcal.MarketUKLSE)
	own := coreTestSeriesOn(t, trendFundListing(), marketcal.MarketUKLSE, dates, 0.25)
	own.Key = strategySeriesKey(trendFundID, trendFundConID)
	own.Dates, own.Closes = own.Dates[len(own.Dates)-ownSessions:], own.Closes[len(own.Closes)-ownSessions:]
	sig := coreTestSeriesOn(t, signalListing(), marketcal.MarketUKLSE, dates, 0.25)
	sig.Key = strategySignalSeriesKey(trendFundID, signalConID)
	for _, series := range []*mandateHistorySeries{own, sig} {
		if err := srv.saveMandateSeries(t.Context(), series); err != nil {
			t.Fatal(err)
		}
	}
	if withRate {
		// The rate trades every weekday, a London holiday included.
		need := strategyFXNeed(trendFundID, "GBP", "USD")
		fx := &mandateHistorySeries{Version: mandateHistoryDocVer, Key: need.Key, FetchedAt: mandateTestNow.Add(-time.Hour), Source: "test", Contract: need.Contract}
		first, _ := time.Parse(time.DateOnly, dates[0])
		for day := first.AddDate(0, 0, -7); day.Format(time.DateOnly) <= dates[len(dates)-1]; day = day.AddDate(0, 0, 1) {
			if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
				fx.Dates, fx.Closes = append(fx.Dates, day.Format(time.DateOnly)), append(fx.Closes, 1.25)
			}
		}
		if err := srv.saveMandateSeries(t.Context(), fx); err != nil {
			t.Fatal(err)
		}
	}
	srv.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders"))
	srv.tradeProposals = &proposalEngine{server: srv, now: srv.now, queued: &queuedAuthStore{}, store: &proposalStore{}}
	measure(t, srv)
	return srv
}

// The trend is the fund's, not one share class's (strategy-evaluator.md §7,
// 2026-10-19): while the traded class's own closes do not fill the window,
// the month is measured on the older class's series, in the currency it is
// measured in, and sized, limit-checked and marked on the traded line's own
// close; a missing rate holds the entry, named; once the traded class's
// closes fill the window they take over.
func TestStrategyMeasuresOnItsSignalSeriesUntilItsOwnFillsTheWindow(t *testing.T) {
	srv := signalTrendServer(t, 100, true)
	res := liveTrendTarget(t, srv)
	if res.Status != rpc.CoreStatusReady || res.Decision == nil || res.Decision.Decision != "2026-09-30" {
		t.Fatalf("status %s reasons %v decision %+v", res.Status, res.Reasons, res.Decision)
	}
	own := srv.mandateHistorySeries(t.Context(), strategySeriesKey(trendFundID, trendFundConID))
	ownClose, _, _ := strategyLineCloseOn(own, "2026-09-30")
	if !strings.Contains(res.Decision.SignalFrom, "SYNS ("+signalISIN+") in USD") || res.Decision.TradedClose != ownClose || res.Decision.LineClose() != ownClose {
		t.Fatalf("signal from %q, traded close %g (own %g)", res.Decision.SignalFrom, res.Decision.TradedClose, ownClose)
	}
	sigSeries := srv.mandateHistorySeries(t.Context(), strategySignalSeriesKey(trendFundID, signalConID))
	if gbp, _, _ := strategyLineCloseOn(sigSeries, "2026-09-30"); math.Abs(res.Decision.Close-gbp*1.25) > 1e-9 {
		t.Fatalf("the signal close %g is not the GBP close %g in USD", res.Decision.Close, gbp)
	}
	if ev := res.Evidence.Signal; ev == nil || !ev.InUse || ev.OwnSessions >= core.WindowCloses || ev.ConID != signalConID || ev.MeasuredIn != "USD" || ev.FXKey == "" {
		t.Fatalf("signal evidence %+v", res.Evidence.Signal)
	}
	if want := int(math.Floor(res.Sizing.Current.Notional * 25 / 100 / ownClose)); res.Sizing.Current.TargetShares != want {
		t.Fatalf("target %d shares, want %d on the traded close", res.Sizing.Current.TargetShares, want)
	}
	_, rows, _ := strategyGeneration(t, srv, srv.tradeProposals, mandateTestPositions(100))
	if len(rows) != 1 || rows[0].Core.DecisionClose != ownClose {
		t.Fatalf("rows %+v", rows)
	}

	// Held, the young line is measured on the older class's returns, so the
	// book's verdict stays measurable (it needs more closes than the young
	// line has).
	recordEntryFill(t, srv, "synthetic-signal-1", "exec-signal-1", rpc.OrderActionBuy, 50, ownClose, coreDecisionNow.Add(-time.Hour))
	held := withSYNF(mandateTestPositions(100), 50, ownClose)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return held, nil }
	verdict := srv.evaluateMandate(t.Context(), false)
	if verdict.Verdict == nil || verdict.Verdict.Status == mandate.StatusUnknown ||
		!slices.ContainsFunc(verdict.Evidence.Series, func(e rpc.MandateSeriesEvidence) bool {
			return strings.Contains(e.Source, "returns of SYNS, standing in for SYNF")
		}) {
		t.Fatalf("the book with the young line held: %+v reasons %v", verdict.Verdict.Status, verdict.Verdict.Reasons)
	}

	// No rate: the entry holds and names it.
	if res := liveTrendTarget(t, signalTrendServer(t, 100, false)); res.Status != rpc.CoreStatusHold || !slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "no daily GBP.USD rate yet") }) {
		t.Fatalf("without a rate: %s %v", res.Status, res.Reasons)
	}
	// The traded class's own closes fill the window: they take over.
	full := liveTrendTarget(t, signalTrendServer(t, mandateHistoryKeep, true))
	if full.Status != rpc.CoreStatusReady || full.Decision.SignalFrom != "" || full.Decision.TradedClose != 0 || full.Evidence.Signal == nil || full.Evidence.Signal.InUse {
		t.Fatalf("own closes filling the window: %s %v %+v %+v", full.Status, full.Reasons, full.Decision, full.Evidence.Signal)
	}
}

// The signal month never drifts on its rate (refuter round 4, S1): a close
// converts at the previous weekday's rate, so the decision day's own rate,
// which arrives after the US close, changes nothing once the month froze;
// until the previous weekday's rate is in, the month waits for it.
func TestSignalMonthConvertsAtThePreviousSessionsRate(t *testing.T) {
	srv := signalTrendServer(t, 100, true)
	need := strategyFXNeed(trendFundID, "GBP", "USD")
	fx := srv.mandateHistorySeries(t.Context(), need.Key)
	cut := slices.Index(fx.Dates, "2026-09-30")
	if cut < 0 {
		t.Fatal("no rate for the decision day in the fixture")
	}
	// Without the decision day's rate: the month freezes.
	early := *fx
	early.Dates, early.Closes = slices.Clone(fx.Dates[:cut]), slices.Clone(fx.Closes[:cut])
	if err := srv.saveMandateSeries(t.Context(), &early); err != nil {
		t.Fatal(err)
	}
	first := liveTrendTarget(t, srv)
	if first.Status != rpc.CoreStatusReady || first.Decision == nil || first.Decision.FrozenAt.IsZero() {
		t.Fatalf("before the decision day's rate: %s %v", first.Status, first.Reasons)
	}
	// The decision day's rate arrives, at another level: no drift.
	late := *fx
	late.Dates, late.Closes = slices.Clone(fx.Dates), slices.Clone(fx.Closes)
	late.Closes[cut] = 1.40
	if err := srv.saveMandateSeries(t.Context(), &late); err != nil {
		t.Fatal(err)
	}
	if again := liveTrendTarget(t, srv); again.Status != rpc.CoreStatusReady || again.Decision.Drift {
		t.Fatalf("S1: the decision day's rate made the month drift: %s %v", again.Status, again.Reasons)
	}
	// The previous weekday's rate missing: the month waits for it.
	short := *fx
	short.Dates, short.Closes = slices.Clone(fx.Dates[:cut-1]), slices.Clone(fx.Closes[:cut-1])
	srv2 := signalTrendServer(t, 100, true)
	if err := srv2.saveMandateSeries(t.Context(), &short); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv2); res.Status != rpc.CoreStatusHold || !slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "the signal waits for the GBP.USD rate of") }) {
		t.Fatalf("without the previous weekday's rate: %s %v", res.Status, res.Reasons)
	}
}

// A month frozen on the signal series keeps it until the next decision
// (S2): the line's own closes filling the window inside that month do not
// decide it again.
func TestSignalMonthKeepsItsSeriesUntilTheNextDecision(t *testing.T) {
	srv := signalTrendServer(t, 100, true)
	if res := liveTrendTarget(t, srv); res.Decision == nil || res.Decision.SignalFrom == "" || res.Decision.FrozenAt.IsZero() {
		t.Fatalf("first reading %+v %v", res.Decision, res.Reasons)
	}
	full := coreTestSeriesOn(t, trendFundListing(), marketcal.MarketUKLSE, coreTestDatesOn(t, marketcal.MarketUKLSE), 0.25)
	full.Key = strategySeriesKey(trendFundID, trendFundConID)
	if err := srv.saveMandateSeries(t.Context(), full); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv); res.Decision == nil || res.Decision.SignalFrom == "" || res.Decision.Drift || res.Status != rpc.CoreStatusReady {
		t.Fatalf("S2: the month was decided again on the line's own closes: %s %+v %v", res.Status, res.Decision, res.Reasons)
	}
}

// The target is sized on the traded line's close of its last session on or
// before the decision day, never an older one (S3).
func TestSignalMonthWaitsForTheTradedLinesClose(t *testing.T) {
	srv := signalTrendServer(t, 100, true)
	own := srv.mandateHistorySeries(t.Context(), strategySeriesKey(trendFundID, trendFundConID))
	cut := slices.Index(own.Dates, "2026-09-30")
	short := *own
	short.Dates, short.Closes = slices.Clone(own.Dates[:cut]), slices.Clone(own.Closes[:cut])
	if err := srv.saveMandateSeries(t.Context(), &short); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv); res.Status != rpc.CoreStatusHold || res.Decision != nil && res.Decision.TradedClose != 0 ||
		!slices.ContainsFunc(res.Reasons, func(r string) bool { return strings.Contains(r, "the decision waits for SYNF's close of 2026-09-30") }) {
		t.Fatalf("S3: without the decision day's own close: %s %v", res.Status, res.Reasons)
	}
}

// A rate series read before IDEALPRO's 17:00 New York roll of the decision
// day's previous rate holds the month until a later read (refuter round 5,
// F1): the partial bar would be rewritten and the frozen month drift.
func TestSignalMonthWaitsForTheFinalRate(t *testing.T) {
	srv := signalTrendServer(t, 100, true)
	fx := *srv.mandateHistorySeries(t.Context(), strategyFXNeed(trendFundID, "GBP", "USD").Key)
	fx.FetchedAt = time.Date(2026, 9, 29, 20, 30, 0, 0, time.UTC) // 16:30 New York, before the 29th's bar closes
	if err := srv.saveMandateSeries(t.Context(), &fx); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv); res.Status != rpc.CoreStatusHold || !slices.ContainsFunc(res.Reasons, func(r string) bool {
		return strings.Contains(r, "the signal waits for the final GBP.USD rate of 2026-09-29")
	}) {
		t.Fatalf("F1: a partial rate bar: %s %v", res.Status, res.Reasons)
	}
	fx.FetchedAt = time.Date(2026, 9, 29, 21, 30, 0, 0, time.UTC)
	if err := srv.saveMandateSeries(t.Context(), &fx); err != nil {
		t.Fatal(err)
	}
	if res := liveTrendTarget(t, srv); res.Status != rpc.CoreStatusReady {
		t.Fatalf("after the roll: %s %v", res.Status, res.Reasons)
	}
}

// The same through the production placement of a strategy fill beside the
// owner's units on one contract.
func TestStrategyPlacementInventsNoBookDrift(t *testing.T) {
	base := mandateTestPositions(100)
	srv, _ := liveTrendServer(t, testLiveTrendConstitution(), withSYNF(base, 500, 90), coreDecisionNow)
	own := *srv.mandateHistorySeries(t.Context(), strategySeriesKey(trendFundID, trendFundConID))
	own.Key = "u:SYNF"
	if err := srv.saveMandateSeries(t.Context(), &own); err != nil {
		t.Fatal(err)
	}
	recordEntryFill(t, srv, "synthetic-placement", "exec-placement", rpc.OrderActionBuy, 159, 90, coreDecisionNow.Add(-time.Hour))
	pos := withSYNF(base, 659, 90)
	input := withPositions(t, srv, pos)
	if strategyUnitsInBook(input, trendLiveSleeve, trendFundConID) != 159 || strategyOwnerUnitsInBook(input, trendFundConID) != 500 {
		t.Fatal("the fixture did not split the holding")
	}
	if drift := mandateBookDrift(*input, pos, coreDecisionNow); drift != "" {
		t.Fatalf("a fresh verdict invents book drift: %s", drift)
	}
}
