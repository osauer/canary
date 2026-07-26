package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

type regimeSnapshotTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *regimeSnapshotTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *regimeSnapshotTestClock) Set(now time.Time) {
	clock.mu.Lock()
	clock.now = now
	clock.mu.Unlock()
}

type regimeSnapshotFaultStore struct {
	base                 *corestore.Store
	fail                 atomic.Bool
	returnPreparedOnFail atomic.Bool
	commitThenFail       atomic.Bool
	beforeCAS            func(corestore.StateDocumentCAS) corestore.StateDocumentCAS
}

func regimeSnapshotTestNow() time.Time {
	// corestore timestamps commits with the wall clock. Keep the injected cache
	// clock just ahead of that timestamp so freshness tests are deterministic
	// without replacing daemon.db's authoritative UpdatedAt.
	return time.Now().UTC().Add(time.Second)
}

func (store *regimeSnapshotFaultStore) GetStateDocument(ctx context.Context, scope, kind string) (corestore.StateDocument, bool, error) {
	return store.base.GetStateDocument(ctx, scope, kind)
}

func (store *regimeSnapshotFaultStore) CompareAndSwapStateDocument(ctx context.Context, update corestore.StateDocumentCAS) (corestore.StateDocument, error) {
	if store.beforeCAS != nil {
		update = store.beforeCAS(update)
	}
	if store.commitThenFail.Load() {
		saved, err := store.base.CompareAndSwapStateDocument(ctx, update)
		if err != nil {
			return saved, err
		}
		return saved, errors.New("injected authority-head failure after durable commit")
	}
	if store.fail.Load() {
		if store.returnPreparedOnFail.Load() {
			return corestore.StateDocument{
				ScopeKey: update.ScopeKey, Kind: update.Kind, Revision: update.ExpectedRevision + 1,
				JSON: append([]byte(nil), update.JSON...), UpdatedAt: time.Now().UTC(),
			}, errors.New("injected rollback after preparing authoritative state")
		}
		return corestore.StateDocument{}, errors.New("injected authoritative publish failure")
	}
	return store.base.CompareAndSwapStateDocument(ctx, update)
}

func openRegimeSnapshotTestStore(t *testing.T) *corestore.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open daemon SQLite authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newRegimeSnapshotTestCache(
	t *testing.T,
	store regimeSnapshotStateStore,
	daemonContext context.Context,
	clock *regimeSnapshotTestClock,
) *regimeSnapshotCache {
	t.Helper()
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor:          time.Minute,
		RefreshTimeout:    5 * time.Second,
		FailureRetryAfter: 5 * time.Minute,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatalf("load regime snapshot cache: %v", err)
	}
	return cache
}

func regimeSnapshotCacheFixture(at time.Time, label string) *rpc.RegimeSnapshotResult {
	snapshot := &rpc.RegimeSnapshotResult{
		AsOf: at.UTC(),
		Summary: rpc.RegimeSummary{
			Label:         label,
			DominantRisks: []string{"credit", "volatility"},
		},
		VIXTermStructure: rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK},
		VolOfVol:         rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusOK},
		CreditSpreads:    rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK},
		FundingStress:    rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK},
		USDJPY:           rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Symbol: "USD.JPY"},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: "ready",
				Result: &rpc.GammaZeroComputed{
					Scope: "combined",
					LegDiagnostics: &rpc.GammaLegDiagnostics{
						ByUnderlying: map[string]rpc.GammaLegDiagnosticCounts{
							"SPX": {PricedLegs: 10, OpenInterestLegs: 8},
						},
					},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{Status: rpc.RegimeStatusOK},
		WarningDetails: []rpc.RegimeWarning{
			{Code: "fixture", Scope: "test", Severity: "info", Message: "fixture warning"},
		},
		SpecDoc: "https://osauer.dev/canary/docs/internals/regime-dashboard.html",
	}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	return snapshot
}

func waitForRegimeSnapshotCache(t *testing.T, cache *regimeSnapshotCache, condition func(regimeSnapshotCacheView) bool) regimeSnapshotCacheView {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view, err := cache.current()
		if err != nil {
			t.Fatalf("read cache: %v", err)
		}
		if condition(view) {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	view, _ := cache.current()
	t.Fatalf("cache condition not reached: health=%+v revision=%d", view.Health, view.Revision)
	return regimeSnapshotCacheView{}
}

func TestRegimeSnapshotCacheColdCallersJoinOneRefresh(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var publishCallbacks atomic.Int32
	refresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return regimeSnapshotCacheFixture(now, "cold-published"), true, func(context.Context, regimeSnapshotPublication) error {
				publishCallbacks.Add(1)
				return nil
			}, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}

	const callers = 12
	results := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			view, err := cache.serve(ctx, refresh)
			if err == nil && publishCallbacks.Load() != 1 {
				err = errors.New("cold joiner released before after-publish callback")
			}
			if err == nil && (view.Snapshot == nil || view.Snapshot.Summary.Label != "cold-published") {
				err = errors.New("joined caller did not receive published snapshot")
			}
			results <- err
		}()
	}
	<-started
	close(release)
	for range callers {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls=%d, want one", got)
	}
	if got := publishCallbacks.Load(); got != 1 {
		t.Fatalf("after-publish callbacks=%d, want one", got)
	}

	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 || view.Health.Status != rpc.RegimeAuthorityFresh || view.Health.Refreshing {
		t.Fatalf("published view=%+v health=%+v", view, view.Health)
	}
	if view.Snapshot.AuthorityHealth == nil || *view.Snapshot.AuthorityHealth != view.Health {
		t.Fatalf("snapshot authority health=%+v, want %+v", view.Snapshot.AuthorityHealth, view.Health)
	}
	if err := rpc.ValidateRegimeAuthorityHealth(view.Health); err != nil {
		t.Fatalf("invalid health: %v", err)
	}
	document, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok || document.Revision != 1 {
		t.Fatalf("persisted state ok=%v revision=%d err=%v", ok, document.Revision, err)
	}
	if bytes.Contains(document.JSON, []byte("authority_health")) {
		t.Fatalf("response-only authority health persisted: %s", document.JSON)
	}
}

