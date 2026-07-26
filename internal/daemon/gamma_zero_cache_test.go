package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestGammaZeroCache_SingleflightWithinSession proves the cache's
// singleflight invariant: two concurrent kickOrJoin calls in the same
// NY session share one compute, even if both arrive while the first
// is still in flight. This is load-bearing — without it, a dashboard
// generator + a CLI poll at the same moment would launch duplicate
// fan-outs and double the gateway market-data slot pressure.
func TestGammaZeroCache_SingleflightWithinSession(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	block := make(chan struct{}) // hold the compute until both callers have joined

	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	var first, second *gammaComputation
	var firstFresh, secondFresh bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		first, firstFresh = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	}()
	// Brief gap to make sure the first caller has acquired the mutex
	// and registered c.current before the second hits.
	time.Sleep(20 * time.Millisecond)
	go func() {
		defer wg.Done()
		second, secondFresh = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	}()
	// Let the second caller observe in-flight state, then unblock.
	time.Sleep(20 * time.Millisecond)
	close(block)
	wg.Wait()

	if first != second {
		t.Fatalf("singleflight broken: got two distinct jobs %p vs %p", first, second)
	}
	if !firstFresh {
		t.Errorf("first caller should report fresh=true, got false")
	}
	if secondFresh {
		t.Errorf("second caller should report fresh=false (joined existing), got true")
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("compute ran %d times, want exactly 1", got)
	}

	<-first.done
	if first.result == nil || first.result.SpotUnderlying != 5000 {
		t.Errorf("first.result mismatch: %+v", first.result)
	}
}

// TestGammaZeroCache_SessionRollover verifies a new compute kicks off
// when the NY session date changes while still serving yesterday's
// known-good result until the refresh lands.
func TestGammaZeroCache_SessionRollover(t *testing.T) {
	c := newGammaZeroCache()
	day1 := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, day1, 300, compute)
	<-job1.done
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, day2, 300, compute)
	if !fresh1 {
		t.Errorf("day-1 should be a fresh kickoff: got fresh1=false")
	}
	if fresh2 || job2 != job1 {
		t.Fatalf("session rollover should serve LKG while refreshing: fresh2=%v job2=%p want job1=%p", fresh2, job2, job1)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	var job3 *gammaComputation
	for time.Now().Before(deadline) {
		job3, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, day2, 300, compute)
		if job3 != job1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job3 == job1 {
		t.Fatal("session rollover refresh never promoted")
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("expected 2 compute runs across the rollover, got %d", got)
	}
}

// TestGammaZeroCache_ForceSupersedesInflight proves force() cancels
// the in-flight compute and starts a new one, distinct from the
// superseded job. The old job's done channel may never close (the
// goroutine returns without recording a result on cancel); callers
// that were waiting on it are responsible for their own timeouts.
func TestGammaZeroCache_ForceSupersedesInflight(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	cancelled := make(chan struct{})
	slowCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		select {
		case <-ctx.Done():
			close(cancelled)
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
			return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
		}
	}
	fastCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return &rpc.GammaZeroComputed{SpotUnderlying: 5050}, nil
	}

	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, slowCompute)
	time.Sleep(30 * time.Millisecond) // let slowCompute enter its select

	job2 := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, 300, fastCompute)

	if job1 == job2 {
		t.Fatal("force should produce a new job distinct from the superseded one")
	}

	select {
	case <-cancelled:
		// expected: the slow compute saw ctx.Done() and exited
	case <-time.After(500 * time.Millisecond):
		t.Fatal("superseded compute was not cancelled within budget")
	}

	<-job2.done
	if job2.result == nil || job2.result.SpotUnderlying != 5050 {
		t.Errorf("force job result mismatch: %+v", job2.result)
	}
}

func TestGammaZeroCache_SnapshotCombinedSliceUsesCanonicalPerIndex(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	combinedAsOf := now.Add(-time.Minute)
	staleSingleAsOf := now.Add(-4 * time.Hour)

	spy := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 749.26,
		GammaSign:      "negative",
		GammaTotalAbs:  6.2e9,
		LegCount:       375,
		AsOf:           combinedAsOf,
	}
	spx := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7506.83,
		GammaSign:      "negative",
		GammaTotalAbs:  25.8e9,
		LegCount:       821,
		AsOf:           combinedAsOf,
	}
	combined := &rpc.GammaZeroComputed{
		Scope:           rpc.GammaZeroScopeCombined,
		GammaTotalAbs:   32.0e9,
		RegimeAgreement: "agree:short-gamma",
		PerIndex: map[string]*rpc.GammaZeroComputed{
			"SPY": spy,
			"SPX": spx,
		},
		AsOf: combinedAsOf,
	}
	staleSPXOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7473.47,
		GammaSign:      "negative",
		GammaTotalAbs:  38.4e9,
		LegCount:       824,
		AsOf:           staleSingleAsOf,
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(combined, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPX:      {current: newPersistedComputation(staleSPXOnly, rpc.GammaZeroScopeSPX, now)},
	}

	env, ok := c.snapshotCombinedSlice(rpc.GammaZeroScopeSPX, func() time.Time { return now })
	if !ok {
		t.Fatal("snapshotCombinedSlice returned ok=false")
	}
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want %q", env.Status, rpc.GammaZeroStatusReady)
	}
	if env.Result == nil || env.Result.Scope != rpc.GammaZeroScopeSPX {
		t.Fatalf("Result = %+v, want SPX per-index result", env.Result)
	}
	if env.Result.SpotUnderlying != spx.SpotUnderlying {
		t.Fatalf("SpotUnderlying = %.2f, want canonical combined SPX slice %.2f",
			env.Result.SpotUnderlying, spx.SpotUnderlying)
	}
	if env.Result.GammaTotalAbs != spx.GammaTotalAbs {
		t.Fatalf("GammaTotalAbs = %.1f, want canonical combined SPX slice %.1f",
			env.Result.GammaTotalAbs, spx.GammaTotalAbs)
	}

	priorSession := now.Add(-24 * time.Hour)
	priorCombined := cloneGammaComputed(combined)
	priorCombined.AsOf = priorSession
	priorCombined.PerIndex["SPY"].AsOf = priorSession
	priorCombined.PerIndex["SPX"].AsOf = priorSession
	c.slots[rpc.GammaZeroScopeCombined].current = newPersistedComputation(priorCombined, rpc.GammaZeroScopeCombined, now)
	delete(c.slots, rpc.GammaZeroScopeSPX)
	env, ok = c.snapshotCombinedSlice(rpc.GammaZeroScopeSPX, func() time.Time { return now })
	if !ok {
		t.Fatal("prior-session combined slice should serve as last-known-good context")
	}
	if env.Result == nil || !env.Result.AsOf.Equal(priorSession) {
		t.Fatalf("prior-session slice as_of = %+v, want %s", env.Result, priorSession)
	}
	if env.Result.Quality == nil || env.Result.Quality.Rankability != rpc.GammaRankabilityBlocked {
		t.Fatalf("prior-session slice quality = %+v, want blocked freshness", env.Result.Quality)
	}
}

func TestGammaZeroCache_SnapshotCombinedSliceUsesCanonicalDegradedScope(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	freshAsOf := now.Add(-time.Minute)
	staleAsOf := now.Add(-4 * time.Hour)

	freshSPX := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7506.83,
		GammaSign:      "negative",
		GammaTotalAbs:  38.65e9,
		LegCount:       417,
		PricedLegCount: 934,
		Warnings:       []string{"spy_unavailable:throttled"},
		AsOf:           freshAsOf,
	}
	staleSPXOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7473.47,
		GammaSign:      "negative",
		GammaTotalAbs:  16.93e9,
		LegCount:       397,
		PricedLegCount: 947,
		AsOf:           staleAsOf,
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(freshSPX, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPX:      {current: newPersistedComputation(staleSPXOnly, rpc.GammaZeroScopeSPX, now)},
	}

	env, ok := c.snapshotCombinedSlice(rpc.GammaZeroScopeSPX, func() time.Time { return now })
	if !ok {
		t.Fatal("snapshotCombinedSlice returned ok=false")
	}
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want %q", env.Status, rpc.GammaZeroStatusReady)
	}
	if env.Result == nil || env.Result.Scope != rpc.GammaZeroScopeSPX {
		t.Fatalf("Result = %+v, want canonical degraded SPX result", env.Result)
	}
	if env.Result.AsOf != freshAsOf {
		t.Fatalf("AsOf = %s, want fresh canonical degraded SPX as_of %s", env.Result.AsOf, freshAsOf)
	}
	if env.Result.GammaTotalAbs != freshSPX.GammaTotalAbs {
		t.Fatalf("GammaTotalAbs = %.1f, want canonical degraded SPX %.1f",
			env.Result.GammaTotalAbs, freshSPX.GammaTotalAbs)
	}
	if _, ok := c.snapshotCombinedSlice(rpc.GammaZeroScopeSPY, func() time.Time { return now }); ok {
		t.Fatal("SPX-only degraded combined result must not satisfy a SPY single-scope request")
	}
}

func TestPreferOwnGammaSnapshotUsesFresherForcedScope(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	canonical := rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeSPX,
			AsOf:  now.Add(-5 * time.Minute),
		},
	}
	own := rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeSPX,
			AsOf:  now,
		},
	}

	if !preferOwnGammaSnapshot(canonical, own) {
		t.Fatalf("fresh forced SPX scope should win over older canonical combined slice")
	}
	if preferOwnGammaSnapshot(own, canonical) {
		t.Fatalf("older own scope must not replace fresher canonical slice")
	}
}

