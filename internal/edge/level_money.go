package edge

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The level replay in money: every result of a stock-day valued on the calls
// the owner held on the stock, beside the stock. Readings fixed before the
// first run on his sessions:
//
//   - A recorded option price is the statement's mark, a trade's closing
//     price or the recorder's mid of that contract on that session, in that
//     order. An end value is recorded when every held contract has one.
//   - The option's price at the moment is always modelled: Black-Scholes at
//     the moment's stock price with the volatility backed out of the
//     contract's recorded price at the prior session's close (rate zero).
//   - A price during the session, and an end value that is not recorded, is
//     the price at the moment plus the option's delta at the moment times
//     the stock's move since, never below zero. The delta is that model's.
//   - A contract that has expired by the valuation day and has no recorded
//     price is valued at intrinsic value on that day's stock close.
//   - A day whose held calls have no recorded price at the prior close, or
//     a price no volatility explains, has no option result and is counted.
//     (Until the first run on the owner's sessions such a contract was
//     priced at intrinsic value at the moment; on five days of one name that
//     put the moment's value at a sixteenth of the recorded close, so the
//     day is now left out. No version changed.)
//
// Nothing here changes a version: the option figure is the version's own
// sales and buys put through the option's price.

// LevelClose is one later session's daily close.
type LevelClose struct {
	Day   string
	Close float64
}

type levelOptContract struct {
	conid  int64
	symbol string
	strike float64
	expiry string
	// weight is contracts times multiplier.
	weight float64
}

type levelOptFill struct {
	conid  int64
	at     time.Time
	qty    float64 // signed: a buy is positive
	price  float64
	weight float64 // qty times multiplier, signed
}

// LevelOptionBook is the option evidence the replay prices from: the long
// calls held at each statement day's close, every recorded price of a
// contract by day, and the owner's option trades by underlying and day.
type LevelOptionBook struct {
	held  map[string]map[string][]levelOptContract
	marks map[int64]map[string]float64
	fills map[string]map[string][]levelOptFill
	meta  map[int64]levelOptContract
}

// NewLevelOptionBook reads the statements and the recorder's quotes.
func NewLevelOptionBook(statements []flexstmt.Statement, chain []RecordedOptionMark) *LevelOptionBook {
	b := &LevelOptionBook{held: map[string]map[string][]levelOptContract{}, marks: map[int64]map[string]float64{}, fills: map[string]map[string][]levelOptFill{}, meta: map[int64]levelOptContract{}}
	ev, err := collectEvidence(statements)
	if err != nil {
		return b
	}
	instruments := map[int64]flexstmt.Instrument{}
	for _, item := range ev.instruments {
		instruments[item.ConID] = item
	}
	// call returns the contract of a long-call row, and its underlying.
	call := func(conid int64, underlying string, mult *float64) (levelOptContract, string, bool) {
		inst, ok := instruments[conid]
		if !ok || optionPutCall(inst.PutCall) != "call" || inst.Strike == nil || *inst.Strike <= 0 || optionExpiry(inst.Expiry) == "" {
			return levelOptContract{}, "", false
		}
		m := 100.0
		for _, v := range []*float64{inst.Multiplier, mult} {
			if v != nil && *v > 0 {
				m = *v
			}
		}
		symbol := strings.ToUpper(firstNonEmpty(inst.UnderlyingSymbol, underlying))
		return levelOptContract{conid: conid, symbol: symbol, strike: *inst.Strike, expiry: optionExpiry(inst.Expiry), weight: m}, symbol, symbol != ""
	}
	set := func(conid int64, day string, price float64) {
		if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return
		}
		if b.marks[conid] == nil {
			b.marks[conid] = map[string]float64{}
		}
		if _, ok := b.marks[conid][day]; !ok {
			b.marks[conid][day] = price
		}
	}
	for day, rows := range replaySnapshots(statements) {
		for _, row := range rows {
			if !strings.EqualFold(row.AssetClass, "OPT") {
				continue
			}
			if row.MarkPrice != nil {
				set(row.ConID, day, *row.MarkPrice)
			}
			c, symbol, ok := call(row.ConID, row.UnderlyingSymbol, row.Multiplier)
			if !ok || row.Quantity == nil || *row.Quantity <= 0 {
				continue
			}
			b.meta[c.conid] = c
			c.weight *= *row.Quantity
			if b.held[day] == nil {
				b.held[day] = map[string][]levelOptContract{}
			}
			b.held[day][symbol] = append(b.held[day][symbol], c)
		}
	}
	for _, trade := range ev.trades {
		if !strings.EqualFold(trade.AssetClass, "OPT") || trade.Quantity == nil || trade.Price == nil || *trade.Price < 0 {
			continue
		}
		c, symbol, ok := call(trade.ConID, trade.UnderlyingSymbol, trade.Multiplier)
		if !ok {
			continue
		}
		b.meta[c.conid] = c
		day := dayKey(tradeStatementDay(trade))
		qty := math.Abs(*trade.Quantity)
		if strings.HasPrefix(strings.ToUpper(trade.Side), "S") {
			qty = -qty
		}
		if qty == 0 {
			continue
		}
		if b.fills[symbol] == nil {
			b.fills[symbol] = map[string][]levelOptFill{}
		}
		b.fills[symbol][day] = append(b.fills[symbol][day], levelOptFill{conid: c.conid, at: trade.ExecutedAt, qty: qty, price: *trade.Price, weight: qty * c.weight})
	}
	for _, trade := range ev.trades {
		if _, ok := b.meta[trade.ConID]; ok && trade.ClosePrice != nil && *trade.ClosePrice > 0 {
			set(trade.ConID, dayKey(tradeStatementDay(trade)), *trade.ClosePrice)
		}
	}
	byIdentity := map[string]int64{}
	for conid, c := range b.meta {
		byIdentity[replayIdentity(c.symbol, c.expiry, c.strike, "C")] = conid
	}
	for _, q := range chain {
		if q.Bid <= 0 || q.Ask < q.Bid || q.Day.IsZero() {
			continue
		}
		conid := q.ConID
		if _, ok := b.meta[conid]; !ok {
			conid, ok = byIdentity[replayIdentity(strings.ToUpper(q.Underlying), optionExpiry(q.Expiry), q.Strike, strings.ToUpper(q.Right))]
			if !ok {
				continue
			}
		}
		set(conid, dayKey(q.Day), (q.Bid+q.Ask)/2)
	}
	return b
}