func TestRegimeSnapshotCacheDeepCopiesIngressAndEveryEgress(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	original := regimeSnapshotCacheFixture(now, "immutable")

	first, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return original, true, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	original.Summary.DominantRisks[0] = "mutated-ingress"
	original.GammaZero.Envelope.Result.LegDiagnostics.ByUnderlying["SPX"] = rpc.GammaLegDiagnosticCounts{PricedLegs: 999}
	first.Snapshot.Summary.DominantRisks[1] = "mutated-egress"
	first.Snapshot.WarningDetails[0].Code = "mutated"
	first.Snapshot.GammaZero.Envelope.Result.LegDiagnostics.ByUnderlying["SPX"] = rpc.GammaLegDiagnosticCounts{PricedLegs: 777}

	second, err := cache.serve(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Snapshot.Summary.DominantRisks; len(got) != 2 || got[0] != "credit" || got[1] != "volatility" {
		t.Fatalf("cached nested slice aliased: %v", got)
	}
	if got := second.Snapshot.WarningDetails[0].Code; got != "fixture" {
		t.Fatalf("cached warning aliased: %q", got)
	}
	if got := second.Snapshot.GammaZero.Envelope.Result.LegDiagnostics.ByUnderlying["SPX"].PricedLegs; got != 10 {
		t.Fatalf("cached nested map aliased: %d", got)
	}
	if second.Fingerprint != rpc.BuildRegimeFingerprint(second.Snapshot) {
		t.Fatalf("fingerprint changed across deep-copy serve")
	}
}

func TestRegimeSnapshotCacheWarmStaleServesImmediatelyAndRefreshOutlivesCaller(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	firstAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: firstAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	var calls atomic.Int32
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		calls.Add(1)
		return regimeSnapshotCacheFixture(firstAt, "last-good"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	staleAt := firstAt.Add(2 * time.Minute)
	clock.Set(staleAt)
	started := make(chan struct{})
	release := make(chan struct{})
	refresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		if calls.Add(1) == 2 {
			close(started)
		}
		select {
		case <-release:
			return regimeSnapshotCacheFixture(staleAt, "refreshed"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}
	callerContext, cancelCaller := context.WithCancel(context.Background())
	stale, err := cache.serve(callerContext, refresh)
	if err != nil {
		t.Fatal(err)
	}
	if stale.Snapshot.Summary.Label != "last-good" || stale.Health.Status != rpc.RegimeAuthorityStale || !stale.Health.Refreshing {
		t.Fatalf("stale serve=%q health=%+v", stale.Snapshot.Summary.Label, stale.Health)
	}
	<-started
	cancelCaller()
	second, err := cache.serve(context.Background(), refresh)
	if err != nil || second.Snapshot.Summary.Label != "last-good" || !second.Health.Refreshing {
		t.Fatalf("second stale serve=%q health=%+v err=%v", second.Snapshot.Summary.Label, second.Health, err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("concurrent stale refresh calls=%d, want one refresh behind seed", got)
	}
	close(release)
	// Publication identity comes from SQLite's exact commit timestamp, not the
	// synthetic clock used to age the first revision stale. Drain publication
	// before advancing the injected clock past that exact commit so scheduling
	// cannot manufacture a test-only clock rollback.
	cache.wait()
	cache.mu.Lock()
	committedAt := cache.lastSuccessAt
	cache.mu.Unlock()
	clock.Set(committedAt.Add(time.Second))
	refreshed, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Revision != 2 || refreshed.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("refreshed revision=%d health=%+v", refreshed.Revision, refreshed.Health)
	}
}

func TestRegimeSnapshotCacheColdCallerTimeoutDoesNotCancelDaemonRefresh(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	release := make(chan struct{})
	started := make(chan struct{})
	refresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(now, "landed-after-timeout"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}
	callerContext, cancelCaller := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelCaller()
	_, err := cache.serve(callerContext, refresh)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cold caller error=%v, want deadline exceeded", err)
	}
	<-started
	close(release)
	view := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return view.Snapshot != nil && view.Snapshot.Summary.Label == "landed-after-timeout"
	})
	if view.Revision != 1 || view.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("late refresh view=%+v", view)
	}
}

