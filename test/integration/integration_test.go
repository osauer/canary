//go:build !windows

// Package integration runs end-to-end tests of the full `canary daemon` and CLI
// stack against a live IB Gateway. The tests deliberately do not mock or stub
// IBKR; they prove the shipped multi-mode binary connects and talks to the real
// gateway.
//
// Binding Make targets choose one meaning explicitly: hermetic mode selects
// only daemon/CLI lifecycle tests and never probes a Gateway; live mode
// requires a reachable, fully-handshaken Gateway and fails on absence. Direct
// `go test` retains optional-live skips for ordinary Go tooling.
//
// Unix-only: launchSharedDaemon uses Setpgid + kill(-pgid) to ensure the
// spawned daemon never orphans if go test is interrupted. Windows has no
// equivalent in syscall.SysProcAttr; the package was already Unix-only in
// practice (TestMain skips with "Unix-only daemon" on Windows) so the
// build tag just makes that explicit.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
)

var (
	sharedSocket  string
	sharedCLI     string
	sharedStop    func()
	sharedSkipped bool

	// reaperPipe holds the write end of the watchdog pipe for the life of
	// the test process. Never written to — its only job is to be closed by
	// the kernel when this process dies, which is what wakes the reaper.
	// The package-level reference keeps the GC finalizer from closing it
	// early.
	reaperPipe *os.File
)

const (
	integrationCLIUnaryTimeout     = 2 * time.Second
	integrationCLIUnaryTimeoutText = "2s"
	integrationCLILongTimeoutText  = "3s"

	integrationModeEnv = "INTEGRATION_TEST_MODE"
)

type integrationTestMode string

const (
	// integrationModeOptional preserves direct `go test ./...` behavior:
	// lifecycle tests run everywhere and live tests skip when no Gateway is
	// reachable. Binding Make targets choose an explicit mode instead.
	integrationModeOptional integrationTestMode = "optional"
	// integrationModeHermetic runs only the lifecycle inventory selected by
	// the caller and never probes or waits for a live Gateway.
	integrationModeHermetic integrationTestMode = "hermetic"
	// integrationModeLive requires a reachable, fully handshaken Gateway.
	// Absence is a package failure, never a green skip.
	integrationModeLive integrationTestMode = "live"
)

func parseIntegrationTestMode(raw string) (integrationTestMode, error) {
	switch mode := integrationTestMode(strings.ToLower(strings.TrimSpace(raw))); mode {
	case "":
		return integrationModeOptional, nil
	case integrationModeOptional, integrationModeHermetic, integrationModeLive:
		return mode, nil
	default:
		return "", fmt.Errorf("%s must be optional, hermetic, or live (got %q)", integrationModeEnv, raw)
	}
}

// TestMain builds the single canary binary, then either runs the hermetic
// lifecycle inventory or launches one daemon (`canary daemon --foreground`)
// shared by every selected live test. Per-test live daemons are too slow
// (each handshake is multi-second) and risk overwhelming the gateway with
// rapid-fire client-ID changes.
func TestMain(m *testing.M) {
	mode, err := parseIntegrationTestMode(os.Getenv(integrationModeEnv))
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: " + err.Error() + "\n")
		os.Exit(2)
	}

	// Always build the binary first — lifecycle tests (kill/respawn,
	// non-responsive daemon) exercise the CLI's daemon-management path
	// which doesn't need a live gateway.
	cli, err := buildBin()
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: build failed: " + err.Error() + "\n")
		os.Exit(2)
	}
	sharedCLI = cli

	// Last-resort teardown for exits where no Go code runs at all (go
	// test -timeout panic, SIGKILL): a detached reaper kills every daemon
	// spawned from this run's binary the moment this process dies. The
	// in-process paths below (signal handler, stop(), per-test t.Cleanup)
	// remain the prompt, file-removing cleanup; the reaper is the backstop
	// that keeps daemons from accumulating and wedging TWS.
	startReaper(cli)

	if mode == integrationModeHermetic {
		os.Exit(m.Run())
	}

	if !probeGatewayReachable() {
		if mode == integrationModeLive {
			_, _ = os.Stderr.WriteString("integration: live mode requires a reachable IB Gateway\n")
			os.Exit(1)
		}
		sharedSkipped = true
		os.Exit(m.Run())
	}
	socketPath, stop, err := launchSharedDaemon(cli)
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: launch failed (gateway may be in degraded API-mute state — restart it and re-run): " + err.Error() + "\n")
		if mode == integrationModeLive {
			if stop != nil {
				stop()
			}
			os.Exit(1)
		}
		sharedSkipped = true
		if stop != nil {
			stop()
		}
		os.Exit(m.Run())
	}
	sharedSocket = socketPath
	sharedStop = stop

	// The daemon socket appears even when ibkrd's connector ran into the
	// "degraded mode" branch (handshake failed → daemon stays up but
	// disconnected). Ask the daemon whether it actually reached the gateway
	// before declaring the suite live; on a degraded gateway, every test
	// skips cleanly instead of failing with cascading IBKR-unavailable errors.
	if !daemonReachedGateway(socketPath) {
		message := "integration: daemon started but failed to handshake with IB Gateway (likely in degraded API-mute state — restart it and re-run)"
		if mode == integrationModeLive {
			_, _ = os.Stderr.WriteString(message + "\n")
			stop()
			os.Exit(1)
		}
		_, _ = os.Stderr.WriteString(message + "; skipping optional live tests.\n")
		sharedSkipped = true
	}

	// Route SIGINT/SIGTERM through stop() so a Ctrl-C on `go test` (or any
	// signal short of SIGKILL) tears the spawned daemon down rather than
	// orphaning it. SIGKILL skips all of this — that's what the reaper
	// started above is for. The goroutine exits when stop() runs to
	// completion below or when the process dies.
	sigC := make(chan os.Signal, 1)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigC
		if sharedStop != nil {
			sharedStop()
		}
		os.Exit(130)
	}()

	code := m.Run()
	stop()
	os.Exit(code)
}

