package daemon

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// combineGammaResults builds the SPY+SPX result envelope from the two
// per-index only in combined scope.
func combineGammaResults(spy, spx *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if spy == nil && spx == nil {
		return nil
	}
	// One-sided fallbacks. The entitlement-graceful path in
	if spy == nil {
		return spx
	}
	if spx == nil {
		return spy
	}

	// Top strikes: merge, sort by AbsGEX descending, take top-K. SPX
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
	if len(spy.Profile) > 0 && len(spx.Profile) > 0 && sameProfileGrid(spy.Profile, spx.Profile) {
		var combinedWarnings []string
		out.Profile, combinedWarnings = combineProfileBuckets(spy.Profile, spx.Profile, "", nil)
		out.Warnings = canonicalGammaWarningUnion(out.Warnings, combinedWarnings)
	}
	return out
}

// gammaInputAsOf returns the oldest spot observation the model was built on.
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
//   - any pair of corresponding Spot values is not exactly equal;
//
// The exact-equality spot check is intentional: dealer GEX has no
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
// live connector (same pattern as fetchIBKRBorrowFees).
var runGammaUnderlyingPhase = runUnderlyingPhase

// computeGammaCombined runs SPY then SPX serially under separate
// Availability-graceful degradation (per design §8.2): if one side
// fails because the gateway could not produce a usable OI/IV/GEX slice
// warning rather than failing the whole default/regime gamma row.
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
// IBKR error code in the message; falls back to "unavailable" for
// non-IBKR errors (gateway disconnect, ctx cancel).
// Token formats:
//
//	unavailable → every unclassified failure; raw causes remain local-log only
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

// runUnderlyingPhase wraps a (Hold underlying → computeGammaZeroFor →
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
	// Every gamma compute — startup prewarm, scheduler refresh, and
	bgCtx = ibkrlib.WithRequestPriority(bgCtx, ibkrlib.PriorityBackground)
	result, err := runUnderlyingPhaseOnce(bgCtx, s, c, underlying, params, prog, progressBase)
	if err == nil || !shouldRetryGammaWithDelayed(err, time.Now()) {
		return result, err
	}
	if c == nil {
		return nil, err
	}

	// IBKR 354 kills the original reqID and the connector remembers that
	// production fetcher accepts only delayed model tick 83 beside a delayed
	// spot. No other cause and no off-hours failure takes this path.
	releaseMode, fallbackErr := c.BeginDelayedMarketDataFallback(bgCtx, underlying)
	if fallbackErr != nil {
		return nil, fmt.Errorf("zero-gamma: %s delayed fallback after IBKR 354 unavailable: %w", underlying, fallbackErr)
	}
	defer releaseMode()
	prog.Store(progressBase)
	result, retryErr := runUnderlyingPhaseOnce(bgCtx, s, c, underlying, params, prog, progressBase)
	if retryErr != nil {
		return result, fmt.Errorf("zero-gamma: %s delayed fallback after IBKR 354 failed: %w", underlying, retryErr)
	}
	return result, nil
}

func shouldRetryGammaWithDelayed(err error, at time.Time) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if gammaClassifySession(at) != rpc.SessionRTH {
		return false
	}
	if spotErr, ok := errors.AsType[*gammaSpotError](err); ok {
		return spotErr.code == 354
	}
	if absent, ok := errors.AsType[*ibkrlib.MarketDataAbsenceError](err); ok {
		return absent.Code == 354
	}
	if rejected, ok := errors.AsType[*SubscriptionRejectedError](err); ok {
		return rejected.Rejection.Code == 354
	}
	return false
}

func runUnderlyingPhaseOnce(
	bgCtx context.Context,
	s *Server,
	c *ibkrlib.Connector,
	underlying string,
	params rpc.GammaZeroParams,
	prog *atomic.Int32,
	progressBase int32,
) (*rpc.GammaZeroComputed, error) {
	release, err := s.subs.Hold(bgCtx, underlying)
	if err != nil {
		return nil, fmt.Errorf("hold %s underlying: %w", underlying, err)
	}
	defer release()

	innerProg := &atomic.Int32{}
	stopProgress := make(chan struct{})
	defer close(stopProgress)
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-bgCtx.Done():
				return
			case <-stopProgress:
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