func TestRegimeSnapshotCacheShutdownDrainsRefreshAndRejectsReplacement(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	started := make(chan struct{})
	exited := make(chan struct{})
	serveDone := make(chan error, 1)
	go func() {
		_, err := cache.serve(context.Background(), func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
			close(started)
			<-ctx.Done()
			close(exited)
			return nil, false, nil, ctx.Err()
		})
		serveDone <- err
	}()
	<-started

	cancelDaemon()
	cache.wait()
	select {
	case <-exited:
	default:
		t.Fatal("shutdown wait returned before refresh exited")
	}
	if cache.refreshing() {
		t.Fatal("shutdown left refresh registered")
	}
	var unavailable *regimeSnapshotCacheUnavailableError
	if err := <-serveDone; !errors.As(err, &unavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cold shutdown serve error=%v, want typed unavailable wrapping cancellation", err)
	}

	var replacementCalls atomic.Int32
	_, err := cache.serve(context.Background(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		replacementCalls.Add(1)
		return regimeSnapshotCacheFixture(now, "must-not-publish"), true, nil, nil
	})
	if !errors.As(err, &unavailable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("post-shutdown serve error=%v, want typed unavailable wrapping cancellation", err)
	}
	if got := replacementCalls.Load(); got != 0 {
		t.Fatalf("post-shutdown replacement refresh calls=%d, want 0", got)
	}
}

func TestRegimeSnapshotCacheIncompleteAndFailedRefreshPreserveLastGood(t *testing.T) {
	baseStore := openRegimeSnapshotTestStore(t)
	store := &regimeSnapshotFaultStore{base: baseStore}
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	firstAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: firstAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(firstAt, "stable"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, ok, err := baseStore.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok {
		t.Fatalf("load before state ok=%v err=%v", ok, err)
	}
	beforeView, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}

	clock.Set(firstAt.Add(2 * time.Minute))
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "partial-must-not-land"), false, nil, nil
	}); err != nil {
		t.Fatalf("warm stale serve should retain last-good: %v", err)
	}
	incomplete := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailureRefreshIncomplete
	})
	assertRegimeSnapshotStateUnchanged(t, baseStore, before, beforeView, incomplete)

	clock.Set(firstAt.Add(8 * time.Minute))
	store.fail.Store(true)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "publish-must-not-land"), true, nil, nil
	}); err != nil {
		t.Fatalf("warm stale serve should retain last-good on publish failure: %v", err)
	}
	publishFailed := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailurePublishFailed
	})
	assertRegimeSnapshotStateUnchanged(t, baseStore, before, beforeView, publishFailed)
}

func assertRegimeSnapshotStateUnchanged(
	t *testing.T,
	store *corestore.Store,
	wantDocument corestore.StateDocument,
	wantView regimeSnapshotCacheView,
	gotView regimeSnapshotCacheView,
) {
	t.Helper()
	gotDocument, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok {
		t.Fatalf("load after state ok=%v err=%v", ok, err)
	}
	if gotDocument.Revision != wantDocument.Revision || !bytes.Equal(gotDocument.JSON, wantDocument.JSON) {
		t.Fatalf("authoritative bytes/revision changed: before rev=%d after rev=%d", wantDocument.Revision, gotDocument.Revision)
	}
	if gotView.Revision != wantView.Revision || gotView.Fingerprint != wantView.Fingerprint || gotView.Snapshot.Summary.Label != "stable" {
		t.Fatalf("last-good changed: before=%+v after=%+v", wantView, gotView)
	}
}

func TestRegimeSnapshotCacheColdIncompleteIsTypedUnavailableAndDoesNotPersist(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(now, "partial"), false, nil, nil
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) || !errors.Is(err, errRegimeSnapshotRefreshIncomplete) {
		t.Fatalf("error=%v, want typed cold unavailable wrapping incomplete", err)
	}
	if view.Snapshot != nil || view.Revision != 0 || view.Health.Status != rpc.RegimeAuthorityUnavailable || view.Health.FailureCode != rpc.RegimeAuthorityFailureRefreshIncomplete {
		t.Fatalf("cold incomplete view=%+v", view)
	}
	if _, ok, readErr := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind); readErr != nil || ok {
		t.Fatalf("incomplete refresh persisted state: ok=%v err=%v", ok, readErr)
	}
}

