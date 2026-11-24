package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// marketReadCacheFor is how long one read answers every caller. A console
// polling the read then costs the broker one read per window, however many
// pollers there are.
const marketReadCacheFor = 30 * time.Second

// marketReadQuoteParallel and marketReadQuoteTimeout bound the transient
// quote looks, as market.snapshot bounds its quote reads.
const (
	marketReadQuoteParallel = 4
	marketReadQuoteTimeout  = 2 * time.Second
)

// marketReadCache holds the last read and the one in flight. It is memory
// only: nothing of a read outlives the process.
type marketReadCache struct {
	mu     sync.Mutex
	result *rpc.MarketReadResult
	at     time.Time
	flight chan struct{}
}

// The contracts a read uses, spelled as the daemon's other readers spell
// them so their chart series are shared rather than duplicated.
var (
	marketReadSPX = rpc.ContractParams{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	marketReadVIX = rpc.ContractParams{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	marketReadSPY = rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}
)

// marketReadLeaderContracts is the market tape's leader basket with one
// share line per company.
func marketReadLeaderContracts() []rpc.ContractParams {
	var out []rpc.ContractParams
	for _, c := range tapeResearchContracts() {
		if c.SecType != "STK" || c.Symbol == "SPY" || spx.CompanyRepresentative(c.Symbol) != c.Symbol {
			continue
		}
		out = append(out, c)
	}
	return out
}

func (s *Server) handleMarketRead(ctx context.Context, req *rpc.Request) (*rpc.MarketReadResult, error) {
	if err := rpc.ValidateMarketReadParams(req.Params); err != nil {
		return nil, errBadRequest(err.Error())
	}
	cache := &s.marketRead
	for {
		cache.mu.Lock()
		if cache.result != nil && s.nowUTC().Sub(cache.at) < marketReadCacheFor {
			result := cache.result
			cache.mu.Unlock()
			return result, nil
		}
		if flight := cache.flight; flight != nil {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-flight:
			}
			continue
		}
		flight := make(chan struct{})
		cache.flight = flight
		cache.mu.Unlock()
		// One read serves every waiting caller, so the first caller leaving
		// does not cancel it for the rest.
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		result, err := s.readMarket(readCtx)
		cancel()
		cache.mu.Lock()
		cache.flight = nil
		if err == nil {
			cache.result, cache.at = result, s.nowUTC()
		}
		cache.mu.Unlock()
		close(flight)
		return result, err
	}
}

// marketReadSources is what one read acquired. Each is nil when it could not
// be read.
type marketReadSources struct {
	SPXDaily    *rpc.MarketHistoryResult
	SPXIntraday *rpc.MarketHistoryResult
	// SPYIntraday is the whole retained intraday series, prior sessions
	// included, for the volume baseline.
	SPYIntraday *rpc.MarketHistoryResult
	VIXIntraday *rpc.MarketHistoryResult
	SPXQuote    *broker.BriefQuote
	VIXQuote    *broker.BriefQuote
	Leaders     []marketReadLeaderSource
}

type marketReadLeaderSource struct {
	Symbol string
	Quote  *broker.BriefQuote
	Daily  *rpc.MarketHistoryResult
	// Intraday is the company's intraday series when the chart cache already
	// holds it; it is never requested for the read.
	Intraday *rpc.MarketHistoryResult
}

