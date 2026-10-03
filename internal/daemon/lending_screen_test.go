package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"math"
	"testing"
	"time"
)

func TestLendingScreenRanksFullFeedAndExcludesKnown(t *testing.T) {
	now := time.Date(2026, 10, 2, 20, 0, 0, 0, time.UTC)
	bulk := marketEventBorrowFeeEntry{AsOf: now.Add(-time.Minute), FetchedAt: now, SkippedRows: 2, Symbols: map[string]marketEventBorrowFeeRecord{}}
	for symbol, rate := range map[string]float64{"AAA": 90, "BBB": 90, "CCC": 150, "DDD": 20, "EEE": 70, "FFF": 65, "GGG": math.NaN()} {
		bulk.Symbols[symbol] = marketEventBorrowFeeRecord{Symbol: symbol, Name: "Synthetic " + symbol, Currency: "USD", FeeRate: rate}
	}
	rec := bulk.Symbols["EEE"]
	rec.FeeRateUnpublished = true
	bulk.Symbols["EEE"] = rec
	rec = bulk.Symbols["FFF"]
	rec.Currency = "EUR"
	bulk.Symbols["FFF"] = rec
	health := rpc.SourceHealth{Status: rpc.SourceStatusOK, RefreshState: rpc.SourceRefreshCurrent}
	p := rpc.LendingScreenParams{MinRate: 50, Limit: 1, Exclude: []string{"CCC"}}
	got := projectLendingScreen(bulk, health, p, now)
	if err := rpc.ValidateLendingScreenResult(got, p); err != nil {
		t.Fatal(err)
	}
	if got.Status != "observed" || got.Total != 6 || got.Usable != 4 || got.Matching != 2 || !got.Truncated || len(got.Rows) != 1 || got.Rows[0].Symbol != "AAA" || got.Rows[0].Name != "Synthetic AAA" || got.SkippedRows != 2 {
		t.Fatalf("incorrect discovery: %+v", got)
	}
	p.MinRate = 1000
	empty := projectLendingScreen(bulk, health, p, now)
	if empty.Status != "observed" || empty.Matching != 0 || len(empty.Rows) != 0 {
		t.Fatal("empty differs from unavailable")
	}
	for _, state := range []string{rpc.SourceRefreshFetchFailedBackoff, rpc.SourceRefreshFetchFailed} {
		health.RefreshState = state
		got = projectLendingScreen(bulk, health, p, now)
		if got.Status != "unavailable" || len(got.Rows) != 0 || got.Usable != 0 {
			t.Fatal("failed source ranked")
		}
	}
	health.RefreshState = rpc.SourceRefreshNotDue
	got = projectLendingScreen(bulk, health, p, now.Add(97*time.Hour))
	if got.Status != "unavailable" {
		t.Fatal("old off-session evidence ranked")
	}
	got = projectLendingScreen(bulk, health, p, now.Add(-time.Hour))
	if got.Status != "unavailable" {
		t.Fatal("future evidence ranked")
	}
}
