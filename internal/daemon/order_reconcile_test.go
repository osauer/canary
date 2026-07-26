package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// newOrderReconcileTestServer pins a concrete paper scope (DU1234567) so the
// sweep's scope gate opens, and installs a fresh journal.
func newOrderReconcileTestServer(t *testing.T, now time.Time) *Server {
	t.Helper()
	srv := newTestServer(t)
	srv.cfg.Gateway.Account = "DU1234567"
	srv.cfg.Trading.Mode = config.TradingModePaper
	srv.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	srv.now = func() time.Time { return now }
	return srv
}

func seedReconcileGhostRow(t *testing.T, srv *Server, ref string, permID int, at time.Time, extra ...orderJournalEvent) {
	t.Helper()
	base := orderJournalEvent{
		At:              at,
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        ref,
		ReservedOrderID: 78,
		ClientID:        15,
		PermID:          permID,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4001",
		Mode:            "paper",
		Symbol:          "AMD",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        20,
		Status:          "PreSubmitted",
		Remaining:       20,
		SendState:       orderSendStateBrokerAcknowledged,
	}
	if err := srv.orderJournal.Append(base); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
	for _, ev := range extra {
		if ev.Endpoint == "" {
			ev.Endpoint = base.Endpoint
		}
		if ev.ClientID == 0 {
			ev.ClientID = base.ClientID
		}
		if ev.Account == "" {
			ev.Account = base.Account
		}
		if ev.Mode == "" {
			ev.Mode = base.Mode
		}
		if err := srv.orderJournal.Append(ev); err != nil {
			t.Fatalf("seed extra journal event: %v", err)
		}
	}
}

func reconcileTestSnapshot(complete bool, permIDs ...int) func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
	return func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		snap := ibkrlib.OpenOrderSnapshot{Complete: complete, AsOf: time.Now().UTC()}
		for _, id := range permIDs {
			snap.Orders = append(snap.Orders, ibkrlib.OrderLifecycleEvent{
				Type:    ibkrlib.OrderLifecycleEventOpenOrder,
				OrderID: 9000 + id,
				PermID:  id,
			})
		}
		return snap, nil
	}
}

func loadSingleOrderView(t *testing.T, srv *Server, ref string) rpc.OrderView {
	t.Helper()
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatalf("loadOrderViews: %v", err)
	}
	for _, v := range views {
		if v.OrderRef == ref {
			return v
		}
	}
	t.Fatalf("order view %q not found", ref)
	return rpc.OrderView{}
}

func TestReconcileClosesRowAbsentFromCompleteSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-1", 555, now.Add(-2*time.Hour))
	srv.orderSnapshotFn = reconcileTestSnapshot(true, 111, 222)

	srv.reconcileOrderJournalWithBroker(context.Background())

	view := loadSingleOrderView(t, srv, "ghost-1")
	if view.Open {
		t.Fatalf("row still open after reconcile: %+v", view)
	}
	if view.LifecycleStatus != rpc.OrderLifecycleClosedReconciled {
		t.Fatalf("lifecycle = %q, want closed_reconciled", view.LifecycleStatus)
	}
	if view.SendState != orderSendStateTerminal {
		t.Fatalf("send_state = %q, want terminal", view.SendState)
	}
	if !orderLifecycleStatusIsTerminal(view.LifecycleStatus) {
		t.Fatal("closed_reconciled must be terminal")
	}
	// Sticky last-known broker Status stays visible on the view for the
	// audit trail; it must not resurrect the row.
	if view.Status != "PreSubmitted" {
		t.Fatalf("sticky status = %q, want PreSubmitted retained", view.Status)
	}

	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	last := events[len(events)-1]
	if last.Type != orderJournalEventReconciledAbsent || last.PermID != 555 || last.Status != "" {
		t.Fatalf("reconcile event = %+v", last)
	}

	// Idempotent: a second sweep appends nothing (the row is terminal).
	srv.reconcileOrderJournalWithBroker(context.Background())
	after, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents after resweep: %v", err)
	}
	if len(after) != len(events) {
		t.Fatalf("resweep appended events: %d -> %d", len(events), len(after))
	}
}

func TestReconcileClosesWedgedCancelRequestedRow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-wedged", 556, now.Add(-3*time.Hour), orderJournalEvent{
		At:              now.Add(-2 * time.Hour),
		Type:            orderJournalEventCancelRequested,
		OrderRef:        "ghost-wedged",
		ReservedOrderID: 78,
		ClientID:        15,
		Account:         "DU1234567",
		Mode:            "paper",
		SendState:       orderSendStateSendAttempted,
		Message:         "live broker cancel attempted",
	})
	before := loadSingleOrderView(t, srv, "ghost-wedged")
	if !before.Open || before.CancelEligible {
		t.Fatalf("precondition: wedged row should be open and cancel-ineligible, got %+v", before)
	}
	srv.orderSnapshotFn = reconcileTestSnapshot(true)

	srv.reconcileOrderJournalWithBroker(context.Background())

	view := loadSingleOrderView(t, srv, "ghost-wedged")
	if view.Open || view.LifecycleStatus != rpc.OrderLifecycleClosedReconciled {
		t.Fatalf("wedged row not closed: %+v", view)
	}
}

