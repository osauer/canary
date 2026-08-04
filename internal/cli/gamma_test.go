package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaReadyFixture returns a realistic Ready-state envelope: combined
// SPY+SPX scope, agreement on long-γ, non-zero magnitude, populated
// citations + convention so default-mode and --explain assertions have
// the same payload to query.
func gammaReadyFixture() *rpc.GammaZeroSPXResult {
	now := time.Date(2026, 5, 23, 4, 25, 0, 0, time.UTC) // 06:25 CEST
	spy := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeSPY,
		SpotUnderlying:          743.73,
		SpotAt:                  now,
		GammaSign:               "positive",
		GammaTotalAbs:           1.8e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                1052,
		PricedLegCount:          1200,
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPY",
		AsOf:                    now,
		TopStrikes: []rpc.StrikeConcentration{{
			Underlying: "SPY",
			Expiry:     "2026-05-29",
			Strike:     740,
			Right:      "P",
			AbsGEX:     180_000_000,
			OI:         12_000,
		}},
	}
	spx := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeSPX,
		SpotUnderlying:          5430.0,
		GammaSign:               "positive",
		GammaTotalAbs:           4.2e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                2150,
		PricedLegCount:          2400,
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPX",
		AsOf:                    now,
		TopStrikes: []rpc.StrikeConcentration{{
			Underlying: "SPX",
			Expiry:     "2026-05-29",
			Strike:     5400,
			Right:      "C",
			AbsGEX:     2_200_000_000,
			OI:         8_000,
		}},
	}
	combined := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeCombined,
		GammaTotalAbs:           6.0e9,
		GammaTotalAbsConvention: "sign-agnostic",
		LegCount:                3202,
		PricedLegCount:          3600,
		Params:                  rpc.GammaZeroParams{StrikeWidthPct: 0.10},
		Expirations:             []string{"2026-05-26", "2026-05-29"},
		Method:                  "bs-gamma-profile-v3-stickymoneyness-0dte-split",
		Source:                  "SPY+SPX",
		AsOf:                    now,
		Quality: &rpc.GammaSignalQuality{
			Rankability:       rpc.GammaRankabilityRankable,
			RankabilityReason: "all rankability gates passed",
			Freshness:         "fresh",
			Session:           rpc.SessionRTH.String(),
			Gates: []rpc.GammaQualityGate{{
				Name:   "freshness",
				Status: rpc.GammaQualityGatePass,
				Reason: "same session and inside freshness TTL",
			}},
		},
		TopStrikes: []rpc.StrikeConcentration{
			{
				Underlying: "SPX",
				Expiry:     "2026-05-29",
				Strike:     5400,
				Right:      "C",
				AbsGEX:     2_200_000_000,
				OI:         8_000,
			},
			{
				Underlying: "SPY",
				Expiry:     "2026-05-29",
				Strike:     740,
				Right:      "P",
				AbsGEX:     180_000_000,
				OI:         12_000,
			},
		},
		PerIndex:        map[string]*rpc.GammaZeroComputed{"SPY": spy, "SPX": spx},
		RegimeAgreement: "agree:long-gamma",
		MethodologyCitations: []string{
			"Perfiliev (2022) — BS-sweep baseline",
			"Derman / Daglish-Hull-Suo — sticky-moneyness skew dynamics",
			"SqueezeMetrics (2017) — naive-sign GEX, deprecated 2022+",
			"Cboe 2025 — 0DTE = ~59% of SPX volume",
		},
	}
	return &rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: combined,
	}
}

