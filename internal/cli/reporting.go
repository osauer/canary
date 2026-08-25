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
	if sub != "status" {
		return fail(env, "reporting: unknown subcommand %q (try `canary reporting status`)", sub)
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
	fmt.Fprintln(env.Stdout, "       canary setup reporting")
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, "status separates local credentials, broker reachability, report freshness,")
	fmt.Fprintln(env.Stdout, "observed schema, proven missing requirements, and unproved empty sections.")
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
		switch section.Status {
		case "missing":
			if len(section.MissingFields) == 0 {
				fmt.Fprintf(env.Stdout, "  missing: %s\n", section.Key)
			} else {
				fmt.Fprintf(env.Stdout, "  missing: %s.%s\n", section.Key, strings.Join(section.MissingFields, ", "+section.Key+"."))
			}
		case "unproved":
			fmt.Fprintf(env.Stdout, "  unproved: %s (section was empty)\n", section.Key)
		}
	}
	if result.Action != "" {
		fmt.Fprintf(env.Stdout, "  action: %s\n", result.Action)
	}
	fmt.Fprintln(env.Stdout, "  guide: https://osauer.dev/canary/docs/start/reporting.html")
}
