package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// marketReadCacheFor is how long one read answers every caller. A console
// polling the read then costs the broker one read per window, however many
// pollers there are.
const marketReadCacheFor = 30 * time.Second

// marketReadBudget is how long a read waits for its inputs. What has not
// arrived by then is unavailable with a reason; the slow acquisitions go on
// without the caller and land in a later read.
const marketReadBudget = 5 * time.Second

// marketReadServeLastFor is how old a last good read may be and still answer
// a caller while the next read is in flight.
const marketReadServeLastFor = 5 * time.Minute

// marketReadRetryAfter spaces the background history reads the read starts
// for a series it found missing, so a poller does not ask every window.
const marketReadRetryAfter = 10 * time.Minute

// marketReadQuoteParallel and marketReadQuoteTimeout bound the transient
// quote looks, so all twelve fit inside the budget.
const (
	marketReadQuoteParallel = 6
	marketReadQuoteTimeout  = 1500 * time.Millisecond
)

// marketReadCache holds the last read and the one in flight, the background
// history reads started recently, and the session whose volume baseline was
// seeded. It is memory only: nothing of a read outlives the process.
type marketReadCache struct {
	mu        sync.Mutex
	result    *rpc.MarketReadResult
	at        time.Time
	flight    chan struct{}
	requested map[string]time.Time
	seeded    string
	// scope is the account scope the result was read in.
	scope brokerStateScope
}