func TestRenderGammaSkippedBannerUsesWarningDetailsAfterJSONRoundTrip(t *testing.T) {
	t.Parallel()
	wire := &rpc.GammaZeroComputed{
		WarningDetails: []rpc.GammaWarningDetail{
			{Code: "spx_unavailable:zero_magnitude", Scope: "SPX"},
		},
		Warnings: []string{"spx_unavailable:zero_magnitude"},
	}
	var payload []byte
	var err error
	if payload, err = json.Marshal(wire); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var roundTrip rpc.GammaZeroComputed
	if err := json.Unmarshal(payload, &roundTrip); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(roundTrip.Warnings) != 0 {
		t.Fatalf("warnings should be internal-only after JSON round trip: %v", roundTrip.Warnings)
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if !renderGammaSkippedBanner(env, &roundTrip) {
		t.Fatal("expected SPX skipped banner from warning_details")
	}
	if got := stdout.String(); !strings.Contains(got, "SPX skipped") ||
		!strings.Contains(got, "zero usable gamma magnitude") {
		t.Fatalf("banner did not explain SPX skip: %q", got)
	}
}

func TestRenderGammaSkippedBannerExplainsContextCanceledWithSessionContext(t *testing.T) {
	t.Parallel()
	wire := &rpc.GammaZeroComputed{
		Scope: rpc.GammaZeroScopeSPY,
		AsOf:  time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC), // Sunday 10:00 EDT, closed.
		WarningDetails: []rpc.GammaWarningDetail{
			{
				Code:    "spx_unavailable:context canceled",
				Scope:   "SPX",
				Message: "SPX option chain was skipped: context canceled.",
				Impact:  "Showing SPY only; SPX gamma is not included.",
				Action:  "Retry later or run --only=spy.",
			},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if !renderGammaSkippedBanner(env, wire) {
		t.Fatal("expected SPX skipped banner from warning_details")
	}
	out := stdout.String()
	for _, want := range []string{
		"SPX skipped",
		"fetch was canceled before usable data landed",
		"outside regular U.S. option hours",
		"not a confirmed root cause",
		"canary gamma --only=spy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("banner missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "context canceled") {
		t.Fatalf("banner should not leak raw context error:\n%s", out)
	}
}

func TestRenderGammaSkippedBannerFramesEntitlementAsNotAfterHours(t *testing.T) {
	t.Parallel()
	wire := &rpc.GammaZeroComputed{
		Scope: rpc.GammaZeroScopeSPY,
		WarningDetails: []rpc.GammaWarningDetail{
			{
				Code:    "spx_unavailable:354",
				Scope:   "SPX",
				Message: "SPX option chain was skipped: missing CBOE OPRA entitlement (IBKR 354).",
				Impact:  "Showing SPY only; SPX gamma is not included.",
				Action:  "Subscribe to the required market data or run --only=spy to suppress this banner.",
			},
		},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if !renderGammaSkippedBanner(env, wire) {
		t.Fatal("expected SPX skipped banner from warning_details")
	}
	out := stdout.String()
	for _, want := range []string{
		"missing CBOE OPRA entitlement",
		"not an after-hours",
		"condition",
		"canary gamma --only=spy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("entitlement banner missing %q:\n%s", want, out)
		}
	}
}

func TestFallbackGammaWarningDetailTreatsSPXOIMissingAsDataQualityOffHours(t *testing.T) {
	t.Parallel()
	got := fallbackGammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPX,
		AsOf:           time.Date(2026, time.June, 1, 22, 0, 0, 0, time.UTC),
		PricedLegCount: 927,
		LegCount:       335,
	}, "oi_missing")
	if got.Severity != "data_quality" || got.Scope != "SPX" {
		t.Fatalf("warning detail = %+v, want SPX data_quality", got)
	}
	for _, want := range []string{
		"Open-interest ticks were missing for 592 priced legs",
		"Missing OI is unknown, not zero",
		"SPX option OI should normally be stable",
	} {
		if !strings.Contains(got.Message+" "+got.Impact+" "+got.Action, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestFallbackGammaWarningDetailCountsPositiveOIAsObservedForLegacyCache(t *testing.T) {
	t.Parallel()
	got := fallbackGammaWarningDetail(&rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC),
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
	for _, want := range []string{"749 priced legs", "2 had observed OI", "2 had positive OI"} {
		if !strings.Contains(got.Message+" "+got.Impact, want) {
			t.Fatalf("warning detail missing %q: %+v", want, got)
		}
	}
}

func TestFallbackGammaWarningDetailUsesDaemonRTHSessionAt1610ET(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, time.July, 13, 20, 10, 0, 0, time.UTC) // Monday 16:10 EDT.
	if legacy := rpc.ClassifySession(asOf); legacy != rpc.SessionPost {
		t.Fatalf("test timestamp classified as %s, want legacy post-session disagreement", legacy)
	}
	got := fallbackGammaWarningDetail(&rpc.GammaZeroComputed{
		Scope: rpc.GammaZeroScopeSPY,
		AsOf:  asOf,
		Quality: &rpc.GammaSignalQuality{
			Session: rpc.SessionRTH.String(),
		},
	}, "oi_missing")

	if got.Severity != "data_quality" {
		t.Fatalf("warning detail = %+v, want data_quality from daemon RTH session", got)
	}
	if !strings.Contains(got.Action, "during regular U.S. option hours") {
		t.Fatalf("warning action did not use daemon RTH session: %+v", got)
	}
	if strings.Contains(got.Impact, "simplified local weekday/time fallback") {
		t.Fatalf("warning detail disclosed a fallback despite daemon session: %+v", got)
	}
}

func TestFallbackGammaWarningDetailUsesDaemonClosedSessionOnExchangeHoliday(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, time.July, 3, 18, 0, 0, 0, time.UTC) // Friday 14:00 EDT; Independence Day observed.
	if legacy := rpc.ClassifySession(asOf); legacy != rpc.SessionRTH {
		t.Fatalf("test timestamp classified as %s, want legacy RTH disagreement", legacy)
	}
	got := fallbackGammaWarningDetail(&rpc.GammaZeroComputed{
		Scope: rpc.GammaZeroScopeSPY,
		AsOf:  asOf,
		Quality: &rpc.GammaSignalQuality{
			Session: rpc.SessionClosed.String(),
		},
	}, "oi_missing")

	if got.Severity != "info" {
		t.Fatalf("warning detail = %+v, want info from daemon closed session", got)
	}
	if !strings.Contains(got.Action, "while the regular U.S. option-data surface is closed") {
		t.Fatalf("warning action did not use daemon closed session: %+v", got)
	}
	if strings.Contains(got.Action, "during regular U.S. option hours") {
		t.Fatalf("warning action over-flagged holiday as RTH: %+v", got)
	}
}

func TestFallbackGammaWarningDetailDisclosesEmptyDaemonSessionFallback(t *testing.T) {
	t.Parallel()
	got := fallbackGammaWarningDetail(&rpc.GammaZeroComputed{
		Scope: rpc.GammaZeroScopeSPY,
		AsOf:  time.Date(2026, time.July, 2, 19, 0, 0, 0, time.UTC), // Thursday 15:00 EDT.
		Quality: &rpc.GammaSignalQuality{
			Session: "",
		},
	}, "oi_missing")

	if got.Severity != "data_quality" {
		t.Fatalf("warning detail = %+v, want data_quality from timestamp fallback", got)
	}
	if !strings.Contains(got.Action, "during regular U.S. option hours") {
		t.Fatalf("warning action did not use timestamp fallback session: %+v", got)
	}
	if !strings.Contains(got.Impact, "Session context was missing from the daemon snapshot") ||
		!strings.Contains(got.Impact, "simplified local weekday/time fallback") ||
		!strings.Contains(got.Impact, "does not model exchange holidays or early closes") {
		t.Fatalf("warning detail did not disclose session fallback: %+v", got)
	}
}

// TestRenderGamma_HeroHasTitleTimestampAnchor pins the shared hero
// shape applied to gamma: a title on the same line as a timestamp
// (joined with "  ·  "), followed by an indented SPY spot anchor.
func TestRenderGamma_HeroHasTitleTimestampAnchor(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// Title + timestamp share one line.
	if !strings.Contains(out, "Dealer gamma · SPY+SPX") {
		t.Errorf("hero title missing:\n%s", out)
	}
	if !strings.Contains(out, "  ·  ") {
		t.Errorf("hero title/timestamp separator missing:\n%s", out)
	}
	// Anchor — SPY spot price, indented two spaces.
	if !strings.Contains(out, "  SPY 743.73") {
		t.Errorf("hero anchor with SPY spot missing:\n%s", out)
	}
}

func TestRenderGamma_HeroSummaryColorFollowsRegime(t *testing.T) {
	t.Parallel()

	t.Run("long gamma is green", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
			t.Fatalf("code=%d", code)
		}
		out := stdout.String()
		if !strings.Contains(out, ansiBold+ansiGreen+"long-γ (stabilizing regime)") {
			t.Fatalf("long-gamma hero summary should be bold green:\n%q", out)
		}
	})

	t.Run("short gamma is red", func(t *testing.T) {
		t.Parallel()
		fix := gammaReadyFixture()
		fix.Result.RegimeAgreement = "agree:short-gamma"
		for _, sub := range fix.Result.PerIndex {
			sub.GammaSign = "negative"
		}

		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
		if code := renderGammaText(env, fix, false); code != 0 {
			t.Fatalf("code=%d", code)
		}
		out := stdout.String()
		if !strings.Contains(out, ansiBold+ansiRed+"short-γ (amplifying regime)") {
			t.Fatalf("short-gamma hero summary should be bold red:\n%q", out)
		}
	})
}

