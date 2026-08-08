package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

// StressInput is the typed input consumed by the shared stress evaluator.
type StressInput = rpc.StressInput

// StressResult is the complete typed output of a stress evaluation.
type StressResult = rpc.StressResult

// StressRow is one evidence row in a stress result.
type StressRow = rpc.StressRow

// StressMarketIndicator is one normalized market input in a stress summary.
type StressMarketIndicator = rpc.StressMarketIndicator

// StressMarketSummary is the market-side evidence summarized for the stress read.
type StressMarketSummary = rpc.StressMarketSummary

const (
	stressActionWatch         = stress.ActionWatch
	stressActionDefend        = stress.ActionDefend
	stressActionRebalance     = stress.ActionRebalance
	stressActionDeploy        = stress.ActionDeploy
	stressActionConfirmInputs = stress.ActionConfirmInputs
	stressInputOK             = stress.InputOK
	stressInputDegraded       = stress.InputDegraded
	stressPortfolioFitLow     = stress.PortfolioFitLow
	stressPortfolioFitUnknown = stress.PortfolioFitUnknown
)

// ComputeStress evaluates in through the shared pure stress engine.
func ComputeStress(in StressInput) StressResult {
	return stress.ComputeStress(in)
}

func stressMarketEvidence(m StressMarketSummary) string {
	return stress.MarketEvidence(m)
}

func stressPortfolioEvidence(p rpc.StressPortfolioSummary) string {
	return stress.PortfolioEvidence(p)
}

func stressAmbiguityEvidence(m StressMarketSummary) string {
	return stress.AmbiguityEvidence(m)
}

func formatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	return stress.FormatProtectionCoverageEvidence(c)
}

func appendUniqueString(values []string, value string) []string {
	return stress.AppendUniqueString(values, value)
}

func runStress(ctx context.Context, env *Env, args []string) int {
	if slicesContains(args, "history") {
		return runStressHistory(ctx, env, args)
	}
	fs := flagSet(env, "stress")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON for scheduling")
	details := fs.Bool("details", false, "show full stress evidence rows")
	view := fs.String("view", rpc.ViewFull, "JSON response view: full | alert")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	if *view != rpc.ViewFull && *view != rpc.ViewAlert {
		return fail(env, "stress: --view must be %q or %q (got %q)", rpc.ViewFull, rpc.ViewAlert, *view)
	}
	if *view != rpc.ViewFull && !*jsonOut {
		return fail(env, "stress: --view requires --json")
	}
	if !*jsonOut && isTerminal(env.Stdout) {
		stop := startStressSpinner(env)
		res, err := stress.FetchStress(ctx, env.Conn)
		stop()
		if err != nil {
			return fail(env, "stress: %v", err)
		}
		return renderStressTextDetails(env, env.Stdout, &res, *details)
	}
	res, positions, err := stress.FetchStressSnapshot(ctx, env.Conn)
	if err != nil {
		return fail(env, "stress: %v", err)
	}
	if *jsonOut {
		if *view == rpc.ViewAlert {
			return printJSON(env, rpc.CompactStressAlert(&res, &positions))
		}
		return printJSON(env, res)
	}
	return renderStressTextDetails(env, env.Stdout, &res, *details)
}

