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
// breadth reading confirmed stress on 2026-10-14 (now an early warning, which
// the widening cash spread opens), and proxy, breadth and yen reading panic
// on 2026-09-10 with SPY flat.
func TestRegimeVoteOnlyAStressWitnessStartsAWarning(t *testing.T) {
	if calm := regimeVote(calmRegime()); calm.Stage != LifecycleQuiet || calm.Readiness != "ready" {
		t.Fatalf("the calm fixture reads %s/%s, want quiet/ready", calm.Stage, calm.Readiness)
	}
	for _, tc := range []struct {
		name  string
		shape func(r *RegimeSnapshotResult)
		want  string
	}{
		{"proxy red, cash credit current and green", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleQuiet},
		{"proxy and breadth red, cash credit widening half a point", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
		}, LifecycleEarlyWarning},
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
		{"a VVIX red that may not confirm yet, with the proxy red", func(r *RegimeSnapshotResult) {
			red(&r.VolOfVol.RegimeIndicatorMeta, false)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleEarlyWarning},
		{"an eligible VVIX red with the proxy red", func(r *RegimeSnapshotResult) {
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleConfirmedStress},
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

// regimePosture finishes the snapshot as the daemon does and returns what a
// reader sees: the stage, the headline and the tone.
func regimePosture(r *RegimeSnapshotResult) RegimePosture {
	ApplyRegimeClusterTallies(&r.Composite, BuildRegimeClusterBands(r))
	r.Lifecycle = regimeVote(r)
	return BuildRegimePosture(r)
}

// Every surface follows the vote. A co-sign alone is context: it changes
// neither the stage, nor the headline, nor the tone. The Watch headlines are for a stress
// witness that warns without confirming, which includes cash credit spreads
// widening by the cash row's own yellow step.
func TestRegimeHeadlineAndToneFollowTheVote(t *testing.T) {
	for _, tc := range []struct {
		name               string
		shape              func(r *RegimeSnapshotResult)
		stage, label, tone string
	}{
		{"proxy red alone", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, LifecycleQuiet, "Normal regime", RegimeToneNormal},
		{"proxy, yen and breadth yellow", func(r *RegimeSnapshotResult) {
			r.HYGSPYDivergence.Band, r.USDJPY.Band, r.Breadth.Band = "yellow", "yellow", "yellow"
		}, LifecycleQuiet, "Normal regime", RegimeToneNormal},
		{"proxy red on a day VIX jumps 12%", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.VIXTermStructure.VIXChangePct = new(12.0)
		}, LifecycleEarlyWarning, "Normal regime", RegimeToneWatch},
		{"proxy red on a day SPY gains 1%", func(r *RegimeSnapshotResult) {
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.HYGSPYDivergence.SPYChangePct = new(1.0)
		}, LifecycleQuiet, "Normal regime", RegimeToneNormal},
		// Opportunity counts every cluster at yellow, co-signs included, as it
		// did before the roles: two yellows are one too many. With no witness
		// among them the day is not stabilization either.
		{"proxy and breadth yellow, SPY +1.5% and VIX -10%", func(r *RegimeSnapshotResult) {
			r.HYGSPYDivergence.Band, r.Breadth.Band = "yellow", "yellow"
			r.HYGSPYDivergence.SPYChangePct, r.VIXTermStructure.VIXChangePct = new(1.5), new(-10.0)
		}, LifecycleQuiet, "Normal regime", RegimeToneNormal},
		{"term inversion on its first day", func(r *RegimeSnapshotResult) {
			red(&r.VIXTermStructure.RegimeIndicatorMeta, false)
		}, LifecycleEarlyWarning, "Stress signal present", RegimeToneWatch},
		{"VVIX red that may not confirm yet", func(r *RegimeSnapshotResult) {
			red(&r.VolOfVol.RegimeIndicatorMeta, false)
		}, LifecycleEarlyWarning, "Stress signal present", RegimeToneWatch},
		{"yellow VVIX alone", func(r *RegimeSnapshotResult) {
			r.VolOfVol.Band = "yellow"
		}, LifecycleQuiet, "Normal regime", RegimeToneWatch},
		{"yellow VVIX on a day SPY gains 1%", func(r *RegimeSnapshotResult) {
			r.VolOfVol.Band = "yellow"
			r.HYGSPYDivergence.SPYChangePct = new(1.0)
		}, LifecycleStabilization, "Normal regime", RegimeToneWatch},
		{"cash credit 0.5 points wider", func(r *RegimeSnapshotResult) {
			r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
		}, LifecycleEarlyWarning, "Watch: one unconfirmed stress signal", RegimeToneWatch},
		{"cash credit at 4.5 without widening", func(r *RegimeSnapshotResult) {
			r.CreditSpreads.Band, r.CreditSpreads.HYOAS = "yellow", new(4.5)
		}, LifecycleQuiet, "Normal regime", RegimeToneWatch},
		{"2025-03-17: yellow term structure and proxy, cash credit widening", func(r *RegimeSnapshotResult) {
			r.VIXTermStructure.Band, r.HYGSPYDivergence.Band = "yellow", "yellow"
			r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.62)
		}, LifecycleEarlyWarning, "Watch: one unconfirmed stress signal", RegimeToneWatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			got := regimePosture(r)
			if got.Stage != tc.stage || got.Label != tc.label || got.Tone != tc.tone {
				t.Fatalf("stage %s, headline %q, tone %s; want %s, %q, %s", got.Stage, got.Label, got.Tone, tc.stage, tc.label, tc.tone)
			}
		})
	}
}

