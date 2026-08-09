package stress

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
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
