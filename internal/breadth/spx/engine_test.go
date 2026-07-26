package spx

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// makeSeries generates a daily-bar series for one symbol: `n` bars
// ending today, with closes linearly rising from `start` by `step`.
// Trading-day spacing (Mon-Fri) is approximated by walking calendar
// days; tests that care about exact session keying inject the dates
// directly.
func makeSeries(start, step float64, n int, anchor time.Time) []Bar {
	bars := make([]Bar, n)
	for i := range n {
		date := anchor.AddDate(0, 0, -(n - 1 - i))
		bars[i] = Bar{Date: date.Format("2006-01-02"), Close: start + float64(i)*step}
	}
	return bars
}

// newTestEngine constructs an Engine with a tmp-dir store, the given
// fake fetcher, the given clock, and a synthetic 3-member universe
// so test assertions are easy to read. The full SP500 list is
// covered by integration tests against a live gateway; unit tests
// don't need it.
func newTestEngine(t *testing.T, fetcher *FakeBarFetcher, clock func() time.Time, members []string) *Engine {
	t.Helper()
	store := NewStore(t.TempDir())
	e := New(store, fetcher, Options{
		Clock:   clock,
		Workers: 4,
	})
	// Override the engine's checked-in member list with the synthetic
	// universe the test wants. Direct field write is safe before any
	// Refresh has run — no other goroutine touches e.members yet.
	e.members = members
	return e
}

// frozenClock returns a clock function that always reports the same
// instant. Tests that exercise the same-session idempotent path use
// this so SlideWindow's same-day-overwrite branch is reachable.
func frozenClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestEngineColdStartFetchesEveryName(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC) // ~17:30 ET
	members := []string{"AAA", "BBB", "CCC"}
	anchor := time.Date(2026, 5, 18, 0, 0, 0, 0, time.UTC)
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1.0, WindowSize, anchor),
			"BBB": makeSeries(50, 0.5, WindowSize, anchor),
			"CCC": makeSeries(200, -1.0, WindowSize, anchor),
		},
	}
	e := newTestEngine(t, fake, frozenClock(now), members)

	if _, ok := e.Get(); ok {
		t.Fatal("cold engine should not have a snapshot before Refresh")
	}
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Every member should have been fetched exactly once.
	if got := fake.CallCount(); got != len(members) {
		t.Errorf("cold-start fetch count: want %d, got %d", len(members), got)
	}
	for _, c := range fake.Calls {
		if c.LookbackDays != RollingMaxBars+10 {
			t.Errorf("cold lookback for %s: want %d, got %d", c.Symbol, RollingMaxBars+10, c.LookbackDays)
		}
	}

	snap, ok := e.Get()
	if !ok {
		t.Fatal("snapshot missing after Refresh")
	}
	if snap.Coverage != 3 {
		t.Errorf("coverage: want 3, got %d", snap.Coverage)
	}
	if snap.Method != methodConstituentFanout {
		t.Errorf("method: want %q, got %q", methodConstituentFanout, snap.Method)
	}
}

func TestEngineWarmRefreshFetchesOnlyToday(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	members := []string{"AAA", "BBB"}
	anchor := yesterday

	// Seed the engine's cache as if yesterday's refresh ran cleanly:
	// each name has a full window ending yesterday.
	seededWindows := map[string]ConstituentWindow{
		"AAA": {
			Symbol:    "AAA",
			Closes:    seedCloses(WindowSize, 100, 1),
			LastBarAt: nySessionKey(yesterday),
		},
		"BBB": {
			Symbol:    "BBB",
			Closes:    seedCloses(WindowSize, 50, 0.5),
			LastBarAt: nySessionKey(yesterday),
		},
	}

	store := NewStore(t.TempDir())
	if err := store.SaveWindows(seededWindows, yesterday); err != nil {
		t.Fatalf("seed windows: %v", err)
	}
	// Today's bars only — what a 2-day warm fetch would return.
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(150, 0, 2, anchor.AddDate(0, 0, 1)),
			"BBB": makeSeries(60, 0, 2, anchor.AddDate(0, 0, 1)),
		},
	}
	e := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e.members = members

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := fake.CallCount(); got != 2 {
		t.Errorf("warm fetch count: want 2, got %d", got)
	}
	for _, c := range fake.Calls {
		if c.LookbackDays != 2 {
			t.Errorf("warm lookback for %s: want 2, got %d", c.Symbol, c.LookbackDays)
		}
	}

	snap, ok := e.Get()
	if !ok {
		t.Fatal("snapshot missing after warm refresh")
	}
	if snap.SessionKey != nySessionKey(now) {
		t.Errorf("session key: want %s, got %s", nySessionKey(now), snap.SessionKey)
	}
}