// TestRenderGamma_DefaultOmitsMetadataBlock pins U1: the metadata block
// (Skew model, Method, Source, Compute, Derived IV, Leg count, Scope)
// no longer renders in default mode. Citations and the sign-convention
// disclosure are also gated behind --explain.
func TestRenderGamma_DefaultOmitsMetadataBlock(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, banned := range []string{
		"Method      bs-gamma",
		"Source      ",
		"Leg count   ",
		"Skew model",
		"Citations",
		"Perfiliev (2022)",
		"Disclosure:",
		"Rankability",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render must not surface %q (--explain only):\n%s", banned, out)
		}
	}
}

func TestRenderGamma_DefaultUsesPlainSignalReadout(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.Summary = &rpc.GammaZeroSummary{
		PrimaryStatement: "Zero-gamma: SPY none in $645.59-$878.11 (long-gamma). No combined zero is computed across SPY/SPX price scales.",
	}
	fix.Result.Quality = &rpc.GammaSignalQuality{
		Rankability:       rpc.GammaRankabilityContextOnly,
		RankabilityReason: "freshness: market is closed; cached gamma is context only",
		Freshness:         "closed_session_context",
		Session:           rpc.SessionClosed.String(),
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Signal     after-hours context · cached snapshot is not a fresh market-structure read",
		"long-γ (stabilizing regime)",
		"no γ-zero transition found in sweep",
		"SPY/SPX agree",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{
		"Rankability context_only",
		"freshness:",
		"No combined zero",
		"SPY none in",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render leaked raw diagnostic phrase %q:\n%s", banned, out)
		}
	}
}

