package daemon

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	brokerIBKR "github.com/osauer/canary/v3/internal/broker/ibkr"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The level replay (edge.levels) replays the owner's level rule on the
// five-minute bars of every past session he held a call on a stock
// (internal/edge/level_replay.go has the rule). It reads what the exit
// replay reads (the Flex statements and each name's retained daily history)
// and, beside it, the sessions' five-minute bars, which it keeps: one state
// document per name and month, never evicted, so a run tomorrow without the
// gateway gives the same answer. A session it lacks is read from the broker
// in the background, one window of at most seven days at a time, and the run
// answers with what it holds and counts the rest as pending.

const (
	stateKindLevelBars = "edge_level_bars"
	levelBarsDocVer    = 1
	// levelIndexSymbol is the index the cut's index reading uses: the S&P 500
	// fund, because the dated five-minute read serves US stocks and funds
	// only.
	levelIndexSymbol = "SPY"
	// levelFillWindow bounds one background fill of missing sessions;
	// levelReadTimeout one broker read.
	levelFillWindow  = 45 * time.Minute
	levelReadTimeout = 20 * time.Second
	// levelHorizonSessions is the second valuation's horizon, the one the
	// scorecard judges a decision at.
	levelHorizonSessions = 5
	// levelDailyLeadDays is how far before a name's first sample session its
	// daily history must reach: the 14-session average true range.
	levelDailyLeadDays = 30
)

const levelMethodEverySession = "Every session in the bounds for each name of the held sample (the US stocks and funds a long call was held on in any session the statements and the calendar cover), one contract each, as if a call had been held throughout. These are the names held since, so in an earlier period they are its survivors, and a name not listed then is left out. Before Canary's calendar a session is a day SPY has a recorded daily bar, at the regular hours, shortened where SPY's five-minute bars end early. The two halves are the sample's sessions in order, cut where half its stock-days lie before."

const levelMethodHistory = "Five-minute regular-session TRADES bars from the broker, kept by Canary once read (one document per name and month); the prior session's range, the average true range and the closes come from the daily history the exit replay keeps."

// levelBarsDoc is one name's five-minute bars of one month, by session. A
// session read with no bars is kept empty, so it is not read again.
type levelBarsDoc struct {
	Version  int                         `json:"version"`
	Symbol   string                      `json:"symbol"`
	Month    string                      `json:"month"`
	Sessions map[string]levelBarsSession `json:"sessions"`
}

type levelBarsSession struct {
	ReadAt time.Time     `json:"read_at"`
	Bars   []levelBarRow `json:"bars"`
}

type levelBarRow struct {
	T time.Time `json:"t"`
	O float64   `json:"o"`
	H float64   `json:"h"`
	L float64   `json:"l"`
	C float64   `json:"c"`
}

// levelFillStore holds the one background fill in flight; a run that starts
// while it goes on waits on it instead of starting another.
type levelFillStore struct {
	mu   sync.Mutex
	done chan struct{}
	// lastErr is the first broker failure of the last fill; daily names the
	// names whose daily history a fill has tried, and failed the names whose
	// five-minute read failed, so neither is waited on again.
	lastErr error
	daily   map[string]bool
	failed  map[string]bool
}

func (st *levelFillStore) dailyDone(symbol string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.daily[symbol]
}

// barsFailed reports a name whose five-minute read failed in a fill that has
// ended: its sessions are left out, not pending.
func (st *levelFillStore) barsFailed(symbol string) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.done == nil && st.failed[symbol]
}

func (st *levelFillStore) mark(set *map[string]bool, symbol string, on bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if *set == nil {
		*set = map[string]bool{}
	}
	if on {
		(*set)[symbol] = true
	} else {
		delete(*set, symbol)
	}
}

// state says why days are still pending: the fill goes on, or it stopped.
func (st *levelFillStore) state() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch {
	case st.done != nil:
		return "the broker is still sending them; the read goes on after this answer"
	case st.lastErr != nil:
		return "a broker read failed (" + st.lastErr.Error() + "); the next run reads again"
	}
	return ""
}

func levelBarsScope(symbol, month string) string {
	sum := sha256.Sum256([]byte("STK|" + strings.ToUpper(symbol) + "|USD"))
	return "edge/levels/bars/" + hex.EncodeToString(sum[:]) + "/" + month
}