// runStressHistory renders the daemon's derived stress-decision index
// (stress.history). Read-only: rows are journal evidence; the daemon owns
// filtering, ordering, and index-health disclosure.
func runStressHistory(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "stress")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	since := fs.String("since", "", "inclusive lower boundary: YYYY-MM-DD UTC day or RFC3339 timestamp")
	until := fs.String("until", "", "upper boundary: YYYY-MM-DD UTC day (whole day included) or RFC3339 timestamp")
	severity := fs.String("severity", "", "exact severity filter (e.g. observe, watch, act, urgent)")
	action := fs.String("action", "", "exact action filter (e.g. watch, defend, rebalance)")
	limit := fs.Int("limit", 0, "max rows, newest first (default 50, max 500)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "history" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "stress history: usage is `canary stress history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--severity SEV] [--action ACTION] [--limit N] [--json]`")
	}
	params := rpc.StressHistoryParams{
		Since:    strings.TrimSpace(*since),
		Until:    strings.TrimSpace(*until),
		Severity: strings.TrimSpace(*severity),
		Action:   strings.TrimSpace(*action),
		Limit:    *limit,
	}
	var res rpc.StressHistoryResult
	if err := env.Conn.Call(ctx, rpc.MethodStressHistory, params, &res); err != nil {
		return fail(env, "stress history: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderStressHistoryText(env, env.Stdout, &res)
	return 0
}

// renderStressHistoryText prints the newest-first decision table plus the
// index-freshness footer. Summaries are journal free text — truncated to
// the terminal, never interpreted.
func renderStressHistoryText(env *Env, out io.Writer, res *rpc.StressHistoryResult) {
	width := outputColumns(out)
	if width < 60 {
		width = 120
	}
	header := fmt.Sprintf("Stress history  %s → %s UTC  %d of %d rows",
		res.Since.UTC().Format("2006-01-02"), res.Until.UTC().Format("2006-01-02"), res.Count, res.TotalCount)
	if res.Truncated {
		header += " (truncated; raise --limit)"
	}
	fmt.Fprintln(out, header)
	if len(res.Entries) == 0 {
		fmt.Fprintln(out, "  no indexed stress decisions in this window")
	} else {
		actionW, stageW := 6, 5
		for _, e := range res.Entries {
			actionW = min(max(actionW, len(e.Action)), 14)
			stageW = min(max(stageW, len(e.MarketStage)), 16)
		}
		fmt.Fprintf(out, "  %s\n", env.dim(fmt.Sprintf("%-16s  %-7s  %-*s  %-*s  %s",
			"AT (UTC)", "SEV", actionW, "ACTION", stageW, "STAGE", "SUMMARY")))
		summaryW := max(width-(2+16+2+7+2+actionW+2+stageW+2), 16)
		for _, e := range res.Entries {
			fmt.Fprintf(out, "  %-16s  %-7s  %-*s  %-*s  %s\n",
				e.At.UTC().Format("2006-01-02 15:04"),
				truncateVisible(nonEmpty(e.Severity, "-"), 7),
				actionW, truncateVisible(nonEmpty(e.Action, "-"), actionW),
				stageW, truncateVisible(nonEmpty(e.MarketStage, "-"), stageW),
				truncateVisible(e.Summary, summaryW))
		}
	}
	renderHistoryIndexFooter(env, out, res.Index)
}

func startStressSpinner(env *Env) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	go func() {
		defer close(done)
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stop:
				fmt.Fprint(env.Stdout, "\r\x1b[K")
				return
			case <-ticker.C:
				fmt.Fprintf(env.Stdout, "\r\x1b[K%s %s",
					env.dim("Populating stress read: account, positions, regime, gamma, breadth"), frames[i])
				i = (i + 1) % len(frames)
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func renderStressText(env *Env, out io.Writer, r *StressResult) int {
	return renderStressTextDetails(env, out, r, false)
}

func renderStressTextDetails(env *Env, out io.Writer, r *StressResult, details bool) int {
	width := outputColumns(out)
	if width == 0 {
		width = 120
	}
	return renderStressTextWidthDetails(env, out, r, width, details)
}

func renderStressTextWidth(env *Env, out io.Writer, r *StressResult, width int) int {
	return renderStressTextWidthDetails(env, out, r, width, false)
}

func renderStressTextWidthDetails(env *Env, out io.Writer, r *StressResult, width int, details bool) int {
	if width < 40 {
		width = 80
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Portfolio Stress  ·  %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	fmt.Fprintln(out)
	renderStressKV(out, "Action", stressHeadlineText(r), width, func(string) string {
		return stressHeadlineLabel(env, r)
	})
	renderStressKV(out, "Guidance", r.Summary, width, env.bold)
	if len(r.PrimaryDrivers) > 0 {
		renderStressKV(out, "Drivers", strings.Join(signalDisplayStrings(r.PrimaryDrivers), ", "), width, nil)
	}
	renderStressKV(out, "Next step", stressPlannerStepText(r.PlannerModeHint, r.PlannerReadiness), width, func(s string) string {
		return stressPlannerStepLabel(env, r.PlannerModeHint, r.PlannerReadiness)
	})
	fmt.Fprintln(out)

	fmt.Fprintln(out, "  Why this fired")
	renderStressSectionRow(out, "Market weather", stressMarketReadText(r), width, nil)
	renderStressSectionRow(out, "Portfolio shape", stressPortfolioFitText(r), width, nil)
	renderStressSectionRow(out, "Combined read", stressCombinedReadText(r), width, nil)
	renderStressMarketIndicators(env, out, r.MarketIndicators, width)

	renderStressWarnings(env, out, r.Warnings, width)

	if details {
		fmt.Fprintln(out)
		renderStressRowsStacked(env, out, r.Rows, width)
	}
	if r.Fingerprint.Key != "" {
		fmt.Fprintln(out)
		renderStressKV(out, "Alert ID", r.Fingerprint.Version+" "+r.Fingerprint.Key, width, env.dim)
	}
	return 0
}

func stressHeadlineText(r *StressResult) string {
	return strings.ToUpper(stressActionDisplay(r.Action)) + " · " + stressHeadlineReason(r)
}

func stressHeadlineLabel(env *Env, r *StressResult) string {
	text := stressHeadlineText(r)
	switch r.Action {
	case stressActionDefend:
		return env.bold(env.red(text))
	case stressActionWatch, stressActionRebalance, stressActionConfirmInputs:
		return env.bold(env.yellow(text))
	case stressActionDeploy:
		return env.bold(env.green(text))
	default:
		return env.bold(text)
	}
}

func stressActionDisplay(action string) string {
	switch action {
	case stressActionDefend:
		return "defend"
	case stressActionWatch:
		return "watch"
	case stressActionRebalance:
		return "rebalance"
	case stressActionDeploy:
		return "deploy"
	case stressActionConfirmInputs:
		return "confirm inputs"
	default:
		return "stand down"
	}
}

func stressHeadlineReason(r *StressResult) string {
	switch r.Action {
	case stressActionDefend:
		return "market stress confirmed against vulnerable portfolio"
	case stressActionWatch:
		if r.PortfolioFit == stressPortfolioFitLow || r.PortfolioFit == stressPortfolioFitUnknown {
			return "market pressure; portfolio fit is not a defense trigger"
		}
		return "market pressure with portfolio exposure"
	case stressActionRebalance:
		return "portfolio shape outside limits; market stress unconfirmed"
	case stressActionDeploy:
		return "constructive tape with clean inputs"
	case stressActionConfirmInputs:
		return "input health blocks the stress read"
	default:
		return "no market-context action"
	}
}

func stressMarketReadText(r *StressResult) string {
	return fmt.Sprintf("%s — %s", r.MarketConfirmation, stressMarketEvidence(r.Market))
}

func stressPortfolioFitText(r *StressResult) string {
	return fmt.Sprintf("%s — %s", r.PortfolioFit, stressPortfolioEvidence(r.Portfolio))
}

func stressCombinedReadText(r *StressResult) string {
	switch r.Action {
	case stressActionDefend:
		return "market confirmation and portfolio fit agree; defensive action is justified by this stress read."
	case stressActionWatch:
		return "market pressure is not strong enough for automatic defense; stage a plan and wait for confirmation."
	case stressActionRebalance:
		return "portfolio shape is high risk, but market weather is not the trigger; use portfolio-risk workflow."
	case stressActionDeploy:
		return "constructive tape is visible; size only inside the existing risk budget."
	case stressActionConfirmInputs:
		return "the monitor cannot separate signal from input failure yet."
	default:
		return "market weather and portfolio shape do not call for a stress action."
	}
}

type stressInputHealthRow struct {
	label string
	text  string
}

func stressInputHealthRows(r *StressResult) []stressInputHealthRow {
	rows := []stressInputHealthRow{{
		label: "Overall",
		text:  r.InputHealth,
	}}
	if r.Portfolio.DailyPnLPct == nil {
		rows = append(rows, stressInputHealthRow{label: "Warming input", text: "account daily P&L has not produced a usable frame yet"})
	}
	if len(r.Market.DegradedClusters) > 0 {
		rows = append(rows, stressInputHealthRow{label: "Degraded input", text: stressClusterList(r.Market.DegradedClusters)})
	}
	if len(r.Market.StaleClusters) > 0 {
		rows = append(rows, stressInputHealthRow{label: "Stale input", text: stressClusterList(r.Market.StaleClusters)})
	}
	if len(r.Market.PartialClusters) > 0 || len(r.Market.AmbiguousClusters) > 0 || len(r.Market.ComputingClusters) > 0 {
		rows = append(rows, stressInputHealthRow{label: "Incomplete input", text: stressAmbiguityEvidence(r.Market)})
	}
	for _, h := range r.SourceHealth {
		if h.Status == "" || h.Status == rpc.RegimeStatusOK {
			continue
		}
		rows = append(rows, stressInputHealthRow{label: "Source status", text: h.Source + " " + h.Status})
	}
	if len(rows) == 1 && r.InputHealth == stressInputOK {
		rows[0].text = "ok — account, positions, and regime inputs are usable"
	}
	return rows
}

func renderStressSectionRow(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 16
	available := max(width-4-labelW-1, 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, "", line)
		}
	}
}

func humanList(value string) string {
	clean := []string{}
	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		clean = append(clean, stressClusterDisplayName(part))
	}
	if len(clean) == 0 {
		return strings.TrimSpace(value)
	}
	if len(clean) == 1 {
		return clean[0]
	}
	if len(clean) == 2 {
		return clean[0] + " and " + clean[1]
	}
	return strings.Join(clean[:len(clean)-1], ", ") + ", and " + clean[len(clean)-1]
}

