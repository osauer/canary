package edge

import (
	"math"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// replayBook builds a synthetic statement set: one statement of executions,
// instruments, equity and rates, and one Open Positions statement per day.
type replayBook struct {
	t         *testing.T
	base      string
	main      flexstmt.Statement
	snapshots map[string][]flexstmt.OpenPosition
	bars      map[string][]UnderlyingBar
	chain     []RecordedOptionMark
	notes     map[string]ExitReplayHistoryNote
	closed    map[string][]time.Time
	seq       int
}

func newReplayBook(t *testing.T, base string) *replayBook {
	return &replayBook{t: t, base: base, snapshots: map[string][]flexstmt.OpenPosition{}, bars: map[string][]UnderlyingBar{},
		main: flexstmt.Statement{AccountID: "U-SYNTH", WhenGenerated: replayDay("2026-10-31"), Coverage: []flexstmt.SectionCoverage{{Key: "trades", Present: true}}}}
}

func replayDay(day string) time.Time {
	value, err := time.Parse(time.DateOnly, day)
	if err != nil {
		panic(err)
	}
	return value
}

func (b *replayBook) option(conid int64, underlying, right string, strike float64, expiry string) {
	b.main.Instruments = append(b.main.Instruments, flexstmt.Instrument{ConID: conid, Symbol: underlying + " " + expiry + " " + right, UnderlyingSymbol: underlying, AssetClass: "OPT", Currency: "USD", Multiplier: new(100.0), Strike: new(strike), Expiry: expiry, PutCall: right})
}

// trade records one execution of n contracts; a positive n buys. Net cash is
// the premium plus or minus the commission.
func (b *replayBook) trade(conid int64, day string, n, price, commission float64, openClose string, fx float64, realized float64) {
	b.seq++
	side, net := "BUY", -n*price*100-commission
	if n < 0 {
		side, net = "SELL", -n*price*100-commission
	}
	symbol := ""
	for _, inst := range b.main.Instruments {
		if inst.ConID == conid {
			symbol = inst.Symbol
		}
	}
	trade := flexstmt.Trade{
		RecordID: "trade-" + strconv.Itoa(b.seq), AccountID: "U-SYNTH", ConID: conid, Symbol: symbol, AssetClass: "OPT", Currency: "USD",
		TradeID: "T" + strconv.Itoa(b.seq), ExecutionID: "E" + strconv.Itoa(b.seq), ExecutedAt: replayDay(day).Add(15 * time.Hour), ReportDate: replayDay(day),
		Side: side, Quantity: new(math.Abs(n)), Price: new(price), Multiplier: new(100.0), Commission: new(-commission), Taxes: new(0.0),
		OpenClose: openClose, NetCash: new(net), RealizedPNL: new(realized), LevelOfDetail: "EXECUTION",
	}
	if fx > 0 {
		trade.FXRateToBase = new(fx)
	}
	for _, inst := range b.main.Instruments {
		if inst.ConID == conid {
			trade.UnderlyingSymbol = inst.UnderlyingSymbol
		}
	}
	b.main.Trades = append(b.main.Trades, trade)
}

// held records the owner's position on a statement day; an empty call is an
// empty book that day.
func (b *replayBook) held(day string, rows ...flexstmt.OpenPosition) {
	if _, ok := b.snapshots[day]; !ok {
		b.snapshots[day] = []flexstmt.OpenPosition{}
	}
	for _, row := range rows {
		row.ReportDate, row.AccountID, row.AssetClass, row.Currency, row.LevelOfDetail = replayDay(day), "U-SYNTH", "OPT", "USD", "SUMMARY"
		row.Multiplier = new(100.0)
		b.snapshots[day] = append(b.snapshots[day], row)
	}
}

func position(conid int64, quantity, mark, costBasis float64) flexstmt.OpenPosition {
	return flexstmt.OpenPosition{ConID: conid, Quantity: new(quantity), MarkPrice: new(mark), CostBasisMoney: new(costBasis)}
}

func (b *replayBook) nlv(day string, total float64) {
	b.main.Equity = append(b.main.Equity, flexstmt.EquityRow{ReportDate: replayDay(day), TotalBase: total})
}

func (b *replayBook) rate(day string, usdToBase float64) {
	b.main.FXRates = append(b.main.FXRates, flexstmt.FXRate{RecordID: "fx-" + day, Date: replayDay(day), FromCurrency: "USD", ToCurrency: b.base, Rate: new(usdToBase)})
}

func (b *replayBook) quote(conid int64, day string, bid, ask float64) {
	b.chain = append(b.chain, RecordedOptionMark{ConID: conid, Day: replayDay(day), Bid: bid, Ask: ask})
}

// steadyBars writes weekday closes for an underlying from July to December:
// close fixed unless set, high and low one point either side.
func (b *replayBook) steadyBars(symbol string, close float64, set map[string]float64) {
	for day := replayDay("2026-07-01"); day.Before(replayDay("2026-12-31")); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		value := close
		if v, ok := set[dayKey(day)]; ok {
			value = v
		}
		b.bars[symbol] = append(b.bars[symbol], UnderlyingBar{Day: day, Close: value, High: value + 1, Low: value - 1})
	}
}

