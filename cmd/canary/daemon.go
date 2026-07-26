package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
)

func runDaemon(args []string) {
	fs := flag.NewFlagSet(productidentity.Executable+" daemon", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path (default $XDG_CONFIG_HOME/ibkr/config.toml)")
	socket := fs.String("socket", "", "unix socket path (default $XDG_RUNTIME_DIR/ibkr/ibkr.sock)")
	logPath := fs.String("log", "", "log file path (default ~/.local/state/ibkr/ibkr-daemon.log; 'stderr' for stderr)")
	foreground := fs.Bool("foreground", false, "run in foreground; do not idle-shutdown")
	showVer := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *showVer {
		printVersion(os.Stdout, productidentity.Executable+" daemon", false)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
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

	srv := daemon.New(daemon.Options{
		Config:     resolved,
		SocketPath: socketPath,
		Version:    effectiveVersion(),
		Logger:     logger,
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

// maxDaemonLogBytes caps the daemon log at rotation time. The log is a
// diagnostic stream — trade audit lives in the order journal — opened O_APPEND
// and shared across the many idle-shutdown/autospawn restarts a box sees in a
// day, so without a cap it grows unbounded over weeks (observed at 1.5 GB).
// At each daemon boot we roll an over-cap file aside to <path>.1 (one
// generation kept) and start fresh, bounding on-disk use to ~2x this.
const maxDaemonLogBytes = 64 << 20 // 64 MiB

func openDaemonLog(path string) (io.Writer, error) {
	if path == "stderr" {
		return os.Stderr, nil
	}
	if path == "" {
		path = dial.DefaultLogPath()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	rotateDaemonLogIfLarge(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// rotateDaemonLogIfLarge rolls path aside to path+".1" when it has grown past
// maxDaemonLogBytes, keeping a single previous generation. Best-effort: any
// stat/rename error leaves the existing file in place for the O_APPEND open to
// continue, so a rotation hiccup never blocks daemon start.
func rotateDaemonLogIfLarge(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < maxDaemonLogBytes {
		return
	}
	_ = os.Rename(path, path+".1")
}
