package rpc

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

func TestAlertCandidateRPCTypesAreLosslessAliases(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	key, err := BuildAlertEpisodeKey(AlertSourceRegime, AlertKindMarketState, "classified-regime-episode")
	if err != nil {
		t.Fatal(err)
	}
	riskValue := risk.AlertCandidate{
		EpisodeKey:          key,
		OccurrenceKey:       mustRPCAlertOccurrenceKey(t, key, "occurrence-1"),
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source:              risk.AlertSourceRegime,
		Kind:                risk.AlertKindMarketState,
		PresentationCode:    risk.AlertPresentationRegimeMarketStress,
		State:               risk.AlertEpisodeOpen,
		Severity:            risk.AlertSeverityWatch,
		EvidenceHealth:      risk.AlertEvidenceCurrent,
		Destination:         risk.AlertDestinationMonitor,
		EvidenceAsOf:        now.Add(-time.Minute),
		StateChangedAt:      now.Add(-2 * time.Minute),
		ObservedAt:          now,
	}
	var wireValue AlertCandidate = riskValue
	var backToRisk risk.AlertCandidate = wireValue
	if !reflect.DeepEqual(backToRisk, riskValue) {
		t.Fatalf("RPC conversion lost fields: got %#v want %#v", backToRisk, riskValue)
	}
	if err := ValidateAlertCandidate(wireValue); err != nil {
		t.Fatal(err)
	}
	if got, err := risk.BuildAlertEpisodeKey(risk.AlertSourceRegime, risk.AlertKindMarketState, "classified-regime-episode"); err != nil || got != key {
		t.Fatalf("RPC episode helper drifted: got=%q err=%v want=%q", got, err, key)
	}
}

func TestAlertCandidateRPCJSONUsesRiskValidation(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	key, err := BuildAlertEpisodeKey(AlertSourceRegime, AlertKindMarketState, "classified-regime-episode")
	if err != nil {
		t.Fatal(err)
	}
	candidate := AlertCandidate{
		EpisodeKey:          key,
		OccurrenceKey:       mustRPCAlertOccurrenceKey(t, key, "occurrence-1"),
		EvidenceFingerprint: "sha256:" + strings.Repeat("b", 64),
		Source:              AlertSourceRegime,
		Kind:                AlertKindMarketState,
		PresentationCode:    AlertPresentationRegimeMarketStress,
		State:               AlertEpisodeOpen,
		Severity:            AlertSeverityWatch,
		EvidenceHealth:      AlertEvidenceCurrent,
		Destination:         AlertDestinationMonitor,
		EvidenceAsOf:        now.Add(-time.Minute),
		StateChangedAt:      now.Add(-2 * time.Minute),
		ObservedAt:          now,
	}
	snapshot := AlertCandidateSnapshot{
		SchemaVersion: AlertCandidateSnapshotVersion,
		AuthorityScope: func() string {
			scope, err := BuildAlertAuthorityScope("DU-TEST", "paper")
			if err != nil {
				t.Fatal(err)
			}
			return scope
		}(),
		AsOf:         now,
		CurrentState: AlertSnapshotActive,
		Coverage: AlertCoverage{
			State:           AlertCoverageComplete,
			Freshness:       AlertCoverageCurrent,
			AsOf:            now,
			ExpectedSources: []AlertSource{AlertSourceRegime},
			CoveredSources:  []AlertSource{AlertSourceRegime},
		},
		Sources: []AlertSourceCoverage{{
			Source: AlertSourceRegime, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent,
			InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true,
		}},
		Candidates: []AlertCandidate{candidate},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AlertCandidateSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, snapshot) {
		t.Fatalf("RPC JSON round trip mismatch: got %#v want %#v", decoded, snapshot)
	}
	if err := ValidateAlertCandidateSnapshot(decoded); err != nil {
		t.Fatal(err)
	}

	hostile := strings.Replace(string(raw), `"destination":"monitor"`, `"destination":"monitor","device_id":"private"`, 1)
	if err := json.Unmarshal([]byte(hostile), &decoded); err == nil {
		t.Fatal("RPC alias accepted private delivery target extension")
	}
}

func TestAlertCandidateRPCDoesNotAddDisplayOrDeliveryIdentity(t *testing.T) {
	candidateType := reflect.TypeFor[AlertCandidate]()
	if _, ok := candidateType.FieldByName("OccurrenceKey"); !ok {
		t.Fatal("RPC candidate is missing daemon-authored OccurrenceKey")
	}
	for _, forbidden := range []string{"Title", "Body", "Message", "Details", "DisplayID", "TargetID", "DeviceID", "DeliveryID"} {
		if _, ok := candidateType.FieldByName(forbidden); ok {
			t.Fatalf("RPC candidate exposes forbidden field %q", forbidden)
		}
	}
	if got, want := candidateType.NumField(), reflect.TypeFor[risk.AlertCandidate]().NumField(); got != want {
		t.Fatalf("RPC candidate field count = %d, risk contract = %d", got, want)
	}
}

func mustRPCAlertOccurrenceKey(t *testing.T, episodeKey string, identity string) string {
	t.Helper()
	key, err := BuildAlertOccurrenceKey(episodeKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
