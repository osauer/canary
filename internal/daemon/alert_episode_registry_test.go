package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestAlertEpisodeRegistryPersistsEmptyEvaluationAcrossRestart(t *testing.T) {
	path := alertRegistryTestPath(t)
	store := openAlertRegistryTestStore(t, path)
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 21, 7, 30, 0, 0, time.UTC)
	snapshot, err := registry.Apply(t.Context(), alertRegistryEvaluation(at, alertRegistryCompleteCoverage(at)))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentState != rpc.AlertSnapshotClear || snapshot.Candidates == nil || len(snapshot.Candidates) != 0 {
		t.Fatalf("empty evaluation snapshot=%+v", snapshot)
	}
	if len(registry.document.Scopes) != 1 || registry.document.Scopes[0].Episodes == nil {
		t.Fatal("empty evaluation collapsed episodes to nil")
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, alertEpisodeRegistryStateKind)
	if err != nil || !ok || doc.Revision != 1 || !strings.Contains(string(doc.JSON), `"episodes":[]`) {
		t.Fatalf("empty registry document=%+v ok=%v err=%v", doc, ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openAlertRegistryTestStore(t, path)
	restarted, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, ok, err := restarted.Snapshot(alertRegistryAuthority(), at)
	if err != nil || !ok || afterRestart.CurrentState != rpc.AlertSnapshotClear || afterRestart.Candidates == nil || len(afterRestart.Candidates) != 0 {
		t.Fatalf("empty restart snapshot=%+v ok=%v err=%v", afterRestart, ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAlertEpisodeRegistryFutureDurableBoundaryFailsCoverageClosed(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	observation := alertRegistryObservation(t, "future-boundary", future, true)
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(future, alertRegistryCompleteCoverage(future), observation)); err != nil {
		t.Fatal(err)
	}

	snapshot, ok, err := registry.Snapshot(alertRegistryAuthority(), future.Add(-time.Minute))
	if err != nil || !ok {
		t.Fatalf("future snapshot ok=%v err=%v", ok, err)
	}
	if snapshot.CurrentState != rpc.AlertSnapshotActive || snapshot.IsClear() || snapshot.Coverage.State != rpc.AlertCoverageUnavailable ||
		snapshot.Coverage.Freshness != rpc.AlertCoverageUnknown || len(snapshot.Coverage.CoveredSources) != 0 {
		t.Fatalf("future durable boundary did not fail coverage closed: %+v", snapshot)
	}
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("future active evidence was not retained stale: %+v", snapshot.Candidates)
	}
}

func TestAlertEpisodeRegistryLifecycleSurvivesRestart(t *testing.T) {
	path := alertRegistryTestPath(t)
	store := openAlertRegistryTestStore(t, path)
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := registry.Snapshot(alertRegistryAuthority(), time.Now().UTC()); err != nil || ok {
		t.Fatalf("fresh snapshot ok=%v err=%v", ok, err)
	}

	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	observation := alertRegistryObservation(t, "lifecycle", base, true)
	open, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), observation))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, open, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	openingOccurrence := open.Candidates[0].OccurrenceKey
	openingChangedAt := open.Candidates[0].StateChangedAt

	refreshedObservation := observation
	refreshedObservation.ObservedAt = base.Add(time.Minute)
	refreshedObservation.EvidenceAsOf = refreshedObservation.ObservedAt
	refreshedObservation.EvidenceFingerprint = alertRegistryFingerprint("lifecycle-evidence-refresh")
	refreshed, err := registry.Apply(t.Context(), alertRegistryEvaluation(refreshedObservation.ObservedAt, alertRegistryCompleteCoverage(refreshedObservation.ObservedAt), refreshedObservation))
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Candidates[0]; got.OccurrenceKey != openingOccurrence || !got.StateChangedAt.Equal(openingChangedAt) {
		t.Fatalf("evidence refresh rotated lifecycle: %+v", got)
	}

	escalatedObservation := refreshedObservation
	escalatedObservation.ObservedAt = base.Add(2 * time.Minute)
	escalatedObservation.EvidenceAsOf = escalatedObservation.ObservedAt
	escalatedObservation.Severity = rpc.AlertSeverityAct
	escalatedObservation.EscalationFingerprint = alertRegistryFingerprint("qualifying-escalation-1")
	escalated, err := registry.Apply(t.Context(), alertRegistryEvaluation(escalatedObservation.ObservedAt, alertRegistryCompleteCoverage(escalatedObservation.ObservedAt), escalatedObservation))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, escalated, rpc.AlertEpisodeEscalated, rpc.AlertEvidenceCurrent)
	escalatedOccurrence := escalated.Candidates[0].OccurrenceKey
	if escalatedOccurrence == openingOccurrence {
		t.Fatal("qualifying escalation did not rotate occurrence")
	}
	replayed, err := registry.Apply(t.Context(), alertRegistryEvaluation(escalatedObservation.ObservedAt, alertRegistryCompleteCoverage(escalatedObservation.ObservedAt), escalatedObservation))
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Candidates[0].OccurrenceKey != escalatedOccurrence {
		t.Fatal("escalation replay rotated occurrence")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAlertRegistryTestStore(t, path)
	registry, err = newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, ok, err := registry.Snapshot(alertRegistryAuthority(), escalatedObservation.ObservedAt)
	if err != nil || !ok {
		t.Fatalf("restart snapshot ok=%v err=%v", ok, err)
	}
	if got := afterRestart.Candidates[0]; got.State != rpc.AlertEpisodeEscalated || got.OccurrenceKey != escalatedOccurrence {
		t.Fatalf("restart lost escalated occurrence: %+v", got)
	}

	recoveryObservation := escalatedObservation
	recoveryObservation.Active = false
	recoveryObservation.ObservedAt = base.Add(3 * time.Minute)
	recoveryObservation.EvidenceAsOf = recoveryObservation.ObservedAt
	recoveryObservation.EvidenceFingerprint = alertRegistryFingerprint("authoritative-negative")
	recoveryObservation.ProducerDecisionReason = "classified_clear"
	recovered, err := registry.Apply(t.Context(), alertRegistryEvaluation(recoveryObservation.ObservedAt, alertRegistryCompleteCoverage(recoveryObservation.ObservedAt), recoveryObservation))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, recovered, rpc.AlertEpisodeRecovered, rpc.AlertEvidenceCurrent)
	if recovered.CurrentState != rpc.AlertSnapshotClear || recovered.Candidates[0].OccurrenceKey != escalatedOccurrence {
		t.Fatalf("recovery changed occurrence or clear state: %+v", recovered)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAlertRegistryTestStore(t, path)
	registry, err = newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	recoveryAfterRestart, ok, err := registry.Snapshot(alertRegistryAuthority(), recoveryObservation.ObservedAt)
	if err != nil || !ok || len(recoveryAfterRestart.Candidates) != 1 || recoveryAfterRestart.Candidates[0].State != rpc.AlertEpisodeRecovered {
		t.Fatalf("unobserved recovery was not replayable after restart: %+v ok=%v err=%v", recoveryAfterRestart, ok, err)
	}

	confirmedNegative := recoveryObservation
	confirmedNegative.ObservedAt = base.Add(4 * time.Minute)
	confirmedNegative.EvidenceAsOf = confirmedNegative.ObservedAt
	confirmedNegative.EvidenceFingerprint = alertRegistryFingerprint("still-clear")
	clear, err := registry.Apply(t.Context(), alertRegistryEvaluation(confirmedNegative.ObservedAt, alertRegistryCompleteCoverage(confirmedNegative.ObservedAt), confirmedNegative))
	if err != nil {
		t.Fatal(err)
	}
	if clear.CurrentState != rpc.AlertSnapshotClear || len(clear.Candidates) != 0 {
		t.Fatalf("confirmed inactive episode remained visible: %+v", clear)
	}

	reopenObservation := confirmedNegative
	reopenObservation.Active = true
	reopenObservation.EscalationFingerprint = ""
	reopenObservation.ObservedAt = base.Add(5 * time.Minute)
	reopenObservation.EvidenceAsOf = reopenObservation.ObservedAt
	reopenObservation.EvidenceFingerprint = alertRegistryFingerprint("reopened-positive")
	reopenObservation.ProducerDecisionReason = "classified_active"
	reopened, err := registry.Apply(t.Context(), alertRegistryEvaluation(reopenObservation.ObservedAt, alertRegistryCompleteCoverage(reopenObservation.ObservedAt), reopenObservation))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, reopened, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	if reopened.Candidates[0].OccurrenceKey == escalatedOccurrence {
		t.Fatal("reopen reused recovered occurrence")
	}

	events, err := store.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: alertEpisodeDecisionEventType, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("decision events=%d want 7", len(events))
	}
	var event alertEpisodeDecisionEvent
	if err := json.Unmarshal(events[len(events)-1].PayloadJSON, &event); err != nil {
		t.Fatal(err)
	}
	if len(event.Decisions) != 1 || event.Decisions[0].PolicyFingerprint != reopenObservation.PolicyFingerprint || event.Decisions[0].ProducerDecisionReason != "classified_active" || event.Decisions[0].EvidenceAsOf != reopenObservation.EvidenceAsOf {
		t.Fatalf("redacted decision audit incomplete: %+v", event)
	}
	if strings.Contains(string(events[len(events)-1].PayloadJSON), "account") || strings.Contains(string(events[len(events)-1].PayloadJSON), "symbol") {
		t.Fatalf("decision event exposes forbidden subject field: %s", events[len(events)-1].PayloadJSON)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAlertEpisodeRegistryNeverRecoversFromOutageStalePartialOrOmission(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 9, 0, 0, 0, time.UTC)
	positive := alertRegistryObservation(t, "outage", base, true)
	open, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), positive))
	if err != nil {
		t.Fatal(err)
	}
	changedAt := open.Candidates[0].StateChangedAt

	unavailableAt := base.Add(time.Minute)
	unavailableCoverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: unavailableAt,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{},
	}
	unavailable, err := registry.Apply(t.Context(), alertRegistryEvaluation(unavailableAt, unavailableCoverage))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, unavailable, rpc.AlertEpisodeOpen, rpc.AlertEvidenceUnavailable)

	partialAt := base.Add(2 * time.Minute)
	partialNegative := positive
	partialNegative.Active = false
	partialNegative.ObservedAt = partialAt
	partialNegative.EvidenceAsOf = partialAt
	partialNegative.EvidenceFingerprint = alertRegistryFingerprint("partial-negative")
	partialNegative.EvidenceHealth = rpc.AlertEvidencePartial
	partialNegative.ProducerDecisionReason = "classified_clear"
	partialCoverage := rpc.AlertCoverage{
		State: rpc.AlertCoveragePartial, Freshness: rpc.AlertCoverageCurrent, AsOf: partialAt,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress, rpc.AlertSourceRegime},
		CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
	}
	partial, err := registry.Apply(t.Context(), alertRegistryEvaluation(partialAt, partialCoverage, partialNegative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, partial, rpc.AlertEpisodeOpen, rpc.AlertEvidencePartial)

	staleAt := base.Add(3 * time.Minute)
	staleNegative := partialNegative
	staleNegative.ObservedAt = staleAt
	staleNegative.EvidenceAsOf = staleAt.Add(-time.Minute)
	staleNegative.EvidenceHealth = rpc.AlertEvidenceStale
	staleNegative.EvidenceFingerprint = alertRegistryFingerprint("stale-negative")
	staleCoverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageStale, AsOf: staleAt,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceStress},
	}
	stale, err := registry.Apply(t.Context(), alertRegistryEvaluation(staleAt, staleCoverage, staleNegative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, stale, rpc.AlertEpisodeOpen, rpc.AlertEvidenceStale)

	omittedAt := base.Add(4 * time.Minute)
	omitted, err := registry.Apply(t.Context(), alertRegistryEvaluation(omittedAt, alertRegistryCompleteCoverage(omittedAt)))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, omitted, rpc.AlertEpisodeOpen, rpc.AlertEvidenceUnavailable)
	if !omitted.Candidates[0].StateChangedAt.Equal(changedAt) {
		t.Fatal("degraded evidence changed active lifecycle timestamp")
	}

	recoveryAt := base.Add(5 * time.Minute)
	authoritativeNegative := partialNegative
	authoritativeNegative.ObservedAt = recoveryAt
	authoritativeNegative.EvidenceAsOf = recoveryAt
	authoritativeNegative.EvidenceHealth = rpc.AlertEvidenceCurrent
	authoritativeNegative.EvidenceFingerprint = alertRegistryFingerprint("complete-negative")
	recovered, err := registry.Apply(t.Context(), alertRegistryEvaluation(recoveryAt, alertRegistryCompleteCoverage(recoveryAt), authoritativeNegative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, recovered, rpc.AlertEpisodeRecovered, rpc.AlertEvidenceCurrent)
}

