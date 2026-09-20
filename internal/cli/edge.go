package cli

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runEdge(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge")
	window := fs.String("window", "365d", "optional review override: 90d or 365d (default 365d)")
	horizon := fs.Int("horizon", 0, "optional highlighted horizon override: 1, 5, or 20 (default automatic)")
	limit := fs.Int("limit", rpc.MaxEdgeFindings, "maximum findings: 1-3")
	change := fs.String("change", "", "opaque change ID for one detailed result")
	option := fs.String("option", "", "opaque option episode or open-position ID for one detailed result")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	params, err := rpc.NormalizeEdgeSnapshotParams(rpc.EdgeSnapshotParams{Window: *window, HorizonSessions: *horizon, Limit: *limit, ChangeID: *change, OptionID: *option})
	if err != nil {
		return fail(env, "edge: %v", err)
	}
	var result rpc.EdgeResult
	if err := env.Conn.Call(ctx, rpc.MethodEdgeSnapshot, params, &result); err != nil {
		return fail(env, "edge: %v", err)
	}
	if err := rpc.ValidateEdgeResult(result); err != nil {
		return fail(env, "edge: daemon returned an invalid result: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderEdgeText(env, result)
	return 0
}

// edgeMoneyWidth right-aligns money cells; "-€ 13,716.62" is 12 cells.
const edgeMoneyWidth = 13

// renderEdgeText projects the typed Edge result in the desk layout shared
// with brief and stress: a title line, the headline, then labelled
// sections separated by blank lines. Money goes through the house
// formatter everywhere, including the headline, which is composed here from
// the typed pattern the daemon selected. The daemon's own prose headline is
// shown only when no pattern is selected, because that text explains a gate
// and carries no amounts.
func renderEdgeText(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	width := briefProseWidth(out)
	fmt.Fprintln(out, edgeTitle(env, result))
	if result.State == rpc.EdgeStateActionRequired && result.Setup != nil {
		fmt.Fprintln(out, "  Flex setup is required before Canary can calculate broker-truth results.")
		for i, step := range result.Setup.Steps {
			fmt.Fprintf(out, "  %d. %s\n", i+1, sanitizeRunText(step))
		}
		if missing := result.Setup.MissingRequirements; len(missing) > 0 {
			shown := missing
			suffix := ""
			if len(shown) > 8 {
				shown = shown[:8]
				suffix = fmt.Sprintf(" (+%d more; use --json)", len(missing)-len(shown))
			}
			fmt.Fprintf(out, "  Missing (%d): %s%s\n", len(missing), sanitizeRunText(strings.Join(shown, ", ")), suffix)
		}
		return
	}
	edgeProse(env, edgeHeadlineText(env, result), width)
	if result.ReviewNote != "" {
		edgeProse(env, sanitizeRunText(result.ReviewNote), width)
	}
	renderEdgeEvidence(env, result)
	renderEdgeAccount(env, result, width)
	renderEdgeMatrix(env, result, width)
	renderEdgeFindings(env, result)
	renderEdgeOptions(env, result)
	if result.Change != nil {
		renderEdgeChange(env, *result.Change, edgeBaseCurrency(result))
	}
	if result.Option != nil {
		renderEdgeOptionDetail(env, *result.Option, edgeBaseCurrency(result))
	}
	fmt.Fprintln(out, "\nDetails: canary edge --change <id> · canary edge --option <id> · canary edge --json")
}

func edgeTitle(env *Env, result rpc.EdgeResult) string {
	parts := []string{"Canary Edge"}
	if result.AutomaticHorizon {
		period := result.Window
		if result.Window == "365d" {
			period = "one-year"
		}
		parts = append(parts, fmt.Sprintf("automatic %s decision review", period), "selected "+edgeSessionCount(result.HorizonSessions))
	} else {
		parts = append(parts, fmt.Sprintf("%s decision review", result.Window), fmt.Sprintf("%d-session lens", result.HorizonSessions))
	}
	if !result.AsOf.IsZero() {
		parts = append(parts, "evidence to "+result.AsOf.Format(time.DateOnly))
	}
	if result.State != rpc.EdgeStateCurrent {
		parts = append(parts, env.yellow(string(result.State)))
	}
	return strings.Join(parts, " · ")
}

// edgeHeadlineText composes the headline from the typed pattern the daemon
// selected so its money matches every other line; it never re-selects or
// re-ranks. Without a selected pattern the daemon's gate explanation stands.
func edgeHeadlineText(env *Env, result rpc.EdgeResult) string {
	pattern, lens := edgeSelectedPattern(result)
	if pattern == nil || lens.TotalBase == nil || lens.MedianBase == nil || pattern.Direction == "" {
		return sanitizeRunText(result.Headline)
	}
	ccy := edgeBaseCurrency(result)
	direction := strings.ToUpper(pattern.Direction[:1]) + pattern.Direction[1:]
	return fmt.Sprintf("%s %s: %s price impact across %d of %d changes at %s; median %s.", direction, edgeActionPlural(pattern.Action), edgeSignedMoney(env, *lens.TotalBase, ccy), lens.SampleCount, pattern.EligibleChanges, edgeSessionCount(lens.Sessions), edgeSignedMoney(env, *lens.MedianBase, ccy))
}

func edgeSelectedPattern(result rpc.EdgeResult) (*rpc.EdgeDecisionPattern, *rpc.EdgePatternHorizon) {
	if result.ReviewAction == "" {
		return nil, nil
	}
	for i := range result.Patterns {
		pattern := &result.Patterns[i]
		if pattern.Action != result.ReviewAction || pattern.Direction != result.ReviewDirection {
			continue
		}
		for j := range pattern.Horizons {
			if pattern.Horizons[j].Sessions == result.HorizonSessions {
				return pattern, &pattern.Horizons[j]
			}
		}
	}
	return nil, nil
}

func renderEdgeEvidence(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	selection := result.HorizonSelection
	fmt.Fprintln(out, "\nEvidence")
	coverage := []string{fmt.Sprintf("%d of %d eligible stock/ETF changes scored at %s (%.1f%%)", selection.ScoredChanges, selection.EligibleChanges, edgeSessionCount(result.HorizonSessions), selection.CoveragePct)}
	if result.Coverage.TradeChanges > 0 {
		outside := result.Coverage.TradeChanges - result.Coverage.EligibleChanges
		coverage = append(coverage, fmt.Sprintf("%s broker position changes found, %s outside the stock/ETF review", edgeCount(result.Coverage.TradeChanges), edgeCount(outside)))
	}
	edgeRow(env, "Coverage", coverage...)
	edgeRow(env, "Horizon", edgeHorizonNote(result)...)
	if pattern, lens := edgeSelectedPattern(result); pattern != nil {
		group := pattern.Direction + " " + edgeActionPlural(pattern.Action)
		notional := "size coverage unavailable: incomplete execution amounts"
		if lens.NotionalCoveragePct != nil {
			notional = fmt.Sprintf("size coverage %.1f%% of execution notional", *lens.NotionalCoveragePct)
		}
		edgeRow(env, "Selected sample", fmt.Sprintf("%d of %d %s", lens.SampleCount, pattern.EligibleChanges, group), notional)
		if lens.LargestDateSharePct != nil && lens.LargestContractSharePct != nil && lens.WithoutLargestBase != nil {
			edgeRow(env, "Concentration", fmt.Sprintf("largest date %.1f%% and largest contract %.1f%% of absolute impact", *lens.LargestDateSharePct, *lens.LargestContractSharePct), "without the largest decision "+edgeSignedMoney(env, *lens.WithoutLargestBase, ccy))
		}
		if len(lens.Months) > 0 {
			fmt.Fprintln(out, "  By month")
			for _, month := range lens.Months {
				fmt.Fprintf(out, "    %s  %2d %-9s  total %s  median %s\n", month.Month, month.SampleCount, pluralWord(month.SampleCount, "decision", "decisions"), padLeftVisible(edgeSignedMoney(env, month.TotalBase, ccy), edgeMoneyWidth), padLeftVisible(edgeSignedMoney(env, month.MedianBase, ccy), edgeMoneyWidth))
			}
		}
		for _, comparison := range pattern.Comparisons {
			label := fmt.Sprintf("Same decisions %d to %d sessions", comparison.EarlierSessions, comparison.LaterSessions)
			if comparison.SampleCount == 0 || comparison.EarlierTotalBase == nil || comparison.LaterTotalBase == nil || comparison.MedianDifferenceBase == nil {
				edgeRow(env, label, "no common sample")
				continue
			}
			edgeRow(env, label, fmt.Sprintf("n=%d", comparison.SampleCount), fmt.Sprintf("%s becomes %s", edgeSignedMoney(env, *comparison.EarlierTotalBase, ccy), edgeSignedMoney(env, *comparison.LaterTotalBase, ccy)), "median paired change "+edgeSignedMoney(env, *comparison.MedianDifferenceBase, ccy))
		}
		edgeRow(env, "Risk context", fmt.Sprintf("%d linked protection, %d partial; purpose of the rest unknown", lens.LinkedProtectionCount, lens.PartialProtectionCount), "local context "+result.ProtectionState, "a price outcome does not grade risk management")
	}
	if len(result.MarketContext) > 0 || len(result.MarketContextMissing) > 0 {
		parts := make([]string, 0, len(result.MarketContext)+len(result.MarketContextMissing)+1)
		parts = append(parts, "median benchmark move over the selected intervals")
		for _, context := range result.MarketContext {
			value := edgeSignedPercent(context.MedianChangePct)
			if context.Kind == "volatility_index" && context.MedianChangePoints != nil {
				value = fmt.Sprintf("%+.2f pts", *context.MedianChangePoints)
			}
			parts = append(parts, fmt.Sprintf("%s %s (n=%d)", context.Label, value, context.SampleCount))
		}
		for _, key := range result.MarketContextMissing {
			parts = append(parts, edgeMarketContextLabel(key)+" unavailable")
		}
		edgeRow(env, "Market context", parts...)
	}
	if !result.LastFullRevalidation.IsZero() {
		edgeRow(env, "Last full revalidation", result.LastFullRevalidation.Local().Format(time.DateOnly))
	}
}

// edgeHorizonNote says which horizon the review shows and why, including
// the coverage of every longer horizon the automatic selection passed over.
func edgeHorizonNote(result rpc.EdgeResult) []string {
	selection := result.HorizonSelection
	floors := ""
	if selection.MinimumCoveragePct > 0 && selection.MinimumSample > 0 {
		floors = fmt.Sprintf("at least %.0f%% coverage and a %d-observation action sample", selection.MinimumCoveragePct, selection.MinimumSample)
	}
	if !result.AutomaticHorizon {
		parts := []string{edgeSessionCount(result.HorizonSessions) + " selected with --horizon"}
		if floors != "" {
			parts = append(parts, "automatic mode picks the longest horizon with "+floors)
		}
		return parts
	}
	parts := []string{edgeSessionCount(result.HorizonSessions) + " selected automatically"}
	for _, sessions := range []int{20, 5} {
		if sessions <= result.HorizonSessions || selection.EligibleChanges <= 0 {
			continue
		}
		pct := float64(result.Coverage.ScoredByHorizon[sessions]) / float64(selection.EligibleChanges) * 100
		parts = append(parts, fmt.Sprintf("%s %.1f%% coverage", edgeSessionCount(sessions), pct))
	}
	if floors != "" {
		parts = append(parts, "the longest horizon with "+floors+" is selected")
	}
	return parts
}

func renderEdgeAccount(env *Env, result rpc.EdgeResult, width int) {
	if result.Account == nil {
		return
	}
	out := env.Stdout
	account := result.Account
	fmt.Fprintln(out, "\nAccount P/L")
	edgeRow(env, env.bold(edgeSignedMoney(env, account.ProfitLossBase, account.BaseCurrency)), fmt.Sprintf("%s to %s", account.ActualFrom.Format(time.DateOnly), account.ActualTo.Format(time.DateOnly)), "external flows "+edgeSignedMoney(env, account.ExternalFlowsBase, account.BaseCurrency))
	edgeProse(env, env.dim("Everything after confirmed flows, options and market moves included; not a sum of the rows below."), width)
}

// renderEdgeMatrix prints the all-sample matrix with its caveat directly
// above it, so "different decision sets" refers to the rows the reader is
// looking at.
func renderEdgeMatrix(env *Env, result rpc.EdgeResult, width int) {
	if len(result.ActionRollups) == 0 {
		return
	}
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	fmt.Fprintln(out, "\nDecisions by action")
	edgeProse(env, env.dim("Columns are separate decision sets; compare horizons under Same decisions."), width)
	fmt.Fprintln(out, strings.TrimRight(fmt.Sprintf("  %-6s  %s  %s  %s", "ACTION", edgeMatrixHeader("1 SESSION"), edgeMatrixHeader("5 SESSIONS"), edgeMatrixHeader("20 SESSIONS")), " "))
	for _, row := range result.ActionRollups {
		cells := make([]string, 0, 3)
		for _, sessions := range []int{1, 5, 20} {
			cells = append(cells, edgeRollupCell(env, edgeRollupAt(row, sessions), ccy))
		}
		fmt.Fprintln(out, strings.TrimRight(fmt.Sprintf("  %-6s  %s", strings.ToUpper(row.Action), strings.Join(cells, "  ")), " "))
	}
}

func edgeMatrixHeader(label string) string {
	return padLeftVisible(label, edgeMoneyWidth) + "  " + strings.Repeat(" ", 5)
}

func edgeRollupAt(row rpc.EdgeActionRollup, sessions int) *rpc.EdgeHorizonRollup {
	for i := range row.Horizons {
		if row.Horizons[i].Sessions == sessions {
			return &row.Horizons[i]
		}
	}
	return nil
}

func edgeRollupCell(env *Env, value *rpc.EdgeHorizonRollup, currency string) string {
	if value == nil {
		return padLeftVisible(env.dim("—"), edgeMoneyWidth) + "  " + padRightVisible("", 5)
	}
	count := padRightVisible(fmt.Sprintf("n=%d", value.SampleCount), 5)
	if value.TotalBase == nil {
		return padLeftVisible(env.dim("—"), edgeMoneyWidth) + "  " + count
	}
	return padLeftVisible(edgeSignedMoney(env, *value.TotalBase, currency), edgeMoneyWidth) + "  " + count
}

func renderEdgeFindings(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	fmt.Fprintf(out, "\nFindings  %s\n", env.dim(fmt.Sprintf("by absolute impact at %s; not the headline group", edgeSessionCount(result.HorizonSessions))))
	if len(result.Findings) == 0 {
		fmt.Fprintln(out, env.dim("  none clears the account-materiality gates"))
		return
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(out, "  %s  %s  %s %s  %s\n", padLeftVisible(edgeSignedMoney(env, finding.DecisionImpactBase, ccy), edgeMoneyWidth), padLeftVisible(fmt.Sprintf("%+.2f%%", finding.DecisionImpactPct), 8), sanitizeRunText(finding.Symbol), finding.Action, env.dim(sanitizeRunText(finding.ChangeID)))
		if len(finding.MarketContext) > 0 {
			fmt.Fprintln(out, "      "+env.dim(edgeFindingContext(finding.MarketContext)))
		}
	}
}

func renderEdgeOptions(env *Env, result rpc.EdgeResult) {
	out := env.Stdout
	options := result.Options
	currency := edgeBaseCurrency(result)
	fmt.Fprintf(out, "\nOptions  %s\n", env.dim("broker actuals in separate scopes; they do not add together"))
	renderEdgeOptionCycles(env, result)
	if options.Realized.TotalCount == 0 {
		edgeRow(env, "Realized episodes", "no broker-reported episode available in the selected window")
	} else {
		known := "P/L unavailable"
		if options.Realized.KnownPNLBase != nil {
			known = edgeSignedMoney(env, *options.Realized.KnownPNLBase, currency) + " known"
		}
		values := []string{known, fmt.Sprintf("%d %s: %d positive, %d negative, %d flat", options.Realized.TotalCount, pluralWord(options.Realized.TotalCount, "episode", "episodes"), options.Realized.PositiveCount, options.Realized.NegativeCount, options.Realized.FlatCount)}
		if incomplete := options.Realized.PartialCount + options.Realized.UnavailableCount; incomplete > 0 {
			values = append(values, fmt.Sprintf("%d incomplete", incomplete))
		}
		if options.Realized.Truncated {
			values = append(values, fmt.Sprintf("showing %d", len(options.Realized.Episodes)))
		}
		edgeRow(env, "Realized episodes", values...)
		renderEdgeOptionExtremes(env, options.Realized.Episodes, currency)
	}
	switch {
	case options.Open.SnapshotDate.IsZero():
		edgeRow(env, "Open snapshot", "no dated Flex Open Positions snapshot available")
	case options.Open.TotalCount == 0:
		edgeRow(env, "Open snapshot "+options.Open.SnapshotDate.Format(time.DateOnly), "0 positions", "confirmed empty")
	default:
		known := "P/L unavailable"
		if options.Open.KnownPNLBase != nil {
			known = edgeSignedMoney(env, *options.Open.KnownPNLBase, currency) + " known"
		}
		values := []string{known, fmt.Sprintf("%d %s: %d positive, %d negative, %d flat", options.Open.TotalCount, pluralWord(options.Open.TotalCount, "position", "positions"), options.Open.PositiveCount, options.Open.NegativeCount, options.Open.FlatCount)}
		if options.Open.UnavailableCount > 0 {
			values = append(values, fmt.Sprintf("%d unavailable", options.Open.UnavailableCount))
		}
		if options.Open.Truncated {
			values = append(values, fmt.Sprintf("showing %d", len(options.Open.Positions)))
		}
		edgeRow(env, "Open snapshot "+options.Open.SnapshotDate.Format(time.DateOnly), values...)
		renderEdgeOpenOptionExtremes(env, options.Open.Positions, currency)
	}
	if n := options.Coverage.OpeningOnlyZeroEpisodes; n > 0 {
		edgeRow(env, "Activity", fmt.Sprintf("%d opening-only zero-P/L %s retained as coverage, not realized results", n, pluralWord(n, "episode", "episodes")))
	}
}

func renderEdgeOptionCycles(env *Env, result rpc.EdgeResult) {
	cycles := result.Options.Cycles
	if cycles.Reasons == nil {
		return
	}
	out := env.Stdout
	ccy := edgeBaseCurrency(result)
	edgeRow(env, "Completed positions",
		fmt.Sprintf("%d proven flat-to-flat contract %s", cycles.CompletedCount, pluralWord(cycles.CompletedCount, "cycle", "cycles")),
		fmt.Sprintf("%d complete P/L", cycles.CompletePNLCount),
		fmt.Sprintf("%d %s still open", cycles.OpenContractCount, pluralWord(cycles.OpenContractCount, "contract", "contracts")),
		fmt.Sprintf("%d %s excluded", cycles.ExcludedContracts, pluralWord(cycles.ExcludedContracts, "contract", "contracts")),
		"a subset of realized activity, not an additional total or a multi-leg strategy")
	if len(cycles.Reasons) > 0 {
		reasons := make([]string, 0, len(cycles.Reasons))
		for reason, count := range cycles.Reasons {
			reasons = append(reasons, fmt.Sprintf("%d %s", count, strings.ReplaceAll(reason, "_", " ")))
		}
		sort.Strings(reasons)
		edgeRow(env, "Reconstruction gaps", strings.Join(reasons, "; "), "opening/window exclusions count positions; other exclusions count contracts")
	}
	for i, row := range cycles.Cycles {
		if i == 3 {
			break
		}
		amount := env.dim("unavailable")
		if row.RealizedPNLBase != nil {
			amount = edgeSignedMoney(env, *row.RealizedPNLBase, ccy)
		}
		fmt.Fprintf(out, "    %s  %s %s · %s to %s · %d %s · %s\n", padLeftVisible(amount, edgeMoneyWidth), row.Direction, edgeCompactSymbol(row.Symbol), row.OpenedAt.Format(time.DateOnly), row.ClosedAt.Format(time.DateOnly), row.ExecutionCount, pluralWord(row.ExecutionCount, "execution", "executions"), row.PNLStatus)
	}
}

func renderEdgeOptionExtremes(env *Env, episodes []rpc.EdgeOptionEpisodeSummary, currency string) {
	var gain, loss *rpc.EdgeOptionEpisodeSummary
	for i := range episodes {
		row := &episodes[i]
		if row.RealizedPNLBase == nil {
			continue
		}
		if *row.RealizedPNLBase > 0 && (gain == nil || *row.RealizedPNLBase > *gain.RealizedPNLBase) {
			gain = row
		}
		if *row.RealizedPNLBase < 0 && (loss == nil || *row.RealizedPNLBase < *loss.RealizedPNLBase) {
			loss = row
		}
	}
	for _, row := range []*rpc.EdgeOptionEpisodeSummary{gain, loss} {
		if row == nil {
			continue
		}
		fmt.Fprintf(env.Stdout, "    %s  %s · %s · %s · %s\n", padLeftVisible(edgeSignedMoney(env, *row.RealizedPNLBase, currency), edgeMoneyWidth), edgeOptionEpisodeLabel(*row), row.ActivityFrom.Format(time.DateOnly), row.PNLStatus, env.dim(sanitizeRunText(row.ID)))
	}
}

func renderEdgeOpenOptionExtremes(env *Env, positions []rpc.EdgeOptionOpenPositionSummary, currency string) {
	var gain, loss *rpc.EdgeOptionOpenPositionSummary
	for i := range positions {
		row := &positions[i]
		if row.OpenPNLBase == nil {
			continue
		}
		if *row.OpenPNLBase > 0 && (gain == nil || *row.OpenPNLBase > *gain.OpenPNLBase) {
			gain = row
		}
		if *row.OpenPNLBase < 0 && (loss == nil || *row.OpenPNLBase < *loss.OpenPNLBase) {
			loss = row
		}
	}
	for _, row := range []*rpc.EdgeOptionOpenPositionSummary{gain, loss} {
		if row == nil {
			continue
		}
		fmt.Fprintf(env.Stdout, "    %s  %s · %s · %s\n", padLeftVisible(edgeSignedMoney(env, *row.OpenPNLBase, currency), edgeMoneyWidth), edgeOptionContractLabel(row.Underlying, row.Symbol, row.Expiry, row.Strike, row.PutCall), row.PNLStatus, env.dim(sanitizeRunText(row.ID)))
	}
}

func edgeOptionEpisodeLabel(row rpc.EdgeOptionEpisodeSummary) string {
	labels := make([]string, 0, len(row.Legs))
	for _, leg := range row.Legs {
		labels = append(labels, edgeOptionContractLabel(leg.Underlying, leg.Symbol, leg.Expiry, leg.Strike, leg.PutCall))
	}
	if len(labels) == 0 {
		return firstNonEmptyCLI(sanitizeRunText(row.Underlying), "Option episode")
	}
	if len(labels) > 3 {
		return strings.Join(labels[:3], " + ") + fmt.Sprintf(" +%d legs", len(labels)-3)
	}
	return strings.Join(labels, " + ")
}

func edgeOptionContractLabel(underlying, symbol, expiry string, strike *float64, putCall string) string {
	root := firstNonEmptyCLI(sanitizeRunText(underlying), edgeCompactSymbol(symbol), "Option")
	parts := []string{root}
	if expiry != "" {
		parts = append(parts, sanitizeRunText(expiry))
	}
	if strike != nil {
		parts = append(parts, fmt.Sprintf("%.4g", *strike))
	}
	if putCall != "" {
		parts = append(parts, strings.ToUpper(putCall[:1]))
	}
	return strings.Join(parts, " ")
}

// edgeCompactSymbol collapses the padded OCC spelling a broker row carries
// ("NOW   260821C00115000") to single spaces after dropping control bytes.
func edgeCompactSymbol(symbol string) string {
	return strings.Join(strings.Fields(sanitizeRunText(symbol)), " ")
}

func firstNonEmptyCLI(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func renderEdgeOptionDetail(env *Env, detail rpc.EdgeOptionDetail, currency string) {
	out := env.Stdout
	fmt.Fprintf(out, "\nOption %s · %s\n", sanitizeRunText(detail.ID), strings.ReplaceAll(detail.Kind, "_", " "))
	if detail.Episode != nil {
		episode := detail.Episode
		values := []string{episode.Grouping, episode.Lifecycle, episode.ActivityFrom.Local().Format("2006-01-02 15:04 MST")}
		if episode.RealizedPNLBase != nil {
			values = append(values, "realized "+edgeSignedMoney(env, *episode.RealizedPNLBase, currency))
		}
		values = append(values, episode.PNLStatus)
		if len(episode.MissingEvidence) > 0 {
			values = append(values, "missing "+strings.Join(episode.MissingEvidence, ", "))
		}
		edgeRow(env, "Episode", values...)
		for _, leg := range episode.Legs {
			values := []string{leg.Side + " " + leg.OpenClose}
			if leg.Quantity != nil {
				values = append(values, fmt.Sprintf("qty %.4g", *leg.Quantity))
			}
			if leg.ExecutionPrice != nil {
				values = append(values, fmt.Sprintf("@ %.4g %s", *leg.ExecutionPrice, sanitizeRunText(leg.Currency)))
			}
			if leg.RealizedPNLBase != nil {
				values = append(values, "realized "+edgeSignedMoney(env, *leg.RealizedPNLBase, currency))
			}
			if leg.DirectCostsBase != nil {
				values = append(values, "costs "+edgeSignedMoney(env, *leg.DirectCostsBase, currency))
			}
			if len(leg.MissingEvidence) > 0 {
				values = append(values, "missing "+strings.Join(leg.MissingEvidence, ", "))
			}
			edgeRow(env, "Leg "+edgeOptionContractLabel(leg.Underlying, leg.Symbol, leg.Expiry, leg.Strike, leg.PutCall), values...)
		}
		return
	}
	if detail.OpenPosition != nil {
		position := detail.OpenPosition
		values := []string{"snapshot " + position.SnapshotDate.Format(time.DateOnly), position.Side}
		if position.Quantity != nil {
			values = append(values, fmt.Sprintf("qty %.4g", *position.Quantity))
		}
		if position.MarkPrice != nil {
			values = append(values, fmt.Sprintf("mark %.4g %s", *position.MarkPrice, sanitizeRunText(position.Currency)))
		}
		if position.CostBasisMoney != nil {
			values = append(values, fmt.Sprintf("cost basis %s %s", formatMoneyBare(*position.CostBasisMoney), sanitizeRunText(position.Currency)))
		}
		if position.OpenPNLBase != nil {
			values = append(values, "open "+edgeSignedMoney(env, *position.OpenPNLBase, currency))
		}
		values = append(values, position.PNLStatus)
		if len(position.MissingEvidence) > 0 {
			values = append(values, "missing "+strings.Join(position.MissingEvidence, ", "))
		}
		edgeRow(env, edgeOptionContractLabel(position.Underlying, position.Symbol, position.Expiry, position.Strike, position.PutCall), values...)
	}
}

func renderEdgeChange(env *Env, change rpc.EdgeChangeDetail, currency string) {
	out := env.Stdout
	fmt.Fprintf(out, "\nChange %s · %s %s %+.4g · %s\n", sanitizeRunText(change.ID), sanitizeRunText(change.Symbol), change.Action, change.DeltaQuantity, change.ExecutedAt.Local().Format("2006-01-02 15:04 MST"))
	values := []string{fmt.Sprintf("%.4g to %.4g", change.PositionBefore, change.PositionAfter)}
	if change.ExecutionVWAP != nil {
		values = append(values, fmt.Sprintf("execution VWAP %.4g", *change.ExecutionVWAP))
	}
	if change.Multiplier != nil {
		values = append(values, fmt.Sprintf("multiplier %.4g", *change.Multiplier))
	}
	if change.DirectCostsBase != nil {
		values = append(values, "direct costs "+edgeSignedMoney(env, *change.DirectCostsBase, currency))
	}
	edgeRow(env, "Position", values...)
	for _, score := range change.Scores {
		value := env.dim("unavailable (" + score.Reason + ")")
		if score.DecisionImpactBase != nil {
			value = edgeSignedMoney(env, *score.DecisionImpactBase, currency)
			if score.DecisionImpactPct != nil && score.DecisionNotionalBase != nil {
				value += fmt.Sprintf(" · %+.2f%% of %s", *score.DecisionImpactPct, edgeSignedMoney(env, *score.DecisionNotionalBase, currency))
			}
			if score.HorizonDay != nil && score.HorizonClose != nil && score.HorizonFX != nil {
				value += fmt.Sprintf(" · %s close %.4g FX %.6g", score.HorizonDay.Format(time.DateOnly), *score.HorizonClose, *score.HorizonFX)
			}
		}
		fmt.Fprintf(out, "  %-11s  %s\n", edgeSessionCount(score.Sessions), value)
		if len(score.MarketContext) > 0 {
			fmt.Fprintln(out, "               "+env.dim(edgeFindingContext(score.MarketContext)))
		}
	}
}

// edgeFindingContext keeps per-decision benchmark moves short: the ticker
// keys stand in for the full proxy labels that the Evidence row spells out.
func edgeFindingContext(rows []rpc.EdgeMarketContext) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		value := fmt.Sprintf("%+.2f%%", row.ChangePct)
		if row.Kind == "volatility_index" && row.ChangePoints != nil {
			value = fmt.Sprintf("%+.2f pts", *row.ChangePoints)
		}
		label := strings.ToUpper(row.Key)
		if label == "" {
			label = row.Label
		}
		parts = append(parts, sanitizeRunText(label)+" "+value)
	}
	return strings.Join(parts, " · ")
}

func edgeMarketContextLabel(key string) string {
	switch key {
	case "spy":
		return "S&P 500 proxy (SPY)"
	case "qqq":
		return "Nasdaq-100 proxy (QQQ)"
	case "dia":
		return "Dow proxy (DIA)"
	case "vix":
		return "CBOE VIX"
	default:
		return strings.ToUpper(key)
	}
}

func edgeActionPlural(action string) string {
	switch action {
	case "open":
		return "opens"
	case "add":
		return "adds"
	case "trim":
		return "trims"
	case "exit":
		return "exits"
	default:
		return action + "s"
	}
}

func edgeSignedPercent(value *float64) string {
	if value == nil {
		return "—"
	}
	return fmt.Sprintf("%+.2f%%", *value)
}

func edgeSessionCount(sessions int) string {
	return fmt.Sprintf("%d %s", sessions, pluralWord(sessions, "session", "sessions"))
}

func edgeBaseCurrency(result rpc.EdgeResult) string {
	if result.Account != nil && result.Account.BaseCurrency != "" {
		return result.Account.BaseCurrency
	}
	return "BASE"
}

// edgeSignedMoney is the one money spelling on the Edge screen: the house
// currency prefix, thousands grouping, a real zero, and sign colour.
func edgeSignedMoney(env *Env, value float64, currency string) string {
	return env.colorBySign(value, formatMoneyCcyForPnL(value, currency), signPnL)
}

func edgeCount(n int) string {
	return groupThousands(strconv.Itoa(n))
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// edgeRow prints one labelled row in the desk's "Label · value · value"
// shape with a hanging indent. Unlike riskReadLine it does not strip escape
// bytes, so sign colour survives; callers sanitize broker-sourced text.
func edgeRow(env *Env, label string, values ...string) {
	text := briefJoin(append([]string{label}, values...)...)
	edgeProse(env, text, briefProseWidth(env.Stdout))
}

// edgeMoneyGlue matches the house money prefix ("€ ", "-$ ", "CHF ") before
// its amount so a wrapped row never splits the two.
var edgeMoneyGlue = regexp.MustCompile(`([€$£¥]|\b[A-Z]{3,4}) (\d)`)

// edgeGlue stands in for a space the wrapper must not break on. It is a
// private-use rune, one cell wide for visibleLen, and never reaches the
// terminal; a no-break space would not do because unicode.IsSpace splits it.
const edgeGlue = "\ue000"

// edgeProse wraps one row or paragraph at the prose measure with a hanging
// indent. Money keeps its prefix and amount together, and the "·" joiner
// stays at the end of a line rather than opening the continuation.
func edgeProse(env *Env, text string, width int) {
	glued := edgeMoneyGlue.ReplaceAllString(text, "$1"+edgeGlue+"$2")
	glued = strings.ReplaceAll(glued, " · ", edgeGlue+"· ")
	for i, line := range wrapVisibleText(glued, width-4) {
		indent := "  "
		if i > 0 {
			indent = "    "
		}
		fmt.Fprintln(env.Stdout, indent+strings.ReplaceAll(line, edgeGlue, " "))
	}
}
