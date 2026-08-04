package daemon

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
)

// breadthConnectServer builds the minimum Server that can reach the bulk
// lane's dial gate: a live serverCtx, an installed breadth engine, and a
// clock. The endpoint points at a closed local port so any dial the gate
// does allow is refused immediately instead of reaching a real gateway.
func breadthConnectServer(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{
		cfg:       &config.Resolved{},
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		serverCtx: ctx,
		now:       time.Now,
		endpoint:  discover.Endpoint{Host: "127.0.0.1", Port: 1},
	}
	s.installSubs()
	s.installBreadthEngine()
	if s.breadth == nil {
		t.Fatal("breadth engine not installed")
	}
	return s
}

// TestBreadthGatewayConnector_NilUnstarted pins the contract that
// breadthGatewayConnector returns nil before the bulk-historical
// handshake has landed (or if it failed). The breadth fetcher
// relies on this nil to surface "no gateway" gracefully — a
// regression that returned a stub or fell through to the primary
// connector would mask bulk-connector startup failures and silently
// re-introduce the slot-pool contention this layer exists to avoid.
func TestBreadthGatewayConnector_NilUnstarted(t *testing.T) {
	t.Parallel()
	s := &Server{}
	if got := s.breadthGatewayConnector(); got != nil {
		t.Errorf("breadthGatewayConnector on zero-value Server = %v, want nil", got)
	}
}

// TestBreadthFetcher_WiredToBulkConnector documents the routing
// invariant: installBreadthEngine wires the fetcher's getConn thunk
// to breadthGatewayConnector, not the primary gatewayConnector. The
// fetcher's getConn must return nil here (the bulk connector hasn't
// been started) — proving the closure resolves through the bulk
// path. If a future change accidentally re-binds the fetcher to
// s.gatewayConnector, this test won't catch it (the primary is also
// nil here); the smoke test is the canonical guarantee of correct
// routing against a live gateway. This unit test exists as a
// nil-safety pin for the bulk-path closure specifically.
func TestBreadthFetcher_WiredToBulkConnector(t *testing.T) {
	t.Parallel()
	s := &Server{}
	s.installSubs()
	s.installBreadthEngine()
	if s.breadth == nil {
		t.Fatal("breadth engine not installed")
	}
	// Engine refuses a refresh when the fetcher's getConn returns nil,
	// surfacing a clean error rather than calling into a nil connector.
	// We can't expose the fetcher's getConn from spx.Engine, but we can
	// assert breadthGatewayConnector — the actual closure target — is
	// nil-safe here, which means the engine will see nil for every leg
	// of the first refresh attempt until startBreadthConnector lands.
	if got := s.breadthGatewayConnector(); got != nil {
		t.Errorf("breadthGatewayConnector after installBreadthEngine = %v, want nil (no bulk handshake yet)", got)
	}
}

// The 2026-08-03 regression, pinned. TWS closed both sockets at 23:45;
// the primary reconnected 26 s later but the bulk lane was behind a
// sync.Once, so s.breadthConnector stayed non-nil-but-dead and breadth
// served "no gateway connector" for every S&P name until the daemon was
// restarted 7 h later. A dead connector must therefore be REDIALLABLE:
// if this gate ever refuses again, breadth silently stops publishing for
// the rest of the process lifetime.
func TestClaimBreadthConnect_DeadConnectorIsRedialable(t *testing.T) {
	t.Parallel()
	s := breadthConnectServer(t)
	dead := s.newConnector(discover.Endpoint{Host: "127.0.0.1", Port: 1, ClientID: 16})
	if dead.IsReady() {
		t.Fatal("freshly built connector reports ready without a handshake; test premise is wrong")
	}
	s.breadthConnector = dead

	if !s.claimBreadthConnect() {
		t.Fatal("claimBreadthConnect refused a dead bulk connector — breadth can never recover from a mid-session gateway drop")
	}
}

