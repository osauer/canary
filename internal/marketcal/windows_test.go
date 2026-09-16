package marketcal

import (
	"testing"
	"time"
)

// Scheduling from the outer envelope must not turn lunch, holidays, or missing
// calendar years into work windows.
func TestCashWindowsAndCoverage(t *testing.T) {
	for _, tc := range []struct {
		market  Market
		at      string
		state   State
		open    bool
		windows int
		next    string
	}{
		{MarketJPTSE, "2026-09-14T11:29:59+09:00", StateRegular, true, 2, ""},
		{MarketJPTSE, "2026-09-14T11:30:00+09:00", StateRegular, false, 2, "2026-09-14T12:30:00+09:00"},
		{MarketJPTSE, "2026-09-14T12:30:00+09:00", StateRegular, true, 2, ""},
		{MarketJPTSE, "2026-09-14T15:30:00+09:00", StateRegular, false, 2, "2026-09-15T09:00:00+09:00"},
		{MarketJPTSE, "2026-09-22T10:00:00+09:00", StateHoliday, false, 0, "2026-09-24T09:00:00+09:00"},
		{MarketJPTSE, "2028-01-04T10:00:00+09:00", StateUnknown, false, 0, ""},
		{MarketHKHKEX, "2026-09-14T12:00:00+08:00", StateRegular, false, 2, "2026-09-14T13:00:00+08:00"},
		{MarketHKHKEX, "2026-12-24T12:00:00+08:00", StateEarlyClose, false, 1, "2026-12-28T09:30:00+08:00"},
		{MarketHKHKEX, "2026-02-16T11:30:00+08:00", StateEarlyClose, true, 1, ""},
		{MarketHKHKEX, "2026-04-07T10:00:00+08:00", StateHoliday, false, 0, "2026-04-08T09:30:00+08:00"},
		{MarketHKHKEX, "2027-01-04T10:00:00+08:00", StateUnknown, false, 0, ""},
		{MarketUKLSE, "2026-12-24T12:30:00Z", StateEarlyClose, false, 1, "2026-12-29T08:00:00Z"},
		{MarketUKLSE, "2028-12-22T12:30:00Z", StateEarlyClose, false, 1, "2028-12-27T08:00:00Z"},
		{MarketUKLSE, "2026-05-04T10:00:00+01:00", StateHoliday, false, 0, "2026-05-05T08:00:00+01:00"},
	} {
		t.Run(string(tc.market)+"/"+tc.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			s, err := New().SessionAt(tc.market, at)
			if err != nil {
				t.Fatal(err)
			}
			if s.State != tc.state || s.IsOpen != tc.open || len(s.Windows) != tc.windows {
				t.Fatalf("invented or lost trading interval: %+v", s)
			}
			if tc.next != "" && (s.NextOpen == nil || s.NextOpen.Format(time.RFC3339) != tc.next) {
				t.Fatalf("incorrect next opening: %+v", s.NextOpen)
			}
			if tc.state == StateUnknown && (!s.Open.IsZero() || !s.Close.IsZero() || s.NextOpen != nil) {
				t.Fatal("unknown date supplied actionable times")
			}
		})
	}
}

func TestBerlinCalendarConversionAtDSTBoundaries(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		market     Market
		date, want string
	}{
		{MarketUSEquity, "2026-09-14", "15:30"}, {MarketUSEquity, "2026-10-26", "14:30"},
		{MarketUSEquity, "2026-11-02", "15:30"}, {MarketUKLSE, "2026-10-26", "09:00"},
		{MarketJPTSE, "2026-09-14", "02:00"}, {MarketJPTSE, "2026-10-26", "01:00"},
		{MarketHKHKEX, "2026-09-14", "03:30"}, {MarketHKHKEX, "2026-10-26", "02:30"},
	} {
		r, err := New().Query(Query{Market: tc.market, Date: tc.date, Days: 1})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Session.Open.In(berlin).Format("15:04"); got != tc.want {
			t.Fatalf("%s %s: Berlin open %s, want %s", tc.market, tc.date, got, tc.want)
		}
	}
}
