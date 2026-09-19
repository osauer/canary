package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestMarketHistoryRefreshWaitsOnTheBackgroundLane(t *testing.T) {
	log := &bytes.Buffer{}
	s := &Server{logger: NewLogger(log, "info")}
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "5Y"}
	s.rememberMarketHistory(p)
	key, _, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	var seen []time.Duration
	request := func(ctx context.Context, got rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		if got.Range != "5Y" || ibkrlib.RequestPriorityFrom(ctx) != ibkrlib.PriorityBackground {
			t.Fatalf("refresh must ask for the remembered range on the background lane: %s %v", got.Range, ibkrlib.RequestPriorityFrom(ctx))
		}
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("refresh without a deadline")
		}
		seen = append(seen, time.Until(deadline))
		switch len(seen) {
		case 1:
			return nil, errors.New("historical rate limit: context deadline exceeded")
		case 2:
			return &rpc.MarketHistoryResult{Cache: &rpc.MarketHistoryCache{Selected: "cache", RefreshFailed: true}}, nil
		}
		return &rpc.MarketHistoryResult{Cache: &rpc.MarketHistoryCache{Selected: "ibkr"}}, nil
	}
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if len(seen) != 1 || seen[0] < 3*time.Minute || seen[0] > marketHistoryRefreshWindow {
		t.Fatalf("a refresh must outlast the pacing queue, not a thirty-second read: %v", seen)
	}
	if item := s.marketData.interest[key]; item.Failures != 1 || item.RetryAt.Before(time.Now().Add(50*time.Second)) {
		t.Fatalf("first failure must back off a minute: %+v", item)
	}
	if !strings.Contains(log.String(), "market history refresh SPY 5Y: historical rate limit") {
		t.Fatalf("a failed refresh must say why: %q", log.String())
	}
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if len(seen) != 1 {
		t.Fatal("a key in backoff must not be read again")
	}
	item := s.marketData.interest[key]
	item.RetryAt = time.Time{}
	s.marketData.interest[key] = item
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if item := s.marketData.interest[key]; len(seen) != 2 || item.Failures != 2 {
		t.Fatalf("a refused refresh counts as a failure: %+v reads=%d", item, len(seen))
	}
	item = s.marketData.interest[key]
	item.RetryAt = time.Time{}
	s.marketData.interest[key] = item
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if item := s.marketData.interest[key]; len(seen) != 3 || item.Failures != 0 || !item.RetryAt.IsZero() {
		t.Fatalf("a successful refresh clears the streak: %+v", item)
	}
	if !strings.Contains(log.String(), "market history refresh SPY 5Y: recovered") {
		t.Fatalf("recovery must be logged: %q", log.String())
	}
	s.refreshMarketHistoryInterest(t.Context(), "unknown", request)
	if len(seen) != 3 {
		t.Fatal("a forgotten key is not read")
	}
}

func TestMarketHistoryFallbackNamesItsCause(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	log := &bytes.Buffer{}
	s.logger = NewLogger(log, "warn")
	if _, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	}); err != nil {
		t.Fatal(err)
	}
	later := now.AddDate(0, 0, 8)
	got, err := s.readRetainedHistory(t.Context(), key, p, later, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return nil, errors.New("historical rate limit: context deadline exceeded")
	})
	if err != nil || !got.Cache.RefreshFailed {
		t.Fatalf("recorded history must still be served: %+v %v", got, err)
	}
	if !strings.Contains(log.String(), "IBKR refresh failed: historical rate limit: context deadline exceeded; serving recorded history through "+r.End.UTC().Format("2006-01-02")) {
		t.Fatalf("the fallback must name its cause and coverage: %q", log.String())
	}
}
