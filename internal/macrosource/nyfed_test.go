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

// nyfedPage builds a synthetic monthly calendar shaped like the New York Fed
// page: a month heading, the Eastern-time statement, a NEXT MONTH link and one
// dated cell per weekday. releases maps a day to the entry markup in its cell.
func nyfedPage(month time.Time, releases map[int]string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `<p>All releases all Eastern Time.</p><td class="ts-data-table-head"><div>%s</div></td>`, month.Format("January 2006"))
	fmt.Fprintf(&b, `<a href="/research/calendars/i-%s.html">NEXT MONTH</a>`, strings.ToLower(month.AddDate(0, 1, 0).Format("Jan06")))
	b.WriteString(`<table class="research-table-1col greyborder">`)
	for day := month; day.Month() == month.Month(); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		fmt.Fprintf(&b, `<td><div>%02d<br/>`, day.Day())
		if entries, ok := releases[day.Day()]; ok {
			b.WriteString(`<span>` + entries + `</span>`)
		}
		b.WriteString(`</div></td>`)
	}
	b.WriteString(`</table>`)
	return b.String()
}

func nyfedFixture(month time.Time, releaseDay int) string {
	return nyfedPage(month, map[int]string{releaseDay: `<a href="https://www.bls.gov/news.release/cpi.toc.htm">Synthetic inflation release</a><br/>(08:30)<br/><br/>`})
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

// Synthetic release entries in the shapes the live calendar prints, including
// its line breaks. The October 2026 page began printing some time labels
// inside the link text, which rejected the whole backup calendar.
const (
	nyfedAfterEntry   = `<a href="https://www.bls.gov/news.release/synthetic.htm" target="_NEW">Synthetic employment release</a><br/>(08:30)<br/><br/>` + "\n"
	nyfedPDFEntry     = `<a href="https://www.census.gov/synthetic/release.pdf" target="_NEW">Synthetic construction release</a><img src="/medialibrary/media/images/v2/icons/pdf.gif" alt="PDF" border="0"><br/>(10:00)<br/><br/>` + "\n"
	nyfedUnknownEntry = `<a href="https://www.newyorkfed.org/research/synthetic-auction" target="_NEW">Synthetic auction results</a><br/>(TBD)<br/><br/>` + "\n"
)

// nyfedInLinkEntry prints label inside the link text, after a line break.
func nyfedInLinkEntry(title, label string) string {
	return `<a href="https://www.newyorkfed.org/research/synthetic-indicators">` + title + "\n(" + label + `)</a><br/><br/>` + "\n"
}

// nyfedMonthSpec is the source identity fetchNYFedNext gives a chained page.
func nyfedMonthSpec(t *testing.T, month time.Time) Spec {
	spec := sourceSpec(t, "nyfed-calendar")
	spec.URL = "https://www.newyorkfed.org/research/calendars/i-" + strings.ToLower(month.Format("Jan06")) + ".html"
	return spec
}

func TestNYFedReadsTimeLabelInsideReleaseLink(t *testing.T) {
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 24, 15, 28, 0, 0, time.UTC)
	spec := nyfedMonthSpec(t, oct)
	eastern, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		label, precision string
		at               time.Time
	}{
		{"10:00", "source_label", time.Time{}},
		{"02:00", "source_label", time.Time{}},
		{"12:45", "source_label", time.Time{}},
		{"13:30", "instant", time.Date(2026, 10, 2, 13, 30, 0, 0, eastern)},
		{"00:15", "instant", time.Date(2026, 10, 2, 0, 15, 0, 0, eastern)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			page := nyfedPage(oct, map[int]string{2: nyfedInLinkEntry("Synthetic Distribution Indicators (SDIs)", tc.label)})
			batch, err := Parse(spec, []byte(page), now)
			if err != nil {
				t.Fatalf("in-link time label rejected the backup calendar: %v", err)
			}
			if len(batch.Events) != 1 || batch.SkippedItems != 0 {
				t.Fatalf("events=%d skipped=%d, want one kept release", len(batch.Events), batch.SkippedItems)
			}
			e := batch.Events[0]
			if e.Title != "Synthetic Distribution Indicators (SDIs)" || e.TimeLabel != tc.label || e.Date != "2026-10-02" || e.TimePrecision != tc.precision || !e.ScheduledAt.Equal(tc.at) || e.SourceURL != spec.URL {
				t.Fatalf("in-link release misread: %+v", e)
			}
		})
	}
	// The new shape widens entry recognition only; page-level checks still fail
	// the whole page.
	valid := nyfedPage(oct, map[int]string{2: nyfedInLinkEntry("Synthetic indicators", "10:00")})
	for name, tc := range map[string]struct{ page, want string }{
		"impossible clock":   {strings.Replace(valid, "(10:00)", "(25:30)", 1), "calendar source: New York Fed release time invalid"},
		"undated release":    {strings.Replace(valid, "<td><div>02<br/>", "<td><div>", 1), "calendar source: New York Fed release omitted its date"},
		"timezone removed":   {strings.Replace(valid, "all Eastern Time", "all local times", 1), "calendar source: New York Fed calendar format or timezone changed"},
		"table truncated":    {strings.Replace(valid, "</table>", "", 1), "calendar source: New York Fed calendar format or timezone changed"},
		"weekday missing":    {strings.Replace(valid, "<td><div>05<br/></div></td>", "", 1), "calendar source: New York Fed calendar weekdays incomplete"},
		"duplicate day":      {strings.Replace(valid, "<td><div>05<br/>", "<td><div>02<br/>", 1), "calendar source: New York Fed calendar day invalid"},
		"month and URL skew": {strings.Replace(valid, "October 2026", "November 2026", 1), "calendar source: New York Fed calendar URL and month disagree"},
	} {
		if _, err := Parse(spec, []byte(tc.page), now); err == nil || err.Error() != tc.want {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}

func TestNYFedReadsBothReleaseShapesInOneDay(t *testing.T) {
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	spec := nyfedMonthSpec(t, oct)
	cell := nyfedAfterEntry + nyfedInLinkEntry("Synthetic Distribution Indicators (SDIs)", "10:00") + nyfedPDFEntry + nyfedInLinkEntry("Synthetic staff nowcast", "12:45")
	batch, err := Parse(spec, []byte(nyfedPage(oct, map[int]string{2: cell})), time.Date(2026, 9, 24, 15, 28, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("mixed release shapes rejected the backup calendar: %v", err)
	}
	want := []struct{ title, label string }{
		{"Synthetic employment release", "08:30"},
		{"Synthetic Distribution Indicators (SDIs)", "10:00"},
		{"Synthetic construction release", "10:00"},
		{"Synthetic staff nowcast", "12:45"},
	}
	if len(batch.Events) != len(want) || batch.SkippedItems != 0 {
		t.Fatalf("events=%d skipped=%d, want %d kept releases", len(batch.Events), batch.SkippedItems, len(want))
	}
	for i, e := range batch.Events {
		// A title that absorbed a neighboring entry would carry its text.
		if e.Title != want[i].title || e.TimeLabel != want[i].label || e.Date != "2026-10-02" {
			t.Fatalf("release %d misread: title=%q label=%q date=%s", i, e.Title, e.TimeLabel, e.Date)
		}
	}
}

func TestNYFedSkipsUnrecognizedReleaseEntryAndDisclosesIt(t *testing.T) {
	sep, oct := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 24, 15, 28, 0, 0, time.UTC)
	spec := nyfedMonthSpec(t, oct)
	untitled := `<a href="https://www.newyorkfed.org/research/synthetic-untitled"><img src="/synthetic.gif" alt=""></a><br/>(08:30)<br/><br/>` + "\n"
	untimed := `<a href="https://www.newyorkfed.org/research/synthetic-survey">Synthetic survey</a><br/><br/>` + "\n"
	// The label sits after the link's own closing tag, so it is not in-link.
	stray := `<a href="https://www.newyorkfed.org/research/synthetic-notice">Synthetic notice</a> revised (10:00)</a><br/><br/>` + "\n"
	page := nyfedPage(oct, map[int]string{
		2: nyfedAfterEntry + nyfedUnknownEntry + nyfedInLinkEntry("Synthetic Distribution Indicators (SDIs)", "10:00"),
		5: untitled + nyfedPDFEntry + untimed + stray,
	})
	batch, err := Parse(spec, []byte(page), now)
	if err != nil {
		t.Fatalf("unrecognized entries discarded the backup calendar: %v", err)
	}
	var kept []string
	for _, e := range batch.Events {
		kept = append(kept, e.Date+" "+e.TimeLabel+" "+e.Title)
	}
	if strings.Join(kept, "|") != "2026-10-02 08:30 Synthetic employment release|2026-10-02 10:00 Synthetic Distribution Indicators (SDIs)|2026-10-05 10:00 Synthetic construction release" {
		t.Fatalf("kept releases = %q", kept)
	}
	if batch.SkippedItems != 4 || batch.SkippedDisclosure(spec) != "4 calendar entries skipped: unrecognized release title or time label" {
		t.Fatalf("omission hidden: skipped=%d disclosure=%q", batch.SkippedItems, batch.SkippedDisclosure(spec))
	}
	if err := ValidateBatch(spec, batch, now); err != nil {
		t.Fatalf("partial calendar fails restore validation: %v", err)
	}
	batch.SkippedItems = -1
	if ValidateBatch(spec, batch, now) == nil {
		t.Fatal("negative skipped count restored as valid")
	}
	one, err := Parse(spec, []byte(nyfedPage(oct, map[int]string{2: nyfedAfterEntry + nyfedUnknownEntry})), now)
	if err != nil || one.SkippedDisclosure(spec) != "1 calendar entry skipped: unrecognized release title or time label" {
		t.Fatalf("single omission disclosure = %q, %v", one.SkippedDisclosure(spec), err)
	}

	// Month end chains the published next month; both months' omissions are
	// disclosed with the combined batch.
	landing := sourceSpec(t, "nyfed-calendar")
	pages := map[string]string{
		landing.URL: nyfedPage(sep, map[int]string{28: nyfedAfterEntry + nyfedUnknownEntry}),
		spec.URL:    nyfedPage(oct, map[int]string{2: nyfedInLinkEntry("Synthetic Distribution Indicators (SDIs)", "10:00") + untimed}),
	}
	client := NewClient()
	client.HTTP.Transport = publicTransport(func(req *http.Request) (*http.Response, error) {
		body, ok := pages[req.URL.String()]
		if !ok {
			t.Fatalf("collector requested an unpublished URL: %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	combined, err := client.Fetch(t.Context(), landing, time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC), Batch{})
	if err != nil {
		t.Fatalf("month-end chain failed on an unrecognized entry: %v", err)
	}
	if len(combined.Events) != 2 || combined.WindowEnd != "2026-10-31" || combined.SkippedItems != 2 || combined.Events[1].SourceURL != spec.URL || combined.Events[1].TimeLabel != "10:00" {
		t.Fatalf("month-end chain lost a release or an omission: events=%d end=%s skipped=%d", len(combined.Events), combined.WindowEnd, combined.SkippedItems)
	}
}

func TestNYFedFailsWhenNoReleaseEntryIsRecognized(t *testing.T) {
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 9, 24, 15, 28, 0, 0, time.UTC)
	spec := nyfedMonthSpec(t, oct)
	untimed := `<a href="https://www.newyorkfed.org/research/synthetic-survey">Synthetic survey</a><br/><br/>`
	for name, tc := range map[string]struct {
		page, want string
	}{
		"every entry unrecognized":  {nyfedPage(oct, map[int]string{2: nyfedUnknownEntry + untimed, 5: nyfedUnknownEntry}), "calendar source: New York Fed release time or format invalid"},
		"page defect beside a skip": {strings.Replace(nyfedPage(oct, map[int]string{2: nyfedAfterEntry + nyfedUnknownEntry}), "<td><div>05<br/></div></td>", "", 1), "calendar source: New York Fed calendar weekdays incomplete"},
		"no release at all":         {nyfedPage(oct, nil), "source supplied no usable records"},
	} {
		t.Run(name, func(t *testing.T) {
			batch, err := Parse(spec, []byte(tc.page), now)
			if err == nil || err.Error() != tc.want || len(batch.Events) != 0 || batch.SkippedItems != 0 {
				t.Fatalf("got events=%d skipped=%d err=%v, want %q", len(batch.Events), batch.SkippedItems, err, tc.want)
			}
		})
	}
}
