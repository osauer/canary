package regimerows

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Spec thresholds, baked from docs/docs/internals/regime-dashboard.md. The
// daemon stays threshold-free (the spec calls these user-tunable) — the
// renderer is the right home for defaults. When a real user asks for
// tuning, lift these to env vars or a config file; until then, YAGNI.
const (
	vixRatioGreen      = 0.92 // VIX/VIX3M below this is healthy contango
	vixRatioRed        = 1.00 // above this is backwardation
	vvixYellow         = 90.0
	vvixRed            = 110.0
	hyOASYellow        = 4.0
	hyOASRed           = 5.5
	hyOASWidenYellow   = 0.50 // percentage points over ~20 observations
	hyOASWidenRed      = 1.00
	fundingYellowBps   = 25.0
	fundingRedBps      = 75.0
	usdJpyMoveYellow   = 1.0  // % weekly yen strengthening
	usdJpyMoveRed      = 2.0  // % weekly yen strengthening
	hygSpyNearHighProx = 0.97 // SPY ≥ 0.97 × 52-w high = "near highs"
	breadthGreen       = 55.0 // % SPX constituents above 50-DMA
	breadthRed         = 40.0 // < this with SPX at highs is classic divergence
)

// Band is the classified state of one indicator. unranked covers
// computing / unavailable / error — these don't contribute to the
// composite count (user-confirmed decision; honest about coverage).
type Band int

// Indicator bands. BandUnranked represents unavailable or non-comparable
// evidence and does not contribute to ranked counts.
const (
	BandUnranked Band = iota
	BandGreen
	BandYellow
	BandRed
)

// Row is the rendered shape of one indicator: a fixed-width row
// the layout assembles top-to-bottom. Kept as a struct so the composite
// counter and the row renderer share one source of truth.
type Row struct {
	Name      string // "VIX/VIX3M"
	Cluster   string // cluster token for composite de-duplication
	Value     string // value cell, plain; dim-suffix attached at render time
	AsOf      string // compact freshness badge ("live", "close D-1", "cached 11:42")
	Band      Band   // for glyph + band-word coloring
	Reason    string // parenthetical band justification ("<0.92 contango")
	Status    string // rpc.RegimeStatus*; drives glyph for unranked + stale suffix
	StateNote string // override for unranked / loading rows ("42s ETA · 40% done")
	// quality is the row's compact provenance tag, e.g. "· est 18s" or
	// "· modelled". Empty string when every value on the row came from
	// a firm-live tick — the default case stays unannotated to keep the
	// rendering uncluttered. Each row builder computes this from the
	// rpc.Quality pointers attached to the values it consumed.
	Quality string
	// streak summarises the consecutive-sessions-in-band counter on a
	// short inline marker like "day 3" — appended next to the band word
	// so a reader sees "yellow · day 3" without scanning sideways. The
	// streak counter is daemon-classified using the spec defaults; a
	// renderer with custom thresholds reads the raw value cell instead.
	Streak string
}

func streakMarker(s *rpc.StreakInfo) string {
	if s == nil || s.Sessions <= 0 {
		return ""
	}
	return fmt.Sprintf("day %d", s.Sessions)
}

// qualityTag compresses a set of *rpc.Quality pointers into a short
// suffix string for the row's right edge. Returns "" when every
// attached Quality is firm-live (the default-case row reads as fresh
// with no extra ink). Picks the worst-of across attached values:
//
//   - any modelled/proxy → "· modelled"  (e.g. gamma's BS-sweep γ-zero)
//   - any derived/estimate → "· est"     (e.g. SPY 52w-high fallback)
//   - any firm/frozen → "· frozen"       (gateway-frozen tick)
//   - otherwise → ""                     (all firm/live)
//
// Age suffix appends when the worst Quality.AsOf is older than the
// per-class threshold. Tick-data (est/frozen) decays over seconds, so
// any age > 5 s surfaces as "· est 18s". Modelled outputs are stable
// over the snapshot horizon — a 37 s old BS-sweep result is no more
// stale than a 1 s old one — so the age suffix only fires past 5 min,
// as a stale-model warning rather than a freshness clock.
func qualityTag(now time.Time, qs ...*rpc.Quality) string {
	worstAt := time.Time{}
	rank := func(q *rpc.Quality) int {
		if q == nil {
			return 0
		}
		switch {
		case q.FreshnessClass == rpc.FreshnessModelled || q.Confidence == rpc.ConfidenceProxy:
			return 5
		case q.FreshnessClass == rpc.FreshnessDerived && q.Confidence == rpc.ConfidenceFirm:
			return 2
		case q.FreshnessClass == rpc.FreshnessDerived || q.Confidence == rpc.ConfidenceEstimate:
			return 4
		case q.FreshnessClass == rpc.FreshnessFrozen:
			return 3
		case q.FreshnessClass == rpc.FreshnessLive:
			return 1
		}
		return 0
	}
	worstRank := 0
	for _, q := range qs {
		if r := rank(q); r > worstRank {
			worstRank = r
			worstAt = q.AsOf
		}
	}
	type tagSpec struct {
		label     string
		threshold time.Duration
		ageText   func(time.Duration) string
	}
	seconds := func(d time.Duration) string { return fmt.Sprintf("%ds", int(d.Seconds())) }
	specs := map[int]tagSpec{
		4: {"· est", 5 * time.Second, seconds},
		// The threshold sits above the gamma compute's own wall clock (SPY and
		// SPX run serially, ~14 min) because the model ages from its input
		// spot — at 5 min the badge fired on every result at birth and stopped
		// carrying meaning.
		5: {"· modelled", 20 * time.Minute, modelledAgeText},
		3: {"· frozen", 5 * time.Second, seconds},
		2: {"· official", 36 * time.Hour, func(d time.Duration) string { return fmt.Sprintf("%dd old", int(d.Hours()/24)) }},
	}
	s, ok := specs[worstRank]
	if !ok {
		return ""
	}
	if !worstAt.IsZero() {
		if age := now.Sub(worstAt); age > s.threshold {
			return s.label + " " + s.ageText(age)
		}
	}
	return s.label
}

