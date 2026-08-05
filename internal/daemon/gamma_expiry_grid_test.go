package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

func testClassedGrid(dates ...string) map[string][]ibkrlib.ExpiryClassedStrikes {
	out := make(map[string][]ibkrlib.ExpiryClassedStrikes, len(dates))
	for _, d := range dates {
		out[d] = []ibkrlib.ExpiryClassedStrikes{{
			TradingClass: "SPY",
			Strikes:      []float64{480, 490, 500, 510, 520},
		}}
	}
	return out
}

// TestExpiryGridStoreRoundTrip pins the fallback lifecycle: a noted
// fetch is served back within the age window, survives a process
// restart via the per-symbol JSON file, and expires past
// gammaExpiryGridMaxAge.
func TestExpiryGridStoreRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC)
	grid := testClassedGrid("2026-06-10", "2026-06-11", "2026-06-12", "2026-06-19")

	g := newExpiryGridStore(dir)
	if _, _, ok := g.fallback("SPY", now); ok {
		t.Fatal("cold store should have no fallback")
	}
	if err := g.noteFetched("SPY", grid, now); err != nil {
		t.Fatalf("noteFetched: %v", err)
	}
	got, asOf, ok := g.fallback("SPY", now.Add(24*time.Hour))
	if !ok || !asOf.Equal(now) || len(got) != len(grid) {
		t.Fatalf("in-memory fallback: ok=%v asOf=%s len=%d", ok, asOf, len(got))
	}

	// Fresh store over the same dir = daemon restart mid-outage.
	g2 := newExpiryGridStore(dir)
	got, asOf, ok = g2.fallback("SPY", now.Add(48*time.Hour))
	if !ok || !asOf.Equal(now) || len(got) != len(grid) {
		t.Fatalf("disk fallback after restart: ok=%v asOf=%s len=%d", ok, asOf, len(got))
	}

	// Past the age window: no fallback.
	if _, _, ok := g2.fallback("SPY", now.Add(gammaExpiryGridMaxAge+time.Hour)); ok {
		t.Fatal("fallback should expire past gammaExpiryGridMaxAge")
	}

	// Nil store is a no-op on both paths.
	var nilStore *expiryGridStore
	if err := nilStore.noteFetched("SPY", grid, now); err != nil {
		t.Fatalf("nil noteFetched: %v", err)
	}
	if _, _, ok := nilStore.fallback("SPY", now); ok {
		t.Fatal("nil store should have no fallback")
	}
}

func TestExpiryGridStoreUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openMarketTestCoreStore(t)
	now := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC)
	grid := testClassedGrid("2026-06-10", "2026-06-11", "2026-06-12", "2026-06-19")
	g := newExpiryGridStore(legacyDir)
	if err := g.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	if err := g.noteFetched("SPY", grid, now); err != nil {
		t.Fatalf("noteFetched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, expiryGridFilename("SPY"))); !os.IsNotExist(err) {
		t.Fatalf("runtime save touched legacy file: %v", err)
	}

	restarted := newExpiryGridStore(legacyDir)
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	got, asOf, ok := restarted.fallback("SPY", now.Add(24*time.Hour))
	if !ok || !asOf.Equal(now) || len(got) != len(grid) {
		t.Fatalf("SQLite fallback: ok=%v asOf=%s len=%d", ok, asOf, len(got))
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: expiryGridAuthorityScope("SPY"), Source: expiryGridSource, Kind: expiryGridObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
}

// TestExpiryGridStoreRejectsPartialGrid pins the poisoning guard:
// fetchOptionExpiriesData returns partial frames as success when the
// end marker times out, and a flapping farm must not overwrite a good
// grid with a fragment that then serves as the fallback for days.
func TestExpiryGridStoreRejectsPartialGrid(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC)
	full := testClassedGrid(
		"2026-06-10", "2026-06-11", "2026-06-12", "2026-06-15", "2026-06-16",
		"2026-06-17", "2026-06-18", "2026-06-19", "2026-06-22", "2026-06-23",
	)
	partial := testClassedGrid("2026-06-10")

	g := newExpiryGridStore(t.TempDir())
	if err := g.noteFetched("SPY", full, now); err != nil {
		t.Fatalf("noteFetched full: %v", err)
	}
	if err := g.noteFetched("SPY", partial, now.Add(time.Hour)); err == nil {
		t.Fatal("partial grid should be rejected, not stored")
	}
	got, asOf, ok := g.fallback("SPY", now.Add(2*time.Hour))
	if !ok || len(got) != len(full) || !asOf.Equal(now) {
		t.Fatalf("fallback should still serve the full grid: ok=%v len=%d asOf=%s", ok, len(got), asOf)
	}

	// Legitimate shrinkage (an expiry rolling off) is accepted.
	slightlySmaller := testClassedGrid(
		"2026-06-11", "2026-06-12", "2026-06-15", "2026-06-16", "2026-06-17",
		"2026-06-18", "2026-06-19", "2026-06-22", "2026-06-23",
	)
	if err := g.noteFetched("SPY", slightlySmaller, now.Add(3*time.Hour)); err != nil {
		t.Fatalf("legitimate shrinkage should be accepted: %v", err)
	}
}