func stressClusterList(clusters []string) string {
	return humanList(strings.Join(clusters, ","))
}

func stressClusterDisplayName(cluster string) string {
	switch strings.ToLower(strings.TrimSpace(cluster)) {
	case "fx":
		return "FX"
	default:
		return strings.TrimSpace(cluster)
	}
}

func renderStressKV(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 10
	available := max(width-2-labelW-1, 24)
	lines := wrapVisibleText(value, available)
	for i, line := range lines {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "  %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "  %-*s %s\n", labelW, "", line)
		}
	}
}

func renderStressRowsStacked(env *Env, out io.Writer, rows []StressRow, width int) {
	fmt.Fprintln(out, "  Details")
	for _, row := range rows {
		if row.Title == "Portfolio stress" {
			continue
		}
		state := stressRiskStateLabel(env, row.Direction, row.Severity, false)
		fmt.Fprintf(out, "  %s · %s\n", row.Title, state)
		renderStressIndented(out, "guidance", row.Guidance, width, nil)
		if row.Evidence != "" {
			renderStressIndented(out, "evidence", row.Evidence, width, env.dim)
		}
	}
}

func renderStressMarketIndicators(env *Env, out io.Writer, indicators []StressMarketIndicator, width int) {
	if len(indicators) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Market indicators")
	nameW := 17
	statusW := 6
	asOfW := 10
	for _, row := range indicators {
		nameW = max(nameW, min(visibleLen(row.Name), 22))
		statusW = max(statusW, visibleLen(row.Status))
		asOfW = max(asOfW, min(visibleLen(ifNonEmpty(row.AsOf, "—")), 12))
	}
	detailW := max(width-4-nameW-2-statusW-2-asOfW-2, 24)
	header := fmt.Sprintf("    %s  %s  %s  %s",
		padRightVisible("INDICATOR", nameW),
		padRightVisible("STATE", statusW),
		padRightVisible("AS OF", asOfW),
		"READING / COMMENT")
	fmt.Fprintln(out, env.dim(header))
	for _, row := range indicators {
		detail := strings.TrimSpace(row.Reading)
		if row.Comment != "" {
			if detail != "" {
				detail += " — "
			}
			detail += row.Comment
		}
		if detail == "" {
			detail = "—"
		}
		lines := wrapVisibleText(detail, detailW)
		status := padRightVisible(stressIndicatorStatusLabel(env, row.Status), statusW)
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(out, "    %s  %s  %s  %s\n",
					padRightVisible(row.Name, nameW),
					status,
					padRightVisible(ifNonEmpty(row.AsOf, "—"), asOfW),
					line)
				continue
			}
			fmt.Fprintf(out, "    %s  %s  %s  %s\n",
				strings.Repeat(" ", nameW),
				strings.Repeat(" ", statusW),
				strings.Repeat(" ", asOfW),
				line)
		}
	}
}

