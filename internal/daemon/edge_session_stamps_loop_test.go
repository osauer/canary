package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// stampLoopSnapshot is a complete Regime snapshot computed at the given UTC
// time with the given stage, in the shape the cache persists.
func stampLoopSnapshot(t *testing.T, at, stage string) *rpc.RegimeSnapshotResult {
	t.Helper()
	asOf, err := time.Parse("2006-01-02 15:04:05", at)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := regimeSnapshotCacheFixture(asOf, stage)
	snapshot.Lifecycle.Stage = stage
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	return snapshot
}

// stampLoopFeed stands in for the market: a refresh publishes the snapshot
// the test queued, and fails when none is queued.
type stampLoopFeed struct {
	mu   sync.Mutex
	next *rpc.RegimeSnapshotResult
	// failProjections makes the next snapshot commit and then fail the
	// projections Regime derives from it.
	failProjections bool
}

func (f *stampLoopFeed) refresh(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.next == nil {
		return nil, false, nil, errors.New("no new snapshot")
	}
	snapshot := f.next
	f.next = nil
	if f.failProjections {
		f.failProjections = false
		return snapshot, true, func(context.Context, regimeSnapshotPublication) error { return errors.New("projection failed") }, nil
	}
	return snapshot, true, nil, nil
}

// stampLoopDaemon is a daemon with its real Regime cache on the store at
// path. The cache runs on the wall clock, as the store's commit times do;
// what a record says comes from the snapshots' own times and the daemon's
// fixed clock, so no assertion depends on today's date. That clock stands at
// 17:00 New York on Wednesday 25 November 2026, an hour after the close.
func stampLoopDaemon(t *testing.T, path string) *Server {
	t.Helper()
	return stampLoopDaemonAt(t, path, time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC))
}

func stampLoopDaemonAt(t *testing.T, path string, now time.Time) *Server {
	t.Helper()
	s := stampTestServer(t, path, &now)
	daemonContext, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, s.coreStore, regimeSnapshotCacheOptions{
		FreshFor: time.Hour, RefreshTimeout: 5 * time.Second, FailureRetryAfter: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.regimeSnapshots = cache
	return s
}

// runStampLoop runs the loop the daemon starts, on a short tick, and returns
// a function that stops it and waits for it.
func runStampLoop(t *testing.T, s *Server, ready func() bool, feed *stampLoopFeed) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.regimeRefreshLoop(ctx, time.Millisecond, ready, feed.refresh)
	}()
	stop = sync.OnceFunc(func() { cancel(); <-done; s.regimeSnapshots.wait() })
	t.Cleanup(stop)
	return stop
}

// publishToLoop hands the loop one new snapshot, as a dependency publication
// does in the daemon, and waits until the cache holds it and the loop has
// run at least one full tick on it.
func publishToLoop(t *testing.T, s *Server, feed *stampLoopFeed, snapshot *rpc.RegimeSnapshotResult) int64 {
	t.Helper()
	before, _ := s.regimeSnapshots.held()
	feed.mu.Lock()
	feed.next = snapshot
	feed.mu.Unlock()
	s.regimeSnapshots.invalidateForDependencyPublication()
	waitForStampLoop(t, "the cache to hold the new snapshot", func() bool {
		revision, _ := s.regimeSnapshots.held()
		return revision > before
	})
	revision, _ := s.regimeSnapshots.held()
	settleStampLoop(t, s, revision)
	return revision
}

// settleStampLoop waits until the loop's own step has looked at revision.
func settleStampLoop(t *testing.T, s *Server, revision int64) {
	t.Helper()
	waitForStampLoop(t, "the loop to look at the held snapshot", func() bool {
		return s.edgeSessionStampLooked.Load() >= revision
	})
}

