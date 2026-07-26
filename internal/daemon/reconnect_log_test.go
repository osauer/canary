package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/discover"
)

// These tests pin the reconnect-retry log-volume fix: the connect-failure
// verdicts in connectWithFailover / tryOneHandshake now log once on the
// transition and demote identical repeats to Debug. While the gateway is down
// the daemon rebuilds the connector every cycle; each cycle used to re-emit the
// same "gateway not connected" / "no endpoint usable" WARN lines, flooding
// ibkr-daemon.log (~50k lines over a 13.5h off-hours window). Follow-up #3 to
// the order-status log dedupe (project_daily_pnl_freeze_2026_07_01).
//
// NewLogger installs a process-global pkg/ibkr sink, so these do not call
// t.Parallel(); they assert on their own daemon-logger buffer, which the fake
// attempterFactory keeps free of any pkg/ibkr connector output.

// countLevelLines counts slog text lines at the given level containing needle.
func countLevelLines(buf *bytes.Buffer, level, needle string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "level="+level) && strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

func failingFailoverServer(buf *bytes.Buffer) *Server {
	srv := &Server{
		logger:  NewLogger(buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			port:      ep.Port,
			connectOk: false,
			lastError: "dial tcp 127.0.0.1:7496: connect: connection refused",
		}
	}
	return srv
}

// TestConnectWithFailover_UnreachableVerdictDedupes drives three reconnect
// cycles against a down single-endpoint gateway and asserts each verdict
// surfaces once at WARN and then rides at Debug.
func TestConnectWithFailover_UnreachableVerdictDedupes(t *testing.T) {
	var buf bytes.Buffer
	srv := failingFailoverServer(&buf)
	primary := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for range 3 {
		srv.connectWithFailover(ctx, primary)
	}

	const gwVerdict = "Daemon up but gateway not connected"
	if got := countLevelLines(&buf, "WARN", gwVerdict); got != 1 {
		t.Fatalf("gateway-unreachable verdict logged %d WARN lines over 3 cycles, want exactly 1\n%s", got, buf.String())
	}
	if got := countLevelLines(&buf, "DEBUG", gwVerdict); got != 2 {
		t.Fatalf("gateway-unreachable verdict logged %d DEBUG repeats over 3 cycles, want 2\n%s", got, buf.String())
	}

	// The single-endpoint exhaustion verdict dedupes the same way.
	const exhaustion = "Daemon up but no endpoint usable"
	if got := countLevelLines(&buf, "WARN", exhaustion); got != 1 {
		t.Fatalf("no-endpoint-usable verdict logged %d WARN lines over 3 cycles, want exactly 1\n%s", got, buf.String())
	}
}

// TestConnectWithFailover_RecoveryResetsAndReArms pins the episode lifecycle:
// an outage logs one WARN, a successful handshake logs the recovery bookend and
// clears the dedupe, and a fresh outage then logs a new WARN transition.
func TestConnectWithFailover_RecoveryResetsAndReArms(t *testing.T) {
	var buf bytes.Buffer
	down := true
	srv := &Server{
		logger:  NewLogger(&buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			port:      ep.Port,
			connectOk: !down,
			lastError: "dial tcp 127.0.0.1:7496: connect: connection refused",
		}
	}
	primary := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Outage: two failing cycles → one WARN transition.
	srv.connectWithFailover(ctx, primary)
	srv.connectWithFailover(ctx, primary)
	if got := countLevelLines(&buf, "WARN", "Daemon up but gateway not connected"); got != 1 {
		t.Fatalf("outage should log exactly 1 WARN transition, got %d\n%s", got, buf.String())
	}

	// Gateway returns: success logs the recovery bookend and resets the latch.
	down = false
	srv.connectWithFailover(ctx, primary)
	if !strings.Contains(buf.String(), "Gateway reachable again") {
		t.Fatalf("recovery must log the bookend after an outage:\n%s", buf.String())
	}

	// A second, distinct outage must log a fresh WARN — the reset re-armed it.
	down = true
	buf.Reset()
	srv.connectWithFailover(ctx, primary)
	if got := countLevelLines(&buf, "WARN", "Daemon up but gateway not connected"); got != 1 {
		t.Fatalf("second outage after recovery must log a fresh WARN, got %d\n%s", got, buf.String())
	}
}

