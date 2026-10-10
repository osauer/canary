package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runProfile(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "profile")
	kind := fs.String("kind", "cpu", "cpu or allocs")
	duration := fs.Duration("duration", time.Minute, "capture duration, whole seconds from 1s to 2m")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || (*kind != "cpu" && *kind != "allocs") || *duration < time.Second || *duration > rpc.ProfileMaxDuration || *duration%time.Second != 0 {
		return fail(env, "profile: choose cpu or allocs and a whole-second duration from 1s to 2m")
	}
	var result rpc.ProfileResult
	if err := env.Conn.Call(ctx, rpc.MethodProfileCapture, rpc.ProfileParams{Kind: *kind, Seconds: int(*duration / time.Second)}, &result); err != nil {
		return fail(env, "profile: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	fmt.Fprintf(env.Stdout, "Captured %s profile from daemon PID %d\nDirectory: %s\n", result.Kind, result.PID, result.Directory)
	for _, name := range result.Files {
		fmt.Fprintln(env.Stdout, name)
	}
	return 0
}
