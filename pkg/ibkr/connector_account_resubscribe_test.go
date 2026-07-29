package ibkr

import (
	"bufio"
	"bytes"
	"testing"
	"time"
)

// These tests pin the decision function of the dead-portfolio-stream
// self-heal (maybeResubscribeAccountUpdates). The TWS account-updates
// stream occasionally fails to start after a rapid reconnect (observed
// 2026-06-11: one boot delivered no msgPortfolioValue at all while quotes
// and account summary flowed normally), so CachedPositions resubscribes
// behind an empty read. Until the 2026-06-12 pre-release review only the
// leaf predicate accountSummaryShowsPositions was covered — the trigger,
// throttle, and guards shipped untested.

// newAcctResubscribeRig returns a connector wired to a fake connected
// gateway whose outbound frames land in the returned buffer.
func newAcctResubscribeRig(t *testing.T) (*Connector, *Connection, *safeBuffer) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	var out safeBuffer
	conn := NewConnection(nil)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	conn.writer = bufio.NewWriter(&out)
	c.conn = conn
	t.Cleanup(conn.rateLimiter.Stop)
	return c, conn, &out
}

// TestCachedPositionsHealsDeadPortfolioStream pins the heal end to end:
// an empty position cache while the account summary reports gross position
// value sends exactly one reqAcctData frame, polls inside the throttle
// window stay quiet, and the window boundary (>=) re-arms the attempt. An
// inverted staleness comparison would fail twice here — firing inside the
// window (and self-perpetuating, since every RequestAccountUpdates resets
// the stamp it is compared against) and staying dead at the boundary.
func TestCachedPositionsHealsDeadPortfolioStream(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.account = "DU1234567"
	conn.accountSummary["GrossPositionValue"] = "250000.00"
	conn.accountMu.Unlock()

	// Far from wall-clock time so a half-seamed path (one side stamping
	// real time.Now) cannot land near the virtual timeline and pass.
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	c.acctUpdatesNow = func() time.Time { return now }

	acctFrame := conn.encodeMsg(reqAcctData, "2", "1", "DU1234567")
	frames := func() int { return bytes.Count(out.Bytes(), acctFrame) }

	positions, err := c.CachedPositions()
	if err != nil || len(positions) != 0 {
		t.Fatalf("CachedPositions = %v, %v; want empty, nil", positions, err)
	}
	if got := frames(); got != 1 {
		t.Fatalf("first empty poll sent %d reqAcctData frames, want exactly 1", got)
	}

	now = now.Add(acctUpdatesResubscribeThrottle - time.Second)
	if _, err := c.CachedPositions(); err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if got := frames(); got != 1 {
		t.Fatalf("poll inside throttle window sent %d reqAcctData frames, want 1", got)
	}

	now = now.Add(time.Second) // exactly acctUpdatesResubscribeThrottle after the subscribe
	if _, err := c.CachedPositions(); err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if got := frames(); got != 2 {
		t.Fatalf("poll at throttle boundary sent %d reqAcctData frames, want 2", got)
	}
}

func TestCachedPositionsWithHealthRepairsScopeConflictWithNonemptyCache(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.account = "DU1234567"
	conn.accountMu.Unlock()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	c.acctUpdatesNow = func() time.Time { return now }
	conn.resetPortfolioStreamHealth("DU1234567", now.Add(-time.Minute))
	conn.handlePortfolioValue([]string{
		"7", "8", "4391", "AMD", "STK", "", "0", "", "1", "NASDAQ", "USD", "AMD", "AMD",
		"20", "100", "2000", "90", "200", "0", "DU1234567",
	})
	if !conn.completePortfolioDownload("DU1234567", now.Add(-30*time.Second)) {
		t.Fatal("initial scoped portfolio completion was rejected")
	}
	if conn.acceptPortfolioAccountFrame("U1234567", now) {
		t.Fatal("foreign portfolio frame was accepted")
	}

	acctFrame := conn.encodeMsg(reqAcctData, "2", "1", "DU1234567")
	frames := func() int { return bytes.Count(out.Bytes(), acctFrame) }
	positions, health, err := c.CachedPositionsWithHealth()
	if err != nil || len(positions) != 1 {
		t.Fatalf("CachedPositionsWithHealth = %v, %+v, %v", positions, health, err)
	}
	if frames() != 1 || !health.ScopeConflictAt.IsZero() || health.RequestedAt.IsZero() || !health.InitialCompletedAt.IsZero() {
		t.Fatalf("first scope repair health=%+v frames=%d", health, frames())
	}

	// A repeated foreign frame re-latches the typed conflict. The throttle
	// keeps the immediate read from looping, then re-arms at its boundary.
	if conn.acceptPortfolioAccountFrame("U1234567", now.Add(time.Second)) {
		t.Fatal("repeated foreign portfolio frame was accepted")
	}
	_, held, err := c.CachedPositionsWithHealth()
	if err != nil || held.ScopeConflictAt.IsZero() || frames() != 1 {
		t.Fatalf("throttled repeated conflict health=%+v frames=%d err=%v", held, frames(), err)
	}

	now = now.Add(acctUpdatesResubscribeThrottle)
	_, repaired, err := c.CachedPositionsWithHealth()
	if err != nil || frames() != 2 || !repaired.ScopeConflictAt.IsZero() || repaired.RequestedAt.IsZero() || !repaired.InitialCompletedAt.IsZero() {
		t.Fatalf("repeated conflict repair health=%+v frames=%d err=%v", repaired, frames(), err)
	}
}