func TestRenderGamma_DefaultFiltersDiagnosticDataNotes(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.PerIndex["SPX"].WarningDetails = []rpc.GammaWarningDetail{
		{
			Code:    "oi_missing",
			Scope:   "SPX",
			Message: "Open-interest ticks were missing for 592 priced legs.",
			Impact:  "Missing OI is unknown, not zero.",
			Action:  "Check TWS before trusting magnitude.",
		},
		{
			Code:    "strike_budget_capped",
			Scope:   "SPX",
			Message: "The strike fan-out was capped to the nearest 80 listed strikes per expiry.",
			Impact:  "Farther strikes were skipped.",
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, banned := range []string{
		"Open-interest ticks",
		"nearest 80",
		"Data notes:",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default render should hide diagnostic note %q:\n%s", banned, out)
		}
	}

	stdout.Reset()
	if code := renderGammaText(env, fix, true); code != 0 {
		t.Fatalf("explain code=%d", code)
	}
	explain := stdout.String()
	for _, want := range []string{
		"Open-interest ticks were missing",
		"Action: Check TWS",
	} {
		if !strings.Contains(explain, want) {
			t.Errorf("--explain should retain diagnostic note %q:\n%s", want, explain)
		}
	}
	if strings.Contains(explain, "nearest 80") {
		t.Errorf("--explain should hide low-level strike cap diagnostics:\n%s", explain)
	}

	stdout.Reset()
	if code := renderGammaTextWithOptions(env, fix, gammaRenderOptions{Explain: true, Diagnostics: true}); code != 0 {
		t.Fatalf("diagnostics code=%d", code)
	}
	diagnostics := stdout.String()
	for _, want := range []string{
		"Open-interest ticks were missing",
		"nearest 80",
		"Action: Check TWS",
	} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("--diagnostics should retain diagnostic note %q:\n%s", want, diagnostics)
		}
	}
}

