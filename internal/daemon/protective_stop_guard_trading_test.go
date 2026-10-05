//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// stopGuardRig is a paper trading server with one Canary-placed GTC trailing
// stop for 100 shares (seedProtectiveTrailJournal) and a fake broker.
type stopGuardRig struct {
	srv       *Server
	mu        sync.Mutex
	now       time.Time
	position  float64
	current   bool
	hand      []ibkrlib.OrderLifecycleEvent
	modified  []*ibkrlib.RawOrder
	cancelled []int
}

func newStopGuardRig(t *testing.T, trading config.Trading) *stopGuardRig {
	t.Helper()
	trading.Mode = config.TradingModePaper
	srv := newOrderPreviewTestServer(t, trading)
	r := &stopGuardRig{srv: srv, now: time.Date(2026, 10, 5, 15, 40, 0, 0, time.UTC), position: 100, current: true}
	srv.now = r.clock
	srv.decisions = newDecisionLog(filepath.Join(t.TempDir(), "daemon.db"))
	t.Cleanup(func() { _ = srv.decisions.Close() })
	seedProtectiveTrailJournal(t, srv, r.now)
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		orders := append([]ibkrlib.OrderLifecycleEvent{{
			Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
			Symbol: "AMD", SecType: "STK", ConID: 4391, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
			TIF: rpc.OrderTIFGTC, TotalQuantity: 100, Remaining: 100, Status: "Submitted",
		}}, r.hand...)
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.now, Orders: orders}, scope, nil
	}
	srv.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.current {
			return nil, false
		}
		return []*ibkrlib.RawPosition{{Account: "DU1234567", Position: r.position,
			Contract: ibkrlib.Contract{ConID: 4391, Symbol: "AMD", SecType: "STK", Currency: "USD"}}}, true
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		after := r.position - float64(qty)
		if action == rpc.OrderActionBuy {
			after = r.position + float64(qty)
		}
		return rpc.OrderPositionImpact{Before: r.position, After: after, Effect: classifyPositionEffect(r.position, after)}, nil
	}
	srv.orderPreviewQuote = fixedPreviewQuote(99.9, 100.1)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		copied := *order
		r.modified = append(r.modified, &copied)
		return nil
	}
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.cancelled = append(r.cancelled, orderID)
		return nil
	}
	return r
}

func (r *stopGuardRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *stopGuardRig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *stopGuardRig) set(position float64, current bool) {
	r.mu.Lock()
	r.position, r.current = position, current
	r.mu.Unlock()
}

// pass runs the guard twice, settle apart, the way two cadence ticks do.
func (r *stopGuardRig) pass(t *testing.T) {
	t.Helper()
	r.srv.runProtectiveStopGuard(t.Context())
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
}

func (r *stopGuardRig) writes() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.modified), len(r.cancelled)
}

func stopGuardJournalOrigin(t *testing.T, srv *Server, eventType string) (orderJournalEvent, bool) {
	t.Helper()
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == eventType {
			return events[i], true
		}
	}
	return orderJournalEvent{}, false
}

// A hand sale leaves 50 shares under Canary's 100-share stop: the guard
// shrinks the stop to 50 through the modify path, keeps the trail, and
// journals its own origin, without allow_stock_short.
func TestProtectiveStopGuardShrinksOwnStopToThePosition(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.set(50, true)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("first sighting wrote %d modifies %d cancels, want none before the settle time", m, c)
	}
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 1 || c != 0 {
		t.Fatalf("writes = %d modifies %d cancels, want one shrink", m, c)
	}
	sent := r.modified[0]
	if sent.OrderID != 1002 || sent.TotalQty != 50 || sent.OrderType != rpc.OrderTypeTRAIL || sent.TIF != rpc.OrderTIFGTC ||
		sent.AuxPrice != 8.5 || sent.TrailStopPrice != 90 {
		t.Fatalf("modify = %+v, want order 1002 at 50 shares with trail 8.5, stop 90, GTC", sent)
	}
	ev, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventModifyRequested)
	if !ok || ev.Origin != rpc.OrderOriginDaemonProtectiveGuard || ev.Quantity != 50 {
		t.Fatalf("journal modify = %+v (found %v), want the guard origin at 50", ev, ok)
	}
	if r.srv.protectiveStopGuardGrant.Load() != nil {
		t.Fatal("grant outlived the write")
	}
}

