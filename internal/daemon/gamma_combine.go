package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

// combineGammaResults builds the SPY+SPX result envelope from the two
// single-underlying GammaZeroComputed payloads.
//
// What it DOES surface:
//
//   - PerIndex: the two single-underlying GammaZeroComputed payloads,
//     each fully formed with their own ZeroGamma / GapPct / Profile.
//     This is the load-bearing decision surface for the user.
//   - RegimeAgreement: classifies whether SPY and SPX agree on the
//     gamma regime (long-γ / short-γ / transition) or disagree
//     (one stabilising, one amplifying).
//   - GammaTotalAbs: the sum across both indices, framed as "total
//     dealer-book size" — a diagnostic, not a regime headline.
//   - TopStrikes: merged + sorted + top-K. With the 100× per-contract
//     scaling SPX rows will dominate; the renderer's INDEX column
//     makes the imbalance visible rather than hidden.
//   - Expirations, LegCount, PricedLegCount, IV-source counters, and
//     LegDiagnostics: unioned / summed for the diagnostic footer.
//   - Warnings: unioned across spy + spx then deduped. They hydrate into
//     WarningDetails before the result leaves the cache.
//
// What it DOES NOT surface: a top-level spot, gamma sign, or zero-gamma
// price. SPY and SPX live on different price scales, so those fields are
// per-index only in combined scope.
func combineGammaResults(spy, spx *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if spy == nil && spx == nil {
		return nil
	}
	// One-sided fallbacks. The entitlement-graceful path in
	// computeGammaCombined returns the SPY-only result directly when
	// SPX errors, so these branches are defensive — they should not
	// fire on a healthy combined run.
	if spy == nil {
		return spx
	}
	if spx == nil {
		return spy
	}

	// Top strikes: merge, sort by AbsGEX descending, take top-K. SPX
	// rows will dominate per the spot² scaling; the renderer surfaces
	// the INDEX column so the imbalance is visible.
	allTop := append(append([]rpc.StrikeConcentration{}, spy.TopStrikes...), spx.TopStrikes...)
	sort.SliceStable(allTop, func(i, j int) bool {
		return allTop[i].AbsGEX > allTop[j].AbsGEX
	})
	if len(allTop) > topStrikesK {
		allTop = allTop[:topStrikesK]
	}
	combinedAbs := spy.GammaTotalAbs + spx.GammaTotalAbs
	var topConcPct float64
	if combinedAbs > 0 && len(allTop) > 0 {
		topConcPct = allTop[0].AbsGEX / combinedAbs * 100
	}

	asOf := combinedGammaAsOf(spy.AsOf, spx.AsOf)
	method := spy.Method
	if method == "" {
		method = spx.Method
	}
	convention := spy.GammaTotalAbsConvention
	if convention == "" {
		convention = spx.GammaTotalAbsConvention
	}
	citations := spy.MethodologyCitations
	if len(citations) == 0 {
		citations = spx.MethodologyCitations
	}
	params := spy.Params

	out := &rpc.GammaZeroComputed{
		Scope:                   rpc.GammaZeroScopeCombined,
		GammaTotalAbs:           combinedAbs,
		GammaTotalAbsConvention: convention,
		TopStrikes:              allTop,
		TopConcentrationPct:     topConcPct,
		LegCount:                spy.LegCount + spx.LegCount,
		PricedLegCount:          spy.PricedLegCount + spx.PricedLegCount,
		DerivedIVLegs:           spy.DerivedIVLegs + spx.DerivedIVLegs,
		ModelTickLegs:           spy.ModelTickLegs + spx.ModelTickLegs,
		DerivedLiveMidLegs:      spy.DerivedLiveMidLegs + spx.DerivedLiveMidLegs,
		DerivedPrevCloseLegs:    spy.DerivedPrevCloseLegs + spx.DerivedPrevCloseLegs,
		LegDiagnostics:          combineGammaLegDiagnostics(spy.LegDiagnostics, spx.LegDiagnostics),
		CollectionDiagnostics:   append(append([]rpc.GammaCollectionDiagnostic{}, spy.CollectionDiagnostics...), spx.CollectionDiagnostics...),
		Expirations:             dedupeStrings(append(append([]string{}, spy.Expirations...), spx.Expirations...)),
		Params:                  params,
		Source:                  "computed from IBKR SPY+SPX option chains",
		Method:                  method,
		MethodologyCitations:    citations,
		AsOf:                    asOf,
		DurationMS:              spy.DurationMS + spx.DurationMS,
		RegimeAgreement:         classifyRegimeAgreement(spy, spx),
		Warnings:                canonicalGammaWarningUnion(spy.Warnings, spx.Warnings),
		PerIndex: map[string]*rpc.GammaZeroComputed{
			"SPY": spy,
			"SPX": spx,
		},
	}
	sort.Strings(out.Expirations)

	// A combined SPY+SPX gamma-zero level is intentionally not
	// interpolated: the two underlyings live on different spot scales.
	// Per-index profiles remain available under PerIndex.
	if len(spy.Profile) > 0 && len(spx.Profile) > 0 && sameProfileGrid(spy.Profile, spx.Profile) {
		var combinedWarnings []string
		out.Profile, combinedWarnings = combineProfileBuckets(spy.Profile, spx.Profile, "", nil)
		out.Warnings = canonicalGammaWarningUnion(out.Warnings, combinedWarnings)
	}
	return out
}

