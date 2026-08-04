package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRenderStatus_BackgroundLine pins the rendering contract: the
// `Background` line appears iff `result.BackgroundTasks` is non-empty;
// wire tokens are mapped to short verb phrases (so the row reads as
// English); phrases are comma-separated when multiple tasks run; an
// unknown token falls through verbatim. Empty list omits the line.
func TestRenderStatus_BackgroundLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tasks   []rpc.BackgroundTaskStatus
		want    string // substring that MUST appear
		notWant string // substring that MUST NOT appear
	}{
		{
			name:    "idle daemon omits line",
			tasks:   nil,
			notWant: "Background:",
		},
		{
			name:  "single task renders as verb phrase",
			tasks: []rpc.BackgroundTaskStatus{{Name: "breadth-spx"}},
			want:  "Background     refreshing rolling SPX breadth",
		},
		{
			name: "multiple tasks render comma-separated",
			tasks: []rpc.BackgroundTaskStatus{
				{Name: "breadth-spx"},
				{Name: "gamma-zero"},
			},
			want: "Background     refreshing rolling SPX breadth, computing dealer zero-gamma",
		},
		{
			name:  "unknown token falls through verbatim",
			tasks: []rpc.BackgroundTaskStatus{{Name: "future-task"}},
			want:  "Background     future-task",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
			res := &rpc.HealthResult{
				DaemonVersion:   "test",
				Connected:       true,
				ServerVersion:   200,
				BackgroundTasks: tc.tasks,
			}
			renderStatusText(env, res, nil)
			got := stdout.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("status missing expected substring %q:\n%s", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("status contained unexpected substring %q:\n%s", tc.notWant, got)
			}
		})
	}
}

func TestIsHandshakeInFlight(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		want bool
	}{
		{"connected", rpc.HealthResult{Connected: true}, false},
		{"degraded with error", rpc.HealthResult{LastError: "boom"}, false},
		{"connecting (no error yet)", rpc.HealthResult{}, true},
		{"connected wins over stale error", rpc.HealthResult{Connected: true, LastError: "stale"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHandshakeInFlight(tc.in); got != tc.want {
				t.Fatalf("isHandshakeInFlight(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// waitForHandshake must return immediately when the fetcher reports a
// connected gateway on the first poll — no extra polls, no busy-wait.
func TestWaitForHandshakeReturnsOnConnected(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		calls.Add(1)
		return rpc.HealthResult{Connected: true}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	if !res.Connected {
		t.Fatalf("expected Connected, got %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", got)
	}
}

// Once the daemon reports a LastError (handshake failed), the wait must
// stop and surface that result — don't keep polling against a known-bad
// gateway.
func TestWaitForHandshakeReturnsOnError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		calls.Add(1)
		return rpc.HealthResult{LastError: "dial timeout"}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	if res.LastError != "dial timeout" {
		t.Fatalf("expected LastError preserved, got %+v", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 fetch, got %d", got)
	}
}

// When the gateway is wedged (every poll returns "still connecting"),
// the wait must hit its budget and return the last snapshot. Verifies
// the bound holds even with a fast poll interval — no infinite loop.
func TestWaitForHandshakeRespectsBudget(t *testing.T) {
	t.Parallel()
	// Fetcher mirrors real daemon behavior: static fields (DaemonVersion,
	// Profile, …) come back populated even while Connected is still false.
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{DaemonVersion: "v1"}, nil
	}
	var w bytes.Buffer
	start := time.Now()
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{DaemonVersion: "v1"}, 80*time.Millisecond, 10*time.Millisecond)
	elapsed := time.Since(start)

	if res.Connected || res.LastError != "" {
		t.Fatalf("expected still-connecting result, got %+v", res)
	}
	if res.DaemonVersion != "v1" {
		t.Fatalf("expected DaemonVersion preserved, got %+v", res)
	}
	if elapsed < 60*time.Millisecond {
		t.Fatalf("wait returned too early: %s (budget 80ms)", elapsed)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("wait overshot budget: %s (budget 80ms)", elapsed)
	}
}

// ctx cancellation (Ctrl+C) must short-circuit the wait — return the
// last good snapshot immediately rather than spinning out the budget.
func TestWaitForHandshakeRespectsContextCancel(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the loop runs

	var w bytes.Buffer
	start := time.Now()
	res := waitForHandshake(ctx, &w, fetch, rpc.HealthResult{DaemonVersion: "init"}, 5*time.Second, 100*time.Millisecond)
	elapsed := time.Since(start)

	if res.DaemonVersion != "init" {
		t.Fatalf("expected initial snapshot returned, got %+v", res)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("ctx cancel ignored; took %s", elapsed)
	}
}

// A transient RPC error during polling must not panic the CLI — return
// the last good snapshot and stop polling. (Daemon could be SIGTERMing
// mid-status, etc.) The "last good" snapshot is whichever fetch most
// recently succeeded — we don't fall back to the original initial.
func TestWaitForHandshakeReturnsOnFetchError(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		n := calls.Add(1)
		if n > 1 {
			return rpc.HealthResult{}, errors.New("conn closed")
		}
		return rpc.HealthResult{DaemonVersion: "fresh"}, nil
	}
	var w bytes.Buffer
	res := waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{DaemonVersion: "init"}, 5*time.Second, 1*time.Millisecond)

	if res.DaemonVersion != "fresh" {
		t.Fatalf("expected most-recent successful snapshot returned, got %+v", res)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("expected at least 2 fetch attempts (one ok, one erroring); got %d", got)
	}
}

// The progress UI ("waiting for IB Gateway handshake (up to N)" + dots)
// must land on stderr so it doesn't pollute structured stdout from
// neighboring shell commands.
func TestWaitForHandshakeWritesProgressToWriter(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{Connected: true}, nil
	}
	var w bytes.Buffer
	_ = waitForHandshake(context.Background(), &w, fetch, rpc.HealthResult{}, 5*time.Second, 1*time.Millisecond)
	out := w.String()
	if !strings.Contains(out, "waiting for IB Gateway handshake") {
		t.Fatalf("progress message missing from output: %q", out)
	}
	if !strings.Contains(out, ".") {
		t.Fatalf("expected at least one progress dot in output: %q", out)
	}
}

