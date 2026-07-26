package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	regimeFreshnessDelayed     = "delayed"
	regimeFreshnessDailyClose  = "daily_close"
	regimeFreshnessCached      = "cached"
	regimeFreshnessUnavailable = "unavailable"
	regimeFreshnessComputing   = "computing"
)

// annotateRegimeMetadata attaches the interpretation layer to each row. The
// band comes from the policy pass (populateStreaks): post-hysteresis, with
// freshness and confirmation eligibility computed once daemon-side. When a
// policy entry is absent (direct test callers), the raw classifier output is
// used — no hysteresis, no eligibility.
func annotateRegimeMetadata(r *rpc.RegimeSnapshotResult, policies map[string]regimeRowPolicy) {
	if r == nil {
		return
	}
	now := r.AsOf
	if now.IsZero() {
		now = time.Now()
	}
	band := func(key, raw string) string {
		if p, ok := policies[key]; ok {
			return bandOrUnranked(p.band)
		}
		return bandOrUnranked(raw)
	}
	reason := func(key, raw, text string) string {
		p, ok := policies[key]
		if !ok || p.band != "red" || raw == "red" {
			return text
		}
		if p.freshness != nil && p.freshness.Class == rpc.RegimeFreshnessOverdue {
			return text + " · prior-session band shown for awareness"
		}
		return text + " · hysteresis holding red"
	}
	policy := func(key string) (*rpc.RegimeEligibility, *rpc.RegimeFreshness) {
		p, ok := policies[key]
		if !ok {
			return nil, nil
		}
		return p.eligibility, p.freshness
	}

	vixRaw := bandForVIX(r.VIXTermStructure)
	vixElig, vixFresh := policy(StreakKeyVIXTerm)
	r.VIXTermStructure.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyVIXTerm, vixRaw),
		BandReason:  reason(StreakKeyVIXTerm, vixRaw, vixBandReason(r.VIXTermStructure)),
		Thresholds:  rpc.HeuristicThresholds("vix_term_structure_v1", "VIX/VIX3M < 0.92", "0.92 <= VIX/VIX3M < 1.00", "VIX/VIX3M >= 1.00"),
		AsOf:        gatewayAsOf(now, r.VIXTermStructure.Status, r.VIXTermStructure.DataType, "Cboe VIX and VIX3M via IBKR index market data", r.VIXTermStructure.VIXQuality, r.VIXTermStructure.VIX3MQuality),
		Eligibility: vixElig,
		Freshness:   vixFresh,
	}
	vvixRaw := bandForVolOfVol(r.VolOfVol)
	vvixElig, vvixFresh := policy(StreakKeyVolOfVol)
	r.VolOfVol.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyVolOfVol, vvixRaw),
		BandReason:  reason(StreakKeyVolOfVol, vvixRaw, volOfVolBandReason(r.VolOfVol)),
		Thresholds:  rpc.HeuristicThresholds("vvix_daily_v1", "VVIX < 90", "90 <= VVIX < 110", "VVIX >= 110"),
		AsOf:        officialRowAsOf(now, r.VolOfVol.AsOfDate, "Cboe official VVIX daily close", r.VolOfVol.Status),
		Eligibility: vvixElig,
		Freshness:   vvixFresh,
	}
	hygRaw := bandForHYGSPY(r.HYGSPYDivergence)
	hygElig, hygFresh := policy(StreakKeyHYGSPY)
	r.HYGSPYDivergence.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyHYGSPY, hygRaw),
		BandReason:  reason(StreakKeyHYGSPY, hygRaw, hygSPYBandReason(r.HYGSPYDivergence)),
		Thresholds:  rpc.HeuristicThresholds("hyg_spy_credit_proxy_v1", "HYG >= 50-day SMA", "HYG < 50-day SMA", "HYG < 50-day SMA and SPY >= 97% of 52-week high"),
		AsOf:        gatewayAsOf(now, r.HYGSPYDivergence.Status, r.HYGSPYDivergence.HYGDataType, "IBKR HYG/SPY quotes plus HMDS daily bars", r.HYGSPYDivergence.HYGQuality, r.HYGSPYDivergence.HYG50DMAQuality, r.HYGSPYDivergence.SPYQuality, r.HYGSPYDivergence.SPY52WHighQuality),
		Eligibility: hygElig,
		Freshness:   hygFresh,
	}
	oasRaw := bandForCreditSpreads(r.CreditSpreads)
	oasElig, oasFresh := policy(StreakKeyCredit)
	r.CreditSpreads.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyCredit, oasRaw),
		BandReason:  reason(StreakKeyCredit, oasRaw, creditSpreadBandReason(r.CreditSpreads)),
		Thresholds:  rpc.HeuristicThresholds("hy_ig_oas_v1", "HY OAS < 4.0 and 20d widening < 0.50 pp", "HY OAS 4.0-5.5 or 20d widening >= 0.50 pp", "HY OAS >= 5.5 or 20d widening >= 1.00 pp"),
		AsOf:        officialRowAsOf(now, r.CreditSpreads.AsOfDate, "FRED/St. Louis Fed official ICE BofA OAS series", r.CreditSpreads.Status),
		Eligibility: oasElig,
		Freshness:   oasFresh,
	}
	fundRaw := bandForFundingStress(r.FundingStress)
	fundElig, fundFresh := policy(StreakKeyFunding)
	r.FundingStress.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyFunding, fundRaw),
		BandReason:  reason(StreakKeyFunding, fundRaw, fundingBandReason(r.FundingStress)),
		Thresholds:  rpc.HeuristicThresholds("funding_cp_tbill_v1", "CP/T-bill spread < 25 bp", "25 <= spread < 75 bp", "spread >= 75 bp"),
		AsOf:        officialRowAsOf(now, r.FundingStress.AsOfDate, "Federal Reserve CP DDP plus U.S. Treasury Daily Treasury Bill Rates", r.FundingStress.Status),
		Eligibility: fundElig,
		Freshness:   fundFresh,
	}
	fxRaw := bandForUSDJPY(r.USDJPY)
	fxElig, fxFresh := policy(StreakKeyUSDJPY)
	r.USDJPY.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyUSDJPY, fxRaw),
		BandReason:  reason(StreakKeyUSDJPY, fxRaw, usdJPYBandReason(r.USDJPY)),
		Thresholds:  rpc.HeuristicThresholds("usd_jpy_carry_proxy_v1", "yen strengthening < 1% over the week", "yen strengthening 1-2% over the week", "yen strengthening >= 2% over the week"),
		AsOf:        gatewayAsOf(now, r.USDJPY.Status, r.USDJPY.DataType, "IBKR CASH/IDEALPRO USD.JPY plus HMDS midpoint bars", r.USDJPY.LastQuality, r.USDJPY.Close7DAgoQuality),
		Eligibility: fxElig,
		Freshness:   fxFresh,
	}
	gammaRaw := bandForGamma(r.GammaZero)
	gammaElig, gammaFresh := policy(StreakKeyGammaZero)
	r.GammaZero.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyGammaZero, gammaRaw),
		BandReason:  reason(StreakKeyGammaZero, gammaRaw, gammaBandReason(r.GammaZero)),
		Thresholds:  rpc.HeuristicThresholds("dealer_gamma_v3", "spot > 2% above gamma-zero or profile wholly long-gamma", "spot within +/-2% of gamma-zero or mixed gamma profile", "spot below gamma-zero, profile wholly short-gamma, or dominant/equal exposure is amplifying"),
		AsOf:        gammaAsOf(now, r.GammaZero),
		Eligibility: gammaElig,
		Freshness:   gammaFresh,
	}
	breadthRaw := bandForBreadth(r.Breadth)
	breadthElig, breadthFresh := policy(StreakKeyBreadth)
	r.Breadth.RegimeIndicatorMeta = rpc.RegimeIndicatorMeta{
		Band:        band(StreakKeyBreadth, breadthRaw),
		BandReason:  reason(StreakKeyBreadth, breadthRaw, breadthBandReason(r.Breadth)),
		Thresholds:  rpc.HeuristicThresholds("spx_breadth_50dma_v1", "SPX members above 50-DMA > 55%", "40% <= members above 50-DMA <= 55%", "members above 50-DMA < 40%"),
		AsOf:        breadthAsOf(now, r.Breadth),
		Eligibility: breadthElig,
		Freshness:   breadthFresh,
	}
}