// daemonReachedGateway polls status.health on the freshly launched daemon
// until it reports connected=true with a non-zero server version. The
// daemon binds its socket before its connector finishes the async gateway
// handshake (observably ~0.7s against paper TWS), so a single-shot check
// here raced the handshake and could mark a healthy suite "no live
// gateway" — every live test skipped and the run still exited 0. The
// 25s budget mirrors the daemon's own handshake wait (`canary restart`).
// A daemon whose connector entered degraded mode stays disconnected
// forever, so a truly muted gateway still skips cleanly — it just pays
// the full budget once.
func daemonReachedGateway(socketPath string) bool {
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if daemonHealthConnected(socketPath) {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return false
}

func daemonHealthConnected(socketPath string) bool {
	conn, err := dial.Connect(socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var res rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		return false
	}
	return res.Connected && res.ServerVersion > 0
}

// startReaper launches a detached watchdog that kills every daemon spawned
// from this run's binary once the test process is gone. The watchdog blocks
// reading a pipe whose write end only this process holds (spawned daemons
// don't inherit it — Go marks non-stdio fds close-on-exec), so it fires on
// ANY exit: clean, t.Fatal, panic, go test -timeout, Ctrl-C, even SIGKILL —
// exactly the paths where t.Cleanup and TestMain teardown never run. It
// lives in its own session so a terminal Ctrl-C doesn't take it down along
// with the suite before it can sweep.
//
// The kill pattern "<binpath> (daemon|app)" matches the shared daemon
// (launchSharedDaemon), anything the lifecycle tests autospawn through
// the CLI, and any `canary app` a regression spawns from the test binary
// (no test does so on purpose — see the leak tripwire in lifecycleEnv),
// and nothing else: the binary path is unique to this run's temp dir. SIGTERM first so daemons unlink their socket/lock files; SIGKILL the
// stragglers (e.g. a SIGSTOPped daemon from the stuck-daemon test) two
// seconds later. The pattern travels via the environment, not argv —
// embedding it in argv would make the shell match its own pkill -f pattern
// and kill itself mid-sweep.
func startReaper(cliBin string) {
	r, w, err := os.Pipe()
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: reaper pipe failed (daemons may outlive an aborted run): " + err.Error() + "\n")
		return
	}
	cmd := exec.Command("/bin/sh", "-c",
		`cat >/dev/null; pkill -TERM -f "$CANARY_REAP_PATTERN"; sleep 2; pkill -KILL -f "$CANARY_REAP_PATTERN"; exit 0`)
	cmd.Env = append(os.Environ(), "CANARY_REAP_PATTERN="+regexp.QuoteMeta(cliBin)+" (daemon|app)")
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		_, _ = os.Stderr.WriteString("integration: reaper start failed (daemons may outlive an aborted run): " + err.Error() + "\n")
		return
	}
	_ = r.Close()
	reaperPipe = w
	go func() { _ = cmd.Process.Release() }()
}