// readMarket acquires the read's inputs through the daemon's existing paths:
// cached chart history (followed, so the refresh worker keeps it current) and
// brief quote looks through the broker seam, each line released when read.
func (s *Server) readMarket(ctx context.Context) (*rpc.MarketReadResult, error) {
	now := s.nowUTC()
	sessions, err := tapeArchiveCalendar(now.AddDate(0, 0, -45), 46)
	if err != nil {
		return nil, err
	}
	_, prior, state, ok := marketReadSessions(sessions, now)
	if !ok {
		return nil, fmt.Errorf("US equity session calendar unavailable for %s", now.UTC().Format(time.DateOnly))
	}
	var src marketReadSources
	var wg sync.WaitGroup
	history := func(dst **rpc.MarketHistoryResult, c rpc.ContractParams, r string) {
		wg.Go(func() {
			raw, _ := json.Marshal(rpc.MarketHistoryParams{Contract: c, Range: r})
			*dst, _ = s.handleMarketHistory(ctx, &rpc.Request{Params: raw})
		})
	}
	// daily reads a daily series from the chart cache, and through the
	// bounded history request only when the cache has no bar of the prior
	// session: gone from the cache, or last read before that session closed.
	daily := func(c rpc.ContractParams, cached func() *rpc.MarketHistoryResult) *rpc.MarketHistoryResult {
		r := cached()
		if marketReadDailyBar(r, c.Symbol, prior.Date) != nil {
			return r
		}
		if fresh, err := s.marketHistoryRequest(ctx, rpc.MarketHistoryParams{Contract: c, Range: "6M"}); err == nil {
			return fresh
		}
		return r
	}
	wg.Go(func() {
		src.SPXDaily = daily(marketReadSPX, func() *rpc.MarketHistoryResult {
			raw, _ := json.Marshal(rpc.MarketHistoryParams{Contract: marketReadSPX, Range: "6M"})
			r, _ := s.handleMarketHistory(ctx, &rpc.Request{Params: raw})
			return r
		})
	})
	history(&src.SPXIntraday, marketReadSPX, "1D")
	history(&src.VIXIntraday, marketReadVIX, "1D")
	wg.Go(func() {
		raw, _ := json.Marshal(rpc.MarketHistoryParams{Contract: marketReadSPY, Range: "1D"})
		selected, err := s.handleMarketHistory(ctx, &rpc.Request{Params: raw})
		if err != nil || selected == nil {
			return
		}
		src.SPYIntraday = selected
		// The selection keeps only the current day; the retained series
		// holds the prior sessions the baseline needs.
		if whole := s.retainedHistory(ctx, rpc.MarketHistoryParams{Contract: marketReadSPY, Range: "1D"}); whole != nil {
			src.SPYIntraday = whole
		}
	})
	contracts := marketReadLeaderContracts()
	src.Leaders = make([]marketReadLeaderSource, len(contracts))
	for i, c := range contracts {
		src.Leaders[i].Symbol = c.Symbol
		wg.Go(func() {
			src.Leaders[i].Daily = daily(c, func() *rpc.MarketHistoryResult {
				r, _ := s.retainedOrRequestedHistory(ctx, rpc.MarketHistoryParams{Contract: c, Range: "6M"}, now)
				return r
			})
			src.Leaders[i].Intraday = s.retainedHistory(ctx, rpc.MarketHistoryParams{Contract: c, Range: "1D"})
		})
	}
	reads := s.brokerReads()
	sem := make(chan struct{}, marketReadQuoteParallel)
	quote := func(dst **broker.BriefQuote, c rpc.ContractParams, open bool) {
		if reads == nil {
			return
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			q, err := reads.BriefQuote(ctx, broker.Instrument{Symbol: c.Symbol, SecType: c.SecType, Currency: c.Currency}, nil, marketReadQuoteTimeout, func(q broker.BriefQuote) bool {
				// A price and the prior close end the look; a company's look
				// also waits for the session open while there is one.
				return (q.Last > 0 || q.Mark > 0) && q.Close > 0 && (!open || q.Open > 0)
			})
			if err == nil || q.Last > 0 || q.Mark > 0 {
				*dst = &q
			}
		})
	}
	quote(&src.SPXQuote, marketReadSPX, false)
	quote(&src.VIXQuote, marketReadVIX, false)
	for i, c := range contracts {
		quote(&src.Leaders[i].Quote, c, state != rpc.MarketReadPre)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return assembleMarketRead(now, sessions, src)
}

// retainedHistory is a series exactly as the chart cache retains it, every
// recorded day included, or nil. It never asks the broker.
func (s *Server) retainedHistory(ctx context.Context, p rpc.MarketHistoryParams) *rpc.MarketHistoryResult {
	key, _, err := marketHistoryIdentity(p)
	if err != nil {
		return nil
	}
	saved, _, err := s.loadMarketHistory(ctx, key)
	if err != nil || saved == nil {
		return nil
	}
	r := saved.Result
	return &r
}

// historyFetchedAt is when the bars of a history result were read from the
// broker.
func historyFetchedAt(r *rpc.MarketHistoryResult) time.Time {
	if r.Cache != nil && !r.Cache.FetchedAt.IsZero() {
		return r.Cache.FetchedAt
	}
	return r.AsOf
}

