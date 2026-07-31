package stress

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// stressTestNow pins tests to a regular NY trading date (Mon 2026-06-01,
// 11:00 ET) so session-aware tape severity never depends on the wall-clock
// weekday the suite happens to run on.
var stressTestNow = time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC)

func TestComputeStressAmbiguityDoesNotLookSafe(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime: rpc.RegimeSnapshotResult{
			Composite: rpc.RegimeComposite{ClusterRankedCount: 0, ClusterUnrankedCount: 6},
			GammaZero: rpc.RegimeGammaZero{
				Status: rpc.RegimeStatusComputing,
			},
			Breadth: rpc.RegimeBreadth{
				Status: rpc.RegimeStatusComputing,
			},
		},
		Now: time.Date(2026, 5, 28, 21, 55, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch for ambiguous all-unranked market", res.Direction, res.Severity)
	}
	if res.Action != stressActionConfirmInputs || res.MarketConfirmation != stressMarketBlocked || res.InputHealth != stressInputDegraded {
		t.Fatalf("decision = action %s market %s input %s, want confirm_inputs/blocked/degraded", res.Action, res.MarketConfirmation, res.InputHealth)
	}
	if res.PlannerModeHint != risk.PlannerModeConfirmData || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want confirm_data/blocked", res.PlannerModeHint, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "Some market inputs are incomplete") {
		t.Fatalf("expected data-quality ambiguity row, rows: %+v", res.Rows)
	}
}

func TestComputeStressConfirmedStressWithIncompleteGammaBreadthStillDelevers(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.GrossPositionValue = 130_000
	delta := 95_000.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &delta,
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "SPY", MarketValueBase: 60_000, MarketValuePctNLV: new(60.0),
			}},
			GreeksCoverage: 2,
			GreeksTotal:    2,
		}},
		Regime: redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	if res.Action != stressActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "treat the stress readings as tentative") {
		t.Fatalf("expected ambiguity filter disclosure, rows: %+v", res.Rows)
	}
}

func TestComputeStressImmediateMarginDangerLiquidatesDespiteAmbiguousMarket(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0.07
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime: rpc.RegimeSnapshotResult{
			Composite: rpc.RegimeComposite{ClusterRankedCount: 1, ClusterUnrankedCount: 5},
			GammaZero: rpc.RegimeGammaZero{
				Status: rpc.RegimeStatusComputing,
			},
		},
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch because market inputs are blocked", res.Direction, res.Severity)
	}
	if res.Action != stressActionConfirmInputs {
		t.Fatalf("action = %s, want confirm_inputs for account-only margin danger in stress", res.Action)
	}
}

func TestComputeStressZeroExcessLiquidityLiquidatesWhenMarginContextPresent(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0
	acct.ExcessLiquidity = 0
	acct.MaintenanceMargin = 80_000
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    healthyStressRegime(),
	})
	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because account-only margin danger is outside stress scope", res.Direction, res.Severity)
	}
	if res.Action != stressActionStandDown {
		t.Fatalf("action = %s, want stand_down without market pressure", res.Action)
	}
	if !rowContainsEvidence(res.Rows, "Immediate margin safety", "cushion 0%") {
		t.Fatalf("expected zero-cushion evidence, rows: %+v", res.Rows)
	}
}

func TestComputeStressLookAheadMarginDangerLiquidates(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.LookAheadExcess = 8_000
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    healthyStressRegime(),
	})
	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because look-ahead margin alone is outside stress scope", res.Direction, res.Severity)
	}
	if !rowContainsEvidence(res.Rows, "Immediate margin safety", "look-ahead cushion 8%") {
		t.Fatalf("expected look-ahead cushion evidence, rows: %+v", res.Rows)
	}
}

func TestComputeStressEarlyStressRequiresSecondIndependentCluster(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 1, ClusterYellowCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VolOfVol.Band = "red"
	r.Breadth.Band = "yellow"
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for one red cluster plus yellow", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeStage {
		t.Fatalf("planner_mode_hint = %s, want stage", res.PlannerModeHint)
	}
	if !rowContains(res.Rows, "Early stress filtered", "second independent red cluster") {
		t.Fatalf("expected early-stress filter row, rows: %+v", res.Rows)
	}
}

func TestComputeStressLowExposureMarketWatchDoesNotClaimPortfolioExposed(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.GrossPositionValue = 0
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterYellowCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VolOfVol.Band = "red"
	r.Breadth.Band = "yellow"
	res := ComputeStress(StressInput{
		Account: acct,
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Action != stressActionWatch || res.PortfolioFit != stressPortfolioFitLow {
		t.Fatalf("decision = action %s portfolio_fit %s, want watch/low", res.Action, res.PortfolioFit)
	}
	if res.PortfolioAlertRelevant == nil || *res.PortfolioAlertRelevant {
		t.Fatalf("portfolio_alert_relevant = %v, want stamped false for a low-fit flat book", res.PortfolioAlertRelevant)
	}
	summary := strings.ToLower(res.Summary)
	if strings.Contains(summary, "portfolio is exposed") || strings.Contains(summary, "stage reductions") {
		t.Fatalf("summary should not claim low-exposure portfolio is exposed: %q", res.Summary)
	}
	if !strings.Contains(summary, "your exposure is low") {
		t.Fatalf("summary should explain low exposure watch posture, got %q", res.Summary)
	}
}

func TestComputeStressSingleGammaRedIsNotLifecycleConfirmation(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -0.1
	vixPct := 5.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.MarketConfirmation == stressMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want not confirmed for one red gamma cluster", res.MarketConfirmation)
	}
	if res.Market.RegimePosture.Label != "Stress signal present" || res.Market.RegimePosture.Tone != rpc.RegimeToneWatch {
		t.Fatalf("market posture = %+v, want Stress signal present/watch", res.Market.RegimePosture)
	}
	if !hasSignal(res.Signals, risk.SignalGammaRed) {
		t.Fatalf("missing gamma red watch signal, signals: %+v", res.Signals)
	}
	marketEvidence := stressMarketEvidence(res.Market)
	if strings.Contains(marketEvidence, "SPY") || strings.Contains(marketEvidence, "VIX") {
		t.Fatalf("market cluster evidence should not mix tape indicators: %q", marketEvidence)
	}
	if got := stressTapeEvidence(res.Market); !strings.Contains(got, "SPY") || !strings.Contains(got, "VIX") {
		t.Fatalf("tape evidence should remain separately available, got %q", got)
	}
}

func TestComputeStressContextOnlyGammaDoesNotConfirmStress(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	r.GammaZero.Status = rpc.RegimeStatusOK
	r.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:         rpc.GammaZeroScopeCombined,
			GammaSign:     "negative",
			GammaTotalAbs: 10_000_000_000,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityContextOnly,
				RankabilityReason: "freshness: market is closed; cached gamma is context only",
			},
		},
	}
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}

	res := ComputeStress(StressInput{
		Account:   baseStressAccount(),
		Positions: freshStressPositions(),
		Regime:    r,
		Now:       time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})

	if slices.Contains(res.Market.RedClusterNames, "gamma") {
		t.Fatalf("red clusters = %+v, want context-only gamma excluded", res.Market.RedClusterNames)
	}
	if slices.Contains(res.Market.DegradedClusters, "gamma") {
		t.Fatalf("degraded clusters = %+v, want context-only gamma excluded", res.Market.DegradedClusters)
	}
	if slices.Contains(res.Market.AmbiguousClusters, "gamma") {
		t.Fatalf("ambiguous clusters = %+v, want context-only gamma excluded", res.Market.AmbiguousClusters)
	}
	if hasSignal(res.Signals, risk.SignalGammaRed) {
		t.Fatalf("signals include gamma_red despite context-only gamma: %+v", res.Signals)
	}
	if res.InputHealth != stressInputOK {
		t.Fatalf("input_health = %s, want ok for context-only gamma", res.InputHealth)
	}
	for _, h := range res.SourceHealth {
		if h.Source == "regime" && h.Status != rpc.RegimeStatusOK {
			t.Fatalf("regime source health = %+v, want ok when only gamma is context-only", h)
		}
	}
	if hasSignal(res.Signals, risk.SignalRiskDataDegraded) {
		t.Fatalf("signals include data degraded despite context-only gamma: %+v", res.Signals)
	}
}

func TestComputeStressGammaRequiresExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		status         string
		envelopeStatus string
		quality        *rpc.GammaSignalQuality
	}{
		{name: "nil_quality", status: rpc.RegimeStatusOK, envelopeStatus: rpc.GammaZeroStatusReady},
		{name: "blocked_quality", status: rpc.RegimeStatusOK, envelopeStatus: rpc.GammaZeroStatusReady, quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "OI coverage blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := healthyStressRegime()
			r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
			r.GammaZero.Band = "red"
			r.GammaZero.Status = tc.status
			r.GammaZero.Envelope.Status = tc.envelopeStatus
			r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
				Quality: tc.quality,
			}

			res := ComputeStress(StressInput{Now: stressTestNow,
				Account: baseStressAccount(),
				Regime:  r,
			})

			if slices.Contains(res.Market.RedClusterNames, "gamma") {
				t.Fatalf("red clusters = %+v, want gamma excluded", res.Market.RedClusterNames)
			}
			if res.Market.RankedClusters != 5 || res.Market.UnrankedClusters != 1 {
				t.Fatalf("cluster coverage = ranked %d unranked %d, want 5/1", res.Market.RankedClusters, res.Market.UnrankedClusters)
			}
			if !slices.Contains(res.Market.AmbiguousClusters, "gamma") {
				t.Fatalf("ambiguous clusters = %+v, want gamma ambiguous", res.Market.AmbiguousClusters)
			}
			if !slices.Contains(res.Market.DegradedClusters, "gamma") {
				t.Fatalf("degraded clusters = %+v, want gamma degraded", res.Market.DegradedClusters)
			}
			if hasSignal(res.Signals, risk.SignalGammaRed) {
				t.Fatalf("signals include gamma_red despite non-rankable gamma: %+v", res.Signals)
			}
		})
	}
}

func TestComputeStressStandalonePremarketSPYDropWatches(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	spyPct := -2.7
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for unconfirmed premarket SPY drop", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeStage {
		t.Fatalf("planner_mode_hint = %s, want stage", res.PlannerModeHint)
	}
	if !rowContains(res.Rows, "Index tape shock", "second pass") {
		t.Fatalf("expected direct tape second-pass row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing selloff signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch || !containsString(sig.BlockedBy, "confirmation") {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want watch blocked by confirmation", sig.Severity, sig.BlockedBy)
	}
}

func TestComputeStressProvisionalRedDoesNotConfirmHardTapeStress(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.GrossPositionValue = 180_000
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -2.6
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: acct,
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "SPY", DollarDeltaBase: new(45_000.0),
			}},
		}},
		Regime: r,
		Now:    time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.MarketConfirmation == stressMarketConfirmed || res.Market.EligibleRedClusters != 0 {
		t.Fatalf("market = confirmation %s eligible %d, want provisional/non-confirmed", res.MarketConfirmation, res.Market.EligibleRedClusters)
	}
	gross, ok := findSignal(res.Signals, risk.SignalGrossExposureHigh)
	if !ok {
		t.Fatalf("missing gross exposure signal, signals: %+v", res.Signals)
	}
	if gross.Severity != risk.SeverityWatch || gross.Direction != risk.DirectionRebalance {
		t.Fatalf("gross exposure signal = %+v, want watch/rebalance for provisional red plus hard tape", gross)
	}
	concentration, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok {
		t.Fatalf("missing concentration signal, signals: %+v", res.Signals)
	}
	if concentration.Severity != risk.SeverityWatch || concentration.Direction != risk.DirectionRebalance {
		t.Fatalf("concentration signal = %+v, want watch/rebalance for provisional red plus hard tape", concentration)
	}
}

func TestComputeStressConfirmedSPYVIXShockDelevers(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	spyPct := -2.6
	vixPct := 12.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for confirmed SPY/VIX context without high portfolio fit", res.Direction, res.Severity)
	}
	if res.Action != stressActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
	}
	if !rowContains(res.Rows, "Index tape shock", "direct SPY stress is confirmed") {
		t.Fatalf("expected confirmed direct tape row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing selloff signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityAct || len(sig.BlockedBy) > 0 {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want act without blocked_by", sig.Severity, sig.BlockedBy)
	}
}

func TestComputeStressGrossDollarDeltaCatchesOffsettingOptionBook(t *testing.T) {
	t.Parallel()
	net := 0.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &net,
			ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", DollarDeltaBase: new(90_000.0)},
				{Underlying: "MSFT", DollarDeltaBase: new(-90_000.0)},
			},
		}},
		Regime: redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	if !rowContainsEvidence(res.Rows, "US equity/options exposure", "gross delta 180% NLV") {
		t.Fatalf("expected gross delta evidence, rows: %+v", res.Rows)
	}
}