func TestRegimeSnapshotCacheSuppressesWarmFailureRetriesUntilDue(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	firstAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: firstAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(firstAt, "stable-during-cooldown"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	attemptAt := firstAt.Add(2 * time.Minute)
	clock.Set(attemptAt)
	var failedCalls atomic.Int32
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		failedCalls.Add(1)
		return nil, false, nil, errors.New("upstream unavailable")
	}); err != nil {
		t.Fatalf("warm last-good should survive failed refresh: %v", err)
	}
	warmFailed := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailureRefreshFailed
	})
	if warmFailed.Snapshot.Summary.Label != "stable-during-cooldown" {
		t.Fatalf("failed refresh replaced last-good: %+v", warmFailed)
	}

	clock.Set(attemptAt.Add(4 * time.Minute))
	var recoveryCalls atomic.Int32
	suppressed, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		recoveryCalls.Add(1)
		return regimeSnapshotCacheFixture(clock.Now(), "too-early"), true, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recoveryCalls.Load() != 0 || suppressed.Health.Refreshing || suppressed.Health.FailureCode != rpc.RegimeAuthorityFailureRefreshFailed || suppressed.Snapshot.Summary.Label != "stable-during-cooldown" {
		t.Fatalf("warm retry was not suppressed: calls=%d view=%+v", recoveryCalls.Load(), suppressed)
	}

	clock.Set(attemptAt.Add(5 * time.Minute))
	started := make(chan struct{})
	release := make(chan struct{})
	recoverRefresh := func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		recoveryCalls.Add(1)
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(clock.Now(), "recovered-when-due"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}
	due, err := cache.serve(t.Context(), recoverRefresh)
	if err != nil || due.Snapshot.Summary.Label != "stable-during-cooldown" || !due.Health.Refreshing {
		t.Fatalf("due retry did not start behind stale LKG: view=%+v err=%v", due, err)
	}
	<-started
	close(release)
	recovered := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return view.Snapshot != nil && view.Snapshot.Summary.Label == "recovered-when-due" && !view.Health.Refreshing
	})
	if recovered.Health.FailureCode != rpc.RegimeAuthorityFailureNone || recovered.Revision != 2 || recoveryCalls.Load() != 1 || failedCalls.Load() != 1 {
		t.Fatalf("recovery view=%+v failed_calls=%d recovery_calls=%d", recovered, failedCalls.Load(), recoveryCalls.Load())
	}
}

func TestRegimeSnapshotCacheSuppressesColdFailureRetriesUntilDue(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	attemptAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: attemptAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	var failedCalls atomic.Int32
	_, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		failedCalls.Add(1)
		return nil, false, nil, errors.New("cold acquisition failed")
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("first cold error=%v, want typed unavailable", err)
	}

	clock.Set(attemptAt.Add(4 * time.Minute))
	var recoveryCalls atomic.Int32
	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		recoveryCalls.Add(1)
		return regimeSnapshotCacheFixture(clock.Now(), "too-early"), true, nil, nil
	})
	if !errors.As(err, &unavailable) || !errors.Is(err, errRegimeSnapshotRefreshSuppressed) {
		t.Fatalf("suppressed cold error=%v", err)
	}
	if recoveryCalls.Load() != 0 || view.Snapshot != nil || view.Health.Refreshing || view.Health.FailureCode != rpc.RegimeAuthorityFailureRefreshFailed {
		t.Fatalf("cold suppression view=%+v recovery_calls=%d", view, recoveryCalls.Load())
	}

	clock.Set(attemptAt.Add(5 * time.Minute))
	started := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		view, err := cache.serve(context.Background(), func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
			recoveryCalls.Add(1)
			close(started)
			select {
			case <-release:
				return regimeSnapshotCacheFixture(clock.Now(), "cold-recovered"), true, nil, nil
			case <-ctx.Done():
				return nil, false, nil, ctx.Err()
			}
		})
		if err == nil && (view.Snapshot == nil || view.Snapshot.Summary.Label != "cold-recovered") {
			err = errors.New("cold retry did not return recovered state")
		}
		result <- err
	}()
	<-started
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if failedCalls.Load() != 1 || recoveryCalls.Load() != 1 {
		t.Fatalf("failed_calls=%d recovery_calls=%d", failedCalls.Load(), recoveryCalls.Load())
	}
}

func TestRegimeSnapshotCacheAllowRefreshNowPreservesFailedAuthority(t *testing.T) {
	for _, warm := range []bool{false, true} {
		name := "cold"
		if warm {
			name = "warm"
		}
		t.Run(name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			daemonContext := t.Context()
			firstAt := regimeSnapshotTestNow()
			clock := &regimeSnapshotTestClock{now: firstAt}
			cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
			if warm {
				if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
					return regimeSnapshotCacheFixture(firstAt, "preserved-warm"), true, nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				clock.Set(firstAt.Add(2 * time.Minute))
			}

			_, refreshErr := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
				return nil, false, nil, errors.New("gateway temporarily unavailable")
			})
			if warm && refreshErr != nil {
				t.Fatalf("warm failure should serve last-good: %v", refreshErr)
			}
			if !warm && refreshErr == nil {
				t.Fatal("cold failure should be unavailable")
			}
			before := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
				return !view.Health.Refreshing && view.Health.FailureCode == rpc.RegimeAuthorityFailureRefreshFailed
			})
			beforeDocument, beforeExists, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
			if err != nil {
				t.Fatal(err)
			}
			cache.mu.Lock()
			dueBefore := cache.refreshDueLocked(clock.Now())
			cache.mu.Unlock()
			if dueBefore {
				t.Fatal("failed refresh unexpectedly due before backoff reset")
			}

			cache.allowRefreshNow()
			after, err := cache.current()
			if err != nil {
				t.Fatal(err)
			}
			cache.mu.Lock()
			dueAfter := cache.refreshDueLocked(clock.Now())
			cache.mu.Unlock()
			if !dueAfter {
				t.Fatal("backoff reset did not make refresh immediately due")
			}
			if after.Health.FailureCode != before.Health.FailureCode || after.Health.Status != before.Health.Status || after.Revision != before.Revision || after.Fingerprint != before.Fingerprint {
				t.Fatalf("allowRefreshNow changed authority state: before=%+v after=%+v", before, after)
			}
			if (before.Snapshot == nil) != (after.Snapshot == nil) || (after.Snapshot != nil && after.Snapshot.Summary.Label != "preserved-warm") {
				t.Fatalf("allowRefreshNow changed last-good snapshot: before=%+v after=%+v", before.Snapshot, after.Snapshot)
			}
			afterDocument, afterExists, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
			if err != nil {
				t.Fatal(err)
			}
			if beforeExists != afterExists || beforeDocument.Revision != afterDocument.Revision || !bytes.Equal(beforeDocument.JSON, afterDocument.JSON) {
				t.Fatalf("allowRefreshNow changed SQLite authority: before_exists=%v after_exists=%v before_rev=%d after_rev=%d", beforeExists, afterExists, beforeDocument.Revision, afterDocument.Revision)
			}
		})
	}
}

