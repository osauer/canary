package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A retained prior-session snapshot is replaced by the next regular-session
// refresh. These tests witness that recovery path: nothing computes while the
// options session is closed, the first tick after the open refreshes behind
// the retained value, any successful result (clean or blocked) replaces it and
// survives a restart, and a failed refresh keeps it visible behind the
// escalating retry gate.

func TestGammaSessionRolloverReplacesRetainedBlockedSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name        string
		spxWarnings []string
		want        string
	}{
		{name: "clean_success", spxWarnings: []string{"expiries_stale:2d"}, want: rpc.GammaRankabilityRankable},
		{name: "blocked_success", spxWarnings: []string{"throttled"}, want: rpc.GammaRankabilityBlocked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGammaRolloverRun(t, "2026-09-23 16:05", "2026-09-24 07:00")
			r.assertOvernightNotDue(t, "2026-09-24 07:00")
			refresh := r.openSession(t, "2026-09-24 09:31")

			publishedAt := gammaRolloverNY(t, "2026-09-24 09:41")
			r.compute.finish(t, gammaRolloverResult(publishedAt, tc.spxWarnings...), nil)
			r.awaitPublication(t, refresh)

			at := gammaRolloverNY(t, "2026-09-24 09:45")
			env := r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return at })
			if env.Status != rpc.GammaZeroStatusReady || env.Refreshing || !env.Result.AsOf.Equal(publishedAt) {
				t.Fatalf("session refresh did not replace the retained snapshot: status=%s refreshing=%v as_of=%s", env.Status, env.Refreshing, env.Result.AsOf)
			}
			if got := env.Result.Quality.Rankability; got != tc.want {
				t.Fatalf("replacement rankability = %s (%s), want %s", got, env.Result.Quality.RankabilityReason, tc.want)
			}
			assertGammaSPXWarnings(t, env.Result, tc.spxWarnings)
			if got := gammaOperationalCadence(&env, at); got != rpc.DataCadenceCurrent {
				t.Fatalf("cadence after replacement = %s, want current", got)
			}

			restarted := newGammaRolloverCache(t, r.store, at)
			reloaded := restarted.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return at })
			if reloaded.Status != rpc.GammaZeroStatusReady || !reloaded.Result.AsOf.Equal(publishedAt) {
				t.Fatalf("restart reloaded status=%s as_of=%v, want the %s replacement", reloaded.Status, reloaded.Result.AsOf, publishedAt)
			}
			if got := reloaded.Result.Quality.Rankability; got != tc.want {
				t.Fatalf("restart changed rankability to %s (%s), want %s", got, reloaded.Result.Quality.RankabilityReason, tc.want)
			}
			assertGammaSPXWarnings(t, reloaded.Result, tc.spxWarnings)
			idle := &gammaRolloverCompute{outcomes: make(chan gammaRolloverOutcome)}
			if _, fresh := restarted.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, at, 1, idle.fn); fresh || restarted.refreshInFlight(rpc.GammaZeroScopeCombined) {
				t.Fatalf("restart recomputed a current-session snapshot: fresh=%v", fresh)
			}
		})
	}
}