// day returns the long calls held on a stock at the prior session's close
// and the owner's trades in the stock's calls on the session.
func (b *LevelOptionBook) day(symbol, prior, session string) ([]levelOptContract, []levelOptFill) {
	if b == nil {
		return nil, nil
	}
	return b.held[prior][symbol], b.fills[symbol][session]
}

func (b *LevelOptionBook) mark(conid int64, day string) (float64, bool) {
	if b == nil {
		return 0, false
	}
	v, ok := b.marks[conid][day]
	return v, ok
}

// levelOptLeg is one held contract priced at a moment.
type levelOptLeg struct {
	levelOptContract
	vol   float64
	price float64
	delta float64
}

// levelOptModel is the calls held on a stock, priced at a moment: the value
// at another stock price is each leg's price plus its delta times the move.
type levelOptModel struct {
	book *LevelOptionBook
	legs []levelOptLeg
	spot float64
	v0   float64
}

func levelYears(day, expiry string) float64 {
	return max(parseDay(expiry).Sub(parseDay(day)).Hours()/24, 0) / 365
}

// levelOptVols backs each held contract's volatility out of its recorded
// price on day at the stock's close then; false when a contract has no
// recorded price that day or no volatility prices it.
func (b *LevelOptionBook) levelOptVols(contracts []levelOptContract, day string, spot float64) ([]levelOptLeg, bool) {
	legs := make([]levelOptLeg, 0, len(contracts))
	for _, c := range contracts {
		price, ok := b.mark(c.conid, day)
		if !ok {
			return nil, false
		}
		vol, ok := mandate.ImpliedVolatility(true, price, spot, c.strike, levelYears(day, c.expiry))
		if !ok {
			return nil, false
		}
		legs = append(legs, levelOptLeg{levelOptContract: c, vol: vol})
	}
	return legs, len(legs) > 0
}