func TestPreferOwnGammaSnapshotUsesCleanScopeOverFallbackCanonical(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	canonical := rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:    rpc.GammaZeroScopeSPX,
			AsOf:     now,
			Warnings: []string{"spx_cache_fallback:fetch_canceled"},
		},
	}
	own := rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeSPX,
			AsOf:  now,
		},
	}

	if !preferOwnGammaSnapshot(canonical, own) {
		t.Fatalf("clean own SPX scope should win over equal-time fallback canonical slice")
	}
	if preferOwnGammaSnapshot(own, canonical) {
		t.Fatalf("fallback-tainted own scope must not replace equal-time clean canonical slice")
	}
}

func TestGammaZeroCache_CombinedSnapshotUsesCachedSPXFallback(t *testing.T) {
	c := newGammaZeroCache()
	// Saturday, when the cache must not kick a fresh option fan-out.
	now := time.Date(2026, 5, 30, 14, 0, 0, 0, time.UTC)
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}
	spyAsOf := now.Add(-18 * time.Hour)
	spxAsOf := now.Add(-20 * time.Hour)

	spyOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 756.34,
		GammaSign:      "positive",
		GammaTotalAbs:  1.2e9,
		LegCount:       120,
		AsOf:           spyAsOf,
		Warnings:       []string{"oi_missing", "spx_unavailable:fetch_canceled"},
	}
	spxOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7511.55,
		GammaSign:      "negative",
		GammaTotalAbs:  5.6e9,
		LegCount:       300,
		AsOf:           spxAsOf,
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(spyOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPX:      {current: newPersistedComputation(spxOnly, rpc.GammaZeroScopeSPX, now)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil || got.Scope != rpc.GammaZeroScopeCombined {
		t.Fatalf("Result scope = %+v, want combined", got)
	}
	if got.PerIndex["SPY"] == nil || got.PerIndex["SPX"] == nil {
		t.Fatalf("fallback result missing per-index slices: %+v", got.PerIndex)
	}
	if got.RegimeAgreement != "disagree" {
		t.Fatalf("RegimeAgreement = %q, want disagree", got.RegimeAgreement)
	}
	if !got.AsOf.Equal(spxAsOf) {
		t.Fatalf("combined AsOf = %v, want older SPX fallback as_of %v", got.AsOf, spxAsOf)
	}
	if got.Summary == nil || got.Summary.Confidence != "degraded" {
		t.Fatalf("summary confidence = %+v, want degraded", got.Summary)
	}
	if !hasGammaWarning(got.WarningDetails, "spx_cache_fallback:fetch_canceled") {
		t.Fatalf("top-level warning_details missing SPX cache fallback: %+v", got.WarningDetails)
	}
	if hasGammaWarning(got.WarningDetails, "spx_unavailable:fetch_canceled") {
		t.Fatalf("fallback result should not say SPX is excluded: %+v", got.WarningDetails)
	}
	if hasGammaWarning(got.PerIndex["SPY"].WarningDetails, "spx_unavailable:fetch_canceled") {
		t.Fatalf("SPY child should not retain SPX-unavailable warning: %+v", got.PerIndex["SPY"].WarningDetails)
	}
	if !hasGammaWarning(got.PerIndex["SPY"].WarningDetails, "oi_missing") {
		t.Fatalf("SPY child lost its own warning: %+v", got.PerIndex["SPY"].WarningDetails)
	}
}

func TestGammaZeroCache_CombinedSnapshotRejectsPriorSPXFallbackDuringRTH(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 6, 2, 14, 12, 0, 0, time.UTC)
	if cls := gammaClassifySession(now); cls != rpc.SessionRTH {
		t.Fatalf("test fixture sanity check: expected SessionRTH, got %v", cls)
	}
	spyAsOf := now.Add(-time.Minute)
	spxAsOf := now.Add(-24 * time.Hour)

	spyOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 756.34,
		GammaSign:      "positive",
		GammaTotalAbs:  1.2e9,
		LegCount:       120,
		AsOf:           spyAsOf,
		Warnings:       []string{"spx_unavailable:timeout"},
	}
	spxOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7511.55,
		GammaSign:      "negative",
		GammaTotalAbs:  5.6e9,
		LegCount:       300,
		AsOf:           spxAsOf,
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(spyOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPX:      {current: newPersistedComputation(spxOnly, rpc.GammaZeroScopeSPX, spxAsOf)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil || got.Scope != rpc.GammaZeroScopeSPY {
		t.Fatalf("Result scope = %+v, want current SPY-only degraded result", got)
	}
	if got.PerIndex["SPX"] != nil {
		t.Fatalf("stale prior-session SPX fallback was merged into current result: %+v", got.PerIndex["SPX"])
	}
	if !got.AsOf.Equal(spyAsOf) {
		t.Fatalf("result AsOf = %v, want current SPY as_of %v", got.AsOf, spyAsOf)
	}
	if got.Quality == nil || got.Quality.Rankability == rpc.GammaRankabilityRankable {
		t.Fatalf("quality = %+v, want degraded SPY proxy", got.Quality)
	}
	if !hasGammaWarning(got.WarningDetails, "spx_unavailable:timeout") {
		t.Fatalf("current SPX failure warning missing: %+v", got.WarningDetails)
	}
	if hasGammaWarning(got.WarningDetails, "spx_cache_fallback:timeout") {
		t.Fatalf("stale SPX fallback should not be advertised: %+v", got.WarningDetails)
	}
}

func TestGammaZeroCache_CombinedSnapshotRebuildsFromNewerSingleScopes(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	oldAsOf := now.Add(-10 * time.Minute)
	newAsOf := now.Add(-time.Minute)

	oldSPXOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7580,
		GammaSign:      "negative",
		GammaTotalAbs:  8.6e8,
		LegCount:       37,
		AsOf:           oldAsOf,
		Warnings:       []string{"oi_missing", "spy_unavailable:zero_magnitude"},
	}
	spyOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 758,
		GammaSign:      "negative",
		GammaTotalAbs:  2.3e7,
		LegCount:       3,
		AsOf:           newAsOf,
		Warnings:       []string{"oi_missing"},
		Quality:        &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
	}
	spxOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7581,
		GammaSign:      "negative",
		GammaTotalAbs:  9.1e8,
		LegCount:       37,
		AsOf:           newAsOf,
		Warnings:       []string{"oi_missing"},
		Quality:        &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(oldSPXOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPY:      {current: newPersistedComputation(spyOnly, rpc.GammaZeroScopeSPY, now)},
		rpc.GammaZeroScopeSPX:      {current: newPersistedComputation(spxOnly, rpc.GammaZeroScopeSPX, now)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil || got.Scope != rpc.GammaZeroScopeCombined {
		t.Fatalf("Result = %+v, want rebuilt combined", got)
	}
	if got.PerIndex["SPY"] == nil || got.PerIndex["SPX"] == nil {
		t.Fatalf("rebuilt result missing per-index slices: %+v", got.PerIndex)
	}
	if hasGammaWarning(got.WarningDetails, "spy_unavailable:zero_magnitude") {
		t.Fatalf("rebuilt combined must not keep stale SPY-excluded warning: %+v", got.WarningDetails)
	}
	if got.LegCount != spyOnly.LegCount+spxOnly.LegCount {
		t.Fatalf("LegCount = %d, want fresh single-scope sum %d", got.LegCount, spyOnly.LegCount+spxOnly.LegCount)
	}
	if got.Summary == nil || got.Summary.PerIndex["SPY"].LegCount != spyOnly.LegCount {
		t.Fatalf("summary did not include fresh SPY slice: %+v", got.Summary)
	}
}

func TestGammaZeroCache_CurrentSPYFailureDoesNotBackfillStaleSPY(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 6, 2, 14, 12, 0, 0, time.UTC)
	staleSPYAsOf := now.Add(-24 * time.Hour)
	freshSPXAsOf := now.Add(-time.Minute)

	staleSPY := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 760,
		GammaSign:      "negative",
		GammaTotalAbs:  2.3e7,
		LegCount:       2,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 760, Right: "P", AbsGEX: 1.4e7, Expiry: "2026-06-05"},
			{Underlying: "SPY", Strike: 758, Right: "P", AbsGEX: 9e6, Expiry: "2026-06-05"},
		},
		AsOf:   staleSPYAsOf,
		Method: gammaMethodToken,
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "freshness: computed for prior session",
		},
	}
	freshSPXOnly := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 7600,
		GammaSign:      "negative",
		GammaTotalAbs:  9.1e8,
		LegCount:       395,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 7600, Right: "P", AbsGEX: 9.1e8, Expiry: "2026-06-05"},
		},
		AsOf:     freshSPXAsOf,
		Method:   gammaMethodToken,
		Warnings: []string{"spy_unavailable:zero_magnitude"},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(freshSPXOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPY:      {current: newPersistedComputation(staleSPY, rpc.GammaZeroScopeSPY, now)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil {
		t.Fatal("Result is nil")
	}
	if got.Scope != rpc.GammaZeroScopeSPX {
		t.Fatalf("scope = %q, want current SPX-only degraded result", got.Scope)
	}
	if got.PerIndex["SPY"] != nil {
		t.Fatalf("current SPY failure was backfilled from stale cache: %+v", got.PerIndex["SPY"])
	}
	if got.LegCount != freshSPXOnly.LegCount || got.GammaTotalAbs != freshSPXOnly.GammaTotalAbs {
		t.Fatalf("combined metrics included stale SPY: leg_count=%d gamma_total_abs=%v", got.LegCount, got.GammaTotalAbs)
	}
	for _, top := range got.TopStrikes {
		if top.Underlying == "SPY" {
			t.Fatalf("top_strikes included stale SPY leg after current SPY failure: %+v", got.TopStrikes)
		}
	}
	if !hasGammaWarning(got.WarningDetails, "spy_unavailable:zero_magnitude") {
		t.Fatalf("current SPY failure diagnostic missing: %+v", got.WarningDetails)
	}
}

func TestGammaZeroCache_CurrentSPYFailureDoesNotBackfillBlockedSPY(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 6, 2, 14, 12, 0, 0, time.UTC)
	asOf := now.Add(-time.Minute)

	blockedSPY := &rpc.GammaZeroComputed{
		Scope:         rpc.GammaZeroScopeSPY,
		GammaSign:     "negative",
		GammaTotalAbs: 2.3e7,
		LegCount:      2,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 760, Right: "P", AbsGEX: 1.4e7, Expiry: "2026-06-05"},
		},
		AsOf:   asOf,
		Method: gammaMethodToken,
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "oi_observed_coverage: OI observed on 0.3% of priced legs",
		},
	}
	freshSPXOnly := &rpc.GammaZeroComputed{
		Scope:         rpc.GammaZeroScopeSPX,
		GammaSign:     "negative",
		GammaTotalAbs: 9.1e8,
		LegCount:      395,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 7600, Right: "P", AbsGEX: 9.1e8, Expiry: "2026-06-05"},
		},
		AsOf:     asOf,
		Method:   gammaMethodToken,
		Warnings: []string{"spy_unavailable:zero_magnitude"},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(freshSPXOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPY:      {current: newPersistedComputation(blockedSPY, rpc.GammaZeroScopeSPY, now)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil {
		t.Fatal("Result is nil")
	}
	if got.Scope != rpc.GammaZeroScopeSPX {
		t.Fatalf("scope = %q, want current SPX-only degraded result", got.Scope)
	}
	if got.PerIndex["SPY"] != nil {
		t.Fatalf("current SPY failure was backfilled from blocked cache: %+v", got.PerIndex["SPY"])
	}
	if got.LegCount != freshSPXOnly.LegCount || got.GammaTotalAbs != freshSPXOnly.GammaTotalAbs {
		t.Fatalf("combined metrics included blocked SPY: leg_count=%d gamma_total_abs=%v", got.LegCount, got.GammaTotalAbs)
	}
}

func TestGammaZeroCache_CurrentSPYFailureDoesNotBackfillRawWeakSPY(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 6, 2, 14, 12, 0, 0, time.UTC)
	asOf := now.Add(-time.Minute)

	rawWeakSPY := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		GammaSign:      "negative",
		GammaTotalAbs:  2.3e7,
		LegCount:       2,
		PricedLegCount: 751,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 760, Right: "P", AbsGEX: 1.4e7, Expiry: "2026-06-05"},
		},
		AsOf:     asOf,
		Method:   gammaMethodToken,
		Warnings: []string{"oi_missing"},
		// Quality intentionally nil: persisted cache entries are stored raw
		// and annotated only on the served clone.
	}
	freshSPXOnly := &rpc.GammaZeroComputed{
		Scope:         rpc.GammaZeroScopeSPX,
		GammaSign:     "negative",
		GammaTotalAbs: 9.1e8,
		LegCount:      395,
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 7600, Right: "P", AbsGEX: 9.1e8, Expiry: "2026-06-05"},
		},
		AsOf:     asOf,
		Method:   gammaMethodToken,
		Warnings: []string{"spy_unavailable:throttled"},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(freshSPXOnly, rpc.GammaZeroScopeCombined, now)},
		rpc.GammaZeroScopeSPY:      {current: newPersistedComputation(rawWeakSPY, rpc.GammaZeroScopeSPY, now)},
	}

	env := c.snapshotCurrent(rpc.GammaZeroScopeCombined, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("Status = %q, want ready", env.Status)
	}
	got := env.Result
	if got == nil {
		t.Fatal("Result is nil")
	}
	if got.Scope != rpc.GammaZeroScopeSPX {
		t.Fatalf("scope = %q, want current SPX-only degraded result", got.Scope)
	}
	if got.PerIndex["SPY"] != nil {
		t.Fatalf("current SPY failure was backfilled from raw weak cache: %+v", got.PerIndex["SPY"])
	}
	if got.LegCount != freshSPXOnly.LegCount || got.GammaTotalAbs != freshSPXOnly.GammaTotalAbs {
		t.Fatalf("combined metrics included raw weak SPY: leg_count=%d gamma_total_abs=%v", got.LegCount, got.GammaTotalAbs)
	}
}