func (b *replayBook) statements() []flexstmt.Statement {
	out := []flexstmt.Statement{b.main}
	for day, rows := range b.snapshots {
		out = append(out, flexstmt.Statement{AccountID: "U-SYNTH", FromDate: replayDay(day), ToDate: replayDay(day), WhenGenerated: replayDay(day).Add(30 * time.Hour),
			Coverage: []flexstmt.SectionCoverage{{Key: "open_positions", Present: true, RowCount: len(rows)}}, Positions: rows})
	}
	return out
}

func replayTestPolicy() ExitReplayPolicy {
	return ExitReplayPolicy{
		Exit: risk.OptionExitPolicy{MinDTE: 14, LossExitPct: 60, ProfitArmGainPct: 50, ProfitTrailPct: 30, LockedGainPct: 5, MinTrailPct: 20, MaxTrailPct: 50,
			MaxSpreadPctOfMid: 25, MinTrailAbs: 0.10, SpreadMultiple: 2, ExpiryCloseDTE: 7},
		RecoverCostGainPct: 100, OptionNameActPct: 15, StockTrailRangeMultiple: 2.5, HedgeSymbols: []string{"SPY", "SPX", "SPXW", "QQQ", "IWM"}, Source: "synthetic",
	}
}

func (b *replayBook) run(rules ...string) rpc.EdgeReplayResult {
	b.t.Helper()
	params, err := rpc.NormalizeEdgeReplayParams(rpc.EdgeReplayParams{Rules: rules})
	if err != nil {
		b.t.Fatal(err)
	}
	res := ExitReplay(ExitReplayInput{Statements: b.statements(), BaseCurrency: b.base, Rules: params.Rules, Policy: replayTestPolicy(), Underlyings: b.bars, HistoryNotes: b.notes, ClosedSessions: b.closed, ChainMarks: b.chain})
	if res.State != rpc.EdgeStateCurrent {
		b.t.Fatalf("replay state %s (%s)", res.State, res.Reason)
	}
	if err := rpc.ValidateEdgeReplayResult(res); err != nil {
		b.t.Fatal(err)
	}
	return res
}

func replayRun(t *testing.T, res rpc.EdgeReplayResult, id, prices string) rpc.EdgeReplayRun {
	t.Helper()
	for _, run := range res.Runs {
		if run.ID == id && run.Prices == prices {
			return run
		}
	}
	t.Fatalf("no %s run %s", prices, id)
	return rpc.EdgeReplayRun{}
}

func replayReason(run rpc.EdgeReplayRun, reason string) rpc.EdgeReplayReason {
	for _, row := range run.ExitsByReason {
		if row.Reason == reason {
			return row
		}
	}
	return rpc.EdgeReplayReason{}
}

func near(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s = %.6f, want %.6f", label, got, want)
	}
}

func TestExitReplayLossExitAtTheRulebookLine(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9001, "SYNX", "C", 100, "20261016")
	b.steadyBars("SYNX", 95, nil)
	b.held("2026-09-01")
	b.trade(9001, "2026-09-02", 2, 2.00, 1, "O", 0, 0)
	b.held("2026-09-02", position(9001, 2, 2.00, 401))
	b.held("2026-09-03", position(9001, 2, 1.50, 401))
	b.held("2026-09-04", position(9001, 2, 0.70, 401)) // -65% of 2.005 per share
	b.held("2026-09-07", position(9001, 2, 0.60, 401))
	b.trade(9001, "2026-09-08", -2, 0.50, 1, "C", 0, -302)
	b.held("2026-09-08")
	res := b.run("loss_exit")

	actual := replayRun(t, res, rpc.EdgeReplaySetActual, rpc.EdgeReplayPricesActual)
	near(t, "actual P/L", actual.PNLBase, 99-401)
	run := replayRun(t, res, rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesRecorded)
	// Two exchange executions, two contracts each, one dollar each: 0.50 a contract.
	near(t, "commission per contract", *res.Parameters.CommissionPerContractBase, 0.5)
	near(t, "loss-exit P/L", run.PNLBase, 2*(70-0.5)-401)
	if got := replayReason(run, rpc.EdgeReplayRuleLossExit); got.Events != 1 || got.Units != 2 {
		t.Fatalf("loss exits = %+v", got)
	}
	if run.VsOwner.Earlier != 2 || run.ContractsVsOwner.Earlier != 1 {
		t.Fatalf("timing = %+v / %+v, want both contracts earlier", run.VsOwner, run.ContractsVsOwner)
	}
	near(t, "difference", run.DiffVsActualBase, run.PNLBase-actual.PNLBase)
}

