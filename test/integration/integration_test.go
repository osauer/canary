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
	integrationModeOptional integrationTestMode = "optional"

	integrationModeHermetic integrationTestMode = "hermetic"

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

func TestMain(m *testing.M) {
	mode, err := parseIntegrationTestMode(os.Getenv(integrationModeEnv))
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: " + err.Error() + "\n")
		os.Exit(2)
	}

	cli, err := buildBin()
	if err != nil {
		_, _ = os.Stderr.WriteString("integration: build failed: " + err.Error() + "\n")
		os.Exit(2)
	}
	sharedCLI = cli

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

	cmd.Env = append(os.Environ(),
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
		"XDG_CACHE_HOME="+filepath.Join(dir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "config"),
		"XDG_DATA_HOME="+filepath.Join(dir, "data"),
	)

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

		degraded := q.QuoteQuality == "prev_close" || q.QuoteQuality == "stale" || q.QuoteQuality == "missing"
		if degraded && !sessionOpen(t, conn, "us") {
			t.Skipf("US equity session closed; daemon disclosed quote_quality=%q with no bid/ask/last — nothing live to assert", q.QuoteQuality)
		}
		t.Errorf("AAPL snapshot delivered no bid/ask/last; suspect timeout or entitlement issue: %+v", q)
	}
}

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