func probeGatewayReachable() bool {
	host := "127.0.0.1"
	port := 4001
	if v := os.Getenv("IBKR_TEST_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func skipIfNoGateway(t *testing.T) {
	t.Helper()
	if sharedSkipped {
		t.Skip("IB Gateway not reachable; skipping live integration test")
	}
}

func buildBin() (string, error) {
	dir, err := os.MkdirTemp("", "canary-integration-")
	if err != nil {
		return "", err
	}
	out := filepath.Join(dir, "canary")
	ldflags := fmt.Sprintf("-X main.cliUnaryTimeout=%s -X main.cliLongUnaryTimeout=%s", integrationCLIUnaryTimeoutText, integrationCLILongTimeoutText)
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", out, "../../cmd/canary")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out, nil
}

func launchSharedDaemon(cliBin string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "canary-integration-run-")
	if err != nil {
		return "", nil, err
	}
	socketPath := filepath.Join(dir, "ibkr.sock")
	logPath := filepath.Join(dir, "ibkr-daemon.log")
	cfgPath := filepath.Join(dir, "config.toml")
	cid := nextClientID()
	port := 4001
	if v := os.Getenv("IBKR_TEST_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	// Pin every dimension explicitly so the test harness doesn't depend
	// on AUTO discovery — a deterministic config matches the historical
	// behavior the integration tests were written for, and doesn't probe
	// extra ports on the live gateway during a test run.
	cfg := "[gateway]\nhost = \"127.0.0.1\"\nport = " +
		strconv.Itoa(port) + "\nclient_id = " + strconv.Itoa(cid) + "\ntls = false\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		return "", nil, err
	}
	cmd := exec.Command(cliBin, "daemon",
		"--config", cfgPath,
		"--socket", socketPath,
		"--foreground",
		"--log", logPath,
	)
	// A private socket is not a complete daemon isolation boundary: the sole
	// authority and its persistence lock are XDG-state scoped. Keep the live
	// integration daemon away from the installed daemon and from lifecycle
	// tests, and prevent any test run from cutting over the developer's state.
	cmd.Env = append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
	)
	// Place the daemon in its own process group so stop() can signal the
	// whole group via kill(-pid). Without this, a daemon that ever spawned
	// helpers (or any future grandchild) would survive a test panic that
	// skipped stop(). macOS does not propagate parent death to children, so
	// the group-signal is the only reliable cleanup path.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	pgid := cmd.Process.Pid
	stop := func() {
		_ = syscall.Kill(-pgid, syscall.SIGINT)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = os.RemoveAll(dir)
	}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			return socketPath, stop, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	stop()
	return "", nil, fmt.Errorf("daemon socket did not appear within 25s; see %s", logPath)
}

// nextClientID generates a unique client ID per daemon process so the IBKR
// gateway doesn't reject overlapping handshakes (one connection per ID).
//
// The counter base is PID-bucketed: a fixed base meant two concurrent
// suite runs (parallel dev sessions) both claimed 20, 21, … against the
// shared gateway and flaked with TWS error 326. Eight 19-wide windows
// spanning 20-96 and 105-181 keep clear of the default daemon client ID
// 15, regime's 100-104 reservation, and the smoke scripts' 200+ range.
// scripts/with-gateway-lock.sh serializes whole runs across sessions;
// this is defense in depth for anything not routed through make.
var clientIDCounter = func() int32 {
	buckets := []int32{20, 39, 58, 77, 105, 124, 143, 162}
	return buckets[os.Getpid()%len(buckets)]
}()

func nextClientID() int { return int(atomic.AddInt32(&clientIDCounter, 1)) }

func client(t *testing.T) *dial.Conn {
	t.Helper()
	conn, err := dial.Connect(sharedSocket)
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestStatusReportsConnected(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var res rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, nil, &res); err != nil {
		t.Fatalf("status.health: %v", err)
	}
	if !res.Connected {
		t.Fatalf("expected daemon to report connected, got %+v", res)
	}
	if res.ServerVersion == 0 {
		t.Errorf("expected non-zero server version, got %d", res.ServerVersion)
	}
}

