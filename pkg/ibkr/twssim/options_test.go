package twssim

import (
	"testing"
	"time"
)

// The short expiry is inside seven days and every other one beyond it, on
// every weekday, at either side of midnight UTC and across a month end, by
// the rule add call-plan applies: refused unless expiry (midnight UTC) is
// after now plus seven days.
func TestOptionExpiriesHoldTheirHorizonsOnEveryDay(t *testing.T) {
	refused := func(expiry string, now time.Time) bool {
		d, err := time.Parse("20060102", expiry)
		if err != nil {
			t.Fatal(err)
		}
		return !d.After(now.Add(7 * 24 * time.Hour))
	}
	bands := [][2]int{{1, 7}, {15, 21}, {29, 35}, {57, 63}, {113, 119}}
	for _, start := range []time.Time{
		time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC), // a Monday
		time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC), // Thanksgiving week
		time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC), // across a month and year end
		time.Date(2027, 2, 22, 0, 0, 0, 0, time.UTC),  // across a short month
	} {
		for d := range 7 {
			for _, offset := range []time.Duration{time.Second, 12 * time.Hour, 24*time.Hour - time.Second} {
				now := start.AddDate(0, 0, d).Add(offset)
				exps := OptionExpiries(now)
				if len(exps) != 5 {
					t.Fatalf("%s: %v", now, exps)
				}
				for i, e := range exps {
					exp, _ := time.Parse("20060102", e)
					days := int(exp.Sub(now.UTC().Truncate(24*time.Hour)).Hours() / 24)
					if exp.Weekday() != time.Friday || days < bands[i][0] || days > bands[i][1] {
						t.Fatalf("%s: expiry %d %s is %d days out, want a Friday in %v", now, i, e, days, bands[i])
					}
					if refused(e, now) != (i == 0) {
						t.Fatalf("%s: expiry %s refused=%t", now, e, refused(e, now))
					}
				}
			}
		}
	}
}
