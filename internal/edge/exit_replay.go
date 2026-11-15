package edge

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// ExitReplayATRSessions is the stock trail's range: the average true range of
// the last 20 sessions, as the protection policy's stock_trail_range_multiple
// defines it (long-option-exits LO-H4).
const ExitReplayATRSessions = 20

// UnderlyingBar is one daily TRADES bar of an option's underlying, labelled
// with its session date. High and Low are zero when the history carried none.
type UnderlyingBar struct {
	Day   time.Time
	Close float64
	High  float64
	Low   float64
}

// RecordedOptionMark is one recorded option-chain quote of an exact contract
// on a session day. ConID is zero when the line carried none; the contract
// then matches on underlying, expiry (YYYY-MM-DD), strike and right (C or P).
type RecordedOptionMark struct {
	ConID      int64
	Underlying string
	Expiry     string
	Strike     float64
	Right      string
	Day        time.Time
	Bid        float64
	Ask        float64
}

// ExitReplayPolicy carries the rule numbers in force. Exit is the policy
// risk.EvaluateOptionExit runs with: the Rulebook's loss line and expiry act
// level and the protection policy's option profit trail.
type ExitReplayPolicy struct {
	Exit                    risk.OptionExitPolicy
	RecoverCostGainPct      float64
	OptionNameActPct        float64
	StockTrailRangeMultiple float64
	HedgeSymbols            []string
	// Source names the policies the numbers were read from.
	Source string
}

// ExitReplayInput contains only retained evidence: the Flex statements, the
// cached daily history of the underlyings, recorded chain quotes and the
// measured fill half-spread.
type ExitReplayInput struct {
	Statements   []flexstmt.Statement
	BaseCurrency string
	// From and To bound the window; zero means every retained statement.
	From   time.Time
	To     time.Time
	Rules  []string
	Policy ExitReplayPolicy
	// Underlyings holds daily bars by underlying symbol. HistoryNotes says,
	// by symbol, why the bars may fall short of the days the replay needs;
	// HistorySource says where the bars came from.
	Underlyings   map[string][]UnderlyingBar
	HistoryNotes  map[string]ExitReplayHistoryNote
	HistorySource string
	// ClosedSessions lists, by underlying symbol, the days its exchange held
	// no session by Canary's official exchange calendar (a holiday or a
	// weekend). Such a statement day without a bar carries the previous close
	// and option mark; a day not listed without a bar stays untestable.
	ClosedSessions map[string][]time.Time
	ChainMarks     []RecordedOptionMark
	// HalfSpreadPct is the measured half-spread in percent of the price; nil
	// when no measured option fill exists.
	HalfSpreadPct     *float64
	HalfSpreadSamples int
	HalfSpreadSource  string
}

// ExitReplayHistoryNote is the caller's account of an underlying whose daily
// history may not span the days the replay needs: one of the
// rpc.EdgeReplayHistory reasons and what Canary holds.
type ExitReplayHistoryNote struct {
	Reason string
	Detail string
}

// ExitReplayHistorySlackDays is how far a name's daily history may stop short
// of either end of the days it is needed and still span them: a long weekend
// with a holiday. A session inside the span without a bar stays an untestable
// day, counted in the run's coverage.
const ExitReplayHistorySlackDays = 4

// ExitReplayHistoryCovers reports whether daily bars span first through
// last, within ExitReplayHistorySlackDays at either end.
func ExitReplayHistoryCovers(bars []UnderlyingBar, first, last time.Time) bool {
	days := make([]string, 0, len(bars))
	for _, bar := range bars {
		if bar.Close > 0 && !bar.Day.IsZero() {
			days = append(days, dayKey(bar.Day))
		}
	}
	slices.Sort(days)
	return historySpans(days, dayKey(first), dayKey(last))
}

func historySpans(days []string, first, last string) bool {
	if len(days) == 0 {
		return false
	}
	return days[0] <= dayKey(parseDay(first).AddDate(0, 0, ExitReplayHistorySlackDays)) && days[len(days)-1] >= dayKey(parseDay(last).AddDate(0, 0, -ExitReplayHistorySlackDays))
}

// Mark sources, in the order a day's price is chosen.
const (
	replayMarkStatement  = "statement"
	replayMarkTradeClose = "trade_close"
	replayMarkChainMid   = "chain_mid"
	replayMarkModelled   = "modelled"
)

// Unit outcome kinds.
const (
	replayOutcomeActual   = "actual"
	replayOutcomeRule     = "rule"
	replayOutcomeSettle   = "expiry_settle"
	replayOutcomeOpenEnd  = "open_end"
	replayOutcomeKeptReal = "kept_actual"
)

const replayFarDay = "9999-12-31"

type replayContract struct {
	conid      int64
	symbol     string
	underlying string
	currency   string
	right      string
	strike     float64
	mult       float64
	expiry     string // YYYY-MM-DD
	lots       []*replayLot
	preheld    bool
	executions []flexstmt.Trade
}

type replayLot struct {
	id  int
	c   *replayContract
	day string
	qty int
	// costPerContract is the multiplier-inclusive cost in the trade currency,
	// commission included, as IBKR's average cost reports it.
	costPerContract float64
	pre             bool
	units           []*replayUnit
}

type replayUnit struct {
	lot       *replayLot
	startBase float64
	actDay    string // empty while open at the window end
	actBase   float64
	actKind   string
}

type replayMark struct {
	price  float64
	source string
}

type replayOutcome struct {
	day    string
	base   float64
	kind   string
	reason string
}

type replay struct {
	in         ExitReplayInput
	ev         evidence
	base       string
	start      string
	from       string
	to         string
	days       []string
	snapshots  map[string][]flexstmt.OpenPosition
	nlv        map[string]float64
	contracts  map[int64]*replayContract
	order      []int64
	bySymbol   map[string][]int64
	symbols    []string
	marks      map[int64]map[string]replayMark
	markDays   map[int64][]string
	bars       map[string]map[string]UnderlyingBar
	barDays    map[string][]string
	closed     map[string]map[string]bool
	lifecycle  map[string]string
	halfSpread float64
	commBase   float64
	units      []*replayUnit
	scope      rpc.EdgeReplayScope
	missing    []rpc.EdgeReplayMissingHistory
	lotSeq     int
	fxCache    map[string]float64
}

// ExitReplay replays long-option exit rules over the owner's option trades in
// the window (long-option-exits LO-V1). Entries are the owner's actual buys;
// each rule set replaces the exits. Rules are evaluated once per statement day
// at that day's recorded closing mark. It is read-only and deterministic.
func ExitReplay(in ExitReplayInput) rpc.EdgeReplayResult {
	out := rpc.EdgeReplayResult{
		SchemaVersion: rpc.EdgeReplaySchemaVersion, State: rpc.EdgeStateCurrent, NotExecution: true,
		Runs: []rpc.EdgeReplayRun{}, Method: exitReplayMethod(in.HistorySource), MissingHistory: []rpc.EdgeReplayMissingHistory{},
		Scope:          rpc.EdgeReplayScope{Underlyings: []string{}, ExcludedOther: map[string]int{}},
		Reconciliation: rpc.EdgeReplayReconciliation{ReconciliationNotes: []string{}, NotCompared: []rpc.EdgeReplayReconContract{}, ResidualContracts: []rpc.EdgeReplayReconContract{}},
	}
	out.Parameters = exitReplayParameters(in)
	ev, err := collectEvidence(in.Statements)
	if err != nil {
		out.State, out.Reason = rpc.EdgeStateInsufficient, "statements_unreadable"
		return out
	}
	r := &replay{in: in, ev: ev, base: strings.ToUpper(strings.TrimSpace(in.BaseCurrency))}
	reason := r.prepare()
	if len(r.missing) > 0 {
		out.Incomplete, out.MissingHistory = true, r.missing
	}
	if reason != "" {
		out.State, out.Reason = rpc.EdgeStateInsufficient, reason
		out.Scope = r.scope
		if out.Scope.ExcludedOther == nil {
			out.Scope.ExcludedOther = map[string]int{}
		}
		if out.Scope.Underlyings == nil {
			out.Scope.Underlyings = []string{}
		}
		return out
	}
	out.From, out.To = parseDay(r.days[0]), parseDay(r.to)
	out.StartStateDate = parseDay(r.start)
	out.BaseCurrency = r.base
	out.StatementDays = len(r.days)
	out.Scope = r.scope
	if r.commBase > 0 {
		out.Parameters.CommissionPerContractBase = new(r.commBase)
	}
	out.Parameters.CommissionContracts = r.commissionContracts()

	actual := r.actualRun()
	out.Runs = append(out.Runs, actual)
	for _, prices := range []string{rpc.EdgeReplayPricesRecorded, rpc.EdgeReplayPricesModelled} {
		for _, id := range in.Rules {
			if id == rpc.EdgeReplaySetActual {
				continue
			}
			rules := rpc.EdgeReplaySetRules(id)
			if rules == nil {
				continue
			}
			run := r.simulate(id, rules, prices == rpc.EdgeReplayPricesModelled)
			run.DiffVsActualBase = run.PNLBase - actual.PNLBase
			for i := range run.ByUnderlying {
				run.ByUnderlying[i].DiffVsActualBase = run.ByUnderlying[i].PNLBase - actualByName(actual, run.ByUnderlying[i].Underlying)
			}
			out.Runs = append(out.Runs, run)
		}
	}
	out.Reconciliation = r.reconcile(actual.PNLBase)
	return out
}

func actualByName(run rpc.EdgeReplayRun, name string) float64 {
	for _, row := range run.ByUnderlying {
		if row.Underlying == name {
			return row.PNLBase
		}
	}
	return 0
}

