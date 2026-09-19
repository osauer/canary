package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runReporting(ctx context.Context, env *Env, args []string) int {
	if len(args) > 0 && helpArg(args[0]) {
		printReportingUsage(env)
		return 0
	}
	sub := "status"
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	if sub == "performance" {
		return runReportingPerformance(ctx, env, args)
	}
	if sub != "status" {
		return fail(env, "reporting: unknown subcommand %q (try `canary reporting status` or `canary reporting performance`)", sub)
	}
	fs := flagSet(env, "reporting status")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "reporting status: usage is `canary reporting status [--json]`")
	}
	var result rpc.ReportingStatusResult
	if err := env.Conn.Call(ctx, rpc.MethodReportingStatus, struct{}{}, &result); err != nil {
		return fail(env, "reporting status: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderReportingStatus(env, &result)
	return 0
}

func printReportingUsage(env *Env) {
	fmt.Fprintln(env.Stdout, "canary reporting — shared IBKR statement reporting for Recon and Edge")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "Usage: canary reporting status [--json]")
	fmt.Fprintln(env.Stdout, "       canary reporting performance [--json]")
	fmt.Fprintln(env.Stdout, "       canary setup reporting")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "status separates local credentials, broker reachability, report freshness,")
	fmt.Fprintln(env.Stdout, "observed schema, absent sections, present-empty sections, and proven missing fields.")
	fmt.Fprintln(env.Stdout, "performance prints the retained statement equity series, its external flows and")
	fmt.Fprintln(env.Stdout, "year-to-date income sums; it computes no return and names no account.")
}

func runReportingPerformance(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "reporting performance")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "reporting performance: usage is `canary reporting performance [--json]`")
	}
	var result rpc.ReportingPerformanceResult
	if err := env.Conn.Call(ctx, rpc.MethodReportingPerformance, struct{}{}, &result); err != nil {
		return fail(env, "reporting performance: %v", err)
	}
	if err := rpc.ValidateReportingPerformanceResult(result); err != nil {
		return fail(env, "reporting performance: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderReportingPerformance(env, &result)
	return 0
}

func renderReportingPerformance(env *Env, result *rpc.ReportingPerformanceResult) {
	fmt.Fprintf(env.Stdout, "Statement performance series — %s", result.Evidence.State)
	if result.Evidence.Reason != "" {
		fmt.Fprintf(env.Stdout, " (%s)", result.Evidence.Reason)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "  coverage: %s .. %s  equity_days=%d  flows=%d  unclassified_lines=%d  base=%s\n",
		emptyDash(result.Evidence.CoverageFrom), emptyDash(result.Evidence.CoverageTo), result.Evidence.EquityDays, len(result.Flows), result.Evidence.UnclassifiedLines, emptyDash(result.BaseCurrency))
	if ytd := result.YearToDate; ytd != nil {
		fmt.Fprintf(env.Stdout, "  year to date %s .. %s: realised=%.2f closes=%d commissions=%.2f dividends=%.2f interest=%.2f withholding=%.2f fees=%.2f other=%.2f unconverted_trades=%d\n",
			ytd.From, ytd.Through, ytd.RealisedBase, ytd.Closes, ytd.CommissionsBase, ytd.DividendsBase, ytd.InterestBase, ytd.WithholdingTaxBase, ytd.FeesBase, ytd.OtherBase, ytd.UnconvertedTrades)
	}
	fmt.Fprintln(env.Stdout, "  The series is statement closes; returns, drawdowns and comparisons are the consumer's to compute.")
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func renderReportingStatus(env *Env, result *rpc.ReportingStatusResult) {
	fmt.Fprintf(env.Stdout, "Broker reporting — %s", result.State)
	if result.Reason != "" {
		fmt.Fprintf(env.Stdout, " (%s)", result.Reason)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "  local: enabled=%t  query_configured=%t  token_present=%t  token_private=%t\n",
		result.Local.Enabled, result.Local.QueryConfigured, result.Local.TokenFilePresent, result.Local.TokenFilePrivate)
	fmt.Fprintf(env.Stdout, "  broker: %s", result.Broker.State)
	if result.Broker.Reason != "" {
		fmt.Fprintf(env.Stdout, " / %s", result.Broker.Reason)
	}
	fmt.Fprintf(env.Stdout, "  reachability=%s", result.Broker.Reachability)
	if result.Broker.BrokerCode != "" {
		fmt.Fprintf(env.Stdout, "  code=%s", result.Broker.BrokerCode)
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "  evidence: %s", result.Evidence.State)
	if result.Evidence.SchemaFingerprint != "" {
		fmt.Fprintf(env.Stdout, "  schema=%s", result.Evidence.SchemaFingerprint)
	}
	if !result.Evidence.CoverageTo.IsZero() {
		fmt.Fprintf(env.Stdout, "  coverage_to=%s", result.Evidence.CoverageTo.Format("2006-01-02"))
	}
	fmt.Fprintln(env.Stdout)

	for _, section := range result.Requirements {
		detail := ""
		if section.LevelOfDetail != "" {
			detail = "; detail=" + section.LevelOfDetail
		}
		switch section.Status {
		case "missing":
			if len(section.MissingFields) == 0 {
				fmt.Fprintf(env.Stdout, "  missing: %s\n", section.Key)
			} else {
				fmt.Fprintf(env.Stdout, "  missing: %s.%s\n", section.Key, strings.Join(section.MissingFields, ", "+section.Key+"."))
			}
		case "absent":
			fmt.Fprintf(env.Stdout, "  absent: %s (%s%s; section was not returned)\n", section.Key, section.Label, detail)
		case "empty":
			fmt.Fprintf(env.Stdout, "  empty: %s (%s%s; section returned with no rows)\n", section.Key, section.Label, detail)
		}
	}
	if result.Action != "" {
		fmt.Fprintf(env.Stdout, "  action: %s\n", result.Action)
	}
	fmt.Fprintln(env.Stdout, "  guide: https://osauer.dev/canary/docs/start/reporting.html")
}