// gammaInputAsOf returns the oldest spot observation the model was built on.
// SPY and SPX compute serially, so the result is published minutes after its
// inputs were sampled; ageing the freshness badge from completion time hides
// that gap and understates how stale the published level really is.
func gammaInputAsOf(c *rpc.GammaZeroComputed) time.Time {
	if c == nil {
		return time.Time{}
	}
	oldest := c.SpotAt
	for _, idx := range c.PerIndex {
		if idx == nil || idx.SpotAt.IsZero() {
			continue
		}
		if oldest.IsZero() || idx.SpotAt.Before(oldest) {
			oldest = idx.SpotAt
		}
	}
	if oldest.IsZero() {
		return c.AsOf
	}
	return oldest
}

func combinedGammaAsOf(spyAsOf, spxAsOf time.Time) time.Time {
	if spyAsOf.IsZero() {
		return spxAsOf
	}
	if spxAsOf.IsZero() {
		return spyAsOf
	}
	if spyAsOf.Before(spxAsOf) {
		return spyAsOf
	}
	return spxAsOf
}

// combineProfileBuckets sums the GEX values of two sweep profiles
// bucket-by-bucket on the assumption they share the same Spot grid.
// Returns nil + a warning appended to warnings when:
//   - the two lengths differ;
//   - any pair of corresponding Spot values is not exactly equal;
//   - either side is empty (no useful sum is possible).
//
// The exact-equality spot check is intentional: dealer GEX has no
// natural interpretation across spot scales, so any drift means we
// can't be sure the buckets represent the same scenario, and an
// incorrect sum is worse than a missing one. In SPY+SPX production
// the grids will always differ (SPY anchors ~540, SPX anchors ~5400)
// so this path is effectively "bail with a warning" today. The
// summed path exists for future per-index combinations where the
// grids align by construction (e.g. two trading classes of the same
// underlying).
func combineProfileBuckets(a, b []rpc.GammaProfilePoint, mismatchWarn string, warnings []string) ([]rpc.GammaProfilePoint, []string) {
	if len(a) == 0 || len(b) == 0 {
		return nil, warnings
	}
	if len(a) != len(b) {
		if mismatchWarn == "" {
			return nil, warnings
		}
		return nil, append(warnings, mismatchWarn)
	}
	for i := range a {
		if a[i].Spot != b[i].Spot {
			if mismatchWarn == "" {
				return nil, warnings
			}
			return nil, append(warnings, mismatchWarn)
		}
	}
	out := make([]rpc.GammaProfilePoint, len(a))
	for i := range a {
		out[i] = rpc.GammaProfilePoint{Spot: a[i].Spot, GEX: a[i].GEX + b[i].GEX}
	}
	return out, warnings
}

