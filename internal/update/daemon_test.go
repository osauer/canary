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
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/procinfo"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// TestLooksLikeProductDaemon models a host on which only the Mac mini's Desk
// runtime executable exists; a single-field executable never consults it.
func TestLooksLikeProductDaemon(t *testing.T) {
	t.Parallel()
	const desk = "/Users/desk/Library/Application Support/Desk/runtime/canary"
	installed := func(path string) bool { return path == desk }
	cases := []struct {
		name       string
		cmdline    string
		executable string // empty when the command is not a Canary daemon
	}{
		{"install path with spaces", desk + " daemon", desk},
		{"foreground flag", "/usr/local/bin/canary daemon --foreground", "/usr/local/bin/canary"},
		{"bare command", "canary daemon", "canary"},
		{"pre-upgrade executable", "/usr/local/bin/ibkr daemon", "/usr/local/bin/ibkr"},
		{"spaced path, other subcommand", desk + " status", ""},
		{"spaced path, other executable", "/Users/x/Application Support/other daemon", ""},
		{"spaced path missing from disk", "/Users/x/Application Support/Desk/runtime/canary daemon", ""},
		{"interpreter argument", "python3 canary daemon", ""},
		{"daemon word is not the subcommand", "/usr/local/bin/canary status daemon", ""},
		{"similar executable", "/usr/local/bin/canary-helper daemon", ""},
		{"echo wrapper", "echo /usr/local/bin/canary daemon", ""},
		{"absolute echo wrapper", "/bin/echo /usr/local/bin/canary daemon", ""},
		{"shell wrapper", "/bin/sh -c /usr/local/bin/canary daemon", ""},
		{"shell wrapper around install path", "/bin/sh -c " + desk + " daemon", ""},
		{"env wrapper", "/usr/bin/env canary daemon", ""},
		{"unrelated", "/bin/sleep 30", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			executable, args, ok := splitManagedCommand(tc.cmdline, "daemon", installed)
			if ok != (tc.executable != "") || executable != tc.executable || (ok && args[0] != "daemon") {
				t.Fatalf("splitManagedCommand(%q) = %q, %q, %v; want executable %q", tc.cmdline, executable, args, ok, tc.executable)
			}
		})
	}
	if !commandHasFlag("/usr/local/bin/canary daemon --foreground", "foreground") {
		t.Fatal("--foreground was not detected")
	}
}

// TestFindDaemonProcessAcceptsInstallPathWithSpaces reproduces the Mac mini
// refusal: launchd runs the daemon from Desk's "Application Support" runtime,
// so ps reports an executable path that contains a space.
func TestFindDaemonProcessAcceptsInstallPathWithSpaces(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, "Application Support", "Desk", "runtime")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(runtimeDir, productidentity.Executable)
	if err := os.WriteFile(executable, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(dir, "Application Support", "Removed", productidentity.Executable)
	socketPath := filepath.Join(dir, productidentity.DaemonSocketName)

	// The pidfile holder must be a live process other than the test itself.
	holder := exec.Command("sleep", "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Signal(syscall.SIGKILL)
		_, _ = holder.Process.Wait()
	})
	pid := holder.Process.Pid
	if err := os.WriteFile(dial.LockPath(socketPath), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	savedLookup := lookupProcessCommandLine
	t.Cleanup(func() { lookupProcessCommandLine = savedLookup })
	for _, tc := range []struct {
		cmdline    string
		foreground bool
		wantErr    error
	}{
		{cmdline: executable + " daemon"},
		{cmdline: executable + " daemon --foreground", foreground: true},
		{cmdline: executable + " status", wantErr: ErrDaemonUnverified},
		{cmdline: removed + " daemon", wantErr: ErrDaemonUnverified},
	} {
		lookupProcessCommandLine = func(_ context.Context, got int) (string, error) {
			if got != pid {
				return "", fmt.Errorf("looked up pid %d, want %d", got, pid)
			}
			return tc.cmdline, nil
		}
		proc, err := FindDaemonProcess(t.Context(), socketPath)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("FindDaemonProcess(%q) err = %v, want %v", tc.cmdline, err, tc.wantErr)
			}
			continue
		}
		if err != nil || proc.PID != pid || proc.Foreground != tc.foreground {
			t.Fatalf("FindDaemonProcess(%q) = pid %d foreground %v err %v; want pid %d foreground %v",
				tc.cmdline, proc.PID, proc.Foreground, err, pid, tc.foreground)
		}
	}
}