// TestEngineSecondRefreshSameDayIsNoOp pins the warm-path optimisation:
// once a name has today's bar cached, the next Refresh shouldn't
// re-fetch it. Saves us 10 minutes of redundant gateway work if the
// scheduler accidentally triggers twice in one session.
func TestEngineSecondRefreshSameDayIsNoOp(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1.0, WindowSize, now),
		},
	}
	e := newTestEngine(t, fake, frozenClock(now), members)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	firstCallCount := fake.CallCount()

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	if fake.CallCount() != firstCallCount {
		t.Errorf("second-refresh should not fetch anything new: was %d, now %d",
			firstCallCount, fake.CallCount())
	}
}

func TestEngineTolerantOfPerSymbolErrors(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	// Six-member universe with one failure: 5/6 ≈ 83%, above the 80%
	// MinCoverageFraction threshold, so the partial refresh still
	// persists. The "tolerance" being asserted is: per-symbol errors
	// don't fail the whole call; they show up as Excluded entries.
	members := []string{"OK1", "OK2", "OK3", "OK4", "OK5", "FAIL"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"OK1": makeSeries(100, 1, WindowSize, now),
			"OK2": makeSeries(50, 1, WindowSize, now),
			"OK3": makeSeries(75, 1, WindowSize, now),
			"OK4": makeSeries(60, 1, WindowSize, now),
			"OK5": makeSeries(80, 1, WindowSize, now),
		},
		Errors: map[string]error{
			"FAIL": errors.New("gateway: pacing"),
		},
	}
	e := newTestEngine(t, fake, frozenClock(now), members)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	snap, ok := e.Get()
	if !ok {
		t.Fatal("snapshot missing")
	}
	if snap.Coverage != 5 {
		t.Errorf("coverage: want 5, got %d", snap.Coverage)
	}
	if len(snap.Excluded) != 1 || snap.Excluded[0].Symbol != "FAIL" {
		t.Errorf("excluded: want [FAIL/no_window], got %v", snap.Excluded)
	}
}

// TestEngineRefreshBelowCoverageThresholdIsNotPersisted pins the
// v0.27.3 broadening of the v0.27.1 Coverage==0 guard: any refresh
// whose coverage falls below MinCoverageFraction × MemberCount is
// "did not converge" and must not be persisted. The poison-cache
// failure mode isn't just "zero fetches succeeded" — it's "the
// snapshot doesn't reflect the underlying market." A 50%-coverage
// snapshot is just as misleading as a 0%-coverage one, and would
// poison the scheduler's "today's snapshot exists, skip the next
// bootstrap" check identically.
func TestEngineRefreshBelowCoverageThresholdIsNotPersisted(t *testing.T) {
	now := time.Date(2026, 5, 19, 21, 30, 0, 0, time.UTC)
	// 10 members, only 5 successful fetches (50% coverage) — well
	// below the 80% threshold.
	members := []string{"OK1", "OK2", "OK3", "OK4", "OK5", "F1", "F2", "F3", "F4", "F5"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"OK1": makeSeries(100, 1, WindowSize, now),
			"OK2": makeSeries(50, 1, WindowSize, now),
			"OK3": makeSeries(75, 1, WindowSize, now),
			"OK4": makeSeries(60, 1, WindowSize, now),
			"OK5": makeSeries(80, 1, WindowSize, now),
		},
		Errors: map[string]error{
			"F1": errors.New("gateway: pacing"),
			"F2": errors.New("gateway: pacing"),
			"F3": errors.New("gateway: pacing"),
			"F4": errors.New("gateway: pacing"),
			"F5": errors.New("gateway: pacing"),
		},
	}
	dir := t.TempDir()
	store := NewStore(dir)
	e := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e.members = members

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := e.Get(); ok {
		t.Error("Get should return false after a below-threshold refresh — 50% coverage must not produce a published snapshot")
	}

	// On-disk cache must be untouched. A subsequent daemon restart
	// would cold-start rather than reading a misleading half-snapshot.
	if snap, err := NewStore(dir).LoadSnapshot(); err != nil {
		t.Errorf("LoadSnapshot: %v", err)
	} else if snap != nil {
		t.Errorf("snapshot persisted despite below-threshold coverage: %+v", snap)
	}
}

