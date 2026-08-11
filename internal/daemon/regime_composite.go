package daemon

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// composite-building logic. Mirrors the CLI's tallyComposite +
// verdict() exactly so wire consumers (MCP, dashboard generators)

// verdictFloor is the minimum ranked-row count required to claim a
// verdict above "insufficient signal." Mirrors cli.verdictFloor —
// kept in sync by hand at v1; if a third reader appears, lift to a
// shared package.
const verdictFloor = 3

// buildRegimeComposite returns the same {verdict, green, yellow, red,
// ranked, unranked} rollup the CLI renders above the indicator rows,
// computed from the daemon-side classifiers. Always non-zero shape:
// when every row is unranked the verdict still surfaces honestly
// ("No ranked indicators — see rows below for state").
// buildRegimeComposite tallies the SERVED row bands (post-hysteresis, set by
// annotateRegimeMetadata) and fills the cluster counts through the shared
// rpc combination — the single copy of rescue/eligibility policy. Verdict is
// intentionally left empty here: the unified headline needs the lifecycle
// stage, so the handler assigns it via rpc.RegimeHeadline after
// BuildRegimeLifecycle runs.
func buildRegimeComposite(r *rpc.RegimeSnapshotResult) rpc.RegimeComposite {
	if r == nil {
		return rpc.RegimeComposite{Verdict: "No usable signal yet"}
	}
	rowBands := []string{
		r.VIXTermStructure.Band,
		r.VolOfVol.Band,
		r.HYGSPYDivergence.Band,
		r.CreditSpreads.Band,
		r.FundingStress.Band,
		r.USDJPY.Band,
		r.GammaZero.Band,
		r.Breadth.Band,
	}
	var c rpc.RegimeComposite
	for _, b := range rowBands {
		switch b {
		case "green":
			c.GreenCount++
			c.RankedCount++
		case "yellow":
			c.YellowCount++
			c.RankedCount++
		case "red":
			c.RedCount++
			c.RankedCount++
		default:
			c.UnrankedCount++
		}
	}
	rpc.ApplyRegimeClusterTallies(&c, rpc.BuildRegimeClusterBands(r))
	return c
}

func buildRegimeSummary(r *rpc.RegimeSnapshotResult) rpc.RegimeSummary {
	if r == nil {
		return rpc.RegimeSummary{
			Label:      "No ranked indicators",
			Evidence:   "0 ranked",
			PunchLine:  "No regime indicators produced a rankable reading.",
			Confidence: "low",
			NotAdvice:  regimeNotAdvice,
		}
	}
	c := r.Composite
	if c.Verdict == "" {
		c = buildRegimeComposite(r)
	}
	rows := regimeEvidenceRows(r)
	dominant := regimeDominantRisks(rows)
	return rpc.RegimeSummary{
		Label:             c.Verdict,
		Evidence:          regimeClusterEvidenceBalance(c),
		IndicatorEvidence: regimeEvidenceBalance(c),
		PunchLine:         regimePunchLine(rows),
		Confidence:        regimeEvidenceConfidence(c, rows),
		DominantRisks:     dominant,
		NotAdvice:         regimeNotAdvice,
	}
}

const regimeNotAdvice = "Regime read only; not investment advice or a trade recommendation."

type regimeEvidenceRow struct {
	scope   string
	name    string
	status  string
	band    string
	message string
}

