package daemon

import (
	"context"
	"errors"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
	"time"
)

func TestMandateHistoryBudgetAdvancesWithCompletedVenueSession(t *testing.T) {
	for _, market := range []marketcal.Market{marketcal.MarketUSEquity, marketcal.MarketDEXetra, marketcal.MarketUKLSE} {
		for _, fail := range []bool{false, true} {
			t.Run(string(market)+"/"+map[bool]string{false: "success", true: "exhausted"}[fail], func(t *testing.T) {
				now := time.Date(2026, 10, 16, 22, 0, 0, 0, time.UTC)
				srv := &Server{now: func() time.Time { return now }}
				reads := 0
				srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, at time.Time) (*rpc.MarketHistoryResult, error) {
					reads++
					if fail {
						return nil, errors.New("synthetic unavailable")
					}
					day, _, ok := mandateLastSessionsIn(at, market)
					if !ok {
						t.Fatal("calendar missing")
					}
					stamp, _ := time.Parse(time.DateOnly, day)
					return &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: at, Source: "synthetic", Points: []rpc.MarketHistoryPoint{{At: stamp, Value: 100}}}, nil
				}
				source := &fakeSetupSource{}
				need := mandateHistoryNeed{Key: "u:SYNX", Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"}, Market: market}
				for range 5 {
					srv.refreshMandateSeries(t.Context(), source, need)
					now = now.Add(mandateHistoryRetryAfter)
				}
				want := 1
				if fail {
					want = mandateHistoryAttemptsPerSession
				}
				if reads != want {
					t.Fatalf("same session reads=%d want%d", reads, want)
				}
				now = time.Date(2026, 10, 18, 22, 0, 0, 0, time.UTC)
				srv.refreshMandateSeries(t.Context(), source, need)
				if reads != want {
					t.Fatalf("weekend reset: %d", reads)
				}
				now = time.Date(2026, 10, 19, 22, 0, 0, 0, time.UTC)
				fail = false
				srv.refreshMandateSeries(t.Context(), source, need)
				if reads != want+1 {
					t.Fatalf("next market session did not refresh: %d want%d", reads, want+1)
				}
			})
		}
	}
}
