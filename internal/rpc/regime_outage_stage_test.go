package rpc

import "testing"

// heldRed is a row whose banding input went dark: status error, cadence
// overdue, its last red band held from memory. confirmed marks a red that
// had confirmed before the input went dark (the daemon's latch).
func heldRed(meta *RegimeIndicatorMeta, status *string, confirmed bool) {
	*status = RegimeStatusError
	meta.Band = "red"
	meta.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	meta.Eligibility = nil
	if confirmed {
		meta.Eligibility = &RegimeEligibility{Eligible: false, Latched: true, Reasons: []string{RegimeEligibilityInputUnavailable}}
	}
}

// 2026-11-21 06:30 CET: TWS lost its IBKR link. VIX/VIX3M and HYG/SPY lost
// their spot ticks and held their red bands; VVIX stayed an eligible red on
// Cboe's close. The vol and credit clusters had confirmed stress at 05:44,
// and the outage alone took credit's confirmation away, so the stage read
// early_warning and on a trading date would have relaxed the Rulebook. A
// stage that reads calmer only because an outage hid a confirmation cannot
// prove itself: the required-input gate blanks it to data_quality, which
// holds the Rulebook's latch. A held red that never confirmed hides nothing
// and leaves the dark-witness early warning of 2026-11-18 as it was.
func TestRegimeOutageNeverReadsCalmerThanTheConfirmationItHid(t *testing.T) {
	outage := func(confirmed bool) *RegimeSnapshotResult {
		r := calmRegime()
		red(&r.VolOfVol.RegimeIndicatorMeta, true)
		heldRed(&r.VIXTermStructure.RegimeIndicatorMeta, &r.VIXTermStructure.Status, confirmed)
		r.VIXTermStructure.Ratio, r.VIXTermStructure.VIX, r.VIXTermStructure.VIXQuality = nil, nil, nil
		heldRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, &r.HYGSPYDivergence.Status, confirmed)
		r.HYGSPYDivergence.SPYChangePct, r.HYGSPYDivergence.SPYQuality = nil, nil
		red(&r.GammaZero.RegimeIndicatorMeta, false)
		r.Breadth.Band = "yellow"
		return r
	}
	if got := regimeVote(outage(true)); got.Stage != LifecycleDataQuality || len(got.ConfirmedBy) != 0 {
		t.Fatalf("outage that hid a confirmation reads %s (confirmed by %v), want data_quality", got.Stage, got.ConfirmedBy)
	}
	if got := regimeVote(outage(false)); got.Stage != LifecycleEarlyWarning {
		t.Fatalf("outage of reds that never confirmed reads %s, want early_warning", got.Stage)
	}

	restored := calmRegime()
	red(&restored.VolOfVol.RegimeIndicatorMeta, true)
	red(&restored.VIXTermStructure.RegimeIndicatorMeta, true)
	red(&restored.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&restored.GammaZero.RegimeIndicatorMeta, false)
	restored.Breadth.Band = "yellow"
	if got := regimeVote(restored); got.Stage != LifecycleConfirmedStress {
		t.Fatalf("inputs back read %s, want confirmed_stress", got.Stage)
	}
}

// latchedOverdue is a red served frozen past its window: it still bands, its
// currency blocks confirmation, and the latch it earned rides along.
func latchedOverdue(meta *RegimeIndicatorMeta, status *string) {
	*status = RegimeStatusStale
	meta.Band = "red"
	meta.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	meta.Eligibility = &RegimeEligibility{Latched: true, Reasons: []string{"data_overdue"}}
}

// The counterfactual re-reads the snapshot under the witness and co-sign
// rules, never by adding a cluster per dark red (Fable review 2026-11-21).
func TestRegimeOutageCounterfactualKeepsTheWitnessRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  string
	}{
		{"sole witness dark beside a live confirming proxy", func(r *RegimeSnapshotResult) {
			heldRed(&r.VIXTermStructure.RegimeIndicatorMeta, &r.VIXTermStructure.Status, true)
			r.VIXTermStructure.Ratio, r.VIXTermStructure.VIX, r.VIXTermStructure.VIXQuality = nil, nil, nil
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleDataQuality},
		{"latched co-signs dark with no witness ever standing", func(r *RegimeSnapshotResult) {
			heldRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, &r.HYGSPYDivergence.Status, true)
			r.HYGSPYDivergence.SPYChangePct, r.HYGSPYDivergence.SPYQuality = nil, nil
			heldRed(&r.USDJPY.RegimeIndicatorMeta, &r.USDJPY.Status, true)
			r.FundingStress = RegimeFundingStress{Status: RegimeStatusError}
			r.Breadth.Band = "yellow"
		}, LifecycleEarlyWarning},
		{"latched proxy dark beside a live VIX spike", func(r *RegimeSnapshotResult) {
			heldRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, &r.HYGSPYDivergence.Status, true)
			r.VIXTermStructure.VIXChangePct = new(22.0)
		}, LifecycleEarlyWarning},
		{"frozen past the window instead of missing", func(r *RegimeSnapshotResult) {
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			latchedOverdue(&r.VIXTermStructure.RegimeIndicatorMeta, &r.VIXTermStructure.Status)
			latchedOverdue(&r.HYGSPYDivergence.RegimeIndicatorMeta, &r.HYGSPYDivergence.Status)
			red(&r.GammaZero.RegimeIndicatorMeta, false)
			r.Breadth.Band = "yellow"
		}, LifecycleDataQuality},
	} {
		r := calmRegime()
		tc.shape(r)
		got := regimeVote(r)
		if got.Stage != tc.want {
			t.Errorf("%s: stage %s (governors %+v), want %s", tc.name, got.Stage, got.Governors, tc.want)
		}
	}
}

// A red whose currency blocks confirmation keeps the latch it earned on the
// eligibility it serves, so the lifecycle can see the confirmation it hides.
func TestRegimeEligibilityCarriesTheLatchThroughACurrencyGap(t *testing.T) {
	got := EvaluateRegimeEligibility(RegimeEligibilityInput{Indicator: RegimeIndicatorHYGSPY, Band: "red", StreakSessions: 3, FreshnessClass: RegimeFreshnessOverdue, Latched: true})
	if got == nil || got.Eligible || !got.Latched {
		t.Fatalf("overdue latched red: %+v, want not eligible and latched", got)
	}
}
