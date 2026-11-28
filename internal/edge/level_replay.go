package edge

import (
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The level replay (edge.levels): the owner's level rule for a losing call,
// replayed on the stock's five-minute bars of every past session he held a
// call on it. It sits beside the exit replay, whose clock is the statement
// day on option lots; this one's is the five-minute bar on the stock. It
// shares the exit replay's statements, snapshots and daily history, and the
// market read's position and midpoint (rpc.MarketReadHeldPosition,
// rpc.MarketReadDayMid).
//
// The versions are fixed here, before the first run on the owner's sessions,
// and nothing is tuned to the sample:
//
//   - The moment is the look, the close of the bar that ends 30 or 60 minutes
//     after the open; each look is run on its own. A watched row whose stock closes a bar under the
//     prior low later, before the last hour, is drafted at that bar's close.
//   - Levels are frozen at the moment from the bars completed by then: the cut
//     level is the day's low so far, the trims are the day's midpoint and the
//     prior low (once the stock trades under it), each only while above the
//     price, nearer first; the stop level is the cut level less the
//     14-session average true range through the prior session.
//   - Halves: one trim of half at the nearer trim level. Thirds: a third at
//     each trim level. The cut or the stop sells the rest.
//   - A trim sells when a bar trades at its level, at the level or the bar's
//     open when it opened above. The stop sells when a bar trades at its
//     level, at the level or the bar's open when it opened under. When one
//     bar reaches both, the one nearer its open comes first.
//   - The cut sells the rest at the second of two consecutive completed
//     closes under the cut level, at that close.
//   - The last-hour sale: at the first bar of the last hour, if no sale has
//     fired, the first trim's size (half or a third) at that bar's open.
//   - The buy back: after the cut or the stop sold, two consecutive closes
//     above the cut level buy back what they sold, at the second close; once
//     per day. After it no trim and no last-hour sale; the cut and the stop
//     still sell, and that sale is final.
//   - The cut with the index agreeing: the rule, its cut sold only at a close
//     where the index's bar closed under the index's own low through the
//     look (its first-hour low at the first-hour look); the
//     trims, the stop and the last-hour sale as in the rule; no buy back.
//   - Reduced at once: the first trim's size sold at the moment's price.
//   - Each result is the position's value against its value at the moment,
//     at the session's daily close and at the daily close five sessions on.

// LevelBar is one five-minute regular-session bar.
type LevelBar struct {
	Start                  time.Time
	Open, High, Low, Close float64
}

// LevelSession is one regular session of Canary's exchange calendar.
type LevelSession struct {
	Date        string
	Open, Close time.Time
	Short       bool
	// Prior is the calendar's previous session.
	Prior string
}

// LevelStockDay is one stock and session the owner held a long call on the
// stock at the prior session's close, with the contracts held then.
type LevelStockDay struct {
	Symbol    string
	Currency  string
	Session   LevelSession
	Contracts float64
}

// LevelEverySession is the sample of an every-session run: each name on each
// session, one contract, as if a call had been held throughout.
func LevelEverySession(names []string, sessions []LevelSession) []LevelStockDay {
	out := make([]LevelStockDay, 0, len(names)*len(sessions))
	for _, session := range sessions {
		for _, name := range names {
			out = append(out, LevelStockDay{Symbol: name, Currency: "USD", Session: session, Contracts: 1})
		}
	}
	return out
}

// Reasons a stock-day leaves the sample.
const (
	LevelExcludedNoPriorStatement = "no_statement_of_prior_session"
	LevelExcludedIndex            = "index_underlying"
	LevelExcludedNotUS            = "not_a_us_listing"
	LevelExcludedSpread           = "short_option_on_the_stock"
	LevelExcludedInstrument       = "instrument_unavailable"
	LevelExcludedPriorBar         = "prior_daily_bar_unavailable"
	LevelExcludedATR              = "average_range_unavailable"
	LevelExcludedNoBars           = "five_minute_bars_unavailable"
	LevelExcludedFirstHour        = "first_hour_incomplete"
	LevelExcludedBasis            = "daily_and_five_minute_bars_disagree"
	LevelExcludedDayBar           = "session_daily_bar_unavailable"
)

// LevelReplaySample lists every stock-day of the given sessions on which the
// prior session's Open Positions statement held a long call on the stock,
// and counts what it leaves out. The statement of the prior session, never
// that day's, decides, so a day the owner sold stays in. isIndex names a cash
// index underlying. A stock with a short option against it (a spread, a
// written call) is left alone, as the day plan leaves it.
func LevelReplaySample(statements []flexstmt.Statement, sessions []LevelSession, isIndex func(string) bool) ([]LevelStockDay, map[string]int) {
	excluded := map[string]int{}
	ev, err := collectEvidence(statements)
	if err != nil {
		return nil, excluded
	}
	instruments := map[int64]flexstmt.Instrument{}
	for _, item := range ev.instruments {
		instruments[item.ConID] = item
	}
	snapshots := replaySnapshots(statements)
	var out []LevelStockDay
	for _, session := range sessions {
		rows, ok := snapshots[session.Prior]
		if !ok {
			excluded[LevelExcludedNoPriorStatement]++
			continue
		}
		type holding struct {
			currency   string
			calls      float64
			short      bool
			unresolved bool
		}
		held := map[string]*holding{}
		for _, row := range rows {
			if !strings.EqualFold(row.AssetClass, "OPT") || row.Quantity == nil || almostEqual(*row.Quantity, 0) {
				continue
			}
			inst := instruments[row.ConID]
			symbol := strings.ToUpper(firstNonEmpty(inst.UnderlyingSymbol, row.UnderlyingSymbol))
			if symbol == "" {
				continue
			}
			h := held[symbol]
			if h == nil {
				h = &holding{currency: strings.ToUpper(firstNonEmpty(row.Currency, inst.Currency))}
				held[symbol] = h
			}
			switch {
			case *row.Quantity < 0:
				h.short = true
			case optionPutCall(inst.PutCall) == "call":
				h.calls += *row.Quantity
			case optionPutCall(inst.PutCall) == "":
				h.unresolved = true
			}
		}
		for _, symbol := range slices.Sorted(maps.Keys(held)) {
			h := held[symbol]
			if h.calls <= 0 && !h.unresolved {
				continue
			}
			switch {
			case isIndex(symbol):
				excluded[LevelExcludedIndex]++
			case h.currency != "" && h.currency != "USD":
				excluded[LevelExcludedNotUS]++
			case h.short:
				excluded[LevelExcludedSpread]++
			case h.calls <= 0:
				excluded[LevelExcludedInstrument]++
			default:
				out = append(out, LevelStockDay{Symbol: symbol, Currency: "USD", Session: session, Contracts: h.calls})
			}
		}
	}
	return out, excluded
}

// LevelDayInput is one sample stock-day with the market data the rule reads.
type LevelDayInput struct {
	Day LevelStockDay
	// Prior is the prior session's daily bar; ATR14 the 14-session average
	// true range through it.
	Prior rpc.MarketReadPrior
	ATR14 float64
	// DayHigh, DayLow and Close are the session's daily bar; Close5 the
	// close five sessions on, zero when it has not closed.
	DayHigh, DayLow, Close float64
	Close5                 float64
	Close5Day              string
	Bars                   []LevelBar
	// Unavailable, when set, leaves the day out with that reason.
	Unavailable string
}

// LevelReplayInput is every sample day, the index's bars by session and the
// look, in minutes after the open (60 when zero).
type LevelReplayInput struct {
	Days        []LevelDayInput
	Index       map[string][]LevelBar
	LookMinutes int
}

// levelBasisPct is how far the session's five-minute range may sit from its
// daily bar before the two are read as different price bases (a split read
// on one side only).
const levelBasisPct = 2.0

// LevelReplayOutput is the replayed days and the reasons days left out.
type LevelReplayOutput struct {
	Days     []rpc.EdgeLevelsDay
	Excluded map[string]int
	// tallies keeps the path counts of each day drafted at the look.
	tallies map[int]levelPath
}

// LevelReplay replays every day and returns one row each.
func LevelReplay(in LevelReplayInput) LevelReplayOutput {
	out := LevelReplayOutput{Excluded: map[string]int{}, tallies: map[int]levelPath{}}
	look := time.Duration(in.LookMinutes) * time.Minute
	if look <= 0 {
		look = time.Hour
	}
	for _, d := range in.Days {
		row, path, reason := levelReplayDay(d, in.Index[d.Day.Session.Date], look)
		if reason != "" {
			out.Excluded[reason]++
			continue
		}
		if path != nil {
			out.tallies[len(out.Days)] = *path
		}
		out.Days = append(out.Days, row)
	}
	return out
}

func levelReplayDay(d LevelDayInput, index []LevelBar, lookAfter time.Duration) (rpc.EdgeLevelsDay, *levelPath, string) {
	s := d.Day.Session
	row := rpc.EdgeLevelsDay{Symbol: d.Day.Symbol, Session: s.Date, Contracts: d.Day.Contracts, Short: s.Short, Prior: d.Prior, ATR14: d.ATR14, SessionHigh: d.DayHigh, SessionLow: d.DayLow, Close: d.Close}
	switch {
	case d.Unavailable != "":
		return row, nil, d.Unavailable
	case d.Prior.High <= 0 || d.Prior.Low <= 0 || d.Prior.Close <= 0:
		return row, nil, LevelExcludedPriorBar
	case d.ATR14 <= 0:
		return row, nil, LevelExcludedATR
	case d.Close <= 0 || d.DayHigh <= 0 || d.DayLow <= 0:
		return row, nil, LevelExcludedDayBar
	}
	bars := levelSessionBars(d.Bars, s)
	if len(bars) == 0 {
		return row, nil, LevelExcludedNoBars
	}
	look := s.Open.Add(lookAfter)
	lookIdx := -1
	for i, b := range bars {
		if b.Start.Add(5 * time.Minute).Equal(look) {
			lookIdx = i
		}
	}
	if lookIdx < 0 || bars[0].Start.After(s.Open) {
		return row, nil, LevelExcludedFirstHour
	}
	high, low := bars[0].High, bars[0].Low
	for _, b := range bars[1:] {
		high, low = max(high, b.High), min(low, b.Low)
	}
	if math.Abs(high-d.DayHigh)/d.DayHigh*100 > levelBasisPct || math.Abs(low-d.DayLow)/d.DayLow*100 > levelBasisPct {
		return row, nil, LevelExcludedBasis
	}
	if d.Close5 > 0 {
		row.Close5, row.Close5Day = new(d.Close5), d.Close5Day
	}
	lookPrice := bars[lookIdx].Close
	row.Row = levelRowKind(rpc.MarketReadHeldPosition(lookPrice, d.Prior))
	row.DownOnDay = lookPrice < d.Prior.Close
	indexLow, indexAt := levelIndexLow(index, s, look)
	gate := func(at time.Time) bool {
		bar, ok := indexAt[at.Unix()]
		return ok && indexLow > 0 && bar.Close < indexLow
	}
	var path *levelPath
	row.Look, path = levelDraft(bars, lookIdx, d, row.Close5, gate, row.Row == rpc.EdgeLevelsRowLosing || row.DownOnDay)
	if row.Row == rpc.EdgeLevelsRowWatched {
		lastHour := s.Close.Add(-time.Hour)
		for i := lookIdx + 1; i < len(bars) && bars[i].Start.Add(5*time.Minute).Before(lastHour); i++ {
			if bars[i].Close < d.Prior.Low {
				later, _ := levelDraft(bars, i, d, row.Close5, gate, true)
				row.Later = &later
				break
			}
		}
	}
	return row, path, ""
}

// levelDraft freezes the levels at bars[moment] and values the versions
// from there; a row the rule does not draft gets the held result only.
func levelDraft(bars []LevelBar, moment int, d LevelDayInput, close5 *float64, gate func(time.Time) bool, drafted bool) (rpc.EdgeLevelsDraft, *levelPath) {
	s := d.Day.Session
	lv := levelsAt(bars[:moment+1], d.Prior, d.ATR14)
	at := bars[moment].Start.Add(5 * time.Minute)
	draft := rpc.EdgeLevelsDraft{Moment: at, MomentPrice: lv.price, DayHigh: lv.high, DayLow: lv.low, DayMid: lv.mid, Cut: lv.cut, Stop: lv.stop, Trims: slices.Clone(lv.trims), Results: []rpc.EdgeLevelsDayResult{}}
	if draft.Trims == nil {
		draft.Trims = []float64{}
	}
	value := func(sim levelSim, size, version string) rpc.EdgeLevelsDayResult {
		res := rpc.EdgeLevelsDayResult{Version: version, Size: size, ClosePct: sim.valuePct(d.Close, lv.price), Events: sim.events}
		if close5 != nil {
			res.H5Pct = new(sim.valuePct(*close5, lv.price))
		}
		return res
	}
	draft.Results = append(draft.Results, value(levelSim{units: 1, events: []rpc.EdgeLevelsEvent{}}, "", rpc.EdgeLevelsVersionHeld))
	if !drafted {
		return draft, nil
	}
	after := bars[moment+1:]
	for _, size := range []string{rpc.EdgeLevelsSizeHalves, rpc.EdgeLevelsSizeThirds} {
		first := levelShare(size)
		atOnce := levelSim{units: 1 - first, cash: first * lv.price, events: []rpc.EdgeLevelsEvent{{At: at, Kind: "sale", Fraction: first, Price: lv.price}}}
		draft.Results = append(draft.Results,
			value(atOnce, size, rpc.EdgeLevelsVersionAtOnce),
			value(runLevelRule(after, lv, size, false, nil, s), size, rpc.EdgeLevelsVersionRule),
			value(runLevelRule(after, lv, size, true, nil, s), size, rpc.EdgeLevelsVersionBuyBack),
			value(runLevelRule(after, lv, size, false, gate, s), size, rpc.EdgeLevelsVersionIndex))
	}
	path := levelPathOf(after, lv, d.Close)
	halves := runLevelRule(after, lv, rpc.EdgeLevelsSizeHalves, false, nil, s)
	path.cuts, path.cutSale, path.trims, path.timeSales = halves.cuts, halves.cutPrice, halves.trims, halves.timeSales
	path.roundTrips = runLevelRule(after, lv, rpc.EdgeLevelsSizeHalves, true, nil, s).roundTrips
	return draft, &path
}

// levelShare is the first trim's size: half, or a third.
func levelShare(size string) float64 {
	if size == rpc.EdgeLevelsSizeThirds {
		return 1.0 / 3
	}
	return 0.5
}

func levelRowKind(position string) string {
	switch position {
	case rpc.MarketReadBelowPriorLow:
		return rpc.EdgeLevelsRowLosing
	case rpc.MarketReadAbovePriorHigh:
		return rpc.EdgeLevelsRowWorking
	}
	return rpc.EdgeLevelsRowWatched
}

// levelSessionBars keeps the bars inside the session, in time order.
func levelSessionBars(bars []LevelBar, s LevelSession) []LevelBar {
	var out []LevelBar
	for _, b := range bars {
		if !b.Start.Before(s.Open) && !b.Start.Add(5*time.Minute).After(s.Close) && b.Close > 0 && b.High >= b.Low {
			out = append(out, b)
		}
	}
	slices.SortFunc(out, func(a, b LevelBar) int { return a.Start.Compare(b.Start) })
	return slices.CompactFunc(out, func(a, b LevelBar) bool { return a.Start.Equal(b.Start) })
}

// levelIndexLow is the index's low through the look (its first-hour low at
// the first-hour look) and its bars by start.
func levelIndexLow(index []LevelBar, s LevelSession, look time.Time) (float64, map[int64]LevelBar) {
	low := 0.0
	byStart := map[int64]LevelBar{}
	for _, b := range levelSessionBars(index, s) {
		byStart[b.Start.Unix()] = b
		if b.Start.Before(look) && (low == 0 || b.Low < low) {
			low = b.Low
		}
	}
	return low, byStart
}

// levels is a row's levels, frozen at its moment.
type levels struct {
	price, high, low, mid float64
	cut, stop             float64
	trims                 []float64
}

func levelsAt(bars []LevelBar, prior rpc.MarketReadPrior, atr float64) levels {
	lv := levels{price: bars[len(bars)-1].Close, high: bars[0].High, low: bars[0].Low}
	for _, b := range bars[1:] {
		lv.high, lv.low = max(lv.high, b.High), min(lv.low, b.Low)
	}
	lv.mid = rpc.MarketReadDayMid(lv.high, lv.low)
	lv.cut = lv.low
	lv.stop = lv.cut - atr
	for _, t := range []float64{lv.mid, prior.Low} {
		if t > lv.price && !slices.Contains(lv.trims, t) {
			lv.trims = append(lv.trims, t)
		}
	}
	slices.Sort(lv.trims)
	return lv
}

// levelSim is one version's position through a session: units held as a
// fraction of the position at the moment, and cash in stock-price units.
type levelSim struct {
	units, cash float64
	events      []rpc.EdgeLevelsEvent
	// What fired, for the tally.
	cuts, trims, timeSales int
	cutPrice               float64
	roundTrips             []levelRoundTrip
}

type levelRoundTrip struct{ sold, bought float64 }

func (m levelSim) valuePct(price, at float64) float64 {
	return (m.cash+m.units*price)/at*100 - 100
}

func (m *levelSim) sell(at time.Time, kind string, fraction, price float64) {
	fraction = min(fraction, m.units)
	if fraction <= 1e-12 {
		return
	}
	m.units -= fraction
	m.cash += fraction * price
	m.events = append(m.events, rpc.EdgeLevelsEvent{At: at, Kind: kind, Fraction: fraction, Price: price})
}

// runLevelRule runs the rule on the bars after the moment. buyBack adds the
// buy back; gate, when set, lets the cut sell only at a close it allows.
func runLevelRule(bars []LevelBar, lv levels, size string, buyBack bool, gate func(time.Time) bool, s LevelSession) levelSim {
	m := levelSim{units: 1, events: []rpc.EdgeLevelsEvent{}}
	trims, share := lv.trims, levelShare(size)
	if size == rpc.EdgeLevelsSizeHalves && len(trims) > 1 {
		trims = trims[:1]
	}
	nextTrim, fired, timeDone := 0, false, false
	below, above := 0, 0
	sold, bought, final := 0.0, false, false
	soldPrice, saleBar := 0.0, -1
	lastHour := s.Close.Add(-time.Hour)
	for i, b := range bars {
		end := b.Start.Add(5 * time.Minute)
		if !timeDone && !b.Start.Before(lastHour) {
			timeDone = true
			if !fired && !bought && m.units > 0 {
				m.sell(b.Start, "last_hour", share, b.Open)
				m.timeSales++
			}
		}
		// In-bar touches: the trims on the way up, the stop on the way down,
		// the one nearer the bar's open first.
		stopHit := m.units > 0 && b.Low <= lv.stop
		stopFirst := stopHit
		if stopHit && nextTrim < len(trims) && !bought && b.High >= trims[nextTrim] {
			stopFirst = math.Abs(b.Open-lv.stop) < math.Abs(trims[nextTrim]-b.Open)
		}
		if stopFirst {
			before := m.units
			m.sell(b.Start, "stop", m.units, min(lv.stop, b.Open))
			if !final {
				sold, soldPrice, saleBar = before, min(lv.stop, b.Open), i
			}
			fired, below, above = true, 0, 0
		}
		for !bought && m.units > 0 && nextTrim < len(trims) && b.High >= trims[nextTrim] {
			m.sell(b.Start, "trim", share, max(trims[nextTrim], b.Open))
			m.trims++
			nextTrim++
			fired = true
		}
		if stopHit && !stopFirst && m.units > 0 {
			before := m.units
			m.sell(b.Start, "stop", m.units, min(lv.stop, b.Open))
			if !final {
				sold, soldPrice, saleBar = before, min(lv.stop, b.Open), i
			}
			fired, below, above = true, 0, 0
		}
		// Close-based: the cut, then the buy back.
		if b.Close < lv.cut {
			below++
		} else {
			below = 0
		}
		if m.units > 0 && below >= 2 && (gate == nil || gate(b.Start)) {
			before := m.units
			m.sell(end, "cut", m.units, b.Close)
			m.cuts++
			if m.cutPrice == 0 {
				m.cutPrice = b.Close
			}
			if !final {
				sold, soldPrice, saleBar = before, b.Close, i
			}
			fired, above = true, 0
			continue
		}
		if buyBack && !bought && !final && sold > 0 && m.units <= 1e-12 && i > saleBar {
			if b.Close > lv.cut {
				above++
			} else {
				above = 0
			}
			if above >= 2 {
				m.units += sold
				m.cash -= sold * b.Close
				m.events = append(m.events, rpc.EdgeLevelsEvent{At: end, Kind: "buy_back", Fraction: sold, Price: b.Close})
				m.roundTrips = append(m.roundTrips, levelRoundTrip{sold: soldPrice, bought: b.Close})
				bought, final, below = true, true, 0
			}
		}
	}
	return m
}

// levelPath is what the review counts on one drafted day's price path, the
// sale counts of the rule in halves and the buy back's round trips.
type levelPath struct {
	crossings, crossingsHeld, crossingsNotHeld int
	waitVsTouch                                []float64
	stopTouched                                bool
	stopClosedAboveCut, stopClosedBelowStop    bool
	lowBroke, stalled                          bool
	stallShort                                 float64
	cuts, trims, timeSales                     int
	cutSale, close                             float64
	roundTrips                                 []levelRoundTrip
}

func levelPathOf(bars []LevelBar, lv levels, close float64) levelPath {
	p := levelPath{close: close}
	in, closes := false, 0
	best := lv.price
	for _, b := range bars {
		if !p.lowBroke {
			if b.Low < lv.cut {
				p.lowBroke = true
				if len(lv.trims) > 0 && best < lv.trims[0] {
					p.stalled, p.stallShort = true, (lv.trims[0]-best)/lv.trims[0]*100
				}
			} else {
				best = max(best, b.High)
			}
		}
		if !p.stopTouched && b.Low <= lv.stop {
			p.stopTouched = true
			p.stopClosedAboveCut, p.stopClosedBelowStop = close > lv.cut, close < lv.stop
		}
		if !in && b.Low < lv.cut {
			in, closes = true, 0
			p.crossings++
		}
		if !in {
			continue
		}
		if b.Close < lv.cut {
			closes++
			if closes == 2 {
				p.crossingsHeld++
				p.waitVsTouch = append(p.waitVsTouch, (b.Close-lv.cut)/lv.cut*100)
			}
			continue
		}
		if closes < 2 {
			p.crossingsNotHeld++
		}
		in = false
	}
	if in && closes < 2 {
		p.crossingsNotHeld++
	}
	return p
}

// LevelReplaySummary groups the days by reading, row, half and size and
// tallies the counts by reading and half. Split is the first session of the
// second half: the sample's sessions in order, cut where half its stock-days
// lie before.
func LevelReplaySummary(out LevelReplayOutput) (sums []rpc.EdgeLevelsSum, tallies []rpc.EdgeLevelsTally, split string) {
	days := out.Days
	dates := make([]string, 0, len(days))
	for _, d := range days {
		dates = append(dates, d.Session)
	}
	slices.Sort(dates)
	if len(dates) > 0 {
		split = dates[len(dates)/2]
	}
	for i := range days {
		days[i].Half = rpc.EdgeLevelsHalfFirst
		if days[i].Session >= split {
			days[i].Half = rpc.EdgeLevelsHalfSecond
		}
	}
	type group struct{ reading, row string }
	groups := []group{
		{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsRowLosing},
		{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsRowWatchedLater},
		{rpc.EdgeLevelsReadingDownOnDay, rpc.EdgeLevelsRowLosing},
		{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsRowWatched},
		{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsRowWorking},
	}
	// draftOf is the draft a group reads on a day, or nil when the day is
	// not in the group.
	draftOf := func(g group, d *rpc.EdgeLevelsDay) *rpc.EdgeLevelsDraft {
		switch {
		case g.reading == rpc.EdgeLevelsReadingDownOnDay && d.DownOnDay:
			return &d.Look
		case g.reading == rpc.EdgeLevelsReadingDownOnDay:
			return nil
		case g.row == rpc.EdgeLevelsRowWatchedLater:
			return d.Later
		case d.Row == g.row:
			return &d.Look
		}
		return nil
	}
	halves := []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond}
	for _, g := range groups {
		drafted := g.row == rpc.EdgeLevelsRowLosing || g.row == rpc.EdgeLevelsRowWatchedLater
		for _, half := range halves {
			var in []int
			var drafts []*rpc.EdgeLevelsDraft
			for i := range days {
				if draft := draftOf(g, &days[i]); draft != nil && (half == rpc.EdgeLevelsHalfAll || days[i].Half == half) {
					in, drafts = append(in, i), append(drafts, draft)
				}
			}
			sizes, versions := []string{rpc.EdgeLevelsSizeHalves, rpc.EdgeLevelsSizeThirds}, rpc.EdgeLevelsVersions
			if !drafted {
				sizes, versions = []string{""}, versions[:1]
			}
			for _, size := range sizes {
				sum := rpc.EdgeLevelsSum{Reading: g.reading, Row: g.row, Half: half, Size: size, Results: []rpc.EdgeLevelsResultMean{}}
				sum.StockDays, sum.Sessions, sum.Names = levelCounts(days, in)
				for _, v := range versions {
					sum.Results = append(sum.Results, levelMean(drafts, v, size))
				}
				sums = append(sums, sum)
			}
		}
	}
	for _, g := range groups[:3] {
		if g.row == rpc.EdgeLevelsRowWatchedLater {
			continue
		}
		for _, half := range halves {
			t := rpc.EdgeLevelsTally{Reading: g.reading, Half: half}
			var cutVs, wait, stall, trips []float64
			for i := range days {
				p, ok := out.tallies[i]
				if !ok || draftOf(g, &days[i]) == nil || half != rpc.EdgeLevelsHalfAll && days[i].Half != half {
					continue
				}
				t.Days++
				t.Crossings += p.crossings
				t.CrossingsHeld += p.crossingsHeld
				t.CrossingsNotHeld += p.crossingsNotHeld
				wait = append(wait, p.waitVsTouch...)
				if p.stopTouched {
					t.StopTouches++
					if p.stopClosedAboveCut {
						t.StopClosedAboveCut++
					}
					if p.stopClosedBelowStop {
						t.StopClosedBelowStop++
					}
				}
				if p.lowBroke {
					t.LowBreaks++
					if p.stalled {
						t.BouncesStalled++
						stall = append(stall, p.stallShort)
					}
				}
				if p.cuts > 0 {
					t.Cuts++
					if p.close > p.cutSale {
						t.CutsClosedAbove++
					}
					cutVs = append(cutVs, (p.close-p.cutSale)/p.cutSale*100)
				}
				t.Trims += p.trims
				t.TimeSales += p.timeSales
				for _, r := range p.roundTrips {
					t.RoundTrips++
					trips = append(trips, (r.bought-r.sold)/r.sold*100)
				}
			}
			t.CutCloseVsSalePct, t.WaitVsTouchPct, t.StallShortPct, t.RoundTripCostPct = levelAvg(cutVs), levelAvg(wait), levelAvg(stall), levelAvg(trips)
			tallies = append(tallies, t)
		}
	}
	return sums, tallies, split
}

