package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const gammaNotAdvice = "Market-structure context only; not a trade recommendation."

func hydrateGammaComputed(c *rpc.GammaZeroComputed) *rpc.GammaZeroComputed {
	if c == nil {
		return nil
	}
	// Both cached payloads and child results are data-boundary input. Reduce
	// every warning surface to typed codes and rebuild the structured prose;
	// never preserve producer-supplied detail text across hydration.
	legacyCodes := make([]string, 0, len(c.WarningDetails))
	for _, detail := range c.WarningDetails {
		legacyCodes = append(legacyCodes, detail.Code)
	}
	c.Warnings = canonicalGammaWarningUnion(c.Warnings, legacyCodes)
	c.WarningDetails = nil
	for _, sub := range c.PerIndex {
		hydrateGammaComputed(sub)
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		c.WarningDetails = buildCombinedGammaWarningDetails(c)
	} else {
		c.WarningDetails = buildGammaWarningDetails(c)
	}
	c.Summary = buildGammaSummary(c)
	return c
}

// buildCombinedGammaWarningDetails projects the per-index warning surfaces to
// the combined envelope without pretending a SPY or SPX condition applies to
// both indices. The (scope, code) pair is the identity: duplicate copies from
// repeated hydration collapse, while the same code on SPY and SPX remains two
// distinct diagnostics.
func buildCombinedGammaWarningDetails(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}

	type warningKey struct {
		scope string
		code  string
	}
	seen := make(map[warningKey]struct{})
	childCodes := make(map[string]struct{})
	out := make([]rpc.GammaWarningDetail, 0, len(c.WarningDetails)+len(c.Warnings))
	appendDetail := func(d rpc.GammaWarningDetail) {
		if d.Code == "" {
			return
		}
		key := warningKey{scope: d.Scope, code: d.Code}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, d)
	}

	for _, label := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[label]
		if sub == nil {
			continue
		}
		for _, code := range sub.Warnings {
			if code != "" {
				childCodes[code] = struct{}{}
			}
		}
		for _, d := range sub.WarningDetails {
			if d.Code != "" {
				childCodes[d.Code] = struct{}{}
			}
			appendDetail(d)
		}
	}

	// Preserve any combined-local structured warning that was attached by a
	// cache/fallback path. Child-derived copies lose to the freshly hydrated
	// child details above, which keeps repeated hydration deterministic.
	for _, d := range c.WarningDetails {
		appendDetail(d)
	}
	// Warnings is deliberately the code-only union for daemon-internal
	// compatibility. Only codes absent from both children are combined-local;
	// child codes already have their correctly scoped details above.
	for _, code := range c.Warnings {
		if code == "" {
			continue
		}
		if _, inherited := childCodes[code]; inherited {
			continue
		}
		appendDetail(gammaWarningDetail(c, code))
	}
	return out
}

func buildGammaSummary(c *rpc.GammaZeroComputed) *rpc.GammaZeroSummary {
	if c == nil {
		return nil
	}
	out := &rpc.GammaZeroSummary{
		NotAdvice:  gammaNotAdvice,
		Confidence: gammaResultConfidence(c),
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		out.PerIndex = make(map[string]rpc.GammaIndexSummary, len(c.PerIndex))
		var parts []string
		statuses := map[string]int{}
		regimes := map[string]int{}
		confidences := map[string]int{}
		for _, key := range []string{"SPX", "SPY"} {
			sub := c.PerIndex[key]
			if sub == nil {
				continue
			}
			item := buildGammaIndexSummary(sub, key)
			out.PerIndex[key] = item
			statuses[item.ZeroGammaStatus]++
			regimes[item.Regime]++
			confidences[item.Confidence]++
			parts = append(parts, gammaCombinedIndexStatement(key, item))
		}
		out.ZeroGammaStatus = combineSummaryStatus(statuses)
		out.Regime = combineSummaryRegime(c.RegimeAgreement, regimes)
		out.Confidence = combineSummaryConfidence(confidences)
		if c.Quality != nil && c.Quality.Rankability == rpc.GammaRankabilityRankable {
			out.Confidence = gammaResultConfidence(c)
		}
		if len(parts) == 0 {
			out.PrimaryStatement = "Zero-gamma: unavailable for SPY+SPX."
		} else {
			out.PrimaryStatement = "Zero-gamma: " + strings.Join(parts, "; ") + ". Price levels remain per-index because SPY and SPX use different price scales."
		}
		return out
	}
	item := buildGammaIndexSummary(c, gammaUnderlyingLabel(c))
	out.ZeroGammaStatus = item.ZeroGammaStatus
	out.Regime = item.Regime
	out.PrimaryStatement = "Zero-gamma: " + gammaIndexStatement(item) + "."
	return out
}

