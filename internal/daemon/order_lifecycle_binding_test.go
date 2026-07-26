package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestOrderLifecycleRegistrationRejectsDelayedOldEpochAfterSamePointerRepublish(t *testing.T) {
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "registration-current", 702, now.Add(-time.Hour))
	srv.orderLifecycleSessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return true }
	connector := &ibkrlib.Connector{}
	srv.mu.Lock()
	srv.connector = connector
	srv.connectorEpoch = 1
	srv.mu.Unlock()

	capturedOldEpoch := make(chan struct{})
	releaseOldRegistration := make(chan struct{})
	var captures atomic.Int32
	srv.orderLifecycleRegisterAfterCapture = func() {
		if captures.Add(1) == 1 {
			close(capturedOldEpoch)
			<-releaseOldRegistration
		}
	}
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		srv.registerOrderLifecycleJournal(connector)
	}()
	<-capturedOldEpoch

	if !srv.withConnectorEvidencePublication(connector, connector, func() {
		srv.connectorEpoch = 3
	}) {
		t.Fatal("same-pointer Connector republication did not apply")
	}
	srv.registerOrderLifecycleJournal(connector)
	close(releaseOldRegistration)
	<-oldDone

	srv.orderLifecycleHandlersMu.Lock()
	defer srv.orderLifecycleHandlersMu.Unlock()
	if len(srv.orderLifecycleHandlers) != 1 {
		t.Fatalf("lifecycle bindings = %d, want one handler for the same Connector", len(srv.orderLifecycleHandlers))
	}
	binding := srv.orderLifecycleHandlers[connector]
	if binding == nil || binding.connectorEpoch.Load() != 3 {
		got := uint64(0)
		if binding != nil {
			got = binding.connectorEpoch.Load()
		}
		t.Fatalf("lifecycle binding epoch = %d, want current republished epoch 3", got)
	}
	before, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head before current receipt: %v", err)
	}
	srv.boundOrderLifecycleHandler(connector, binding)(ibkrlib.OrderLifecycleReceipt{Event: ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, PermID: 702,
		ClientID: 15, ClientIDPresent: true, Status: "Cancelled",
	}})
	after, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head after current receipt: %v", err)
	}
	if after.LastEventSeq != before.LastEventSeq+1 {
		t.Fatalf("current epoch-3 receipt journal head %d -> %d, want exactly one append", before.LastEventSeq, after.LastEventSeq)
	}
	if srv.orderLifecyclePersistenceUncertain.Load() || srv.orderLifecyclePersistenceFailures.Load() != 0 {
		t.Fatalf("current epoch-3 receipt latched uncertainty failures=%d latch=%v", srv.orderLifecyclePersistenceFailures.Load(), srv.orderLifecyclePersistenceUncertain.Load())
	}
}

func TestOrderLifecycleReceiptFromUnpublishedConnectorLeavesJournalAndLatchesUntilCurrentReconcile(t *testing.T) {
	now := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedReconcileGhostRow(t, srv, "stale-a", 701, now.Add(-2*time.Hour))
	// Make socket receipt validation succeed so daemon publication identity is
	// the only reason the retired A callback is rejected.
	srv.orderLifecycleSessionCurrentForTest = func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool { return true }
	connectorA := &ibkrlib.Connector{}
	connectorB := &ibkrlib.Connector{}
	srv.mu.Lock()
	srv.connector = connectorA
	srv.connectorEpoch = 1
	srv.mu.Unlock()
	srv.registerOrderLifecycleJournal(connectorA)
	srv.orderLifecycleHandlersMu.Lock()
	bindingA := srv.orderLifecycleHandlers[connectorA]
	srv.orderLifecycleHandlersMu.Unlock()
	if bindingA == nil {
		t.Fatal("missing lifecycle binding for Connector A")
	}
	handlerA := srv.boundOrderLifecycleHandler(connectorA, bindingA)
	before, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head before stale receipt: %v", err)
	}

	if !srv.withConnectorEvidencePublication(connectorA, connectorB, func() {
		srv.connector = connectorB
		srv.connectorEpoch = 2
	}) {
		t.Fatal("Connector A unpublication did not apply")
	}
	// A has been unpublished but deliberately not stopped. The validator seam
	// makes its receipt otherwise current; only daemon A->B publication drift
	// may prevent this callback from appending.
	handlerA(ibkrlib.OrderLifecycleReceipt{Event: ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 78, PermID: 701,
		ClientID: 15, ClientIDPresent: true, Status: "Cancelled",
	}})
	after, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("journal head after stale receipt: %v", err)
	}
	if after.LastEventSeq != before.LastEventSeq {
		t.Fatalf("stale Connector A receipt changed journal head %d -> %d", before.LastEventSeq, after.LastEventSeq)
	}
	if got := srv.orderLifecyclePersistenceFailures.Load(); got != 1 || !srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatalf("stale receipt failures=%d latch=%v, want 1/true", got, srv.orderLifecyclePersistenceUncertain.Load())
	}

	// Only a complete, stable refresh from the currently published B route can
	// clear the fail-closed latch.
	srv.orderSnapshotFn = reconcileTestSnapshot(true)
	srv.stableBrokerEvidenceForTest = func(binding daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
		srv.mu.Lock()
		current := srv.connector == connectorB && srv.connectorEpoch == 2
		srv.mu.Unlock()
		if !current || binding.connector != connectorB || binding.connectorEpoch != 2 {
			return false, nil
		}
		return true, commit()
	}
	srv.reconcileOrderJournalWithBroker(context.Background())
	if srv.orderLifecyclePersistenceUncertain.Load() {
		t.Fatal("stable complete reconcile from current Connector B did not clear lifecycle persistence latch")
	}
}
