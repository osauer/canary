package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A fetch failure that repeats on every regime read is one incident: the
// first attempt warns, repeats stay quiet, a changed failure warns again, and
// the recovery bookends with the attempt count. (DTB3 logged the same
// month-start failure 146 times on 2026-10-01.)
func TestRegimeSeriesCacheRepeatedFailureWarnsOnce(t *testing.T) {
	var warned []string
	cache := newRegimeSeriesCache(t.TempDir(), func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	})
	recent := []regimeSeriesPoint{{Date: time.Now().Add(-24 * time.Hour), Value: 4.0}}
	cache.put("DTB3", recent, time.Now().Add(-13*time.Hour)) // outside fresh window, inside fallback age

	empty := errors.New("month 202610: treasury XML contained no usable 13-week bill observations")
	failing := func(context.Context, string) ([]regimeSeriesPoint, error) { return nil, empty }
	for range 5 {
		if _, err := cache.fetch(context.Background(), "DTB3", failing); err != nil {
			t.Fatalf("fetch with fallback: %v", err)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "serving cached series") {
		t.Fatalf("repeated failure warnings = %v, want exactly one", warned)
	}

	// A different failure is a new incident.
	timeout := errors.New("Get treasury: context deadline exceeded")
	if _, err := cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) { return nil, timeout }); err != nil {
		t.Fatalf("fetch with fallback: %v", err)
	}
	if len(warned) != 2 || !strings.Contains(warned[1], "deadline exceeded") {
		t.Fatalf("changed failure not warned: %v", warned)
	}
	// Its repeats stay quiet too.
	for range 3 {
		_, _ = cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) { return nil, timeout })
	}
	if len(warned) != 2 {
		t.Fatalf("repeat of the changed failure warned: %v", warned)
	}

	// Recovery bookends with the attempt count of the open incident.
	fresh := []regimeSeriesPoint{{Date: time.Now().Add(-2 * time.Hour), Value: 4.1}}
	got, err := cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) { return fresh, nil })
	if err != nil || len(got) != 1 {
		t.Fatalf("recovered fetch: len %d err %v", len(got), err)
	}
	if len(warned) != 3 || !strings.Contains(warned[2], "recovered after 4 failed attempts") {
		t.Fatalf("recovery bookend missing or wrong: %v", warned)
	}

	// A one-off failure followed by success needs no bookend.
	cache.put("DTB3", fresh, time.Now().Add(-13*time.Hour))
	_, _ = cache.fetch(context.Background(), "DTB3", failing)
	_, _ = cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) { return fresh, nil })
	if len(warned) != 4 || !strings.Contains(warned[3], "serving cached series") {
		t.Fatalf("one-off failure handling: %v", warned)
	}

	// Series are independent incidents.
	if _, err := cache.fetch(context.Background(), "RIFSPPFAAD90NB", failing); err == nil {
		t.Fatal("fetch with no fallback: want error")
	}
	if len(warned) != 5 || !strings.Contains(warned[4], "no usable cached fallback") {
		t.Fatalf("other series not warned on first failure: %v", warned)
	}
}
