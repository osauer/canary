package dial

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AutospawnTimeout is the budget for this executable's `daemon` mode to start
// and open its Unix socket. Discovery + the gateway handshake run in the background, so the
// socket appears as soon as the daemon reaches its accept loop — sub-second
// on a healthy machine.
const AutospawnTimeout = 5 * time.Second

// AutospawnAndConnect spawns this binary's `daemon` mode (located via
// os.Executable), waits for the Unix socket to appear at socketPath, and
// returns a live connection. On wait failure the returned error is annotated
// with whatever the lock file tells us plus the last daemon log line.
//
// Shared between the CLI entry and internal/mcp (stdio MCP server) —
// both surfaces need the same "is the daemon up? if not, start it" dance.
//
// Pre-spawn check: if the lock file points at a live PID, the daemon is
// already running — either still booting (socket not yet up) or stuck.
// Spawning another daemon there is wasted work because the flock would
// reject it; worse, when the lock file has been deleted out from under a
// live daemon (manual `rm`, aggressive cleanup script), a fresh spawn
// can co-exist with the old one and both hold a gateway connection.
//
// Shutdown race: the daemon's Stop sequence removes the socket BEFORE it
// releases the lock. A CLI invocation that arrives during that window
// sees "PID alive + lock present + socket gone" — looks identical to a
// stuck daemon. To distinguish: poll PID liveness while waiting; when
// the daemon finishes exiting, fall through to spawn a fresh one. Only
// surface the "stuck daemon" error when the PID stays alive through the
// full budget.
func AutospawnAndConnect(socketPath string) (*Conn, error) {
	return AutospawnAndConnectContext(context.Background(), socketPath)
}

// AutospawnAndConnectContext is AutospawnAndConnect with a caller-owned
// cancellation signal. It is used by stdio MCP so protocol shutdown can abort a
// pending daemon startup instead of leaving the server around after its host is
// gone.
func AutospawnAndConnectContext(ctx context.Context, socketPath string) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lockPath := LockPath(socketPath)
	if pid := LockHolderPID(lockPath); pid > 0 && IsProcessAlive(pid) {
		if conn, ok := waitForSocketOrPIDDeath(ctx, socketPath, pid, AutospawnTimeout); ok {
			return conn, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Either the PID died during the wait (graceful shutdown finished
		// — fall through to spawn) or the budget ran out with the PID
		// still alive (stuck daemon — surface the error).
		if IsProcessAlive(pid) {
			msg := fmt.Sprintf("daemon PID %d is running but never opened the socket %s within %s\n  if it's stuck, run: kill %d",
				pid, socketPath, AutospawnTimeout, pid)
			if tail := TailLastLine(DefaultLogPath(), 0); tail != "" {
				msg = fmt.Sprintf("%s\n  last daemon log: %s", msg, tail)
			}
			return nil, errors.New(msg)
		}
		// PID died — fall through to spawn.
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := spawnDaemon(); err != nil {
		return nil, fmt.Errorf("failed to start daemon: %w", err)
	}
	conn, waitErr := WaitForSocketContext(ctx, socketPath, AutospawnTimeout)
	if waitErr == nil {
		return conn, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pid := LockHolderPID(lockPath)
	msg := waitErr.Error()
	if pid > 0 && IsProcessAlive(pid) {
		msg = fmt.Sprintf("%s\n  daemon PID %d holds %s but never opened the socket\n  if it's stuck, run: kill %d",
			msg, pid, lockPath, pid)
	}
	if tail := TailLastLine(DefaultLogPath(), 0); tail != "" {
		msg = fmt.Sprintf("%s\n  last daemon log: %s", msg, tail)
	}
	return nil, errors.New(msg)
}

// AutospawnAndConnectContextFromExecutable starts exactly executable and then
// verifies that the spawned PID owns the daemon lock before returning a
// connection. It is intentionally stricter than the ordinary autospawn path:
// callers use it after replacing an installed binary and stopping the prior
// daemon, so connecting to a concurrently started daemon from an unknown
// executable would be a false-success cutover.
func AutospawnAndConnectContextFromExecutable(ctx context.Context, socketPath, executable string) (*Conn, error) {
	return AutospawnAndConnectContextFromExecutableWithTimeout(ctx, socketPath, executable, AutospawnTimeout)
}

// AutospawnAndConnectContextFromExecutableWithTimeout is the exact-executable
// cutover path with a caller-owned startup budget. Ordinary CLI/MCP autospawn
// retains AutospawnTimeout; restart uses its explicit timeout because a first
// start may include validated schema migration before the socket is published.
func AutospawnAndConnectContextFromExecutableWithTimeout(ctx context.Context, socketPath, executable string, startupTimeout time.Duration) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if startupTimeout <= 0 {
		startupTimeout = AutospawnTimeout
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return nil, errors.New("daemon executable is empty")
	}

	lockPath := LockPath(socketPath)
	if pid := LockHolderPID(lockPath); pid > 0 && IsProcessAlive(pid) {
		return nil, fmt.Errorf("refusing exact-executable daemon start: pid %d already owns %s", pid, lockPath)
	}
	if conn, err := Connect(socketPath); err == nil {
		_ = conn.Close()
		return nil, fmt.Errorf("refusing exact-executable daemon start: %s is already serving without a verified live lock owner", socketPath)
	} else if !errors.Is(err, ErrSocketMissing) {
		return nil, err
	}

	spawnedPID, err := spawnDaemonFromExecutable(executable)
	if err != nil {
		return nil, fmt.Errorf("failed to start daemon from %s: %w", executable, err)
	}
	conn, waitErr := WaitForSocketContext(ctx, socketPath, startupTimeout)
	if waitErr != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg := waitErr.Error()
		if tail := TailLastLine(DefaultLogPath(), 0); tail != "" {
			msg = fmt.Sprintf("%s\n  last daemon log: %s", msg, tail)
		}
		return nil, errors.New(msg)
	}

	holderPID := LockHolderPID(lockPath)
	if holderPID != spawnedPID || !IsProcessAlive(holderPID) {
		_ = conn.Close()
		return nil, fmt.Errorf("daemon executable verification failed: spawned pid %d but live lock owner is pid %d", spawnedPID, holderPID)
	}
	return conn, nil
}

// waitForSocketOrPIDDeath polls for two outcomes in parallel: the socket
// becoming available (return conn, true) or the watched PID dying
// (return nil, false). On budget exhaustion returns (nil, false) too —
// callers distinguish stuck-but-alive from genuinely-dead by probing
// IsProcessAlive again after the call.
func waitForSocketOrPIDDeath(ctx context.Context, socketPath string, pid int, timeout time.Duration) (*Conn, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, false
		}
		if conn, err := Connect(socketPath); err == nil {
			return conn, true
		}
		if !IsProcessAlive(pid) {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(75 * time.Millisecond):
		}
	}
	return nil, false
}