// prepare resolves the window, the scope, the owner's lots and the recorded
// marks. A non-empty result names why the replay cannot run.
func (r *replay) prepare() string {
	r.scope = rpc.EdgeReplayScope{Underlyings: []string{}, ExcludedOther: map[string]int{}}
	r.snapshots = replaySnapshots(r.ev.statements)
	snapDays := slices.Sorted(maps.Keys(r.snapshots))
	if len(snapDays) == 0 {
		return "open_positions_unavailable"
	}
	last := snapDays[len(snapDays)-1]
	r.to = last
	if !r.in.To.IsZero() && dayKey(r.in.To) < last {
		r.to = dayKey(r.in.To)
	}
	from := ""
	if r.in.From.IsZero() {
		if len(snapDays) < 2 {
			return "statement_history_too_short"
		}
		from = nextDayKey(snapDays[0])
	} else {
		from = dayKey(r.in.From)
	}
	for _, day := range snapDays {
		if day < from {
			r.start = day
		}
	}
	if r.start == "" {
		return "start_snapshot_unavailable"
	}
	r.nlv = map[string]float64{}
	for _, item := range r.ev.equity {
		r.nlv[dayKey(item.row.ReportDate)] += item.row.TotalBase
	}
	r.lifecycle = map[string]string{}
	executed := map[string]bool{}
	var trades []flexstmt.Trade
	for _, trade := range r.ev.trades {
		if !strings.EqualFold(trade.AssetClass, "OPT") || trade.ConID == 0 {
			continue
		}
		if trade.LevelOfDetail != "" && trade.LevelOfDetail != "EXECUTION" && trade.LevelOfDetail != "EXECUTIONS" {
			continue
		}
		trades = append(trades, trade)
		if trade.TradeID != "" {
			executed[lifecycleTradeKey(trade.ConID, trade.TradeID)] = true
		}
	}
	for _, event := range r.ev.optionEvents {
		key := lifecycleTradeKey(event.ConID, event.TradeID)
		if kind := optionCycleCloser(event.TransactionType); event.TradeID != "" && executed[key] && kind != "" {
			r.lifecycle[key] = kind
		}
	}
	daySet := map[string]bool{}
	inWindow := func(day string) bool { return day > r.start && day <= r.to }
	for _, day := range snapDays {
		if inWindow(day) {
			daySet[day] = true
		}
	}
	for day := range r.nlv {
		if inWindow(day) {
			daySet[day] = true
		}
	}
	windowTrades := map[int64][]flexstmt.Trade{}
	for _, trade := range trades {
		day := dayKey(tradeStatementDay(trade))
		if !inWindow(day) {
			continue
		}
		daySet[day] = true
		windowTrades[trade.ConID] = append(windowTrades[trade.ConID], trade)
	}
	r.days = slices.Sorted(maps.Keys(daySet))
	// A requested window needs a baseline after all earlier activity. A
	// missing intervening snapshot cannot be replaced by replaying days
	// the caller excluded.
	if !r.in.From.IsZero() && len(r.days) > 0 && r.days[0] < from {
		return "start_snapshot_unavailable"
	}
	if len(r.days) == 0 {
		return "window_empty"
	}
	r.from = r.days[0]
	r.buildBars()
	r.buildScope(trades, windowTrades)
	if len(r.contracts) == 0 {
		return "no_long_options_in_window"
	}
	r.buildMarks(trades)
	r.settleOwnerEnd()
	if len(r.contracts) == 0 {
		return "no_long_options_in_window"
	}
	r.leaveOutMissingHistory()
	if len(r.contracts) == 0 {
		return "underlying_history_unavailable"
	}
	r.halfSpread = 0
	if r.in.HalfSpreadPct != nil && *r.in.HalfSpreadPct > 0 && *r.in.HalfSpreadPct < 100 {
		r.halfSpread = *r.in.HalfSpreadPct / 100
	}
	r.commBase = r.averageCommission()
	r.finishScope()
	return ""
}

// replaySnapshots returns the Open Positions summary rows by report date,
// taking each date from the latest-generated statement that carries it. A
// statement whose section is present and empty is an empty book on its date.
func replaySnapshots(statements []flexstmt.Statement) map[string][]flexstmt.OpenPosition {
	type candidate struct {
		generated time.Time
		rows      []flexstmt.OpenPosition
	}
	best := map[string]candidate{}
	for _, st := range statements {
		if !sectionPresent(st.Coverage, "open_positions") {
			continue
		}
		byDay := map[string][]flexstmt.OpenPosition{}
		for _, row := range st.Positions {
			if row.ReportDate.IsZero() || row.LevelOfDetail != "" && row.LevelOfDetail != "SUMMARY" {
				continue
			}
			byDay[dayKey(row.ReportDate)] = append(byDay[dayKey(row.ReportDate)], row)
		}
		if len(st.Positions) == 0 && !st.ToDate.IsZero() {
			byDay[dayKey(st.ToDate)] = nil
		}
		for day, rows := range byDay {
			if current, ok := best[day]; !ok || st.WhenGenerated.After(current.generated) {
				best[day] = candidate{generated: st.WhenGenerated, rows: rows}
			}
		}
	}
	out := make(map[string][]flexstmt.OpenPosition, len(best))
	for day, item := range best {
		out[day] = item.rows
	}
	return out
}

func (r *replay) buildBars() {
	r.bars, r.barDays = map[string]map[string]UnderlyingBar{}, map[string][]string{}
	for symbol, bars := range r.in.Underlyings {
		key := strings.ToUpper(strings.TrimSpace(symbol))
		byDay := map[string]UnderlyingBar{}
		for _, bar := range bars {
			if bar.Close > 0 && !bar.Day.IsZero() {
				byDay[dayKey(bar.Day)] = bar
			}
		}
		r.bars[key] = byDay
		r.barDays[key] = slices.Sorted(maps.Keys(byDay))
	}
	r.closed = map[string]map[string]bool{}
	for symbol, days := range r.in.ClosedSessions {
		key := strings.ToUpper(strings.TrimSpace(symbol))
		if r.closed[key] == nil {
			r.closed[key] = map[string]bool{}
		}
		for _, day := range days {
			r.closed[key][dayKey(day)] = true
		}
	}
}

// closedSession reports whether the underlying's exchange held no session on
// day by the official calendar and the history carries no bar for it: no
// rule can act on such a day.
func (r *replay) closedSession(symbol, day string) bool {
	_, bar := r.bars[symbol][day]
	return r.closed[symbol][day] && !bar
}

// spotOn is the underlying close a day is priced with: the day's own bar,
// or on a closed session the last close before it.
func (r *replay) spotOn(symbol, day string) (float64, bool) {
	if spot, ok := r.close(symbol, day); ok {
		return spot, true
	}
	if r.closedSession(symbol, day) {
		return r.closeAtOrBefore(symbol, day)
	}
	return 0, false
}

func (r *replay) close(symbol, day string) (float64, bool) {
	bar, ok := r.bars[symbol][day]
	return bar.Close, ok
}

// closeAtOrBefore is the last close on or before day.
func (r *replay) closeAtOrBefore(symbol, day string) (float64, bool) {
	days := r.barDays[symbol]
	i := sort.SearchStrings(days, nextDayKey(day))
	if i == 0 {
		return 0, false
	}
	return r.bars[symbol][days[i-1]].Close, true
}

// atr is the simple average true range of the ExitReplayATRSessions sessions
// ending on day; it needs one earlier close for the first range. A bar
// without a high and low enters as the true range's close-only form, the
// move from the previous close; closeOnly lists those bars' days.
func (r *replay) atr(symbol, day string) (atr float64, closeOnly []string, ok bool) {
	days := r.barDays[symbol]
	i := sort.SearchStrings(days, day)
	if i >= len(days) || days[i] != day || i < ExitReplayATRSessions {
		return 0, nil, false
	}
	total := 0.0
	for j := i - ExitReplayATRSessions + 1; j <= i; j++ {
		bar, prev := r.bars[symbol][days[j]], r.bars[symbol][days[j-1]]
		if bar.High <= 0 || bar.Low <= 0 {
			total += math.Abs(bar.Close - prev.Close)
			closeOnly = append(closeOnly, days[j])
			continue
		}
		total += math.Max(bar.High-bar.Low, math.Max(math.Abs(bar.High-prev.Close), math.Abs(bar.Low-prev.Close)))
	}
	return total / ExitReplayATRSessions, closeOnly, true
}

// highestClose is the highest close from first through day, both inclusive;
// first before the history's start reads from the start.
func (r *replay) highestClose(symbol, first, day string) (float64, bool) {
	hi, ok := 0.0, false
	for _, d := range r.barDays[symbol] {
		if d < first || d > day {
			continue
		}
		hi, ok = math.Max(hi, r.bars[symbol][d].Close), true
	}
	return hi, ok
}

// fx converts the trade currency to base at the day's broker rate. Rates
// are looked up once per currency and day.
func (r *replay) fx(currency, day string) (float64, bool) {
	key := currency + "\x00" + day
	if rate, ok := r.fxCache[key]; ok {
		return rate, rate > 0
	}
	value := 0.0
	if rate := baseConversionFX(currency, r.base, parseDay(day), nil, r.ev.fxRates); rate != nil && *rate > 0 {
		value = *rate
	}
	if r.fxCache == nil {
		r.fxCache = map[string]float64{}
	}
	r.fxCache[key] = value
	return value, value > 0
}

func (r *replay) executionFX(trade flexstmt.Trade) (float64, bool) {
	rate := baseConversionFX(trade.Currency, r.base, trade.ExecutedAt, trade.FXRateToBase, r.ev.fxRates)
	if rate == nil || *rate <= 0 {
		return 0, false
	}
	return *rate, true
}

