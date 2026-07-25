package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertDeliveryV3SamplesModeAfterBaseline(t *testing.T) {
	base := time.Date(2026, 7, 22, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		mode        string
		severity    rpc.AlertSeverity
		disposition string
		due         int
	}{
		{name: "none suppresses watch", mode: AlertModeNone, severity: rpc.AlertSeverityWatch, disposition: AlertDispositionModeSuppressed},
		{name: "act only suppresses watch", mode: AlertModeActOnly, severity: rpc.AlertSeverityWatch, disposition: AlertDispositionModeSuppressed},
		{name: "act only arms act", mode: AlertModeActOnly, severity: rpc.AlertSeverityAct, disposition: AlertDispositionEligible, due: 1},
		{name: "watch and act arms watch", mode: AlertModeWatchAndAct, severity: rpc.AlertSeverityWatch, disposition: AlertDispositionEligible, due: 1},
		{name: "observe remains inbox only", mode: AlertModeWatchAndAct, severity: rpc.AlertSeverityObserve, disposition: AlertDispositionObserveOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetAlertMode(tc.mode); err != nil {
				t.Fatal(err)
			}
			source := rpc.AlertSourceStress
			if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent)); err != nil {
				t.Fatal(err)
			}
			at := base.Add(time.Minute)
			candidate := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, tc.name, "open", at)
			candidate.Severity = tc.severity
			view, err := store.ObserveAlertSnapshot(testAlertSnapshot(at, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent, candidate))
			if err != nil {
				t.Fatal(err)
			}
			got := view.Occurrences[len(view.Occurrences)-1]
			if got.Disposition != tc.disposition || len(store.AlertDeliveriesDue(at)) != tc.due {
				t.Fatalf("disposition=%q due=%d, want %q/%d", got.Disposition, len(store.AlertDeliveriesDue(at)), tc.disposition, tc.due)
			}
		})
	}
}

func TestAlertDeliveryV3CutoverNeverRetroactivelyArms(t *testing.T) {
	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatal(err)
	}
	source := rpc.AlertSourceStress
	unknown := testAlertSnapshot(base, []rpc.AlertSource{source}, nil, rpc.AlertCoverageUnknown)
	if _, err := store.ObserveAlertSnapshot(unknown); err != nil {
		t.Fatal(err)
	}
	if alertDeliveryBaselineEstablished(store.data.AlertDelivery, unknown.AuthorityScope) {
		t.Fatal("unknown coverage established a transport baseline")
	}
	if err := store.SetAlertMode(AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	openedAt := base.Add(time.Minute)
	candidate := testAlertCandidate(t, source, rpc.AlertKindPortfolioRisk, "cutover", "open", openedAt)
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(openedAt, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent, candidate))
	if err != nil {
		t.Fatal(err)
	}
	if !alertDeliveryBaselineEstablished(store.data.AlertDelivery, unknown.AuthorityScope) || view.Occurrences[0].Disposition != AlertDispositionCutoverExisting || len(store.AlertDeliveriesDue(openedAt)) != 0 {
		t.Fatalf("first trustworthy snapshot retroactively armed cutover state: %+v", view)
	}
	revisedAt := openedAt.Add(time.Minute)
	revised := reviseAlertCandidate(candidate, revisedAt, "b", candidate.State, rpc.AlertSeverityUrgent)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(revisedAt, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent, revised))
	if err != nil {
		t.Fatal(err)
	}
	if view.Occurrences[0].Disposition != AlertDispositionCutoverExisting || len(store.AlertDeliveriesDue(revisedAt)) != 0 {
		t.Fatalf("later mode or severity revision armed an existing cutover occurrence: %+v", view)
	}
}