func (s *Server) handleEdgeLevels(ctx context.Context, req *rpc.Request) (*rpc.EdgeLevelsResult, error) {
	var params rpc.EdgeLevelsParams
	if err := decodeFor(api.EdgeLevels, req.Params, &params); err != nil {
		return nil, err
	}
	params, err := rpc.NormalizeEdgeLevelsParams(params)
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	result := &rpc.EdgeLevelsResult{SchemaVersion: rpc.EdgeLevelsSchemaVersion, State: rpc.EdgeStateCurrent, NotExecution: true, Method: levelMethod(),
		Looks: []rpc.EdgeLevelsLook{}}
	stateOnly := func(state, reason string) *rpc.EdgeLevelsResult {
		result.State, result.Reason = state, reason
		return result
	}
	if s.cfg == nil || !s.cfg.Flex.Enabled || strings.TrimSpace(s.cfg.Flex.QueryID) == "" {
		return stateOnly(rpc.EdgeStateActionRequired, "flex_configuration_required"), nil
	}
	if s.coreStore == nil {
		return nil, fmt.Errorf("edge authority is unavailable")
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return stateOnly(rpc.EdgeStateUnavailable, "account_scope_unavailable"), nil
	}
	loadCtx, cancel := context.WithTimeout(ctx, edgeStatementLoadTimeout)
	projected, err := s.loadEdgeProjectionEvidence(loadCtx, scope, edgeScopeFingerprint(scope))
	cancel()
	if err != nil {
		return nil, err
	}
	statements := projected.Statements
	if len(statements) == 0 {
		return stateOnly(rpc.EdgeStateBackfilling, "statement_account_pending"), nil
	}
	now := s.edgeNow()
	sessions := levelSessions(params, now)
	isIndex := func(symbol string) bool { _, ok := rpc.IndexUnderlyingContract(symbol); return ok }
	// The daily history: what the exit replay retains, read without the
	// broker; a name it does not span is read in the background fill.
	historyRange := ""
	dailySpan := func(symbol, firstSession, lastSession string) (*replayRead, bool) {
		first := parseLevelDay(firstSession).AddDate(0, 0, -levelDailyLeadDays)
		last := parseLevelDay(lastSession).AddDate(0, 0, 10)
		if yesterday := now.AddDate(0, 0, -1); last.After(yesterday) {
			last = yesterday
		}
		read := s.replayHistoryHeld(ctx, edgecore.ExitReplayUnderlying{Symbol: symbol, Currency: "USD", First: first, Last: last}, historyRange)
		return read, read.note.Reason != "" || edgecore.ExitReplayHistoryCovers(levelRanged(read.merged), first, last)
	}
	sample, excluded := edgecore.LevelReplaySample(statements, sessions, isIndex)
	recorded := false
	if params.EverySession {
		// The names are the ones of the whole held sample; the sessions are
		// the calendar's, or before it the days the index has a daily bar.
		result.EverySession, result.Method.Sample = true, levelMethodEverySession
		held, _ := edgecore.LevelReplaySample(statements, levelSessions(rpc.EdgeLevelsParams{}, now), isIndex)
		names := map[string]bool{}
		for _, d := range held {
			names[d.Symbol] = true
		}
		if to := cmp.Or(params.To, now.Format(time.DateOnly)); levelBeforeCalendar(params.From, now) {
			recorded = true
			historyRange = replayHistoryRange(nil, parseLevelDay(params.From), now)
			read, ok := dailySpan(levelIndexSymbol, params.From, to)
			if !ok {
				wait, cancelWait := context.WithTimeout(ctx, replayHistoryBudget(ctx))
				select {
				case <-s.startLevelFill(ctx, []*replayRead{read}, nil):
				case <-wait.Done():
				}
				cancelWait()
				if read, ok = dailySpan(levelIndexSymbol, params.From, to); !ok {
					return stateOnly(rpc.EdgeStateBackfilling, "index_history_pending"), nil
				}
			}
			sessions = levelRecordedSessions(levelRanged(read.merged), params.From, to, now)
		}
		sample, excluded = edgecore.LevelEverySession(slices.Sorted(maps.Keys(names)), sessions), map[string]int{}
	}
	common := maps.Clone(excluded)
	if len(sample) == 0 {
		return stateOnly(rpc.EdgeStateInsufficient, "no_call_held_in_the_sessions"), nil
	}

	bySymbol := map[string][]edgecore.LevelStockDay{}
	for _, d := range sample {
		bySymbol[d.Symbol] = append(bySymbol[d.Symbol], d)
	}
	symbols := slices.Sorted(maps.Keys(bySymbol))
	historyRange = replayHistoryRange(nil, parseLevelDay(sample[0].Session.Date), now)
	dailyRead := func(symbol string) (*replayRead, bool) {
		days := bySymbol[symbol]
		return dailySpan(symbol, days[0].Session.Date, days[len(days)-1].Session.Date)
	}
	var dailyMissing []*replayRead
	for _, symbol := range symbols {
		if read, ok := dailyRead(symbol); !ok {
			dailyMissing = append(dailyMissing, read)
		}
	}

	// The five-minute bars: what Canary keeps, and the fill for the rest.
	need := map[string][]edgecore.LevelSession{}
	for _, d := range sample {
		need[d.Symbol] = append(need[d.Symbol], d.Session)
		need[levelIndexSymbol] = append(need[levelIndexSymbol], d.Session)
	}
	missing := s.levelMissing(ctx, need)
	if len(missing) > 0 || len(dailyMissing) > 0 {
		wait, cancelWait := context.WithTimeout(ctx, replayHistoryBudget(ctx))
		select {
		case <-s.startLevelFill(ctx, dailyMissing, missing):
		case <-wait.Done():
		}
		cancelWait()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	daily, dailyPending := map[string][]edgecore.UnderlyingBar{}, map[string]bool{}
	for _, symbol := range symbols {
		read, ok := dailyRead(symbol)
		daily[symbol] = read.merged
		dailyPending[symbol] = !ok && !s.levelFill.dailyDone(symbol)
	}
	held := s.levelRetained(ctx, need)
	result.PendingReason = s.levelFill.state()

	in := edgecore.LevelReplayInput{Index: map[string][]edgecore.LevelBar{}}
	if !params.EverySession {
		// The calls he held, their recorded prices and his own trades: the
		// replay in money. An every-session run has no held calls to price.
		first, last := parseLevelDay(sample[0].Session.Prior), parseLevelDay(sample[len(sample)-1].Session.Date).AddDate(0, 0, 3*levelHorizonSessions)
		in.Book = edgecore.NewLevelOptionBook(statements, s.replayChainMarks(ctx, symbols, first, last))
	}
	for _, session := range need[levelIndexSymbol] {
		if bars, ok := held[levelIndexSymbol][session.Date]; ok {
			in.Index[session.Date] = bars
		}
	}
	for _, d := range sample {
		if recorded {
			d.Session = levelRecordedClose(d.Session, in.Index[d.Session.Date])
		}
		bars, ok := held[d.Symbol][d.Session.Date]
		if !ok && s.levelFill.barsFailed(d.Symbol) {
			common[edgecore.LevelExcludedNoBars]++
			continue
		}
		if !ok || dailyPending[d.Symbol] {
			result.Pending++
			continue
		}
		in.Days = append(in.Days, levelDayInput(d, daily[d.Symbol], bars))
	}
	for _, minutes := range rpc.EdgeLevelsLookMinutes {
		in.LookMinutes = minutes
		out := edgecore.LevelReplay(in)
		look := rpc.EdgeLevelsLook{LookMinutes: minutes, Sample: rpc.EdgeLevelsSample{Rows: map[string]int{}, Excluded: maps.Clone(common)}}
		for reason, n := range out.Excluded {
			look.Sample.Excluded[reason] += n
		}
		look.Summary, look.Counts, look.Split = edgecore.LevelReplaySummary(out)
		look.Days = out.Days
		look.Insurance, look.Entries, look.Option = edgecore.LevelReplayInsurance(out.Days), edgecore.LevelReplayEntries(out.Days), edgecore.LevelReplayOptionShare(out.Days)
		look.Actual = edgecore.LevelReplayActual(out.Days)
		if look.Insurance == nil {
			look.Insurance = []rpc.EdgeLevelsInsurance{}
		}
		if look.Days == nil {
			look.Days = []rpc.EdgeLevelsDay{}
		}
		if look.Summary == nil {
			look.Summary = []rpc.EdgeLevelsSum{}
		}
		if look.Counts == nil {
			look.Counts = []rpc.EdgeLevelsTally{}
		}
		sessionSet, nameSet := map[string]bool{}, map[string]bool{}
		for _, d := range out.Days {
			sessionSet[d.Session], nameSet[d.Symbol] = true, true
			look.Sample.Rows[d.Row]++
			if d.Later != nil {
				look.Sample.Rows[rpc.EdgeLevelsRowWatchedLater]++
			}
			if result.From == "" || d.Session < result.From {
				result.From = d.Session
			}
			result.To = max(result.To, d.Session)
		}
		look.Sample.StockDays, look.Sample.Sessions, look.Sample.Names = len(out.Days), len(sessionSet), len(nameSet)
		result.Looks = append(result.Looks, look)
	}
	result.Incomplete = result.Pending > 0
	if !result.Incomplete {
		result.PendingReason = ""
	}
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return stateOnly(rpc.EdgeStateUnavailable, "account_scope_changed"), nil
	}
	if err := rpc.ValidateEdgeLevelsResult(*result); err != nil {
		return nil, fmt.Errorf("invalid Edge level replay: %w", err)
	}
	return result, nil
}

