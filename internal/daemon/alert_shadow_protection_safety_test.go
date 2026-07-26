package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAlertShadowProtectionFingerprintBindsMapperReceiptTimes(t *testing.T) {
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	input := alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base.Add(-time.Second), OrderSnapshotAsOf: base.Add(-2 * time.Second),
		OrderSnapshotComplete: true, OrderUniverse: protectionOrderUniverseJournaledAPI,
		Status: orderIntegrityHealthCurrent, Scope: alertShadowTestBrokerScope(t),
		Summary: rpc.ProtectionCoverageSummary{AsOf: base.Add(-3 * time.Second), Status: "ok"},
	}
	baseline, err := alertShadowProtectionInputFingerprint(input)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []struct {
		name string
		edit func(*alertShadowProtectionInput)
	}{
		{"portfolio receipt", func(in *alertShadowProtectionInput) { in.EvidenceAsOf = in.EvidenceAsOf.Add(time.Millisecond) }},
		{"order receipt", func(in *alertShadowProtectionInput) {
			in.OrderSnapshotAsOf = in.OrderSnapshotAsOf.Add(time.Millisecond)
		}},
		{"summary receipt", func(in *alertShadowProtectionInput) { in.Summary.AsOf = in.Summary.AsOf.Add(time.Millisecond) }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			changed := input
			mutation.edit(&changed)
			fingerprint, err := alertShadowProtectionInputFingerprint(changed)
			if err != nil {
				t.Fatal(err)
			}
			if fingerprint == baseline {
				t.Fatal("mapper-relevant receipt time did not change the input fingerprint")
			}
		})
	}
}

func TestProtectionCoverageCreditsOnlyBrokerWorkingStates(t *testing.T) {
	position := rpc.PositionView{Symbol: "AAA", SecType: "STK", ConID: 101, Quantity: 10}
	base := rpc.OrderView{
		Open: true, Symbol: "AAA", SecType: "STK", ConID: 101, Action: rpc.OrderActionSell,
		OrderType: "STP", OpenClose: "C", Quantity: 10, Remaining: 10,
	}
	for _, status := range []string{rpc.OrderLifecyclePendingSubmit, rpc.OrderLifecyclePendingCancel, rpc.OrderLifecycleUnknownReconcileRequired, ""} {
		order := base
		order.LifecycleStatus = status
		if protectionCoverageOrderCounts(order, position) {
			t.Fatalf("non-working lifecycle %q counted as protection", status)
		}
	}
	for _, status := range []string{rpc.OrderLifecycleSubmitted, rpc.OrderLifecyclePreSubmitted} {
		order := base
		order.LifecycleStatus = status
		if !protectionCoverageOrderCounts(order, position) {
			t.Fatalf("broker-working lifecycle %q did not count as protection", status)
		}
	}
}

func TestProtectionUsesSnapshotOrderFieldsInsteadOfStaleJournalProjection(t *testing.T) {
	asOf := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	journal := rpc.OrderView{
		ReservedOrderID: 91, ClientID: 31, PermID: 555, Open: true,
		Symbol: "STALE", SecType: "STK", ConID: 1, Action: rpc.OrderActionBuy,
		OrderType: "LMT", Quantity: 1, LifecycleStatus: rpc.OrderLifecyclePendingSubmit,
		OpenClose: "C",
	}
	broker := ibkrlib.OrderLifecycleEvent{
		Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 91, ClientID: 31, ClientIDPresent: true, PermID: 555,
		ConID: 101, Symbol: "AAA", SecType: "STK", Action: rpc.OrderActionSell, OrderType: "STP",
		TotalQuantity: 10, Status: "Submitted", TIF: rpc.OrderTIFGTC,
	}
	view := protectionOrderViewFromSnapshot(journal, broker, asOf)
	position := rpc.PositionView{Symbol: "AAA", SecType: "STK", ConID: 101, Quantity: 10}
	if view.Symbol != "AAA" || view.ConID != 101 || view.Action != rpc.OrderActionSell || view.OrderType != "STP" ||
		view.Quantity != 10 || view.LifecycleStatus != rpc.OrderLifecycleSubmitted || !view.BrokerTruthAsOf.Equal(asOf) ||
		!protectionCoverageOrderCounts(view, position) {
		t.Fatalf("snapshot order fields did not replace stale journal projection: %+v", view)
	}
}

func TestProtectionUnmatchedInventoryIsPartialAndCannotClear(t *testing.T) {
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	summary := rpc.ProtectionCoverageSummary{
		AsOf: base, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
		ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}},
	}
	markProtectionSummaryUnmatchedInventory(&summary)
	batch := alertShadowMapProtection(alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, OrderSnapshotAsOf: base, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent,
		Scope: alertShadowTestBrokerScope(t), Summary: summary,
	}, base.Add(time.Second))
	if batch.Covered || batch.NegativeReady || batch.Status != alertShadowStatusPartial || batch.EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("unmatched all-client inventory was trusted as a negative: %+v", batch)
	}
}

