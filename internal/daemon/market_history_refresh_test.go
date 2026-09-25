package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// The broker's "no security definition" verdict does not change within a
// broker session. The worker remembers it per contract, so both remembered
// ranges stop asking, the verdict is said once rather than per attempt per
// range, recorded history keeps being served, and a later successful read
// clears the memory.
func TestMarketHistoryRefreshRemembersTheBrokersDefinitionVerdict(t *testing.T) {
	s, p, key, now, r := historyFixture(t)
	log := &bytes.Buffer{}
	s.logger = NewLogger(log, "info")
	if _, err := s.readRetainedHistory(t.Context(), key, p, now, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		return &r, nil
	}); err != nil {
		t.Fatal(err)
	}
	clock := now.AddDate(0, 0, 8) // the weekly full read is due
	s.now = func() time.Time { return clock }
	intraday := p
	intraday.Range = "1D"
	s.rememberMarketHistory(p)
	s.rememberMarketHistory(intraday)
	intradayKey, _, err := marketHistoryIdentity(intraday)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.marketData.interest[key]; !ok || len(s.marketData.interest) != 2 {
		t.Fatalf("both ranges must be remembered under their own keys: %d", len(s.marketData.interest))
	}
	reads := 0
	answer := fmt.Errorf("contract details unresolved for SYNTH: %w", ibkrlib.ErrContractNoDefinition)
	request := func(ctx context.Context, got rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		k, got, err := marketHistoryIdentity(got)
		if err != nil {
			t.Fatal(err)
		}
		return s.readRetainedHistory(ctx, k, got, clock, func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
			reads++
			if answer != nil {
				return nil, answer
			}
			fresh := r
			fresh.AsOf = clock
			fresh.RequestedStart = clock.AddDate(0, -1, -1)
			return &fresh, nil
		})
	}
	verdicts := func() int { return strings.Count(log.String(), "no security definition for contract") }
	retryNow := func(key string) {
		item := s.marketData.interest[key]
		item.RetryAt = time.Time{}
		s.marketData.interest[key] = item
	}

	// Recorded bars exist for the daily series, so the store swallows the
	// verdict into a served fallback and relays it to the worker's memory.
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if reads != 1 || verdicts() != 1 || strings.Contains(log.String(), "IBKR refresh failed") {
		t.Fatalf("the verdict must be said once, not as a per-attempt fallback: reads=%d %q", reads, log.String())
	}
	if !strings.Contains(log.String(), "market history refresh SYNTH: contract details unresolved for SYNTH: no security definition for contract; paused for the rest of this broker session and until at least "+clock.Add(30*time.Minute).Format(time.TimeOnly)) {
		t.Fatalf("the verdict must name its symbol and floor: %q", log.String())
	}
	if miss, ok := s.marketData.definitionMisses[p.Contract]; !ok || !miss.At.Equal(clock) {
		t.Fatalf("the verdict is remembered by contract: %+v %t", miss, ok)
	}
	// The intraday range of the same contract is not asked at all, and the
	// daily one is held by the verdict rather than by its own backoff.
	s.refreshMarketHistoryInterest(t.Context(), intradayKey, request)
	retryNow(key)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if reads != 1 || verdicts() != 1 {
		t.Fatalf("a contract under a verdict must not be read again within the floor: reads=%d verdicts=%d", reads, verdicts())
	}
	// Past the floor the verdict holds while the broker session lasts.
	clock = clock.Add(31 * time.Minute)
	s.marketHistorySessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return true }
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if reads != 1 {
		t.Fatalf("the verdict lapsed while its broker session was still current: reads=%d", reads)
	}
	// A new session earns one probe. Nothing is recorded for the intraday
	// series, so its verdict returns as an error and the worker remembers it
	// itself; the daily series is then skipped again.
	s.marketHistorySessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return false }
	s.refreshMarketHistoryInterest(t.Context(), intradayKey, request)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if reads != 2 || verdicts() != 2 || strings.Contains(log.String(), "next attempt after") {
		t.Fatalf("a new session is asked once and told once: reads=%d verdicts=%d %q", reads, verdicts(), log.String())
	}
	// Bars clear the verdict, and the worker says the series recovered.
	clock = clock.Add(31 * time.Minute)
	answer = nil
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if _, refused := s.marketData.definitionMisses[p.Contract]; reads != 3 || refused || s.marketData.interest[key].Failures != 0 {
		t.Fatalf("a successful read must clear the verdict: reads=%d refused=%t %+v", reads, refused, s.marketData.interest[key])
	}
	if !strings.Contains(log.String(), "market history refresh SYNTH 1M: recovered") {
		t.Fatalf("recovery must be logged: %q", log.String())
	}
	s.refreshMarketHistoryInterest(t.Context(), intradayKey, request)
	if reads != 4 {
		t.Fatalf("with the verdict cleared the intraday series is read again: reads=%d", reads)
	}
}