func TestComputeStressExposureDeleverHasMatchingSignal(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.GrossPositionValue = 110_000
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch until degraded inputs are clean", res.Direction, res.Severity)
	}
	sig, ok := findSignal(res.Signals, risk.SignalGrossExposureHigh)
	if !ok {
		t.Fatalf("missing gross exposure signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityAct || sig.Threshold == nil || *sig.Threshold != stressPolicy.GrossExposureStressActPct {
		t.Fatalf("gross exposure signal = severity %q threshold %v, want act at stress threshold", sig.Severity, sig.Threshold)
	}
}

func TestComputeStressLargestDeltaConcentrationWatchesWithoutMarketStress(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{AsOf: stressTestNow, Portfolio: &rpc.PositionsPortfolio{
			ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", DollarDeltaBase: new(45_000.0)},
			},
		}},
		Regime: healthyStressRegime(),
	})
	if res.Direction != risk.DirectionRebalance || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want rebalance/watch on largest dollar-delta concentration", res.Direction, res.Severity)
	}
	if res.Action != stressActionRebalance || res.PortfolioFit != stressPortfolioFitHigh {
		t.Fatalf("decision = action %s fit %s, want rebalance/high", res.Action, res.PortfolioFit)
	}
	if res.PortfolioAlertRelevant == nil || !*res.PortfolioAlertRelevant {
		t.Fatalf("portfolio_alert_relevant = %v, want stamped true for a concentrated book", res.PortfolioAlertRelevant)
	}
	if res.PlannerModeHint != risk.PlannerModeRebalance || res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner = %s/%s, want rebalance/ready", res.PlannerModeHint, res.PlannerReadiness)
	}
	if !rowContainsEvidence(res.Rows, "Largest concentration", "AAPL delta 45% NLV") {
		t.Fatalf("expected largest-delta evidence, rows: %+v", res.Rows)
	}
	if !hasSignal(res.Signals, risk.SignalSingleNameDeltaHigh) {
		t.Fatalf("expected single-name delta signal, signals: %+v", res.Signals)
	}
	sig, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok || sig.Direction != risk.DirectionRebalance || sig.Posture != risk.PortfolioPostureRebalance {
		t.Fatalf("single-name delta signal = %+v, want rebalance direction/posture", sig)
	}
}

func TestComputeStressProvisionalRedsDoNotCreateShortConvexityActSignal(t *testing.T) {
	t.Parallel()
	gamma := -1.0
	r := healthyStressRegime()
	r.FundingStress.Band = "red"
	r.GammaZero.Band = "red"
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{Portfolio: &rpc.PositionsPortfolio{
			Gamma:          &gamma,
			GreeksCoverage: 1,
			GreeksTotal:    1,
		}},
		Regime: r,
		Now:    time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Market.EligibleRedClusters != 0 || res.Market.RedClusters < 2 {
		t.Fatalf("market reds = %d eligible = %d, want multiple provisional reds", res.Market.RedClusters, res.Market.EligibleRedClusters)
	}
	if sig, ok := findSignal(res.Signals, risk.SignalShortConvexityHigh); ok {
		t.Fatalf("short-convexity signal = %+v, want no act-grade convexity signal from provisional reds", sig)
	}
	if res.Severity != risk.SeverityWatch {
		t.Fatalf("severity = %s, want watch for provisional reds", res.Severity)
	}
}

func TestComputeStressHeldUnderlyingPnLShockRebalancesWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	dailyLoss := -2_500.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyStressRegime(),
	})
	if res.MarketConfirmation != stressMarketNone {
		t.Fatalf("market_confirmation = %s, want none; held-name stress must not confirm market tape", res.MarketConfirmation)
	}
	if res.Direction != risk.DirectionRebalance || res.Action != stressActionRebalance || res.PortfolioFit != stressPortfolioFitHigh {
		t.Fatalf("decision = %s/%s fit %s, want rebalance/rebalance/high", res.Direction, res.Action, res.PortfolioFit)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldUnderlyingPnLShock)
	if !ok {
		t.Fatalf("missing held P&L shock signal: %+v", res.Signals)
	}
	if sig.Subject != "XYZ" || sig.Direction != risk.DirectionRebalance || sig.Severity != risk.SeverityWatch {
		t.Fatalf("held P&L signal = %+v, want XYZ rebalance/watch", sig)
	}
	if len(res.Portfolio.HeldStress) != 1 || res.Portfolio.HeldStress[0].Underlying != "XYZ" {
		t.Fatalf("held_stress = %+v, want one XYZ row", res.Portfolio.HeldStress)
	}
	if !rowContainsEvidence(res.Rows, "Held-name stress", "XYZ daily P&L -2.5% NLV") {
		t.Fatalf("expected held-name evidence, rows: %+v", res.Rows)
	}
}

func TestComputeStressHeldOptionExpiryConcentrationRebalances(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 4, 14, 0, 0, 0, time.UTC)
	underlying := 50.0
	delta := 0.60
	gamma := -0.03
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now,
			Options: []rpc.PositionView{{
				Symbol:     "XYZ",
				SecType:    rpc.SecTypeOption,
				Quantity:   10,
				Multiplier: 100,
				Expiry:     now.Add(5 * 24 * time.Hour).Format("20060102"),
				Delta:      &delta,
				Gamma:      &gamma,
				Underlying: &underlying,
			}},
			Portfolio: &rpc.PositionsPortfolio{
				GreeksCoverage: 1,
				GreeksTotal:    1,
			},
		},
		Regime: healthyStressRegime(),
		Now:    now,
	})
	sig, ok := findSignal(res.Signals, risk.SignalHeldOptionExpiryConcentration)
	if !ok {
		t.Fatalf("missing held option expiry signal: %+v", res.Signals)
	}
	if sig.Subject != "XYZ" || sig.Direction != risk.DirectionRebalance || sig.Observed == nil || *sig.Observed != 30 {
		t.Fatalf("held option signal = %+v, want XYZ rebalance at 30%% NLV", sig)
	}
	if res.Action != stressActionRebalance || res.PortfolioFit != stressPortfolioFitHigh {
		t.Fatalf("decision = action %s fit %s, want rebalance/high", res.Action, res.PortfolioFit)
	}
	if got := res.Portfolio.HeldStress[0].NearExpiryMinDTE; got == nil || *got != 5 {
		t.Fatalf("near_expiry_min_dte = %v, want 5", got)
	}
	if !rowContainsEvidence(res.Rows, "Held-name stress", "XYZ near-expiry delta 30% NLV at 5 DTE") {
		t.Fatalf("expected held option evidence, rows: %+v", res.Rows)
	}
}

func TestComputeStressHeldLiquidityDegradedIsDataQualityOnly(t *testing.T) {
	t.Parallel()
	spread := 1.20
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Stocks: []rpc.PositionView{{
				Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 1_000, SpreadPct: &spread, QuoteQuality: "firm",
			}},
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0),
				}},
			},
		},
		Regime:       healthyStressRegime(),
		MarketEvents: healthyStressMarketEvents(stressTestNow, "XYZ"),
	})
	if res.Direction != "" || res.Action != stressActionStandDown {
		t.Fatalf("decision = %s/%s, want stand_down for held-liquidity-only evidence", res.Direction, res.Action)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldLiquidityDegraded)
	if !ok || sig.Direction != risk.DirectionDataQuality {
		t.Fatalf("held liquidity signal = %+v ok=%v, want data-quality signal", sig, ok)
	}
	if !rowContains(res.Rows, "Held-name stress", "Confirm held-name quotes") {
		t.Fatalf("expected held-name data-quality row, rows: %+v", res.Rows)
	}
}

func TestComputeStressSignalsExposureAndDecisionShape(t *testing.T) {
	t.Parallel()
	delta := 140_000.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{AsOf: stressTestNow, Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &delta,
			ExposureBase: []rpc.UnderlyingExposure{{
				Underlying: "LMN", MarketValueBase: 40_000, MarketValuePctNLV: new(40.0), DollarDeltaBase: new(140_000.0),
			}},
		}},
		Regime: healthyStressRegime(),
	})
	for _, want := range []risk.SignalID{
		risk.SignalNetDeltaHigh,
		risk.SignalSingleNameExposureHigh,
		risk.SignalSingleNameDeltaHigh,
	} {
		if !hasSignal(res.Signals, want) {
			t.Fatalf("missing signal %s in %+v", want, res.Signals)
		}
	}
	if res.Direction != risk.DirectionRebalance {
		t.Fatalf("direction = %q, want rebalance", res.Direction)
	}
	if res.Action != stressActionRebalance || res.PortfolioFit != stressPortfolioFitHigh || res.InputHealth != stressInputOK {
		t.Fatalf("decision = action %s fit %s input %s, want rebalance/high/ok", res.Action, res.PortfolioFit, res.InputHealth)
	}
	if res.PlannerReadiness != risk.PlannerReadinessReady {
		t.Fatalf("planner_readiness = %q, want ready", res.PlannerReadiness)
	}
	if res.PlannerModeHint != risk.PlannerModeRebalance {
		t.Fatalf("planner_mode_hint = %s, want rebalance", res.PlannerModeHint)
	}
}

func TestComputeStressFastCarryUnwindActs(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 1, ClusterYellowCount: 2, ClusterGreenCount: 3, ClusterRankedCount: 6}
	r.USDJPY.Band = "red"
	r.VIXTermStructure.Band = "yellow"
	r.Breadth.Band = "yellow"
	spyPct := -2.0
	vixPct := 16.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch context without vulnerable portfolio fit", res.Direction, res.Severity)
	}
	if res.MarketConfirmation != stressMarketConfirmed || res.PortfolioFit != stressPortfolioFitLow || res.Action != stressActionWatch {
		t.Fatalf("decision = market %s fit %s action %s, want confirmed/low/watch", res.MarketConfirmation, res.PortfolioFit, res.Action)
	}
	if !rowContains(res.Rows, "Fast carry unwind", "FX stress is confirmed") {
		t.Fatalf("expected fast-carry market row, rows: %+v", res.Rows)
	}
	if !hasSignal(res.Signals, risk.SignalFXCarryUnwind) {
		t.Fatalf("missing FX carry unwind signal, signals: %+v", res.Signals)
	}
}

func TestComputeStressConstructiveTapeShowsOpportunityPosture(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.HYGSPYDivergence.SPYChangePct = new(3.0)
	r.VIXTermStructure.VIXChangePct = new(-25.0)
	res := ComputeStress(StressInput{
		Account:   baseStressAccount(),
		Positions: freshStressPositions(),
		Regime:    r,
		Now:       time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionConstructive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want constructive/watch", res.Direction, res.Severity)
	}
	if res.Action != stressActionDeploy || res.MarketConfirmation != stressMarketConfirmed {
		t.Fatalf("decision = action %s market %s, want deploy/confirmed", res.Action, res.MarketConfirmation)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketRallyViolent)
	if !ok || sig.Posture != risk.PortfolioPostureOpportunity {
		t.Fatalf("market rally signal = %+v, want opportunity posture", sig)
	}
}

func TestComputeStressPositivePnLShockProtectsGains(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	daily := 12_000.0
	acct.DailyPnL = &daily
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    healthyStressRegime(),
	})
	sig, ok := findSignal(res.Signals, risk.SignalPortfolioPnLShock)
	if !ok {
		t.Fatalf("missing P&L shock signal, signals: %+v", res.Signals)
	}
	if sig.Direction != risk.DirectionDefensive || sig.Severity != risk.SeverityWatch {
		t.Fatalf("P&L signal = direction %q severity %q, want defensive/watch", sig.Direction, sig.Severity)
	}
	if sig.ConfidenceImpact == "" {
		t.Fatalf("P&L signal should explain why a gain is not directly deployable: %+v", sig)
	}
	if res.Direction != "" || res.Action != stressActionStandDown {
		t.Fatalf("decision = direction %q action %q, want no stress action for P&L-only evidence", res.Direction, res.Action)
	}
}

