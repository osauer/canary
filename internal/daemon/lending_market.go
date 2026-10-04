package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const lendingMarketCapacity = 30000
const lendingMarketPendingPerFamily = 150
const lendingMarketLifetime = 5 * time.Minute

type lendingMarketEntry struct {
	contract       rpc.ContractParams
	until, retry   time.Time
	row            rpc.LendingMarketRow
	daily          rpc.LendingMarketRow
	family         string
	quoteUntil     time.Time
	attempted      time.Time
	focusUntil     time.Time
	focusFamily    string
	focusRank      int
	pricePhaseDone bool
	brokerIdentity bool
	fullHistoryAt  time.Time
}
type lendingMarketCache struct {
	mu            sync.Mutex
	entries       map[string]lendingMarketEntry
	lastFamily    string
	foregroundRun int
}

func (s *Server) handleLendingMarket(ctx context.Context, req *rpc.Request) (*rpc.LendingMarketResult, error) {
	var p rpc.LendingMarketParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	symbols, err := rpc.NormalizeLendingRateSymbols(p.Symbols)
	if err != nil {
		return nil, err
	}
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	bulk, health, _ := s.marketEvents.loadBorrowFees(ctx)
	now := s.now().UTC()
	rows := s.lendingMarketRowsForFamily(bulk, health, symbols, now, "named")
	return &rpc.LendingMarketResult{Kind: "lending_market", Symbols: symbols, Rows: rows}, nil
}

// lendingMarketRows admits at most a small pending batch per source family.
// Every candidate still receives a row, so capacity cannot silently erase the
// wider universe. Later reads replenish admission as the joined worker advances.
func (s *Server) lendingMarketRows(bulk marketEventBorrowFeeEntry, health rpc.SourceHealth, symbols []string, now time.Time) []rpc.LendingMarketRow {
	return s.lendingMarketRowsForFamily(bulk, health, symbols, now, "lending")
}
func (s *Server) lendingMarketRowsForFamily(bulk marketEventBorrowFeeEntry, health rpc.SourceHealth, symbols []string, now time.Time, family string, displayed ...[]string) []rpc.LendingMarketRow {
	usable := borrowFeeFTPPolicyUsable(health) && !bulk.AsOf.IsZero() && !bulk.FetchedAt.IsZero() && !bulk.FetchedAt.After(now) && !bulk.AsOf.After(bulk.FetchedAt) && now.Sub(bulk.AsOf) <= 96*time.Hour
	out := make([]rpc.LendingMarketRow, 0, len(symbols))
	cache := &s.lendingMarket
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]lendingMarketEntry{}
	}
	focus := map[string]int{}
	if len(displayed) > 0 {
		for i, symbol := range displayed[0][:min(100, len(displayed[0]))] {
			focus[symbol] = i
		}
	}
	// Replace this family's focus set on each request; tab changes cannot grow
	// a permanent foreground universe. Other families retain their own bounded set.
	for key, e := range cache.entries {
		if e.focusFamily == family {
			_, stillFocused := focus[key]
			if !stillFocused && (e.contract.ConID <= 0 || e.attempted.IsZero()) {
				delete(cache.entries, key)
				continue
			}
			e.focusUntil = time.Time{}
			cache.entries[key] = e
		}
	}
	pending := 0
	for key, e := range cache.entries {
		if !now.Before(e.until) {
			delete(cache.entries, key)
			continue
		}
		if e.family == family && e.attempted.IsZero() && !now.Before(e.focusUntil) {
			pending++
		}
	}
	lastDay := lendingCompletedSession(now)
	for _, symbol := range symbols {
		rec := bulk.Symbols[symbol]
		id, _ := strconv.Atoi(rec.ConID)
		rank, focused := focus[symbol]
		c := rpc.ContractParams{Symbol: symbol, ConID: id, SecType: "STK", Exchange: "SMART", Currency: "USD"}
		exactFeed := usable && id > 0 && rec.Symbol == symbol && rec.Currency == "USD"
		e, exists := cache.entries[symbol]
		if !exactFeed {
			if exists && e.brokerIdentity {
				c = e.contract
			} else if focused {
				c.ConID = 0
			} else {
				out = append(out, rpc.LendingMarketRow{Symbol: symbol, Status: "unavailable", Detail: "Exact USD stock identity unavailable from borrowing feed."})
				continue
			}
		}
		if exists && exactFeed && e.contract.ConID == c.ConID && e.contract.Symbol == c.Symbol && e.contract.SecType == c.SecType && e.contract.Currency == c.Currency {
			c = e.contract
		}
		if exists && e.contract != c && !(e.brokerIdentity && !exactFeed) {
			delete(cache.entries, symbol)
			exists = false
		}
		if !exists {
			empty := rpc.LendingMarketRow{Symbol: symbol, Contract: c, Status: "pending", Detail: "Awaiting bounded background market coverage."}
			if c.ConID == 0 {
				empty.Detail = "Resolving exact USD stock identity."
			}
			if len(cache.entries) >= lendingMarketCapacity || (!focused && pending >= lendingMarketPendingPerFamily) {
				out = append(out, empty)
				continue
			}
			e = lendingMarketEntry{contract: c, family: family, row: empty, brokerIdentity: !exactFeed}
			if !focused {
				pending++
			}
		}
		if focused {
			e.focusFamily = family
			e.focusRank = rank
			e.focusUntil = now.Add(15 * time.Minute)
			e.quoteUntil = now.Add(15 * time.Minute)
		}
		e.until = now.Add(24 * time.Hour)
		if family == "named" {
			e.quoteUntil = now.Add(15 * time.Minute)
		}
		cache.entries[symbol] = e
		out = append(out, lendingMarketVisible(e, now, lastDay))
	}
	return out
}