func stressIndicatorStatusLabel(env *Env, status string) string {
	switch status {
	case "green":
		return env.green(status)
	case "amber":
		return env.yellow(status)
	case "red":
		return env.red(status)
	case "context":
		return env.dim(status)
	default:
		return env.dim(status)
	}
}

func renderStressIndented(out io.Writer, label, value string, width int, color func(string) string) {
	const labelW = 9
	available := max(width-4-labelW-1, 24)
	for i, line := range wrapVisibleText(value, available) {
		if color != nil {
			line = color(line)
		}
		if i == 0 {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, label, line)
		} else {
			fmt.Fprintf(out, "    %-*s %s\n", labelW, "", line)
		}
	}
}

func renderStressWarnings(env *Env, out io.Writer, warnings []string, width int) {
	if len(warnings) == 0 {
		return
	}
	type inputCheck struct {
		label string
		text  string
	}
	checks := []inputCheck{}
	for _, warning := range warnings {
		label, text := stressWarningLabel(warning)
		text = stressInputCheckText(label, text)
		if !stressInputCheckRenderable(label, text) {
			continue
		}
		checks = append(checks, inputCheck{label: label, text: text})
	}
	if len(checks) == 0 {
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "  Input checks")
	for _, check := range checks {
		label, text := check.label, check.text
		labelText := label + ":"
		available := max(width-4-visibleLen(labelText)-1, 24)
		lines := wrapVisibleText(text, available)
		labelText = stressWarningLabelColor(env, label, labelText)
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(out, "    %s %s\n", labelText, line)
			} else {
				fmt.Fprintf(out, "    %s %s\n", strings.Repeat(" ", visibleLen(label)+1), line)
			}
		}
	}
}

