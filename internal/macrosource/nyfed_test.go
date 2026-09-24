package macrosource

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func nyfedFixture(month time.Time, releaseDay int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p>All releases all Eastern Time.</p><td class="ts-data-table-head"><div>%s</div></td>`, month.Format("January 2006"))
	fmt.Fprintf(&b, `<a href="/research/calendars/i-%s.html">NEXT MONTH</a>`, strings.ToLower(month.AddDate(0, 1, 0).Format("Jan06")))
	b.WriteString(`<table class="research-table-1col greyborder">`)
	for day := month; day.Month() == month.Month(); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		fmt.Fprintf(&b, `<td><div>%02d<br/>`, day.Day())
		if day.Day() == releaseDay {
			b.WriteString(`<span><a href="https://www.bls.gov/news.release/cpi.toc.htm">Synthetic inflation release</a><br/>(08:30)<br/><br/></span>`)
		}
		b.WriteString(`</div></td>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

func TestNYFedCalendarPreservesBackupProvenanceAndRejectsMissingDays(t *testing.T) {
	spec := sourceSpec(t, "nyfed-calendar")
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	payload := nyfedFixture(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 11)
	batch, err := Parse(spec, []byte(payload), now)
	if err != nil {
		t.Fatal(err)
	}
	e := batch.Events[0]
	if len(batch.Events) != 1 || e.SourceID != "nyfed-calendar" || e.SourceURL != spec.URL || !e.ScheduledAt.IsZero() || e.TimePrecision != "source_label" || e.TimeLabel != "08:30" || batch.WindowStart != "2026-09-01" || batch.WindowEnd != "2026-09-30" {
		t.Fatal("backup lost identity, Eastern time or bounded coverage")
	}
	for _, bad := range []string{
		strings.Replace(payload, "all Eastern Time", "all local times", 1),
		strings.Replace(payload, "<td><div>09<br/></div></td>", "", 1),
		strings.Replace(payload, "(08:30)", "(TBD)", 1),
		strings.Replace(payload, "(08:30)", "(25:30)", 1),
		strings.Replace(payload, "</table>", "", 1),
	} {
		if _, err := Parse(spec, []byte(bad), now); err == nil {
			t.Fatal("incomplete calendar replaced previous evidence")
		}
	}
}

func TestNYFedMonthEndUsesOnlyPublishedNextMonthAndFailsClosed(t *testing.T) {
	spec := sourceSpec(t, "nyfed-calendar")
	month := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	first, next := nyfedFixture(month, 11), nyfedFixture(month.AddDate(0, 1, 0), 2)
	now := time.Date(2026, 9, 29, 19, 0, 0, 0, time.UTC)
	client := NewClient()
	calls := 0
	failNext := false
	client.HTTP.Transport = publicTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatal("calendar request carried credentials")
		}
		data, status := first, 200
		if req.URL.String() != spec.URL {
			if req.URL.String() != "https://www.newyorkfed.org/research/calendars/i-oct26.html" {
				t.Fatal("collector followed an arbitrary link")
			}
			data = next
			if failNext {
				status = 403
			}
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(data)), Request: req}, nil
	})
	batch, err := client.Fetch(context.Background(), spec, now, Batch{})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(batch.Events) != 2 || batch.WindowEnd != "2026-10-31" || batch.Events[1].SourceURL != "https://www.newyorkfed.org/research/calendars/i-oct26.html" {
		t.Fatal("month end lost an overnight release or its provenance")
	}
	failNext = true
	if _, err := client.Fetch(context.Background(), spec, now, Batch{}); err == nil {
		t.Fatal("failed next-month fetch claimed covered overnight window")
	}
	first = strings.Replace(first, `/research/calendars/i-oct26.html`, `https://attacker.test/calendar`, 1)
	failNext = false
	if _, err := client.Fetch(context.Background(), spec, now, Batch{}); err == nil {
		t.Fatal("missing official next-month link accepted")
	}
}

func TestNYFedAmbiguousAfternoonLabelDoesNotBecomeOvernightInstant(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	payload := nyfedFixture(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 11)
	for _, label := range []string{"02:00", "12:45"} {
		batch, err := Parse(sourceSpec(t, "nyfed-calendar"), []byte(strings.Replace(payload, "(08:30)", "("+label+")", 1)), now)
		if err != nil {
			t.Fatal(err)
		}
		event := batch.Events[0]
		if !event.ScheduledAt.IsZero() || event.TimePrecision != "source_label" || event.TimeLabel != label || event.Date != "2026-09-11" {
			t.Fatal("ambiguous source label acquired an invented overnight release time")
		}
	}
}

