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
			if r == nil {
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
			r := s.retainedHistory(ctx, rpc.MarketHistoryParams{Contract: c, Range: "1D"})
			set(func() { src.Leaders[i].Intraday = r })
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
				set(func() { *dst = &q })
			}
		})
	}
	quote(&src.SPXQuote, marketReadSPX, false)
	quote(&src.VIXQuote, marketReadVIX, false)
	for i, c := range contracts {
		quote(&src.Leaders[i].Quote, c, state != rpc.MarketReadPre)
	}
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
	mu.Unlock()
	out, err := assembleMarketRead(now, sessions, got)
	if err == nil && state != rpc.MarketReadClosed && out.Index.Volume != nil && out.Index.Volume.BaselineSessions < marketReadBaselineMinSessions {
		s.seedMarketReadBaseline(session.Date)
	}
	return out, err
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

// marketReadQuoteLast is a quote's price, the last trade or else the broker's
// mark, with its time and what it is, or why there is none. A broker trade
// time is carried as it is. Before the session opens a price equal to the
// previous close is that close and carries the prior session's close time:
// a line opened now delivers the old print with a fresh receipt time.
func marketReadQuoteLast(q *broker.BriefQuote, state string, prior marketcal.Session) (*float64, time.Time, string, string) {
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
	case !q.TradeAt.IsZero():
		return new(price), q.TradeAt, "trade", ""
	}
	return new(price), q.ObservedAt, "quote", ""
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
	index.Last, index.LastAt, index.LastSource, index.LastUnavailable = marketReadQuoteLast(src.SPXQuote, state, prior)
	if index.Last == nil && len(index.Bars) > 0 {
		last := index.Bars[len(index.Bars)-1]
		index.Last, index.LastAt, index.LastSource, index.LastUnavailable = new(last.Close), last.At.Add(marketReadBar), "bar", ""
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
	if first := marketReadFirstBar(src.SPXIntraday, "SPX", session); first != nil && len(index.Bars) > 0 && index.Bars[0].At.Equal(session.Open) {
		in.SessionOpen = new(first.Open)
	}

	// VIX.
	vix := &rpc.MarketReadVIX{Line: marketReadVIXLine, Bars: []rpc.MarketReadVIXBar{}}
	vix.Last, vix.LastAt, vix.LastSource, vix.LastUnavailable = marketReadQuoteLast(src.VIXQuote, state, prior)
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
		row.Last, row.LastAt, _, _ = marketReadQuoteLast(l.Quote, state, prior)
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
		leaders.Rows = append(leaders.Rows, row)
	}
	leaders.GappedUp, leaders.HoldingGapUp = marketReadLeaderCounts(leaders.Rows)
	_, leaders.BelowPriorLow = marketReadLeaderBelowPriorLow(leaders.Rows)
	out.Leaders = leaders
	in.Leaders = leaders.Rows

	out.Tags, out.TagEvents = marketReadTags(in)
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