// levelOptModelAt prices the legs at spot on day. With recorded set, a leg
// with a recorded price that day takes it (an entry at a close); otherwise
// every price is the model's. The second value reports every leg recorded.
func (b *LevelOptionBook) levelOptModelAt(legs []levelOptLeg, day string, spot float64, recorded bool) (*levelOptModel, bool) {
	m := &levelOptModel{book: b, legs: slices.Clone(legs), spot: spot}
	all := recorded
	for i := range m.legs {
		leg := &m.legs[i]
		t := levelYears(day, leg.expiry)
		leg.price = mandate.OptionPrice(true, spot, leg.strike, t, leg.vol)
		h := spot * 0.001
		leg.delta = (mandate.OptionPrice(true, spot+h, leg.strike, t, leg.vol) - mandate.OptionPrice(true, spot-h, leg.strike, t, leg.vol)) / (2 * h)
		if price, ok := b.mark(leg.conid, day); recorded && ok {
			leg.price = price
		} else {
			all = false
		}
		m.v0 += leg.weight * leg.price
	}
	if m.v0 <= 0 {
		return nil, false
	}
	return m, all
}

// at is the position's modelled value at a stock price.
func (m *levelOptModel) at(spot float64) float64 {
	v := 0.0
	for _, leg := range m.legs {
		v += leg.weight * max(0, leg.price+leg.delta*(spot-m.spot))
	}
	return v
}

// end is the position's value on a day at the stock's close then: recorded
// when every leg has a recorded price, else each leg its recorded price, its
// intrinsic value once expired, or the model.
func (m *levelOptModel) end(day string, spot float64) (float64, string) {
	v, source := 0.0, rpc.EdgeLevelsRecorded
	for _, leg := range m.legs {
		switch price, ok := m.book.mark(leg.conid, day); {
		case ok:
			v += leg.weight * price
		case leg.expiry < day:
			v, source = v+leg.weight*max(0, spot-leg.strike), rpc.EdgeLevelsModelled
		default:
			v, source = v+leg.weight*max(0, leg.price+leg.delta*(spot-m.spot)), rpc.EdgeLevelsModelled
		}
	}
	return v, source
}

// result values a version's sales and buys on the calls.
func (m *levelOptModel) result(sim levelSim, day string, close float64, day5 string, close5 *float64) *rpc.EdgeLevelsOptionPct {
	cash := 0.0
	for _, e := range sim.events {
		if e.Kind == "buy_back" {
			cash -= e.Fraction * m.at(e.Price)
		} else {
			cash += e.Fraction * m.at(e.Price)
		}
	}
	end, source := m.end(day, close)
	out := &rpc.EdgeLevelsOptionPct{ClosePct: (cash+sim.units*end)/m.v0*100 - 100, CloseSource: source}
	if close5 != nil && day5 != "" {
		end5, source5 := m.end(day5, *close5)
		out.H5Pct, out.H5Source = new((cash+sim.units*end5)/m.v0*100-100), source5
	}
	return out
}

// levelActual values what the owner did that session in the stock's calls:
// the position after his trades, at the trades' own prices, against the
// modelled value at the moment. A contract he bought that the model does not
// hold needs a recorded price at the valuation day, or the result is absent.
func (m *levelOptModel) levelActual(fills []levelOptFill, day string, close float64, day5 string, close5 *float64) *rpc.EdgeLevelsActual {
	if len(fills) == 0 {
		return nil
	}
	out := &rpc.EdgeLevelsActual{}
	cash := 0.0
	weights, order := map[int64]float64{}, []int64{}
	for _, leg := range m.legs {
		weights[leg.conid] = leg.weight
		order = append(order, leg.conid)
	}
	for _, f := range fills {
		if f.qty > 0 {
			out.Buys++
		} else {
			out.Sales++
		}
		cash -= f.weight * f.price
		if _, ok := weights[f.conid]; !ok {
			order = append(order, f.conid)
		}
		weights[f.conid] += f.weight
	}
	value := func(day string, spot float64) (float64, string, bool) {
		v, source := 0.0, rpc.EdgeLevelsRecorded
		for _, conid := range order {
			w := weights[conid]
			if math.Abs(w) < 1e-9 {
				continue
			}
			i := slices.IndexFunc(m.legs, func(l levelOptLeg) bool { return l.conid == conid })
			switch price, ok := m.book.mark(conid, day); {
			case ok:
				v += w * price
			case i < 0:
				return 0, "", false
			case m.legs[i].expiry < day:
				v, source = v+w*max(0, spot-m.legs[i].strike), rpc.EdgeLevelsModelled
			default:
				v, source = v+w*max(0, m.legs[i].price+m.legs[i].delta*(spot-m.spot)), rpc.EdgeLevelsModelled
			}
		}
		return v, source, true
	}
	end, source, ok := value(day, close)
	if !ok {
		return out
	}
	out.Result = &rpc.EdgeLevelsOptionPct{ClosePct: (cash+end)/m.v0*100 - 100, CloseSource: source}
	if close5 != nil && day5 != "" {
		if end5, source5, ok := value(day5, *close5); ok {
			out.Result.H5Pct, out.Result.H5Source = new((cash+end5)/m.v0*100-100), source5
		}
	}
	return out
}