func TestRenderGamma_DefaultSurfacesAllDerivedIVDataQuality(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.PerIndex["SPX"].DerivedIVLegs = 865
	fix.Result.PerIndex["SPX"].PricedLegCount = 865
	fix.Result.PerIndex["SPX"].DerivedPrevCloseLegs = 865
	fix.Result.PerIndex["SPX"].WarningDetails = []rpc.GammaWarningDetail{{
		Code:     "all_iv_derived",
		Scope:    "SPX",
		Severity: "data_quality",
		Message:  "No gateway model IV ticks landed; all implied volatilities were back-solved.",
		Impact:   "The result is more model-dependent: 865/865 priced legs used quote/close inversion (865 prior option close) instead of IBKR model-computation ticks.",
	}}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Context:",
		"No gateway model IV ticks landed",
		"865/865 priced legs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGamma_DefaultRendersCacheFallbackAsSourceNote(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.WarningDetails = []rpc.GammaWarningDetail{{
		Code:    "spx_cache_fallback:timeout",
		Scope:   "SPX",
		Message: "SPX live refresh timed out; using the last successful cached SPX slice.",
		Impact:  "SPX is included from cache; quality.rankability shows whether the gamma read is fresh and covered enough to act as a market-structure signal.",
		Action:  "Refresh during 09:30-16:15 ET.",
	}}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Source note:",
		"using cached SPX slice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default cache fallback render missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{
		"Data notes:",
		"live refresh",
		"quality.rankability",
		"not fresh confirmation",
		"treat the combined gamma regime as degraded",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default cache fallback render leaked %q:\n%s", banned, out)
		}
	}

	stdout.Reset()
	if code := renderGammaText(env, fix, true); code != 0 {
		t.Fatalf("explain code=%d", code)
	}
	explain := stdout.String()
	for _, want := range []string{
		"Data notes:",
		"using cached SPX slice",
	} {
		if !strings.Contains(explain, want) {
			t.Errorf("--explain cache fallback render missing %q:\n%s", want, explain)
		}
	}
	for _, banned := range []string{
		"live refresh",
		"quality.rankability",
		"not fresh confirmation",
	} {
		if strings.Contains(explain, banned) {
			t.Errorf("--explain cache fallback render leaked %q:\n%s", banned, explain)
		}
	}
}

func TestGammaSPXCacheFallbackContextLineNamesMarketPhase(t *testing.T) {
	t.Parallel()
	got := gammaSPXCacheFallbackContextLine(time.Date(2026, 6, 3, 2, 30, 0, 0, time.FixedZone("EDT", -4*60*60)))
	for _, want := range []string{
		"overnight (options closed)",
		"using cached SPX slice",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("cache fallback context missing %q: %q", want, got)
		}
	}
}

// TestRenderGamma_DefaultShowsMagnitudeCoPrimary pins U1.5: magnitude
// is promoted to a peer line with the convention label drawn from the
// wire's GammaTotalAbsConvention field.
func TestRenderGamma_DefaultShowsMagnitudeCoPrimary(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "Magnitude") {
		t.Errorf("default render should surface a Magnitude line:\n%s", out)
	}
	if !strings.Contains(out, "$6.00B per 1% move") {
		t.Errorf("Magnitude line should carry formatted total + per-move suffix:\n%s", out)
	}
	if !strings.Contains(out, "(sign-agnostic)") {
		t.Errorf("Magnitude line should label convention from wire:\n%s", out)
	}
}

func TestRenderGamma_DefaultShowsCanonicalSPXTopStrikes(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Top SPX strikes by |GEX| (canonical concentration):",
		"SPY context strikes are available with --explain or `canary gamma --only=spy`.",
		"DTE",
		"SPOT",
		"2026-05-29   6   5400  -0.6%   C",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render missing SPX top-strike marker %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{
		"INDEX",
		"2026-05-29   6    740  -0.5%   P",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("default combined render should not show %q:\n%s", banned, out)
		}
	}
}

func TestFormatGammaStrikeSpotDelta(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		strike float64
		spot   float64
		want   string
	}{
		{"below", 5400, 5430, "-0.6%"},
		{"above", 5500, 5430, "+1.3%"},
		{"atm", 5430.5, 5430, "ATM"},
		{"unknown", 5400, 0, "—"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatGammaStrikeSpotDelta(tc.strike, tc.spot); got != tc.want {
				t.Fatalf("formatGammaStrikeSpotDelta(%v, %v) = %q, want %q", tc.strike, tc.spot, got, tc.want)
			}
		})
	}
}