func regimeEvidenceRows(r *rpc.RegimeSnapshotResult) []regimeEvidenceRow {
	if r == nil {
		return nil
	}
	return []regimeEvidenceRow{
		{
			scope:   "vix_term_structure",
			name:    "volatility term structure",
			status:  r.VIXTermStructure.Status,
			band:    bandForVIX(r.VIXTermStructure),
			message: r.VIXTermStructure.ErrorMessage,
		},
		{
			scope:   "vol_of_vol",
			name:    "vol-of-vol",
			status:  r.VolOfVol.Status,
			band:    bandForVolOfVol(r.VolOfVol),
			message: r.VolOfVol.ErrorMessage,
		},
		{
			scope:   "hyg_spy_divergence",
			name:    "ETF credit proxy",
			status:  r.HYGSPYDivergence.Status,
			band:    bandForHYGSPY(r.HYGSPYDivergence),
			message: r.HYGSPYDivergence.ErrorMessage,
		},
		{
			scope:   "credit_spreads",
			name:    "cash credit spreads",
			status:  r.CreditSpreads.Status,
			band:    bandForCreditSpreads(r.CreditSpreads),
			message: r.CreditSpreads.ErrorMessage,
		},
		{
			scope:   "funding_stress",
			name:    "funding spread",
			status:  r.FundingStress.Status,
			band:    bandForFundingStress(r.FundingStress),
			message: r.FundingStress.ErrorMessage,
		},
		{
			scope:   "usd_jpy",
			name:    "FX carry proxy",
			status:  r.USDJPY.Status,
			band:    bandForUSDJPY(r.USDJPY),
			message: r.USDJPY.ErrorMessage,
		},
		{
			scope:   "gamma_zero",
			name:    "dealer gamma",
			status:  r.GammaZero.Status,
			band:    bandForGamma(r.GammaZero),
			message: r.GammaZero.Envelope.Error,
		},
		{
			scope:   "breadth",
			name:    "breadth",
			status:  r.Breadth.Status,
			band:    bandForBreadth(r.Breadth),
			message: "",
		},
	}
}

func regimeEvidenceBalance(c rpc.RegimeComposite) string {
	var parts []string
	if c.GreenCount > 0 {
		parts = append(parts, fmt.Sprintf("%d green", c.GreenCount))
	}
	if c.YellowCount > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow", c.YellowCount))
	}
	if c.RedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d red", c.RedCount))
	}
	if c.UnrankedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked", c.UnrankedCount))
	}
	if len(parts) == 0 {
		return "0 ranked"
	}
	return strings.Join(parts, " / ")
}

func regimeClusterEvidenceBalance(c rpc.RegimeComposite) string {
	var parts []string
	if c.ClusterGreenCount > 0 {
		parts = append(parts, fmt.Sprintf("%d green %s", c.ClusterGreenCount, plural(c.ClusterGreenCount, "cluster", "clusters")))
	}
	if c.ClusterYellowCount > 0 {
		parts = append(parts, fmt.Sprintf("%d yellow %s", c.ClusterYellowCount, plural(c.ClusterYellowCount, "cluster", "clusters")))
	}
	if c.ClusterRedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d red %s", c.ClusterRedCount, plural(c.ClusterRedCount, "cluster", "clusters")))
	}
	if c.ClusterUnrankedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d unranked %s", c.ClusterUnrankedCount, plural(c.ClusterUnrankedCount, "cluster", "clusters")))
	}
	if len(parts) == 0 {
		return "0 ranked clusters"
	}
	return strings.Join(parts, " / ")
}

func regimeEvidenceConfidence(c rpc.RegimeComposite, rows []regimeEvidenceRow) string {
	switch {
	case c.ClusterRankedCount < verdictFloor:
		return "low"
	case c.ClusterUnrankedCount > 0 || hasStaleRegimeEvidence(rows):
		return "medium"
	default:
		return "high"
	}
}

func hasStaleRegimeEvidence(rows []regimeEvidenceRow) bool {
	for _, row := range rows {
		if row.status == rpc.RegimeStatusStale && row.band != "" {
			return true
		}
	}
	return false
}

func regimeDominantRisks(rows []regimeEvidenceRow) []string {
	var out []string
	for _, row := range rows {
		if row.band == "red" {
			out = append(out, row.name)
		}
	}
	return out
}

