package daemon

import (
	"context"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
	"time"
)

func TestMandateRefreshesIntradayBarAfterClose(t *testing.T) {
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	srv := &Server{now: func() time.Time { return now }}
	reads := 0
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, at time.Time) (*rpc.MarketHistoryResult, error) {
		reads++
		value := 100.0
		if at.Hour() >= 21 {
			value = 120
		}
		return &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: at, Source: "synthetic", Points: []rpc.MarketHistoryPoint{{At: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), Value: 99}, {At: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), Value: value}}}, nil
	}
	need := mandateHistoryNeed{Key: "u:SYNX", Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"}, Market: marketcal.MarketUSEquity}
	source := &fakeSetupSource{}
	srv.refreshMandateSeries(t.Context(), source, need)
	now = time.Date(2026, 10, 19, 21, 0, 0, 0, time.UTC)
	srv.refreshMandateSeries(t.Context(), source, need)
	series := srv.mandateHistoryForNeed(t.Context(), need)
	if reads != 2 || series.Closes[len(series.Closes)-1] != 120 {
		t.Fatalf("after completed close: reads=%d last=%s close=%v due=%v stale=%v; expected new completed close 120, not retained intraday 100", reads, series.Dates[len(series.Dates)-1], series.Closes[len(series.Closes)-1], mandateSeriesDue(series, now), mandateSeriesStale(series, now))
	}
}
