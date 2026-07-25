package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestRegimeSnapshotCacheRefreshesAheadWithoutChangingHardCeiling(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	refresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(clock.Now(), "second"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}

	clock.Set(firstCommit.Add(39 * time.Second))
	if cache.startRefreshAhead(refresh, 20*time.Second) {
		t.Fatal("refresh started before the ahead window")
	}
	clock.Set(firstCommit.Add(40 * time.Second))
	if !cache.startRefreshAhead(refresh, 20*time.Second) {
		t.Fatal("refresh did not start at the ahead boundary")
	}
	<-started
	beforeDeadline, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if beforeDeadline.Health.Status != rpc.RegimeAuthorityFresh || !beforeDeadline.Health.Refreshing {
		t.Fatalf("early refresh health=%+v, want fresh and refreshing", beforeDeadline.Health)
	}

	clock.Set(firstCommit.Add(time.Minute))
	atDeadline, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if atDeadline.Health.Status != rpc.RegimeAuthorityStale || !atDeadline.Health.Refreshing {
		t.Fatalf("hard ceiling health=%+v, want stale and refreshing", atDeadline.Health)
	}

	close(release)
	cache.wait()
	cache.mu.Lock()
	secondCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(secondCommit)
	recovered, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || recovered.Revision != 2 || recovered.Health.Status != rpc.RegimeAuthorityFresh || recovered.Snapshot.Summary.Label != "second" {
		t.Fatalf("recovered view=%+v calls=%d", recovered, calls.Load())
	}
}

func TestRegimeSnapshotCacheEarlyFailureRetriesWithinFreshnessWindow(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor:          regimeSnapshotFreshFor,
		RefreshTimeout:    regimeSnapshotRefreshTimeout,
		FailureRetryAfter: regimeSnapshotFailureRetry,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()

	firstAttempt := firstCommit.Add(regimeSnapshotFreshFor - regimeSnapshotRefreshAhead)
	clock.Set(firstAttempt)
	if !cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return nil, false, nil, errors.New("temporary source failure")
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("early failure attempt did not start")
	}
	warmFailed := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailureRefreshFailed
	})
	if warmFailed.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("early failure prematurely made authority stale: %+v", warmFailed.Health)
	}

	clock.Set(firstAttempt.Add(regimeSnapshotFailureRetry - time.Nanosecond))
	if cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "too early"), true, nil, nil
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("failed refresh retried before the bounded quiet period")
	}

	clock.Set(firstAttempt.Add(regimeSnapshotFailureRetry))
	if !cache.startRefreshAhead(func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "recovered"), true, nil, nil
	}, regimeSnapshotRefreshAhead) {
		t.Fatal("failed refresh did not retry while last-good was still fresh")
	}
	cache.wait()
	cache.mu.Lock()
	secondCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(secondCommit)
	recovered, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Revision != 2 || recovered.Health.Status != rpc.RegimeAuthorityFresh || recovered.Snapshot.Summary.Label != "recovered" {
		t.Fatalf("early retry did not recover authority: %+v", recovered)
	}
}

func TestRegimeRefreshLoopRunsWithoutAlertOrApp(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "first"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	firstCommit := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(firstCommit.Add(40 * time.Second))

	loopContext, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer close(loopDone)
		runRegimeRefreshLoop(
			loopContext,
			cache,
			time.Hour,
			20*time.Second,
			nil,
			func() bool { return true },
			func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
				close(started)
				select {
				case <-release:
					return regimeSnapshotCacheFixture(clock.Now(), "scheduled"), true, nil, nil
				case <-ctx.Done():
					return nil, false, nil, ctx.Err()
				}
			},
		)
	}()

	<-started
	close(release)
	cache.wait()
	cancelLoop()
	<-loopDone
	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 2 || view.Snapshot == nil || view.Snapshot.Summary.Label != "scheduled" {
		t.Fatalf("scheduler did not publish without alert/app owners: %+v", view)
	}
}

func TestSuccessfulGammaPublicationRefreshesRegimeAndWakesConsumers(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	server := &Server{regimeSnapshots: cache, zeroGamma: newGammaZeroCache()}
	server.zeroGamma.setPublicationCallback(server.handleGammaPublication)
	stressWake := server.stressEvaluationWakeChannel()
	rulebookWake := server.rulebookRefreshWakeChannel()
	afterPublish := func(_ context.Context, publication regimeSnapshotPublication) error {
		server.publishRulesRegimeStageState(regimeDependencyTestStage(publication), publication)
		return nil
	}
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "gamma unavailable"), true, afterPublish, nil
	}); err != nil {
		t.Fatal(err)
	}
	assertRegimeDependencyWake(t, "initial Canary", stressWake)
	assertRegimeDependencyWake(t, "initial Rulebook", rulebookWake)

	loopContext, cancelLoop := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	readyChecked := make(chan struct{}, 1)
	var refreshCalls atomic.Int32
	go func() {
		defer close(loopDone)
		runRegimeRefreshLoop(
			loopContext,
			cache,
			time.Hour,
			20*time.Second,
			server.regimeRefreshWakeChannel(),
			func() bool {
				select {
				case readyChecked <- struct{}{}:
				default:
				}
				return true
			},
			func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
				refreshCalls.Add(1)
				envelope := server.zeroGamma.snapshotCurrent(rpc.GammaZeroScopeCombined, clock.Now)
				label := "gamma unavailable"
				if envelope.Status == rpc.GammaZeroStatusReady && envelope.Result != nil {
					label = "gamma recovered"
				}
				return regimeSnapshotCacheFixture(clock.Now(), label), true, afterPublish, nil
			},
		)
	}()
	<-readyChecked

	failed := server.zeroGamma.force(daemonContext, rpc.GammaZeroScopeCombined, clock.Now(), 0,
		func(context.Context, *atomic.Int32) (*rpc.GammaZeroComputed, error) {
			return nil, errors.New("temporary gamma failure")
		})
	<-failed.done
	time.Sleep(20 * time.Millisecond)
	stable, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls.Load() != 0 || stable.Revision != 1 || stable.Snapshot.Summary.Label != "gamma unavailable" {
		t.Fatalf("failed Gamma changed Regime authority: calls=%d view=%+v", refreshCalls.Load(), stable)
	}
	assertNoRegimeDependencyWake(t, "failed Gamma Canary", stressWake)
	assertNoRegimeDependencyWake(t, "failed Gamma Rulebook", rulebookWake)

	recoveredGamma := helperGammaResult(clock.Now())
	recoveredGamma.Scope = rpc.GammaZeroScopeCombined
	succeeded := server.zeroGamma.force(daemonContext, rpc.GammaZeroScopeCombined, clock.Now(), 0,
		func(context.Context, *atomic.Int32) (*rpc.GammaZeroComputed, error) {
			return recoveredGamma, nil
		})
	<-succeeded.done
	recovered := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return view.Revision == 2 && view.Snapshot != nil && view.Snapshot.Summary.Label == "gamma recovered"
	})
	if refreshCalls.Load() != 1 || recovered.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("Gamma recovery refresh calls=%d view=%+v", refreshCalls.Load(), recovered)
	}
	assertRegimeDependencyWake(t, "recovered Gamma Canary", stressWake)
	assertRegimeDependencyWake(t, "recovered Gamma Rulebook", rulebookWake)

	cancelLoop()
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("Regime dependency wake loop did not stop")
	}
}
