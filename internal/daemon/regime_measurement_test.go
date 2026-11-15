package daemon

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestFundingUsesSamePublicationJoinAtBothEnds(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	cpDate := now.AddDate(0, 0, -2)
	deps := &regimeDeps{now: func() time.Time { return now }, officialSeries: func(_ context.Context, id string) ([]regimeSeriesPoint, error) {
		if id == fredSeriesCP3M {
			return []regimeSeriesPoint{{Date: cpDate, Value: 4.2}}, nil
		}
		return []regimeSeriesPoint{{Date: cpDate, Value: 4.1}, {Date: cpDate.AddDate(0, 0, 1), Value: 5.0}}, nil
	}}
	got := fetchRegimeFundingStress(t.Context(), deps)
	if got.SpreadBps == nil || math.Abs(*got.SpreadBps-10) > 1e-8 {
		t.Fatalf("future bill print contaminated current spread: %+v", got)
	}
	if got.TBill3MQuality == nil || !got.TBill3MQuality.AsOf.Equal(cpDate) {
		t.Fatal("joined observation timestamp lost")
	}
}

func TestLiveHYGCannotFreshenOldHistory(t *testing.T) {
	now := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	var warnings []string
	deps := hygSpotMissDeps(now.AddDate(0, 0, -10), &warnings)
	deps.now = func() time.Time { return now }
	deps.snapshot = func(context.Context, string, time.Duration) snapshotQuote {
		return snapshotQuote{price: 79, dataType: "live", observedAt: now}
	}
	got := fetchRegimeHYGSPY(t.Context(), deps)
	if got.Status != rpc.RegimeStatusStale || !slices.Contains(got.FieldsMissing, "hyg_history_stale") {
		t.Fatalf("stale baseline promoted by live quote: %+v", got)
	}
	if (hygSpyStreaks{}).fresh(&rpc.RegimeSnapshotResult{HYGSPYDivergence: got}, now) {
		t.Fatal("stale baseline can confirm")
	}
	at := hygSPYRowAsOf(now, got)
	if !at.Time.Before(now.AddDate(0, 0, -9)) {
		t.Fatal("source summary hid stale baseline")
	}
}
