package cli

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/daemon"
)

// runPolicyDefault prints the embedded default protection or opportunity
// policy as activation-ready TOML. Purely local and read-only: it needs no
// running daemon, and printing is the supported alternative to shipping
// template files (a printed default cannot drift from the code).
func runPolicyDefault(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy default")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 {
		return fail(env, "policy default: exactly one of protection|opportunity is required")
	}
	name := fs.Arg(0)
	raw, err := daemon.DefaultPolicyTOML(name)
	if err != nil {
		return fail(env, "policy default: %v", err)
	}
	fmt.Fprintf(env.Stdout, "# Embedded default %s policy (what the daemon runs when no file exists).\n", name)
	fmt.Fprintf(env.Stdout, "# To customize: save to ~/.config/ibkr/policies/%s-policy.toml, edit, and\n", name)
	fmt.Fprintln(env.Stdout, "# bump policy_version on every edit — an edited file at an unchanged")
	fmt.Fprintln(env.Stdout, "# version reports drift and is not adopted. Key reference: docs/docs/reference/config.md.")
	env.Stdout.Write(raw)
	return 0
}