func TestAlertEpisodeRegistryRecoversOnlyCurrentCoveredSourceUnderAggregatePartialCoverage(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 9, 30, 0, 0, time.UTC)
	expected := alertRegistryExpectedSources()
	complete := rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: base,
		ExpectedSources: expected, CoveredSources: append([]rpc.AlertSource(nil), expected...),
	}
	canary := alertRegistryObservation(t, "per-source-canary", base, true)
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, complete, canary)); err != nil {
		t.Fatal(err)
	}

	partialAt := base.Add(time.Minute)
	canaryNegative := canary
	canaryNegative.Active = false
	canaryNegative.ObservedAt = partialAt
	canaryNegative.EvidenceAsOf = partialAt
	canaryNegative.EvidenceFingerprint = alertRegistryFingerprint("per-source-canary-negative")
	canaryNegative.ProducerDecisionReason = "classified_clear"
	partial := rpc.AlertCoverage{
		State: rpc.AlertCoveragePartial, Freshness: rpc.AlertCoverageCurrent, AsOf: partialAt,
		ExpectedSources: expected, CoveredSources: []rpc.AlertSource{rpc.AlertSourceStress},
	}
	recovered, err := registry.Apply(t.Context(), alertRegistryEvaluation(partialAt, partial, canaryNegative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, recovered, rpc.AlertEpisodeRecovered, rpc.AlertEvidenceCurrent)
	if recovered.CurrentState != rpc.AlertSnapshotUnknown {
		t.Fatalf("aggregate partial coverage reported %s, want unknown", recovered.CurrentState)
	}

	reopenAt := base.Add(2 * time.Minute)
	canary.Active = true
	canary.ObservedAt = reopenAt
	canary.EvidenceAsOf = reopenAt
	canary.EvidenceFingerprint = alertRegistryFingerprint("per-source-canary-reopen")
	regime := alertRegistryObservation(t, "per-source-regime", reopenAt, true)
	regime.Source = rpc.AlertSourceRegime
	regime.Kind = rpc.AlertKindMarketState
	regime.PresentationCode = rpc.AlertPresentationRegimeMarketStress
	regime.EpisodeKey, err = rpc.BuildAlertEpisodeKey(regime.Source, regime.Kind, "per-source-regime")
	if err != nil {
		t.Fatal(err)
	}
	complete.AsOf = reopenAt
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(reopenAt, complete, canary, regime)); err != nil {
		t.Fatal(err)
	}

	mixedAt := base.Add(3 * time.Minute)
	canaryNegative = canary
	canaryNegative.Active = false
	canaryNegative.ObservedAt = mixedAt
	canaryNegative.EvidenceAsOf = mixedAt
	canaryNegative.EvidenceFingerprint = alertRegistryFingerprint("per-source-canary-negative-2")
	canaryNegative.ProducerDecisionReason = "classified_clear"
	regimeNegative := regime
	regimeNegative.Active = false
	regimeNegative.ObservedAt = mixedAt
	regimeNegative.EvidenceAsOf = mixedAt.Add(-time.Minute)
	regimeNegative.EvidenceFingerprint = alertRegistryFingerprint("per-source-regime-uncovered")
	regimeNegative.EvidenceHealth = rpc.AlertEvidencePartial
	regimeNegative.ProducerDecisionReason = "classified_clear"
	partial.AsOf = mixedAt
	mixed, err := registry.Apply(t.Context(), alertRegistryEvaluation(mixedAt, partial, canaryNegative, regimeNegative))
	if err != nil {
		t.Fatal(err)
	}
	states := make(map[rpc.AlertSource]rpc.AlertEpisodeState, len(mixed.Candidates))
	for _, candidate := range mixed.Candidates {
		states[candidate.Source] = candidate.State
	}
	if states[rpc.AlertSourceStress] != rpc.AlertEpisodeRecovered || states[rpc.AlertSourceRegime] != rpc.AlertEpisodeOpen {
		t.Fatalf("per-source recovery states=%v", states)
	}
}

