package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// legacyAlertCandidate is the exact candidate shape written by alert delivery
// v2. DeliveryPreference is decoded and validated only so malformed legacy
// state fails closed; it is never copied into the active v3 authority.
type legacyAlertCandidate struct {
	EpisodeKey          string                  `json:"episode_key"`
	OccurrenceKey       string                  `json:"occurrence_key"`
	EvidenceFingerprint string                  `json:"evidence_fingerprint"`
	Source              rpc.AlertSource         `json:"source"`
	Kind                rpc.AlertKind           `json:"kind"`
	State               rpc.AlertEpisodeState   `json:"state"`
	Severity            rpc.AlertSeverity       `json:"severity"`
	DeliveryPreference  string                  `json:"delivery_preference"`
	EvidenceHealth      rpc.AlertEvidenceHealth `json:"evidence_health"`
	Destination         rpc.AlertDestination    `json:"destination"`
	EvidenceAsOf        time.Time               `json:"evidence_as_of"`
	StateChangedAt      time.Time               `json:"state_changed_at"`
	ObservedAt          time.Time               `json:"observed_at"`
}

func (candidate legacyAlertCandidate) current() rpc.AlertCandidate {
	return rpc.AlertCandidate{
		EpisodeKey: candidate.EpisodeKey, OccurrenceKey: candidate.OccurrenceKey,
		EvidenceFingerprint: candidate.EvidenceFingerprint, Source: candidate.Source, Kind: candidate.Kind,
		PresentationCode: legacyAlertPresentationCode(candidate.Source), State: candidate.State, Severity: candidate.Severity,
		EvidenceHealth: candidate.EvidenceHealth, Destination: candidate.Destination,
		EvidenceAsOf: candidate.EvidenceAsOf, StateChangedAt: candidate.StateChangedAt, ObservedAt: candidate.ObservedAt,
	}
}

func validLegacyAlertDeliveryPreference(value string) bool {
	switch value {
	case "unapproved", "record_only", "inbox", "digest", "page":
		return true
	default:
		return false
	}
}

type legacyAlertCandidateSnapshotV2 struct {
	SchemaVersion  string                 `json:"schema_version"`
	AuthorityScope string                 `json:"authority_scope"`
	AsOf           time.Time              `json:"as_of"`
	CurrentState   rpc.AlertSnapshotState `json:"current_state"`
	Coverage       rpc.AlertCoverage      `json:"coverage"`
	Candidates     []legacyAlertCandidate `json:"candidates"`
}

func (snapshot legacyAlertCandidateSnapshotV2) validationSnapshot() (rpc.AlertCandidateSnapshot, error) {
	if snapshot.SchemaVersion != legacyAlertSnapshotVersionV2 {
		return rpc.AlertCandidateSnapshot{}, errors.New("invalid legacy v2 alert candidate snapshot schema_version")
	}
	candidates := make([]rpc.AlertCandidate, 0, len(snapshot.Candidates))
	for i, candidate := range snapshot.Candidates {
		if !validLegacyAlertDeliveryPreference(candidate.DeliveryPreference) {
			return rpc.AlertCandidateSnapshot{}, fmt.Errorf("invalid legacy v2 alert candidate delivery_preference at index %d", i)
		}
		candidates = append(candidates, candidate.current())
	}
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: snapshot.AuthorityScope,
		AsOf: snapshot.AsOf, CurrentState: snapshot.CurrentState, Coverage: cloneAlertCoverage(snapshot.Coverage),
		Sources: legacyAlertSourceRows(snapshot.Coverage), Candidates: candidates,
	}, nil
}

type legacyAlertDeliveryOccurrenceV2 struct {
	AuthorityScope      string                  `json:"authority_scope"`
	OccurrenceKey       string                  `json:"occurrence_key"`
	EpisodeKey          string                  `json:"episode_key"`
	EvidenceFingerprint string                  `json:"evidence_fingerprint"`
	DisplayID           string                  `json:"display_id"`
	Source              rpc.AlertSource         `json:"source"`
	Kind                rpc.AlertKind           `json:"kind"`
	State               rpc.AlertEpisodeState   `json:"state"`
	Severity            rpc.AlertSeverity       `json:"severity"`
	DeliveryPreference  string                  `json:"delivery_preference"`
	EvidenceHealth      rpc.AlertEvidenceHealth `json:"evidence_health"`
	Destination         rpc.AlertDestination    `json:"destination"`
	EvidenceAsOf        time.Time               `json:"evidence_as_of"`
	StateChangedAt      time.Time               `json:"state_changed_at"`
	ObservedAt          time.Time               `json:"observed_at"`
	FirstSeenAt         time.Time               `json:"first_seen_at"`
	LastSeenAt          time.Time               `json:"last_seen_at"`
	EndedAt             time.Time               `json:"ended_at,omitzero"`
	EndReason           string                  `json:"end_reason,omitempty"`
	AttentionSeq        uint64                  `json:"attention_v2_seq"`
	TransportEligible   bool                    `json:"transport_eligible"`
}

