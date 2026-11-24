package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
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
	// current marks a band that is a current rating (rpc.RegimeRowCurrent):
	// live, or frozen and not due on the row's own schedule.
	current bool
	// context marks a co-sign row that is red while no stress witness
	// stands: shown, but not a stress reading on its own.
	context bool
	// widening marks the cash credit row while its widening opens a warning
	// from a yellow: it is named as the cause, not as one more mixed reading.
	widening bool
	// weak marks a co-sign row at yellow or red beside a stress witness
	// nobody can read: with it the warning flashes, so it is named as weak,
	// not as mixed or as context.
	weak bool
}

func regimeEvidenceRows(r *rpc.RegimeSnapshotResult) []regimeEvidenceRow {
	if r == nil {
		return nil
	}
	cb := rpc.BuildRegimeClusterBands(r)
	openers := rpc.RegimeWarningOpeners(r)
	darkWitness := slices.Contains(openers, rpc.RegimeWarningWitnessUnavailable)
	// served words a usable row by the band the daemon serves, which holds a
	// red through its exit hysteresis, so the summary agrees with the
	// clusters. A row with no usable reading keeps its unranked state.
	row := func(indicator, scope, status, computed, message string, meta rpc.RegimeIndicatorMeta) regimeEvidenceRow {
		band := computed
		if computed != "" && meta.Band != "" {
			band = meta.Band
		}
		freshness := ""
		if meta.Freshness != nil {
			freshness = meta.Freshness.Class
		}
		return regimeEvidenceRow{
			scope: scope, name: rpc.RegimeRowName(indicator), status: status, band: band, message: message,
			current: rpc.RegimeRowCurrent(status, freshness) && rpc.RegimeIndicatorCurrent(r, indicator),
			// A co-sign red with no stress witness standing, decided per row.
			context: band == "red" && cb.RowRed(indicator) == rpc.RegimeRedContext,
			weak:    darkWitness && rpc.RegimeIndicatorRole(indicator) == rpc.RegimeRoleCoSign && (band == "yellow" || band == "red"),
		}
	}
	credit := row(rpc.RegimeIndicatorCredit, "credit_spreads", r.CreditSpreads.Status, bandForCreditSpreads(r.CreditSpreads), r.CreditSpreads.ErrorMessage, r.CreditSpreads.RegimeIndicatorMeta)
	// The opener is listed only while the row is not red.
	credit.widening = slices.Contains(openers, rpc.RegimeWarningCashWidening)
	return []regimeEvidenceRow{
		row(rpc.RegimeIndicatorVIXTerm, "vix_term_structure", r.VIXTermStructure.Status, bandForVIX(r.VIXTermStructure), r.VIXTermStructure.ErrorMessage, r.VIXTermStructure.RegimeIndicatorMeta),
		row(rpc.RegimeIndicatorVolOfVol, "vol_of_vol", r.VolOfVol.Status, bandForVolOfVol(r.VolOfVol), r.VolOfVol.ErrorMessage, r.VolOfVol.RegimeIndicatorMeta),
		row(rpc.RegimeIndicatorHYGSPY, "hyg_spy_divergence", r.HYGSPYDivergence.Status, bandForHYGSPY(r.HYGSPYDivergence), r.HYGSPYDivergence.ErrorMessage, r.HYGSPYDivergence.RegimeIndicatorMeta),
		credit,
		row(rpc.RegimeIndicatorFunding, "funding_stress", r.FundingStress.Status, bandForFundingStress(r.FundingStress), r.FundingStress.ErrorMessage, r.FundingStress.RegimeIndicatorMeta),
		row(rpc.RegimeIndicatorUSDJPY, "usd_jpy", r.USDJPY.Status, bandForUSDJPY(r.USDJPY), r.USDJPY.ErrorMessage, r.USDJPY.RegimeIndicatorMeta),
		row(rpc.RegimeIndicatorGammaZero, "gamma_zero", r.GammaZero.Status, bandForGamma(r.GammaZero), r.GammaZero.Envelope.Error, r.GammaZero.RegimeIndicatorMeta),
		row(rpc.RegimeIndicatorBreadth, "breadth", r.Breadth.Status, bandForBreadth(r.Breadth), "", r.Breadth.RegimeIndicatorMeta),
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
		if row.band == "red" && !row.context {
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
		switch {
		case row.weak:
			key = "weak"
		case row.context:
			key = "context"
		case row.widening:
			key = "widening"
		case key == "":
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
		{"widening", "widening"},
		{"weak", "weak"},
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
	if len(parts) == 0 && len(groups["context"]) == 0 {
		return "No regime indicators produced a rankable reading."
	}
	if names := staleRegimeEvidenceNames(rows); len(names) > 0 {
		verb := "is"
		if len(names) > 1 {
			verb = "are"
		}
		parts = append(parts, fmt.Sprintf("%s %s ranked from stale data", joinHuman(names), verb))
	}
	sentences := []string{}
	if len(parts) > 0 {
		sentences = append(sentences, strings.Join(parts, "; ")+".")
	}
	if names := groups["context"]; len(names) > 0 {
		sentences = append(sentences, rpc.RegimeContextNote+": "+joinHuman(names)+".")
	}
	return strings.Join(sentences, " ")
}

func staleRegimeEvidenceNames(rows []regimeEvidenceRow) []string {
	var names []string
	for _, row := range rows {
		if row.status == rpc.RegimeStatusStale && row.band != "" && !row.current {
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
		if row.current {
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
	now := nyTime(r.AsOf)
	if w, ok := warningForVIX3MCrossCheck(r.VIXTermStructure, now); ok {
		warnings = append(warnings, w)
	}
	if pending, ok := volOfVolPublicationPending(r, now); ok {
		warnings = append(warnings, rpc.RegimeWarning{
			Code:     "vvix_official_close_pending",
			Scope:    "vol_of_vol",
			Severity: "info",
			Message:  fmt.Sprintf("Cboe has not published a VVIX close after %s; the row is overdue since %s", pending.OfficialDate, pending.Since.UTC().Format("2006-01-02 15:04Z")),
			Impact:   "A newer VVIX close was due by 09:30 New York the morning after its session and has not been read, so the row cannot confirm stress and its band is prior-session context. The delay is Cboe's publication, not a Canary or broker fault.",
			Action:   "Nothing to fix locally: Canary re-reads Cboe's VVIX file on every regime refresh and the row recovers once Cboe publishes.",
		})
	}
	return warnings
}

// warningForVIX3MCrossCheck names what the two independent VIX3M sources
// established. A stale row alone reads as ordinary off-hours context, so the
// one case an operator must act on — a broker leg that has stopped tracking the
// index — needs to say so in its own words.
func warningForVIX3MCrossCheck(row rpc.RegimeVIXTerm, now time.Time) (rpc.RegimeWarning, bool) {
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
		// Read but not yet published is Cboe's delay; only a failed or
		// unusable read is a reachability problem.
		if pending, ok := vix3mPublicationPending(row, now); ok {
			return rpc.RegimeWarning{
				Code:     "vix3m_official_close_pending",
				Scope:    "vix_term_structure",
				Severity: "info",
				Message:  fmt.Sprintf("Cboe has not published a VIX3M close after %s; the off-window gateway leg is unverified since %s", pending.OfficialDate, pending.Since.UTC().Format("2006-01-02 15:04Z")),
				Impact:   "Nothing dates the off-window gateway leg, so the vol cluster is overdue rather than not_due. The delay is Cboe's publication, not a Canary or broker fault.",
				Action:   "Nothing to fix locally: Canary re-reads Cboe's VIX3M file on every regime refresh and the row recovers once Cboe publishes.",
			}, true
		}
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
		if row.status == rpc.RegimeStatusStale {
			// A stale VVIX row was read; its file holds only old closes.
			w.Action = "Nothing to fix locally: Canary re-reads Cboe's VVIX file on every regime refresh and the row recovers once Cboe publishes."
		}
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
	return rpc.HYGSPYCloseBand(r)
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

// Cluster combination (raw worst-of bands, the weight of each row's role,
