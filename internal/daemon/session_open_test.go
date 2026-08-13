package daemon

import (
	"testing"
	"time"
)

// A backend-link loss must warn per event whenever ANY supported market is in
// session, not only the US one — a Xetra-morning loss is an
// order-transmission hole hours before New York opens.
func TestAnySupportedMarketOpenUnionsCalendars(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		// Wed 10:00 Berlin: Xetra open, US closed.
		{"xetra morning", time.Date(2026, 8, 12, 10, 0, 0, 0, berlin), true},
		// Wed 16:00 Berlin: Xetra and US both open (overlap).
		{"overlap", time.Date(2026, 8, 12, 16, 0, 0, 0, berlin), true},
		// Wed 20:00 Berlin: Xetra closed, US open.
		{"us afternoon", time.Date(2026, 8, 12, 20, 0, 0, 0, berlin), true},
		// Wed 23:30 Berlin: everything supported is closed.
		{"late evening", time.Date(2026, 8, 12, 23, 30, 0, 0, berlin), false},
		// Saturday noon: nothing open anywhere.
		{"weekend", time.Date(2026, 8, 15, 12, 0, 0, 0, berlin), false},
	}
	for _, tc := range cases {
		if got := anySupportedMarketOpen(tc.at); got != tc.want {
			t.Errorf("%s (%s): open = %t, want %t", tc.name, tc.at, got, tc.want)
		}
	}
}