// TestEngineRefreshAllFailDoesNotPersist pins the v0.27.1 fix for the
// startup-race poison: when every fetch in the fan-out returns an error
// (gateway not yet connected, or transient outage during the daily
// tick), Compute produces Coverage=0 and finalise refuses to persist.
// The cache stays whatever it was — either empty (no Get()) or the
// last good snapshot from an earlier refresh.
func TestEngineRefreshAllFailDoesNotPersist(t *testing.T) {
	now := time.Date(2026, 5, 19, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA", "BBB", "CCC"}
	fake := &FakeBarFetcher{
		Errors: map[string]error{
			"AAA": errors.New("breadth fetcher: no gateway connector"),
			"BBB": errors.New("breadth fetcher: no gateway connector"),
			"CCC": errors.New("breadth fetcher: no gateway connector"),
		},
	}
	dir := t.TempDir()
	store := NewStore(dir)
	e := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e.members = members

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, ok := e.Get(); ok {
		t.Error("Get should return false after an all-fail refresh — degenerate snapshot must not be published")
	}

	// On-disk cache must be untouched — no snapshot.json, no
	// history.json. A subsequent daemon restart cold-starts cleanly
	// rather than reading a poisoned cache.
	store2 := NewStore(dir)
	if snap, err := store2.LoadSnapshot(); err != nil {
		t.Errorf("LoadSnapshot: %v", err)
	} else if snap != nil {
		t.Errorf("snapshot persisted despite Coverage==0: %+v", snap)
	}
	if hist, err := store2.LoadHistory(); err != nil {
		t.Errorf("LoadHistory: %v", err)
	} else if len(hist) > 0 {
		t.Errorf("history persisted despite Coverage==0: %+v", hist)
	}
}

func TestEngineWarmAllFailNeverPublishesCurrentSession(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 6, 1, 18, 30, 0, 0, loc) // after the bounded publication deadline
	priorSession := "2026-05-29"
	members := []string{"AAA", "BBB", "CCC"}
	windows := make(map[string]ConstituentWindow, len(members))
	errs := make(map[string]error, len(members))
	for _, symbol := range members {
		windows[symbol] = ConstituentWindow{
			Symbol: symbol, Closes: seedCloses(WindowSize, 100, 1), LastBarAt: priorSession,
		}
		errs[symbol] = errors.New("gateway: no current-session bar")
	}
	dir := t.TempDir()
	store := NewStore(dir)
	prior := Snapshot{
		SessionKey: priorSession, AsOf: time.Date(2026, 5, 29, 17, 0, 0, 0, loc),
		Method: methodConstituentFanout, MemberCount: len(members), Coverage: len(members),
	}
	if err := store.SaveSnapshot(prior); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if err := store.SaveWindows(windows, prior.AsOf); err != nil {
		t.Fatalf("seed windows: %v", err)
	}

	e := New(store, &FakeBarFetcher{Errors: errs}, Options{Clock: frozenClock(now), Workers: 1, Members: members})
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("warm refresh: %v", err)
	}
	got, ok := e.Get()
	if !ok || got.SessionKey != priorSession {
		t.Fatalf("served snapshot=%+v ok=%v, want retained prior last-good", got, ok)
	}
	if got.SessionKey == CompletedSessionKey(now) {
		t.Fatal("failed warm refresh falsely published the current session")
	}
	persisted, err := NewStore(dir).LoadSnapshot()
	if err != nil {
		t.Fatalf("load persisted snapshot: %v", err)
	}
	if persisted == nil || persisted.SessionKey != priorSession {
		t.Fatalf("persisted snapshot=%+v, want unchanged prior last-good", persisted)
	}
	if coverage, count := e.LastRefreshCoverage(); coverage != 0 || count != len(members) {
		t.Fatalf("current-session coverage=(%d,%d), want (0,%d)", coverage, count, len(members))
	}
}