func TestAccountSummaryReturnsLiveData(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	var res rpc.AccountResult
	if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
		t.Fatalf("account.summary: %v", err)
	}
	if res.AccountID == "" {
		t.Fatalf("account_id missing from response: %+v", res)
	}
	if res.NetLiquidation == 0 {
		t.Errorf("net_liquidation reported as zero (suspicious): %+v", res)
	}
}

func TestPositionsReturnLiveMarks(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// First call may race the streaming portfolio update; retry briefly until
	// at least one position has a non-zero mark.
	var res rpc.PositionsResult
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.Call(ctx, rpc.MethodPositionsList, nil, &res); err != nil {
			t.Fatalf("positions.list: %v", err)
		}
		if positionsHaveMarks(res.Stocks) || positionsHaveMarks(res.Options) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(res.Stocks)+len(res.Options) == 0 {
		t.Skip("paper account has no open positions to verify marks against")
	}
	t.Errorf("no position carried a non-zero mark within 10s: %+v", res)
}

func positionsHaveMarks(rows []rpc.PositionView) bool {
	for _, p := range rows {
		if p.Mark != 0 {
			return true
		}
	}
	return false
}

// TestDailyPnLEventuallyPopulated waits for the daemon's reqPnL stream to
// emit its first frame. The subscription is kicked from post-connect
// setup; the gateway typically returns the first frame within a couple
// of seconds. Skips cleanly when no gateway is reachable.
//
// We assert account-level Daily P&L lands on the wire as non-nil — the
// actual value can be zero (flat day) and that's fine. The "no zero
// substitution" contract is enforced upstream; this test pins the
// streaming arrival, not the magnitude.
func TestDailyPnLEventuallyPopulated(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	// reqPnL is streaming; the first frame can arrive a beat after
	// post-connect setup. Poll account.summary until it lands or the
	// budget expires.
	deadline := time.Now().Add(10 * time.Second)
	var last rpc.AccountResult
	for time.Now().Before(deadline) {
		var res rpc.AccountResult
		if err := conn.Call(ctx, rpc.MethodAccountSummary, nil, &res); err != nil {
			t.Fatalf("account.summary: %v", err)
		}
		last = res
		if res.DailyPnL != nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	// Some IBKR account types (e.g. cash-only sandboxes) never emit
	// PnL frames. Treat as a Skip rather than a Fail to keep the
	// matrix green for those cases; the failure mode that matters is
	// "PnL field arrives populated", and missing data is captured by
	// the dailyPnL pointer being nil — exactly what the wire contract
	// promises.
	t.Skipf("DailyPnL did not arrive within 10s — account may not be entitled to reqPnL feeds. Last response: %+v", last)
}

func TestQuoteSnapshotReturnsPrice(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var q rpc.Quote
	params := rpc.QuoteSnapshotParams{
		Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
	}
	if err := conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
		t.Fatalf("quote.snapshot AAPL: %v", err)
	}
	if q.Symbol != "AAPL" {
		t.Errorf("symbol echoed wrong: %q", q.Symbol)
	}
	if q.DataType == "" {
		t.Errorf("data_type required on every quote response")
	}
	if q.Bid == nil && q.Ask == nil && q.Last == nil {
		// Closed-market tolerance, gated on typed disclosures rather than
		// error text: the daemon's official calendar must say the US equity
		// session is closed AND the quote itself must disclose a degraded
		// quality — the coherent closed-market shapes. An empty quote during
		// an open session, or one claiming firm quality with no prices,
		// still fails.
		degraded := q.QuoteQuality == "prev_close" || q.QuoteQuality == "stale" || q.QuoteQuality == "missing"
		if degraded && !sessionOpen(t, conn, "us") {
			t.Skipf("US equity session closed; daemon disclosed quote_quality=%q with no bid/ask/last — nothing live to assert", q.QuoteQuality)
		}
		t.Errorf("AAPL snapshot delivered no bid/ask/last; suspect timeout or entitlement issue: %+v", q)
	}
}

// sessionOpen asks the daemon's official market calendar whether the given
// market's regular session is open right now. It is the typed authority
// (embedded marketcal schedules: holidays, early closes) behind the
// closed-market tolerances in the quote and chain tests — never error-text
// matching. When the calendar call itself errors, the session is reported
// open so no skip is licensed and the caller's original failure stands.
func sessionOpen(t *testing.T, conn *dial.Conn, market string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res rpc.MarketCalendarResult
	if err := conn.Call(ctx, rpc.MethodMarketCalendar, rpc.MarketCalendarParams{Market: market}, &res); err != nil {
		return true
	}
	return res.Session.IsOpen
}