func levelMethod() rpc.EdgeLevelsMethod {
	return rpc.EdgeLevelsMethod{
		Rule: []string{
			"The moment is the look, the close of the five-minute bar that ends 30 or 60 minutes after the open, each look reported on its own; a watched row whose stock closes a bar under the prior low later, before the last hour, is drafted again at that close.",
			"The row is the price at the look against the prior session's daily range, as the market read's held rows place it: under the low losing, inside watched, above the high working. The second reading drafts every call whose stock is under the prior close at the look.",
			"Levels are frozen at the moment from the bars completed by then: the cut level is the day's low so far; the trims are the day's midpoint and the prior low, each only while above the price, nearer first; the stop level is the cut level less the 14-session average true range through the prior session (the design's reading of one average daily range).",
			"Halves: one trim of half at the nearer trim level. Thirds: a third at each trim level. The cut or the stop sells the rest.",
			"A trim sells when a bar trades at its level, at the level or at the bar's open when it opened above; the stop likewise on the way down; when one bar reaches both, the one nearer its open comes first.",
			"The cut sells at the second of two consecutive completed five-minute closes under the cut level, at that close.",
			"The last-hour sale: at the first bar of the session's last hour, if no sale has fired, the first trim's size at that bar's open.",
			"The buy back: after the cut or the stop sold, two consecutive closes above the cut level buy back what they sold, at the second close, once per day; after it no trim and no last-hour sale, and a later cut or stop sale is final.",
			"The cut with the index agreeing: the rule, its cut sold only at a close where SPY's bar closed under SPY's own low through the look; no buy back.",
			"Reduced at once: the first trim's size sold at the moment's price.",
			"The rule with the nearer trim: the rule, its first trim a quarter of the way up the day's range at the moment (low plus a quarter of high less low) in place of the midpoint; the prior low stays the second trim level, the nearer one first.",
			"Each result is the position's value against its value at the moment, in percent, at the session's daily close and at the daily close five sessions on. These versions were fixed before the first run on the owner's sessions; nothing is tuned to the sample.",
		},
		Prices:  "The moment's price and every valuation are recorded prices (a five-minute close, a daily close). Every sale and buy back price is modelled: the level, or the bar's open or close the rule names. Stock prices only; no option value, no spread or fee.",
		Sample:  "Every regular session in Canary's exchange calendar (from its first covered day) on which the Open Positions statement of the prior session held a long call on a US stock or fund; a stock with a short option against it is left alone, as the day plan leaves it, and so is a cash index. The two halves are the sample's sessions in order, cut where half its stock-days lie before.",
		Index:   "SPY five-minute bars: the dated five-minute read serves US stocks and funds only, so the index is read through the S&P 500 fund.",
		History: levelMethodHistory,
		Money:   "Each result is also valued on the long calls held on the stock at the prior session's close, in percent of their value at the moment. A recorded option price is the statement's mark, a trade's closing price or the recorder's mid of that contract on that session, in that order; an end value is recorded when every held contract has one. The price at the moment is always modelled: Black-Scholes at the moment's stock price with the volatility backed out of the contract's recorded price at the prior close, rate zero. A price during the session, and an end value that is not recorded, is the price at the moment plus that model's delta at the moment times the stock's move since, never below zero; a contract expired by the valuation day without a recorded price is valued at intrinsic value. A day whose held calls have no recorded price at the prior close, or a price no volatility explains, has no option result. Actual is the position after the owner's trades in the stock's calls that session (the statement's execution rows, hand trades among them, whenever in the session they were made) at the trades' own prices, against the same modelled value at the moment; a session with an expiry, exercise or assignment is counted and not valued, and a day whose position after the trades is not the session statement's is kept out of the aggregate. The insurance rows and the Actual rows on the calls are weighted by the calls' value at the moment, each with the largest single day's share of that weight; the mean and the median of the percentages stand beside them.",
		Test: []string{
			"A cut is a fall that turned when the stock closed that session above the cut level, and a fall that kept going when it closed at or under it. The cost in the first and the saving in the second are the version's result less held's on those days, per cut, at the close and five sessions on. The worst loss is the lowest result over every drafted day of the group, held and with the plan.",
			"The buy back on a later session: after the rule's cut (in halves), the first daily close above the cut level within the five sessions after the cut's session, which itself does not count. Bought once, at that close, and judged five sessions after the buy against staying out. A cut with no such close is counted as never; one whose sessions have not all closed as pending.",
			"The owner's own adds are his buys to open, one per order and contract at the order's average price.",
			"The adds, read on every stock-day and each once per session: two consecutive completed five-minute closes above the prior session's high after the look; and the reclaim, on a session that opened under the prior session's low, two consecutive closes above that low after the look. Bought at the second close, only at a close where SPY's bar has closed at or above SPY's own low through the look. A third of the position, so each is judged on what was bought: the close five sessions on against the buy, against not adding. The owner's own adds are the calls he bought on the stock that session, each judged on its recorded price five sessions on against what he paid.",
		},
	}
}

