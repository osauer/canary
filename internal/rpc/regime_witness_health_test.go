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

// regimeWitnessUnavailable is the opener of a warning that a dark stress
// witness raises beside a weak co-sign (rpc.RegimeWarningWitnessUnavailable).
const regimeWitnessUnavailable = "witness_unavailable"

func coSignsYellow(r *RegimeSnapshotResult) {
	r.HYGSPYDivergence.Band, r.Breadth.Band, r.USDJPY.Band = "yellow", "yellow", "yellow"
}

func termRowLost(r *RegimeSnapshotResult) {
	// VIX3M without a tick: the row errors and its last band is held green.
	r.VIXTermStructure.Status = RegimeStatusError
	r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	r.VIXTermStructure.Ratio = nil
}

// A dark stress witness beside a weak co-sign reads early_warning. Owner
// ruling 2026-11-18 14:47 CET: asked "When a real stress input is dark and
// weak readings are yellow or red, what should Regime say?", the owner
// answered "Early warning", which keeps the operator decision of 2026-08-11
// that unknown cash evidence never softens a live warning. The headline is
// the Watch headline for a signal that warns without confirming, the tone is
// watch, and the opener names the unavailable witness. A witness on its own
// typed not-due schedule is not dark, a healthy panel with co-signs red stays
// quiet, and a dark witness with every co-sign green changes nothing.
func TestRegimeDarkWitnessBesideAWeakCoSignIsAnEarlyWarning(t *testing.T) {
	const watch = "Watch: one unconfirmed stress signal"
	for _, tc := range []struct {
		name         string
		shape        func(r *RegimeSnapshotResult)
		stage, label string
		tone         string
		opener       bool
	}{
		{"term row lost, VVIX green; proxy, breadth and yen yellow; SPY -1.4%", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			coSignsYellow(r)
			r.HYGSPYDivergence.SPYChangePct = new(-1.4)
		}, LifecycleEarlyWarning, watch, RegimeToneWatch, true},
		{"term row lost; proxy, breadth and yen red; SPY -1.4%", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			red(&r.USDJPY.RegimeIndicatorMeta, true)
			r.HYGSPYDivergence.SPYChangePct = new(-1.4)
		}, LifecycleEarlyWarning, watch, RegimeToneWatch, true},
		{"cash credit unavailable; proxy red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
		}, LifecycleEarlyWarning, watch, RegimeToneWatch, true},
		{"funding overdue; breadth yellow", func(r *RegimeSnapshotResult) {
			r.Breadth.Band = "yellow"
			r.FundingStress.Status = RegimeStatusStale
			r.FundingStress.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}, LifecycleEarlyWarning, watch, RegimeToneWatch, true},
		{"VVIX unavailable; proxy red on a strong rebound", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.VolOfVol = RegimeVolOfVol{Status: RegimeStatusUnavailable}
			r.HYGSPYDivergence.SPYChangePct, r.VIXTermStructure.VIXChangePct = new(1.6), new(-11.0)
		}, LifecycleEarlyWarning, watch, RegimeToneWatch, true},
		{"control: every witness current and green, four co-signs red", coSignsRed, LifecycleQuiet, "Normal regime", RegimeToneNormal, false},
		{"control: term row lost, every co-sign green", termRowLost, LifecycleQuiet, "Normal regime", RegimeToneDataQuality, false},
		{"control: term row on its typed not-due schedule after the close; proxy red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.VIXTermStructure.Status = RegimeStatusStale
			r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
			r.VolOfVol.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
		}, LifecycleQuiet, "Normal regime", RegimeToneNormal, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			posture := regimePosture(r)
			if posture.Stage != tc.stage || posture.Label != tc.label || posture.Tone != tc.tone {
				t.Fatalf("stage %s, headline %q, tone %s (readiness %s); want %s, %q, %s",
					posture.Stage, posture.Label, posture.Tone, posture.Readiness, tc.stage, tc.label, tc.tone)
			}
			if got := slices.Contains(RegimeWarningOpeners(r), regimeWitnessUnavailable); got != tc.opener {
				t.Fatalf("openers %v; want %s listed: %v", RegimeWarningOpeners(r), regimeWitnessUnavailable, tc.opener)
			}
		})
	}
}