func regimePunchLine(rows []regimeEvidenceRow) string {
	if len(rows) == 0 {
		return "No regime indicators produced a rankable reading."
	}
	groups := map[string][]string{}
	for _, row := range rows {
		key := row.band
		if key == "" {
			key = unrankedEvidenceState(row.status)
		}
		groups[key] = append(groups[key], row.name)
	}
	var parts []string
	for _, spec := range []struct {
		key  string
		word string
	}{
		{"red", "stressed"},
		{"yellow", "mixed"},
		{"green", "constructive"},
		{"computing", "computing"},
		{"unavailable", "unavailable"},
		{"unranked", "unranked"},
	} {
		names := groups[spec.key]
		if len(names) == 0 {
			continue
		}
		verb := "is"
		if len(names) > 1 {
			verb = "are"
		}
		parts = append(parts, fmt.Sprintf("%s %s %s", joinHuman(names), verb, spec.word))
	}
	if len(parts) == 0 {
		return "No regime indicators produced a rankable reading."
	}
	if names := staleRegimeEvidenceNames(rows); len(names) > 0 {
		verb := "is"
		if len(names) > 1 {
			verb = "are"
		}
		parts = append(parts, fmt.Sprintf("%s %s ranked from stale data", joinHuman(names), verb))
	}
	return strings.Join(parts, "; ") + "."
}

func staleRegimeEvidenceNames(rows []regimeEvidenceRow) []string {
	var names []string
	for _, row := range rows {
		if row.status == rpc.RegimeStatusStale && row.band != "" {
			names = append(names, row.name)
		}
	}
	return names
}

func unrankedEvidenceState(status string) string {
	switch status {
	case rpc.RegimeStatusComputing:
		return "computing"
	case rpc.RegimeStatusError, rpc.RegimeStatusUnavailable:
		return "unavailable"
	default:
		return "unranked"
	}
}

