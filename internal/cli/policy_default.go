package cli

import (
	"context"

	"github.com/osauer/canary/v2/internal/daemon"
)

// runPolicyDefault prints Canary's default file for a policy: the same
// commented template Canary writes when the file is missing, so it cannot
// drift from the code. Purely local and read-only; it needs no daemon.
func runPolicyDefault(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy default")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy default: exactly one of rulebook|protection|opportunity|constitution is required")
	}
	daemon.SetRulebookEditRelease(env.Version)
	raw, err := daemon.DefaultPolicyTOML(fs.Arg(0))
	if err != nil {
		return fail(env, "policy default: %v", err)
	}
	env.Stdout.Write(raw)
	return 0
}