// levelSessions lists the regular sessions in the calendar that have closed
// by now, within the requested bounds, each with the calendar's prior
// session. A session whose prior lies outside the calendar is left out.
func levelSessions(p rpc.EdgeLevelsParams, now time.Time) []edgecore.LevelSession {
	cal := marketcal.New()
	probe, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now, Days: 1})
	if err != nil || probe.CoverageStart == "" {
		return nil
	}
	start := parseLevelDay(probe.CoverageStart)
	if p.From != "" && parseLevelDay(p.From).After(start) {
		start = parseLevelDay(p.From)
	}
	end := now
	if p.To != "" && parseLevelDay(p.To).AddDate(0, 0, 1).Before(end) {
		end = parseLevelDay(p.To).AddDate(0, 0, 1)
	}
	var out []edgecore.LevelSession
	prior := ""
	for day := parseLevelDay(probe.CoverageStart); day.Before(end); day = day.AddDate(0, 0, 400) {
		res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: day.Format(time.DateOnly), Days: 400})
		if err != nil {
			break
		}
		for _, sess := range res.Sessions {
			if sess.Open.IsZero() || sess.State == marketcal.StateUnknown || sess.State == marketcal.StateHoliday || sess.State == marketcal.StateClosed {
				continue
			}
			if sess.Close.After(now) || !sess.Open.Before(end) {
				break
			}
			if prior != "" && !parseLevelDay(sess.Date).Before(start) {
				out = append(out, edgecore.LevelSession{Date: sess.Date, Open: sess.Open, Close: sess.Close, Short: sess.State == marketcal.StateEarlyClose, Prior: prior})
			}
			prior = sess.Date
		}
	}
	return out
}