// marketReadIntraday checks that r is the intraday series of symbol and
// fresh enough for the session state, and says why not.
func marketReadIntraday(r *rpc.MarketHistoryResult, symbol, state string, now time.Time) (time.Time, string) {
	if r == nil || r.Contract.Symbol != symbol || r.Interval != "5 mins" {
		return time.Time{}, symbol + " intraday bars unavailable"
	}
	fetched := historyFetchedAt(r)
	if state == rpc.MarketReadOpen && now.Sub(fetched) > marketReadStaleAfterMinutes*time.Minute {
		return fetched, fmt.Sprintf("%s bars were last read at %s, more than %d minutes ago", symbol, fetched.UTC().Format(time.RFC3339), marketReadStaleAfterMinutes)
	}
	return fetched, ""
}

// marketReadDailyBar is the regular-session daily bar of date, or nil.
func marketReadDailyBar(r *rpc.MarketHistoryResult, symbol, date string) *rpc.MarketHistoryPoint {
	if r == nil || r.Contract.Symbol != symbol || r.Interval != "1 day" || !r.RegularHoursOnly {
		return nil
	}
	for i := range r.Points {
		if r.Points[i].At.UTC().Format(time.DateOnly) == date {
			return &r.Points[i]
		}
	}
	return nil
}

// marketReadQuoteLast is a quote's current price, the last trade or else the
// broker's mark, or why it has none. A close alone is not a current price.
func marketReadQuoteLast(q *broker.BriefQuote) (*float64, time.Time, string) {
	if q == nil {
		return nil, time.Time{}, "quote unavailable"
	}
	price := q.Last
	if !isFinitePrice(price) {
		price = q.Mark
	}
	if !isFinitePrice(price) {
		return nil, time.Time{}, "quote has no current price"
	}
	return new(price), q.ObservedAt, ""
}

