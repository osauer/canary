package daemon

import (
	"testing"
	"time"

	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// The options calendar closes at 16:15 New York, the late close of SPY-style
// classes; single-stock options close at 16:00. Each class's own broker hours
// decide its close, never later than the calendar.
func TestAddClassOptionsCalendarUsesTheClassClose(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	at := func(date, clock string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, ny)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	const (
		stock   = "20261022:0830-20261022:1500;20261023:0830-20261023:1500"
		late    = "20261022:0830-20261022:1515;20261023:0830-20261023:1515"
		halfDay = "20261127:0830-20261127:1215"
	)
	for _, tc := range []struct {
		name, hours, tz, date, now, mode, close string
	}{
		{"stock class before its close", stock, "US/Central", "2026-10-22", "15:59", "live", "16:00"},
		{"stock class after its close", stock, "US/Central", "2026-10-22", "16:05", "planning", "16:00"},
		{"late class after the stock close", late, "US/Central", "2026-10-22", "16:05", "live", "16:15"},
		{"late class at the calendar close", late, "US/Central", "2026-10-22", "16:15", "planning", "16:15"},
		{"broker later than the calendar", "20261022:0830-20261022:1600", "US/Central", "2026-10-22", "16:10", "live", "16:15"},
		{"broker earlier than the stock close", "20261022:0830-20261022:1400", "US/Central", "2026-10-22", "15:30", "planning", "15:00"},
		{"hours missing", "", "US/Central", "2026-10-22", "16:05", "planning", "16:00"},
		{"zone missing", late, "", "2026-10-22", "16:05", "planning", "16:00"},
		{"hours unreadable", "20261022:0830+1515", "US/Central", "2026-10-22", "16:05", "planning", "16:00"},
		{"no broker window today", "20261023:0830-20261023:1515", "US/Central", "2026-10-22", "15:00", "live", "16:00"},
		{"half day stays at the calendar close", halfDay, "US/Central", "2026-11-27", "12:55", "live", "13:00"},
		{"half day after the calendar close", halfDay, "US/Central", "2026-11-27", "13:05", "planning", ""},
		{"weekend", late, "US/Central", "2026-10-24", "12:00", "planning", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := at(tc.date, tc.now)
			windows, ok := ibkr.ResolvedOrderContract{TimeZoneID: tc.tz, LiquidHours: tc.hours}.SessionWindows()
			got := addClassOptionsCalendar(addOptionsCalendar(now), windows, ok, now)
			if got.mode != tc.mode {
				t.Fatalf("mode %s, want %s (session %+v)", got.mode, tc.mode, got.current)
			}
			if tc.close != "" {
				want := at(tc.date, tc.close)
				if !got.current.Close.Equal(want) || len(got.current.Windows) == 0 || !got.current.Windows[len(got.current.Windows)-1].Close.Equal(want) {
					t.Fatalf("session close %s windows %+v, want %s", got.current.Close.In(ny), got.current.Windows, want)
				}
			}
			if got.mode == "planning" && tc.date == "2026-10-22" {
				if got.last == nil || got.last.Date != "2026-10-22" || !got.last.Close.Equal(at(tc.date, tc.close)) {
					t.Fatalf("the closed class session is not today's completed session: %+v", got.last)
				}
				if got.current.IsOpen || got.current.NextOpen == nil || !got.current.NextOpen.Equal(at("2026-10-23", "09:30")) {
					t.Fatalf("closed class does not reopen at the next session: %+v", got.current)
				}
			}
		})
	}
}