// Two defective clusters still blank the read: the existing floor of the
// required-input gate (RegimeCurrencyBlankFloor) decides, and the headline
// says data incomplete. The rulebook's stage latch then holds.
func TestRegimeTwoDarkClustersStillBlankTheRead(t *testing.T) {
	r := calmRegime()
	termRowLost(r)
	r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	if posture := regimePosture(r); posture.Stage != LifecycleDataQuality || posture.Label != "Market state undefined — data incomplete" {
		t.Fatalf("stage %s, headline %q; want data_quality under the data-incomplete headline", posture.Stage, posture.Label)
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
// has no current reading. After the close the term inversion stands on its
// not-due schedule, latched; with VVIX unavailable at the same time the vol
// cluster is defective and the latched red is not a current reading, so the
// proxy and breadth red cannot hold the stress. The stage falls to what its
// current evidence proves: the dark VVIX beside the red co-signs is the early
// warning, disclosed as a downgrade. With a second, clean witness it stands.
func TestRegimeConfirmedStressDoesNotRestOnCoSignsWhenItsWitnessIsBroken(t *testing.T) {
	r := calmRegime()
	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	r.VIXTermStructure.Eligibility.Latched = true
	r.VIXTermStructure.Status = RegimeStatusStale
	r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
	r.VolOfVol = RegimeVolOfVol{Status: RegimeStatusUnavailable}
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	if got := regimeVote(r); got.Stage != LifecycleEarlyWarning || len(got.ConfirmedBy) != 0 ||
		!slices.ContainsFunc(got.Governors, func(g GovernorAction) bool { return g.Action == "stage_downgraded" && g.To == LifecycleEarlyWarning }) {
		t.Fatalf("stage %s (readiness %s, confirmed_by %v, governors %+v), want early_warning by a disclosed downgrade", got.Stage, got.Readiness, got.ConfirmedBy, got.Governors)
	}

	red(&r.FundingStress.RegimeIndicatorMeta, true)
	if got := regimeVote(r); got.Stage != LifecycleConfirmedStress {
		t.Fatalf("with funding an eligible red: stage %s, want confirmed_stress", got.Stage)
	}
}

// The dark-witness test reads the row-level parts of the input verdict: a
// witness row that cannot fill a field it needs to band, or a current-typed
// row older than its cluster's age bound, is dark although its class says
// fresh.
func TestRegimeDarkWitnessReadsTheRowsOwnDefects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
	}{
		{"term row without VIX3M, band unranked", func(r *RegimeSnapshotResult) {
			r.VIXTermStructure.Band, r.VIXTermStructure.Ratio = "", nil
			r.VIXTermStructure.FieldsMissing = []string{"vix3m"}
		}},
		{"funding typed fresh, eight days old", func(r *RegimeSnapshotResult) {
			r.FundingStress.AsOf = &RegimeAsOfSummary{Time: r.AsOf.AddDate(0, 0, -8)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			if got := regimeVote(r); got.Stage != LifecycleEarlyWarning || !slices.Contains(RegimeWarningOpeners(r), regimeWitnessUnavailable) {
				t.Fatalf("stage %s (readiness %s), openers %v; want early_warning opened by the unavailable witness", got.Stage, got.Readiness, RegimeWarningOpeners(r))
			}
		})
	}
}

// A witness is judged by its own row. A cluster-mate's outage does not mute a
// current witness red: the cash red stands with the proxy row unavailable,
// and VVIX at 125 stands with the term row in error.
func TestRegimeWitnessIsJudgedByItsOwnRow(t *testing.T) {
	proxyLost := func(r *RegimeSnapshotResult) {
		r.HYGSPYDivergence = RegimeHYGSPYDivergence{Status: RegimeStatusUnavailable}
	}
	cashRed := func(r *RegimeSnapshotResult) {
		red(&r.CreditSpreads.RegimeIndicatorMeta, true)
		r.CreditSpreads.HYOAS = new(5.8)
	}
	for _, tc := range []struct {
		name   string
		shape  func(r *RegimeSnapshotResult)
		stage  string
		opener string
	}{
		{"cash red, proxy row unavailable", func(r *RegimeSnapshotResult) {
			cashRed(r)
			proxyLost(r)
		}, LifecycleEarlyWarning, RegimeWarningEligibleRed},
		{"term row in error, VVIX 125", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			r.VolOfVol.Last = new(125.0)
		}, LifecycleEarlyWarning, RegimeWarningEligibleRed},
		{"term row in error, VVIX 125, proxy and breadth red", func(r *RegimeSnapshotResult) {
			termRowLost(r)
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			r.VolOfVol.Last = new(125.0)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
		}, LifecycleConfirmedStress, RegimeWarningEligibleRed},
		{"cash red and funding red, proxy row unavailable", func(r *RegimeSnapshotResult) {
			cashRed(r)
			red(&r.FundingStress.RegimeIndicatorMeta, true)
			proxyLost(r)
		}, LifecycleConfirmedStress, RegimeWarningEligibleRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			got := regimeVote(r)
			if got.Stage != tc.stage || !slices.Contains(RegimeWarningOpeners(r), tc.opener) {
				t.Fatalf("stage %s (readiness %s, confirmed_by %v), openers %v; want %s opened by %s",
					got.Stage, got.Readiness, got.ConfirmedBy, RegimeWarningOpeners(r), tc.stage, tc.opener)
			}
		})
	}
}

