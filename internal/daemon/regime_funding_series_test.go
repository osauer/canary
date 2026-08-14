package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func treasuryFeedXML(dates []time.Time) string {
	var b strings.Builder
	b.WriteString("<feed>")
	for i, d := range dates {
		fmt.Fprintf(&b,
			"<entry><content><properties><INDEX_DATE>%s</INDEX_DATE><ROUND_B1_CLOSE_13WK_2>%.2f</ROUND_B1_CLOSE_13WK_2></properties></content></entry>",
			d.Format("2006-01-02T15:04:05"), 4.20+float64(i)*0.01)
	}
	b.WriteString("</feed>")
	return b.String()
}

// The bill leg's two-month merge is all-or-error: a month that fails to fetch
// must fail the whole read, because a shorter merged series would be cached as
// a complete fresh success and pin the derived spread to the older month for
// the cache's full fresh window.
func TestFetchTreasury13WeekBillAllOrError(t *testing.T) {
	now := time.Now().UTC()
	prevMonth := now.AddDate(0, -1, 0).Format("200601")
	curMonth := now.Format("200601")
	prevStart, _ := time.Parse("200601", prevMonth)
	curStart, _ := time.Parse("200601", curMonth)
	prevDates := []time.Time{prevStart.AddDate(0, 0, 10), prevStart.AddDate(0, 0, 12)}
	curDates := []time.Time{curStart.AddDate(0, 0, 1), curStart.AddDate(0, 0, 2)}

	var curMonthMode string // "ok" | "http500" | "empty"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		month := r.URL.Query().Get("month")
		switch {
		case month == prevMonth:
			fmt.Fprint(w, treasuryFeedXML(prevDates))
		case month == curMonth && curMonthMode == "ok":
			fmt.Fprint(w, treasuryFeedXML(curDates))
		case month == curMonth && curMonthMode == "empty":
			fmt.Fprint(w, "<feed></feed>")
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	orig := treasuryBillRatesXMLURL
	treasuryBillRatesXMLURL = func(month string) string { return srv.URL + "?month=" + month }
	t.Cleanup(func() { treasuryBillRatesXMLURL = orig })

	curMonthMode = "ok"
	points, err := fetchTreasury13WeekBill(context.Background())
	if err != nil {
		t.Fatalf("both months ok: unexpected error %v", err)
	}
	if len(points) != len(prevDates)+len(curDates) {
		t.Fatalf("both months ok: got %d points, want %d", len(points), len(prevDates)+len(curDates))
	}
	if last := points[len(points)-1].Date; !last.Equal(curDates[len(curDates)-1]) {
		t.Fatalf("both months ok: series ends %s, want %s", last, curDates[len(curDates)-1])
	}

	for _, mode := range []string{"http500", "empty"} {
		curMonthMode = mode
		points, err = fetchTreasury13WeekBill(context.Background())
		if err == nil {
			t.Fatalf("current month %s: got %d points and nil error, want error", mode, len(points))
		}
		if !strings.Contains(err.Error(), "month "+curMonth) {
			t.Fatalf("current month %s: error %q does not name the failed month", mode, err)
		}
	}
}

// A fetch whose newest observation is older than the cached entry's must not
// replace it: the cache keeps the newer copy, re-stamps its fetch time, and
// the caller's read uses the retained series, not the regressed fetch.
func TestRegimeSeriesCacheRefusesRegression(t *testing.T) {
	var warned []string
	cache := newRegimeSeriesCache(t.TempDir(), func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	})
	day := func(d int) time.Time { return time.Date(2026, 8, d, 0, 0, 0, 0, time.UTC) }
	newer := []regimeSeriesPoint{{Date: day(1), Value: 4.0}, {Date: day(11), Value: 4.1}}
	older := []regimeSeriesPoint{{Date: day(1), Value: 4.0}}

	longAgo := time.Now().Add(-48 * time.Hour)
	if got := cache.put("DTB3", newer, longAgo); len(got) != 2 {
		t.Fatalf("seed put returned %d points, want 2", len(got))
	}

	got, err := cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) {
		return older, nil
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 2 || !got[len(got)-1].Date.Equal(day(11)) {
		t.Fatalf("fetch returned regressed series (len %d), want retained series ending %s", len(got), day(11))
	}
	if len(warned) == 0 || !strings.Contains(warned[len(warned)-1], "refusing") {
		t.Fatalf("expected a refusal warning, got %v", warned)
	}

	// The kept entry was re-stamped as freshly fetched: the next read serves
	// it from the fresh window without invoking the fetcher.
	got, err = cache.fetch(context.Background(), "DTB3", func(context.Context, string) ([]regimeSeriesPoint, error) {
		return nil, errors.New("fetcher must not run inside the fresh window")
	})
	if err != nil || len(got) != 2 {
		t.Fatalf("post-refusal fetch: len %d err %v, want retained series from fresh cache", len(got), err)
	}
}

// A truncated-but-parsable upstream response is accepted by the CSV fetcher —
// truncation at a row boundary is indistinguishable from real publication lag
// there — and the cache flags it at write time against the declared
// business-daily cadence. This is the cold-cache case the regression guard
// cannot catch: no better entry exists, so without the cadence check the
// short series would be stored as an ordinary fresh success.
func TestRegimeSeriesCacheFlagsBehindCadenceWrite(t *testing.T) {
	stale := time.Now().UTC().AddDate(0, 0, -15)
	body := "observation_date,BAMLH0A0HYM2\n" +
		stale.AddDate(0, 0, -1).Format("2006-01-02") + ",3.10\n" +
		stale.Format("2006-01-02") + ",3.15\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	var warned []string
	cache := newRegimeSeriesCache(t.TempDir(), func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	})
	points, err := cache.fetch(context.Background(), "BAMLH0A0HYM2", func(ctx context.Context, _ string) ([]regimeSeriesPoint, error) {
		return fetchCSVSeries(ctx, srv.URL, "BAMLH0A0HYM2", "2006-01-02")
	})
	if err != nil || len(points) != 2 {
		t.Fatalf("fetch: len %d err %v, want the short series accepted", len(points), err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "behind a business-daily publication cadence") {
		t.Fatalf("behind-cadence write not flagged, got %v", warned)
	}
}

// A failed fetch that lands on the cached fallback logs the failure and the
// age of what is being served; a failed fetch with no fallback logs too. The
// silent path is what let a transient upstream failure go undiagnosed.
func TestRegimeSeriesCacheLogsFallback(t *testing.T) {
	var warned []string
	cache := newRegimeSeriesCache(t.TempDir(), func(format string, args ...any) {
		warned = append(warned, fmt.Sprintf(format, args...))
	})
	recent := []regimeSeriesPoint{{Date: time.Now().Add(-24 * time.Hour), Value: 4.0}}
	cache.put("DTB3", recent, time.Now().Add(-13*time.Hour)) // outside fresh window, inside fallback age

	failing := func(context.Context, string) ([]regimeSeriesPoint, error) {
		return nil, errors.New("HTTP 500")
	}
	if _, err := cache.fetch(context.Background(), "DTB3", failing); err != nil {
		t.Fatalf("fetch with fallback: %v", err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "serving cached series") {
		t.Fatalf("fallback warning missing, got %v", warned)
	}

	warned = nil
	if _, err := cache.fetch(context.Background(), "RIFSPPFAAD90NB", failing); err == nil {
		t.Fatal("fetch with no fallback: want error")
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "no usable cached fallback") {
		t.Fatalf("no-fallback warning missing, got %v", warned)
	}
}