// levelBeforeCalendar reports a first session that lies before the calendar's
// first covered day.
func levelBeforeCalendar(from string, now time.Time) bool {
	probe, err := marketcal.New().Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now, Days: 1})
	return err == nil && from != "" && probe.CoverageStart != "" && from < probe.CoverageStart
}

// levelRecordedSessions lists the sessions of a period the calendar does not
// cover from what was recorded: a session is a day the index has a daily bar,
// its prior the bar before it, its hours the regular 09:30 to 16:00 in New
// York. levelRecordedClose shortens a session the index stopped trading
// early.
func levelRecordedSessions(index []edgecore.UnderlyingBar, from, to string, now time.Time) []edgecore.LevelSession {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil
	}
	index = slices.Clone(index)
	slices.SortFunc(index, func(a, b edgecore.UnderlyingBar) int { return a.Day.Compare(b.Day) })
	var out []edgecore.LevelSession
	for i, bar := range index {
		date := bar.Day.Format(time.DateOnly)
		if i == 0 || date < from || date > to {
			continue
		}
		y, m, d := bar.Day.Date()
		session := edgecore.LevelSession{Date: date, Open: time.Date(y, m, d, 9, 30, 0, 0, loc), Close: time.Date(y, m, d, 16, 0, 0, 0, loc), Prior: index[i-1].Day.Format(time.DateOnly)}
		if session.Close.After(now) {
			break
		}
		out = append(out, session)
	}
	return out
}