func TestExitReplayExpiryCloseInTheMoneyInsideTheRunway(t *testing.T) {
	for _, tc := range []struct {
		name   string
		strike float64
		exits  int
	}{{"in the money", 90, 1}, {"out of the money", 100, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			b := newReplayBook(t, "USD")
			b.option(9002, "SYNX", "C", tc.strike, "20260918")
			b.steadyBars("SYNX", 95, nil)
			b.held("2026-09-01")
			b.trade(9002, "2026-09-02", 1, 5.00, 1, "O", 0, 0)
			for _, day := range []string{"2026-09-02", "2026-09-03", "2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11", "2026-09-14", "2026-09-15"} {
				b.held(day, position(9002, 1, 5.20, 501))
			}
			b.trade(9002, "2026-09-16", -1, 6.00, 1, "C", 0, 98)
			b.held("2026-09-16")
			run := replayRun(t, b.run("expiry_close"), rpc.EdgeReplayRuleExpiryClose, rpc.EdgeReplayPricesRecorded)
			got := replayReason(run, rpc.EdgeReplayRuleExpiryClose)
			if got.Events != tc.exits {
				t.Fatalf("expiry closes = %+v, want %d", got, tc.exits)
			}
			if tc.exits == 1 {
				// 2026-09-10 is eight days out, 2026-09-11 seven: the close
				// lands on the runway's first day at 5.20 less a dollar.
				near(t, "expiry-close P/L", run.PNLBase, 520-1-501)
			}
		})
	}
}

// trailBook holds one call that rises to a high water of 2.00 and falls to
// 1.35, below the 30% trail's stop of 1.40; with an add the owner buys a
// second contract at 1.50 on the way down.
func trailBook(t *testing.T, add bool) *replayBook {
	b := newReplayBook(t, "USD")
	b.option(9003, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.held("2026-09-01")
	b.trade(9003, "2026-09-02", 1, 1.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9003, 1, 1.00, 100))
	b.held("2026-09-03", position(9003, 1, 1.60, 100))
	b.held("2026-09-04", position(9003, 1, 2.00, 100))
	qty, cost := 1.0, 100.0
	if add {
		b.trade(9003, "2026-09-07", 1, 1.50, 0, "O", 0, 0)
		qty, cost = 2, 250
	}
	b.held("2026-09-07", position(9003, qty, 1.50, cost))
	b.held("2026-09-08", position(9003, qty, 1.35, cost))
	b.held("2026-09-09", position(9003, qty, 1.30, cost))
	b.trade(9003, "2026-09-10", -qty, 1.20, 0, "C", 0, 0)
	b.held("2026-09-10")
	return b
}

func TestExitReplayProfitTrailFromTheCarriedHighWater(t *testing.T) {
	run := replayRun(t, trailBook(t, false).run("profit_trail"), rpc.EdgeReplayRuleProfitTrail, rpc.EdgeReplayPricesRecorded)
	if got := replayReason(run, rpc.EdgeReplayRuleProfitTrail); got.Events != 1 || got.Units != 1 {
		t.Fatalf("profit trail exits = %+v", got)
	}
	near(t, "profit-trail P/L", run.PNLBase, 135-100)

	// The same state through the daemon's evaluator: armed at 1.60, high
	// water 2.00, and 1.35 is through the stop of 1.40.
	pol := replayTestPolicy().Exit
	decision := risk.EvaluateOptionExit(risk.OptionExitInput{ConID: 1, Quantity: 1, Multiplier: 100, AvgCost: 100, Bid: 1.35, Ask: 1.35, DTE: 73,
		DirectionalIntent: true, Standalone: true, EconomicRoleAllowed: true, QuoteLive: true, QuoteFresh: true, SessionOpen: true, Right: "C", Strike: 100, Underlying: 105,
		CarriedHighWater: risk.OptionExitHighWater{PerShare: 2, CostPremium: 1, Quantity: 1}}, pol)
	if decision.Action != risk.OptionExitActionProfitTake {
		t.Fatalf("evaluator action %q, want the take the replay made", decision.Action)
	}
}

func TestExitReplayProfitTrailHighWaterResetsOnAdd(t *testing.T) {
	run := replayRun(t, trailBook(t, true).run("profit_trail"), rpc.EdgeReplayRuleProfitTrail, rpc.EdgeReplayPricesRecorded)
	if len(run.ExitsByReason) != 0 {
		t.Fatalf("an add changed cost basis and quantity, so the carried high water must reset; exits = %+v", run.ExitsByReason)
	}
}

