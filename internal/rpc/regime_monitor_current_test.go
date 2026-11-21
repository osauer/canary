package rpc

import "testing"

// Every surface reads one verdict for "is this row a current rating", served
// on the monitor row (RegimeRowCurrent). Desk's tiles read the monitor; on
// 2026-11-21 they showed Volatility "Stale" for Friday's close on a Saturday,
// a verdict Desk derived itself from the status alone.
func TestRegimeMonitorServesTheCurrentVerdict(t *testing.T) {
	for _, tc := range []struct {
		status, class string
		want          bool
	}{
		{RegimeStatusStale, RegimeFreshnessNotDue, true},
		{RegimeStatusStale, RegimeFreshnessOverdue, false},
		{RegimeStatusOK, RegimeFreshnessFresh, true},
		{RegimeStatusError, RegimeFreshnessOverdue, false},
	} {
		r := &RegimeSnapshotResult{VIXTermStructure: RegimeVIXTerm{Status: tc.status, Ratio: new(1.16), RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red", Freshness: &RegimeFreshness{Class: tc.class}}}}
		got := CompactRegimeMonitor(r).Indicators[0]
		if got.Name != "VIX/VIX3M" || got.Current != tc.want {
			t.Errorf("%s/%s: current=%v, want %v", tc.status, tc.class, got.Current, tc.want)
		}
	}
}