func TestGammaSessionRolloverFailedRefreshKeepsRetainedSnapshot(t *testing.T) {
	r := newGammaRolloverRun(t, "2026-09-23 16:05", "2026-09-24 07:00")
	refresh := r.openSession(t, "2026-09-24 09:31")
	failure := errors.New("zero-gamma: option expiries timeout for SPX after 45s")

	// Each failure escalates the quiet period from the failed attempt's start:
	// 60 s, then 120 s, then 240 s.
	for _, step := range []struct{ failedAt, quiet, retry string }{
		{"2026-09-24 09:31", "2026-09-24 09:31:59", "2026-09-24 09:32"},
		{"2026-09-24 09:32", "2026-09-24 09:33:59", "2026-09-24 09:34"},
	} {
		r.compute.finish(t, nil, failure)
		awaitGammaJob(t, refresh)
		quiet := gammaRolloverNY(t, step.quiet)
		r.tick(t, quiet)
		if job := r.refreshJob(); job != nil {
			t.Fatalf("refresh failed at %s was retried inside its backoff at %s", step.failedAt, step.quiet)
		}
		env := r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return quiet })
		if env.Status != rpc.GammaZeroStatusReady || !env.Result.AsOf.Equal(r.retainedAt) {
			t.Fatalf("failed refresh replaced the retained snapshot: status=%s as_of=%v", env.Status, env.Result.AsOf)
		}
		if !slices.Contains(env.Result.Warnings, "refresh_failed:timeout") {
			t.Fatalf("failed refresh is not disclosed on the retained snapshot: %v", env.Result.Warnings)
		}
		if env.Result.Quality.Rankability != rpc.GammaRankabilityBlocked {
			t.Fatalf("retained snapshot after failed refresh ranked as %s", env.Result.Quality.Rankability)
		}
		r.tick(t, gammaRolloverNY(t, step.retry))
		if refresh = r.refreshJob(); refresh == nil {
			t.Fatalf("no retry once the backoff after the %s failure elapsed", step.failedAt)
		}
	}
	r.compute.finish(t, nil, failure)
	awaitGammaJob(t, refresh)
	reconnect := gammaRolloverNY(t, "2026-09-24 09:35")
	r.tick(t, reconnect)
	if r.refreshJob() != nil {
		t.Fatal("third failure did not escalate the backoff past one minute")
	}
	r.cache.resetRetryBackoff()
	r.tick(t, reconnect)
	if refresh = r.refreshJob(); refresh == nil {
		t.Fatal("reconnect did not reset the gamma retry backoff")
	}
	r.compute.awaitCalls(t, 4)
	if got := r.compute.calls.Load(); got != 4 {
		t.Fatalf("compute attempts = %d, want 4 (initial, two backoff retries, reconnect)", got)
	}

	publishedAt := gammaRolloverNY(t, "2026-09-24 09:44")
	r.compute.finish(t, gammaRolloverResult(publishedAt), nil)
	r.awaitPublication(t, refresh)
	at := gammaRolloverNY(t, "2026-09-24 09:45")
	env := r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return at })
	if !env.Result.AsOf.Equal(publishedAt) || slices.ContainsFunc(env.Result.Warnings, func(code string) bool {
		return strings.HasPrefix(code, "refresh_failed:")
	}) {
		t.Fatalf("recovered snapshot as_of=%s warnings=%v", env.Result.AsOf, env.Result.Warnings)
	}
}

func TestGammaClosedSessionNeverComputes(t *testing.T) {
	for _, tc := range []struct{ name, retainedAt, at string }{
		{name: "weekend", retainedAt: "2026-09-25 16:05", at: "2026-09-26 12:00"},
		{name: "labor_day", retainedAt: "2026-09-04 16:05", at: "2026-09-07 12:00"},
		{name: "thanksgiving", retainedAt: "2026-11-25 16:05", at: "2026-11-26 10:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := gammaRolloverNY(t, tc.at)
			if gammaClassifySession(at) != rpc.SessionClosed {
				t.Fatalf("%s is not a closed options session in the market calendar", tc.at)
			}
			r := newGammaRolloverRun(t, tc.retainedAt, tc.at)
			r.assertOvernightNotDue(t, tc.at)

			cold := newGammaRolloverCache(t, openGammaRolloverStore(t), at)
			if job, fresh := cold.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, at, 1, r.compute.fn); job != nil || fresh || cold.refreshInFlight(rpc.GammaZeroScopeCombined) {
				t.Fatalf("closed session started a cold compute: job=%v fresh=%v", job != nil, fresh)
			}
			if got := r.compute.calls.Load(); got != 0 {
				t.Fatalf("closed session ran %d computes", got)
			}
		})
	}
}

// gammaRolloverRun is one daemon lifetime over a store that already holds a
// prior-session combined snapshot whose SPX slice was persisted with the
// blocking unclassified_data_warning.
type gammaRolloverRun struct {
	store      *corestore.Store
	cache      *gammaZeroCache
	compute    *gammaRolloverCompute
	published  chan string
	retainedAt time.Time
}