// modelledAgeText spells out "min" because this table's other units are
// seconds and days, so a bare "m" beside "1d old" reads as months. It rolls to
// hours for an overnight model: "525 min old" is arithmetic, not a reading.
func modelledAgeText(d time.Duration) string {
	if m := int(d.Minutes()); m < 120 {
		return fmt.Sprintf("%d min old", m)
	}
	return fmt.Sprintf("%dh old", int(d.Hours()))
}

// glyph picks the row badge from the row's band and status. Ranked rows
// use a filled circle colored by band; unranked rows use a distinct
// glyph per failure mode so the reader can scan the column.

func asOfLabel(meta *rpc.RegimeAsOfSummary, status string) string {
	if meta != nil && meta.Label != "" {
		return meta.Label
	}
	switch status {
	case rpc.RegimeStatusOK:
		return "live"
	case rpc.RegimeStatusStale:
		return "stale"
	case rpc.RegimeStatusComputing:
		return "computing"
	case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		return "unavailable"
	default:
		return "—"
	}
}

// ----------------------------------------------------------------------------
// Per-indicator row builders. Each one consumes a raw RPC row and
// emits the (name, value, band, reason, status) tuple the renderer
// lays out. Threshold derivation lives here, with the spec defaults
// from the top of the file.

func rowVIXTerm(now time.Time, r rpc.RegimeVIXTerm) Row {
	row := Row{Name: "VIX/VIX3M", Cluster: "equity_vol", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Ratio == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "ratio unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "VIX/VIX3M not in this read")
		return row
	}
	row.Value = fmt.Sprintf("%.3f  (%s / %s)", *r.Ratio, floatPtr(r.VIX, 2), floatPtr(r.VIX3M, 2))
	row.Quality = qualityTag(now, r.VIXQuality, r.VIX3MQuality)
	switch {
	case *r.Ratio < vixRatioGreen:
		row.Band, row.Reason = BandGreen, "vol curve in contango"
	case *r.Ratio < vixRatioRed:
		row.Band, row.Reason = BandYellow, "vol curve flattening"
	default:
		row.Band, row.Reason = BandRed, "vol curve inverted"
	}
	return row
}

func rowVolOfVol(now time.Time, r rpc.RegimeVolOfVol) Row {
	row := Row{Name: "VVIX", Cluster: "equity_vol", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.Last == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "VVIX unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "VVIX not in this read")
		return row
	}
	row.Value = fmt.Sprintf("%.1f", *r.Last)
	if r.Change20D != nil {
		row.Value += fmt.Sprintf("  %+.1f%%/20d", *r.Change20D)
	}
	row.Quality = qualityTag(now, r.ValueQuality)
	switch {
	case *r.Last < vvixYellow:
		row.Band, row.Reason = BandGreen, "vol-of-vol calm"
	case *r.Last < vvixRed:
		row.Band, row.Reason = BandYellow, "vol-of-vol elevated"
	default:
		row.Band, row.Reason = BandRed, "vol-of-vol shock"
	}
	return row
}

func rowHYGSPY(now time.Time, r rpc.RegimeHYGSPYDivergence) Row {
	row := Row{Name: "HYG vs SPY", Cluster: "credit", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "HYG/SPY unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "credit proxy not in this read")
		return row
	}
	if r.HYGPrice == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "HYG price unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "HYG not in this read")
		return row
	}
	// Value cell: HYG vs its 50dma is the structural signal; SPY's
	// distance from the 52w high is the modifier (yellow band trigger).
	hyg50 := "—"
	if r.HYG50DMA != nil {
		hyg50 = fmt.Sprintf("%.2f", *r.HYG50DMA)
	}
	row.Value = fmt.Sprintf("HYG %.2f vs 50d %s", *r.HYGPrice, hyg50)
	row.Quality = qualityTag(now, r.HYGQuality, r.HYG50DMAQuality, r.SPYQuality, r.SPY52WHighQuality)
	// Banding. HYG below 50dma while SPY is near highs is the credit-
	// equity divergence this row exists to catch. Streaks carry the
	// "is this sustained?" context; the band itself should not hide the
	// current divergence.
	switch {
	case r.HYG50DMA == nil:
		row.Band, row.Reason = BandUnranked, "need HYG 50-day average"
	case *r.HYGPrice >= *r.HYG50DMA:
		row.Band, row.Reason = BandGreen, "credit holding above trend"
	case r.SPY52WHigh != nil && r.SPYPrice != nil && *r.SPYPrice >= hygSpyNearHighProx**r.SPY52WHigh:
		row.Band, row.Reason = BandRed, "credit lagging while SPY is near highs"
	case r.SPY52WHigh != nil:
		row.Band, row.Reason = BandYellow, "credit slipped below trend"
	default:
		// HYG < 50dma + SPY 52w high missing: we can't tell whether
		// the divergence is "near highs" or not. Surface honestly
		// rather than guess.
		row.Band, row.Reason = BandUnranked, "need SPY high anchor"
	}
	return row
}