func stressInputCheckRenderable(label, text string) bool {
	if label != "context" {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "action:") &&
		!strings.Contains(lower, "no immediate fix") &&
		!strings.Contains(lower, "closed-session") &&
		!strings.Contains(lower, "context-only")
}

func signalDisplayStrings(ids []risk.SignalID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, signalDisplayString(id))
	}
	return out
}

func signalDisplayString(id risk.SignalID) string {
	switch id {
	case risk.SignalMarginCushionLow:
		return "margin cushion"
	case risk.SignalLookAheadCushionLow:
		return "look-ahead cushion"
	case risk.SignalPortfolioPnLShock:
		return "P&L shock"
	case risk.SignalFXCarryUnwind:
		return "FX carry unwind"
	case risk.SignalGammaRed:
		return "gamma red"
	case risk.SignalSingleNameExposureHigh:
		return "title exposure"
	case risk.SignalSingleNameDeltaHigh:
		return "title delta"
	case risk.SignalHeldUnderlyingPnLShock:
		return "held P&L shock"
	case risk.SignalHeldOptionExpiryConcentration:
		return "held expiry risk"
	case risk.SignalHeldLiquidityDegraded:
		return "held liquidity"
	case risk.SignalGrossExposureHigh:
		return "gross exposure"
	case risk.SignalNetDeltaHigh:
		return "net delta"
	case risk.SignalGrossDeltaHigh:
		return "gross delta"
	case risk.SignalRiskDataDegraded:
		return "data degraded"
	case risk.SignalMarketDataStale:
		return "market data stale"
	case risk.SignalOptionGreeksDegraded:
		return "option greeks"
	default:
		return string(id)
	}
}