func hasGammaWarning(details []rpc.GammaWarningDetail, code string) bool {
	for _, d := range details {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestGammaZeroCache_RetriesErrorAfterTTL pins the no-poison
// invariant: a transient compute error must NOT stick in cache for
// the rest of the NY trading session. Before this fix the cache
// returned c.current unconditionally on session-key match; a
// gateway-side cold-start timeout at 9:30 AM ET poisoned every
// regime/gamma call until midnight NY rolled the session key.
//
// Within the TTL window: same-session callers see the cached error
// (no thundering herd). Past the TTL: a fresh compute is kicked.
// In-flight jobs and successful results stay sticky in both cases.
func TestGammaZeroCache_RetriesErrorAfterTTL(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	bonk := errors.New("gateway timeout")
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return nil, bonk
	}

	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	<-job1.done
	if !fresh1 || job1.err == nil {
		t.Fatalf("first kickoff: fresh=%v err=%v, want fresh=true err!=nil", fresh1, job1.err)
	}

	// Same instant: same-session caller must see the cached error,
	// not trigger a new compute. This is the dampener against retry
	// storms while a gateway is genuinely flapping.
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	if fresh2 || job2 != job1 {
		t.Errorf("within TTL: got fresh=%v job=%p (want fresh=false job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("within TTL: compute ran %d times, want 1", got)
	}

	// Past the TTL: same session key, but the cached error is now
	// stale-on-error. kickOrJoin must launch a fresh attempt.
	past := now.Add(gammaErrorRetryTTL + time.Second)
	successCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5050}, nil
	}
	job3, fresh3 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, past, 300, successCompute)
	if !fresh3 || job3 == job1 {
		t.Errorf("past TTL: got fresh=%v job=%p (want fresh=true new job)", fresh3, job3)
	}
	<-job3.done
	if job3.err != nil || job3.result == nil {
		t.Errorf("retry compute should have succeeded: err=%v result=%v", job3.err, job3.result)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("past TTL: compute ran %d times, want 2", got)
	}

	// Past the TTL again, but now the cache holds a SUCCESS — the
	// retry path must not fire on healthy state.
	future := past.Add(2 * gammaErrorRetryTTL)
	job4, fresh4 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, future, 300, errCompute)
	if fresh4 || job4 != job3 {
		t.Errorf("success cache must stay sticky regardless of age: fresh=%v job=%p want job=%p", fresh4, job4, job3)
	}
}

// TestGammaZeroCache_SoftTTLRefreshesBehindStale proves the
// refresh-while-stale invariant: when the cached successful compute
// ages past softTTLRTH, the next kickOrJoin returns the stale value
// IMMEDIATELY and kicks a background refresh. The promotion of that
// refresh to current happens on the NEXT kickOrJoin — so a single
// caller doesn't pay for the refresh, the system pays it over the
// next two calls.
//
// Load-bearing: this is what keeps `canary regime` fast after a
// long-idle daemon. Without it, the same-session cache served a
// 10-min-old answer with no path to freshness until midnight NY.
func TestGammaZeroCache_SoftTTLRefreshesBehindStale(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		// Distinct payload per run so we can tell first vs refresh apart.
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// First call: kicks the initial compute.
	job1, fresh1 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done
	if !fresh1 || job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff: fresh=%v result=%+v", fresh1, job1.result)
	}

	// Within softTTL: same job returned, no refresh kicked.
	withinTTL := now.Add(softTTLRTH - time.Second)
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, withinTTL, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("within softTTL: got fresh=%v job=%p (want fresh=false job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("within softTTL: compute ran %d times, want 1 (no refresh expected)", got)
	}

	// Past softTTL: STILL get the stale value, but a refresh kicks
	// behind it. The caller doesn't block — they see the cached
	// SpotUnderlying=5001 immediately.
	pastTTL := now.Add(softTTLRTH + time.Second)
	job3, fresh3 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	if fresh3 || job3 != job1 {
		t.Errorf("past softTTL: caller should get the stale value: got fresh=%v job=%p (want fresh=false job=%p)", fresh3, job3, job1)
	}
	if job3.result == nil || job3.result.SpotUnderlying != 5001 {
		t.Errorf("past softTTL: caller should see stale result 5001, got %+v", job3.result)
	}

	// Wait for the refresh to land. We can't reach into c.refresh
	// directly without exposing internals; instead, poll kickOrJoin
	// until we see the promoted result. The refresh compute is
	// synchronous in this test, so this resolves on the next call.
	deadline := time.Now().Add(500 * time.Millisecond)
	var job4 *gammaComputation
	for time.Now().Before(deadline) {
		job4, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
		if job4 != job1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job4 == job1 {
		t.Fatal("refresh never promoted — kickOrJoin kept returning the stale job")
	}
	if job4.result == nil || job4.result.SpotUnderlying != 5002 {
		t.Errorf("refresh promotion: got %+v, want SpotUnderlying=5002 (the refreshed compute)", job4.result)
	}
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("after refresh: compute ran %d times, want exactly 2 (initial + one refresh)", got)
	}
}

