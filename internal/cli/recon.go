package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runRecon renders the post-trade reconciliation report
// (internal-docs/design/post-trade-truth.md): broker statement flows vs. the
// declared capital-event ledger. show is read-only; dismiss is a human-only
// governance write the daemon refuses from agent origins. The report id
// printed here is what `canary policy capital-event reconcile`
// signs off.
func runRecon(ctx context.Context, env *Env, args []string) int {
	if len(args) == 1 && helpArg(args[0]) {
		printReconUsage(env)
		return 0
	}
	sub := "show"
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	if sub == "help" {
		if len(args) == 0 {
			printReconUsage(env)
			return 0
		}
		if len(args) == 1 {
			return printReconActionUsage(env, args[0])
		}
		return fail(env, "recon help: expected one action name")
	}
	if len(args) == 1 && helpArg(args[0]) {
		return printReconActionUsage(env, sub)
	}
	switch sub {
	case "show":
		return runReconShow(ctx, env, args)
	case "backtest":
		return runReconBacktest(ctx, env, args)
	case "equity":
		return runReconEquity(ctx, env, args)
	case "dismiss":
		return runReconDismiss(ctx, env, args)
	default:
		return fail(env, "recon: unknown subcommand %q (try `canary recon show`)", sub)
	}
}

func printReconUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "canary recon — compare retained broker statements with Canary's capital ledger")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Actions:")
	fmt.Fprintln(env.Stdout, "  show       Show the current report and unresolved statement/ledger differences.")
	fmt.Fprintln(env.Stdout, "  backtest   Replay statement equity against runtime drawdown history.")
	fmt.Fprintln(env.Stdout, "  equity     Show the retained daily statement-equity timeline and capital events.")
	fmt.Fprintln(env.Stdout, "  dismiss    Mark one exceptional report line as deliberately not being a ledger event.")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Start with `canary recon show`. Add --refresh to request one background statement fetch.")
	fmt.Fprintln(env.Stdout, "A qualifying clean report extends the policy clock automatically; routine clean reports")
	fmt.Fprintln(env.Stdout, "do not need manual sign-off.")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Only dismiss is a human-only write. Run `canary recon help dismiss` or")
	fmt.Fprintln(env.Stdout, "`canary recon dismiss --help` before using it.")
}

func printReconActionUsage(env *Env, action string) int {
	switch action {
	case "show":
		fmt.Fprintln(env.Stdout, "canary recon show — inspect the current reconciliation report")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Usage: canary recon show [--refresh] [--json]")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "This is read-only. --refresh requests one background Flex statement fetch before reporting.")
	case "backtest":
		fmt.Fprintln(env.Stdout, "canary recon backtest — replay statement equity against drawdown history")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Usage: canary recon backtest [--refresh] [--json]")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "This is read-only and helps diagnose high-water marks and historical threshold crossings.")
	case "equity":
		fmt.Fprintln(env.Stdout, "canary recon equity — inspect retained statement equity and capital events")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Usage: canary recon equity [--since TIME] [--until TIME] [--limit N] [--json]")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "This is read-only. Results are newest first and show index health when ingestion is incomplete.")
	case "dismiss":
		fmt.Fprintln(env.Stdout, "canary recon dismiss — classify one exceptional report line as not being a ledger event")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Usage: canary recon dismiss --line ID --reason TEXT [--json]")
		fmt.Fprintln(env.Stdout)
		fmt.Fprintln(env.Stdout, "Use the line ID from `canary recon show`. This human-only action journals your reason")
		fmt.Fprintln(env.Stdout, "and marks the exception dismissed; it does not delete or alter the retained broker statement.")
	default:
		return fail(env, "recon help: unknown action %q (choose show, backtest, equity, or dismiss)", action)
	}
	return 0
}

