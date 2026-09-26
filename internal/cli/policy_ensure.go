package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon"
)

// RunPolicyLocal runs the policy subcommands that need no daemon: default
// (print a template) and ensure (write missing files, review/apply conversions).
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
// previews existing files; applying a reviewed plan is separate from startup.
// It never replaces owner settings: an explicitly applied migration backs the
// file up first, adds new keys at their defaults and comments out retired
// ones; recommendations are reported, never applied.
func runPolicyEnsure(_ context.Context, env *Env, args []string) int {
	fs := flagSet(env, "policy ensure")
	applyPlan := fs.String("apply-plan", "", "apply an exact reviewed JSON dry-run plan, with backups")
	dryRun := fs.Bool("dry-run", false, "report what would be written or migrated without writing anything")
	jsonOut := fs.Bool("json", false, "emit the actions as JSON")
	configPath := fs.String("config", "", "config file that names the policy paths (default: the daemon's config)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "policy ensure: usage is `canary policy ensure [--dry-run | --apply-plan FILE] [--json]`")
	}
	if *dryRun && *applyPlan != "" {
		return fail(env, "policy ensure: choose --dry-run or --apply-plan")
	}
	approved := map[string]daemon.PolicyMigrationApproval{}
	if *applyPlan != "" {
		file, err := os.Open(*applyPlan)
		if err != nil {
			return fail(env, "policy ensure: read reviewed plan: %v", err)
		}
		data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		file.Close()
		if err != nil {
			return fail(env, "policy ensure: read reviewed plan: %v", err)
		}
		if len(data) > 1<<20 {
			return fail(env, "policy ensure: reviewed plan exceeds 1 MiB")
		}
		var plan struct {
			Actions []daemon.PolicyFileAction `json:"actions"`
		}
		if err := json.Unmarshal(data, &plan); err != nil {
			return fail(env, "policy ensure: invalid plan: %v", err)
		}
		for _, a := range plan.Actions {
			if a.Action != daemon.PolicyFileWouldMigrate {
				continue
			}
			if _, duplicate := approved[a.Policy]; duplicate {
				return fail(env, "policy ensure: duplicate policy in plan")
			}
			if a.BeforeSHA256 == "" || a.AfterSHA256 == "" {
				return fail(env, "policy ensure: plan lacks exact file hashes")
			}
			approved[a.Policy] = daemon.PolicyMigrationApproval{Path: a.Path, BeforeSHA256: a.BeforeSHA256, AfterSHA256: a.AfterSHA256}
		}
		if len(approved) == 0 {
			return fail(env, "policy ensure: plan contains no reviewed conversion")
		}
	}
	set, cfgErr := daemon.PolicyFileSetFromConfigFile(*configPath)
	if cfgErr != nil && *applyPlan != "" {
		return fail(env, "policy ensure: cannot apply a plan with unreadable configuration: %v", cfgErr)
	}
	actions := daemon.EnsurePolicyFiles(set, daemon.EnsureOptions{Release: env.Version, DryRun: *dryRun, Approved: approved})
	failed := false
	for _, a := range actions {
		if a.Action == daemon.PolicyFileFailed {
			failed = true
		}
	}
	if *jsonOut {
		code := printJSON(env, struct {
			ConfigError string                    `json:"config_error,omitempty"`
			Actions     []daemon.PolicyFileAction `json:"actions"`
		}{errString(cfgErr), actions})
		if code == 0 && failed {
			return 1
		}
		return code
	}
	if cfgErr != nil {
		fmt.Fprintf(env.Stderr, "policy ensure: config unreadable (%v); using the default policy paths\n", cfgErr)
	}
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
		if a.Action == daemon.PolicyFileWouldMigrate && a.Diff != "" {
			fmt.Fprint(env.Stdout, a.Diff)
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
