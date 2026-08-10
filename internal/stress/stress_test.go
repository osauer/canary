package stress

import (
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"slices"
	"strings"
	"testing"
	"time"
)

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

func TestComputeStressUnmeasuredExposureRefusesACleanPass(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{Now: stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{AsOf: stressTestNow, Portfolio: &rpc.PositionsPortfolio{
			ExposureBase: []rpc.UnderlyingExposure{
				{Underlying: "AAPL", DollarDeltaBase: new(4_000.0), MarketValuePctNLV: new(4.0)},
			},
			ExposureUnmeasured: []string{"NESN"},
		}},
		Regime: healthyStressRegime(),
	})
	if got := res.Portfolio.ExposureUnmeasured; len(got) != 1 || got[0] != "NESN" {
		t.Fatalf("portfolio.exposure_unmeasured = %v, want [NESN]", got)
	}
	for _, title := range []string{"US equity/options exposure", "Largest concentration"} {
		row := stressRowByTitle(res.Rows, title)
		if row == nil {
			t.Fatalf("missing row %q, rows: %+v", title, res.Rows)
		}
		if row.Severity == risk.SeverityObserve || row.Direction != risk.DirectionDataQuality {
			t.Fatalf("%s = %s/%s, want a data-quality watch over a partial book", title, row.Direction, row.Severity)
		}
		if !strings.Contains(row.Evidence, "NESN") {
			t.Fatalf("%s evidence does not name the gap: %q", title, row.Evidence)
		}
	}
	sig, ok := findSignal(res.Signals, risk.SignalRiskDataDegraded)
	if !ok || sig.Subject != "exposure_base" || sig.Severity != risk.SeverityWatch {
		t.Fatalf("exposure completeness signal = %+v, want a watch on exposure_base", sig)
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

func freshStressPositions() rpc.PositionsResult {
	return rpc.PositionsResult{AsOf: stressTestNow}
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

func stressRowByTitle(rows []StressRow, title string) *StressRow {
	for i := range rows {
		if rows[i].Title == title {
			return &rows[i]
		}
	}
	return nil
}

func TestStressIncident20260612Regression(t *testing.T) {
	t.Parallel()
	spyChange := 0.3
	vixChange := -3.45
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{
		ClusterGreenCount: 4, ClusterYellowCount: 1, ClusterRedCount: 1,
		ClusterRankedCount: 6, ClusterProvisionalRedCount: 2,
	}

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
		Account: baseStressAccount(),
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
	if res.MarketConfirmation == stressMarketConfirmed {
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

func TestHYGSPYIndicatorShowsGapAndProvisionalConfirmation(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	hyg, average, spy, high := 79.51, 79.70, 100.0, 100.0
	r.HYGSPYDivergence.HYGPrice = &hyg
	r.HYGSPYDivergence.HYG50DMA = &average
	r.HYGSPYDivergence.SPYPrice = &spy
	r.HYGSPYDivergence.SPY52WHigh = &high
	r.HYGSPYDivergence.Band = "red"
	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Reasons: []string{"depth_below_min"}}

	rows := stressMarketIndicators(r, stressTestNow)
	var got StressMarketIndicator
	for _, row := range rows {
		if row.Name == "HYG vs SPY" {
			got = row
			break
		}
	}
	if got.Status != "amber" {
		t.Fatalf("status = %q, want amber while confirmation is provisional", got.Status)
	}
	if !strings.Contains(got.Reading, "0.24% below 50d 79.70") {
		t.Fatalf("reading = %q, want measured gap and average", got.Reading)
	}
	if !strings.Contains(got.Comment, "confirmation starts at 0.25%") {
		t.Fatalf("comment = %q, want confirmation floor", got.Comment)
	}

	r.HYGSPYDivergence.Eligibility = nil
	rows = stressMarketIndicators(r, stressTestNow)
	for _, row := range rows {
		if row.Name == "HYG vs SPY" && row.Status != "amber" {
			t.Fatalf("missing-eligibility status = %q, want amber", row.Status)
		}
	}

	r.HYGSPYDivergence.Eligibility = &rpc.RegimeEligibility{Eligible: true}
	rows = stressMarketIndicators(r, stressTestNow)
	for _, row := range rows {
		if row.Name == "HYG vs SPY" && row.Status != "red" {
			t.Fatalf("confirmed status = %q, want red", row.Status)
		}
	}
}
