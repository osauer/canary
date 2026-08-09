package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestAlertEpisodeRegistryCurrentLifecycleSurvivesRestart(t *testing.T) {
	path := alertRegistryTestPath(t)
	store := openAlertRegistryTestStore(t, path)
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 21, 8, 0, 0, 0, time.UTC)
	positive := alertRegistryObservation(t, "lifecycle", base, true)
	opened, err := registry.Apply(t.Context(), alertRegistryEvaluation(base, alertRegistryCompleteCoverage(base), positive))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, opened, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	occurrence := opened.Candidates[0].OccurrenceKey

	// Missing producer evidence must retain the active fact and make its data
	// quality explicit; it is not negative evidence.
	outageAt := base.Add(time.Minute)
	outage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: outageAt,
		ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress}, CoveredSources: []rpc.AlertSource{},
	}
	held, err := registry.Apply(t.Context(), alertRegistryEvaluation(outageAt, outage))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, held, rpc.AlertEpisodeOpen, rpc.AlertEvidenceUnavailable)
	if held.Candidates[0].OccurrenceKey != occurrence {
		t.Fatal("outage rotated active occurrence")
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openAlertRegistryTestStore(t, path)
	registry, err = newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	restarted, ok, err := registry.Snapshot(alertRegistryAuthority(), outageAt)
	if err != nil || !ok || restarted.Candidates[0].OccurrenceKey != occurrence {
		t.Fatalf("restart snapshot=%+v ok=%v err=%v", restarted, ok, err)
	}

	negativeAt := base.Add(2 * time.Minute)
	negative := positive
	negative.Active = false
	negative.ObservedAt, negative.EvidenceAsOf = negativeAt, negativeAt
	negative.EvidenceFingerprint = alertRegistryFingerprint("authoritative-negative")
	negative.ProducerDecisionReason = "classified_clear"
	recovered, err := registry.Apply(t.Context(), alertRegistryEvaluation(negativeAt, alertRegistryCompleteCoverage(negativeAt), negative))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, recovered, rpc.AlertEpisodeRecovered, rpc.AlertEvidenceCurrent)
	if recovered.CurrentState != rpc.AlertSnapshotClear || recovered.Candidates[0].OccurrenceKey != occurrence {
		t.Fatalf("recovery changed occurrence or clear state: %+v", recovered)
	}

	confirmedAt := base.Add(3 * time.Minute)
	negative.ObservedAt, negative.EvidenceAsOf = confirmedAt, confirmedAt
	negative.EvidenceFingerprint = alertRegistryFingerprint("still-clear")
	clear, err := registry.Apply(t.Context(), alertRegistryEvaluation(confirmedAt, alertRegistryCompleteCoverage(confirmedAt), negative))
	if err != nil || clear.CurrentState != rpc.AlertSnapshotClear || len(clear.Candidates) != 0 {
		t.Fatalf("confirmed clear snapshot=%+v err=%v", clear, err)
	}

	reopenAt := base.Add(4 * time.Minute)
	positive.ObservedAt, positive.EvidenceAsOf = reopenAt, reopenAt
	positive.EvidenceFingerprint = alertRegistryFingerprint("reopened")
	reopened, err := registry.Apply(t.Context(), alertRegistryEvaluation(reopenAt, alertRegistryCompleteCoverage(reopenAt), positive))
	if err != nil {
		t.Fatal(err)
	}
	assertAlertRegistryCandidate(t, reopened, rpc.AlertEpisodeOpen, rpc.AlertEvidenceCurrent)
	if reopened.Candidates[0].OccurrenceKey == occurrence {
		t.Fatal("reopen reused recovered occurrence")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAlertEpisodeRegistryRejectsEquivocationAtomically(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	registry, err := newAlertEpisodeRegistry(t.Context(), store)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	one := alertRegistryObservation(t, "equivocation", at, true)
	two := one
	two.Active = false
	two.EvidenceFingerprint = alertRegistryFingerprint("contradictory")
	if _, err := registry.Apply(t.Context(), alertRegistryEvaluation(at, alertRegistryCompleteCoverage(at), one, two)); err == nil || !strings.Contains(err.Error(), "equivocation") {
		t.Fatalf("equivocation error=%v", err)
	}
	if _, ok, err := registry.Snapshot(alertRegistryAuthority(), at); err != nil || ok {
		t.Fatalf("failed batch mutated registry ok=%v err=%v", ok, err)
	}
}

func TestAlertEpisodeRegistryRejectsPreBridgeDocument(t *testing.T) {
	store := openAlertRegistryTestStore(t, alertRegistryTestPath(t))
	defer store.Close()
	_, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		JSON: []byte(`{"version":3,"updated_at":"2026-07-21T12:00:00Z","scopes":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newAlertEpisodeRegistry(t.Context(), store); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("pre-bridge document error=%v", err)
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