func TestFormatGammaStrikeDTEUsesSnapshotDate(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 6, 2, 21, 20, 0, 0, time.FixedZone("CEST", 2*60*60))
	cases := []struct {
		name   string
		expiry string
		asOf   time.Time
		want   string
	}{
		{"same_day", "2026-06-02", asOf, "0"},
		{"next_day", "2026-06-03", asOf, "1"},
		{"six_days", "2026-06-08", asOf, "6"},
		{"expired", "2026-06-01", asOf, "exp"},
		{"unknown", "2026-06-02", time.Time{}, "—"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatGammaStrikeDTE(tc.expiry, tc.asOf); got != tc.want {
				t.Fatalf("formatGammaStrikeDTE(%q, %v) = %q, want %q", tc.expiry, tc.asOf, got, tc.want)
			}
		})
	}
}

func TestRenderGamma_HighlightsTopStrikeConcentration(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, ansiBold+"    2026-05-29") {
		t.Fatalf("top strike row should be bold-highlighted:\n%q", out)
	}
}

// TestRenderGamma_MagnitudeOmittedWhenZero pins the conditional render:
// a Result with GammaTotalAbs=0 (no-aggregator-data case) skips the
// Magnitude line entirely rather than printing "$0.00 per 1% move".
func TestRenderGamma_MagnitudeOmittedWhenZero(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.GammaTotalAbs = 0
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "Magnitude") {
		t.Errorf("zero magnitude should omit the Magnitude line:\n%s", out)
	}
}

// TestRenderGamma_ColdRendersFriendlyExplainer pins the cold-state UX:
// when the daemon returns Status=cold (e.g. off-hours with no v3 cache
// yet), the renderer prints a clear explainer instead of the harsh
// "without a result payload" defensive fallback. Includes the hint to
// pass --force.
func TestRenderGamma_ColdRendersFriendlyExplainer(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("cold should exit 0, got %d", code)
	}
	out := stdout.String()
	if strings.Contains(out, "without a result payload") {
		t.Errorf("cold path should not emit the defensive fallback:\n%s", out)
	}
	for _, want := range []string{
		"no data yet",
		"cold cache",
		"normally prewarms gamma after gateway startup",
		"15-minute soft TTL",
		"automatic refresh is not due",
		"09:30-16:15 ET",
		"--force",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cold explainer missing %q in:\n%s", want, out)
		}
	}
	for _, stale := range []string{"first call", "once per NY trading session"} {
		if strings.Contains(out, stale) {
			t.Errorf("cold explainer retained stale cadence claim %q:\n%s", stale, out)
		}
	}
}

func TestRenderGamma_ComputingExplainsPrewarmAndIntradayRefresh(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusComputing, Progress: 25, EtaSeconds: 90}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("computing should exit 0, got %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"prewarms gamma after gateway startup", "15-minute soft TTL", "off-hours automatic refresh is not due"} {
		if !strings.Contains(out, want) {
			t.Errorf("computing explainer missing %q:\n%s", want, out)
		}
	}
	for _, stale := range []string{"once per NY trading session", "subsequent calls within the day"} {
		if strings.Contains(out, stale) {
			t.Errorf("computing explainer retained stale cadence claim %q:\n%s", stale, out)
		}
	}
}

func TestGammaCommandCatalogDescribesRuntimeRefreshCadence(t *testing.T) {
	t.Parallel()
	cmd, ok := lookupCommand("gamma")
	if !ok {
		t.Fatal("gamma command missing from catalog")
	}
	for _, want := range []string{"daemon-prewarmed", "15m in RTH", "off-hours refresh not due"} {
		if !strings.Contains(cmd.Summary, want) {
			t.Errorf("gamma catalog missing %q: %s", want, cmd.Summary)
		}
	}
	if strings.Contains(cmd.Summary, "once per NY trading day") {
		t.Fatalf("gamma catalog retained stale once-daily claim: %s", cmd.Summary)
	}
}

func TestRenderGamma_ColdRendersDaemonReason(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{
		Status:     rpc.GammaZeroStatusCold,
		ColdReason: "persisted gamma cache for spy+spx was rejected: per_index[SPX]: zero-gamma invalid result",
		ColdAction: "Run `canary gamma --force` for a diagnostic off-hours recompute.",
	}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("cold should exit 0, got %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Reason      persisted gamma cache",
		"per_index[SPX]",
		"diagnostic off-hours recompute",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cold reason render missing %q in:\n%s", want, out)
		}
	}
}