func buildGammaIndexSummary(c *rpc.GammaZeroComputed, label string) rpc.GammaIndexSummary {
	if label == "" {
		label = gammaUnderlyingLabel(c)
	}
	status, regime := gammaZeroStatusAndRegime(c)
	return rpc.GammaIndexSummary{
		Underlying:      label,
		SpotUnderlying:  c.SpotUnderlying,
		DataType:        c.DataType,
		ZeroGamma:       c.ZeroGamma,
		ZeroGammaStatus: status,
		Regime:          regime,
		SweepLowAbs:     c.SweepLowAbs,
		SweepHighAbs:    c.SweepHighAbs,
		LegCount:        c.LegCount,
		PricedLegCount:  c.PricedLegCount,
		GammaTotalAbs:   c.GammaTotalAbs,
		Confidence:      gammaResultConfidence(c),
		Interpretation:  gammaInterpretation(c, status, regime),
	}
}

func gammaCombinedIndexStatement(key string, s rpc.GammaIndexSummary) string {
	switch key {
	case "SPX":
		s.Underlying = "SPX canonical"
	case "SPY":
		s.Underlying = "SPY context"
	}
	return gammaIndexStatement(s)
}

func gammaIndexStatement(s rpc.GammaIndexSummary) string {
	label := s.Underlying
	if label == "" {
		label = "underlying"
	}
	switch s.ZeroGammaStatus {
	case "crossing":
		if s.ZeroGamma != nil {
			regime := strings.ReplaceAll(s.Regime, "_", "-")
			if regime != "" {
				return fmt.Sprintf("%s zero-gamma %s (%s)", label, formatGammaSummaryPrice(*s.ZeroGamma), regime)
			}
			return fmt.Sprintf("%s zero-gamma %s", label, formatGammaSummaryPrice(*s.ZeroGamma))
		}
	case "none_in_window":
		rangeText := gammaSummaryRange(s.SweepLowAbs, s.SweepHighAbs)
		regime := strings.ReplaceAll(s.Regime, "_", "-")
		if rangeText != "" && regime != "" {
			return fmt.Sprintf("%s stayed %s across %s", label, regime, rangeText)
		}
		if regime != "" {
			return fmt.Sprintf("%s stayed %s across the swept range", label, regime)
		}
		return fmt.Sprintf("%s had no crossing in the swept range", label)
	case "unavailable":
		if s.Interpretation != "" {
			return fmt.Sprintf("%s unavailable (%s)", label, s.Interpretation)
		}
		return fmt.Sprintf("%s unavailable", label)
	}
	return fmt.Sprintf("%s indeterminate", label)
}

func gammaZeroStatusAndRegime(c *rpc.GammaZeroComputed) (string, string) {
	if c == nil {
		return "unavailable", "unavailable"
	}
	if c.ZeroGamma != nil {
		return "crossing", rpc.GammaRegimeFromGap(c.GapPct)
	}
	if c.LegCount > 0 && c.GammaTotalAbs == 0 && gammaProfileAllZero(c.Profile) {
		return "unavailable", "unavailable"
	}
	switch c.GammaSign {
	case "positive":
		return "none_in_window", "long_gamma"
	case "negative":
		return "none_in_window", "short_gamma"
	case "no_data":
		return "unavailable", "unavailable"
	default:
		return "unavailable", "unavailable"
	}
}

func gammaInterpretation(c *rpc.GammaZeroComputed, status, regime string) string {
	switch status {
	case "crossing":
		if c.ZeroGamma != nil {
			if c.GapPct != nil {
				return fmt.Sprintf("signed gamma profile crosses zero at %s; spot is %+.1f%% from that level",
					formatGammaSummaryPrice(*c.ZeroGamma), *c.GapPct)
			}
			return "signed gamma profile crosses zero at " + formatGammaSummaryPrice(*c.ZeroGamma)
		}
	case "none_in_window":
		switch regime {
		case "long_gamma":
			return "no crossing; model stayed long-gamma across the swept range"
		case "short_gamma":
			return "no crossing; model stayed short-gamma across the swept range"
		}
	case "unavailable":
		if c != nil && c.LegCount > 0 && c.GammaTotalAbs == 0 {
			return "no usable gamma magnitude from landed legs"
		}
		return "no usable signed gamma profile"
	}
	return ""
}