func TestReconcileHeadCASRejectsLateJournalMutation(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-cas", 558, now.Add(-2*time.Hour))
	srv.orderSnapshotFn = reconcileTestSnapshot(true)
	srv.orderReconcileBeforeCommit = func() {
		if err := srv.orderJournal.Append(orderJournalEvent{
			At: now, Type: orderJournalEventStatusUpdated, OrderRef: "ghost-cas",
			ReservedOrderID: 78, ClientID: 15, PermID: 558, Account: "DU1234567",
			Endpoint: "127.0.0.1:4001", Mode: "paper", Status: "Submitted",
			Remaining: 20, SendState: orderSendStateBrokerAcknowledged,
		}); err != nil {
			t.Fatal(err)
		}
	}

	srv.reconcileOrderJournalWithBroker(t.Context())

	if view := loadSingleOrderView(t, srv, "ghost-cas"); !view.Open || view.LifecycleStatus == rpc.OrderLifecycleClosedReconciled {
		t.Fatalf("stale reconciliation closed a later journal head: %+v", view)
	}
}

func TestReconcileClearsPersistenceLatchOnlyAfterStableCompleteRefresh(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-latch", 559, now.Add(-time.Minute))
	srv.orderLifecyclePersistenceFailures.Store(1)
	srv.orderLifecyclePersistenceUncertain.Store(true)
	srv.orderSnapshotFn = reconcileTestSnapshot(true)

	srv.reconcileOrderJournalWithBroker(t.Context())

	if srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("stable complete reconciliation did not clear lifecycle persistence latch")
	}
	if view := loadSingleOrderView(t, srv, "ghost-latch"); view.Open || view.LifecycleStatus != rpc.OrderLifecycleClosedReconciled {
		t.Fatalf("latched reconciliation retained broker-absent row inside ordinary grace window: %+v", view)
	}
}

func TestReconcileDoesNotClearPersistenceLatchAfterAnotherFailure(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "present-latch", 560, now.Add(-time.Hour))
	srv.orderLifecyclePersistenceFailures.Store(1)
	srv.orderLifecyclePersistenceUncertain.Store(true)
	srv.orderSnapshotFn = func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		srv.orderLifecyclePersistenceFailures.Add(1)
		return reconcileTestSnapshot(true, 560)(context.Background())
	}

	srv.reconcileOrderJournalWithBroker(t.Context())

	if !srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("reconciliation cleared latch despite a failure during the refresh")
	}
}

func TestReconcileDoesNotLoseFailureRacingFinalLatchClear(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "present-final-window", 561, now.Add(-time.Hour))
	srv.orderLifecyclePersistenceFailures.Store(1)
	srv.orderLifecyclePersistenceUncertain.Store(true)
	srv.orderSnapshotFn = reconcileTestSnapshot(true, 561)
	srv.orderReconcileBeforeLatchClear = func() {
		srv.orderLifecyclePersistenceFailures.Add(1)
		srv.orderLifecyclePersistenceUncertain.Store(true)
	}

	srv.reconcileOrderJournalWithBroker(t.Context())

	if !srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("reconciliation lost a persistence failure racing the final latch clear")
	}
}

func TestReconcileLeavesPresentYoungUnackedAndOffScopeRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	// Present at broker.
	seedReconcileGhostRow(t, srv, "present-1", 700, now.Add(-2*time.Hour))
	// Inside the grace window.
	seedReconcileGhostRow(t, srv, "young-1", 701, now.Add(-time.Minute))
	// Never broker-accepted (no PermID).
	seedReconcileGhostRow(t, srv, "unacked-1", 0, now.Add(-2*time.Hour))
	// Other account scope.
	off := orderJournalEvent{
		At:              now.Add(-2 * time.Hour),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "offscope-1",
		ReservedOrderID: 91,
		ClientID:        15,
		PermID:          702,
		Account:         "U1234567",
		Endpoint:        "127.0.0.1:4001",
		Mode:            "live",
		Symbol:          "IBM",
		SecType:         "STK",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        5,
		Status:          "PreSubmitted",
		Remaining:       5,
		SendState:       orderSendStateBrokerAcknowledged,
	}
	if err := srv.orderJournal.Append(off); err != nil {
		t.Fatalf("seed off-scope: %v", err)
	}
	srv.orderSnapshotFn = reconcileTestSnapshot(true, 700)

	srv.reconcileOrderJournalWithBroker(context.Background())

	for _, ref := range []string{"present-1", "young-1", "unacked-1", "offscope-1"} {
		if view := loadSingleOrderView(t, srv, ref); !view.Open {
			t.Fatalf("row %q must stay open, got %+v", ref, view)
		}
	}
}