// lendingDisplayedFocus keeps visible rows first and a bounded source-ranked
// discovery tail. A filter with one known match must not cancel all unresolved
// candidates before the worker can discover whether they also qualify.
func lendingDisplayedFocus(visible, source []string, limit int) []string {
	out := make([]string, 0, 100)
	for _, list := range [][]string{visible, source[:min(limit, len(source))]} {
		for _, symbol := range list {
			if len(out) < 100 && !slices.Contains(out, symbol) {
				out = append(out, symbol)
			}
		}
	}
	return out
}

func lendingCompletedSession(now time.Time) string {
	sessions, _ := tapeArchiveCalendar(now.AddDate(0, 0, -10), 12)
	last := ""
	for _, session := range sessions {
		if !session.Close.Add(15 * time.Minute).After(now) {
			last = session.Date
		}
	}
	return last
}

func lendingMarketVisible(e lendingMarketEntry, now time.Time, lastDay string) rpc.LendingMarketRow {
	if !e.row.CheckedAt.After(now) && now.Before(e.row.ValidUntil) && (e.row.Price == nil || e.row.PriceKind == "intraday" || e.row.PriceKind == "delayed" || e.daily.PriceAt.Format(time.DateOnly) == lastDay) {
		return e.row
	}
	// A fresh receipt validates retained completed-session fields, not a new
	// trade. Source dates remain unchanged; stale intraday values are discarded.
	if e.daily.Price != nil && e.daily.PriceAt.Format(time.DateOnly) == lastDay && lastDay != "" {
		r := e.daily
		r.CheckedAt = now
		r.ValidUntil = now.Add(lendingMarketLifetime)
		return r
	}
	return rpc.LendingMarketRow{Symbol: e.contract.Symbol, Contract: e.contract, Status: "pending", Detail: "Refreshing completed-session market context."}
}

func lendingMarketDue(e lendingMarketEntry, now time.Time, lastDay string) bool {
	if e.contract.ConID <= 0 && !now.Before(e.focusUntil) {
		return false
	}
	if now.Before(e.retry) {
		return false
	}
	if e.daily.Price == nil || e.daily.PriceAt.Format(time.DateOnly) != lastDay {
		return true
	}
	return now.Before(e.quoteUntil) || (e.daily.Status != "available" && now.Sub(e.attempted) >= time.Hour)
}

// selectLendingMarket advances fairly across research families and prioritizes
// never-read candidates over retries within each family.
func selectLendingMarket(cache *lendingMarketCache, now time.Time, lastDay string) (string, lendingMarketEntry) {
	families := []string{"lending", "short_interest", "named"}
	start := slices.Index(families, cache.lastFamily) + 1
	// Three displayed jobs, then one background job when both have work.
	foregroundFirst := cache.foregroundRun < 3
	for pass := range 2 {
		foreground := foregroundFirst != (pass == 1)
		for offset := range len(families) {
			family := families[(start+offset)%len(families)]
			var symbol string
			var selected lendingMarketEntry
			for key, e := range cache.entries {
				if !now.Before(e.until) {
					delete(cache.entries, key)
					continue
				}
				focused := now.Before(e.focusUntil)
				owner := e.family
				if focused {
					owner = e.focusFamily
				}
				if owner != family || focused != foreground || !lendingMarketDue(e, now, lastDay) {
					continue
				}
				better := symbol == ""
				if foreground {
					tier := lendingForegroundTier(e, lastDay)
					selectedTier := lendingForegroundTier(selected, lastDay)
					better = better || tier < selectedTier || (tier == selectedTier && (e.focusRank < selected.focusRank || e.focusRank == selected.focusRank && key < symbol))
				} else {
					better = better || e.attempted.Before(selected.attempted) || e.attempted.Equal(selected.attempted) && key < symbol
				}
				if better {
					symbol, selected = key, e
				}
			}
			if symbol != "" {
				cache.lastFamily = family
				if foreground {
					cache.foregroundRun++
				} else {
					cache.foregroundRun = 0
				}
				return symbol, selected
			}
		}
	}
	return "", lendingMarketEntry{}
}