// A flat position cancels the stop; the cancel is journaled with the guard
// origin and goes through even while trading is frozen.
func TestProtectiveStopGuardCancelsAtZeroEvenWhenFrozen(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.srv.platformSettings = frozenPlatformSettings()
	r.set(0, true)
	r.pass(t)
	if m, c := r.writes(); m != 0 || c != 1 || r.cancelled[0] != 1002 {
		t.Fatalf("writes = %d modifies %v cancels, want one cancel of order 1002", m, r.cancelled)
	}
	ev, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventCancelRequested)
	if !ok || ev.Origin != rpc.OrderOriginDaemonProtectiveGuard {
		t.Fatalf("journal cancel = %+v (found %v), want the guard origin", ev, ok)
	}
}

// The freeze blocks every modify (docs/docs/reference/config.md), so a
// shrink waits while frozen and is recorded as blocked.
func TestProtectiveStopGuardShrinkWaitsWhileFrozen(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.srv.platformSettings = frozenPlatformSettings()
	r.set(50, true)
	r.pass(t)
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("frozen shrink wrote %d modifies %d cancels, want none", m, c)
	}
	lines := readDecisionLines(t, r.srv.decisions.path)
	if len(lines) != 1 || lines[0].Event != "protective_stop_guard.shrink" || lines[0].Outcome != protectiveStopGuardOutcomeBlocked {
		t.Fatalf("decisions = %+v, want one blocked shrink", lines)
	}
}

// Trading disabled (the kill switch) does nothing and records the blocker.
func TestProtectiveStopGuardRespectsTheKillSwitch(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.srv.cfg.Trading.Mode = config.TradingModeDisabled
	r.set(0, true)
	r.pass(t)
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("disabled trading wrote %d modifies %d cancels, want none", m, c)
	}
	lines := readDecisionLines(t, r.srv.decisions.path)
	if len(lines) != 1 || lines[0].Outcome != protectiveStopGuardOutcomeBlocked {
		t.Fatalf("decisions = %+v, want one blocked cancel", lines)
	}
}

// A stale or partial projection never moves an order, and the settle clock
// restarts once evidence returns.
func TestProtectiveStopGuardWaitsForACurrentProjection(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.set(0, true)
	r.srv.runProtectiveStopGuard(t.Context())
	r.set(0, false)
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
	r.set(0, true)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("guard wrote %d modifies %d cancels across an evidence gap, want none", m, c)
	}
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
	if _, c := r.writes(); c != 1 {
		t.Fatalf("cancels = %d, want one after two current passes", c)
	}
}

// A sell stop placed by hand in TWS is reported, never touched.
func TestProtectiveStopGuardLeavesHandOrdersAlone(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.hand = []ibkrlib.OrderLifecycleEvent{{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7777, ClientID: 0, ClientIDPresent: true, Account: "DU1234567",
		Symbol: "AMD", SecType: "STK", ConID: 4391, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
		TotalQuantity: 300, Remaining: 300, Status: "Submitted",
	}}
	r.set(150, true)
	r.pass(t)
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("guard wrote %d modifies %d cancels, want none: Canary's stop fits, the hand order is the owner's", m, c)
	}
	lines := readDecisionLines(t, r.srv.decisions.path)
	if len(lines) != 1 || lines[0].Event != "protective_stop_guard.report" || lines[0].Outcome != protectiveStopGuardOutcomeReported {
		t.Fatalf("decisions = %+v, want one report, logged once", lines)
	}
}

// The guard origin is refused outside a grant, and a grant covers only its
// own order and change.
func TestProtectiveStopGuardOriginNeedsAnExactGrant(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginDaemonProtectiveGuard}); err == nil {
		t.Fatal("guard-origin cancel without a grant was accepted")
	}
	r.srv.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: protectiveStopGuardCancel, OrderRef: "other", ReservedOrderID: 9})
	if _, err := r.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginDaemonProtectiveGuard}); err == nil {
		t.Fatal("guard-origin cancel under another order's grant was accepted")
	}
	r.srv.protectiveStopGuardGrant.Store(nil)
	if _, c := r.writes(); c != 0 {
		t.Fatalf("cancels = %d, want none", c)
	}
}