func TestRegimeSnapshotCacheNeverCallsAfterPublishOnFailure(t *testing.T) {
	now := regimeSnapshotTestNow()
	tests := []struct {
		name         string
		faultPublish bool
		refresh      func(regimeSnapshotAfterPublishFunc) regimeSnapshotRefreshFunc
	}{
		{
			name: "acquisition error",
			refresh: func(after regimeSnapshotAfterPublishFunc) regimeSnapshotRefreshFunc {
				return func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
					return nil, false, after, errors.New("acquisition failed")
				}
			},
		},
		{
			name: "incomplete acquisition",
			refresh: func(after regimeSnapshotAfterPublishFunc) regimeSnapshotRefreshFunc {
				return func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
					return regimeSnapshotCacheFixture(now, "partial"), false, after, nil
				}
			},
		},
		{
			name: "invalid composed snapshot",
			refresh: func(after regimeSnapshotAfterPublishFunc) regimeSnapshotRefreshFunc {
				return func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
					snapshot := regimeSnapshotCacheFixture(now, "invalid")
					snapshot.Breadth.Status = ""
					snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
					return snapshot, true, after, nil
				}
			},
		},
		{
			name:         "SQLite publish failure",
			faultPublish: true,
			refresh: func(after regimeSnapshotAfterPublishFunc) regimeSnapshotRefreshFunc {
				return func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
					return regimeSnapshotCacheFixture(now, "valid-but-unpublished"), true, after, nil
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseStore := openRegimeSnapshotTestStore(t)
			faultStore := &regimeSnapshotFaultStore{base: baseStore}
			faultStore.fail.Store(test.faultPublish)
			daemonContext := t.Context()
			clock := &regimeSnapshotTestClock{now: now}
			cache := newRegimeSnapshotTestCache(t, faultStore, daemonContext, clock)
			var calls atomic.Int32
			_, err := cache.serve(t.Context(), test.refresh(func(context.Context, regimeSnapshotPublication) error {
				calls.Add(1)
				return nil
			}))
			if err == nil {
				t.Fatal("failure case unexpectedly succeeded")
			}
			if got := calls.Load(); got != 0 {
				t.Fatalf("after-publish callback calls=%d, want zero", got)
			}
			if _, ok, readErr := baseStore.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind); readErr != nil || ok {
				t.Fatalf("failed refresh persisted state: ok=%v err=%v", ok, readErr)
			}
		})
	}
}

func TestRegimeSnapshotCacheDoesNotPromotePreparedStateWhenTransactionRollsBack(t *testing.T) {
	baseStore := openRegimeSnapshotTestStore(t)
	store := &regimeSnapshotFaultStore{base: baseStore}
	store.fail.Store(true)
	store.returnPreparedOnFail.Store(true)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	var callbackCalls atomic.Int32

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "must not become authority"), true,
			func(context.Context, regimeSnapshotPublication) error {
				callbackCalls.Add(1)
				return nil
			}, nil
	})
	if _, ok := errors.AsType[*regimeSnapshotCacheUnavailableError](err); !ok {
		t.Fatalf("serve error=%v, want typed unavailable", err)
	}
	if view.Snapshot != nil || view.Revision != 0 {
		t.Fatalf("rolled-back prepared state was promoted: %+v", view)
	}
	if callbackCalls.Load() != 0 {
		t.Fatalf("after-publish calls=%d, want zero without a committed CAS", callbackCalls.Load())
	}
	if pending, revision := cache.projectionFailure(); pending || revision != 0 {
		t.Fatalf("rolled-back state marked projection pending=%v revision=%d", pending, revision)
	}
	if _, ok, loadErr := baseStore.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind); loadErr != nil || ok {
		t.Fatalf("rolled-back state exists in SQLite: ok=%v err=%v", ok, loadErr)
	}
}

func TestRegimeSnapshotCacheReadbackPromotesCommitWhenPostCommitObserverFails(t *testing.T) {
	baseStore := openRegimeSnapshotTestStore(t)
	store := &regimeSnapshotFaultStore{base: baseStore}
	store.commitThenFail.Store(true)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	var callbackPublication regimeSnapshotPublication

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "durably committed"), true,
			func(_ context.Context, publication regimeSnapshotPublication) error {
				callbackPublication = publication
				return nil
			}, nil
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "last-good is unavailable") {
		t.Fatalf("serve error=%v, want typed post-commit authority failure", err)
	}
	if view.Revision != 1 || view.Snapshot == nil || view.Snapshot.Summary.Label != "durably committed" {
		t.Fatalf("committed readback was not promoted: %+v", view)
	}
	if callbackPublication.Revision != 1 || callbackPublication.PublishedAt.IsZero() || callbackPublication.Fingerprint != view.Fingerprint {
		t.Fatalf("after-publish publication=%+v view=%+v", callbackPublication, view)
	}
	if pending, revision := cache.projectionFailure(); !pending || revision != 1 {
		t.Fatalf("post-commit failure projection pending=%v revision=%d, want true/1", pending, revision)
	}
	document, ok, loadErr := baseStore.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if loadErr != nil || !ok || document.Revision != 1 || !bytes.Equal(document.JSON, cache.raw) {
		t.Fatalf("durable readback document: ok=%v revision=%d err=%v", ok, document.Revision, loadErr)
	}
}