func lendingForegroundTier(e lendingMarketEntry, lastDay string) int {
	if e.attempted.IsZero() {
		return 0
	}
	if e.daily.Price == nil || e.daily.PriceAt.Format(time.DateOnly) != lastDay {
		return 1
	}
	return 2
}

// One joined, background-priority worker; no fan-out from a screen read.
func (s *Server) startLendingMarketRefresh(ctx context.Context) {
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			now := s.now().UTC()
			cache := &s.lendingMarket
			cache.mu.Lock()
			symbol, selected := selectLendingMarket(cache, now, lendingCompletedSession(now))
			cache.mu.Unlock()
			if symbol == "" {
				continue
			}
			readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), 35*time.Second)
			row, daily := s.readLendingMarketContext(readCtx, selected)
			cancel()
			cache.mu.Lock()
			if current, ok := cache.entries[symbol]; ok && current.contract == selected.contract {
				current.row = row
				current.daily = daily
				if row.Contract.ConID > 0 {
					current.contract = row.Contract
				}
				phaseOne := now.Before(selected.focusUntil) && !selected.pricePhaseDone
				current.pricePhaseDone = current.pricePhaseDone || phaseOne
				if !phaseOne && lendingNeedsHistory(selected, now, lendingCompletedSession(now)) {
					current.fullHistoryAt = s.now().UTC()
				}
				current.attempted = s.now().UTC()
				current.retry = s.now().Add(lendingMarketLifetime)
				if phaseOne && daily.Price != nil {
					current.retry = s.now()
				}
				cache.entries[symbol] = current
			}
			cache.mu.Unlock()
		}
	})
}

type lendingIdentityResolver interface {
	CaptureSession() (ibkrlib.ConnectorSessionBinding, bool)
	SessionCurrent(ibkrlib.ConnectorSessionBinding) bool
	ResolveOrderContractForSession(context.Context, ibkrlib.ConnectorSessionBinding, ibkrlib.Contract, time.Duration) (ibkrlib.ResolvedOrderContract, error)
}

func resolveLendingMarketIdentity(ctx context.Context, source rpc.ContractParams, c lendingIdentityResolver) (rpc.ContractParams, error) {
	if c == nil {
		return source, errors.New("gateway unavailable")
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return source, errors.New("gateway unavailable")
	}
	resolved, err := c.ResolveOrderContractForSession(ctx, binding, ibkrlib.Contract{Symbol: source.Symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"}, 4*time.Second)
	if err != nil {
		return source, err
	}
	r := resolved.Contract
	if !c.SessionCurrent(binding) || r.ConID <= 0 || r.Symbol != source.Symbol || r.SecType != "STK" || r.Currency != "USD" {
		return source, errors.New("exact stock identity unavailable")
	}
	out := rpc.ContractParams{Symbol: r.Symbol, ConID: r.ConID, SecType: r.SecType, Currency: r.Currency, Exchange: r.Exchange, PrimaryExch: r.PrimaryExch, LocalSymbol: r.LocalSymbol, TradingClass: r.TradingClass}
	if !usChartCalendar(out) {
		return source, errors.New("unsupported stock calendar")
	}
	return out, nil
}

func (s *Server) readLendingMarketContext(ctx context.Context, e lendingMarketEntry) (rpc.LendingMarketRow, rpc.LendingMarketRow) {
	c := e.contract
	now := s.now().UTC()
	if c.ConID <= 0 {
		connector := s.gatewayConnector()
		if connector == nil {
			return lendingIdentityUnavailable(c, now), rpc.LendingMarketRow{}
		}
		resolved, err := resolveLendingMarketIdentity(ctx, c, connector)
		if err != nil {
			return lendingIdentityUnavailable(c, s.now().UTC()), rpc.LendingMarketRow{}
		}
		c = resolved
	}
	phaseOne := now.Before(e.focusUntil) && !e.pricePhaseDone
	daily := e.daily
	if phaseOne || lendingNeedsHistory(e, now, lendingCompletedSession(now)) {
		rangeName := "1Y"
		readCtx := ctx
		cancel := func() {}
		if phaseOne {
			rangeName = "1M"
			readCtx, cancel = context.WithTimeout(ctx, 8*time.Second)
		}
		h, _ := s.marketHistoryRequest(readCtx, rpc.MarketHistoryParams{Contract: c, Range: rangeName})
		cancel()
		fresh := projectLendingMarket(c, h, nil, s.now().UTC())
		daily = retainLendingCompletedPrice(daily, fresh, lendingCompletedSession(s.now().UTC()))
	}
	daily.CheckedAt = s.now().UTC()
	daily.ValidUntil = daily.CheckedAt.Add(lendingMarketLifetime)
	row := daily
	if ctx.Err() == nil && now.Before(e.quoteUntil) {
		raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: c, TimeoutMs: 1500})
		q, _ := s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
		row = overlayLendingQuote(daily, c, q, s.now().UTC())
		if daily.Price == nil && row.PriceKind == "close" {
			daily = row
		}
	}
	return row, daily
}