func rowCreditSpreads(now time.Time, r rpc.RegimeCreditSpreads) Row {
	row := Row{Name: "HY/IG OAS", Cluster: "credit", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.HYOAS == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "OAS unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "official spreads not in this read")
		return row
	}
	ig := "—"
	if r.IGOAS != nil {
		ig = fmt.Sprintf("%.2f", *r.IGOAS)
	}
	row.Value = fmt.Sprintf("HY %.2f / IG %s", *r.HYOAS, ig)
	if r.HY20DChange != nil {
		row.Value += fmt.Sprintf("  Δ20d %+.2f", *r.HY20DChange)
	}
	row.Quality = qualityTag(now, r.HYOASQuality, r.IGOASQuality, r.SpreadQuality)
	switch {
	case *r.HYOAS >= hyOASRed || (r.HY20DChange != nil && *r.HY20DChange >= hyOASWidenRed):
		row.Band, row.Reason = BandRed, "cash credit stress"
	case *r.HYOAS >= hyOASYellow || (r.HY20DChange != nil && *r.HY20DChange >= hyOASWidenYellow):
		row.Band, row.Reason = BandYellow, "cash spreads elevated/widening"
	default:
		row.Band, row.Reason = BandGreen, "cash spreads calm"
	}
	return row
}

func rowFundingStress(now time.Time, r rpc.RegimeFundingStress) Row {
	row := Row{Name: "funding spread", Cluster: "funding", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable || r.SpreadBps == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "funding unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "official funding not in this read")
		return row
	}
	cp := "—"
	if r.CP3M != nil {
		cp = fmt.Sprintf("%.2f", *r.CP3M)
	}
	tb := "—"
	if r.TBill3M != nil {
		tb = fmt.Sprintf("%.2f", *r.TBill3M)
	}
	row.Value = fmt.Sprintf("%.0fbp  CP %s / bills %s", *r.SpreadBps, cp, tb)
	row.Quality = qualityTag(now, r.CP3MQuality, r.TBill3MQuality, r.SpreadQuality)
	switch {
	case *r.SpreadBps < fundingYellowBps:
		row.Band, row.Reason = BandGreen, "funding calm"
	case *r.SpreadBps < fundingRedBps:
		row.Band, row.Reason = BandYellow, "funding spread wider"
	default:
		row.Band, row.Reason = BandRed, "funding stress"
	}
	return row
}

func rowUSDJPY(now time.Time, r rpc.RegimeUSDJPY) Row {
	row := Row{Name: "USD/JPY", Cluster: "fx_carry", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status == rpc.RegimeStatusError || r.Status == rpc.RegimeStatusUnavailable {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "no FX tick"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "FX not in this read")
		return row
	}
	if r.Last == nil {
		if row.Status == "" {
			row.Status = rpc.RegimeStatusUnavailable
		}
		row.Value = "—"
		row.StateNote = "FX tick unavailable"
		row.Reason = shortUnavailableReason(r.ErrorMessage, "FX not in this read")
		return row
	}
	wkly := "—"
	if r.WeeklyChange != nil {
		sign := "+"
		if *r.WeeklyChange < 0 {
			sign = ""
		}
		wkly = fmt.Sprintf("%s%.2f%%/wk", sign, *r.WeeklyChange)
	}
	row.Value = fmt.Sprintf("%.4f  %s", *r.Last, wkly)
	row.Quality = qualityTag(now, r.LastQuality, r.Close7DAgoQuality)
	// Spec: yen strengthening (USD/JPY *falling*) is the risk signal.
	// Convention: WeeklyChange negative = yen strengthening.
	if r.WeeklyChange == nil {
		row.Band, row.Reason = BandUnranked, "need weekly move"
		return row
	}
	move := -*r.WeeklyChange // positive when yen strengthening
	switch {
	case move < usdJpyMoveYellow:
		row.Band, row.Reason = BandGreen, "carry stable"
	case move < usdJpyMoveRed:
		row.Band, row.Reason = BandYellow, "yen strengthening"
	default:
		row.Band, row.Reason = BandRed, "yen strengthening fast"
	}
	return row
}

// gammaRowLabel returns the regime row's indicator name, varying with
// the underlying gamma envelope's Scope so combined runs don't claim
// to be SPY. Falls back to "SPY γ-zero" for envelopes without a Scope
// (older or incomplete envelopes) or when no Result has landed yet; the
// fallback label preserves the established rendering contract.
func gammaRowLabel(r rpc.RegimeGammaZero) string {
	res := r.Envelope.Result
	if res == nil {
		// No envelope yet (cold / computing / error). Regime always
		// requests the combined SPY+SPX gamma — label accordingly so
		// the row name doesn't silently flip from "γ-zero (SPY+SPX)"
		// to "SPY γ-zero" depending on whether a compute has landed.
		return "γ-zero (SPY+SPX)"
	}
	switch res.Scope {
	case rpc.GammaZeroScopeSPX:
		return "SPX γ-zero"
	case rpc.GammaZeroScopeCombined:
		return "γ-zero (SPY+SPX)"
	default:
		return "SPY γ-zero"
	}
}