// levelRecordedClose ends a recorded session where the index's last
// five-minute bar ended, when that is before the regular close: an early
// close the calendar would have named.
func levelRecordedClose(session edgecore.LevelSession, index []edgecore.LevelBar) edgecore.LevelSession {
	if len(index) == 0 {
		return session
	}
	last := index[0].Start
	for _, b := range index {
		if b.Start.After(last) {
			last = b.Start
		}
	}
	if end := last.Add(5 * time.Minute); end.Before(session.Close) {
		session.Close, session.Short = end, true
	}
	return session
}

// levelRanged keeps the daily bars that carry a high and a low: the rule
// reads the prior session's range and the average true range, which a
// close-only bar (the mandate's retained closes) cannot give.
func levelRanged(bars []edgecore.UnderlyingBar) []edgecore.UnderlyingBar {
	var out []edgecore.UnderlyingBar
	for _, b := range bars {
		if b.High > 0 && b.Low > 0 {
			out = append(out, b)
		}
	}
	return out
}

func parseLevelDay(day string) time.Time {
	t, _ := time.Parse(time.DateOnly, day)
	return t
}

// levelDayInput joins a sample day with its daily history and bars.
func levelDayInput(d edgecore.LevelStockDay, daily []edgecore.UnderlyingBar, bars []edgecore.LevelBar) edgecore.LevelDayInput {
	in := edgecore.LevelDayInput{Day: d, Bars: bars}
	idx := slices.IndexFunc(daily, func(b edgecore.UnderlyingBar) bool { return b.Day.Format(time.DateOnly) == d.Session.Date })
	priorIdx := slices.IndexFunc(daily, func(b edgecore.UnderlyingBar) bool { return b.Day.Format(time.DateOnly) == d.Session.Prior })
	if priorIdx >= 0 && daily[priorIdx].High > 0 && daily[priorIdx].Low > 0 {
		p := daily[priorIdx]
		in.Prior = rpc.MarketReadPrior{Date: d.Session.Prior, High: p.High, Low: p.Low, Close: p.Close}
		hist := make([]ibkrlib.HistoricalBar, 0, priorIdx+1)
		for _, b := range daily[:priorIdx+1] {
			hist = append(hist, ibkrlib.HistoricalBar{Time: b.Day, High: b.High, Low: b.Low, Close: b.Close})
		}
		in.ATR14 = technicalATR(hist, 14)
	} else {
		in.Unavailable = edgecore.LevelExcludedPriorBar
	}
	if idx < 0 || daily[idx].High <= 0 || daily[idx].Low <= 0 {
		if in.Unavailable == "" {
			in.Unavailable = edgecore.LevelExcludedDayBar
		}
		return in
	}
	in.DayHigh, in.DayLow, in.Close = daily[idx].High, daily[idx].Low, daily[idx].Close
	if h := idx + levelHorizonSessions; h < len(daily) {
		in.Close5, in.Close5Day = daily[h].Close, daily[h].Day.Format(time.DateOnly)
	}
	for _, b := range daily[idx+1 : min(len(daily), idx+1+2*levelHorizonSessions)] {
		in.Next = append(in.Next, edgecore.LevelClose{Day: b.Day.Format(time.DateOnly), Close: b.Close})
	}
	return in
}

// levelLoad reads the month documents of one name.
func (s *Server) levelLoad(ctx context.Context, symbol, month string) (levelBarsDoc, int64) {
	doc := levelBarsDoc{Version: levelBarsDocVer, Symbol: symbol, Month: month, Sessions: map[string]levelBarsSession{}}
	if s.coreStore == nil {
		return doc, 0
	}
	raw, ok, err := s.coreStore.GetStateDocument(ctx, levelBarsScope(symbol, month), stateKindLevelBars)
	if err != nil || !ok {
		return doc, 0
	}
	var saved levelBarsDoc
	if json.Unmarshal(raw.JSON, &saved) != nil || saved.Version != levelBarsDocVer || saved.Symbol != symbol || saved.Month != month || saved.Sessions == nil {
		return doc, raw.Revision
	}
	return saved, raw.Revision
}