func bandOrUnranked(band string) string {
	if band == "" {
		return "unranked"
	}
	return band
}

func vixBandReason(r rpc.RegimeVIXTerm) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "VIX/VIX3M tick missing")
	}
	switch bandForVIX(r) {
	case "green":
		return "<0.92 contango"
	case "yellow":
		return "0.92-1.00 flattening"
	case "red":
		return ">=1.00 backwardation"
	default:
		return "ratio unavailable"
	}
}

func volOfVolBandReason(r rpc.RegimeVolOfVol) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "Cboe VVIX file unavailable")
	}
	switch bandForVolOfVol(r) {
	case "green":
		return "<90 vol-of-vol"
	case "yellow":
		return "90-110"
	case "red":
		return ">=110 vol-of-vol shock"
	default:
		return "VVIX unavailable"
	}
}

func hygSPYBandReason(r rpc.RegimeHYGSPYDivergence) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "credit proxy tick missing")
	}
	switch bandForHYGSPY(r) {
	case "green":
		return "HYG >= 50dma"
	case "yellow":
		return "HYG < 50dma"
	case "red":
		return "HYG < 50dma; SPY near highs"
	default:
		if r.HYG50DMA == nil {
			return "50dma missing; cannot band"
		}
		return "SPY 52w-high context missing"
	}
}