func TestRegimeSnapshotCacheCallbackPanicPromotesCommittedRevisionAndCleansUp(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	firstAt := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: firstAt}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(firstAt, "sqlite-committed-before-panic"), true, func(context.Context, regimeSnapshotPublication) error {
			panic("injected after-publish panic")
		}, nil
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(errors.Unwrap(unavailable).Error(), "after-publish callback panicked") {
		t.Fatalf("callback panic error=%v", err)
	}
	if cache.refreshing() {
		t.Fatal("callback panic left refresh wedged")
	}
	if view.Snapshot == nil || view.Snapshot.Summary.Label != "sqlite-committed-before-panic" || view.Revision != 1 || view.Health.FailureCode != rpc.RegimeAuthorityFailurePublishFailed || view.Health.Refreshing {
		t.Fatalf("panic recovery view=%+v", view)
	}
	document, ok, readErr := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if readErr != nil || !ok || document.Revision != 1 {
		t.Fatalf("committed document ok=%v revision=%d err=%v", ok, document.Revision, readErr)
	}
	if err := cache.markProjectionRepaired(1); err != nil {
		t.Fatalf("mark projection repaired: %v", err)
	}

	// Once the daemon-side projection repair has completed, advancing the clock
	// proves the next CAS uses the promoted revision instead of wedging on
	// expected=0. Until that repair, N+1 publication is intentionally blocked.
	clock.Set(firstAt.Add(6 * time.Minute))
	started := make(chan struct{})
	release := make(chan struct{})
	if _, err := cache.serve(t.Context(), func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(clock.Now(), "revision-two"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}); err != nil {
		t.Fatalf("stale LKG should serve during retry: %v", err)
	}
	<-started
	close(release)
	recovered := waitForRegimeSnapshotCache(t, cache, func(view regimeSnapshotCacheView) bool {
		return view.Revision == 2 && view.Snapshot != nil && view.Snapshot.Summary.Label == "revision-two" && !view.Health.Refreshing
	})
	if recovered.Health.FailureCode != rpc.RegimeAuthorityFailureNone {
		t.Fatalf("successful retry did not heal callback failure: %+v", recovered.Health)
	}
}

func TestRegimeSnapshotCacheStrictRestartHydration(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	firstContext, cancelFirst := context.WithCancel(context.Background())
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	first := newRegimeSnapshotTestCache(t, store, firstContext, clock)
	if _, err := first.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(now, "survives-restart"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cancelFirst()

	secondContext, cancelSecond := context.WithCancel(context.Background())
	t.Cleanup(cancelSecond)
	second := newRegimeSnapshotTestCache(t, store, secondContext, clock)
	var refreshCalls atomic.Int32
	view, err := second.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		refreshCalls.Add(1)
		return nil, false, nil, errors.New("fresh hydration must not refresh")
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls.Load() != 0 || view.Snapshot.Summary.Label != "survives-restart" || view.Revision != 1 {
		t.Fatalf("restart hydration view=%+v refresh_calls=%d", view, refreshCalls.Load())
	}
}

func TestRegimeSnapshotCacheFutureHydrationIsStaleAndSuppressesRefresh(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	now := regimeSnapshotTestNow()
	raw, _, err := encodeRegimeSnapshotDocument(regimeSnapshotCacheFixture(now, "future commit"))
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}

	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: saved.UpdatedAt.Add(-time.Minute)}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	var refreshCalls atomic.Int32
	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		refreshCalls.Add(1)
		return regimeSnapshotCacheFixture(clock.Now(), "must not publish"), true, nil, nil
	})
	if err != nil {
		t.Fatalf("future LKG should remain available only as stale context: %v", err)
	}
	if view.Snapshot == nil || view.Revision != 1 || view.Health.Status != rpc.RegimeAuthorityStale || view.Health.FailureCode != rpc.RegimeAuthorityFailureClockInvalid {
		t.Fatalf("future hydration view=%+v", view)
	}
	if refreshCalls.Load() != 0 || cache.refreshing() {
		t.Fatalf("future hydration refresh calls=%d refreshing=%v, want suppressed", refreshCalls.Load(), cache.refreshing())
	}

	clock.Set(saved.UpdatedAt.Add(30 * time.Second))
	recovered, err := cache.serve(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Health.Status != rpc.RegimeAuthorityFresh || recovered.Health.FailureCode != rpc.RegimeAuthorityFailureNone {
		t.Fatalf("caught-up clock health=%+v, want normal fresh authority", recovered.Health)
	}
}

