package stress

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// stressAfterVote finishes the regime read the way the daemon does, so the
// served lifecycle, headline and posture are the vote's own, then computes
// the stress read over it.
func stressAfterVote(shape func(r *rpc.RegimeSnapshotResult)) (rpc.RegimeSnapshotResult, StressResult) {
	r := healthyStressRegime()
	r.AsOf = stressTestNow
	r.TapeSessionState = rpc.TapeSessionTradingDate
	r.HYGSPYDivergence.SPYChangePct = new(0.0)
	r.VIXTermStructure.VIXChangePct = new(0.0)
	shape(&r)
	r.SourceHealth = rpc.BuildRegimeSourceHealth(&r, r.AsOf)
	r.Composite = rpc.RegimeComposite{}
	rpc.ApplyRegimeClusterTallies(&r.Composite, rpc.BuildRegimeClusterBands(&r))
	r.Lifecycle = rpc.BuildRegimeLifecycle(&r)
	r.Composite.Verdict = rpc.RegimeHeadline(r.Composite, r.Lifecycle.Stage)
	r.Posture = rpc.BuildRegimePosture(&r)
	return r, ComputeStress(StressInput{Account: baseStressAccount(), Positions: freshStressPositions(), Regime: r, Now: stressTestNow})
}

// A dark stress witness beside weak co-signs is an early warning in the
// stress read too, worded with the regime's own cause: the reading that is
// unavailable and the co-signs that are weak (owner ruling 2026-11-18 14:47
// CET, "Early warning").
func TestStressReadsTheDarkWitnessWarning(t *testing.T) {
	t.Parallel()
	r, res := stressAfterVote(func(r *rpc.RegimeSnapshotResult) {
		r.VIXTermStructure.Status = rpc.RegimeStatusError
		r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
		r.HYGSPYDivergence.Band, r.Breadth.Band = "yellow", "yellow"
	})
	if r.Lifecycle.Stage != rpc.LifecycleEarlyWarning {
		t.Fatalf("regime stage %s (readiness %s), want early_warning", r.Lifecycle.Stage, r.Lifecycle.Readiness)
	}
	if !slices.Contains(res.Market.WarningOpeners, "witness_unavailable") || res.MarketConfirmation != stressMarketPartial {
		t.Fatalf("openers %v, market confirmation %q; want the unavailable witness opening a partial read", res.Market.WarningOpeners, res.MarketConfirmation)
	}
	const cause = "volatility term structure unavailable; ETF credit proxy and breadth weak"
	if want := "An early market warning is flashing (" + cause + ", not confirmed yet)"; !strings.Contains(res.Summary, want) {
		t.Fatalf("summary %q, want it to contain %q", res.Summary, want)
	}
	row := stressRowByTitle(res.Rows, "Market stress")
	if row == nil || !strings.Contains(row.Guidance, "An early warning is flashing ("+cause+", not confirmed)") {
		t.Fatalf("market row %+v, want the early warning named with its cause", row)
	}
}

// Three weak co-sign clusters beside a dark witness: the warning opener
// counts as a cluster arm, so the market row is a deteriorating tape, and
// the summary names every co-sign as weak.
func TestStressDarkWitnessBesideThreeWeakCoSigns(t *testing.T) {
	t.Parallel()
	_, res := stressAfterVote(func(r *rpc.RegimeSnapshotResult) {
		r.VIXTermStructure.Status = rpc.RegimeStatusError
		r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		stressRed(&r.Breadth.RegimeIndicatorMeta, true)
		stressRed(&r.USDJPY.RegimeIndicatorMeta, true)
	})
	const want = "(volatility term structure unavailable; ETF credit proxy, FX carry proxy, and breadth weak, not confirmed yet)"
	if stressRowByTitle(res.Rows, "Deteriorating tape") == nil || !strings.Contains(res.Summary, want) {
		t.Fatalf("summary %q, deteriorating tape row %v; want the row and the cause %s", res.Summary, stressRowByTitle(res.Rows, "Deteriorating tape") != nil, want)
	}
}

// The stress read and the vote agree on one input: with the proxy row
// unavailable, a cash red and a funding red are confirmed stress in both, at
// the same grade.
func TestStressAgreesWithTheVoteWhenTheCashRowsMateIsDark(t *testing.T) {
	t.Parallel()
	r, res := stressAfterVote(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.CreditSpreads.RegimeIndicatorMeta, true)
		r.CreditSpreads.HYOAS = new(5.8)
		stressRed(&r.FundingStress.RegimeIndicatorMeta, true)
		r.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusUnavailable}
	})
	if r.Lifecycle.Stage != rpc.LifecycleConfirmedStress {
		t.Fatalf("regime stage %s (readiness %s), want confirmed_stress", r.Lifecycle.Stage, r.Lifecycle.Readiness)
	}
	title := "Confirmed stress held at watch"
	if r.Lifecycle.Severity == "act" {
		title = "Confirmed market stress"
	}
	if stressRowByTitle(res.Rows, title) == nil {
		var titles []string
		for _, row := range res.Rows {
			titles = append(titles, row.Title)
		}
		t.Fatalf("regime confirmed_stress at %s; stress rows %v, want %q", r.Lifecycle.Severity, titles, title)
	}
}