func TestWaitForStatusVerdictJSONSuppressesProgress(t *testing.T) {
	t.Parallel()
	fetch := func(ctx context.Context) (rpc.HealthResult, error) {
		return rpc.HealthResult{Connected: true}, nil
	}
	var progress bytes.Buffer
	res := waitForStatusVerdict(context.Background(), &progress, true, rpc.HealthResult{}, fetch)
	if !res.Connected {
		t.Fatalf("expected connected result after wait, got %+v", res)
	}
	if progress.Len() != 0 {
		t.Fatalf("JSON status wait wrote progress output: %q", progress.String())
	}
}

func TestRenderStatus_FlightDeckShape(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		Account:       "DU0000000",
		AccountMode:   rpc.AccountModePaper,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4002,
		PortOrigin:    "discovered",
		ClientID:      17,
		Connected:     true,
		ServerVersion: 178,
		Members: rpc.MembersHealth{
			Source:       "cache",
			AsOf:         time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC),
			Count:        503,
			RefreshState: "healthy",
		},
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  READY",
		"Session        DU0000000 (PAPER) via 127.0.0.1:4002 (tls=false, discovered), client 17",
		"Market data    Live",
		"Daemon         v1.0.0, up 30m42s",
		"TWS            API server 178",
		"SPX members    cache:2026-05-22, 503 names",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Next concern") {
		t.Fatalf("clean status should omit Next concern:\n%s", got)
	}
}