func rowGamma(now time.Time, r rpc.RegimeGammaZero) Row {
	row := Row{Name: gammaRowLabel(r), Cluster: "dealer_gamma", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	switch r.Status {
	case rpc.RegimeStatusComputing:
		return rowGammaComputing(row, r)
	case rpc.RegimeStatusError:
		row.Value = ""
		row.StateNote = ifNonEmpty(r.Envelope.Error, "compute failed")
		row.Reason = "retry on the next regime call"
		return row
	case rpc.RegimeStatusUnavailable:
		return rowGammaUnavailable(row, r)
	case rpc.RegimeStatusOK:
		return rowGammaOK(now, row, r)
	}
	row.Value = "—"
	row.StateNote = string(r.Status)
	return row
}

func rowGammaComputing(row Row, r rpc.RegimeGammaZero) Row {
	row.Value = ""
	eta := r.Envelope.EtaSeconds
	note := fmt.Sprintf("building  %ds ETA", eta)
	if r.Envelope.Progress > 0 {
		note += fmt.Sprintf(" · %d%%", r.Envelope.Progress)
	}
	// If the in-flight compute is a retry of a recent failure, surface
	// the prior error context instead of a clean first-call message.
	if r.Envelope.RetryOfErrorAt != nil && r.Envelope.RetryOfErrorSummary != "" {
		row.Reason = "retrying last failed gamma refresh"
	} else {
		row.Reason = "building dealer-gamma snapshot"
	}
	row.StateNote = note
	return row
}

func rowGammaUnavailable(row Row, r rpc.RegimeGammaZero) Row {
	row.Value = ""
	row.StateNote = "unavailable"
	if r.Envelope.Status == rpc.GammaZeroStatusCold {
		row.Reason = "no gamma snapshot yet"
	} else {
		row.Reason = "gamma snapshot unavailable"
	}
	return row
}

func rowGammaOK(now time.Time, row Row, r rpc.RegimeGammaZero) Row {
	c := r.Envelope.Result
	if c == nil {
		row.Value = "—"
		row.StateNote = "envelope missing payload"
		return row
	}
	gammaRankable := gammaRowExplicitlyRankable(c)
	row.Reason = rowGammaInitialReason(c, gammaRankable)
	// Gamma's two scalars are always modelled (zero_gamma via the BS
	// sweep) or derived (|Γ|·OI sum from observed OI+IV); the row will
	// carry "· modelled" regardless of ranking.
	row.Quality = qualityTag(now, r.ZeroGammaQuality, r.GammaTotalAbsQuality)
	if c.Scope == rpc.GammaZeroScopeCombined && len(c.PerIndex) > 0 {
		return rowGammaCombined(row, c, gammaRankable)
	}
	if c.ZeroGamma != nil && c.GapPct != nil {
		return rowGammaCrossing(row, r, c, gammaRankable)
	}
	return rowGammaSignedProfile(row, c, gammaRankable)
}

func rowGammaInitialReason(c *rpc.GammaZeroComputed, gammaRankable bool) string {
	if !gammaRankable {
		return regimeGammaNoVoteReason(c)
	}
	if c.Quality != nil && c.Quality.RankabilityReason != "" {
		return regimeGammaQualityReason(c.Quality)
	}
	return ""
}

func rowGammaCombined(row Row, c *rpc.GammaZeroComputed, gammaRankable bool) Row {
	row.Value = formatRegimeGammaAgreement(c)
	if c.GammaTotalAbs > 0 {
		row.Value += fmt.Sprintf("  |GEX| %.1fbn", c.GammaTotalAbs/1e9)
	}
	if gammaRankable {
		row.Band = rankableGammaCombinedRegimeBand(c)
	} else {
		row.Band = BandUnranked
	}
	switch row.Band {
	case BandGreen:
		row.Reason = regimeGammaCombinedReason(c, BandGreen)
	case BandRed:
		row.Reason = regimeGammaCombinedReason(c, BandRed)
	case BandYellow:
		row.Reason = regimeGammaCombinedReason(c, BandYellow)
	default:
		if row.Reason == "" {
			row.Reason = "no usable dealer-gamma profile"
		}
	}
	return row
}

func rowGammaCrossing(row Row, r rpc.RegimeGammaZero, c *rpc.GammaZeroComputed, gammaRankable bool) Row {
	sign := "+"
	if *c.GapPct < 0 {
		sign = ""
	}
	row.Value = fmt.Sprintf("spot %.2f → γ-zero %.2f  %s%.1f%%",
		c.SpotUnderlying, *c.ZeroGamma, sign, *c.GapPct)
	// Annotate horizon disagreement when the renderer would otherwise
	// mask it. "diverge" is the high-information case: near vs term
	// γ-zero straddle spot, meaning the headline cancels the real signal.
	if note := horizonAgreementNote(r.HorizonAgreement, c); note != "" {
		row.Value += "  " + note
	}
	row.Band = BandUnranked
	if !gammaRankable {
		return row
	}
	switch rpc.GammaRegimeFromGap(c.GapPct) {
	case "long_gamma":
		row.Band, row.Reason = BandGreen, fmt.Sprintf("spot >%.0f%% above γ-zero", rpc.GammaTransitionGapPct)
	case "transition_gamma":
		row.Band, row.Reason = BandYellow, fmt.Sprintf("spot within ±%.0f%% of γ-zero", rpc.GammaTransitionGapPct)
	default:
		row.Band, row.Reason = BandRed, "spot below γ-zero"
	}
	return row
}

func rowGammaSignedProfile(row Row, c *rpc.GammaZeroComputed, gammaRankable bool) Row {
	// No crossing. GammaSign tells us which side of zero the whole swept
	// profile landed on. Magnitude is rendered only when non-zero so an
	// empty profile or v2 daemon does not paint a misleading "$0.0bn".
	mag := ""
	if c.GammaTotalAbs > 0 {
		mag = fmt.Sprintf("  |GEX| %.1fbn", c.GammaTotalAbs/1e9)
	}
	spotPrefix := fmt.Sprintf("spot %.2f · ", c.SpotUnderlying)
	switch c.GammaSign {
	case "positive":
		row.Value = fmt.Sprintf("%slong-γ%s", spotPrefix, mag)
		if gammaRankable {
			row.Band = BandGreen
			row.Reason = "dealer long-γ · stabilizing"
		} else {
			row.Band = BandUnranked
		}
	case "negative":
		row.Value = fmt.Sprintf("%sshort-γ%s", spotPrefix, mag)
		if gammaRankable {
			row.Band = BandRed
			row.Reason = "dealer short-γ · amplifying"
		} else {
			row.Band = BandUnranked
		}
	default:
		row.Value = fmt.Sprintf("spot %.2f", c.SpotUnderlying)
		row.Band = BandUnranked
		row.Reason = "sweep produced no signed profile"
	}
	return row
}

func formatRegimeGammaAgreement(c *rpc.GammaZeroComputed) string {
	switch {
	case c == nil:
		return "dealer gamma unavailable"
	case c.Summary != nil && c.Summary.Regime == "long_gamma":
		return "long-γ (stabilizing)"
	case c.Summary != nil && c.Summary.Regime == "short_gamma":
		return "short-γ (amplifying)"
	}
	switch c.RegimeAgreement {
	case "agree:long-gamma":
		return "long-γ (stabilizing)"
	case "agree:short-gamma":
		return "short-γ (amplifying)"
	case "disagree":
		return "mixed dealer-gamma read"
	default:
		value := formatRegimeAgreement(c)
		value = strings.ReplaceAll(value, "long-γ (stabilizing regime)", "long-γ (stabilizing)")
		value = strings.ReplaceAll(value, "short-γ (amplifying regime)", "short-γ (amplifying)")
		value = strings.ReplaceAll(value, " · no γ-zero transition found in sweep", "")
		value = strings.ReplaceAll(value, " · SPY/SPX agree", "")
		value = strings.ReplaceAll(value,
			" (DISAGREEMENT — model regimes differ; use per-index below as primary)",
			"")
		return strings.TrimSpace(value)
	}
}

func regimeGammaCombinedReason(c *rpc.GammaZeroComputed, band Band) string {
	noCrossing := c != nil && c.Summary != nil && c.Summary.ZeroGammaStatus == "none_in_window"
	switch band {
	case BandGreen:
		if noCrossing {
			return "long-γ across sweep; dealer hedging can dampen moves"
		}
		return "dealer gamma stabilizing"
	case BandRed:
		if noCrossing {
			return "short-γ across sweep; dealer hedging can amplify moves"
		}
		return "dealer gamma amplifying"
	case BandYellow:
		return "mixed dealer-gamma read"
	default:
		return "dealer-gamma profile not usable"
	}
}

func gammaCombinedRegimeBand(c *rpc.GammaZeroComputed) Band {
	if c != nil && c.Quality != nil && c.Quality.Rankability != rpc.GammaRankabilityRankable {
		return BandUnranked
	}
	type weightedBand struct {
		band   Band
		weight float64
	}
	var bands []weightedBand
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil {
			continue
		}
		b := gammaSingleRegimeBand(sub)
		if b != BandUnranked {
			bands = append(bands, weightedBand{band: b, weight: gammaPerIndexWeight(key, sub)})
		}
	}
	if len(bands) == 0 {
		return BandUnranked
	}
	first := bands[0].band
	total := 0.0
	redWeight := 0.0
	for _, b := range bands[1:] {
		if b.band != first {
			first = BandUnranked
		}
	}
	for _, b := range bands {
		total += b.weight
		if b.band == BandRed {
			redWeight += b.weight
		}
	}
	if first != BandUnranked {
		return first
	}
	if total > 0 && redWeight/total >= 0.5 {
		return BandRed
	}
	return BandYellow
}

