package stress

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type stressCallStub struct {
	calls         []string
	failMethod    string
	accountCalls  int
	marketSymbols []string
}

func (s *stressCallStub) Call(_ context.Context, method string, params, result any) error {
	s.calls = append(s.calls, method)
	if method == s.failMethod {
		return errors.New("stub failure")
	}
	now := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	switch method {
	case rpc.MethodAccountSummary:
		s.accountCalls++
		account := rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100_000, AsOf: now}
		if s.accountCalls > 1 {
			dailyPnL := -100.0
			account.DailyPnL = &dailyPnL
		}
		*result.(*rpc.AccountResult) = account
	case rpc.MethodPositionsList:
		*result.(*rpc.PositionsResult) = rpc.PositionsResult{
			AsOf: now,
			Stocks: []rpc.PositionView{
				{Symbol: "msft"},
				{Symbol: "AAPL"},
			},
		}
	case rpc.MethodRegimeSnapshot:
		*result.(*rpc.RegimeSnapshotResult) = rpc.RegimeSnapshotResult{AsOf: now}
	case rpc.MethodMarketEventsSnapshot:
		s.marketSymbols = slices.Clone(params.(rpc.MarketEventsParams).Symbols)
		return errors.New("market events unavailable")
	}
	return nil
}

func TestFetchStressSnapshotPreservesCallOrderAndFallbacks(t *testing.T) {
	t.Parallel()
	conn := &stressCallStub{}
	result, positions, _, err := FetchStressSnapshotWithRegime(t.Context(), conn)
	if err != nil {
		t.Fatalf("FetchStressSnapshotWithRegime: %v", err)
	}
	wantCalls := []string{
		rpc.MethodAccountSummary,
		rpc.MethodPositionsList,
		rpc.MethodRegimeSnapshot,
		rpc.MethodMarketEventsSnapshot,
		rpc.MethodAccountSummary,
	}
	if !slices.Equal(conn.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", conn.calls, wantCalls)
	}
	if !slices.Equal(conn.marketSymbols, []string{"AAPL", "MSFT"}) {
		t.Fatalf("market symbols = %v, want sorted held symbols", conn.marketSymbols)
	}
	if len(positions.Stocks) != 2 {
		t.Fatalf("positions stocks = %d, want 2", len(positions.Stocks))
	}
	if result.Portfolio.DailyPnLPct == nil {
		t.Fatal("daily P&L refresh was not used")
	}
	if result.SourceFingerprints.MarketEvents == nil {
		t.Fatal("market-events fallback fingerprint missing")
	}
	foundFallback := false
	for _, health := range result.SourceHealth {
		if health.Source == "market_events" && health.Status == rpc.SourceStatusUnknown {
			foundFallback = true
		}
	}
	if !foundFallback {
		t.Fatalf("market-events fallback health missing: %+v", result.SourceHealth)
	}
	if result.InputHealth != InputDegraded || !strings.Contains(strings.Join(result.Warnings, "\n"), "market-event source market_events: unknown") {
		t.Fatalf("fallback decision = input %q warnings %+v, want degraded with explicit unknown market-event source", result.InputHealth, result.Warnings)
	}
}

func TestFetchStressSnapshotWrapsRequiredSourceErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		want   string
	}{
		{method: rpc.MethodAccountSummary, want: "account: stub failure"},
		{method: rpc.MethodPositionsList, want: "positions: stub failure"},
		{method: rpc.MethodRegimeSnapshot, want: "regime: stub failure"},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			conn := &stressCallStub{failMethod: test.method}
			_, _, _, err := FetchStressSnapshotWithRegime(t.Context(), conn)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
