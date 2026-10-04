package daemon

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const lendingMarketCapacity = 150
const lendingMarketLifetime = 5 * time.Minute

type lendingMarketEntry struct {
	contract     rpc.ContractParams
	until, retry time.Time
	row          rpc.LendingMarketRow
}
type lendingMarketCache struct {
	mu      sync.Mutex
	entries map[string]lendingMarketEntry
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
	usable := borrowFeeFTPPolicyUsable(health) && !bulk.AsOf.IsZero() && !bulk.FetchedAt.After(now) && !bulk.AsOf.After(bulk.FetchedAt) && now.Sub(bulk.AsOf) <= 96*time.Hour
	out := &rpc.LendingMarketResult{Kind: "lending_market", Symbols: symbols, Rows: []rpc.LendingMarketRow{}}
	cache := &s.lendingMarket
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = map[string]lendingMarketEntry{}
	}
	for symbol, e := range cache.entries {
		if !now.Before(e.until) {
			delete(cache.entries, symbol)
		}
	}
	for _, symbol := range symbols {
		rec := bulk.Symbols[symbol]
		id, _ := strconv.Atoi(rec.ConID)
		empty := rpc.LendingMarketRow{Symbol: symbol, Status: "unavailable", Detail: "Exact USD stock identity unavailable from borrowing feed."}
		if !usable || id <= 0 || rec.Symbol != symbol || rec.Currency != "USD" {
			out.Rows = append(out.Rows, empty)
			continue
		}
		c := rpc.ContractParams{Symbol: symbol, ConID: id, SecType: "STK", Exchange: "SMART", Currency: "USD"}
		e, exists := cache.entries[symbol]
		if exists && e.contract != c {
			delete(cache.entries, symbol)
			exists = false
		}
		if !exists {
			if len(cache.entries) >= lendingMarketCapacity {
				empty.Detail = "Market context capacity reached; retry later."
				out.Rows = append(out.Rows, empty)
				continue
			}
			e = lendingMarketEntry{contract: c, row: rpc.LendingMarketRow{Symbol: symbol, Contract: c, Status: "pending", Detail: "Loading price and daily history."}}
		}
		e.until = now.Add(15 * time.Minute)
		cache.entries[symbol] = e
		row := e.row
		if !row.ValidUntil.IsZero() && !now.Before(row.ValidUntil) {
			row = rpc.LendingMarketRow{Symbol: symbol, Contract: c, Status: "pending", Detail: "Refreshing market context."}
		}
		out.Rows = append(out.Rows, row)
	}
	return out, nil
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
			var symbol string
			var selected lendingMarketEntry
			for key, e := range cache.entries {
				if !now.Before(e.until) {
					delete(cache.entries, key)
					continue
				}
				if now.Before(e.retry) {
					continue
				}
				if symbol == "" || e.retry.Before(selected.retry) || (e.retry.Equal(selected.retry) && key < symbol) {
					symbol, selected = key, e
				}
			}
			cache.mu.Unlock()
			if symbol == "" {
				continue
			}
			readCtx, cancel := context.WithTimeout(ibkrlib.WithRequestPriority(ctx, ibkrlib.PriorityBackground), 35*time.Second)
			row := s.readLendingMarket(readCtx, selected.contract)
			cancel()
			cache.mu.Lock()
			if current, ok := cache.entries[symbol]; ok && current.contract == selected.contract {
				current.row = row
				current.retry = s.now().Add(lendingMarketLifetime)
				cache.entries[symbol] = current
			}
			cache.mu.Unlock()
		}
	})
}

func (s *Server) readLendingMarket(ctx context.Context, c rpc.ContractParams) rpc.LendingMarketRow {
	// Direct history cache path avoids registering 50 new chart refresh interests.
	h, _ := s.marketHistoryRequest(ctx, rpc.MarketHistoryParams{Contract: c, Range: "1Y"})
	var q *rpc.Quote
	if ctx.Err() == nil {
		raw, _ := json.Marshal(rpc.QuoteSnapshotParams{Contract: c, TimeoutMs: 1500})
		q, _ = s.handleQuoteSnapshot(ctx, &rpc.Request{Params: raw})
	}
	return projectLendingMarket(c, h, q, s.now().UTC())
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