func rankableGammaCombinedRegimeBand(c *rpc.GammaZeroComputed) Band {
	if !gammaRowExplicitlyRankable(c) {
		return BandUnranked
	}
	type weightedBand struct {
		band   Band
		weight float64
	}
	var bands []weightedBand
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if !gammaRowExplicitlyRankable(sub) {
			continue
		}
		b := gammaSingleRegimeBand(sub)
		if b != BandUnranked {
			bands = append(bands, weightedBand{band: b, weight: gammaPerIndexWeight(key, sub)})
		}
	}
	if len(bands) == 0 {
		return BandUnranked
	}
	first := bands[0].band
	total := 0.0
	redWeight := 0.0
	for _, b := range bands[1:] {
		if b.band != first {
			first = BandUnranked
		}
	}
	for _, b := range bands {
		total += b.weight
		if b.band == BandRed {
			redWeight += b.weight
		}
	}
	if first != BandUnranked {
		return first
	}
	if total > 0 && redWeight/total >= 0.5 {
		return BandRed
	}
	return BandYellow
}

func gammaPerIndexWeight(key string, c *rpc.GammaZeroComputed) float64 {
	if c != nil && c.GammaTotalAbs > 0 {
		return c.GammaTotalAbs
	}
	if key == "SPX" {
		return 100
	}
	return 1
}

