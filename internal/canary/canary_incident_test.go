package canary

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// TestCanaryIncident20260612Regression replays the 2026-06-12 false positive
// on the canary surface — the SPA panel and alert pipeline the user actually
// watches. Canary previously recomputed cluster confirmation from raw bands
// and emitted "Confirmed market stress" (severity act) for two marginal
// reds. With served eligibility both reds are provisional: canary must hold
// at watch, emit no confirmed-stress row or act-grade regime signal, and
// disclose the unconfirmed clusters.
func TestCanaryIncident20260612Regression(t *testing.T) {
	t.Parallel()
	spyChange := 0.3
	vixChange := -3.45
	r := healthyCanaryRegime()
	r.Composite = rpc.RegimeComposite{
		ClusterGreenCount: 4, ClusterYellowCount: 1, ClusterRedCount: 1,
		ClusterRankedCount: 6, ClusterProvisionalRedCount: 2,
	}
	// Served rows as the post-fix daemon emits them: HYG red provisional
	// (7 bps depth, day 1), gamma red provisional (prior-evening cache,
	// status stale), tape green.
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Reasons: []string{"depth_below_min", "streak_1_of_2"}}
	r.HYGSPYDivergence.SPYChangePct = &spyChange
	r.VIXTermStructure.VIXChangePct = &vixChange
	r.GammaZero.Band = "red"
	r.GammaZero.Status = rpc.RegimeStatusStale
	r.GammaZero.Eligibility = &rpc.RegimeEligibility{Reasons: []string{"data_overdue"}}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		GammaSign: "negative",
		Quality:   &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable},
	}

	res := ComputeStress(StressInput{
		Account: baseCanaryAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 12, 13, 30, 0, 0, time.UTC),
	})

	if res.Market.EligibleRedClusters != 0 {
		t.Fatalf("eligible red clusters = %d, want 0", res.Market.EligibleRedClusters)
	}
	for _, want := range []string{"credit", "gamma"} {
		if !slices.Contains(res.Market.UnconfirmedRedClusterNames, want) {
			t.Fatalf("unconfirmed = %v, want %s disclosed", res.Market.UnconfirmedRedClusterNames, want)
		}
	}
	if res.MarketConfirmation == canaryMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want not confirmed for provisional reds", res.MarketConfirmation)
	}
	for _, row := range res.Rows {
		if row.Title == "Confirmed market stress" {
			t.Fatalf("incident row regression: %+v", row)
		}
		if row.Severity == risk.SeverityAct && strings.Contains(strings.ToLower(row.Title), "market") {
			t.Fatalf("act-grade market row on provisional evidence: %+v", row)
		}
	}
	if _, ok := findSignal(res.Signals, risk.SignalRegimeStressConfirmed); ok {
		t.Fatalf("confirmed stress signal fired on provisional reds: %+v", res.Signals)
	}
}