func TestExitReplayRecoverCostSellsTheCostAndLetsTheRestRun(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9004, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.held("2026-09-01")
	b.trade(9004, "2026-09-02", 10, 1.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9004, 10, 1.00, 1000))
	b.held("2026-09-03", position(9004, 10, 1.50, 1000))
	b.held("2026-09-04", position(9004, 10, 2.10, 1000)) // worth 2,100 on a cost of 1,000
	b.held("2026-09-07", position(9004, 10, 2.50, 1000))
	b.held("2026-09-08", position(9004, 10, 2.40, 1000))
	run := replayRun(t, b.run("recover_cost"), rpc.EdgeReplayRuleRecoverCost, rpc.EdgeReplayPricesRecorded)
	if got := replayReason(run, rpc.EdgeReplayRuleRecoverCost); got.Events != 1 || got.Units != 5 {
		t.Fatalf("recover-cost exits = %+v, want once, ceil(1000/210) = 5 contracts", got)
	}
	near(t, "recover-cost P/L", run.PNLBase, 5*210+5*240-1000)
}

func TestExitReplayTrimsTheNameBackToItsCap(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9005, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.held("2026-09-01")
	b.trade(9005, "2026-09-02", 10, 1.00, 0, "O", 0, 0)
	for _, day := range []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04"} {
		b.nlv(day, 10000)
	}
	b.held("2026-09-02", position(9005, 10, 1.00, 1000))
	b.held("2026-09-03", position(9005, 10, 2.00, 1000)) // 2,000 against a 1,500 cap
	b.held("2026-09-04", position(9005, 10, 2.00, 1000))
	run := replayRun(t, b.run("trim_to_cap"), rpc.EdgeReplayRuleTrimToCap, rpc.EdgeReplayPricesRecorded)
	if got := replayReason(run, rpc.EdgeReplayRuleTrimToCap); got.Events != 1 || got.Units != 3 {
		t.Fatalf("trims = %+v, want one event of three contracts (7 x 200 = 1,400)", got)
	}
	near(t, "trim P/L", run.PNLBase, 3*200+7*200-1000)
	if run.PeakPctNLV == nil || *run.PeakPctNLV > 15+1e-9 {
		t.Fatalf("peak share of NLV = %v, want at most the cap", run.PeakPctNLV)
	}
}

func TestExitReplayStockTrailUsesTheRangeOfTheStock(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9006, "SYNX", "C", 100, "20261218")
	// Steady at 100 with a two-point range; the stock peaks at 110 on
	// 2026-09-04, closes 4 below on 2026-09-07 (inside 2.5 ranges) and 7
	// below on 2026-09-08 (outside).
	b.steadyBars("SYNX", 100, map[string]float64{"2026-09-02": 102, "2026-09-03": 106, "2026-09-04": 110, "2026-09-07": 106, "2026-09-08": 103, "2026-09-09": 103})
	b.held("2026-09-01")
	b.trade(9006, "2026-09-02", 1, 3.00, 0, "O", 0, 0)
	for _, day := range []string{"2026-09-02", "2026-09-03", "2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09"} {
		b.held(day, position(9006, 1, 3.00, 300))
	}
	run := replayRun(t, b.run("stock_trail"), rpc.EdgeReplayRuleStockTrail, rpc.EdgeReplayPricesRecorded)
	got := replayReason(run, rpc.EdgeReplayRuleStockTrail)
	if got.Events != 1 {
		t.Fatalf("stock-trail exits = %+v", got)
	}
	r := &replay{in: ExitReplayInput{Underlyings: b.bars}}
	r.buildBars()
	atr7, _, _ := r.atr("SYNX", "2026-09-07")
	atr8, _, _ := r.atr("SYNX", "2026-09-08")
	if 110-106 >= 2.5*atr7 || 110-103 <= 2.5*atr8 {
		t.Fatalf("fixture ranges moved: ATR %.3f and %.3f", atr7, atr8)
	}
	if run.VsOwner.Earlier != 1 {
		t.Fatalf("timing = %+v; the owner still holds, the trail sold on 2026-09-08", run.VsOwner)
	}
}

func TestExitReplayUntestableDayKeepsTheOwnersOutcome(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9007, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 98, nil)
	b.held("2026-09-01")
	b.trade(9007, "2026-09-02", 1, 1.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9007, 1, 1.00, 100))
	b.nlv("2026-09-03", 10000) // a statement day without a mark for the contract
	b.held("2026-09-04", position(9007, 1, 0.30, 100))
	b.trade(9007, "2026-09-07", -1, 0.35, 0, "C", 0, -65)
	b.held("2026-09-07")
	res := b.run("loss_exit")

	recorded := replayRun(t, res, rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesRecorded)
	if recorded.Coverage.UntestableEvents != 1 || recorded.Coverage.UnitsKeptActual != 1 || recorded.VsOwner.Untestable != 1 || len(recorded.ExitsByReason) != 0 {
		t.Fatalf("recorded run = %+v", recorded)
	}
	near(t, "recorded run keeps the owner's P/L", recorded.PNLBase, 35-100)

	modelled := replayRun(t, res, rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesModelled)
	if modelled.Coverage.ModelledDays != 1 || modelled.Coverage.UntestableEvents != 0 {
		t.Fatalf("modelled coverage = %+v", modelled.Coverage)
	}
	if got := replayReason(modelled, rpc.EdgeReplayRuleLossExit); got.Events != 1 {
		t.Fatalf("modelled run exits = %+v, want the loss exit at the 2026-09-04 mark", modelled.ExitsByReason)
	}
	near(t, "modelled loss exit", modelled.PNLBase, 30-100)
}