func gammaResultConfidence(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "unavailable"
	}
	if c.Quality != nil {
		switch c.Quality.Rankability {
		case rpc.GammaRankabilityRankable:
			return "estimate"
		case rpc.GammaRankabilityContextOnly, rpc.GammaRankabilityBlocked:
			return "degraded"
		case rpc.GammaRankabilityUnavailable:
			return "unavailable"
		}
	}
	if c.LegCount > 0 && c.GammaTotalAbs == 0 && gammaProfileAllZero(c.Profile) {
		return "unavailable"
	}
	for _, w := range gammaWarningCodes(c) {
		switch {
		case w == "throttled", w == "all_iv_derived", w == "cache_stale_off_hours", w == "oi_missing":
			return "degraded"
		case strings.HasPrefix(w, "spy_unavailable:"):
			return "degraded"
		case strings.HasPrefix(w, "spx_unavailable:"):
			return "degraded"
		case strings.HasPrefix(w, "skew_fallback:"):
			return "degraded"
		}
	}
	return "estimate"
}

// gammaWarningCodes returns warnings that apply to this quality node. A
// combined envelope also projects its children's warnings for wire consumers;
// those inherited details are excluded here because each child already gates
// its own quality and SPY context must not re-gate the SPX-canonical result.
func gammaWarningCodes(c *rpc.GammaZeroComputed) []string {
	if c == nil {
		return nil
	}
	type warningKey struct {
		scope string
		code  string
	}
	childCodes := map[string]struct{}{}
	childDetails := map[warningKey]struct{}{}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		for _, sub := range c.PerIndex {
			if sub == nil {
				continue
			}
			for _, code := range sub.Warnings {
				if code != "" {
					childCodes[code] = struct{}{}
				}
			}
			for _, d := range sub.WarningDetails {
				if d.Code == "" {
					continue
				}
				childCodes[d.Code] = struct{}{}
				childDetails[warningKey{scope: d.Scope, code: d.Code}] = struct{}{}
			}
		}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, code := range c.Warnings {
		if code == "" {
			continue
		}
		if _, inherited := childCodes[code]; inherited {
			continue
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		out = append(out, code)
	}
	for _, d := range c.WarningDetails {
		if d.Code == "" {
			continue
		}
		if _, inherited := childDetails[warningKey{scope: d.Scope, code: d.Code}]; inherited {
			continue
		}
		if _, ok := seen[d.Code]; ok {
			continue
		}
		seen[d.Code] = struct{}{}
		out = append(out, d.Code)
	}
	return out
}

func combineSummaryStatus(counts map[string]int) string {
	if len(counts) == 0 {
		return "unavailable"
	}
	if len(counts) == 1 {
		for k := range counts {
			return k
		}
	}
	if counts["unavailable"] > 0 {
		return "mixed_degraded"
	}
	return "mixed"
}

func combineSummaryRegime(agreement string, regimes map[string]int) string {
	switch agreement {
	case "agree:long-gamma":
		return "long_gamma"
	case "agree:short-gamma":
		return "short_gamma"
	case "agree:transition-gamma":
		return "transition_gamma"
	case "disagree":
		return "mixed"
	}
	if len(regimes) == 1 {
		for k := range regimes {
			return k
		}
	}
	if len(regimes) == 0 {
		return "unavailable"
	}
	return "mixed"
}

func combineSummaryConfidence(counts map[string]int) string {
	switch {
	case len(counts) == 0:
		return "unavailable"
	case counts["unavailable"] > 0:
		return "unavailable"
	case counts["degraded"] > 0:
		return "degraded"
	default:
		return "estimate"
	}
}

func buildGammaWarningDetails(c *rpc.GammaZeroComputed) []rpc.GammaWarningDetail {
	if c == nil {
		return nil
	}
	codes := gammaWarningCodes(c)
	if len(codes) == 0 {
		return c.WarningDetails
	}
	out := make([]rpc.GammaWarningDetail, 0, len(codes))
	for _, code := range codes {
		out = append(out, gammaWarningDetail(c, code))
	}
	return out
}

func gammaWarningDetail(c *rpc.GammaZeroComputed, code string) rpc.GammaWarningDetail {
	scope := gammaWarningScope(c, code)
	d := rpc.GammaWarningDetail{
		Code:     code,
		Scope:    scope,
		Severity: "info",
	}
	switch {
	case code == "no_crossing_in_window":
		d.Message = "No signed gamma-zero crossing was found in the swept range."
		d.Impact = "Use the regime label and swept range instead of a zero-gamma level."
	case code == "0dte_no_legs":
		d.Message = "No same-day expiry legs were included."
		d.Impact = "The 0DTE horizon is unavailable for this run."
	case code == "1to7_no_legs":
		d.Message = "No 1-7 DTE legs were included."
		d.Impact = "The weekly horizon is unavailable for this run."
	case code == "term_no_legs":
		d.Message = "No >7 DTE legs were included."
		d.Impact = "The term horizon is unavailable for this run."
	case code == "throttled":
		d.Severity = "data_quality"
		d.Message = "The gateway throttled part of the option fan-out."
		d.Impact = "Coverage may be incomplete; treat this slice as lower confidence."
		d.Action = "Retry later or during regular trading hours; avoid repeated forced runs."
	case code == "oi_missing":
		session := gammaWarningSession(c)
		if gammaOIMissingUnexpected(d.Scope, session) {
			d.Severity = "data_quality"
		}
		missing := gammaOIMissingCount(c.LegDiagnostics)
		if missing == 0 {
			missing = max(c.PricedLegCount-c.LegCount, 0)
		}
		d.Message = fmt.Sprintf("Open-interest ticks were missing for %d priced legs.", missing)
		d.Impact = fmt.Sprintf("%d priced legs contributed to IV/skew fitting; %d legs had observed OI and %d had positive OI for dealer GEX. Missing OI is unknown, not zero.", c.PricedLegCount, gammaOIObservedCount(c), c.LegCount)
		d.Action = gammaOIMissingAction(d.Scope, session)
	case code == "all_iv_derived":
		d.Severity = "data_quality"
		d.Message = "No gateway model IV ticks landed; all implied volatilities were back-solved."
		d.Impact = gammaIVSourceImpact(c)
		d.Action = "Treat gamma as non-voting until IBKR model-computation ticks resume; inspect gateway/farm notices and active market-data subscription pressure."
	case code == "strike_budget_capped":
		d.Severity = "methodology"
		d.Message = "The strike fan-out was capped to the nearest 80 listed strikes per expiry."
		d.Impact = "Farther out-of-money strikes inside the ±10% candidate window were skipped to keep the gateway request budget bounded."
	case code == "cache_stale_off_hours":
		d.Severity = "data_quality"
		d.Message = "The cached gamma result is older than 24 hours and markets are closed."
		d.Impact = "The daemon served the last persisted snapshot rather than recomputing against a closed market."
	case code == "unclassified_data_warning":
		d.Severity = "data_quality"
		d.Message = "An unclassified gamma data warning was received."
		d.Impact = "The affected gamma slice is not trusted for ranking until a typed warning is available."
		d.Action = "Inspect local daemon diagnostics and retry after the data source recovers."
	case strings.HasPrefix(code, "refresh_failed:"):
		d.Severity = "data_quality"
		summary := strings.TrimPrefix(code, "refresh_failed:")
		summary = strings.ReplaceAll(summary, "_", " ")
		d.Message = "The latest gamma refresh failed."
		d.Impact = "The daemon is serving an older cached gamma snapshot; do not treat it as a fresh market-structure read."
		if summary != "" {
			d.Action = "Inspect gateway/farm state and retry after resolving: " + summary + "."
		} else {
			d.Action = "Inspect gateway/farm state and retry after resolving the refresh failure."
		}
	case strings.HasPrefix(code, "spy_unavailable:"):
		d.Severity = "data_quality"
		d.Message, d.Impact, d.Action = spyUnavailableWarningText(strings.TrimPrefix(code, "spy_unavailable:"))
	case strings.HasPrefix(code, "spx_unavailable:"):
		d.Severity = "data_quality"
		d.Message, d.Impact, d.Action = spxUnavailableWarningText(strings.TrimPrefix(code, "spx_unavailable:"))
	case strings.HasPrefix(code, "spx_cache_fallback"):
		d.Severity = "info"
		d.Message, d.Impact, d.Action = spxCacheFallbackWarningText(strings.TrimPrefix(code, "spx_cache_fallback"))
	case strings.HasPrefix(code, "skew_fallback:"):
		d.Severity = "methodology"
		expiry := strings.TrimPrefix(code, "skew_fallback:")
		d.Scope = expiry
		d.Message = "Skew fit fell back to sticky-IV for expiry " + expiry + "."
		d.Impact = "That expiry used the simpler IV assumption during the sweep."
	default:
		// Warning codes are typed before reaching this renderer. Keep the
		// fallback generic so a corrupt cache or future producer cannot turn
		// arbitrary text into browser-visible copy.
		d.Code = "unclassified_data_warning"
		d.Severity = "data_quality"
		d.Message = "An unclassified gamma data warning was received."
		d.Impact = "The affected gamma slice is not trusted for ranking until a typed warning is available."
	}
	return d
}

func gammaIVSourceImpact(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "The result is more model-dependent because every priced leg used quote/close inversion."
	}
	denom := c.PricedLegCount
	if denom == 0 {
		denom = c.LegCount
	}
	var parts []string
	if c.DerivedLiveMidLegs > 0 {
		parts = append(parts, fmt.Sprintf("%d live bid/ask midpoint", c.DerivedLiveMidLegs))
	}
	if c.DerivedPrevCloseLegs > 0 {
		parts = append(parts, fmt.Sprintf("%d prior option close", c.DerivedPrevCloseLegs))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("The result is more model-dependent because %d/%d priced legs used quote/close inversion instead of IBKR model-computation ticks.", c.DerivedIVLegs, denom)
	}
	return fmt.Sprintf("The result is more model-dependent: %d/%d priced legs used quote/close inversion (%s) instead of IBKR model-computation ticks.", c.DerivedIVLegs, denom, strings.Join(parts, ", "))
}