// The fetch path calls breadthGatewayConnector once per planned symbol —
// 503 times in a cold tick. Exactly one of those may start a dial; the
// rest must fall straight through to nil. Without the in-flight gate the
// fan-out spawns hundreds of parallel handshakes on the same clientID.
func TestClaimBreadthConnect_InFlightGateBlocksStackedDials(t *testing.T) {
	t.Parallel()
	s := breadthConnectServer(t)
	if !s.claimBreadthConnect() {
		t.Fatal("first claim must win the slot")
	}
	for i := range 3 {
		if s.claimBreadthConnect() {
			t.Fatalf("claim %d succeeded while a dial was already in flight", i+2)
		}
	}
	s.releaseBreadthConnect(true)
	if !s.claimBreadthConnect() {
		t.Fatal("slot must be reclaimable after release")
	}
}

// No serverCtx means Start has not run (or has already returned and
// cancelled it) — there is no lifetime to attach a dial goroutine to.
// Mirrors TestTriggerReconnect_NoServerCtxBails for the bulk lane, and
// keeps the zero-value Server that other tests construct safe: this
// refusal fires before the gate reads s.now.
func TestClaimBreadthConnect_NoServerCtxBails(t *testing.T) {
	t.Parallel()
	s := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	if s.claimBreadthConnect() {
		t.Fatal("claimBreadthConnect must refuse when serverCtx is nil")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s.serverCtx = cancelled
	if s.claimBreadthConnect() {
		t.Fatal("claimBreadthConnect must refuse when serverCtx is already cancelled")
	}
}

// A gateway that stays down re-triggers this lane on every breadth tick.
// The failure streak has to widen the quiet period exactly like the
// primary lane's, or a multi-hour outage redials as fast as breadth
// polls — the flood the primary lane's reconnectBackoff was added to
// stop (~50k identical lines over one overnight outage).
func TestClaimBreadthConnect_BackoffThrottlesRepeatDials(t *testing.T) {
	t.Parallel()
	s := breadthConnectServer(t)
	base := time.Now()
	clock := base
	s.now = func() time.Time { return clock }

	if !s.claimBreadthConnect() {
		t.Fatal("first claim must win the slot")
	}
	s.releaseBreadthConnect(false)

	// Inside the streak-1 quiet period: refused.
	clock = base.Add(reconnectBackoff(1) / 2)
	if s.claimBreadthConnect() {
		t.Fatal("claim succeeded inside the backoff quiet period")
	}

	// Past it: allowed again.
	clock = base.Add(reconnectBackoff(1))
	if !s.claimBreadthConnect() {
		t.Fatal("claim refused after the backoff quiet period elapsed")
	}
}

// Streak resets on success so the NEXT genuine drop redials immediately
// rather than inheriting the previous outage's 15 s ceiling. A gateway
// blip that recovers must not leave breadth throttled.
func TestReleaseBreadthConnect_SuccessResetsStreak(t *testing.T) {
	t.Parallel()
	s := breadthConnectServer(t)
	s.breadthConnectFailStreak = 4

	s.releaseBreadthConnect(true)

	s.mu.Lock()
	streak, inFlight := s.breadthConnectFailStreak, s.breadthConnectInFlight
	s.mu.Unlock()
	if streak != 0 {
		t.Errorf("failure streak after a successful dial = %d, want 0", streak)
	}
	if inFlight {
		t.Error("release left the dial slot claimed")
	}
}

// The wiring proof: the fetch path itself must start the recovery. The
// engine has no other route back to a live connector — postConnectSetup
// only re-runs when the PRIMARY reconnects, so a bulk-only socket death
// would otherwise strand breadth exactly as the sync.Once did.
func TestBreadthGatewayConnector_DeadConnectorStartsRedial(t *testing.T) {
	t.Parallel()
	s := breadthConnectServer(t)
	s.breadthConnector = s.newConnector(discover.Endpoint{Host: "127.0.0.1", Port: 1, ClientID: 16})

	if got := s.breadthGatewayConnector(); got != nil {
		t.Fatalf("breadthGatewayConnector returned %v for a dead connector, want nil", got)
	}

	// The attempt stamp is written synchronously inside the claim,
	// before the dial goroutine is spawned, so reading it here is not a
	// race with the goroutine's release.
	s.mu.Lock()
	stamped := s.lastBreadthConnectAttemptAt
	s.mu.Unlock()
	if stamped.IsZero() {
		t.Fatal("a dead bulk connector did not start a redial — breadth cannot recover on its own")
	}
}