// The contracts a read uses, spelled as the daemon's other readers spell
// them so their chart series are shared rather than duplicated.
var (
	marketReadSPX = rpc.ContractParams{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	marketReadVIX = rpc.ContractParams{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	marketReadSPY = rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	marketReadNDX = rpc.ContractParams{Symbol: "NDX", SecType: "IND", Exchange: "NASDAQ", Currency: "USD"}
	marketReadQQQ = rpc.ContractParams{Symbol: "QQQ", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	// The index futures roots, spelled as market.snapshot spells them.
	marketReadES = rpc.ContractParams{Symbol: "ES", SecType: "FUT", Exchange: "CME", Currency: "USD"}
	marketReadNQ = rpc.ContractParams{Symbol: "NQ", SecType: "FUT", Exchange: "CME", Currency: "USD"}
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
	// The held block names the account's underlyings, so a read serves only
	// the account scope it was taken in.
	scope := s.currentBrokerStateScope()
	for {
		cache.mu.Lock()
		if cache.result != nil && cache.scope != scope {
			cache.result = nil
		}
		age := s.nowUTC().Sub(cache.at)
		if cache.result != nil && age < marketReadCacheFor {
			result := cache.result
			cache.mu.Unlock()
			return result, nil
		}
		if flight := cache.flight; flight != nil {
			// A read is in flight: a recent last read answers now, its as_of
			// saying when; otherwise wait for the read.
			if cache.result != nil && age < marketReadServeLastFor {
				result := cache.result
				cache.mu.Unlock()
				return result, nil
			}
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
		// does not cancel it for the rest. It returns at its budget.
		result, err := s.readMarket(context.WithoutCancel(ctx))
		cache.mu.Lock()
		cache.flight = nil
		if err == nil {
			cache.result, cache.at, cache.scope = result, s.nowUTC(), scope
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
	// ES and NQ are the front index futures, looked at before the open only.
	ES, NQ *marketReadFutureSource
	// NDX and QQQ are looked at while the session is open; QQQ stands in
	// when the index does not quote.
	NDXQuote, QQQQuote *broker.BriefQuote
	// Held is one source per held underlying; HeldUnavailable says why there
	// are none and HeldTruncated that the cap cut the list.
	Held            []marketReadHeldSource
	HeldUnavailable string
	HeldTruncated   bool
}

// marketReadFutureSource is one look at a front future: the dated contract
// it resolved to and its line.
type marketReadFutureSource struct {
	Contract string
	Quote    broker.BriefQuote
}

type marketReadLeaderSource struct {
	Symbol string
	Quote  *broker.BriefQuote
	Daily  *rpc.MarketHistoryResult
	// Intraday is the company's intraday series when the chart cache already
	// holds it; it is never requested for the read.
	Intraday *rpc.MarketHistoryResult
}

// readMarket acquires the read's inputs and assembles what has arrived
// within the budget. History comes from the chart cache only: a series the
// cache does not hold, or holds without a bar the read needs, is read in the
// background through the bounded history request on the background pacing
// lane and serves a later read. The intraday series are followed, so the
// refresh worker keeps them current. Quotes are brief looks through the
// broker seam, each line released when read.
func (s *Server) readMarket(ctx context.Context) (*rpc.MarketReadResult, error) {
	now := s.nowUTC()
	sessions, err := tapeArchiveCalendar(now.AddDate(0, 0, -45), 46)
	if err != nil {
		return nil, err
	}
	session, prior, state, ok := marketReadSessions(sessions, now)
	if !ok {
		return nil, fmt.Errorf("US equity session calendar unavailable for %s", now.UTC().Format(time.DateOnly))
	}
	var mu sync.Mutex
	var src marketReadSources
	set := func(f func()) {
		mu.Lock()
		f()
		mu.Unlock()
	}
	var wg sync.WaitGroup
	intraday := func(dst **rpc.MarketHistoryResult, c rpc.ContractParams) {
		wg.Go(func() {
			p := rpc.MarketHistoryParams{Contract: c, Range: "1D"}
			s.rememberMarketHistory(p)
			r := s.retainedHistory(ctx, p)
			// A missing series, or one the refresh worker has let go stale,
			// is read in the background, at most once per retry window.
			if _, reason := marketReadIntraday(r, c.Symbol, state, now); reason != "" {
				s.requestMarketReadHistory(p, now)
			}
			set(func() { *dst = r })
		})
	}
	// daily serves a daily series from the chart cache; one without the bars
	// the read needs (the prior session's, and once the session has closed
	// its own) is read in the background.
	daily := func(c rpc.ContractParams, follow bool, dst func(*rpc.MarketHistoryResult)) {
		wg.Go(func() {
			p := rpc.MarketHistoryParams{Contract: c, Range: "6M"}
			if follow {
				s.rememberMarketHistory(p)
			}
			r := s.retainedHistory(ctx, p)
			if marketReadDailyBar(r, c.Symbol, prior.Date) == nil || state == rpc.MarketReadClosed && marketReadDailyBar(r, c.Symbol, session.Date) == nil {
				s.requestMarketReadHistory(p, now)
			}
			set(func() { dst(r) })
		})
	}
	daily(marketReadSPX, true, func(r *rpc.MarketHistoryResult) { src.SPXDaily = r })
	intraday(&src.SPXIntraday, marketReadSPX)
	intraday(&src.VIXIntraday, marketReadVIX)
	intraday(&src.SPYIntraday, marketReadSPY)
	contracts := marketReadLeaderContracts()
	src.Leaders = make([]marketReadLeaderSource, len(contracts))
	for i, c := range contracts {
		src.Leaders[i].Symbol = c.Symbol
		daily(c, false, func(r *rpc.MarketHistoryResult) { src.Leaders[i].Daily = r })
		wg.Go(func() {
			p := rpc.MarketHistoryParams{Contract: c, Range: "1D"}
			r := s.retainedHistory(ctx, p)
			// The first bar is the open's fallback where the quote names
			// none. A leader nothing else charts had no bars at all, so
			// its gap went unread all session (2026-11-25). In the session
			// a series without that bar is read once in the background; it
			// is not followed, the first bar being all the read needs.
			if state == rpc.MarketReadOpen && marketReadFirstBar(r, c.Symbol, session) == nil {
				s.requestMarketReadHistory(p, now)
			}
			set(func() { src.Leaders[i].Intraday = r })
		})
	}
	reads := s.brokerReads()
	sem := make(chan struct{}, marketReadQuoteParallel)
	quote := func(dst **broker.BriefQuote, c rpc.ContractParams, open bool) {
		if reads == nil && s.marketReadLookForTest == nil {
			return
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var q broker.BriefQuote
			var err error
			if s.marketReadLookForTest != nil {
				q, _, err = s.marketReadLookForTest(ctx, c)
			} else {
				q, err = reads.BriefQuote(ctx, broker.Instrument{Symbol: c.Symbol, SecType: c.SecType, Currency: c.Currency}, nil, marketReadQuoteTimeout, func(q broker.BriefQuote) bool {
					// A price and the prior close end the look; a company's look
					// also waits for the session open while there is one.
					return (q.Last > 0 || q.Mark > 0) && q.Close > 0 && (!open || q.Open > 0)
				})
			}
			if err == nil || q.Last > 0 || q.Mark > 0 {
				set(func() { *dst = &q })
			}
		})
	}
	quote(&src.SPXQuote, marketReadSPX, false)
	quote(&src.VIXQuote, marketReadVIX, false)
	for i, c := range contracts {
		quote(&src.Leaders[i].Quote, c, state != rpc.MarketReadPre)
	}
	switch state {
	case rpc.MarketReadPre:
		// Before the open the index futures indicate where the session opens.
		future := func(dst **marketReadFutureSource, root rpc.ContractParams) {
			if reads == nil && s.marketReadLookForTest == nil {
				return
			}
			wg.Go(func() {
				q, contract, err := s.marketReadFrontLook(ctx, reads, root)
				if err == nil && (q.Last > 0 || q.Mark > 0) {
					set(func() { *dst = &marketReadFutureSource{Contract: contract, Quote: q} })
				}
			})
		}
		future(&src.ES, marketReadES)
		future(&src.NQ, marketReadNQ)
	case rpc.MarketReadOpen:
		quote(&src.NDXQuote, marketReadNDX, false)
		quote(&src.QQQQuote, marketReadQQQ, false)
	}
	s.acquireMarketReadHeld(ctx, reads, contracts, &wg, set, daily, &src)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	budget := time.NewTimer(marketReadBudget)
	defer budget.Stop()
	select {
	case <-done:
	case <-budget.C:
	case <-ctx.Done():
	}
	// Take what has arrived; anything later is left to the next read.
	mu.Lock()
	got := src
	got.Leaders = append([]marketReadLeaderSource(nil), src.Leaders...)
	got.Held = append([]marketReadHeldSource(nil), src.Held...)
	mu.Unlock()
	out, err := assembleMarketRead(now, sessions, got)
	if err == nil && state != rpc.MarketReadClosed && out.Index.Volume != nil && out.Index.Volume.BaselineSessions < marketReadBaselineMinSessions {
		s.seedMarketReadBaseline(session.Date)
	}
	return out, err
}

// marketReadFrontLook resolves a futures root to its front contract and takes
// one brief quote of it, as market.snapshot does: the resolution with its own
// short timeout and cache, the quote through the transient snapshot, which
// releases its line when read.
func (s *Server) marketReadFrontLook(ctx context.Context, reads broker.Reads, root rpc.ContractParams) (broker.BriefQuote, string, error) {
	if s.marketReadLookForTest != nil {
		return s.marketReadLookForTest(ctx, root)
	}
	c, err := reads.FrontContract(ctx, broker.Instrument{Symbol: root.Symbol, SecType: "FUT", Venue: root.Exchange, Currency: root.Currency}, s.nowUTC())
	if err != nil {
		return broker.BriefQuote{}, "", err
	}
	conID, _ := c.IDInt()
	contract := rpc.ContractParams{ConID: conID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Venue, Currency: c.Currency, Expiry: c.Expiry, LocalSymbol: c.LocalSymbol, TradingClass: c.TradingClass, Multiplier: c.Multiplier}
	raw, err := json.Marshal(rpc.QuoteSnapshotParams{Contract: contract, TimeoutMs: int(marketReadQuoteTimeout / time.Millisecond)})
	if err != nil {
		return broker.BriefQuote{}, c.LocalSymbol, err
	}
	q, err := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	if err != nil {
		return broker.BriefQuote{}, c.LocalSymbol, err
	}
	if q == nil {
		return broker.BriefQuote{}, c.LocalSymbol, errors.New("quote absent")
	}
	out := broker.BriefQuote{TradeAt: q.TradeAt, ObservedAt: q.QuotePriceAt}
	if out.ObservedAt.IsZero() {
		out.ObservedAt = q.ReceivedAt
	}
	for _, p := range []*float64{q.Last, q.Mark, q.QuotePrice} {
		if p != nil && isFinitePrice(*p) {
			out.Last = *p
			break
		}
	}
	if q.PrevClose != nil && isFinitePrice(*q.PrevClose) {
		out.Close = *q.PrevClose
	}
	return out, c.LocalSymbol, nil
}

// requestMarketReadHistory reads one series in the background through the
// bounded, coalescing history request, at most once per retry window, on the
// background pacing lane so it never waits ahead of an interactive request.
func (s *Server) requestMarketReadHistory(p rpc.MarketHistoryParams, now time.Time) {
	key, _, err := marketHistoryIdentity(p)
	if err != nil {
		return
	}
	cache := &s.marketRead
	cache.mu.Lock()
	if at, ok := cache.requested[key]; ok && now.Sub(at) < marketReadRetryAfter {
		cache.mu.Unlock()
		return
	}
	if cache.requested == nil {
		cache.requested = map[string]time.Time{}
	}
	for k, at := range cache.requested {
		if now.Sub(at) >= marketReadRetryAfter {
			delete(cache.requested, k)
		}
	}
	cache.requested[key] = now
	cache.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(context.Background(), ibkrlib.PriorityBackground), marketHistoryRefreshWindow)
		defer cancel()
		_, _ = s.marketHistoryRequest(ctx, p)
	}()
}

// seedMarketReadBaseline reads SPY's intraday window in full once per
// session, in the background, when the retained series holds too few prior
// sessions for a volume baseline. A routine refresh reads only the recent
// tail, so a record that lost its earlier days would otherwise stay short.
func (s *Server) seedMarketReadBaseline(date string) {
	cache := &s.marketRead
	cache.mu.Lock()
	if cache.seeded == date {
		cache.mu.Unlock()
		return
	}
	cache.seeded = date
	cache.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(context.Background(), ibkrlib.PriorityBackground), marketHistoryRefreshWindow)
		defer cancel()
		_ = s.seedIntradayHistory(ctx, rpc.MarketHistoryParams{Contract: marketReadSPY, Range: "1D"})
	}()
}

// seedIntradayHistory reads an intraday series' whole padded window from the
// broker and adds it to the retained record, removing nothing recorded.
func (s *Server) seedIntradayHistory(ctx context.Context, p rpc.MarketHistoryParams) error {
	key, p, err := marketHistoryIdentity(p)
	if err != nil {
		return err
	}
	fetch := s.fetchMarketHistoryDays
	if s.marketHistoryFetchForTest != nil {
		fetch = s.marketHistoryFetchForTest
	}
	now := time.Now()
	fresh, err := fetch(ctx, p, 0, now)
	if err == nil && fresh == nil {
		err = errors.New("history response absent")
	}
	if err == nil {
		err = s.validStoredHistory(*fresh)
	}
	if err == nil && !historyResponseMatches(p, *fresh, now) {
		err = errors.New("history response identity or clock changed")
	}
	if err != nil {
		return err
	}
	s.marketData.storeMu.Lock()
	defer s.marketData.storeMu.Unlock()
	latest, _, err := s.loadMarketHistory(ctx, key)
	if err != nil {
		return err
	}
	if latest != nil && (latest.Result.Contract != fresh.Contract || latest.Result.PriceBasis != fresh.PriceBasis) {
		return errors.New("retained series changed identity")
	}
	if latest != nil && latest.Result.AsOf.After(fresh.AsOf) {
		return nil // a newer read landed meanwhile; the next seed can add the days
	}
	next := mergeMarketHistory(key, latest, *fresh, false, now)
	if s.coreStore == nil {
		return nil
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return err
	}
	_, err = s.coreStore.SaveChartCache(ctx, key, payload)
	return err
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
	fetched, stale, reason := marketReadIntradaySeries(r, symbol, state, now)
	if reason == "" && stale {
		reason = marketReadStaleReason
	}
	return fetched, reason
}

// marketReadStaleReason says why bars are stale; when they were read is in
// bars_as_of.
var marketReadStaleReason = fmt.Sprintf("bars not refreshed in the last %d minutes", marketReadStaleAfterMinutes)

// marketReadIntradaySeries checks that r is the intraday series of symbol,
// saying why not, and reports when it was read and whether that is longer
// ago than the session state allows.
func marketReadIntradaySeries(r *rpc.MarketHistoryResult, symbol, state string, now time.Time) (fetched time.Time, stale bool, unavailable string) {
	if r == nil || r.Contract.Symbol != symbol || r.Interval != "5 mins" {
		return time.Time{}, false, symbol + " intraday bars unavailable"
	}
	fetched = historyFetchedAt(r)
	return fetched, state == rpc.MarketReadOpen && now.Sub(fetched) > marketReadStaleAfterMinutes*time.Minute, ""
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

// marketReadQuoteLast is a quote's price, the last trade or else the broker's
// mark, with its time and what it is, or why there is none. A broker trade
// time is carried as it is. Before the session opens a price equal to the
// previous close is that close and carries the prior session's close time:
// a line opened now delivers the old print with a fresh receipt time. A trade
// timed before the session's New York day is the prior session's last print,
// not a price of today, whatever it differs from the official close by; it
// keeps its real time.
func marketReadQuoteLast(q *broker.BriefQuote, state string, session, prior marketcal.Session) (*float64, time.Time, string, string) {
	if q == nil {
		return nil, time.Time{}, "", "quote unavailable"
	}
	price := q.Last
	if !isFinitePrice(price) {
		price = q.Mark
	}
	switch {
	case !isFinitePrice(price):
		return nil, time.Time{}, "", "quote has no current price"
	case state == rpc.MarketReadPre && q.Close > 0 && price == q.Close:
		return new(price), prior.Close.UTC(), "prior_close", ""
	case state == rpc.MarketReadPre && !q.TradeAt.IsZero() && q.TradeAt.Before(marketReadDayStart(session)):
		return new(price), q.TradeAt, "prior_close", ""
	case !q.TradeAt.IsZero():
		return new(price), q.TradeAt, "trade", ""
	}
	return new(price), q.ObservedAt, "quote", ""
}

// marketReadDayStart is midnight in New York of the session's day.
func marketReadDayStart(session marketcal.Session) time.Time {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return session.Open
	}
	open := session.Open.In(loc)
	return time.Date(open.Year(), open.Month(), open.Day(), 0, 0, 0, 0, loc)
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
	indexStale := false
	if fetched, stale, reason := marketReadIntradaySeries(src.SPXIntraday, "SPX", state, now); reason != "" {
		index.BarsUnavailable = reason
	} else {
		// Late bars still draw the line; they draw no tags.
		index.Bars, index.BarsAsOf, index.BarsStale, indexStale = marketReadBars(src.SPXIntraday.Points, session, fetched), fetched, stale, stale
		if stale {
			index.BarsStaleReason = marketReadStaleReason
		}
	}
	index.Last, index.LastAt, index.LastSource, index.LastUnavailable = marketReadQuoteLast(src.SPXQuote, state, session, prior)
	if index.Last == nil && len(index.Bars) > 0 {
		last := index.Bars[len(index.Bars)-1]
		index.Last, index.LastAt, index.LastSource, index.LastUnavailable = new(last.Close), last.At.Add(marketReadBar), "bar", ""
	}
	switch {
	case state == rpc.MarketReadPre:
		// Before the open the index has no position of today, whatever its last
		// price; the position the futures imply is in indicated.
		index.PositionUnavailable = "the session has not opened"
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
	if fetched, stale, reason := marketReadIntradaySeries(src.SPYIntraday, "SPY", state, now); reason != "" {
		index.VolumeUnavailable = reason
	} else {
		index.Volume = marketReadVolume(src.SPYIntraday.Points, session, priors, fetched)
		index.Volume.BarsAsOf, index.Volume.BarsStale = fetched, stale
		if stale {
			index.Volume.BarsStaleReason = marketReadStaleReason
		} else {
			in.Volume = index.Volume
		}
	}
	out.Index = index
	in.Prior, in.IndexBars, in.IndexStale = index.Prior, index.Bars, indexStale
	if first := marketReadFirstBar(src.SPXIntraday, "SPX", session); first != nil && len(index.Bars) > 0 && index.Bars[0].At.Equal(session.Open) {
		in.SessionOpen = new(first.Open)
	}

	// VIX.
	vix := &rpc.MarketReadVIX{Line: marketReadVIXLine, Bars: []rpc.MarketReadVIXBar{}}
	vix.Last, vix.LastAt, vix.LastSource, vix.LastUnavailable = marketReadQuoteLast(src.VIXQuote, state, session, prior)
	if src.VIXQuote != nil && isFinitePrice(src.VIXQuote.Close) {
		vix.PriorClose = new(src.VIXQuote.Close)
	} else {
		vix.PriorCloseUnavailable = "VIX previous close not quoted"
	}
	if fetched, stale, reason := marketReadIntradaySeries(src.VIXIntraday, "VIX", state, now); reason != "" {
		vix.BarsUnavailable = reason
	} else {
		for _, b := range marketReadBars(src.VIXIntraday.Points, session, fetched) {
			vix.Bars = append(vix.Bars, rpc.MarketReadVIXBar{At: b.At, Close: b.Close})
		}
		vix.BarsAsOf, vix.BarsStale, in.VIXStale = fetched, stale, stale
		if stale {
			vix.BarsStaleReason = marketReadStaleReason
		}
	}
	if vix.Last == nil && len(vix.Bars) > 0 {
		last := vix.Bars[len(vix.Bars)-1]
		vix.Last, vix.LastAt, vix.LastSource, vix.LastUnavailable = new(last.Close), last.At.Add(marketReadBar), "bar", ""
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
		leaders.Rows = append(leaders.Rows, marketReadLeaderRow(l, state, session, prior))
	}
	leaders.GappedUp, leaders.HoldingGapUp = marketReadLeaderCounts(leaders.Rows)
	leaders.GappedDown, leaders.HoldingGapDown, _ = marketReadLeaderDownCounts(leaders.Rows)
	_, leaders.BelowPriorLow = marketReadLeaderBelowPriorLow(leaders.Rows)
	out.Leaders = leaders
	in.Leaders = leaders.Rows
	out.Held = marketReadHeldBlock(now, state, session, prior, src, leaders.Rows)

	out.Tags, out.TagEvents = marketReadTags(in)
	out.Indications = []rpc.MarketReadIndication{}
	switch state {
	case rpc.MarketReadPre:
		out.Indicated = marketReadIndicate(now, index.Prior, src, leaders.Rows)
		out.Indications = marketReadIndications(out.Indicated, index.Prior)
		out.RelativeUnavailable = "before the open the Nasdaq is read in indicated"
	case rpc.MarketReadOpen:
		out.Relative = marketReadRelate(index, src, state, session, prior)
		if tag := marketReadRelativeTag(out.Relative); tag != nil {
			out.Tags = append(out.Tags, *tag)
		}
	default:
		out.RelativeUnavailable = "the session has closed; the Nasdaq beside the S&P 500 is read while it is open"
	}
	return out, nil
}

// marketReadLeaderRow reads one company against its opening gap: its prior
// close and low, its open, its last and its state.
func marketReadLeaderRow(l marketReadLeaderSource, state string, session, prior marketcal.Session) rpc.MarketReadLeader {
	row := rpc.MarketReadLeader{Symbol: l.Symbol}
	if bar := marketReadDailyBar(l.Daily, l.Symbol, prior.Date); bar != nil {
		row.PriorClose, row.PriorCloseSource = new(bar.Value), "daily_bar"
		if bar.Low != nil {
			row.PriorLow = new(*bar.Low)
		}
	} else if state != rpc.MarketReadClosed && l.Quote != nil && isFinitePrice(l.Quote.Close) {
		// Before and during the session the quote's previous close is the
		// prior session's close. After the close it can already name
		// today's, so it stands in only until then. The prior low has no
		// such stand-in.
		row.PriorClose, row.PriorCloseSource = new(l.Quote.Close), "quote_previous_close"
	}
	var lastSource string
	row.Last, row.LastAt, lastSource, _ = marketReadQuoteLast(l.Quote, state, session, prior)
	if state == rpc.MarketReadPre {
		marketReadIndicatedGap(&row, lastSource)
	}
	closing := marketReadDailyBar(l.Daily, l.Symbol, session.Date)
	if state == rpc.MarketReadClosed {
		// A closed session is read at its close, never at a later print.
		row.Last, row.LastAt = nil, time.Time{}
		if closing != nil {
			row.Last, row.LastAt = new(closing.Value), session.Close.UTC()
		}
	}
	if state == rpc.MarketReadClosed && closing != nil && closing.Open != nil && isFinitePrice(*closing.Open) {
		row.Open, row.OpenSource = new(*closing.Open), "daily_bar"
	} else if state != rpc.MarketReadPre {
		// Before the open the opening tick still names the last session.
		if l.Quote != nil && isFinitePrice(l.Quote.Open) {
			row.Open, row.OpenSource = new(l.Quote.Open), "session_open"
		} else if first := marketReadFirstBar(l.Intraday, l.Symbol, session); first != nil {
			row.Open, row.OpenSource = new(first.Open), "first_bar"
		} else {
			row.StateUnavailable = "session open not quoted and no intraday bar retained"
		}
	}
	switch {
	case state == rpc.MarketReadPre:
		row.StateUnavailable = "the session has not opened"
	case state == rpc.MarketReadClosed && closing == nil:
		row.StateUnavailable = "the session's close is not yet in the daily bars"
	default:
		marketReadLeaderState(&row)
	}
	switch {
	case row.PriorClose == nil:
		row.ChangeUnavailable = "prior close unavailable"
	case row.Last == nil && state == rpc.MarketReadClosed:
		row.ChangeUnavailable = "the session's close is not yet in the daily bars"
	case row.Last == nil:
		row.ChangeUnavailable = "current price unavailable"
	case state == rpc.MarketReadPre && lastSource == "prior_close":
		row.ChangeUnavailable = "no pre-market trade today: the price is the prior session's"
	default:
		row.ChangePct = tapeFinite(100 * (*row.Last / *row.PriorClose - 1))
		if row.ChangePct == nil {
			row.ChangeUnavailable = "change could not be measured"
		}
	}
	return row
}

// marketReadIndicatedGap sets a leader's pre-market price against its prior
// close, or says why there is none. A price of the prior session (its close,
// or its last print timed before the session's day) indicates nothing about
// today.
func marketReadIndicatedGap(row *rpc.MarketReadLeader, lastSource string) {
	switch {
	case row.PriorClose == nil:
		row.IndicatedGapUnavailable = "prior close unavailable"
	case row.Last == nil:
		row.IndicatedGapUnavailable = "no pre-market price"
	case lastSource == "prior_close":
		row.IndicatedGapUnavailable = "no pre-market trade today: the price is the prior session's"
	default:
		row.IndicatedGapPct = tapeFinite(100 * (*row.Last / *row.PriorClose - 1))
		if row.IndicatedGapPct == nil {
			row.IndicatedGapUnavailable = "indicated gap could not be measured"
		}
	}
}

// marketReadFuture is one front future's last price against its prior close,
// or why it has none.
func marketReadFuture(f *marketReadFutureSource, root string) (*rpc.MarketReadIndicatedFuture, string) {
	if f == nil {
		return nil, root + " front future not quoted"
	}
	last := f.Quote.Last
	if !isFinitePrice(last) {
		last = f.Quote.Mark
	}
	if !isFinitePrice(last) {
		return nil, root + " front future quote has no price"
	}
	if !isFinitePrice(f.Quote.Close) {
		return nil, root + " front future previous close not quoted"
	}
	change := tapeFinite(100 * (last/f.Quote.Close - 1))
	if change == nil {
		return nil, root + " front future change could not be measured"
	}
	source := root + " front future"
	if f.Contract != "" {
		source += " " + f.Contract
	}
	at := f.Quote.TradeAt
	if at.IsZero() {
		at = f.Quote.ObservedAt
	}
	return &rpc.MarketReadIndicatedFuture{Source: source, Last: last, LastAt: at, PriorClose: f.Quote.Close, ChangePct: *change}, ""
}

// marketReadIndicate measures what the pre-market prices indicate for the
// open: the S&P 500 future's change applied to the index's prior close, the
// Nasdaq future beside it, and the leaders by their indicated gaps.
func marketReadIndicate(now time.Time, prior *rpc.MarketReadPrior, src marketReadSources, rows []rpc.MarketReadLeader) *rpc.MarketReadIndicated {
	ind := &rpc.MarketReadIndicated{AsOf: now}
	if es, reason := marketReadFuture(src.ES, "ES"); reason != "" {
		ind.IndexUnavailable = reason
	} else {
		x := &rpc.MarketReadIndicatedIndex{Source: es.Source, Last: es.Last, LastAt: es.LastAt, PriorClose: es.PriorClose, ChangePct: es.ChangePct}
		if prior == nil {
			x.ImpliedUnavailable = "SPX prior session range unavailable"
		} else {
			level := prior.Close * (1 + es.ChangePct/100)
			x.ImpliedLevel = &level
			switch {
			case level > prior.High:
				x.ImpliedPosition = "above_prior_high"
			case level < prior.Low:
				x.ImpliedPosition = "below_prior_low"
			default:
				x.ImpliedPosition = "inside"
			}
		}
		ind.Index = x
	}
	ind.Nasdaq, ind.NasdaqUnavailable = marketReadFuture(src.NQ, "NQ")
	l := &rpc.MarketReadIndicatedLeaders{}
	for _, row := range rows {
		if row.IndicatedGapPct == nil || row.Last == nil {
			continue
		}
		l.Quoted++
		switch gap := *row.IndicatedGapPct; {
		case gap >= marketReadGapMinPct:
			l.Up++
		case gap <= -marketReadGapMinPct:
			l.Down++
		default:
			l.Flat++
		}
		if row.PriorLow != nil {
			switch last, low := *row.Last, *row.PriorLow; {
			case last < low:
				l.BelowPriorLow++
			case last <= low*(1+marketReadNearLowPct/100):
				l.NearPriorLow++
			}
		}
	}
	if l.Quoted == 0 {
		ind.LeadersUnavailable = "no leader has a pre-market price"
	} else {
		ind.Leaders = l
	}
	return ind
}

// marketReadNasdaqLine is the Nasdaq 100's day change from the index's look,
// or from QQQ's when the index did not quote, or why neither serves.
func marketReadNasdaqLine(ndx, qqq *broker.BriefQuote, state string, session, prior marketcal.Session) (*rpc.MarketReadRelativeLine, string) {
	for _, c := range []struct {
		q              *broker.BriefQuote
		symbol, source string
	}{{ndx, "NDX", "NDX index"}, {qqq, "QQQ", "QQQ ETF, the index did not quote"}} {
		last, at, _, reason := marketReadQuoteLast(c.q, state, session, prior)
		if reason != "" || !isFinitePrice(c.q.Close) {
			continue
		}
		change := tapeFinite(100 * (*last/c.q.Close - 1))
		if change == nil {
			continue
		}
		return &rpc.MarketReadRelativeLine{Symbol: c.symbol, Source: c.source, Last: *last, LastAt: at, PriorClose: c.q.Close, ChangePct: *change}, ""
	}
	return nil, "neither the Nasdaq 100 index nor QQQ quoted a price and a previous close"
}

// marketReadRelate sets the Nasdaq 100 beside the S&P 500 by their day
// changes, each from its own prior close.
func marketReadRelate(index *rpc.MarketReadIndex, src marketReadSources, state string, session, prior marketcal.Session) *rpc.MarketReadRelative {
	rel := &rpc.MarketReadRelative{}
	rel.Nasdaq, rel.NasdaqUnavailable = marketReadNasdaqLine(src.NDXQuote, src.QQQQuote, state, session, prior)
	var priorClose *float64
	switch {
	case index.Prior != nil:
		priorClose = new(index.Prior.Close)
	case src.SPXQuote != nil && isFinitePrice(src.SPXQuote.Close):
		priorClose = new(src.SPXQuote.Close)
	}
	switch {
	case index.Last == nil:
		rel.SP500ChangeUnavailable = "SPX current price unavailable"
	case priorClose == nil:
		rel.SP500ChangeUnavailable = "SPX prior close unavailable"
	default:
		rel.SP500ChangePct = tapeFinite(100 * (*index.Last / *priorClose - 1))
		if rel.SP500ChangePct == nil {
			rel.SP500ChangeUnavailable = "SPX change could not be measured"
		}
	}
	if rel.Nasdaq != nil && rel.SP500ChangePct != nil {
		rel.SpreadPct = new(rel.Nasdaq.ChangePct - *rel.SP500ChangePct)
	} else {
		rel.SpreadUnavailable = "both day changes are needed"
	}
	return rel
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
