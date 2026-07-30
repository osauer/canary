package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// optionSessionOpen is the policy authority for the options_rth_required
// blocker and the require-live-iv gate. Unlike the display-only
// rpc.IsOptionRTH it must respect the official calendar: holidays closed,
// early closes honored, 16:15 regular close, and a clock-only fallback only
// outside embedded coverage.
func TestOptionSessionOpen(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"holiday mid-morning (Independence Day observed)", time.Date(2026, 7, 3, 10, 0, 0, 0, ny), false},
		{"early close still open (Thanksgiving Friday 12:30)", time.Date(2026, 11, 27, 12, 30, 0, 0, ny), true},
		{"early close after 13:00 (Thanksgiving Friday 13:30)", time.Date(2026, 11, 27, 13, 30, 0, 0, ny), false},
		{"regular day mid-session", time.Date(2026, 7, 30, 10, 0, 0, 0, ny), true},
		{"regular day 16:00-16:15 options window", time.Date(2026, 7, 30, 16, 5, 0, 0, ny), true},
		{"regular day after 16:15", time.Date(2026, 7, 30, 16, 20, 0, 0, ny), false},
		{"weekend", time.Date(2026, 8, 1, 12, 0, 0, 0, ny), false},
		{"outside coverage falls back to weekday clock", time.Date(2030, 1, 10, 12, 0, 0, 0, ny), true},
		{"outside coverage weekend stays closed", time.Date(2030, 1, 12, 12, 0, 0, 0, ny), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := optionSessionOpen(tc.at); got != tc.want {
				t.Fatalf("optionSessionOpen(%s) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

// The two helpers deliberately diverge: the display helper stays holiday-blind
// and closes at 16:00. Pin the divergence so a future "simplification" cannot
// silently reroute a policy gate back through the weaker helper.
func TestOptionSessionOpenDivergesFromDisplayHelper(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load zone: %v", err)
	}
	holiday := time.Date(2026, 7, 3, 10, 0, 0, 0, ny)
	if rpc.IsOptionRTH(holiday) != true || optionSessionOpen(holiday) != false {
		t.Fatalf("holiday divergence lost: display=%v policy=%v", rpc.IsOptionRTH(holiday), optionSessionOpen(holiday))
	}
	lateWindow := time.Date(2026, 7, 30, 16, 5, 0, 0, ny)
	if rpc.IsOptionRTH(lateWindow) != false || optionSessionOpen(lateWindow) != true {
		t.Fatalf("16:00-16:15 divergence lost: display=%v policy=%v", rpc.IsOptionRTH(lateWindow), optionSessionOpen(lateWindow))
	}
}
