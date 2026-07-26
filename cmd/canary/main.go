// Command canary provides terminal, MCP, app-host, and daemon entry points for
// the local trading harness. Broker-connected commands adapt typed requests to
// the long-running daemon, while setup, update, watchlist, and offline research
// workflows may run locally. The daemon subcommand owns broker connectivity and
// runtime state; the MCP subcommand exposes no broker-write tools.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
	"github.com/osauer/canary/v2/internal/tui"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"

	// String vars so integration tests can shrink CLI wall-clock budgets with
	// `go build -ldflags -X ...` while production builds keep the defaults.
	cliUnaryTimeout     = "60s"
	cliLongUnaryTimeout = "90s"
)

func main() {
	if err := retiredProductEnvError(os.LookupEnv); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", productidentity.Executable, err)
		os.Exit(2)
	}
	runtimeVersion := effectiveVersion()
	args := os.Args[1:]
	if len(args) == 0 {
		if tui.IsInteractive(os.Stdin, os.Stdout) {
			ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			os.Exit(tui.Run(ctx, tui.Options{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, Version: runtimeVersion}))
		}
		cli.PrintUsage(os.Stdout)
		return
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		cli.PrintUsage(os.Stdout)
		return
	}

	cmd := args[0]
	rest := args[1:]

	if cmd == "version" || cmd == "--version" {
		printVersion(os.Stdout, productidentity.Executable, hasJSONFlag(rest))
		return
	}

	// `canary daemon` is the long-lived background mode. Special-cased
	// before the autospawn path so that running it manually does the
	// right thing (start the daemon) instead of trying to dial its own
	// socket. The autospawn path also calls back into this entrypoint
	// with the same arg, via os.Executable() — single-binary discovery.
	if cmd == "daemon" {
		runDaemon(rest)
		return
	}

	// `canary mcp` runs the stdio MCP server, spoken by Claude Desktop and
	// other local MCP clients. Like `daemon`, special-cased before
	// autospawn — the MCP server itself dials (and autospawns if needed)
	// the daemon socket internally so it stays responsive across tool
	// calls.
	if cmd == "mcp" {
		os.Exit(runMCP(rest))
	}

	// `canary app` runs the mobile/PWA application layer. It owns its own
	// HyperServe HTTP lifecycle and dials the daemon internally, so it must
	// not go through the one-shot CLI autospawn path.
	if cmd == "app" {
		os.Exit(runApp(rest))
	}

	// `canary setup [client]` writes the MCP server entry into local AI
	// client config files (e.g. claude_desktop_config.json). Purely local
	// — no daemon involvement, special-cased here so we skip the dial.
	if cmd == "setup" {
		os.Exit(runSetup(rest))
	}

	// `canary update` self-updates the binary from GitHub releases.
	// Purely local — no daemon dial (the daemon may itself be the
	// binary we are about to replace; dialing into it before the
	// install would either spawn an idle one or skew the version
	// check). The CLI may SIGTERM the daemon at the end of the
	// install if --restart was requested.
	if cmd == "update" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		os.Exit(cli.RunUpdate(ctx, rest, runtimeVersion, os.Stdin, os.Stdout, os.Stderr))
	}

	// `canary restart` is local process management for the background daemon.
	// It must run before the normal autospawn path; otherwise a missing
	// daemon would be spawned first and then immediately restarted.
	if cmd == "restart" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		os.Exit(cli.RunRestart(ctx, rest, os.Stdout, os.Stderr))
	}

	color := cli.ShouldColor(os.Stdout)

	// `canary watch` defaults to the quote monitor, but add/remove/list/clear
	// stay local metadata so they remain usable without a gateway.
	if cmd == "watch" && !isWatchDaemonInvocation(rest) {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
		os.Exit(cli.Run(ctx, env, cmd, rest))
	}

	// Offline research harnesses consume local JSONL fixtures and should not
	// autospawn or depend on a live gateway. The opportunity capture/export
	// subcommands intentionally sample live scanner, technical, or history
	// snapshots, so they go through the normal daemon path.
	if cmd == "backtest" && !isBacktestDaemonInvocation(rest) {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
		os.Exit(cli.Run(ctx, env, cmd, rest))
	}

	// `canary <cmd> --help` should not spawn the daemon — render help and exit.
	for _, a := range rest {
		if a == "--help" || a == "-h" || a == "-help" {
			env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
			os.Exit(cli.Run(context.Background(), env, cmd, rest))
		}
	}

	// Reject unknown subcommands before autospawn — sparing a dormant
	// install a 100ms+ daemon startup just to fail with the same
	// "unknown subcommand" message cli.Run would produce.
	if !cli.IsKnown(cmd) {
		env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Color: color}
		os.Exit(cli.Run(context.Background(), env, cmd, rest))
	}

	socketPath := dial.DefaultSocketPath()
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnect(socketPath)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", productidentity.Executable, err)
		os.Exit(1)
	}
	defer conn.Close()

	// `status` already prints daemon_version in its body, so the extra
	// pre-flight check would just be noise there. Every other command
	// gets a version-skew check — a fast status.health round-trip whose
	// only output is a stderr warning if the daemon was built from a
	// different revision than this CLI binary.
	if cmd != "status" {
		warnIfDaemonVersionMismatch(conn, runtimeVersion)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Apply a default per-invocation deadline so a non-responsive daemon
	// (deadlocked, SIGSTOP'd, kernel-stuck) cannot hang the CLI forever.
	// Streaming commands (`quote --watch`, `account --watch`, `positions
	// --watch`) bypass — a long-lived watch must outlive any unary budget.
	//
	// `scan` gets a larger budget because the daemon-side MethodScanRun
	// deadline is 75 s (35 s gateway scanner-subscription warmup off-hours
	// + per-row enrichment); the CLI ceiling must exceed it so the
	// classified daemon error reaches the user instead of a raw socket
	// timeout from cancelling the in-flight request.
	if !isStreamingInvocation(cmd, rest) {
		budget := unaryInvocationBudget(cmd, rest)
		var dlCancel context.CancelFunc
		ctx, dlCancel = context.WithTimeout(ctx, budget)
		defer dlCancel()
	}

	env := &cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Conn: conn, Version: runtimeVersion, Color: color, Origin: cli.DetectWriteOrigin(os.Stdin)}
	os.Exit(cli.Run(ctx, env, cmd, rest))
}