// spawnDaemon starts this executable's `daemon` mode detached from the caller.
// current binary is located via os.Executable() — no PATH lookup, no separate
// daemon binary, no executable-name environment override.
//
// Stdout/stderr route to the daemon log file (or /dev/null on fallback).
// Leaving Cmd.Stdout/Stderr at the zero value wired exec to a closed fd on
// macOS and wedged the daemon during startup before it could log.
func spawnDaemon() error {
	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate self: %w", err)
	}
	_, err = spawnDaemonFromExecutable(bin)
	return err
}

func spawnDaemonFromExecutable(bin string) (int, error) {
	bin = strings.TrimSpace(bin)
	if bin == "" {
		return 0, errors.New("daemon executable is empty")
	}
	cmd := exec.Command(bin, "daemon")
	cmd.Stdin = nil

	logPath := DefaultLogPath()
	logDir := filepath.Dir(logPath)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, fmt.Errorf("create daemon log dir: %w", err)
	}
	if err := os.Chmod(logDir, 0o700); err != nil {
		return 0, fmt.Errorf("secure daemon log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		if err := logFile.Chmod(0o600); err != nil {
			_ = logFile.Close()
			return 0, fmt.Errorf("secure daemon log file: %w", err)
		}
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		defer logFile.Close()
	} else {
		devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			return 0, fmt.Errorf("open /dev/null for daemon stdio: %w", err)
		}
		cmd.Stdout = devnull
		cmd.Stderr = devnull
		defer devnull.Close()
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Long-lived callers such as the app must reap a daemon that exits during
	// startup. Process.Release leaves that failed child as a zombie on Unix.
	go func() { _ = cmd.Wait() }()
	return pid, nil
}
