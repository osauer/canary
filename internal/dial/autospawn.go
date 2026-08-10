package dial

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// startupBudgetBase covers the daemon's fixed boot cost: process start,
// that preceded StartupBudget: a desk with no authority to verify still
const startupBudgetBase = 5 * time.Second

// startupBudgetFloorBytesPerSec is the effective throughput the startup budget
// assumes for one full-authority unit of work. Measured end to end on a warm
const startupBudgetFloorBytesPerSec = 40 << 20

// startupBudgetAuthorityPasses is a conservative source-size work-amplification
// allowance for an out-of-place schema upgrade. The v4 maintenance path may
// validate the source, build a disposable migrated snapshot, compact it, create
// the exact-source backup, fingerprint/reverify artifacts, and verify the
// published authority before the socket exists. Some stages touch less after
// pruning and raw copies are faster than integrity scans, so six full-size
// units model the critical path without teaching this adapter schema details.
const startupBudgetAuthorityPasses = 6

// startupBudgetMax is still a hard upper bound for a live but genuinely wedged
// daemon. At the conservative throughput above, a 50 GiB authority receives a
// little over two hours; the four-hour ceiling covers roughly 90 GiB before it
// saturates. A daemon that exits is detected independently and returns at once.
const startupBudgetMax = 4 * time.Hour

// StartupBudget returns how long to wait for a starting daemon to publish its
// socket.
//
// A fixed constant is wrong by construction: before the daemon accepts
// connections it validates the whole authority and may perform a crash-safe
// out-of-place schema upgrade. Both ordinary validation and upgrade work scale
// with the authority size. The budget therefore prices a conservative number
// of source-size work units rather than assuming one validation pass.
//
// An existing daemon.db is the direct size input. When it is absent, the same
// path can mean either a fresh install or a file-backed release that must first
// import and seal its legacy state corpus. The latter is sized recursively
// within the persistent namespace without following symbolic links. An
// incomplete legacy walk receives the finite maximum budget; a daemon that
// cannot start still reports promptly through the independent PID-death path.
func StartupBudget() time.Duration {
	path, err := DefaultAuthorityPath()
	if err != nil {
		return startupBudgetBase
	}
	info, err := os.Stat(path)
	if err == nil {
		return startupBudgetForAuthorityBytes(info.Size())
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return startupBudgetBase
	}
	legacyBytes, err := legacyStateCorpusBytes(filepath.Dir(path))
	if err != nil {
		return startupBudgetMax
	}
	return startupBudgetForAuthorityBytes(legacyBytes)
}

// startupBudgetForAuthorityBytes applies the size model without overflowing
func startupBudgetForAuthorityBytes(authorityBytes int64) time.Duration {
	if authorityBytes <= 0 {
		return startupBudgetBase
	}

	maxWorkSeconds := uint64((startupBudgetMax - startupBudgetBase) / time.Second)
	secondsPerPass := (uint64(authorityBytes) + startupBudgetFloorBytesPerSec - 1) / startupBudgetFloorBytesPerSec
	if secondsPerPass > maxWorkSeconds/startupBudgetAuthorityPasses {
		return startupBudgetMax
	}
	workSeconds := secondsPerPass * startupBudgetAuthorityPasses
	return startupBudgetBase + time.Duration(workSeconds)*time.Second
}

// legacyStateCorpusBytes returns the apparent bytes in the pre-SQLite
// persistent namespace. Apparent size matches the amount the cutover readers
// must consume, including sparse journals. WalkDir deliberately does not
// follow symbolic links; explicit checks make that boundary visible and also
// cover a symbolic-link namespace root.
func legacyStateCorpusBytes(root string) (int64, error) {
	rootInfo, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 {
		return 0, nil
	}
	if !rootInfo.IsDir() {
		return 0, fmt.Errorf("legacy state namespace is not a directory")
	}

	var total int64
	err = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				// A source disappearing during sizing can only reduce the
				// subsequent cutover workload.
				return nil
			}
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total = saturatingAuthorityBytes(total, info.Size())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

func saturatingAuthorityBytes(total, additional int64) int64 {
	const maxInt64 = int64(1<<63 - 1)
	if additional <= 0 {
		return total
	}
	if total >= maxInt64-additional {
		return maxInt64
	}
	return total + additional
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