type stubExpiryFetcher struct {
	grid  map[string][]ibkrlib.ExpiryClassedStrikes
	err   error
	calls int
}

func (s *stubExpiryFetcher) FetchOptionExpiryStrikesClassed(string, time.Duration) (map[string][]ibkrlib.ExpiryClassedStrikes, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.grid, nil
}

// TestBuildPickedExpirationsGridFallback pins the June-9 fix end to
// end at the selection layer: a live secdef failure falls back to the
// last successful grid (with fallback info reporting its age), while a
// cold cache propagates the live error unchanged.
func TestBuildPickedExpirationsGridFallback(t *testing.T) {
	t.Parallel()
	spotAt := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC) // Tue 10:00 ET
	grid := testClassedGrid("2026-06-10", "2026-06-11", "2026-06-12", "2026-06-19")
	grids := newExpiryGridStore(t.TempDir())

	// Live success seeds the store.
	live := &stubExpiryFetcher{grid: grid}
	picked, fb, err := buildPickedExpirations(live, "SPY", spotAt, 3, grids, gammaLogf{})
	if err != nil || fb != nil || len(picked) == 0 {
		t.Fatalf("live path: err=%v fb=%v picked=%d", err, fb, len(picked))
	}

	// Next session: the farm is broken; the cached grid carries the
	// compute, and the fallback info buckets the age.
	nextDay := spotAt.Add(24 * time.Hour)
	farmDown := errors.New("option expiries timeout for SPY after 30s")
	dead := &stubExpiryFetcher{err: farmDown}
	picked, fb, err = buildPickedExpirations(dead, "SPY", nextDay, 3, grids, gammaLogf{})
	if err != nil || len(picked) == 0 {
		t.Fatalf("fallback path: err=%v picked=%d", err, len(picked))
	}
	if fb == nil || !errors.Is(fb.liveErr, farmDown) || fb.staleDays(nextDay) != 1 {
		t.Fatalf("fallback info: %+v", fb)
	}

	// Cold cache: the live error must surface unchanged.
	coldGrids := newExpiryGridStore(t.TempDir())
	if _, _, err := buildPickedExpirations(dead, "SPY", nextDay, 3, coldGrids, gammaLogf{}); !errors.Is(err, farmDown) {
		t.Fatalf("cold-cache error = %v, want the live fetch error", err)
	}
}

// TestGammaQualityExpiriesStaleGates pins the warning→gate mapping: a
// grid within the pass window stays rankable with disclosure; an older
// grid degrades the signal to context-only rather than masquerading as
// full coverage.
func TestGammaQualityExpiriesStaleGates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)

	fresh := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	fresh.Warnings = append(fresh.Warnings, "expiries_stale:1d")
	annotateGammaQuality(fresh, now)
	if got := fresh.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("1d-stale grid rankability = %q, want rankable with disclosure: %+v", got, fresh.Quality)
	}
	var sawPass bool
	for _, gate := range fresh.Quality.Gates {
		if gate.Name == "expiry_grid" && gate.Status == rpc.GammaQualityGatePass {
			sawPass = true
		}
	}
	if !sawPass {
		t.Fatalf("expected a pass-with-disclosure expiry_grid gate: %+v", fresh.Quality.Gates)
	}

	old := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	old.Warnings = append(old.Warnings, "expiries_stale:5d")
	annotateGammaQuality(old, now)
	if got := old.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("5d-stale grid rankability = %q, want context_only: %+v", got, old.Quality)
	}

	mangled := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	mangled.Warnings = append(mangled.Warnings, "expiries_stale:soon")
	annotateGammaQuality(mangled, now)
	if got := mangled.Quality.Rankability; got != rpc.GammaRankabilityContextOnly {
		t.Fatalf("unparseable age must fail closed to context_only, got %q", got)
	}
}