func TestComputeStressMissingDailyPnLIsDataQuality(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.DailyPnL = nil
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime:  healthyStressRegime(),
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch for missing daily P&L", res.Direction, res.Severity)
	}
	if !rowContains(res.Rows, "Portfolio P&L shock", "cannot confirm or reject") {
		t.Fatalf("expected P&L data-quality row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalRiskDataDegraded)
	if !ok || sig.Subject != "account.daily_pnl" {
		t.Fatalf("missing daily P&L data-quality signal, got %+v", res.Signals)
	}
	var accountHealth *rpc.SourceHealth
	for i := range res.SourceHealth {
		if res.SourceHealth[i].Source == "account" {
			accountHealth = &res.SourceHealth[i]
			break
		}
	}
	if accountHealth == nil || accountHealth.Status != "partial" || accountHealth.Confidence != "medium-low" || accountHealth.RefreshState != "" {
		t.Fatalf("account source health = %+v, want partial/medium-low", accountHealth)
	}
}

func TestComputeStressMissingDailyPnLPostCloseIsExpected(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 20, 5, 0, 0, time.UTC) // Monday 16:05 ET.
	acct := baseStressAccount()
	acct.AsOf = now
	acct.DailyPnL = nil
	acct.DailyPnLObservation = &rpc.DailyPnLObservation{
		Status: rpc.DailyPnLObservationNotDue, SessionKey: "2026-06-01", AsOf: now,
	}
	res := ComputeStress(StressInput{
		Now:       now,
		Account:   acct,
		Positions: rpc.PositionsResult{AsOf: now},
		Regime:    healthyStressRegime(),
	})
	if res.InputHealth != stressInputOK || res.Direction == risk.DirectionDataQuality {
		t.Fatalf("decision = input %q direction %q, want healthy core account after close", res.InputHealth, res.Direction)
	}
	health := findSourceHealth(res.SourceHealth, "account")
	if health == nil || health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue || health.Confidence != "high" {
		t.Fatalf("account source health = %+v, want ok/not_due/high", health)
	}
	if len(health.Notes) != 1 || !strings.Contains(health.Notes[0], "not due") {
		t.Fatalf("account source health note = %+v, want expected off-session explanation", health.Notes)
	}
}

func TestComputeStressDailyPnLFailureRemainsDegradedPostClose(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 1, 20, 5, 0, 0, time.UTC) // Monday 16:05 ET.
	acct := baseStressAccount()
	acct.AsOf = now
	acct.DailyPnL = nil
	acct.DailyPnLObservation = &rpc.DailyPnLObservation{
		Status: rpc.DailyPnLObservationMissing, SessionKey: "2026-06-01", AsOf: now.Add(-10 * time.Minute),
	}
	res := ComputeStress(StressInput{
		Now: now, Account: acct, Positions: rpc.PositionsResult{AsOf: now}, Regime: healthyStressRegime(),
	})
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input health = %q, want degraded", res.InputHealth)
	}
	health := findSourceHealth(res.SourceHealth, "account")
	if health == nil || health.Status != rpc.SourceStatusDegraded || health.RefreshState == rpc.SourceRefreshNotDue {
		t.Fatalf("account source health = %+v, want degraded and still due", health)
	}
}

func TestComputeStressWatchMarginSignalDoesNotPublishLowerTarget(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0.30
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime:  healthyStressRegime(),
	})
	sig, ok := findSignal(res.Signals, risk.SignalMarginCushionLow)
	if !ok {
		t.Fatalf("missing margin signal, signals: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch {
		t.Fatalf("margin signal severity = %q, want watch", sig.Severity)
	}
	if sig.Target != nil {
		t.Fatalf("watch margin signal target = %v, want nil because target would be below watch threshold", *sig.Target)
	}
}

func TestComputeStressStandaloneVIXSpikeWatches(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	spyPct := -0.2
	vixPct := 18.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch for standalone VIX spike", res.Direction, res.Severity)
	}
	if !rowContains(res.Rows, "Index tape shock", "flashing early stress") {
		t.Fatalf("expected early VIX tape row, rows: %+v", res.Rows)
	}
}

func TestComputeStressSurfacesProtectionCoverage(t *testing.T) {
	t.Parallel()
	unprotected := 12_000.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			ProtectionCoverage: &rpc.ProtectionCoverageSummary{
				Status:                          "review",
				UnprotectedNotionalBase:         &unprotected,
				UnprotectedNotionalBaseCurrency: "USD",
				Counts:                          rpc.ProtectionCoverageCounts{Unprotected: 1},
				LargestUnprotected:              []rpc.ProtectionCoverageRow{{Underlying: "MSFT", UnprotectedNotionalBase: &unprotected, UnprotectedNotionalBaseCurrency: "USD"}},
			},
		},
		Regime: healthyStressRegime(),
	})
	if res.Portfolio.ProtectionCoverage == nil {
		t.Fatal("portfolio protection coverage missing")
	}
	// The row copy itself names the largest unprotected position and amount
	// (mirrors the Monitor protection panel's "largest unprotected SYM
	// amount" wording); the reader decides from the row, not a disclosure.
	if !rowContains(res.Rows, "Protection coverage", "largest unprotected MSFT $ 12,000.00") {
		t.Fatalf("coverage guidance should name largest unprotected position and amount, rows: %+v", res.Rows)
	}
	if !rowContainsEvidence(res.Rows, "Protection coverage", "unprotected $ 12,000.00") {
		t.Fatalf("coverage evidence missing, rows: %+v", res.Rows)
	}

	// A largest-unprotected row without a valued notional still contributes
	// its name — an unvalued exposure must not fall out of the copy.
	unvalued := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			ProtectionCoverage: &rpc.ProtectionCoverageSummary{
				Status:             "review",
				Counts:             rpc.ProtectionCoverageCounts{Unprotected: 1},
				LargestUnprotected: []rpc.ProtectionCoverageRow{{Underlying: "MSFT"}},
			},
		},
		Regime: healthyStressRegime(),
	})
	if !rowContains(unvalued.Rows, "Protection coverage", "largest unprotected MSFT.") {
		t.Fatalf("coverage guidance should name largest unprotected position without amount, rows: %+v", unvalued.Rows)
	}
}

func TestComputeStressStaleGreenClusterStillWatches(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch when ranked market data is stale", res.Direction, res.Severity)
	}
	if res.PlannerModeHint != risk.PlannerModeConfirmData || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("planner = %s/%s, want confirm_data/blocked", res.PlannerModeHint, res.PlannerReadiness)
	}
	if got := strings.Join(res.Market.StaleClusters, ","); got != "vol" {
		t.Fatalf("stale clusters = %q, want vol", got)
	}
	if !rowContains(res.Rows, "Ambiguity filter", "Some market inputs are incomplete") {
		t.Fatalf("expected stale-data ambiguity row, rows: %+v", res.Rows)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "stale clusters: vol") {
		t.Fatalf("expected stale-cluster warning, warnings: %+v", res.Warnings)
	}
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input_health = %q, want degraded for stale data", res.InputHealth)
	}
}

func TestComputeStressOffHoursVolStaleIsContext(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
	r.VIXTermStructure.AsOf = &rpc.RegimeAsOfSummary{Label: "frozen", Date: "2026-06-01"}
	r.DataQuality = []rpc.DataQualityHealth{{
		Surface:       "regime",
		Status:        rpc.RegimeStatusStale,
		Summary:       "stale: vol",
		StaleClusters: []string{"vol"},
	}}
	r.WarningDetails = []rpc.RegimeWarning{{
		Code:    "vix_term_structure_stale",
		Scope:   "vix_term_structure",
		Message: "volatility term structure stale",
	}}
	res := ComputeStress(StressInput{
		Account:   baseStressAccount(),
		Positions: freshStressPositions(),
		Regime:    r,
		Now:       time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	})
	if got := strings.Join(res.Market.StaleClusters, ","); got != "" {
		t.Fatalf("stale clusters = %q, want none for expected off-hours vol context", got)
	}
	if res.InputHealth != stressInputOK {
		t.Fatalf("input_health = %q, want ok for expected off-hours vol context", res.InputHealth)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings = %+v, want none for expected off-hours vol context", res.Warnings)
	}
	vix, ok := findStressIndicator(res.MarketIndicators, "VIX/VIX3M")
	if !ok {
		t.Fatalf("missing VIX indicator: %+v", res.MarketIndicators)
	}
	if strings.Contains(strings.ToLower(vix.Comment), "stale input") || !strings.Contains(vix.Comment, "closed-session cached context") {
		t.Fatalf("VIX comment = %q, want closed-session context without stale-input action", vix.Comment)
	}
}

func TestComputeStressOffHoursVolOverdueIsDataQuality(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
	r.DataQuality = []rpc.DataQualityHealth{{
		Surface: "regime", Status: rpc.RegimeStatusStale, StaleClusters: []string{"vol"},
	}}
	res := ComputeStress(StressInput{
		Account: baseStressAccount(), Positions: freshStressPositions(), Regime: r,
		Now: time.Date(2026, 6, 1, 11, 5, 0, 0, time.UTC),
	})
	if !slices.Contains(res.Market.StaleClusters, "vol") || res.InputHealth != stressInputDegraded || res.Action != stressActionConfirmInputs {
		t.Fatalf("off-hours overdue vol = stale %v input %q action %q, want explicit data-quality block", res.Market.StaleClusters, res.InputHealth, res.Action)
	}
}

func TestComputeStressGreenClustersAreNotAmbiguous(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.Breadth.Band = "unranked"
	r.Breadth.Status = rpc.RegimeStatusComputing
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "breadth" {
		t.Fatalf("ambiguous clusters = %q, want breadth only", got)
	}
}

func TestComputeStressCarriesSourceTimestamps(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.AsOf = time.Date(2026, 5, 29, 13, 1, 0, 0, time.UTC)
	posAsOf := time.Date(2026, 5, 29, 13, 2, 0, 0, time.UTC)
	regime := healthyStressRegime()
	regime.AsOf = time.Date(2026, 5, 29, 13, 3, 0, 0, time.UTC)
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: rpc.PositionsResult{AsOf: posAsOf},
		Regime:    regime,
	})
	if !res.SourceAsOf.Account.Equal(acct.AsOf) || !res.SourceAsOf.Positions.Equal(posAsOf) || !res.SourceAsOf.Regime.Equal(regime.AsOf) {
		t.Fatalf("source_as_of = %+v, want account=%s positions=%s regime=%s", res.SourceAsOf, acct.AsOf, posAsOf, regime.AsOf)
	}
}

func TestComputeStressCarriesSemanticFingerprints(t *testing.T) {
	t.Parallel()
	regime := healthyStressRegime()
	regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	acct := baseStressAccount()
	acct.Cushion = 0.30
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime:  regime,
	})
	if res.Fingerprint.Version != rpc.StressFingerprintVersion || res.Fingerprint.Key == "" {
		t.Fatalf("stress fingerprint = %+v, want populated %s", res.Fingerprint, rpc.StressFingerprintVersion)
	}
	if res.SourceFingerprints.Regime == nil || *res.SourceFingerprints.Regime != regime.Fingerprint {
		t.Fatalf("source regime fingerprint = %+v, want %+v", res.SourceFingerprints.Regime, regime.Fingerprint)
	}
}

func TestComputeStressJSONCarriesMonitorFields(t *testing.T) {
	t.Parallel()
	regime := healthyStressRegime()
	regime.Fingerprint = rpc.BuildRegimeFingerprint(&regime)
	acct := baseStressAccount()
	acct.Cushion = 0.30
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    regime,
	})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, key := range []string{"fingerprint", "source_fingerprints", "source_health", "action", "market_confirmation", "portfolio_fit", "input_health", "severity", "planner_mode_hint", "planner_readiness", "signals", "rows"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("stress JSON missing %s: %s", key, b)
		}
	}
	if _, ok := wire["market_indicators"]; !ok {
		t.Fatalf("stress JSON missing market_indicators: %s", b)
	}
	if wire["action"] != stressActionStandDown {
		t.Fatalf("action = %#v, want stand_down", wire["action"])
	}
	signals, ok := wire["signals"].([]any)
	if !ok || len(signals) == 0 {
		t.Fatalf("signals missing/malformed: %#v", wire["signals"])
	}
	firstSignal, ok := signals[0].(map[string]any)
	if !ok || firstSignal["posture"] == "" {
		t.Fatalf("signal posture missing: %#v", wire["signals"])
	}
	fp, ok := wire["fingerprint"].(map[string]any)
	if !ok || fp["version"] != rpc.StressFingerprintVersion || fp["key"] == "" {
		t.Fatalf("fingerprint missing/malformed: %#v", wire["fingerprint"])
	}
	sources, ok := wire["source_fingerprints"].(map[string]any)
	if !ok {
		t.Fatalf("source_fingerprints missing/malformed: %#v", wire["source_fingerprints"])
	}
	for _, key := range []string{"account", "positions", "regime"} {
		fp, ok := sources[key].(map[string]any)
		if !ok || fp["key"] == "" {
			t.Fatalf("source_fingerprints.%s missing/malformed: %#v", key, sources[key])
		}
	}
	regimeFP, ok := sources["regime"].(map[string]any)
	if !ok || regimeFP["version"] != rpc.RegimeFingerprintVersion || regimeFP["key"] != regime.Fingerprint.Key {
		t.Fatalf("source_fingerprints.regime = %#v, want %+v", sources["regime"], regime.Fingerprint)
	}
	sourceHealth, ok := wire["source_health"].([]any)
	if !ok || len(sourceHealth) != 3 {
		t.Fatalf("source_health missing/malformed: %#v", wire["source_health"])
	}
	indicators, ok := wire["market_indicators"].([]any)
	if !ok || len(indicators) == 0 {
		t.Fatalf("market_indicators missing/malformed: %#v", wire["market_indicators"])
	}
	firstIndicator, ok := indicators[0].(map[string]any)
	if !ok || firstIndicator["name"] == "" || firstIndicator["status"] == "" {
		t.Fatalf("market indicator missing required scan fields: %#v", indicators[0])
	}
}

