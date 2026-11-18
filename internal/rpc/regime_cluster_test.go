package rpc

import "testing"

// The HYG proxy is a co-sign: alone, its red reads yellow in the cluster
// count whatever the cash row beside it shows short of red. A cash red is a
// witness red and keeps the cluster red.
func TestCreditProxyRedCountsOnlyBesideAWitness(t *testing.T) {
	credit := func(shape func(cs *RegimeCreditSpreads)) string {
		r := calmRegime()
		red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		shape(&r.CreditSpreads)
		return BuildRegimeClusterBands(r).Confirmed[RegimeClusterCredit]
	}
	for name, shape := range map[string]func(cs *RegimeCreditSpreads){
		"a fresh green cash read": func(*RegimeCreditSpreads) {},
		"no cash read":            func(cs *RegimeCreditSpreads) { *cs = RegimeCreditSpreads{} },
		"a stale cash read": func(cs *RegimeCreditSpreads) {
			cs.Status, cs.Freshness = RegimeStatusStale, &RegimeFreshness{Class: RegimeFreshnessStale}
		},
		"cash widening 0.6 points": func(cs *RegimeCreditSpreads) { cs.Band, cs.HY20DChange = "yellow", new(0.6) },
	} {
		if got := credit(shape); got != "yellow" {
			t.Errorf("proxy red beside %s: credit cluster = %q, want yellow", name, got)
		}
	}
	if got := credit(func(cs *RegimeCreditSpreads) { red(&cs.RegimeIndicatorMeta, true) }); got != "red" {
		t.Errorf("a red cash read is a witness: credit cluster = %q, want red", got)
	}
}

// Co-signs do not corroborate each other. On 2026-10-05 the HYG proxy, 2.6%
// below its 50DMA for 21 sessions, and a 19-session breadth red read
// confirmed stress together with VIX at 15.5 and HY OAS at 3.10%. Both count
// once a witness stands.
func TestCoSignRedsCountOnlyBesideAWitness(t *testing.T) {
	r := calmRegime()
	red(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	cb := BuildRegimeClusterBands(r)
	if cb.Confirmed[RegimeClusterCredit] != "yellow" || cb.Confirmed[RegimeClusterBreadth] != "yellow" || cb.EligibleRedCount() != 0 {
		t.Fatalf("credit %q, breadth %q, %d eligible reds; want two yellows and none", cb.Confirmed[RegimeClusterCredit], cb.Confirmed[RegimeClusterBreadth], cb.EligibleRedCount())
	}
	if stage := regimeVote(r).Stage; stage != LifecycleQuiet {
		t.Fatalf("two co-sign reds read %q, want quiet", stage)
	}

	red(&r.VIXTermStructure.RegimeIndicatorMeta, true)
	if got := BuildRegimeClusterBands(r).EligibleRedCount(); got != 3 {
		t.Fatalf("eligible reds = %d, want 3 once the term inversion stands", got)
	}
}

// A witness red that is not eligible does not stand, and a co-sign cannot
// rescue it: the co-sign would then be confirming the very witness it leans
// on. An eligible witness red stands with no further condition.
func TestWitnessRedThatMayNotConfirmIsNotRescuedByACoSign(t *testing.T) {
	r := calmRegime()
	red(&r.VolOfVol.RegimeIndicatorMeta, false)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	cb := BuildRegimeClusterBands(r)
	if cb.Confirmed[RegimeClusterEquityVol] != "red" || cb.Confirmed[RegimeClusterBreadth] != "yellow" || cb.EligibleRedCount() != 0 {
		t.Fatalf("vol %q, breadth %q, %d eligible reds; want a red that does not confirm, a yellow, and none",
			cb.Confirmed[RegimeClusterEquityVol], cb.Confirmed[RegimeClusterBreadth], cb.EligibleRedCount())
	}

	red(&r.FundingStress.RegimeIndicatorMeta, true)
	if got := BuildRegimeClusterBands(r).EligibleRedCount(); got != 2 {
		t.Fatalf("eligible reds = %d, want funding and breadth once funding stands", got)
	}

	red(&r.VolOfVol.RegimeIndicatorMeta, true)
	r.FundingStress.Band, r.FundingStress.Eligibility = "green", nil
	if cb := BuildRegimeClusterBands(r); cb.EligibleRedCount() != 2 {
		t.Fatalf("an eligible VVIX red with breadth red: %d eligible reds, want 2", cb.EligibleRedCount())
	}
}