// TestGammaZeroCache_LongComputeGetsFullSoftTTLAfterSuccess pins the runtime
// failure where a 13-minute combined compute was refreshed only two minutes
// after it finished because the 15-minute TTL was measured from kickoff.
func TestGammaZeroCache_LongComputeGetsFullSoftTTLAfterSuccess(t *testing.T) {
	c := newGammaZeroCache()
	startedAt := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	completedAt := startedAt.Add(13 * time.Minute)
	scope := rpc.GammaZeroScopeCombined

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	current, fresh := c.kickOrJoin(context.Background(), scope, startedAt, 300, compute)
	<-current.done
	if !fresh || current.err != nil {
		t.Fatalf("initial kickoff: fresh=%v err=%v", fresh, current.err)
	}
	if current.completedAt.IsZero() || current.completedAt.Before(startedAt) {
		t.Fatalf("initial success did not record its publication time: %s", current.completedAt)
	}

	// Model the observed long combined run without sleeping for 13 minutes.
	// completedAt is the successful publication boundary recorded by spawnJob.
	current.completedAt = completedAt

	// The old startedAt-based schedule would already refresh here. The fixed
	// schedule keeps the gateway quiet because success is only two minutes old.
	earlyPoll := startedAt.Add(softTTLRTH + time.Second)
	got, fresh := c.kickOrJoin(context.Background(), scope, earlyPoll, 300, compute)
	if fresh || got != current {
		t.Fatalf("early poll should keep serving current: fresh=%v got=%p want=%p", fresh, got, current)
	}
	if runs := computeRuns.Load(); runs != 1 {
		t.Fatalf("early poll started %d computes, want 1", runs)
	}

	// Once the full soft TTL has elapsed after publication, one background
	// refresh is due while callers continue to receive the current result.
	duePoll := completedAt.Add(softTTLRTH + time.Second)
	got, fresh = c.kickOrJoin(context.Background(), scope, duePoll, 300, compute)
	if fresh || got != current {
		t.Fatalf("due poll should refresh behind current: fresh=%v got=%p want=%p", fresh, got, current)
	}
	c.mu.Lock()
	refresh := c.slots[scope].refresh
	c.mu.Unlock()
	if refresh == nil {
		t.Fatal("due poll did not start a background refresh")
	}
	<-refresh.done
	if runs := computeRuns.Load(); runs != 2 {
		t.Fatalf("due poll started %d computes, want 2", runs)
	}
}

// TestGammaZeroCache_SoftTTLDoesNotStackRefreshes proves the
// refresh-while-stale path is itself singleflighted: a second caller
// arriving while a refresh is in flight does NOT kick a second
// refresh. Otherwise a burst of post-TTL pollers (e.g. an MCP agent
// hammering canary_regime) would launch parallel compute fan-outs,
// defeating the gateway-slot-protection that motivates the cache.
func TestGammaZeroCache_SoftTTLDoesNotStackRefreshes(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	block := make(chan struct{})
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		if run >= 2 {
			// Hold the refresh until the test signals — gives the
			// second caller a window to observe in-flight refresh.
			<-block
		}
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// Initial kickoff completes immediately.
	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done

	// Two callers past softTTL, back-to-back. The first kicks the
	// refresh; the second sees an in-flight refresh and does NOT
	// kick another.
	pastTTL := now.Add(softTTLRTH + time.Second)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)

	// Give the goroutines time to settle; the second compute
	// should still be blocked.
	time.Sleep(20 * time.Millisecond)
	if got := computeRuns.Load(); got != 2 {
		t.Errorf("expected exactly 2 computes (initial + one refresh), got %d", got)
	}

	close(block) // let the refresh finish; drains the goroutine
}

// TestGammaZeroCache_SoftTTLFailedRefreshKeepsCachedSuccess proves a
// failed soft-TTL refresh must NOT poison a known-good cached value.
// Without this, a transient gateway hiccup during a refresh could
// overwrite the entire morning's stable γ-zero reading until midnight
// rolled the session key.
func TestGammaZeroCache_SoftTTLFailedRefreshKeepsCachedSuccess(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	bonk := errors.New("transient gateway timeout")
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		if run == 1 {
			return &rpc.GammaZeroComputed{SpotUnderlying: 5001}, nil
		}
		// Subsequent runs (the refresh) fail.
		return nil, bonk
	}

	// Establish the cached success.
	job1, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-job1.done
	if job1.result == nil || job1.result.SpotUnderlying != 5001 {
		t.Fatalf("initial kickoff failed: %+v", job1)
	}

	// Past softTTL: refresh kicks, fails.
	pastTTL := now.Add(softTTLRTH + time.Second)
	c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)

	// Wait for the refresh to finish. Then a follow-up call must
	// STILL return the cached success — the refresh's error gets
	// dropped, not promoted.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if computeRuns.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the goroutine's defer close(done) land

	job3, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, pastTTL, 300, compute)
	if job3.result == nil || job3.result.SpotUnderlying != 5001 {
		t.Errorf("after failed refresh: should still see cached success 5001, got %+v", job3.result)
	}
	if job3.err != nil {
		t.Errorf("after failed refresh: cached success should not have err, got %v", job3.err)
	}
}

// TestGammaZeroCache_SnapshotStates exhaustively covers the four
// envelope states (cold / computing / ready / error) the snapshot helper
// must produce — the dashboard generator branches on Status, so a
// regression that loses one of these states silently breaks the UI.
func TestGammaZeroCache_SnapshotStates(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now.Add(10 * time.Second) }

	// nil job — happens only before the first kickoff this NY trading
	// session. Status is Cold, distinct from Computing so a consumer
	// can tell "first caller must kick" from "kick already in flight."
	// Pre-v0.27.9 these were both Computing; the v0.27.3 breadth
	// State enum retired the same side-channel-inference pattern.
	env := c.snapshot(nil, nowFn)
	if env.Status != rpc.GammaZeroStatusCold {
		t.Errorf("nil job snapshot: got %q, want %q (no compute kicked this session must be Cold, not Computing)", env.Status, rpc.GammaZeroStatusCold)
	}

	// Computing
	block := make(chan struct{})
	computingCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		p.Store(42)
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}
	job, _ := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, computingCompute)
	time.Sleep(20 * time.Millisecond)
	env = c.snapshot(job, nowFn)
	if env.Status != rpc.GammaZeroStatusComputing {
		t.Errorf("computing snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusComputing)
	}
	if env.Progress != 42 {
		t.Errorf("progress not propagated: got %d, want 42", env.Progress)
	}
	if env.EtaSeconds <= 0 {
		t.Errorf("eta_seconds should be positive while computing, got %d", env.EtaSeconds)
	}

	// Ready
	close(block)
	<-job.done
	env = c.snapshot(job, nowFn)
	if env.Status != rpc.GammaZeroStatusReady {
		t.Errorf("ready snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusReady)
	}
	if env.Result == nil || env.Result.SpotUnderlying != 5000 {
		t.Errorf("ready result missing: %+v", env.Result)
	}

	// Error — separate cache to avoid same-session singleflight reuse.
	c2 := newGammaZeroCache()
	bonk := errors.New("compute failed: gateway disconnected")
	diagnostic := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 5000,
		AsOf:           now,
		PricedLegCount: 60,
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{PricedLegs: 60},
		},
		CollectionDiagnostics: []rpc.GammaCollectionDiagnostic{{
			Underlying:             "SPX",
			TradingClass:           "SPXW",
			Expiry:                 "2026-06-02",
			RequestedLegs:          60,
			PricedLegs:             60,
			OIMissingLegs:          60,
			OISourceStatus:         gammaOISourceMissing,
			OIGenericTickRequested: true,
		}},
	}
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return diagnostic, bonk
	}
	errJob, _ := c2.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, errCompute)
	<-errJob.done
	env = c2.snapshot(errJob, nowFn)
	if env.Status != rpc.GammaZeroStatusError {
		t.Errorf("error snapshot: got %q, want %q", env.Status, rpc.GammaZeroStatusError)
	}
	if env.Error != bonk.Error() {
		t.Errorf("error message not propagated: got %q, want %q", env.Error, bonk.Error())
	}
	if env.DiagnosticResult == nil {
		t.Fatalf("error diagnostic_result missing")
	}
	if env.DiagnosticResult.PricedLegCount != 60 || len(env.DiagnosticResult.CollectionDiagnostics) != 1 {
		t.Fatalf("error diagnostic_result did not preserve source funnel: %+v", env.DiagnosticResult)
	}
}