// TestFindDaemonProcessRefusesWhatItCannotVerify covers the v3.25.4 review's
// finding 5: when the pidfile holder cannot be inspected (BusyBox ps had no
// -p) the refusal must name the cause and say nothing was signalled, and an
// emulator or wrapper command must still be refused rather than matched as a
// substring.
func TestFindDaemonProcessRefusesWhatItCannotVerify(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, productidentity.DaemonSocketName)
	holder := exec.Command("sleep", "30")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Signal(syscall.SIGKILL)
		_, _ = holder.Process.Wait()
	})
	pid := holder.Process.Pid
	if err := os.WriteFile(dial.LockPath(socketPath), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	savedLookup := lookupProcessCommandLine
	t.Cleanup(func() { lookupProcessCommandLine = savedLookup })

	const bin = "/tmp/h/.local/bin/canary"
	for _, tc := range []struct {
		name     string
		cmdline  string
		lookup   error
		wantText string
	}{
		{name: "inspection failed", lookup: errors.New("ps -p 1 -o args=: exit status 1: ps: unrecognized option: p"), wantText: "ps: unrecognized option: p; no signal was sent"},
		{name: "procfs missing", lookup: errors.New("process inspection on Linux needs procfs mounted at /proc"), wantText: "needs procfs mounted"},
		{name: "exited during check", lookup: fmt.Errorf("/proc/1: %w", procinfo.ErrNoProcess), wantText: "exited while it was being verified"},
		{name: "unrelated process", cmdline: "sleep 600", wantText: "is not a Canary daemon"},
		{name: "unstripped emulator", cmdline: "/usr/bin/qemu-x86_64 " + bin + " daemon", wantText: "is not a Canary daemon"},
		{name: "unstripped emulator with argv0", cmdline: "/usr/bin/qemu-x86_64 " + bin + " canary daemon", wantText: "is not a Canary daemon"},
		{name: "shell wrapper", cmdline: "/bin/sh -c " + bin + " daemon", wantText: "is not a Canary daemon"},
		{name: "substring only", cmdline: "tail -f " + bin + " daemon.log", wantText: "is not a Canary daemon"},
	} {
		lookupProcessCommandLine = func(context.Context, int) (string, error) {
			return tc.cmdline, tc.lookup
		}
		proc, err := FindDaemonProcess(t.Context(), socketPath)
		if !errors.Is(err, ErrDaemonUnverified) || !strings.Contains(err.Error(), tc.wantText) || proc.PID != 0 {
			t.Fatalf("%s: FindDaemonProcess = pid %d err %v; want ErrDaemonUnverified containing %q", tc.name, proc.PID, err, tc.wantText)
		}
	}
}

// A stop names its requester in the daemon log before the signal, and never
// creates a log that is not there.
func TestStopDaemonNamesTheRequesterInTheDaemonLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "ibkr-daemon.log")
	t.Setenv("CANARY_LOG", logPath)
	stop := func() int {
		t.Helper()
		child := exec.Command("sleep", "30")
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		if err := StopDaemon(child.Process.Pid, 5*time.Second); err != nil {
			_ = child.Process.Kill()
			t.Fatal(err)
		}
		_ = child.Wait()
		return child.Process.Pid
	}

	stop()
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("stop created a daemon log: %v", err)
	}

	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	pid := stop()
	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("daemon pid %d: SIGTERM requested by pid %d (", pid, os.Getpid())
	if !strings.Contains(string(got), want) || !strings.Contains(string(got), "level=INFO") {
		t.Fatalf("daemon log = %q, want a line containing %q", got, want)
	}
}