func TestComputeStressJSONCarriesHeldStress(t *testing.T) {
	t.Parallel()
	dailyLoss := -2_500.0
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: time.Now(),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyStressRegime(),
	})
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	portfolio, ok := wire["portfolio"].(map[string]any)
	if !ok {
		t.Fatalf("portfolio missing/malformed: %s", b)
	}
	held, ok := portfolio["held_stress"].([]any)
	if !ok || len(held) != 1 {
		t.Fatalf("held_stress missing/malformed: %#v", portfolio["held_stress"])
	}
	first, ok := held[0].(map[string]any)
	if !ok || first["underlying"] != "XYZ" || first["daily_pnl_pct_nlv"] == nil || first["signal_ids"] == nil {
		t.Fatalf("held_stress[0] = %#v, want XYZ stress with daily P&L and signal IDs", held[0])
	}
}

func TestComputeStressFingerprintIgnoresTimestampsAndRawValuesInsideBucket(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0.30
	regime := healthyStressRegime()
	first := ComputeStress(StressInput{
		Account: acct,
		Regime:  regime,
		Now:     time.Date(2026, 5, 31, 8, 30, 0, 0, time.UTC),
	})

	acct.AsOf = time.Date(2026, 5, 31, 8, 35, 0, 0, time.UTC)
	acct.Cushion = 0.29
	regime.AsOf = time.Date(2026, 5, 31, 8, 36, 0, 0, time.UTC)
	second := ComputeStress(StressInput{
		Account: acct,
		Regime:  regime,
		Now:     time.Date(2026, 5, 31, 8, 37, 0, 0, time.UTC),
	})
	if first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint changed inside same margin bucket: %v != %v", first.Fingerprint, second.Fingerprint)
	}

	acct.Cushion = 0.19
	third := ComputeStress(StressInput{Now: stressTestNow, Account: acct, Regime: regime})
	if first.Fingerprint == third.Fingerprint {
		t.Fatal("fingerprint did not change after crossing margin severity bucket")
	}
}

func TestComputeStressFingerprintIncludesSourceRegimeFingerprint(t *testing.T) {
	t.Parallel()
	regime := healthyStressRegime()
	regime.Fingerprint = rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:a"}
	first := ComputeStress(StressInput{Now: stressTestNow, Account: baseStressAccount(), Regime: regime})

	regime.Fingerprint = rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:b"}
	second := ComputeStress(StressInput{Now: stressTestNow, Account: baseStressAccount(), Regime: regime})
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("stress fingerprint did not change when source regime fingerprint changed")
	}
}

func TestComputeStressFingerprintIncludesSourceMarketEventsFingerprint(t *testing.T) {
	t.Parallel()
	regime := healthyStressRegime()
	positions := rpc.PositionsResult{AsOf: stressTestNow, Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: -1}}}
	first := ComputeStress(StressInput{Now: stressTestNow,
		Account:   baseStressAccount(),
		Positions: positions,
		Regime:    regime,
		MarketEvents: rpc.MarketEventsResult{
			Kind:        rpc.MarketEventsKind,
			Fingerprint: rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-a"},
		},
	})
	second := ComputeStress(StressInput{Now: stressTestNow,
		Account:   baseStressAccount(),
		Positions: positions,
		Regime:    regime,
		MarketEvents: rpc.MarketEventsResult{
			Kind:        rpc.MarketEventsKind,
			Fingerprint: rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-b"},
		},
	})
	if first.SourceFingerprints.MarketEvents == nil || first.SourceFingerprints.MarketEvents.Key != "sha256:market-a" {
		t.Fatalf("source market-events fingerprint missing: %+v", first.SourceFingerprints.MarketEvents)
	}
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("stress fingerprint did not change when source market-events fingerprint changed")
	}
}

func TestComputeStressFingerprintIgnoresBorrowHealthWithoutShortStock(t *testing.T) {
	t.Parallel()
	positions := rpc.PositionsResult{
		AsOf:   stressTestNow,
		Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
	}
	baseEvents := healthyStressMarketEvents(stressTestNow, "XYZ")
	baseEvents.Fingerprint = rpc.BuildMarketEventsFingerprint(&baseEvents)
	failedEvents := baseEvents
	failedEvents.SourceHealth = slices.Clone(baseEvents.SourceHealth)
	for i := range failedEvents.SourceHealth {
		if failedEvents.SourceHealth[i].Source == "borrow_fee" {
			failedEvents.SourceHealth[i].Status = rpc.SourceStatusUnknown
			failedEvents.SourceHealth[i].LastFailure = &rpc.SourceFailure{
				Code: "timeout", Stage: "ftp_control_connect", FailedAt: stressTestNow, Retryable: true,
			}
		}
	}
	failedEvents.Fingerprint = rpc.BuildMarketEventsFingerprint(&failedEvents)
	input := StressInput{Now: stressTestNow, Account: baseStressAccount(), Positions: positions, Regime: healthyStressRegime(), MarketEvents: baseEvents}
	baseline := ComputeStress(input)
	input.MarketEvents = failedEvents
	changed := ComputeStress(input)
	if baseline.Fingerprint != changed.Fingerprint {
		t.Fatalf("all-long borrow failure changed Stress fingerprint: %s -> %s", baseline.Fingerprint.Key, changed.Fingerprint.Key)
	}
	if baseline.EstablishedAlertProjection == nil || changed.EstablishedAlertProjection == nil {
		t.Fatal("missing established alert projection")
	}
	if baseline.EstablishedAlertProjection.CanonicalFingerprint == changed.EstablishedAlertProjection.CanonicalFingerprint {
		t.Fatal("frozen v1 compatibility identity unexpectedly ignored its established source-status bucket")
	}
	nonBorrowFailure := baseEvents
	nonBorrowFailure.SourceHealth = slices.Clone(baseEvents.SourceHealth)
	for i := range nonBorrowFailure.SourceHealth {
		if nonBorrowFailure.SourceHealth[i].Source == "trading_halts" {
			nonBorrowFailure.SourceHealth[i].Status = rpc.SourceStatusUnknown
		}
	}
	nonBorrowFailure.Fingerprint = rpc.BuildMarketEventsFingerprint(&nonBorrowFailure)
	input.MarketEvents = nonBorrowFailure
	if got := ComputeStress(input); got.Fingerprint == baseline.Fingerprint {
		t.Fatal("all-long relevance filter suppressed non-borrow source-health identity")
	}

	input.Positions.Stocks[0].Quantity = -100
	input.MarketEvents = baseEvents
	shortHealthy := ComputeStress(input)
	input.MarketEvents = failedEvents
	shortFailed := ComputeStress(input)
	if shortHealthy.Fingerprint == shortFailed.Fingerprint {
		t.Fatal("short-book borrow failure did not change Stress fingerprint")
	}
}

func TestComputeStressEstablishedFingerprintIgnoresTypedFailureDetails(t *testing.T) {
	t.Parallel()
	positions := rpc.PositionsResult{
		AsOf:   stressTestNow,
		Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: -100}},
	}
	baseEvents := healthyStressMarketEvents(stressTestNow, "XYZ")
	withFailure := func(code, stage string) rpc.MarketEventsResult {
		events := baseEvents
		events.SourceHealth = slices.Clone(baseEvents.SourceHealth)
		for i := range events.SourceHealth {
			if events.SourceHealth[i].Source == "borrow_fee" {
				events.SourceHealth[i].Status = rpc.SourceStatusUnknown
				events.SourceHealth[i].LastFailure = &rpc.SourceFailure{
					Code: code, Stage: stage, FailedAt: stressTestNow, Retryable: true,
				}
			}
		}
		events.Fingerprint = rpc.BuildMarketEventsFingerprint(&events)
		return events
	}
	input := StressInput{
		Now: stressTestNow, Account: baseStressAccount(), Positions: positions,
		Regime: healthyStressRegime(), MarketEvents: withFailure(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect),
	}
	first := ComputeStress(input)
	input.MarketEvents = withFailure(rpc.SourceFailureConnectionRefused, rpc.SourceFailureStageFTPPassiveConnect)
	second := ComputeStress(input)
	if first.Fingerprint == second.Fingerprint {
		t.Fatal("current Stress v2 identity ignored changed typed failure details")
	}
	if first.EstablishedAlertProjection == nil || second.EstablishedAlertProjection == nil ||
		*first.EstablishedAlertProjection != *second.EstablishedAlertProjection {
		t.Fatalf("typed failure details changed established v1 identity: before=%+v after=%+v", first.EstablishedAlertProjection, second.EstablishedAlertProjection)
	}
}

func TestComputeStressSurfacesDegradedGammaSeparately(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "oi_observed_coverage: OI coverage below threshold",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "gamma" {
		t.Fatalf("ambiguous clusters = %q, want gamma for blocked gamma", got)
	}
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "gamma" {
		t.Fatalf("degraded clusters = %q, want gamma", got)
	}
	if !rowContainsEvidence(res.Rows, "Ambiguity filter", "ambiguous gamma; degraded gamma") {
		t.Fatalf("expected degraded-gamma disclosure row, rows: %+v", res.Rows)
	}
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input_health = %q, want degraded for degraded gamma", res.InputHealth)
	}
}

func TestComputeStressDoesNotDegradeRankableWarningOnlyGamma(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterEligibleRedCount: 2, ClusterGreenCount: 4, ClusterRankedCount: 6}
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Band = "red"
	r.GammaZero.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality:        rankableStressGammaQuality(),
		WarningDetails: []rpc.GammaWarningDetail{{Code: "oi_missing", Severity: "data_quality"}},
	}
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for rankable warning-only gamma", got)
	}
	sig, ok := findSignal(res.Signals, risk.SignalRegimeStressConfirmed)
	if !ok {
		t.Fatalf("missing regime stress signal: %+v", res.Signals)
	}
	if containsString(sig.BlockedBy, "gamma") {
		t.Fatalf("regime stress signal blocked_by = %+v, want gamma usable", sig.BlockedBy)
	}
	if res.MarketConfirmation != stressMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want confirmed for rankable gamma", res.MarketConfirmation)
	}
}

func TestComputeStressMarketIndicatorsCarryDecisionContext(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.VIXTermStructure.AsOf = &rpc.RegimeAsOfSummary{Date: "2026-05-29"}
	r.VIXTermStructure.Ratio = new(0.91)
	r.VIXTermStructure.VIX = new(18.0)
	r.VIXTermStructure.VIX3M = new(19.8)
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Scope:           rpc.GammaZeroScopeCombined,
		SpotUnderlying:  500.0,
		GammaSign:       "negative",
		GammaTotalAbs:   10_000_000_000,
		ZeroGamma:       nil,
		RegimeAgreement: "agree:short-gamma",
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: market is closed; cached gamma is context only",
		},
	}
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}

	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 5, 29, 14, 0, 0, 0, time.UTC),
	})

	vix, ok := findStressIndicator(res.MarketIndicators, "VIX/VIX3M")
	if !ok {
		t.Fatalf("missing VIX indicator: %+v", res.MarketIndicators)
	}
	if vix.Status != "green" || vix.AsOf != "2026-05-29" || !strings.Contains(vix.Reading, "0.910") || !strings.Contains(vix.Comment, "contango") {
		t.Fatalf("VIX indicator = %+v, want green dated reading with contango comment", vix)
	}
	gamma, ok := findStressIndicator(res.MarketIndicators, "γ-zero (SPY+SPX)")
	if !ok {
		t.Fatalf("missing combined gamma indicator: %+v", res.MarketIndicators)
	}
	if gamma.Status != "context" || !strings.Contains(gamma.Reading, "short-γ") || !strings.Contains(gamma.Comment, "context only") {
		t.Fatalf("gamma indicator = %+v, want context decision context", gamma)
	}
	if strings.Contains(gamma.Comment, rpc.GammaRankabilityContextOnly+"; "+rpc.GammaRankabilityContextOnly) {
		t.Fatalf("gamma comment duplicates state note: %+v", gamma)
	}
}