func creditSpreadBandReason(r rpc.RegimeCreditSpreads) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "FRED OAS series unavailable")
	}
	switch bandForCreditSpreads(r) {
	case "green":
		return "HY OAS <4.0"
	case "yellow":
		return "HY OAS elevated/widening"
	case "red":
		return "HY OAS stress"
	default:
		return "HY OAS missing"
	}
}

func fundingBandReason(r rpc.RegimeFundingStress) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "official funding series unavailable")
	}
	switch bandForFundingStress(r) {
	case "green":
		return "<25bp"
	case "yellow":
		return "25-75bp"
	case "red":
		return ">=75bp funding stress"
	default:
		return "spread unavailable"
	}
}

func usdJPYBandReason(r rpc.RegimeUSDJPY) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return shortMetaReason(r.ErrorMessage, "check IDEALPRO entitlement")
	}
	switch bandForUSDJPY(r) {
	case "green":
		return "<1% weekly"
	case "yellow":
		return "yen strengthening 1-2%"
	case "red":
		return "yen strengthening >=2%"
	default:
		return "weekly_change_pct missing"
	}
}

func gammaBandReason(r rpc.RegimeGammaZero) string {
	if r.Status == rpc.RegimeStatusComputing {
		return "first call of the NY session; re-poll for result"
	}
	if r.Status == rpc.RegimeStatusUnavailable {
		return "no cached gamma snapshot"
	}
	if r.Status == rpc.RegimeStatusError {
		return shortMetaReason(r.Envelope.Error, "gamma compute failed")
	}
	c := r.Envelope.Result
	if c == nil {
		return "envelope missing payload"
	}
	if c.Quality == nil {
		return "quality missing; gamma is unranked"
	}
	if c.Quality.Rankability != rpc.GammaRankabilityRankable {
		if c.Quality.RankabilityReason != "" {
			return c.Quality.Rankability + ": " + c.Quality.RankabilityReason
		}
		return c.Quality.Rankability
	}
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		switch bandForGamma(r) {
		case "green":
			return "dealer gamma stabilizing"
		case "red":
			return "dealer gamma amplifying"
		case "yellow":
			return "mixed dealer-gamma read"
		default:
			return "no usable dealer-gamma profile"
		}
	}
	if c.ZeroGamma != nil && c.GapPct != nil {
		switch bandForGamma(r) {
		case "green":
			return "spot >2% above gamma-zero"
		case "yellow":
			return "spot within +/-2% of gamma-zero"
		case "red":
			return "spot below gamma-zero"
		}
	}
	switch c.GammaSign {
	case "positive":
		return "dealer long-gamma; stabilizing"
	case "negative":
		return "dealer short-gamma; amplifying"
	default:
		return "sweep produced no signed profile"
	}
}

func breadthBandReason(r rpc.RegimeBreadth) string {
	if r.Status == rpc.RegimeStatusComputing {
		return "cold-start refresh in progress"
	}
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return "breadth engine offline"
	}
	switch bandForBreadth(r) {
	case "green":
		return ">55% (50d)"
	case "yellow":
		return "40-55% (50d)"
	case "red":
		return "<40% (50d)"
	default:
		return "breadth snapshot unavailable"
	}
}

func shortMetaReason(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if len(s) <= 96 {
		return s
	}
	return s[:93] + "..."
}

func gatewayAsOf(now time.Time, status, dataType, source string, qs ...*rpc.Quality) *rpc.RegimeAsOfSummary {
	source = ifEmpty(source, qualitySource(qs...))
	switch status {
	case rpc.RegimeStatusUnavailable, rpc.RegimeStatusError:
		return unavailableAsOf(source)
	case rpc.RegimeStatusComputing:
		return computingAsOf(source)
	}

	at := qualityTime(now, qs...)
	label := "live"
	freshness := rpc.FreshnessLive
	switch dataType {
	case rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen:
		label = "15m delayed"
		freshness = regimeFreshnessDelayed
	case rpc.MarketDataFrozen:
		label = "frozen"
		freshness = rpc.FreshnessFrozen
	case rpc.MarketDataLive, "":
		if status == rpc.RegimeStatusStale {
			label = "stale"
			freshness = rpc.FreshnessFrozen
		}
	default:
		label = dataType
		freshness = dataType
	}
	return asOfSummary(label, freshness, source, at, "", now)
}