func gammaOIObservedCount(c *rpc.GammaZeroComputed) int {
	if c == nil {
		return 0
	}
	if c.LegDiagnostics == nil {
		return c.LegCount
	}
	return max(c.LegDiagnostics.Total.OpenInterestObservedLegs, c.LegDiagnostics.Total.OpenInterestLegs)
}

func gammaWarningSession(c *rpc.GammaZeroComputed) rpc.SessionClass {
	asOf := time.Now()
	if c != nil && !c.AsOf.IsZero() {
		asOf = c.AsOf
	}
	return gammaClassifySession(asOf)
}

func gammaOIMissingUnexpected(scope string, session rpc.SessionClass) bool {
	scope = strings.ToUpper(strings.TrimSpace(scope))
	return scope == "SPX" || session == rpc.SessionRTH
}

func gammaOIMissingAction(scope string, session rpc.SessionClass) string {
	prefix := "The option request already asks IBKR for generic tick 101 (call/put open interest). "
	if strings.EqualFold(strings.TrimSpace(scope), "SPX") {
		if session == rpc.SessionRTH {
			return prefix + "This affected SPX during regular U.S. option hours, when OI should normally be available if TWS has it; check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
		}
		return prefix + "This affected SPX. SPX option OI should normally be stable across session phases; missing API OI is unknown, not zero. Check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
	}
	switch session {
	case rpc.SessionRTH:
		return prefix + "This happened during regular U.S. option hours, when OI should normally be available if TWS has it; check the same class/expiry/strike in TWS, data-farm health, and API logs before trusting the gamma magnitude."
	case rpc.SessionPre:
		return prefix + "This affected SPY pre-market, outside regular U.S. option hours, so sparse SPY OI is expected for the regular option-data surface; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	case rpc.SessionPost:
		return prefix + "This affected SPY post-market, outside regular U.S. option hours, so sparse SPY OI is expected for the regular option-data surface; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	default:
		return prefix + "This affected SPY while the regular U.S. option-data surface is closed, so sparse SPY OI is expected; missing OI is still unknown, not zero. Retry during 09:30-16:15 ET."
	}
}

