package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestComputeStressTreatsContextOnlyGammaAsContextNotDegraded(t *testing.T) {
	t.Parallel()
	r := healthyStressRegime()
	r.Composite = rpc.RegimeComposite{ClusterGreenCount: 5, ClusterRankedCount: 5, ClusterUnrankedCount: 1}
	r.GammaZero.Band = "red"
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityContextOnly,
			RankabilityReason: "freshness: market is closed; cached gamma is context only",
		},
		Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
		WarningDetails: []rpc.GammaWarningDetail{{
			Code:     "spx_cache_fallback:no_data",
			Scope:    "SPX",
			Severity: "data_quality",
			Message:  "SPX live refresh was unavailable; using the last successful cached SPX slice.",
		}},
	}
	r.WarningDetails = []rpc.RegimeWarning{{
		Code:     "gamma_zero_context_only",
		Scope:    "gamma_zero",
		Severity: "info",
		Message:  "dealer gamma context_only: freshness: market is closed; cached gamma is context only",
		Impact:   "dealer gamma is displayed as context but is not ranked or used as independent stress confirmation.",
	}}
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  r,
	})
	if got := strings.Join(res.Market.DegradedClusters, ","); got != "" {
		t.Fatalf("degraded clusters = %q, want none for context-only gamma", got)
	}
	if got := strings.Join(res.Market.AmbiguousClusters, ","); got != "" {
		t.Fatalf("ambiguous clusters = %q, want none for context-only gamma", got)
	}
	var out bytes.Buffer
	renderStressTextWidthDetails(&Env{}, &out, &res, 120, true)
	rendered := out.String()
	for _, want := range []string{
		"Market indicators",
		"STATE",
		"context",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("render missing %q:\n%s", want, rendered)
		}
	}
	for _, unwanted := range []string{
		"warning: degraded clusters: gamma",
		"Degraded input   gamma",
		"degraded gamma",
		"ambiguous clusters: gamma",
		"verify: gamma cannot confirm",
		"Gamma is after-hours/context-only. Action:",
		"Input checks",
	} {
		if strings.Contains(rendered, unwanted) {
			t.Fatalf("render should not contain %q:\n%s", unwanted, rendered)
		}
	}
}

func TestStressWarningLabelsAreActionOriented(t *testing.T) {
	t.Parallel()
	tests := []struct {
		warning string
		want    string
	}{
		{warning: "ambiguous clusters: funding and gamma", want: "verify"},
		{warning: "funding_stress: funding spread row is unranked; the composite has lower coverage.", want: "verify"},
		{warning: "stale clusters: vol", want: "refresh"},
		{warning: "gamma_zero: dealer gamma context_only: freshness: market is closed; cached gamma is context only", want: "context"},
		{warning: "gamma_zero: dealer gamma blocked", want: "warning"},
		{warning: "credit_spreads: source error", want: "error"},
	}
	for _, tt := range tests {
		got, _ := stressWarningLabel(tt.warning)
		if got != tt.want {
			t.Fatalf("stressWarningLabel(%q) = %q, want %q", tt.warning, got, tt.want)
		}
	}
}

func TestRenderStressTextShowsActionEvidenceAndInputHealth(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	var out bytes.Buffer
	renderStressText(&Env{}, &out, &res)
	got := out.String()
	for _, want := range []string{
		"Action     WATCH",
		"Guidance   Market stress is confirmed, but your exposure is low; keep watching — no reductions needed.",
		"Next step  Stage defensive review",
		"Why this fired",
		"Market weather",
		"Portfolio shape",
		"Combined read",
		"Market indicators",
		"INDICATOR",
		"STATE",
		"READING / COMMENT",
		"Input checks",
		"computing",
		"breadth and gamma",
		"Alert ID   stress-fp-v2 sha256:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Posture") || strings.Contains(got, "Lifecycle") || strings.Contains(got, "Confidence") {
		t.Fatalf("render leaked deleted hero fields:\n%s", got)
	}
	if strings.Contains(got, "Title                        Risk state") {
		t.Fatalf("default render should not use wide details table:\n%s", got)
	}
}