func TestAlertEpisodeRegistryRejectsEquivocationAtomically(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	one := alertRegistryObservation(t, "equivocation", base, true)
	two := one
	two.Active = false
	two.EvidenceFingerprint = alertRegistryFingerprint("contradictory")
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), one, two)); err == nil || !strings.Contains(err.Error(), "equivocation") {
		t.Fatalf("duplicate equivocation error=%v", err)
	}
	if _, ok, err := registry.Snapshot(alertRegistryAuthority(), base); err != nil || ok {
		t.Fatalf("failed batch mutated current registry ok=%v err=%v", ok, err)
	}
	events, err := store.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: alertEpisodeDecisionEventType})
	if err != nil || len(events) != 0 {
		t.Fatalf("failed batch events=%d err=%v", len(events), err)
	}

	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), one)); err != nil {
		t.Fatal(err)
	}
	conflictingReplay := one
	conflictingReplay.EvidenceFingerprint = alertRegistryFingerprint("same-time-different-fact")
	later := base.Add(time.Minute)
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(later, alertRegistryCompleteCoverage(later), conflictingReplay)); err == nil || !strings.Contains(err.Error(), "timestamp equivocation") {
		t.Fatalf("timestamp equivocation error=%v", err)
	}
	events, err = store.LoadEvents(t.Context(), corestore.EventQuery{ScopeKey: daemonStateScope, Type: alertEpisodeDecisionEventType})
	if err != nil || len(events) != 1 {
		t.Fatalf("timestamp equivocation was not atomic events=%d err=%v", len(events), err)
	}
}

