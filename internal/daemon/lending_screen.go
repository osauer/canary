package daemon

import (
	"context"
	"math"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleLendingScreen(ctx context.Context, req *rpc.Request) (*rpc.LendingScreenResult, error) {
	var p rpc.LendingScreenParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeLendingScreenParams(p)
	if err != nil {
		return nil, err
	}
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	// Reuse the source refresh gate, session calendar, durable cache and backoff.
	// No per-symbol broker requests or historical FEE_RATE fallback are made.
	bulk, health, _ := s.marketEvents.loadBorrowFees(ctx)
	now := s.now().UTC()
	result := lendingScreenCandidates(bulk, health, p, now)
	if result.Status == "observed" {
		symbols := make([]string, 0, len(result.Rows))
		// Fee-ranked order controls only bounded admission, never the result scope.
		slices.SortFunc(result.Rows, func(a, b rpc.LendingScreenRow) int {
			return rpc.CompareLendingScreenRows(a, b, rpc.LendingScreenParams{})
		})
		for _, r := range result.Rows {
			symbols = append(symbols, r.Symbol)
		}
		market := s.lendingMarketRows(bulk, health, symbols, now)
		s.marketEvents.mu.Lock()
		dates := mergeLendingBorrowDates(s.marketEvents.borrowingDates, bulk)
		s.marketEvents.mu.Unlock()
		for i := range result.Rows {
			result.Rows[i].Market = &market[i]
			result.Rows[i].History = lendingFeeHistory(dates, result.Rows[i].Symbol, bulk.Symbols[result.Rows[i].Symbol].ConID, now)
			result.Rows[i].HighDates = lendingHighDates(dates, result.Rows[i].Symbol, bulk.Symbols[result.Rows[i].Symbol].ConID, p.MinRate, now)
		}
		finishLendingScreen(&result, p)
	}
	return &result, nil
}

func projectLendingScreen(bulk marketEventBorrowFeeEntry, health rpc.SourceHealth, p rpc.LendingScreenParams, now time.Time) rpc.LendingScreenResult {
	out := lendingScreenCandidates(bulk, health, p, now)
	if out.Status == "observed" {
		finishLendingScreen(&out, p)
	}
	return out
}

func lendingScreenCandidates(bulk marketEventBorrowFeeEntry, health rpc.SourceHealth, p rpc.LendingScreenParams, now time.Time) rpc.LendingScreenResult {
	out := rpc.LendingScreenResult{Kind: "lending_screen", Universe: "us_short_stock", Status: "unavailable", AsOf: bulk.AsOf, ObservedAt: bulk.FetchedAt, Params: p, SkippedRows: bulk.SkippedRows, Rows: []rpc.LendingScreenRow{}, SourceHealth: health}
	for _, rec := range bulk.Symbols {
		if rec.Currency == "USD" {
			out.Total++
		}
	}
	if !borrowFeeFTPPolicyUsable(health) || bulk.AsOf.IsZero() || bulk.FetchedAt.IsZero() || bulk.AsOf.After(bulk.FetchedAt) || bulk.FetchedAt.After(now) || now.Sub(bulk.AsOf) > 96*time.Hour || now.Sub(bulk.FetchedAt) > 96*time.Hour {
		return out
	}
	out.Status = "observed"
	for symbol, rec := range bulk.Symbols {
		valid, err := rpc.NormalizeLendingRateSymbols([]string{symbol})
		if err != nil || valid[0] != symbol || rec.Symbol != symbol || rec.Currency != "USD" || rec.FeeRateUnpublished || math.IsNaN(rec.FeeRate) || math.IsInf(rec.FeeRate, 0) || rec.FeeRate < 0 {
			continue
		}
		out.Usable++
		if rec.FeeRate < p.MinRate || slices.Contains(p.Exclude, symbol) {
			continue
		}
		coverage, _ := bulkBorrowFeeCoverage([]string{symbol}, bulk, health)
		out.Rows = append(out.Rows, rpc.LendingScreenRow{MarketEventBorrowFeeCoverage: coverage[0], Name: rec.Name, Currency: rec.Currency})
	}
	return out
}

func finishLendingScreen(out *rpc.LendingScreenResult, p rpc.LendingScreenParams) {
	out.Coverage = rpc.LendingScreenCoverage{Candidates: len(out.Rows)}
	for _, r := range out.Rows {
		if rpc.LendingScreenMarketCovered(r.Market, p) {
			out.Coverage.Covered++
		} else if r.Market == nil || r.Market.Status == "pending" {
			out.Coverage.Pending++
		} else {
			out.Coverage.Unavailable++
		}
	}
	out.Coverage.Complete = out.Coverage.Covered == out.Coverage.Candidates
	out.Rows = slices.DeleteFunc(out.Rows, func(r rpc.LendingScreenRow) bool {
		return r.HighDates < p.MinHighDates || !rpc.LendingScreenMatchesMarket(r.Market, p)
	})
	slices.SortFunc(out.Rows, func(a, b rpc.LendingScreenRow) int { return rpc.CompareLendingScreenRows(a, b, p) })
	out.Matching = len(out.Rows)
	out.Truncated = out.Matching > p.Limit
	if out.Truncated {
		out.Rows = out.Rows[:p.Limit]
	}
}
