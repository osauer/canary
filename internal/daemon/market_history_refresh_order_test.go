package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A series a reader keeps asking for is refreshed ahead of series nobody has
// asked for in hours, however many are followed and however long each paced
// read takes. On 2026-11-24 the worker walked its sixty-odd series in key
// order once per sweep while paced reads stretched the sweep past twenty
// minutes, so SPX and SPY, late in the key order and read by market.read
// every thirty seconds, went 23 minutes unrefreshed while VIX, early in it,
// stayed current. Synthetic series; the clock is the test's.
func TestMarketHistoryRefreshServesTheSeriesBeingReadFirst(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	s.marketData.interest = map[string]marketHistoryInterest{}
	follow := func(symbol string, asked time.Time) string {
		p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1D"}
		key, normalized, err := marketHistoryIdentity(p)
		if err != nil {
			t.Fatal(err)
		}
		s.marketData.interest[key] = marketHistoryInterest{Params: normalized, Until: asked.Add(marketHistoryInterestFor)}
		return key
	}
	for i := range 20 {
		follow(fmt.Sprintf("OLD%02d", i), now.Add(-time.Duration(i+1)*time.Hour))
	}
	read := follow("READ", now)
	var calls []string
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := func(_ context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		calls = append(calls, p.Contract.Symbol)
		now = now.Add(20 * time.Second) // one paced read
		// market.read asks for its series again on its next read.
		item := s.marketData.interest[read]
		item.Until = now.Add(marketHistoryInterestFor)
		s.marketData.interest[read] = item
		if len(calls) == 60 {
			cancel()
		}
		return &rpc.MarketHistoryResult{}, nil
	}
	s.refreshMarketHistoryDue(ctx, request)
	gap, seen := 0, map[string]bool{}
	for i, symbol := range calls {
		seen[symbol] = true
		if symbol == "READ" {
			gap = 0
			continue
		}
		if gap++; gap > 2 {
			t.Fatalf("call %d: the series being read waited behind %d others: %v", i, gap, calls[:i+1])
		}
	}
	if calls[0] != "READ" || len(seen) != 21 {
		t.Fatalf("first %s; %d of 21 series refreshed: %v", calls[0], len(seen), calls)
	}
}