// holiday makes day a closed session of the underlying's exchange: no bar,
// and listed as closed.
func (b *replayBook) holiday(symbol, day string) {
	b.bars[symbol] = slices.DeleteFunc(b.bars[symbol], func(bar UnderlyingBar) bool { return dayKey(bar.Day) == day })
	if b.closed == nil {
		b.closed = map[string][]time.Time{}
	}
	b.closed[symbol] = append(b.closed[symbol], replayDay(day))
}

// A closed session carries the previous mark: trim cannot sell on it, and a
// later gap is modelled off a mark recorded on the holiday at the last close
// before it.
func TestExitReplayClosedSessionCarriesAndActsOnNoRule(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9012, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 98, nil)
	b.holiday("SYNX", "2026-09-07")
	b.held("2026-09-01")
	b.nlv("2026-09-01", 10000)
	b.trade(9012, "2026-09-02", 4, 1.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9012, 4, 1.00, 400))
	b.nlv("2026-09-02", 10000)
	b.held("2026-09-04", position(9012, 4, 1.00, 400))
	b.nlv("2026-09-04", 10000)
	// The holiday's statement repeats the mark; the name cap (15% of NLV)
	// would trim if a rule could act on the day.
	b.held("2026-09-07", position(9012, 4, 1.00, 400))
	b.nlv("2026-09-07", 1000)
	b.nlv("2026-09-08", 10000) // no mark: modelled off the holiday's mark
	b.held("2026-09-09", position(9012, 4, 0.30, 400))
	b.nlv("2026-09-09", 10000)
	b.trade(9012, "2026-09-10", -4, 0.35, 0, "C", 0, -260)
	b.held("2026-09-10")
	res := b.run("trim_to_cap", "loss_exit")

	trim := replayRun(t, res, rpc.EdgeReplayRuleTrimToCap, rpc.EdgeReplayPricesRecorded)
	if trim.Exits != 0 || trim.Coverage.ClosedSessionDays != 1 {
		t.Fatalf("trim on a closed session = %+v", trim)
	}
	modelled := replayRun(t, res, rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesModelled)
	if modelled.Coverage.ClosedSessionDays != 1 || modelled.Coverage.ModelledDays != 1 || modelled.Coverage.UntestableEvents != 0 {
		t.Fatalf("modelled coverage = %+v", modelled.Coverage)
	}
}

