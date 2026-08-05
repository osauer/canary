package daemon

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestRegimeHistoryCacheFallsBackOnThinLiveBars(t *testing.T) {
	cache := newRegimeHistoryCache(t.TempDir())
	ctx := context.Background()
	good := makeBars(60, 79.5)

	calls := 0
	fetcher := func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		calls++
		switch calls {
		case 1:
			return good, nil
		default:
			return makeBars(30, 1), nil
		}
	}

	first, err := cache.fetch(ctx, "HYG", HYGLookbackDays, fetcher)
	if err != nil {
		t.Fatalf("first fetch error: %v", err)
	}
	if len(first) != len(good) {
		t.Fatalf("first fetch bars=%d, want %d", len(first), len(good))
	}

	cache.freshFor = 0
	second, err := cache.fetch(ctx, "HYG", HYGLookbackDays, fetcher)
	if err != nil {
		t.Fatalf("second fetch error: %v", err)
	}
	if len(second) != len(good) {
		t.Fatalf("second fetch bars=%d, want cached %d after thin live response", len(second), len(good))
	}
	if second[len(second)-1].Close != 79.5 {
		t.Fatalf("second close=%v, want cached good baseline", second[len(second)-1].Close)
	}
}

func TestRegimeHistoryCachePersistsFallbackAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	good := makeBars(10, 150)

	firstCache := newRegimeHistoryCache(dir)
	if _, err := firstCache.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return good, nil
	}); err != nil {
		t.Fatalf("seed fetch error: %v", err)
	}

	secondCache := newRegimeHistoryCache(dir)
	got, err := secondCache.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return nil, errors.New("hmds unavailable")
	})
	if err != nil {
		t.Fatalf("fallback fetch error: %v", err)
	}
	if len(got) != len(good) {
		t.Fatalf("bars=%d, want persisted cached %d", len(got), len(good))
	}
}

func TestRegimeHistoryCacheUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openMarketTestCoreStore(t)
	ctx := context.Background()
	good := makeBars(10, 150)

	first := newRegimeHistoryCache(legacyDir)
	if err := first.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	if _, err := first.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return good, nil
	}); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	entries, err := os.ReadDir(legacyDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy cache was written: entries=%v err=%v", entries, err)
	}

	restarted := newRegimeHistoryCache(legacyDir)
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	got, err := restarted.fetch(ctx, "USD.JPY", USDJPYLookbackDays, func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) {
		return nil, errors.New("hmds unavailable")
	})
	if err != nil || len(got) != len(good) {
		t.Fatalf("SQLite fallback bars=%d err=%v", len(got), err)
	}
	observations, err := authority.ListObservations(ctx, corestore.ObservationQuery{
		ScopeKey: regimeHistoryAuthorityScope("USD.JPY", USDJPYLookbackDays),
		Source:   regimeHistorySource, Kind: regimeHistoryObservationKind,
	})
	if err != nil || len(observations) != 0 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
}
