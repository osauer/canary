package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type shortInterestCache struct {
	mu           sync.Mutex
	publication  shortInterestPublication
	until, retry time.Time
	failed       bool
}

func (s *Server) shortInterestPath() string {
	if s.socketPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(s.socketPath), "short-interest.json")
}

// One joined demand-driven public-source loop. RPC reads never fetch HTTP.
func (s *Server) startShortInterestRefresh(ctx context.Context) {
	if path := s.shortInterestPath(); path != "" && shortInterestRegularFile(path) {
		if file, err := os.Open(path); err == nil {
			var p shortInterestPublication
			err = json.NewDecoder(io.LimitReader(file, 32<<20)).Decode(&p)
			file.Close()
			now := s.now().UTC()
			if err == nil && validShortInterestPublication(p, now) {
				s.shortInterest.mu.Lock()
				s.shortInterest.publication = p
				s.shortInterest.retry = p.FetchedAt.Add(6 * time.Hour)
				s.shortInterest.mu.Unlock()
			}
		}
	}
	s.marketData.loopWG.Go(func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			now := s.now().UTC()
			c := &s.shortInterest
			c.mu.Lock()
			due := now.Before(c.until) && !now.Before(c.retry)
			c.mu.Unlock()
			if !due {
				continue
			}
			readCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			p, err := fetchShortInterest(readCtx, now)
			cancel()
			c.mu.Lock()
			if err == nil && c.publication.SettlementDate > p.SettlementDate {
				err = errors.New("FINRA publication moved backwards")
			}
			c.failed = err != nil
			c.retry = now.Add(6 * time.Hour)
			if err == nil {
				c.publication = p
			} else {
				c.retry = now.Add(30 * time.Minute)
			}
			c.mu.Unlock()
			if err == nil && s.shortInterestPath() != "" {
				path := s.shortInterestPath()
				if body, e := json.Marshal(p); e == nil {
					_ = writePrivateStateAtomic(path, body)
				}
			}
		}
	})
}

func validShortInterestPublication(p shortInterestPublication, now time.Time) bool {
	if p.FetchedAt.IsZero() || p.FetchedAt.After(now) || len(p.Rows) == 0 || len(p.Rows) > shortInterestMaxRows || p.Skipped < 0 {
		return false
	}
	url, date, e := shortInterestLatestURL([]byte(p.URL), now)
	if e != nil || url != p.URL || date != p.SettlementDate {
		return false
	}
	day, e := time.Parse(time.DateOnly, date)
	if e != nil || day.After(p.FetchedAt) {
		return false
	}
	seen := map[string]bool{}
	for _, row := range p.Rows {
		params, _ := rpc.NormalizeShortInterestScreenParams(rpc.ShortInterestScreenParams{Limit: 1})
		r := rpc.ShortInterestScreenResult{Kind: "short_interest_screen", Status: "stale", Source: "FINRA equity short interest", SourceURL: p.URL, Params: params, SettlementDate: date, FetchedAt: p.FetchedAt, CheckedAt: now, Total: 1, Matching: 1, Rows: []rpc.ShortInterestRow{row}, Coverage: rpc.ShortInterestCoverage{Complete: true}}
		if seen[row.Symbol] || rpc.ValidateShortInterestScreenResult(r, params) != nil || row.MarketContext != nil || row.FeeRate != nil {
			return false
		}
		seen[row.Symbol] = true
	}
	return true
}