func TestExitReplayReconcilesWithEdgeAndExcludesHedgesAndShorts(t *testing.T) {
	b := newReplayBook(t, "EUR")
	b.option(9008, "SYNX", "C", 100, "20261120")
	b.option(9009, "SYNY", "C", 50, "20261120")
	b.option(9010, "IWM", "P", 200, "20261120")
	b.option(9011, "SYNZ", "C", 20, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.steadyBars("SYNY", 55, nil)
	b.rate("2026-09-01", 0.90)
	b.rate("2026-09-08", 0.80)
	b.held("2026-09-01", position(9009, 1, 4.00, 350))
	// A flat-to-flat cycle inside the window, bought at 0.90 and sold at 0.80.
	b.trade(9008, "2026-09-02", 2, 1.00, 1, "O", 0.90, 0)
	b.trade(9008, "2026-09-08", -2, 1.50, 1, "C", 0.80, 98)
	// A hedge put and a short call are excluded.
	b.trade(9010, "2026-09-02", 1, 3.00, 1, "O", 0.90, 0)
	b.trade(9011, "2026-09-03", -1, 0.50, 1, "O", 0.90, 0)
	b.held("2026-09-02", position(9008, 2, 1.00, 201), position(9009, 1, 4.10, 350), position(9010, 1, 3.00, 301))
	b.held("2026-09-03", position(9008, 2, 1.20, 201), position(9009, 1, 4.20, 350), position(9010, 1, 3.00, 301), position(9011, -1, 0.50, -49))
	b.held("2026-09-08", position(9009, 1, 4.50, 350), position(9010, 1, 3.00, 301), position(9011, -1, 0.50, -49))
	res := b.run("B")

	if res.Scope.Contracts != 2 || res.Scope.ExcludedHedgeContracts != 1 || res.Scope.ExcludedHedgeExecutions != 1 || res.Scope.ExcludedShortContracts != 1 || res.Scope.PreheldLots != 1 {
		t.Fatalf("scope = %+v", res.Scope)
	}
	rec := res.Reconciliation
	if !rec.Reconciled || rec.EdgeCycles != 1 || rec.ComparedContracts != 1 {
		t.Fatalf("reconciliation = %+v", rec)
	}
	near(t, "Edge cycle P/L", rec.EdgeCyclesPNLBase, 98*0.80)
	near(t, "actual on the cycle", rec.ActualOnCyclesBase, -201*0.90+299*0.80)
	near(t, "FX translation", rec.FXTranslationBase, -201*0.90+(299-98)*0.80)
	near(t, "residual", rec.ResidualBase, 0)
	// The pre-held contract starts at its start mark and ends at its end
	// mark, both at the day's rate.
	near(t, "start value", rec.StartValueBase, 400*0.90)
	near(t, "end value", rec.EndValueBase, 450*0.80)
	near(t, "cash = units", rec.ActualCashBase, rec.ActualUnitsBase)
	actual := replayRun(t, res, rpc.EdgeReplaySetActual, rpc.EdgeReplayPricesActual)
	near(t, "actual P/L", actual.PNLBase, -201*0.90+299*0.80+450*0.80-400*0.90)
}

// exercise books the last execution as the broker's exercise of the option.
func (b *replayBook) exercise(conid int64, day string) {
	last := b.main.Trades[len(b.main.Trades)-1]
	b.main.OptionEvents = append(b.main.OptionEvents, flexstmt.OptionEvent{RecordID: "eae-" + last.TradeID, AccountID: "U-SYNTH", ConID: conid, AssetClass: "OPT", Currency: "USD",
		Date: replayDay(day), TransactionType: "EXERCISE", Quantity: last.Quantity, RealizedPNL: new(0.0), TradeID: last.TradeID})
}

// An exercised option closes with the broker's realized P/L on the option
// leg, not with the cash flows, since its value moves into the stock: the
// contract is named as not compared, and the comparison balances on the
// rest instead of reading as a failure.
func TestExitReplayReconciliationNamesWhatItCouldNotCompare(t *testing.T) {
	b := newReplayBook(t, "EUR")
	b.option(9013, "SYNX", "C", 100, "20261120")
	b.option(9014, "SYNY", "C", 50, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.steadyBars("SYNY", 55, nil)
	b.rate("2026-09-01", 0.90)
	b.rate("2026-09-08", 0.80)
	b.held("2026-09-01")
	b.trade(9013, "2026-09-02", 2, 1.00, 1, "O", 0.90, 0)
	b.trade(9014, "2026-09-02", 1, 2.00, 1, "O", 0.90, 0)
	b.held("2026-09-02", position(9013, 2, 1.00, 201), position(9014, 1, 2.00, 201))
	b.held("2026-09-03", position(9013, 2, 5.00, 201), position(9014, 1, 2.50, 201))
	// The exercise is a booked sale at zero; the broker moves the premium
	// into the stock's basis and books no realized P/L on the option.
	b.trade(9013, "2026-09-08", -2, 0, 0, "C", 0.80, 0)
	b.exercise(9013, "2026-09-08")
	b.trade(9014, "2026-09-08", -1, 2.50, 1, "C", 0.80, 48)
	b.held("2026-09-08")
	res := b.run("B")

	rec := res.Reconciliation
	if !rec.Reconciled || rec.EdgeCycleContracts != 2 || rec.ComparedContracts != 1 {
		t.Fatalf("reconciliation = %+v; the exercised contract must be named, not fail the rest", rec)
	}
	near(t, "residual", rec.ResidualBase, 0)
	near(t, "cash against broker realized", rec.CycleCashResidual, 0)
	if rec.Status != rpc.EdgeReplayReconciledPartly || len(rec.NotCompared) != 1 {
		t.Fatalf("status %q not compared %+v", rec.Status, rec.NotCompared)
	}
	if got := rec.NotCompared[0]; got.Underlying != "SYNX" || got.Contract != "SYNX 2026-11-20 100 C" || got.Reason != rpc.EdgeReplayNotComparedExercised {
		t.Fatalf("not compared = %+v", got)
	}
}

// A residual beyond tolerance on a contract the comparison covers is the one
// failure: the status says so and names the contract.
func TestExitReplayReconciliationFailsOnAResidual(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9015, "SYNX", "C", 100, "20261120")
	b.steadyBars("SYNX", 105, nil)
	b.held("2026-09-01")
	b.trade(9015, "2026-09-02", 1, 1.00, 1, "O", 0, 0)
	b.held("2026-09-02", position(9015, 1, 1.00, 101))
	// Sold for 149 net against a cost of 101; the broker books 30, not 48.
	b.trade(9015, "2026-09-08", -1, 1.50, 1, "C", 0, 30)
	b.held("2026-09-08")
	res := b.run("B")

	rec := res.Reconciliation
	if rec.Reconciled || rec.Status != rpc.EdgeReplayUnreconciled || rec.ComparedContracts != 1 {
		t.Fatalf("reconciliation = %+v; an 18 residual must fail", rec)
	}
	if len(rec.ResidualContracts) != 1 || rec.ResidualContracts[0].Contract != "SYNX 2026-11-20 100 C" {
		t.Fatalf("residual contracts = %+v", rec.ResidualContracts)
	}
}

func TestExitReplayNeedsAStartSnapshot(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9001, "SYNX", "C", 100, "20261016")
	b.held("2026-09-02")
	res := ExitReplay(ExitReplayInput{Statements: b.statements(), BaseCurrency: "USD", Rules: []string{"A"}, Policy: replayTestPolicy()})
	if res.State != rpc.EdgeStateInsufficient || res.Reason != "statement_history_too_short" || !res.NotExecution {
		t.Fatalf("result = %+v", res)
	}
}

