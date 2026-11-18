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
		"a fresh green cash read":     func(*RegimeCreditSpreads) {},
		"no cash read":                func(cs *RegimeCreditSpreads) { *cs = RegimeCreditSpreads{} },
		"a cash read twelve days old": func(cs *RegimeCreditSpreads) { cs.AsOfDate = "2026-08-29" },
		"cash widening 0.6 points":    func(cs *RegimeCreditSpreads) { cs.Band, cs.HY20DChange = "yellow", new(0.6) },
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

// An isolated equity-vol red needs its own corroboration or another witness.
// A co-sign cannot rescue it: the co-sign would then be confirming the very
// witness it leans on.
func TestIsolatedEquityVolRedIsNotRescuedByACoSign(t *testing.T) {
	r := calmRegime()
	red(&r.VolOfVol.RegimeIndicatorMeta, true)
	r.VolOfVol.Last = new(112.0)
	red(&r.Breadth.RegimeIndicatorMeta, true)
	cb := BuildRegimeClusterBands(r)
	if cb.Confirmed[RegimeClusterEquityVol] != "yellow" || cb.Confirmed[RegimeClusterBreadth] != "yellow" {
		t.Fatalf("vol %q, breadth %q; want both yellow", cb.Confirmed[RegimeClusterEquityVol], cb.Confirmed[RegimeClusterBreadth])
	}

	red(&r.FundingStress.RegimeIndicatorMeta, true)
	if got := BuildRegimeClusterBands(r).EligibleRedCount(); got != 3 {
		t.Fatalf("eligible reds = %d, want 3 once funding stands beside the VVIX red", got)
	}
}