func TestProtectionPersistenceUncertaintyIsPartialAndCannotClear(t *testing.T) {
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	summary := rpc.ProtectionCoverageSummary{
		AsOf: base, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
		ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}},
	}
	markProtectionSummaryPersistenceUncertain(&summary)
	batch := alertShadowMapProtection(alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, OrderSnapshotAsOf: base, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent,
		Scope: alertShadowTestBrokerScope(t), Summary: summary,
	}, base.Add(time.Second))
	if batch.Covered || batch.NegativeReady || batch.Status != alertShadowStatusPartial || batch.EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("uncertain lifecycle persistence was trusted as a negative: %+v", batch)
	}
	journalUnknown := false
	for _, row := range summary.ByUnderlying {
		journalUnknown = journalUnknown || row.Underlying == "ORDER_JOURNAL" && row.State == rpc.ProtectionCoverageStateUnknown
	}
	if len(summary.ByUnderlying) != 2 || !journalUnknown {
		t.Fatalf("uncertain journal row was not projected explicitly: %+v", summary)
	}
}

func TestProtectionJournalOrderAbsentFromSnapshotIsPartialAndCannotClear(t *testing.T) {
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	summary := rpc.ProtectionCoverageSummary{
		AsOf: base, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
		ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}},
	}
	markProtectionSummaryJournalOrdersAbsent(&summary, []rpc.OrderView{{
		OrderRef: "journal-only", Open: true, Symbol: "AAA", SecType: "STK",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, Remaining: 10,
	}})
	batch := alertShadowMapProtection(alertShadowProtectionInput{
		AsOf: base, EvidenceAsOf: base, OrderSnapshotAsOf: base, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent,
		Scope: alertShadowTestBrokerScope(t), Summary: summary,
	}, base.Add(time.Second))
	if batch.Covered || batch.NegativeReady || batch.Status != alertShadowStatusPartial || batch.EvidenceHealth != rpc.AlertEvidencePartial {
		t.Fatalf("journal order absent from broker snapshot was trusted as a negative: %+v", batch)
	}
	if summary.Counts.Unknown != 1 || len(summary.ByUnderlying) != 2 ||
		!containsCoverageCode(summary.WarningCodes, "journal_order_absent_from_broker_snapshot") {
		t.Fatalf("journal-to-snapshot absence was not explicit typed evidence: %+v", summary)
	}
}

func TestProtectionPolicyVersionCannotRecoverPriorEpisode(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	scope := alertShadowTestBrokerScope(t)
	base := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceProtection, rpc.AlertKindProtectionGap, scope.account, scope.mode, "AAA")
	if err != nil {
		t.Fatal(err)
	}
	oldPolicy := alertRegistryFingerprint("alert-shadow-protection-policy-v1")
	coverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: base,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceProtection}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceProtection},
	}
	_, err = registry.Apply(t.Context(), alertEpisodeEvaluation{
		AuthorityScope: scope.authority, AsOf: base, Coverage: coverage,
		Observations: []alertEpisodeObservation{{
			EpisodeKey: episode, Source: rpc.AlertSourceProtection, Kind: rpc.AlertKindProtectionGap,
			PresentationCode: rpc.AlertPresentationProtectionReconciliationRequired, Active: true, Severity: rpc.AlertSeverityWatch,
			EvidenceFingerprint: alertRegistryFingerprint("old-protection-evidence"), EvidenceHealth: rpc.AlertEvidenceCurrent,
			Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: base, ObservedAt: base,
			PolicyFingerprint: oldPolicy, ProducerDecisionReason: alertShadowDecisionProtectionActive,
		}}, OpportunitySources: []rpc.AlertSource{rpc.AlertSourceProtection},
	})
	if err != nil {
		t.Fatal(err)
	}

	composer := newAlertShadowComposer(registry)
	now := base.Add(time.Minute)
	composer.now = func() time.Time { return now.Add(time.Second) }
	clear := alertShadowProtectionInput{
		AsOf: now, EvidenceAsOf: now, OrderSnapshotAsOf: now, OrderSnapshotComplete: true,
		OrderUniverse: protectionOrderUniverseJournaledAPI, Status: orderIntegrityHealthCurrent, Scope: scope,
		Summary: rpc.ProtectionCoverageSummary{AsOf: now, Status: "ok", Counts: rpc.ProtectionCoverageCounts{Covered: 1},
			ByUnderlying: []rpc.ProtectionCoverageRow{{Underlying: "AAA", State: rpc.ProtectionCoverageStateCovered}}},
	}
	snapshot, err := composer.ObserveProtection(t.Context(), clear)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].State == rpc.AlertEpisodeRecovered {
		t.Fatalf("new Protection policy recovered a prior-policy episode: %+v", snapshot.Candidates)
	}
}