func lendingIdentityUnavailable(c rpc.ContractParams, now time.Time) rpc.LendingMarketRow {
	return rpc.LendingMarketRow{Symbol: c.Symbol, Contract: c, Status: "unavailable", CheckedAt: now, ValidUntil: now.Add(lendingMarketLifetime), Detail: "Exact USD stock identity could not be resolved."}
}

func lendingNeedsHistory(e lendingMarketEntry, now time.Time, lastDay string) bool {
	return e.daily.Price == nil || e.daily.PriceAt.Format(time.DateOnly) != lastDay || (e.daily.Status != "available" && (e.fullHistoryAt.IsZero() || now.Sub(e.fullHistoryAt) >= time.Hour))
}

func retainLendingCompletedPrice(old, fresh rpc.LendingMarketRow, lastDay string) rpc.LendingMarketRow {
	if old.Price == nil || old.PriceKind != "close" || old.PriceAt.Format(time.DateOnly) != lastDay || old.Contract != fresh.Contract {
		return fresh
	}
	if fresh.Price == nil {
		return old
	}
	if fresh.PriceKind != "close" || fresh.PriceAt.Format(time.DateOnly) != lastDay {
		return fresh
	}
	// Keep independently dated fields from the proven price phase when a thin
	// yearly response lacks them. A changed close can invalidate relative metrics.
	if fresh.Volume == nil && old.VolumeDate == lastDay {
		fresh.Volume = old.Volume
		fresh.VolumeDate = old.VolumeDate
	}
	if *old.Price == *fresh.Price {
		if fresh.DayChangePct == nil {
			fresh.DayChangePct = old.DayChangePct
		}
		if fresh.AvgDollarVolume20D == nil && old.LiquidityAsOf == lastDay {
			fresh.AvgDollarVolume20D = old.AvgDollarVolume20D
			fresh.LiquidityAsOf = old.LiquidityAsOf
		}
		if fresh.YTDChangePct == nil && old.YTDAsOf == lastDay {
			fresh.YTDChangePct = old.YTDChangePct
			fresh.YTDAsOf = old.YTDAsOf
			fresh.YTDBaseDate = old.YTDBaseDate
		}
	}
	if fresh.Price != nil && fresh.DayChangePct != nil && fresh.Volume != nil && fresh.AvgDollarVolume20D != nil && fresh.YTDChangePct != nil {
		fresh.Status = "available"
		fresh.Detail = "Completed-session price and market context available; YTD excludes dividends."
	}
	return fresh
}

