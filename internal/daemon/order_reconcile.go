package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// errOrderJournalHeadUnstable distinguishes "the journal kept advancing under
// the reload" from real read failures: the former is a liveness race worth a
// bounded retry, never evidence the journal is unreadable.
var errOrderJournalHeadUnstable = errors.New("order journal changed during reconciliation reload")

const (
	// orderReconcileGrace keeps the sweep away from rows with recent journal
	// activity so it never races an in-flight place, modify, or cancel.
	orderReconcileGrace = 10 * time.Minute
	// orderReconcileSnapshotWait bounds the reqAllOpenOrders round trip; on
	// expiry the snapshot is incomplete and the sweep proves nothing.
	orderReconcileSnapshotWait = 15 * time.Second
	// orderReconcileConnectDelay lets the gateway's own post-connect order
	// re-binds land (and refresh journal rows) before the first sweep.
	orderReconcileConnectDelay = 45 * time.Second
	// orderReconcileInterval paces the standing sweep while open rows exist.
	orderReconcileInterval = 30 * time.Minute
)

// reconcileOrderJournalWithBroker closes journal rows the broker no longer
// reports. IBKR does not replay missed terminal callbacks (a cancel that
// landed while the daemon was offline is gone for good), so absence from a
// COMPLETE open-order snapshot is the only broker-truth signal a stale row
// will ever get. The sweep is read-only at the broker: its single wire
// interaction is the snapshot request; rows are closed by appending terminal
// reconciled-absent events to the append-only journal, never by rewriting.
//
// Fail-safe rules: no complete snapshot → no action; only rows in the
// current concrete account/mode scope; only rows the broker once accepted
// (PermID set); never rows with journal activity inside the grace window.
// Rows still present in the snapshot are refreshed for free — their
// openOrder callbacks flow through the normal lifecycle journal handler,
// which also restores send_state on rows wedged by a missed cancel ack.
func (s *Server) reconcileOrderJournalWithBroker(ctx context.Context) {
	if s == nil || s.orderJournal == nil {
		return
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		// An unfiltered scope matches every journal row; sweeping against a
		// single connection's snapshot would close other accounts' rows.
		return
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		s.warnf("order reconcile: load order views: %v", err)
		return
	}
	now := s.orderNow()
	var candidates []rpc.OrderView
	for _, v := range views {
		if !v.Open || v.PermID == 0 || !orderViewMatchesBrokerScope(v, scope) {
			continue
		}
		if v.UpdatedAt.IsZero() || now.Sub(v.UpdatedAt) < orderReconcileGrace {
			continue
		}
		candidates = append(candidates, v)
	}
	persistenceUncertain := s.orderLifecyclePersistenceUncertain.Load()
	if len(candidates) == 0 && !persistenceUncertain {
		return
	}

	s.mu.Lock()
	connector, connectorEpoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	initialSession := ibkrlib.ConnectorSessionBinding{}
	if connector != nil {
		initialSession, _ = connector.CaptureSession()
	}
	failureStart := s.orderLifecyclePersistenceFailures.Load()
	snap, err := s.snapshotOpenOrdersFrom(ctx, connector)
	if err != nil && !snap.Complete {
		s.warnf("order reconcile: open-order snapshot unavailable: %v", err)
		return
	}
	if !snap.Complete {
		s.warnf("order reconcile: open-order snapshot incomplete; skipping sweep")
		return
	}

	// Snapshot callbacks are journaled synchronously before openOrderEnd. Reload
	// the heads after completion, and bind a compare-and-append to the exact
	// event frontier so a later local intent/modify cannot be closed from the
	// pre-snapshot view.
	currentViews, _, head, err := s.loadOrderViewsAtStableHead()
	if err != nil {
		s.warnf("order reconcile: reload current order heads: %v", err)
		return
	}
	now = s.orderNow()
	var events []orderJournalEvent
	for _, v := range currentViews {
		if !v.Open || v.PermID == 0 || !orderViewMatchesBrokerScope(v, scope) {
			continue
		}
		if !persistenceUncertain && (v.UpdatedAt.IsZero() || now.Sub(v.UpdatedAt) < orderReconcileGrace) {
			continue
		}
		if openOrderSnapshotContains(snap, v) {
			continue
		}
		ev := orderJournalEventFromView(v, orderJournalEventReconciledAbsent, s.orderNow())
		// orderJournalEventFromView copies neither PermID nor a clean Status:
		// carry the PermID as the row's stable identity key, and carry no
		// broker Status so the sticky earlier Status stays visibly last-known
		// while the reconciled-absent lifecycle branch closes the row.
		ev.PermID = v.PermID
		ev.Status = ""
		ev.SendState = orderSendStateTerminal
		ev.Message = "broker open-order snapshot did not include this order; closed by reconciliation. Final broker state unknown (cancelled or filled outside daemon view) — confirm against broker statements."
		events = append(events, ev)
	}
	receiptSession := snap.Session
	if s.orderSnapshotFn != nil && receiptSession == (ibkrlib.ConnectorSessionBinding{}) {
		receiptSession = initialSession
	}
	commit := func() error {
		if len(events) > 0 {
			if err := s.orderJournal.AppendAllAtHead(events, head); err != nil {
				return err
			}
		} else {
			currentHead, err := s.orderJournal.AuthorityHead()
			if err != nil {
				return err
			}
			if currentHead.LastEventSeq != head {
				return fmt.Errorf("order journal head changed from %d to %d", head, currentHead.LastEventSeq)
			}
		}
		if persistenceUncertain && s.orderLifecyclePersistenceFailures.Load() == failureStart {
			if s.orderReconcileBeforeLatchClear != nil {
				s.orderReconcileBeforeLatchClear()
			}
			s.orderLifecyclePersistenceUncertain.Store(false)
			// A callback failure publishes generation first, then the latch. If
			// it raced the clear above, the generation recheck restores the latch;
			// if it starts afterward, its own Store(true) wins.
			if s.orderLifecyclePersistenceFailures.Load() != failureStart {
				s.orderLifecyclePersistenceUncertain.Store(true)
			}
		}
		return nil
	}
	if s.orderReconcileBeforeCommit != nil {
		s.orderReconcileBeforeCommit()
	}
	if connector != nil {
		var brokerBinding ibkrlib.BrokerEvidenceBinding
		if s.orderSnapshotFn != nil && s.stableBrokerEvidenceForTest != nil {
			// Deterministic tests can model a complete receipt from the current
			// publication without constructing pkg/ibkr's opaque socket token.
			// Production never installs either seam and must capture the exact
			// live Connector frontier below.
			brokerBinding = ibkrlib.BrokerEvidenceBinding{
				Session: receiptSession, OrderLifecycleGeneration: snap.Generation,
			}
		} else {
			var ok bool
			brokerBinding, ok = connector.CaptureBrokerEvidence()
			if !ok || brokerBinding.Session != receiptSession || brokerBinding.OrderLifecycleGeneration != snap.Generation {
				return
			}
		}
		committed, commitErr := s.withStableBrokerEvidence(daemonBrokerEvidenceBinding{
			scope: scope, connector: connector, connectorEpoch: connectorEpoch, broker: brokerBinding,
		}, commit)
		if commitErr != nil {
			s.warnf("order reconcile: append reconciled-absent events: %v", commitErr)
			return
		}
		if !committed {
			return
		}
	} else {
		// Test-only snapshot seams do not carry a live Connector barrier. Keep
		// the production path strict while retaining deterministic store tests.
		if s.orderSnapshotFn == nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
			return
		}
		if err := commit(); err != nil {
			s.warnf("order reconcile: append reconciled-absent events: %v", err)
			return
		}
	}
	s.infof("order reconcile: closed %d journal row(s) absent from a complete broker open-order snapshot (%d snapshot orders, %d candidates)", len(events), len(snap.Orders), len(candidates))
}