func levelCounts(days []rpc.EdgeLevelsDay, in []int) (int, int, int) {
	sessions, names := map[string]bool{}, map[string]bool{}
	for _, i := range in {
		sessions[days[i].Session], names[days[i].Symbol] = true, true
	}
	return len(in), len(sessions), len(names)
}

func levelResult(d *rpc.EdgeLevelsDraft, version, size string) (rpc.EdgeLevelsDayResult, bool) {
	for _, r := range d.Results {
		if r.Version == version && (version == rpc.EdgeLevelsVersionHeld || r.Size == size) {
			return r, true
		}
	}
	return rpc.EdgeLevelsDayResult{}, false
}

func levelMean(drafts []*rpc.EdgeLevelsDraft, version, size string) rpc.EdgeLevelsResultMean {
	m := rpc.EdgeLevelsResultMean{Version: version}
	var closes, h5 []float64
	for _, d := range drafts {
		r, ok := levelResult(d, version, size)
		held, _ := levelResult(d, rpc.EdgeLevelsVersionHeld, "")
		if !ok {
			continue
		}
		closes = append(closes, r.ClosePct)
		if r.ClosePct > held.ClosePct+1e-9 {
			m.CloseBeatsHeld++
		}
		if r.H5Pct != nil && held.H5Pct != nil {
			h5 = append(h5, *r.H5Pct)
			if *r.H5Pct > *held.H5Pct+1e-9 {
				m.H5BeatsHeld++
			}
		}
	}
	m.CloseDays, m.H5Days = len(closes), len(h5)
	m.CloseMeanPct = levelAvg(closes)
	if len(h5) > 0 {
		m.H5MeanPct = new(levelAvg(h5))
	}
	return m
}

func levelAvg(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sum := 0.0
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}