func (occurrence legacyAlertDeliveryOccurrenceV2) current() (alertDeliveryOccurrence, error) {
	if !validLegacyAlertDeliveryPreference(occurrence.DeliveryPreference) {
		return alertDeliveryOccurrence{}, errors.New("invalid legacy v2 occurrence delivery_preference")
	}
	return alertDeliveryOccurrence{
		AuthorityScope: occurrence.AuthorityScope, OccurrenceKey: occurrence.OccurrenceKey, EpisodeKey: occurrence.EpisodeKey,
		EvidenceFingerprint: occurrence.EvidenceFingerprint, DisplayID: occurrence.DisplayID,
		Source: occurrence.Source, Kind: occurrence.Kind, PresentationCode: legacyAlertPresentationCode(occurrence.Source),
		State: occurrence.State, Severity: occurrence.Severity, EvidenceHealth: occurrence.EvidenceHealth,
		Destination: occurrence.Destination, EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt,
		ObservedAt: occurrence.ObservedAt, FirstSeenAt: occurrence.FirstSeenAt, LastSeenAt: occurrence.LastSeenAt,
		EndedAt: occurrence.EndedAt, EndReason: occurrence.EndReason, AttentionSeq: occurrence.AttentionSeq,
		Disposition: AlertDispositionCutoverExisting,
	}, nil
}

type legacyAlertDeliveryPreviousContextV2 struct {
	AuthorityScope     string                  `json:"authority_scope"`
	ArchiveSeq         uint64                  `json:"archive_seq"`
	DisplayID          string                  `json:"display_id"`
	PriorDisplayID     string                  `json:"prior_display_id"`
	Source             rpc.AlertSource         `json:"source"`
	Kind               rpc.AlertKind           `json:"kind"`
	State              rpc.AlertEpisodeState   `json:"state"`
	Severity           rpc.AlertSeverity       `json:"severity"`
	DeliveryPreference string                  `json:"delivery_preference"`
	EvidenceHealth     rpc.AlertEvidenceHealth `json:"evidence_health"`
	Destination        rpc.AlertDestination    `json:"destination"`
	EvidenceAsOf       time.Time               `json:"evidence_as_of"`
	StateChangedAt     time.Time               `json:"state_changed_at"`
	FirstSeenAt        time.Time               `json:"first_seen_at"`
	LastSeenAt         time.Time               `json:"last_seen_at"`
	EndedAt            time.Time               `json:"ended_at"`
	EndReason          string                  `json:"end_reason"`
	OriginalAttention  uint64                  `json:"original_attention_v2_seq"`
}

func (previous legacyAlertDeliveryPreviousContextV2) current() (alertDeliveryPreviousContext, error) {
	if !validLegacyAlertDeliveryPreference(previous.DeliveryPreference) {
		return alertDeliveryPreviousContext{}, errors.New("invalid legacy v2 previous-context delivery_preference")
	}
	return alertDeliveryPreviousContext{
		AuthorityScope: previous.AuthorityScope, ArchiveSeq: previous.ArchiveSeq, DisplayID: previous.DisplayID,
		PriorDisplayID: previous.PriorDisplayID, Source: previous.Source, Kind: previous.Kind,
		PresentationCode: legacyAlertPresentationCode(previous.Source), State: previous.State, Severity: previous.Severity,
		EvidenceHealth: previous.EvidenceHealth, Destination: previous.Destination, EvidenceAsOf: previous.EvidenceAsOf,
		StateChangedAt: previous.StateChangedAt, FirstSeenAt: previous.FirstSeenAt, LastSeenAt: previous.LastSeenAt,
		EndedAt: previous.EndedAt, EndReason: previous.EndReason, OriginalAttention: previous.OriginalAttention,
		Disposition: AlertDispositionCutoverExisting,
	}, nil
}

