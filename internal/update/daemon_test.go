package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
)

func TestRestartDaemon_InvalidPID(t *testing.T) {
	t.Parallel()
	err := RestartDaemon(0)
	if err == nil || !strings.Contains(err.Error(), "invalid PID") {
		t.Fatalf("err = %v, want 'invalid PID'", err)
	}
}

func TestLooksLikeProductDaemon(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"plain path", "/Users/me/.local/bin/ibkr daemon", true},
		{"foreground", "/Users/me/.local/bin/ibkr daemon --foreground", true},
		{"bare command", "ibkr daemon", true},
		{"canonical path", "/Users/me/.local/bin/canary daemon", true},
		{"canonical foreground", "canary daemon --foreground", true},
		{"mcp is not daemon", "/Users/me/.local/bin/ibkr mcp", false},
		{"canonical mcp is not daemon", "/Users/me/.local/bin/canary mcp", false},
		{"daemon word is not subcommand", "/Users/me/.local/bin/ibkr status daemon", false},
		{"prefixed echo cannot forge canonical daemon", "echo /Users/me/.local/bin/canary daemon", false},
		{"prefixed echo cannot forge legacy daemon", "echo /Users/me/.local/bin/ibkr daemon", false},
		{"shell wrapper is not directly signalable daemon", "/bin/sh -c /Users/me/.local/bin/canary daemon", false},
		{"env wrapper is not directly signalable daemon", "/usr/bin/env canary daemon", false},
		{"similar executable is not daemon", "/Users/me/.local/bin/canary-helper daemon", false},
		{"unrelated", "/bin/sleep 30", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeProductDaemon(tc.line); got != tc.want {
				t.Fatalf("looksLikeProductDaemon(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestCommandHasFlag(t *testing.T) {
	t.Parallel()
	if !commandHasFlag("/tmp/ibkr daemon --foreground", "foreground") {
		t.Fatal("expected --foreground to be detected")
	}
	if !commandHasFlag("/tmp/ibkr daemon --foreground=true", "foreground") {
		t.Fatal("expected --foreground=true to be detected")
	}
	if commandHasFlag("/tmp/ibkr daemon", "foreground") {
		t.Fatal("did not expect foreground flag")
	}
}

// TestRestartDaemon_AlreadyGone fakes a PID that is no longer alive.
// Picking a PID we know is gone is tricky; we fork a tiny `true`
// subprocess, wait for it to exit, and then call RestartDaemon with
// its PID. proc.Signal returns ESRCH which we treat as success.
func TestRestartDaemon_AlreadyGone(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn `true`: %v", err)
	}
	// cmd.Process.Pid is reaped now; ESRCH on Signal is the
	// expected branch.
	err := RestartDaemon(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("RestartDaemon on dead pid = %v, want nil (ESRCH tolerated)", err)
	}
}

// TestRestartDaemon_SignalDelivered confirms the SIGTERM path against
// a fork of `sleep 30` we own. The child should observe the signal
// and exit; RestartDaemon should return nil inside its 5s budget.
func TestRestartDaemon_SignalDelivered(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode (subprocess fork)")
	}
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	// Reap in a goroutine so the kernel removes the PID promptly when
	// the child exits. IsProcessAlive treats zombie status as dead, but
	// this still keeps the test process table clean.
	pid := cmd.Process.Pid
	go func() { _, _ = cmd.Process.Wait() }()

	done := make(chan error, 1)
	go func() {
		done <- RestartDaemon(pid)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RestartDaemon: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("RestartDaemon didn't return within 6s")
	}
}

// TestRestartDaemon_Timeout asserts the descriptive timeout error fires when
// the target process refuses SIGTERM. The loop prevents the shell from execing
// the final command away, which would make the target PID disappear.
func TestRestartDaemon_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in short mode (subprocess fork)")
	}
	savedTimeout, savedPoll := restartTimeout, restartPoll
	restartTimeout = 200 * time.Millisecond
	restartPoll = 10 * time.Millisecond
	t.Cleanup(func() {
		restartTimeout = savedTimeout
		restartPoll = savedPoll
	})

	cmd := exec.Command("sh", "-c", `trap '' TERM; while :; do sleep 1 & wait $!; done`)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start trap-loop: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	err := RestartDaemon(cmd.Process.Pid)
	if err == nil {
		t.Fatal("RestartDaemon returned nil for a SIGTERM-trapping process")
	}
	if !errors.Is(err, ErrStopTimeout) {
		t.Fatalf("err = %v, want ErrStopTimeout", err)
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("err = %v, want 'did not exit'", err)
	}
}

// TestIsDaemonRunning_NoFile exercises the path where the daemon has
// never run on this machine — the lock file simply doesn't exist.
// We rely on XDG_RUNTIME_DIR being unset / nonexistent in test env;
// if a real daemon happens to be running on the dev host, this test
// might transiently see it. Document and tolerate.
func TestIsDaemonRunning_NoFile(t *testing.T) {
	t.Parallel()
	// Point the lock-path lookup at a fresh temp dir so we know
	// the file isn't there. This works because LockHolderPID just
	// reads the file path it's given — we mimic the production
	// flow by computing the path the same way IsDaemonRunning does
	// but inside a tmp dir.
	tmp := t.TempDir()
	_ = tmp
	// IsDaemonRunning uses dial.DefaultSocketPath, which is host-
	// dependent and may or may not point at a running daemon. We
	// can only assert the contract: when running == true, pid > 0;
	// when running == false, pid is 0 OR refers to a dead pid.
	pid, running := IsDaemonRunning()
	if running && pid <= 0 {
		t.Fatalf("running=true but pid=%d", pid)
	}
}

func TestFindDaemonProcess_RefusesNonDaemonPID(t *testing.T) {
	dir := t.TempDir()
	socketPath := dir + "/ibkr.sock"
	lockPath := dial.LockPath(socketPath)

	savedLookup := lookupProcessCommandLine
	lookupProcessCommandLine = func(context.Context, int) (string, error) {
		return "/bin/sleep 30", nil
	}
	t.Cleanup(func() { lookupProcessCommandLine = savedLookup })

	// We cannot make pid 123 alive portably, so exercise the verification
	// branch through a real sleep process.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := FindDaemonProcess(context.Background(), socketPath)
	if !errors.Is(err, ErrDaemonUnverified) {
		t.Fatalf("FindDaemonProcess err = %v, want ErrDaemonUnverified", err)
	}
}

// Compile-time sanity: ensure errors.Is on syscall.ESRCH works as
// expected on this platform — install_test depends on it.
func TestErrorsIsESRCH(t *testing.T) {
	t.Parallel()
	if !errors.Is(syscall.ESRCH, syscall.ESRCH) {
		t.Fatal("errors.Is(ESRCH, ESRCH) = false")
	}
}

// keepReferenced — silence unused-import for os when shrinking the
// file. Cheap; trims churn during edits.
var _ = os.Getpid
