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

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon"
	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/logrotate"
	"github.com/osauer/canary/v3/internal/productidentity"
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
	removeAgent := fs.Bool("remove-agent", false, "macOS: stop the daemon and remove its launchd agent; the daemon then starts directly again")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *removeAgent {
		removed, err := dial.RemoveDaemonAgent(context.Background())
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "%s daemon: remove launchd agent: %v\n", productidentity.Executable, err)
			os.Exit(1)
		case removed:
			fmt.Println("Stopped the daemon and removed its launchd agent. The next command starts the daemon directly; `canary restart` installs the agent again.")
		default:
			fmt.Println("No launchd agent for the daemon is installed.")
		}
		return
	}

	if *showVer {
		printVersion(os.Stdout, productidentity.Executable+" daemon", false)
		return
	}

	// Only unreadable account-identity pins stop the daemon (owner decision
	// 2026-09-26); every other unreadable part runs on Canary's default and is
	// reported on status, the brief and the alert inbox.
	socketPath := *socket
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}
	// launchd starts its agent at login and after a kill, with nobody asking.
	// An operator's stop still holds then.
	if dial.RunningUnderDaemonAgent() && errors.Is(dial.CheckAutostart(socketPath), dial.ErrStopped) {
		return
	}

	cfg, configIssues, err := config.LoadForDaemon(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		daemonExit(2)
	}
	resolved, err := cfg.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		daemonExit(2)
	}
	if *foreground {
		resolved.Daemon.SetIdleTimeout(0)
	}

	effectiveLogPath := *logPath
	if effectiveLogPath == "" {
		effectiveLogPath = dial.DefaultLogPath()
	}
	logWriter, err := openDaemonLog(effectiveLogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		daemonExit(2)
	}
	defer func() {
		if c, ok := logWriter.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	// The supervisor redirected stderr into the log file; move the
	// process-level fatal stream (runtime dumps, panics) to its own file so
	// a single SIGQUIT cannot bury the log, and so the log monitor sees a
	// crash as one signal. A failure here is reported and tolerated.
	var crashErr error
	if effectiveLogPath != "stderr" {
		_, crashErr = captureCrashOutput(dial.CrashLogPath(effectiveLogPath))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := daemon.NewLogger(logWriter, resolved.Daemon.LogLevel)
	if crashErr != nil {
		logger.Warnf("crash output stays in the daemon log: %v", crashErr)
	}
	for _, issue := range configIssues {
		logger.Warnf("config: %s", issue)
	}

	// The teardown is bounded from the first of: a stop signal, or the end
	// of serving (an idle exit has no signal).
	shutdownStarted := shutdownBound(daemonShutdownGrace, exitHungShutdown(logger.Errorf))
	go func() {
		<-ctx.Done()
		shutdownStarted()
	}()
	srv := daemon.New(daemon.Options{
		DisableMacroSources: daemonMacroSources == "disabled",
		Config:              resolved,
		SocketPath:          socketPath,
		Version:             effectiveVersion(),
		Logger:              logger,
		EnsurePolicyFiles:   true,
		ConfigIssues:        configIssues,
		ShutdownStarted:     shutdownStarted,
	})
	defer srv.Stop()

	if err := srv.Start(ctx); err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			logger.Infof("Another daemon is already running for socket %s; exiting cleanly", socketPath)
			return
		}
		logger.Errorf("start: %v", err)
		daemonExit(1)
	}
}

// daemonExit ends a daemon that refuses to start. Under the launchd agent
// the status is 0 whatever the reason: launchd restarts the agent ten
// seconds after any other status, and a daemon that cannot start on a broken
// config would be started again every ten seconds for as long as it stays
// broken. The reason is in the log; the next client's start tries again.
func daemonExit(code int) {
	if dial.RunningUnderDaemonAgent() {
		os.Exit(0)
	}
	os.Exit(code)
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