func TestAlertDeliveryV3ModeDowngradeBeforeConfirmIsTerminal(t *testing.T) {
	base := time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "mode-race", "open", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	target := AlertDeliveryTargetRef("mode-device", "mode-subscription")
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, base.Add(time.Second))
	if err != nil || !send {
		t.Fatalf("reserve send=%v err=%v", send, err)
	}
	if err := store.SetAlertMode(AlertModeNone); err != nil {
		t.Fatal(err)
	}
	confirmed, allowed, err := store.ConfirmAlertTransport(reservation.AttemptID, base.Add(2*time.Second))
	if err != nil || allowed || confirmed.AttemptID != reservation.AttemptID {
		t.Fatalf("downgraded confirm=%+v allowed=%v err=%v", confirmed, allowed, err)
	}
	attempt := store.data.AlertDelivery.Attempts[len(store.data.AlertDelivery.Attempts)-1]
	if attempt.Class != AlertDeliveryAttemptModeSuppressed || attempt.CompletedAt.IsZero() {
		t.Fatalf("downgrade attempt was not terminal mode suppression: %+v", attempt)
	}
	if err := store.SetAlertMode(AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	view := store.AlertDelivery(base.Add(3 * time.Second))
	if view.Occurrences[0].Disposition != AlertDispositionModeSuppressed || len(store.AlertDeliveriesDue(base.Add(3*time.Second))) != 0 {
		t.Fatalf("later mode upgrade revived occurrence: %+v", view)
	}
	if _, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, AlertDeliveryTargetRef("other-device", "other-subscription"), base.Add(3*time.Second)); err != nil || send {
		t.Fatalf("later mode upgrade revived transport: send=%v err=%v", send, err)
	}
}

func TestAlertDeliveryV3FreshnessExpiryBeforeConfirmRetriesAfterRefresh(t *testing.T) {
	base := time.Date(2026, 7, 22, 9, 45, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "freshness-race", "open", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	target := AlertDeliveryTargetRef("fresh-device", "fresh-subscription")
	reservation, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, base.Add(59*time.Minute))
	if err != nil || !send {
		t.Fatalf("reserve send=%v err=%v", send, err)
	}
	confirmAt := base.Add(time.Hour + time.Nanosecond)
	confirmed, allowed, err := store.ConfirmAlertTransport(reservation.AttemptID, confirmAt)
	if err != nil || allowed || confirmed.AttemptID != reservation.AttemptID {
		t.Fatalf("expired confirm=%+v allowed=%v err=%v", confirmed, allowed, err)
	}
	first := store.data.AlertDelivery.Attempts[len(store.data.AlertDelivery.Attempts)-1]
	if first.Class != AlertDeliveryAttemptRetry || !first.RetryAt.Equal(confirmAt.Add(time.Minute)) || first.Disposition != "" {
		t.Fatalf("temporary freshness gap became terminal: %+v", first)
	}
	refreshed := reviseAlertCandidate(candidate, first.RetryAt, "b", candidate.State, candidate.Severity)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(first.RetryAt, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, refreshed)); err != nil {
		t.Fatal(err)
	}
	retry, send, err := store.BeginAlertDelivery(candidate.OccurrenceKey, target, first.RetryAt)
	if err != nil || !send || retry.AttemptNumber != 2 {
		t.Fatalf("fresh retry=%+v send=%v err=%v", retry, send, err)
	}
	if _, allowed, err := store.ConfirmAlertTransport(retry.AttemptID, first.RetryAt); err != nil || !allowed {
		t.Fatalf("fresh confirm allowed=%v err=%v", allowed, err)
	}
}

func TestAlertDeliveryV3RequiresFreshExactSourceForDeliveryAndOmission(t *testing.T) {
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	enableTestAlertDelivery(t, store)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "exact-source", "open", base)
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{candidate.Source}, []rpc.AlertSource{candidate.Source}, rpc.AlertCoverageCurrent, candidate)); err != nil {
		t.Fatal(err)
	}
	if due := store.AlertDeliveriesDue(base); len(due) != 1 {
		t.Fatalf("fresh exact source due=%+v", due)
	}
	if due := store.AlertDeliveriesDue(base.Add(time.Hour + time.Nanosecond)); len(due) != 0 {
		t.Fatalf("expired source freshness still authorized delivery: %+v", due)
	}
	partialAt := base.Add(2 * time.Minute)
	expected := []rpc.AlertSource{rpc.AlertSourceStress, rpc.AlertSourceRegime}
	view, err := store.ObserveAlertSnapshot(testAlertSnapshot(partialAt, expected, []rpc.AlertSource{rpc.AlertSourceRegime}, rpc.AlertCoverageCurrent))
	if err != nil {
		t.Fatal(err)
	}
	if !occurrenceBySource(t, view, candidate.Source).EndedAt.IsZero() {
		t.Fatalf("another source's current evidence resolved the candidate: %+v", view)
	}
	clearAt := partialAt.Add(time.Minute)
	view, err = store.ObserveAlertSnapshot(testAlertSnapshot(clearAt, expected, expected, rpc.AlertCoverageCurrent))
	if err != nil {
		t.Fatal(err)
	}
	if occurrenceBySource(t, view, candidate.Source).EndReason != AlertDeliveryEndOmitted {
		t.Fatalf("exact current omission did not resolve the candidate: %+v", view)
	}
}