func gammaSingleRegimeBand(c *rpc.GammaZeroComputed) Band {
	if c == nil {
		return BandUnranked
	}
	if c.Quality != nil && c.Quality.Rankability != rpc.GammaRankabilityRankable {
		return BandUnranked
	}
	if c.GapPct != nil {
		switch rpc.GammaRegimeFromGap(c.GapPct) {
		case "long_gamma":
			return BandGreen
		case "transition_gamma":
			return BandYellow
		default:
			return BandRed
		}
	}
	switch c.GammaSign {
	case "positive":
		return BandGreen
	case "negative":
		return BandRed
	default:
		return BandUnranked
	}
}

func gammaRowExplicitlyRankable(c *rpc.GammaZeroComputed) bool {
	return c != nil && c.Quality != nil && c.Quality.Rankability == rpc.GammaRankabilityRankable
}

func regimeGammaQualityReason(q *rpc.GammaSignalQuality) string {
	if q == nil {
		return "gamma quality unavailable"
	}
	switch q.Rankability {
	case rpc.GammaRankabilityRankable:
		if q.Freshness == "closed_session_cache" {
			return "cached gamma usable"
		}
		return "fresh enough for regime evidence"
	case rpc.GammaRankabilityContextOnly:
		if q.Freshness == "closed_session_context" {
			return "after-hours cached gamma; not a fresh market-structure read"
		}
		return gammaPlainQualityReason(q)
	case rpc.GammaRankabilityBlocked:
		return gammaPlainQualityReason(q)
	case rpc.GammaRankabilityUnavailable:
		return "gamma unavailable"
	default:
		return gammaPlainQualityReason(q)
	}
}

func regimeGammaNoVoteReason(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "gamma payload missing"
	}
	if c.Quality == nil {
		return "quality missing"
	}
	if gammaIsSPYProxy(c) {
		return "SPX unavailable; proxy gamma cannot confirm S&P"
	}
	return regimeGammaQualityReason(c.Quality)
}

func rowBreadth(now time.Time, r rpc.RegimeBreadth) Row {
	row := Row{Name: "SPX breadth", Cluster: "breadth", Status: r.Status, AsOf: asOfLabel(r.AsOf, r.Status), Streak: streakMarker(r.Streak)}
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		switch r.Status {
		case rpc.RegimeStatusUnavailable:
			row.StateNote = "unavailable"
			row.Reason = "no breadth snapshot yet"
		case rpc.RegimeStatusComputing:
			row.StateNote = "building"
			// ~60 min is the IBKR-pacing-limited cold-start cost
			// (60 historical-data requests per 10-min sliding window
			// × 503 names ≈ 85 min in the worst case; observed ~60).
			// Mention --foreground so the user knows how to keep the
			// daemon alive long enough to finish.
			row.Reason = "building breadth snapshot"
		default:
			row.StateNote = string(r.Status)
		}
		return row
	}
	v50 := r.Envelope.PctAbove50DMA
	v200 := r.Envelope.PctAbove200DMA
	row.Value = fmt.Sprintf("%.0f%% above 50d · %.0f%% above 200d", v50, v200)
	if r.NewHighsToday > 0 || r.NewLowsToday > 0 {
		row.Value += fmt.Sprintf("  net highs %+.1f%%", r.NetNewHighsPct)
	}
	row.Quality = qualityTag(now, r.ValueQuality)
	// Renderer caveat: spec red band also requires "SPX within 3% of
	// 52w high" but we don't have SPX 52w-high context inside this row.
	// Conservative call: report red on raw 50-DMA breadth only; do not
	// downgrade to yellow. The spec is most concerned about the
	// breadth collapse itself; the SPX-at-highs modifier sharpens the
	// signal but doesn't invent it.
	switch {
	case v50 >= breadthGreen:
		row.Band, row.Reason = BandGreen, "participation broad"
	case v50 >= breadthRed:
		row.Band, row.Reason = BandYellow, "participation narrowing"
	default:
		row.Band, row.Reason = BandRed, "participation weak"
	}
	return row
}

// ----------------------------------------------------------------------------
// --explain block: compact audit notes for humans. The daemon still carries
// long methodology prose, but the terminal view should explain thresholds,
// provenance, and reading posture without becoming a wall of dim text.

func ifNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func shortUnavailableReason(message, fallback string) string {
	if message == "" {
		return fallback
	}
	switch {
	case strings.Contains(message, "no security definition"), strings.Contains(message, "verified IBKR contract"):
		return "no verified IBKR contract"
	case strings.Contains(message, "entitlement"):
		return "check market-data entitlement"
	case strings.Contains(message, "no spot tick"), strings.Contains(message, "no tick"):
		return "gateway delivered no tick"
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return "fetch timed out"
	default:
		return fallback
	}
}