func TestUnknownMethodReturnsStructuredError(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := conn.Call(ctx, "no.such.method", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	rpcErr, ok := err.(*rpc.Error)
	if !ok {
		t.Fatalf("expected *rpc.Error, got %T: %v", err, err)
	}
	if rpcErr.Code != rpc.CodeUnknownMethod {
		t.Errorf("expected code %q, got %q", rpc.CodeUnknownMethod, rpcErr.Code)
	}
}

func TestTradingVerbsRefused(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, m := range []string{rpc.MethodOrderPlace, rpc.MethodOrderCancel} {
		err := conn.Call(ctx, m, json.RawMessage(`{}`), nil)
		if err == nil {
			t.Errorf("%s: expected refusal in v1, got success", m)
			continue
		}
		rpcErr, ok := err.(*rpc.Error)
		if !ok {
			t.Errorf("%s: expected *rpc.Error, got %T (%v)", m, err, err)
			continue
		}
		if rpcErr.Code != rpc.CodeTradingDisabled {
			t.Errorf("%s: expected code %q, got %q", m, rpc.CodeTradingDisabled, rpcErr.Code)
		}
	}
}

func TestCLIBinaryAccountText(t *testing.T) {
	skipIfNoGateway(t)

	cmd := exec.Command(sharedCLI, "account")
	cmd.Env = append(os.Environ(), "CANARY_SOCKET="+sharedSocket)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("canary account: %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "Account") || !strings.Contains(s, "Net liquidation") {
		t.Errorf("unexpected canary account text output:\n%s", s)
	}
}

// TestScanTopMoversReturnsRows guards against the scanner field-offset
// regression that silently dropped every msgScannerData frame. If the
// dispatcher contract or the parser drifts again, this test catches it
// against the real gateway. Skips when the gateway returns 0 rows (e.g. on
// weekends/holidays the scanner can come back empty for some presets).
func TestScanTopMoversReturnsRows(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	// 80 s = daemon's unaryDeadline(MethodScanRun) (75 s = 35 s scanner +
	// 20 s enrichment + slack) + socket slack. On a fresh client ID against
	// TWS, the cold path is observably ~40 s (28 s scanner-farm warmup +
	// 5 s contract-details warmup + 6 s market-data tick collection); 40 s
	// is on the knife edge and any variance pushes the test ctx over.
	// Setting test ctx ≥ daemon unary deadline lets the daemon's classified
	// "scanner subsystem did not respond within 35s" reach the test on a
	// truly-stuck scanner farm, where isScannerTimeout cleanly skips.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	var res rpc.ScanResult
	params := rpc.ScanRunParams{Preset: "top-movers", Limit: 10}
	if err := conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		// Off-hours scanner subscriptions sometimes hang for the full
		// timeout instead of returning empty rows — TWS behavior, not a
		// regression on our side. Skip rather than fail the suite.
		if isScannerTimeout(err) {
			t.Skipf("scanner timed out (off-hours flakiness): %v", err)
		}
		t.Fatalf("scan.run top-movers: %v", err)
	}
	if len(res.Rows) == 0 {
		t.Skip("scanner returned 0 rows (gateway/preset may have no candidates outside market hours)")
	}
	first := res.Rows[0]
	if first.Symbol == "" {
		t.Errorf("first scanner row has empty symbol: %+v", res)
	}
	if res.Type == "" {
		t.Errorf("scan result missing scan type: %+v", res)
	}
}

// isScannerTimeout matches only the scanner-subscription-specific
// timeout shapes that surface from a cold or off-hours scanner farm.
// Generic "context deadline exceeded" / "i/o timeout" used to be in
// this list too — they were dropped because they also fire on a
// deadlocked handler, a dropped socket, or any other regression a
// scanner test should catch, not skip.
func isScannerTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "scanner subsystem did not respond") ||
		strings.Contains(s, "scanner timed out") ||
		strings.Contains(s, "scanner parameters timed out")
}