func sameProfileGrid(a, b []rpc.GammaProfilePoint) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Spot != b[i].Spot {
			return false
		}
	}
	return true
}

// classifyRegimeAgreement labels the SPY/SPX regime relationship by
// comparing per-index γ-zero sweep outcomes. Returns one of
// "agree:long-gamma", "agree:short-gamma", "agree:transition-gamma",
// "disagree", or "" (unknown — at least one bucket has no_data).
//
// The classification reads GammaSign plus the zero-gamma gap rather
// than fetching any external state:
//
//	per-index regime ∈ { long-gamma, short-gamma, transition-gamma, no-data }
//	  long-gamma:  GammaSign == "positive"    (whole sweep > 0)
//	  short-gamma: GammaSign == "negative"    (whole sweep < 0)
//	  transition:  ZeroGamma != nil and spot is within ±2% of it
//	  no-data:     GammaSign == "no_data" or anything else
//
// disagree fires whenever the two indices land in different non-no_data
// regimes — the actionable case where one book is amplifying while the
// other is stabilizing, regardless of whether the underlying prices
// happen to be correlated.
func classifyRegimeAgreement(spy, spx *rpc.GammaZeroComputed) string {
	spyR := perIndexRegime(spy)
	spxR := perIndexRegime(spx)
	if spyR == "" || spxR == "" {
		return ""
	}
	if spyR != spxR {
		return "disagree"
	}
	return "agree:" + spyR
}

// perIndexRegime maps a single-underlying GammaZeroComputed to a
// regime label. Returns "" on no-data or nil input.
func perIndexRegime(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	if c.ZeroGamma != nil {
		return strings.ReplaceAll(rpc.GammaRegimeFromGap(c.GapPct), "_", "-")
	}
	switch c.GammaSign {
	case "positive":
		return "long-gamma"
	case "negative":
		return "short-gamma"
	}
	return ""
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// runGammaUnderlyingPhase is a package seam over runUnderlyingPhase so
// computeGammaCombined's abort-vs-degrade decision is testable without a
// live connector (same pattern as fetchIBKRBorrowFees).
var runGammaUnderlyingPhase = runUnderlyingPhase

// computeGammaCombined runs SPY then SPX serially under separate
// underlying-holds and combines the two halves into one envelope.
//
// Underlying-hold transition (per design §7.1 audit checklist item 6):
// each half's Hold is scoped to its own function call so a panic or
// error in the SPX half cannot leak the SPY hold. The two halves'
// holds DO NOT overlap — SPY releases at SPY-phase end before SPX
// acquires. This bounds market-data subscription footprint to one
// underlying at a time.
//
// Availability-graceful degradation (per design §8.2): if one side
// fails because the gateway could not produce a usable OI/IV/GEX slice
// (354 entitlement, 200 contract, no-data, zero magnitude, etc.) but
// the other side succeeds, return the successful side with a structured
// warning rather than failing the whole default/regime gamma row.
//
// Degradation is a data-availability verdict, so it is forbidden when
// the combined job's own context is dead: then the phase error is our
// cancellation (daemon shutdown or a force() supersede), and degrading
// would hand spawnJob a half-fetched "success" that gets promoted and
// persisted as the session-final result. Observed 2026-06-10: the
// post-release daemon shutdown cancelled the SPX phase mid-fetch, the
// SPY-only context_only result was persisted at 21:56:23, and every
// daemon start the next day reloaded it as 5/6-ranked regime evidence.
func computeGammaCombined(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
) (*rpc.GammaZeroComputed, error) {
	spyRes, err := runGammaUnderlyingPhase(bgCtx, s, c, "SPY", params, prog, 0)
	if err != nil {
		if bgCtx.Err() != nil {
			return nil, fmt.Errorf("zero-gamma: SPY phase: %w", err)
		}
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spy_unavailable err=%v (trying SPX-only)", err)
		}
		spxRes, spxErr := runGammaUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
		if spxErr != nil {
			return nil, fmt.Errorf("zero-gamma: SPY phase: %w; SPX phase: %w", err, spxErr)
		}
		if ok, reason := gammaOneSidedFallbackUsableForCombined(spxRes, time.Now()); !ok {
			return spxRes, fmt.Errorf("zero-gamma: SPY phase: %w; SPX fallback not usable: %s", err, reason)
		}
		spxRes.Warnings = append(spxRes.Warnings, "spy_unavailable:"+summarizeGammaPhaseFailure(err))
		return hydrateGammaComputed(spxRes), nil
	}

	spxRes, spxErr := runGammaUnderlyingPhase(bgCtx, s, c, "SPX", params, prog, 50)
	if spxErr != nil {
		if bgCtx.Err() != nil {
			return nil, fmt.Errorf("zero-gamma: SPX phase: %w", spxErr)
		}
		if s != nil && s.logger != nil {
			s.logger.Warnf("gamma.combine.spx_unavailable err=%v (degrading to SPY-only)", spxErr)
		}
		if ok, reason := gammaOneSidedFallbackUsableForCombined(spyRes, time.Now()); !ok {
			return spyRes, fmt.Errorf("zero-gamma: SPX phase: %w; SPY fallback not usable: %s", spxErr, reason)
		}
		spyRes.Warnings = append(spyRes.Warnings, "spx_unavailable:"+summarizeGammaPhaseFailure(spxErr))
		return hydrateGammaComputed(spyRes), nil
	}

	combined := combineGammaResults(spyRes, spxRes)
	if combined == nil {
		return nil, fmt.Errorf("zero-gamma: combine produced nil result")
	}
	return hydrateGammaComputed(combined), nil
}