func TestNYFedRolloverUsesPublishedCurrentMonthWithoutRenewingOldMonth(t *testing.T) {
	for _, month := range []time.Time{
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC),
	} {
		t.Run(month.Format("2006-01"), func(t *testing.T) {
			current := month.AddDate(0, 1, 0)
			// Pick a weekday in each month; fixtures deliberately contain no weekends.
			firstDay := month
			for firstDay.Weekday() == time.Saturday || firstDay.Weekday() == time.Sunday {
				firstDay = firstDay.AddDate(0, 0, 1)
			}
			releaseDay := current
			for releaseDay.Weekday() == time.Saturday || releaseDay.Weekday() == time.Sunday {
				releaseDay = releaseDay.AddDate(0, 0, 1)
			}
			first, next := nyfedFixture(month, firstDay.Day()), nyfedFixture(current, releaseDay.Day())
			spec := sourceSpec(t, "nyfed-calendar")
			nextURL := "https://www.newyorkfed.org/research/calendars/i-" + strings.ToLower(current.Format("Jan06")) + ".html"
			client := NewClient()
			calls := 0
			client.HTTP.Transport = publicTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				data := first
				if req.URL.String() != spec.URL {
					if req.URL.String() != nextURL {
						t.Fatal("unpublished URL requested")
					}
					data = next
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data)), Request: req}, nil
			})
			// Last day needs both months. First day must discard the old landing month.
			before := current.Add(-time.Hour)
			batch, err := client.Fetch(t.Context(), spec, before, Batch{})
			if err != nil || len(batch.Events) != 2 || batch.WindowStart != month.Format(time.DateOnly) || batch.WindowEnd != current.AddDate(0, 1, -1).Format(time.DateOnly) {
				t.Fatalf("year/leap rollover lost dates: %v", err)
			}
			now := current.Add(12 * time.Hour)
			batch, err = client.Fetch(t.Context(), spec, now, Batch{})
			if err != nil || calls != 4 || len(batch.Events) != 1 || batch.WindowStart != current.Format(time.DateOnly) || batch.Events[0].SourceURL != nextURL {
				t.Fatalf("published current month unavailable behind lagged landing page: %v", err)
			}
			calls = 0
			if _, err = client.Fetch(t.Context(), spec, current.AddDate(0, 1, 0).Add(12*time.Hour), Batch{}); err == nil || calls != 1 {
				t.Fatal("two-month stale landing page accepted or chased")
			}
			first = strings.Replace(first, strings.TrimPrefix(nextURL, "https://www.newyorkfed.org"), "/research/calendars/i-jan99.html", 1)
			calls = 0
			if _, err = client.Fetch(t.Context(), spec, now, Batch{}); err == nil || calls != 1 {
				t.Fatal("nonconsecutive published link was requested")
			}
		})
	}
}

func TestNYFedRejectsAmbiguousCoverageAndRestoredMonthProvenance(t *testing.T) {
	spec := sourceSpec(t, "nyfed-calendar")
	now := time.Date(2028, 2, 28, 12, 0, 0, 0, time.UTC)
	raw := nyfedFixture(time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC), 1)
	for _, bad := range []string{
		raw + `<td class="ts-data-table-head"><div>March 2028</div></td>`,
		raw + `<table class="research-table-1col greyborder"></table>`,
		strings.Replace(raw, "<td><div>29<br/></div></td>", "", 1),
	} {
		if _, err := Parse(spec, []byte(bad), now); err == nil {
			t.Fatal("ambiguous or partial calendar accepted")
		}
	}
	batch, err := Parse(spec, []byte(raw), now)
	if err != nil {
		t.Fatal(err)
	}
	batch.WindowStart = "2028-02-02"
	if ValidateBatch(spec, batch, now) == nil {
		t.Fatal("partial-month bounds accepted on restore")
	}
	batch.WindowStart = "2028-02-01"
	batch.Events[0].SourceURL = "https://www.newyorkfed.org/research/calendars/i-mar28.html"
	if ValidateBatch(spec, batch, now) == nil {
		t.Fatal("event attributed to wrong source month")
	}
	spec.URL = batch.Events[0].SourceURL
	if _, err := Parse(spec, []byte(raw), now); err == nil {
		t.Fatal("monthly URL served another month without failure")
	}
}

func TestNYFedLaggedLandingNearMonthEndBoundsThreePublishedReads(t *testing.T) {
	spec := sourceSpec(t, "nyfed-calendar")
	dec := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	bodies := []string{nyfedFixture(dec, 1), nyfedFixture(dec.AddDate(0, 1, 0), 4), nyfedFixture(dec.AddDate(0, 2, 0), 1)}
	urls := []string{spec.URL, "https://www.newyorkfed.org/research/calendars/i-jan27.html", "https://www.newyorkfed.org/research/calendars/i-feb27.html"}
	client := NewClient()
	calls := 0
	client.HTTP.Transport = publicTransport(func(req *http.Request) (*http.Response, error) {
		if calls >= len(urls) || req.URL.String() != urls[calls] {
			t.Fatal("collector followed an extra or unpublished calendar")
		}
		body := bodies[calls]
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	now := time.Date(2027, 1, 28, 12, 0, 0, 0, time.UTC)
	batch, err := client.Fetch(t.Context(), spec, now, Batch{})
	if err != nil || calls != 3 || batch.WindowStart != "2027-01-01" || batch.WindowEnd != "2027-02-28" || len(batch.Events) != 2 {
		t.Fatalf("lagged end-of-month coverage incorrect: %v", err)
	}
	for i, e := range batch.Events {
		if e.SourceURL != urls[i+1] || !e.RetrievedAt.Equal(now) {
			t.Fatal("successor source receipt was lost")
		}
	}
}
