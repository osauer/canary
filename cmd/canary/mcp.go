package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/mcp"
	"github.com/osauer/canary/v2/internal/productidentity"
)

const (
	mcpParentPollInterval = 2 * time.Second
)

func runMCP(args []string) int {
	profile, code := parseMCPArgs(args, os.Stdout, os.Stderr)
	if code != 0 {
		return code
	}

	ctx, cancel := mcpLifecycleContext()
	defer cancel()

	socketPath := dial.DefaultSocketPath()
	srv := mcp.NewServer(nil, effectiveVersion())
	srv.SetProfile(profile)
	// Tool calls and streaming subscriptions use short-lived daemon
	// connections so a timed-out MCP call cannot leave a late daemon reply
	// queued on a shared control socket. The daemon is opened lazily on the
	// first request, so an MCP process leaked by its host cannot keep the
	// daemon's active-connection count above zero while idle.
	srv.SetContextDialer(func(ctx context.Context) (*dial.Conn, error) {
		return dialMCPDaemon(ctx, socketPath)
	})
	if err := srv.ServeWithOptions(ctx, os.Stdin, os.Stdout, mcpServeOptions()); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "%s mcp: %v\n", productidentity.Executable, err)
		return 1
	}
	return 0
}

func parseMCPArgs(args []string, stdout, stderr io.Writer) (mcp.Profile, int) {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profileRaw := fs.String("profile", string(mcp.ProfileFull), "tool profile: full | monitor")
	fs.Usage = func() { printMCPUsage(stdout) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return mcp.ProfileFull, 0
		}
		return mcp.ProfileFull, 2
	}
	if fs.NArg() > 0 {
		rejectUnexpectedArgument(stderr, productidentity.Executable+" mcp", fs, printMCPUsage)
		return mcp.ProfileFull, 2
	}
	profile, err := mcp.ParseProfile(*profileRaw)
	if err != nil {
		fmt.Fprintf(stderr, "%s mcp: %v\n", productidentity.Executable, err)
		return mcp.ProfileFull, 2
	}
	return profile, 0
}

func mcpServeOptions() mcp.ServeOptions {
	// The MCP host owns the stdio process lifetime. Do not exit merely
	// because a chat sat idle: Claude may send the next tool call hours
	// later on the same process. Daemon cleanup is handled separately by
	// per-request sockets plus the daemon's own idle timeout/autospawn path.
	return mcp.ServeOptions{}
}

func mcpLifecycleContext() (context.Context, context.CancelFunc) {
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(sigCtx)
	initialParent := os.Getppid()
	go watchMCPParent(ctx, cancel, initialParent, os.Getppid, mcpParentPollInterval)
	return ctx, func() {
		cancel()
		stopSignals()
	}
}

func watchMCPParent(ctx context.Context, cancel context.CancelFunc, initialParent int, getppid func() int, interval time.Duration) {
	if initialParent <= 1 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if current := getppid(); current <= 1 || current != initialParent {
				cancel()
				return
			}
		}
	}
}

func dialMCPDaemon(ctx context.Context, socketPath string) (*dial.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := dial.Connect(socketPath)
	if errors.Is(err, dial.ErrSocketMissing) {
		conn, err = dial.AutospawnAndConnectContext(ctx, socketPath)
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func printMCPUsage(w io.Writer) {
	fmt.Fprintf(w, "%s mcp - run the stdio MCP server for local AI clients\n", productidentity.Executable)
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Usage: %s mcp [--profile full|monitor]\n", productidentity.Executable)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Configure your MCP host with an absolute command path and the arg \"mcp\":")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "{")
	fmt.Fprintln(w, `  "mcpServers": {`)
	fmt.Fprintf(w, "    %q: { %q: %q, %q: [%q] },\n", "canary", "command", "/ABSOLUTE/PATH/TO/canary", "args", "mcp")
	fmt.Fprintf(w, "    %q: { %q: %q, %q: [%q, %q, %q] }\n", "canary-monitor", "command", "/ABSOLUTE/PATH/TO/canary", "args", "mcp", "--profile", "monitor")
	fmt.Fprintln(w, "  }")
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The full profile exposes 13 read-only canary_* tools. It has no resources,")
	fmt.Fprintln(w, "streaming subscriptions, settings writes, order previews, or broker actions.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "The monitor profile exposes only canary_brief and canary_status for low-token checks.")
}