// TestFindZeroCrossing covers the four documented outcomes of the
// zero-crossing search: clean crossing, all-positive sweep,
// all-negative sweep, and the no-data degenerate. Linear interpolation
// is asserted against an analytic anchor to catch off-by-one errors.
func TestFindZeroCrossing(t *testing.T) {
	t.Run("clean_crossing", func(t *testing.T) {
		// Construct a profile that's strictly decreasing through zero
		// between spot 4980 (+100) and spot 5000 (-100). Linear interp
		// should land exactly at 4990.
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 1000},
			{Spot: 4980, GEX: 100},
			{Spot: 5000, GEX: -100},
			{Spot: 5100, GEX: -1000},
		}
		zg, sign := findZeroCrossing(profile)
		if zg == nil {
			t.Fatalf("expected non-nil zero gamma, got sign=%q", sign)
		}
		if math.Abs(*zg-4990) > 0.01 {
			t.Errorf("zero crossing at %.4f, want 4990 (±0.01)", *zg)
		}
		if sign != "" {
			t.Errorf("clean crossing should report empty sign, got %q", sign)
		}
	})

	t.Run("all_positive", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 100},
			{Spot: 5000, GEX: 200},
			{Spot: 5100, GEX: 300},
		}
		zg, sign := findZeroCrossing(profile)
		if zg != nil {
			t.Errorf("all-positive should return nil zero gamma, got %v", *zg)
		}
		if sign != "positive" {
			t.Errorf("all-positive sign: got %q, want positive", sign)
		}
	})

	t.Run("all_negative", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: -100},
			{Spot: 5000, GEX: -200},
			{Spot: 5100, GEX: -300},
		}
		zg, sign := findZeroCrossing(profile)
		if zg != nil {
			t.Errorf("all-negative should return nil zero gamma, got %v", *zg)
		}
		if sign != "negative" {
			t.Errorf("all-negative sign: got %q, want negative", sign)
		}
	})

	t.Run("no_data", func(t *testing.T) {
		zg, sign := findZeroCrossing(nil)
		if zg != nil || sign != "no_data" {
			t.Errorf("empty profile: got (%v, %q), want (nil, no_data)", zg, sign)
		}
		zg, sign = findZeroCrossing([]rpc.GammaProfilePoint{{Spot: 5000, GEX: 100}})
		if zg != nil || sign != "no_data" {
			t.Errorf("single-point profile: got (%v, %q), want (nil, no_data)", zg, sign)
		}
		zg, sign = findZeroCrossing([]rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 0},
			{Spot: 5000, GEX: 0},
			{Spot: 5100, GEX: 0},
		})
		if zg != nil || sign != "no_data" {
			t.Errorf("all-zero profile: got (%v, %q), want (nil, no_data)", zg, sign)
		}
	})

	t.Run("exact_zero_at_sample", func(t *testing.T) {
		profile := []rpc.GammaProfilePoint{
			{Spot: 4900, GEX: 100},
			{Spot: 5000, GEX: 0},
			{Spot: 5100, GEX: -100},
		}
		zg, sign := findZeroCrossing(profile)
		if zg == nil {
			t.Fatalf("expected non-nil zero gamma at exact sample, got sign=%q", sign)
		}
		// Exact 0 at index 1: interpolation between (4900,+100) and
		// (5000,0) lands at 5000 exactly.
		if math.Abs(*zg-5000) > 0.01 {
			t.Errorf("exact-zero sample: got %.4f, want 5000", *zg)
		}
	})
}

// TestRemainingEta exercises the progress-derived ETA. Below
// progress=5 the fallback is initial-minus-elapsed; above that
// threshold the formula projects from elapsed × (100-p)/p. Floor at
// 5s, ceiling at 4× the static initial estimate.
func TestRemainingEta(t *testing.T) {
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)
	job := &gammaComputation{startedAt: now, etaSeconds: 300}

	// progress=0 early — fallback to initial - elapsed
	if got := remainingEta(job, now, 0); got != 300 {
		t.Errorf("at start (progress=0): got %d, want 300", got)
	}
	if got := remainingEta(job, now.Add(60*time.Second), 0); got != 240 {
		t.Errorf("at +60s (progress=0): got %d, want 240", got)
	}

	// progress=50 with elapsed=60s — project 60s remaining
	if got := remainingEta(job, now.Add(60*time.Second), 50); got != 60 {
		t.Errorf("progress=50 +60s: got %d, want 60", got)
	}

	// progress=99 — formula yields ~elapsed × 1/99; floor at 5
	if got := remainingEta(job, now.Add(300*time.Second), 99); got != 5 {
		t.Errorf("progress=99 +300s: got %d, want 5 (floor)", got)
	}

	// Runaway compute: progress=10 after 10 minutes → formula says
	// 600 × 90 / 10 = 5400s, which must be capped at 4 × 300 = 1200s.
	if got := remainingEta(job, now.Add(10*time.Minute), 10); got != 1200 {
		t.Errorf("runaway (progress=10 +10m): got %d, want 1200 (cap)", got)
	}

	// Fallback path past the initial estimate also floors at 5
	if got := remainingEta(job, now.Add(10*time.Minute), 0); got != 5 {
		t.Errorf("past initial estimate (progress=0): got %d, want 5 (floor)", got)
	}
}

// TestNYSessionKey is a sanity check that DST-relevant dates produce
// stable keys. New York observes DST; verify the key changes correctly
// across DST boundaries and stays stable within a single trading day.
func TestNYSessionKey(t *testing.T) {
	// 2026-03-08 02:30 EST → 03:30 EDT (spring forward in NY)
	// Both should produce key "2026-03-08" because they're the same
	// calendar date in NY.
	before := time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC) // 01:30 EST
	after := time.Date(2026, 3, 8, 8, 30, 0, 0, time.UTC)  // 04:30 EDT
	if k1, k2 := nySessionKey(before), nySessionKey(after); k1 != k2 {
		t.Errorf("DST spring-forward changed key within a day: %s vs %s", k1, k2)
	}

	// Same UTC moment in late NY day vs early NY next-day: keys differ.
	utcLateNY := time.Date(2026, 5, 17, 3, 0, 0, 0, time.UTC) // 23:00 EDT on May 16
	utcNextNY := time.Date(2026, 5, 17, 5, 0, 0, 0, time.UTC) // 01:00 EDT on May 17
	k1, k2 := nySessionKey(utcLateNY), nySessionKey(utcNextNY)
	if k1 == k2 {
		t.Errorf("NY date rollover not detected: both keys = %s", k1)
	}
	if k1 != "2026-05-16" {
		t.Errorf("late-NY May 16 should map to 2026-05-16, got %s", k1)
	}
	if k2 != "2026-05-17" {
		t.Errorf("early-NY May 17 should map to 2026-05-17, got %s", k2)
	}
}

// TestGammaZeroCache_ClosedSessionNeverRefreshes proves the closed-
// session infinite TTL rule: outside regular U.S. option-data hours
// (overnight + weekends) a cached successful compute is served
// indefinitely with no background refresh, regardless of how long
// it's been since the value was computed.
//
// Load-bearing: under the pre-session-aware constant TTL (5 min), a
// daemon idling overnight Saturday would re-kick a multi-minute
// option fan-out every 5 min — burning gateway slots on a market
// that won't produce fresh quotes anyway. The session-aware
// softTTLClosed = math.MaxInt64 cuts that to zero.
func TestGammaZeroCache_ClosedSessionNeverRefreshes(t *testing.T) {
	c := newGammaZeroCache()
	// Saturday 2026-05-23 10:00 EDT — weekend, SessionClosed.
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed for Saturday, got %v", cls)
	}

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000 + float64(run)}, nil
	}

	// Pre-seed a Saturday compute as if Friday's daemon persisted it
	// to disk and today's boot loaded it. SessionClosed callers must
	// see this cached value; they must NOT kick a fresh compute (no
	// fresh quotes inbound; the fan-out would land garbage IVs).
	persisted := &rpc.GammaZeroComputed{SpotUnderlying: 5001, AsOf: now}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(persisted, rpc.GammaZeroScopeCombined, now)},
	}
	job1 := c.slots[rpc.GammaZeroScopeCombined].current

	// 8 hours later — well past every active-session softTTL but
	// still SessionClosed and still the same NY trading date.
	later := now.Add(8 * time.Hour)
	if cls := gammaClassifySession(later); cls != rpc.SessionClosed {
		t.Fatalf("later instant should still be SessionClosed (Sat 18:00 EDT), got %v", cls)
	}
	if nySessionKey(later) != nySessionKey(now) {
		t.Fatalf("later instant rolled the session key; pick a smaller offset (got %s vs %s)",
			nySessionKey(later), nySessionKey(now))
	}
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, later, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("closed session: should serve cached job1 with no refresh, got fresh=%v job=%p (want job=%p)", fresh2, job2, job1)
	}
	if got := computeRuns.Load(); got != 0 {
		t.Errorf("closed session: compute ran %d times, want 0 (no kick, no refresh)", got)
	}
}

// TestGammaZeroCache_OptionsOpenRefreshesCachedDiagnostic proves the
// option-session boundary trigger: a cached value from outside regular
// option hours is served once at the open but immediately refreshed
// behind it. Non-force pre-open callers do not start a compute.
func TestGammaZeroCache_OptionsOpenRefreshesCachedDiagnostic(t *testing.T) {
	c := newGammaZeroCache()
	// 09:25 ET Tuesday — five minutes before regular options RTH.
	preMarket := time.Date(2026, 5, 19, 13, 25, 0, 0, time.UTC)
	if cls := gammaClassifySession(preMarket); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected gamma SessionClosed at 09:25 ET, got %v", cls)
	}
	// 09:35 ET same Tuesday — five minutes into regular options RTH.
	rth := time.Date(2026, 5, 19, 13, 35, 0, 0, time.UTC)
	if cls := gammaClassifySession(rth); cls != rpc.SessionRTH {
		t.Fatalf("test fixture sanity check: expected gamma SessionRTH at 09:35 ET, got %v", cls)
	}

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		run := computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 6000 + float64(run), AsOf: rth}, nil
	}

	cached := &rpc.GammaZeroComputed{SpotUnderlying: 5001, AsOf: preMarket}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(cached, rpc.GammaZeroScopeCombined, preMarket)},
	}
	job1 := c.slots[rpc.GammaZeroScopeCombined].current

	preJob, preFresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, preMarket, 300, compute)
	if preFresh || preJob != job1 {
		t.Fatalf("pre-open non-force should serve cached diagnostic without kicking: fresh=%v job=%p want %p", preFresh, preJob, job1)
	}
	if got := computeRuns.Load(); got != 0 {
		t.Fatalf("pre-open non-force compute ran %d times, want 0", got)
	}

	// Cross the open. Age is 10 min, well below softTTLRTH. The
	// boundary path still kicks a refresh behind the served value.
	job2, fresh2 := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, rth, 300, compute)
	if fresh2 || job2 != job1 {
		t.Errorf("option-session transition: caller should still see the cached pre-open value, got fresh=%v job=%p (want job=%p)", fresh2, job2, job1)
	}

	// Poll until the refresh promotes — same idiom as the soft-TTL
	// refresh test. The refresh compute is synchronous here, so it
	// resolves on the next call.
	deadline := time.Now().Add(500 * time.Millisecond)
	var job3 *gammaComputation
	for time.Now().Before(deadline) {
		job3, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, rth, 300, compute)
		if job3 != job1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if job3 == job1 {
		t.Fatal("boundary refresh never promoted — kickOrJoin kept returning the pre-open job")
	}
	if job3.result == nil || job3.result.SpotUnderlying != 6001 {
		t.Errorf("boundary refresh: got %+v, want SpotUnderlying=6001 (the refreshed RTH compute)", job3.result)
	}
	if got := computeRuns.Load(); got != 1 {
		t.Errorf("after boundary refresh: compute ran %d times, want 1 (boundary RTH only)", got)
	}
}

