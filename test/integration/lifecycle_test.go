package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Lifecycle tests exercise the CLI's daemon-management surface, including
// delayed socket appearance. They don't need a live gateway: the daemon stays
// up in degraded mode when its handshake fails, which is exactly the state
// these tests drive against.
//
// Each test gets its own socket/log and complete XDG persistence roots via env
// vars so it can run in parallel without sharing daemon.db, recovery artifacts,
// caches, or user data with the developer's daemon or another test.

// lifecycleEnv returns the env slice + paths a CLI invocation should use
// to keep its daemon isolated from the rest of the system. The directory
// lives under /tmp to stay inside macOS's 104-char Unix-socket path limit.
//
// Teardown is registered here via t.Cleanup — not returned for the caller
// to defer — so it runs on Fatal, Error, and in-test panic alike, and no
// test can forget it: kill whichever daemon holds the lock at teardown
// time (tests kill and respawn daemons mid-flight; the lock always names
// the latest). Exits where no Go code runs at all (go test -timeout panic,
// SIGKILL of the test binary) are covered by the reaper in TestMain.
func lifecycleEnv(t *testing.T) (env []string, socketPath, logPath string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "canary-lifecycle-")
	if err != nil {
		t.Fatal(err)
	}
	socketPath = filepath.Join(dir, "ibkr.sock")
	logPath = filepath.Join(dir, "ibkr-daemon.log")
	env = append(os.Environ(),
		"CANARY_SOCKET="+socketPath,
		"CANARY_LOG="+logPath,
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

// reapLeakedTestApps fails the test if any `canary app` process spawned from
// this run's test binary is still alive, and kills it. No lifecycle test
// may legitimately spawn an app: lifecycleEnv sets CANARY_SOCKET, which makes
// plain `canary restart` skip app management entirely — a surviving app means
// that scoping regressed (the bug that once SIGTERMed the developer's real
// `canary app --remote` and leaked a test-binary replacement on :8765).
// Because this runs in every parallel test's cleanup against one global
// pattern, it stays a pure tripwire only as long as that invariant holds.
func reapLeakedTestApps(t *testing.T) {
	t.Helper()
	pattern := leakedTestAppPattern(sharedCLI)
	out, err := exec.Command("pgrep", "-f", pattern).Output()
	if err != nil {
		return // pgrep exits 1 when nothing matches
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

// leakedTestAppPattern uses the conventional self-excluding character class:
// it matches a real "<sharedCLI> app" argv, but the literal "[a]pp" in pgrep's
// own argv does not match itself. Without this, parallel lifecycle cleanups can
// match each other's pgrep processes and report two false app leaks even though
// restart correctly skipped app management for the overridden socket.
func leakedTestAppPattern(cliPath string) string {
	return regexp.QuoteMeta(cliPath) + " [a]pp"
}

func TestLifecycle_LeakedAppPatternExcludesParallelPgrep(t *testing.T) {
	cliPath := "/tmp/canary-integration/bin/canary"
	pattern := leakedTestAppPattern(cliPath)
	re := regexp.MustCompile(pattern)

	if !re.MatchString(cliPath + " app --remote") {
		t.Fatal("leak tripwire no longer matches a real Canary app process")
	}
	if re.MatchString("pgrep -f " + pattern) {
		t.Fatal("leak tripwire matches its own pgrep argv")
	}
}

// killDaemonTree terminates a daemon and anything it spawned. Autospawned
// daemons run under Setsid (internal/dial/autospawn.go), making the daemon
// its own process-group leader, so kill(-pid) reaches the whole group;
// fall back to the bare PID for a daemon that isn't a group leader.
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

// runCLI invokes the built CLI with the given args under the lifecycle env.
// Returns stdout+stderr concatenated, the exit code, and a deadline-fenced
// error so a wedged CLI doesn't hang the suite.
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

// daemonPID returns the lock holder's PID, or 0 if no daemon is running.
func daemonPID(socketPath string) int {
	return dial.LockHolderPID(dial.LockPath(socketPath))
}

// waitForDaemonExit polls until either the lock holder is gone or the
// deadline passes. Returns true if the daemon exited cleanly.
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

// TestLifecycle_CleanCycle covers the happy path: cold autospawn → daemon
// alive → second invocation reuses the running daemon → SIGTERM → daemon
// exits cleanly with no orphaned files.
//
// Lifecycle tests use `status --json` as the spawn-detection probe: --json
// exits 0 whenever the CLI talked to the daemon (regardless of gateway
// state), whereas non-JSON status exits 1 on a degraded gateway. We only
// care that the daemon process is up; gateway reachability is tested
// elsewhere.
func TestLifecycle_CleanCycle(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	// Cold autospawn — should succeed and leave the daemon running.
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

	// Second invocation should reuse the same daemon (same PID).
	_, code = runCLI(t, env, 5*time.Second, "status", "--json")
	if code != 0 {
		t.Fatalf("second status exit=%d", code)
	}
	if pid2 := daemonPID(socketPath); pid2 != pid1 {
		t.Fatalf("second invocation spawned new daemon: pid1=%d pid2=%d", pid1, pid2)
	}

	// SIGTERM → daemon exits cleanly within a few seconds.
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

	// Lock + socket files should both be gone after clean shutdown.
	if _, err := os.Stat(dial.LockPath(socketPath)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock file should be removed; stat err=%v", err)
	}
	if _, err := os.Stat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket file should be removed; stat err=%v", err)
	}
}

// TestLifecycle_KillThenReinvoke is the user's exact scenario: bring up the
// CLI (which starts a daemon), kill the daemon mid-flight, then reinvoke
// the CLI. The second invocation must autospawn a fresh daemon — not
// surface the cryptic "socket did not appear" error.
func TestLifecycle_KillThenReinvoke(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	// First invocation autospawns the daemon. Use --json so the exit code
	// reflects "CLI reached the daemon," not "gateway is connected" — the
	// test environment may not have a live gateway.
	if _, code := runCLI(t, env, 30*time.Second, "status", "--json"); code != 0 {
		t.Fatalf("first status exit=%d", code)
	}
	pid1 := daemonPID(socketPath)
	if pid1 == 0 {
		t.Fatal("daemon PID is 0 after first status")
	}

	// SIGKILL the daemon — abrupt termination, no defers run, socket
	// file may or may not be unlinked depending on kernel timing.
	proc, err := os.FindProcess(pid1)
	if err != nil {
		t.Fatalf("find process: %v", err)
	}
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("sigkill: %v", err)
	}
	if !waitForDaemonExit(socketPath, 5*time.Second) {
		t.Fatalf("daemon didn't die after SIGKILL")
	}

	// The kernel cleans up the listening socket on process death, so
	// dial.Connect will see it as missing and the CLI's autospawn fires.
	// --json: only autospawn-recovery is under test; gateway state isn't.
	out, code := runCLI(t, env, 30*time.Second, "status", "--json")
	if code != 0 {
		t.Fatalf("post-kill status exit=%d, want 0 (autospawn should recover)\n%s", code, out)
	}
	pid2 := daemonPID(socketPath)
	if pid2 == 0 {
		t.Fatal("daemon PID is 0 after recovery autospawn")
	}
	if pid2 == pid1 {
		t.Fatalf("recovery PID matches killed PID: %d (impossible — was kill applied?)", pid2)
	}
	if !dial.IsProcessAlive(pid2) {
		t.Fatalf("recovery daemon %d not alive", pid2)
	}
}

func TestLifecycle_RestartStartsWhenAbsent(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	out, code := runCLI(t, env, 30*time.Second, "restart", "--json")
	if code != 0 {
		t.Fatalf("restart --json exit=%d, want 0\n%s", code, out)
	}
	var res struct {
		Action     string `json:"action"`
		WasRunning bool   `json:"was_running"`
		Started    bool   `json:"started"`
		NewPID     int    `json:"new_pid"`
		App        *struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"app"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode restart json: %v\n%s", err, out)
	}
	if res.Action != "started" || res.WasRunning || !res.Started || res.NewPID <= 0 {
		t.Fatalf("restart result = %+v", res)
	}
	if pid := daemonPID(socketPath); pid != res.NewPID {
		t.Fatalf("lock PID = %d, restart new_pid=%d", pid, res.NewPID)
	}
	// CANARY_SOCKET is set by lifecycleEnv, so restart must not have touched
	// any app process — least of all the developer's real one.
	if res.App == nil || res.App.Action != "skipped" || res.App.Reason != "socket_overridden" {
		t.Fatalf("app result = %+v, want skipped/socket_overridden", res.App)
	}
}

func TestLifecycle_RestartReplacesRunningDaemon(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	if _, code := runCLI(t, env, 30*time.Second, "status", "--json"); code != 0 {
		t.Fatalf("seed status exit=%d", code)
	}
	oldPID := daemonPID(socketPath)
	if oldPID == 0 {
		t.Fatal("old daemon PID is 0")
	}

	out, code := runCLI(t, env, 30*time.Second, "restart", "--json")
	if code != 0 {
		t.Fatalf("restart --json exit=%d, want 0\n%s", code, out)
	}
	var res struct {
		Action     string `json:"action"`
		WasRunning bool   `json:"was_running"`
		Graceful   bool   `json:"graceful"`
		OldPID     int    `json:"old_pid"`
		NewPID     int    `json:"new_pid"`
		App        *struct {
			Action string `json:"action"`
			Reason string `json:"reason"`
		} `json:"app"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode restart json: %v\n%s", err, out)
	}
	if res.Action != "restarted" || !res.WasRunning || !res.Graceful || res.OldPID != oldPID || res.NewPID <= 0 {
		t.Fatalf("restart result = %+v, oldPID=%d", res, oldPID)
	}
	if res.App == nil || res.App.Action != "skipped" || res.App.Reason != "socket_overridden" {
		t.Fatalf("app result = %+v, want skipped/socket_overridden", res.App)
	}
	if res.NewPID == oldPID {
		t.Fatalf("restart kept same PID: %d", oldPID)
	}
	if !dial.IsProcessAlive(res.NewPID) {
		t.Fatalf("new daemon %d is not alive", res.NewPID)
	}
}

// TestLifecycle_StuckDaemonProducesActionableError simulates the legacy
// zombie scenario: a daemon process holds the lock but has no socket open.
// The CLI's autospawn must fail with the actionable error (PID + kill
// command + log tail), not the bare timeout message.
func TestLifecycle_StuckDaemonProducesActionableError(t *testing.T) {
	t.Parallel()
	env, socketPath, _ := lifecycleEnv(t)

	// First spawn a real daemon so it acquires the lock cleanly. --json
	// so we don't trip the new "exit 1 on degraded gateway" behavior in
	// status — we only need the daemon process up here.
	if _, code := runCLI(t, env, 30*time.Second, "status", "--json"); code != 0 {
		t.Fatal("seed status failed")
	}
	pid := daemonPID(socketPath)
	if pid == 0 {
		t.Fatal("seed daemon PID is 0")
	}

	// SIGSTOP freezes the process — it keeps the flock but stops responding
	// to anything. Then unlink the socket file: now the CLI sees no socket
	// even though the lock holder is alive. This is the exact zombie state
	// the pre-fix idle-shutdown bug created in production.
	proc, _ := os.FindProcess(pid)
	if err := proc.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("sigstop: %v", err)
	}
	defer func() { _ = proc.Signal(syscall.SIGCONT); _ = proc.Signal(syscall.SIGKILL) }()

	if err := os.Remove(socketPath); err != nil {
		t.Fatalf("remove socket: %v", err)
	}

	// Run a CLI command — autospawn should detect the stuck daemon and
	// surface the actionable error. The 10s test timeout covers the 5s
	// autospawnTimeout plus a safety margin for the diagnostic plumbing.
	out, code := runCLI(t, env, 10*time.Second, "status")
	if code == 0 {
		t.Fatalf("status should have failed against stuck daemon, got exit=0\n%s", out)
	}

	expectedFragments := []string{
		fmt.Sprintf("PID %d", pid),
		"never opened the socket",
		fmt.Sprintf("kill %d", pid),
	}
	for _, frag := range expectedFragments {
		if !strings.Contains(out, frag) {
			t.Errorf("error output missing %q\nfull output:\n%s", frag, out)
		}
	}
}

// TestLifecycle_CLIDoesNotHangOnDeafDaemon is the end-to-end version of
// the deaf-socket test below: spawn a stub listener at the canonical
// socket path, run the real `canary status` binary against it, and verify
// the CLI exits within the per-invocation deadline rather than hanging
// until the user hits Ctrl+C. This is the user-facing guarantee the
// per-call timeout in cmd/canary is supposed to provide.
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

	// Stub: accept and hold each connection without ever responding.
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

	// Pre-write a fake lock file so the CLI's autospawn path doesn't
	// fire (dial.Connect succeeds against the live listener anyway).
	_ = os.WriteFile(lockPath, []byte("99999\n"), 0o600)

	env := append(os.Environ(),
		"CANARY_SOCKET="+socketPath,
		"CANARY_LOG="+logPath,
	)
	// TestMain builds the CLI with a shrunken per-call deadline; the
	// production binary keeps the 60s default, but this end-to-end gate
	// only needs to prove that the linked deadline is applied.
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

// TestLifecycle_NonResponsiveSocket exercises what happens when the socket
// is open and accepts connections but nothing reads from it — i.e. a daemon
// that's deadlocked or paused mid-handler. dial.Conn.Call's response read
// has no timeout unless the caller's ctx supplies one; this test pins the
// current behavior so any future change to the deadline strategy is a
// conscious decision rather than an accident.
//
// We seed the scenario with a stub listener that accepts but never replies,
// then use dial.Conn.Call directly (bypassing the CLI's autospawn) so the
// test is hermetic and finishes in milliseconds.
func TestLifecycle_NonResponsiveSocket(t *testing.T) {
	t.Parallel()
	dir, err := os.MkdirTemp("/tmp", "canary-lifecycle-deaf-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socketPath := filepath.Join(dir, "ibkr.sock")

	// Stub: accept and hold the connection open without ever reading or
	// writing. Mirrors a daemon stopped at a kernel-level breakpoint or
	// stuck in a bad cgo call.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold open; do not read or write. The connection's deadline
			// is the only thing that can break us out.
			_ = c
		}
	}()

	conn, err := dial.Connect(socketPath)
	if err != nil {
		t.Fatalf("connect to deaf socket: %v (should succeed — listener is up)", err)
	}
	defer conn.Close()

	// Caller-supplied deadline must be honoured: without it the read would
	// block forever. Pick a short window so the test finishes fast.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = conn.Call(ctx, "status.health", nil, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Call against deaf socket returned nil; expected deadline error")
	}
	if elapsed > 1*time.Second {
		t.Fatalf("Call ignored deadline; took %s (deadline was 200ms)", elapsed)
	}
}
