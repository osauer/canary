package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// marketReadHeldCap is market.snapshot's cap on held underlyings.
const marketReadHeldCap = 12

// marketReadHeldParallel bounds the held names' quote looks in a pool of
// their own, so they never wait ahead of the index, VIX and leader looks:
// twelve looks four at a time at 1.5 seconds each end inside the budget.
const marketReadHeldParallel = 4

// marketReadHeldSource is what one read acquired for a held underlying.
// A held leader acquires nothing of its own: its row is the leader row's.
type marketReadHeldSource struct {
	marketReadLeaderSource
	Leader bool
	// Unquoted says why the name is not looked at, when it is not.
	Unquoted string
	// DayHigh and DayLow are the quote's day high and low; zero when not
	// quoted.
	DayHigh, DayLow float64
}

// marketReadHolding is one held underlying and the contract its market data
// is read through, spelled as market.snapshot spells it so the chart series
// it keeps warm are the ones read here.
type marketReadHolding struct {
	Contract rpc.ContractParams
	Unquoted string
}

// marketReadHoldings lists the underlyings of the current stock and option
// positions as market.snapshot picks them, or says why there are none.
func (s *Server) marketReadHoldings(ctx context.Context) ([]marketReadHolding, bool, string) {
	var positions *rpc.PositionsResult
	var err error
	if s.marketReadPositionsForTest != nil {
		positions, err = s.marketReadPositionsForTest(ctx)
	} else {
		positions, err = s.handlePositionsList(ctx, &rpc.Request{Params: json.RawMessage(`{}`)})
	}
	if err != nil || positions == nil {
		return nil, false, "positions not readable"
	}
	if positions.Authority == nil || positions.Authority.Availability != rpc.AccountDataAvailable || positions.Authority.Freshness != rpc.AccountDataFreshnessCurrent {
		return nil, false, "current holdings unavailable"
	}
	var out []marketReadHolding
	seen := map[string]bool{}
	for _, g := range positions.ByUnderlying {
		contract, ok := rpc.UnderlyingMarketContract(g)
		if !ok || seen[contract.Symbol] {
			continue
		}
		if len(out) >= marketReadHeldCap {
			return out, true, ""
		}
		seen[contract.Symbol] = true
		h := marketReadHolding{Contract: contract}
		switch {
		case !rpc.ExpectsMarketDataGroup(g):
			h.Unquoted = "the holding takes no market data"
		case contract.Currency != "USD":
			h.Unquoted = "not a US listing; the read follows the US session"
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return nil, false, "nothing held"
	}
	return out, false, ""
}

// acquireMarketReadHeld starts the held names' acquisitions beside the
// read's others: the daily series from the chart cache (missing ones read in
// the background), today's bars only from a retained 1D series, and the
// price of a line the daemon already holds, else one brief look in the held
// pool. The positions read itself runs inside the budget: it can probe the
// broker while it prewarms quotes, so it never holds up the other inputs, and
// a read it misses says so.
func (s *Server) acquireMarketReadHeld(ctx context.Context, reads broker.Reads, leaders []rpc.ContractParams, wg *sync.WaitGroup, set func(func()), daily func(rpc.ContractParams, bool, func(*rpc.MarketHistoryResult)), src *marketReadSources) {
	src.HeldUnavailable = "positions not read within the read's budget"
	wg.Go(func() {
		posCtx, cancel := context.WithTimeout(ctx, marketReadBudget)
		defer cancel()
		holdings, truncated, reason := s.marketReadHoldings(posCtx)
		s.startMarketReadHeld(ctx, reads, leaders, holdings, truncated, reason, wg, set, daily, src)
	})
}

func (s *Server) startMarketReadHeld(ctx context.Context, reads broker.Reads, leaders []rpc.ContractParams, holdings []marketReadHolding, truncated bool, reason string, wg *sync.WaitGroup, set func(func()), daily func(rpc.ContractParams, bool, func(*rpc.MarketHistoryResult)), src *marketReadSources) {
	var keys []string
	var lines map[string]*broker.Quote
	if len(holdings) > 0 && reads != nil && s.subs != nil {
		keys, lines = s.subs.heldKeys(), reads.Snapshot()
	}
	held := make([]marketReadHeldSource, len(holdings))
	for i, h := range holdings {
		c := h.Contract
		held[i].Symbol, held[i].Unquoted = c.Symbol, h.Unquoted
		if line := marketReadHeldLine(keys, lines, c); line != nil {
			held[i].DayHigh, held[i].DayLow = line.High, line.Low
			if h.Unquoted == "" {
				held[i].Quote = new(marketReadLineQuote(line))
			}
		}
		if slices.ContainsFunc(leaders, func(l rpc.ContractParams) bool { return l.Symbol == c.Symbol }) {
			held[i].Leader, held[i].Quote, held[i].Unquoted = true, nil, ""
		}
	}
	// Published whole; from here every write goes through set.
	set(func() { src.Held, src.HeldUnavailable, src.HeldTruncated = held, reason, truncated })
	sem := make(chan struct{}, marketReadHeldParallel)
	for i, h := range held {
		if h.Leader || h.Unquoted != "" {
			continue
		}
		c := holdings[i].Contract
		daily(c, false, func(r *rpc.MarketHistoryResult) { src.Held[i].Daily = r })
		wg.Go(func() {
			r := s.retainedHistory(ctx, rpc.MarketHistoryParams{Contract: c, Range: "1D"})
			set(func() { src.Held[i].Intraday = r })
		})
		if h.Quote != nil || reads == nil && s.marketReadLookForTest == nil {
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			q, high, low, err := s.marketReadHeldLook(ctx, c)
			if err == nil || q.Last > 0 || q.Mark > 0 {
				set(func() { src.Held[i].Quote, src.Held[i].DayHigh, src.Held[i].DayLow = &q, high, low })
			}
		})
	}
}