func spyUnavailableWarningText(reason string) (message, impact, action string) {
	switch reason {
	case "354":
		return "SPY option chain was skipped: missing OPRA option market-data entitlement (IBKR 354).",
			"Showing SPX only; SPY gamma is not included.",
			"Check the U.S. options data subscription in IBKR, or run --only=spx to request the SPX surface directly."
	case "200":
		return "SPY option chain was skipped: contract resolution was rejected (IBKR 200).",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later or run --only=spx if SPY is not available on this gateway."
	case "no_data":
		return "SPY option chain was skipped: no option data landed within the window.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET or run --only=spx."
	case "fetch_canceled", "context canceled", "context_canceled":
		return "SPY option-chain fetch was canceled before usable data landed.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spx."
	case "timeout", "context deadline exceeded":
		return "SPY option-chain fetch timed out before usable data landed.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spx."
	case "throttled":
		return "SPY option chain was skipped after gateway throttling.",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later; avoid repeated forced runs."
	case "zero_magnitude":
		return "SPY option chain was skipped because landed legs produced zero usable gamma magnitude.",
			"Showing SPX only; SPY gamma is not included because the SPY slice was not reliable enough to classify.",
			"Retry during regular trading hours or run --only=spy --force for diagnostics."
	default:
		return "SPY option chain was skipped: " + reason + ".",
			"Showing SPX only; SPY gamma is not included.",
			"Retry later or run --only=spx."
	}
}