func gammaOneSidedFallbackUsableForCombined(result *rpc.GammaZeroComputed, now time.Time) (bool, string) {
	if result == nil {
		return false, "result is missing"
	}
	c := cloneGammaComputed(result)
	hydrateGammaComputed(c)
	annotateGammaQuality(c, now)
	if c.Quality == nil {
		return false, "quality is missing"
	}
	switch c.Quality.Rankability {
	case rpc.GammaRankabilityRankable, rpc.GammaRankabilityContextOnly:
		return true, ""
	default:
		if c.Quality.RankabilityReason != "" {
			return false, c.Quality.RankabilityReason
		}
		return false, c.Quality.Rankability
	}
}

// summarizeSPXFailure turns an SPX-phase error into the short token
// the warning-list embeds. Strips the verbose context that's helpful
// in logs but noisy in the renderer banner. Looks for the canonical
// IBKR error code in the message; falls back to "unavailable" for
// non-IBKR errors (gateway disconnect, ctx cancel).
//
// Token formats:
//
//	354       → entitlement gap (most common)
//	200       → contract not found / SPX chain restricted
//	fetch_canceled → compute context was cancelled before SPX landed
//	timeout        → deadline / timeout before SPX landed
//	zero_magnitude → legs landed but all gamma magnitude was zero
//	unavailable → every unclassified failure; raw causes remain local-log only
func summarizeSPXFailure(err error) string {
	return summarizeGammaPhaseFailure(err)
}

func summarizeGammaPhaseFailure(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(msg, "354"):
		return "354"
	case strings.Contains(msg, " 200 ") || strings.Contains(msg, "no security definition"):
		return "200"
	case errors.Is(err, context.Canceled), strings.Contains(lower, "context canceled"), strings.Contains(lower, "context cancelled"):
		return "fetch_canceled"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "timeout"), strings.Contains(lower, "timed out"):
		return "timeout"
	case strings.Contains(msg, "no option data landed"):
		return "no_data"
	case strings.Contains(msg, "throttled"):
		return "throttled"
	case strings.Contains(msg, "low usable leg count"):
		return "low_coverage"
	case strings.Contains(msg, "no usable GEX legs"), strings.Contains(msg, "zero gamma_total_abs/profile/top_strikes"):
		return "zero_magnitude"
	}
	return "unavailable"
}

