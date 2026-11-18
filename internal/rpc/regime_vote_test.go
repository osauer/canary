package rpc

import (
	"slices"
	"testing"
	"time"
)

// calmRegime is a snapshot with all eight rows current and green on a flat
// tape, with source health from the real builder. Each case changes only the
// rows it is about, so the required-input gate has nothing to say unless the
// case breaks an input on purpose.
func calmRegime() *RegimeSnapshotResult {
	asOf := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	meta := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:      "green",
			Freshness: &RegimeFreshness{Class: RegimeFreshnessFresh},
			AsOf:      &RegimeAsOfSummary{Time: asOf},
		}
	}
	r := &RegimeSnapshotResult{AsOf: asOf, TapeSessionState: TapeSessionTradingDate}
	r.VIXTermStructure = RegimeVIXTerm{
		RegimeIndicatorMeta: meta(), Status: RegimeStatusOK,
		VIX: new(16.0), VIX3M: new(18.0), Ratio: new(16.0 / 18.0), VIXChangePct: new(0.0),
		VIXQuality: &Quality{AsOf: asOf, FreshnessClass: FreshnessLive},
	}
	r.VolOfVol = RegimeVolOfVol{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK, Last: new(85.0)}
	r.HYGSPYDivergence = RegimeHYGSPYDivergence{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK, SPYChangePct: new(0.0)}
	r.CreditSpreads = RegimeCreditSpreads{
		RegimeIndicatorMeta: meta(), Status: RegimeStatusOK,
		HYOAS: new(3.0), HY20DChange: new(0.1), AsOfDate: asOf.Format("2006-01-02"),
	}
	r.FundingStress = RegimeFundingStress{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK}
	r.USDJPY = RegimeUSDJPY{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK, Last: new(150.0)}
	r.GammaZero = RegimeGammaZero{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK}
	r.GammaZero.Envelope.Result = &GammaZeroComputed{Quality: &GammaSignalQuality{Rankability: GammaRankabilityRankable}}
	r.Breadth = RegimeBreadth{RegimeIndicatorMeta: meta(), Status: RegimeStatusOK}
	r.Breadth.Envelope = BreadthSPXResult{State: BreadthStateReady, SessionKey: asOf.Format("2006-01-02")}
	return r
}

// red turns a row red, eligible to confirm or provisional.
func red(meta *RegimeIndicatorMeta, eligible bool) {
	meta.Band = "red"
	meta.Eligibility = &RegimeEligibility{Eligible: eligible}
}

// regimeVote finishes the snapshot as the daemon does and returns its
// lifecycle.
func regimeVote(r *RegimeSnapshotResult) LifecycleState {
	r.SourceHealth = BuildRegimeSourceHealth(r, r.AsOf)
	return BuildRegimeLifecycle(r)
}

// Only a stress witness starts a warning: the equity-vol cluster, funding, or
// the cash credit row. The HYG proxy, breadth, the yen and dealer gamma are
// co-signs: they strengthen a warning a witness has started and open nothing
// by themselves, alone or together. Panic needs a fall on the tape.
//
// The shapes are the ones the history replay found: the HYG proxy below its
// average on most sessions of 2026 while cash spreads read calm, proxy plus
// breadth reading confirmed stress on 2026-10-14, and proxy, breadth and yen
// reading panic on 2026-09-10 with SPY flat.
func TestRegimeVoteOnlyAStressWitnessStartsAWarning(t *testing.T) {
	if calm := regimeVote(calmRegime()); calm.Stage != LifecycleQuiet || calm.Readiness != "ready" {
		t.Fatalf("the calm fixture reads %s/%s, want quiet/ready", calm.Stage, calm.Readiness)
	}
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  string
	}{
		{"proxy red, cash credit unavailable", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
		}, LifecycleQuiet},
		{"proxy red, cash credit six days old", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads.AsOfDate = r.AsOf.AddDate(0, 0, -6).Format("2006-01-02")
		}, LifecycleQuiet},
		{"proxy and breadth red, cash credit widening half a point", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
		}, LifecycleQuiet},
		{"proxy, breadth and yen red on a flat tape", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			red(&r.USDJPY.RegimeIndicatorMeta, true)
		}, LifecycleQuiet},
		{"gamma red alone", func(r *RegimeSnapshotResult) {
			red(&r.GammaZero.RegimeIndicatorMeta, true)
		}, LifecycleQuiet},
		{"gamma, proxy and breadth yellow", func(r *RegimeSnapshotResult) {
			r.GammaZero.Band, r.HYGSPYDivergence.Band, r.Breadth.Band = "yellow", "yellow", "yellow"
		}, LifecycleQuiet},
		{"proxy and breadth red beside a yellow VVIX", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.VolOfVol.Band = "yellow"
		}, LifecycleEarlyWarning},
		{"term inversion on its first day", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, false)
		}, LifecycleEarlyWarning},
		{"deep term inversion with the proxy red", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleConfirmedStress},
		{"isolated VVIX red with the proxy red", func(r *RegimeSnapshotResult) {
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			r.VolOfVol.Last = new(112.0)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleEarlyWarning},
		{"three reds with a witness on a flat tape", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
		}, LifecycleConfirmedStress},
		{"three reds with a witness and SPY down 4.5%", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.HYGSPYDivergence.SPYChangePct = new(-4.5)
		}, LifecyclePanic},
		{"cash credit red alone", func(r *RegimeSnapshotResult) {
			red(&r.CreditSpreads.RegimeIndicatorMeta, true)
			r.CreditSpreads.HYOAS = new(5.8)
		}, LifecycleEarlyWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			if got := regimeVote(r); got.Stage != tc.want {
				t.Fatalf("stage = %s (readiness %s, unconfirmed %v), want %s", got.Stage, got.Readiness, got.Unconfirmed, tc.want)
			}
		})
	}
}

