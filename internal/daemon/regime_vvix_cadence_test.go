package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestVVIXHolidayCadence(t *testing.T) {
	for _, tc := range []struct {
		name, now, date, status, want string
		value                         float64
	}{
		{"Labor Day preopen", "2026-09-08T07:00:00Z", "2026-09-04", "ok", "not_due", 84},
		{"Labor Day intraday", "2026-09-08T18:00:00Z", "2026-09-04", "ok", "not_due", 84},
		{"missed Tuesday close", "2026-09-08T22:00:00Z", "2026-09-04", "ok", "overdue", 84},
		{"missing Friday", "2026-09-08T07:00:00Z", "2026-09-03", "ok", "overdue", 84},
		{"ordinary budget", "2026-09-05T07:00:00Z", "2026-09-04", "ok", "fresh", 84},
		{"holiday", "2026-09-07T18:00:00Z", "2026-09-04", "ok", "fresh", 84},
		{"unknown calendar", "2029-09-04T07:00:00Z", "2029-08-31", "ok", "overdue", 84},
		{"future date", "2026-09-08T07:00:00Z", "2026-09-09", "ok", "overdue", 84},
		{"invalid date", "2026-09-08T07:00:00Z", "invalid", "ok", "overdue", 84},
		{"failed source", "2026-09-08T07:00:00Z", "2026-09-04", "error", "overdue", 84},
		{"zero", "2026-09-08T07:00:00Z", "2026-09-04", "ok", "overdue", 0},
		{"nan", "2026-09-08T07:00:00Z", "2026-09-04", "ok", "overdue", math.NaN()},
		{"infinite", "2026-09-08T07:00:00Z", "2026-09-04", "ok", "overdue", math.Inf(1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			res := &rpc.RegimeSnapshotResult{VolOfVol: rpc.RegimeVolOfVol{Status: tc.status, AsOfDate: tc.date, Last: &tc.value}}
			if got := volOfVolCadenceClass(res, now); got != tc.want {
				t.Fatalf("cadence=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestVVIXHolidayContextPropagatesWithoutConfirming(t *testing.T) {
	now := time.Date(2026, 9, 8, 7, 0, 0, 0, time.UTC)
	ratio, vvix := 0.85, 125.0
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale, Ratio: &ratio, VIX3MCrossCheck: rpc.VIX3MCrossCheckAgree,
			VIXQuality:   &rpc.Quality{AsOf: now, FreshnessClass: rpc.FreshnessFrozen},
			VIX3MQuality: &rpc.Quality{AsOf: now, FreshnessClass: rpc.FreshnessFrozen},
		},
		VolOfVol: rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, AsOfDate: "2026-09-04", Last: &vvix},
	}
	streaks := NewStreakStore(t.TempDir())
	policies := (&Server{}).populateStreaksWithStore(res, streaks)
	annotateRegimeMetadata(res, policies)
	if res.VolOfVol.Freshness.Class != rpc.RegimeFreshnessNotDue || res.VolOfVol.Streak != nil {
		t.Fatalf("holiday close banked a stress session: freshness=%+v streak=%+v", res.VolOfVol.Freshness, res.VolOfVol.Streak)
	}
	if res.VolOfVol.Eligibility != nil && res.VolOfVol.Eligibility.Eligible {
		t.Fatal("holiday context confirmed stress")
	}
	assertVol := func(want string) {
		t.Helper()
		for _, source := range rpc.BuildRegimeSourceHealth(res, now) {
			if source.Source == "vol" {
				if source.Status != want || (want == rpc.SourceStatusOK && source.RefreshState != rpc.SourceRefreshNotDue) {
					t.Fatalf("vol source=%+v, want %s", source, want)
				}
				return
			}
		}
		t.Fatal("missing vol source")
	}
	assertVol(rpc.SourceStatusOK)
	if res.VolOfVol.Freshness.NextDueAt == nil {
		t.Fatal("scheduled context lacks a publication deadline")
	}
	for _, source := range rpc.BuildRegimeSourceHealth(res, *res.VolOfVol.Freshness.NextDueAt) {
		if source.Source == "vol" && source.Status == rpc.SourceStatusOK {
			t.Fatal("retained snapshot stayed healthy at the next publication boundary")
		}
	}
	res.VIXTermStructure.Status = rpc.RegimeStatusOK
	for _, source := range rpc.BuildRegimeSourceHealth(res, *res.VolOfVol.Freshness.NextDueAt) {
		if source.Source == "vol" && source.Status == rpc.SourceStatusOK {
			t.Fatal("retained OK rows hid the expired publication deadline")
		}
	}
	res.VIXTermStructure.Status = rpc.RegimeStatusStale
	deadline := res.VolOfVol.Freshness.NextDueAt
	res.VolOfVol.Freshness.NextDueAt = nil
	assertVol(rpc.SourceStatusStale)
	res.VolOfVol.Freshness.NextDueAt = deadline
	res.VIXTermStructure.AsOf.Time = now.Add(-5 * 24 * time.Hour)
	assertVol(rpc.SourceStatusStale)
	res.VIXTermStructure.AsOf.Time = now
	res.VIXTermStructure.Freshness.Class = rpc.RegimeFreshnessOverdue
	assertVol(rpc.SourceStatusStale)
}