func (r *replay) hedgeSymbol(symbol string) bool {
	return slices.ContainsFunc(r.in.Policy.HedgeSymbols, func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), symbol) })
}

// buildScope classifies every option held at the start or traded in the
// window and builds the owner's lots of each long option in scope.
func (r *replay) buildScope(trades []flexstmt.Trade, windowTrades map[int64][]flexstmt.Trade) {
	instruments := map[int64]flexstmt.Instrument{}
	for _, item := range r.ev.instruments {
		instruments[item.ConID] = item
	}
	startRows := map[int64]flexstmt.OpenPosition{}
	for _, row := range r.snapshots[r.start] {
		if strings.EqualFold(row.AssetClass, "OPT") && row.ConID != 0 && row.Quantity != nil && !almostEqual(*row.Quantity, 0) {
			startRows[row.ConID] = row
		}
	}
	candidates := map[int64]bool{}
	for conid := range windowTrades {
		candidates[conid] = true
	}
	for conid := range startRows {
		candidates[conid] = true
	}
	r.contracts = map[int64]*replayContract{}
	for _, conid := range slices.Sorted(maps.Keys(candidates)) {
		rows := windowTrades[conid]
		sort.SliceStable(rows, func(i, j int) bool {
			if !rows[i].ExecutedAt.Equal(rows[j].ExecutedAt) {
				return rows[i].ExecutedAt.Before(rows[j].ExecutedAt)
			}
			return rows[i].RecordID < rows[j].RecordID
		})
		start, preheld := startRows[conid]
		inst := instruments[conid]
		c := &replayContract{conid: conid, symbol: inst.Symbol, underlying: strings.ToUpper(inst.UnderlyingSymbol), currency: inst.Currency, expiry: optionExpiry(inst.Expiry), executions: rows}
		switch optionPutCall(inst.PutCall) {
		case "call":
			c.right = "C"
		case "put":
			c.right = "P"
		}
		if inst.Strike != nil {
			c.strike = *inst.Strike
		}
		if inst.Multiplier != nil {
			c.mult = *inst.Multiplier
		}
		for _, row := range rows {
			c.symbol = firstNonEmpty(c.symbol, row.Symbol)
			c.underlying = strings.ToUpper(firstNonEmpty(c.underlying, row.UnderlyingSymbol))
			c.currency = firstNonEmpty(c.currency, row.Currency)
			if c.mult <= 0 && row.Multiplier != nil {
				c.mult = *row.Multiplier
			}
		}
		if preheld {
			c.symbol = firstNonEmpty(c.symbol, start.Symbol)
			c.underlying = strings.ToUpper(firstNonEmpty(c.underlying, start.UnderlyingSymbol))
			c.currency = firstNonEmpty(c.currency, start.Currency)
			if c.mult <= 0 && start.Multiplier != nil {
				c.mult = *start.Multiplier
			}
		}
		c.currency = strings.ToUpper(c.currency)
		short := preheld && *start.Quantity < 0
		for _, row := range rows {
			if strings.EqualFold(row.Side, "SELL") && strings.Contains(strings.ToUpper(row.OpenClose), "O") {
				short = true
			}
		}
		switch {
		case c.right == "P" && r.hedgeSymbol(c.underlying):
			r.scope.ExcludedHedgeContracts++
			r.scope.ExcludedHedgeExecutions += len(rows)
			continue
		case short:
			r.scope.ExcludedShortContracts++
			r.scope.ExcludedShortExecutions += len(rows)
			continue
		case c.right == "" || c.strike <= 0 || c.expiry == "" || c.mult <= 0 || c.underlying == "":
			r.scope.ExcludedOther["instrument_unavailable"]++
			continue
		}
		if reason := r.buildLots(c, start, preheld, trades); reason != "" {
			r.scope.ExcludedOther[reason]++
			continue
		}
		r.contracts[conid] = c
	}
}

// buildLots turns the owner's executions into whole-contract units: one lot
// per buy day, sells taken first in first out. Pre-held contracts start as one
// lot valued at the start snapshot's mark.
func (r *replay) buildLots(c *replayContract, start flexstmt.OpenPosition, preheld bool, trades []flexstmt.Trade) string {
	var queue []*replayUnit
	var lots []*replayLot
	newLot := func(day string, qty int, costPer, startPerUnit float64, pre bool) *replayLot {
		r.lotSeq++
		lot := &replayLot{id: r.lotSeq, c: c, day: day, qty: qty, costPerContract: costPer, pre: pre}
		for range qty {
			unit := &replayUnit{lot: lot, startBase: startPerUnit}
			lot.units = append(lot.units, unit)
		}
		lots = append(lots, lot)
		queue = append(queue, lot.units...)
		return lot
	}
	if preheld {
		qty := *start.Quantity
		if !wholeContracts(qty) || start.MarkPrice == nil {
			return "start_position_incomplete"
		}
		cost := 0.0
		switch {
		case start.CostBasisMoney != nil:
			cost = *start.CostBasisMoney / qty
		case start.CostBasisPrice != nil:
			cost = *start.CostBasisPrice * c.mult
		default:
			return "start_position_incomplete"
		}
		fx, ok := r.fx(c.currency, r.start)
		if !ok {
			return "fx_unavailable"
		}
		first := replayHeldCycleStart(c.conid, qty, r.start, trades)
		newLot(first, int(math.Round(qty)), cost, *start.MarkPrice*c.mult*fx, true)
		c.preheld = true
	}
	byDay := map[string][]flexstmt.Trade{}
	for _, trade := range c.executions {
		if !strings.EqualFold(trade.Side, "BUY") && !strings.EqualFold(trade.Side, "SELL") {
			return "execution_path_incomplete"
		}
		byDay[dayKey(tradeStatementDay(trade))] = append(byDay[dayKey(tradeStatementDay(trade))], trade)
	}
	for _, day := range slices.Sorted(maps.Keys(byDay)) {
		qty, cost, costBase := 0.0, 0.0, 0.0
		for _, trade := range byDay[day] {
			if !strings.EqualFold(trade.Side, "BUY") {
				continue
			}
			if trade.Quantity == nil || trade.NetCash == nil || !wholeContracts(*trade.Quantity) {
				return "execution_amount_missing"
			}
			fx, ok := r.executionFX(trade)
			if !ok {
				return "fx_unavailable"
			}
			qty += math.Abs(*trade.Quantity)
			cost += -*trade.NetCash
			costBase += -*trade.NetCash * fx
		}
		if qty > 0 {
			newLot(day, int(math.Round(qty)), cost/qty, costBase/qty, false)
		}
		for _, trade := range byDay[day] {
			if !strings.EqualFold(trade.Side, "SELL") {
				continue
			}
			if trade.Quantity == nil || trade.NetCash == nil || !wholeContracts(*trade.Quantity) {
				return "execution_amount_missing"
			}
			fx, ok := r.executionFX(trade)
			if !ok {
				return "fx_unavailable"
			}
			n := int(math.Round(math.Abs(*trade.Quantity)))
			kind := "sold"
			if trade.TradeID != "" && r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)] != "" {
				kind = r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)]
			}
			for range n {
				if len(queue) == 0 {
					return "position_path_unbalanced"
				}
				unit := queue[0]
				queue = queue[1:]
				unit.actDay, unit.actBase, unit.actKind = day, *trade.NetCash*fx/float64(n), kind
			}
		}
	}
	for _, unit := range queue {
		unit.actKind = "open"
	}
	c.lots = lots
	return ""
}

// replayHeldCycleStart walks the owner's executions backwards from the
// baseline quantity to its most recent flat state. An older completed cycle
// cannot seed this holding's price high. Without a balanced execution path,
// the known start snapshot is the earliest usable state.
func replayHeldCycleStart(conid int64, quantity float64, start string, trades []flexstmt.Trade) string {
	rows := make([]flexstmt.Trade, 0)
	for _, trade := range trades {
		if trade.ConID == conid && dayKey(tradeStatementDay(trade)) <= start {
			rows = append(rows, trade)
		}
	}
	slices.SortStableFunc(rows, func(a, b flexstmt.Trade) int {
		if order := a.ExecutedAt.Compare(b.ExecutedAt); order != 0 {
			return order
		}
		return strings.Compare(a.RecordID, b.RecordID)
	})
	for _, trade := range slices.Backward(rows) {
		if trade.Quantity == nil || !wholeContracts(*trade.Quantity) {
			return start
		}
		switch strings.ToUpper(trade.Side) {
		case "BUY":
			quantity -= math.Abs(*trade.Quantity)
		case "SELL":
			quantity += math.Abs(*trade.Quantity)
		default:
			return start
		}
		if almostEqual(quantity, 0) {
			return dayKey(tradeStatementDay(trade))
		}
		if quantity < 0 {
			return start
		}
	}
	return start
}

func wholeContracts(q float64) bool {
	return !math.IsNaN(q) && !math.IsInf(q, 0) && !almostEqual(q, 0) && math.Abs(q-math.Round(q)) <= 1e-9
}