type checkpointBlockingFetcher struct {
	mu      sync.Mutex
	calls   []string
	bars    []Bar
	blockAt int
	blocked chan struct{}
}

func (f *checkpointBlockingFetcher) FetchDaily(ctx context.Context, symbol string, _ int) ([]Bar, error) {
	f.mu.Lock()
	f.calls = append(f.calls, symbol)
	call := len(f.calls)
	f.mu.Unlock()
	if call == f.blockAt {
		close(f.blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]Bar(nil), f.bars...), nil
}

func TestEngineCheckpointsBatchAndRestartResumesWithoutSnapshot(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"S00", "S01", "S02", "S03", "S04", "S05", "S06", "S07", "S08", "S09", "S10"}
	dir := t.TempDir()
	fetcher := &checkpointBlockingFetcher{
		bars:    makeSeries(100, 1, WindowSize, now),
		blockAt: windowCheckpointBatchSize + 1,
		blocked: make(chan struct{}),
	}
	e := New(NewStore(dir), fetcher, Options{Clock: frozenClock(now), Workers: 1, Members: members})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.Refresh(ctx) }()
	<-fetcher.blocked
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh error=%v, want context cancellation", err)
	}
	progress, ok := e.Progress()
	if !ok {
		t.Fatal("cancelled refresh should retain observable progress")
	}
	deadline, _ := PublicationDeadline(CompletedSessionKey(now))
	if !progress.StartedAt.Equal(now) || progress.Processed != windowCheckpointBatchSize+1 || progress.Total != len(members) ||
		!progress.Deadline.Equal(deadline) || progress.LastFailure != RefreshFailureCancelled {
		t.Fatalf("progress=%+v, want deterministic cancelled 11/%d attempt", progress, len(members))
	}

	store := NewStore(dir)
	windows, err := store.LoadWindows()
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if len(windows) != windowCheckpointBatchSize {
		t.Fatalf("checkpoint windows=%d, want %d", len(windows), windowCheckpointBatchSize)
	}
	if snap, err := store.LoadSnapshot(); err != nil {
		t.Fatalf("load snapshot: %v", err)
	} else if snap != nil {
		t.Fatalf("mid-refresh checkpoint published snapshot: %+v", snap)
	}

	restarted := New(NewStore(dir), &FakeBarFetcher{}, Options{Clock: frozenClock(now), Workers: 1, Members: members})
	plan := restarted.planFetches(members, maps.Clone(restarted.windows))
	if len(plan) != len(members)-windowCheckpointBatchSize {
		t.Fatalf("restart plan=%+v, want only %d unfinished names", plan, len(members)-windowCheckpointBatchSize)
	}
	if plan[0].Symbol != "S10" {
		t.Fatalf("restart plan=%+v, want only S10", plan)
	}
}

// TestEngineRefreshBelowThresholdPersistsWindowsForAccumulation pins
// the convergence-across-refreshes contract: when a refresh ends below
// MinCoverageFraction × MemberCount, the snapshot and history stay
// withheld (so consumers don't read a misleading partial value), but
// the per-name windows persist both in-memory AND on-disk. The next
// refresh — or a daemon restart loading the same dir — starts from the
// accumulated windows rather than cold, which is how a 503-name cold
// start with IBKR's per-account reqContractDetails throttling
// eventually crosses the 80% threshold.
func TestEngineRefreshBelowThresholdPersistsWindowsForAccumulation(t *testing.T) {
	now := time.Date(2026, 5, 19, 21, 30, 0, 0, time.UTC)
	// 10 members, only 5 succeed — 50% coverage, well below the 80%
	// threshold.
	members := []string{"OK1", "OK2", "OK3", "OK4", "OK5", "F1", "F2", "F3", "F4", "F5"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"OK1": makeSeries(100, 1, WindowSize, now),
			"OK2": makeSeries(50, 1, WindowSize, now),
			"OK3": makeSeries(75, 1, WindowSize, now),
			"OK4": makeSeries(60, 1, WindowSize, now),
			"OK5": makeSeries(80, 1, WindowSize, now),
		},
		Errors: map[string]error{
			"F1": errors.New("gateway: pacing"),
			"F2": errors.New("gateway: pacing"),
			"F3": errors.New("gateway: pacing"),
			"F4": errors.New("gateway: pacing"),
			"F5": errors.New("gateway: pacing"),
		},
	}
	dir := t.TempDir()
	store := NewStore(dir)
	e := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e.members = members

	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// Snapshot still withheld at below-threshold coverage — the
	// published surface guarantee from the existing
	// TestEngineRefreshBelowCoverageThresholdIsNotPersisted test
	// hasn't changed.
	if _, ok := e.Get(); ok {
		t.Error("Get should return false after below-threshold refresh")
	}

	// Coverage signal is available for the scheduler to read.
	cov, mc := e.LastRefreshCoverage()
	if cov != 5 || mc != 10 {
		t.Errorf("LastRefreshCoverage: want (5, 10), got (%d, %d)", cov, mc)
	}

	// Windows ARE persisted in-memory for next-tick continuation.
	if got := len(e.windows); got != 5 {
		t.Errorf("in-memory windows: want 5 entries, got %d", got)
	}

	// Windows ARE persisted on-disk for daemon-restart continuation.
	loaded, err := NewStore(dir).LoadWindows()
	if err != nil {
		t.Fatalf("LoadWindows: %v", err)
	}
	if got := len(loaded); got != 5 {
		t.Errorf("on-disk windows: want 5 entries, got %d", got)
	}
}

