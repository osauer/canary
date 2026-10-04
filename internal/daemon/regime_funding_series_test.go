package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fundingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f fundingRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestRegimeFundingTreasuryLatencyBudget(t *testing.T) {
	// A real Treasury response needed 18.52s. Inspect effective deadlines in
	// the transport instead of making a hermetic test wait for that latency.
	// This catches either the old HTTP timeout or the old enclosing budget.
	day := time.Now().UTC().Truncate(24 * time.Hour)
	requests := make(chan time.Duration, 2)
	orig := regimeTreasuryHTTPClient
	client := *orig
	client.Transport = fundingRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) < 20*time.Second {
			return nil, errors.New("Treasury request budget cannot accommodate a 20-second response")
		}
		requests <- time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(treasuryFeedXML([]time.Time{day}))),
			Header:     make(http.Header),
		}, nil
	})
	regimeTreasuryHTTPClient = &client
	t.Cleanup(func() { regimeTreasuryHTTPClient = orig })

	deps := &regimeDeps{officialSeries: func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
		if seriesID == fredSeriesTBill3M {
			return fetchOfficialRegimeSeries(ctx, seriesID)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 12*time.Second {
			return nil, errors.New("commercial-paper fetch lost its existing deadline")
		}
		return []regimeSeriesPoint{{Date: day, Value: 4.3}}, nil
	}}
	out := fetchRegimeFundingStress(context.Background(), deps)
	if out.Status != "ok" || out.SpreadBps == nil {
		t.Fatalf("funding should accept the bounded Treasury response: %s %s", out.Status, out.ErrorMessage)
	}
	if len(requests) != 2 {
		t.Fatalf("Treasury month requests = %d, want 2", len(requests))
	}
	for range 2 {
		if budget := <-requests; budget > 25*time.Second {
			t.Fatalf("Treasury HTTP deadline exceeds its 25-second bound: %s", budget)
		}
	}
}

func TestRegimeFundingParentDeadlineRemainsBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	started := make(chan string, 2)
	stopped := make(chan struct{}, 2)
	deps := &regimeDeps{officialSeries: func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.Equal(parentDeadline) {
			t.Errorf("%s did not inherit the shorter parent deadline", seriesID)
		}
		started <- seriesID
		<-ctx.Done()
		stopped <- struct{}{}
		return nil, ctx.Err()
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		out := fetchRegimeFundingStress(ctx, deps)
		if out.Status != "error" || out.SpreadBps != nil {
			t.Errorf("cancelled funding fetch returned a measurement: %s", out.Status)
		}
	}()
	for range 2 {
		<-started
	}
	cancel()
	<-done
	for range 2 {
		<-stopped
	}
}

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

func TestTreasuryBillMonthsAcrossShorterMonths(t *testing.T) {
	for _, tc := range []struct {
		date string
		want [2]string
	}{
		{"2026-03-31", [2]string{"202602", "202603"}},
		{"2026-05-31", [2]string{"202604", "202605"}},
		{"2026-08-31", [2]string{"202607", "202608"}},
		{"2026-01-31", [2]string{"202512", "202601"}},
	} {
		t.Run(tc.date, func(t *testing.T) {
			now, err := time.Parse("2006-01-02", tc.date)
			if err != nil {
				t.Fatal(err)
			}
			if got := treasuryBillMonths(now); got != tc.want {
				t.Fatalf("months = %v, want %v", got, tc.want)
			}
		})
	}
}

// The bill leg's two-month merge is all-or-error: a month that fails to fetch
// must fail the whole read, because a shorter merged series would be cached as
// a complete fresh success and pin the derived spread to the older month for
// the cache's full fresh window.
func TestFetchTreasury13WeekBillAllOrError(t *testing.T) {
	now := time.Now().UTC()
	months := treasuryBillMonths(now)
	prevMonth, curMonth := months[0], months[1]
	prevStart, _ := time.Parse("200601", prevMonth)
	curStart, _ := time.Parse("200601", curMonth)
	prevDates := []time.Time{prevStart.AddDate(0, 0, 10), prevStart.AddDate(0, 0, 12)}
	curDates := []time.Time{curStart.AddDate(0, 0, 1), curStart.AddDate(0, 0, 2)}

	var curMonthMode string // "ok" | "http500" | "empty"
	prevMonthMode := "ok"   // "ok" | "empty"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		month := r.URL.Query().Get("month")
		switch {
		case month == prevMonth && prevMonthMode == "empty":
			fmt.Fprint(w, "<feed></feed>")
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

	curMonthMode = "http500"
	points, err = fetchTreasury13WeekBill(context.Background())
	if err == nil {
		t.Fatalf("current month http500: got %d points and nil error, want error", len(points))
	}
	if !strings.Contains(err.Error(), "month "+curMonth) {
		t.Fatalf("current month http500: error %q does not name the failed month", err)
	}

	// A current-month file with no print yet is how Treasury publishes the
	// first days of every month; the series then ends with the previous
	// month's last print rather than failing the read.
	curMonthMode = "empty"
	points, err = fetchTreasury13WeekBill(context.Background())
	if err != nil {
		t.Fatalf("empty current month: unexpected error %v", err)
	}
	if len(points) != len(prevDates) || !points[len(points)-1].Date.Equal(prevDates[len(prevDates)-1]) {
		t.Fatalf("empty current month: got %d points ending %v, want the %d previous-month points", len(points), points[len(points)-1].Date, len(prevDates))
	}

	// The previous month must still fetch and carry prints: an empty previous
	// month is a truncated feed, and an empty current month cannot excuse it.
	prevMonthMode = "empty"
	if _, err = fetchTreasury13WeekBill(context.Background()); err == nil || !strings.Contains(err.Error(), "month "+prevMonth) {
		t.Fatalf("empty previous month: err=%v, want an error naming %s", err, prevMonth)
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
