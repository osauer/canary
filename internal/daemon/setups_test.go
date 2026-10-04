package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/setups"
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
	baseline, current := src.reads("AAA")
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
	if baseline, current = src.reads("AAA"); baseline != 5 || current != 2 {
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
	if baseline, current := src.reads("AAA"); baseline != 5 || current != 2 {
		t.Fatalf("duplicate cold acquisition: baseline=%d current=%d", baseline, current)
	}
}

func setupResultJSONWithoutBaselineClock(t *testing.T, r *rpc.SetupResult) string {
	t.Helper()
	c := *r
	c.BaselineObservedAt, c.InputHash = time.Time{}, ""
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSetupProfileRollsForwardInsteadOfRebuilding(t *testing.T) {
	ny := setupNY(t)
	day1 := time.Date(2026, 9, 30, 10, 17, 0, 0, ny)
	clock := &setupTestClock{t: day1}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	if r := mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" || !r.BaselineObservedAt.Equal(day1) {
		t.Fatalf("day 1: %+v", r)
	}
	if reads := src.baselineReads("AAA"); len(reads) != 5 {
		t.Fatalf("cold window used %d reads", len(reads))
	}

	// The next session needs Sep 2..Sep 30; nineteen are held, so only Sep 30
	// is read and Sep 1 leaves the window.
	day2 := time.Date(2026, 10, 1, 10, 17, 0, 0, ny)
	clock.set(day2)
	spike := time.Date(2026, 10, 1, 10, 5, 0, 0, ny)
	src.setVolume(spike, 1000)
	r := mustEvaluateSetup(t, s, "AAA", time.Time{})
	reads := src.baselineReads("AAA")
	if len(reads) != 6 || !reads[5].start.Equal(time.Date(2026, 9, 30, 9, 30, 0, 0, ny)) || !reads[5].end.Equal(time.Date(2026, 9, 30, 16, 0, 0, 0, ny)) {
		t.Fatalf("roll-forward reads: %+v", reads)
	}
	if r.State != "confirmed" || len(r.Baseline) != 20 || r.Baseline[0].Date != "2026-09-02" || r.Baseline[19].Date != "2026-09-30" {
		t.Fatalf("rolled window: %+v", r)
	}
	// Nineteen sessions were read on day 1, so the baseline clock stays there.
	if !r.BaselineObservedAt.Equal(day1) {
		t.Fatalf("baseline clock %s, want oldest acquisition %s", r.BaselineObservedAt, day1)
	}
	// The evaluator receives exactly the evidence a full rebuild would read.
	fresh := newFakeSetupSource()
	fresh.setVolume(spike, 1000)
	cold := mustEvaluateSetup(t, setupTestServer(fresh, clock), "AAA", time.Time{})
	if got, want := setupResultJSONWithoutBaselineClock(t, r), setupResultJSONWithoutBaselineClock(t, cold); got != want {
		t.Fatalf("rolled evaluation differs from rebuild:\n%s\n%s", got, want)
	}

	// After a long weekend the two missing sessions share one read.
	clock.set(time.Date(2026, 10, 5, 10, 17, 0, 0, ny))
	if r = mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("after weekend: %+v", r)
	}
	reads = src.baselineReads("AAA")
	if len(reads) != 7 || !reads[6].start.Equal(time.Date(2026, 10, 1, 9, 30, 0, 0, ny)) || !reads[6].end.Equal(time.Date(2026, 10, 2, 16, 0, 0, 0, ny)) {
		t.Fatalf("weekend roll reads: %+v", reads)
	}
	for _, row := range s.setupProfiles.rows {
		if len(row.sessions) != 20 {
			t.Fatalf("row kept %d sessions outside the window", len(row.sessions)-20)
		}
		if _, ok := row.sessions["2026-09-02"]; ok {
			t.Fatal("session outside the window was retained")
		}
	}
}