// TestComputeStressServesGaugeTriggers pins the trigger contract the Monitor
// faces render: every regime indicator carries the SERVED compact trip from
// its own threshold metadata, dealer gamma prefers the measured level pair
// (naming the index, because a combined result blends no scales), and the
// cushion reading is served beside the policy floor it trips at. A renderer
// that has to invent any of these is inventing policy.
func TestComputeStressServesGaugeTriggers(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Breadth.Thresholds = rpc.HeuristicThresholds("spx_breadth_50dma_v1", "> 55%", "40-55%", "< 40%", "trips <40% (50d)")
	r.FundingStress.Thresholds = rpc.HeuristicThresholds("funding_cp_tbill_v1", "< 25 bp", "25-75 bp", ">= 75 bp", "trips >=75 bp")
	r.GammaZero.Thresholds = rpc.HeuristicThresholds("dealer_gamma_v3", "above", "within", "below", "trips spot below gamma-zero")
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Scope:   rpc.GammaZeroScopeCombined,
		Quality: rankableStressGammaQuality(),
		PerIndex: map[string]*rpc.GammaZeroComputed{
			"SPX": {SpotUnderlying: 6135.2, ZeroGamma: new(6070.0)},
		},
	}

	res := ComputeStress(StressInput{Now: stressTestNow, Account: baseStressAccount(), Regime: r})

	breadth, ok := findStressIndicator(res.MarketIndicators, "SPX breadth")
	if !ok || breadth.Trip != "trips <40% (50d)" {
		t.Fatalf("breadth trip = %+v, want the served compact trip", breadth)
	}
	funding, ok := findStressIndicator(res.MarketIndicators, "funding spread")
	if !ok || funding.Trip != "trips >=75 bp" {
		t.Fatalf("funding trip = %+v, want the served compact trip", funding)
	}
	gamma, ok := findStressIndicator(res.MarketIndicators, "γ-zero (SPY+SPX)")
	if !ok || gamma.Trip != "SPX spot 6135.20 · γ-zero 6070.00" {
		t.Fatalf("gamma trip = %+v, want the served SPX level pair", gamma)
	}

	// No usable crossing anywhere: the face falls back to the worded rule
	// rather than printing a level nobody measured.
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeCombined, Quality: rankableStressGammaQuality()}
	res = ComputeStress(StressInput{Now: stressTestNow, Account: baseStressAccount(), Regime: r})
	gamma, ok = findStressIndicator(res.MarketIndicators, "γ-zero (SPY+SPX)")
	if !ok || gamma.Trip != "trips spot below gamma-zero" {
		t.Fatalf("gamma trip without a crossing = %+v, want the worded fallback", gamma)
	}

	// A producer that serves no thresholds leaves the face reading-only.
	vix, ok := findStressIndicator(res.MarketIndicators, "VIX/VIX3M")
	if !ok || vix.Trip != "" {
		t.Fatalf("VIX trip = %+v, want empty when no thresholds are served", vix)
	}

	if res.Portfolio.CushionTripPct == nil || *res.Portfolio.CushionTripPct != 35 {
		t.Fatalf("cushion trip = %v, want the served margin watch floor", res.Portfolio.CushionTripPct)
	}
}

func TestComputeStressMarketSummaryCarriesTapeLevels(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.HYGSPYDivergence.SPYPrice = new(512.34)
	r.HYGSPYDivergence.SPYChangePct = new(-0.75)
	r.VIXTermStructure.VIX = new(16.34)
	r.VIXTermStructure.VIXChangePct = new(3.61)

	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if res.Market.SPYPrice == nil || *res.Market.SPYPrice != 512.34 {
		t.Fatalf("spy_price = %v, want 512.34", res.Market.SPYPrice)
	}
	if res.Market.VIX == nil || *res.Market.VIX != 16.34 {
		t.Fatalf("vix = %v, want 16.34", res.Market.VIX)
	}
	if res.Market.SPYChangePct == nil || *res.Market.SPYChangePct != -0.75 {
		t.Fatalf("spy_change_pct = %v, want -0.75", res.Market.SPYChangePct)
	}
	if res.Market.VIXChangePct == nil || *res.Market.VIXChangePct != 3.61 {
		t.Fatalf("vix_change_pct = %v, want 3.61", res.Market.VIXChangePct)
	}
}

func TestComputeStressMarginPressureDoesNotActWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0.12
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: cached gamma is context only",
		},
	}
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}

	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    r,
	})

	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because context gamma does not block stress", res.Direction, res.Severity)
	}
	if res.Action != stressActionStandDown || res.InputHealth != stressInputOK {
		t.Fatalf("decision = action %s input %s, want stand_down/ok", res.Action, res.InputHealth)
	}
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for context-only gamma", got)
	}
	if hasSignal(res.Signals, risk.SignalRiskDataDegraded) {
		t.Fatalf("context gamma should not produce risk data degraded signal: %+v", res.Signals)
	}
}

func TestComputeStressDailyLossDoesNotActWithoutMarketConfirmation(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	daily := -11_000.0
	acct.DailyPnL = &daily
	r := healthyStressRegime()
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: cached gamma is context only",
		},
	}
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}

	res := ComputeStress(StressInput{Now: stressTestNow,
		Account:   acct,
		Positions: freshStressPositions(),
		Regime:    r,
	})

	if res.Direction != "" || res.Severity != risk.SeverityObserve {
		t.Fatalf("state = %s/%s, want observe because context gamma does not turn daily P&L into stress action", res.Direction, res.Severity)
	}
	if res.Action != stressActionStandDown || res.InputHealth != stressInputOK {
		t.Fatalf("decision = action %s input %s, want stand_down/ok", res.Action, res.InputHealth)
	}
}

func TestComputeStressMarginPressureDoesNotConfirmMarketTape(t *testing.T) {
	t.Parallel()
	acct := baseStressAccount()
	acct.Cushion = 0.12
	r := healthyStressRegime()
	spyPct := -2.6
	vixPct := 2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct

	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: acct,
		Regime:  r,
	})

	if res.Direction != risk.DirectionDefensive || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want defensive/watch from partial market pressure plus margin exposure", res.Direction, res.Severity)
	}
	if res.Action != stressActionWatch || res.PlannerReadiness != risk.PlannerReadinessPrestage {
		t.Fatalf("action/readiness = %s/%s, want watch/prestage", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarketSelloffViolent)
	if !ok {
		t.Fatalf("missing market selloff signal: %+v", res.Signals)
	}
	if sig.Severity != risk.SeverityWatch || !containsString(sig.BlockedBy, "confirmation") {
		t.Fatalf("selloff signal = severity %q blocked_by %+v, want watch blocked by market confirmation", sig.Severity, sig.BlockedBy)
	}
	if !rowContains(res.Rows, "Index tape shock", "needs confirmation") {
		t.Fatalf("expected tape row to require confirmation, rows: %+v", res.Rows)
	}
}

func TestComputeStressStaleAccountBlocksMarginAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	acct := baseStressAccount()
	acct.AsOf = now.Add(-2 * time.Hour)
	acct.Cushion = 0.07

	res := ComputeStress(StressInput{
		Account: acct,
		Regime:  healthyStressRegime(),
		Now:     now,
	})

	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch until account refresh", res.Direction, res.Severity)
	}
	if res.Action != stressActionConfirmInputs || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("action/readiness = %s/%s, want confirm_inputs/blocked", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalMarginCushionLow)
	if !ok {
		t.Fatalf("missing margin signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "account") || sig.Confidence != "medium-low" {
		t.Fatalf("margin signal = blocked_by %+v confidence %q, want stale account block", sig.BlockedBy, sig.Confidence)
	}
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input_health = %s, want degraded", res.InputHealth)
	}
}

func TestComputeStressStaleRegimeAuthorityCannotLookClear(t *testing.T) {
	t.Parallel()
	now := stressTestNow
	regime := healthyStressRegime()
	regime.AsOf = now
	lastSuccess := now.Add(-10 * time.Minute)
	ageSeconds := int64((10 * time.Minute) / time.Second)
	regime.AuthorityHealth = &rpc.RegimeAuthorityHealth{
		Status: rpc.RegimeAuthorityStale, LastSuccessAt: &lastSuccess,
		LastSuccessAgeSeconds: &ageSeconds, FailureCode: rpc.RegimeAuthorityFailureRefreshFailed,
	}

	res := ComputeStress(StressInput{
		Account: baseStressAccount(), Positions: freshStressPositions(), Regime: regime, Now: now,
	})
	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch || res.Action != stressActionConfirmInputs {
		t.Fatalf("decision=%s/%s action=%s, want data_quality/watch confirm_inputs", res.Direction, res.Severity, res.Action)
	}
	if res.InputHealth != stressInputDegraded || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("input/readiness=%s/%s, want degraded/blocked", res.InputHealth, res.PlannerReadiness)
	}
	signal, ok := findSignal(res.Signals, risk.SignalRiskDataDegraded)
	if !ok || !containsString(signal.BlockedBy, "regime") {
		t.Fatalf("missing Regime authority data-quality signal: %+v", res.Signals)
	}
	health := findSourceHealth(res.SourceHealth, "regime")
	if health == nil || health.Status != rpc.RegimeStatusStale {
		t.Fatalf("regime source health=%+v, want stale", health)
	}
	if notes := strings.Join(health.Notes, "\n"); !strings.Contains(notes, "regime last-good authority stale (refresh_failed)") {
		t.Fatalf("regime source notes=%q", notes)
	}
}

func TestComputeStressEstablishedAlertProjectionPreservesActAcrossAuthorityHealth(t *testing.T) {
	t.Parallel()
	now := stressTestNow
	regime := healthyStressRegime()
	regime.AsOf = now
	regime.Composite = rpc.RegimeComposite{
		ClusterGreenCount:       4,
		ClusterRedCount:         2,
		ClusterEligibleRedCount: 2,
		ClusterRankedCount:      6,
	}
	regime.VIXTermStructure.Band = "red"
	regime.VIXTermStructure.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	regime.VolOfVol.Band = "red"
	regime.VolOfVol.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	regime.HYGSPYDivergence.Band = "red"
	regime.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	regime.CreditSpreads.Band = "red"
	regime.CreditSpreads.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	account := baseStressAccount()
	account.AsOf = now
	account.GrossPositionValue = 110_000
	positions := freshStressPositions()

	lastSuccess := now
	ageSeconds := int64(0)
	freshRegime := regime
	freshRegime.AuthorityHealth = &rpc.RegimeAuthorityHealth{
		Status:                rpc.RegimeAuthorityFresh,
		LastSuccessAt:         &lastSuccess,
		LastSuccessAgeSeconds: &ageSeconds,
	}
	fresh := ComputeStress(StressInput{Account: account, Positions: positions, Regime: freshRegime, Now: now})
	if fresh.Action != stressActionDefend || fresh.Severity != risk.SeverityAct {
		t.Fatalf("fresh main decision=%s/%s, want defend/act", fresh.Action, fresh.Severity)
	}
	if fresh.EstablishedAlertProjection == nil {
		t.Fatal("fresh result missing established alert projection")
	}
	if err := rpc.ValidateEstablishedAlertProjection(*fresh.EstablishedAlertProjection); err != nil {
		t.Fatalf("fresh established projection invalid: %v", err)
	}
	if !fresh.EstablishedAlertProjection.OccurrenceEligible || !fresh.EstablishedAlertProjection.ActOnlyEligible {
		t.Fatalf("fresh established eligibility=%+v, want occurrence and act-only eligible", fresh.EstablishedAlertProjection)
	}
	if fresh.Fingerprint.Version != rpc.StressFingerprintVersion ||
		fresh.PolicyFingerprint.Version != risk.StressPolicyFingerprintVersion ||
		len(fresh.Rows) == 0 || fresh.Rows[0].Title != "Portfolio stress" {
		t.Fatalf("current Stress labels changed: fingerprint=%+v policy=%+v rows=%+v",
			fresh.Fingerprint, fresh.PolicyFingerprint, fresh.Rows)
	}
	if got, want := fresh.EstablishedAlertProjection.CanonicalFingerprint.Key, "sha256:6a6e879570bc1a4d98c9cf45deca585c45c64ec3d46b4a99843a31282b1ee45c"; got != want {
		t.Fatalf("established act fingerprint=%s, want ad5b77b golden %s", got, want)
	}

	for _, test := range []struct {
		name   string
		health *rpc.RegimeAuthorityHealth
		status string
	}{
		{
			name: "stale",
			health: &rpc.RegimeAuthorityHealth{
				Status:                rpc.RegimeAuthorityStale,
				LastSuccessAt:         &lastSuccess,
				LastSuccessAgeSeconds: &ageSeconds,
				FailureCode:           rpc.RegimeAuthorityFailureRefreshFailed,
			},
			status: rpc.RegimeStatusStale,
		},
		{
			name: "unavailable",
			health: &rpc.RegimeAuthorityHealth{
				Status:      rpc.RegimeAuthorityUnavailable,
				FailureCode: rpc.RegimeAuthorityFailureNoLastGood,
			},
			status: rpc.RegimeStatusUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changedRegime := regime
			changedRegime.AuthorityHealth = test.health
			changed := ComputeStress(StressInput{Account: account, Positions: positions, Regime: changedRegime, Now: now})
			health := findSourceHealth(changed.SourceHealth, "regime")
			if health == nil || health.Status != test.status {
				t.Fatalf("main regime source health=%+v, want %s", health, test.status)
			}
			if changed.InputHealth != stressInputDegraded {
				t.Fatalf("main input health=%s, want degraded", changed.InputHealth)
			}
			if changed.EstablishedAlertProjection == nil || *changed.EstablishedAlertProjection != *fresh.EstablishedAlertProjection {
				t.Fatalf("authority health changed established projection:\nfresh=%+v\nchanged=%+v", fresh.EstablishedAlertProjection, changed.EstablishedAlertProjection)
			}
		})
	}
}

