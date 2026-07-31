package rpc

import (
	"testing"
	"time"
)

// TestRegimeCurrencyConsumptionPolicy pins the one place authority is decided.
// Confirmation is an allowlist on fresh: every other class, including one the
// vocabulary does not know yet, must fail closed.
func TestRegimeCurrencyConsumptionPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		class       string
		confirm     bool
		context     bool
		scheduled   bool
		grade       string
		description string
	}{
		{class: RegimeFreshnessFresh, confirm: true, context: true, grade: RegimeCurrencyGradeNone},
		{class: RegimeFreshnessNotDue, context: true, scheduled: true, grade: RegimeCurrencyGradeNone},
		{class: RegimeFreshnessPending, context: true, scheduled: true, grade: RegimeCurrencyGradeNone},
		{class: RegimeFreshnessStale, context: true, grade: RegimeCurrencyGradeDegrade},
		{class: RegimeFreshnessOverdue, grade: RegimeCurrencyGradeFatal},
		{class: "", grade: RegimeCurrencyGradeFatal, description: "untyped"},
		{class: "someday_maybe", grade: RegimeCurrencyGradeFatal, description: "unknown"},
	} {
		name := tc.class
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := RegimeCurrencyMayConfirm(tc.class); got != tc.confirm {
				t.Fatalf("MayConfirm(%q) = %v, want %v", tc.class, got, tc.confirm)
			}
			if got := RegimeCurrencyMayContext(tc.class); got != tc.context {
				t.Fatalf("MayContext(%q) = %v, want %v", tc.class, got, tc.context)
			}
			if got := RegimeCurrencyScheduled(tc.class); got != tc.scheduled {
				t.Fatalf("Scheduled(%q) = %v, want %v", tc.class, got, tc.scheduled)
			}
			if got := RegimeCurrencyGrade(tc.class); got != tc.grade {
				t.Fatalf("Grade(%q) = %q, want %q", tc.class, got, tc.grade)
			}
		})
	}
}

// TestRegimeRowCurrencyReconcilesStatus pins that a row cannot claim more
// currency than its status supports, and that untyped freshness is overdue.
func TestRegimeRowCurrencyReconcilesStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		status    string
		freshness *RegimeFreshness
		want      string
	}{
		{name: "ok+fresh", status: RegimeStatusOK, freshness: &RegimeFreshness{Class: RegimeFreshnessFresh}, want: RegimeFreshnessFresh},
		{name: "stale row cannot be fresh", status: RegimeStatusStale, freshness: &RegimeFreshness{Class: RegimeFreshnessFresh}, want: RegimeFreshnessOverdue},
		{name: "stale row may be scheduled", status: RegimeStatusStale, freshness: &RegimeFreshness{Class: RegimeFreshnessNotDue}, want: RegimeFreshnessNotDue},
		{name: "stale row may be bounded stale", status: RegimeStatusStale, freshness: &RegimeFreshness{Class: RegimeFreshnessStale}, want: RegimeFreshnessStale},
		{name: "error row is overdue", status: RegimeStatusError, freshness: &RegimeFreshness{Class: RegimeFreshnessNotDue}, want: RegimeFreshnessOverdue},
		{name: "computing row is overdue", status: RegimeStatusComputing, freshness: &RegimeFreshness{Class: RegimeFreshnessPending}, want: RegimeFreshnessOverdue},
		{name: "no typed freshness", status: RegimeStatusOK, want: RegimeFreshnessOverdue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := RegimeRowCurrency(tc.status, tc.freshness); got != tc.want {
				t.Fatalf("RegimeRowCurrency(%q,%+v) = %q, want %q", tc.status, tc.freshness, got, tc.want)
			}
		})
	}
}