func newGammaRolloverRun(t *testing.T, retainedAt, startedAt string) *gammaRolloverRun {
	t.Helper()
	r := &gammaRolloverRun{
		store:      openGammaRolloverStore(t),
		compute:    &gammaRolloverCompute{outcomes: make(chan gammaRolloverOutcome)},
		published:  make(chan string, 8),
		retainedAt: gammaRolloverNY(t, retainedAt),
	}
	retained := hydrateGammaComputed(gammaRolloverResult(r.retainedAt, "unclassified_data_warning"))
	seed := newGammaZeroStore("")
	if err := seed.UseCoreStore(r.store); err != nil {
		t.Fatal(err)
	}
	if err := seed.Save(rpc.GammaZeroScopeCombined, nySessionKey(r.retainedAt), retained); err != nil {
		t.Fatal(err)
	}
	r.cache = newGammaRolloverCache(t, r.store, gammaRolloverNY(t, startedAt))
	r.cache.setPublicationCallback(func(scope string) { r.published <- scope })
	return r
}

// assertOvernightNotDue checks the retained snapshot before the options open:
// served as the last completed session's evidence, blocked by its persisted
// warning, not due, and never recomputed.
func (r *gammaRolloverRun) assertOvernightNotDue(t *testing.T, value string) {
	t.Helper()
	at := gammaRolloverNY(t, value)
	job, fresh := r.cache.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, at, 1, r.compute.fn)
	if fresh || job == nil || job.result == nil || !job.result.AsOf.Equal(r.retainedAt) || r.refreshJob() != nil {
		t.Fatalf("closed session did not serve the retained snapshot without computing: fresh=%v refresh=%v", fresh, r.refreshJob() != nil)
	}
	env := r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return at })
	if env.Status != rpc.GammaZeroStatusReady || env.Refreshing || !env.Result.AsOf.Equal(r.retainedAt) {
		t.Fatalf("retained snapshot not served: status=%s refreshing=%v", env.Status, env.Refreshing)
	}
	if spx := env.Result.PerIndex["SPX"]; spx == nil || !gammaQualityHasGate(spx.Quality, "warning_contract", rpc.GammaQualityGateBlock) ||
		env.Result.Quality.Rankability != rpc.GammaRankabilityBlocked || len(env.Result.Quality.Blockers) != 1 {
		t.Fatalf("retained fixture is not blocked by its persisted warning: %+v", env.Result.Quality)
	}
	if got := gammaOperationalCadence(&env, at); got != rpc.DataCadenceNotDue {
		t.Fatalf("cadence before the options open = %s, want not_due", got)
	}
	if got := gammaCadenceClass(gammaRolloverRegime(env), at); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("regime cadence before the options open = %s, want not_due", got)
	}
}

// openSession runs the first scheduler tick after the options open and
// returns the single refresh it started behind the retained snapshot.
func (r *gammaRolloverRun) openSession(t *testing.T, value string) *gammaComputation {
	t.Helper()
	open := gammaRolloverNY(t, value)
	job, fresh := r.cache.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, open, 1, r.compute.fn)
	if fresh || job == nil || job.result == nil || !job.result.AsOf.Equal(r.retainedAt) {
		t.Fatalf("session open did not keep serving the retained snapshot: fresh=%v", fresh)
	}
	refresh := r.refreshJob()
	if refresh == nil {
		t.Fatal("session open did not start a refresh behind the retained snapshot")
	}
	r.compute.awaitCalls(t, 1)
	r.tick(t, open.Add(time.Minute))
	if again := r.refreshJob(); again != refresh || r.compute.calls.Load() != 1 {
		t.Fatal("a second tick started another refresh while one was in flight")
	}

	env := r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return open })
	if env.Status != rpc.GammaZeroStatusReady || !env.Refreshing || !env.Result.AsOf.Equal(r.retainedAt) {
		t.Fatalf("refreshing session open served status=%s refreshing=%v", env.Status, env.Refreshing)
	}
	if !gammaPublicationPending(&env, open) || gammaCadenceClass(gammaRolloverRegime(env), open) != rpc.RegimeFreshnessPending {
		t.Fatal("in-flight first session refresh is not pending")
	}
	late := open.Add(gammaPublicationWindow)
	env = r.cache.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return late })
	if !env.Refreshing || gammaPublicationPending(&env, late) || gammaCadenceClass(gammaRolloverRegime(env), late) != rpc.RegimeFreshnessOverdue {
		t.Fatal("first session refresh stayed pending past the publication window")
	}
	return refresh
}