func wrapVisibleText(text string, width int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return []string{""}
	}
	if width <= 0 {
		return []string{text}
	}
	words := strings.Fields(text)
	lines := []string{}
	line := ""
	for _, word := range words {
		for visibleLen(word) > width {
			head, tail := splitVisibleWord(word, width)
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			lines = append(lines, head)
			word = tail
		}
		if line == "" {
			line = word
			continue
		}
		if visibleLen(line)+1+visibleLen(word) <= width {
			line += " " + word
			continue
		}
		lines = append(lines, line)
		line = word
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

func splitVisibleWord(word string, width int) (string, string) {
	if width <= 0 {
		return word, ""
	}
	used := 0
	for i := range word {
		if used == width {
			return word[:i], word[i:]
		}
		used++
	}
	return word, ""
}

func stressWarningLabel(warning string) (string, string) {
	lower := strings.ToLower(warning)
	switch {
	case strings.Contains(lower, "error"):
		return "error", warning
	case strings.Contains(lower, "context_only") ||
		strings.Contains(lower, "context only") ||
		strings.Contains(lower, "displayed as context") ||
		strings.Contains(lower, "frozen") ||
		strings.Contains(lower, "closed"):
		return "context", warning
	case strings.Contains(lower, "stale"):
		return "refresh", warning
	case strings.Contains(lower, "computing") || strings.Contains(lower, "ambiguous") || strings.Contains(lower, "unranked"):
		return "verify", warning
	default:
		return "warning", warning
	}
}

func stressInputCheckText(label, warning string) string {
	lower := strings.ToLower(warning)
	switch {
	case strings.Contains(lower, "gamma_zero") && (strings.Contains(lower, "context_only") || strings.Contains(lower, "context only")):
		return "Gamma is after-hours/context-only. Action: no immediate fix; refresh during active option hours before using gamma as confirmation."
	case strings.Contains(lower, "funding") && strings.Contains(lower, "unranked"):
		return "Funding is not usable confirmation yet. Action: ignore it for escalation and rerun `canary regime` after the source updates."
	case strings.Contains(lower, "ambiguous clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s cannot confirm the stress read yet. Action: check the n/a row in Market indicators and verify with `canary regime` before escalating.", humanList(clusters))
	case strings.Contains(lower, "partial clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s is partially usable. Action: inspect the affected Market indicators row; rely only on fresh independent clusters for confirmation.", humanList(clusters))
	case strings.Contains(lower, "degraded clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("%s has degraded input quality. Action: inspect the source command before using it as confirmation.", humanList(clusters))
	case strings.Contains(lower, "regime cluster") && strings.Contains(lower, "unranked"):
		return "One or more regime clusters are unranked. Action: treat the stress read as context until the n/a Market indicators rows rank."
	case strings.Contains(lower, "stale clusters:"):
		clusters := strings.TrimSpace(warning[strings.LastIndex(warning, ":")+1:])
		return fmt.Sprintf("Refresh %s before escalation. Action: rerun `canary stress`; stale rows remain context, not fresh confirmation.", humanList(clusters))
	case strings.Contains(lower, "vix_term_structure") && strings.Contains(lower, "stale"):
		return "Volatility term structure is stale. Action: rerun `canary regime` or `canary stress` before treating vol as fresh confirmation."
	case strings.Contains(lower, "computing"):
		return warning + ". Action: wait for the daemon compute to finish, then rerun `canary stress`."
	case label == "error":
		return warning + ". Action: fix the source or daemon issue before relying on this stress read."
	case label == "warning":
		return warning + ". Action: inspect the affected row before escalating."
	default:
		return warning
	}
}

func stressWarningLabelColor(env *Env, label, text string) string {
	switch label {
	case "context":
		return env.dim(text)
	case "verify", "refresh":
		return env.yellow(text)
	case "error":
		return env.red(text)
	default:
		return env.yellow(text)
	}
}

func stressRiskStateLabel(env *Env, direction risk.SignalDirection, severity risk.SignalSeverity, current bool) string {
	label := stressRiskStateText(direction, severity)
	if current {
		label = "[" + label + "]"
	}
	switch severity {
	case risk.SeverityObserve:
		label = env.green(label)
	case risk.SeverityWatch:
		label = env.yellow(label)
	case risk.SeverityAct, risk.SeverityUrgent:
		label = env.red(label)
	}
	if current {
		label = env.bold(label)
	}
	return label
}

func stressRiskStateText(direction risk.SignalDirection, severity risk.SignalSeverity) string {
	directionLabel := stressDirectionLabel(direction)
	severityLabel := stressSeverityLabel(severity)
	if directionLabel == "" {
		return severityLabel
	}
	return directionLabel + " / " + severityLabel
}

func stressDirectionLabel(direction risk.SignalDirection) string {
	switch direction {
	case risk.DirectionDefensive:
		return "Defensive"
	case risk.DirectionConstructive:
		return "Constructive"
	case risk.DirectionRebalance:
		return "Rebalance"
	case risk.DirectionMixed:
		return "Mixed"
	case risk.DirectionDataQuality:
		return "Data quality"
	default:
		return ""
	}
}

func stressSeverityLabel(severity risk.SignalSeverity) string {
	switch severity {
	case risk.SeverityUrgent:
		return "Urgent"
	case risk.SeverityAct:
		return "Act"
	case risk.SeverityWatch:
		return "Watch"
	default:
		return "Observe"
	}
}

func stressPlannerStepLabel(env *Env, mode risk.PlannerMode, readiness risk.PlannerReadiness) string {
	label := stressPlannerStepText(mode, readiness)
	if readiness == risk.PlannerReadinessBlocked {
		return env.yellow(label)
	}
	if readiness == risk.PlannerReadinessReady {
		return env.red(label)
	}
	return label
}

func stressPlannerStepText(mode risk.PlannerMode, readiness risk.PlannerReadiness) string {
	switch mode {
	case risk.PlannerModeDefend:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before defend"
		}
		return "Review defensive actions"
	case risk.PlannerModeStage:
		return "Stage defensive review"
	case risk.PlannerModeDeploy:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before deploy"
		}
		return "Review deployment actions"
	case risk.PlannerModeRebalance:
		if readiness == risk.PlannerReadinessBlocked {
			return "Confirm data before rebalance"
		}
		return "Review rebalance actions"
	case risk.PlannerModeConfirmData:
		return "Confirm data"
	default:
		if readiness == risk.PlannerReadinessWatch {
			return "Watch"
		}
		return "No follow-up"
	}
}