// TestRegimeScheduledCurrencyIsBoundedByMaxAge closes the off-hours hole: a
// dead subscription still serving its last value cannot hold a scheduled class
// open forever. The bound is the cluster's own served staleness policy.
func TestRegimeScheduledCurrencyIsBoundedByMaxAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 3, 0, 0, 0, time.UTC)
	build := func(observed time.Time) RegimeSnapshotResult {
		asOf := RegimeAsOfSummary{Time: observed}
		notDue := &RegimeFreshness{Class: RegimeFreshnessNotDue, MaxAgeSeconds: RegimeSourceMaxAgeSeconds("fx")}
		return RegimeSnapshotResult{
			AsOf: now,
			USDJPY: RegimeUSDJPY{
				Status:              RegimeStatusStale,
				RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "green", Freshness: notDue, AsOf: &asOf},
			},
		}
	}
	within := build(now.Add(-24 * time.Hour))
	if got := RegimeClusterCurrency(within, "fx"); got != RegimeFreshnessNotDue {
		t.Fatalf("fx currency inside the bound = %q, want not_due", got)
	}
	beyond := build(now.Add(-5 * 24 * time.Hour))
	if got := RegimeClusterCurrency(beyond, "fx"); got != RegimeFreshnessOverdue {
		t.Fatalf("fx currency past the served max age = %q, want overdue", got)
	}
	if _, scheduled := RegimeClusterScheduledContext(beyond, "fx"); scheduled {
		t.Fatal("a value past its served max age must not read as a scheduled publication gap")
	}
}

// TestRegimeVIXTapeArmsReadTheirOwnLeg is the symptom-4 regression: a VIX3M
// timeout carries the ratio leg but says nothing about the live VIX print, so
// the arms that read only the day-change stay current while the ratio co-sign,
// which genuinely depends on both legs, does not.
func TestRegimeVIXTapeArmsReadTheirOwnLeg(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	ratio := 1.02
	change := 12.5
	r := fullGreenLifecycleFixture()
	r.AsOf = now
	r.TapeSessionState = TapeSessionTradingDate
	r.VIXTermStructure.VIX = new(21.0)
	r.VIXTermStructure.VIX3M = new(20.6)
	r.VIXTermStructure.Ratio = &ratio
	r.VIXTermStructure.VIXChangePct = &change
	r.VIXTermStructure.VIXQuality = &Quality{AsOf: now, FreshnessClass: FreshnessLive, Confidence: ConfidenceFirm}

	if !regimeLifecycleVIXTapeCurrent(r) {
		t.Fatal("a live VIX print must be current for the day-change arms")
	}
	if !regimeLifecycleVIXTermCurrent(r) {
		t.Fatal("a fresh vol cluster must be current for the ratio co-sign")
	}

	// The thin index missed its poll: the ratio leg is carried and the row is
	// stale, while VIX keeps printing live.
	carried := r
	carried.VIXTermStructure.Status = RegimeStatusStale
	carried.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessStale}
	if !regimeLifecycleVIXTapeCurrent(carried) {
		t.Fatal("a missed VIX3M poll must not demote the live VIX day-change leg")
	}
	if regimeLifecycleVIXTermCurrent(carried) {
		t.Fatal("a carried VIX3M must not co-sign through the ratio arm")
	}
	if !regimeTapeCosign(&carried) {
		t.Fatal("VIX +12.5% on a live print must still co-sign")
	}

	// A dead VIX leg fails closed on both.
	dead := carried
	dead.VIXTermStructure.VIXQuality = &Quality{AsOf: now, FreshnessClass: FreshnessFrozen, Confidence: ConfidenceFirm}
	if regimeLifecycleVIXTapeCurrent(dead) {
		t.Fatal("a frozen VIX print is not a current day-change observation")
	}
	if regimeTapeCosign(&dead) {
		t.Fatal("no current tape leg must mean no co-signature")
	}
}

