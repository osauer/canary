package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/update"
)

// updateOptions is the parsed flag state for `canary update`. Carries
type updateOptions struct {
	check     bool
	force     bool
	restart   bool
	noRestart bool

	// installedVersion is the running binary's version string (e.g.
	installedVersion string

	// in/out/err are the I/O streams. Stdin is read for the
	in  io.Reader
	out io.Writer
	err io.Writer

	// isTTY is the result of TTY detection for in. Tests inject
	isTTY bool
}

// RunUpdate is the entrypoint cmd/canary/main.go dispatches to. It does
func RunUpdate(ctx context.Context, args []string, version string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts := updateOptions{
		installedVersion: version,
		in:               stdin,
		out:              stdout,
		err:              stderr,
		isTTY:            isStdinTTY(stdin),
	}
	if exit, ok := parseUpdateFlags(args, &opts, stdout, stderr); !ok {
		return exit
	}
	return runUpdateCore(ctx, &opts, fetchLatestReleaseAdapter, runInstallAdapter, restartInstalledStackAdapter)
}

// parseUpdateFlags wires a flag.FlagSet against opts. Returns
// immediately (flag parse failure or --help).
func parseUpdateFlags(args []string, opts *updateOptions, stdout, stderr io.Writer) (int, bool) {
	env := &Env{Stdout: stdout, Stderr: stderr}
	fs := flagSet(env, "update")
	fs.BoolVar(&opts.check, "check", false, "dry-run: report what would change, do not install")
	fs.BoolVar(&opts.force, "force", false, "install latest even if same version as current (corrupt-binary recovery)")
	fs.BoolVar(&opts.restart, "restart", false, "restart the running app first, then the daemon, after install (skip prompt)")
	fs.BoolVar(&opts.noRestart, "no-restart", false, "do not restart the app or daemon after install (skip prompt)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err), false
	}
	if opts.restart && opts.noRestart {
		fmt.Fprintf(stderr, "%s update: --restart and --no-restart are mutually exclusive\n", productidentity.Executable)
		return 2, false
	}
	return 0, true
}

// runUpdateCore is the testable core: takes injectable adapters for
type fetchFunc func(ctx context.Context, installedVersion string) (*update.Release, error)
type installFunc func(ctx context.Context, plan *update.Plan) error
type stackRestartFunc func(ctx context.Context, installedExecutable string, stdout, stderr io.Writer) int

func runUpdateCore(ctx context.Context, opts *updateOptions, fetch fetchFunc, doInstall installFunc, restart stackRestartFunc) int {
	// TTY ambiguity gate: in non-interactive mode without an explicit
	if !opts.check && !opts.isTTY && !opts.restart && !opts.noRestart {
		fmt.Fprintf(opts.err, "%s update: ambiguous in non-interactive mode — pass --restart or --no-restart\n", productidentity.Executable)
		return 2
	}

	installed := normalizeVersion(opts.installedVersion)
	rel, err := fetch(ctx, installed)
	if err != nil {
		fmt.Fprintf(opts.err, "%s update: could not reach GitHub releases API: %v\n", productidentity.Executable, err)
		return 1
	}

	latest := normalizeVersion(rel.TagName)

	// Version-compare decision. --force always installs (bypasses).
	needsInstall := opts.force || versionNewer(latest, installed)

	if opts.check {
		renderCheck(opts.out, installed, latest, needsInstall, opts.force)
		return 0
	}

	if !needsInstall {
		fmt.Fprintf(opts.out, "%s update: already on %s (latest is %s)\n", productidentity.Executable, installed, latest)
		return 0
	}

	// Confirm the platform has an asset before fetching anything.
	if _, _, ok := rel.AssetForHost(); !ok {
		fmt.Fprintf(opts.err, "%s update: no release asset for %s/%s on %s\n", productidentity.Executable, runtime.GOOS, runtime.GOARCH, latest)
		return 1
	}
	plan, err := update.PlanFor(rel)
	if err != nil {
		fmt.Fprintf(opts.err, "%s update: %v\n", productidentity.Executable, err)
		return 1
	}

	fmt.Fprintf(opts.out, "%s update: installing %s -> %s\n", productidentity.Executable, latest, plan.DestPath)
	if err := doInstall(ctx, plan); err != nil {
		if errors.Is(err, update.ErrInstallInProgress) {
			fmt.Fprintf(opts.err, "%s update: another Canary update is already running\n", productidentity.Executable)
			return 1
		}
		fmt.Fprintf(opts.err, "%s update: install failed: %v\n", productidentity.Executable, err)
		return 1
	}
	fmt.Fprintf(opts.out, "%s update: installed %s at %s\n", productidentity.Executable, latest, plan.DestPath)

	// Local stack restart decision. This deliberately reuses the canonical
	// app-first restart path: a running app must be on the newly-installed
	doRestart := opts.restart
	if !opts.restart && !opts.noRestart && opts.isTTY {
		doRestart = promptRestart(opts.in, opts.out)
	}

	if doRestart {
		fmt.Fprintf(opts.out, "%s update: restarting the local app/daemon stack (app first)\n", productidentity.Executable)
		if restart == nil {
			fmt.Fprintf(opts.err, "%s update: restart adapter unavailable\n", productidentity.Executable)
			return 1
		}
		if exit := restart(ctx, plan.DestPath, opts.out, opts.err); exit != 0 {
			fmt.Fprintf(opts.err, "%s update: stack restart failed; app and daemon stages are ordered but non-atomic, and successful earlier stages are not rolled back (fix the reported failure and rerun `%s restart`)\n", productidentity.Executable, productidentity.Executable)
			return exit
		}
		fmt.Fprintf(opts.out, "%s update: requested restart stages completed; previously stopped processes remain stopped\n", productidentity.Executable)
		return 0
	}

	fmt.Fprintf(opts.out, "%s update: app and daemon were not restarted; run `%s restart` to move running processes to the installed binary\n", productidentity.Executable, productidentity.Executable)
	return 0
}

