package risk

import "testing"

// The window verdict reads only usable dates: unknown, stale or uncomputable
// earnings have no verdict, and the window is zero through the freeze inclusive.
func TestEarningsWindowReadsOnlyUsableDates(t *testing.T) {
	cases := []struct {
		name string
		in   EarningsInput
		want string
	}{
		{"unknown", EarningsInput{SessionsUntil: new(1)}, ""},
		{"stale", EarningsInput{Known: true, Stale: true, SessionsUntil: new(1)}, ""},
		{"uncomputable", EarningsInput{Known: true}, ""},
		{"today", EarningsInput{Known: true, SessionsUntil: new(0)}, EarningsWindowInside},
		{"edge", EarningsInput{Known: true, SessionsUntil: new(3)}, EarningsWindowInside},
		{"beyond", EarningsInput{Known: true, SessionsUntil: new(4)}, EarningsWindowOutside},
	}
	for _, c := range cases {
		if got := EarningsWindow(c.in, 3); got != c.want {
			t.Errorf("%s: EarningsWindow = %q, want %q", c.name, got, c.want)
		}
	}
}
