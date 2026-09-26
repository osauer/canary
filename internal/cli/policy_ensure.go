package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon"
)

// RunPolicyLocal runs the policy subcommands that need no daemon: default
// (print a template) and ensure (write missing files, migrate existing ones).
// The install path runs `canary policy ensure` before any daemon exists.
func RunPolicyLocal(ctx context.Context, env *Env, args []string) int {
	sub := ""
	if idx := firstPositionalIndex(args); idx >= 0 {
		sub = args[idx]
		args = append(append([]string{}, args[:idx]...), args[idx+1:]...)
	}
	switch sub {
	case "default":
		return runPolicyDefault(ctx, env, args)
	case "ensure":
		return runPolicyEnsure(ctx, env, args)
	default:
		return fail(env, "policy: %q needs the daemon", sub)
	}
}

// PolicyLocalSubcommand reports whether args name a policy subcommand that
// runs without the daemon.
func PolicyLocalSubcommand(args []string) bool {
	idx := firstPositionalIndex(args)
	return idx >= 0 && (args[idx] == "default" || args[idx] == "ensure")
}

// runPolicyEnsure writes every missing policy file from Canary's template and
// migrates every existing one in place, exactly as the daemon does at start.
// It never overwrites a file or changes a value you set: a migration backs the
// file up first, adds new keys at their defaults and comments out retired
// ones; recommendations are reported, never applied.
func runPolicyEnsure(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy ensure")
	dryRun := fs.Bool("dry-run", false, "report what would be written or migrated without writing anything")
	jsonOut := fs.Bool("json", false, "emit the actions as JSON")
	configPath := fs.String("config", "", "config file that names the policy paths (default: the daemon's config)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "policy ensure: usage is `canary policy ensure [--dry-run] [--json]`")
	}
	set, cfgErr := daemon.PolicyFileSetFromConfigFile(*configPath)
	actions := daemon.EnsurePolicyFiles(set, daemon.EnsureOptions{Release: env.Version, DryRun: *dryRun})
	if *jsonOut {
		return printJSON(env, struct {
			ConfigError string                    `json:"config_error,omitempty"`
			Actions     []daemon.PolicyFileAction `json:"actions"`
		}{errString(cfgErr), actions})
	}
	if cfgErr != nil {
		fmt.Fprintf(env.Stderr, "policy ensure: config unreadable (%v); using the default policy paths\n", cfgErr)
	}
	failed := false
	for _, a := range actions {
		fmt.Fprintf(env.Stdout, "%-12s %-13s %s\n", a.Policy, strings.ReplaceAll(a.Action, "_", " "), a.Path)
		if a.Backup != "" {
			fmt.Fprintf(env.Stdout, "    backup   %s\n", a.Backup)
		}
		for _, c := range a.Changes {
			fmt.Fprintf(env.Stdout, "    change   %s\n", c)
		}
		for _, n := range a.Notes {
			fmt.Fprintf(env.Stdout, "    note     %s\n", n)
		}
		if a.Error != "" {
			fmt.Fprintf(env.Stdout, "    error    %s\n", a.Error)
		}
		if a.Action == daemon.PolicyFileFailed {
			failed = true
		}
	}
	if failed {
		return 1
	}
	return 0
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
