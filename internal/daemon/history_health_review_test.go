package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestHistoryRefreshBackoffBeginsAfterSlowFailure(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{now: func() time.Time { return now }}
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNTHETIC", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1M"}
	s.rememberMarketHistory(p)
	key, _, err := marketHistoryIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	request := func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		reads++
		if reads == 1 {
			// A background read can spend most of its four-minute budget
			// waiting on the historical pacing lane before it fails.
			now = now.Add(3 * time.Minute)
			return nil, context.DeadlineExceeded
		}
		return &rpc.MarketHistoryResult{Cache: &rpc.MarketHistoryCache{Selected: "ibkr"}}, nil
	}
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if item := s.marketData.interest[key]; item.Failures != 1 || !item.RetryAt.Equal(now.Add(time.Minute)) {
		t.Errorf("slow failure must retain the existing one-minute cooldown after completion: %+v; completed=%s", item, now)
	}
	now = now.Add(30 * time.Second)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if reads != 1 {
		t.Fatalf("next worker tick retried before the post-failure cooldown: reads=%d", reads)
	}
	now = now.Add(30 * time.Second)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if item := s.marketData.interest[key]; reads != 2 || item.Failures != 0 || !item.RetryAt.IsZero() {
		t.Fatalf("due successful read must clear the failure streak: reads=%d, item=%+v", reads, item)
	}
}

func TestHistoryHealthTimeoutExpiresWithoutAnotherRequest(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{now: func() time.Time { return now }}
	s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
	now = now.Add(time.Minute)
	s.observeHistorySource(false, context.DeadlineExceeded, nil, broker.SessionBinding{})
	failureAt := now

	now = now.Add(time.Minute)
	s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
	successAt := now
	row := s.feedDataHealth(historySourceID, now)
	// A newer success shows the service answering: the timeout was that one
	// request's, kept on the row but not a source concern (owner instruction
	// 2026-11-21 08:38 CET).
	if row.State != "current" || row.Failure == nil || row.Failure.Code != rpc.SourceFailureTimeout || !strings.Contains(row.Receiving, "some instrument requests failed") {
		t.Fatalf("a newer success must keep the timeout on the row without a source concern: %+v", row)
	}
	report, err := finalizeDataHealth([]rpc.DataSourceHealth{row}, "current", now, rpc.DataHealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	health := &rpc.HealthResult{Connected: true, DataHealth: &report}
	if verdict := authoritativeHealthVerdict(health); verdict.State != "READY" {
		t.Fatalf("one instrument's timeout raised the verdict: %+v", verdict)
	}

	// Expiry is evaluated on a passive status read. No new producer receipt
	// is necessary, and reading status must not manufacture a new success.
	now = failureAt.Add(5 * time.Minute)
	row = s.feedDataHealth(historySourceID, now)
	if row.State != "current" || row.Failure != nil || len(row.ProblemIDs) != 0 || !row.LastSuccess.Equal(successAt) {
		t.Fatalf("expired failure must stop degrading the still-current success: %+v", row)
	}
	report, err = finalizeDataHealth([]rpc.DataSourceHealth{row}, "current", now, rpc.DataHealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	health.DataHealth = &report
	if verdict := authoritativeHealthVerdict(health); verdict.State != "READY" {
		t.Fatalf("passive timeout expiry must clear its attention verdict: %+v", verdict)
	}
	now = successAt.Add(30 * time.Minute)
	if row = s.feedDataHealth(historySourceID, now); row.State != "unknown" || !row.LastSuccess.Equal(successAt) {
		t.Fatalf("expired success must leave historical evidence, not claim current availability: %+v", row)
	}
}