func TestExitReplayChainQuotesCarryTheRunToExpirySettlement(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9012, "SYNX", "C", 90, "20260911")
	b.steadyBars("SYNX", 95, map[string]float64{"2026-09-11": 97})
	b.held("2026-09-01")
	b.trade(9012, "2026-09-02", 1, 5.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9012, 1, 5.00, 500))
	b.held("2026-09-03", position(9012, 1, 5.10, 500))
	b.trade(9012, "2026-09-04", -1, 5.20, 0, "C", 0, 20)
	for _, day := range []string{"2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11"} {
		b.held(day)
	}
	// The owner sold on 2026-09-04; recorded chain quotes mark the contract
	// through 2026-09-10, and on expiry day it settles at the close.
	for _, day := range []string{"2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09", "2026-09-10"} {
		b.quote(9012, day, 5.00, 5.20)
	}
	run := replayRun(t, b.run("loss_exit"), rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesRecorded)
	if run.Coverage.StatementDays != 2 || run.Coverage.ChainMidDays != 5 || run.Coverage.UntestableEvents != 0 {
		t.Fatalf("coverage = %+v, want two statement marks and the five quoted sessions from the sale on", run.Coverage)
	}
	near(t, "settled at intrinsic", run.PNLBase, (97-90)*100-500)
	if run.VsOwner.Later != 1 {
		t.Fatalf("timing = %+v, want later than the owner", run.VsOwner)
	}
}

// A name whose underlying history is missing cannot be tested. It must leave
// every run, named, instead of keeping the owner's outcome unannounced.
func TestExitReplayLeavesOutANameWithoutHistory(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9101, "SYNA", "C", 100, "20261016")
	b.option(9102, "SYNB", "C", 100, "20261016")
	b.steadyBars("SYNA", 95, nil) // SYNB has no history at all
	b.held("2026-09-01")
	b.trade(9101, "2026-09-02", 1, 2.00, 0, "O", 0, 0)
	b.trade(9102, "2026-09-02", 1, 2.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9101, 1, 2.00, 200), position(9102, 1, 2.00, 200))
	b.held("2026-09-03", position(9101, 1, 0.70, 200), position(9102, 1, 0.70, 200)) // both past the -60% line
	b.trade(9101, "2026-09-08", -1, 1.50, 0, "C", 0, -50)
	b.trade(9102, "2026-09-08", -1, 1.50, 0, "C", 0, -50)
	b.held("2026-09-08")
	b.notes = map[string]ExitReplayHistoryNote{"SYNB": {Reason: rpc.EdgeReplayHistoryGatewayOffline, Detail: "no retained history"}}
	res := b.run("loss_exit")

	if !res.Incomplete || len(res.MissingHistory) != 1 {
		t.Fatalf("incomplete = %v, missing = %+v; SYNB must be named", res.Incomplete, res.MissingHistory)
	}
	gap := res.MissingHistory[0]
	if gap.Underlying != "SYNB" || gap.Reason != rpc.EdgeReplayHistoryGatewayOffline || gap.Detail != "no retained history" || gap.Contracts != 1 ||
		dayKey(gap.NeededFrom) != "2026-09-02" || dayKey(gap.NeededTo) != "2026-09-08" || !gap.HistoryFrom.IsZero() {
		t.Fatalf("missing history = %+v", gap)
	}
	near(t, "owner's P/L on SYNB, left out", gap.ActualPNLBase, 150-200)
	near(t, "actual P/L on SYNA alone", replayRun(t, res, rpc.EdgeReplaySetActual, rpc.EdgeReplayPricesActual).PNLBase, 150-200)
	if got := res.Scope.Underlyings; len(got) != 1 || got[0] != "SYNA" {
		t.Fatalf("scope underlyings = %v; SYNB has no history and must leave the runs", got)
	}
	run := replayRun(t, res, rpc.EdgeReplayRuleLossExit, rpc.EdgeReplayPricesRecorded)
	if run.VsOwner.Untestable != 0 || run.Coverage.UnitsKeptActual != 0 {
		t.Fatalf("loss-exit run kept the owner's outcome silently: %+v / %+v", run.VsOwner, run.Coverage)
	}
	near(t, "loss-exit P/L on SYNA alone", run.PNLBase, 70-200)
}

