package rpc

import (
	"testing"
	"time"
)

// spyPrint sets the SPY day change and the print it rides on.
func spyPrint(r *RegimeSnapshotResult, pct float64, class string, at time.Time) {
	r.HYGSPYDivergence.SPYChangePct = new(pct)
	r.HYGSPYDivergence.SPYQuality = &Quality{AsOf: at, FreshnessClass: class}
}

// latchedTermRed is a term inversion that stood in its window and now
// stands on its valid typed not-due schedule, the evening after.
func latchedTermRed(r *RegimeSnapshotResult) {
	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	r.VIXTermStructure.Eligibility.Latched = true
	r.VIXTermStructure.Status = RegimeStatusStale
	r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
	r.VIXTermStructure.VIX3MCrossCheck = VIX3MCrossCheckAgree
	r.VIXTermStructure.VIXQuality = &Quality{AsOf: r.AsOf, FreshnessClass: FreshnessFrozen}
}

// Scheduled evidence is current evidence. On a crash-day evening the SPY
// print is frozen but dated today, the term inversion stands latched on its
// not-due schedule, and VVIX is an eligible red: that is panic at urgent,
// with or without an unrelated impaired input beside it.
func TestRegimeCrashDayEveningReadsItsOwnTape(t *testing.T) {
	for _, tc := range []struct {
		name     string
		impaired func(r *RegimeSnapshotResult)
	}{
		{"every other input current", func(*RegimeSnapshotResult) {}},
		{"breadth overdue", func(r *RegimeSnapshotResult) {
			r.Breadth.Status = RegimeStatusStale
			r.Breadth.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}},
		{"funding in error", func(r *RegimeSnapshotResult) { r.FundingStress = RegimeFundingStress{Status: RegimeStatusError} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			spyPrint(r, -7.5, FreshnessFrozen, r.AsOf)
			latchedTermRed(r)
			red(&r.VolOfVol.RegimeIndicatorMeta, true)
			r.VolOfVol.Last = new(150.0)
			r.HYGSPYDivergence.Band, r.Breadth.Band = "yellow", "yellow"
			tc.impaired(r)
			if got := regimeVote(r); got.Stage != LifecyclePanic || got.Severity != "urgent" {
				t.Fatalf("stage %s at %s (readiness %s, governors %+v); want panic at urgent", got.Stage, got.Severity, got.Readiness, got.Governors)
			}
		})
	}
}

// A confirmed stress on a latched term inversion holds through an unrelated
// error on any evening or weekend: the latched witness is scheduled
// evidence, so the stress stands; it never falls to an early warning.
func TestRegimeLatchedTermInversionHoldsBesideAnUnrelatedError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		error func(r *RegimeSnapshotResult)
	}{
		{"yen row in error", func(r *RegimeSnapshotResult) { r.USDJPY = RegimeUSDJPY{Status: RegimeStatusError} }},
		{"funding in error", func(r *RegimeSnapshotResult) { r.FundingStress = RegimeFundingStress{Status: RegimeStatusError} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			latchedTermRed(r)
			red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			tc.error(r)
			if got := regimeVote(r); got.Stage != LifecycleConfirmedStress {
				t.Fatalf("stage %s (readiness %s, governors %+v); want confirmed_stress", got.Stage, got.Readiness, got.Governors)
			}
		})
	}
}

// A prior session's SPY print is not today's tape: a -7.5% move dated
// yesterday does not pick panic.
func TestRegimePriorSessionSPYPrintIsNotTheTape(t *testing.T) {
	r := calmRegime()
	spyPrint(r, -7.5, FreshnessFrozen, r.AsOf.AddDate(0, 0, -1))
	if got := regimeVote(r); got.Stage == LifecyclePanic || got.Stage == LifecycleConfirmedStress {
		t.Fatalf("stage %s on yesterday's print; want no stress stage from the tape", got.Stage)
	}
}

// A panic on a current SPY print at or below -7% is exempt from the
// two-cluster blanking floor, as the severity governor already exempts it.
func TestRegimeCrashPanicIsExemptFromTheBlankFloor(t *testing.T) {
	r := calmRegime()
	spyPrint(r, -7.5, FreshnessLive, r.AsOf)
	r.CreditSpreads = RegimeCreditSpreads{Status: RegimeStatusUnavailable}
	r.Breadth.Status = RegimeStatusStale
	r.Breadth.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	if got := regimeVote(r); got.Stage != LifecyclePanic {
		t.Fatalf("stage %s (readiness %s); want panic", got.Stage, got.Readiness)
	}
}