// marketReadHeldLook takes one brief look at a held underlying through the
// contract-exact transient quote, as market.snapshot does, its line released
// when read.
func (s *Server) marketReadHeldLook(ctx context.Context, c rpc.ContractParams) (broker.BriefQuote, float64, float64, error) {
	if s.marketReadLookForTest != nil {
		q, _, err := s.marketReadLookForTest(ctx, c)
		return q, 0, 0, err
	}
	raw, err := json.Marshal(rpc.QuoteSnapshotParams{Contract: c, TimeoutMs: int(marketReadQuoteTimeout / time.Millisecond)})
	if err != nil {
		return broker.BriefQuote{}, 0, 0, err
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	if err != nil || q == nil {
		return broker.BriefQuote{}, 0, 0, err
	}
	finite := func(p *float64) float64 {
		if p != nil && isFinitePrice(*p) {
			return *p
		}
		return 0
	}
	out := broker.BriefQuote{Last: finite(q.Last), Mark: finite(q.Mark), Close: finite(q.PrevClose), Open: finite(q.DayOpen), TradeAt: q.TradeAt, ObservedAt: q.QuotePriceAt}
	if out.Mark == 0 {
		out.Mark = finite(q.QuotePrice)
	}
	if out.ObservedAt.IsZero() {
		out.ObservedAt = q.ReceivedAt
	}
	return out, finite(q.DayHigh), finite(q.DayLow), nil
}

// marketReadHeldLine is the freshest line the daemon holds for c's share or
// index line, or nil. A line of another kind (an option on the same
// underlying), another currency or another contract id is not c's.
func marketReadHeldLine(keys []string, lines map[string]*broker.Quote, c rpc.ContractParams) *broker.Quote {
	var best *broker.Quote
	for _, key := range keys {
		q := lines[key]
		if q == nil || !marketReadLineIs(key, c) || !(isFinitePrice(q.Last) || isFinitePrice(q.MarkPrice)) || !isFinitePrice(q.Close) {
			continue
		}
		if best == nil || q.Timestamp.After(best.Timestamp) {
			best = q
		}
	}
	return best
}

// marketReadLineIs reports whether a quote-cache key names c's line. A bare
// symbol is the default US route; a routed key spells symbol, kind,
// exchange, primary exchange, currency, local symbol and trading class, then
// the contract id when it has one.
func marketReadLineIs(key string, c rpc.ContractParams) bool {
	if key == c.Symbol {
		return c.SecType == "STK" && c.Currency == "USD"
	}
	parts := strings.Split(key, "|")
	if len(parts) < 7 || parts[0] != c.Symbol || parts[1] != c.SecType || parts[4] != "" && parts[4] != c.Currency {
		return false
	}
	for _, p := range parts[7:] {
		if id, ok := strings.CutPrefix(p, "CONID:"); ok && c.ConID > 0 && id != strconv.Itoa(c.ConID) {
			return false
		}
	}
	return true
}

// marketReadLineQuote reads a held line as a brief look would.
func marketReadLineQuote(q *broker.Quote) broker.BriefQuote {
	out := broker.BriefQuote{Bid: q.Bid, Ask: q.Ask, Last: q.Last, Mark: q.MarkPrice, Close: q.Close, Open: q.Open, FeedType: q.FeedType, ObservedAt: q.LastPriceTick, TradeAt: q.LastTradeTime}
	if out.ObservedAt.IsZero() {
		out.ObservedAt = q.Timestamp
	}
	return out
}

// marketReadHeldBlock reads every held underlying. A held leader takes the
// leader row as it was built, so both blocks carry the same values.
func marketReadHeldBlock(now time.Time, state string, session, prior marketcal.Session, src marketReadSources, leaders []rpc.MarketReadLeader) *rpc.MarketReadHeld {
	held := &rpc.MarketReadHeld{Rows: []rpc.MarketReadHeldRow{}, RowsUnavailable: src.HeldUnavailable, Truncated: src.HeldTruncated}
	for _, h := range src.Held {
		var row rpc.MarketReadLeader
		built := false
		if h.Leader {
			for i, l := range src.Leaders {
				if l.Symbol == h.Symbol && i < len(leaders) {
					dayHigh, dayLow := h.DayHigh, h.DayLow
					h.marketReadLeaderSource, h.DayHigh, h.DayLow = l, dayHigh, dayLow
					row, built = leaders[i], true
					break
				}
			}
		}
		if !built {
			row = marketReadLeaderRow(h.marketReadLeaderSource, state, session, prior)
		}
		held.Rows = append(held.Rows, marketReadHeldRow(h, row, now, state, session, prior))
	}
	if len(held.Rows) == 0 && held.RowsUnavailable == "" {
		held.RowsUnavailable = "nothing held"
	}
	return held
}

// marketReadHeldRow reads one held underlying from its source and its
// leader-rule row: the prior session's range, today's range so far and its
// midpoint, and the current price against the prior range.
func marketReadHeldRow(h marketReadHeldSource, base rpc.MarketReadLeader, now time.Time, state string, session, prior marketcal.Session) rpc.MarketReadHeldRow {
	row := rpc.MarketReadHeldRow{Symbol: h.Symbol, Open: base.Open, OpenSource: base.OpenSource, Last: base.Last, LastAt: base.LastAt, ChangePct: base.ChangePct, ChangeUnavailable: base.ChangeUnavailable, State: base.State, StateUnavailable: base.StateUnavailable}
	if bar := marketReadDailyBar(h.Daily, h.Symbol, prior.Date); bar != nil && bar.High != nil && bar.Low != nil {
		row.Prior = &rpc.MarketReadPrior{Date: prior.Date, High: *bar.High, Low: *bar.Low, Close: bar.Value}
	} else {
		row.PriorUnavailable = h.Symbol + " daily bar of " + prior.Date + " unavailable"
	}
	_, _, lastSource, lastReason := marketReadQuoteLast(h.Quote, state, session, prior)
	switch {
	case h.Unquoted != "":
		row.LastUnavailable, row.StateUnavailable, row.ChangeUnavailable = h.Unquoted, h.Unquoted, h.Unquoted
	case state == rpc.MarketReadClosed && row.Last != nil:
		row.LastSource = "daily_bar"
	case state == rpc.MarketReadClosed:
		row.LastUnavailable = "the session's close is not yet in the daily bars"
	case row.Last != nil:
		row.LastSource = lastSource
	default:
		row.LastUnavailable = lastReason
	}
	marketReadHeldDay(&row, h, now, state, session)
	switch {
	case state == rpc.MarketReadPre:
		row.PositionUnavailable = "the session has not opened"
	case row.Prior == nil:
		row.PositionUnavailable = "prior session range unavailable"
	case row.Last == nil:
		row.PositionUnavailable = "current price unavailable"
	default:
		row.Position = rpc.MarketReadHeldPosition(*row.Last, *row.Prior)
	}
	return row
}

// marketReadHeldDay sets today's regular-session range so far. While the
// session is open it comes from today's completed bars when the 1D series is
// retained and current, else from the quote's day high and low, and is
// widened to the current price, which can lie past the last completed bar.
// Once the session has closed it comes from the session's daily bar, else
// its bars. Before the open there is none: a quote's high and low still name
// the prior session.
func marketReadHeldDay(row *rpc.MarketReadHeldRow, h marketReadHeldSource, now time.Time, state string, session marketcal.Session) {
	if state == rpc.MarketReadPre {
		row.DayUnavailable = "the session has not opened"
		return
	}
	var high, low float64
	bars := func() bool {
		if _, reason := marketReadIntraday(h.Intraday, h.Symbol, state, now); reason != "" {
			return false
		}
		got := marketReadBars(h.Intraday.Points, session, historyFetchedAt(h.Intraday))
		if len(got) == 0 {
			return false
		}
		high, low = got[0].High, got[0].Low
		for _, b := range got[1:] {
			high, low = max(high, b.High), min(low, b.Low)
		}
		return true
	}
	switch {
	case state == rpc.MarketReadClosed:
		if bar := marketReadDailyBar(h.Daily, h.Symbol, session.Date); bar != nil && bar.High != nil && bar.Low != nil {
			high, low, row.DaySource = *bar.High, *bar.Low, "daily_bar"
		} else if bars() {
			row.DaySource = "bars"
		} else {
			row.DayUnavailable = "the session's range is not yet in the daily bars"
			return
		}
	case bars():
		row.DaySource = "bars"
	case h.Unquoted == "" && isFinitePrice(h.DayHigh) && isFinitePrice(h.DayLow) && h.DayHigh >= h.DayLow:
		high, low, row.DaySource = h.DayHigh, h.DayLow, "quote"
	default:
		row.DayUnavailable = "no regular bars of today retained and the quote has no day high and low"
		return
	}
	if state == rpc.MarketReadOpen && row.Last != nil {
		high, low = max(high, *row.Last), min(low, *row.Last)
	}
	row.DayHigh, row.DayLow, row.DayMid = new(high), new(low), new(rpc.MarketReadDayMid(high, low))
}