func joinHuman(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

// regimeClusterScheduled reports a cluster whose non-fresh currency is its own
func regimeClusterScheduled(r rpc.RegimeSnapshotResult, cluster string) bool {
	_, ok := rpc.RegimeClusterScheduledContext(r, cluster)
	return ok
}

func buildRegimeWarnings(r *rpc.RegimeSnapshotResult) []rpc.RegimeWarning {
	if r == nil {
		return nil
	}
	rows := regimeEvidenceRows(r)
	warnings := make([]rpc.RegimeWarning, 0, len(rows))
	for _, row := range rows {
		if cluster := regimeWarningCluster(row.scope); cluster != "" && regimeClusterScheduled(*r, cluster) {
			continue
		}
		if row.status == rpc.RegimeStatusOK {
			continue
		}
		w, ok := warningForRegimeRow(row)
		if ok {
			warnings = append(warnings, w)
		}
	}
	if !regimeClusterScheduled(*r, "gamma") {
		if w, ok := warningForGammaRankability(r.GammaZero); ok {
			warnings = append(warnings, w)
		}
	}
	if w, ok := warningForVIX3MCrossCheck(r.VIXTermStructure); ok {
		warnings = append(warnings, w)
	}
	return warnings
}

// warningForVIX3MCrossCheck names what the two independent VIX3M sources
// established. A stale row alone reads as ordinary off-hours context, so the
// one case an operator must act on — a broker leg that has stopped tracking the
// index — needs to say so in its own words.
func warningForVIX3MCrossCheck(row rpc.RegimeVIXTerm) (rpc.RegimeWarning, bool) {
	switch row.VIX3MCrossCheck {
	case rpc.VIX3MCrossCheckDisagree:
		return rpc.RegimeWarning{
			Code:     "vix3m_source_disagreement",
			Scope:    "vix_term_structure",
			Severity: "error",
			Message: fmt.Sprintf("gateway VIX3M %s differs from Cboe's published %s close %s",
				formatVIX3MLevel(row.VIX3MGatewayLast), row.VIX3MOfficialDate, formatVIX3MLevel(row.VIX3MOfficial)),
			Impact: "The gateway leg is not the close it claims to be, so the vol cluster is overdue rather than not_due and cannot confirm. The served VIX3M is Cboe's close.",
			Action: "Check the VIX3M index market-data entitlement and the daemon's VIX3M contract id, then restart the daemon.",
		}, true
	case rpc.VIX3MCrossCheckUnverified:
		return rpc.RegimeWarning{
			Code:     "vix3m_official_close_unavailable",
			Scope:    "vix_term_structure",
			Severity: "warning",
			Message:  "no Cboe VIX3M close within one session of the last completed window",
			Impact:   "Nothing dates the off-window gateway leg, so the vol cluster is overdue rather than not_due.",
			Action:   "Retry once Cboe's official VIX3M daily file is reachable.",
		}, true
	default:
		return rpc.RegimeWarning{}, false
	}
}

func formatVIX3MLevel(v *float64) string {
	if v == nil {
		return "(absent)"
	}
	return strconv.FormatFloat(*v, 'f', 2, 64)
}

func regimeWarningCluster(scope string) string {
	switch scope {
	case "vix_term_structure", "vol_of_vol":
		return "vol"
	case "hyg_spy_divergence", "credit_spreads":
		return "credit"
	case "funding_stress":
		return "funding"
	case "usd_jpy":
		return "fx"
	case "gamma_zero":
		return "gamma"
	case "breadth":
		return "breadth"
	default:
		return ""
	}
}

func warningForGammaRankability(g rpc.RegimeGammaZero) (rpc.RegimeWarning, bool) {
	c := g.Envelope.Result
	if c == nil {
		return rpc.RegimeWarning{}, false
	}
	if c.Quality == nil {
		return rpc.RegimeWarning{
			Code:     "gamma_zero_quality_missing",
			Scope:    "gamma_zero",
			Severity: "warning",
			Message:  "dealer gamma quality missing",
			Impact:   "dealer gamma is shown for awareness but is not a market-structure signal in this snapshot.",
			Action:   "Refresh with a gamma build that emits rankability quality before relying on the gamma read.",
		}, true
	}
	if c.Quality.Rankability == rpc.GammaRankabilityRankable {
		return rpc.RegimeWarning{}, false
	}
	severity := "warning"
	if c.Quality.Rankability == rpc.GammaRankabilityContextOnly {
		severity = "info"
	}
	message := "dealer gamma " + c.Quality.Rankability
	if c.Quality.RankabilityReason != "" {
		message += ": " + c.Quality.RankabilityReason
	}
	return rpc.RegimeWarning{
		Code:     "gamma_zero_" + c.Quality.Rankability,
		Scope:    "gamma_zero",
		Severity: severity,
		Message:  message,
		Impact:   "dealer gamma is shown for awareness but is not a market-structure signal in this snapshot.",
		Action:   "Refresh when the option-data surface is active and verify OI/skew coverage before relying on the gamma read.",
	}, true
}

func warningForRegimeRow(row regimeEvidenceRow) (rpc.RegimeWarning, bool) {
	if row.status == "" {
		return rpc.RegimeWarning{}, false
	}
	w := rpc.RegimeWarning{
		Code:     row.scope + "_" + row.status,
		Scope:    row.scope,
		Severity: "warning",
		Message:  ifEmpty(row.message, row.name+" "+row.status),
		Impact:   row.name + " is unranked; the composite has lower coverage.",
		Action:   "Retry when the relevant market data feed is active.",
	}
	switch row.status {
	case rpc.RegimeStatusStale:
		w.Severity = "info"
		w.Impact = row.name + " is ranked from stale or frozen data; treat the band as a slower regime read."
		w.Action = "Refresh during regular market hours for a live tick."
	case rpc.RegimeStatusComputing:
		w.Message = row.name + " is still computing."
		w.Impact = row.name + " is temporarily unranked; the composite may change when it lands."
		w.Action = "Re-run after the reported ETA or inspect the dedicated command for progress."
	case rpc.RegimeStatusUnavailable:
		w.Message = ifEmpty(row.message, row.name+" unavailable")
	case rpc.RegimeStatusError:
		w.Severity = "error"
	default:
		return rpc.RegimeWarning{}, false
	}
	switch row.scope {
	case "vix_term_structure":
		w.Action = "Retry during Cboe index calculation hours or check index market-data entitlement."
	case "vol_of_vol":
		w.Action = "Retry once Cboe's official VVIX daily file is reachable."
	case "hyg_spy_divergence":
		w.Action = "Retry when ETF quotes are live or check equity/ETF market-data entitlement."
	case "credit_spreads":
		w.Action = "Retry when FRED/St. Louis Fed is reachable; the row uses official ICE BofA OAS series."
	case "funding_stress":
		w.Action = "Retry when FRED/St. Louis Fed is reachable; the row uses official Federal Reserve funding series."
	case "usd_jpy":
		w.Action = "Check IDEALPRO FX market-data entitlement, then retry when FX ticks are available."
	case "gamma_zero":
		if row.status == rpc.RegimeStatusComputing {
			w.Action = "Re-run the brief after the ETA or inspect the Gamma subsystem in canary status."
		} else {
			w.Action = "Run during NY market hours or let the gamma prewarm finish, then retry."
		}
	case "breadth":
		if row.status == rpc.RegimeStatusComputing {
			w.Action = "Keep the daemon running until the IBKR-paced breadth refresh finishes."
		} else {
			w.Action = "Inspect the Breadth subsystem and cached-session context in canary status."
		}
	}
	return w, true
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// bandForVIX classifies the VIX/VIX3M row. Mirrors the CLI's
func bandForVIX(r rpc.RegimeVIXTerm) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyVIXTermBand(r.Ratio)
}

func bandForVolOfVol(r rpc.RegimeVolOfVol) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyVolOfVolBand(r.Last, r.Change5D)
}