func TestSetupReplayKeepsItsOwnProfile(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	s := setupTestServer(src, clock)
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	// A same-session replay has the live window and reads no history.
	if r := mustEvaluateSetup(t, s, "AAA", time.Date(2026, 9, 30, 10, 5, 0, 0, ny)); r.EvidenceKind != "historical_reconstruction" || r.State != "watching" {
		t.Fatalf("same-session replay: %+v", r)
	}
	if reads := src.baselineReads("AAA"); len(reads) != 5 {
		t.Fatalf("same-session replay re-read the baseline: %d", len(reads))
	}
	// A replay on another session builds its own window and leaves the live
	// window intact; repeating it reuses the replay window.
	past := time.Date(2026, 9, 15, 10, 17, 0, 0, ny)
	var afterReplay int
	for i := range 2 {
		if r := mustEvaluateSetup(t, s, "AAA", past); r.State != "watching" || r.SessionDate != "2026-09-15" {
			t.Fatalf("replay: %+v", r)
		}
		baseline, _ := src.reads("AAA")
		if i == 0 && (baseline-5 < 4 || baseline-5 > 5) {
			t.Fatalf("replay window used %d reads", baseline-5)
		}
		if i == 1 && baseline != afterReplay {
			t.Fatalf("repeated replay re-read %d sessions", baseline-afterReplay)
		}
		afterReplay = baseline
	}
	mustEvaluateSetup(t, s, "AAA", time.Time{})
	if baseline, _ := src.reads("AAA"); baseline != afterReplay {
		t.Fatalf("live window was rebuilt after a replay: %d reads", baseline-afterReplay)
	}
}

func TestSetupFetchRangesAreSingleBoundedReads(t *testing.T) {
	ny := setupNY(t)
	_, window, err := setupSessionWindows(time.Date(2026, 9, 30, 10, 17, 0, 0, ny), 20)
	if err != nil {
		t.Fatal(err)
	}
	ranges := setupFetchRanges(window)
	covered := 0
	for _, r := range ranges {
		if r[1].Sub(r[0]) > 7*24*time.Hour || !r[1].After(r[0]) {
			t.Fatalf("range %v exceeds one bounded read", r)
		}
		for _, w := range window {
			if !w.Open.Before(r[0]) && !w.Close.After(r[1]) {
				covered++
			}
		}
	}
	if covered != 20 || len(ranges) != 5 {
		t.Fatalf("ranges %d covered %d sessions", len(ranges), covered)
	}
	if got := setupFetchRanges(window[18:]); len(got) != 1 {
		t.Fatalf("adjacent sessions use %d reads", len(got))
	}
	if got := setupFetchRanges([]setups.Session{window[0], window[19]}); len(got) != 2 {
		t.Fatalf("distant sessions use %d reads", len(got))
	}
}

func TestSetupIncompleteWindowKeepsCompleteSessions(t *testing.T) {
	ny := setupNY(t)
	clock := &setupTestClock{t: time.Date(2026, 9, 30, 10, 17, 0, 0, ny)}
	src := newFakeSetupSource()
	src.setGap("2026-09-16", true)
	s := setupTestServer(src, clock)
	r := mustEvaluateSetup(t, s, "AAA", time.Time{})
	if r.State != "unavailable" || len(r.Reasons) != 1 || r.Reasons[0] != "baseline_history_unavailable: baseline_bars_incomplete" {
		t.Fatalf("incomplete window: %+v", r.Reasons)
	}
	src.setGap("2026-09-16", false)
	if r = mustEvaluateSetup(t, s, "AAA", time.Time{}); r.State != "watching" {
		t.Fatalf("after repair: %+v", r)
	}
	// Only the incomplete session is read again.
	reads := src.baselineReads("AAA")
	if len(reads) != 6 || !reads[5].start.Equal(time.Date(2026, 9, 16, 9, 30, 0, 0, ny)) || !reads[5].end.Equal(time.Date(2026, 9, 16, 16, 0, 0, 0, ny)) {
		t.Fatalf("repair reads: %+v", reads)
	}
}