// horizonAgreementNote returns a short parenthetical for the gamma row
// when the horizon-bucketed γ-zero readings disagree with the combined
// headline. v4 enum: "all_long" / "all_short" / "all_transition"
// agree with the combined reading and don't need a note; the renderer
// stays silent. "diverge:0dte_vs_term" is the high-information case —
// 0DTE and term γ regimes disagree, which the combined headline can
// average over.
func horizonAgreementNote(agreement string, c *rpc.GammaZeroComputed) string {
	if c == nil {
		return ""
	}
	fmtBucket := func(p *float64, sign string) string {
		if p == nil {
			switch sign {
			case "positive":
				return "long"
			case "negative":
				return "short"
			default:
				return "—"
			}
		}
		return fmt.Sprintf("%.0f", *p)
	}
	switch agreement {
	case "diverge:0dte_vs_term":
		return fmt.Sprintf("(0DTE %s · term %s · diverge)",
			fmtBucket(c.ZeroGamma0DTE, c.GammaSign0DTE), fmtBucket(c.ZeroGammaTerm, c.GammaSignTerm))
	case "diverge:partial":
		return fmt.Sprintf("(0DTE %s · 1-7 %s · term %s · diverge)",
			fmtBucket(c.ZeroGamma0DTE, c.GammaSign0DTE),
			fmtBucket(c.ZeroGamma1to7, c.GammaSign1to7),
			fmtBucket(c.ZeroGammaTerm, c.GammaSignTerm))
	case "0dte_only":
		if c.ZeroGamma0DTE != nil {
			return fmt.Sprintf("(0DTE %.0f only · no 1-7 or term crossing)", *c.ZeroGamma0DTE)
		}
		return fmt.Sprintf("(0DTE %s only · no 1-7 or term signal)", fmtBucket(nil, c.GammaSign0DTE))
	case "1to7_only":
		if c.ZeroGamma1to7 != nil {
			return fmt.Sprintf("(1-7 %.0f only · no 0DTE or term crossing)", *c.ZeroGamma1to7)
		}
		return fmt.Sprintf("(1-7 %s only · no 0DTE or term signal)", fmtBucket(nil, c.GammaSign1to7))
	case "term_only":
		if c.ZeroGammaTerm != nil {
			return fmt.Sprintf("(term %.0f only · no near crossing)", *c.ZeroGammaTerm)
		}
		return fmt.Sprintf("(term %s only · no near signal)", fmtBucket(nil, c.GammaSignTerm))
	}
	return ""
}

func gammaPlainQualityReason(q *rpc.GammaSignalQuality) string {
	if q == nil {
		return "quality unavailable"
	}
	reason := strings.TrimSpace(q.RankabilityReason)
	if reason == "" {
		switch q.Rankability {
		case rpc.GammaRankabilityRankable:
			return "signal quality passed"
		case rpc.GammaRankabilityUnavailable:
			return "no usable gamma payload"
		default:
			return "quality gate did not pass"
		}
	}
	if _, rest, ok := strings.Cut(reason, ": "); ok {
		reason = rest
	}
	switch {
	case reason == "market is closed; cached gamma is context only":
		return "market is closed; cached snapshot is context only"
	case strings.HasPrefix(reason, "SPX option chain unavailable; using SPY proxy:"):
		return "SPX unavailable; SPY is proxy/context only"
	case strings.HasPrefix(reason, "SPX slice is"):
		return strings.ReplaceAll(reason, "context only", "context")
	case reason == "all rankability gates passed":
		return "signal quality passed"
	default:
		return reason
	}
}

func gammaIsSPYProxy(c *rpc.GammaZeroComputed) bool {
	if c == nil || c.Scope != rpc.GammaZeroScopeSPY {
		return false
	}
	for _, d := range c.WarningDetails {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.Code)), "spx_unavailable:") {
			return true
		}
	}
	if len(c.WarningDetails) > 0 {
		return false
	}
	for _, code := range c.Warnings {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(code)), "spx_unavailable:") {
			return true
		}
	}
	return false
}

func floatPtr(p *float64, decimals int) string {
	if p == nil {
		return "—"
	}
	return fmt.Sprintf("%.*f", decimals, *p)
}

func formatSpotPrice(v float64) string {
	return fmt.Sprintf("$%.2f", v)
}

func formatRegimeAgreement(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "—"
	}
	switch c.RegimeAgreement {
	case "agree:long-gamma":
		return "long-γ (stabilizing regime)" + formatAgreementNoCrossingSuffix(c, "positive") + " · SPY/SPX agree"
	case "agree:short-gamma":
		return "short-γ (amplifying regime)" + formatAgreementNoCrossingSuffix(c, "negative") + " · SPY/SPX agree"
	case "agree:transition-gamma":
		spy := c.PerIndex["SPY"]
		spx := c.PerIndex["SPX"]
		if spy != nil && spy.ZeroGamma != nil && spx != nil && spx.ZeroGamma != nil {
			return fmt.Sprintf("SPY γ-zero %s · SPX γ-zero %s (both near transition · agreement)",
				formatSpotPrice(*spy.ZeroGamma), formatSpotPrice(*spx.ZeroGamma))
		}
		return "SPY and SPX both near γ-zero transition (agreement)"
	case "disagree":
		return formatRegimeDisagreement(c)
	}
	return "indeterminate (insufficient per-index data)"
}

func formatAgreementNoCrossingSuffix(c *rpc.GammaZeroComputed, sign string) string {
	if c == nil {
		return ""
	}
	for _, key := range []string{"SPY", "SPX"} {
		sub := c.PerIndex[key]
		if sub == nil || sub.ZeroGamma != nil || sub.GammaSign != sign {
			return ""
		}
	}
	return " · no γ-zero transition found in sweep"
}

// formatRegimeDisagreement renders the actionable case — one index
// stabilising while the other is amplifying. Names both sides so the
// reader knows which book sits where.
func formatRegimeDisagreement(c *rpc.GammaZeroComputed) string {
	spy := perIndexRegimeWord(c.PerIndex["SPY"])
	spx := perIndexRegimeWord(c.PerIndex["SPX"])
	return fmt.Sprintf("SPY %s · SPX %s (DISAGREEMENT — model regimes differ; use per-index below as primary)",
		spy, spx)
}