type legacyAlertDeliveryDataV2 struct {
	Version                  string                                   `json:"version"`
	Generation               uint64                                   `json:"generation"`
	Snapshot                 legacyAlertCandidateSnapshotV2           `json:"snapshot"`
	SourceWatermarks         map[rpc.AlertSource]time.Time            `json:"source_watermarks"`
	SourceWatermarksByScope  map[string]map[rpc.AlertSource]time.Time `json:"source_watermarks_by_scope"`
	Episodes                 []alertDeliveryEpisode                   `json:"episodes,omitempty"`
	Occurrences              []legacyAlertDeliveryOccurrenceV2        `json:"occurrences,omitempty"`
	PreviousContexts         []legacyAlertDeliveryPreviousContextV2   `json:"previous_contexts,omitempty"`
	PreviousContextHighWater uint64                                   `json:"previous_context_high_water_seq"`
	Attempts                 []alertDeliveryAttempt                   `json:"attempts,omitempty"`
	Receipts                 []alertDeliveryReceipt                   `json:"receipts,omitempty"`
	RetiredTargets           map[string]time.Time                     `json:"retired_targets"`
	Health                   AlertDeliveryHealth                      `json:"delivery_health"`
	AttentionHighWaterSeq    uint64                                   `json:"attention_v2_high_water_seq"`
	AttentionReadThroughSeq  uint64                                   `json:"attention_v2_read_through_seq"`
}

func decodeAlertDeliveryV2(raw []byte) (*alertDeliveryData, error) {
	var legacy legacyAlertDeliveryDataV2
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&legacy); err != nil {
		return nil, fmt.Errorf("decode exact alert delivery v2: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode exact alert delivery v2: trailing data")
	}
	if legacy.Version != legacyAlertDeliveryVersionV2 {
		return nil, errors.New("invalid alert delivery v2 version")
	}
	validationSnapshot, err := legacy.Snapshot.validationSnapshot()
	if err != nil {
		return nil, err
	}
	if err := rpc.ValidateAlertCandidateSnapshot(validationSnapshot); err != nil {
		return nil, fmt.Errorf("invalid alert delivery v2 snapshot: %w", err)
	}

	current := &alertDeliveryData{
		Version: AlertDeliveryVersion, Generation: legacy.Generation,
		SourceWatermarks:         cloneAlertSourceWatermarks(legacy.SourceWatermarks),
		SourceWatermarksByScope:  cloneAlertSourceWatermarksByScope(legacy.SourceWatermarksByScope),
		Episodes:                 append([]alertDeliveryEpisode(nil), legacy.Episodes...),
		PreviousContextHighWater: legacy.PreviousContextHighWater,
		Attempts:                 append([]alertDeliveryAttempt(nil), legacy.Attempts...), Receipts: append([]alertDeliveryReceipt(nil), legacy.Receipts...),
		RetiredTargets: make(map[string]time.Time, len(legacy.RetiredTargets)), Baselines: make(map[string]alertDeliveryBaseline),
		Health: legacy.Health, AttentionHighWaterSeq: legacy.AttentionHighWaterSeq,
		AttentionReadThroughSeq: legacy.AttentionReadThroughSeq, migratedV2: true,
	}
	maps.Copy(current.RetiredTargets, legacy.RetiredTargets)
	for _, occurrence := range legacy.Occurrences {
		converted, err := occurrence.current()
		if err != nil {
			return nil, err
		}
		current.Occurrences = append(current.Occurrences, converted)
	}
	for _, previous := range legacy.PreviousContexts {
		converted, err := previous.current()
		if err != nil {
			return nil, err
		}
		current.PreviousContexts = append(current.PreviousContexts, converted)
	}
	for i := range current.Attempts {
		if current.Attempts[i].Class == "policy_unapproved" {
			current.Attempts[i].Class = AlertDeliveryAttemptModeSuppressed
		}
	}
	if current.Health.State == "shadow" {
		current.Health.State = AlertDeliveryHealthHealthy
		current.Health.Class = ""
	}
	current.Snapshot = failClosedMigratedAlertSnapshot(legacy.Snapshot, current)
	return current, nil
}

