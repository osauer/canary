package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestClassifyRegimeAgreement pins the four classifier outcomes plus
// the no-data fallback. Replaces the earlier 20-day price-correlation
// gate — that gate fired ~never because SPY/SPX prices stay > 0.99
// correlated essentially always, and missed the actual case worth
// flagging (gamma regimes that decouple while prices stay tightly
// correlated, which IS the actionable signal).
func TestClassifyRegimeAgreement(t *testing.T) {
	cases := []struct {
		name string
		spy  *rpc.GammaZeroComputed
		spx  *rpc.GammaZeroComputed
		want string
	}{
		{
			name: "both_short_gamma",
			spy:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "agree:short-gamma",
		},
		{
			name: "both_long_gamma",
			spy:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			want: "agree:long-gamma",
		},
		{
			name: "both_transition",
			spy:  &rpc.GammaZeroComputed{ZeroGamma: new(545.0), GapPct: new(0.5)},
			spx:  &rpc.GammaZeroComputed{ZeroGamma: new(5450.0), GapPct: new(-0.5)},
			want: "agree:transition-gamma",
		},
		{
			name: "crossing_above_zero_is_long_gamma",
			spy:  &rpc.GammaZeroComputed{ZeroGamma: new(545.0), GapPct: new(3.0)},
			spx:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			want: "agree:long-gamma",
		},
		{
			name: "disagree_long_vs_short",
			spy:  &rpc.GammaZeroComputed{GammaSign: "positive"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "disagree",
		},
		{
			name: "disagree_transition_vs_short",
			spy:  &rpc.GammaZeroComputed{ZeroGamma: new(545.0), GapPct: new(0.5)},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "disagree",
		},
		{
			name: "no_data_spy",
			spy:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "",
		},
		{
			name: "both_no_data",
			spy:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			spx:  &rpc.GammaZeroComputed{GammaSign: "no_data"},
			want: "",
		},
		{
			name: "nil_one_side",
			spy:  nil,
			spx:  &rpc.GammaZeroComputed{GammaSign: "negative"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRegimeAgreement(tc.spy, tc.spx); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCombineGammaResultsMergesTopStrikes — SPX rows dominate by raw
// dollar gamma (the 100× per-contract scaling makes this structural).
// The merge produces a single sorted list; INDEX column on the renderer
// makes the imbalance visible rather than hidden.
func TestCombineGammaResultsMergesTopStrikes(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 540,
		GammaTotalAbs:  5e9,
		GammaSign:      "negative",
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{
				PricedLegs:        100,
				OpenInterestLegs:  80,
				GammaPositiveLegs: 100,
				AbsGEXLegs:        80,
			},
			ByUnderlying: map[string]rpc.GammaLegDiagnosticCounts{
				"SPY": {PricedLegs: 100, OpenInterestLegs: 80, GammaPositiveLegs: 100, AbsGEXLegs: 80},
			},
			ByTradingClass: map[string]rpc.GammaLegDiagnosticCounts{
				"SPY": {PricedLegs: 100, OpenInterestLegs: 80, GammaPositiveLegs: 100, AbsGEXLegs: 80},
			},
		},
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPY", Strike: 540, Right: "C", AbsGEX: 8e8, Expiry: "2026-06-19"},
			{Underlying: "SPY", Strike: 540, Right: "P", AbsGEX: 6e8, Expiry: "2026-06-19"},
		},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 5400,
		GammaTotalAbs:  18e9,
		GammaSign:      "negative",
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{
				PricedLegs:        200,
				OpenInterestLegs:  150,
				GammaPositiveLegs: 200,
				AbsGEXLegs:        150,
			},
			ByUnderlying: map[string]rpc.GammaLegDiagnosticCounts{
				"SPX": {PricedLegs: 200, OpenInterestLegs: 150, GammaPositiveLegs: 200, AbsGEXLegs: 150},
			},
			ByTradingClass: map[string]rpc.GammaLegDiagnosticCounts{
				"SPX":  {PricedLegs: 40, OpenInterestLegs: 30, GammaPositiveLegs: 40, AbsGEXLegs: 30},
				"SPXW": {PricedLegs: 160, OpenInterestLegs: 120, GammaPositiveLegs: 160, AbsGEXLegs: 120},
			},
		},
		TopStrikes: []rpc.StrikeConcentration{
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5400, Right: "C", AbsGEX: 7e9, Expiry: "2026-06-19"},
			{Underlying: "SPX", TradingClass: "SPXW", Strike: 5300, Right: "P", AbsGEX: 5e9, Expiry: "2026-06-19"},
		},
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if combined.Scope != rpc.GammaZeroScopeCombined {
		t.Errorf("Scope = %q, want %q", combined.Scope, rpc.GammaZeroScopeCombined)
	}
	if combined.SpotUnderlying != 0 || combined.ZeroGamma != nil || combined.GammaSign != "" {
		t.Errorf("combined envelope must not expose top-level spot/zero/sign: spot=%v zero=%v sign=%q",
			combined.SpotUnderlying, combined.ZeroGamma, combined.GammaSign)
	}
	if combined.PerIndex["SPY"] != spy || combined.PerIndex["SPX"] != spx {
		t.Errorf("PerIndex pointers don't match the inputs")
	}
	if combined.GammaTotalAbs != 23e9 {
		t.Errorf("combined GammaTotalAbs = %v, want 23e9", combined.GammaTotalAbs)
	}
	wantDiagnostics := rpc.GammaLegDiagnosticCounts{
		PricedLegs:        300,
		OpenInterestLegs:  230,
		GammaPositiveLegs: 300,
		AbsGEXLegs:        230,
	}
	if combined.LegDiagnostics == nil {
		t.Fatal("combined LegDiagnostics is nil")
	}
	if combined.LegDiagnostics.Total != wantDiagnostics {
		t.Errorf("combined LegDiagnostics.Total = %+v, want %+v", combined.LegDiagnostics.Total, wantDiagnostics)
	}
	if got := combined.LegDiagnostics.ByUnderlying["SPY"].PricedLegs; got != 100 {
		t.Errorf("combined SPY priced legs = %d, want 100", got)
	}
	if got := combined.LegDiagnostics.ByUnderlying["SPX"].PricedLegs; got != 200 {
		t.Errorf("combined SPX priced legs = %d, want 200", got)
	}
	if got := combined.LegDiagnostics.ByTradingClass["SPXW"].AbsGEXLegs; got != 120 {
		t.Errorf("combined SPXW GEX legs = %d, want 120", got)
	}
	// Regime classifier fires on the inputs (both short-γ → agree).
	if combined.RegimeAgreement != "agree:short-gamma" {
		t.Errorf("RegimeAgreement = %q, want agree:short-gamma", combined.RegimeAgreement)
	}
	if len(combined.TopStrikes) != 4 {
		t.Fatalf("merged top strikes len = %d, want 4: %+v", len(combined.TopStrikes), combined.TopStrikes)
	}
	wantOrder := []struct {
		underlying string
		absGEX     float64
	}{
		{"SPX", 7e9},
		{"SPX", 5e9},
		{"SPY", 8e8},
		{"SPY", 6e8},
	}
	for i, w := range wantOrder {
		if combined.TopStrikes[i].Underlying != w.underlying || combined.TopStrikes[i].AbsGEX != w.absGEX {
			t.Errorf("top[%d] = %+v, want underlying=%s absGEX=%v",
				i, combined.TopStrikes[i], w.underlying, w.absGEX)
		}
	}
}

func TestCombineGammaResultsUsesOldestPerIndexAsOf(t *testing.T) {
	t.Parallel()

	spyAsOf := time.Date(2026, 6, 1, 12, 58, 0, 0, time.UTC)
	spxAsOf := time.Date(2026, 6, 1, 20, 35, 0, 0, time.UTC)
	spy := &rpc.GammaZeroComputed{GammaSign: "negative", AsOf: spyAsOf}
	spx := &rpc.GammaZeroComputed{GammaSign: "negative", AsOf: spxAsOf}

	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if !combined.AsOf.Equal(spyAsOf) {
		t.Fatalf("combined AsOf = %v, want oldest per-index timestamp %v", combined.AsOf, spyAsOf)
	}
}

// TestCombineGammaResultsDisagreementSurfaces pins the actionable
// case — SPY long-γ + SPX short-γ — produces RegimeAgreement="disagree"
// so the renderer can flag the institutional/retail divergence.
func TestCombineGammaResultsDisagreementSurfaces(t *testing.T) {
	spy := &rpc.GammaZeroComputed{SpotUnderlying: 540, GammaSign: "positive"}
	spx := &rpc.GammaZeroComputed{SpotUnderlying: 5400, GammaSign: "negative"}
	combined := combineGammaResults(spy, spx)
	if combined.RegimeAgreement != "disagree" {
		t.Errorf("RegimeAgreement = %q, want disagree", combined.RegimeAgreement)
	}
}

// TestGammaScopeForRequestDefaultsToCombined pins that an empty Scope
// (today's `canary gamma` no flag) lands on the combined path.
func TestGammaScopeForRequestDefaultsToCombined(t *testing.T) {
	got, err := gammaScopeForRequest("")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != rpc.GammaZeroScopeCombined {
		t.Errorf("empty scope: got %q, want %q", got, rpc.GammaZeroScopeCombined)
	}
}

func TestSummarizeSPXFailureClassifiesCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"context_canceled", context.Canceled, "fetch_canceled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeSPXFailure(tc.err); got != tc.want {
				t.Fatalf("summarizeSPXFailure(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestCombineGammaResultsSumsProfileOnSharedGrid covers the happy
// path of combineProfileBuckets: when both halves share an identical
// Spot grid (which is contrived for SPY+SPX but the helper's contract
// nonetheless), the combined Profile carries the bucket-wise GEX sum.
func TestCombineGammaResultsSumsProfileOnSharedGrid(t *testing.T) {
	// Contrived shared grid (real SPY/SPX never align this way —
	// SPY anchors ~540, SPX anchors ~5400 — so the mismatch path
	// is the production case; see TestCombineGammaResultsProfileGridMismatch).
	spy := &rpc.GammaZeroComputed{
		SpotUnderlying: 110,
		ZeroGamma:      new(108.0),
		GammaSign:      "negative",
		Profile: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: 10},
			{Spot: 110, GEX: 20},
			{Spot: 120, GEX: 30},
		},
	}
	spx := &rpc.GammaZeroComputed{
		SpotUnderlying: 110,
		Profile: []rpc.GammaProfilePoint{
			{Spot: 100, GEX: -5},
			{Spot: 110, GEX: 100},
			{Spot: 120, GEX: -7},
		},
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	wantProfile := []rpc.GammaProfilePoint{
		{Spot: 100, GEX: 5},
		{Spot: 110, GEX: 120},
		{Spot: 120, GEX: 23},
	}
	if len(combined.Profile) != len(wantProfile) {
		t.Fatalf("combined.Profile len = %d, want %d (Profile=%+v)", len(combined.Profile), len(wantProfile), combined.Profile)
	}
	for i, w := range wantProfile {
		if combined.Profile[i] != w {
			t.Errorf("combined.Profile[%d] = %+v, want %+v", i, combined.Profile[i], w)
		}
	}
	if combined.SpotUnderlying != 0 || combined.ZeroGamma != nil || combined.GammaSign != "" {
		t.Errorf("combined envelope must not shallow-copy SPY fields: spot=%v zero=%v sign=%q",
			combined.SpotUnderlying, combined.ZeroGamma, combined.GammaSign)
	}
	if len(combined.Warnings) != 0 {
		t.Errorf("combined warnings should stay empty; got %v", combined.Warnings)
	}
}

// TestCombineGammaResultsProfileGridMismatch covers the normal SPY+SPX
// case: profiles sit on different spot scales, so the combined envelope
// omits a top-level profile and leaves charting to per_index.
func TestCombineGammaResultsProfileGridMismatch(t *testing.T) {
	spy := &rpc.GammaZeroComputed{
		Profile: []rpc.GammaProfilePoint{
			{Spot: 540, GEX: 100},
			{Spot: 550, GEX: -100},
		},
	}
	spx := &rpc.GammaZeroComputed{
		Profile: []rpc.GammaProfilePoint{
			{Spot: 5400, GEX: 1000},
			{Spot: 5500, GEX: -1000},
		},
	}
	combined := combineGammaResults(spy, spx)
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if combined.Profile != nil {
		t.Errorf("grid mismatch: combined.Profile should be nil, got %+v", combined.Profile)
	}
	if len(combined.Warnings) != 0 {
		t.Errorf("grid mismatch should not create top-level warnings: %v", combined.Warnings)
	}
}

// TestCombineGammaResultsUnionsScopedWarnings proves the combined wire
// envelope carries the warning union without collapsing the affected index.
// The code-only daemon surface dedupes by code; structured wire details dedupe
// by (scope, code), so the same condition on both books remains explicit.
func TestCombineGammaResultsUnionsScopedWarnings(t *testing.T) {
	now := time.Date(2026, 6, 2, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now.Add(-5*time.Minute))
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now.Add(-5*time.Minute))
	spy.Warnings = []string{"oi_missing", "strike_budget_capped", "oi_missing"}
	spx.Warnings = []string{"strike_budget_capped", "oi_missing", "strike_budget_capped"}

	combined := hydrateGammaComputed(combineGammaResults(spy, spx))
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if got, want := combined.Warnings, []string{"oi_missing", "strike_budget_capped"}; !equalStringSlices(got, want) {
		t.Fatalf("combined warning codes = %v, want %v", got, want)
	}
	if len(combined.PerIndex["SPY"].WarningDetails) != 2 || len(combined.PerIndex["SPX"].WarningDetails) != 2 {
		t.Fatalf("per-index warnings were not preserved: spy=%v spx=%v",
			combined.PerIndex["SPY"].WarningDetails, combined.PerIndex["SPX"].WarningDetails)
	}

	wantDetails := map[string]bool{
		"SPY/oi_missing":           false,
		"SPY/strike_budget_capped": false,
		"SPX/oi_missing":           false,
		"SPX/strike_budget_capped": false,
	}
	for _, d := range combined.WarningDetails {
		key := d.Scope + "/" + d.Code
		seen, ok := wantDetails[key]
		if !ok {
			t.Fatalf("unexpected combined warning detail %+v", d)
		}
		if seen {
			t.Fatalf("duplicate combined warning detail for %s: %+v", key, combined.WarningDetails)
		}
		wantDetails[key] = true
	}
	if len(combined.WarningDetails) != len(wantDetails) {
		t.Fatalf("combined warning details = %+v, want one detail per scoped code", combined.WarningDetails)
	}
	for key, seen := range wantDetails {
		if !seen {
			t.Errorf("combined warning details missing %s: %+v", key, combined.WarningDetails)
		}
	}

	// Hydration happens on cache loads as well as fresh computes. Repeating it
	// must not duplicate the projected warning details.
	hydrateGammaComputed(combined)
	if len(combined.WarningDetails) != len(wantDetails) {
		t.Fatalf("repeated hydration duplicated warning details: %+v", combined.WarningDetails)
	}

	annotateGammaQuality(combined, now)
	if got := combined.Quality.Rankability; got != rpc.GammaRankabilityRankable {
		t.Fatalf("combined rankability = %q, want rankable SPX-canonical result after warning projection: %+v", got, combined.Quality)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGammaOneSidedFallbackUsableForCombinedRejectsBlockedQuality(t *testing.T) {
	now := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	result := gammaCombineFallbackFixture(now.Add(-5*time.Minute), 1, 1)

	ok, reason := gammaOneSidedFallbackUsableForCombined(result, now)
	if ok {
		t.Fatal("one-leg fallback should not be usable for a combined ready result")
	}
	if reason == "" {
		t.Fatal("blocked fallback should include the quality reason")
	}
}

func TestGammaOneSidedFallbackUsableForCombinedAcceptsContextOnly(t *testing.T) {
	now := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	result := gammaCombineFallbackFixture(now.Add(-5*time.Minute), gammaMinPricedLegs, gammaMinGEXLegs)

	ok, reason := gammaOneSidedFallbackUsableForCombined(result, now)
	if !ok {
		t.Fatalf("context-only fallback should remain usable, reason=%q", reason)
	}
}

func TestSummarizeGammaPhaseFailureLowUsableLegCount(t *testing.T) {
	err := errors.New("zero-gamma: low usable leg count: 1 priced legs/1 OI-weighted GEX legs; need at least 100/25")
	if got := summarizeGammaPhaseFailure(err); got != "low_coverage" {
		t.Fatalf("summarizeGammaPhaseFailure = %q, want low_coverage", got)
	}
}

func TestGammaFailureAndUnionNeverExposeUntrustedText(t *testing.T) {
	const secret = "private://broker-token-sentinel"
	if got := summarizeGammaPhaseFailure(errors.New(secret)); got != "unavailable" {
		t.Fatalf("unknown failure summary = %q, want stable unavailable", got)
	}

	now := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	spy := rankableGammaFixture(rpc.GammaZeroScopeSPY, now)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now)
	spy.Warnings = []string{"oi_missing", secret}
	spy.WarningDetails = []rpc.GammaWarningDetail{{
		Code: secret, Scope: secret, Severity: secret, Message: secret, Impact: secret, Action: secret,
	}}
	combined := hydrateGammaComputed(combineGammaResults(spy, spx))
	if combined == nil {
		t.Fatal("combined is nil")
	}
	if got, want := combined.Warnings, []string{"oi_missing", "unclassified_data_warning"}; !equalStringSlices(got, want) {
		t.Fatalf("combined warnings = %v, want %v", got, want)
	}
	for _, detail := range combined.WarningDetails {
		if strings.Contains(detail.Code+detail.Message+detail.Impact+detail.Action, secret) {
			t.Fatalf("untrusted warning leaked through structured detail: %+v", detail)
		}
	}
	raw, err := json.Marshal(combined)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("untrusted warning leaked anywhere in recursive wire JSON: %s", raw)
	}
}