func TestComputeStressEstablishedAlertProjectionIgnoresNewMarketEventRequirements(t *testing.T) {
	t.Parallel()
	now := stressTestNow
	account := baseStressAccount()
	account.AsOf = now
	positions := rpc.PositionsResult{
		AsOf:   now,
		Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
	}
	events := healthyStressMarketEvents(now, "XYZ")
	events.Fingerprint = rpc.BuildMarketEventsFingerprint(&events)
	baseline := ComputeStress(StressInput{
		Account: account, Positions: positions, Regime: healthyStressRegime(), MarketEvents: events, Now: now,
	})
	if baseline.EstablishedAlertProjection == nil {
		t.Fatal("baseline missing established alert projection")
	}
	if got, want := baseline.EstablishedAlertProjection.CanonicalFingerprint.Key, "sha256:4c84336687746ce418ce19bda97c1209e3c3731fdd85ab06820288be5a1b6c42"; got != want {
		t.Fatalf("established MarketEvents fingerprint=%s, want ad5b77b golden %s", got, want)
	}

	partial := events
	partial.SourceHealth = slices.Clone(events.SourceHealth)
	for i := range partial.SourceHealth {
		if partial.SourceHealth[i].Source == "trading_halts" {
			partial.SourceHealth[i].Status = rpc.SourceStatusPartial
		}
	}
	// The daemon-authored semantic fingerprint is held constant to isolate the
	// new required-source interpretation from a genuinely changed source
	// fingerprint. ad5b77b treated a provided partial child as healthy.
	partial.Fingerprint = events.Fingerprint

	missingRequiredChild := events
	missingRequiredChild.SourceHealth = slices.DeleteFunc(slices.Clone(events.SourceHealth), func(health rpc.SourceHealth) bool {
		return health.Source == "trading_halts"
	})
	missingRequiredChild.Fingerprint = events.Fingerprint

	for _, test := range []struct {
		name       string
		events     rpc.MarketEventsResult
		wantStatus string
	}{
		{name: "partial_child", events: partial, wantStatus: rpc.SourceStatusPartial},
		{name: "missing_required_child", events: missingRequiredChild, wantStatus: rpc.SourceStatusUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed := ComputeStress(StressInput{
				Account: account, Positions: positions, Regime: healthyStressRegime(), MarketEvents: test.events, Now: now,
			})
			health := findSourceHealth(changed.SourceHealth, "market_events")
			if health == nil || health.Status != test.wantStatus {
				t.Fatalf("main market-events source health=%+v, want %s", health, test.wantStatus)
			}
			if changed.Action != stressActionConfirmInputs || changed.InputHealth != stressInputDegraded {
				t.Fatalf("main decision=%s input=%s, want confirm_inputs/degraded", changed.Action, changed.InputHealth)
			}
			if changed.EstablishedAlertProjection == nil || *changed.EstablishedAlertProjection != *baseline.EstablishedAlertProjection {
				t.Fatalf("new MarketEvents interpretation changed established projection:\nbaseline=%+v\nchanged=%+v", baseline.EstablishedAlertProjection, changed.EstablishedAlertProjection)
			}
		})
	}
}

func TestComputeStressEstablishedAlertProjectionKeepsMissingMarketEventsAdvisory(t *testing.T) {
	t.Parallel()
	account := baseStressAccount()
	account.AsOf = stressTestNow
	res := ComputeStress(StressInput{
		Account: account,
		Positions: rpc.PositionsResult{
			AsOf:   stressTestNow,
			Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
		},
		Regime: healthyStressRegime(),
		Now:    stressTestNow,
	})
	if res.Action != stressActionConfirmInputs || res.InputHealth != stressInputDegraded {
		t.Fatalf("main decision=%s input=%s, want missing MarketEvents to remain confirm_inputs/degraded", res.Action, res.InputHealth)
	}
	health := findSourceHealth(res.SourceHealth, "market_events")
	if health == nil || health.Status != rpc.SourceStatusUnknown {
		t.Fatalf("main market-events source health=%+v, want unknown", health)
	}
	projection := res.EstablishedAlertProjection
	if projection == nil {
		t.Fatal("missing established alert projection")
	}
	if projection.Action != stressActionStandDown || projection.Severity != risk.SeverityObserve || projection.OccurrenceEligible || projection.ActOnlyEligible {
		t.Fatalf("established projection=%+v, want ad5 stand_down/observe/ineligible", projection)
	}
}

func TestComputeStressEstablishedAlertProjectionExcludesNewMissingAccountTimestampIssue(t *testing.T) {
	t.Parallel()
	account := baseStressAccount()
	account.AsOf = time.Time{}
	res := ComputeStress(StressInput{
		Account: account, Positions: freshStressPositions(), Regime: healthyStressRegime(), Now: stressTestNow,
	})
	if res.Action != stressActionConfirmInputs || res.InputHealth != stressInputDegraded {
		t.Fatalf("main decision=%s input=%s, want confirm_inputs/degraded", res.Action, res.InputHealth)
	}
	projection := res.EstablishedAlertProjection
	if projection == nil {
		t.Fatal("missing established alert projection")
	}
	if projection.Action != stressActionStandDown || projection.Severity != risk.SeverityObserve || projection.OccurrenceEligible || projection.ActOnlyEligible {
		t.Fatalf("established projection=%+v, want ad5 stand_down/observe/ineligible", projection)
	}
}

func TestComputeStressEstablishedAlertProjectionKeepsAccountAndPositionsConfirmInputsEligible(t *testing.T) {
	t.Parallel()
	now := stressTestNow
	staleAccount := baseStressAccount()
	staleAccount.AsOf = now.Add(-2 * time.Hour)
	staleAccount.Cushion = 0.07
	freshAccount := baseStressAccount()
	freshAccount.AsOf = now
	for _, test := range []struct {
		name      string
		account   rpc.AccountResult
		positions rpc.PositionsResult
		wantKey   string
	}{
		{
			name: "stale_account", account: staleAccount, positions: freshStressPositions(),
			wantKey: "sha256:183f4f116827746d7b1f8823112b5b3f8a3d4b3d3f73f6231677dcbdca196ecb",
		},
		{
			name: "missing_positions", account: freshAccount,
			wantKey: "sha256:98e6947dafd31135972ef90ea9767575cecfdba3c37ed619de453da7f1a4dde7",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			res := ComputeStress(StressInput{
				Account: test.account, Positions: test.positions, Regime: healthyStressRegime(), Now: now,
			})
			projection := res.EstablishedAlertProjection
			if projection == nil {
				t.Fatal("missing established alert projection")
			}
			if projection.Action != stressActionConfirmInputs || projection.Severity != risk.SeverityWatch ||
				!projection.OccurrenceEligible || !projection.ActOnlyEligible || !projection.PortfolioRelevant {
				t.Fatalf("established projection=%+v, want confirm_inputs/watch and both delivery modes eligible", projection)
			}
			if projection.CanonicalFingerprint.Key != test.wantKey {
				t.Fatalf("established fingerprint=%s, want ad5b77b golden %s", projection.CanonicalFingerprint.Key, test.wantKey)
			}
		})
	}
}

func TestComputeStressStalePositionsBlocksRebalanceAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now.Add(-2 * time.Hour),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{
					{Underlying: "AAPL", DollarDeltaBase: new(45_000.0)},
				},
			},
		},
		Regime: healthyStressRegime(),
		Now:    now,
	})

	if res.Direction != risk.DirectionDataQuality || res.Severity != risk.SeverityWatch {
		t.Fatalf("state = %s/%s, want data_quality/watch from stale position exposure", res.Direction, res.Severity)
	}
	if res.Action != stressActionConfirmInputs || res.PlannerReadiness != risk.PlannerReadinessBlocked {
		t.Fatalf("action/readiness = %s/%s, want confirm_inputs/blocked until positions refresh", res.Action, res.PlannerReadiness)
	}
	sig, ok := findSignal(res.Signals, risk.SignalSingleNameDeltaHigh)
	if !ok {
		t.Fatalf("missing single-name delta signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "positions") || sig.Confidence != "medium-low" {
		t.Fatalf("single-name signal = blocked_by %+v confidence %q, want stale positions block", sig.BlockedBy, sig.Confidence)
	}
}

func TestComputeStressStalePositionsBlockHeldStressAction(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 29, 16, 0, 0, 0, time.UTC)
	dailyLoss := -2_500.0
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf: now.Add(-2 * time.Hour),
			Portfolio: &rpc.PositionsPortfolio{
				ExposureBase: []rpc.UnderlyingExposure{{
					Underlying: "XYZ", MarketValueBase: 30_000, MarketValuePctNLV: new(30.0), DailyPnLBase: &dailyLoss,
				}},
			},
		},
		Regime: healthyStressRegime(),
		Now:    now,
	})
	if res.Direction != risk.DirectionDataQuality || res.Action != stressActionConfirmInputs {
		t.Fatalf("decision = %s/%s, want confirm_inputs while held-stress positions are stale", res.Direction, res.Action)
	}
	sig, ok := findSignal(res.Signals, risk.SignalHeldUnderlyingPnLShock)
	if !ok {
		t.Fatalf("missing held P&L signal: %+v", res.Signals)
	}
	if !containsString(sig.BlockedBy, "positions") || sig.Confidence != "medium-low" {
		t.Fatalf("held P&L signal = blocked_by %+v confidence %q, want stale positions block", sig.BlockedBy, sig.Confidence)
	}
}

func TestComputeStressStaleRedClusterDoesNotConfirmLifecycle(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterGreenCount: 4, ClusterRankedCount: 6}
	r.VIXTermStructure.Band = "red"
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.VolOfVol.Band = "red"
	r.HYGSPYDivergence.Band = "red"
	r.CreditSpreads.Band = "red"

	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})

	// Pre-eligibility this emitted an act-grade confirmed signal with a
	// BlockedBy disclosure. Under the confirmation gates a stale red is
	// not eligible at all, so the confirmed signal must not fire — the
	// visible reds warn through the early signal instead.
	if _, confirmed := findSignal(res.Signals, risk.SignalRegimeStressConfirmed); confirmed {
		t.Fatalf("stale red clusters must not emit confirmed stress: %+v", res.Signals)
	}
	if _, ok := findSignal(res.Signals, risk.SignalRegimeStressEarly); !ok {
		t.Fatalf("missing early regime stress signal: %+v", res.Signals)
	}
	if res.MarketConfirmation == stressMarketConfirmed {
		t.Fatalf("market_confirmation = %s, want stale vol to block confirmed market stress", res.MarketConfirmation)
	}
	if res.InputHealth != stressInputDegraded {
		t.Fatalf("input_health = %s, want degraded", res.InputHealth)
	}
}

func TestComputeStressRegimeSourceHealthKeepsStaleAndDegradedNotes(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 4, ClusterRedCount: 1, ClusterRankedCount: 6}
	r.VIXTermStructure.Status = rpc.RegimeStatusStale
	r.GammaZero.Band = "red"
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityBlocked,
			RankabilityReason: "freshness: cache age exceeds TTL",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
	}
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		Now:     time.Date(2026, 6, 1, 15, 0, 0, 0, time.UTC),
	})
	var regimeHealth *rpc.SourceHealth
	for i := range res.SourceHealth {
		if res.SourceHealth[i].Source == "regime" {
			regimeHealth = &res.SourceHealth[i]
			break
		}
	}
	if regimeHealth == nil {
		t.Fatalf("missing regime source health: %+v", res.SourceHealth)
	}
	if regimeHealth.Status != "partial" {
		t.Fatalf("regime source status = %q, want partial", regimeHealth.Status)
	}
	notes := strings.Join(regimeHealth.Notes, "\n")
	for _, want := range []string{"stale clusters: vol", "degraded clusters: gamma"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("regime source notes = %q, want %q", notes, want)
		}
	}
}