// levelLaterBuyBack is the buy back on a later session: after a cut at
// level cut, the first daily close above it within the five sessions after
// the cut's (the cut's own session does not count), bought once at that
// close and judged five sessions after the buy against staying out. A cut
// whose five sessions have not all closed without such a close is pending.
func levelLaterBuyBack(cut float64, next []LevelClose, legs []levelOptLeg, book *LevelOptionBook) *rpc.EdgeLevelsEntry {
	e := &rpc.EdgeLevelsEntry{Kind: rpc.EdgeLevelsEntryLaterBuyBack, State: rpc.EdgeLevelsEntryNever}
	for i := range levelHorizon {
		if i >= len(next) {
			e.State = rpc.EdgeLevelsEntryPending
			return e
		}
		if next[i].Close <= cut {
			continue
		}
		e.State, e.Day, e.Price = rpc.EdgeLevelsEntryBought, next[i].Day, next[i].Close
		if j := i + levelHorizon; j < len(next) {
			e.H5Pct = new((next[j].Close/e.Price - 1) * 100)
			if m, recorded := book.levelOptModelAt(legs, e.Day, e.Price, true); m != nil && len(legs) > 0 {
				end, source := m.end(next[j].Day, next[j].Close)
				if !recorded {
					source = rpc.EdgeLevelsModelled
				}
				e.OptionH5Pct, e.OptionSource = new((end/m.v0-1)*100), source
			}
		}
		return e
	}
	return e
}

// levelHorizon is the sessions an entry is judged after, and a later buy
// back may come within.
const levelHorizon = 5

// levelAdds are the adds the two rules under test make after the look, each
// once per session: two consecutive completed five-minute closes above the
// prior session's high; and the reclaim, on a session that opened under the
// prior session's low, two consecutive closes above that low. An add is
// bought at the second close, and only at a close where the index's bar has
// closed at or above the index's own low through the look (a bar the index
// lacks allows none). Each is judged five sessions on against not adding.
func levelAdds(bars []LevelBar, lookIdx int, prior rpc.MarketReadPrior, notUnder func(time.Time) bool, close5 *float64, day5 string, m *levelOptModel) []rpc.EdgeLevelsEntry {
	var out []rpc.EdgeLevelsEntry
	rules := []struct {
		kind  string
		level float64
		on    bool
	}{
		{rpc.EdgeLevelsEntryAddAboveHigh, prior.High, true},
		{rpc.EdgeLevelsEntryAddReclaim, prior.Low, bars[0].Open < prior.Low},
	}
	for _, rule := range rules {
		if !rule.on {
			continue
		}
		above := 0
		for _, b := range bars[lookIdx+1:] {
			if b.Close > rule.level {
				above++
			} else {
				above = 0
			}
			if above < 2 || !notUnder(b.Start) {
				continue
			}
			e := rpc.EdgeLevelsEntry{Kind: rule.kind, State: rpc.EdgeLevelsEntryBought, At: b.Start.Add(5 * time.Minute), Price: b.Close}
			if close5 != nil {
				e.H5Pct = new((*close5/b.Close - 1) * 100)
				if m != nil {
					if at := m.at(b.Close); at > 0 {
						end, source := m.end(day5, *close5)
						e.OptionH5Pct, e.OptionSource = new((end/at-1)*100), source
						if source == rpc.EdgeLevelsRecorded {
							// The buy's own price is modelled, so the pair is.
							e.OptionSource = rpc.EdgeLevelsModelled
						}
					}
				}
			} else {
				e.State = rpc.EdgeLevelsEntryPending
			}
			out = append(out, e)
			break
		}
	}
	return out
}