// TestClassifySession exercises the four-way classifier on
// representative instants spanning the daily and weekly boundaries.
// Kept here rather than rpc/rpc_test.go because the cache is the
// only caller today and its tests are where any regression would
// show up first.
func TestClassifySession(t *testing.T) {
	// Tuesday 2026-05-19.
	tests := []struct {
		name string
		t    time.Time
		want rpc.SessionClass
	}{
		{"weekday 03:59 ET → closed (before pre open)", time.Date(2026, 5, 19, 7, 59, 0, 0, time.UTC), rpc.SessionClosed},
		{"weekday 04:00 ET → pre", time.Date(2026, 5, 19, 8, 0, 0, 0, time.UTC), rpc.SessionPre},
		{"weekday 09:29 ET → pre", time.Date(2026, 5, 19, 13, 29, 0, 0, time.UTC), rpc.SessionPre},
		{"weekday 09:30 ET → rth", time.Date(2026, 5, 19, 13, 30, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 15:59 ET → rth", time.Date(2026, 5, 19, 19, 59, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 16:00 ET → post", time.Date(2026, 5, 19, 20, 0, 0, 0, time.UTC), rpc.SessionPost},
		{"weekday 19:59 ET → post", time.Date(2026, 5, 19, 23, 59, 0, 0, time.UTC), rpc.SessionPost},
		{"weekday 20:00 ET → closed", time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"saturday 10:00 ET → closed", time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"sunday 10:00 ET → closed", time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
	}
	for _, tc := range tests {
		if got := rpc.ClassifySession(tc.t); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGammaClassifySessionUsesRegularOptionsHours(t *testing.T) {
	tests := []struct {
		name string
		t    time.Time
		want rpc.SessionClass
	}{
		{"weekday 09:25 ET closed", time.Date(2026, 5, 19, 13, 25, 0, 0, time.UTC), rpc.SessionClosed},
		{"weekday 09:30 ET rth", time.Date(2026, 5, 19, 13, 30, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 16:10 ET rth", time.Date(2026, 5, 19, 20, 10, 0, 0, time.UTC), rpc.SessionRTH},
		{"weekday 16:16 ET closed", time.Date(2026, 5, 19, 20, 16, 0, 0, time.UTC), rpc.SessionClosed},
		{"saturday 10:00 ET closed", time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"holiday closed", time.Date(2026, 5, 25, 14, 0, 0, 0, time.UTC), rpc.SessionClosed},
		{"early close after 13:00 ET closed", time.Date(2026, 11, 27, 18, 5, 0, 0, time.UTC), rpc.SessionClosed},
	}
	for _, tc := range tests {
		if got := gammaClassifySession(tc.t); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestGammaZeroCache_OffHoursServesFreshCacheWithoutWarning pins the
// SessionClosed serve-cached-no-kick contract for a fresh persisted
// value: a daemon that's been running through a session boundary (or
// rebooted with today's cache on disk) must serve the cached result
// to off-hours callers and must NOT append cache_stale_off_hours when
// the result is under 24h old.
func TestGammaZeroCache_OffHoursServesFreshCacheWithoutWarning(t *testing.T) {
	c := newGammaZeroCache()
	// Saturday 2026-05-23 10:00 EDT — SessionClosed, weekend.
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC)
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	// Cache holds a result computed 6h ago — fresh for off-hours
	// (well under the 24h stale gate).
	cached := &rpc.GammaZeroComputed{SpotUnderlying: 5001, AsOf: now.Add(-6 * time.Hour)}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(cached, rpc.GammaZeroScopeCombined, now)},
	}
	cachedJob := c.slots[rpc.GammaZeroScopeCombined].current

	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 9999}, nil
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if fresh || job != cachedJob {
		t.Errorf("off-hours w/ fresh cache: got fresh=%v job=%p, want fresh=false job=%p", fresh, job, cachedJob)
	}
	if kicked.Load() != 0 {
		t.Errorf("off-hours must not kick a fresh compute, kicked=%d", kicked.Load())
	}

	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("snapshot status = %q, want ready", env.Status)
	}
	for _, w := range env.Result.Warnings {
		if w == "cache_stale_off_hours" {
			t.Errorf("fresh off-hours cache should not carry cache_stale_off_hours; warnings=%v", env.Result.Warnings)
		}
	}
}

func TestGammaZeroCache_InvalidReadyResultRejected(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 22, 15, 0, 0, 0, time.UTC)
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return &rpc.GammaZeroComputed{
			SpotUnderlying: 743.73,
			LegCount:       1060,
			Profile: []rpc.GammaProfilePoint{
				{Spot: 740, GEX: 0},
				{Spot: 745, GEX: 0},
			},
			AsOf: now,
		}, nil
	}

	job := c.force(context.Background(), rpc.GammaZeroScopeSPY, now, 300, compute)
	<-job.done

	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusError {
		t.Fatalf("snapshot status = %q, want error", env.Status)
	}
	if !strings.Contains(env.Error, "zero gamma_total_abs/profile/top_strikes") {
		t.Fatalf("snapshot error = %q, want invalid-result explanation", env.Error)
	}
}

func TestValidateGammaComputedAllowsLegsWithMagnitude(t *testing.T) {
	err := validateGammaComputed(&rpc.GammaZeroComputed{
		LegCount:       2,
		GammaTotalAbs:  1,
		TopStrikes:     []rpc.StrikeConcentration{{Strike: 740, AbsGEX: 1}},
		Profile:        []rpc.GammaProfilePoint{{Spot: 740, GEX: 1}},
		SpotUnderlying: 743.73,
		AsOf:           time.Now(),
	})
	if err != nil {
		t.Fatalf("validateGammaComputed returned %v, want nil", err)
	}
}

// TestGammaZeroCache_OffHoursStaleCacheGetsWarning proves the
// >24h-old branch of the SessionClosed serve path stamps the
// cache_stale_off_hours warning on the served envelope (via copy-on-
// write — the underlying cached pointer's Warnings must not be
// mutated, so concurrent snapshots don't see drift).
func TestGammaZeroCache_OffHoursStaleCacheGetsWarning(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC) // Sunday 10:00 EDT, SessionClosed
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	// Cache result is 36h old — past the 24h stale gate.
	cached := &rpc.GammaZeroComputed{
		SpotUnderlying: 5001,
		AsOf:           now.Add(-36 * time.Hour),
		Warnings:       []string{"all_iv_derived"},
	}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: newPersistedComputation(cached, rpc.GammaZeroScopeCombined, now)},
	}
	cachedJob := c.slots[rpc.GammaZeroScopeCombined].current

	env := c.snapshot(cachedJob, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusReady {
		t.Fatalf("snapshot status = %q, want ready", env.Status)
	}
	var seen bool
	for _, w := range env.Result.Warnings {
		if w == "cache_stale_off_hours" {
			seen = true
		}
	}
	if !seen {
		t.Errorf("stale off-hours cache must carry cache_stale_off_hours warning; got %v", env.Result.Warnings)
	}
	// Existing warning must survive the dedup'd union.
	var hasOld bool
	for _, w := range env.Result.Warnings {
		if w == "all_iv_derived" {
			hasOld = true
		}
	}
	if !hasOld {
		t.Errorf("existing warning lost during stale-tagging: got %v", env.Result.Warnings)
	}
	// Cache pointer must NOT have been mutated — copy-on-write
	// guarantees concurrent snapshots see the original.
	for _, w := range cached.Warnings {
		if w == "cache_stale_off_hours" {
			t.Errorf("snapshot mutated the cached Warnings slice: %v", cached.Warnings)
		}
	}
}

// TestGammaZeroCache_OffHoursForceAllowsPollToJoin proves that once
// force() kicks an in-flight compute on a closed session, a follow-up
// non-force kickOrJoin returns the same job rather than reporting Cold.
// Without this, a user running `canary gamma --force` off-hours and then
// polling with `canary gamma` would see a fresh Cold message between
// progress updates — confusing and wrong, since the compute IS running.
func TestGammaZeroCache_OffHoursForceAllowsPollToJoin(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed

	block := make(chan struct{})
	defer close(block)
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		<-block
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}

	forced := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if forced == nil {
		t.Fatal("force() returned nil on closed session")
	}

	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if job == nil {
		t.Fatal("off-hours poll after force: expected to join in-flight job, got nil")
	}
	if job != forced {
		t.Errorf("off-hours poll after force: returned a different job (%p) than the in-flight one (%p)", job, forced)
	}
	if fresh {
		t.Errorf("off-hours poll after force: fresh must be false (join, not kick), got true")
	}
}