func TestRegimeSnapshotCacheClockRollbackCannotPublishInFlightRefresh(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	initialClock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, initialClock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(initialClock.Now(), "revision one"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	cache.mu.Lock()
	committedAt := cache.lastSuccessAt
	cache.mu.Unlock()
	initialClock.Set(committedAt.Add(2 * time.Minute))
	started := make(chan struct{})
	release := make(chan struct{})
	if _, err := cache.serve(t.Context(), func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(initialClock.Now(), "must not become revision two"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}); err != nil {
		t.Fatalf("stale LKG should serve while refresh starts: %v", err)
	}
	<-started
	initialClock.Set(committedAt.Add(-time.Minute))
	close(release)
	cache.wait()

	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 || view.Snapshot == nil || view.Snapshot.Summary.Label != "revision one" || view.Health.Status != rpc.RegimeAuthorityStale || view.Health.FailureCode != rpc.RegimeAuthorityFailureClockInvalid {
		t.Fatalf("rollback view=%+v", view)
	}
	document, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok || document.Revision != 1 {
		t.Fatalf("rollback SQLite revision: ok=%v revision=%d err=%v", ok, document.Revision, err)
	}
}

func TestRegimeSnapshotCacheAtomicCommitFloorClosesPostCheckRollback(t *testing.T) {
	baseStore := openRegimeSnapshotTestStore(t)
	store := &regimeSnapshotFaultStore{base: baseStore}
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "revision one"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	committedAt := cache.lastSuccessAt
	cache.mu.Unlock()

	observedFloor := make(chan time.Time, 1)
	store.beforeCAS = func(update corestore.StateDocumentCAS) corestore.StateDocumentCAS {
		if update.ExpectedRevision == 1 {
			observedFloor <- update.UpdatedAtNotBefore
			// Simulate wall time stepping behind the retained commit after the
			// cache pre-check but before Corestore captures its commit stamp.
			clock.Set(committedAt.Add(-time.Minute))
			update.UpdatedAtNotBefore = time.Now().UTC().Add(time.Hour)
		}
		return update
	}
	clock.Set(committedAt.Add(2 * time.Minute))
	started := make(chan struct{})
	release := make(chan struct{})
	if _, err := cache.serve(t.Context(), func(ctx context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		close(started)
		select {
		case <-release:
			return regimeSnapshotCacheFixture(clock.Now(), "must not commit"), true, nil, nil
		case <-ctx.Done():
			return nil, false, nil, ctx.Err()
		}
	}); err != nil {
		t.Fatalf("stale LKG should serve while refresh starts: %v", err)
	}
	<-started
	close(release)
	cache.wait()

	if floor := <-observedFloor; !floor.Equal(committedAt) {
		t.Fatalf("atomic commit floor=%s, want retained commit %s", floor, committedAt)
	}
	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != 1 || view.Snapshot == nil || view.Snapshot.Summary.Label != "revision one" ||
		view.Health.Status != rpc.RegimeAuthorityStale || view.Health.FailureCode != rpc.RegimeAuthorityFailureClockInvalid {
		t.Fatalf("post-check rollback view=%+v", view)
	}
	if pending, revision := cache.projectionFailure(); pending || revision != 0 {
		t.Fatalf("rejected rollback marked projection pending=%v revision=%d", pending, revision)
	}
	document, ok, err := baseStore.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok || document.Revision != 1 {
		t.Fatalf("atomic rollback SQLite revision: ok=%v revision=%d err=%v", ok, document.Revision, err)
	}
}

func TestHandleRegimeSnapshotWithholdsRevisionThatBecomesProjectionPendingAfterRepairCheck(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	clock := &regimeSnapshotTestClock{now: regimeSnapshotTestNow()}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return regimeSnapshotCacheFixture(clock.Now(), "must be withheld"), true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	// Model the exact race boundary: handleRegimeSnapshot has already observed
	// no pending repair, then the publisher marks the served revision pending
	// before serve takes its atomic projection gate. The injected clock runs
	// under cache.mu, so this transition is deterministic and race-free.
	cache.mu.Lock()
	servedAt := cache.lastSuccessAt
	cache.now = func() time.Time {
		cache.projectionPending = true
		cache.projectionRevision = cache.revision
		cache.failureCode = rpc.RegimeAuthorityFailurePublishFailed
		return servedAt
	}
	cache.mu.Unlock()

	server := &Server{regimeSnapshots: cache}
	snapshot, err := server.handleRegimeSnapshot(t.Context(), nil)
	var unavailable *regimeSnapshotCacheUnavailableError
	if snapshot != nil || !errors.As(err, &unavailable) || !errors.Is(unavailable, errRegimeSnapshotProjectionPending) {
		t.Fatalf("handler snapshot=%+v error=%v, want pending revision withheld", snapshot, err)
	}
}

func TestRegimeSnapshotCacheRejectsMalformedPersistedState(t *testing.T) {
	now := regimeSnapshotTestNow()
	valid, _, err := encodeRegimeSnapshotDocument(regimeSnapshotCacheFixture(now, "valid"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		wantErr string
	}{
		{
			name: "unknown field",
			mutate: func(raw []byte) []byte {
				trimmed := bytes.TrimSuffix(raw, []byte("}"))
				return append(trimmed, []byte(`,"legacy_authority":true}`)...)
			},
			wantErr: "unknown field",
		},
		{
			name: "fingerprint mismatch",
			mutate: func(raw []byte) []byte {
				var snapshot rpc.RegimeSnapshotResult
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
				snapshot.VIXTermStructure.Status = "error"
				mutated, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				return mutated
			},
			wantErr: "fingerprint mismatch",
		},
		{
			name: "response metadata persisted",
			mutate: func(raw []byte) []byte {
				var snapshot rpc.RegimeSnapshotResult
				if err := json.Unmarshal(raw, &snapshot); err != nil {
					t.Fatal(err)
				}
				age := int64(0)
				successAt := now
				snapshot.AuthorityHealth = &rpc.RegimeAuthorityHealth{
					Status: rpc.RegimeAuthorityFresh, LastSuccessAt: &successAt, LastSuccessAgeSeconds: &age,
				}
				mutated, err := json.Marshal(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				return mutated
			},
			wantErr: "response-only authority health",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openRegimeSnapshotTestStore(t)
			raw := test.mutate(bytes.Clone(valid))
			if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
				ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, JSON: raw,
			}); err != nil {
				t.Fatalf("seed malformed state: %v", err)
			}
			daemonContext := t.Context()
			_, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
				FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: 5 * time.Minute,
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("load error=%v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestRegimeSnapshotCacheHydratesV1AndRefreshesToV2(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	now := regimeSnapshotTestNow()
	legacy := regimeSnapshotCacheFixture(now, "legacy v1")
	legacy.Fingerprint.Version = "regime-fp-v1"
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: regimeSnapshotStateKind, JSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}

	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	cache, err := loadRegimeSnapshotCache(t.Context(), daemonContext, store, regimeSnapshotCacheOptions{
		FreshFor: time.Minute, RefreshTimeout: time.Second, FailureRetryAfter: time.Minute,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("hydrate v1 snapshot: %v", err)
	}
	view, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != saved.Revision || view.Fingerprint != legacy.Fingerprint ||
		view.Health.Status != rpc.RegimeAuthorityStale {
		t.Fatalf("hydrated v1 view revision=%d fingerprint=%+v health=%+v", view.Revision, view.Fingerprint, view.Health)
	}
	unchanged, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, regimeSnapshotStateKind)
	if err != nil || !ok {
		t.Fatalf("read unchanged v1 document: ok=%v err=%v", ok, err)
	}
	if unchanged.Revision != saved.Revision || !unchanged.UpdatedAt.Equal(saved.UpdatedAt) || !bytes.Equal(unchanged.JSON, saved.JSON) {
		t.Fatal("v1 hydration rewrote the authoritative publication")
	}

	v2 := regimeSnapshotCacheFixture(now.Add(time.Minute), "current v2")
	if _, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return v2, true, nil, nil
	}); err != nil {
		t.Fatalf("serve stale v1 snapshot: %v", err)
	}
	cache.wait()
	upgraded, err := cache.current()
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.Revision != saved.Revision+1 || upgraded.Fingerprint.Version != rpc.RegimeFingerprintVersion ||
		upgraded.Health.Status != rpc.RegimeAuthorityFresh {
		t.Fatalf("upgraded view revision=%d fingerprint=%+v health=%+v", upgraded.Revision, upgraded.Fingerprint, upgraded.Health)
	}
}

func TestRegimeSnapshotEncoderRemainsV2Only(t *testing.T) {
	legacy := regimeSnapshotCacheFixture(regimeSnapshotTestNow(), "legacy writer")
	legacy.Fingerprint.Version = "regime-fp-v1"
	if _, _, err := encodeRegimeSnapshotDocument(legacy); err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("encode v1 error=%v, want fingerprint mismatch", err)
	}
}

