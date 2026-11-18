package rpc

import (
	"slices"
	"testing"
)

func coSignsRed(r *RegimeSnapshotResult) {
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.USDJPY.RegimeIndicatorMeta, true)
	red(&r.GammaZero.RegimeIndicatorMeta, true)
	red(&r.Breadth.RegimeIndicatorMeta, true)
}

// A dark stress witness never reads as a calm one. With a co-sign red on the
// panel and a witness row that cannot be read, nothing says the witness is
// calm: the read is undefined, not quiet, and the rulebook's stage latch
// holds. Operator decision 2026-08-11: unknown cash evidence never softens a
// live warning. A witness on its own typed not-due schedule is not dark.
func TestRegimeDarkWitnessIsNeverCalm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  string
	}{
		{"every witness current and green, four co-signs red", coSignsRed, LifecycleQuiet},
		{"VIX3M lost: term row in error with its last band held green", func(r *RegimeSnapshotResult) {
			coSignsRed(r)
			r.VIXTermStructure.Status = RegimeStatusError
			r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
			r.VIXTermStructure.Ratio = nil
		}, LifecycleDataQuality},
		{"cash credit unavailable; proxy, breadth and yen red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			red(&r.USDJPY.RegimeIndicatorMeta, true)
			r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
		}, LifecycleDataQuality},
		{"cash credit stale; proxy red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads.Status = RegimeStatusStale
			r.CreditSpreads.Freshness = &RegimeFreshness{Class: RegimeFreshnessStale}
		}, LifecycleDataQuality},
		{"funding unavailable; breadth red", func(r *RegimeSnapshotResult) {
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.FundingStress = RegimeFundingStress{Status: RegimeStatusUnavailable}
		}, LifecycleDataQuality},
		{"VVIX unavailable; proxy red on a strong rebound", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.VolOfVol = RegimeVolOfVol{Status: RegimeStatusUnavailable}
			r.HYGSPYDivergence.SPYChangePct, r.VIXTermStructure.VIXChangePct = new(1.6), new(-11.0)
		}, LifecycleDataQuality},
		{"funding typed not-due but a month old; proxy red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.FundingStress.Status = RegimeStatusStale
			r.FundingStress.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
			r.FundingStress.AsOf = &RegimeAsOfSummary{Time: r.AsOf.AddDate(0, 0, -30)}
		}, LifecycleDataQuality},
		{"cash credit on its typed not-due schedule; proxy red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads.Status = RegimeStatusStale
			r.CreditSpreads.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
		}, LifecycleQuiet},
		{"cash credit unavailable and no co-sign red", func(r *RegimeSnapshotResult) {
			r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
		}, LifecycleQuiet},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			posture := regimePosture(r)
			if posture.Stage != tc.want {
				t.Fatalf("stage %s, headline %q, readiness %s; want %s", posture.Stage, posture.Label, posture.Readiness, tc.want)
			}
			if tc.want == LifecycleDataQuality && posture.Label != "Market state undefined — data incomplete" {
				t.Fatalf("headline %q, want the data-incomplete headline", posture.Label)
			}
		})
	}
}

// A witness row that was eligible in its window keeps standing through its
// own not-due window: a confirmed stress resting on a term inversion does not
// drop at the close and come back at the open. Overdue, stale or failed
// evidence still drops it, and a co-sign's prior-date reading stays context.
func TestRegimeWitnessStaysEligibleThroughItsNotDueWindow(t *testing.T) {
	eligible := func(indicator, class string, latched bool) bool {
		got := EvaluateRegimeEligibility(RegimeEligibilityInput{
			Indicator: indicator, Band: "red", Depth: new(1.06), StreakSessions: 2,
			Fresh: false, FreshnessClass: class, Latched: latched,
		})
		return got != nil && got.Eligible
	}
	if !eligible(RegimeIndicatorVIXTerm, RegimeFreshnessNotDue, true) {
		t.Error("a latched term inversion on its not-due schedule lost its eligibility")
	}
	if eligible(RegimeIndicatorVIXTerm, RegimeFreshnessNotDue, false) {
		t.Error("a term inversion that was never eligible became eligible off its window")
	}
	for _, class := range []string{RegimeFreshnessOverdue, RegimeFreshnessStale, RegimeFreshnessPending} {
		if eligible(RegimeIndicatorVIXTerm, class, true) {
			t.Errorf("a latched term inversion stayed eligible while %s", class)
		}
	}
	if eligible(RegimeIndicatorGammaZero, RegimeFreshnessNotDue, true) {
		t.Error("a latched gamma red stayed eligible on a prior-date reading")
	}
}

