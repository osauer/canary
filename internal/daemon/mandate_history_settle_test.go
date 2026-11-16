package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A daily bar read while the closing auction still runs is not a close. On
// 2026-10-30 the core's London line was read 40 seconds after the scheduled
// close, the last continuous print was kept as the month-end close, the
// month was frozen on it, and the next session's read of the auction close
// held the month with core_target_drift. The store keeps a session's bar
// only from a read taken coreDecisionAfter past its scheduled close, and
// reads again then.
func TestMandateHistoryKeepsNoCloseReadDuringTheClosingAuction(t *testing.T) {
	const provisional, final = 101.0, 101.5
	closeAt := time.Date(2026, 10, 30, 16, 30, 0, 0, time.UTC) // 16:30 London
	now := closeAt.Add(40 * time.Second)
	srv := &Server{now: func() time.Time { return now }}
	reads := 0
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, at time.Time) (*rpc.MarketHistoryResult, error) {
		reads++
		value := provisional
		if !at.Before(closeAt.Add(10 * time.Minute)) {
			value = final
		}
		return &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: at, Source: "synthetic", Points: []rpc.MarketHistoryPoint{
			{At: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Value: 100},
			{At: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), Value: value},
		}}, nil
	}
	need := mandateHistoryNeed{Key: "core:con:9030", Contract: rpc.ContractParams{ConID: 9030, Symbol: "SYNF", SecType: "STK", Currency: "EUR", Exchange: "LSEETF", PrimaryExch: "LSEETF"}, Market: marketcal.MarketUKLSE}
	source := &fakeSetupSource{}

	srv.refreshMandateSeries(t.Context(), source, need)
	if series := srv.mandateHistoryForNeed(t.Context(), need); series != nil && series.Dates[len(series.Dates)-1] == "2026-10-30" {
		t.Fatalf("40 seconds after the scheduled close the store keeps %v as the close of 2026-10-30; the auction has not ended", series.Closes[len(series.Closes)-1])
	}

	now = closeAt.Add(coreDecisionAfter + time.Minute)
	srv.refreshMandateSeries(t.Context(), source, need)
	series := srv.mandateHistoryForNeed(t.Context(), need)
	if series == nil || series.Dates[len(series.Dates)-1] != "2026-10-30" || series.Closes[len(series.Closes)-1] != final {
		t.Fatalf("after the close settled: reads %d, series %+v; want the close of 2026-10-30 at %v", reads, series, final)
	}
	if mandateSeriesDue(series, now) || mandateSeriesStale(series, now) {
		t.Fatalf("the settled series reads due %v stale %v", mandateSeriesDue(series, now), mandateSeriesStale(series, now))
	}

	// A document an older build kept from a read inside the margin loses
	// that bar when it is loaded, so no decision is frozen on it.
	kept := completedMandateSeries(&mandateHistorySeries{Version: mandateHistoryDocVer, Key: need.Key, Contract: need.Contract, Market: need.Market,
		Dates: []string{"2026-10-29", "2026-10-30"}, Closes: []float64{100, provisional}, FetchedAt: closeAt.Add(40 * time.Second)})
	if kept == nil || len(kept.Dates) != 1 || kept.Dates[0] != "2026-10-29" {
		t.Fatalf("a retained read from inside the margin keeps %+v", kept)
	}
}