// Off-hours the daemon retains a failed attempt without retrying it, so the
// reason line can be hours old and reads as a live fault without the
// retention context (github.com/osauer/canary#26).
func TestRenderGamma_ErrorRendersRetentionContext(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.GammaZeroSPXResult{
		Status:     rpc.GammaZeroStatusError,
		Error:      "zero-gamma: SPY phase: zero-gamma: no SPY spot available (gateway returned no live tick)",
		ColdReason: "the last gamma attempt failed at 2026-05-22 15:42 EDT and the regular U.S. options session is closed, so no automatic retry runs until it reopens",
		ColdAction: "Run `canary gamma --force` for a diagnostic off-hours recompute.",
	}
	if code := renderGammaText(env, res, false); code != 1 {
		t.Fatalf("error should exit 1, got %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Status      error",
		"Reason      zero-gamma: SPY phase",
		"Retained    the last gamma attempt failed at 2026-05-22 15:42 EDT",
		"diagnostic off-hours recompute",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("error render missing %q in:\n%s", want, out)
		}
	}
}

// TestRenderGamma_ExplainIsConcise pins the default --explain contract:
// interpretation and methodology stay visible, while source diagnostics,
// citations, and raw gate dumps move behind --diagnostics.
func TestRenderGamma_ExplainIsConcise(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"How to read",
		"Gamma is how fast option delta changes",
		"Per-bucket γ-zero",
		"Method      SPY + SPX · ±10% sweep · 80 strikes/expiry · 2 expirations · 3202 GEX legs",
		"Quality     rankable · fresh · RTH",
		"Scale",
		"Disclosure",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q:\n%s", want, out)
		}
	}
	for _, banned := range []string{
		"Citations",
		"Perfiliev (2022)",
		"Source diagnostics",
		"Signal quality",
		"Rankability rankable",
		"Gates",
		"Source      SPY+SPX",
		"freshness: pass",
	} {
		if strings.Contains(out, banned) {
			t.Errorf("--explain output leaked diagnostic phrase %q:\n%s", banned, out)
		}
	}
}

func TestRenderGamma_ExplainShowsCombinedTopStrikesWithIndex(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Top strikes by |GEX| (SPY+SPX diagnostic):",
		"IDX",
		"DTE",
		"SPOT",
		"SPX  2026-05-29   6   5400  -0.6%   C",
		"SPY  2026-05-29   6    740  -0.5%   P",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain combined top strikes missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGamma_SPXUnavailableLabelsSPYProxyStrikes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 24, 14, 0, 0, 0, time.UTC)
	res := &rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:          rpc.GammaZeroScopeSPY,
			SpotUnderlying: 743.73,
			GammaSign:      "positive",
			GammaTotalAbs:  1.8e9,
			LegCount:       1052,
			AsOf:           now,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityContextOnly,
				RankabilityReason: "spx_coverage: SPX option chain unavailable; using SPY proxy: timeout",
			},
			TopStrikes: []rpc.StrikeConcentration{{
				Underlying: "SPY",
				Expiry:     "2026-05-29",
				Strike:     740,
				Right:      "P",
				AbsGEX:     180_000_000,
				OI:         12_000,
			}},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code:    "spx_unavailable:timeout",
				Scope:   "SPX",
				Message: "SPX option-chain fetch timed out before usable data landed.",
				Impact:  "Showing SPY only; SPX gamma is not included.",
				Action:  "Retry during 09:30-16:15 ET or run --only=spy.",
			}},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, res, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"SPX skipped",
		"Signal     SPY proxy only",
		"Top SPY proxy strikes by |GEX|:",
		"SPX is unavailable; treat this as proxy context, not canonical S&P dealer gamma.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("SPY proxy render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderGamma_DiagnosticsSurfacesSourceDiagnostics(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.CollectionDiagnostics = []rpc.GammaCollectionDiagnostic{{
		Underlying:             "SPY",
		TradingClass:           "SPY",
		Expiry:                 "2026-06-05",
		QualifiedContracts:     160,
		RequestedLegs:          80,
		PricedLegs:             75,
		MarketDataGenericTicks: "100,101,104,106",
		OIGenericTickRequested: true,
		OILiveObservedLegs:     0,
		OICarriedForwardLegs:   0,
		OIPositiveLegs:         0,
		OIMissingLegs:          75,
		Timeouts:               2,
		EntitlementErrors:      1,
		StrikeCandidates:       100,
		StrikeSelected:         80,
		StrikeCap:              80,
		StrikeCapTruncated:     true,
		ExpiryCapTruncated:     true,
		CollectionDurationMS:   2100,
		OISourceStatus:         "missing",
	}}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(stdout.String(), "Source diagnostics") {
		t.Fatalf("--explain should not show source diagnostics by default:\n%s", stdout.String())
	}

	stdout.Reset()
	if code := renderGammaTextWithOptions(env, fix, gammaRenderOptions{Explain: true, Diagnostics: true}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Source diagnostics",
		"SPY/SPY 2026-06-05",
		"q160 req80 priced75",
		"OI live0 carry0 pos0 miss75",
		"tick101 yes",
		"ticks 100,101,104,106",
		"missing",
		"failures timeout=2 · entitlement=1",
		"caps     strikes 80/100 cap 80 truncated · expiry cap truncated",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain source diagnostics missing %q:\n%s", want, out)
		}
	}
}