func TestDecodeRegimeSnapshotDocumentRejectsTrailingJSON(t *testing.T) {
	now := regimeSnapshotTestNow()
	raw, _, err := encodeRegimeSnapshotDocument(regimeSnapshotCacheFixture(now, "valid"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeRegimeSnapshotDocument(append(raw, []byte(` {}`)...))
	if err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("decode error=%v, want trailing JSON rejection", err)
	}
}

func TestRegimeSnapshotCacheRejectsStructurallyIncompletePublish(t *testing.T) {
	store := openRegimeSnapshotTestStore(t)
	daemonContext, cancelDaemon := context.WithCancel(context.Background())
	t.Cleanup(cancelDaemon)
	now := regimeSnapshotTestNow()
	clock := &regimeSnapshotTestClock{now: now}
	cache := newRegimeSnapshotTestCache(t, store, daemonContext, clock)
	snapshot := regimeSnapshotCacheFixture(now, "missing-row")
	snapshot.Breadth.Status = ""
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)

	view, err := cache.serve(t.Context(), func(context.Context) (*rpc.RegimeSnapshotResult, bool, regimeSnapshotAfterPublishFunc, error) {
		return snapshot, true, nil, nil
	})
	var unavailable *regimeSnapshotCacheUnavailableError
	if !errors.As(err, &unavailable) || !strings.Contains(errors.Unwrap(unavailable).Error(), "breadth status is required") {
		t.Fatalf("error=%v, want structural publish rejection", err)
	}
	if view.Snapshot != nil || view.Health.FailureCode != rpc.RegimeAuthorityFailurePublishFailed {
		t.Fatalf("invalid snapshot was published: %+v", view)
	}
}