func officialRowAsOf(now time.Time, dateText, source, status string) *rpc.RegimeAsOfSummary {
	if status == rpc.RegimeStatusUnavailable || status == rpc.RegimeStatusError {
		return unavailableAsOf(source)
	}
	date, ok := parseRegimeDate(dateText)
	if !ok {
		if status == rpc.RegimeStatusComputing {
			return computingAsOf(source)
		}
		return unavailableAsOf(source)
	}
	ageDays := calendarAgeDays(date, now)
	label := "close today"
	if ageDays == 1 {
		label = "close D-1"
	} else if ageDays > 1 {
		label = fmt.Sprintf("%dd old", ageDays)
	}
	return asOfSummary(label, regimeFreshnessDailyClose, source, date, date.Format("2006-01-02"), now)
}

func gammaAsOf(now time.Time, r rpc.RegimeGammaZero) *rpc.RegimeAsOfSummary {
	switch r.Status {
	case rpc.RegimeStatusComputing:
		return computingAsOf("SPY+SPX dealer gamma cache")
	case rpc.RegimeStatusUnavailable, rpc.RegimeStatusError:
		return unavailableAsOf("SPY+SPX dealer gamma cache")
	}
	if r.Envelope.Result == nil || r.Envelope.Result.AsOf.IsZero() {
		return unavailableAsOf("SPY+SPX dealer gamma cache")
	}
	return cachedAsOf(now, r.Envelope.Result.AsOf, "SPY+SPX dealer gamma cache")
}

func breadthAsOf(now time.Time, r rpc.RegimeBreadth) *rpc.RegimeAsOfSummary {
	source := ifEmpty(r.Envelope.Source, "SPX constituent breadth cache")
	if r.Envelope.Method != "" {
		source += " (" + r.Envelope.Method + ")"
	}
	switch r.Status {
	case rpc.RegimeStatusComputing:
		return computingAsOf(source)
	case rpc.RegimeStatusUnavailable, rpc.RegimeStatusError:
		return unavailableAsOf(source)
	}
	if r.Envelope.AsOf.IsZero() {
		return unavailableAsOf(source)
	}
	return cachedAsOf(now, r.Envelope.AsOf, source)
}

func cachedAsOf(now, at time.Time, source string) *rpc.RegimeAsOfSummary {
	label := "cached " + at.In(now.Location()).Format("15:04")
	if days := calendarAgeDays(at, now); days > 0 {
		label = fmt.Sprintf("%dd old", days)
	}
	return asOfSummary(label, regimeFreshnessCached, source, at, "", now)
}

func unavailableAsOf(source string) *rpc.RegimeAsOfSummary {
	return &rpc.RegimeAsOfSummary{
		Label:     "unavailable",
		Freshness: regimeFreshnessUnavailable,
		Source:    source,
	}
}

func computingAsOf(source string) *rpc.RegimeAsOfSummary {
	return &rpc.RegimeAsOfSummary{
		Label:     "computing",
		Freshness: regimeFreshnessComputing,
		Source:    source,
	}
}

func asOfSummary(label, freshness, source string, at time.Time, date string, now time.Time) *rpc.RegimeAsOfSummary {
	out := &rpc.RegimeAsOfSummary{
		Label:     label,
		Time:      at,
		Date:      date,
		Freshness: freshness,
		Source:    source,
	}
	if !at.IsZero() && !now.IsZero() && now.After(at) {
		out.AgeSeconds = int64(now.Sub(at).Seconds())
	}
	return out
}

func qualityTime(fallback time.Time, qs ...*rpc.Quality) time.Time {
	for _, q := range qs {
		if q != nil && !q.AsOf.IsZero() {
			return q.AsOf
		}
	}
	return fallback
}

func qualitySource(qs ...*rpc.Quality) string {
	for _, q := range qs {
		if q != nil && q.Source != "" {
			return q.Source
		}
	}
	return ""
}

func parseRegimeDate(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	return t, err == nil
}

func calendarAgeDays(at, now time.Time) int {
	if at.IsZero() || now.IsZero() {
		return 0
	}
	loc := now.Location()
	a := at.In(loc)
	n := now.In(loc)
	ad := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
	nd := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	days := int(nd.Sub(ad).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}