func TestRenderStatus_ConnectedAccountAndPaperBadge(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	res := &rpc.HealthResult{
		DaemonVersion:    "v1.0.0",
		Account:          "",
		ConnectedAccount: "DU1234567",
		AccountMode:      rpc.AccountModePaper,
		GatewayHost:      "127.0.0.1",
		GatewayPort:      4002,
		PortOrigin:       "discovered",
		ClientID:         15,
		Connected:        true,
		ServerVersion:    203,
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	wantBadge := ansiYellow + ansiBold + "PAPER" + ansiReset + ansiReset
	if !strings.Contains(got, "Session") || !strings.Contains(got, "DU1234567 ("+wantBadge+") via 127.0.0.1:4002") {
		t.Fatalf("status should render connected paper account with prominent badge:\n%q", got)
	}
	if strings.Contains(got, "auto-detect") {
		t.Fatalf("connected account should replace auto-detect placeholder:\n%s", got)
	}
}

func TestStatusAccountIDPrefersPinOverManagedAccountsAggregate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		pinned    string
		connected string
		want      string
	}{
		{name: "single account login", pinned: "DU2222222", connected: "DU2222222", want: "DU2222222"},
		{name: "connected before the pin resolves", connected: "DU2222222", want: "DU2222222"},
		{name: "multi-account login shows the pin", pinned: "DU2222222", connected: "DU1111111,DU2222222", want: "DU2222222"},
		{name: "unpinned multi-account login names no sibling", connected: "DU1111111,DU2222222", want: "auto-detect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := statusAccountID(rpc.HealthResult{Account: test.pinned, ConnectedAccount: test.connected})
			if got != test.want {
				t.Fatalf("statusAccountID = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFormatStatusAccountMode(t *testing.T) {
	t.Parallel()
	if got := formatStatusAccountMode(&Env{Color: false}, rpc.AccountModePaper); got != "PAPER" {
		t.Fatalf("paper no-color badge = %q, want PAPER", got)
	}
	if got := formatStatusAccountMode(&Env{Color: false}, rpc.AccountModeLive); got != "live" {
		t.Fatalf("live badge = %q, want live", got)
	}
	if got := formatStatusAccountMode(&Env{Color: false}, ""); got != "" {
		t.Fatalf("empty mode badge = %q, want empty", got)
	}
}

func TestRenderStatus_VersionDrift(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Version: "v1.2.4"}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.2.3",
		UptimeSeconds: 1842,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   7496,
		ClientID:      15,
		Connected:     true,
		ServerVersion: 203,
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  ATTENTION",
		"Daemon         v1.2.3, up 30m42s",
		"Next concern   CLI version v1.2.4 differs from daemon v1.2.3; run `canary restart` to pick up the new binary",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStatus_DataQualityKeepsGatewayReady(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   7496,
		ClientID:      15,
		Connected:     true,
		ServerVersion: 203,
		DataQuality: []rpc.DataQualityHealth{
			{Surface: "gamma", Status: "degraded", Summary: "degraded: SPX excluded", DegradedClusters: []string{"gamma"}},
			{Surface: "regime", Status: "stale", Summary: "stale: vol, credit", StaleClusters: []string{"vol", "credit"}},
		},
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  READY",
		"Data quality   gamma degraded",
		"SPX excluded",
		"regime stale",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Next concern") {
		t.Fatalf("data-quality-only status should not duplicate Next concern:\n%s", got)
	}
}

func TestRenderStatus_DataFarmsIssueGetsAttention(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   7496,
		ClientID:      15,
		Connected:     true,
		ServerVersion: 203,
		DataFarms: []rpc.DataFarmHealth{{
			Name:   "usopt",
			Type:   "market",
			Status: "disconnected",
			Code:   2103,
		}},
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  ATTENTION",
		"Data farms     market:usopt disconnected (IBKR 2103)",
		"Next concern   Data farm issue: market:usopt disconnected (IBKR 2103)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

func TestRenderStatus_SubsystemIssueGetsAttention(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	res := &rpc.HealthResult{
		DaemonVersion: "v1.0.0",
		UptimeSeconds: 1842,
		GatewayHost:   "127.0.0.1",
		GatewayPort:   7496,
		ClientID:      15,
		Connected:     true,
		ServerVersion: 203,
		Subsystems: []rpc.SubsystemHealth{{
			Name:    "quote",
			Status:  "degraded",
			Message: "no market-data farm connection notice observed; quotes may time out",
		}},
	}
	renderStatusText(env, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  ATTENTION",
		"Subsystems     quote:degraded",
		"Next concern   Subsystem issue: quote degraded: no market-data farm connection notice observed; quotes may time out",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
}

// TestRenderStatus_MarketAccessRow pins the surface issue #26 was missing: a
// route key the gateway is refusing shows up once, in `canary status`, naming
// the symbol, the IBKR code, and when the suppression window lifts. The desk
// with nothing refused shows no row at all.
func TestRenderStatus_MarketAccessRow(t *testing.T) {
	t.Parallel()
	observed := time.Date(2026, 8, 4, 14, 3, 0, 0, time.UTC).Local()
	base := func(access []rpc.MarketDataAccessHealth) *rpc.HealthResult {
		return &rpc.HealthResult{
			DaemonVersion:    "v1.0.0",
			UptimeSeconds:    1842,
			GatewayHost:      "127.0.0.1",
			GatewayPort:      7496,
			ClientID:         15,
			Connected:        true,
			ServerVersion:    203,
			MarketDataAccess: access,
		}
	}

	var quiet bytes.Buffer
	renderStatusText(&Env{Stdout: &quiet, Stderr: &bytes.Buffer{}}, base(nil), nil)
	if strings.Contains(quiet.String(), "Market access") {
		t.Fatalf("desk with no rejection must not render the row:\n%s", quiet.String())
	}

	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base([]rpc.MarketDataAccessHealth{{
		RouteKey:   "SPX|IND|CBOE",
		Symbol:     "SPX",
		Code:       354,
		Reason:     rpc.MarketDataAccessNotSubscribed,
		ObservedAt: observed,
		RetryAt:    observed.Add(30 * time.Minute),
	}}), nil)
	got := stdout.String()
	want := "SPX not subscribed (IBKR 354, seen " + observed.Format("15:04") + ", retry " + observed.Add(30*time.Minute).Format("15:04") + ")"
	for _, substr := range []string{
		"IBKR Gateway  ATTENTION",
		"Market access  " + want,
		"Next concern   Market data access: " + want,
	} {
		if !strings.Contains(got, substr) {
			t.Fatalf("status missing %q:\n%s", substr, got)
		}
	}
}

// TestFormatMarketDataAccessValueCollapsesTail keeps the one-line row readable
// when a whole watchlist is refused: the first few keys are named and the rest
// become a count, rather than a row that wraps off the screen.
func TestFormatMarketDataAccessValueCollapsesTail(t *testing.T) {
	t.Parallel()
	items := make([]rpc.MarketDataAccessHealth, 0, 5)
	for _, symbol := range []string{"AAPL", "MSFT", "NVDA", "SPX", "ZVZZT"} {
		items = append(items, rpc.MarketDataAccessHealth{
			RouteKey: symbol, Symbol: symbol, Code: 354, Reason: rpc.MarketDataAccessNotSubscribed,
		})
	}
	got := formatMarketDataAccessValue(items)
	want := "AAPL not subscribed (IBKR 354); MSFT not subscribed (IBKR 354); NVDA not subscribed (IBKR 354); +2 more"
	if got != want {
		t.Fatalf("formatMarketDataAccessValue =\n%s\nwant\n%s", got, want)
	}
	if unknown := formatMarketDataAccessValue([]rpc.MarketDataAccessHealth{{
		RouteKey: "ZVZZT", Symbol: "ZVZZT", Code: 322, Reason: rpc.MarketDataAccessRejected,
	}}); unknown != "ZVZZT refused (IBKR 322)" {
		t.Fatalf("non-354 rejection = %q, want %q", unknown, "ZVZZT refused (IBKR 322)")
	}
}

func TestNextConcernPriority(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		cli  string
		want string
	}{
		{
			name: "gateway error wins",
			in:   rpc.HealthResult{LastError: "dial timeout", DataType: rpc.MarketDataDelayed},
			want: "Gateway offline: dial timeout",
		},
		{
			name: "handshake pending",
			in:   rpc.HealthResult{},
			cli:  "v1.2.4",
			want: "Gateway handshake still in progress",
		},
		{
			name: "version drift before market data",
			in: rpc.HealthResult{
				DaemonVersion: "v1.2.3",
				Connected:     true,
				DataType:      rpc.MarketDataFrozen,
			},
			cli:  "v1.2.4",
			want: "CLI version v1.2.4 differs from daemon v1.2.3; run `canary restart` to pick up the new binary",
		},
		{
			name: "data farm before market data",
			in: rpc.HealthResult{
				Connected: true,
				DataType:  rpc.MarketDataFrozen,
				DataFarms: []rpc.DataFarmHealth{{
					Name:   "ushmds",
					Type:   "historical",
					Status: "disconnected",
					Code:   2105,
				}},
			},
			want: "Data farm issue: historical:ushmds disconnected (IBKR 2105)",
		},
		{
			name: "data farm before subsystem",
			in: rpc.HealthResult{
				Connected: true,
				DataFarms: []rpc.DataFarmHealth{{
					Name:   "usfarm",
					Type:   "market",
					Status: "disconnected",
					Code:   2103,
				}},
				Subsystems: []rpc.SubsystemHealth{{
					Name:    "quote",
					Status:  "degraded",
					Message: "no market-data farm connection notice observed; quotes may time out",
				}},
			},
			want: "Data farm issue: market:usfarm disconnected (IBKR 2103)",
		},
		{
			name: "data farm before market access",
			in: rpc.HealthResult{
				Connected: true,
				DataFarms: []rpc.DataFarmHealth{{
					Name:   "usfarm",
					Type:   "market",
					Status: "disconnected",
					Code:   2103,
				}},
				MarketDataAccess: []rpc.MarketDataAccessHealth{{
					RouteKey: "SPX",
					Symbol:   "SPX",
					Code:     354,
					Reason:   rpc.MarketDataAccessNotSubscribed,
				}},
			},
			want: "Data farm issue: market:usfarm disconnected (IBKR 2103)",
		},
		{
			name: "market access before subsystem",
			in: rpc.HealthResult{
				Connected: true,
				MarketDataAccess: []rpc.MarketDataAccessHealth{{
					RouteKey: "SPX",
					Symbol:   "SPX",
					Code:     354,
					Reason:   rpc.MarketDataAccessNotSubscribed,
				}},
				Subsystems: []rpc.SubsystemHealth{{
					Name:   "quote",
					Status: "degraded",
				}},
			},
			want: "Market data access: SPX not subscribed (IBKR 354)",
		},
		{
			name: "subsystem before market data",
			in: rpc.HealthResult{
				Connected: true,
				DataType:  rpc.MarketDataFrozen,
				Subsystems: []rpc.SubsystemHealth{{
					Name:    "quote",
					Status:  "degraded",
					Message: "no market-data farm connection notice observed; quotes may time out",
				}},
			},
			want: "Subsystem issue: quote degraded: no market-data farm connection notice observed; quotes may time out",
		},
		{
			name: "market data before members",
			in: rpc.HealthResult{
				Connected: true,
				DataType:  rpc.MarketDataFrozen,
				Members:   rpc.MembersHealth{Source: "cache", RefreshState: "parse_failed"},
			},
			want: "Market data is Frozen",
		},
		{
			name: "members refresh",
			in: rpc.HealthResult{
				Connected: true,
				Members:   rpc.MembersHealth{Source: "cache", RefreshState: "parse_failed"},
			},
			want: "SPX members refresh parse_failed",
		},
		{
			name: "trading blocked",
			in: rpc.HealthResult{
				Connected: true,
				Trading: rpc.TradingStatus{
					Mode:    "paper",
					Blocked: true,
					Blockers: []rpc.TradingBlocker{{
						Code:    "gateway_account_unpinned",
						Message: "order submission requires a pinned account",
					}},
				},
			},
			want: "Trading blocked: order submission requires a pinned account",
		},
		{
			name: "background work",
			in: rpc.HealthResult{
				Connected:       true,
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "Background work: computing dealer zero-gamma",
		},
		{
			name: "data quality does not mask background work",
			in: rpc.HealthResult{
				Connected:       true,
				DataQuality:     []rpc.DataQualityHealth{{Surface: "regime", Status: "stale", Summary: "stale: vol, credit"}},
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "Background work: computing dealer zero-gamma",
		},
		{
			name: "none",
			in:   rpc.HealthResult{Connected: true},
			want: "None",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nextConcern(tc.in, tc.cli)
			if got.Text != tc.want {
				t.Fatalf("nextConcern(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

func TestFormatDataQualityValueParenthesizesOffHoursContext(t *testing.T) {
	t.Parallel()
	closed := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	rth := time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC)
	items := []rpc.DataQualityHealth{
		{Surface: "gamma", Status: "degraded", Summary: "degraded: SPX excluded", DegradedClusters: []string{"gamma"}},
		{Surface: "regime", Status: "stale", Summary: "stale: vol, credit", StaleClusters: []string{"vol", "credit"}},
	}
	if got, want := formatDataQualityValueAt(items, closed), "gamma degraded (SPX excluded); regime stale (off-hours: vol, credit)"; got != want {
		t.Fatalf("closed format = %q, want %q", got, want)
	}
	if got, want := formatDataQualityValueAt(items, rth), "gamma degraded (SPX excluded); regime stale: vol, credit"; got != want {
		t.Fatalf("RTH format = %q, want %q", got, want)
	}
}

func TestFormatDataQualityValueKeepsSPXCacheFallback(t *testing.T) {
	t.Parallel()
	items := []rpc.DataQualityHealth{
		{Surface: "gamma", Status: "degraded", Summary: "degraded: SPX cache fallback", DegradedClusters: []string{"gamma"}},
	}
	if got, want := formatDataQualityValueAt(items, time.Date(2026, time.June, 1, 6, 0, 0, 0, time.UTC)), "gamma degraded: SPX cache fallback"; got != want {
		t.Fatalf("format = %q, want %q", got, want)
	}
}

func TestFormatDataFarmsValue(t *testing.T) {
	t.Parallel()
	farms := []rpc.DataFarmHealth{
		{Name: "usopt", Type: "market", Status: "disconnected", Code: 2103},
		{Name: "tws-server", Type: "connectivity", Status: "broken", Code: 2110},
	}
	want := "market:usopt disconnected (IBKR 2103), connectivity:tws-server broken (IBKR 2110)"
	if got := formatDataFarmsValue(farms); got != want {
		t.Fatalf("formatDataFarmsValue = %q, want %q", got, want)
	}
}

func TestStatusVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.HealthResult
		cli  string
		want string
	}{
		{
			name: "ready",
			in:   rpc.HealthResult{Connected: true},
			want: "READY",
		},
		{
			name: "background is still ready",
			in: rpc.HealthResult{
				Connected:       true,
				BackgroundTasks: []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}},
			},
			want: "READY",
		},
		{
			name: "version drift",
			in:   rpc.HealthResult{DaemonVersion: "v1.2.3", Connected: true},
			cli:  "v1.2.4",
			want: "ATTENTION",
		},
		{
			name: "market data warning",
			in:   rpc.HealthResult{Connected: true, DataType: rpc.MarketDataDelayed},
			want: "ATTENTION",
		},
		{
			name: "trading blocked",
			in: rpc.HealthResult{
				Connected: true,
				Trading:   rpc.TradingStatus{Mode: "paper", Blocked: true},
			},
			want: "ATTENTION",
		},
		{
			name: "data farm warning",
			in: rpc.HealthResult{
				Connected: true,
				DataFarms: []rpc.DataFarmHealth{{
					Name:   "usopt",
					Type:   "market",
					Status: "disconnected",
				}},
			},
			want: "ATTENTION",
		},
		{
			name: "subsystem warning",
			in: rpc.HealthResult{
				Connected: true,
				Subsystems: []rpc.SubsystemHealth{{
					Name:   "scanner",
					Status: "degraded",
				}},
			},
			want: "ATTENTION",
		},
		{
			name: "starting",
			in:   rpc.HealthResult{},
			want: "STARTING",
		},
		{
			name: "offline",
			in:   rpc.HealthResult{LastError: "dial timeout"},
			want: "OFFLINE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusVerdict(tc.in, tc.cli)
			if got.Text != tc.want {
				t.Fatalf("statusVerdict(%+v) = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

func TestDaemonVersionDrift(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		daemon string
		cli    string
		want   bool
	}{
		{name: "same", daemon: "v1.2.3", cli: "v1.2.3"},
		{name: "different", daemon: "v1.2.3", cli: "v1.2.4", want: true},
		{name: "empty cli quiet", daemon: "v1.2.3"},
		{name: "empty daemon quiet", cli: "v1.2.3"},
		{name: "dev cli quiet", daemon: "v1.2.3", cli: "dev"},
		{name: "dev daemon quiet", daemon: "dev", cli: "v1.2.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := daemonVersionDrift(tc.daemon, tc.cli)
			if got != tc.want {
				t.Fatalf("daemonVersionDrift(%q, %q) = %v, want %v", tc.daemon, tc.cli, got, tc.want)
			}
		})
	}
}

// TestFormatMembersValue pins the four rendering variants of the
// S&P500 members row: healthy (no refresh: tail), pinned (env/config),
// silent rot (parse_failed / network_failed). Zero-value source omits
// the line entirely so a daemon that hasn't populated MembersHealth
// yet doesn't show a misleading "S&P500 members: :" row.
func TestFormatMembersValue(t *testing.T) {
	t.Parallel()
	d := time.Date(2026, time.May, 22, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		health rpc.MembersHealth
		want   string
		empty  bool
	}{
		{
			name:   "healthy cache",
			health: rpc.MembersHealth{Source: "cache", AsOf: d, Count: 503, RefreshState: "healthy"},
			want:   "cache:2026-05-22, 503 names",
		},
		{
			name:   "healthy embedded",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "healthy"},
			want:   "embedded:2026-05-22, 503 names",
		},
		{
			name:   "empty refresh state (no refresher attached) treated as healthy",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: ""},
			want:   "embedded:2026-05-22, 503 names",
		},
		{
			name:   "parse failure surfaces",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "parse_failed"},
			want:   "embedded:2026-05-22, 503 names, refresh parse_failed",
		},
		{
			name:   "network failure surfaces",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "network_failed"},
			want:   "embedded:2026-05-22, 503 names, refresh network_failed",
		},
		{
			name:   "disabled config",
			health: rpc.MembersHealth{Source: "embedded", AsOf: d, Count: 503, RefreshState: "disabled (config)"},
			want:   "embedded:2026-05-22, 503 names, refresh disabled (config)",
		},
		{
			name:   "disabled env on cache file",
			health: rpc.MembersHealth{Source: "cache", AsOf: d, Count: 503, RefreshState: "disabled (env)"},
			want:   "cache:2026-05-22, 503 names, refresh disabled (env)",
		},
		{
			name:   "empty source omits row",
			health: rpc.MembersHealth{},
			empty:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatMembersValue(tc.health)
			if tc.empty {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("missing substring %q:\n%s", tc.want, got)
			}
		})
	}
}

func TestFormatSubsystemsValueCollapsesTheHealthyMajority(t *testing.T) {
	t.Parallel()
	env := &Env{Color: false}
	allReady := []rpc.SubsystemHealth{{Name: "storage", Status: "ready"}, {Name: "quote", Status: "ready"}, {Name: "gamma", Status: "ready"}}
	if got := formatSubsystemsValue(env, allReady); got != "all 3 ready" {
		t.Fatalf("all-ready subsystems = %q, want the collapsed count", got)
	}
	mixed := []rpc.SubsystemHealth{{Name: "storage", Status: "ready"}, {Name: "quote", Status: "degraded"}, {Name: "gamma", Status: "ready"}}
	if got := formatSubsystemsValue(env, mixed); got != "quote:degraded, +2 ready" {
		t.Fatalf("mixed subsystems = %q, want the exception plus the ready count", got)
	}
	// A lone exception keeps the exact legacy shape.
	one := []rpc.SubsystemHealth{{Name: "quote", Status: "degraded"}}
	if got := formatSubsystemsValue(env, one); got != "quote:degraded" {
		t.Fatalf("single degraded subsystem = %q", got)
	}
}

func TestRenderStatusAlertCoverageRow(t *testing.T) {
	t.Parallel()
	base := func() *rpc.HealthResult {
		return &rpc.HealthResult{DaemonVersion: "v1.0.0", Account: "DU0000000", AccountMode: rpc.AccountModePaper,
			GatewayHost: "127.0.0.1", GatewayPort: 4002, PortOrigin: "discovered", ClientID: 17, Connected: true, ServerVersion: 178}
	}
	partial := &rpc.AlertCandidateSnapshot{Coverage: rpc.AlertCoverage{
		State: rpc.AlertCoveragePartial, Freshness: rpc.AlertCoverageCurrent,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRulebook, rpc.AlertSourceRegime, rpc.AlertSourceProtection},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceRulebook, rpc.AlertSourceProtection},
	}}
	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), partial)
	if !strings.Contains(stdout.String(), "Alerts         2/3 sources covered — missing: regime") {
		t.Fatalf("partial alert coverage row missing:\n%s", stdout.String())
	}

	complete := &rpc.AlertCandidateSnapshot{Coverage: rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRulebook},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceRulebook},
	}}
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), complete)
	if !strings.Contains(stdout.String(), "Alerts         1/1 sources covered") {
		t.Fatalf("complete alert coverage row missing:\n%s", stdout.String())
	}

	// No snapshot (older daemon): the row is simply absent.
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), nil)
	if strings.Contains(stdout.String(), "Alerts ") {
		t.Fatalf("alerts row rendered without a snapshot:\n%s", stdout.String())
	}
}