// buildMarks collects every recorded closing price of the contracts in scope:
// the statement's Open Positions mark, else an execution row's closing price,
// else the mid of a two-sided recorded chain quote.
func (r *replay) buildMarks(trades []flexstmt.Trade) {
	r.marks, r.markDays = map[int64]map[string]replayMark{}, map[int64][]string{}
	set := func(conid int64, day string, price float64, source string) {
		if _, ok := r.contracts[conid]; !ok || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return
		}
		if r.marks[conid] == nil {
			r.marks[conid] = map[string]replayMark{}
		}
		if _, ok := r.marks[conid][day]; !ok {
			r.marks[conid][day] = replayMark{price: price, source: source}
		}
	}
	for day, rows := range r.snapshots {
		if day < r.start || day > r.to {
			continue
		}
		for _, row := range rows {
			if strings.EqualFold(row.AssetClass, "OPT") && row.MarkPrice != nil {
				set(row.ConID, day, *row.MarkPrice, replayMarkStatement)
			}
		}
	}
	for _, trade := range trades {
		lifecycle := trade.TradeID != "" && r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)] != ""
		if !lifecycle && trade.ClosePrice != nil && *trade.ClosePrice > 0 {
			set(trade.ConID, dayKey(tradeStatementDay(trade)), *trade.ClosePrice, replayMarkTradeClose)
		}
	}
	byIdentity := map[string]int64{}
	for conid, c := range r.contracts {
		byIdentity[replayIdentity(c.underlying, c.expiry, c.strike, c.right)] = conid
	}
	for _, quote := range r.in.ChainMarks {
		if quote.Bid <= 0 || quote.Ask < quote.Bid || quote.Day.IsZero() {
			continue
		}
		conid := quote.ConID
		if _, ok := r.contracts[conid]; !ok {
			conid = byIdentity[replayIdentity(quote.Underlying, optionExpiry(quote.Expiry), quote.Strike, strings.ToUpper(quote.Right))]
		}
		day := dayKey(quote.Day)
		if day < r.start || day > r.to {
			continue
		}
		set(conid, day, (quote.Bid+quote.Ask)/2, replayMarkChainMid)
	}
	for conid, days := range r.marks {
		r.markDays[conid] = slices.Sorted(maps.Keys(days))
	}
}

func replayIdentity(underlying, expiry string, strike float64, right string) string {
	return strings.ToUpper(strings.TrimSpace(underlying)) + "|" + expiry + "|" + strconv.FormatFloat(strike, 'f', 4, 64) + "|" + right
}

// settleOwnerEnd values the units the owner still held at the window end: at
// the end day's mark, or, for a contract that expired in the window without a
// retained lifecycle row, at its intrinsic value on the expiry close. A
// contract without either leaves the scope.
func (r *replay) settleOwnerEnd() {
	for _, conid := range slices.Sorted(maps.Keys(r.contracts)) {
		c := r.contracts[conid]
		ok := true
		for _, lot := range c.lots {
			for _, unit := range lot.units {
				if unit.actKind != "open" {
					continue
				}
				if c.expiry <= r.to {
					spot, haveSpot := r.closeAtOrBefore(c.underlying, c.expiry)
					fx, haveFX := r.fx(c.currency, c.expiry)
					if !haveSpot || !haveFX {
						ok = false
						continue
					}
					unit.actDay, unit.actBase, unit.actKind = c.expiry, replayIntrinsic(c.right, spot, c.strike)*c.mult*fx, "expiry_unbooked"
					continue
				}
				mark, haveMark := r.marks[conid][r.to]
				fx, haveFX := r.fx(c.currency, r.to)
				if !haveMark || !haveFX {
					ok = false
					continue
				}
				unit.actBase = mark.price * c.mult * fx
			}
		}
		if !ok {
			r.scope.ExcludedOther["end_value_unavailable"]++
			delete(r.contracts, conid)
		}
	}
}

// leaveOutMissingHistory removes every underlying whose daily history does
// not span the statement days its options were held: such a name cannot be
// tested, and keeping the owner's outcome for its days would change every
// run's total without saying so. Each one is listed with the caller's reason
// and the owner's own P/L on it.
func (r *replay) leaveOutMissingHistory() {
	type need struct {
		first, last string
		conids      []int64
	}
	needs := map[string]*need{}
	for _, conid := range slices.Sorted(maps.Keys(r.contracts)) {
		c := r.contracts[conid]
		first, last := r.heldDays(c)
		if first == "" {
			continue
		}
		n := needs[c.underlying]
		if n == nil {
			n = &need{first: first, last: last}
			needs[c.underlying] = n
		}
		n.first, n.last, n.conids = min(n.first, first), max(n.last, last), append(n.conids, conid)
	}
	for _, name := range slices.Sorted(maps.Keys(needs)) {
		n, days := needs[name], r.barDays[name]
		if historySpans(days, n.first, n.last) {
			continue
		}
		note := r.in.HistoryNotes[name]
		gap := rpc.EdgeReplayMissingHistory{Underlying: name, Reason: note.Reason, Detail: note.Detail, NeededFrom: parseDay(n.first), NeededTo: parseDay(n.last)}
		if gap.Reason == "" {
			gap.Reason = rpc.EdgeReplayHistoryNotRetained
		}
		if len(days) > 0 {
			gap.HistoryFrom, gap.HistoryTo = parseDay(days[0]), parseDay(days[len(days)-1])
		}
		for _, conid := range n.conids {
			for _, lot := range r.contracts[conid].lots {
				for _, unit := range lot.units {
					gap.ActualPNLBase += unit.actBase - unit.startBase
				}
			}
			gap.Contracts++
			delete(r.contracts, conid)
		}
		r.missing = append(r.missing, gap)
	}
}

// heldDays is the first and last statement day a rule run can hold the
// contract: from the window's start (held then) or its first buy, through its
// expiry or the window's end.
func (r *replay) heldDays(c *replayContract) (string, string) {
	from, to := "", min(c.expiry, r.to)
	for _, lot := range c.lots {
		day := lot.day
		if lot.pre {
			day = r.from
		}
		if from == "" || day < from {
			from = day
		}
	}
	first, last := "", ""
	for _, day := range r.days {
		if day < from || day > to {
			continue
		}
		if first == "" {
			first = day
		}
		last = day
	}
	return first, last
}

func replayIntrinsic(right string, spot, strike float64) float64 {
	if right == "C" {
		return math.Max(0, spot-strike)
	}
	return math.Max(0, strike-spot)
}

// averageCommission is the owner's actual commission per contract in base
// currency over the window's exchange executions in scope.
func (r *replay) averageCommission() float64 {
	total, contracts := 0.0, 0.0
	for _, c := range r.contracts {
		for _, trade := range c.executions {
			if trade.TradeID != "" && r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)] != "" || trade.Commission == nil || trade.Quantity == nil {
				continue
			}
			commissionCurrency := firstNonEmpty(trade.CommissionCurrency, trade.Currency)
			var fx *float64
			if strings.EqualFold(commissionCurrency, trade.Currency) {
				fx = baseConversionFX(trade.Currency, r.base, trade.ExecutedAt, trade.FXRateToBase, r.ev.fxRates)
			} else {
				fx = baseConversionFX(commissionCurrency, r.base, trade.ExecutedAt, nil, r.ev.fxRates)
			}
			if fx == nil {
				continue
			}
			total += math.Abs(*trade.Commission) * *fx
			contracts += math.Abs(*trade.Quantity)
		}
	}
	if contracts == 0 {
		return 0
	}
	return total / contracts
}

func (r *replay) commissionContracts() float64 {
	contracts := 0.0
	for _, c := range r.contracts {
		for _, trade := range c.executions {
			if trade.TradeID != "" && r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)] != "" || trade.Commission == nil || trade.Quantity == nil {
				continue
			}
			contracts += math.Abs(*trade.Quantity)
		}
	}
	return contracts
}

func (r *replay) finishScope() {
	r.bySymbol = map[string][]int64{}
	r.order = slices.Sorted(maps.Keys(r.contracts))
	for _, conid := range r.order {
		c := r.contracts[conid]
		r.bySymbol[c.underlying] = append(r.bySymbol[c.underlying], conid)
		for _, lot := range c.lots {
			if lot.pre {
				r.scope.PreheldLots++
			} else {
				r.scope.BuyLots++
				r.scope.ContractsBought += float64(lot.qty)
			}
			r.units = append(r.units, lot.units...)
		}
	}
	r.symbols = slices.Sorted(maps.Keys(r.bySymbol))
	r.scope.Contracts = len(r.contracts)
	r.scope.Underlyings = append([]string{}, r.symbols...)
}

// mark is the price that decides a held contract on day: a recorded mark,
// or, in the modelled run, Black-Scholes at the day's underlying close with
// the volatility backed out of the last recorded mark (rate zero, as the
// mandate's revaluation). A volatility that cannot be backed out prices at
// intrinsic value. Without a prior recorded mark the day stays untestable.
func (r *replay) mark(c *replayContract, day string, modelled bool) (replayMark, bool) {
	if mark, ok := r.marks[c.conid][day]; ok {
		return mark, true
	}
	if !modelled || day > c.expiry {
		return replayMark{}, false
	}
	days := r.markDays[c.conid]
	i := sort.SearchStrings(days, day)
	if i == 0 {
		return replayMark{}, false
	}
	prior := days[i-1]
	priorSpot, ok := r.spotOn(c.underlying, prior)
	if !ok {
		return replayMark{}, false
	}
	spot, ok := r.close(c.underlying, day)
	if !ok {
		return replayMark{}, false
	}
	call := c.right == "C"
	vol, ok := mandate.ImpliedVolatility(call, r.marks[c.conid][prior].price, priorSpot, c.strike, replayYears(prior, c.expiry))
	if !ok {
		vol = 0
	}
	return replayMark{price: mandate.OptionPrice(call, spot, c.strike, replayYears(day, c.expiry), vol), source: replayMarkModelled}, true
}

func replayYears(day, expiry string) float64 {
	return float64(replayDTE(day, expiry)) / 365
}