func TestAlertDeliveryV3ViewAgesClearEvidenceWithoutDroppingSource(t *testing.T) {
	base := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := rpc.AlertSourceStress
	if _, err := store.ObserveAlertSnapshot(testAlertSnapshot(base, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageCurrent)); err != nil {
		t.Fatal(err)
	}
	current := store.AlertDelivery(base)
	if current.CurrentState != rpc.AlertSnapshotClear || current.Coverage.Freshness != rpc.AlertCoverageCurrent {
		t.Fatalf("fresh clear view=%+v", current)
	}
	aged := store.AlertDelivery(base.Add(time.Hour + time.Nanosecond))
	if aged.CurrentState != rpc.AlertSnapshotUnknown || aged.Coverage.Freshness != rpc.AlertCoverageStale || len(aged.Sources) != 1 {
		t.Fatalf("expired clear evidence still looked current: %+v", aged)
	}
	row := aged.Sources[0]
	if row.Source != source || row.Status != "stale" || row.Reason != "freshness_expired" || row.EvidenceHealth != rpc.AlertEvidenceStale || !row.Covered || !row.FreshUntil.Equal(base.Add(time.Hour)) {
		t.Fatalf("expired source was dropped or lost its exact reason: %+v", row)
	}

	staleStore, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	staleSnapshot := testAlertSnapshot(base, []rpc.AlertSource{source}, []rpc.AlertSource{source}, rpc.AlertCoverageStale)
	staleSnapshot.Sources[0].Status = "producer_current"
	staleSnapshot.Sources[0].Reason = "producer_available"
	staleSnapshot.Sources[0].EvidenceHealth = rpc.AlertEvidenceCurrent
	if _, err := staleStore.ObserveAlertSnapshot(staleSnapshot); err != nil {
		t.Fatal(err)
	}
	agedStale := staleStore.AlertDelivery(base.Add(time.Hour + time.Nanosecond))
	if len(agedStale.Sources) != 1 || agedStale.Sources[0].Status != "stale" || agedStale.Sources[0].Reason != "freshness_expired" || agedStale.Sources[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("row inside aggregate-stale snapshot stopped aging: %+v", agedStale)
	}
}

func TestAlertDeliveryV2MigratesExactlyAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	raw, candidate := testAlertDeliveryV2Ledger(t)
	writeAlertDeliveryQuarantineFixture(t, dir, raw, AlertModeWatchAndAct)
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if store.alertDeliveryQuarantinedLocked() {
		t.Fatal("exact v2 ledger was quarantined")
	}
	view := store.AlertDelivery(candidate.ObservedAt)
	acceptedAt := candidate.ObservedAt.Add(-time.Second)
	if !view.Initialized || view.Version != AlertDeliveryVersion || view.Generation != 7 || view.CurrentState != rpc.AlertSnapshotActive || view.Coverage.State != rpc.AlertCoverageUnavailable || len(view.Occurrences) != 1 || view.Occurrences[0].Disposition != AlertDispositionCutoverExisting || view.Attention.UnreadCount != 1 || view.AttemptTotals.Accepted != 1 || !view.DeliveryHealth.LastAcceptedAt.Equal(acceptedAt) || len(store.AlertDeliveriesDue(candidate.ObservedAt)) != 0 {
		t.Fatalf("v2 migration did not preserve visible history and fail closed: %+v", view)
	}
	persisted, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, obsolete := range []string{`"version":"alert-delivery-v2"`, `"delivery_preference"`, `"transport_eligible"`, `"shadow"`} {
		if strings.Contains(string(persisted), obsolete) {
			t.Fatalf("v2-only field remained after atomic migration: %s", obsolete)
		}
	}
}

func TestAlertDeliveryV2UnknownFieldIsQuarantined(t *testing.T) {
	raw, _ := testAlertDeliveryV2Ledger(t)
	topLevel := append(append(json.RawMessage(nil), raw[:len(raw)-1]...), []byte(`,"private_marker":"reject-me"}`)...)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(envelope["snapshot"], &snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot["private_marker"] = json.RawMessage(`"reject-me"`)
	snapshotRaw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	envelope["snapshot"] = snapshotRaw
	nested, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "top level", raw: topLevel},
		{name: "nested snapshot", raw: nested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeAlertDeliveryQuarantineFixture(t, dir, tc.raw, AlertModeWatchAndAct)
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			assertAlertDeliveryQuarantined(t, store)
			assertAlertDeliveryQuarantineArtifact(t, dir, tc.raw)
			if due := store.AlertDeliveriesDue(time.Now().UTC()); len(due) != 0 {
				t.Fatalf("unknown v2 field produced delivery work: %+v", due)
			}
		})
	}
}

