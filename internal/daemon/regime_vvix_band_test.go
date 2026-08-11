package daemon

import "testing"

// vvix_daily_v2: amber is a transition (level AND 5-session rise), never a
// resting zone; red at 110 ignores trend; a missing rise holds amber at
// level rather than softening the warning.
func TestClassifyVolOfVolBandV2(t *testing.T) {
	v := func(x float64) *float64 { return &x }
	cases := []struct {
		name   string
		vvix   *float64
		rise5d *float64
		want   string
	}{
		{"below level", v(85), v(20), "green"},
		{"at level, falling", v(92.5), v(-2.9), "green"},
		{"at level, flat", v(95), v(0), "green"},
		{"at level, rising under gate", v(95), v(2.9), "green"},
		{"at level, rising at gate", v(95), v(3.0), "yellow"},
		{"at level, rise unknown", v(95), nil, "yellow"},
		{"red ignores trend", v(111), v(-10), "red"},
		{"red boundary", v(110), nil, "red"},
		{"no reading", nil, v(5), ""},
	}
	for _, tc := range cases {
		if got := classifyVolOfVolBand(tc.vvix, tc.rise5d); got != tc.want {
			t.Errorf("%s: band = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// funding_cp_tbill_v2 mirrors the same shape: amber needs level AND a +10 bp
// rise over five CP publications, red at 75 bp ignores trend, and a missing
// rise holds amber at level.
func TestClassifyFundingStressBandV2(t *testing.T) {
	v := func(x float64) *float64 { return &x }
	cases := []struct {
		name   string
		spread *float64
		rise   *float64
		want   string
	}{
		{"calm level", v(10), v(50), "green"},
		{"at level, flat", v(31), v(0), "green"},
		{"at level, rising under gate", v(31), v(9), "green"},
		{"at level, rising at gate", v(31), v(10), "yellow"},
		{"at level, rise unknown", v(31), nil, "yellow"},
		{"red ignores trend", v(80), v(-20), "red"},
		{"red boundary", v(75), nil, "red"},
		{"no reading", nil, v(50), ""},
	}
	for _, tc := range cases {
		if got := classifyFundingStressBand(tc.spread, tc.rise); got != tc.want {
			t.Errorf("%s: band = %q, want %q", tc.name, got, tc.want)
		}
	}
}
