package state

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertDeliveryHealthySourceWorksDespiteMissingOrStaleSibling(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "missing"
		if stale {
			name = "stale"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetAlertMode(AlertModeWatchAndAct); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Truncate(time.Second)
			good, bad := rpc.AlertSourceStress, rpc.AlertSourceRiskPolicy
			expected := []rpc.AlertSource{good, bad}
			snapshot := func(when time.Time, candidates ...rpc.AlertCandidate) rpc.AlertCandidateSnapshot {
				for i := range candidates {
					candidates[i].ObservedAt, candidates[i].EvidenceAsOf = when, when
				}
				covered := []rpc.AlertSource{good}
				if stale {
					covered = expected
				}
				s := testAlertSnapshot(when, expected, covered, rpc.AlertCoverageCurrent, candidates...)
				if stale {
					s.Coverage.Freshness = rpc.AlertCoverageStale
					for i := range s.Sources {
						if s.Sources[i].Source == bad {
							s.Sources[i].EvidenceHealth = rpc.AlertEvidenceStale
							s.Sources[i].Status = "test_stale"
							s.Sources[i].Reason = "test_stale"
						}
					}
				}
				return s
			}
			initial := testAlertCandidate(t, good, rpc.AlertKindPortfolioRisk, "old-condition", "old-occurrence", at)
			if _, err := store.ObserveAlertSnapshot(snapshot(at, initial)); err != nil {
				t.Fatal(err)
			}
			if len(store.AlertDeliveriesDue(at)) != 0 {
				t.Fatal("commissioning sent backlog")
			}
			later := at.Add(time.Second)
			fresh := testAlertCandidate(t, good, rpc.AlertKindPortfolioRisk, "new-condition", "new-occurrence", later)
			if _, err := store.ObserveAlertSnapshot(snapshot(later, initial, fresh)); err != nil {
				t.Fatal(err)
			}
			if due := store.AlertDeliveriesDue(later); len(due) != 1 {
				t.Fatalf("healthy source silenced by %s sibling: due=%d", name, len(due))
			}
			target := AlertDeliveryTargetRef("synthetic-device", "synthetic-subscription")
			reservation, send, err := store.BeginAlertDelivery(fresh.OccurrenceKey, target, later)
			if err != nil || !send {
				t.Fatalf("begin: %v %v", send, err)
			}
			if _, send, err := store.ConfirmAlertTransport(reservation.AttemptID, later); err != nil || !send {
				t.Fatalf("confirm: %v %v", send, err)
			}
			if _, err := store.CompleteAlertDelivery(reservation.AttemptID, AlertDeliveryCompletionAccepted, later); err != nil {
				t.Fatal(err)
			}
			receipts := store.data.AlertDelivery.Receipts
			restarted, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.ObserveAlertSnapshot(snapshot(later.Add(time.Second), initial, fresh)); err != nil {
				t.Fatal(err)
			}
			_, resend, err := restarted.BeginAlertDelivery(fresh.OccurrenceKey, target, later.Add(time.Second))
			if err != nil || resend || !reflect.DeepEqual(receipts, restarted.data.AlertDelivery.Receipts) {
				t.Fatal("restart replayed a receipt or lost it")
			}
			// A newly recovered sibling establishes its own baseline without backlog.
			badCandidate := testAlertCandidate(t, bad, rpc.AlertKindGovernance, "previously-unknown", "first-seen", later.Add(2*time.Second))
			full := testAlertSnapshot(later.Add(2*time.Second), expected, expected, rpc.AlertCoverageCurrent, initial, fresh, badCandidate)
			for i := range full.Candidates {
				full.Candidates[i].ObservedAt, full.Candidates[i].EvidenceAsOf = full.AsOf, full.AsOf
			}
			if _, err := restarted.ObserveAlertSnapshot(full); err != nil {
				t.Fatal(err)
			}
			if len(restarted.AlertDeliveriesDue(full.AsOf)) != 1 {
				t.Fatal("recovered source changed the single existing transport candidate")
			}
			otherScope, err := rpc.BuildAlertAuthorityScope("OTHER-SYNTHETIC", "paper")
			if err != nil {
				t.Fatal(err)
			}
			full.AuthorityScope = otherScope
			full.AsOf = full.AsOf.Add(time.Second)
			full.Coverage.AsOf = full.AsOf
			for i := range full.Candidates {
				full.Candidates[i].ObservedAt, full.Candidates[i].EvidenceAsOf = full.AsOf, full.AsOf
			}
			if _, err := restarted.ObserveAlertSnapshot(full); err != nil {
				t.Fatal(err)
			}
			if len(restarted.AlertDeliveriesDue(full.AsOf)) != 0 {
				t.Fatal("new scope inherited old scope commissioning")
			}
		})
	}
}

func TestAlertSourceBaselineMigrationNeedsStoredCoverageProof(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	good, bad := rpc.AlertSourceStress, rpc.AlertSourceRiskPolicy
	data := alertDeliveryData{
		Baselines:               map[string]alertDeliveryBaseline{defaultTestAlertAuthorityScope: {EstablishedAt: at, SnapshotAsOf: at}},
		SourceWatermarksByScope: map[string]map[rpc.AlertSource]time.Time{defaultTestAlertAuthorityScope: {good: at, bad: at.Add(-time.Second)}},
	}
	migrateAlertSourceBaselines(&data)
	if !alertSourceBaselineEstablished(&data, defaultTestAlertAuthorityScope, good) || alertSourceBaselineEstablished(&data, defaultTestAlertAuthorityScope, bad) {
		t.Fatal("legacy global baseline commissioned an unproven source")
	}
	if err := validateAlertSourceBaselines(&data, 1); err != nil {
		t.Fatal(err)
	}
	if err := validateAlertSourceBaselines(&data, 0); err == nil {
		t.Fatal("unbounded baseline state accepted")
	}
	data.SourceBaselines[defaultTestAlertAuthorityScope][bad] = alertDeliveryBaseline{EstablishedAt: at, SnapshotAsOf: at.Add(time.Second)}
	if err := validateAlertSourceBaselines(&data, 10); err == nil {
		t.Fatal("future baseline accepted")
	}
}