func TestAlertEpisodeRegistryBoundsRecoveredHistoryWithoutEvictingActive(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistryWithInactiveLimit(t.Context(), store, 2)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 11, 0, 0, 0, time.UTC)
	for i := range 4 {
		at := base.Add(time.Duration(i*2) * time.Minute)
		observation := alertRegistryObservation(t, "recovered-"+string(rune('a'+i)), at, true)
		if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(at, alertRegistryCompleteCoverage(at), observation)); err != nil {
			t.Fatal(err)
		}
		observation.Active = false
		observation.ObservedAt = at.Add(time.Minute)
		observation.EvidenceAsOf = observation.ObservedAt
		observation.EvidenceFingerprint = alertRegistryFingerprint("negative-" + string(rune('a'+i)))
		observation.ProducerDecisionReason = "classified_clear"
		if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(observation.ObservedAt, alertRegistryCompleteCoverage(observation.ObservedAt), observation)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(registry.document.Scopes[0].Episodes); got != 2 {
		t.Fatalf("recovered history=%d want 2", got)
	}
	for _, record := range registry.document.Scopes[0].Episodes {
		if record.State != rpc.AlertEpisodeRecovered {
			t.Fatalf("unexpected retained record: %+v", record)
		}
	}

	activeAt := base.Add(10 * time.Minute)
	active := []alertEpisodeObservation{
		alertRegistryObservation(t, "active-a", activeAt, true),
		alertRegistryObservation(t, "active-b", activeAt, true),
		alertRegistryObservation(t, "active-c", activeAt, true),
	}
	snapshot, err := registry.Apply(t.Context(), alertRegistryEvaluation(activeAt, alertRegistryCompleteCoverage(activeAt), active...))
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Candidates) != 3 || snapshot.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("active snapshot=%+v", snapshot)
	}
	activeCount, inactiveCount := 0, 0
	for _, record := range registry.document.Scopes[0].Episodes {
		if record.State == rpc.AlertEpisodeRecovered {
			inactiveCount++
		} else {
			activeCount++
		}
	}
	if activeCount != 3 || inactiveCount != 2 {
		t.Fatalf("bounded registry active=%d inactive=%d", activeCount, inactiveCount)
	}
}

