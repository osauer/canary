package twssim

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
)

// The daily bars end on the last weekday whose New York open has passed, at
// any hour and in either half of the year: a daemon never finds the series
// older than the last session it counts as completed, on an early close
// (2026-11-27, 13:00) included, and before the open it finds no bar of today.
func TestDailyBarsEndOnTheLastOpenedNewYorkSession(t *testing.T) {
	at := func(s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04", s, newYork)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct{ name, now, last string }{
		{"a minute before the open", "2026-11-24 09:29", "2026-11-23"},
		{"at the open", "2026-11-24 09:30", "2026-11-24"},
		{"in the session", "2026-11-24 12:00", "2026-11-24"},
		{"after an early close", "2026-11-27 13:40", "2026-11-27"},
		{"evening, already the next UTC day", "2026-11-24 20:30", "2026-11-24"},
		{"before the open", "2026-11-25 04:00", "2026-11-24"},
		{"Saturday", "2026-11-28 12:00", "2026-11-27"},
		{"Sunday evening", "2026-11-29 18:00", "2026-11-27"},
		{"Monday before the open", "2026-11-30 08:00", "2026-11-27"},
		{"summer time, a minute before the open", "2026-07-14 09:29", "2026-07-13"},
		{"summer time, at the open", "2026-07-14 09:30", "2026-07-14"},
	} {
		dates := dailyBarDates(at(tc.now).UTC(), 30)
		if len(dates) == 0 {
			t.Fatalf("%s: no bars", tc.name)
		}
		if got := dates[len(dates)-1].Format("2006-01-02"); got != tc.last {
			t.Errorf("%s (%s New York): last bar %s, want %s", tc.name, tc.now, got, tc.last)
		}
		for i, d := range dates {
			if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
				t.Errorf("%s: bar %d falls on a %s", tc.name, i, wd)
			}
			if i > 0 && !d.After(dates[i-1]) {
				t.Errorf("%s: bars out of order at %d", tc.name, i)
			}
		}
	}
}

// At every half hour of five weeks that hold Thanksgiving, its early close
// and the Christmas one, the last bar is no older than the session the
// daemon counts as completed and no newer than today in New York: the
// bounds the daemon's regime history must meet to read as current
// (regimeHistoryCurrent). A simulator outside them makes every regime row
// that needs history read stale for those hours.
func TestDailyBarsAreNeverOlderThanTheCompletedSession(t *testing.T) {
	start := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	for now := start; now.Before(start.AddDate(0, 0, 35)); now = now.Add(30 * time.Minute) {
		dates := dailyBarDates(now, 30)
		last := dates[len(dates)-1].Format("2006-01-02")
		completed, today := spx.CompletedSessionKey(now), now.In(newYork).Format("2006-01-02")
		if last < completed || last > today {
			t.Fatalf("at %s the last bar is %s, outside the completed session %s and today %s", now.Format(time.RFC3339), last, completed, today)
		}
	}
}
