package daemon

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestRegimeSeriesCacheUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openMarketTestCoreStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	points := makeSeries(21, 3.50)
	points[20].Value = 3.80

	first := newRegimeSeriesCache(legacyDir)
	if err := first.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	first.put(fredSeriesHYOAS, points, now.Add(-13*time.Hour))
	entries, err := os.ReadDir(legacyDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy cache was written: entries=%v err=%v", entries, err)
	}

	restarted := newRegimeSeriesCache(legacyDir)
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	restarted.freshFor = time.Nanosecond
	got, err := restarted.fetch(ctx, fredSeriesHYOAS, func(context.Context, string) ([]regimeSeriesPoint, error) {
		return nil, errors.New("official source unavailable")
	})
	if err != nil || len(got) != len(points) || got[len(got)-1].Value != 3.80 {
		t.Fatalf("SQLite fallback points=%d err=%v", len(got), err)
	}
	observations, err := authority.ListObservations(ctx, corestore.ObservationQuery{
		ScopeKey: regimeSeriesAuthorityScope(fredSeriesHYOAS),
		Source:   regimeSeriesObservationSource(fredSeriesHYOAS), Kind: regimeSeriesObservationKind,
	})
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
}