func replayDTE(day, expiry string) int {
	return int(math.Round(parseDay(expiry).Sub(parseDay(day)).Hours() / 24))
}

// actualRun is set A: the owner's own exits.
func (r *replay) actualRun() rpc.EdgeReplayRun {
	run := rpc.EdgeReplayRun{ID: rpc.EdgeReplaySetActual, Rules: []string{}, Prices: rpc.EdgeReplayPricesActual, ExitsByReason: []rpc.EdgeReplayReason{}, ByUnderlying: []rpc.EdgeReplayNameRow{}}
	outcomes := map[*replayUnit]replayOutcome{}
	reasons := map[string]*rpc.EdgeReplayReason{}
	events := map[string]bool{}
	for _, unit := range r.units {
		outcomes[unit] = replayOutcome{day: unit.actDay, base: unit.actBase, kind: replayOutcomeActual, reason: unit.actKind}
		if unit.actDay == "" {
			continue
		}
		reason := unit.actKind
		row := reasons[reason]
		if row == nil {
			row = &rpc.EdgeReplayReason{Reason: reason}
			reasons[reason] = row
		}
		row.Units++
		key := reason + "\x00" + strconv.FormatInt(unit.lot.c.conid, 10) + "\x00" + unit.actDay
		if !events[key] {
			events[key] = true
			row.Events++
			run.Exits++
		}
	}
	run.ExitsByReason = sortedReasons(reasons)
	for _, day := range r.days {
		rows, ok := r.snapshots[day]
		if !ok {
			continue
		}
		value := 0.0
		for _, row := range rows {
			c, inScope := r.contracts[row.ConID]
			if !inScope || row.Quantity == nil || row.MarkPrice == nil {
				continue
			}
			fx, haveFX := r.fx(c.currency, day)
			if !haveFX {
				continue
			}
			value += *row.Quantity * *row.MarkPrice * c.mult * fx
		}
		r.notePeak(&run, value, day)
	}
	r.summarize(&run, outcomes)
	return run
}

func (r *replay) notePeak(run *rpc.EdgeReplayRun, value float64, day string) {
	if value <= run.PeakValueBase {
		return
	}
	run.PeakValueBase, run.PeakDate = value, parseDay(day)
	run.PeakNLVBase, run.PeakPctNLV = nil, nil
	if nlv, ok := r.nlv[day]; ok && nlv > 0 {
		run.PeakNLVBase = new(nlv)
		run.PeakPctNLV = new(value / nlv * 100)
	}
}

func sortedReasons(in map[string]*rpc.EdgeReplayReason) []rpc.EdgeReplayReason {
	out := make([]rpc.EdgeReplayReason, 0, len(in))
	for _, key := range slices.Sorted(maps.Keys(in)) {
		out = append(out, *in[key])
	}
	return out
}

// summarize totals a run's outcomes: P/L, by underlying, and the timing of
// each unit's and each contract's exit against the owner's.
func (r *replay) summarize(run *rpc.EdgeReplayRun, outcomes map[*replayUnit]replayOutcome) {
	byName := map[string]float64{}
	for _, unit := range r.units {
		outcome := outcomes[unit]
		pnl := outcome.base - unit.startBase
		run.PNLBase += pnl
		byName[unit.lot.c.underlying] += pnl
		if outcome.kind == replayOutcomeKeptReal {
			run.VsOwner.Untestable++
			continue
		}
		addTiming(&run.VsOwner, outcome.day, unit.actDay)
	}
	for _, conid := range r.order {
		simLast, ownerLast, testable := "", "", false
		for _, lot := range r.contracts[conid].lots {
			for _, unit := range lot.units {
				outcome := outcomes[unit]
				testable = testable || outcome.kind != replayOutcomeKeptReal
				simLast, ownerLast = max(simLast, orFar(outcome.day)), max(ownerLast, orFar(unit.actDay))
			}
		}
		if !testable {
			run.ContractsVsOwner.Untestable++
			continue
		}
		addTiming(&run.ContractsVsOwner, simLast, ownerLast)
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		run.ByUnderlying = append(run.ByUnderlying, rpc.EdgeReplayNameRow{Underlying: name, PNLBase: byName[name]})
	}
}

func orFar(day string) string {
	if day == "" {
		return replayFarDay
	}
	return day
}

func addTiming(t *rpc.EdgeReplayTiming, sim, owner string) {
	sim, owner = orFar(sim), orFar(owner)
	switch {
	case sim < owner:
		t.Earlier++
	case sim > owner:
		t.Later++
	default:
		t.Same++
	}
}

type replayHeld struct {
	lot   *replayLot
	units []*replayUnit
}

type replaySim struct {
	r         *replay
	run       *rpc.EdgeReplayRun
	rules     map[string]bool
	modelled  bool
	exit      risk.OptionExitPolicy
	core      bool
	outcomes  map[*replayUnit]replayOutcome
	held      map[int64][]*replayHeld
	highWater map[int64]risk.OptionExitHighWater
	lastMark  map[int64]replayDayPrice
	recovered map[int]bool
	reasons   map[string]*rpc.EdgeReplayReason
	// closeOnly holds the underlying bars, by symbol and day, already
	// counted as close-only ranges.
	closeOnly map[string]bool
}

// simulate runs one rule set over the window. The owner's buys enter on
// their day; every exit is the rule set's.
func (r *replay) simulate(id string, rules []string, modelled bool) rpc.EdgeReplayRun {
	prices := rpc.EdgeReplayPricesRecorded
	if modelled {
		prices = rpc.EdgeReplayPricesModelled
	}
	run := rpc.EdgeReplayRun{ID: id, Rules: append([]string{}, rules...), Prices: prices, ExitsByReason: []rpc.EdgeReplayReason{}, ByUnderlying: []rpc.EdgeReplayNameRow{}}
	s := &replaySim{r: r, run: &run, rules: map[string]bool{}, modelled: modelled, outcomes: map[*replayUnit]replayOutcome{},
		held: map[int64][]*replayHeld{}, highWater: map[int64]risk.OptionExitHighWater{}, recovered: map[int]bool{}, reasons: map[string]*rpc.EdgeReplayReason{},
		lastMark: map[int64]replayDayPrice{}, closeOnly: map[string]bool{}}
	for _, rule := range rules {
		s.rules[rule] = true
	}
	s.exit = replayExitPolicy(r.in.Policy.Exit, s.rules)
	s.core = s.rules[rpc.EdgeReplayRuleLossExit] || s.rules[rpc.EdgeReplayRuleExpiryClose] || s.rules[rpc.EdgeReplayRuleProfitTrail]
	for _, conid := range r.order {
		c := r.contracts[conid]
		for _, lot := range c.lots {
			if lot.pre {
				s.held[conid] = append(s.held[conid], &replayHeld{lot: lot, units: append([]*replayUnit{}, lot.units...)})
				if s.rules[rpc.EdgeReplayRuleProfitTrail] {
					if hw, ok := r.initialHighWater(lot); ok {
						s.highWater[conid] = hw
					}
				}
			}
		}
	}
	for i, day := range r.days {
		prev := r.start
		if i > 0 {
			prev = r.days[i-1]
		}
		for _, conid := range r.order {
			for _, lot := range r.contracts[conid].lots {
				if !lot.pre && lot.day == day {
					s.held[conid] = append(s.held[conid], &replayHeld{lot: lot, units: append([]*replayUnit{}, lot.units...)})
				}
			}
		}
		for _, symbol := range r.symbols {
			for _, conid := range r.bySymbol[symbol] {
				s.evaluate(r.contracts[conid], day, prev)
			}
			if s.rules[rpc.EdgeReplayRuleTrimToCap] && !r.closedSession(symbol, day) {
				s.trim(symbol, day)
			}
		}
		value := 0.0
		for _, conid := range r.order {
			n := s.count(conid)
			last, ok := s.lastMark[conid]
			if n == 0 || !ok {
				continue
			}
			c := r.contracts[conid]
			if fx, haveFX := r.fx(c.currency, day); haveFX {
				value += last.price * c.mult * fx * float64(n)
			}
		}
		r.notePeak(&run, value, day)
		for _, conid := range r.order {
			s.settleExpiry(r.contracts[conid], day, prev)
		}
	}
	for _, conid := range r.order {
		c := r.contracts[conid]
		units := s.heldUnits(conid)
		if len(units) == 0 {
			continue
		}
		last, ok := s.lastMark[conid]
		fx, haveFX := r.fx(c.currency, r.to)
		if ok && last.day == r.to && haveFX {
			for _, unit := range units {
				s.outcomes[unit] = replayOutcome{base: last.price * c.mult * fx, kind: replayOutcomeOpenEnd}
			}
			continue
		}
		s.keepActual(conid, false)
	}
	run.ExitsByReason = sortedReasons(s.reasons)
	r.summarize(&run, s.outcomes)
	return run
}

// replayExitPolicy switches off the rules a set does not run by giving them a
// line no option reaches: a loss line beyond any loss, a profit trail that
// never arms, and an expiry act level below zero, which matches no expiry.
func replayExitPolicy(pol risk.OptionExitPolicy, rules map[string]bool) risk.OptionExitPolicy {
	if !rules[rpc.EdgeReplayRuleLossExit] {
		pol.LossExitPct = math.MaxFloat64
	}
	if !rules[rpc.EdgeReplayRuleExpiryClose] {
		pol.ExpiryCloseDTE = -1
	}
	if !rules[rpc.EdgeReplayRuleProfitTrail] {
		pol.ProfitArmGainPct = math.MaxFloat64
	}
	return pol
}

