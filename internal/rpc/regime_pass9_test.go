package rpc

import "testing"

// threeWitnessesRed is VVIX 130, funding at 75 bp and HY OAS at 5.5, each an
// eligible red on its real, still pending_backtest threshold set, with SPY
// flat and no term inversion.
func threeWitnessesRed(r *RegimeSnapshotResult) {
	for _, row := range []struct {
		meta      *RegimeIndicatorMeta
		indicator string
	}{
		{&r.VolOfVol.RegimeIndicatorMeta, RegimeIndicatorVolOfVol},
		{&r.FundingStress.RegimeIndicatorMeta, RegimeIndicatorFunding},
		{&r.CreditSpreads.RegimeIndicatorMeta, RegimeIndicatorCredit},
	} {
		red(row.meta, true)
		row.meta.Thresholds = RegimeThresholdsFor(row.indicator)
	}
	r.VolOfVol.Last = new(130.0)
	r.FundingStress.SpreadBps = new(75.0)
	r.CreditSpreads.HYOAS = new(5.5)
}

// Owner ruling 2026-11-18 16:56 CET, "Yes, keep the page": three stress
// witness clusters eligible red and standing read confirmed stress at act on
// a flat market, exempt from the severity governor's watch cap, also on
// their valid schedules after the close. Two witness clusters with a co-sign
// keep today's rules, and a dark witness row does not stand.
func TestRegimeThreeStandingWitnessesReadAct(t *testing.T) {
	scheduled := func(r *RegimeSnapshotResult) {
		for _, meta := range []*RegimeIndicatorMeta{&r.VolOfVol.RegimeIndicatorMeta, &r.FundingStress.RegimeIndicatorMeta, &r.CreditSpreads.RegimeIndicatorMeta} {
			meta.Eligibility.Latched = true
			meta.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
		}
		r.VIXTermStructure.Status = RegimeStatusStale
		r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
		r.VIXTermStructure.VIX3MCrossCheck = VIX3MCrossCheckAgree
		r.VIXTermStructure.VIXQuality = &Quality{AsOf: r.AsOf, FreshnessClass: FreshnessFrozen}
	}
	for _, tc := range []struct {
		name     string
		shape    func(r *RegimeSnapshotResult)
		severity string
	}{
		{"three witnesses current", threeWitnessesRed, "act"},
		{"three witnesses after the close, on their schedules", func(r *RegimeSnapshotResult) {
			threeWitnessesRed(r)
			scheduled(r)
		}, "act"},
		{"three witnesses on a Saturday, on their schedules", func(r *RegimeSnapshotResult) {
			threeWitnessesRed(r)
			scheduled(r)
			r.TapeSessionState = TapeSessionClosedDate
		}, "act"},
		{"two witnesses and a co-sign", func(r *RegimeSnapshotResult) {
			threeWitnessesRed(r)
			r.CreditSpreads.Band, r.CreditSpreads.Eligibility, r.CreditSpreads.HYOAS = "green", nil, new(3.0)
			red(&r.Breadth.RegimeIndicatorMeta, true)
			r.Breadth.Thresholds = RegimeThresholdsFor(RegimeIndicatorBreadth)
		}, "watch"},
		{"one of the three witness rows dark", func(r *RegimeSnapshotResult) {
			threeWitnessesRed(r)
			r.FundingStress.Status = RegimeStatusStale
			r.FundingStress.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
		}, "watch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := calmRegime()
			tc.shape(r)
			got := regimeVote(r)
			if got.Stage != LifecycleConfirmedStress || got.Severity != tc.severity {
				t.Fatalf("stage %s at %s (governors %+v); want confirmed_stress at %s", got.Stage, got.Severity, got.Governors, tc.severity)
			}
		})
	}
}