func testAlertDeliveryV2Ledger(t *testing.T) (json.RawMessage, rpc.AlertCandidate) {
	t.Helper()
	at := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	candidate := testAlertCandidate(t, rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, "v2-migration", "open", at)
	legacyCandidate := legacyAlertCandidate{
		EpisodeKey: candidate.EpisodeKey, OccurrenceKey: candidate.OccurrenceKey, EvidenceFingerprint: candidate.EvidenceFingerprint,
		Source: candidate.Source, Kind: candidate.Kind, State: candidate.State, Severity: candidate.Severity,
		DeliveryPreference: "page", EvidenceHealth: candidate.EvidenceHealth, Destination: candidate.Destination,
		EvidenceAsOf: candidate.EvidenceAsOf, StateChangedAt: candidate.StateChangedAt, ObservedAt: candidate.ObservedAt,
	}
	scope := defaultTestAlertAuthorityScope
	target := AlertDeliveryTargetRef("v2-device", "v2-subscription")
	receiptKey := alertDeliveryReceiptKey(scope, candidate.OccurrenceKey, target)
	reservedAt, acceptedAt := at.Add(-2*time.Second), at.Add(-time.Second)
	ledger := legacyAlertDeliveryDataV2{
		Version: legacyAlertDeliveryVersionV2, Generation: 7,
		Snapshot: legacyAlertCandidateSnapshotV2{
			SchemaVersion: legacyAlertSnapshotVersionV2, AuthorityScope: scope, AsOf: at, CurrentState: rpc.AlertSnapshotActive,
			Coverage:   rpc.AlertCoverage{State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at, ExpectedSources: []rpc.AlertSource{candidate.Source}, CoveredSources: []rpc.AlertSource{candidate.Source}},
			Candidates: []legacyAlertCandidate{legacyCandidate},
		},
		SourceWatermarks:        map[rpc.AlertSource]time.Time{candidate.Source: at},
		SourceWatermarksByScope: map[string]map[rpc.AlertSource]time.Time{scope: {candidate.Source: at}},
		Episodes: []alertDeliveryEpisode{{
			AuthorityScope: scope, EpisodeKey: candidate.EpisodeKey, Source: candidate.Source, Kind: candidate.Kind,
			CurrentOccurrenceKey: candidate.OccurrenceKey, State: candidate.State, FirstSeenAt: at, LastSeenAt: at,
		}},
		Occurrences: []legacyAlertDeliveryOccurrenceV2{{
			AuthorityScope: scope, OccurrenceKey: candidate.OccurrenceKey, EpisodeKey: candidate.EpisodeKey,
			EvidenceFingerprint: candidate.EvidenceFingerprint, DisplayID: alertDeliveryDisplayID(scope, candidate.OccurrenceKey),
			Source: candidate.Source, Kind: candidate.Kind, State: candidate.State, Severity: candidate.Severity,
			DeliveryPreference: "page", EvidenceHealth: candidate.EvidenceHealth, Destination: candidate.Destination,
			EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at, FirstSeenAt: at, LastSeenAt: at,
			AttentionSeq: 1, TransportEligible: true,
		}},
		Attempts: []alertDeliveryAttempt{{
			AuthorityScope: scope, ID: alertDeliveryAttemptID(receiptKey, 1, reservedAt), OccurrenceKey: candidate.OccurrenceKey,
			TargetRef: target, ReceiptKey: receiptKey, AttemptNumber: 1, ReservedAt: reservedAt, CompletedAt: acceptedAt,
			Class: AlertDeliveryAttemptAccepted, Disposition: AlertDeliveryCompletionApplied,
		}},
		Receipts: []alertDeliveryReceipt{{
			AuthorityScope: scope, OccurrenceKey: candidate.OccurrenceKey, TargetRef: target, ReceiptKey: receiptKey, AcceptedAt: acceptedAt,
		}},
		RetiredTargets:        make(map[string]time.Time),
		Health:                AlertDeliveryHealth{State: "shadow", Class: "shadow", UpdatedAt: at, LastAcceptedAt: acceptedAt},
		AttentionHighWaterSeq: 1,
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	return raw, candidate
}