var retiredProductEnv = []struct {
	retired   string
	canonical string
}{
	{"IBKR_CONFIG", "CANARY_CONFIG"},
	{"IBKR_INSTALL_DIR", "CANARY_INSTALL_DIR"},
	{"IBKR_SOCKET", "CANARY_SOCKET"},
	{"IBKR_LOG", "CANARY_LOG"},
	{"IBKR_COLOR", "CANARY_COLOR"},
	{"IBKR_APP_ADDR", "CANARY_APP_ADDR"},
	{"IBKR_APP_PUBLIC_URL", "CANARY_APP_PUBLIC_URL"},
	{"IBKR_APP_REMOTE", "CANARY_APP_REMOTE"},
	{"IBKR_APP_REMOTE_URL", "CANARY_APP_REMOTE_URL"},
	{"IBKR_APP_STATE_DIR", "CANARY_APP_STATE_DIR"},
	{"IBKR_SPX_MEMBERS_AUTO_REFRESH", "CANARY_SPX_MEMBERS_AUTO_REFRESH"},
}

func retiredProductEnvError(lookup func(string) (string, bool)) error {
	for _, env := range retiredProductEnv {
		if _, exists := lookup(env.retired); exists {
			return fmt.Errorf("retired environment variable %s is set; use %s", env.retired, env.canonical)
		}
	}
	return nil
}

func unaryInvocationBudget(cmd string, rest []string) time.Duration {
	if cmd == "scan" || cmd == "technical" || cmd == "stress" || cmd == "brief" || (cmd == "backtest" && isBacktestDaemonInvocation(rest)) {
		return parseDurationOr(cliLongUnaryTimeout, 90*time.Second)
	}
	// The daemon-side MethodTradingPaperSmoke deadline is 100 s (preview,
	// place, ack wait, detached cancel budget); the CLI ceiling must exceed
	// it so the classified daemon error reaches the user.
	if cmd == "trading" && len(rest) > 0 && rest[0] == "paper-smoke" {
		return 120 * time.Second
	}
	// `proposals reduce --portfolio` previews/places each eligible leg
	// sequentially; the daemon-side bucket is 120 s, so the CLI ceiling must
	// exceed it for a classified basket error to reach the user.
	if cmd == "proposals" {
		reduce, portfolio := false, false
		for _, a := range rest {
			switch a {
			case "reduce":
				reduce = true
			case "--portfolio", "-portfolio":
				portfolio = true
			}
		}
		if reduce && portfolio {
			return 150 * time.Second
		}
	}
	return parseDurationOr(cliUnaryTimeout, 60*time.Second)
}

func isBacktestDaemonInvocation(rest []string) bool {
	for _, arg := range rest {
		if arg == "--help" || arg == "-h" || arg == "-help" || arg == "help" {
			return false
		}
	}
	for _, arg := range rest {
		if arg == "capture-opportunity" || arg == "export-opportunity-bars" {
			return true
		}
	}
	return false
}

func parseDurationOr(raw string, fallback time.Duration) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// warnIfDaemonVersionMismatch fires a tight-timeout status.health call
// after Connect and prints a stderr warning if the daemon was built from
// a different revision than this CLI binary. Best-effort: any RPC error
// (timeout, daemon mid-restart, transport hiccup) is swallowed because a
// failure here must not interfere with the user's actual command.
//
// Quiet cases (no warning):
//   - exact version match
//   - either side stamps the "dev" placeholder — a dev build can't sensibly
//     compare against a tagged release, and "warn against yourself every
//     run" is the wrong default for a working tree
func warnIfDaemonVersionMismatch(conn *dial.Conn, cliVersion string) {
	if cliVersion == "" || cliVersion == "dev" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	daemonVersion, err := conn.DaemonVersion(ctx)
	if err != nil || daemonVersion == "" || daemonVersion == "dev" {
		return
	}
	if daemonVersion == cliVersion {
		return
	}
	fmt.Fprintf(os.Stderr,
		"%s: warning: CLI version %s does not match daemon version %s — run `%s restart` to pick up the new binary.\n",
		productidentity.Executable, cliVersion, daemonVersion, productidentity.Executable)
}

// isStreamingInvocation reports whether the CLI invocation will hold the
// daemon socket open for an open-ended stream. `quote`, `account`, `positions`,
// and `watch` all support `--watch` as a long-running render loop.
func isStreamingInvocation(cmd string, args []string) bool {
	switch cmd {
	case "quote", "account", "positions", "watch":
	default:
		return false
	}
	for _, a := range args {
		if a == "--watch" || a == "-watch" || a == "--watch=true" {
			return true
		}
	}
	return false
}

func isWatchDaemonInvocation(args []string) bool {
	localOnly := false
	for _, a := range args {
		name := strings.TrimLeft(a, "-")
		if i := strings.Index(name, "="); i >= 0 {
			name = name[:i]
		}
		switch a {
		case "--watch", "-watch", "--watch=true", "--quotes", "-quotes", "--quotes=true":
			return true
		}
		switch name {
		case "add", "remove", "clear", "list":
			localOnly = true
		}
	}
	return !localOnly
}