// A co-sign red that opens nothing stays on the panel: the quiet read names
// it as unconfirmed and as suppressed context.
func TestRegimeCoSignRedIsContextOnAQuietDay(t *testing.T) {
	r := calmRegime()
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	got := regimeVote(r)
	if got.Stage != LifecycleQuiet || !slices.Contains(got.Unconfirmed, "credit") || !slices.Contains(got.Suppressed, "credit") {
		t.Fatalf("stage %s, unconfirmed %v, suppressed %v; want quiet with credit named in both", got.Stage, got.Unconfirmed, got.Suppressed)
	}
}

// The cash row stands on its own evidence. A cash red that may not confirm is
// a provisional witness even when the proxy beside it is eligible; the proxy
// counts once another witness stands.
func TestRegimeCashCreditRedStandsOnItsOwnEvidence(t *testing.T) {
	r := calmRegime()
	red(&r.CreditSpreads.RegimeIndicatorMeta, false)
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	cb := BuildRegimeClusterBands(r)
	if cb.Confirmed[RegimeClusterCredit] != "red" || cb.EligibleRedCount() != 0 {
		t.Fatalf("credit = %s with %d eligible reds, want a red that confirms nothing", cb.Confirmed[RegimeClusterCredit], cb.EligibleRedCount())
	}
	if stage := regimeVote(r).Stage; stage != LifecycleEarlyWarning {
		t.Fatalf("stage = %s, want early_warning from the provisional cash red", stage)
	}

	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	if got := BuildRegimeClusterBands(r).EligibleRedCount(); got != 2 {
		t.Fatalf("eligible reds = %d, want 2: the term inversion stands and the proxy co-signs it", got)
	}
}

// A warning cannot outlive a broken witness on a co-sign's back: when the
// only cluster that opens it is defective, the read is undefined, whatever
// the co-signs show.
func TestRegimeWarningDoesNotRestOnACoSignWhenItsWitnessIsBroken(t *testing.T) {
	r := calmRegime()
	red(&r.VIXTermStructure.RegimeIndicatorMeta, false)
	r.VolOfVol = RegimeVolOfVol{Status: RegimeStatusError}
	r.HYGSPYDivergence.Band = "yellow"
	if got := regimeVote(r); got.Stage != LifecycleDataQuality {
		t.Fatalf("stage = %s (readiness %s), want data_quality", got.Stage, got.Readiness)
	}
}

// RegimeWarningOpeners names each arm that opens a warning, so a report can
// say what a warning rested on. A co-sign red is not among them.
func TestRegimeWarningOpenersNameTheArm(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  []string
	}{
		{"nothing red", func(*RegimeSnapshotResult) {}, nil},
		{"co-signs red", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.USDJPY.RegimeIndicatorMeta, true)
		}, nil},
		{"term inversion on its first day", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, false)
		}, []string{"provisional_witness:vol"}},
		{"cash credit red with the proxy and a VIX jump", func(r *RegimeSnapshotResult) {
			red(&r.CreditSpreads.RegimeIndicatorMeta, true)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.VIXTermStructure.VIXChangePct = new(12.0)
		}, []string{"eligible_red", "vix_tape"}},
		{"a yellow VVIX beside two co-sign reds and a falling SPY", func(r *RegimeSnapshotResult) {
			r.VolOfVol.Band = "yellow"
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.HYGSPYDivergence.SPYChangePct = new(-1.6)
		}, []string{"three_yellows", "spy_tape"}},
	} {
		r := calmRegime()
		tc.shape(r)
		if got := RegimeWarningOpeners(r); !slices.Equal(got, tc.want) {
			t.Errorf("%s: openers = %v, want %v", tc.name, got, tc.want)
		}
	}
}
