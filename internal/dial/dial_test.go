package dial

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestSpawnDaemonFromExecutableReapsShortLivedChild(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "exit-immediately")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANARY_LOG", filepath.Join(dir, "daemon.log"))

	pid, err := spawnDaemonFromExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("short-lived daemon child %d was not reaped", pid)
}

func TestAutospawnExactExecutableHonorsCallerStartupTimeout(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "slow-daemon")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nsleep 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANARY_LOG", filepath.Join(dir, "daemon.log"))

	timeout := 150 * time.Millisecond
	startedAt := time.Now()
	_, err := AutospawnAndConnectContextFromExecutableWithTimeout(context.Background(), shortSocketPath(t), executable, timeout)
	elapsed := time.Since(startedAt)
	if err == nil {
		t.Fatal("slow daemon unexpectedly opened a socket")
	}
	if elapsed < timeout || elapsed > time.Second {
		t.Fatalf("exact startup wait = %s, want caller budget near %s rather than ordinary %s", elapsed, timeout, AutospawnTimeout)
	}
}

// shortSocketPath returns a socket path under /tmp so it stays under the
// 104-char Unix-socket path limit macOS enforces (TempDir paths under
// /var/folders/ exceed this).
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ibkr-dial-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// seedOrphanSocket leaves a Unix-socket inode at path with no listener
// behind it — the state a crashed daemon leaves on disk. macOS sometimes
// keeps the listener half-alive briefly after Close (Dial returns
// success for a tiny window before the kernel tears down the socket
// state), which used to flake the orphan-socket dial tests. We poll
// until Dial fails so the test starts in the known orphan state.
func seedOrphanSocket(t *testing.T, path string) {
	t.Helper()
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("unix", path, 50*time.Millisecond)
		if err != nil {
			return
		}
		_ = c.Close()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("orphan socket at %s still accepting connections after 500ms", path)
}

// An orphaned socket file (file exists but no listener) must surface as
// ErrSocketMissing so cmd/canary's autospawn and WaitForSocket's retry both
// trigger. Pre-fix, ECONNREFUSED leaked through and broke autospawn after
// any unclean daemon exit.
func TestConnectOrphanSocketReportsMissing(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	seedOrphanSocket(t, path)

	if _, err := Connect(path); !errors.Is(err, ErrSocketMissing) {
		t.Fatalf("Connect over orphan socket: got %v, want ErrSocketMissing", err)
	}
}

// silentSocket starts a Unix-socket listener that accepts the first
// connection and then sits silently — never replies, never closes. Used to
// simulate a wedged daemon for the ctx-cancellation test. The returned
// stop func closes both the listener and any held connection so the test
// can reclaim the port quickly.
func silentSocket(t *testing.T) (path string, stop func()) {
	t.Helper()
	path = shortSocketPath(t)
	addr, err := net.ResolveUnixAddr("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", addr)
	if err != nil {
		t.Fatal(err)
	}

	var (
		held net.Conn
		mu   sync.Mutex
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = c
			mu.Unlock()
		}
	}()

	return path, func() {
		_ = ln.Close()
		mu.Lock()
		if held != nil {
			_ = held.Close()
		}
		mu.Unlock()
	}
}

// waitForSocketOrPIDDeath returns (nil, false) promptly when the watched
// PID exits during the wait — this is the shutdown-race signal the
// autospawn pre-check uses to fall through to spawning a fresh daemon
// instead of misreporting a graceful shutdown as a stuck daemon.
//
// Reproduces the user-reported bug: idle-shutdown removes the socket
// before releasing the lock, so a CLI invocation arriving mid-shutdown
// sees PID alive + lock present + socket gone. Without PID-death
// detection, the pre-check times out and prints a misleading "PID is
// stuck" error.
func TestWaitForSocketOrPIDDeathFallsThroughOnPIDExit(t *testing.T) {
	t.Parallel()
	// Run a short-lived child process that exits ~150ms in. The wait
	// budget is generous (1s) so the exit lands well inside the window.
	cmd := exec.Command("sleep", "0.15")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	// Reap concurrently so the OS releases the child PID promptly when
	// it exits. IsProcessAlive treats zombie status as dead, but this
	// still keeps the test process table clean.
	go func() { _ = cmd.Wait() }()
	pid := cmd.Process.Pid

	// Socket path that will never appear — we want the wait to be
	// decided by PID death, not by a successful Connect.
	missing := shortSocketPath(t)
	start := time.Now()
	conn, ok := waitForSocketOrPIDDeath(context.Background(), missing, pid, 1*time.Second)
	elapsed := time.Since(start)

	if ok || conn != nil {
		t.Fatalf("expected (nil, false) on PID death; got conn=%v ok=%v", conn, ok)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("returned after %s — should bail within a few poll intervals of PID exit (~150ms+slack)", elapsed)
	}
	if IsProcessAlive(pid) {
		t.Errorf("test invariant broken: PID %d still alive at end of wait", pid)
	}
}

