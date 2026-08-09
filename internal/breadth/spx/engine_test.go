package spx

import (
	"context"
	"errors"
	"maps"
	"sync"

	"testing"
	"time"
)

func makeSeries(start, step float64, n int, anchor time.Time) []Bar {
	bars := make([]Bar, n)
	for i := range n {
		date := anchor.AddDate(0, 0, -(n - 1 - i))
		bars[i] = Bar{Date: date.Format("2006-01-02"), Close: start + float64(i)*step}
	}
	return bars
}

func newTestEngine(t *testing.T, fetcher *FakeBarFetcher, clock func() time.Time, members []string) *Engine {
	t.Helper()
	store := NewStore(t.TempDir())
	e := New(store, fetcher, Options{
		Clock:   clock,
		Workers: 4,
	})

	e.members = members
	return e
}

func frozenClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func TestEngineColdStartFetchesEveryName(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)
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

func TestEngineTolerantOfPerSymbolErrors(t *testing.T) {
	now := time.Date(2026, 5, 18, 21, 30, 0, 0, time.UTC)

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

func TestEngineRefreshBelowCoverageThresholdIsNotPersisted(t *testing.T) {
	now := time.Date(2026, 5, 19, 21, 30, 0, 0, time.UTC)

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

	if snap, err := NewStore(dir).LoadSnapshot(); err != nil {
		t.Errorf("LoadSnapshot: %v", err)
	} else if snap != nil {
		t.Errorf("snapshot persisted despite below-threshold coverage: %+v", snap)
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
