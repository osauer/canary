package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// runRules renders the daemon's advisory trading-rulebook checklist
// (internal-docs/design/trading-rulebook.md). Read-only; verdicts, ranking, and
// thresholds all come from the daemon — this renderer adds no policy.
func runRules(ctx context.Context, env *Env, args []string) int {
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

	if !res.Enabled {
		fmt.Fprintln(env.Stdout, "Trading rulebook is disabled (features.rulebook.enabled=false).")
		return 0
	}
	fmt.Fprintf(env.Stdout, "Trading rulebook — %s  policy %s v%d  status %s\n",
		res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.PolicyID, res.PolicyVersion, res.Status)
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-9s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		} else if h.Source == "earnings" && len(h.Notes) > 0 {
			fmt.Fprintf(env.Stdout, "  input %-9s ok (informational) %s\n", h.Source, strings.Join(h.Notes, "; "))
		}
	}
	fmt.Fprintln(env.Stdout)

	order := res.Ranked
	if len(order) != len(res.Rules) {
		order = order[:0]
		for i := range res.Rules {
			order = append(order, i)
		}
	}
	shown := 0
	for _, ix := range order {
		r := res.Rules[ix]
		if r.Status == risk.RuleStatusPass && !*all {
			continue
		}
		shown++
		fmt.Fprintf(env.Stdout, "%s  %2d %-22s %s\n", ruleGlyph(r.Status), r.Number, r.ID, ruleHeadline(r))
		fmt.Fprintf(env.Stdout, "      %s\n", r.Evidence)
		for i, o := range r.Offenders {
			if i >= 5 {
				fmt.Fprintf(env.Stdout, "      … %d more\n", len(r.Offenders)-i)
				break
			}
			line := o.Symbol
			if o.Leg != "" {
				line = o.Leg
			}
			if o.Note != "" {
				line += " — " + o.Note
			}
			fmt.Fprintf(env.Stdout, "      • %s\n", line)
		}
		for i, o := range r.Exempt {
			if i >= 5 {
				fmt.Fprintf(env.Stdout, "      … %d more exemptions\n", len(r.Exempt)-i)
				break
			}
			line := o.Symbol
			if o.Leg != "" {
				line = o.Leg
			}
			if o.Note != "" {
				line += " — " + o.Note
			}
			fmt.Fprintf(env.Stdout, "      exempt: %s\n", line)
		}
		for _, note := range r.Notes {
			fmt.Fprintf(env.Stdout, "      (%s)\n", note)
		}
	}
	if shown == 0 {
		if notEvaluated := res.BreachCounts[risk.RuleStatusNotEvaluated]; notEvaluated > 0 {
			fmt.Fprintf(env.Stdout, "%d rules were not evaluated; rerun with --all to see the full checklist.\n", notEvaluated)
		} else {
			fmt.Fprintf(env.Stdout, "All %d rules pass. Rerun with --all to see the full checklist.\n", len(res.Rules))
		}
	}
	passes := res.BreachCounts[risk.RuleStatusPass]
	fmt.Fprintf(env.Stdout, "\n%d act, %d watch, %d unknown, %d info, %d pass",
		res.BreachCounts[risk.RuleStatusAct], res.BreachCounts[risk.RuleStatusWatch],
		res.BreachCounts[risk.RuleStatusUnknown], res.BreachCounts[risk.RuleStatusInfo], passes)
	if n := res.BreachCounts[risk.RuleStatusNotEvaluated]; n > 0 {
		fmt.Fprintf(env.Stdout, ", %d not evaluated", n)
	}
	fmt.Fprintln(env.Stdout)
	if len(res.Earnings) > 0 {
		var unresolved, terminal, nonissuer []string
		for _, e := range res.Earnings {
			if e.Status == rpc.EarningsStatusNotApplicable {
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
			fmt.Fprintf(env.Stdout, "Earnings not applicable: %s — exact-contract evidence and provenance are available in --json.\n", strings.Join(terminal, ", "))
		}
		if len(nonissuer) > 0 {
			fmt.Fprintf(env.Stdout, "Issuer earnings not applicable: %s — exact broker identity is available in --json without exposing the contract identifier.\n", strings.Join(nonissuer, ", "))
		}
		if len(unresolved) > 0 {
			fmt.Fprintf(env.Stdout, "Earnings unresolved: %s — set an authoritative override with `canary settings set features.rulebook.earnings_overrides.<SYM>=YYYY-MM-DD` if needed (rules 6-8 stay unknown, never pass).\n",
				strings.Join(unresolved, ", "))
		}
		if wshEntitlementNotice(&res) {
			fmt.Fprintln(env.Stdout, "Earnings source notice: the optional Wall Street Horizon earnings feed is unavailable because this account lacks the WSH research subscription. Nasdaq remains active; names without a usable date stay unknown, never pass.")
		}
	}
	return 0
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

// renderRulesHistoryText prints the newest-first transition table, the
// shared index footer, and — when every row carries the same policy
// provenance — one compact policy line.
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
		fmt.Fprintln(out, "  no indexed rule transitions in this window")
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
	renderHistoryIndexFooter(env, out, res.Index)
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

func ruleGlyph(status string) string {
	switch status {
	case risk.RuleStatusAct:
		return "ACT "
	case risk.RuleStatusWatch:
		return "WARN"
	case risk.RuleStatusInfo:
		return "INFO"
	case risk.RuleStatusUnknown:
		return "?   "
	case risk.RuleStatusNotEvaluated:
		return "--  "
	default:
		return "ok  "
	}
}

func ruleHeadline(r risk.RuleRow) string {
	if r.Observed != nil && r.Threshold != nil {
		return fmt.Sprintf("%s (observed %.1f vs %.1f %s)", r.Title, *r.Observed, *r.Threshold, r.Unit)
	}
	if r.Reason != "" {
		return fmt.Sprintf("%s (%s)", r.Title, r.Reason)
	}
	return r.Title
}
