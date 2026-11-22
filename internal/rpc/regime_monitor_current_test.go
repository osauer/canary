package rpc

import (
	"testing"
	"time"
)

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

// A reading older than its cluster's age bound is not a current rating, even
// on a shut venue: USD/JPY served from a daily bar ten days old read as
// current while the FX cluster was already overdue.
func TestRegimeMonitorCurrentVerdictHasAnAgeBound(t *testing.T) {
	now := time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{{36 * time.Hour, true}, {10 * 24 * time.Hour, false}} {
		r := &RegimeSnapshotResult{AsOf: now, USDJPY: RegimeUSDJPY{Status: RegimeStatusStale, RegimeIndicatorMeta: RegimeIndicatorMeta{
			Band: "green", Freshness: &RegimeFreshness{Class: RegimeFreshnessNotDue}, AsOf: &RegimeAsOfSummary{Time: now.Add(-tc.age)},
		}}}
		if got := RegimeIndicatorCurrent(r, RegimeIndicatorUSDJPY); got != tc.want {
			t.Errorf("reading %s old: current=%v, want %v", tc.age, got, tc.want)
		}
		for _, row := range CompactRegimeMonitor(r).Indicators {
			if row.Name == "USD/JPY" && row.Current != tc.want {
				t.Errorf("monitor row %s old: current=%v, want %v", tc.age, row.Current, tc.want)
			}
		}
	}
}
