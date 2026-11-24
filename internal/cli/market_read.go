package cli

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
)

// runMarketRead prints the session read as JSON; it has no text view.
func runMarketRead(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "market read", "Today's US session against yesterday's, with tags that hold", "canary market read [--json]")
	fs.Bool("json", true, "the read as JSON (the only output)")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		fmt.Fprint(env.Stdout, "\nExamples:\n  canary market read --json\n\nRead-only and descriptive; a missing value is absent with a reason.\n")
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	var result rpc.MarketReadResult
	if err := rpc.Call(ctx, env.Conn, api.MarketRead, rpc.NoParams{}, &result); err != nil {
		return fail(env, "market read: %v", err)
	}
	return printJSON(env, result)
}