func (r *gammaRolloverRun) tick(t *testing.T, at time.Time) {
	t.Helper()
	job, fresh := r.cache.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, at, 1, r.compute.fn)
	if fresh || job == nil || !job.result.AsOf.Equal(r.retainedAt) {
		t.Fatalf("tick at %s stopped serving the retained snapshot: fresh=%v", at, fresh)
	}
}

func (r *gammaRolloverRun) refreshJob() *gammaComputation {
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	if slot := r.cache.slots[rpc.GammaZeroScopeCombined]; slot != nil {
		return slot.refresh
	}
	return nil
}

func (r *gammaRolloverRun) awaitPublication(t *testing.T, job *gammaComputation) {
	t.Helper()
	awaitGammaJob(t, job)
	select {
	case scope := <-r.published:
		if scope != rpc.GammaZeroScopeCombined {
			t.Fatalf("published scope %q", scope)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("successful refresh was never published")
	}
}

// gammaRolloverCompute stands in for the broker fan-out. Each call blocks
// until the test supplies its outcome.
type gammaRolloverCompute struct {
	calls    atomic.Int32
	outcomes chan gammaRolloverOutcome
}

type gammaRolloverOutcome struct {
	result *rpc.GammaZeroComputed
	err    error
}

func (g *gammaRolloverCompute) fn(ctx context.Context, _ *atomic.Int32) (*rpc.GammaZeroComputed, error) {
	g.calls.Add(1)
	select {
	case out := <-g.outcomes:
		return out.result, out.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (g *gammaRolloverCompute) finish(t *testing.T, result *rpc.GammaZeroComputed, err error) {
	t.Helper()
	select {
	case g.outcomes <- gammaRolloverOutcome{result: result, err: err}:
	case <-time.After(5 * time.Second):
		t.Fatal("no compute was waiting for an outcome")
	}
}

func (g *gammaRolloverCompute) awaitCalls(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for g.calls.Load() < want {
		if time.Now().After(deadline) {
			t.Fatalf("compute calls = %d, want %d", g.calls.Load(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitGammaJob(t *testing.T, job *gammaComputation) {
	t.Helper()
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatal("gamma refresh never finished")
	}
}

func openGammaRolloverStore(t *testing.T) *corestore.Store {
	t.Helper()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newGammaRolloverCache models a daemon start at now: the production cache
// constructor followed by the daemon.db attachment.
func newGammaRolloverCache(t *testing.T, store *corestore.Store, now time.Time) *gammaZeroCache {
	t.Helper()
	cache := newGammaZeroCacheWithStore(newGammaZeroStore(""), now, nil)
	if err := cache.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	return cache
}

// gammaRolloverResult is a synthetic combined compute whose SPX slice carries
// spxWarnings. Everything else passes the quality gates.
func gammaRolloverResult(asOf time.Time, spxWarnings ...string) *rpc.GammaZeroComputed {
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, asOf)
	spx.Warnings = spxWarnings
	return combineGammaResults(rankableGammaFixture(rpc.GammaZeroScopeSPY, asOf), spx)
}

func gammaRolloverRegime(env rpc.GammaZeroSPXResult) *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{GammaZero: rpc.RegimeGammaZero{Status: rpc.RegimeStatusStale, Envelope: env}}
}

func gammaRolloverNY(t *testing.T, value string) time.Time {
	t.Helper()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	layout := "2006-01-02 15:04"
	if strings.Count(value, ":") == 2 {
		layout = "2006-01-02 15:04:05"
	}
	at, err := time.ParseInLocation(layout, value, ny)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func gammaQualityHasGate(q *rpc.GammaSignalQuality, name, status string) bool {
	return q != nil && slices.ContainsFunc(q.Gates, func(gate rpc.GammaQualityGate) bool {
		return gate.Name == name && gate.Status == status
	})
}

func assertGammaSPXWarnings(t *testing.T, result *rpc.GammaZeroComputed, want []string) {
	t.Helper()
	spx := result.PerIndex["SPX"]
	if spx == nil {
		t.Fatal("combined result lost its SPX slice")
	}
	if !slices.Equal(spx.Warnings, want) || slices.Contains(result.Warnings, "unclassified_data_warning") {
		t.Fatalf("SPX warnings lost their typed codes: spx=%v combined=%v, want %v", spx.Warnings, result.Warnings, want)
	}
}
