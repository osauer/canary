//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
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
	// broker is what the fake broker's open-order inventory shows of
	// Canary's stop: its quantity, permanent ID and whether it still works.
	broker struct {
		quantity float64
		permID   int
		gone     bool
	}
}

func newStopGuardRig(t *testing.T, trading config.Trading) *stopGuardRig {
	t.Helper()
	return newStopGuardRigPlacedThrough(t, trading, "127.0.0.1:4002")
}

// newStopGuardRigPlacedThrough is the rig with Canary's stop journaled as
// placed through endpoint; the server is connected to 127.0.0.1:4002.
func newStopGuardRigPlacedThrough(t *testing.T, trading config.Trading, endpoint string) *stopGuardRig {
	t.Helper()
	return newStopGuardRigIn(t, trading, t.TempDir(), endpoint, 4002, true)
}

// newStopGuardRigIn is the rig on the order authority in dir, connected to
// 127.0.0.1:port. With seed it journals Canary's stop as placed through
// placed; without, it reads the journal an earlier server wrote in dir, the
// way a restarted daemon does.
func newStopGuardRigIn(t *testing.T, trading config.Trading, dir, placed string, port int, seed bool) *stopGuardRig {
	t.Helper()
	trading.Mode = config.TradingModePaper
	srv := newOrderPreviewTestServerIn(t, trading, dir)
	srv.endpoint.Port = port
	srv.cfg.Gateway.Port = new(port)
	r := &stopGuardRig{srv: srv, now: time.Date(2026, 10, 5, 15, 40, 0, 0, time.UTC), position: 100, current: true}
	r.broker.quantity = 100
	srv.now = r.clock
	srv.decisions = newDecisionLog(filepath.Join(t.TempDir(), "daemon.db"))
	t.Cleanup(func() { _ = srv.decisions.Close() })
	if seed {
		seedProtectiveTrailJournalPlacedThrough(t, srv, r.now, placed)
	}
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var orders []ibkrlib.OrderLifecycleEvent
		if !r.broker.gone {
			orders = append(orders, ibkrlib.OrderLifecycleEvent{
				Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1002, PermID: r.broker.permID, ClientID: 31, ClientIDPresent: true, Account: "DU1234567",
				Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
				TIF: rpc.OrderTIFGTC, TotalQuantity: r.broker.quantity, Remaining: r.broker.quantity, Status: "Submitted",
			})
		}
		orders = append(orders, r.hand...)
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.now, Orders: orders}, scope, nil
	}
	srv.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.current {
			return nil, false
		}
		return []*ibkrlib.RawPosition{{Account: "DU1234567", Position: r.position,
			Contract: ibkrlib.Contract{ConID: 910001, Symbol: "SYW", SecType: "STK", Currency: "USD"}}}, true
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

// A full close sent beside Canary's own full-position stop is admitted
// (owner decision 2026-10-12 19:55 CEST); the guard does nothing while both
// work and the position is still held, and cancels its stop on the first
// settle after the close fills and the projection reads flat.
func TestProtectiveStopGuardCancelsOwnStopAfterAFullCloseFills(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	r.pass(t)
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("a fully held position wrote %d modifies %d cancels, want none", m, c)
	}
	r.set(0, true)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 0 || c != 0 {
		t.Fatalf("the first flat sighting wrote %d modifies %d cancels, want none before the settle time", m, c)
	}
	r.advance(protectiveStopGuardSettle + time.Second)
	r.srv.runProtectiveStopGuard(t.Context())
	if m, c := r.writes(); m != 0 || c != 1 || r.cancelled[0] != 1002 {
		t.Fatalf("writes = %d modifies %v cancels, want one cancel of order 1002 on the first settle after the fill", m, r.cancelled)
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
		Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
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

// stopGuardRows lists the journal's views of Canary's stop.
func stopGuardRows(t *testing.T, srv *Server) []rpc.OrderView {
	t.Helper()
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	var rows []rpc.OrderView
	for _, v := range views {
		if v.OrderRef == "ord-prot" {
			rows = append(rows, v)
		}
	}
	return rows
}

// A stop placed through TWS while the daemon now runs on IB Gateway (the
// 2026-10-19 move; owner decision 2026-10-20 08:00 CEST): the guard's
// change and the owner's cancel both go through, each journaled on the
// order's one row, keyed to the socket it was placed through.
func TestProtectiveStopGuardFollowsAStopPlacedThroughAnotherApp(t *testing.T) {
	t.Parallel()
	r := newStopGuardRigPlacedThrough(t, config.Trading{}, "127.0.0.1:7496")
	r.set(50, true)
	r.pass(t)
	if m, c := r.writes(); m != 1 || c != 0 {
		t.Fatalf("writes = %d modifies %d cancels, want one shrink of the stop placed through TWS; decisions %+v", m, c, readDecisionLines(t, r.srv.decisions.path))
	}
	if sent := r.modified[0]; sent.OrderID != 1002 || sent.TotalQty != 50 {
		t.Fatalf("modify = %+v, want order 1002 at 50 shares", sent)
	}
	modify, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventModifyRequested)
	if !ok || modify.Endpoint != "127.0.0.1:7496" || modify.Quantity != 50 {
		t.Fatalf("journal modify = %+v (found %v), want it keyed to the socket the stop was placed through", modify, ok)
	}
	done, ok := stopGuardJournalOrigin(t, r.srv, orderJournalEventSendCompleted)
	if !ok || done.Endpoint != "127.0.0.1:7496" {
		t.Fatalf("journal send outcome = %+v (found %v), want it keyed to the same socket", done, ok)
	}
	if rows := stopGuardRows(t, r.srv); len(rows) != 1 || rows[0].Endpoint != "127.0.0.1:7496" || rows[0].Quantity != 50 || !rows[0].Open {
		t.Fatalf("rows = %+v, want one open row at 50 on the socket the stop was placed through", rows)
	}

	owner := newStopGuardRigPlacedThrough(t, config.Trading{}, "127.0.0.1:7496")
	if _, err := owner.srv.cancelOrder(t.Context(), rpc.OrderCancelParams{ID: "ord-prot", Origin: rpc.OrderOriginHumanTTY}); err != nil {
		t.Fatalf("owner cancel of the stop placed through TWS: %v", err)
	}
	if _, c := owner.writes(); c != 1 || owner.cancelled[0] != 1002 {
		t.Fatalf("cancels = %v, want order 1002", owner.cancelled)
	}
	cancel, ok := stopGuardJournalOrigin(t, owner.srv, orderJournalEventCancelRequested)
	if !ok || cancel.Endpoint != "127.0.0.1:7496" {
		t.Fatalf("journal cancel = %+v (found %v), want it keyed to the socket the stop was placed through", cancel, ok)
	}
	if rows := stopGuardRows(t, owner.srv); len(rows) != 1 {
		t.Fatalf("rows after the cancel = %d, want one", len(rows))
	}
}