func failClosedMigratedAlertSnapshot(legacy legacyAlertCandidateSnapshotV2, data *alertDeliveryData) rpc.AlertCandidateSnapshot {
	expected := append([]rpc.AlertSource(nil), legacy.Coverage.ExpectedSources...)
	coverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: legacy.AsOf,
		ExpectedSources: expected, CoveredSources: []rpc.AlertSource{},
	}
	snapshot := rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: legacy.AuthorityScope,
		AsOf: legacy.AsOf, CurrentState: rpc.AlertSnapshotUnknown, Coverage: coverage,
		Sources: unavailableAlertSourceRows(expected, "cutover_unverified"), Candidates: []rpc.AlertCandidate{},
	}
	for _, episode := range data.Episodes {
		if episode.AuthorityScope != legacy.AuthorityScope || episode.State == rpc.AlertEpisodeRecovered {
			continue
		}
		occurrence, _, ok := findAlertDeliveryOccurrence(data, episode.AuthorityScope, episode.CurrentOccurrenceKey)
		if !ok || !occurrence.EndedAt.IsZero() {
			continue
		}
		candidate := alertCandidateFromOccurrence(occurrence)
		candidate.EvidenceHealth = rpc.AlertEvidenceUnavailable
		snapshot.Candidates = append(snapshot.Candidates, candidate)
	}
	if len(snapshot.Candidates) > 0 {
		snapshot.CurrentState = rpc.AlertSnapshotActive
	}
	return snapshot
}

func legacyAlertSourceRows(coverage rpc.AlertCoverage) []rpc.AlertSourceCoverage {
	covered := make(map[rpc.AlertSource]bool, len(coverage.CoveredSources))
	for _, source := range coverage.CoveredSources {
		covered[source] = true
	}
	rows := make([]rpc.AlertSourceCoverage, 0, len(coverage.ExpectedSources))
	for _, source := range coverage.ExpectedSources {
		if !covered[source] {
			rows = append(rows, rpc.AlertSourceCoverage{Source: source, Status: "unavailable", Reason: "legacy_v2_uncovered", EvidenceHealth: rpc.AlertEvidenceUnavailable})
			continue
		}
		health, status := rpc.AlertEvidenceCurrent, "current"
		if coverage.Freshness == rpc.AlertCoverageStale {
			health, status = rpc.AlertEvidenceStale, "stale"
		}
		rows = append(rows, rpc.AlertSourceCoverage{
			Source: source, Status: status, Reason: "legacy_v2", EvidenceHealth: health,
			InputAsOf: coverage.AsOf, ObservedAt: coverage.AsOf, EvidenceAsOf: coverage.AsOf, FreshUntil: coverage.AsOf, Covered: true,
		})
	}
	slices.SortFunc(rows, func(a, b rpc.AlertSourceCoverage) int {
		if a.Source < b.Source {
			return -1
		}
		if a.Source > b.Source {
			return 1
		}
		return 0
	})
	return rows
}

func unavailableAlertSourceRows(expected []rpc.AlertSource, reason string) []rpc.AlertSourceCoverage {
	rows := make([]rpc.AlertSourceCoverage, 0, len(expected))
	for _, source := range expected {
		rows = append(rows, rpc.AlertSourceCoverage{
			Source: source, Status: "unavailable", Reason: reason, EvidenceHealth: rpc.AlertEvidenceUnavailable,
		})
	}
	slices.SortFunc(rows, func(a, b rpc.AlertSourceCoverage) int {
		if a.Source < b.Source {
			return -1
		}
		if a.Source > b.Source {
			return 1
		}
		return 0
	})
	return rows
}

func legacyAlertPresentationCode(source rpc.AlertSource) rpc.AlertPresentationCode {
	switch source {
	case rpc.AlertSourceStress:
		return rpc.AlertPresentationPortfolioStress
	case rpc.AlertSourceRegime:
		return rpc.AlertPresentationRegimeMarketStress
	case rpc.AlertSourceRulebook:
		return rpc.AlertPresentationRulebookLegacyCondition
	case rpc.AlertSourceRiskPolicy:
		return rpc.AlertPresentationRiskPolicyLegacyCondition
	case rpc.AlertSourceProtection:
		return rpc.AlertPresentationProtectionReconciliationRequired
	case rpc.AlertSourceOrderIntegrity:
		return rpc.AlertPresentationOrderIntegrityMismatch
	case rpc.AlertSourceReconciliation:
		return rpc.AlertPresentationReconciliationLegacyCondition
	case rpc.AlertSourceGovernance:
		return rpc.AlertPresentationGovernanceLegacyCondition
	case rpc.AlertSourceDataHealth:
		return rpc.AlertPresentationDataHealthQuality
	case rpc.AlertSourceDelivery:
		return rpc.AlertPresentationDeliveryHealth
	default:
		return ""
	}
}
