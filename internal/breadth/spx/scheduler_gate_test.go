package spx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// A red health gate must refuse the fan-out before any per-symbol fetch:
// burning a full universe of timeouts into a known-dead farm costs the whole
// publication window and classifies a transport outage as a coverage problem.
func TestRefreshHealthGateRefusesFanOut(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA", "BBB", "CCC"}
	fake := &FakeBarFetcher{}
	e := newTestEngine(t, fake, frozenClock(now), members)
	gateErr := errors.New("historical data farm is broken")
	e.healthGate = func() error { return gateErr }

	if err := e.Refresh(context.Background()); !errors.Is(err, gateErr) {
		t.Fatalf("refresh error = %v, want the gate's error", err)
	}
	if got := fake.CallCount(); got != 0 {
		t.Fatalf("refused refresh made %d fetches, want 0", got)
	}
	progress, ok := e.Progress()
	if !ok || progress.LastFailure != RefreshFailureTransport {
		t.Fatalf("progress = %+v ok=%v, want transport_unavailable failure", progress, ok)
	}
}

// A gate that turns red mid-sweep stops dispatching: the remaining names
// would each burn a full fetch timeout. The attempt classifies as transport,
// and the completed portion stays checkpointed for the next attempt.
func TestRefreshHealthGateAbortsMidSweep(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	anchor := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	members := []string{"AAA", "BBB", "CCC"}
	fake := &FakeBarFetcher{Bars: map[string][]Bar{
		"AAA": makeSeries(100, 1.0, WindowSize, anchor),
		"BBB": makeSeries(50, 0.5, WindowSize, anchor),
		"CCC": makeSeries(200, -1.0, WindowSize, anchor),
	}}
	e := newTestEngine(t, fake, frozenClock(now), members)
	e.workers = 1
	var gateCalls atomic.Int64
	// Call 1 is the pre-sweep check, call 2 admits the first symbol; the
	// third check (second symbol) finds the farm dead.
	e.healthGate = func() error {
		if gateCalls.Add(1) > 2 {
			return errors.New("farm went down mid-sweep")
		}
		return nil
	}

	err := e.Refresh(context.Background())
	if err == nil {
		t.Fatal("mid-sweep gate failure must fail the refresh")
	}
	if got := fake.CallCount(); got != 1 {
		t.Fatalf("aborted sweep made %d fetches, want 1", got)
	}
	progress, _ := e.Progress()
	if progress.LastFailure != RefreshFailureTransport {
		t.Fatalf("last failure = %q, want %q", progress.LastFailure, RefreshFailureTransport)
	}
	if _, ok := e.Get(); ok {
		t.Fatal("aborted sweep must not publish a snapshot")
	}
}

// Transport failures poll on the short cadence; a completed-but-short pass
// holds the paced retry cadence with no counter budget — the daily tick is
// the calendar bound.
func TestNextWaitCadences(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	e := newTestEngine(t, &FakeBarFetcher{}, frozenClock(now), []string{"AAA"})
	if got := e.nextWait(false, true); got != transportRetryDelay {
		t.Fatalf("transport wait = %s, want %s", got, transportRetryDelay)
	}
	if got := e.nextWait(true, false); got != belowThresholdRetryDelay {
		t.Fatalf("retry wait = %s, want %s", got, belowThresholdRetryDelay)
	}
	if got := e.nextWait(false, false); got <= 0 {
		t.Fatalf("idle wait = %s, want positive time until next daily tick", got)
	}
}

// Failed counts only failures, so a completed pass with zero usable bars is
// distinguishable on the wire from a healthy pass with the same Processed.
func TestRefreshProgressCountsFailures(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	anchor := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	members := []string{"AAA", "BBB", "CCC"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1.0, WindowSize, anchor),
			"CCC": makeSeries(200, -1.0, WindowSize, anchor),
		},
		Errors: map[string]error{"BBB": errors.New("timeout")},
	}
	e := newTestEngine(t, fake, frozenClock(now), members)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	progress, _ := e.Progress()
	if progress.Processed != 3 || progress.Failed != 1 {
		t.Fatalf("processed=%d failed=%d, want 3 and 1", progress.Processed, progress.Failed)
	}
}

// Kick wakes a sleeping scheduler immediately instead of letting it finish
// the current delay — the daemon fires it when the bulk lane is rebuilt.
func TestKickWakesSleepingRun(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	e := newTestEngine(t, &FakeBarFetcher{}, frozenClock(now), []string{"AAA"})
	// A current snapshot suppresses the bootstrap refresh, so Run goes
	// straight to the long daily-tick sleep.
	e.snapshot = &Snapshot{SessionKey: CompletedSessionKey(now), AsOf: now}

	gateCalled := make(chan struct{}, 4)
	e.healthGate = func() error {
		gateCalled <- struct{}{}
		return errors.New("hold the refresh; the test only needs the wake-up")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()

	e.Kick()
	select {
	case <-gateCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("Kick did not wake the scheduler within 5s")
	}
	cancel()
	<-done
}