func spxUnavailableWarningText(reason string) (message, impact, action string) {
	switch reason {
	case "354":
		return "SPX option chain was skipped: missing CBOE OPRA entitlement (IBKR 354).",
			"Showing SPY only; SPX gamma is not included.",
			"Subscribe to the required market data or run --only=spy to suppress this banner."
	case "200":
		return "SPX option chain was skipped: contract resolution was rejected (IBKR 200).",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later or run --only=spy if SPX is not available on this gateway."
	case "no_data":
		return "SPX option chain was skipped: no option data landed within the window.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during regular trading hours or run --only=spy."
	case "fetch_canceled", "context canceled", "context_canceled":
		return "SPX option-chain fetch was canceled before usable data landed.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spy."
	case "timeout", "context deadline exceeded":
		return "SPX option-chain fetch timed out before usable data landed.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry during 09:30-16:15 ET; if it repeats during regular hours, check TWS/daemon market-data logs or run --only=spy."
	case "throttled":
		return "SPX option chain was skipped after gateway throttling.",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later; avoid repeated forced runs."
	case "zero_magnitude":
		return "SPX option chain was skipped because landed legs produced zero usable gamma magnitude.",
			"Showing SPY only; the SPX slice was not reliable enough to classify.",
			"Retry during regular trading hours or run --only=spx --force for diagnostics."
	default:
		return "SPX option chain was skipped: " + reason + ".",
			"Showing SPY only; SPX gamma is not included.",
			"Retry later or run --only=spy."
	}
}

func spxCacheFallbackWarningText(reason string) (message, impact, action string) {
	reason = strings.TrimPrefix(reason, ":")
	if reason == "" {
		reason = "previous_success"
	}
	switch reason {
	case "fetch_canceled", "context canceled", "context_canceled":
		message = "SPX live refresh was canceled; using the last successful cached SPX slice."
	case "timeout", "context deadline exceeded":
		message = "SPX live refresh timed out; using the last successful cached SPX slice."
	case "throttled":
		message = "SPX live refresh was throttled; using the last successful cached SPX slice."
	case "354":
		message = "SPX live refresh hit an entitlement error; using the last successful cached SPX slice."
	case "200":
		message = "SPX live refresh hit a contract-resolution error; using the last successful cached SPX slice."
	default:
		message = "SPX live refresh was unavailable; using the last successful cached SPX slice."
	}
	return message,
		"SPX is included from cache; quality.rankability shows whether the gamma read is fresh and covered enough to act as a market-structure signal.",
		"Refresh during 09:30-16:15 ET and inspect the SPX per-index as_of before treating it as a fresh market-structure read."
}

func gammaWarningScope(c *rpc.GammaZeroComputed, code string) string {
	if strings.HasPrefix(code, "spy_unavailable:") {
		return "SPY"
	}
	if strings.HasPrefix(code, "spx_unavailable:") || strings.HasPrefix(code, "spx_cache_fallback") {
		return "SPX"
	}
	return gammaUnderlyingLabel(c)
}

func gammaUnderlyingLabel(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	switch c.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX"
	case rpc.GammaZeroScopeCombined:
		return "SPY+SPX"
	default:
		return "SPY"
	}
}

func gammaSummaryRange(lo, hi float64) string {
	if lo <= 0 || hi <= 0 {
		return ""
	}
	return formatGammaSummaryPrice(lo) + "-" + formatGammaSummaryPrice(hi)
}

func formatGammaSummaryPrice(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}