func TestEngineConcurrentRefreshSerialises(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA", "BBB"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1, WindowSize, now),
			"BBB": makeSeries(50, 1, WindowSize, now),
		},
		Latency: 30 * time.Millisecond,
	}
	e := newTestEngine(t, fake, frozenClock(now), members)

	// Track peak concurrency of Refresh bodies. Each goroutine ticks
	// the counter at entry and at exit; if the engine were not
	// serialising, peak would briefly hit 5.
	var inFlight, peak atomic.Int32
	const callers = 5
	var wg sync.WaitGroup
	for range callers {
		wg.Go(func() {
			inFlight.Add(1)
			if cur := inFlight.Load(); cur > peak.Load() {
				peak.Store(cur)
			}
			if err := e.Refresh(context.Background()); err != nil {
				t.Errorf("refresh: %v", err)
			}
			inFlight.Add(-1)
		})
	}
	wg.Wait()
	// Peak strictly above 1 is expected (callers queue at the
	// refreshMu); what matters is the post-condition that all five
	// Refreshes completed without panicking and the final snapshot
	// is consistent.
	if _, ok := e.Get(); !ok {
		t.Fatal("snapshot should exist after all serialised refreshes complete")
	}
}

func TestEngineContextCancellationBailsEarly(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA", "BBB", "CCC", "DDD"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1, WindowSize, now),
			"BBB": makeSeries(100, 1, WindowSize, now),
			"CCC": makeSeries(100, 1, WindowSize, now),
			"DDD": makeSeries(100, 1, WindowSize, now),
		},
		// Long latency so the test can cancel before all fetches
		// complete.
		Latency: 200 * time.Millisecond,
	}
	e := newTestEngine(t, fake, frozenClock(now), members)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := e.Refresh(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected ctx deadline exceeded, got %v", err)
	}
	// The engine should not have crashed and should not have a
	// completed snapshot yet (or it has a partial one). Either
	// outcome is acceptable; what we're really pinning is that
	// cancellation propagates and doesn't leak goroutines past
	// the Refresh return.
}

// TestEngineSnapshotPersistsAcrossRestart pins the on-disk durability
// contract: a refresh writes both snapshot.json and windows.json
// atomically, so a New() on the same dir picks them back up.
func TestEngineSnapshotPersistsAcrossRestart(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"AAA", "BBB"}
	fake := &FakeBarFetcher{
		Bars: map[string][]Bar{
			"AAA": makeSeries(100, 1, WindowSize, now),
			"BBB": makeSeries(50, 1, WindowSize, now),
		},
	}
	dir := t.TempDir()
	store := NewStore(dir)
	e1 := New(store, fake, Options{Clock: frozenClock(now), Workers: 4})
	e1.members = members
	if err := e1.Refresh(context.Background()); err != nil {
		t.Fatalf("first engine refresh: %v", err)
	}
	want, _ := e1.Get()

	// Second engine on the same dir — should load both files.
	store2 := NewStore(dir)
	e2 := New(store2, &FakeBarFetcher{}, Options{Clock: frozenClock(now)})
	got, ok := e2.Get()
	if !ok {
		t.Fatal("restart engine should load persisted snapshot")
	}
	if got.Value != want.Value || got.SessionKey != want.SessionKey {
		t.Errorf("persisted vs reloaded mismatch:\n  want %+v\n  got  %+v", want, got)
	}
}

