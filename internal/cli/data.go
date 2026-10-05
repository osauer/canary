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
	renderDataHealth(env, r)
	return 0
}

func dataHealthStyle(env *Env, state string, expected bool) func(string) string {
	if expected {
		return env.bold
	}
	switch state {
	case "current", "not_due":
		return env.green
	case "unavailable":
		return env.red
	case "limited", "unverified", "unknown":
		return env.yellow
	default:
		return env.bold
	}
}

func renderDataHealth(env *Env, r rpc.DataHealthResult) {
	expectedOnly := r.ScopeState == "current" && r.Summary.ExpectedDelays > 0 && r.Summary.Limited == r.Summary.ExpectedDelays && r.Summary.Unavailable == 0 && r.Summary.Unverified == 0 && r.Summary.Problems == 0
	displayLine(env, r.Summary.Label, dataHealthStyle(env, r.Summary.State, expectedOnly))
	displayLine(env, fmt.Sprintf("Required %d · current %d · limited %d · unavailable %d · unverified %d", r.Summary.Required, r.Summary.Current, r.Summary.Limited, r.Summary.Unavailable, r.Summary.Unverified), nil)
	if r.Summary.ExpectedDelays > 0 {
		displayLine(env, fmt.Sprintf("%d limited source(s) awaiting the publisher; not counted as source problems.", r.Summary.ExpectedDelays), nil)
	}
	displayLine(env, "Report "+r.AsOf.Format("2006-01-02 15:04:05 MST")+" · valid until "+r.ValidUntil.Format("2006-01-02 15:04:05 MST"), nil)
	if len(r.Concerns) > 0 {
		displayLine(env, "Needs attention", env.bold)
		for _, concern := range r.Concerns {
			displayLine(env, "  "+concern.Label+" ["+concern.State+"]", dataHealthStyle(env, concern.State, false))
		}
	}
	// Preserve the producer's page order, every existing source detail, and access
	// evidence. Concerns above can refer to sources on later pages.
	for _, row := range r.Sources {
		fmt.Fprintln(env.Stdout)
		expected := row.State == "limited" && rpc.DataHealthCauseExpected(row.Cause)
		displayLine(env, row.Name+": "+row.Receiving+" ["+row.State+"]", dataHealthStyle(env, row.State, expected))
		if row.Access != nil {
			retry := "not scheduled"
			if !row.Access.RetryAt.IsZero() {
				retry = row.Access.RetryAt.Format("2006-01-02 15:04 MST")
			}
			displayLine(env, fmt.Sprintf("  Live access: %s (IBKR %d); retry %s", row.Access.Reason, row.Access.Code, retry), nil)
		}
		if row.Detail != "" {
			displayLine(env, "  "+row.Detail, nil)
		}
		if row.Action != "" && row.State != "current" && row.State != "not_due" {
			displayLine(env, "  Next: "+row.Action, nil)
		}
	}
	if r.NextOffset != nil {
		fmt.Fprintln(env.Stdout)
		displayLine(env, fmt.Sprintf("Sources %d–%d of %d; more sources:", r.Offset+1, r.Offset+len(r.Sources), r.Summary.Total), nil)
		command := fmt.Sprintf("canary data health --offset %d --revision %s", *r.NextOffset, r.Revision)
		if visibleLen(command) <= briefProseWidth(env.Stdout) {
			fmt.Fprintln(env.Stdout, command)
		} else {
			// Shell continuations keep the command copyable on narrow terminals.
			fmt.Fprintln(env.Stdout, "canary data health \\")
			fmt.Fprintf(env.Stdout, "  --offset %d \\\n", *r.NextOffset)
			fmt.Fprintln(env.Stdout, "  --revision "+r.Revision)
		}
	} else {
		displayLine(env, fmt.Sprintf("Sources shown: %d of %d.", len(r.Sources), r.Summary.Total), nil)
	}
}