func waitForStampLoop(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// The daemon writes a session's record itself. Nothing reads Regime in this
// test: no request, no handler. The loop the daemon runs publishes a snapshot
// before Wednesday's close, one after it and a later one with another stage;
// the session's record is the first snapshot after the close, written once.
// A restart on the same store, holding a snapshot with yet another stage,
// changes nothing.
func TestEdgeSessionStampIsWrittenWithoutAReader(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	s := stampLoopDaemon(t, path)
	feed := &stampLoopFeed{}
	stop := runStampLoop(t, s, func() bool { return true }, feed)

	publishToLoop(t, s, feed, stampLoopSnapshot(t, "2026-11-25 20:59:30", rpc.LifecycleEarlyWarning))
	if got := stampLog(t, s); len(got) != 0 {
		t.Fatalf("a snapshot computed before the close was recorded: %+v", got)
	}
	afterClose := publishToLoop(t, s, feed, stampLoopSnapshot(t, "2026-11-25 21:00:30", rpc.LifecycleQuiet))
	got := stampLog(t, s)
	if len(got) != 1 || got[0].Session != "2026-11-25" || got[0].RegimeStage != rpc.LifecycleQuiet || got[0].SnapshotRevision != afterClose ||
		!got[0].ServedAt.Equal(time.Date(2026, 11, 25, 21, 0, 30, 0, time.UTC)) {
		t.Fatalf("with no Regime read after the close the log is %+v, want one record: 2026-11-25 quiet from revision %d", got, afterClose)
	}
	publishToLoop(t, s, feed, stampLoopSnapshot(t, "2026-11-25 21:06:00", rpc.LifecyclePanic))
	if again := stampLog(t, s); len(again) != 1 || !reflect.DeepEqual(again[0], got[0]) {
		t.Fatalf("a later snapshot of the same evening changed the record: %+v, was %+v", again, got)
	}

	stop()
	if err := s.coreStore.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := stampLoopDaemon(t, path)
	held, _ := restarted.regimeSnapshots.held()
	runStampLoop(t, restarted, func() bool { return false }, feed)
	settleStampLoop(t, restarted, held)
	if again := stampLog(t, restarted); len(again) != 1 || !reflect.DeepEqual(again[0], got[0]) {
		t.Fatalf("a restart changed the record: %+v, was %+v", again, got)
	}
}

// stampLoopDaemonHolding is a daemon started at now on a store that already
// holds one Regime snapshot, with its loop running and no gateway: what a
// restart finds. It returns once the loop has looked at that snapshot.
func stampLoopDaemonHolding(t *testing.T, snapshot *rpc.RegimeSnapshotResult, now time.Time) *Server {
	t.Helper()
	path := filepath.Join(privateTestDir(t), "daemon.db")
	first := stampLoopDaemonAt(t, path, now)
	raw, _, err := encodeRegimeSnapshotDocument(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	if err := first.coreStore.Close(); err != nil {
		t.Fatal(err)
	}
	s := stampLoopDaemonAt(t, path, now)
	held, _ := s.regimeSnapshots.held()
	if held != 1 {
		t.Fatalf("held revision=%d, want the stored snapshot", held)
	}
	runStampLoop(t, s, func() bool { return false }, &stampLoopFeed{})
	settleStampLoop(t, s, held)
	return s
}

// A daemon that starts holding a snapshot from after the close, with no
// gateway to refresh it, still writes the session's record: the write needs
// the snapshot the daemon holds, not a connection and not a reader.
func TestEdgeSessionStampNeedsNoGateway(t *testing.T) {
	s := stampLoopDaemonHolding(t, stampLoopSnapshot(t, "2026-11-25 21:02:00", rpc.LifecycleConfirmedStress), time.Date(2026, 11, 25, 22, 0, 0, 0, time.UTC))
	if got := stampLog(t, s); len(got) != 1 || got[0].Session != "2026-11-25" || got[0].RegimeStage != rpc.LifecycleConfirmedStress {
		t.Fatalf("log=%+v, want the held snapshot recorded for 2026-11-25", got)
	}
}

// A snapshot whose own projections are still owed is not what Regime serves,
// so it is not recorded either; once they are repaired it is.
func TestEdgeSessionStampWaitsForRegimeProjections(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	s := stampLoopDaemon(t, path)
	feed := &stampLoopFeed{}
	runStampLoop(t, s, func() bool { return true }, feed)
	before := publishToLoop(t, s, feed, stampLoopSnapshot(t, "2026-11-25 20:59:30", rpc.LifecycleQuiet))

	// The next snapshot commits but its projections fail: the cache holds it
	// as pending.
	feed.mu.Lock()
	feed.next = stampLoopSnapshot(t, "2026-11-25 21:00:30", rpc.LifecycleEarlyWarning)
	feed.failProjections = true
	feed.mu.Unlock()
	s.regimeSnapshots.invalidateForDependencyPublication()
	waitForStampLoop(t, "the pending snapshot", func() bool {
		pending, _ := s.regimeSnapshots.projectionFailure()
		return pending
	})
	_, revision := s.regimeSnapshots.projectionFailure()
	if revision <= before {
		t.Fatalf("pending revision=%d, want one after %d", revision, before)
	}
	time.Sleep(20 * time.Millisecond)
	if got := stampLog(t, s); len(got) != 0 {
		t.Fatalf("a snapshot with projections owed was recorded: %+v", got)
	}
	if err := s.regimeSnapshots.markProjectionRepaired(revision); err != nil {
		t.Fatal(err)
	}
	settleStampLoop(t, s, revision)
	if got := stampLog(t, s); len(got) != 1 || got[0].RegimeStage != rpc.LifecycleEarlyWarning || got[0].SnapshotRevision != revision {
		t.Fatalf("after the repair the log is %+v, want the repaired snapshot recorded", got)
	}
}

// A daemon stopped in the evening and started the next morning still holds
// the evening's snapshot. It writes no record from it: the premium line is
// read when the record is written, and the next day's policy is not that
// evening's. The session stays without a record.
func TestEdgeSessionStampIsNotWrittenTheNextDay(t *testing.T) {
	s := stampLoopDaemonHolding(t, stampLoopSnapshot(t, "2026-11-25 21:02:00", rpc.LifecycleConfirmedStress), time.Date(2026, 11, 26, 14, 0, 0, 0, time.UTC))
	if got := stampLog(t, s); len(got) != 0 {
		t.Fatalf("the next day's daemon recorded the evening before: %+v", got)
	}
}
