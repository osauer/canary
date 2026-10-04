package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
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

type setupEvaluation struct {
	result *rpc.SetupResult
	err    error
}

func goEvaluateSetup(ctx context.Context, s *Server, symbol string) <-chan setupEvaluation {
	out := make(chan setupEvaluation, 1)
	go func() {
		r, err := evaluateSetupForTest(ctx, s, symbol, 0, time.Time{})
		out <- setupEvaluation{r, err}
	}()
	return out
}

func TestSetupCachedProfileDoesNotWaitForColdAcquisition(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("warm-up evaluation: %+v", r)
	}
	baseline, current := src.reads("AAA", "2026-09-30")
	if baseline != 5 || current != 1 {
		t.Fatalf("cold acquisition reads baseline=%d current=%d", baseline, current)
	}
	// BBB's cold acquisition holds the serializing gate while its broker read
	// is outstanding.
	release := src.holdSymbol("BBB")
	defer release()
	cold := goEvaluateSetup(t.Context(), s, "BBB")
	<-src.entered
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := evaluateSetupForTest(ctx, s, "AAA", 0, time.Time{})
	if err != nil || r.State != "watching" || r.BaselineSessions != 20 {
		t.Fatalf("cached profile waited behind a cold acquisition: %+v %v", r, err)
	}
	if baseline, current = src.reads("AAA", "2026-09-30"); baseline != 5 || current != 2 {
		t.Fatalf("cached profile re-read history: baseline=%d current=%d", baseline, current)
	}
	release()
	if got := <-cold; got.err != nil || got.result.State != "watching" {
		t.Fatalf("cold acquisition: %+v %v", got.result, got.err)
	}
}

func TestSetupColdAcquisitionRechecksCacheAfterGate(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	waiting := make(chan struct{}, 1)
	s.setupProfiles.waitForTest = func() { waiting <- struct{}{} }
	release := src.holdSymbol("AAA")
	defer release()
	first := goEvaluateSetup(t.Context(), s, "AAA")
	<-src.entered
	second := goEvaluateSetup(t.Context(), s, "AAA")
	<-waiting
	release()
	for _, ch := range []<-chan setupEvaluation{first, second} {
		if got := <-ch; got.err != nil || got.result.State != "watching" {
			t.Fatalf("evaluation: %+v %v", got.result, got.err)
		}
	}
	// The waiter found the profile its predecessor stored instead of
	// collecting the same twenty sessions again.
	if baseline, current := src.reads("AAA", "2026-09-30"); baseline != 5 || current != 2 {
		t.Fatalf("duplicate cold acquisition: baseline=%d current=%d", baseline, current)
	}
}