// Some names carry closes without a high and low. The stock trail must then
// measure those days by the close-only true range, not go blind.
func TestExitReplayStockTrailMeasuresCloseOnlyDays(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9103, "SYNX", "C", 100, "20261218")
	// Closes alternate 99 and 101 (a close-to-close range of two), then the
	// stock peaks at 110 on 2026-09-04, closes 4 below on 2026-09-07 and
	// 8 below on 2026-09-08; no bar carries a high or a low.
	set := map[string]float64{"2026-09-02": 102, "2026-09-03": 106, "2026-09-04": 110, "2026-09-07": 106, "2026-09-08": 102, "2026-09-09": 102}
	i := 0
	for day := replayDay("2026-07-01"); day.Before(replayDay("2026-12-31")); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		value := 99.0 + 2*float64(i%2)
		i++
		if v, ok := set[dayKey(day)]; ok {
			value = v
		}
		b.bars["SYNX"] = append(b.bars["SYNX"], UnderlyingBar{Day: day, Close: value})
	}
	b.held("2026-09-01")
	b.trade(9103, "2026-09-02", 1, 3.00, 0, "O", 0, 0)
	for _, day := range []string{"2026-09-02", "2026-09-03", "2026-09-04", "2026-09-07", "2026-09-08", "2026-09-09"} {
		b.held(day, position(9103, 1, 3.00, 300))
	}
	run := replayRun(t, b.run("stock_trail"), rpc.EdgeReplayRuleStockTrail, rpc.EdgeReplayPricesRecorded)
	if got := replayReason(run, rpc.EdgeReplayRuleStockTrail); got.Events != 1 {
		t.Fatalf("stock-trail exits = %+v; closes without a range must still measure the trail", run.ExitsByReason)
	}
	// Five evaluated days, 2026-09-02 to 09-08, each over 20 sessions: 24
	// distinct bars, every one without a range.
	if run.Coverage.CloseOnlyRangeDays != 24 || run.Coverage.TrailUnmeasuredDays != 0 {
		t.Fatalf("coverage = %+v, want 24 close-only ranges and no unmeasured day", run.Coverage)
	}
	r := &replay{in: ExitReplayInput{Underlyings: b.bars}}
	r.buildBars()
	atr7, _, ok7 := r.atr("SYNX", "2026-09-07")
	atr8, _, ok8 := r.atr("SYNX", "2026-09-08")
	if !ok7 || !ok8 || 110-106 >= 2.5*atr7 || 110-102 <= 2.5*atr8 {
		t.Fatalf("fixture ranges moved: ATR %.3f (%v) and %.3f (%v)", atr7, ok7, atr8, ok8)
	}
}

// A trail without 21 sessions of history cannot measure its range; the run
// counts those days instead of leaving the trail silently off.
func TestExitReplayStockTrailCountsUnmeasuredDays(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9104, "SYNX", "C", 100, "20261218")
	for _, day := range []string{"2026-08-27", "2026-08-28", "2026-08-31", "2026-09-01", "2026-09-02", "2026-09-03"} {
		b.bars["SYNX"] = append(b.bars["SYNX"], UnderlyingBar{Day: replayDay(day), Close: 100, High: 101, Low: 99})
	}
	b.held("2026-09-01")
	b.trade(9104, "2026-09-02", 1, 3.00, 0, "O", 0, 0)
	b.held("2026-09-02", position(9104, 1, 3.00, 300))
	b.held("2026-09-03", position(9104, 1, 3.00, 300))
	run := replayRun(t, b.run("stock_trail"), rpc.EdgeReplayRuleStockTrail, rpc.EdgeReplayPricesRecorded)
	if run.Coverage.TrailUnmeasuredDays != 2 || run.Exits != 0 {
		t.Fatalf("coverage = %+v, exits %d; both held days lack 21 sessions", run.Coverage, run.Exits)
	}
}

// The daily history a caller reads spans each underlying's first option
// record through its last expiry or the latest statement, whichever is
// earlier.
func TestExitReplayUnderlyingsNameTheDaysNeeded(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9105, "SYNA", "C", 100, "20261016")
	b.option(9106, "SYNB", "C", 100, "20260904")
	b.held("2026-09-01")
	b.trade(9105, "2026-09-02", 1, 2.00, 0, "O", 0, 0)
	b.trade(9106, "2026-09-03", 1, 2.00, 0, "O", 0, 0)
	b.held("2026-09-08", position(9105, 1, 2.00, 200))
	got := ExitReplayUnderlyings(b.statements(), time.Time{}, time.Time{}, nil)
	if len(got) != 2 || got[0].Symbol != "SYNA" || dayKey(got[0].First) != "2026-09-02" || dayKey(got[0].Last) != "2026-09-08" ||
		got[1].Symbol != "SYNB" || dayKey(got[1].First) != "2026-09-03" || dayKey(got[1].Last) != "2026-09-04" {
		t.Fatalf("underlyings = %+v", got)
	}
}