// runReconEquity renders the statement-derived daily equity series joined
// with declared capital events (recon.equity) from the daemon's derived
// history index. Read-only: statements and journals stay the evidence of
// record.
func runReconEquity(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon equity")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	since := fs.String("since", "", "inclusive lower boundary: YYYY-MM-DD UTC day or RFC3339 timestamp")
	until := fs.String("until", "", "upper boundary: YYYY-MM-DD UTC day (whole day included) or RFC3339 timestamp")
	limit := fs.Int("limit", 0, "max equity days, newest first (default 200, max 1000)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "recon equity: usage is `canary recon equity [--since YYYY-MM-DD|RFC3339] [--until YYYY-MM-DD|RFC3339] [--limit N] [--json]`")
	}
	params := rpc.ReconEquityParams{
		Since: strings.TrimSpace(*since),
		Until: strings.TrimSpace(*until),
		Limit: *limit,
	}
	var res rpc.ReconEquityResult
	if err := env.Conn.Call(ctx, rpc.MethodReconEquity, params, &res); err != nil {
		return fail(env, "recon equity: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderReconEquityText(env, env.Stdout, &res)
	return 0
}

// renderReconEquityText prints the newest-first equity-day table with declared
// capital events interleaved at their place in the timeline, followed by the
// direct storage authority shared by both views.
func renderReconEquityText(env *Env, out io.Writer, res *rpc.ReconEquityResult) {
	header := fmt.Sprintf("Statement equity  %s → %s UTC  %d of %d days",
		res.Since.UTC().Format("2006-01-02"), res.Until.UTC().Format("2006-01-02"), res.Count, res.TotalCount)
	if res.Truncated {
		header += " (truncated; raise --limit)"
	}
	fmt.Fprintln(out, header)
	if len(res.Days) == 0 && len(res.Events) == 0 {
		fmt.Fprintln(out, "  no statement equity days in this window")
	} else {
		fmt.Fprintf(out, "  %s\n", env.dim(fmt.Sprintf("%-10s  %14s  %s", "DAY", "EQUITY(BASE)", "SOURCE")))
		events := res.Events // newest first, like Days
		ei := 0
		printEventsNewerThan := func(cutoff time.Time) {
			for ei < len(events) && (cutoff.IsZero() || !events[ei].At.Before(cutoff)) {
				ev := events[ei]
				line := fmt.Sprintf("%s  %s %s", ev.At.UTC().Format("2006-01-02 15:04"), ev.Type, formatEquityAmount(ev.AmountBase))
				if ev.Origin != "" {
					line += "  " + ev.Origin
				}
				if ev.Note != "" {
					line += "  " + ev.Note
				}
				fmt.Fprintf(out, "  %s\n", env.yellow("· "+line))
				ei++
			}
		}
		for _, day := range res.Days {
			if dayStart, err := time.ParseInLocation("2006-01-02", day.Day, time.UTC); err == nil {
				printEventsNewerThan(dayStart.Add(24 * time.Hour))
			}
			fmt.Fprintf(out, "  %-10s  %14.2f  %s\n", day.Day, day.EquityBase, env.dim(day.SourceStmt))
		}
		printEventsNewerThan(time.Time{})
		if res.EventsTruncated {
			fmt.Fprintf(out, "  %s\n", env.dim("older capital events truncated"))
		}
	}
	fmt.Fprintf(out, "  %s\n", env.dim("source: daemon.db · retained statement projection and declared capital events"))
}

func formatEquityAmount(amount float64) string {
	if amount == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", amount)
}

func runReconShow(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon show")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	refresh := fs.Bool("refresh", false, "kick one background Flex statement fetch before reporting")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.ReconResult
	if err := env.Conn.Call(ctx, rpc.MethodReconSnapshot, rpc.ReconSnapshotParams{Refresh: *refresh}, &res); err != nil {
		return fail(env, "recon: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	fmt.Fprintf(env.Stdout, "Reconciliation — %s  status %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.Status)
	if res.ReportID != "" {
		fmt.Fprintf(env.Stdout, "  report %s  statements as of %s  coverage %s → %s\n",
			res.ReportID, res.StatementAsOf.Local().Format("2006-01-02 15:04"),
			res.CoverageFrom.Format("2006-01-02"), res.CoverageTo.Format("2006-01-02"))
	}
	if res.Message != "" {
		fmt.Fprintf(env.Stdout, "  note: %s\n", res.Message)
	}
	if !res.Fetch.Configured {
		fmt.Fprintln(env.Stdout, "  fetch: [flex] is not configured (set enabled, query_id, and the token file)")
	} else if res.Fetch.LastError != "" {
		fmt.Fprintf(env.Stdout, "  fetch: last attempt %s failed: %s\n", res.Fetch.LastAttempt.Local().Format("2006-01-02 15:04"), res.Fetch.LastError)
	} else if !res.Fetch.LastSuccess.IsZero() {
		fmt.Fprintf(env.Stdout, "  fetch: last success %s\n", res.Fetch.LastSuccess.Local().Format("2006-01-02 15:04"))
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-11s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}

	if len(res.Counts) > 0 {
		fmt.Fprintf(env.Stdout, "\n  matched %d", res.Counts["matched"])
		if n := res.Counts[rpc.ReconConfirmed]; n > 0 {
			fmt.Fprintf(env.Stdout, "  %s %d", rpc.ReconConfirmed, n)
		}
		for _, cat := range []string{rpc.ReconMissingFromLedger, rpc.ReconLedgerOnly, rpc.ReconAmountMismatch, rpc.ReconDateMismatch, rpc.ReconAmbiguous, rpc.ReconUncategorized} {
			if n := res.Counts[cat]; n > 0 {
				fmt.Fprintf(env.Stdout, "  %s %d", cat, n)
			}
		}
		fmt.Fprintf(env.Stdout, "  — %d unresolved\n", res.Unresolved)
	}
	if len(res.Confirmed) > 0 {
		fmt.Fprintf(env.Stdout, "\n  confirmed by broker: %d flow(s) — no declaration needed\n", len(res.Confirmed))
		for _, row := range res.Confirmed {
			fmt.Fprintln(env.Stdout, formatReconDisclosedFlow(row))
		}
	}
	for _, ex := range res.Exceptions {
		mark := "•"
		if ex.Dismissed {
			mark = "✓"
		}
		category := ex.Category
		if ex.PreGenesis {
			category += " [pre-genesis]"
		}
		fmt.Fprintf(env.Stdout, "  %s %-19s %s", mark, category, ex.LineID)
		if ex.AmountBase != nil {
			fmt.Fprintf(env.Stdout, "  %.2f", *ex.AmountBase)
		}
		if !ex.ValueDate.IsZero() {
			fmt.Fprintf(env.Stdout, "  %s", ex.ValueDate.Format("2006-01-02"))
		}
		if ex.Description != "" {
			fmt.Fprintf(env.Stdout, "  %s", ex.Description)
		}
		fmt.Fprintln(env.Stdout)
		if ex.Note != "" {
			fmt.Fprintf(env.Stdout, "      %s\n", ex.Note)
		}
		if ex.Dismissed {
			fmt.Fprintf(env.Stdout, "      dismissed: %s\n", ex.DismissReason)
		}
	}
	if len(res.Baseline) > 0 {
		fmt.Fprintf(env.Stdout, "\n  baseline (pre-genesis, before %s): %d flow(s) — no action needed\n",
			res.GenesisAt.UTC().Format("2006-01-02"), len(res.Baseline))
		for _, row := range res.Baseline {
			amount := "—"
			if row.AmountBase != nil {
				amount = fmt.Sprintf("%.2f", *row.AmountBase)
			}
			fmt.Fprintf(env.Stdout, "    %s  %s  %s", row.ValueDate.UTC().Format("2006-01-02"), row.Type, amount)
			if row.Description != "" {
				fmt.Fprintf(env.Stdout, "  %s", row.Description)
			}
			fmt.Fprintln(env.Stdout)
		}
	}
	if res.Equity != nil {
		fmt.Fprintf(env.Stdout, "\n  equity check: statement %s %.2f", res.Equity.StatementDate.Format("2006-01-02"), res.Equity.StatementTotalBase)
		if res.Equity.DivergencePct != nil {
			fmt.Fprintf(env.Stdout, "  vs runtime %+.2f%%", *res.Equity.DivergencePct)
		} else if !res.Equity.SameDay {
			fmt.Fprint(env.Stdout, "  same-day runtime sample unavailable")
		}
		fmt.Fprintln(env.Stdout)
	}
	if res.StatementCumFlowsBase != nil && !res.LastAutoExtendedAt.IsZero() {
		fmt.Fprintln(env.Stdout, formatReconAutoExtend(res))
	}
	if res.Status == rpc.ReconStatusActive && res.Unresolved == 0 && res.ReportID != "" {
		if res.StatementCumFlowsBase != nil {
			fmt.Fprintln(env.Stdout, reconCleanEvidenceMessage(res))
		} else {
			fmt.Fprintln(env.Stdout, "\nClean. Sign off with: canary policy capital-event reconcile")
		}
	}
	return 0
}

func formatReconDisclosedFlow(row rpc.ReconException) string {
	amount := "—"
	if row.AmountBase != nil {
		amount = fmt.Sprintf("%.2f", *row.AmountBase)
	}
	line := fmt.Sprintf("    %s  %s  %s", row.ValueDate.UTC().Format("2006-01-02"), row.Type, amount)
	if row.Description != "" {
		line += "  " + row.Description
	}
	return line
}

func formatReconAutoExtend(res rpc.ReconResult) string {
	current := ""
	if res.LastAutoExtendReportID == res.ReportID {
		current = " (current report)"
	}
	return fmt.Sprintf("  automatic evidence: report %s extended the clock %s%s",
		res.LastAutoExtendReportID, res.LastAutoExtendedAt.Local().Format("2006-01-02 15:04"), current)
}

func reconCleanEvidenceMessage(res rpc.ReconResult) string {
	if res.LastAutoExtendReportID == res.ReportID && !res.LastAutoExtendedAt.IsZero() {
		return "\nClean. This report has extended the reconcile clock automatically; human sign-off remains available for exceptional operation."
	}
	return "\nClean report. Automatic extension has not been recorded; the policy or evidence gate may be refusing it. Human sign-off remains available for exceptional operation."
}

func runReconBacktest(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon backtest")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	refresh := fs.Bool("refresh", false, "kick one background Flex statement fetch before reporting")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	var res rpc.ReconBacktestResult
	if err := env.Conn.Call(ctx, rpc.MethodReconBacktest, rpc.ReconSnapshotParams{Refresh: *refresh}, &res); err != nil {
		return fail(env, "recon backtest: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}

	fmt.Fprintf(env.Stdout, "Recon backtest — %s  status %s\n", res.AsOf.Local().Format("2006-01-02 15:04 MST"), res.Status)
	if res.ReportID != "" {
		fmt.Fprintf(env.Stdout, "  report %s  statements as of %s  coverage %s → %s\n",
			res.ReportID, res.StatementAsOf.Local().Format("2006-01-02 15:04"),
			res.CoverageFrom.Format("2006-01-02"), res.CoverageTo.Format("2006-01-02"))
	}
	if !res.GenesisAt.IsZero() || res.PolicyFingerprint != nil {
		fmt.Fprint(env.Stdout, "  runtime genesis ")
		if res.GenesisAt.IsZero() {
			fmt.Fprint(env.Stdout, "not recorded")
		} else {
			fmt.Fprint(env.Stdout, res.GenesisAt.Format("2006-01-02"))
		}
		if res.PolicyFingerprint != nil {
			fmt.Fprintf(env.Stdout, "  policy %s…", truncate(res.PolicyFingerprint.Key, 12))
		}
		fmt.Fprintln(env.Stdout)
	}
	if res.Message != "" {
		fmt.Fprintf(env.Stdout, "  note: %s\n", res.Message)
	}
	for _, h := range res.InputHealth {
		if h.Status != "ok" {
			fmt.Fprintf(env.Stdout, "  input %-11s %s %s\n", h.Source, h.Status, strings.Join(h.Notes, "; "))
		}
	}
	if res.Status != rpc.ReconStatusActive && res.Status != rpc.ReconStatusDegraded {
		return 0
	}

	fmt.Fprintln(env.Stdout, "\n  Flows over the window (review this list against memory — the R3 gate):")
	if len(res.Flows) == 0 {
		fmt.Fprintln(env.Stdout, "    none — the window contains no external flows")
	}
	for _, flow := range res.Flows {
		status := flow.Status
		if flow.PreGenesis {
			status += " [pre-genesis]"
		}
		if flow.Dismissed {
			status += " [dismissed]"
		}
		amount := "—"
		if flow.AmountBase != nil {
			amount = fmt.Sprintf("%.2f", *flow.AmountBase)
		}
		fmt.Fprintf(env.Stdout, "    %s  %s  %s  %s",
			flow.ValueDate.Format("2006-01-02"), flow.Type, amount, status)
		if flow.Description != "" {
			fmt.Fprintf(env.Stdout, "  %s", flow.Description)
		}
		fmt.Fprintln(env.Stdout)
	}
	if len(res.FlowCounts) > 0 {
		fmt.Fprint(env.Stdout, "  flow counts:")
		for _, key := range sortedCountKeys(res.FlowCounts) {
			fmt.Fprintf(env.Stdout, " %s %d", key, res.FlowCounts[key])
		}
		fmt.Fprintln(env.Stdout)
	}
	if len(res.ClassifiedCounts) > 0 {
		fmt.Fprint(env.Stdout, "  classified lines: ")
		for i, key := range sortedCountKeys(res.ClassifiedCounts) {
			if i > 0 {
				fmt.Fprint(env.Stdout, ", ")
			}
			fmt.Fprintf(env.Stdout, "%s %d", key, res.ClassifiedCounts[key])
		}
		fmt.Fprintf(env.Stdout, "   uncategorized: %d\n", res.UncategorizedCount)
	} else {
		fmt.Fprintf(env.Stdout, "  classified lines: none   uncategorized: %d\n", res.UncategorizedCount)
	}
	fmt.Fprintf(env.Stdout, "  equity days: %d\n", res.EquityDays)

	if res.Replay == nil {
		if res.Message != "" {
			fmt.Fprintf(env.Stdout, "\n  Equity replay unavailable: %s\n", res.Message)
		}
		return 0
	}
	replay := res.Replay
	fmt.Fprintln(env.Stdout, "\n  Equity replay (statement EOD vs runtime intraday):")
	fmt.Fprintf(env.Stdout, "    days %d  %s → %s\n", replay.Days, formatDateOrUnavailable(replay.FirstDay), formatDateOrUnavailable(replay.LastDay))
	fmt.Fprintf(env.Stdout, "    replayed peak %.2f on %s", replay.ReplayedPeakBase, formatDateOrUnavailable(replay.ReplayedPeakAt))
	if replay.RuntimePeakBase != nil {
		fmt.Fprintf(env.Stdout, "   runtime %.2f on %s", *replay.RuntimePeakBase, formatDateOrUnavailable(replay.RuntimePeakAt))
	} else {
		fmt.Fprint(env.Stdout, "   runtime unavailable")
	}
	if replay.PeakDivergencePct != nil {
		fmt.Fprintf(env.Stdout, "  (%+.2f%%)", *replay.PeakDivergencePct)
	}
	fmt.Fprintln(env.Stdout)
	renderBacktestCrossing(env, "warn", replay.Crossings)
	renderBacktestCrossing(env, "block", replay.Crossings)
	fmt.Fprintf(env.Stdout, "    same-day checks %d", replay.SameDayComparisons)
	if replay.MaxSameDayDivergencePct != nil {
		fmt.Fprintf(env.Stdout, "  max divergence %+.2f%%", *replay.MaxSameDayDivergencePct)
	} else {
		fmt.Fprint(env.Stdout, "  max divergence unavailable")
	}
	fmt.Fprintln(env.Stdout)
	for _, note := range replay.Notes {
		fmt.Fprintf(env.Stdout, "    note: %s\n", note)
	}
	return 0
}

func renderBacktestCrossing(env *Env, tier string, crossings []rpc.ReconBacktestCrossing) {
	for _, crossing := range crossings {
		if crossing.Tier != tier {
			continue
		}
		runtimeAt := "not recorded"
		if !crossing.RuntimeAt.IsZero() {
			runtimeAt = crossing.RuntimeAt.Local().Format("2006-01-02 15:04 MST")
		}
		fmt.Fprintf(env.Stdout, "    %-5s crossed %s (replayed %.2f%%)   runtime %s\n",
			tier, formatDateOrUnavailable(crossing.ReplayedAt), crossing.ReplayedConsumedPct, runtimeAt)
		return
	}
	fmt.Fprintf(env.Stdout, "    %-5s not crossed   runtime not recorded\n", tier)
}

func sortedCountKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func formatDateOrUnavailable(v time.Time) string {
	if v.IsZero() {
		return "unavailable"
	}
	return v.Format("2006-01-02")
}

func runReconDismiss(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "recon dismiss")
	line := fs.String("line", "", "exception line id from `canary recon show`")
	reason := fs.String("reason", "", "why this line is deliberately not a ledger event (journaled verbatim)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	params := rpc.ReconDismissParams{LineID: *line, Reason: *reason, Origin: env.Origin}
	var res rpc.RiskPolicyWriteResult
	if err := env.Conn.Call(ctx, rpc.MethodReconDismiss, params, &res); err != nil {
		return fail(env, "recon dismiss: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	fmt.Fprintln(env.Stdout, res.Message)
	return 0
}