// TestPostConnectSetup_NoBookendOnCleanFirstConnect guards against a spurious
// "reachable again" line when the daemon connects without any prior outage.
func TestPostConnectSetup_NoBookendOnCleanFirstConnect(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{
		logger:  NewLogger(&buf, "debug"),
		streams: map[string]context.CancelFunc{},
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}

	srv.postConnectSetup(&fakeAttempter{port: 7496, connectOk: true}, ep)

	if strings.Contains(buf.String(), "Gateway reachable again") {
		t.Fatalf("clean first connect must not log a recovery bookend:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "Connected to IB Gateway") {
		t.Fatalf("postConnectSetup must still log the connect line:\n%s", buf.String())
	}
}

// TestReconnectBackoff_Schedule pins the quiet-period schedule: 1s, 2s, 4s, 8s,
// then capped at reconnectBackoffMax. Without this gate a multi-hour outage
// retried ~2.6/s (66,900 attempts over a 7h outage, 2026-07-08).
func TestReconnectBackoff_Schedule(t *testing.T) {
	t.Parallel()
	cases := []struct {
		streak int
		want   time.Duration
	}{
		{0, reconnectBackoffBase}, // gate exempts streak 0; base is the floor for callers that pass 0/1
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, reconnectBackoffMax}, // 16s would exceed the 15s cap
		{9, reconnectBackoffMax},
		{99, reconnectBackoffMax}, // shift-overflow guard still returns the cap
	}
	for _, c := range cases {
		if got := reconnectBackoff(c.streak); got != c.want {
			t.Errorf("reconnectBackoff(%d) = %s, want %s", c.streak, got, c.want)
		}
	}
	// The cap must stay below the CLI's handshakeWaitBudget (internal/cli
	// status.go, 25s) so a user moving IBKR from Gateway to TWS recovers
	// within a single `canary status`. Duplicated as a literal because that
	// const is unexported in another package.
	const cliHandshakeWaitBudget = 25 * time.Second
	if reconnectBackoffMax >= cliHandshakeWaitBudget {
		t.Fatalf("reconnectBackoffMax (%s) must stay below the CLI handshakeWaitBudget (%s) so a single `canary status` still recovers",
			reconnectBackoffMax, cliHandshakeWaitBudget)
	}
}

// TestReconnectAllowed_GatesWithinWindow pins the gate: streak 0 always fires
// (fresh drop reconnects at once); a nonzero streak is blocked until the
// backoff elapses since the last attempt, then fires again.
func TestReconnectAllowed_GatesWithinWindow(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_700_000_000, 0)
	srv := &Server{now: func() time.Time { return base }}

	// streak 0: always allowed regardless of elapsed time.
	srv.reconnectFailStreak = 0
	srv.lastReconnectAttemptAt = base
	if !srv.reconnectAllowed(base) {
		t.Fatal("streak 0 must always be allowed")
	}

	// streak 1 → 1s window. Still blocked just under; allowed at the boundary.
	srv.reconnectFailStreak = 1
	srv.lastReconnectAttemptAt = base
	if srv.reconnectAllowed(base.Add(999 * time.Millisecond)) {
		t.Fatal("streak 1 must be blocked before its 1s window elapses")
	}
	if !srv.reconnectAllowed(base.Add(time.Second)) {
		t.Fatal("streak 1 must be allowed once its 1s window elapses")
	}

	// streak 5 sits at the cap (15s), not 16s.
	srv.reconnectFailStreak = 5
	srv.lastReconnectAttemptAt = base
	if srv.reconnectAllowed(base.Add(14 * time.Second)) {
		t.Fatal("streak 5 must still be blocked at 14s (cap is 15s)")
	}
	if !srv.reconnectAllowed(base.Add(reconnectBackoffMax)) {
		t.Fatal("streak 5 must be allowed at the 15s cap")
	}
}

// TestReconnectStreak_LifecycleBumpsAndResets pins the state machine that
// feeds the gate: a failed cycle bumps the streak, a cycle cut short by
// shutdown does not, and a successful handshake (postConnectSetup) clears it.
func TestReconnectStreak_LifecycleBumpsAndResets(t *testing.T) {
	var buf bytes.Buffer
	srv := &Server{
		logger:  NewLogger(&buf, "error"),
		streams: map[string]context.CancelFunc{},
		now:     time.Now,
	}
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	srv.noteReconnectOutcome(live, false)
	srv.noteReconnectOutcome(live, false)
	if srv.reconnectFailStreak != 2 {
		t.Fatalf("two failed cycles → streak 2, got %d", srv.reconnectFailStreak)
	}

	// A cycle that ended because the daemon is shutting down must not inflate
	// the streak (it isn't a gateway failure).
	srv.noteReconnectOutcome(cancelled, false)
	if srv.reconnectFailStreak != 2 {
		t.Fatalf("shutdown-cut cycle must not bump streak, got %d", srv.reconnectFailStreak)
	}

	// A connected outcome is a no-op here (postConnectSetup owns the reset).
	srv.noteReconnectOutcome(live, true)
	if srv.reconnectFailStreak != 2 {
		t.Fatalf("connected outcome must not touch streak, got %d", srv.reconnectFailStreak)
	}

	// A completed handshake clears the streak so the next drop reconnects fast.
	srv.postConnectSetup(&fakeAttempter{port: 7496, connectOk: true}, discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15})
	if srv.reconnectFailStreak != 0 {
		t.Fatalf("postConnectSetup must reset streak to 0, got %d", srv.reconnectFailStreak)
	}
}
