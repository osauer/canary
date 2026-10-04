package rpc

import (
	"math"
	"testing"
	"time"
)

func TestShortInterestScopeAndOrdering(t *testing.T) {
	p, _ := NormalizeShortInterestScreenParams(ShortInterestScreenParams{SortBy: "days_to_cover"})
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	r := ShortInterestScreenResult{Kind: "short_interest_screen", Status: "available", Source: "FINRA equity short interest", SourceURL: "https://cdn.finra.org/equity/otcmarket/biweekly/shrt20260915.csv", SettlementDate: "2026-09-15", FetchedAt: now, CheckedAt: now, Params: p, Total: 3, Matching: 3, Coverage: ShortInterestCoverage{Candidates: 3, Unavailable: 3}, Rows: []ShortInterestRow{
		{Symbol: "AAA", ShortInterestShares: 100, AverageDailyVolume: 10, DaysToCover: new(10.0), SettlementDate: "2026-09-15"},
		{Symbol: "BBB", ShortInterestShares: 100, AverageDailyVolume: 10, DaysToCover: new(10.0), SettlementDate: "2026-09-15"},
		{Symbol: "CCC", ShortInterestShares: 100, SettlementDate: "2026-09-15"},
	}}
	if err := ValidateShortInterestScreenResult(r, p); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.Rows = append([]ShortInterestRow(nil), r.Rows...)
	bad.Rows[0], bad.Rows[2] = bad.Rows[2], bad.Rows[0]
	if ValidateShortInterestScreenResult(bad, p) == nil {
		t.Fatal("missing evidence sorted first accepted")
	}
	bad = r
	bad.Rows = r.Rows[:2]
	if ValidateShortInterestScreenResult(bad, p) == nil {
		t.Fatal("silent truncation accepted")
	}
	bad = r
	bad.SourceURL = "https://example.test/short.csv"
	if ValidateShortInterestScreenResult(bad, p) == nil {
		t.Fatal("foreign source accepted")
	}
	bad = r
	bad.Params.SortBy = "symbol"
	if ValidateShortInterestScreenResult(bad, p) == nil {
		t.Fatal("scope mismatch accepted")
	}
	for _, direction := range []string{"asc", "desc"} {
		p.SortDir = direction
		SortShortInterestRows(r.Rows, p)
		if r.Rows[2].Symbol != "CCC" {
			t.Fatal("missing must stay last")
		}
	}
}

func TestShortInterestParams(t *testing.T) {
	for _, p := range []ShortInterestScreenParams{{Limit: 101}, {MinPrice: math.NaN()}, {MinDaysToCover: math.Inf(1)}, {SortBy: "short_interest_pct_float"}, {SortDir: "backwards"}, {MinAverageVolume: -1}} {
		if _, err := NormalizeShortInterestScreenParams(p); err == nil {
			t.Fatal("bad params accepted", p)
		}
	}
	p, err := NormalizeShortInterestScreenParams(ShortInterestScreenParams{Exclude: []string{"bbb", "AAA", "aaa"}})
	if err != nil || p.SortBy != "short_interest_shares" || p.SortDir != "desc" || p.Limit != 50 || len(p.Exclude) != 2 {
		t.Fatal(p, err)
	}
}