func TestReconcileSkipsIncompleteSnapshotAndNonConcreteScope(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "ghost-1", 555, now.Add(-2*time.Hour))

	// Incomplete snapshot proves nothing.
	srv.orderSnapshotFn = reconcileTestSnapshot(false)
	srv.reconcileOrderJournalWithBroker(context.Background())
	if view := loadSingleOrderView(t, srv, "ghost-1"); !view.Open {
		t.Fatalf("incomplete snapshot closed a row: %+v", view)
	}

	// Non-concrete scope must not sweep at all (unfiltered scope would
	// match every account's rows).
	srv.cfg.Gateway.Account = ""
	called := false
	srv.orderSnapshotFn = func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		called = true
		return ibkrlib.OpenOrderSnapshot{Complete: true}, nil
	}
	srv.reconcileOrderJournalWithBroker(context.Background())
	if called {
		t.Fatal("sweep must not snapshot under a non-concrete scope")
	}
	if view := loadSingleOrderView(t, srv, "ghost-1"); !view.Open {
		t.Fatalf("non-concrete scope closed a row: %+v", view)
	}
}

func TestOpenOrderSnapshotContainsMatching(t *testing.T) {
	t.Parallel()
	snap := ibkrlib.OpenOrderSnapshot{Complete: true, Orders: []ibkrlib.OrderLifecycleEvent{
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 78, ClientID: 15, ClientIDPresent: true, PermID: 555},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 91, ClientID: 0, ClientIDPresent: true},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 92, ClientID: 0},
	}}
	if !openOrderSnapshotContains(snap, rpc.OrderView{PermID: 555}) {
		t.Fatal("perm-id match failed")
	}
	if openOrderSnapshotContains(snap, rpc.OrderView{ReservedOrderID: 78, ClientID: 15}) {
		t.Fatal("known snapshot perm-id must not fall back to order-id+client")
	}
	if !openOrderSnapshotContains(snap, rpc.OrderView{ReservedOrderID: 91, ClientID: 0}) {
		t.Fatal("explicit client-0 order-id match failed")
	}
	if openOrderSnapshotContains(snap, rpc.OrderView{ReservedOrderID: 92, ClientID: 0}) {
		t.Fatal("legacy callback without client provenance must fail closed")
	}
	if openOrderSnapshotContains(snap, rpc.OrderView{PermID: 999, ReservedOrderID: 12}) {
		t.Fatal("absent order matched")
	}
	if openOrderSnapshotContains(snap, rpc.OrderView{ReservedOrderID: 78, ClientID: 15, PermID: 998}) {
		t.Fatal("same order id with disagreeing permanent ids matched")
	}
	if openOrderSnapshotContains(snap, rpc.OrderView{ReservedOrderID: 91, ClientID: 1}) {
		t.Fatal("client 0 must not act as a wildcard")
	}
}

func TestAppendOrderLifecycleEventDropsUnmatchedBrokerCallbacks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "known-1", 555, now.Add(-time.Hour))
	baseline, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}

	// Unmatched openOrder (e.g. a manual TWS order in a snapshot): dropped.
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 4242, PermID: 999,
		Symbol: "MSFT", SecType: "STK", Status: "Submitted",
	})
	// Unmatched orderStatus: dropped.
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 4242, PermID: 999,
		Status: "Submitted", Remaining: 1,
	})
	// A colliding order ID must not override two known, disagreeing permanent
	// identities. This callback belongs to another broker order and is dropped.
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, ClientID: 15, ClientIDPresent: true, PermID: 999,
		Status: "Submitted", Remaining: 1,
	})
	// A legacy callback without explicit client provenance cannot borrow the
	// current route's client ID and fall back to a colliding local order ID.
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78,
		Status: "Submitted", Remaining: 1,
	})
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != len(baseline) {
		t.Fatalf("unmatched broker callbacks were journaled: %d -> %d", len(baseline), len(events))
	}

	// Matched orderStatus (by reserved order id + client): journaled.
	srv.appendOrderLifecycleEvent(ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, ClientID: 15, PermID: 555,
		Status: "PreSubmitted", Remaining: 20,
	})
	events, err = srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != len(baseline)+1 {
		t.Fatalf("matched broker callback was not journaled: %d -> %d", len(baseline), len(events))
	}
}