// promptRestart reads stdin for a [Y/n] response. Defaults to Y on
func promptRestart(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Restart the local app/daemon stack now? [Y/n] ")
	br := bufio.NewReader(in)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		// EOF before any input — treat as "Y" (the default).
		return true
	}
	line = strings.TrimSpace(strings.ToLower(line))
	if line == "" || line == "y" || line == "yes" {
		return true
	}
	return false
}

// renderCheck prints the dry-run summary. Exit code per design
// (informational), non-zero only on actual fetch failures. So
func renderCheck(w io.Writer, installed, latest string, needsInstall bool, forced bool) {
	switch {
	case forced:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\n--force was set; `%s update` would re-install %s\n", installed, latest, productidentity.Executable, latest)
	case needsInstall:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\n`%s update` would install %s\n", installed, latest, productidentity.Executable, latest)
	default:
		fmt.Fprintf(w, "installed: %s\nlatest:    %s\nalready on latest; nothing to do\n", installed, latest)
	}
}

// normalizeVersion canonicalises a version string so semver.Compare
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || v == "dev" {
		return v
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}

// versionNewer reports whether latest > installed. A "dev" or empty
func versionNewer(latest, installed string) bool {
	if installed == "" || installed == "dev" {
		// Any tagged release is newer than the dev placeholder.
		return semver.IsValid(latest)
	}
	if !semver.IsValid(latest) || !semver.IsValid(installed) {
		// Can't compare — be conservative and say "not newer" so
		// we don't install on a parse failure. --force overrides
		return false
	}
	return semver.Compare(latest, installed) > 0
}

// isStdinTTY reports whether stdin is a real terminal. Mirrors the
// pass a *bytes.Buffer so the type assertion fails and isTTY=false,
func isStdinTTY(in io.Reader) bool {
	f, ok := in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// --- production adapters (thin shims so runUpdateCore stays pure). ---

func fetchLatestReleaseAdapter(ctx context.Context, installedVersion string) (*update.Release, error) {
	return update.FetchLatestRelease(ctx, installedVersion)
}

func runInstallAdapter(ctx context.Context, plan *update.Plan) error {
	return update.RunInstall(ctx, plan)
}

func restartInstalledStackAdapter(ctx context.Context, installedExecutable string, stdout, stderr io.Writer) int {
	return restartInstalledStackAdapterUsing(ctx, installedExecutable, stdout, stderr, productionDaemonRestartDepsForExecutable, productionAppRestartDepsForExecutable)
}

type daemonRestartDepsFactory func(string) restartDeps
type appRestartDepsFactory func(string) appRestartDeps

func restartInstalledStackAdapterUsing(ctx context.Context, installedExecutable string, stdout, stderr io.Writer, daemonFactory daemonRestartDepsFactory, appFactory appRestartDepsFactory) int {
	opts := &restartOptions{
		timeout: restartDefaultTimeout,
		out:     stdout,
		err:     stderr,
	}
	return runRestartStackCore(ctx, opts, daemonFactory(installedExecutable), appFactory(installedExecutable), restartStackBehavior{startDaemonWhenMissing: false})
}
