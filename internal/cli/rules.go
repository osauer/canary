package cli

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// sellOnlyLine states the result-level sell-only fact under the header,
// naming the rules behind it by number and title; "" while it is inactive.
func sellOnlyLine(res rpc.RulesResult) string {
	if !res.SellOnly.Active {
		return ""
	}
	names := make([]string, 0, len(res.SellOnly.Rules))
	for _, id := range res.SellOnly.Rules {
		name := id
		for _, r := range res.Rules {
			if r.ID == id {
				name = fmt.Sprintf("rule %d %s", r.Number, strings.ToLower(r.Title))
				break
			}
		}
		names = append(names, name)
	}
	return "  sell-only  " + strings.Join(names, " and ") + " at watch or act: buys work against the Rulebook (advisory)"
}

// runRules renders the daemon's advisory trading-rulebook checklist
// (internal-docs/design/trading-rulebook.md). Read-only; verdicts, ranking, and
// thresholds all come from the daemon — this renderer adds no policy.
func runRules(ctx context.Context, env *Env, args []string) int {
	// The dispatcher hoists flags ahead of positionals, so the subcommand
	// can follow its own flags.
	if idx := firstPositionalIndex(args); idx >= 0 && args[idx] == "policy" {
		return runRulesPolicy(ctx, env, append(append([]string{}, args[:idx]...), args[idx+1:]...))
	}
	if slicesContains(args, "history") {
		return runRulesHistory(ctx, env, args)
	}
	fs := flagSet(env, "rules")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	symbol := fs.String("symbol", "", "narrow offender lists to one underlying")
	all := fs.Bool("all", false, "show passing rules too (default shows breaches first, passes compact)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	var res rpc.RulesResult
	if err := env.Conn.Call(ctx, rpc.MethodRulesSnapshot, rpc.RulesSnapshotParams{Symbol: *symbol}, &res); err != nil {
		return fail(env, "rules: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	renderRulesText(env, env.Stdout, &res, *all)
	return 0
}

// renderRulesText prints the checklist: breaches first, each rule's evidence
// and offenders beneath it, wrapped to the terminal with a hanging indent.
// Rules the policy turned off fold into one line unless all is set; their
// count stays in the summary.
func renderRulesText(env *Env, out io.Writer, res *rpc.RulesResult, all bool) {
	if !res.Enabled {
		fmt.Fprintln(out, "Trading rulebook is disabled (features.rulebook.enabled=false).")
		return
	}
	width := outputColumns(out)
	source := ""
	if st := res.PolicyStatus; st != nil {
		source = " (compiled baseline)"
		if st.Source == "file" {
			source = " (your policy file)"
		}
	}
	writeRuleLines(out, "", "  ", fmt.Sprintf("%s — %s  policy %s v%d%s  status %s", env.bold("Trading rulebook"),
		res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.PolicyID, res.PolicyVersion, source, res.Status), width)
	if line := sellOnlyLine(*res); line != "" {
		writeRuleLines(out, "  ", "             ", strings.TrimSpace(line), width)
	}
	if st := res.PolicyStatus; st != nil && st.Message != "" {
		writeRuleLines(out, "  ", "            ", fmt.Sprintf("policy    %s: %s", st.Status, st.Message), width)
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			writeRuleLines(out, "  ", "                ", fmt.Sprintf("input %-9s %s %s", h.Source, h.Status, strings.Join(h.Notes, "; ")), width)
		} else if h.Source == "earnings" && len(h.Notes) > 0 {
			writeRuleLines(out, "  ", "                ", fmt.Sprintf("input %-9s ok (informational) %s", h.Source, strings.Join(h.Notes, "; ")), width)
		}
	}
	fmt.Fprintln(out)

	order := res.Ranked
	if len(order) != len(res.Rules) {
		order = order[:0]
		for i := range res.Rules {
			order = append(order, i)
		}
	}
	var shownRows []risk.RuleRow
	var turnedOff []string
	for _, ix := range order {
		r := res.Rules[ix]
		switch {
		case all:
		case r.Status == risk.RuleStatusPass:
			continue
		case r.Status == risk.RuleStatusNotEvaluated && r.Reason == risk.RuleReasonRuleOff:
			turnedOff = append(turnedOff, strconv.Itoa(r.Number))
			continue
		}
		shownRows = append(shownRows, r)
	}
	// Status and number sit in a ten-cell gutter; the headline, evidence,
	// offenders and notes all hang from the column after it.
	const gutter = "          "
	for _, r := range shownRows {
		// The rule id trails the headline, dim: the number and title are what
		// a reader scans, and the id stays at hand for --rule and the policy.
		prefix := fmt.Sprintf("%s %2d  ", ruleStatusLabel(env, r.Status), r.Number)
		writeRuleLinesTrail(out, prefix, gutter, ruleHeadline(r), env.dim(r.ID), width)
		writeRuleLines(out, gutter, gutter, r.Evidence, width)
		for i, o := range r.Offenders {
			if i >= 5 {
				fmt.Fprintf(out, "%s… %d more\n", gutter, len(r.Offenders)-i)
				break
			}
			writeRuleLines(out, gutter+"• ", gutter+"  ", offenderLine(r, o), width)
			for _, detail := range issuerDetailLines(o.Issuer, res.BaseCurrency) {
				writeRuleLines(out, gutter+"    ", gutter+"      ", detail, width)
			}
		}
		for i, o := range r.Exempt {
			if i >= 5 {
				fmt.Fprintf(out, "%s… %d more exemptions\n", gutter, len(r.Exempt)-i)
				break
			}
			line := o.Symbol
			if o.Leg != "" {
				line = o.Leg
			}
			if o.Note != "" {
				line += " — " + o.Note
			}
			writeRuleLines(out, gutter+"exempt: ", gutter+"  ", line, width)
		}
		for _, note := range r.Notes {
			writeRuleLines(out, gutter, gutter+" ", "("+note+")", width)
		}
	}
	if len(turnedOff) > 0 {
		var folded bytes.Buffer
		writeRuleLines(&folded, "--        ", gutter, "rules "+strings.Join(turnedOff, ", ")+" are turned off in the Rulebook policy (--all lists them)", width)
		for line := range strings.SplitSeq(strings.TrimSuffix(folded.String(), "\n"), "\n") {
			fmt.Fprintln(out, env.dim(line))
		}
	}
	if len(shownRows) == 0 && len(turnedOff) == 0 {
		if notEvaluated := res.BreachCounts[risk.RuleStatusNotEvaluated]; notEvaluated > 0 {
			fmt.Fprintf(out, "%d rules were not evaluated; rerun with --all to see the full checklist.\n", notEvaluated)
		} else {
			fmt.Fprintf(out, "All %d rules pass. Rerun with --all to see the full checklist.\n", len(res.Rules))
		}
	}
	passes := res.BreachCounts[risk.RuleStatusPass]
	act := fmt.Sprintf("%d act", res.BreachCounts[risk.RuleStatusAct])
	if res.BreachCounts[risk.RuleStatusAct] > 0 {
		act = env.red(act)
	}
	watch := fmt.Sprintf("%d watch", res.BreachCounts[risk.RuleStatusWatch])
	if res.BreachCounts[risk.RuleStatusWatch] > 0 {
		watch = env.yellow(watch)
	}
	fmt.Fprintf(out, "\n%s, %s, %d unknown, %d info, %d pass", act, watch,
		res.BreachCounts[risk.RuleStatusUnknown], res.BreachCounts[risk.RuleStatusInfo], passes)
	if n := res.BreachCounts[risk.RuleStatusNotEvaluated]; n > 0 {
		fmt.Fprintf(out, ", %d not evaluated", n)
	}
	fmt.Fprintln(out)
	if len(res.Earnings) > 0 {
		var unresolved, terminal, nonissuer, byType []string
		for _, e := range res.Earnings {
			if e.Status == rpc.EarningsStatusNotApplicable {
				// A security-type classification is weaker authority than a
				// broker identity proof; the line must not claim the proof.
				if e.Source == "security_type" {
					byType = append(byType, fmt.Sprintf("%s (%s)", e.Symbol, strings.ToLower(nonEmpty(e.SecurityType, "no issuer"))))
					continue
				}
				nonissuer = append(nonissuer, fmt.Sprintf("%s (broker-proven nonissuer)", e.Symbol))
				continue
			}
			if e.Status == rpc.EarningsStatusTerminalNonReporting {
				review := ""
				if e.Terminal != nil && !e.Terminal.RevalidateAfter.IsZero() {
					review = "; review by " + e.Terminal.RevalidateAfter.Format("2006-01-02")
				}
				terminal = append(terminal, fmt.Sprintf("%s (terminal/non-reporting%s)", e.Symbol, review))
				continue
			}
			if e.Source == "unknown" || e.Status != "" && e.Status != rpc.EarningsStatusDate {
				reason := e.Reason
				if reason == "" {
					reason = e.Status
				}
				unresolved = append(unresolved, fmt.Sprintf("%s (%s)", e.Symbol, earningsOutcomeLabel(reason)))
			}
		}
		if len(terminal) > 0 {
			writeRuleLines(out, "", "  ", fmt.Sprintf("Earnings not applicable: %s — exact-contract evidence and provenance are available in --json.", strings.Join(terminal, ", ")), width)
		}
		if len(nonissuer) > 0 {
			writeRuleLines(out, "", "  ", fmt.Sprintf("Issuer earnings not applicable: %s — exact broker identity is available in --json without exposing the contract identifier.", strings.Join(nonissuer, ", ")), width)
		}
		if len(byType) > 0 {
			writeRuleLines(out, "", "  ", "Issuer earnings not applicable by security type: "+strings.Join(byType, ", ")+".", width)
		}
		if len(unresolved) > 0 {
			writeRuleLines(out, "", "  ", fmt.Sprintf("Earnings unresolved: %s — set an authoritative override with `canary settings set features.rulebook.earnings_overrides.<SYM>=YYYY-MM-DD` if needed (rules 6-8 stay unknown, never pass).",
				strings.Join(unresolved, ", ")), width)
		}
		if wshEntitlementNotice(res) {
			writeRuleLines(out, "", "  ", "Earnings source notice: the optional Wall Street Horizon earnings feed is unavailable because this account lacks the WSH research subscription. Nasdaq remains active; names without a usable date stay unknown, never pass.", width)
		}
	}
}

// writeRuleLines prints text after first, wrapping at width with rest as the
// hanging indent. Width 0 (a pipe or file) leaves the line whole for grep.
func writeRuleLines(out io.Writer, first, rest, text string, width int) {
	writeRuleLinesTrail(out, first, rest, text, "", width)
}

// writeRuleLinesTrail is writeRuleLines with a trailing tag set two cells
// (one when only one is left) after the text on its last line, or on its own
// continuation line when it does not fit there.
func writeRuleLinesTrail(out io.Writer, first, rest, text, trail string, width int) {
	// A line that fits prints as written, keeping its column padding; only
	// a line that would overrun is re-flowed word by word.
	lines := []string{text}
	if width > 0 && visibleLen(first)+visibleLen(text) > width {
		lines = wrapVisibleText(text, width-visibleLen(first))
		if len(lines) > 1 {
			tail := strings.Join(lines[1:], " ")
			lines = append(lines[:1], wrapVisibleText(tail, width-visibleLen(rest))...)
		}
	}
	if trail != "" {
		last := len(lines) - 1
		indent := visibleLen(rest)
		if last == 0 {
			indent = visibleLen(first)
		}
		switch room := width - indent - visibleLen(lines[last]) - visibleLen(trail); {
		case width <= 0 || room >= 2:
			lines[last] += "  " + trail
		case room == 1:
			lines[last] += " " + trail
		default:
			lines = append(lines, trail)
		}
	}
	for i, line := range lines {
		indent := rest
		if i == 0 {
			indent = first
		}
		fmt.Fprintln(out, indent+line)
	}
}

// wshEntitlementNotice recognizes only the durable WSH entitlement result.
// Other provider failures remain their own typed outcomes; adapters must not
// infer account subscription state from an arbitrary failure or free text.
func wshEntitlementNotice(res *rpc.RulesResult) bool {
	if res == nil {
		return false
	}
	for _, earnings := range res.Earnings {
		for _, provider := range earnings.Providers {
			failure := provider.LastFailure
			if provider.Provider != "ibkr_wsh" || failure == nil ||
				failure.Code != rpc.SourceFailureNotEntitled || failure.Retryable ||
				(failure.Stage != rpc.SourceFailureStageWSHMetadata && failure.Stage != rpc.SourceFailureStageWSHEvent) {
				continue
			}
			return true
		}
	}
	return false
}

func earningsOutcomeLabel(reason string) string {
	switch strings.TrimSpace(reason) {
	case rpc.EarningsStatusNoDatePublished:
		return "no date published"
	case rpc.EarningsStatusUnsupportedSecurity:
		return "unsupported security"
	case rpc.EarningsStatusFormatChange:
		return "provider format changed"
	case rpc.EarningsStatusTransportFailure:
		return "provider transport failed"
	case rpc.EarningsStatusConflictingSources:
		return "providers conflict"
	case rpc.EarningsStatusNotApplicable:
		return "broker-proven nonissuer"
	case "date_elapsed":
		return "published date elapsed"
	case "not_observed", "":
		return "not checked yet"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
}

// runRulesHistory renders the daemon's derived rule-transition index
// (rules.history). Evidence strings are journal free text — rendered,
// truncated to the terminal, never parsed into authority.
func runRulesHistory(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "rules")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	since := fs.String("since", "", "inclusive lower boundary: YYYY-MM-DD UTC day or RFC3339 timestamp")
	until := fs.String("until", "", "upper boundary: YYYY-MM-DD UTC day (whole day included) or RFC3339 timestamp")
	rule := fs.String("rule", "", "exact rule id filter (e.g. single_name_exposure)")
	limit := fs.Int("limit", 0, "max rows, newest first (default 50, max 500)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "history" {
		rest = rest[1:]
	}
	if len(rest) != 0 {
		return fail(env, "rules history: usage is `canary rules history [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--rule ID] [--limit N] [--json]`")
	}
	params := rpc.RulesHistoryParams{
		Since: strings.TrimSpace(*since),
		Until: strings.TrimSpace(*until),
		Rule:  strings.TrimSpace(*rule),
		Limit: *limit,
	}
	var res rpc.RulesHistoryResult
	if err := env.Conn.Call(ctx, rpc.MethodRulesHistory, params, &res); err != nil {
		return fail(env, "rules history: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderRulesHistoryText(env, env.Stdout, &res)
	return 0
}

// renderRulesHistoryText prints the newest-first transition table, its direct
// storage authority, and — when every row carries the same policy provenance —
// one compact policy line.
func renderRulesHistoryText(env *Env, out io.Writer, res *rpc.RulesHistoryResult) {
	width := outputColumns(out)
	if width < 60 {
		width = 120
	}
	header := fmt.Sprintf("Rules history  %s → %s UTC  %d of %d rows",
		res.Since.UTC().Format("2006-01-02"), res.Until.UTC().Format("2006-01-02"), res.Count, res.TotalCount)
	if res.Truncated {
		header += " (truncated; raise --limit)"
	}
	fmt.Fprintln(out, header)
	if len(res.Entries) == 0 {
		fmt.Fprintln(out, "  no rule transitions recorded in this window")
	} else {
		ruleW, transW := 4, 10
		for _, e := range res.Entries {
			ruleW = min(max(ruleW, len(e.Rule)), 24)
			transW = min(max(transW, len(ruleTransitionLabel(e))), 20)
		}
		fmt.Fprintf(out, "  %s\n", env.dim(fmt.Sprintf("%-16s  %-*s  %-*s  %s",
			"AT (UTC)", ruleW, "RULE", transW, "WAS→STATUS", "EVIDENCE")))
		evidenceW := max(width-(2+16+2+ruleW+2+transW+2), 16)
		for _, e := range res.Entries {
			fmt.Fprintf(out, "  %-16s  %-*s  %-*s  %s\n",
				e.At.UTC().Format("2006-01-02 15:04"),
				ruleW, truncateVisible(e.Rule, ruleW),
				transW, truncateVisible(ruleTransitionLabel(e), transW),
				truncateVisible(e.Evidence, evidenceW))
		}
	}
	fmt.Fprintf(out, "  %s\n", env.dim("source: daemon.db · direct history read"))
	if id, version, uniform := uniformRulesPolicy(res.Entries); uniform {
		fmt.Fprintf(out, "  %s\n", env.dim(fmt.Sprintf("policy %s v%d", id, version)))
	}
}

// ruleTransitionLabel renders was→status; a first observation (empty was)
// reads as the bare status.
func ruleTransitionLabel(e rpc.RuleTransitionEntry) string {
	if e.Was == "" {
		return e.Status
	}
	return e.Was + "→" + e.Status
}

// uniformRulesPolicy reports the single (policy id, version) shared by
// every entry, when there is exactly one.
func uniformRulesPolicy(entries []rpc.RuleTransitionEntry) (string, int, bool) {
	id, version := "", 0
	for _, e := range entries {
		if e.PolicyID == "" {
			return "", 0, false
		}
		if id == "" {
			id, version = e.PolicyID, e.PolicyVersion
			continue
		}
		if e.PolicyID != id || e.PolicyVersion != version {
			return "", 0, false
		}
	}
	return id, version, id != ""
}

// ruleStatusLabel is the five-cell status column: the status words the
// summary line counts, tinted red for act, yellow for watch and unknown.
func ruleStatusLabel(env *Env, status string) string {
	switch status {
	case risk.RuleStatusAct:
		return env.red("ACT  ")
	case risk.RuleStatusWatch:
		return env.yellow("WATCH")
	case risk.RuleStatusInfo:
		return "INFO "
	case risk.RuleStatusUnknown:
		return env.yellow("?    ")
	case risk.RuleStatusNotEvaluated:
		return env.dim("--   ")
	default:
		return env.green("ok   ")
	}
}

// ruleHeadline sets the observed value beside the limit the daemon reported
// for the row's status. A two-band rule also names both bands, so a watch row
// shows how far the act level is; the renderer never picks a band itself.
func ruleHeadline(r risk.RuleRow) string {
	if r.Observed != nil && r.Threshold != nil {
		if r.WatchThreshold != nil && r.ActThreshold != nil {
			return fmt.Sprintf("%s (observed %.1f vs %s%s; watch %s, act %s)", r.Title, *r.Observed,
				ruleLimitText(*r.Threshold), ruleUnitSuffix(r.Unit), ruleLimitText(*r.WatchThreshold), ruleLimitText(*r.ActThreshold))
		}
		return fmt.Sprintf("%s (observed %.1f vs %.1f%s)", r.Title, *r.Observed, *r.Threshold, ruleUnitSuffix(r.Unit))
	}
	if r.Reason != "" {
		return fmt.Sprintf("%s (%s)", r.Title, r.Reason)
	}
	return r.Title
}

// ruleUnitSuffix joins a unit to the number before it: a percent unit sits
// against the number ("30% NLV"), any other unit after a space ("7 days").
func ruleUnitSuffix(unit string) string {
	if unit == "" || strings.HasPrefix(unit, "%") {
		return unit
	}
	return " " + unit
}

// ruleLimitText prints a policy limit as configured (7.5 stays 7.5, 40 stays
// 40), matching how the daemon's evidence line quotes it.
func ruleLimitText(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// offenderLine renders one offender. On rules with watch and act levels, and
// on the watch-only rules 16-18, each offender carries its own band, so a
// second offender between the levels reads watch although the row acts.
func offenderLine(r risk.RuleRow, o risk.RuleOffender) string {
	line := o.Symbol
	if o.Leg != "" {
		line = o.Leg
	}
	var parts []string
	if o.Status == risk.RuleStatusAct || o.Status == risk.RuleStatusWatch {
		parts = append(parts, fmt.Sprintf("%s at %s", o.Status, offenderObserved(r.Unit, o.Observed)))
	}
	if o.Note != "" {
		parts = append(parts, o.Note)
	}
	if len(parts) > 0 {
		line += " — " + strings.Join(parts, ": ")
	}
	return line
}

func offenderObserved(unit string, v float64) string {
	if strings.HasPrefix(unit, "%") {
		return fmt.Sprintf("%.1f%s", v, unit)
	}
	return strings.TrimSpace(fmt.Sprintf("%.1f %s", v, unit))
}

// issuerDetailLines lists what rule 1 netted for one issuer: the lines it
// joined and the legs that move most at the worst price, losses and hedge
// gains alike, with the hedges credited or not and the legs that keep losing
// as the price rises.
func issuerDetailLines(x *risk.IssuerExposure, ccy string) []string {
	if x == nil {
		return nil
	}
	var out []string
	if len(x.Lines) > 1 || (len(x.Lines) == 1 && !strings.EqualFold(x.Lines[0], x.Issuer)) {
		out = append(out, "lines "+strings.Join(x.Lines, ", "))
	}
	legs := slices.Clone(x.Legs)
	slices.SortStableFunc(legs, func(a, b risk.IssuerLeg) int { return cmp.Compare(math.Abs(b.LossBase), math.Abs(a.LossBase)) })
	for i, l := range legs {
		if i >= 4 {
			out = append(out, fmt.Sprintf("… %d more legs (--json has all)", len(legs)-i))
			break
		}
		qty := fmt.Sprintf("%+g", l.Quantity)
		if l.Kind == risk.IssuerLegStock {
			qty = fmt.Sprintf("%g sh", l.Quantity)
		}
		effect := "flat"
		switch {
		case l.LossBase > 0:
			effect = "loses " + strings.TrimSpace(formatMoneyCcy(l.LossBase, ccy))
		case l.LossBase < 0:
			effect = "gains " + strings.TrimSpace(formatMoneyCcy(-l.LossBase, ccy))
		}
		line := fmt.Sprintf("%s  %s  %s", l.Leg, qty, effect)
		var flags []string
		switch l.Hedge {
		case risk.IssuerHedgeCredited:
			flags = append(flags, "hedge credited")
		case risk.IssuerHedgeUncredited:
			flags = append(flags, "hedge not credited")
		}
		if l.Unbounded {
			flags = append(flags, "unbounded, sized at the takeover gap")
		}
		if l.Note != "" {
			flags = append(flags, l.Note)
		}
		if len(flags) > 0 {
			line += "  (" + strings.Join(flags, "; ") + ")"
		}
		out = append(out, line)
	}
	return out
}