func (s *Server) handleShortInterestScreen(ctx context.Context, req *rpc.Request) (*rpc.ShortInterestScreenResult, error) {
	var p rpc.ShortInterestScreenParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeShortInterestScreenParams(p)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	c := &s.shortInterest
	c.mu.Lock()
	c.until = now.Add(15 * time.Minute)
	pub, failed := c.publication, c.failed
	c.mu.Unlock()
	out := &rpc.ShortInterestScreenResult{Kind: "short_interest_screen", Status: "pending", Source: "FINRA equity short interest", SourceURL: shortInterestIndexURL, Params: p, Rows: []rpc.ShortInterestRow{}, Coverage: rpc.ShortInterestCoverage{Complete: true}, Detail: "Loading the latest FINRA publication."}
	if len(pub.Rows) == 0 {
		if failed {
			out.Status = "unavailable"
			out.Detail = "FINRA publication unavailable; the daemon will retry."
		}
		return out, nil
	}
	out.Status = "available"
	out.SourceURL = pub.URL
	out.SettlementDate = pub.SettlementDate
	out.FetchedAt = pub.FetchedAt
	out.CheckedAt = now
	out.Total = len(pub.Rows)
	out.SkippedRows = pub.Skipped
	settlement, _ := time.Parse(time.DateOnly, pub.SettlementDate)
	if failed || pub.FetchedAt.After(now) || now.Sub(pub.FetchedAt) > 48*time.Hour || now.Sub(settlement) > 35*24*time.Hour {
		out.Status = "stale"
	}
	out.Detail = "FINRA twice-monthly US equities, including funds and OTC. Settlement positions, not daily short-sale volume. Float data unavailable. Days to cover is floored at 1 by FINRA; average shares covers the reporting cycle. Market filters and rankings use only covered identities; coverage may be incomplete."
	if failed {
		out.Detail += " Latest source refresh failed; retained publication shown."
	}
	candidates := []rpc.ShortInterestRow{}
	for _, row := range pub.Rows {
		if slices.Contains(p.Exclude, row.Symbol) || (p.ListedOnly && row.Market == "OTC") || row.AverageDailyVolume < p.MinAverageVolume || (p.MinDaysToCover > 0 && (row.DaysToCover == nil || *row.DaysToCover < p.MinDaysToCover)) {
			continue
		}
		candidates = append(candidates, row)
	}
	// Prioritize the source ranking before shared bounded enrichment admission.
	rpc.SortShortInterestRows(candidates, p)
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	bulk, health, _ := s.marketEvents.loadBorrowFees(ctx)
	symbols := make([]string, len(candidates))
	for i, row := range candidates {
		symbols[i] = row.Symbol
	}
	contextRows := s.lendingMarketRowsForFamily(bulk, health, symbols, now, "short_interest", symbols[:min(p.Limit, len(symbols))])
	out.Coverage = rpc.ShortInterestCoverage{Candidates: len(candidates)}
	for i, row := range candidates {
		if i < len(contextRows) {
			m := contextRows[i]
			row.MarketContext = &m
			if m.Status == "pending" {
				out.Coverage.Pending++
			} else if rpc.LendingScreenMarketCovered(&m, rpc.LendingScreenParams{SortBy: p.SortBy, MinPrice: p.MinPrice, MinAvgDollarVolume20D: p.MinAvgDollarVolume20D}) {
				out.Coverage.Covered++
			} else {
				out.Coverage.Unavailable++
			}
		} else {
			out.Coverage.Unavailable++
		}
		// Keep borrowing fees on their independent producer clock.
		rec, hasFee := bulk.Symbols[row.Symbol]
		if hasFee && borrowFeeFTPPolicyUsable(health) && !bulk.AsOf.IsZero() && !bulk.AsOf.After(bulk.FetchedAt) && !bulk.FetchedAt.After(now) && now.Sub(bulk.AsOf) <= 96*time.Hour && rec.Symbol == row.Symbol && rec.Currency == "USD" && !rec.FeeRateUnpublished && rec.FeeRate >= 0 && !math.IsNaN(rec.FeeRate) && !math.IsInf(rec.FeeRate, 0) {
			row.FeeRate = new(rec.FeeRate)
			row.FeeAsOf = bulk.AsOf
		}
		if p.MinPrice > 0 && (row.MarketContext == nil || row.MarketContext.Price == nil || *row.MarketContext.Price < p.MinPrice) {
			continue
		}
		if p.MinAvgDollarVolume20D > 0 && (row.MarketContext == nil || row.MarketContext.AvgDollarVolume20D == nil || *row.MarketContext.AvgDollarVolume20D < p.MinAvgDollarVolume20D) {
			continue
		}
		out.Rows = append(out.Rows, row)
	}
	out.Coverage.Complete = out.Coverage.Candidates == out.Coverage.Covered
	rpc.SortShortInterestRows(out.Rows, p)
	out.Matching = len(out.Rows)
	out.Truncated = out.Matching > p.Limit
	if out.Truncated {
		out.Rows = out.Rows[:p.Limit]
	}
	// Keep the actual returned rows responsive after market sorting/filtering;
	// the earlier source prefix provides discovery progress when none are priced.
	if len(out.Rows) > 0 {
		displayed := make([]string, len(out.Rows))
		for i, row := range out.Rows {
			displayed[i] = row.Symbol
		}
		focus := lendingDisplayedFocus(displayed, symbols, p.Limit)
		s.lendingMarketRowsForFamily(bulk, health, focus, now, "short_interest", focus)
	}
	return out, nil
}

func shortInterestRegularFile(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() <= 32<<20
}