// bandForHYGSPY classifies the HYG vs SPY row. HYG below its 50dma
func bandForHYGSPY(r rpc.RegimeHYGSPYDivergence) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	if r.HYG50DMA == nil || r.HYGPrice == nil {
		return ""
	}
	if *r.HYGPrice >= *r.HYG50DMA {
		return "green"
	}
	// HYG below 50dma. Red requires SPY near highs; otherwise the row is
	// yellow. The unranked case is "SPY 52w-high context missing".
	if r.SPY52WHigh == nil || r.SPYPrice == nil {
		return ""
	}
	if *r.SPYPrice >= 0.97**r.SPY52WHigh {
		return "red"
	}
	return "yellow"
}

func bandForCreditSpreads(r rpc.RegimeCreditSpreads) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyCreditSpreadsBand(r)
}

func bandForFundingStress(r rpc.RegimeFundingStress) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyFundingStressBand(r.SpreadBps, r.Change5Bps)
}

// bandForUSDJPY classifies the USD/JPY row. Unranked on
// unavailable/error/computing rows or when the weekly change didn't
// land.
func bandForUSDJPY(r rpc.RegimeUSDJPY) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	return classifyUSDJPYBand(r.WeeklyChange)
}

// bandForGamma classifies the gamma row. Three paths matching the
// CLI's rowGamma logic: a real crossing reads on gap distance;
// no-crossing reads on the signed-profile sign; no data stays unranked.
func bandForGamma(r rpc.RegimeGammaZero) string {
	if r.Status != rpc.RegimeStatusOK || r.Envelope.Result == nil {
		return ""
	}
	c := r.Envelope.Result
	return classifyGammaComputedBand(c)
}

// bandForBreadth classifies the SPX breadth row. Gated on
// status=ok/stale AND envelope state=ready — the CLI does the same
// gate before pulling the value cell.
func bandForBreadth(r rpc.RegimeBreadth) string {
	if r.Status != rpc.RegimeStatusOK && r.Status != rpc.RegimeStatusStale {
		return ""
	}
	if r.Envelope.State != rpc.BreadthStateReady {
		return ""
	}
	return classifyBreadthBand(r.Envelope.PctAbove50DMA)
}

// Cluster combination (raw worst-of bands, isolated-red downgrades,