// assembleMarketRead measures the acquired inputs and draws the tags. It
// reads no clock and no broker, so a test can give it any market.
func assembleMarketRead(now time.Time, sessions []marketcal.Session, src marketReadSources) (*rpc.MarketReadResult, error) {
	session, prior, state, ok := marketReadSessions(sessions, now)
	if !ok {
		return nil, fmt.Errorf("US equity session calendar unavailable for %s", now.UTC().Format(time.DateOnly))
	}
	out := &rpc.MarketReadResult{AsOf: now, Thresholds: marketReadThresholds(), Tags: []rpc.MarketReadTag{},
		Session: rpc.MarketReadSession{Date: session.Date, State: state, Open: session.Open.UTC(), Close: session.Close.UTC()}}
	var priors []marketcal.Session
	for _, candidate := range sessions {
		if candidate.Date < session.Date {
			priors = append(priors, candidate)
		}
	}
	in := marketReadTagInputs{State: state}

	// Index.
	index := &rpc.MarketReadIndex{Symbol: "SPX", Bars: []rpc.MarketReadBar{}}
	if bar := marketReadDailyBar(src.SPXDaily, "SPX", prior.Date); bar != nil && bar.High != nil && bar.Low != nil {
		index.Prior = &rpc.MarketReadPrior{Date: prior.Date, High: *bar.High, Low: *bar.Low, Close: bar.Value}
	} else {
		index.PriorUnavailable = "SPX daily bar of " + prior.Date + " unavailable"
	}
	if _, reason := marketReadIntraday(src.SPXIntraday, "SPX", state, now); reason != "" {
		index.BarsUnavailable = reason
	} else {
		index.Bars = marketReadBars(src.SPXIntraday.Points, session, historyFetchedAt(src.SPXIntraday))
	}
	index.Last, index.LastAt, index.LastUnavailable = marketReadQuoteLast(src.SPXQuote)
	if index.Last == nil && len(index.Bars) > 0 {
		last := index.Bars[len(index.Bars)-1]
		index.Last, index.LastAt, index.LastUnavailable = new(last.Close), last.At.Add(marketReadBar), ""
	}
	switch {
	case index.Prior == nil:
		index.PositionUnavailable = "prior session range unavailable"
	case index.Last == nil:
		index.PositionUnavailable = "current price unavailable"
	case *index.Last > index.Prior.High:
		index.Position = "above_prior_high"
	case *index.Last < index.Prior.Low:
		index.Position = "below_prior_low"
	default:
		index.Position = "inside"
	}
	if fetched, reason := marketReadIntraday(src.SPYIntraday, "SPY", state, now); reason != "" {
		index.VolumeUnavailable = reason
	} else {
		index.Volume = marketReadVolume(src.SPYIntraday.Points, session, priors, fetched)
		in.Volume = index.Volume
	}
	out.Index = index
	in.Prior, in.IndexBars = index.Prior, index.Bars

	// VIX.
	vix := &rpc.MarketReadVIX{Line: marketReadVIXLine, Bars: []rpc.MarketReadVIXBar{}}
	vix.Last, vix.LastAt, vix.LastUnavailable = marketReadQuoteLast(src.VIXQuote)
	if src.VIXQuote != nil && isFinitePrice(src.VIXQuote.Close) {
		vix.PriorClose = new(src.VIXQuote.Close)
	} else {
		vix.PriorCloseUnavailable = "VIX previous close not quoted"
	}
	if _, reason := marketReadIntraday(src.VIXIntraday, "VIX", state, now); reason != "" {
		vix.BarsUnavailable = reason
	} else {
		for _, b := range marketReadBars(src.VIXIntraday.Points, session, historyFetchedAt(src.VIXIntraday)) {
			vix.Bars = append(vix.Bars, rpc.MarketReadVIXBar{At: b.At, Close: b.Close})
		}
	}
	if vix.Last == nil && len(vix.Bars) > 0 {
		last := vix.Bars[len(vix.Bars)-1]
		vix.Last, vix.LastAt, vix.LastUnavailable = new(last.Close), last.At.Add(marketReadBar), ""
	}
	if vix.Last != nil {
		vix.Position = "below_line"
		if *vix.Last > marketReadVIXLine {
			vix.Position = "above_line"
		}
		if vix.PriorClose != nil {
			vix.ChangePct = tapeFinite(100 * (*vix.Last / *vix.PriorClose - 1))
		}
	}
	out.VIX = vix
	in.VIXBars, in.VIXPriorClose = vix.Bars, vix.PriorClose

	// Leaders.
	leaders := &rpc.MarketReadLeaders{Method: tapeLeaderMethod + ", one share line per company", Rows: []rpc.MarketReadLeader{}}
	for _, l := range src.Leaders {
		row := rpc.MarketReadLeader{Symbol: l.Symbol}
		if bar := marketReadDailyBar(l.Daily, l.Symbol, prior.Date); bar != nil {
			row.PriorClose = new(bar.Value)
			if bar.Low != nil {
				row.PriorLow = new(*bar.Low)
			}
		}
		row.Last, row.LastAt, _ = marketReadQuoteLast(l.Quote)
		if state != rpc.MarketReadPre {
			// Before the open the opening tick still names the last session.
			if l.Quote != nil && isFinitePrice(l.Quote.Open) {
				row.Open, row.OpenSource = new(l.Quote.Open), "session_open"
			} else if first := marketReadFirstBar(l.Intraday, l.Symbol, session); first != nil {
				row.Open, row.OpenSource = new(first.Open), "first_bar"
			} else {
				row.StateUnavailable = "session open not quoted and no intraday bar retained"
			}
		}
		if state == rpc.MarketReadOpen {
			marketReadLeaderState(&row)
		} else {
			row.StateUnavailable = "states are read while the session is open"
		}
		leaders.Rows = append(leaders.Rows, row)
	}
	leaders.GappedUp, leaders.HoldingGapUp = marketReadLeaderCounts(leaders.Rows)
	out.Leaders = leaders
	in.Leaders = leaders.Rows

	out.Tags = marketReadTags(in)
	return out, nil
}

// marketReadFirstBar is the bar that opened the session in a retained
// intraday series, or nil.
func marketReadFirstBar(r *rpc.MarketHistoryResult, symbol string, session marketcal.Session) *rpc.MarketReadBar {
	if r == nil || r.Contract.Symbol != symbol || r.Interval != "5 mins" {
		return nil
	}
	for _, p := range r.Points {
		if p.At.Equal(session.Open) && p.Open != nil && isFinitePrice(*p.Open) {
			return &rpc.MarketReadBar{At: p.At, Open: *p.Open, Close: p.Value}
		}
	}
	return nil
}