// An eligible witness red stands. The session's SPY and VIX moves do not
// decide whether it does: one tick must not turn every co-sign beside a held
// VVIX red from context into confirmation and back.
func TestRegimeEligibleWitnessStandsWhateverTheTape(t *testing.T) {
	stage := func(spy float64) (string, int) {
		r := calmRegime()
		red(&r.VolOfVol.RegimeIndicatorMeta, true)
		r.VolOfVol.Last = new(116.0)
		red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		red(&r.Breadth.RegimeIndicatorMeta, true)
		red(&r.USDJPY.RegimeIndicatorMeta, true)
		r.HYGSPYDivergence.SPYChangePct = new(spy)
		return regimeVote(r).Stage, BuildRegimeClusterBands(r).EligibleRedCount()
	}
	for _, spy := range []float64{0, -0.99, -1.0} {
		if got, reds := stage(spy); got != LifecycleConfirmedStress || reds != 4 {
			t.Errorf("held VVIX red, eligible, with three eligible co-signs and SPY %+.2f%%: stage %s with %d eligible reds; want confirmed_stress with 4", spy, got, reds)
		}
	}
}

// The widening opener is the cash row's: it opens while that row is current,
// whatever the proxy row beside it in the cluster can or cannot serve.
func TestRegimeCashWideningIsJudgedByTheCashRow(t *testing.T) {
	r := calmRegime()
	r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.7)
	r.HYGSPYDivergence.RegimeIndicatorMeta = RegimeIndicatorMeta{}
	r.HYGSPYDivergence.Status = RegimeStatusUnavailable
	if got := regimeVote(r); got.Stage != LifecycleEarlyWarning {
		t.Fatalf("cash widening with the proxy row unavailable: stage %s (readiness %s), want early_warning", got.Stage, got.Readiness)
	}
}

// When the tape confirms stress and every red is a co-sign, the read says
// what confirmed it: the tape. The co-signs stay context and the severity is
// the tape arm's own.
func TestRegimeTapeConfirmationNamesTheTape(t *testing.T) {
	r := calmRegime()
	coSignsRed(r)
	r.HYGSPYDivergence.SPYChangePct = new(-4.5)
	got := regimeVote(r)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" || !slices.Equal(got.ConfirmedBy, []string{"tape"}) || len(got.Suppressed) != 4 {
		t.Fatalf("stage %s, severity %s, confirmed_by %v, suppressed %v; want confirmed_stress at act, confirmed by the tape, four co-signs suppressed",
			got.Stage, got.Severity, got.ConfirmedBy, got.Suppressed)
	}

	// A witness that confirms is still named by its cluster.
	r = calmRegime()
	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	if got := regimeVote(r); !slices.Equal(got.ConfirmedBy, []string{"vol", "credit"}) {
		t.Fatalf("confirmed_by %v, want the clusters that confirm", got.ConfirmedBy)
	}
}

// A confirmed stress cannot rest on co-signs when the witness they lean on
// is broken: with the only standing witness in a defective cluster the read
// is undefined, however many co-signs are current and red.
func TestRegimeConfirmedStressDoesNotRestOnCoSignsWhenItsWitnessIsBroken(t *testing.T) {
	r := calmRegime()
	red(&r.VolOfVol.RegimeIndicatorMeta, true)
	r.VolOfVol.Last = new(125.0)
	r.VIXTermStructure.Band = "red"
	r.VIXTermStructure.Status = RegimeStatusError
	r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	if got := regimeVote(r); got.Stage != LifecycleDataQuality {
		t.Fatalf("stage %s (readiness %s, confirmed_by %v), want data_quality", got.Stage, got.Readiness, got.ConfirmedBy)
	}

	// With a second, clean witness the stress stands on current evidence.
	red(&r.FundingStress.RegimeIndicatorMeta, true)
	if got := regimeVote(r); got.Stage != LifecycleConfirmedStress {
		t.Fatalf("with funding an eligible red: stage %s, want confirmed_stress", got.Stage)
	}
}