// initialHighWater carries the profit trail of a contract held at the start:
// the highest statement mark from its first buy through the start date, when
// that high reached the arming gain over its cost.
func (r *replay) initialHighWater(lot *replayLot) (risk.OptionExitHighWater, bool) {
	c := lot.c
	high := 0.0
	for _, day := range slices.Sorted(maps.Keys(r.snapshots)) {
		if day < lot.day || day > r.start {
			continue
		}
		matched := false
		for _, row := range r.snapshots[day] {
			if row.ConID != c.conid || row.Quantity == nil || !almostEqual(*row.Quantity, float64(lot.qty)) || row.MarkPrice == nil {
				continue
			}
			costPer := 0.0
			switch {
			case row.CostBasisMoney != nil && *row.Quantity > 0:
				costPer = *row.CostBasisMoney / *row.Quantity
			case row.CostBasisPrice != nil:
				costPer = *row.CostBasisPrice * c.mult
			}
			if !almostEqual(costPer, lot.costPerContract) {
				continue
			}
			matched = true
			high = math.Max(high, *row.MarkPrice)
		}
		if !matched {
			high = 0
		}
	}
	cost := lot.costPerContract / c.mult
	if high <= 0 || cost <= 0 || (high/cost-1)*100 < r.in.Policy.Exit.ProfitArmGainPct {
		return risk.OptionExitHighWater{}, false
	}
	return risk.OptionExitHighWater{PerShare: high, CostPremium: cost, Quantity: float64(lot.qty)}, true
}

func (s *replaySim) heldUnits(conid int64) []*replayUnit {
	var out []*replayUnit
	for _, entry := range s.held[conid] {
		out = append(out, entry.units...)
	}
	return out
}

func (s *replaySim) count(conid int64) int {
	n := 0
	for _, entry := range s.held[conid] {
		n += len(entry.units)
	}
	return n
}

// evaluate decides one held contract on one statement day.
func (s *replaySim) evaluate(c *replayContract, day, prev string) {
	units := s.heldUnits(c.conid)
	if len(units) == 0 {
		return
	}
	if day > c.expiry {
		// Past expiry nothing can act; settleExpiry has settled the
		// contract or, without a recent mark, the end keeps the owner's
		// outcome.
		return
	}
	if s.r.closedSession(c.underlying, day) {
		// The exchange was closed: nothing traded, no rule can act, and the
		// contract carries its last mark into the next session.
		if last, ok := s.carriedMark(c, day); ok {
			s.lastMark[c.conid] = replayDayPrice{day: day, price: last}
			s.run.Coverage.ClosedSessionDays++
			return
		}
	}
	mark, ok := s.r.mark(c, day, s.modelled)
	if !ok {
		// On expiry day a contract marked the session before settles from
		// the underlying close below; any other day without a recorded
		// price cannot be tested and keeps the owner's outcome.
		if day == c.expiry && s.lastMark[c.conid].day == prev {
			return
		}
		s.keepActual(c.conid, true)
		return
	}
	spot, haveSpot := s.r.close(c.underlying, day)
	fx, haveFX := s.r.fx(c.currency, day)
	if !haveSpot || !haveFX {
		s.keepActual(c.conid, true)
		return
	}
	switch mark.source {
	case replayMarkStatement:
		s.run.Coverage.StatementDays++
	case replayMarkTradeClose:
		s.run.Coverage.TradeCloseDays++
	case replayMarkChainMid:
		s.run.Coverage.ChainMidDays++
	case replayMarkModelled:
		s.run.Coverage.ModelledDays++
	}
	s.lastMark[c.conid] = replayDayPrice{day: day, price: mark.price}
	if s.core {
		if reason := s.coreExit(c, units, mark.price, spot, day); reason != "" {
			s.sell(c, units, day, mark.price, fx, reason)
			return
		}
	}
	if s.rules[rpc.EdgeReplayRuleRecoverCost] {
		s.recoverCost(c, day, mark.price, fx)
	}
	if s.rules[rpc.EdgeReplayRuleStockTrail] {
		s.stockTrail(c, day, mark.price, spot, fx)
	}
}

// carriedMark is the option mark a closed session carries: the last price
// the run decided the contract on, else its last recorded mark before day.
func (s *replaySim) carriedMark(c *replayContract, day string) (float64, bool) {
	if last, ok := s.lastMark[c.conid]; ok {
		return last.price, true
	}
	days := s.r.markDays[c.conid]
	i := sort.SearchStrings(days, day)
	if i == 0 {
		return 0, false
	}
	return s.r.marks[c.conid][days[i-1]].price, true
}

// coreExit runs risk.EvaluateOptionExit on the day's state: the closing mark
// stands in for the executable bid (and ask), the cost basis is the average
// cost of the contracts still held, the underlying price is the day's close
// and the profit trail's high water is carried day to day. A carried mark
// resets when the cost basis or quantity changes, as the daemon's does.
func (s *replaySim) coreExit(c *replayContract, units []*replayUnit, mark, spot float64, day string) string {
	avg := 0.0
	for _, unit := range units {
		avg += unit.lot.costPerContract
	}
	avg /= float64(len(units))
	if mark <= 0 {
		// A worthless mark is a total loss; the evaluator needs a positive
		// bid to measure it, the loss line does not.
		if s.rules[rpc.EdgeReplayRuleLossExit] && avg > 0 {
			return rpc.EdgeReplayRuleLossExit
		}
		return ""
	}
	quantity := float64(len(units))
	decision := risk.EvaluateOptionExit(risk.OptionExitInput{
		ConID: 1, Quantity: quantity, Multiplier: int(math.Round(c.mult)), AvgCost: avg,
		Bid: mark, Ask: mark, DTE: replayDTE(day, c.expiry),
		DirectionalIntent: true, Standalone: true, EconomicRoleAllowed: true, QuoteLive: true, QuoteFresh: true, SessionOpen: true,
		Right: c.right, Strike: c.strike, Underlying: spot, CarriedHighWater: s.highWater[c.conid],
	}, s.exit)
	switch decision.Action {
	case risk.OptionExitActionLoss:
		return rpc.EdgeReplayRuleLossExit
	case risk.OptionExitActionExpiryClose:
		return rpc.EdgeReplayRuleExpiryClose
	case risk.OptionExitActionProfitTake:
		return rpc.EdgeReplayRuleProfitTrail
	case risk.OptionExitActionProfitTrail:
		s.highWater[c.conid] = risk.OptionExitHighWater{PerShare: decision.HighWater, CostPremium: decision.CostPremium, Quantity: quantity}
	}
	return ""
}

// recoverCost sells, once per buy lot, enough contracts to recover the lot's
// cost after the lot's value reaches the harvest gain over that cost; the
// rest runs.
func (s *replaySim) recoverCost(c *replayContract, day string, mark, fx float64) {
	value := mark * c.mult
	net := value*(1-s.r.halfSpread) - s.r.commBase/fx
	for _, entry := range s.held[c.conid] {
		if s.recovered[entry.lot.id] || len(entry.units) == 0 {
			continue
		}
		cost := float64(entry.lot.qty) * entry.lot.costPerContract
		if net <= 0 || cost <= 0 || value*float64(len(entry.units)) < (1+s.r.in.Policy.RecoverCostGainPct/100)*cost {
			continue
		}
		n := min(len(entry.units), int(math.Ceil(cost/net)))
		s.recovered[entry.lot.id] = true
		s.sell(c, append([]*replayUnit{}, entry.units[:n]...), day, mark, fx, rpc.EdgeReplayRuleRecoverCost)
	}
}

// stockTrail closes a buy lot when the underlying closes more than the
// policy's multiple of its 20-session average true range below its highest
// close since the lot was bought. A day the range cannot be measured is
// counted, never skipped silently.
func (s *replaySim) stockTrail(c *replayContract, day string, mark, spot, fx float64) {
	atr, closeOnly, ok := s.r.atr(c.underlying, day)
	if !ok {
		s.run.Coverage.TrailUnmeasuredDays++
		return
	}
	for _, d := range closeOnly {
		if key := c.underlying + "\x00" + d; !s.closeOnly[key] {
			s.closeOnly[key] = true
			s.run.Coverage.CloseOnlyRangeDays++
		}
	}
	for _, entry := range s.held[c.conid] {
		if len(entry.units) == 0 {
			continue
		}
		high, ok := s.r.highestClose(c.underlying, entry.lot.day, day)
		if ok && spot < high-s.r.in.Policy.StockTrailRangeMultiple*atr {
			s.sell(c, append([]*replayUnit{}, entry.units...), day, mark, fx, rpc.EdgeReplayRuleStockTrail)
		}
	}
}

// trim sells one contract at a time, the largest holding on the name first,
// until the name's long options are worth no more than the Rulebook's name
// cap of that day's NLV. Only contracts marked that day can be sold.
func (s *replaySim) trim(symbol, day string) {
	nlv, ok := s.r.nlv[day]
	if !ok || nlv <= 0 {
		return
	}
	limit := s.r.in.Policy.OptionNameActPct / 100 * nlv
	trimmed := map[int64]bool{}
	for {
		value := 0.0
		var best int64
		bestValue := -1.0
		for _, conid := range s.r.bySymbol[symbol] {
			c := s.r.contracts[conid]
			n := s.count(conid)
			last, ok := s.lastMark[conid]
			fx, haveFX := s.r.fx(c.currency, day)
			if n == 0 || !ok || !haveFX {
				continue
			}
			line := last.price * c.mult * fx * float64(n)
			value += line
			if last.day == day && line > bestValue {
				best, bestValue = conid, line
			}
		}
		if value <= limit || bestValue < 0 {
			break
		}
		c := s.r.contracts[best]
		var unit *replayUnit
		for _, entry := range s.held[best] {
			if len(entry.units) > 0 {
				unit = entry.units[0]
				break
			}
		}
		fx, _ := s.r.fx(c.currency, day)
		s.sellQuiet(c, []*replayUnit{unit}, day, s.lastMark[best].price, fx, rpc.EdgeReplayRuleTrimToCap)
		if !trimmed[best] {
			trimmed[best] = true
			s.reason(rpc.EdgeReplayRuleTrimToCap).Events++
			s.run.Exits++
		}
	}
}

