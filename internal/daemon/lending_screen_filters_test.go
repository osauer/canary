package daemon

import (
	"fmt"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestLendingScreenFiltersAcrossWholeCandidateSetBeforeLimit(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	bulk := marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{}}
	for i := range 80 {
		symbol := fmt.Sprintf("X%03d", i)
		bulk.Symbols[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, ConID: fmt.Sprint(91000 + i), Currency: "USD", FeeRate: float64(150 - i)}
	}
	health := rpc.SourceHealth{Status: rpc.SourceStatusOK, RefreshState: rpc.SourceRefreshNotDue}
	p := rpc.LendingScreenParams{MinRate: 50, Limit: 1, MinPrice: 5, MinAvgDollarVolume20D: 10000000, SortBy: "avg_dollar_volume_20d", SortDir: "desc"}
	out := lendingScreenCandidates(bulk, health, p, now)
	for i := range out.Rows {
		r := &out.Rows[i]
		turn := 100.0
		if r.Symbol == "X075" {
			turn = 20000000
		}
		r.Market = &rpc.LendingMarketRow{Symbol: r.Symbol, Contract: rpc.ContractParams{Symbol: r.Symbol, ConID: 91001, SecType: "STK", Currency: "USD"}, Status: "partial", CheckedAt: now, ValidUntil: now.Add(5 * time.Minute), Price: new(10.0), PriceKind: "close", PriceAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), AvgDollarVolume20D: &turn, LiquidityAsOf: "2026-10-02"}
	}
	finishLendingScreen(&out, p)
	if out.Matching != 1 || out.Rows[0].Symbol != "X075" || !out.Coverage.Complete || out.Coverage.Covered != 80 {
		t.Fatalf("late liquid candidate lost: %+v", out)
	}
	if err := rpc.ValidateLendingScreenResult(out, p); err != nil {
		t.Fatal(err)
	}
	// Incomplete coverage must never masquerade as an exhaustive zero match.
	out = lendingScreenCandidates(bulk, health, p, now)
	finishLendingScreen(&out, p)
	if out.Matching != 0 || out.Coverage.Complete || out.Coverage.Pending != 80 {
		t.Fatalf("unknown became no opportunities: %+v", out)
	}
}

func TestLendingMarketAdmissionProgressAndFairness(t *testing.T) {
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	s := &Server{}
	bulk := marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{}}
	symbols := []string{}
	for i := range 400 {
		symbol := fmt.Sprintf("X%03d", i)
		symbols = append(symbols, symbol)
		bulk.Symbols[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, ConID: fmt.Sprint(91000 + i), Currency: "USD"}
	}
	health := rpc.SourceHealth{Status: rpc.SourceStatusOK, RefreshState: rpc.SourceRefreshNotDue}
	first := s.lendingMarketRows(bulk, health, symbols, now)
	if len(first) != 400 || len(s.lendingMarket.entries) != 150 || first[399].Status != "pending" {
		t.Fatal("unbounded admission or lost candidate")
	}
	for key, e := range s.lendingMarket.entries {
		e.attempted = now
		e.retry = now.Add(5 * time.Minute)
		s.lendingMarket.entries[key] = e
	}
	s.lendingMarketRows(bulk, health, symbols, now)
	if len(s.lendingMarket.entries) != 300 {
		t.Fatal("completed initial batch locked out later candidates")
	}
	// A separate family gets its turn even with a lending backlog.
	e := s.lendingMarket.entries["X000"]
	e.family = "short_interest"
	e.retry = time.Time{}
	s.lendingMarket.entries["X000"] = e
	s.lendingMarket.lastFamily = "lending"
	symbol, _ := selectLendingMarket(&s.lendingMarket, now, "2026-10-02")
	if symbol != "X000" {
		t.Fatal("short interest starved")
	}
	symbol, selected := selectLendingMarket(&s.lendingMarket, now, "2026-10-02")
	if symbol != "X150" || !selected.attempted.IsZero() {
		t.Fatal("new lending candidates starved behind retries")
	}
}

func TestLendingMarketCompletedCacheAndIntradayExpiry(t *testing.T) {
	c, h, now := lendingMarketFixture(t)
	daily := projectLendingMarket(c, h, nil, now)
	intraday := daily
	intraday.Price = new(22.0)
	intraday.PriceKind = "intraday"
	intraday.PriceAt = now
	intraday.ValidUntil = now.Add(time.Minute)
	e := lendingMarketEntry{contract: c, row: intraday, daily: daily}
	got := lendingMarketVisible(e, now.Add(2*time.Minute), "2026-10-02")
	if got.PriceKind != "close" || *got.Price != 20 || got.LiquidityAsOf != "2026-10-02" || !got.CheckedAt.Equal(now.Add(2*time.Minute)) {
		t.Fatal("stale trade renewed or completed source dates changed")
	}
	got = lendingMarketVisible(e, now.Add(2*time.Minute), "2026-10-05")
	if got.Price != nil || got.Status != "pending" {
		t.Fatal("previous completed session relabelled current")
	}
	if lendingMarketDue(e, now, "2026-10-02") {
		t.Fatal("current completed history reacquired without quote interest")
	}
	if !lendingMarketDue(e, now, "2026-10-05") {
		t.Fatal("new completed session not refreshed")
	}
}
