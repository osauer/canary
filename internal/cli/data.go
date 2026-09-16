package cli

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func runData(ctx context.Context, env *Env, args []string) int {
	command := "health"
	fs := flagSet(env, "data")
	jsonOut := fs.Bool("json", false, "emit the authoritative RPC response")
	offset := fs.Int("offset", 0, "source page offset")
	limit := fs.Int("limit", 24, "maximum sources on a page")
	revision := fs.String("revision", "", "report revision for following pages")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 1 || fs.NArg() == 1 && fs.Arg(0) != "health" && fs.Arg(0) != "check" {
		return fail(env, "data: use health or check")
	}
	if fs.NArg() == 1 {
		command = fs.Arg(0)
	}
	if command == "check" {
		var r rpc.DataCheckResult
		if err := env.Conn.Call(ctx, rpc.MethodDataCheck, nil, &r); err != nil {
			return fail(env, "data check: %v", err)
		}
		if *jsonOut {
			return printJSON(env, r)
		}
		fmt.Fprintf(env.Stdout, "%s\nChecked %d · reused %d · limited %d · failed %d · deferred %d\n", r.Detail, r.Checked, r.Reused, r.Limited, r.Failed, r.Deferred)
		return 0
	}
	var r rpc.DataHealthResult
	if err := env.Conn.Call(ctx, rpc.MethodDataHealth, rpc.DataHealthParams{Offset: *offset, Limit: *limit, Revision: *revision}, &r); err != nil {
		return fail(env, "data health: %v", err)
	}
	if *jsonOut {
		return printJSON(env, r)
	}
	fmt.Fprintf(env.Stdout, "%s\nRequired %d · current %d · limited %d · unavailable %d · unverified %d\n", r.Summary.Label, r.Summary.Required, r.Summary.Current, r.Summary.Limited, r.Summary.Unavailable, r.Summary.Unverified)
	fmt.Fprintf(env.Stdout, "Report %s · valid until %s\n", r.AsOf.Format("2006-01-02 15:04:05 MST"), r.ValidUntil.Format("15:04:05 MST"))
	for _, row := range r.Sources {
		fmt.Fprintf(env.Stdout, "%s: %s [%s]\n", row.Name, row.Receiving, row.State)
		if row.Access != nil {
			fmt.Fprintf(env.Stdout, "  Live access: %s (IBKR %d); retry %s\n", row.Access.Reason, row.Access.Code, row.Access.RetryAt.Format("15:04"))
		}
		if row.Detail != "" {
			fmt.Fprintf(env.Stdout, "  %s\n", row.Detail)
		}
	}
	if r.NextOffset != nil {
		fmt.Fprintf(env.Stdout, "More sources: canary data health --offset %d --revision %s\n", *r.NextOffset, r.Revision)
	}
	return 0
}