// levelRetained returns the kept bars of the needed sessions, by name and
// session; a session read with no bars is present and empty.
func (s *Server) levelRetained(ctx context.Context, need map[string][]edgecore.LevelSession) map[string]map[string][]edgecore.LevelBar {
	out := map[string]map[string][]edgecore.LevelBar{}
	for symbol, sessions := range need {
		docs := map[string]levelBarsDoc{}
		out[symbol] = map[string][]edgecore.LevelBar{}
		for _, session := range sessions {
			month := session.Date[:7]
			doc, ok := docs[month]
			if !ok {
				doc, _ = s.levelLoad(ctx, symbol, month)
				docs[month] = doc
			}
			kept, ok := doc.Sessions[session.Date]
			if !ok {
				continue
			}
			bars := make([]edgecore.LevelBar, 0, len(kept.Bars))
			for _, b := range kept.Bars {
				bars = append(bars, edgecore.LevelBar{Start: b.T, Open: b.O, High: b.H, Low: b.L, Close: b.C})
			}
			out[symbol][session.Date] = bars
		}
	}
	return out
}

// levelMissing lists, by name, the needed sessions Canary does not keep.
func (s *Server) levelMissing(ctx context.Context, need map[string][]edgecore.LevelSession) map[string][]edgecore.LevelSession {
	held := s.levelRetained(ctx, need)
	out := map[string][]edgecore.LevelSession{}
	for symbol, sessions := range need {
		seen := map[string]bool{}
		for _, session := range sessions {
			if _, ok := held[symbol][session.Date]; ok || seen[session.Date] {
				continue
			}
			seen[session.Date] = true
			out[symbol] = append(out[symbol], session)
		}
		slices.SortFunc(out[symbol], func(a, b edgecore.LevelSession) int { return strings.Compare(a.Date, b.Date) })
		if len(out[symbol]) == 0 {
			delete(out, symbol)
		}
	}
	return out
}

// startLevelFill starts the background read of the missing daily histories
// and sessions, or returns the one in flight. It reads one name at a time,
// the daily history first, then windows of at most seven calendar days of
// five-minute bars, and keeps each as it lands. A name whose read fails is
// logged and passed over; the gateway going away ends the fill, and the next
// run starts again from what is kept.
func (s *Server) startLevelFill(ctx context.Context, daily []*replayRead, missing map[string][]edgecore.LevelSession) <-chan struct{} {
	st := &s.levelFill
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done != nil {
		return st.done
	}
	done := make(chan struct{})
	st.done, st.lastErr = done, nil
	go func() {
		var err error
		defer func() {
			st.mu.Lock()
			st.done, st.lastErr = nil, err
			st.mu.Unlock()
			close(done)
		}()
		fillCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), levelFillWindow)
		defer cancel()
		note := func(what, symbol string, e error) {
			if err == nil {
				err = e
			}
			s.levelFillStopped(what, symbol, e)
		}
		for _, read := range daily {
			if fillCtx.Err() != nil || s.brokerReads() == nil {
				note("daily bars", read.underlying.Symbol, fmt.Errorf("gateway unavailable"))
				return
			}
			if e := s.levelFillDaily(fillCtx, read); e != nil {
				note("daily bars", read.underlying.Symbol, e)
			}
			st.mark(&st.daily, read.underlying.Symbol, true)
		}
		for _, symbol := range slices.Sorted(maps.Keys(missing)) {
			if fillCtx.Err() != nil || s.setupSource() == nil {
				note("five-minute bars", symbol, fmt.Errorf("gateway unavailable"))
				return
			}
			e := s.levelFillSymbol(fillCtx, symbol, missing[symbol])
			if fillCtx.Err() != nil {
				// The fill's own window ran out mid-name: the name did not
				// fail, and the next run reads on from what is kept.
				note("five-minute bars", symbol, fmt.Errorf("the fill's %s window ran out", levelFillWindow))
				return
			}
			if e != nil {
				note("five-minute bars", symbol, e)
			}
			st.mark(&st.failed, symbol, e != nil)
		}
	}()
	return done
}

func (s *Server) levelFillStopped(what, symbol string, err error) {
	if s.logger != nil {
		s.logger.Warnf("edge levels: reading %s of %s failed: %v; the next run reads again", what, strings.ToUpper(symbol), err)
	}
}

