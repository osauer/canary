package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func newAlertShadowOrderEvidenceServer(t *testing.T, now *time.Time) (*Server, alertShadowBrokerScope) {
	t.Helper()
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now.UTC() })
	server.orderJournal = newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "order-journal.jsonl"))
	scope, err := newAlertShadowBrokerScope(server.currentBrokerStateScope())
	if err != nil {
		t.Fatal(err)
	}
	return server, scope
}

func appendAlertShadowOrderEvidenceRow(t *testing.T, journal *orderJournalStore, ref string, at time.Time) {
	t.Helper()
	if err := journal.Append(alertShadowOrderEvidenceEvent(ref, at)); err != nil {
		t.Fatal(err)
	}
}

func alertShadowOrderEvidenceEvent(ref string, at time.Time) orderJournalEvent {
	id := 101 + len(ref)
	return orderJournalEvent{
		At: at, Type: orderJournalEventBrokerAcknowledged, OrderRef: ref,
		ReservedOrderID: id, ClientID: 7, PermID: 9000 + id,
		Account: "DU-HEARTBEAT", Endpoint: "127.0.0.1:4002", Mode: rpc.AccountModePaper,
		Symbol: "AAA", SecType: "STK", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
		TIF: rpc.OrderTIFGTC, Quantity: 10, Remaining: 10, Status: "PreSubmitted",
		SendState: orderSendStateBrokerAcknowledged,
	}
}

func seedAlertShadowOrderIntegrityEpisode(t *testing.T, server *Server, scope alertShadowBrokerScope, now *time.Time) {
	t.Helper()
	order := alertShadowTestMismatchedOrder(*now, scope)
	for range 2 {
		server.observeOrderIntegrityAlertShadow(t.Context(), orderIntegrityEvaluation{
			AsOf: *now, EvidenceAsOf: *now, Status: orderIntegrityHealthCurrent,
			Scope: brokerStateScope{Account: scope.account, Mode: scope.mode}, Orders: []rpc.OrderView{order},
		})
		*now = now.Add(30 * time.Second)
		order.BrokerTruthAsOf = *now
	}
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceOrderIntegrity); status.Active != 1 {
		t.Fatalf("failed to seed Order Integrity episode: %+v", status)
	}
}

func TestOrderIntegritySharedCommitRejectsSessionDriftOnHandlerPath(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	server, scope := newAlertShadowOrderEvidenceServer(t, &now)
	seedAlertShadowOrderIntegrityEpisode(t, server, scope, &now)
	appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "baseline", now)
	head, err := server.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	connector := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	t.Cleanup(func() { _ = connector.Stop() })
	server.stableBrokerEvidenceForTest = func(daemonBrokerEvidenceBinding, func() error) (bool, error) {
		return false, nil
	}
	server.observeOrderIntegrityAlertShadow(t.Context(), orderIntegrityEvaluation{
		AsOf: now, EvidenceAsOf: now, Status: orderIntegrityHealthCurrent,
		Scope: brokerStateScope{Account: scope.account, Mode: scope.mode}, Orders: []rpc.OrderView{},
		connector: connector, orderJournal: server.orderJournal, orderAuthorityHeadSeq: head.LastEventSeq,
	})
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceOrderIntegrity); status.Active != 1 || status.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("session drift on shared handler path recovered Order Integrity: %+v", status)
	}
}

func TestOrderIntegritySharedCommitRejectsJournalAdvanceAfterRead(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	server, scope := newAlertShadowOrderEvidenceServer(t, &now)
	seedAlertShadowOrderIntegrityEpisode(t, server, scope, &now)
	appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "baseline", now)
	head, err := server.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	connector := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	t.Cleanup(func() { _ = connector.Stop() })
	server.stableBrokerEvidenceForTest = func(_ daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
		if err := commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	server.orderIntegrityBeforeCommit = func() {
		appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "late-local-order", now.Add(time.Second))
	}
	server.observeOrderIntegrityAlertShadow(t.Context(), orderIntegrityEvaluation{
		AsOf: now, EvidenceAsOf: now, Status: orderIntegrityHealthCurrent,
		Scope: brokerStateScope{Account: scope.account, Mode: scope.mode}, Orders: []rpc.OrderView{},
		connector: connector, orderJournal: server.orderJournal, orderAuthorityHeadSeq: head.LastEventSeq,
	})
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceOrderIntegrity); status.Active != 1 || status.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("late journal append recovered Order Integrity: %+v", status)
	}
}

