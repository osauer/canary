package daemon

import (
	"context"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
	"testing"
	"time"
)

func TestAmbiguousPreviewStatusLatchesEvidenceWithoutInventingCancellation(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "synthetic-preview", 701, now.Add(-time.Hour))
	connector := &ibkrlib.Connector{}
	srv.connector = connector
	srv.connectorEpoch = 1
	srv.orderLifecycleSessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return true }
	srv.registerOrderLifecycleJournal(connector)
	binding := srv.orderLifecycleHandlers[connector]
	before, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	srv.boundOrderLifecycleHandler(connector, binding)(ibkrlib.OrderLifecycleReceipt{Uncertain: true, Event: ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, PermID: 701, ClientID: 15, ClientIDPresent: true, Status: "Cancelled"}})
	after, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	if after.LastEventSeq != before.LastEventSeq || !srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("ambiguous preview status became a journal fact or failed to invalidate evidence")
	}
	srv.orderSnapshotFn = reconcileTestSnapshot(true)
	srv.stableBrokerEvidenceForTest = func(_ daemonBrokerEvidenceBinding, commit func() error) (bool, error) { return true, commit() }
	srv.reconcileOrderJournalWithBroker(context.Background())
	if srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("complete current reconciliation did not clear uncertainty")
	}
	views, _, err := srv.loadOrderViews()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.OrderRef == "synthetic-preview" && (v.Open || v.Status == "Cancelled") {
			t.Fatalf("absence invented a cancellation or stayed open: %+v", v)
		}
	}
}