// perIndexRegimeWord turns a per-index result into a short label
// for the disagreement summary. Mirrors the RegimeAgreement classifier
// on the daemon side.
func perIndexRegimeWord(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "—"
	}
	if c.ZeroGamma != nil {
		return fmt.Sprintf("%s @ %s", gammaRegimeWord(c), formatSpotPrice(*c.ZeroGamma))
	}
	switch c.GammaSign {
	case "positive":
		return "long-γ"
	case "negative":
		return "short-γ"
	}
	return "—"
}

func gammaRegimeWord(c *rpc.GammaZeroComputed) string {
	if c == nil {
		return "unavailable"
	}
	if c.GapPct != nil {
		switch rpc.GammaRegimeFromGap(c.GapPct) {
		case "long_gamma":
			return "long-γ"
		case "transition_gamma":
			return "transition"
		default:
			return "short-γ"
		}
	}
	switch c.GammaSign {
	case "positive":
		return "long-γ"
	case "negative":
		return "short-γ"
	}
	return "transition"
}

// gammaHeaderForScope returns the renderer's section header — varies
// with Result.Scope so SPX-only and combined runs don't claim to be
// SPY. Falls back to the SPY title for an empty Scope so incomplete envelopes
// retain the established rendering contract.

// String returns the stable lowercase band label, or an empty string for an
// unranked value.
func (b Band) String() string {
	switch b {
	case BandGreen:
		return "green"
	case BandYellow:
		return "yellow"
	case BandRed:
		return "red"
	default:
		return ""
	}
}

// StreakMarker formats a positive consecutive-session count for a row.
func StreakMarker(s *rpc.StreakInfo) string {
	return streakMarker(s)
}

// QualityTag returns a compact worst-source provenance and age label.
func QualityTag(now time.Time, qualities ...*rpc.Quality) string {
	return qualityTag(now, qualities...)
}

// AsOfLabel returns the producer label when present and otherwise derives a
// conservative label from status.
func AsOfLabel(meta *rpc.RegimeAsOfSummary, status string) string {
	return asOfLabel(meta, status)
}

// VIXTerm builds the VIX term-structure presentation row.
func VIXTerm(now time.Time, row rpc.RegimeVIXTerm) Row {
	return rowVIXTerm(now, row)
}

// VolOfVol builds the volatility-of-volatility presentation row.
func VolOfVol(now time.Time, row rpc.RegimeVolOfVol) Row {
	return rowVolOfVol(now, row)
}

// HYGSPY builds the credit-versus-equity divergence presentation row.
func HYGSPY(now time.Time, row rpc.RegimeHYGSPYDivergence) Row {
	return rowHYGSPY(now, row)
}

// CreditSpreads builds the official credit-spread presentation row.
func CreditSpreads(now time.Time, row rpc.RegimeCreditSpreads) Row {
	return rowCreditSpreads(now, row)
}

// FundingStress builds the short-term funding-stress presentation row.
func FundingStress(now time.Time, row rpc.RegimeFundingStress) Row {
	return rowFundingStress(now, row)
}

// USDJPY builds the dollar-yen carry-stress presentation row.
func USDJPY(now time.Time, row rpc.RegimeUSDJPY) Row {
	return rowUSDJPY(now, row)
}

// GammaRowLabel returns the scope-aware label for a gamma row.
func GammaRowLabel(row rpc.RegimeGammaZero) string {
	return gammaRowLabel(row)
}

// Gamma builds the dealer-gamma presentation row with provenance disclosure.
func Gamma(now time.Time, row rpc.RegimeGammaZero) Row {
	return rowGamma(now, row)
}

// Breadth builds the S&P 500 breadth presentation row.
func Breadth(now time.Time, row rpc.RegimeBreadth) Row {
	return rowBreadth(now, row)
}

// IfNonEmpty returns value when non-empty and fallback otherwise.
func IfNonEmpty(value, fallback string) string {
	return ifNonEmpty(value, fallback)
}

// GammaPlainQualityReason summarizes gamma quality without terminal styling.
func GammaPlainQualityReason(quality *rpc.GammaSignalQuality) string {
	return gammaPlainQualityReason(quality)
}

// GammaIsSPYProxy reports whether an SPX-context result is backed by the SPY
// proxy rather than canonical SPX data.
func GammaIsSPYProxy(result *rpc.GammaZeroComputed) bool {
	return gammaIsSPYProxy(result)
}

// FormatSpotPrice formats a spot value for regime presentation.
func FormatSpotPrice(value float64) string {
	return formatSpotPrice(value)
}

// FormatRegimeAgreement summarizes SPY/SPX gamma agreement.
func FormatRegimeAgreement(result *rpc.GammaZeroComputed) string {
	return formatRegimeAgreement(result)
}

// GammaRegimeWord returns the plain-language gamma regime classification.
func GammaRegimeWord(result *rpc.GammaZeroComputed) string {
	return gammaRegimeWord(result)
}

// GammaSingleRegimeBand maps a single-scope gamma result to a presentation
// band.
func GammaSingleRegimeBand(result *rpc.GammaZeroComputed) Band {
	return gammaSingleRegimeBand(result)
}

// GammaCombinedRegimeBand maps a combined SPY/SPX gamma result to a
// presentation band.
func GammaCombinedRegimeBand(result *rpc.GammaZeroComputed) Band {
	return gammaCombinedRegimeBand(result)
}