func TestStressInputHealthRowsHumanizeMarketIssues(t *testing.T) {
	t.Parallel()
	res := StressResult{
		InputHealth: stressInputDegraded,
		Market: StressMarketSummary{
			DegradedClusters: []string{"gamma"},
			StaleClusters:    []string{"credit", "fx", "vol"},
		},
	}
	rows := stressInputHealthRows(&res)
	got := fmt.Sprint(rows)
	for _, want := range []string{
		"Degraded input gamma",
		"Stale input credit, FX, and vol",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("input health rows missing %q: %s", want, got)
		}
	}
}

func TestRenderStressDetailsShowsRowsWhenRequested(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  healthyStressRegime(),
	})
	var out bytes.Buffer
	renderStressTextWidthDetails(&Env{}, &out, &res, 100, true)
	if !strings.Contains(out.String(), "Details") || !strings.Contains(out.String(), "Immediate margin safety") {
		t.Fatalf("details render missing row evidence:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Portfolio stress ·") {
		t.Fatalf("details render should not duplicate the top-level stress row:\n%s", out.String())
	}
}

func TestRenderStressTextWrapsAtCommonTerminalWidths(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	res.Warnings = append(res.Warnings,
		"vix_term_structure: volatility term structure stale",
		"breadth: breadth is still computing.",
		"long_detail: "+strings.Repeat("after-hours-market-data-limitation ", 5),
	)

	for _, width := range []int{80, 100, 120} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			for _, color := range []bool{false, true} {
				var out bytes.Buffer
				renderStressTextWidth(&Env{Color: color}, &out, &res, width)
				for i, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
					if got := visibleLen(line); got > width {
						t.Fatalf("line %d visible width = %d, want <= %d:\n%s\nfull output:\n%s", i+1, got, width, line, out.String())
					}
				}
			}
		})
	}
}

func TestRenderStressTextHidesDetailsUnlessRequested(t *testing.T) {
	t.Parallel()
	res := ComputeStress(StressInput{
		Account: baseStressAccount(),
		Regime:  redVolCreditRegimeWithComputingSlowRows(),
		Now:     time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
	})
	var normal bytes.Buffer
	renderStressTextWidth(&Env{}, &normal, &res, 120)
	if strings.Contains(normal.String(), "  Details\n") {
		t.Fatalf("default stress render should hide full details:\n%s", normal.String())
	}
	if strings.Contains(normal.String(), "Title                        Risk state") {
		t.Fatalf("default stress render should not use wide table:\n%s", normal.String())
	}

	var details bytes.Buffer
	renderStressTextWidthDetails(&Env{}, &details, &res, 120, true)
	if !strings.Contains(details.String(), "  Details\n") {
		t.Fatalf("details stress render should use stacked details:\n%s", details.String())
	}
	if strings.Contains(details.String(), "Title                        Risk state") {
		t.Fatalf("details stress render should not use wide table:\n%s", details.String())
	}
}

func TestRenderStressTextColorsCurrentState(t *testing.T) {
	t.Parallel()
	res := StressResult{
		AsOf:             time.Date(2026, 5, 29, 5, 55, 0, 0, time.FixedZone("CEST", 2*60*60)),
		Action:           stressActionWatch,
		Direction:        risk.DirectionDefensive,
		Severity:         risk.SeverityWatch,
		PlannerModeHint:  risk.PlannerModeStage,
		PlannerReadiness: risk.PlannerReadinessPrestage,
		Summary:          "Freeze new risk.",
		Rows: []StressRow{{
			Title:     "Portfolio stress",
			Direction: risk.DirectionDefensive,
			Severity:  risk.SeverityWatch,
			Guidance:  "Freeze new risk.",
		}},
	}
	var out bytes.Buffer
	renderStressText(&Env{Color: true}, &out, &res)
	got := out.String()
	if !strings.Contains(got, ansiBold) || !strings.Contains(got, ansiYellow) || !strings.Contains(got, "WATCH") {
		t.Fatalf("current watch action is not bold yellow:\n%q", got)
	}
	if strings.Contains(got, "CURRENT") {
		t.Fatalf("render should not repeat CURRENT:\n%q", got)
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