func TestCachedPositionsKeepsZeroValueStockPositionsVisible(t *testing.T) {
	c, conn, _ := newAcctResubscribeRig(t)
	conn.positionsMu.Lock()
	conn.positions = map[string]*RawPosition{
		"AMD": {
			Contract:    Contract{ConID: 4391, Symbol: "AMD", SecType: "STK", Currency: "USD"},
			Position:    20,
			MarketPrice: 100,
			MarketValue: 2000,
		},
		"ZVZZT": {
			Contract:      Contract{ConID: 12345, Symbol: "ZVZZT", SecType: "STK", Currency: "USD"},
			Position:      20000,
			MarketPrice:   0,
			MarketValue:   0,
			AverageCost:   0.33,
			UnrealizedPNL: -6600,
		},
	}
	conn.positionsMu.Unlock()

	positions, err := c.CachedPositions()
	if err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("positions len = %d, want AMD and ZVZZT: %+v", len(positions), positions)
	}
	if c.IsSymbolInactive("ZVZZT") {
		t.Fatal("zero-value stock position must stay visible and must not be marked inactive without broker definition evidence")
	}
	c.contractMu.RLock()
	_, cached := c.contractCache["ZVZZT"]
	c.contractMu.RUnlock()
	if cached {
		t.Fatal("zero-value stock position must not seed contract cache for quote routing")
	}
}

func TestCachedPositionsKeepsInactiveHeldZeroValueStockVisible(t *testing.T) {
	c, conn, _ := newAcctResubscribeRig(t)
	c.markSymbolInactive("ZVZZT", "No security definition has been found for the request")
	conn.positionsMu.Lock()
	conn.positions = map[string]*RawPosition{
		"ZVZZT": {
			Contract:      Contract{ConID: 12345, Symbol: "ZVZZT", SecType: "STK", Currency: "USD"},
			Position:      20000,
			MarketPrice:   0,
			MarketValue:   0,
			AverageCost:   0.33,
			UnrealizedPNL: -6600,
		},
	}
	conn.positionsMu.Unlock()

	positions, err := c.CachedPositions()
	if err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if len(positions) != 1 || positions[0].Contract.Symbol != "ZVZZT" {
		t.Fatalf("held inactive zero-value stock should remain visible, got %+v", positions)
	}
	c.contractMu.RLock()
	_, cached := c.contractCache["ZVZZT"]
	c.contractMu.RUnlock()
	if cached {
		t.Fatal("held inactive zero-value stock must not seed contract cache for quote routing")
	}
}

// TestMaybeResubscribeAccountUpdatesRequiresConnection pins the connected
// guard: no bytes reach a disconnected wire, and a torn-down connection
// (conn == nil, possible between CachedPositions' connectivity check
// and the heal) must not panic the poll path on the GetAccountSummary
// dereference.
func TestMaybeResubscribeAccountUpdatesRequiresConnection(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.accountSummary["GrossPositionValue"] = "250000.00"
	conn.accountMu.Unlock()
	conn.status = StatusDisconnected

	c.maybeResubscribeAccountUpdates()
	if got := out.Len(); got != 0 {
		t.Fatalf("disconnected resubscribe wrote %d bytes to the wire, want 0", got)
	}

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	c.maybeResubscribeAccountUpdates()
}

// TestMaybeResubscribeAccountUpdatesSkipsFlatAccount pins the
// genuinely-flat case: with the throttle long expired (zero stamp), only
// the absent gross position value stands between a constantly-polled empty
// cache and a resubscribe loop — a flat account must never trigger one.
func TestMaybeResubscribeAccountUpdatesSkipsFlatAccount(t *testing.T) {
	c, conn, out := newAcctResubscribeRig(t)
	conn.accountMu.Lock()
	conn.account = "DU1234567"
	conn.accountSummary["GrossPositionValue"] = "0"
	conn.accountSummary["NetLiquidation"] = "100000.00"
	conn.accountMu.Unlock()

	if _, err := c.CachedPositions(); err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if got := out.Len(); got != 0 {
		t.Fatalf("flat account put %d bytes on the wire, want none", got)
	}
}
