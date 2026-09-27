package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// Errors reported while locating or stopping the daemon process.
var (
	ErrDaemonNotRunning = errors.New("daemon not running")
	ErrDaemonUnverified = errors.New("daemon process could not be verified")
	ErrStopTimeout      = errors.New("daemon stop timed out")
)

// DaemonProcess is the verified process currently holding the daemon pidfile.
type DaemonProcess struct {
	PID        int
	Command    string
	SocketPath string
	LockPath   string
	Foreground bool
}

// FindDaemonProcess returns the live Canary daemon process for socketPath.
//
// Commands that send signals must not trust a stale or forged pidfile. A live
// pidfile holder is
// accepted only when its command line uses the canonical `canary daemon` or a
// pre-upgrade `ibkr daemon` process that must be quiesced. A responding
// socket without a verifiable pidfile is treated as unverified rather than
// killed.
func FindDaemonProcess(ctx context.Context, socketPath string) (DaemonProcess, error) {
	if socketPath == "" {
		socketPath = dial.DefaultSocketPath()
	}
	lockPath := dial.LockPath(socketPath)
	proc := DaemonProcess{SocketPath: socketPath, LockPath: lockPath}

	pid := dial.LockHolderPID(lockPath)
	if pid <= 0 || !dial.IsProcessAlive(pid) {
		conn, err := dial.Connect(socketPath)
		if err == nil {
			_ = conn.Close()
			return proc, fmt.Errorf("%w: socket %s is serving but %s has no live PID", ErrDaemonUnverified, socketPath, lockPath)
		}
		if errors.Is(err, dial.ErrSocketMissing) {
			return proc, ErrDaemonNotRunning
		}
		return proc, err
	}

	cmdline, err := lookupProcessCommandLine(ctx, pid)
	if err != nil {
		return proc, fmt.Errorf("%w: pid %d: %v", ErrDaemonUnverified, pid, err)
	}
	if !looksLikeProductDaemon(cmdline) {
		return proc, fmt.Errorf("%w: pid %d command %q is not a Canary daemon", ErrDaemonUnverified, pid, cmdline)
	}

	proc.PID = pid
	proc.Command = cmdline
	proc.Foreground = commandHasFlag(cmdline, "foreground")
	return proc, nil
}

var lookupProcessCommandLine = processCommandLine

func processCommandLine(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("invalid PID")
	}
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "args=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return "", errors.New("empty command line")
	}
	return cmdline, nil
}

func looksLikeProductDaemon(cmdline string) bool {
	_, _, ok := SplitManagedCommand(cmdline, "daemon")
	return ok
}

// SplitManagedCommand splits a command line printed by `ps -o args=` into a
// managed Canary executable and its arguments, which start with subcommand.
// ok is false unless subcommand directly follows an executable whose base
// name is a managed product executable.
//
// ps joins argv with spaces, so an install path containing a space (macOS
// "Application Support") spans several fields and reads exactly like a
// wrapper whose arguments end in a managed path, such as
// "/bin/sh -c /opt/bin/canary daemon". A single-field executable is matched
// lexically, which keeps a bare "canary daemon" and a pre-upgrade binary
// already removed from disk recognisable. An executable spanning fields must
// also be an absolute path to an existing regular file: that check, not the
// text, tells an install path from a wrapper. Fields are rejoined with single
// spaces, so a path with consecutive spaces is refused rather than guessed.
func SplitManagedCommand(cmdline, subcommand string) (executable string, args []string, ok bool) {
	return splitManagedCommand(cmdline, subcommand, func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.Mode().IsRegular()
	})
}

func splitManagedCommand(cmdline, subcommand string, isFile func(string) bool) (string, []string, bool) {
	fields := strings.Fields(cmdline)
	for i := 1; i < len(fields); i++ {
		if fields[i] != subcommand {
			continue
		}
		executable := strings.Join(fields[:i], " ")
		if !productidentity.IsManagedProcessExecutableBase(filepath.Base(executable)) {
			continue
		}
		if i > 1 && (!filepath.IsAbs(executable) || !isFile(executable)) {
			continue
		}
		return executable, fields[i:], true
	}
	return "", nil, false
}

func commandHasFlag(cmdline, name string) bool {
	long := "--" + name
	short := "-" + name
	for field := range strings.FieldsSeq(cmdline) {
		if field == long || field == short || strings.HasPrefix(field, long+"=") || strings.HasPrefix(field, short+"=") {
			return true
		}
	}
	return false
}

// Restart timing controls. Tests may shorten these package variables.
var (
	restartTimeout = 5 * time.Second
	restartPoll    = 100 * time.Millisecond
)

// StopDaemon sends SIGTERM to pid and waits until it exits. It does not
// verify the PID; callers that read a pidfile should call FindDaemonProcess
// first. On timeout it returns an error wrapping ErrStopTimeout so callers can
// decide whether to escalate to SIGKILL.
func StopDaemon(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return errors.New("StopDaemon: invalid PID")
	}
	if timeout <= 0 {
		timeout = restartTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// ESRCH or os.ErrProcessDone = process already gone — treat
		// as success. The Go runtime returns ErrProcessDone for PIDs
		// it has already reaped (e.g. test subprocess after Wait);
		// ESRCH is the raw syscall result on an unreaped PID we no
		// longer own. Either way, the post-condition we want is
		// "process is not running" and we have it.
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		// EPERM = we don't own the process. The daemon should always be
		// owned by the same user as the CLI; if not, the user is doing
		// something the update flow shouldn't paper over.
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(restartPoll)
	}
	return fmt.Errorf("%w: daemon (pid %d) did not exit within %s after SIGTERM", ErrStopTimeout, pid, timeout)
}

// KillDaemon sends SIGKILL to pid and waits until it exits. It is intended
// only as an explicit --force fallback after StopDaemon timed out.
func KillDaemon(pid int, timeout time.Duration) error {
	if pid <= 0 {
		return errors.New("KillDaemon: invalid PID")
	}
	if timeout <= 0 {
		timeout = restartTimeout
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find pid %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !dial.IsProcessAlive(pid) {
			return nil
		}
		time.Sleep(restartPoll)
	}
	return fmt.Errorf("daemon (pid %d) did not exit within %s after SIGKILL", pid, timeout)
}