func TestCanonicalGammaWarningUnionKeepsPreviousSuccessFallbackTyped(t *testing.T) {
	got := canonicalGammaWarningUnion([]string{"spx_cache_fallback:previous_success"})
	want := []string{"spx_cache_fallback:previous_success"}
	if !equalStringSlices(got, want) {
		t.Fatalf("canonical warnings = %v, want %v", got, want)
	}

	now := time.Date(2026, 6, 8, 15, 0, 0, 0, time.UTC)
	spx := rankableGammaFixture(rpc.GammaZeroScopeSPX, now)
	spx.Warnings = got
	hydrateGammaComputed(spx)
	if !equalStringSlices(spx.Warnings, want) {
		t.Fatalf("hydrated warnings = %v, want %v", spx.Warnings, want)
	}
	annotateGammaQuality(spx, now)
	if spx.Quality == nil || spx.Quality.Rankability != rpc.GammaRankabilityRankable {
		t.Fatalf("fresh previous-success fallback lost rankability: %+v", spx.Quality)
	}
}

// TestComputeGammaCombinedAbortsOnCancelledJobContext pins the
// degrade-vs-abort contract: an SPX phase failure with the job's own
// context dead must fail the whole combined compute — the failure is our
// cancellation (daemon shutdown, force() supersede), not a
// data-availability verdict, and a degraded "success" would be promoted
// and persisted as the session-final result (2026-06-10 incident: a
// post-release shutdown cancelled the SPX phase mid-fetch and the
// SPY-only context_only result was served as unranked regime evidence
// all of the following day). The same failure with a live job context
// keeps the SPY side, degrading per design §8.2.
func TestComputeGammaCombinedAbortsOnCancelledJobContext(t *testing.T) {
	orig := runGammaUnderlyingPhase
	t.Cleanup(func() { runGammaUnderlyingPhase = orig })

	spy := gammaCombineFallbackFixture(time.Now().Add(-time.Minute), gammaMinPricedLegs, gammaMinGEXLegs)

	t.Run("cancelled job context aborts", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runGammaUnderlyingPhase = func(_ context.Context, _ *Server, _ *ibkrlib.Connector, underlying string, _ rpc.GammaZeroParams, _ *atomic.Int32, _ int32) (*rpc.GammaZeroComputed, error) {
			if underlying == "SPY" {
				return cloneGammaComputed(spy), nil
			}
			cancel() // shutdown/supersede arrives mid-SPX-fetch
			return nil, context.Canceled
		}
		res, err := computeGammaCombined(ctx, nil, nil, rpc.GammaZeroParams{}, new(atomic.Int32))
		if err == nil {
			t.Fatalf("cancelled job must fail the combined compute, got result %+v", res)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want wrapped context.Canceled", err)
		}
		if res != nil {
			t.Fatalf("cancelled job must not return a persistable result, got %+v", res)
		}
	})

	t.Run("live job context keeps the SPY side", func(t *testing.T) {
		runGammaUnderlyingPhase = func(_ context.Context, _ *Server, _ *ibkrlib.Connector, underlying string, _ rpc.GammaZeroParams, _ *atomic.Int32, _ int32) (*rpc.GammaZeroComputed, error) {
			if underlying == "SPY" {
				return cloneGammaComputed(spy), nil
			}
			return nil, errors.New("error 354: not subscribed")
		}
		res, err := computeGammaCombined(context.Background(), nil, nil, rpc.GammaZeroParams{}, new(atomic.Int32))
		if res == nil {
			t.Fatalf("SPX data failure with a live job context must keep the SPY side, err=%v", err)
		}
	})
}

func gammaCombineFallbackFixture(asOf time.Time, pricedLegs, gexLegs int) *rpc.GammaZeroComputed {
	return &rpc.GammaZeroComputed{
		Scope:          rpc.GammaZeroScopeSPY,
		AsOf:           asOf,
		GammaTotalAbs:  1,
		LegCount:       gexLegs,
		PricedLegCount: pricedLegs,
		LegCount0DTE:   1,
		LegCount1to7:   1,
		LegCountTerm:   1,
		LegDiagnostics: &rpc.GammaLegDiagnostics{
			Total: rpc.GammaLegDiagnosticCounts{
				PricedLegs:        pricedLegs,
				OpenInterestLegs:  pricedLegs,
				GammaPositiveLegs: pricedLegs,
				AbsGEXLegs:        gexLegs,
			},
			ByUnderlying: map[string]rpc.GammaLegDiagnosticCounts{
				"SPY": {
					PricedLegs:        pricedLegs,
					OpenInterestLegs:  pricedLegs,
					GammaPositiveLegs: pricedLegs,
					AbsGEXLegs:        gexLegs,
				},
			},
		},
		SkewFitQuality: map[string]rpc.SkewFitInfo{
			"20260619": {RSquared: 0.95},
		},
	}
}