// TestScanParamsReturnsCatalog exercises the reqScannerParameters round-trip
// against the live gateway. The XML payload is large (typically 1-2 MB on a
// Pro account); this test pins (a) that the wire-level reader handles a
// multi-megabyte frame without the 1 MB cap that originally truncated it
// and desynced the stream, (b) that the XML parser extracts the three
// catalog lists, and (c) that --instrument STK is a strict subset of the
// unfiltered catalog. If any of those regress, this test fails before
// users see "scanner timed out".
func TestScanParamsReturnsCatalog(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var full rpc.ScanParamsResult
	if err := conn.Call(ctx, rpc.MethodScanParams, rpc.ScanParamsParams{}, &full); err != nil {
		// The catalog request is gateway-stored data, not a live scanner
		// subscription — no off-hours skip applies here. This test is the
		// time-of-day-independent regression guard for the wire/parser
		// path, so an error must fail, not be silently swallowed.
		t.Fatalf("scan.params (unfiltered): %v", err)
	}
	if len(full.Instruments) == 0 || len(full.Locations) == 0 || len(full.ScanTypes) == 0 {
		t.Fatalf("scan.params returned empty catalog: %+v", full)
	}
	// Sanity-check well-known scanCodes the daemon hardcodes as defaults.
	want := map[string]bool{
		"TOP_PERC_GAIN":                false,
		"TOP_PERC_LOSE":                false,
		"MOST_ACTIVE":                  false,
		"HOT_BY_VOLUME":                false,
		"HIGH_OPEN_GAP":                false,
		"HIGH_OPT_IMP_VOLAT_OVER_HIST": false,
		"HOT_BY_OPT_VOLUME":            false,
	}
	for _, st := range full.ScanTypes {
		if _, ok := want[st.Code]; ok {
			want[st.Code] = true
		}
	}
	for code, found := range want {
		if !found {
			t.Errorf("scan.params missing default-preset scanCode %q from the live catalog (the v0.11 defaults were validated against IB Gateway server-version 203 — if your gateway drops one of these, the default preset fails)", code)
		}
	}

	var stk rpc.ScanParamsResult
	if err := conn.Call(ctx, rpc.MethodScanParams, rpc.ScanParamsParams{Instrument: "STK"}, &stk); err != nil {
		t.Fatalf("scan.params (instrument=STK): %v", err)
	}
	if len(stk.ScanTypes) == 0 {
		t.Fatalf("scan.params with --instrument STK returned 0 scan types")
	}
	if len(stk.ScanTypes) > len(full.ScanTypes) {
		t.Errorf("filtered scan_types (%d) > unfiltered (%d) — filter must be a subset", len(stk.ScanTypes), len(full.ScanTypes))
	}
}

// TestScanAdHocAgainstDefaultLocation runs an ad-hoc scan (Type+Exchange
// only, no preset). Sister to TestScanTopMoversReturnsRows but exercises
// the new ad-hoc dispatch path in handleScanRun. Skips on 0 rows because
// scanner output can be empty outside market hours.
func TestScanAdHocAgainstDefaultLocation(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	// Same rationale as TestScanTopMoversReturnsRows: ctx must outlive the
	// daemon's full unaryDeadline(MethodScanRun) = 75 s so the cold-path
	// completes and the classified scanner-timeout (if it fires) reaches
	// the test instead of being pre-empted.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	params := rpc.ScanRunParams{Type: "TOP_PERC_GAIN", Exchange: "STK.US.MAJOR", Limit: 5}
	var res rpc.ScanResult
	if err := conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		// Off-hours scanner subscriptions can hang for the full timeout
		// rather than returning empty rows. That's a property of TWS, not
		// a bug in our code — skip the same way TestScanTopMoversReturnsRows
		// would have if rows came back empty. The gateway's catalog test
		// (TestScanParamsReturnsCatalog above) is the time-of-day-independent
		// regression guard for the wire/parser path.
		if isScannerTimeout(err) {
			t.Skipf("scanner timed out (off-hours flakiness): %v", err)
		}
		t.Fatalf("ad-hoc scan: %v", err)
	}
	if res.Preset != "" {
		t.Errorf("ad-hoc result.Preset = %q, want empty (preset is only set for named runs)", res.Preset)
	}
	if res.Type != "TOP_PERC_GAIN" {
		t.Errorf("ad-hoc result.Type = %q, want TOP_PERC_GAIN (must echo the scanCode the caller passed)", res.Type)
	}
	if len(res.Rows) == 0 {
		t.Skip("ad-hoc scanner returned 0 rows (off-hours/quiet market)")
	}
}