func TestComputeStressCriticalMarketEventHealthBlocksCleanInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		source string
		status string
	}{
		{name: "halt_and_luld_degraded", source: "trading_halts", status: rpc.SourceStatusDegraded},
		{name: "halt_and_luld_unknown", source: "trading_halts", status: rpc.SourceStatusUnknown},
		{name: "reg_sho_stale", source: "reg_sho_threshold", status: rpc.SourceStatusStale},
		{name: "reg_sho_partial", source: "reg_sho_threshold", status: rpc.SourceStatusPartial},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			positions := rpc.PositionsResult{
				AsOf: stressTestNow,
				Stocks: []rpc.PositionView{{
					Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100,
				}},
			}
			events := healthyStressMarketEvents(stressTestNow, "XYZ")
			for i := range events.SourceHealth {
				if events.SourceHealth[i].Source == test.source {
					events.SourceHealth[i].Status = test.status
				}
			}

			res := ComputeStress(StressInput{
				Account:      baseStressAccount(),
				Positions:    positions,
				Regime:       healthyStressRegime(),
				MarketEvents: events,
				Now:          stressTestNow,
			})

			if res.InputHealth != stressInputDegraded || res.Action != stressActionConfirmInputs {
				t.Fatalf("decision = input %q action %q, want degraded/confirm_inputs", res.InputHealth, res.Action)
			}
			health := findSourceHealth(res.SourceHealth, "market_events")
			if health == nil || health.Status != test.status {
				t.Fatalf("market-events health = %+v, want status %q", health, test.status)
			}
			warning := "market-event source " + test.source + ": " + test.status
			if !strings.Contains(strings.Join(res.Warnings, "\n"), warning) {
				t.Fatalf("warnings = %+v, want %q", res.Warnings, warning)
			}
			if strings.Contains(strings.Join(res.Warnings, "\n"), "borrow_") {
				t.Fatalf("all-long book must not surface borrow health warnings: %+v", res.Warnings)
			}
		})
	}
}

func TestComputeStressMarketEventHealthRequiresCurrentTimestamps(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		mutate     func(*rpc.MarketEventsResult)
		wantStatus string
	}{
		{
			name: "aggregate_timestamp_missing",
			mutate: func(events *rpc.MarketEventsResult) {
				events.AsOf = time.Time{}
			},
			wantStatus: rpc.SourceStatusUnknown,
		},
		{
			name: "aggregate_timestamp_stale",
			mutate: func(events *rpc.MarketEventsResult) {
				events.AsOf = stressTestNow.Add(-11 * time.Minute)
			},
			wantStatus: rpc.SourceStatusStale,
		},
		{
			name: "required_child_timestamp_missing",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[0].AsOf = time.Time{}
			},
			wantStatus: rpc.SourceStatusUnknown,
		},
		{
			name: "required_child_timestamp_stale",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[1].AsOf = stressTestNow.Add(-2 * time.Minute)
				events.SourceHealth[1].AgeSeconds = int64((2 * time.Minute).Seconds())
			},
			wantStatus: rpc.SourceStatusStale,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			events := healthyStressMarketEvents(stressTestNow, "XYZ")
			test.mutate(&events)
			account := baseStressAccount()
			account.AsOf = stressTestNow
			res := ComputeStress(StressInput{
				Account: account,
				Positions: rpc.PositionsResult{
					AsOf:   stressTestNow,
					Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
				},
				Regime:       healthyStressRegime(),
				MarketEvents: events,
				Now:          stressTestNow,
			})
			if res.InputHealth != stressInputDegraded || res.Action != stressActionConfirmInputs {
				t.Fatalf("decision = input %q action %q, want degraded/confirm_inputs", res.InputHealth, res.Action)
			}
			health := findSourceHealth(res.SourceHealth, "market_events")
			if health == nil || health.Status != test.wantStatus {
				t.Fatalf("market-events health = %+v, want status %q", health, test.wantStatus)
			}
		})
	}
}

func TestStressMarketEventSourceHealthUsesOwnCadence(t *testing.T) {
	t.Parallel()
	positions := rpc.PositionsResult{
		AsOf:   stressTestNow,
		Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
	}
	hasIssue := func(issues []stressSourceIssue, source string) bool {
		for _, issue := range issues {
			if issue.Source == source {
				return true
			}
		}
		return false
	}
	for _, test := range []struct {
		name      string
		shortBook bool
		mutate    func(*rpc.MarketEventsResult)
		source    string
		wantIssue bool
	}{
		{
			name: "reg_sho_older_than_generic_budget_but_inside_own_max",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[0].AsOf = stressTestNow.Add(-95 * time.Hour)
				events.SourceHealth[0].AgeSeconds = int64((95 * time.Hour).Seconds())
			},
			source: "reg_sho_threshold",
		},
		{
			name: "reg_sho_at_own_max_is_stale",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[0].AsOf = stressTestNow.Add(-96 * time.Hour)
				events.SourceHealth[0].AgeSeconds = int64((96 * time.Hour).Seconds())
			},
			source: "reg_sho_threshold", wantIssue: true,
		},
		{
			name: "missing_max_uses_conservative_fallback",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[0].AsOf = stressTestNow.Add(-11 * time.Minute)
				events.SourceHealth[0].AgeSeconds = int64((11 * time.Minute).Seconds())
				events.SourceHealth[0].MaxAgeSeconds = 0
			},
			source: "reg_sho_threshold", wantIssue: true,
		},
		{
			name: "future_timestamp_fails_closed",
			mutate: func(events *rpc.MarketEventsResult) {
				events.SourceHealth[0].AsOf = stressTestNow.Add(2 * time.Minute)
			},
			source: "reg_sho_threshold", wantIssue: true,
		},
		{
			name:      "not_due_borrow_fee_is_not_re_staled",
			shortBook: true,
			mutate: func(events *rpc.MarketEventsResult) {
				health := &events.SourceHealth[3]
				health.AsOf = stressTestNow.Add(-72 * time.Hour)
				health.AgeSeconds = int64((72 * time.Hour).Seconds())
				health.RefreshState = rpc.SourceRefreshNotDue
			},
			source: "borrow_fee",
		},
		{
			name:      "explicit_stale_wins_over_not_due",
			shortBook: true,
			mutate: func(events *rpc.MarketEventsResult) {
				health := &events.SourceHealth[3]
				health.Status = rpc.SourceStatusStale
				health.RefreshState = rpc.SourceRefreshNotDue
			},
			source: "borrow_fee", wantIssue: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			events := healthyStressMarketEvents(stressTestNow, "XYZ")
			test.mutate(&events)
			book := positions
			if test.shortBook {
				book.Stocks = []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: -100}}
			}
			issues := stressMarketEventSourceIssues(book, events, stressTestNow)
			if got := hasIssue(issues, test.source); got != test.wantIssue {
				t.Fatalf("issues=%+v, source %q issue=%v want %v", issues, test.source, got, test.wantIssue)
			}
		})
	}
}

func TestComputeStressRealAccountRequiresSnapshotTimestamp(t *testing.T) {
	t.Parallel()
	account := baseStressAccount()
	account.AsOf = time.Time{}
	res := ComputeStress(StressInput{
		Account:   account,
		Positions: freshStressPositions(),
		Regime:    healthyStressRegime(),
		Now:       stressTestNow,
	})
	if res.InputHealth != stressInputDegraded || res.Action != stressActionConfirmInputs {
		t.Fatalf("decision = input %q action %q, want degraded/confirm_inputs", res.InputHealth, res.Action)
	}
	health := findSourceHealth(res.SourceHealth, "account")
	if health == nil || health.Status == rpc.SourceStatusOK {
		t.Fatalf("account health = %+v, want non-ok missing-timestamp state", health)
	}
}

func TestComputeStressMissingCriticalMarketEventContextIsUnknown(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf:   stressTestNow,
			Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: 100}},
		},
		Regime: healthyStressRegime(),
		Now:    stressTestNow,
	})
	if res.InputHealth != stressInputDegraded || res.Action != stressActionConfirmInputs {
		t.Fatalf("decision = input %q action %q, want degraded/confirm_inputs", res.InputHealth, res.Action)
	}
	health := findSourceHealth(res.SourceHealth, "market_events")
	if health == nil || health.Status != rpc.SourceStatusUnknown {
		t.Fatalf("market-events health = %+v, want unknown", health)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "market-event source market_events: unknown") {
		t.Fatalf("warnings = %+v, want explicit missing market-event context", res.Warnings)
	}
}

func TestComputeStressBorrowHealthRequiresShortStockExposure(t *testing.T) {
	t.Parallel()
	events := healthyStressMarketEvents(stressTestNow, "XYZ")
	for i := range events.SourceHealth {
		switch events.SourceHealth[i].Source {
		case "borrow_inventory":
			events.SourceHealth[i].Status = rpc.SourceStatusStale
		case "borrow_fee":
			events.SourceHealth[i].Status = rpc.SourceStatusUnknown
		}
	}
	for _, test := range []struct {
		name       string
		quantity   float64
		wantInput  string
		wantStatus string
		wantBorrow bool
	}{
		{name: "long_stock_ignores_borrow_health", quantity: 100, wantInput: stressInputOK, wantStatus: rpc.SourceStatusOK},
		{name: "short_stock_requires_borrow_health", quantity: -100, wantInput: stressInputDegraded, wantStatus: rpc.SourceStatusUnknown, wantBorrow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			res := ComputeStress(StressInput{
				Account: baseStressAccount(),
				Positions: rpc.PositionsResult{
					AsOf:   stressTestNow,
					Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: rpc.SecTypeStock, Quantity: test.quantity}},
				},
				Regime:       healthyStressRegime(),
				MarketEvents: events,
				Now:          stressTestNow,
			})
			if res.InputHealth != test.wantInput {
				t.Fatalf("input_health = %q, want %q", res.InputHealth, test.wantInput)
			}
			health := findSourceHealth(res.SourceHealth, "market_events")
			if health == nil || health.Status != test.wantStatus {
				t.Fatalf("market-events health = %+v, want status %q", health, test.wantStatus)
			}
			hasBorrowWarning := strings.Contains(strings.Join(res.Warnings, "\n"), "borrow_")
			if hasBorrowWarning != test.wantBorrow {
				t.Fatalf("borrow warning present = %v, want %v; warnings: %+v", hasBorrowWarning, test.wantBorrow, res.Warnings)
			}
		})
	}
}

func TestStressShortStockExposureRejectsExplicitNonStockRows(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		book rpc.PositionsResult
		want bool
	}{
		{name: "stock", book: rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: rpc.SecTypeStock, Quantity: -1}}}, want: true},
		{name: "wire_stock", book: rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: "STK", Quantity: -1}}}, want: true},
		{name: "legacy_stock", book: rpc.PositionsResult{Stocks: []rpc.PositionView{{Quantity: -1}}}, want: true},
		{name: "future", book: rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: "FUT", Quantity: -1}}}},
		{name: "grouped_index", book: rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Stock: &rpc.PositionView{SecType: "IND", Quantity: -1}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := stressHasShortStockExposure(test.book); got != test.want {
				t.Fatalf("stressHasShortStockExposure()=%v, want %v", got, test.want)
			}
		})
	}
}

func TestComputeStressSeparatesPartialFromAmbiguousClusters(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 4, ClusterRedCount: 1, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.HYGSPYDivergence.Band = "green"
	r.HYGSPYDivergence.Status = rpc.RegimeStatusOK
	r.CreditSpreads.Band = "unranked"
	r.CreditSpreads.Status = rpc.RegimeStatusError
	r.FundingStress.Band = "unranked"
	r.FundingStress.Status = rpc.RegimeStatusError
	r.GammaZero.Band = "red"
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "funding" {
		t.Fatalf("ambiguous clusters = %q, want funding", got)
	}
	if got := strings.Join(res.Market.PartialClusters, ","); got != "credit" {
		t.Fatalf("partial clusters = %q, want credit", got)
	}
}

func TestComputeStressOptionsPresentWithoutGreeksIsDataQuality(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{
			AsOf:    time.Now(),
			Options: []rpc.PositionView{{Symbol: "SPY", SecType: rpc.SecTypeOption, Quantity: 1}},
		},
		Regime: healthyStressRegime(),
	})
	if !rowContains(res.Rows, "Options convexity", "greeks coverage is unavailable") {
		t.Fatalf("expected options data-quality row, rows: %+v", res.Rows)
	}
	sig, ok := findSignal(res.Signals, risk.SignalOptionGreeksDegraded)
	if !ok || sig.Direction != risk.DirectionDataQuality {
		t.Fatalf("missing option greeks degraded signal, signals: %+v", res.Signals)
	}
}

func TestStressWarningsSanitizeExternalErrors(t *testing.T) {
	t.Parallel()
	line := stressWarningLine(rpc.RegimeWarning{
		Code:    "credit_spreads_unavailable",
		Scope:   "credit_spreads",
		Message: "HY OAS: GET https://fred.stlouisfed.org/graph/fredgraph.csv?id=BAMLH0A0HYM2: HTTP 404 Not Found",
		Impact:  "cash credit row is unranked; ETF credit proxy may still rank the credit cluster.",
		Action:  "Retry later.",
	})
	if strings.Contains(line, "https://") || strings.Contains(line, "HTTP 404") {
		t.Fatalf("warning leaked noisy transport error: %s", line)
	}
	if !strings.Contains(line, "cash credit row is unranked") {
		t.Fatalf("warning did not preserve useful impact: %s", line)
	}
}

func TestStressWarningsPolishUnrankedImpact(t *testing.T) {
	t.Parallel()
	line := stressWarningLine(rpc.RegimeWarning{
		Scope:  "credit_spreads",
		Impact: "cash credit spreads is unranked; the composite has lower coverage.",
	})
	if !strings.Contains(line, "cash credit spreads row is unranked") {
		t.Fatalf("warning did not polish unranked impact: %s", line)
	}
}

