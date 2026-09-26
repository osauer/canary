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

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/logrotate"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// daemonMacroSources is overridden only by the hermetic integration binary.
// Production builds always start the daemon-owned public feed collectors.
var daemonMacroSources = "enabled"

func runDaemon(args []string) {
	fs := flag.NewFlagSet(productidentity.Executable+" daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default $XDG_CONFIG_HOME/ibkr/config.toml)")
	socket := fs.String("socket", "", "unix socket path (default $XDG_RUNTIME_DIR/ibkr/ibkr.sock)")
	logPath := fs.String("log", "", fmt.Sprintf("log file path (default %s; 'stderr' for stderr)", dial.DisplayPath(dial.DefaultLogPath())))
	foreground := fs.Bool("foreground", false, "run in foreground; do not idle-shutdown")
	showVer := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *showVer {
		printVersion(os.Stdout, productidentity.Executable+" daemon", false)
		return
	}

	// Only unreadable account-identity pins stop the daemon (owner decision
	// 2026-09-26); every other unreadable part runs on Canary's default and is
	// reported on status, the brief and the alert inbox.
	cfg, configIssues, err := config.LoadForDaemon(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(2)
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(2)
	}
	if *foreground {
		resolved.Daemon.SetIdleTimeout(0)
	}

	socketPath := *socket
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}

	logWriter, err := openDaemonLog(*logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		os.Exit(2)
	}
	defer func() {
		if c, ok := logWriter.(io.Closer); ok {
			_ = c.Close()
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := daemon.NewLogger(logWriter, resolved.Daemon.LogLevel)
	for _, issue := range configIssues {
		logger.Warnf("config: %s", issue)
	}

	srv := daemon.New(daemon.Options{
		DisableMacroSources: daemonMacroSources == "disabled",
		Config:              resolved,
		SocketPath:          socketPath,
		Version:             effectiveVersion(),
		Logger:              logger,
		EnsurePolicyFiles:   true,
		ConfigIssues:        configIssues,
	})
	defer srv.Stop()

	if err := srv.Start(ctx); err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			logger.Infof("Another daemon is already running for socket %s; exiting cleanly", socketPath)
			return
		}
		logger.Errorf("start: %v", err)
		os.Exit(1)
	}
}

func openDaemonLog(path string) (io.Writer, error) {
	if path == "stderr" {
		return os.Stderr, nil
	}
	if path == "" {
		path = dial.DefaultLogPath()
	}
	return logrotate.Open(path, logrotate.DefaultMaxBytes)
}
