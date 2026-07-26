package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestGammaZeroStatusAndRegimeUsesGapForCrossing(t *testing.T) {
	cases := []struct {
		name string
		gap  float64
		want string
	}{
		{"above_zero_long_gamma", 3.0, "long_gamma"},
		{"near_zero_transition", 0.5, "transition_gamma"},
		{"below_zero_short_gamma", -3.0, "short_gamma"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zero := 100.0
			c := &rpc.GammaZeroComputed{ZeroGamma: &zero, GapPct: &tc.gap}
			status, regime := gammaZeroStatusAndRegime(c)
			if status != "crossing" {
				t.Fatalf("status = %q, want crossing", status)
			}
			if regime != tc.want {
				t.Fatalf("regime = %q, want %q", regime, tc.want)
			}
		})
	}
}

func TestBuildGammaSummaryCombinedNamesSPXCanonicalAndSPYContext(t *testing.T) {
	t.Parallel()
	zero := 5425.0
	gap := 0.8
	spx := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 5468.4,
		ZeroGamma:      &zero,
		GapPct:         &gap,
		LegCount:       2100,
		GammaTotalAbs:  4_200_000_000,
	}
	spy := &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		SpotUnderlying: 746,
		GammaSign:      "positive",
		SweepLowAbs:    670,
		SweepHighAbs:   820,
		LegCount:       900,
		GammaTotalAbs:  1_200_000_000,
	}
	summary := buildGammaSummary(&rpc.GammaZeroComputed{
		Scope:           rpc.GammaZeroScopeCombined,
		PerIndex:        map[string]*rpc.GammaZeroComputed{"SPY": spy, "SPX": spx},
		RegimeAgreement: "agree:long-gamma",
	})

	if summary == nil {
		t.Fatal("summary nil")
	}
	for _, want := range []string{
		"SPX canonical zero-gamma $5425.00",
		"SPY context stayed long-gamma across $670.00-$820.00",
		"Price levels remain per-index",
	} {
		if !strings.Contains(summary.PrimaryStatement, want) {
			t.Fatalf("primary statement missing %q: %q", want, summary.PrimaryStatement)
		}
	}
	for _, banned := range []string{"No combined zero", "SPY none in", "SPX none in"} {
		if strings.Contains(summary.PrimaryStatement, banned) {
			t.Fatalf("primary statement leaked raw phrase %q: %q", banned, summary.PrimaryStatement)
		}
	}
	if summary.PerIndex["SPX"].ZeroGammaStatus != "crossing" || summary.PerIndex["SPY"].ZeroGammaStatus != "none_in_window" {
		t.Fatalf("per-index statuses = %+v", summary.PerIndex)
	}
}

func TestBuildGammaSummarySingleNoCrossingUsesPlainLanguage(t *testing.T) {
	t.Parallel()
	summary := buildGammaSummary(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		SpotUnderlying: 5468.4,
		GammaSign:      "negative",
		SweepLowAbs:    5000,
		SweepHighAbs:   6000,
		LegCount:       2100,
		GammaTotalAbs:  4_200_000_000,
	})
	if summary == nil {
		t.Fatal("summary nil")
	}
	for _, want := range []string{
		"SPX stayed short-gamma across $5000.00-$6000.00",
		"Zero-gamma:",
	} {
		if !strings.Contains(summary.PrimaryStatement, want) {
			t.Fatalf("primary statement missing %q: %q", want, summary.PrimaryStatement)
		}
	}
	if strings.Contains(summary.PrimaryStatement, "none in") {
		t.Fatalf("primary statement leaked terse no-crossing wording: %q", summary.PrimaryStatement)
	}
}

func TestBuildGammaSummaryUnavailableSliceExplainsMissingPayload(t *testing.T) {
	t.Parallel()
	summary := buildGammaSummary(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		LegCount:       120,
		GammaTotalAbs:  0,
		PricedLegCount: 800,
	})
	if summary == nil {
		t.Fatal("summary nil")
	}
	for _, want := range []string{
		"SPX unavailable",
		"no usable gamma magnitude from landed legs",
	} {
		if !strings.Contains(summary.PrimaryStatement, want) {
			t.Fatalf("primary statement missing %q: %q", want, summary.PrimaryStatement)
		}
	}
	if summary.ZeroGammaStatus != "unavailable" || summary.Confidence != "unavailable" {
		t.Fatalf("summary = %+v, want unavailable status/confidence", summary)
	}
}