func TestGammaZeroCache_OffHoursForceErrorStaysVisible(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed
	computeErr := errors.New("zero-gamma: no usable GEX legs")
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return nil, computeErr
	}

	forced := c.force(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	<-forced.done

	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now.Add(time.Minute), 300, compute)
	if job != forced {
		t.Fatalf("off-hours poll after forced error: got job=%p, want forced job=%p", job, forced)
	}
	if fresh {
		t.Fatalf("off-hours poll after forced error: fresh must be false")
	}
	env := c.snapshotForScope(rpc.GammaZeroScopeCombined, job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusError {
		t.Fatalf("snapshot status = %q, want error", env.Status)
	}
	if !strings.Contains(env.Error, computeErr.Error()) {
		t.Fatalf("snapshot error = %q, want %q", env.Error, computeErr.Error())
	}
}

func TestGammaZeroCache_ForceFailureDoesNotPoisonCachedSuccess(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed
	cached := &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX, SpotUnderlying: 5010, AsOf: now.Add(-2 * time.Hour)}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeSPX: {current: newPersistedComputation(cached, rpc.GammaZeroScopeSPX, now)},
	}
	computeErr := errors.New("zero-gamma: no usable GEX legs")
	diagnostic := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 5020,
		AsOf:           now,
		PricedLegCount: 60,
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{PricedLegs: 60},
		},
		CollectionDiagnostics: []rpc.GammaCollectionDiagnostic{{
			Underlying:             "SPX",
			TradingClass:           "SPXW",
			Expiry:                 "2026-06-02",
			RequestedLegs:          60,
			PricedLegs:             60,
			OIMissingLegs:          60,
			OISourceStatus:         gammaOISourceMissing,
			OIGenericTickRequested: true,
		}},
	}
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return diagnostic, computeErr
	}

	forced := c.force(context.Background(), rpc.GammaZeroScopeSPX, now, 300, compute)
	<-forced.done
	if forced.err == nil || !strings.Contains(forced.err.Error(), computeErr.Error()) {
		t.Fatalf("forced diagnostic err = %v, want %v", forced.err, computeErr)
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeSPX, now.Add(time.Minute), 300, compute)
	if fresh {
		t.Fatalf("poll after failed diagnostic must not kick a fresh compute")
	}
	if job == forced {
		t.Fatalf("failed diagnostic became the served cache")
	}
	if job.result == nil || job.result.SpotUnderlying != cached.SpotUnderlying {
		t.Fatalf("served job = %+v, want cached success %+v", job.result, cached)
	}
	env := c.snapshotForScope(rpc.GammaZeroScopeSPX, job, func() time.Time { return now.Add(time.Minute) })
	if env.Status != rpc.GammaZeroStatusReady || env.Result == nil || env.Result.SpotUnderlying != cached.SpotUnderlying {
		t.Fatalf("served envelope = %+v, want cached ready result", env)
	}
	if env.DiagnosticResult == nil {
		t.Fatalf("failed force diagnostic_result missing from preserved ready envelope")
	}
	if env.DiagnosticResult.SpotUnderlying != diagnostic.SpotUnderlying ||
		len(env.DiagnosticResult.CollectionDiagnostics) != 1 ||
		env.DiagnosticResult.CollectionDiagnostics[0].OIMissingLegs != 60 {
		t.Fatalf("diagnostic_result did not preserve failed refresh source evidence: %+v", env.DiagnosticResult)
	}
}

func TestGammaZeroCache_ForceSuccessPromotesOverCachedSuccess(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed
	cached := &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX, SpotUnderlying: 5010, AsOf: now.Add(-2 * time.Hour)}
	c.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeSPX: {current: newPersistedComputation(cached, rpc.GammaZeroScopeSPX, now)},
	}
	block := make(chan struct{})
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		<-block
		return &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX, SpotUnderlying: 5050, AsOf: now}, nil
	}

	forced := c.force(context.Background(), rpc.GammaZeroScopeSPX, now, 300, compute)
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeSPX, now, 300, compute)
	if fresh {
		t.Fatalf("poll while diagnostic refresh is running must not kick a fresh compute")
	}
	if job == forced {
		t.Fatalf("diagnostic refresh should not replace the served cache until it succeeds")
	}
	if job.result == nil || job.result.SpotUnderlying != cached.SpotUnderlying {
		t.Fatalf("served job while force is running = %+v, want cached success", job.result)
	}

	close(block)
	<-forced.done
	deadline := time.After(time.Second)
	for {
		job, _ = c.kickOrJoin(context.Background(), rpc.GammaZeroScopeSPX, now.Add(time.Minute), 300, compute)
		if job == forced && job.result != nil && job.result.SpotUnderlying == 5050 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("forced success never promoted; served job=%p result=%+v forced=%p", job, job.result, forced)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestGammaZeroCache_OffHoursColdReturnsEmpty proves the
// SessionClosed-no-cache branch: with no usable persisted result on
// hand, kickOrJoin must return (nil, false) and snapshot must report
// Cold rather than starting a doomed compute against a closed
// gateway. The dashboard renderer then surfaces "no data yet; try
// after the open."
func TestGammaZeroCache_OffHoursColdReturnsEmpty(t *testing.T) {
	c := newGammaZeroCache()
	now := time.Date(2026, 5, 23, 14, 0, 0, 0, time.UTC) // Saturday, SessionClosed
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}

	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if job != nil {
		t.Errorf("off-hours cold cache: expected nil job, got %p", job)
	}
	if fresh {
		t.Errorf("off-hours cold cache: fresh must be false, got true")
	}
	if kicked.Load() != 0 {
		t.Errorf("off-hours cold cache: must not kick compute, kicked=%d", kicked.Load())
	}
	env := c.snapshot(job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusCold {
		t.Errorf("snapshot status = %q, want cold", env.Status)
	}
}

func TestGammaZeroCache_OffHoursColdReportsRejectedPersistedCache(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC) // Sunday 10:00 EDT, SessionClosed
	if cls := gammaClassifySession(now); cls != rpc.SessionClosed {
		t.Fatalf("test fixture sanity check: expected SessionClosed, got %v", cls)
	}
	asOf := time.Date(2026, 5, 23, 21, 45, 0, 0, time.UTC)
	combined := helperGammaResult(asOf)
	combined.Scope = rpc.GammaZeroScopeCombined
	combined.PerIndex = map[string]*rpc.GammaZeroComputed{
		"SPY": helperGammaResult(asOf),
		"SPX": {
			Scope:          rpc.GammaZeroScopeSPX,
			SpotUnderlying: 7474.07,
			AsOf:           asOf,
			Method:         gammaMethodToken,
			LegCount:       890,
			Profile: []rpc.GammaProfilePoint{
				{Spot: 7000, GEX: 0},
				{Spot: 7500, GEX: 0},
			},
		},
	}
	if err := store.Save(rpc.GammaZeroScopeCombined, nySessionKey(asOf), combined); err != nil {
		t.Fatalf("Save invalid combined cache: %v", err)
	}

	c := newGammaZeroCacheWithStore(store, now, nil)
	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return helperGammaResult(now), nil
	}
	job, fresh := c.kickOrJoin(context.Background(), rpc.GammaZeroScopeCombined, now, 300, compute)
	if job != nil || fresh {
		t.Fatalf("off-hours invalid cache: got job=%p fresh=%v, want no kick", job, fresh)
	}
	if kicked.Load() != 0 {
		t.Fatalf("off-hours invalid cache must not kick compute, kicked=%d", kicked.Load())
	}

	env := c.snapshotForScope(rpc.GammaZeroScopeCombined, job, func() time.Time { return now })
	if env.Status != rpc.GammaZeroStatusCold {
		t.Fatalf("snapshot status = %q, want cold", env.Status)
	}
	if env.ColdReasonCode != "persisted_cache_rejected" {
		t.Fatalf("cold reason code = %q, want persisted_cache_rejected", env.ColdReasonCode)
	}
	for _, want := range []string{"persisted gamma cache", "per_index[SPX]", "zero gamma_total_abs/profile/top_strikes"} {
		if !strings.Contains(env.ColdReason, want) {
			t.Fatalf("cold reason missing %q: %q", want, env.ColdReason)
		}
	}
	if !strings.Contains(env.ColdAction, "--force") {
		t.Fatalf("cold action should suggest --force, got %q", env.ColdAction)
	}
}

// recordingGammaLogger captures formatted log lines for assertion.
// Safe for concurrent use — ensureLoaded may fire from any of the
// racing entry-point callers.
type recordingGammaLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingGammaLogger) Infof(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *recordingGammaLogger) Warnf(format string, args ...any) {
	l.Infof(format, args...)
}