// TestRegimeOneDefectiveClusterKeepsTheRead is the symptom-1 regression: a
// single impaired cluster degrades and names itself instead of discarding the
// five healthy ones — including when those five are confirming stress, and
// including the quiet and early-warning stages the graded escape hatch never
// covered.
func TestRegimeOneDefectiveClusterKeepsTheRead(t *testing.T) {
	t.Parallel()
	breakFX := func(r *RegimeSnapshotResult) {
		r.USDJPY.Status = RegimeStatusError
		r.USDJPY.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	}
	t.Run("quiet", func(t *testing.T) {
		t.Parallel()
		r := fullGreenLifecycleFixture()
		breakFX(&r)
		got := BuildRegimeLifecycle(&r)
		if got.Stage != LifecycleQuiet || got.Readiness != "degraded" {
			t.Fatalf("state = %s/%s, want quiet/degraded", got.Stage, got.Readiness)
		}
		if !lifecycleGovernorNames(got, "readiness_degraded", "input_currency", "fx") {
			t.Fatalf("governors = %+v, want fx named", got.Governors)
		}
	})
	t.Run("early_warning", func(t *testing.T) {
		t.Parallel()
		r := fullGreenLifecycleFixture()
		r.CreditSpreads.RegimeIndicatorMeta = RegimeIndicatorMeta{
			Band:        "red",
			Freshness:   &RegimeFreshness{Class: RegimeFreshnessFresh},
			Eligibility: &RegimeEligibility{Reasons: []string{"streak_1_of_2"}},
		}
		breakFX(&r)
		got := BuildRegimeLifecycle(&r)
		if got.Stage != LifecycleEarlyWarning || got.Readiness != "degraded" {
			t.Fatalf("state = %s/%s, want early_warning/degraded", got.Stage, got.Readiness)
		}
	})
	t.Run("confirmed_stress", func(t *testing.T) {
		t.Parallel()
		r := fullGreenLifecycleFixture()
		r.CreditSpreads.RegimeIndicatorMeta = eligibleRedMeta()
		r.CreditSpreads.Freshness = &RegimeFreshness{Class: RegimeFreshnessFresh}
		r.FundingStress.RegimeIndicatorMeta = eligibleRedMeta()
		r.FundingStress.Freshness = &RegimeFreshness{Class: RegimeFreshnessFresh}
		breakFX(&r)
		got := BuildRegimeLifecycle(&r)
		if got.Stage != LifecycleConfirmedStress || got.Readiness != "degraded" {
			t.Fatalf("state = %s/%s, want confirmed_stress/degraded", got.Stage, got.Readiness)
		}
		if len(got.ConfirmedBy) != 2 {
			t.Fatalf("confirmed_by = %v, want both current confirming clusters", got.ConfirmedBy)
		}
	})
	t.Run("second defect blanks", func(t *testing.T) {
		t.Parallel()
		r := fullGreenLifecycleFixture()
		breakFX(&r)
		r.VolOfVol.Status = RegimeStatusError
		r.VolOfVol.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		got := BuildRegimeLifecycle(&r)
		if got.Stage != LifecycleDataQuality || got.Readiness != "blocked" {
			t.Fatalf("state = %s/%s, want data_quality/blocked", got.Stage, got.Readiness)
		}
	})
}

// TestRegimeSchedulingContextCannotConfirm pins the confirmation authority the
// model must not loosen: no scheduled or bounded class ever confirms, whatever
// the depth or streak behind it.
func TestRegimeSchedulingContextCannotConfirm(t *testing.T) {
	t.Parallel()
	for _, class := range []string{RegimeFreshnessNotDue, RegimeFreshnessPending, RegimeFreshnessStale, RegimeFreshnessOverdue, "", "someday_maybe"} {
		r := fullGreenLifecycleFixture()
		r.CreditSpreads.RegimeIndicatorMeta = RegimeIndicatorMeta{
			Band:      "red",
			Freshness: &RegimeFreshness{Class: class},
			Eligibility: EvaluateRegimeEligibility(RegimeEligibilityInput{
				Indicator: RegimeIndicatorCredit, Band: "red", StreakSessions: 9,
				Fresh: true, FreshnessClass: class,
			}),
		}
		r.FundingStress.RegimeIndicatorMeta = eligibleRedMeta()
		r.FundingStress.Freshness = &RegimeFreshness{Class: RegimeFreshnessFresh}
		got := BuildRegimeLifecycle(&r)
		if got.Stage == LifecycleConfirmedStress || got.Stage == LifecyclePanic {
			t.Fatalf("class %q confirmed stress: %+v", class, got)
		}
		for _, name := range got.ConfirmedBy {
			if name == "credit" {
				t.Fatalf("class %q reached confirmed_by", class)
			}
		}
	}
}