// Panic is a fall on a current tape. With an input impaired and the SPY print
// not current, three eligible reds and SPY down 4.5% are confirmed stress,
// confirmed by those clusters at the act grade, and the downgrade is
// disclosed; with the print current they are panic.
func TestRegimePanicNeedsACurrentTape(t *testing.T) {
	shape := func(current bool) *RegimeSnapshotResult {
		r := calmRegime()
		red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
		red(&r.FundingStress.RegimeIndicatorMeta, true)
		red(&r.Breadth.RegimeIndicatorMeta, true)
		r.HYGSPYDivergence.SPYChangePct = new(-4.5)
		// The proxy row the SPY print rides on is stale, an input defect, so
		// the stage must survive on current evidence. The print itself is
		// judged by its own currency.
		r.HYGSPYDivergence.Status = RegimeStatusStale
		r.HYGSPYDivergence.Freshness = &RegimeFreshness{Class: RegimeFreshnessStale}
		if !current {
			r.HYGSPYDivergence.SPYQuality.FreshnessClass = FreshnessFrozen
		}
		return r
	}
	got := regimeVote(shape(false))
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" || !slices.Equal(got.ConfirmedBy, []string{"vol", "funding", "breadth"}) {
		t.Fatalf("tape not current: stage %s, severity %s, confirmed_by %v; want confirmed_stress at act, confirmed by vol, funding and breadth",
			got.Stage, got.Severity, got.ConfirmedBy)
	}

	if !slices.ContainsFunc(got.Governors, func(g GovernorAction) bool {
		return g.Action == "stage_downgraded" && g.From == LifecyclePanic && g.To == LifecycleConfirmedStress && g.Reason == "tape_not_current"
	}) {
		t.Fatalf("governors %+v; want the downgrade from panic disclosed", got.Governors)
	}
	if got := regimeVote(shape(true)); got.Stage != LifecyclePanic {
		t.Fatalf("tape current: stage %s, want panic", got.Stage)
	}
}
