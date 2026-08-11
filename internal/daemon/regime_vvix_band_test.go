package daemon

import (
	"context"
	"testing"
	"time"
)

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

// The funding fetcher must serve the five-publication rise the v2 band keys
// on: CP-dated observations joined to the newest bill print at or before each
// date, within the calibration replay's three-day slack.
func TestFetchRegimeFundingStressComputesChange5(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 8, d, 0, 0, 0, 0, time.UTC) }
	cp := []regimeSeriesPoint{}
	bill := []regimeSeriesPoint{}
	// Business days Jul 28 .. Aug 10 style: 3,4,5,6,7,10 plus three prior.
	for i, d := range []int{1, 4, 5, 6, 7, 8, 10} {
		cp = append(cp, regimeSeriesPoint{Date: day(d), Value: 3.80 + float64(i)*0.01})
		bill = append(bill, regimeSeriesPoint{Date: day(d), Value: 3.70})
	}
	deps := &regimeDeps{officialSeries: func(_ context.Context, id string) ([]regimeSeriesPoint, error) {
		if id == fredSeriesCP3M {
			return cp, nil
		}
		return bill, nil
	}}
	out := fetchRegimeFundingStress(context.Background(), deps)
	if out.SpreadBps == nil || out.Change5Bps == nil {
		t.Fatalf("spread=%v change5=%v, want both computed (status %s %s)", out.SpreadBps, out.Change5Bps, out.Status, out.ErrorMessage)
	}
	// Latest Aug 10 spread 16bp minus five publications back (Aug 4) 11bp.
	if got := *out.Change5Bps; got < 4.9 || got > 5.1 {
		t.Fatalf("change5 = %.2f bp, want ~5", got)
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