// orderJournalHeadSettle is how long a stable-head reload waits before
// rereading after it caught the journal mid-write. Back-to-back rereads race
// the same event burst they just observed; one beat of patience makes the
// second attempt meaningful while staying negligible against every caller's
// request budget.
const orderJournalHeadSettle = 25 * time.Millisecond

func (s *Server) loadOrderViewsAtStableHead() ([]rpc.OrderView, map[string][]rpc.OrderEvent, int64, error) {
	for attempt := range 3 {
		if attempt > 0 {
			time.Sleep(orderJournalHeadSettle)
		}
		before, err := s.orderJournal.AuthorityHead()
		if err != nil {
			return nil, nil, 0, err
		}
		views, eventsByKey, err := s.loadOrderViews()
		if err != nil {
			return nil, nil, 0, err
		}
		after, err := s.orderJournal.AuthorityHead()
		if err != nil {
			return nil, nil, 0, err
		}
		if before.LastEventSeq == after.LastEventSeq {
			return views, eventsByKey, after.LastEventSeq, nil
		}
	}
	return nil, nil, 0, errOrderJournalHeadUnstable
}

func (s *Server) snapshotOpenOrdersFrom(ctx context.Context, c *ibkrlib.Connector) (ibkrlib.OpenOrderSnapshot, error) {
	if s.orderSnapshotFn != nil {
		return s.orderSnapshotFn(ctx)
	}
	if c == nil || !c.IsConnected() {
		return ibkrlib.OpenOrderSnapshot{}, ibkrlib.ErrIBKRUnavailable
	}
	snapCtx, cancel := context.WithTimeout(ctx, orderReconcileSnapshotWait)
	defer cancel()
	return c.SnapshotOpenOrders(snapCtx)
}

// openOrderSnapshotContains reports whether the snapshot still carries the
// journal row's broker order. A known PermID is account-wide authority and
// must match exactly; it can never fall back to a colliding client-local order
// ID. Fallback is permitted only when neither side has a PermID and the
// snapshot explicitly carried the exact client ID (including valid client 0).
func openOrderSnapshotContains(snap ibkrlib.OpenOrderSnapshot, view rpc.OrderView) bool {
	_, _, ok := openOrderSnapshotMatch(snap, view)
	return ok
}

func openOrderSnapshotMatch(snap ibkrlib.OpenOrderSnapshot, view rpc.OrderView) (int, ibkrlib.OrderLifecycleEvent, bool) {
	for i, order := range snap.Orders {
		if openOrderSnapshotEventMatches(order, view) {
			return i, order, true
		}
	}
	return -1, ibkrlib.OrderLifecycleEvent{}, false
}

func openOrderSnapshotEventMatches(order ibkrlib.OrderLifecycleEvent, view rpc.OrderView) bool {
	if view.PermID != 0 || order.PermID != 0 {
		return view.PermID != 0 && order.PermID != 0 && order.PermID == view.PermID
	}
	return view.ReservedOrderID > 0 && order.OrderID == view.ReservedOrderID &&
		order.ClientIDPresent && order.ClientID == view.ClientID
}

// runOrderReconcileLoop is the standing sweep, launched once per daemon
// lifetime from postConnectSetup. The per-connect settle-delay one-shots are
// scheduled separately on every successful (re)connect.
func (s *Server) runOrderReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(orderReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileOrderJournalWithBroker(ctx)
		}
	}
}