func TestSPXUnavailableWarningTextFetchCanceledIsUserFacing(t *testing.T) {
	t.Parallel()
	message, impact, action := spxUnavailableWarningText("fetch_canceled")
	for _, got := range []string{message, impact, action} {
		if strings.Contains(got, "context canceled") {
			t.Fatalf("warning text leaked raw context error: message=%q impact=%q action=%q", message, impact, action)
		}
	}
	for _, want := range []string{
		"canceled before usable data landed",
		"Showing SPY only",
		"09:30-16:15 ET",
		"--only=spy",
	} {
		if !strings.Contains(message+" "+impact+" "+action, want) {
			t.Fatalf("warning text missing %q: message=%q impact=%q action=%q", want, message, impact, action)
		}
	}
}

func TestSPYUnavailableWarningTextZeroMagnitudeIsUserFacing(t *testing.T) {
	t.Parallel()
	message, impact, action := spyUnavailableWarningText("zero_magnitude")
	for _, want := range []string{
		"SPY option chain was skipped",
		"Showing SPX only",
		"regular trading hours",
		"--only=spy --force",
	} {
		if !strings.Contains(message+" "+impact+" "+action, want) {
			t.Fatalf("warning text missing %q: message=%q impact=%q action=%q", want, message, impact, action)
		}
	}
}

func TestSPXCacheFallbackWarningTextPointsToRankability(t *testing.T) {
	t.Parallel()
	message, impact, action := spxCacheFallbackWarningText("timeout")
	for _, want := range []string{
		"using the last successful cached SPX slice",
		"quality.rankability shows",
		"market-structure signal",
	} {
		if !strings.Contains(message+" "+impact+" "+action, want) {
			t.Fatalf("cache fallback warning missing %q: message=%q impact=%q action=%q", want, message, impact, action)
		}
	}
	if strings.Contains(message+" "+impact+" "+action, "treat the combined gamma regime as degraded") {
		t.Fatalf("cache fallback warning should not force degraded phrasing: message=%q impact=%q action=%q", message, impact, action)
	}
}

func TestGammaWarningDetailSPYUnavailableScopesToSPY(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX}, "spy_unavailable:zero_magnitude")
	if got.Severity != "data_quality" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY data_quality warning", got)
	}
	for _, want := range []string{"Showing SPX only", "SPY gamma is not included"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailStrikeBudgetCapped(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{Scope: rpc.GammaZeroScopeSPX}, "strike_budget_capped")
	if got.Severity != "methodology" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX methodology warning", got)
	}
	for _, want := range []string{"nearest 80", "gateway request budget"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailAllDerivedIVNamesModelTickFailure(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:                rpc.GammaZeroScopeSPX,
		PricedLegCount:       865,
		DerivedIVLegs:        865,
		DerivedPrevCloseLegs: 865,
	}, "all_iv_derived")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality warning", got)
	}
	for _, want := range []string{"No gateway model IV ticks landed", "865/865 priced legs", "865 prior option close", "non-voting"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingNamesAPIDiagnostic(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC), // 08:00 ET pre-market
		PricedLegCount: 751,
		LegCount:       2,
	}, "oi_missing")
	if got.Severity != "info" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY info warning outside RTH", got)
	}
	for _, want := range []string{"749 priced legs", "2 had positive OI", "generic tick 101", "regular U.S. option-data surface is closed", "sparse SPY OI is expected", "unknown, not zero", "09:30-16:15 ET"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingCountsPositiveOIAsObservedForLegacyCache(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC), // 08:00 ET pre-market
		PricedLegCount: 751,
		LegCount:       2,
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{
				PricedLegs:       751,
				OpenInterestLegs: 2,
			},
		},
	}, "oi_missing")
	if got.Severity != "info" || got.Scope != "SPY" {
		t.Fatalf("warning detail = %+v, want SPY info warning outside RTH", got)
	}
	for _, want := range []string{"749 priced legs", "2 legs had observed OI", "2 had positive OI"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingIsDataQualityForSPXOutsideRTH(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		AsOf:           time.Date(2026, time.June, 1, 22, 0, 0, 0, time.UTC), // 18:00 ET post-market
		PricedLegCount: 927,
		LegCount:       335,
	}, "oi_missing")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality warning outside RTH", got)
	}
	for _, want := range []string{"SPX option OI should normally be stable", "unknown, not zero", "data-farm health"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestGammaWarningDetailOIMissingIsDataQualityDuringRTH(t *testing.T) {
	t.Parallel()
	got := gammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		AsOf:           time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC), // 11:00 ET RTH
		PricedLegCount: 911,
		LegCount:       346,
	}, "oi_missing")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality warning during RTH", got)
	}
	for _, want := range []string{"565 priced legs", "regular U.S. option hours", "should normally be available", "data-farm health"} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}