func (l *recordingGammaLogger) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// TestGammaZeroCache_PersistedLoadOncePerScope pins the lazy
// once-per-process persisted read. Construction must not touch the
// store: daemon.New runs before Server.Start acquires the instance
// lock, so every autospawn race loser builds a full Server, and an
// eager load made each loser re-read the store and re-log "loaded
// persisted result" into the shared daemon log (2026-06-09: ~10
// interleaved triples per spawn burst). And N concurrent plus repeated
// entry-point calls must trigger exactly one store read — one INFO
// line — per scope.
func TestGammaZeroCache_PersistedLoadOncePerScope(t *testing.T) {
	dir := t.TempDir()
	store := newGammaZeroStore(dir)
	now := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC) // Tuesday 10:00 ET, RTH
	asOf := now.Add(-5 * time.Minute)                    // within softTTL — no refresh kicks
	for _, scope := range knownGammaScopes {
		r := helperGammaResult(asOf)
		r.Scope = scope
		if scope == rpc.GammaZeroScopeCombined {
			spy := helperGammaResult(asOf)
			spy.Scope = rpc.GammaZeroScopeSPY
			spx := helperGammaResult(asOf)
			spx.Scope = rpc.GammaZeroScopeSPX
			r.PerIndex = map[string]*rpc.GammaZeroComputed{"SPY": spy, "SPX": spx}
		}
		if err := store.Save(scope, nySessionKey(asOf), r); err != nil {
			t.Fatalf("Save scope=%s: %v", scope, err)
		}
	}

	rec := &recordingGammaLogger{}
	c := newGammaZeroCacheWithStore(store, now, rec)
	if got := rec.count("loaded persisted result"); got != 0 {
		t.Fatalf("construction read the store: %d load lines, want 0 (autospawn losers must stay off the store)", got)
	}

	var kicked atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		kicked.Add(1)
		return helperGammaResult(now), nil
	}
	var wg sync.WaitGroup
	for i := range 12 {
		scope := knownGammaScopes[i%len(knownGammaScopes)]
		wg.Go(func() {
			c.kickOrJoin(context.Background(), scope, now, 300, compute)
			c.snapshotCurrent(scope, func() time.Time { return now })
		})
	}
	wg.Wait()
	// Repeated sequential calls after the concurrent wave must not
	// re-read either.
	for _, scope := range knownGammaScopes {
		c.snapshotCurrent(scope, func() time.Time { return now })
	}
	c.IsComputing()

	if kicked.Load() != 0 {
		t.Errorf("fresh same-session persisted results must not kick computes, kicked=%d", kicked.Load())
	}
	for _, scope := range knownGammaScopes {
		// Trailing " session=" keeps scope=spy from matching scope=spy+spx.
		want := fmt.Sprintf("loaded persisted result scope=%s session=", scope)
		if got := rec.count(want); got != 1 {
			t.Errorf("load lines for scope=%s: got %d, want exactly 1", scope, got)
		}
	}
	if got := rec.count("gamma cache:"); got != len(knownGammaScopes) {
		t.Errorf("total gamma cache log lines: got %d, want %d (one load per scope, no warnings)", got, len(knownGammaScopes))
	}
}

// TestGammaZeroCache_RetryBackoffEscalates pins the escalating retry
// gate: consecutive failures double the quiet period (60s, 2m, 4m, …,
// 15m cap) and a success resets it. The flat 60s gate alone turned the
// 2026-06-09 daylong secdef-farm outage into ~700 doomed ~35s computes;
// the streak caps that at ~30.
func TestGammaZeroCache_RetryBackoffEscalates(t *testing.T) {
	c := newGammaZeroCache()
	t0 := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC) // 10:00 ET, RTH

	var computeRuns atomic.Int32
	errCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return nil, errors.New("secdef farm broken")
	}

	scope := rpc.GammaZeroScopeCombined
	j1, fresh := c.kickOrJoin(context.Background(), scope, t0, 300, errCompute)
	<-j1.done
	if !fresh || computeRuns.Load() != 1 {
		t.Fatalf("first kick: fresh=%v runs=%d", fresh, computeRuns.Load())
	}

	// Streak 1 → 60s gate (unchanged base behavior).
	t1 := t0.Add(gammaErrorRetryTTL + time.Second)
	j2, fresh := c.kickOrJoin(context.Background(), scope, t1, 300, errCompute)
	<-j2.done
	if !fresh || computeRuns.Load() != 2 {
		t.Fatalf("second kick past base TTL: fresh=%v runs=%d", fresh, computeRuns.Load())
	}

	// Streak 2 → 2-minute gate: another 61s is NOT enough.
	t2 := t1.Add(gammaErrorRetryTTL + time.Second)
	j3, fresh := c.kickOrJoin(context.Background(), scope, t2, 300, errCompute)
	if fresh || j3 != j2 || computeRuns.Load() != 2 {
		t.Errorf("streak-2 gate must hold at +61s: fresh=%v runs=%d", fresh, computeRuns.Load())
	}

	// 2 minutes past the second failure: allowed again.
	t3 := t1.Add(2*gammaErrorRetryTTL + time.Second)
	j4, fresh := c.kickOrJoin(context.Background(), scope, t3, 300, errCompute)
	<-j4.done
	if !fresh || computeRuns.Load() != 3 {
		t.Errorf("streak-2 gate should release at +2m: fresh=%v runs=%d", fresh, computeRuns.Load())
	}

	// Success resets the streak.
	okCompute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		computeRuns.Add(1)
		return &rpc.GammaZeroComputed{SpotUnderlying: 5050}, nil
	}
	t4 := t3.Add(4*gammaErrorRetryTTL + time.Second) // past streak-3 gate
	j5, fresh := c.kickOrJoin(context.Background(), scope, t4, 300, okCompute)
	<-j5.done
	if !fresh || j5.err != nil {
		t.Fatalf("recovery kick: fresh=%v err=%v", fresh, j5.err)
	}
	c.mu.Lock()
	streak := c.slots[scope].errStreak
	c.mu.Unlock()
	if streak != 0 {
		t.Errorf("success should reset errStreak, got %d", streak)
	}
}

// TestGammaZeroCache_BackoffTable pins the gammaRetryBackoff curve,
// including the softTTLRTH-aligned cap and the shift-overflow guard.
func TestGammaZeroCache_BackoffTable(t *testing.T) {
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, gammaErrorRetryTTL},
		{1, gammaErrorRetryTTL},
		{2, 2 * time.Minute},
		{3, 4 * time.Minute},
		{4, 8 * time.Minute},
		{5, gammaErrorRetryMaxTTL},
		{12, gammaErrorRetryMaxTTL},
		{70, gammaErrorRetryMaxTTL}, // shift overflow guard
	}
	for _, tc := range cases {
		if got := gammaRetryBackoff(tc.streak); got != tc.want {
			t.Errorf("gammaRetryBackoff(%d) = %s, want %s", tc.streak, got, tc.want)
		}
	}
}

// TestGammaZeroCache_SoftTTLRefreshBackoffAfterFailure pins the fix for
// the June 9 respawn storm: a stale-but-good current whose refreshes
// keep failing must NOT respawn a refresh on every poll. The soft-TTL
// trigger condition stays true forever in that state (a failed refresh
// never advances the current success time), so without the streak gate the
// daemon's 1-minute scheduler reaps and respawns a doomed compute per
// tick.
func TestGammaZeroCache_SoftTTLRefreshBackoffAfterFailure(t *testing.T) {
	c := newGammaZeroCache()
	t0 := time.Date(2026, 5, 19, 14, 0, 0, 0, time.UTC)

	var computeRuns atomic.Int32
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		if computeRuns.Add(1) == 1 {
			return &rpc.GammaZeroComputed{SpotUnderlying: 5000}, nil
		}
		return nil, errors.New("gateway sick")
	}

	scope := rpc.GammaZeroScopeCombined
	j1, _ := c.kickOrJoin(context.Background(), scope, t0, 300, compute)
	<-j1.done
	if j1.err != nil {
		t.Fatalf("seed compute failed: %v", j1.err)
	}

	// Past soft TTL: serve stale, spawn refresh (which fails).
	pastTTL := t0.Add(softTTLRTH + time.Second)
	if job, fresh := c.kickOrJoin(context.Background(), scope, pastTTL, 300, compute); fresh || job != j1 {
		t.Fatalf("past softTTL should serve stale current")
	}
	// Wait for the failed refresh to finish its streak accounting.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		streak := c.slots[scope].errStreak
		c.mu.Unlock()
		if streak == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Scheduler-cadence polls within the gate: reap the failed refresh,
	// but do NOT respawn. Before the fix every one of these spawned a
	// fresh doomed compute.
	for i := 1; i <= 3; i++ {
		tick := pastTTL.Add(time.Duration(i) * 10 * time.Second)
		if job, fresh := c.kickOrJoin(context.Background(), scope, tick, 300, compute); fresh || job != j1 {
			t.Fatalf("tick %d: stale current should keep serving without a new kick", i)
		}
	}
	if got := computeRuns.Load(); got != 2 {
		t.Fatalf("within backoff: compute ran %d times, want 2 (seed + one failed refresh)", got)
	}

	// Past the streak-1 gate (60s from the refresh kickoff): one more
	// attempt is allowed.
	retryAt := pastTTL.Add(gammaErrorRetryTTL + time.Second)
	if _, fresh := c.kickOrJoin(context.Background(), scope, retryAt, 300, compute); fresh {
		t.Fatalf("soft-TTL refresh respawn must stay a background refresh, not a fresh current kick")
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if computeRuns.Load() == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := computeRuns.Load(); got != 3 {
		t.Fatalf("past backoff: compute ran %d times, want 3 (gate released exactly once)", got)
	}

	// resetRetryBackoff (gateway reconnect) re-arms immediately.
	c.resetRetryBackoff()
	c.mu.Lock()
	streak := c.slots[scope].errStreak
	c.mu.Unlock()
	if streak != 0 {
		t.Errorf("resetRetryBackoff should zero the streak, got %d", streak)
	}
}