// levelOwnerAdds are the calls the owner bought on the stock that session,
// each judged on its own contract: the recorded price five sessions on
// against the price he paid. Without that recorded price the add is pending.
func (b *LevelOptionBook) levelOwnerAdds(fills []levelOptFill, day5 string) []rpc.EdgeLevelsEntry {
	var out []rpc.EdgeLevelsEntry
	for _, f := range fills {
		if f.qty <= 0 || f.price <= 0 {
			continue
		}
		e := rpc.EdgeLevelsEntry{Kind: rpc.EdgeLevelsEntryOwnerAdd, State: rpc.EdgeLevelsEntryPending, At: f.at}
		if price, ok := b.mark(f.conid, day5); ok && day5 != "" {
			e.State, e.OptionH5Pct, e.OptionSource = rpc.EdgeLevelsEntryBought, new((price/f.price-1)*100), rpc.EdgeLevelsRecorded
		}
		out = append(out, e)
	}
	return out
}

// LevelReplayEntries counts the buys by kind and half: the later buy backs
// under each reading (after the cuts of the rule in halves) and the adds on
// every day. The days must carry their half.
func LevelReplayEntries(days []rpc.EdgeLevelsDay) []rpc.EdgeLevelsEntrySum {
	var out []rpc.EdgeLevelsEntrySum
	halves := []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond}
	sum := func(kind, reading, half string, pick func(*rpc.EdgeLevelsDay) []rpc.EdgeLevelsEntry) {
		s := rpc.EdgeLevelsEntrySum{Kind: kind, Reading: reading, Half: half}
		var h5, opt []float64
		for i := range days {
			if half != rpc.EdgeLevelsHalfAll && days[i].Half != half {
				continue
			}
			for _, e := range pick(&days[i]) {
				if e.Kind != kind {
					continue
				}
				if kind == rpc.EdgeLevelsEntryLaterBuyBack {
					s.Cuts++
				}
				switch e.State {
				case rpc.EdgeLevelsEntryBought:
					s.Bought++
				case rpc.EdgeLevelsEntryNever:
					s.Never++
				default:
					s.Pending++
				}
				if e.H5Pct != nil {
					h5 = append(h5, *e.H5Pct)
					if *e.H5Pct > 0 {
						s.H5Wins++
					}
				}
				if e.OptionH5Pct != nil {
					opt = append(opt, *e.OptionH5Pct)
					if *e.OptionH5Pct > 0 {
						s.OptionH5Wins++
					}
					if e.OptionSource == rpc.EdgeLevelsRecorded {
						s.OptionRecorded++
					}
				}
			}
		}
		s.Judged, s.OptionJudged = len(h5), len(opt)
		if len(h5) > 0 {
			s.H5MeanPct = new(levelAvg(h5))
		}
		if len(opt) > 0 {
			s.OptionH5MeanPct = new(levelAvg(opt))
		}
		out = append(out, s)
	}
	for _, half := range halves {
		for _, reading := range []string{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsReadingDownOnDay} {
			sum(rpc.EdgeLevelsEntryLaterBuyBack, reading, half, func(d *rpc.EdgeLevelsDay) []rpc.EdgeLevelsEntry {
				in := reading == rpc.EdgeLevelsReadingPriorLow && d.Row == rpc.EdgeLevelsRowLosing || reading == rpc.EdgeLevelsReadingDownOnDay && d.DownOnDay
				if !in || d.Look.LaterBuyBack == nil {
					return nil
				}
				return []rpc.EdgeLevelsEntry{*d.Look.LaterBuyBack}
			})
		}
		for _, kind := range []string{rpc.EdgeLevelsEntryAddAboveHigh, rpc.EdgeLevelsEntryAddReclaim, rpc.EdgeLevelsEntryOwnerAdd} {
			sum(kind, "", half, func(d *rpc.EdgeLevelsDay) []rpc.EdgeLevelsEntry { return d.Adds })
		}
	}
	return out
}

// LevelReplayOptionShare counts how much of the days' option figures rests
// on recorded prices, read off each day's held result at the look.
func LevelReplayOptionShare(days []rpc.EdgeLevelsDay) rpc.EdgeLevelsOptionShare {
	var s rpc.EdgeLevelsOptionShare
	for i := range days {
		d := &days[i]
		if d.Actual != nil {
			s.ActualDays++
			if d.Actual.Result == nil {
				s.ActualUnpriced++
			}
		}
		held, ok := levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, "")
		if !ok || held.Option == nil {
			s.Unavailable++
			continue
		}
		s.Days++
		if held.Option.CloseSource == rpc.EdgeLevelsRecorded {
			s.CloseRecorded++
		}
		if held.Option.H5Pct != nil {
			s.H5Days++
			if held.Option.H5Source == rpc.EdgeLevelsRecorded {
				s.H5Recorded++
			}
		}
	}
	return s
}