// TestRenderGamma_DefaultShowsPerIndexCompact pins U1.4: combined-mode
// default render surfaces one compact line per index ("SPY  no crossing
// · long-γ · 1052 legs") instead of the old multi-line Per-index block.
func TestRenderGamma_DefaultShowsPerIndexCompact(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"SPY        no crossing · long-γ · 1052 GEX legs",
		"SPX        no crossing · long-γ · 2150 GEX legs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("per-index compact line missing %q:\n%s", want, out)
		}
	}
}

// TestRenderGamma_ConvenienceConventionFallback pins the legacy-daemon
// case: when GammaTotalAbsConvention is empty (older daemon), the
// renderer falls back to "sign-agnostic" rather than emitting a stray
// "()" parenthetical.
func TestRenderGamma_ConvenienceConventionFallback(t *testing.T) {
	t.Parallel()
	fix := gammaReadyFixture()
	fix.Result.GammaTotalAbsConvention = ""
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, fix, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "(sign-agnostic)") {
		t.Errorf("missing-convention fallback should be 'sign-agnostic':\n%s", out)
	}
}

// TestGammaHeroSummary_SingleAndCombined pins the hero summary line
// across the three Scope shapes: combined uses formatRegimeAgreement;
// SPY/SPX-only produce a compact single-index regime statement.
func TestGammaHeroSummary_SingleAndCombined(t *testing.T) {
	t.Parallel()
	mk := func(scope, sign string, zg *float64) *rpc.GammaZeroSPXResult {
		return &rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				Scope:           scope,
				SpotUnderlying:  743.73,
				GammaSign:       sign,
				ZeroGamma:       zg,
				RegimeAgreement: "agree:long-gamma",
				PerIndex: map[string]*rpc.GammaZeroComputed{
					"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive"},
					"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "positive"},
				},
			},
		}
	}
	cases := []struct {
		name  string
		input *rpc.GammaZeroSPXResult
		want  string
	}{
		{"combined", mk(rpc.GammaZeroScopeCombined, "positive", nil), "long-γ (stabilizing regime)"},
		{"spy_only_long", mk(rpc.GammaZeroScopeSPY, "positive", nil), "SPY long-γ"},
		{"spx_only_short", mk(rpc.GammaZeroScopeSPX, "negative", nil), "SPX short-γ"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gammaHeroSummary(tc.input)
			if !strings.Contains(got, tc.want) {
				t.Errorf("summary=%q want substring %q", got, tc.want)
			}
		})
	}
}

// TestRenderGamma_ExplainCarriesScalingCaveat pins C4: the
// --explain block always carries a short scaling caveat naming the
// S² scaling and states that zero-gamma levels stay per-index rather
// than pretending the combined envelope has one price scale.
func TestRenderGamma_ExplainCarriesScalingCaveat(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderGammaText(env, gammaReadyFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Scale",
		"via S²",
		"combined |Γ|·OI sums SPY/SPX books",
		"zero-gamma levels stay per-index",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain missing scaling-caveat marker %q:\n%s", want, out)
		}
	}
}
