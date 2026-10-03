package daemon

import (
	"context"
	"errors"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"testing"
	"time"
)

func TestSetupCalendarSelectsTwentyPriorComparableSessions(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, "2026-09-30T10:15:00-04:00")
	current, prior, err := setupSessionWindows(at, 20)
	if err != nil || len(prior) != 20 {
		t.Fatal(err, len(prior))
	}
	for i, p := range prior {
		if !p.Close.Before(current.Open) || p.Close.Sub(p.Open) != current.Close.Sub(current.Open) || p.Open.Weekday() == time.Saturday || p.Open.Weekday() == time.Sunday {
			t.Fatal("noncomparable session", p)
		}
		if i > 0 && !p.Open.After(prior[i-1].Open) {
			t.Fatal("unordered baseline")
		}
	}
	// Thanksgiving's early close cannot borrow regular-session baselines.
	early, _ := time.Parse(time.RFC3339, "2026-11-27T10:15:00-05:00")
	if _, _, err := setupSessionWindows(early, 20); err == nil {
		t.Fatal("invented twenty early-close sessions")
	}
}

func TestSetupHistoryAcquisitionIsBoundedAndNoFutureBarsLeak(t *testing.T) {
	start := time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)
	end := start.Add(29 * 24 * time.Hour)
	count := 0
	bars, err := collectSetupHistory(t.Context(), start, end, func(_ context.Context, from, to time.Time) ([]ibkr.HistoricalBar, error) {
		count++
		if to.Sub(from) > 7*24*time.Hour || to.After(end) {
			t.Fatal("unbounded read")
		}
		return []ibkr.HistoricalBar{{Time: from}, {Time: to}, {Time: from.Add(-time.Minute)}}, nil
	})
	if err != nil || count != 5 || len(bars) != 5 {
		t.Fatal(err, count, len(bars))
	}
	for _, b := range bars {
		if b.Time.Before(start) || !b.Time.Before(end) {
			t.Fatal("outside history window")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = collectSetupHistory(ctx, start, end, func(context.Context, time.Time, time.Time) ([]ibkr.HistoricalBar, error) {
		t.Fatal("read after cancellation")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSetupProfileWaitHonorsCancellation(t *testing.T) {
	var cache setupProfileCache
	if err := cache.lock(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer cache.unlock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if !errors.Is(cache.lock(ctx), context.Canceled) {
		t.Fatal("cache blocked cancellation")
	}
}