func TestAutospawnAndConnectContextHonorsCanceledContextBeforeSpawn(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := AutospawnAndConnectContext(ctx, shortSocketPath(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AutospawnAndConnectContext with canceled ctx = %v, want context.Canceled", err)
	}
}

// DaemonVersion runs one status.health call and returns the daemon_version
// field. A stub socket emulates the daemon-side encoder by reading a single
// request envelope and writing a canned response keyed on the request id.
func TestDaemonVersion(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)

	// Stub daemon: accept one connection, read the request line, reply
	// with a status.health-shaped response carrying our fake version.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		// Extract the id field with a coarse substring scan — the only
		// thing we need is to echo it back. Real daemons parse properly.
		req := string(buf[:n])
		id := "unknown"
		if _, after, ok := strings.Cut(req, `"id":"`); ok {
			rest := after
			if j := strings.Index(rest, `"`); j > 0 {
				id = rest[:j]
			}
		}
		_, _ = c.Write([]byte(`{"id":"` + id + `","ok":true,"result":{"daemon_version":"v0.9.1","connected":true}}` + "\n"))
	}()

	conn, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := conn.DaemonVersion(ctx)
	if err != nil {
		t.Fatalf("DaemonVersion: %v", err)
	}
	if got != "v0.9.1" {
		t.Errorf("DaemonVersion = %q, want v0.9.1", got)
	}
}

// TestCallHonorsContextCancellation verifies that ctx cancellation aborts a
// Call blocked on read within the SIGINT-response budget (~150ms). Pre-fix,
// only the ctx deadline was wired into the socket, so cancellation without
// a deadline left the CLI hung until the daemon eventually replied (or the
// per-invocation deadline in cmd/canary fired 60s later).
func TestCallHonorsContextCancellation(t *testing.T) {
	t.Parallel()
	path, stop := silentSocket(t)
	defer stop()

	conn, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err = conn.Call(ctx, "fake.method", nil, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call returned %v, want context.Canceled", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Call returned after %s — ctx cancellation not honored promptly", elapsed)
	}
}

// TestCallDeadlineDoesNotLeakIntoStream protects against the v0.9.1+
// regression where the tight-timeout version-skew Call left its socket
// deadline armed, killing the subsequent quote --watch Stream after ~1s.
// The shape we want: Call's deadline fires only against Call's own read,
// not against any later operation on the same Conn.
func TestCallDeadlineDoesNotLeakIntoStream(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)

	// Stub daemon: respond instantly to one Call, then accept a Stream
	// subscribe and emit ONE frame after a 600ms pause. Pre-fix, the
	// stale 250ms deadline from the prior Call would fire before the
	// frame ever arrived, and the Stream would return early with no
	// frames seen.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()

		buf := make([]byte, 4096)
		readReq := func() string {
			n, _ := c.Read(buf)
			req := string(buf[:n])
			if _, after, ok := strings.Cut(req, `"id":"`); ok {
				rest := after
				if j := strings.Index(rest, `"`); j > 0 {
					return rest[:j]
				}
			}
			return ""
		}

		// 1. unary Call — reply instantly so Call's tight deadline
		//    never fires of its own accord.
		id := readReq()
		_, _ = c.Write([]byte(`{"id":"` + id + `","ok":true,"result":{}}` + "\n"))

		// 2. Stream subscribe — emit a frame after a deliberate pause
		//    longer than the prior Call's deadline (250ms vs 600ms).
		id = readReq()
		time.Sleep(600 * time.Millisecond)
		_, _ = c.Write([]byte(`{"id":"` + id + `","ok":true,"stream":true,"frame":{"t":"2026-05-11T20:00:00Z"}}` + "\n"))
		// Then close to end the stream so the test can finish.
	}()

	conn, err := Connect(path)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Close()

	// Tight-deadline Call — mirrors the version-skew check in cmd/canary.
	callCtx, callCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer callCancel()
	if err := conn.Call(callCtx, "status.health", nil, nil); err != nil {
		t.Fatalf("Call returned %v, want nil", err)
	}

	// Stream with NO deadline — the long-lived watch case. Must outlive
	// the prior Call's 250ms deadline.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()

	frames := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Stream(streamCtx, "quote.subscribe", nil, func(raw json.RawMessage) error {
			select {
			case frames <- struct{}{}:
			default:
			}
			return nil
		})
	}()

	select {
	case <-frames:
		// Got a frame. Tear down the stream so the test ends cleanly.
		streamCancel()
		<-errCh
	case <-time.After(2 * time.Second):
		t.Fatalf("Stream returned no frames within 2s — stale deadline from prior Call leaked into Stream")
	}
}

// WaitForSocket retries on ErrSocketMissing — including the orphan-socket
// case mapped above — and gives up with a "did not appear" error after the
// deadline.
func TestWaitForSocketRetriesOnOrphanThenTimesOut(t *testing.T) {
	t.Parallel()
	path := shortSocketPath(t)
	seedOrphanSocket(t, path)

	start := time.Now()
	_, err := WaitForSocket(path, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("WaitForSocket gave up after %s — did not retry", elapsed)
	}
}