// levelFillDaily reads a name's daily history through the broker seam and
// keeps it in the exit replay's retained history, merged with what that
// holds.
func (s *Server) levelFillDaily(ctx context.Context, read *replayRead) error {
	a := s.brokerReads()
	if a == nil {
		return fmt.Errorf("gateway unavailable")
	}
	now := s.edgeNow()
	contract, _, _, err := normaliseStockQuoteContract(read.contract)
	if err != nil {
		return err
	}
	days, interval, err := marketHistoryWindow(read.params.Range, now)
	if err != nil {
		return err
	}
	binding, ok := a.Capture()
	if !ok {
		return fmt.Errorf("gateway unavailable")
	}
	series, err := a.ChartBars(ctx, brokerIBKR.Instrument(contract), days, interval, levelReadTimeout)
	if err != nil {
		return err
	}
	if !a.Current(binding) {
		return fmt.Errorf("broker session changed during the history read")
	}
	read.sources = append(slices.Clone(read.sources), levelDailySource(series, now))
	s.saveReplayHistory(ctx, read.contract, read.retained, read.revision, mergeReplaySources(read.sources), read.sources)
	return nil
}

// levelDailySource is a daily read as a retained-history source, each bar
// labelled with its session date; a bar without a sane range is dropped.
func levelDailySource(series broker.BarSeries, asOf time.Time) replaySource {
	src := replaySource{asOf: asOf, bars: map[string]edgecore.UnderlyingBar{}}
	for _, b := range series.Bars {
		if b.Time.IsZero() || !validHistoryRange(b.Open, b.High, b.Low, b.Close) {
			continue
		}
		y, m, d := b.Time.UTC().Date()
		day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		src.bars[day.Format(time.DateOnly)] = edgecore.UnderlyingBar{Day: day, Close: b.Close, High: b.High, Low: b.Low}
	}
	return src
}

func (s *Server) levelFillSymbol(ctx context.Context, symbol string, sessions []edgecore.LevelSession) error {
	c := s.setupSource()
	if c == nil {
		return fmt.Errorf("gateway unavailable")
	}
	binding, ok := c.Capture()
	if !ok {
		return fmt.Errorf("gateway unavailable")
	}
	requested, _, _, err := normaliseStockQuoteContract(rpc.ContractParams{Symbol: symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"})
	if err != nil {
		return err
	}
	contract, err := s.resolveSetupContract(ctx, c, binding, brokerIBKR.Instrument(requested))
	if err != nil {
		return fmt.Errorf("contract resolution: %w", err)
	}
	for len(sessions) > 0 {
		window := []edgecore.LevelSession{sessions[0]}
		for _, next := range sessions[1:] {
			if next.Close.Sub(window[0].Open) > 7*24*time.Hour-time.Minute {
				break
			}
			window = append(window, next)
		}
		sessions = sessions[len(window):]
		if err := ctx.Err(); err != nil {
			return err
		}
		bars, err := c.SessionBars(ctx, contract, window[0].Open, window[len(window)-1].Close, levelReadTimeout)
		if err != nil {
			return err
		}
		if !c.Current(binding) {
			return fmt.Errorf("broker session changed")
		}
		readAt := s.edgeNow()
		byMonth := map[string]map[string]levelBarsSession{}
		for _, session := range window {
			kept := levelBarsSession{ReadAt: readAt, Bars: []levelBarRow{}}
			for _, b := range bars {
				if !b.Time.Before(session.Open) && b.Time.Before(session.Close) {
					kept.Bars = append(kept.Bars, levelBarRow{T: b.Time.UTC(), O: b.Open, H: b.High, L: b.Low, C: b.Close})
				}
			}
			month := session.Date[:7]
			if byMonth[month] == nil {
				byMonth[month] = map[string]levelBarsSession{}
			}
			byMonth[month][session.Date] = kept
		}
		for month, kept := range byMonth {
			if err := s.levelSave(ctx, symbol, month, kept); err != nil {
				return err
			}
		}
	}
	return nil
}

// levelSave merges sessions into a month document.
func (s *Server) levelSave(ctx context.Context, symbol, month string, sessions map[string]levelBarsSession) error {
	if s.coreStore == nil {
		return nil
	}
	for range 3 {
		doc, revision := s.levelLoad(ctx, symbol, month)
		maps.Copy(doc.Sessions, sessions)
		raw, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		if _, err = s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: levelBarsScope(symbol, month), Kind: stateKindLevelBars, ExpectedRevision: revision, JSON: raw}); err == nil {
			return nil
		}
	}
	return fmt.Errorf("keeping the bars of %s %s failed", strings.ToUpper(symbol), month)
}