func projectLendingMarket(c rpc.ContractParams, h *rpc.MarketHistoryResult, q *rpc.Quote, now time.Time) rpc.LendingMarketRow {
	out := rpc.LendingMarketRow{Symbol: c.Symbol, Contract: c, Status: "unavailable", CheckedAt: now, ValidUntil: now.Add(lendingMarketLifetime), Detail: "Price or daily history unavailable."}
	positive := func(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
	sessions, _ := tapeArchiveCalendar(now.AddDate(0, 0, -370), 372)
	dates := []string{}
	for _, s := range sessions {
		if !s.Close.Add(15 * time.Minute).After(now) {
			dates = append(dates, s.Date)
		}
	}
	points := map[string]rpc.MarketHistoryPoint{}
	if h != nil && h.Contract.SecType == "STK" && h.Contract.ConID == c.ConID && h.Contract.Symbol == c.Symbol && h.Contract.Currency == c.Currency && h.Interval == "1 day" && h.TimestampKind == "session_date" && h.PriceBasis == "TRADES" && !h.AsOf.IsZero() && !h.AsOf.After(now) {
		for _, p := range h.Points {
			if positive(p.Value) {
				points[p.At.Format(time.DateOnly)] = p
			}
		}
	}
	if len(dates) > 1 {
		lastDay, priorDay := dates[len(dates)-1], dates[len(dates)-2]
		if last, ok := points[lastDay]; ok {
			out.Price = new(last.Value)
			out.PriceAt = last.At
			out.PriceKind = "close"
			if prior, ok := points[priorDay]; ok {
				out.DayChangePct = new((last.Value/prior.Value - 1) * 100)
			}
			if last.Volume != nil && *last.Volume >= 0 {
				out.Volume = last.Volume
				out.VolumeDate = lastDay
			}
			if len(dates) >= 20 {
				sum := 0.0
				complete := true
				for _, date := range dates[len(dates)-20:] {
					p, ok := points[date]
					if !ok || p.Volume == nil || *p.Volume < 0 {
						complete = false
						break
					}
					sum += p.Value * float64(*p.Volume)
				}
				if complete && !math.IsInf(sum, 0) {
					out.AvgDollarVolume20D = new(sum / 20)
					out.LiquidityAsOf = lastDay
				}
			}
			yearStart := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			// At the calendar's coverage boundary only an actual 31 December
			// bar can prove year-end without guessing an earlier holiday session.
			baseDay := time.Date(now.Year()-1, 12, 31, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
			for _, date := range slices.Backward(dates) {
				if date < yearStart {
					baseDay = date
					break
				}
			}
			if base, ok := points[baseDay]; ok && lastDay >= yearStart {
				out.YTDChangePct = new((last.Value/base.Value - 1) * 100)
				out.YTDBaseDate = baseDay
				out.YTDAsOf = lastDay
			}
		}
	}
	return overlayLendingQuote(out, c, q, now)
}

func overlayLendingQuote(out rpc.LendingMarketRow, c rpc.ContractParams, q *rpc.Quote, now time.Time) rpc.LendingMarketRow {
	positive := func(v float64) bool { return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0) }
	// A regular-close snapshot carries its own official session date. It may
	// supply the price while yearly history is unavailable; an undated prev_close
	// or old last trade never receives today's date.
	if q != nil && q.Contract.Symbol == c.Symbol && q.Contract.ConID == c.ConID && q.Contract.SecType == "STK" && q.Contract.Currency == "USD" && !q.Stale && q.RegularClose != nil && positive(*q.RegularClose) && !q.RegularCloseAt.IsZero() && !q.RegularCloseAt.After(now) && q.RegularCloseAt.UTC().Format(time.DateOnly) == lendingCompletedSession(now) && out.Price == nil {
		out.Price = q.RegularClose
		out.PriceKind = "close"
		out.PriceAt = q.RegularCloseAt
		if q.RegularChangePct != nil && !math.IsNaN(*q.RegularChangePct) && !math.IsInf(*q.RegularChangePct, 0) {
			out.DayChangePct = q.RegularChangePct
		}
	}
	// Only an actual recent trade replaces the last completed close. Frozen or
	// bid/ask indications retain the daily fallback and its matching daily volume.
	if q != nil && q.Contract.SecType == "STK" && q.Contract.ConID == c.ConID && q.Contract.Symbol == c.Symbol && q.Contract.Currency == c.Currency && q.Last != nil && positive(*q.Last) && !q.Stale && !q.TradeAt.IsZero() && !q.TradeAt.After(now) && now.Sub(q.TradeAt) <= 20*time.Minute {
		out.Price = q.Last
		out.PriceAt = q.TradeAt
		out.PriceKind = "intraday"
		if q.DataType == rpc.MarketDataDelayed || q.DataType == rpc.MarketDataDelayedFrozen {
			out.PriceKind = "delayed"
		}
		out.DayChangePct = nil
		if q.PrevClose != nil && positive(*q.PrevClose) {
			out.DayChangePct = new((*q.Last / *q.PrevClose - 1) * 100)
		}
		if q.Volume != nil && *q.Volume >= 0 {
			out.Volume = q.Volume
			loc, _ := time.LoadLocation("America/New_York")
			out.VolumeDate = q.TradeAt.In(loc).Format(time.DateOnly)
		}
	}
	if out.Price != nil {
		out.Status = "partial"
		out.Detail = "YTD is a completed-close price return, excluding dividends; volume is session shares. Liquidity needs all 20 completed sessions."
		if out.DayChangePct != nil && out.Volume != nil && out.YTDChangePct != nil && out.AvgDollarVolume20D != nil {
			out.Status = "available"
		}
	}
	return out
}
