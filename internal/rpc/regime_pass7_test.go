package rpc

import (
	"slices"
	"testing"
)

// The blanking floor counts the clusters whose stress witness is dark, not a
// co-sign cluster's outage, while a dark witness warns beside a current weak
// co-sign: one dark witness and an overdue co-sign row are the early
// warning, two dark witnesses are data incomplete.
func TestRegimeBlankFloorCountsDarkWitnessesBesideTheWarning(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  string
	}{
		{"term row lost; proxy yellow; breadth overdue", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			r.HYGSPYDivergence.Band = "yellow"
			r.Breadth.Status = RegimeStatusStale
			r.Breadth.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}, LifecycleEarlyWarning},
		{"term row lost; proxy yellow; gamma overdue at yellow", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			r.HYGSPYDivergence.Band = "yellow"
			r.GammaZero.Band, r.GammaZero.Status = "yellow", RegimeStatusStale
			r.GammaZero.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}, LifecycleEarlyWarning},
		{"term row and funding lost; proxy yellow", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			r.FundingStress = RegimeFundingStress{Status: RegimeStatusError}
			r.HYGSPYDivergence.Band = "yellow"
		}, LifecycleDataQuality},
		{"term row lost; only an overdue co-sign is weak", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			r.GammaZero.Band, r.GammaZero.Status = "yellow", RegimeStatusStale
			r.GammaZero.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
			r.Breadth.Status = RegimeStatusStale
			r.Breadth.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}, LifecycleDataQuality},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			if got := regimeVote(r); got.Stage != tc.want {
				t.Fatalf("stage %s (readiness %s), openers %v; want %s", got.Stage, got.Readiness, RegimeWarningOpeners(r), tc.want)
			}
			// The opener the stress read words is open exactly when the
			// stage is the warning it names.
			if open := slices.Contains(RegimeWarningOpeners(r), regimeWitnessUnavailable); open != (tc.want == LifecycleEarlyWarning) {
				t.Fatalf("openers %v with stage %s", RegimeWarningOpeners(r), tc.want)
			}
		})
	}
}

// A crash day is judged by the SPY print's own currency, not by the row
// beside it, and a stress stage that cannot survive an input defect falls to
// the highest stage its current evidence proves.
func TestRegimeCrashDayIsNotDataIncompleteBecauseANeighbourIsDown(t *testing.T) {
	proxyStale := func(r *RegimeSnapshotResult) {
		r.HYGSPYDivergence.Status = RegimeStatusStale
		r.HYGSPYDivergence.Freshness = &RegimeFreshness{Class: RegimeFreshnessStale}
	}
	vvix140 := func(r *RegimeSnapshotResult) {
		red(&r.VolOfVol.RegimeIndicatorMeta, true)
		r.VolOfVol.Last = new(140.0)
	}
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  []string
	}{
		{"SPY -7.5% current, HYG row stale", proxyStale, []string{LifecyclePanic}},
		{"SPY -7.5% current, cash row unavailable", func(r *RegimeSnapshotResult) {
			r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
		}, []string{LifecyclePanic}},
		{"SPY -7.5% current, HYG row stale, VVIX 140", func(r *RegimeSnapshotResult) {
			proxyStale(r)
			vvix140(r)
		}, []string{LifecyclePanic}},
		{"SPY print not current, HYG row stale, VVIX 140", func(r *RegimeSnapshotResult) {
			proxyStale(r)
			vvix140(r)
			r.HYGSPYDivergence.SPYQuality.FreshnessClass = FreshnessFrozen
		}, []string{LifecycleEarlyWarning, LifecycleConfirmedStress}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			r.HYGSPYDivergence.SPYChangePct = new(-7.5)
			tc.shape(r)
			if got := regimeVote(r); !slices.Contains(tc.want, got.Stage) {
				t.Fatalf("stage %s (severity %s, readiness %s); want one of %v", got.Stage, got.Severity, got.Readiness, tc.want)
			}
		})
	}
}

// Inside a dark-witness read a co-sign red is the named cause: its standing
// says weak with the served sentence, once, and not context.
func TestRegimeCoSignRedBesideADarkWitnessStandsAsWeak(t *testing.T) {
	r := calmRegime()
	termRowLost(r)
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	for _, s := range RegimeRowStandings(r) {
		if s.Indicator == RegimeIndicatorHYGSPY && (s.Red != "weak" || s.Note == "" || s.Note == RegimeContextNote) {
			t.Fatalf("proxy standing %+v; want weak with its own sentence", s)
		}
	}
	// Outside that state the context sentence stays.
	r = calmRegime()
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	for _, s := range RegimeRowStandings(r) {
		if s.Indicator == RegimeIndicatorHYGSPY && (s.Red != RegimeRedContext || s.Note != RegimeContextNote) {
			t.Fatalf("proxy standing %+v; want context", s)
		}
	}
}