func TestStressWarningsPreferScopedInputChecks(t *testing.T) {
	t.Parallel()
	regime := rpc.RegimeSnapshotResult{
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}},
			Envelope:            rpc.GammaZeroSPXResult{Result: &rpc.GammaZeroComputed{Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityContextOnly}}},
		},
		VIXTermStructure: rpc.RegimeVIXTerm{RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}}},
		WarningDetails: []rpc.RegimeWarning{
			{Scope: "funding_stress", Message: "funding spread row is unranked; the composite has lower coverage."},
			{Scope: "gamma_zero", Message: "dealer gamma context_only: freshness: market is closed; cached gamma is context only"},
			{Scope: "vix_term_structure", Message: "volatility term structure stale"},
		},
	}
	warnings := stressWarnings(StressMarketSummary{
		AmbiguousClusters: []string{"funding"},
		UnrankedClusters:  1,
	}, regime, time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC))
	got := strings.Join(warnings, "\n")
	for _, unwanted := range []string{"ambiguous clusters:", "stale clusters:", "regime cluster(s) unranked"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("generic warning %q should be suppressed when scoped detail exists: %+v", unwanted, warnings)
		}
	}
	for _, unwanted := range []string{"gamma_zero:", "vix_term_structure:"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("context warning %q should be suppressed: %+v", unwanted, warnings)
		}
	}
	for _, want := range []string{"funding_stress:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("scoped warning %q missing: %+v", want, warnings)
		}
	}
}

func baseStressAccount() rpc.AccountResult {
	dailyPnL := 0.0
	return rpc.AccountResult{
		BaseCurrency:       "USD",
		NetLiquidation:     100_000,
		ExcessLiquidity:    50_000,
		Cushion:            0.50,
		GrossPositionValue: 60_000,
		DailyPnL:           &dailyPnL,
		AsOf:               time.Now(),
	}
}

// freshStressPositions is a fetched-and-current empty book. Tests that model
// an account-only decision still need it: a zero positions AsOf against a
// real NetLiquidation is a positions source issue, not a clean empty book.
func freshStressPositions() rpc.PositionsResult {
	return rpc.PositionsResult{AsOf: stressTestNow}
}

func healthyStressMarketEvents(now time.Time, symbols ...string) rpc.MarketEventsResult {
	return rpc.MarketEventsResult{
		Kind:          rpc.MarketEventsKind,
		SchemaVersion: rpc.MarketEventsSchemaVersion,
		AsOf:          now,
		Symbols:       slices.Clone(symbols),
		SourceHealth: []rpc.SourceHealth{
			{Source: "reg_sho_threshold", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: int64((96 * time.Hour).Seconds()), Confidence: "high"},
			{Source: "trading_halts", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: int64(time.Minute.Seconds()), Confidence: "high"},
			{Source: "borrow_inventory", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: int64((2 * time.Minute).Seconds()), Confidence: "medium"},
			{Source: "borrow_fee", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: int64((90 * time.Minute).Seconds()), Confidence: "medium"},
		},
	}
}

func TestBorrowFeeCoverageDoesNotChurnAllLongOrEstablishedMarketEventFingerprint(t *testing.T) {
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	base := healthyStressMarketEvents(now, "XYZ")
	base.Fingerprint = rpc.BuildMarketEventsFingerprint(&base)
	fee := 71.0
	withCoverage := base
	withCoverage.BorrowFeeCoverage = []rpc.MarketEventBorrowFeeCoverage{{
		Symbol: "XYZ", ContractConID: 481516, ContractFingerprint: "sha256:exact",
		CoverageScope: rpc.BorrowFeeCoveragePortfolioOnly, Status: rpc.BorrowFeeCoverageScaleUnknown,
		Reason: "scale_not_commissioned", Source: rpc.BorrowFeeSourceTWSHistorical,
		DataType: rpc.BorrowFeeDataTypeHistorical, AsOf: now.Add(-24 * time.Hour), ObservedAt: now,
		FeeRate: &fee, Entitlement: rpc.BorrowFeeEntitlementObserved,
		ScaleStatus: rpc.BorrowFeeScaleUnverified, PolicyEligible: false,
	}}
	withCoverage.Fingerprint = rpc.BuildMarketEventsFingerprint(&withCoverage)

	longOnly := rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: "stock", Quantity: 10}}}
	if got, want := stressRelevantMarketEventsFingerprint(longOnly, withCoverage), stressRelevantMarketEventsFingerprint(longOnly, base); got != want {
		t.Fatalf("all-long fingerprint changed on borrow-only coverage: %v != %v", got, want)
	}
	if got, want := stressEstablishedMarketEventsFingerprint(withCoverage), stressEstablishedMarketEventsFingerprint(base); got != want {
		t.Fatalf("established fingerprint changed on new coverage projection: %v != %v", got, want)
	}
}

func healthyStressRegime() rpc.RegimeSnapshotResult {
	return rpc.RegimeSnapshotResult{
		Composite: rpc.RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					Quality: rankableStressGammaQuality(),
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green"},
			Status:              rpc.RegimeStatusOK,
		},
	}
}

func rankableStressGammaQuality() *rpc.GammaSignalQuality {
	return &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}
}

func redVolCreditRegimeWithComputingSlowRows() rpc.RegimeSnapshotResult {
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterRedCount: 2, ClusterEligibleRedCount: 2, ClusterGreenCount: 2, ClusterRankedCount: 4, ClusterUnrankedCount: 2}
	r.VIXTermStructure.Band = "red"
	r.VIXTermStructure.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.VolOfVol.Band = "red"
	r.VolOfVol.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.CreditSpreads.Band = "red"
	r.CreditSpreads.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	r.GammaZero.Band = ""
	r.GammaZero.Status = rpc.RegimeStatusComputing
	r.Breadth.Band = ""
	r.Breadth.Status = rpc.RegimeStatusComputing
	return r
}

func rowContains(rows []StressRow, title, text string) bool {
	for _, row := range rows {
		if row.Title == title && strings.Contains(row.Guidance, text) {
			return true
		}
	}
	return false
}

func rowContainsEvidence(rows []StressRow, title, text string) bool {
	for _, row := range rows {
		if row.Title == title && strings.Contains(row.Evidence, text) {
			return true
		}
	}
	return false
}

func hasSignal(signals []risk.Signal, id risk.SignalID) bool {
	_, ok := findSignal(signals, id)
	return ok
}

func findSignal(signals []risk.Signal, id risk.SignalID) (risk.Signal, bool) {
	for _, sig := range signals {
		if sig.ID == id {
			return sig, true
		}
	}
	return risk.Signal{}, false
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func findSourceHealth(items []rpc.SourceHealth, source string) *rpc.SourceHealth {
	for i := range items {
		if items[i].Source == source {
			return &items[i]
		}
	}
	return nil
}

func findStressIndicator(indicators []StressMarketIndicator, name string) (StressMarketIndicator, bool) {
	for _, indicator := range indicators {
		if indicator.Name == name {
			return indicator, true
		}
	}
	return StressMarketIndicator{}, false
}

func stressRowByTitle(rows []StressRow, title string) *StressRow {
	for i := range rows {
		if rows[i].Title == title {
			return &rows[i]
		}
	}
	return nil
}

func TestStressTapeShockClosedDateDemotesToObserve(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	spyPct := -0.99
	vixPct := 12.19
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		// Sunday 16:00 ET: the VIX spike is a frozen Friday print.
		Now: time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC),
	})
	if res.Market.TapeSessionState != rpc.TapeSessionClosedDate || res.Market.TapeSessionReason != "weekend" {
		t.Fatalf("tape session = %q/%q, want closed_date/weekend", res.Market.TapeSessionState, res.Market.TapeSessionReason)
	}
	row := stressRowByTitle(res.Rows, "Index tape shock")
	if row == nil {
		t.Fatalf("missing tape row, rows: %+v", res.Rows)
	}
	if row.Severity != risk.SeverityObserve {
		t.Fatalf("closed-date tape severity = %s, want observe; row: %+v", row.Severity, row)
	}
	if !strings.Contains(row.Guidance, "confirm at next open") || !strings.Contains(row.Guidance, "Mon 09:30") {
		t.Fatalf("demoted guidance must name the next open, got %q", row.Guidance)
	}
	if !strings.Contains(row.Evidence, "frozen last-session prints") || !strings.Contains(row.Evidence, "VIX +12.19%") {
		t.Fatalf("demoted evidence must keep prints and provenance, got %q", row.Evidence)
	}
	if hasSignal(res.Signals, risk.SignalVolSpikeConfirmed) || hasSignal(res.Signals, risk.SignalMarketSelloffViolent) {
		t.Fatalf("closed-date frozen tape must not emit tape signals: %+v", res.Signals)
	}
}

func TestStressTapeShockPreMarketTradingDateKeepsWatch(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	vixPct := 12.0
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		// Tuesday 05:00 ET pre-market: live extended-hours prints are the
		// row's documented purpose and keep full severity.
		Now: time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC),
	})
	if res.Market.TapeSessionState != rpc.TapeSessionTradingDate {
		t.Fatalf("tape session = %q, want trading_date", res.Market.TapeSessionState)
	}
	row := stressRowByTitle(res.Rows, "Index tape shock")
	if row == nil || row.Severity != risk.SeverityWatch {
		t.Fatalf("pre-market tape severity must stay watch, row: %+v", row)
	}
}

func TestStressTapeShockHolidayDemotes(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	spyPct := -2.0
	r.HYGSPYDivergence.SPYChangePct = &spyPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		// Thanksgiving Day 2026 (Thursday) 10:00 ET.
		Now: time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC),
	})
	if res.Market.TapeSessionState != rpc.TapeSessionClosedDate || res.Market.TapeSessionReason == "" {
		t.Fatalf("tape session = %q/%q, want closed_date with a holiday reason", res.Market.TapeSessionState, res.Market.TapeSessionReason)
	}
	row := stressRowByTitle(res.Rows, "Index tape shock")
	if row == nil || row.Severity != risk.SeverityObserve {
		t.Fatalf("holiday tape severity must demote to observe, row: %+v", row)
	}
}

func TestStressTapeShockOutsideCoverageKeepsLegacySeverity(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	vixPct := 12.0
	r.VIXTermStructure.VIXChangePct = &vixPct
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
		// Sunday, but before embedded calendar coverage: replay fails open.
		Now: time.Date(2017, 8, 13, 20, 0, 0, 0, time.UTC),
	})
	if res.Market.TapeSessionState != "" {
		t.Fatalf("tape session = %q, want empty outside coverage", res.Market.TapeSessionState)
	}
	row := stressRowByTitle(res.Rows, "Index tape shock")
	if row == nil || row.Severity != risk.SeverityWatch {
		t.Fatalf("outside-coverage severity must keep legacy watch, row: %+v", row)
	}
}

func TestStressClosedDateTapeConfirmationGates(t *testing.T) {
	t.Parallel()
	spy := -2.6
	vix := 21.0
	m := StressMarketSummary{SPYChangePct: &spy, VIXChangePct: &vix}
	if !confirmedTapeStress(m) {
		t.Fatal("hard drop + hard spike must confirm on a trading/unknown date")
	}
	if !stressPanicMarket(StressMarketSummary{SPYChangePct: new(-4.5)}) {
		t.Fatal("crash tape must reach panic on a trading/unknown date")
	}
	m.TapeSessionState = rpc.TapeSessionClosedDate
	if confirmedTapeStress(m) {
		t.Fatal("frozen closed-date tape must not confirm stress")
	}
	if stressPanicMarket(StressMarketSummary{SPYChangePct: new(-4.5), TapeSessionState: rpc.TapeSessionClosedDate}) {
		t.Fatal("frozen closed-date tape must not reach panic")
	}
	if partialMarketPressure(m) {
		t.Fatal("frozen closed-date tape must not count as partial pressure")
	}
	rally := 3.5
	crush := -25.0
	mc := StressMarketSummary{SPYChangePct: &rally, VIXChangePct: &crush, TapeSessionState: rpc.TapeSessionClosedDate}
	if confirmedConstructiveTape(mc) || partialConstructiveTape(mc) {
		t.Fatal("frozen closed-date tape must not confirm constructive either")
	}
	fx := StressMarketSummary{
		SPYChangePct:    &spy,
		RedClusterNames: []string{"fx"},
	}
	if !stressFastCarryUnwind(fx) {
		t.Fatal("fx red + tape drop must fire carry unwind on a trading/unknown date")
	}
	fx.TapeSessionState = rpc.TapeSessionClosedDate
	if stressFastCarryUnwind(fx) {
		t.Fatal("fx red + frozen tape must not fire carry unwind on a closed date")
	}
	fx.YellowClusterNames = []string{"breadth"}
	if !stressFastCarryUnwind(fx) {
		t.Fatal("cluster-side carry-unwind arm must survive closed dates")
	}
}