// An unentitled index on IBKR's delayed feed trails the market by fifteen
// minutes, so for that long after the US open the broker has no bar inside
// the new session's one-day window. Each such read was refused as "no
// observations within requested range": the record kept its premarket fetch
// clock and was served refresh due, and the worker backed the series off one,
// two, four and eight minutes, past the moment its first bar arrived. The
// broker answered, so the read is not a failure: the series serves the
// previous session, labelled the previous window, fetched now, and is read
// again on its normal cadence until the session's first delayed bar lands.
func TestMarketHistoryRefreshCarriesADelayedIndexThroughTheOpen(t *testing.T) {
	s, _, _, _, _ := historyFixture(t)
	log := &bytes.Buffer{}
	s.logger = NewLogger(log, "warn")
	key, p, err := marketHistoryIdentity(rpc.MarketHistoryParams{Contract: rpc.ContractParams{ConID: 777001, Symbol: "SYNDX", SecType: "IND", Exchange: "NASDAQ", Currency: "USD"}, Range: "1D"})
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	open := time.Date(2026, 9, 15, 9, 30, 0, 0, ny) // Tuesday
	monday := open.AddDate(0, 0, -1)
	mondayLast := monday.Add(77 * 5 * time.Minute)
	var bars []ibkrlib.HistoricalBar
	for _, session := range []time.Time{monday, open} {
		for i := range 78 {
			bars = append(bars, ibkrlib.HistoricalBar{Time: session.Add(time.Duration(i) * 5 * time.Minute), Open: 100, High: 101, Low: 99, Close: 100.5})
		}
	}
	const lag = 15 * time.Minute
	clock := open.Add(-3*time.Hour - 31*time.Minute) // 05:59 New York
	s.now = func() time.Time { return clock }
	reads := 0
	broker := func(_ context.Context, c ibkrlib.Contract, _ int, _ string, _ time.Duration) (ibkrlib.ChartSeries, error) {
		reads++
		series := ibkrlib.ChartSeries{Contract: c, WhatToShow: "TRADES"}
		for _, b := range bars {
			if !b.Time.After(clock.Add(-lag)) {
				series.Bars = append(series.Bars, b)
			}
		}
		return series, nil
	}
	request := func(ctx context.Context, got rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		k, got, err := marketHistoryIdentity(got)
		if err != nil {
			t.Fatal(err)
		}
		return s.readRetainedHistory(ctx, k, got, clock, func(ctx context.Context, p rpc.MarketHistoryParams, tail int, now time.Time) (*rpc.MarketHistoryResult, error) {
			return fetchMarketHistory(ctx, p, tail, now, broker)
		})
	}
	served := func() *rpc.MarketHistoryResult {
		t.Helper()
		raw, _ := json.Marshal(p)
		got, err := s.handleMarketHistory(t.Context(), &rpc.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	s.rememberMarketHistory(p)
	s.refreshMarketHistoryInterest(t.Context(), key, request)
	if got := served(); reads != 1 || got.Cache.Coverage != "observed" || !got.End.Equal(mondayLast) || got.Cache.RefreshDue {
		t.Fatalf("the premarket read must record Monday's session: reads=%d %+v", reads, got.Cache)
	}
	for _, after := range []time.Duration{19 * time.Second, 5*time.Minute + 19*time.Second, 10*time.Minute + 19*time.Second, 15*time.Minute + 19*time.Second, 20*time.Minute + 19*time.Second} {
		clock = open.Add(after)
		before := reads
		s.refreshMarketHistoryInterest(t.Context(), key, request)
		if item := s.marketData.interest[key]; reads != before+1 || item.Failures != 0 || !item.RetryAt.IsZero() {
			t.Fatalf("%s after the open: an answered read must keep the normal cadence: reads=%d failures=%d retry_at=%s", after, reads-before, item.Failures, item.RetryAt)
		}
		got := served()
		c := got.Cache
		if c.RefreshDue || c.RefreshFailed || !c.FetchedAt.Equal(clock) {
			t.Fatalf("%s after the open: the series must be current as of this read: %+v", after, c)
		}
		newest := clock.Add(-lag).Truncate(5 * time.Minute)
		if newest.Before(open) {
			if !c.PreviousWindow || c.Coverage != "previous_window" || !got.End.Equal(mondayLast) {
				t.Fatalf("%s after the open: before its first delayed bar the session must serve Monday as the previous window: end=%s %+v", after, got.End, c)
			}
			continue
		}
		if c.PreviousWindow || c.Coverage != "observed" || !got.Start.Equal(open) || !got.End.Equal(newest) {
			t.Fatalf("%s after the open: the session's delayed bars must be served once they land: %s..%s %+v", after, got.Start, got.End, c)
		}
	}
	if strings.Contains(log.String(), "refresh failed") || strings.Contains(log.String(), "next attempt after") {
		t.Fatalf("answered reads were logged as failures: %q", log.String())
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
