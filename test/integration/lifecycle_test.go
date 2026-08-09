package integration

import (
	"errors"

	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
)

func lifecycleEnv(t *testing.T) (env []string, socketPath, logPath string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "canary-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	socketPath = filepath.Join(dir, "ibkr.sock")
	logPath = filepath.Join(dir, "ibkr-daemon.log")
	configPath := filepath.Join(dir, "config.toml")

	configData := []byte("[gateway]\nhost = \"127.0.0.1\"\nport = 1\nclient_id = 199\ntls = false\n")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	env = append(os.Environ(),
		"CANARY_SOCKET="+socketPath,
		"CANARY_LOG="+logPath,
		"CANARY_CONFIG="+configPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
	)
	t.Cleanup(func() {
		if pid := dial.LockHolderPID(dial.LockPath(socketPath)); pid > 0 {
			killDaemonTree(pid)
		}
		_ = os.RemoveAll(dir)
		reapLeakedTestApps(t)
	})
	return env, socketPath, logPath
}

func reapLeakedTestApps(t *testing.T) {
	t.Helper()
	pattern := leakedTestAppPattern(sharedCLI)
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return
	}
	for pidText := range strings.FieldsSeq(string(out)) {
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		t.Errorf("test binary leaked a `canary app` process (pid %d) — restart's app management escaped the CANARY_SOCKET scope; killing it", pid)
		killDaemonTree(pid)
	}
}

func leakedTestAppPattern(cliPath string) string {
	return regexp.QuoteMeta(cliPath) + " [a]pp"
}

func TestLifecycle_IntegrationModeIsExplicitAndFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		raw  string
		want integrationTestMode
	}{
		{raw: "", want: integrationModeOptional},
		{raw: " optional ", want: integrationModeOptional},
		{raw: "HERMETIC", want: integrationModeHermetic},
		{raw: "live", want: integrationModeLive},
	} {
		got, err := parseIntegrationTestMode(tc.raw)
		if err != nil {
			t.Fatalf("parseIntegrationTestMode(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("parseIntegrationTestMode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if _, err := parseIntegrationTestMode("skip-live"); err == nil {
		t.Fatal("unknown integration mode was accepted")
	}
}

func killDaemonTree(pid int) {
	signalGroup := func(sig syscall.Signal) {
		if err := syscall.Kill(-pid, sig); err != nil {
			_ = syscall.Kill(pid, sig)
		}
	}
	signalGroup(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && dial.IsProcessAlive(pid) {
		time.Sleep(50 * time.Millisecond)
	}
	if dial.IsProcessAlive(pid) {
		signalGroup(syscall.SIGKILL)
	}
}

func runCLI(t *testing.T, env []string, timeout time.Duration, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(sharedCLI, args...)
	cmd.Env = env
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start cli: %v", err)
	}
	done := make(chan error, 1)
	var out, errOut []byte
	go func() {
		out, _ = io.ReadAll(stdout)
		errOut, _ = io.ReadAll(stderr)
		done <- cmd.Wait()
	}()
	select {
	case waitErr := <-done:
		combined := string(out) + string(errOut)
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				return combined, exitErr.ExitCode()
			}
			t.Fatalf("cli wait: %v\n%s", waitErr, combined)
		}
		return combined, 0
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("cli %v hung past %s\nstdout/stderr so far:\n%s%s", args, timeout, string(out), string(errOut))
		return "", -1
	}
}

func daemonPID(socketPath string) int {
	return dial.LockHolderPID(dial.LockPath(socketPath))
}

func waitForDaemonExit(socketPath string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pid := daemonPID(socketPath)
		if pid == 0 || !dial.IsProcessAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestLifecycle_CleanCycle(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	out, code := runCLI(t, env, 30*time.Second, "status", "--json")
	if code != 0 {
		t.Fatalf("status --json exit=%d, want 0 (CLI couldn't reach autospawned daemon)\n%s", code, out)
	}
	if !strings.Contains(out, "daemon_version") {
		t.Fatalf("status --json output missing daemon_version field:\n%s", out)
	}

	pid1 := daemonPID(socketPath)
	if pid1 == 0 || !dial.IsProcessAlive(pid1) {
		t.Fatalf("daemon not alive after autospawn (pid=%d)", pid1)
	}

	_, code = runCLI(t, env, 5*time.Second, "status", "--json")
	if code != 0 {
		t.Fatalf("second status exit=%d", code)
	}
	if pid2 := daemonPID(socketPath); pid2 != pid1 {
		t.Fatalf("second invocation spawned new daemon: pid1=%d pid2=%d", pid1, pid2)
	}

	proc, err := os.FindProcess(pid1)
	if err != nil {
		t.Fatalf("find process %d: %v", pid1, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}
	if !waitForDaemonExit(socketPath, 5*time.Second) {
		t.Fatalf("daemon %d did not exit within 5s of SIGTERM", pid1)
	}

	if _, err := os.Stat(dial.LockPath(socketPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file should be removed; stat err=%v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file should be removed; stat err=%v", err)
	}
}

func TestLifecycle_CLIDoesNotHangOnDeafDaemon(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "canary-lifecycle-cli-deaf-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socketPath := filepath.Join(dir, "ibkr.sock")
	logPath := filepath.Join(dir, "ibkr-daemon.log")
	lockPath := dial.LockPath(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		var holds []net.Conn
		defer func() {
			for _, c := range holds {
				_ = c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			holds = append(holds, c)
			select {
			case <-stop:
				return
			default:
			}
		}
	}()

	_ = os.WriteFile(lockPath, []byte("99999\n"), 0o600)

	env := append(os.Environ(),
		"CANARY_SOCKET="+socketPath,
		"CANARY_LOG="+logPath,
	)

	start := time.Now()
	out, code := runCLI(t, env, 10*time.Second, "status")
	elapsed := time.Since(start)

	if code == 0 {
		t.Fatalf("status against deaf daemon should fail, got exit=0\n%s", out)
	}
	if elapsed > integrationCLIUnaryTimeout+2*time.Second {
		t.Fatalf("CLI took %s — the %s per-call deadline appears to not be applied", elapsed, integrationCLIUnaryTimeout)
	}
	if elapsed < integrationCLIUnaryTimeout/2 {
		t.Logf("note: CLI exited in %s (expected around %s) — deadline may have been shorter than intended", elapsed, integrationCLIUnaryTimeout)
	}
}
