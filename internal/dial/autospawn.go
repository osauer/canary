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

// startupBudgetBase covers the daemon's fixed boot cost: process start,
// endpoint discovery, and socket publication. Discovery and the gateway
// handshake run in the background, so the socket appears as soon as the daemon
// reaches its accept loop — sub-second on a healthy machine, and this margin
// absorbs a loaded desk. It is deliberately unchanged from the flat budget
// that preceded StartupBudget: a desk with no authority to verify still
// detects a wedged daemon just as fast as it used to.
const startupBudgetBase = 5 * time.Second

// startupBudgetFloorBytesPerSec is the authority-verification throughput the
// budget assumes. Measured end to end on a warm NVMe desk (SQLite quick_check
// plus a full payload re-hash, twice) it is roughly 110 MiB/s; budgeting a
// third of that keeps a healthy-but-slow boot inside the budget on a busy or
// slower machine.
const startupBudgetFloorBytesPerSec = 40 << 20

// startupBudgetMax bounds the derived wait so an authority that has grown
// pathologically large still surfaces as a failure rather than hanging the
// caller indefinitely.
const startupBudgetMax = 5 * time.Minute

// StartupBudget returns how long to wait for a starting daemon to publish its
// socket.
//
// A fixed constant is wrong by construction: before the daemon accepts
// connections it verifies the whole authority database — SQLite quick_check,
// the foreign-key check, and a re-hash of every stored payload — so the
// pre-socket window scales linearly with how much history the desk has
// accumulated. A 6 GB authority takes ~50s to verify, which a fixed 5s budget
// reports as a dead daemon while it is in fact healthy.
//
// An authority that cannot be sized falls back to the base: an absent file is
// a first start with nothing to verify, and a stat error must not shorten the
// wait below what a fresh install would get.
func StartupBudget() time.Duration {
	path, err := DefaultAuthorityPath()
	if err != nil {
		return startupBudgetBase
	}
	info, err := os.Stat(path)
	if err != nil {
		return startupBudgetBase
	}
	budget := startupBudgetBase + time.Duration(info.Size()/startupBudgetFloorBytesPerSec)*time.Second
	return min(budget, startupBudgetMax)
}

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
	budget := StartupBudget()
	lockPath := LockPath(socketPath)
	if pid := LockHolderPID(lockPath); pid > 0 && IsProcessAlive(pid) {
		if conn, ok := waitForSocketOrPIDDeath(ctx, socketPath, pid, budget); ok {
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
				pid, socketPath, budget, pid)
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
	spawnedPID, err := spawnDaemon()
	if err != nil {
		return nil, fmt.Errorf("failed to start daemon: %w", err)
	}
	// Watching the spawned PID matters more the longer the budget runs: a
	// daemon that dies on a corrupt authority must report in milliseconds
	// rather than burning the whole verification budget first.
	if conn, ok := waitForSocketOrPIDDeath(ctx, socketPath, spawnedPID, budget); ok {
		return conn, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	msg := fmt.Sprintf("daemon socket did not appear within %s", budget)
	if !IsProcessAlive(spawnedPID) {
		msg = fmt.Sprintf("daemon pid %d exited during startup without opening %s", spawnedPID, socketPath)
	} else if pid := LockHolderPID(lockPath); pid > 0 && IsProcessAlive(pid) {
		msg = fmt.Sprintf("%s\n  daemon PID %d holds %s but never opened the socket\n  if it's stuck, run: kill %d",
			msg, pid, lockPath, pid)
	}
	if tail := TailLastLine(DefaultLogPath(), 0); tail != "" {
		msg = fmt.Sprintf("%s\n  last daemon log: %s", msg, tail)
	}
	return nil, errors.New(msg)
}

// AutospawnAndConnectContextFromExecutableWithTimeout starts exactly executable
// and then verifies that the spawned PID owns the daemon lock before returning
// a connection. It is intentionally stricter than the ordinary autospawn path:
// callers use it after replacing an installed binary and stopping the prior
// daemon, so connecting to a concurrently started daemon from an unknown
// executable would be a false-success cutover.
//
// The startup budget is caller-owned rather than derived here because a
// restart may also have to carry a validated schema migration before the
// socket is published. A non-positive budget falls back to StartupBudget.
func AutospawnAndConnectContextFromExecutableWithTimeout(ctx context.Context, socketPath, executable string, startupTimeout time.Duration) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if startupTimeout <= 0 {
		startupTimeout = StartupBudget()
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
	conn, ok := waitForSocketOrPIDDeath(ctx, socketPath, spawnedPID, startupTimeout)
	if !ok {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		msg := fmt.Sprintf("daemon socket did not appear within %s", startupTimeout)
		if !IsProcessAlive(spawnedPID) {
			msg = fmt.Sprintf("daemon pid %d exited during startup without opening %s", spawnedPID, socketPath)
		}
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
func spawnDaemon() (int, error) {
	bin, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate self: %w", err)
	}
	return spawnDaemonFromExecutable(bin)
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