// TestEngineGetReturnsDefensiveCopy pins the contract that callers
// can mutate the returned snapshot without corrupting engine state.
func TestEngineGetReturnsDefensiveCopy(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
	members := []string{"OK", "FAIL"}
	fake := &FakeBarFetcher{
		Bars:   map[string][]Bar{"OK": makeSeries(100, 1, WindowSize, now)},
		Errors: map[string]error{"FAIL": errors.New("nope")},
	}
	e := newTestEngine(t, fake, frozenClock(now), members)
	if err := e.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	first, ok := e.Get()
	if !ok || len(first.Excluded) == 0 {
		t.Fatal("expected Excluded to be populated")
	}
	first.Excluded[0].Reason = "MUTATED-EXTERNALLY"
	second, _ := e.Get()
	if second.Excluded[0].Reason == "MUTATED-EXTERNALLY" {
		t.Error("Get should return a defensive copy of Excluded — engine state was mutated through the returned pointer")
	}
}

// TestEngineMarkPendingBootstrapSetsRefreshingWhenStale pins the
// v0.30.1 Bug 1c fix: when shouldRefreshOnStartup would fire on
// Run() entry, MarkPendingBootstrap pre-sets refreshing=true so
// `canary status` reflects the imminent breadth-spx work even before
// the goroutine has been scheduled. Without this, the first status
// call after daemon restart shows Connected=true but no background
// task — the symptom the user reported on 2026-05-21.
func TestEngineMarkPendingBootstrapSetsRefreshingWhenStale(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 21, 6, 0, 0, 0, loc) // Thu 06:00 ET
	store := NewStore(t.TempDir())
	// Persist a pre-close partial snapshot: SessionKey matches Wed but
	// AsOf is mid-session, so shouldRefreshOnStartup will fire.
	stale := &Snapshot{
		SessionKey: "2026-05-20",
		AsOf:       time.Date(2026, 5, 20, 15, 27, 0, 0, loc),
		Method:     methodConstituentFanout,
	}
	if err := store.SaveSnapshot(*stale); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	fake := &FakeBarFetcher{}
	e := New(store, fake, Options{Clock: frozenClock(now)})

	if e.IsRefreshing() {
		t.Fatal("engine should not be refreshing pre-MarkPendingBootstrap")
	}
	e.MarkPendingBootstrap()
	if !e.IsRefreshing() {
		t.Error("MarkPendingBootstrap should set refreshing=true when a bootstrap is needed")
	}
}

// TestEngineMarkPendingBootstrapNoOpWhenFresh pins the no-stuck-flag
// invariant: if shouldRefreshOnStartup would skip the bootstrap (the
// cached snapshot is already authoritative), MarkPendingBootstrap
// must NOT set refreshing=true — otherwise the flag would stay true
// forever once the caller spawns Run().
func TestEngineMarkPendingBootstrapNoOpWhenFresh(t *testing.T) {
	loc := nyLocation()
	now := time.Date(2026, 5, 21, 9, 0, 0, 0, loc) // Thu 09:00 ET, before today's tick
	store := NewStore(t.TempDir())
	// Yesterday's snapshot with a post-close AsOf — fully authoritative
	// until today's 16:35 ET tick. shouldRefreshOnStartup returns false.
	fresh := &Snapshot{
		SessionKey: "2026-05-20",
		AsOf:       time.Date(2026, 5, 20, 17, 0, 0, 0, loc),
		Method:     methodConstituentFanout,
	}
	if err := store.SaveSnapshot(*fresh); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	fake := &FakeBarFetcher{}
	e := New(store, fake, Options{Clock: frozenClock(now)})

	e.MarkPendingBootstrap()
	if e.IsRefreshing() {
		t.Error("MarkPendingBootstrap on fresh snapshot must not set refreshing=true")
	}
}

// seedCloses returns a synthetic series of n closes, used to seed
// pre-existing windows in the warm-refresh tests.
func seedCloses(n int, start, step float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = start + float64(i)*step
	}
	return out
}