// Cash credit widening opens a warning as the credit witness, but only from
// a current cash row: an old series is a data gap, not a warning.
func TestRegimeCashCreditWideningOpensAWarning(t *testing.T) {
	r := calmRegime()
	r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
	if got := RegimeWarningOpeners(r); !slices.Equal(got, []string{"cash_widening"}) {
		t.Fatalf("openers = %v, want the cash widening", got)
	}

	r.CreditSpreads.Status = RegimeStatusStale
	r.CreditSpreads.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	if got := RegimeWarningOpeners(r); len(got) != 0 {
		t.Fatalf("openers from an overdue cash row = %v, want none", got)
	}
}

// A co-sign red is listed as suppressed context in every stage, so a reader
// can tell it from a witness that is waiting to confirm.
func TestRegimeCoSignRedIsSuppressedContextInEveryStage(t *testing.T) {
	r := calmRegime()
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.VIXTermStructure.RegimeIndicatorMeta, false)
	got := regimeVote(r)
	if got.Stage != LifecycleEarlyWarning || !slices.Equal(got.Unconfirmed, []string{"vol", "credit"}) || !slices.Equal(got.Suppressed, []string{"credit"}) {
		t.Fatalf("stage %s, unconfirmed %v, suppressed %v; want early_warning with both unconfirmed and only credit suppressed", got.Stage, got.Unconfirmed, got.Suppressed)
	}
}

// The lifecycle fingerprint is versioned by what its fields mean. v3: a
// co-sign red confirms only beside a standing stress witness, and suppressed
// names the co-sign reds that are context in every stage.
func TestLifecycleFingerprintVersion(t *testing.T) {
	if got := BuildLifecycleFingerprint(LifecycleState{}).Version; got != "lifecycle-fp-v3" {
		t.Fatalf("lifecycle fingerprint version = %q, want lifecycle-fp-v3", got)
	}
}

// The credit cluster can hold both roles at once: the proxy red, a co-sign,
// beside a cash row that is widening, a witness that warns without a red.
// The widening opens the warning; the proxy red stays context, so no surface
// names the proxy as the warning that is waiting to confirm (2026-10-14).
func TestRegimeProxyRedBesideCashWideningStaysContext(t *testing.T) {
	r := calmRegime()
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
	posture := regimePosture(r)
	got := r.Lifecycle
	if got.Stage != LifecycleEarlyWarning || posture.Label != "Watch: one unconfirmed stress signal" {
		t.Fatalf("stage %s, headline %q; want early_warning under the one-signal Watch headline", got.Stage, posture.Label)
	}
	if openers := RegimeWarningOpeners(r); !slices.Equal(openers, []string{"cash_widening"}) {
		t.Fatalf("openers = %v, want the cash widening alone", openers)
	}
	if want := []string{"credit", "breadth"}; !slices.Equal(got.Unconfirmed, want) || !slices.Equal(got.Suppressed, want) {
		t.Fatalf("unconfirmed %v, suppressed %v; want both co-sign reds in both lists", got.Unconfirmed, got.Suppressed)
	}
}

// Beside a standing witness a co-sign red that has not passed its own gate is
// waiting to confirm, as a provisional witness is: it is unconfirmed, and it
// is not context.
func TestRegimeCoSignRedBesideAStandingWitnessIsNotContext(t *testing.T) {
	r := calmRegime()
	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, false)
	got := regimeVote(r)
	if got.Stage != LifecycleEarlyWarning || !slices.Equal(got.Unconfirmed, []string{"credit"}) || len(got.Suppressed) != 0 {
		t.Fatalf("stage %s, unconfirmed %v, suppressed %v; want early_warning with credit unconfirmed and nothing suppressed", got.Stage, got.Unconfirmed, got.Suppressed)
	}
}