func (s *replaySim) reason(name string) *rpc.EdgeReplayReason {
	row := s.reasons[name]
	if row == nil {
		row = &rpc.EdgeReplayReason{Reason: name}
		s.reasons[name] = row
	}
	return row
}

// sell closes units at the day's mark less the half-spread and the average
// commission; one call is one exit event.
func (s *replaySim) sell(c *replayContract, units []*replayUnit, day string, mark, fx float64, reason string) {
	s.sellQuiet(c, units, day, mark, fx, reason)
	s.reason(reason).Events++
	s.run.Exits++
}

func (s *replaySim) sellQuiet(c *replayContract, units []*replayUnit, day string, mark, fx float64, reason string) {
	net := math.Max(mark*c.mult*(1-s.r.halfSpread)*fx-s.r.commBase, 0)
	for _, unit := range units {
		s.outcomes[unit] = replayOutcome{day: day, base: net, kind: replayOutcomeRule, reason: reason}
	}
	s.reason(reason).Units += float64(len(units))
	s.remove(c.conid, units)
}

func (s *replaySim) remove(conid int64, units []*replayUnit) {
	gone := map[*replayUnit]bool{}
	for _, unit := range units {
		gone[unit] = true
	}
	for _, entry := range s.held[conid] {
		entry.units = slices.DeleteFunc(entry.units, func(u *replayUnit) bool { return gone[u] })
	}
}

// keepActual gives every contract still held the owner's own outcome: the
// replay cannot test a day without a recorded price.
func (s *replaySim) keepActual(conid int64, event bool) {
	units := s.heldUnits(conid)
	for _, unit := range units {
		s.outcomes[unit] = replayOutcome{day: unit.actDay, base: unit.actBase, kind: replayOutcomeKeptReal, reason: unit.actKind}
	}
	if event {
		s.run.Coverage.UntestableEvents++
	}
	s.run.Coverage.UnitsKeptActual += float64(len(units))
	s.held[conid] = nil
}

// settleExpiry settles a contract still held on its expiry day at intrinsic
// value on the underlying's close, less commission when it pays, when it was
// marked that day or the session before.
func (s *replaySim) settleExpiry(c *replayContract, day, prev string) {
	if c.expiry != day && !(c.expiry > prev && c.expiry < day) {
		return
	}
	units := s.heldUnits(c.conid)
	last, ok := s.lastMark[c.conid]
	if len(units) == 0 || !ok || last.day != day && last.day != prev {
		return
	}
	spot, haveSpot := s.r.closeAtOrBefore(c.underlying, c.expiry)
	fx, haveFX := s.r.fx(c.currency, day)
	if !haveSpot || !haveFX {
		return
	}
	value := replayIntrinsic(c.right, spot, c.strike) * c.mult * fx
	if value > 0 {
		value = math.Max(value-s.r.commBase, 0)
	}
	for _, unit := range units {
		s.outcomes[unit] = replayOutcome{day: day, base: value, kind: replayOutcomeSettle, reason: "expiry"}
	}
	s.held[c.conid] = nil
}

// reconcile ties the actual baseline to the broker: the units' total against
// the window's cash flows and start and end values, and the contracts Edge
// proves flat to flat inside the window against Edge's own option cycles.
func (r *replay) reconcile(actualPNL float64) rpc.EdgeReplayReconciliation {
	out := rpc.EdgeReplayReconciliation{ActualUnitsBase: actualPNL, ReconciliationNotes: []string{}, NotCompared: []rpc.EdgeReplayReconContract{}, ResidualContracts: []rpc.EdgeReplayReconContract{}}
	realized, gaps := 0.0, 0
	for _, c := range r.contracts {
		for _, trade := range c.executions {
			fx, _ := r.executionFX(trade)
			out.FlowsBase += *trade.NetCash * fx
			if trade.RealizedPNL == nil {
				gaps++
				continue
			}
			realized += *trade.RealizedPNL * fx
		}
	}
	for _, unit := range r.units {
		if unit.lot.pre {
			out.StartValueBase += unit.startBase
		}
		if unit.actKind == "open" || unit.actKind == "expiry_unbooked" {
			out.EndValueBase += unit.actBase
		}
	}
	out.ActualCashBase = out.FlowsBase + out.EndValueBase - out.StartValueBase
	out.BrokerRealizedBase, out.BrokerRealizedGaps = new(realized), gaps

	groups := groupTrades(r.ev.trades, r.base, r.ev.fxRates)
	mutations := buildMutations(groups, r.ev)
	byConID := map[int64][]mutation{}
	for _, item := range mutations {
		byConID[item.conid] = append(byConID[item.conid], item)
	}
	initial := initialPositions(groups, byConID, r.ev)
	unbalanced := unbalancedContracts(groups, byConID, r.ev)
	scoped := r.ev
	scoped.trades = nil
	symbols := map[string]int64{}
	ambiguous := map[string]bool{}
	for _, trade := range r.ev.trades {
		if _, ok := r.contracts[trade.ConID]; !ok {
			continue
		}
		scoped.trades = append(scoped.trades, trade)
		if other, seen := symbols[trade.Symbol]; seen && other != trade.ConID {
			ambiguous[trade.Symbol] = true
		}
		symbols[trade.Symbol] = trade.ConID
	}
	cycles := buildOptionCycles(scoped, parseDay(r.from), parseDay(r.to), r.base, nil, initial, unbalanced)
	out.EdgeCycles = cycles.CompletedCount
	type cycleSum struct {
		pnl        float64
		executions int
		complete   bool
	}
	byContract := map[int64]*cycleSum{}
	for _, cycle := range cycles.Cycles {
		conid, ok := symbols[cycle.Symbol]
		if !ok || ambiguous[cycle.Symbol] {
			continue
		}
		sum := byContract[conid]
		if sum == nil {
			sum = &cycleSum{complete: true}
			byContract[conid] = sum
		}
		sum.executions += cycle.ExecutionCount
		if cycle.RealizedPNLBase == nil || cycle.PNLStatus != OptionPNLComplete {
			sum.complete = false
			continue
		}
		sum.pnl += *cycle.RealizedPNLBase
	}
	out.EdgeCycleContracts = len(byContract)
	outcomes := map[int64]float64{}
	for _, unit := range r.units {
		outcomes[unit.lot.c.conid] += unit.actBase - unit.startBase
	}
	tolerance := 0.01 * float64(max(1, len(r.units)))
	for _, conid := range slices.Sorted(maps.Keys(byContract)) {
		sum, c := byContract[conid], r.contracts[conid]
		if reason := r.notComparedReason(c, sum.complete, sum.executions); reason != "" {
			out.NotCompared = append(out.NotCompared, rpc.EdgeReplayReconContract{Underlying: c.underlying, Contract: replayContractLabel(c), Reason: reason})
			continue
		}
		out.ComparedContracts++
		out.EdgeCyclesPNLBase += sum.pnl
		out.ActualOnCyclesBase += outcomes[conid]
		gap := 0.0
		for _, trade := range c.executions {
			fx, _ := r.executionFX(trade)
			realizedRow := 0.0
			if trade.RealizedPNL != nil {
				realizedRow = *trade.RealizedPNL
			}
			out.FXTranslationBase += (*trade.NetCash - realizedRow) * fx
			gap += *trade.NetCash - realizedRow
		}
		out.CycleCashResidual += gap
		if math.Abs(gap) > 0.01*float64(max(1, c.units())) {
			out.ResidualContracts = append(out.ResidualContracts, rpc.EdgeReplayReconContract{Underlying: c.underlying, Contract: replayContractLabel(c), Reason: rpc.EdgeReplayResidualCashVsRealized})
		}
	}
	out.ResidualBase = out.ActualOnCyclesBase - out.EdgeCyclesPNLBase - out.FXTranslationBase
	out.Reconciled = math.Abs(out.ActualUnitsBase-out.ActualCashBase) <= tolerance && math.Abs(out.ResidualBase) <= tolerance && math.Abs(out.CycleCashResidual) <= tolerance
	switch {
	case !out.Reconciled:
		out.Status = rpc.EdgeReplayUnreconciled
	case len(out.NotCompared) > 0:
		out.Status = rpc.EdgeReplayReconciledPartly
	default:
		out.Status = rpc.EdgeReplayReconciledFully
	}
	out.ReconciliationNotes = append(out.ReconciliationNotes,
		"Actual by units equals the window's broker cash flows plus the end value of contracts still held, less the start value of contracts held at the start.",
		"On contracts Edge proves flat to flat inside the window, the actual baseline equals Edge's realized P/L plus FX translation: Edge converts the broker's realized P/L at each closing execution's rate, the replay converts every cash flow at its own rate. In each contract's own currency its cash flows equal the broker's realized P/L.")
	if len(out.NotCompared) > 0 {
		out.ReconciliationNotes = append(out.ReconciliationNotes, "Contracts with an Edge cycle are not compared when they were held at the start, also traded outside the cycle, lack complete broker P/L, or closed by exercise or assignment, where the broker's realized P/L on the option carries value moved to or from the stock that no option cash flow shows; not_compared names each.")
	}
	return out
}

