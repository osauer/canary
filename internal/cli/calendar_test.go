package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderCalendarTextShowsHolidayNextOpenAndCoverage(t *testing.T) {
	t.Parallel()
	loc := mustLoadLocation(t, "America/New_York")
	nextOpen := time.Date(2026, 5, 26, 9, 30, 0, 0, loc)
	nextClose := time.Date(2026, 5, 26, 16, 0, 0, 0, loc)
	res := &rpc.MarketCalendarResult{
		Market:        "us_equity",
		Label:         "US equities",
		Timezone:      "America/New_York",
		CoverageStart: "2026-01-01",
		CoverageEnd:   "2028-12-31",
		Session: rpc.MarketSession{
			Market:   "us_equity",
			Label:    "US equities",
			Date:     "2026-05-25",
			Timezone: "America/New_York",
			State:    "holiday",
			Reason:   "Memorial Day",
			NextOpen: &nextOpen,
		},
		Sessions: []rpc.MarketSession{
			{Date: "2026-05-25", State: "holiday", Reason: "Memorial Day"},
			{Date: "2026-05-26", State: "regular", Open: nextOpen, Close: nextClose},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderCalendarText(env, res); code != 0 {
		t.Fatalf("renderCalendarText code = %d, want 0", code)
	}
	out := stdout.String()
	sessionDate := calendarDateLabel("2026-05-25", time.Now())
	nextSessionDate := calendarDateLabel("2026-05-26", time.Now())
	for _, want := range []string{
		"US equities calendar",
		"Session:       " + sessionDate + "  holiday",
		"Reason:        Memorial Day",
		"Next open:     2026-05-26 09:30 EDT",
		"Coverage:      2026-01-01 to 2028-12-31",
		nextSessionDate,
		"09:30-16:00 EDT",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("calendar output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatCalendarDateLabel(t *testing.T) {
	t.Parallel()
	today := time.Date(2026, 5, 26, 11, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name  string
		date  string
		style calendarDateStyle
		want  string
	}{
		{
			name:  "us format with today marker",
			date:  "2026-05-26",
			style: calendarDateISO,
			want:  "Tue 2026-05-26 (today)",
		},
		{
			name:  "german format with today marker",
			date:  "2026-05-26",
			style: calendarDateDayMonthYear,
			want:  "Tue 26-05-2026 (today)",
		},
		{
			name:  "weekday without today marker",
			date:  "2026-05-25",
			style: calendarDateISO,
			want:  "Mon 2026-05-25",
		},
		{
			name:  "unparseable date falls back unchanged",
			date:  "not-a-date",
			style: calendarDateISO,
			want:  "not-a-date",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatCalendarDateLabel(tc.date, today, tc.style)
			if got != tc.want {
				t.Fatalf("formatCalendarDateLabel(%q) = %q, want %q", tc.date, got, tc.want)
			}
		})
	}
}

func TestCalendarDateStyleFromLocale(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		locale string
		want   calendarDateStyle
	}{
		{locale: "de_DE.UTF-8", want: calendarDateDayMonthYear},
		{locale: "de-DE", want: calendarDateDayMonthYear},
		{locale: "de", want: calendarDateDayMonthYear},
		{locale: "en_US.UTF-8", want: calendarDateISO},
		{locale: "C", want: calendarDateISO},
		{locale: "fr_FR.UTF-8", want: calendarDateISO},
		{locale: "", want: calendarDateISO},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			t.Parallel()
			got := calendarDateStyleFromLocale(tc.locale)
			if got != tc.want {
				t.Fatalf("calendarDateStyleFromLocale(%q) = %v, want %v", tc.locale, got, tc.want)
			}
		})
	}
}

func TestCalendarDateStyleFromEnvUsesLocalePriority(t *testing.T) {
	t.Setenv("LC_TIME", "C")
	t.Setenv("LC_ALL", "en_US.UTF-8")
	t.Setenv("LANG", "de_DE.UTF-8")

	if got := calendarDateStyleFromEnv(); got != calendarDateISO {
		t.Fatalf("calendarDateStyleFromEnv() = %v, want US ISO style from LC_ALL", got)
	}

	t.Setenv("LC_TIME", "de_DE.UTF-8")
	if got := calendarDateStyleFromEnv(); got != calendarDateDayMonthYear {
		t.Fatalf("calendarDateStyleFromEnv() = %v, want German style from LC_TIME", got)
	}
}

func TestRenderCalendarTextDimsClosedDays(t *testing.T) {
	t.Parallel()
	loc := mustLoadLocation(t, "America/New_York")
	open := time.Date(2026, 5, 26, 9, 30, 0, 0, loc)
	closeT := time.Date(2026, 5, 26, 16, 0, 0, 0, loc)
	res := &rpc.MarketCalendarResult{
		Label:         "US equities",
		Timezone:      "America/New_York",
		CoverageStart: "2026-01-01",
		CoverageEnd:   "2028-12-31",
		Session: rpc.MarketSession{
			Date:     "2026-05-26",
			Timezone: "America/New_York",
			State:    "regular",
			Open:     open,
			Close:    closeT,
		},
		Sessions: []rpc.MarketSession{
			{Date: "2026-05-25", State: "holiday", Reason: "Memorial Day"},
			{Date: "2026-05-26", State: "regular", Open: open, Close: closeT},
			{Date: "2026-05-30", State: "closed", Reason: "weekend"},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	if code := renderCalendarText(env, res); code != 0 {
		t.Fatalf("renderCalendarText code = %d, want 0", code)
	}
	out := stdout.String()
	for _, want := range []string{
		ansiDim + "  " + calendarDateLabel("2026-05-25", time.Now()),
		ansiDim + "  " + calendarDateLabel("2026-05-30", time.Now()),
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("closed-day row missing dim prefix %q:\n%s", want, out)
		}
	}
	regularLine := calendarDateLabel("2026-05-26", time.Now()) + "  regular"
	if strings.Contains(out, ansiDim+regularLine) {
		t.Fatalf("regular row should not be dimmed:\n%s", out)
	}
}

func TestQuoteSessionHintSummarizesCalendarContext(t *testing.T) {
	t.Parallel()
	loc := mustLoadLocation(t, "Europe/Berlin")
	nextOpen := time.Date(2026, 12, 28, 9, 0, 0, 0, loc)
	env := &Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}

	got := quoteSessionHint(env, &rpc.MarketSession{
		Label:    "Xetra",
		State:    "holiday",
		Reason:   "Christmas Day",
		NextOpen: &nextOpen,
	})

	for _, want := range []string{"Xetra", "holiday", "Christmas Day", "next open 2026-12-28 09:00 CET"} {
		if !strings.Contains(got, want) {
			t.Fatalf("quoteSessionHint missing %q in %q", want, got)
		}
	}
}

func TestMarketSessionHoursUsesResponseTimezoneName(t *testing.T) {
	t.Parallel()
	open, err := time.Parse(time.RFC3339, "2026-11-27T09:30:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	closeT, err := time.Parse(time.RFC3339, "2026-11-27T13:00:00-05:00")
	if err != nil {
		t.Fatal(err)
	}
	got := marketSessionHours(rpc.MarketSession{
		Timezone: "America/New_York",
		Open:     open,
		Close:    closeT,
	})
	if got != "09:30-13:00 EST" {
		t.Fatalf("marketSessionHours = %q, want EST zone name", got)
	}
}

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}
