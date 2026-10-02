package cli

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func runReportingFX(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "reporting fx")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	backfill := fs.Bool("backfill", false, "resume daily read-only broker statement acquisition")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "usage: canary reporting fx [--backfill] [--json]")
	}
	method := rpc.MethodFX
	if *backfill {
		method = rpc.MethodFXBackfill
	}
	var result rpc.FXResult
	if err := env.Conn.Call(ctx, method, struct{}{}, &result); err != nil {
		return fail(env, "reporting fx: %v", err)
	}
	if err := rpc.ValidateFXResult(result); err != nil {
		return fail(env, "reporting fx: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	fmt.Fprintf(env.Stdout, "FX contribution — %s, through %s, base %s\n", result.State, emptyDash(result.Through), emptyDash(result.BaseCurrency))
	for _, p := range result.Periods {
		if p.Contribution == nil {
			fmt.Fprintf(env.Stdout, "  %-5s unavailable (%d/%d reporting days)\n", p.Key, p.ObservedDays, p.ExpectedDays)
		} else {
			fmt.Fprintf(env.Stdout, "  %-5s %+.2f %s (%s)\n", p.Key, *p.Contribution, result.BaseCurrency, p.State)
		}
	}
	fmt.Fprintf(env.Stdout, "  snapshots %d/%d; background acquisition=%t\n", result.Backfill.SnapshotDays, result.Backfill.ExpectedSnapshots, result.Backfill.Running)
	if result.Reason != "" {
		fmt.Fprintf(env.Stdout, "  evidence: %s\n", result.Reason)
	}
	if result.Backfill.Reason != "" {
		fmt.Fprintf(env.Stdout, "  backfill: %s\n", result.Backfill.Reason)
	}
	fmt.Fprintln(env.Stdout, "  Completed daily valuation attribution of investments, cash and accrued interest; closing native-value convention.")
	return 0
}