// canonicalGammaWarningUnion protects the wire warning surface from raw or
// persisted free text. Known warning codes retain their decision semantics;
// every unrecognised value collapses to one fail-visible stable token.
func canonicalGammaWarningUnion(groups ...[]string) []string {
	var out []string
	unclassified := false
	for _, group := range groups {
		for _, raw := range group {
			code, ok := canonicalGammaWarningCode(raw)
			if !ok {
				if strings.TrimSpace(raw) != "" {
					unclassified = true
				}
				continue
			}
			out = append(out, code)
		}
	}
	if unclassified {
		out = append(out, "unclassified_data_warning")
	}
	return dedupeStrings(out)
}

func canonicalGammaWarningCode(raw string) (string, bool) {
	code := strings.ToLower(strings.TrimSpace(raw))
	switch code {
	case "no_crossing_in_window", "0dte_no_legs", "1to7_no_legs", "term_no_legs",
		"throttled", "oi_missing", "strike_budget_capped", "all_iv_derived",
		"cache_stale_off_hours", "closed_session_cache", "session_closed_no_cache",
		"persisted_cache_rejected", "unclassified_data_warning":
		return code, true
	}
	if suffix, ok := strings.CutPrefix(code, "expiries_stale:"); ok && gammaDigitsWithSuffix(suffix, 'd', 1, 4) {
		return code, true
	}
	if suffix, ok := strings.CutPrefix(code, "skew_fallback:"); ok && gammaDigits(suffix, 8) {
		return code, true
	}
	for _, prefix := range []string{"refresh_failed:", "spy_unavailable:", "spx_unavailable:", "spx_cache_fallback:"} {
		if suffix, ok := strings.CutPrefix(code, prefix); ok && canonicalGammaFailureToken(suffix) {
			return code, true
		}
	}
	if code == "spx_cache_fallback" {
		return code, true
	}
	return "", false
}

func canonicalGammaFailureToken(token string) bool {
	switch token {
	case "354", "200", "fetch_canceled", "timeout", "no_data", "throttled",
		"low_coverage", "zero_magnitude", "unavailable", "previous_success":
		return true
	default:
		return false
	}
}

func gammaDigits(value string, exact int) bool {
	if len(value) != exact {
		return false
	}
	for i := range value {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func gammaDigitsWithSuffix(value string, suffix byte, minDigits, maxDigits int) bool {
	if len(value) < minDigits+1 || len(value) > maxDigits+1 || value[len(value)-1] != suffix {
		return false
	}
	return gammaDigits(value[:len(value)-1], len(value)-1)
}

// runUnderlyingPhase wraps one (Hold underlying → computeGammaZeroFor →
// release underlying) cycle. Progress baseline is the starting %
// (0 for SPY phase, 50 for SPX phase) so the existing 0-100 atomic
// reports cleanly across both halves.
func runUnderlyingPhase(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	underlying string,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
	progressBase int32,
) (*rpc.GammaZeroComputed, error) {
	if s == nil {
		return nil, fmt.Errorf("server is nil")
	}
	release, err := s.subs.Hold(bgCtx, underlying)
	if err != nil {
		return nil, fmt.Errorf("hold %s underlying: %w", underlying, err)
	}
	defer release()

	innerProg := &atomic.Int32{}
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-t.C:
				inner := innerProg.Load()
				if inner <= 0 {
					continue
				}
				prog.Store(progressBase + inner/2)
				if inner >= 100 {
					return
				}
			}
		}
	}()

	var logger gammaLogger
	if s != nil {
		logger = s.logger
	}
	return computeGammaZeroFor(bgCtx, c, underlying, params, productionLegFetcher, time.Now, innerProg, logger, s.gammaOI, s.gammaGrids)
}

// gammaScopeForRequest maps the requested scope onto the actual
// scope the daemon will compute. Empty defaults to combined.
// Unknown scopes surface as an error to the caller.
func gammaScopeForRequest(scope string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "":
		return rpc.GammaZeroScopeCombined, nil
	case rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, rpc.GammaZeroScopeCombined:
		return strings.ToLower(scope), nil
	default:
		return "", fmt.Errorf("unknown scope %q (want spy|spx|spy+spx)", scope)
	}
}