// notComparedReason says why a contract with an Edge cycle cannot be
// compared with it, or "" when it can.
func (r *replay) notComparedReason(c *replayContract, complete bool, executions int) string {
	switch {
	case c.preheld:
		return rpc.EdgeReplayNotComparedHeldAtStart
	case executions != len(c.executions):
		return rpc.EdgeReplayNotComparedOutsideCycle
	case !complete:
		return rpc.EdgeReplayNotComparedBrokerPNL
	}
	for _, trade := range c.executions {
		if kind := r.lifecycle[lifecycleTradeKey(trade.ConID, trade.TradeID)]; kind == "exercise" || kind == "assignment" {
			return rpc.EdgeReplayNotComparedExercised
		}
	}
	return ""
}

func (c *replayContract) units() int {
	n := 0
	for _, lot := range c.lots {
		n += len(lot.units)
	}
	return n
}

// replayContractLabel names a contract for a reader: underlying, expiry,
// strike and right.
func replayContractLabel(c *replayContract) string {
	return fmt.Sprintf("%s %s %s %s", c.underlying, c.expiry, strconv.FormatFloat(c.strike, 'f', -1, 64), c.right)
}

func exitReplayParameters(in ExitReplayInput) rpc.EdgeReplayParameters {
	pol := in.Policy
	params := rpc.EdgeReplayParameters{
		Rules: []rpc.EdgeReplayParameter{
			{Name: "loss_exit_pct", Value: pol.Exit.LossExitPct, Unit: "pct", Source: "Rulebook exit_act_loss_pct"},
			{Name: "expiry_close_dte", Value: float64(pol.Exit.ExpiryCloseDTE), Unit: "days", Source: "Rulebook runway_act_dte"},
			{Name: "profit_arm_gain_pct", Value: pol.Exit.ProfitArmGainPct, Unit: "pct", Source: "protection buckets.trailing_stop.options.profit_arm_gain_pct"},
			{Name: "profit_trail_pct", Value: pol.Exit.ProfitTrailPct, Unit: "pct", Source: "protection buckets.trailing_stop.options.default_pct"},
			{Name: "locked_gain_pct", Value: pol.Exit.LockedGainPct, Unit: "pct", Source: "protection buckets.trailing_stop.options.locked_gain_pct"},
			{Name: "profit_trail_min_dte", Value: float64(pol.Exit.MinDTE), Unit: "days", Source: "protection buckets.trailing_stop.options.min_dte"},
			{Name: "min_trail_abs", Value: pol.Exit.MinTrailAbs, Unit: "price", Source: "protection buckets.trailing_stop.options.min_trail_abs"},
			{Name: "recover_cost_gain_pct", Value: pol.RecoverCostGainPct, Unit: "pct", Source: "protection buckets.trailing_stop.options.recover_cost_gain_pct"},
			{Name: "option_name_act_pct", Value: pol.OptionNameActPct, Unit: "pct_nlv", Source: "Rulebook option_name_act_pct"},
			{Name: "stock_trail_range_multiple", Value: pol.StockTrailRangeMultiple, Unit: "atr", Source: "protection buckets.trailing_stop.options.stock_trail_range_multiple"},
			{Name: "stock_trail_atr_sessions", Value: ExitReplayATRSessions, Unit: "sessions", Source: "stock_trail_range_multiple definition: 20-session average true range"},
		},
		HalfSpreadSamples: in.HalfSpreadSamples, HalfSpreadSource: in.HalfSpreadSource, PolicySource: pol.Source,
	}
	if in.HalfSpreadPct != nil {
		params.HalfSpreadPct = new(*in.HalfSpreadPct)
	}
	return params
}

func exitReplayMethod(historySource string) rpc.EdgeReplayMethod {
	history := fmt.Sprintf("An underlying whose daily history does not span the statement days its options were held, within %d calendar days at either end, leaves every run, the actual one included, and is listed with the reason and your own P/L on it; its days are never kept at your outcome unannounced.", ExitReplayHistorySlackDays)
	if source := strings.TrimSpace(historySource); source != "" {
		history = source + " " + history
	}
	return rpc.EdgeReplayMethod{
		Evaluation: "Rules are evaluated once per statement day at that day's recorded closing mark: the Open Positions mark, else an execution row's closing price, else the mid of a two-sided recorded chain quote. Today's loss line, expiry close and profit trail run through risk.EvaluateOptionExit with the mark as bid and ask.",
		History:    history,
		Exits:      "A rule exit sells at the mark less the measured half-spread and the owner's average commission per contract, converted at the day's broker rate.",
		Untestable: "In the recorded run a held contract on a day without a recorded mark, or on a session inside its underlying's history without a bar, keeps the owner's actual outcome from that day on. A statement day on which the underlying's exchange held no session by Canary's official exchange calendar carries the previous close and option mark; no rule acts on it, and such contract-days are counted.",
		Modelled:   "The modelled run fills such days with Black-Scholes at the underlying close, the volatility backed out of the last recorded mark, rate zero; a volatility that cannot be backed out prices at intrinsic value.",
		HighWater:  "The profit trail's high water is carried from day to day and resets when the cost basis or quantity changes, as the daemon's does; the daemon carries it within a session only.",
		Harvest:    "Recover cost sells, once per buy lot, enough contracts to recover the lot's cost when the lot's value reaches the harvest gain; trim sells the largest holding on a name one contract at a time until the name's long options are worth no more than the name cap of that day's NLV.",
		StockTrail: "A buy lot closes when its underlying closes more than the multiple of its 20-session average true range below its highest close since the lot was bought. A daily bar without a high and low enters the range as the move from the previous close, the true range's close-only form, and is counted; a day with fewer than 21 sessions of history cannot measure the range and is counted as unmeasured.",
		Actual:     "Set A is the owner's actual path: buys at cost, sales at their proceeds, contracts still held at the end at the end day's mark, contracts held at the start at the start day's mark.",
		Exclusions: "Long puts on a Rulebook hedge symbol and short options are excluded; an exercise or assignment counts the option leg only, as Edge does.",
	}
}

func parseDay(day string) time.Time {
	value, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return time.Time{}
	}
	return value
}

func nextDayKey(day string) string {
	return dayKey(parseDay(day).AddDate(0, 0, 1))
}

type replayDayPrice struct {
	day   string
	price float64
}

// ExitReplayUnderlying is one underlying whose daily history the replay
// needs, from First (its first option trade or position in range) through
// Last (its last contract's expiry or the range's end, whichever is earlier).
type ExitReplayUnderlying struct {
	Symbol   string
	Currency string
	First    time.Time
	Last     time.Time
}

// ExitReplayUnderlyings lists the underlyings of the options traded or held
// between from and to (zero bounds are open), long puts on a hedge symbol
// left out, so a caller reads only the history the replay can use.
func ExitReplayUnderlyings(statements []flexstmt.Statement, from, to time.Time, hedgeSymbols []string) []ExitReplayUnderlying {
	ev, err := collectEvidence(statements)
	if err != nil {
		return nil
	}
	instruments := map[int64]flexstmt.Instrument{}
	for _, item := range ev.instruments {
		instruments[item.ConID] = item
	}
	inRange := func(day time.Time) bool {
		return (from.IsZero() || !day.Before(dayStart(from).AddDate(0, 0, -10))) && (to.IsZero() || !day.After(dayStart(to)))
	}
	found := map[string]*ExitReplayUnderlying{}
	unknownExpiry := map[string]bool{}
	latest := time.Time{}
	add := func(conid int64, underlying, currency string, day time.Time) {
		inst := instruments[conid]
		symbol := strings.ToUpper(firstNonEmpty(inst.UnderlyingSymbol, underlying))
		day = dayStart(day)
		if symbol == "" || optionPutCall(inst.PutCall) == "put" && slices.ContainsFunc(hedgeSymbols, func(s string) bool { return strings.EqualFold(strings.TrimSpace(s), symbol) }) {
			return
		}
		u := found[symbol]
		if u == nil {
			u = &ExitReplayUnderlying{Symbol: symbol, Currency: strings.ToUpper(firstNonEmpty(currency, inst.Currency)), First: day}
			found[symbol] = u
		}
		if day.Before(u.First) {
			u.First = day
		}
		expiry := parseDay(optionExpiry(inst.Expiry))
		unknownExpiry[symbol] = unknownExpiry[symbol] || expiry.IsZero()
		u.Last = maxTime(u.Last, expiry)
	}
	for _, trade := range ev.trades {
		if strings.EqualFold(trade.AssetClass, "OPT") && trade.ConID != 0 && inRange(tradeStatementDay(trade)) {
			add(trade.ConID, trade.UnderlyingSymbol, trade.Currency, tradeStatementDay(trade))
		}
	}
	for _, row := range ev.positions {
		if strings.EqualFold(row.AssetClass, "OPT") && row.ConID != 0 && !row.ReportDate.IsZero() && inRange(row.ReportDate) {
			add(row.ConID, row.UnderlyingSymbol, row.Currency, row.ReportDate)
		}
	}
	// The range ends at to, or at the latest statement day when it is open;
	// no contract is needed past its expiry.
	for _, st := range statements {
		if !st.ToDate.IsZero() && inRange(st.ToDate) {
			latest = maxTime(latest, dayStart(st.ToDate))
		}
	}
	end := latest
	if !to.IsZero() {
		end = dayStart(to)
	}
	out := make([]ExitReplayUnderlying, 0, len(found))
	for _, symbol := range slices.Sorted(maps.Keys(found)) {
		u := *found[symbol]
		if unknownExpiry[symbol] || u.Last.After(end) {
			u.Last = end
		}
		out = append(out, u)
	}
	return out
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