// TestScanAdHocMissingExchangeIsBadRequest covers the validation path:
// passing --type without --exchange is a user error (or a confused
// agent), and must surface as bad_request rather than wedging the daemon
// or sending an under-specified subscription to the gateway.
func TestScanAdHocMissingExchangeIsBadRequest(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var res rpc.ScanResult
	err := conn.Call(ctx, rpc.MethodScanRun, rpc.ScanRunParams{Type: "TOP_PERC_GAIN"}, &res)
	if err == nil {
		t.Fatalf("expected bad_request error for ad-hoc scan missing exchange, got success: %+v", res)
	}
	if !strings.Contains(err.Error(), "bad_request") && !strings.Contains(err.Error(), "exchange") {
		t.Errorf("expected error to mention bad_request or exchange, got: %v", err)
	}
}

// TestChainAAPLLegsPopulated guards against the option-chain ConID resolution
// regression. Before the fix, every leg subscribed without a resolved ConID
// and IBKR returned code 200 ("No security definition has been found"); the
// CLI rendered an all-blank table. The fix calls reqContractDetails first
// and feeds the resolved ConID into reqMktData.
//
// The "no fabrication" invariant means cells legitimately stay nil when the
// gateway doesn't deliver a price (e.g. illiquid strikes far from ATM). We
// only assert that AT LEAST one strike has at least one populated leg field —
// any more and the test would be brittle against weekend frozen-data quirks.
func TestChainAAPLLegsPopulated(t *testing.T) {
	skipIfNoGateway(t)
	conn := client(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	expiry := nextThirdFriday(time.Now().UTC())
	params := rpc.ChainFetchParams{
		Symbol: "AAPL",
		Expiry: expiry.Format("2006-01-02"),
		Width:  3, // 7 strikes — keeps the per-leg round-trip count modest
		Side:   "both",
	}
	var res rpc.ChainResult
	if err := conn.Call(ctx, rpc.MethodChainFetch, params, &res); err != nil {
		// Per-leg option market data does not tick outside the options
		// session, so the daemon's classified unary timeout is the expected
		// closed-market shape. Both gate conditions are typed — the rpc
		// error code and the official calendar — so a timeout while the
		// session is open (deadlocked handler, dropped socket) still fails.
		if isRPCTimeout(err) && !sessionOpen(t, conn, "us-options") {
			t.Skipf("chain.fetch timed out with the US options session closed: %v", err)
		}
		t.Fatalf("chain.fetch AAPL %s: %v", params.Expiry, err)
	}
	wantStrikes := 2*params.Width + 1
	if got := len(res.Strikes); got != wantStrikes {
		t.Fatalf("expected %d strikes (ATM ± %d), got %d", wantStrikes, params.Width, got)
	}
	if res.Spot <= 0 {
		t.Errorf("chain spot price not populated: %+v", res)
	}
	if res.DTE <= 0 {
		t.Errorf("chain DTE should be positive, got %d", res.DTE)
	}
	populated := 0
	for _, s := range res.Strikes {
		if s.CallBid != nil || s.CallAsk != nil || s.CallLast != nil ||
			s.PutBid != nil || s.PutAsk != nil || s.PutLast != nil {
			populated++
		}
	}
	if populated == 0 {
		if !sessionOpen(t, conn, "us-options") {
			t.Skip("no strike populated with the US options session closed — frozen feed delivered nothing; the ConID regression check needs an open session")
		}
		t.Errorf("no strike had any leg field populated; ConID resolution likely broken again. result=%+v", res)
	}
}

// isRPCTimeout reports whether err is the daemon's classified unary timeout
// (rpc code "timeout"), as opposed to a client-side context expiry or any
// other error shape.
func isRPCTimeout(err error) bool {
	rpcErr, ok := err.(*rpc.Error)
	return ok && rpcErr.Code == rpc.CodeTimeout
}

// nextThirdFriday returns the third Friday of the month at least 7 days from
// now. AAPL has weekly options too but third-Friday monthlies are universally
// liquid — picking them keeps the test stable across weeks. The resulting
// date is also always > current time, so DTE > 0 is guaranteed.
func nextThirdFriday(now time.Time) time.Time {
	month := now.Month()
	year := now.Year()
	for {
		first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		// Friday is weekday 5 (Sunday=0). Third Friday = first Friday + 14 days.
		offset := (int(time.Friday) - int(first.Weekday()) + 7) % 7
		third := first.AddDate(0, 0, offset+14)
		if third.Sub(now) >= 7*24*time.Hour {
			return third
		}
		month++
		if month > 12 {
			month = 1
			year++
		}
	}
}