func TestAlertEpisodeRegistryRejectsMalformedPersistedAuthority(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	_, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		JSON: []byte(`{"version":1,"as_of":"2026-07-21T12:00:00Z","next_occurrence_sequence":0,"coverage":{"state":"complete","freshness":"current","as_of":"2026-07-21T12:00:00Z","expected_sources":["canary"],"covered_sources":["canary"]},"episodes":[],"legacy_fallback":true}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newAlertEpisodeRegistry(t.Context(), store); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("malformed persisted authority error=%v", err)
	}
}

func TestAlertEpisodeRegistryMigratesV1ToPreservedUnscopedEvidence(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	at := time.Date(2026, 7, 21, 12, 30, 0, 0, time.UTC)
	legacy := alertEpisodeRegistryDocumentV1{
		Version: 1, AsOf: at, Coverage: alertRegistryCompleteCoverage(at),
		Episodes: []alertEpisodeRegistryRecordV1{},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind, JSON: raw,
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	if registry.revision != 2 || len(registry.document.Scopes) != 0 || registry.document.LegacyUnscoped == nil ||
		!json.Valid(registry.document.LegacyUnscoped.Document) || string(registry.document.LegacyUnscoped.Document) != string(raw) {
		t.Fatalf("legacy v1 evidence was not preserved exactly: revision=%d document=%+v", registry.revision, registry.document)
	}
	if snapshot, ok, err := registry.Snapshot(alertRegistryAuthority(), at); err != nil || ok || !snapshot.AsOf.IsZero() {
		t.Fatalf("unscoped v1 evidence became current authority: %+v ok=%v err=%v", snapshot, ok, err)
	}
	if _, err := newAlertEpisodeRegistry(t.Context(), store); err != nil {
		t.Fatalf("migrated v3 registry did not survive restart: %v", err)
	}
}

func TestAlertEpisodeRegistryMigratesV2WithoutLosingOccurrenceIdentity(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 21, 12, 40, 0, 0, time.UTC)
	observation := alertRegistryObservation(t, "v2-migration", at, true)
	observation.Source = rpc.AlertSourceRulebook
	observation.Kind = rpc.AlertKindGovernance
	observation.PresentationCode = rpc.AlertPresentationRulebookSingleNameExposure
	observation.EpisodeKey, err = rpc.BuildAlertEpisodeKey(observation.Source, observation.Kind, "v2-migration")
	if err != nil {
		t.Fatal(err)
	}
	coverage := rpc.AlertCoverage{State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRulebook}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceRulebook}}
	snapshot, err := registry.Apply(t.Context(), alertRegistryEvaluation(at, coverage, observation))
	if err != nil {
		t.Fatal(err)
	}
	wantOccurrence := snapshot.Candidates[0].OccurrenceKey

	raw, err := json.Marshal(registry.document)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["version"] = float64(2)
	for _, scope := range legacy["scopes"].([]any) {
		for _, episode := range scope.(map[string]any)["episodes"].([]any) {
			record := episode.(map[string]any)
			delete(record, "presentation_code")
			record["delivery_preference"] = "unapproved"
		}
	}
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		ExpectedRevision: registry.revision, JSON: legacyRaw,
	}); err != nil {
		t.Fatal(err)
	}

	migrated, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := migrated.Snapshot(alertRegistryAuthority(), at)
	if err != nil || !ok || len(got.Candidates) != 1 {
		t.Fatalf("migrated snapshot=%+v ok=%v err=%v", got, ok, err)
	}
	if got.Candidates[0].OccurrenceKey != wantOccurrence || got.Candidates[0].PresentationCode != rpc.AlertPresentationRulebookLegacyCondition {
		t.Fatalf("v2 lifecycle identity changed: %+v", got.Candidates[0])
	}
	persisted, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, alertEpisodeRegistryStateKind)
	if err != nil || !ok || strings.Contains(string(persisted.JSON), "delivery_preference") {
		t.Fatalf("v2 delivery axis survived migration: ok=%v err=%v json=%s", ok, err, persisted.JSON)
	}
}

func TestAlertEpisodeRegistryRejectsMalformedDurableCommissioningMetrics(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 21, 12, 45, 0, 0, time.UTC)
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(at, alertRegistryCompleteCoverage(at))); err != nil {
		t.Fatal(err)
	}
	malformed := cloneAlertEpisodeRegistryDocument(registry.document)
	measurement := &malformed.Scopes[0].Metrics.Sources[0].Measurements
	measurement.TimeToObserveSamples = 1
	measurement.TimeToObserveTotal = -time.Second
	raw, err := json.Marshal(malformed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		ExpectedRevision: registry.revision, JSON: raw,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := newAlertEpisodeRegistry(t.Context(), store); err == nil || !strings.Contains(err.Error(), "negative latency") {
		t.Fatalf("malformed durable metrics error=%v", err)
	}
}

func alertRegistryTestPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "daemon.db")
}

func openAlertRegistryTestStore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("open alert registry test store: %v", err)
	}
	return store
}

func alertRegistryObservation(t *testing.T, identity string, at time.Time, active bool) alertEpisodeObservation {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk, identity)
	if err != nil {
		t.Fatal(err)
	}
	return alertEpisodeObservation{
		EpisodeKey: episode, Source: rpc.AlertSourceStress, Kind: rpc.AlertKindPortfolioRisk,
		PresentationCode: rpc.AlertPresentationPortfolioStress, Active: active, Severity: rpc.AlertSeverityWatch,
		EvidenceFingerprint: alertRegistryFingerprint("evidence-" + identity), EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, ObservedAt: at,
		PolicyFingerprint: alertRegistryFingerprint("policy-v1"), ProducerDecisionReason: "classified_active",
	}
}

func alertRegistryEvaluation(at time.Time, coverage rpc.AlertCoverage, observations ...alertEpisodeObservation) alertEpisodeEvaluation {
	if observations == nil {
		observations = []alertEpisodeObservation{}
	}
	return alertEpisodeEvaluation{AuthorityScope: alertRegistryAuthority(), AsOf: at, Coverage: coverage, Observations: observations}
}

func alertRegistryAuthority() string {
	authority, err := rpc.BuildAlertAuthorityScope("DU-REGISTRY", rpc.AccountModePaper)
	if err != nil {
		panic(err)
	}
	return authority
}

func alertRegistryCompleteCoverage(at time.Time) rpc.AlertCoverage {
	return rpc.AlertCoverage{
		State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceStress},
	}
}

func alertRegistryExpectedSources() []rpc.AlertSource {
	return []rpc.AlertSource{
		rpc.AlertSourceStress,
		rpc.AlertSourceRegime,
		rpc.AlertSourceRulebook,
		rpc.AlertSourceRiskPolicy,
		rpc.AlertSourceProtection,
		rpc.AlertSourceOrderIntegrity,
		rpc.AlertSourceReconciliation,
		rpc.AlertSourceGovernance,
		rpc.AlertSourceDataHealth,
	}
}

func alertRegistryFingerprint(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func assertAlertRegistryCandidate(t *testing.T, snapshot rpc.AlertCandidateSnapshot, state rpc.AlertEpisodeState, health rpc.AlertEvidenceHealth) {
	t.Helper()
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		t.Fatalf("invalid snapshot: %v", err)
	}
	if len(snapshot.Candidates) != 1 || snapshot.Candidates[0].State != state || snapshot.Candidates[0].EvidenceHealth != health {
		t.Fatalf("candidate=%+v want state=%s health=%s", snapshot.Candidates, state, health)
	}
}