func TestProtectionSharedCommitRejectsJournalAdvanceAfterRead(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	server, scope := newAlertShadowOrderEvidenceServer(t, &now)
	issueOrder := rpc.ProtectionCoverageOrder{
		OrderRef: "opaque-order", Symbol: "AAA", Action: "SELL", OrderType: "STP", Remaining: 10,
		ReconciliationState: "position_mismatch", UpdatedAt: now,
	}
	active := alertShadowProtectionInput{
		AsOf: now, EvidenceAsOf: now, OrderSnapshotAsOf: now, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: rpc.ProtectionCoverageStateReconcileRequired,
			Counts:                  rpc.ProtectionCoverageCounts{ReconcileRequired: 1},
			ReconcileRequiredOrders: []rpc.ProtectionCoverageOrder{issueOrder},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateReconcileRequired,
				Orders: []rpc.ProtectionCoverageOrder{issueOrder}}}},
	}
	server.observeProtectionAlertShadow(t.Context(), active)
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection); status.Active != 1 {
		t.Fatalf("failed to seed Protection episode: %+v", status)
	}
	appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "baseline", now)
	head, err := server.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	connector := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	t.Cleanup(func() { _ = connector.Stop() })
	server.stableBrokerEvidenceForTest = func(_ daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
		if err := commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	server.protectionBeforeCommit = func() {
		appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "late-protection-order", now.Add(time.Second))
	}
	now = now.Add(30 * time.Second)
	clear := alertShadowProtectionInput{
		AsOf: now, EvidenceAsOf: now, OrderSnapshotAsOf: now, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}}},
		orderJournal: server.orderJournal, orderAuthorityHeadSeq: head.LastEventSeq,
	}
	settled := server.observeProtectionAlertShadowStable(t.Context(), daemonBrokerEvidenceBinding{
		scope: brokerStateScope{Account: scope.account, Mode: scope.mode}, connector: connector,
	}, clear)
	if settled {
		t.Fatalf("raced commit reported settled; the heartbeat retry would never fire")
	}
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection); status.Active != 1 || status.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("late journal append recovered Protection: %+v", status)
	}

	// The heartbeat's bounded retry rebuilds from the advanced journal head.
	// That rebuilt observation must settle and may recover the episode.
	server.protectionBeforeCommit = nil
	head, err = server.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	clear.orderAuthorityHeadSeq = head.LastEventSeq
	settled = server.observeProtectionAlertShadowStable(t.Context(), daemonBrokerEvidenceBinding{
		scope: brokerStateScope{Account: scope.account, Mode: scope.mode}, connector: connector,
	}, clear)
	if !settled {
		t.Fatalf("stable commit against the current head did not settle")
	}
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection); status.Measurements.EpisodesRecovered != 1 {
		t.Fatalf("rebuilt observation did not recover Protection: %+v", status)
	}
}

func TestProtectionJournalOrderAbsentFromSnapshotRetainsEpisode(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	server, scope := newAlertShadowOrderEvidenceServer(t, &now)
	issueOrder := rpc.ProtectionCoverageOrder{
		OrderRef: "opaque-order", Symbol: "AAA", Action: "SELL", OrderType: "STP", Remaining: 10,
		ReconciliationState: "position_mismatch", UpdatedAt: now,
	}
	server.observeProtectionAlertShadow(t.Context(), alertShadowProtectionInput{
		AsOf: now, EvidenceAsOf: now, OrderSnapshotAsOf: now, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: rpc.ProtectionCoverageStateReconcileRequired,
			Counts: rpc.ProtectionCoverageCounts{ReconcileRequired: 1}, ReconcileRequiredOrders: []rpc.ProtectionCoverageOrder{issueOrder},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateReconcileRequired,
				Orders: []rpc.ProtectionCoverageOrder{issueOrder}}}},
	})
	now = now.Add(30 * time.Second)
	summary := rpc.ProtectionCoverageSummary{AsOf: now, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
		ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}}}
	markProtectionSummaryJournalOrdersAbsent(&summary, []rpc.OrderView{{
		OrderRef: "journal-only", Open: true, Symbol: "AAA", SecType: "STK",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, Remaining: 10,
	}})
	server.observeProtectionAlertShadow(t.Context(), alertShadowProtectionInput{
		AsOf: now, EvidenceAsOf: now, OrderSnapshotAsOf: now, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope, Summary: summary,
	})
	if status := alertShadowTestSourceStatus(t, server.alertShadow.Status(scope), rpc.AlertSourceProtection); status.Active != 1 || status.Covered || status.Measurements.EpisodesRecovered != 0 {
		t.Fatalf("journal-to-snapshot absence recovered Protection: %+v", status)
	}
}

func TestOrderJournalStableHeadBlocksConcurrentMutation(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	server, _ := newAlertShadowOrderEvidenceServer(t, &now)
	appendAlertShadowOrderEvidenceRow(t, server.orderJournal, "baseline", now)
	head, err := server.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		_, err := server.orderJournal.WithStableAuthorityHead(head.LastEventSeq, func() error {
			close(entered)
			<-release
			return nil
		})
		commitDone <- err
	}()
	<-entered
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- server.orderJournal.Append(alertShadowOrderEvidenceEvent("blocked-local-order", now.Add(time.Second)))
	}()
	select {
	case err := <-mutationDone:
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("journal mutation crossed an in-progress stable-head commit")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-mutationDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("journal mutation remained blocked after stable-head commit")
	}
}
