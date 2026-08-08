package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	alertEpisodeRegistryDocumentVersion = 4
	alertEpisodeRegistryStateKind       = "alert_episode_registry"
	alertEpisodeDecisionEventType       = "alert_episode_decision"
	alertEpisodeDecisionEventAction     = "evaluate"

	// This is a storage bound, not an alert, escalation, or delivery policy.
	// Active episodes are never counted against it and are never evicted.
	alertEpisodeRegistryInactiveLimit = 256
	alertEpisodeRegistryCASAttempts   = 8

	alertShadowDispositionDuplicate    = "duplicate"
	alertShadowDispositionStale        = "stale"
	alertShadowDispositionEquivocation = "equivocation"
)

// alertEpisodeObservation is an already-classified, redacted producer fact.
// The registry does not decide whether a condition is active, its severity,
// presentation, or whether an escalation qualifies. A non-empty
// EscalationFingerprint is the producer's stable identity for a qualifying
// escalation; replaying the same identity does not mint another occurrence.
type alertEpisodeObservation struct {
	EpisodeKey             string
	Source                 rpc.AlertSource
	Kind                   rpc.AlertKind
	PresentationCode       rpc.AlertPresentationCode
	Active                 bool
	EscalationFingerprint  string
	Severity               rpc.AlertSeverity
	EvidenceFingerprint    string
	EvidenceHealth         rpc.AlertEvidenceHealth
	Destination            rpc.AlertDestination
	EvidenceAsOf           time.Time
	ObservedAt             time.Time
	PolicyFingerprint      string
	ProducerDecisionReason string
}

// alertEpisodeEvaluation is one source-coverage boundary. Absence from
// Observations is explicitly an omission, never negative evidence. An active
// episode recovers only from an Active=false observation inside complete,
// current coverage with current evidence for that covered source.
type alertEpisodeEvaluation struct {
	AuthorityScope       string
	AsOf                 time.Time
	Coverage             rpc.AlertCoverage
	Observations         []alertEpisodeObservation
	OpportunitySources   []rpc.AlertSource
	SourceStates         []alertEpisodeRegistrySourceState
	CursorKind           string
	Cursor               alertShadowInputCursor
	PendingApplyFailures uint64
}

// alertEpisodeRegistry owns producer lifecycle identity and its durable audit.
// It has no sender, target, page policy, cooldown, or threshold.
type alertEpisodeRegistry struct {
	mu            sync.Mutex
	core          *corestore.Store
	revision      int64
	document      alertEpisodeRegistryDocument
	inactiveLimit int
}

type alertEpisodeRegistryDocument struct {
	Version                int                                 `json:"version"`
	UpdatedAt              time.Time                           `json:"updated_at"`
	NextOccurrenceSequence uint64                              `json:"next_occurrence_sequence"`
	Scopes                 []alertEpisodeRegistryScopeDocument `json:"scopes"`
	LegacyUnscoped         *alertEpisodeLegacyUnscoped         `json:"legacy_unscoped,omitempty"`
}

type alertEpisodeLegacyUnscoped struct {
	MigratedAt  time.Time       `json:"migrated_at"`
	Fingerprint string          `json:"fingerprint"`
	Document    json.RawMessage `json:"document"`
}

type alertEpisodeRegistryDocumentV1 struct {
	Version                int                            `json:"version"`
	AsOf                   time.Time                      `json:"as_of"`
	NextOccurrenceSequence uint64                         `json:"next_occurrence_sequence"`
	Coverage               rpc.AlertCoverage              `json:"coverage"`
	Episodes               []alertEpisodeRegistryRecordV1 `json:"episodes"`
}

type alertEpisodeRegistryRecordV1 struct {
	EpisodeKey                 string                  `json:"episode_key"`
	OccurrenceKey              string                  `json:"occurrence_key"`
	Source                     rpc.AlertSource         `json:"source"`
	Kind                       rpc.AlertKind           `json:"kind"`
	State                      rpc.AlertEpisodeState   `json:"state"`
	Severity                   rpc.AlertSeverity       `json:"severity"`
	DeliveryPreference         string                  `json:"delivery_preference"`
	EvidenceFingerprint        string                  `json:"evidence_fingerprint"`
	EvidenceHealth             rpc.AlertEvidenceHealth `json:"evidence_health"`
	Destination                rpc.AlertDestination    `json:"destination"`
	EvidenceAsOf               time.Time               `json:"evidence_as_of"`
	StateChangedAt             time.Time               `json:"state_changed_at"`
	ObservedAt                 time.Time               `json:"observed_at"`
	PolicyFingerprint          string                  `json:"policy_fingerprint"`
	ProducerDecisionReason     string                  `json:"producer_decision_reason"`
	RegistryDecisionReason     string                  `json:"registry_decision_reason"`
	LastSourceObservedAt       time.Time               `json:"last_source_observed_at"`
	LastObservationFingerprint string                  `json:"last_observation_fingerprint"`
	LastEscalationFingerprint  string                  `json:"last_escalation_fingerprint,omitempty"`
	EmitRecovered              bool                    `json:"emit_recovered"`
}

type alertEpisodeRegistryDocumentV2 struct {
	Version                int                                   `json:"version"`
	UpdatedAt              time.Time                             `json:"updated_at"`
	NextOccurrenceSequence uint64                                `json:"next_occurrence_sequence"`
	Scopes                 []alertEpisodeRegistryScopeDocumentV2 `json:"scopes"`
	LegacyUnscoped         *alertEpisodeLegacyUnscoped           `json:"legacy_unscoped,omitempty"`
}

type alertEpisodeRegistryScopeDocumentV2 struct {
	AuthorityScope string                            `json:"authority_scope"`
	AsOf           time.Time                         `json:"as_of"`
	Coverage       rpc.AlertCoverage                 `json:"coverage"`
	SourceStates   []alertEpisodeRegistrySourceState `json:"source_states"`
	Cursors        alertShadowDurableCursorsV3       `json:"input_cursors"`
	Metrics        alertShadowDurableMetrics         `json:"commissioning_metrics"`
	Episodes       []alertEpisodeRegistryRecordV2    `json:"episodes"`
}

// alertEpisodeRegistryDocumentV3 is the document shape immediately before the
// portfolio-stress rename. It differs from the current shape in exactly two
// persisted values — the "canary" input-cursor key and the
// canary_portfolio_stress presentation code — so the record layout is shared
// and only the values are rewritten on upgrade.
type alertEpisodeRegistryDocumentV3 struct {
	Version                int                                   `json:"version"`
	UpdatedAt              time.Time                             `json:"updated_at"`
	NextOccurrenceSequence uint64                                `json:"next_occurrence_sequence"`
	Scopes                 []alertEpisodeRegistryScopeDocumentV3 `json:"scopes"`
	LegacyUnscoped         *alertEpisodeLegacyUnscoped           `json:"legacy_unscoped,omitempty"`
}

type alertEpisodeRegistryScopeDocumentV3 struct {
	AuthorityScope string                            `json:"authority_scope"`
	AsOf           time.Time                         `json:"as_of"`
	Coverage       rpc.AlertCoverage                 `json:"coverage"`
	SourceStates   []alertEpisodeRegistrySourceState `json:"source_states"`
	Cursors        alertShadowDurableCursorsV3       `json:"input_cursors"`
	Metrics        alertShadowDurableMetrics         `json:"commissioning_metrics"`
	Episodes       []alertEpisodeRegistryRecord      `json:"episodes"`
}

// legacyStressPresentationCode is the pre-rename presentation code carried by
// stored v3 episodes. validAlertPresentationCode no longer accepts it, so an
// unupgraded document would fail validation and take the whole registry — every
// open episode and occurrence sequence with it — out of service.
const legacyStressPresentationCode rpc.AlertPresentationCode = "canary_portfolio_stress"

type alertEpisodeRegistryRecordV2 struct {
	AuthorityScope             string                  `json:"authority_scope"`
	EpisodeKey                 string                  `json:"episode_key"`
	OccurrenceKey              string                  `json:"occurrence_key"`
	Source                     rpc.AlertSource         `json:"source"`
	Kind                       rpc.AlertKind           `json:"kind"`
	State                      rpc.AlertEpisodeState   `json:"state"`
	Severity                   rpc.AlertSeverity       `json:"severity"`
	DeliveryPreference         string                  `json:"delivery_preference"`
	EvidenceFingerprint        string                  `json:"evidence_fingerprint"`
	EvidenceHealth             rpc.AlertEvidenceHealth `json:"evidence_health"`
	Destination                rpc.AlertDestination    `json:"destination"`
	EvidenceAsOf               time.Time               `json:"evidence_as_of"`
	StateChangedAt             time.Time               `json:"state_changed_at"`
	ObservedAt                 time.Time               `json:"observed_at"`
	PolicyFingerprint          string                  `json:"policy_fingerprint"`
	ProducerDecisionReason     string                  `json:"producer_decision_reason"`
	RegistryDecisionReason     string                  `json:"registry_decision_reason"`
	LastSourceObservedAt       time.Time               `json:"last_source_observed_at"`
	LastObservationFingerprint string                  `json:"last_observation_fingerprint"`
	LastEscalationFingerprint  string                  `json:"last_escalation_fingerprint,omitempty"`
	EmitRecovered              bool                    `json:"emit_recovered"`
}

type alertEpisodeRegistryScopeDocument struct {
	AuthorityScope string                            `json:"authority_scope"`
	AsOf           time.Time                         `json:"as_of"`
	Coverage       rpc.AlertCoverage                 `json:"coverage"`
	SourceStates   []alertEpisodeRegistrySourceState `json:"source_states"`
	Cursors        alertShadowDurableCursors         `json:"input_cursors"`
	Metrics        alertShadowDurableMetrics         `json:"commissioning_metrics"`
	Episodes       []alertEpisodeRegistryRecord      `json:"episodes"`
}

type alertEpisodeRegistrySourceState struct {
	Source         rpc.AlertSource         `json:"source"`
	Status         string                  `json:"status"`
	Reason         string                  `json:"reason"`
	InputAsOf      time.Time               `json:"input_as_of"`
	ObservedAt     time.Time               `json:"observed_at"`
	EvidenceAsOf   time.Time               `json:"evidence_as_of"`
	EvidenceHealth rpc.AlertEvidenceHealth `json:"evidence_health"`
	Covered        bool                    `json:"covered"`
	FreshUntil     time.Time               `json:"fresh_until"`
	// DuplicateCandidates is an evaluation-local commissioning delta. It is
	// folded atomically into Metrics by Apply and never persisted as source
	// authority.
	DuplicateCandidates uint64 `json:"-"`
}

type alertShadowDurableCursors struct {
	Stress         alertShadowInputCursor `json:"stress"`
	Nudges         alertShadowInputCursor `json:"nudges"`
	Regime         alertShadowInputCursor `json:"regime,omitzero"`
	Rulebook       alertShadowInputCursor `json:"rulebook,omitzero"`
	Protection     alertShadowInputCursor `json:"protection,omitzero"`
	OrderIntegrity alertShadowInputCursor `json:"order_integrity,omitzero"`
	DataHealth     alertShadowInputCursor `json:"data_health,omitzero"`
}

// alertShadowDurableCursorsV3 is the pre-rename cursor block: the
// portfolio-stress producer cursor was stored under the key "canary". Document
// versions 2 and 3 both carry it, so both legacy decode paths use this shape
// and hand it to upgradeAlertShadowDurableCursors. Decoding is strict
// (DisallowUnknownFields), so a v3 document would be rejected outright by the
// current shape — the version bump is what routes it here instead.
type alertShadowDurableCursorsV3 struct {
	Canary         alertShadowInputCursor `json:"canary"`
	Nudges         alertShadowInputCursor `json:"nudges"`
	Regime         alertShadowInputCursor `json:"regime,omitzero"`
	Rulebook       alertShadowInputCursor `json:"rulebook,omitzero"`
	Protection     alertShadowInputCursor `json:"protection,omitzero"`
	OrderIntegrity alertShadowInputCursor `json:"order_integrity,omitzero"`
	DataHealth     alertShadowInputCursor `json:"data_health,omitzero"`
}

// upgradeAlertShadowDurableCursors carries every stored cursor across the key
// rename. The portfolio-stress cursor keeps its exact as_of and fingerprint, so
// an upgraded registry does not re-observe an input it has already consumed.
func upgradeAlertShadowDurableCursors(old alertShadowDurableCursorsV3) alertShadowDurableCursors {
	return alertShadowDurableCursors{
		Stress: old.Canary, Nudges: old.Nudges, Regime: old.Regime, Rulebook: old.Rulebook,
		Protection: old.Protection, OrderIntegrity: old.OrderIntegrity, DataHealth: old.DataHealth,
	}
}

type alertShadowDurableMetrics struct {
	AsOf                  time.Time                              `json:"as_of,omitzero"`
	Evaluations           uint64                                 `json:"evaluations"`
	RegistryApplyFailures uint64                                 `json:"registry_apply_failures"`
	Equivocations         uint64                                 `json:"equivocations"`
	LastErrorCode         string                                 `json:"last_error_code,omitempty"`
	Sources               []alertShadowDurableSourceMeasurements `json:"sources"`
}

type alertShadowDurableSourceMeasurements struct {
	Source       rpc.AlertSource          `json:"source"`
	Measurements alertShadowSourceMetrics `json:"measurements"`
}

type alertEpisodeRegistryRecord struct {
	AuthorityScope             string                    `json:"authority_scope"`
	EpisodeKey                 string                    `json:"episode_key"`
	OccurrenceKey              string                    `json:"occurrence_key"`
	Source                     rpc.AlertSource           `json:"source"`
	Kind                       rpc.AlertKind             `json:"kind"`
	PresentationCode           rpc.AlertPresentationCode `json:"presentation_code"`
	State                      rpc.AlertEpisodeState     `json:"state"`
	Severity                   rpc.AlertSeverity         `json:"severity"`
	EvidenceFingerprint        string                    `json:"evidence_fingerprint"`
	EvidenceHealth             rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination                rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf               time.Time                 `json:"evidence_as_of"`
	StateChangedAt             time.Time                 `json:"state_changed_at"`
	ObservedAt                 time.Time                 `json:"observed_at"`
	PolicyFingerprint          string                    `json:"policy_fingerprint"`
	ProducerDecisionReason     string                    `json:"producer_decision_reason"`
	RegistryDecisionReason     string                    `json:"registry_decision_reason"`
	LastSourceObservedAt       time.Time                 `json:"last_source_observed_at"`
	LastObservationFingerprint string                    `json:"last_observation_fingerprint"`
	LastEscalationFingerprint  string                    `json:"last_escalation_fingerprint,omitempty"`
	EmitRecovered              bool                      `json:"emit_recovered"`
}

type alertEpisodeDecisionEvent struct {
	Version        int                    `json:"version"`
	AuthorityScope string                 `json:"authority_scope"`
	AsOf           time.Time              `json:"as_of"`
	Coverage       rpc.AlertCoverage      `json:"coverage"`
	Decisions      []alertEpisodeDecision `json:"decisions"`
}

type alertEpisodeDecision struct {
	EpisodeKey             string                    `json:"episode_key"`
	OccurrenceKey          string                    `json:"occurrence_key,omitempty"`
	Source                 rpc.AlertSource           `json:"source"`
	Kind                   rpc.AlertKind             `json:"kind"`
	PresentationCode       rpc.AlertPresentationCode `json:"presentation_code"`
	Action                 string                    `json:"action"`
	BeforeState            rpc.AlertEpisodeState     `json:"before_state,omitempty"`
	AfterState             rpc.AlertEpisodeState     `json:"after_state,omitempty"`
	Severity               rpc.AlertSeverity         `json:"severity"`
	EvidenceFingerprint    string                    `json:"evidence_fingerprint"`
	EvidenceHealth         rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination            rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf           time.Time                 `json:"evidence_as_of"`
	ObservedAt             time.Time                 `json:"observed_at"`
	PolicyFingerprint      string                    `json:"policy_fingerprint"`
	ProducerDecisionReason string                    `json:"producer_decision_reason"`
	RegistryDecisionReason string                    `json:"registry_decision_reason"`
}

const (
	alertDecisionOpened                      = "opened"
	alertDecisionReopened                    = "reopened"
	alertDecisionEscalated                   = "escalated"
	alertDecisionRefreshedActive             = "refreshed_active"
	alertDecisionRecovered                   = "recovered"
	alertDecisionConfirmedRecovered          = "confirmed_recovered"
	alertDecisionNegativeWithoutEpisode      = "negative_without_episode"
	alertDecisionHeldOmitted                 = "held_omitted"
	alertDecisionHeldPartial                 = "held_partial"
	alertDecisionHeldStale                   = "held_stale"
	alertDecisionHeldUnavailable             = "held_unavailable"
	alertDecisionHeldUntrustedEvidence       = "held_untrusted_evidence"
	alertDecisionReasonPositive              = "producer_positive"
	alertDecisionReasonAuthoritativeNegative = "authoritative_negative"
	alertDecisionReasonSourceOmitted         = "source_omitted"
	alertDecisionReasonLegacyRetired         = "legacy_presentation_retired"
)

func newAlertEpisodeRegistry(ctx context.Context, core *corestore.Store) (*alertEpisodeRegistry, error) {
	return newAlertEpisodeRegistryWithInactiveLimit(ctx, core, alertEpisodeRegistryInactiveLimit)
}

func newAlertEpisodeRegistryWithInactiveLimit(ctx context.Context, core *corestore.Store, inactiveLimit int) (*alertEpisodeRegistry, error) {
	if core == nil {
		return nil, errors.New("alert episode registry requires SQLite authority")
	}
	if inactiveLimit < 0 {
		return nil, errors.New("alert episode registry inactive limit must not be negative")
	}
	r := &alertEpisodeRegistry{core: core, inactiveLimit: inactiveLimit}
	if err := r.reload(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Apply evaluates one already-classified producer batch and atomically persists
// the current registry. The registry document contains the operational
// lifecycle and commissioning counters; beta transition events were an
// unbounded duplicate with no operational reader.
func (r *alertEpisodeRegistry) Apply(ctx context.Context, evaluation alertEpisodeEvaluation) (rpc.AlertCandidateSnapshot, error) {
	if r == nil || r.core == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert episode registry is unavailable")
	}
	if err := validateAlertEpisodeEvaluation(evaluation); err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for range alertEpisodeRegistryCASAttempts {
		next, _, err := applyAlertEpisodeEvaluation(r.document, evaluation, r.inactiveLimit)
		if err != nil {
			return rpc.AlertCandidateSnapshot{}, err
		}
		documentJSON, err := json.Marshal(next)
		if err != nil {
			return rpc.AlertCandidateSnapshot{}, fmt.Errorf("encode alert episode registry: %w", err)
		}
		update := corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
			ExpectedRevision: r.revision, JSON: documentJSON,
		}
		saved, err := r.core.CompareAndSwapStateDocument(ctx, update)
		if errors.Is(err, corestore.ErrRevisionConflict) {
			if reloadErr := r.reload(ctx); reloadErr != nil {
				return rpc.AlertCandidateSnapshot{}, reloadErr
			}
			continue
		}
		if err != nil {
			return rpc.AlertCandidateSnapshot{}, fmt.Errorf("persist alert episode registry: %w", err)
		}
		r.revision = saved.Revision
		r.document = next
		scopeDocument, ok := findAlertEpisodeScope(next.Scopes, evaluation.AuthorityScope)
		if !ok {
			return rpc.AlertCandidateSnapshot{}, errors.New("persisted alert episode scope is missing")
		}
		return alertEpisodeSnapshot(scopeDocument, evaluation.AsOf)
	}
	return rpc.AlertCandidateSnapshot{}, errors.New("persist alert episode registry: repeated revision conflict")
}

// RecordInputDisposition durably accounts for an input that correctly did not
// enter lifecycle evaluation. It mutates only redacted commissioning counters;
// producer cursors and candidate state remain unchanged.
func (r *alertEpisodeRegistry) RecordInputDisposition(ctx context.Context, authorityScope string, at time.Time, sources []rpc.AlertSource, disposition string) error {
	if r == nil || r.core == nil {
		return errors.New("alert episode registry is unavailable")
	}
	if err := rpc.ValidateAlertAuthorityScope(authorityScope); err != nil {
		return err
	}
	if at.IsZero() {
		return errors.New("alert registry input disposition requires as_of")
	}
	if disposition != alertShadowDispositionDuplicate && disposition != alertShadowDispositionStale && disposition != alertShadowDispositionEquivocation {
		return errors.New("invalid alert registry input disposition")
	}
	seen := make(map[rpc.AlertSource]struct{}, len(sources))
	for _, source := range sources {
		if _, ok := seen[source]; ok {
			return errors.New("duplicate alert registry disposition source")
		}
		seen[source] = struct{}{}
		if !alertCoverageContains(alertShadowExpectedSourceSlice(), source) {
			return errors.New("invalid alert registry disposition source")
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for range alertEpisodeRegistryCASAttempts {
		next := cloneAlertEpisodeRegistryDocument(r.document)
		scopeIndex := -1
		for i := range next.Scopes {
			if next.Scopes[i].AuthorityScope == authorityScope {
				scopeIndex = i
				break
			}
		}
		if scopeIndex < 0 {
			next.Scopes = append(next.Scopes, alertEpisodeRegistryScopeDocument{
				AuthorityScope: authorityScope, AsOf: at.UTC(),
				Coverage: rpc.AlertCoverage{
					State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: at.UTC(),
					ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: []rpc.AlertSource{},
				},
				SourceStates: []alertEpisodeRegistrySourceState{},
				Metrics: alertShadowDurableMetrics{
					AsOf: at.UTC(), Sources: newAlertShadowDurableSourceMeasurements(),
				},
				Episodes: []alertEpisodeRegistryRecord{},
			})
			scopeIndex = len(next.Scopes) - 1
		}
		if next.UpdatedAt.IsZero() || at.After(next.UpdatedAt) {
			next.UpdatedAt = at.UTC()
		}
		metrics := &next.Scopes[scopeIndex].Metrics
		if metrics.AsOf.IsZero() || at.After(metrics.AsOf) {
			metrics.AsOf = at.UTC()
		}
		if disposition == alertShadowDispositionEquivocation {
			metrics.Equivocations++
		}
		for source := range seen {
			measurement := alertShadowDurableSourceMetric(metrics, source)
			if measurement == nil {
				return errors.New("alert registry disposition source metric is missing")
			}
			switch disposition {
			case alertShadowDispositionDuplicate:
				measurement.DuplicateInputs++
			case alertShadowDispositionStale:
				measurement.StaleSuppressions++
			case alertShadowDispositionEquivocation:
				measurement.Equivocations++
			}
		}
		sort.Slice(next.Scopes, func(i, j int) bool { return next.Scopes[i].AuthorityScope < next.Scopes[j].AuthorityScope })
		if err := validateAlertEpisodeRegistryDocument(next, r.inactiveLimit); err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode alert episode registry disposition: %w", err)
		}
		saved, err := r.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
			ExpectedRevision: r.revision, JSON: raw,
		})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			if reloadErr := r.reload(ctx); reloadErr != nil {
				return reloadErr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("persist alert registry input disposition: %w", err)
		}
		r.revision = saved.Revision
		r.document = next
		return nil
	}
	return errors.New("persist alert registry input disposition: repeated revision conflict")
}

// RecordApplyFailure durably accounts for a lifecycle evaluation that could
// not be committed. It intentionally advances neither producer cursors nor
// candidate/source authority. A later successful retry remains the only path
// that may mutate lifecycle; this document change is commissioning evidence
// only.
func (r *alertEpisodeRegistry) RecordApplyFailure(ctx context.Context, authorityScope string, at time.Time) error {
	if r == nil || r.core == nil {
		return errors.New("alert episode registry is unavailable")
	}
	if err := rpc.ValidateAlertAuthorityScope(authorityScope); err != nil {
		return err
	}
	if at.IsZero() {
		return errors.New("alert registry apply failure requires as_of")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for range alertEpisodeRegistryCASAttempts {
		next := cloneAlertEpisodeRegistryDocument(r.document)
		scopeIndex := -1
		for i := range next.Scopes {
			if next.Scopes[i].AuthorityScope == authorityScope {
				scopeIndex = i
				break
			}
		}
		if scopeIndex < 0 {
			next.Scopes = append(next.Scopes, alertEpisodeRegistryScopeDocument{
				AuthorityScope: authorityScope,
				AsOf:           at.UTC(),
				Coverage: rpc.AlertCoverage{
					State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: at.UTC(),
					ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: []rpc.AlertSource{},
				},
				SourceStates: []alertEpisodeRegistrySourceState{},
				Metrics: alertShadowDurableMetrics{
					AsOf: at.UTC(), Sources: newAlertShadowDurableSourceMeasurements(),
				},
				Episodes: []alertEpisodeRegistryRecord{},
			})
			scopeIndex = len(next.Scopes) - 1
		}
		if next.UpdatedAt.IsZero() || at.After(next.UpdatedAt) {
			next.UpdatedAt = at.UTC()
		}
		metrics := &next.Scopes[scopeIndex].Metrics
		if metrics.AsOf.IsZero() || at.After(metrics.AsOf) {
			metrics.AsOf = at.UTC()
		}
		metrics.RegistryApplyFailures++
		metrics.LastErrorCode = alertShadowReasonRegistryApplyFailed
		sort.Slice(next.Scopes, func(i, j int) bool { return next.Scopes[i].AuthorityScope < next.Scopes[j].AuthorityScope })
		if err := validateAlertEpisodeRegistryDocument(next, r.inactiveLimit); err != nil {
			return err
		}
		raw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("encode alert registry apply failure: %w", err)
		}
		saved, err := r.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
			ExpectedRevision: r.revision, JSON: raw,
		})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			if reloadErr := r.reload(ctx); reloadErr != nil {
				return reloadErr
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("persist alert registry apply failure: %w", err)
		}
		r.revision = saved.Revision
		r.document = next
		return nil
	}
	return errors.New("persist alert registry apply failure: repeated revision conflict")
}

// Snapshot returns the durable current producer view. The bool is false until
// the first evaluation has been committed; callers must represent that state
// as unavailable instead of manufacturing a clear snapshot.
func (r *alertEpisodeRegistry) Snapshot(authorityScope string, now time.Time) (rpc.AlertCandidateSnapshot, bool, error) {
	if r == nil {
		return rpc.AlertCandidateSnapshot{}, false, errors.New("alert episode registry is unavailable")
	}
	if err := rpc.ValidateAlertAuthorityScope(authorityScope); err != nil {
		return rpc.AlertCandidateSnapshot{}, false, err
	}
	if now.IsZero() {
		return rpc.AlertCandidateSnapshot{}, false, errors.New("alert episode registry snapshot requires now")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.revision == 0 {
		return rpc.AlertCandidateSnapshot{}, false, nil
	}
	scopeDocument, ok := findAlertEpisodeScope(r.document.Scopes, authorityScope)
	if !ok {
		return rpc.AlertCandidateSnapshot{}, false, nil
	}
	snapshot, err := alertEpisodeSnapshot(scopeDocument, now.UTC())
	return snapshot, true, err
}

// scopeState returns a private clone used to hydrate one in-memory producer
// context after restart. It contains only opaque scope identity and redacted
// alert state; raw broker account/mode values are never stored here.
func (r *alertEpisodeRegistry) scopeState(authorityScope string) (alertEpisodeRegistryScopeDocument, bool, error) {
	if r == nil {
		return alertEpisodeRegistryScopeDocument{}, false, errors.New("alert episode registry is unavailable")
	}
	if err := rpc.ValidateAlertAuthorityScope(authorityScope); err != nil {
		return alertEpisodeRegistryScopeDocument{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	scopeDocument, ok := findAlertEpisodeScope(r.document.Scopes, authorityScope)
	if !ok {
		return alertEpisodeRegistryScopeDocument{}, false, nil
	}
	return cloneAlertEpisodeScopeDocument(scopeDocument), true, nil
}

func (r *alertEpisodeRegistry) reload(ctx context.Context) error {
	doc, ok, err := r.core.GetStateDocument(ctx, daemonStateScope, alertEpisodeRegistryStateKind)
	if err != nil {
		return fmt.Errorf("load alert episode registry: %w", err)
	}
	if !ok {
		r.revision = 0
		r.document = alertEpisodeRegistryDocument{Version: alertEpisodeRegistryDocumentVersion, Scopes: []alertEpisodeRegistryScopeDocument{}}
		return nil
	}
	var version struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(doc.JSON, &version); err != nil {
		return fmt.Errorf("load alert episode registry revision %d: %w", doc.Revision, err)
	}
	if version.Version == 1 {
		return r.migrateV1(ctx, doc)
	}
	if version.Version == 2 {
		return r.migrateV2(ctx, doc)
	}
	if version.Version == 3 {
		return r.migrateV3(ctx, doc)
	}
	decoded, err := decodeAlertEpisodeRegistryDocument(doc.JSON)
	if err != nil {
		return fmt.Errorf("load alert episode registry revision %d: %w", doc.Revision, err)
	}
	if err := validateAlertEpisodeRegistryDocument(decoded, r.inactiveLimit); err != nil {
		return fmt.Errorf("load alert episode registry revision %d: %w", doc.Revision, err)
	}
	r.revision = doc.Revision
	r.document = decoded
	return nil
}

// migrateV3 carries a stored pre-rename registry across the portfolio-stress
// rename. Nothing is discarded and nothing is re-derived: every episode keeps
// its episode key, occurrence key, state, severity, timestamps, and
// fingerprints, the occurrence sequence is preserved so no in-flight occurrence
// can be minted twice, and the producer cursor keeps its as_of/fingerprint so
// the next evaluation does not re-observe an already-consumed input. Only the
// two renamed values change: the "canary" cursor key becomes "stress" and the
// canary_portfolio_stress presentation code becomes portfolio_stress.
func (r *alertEpisodeRegistry) migrateV3(ctx context.Context, stored corestore.StateDocument) error {
	legacy, err := decodeAlertEpisodeRegistryDocumentV3(stored.JSON)
	if err != nil {
		return fmt.Errorf("load alert episode registry revision %d legacy v3: %w", stored.Revision, err)
	}
	next := alertEpisodeRegistryDocument{
		Version: alertEpisodeRegistryDocumentVersion, UpdatedAt: legacy.UpdatedAt,
		NextOccurrenceSequence: legacy.NextOccurrenceSequence,
		Scopes:                 make([]alertEpisodeRegistryScopeDocument, 0, len(legacy.Scopes)),
		LegacyUnscoped:         legacy.LegacyUnscoped,
	}
	for _, oldScope := range legacy.Scopes {
		scope := alertEpisodeRegistryScopeDocument{
			AuthorityScope: oldScope.AuthorityScope, AsOf: oldScope.AsOf, Coverage: oldScope.Coverage,
			SourceStates: oldScope.SourceStates, Cursors: upgradeAlertShadowDurableCursors(oldScope.Cursors),
			Metrics: oldScope.Metrics, Episodes: oldScope.Episodes,
		}
		for i := range scope.Episodes {
			if scope.Episodes[i].PresentationCode == legacyStressPresentationCode {
				scope.Episodes[i].PresentationCode = rpc.AlertPresentationPortfolioStress
			}
		}
		next.Scopes = append(next.Scopes, scope)
	}
	if err := validateAlertEpisodeRegistryDocument(next, r.inactiveLimit); err != nil {
		return fmt.Errorf("migrate alert episode registry v3: %w", err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode migrated alert episode registry v3: %w", err)
	}
	saved, err := r.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		ExpectedRevision: stored.Revision, JSON: raw,
	})
	if err != nil {
		return fmt.Errorf("persist migrated alert episode registry v3: %w", err)
	}
	r.revision, r.document = saved.Revision, next
	return nil
}

func (r *alertEpisodeRegistry) migrateV2(ctx context.Context, stored corestore.StateDocument) error {
	legacy, err := decodeAlertEpisodeRegistryDocumentV2(stored.JSON)
	if err != nil {
		return fmt.Errorf("load alert episode registry revision %d legacy v2: %w", stored.Revision, err)
	}
	next := alertEpisodeRegistryDocument{
		Version: alertEpisodeRegistryDocumentVersion, UpdatedAt: legacy.UpdatedAt,
		NextOccurrenceSequence: legacy.NextOccurrenceSequence,
		Scopes:                 make([]alertEpisodeRegistryScopeDocument, 0, len(legacy.Scopes)),
		LegacyUnscoped:         legacy.LegacyUnscoped,
	}
	for _, oldScope := range legacy.Scopes {
		scope := alertEpisodeRegistryScopeDocument{
			AuthorityScope: oldScope.AuthorityScope, AsOf: oldScope.AsOf, Coverage: oldScope.Coverage,
			SourceStates: oldScope.SourceStates, Cursors: upgradeAlertShadowDurableCursors(oldScope.Cursors),
			Metrics:  oldScope.Metrics,
			Episodes: make([]alertEpisodeRegistryRecord, 0, len(oldScope.Episodes)),
		}
		for i, old := range oldScope.Episodes {
			if !validLegacyAlertDeliveryPreference(old.DeliveryPreference) {
				return fmt.Errorf("migrate alert episode registry v2: invalid delivery preference at record %d", i)
			}
			scope.Episodes = append(scope.Episodes, alertEpisodeRegistryRecord{
				AuthorityScope: old.AuthorityScope, EpisodeKey: old.EpisodeKey, OccurrenceKey: old.OccurrenceKey,
				Source: old.Source, Kind: old.Kind, PresentationCode: legacyAlertPresentationCode(old.Source),
				State: old.State, Severity: old.Severity, EvidenceFingerprint: old.EvidenceFingerprint,
				EvidenceHealth: old.EvidenceHealth, Destination: old.Destination, EvidenceAsOf: old.EvidenceAsOf,
				StateChangedAt: old.StateChangedAt, ObservedAt: old.ObservedAt, PolicyFingerprint: old.PolicyFingerprint,
				ProducerDecisionReason: old.ProducerDecisionReason, RegistryDecisionReason: old.RegistryDecisionReason,
				LastSourceObservedAt: old.LastSourceObservedAt, LastObservationFingerprint: old.LastObservationFingerprint,
				LastEscalationFingerprint: old.LastEscalationFingerprint, EmitRecovered: old.EmitRecovered,
			})
		}
		next.Scopes = append(next.Scopes, scope)
	}
	if err := validateAlertEpisodeRegistryDocument(next, r.inactiveLimit); err != nil {
		return fmt.Errorf("migrate alert episode registry v2: %w", err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode migrated alert episode registry v2: %w", err)
	}
	saved, err := r.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		ExpectedRevision: stored.Revision, JSON: raw,
	})
	if err != nil {
		return fmt.Errorf("persist migrated alert episode registry v2: %w", err)
	}
	r.revision, r.document = saved.Revision, next
	return nil
}

func (r *alertEpisodeRegistry) migrateV1(ctx context.Context, stored corestore.StateDocument) error {
	legacy, err := decodeAlertEpisodeRegistryDocumentV1(stored.JSON)
	if err != nil {
		return fmt.Errorf("load alert episode registry revision %d legacy v1: %w", stored.Revision, err)
	}
	if err := validateAlertEpisodeRegistryDocumentV1(legacy, r.inactiveLimit); err != nil {
		return fmt.Errorf("load alert episode registry revision %d legacy v1: %w", stored.Revision, err)
	}
	migratedAt := time.Now().UTC()
	if migratedAt.Before(legacy.AsOf) {
		migratedAt = legacy.AsOf.UTC()
	}
	digest := sha256.Sum256(stored.JSON)
	next := alertEpisodeRegistryDocument{
		Version: alertEpisodeRegistryDocumentVersion, UpdatedAt: migratedAt,
		NextOccurrenceSequence: legacy.NextOccurrenceSequence,
		Scopes:                 []alertEpisodeRegistryScopeDocument{},
		LegacyUnscoped: &alertEpisodeLegacyUnscoped{
			MigratedAt: migratedAt, Fingerprint: "sha256:" + hex.EncodeToString(digest[:]),
			Document: append(json.RawMessage(nil), stored.JSON...),
		},
	}
	if err := validateAlertEpisodeRegistryDocument(next, r.inactiveLimit); err != nil {
		return fmt.Errorf("migrate alert episode registry v1: %w", err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode migrated alert episode registry v1: %w", err)
	}
	saved, err := r.core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: alertEpisodeRegistryStateKind,
		ExpectedRevision: stored.Revision, JSON: raw,
	})
	if err != nil {
		return fmt.Errorf("persist migrated alert episode registry v1: %w", err)
	}
	r.revision = saved.Revision
	r.document = next
	return nil
}

func applyAlertEpisodeEvaluation(base alertEpisodeRegistryDocument, evaluation alertEpisodeEvaluation, inactiveLimit int) (alertEpisodeRegistryDocument, []alertEpisodeDecision, error) {
	next := cloneAlertEpisodeRegistryDocument(base)
	next.Version = alertEpisodeRegistryDocumentVersion
	if next.UpdatedAt.IsZero() || evaluation.AsOf.After(next.UpdatedAt) {
		next.UpdatedAt = evaluation.AsOf.UTC()
	}

	scopeIndex := -1
	for i := range next.Scopes {
		if next.Scopes[i].AuthorityScope == evaluation.AuthorityScope {
			scopeIndex = i
			break
		}
	}
	if scopeIndex < 0 {
		next.Scopes = append(next.Scopes, alertEpisodeRegistryScopeDocument{
			AuthorityScope: evaluation.AuthorityScope,
			SourceStates:   []alertEpisodeRegistrySourceState{},
			Metrics: alertShadowDurableMetrics{
				Sources: newAlertShadowDurableSourceMeasurements(),
			},
			Episodes: []alertEpisodeRegistryRecord{},
		})
		scopeIndex = len(next.Scopes) - 1
	}
	scopeDocument := &next.Scopes[scopeIndex]
	if !scopeDocument.AsOf.IsZero() && evaluation.AsOf.Before(scopeDocument.AsOf) {
		return alertEpisodeRegistryDocument{}, nil, errors.New("alert episode evaluation moves scoped as_of backwards")
	}
	beforeScope := cloneAlertEpisodeScopeDocument(*scopeDocument)
	scopeDocument.AsOf = evaluation.AsOf.UTC()
	scopeDocument.Coverage = cloneAlertCoverage(evaluation.Coverage)
	mergeAlertEpisodeSourceStates(scopeDocument, evaluation.SourceStates)
	for i := range scopeDocument.Episodes {
		if scopeDocument.Episodes[i].State == rpc.AlertEpisodeRecovered {
			scopeDocument.Episodes[i].EmitRecovered = false
		}
	}

	observations := append([]alertEpisodeObservation(nil), evaluation.Observations...)
	sort.Slice(observations, func(i, j int) bool { return observations[i].EpisodeKey < observations[j].EpisodeKey })
	byEpisode := make(map[string]int, len(scopeDocument.Episodes))
	for i := range scopeDocument.Episodes {
		byEpisode[scopeDocument.Episodes[i].EpisodeKey] = i
	}
	seen := make(map[string]struct{}, len(observations))
	decisions := make([]alertEpisodeDecision, 0, len(observations)+len(scopeDocument.Episodes))

	for _, observation := range observations {
		if _, duplicate := seen[observation.EpisodeKey]; duplicate {
			return alertEpisodeRegistryDocument{}, nil, fmt.Errorf("alert episode observation equivocation for %s", observation.EpisodeKey)
		}
		seen[observation.EpisodeKey] = struct{}{}
		observationFingerprint, err := alertEpisodeObservationFingerprint(observation)
		if err != nil {
			return alertEpisodeRegistryDocument{}, nil, err
		}
		idx, exists := byEpisode[observation.EpisodeKey]
		if exists {
			record := &scopeDocument.Episodes[idx]
			if record.Source != observation.Source || record.Kind != observation.Kind {
				return alertEpisodeRegistryDocument{}, nil, fmt.Errorf("alert episode identity equivocation for %s", observation.EpisodeKey)
			}
			if observation.ObservedAt.Before(record.LastSourceObservedAt) {
				return alertEpisodeRegistryDocument{}, nil, fmt.Errorf("alert episode observation is out of order for %s", observation.EpisodeKey)
			}
			if observation.ObservedAt.Equal(record.LastSourceObservedAt) && observationFingerprint != record.LastObservationFingerprint {
				return alertEpisodeRegistryDocument{}, nil, fmt.Errorf("alert episode observation timestamp equivocation for %s", observation.EpisodeKey)
			}
		}

		if observation.Active {
			if !exists {
				occurrence, err := allocateAlertOccurrence(&next, observation.EpisodeKey)
				if err != nil {
					return alertEpisodeRegistryDocument{}, nil, err
				}
				state := rpc.AlertEpisodeOpen
				if observation.EscalationFingerprint != "" {
					state = rpc.AlertEpisodeEscalated
				}
				record := recordFromAlertObservation(evaluation.AuthorityScope, observation, occurrence, state, observation.ObservedAt, observationFingerprint)
				record.RegistryDecisionReason = alertDecisionReasonPositive
				scopeDocument.Episodes = append(scopeDocument.Episodes, record)
				byEpisode[record.EpisodeKey] = len(scopeDocument.Episodes) - 1
				decisions = append(decisions, decisionFromAlertRecord(record, "", alertDecisionOpened))
				continue
			}

			record := &scopeDocument.Episodes[idx]
			before := record.State
			action := alertDecisionRefreshedActive
			if record.State == rpc.AlertEpisodeRecovered {
				occurrence, err := allocateAlertOccurrence(&next, observation.EpisodeKey)
				if err != nil {
					return alertEpisodeRegistryDocument{}, nil, err
				}
				record.OccurrenceKey = occurrence
				record.State = rpc.AlertEpisodeOpen
				if observation.EscalationFingerprint != "" {
					record.State = rpc.AlertEpisodeEscalated
				}
				record.StateChangedAt = observation.ObservedAt
				record.LastEscalationFingerprint = observation.EscalationFingerprint
				action = alertDecisionReopened
			} else if observation.EscalationFingerprint != "" && observation.EscalationFingerprint != record.LastEscalationFingerprint {
				occurrence, err := allocateAlertOccurrence(&next, observation.EpisodeKey)
				if err != nil {
					return alertEpisodeRegistryDocument{}, nil, err
				}
				record.OccurrenceKey = occurrence
				record.State = rpc.AlertEpisodeEscalated
				record.StateChangedAt = observation.ObservedAt
				record.LastEscalationFingerprint = observation.EscalationFingerprint
				action = alertDecisionEscalated
			}
			applyAlertObservationToRecord(record, observation, observationFingerprint)
			record.RegistryDecisionReason = alertDecisionReasonPositive
			decisions = append(decisions, decisionFromAlertRecord(*record, before, action))
			continue
		}

		if !exists {
			decisions = append(decisions, decisionFromAlertObservation(observation, alertDecisionNegativeWithoutEpisode, ""))
			continue
		}
		record := &scopeDocument.Episodes[idx]
		before := record.State
		if record.State == rpc.AlertEpisodeRecovered {
			if alertObservationCanRecover(evaluation.Coverage, observation) {
				applyAlertObservationToRecord(record, observation, observationFingerprint)
				record.EvidenceHealth = rpc.AlertEvidenceCurrent
				record.RegistryDecisionReason = alertDecisionReasonAuthoritativeNegative
				decisions = append(decisions, decisionFromAlertRecord(*record, before, alertDecisionConfirmedRecovered))
				continue
			}
			// A degraded negative cannot revise the already-authoritative recovery
			// candidate into an invalid stale/partial recovered candidate. Retain
			// that lifecycle fact while journaling this source observation.
			record.LastSourceObservedAt = observation.ObservedAt
			record.LastObservationFingerprint = observationFingerprint
			record.PolicyFingerprint = observation.PolicyFingerprint
			record.ProducerDecisionReason = observation.ProducerDecisionReason
			action := alertHoldDecision(evaluation.Coverage, observation)
			record.RegistryDecisionReason = action
			decision := decisionFromAlertObservation(observation, action, record.State)
			decision.OccurrenceKey = record.OccurrenceKey
			decision.BeforeState = before
			decision.RegistryDecisionReason = action
			decisions = append(decisions, decision)
			continue
		}
		applyAlertObservationToRecord(record, observation, observationFingerprint)
		if alertObservationCanRecover(evaluation.Coverage, observation) {
			record.State = rpc.AlertEpisodeRecovered
			record.StateChangedAt = observation.ObservedAt
			record.EvidenceHealth = rpc.AlertEvidenceCurrent
			record.RegistryDecisionReason = alertDecisionReasonAuthoritativeNegative
			record.EmitRecovered = true
			decisions = append(decisions, decisionFromAlertRecord(*record, before, alertDecisionRecovered))
			continue
		}
		action := alertHoldDecision(evaluation.Coverage, observation)
		record.RegistryDecisionReason = action
		decisions = append(decisions, decisionFromAlertRecord(*record, before, action))
	}

	opportunitySources := alertEpisodeOpportunitySet(evaluation)
	for i := range scopeDocument.Episodes {
		record := &scopeDocument.Episodes[i]
		if record.State == rpc.AlertEpisodeRecovered {
			continue
		}
		if _, opportunity := opportunitySources[record.Source]; !opportunity {
			continue
		}
		if _, observed := seen[record.EpisodeKey]; observed {
			continue
		}
		if alertLegacyPresentationCanRetire(evaluation, *record) {
			before := record.State
			record.State = rpc.AlertEpisodeRecovered
			record.StateChangedAt = evaluation.AsOf.UTC()
			record.ObservedAt = evaluation.AsOf.UTC()
			record.LastSourceObservedAt = evaluation.AsOf.UTC()
			record.EvidenceAsOf = evaluation.AsOf.UTC()
			record.EvidenceHealth = rpc.AlertEvidenceCurrent
			record.RegistryDecisionReason = alertDecisionReasonLegacyRetired
			record.EmitRecovered = true
			decisions = append(decisions, decisionFromAlertRecord(*record, before, alertDecisionRecovered))
			continue
		}
		before := record.State
		record.EvidenceHealth = omittedAlertEvidenceHealth(evaluation.Coverage, record.Source)
		record.ObservedAt = evaluation.AsOf
		record.RegistryDecisionReason = alertDecisionReasonSourceOmitted
		decisions = append(decisions, decisionFromAlertRecord(*record, before, alertDecisionHeldOmitted))
	}

	trimInactiveAlertEpisodes(scopeDocument, inactiveLimit)
	sort.Slice(scopeDocument.Episodes, func(i, j int) bool {
		return scopeDocument.Episodes[i].EpisodeKey < scopeDocument.Episodes[j].EpisodeKey
	})
	sort.Slice(decisions, func(i, j int) bool {
		if decisions[i].EpisodeKey == decisions[j].EpisodeKey {
			return decisions[i].Action < decisions[j].Action
		}
		return decisions[i].EpisodeKey < decisions[j].EpisodeKey
	})
	applyAlertShadowEvaluationMetrics(scopeDocument, beforeScope, evaluation, decisions)
	applyAlertShadowCursor(scopeDocument, evaluation.CursorKind, evaluation.Cursor)
	sort.Slice(next.Scopes, func(i, j int) bool { return next.Scopes[i].AuthorityScope < next.Scopes[j].AuthorityScope })
	if err := validateAlertEpisodeRegistryDocument(next, inactiveLimit); err != nil {
		return alertEpisodeRegistryDocument{}, nil, fmt.Errorf("build alert episode registry: %w", err)
	}
	return next, decisions, nil
}

func alertLegacyPresentationCanRetire(evaluation alertEpisodeEvaluation, record alertEpisodeRegistryRecord) bool {
	if record.PresentationCode != rpc.AlertPresentationRulebookLegacyCondition || record.Source != rpc.AlertSourceRulebook {
		return false
	}
	if evaluation.Coverage.Freshness != rpc.AlertCoverageCurrent || !alertCoverageContains(evaluation.Coverage.CoveredSources, record.Source) {
		return false
	}
	// Retirement is a compatibility lifecycle transition, not an inference
	// that an old untyped condition recovered. It is allowed only after the
	// current typed Rulebook producer has supplied at least one observation in
	// the same evaluation; those named episode keys supersede the unscoped v2
	// compatibility lamp.
	for _, observation := range evaluation.Observations {
		if observation.Source == rpc.AlertSourceRulebook && observation.PresentationCode != rpc.AlertPresentationRulebookLegacyCondition {
			return true
		}
	}
	return false
}

func recordFromAlertObservation(authorityScope string, observation alertEpisodeObservation, occurrence string, state rpc.AlertEpisodeState, changedAt time.Time, observationFingerprint string) alertEpisodeRegistryRecord {
	record := alertEpisodeRegistryRecord{
		AuthorityScope: authorityScope, EpisodeKey: observation.EpisodeKey, OccurrenceKey: occurrence, Source: observation.Source,
		Kind: observation.Kind, State: state, StateChangedAt: changedAt,
		LastEscalationFingerprint: observation.EscalationFingerprint,
	}
	applyAlertObservationToRecord(&record, observation, observationFingerprint)
	return record
}

func applyAlertObservationToRecord(record *alertEpisodeRegistryRecord, observation alertEpisodeObservation, observationFingerprint string) {
	record.Severity = observation.Severity
	record.PresentationCode = observation.PresentationCode
	record.EvidenceFingerprint = observation.EvidenceFingerprint
	record.EvidenceHealth = observation.EvidenceHealth
	record.Destination = observation.Destination
	record.EvidenceAsOf = observation.EvidenceAsOf
	record.ObservedAt = observation.ObservedAt
	record.PolicyFingerprint = observation.PolicyFingerprint
	record.ProducerDecisionReason = observation.ProducerDecisionReason
	record.LastSourceObservedAt = observation.ObservedAt
	record.LastObservationFingerprint = observationFingerprint
}

func allocateAlertOccurrence(document *alertEpisodeRegistryDocument, episodeKey string) (string, error) {
	document.NextOccurrenceSequence++
	key, err := rpc.BuildAlertOccurrenceKey(episodeKey, fmt.Sprintf("sequence:%d", document.NextOccurrenceSequence))
	if err != nil {
		return "", fmt.Errorf("allocate alert occurrence: %w", err)
	}
	return key, nil
}

func alertObservationCanRecover(coverage rpc.AlertCoverage, observation alertEpisodeObservation) bool {
	// CoveredSources is the per-source completeness claim. Aggregate coverage
	// may remain partial while one source is current and authoritative; that
	// source may recover only its own explicit negative episode. Aggregate
	// partial coverage still prevents a clear snapshot and omission is handled
	// separately, never as negative evidence.
	if coverage.Freshness != rpc.AlertCoverageCurrent || observation.EvidenceHealth != rpc.AlertEvidenceCurrent {
		return false
	}
	return alertCoverageContains(coverage.CoveredSources, observation.Source)
}

func alertHoldDecision(coverage rpc.AlertCoverage, observation alertEpisodeObservation) string {
	if coverage.State == rpc.AlertCoverageUnavailable {
		return alertDecisionHeldUnavailable
	}
	if coverage.Freshness == rpc.AlertCoverageStale {
		return alertDecisionHeldStale
	}
	if !alertCoverageContains(coverage.CoveredSources, observation.Source) {
		return alertDecisionHeldPartial
	}
	return alertDecisionHeldUntrustedEvidence
}

func omittedAlertEvidenceHealth(coverage rpc.AlertCoverage, source rpc.AlertSource) rpc.AlertEvidenceHealth {
	if coverage.State == rpc.AlertCoverageUnavailable {
		return rpc.AlertEvidenceUnavailable
	}
	if coverage.Freshness == rpc.AlertCoverageStale {
		return rpc.AlertEvidenceStale
	}
	if !alertCoverageContains(coverage.CoveredSources, source) {
		return rpc.AlertEvidencePartial
	}
	return rpc.AlertEvidenceUnavailable
}

func alertCoverageContains(sources []rpc.AlertSource, source rpc.AlertSource) bool {
	return slices.Contains(sources, source)
}

func trimInactiveAlertEpisodes(document *alertEpisodeRegistryScopeDocument, inactiveLimit int) {
	type inactive struct {
		index int
		at    time.Time
		key   string
	}
	var recovered []inactive
	for i, record := range document.Episodes {
		if record.State == rpc.AlertEpisodeRecovered {
			recovered = append(recovered, inactive{index: i, at: record.ObservedAt, key: record.EpisodeKey})
		}
	}
	if len(recovered) <= inactiveLimit {
		return
	}
	sort.Slice(recovered, func(i, j int) bool {
		if recovered[i].at.Equal(recovered[j].at) {
			return recovered[i].key < recovered[j].key
		}
		return recovered[i].at.Before(recovered[j].at)
	})
	drop := make(map[int]struct{}, len(recovered)-inactiveLimit)
	for _, item := range recovered[:len(recovered)-inactiveLimit] {
		drop[item.index] = struct{}{}
	}
	kept := make([]alertEpisodeRegistryRecord, 0, len(document.Episodes)-len(drop))
	for i, record := range document.Episodes {
		if _, remove := drop[i]; !remove {
			kept = append(kept, record)
		}
	}
	document.Episodes = kept
}

func alertEpisodeSnapshot(document alertEpisodeRegistryScopeDocument, now time.Time) (rpc.AlertCandidateSnapshot, error) {
	clockInvalid := now.Before(document.AsOf)
	// Candidate timestamps are immutable audit evidence and must remain at or
	// before the snapshot timestamp. Keep the durable boundary as the wire
	// timestamp during rollback, but strip all current coverage below so the
	// future state can never manufacture a clear or a trustworthy recovery.
	if clockInvalid {
		now = document.AsOf
	}
	now = now.UTC()
	coverage, staleSources := projectAlertEpisodeCoverage(document, now)
	sources := projectAlertSourceCoverage(document, coverage, staleSources, now)
	if clockInvalid {
		coverage = rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: now,
			ExpectedSources: append([]rpc.AlertSource(nil), document.Coverage.ExpectedSources...),
			CoveredSources:  []rpc.AlertSource{},
		}
		staleSources = make(map[rpc.AlertSource]struct{}, len(coverage.ExpectedSources))
		for _, source := range coverage.ExpectedSources {
			staleSources[source] = struct{}{}
		}
		sources = unavailableAlertSourceCoverage(coverage.ExpectedSources)
	}
	candidates := make([]rpc.AlertCandidate, 0, len(document.Episodes))
	for _, record := range document.Episodes {
		if record.State == rpc.AlertEpisodeRecovered && !record.EmitRecovered {
			continue
		}
		candidate := alertCandidateFromRecord(record)
		if _, stale := staleSources[candidate.Source]; stale {
			if candidate.State == rpc.AlertEpisodeRecovered {
				continue
			}
			if candidate.EvidenceHealth != rpc.AlertEvidenceError {
				candidate.EvidenceHealth = rpc.AlertEvidenceStale
			}
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Source != candidates[j].Source {
			return candidates[i].Source < candidates[j].Source
		}
		if candidates[i].Kind != candidates[j].Kind {
			return candidates[i].Kind < candidates[j].Kind
		}
		return candidates[i].EpisodeKey < candidates[j].EpisodeKey
	})
	state := rpc.AlertSnapshotUnknown
	for _, candidate := range candidates {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			state = rpc.AlertSnapshotActive
			break
		}
	}
	if state != rpc.AlertSnapshotActive && coverage.State == rpc.AlertCoverageComplete && coverage.Freshness == rpc.AlertCoverageCurrent {
		state = rpc.AlertSnapshotClear
	}
	snapshot := rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: document.AuthorityScope, AsOf: now,
		CurrentState: state, Coverage: coverage, Sources: sources, Candidates: candidates,
	}
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return rpc.AlertCandidateSnapshot{}, fmt.Errorf("validate alert episode snapshot: %w", err)
	}
	return snapshot, nil
}

func projectAlertSourceCoverage(document alertEpisodeRegistryScopeDocument, coverage rpc.AlertCoverage, stale map[rpc.AlertSource]struct{}, now time.Time) []rpc.AlertSourceCoverage {
	states := make(map[rpc.AlertSource]alertEpisodeRegistrySourceState, len(document.SourceStates))
	for _, state := range document.SourceStates {
		states[state.Source] = state
	}
	rows := make([]rpc.AlertSourceCoverage, 0, len(coverage.ExpectedSources))
	for _, source := range coverage.ExpectedSources {
		state, ok := states[source]
		if !ok {
			row := rpc.AlertSourceCoverage{Source: source, Status: alertShadowStatusNotObserved, Reason: alertShadowReasonNotObserved, EvidenceHealth: rpc.AlertEvidenceUnavailable}
			if alertCoverageContains(coverage.CoveredSources, source) {
				row.Status, row.Reason, row.EvidenceHealth, row.Covered = alertShadowStatusCurrent, alertShadowReasonCurrent, rpc.AlertEvidenceCurrent, true
				row.InputAsOf, row.ObservedAt, row.EvidenceAsOf, row.FreshUntil = document.AsOf, document.AsOf, document.AsOf, now
			}
			rows = append(rows, row)
			continue
		}
		row := rpc.AlertSourceCoverage{
			Source: state.Source, Status: state.Status, Reason: state.Reason, EvidenceHealth: state.EvidenceHealth,
			InputAsOf: state.InputAsOf, ObservedAt: state.ObservedAt, EvidenceAsOf: state.EvidenceAsOf,
			FreshUntil: state.FreshUntil, Covered: state.Covered,
		}
		if _, expired := stale[source]; expired {
			row.Status, row.Reason = alertShadowStatusStale, alertShadowReasonProducerSilent
			if row.EvidenceHealth != rpc.AlertEvidenceError {
				row.EvidenceHealth = rpc.AlertEvidenceStale
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Source < rows[j].Source })
	return rows
}

func unavailableAlertSourceCoverage(expected []rpc.AlertSource) []rpc.AlertSourceCoverage {
	rows := make([]rpc.AlertSourceCoverage, 0, len(expected))
	for _, source := range expected {
		rows = append(rows, rpc.AlertSourceCoverage{
			Source: source, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceTimeInvalid,
			EvidenceHealth: rpc.AlertEvidenceUnavailable,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Source < rows[j].Source })
	return rows
}

func projectAlertEpisodeCoverage(document alertEpisodeRegistryScopeDocument, now time.Time) (rpc.AlertCoverage, map[rpc.AlertSource]struct{}) {
	if len(document.SourceStates) == 0 {
		coverage := cloneAlertCoverage(document.Coverage)
		coverage.AsOf = now
		return coverage, map[rpc.AlertSource]struct{}{}
	}
	coveredState := make(map[rpc.AlertSource]alertEpisodeRegistrySourceState, len(document.SourceStates))
	stale := make(map[rpc.AlertSource]struct{})
	for _, state := range document.SourceStates {
		coveredState[state.Source] = state
	}
	covered := make([]rpc.AlertSource, 0, len(document.SourceStates))
	for _, source := range document.Coverage.ExpectedSources {
		state, ok := coveredState[source]
		if !ok {
			continue
		}
		if !state.Covered {
			continue
		}
		covered = append(covered, source)
		if !state.FreshUntil.IsZero() && now.After(state.FreshUntil) {
			stale[source] = struct{}{}
		}
	}
	coverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: now,
		ExpectedSources: cloneAlertSources(document.Coverage.ExpectedSources), CoveredSources: covered,
	}
	if len(covered) > 0 {
		coverage.State = rpc.AlertCoveragePartial
		if len(covered) == len(coverage.ExpectedSources) {
			coverage.State = rpc.AlertCoverageComplete
		}
		coverage.Freshness = rpc.AlertCoverageCurrent
		if len(stale) > 0 {
			coverage.Freshness = rpc.AlertCoverageStale
		}
	}
	return coverage, stale
}

func alertCandidateFromRecord(record alertEpisodeRegistryRecord) rpc.AlertCandidate {
	return rpc.AlertCandidate{
		EpisodeKey: record.EpisodeKey, OccurrenceKey: record.OccurrenceKey,
		EvidenceFingerprint: record.EvidenceFingerprint, Source: record.Source, Kind: record.Kind,
		PresentationCode: record.PresentationCode, State: record.State, Severity: record.Severity,
		EvidenceHealth: record.EvidenceHealth, Destination: record.Destination,
		EvidenceAsOf: record.EvidenceAsOf, StateChangedAt: record.StateChangedAt, ObservedAt: record.ObservedAt,
	}
}

func decisionFromAlertRecord(record alertEpisodeRegistryRecord, before rpc.AlertEpisodeState, action string) alertEpisodeDecision {
	return alertEpisodeDecision{
		EpisodeKey: record.EpisodeKey, OccurrenceKey: record.OccurrenceKey, Source: record.Source, Kind: record.Kind,
		PresentationCode: record.PresentationCode, Action: action, BeforeState: before, AfterState: record.State, Severity: record.Severity,
		EvidenceFingerprint: record.EvidenceFingerprint,
		EvidenceHealth:      record.EvidenceHealth, Destination: record.Destination, EvidenceAsOf: record.EvidenceAsOf,
		ObservedAt: record.ObservedAt, PolicyFingerprint: record.PolicyFingerprint,
		ProducerDecisionReason: record.ProducerDecisionReason, RegistryDecisionReason: record.RegistryDecisionReason,
	}
}

func decisionFromAlertObservation(observation alertEpisodeObservation, action string, after rpc.AlertEpisodeState) alertEpisodeDecision {
	return alertEpisodeDecision{
		EpisodeKey: observation.EpisodeKey, Source: observation.Source, Kind: observation.Kind,
		PresentationCode: observation.PresentationCode, Action: action, AfterState: after, Severity: observation.Severity,
		EvidenceFingerprint: observation.EvidenceFingerprint,
		EvidenceHealth:      observation.EvidenceHealth, Destination: observation.Destination,
		EvidenceAsOf: observation.EvidenceAsOf, ObservedAt: observation.ObservedAt,
		PolicyFingerprint: observation.PolicyFingerprint, ProducerDecisionReason: observation.ProducerDecisionReason,
		RegistryDecisionReason: alertDecisionReasonAuthoritativeNegative,
	}
}

func validateAlertEpisodeEvaluation(evaluation alertEpisodeEvaluation) error {
	if err := rpc.ValidateAlertAuthorityScope(evaluation.AuthorityScope); err != nil {
		return err
	}
	if evaluation.AsOf.IsZero() {
		return errors.New("alert episode evaluation requires as_of")
	}
	if err := evaluation.Coverage.Validate(); err != nil {
		return fmt.Errorf("invalid alert episode coverage: %w", err)
	}
	if evaluation.Coverage.AsOf.After(evaluation.AsOf) {
		return errors.New("alert episode coverage is after evaluation as_of")
	}
	if evaluation.Coverage.Freshness == rpc.AlertCoverageCurrent && !evaluation.Coverage.AsOf.Equal(evaluation.AsOf) {
		return errors.New("current alert episode coverage must match evaluation as_of")
	}
	if evaluation.Observations == nil {
		return errors.New("alert episode evaluation requires observations")
	}
	opportunities := make(map[rpc.AlertSource]struct{}, len(evaluation.OpportunitySources))
	for _, source := range evaluation.OpportunitySources {
		if !alertCoverageContains(evaluation.Coverage.ExpectedSources, source) {
			return errors.New("alert episode opportunity source is outside expected coverage")
		}
		if _, duplicate := opportunities[source]; duplicate {
			return errors.New("alert episode evaluation contains duplicate opportunity source")
		}
		opportunities[source] = struct{}{}
	}
	seenStates := make(map[rpc.AlertSource]struct{}, len(evaluation.SourceStates))
	for _, state := range evaluation.SourceStates {
		if err := validateAlertEpisodeSourceState(state, evaluation.Coverage.ExpectedSources, evaluation.AsOf); err != nil {
			return err
		}
		if _, duplicate := seenStates[state.Source]; duplicate {
			return errors.New("alert episode evaluation contains duplicate source state")
		}
		seenStates[state.Source] = struct{}{}
		if len(opportunities) > 0 {
			if _, ok := opportunities[state.Source]; !ok {
				return errors.New("alert episode source state has no evaluation opportunity")
			}
		}
	}
	if evaluation.CursorKind != "" {
		if evaluation.CursorKind != alertShadowCursorStress && evaluation.CursorKind != alertShadowCursorNudges &&
			evaluation.CursorKind != alertShadowCursorRegime && evaluation.CursorKind != alertShadowCursorRulebook &&
			evaluation.CursorKind != alertShadowCursorProtection && evaluation.CursorKind != alertShadowCursorOrderIntegrity &&
			evaluation.CursorKind != alertShadowCursorDataHealth {
			return errors.New("alert episode evaluation has invalid cursor kind")
		}
		if err := validateAlertShadowInputCursor(evaluation.Cursor); err != nil {
			return err
		}
	}
	for i, observation := range evaluation.Observations {
		if err := validateAlertEpisodeObservation(observation, evaluation.Coverage, evaluation.AsOf); err != nil {
			return fmt.Errorf("invalid alert episode observation %d: %w", i, err)
		}
	}
	return nil
}

func validateAlertEpisodeSourceState(state alertEpisodeRegistrySourceState, expected []rpc.AlertSource, asOf time.Time) error {
	if !alertCoverageContains(expected, state.Source) {
		return errors.New("alert episode source state is outside expected coverage")
	}
	if !validAlertDecisionCode(state.Status) || !validAlertDecisionCode(state.Reason) {
		return errors.New("alert episode source state has invalid status or reason")
	}
	if state.InputAsOf.IsZero() || state.ObservedAt.IsZero() || state.EvidenceAsOf.IsZero() || state.FreshUntil.IsZero() {
		return errors.New("alert episode source state requires timestamps")
	}
	if state.InputAsOf.After(state.ObservedAt) || state.ObservedAt.After(asOf) || state.EvidenceAsOf.After(asOf) {
		return errors.New("alert episode source state timestamps are out of order")
	}
	if state.FreshUntil.Before(state.ObservedAt) {
		return errors.New("alert episode source state fresh_until precedes observation")
	}
	switch state.EvidenceHealth {
	case rpc.AlertEvidenceCurrent, rpc.AlertEvidencePartial, rpc.AlertEvidenceStale, rpc.AlertEvidenceUnavailable, rpc.AlertEvidenceError:
	default:
		return errors.New("alert episode source state has invalid evidence health")
	}
	if state.Covered && state.EvidenceHealth != rpc.AlertEvidenceCurrent {
		return errors.New("covered alert episode source state requires current evidence")
	}
	return nil
}

func validateAlertEpisodeObservation(observation alertEpisodeObservation, coverage rpc.AlertCoverage, asOf time.Time) error {
	dummyOccurrence, err := rpc.BuildAlertOccurrenceKey(observation.EpisodeKey, "validation")
	if err != nil {
		return err
	}
	dummyState := rpc.AlertEpisodeOpen
	candidate := rpc.AlertCandidate{
		EpisodeKey: observation.EpisodeKey, OccurrenceKey: dummyOccurrence,
		EvidenceFingerprint: observation.EvidenceFingerprint, Source: observation.Source, Kind: observation.Kind,
		PresentationCode: observation.PresentationCode, State: dummyState, Severity: observation.Severity,
		EvidenceHealth: observation.EvidenceHealth, Destination: observation.Destination,
		EvidenceAsOf: observation.EvidenceAsOf, StateChangedAt: observation.ObservedAt, ObservedAt: observation.ObservedAt,
	}
	if err := rpc.ValidateAlertCandidate(candidate); err != nil {
		return err
	}
	if observation.ObservedAt.After(asOf) {
		return errors.New("observation observed_at is after evaluation as_of")
	}
	if !alertCoverageContains(coverage.ExpectedSources, observation.Source) {
		return errors.New("observation source is outside expected coverage")
	}
	if observation.EvidenceHealth == rpc.AlertEvidenceCurrent && !alertCoverageContains(coverage.CoveredSources, observation.Source) {
		return errors.New("current observation source is not covered")
	}
	if !validAlertRegistryFingerprint(observation.PolicyFingerprint) {
		return errors.New("observation policy fingerprint is invalid")
	}
	if observation.EscalationFingerprint != "" && !validAlertRegistryFingerprint(observation.EscalationFingerprint) {
		return errors.New("observation escalation fingerprint is invalid")
	}
	if !validAlertDecisionCode(observation.ProducerDecisionReason) {
		return errors.New("observation producer decision reason is invalid")
	}
	return nil
}

func validateAlertEpisodeRegistryDocument(document alertEpisodeRegistryDocument, inactiveLimit int) error {
	if document.Version != alertEpisodeRegistryDocumentVersion {
		return errors.New("invalid alert episode registry version")
	}
	if document.UpdatedAt.IsZero() {
		return errors.New("alert episode registry is missing updated_at")
	}
	if document.Scopes == nil {
		return errors.New("alert episode registry requires scopes")
	}
	if document.LegacyUnscoped != nil {
		legacy := document.LegacyUnscoped
		if legacy.MigratedAt.IsZero() || legacy.MigratedAt.After(document.UpdatedAt) || !validAlertRegistryFingerprint(legacy.Fingerprint) || len(legacy.Document) == 0 {
			return errors.New("alert episode registry legacy unscoped evidence is invalid")
		}
		digest := sha256.Sum256(legacy.Document)
		if legacy.Fingerprint != "sha256:"+hex.EncodeToString(digest[:]) {
			return errors.New("alert episode registry legacy unscoped fingerprint mismatch")
		}
		decoded, err := decodeAlertEpisodeRegistryDocumentV1(legacy.Document)
		if err != nil {
			return fmt.Errorf("decode alert episode registry legacy unscoped evidence: %w", err)
		}
		if err := validateAlertEpisodeRegistryDocumentV1(decoded, inactiveLimit); err != nil {
			return fmt.Errorf("validate alert episode registry legacy unscoped evidence: %w", err)
		}
	}
	seenOccurrences := make(map[string]struct{})
	totalEpisodes := 0
	previousScope := ""
	for scopeIndex, scopeDocument := range document.Scopes {
		if err := rpc.ValidateAlertAuthorityScope(scopeDocument.AuthorityScope); err != nil {
			return fmt.Errorf("invalid alert episode registry scope %d: %w", scopeIndex, err)
		}
		if previousScope != "" && scopeDocument.AuthorityScope <= previousScope {
			return errors.New("alert episode registry scopes are not canonical")
		}
		previousScope = scopeDocument.AuthorityScope
		if scopeDocument.AsOf.IsZero() || scopeDocument.AsOf.After(document.UpdatedAt) {
			return fmt.Errorf("invalid alert episode registry scope timestamp at %d", scopeIndex)
		}
		if err := scopeDocument.Coverage.Validate(); err != nil {
			return fmt.Errorf("invalid alert episode registry scope coverage %d: %w", scopeIndex, err)
		}
		if scopeDocument.Coverage.AsOf.After(scopeDocument.AsOf) ||
			(scopeDocument.Coverage.Freshness == rpc.AlertCoverageCurrent && !scopeDocument.Coverage.AsOf.Equal(scopeDocument.AsOf)) {
			return fmt.Errorf("invalid alert episode registry scope coverage timestamp at %d", scopeIndex)
		}
		if scopeDocument.SourceStates == nil || scopeDocument.Episodes == nil {
			return fmt.Errorf("alert episode registry scope %d requires source states and episodes", scopeIndex)
		}
		previousSource := rpc.AlertSource("")
		for _, sourceState := range scopeDocument.SourceStates {
			if previousSource != "" && sourceState.Source <= previousSource {
				return errors.New("alert episode registry source states are not canonical")
			}
			previousSource = sourceState.Source
			if err := validateAlertEpisodeSourceState(sourceState, scopeDocument.Coverage.ExpectedSources, scopeDocument.AsOf); err != nil {
				return fmt.Errorf("invalid alert episode registry source state: %w", err)
			}
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.Stress); err != nil {
			return fmt.Errorf("invalid alert episode registry Stress cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.Nudges); err != nil {
			return fmt.Errorf("invalid alert episode registry Nudge cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.Regime); err != nil {
			return fmt.Errorf("invalid alert episode registry Regime cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.Rulebook); err != nil {
			return fmt.Errorf("invalid alert episode registry Rulebook cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.Protection); err != nil {
			return fmt.Errorf("invalid alert episode registry Protection cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.OrderIntegrity); err != nil {
			return fmt.Errorf("invalid alert episode registry Order Integrity cursor: %w", err)
		}
		if err := validateAlertShadowInputCursorOptional(scopeDocument.Cursors.DataHealth); err != nil {
			return fmt.Errorf("invalid alert episode registry Data Health cursor: %w", err)
		}
		if err := validateAlertShadowDurableMetrics(scopeDocument.Metrics, document.UpdatedAt); err != nil {
			return fmt.Errorf("invalid alert episode registry commissioning metrics: %w", err)
		}
		seenEpisodes := make(map[string]struct{}, len(scopeDocument.Episodes))
		inactive := 0
		previousKey := ""
		for recordIndex, record := range scopeDocument.Episodes {
			if record.AuthorityScope != scopeDocument.AuthorityScope {
				return fmt.Errorf("alert episode registry record %d has wrong authority scope", recordIndex)
			}
			if previousKey != "" && record.EpisodeKey <= previousKey {
				return errors.New("alert episode registry episodes are not canonical")
			}
			previousKey = record.EpisodeKey
			candidate := alertCandidateFromRecord(record)
			if err := rpc.ValidateAlertCandidate(candidate); err != nil {
				return fmt.Errorf("invalid alert episode registry record %d: %w", recordIndex, err)
			}
			if record.ObservedAt.After(scopeDocument.AsOf) {
				return fmt.Errorf("alert episode registry record %d is after scoped as_of", recordIndex)
			}
			if _, duplicate := seenEpisodes[record.EpisodeKey]; duplicate {
				return errors.New("alert episode registry contains duplicate episode")
			}
			seenEpisodes[record.EpisodeKey] = struct{}{}
			if _, duplicate := seenOccurrences[record.OccurrenceKey]; duplicate {
				return errors.New("alert episode registry contains duplicate occurrence")
			}
			seenOccurrences[record.OccurrenceKey] = struct{}{}
			if !validAlertRegistryFingerprint(record.PolicyFingerprint) || !validAlertRegistryFingerprint(record.LastObservationFingerprint) {
				return fmt.Errorf("invalid alert episode registry audit fingerprint at record %d", recordIndex)
			}
			if record.LastEscalationFingerprint != "" && !validAlertRegistryFingerprint(record.LastEscalationFingerprint) {
				return fmt.Errorf("invalid alert episode registry escalation fingerprint at record %d", recordIndex)
			}
			if !validAlertDecisionCode(record.ProducerDecisionReason) || !validAlertDecisionCode(record.RegistryDecisionReason) {
				return fmt.Errorf("invalid alert episode registry decision reason at record %d", recordIndex)
			}
			if record.LastSourceObservedAt.IsZero() || record.LastSourceObservedAt.After(scopeDocument.AsOf) {
				return fmt.Errorf("invalid alert episode registry source timestamp at record %d", recordIndex)
			}
			if record.State == rpc.AlertEpisodeRecovered {
				inactive++
			} else if record.EmitRecovered {
				return fmt.Errorf("active alert episode registry record %d emits recovery", recordIndex)
			}
		}
		if inactive > inactiveLimit {
			return errors.New("alert episode registry inactive history exceeds bound")
		}
		totalEpisodes += len(scopeDocument.Episodes)
	}
	if document.NextOccurrenceSequence < uint64(totalEpisodes) {
		return errors.New("alert episode registry occurrence sequence regressed")
	}
	return nil
}

func decodeAlertEpisodeRegistryDocument(raw []byte) (alertEpisodeRegistryDocument, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document alertEpisodeRegistryDocument
	if err := decoder.Decode(&document); err != nil {
		return alertEpisodeRegistryDocument{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return alertEpisodeRegistryDocument{}, errors.New("alert episode registry contains trailing JSON")
		}
		return alertEpisodeRegistryDocument{}, err
	}
	return document, nil
}

func decodeAlertEpisodeRegistryDocumentV1(raw []byte) (alertEpisodeRegistryDocumentV1, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document alertEpisodeRegistryDocumentV1
	if err := decoder.Decode(&document); err != nil {
		return alertEpisodeRegistryDocumentV1{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return alertEpisodeRegistryDocumentV1{}, errors.New("legacy alert episode registry contains trailing JSON")
		}
		return alertEpisodeRegistryDocumentV1{}, err
	}
	return document, nil
}

func decodeAlertEpisodeRegistryDocumentV2(raw []byte) (alertEpisodeRegistryDocumentV2, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document alertEpisodeRegistryDocumentV2
	if err := decoder.Decode(&document); err != nil {
		return alertEpisodeRegistryDocumentV2{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return alertEpisodeRegistryDocumentV2{}, errors.New("legacy alert episode registry v2 contains trailing JSON")
		}
		return alertEpisodeRegistryDocumentV2{}, err
	}
	if document.Version != 2 {
		return alertEpisodeRegistryDocumentV2{}, errors.New("invalid legacy alert episode registry v2 version")
	}
	return document, nil
}

func decodeAlertEpisodeRegistryDocumentV3(raw []byte) (alertEpisodeRegistryDocumentV3, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var document alertEpisodeRegistryDocumentV3
	if err := decoder.Decode(&document); err != nil {
		return alertEpisodeRegistryDocumentV3{}, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return alertEpisodeRegistryDocumentV3{}, errors.New("legacy alert episode registry v3 contains trailing JSON")
		}
		return alertEpisodeRegistryDocumentV3{}, err
	}
	if document.Version != 3 {
		return alertEpisodeRegistryDocumentV3{}, errors.New("invalid legacy alert episode registry v3 version")
	}
	return document, nil
}

func validateAlertEpisodeRegistryDocumentV1(document alertEpisodeRegistryDocumentV1, inactiveLimit int) error {
	if document.Version != 1 || document.AsOf.IsZero() {
		return errors.New("invalid legacy alert episode registry header")
	}
	if err := document.Coverage.Validate(); err != nil {
		return err
	}
	if document.Coverage.AsOf.After(document.AsOf) ||
		(document.Coverage.Freshness == rpc.AlertCoverageCurrent && !document.Coverage.AsOf.Equal(document.AsOf)) {
		return errors.New("invalid legacy alert episode registry coverage timestamp")
	}
	if document.Episodes == nil {
		return errors.New("legacy alert episode registry requires episodes")
	}
	seenEpisodes := make(map[string]struct{}, len(document.Episodes))
	seenOccurrences := make(map[string]struct{}, len(document.Episodes))
	previous := ""
	inactive := 0
	for i, record := range document.Episodes {
		if previous != "" && record.EpisodeKey <= previous {
			return errors.New("legacy alert episode registry episodes are not canonical")
		}
		previous = record.EpisodeKey
		candidate := rpc.AlertCandidate{
			EpisodeKey: record.EpisodeKey, OccurrenceKey: record.OccurrenceKey,
			EvidenceFingerprint: record.EvidenceFingerprint, Source: record.Source, Kind: record.Kind,
			PresentationCode: legacyAlertPresentationCode(record.Source), State: record.State, Severity: record.Severity,
			EvidenceHealth: record.EvidenceHealth, Destination: record.Destination,
			EvidenceAsOf: record.EvidenceAsOf, StateChangedAt: record.StateChangedAt, ObservedAt: record.ObservedAt,
		}
		if err := rpc.ValidateAlertCandidate(candidate); err != nil {
			return fmt.Errorf("invalid legacy alert episode registry record %d: %w", i, err)
		}
		if !validLegacyAlertDeliveryPreference(record.DeliveryPreference) {
			return fmt.Errorf("invalid legacy alert episode registry delivery preference at record %d", i)
		}
		if record.ObservedAt.After(document.AsOf) || record.LastSourceObservedAt.IsZero() || record.LastSourceObservedAt.After(document.AsOf) {
			return fmt.Errorf("invalid legacy alert episode registry timestamp at record %d", i)
		}
		if _, duplicate := seenEpisodes[record.EpisodeKey]; duplicate {
			return errors.New("legacy alert episode registry contains duplicate episode")
		}
		seenEpisodes[record.EpisodeKey] = struct{}{}
		if _, duplicate := seenOccurrences[record.OccurrenceKey]; duplicate {
			return errors.New("legacy alert episode registry contains duplicate occurrence")
		}
		seenOccurrences[record.OccurrenceKey] = struct{}{}
		if !validAlertRegistryFingerprint(record.PolicyFingerprint) || !validAlertRegistryFingerprint(record.LastObservationFingerprint) ||
			(record.LastEscalationFingerprint != "" && !validAlertRegistryFingerprint(record.LastEscalationFingerprint)) {
			return fmt.Errorf("invalid legacy alert episode registry fingerprint at record %d", i)
		}
		if !validAlertDecisionCode(record.ProducerDecisionReason) || !validAlertDecisionCode(record.RegistryDecisionReason) {
			return fmt.Errorf("invalid legacy alert episode registry reason at record %d", i)
		}
		if record.State == rpc.AlertEpisodeRecovered {
			inactive++
		} else if record.EmitRecovered {
			return fmt.Errorf("active legacy alert episode registry record %d emits recovery", i)
		}
	}
	if inactive > inactiveLimit || document.NextOccurrenceSequence < uint64(len(document.Episodes)) {
		return errors.New("legacy alert episode registry bounds are invalid")
	}
	return nil
}

func alertEpisodeObservationFingerprint(observation alertEpisodeObservation) (string, error) {
	raw, err := json.Marshal(struct {
		EpisodeKey             string                    `json:"episode_key"`
		Source                 rpc.AlertSource           `json:"source"`
		Kind                   rpc.AlertKind             `json:"kind"`
		PresentationCode       rpc.AlertPresentationCode `json:"presentation_code"`
		Active                 bool                      `json:"active"`
		EscalationFingerprint  string                    `json:"escalation_fingerprint,omitempty"`
		Severity               rpc.AlertSeverity         `json:"severity"`
		EvidenceFingerprint    string                    `json:"evidence_fingerprint"`
		EvidenceHealth         rpc.AlertEvidenceHealth   `json:"evidence_health"`
		Destination            rpc.AlertDestination      `json:"destination"`
		EvidenceAsOf           time.Time                 `json:"evidence_as_of"`
		ObservedAt             time.Time                 `json:"observed_at"`
		PolicyFingerprint      string                    `json:"policy_fingerprint"`
		ProducerDecisionReason string                    `json:"producer_decision_reason"`
	}{
		EpisodeKey: observation.EpisodeKey, Source: observation.Source, Kind: observation.Kind,
		PresentationCode: observation.PresentationCode,
		Active:           observation.Active, EscalationFingerprint: observation.EscalationFingerprint,
		Severity:            observation.Severity,
		EvidenceFingerprint: observation.EvidenceFingerprint, EvidenceHealth: observation.EvidenceHealth,
		Destination: observation.Destination, EvidenceAsOf: observation.EvidenceAsOf,
		ObservedAt: observation.ObservedAt, PolicyFingerprint: observation.PolicyFingerprint,
		ProducerDecisionReason: observation.ProducerDecisionReason,
	})
	if err != nil {
		return "", fmt.Errorf("fingerprint alert episode observation: %w", err)
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validAlertRegistryFingerprint(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && value == strings.ToLower(value)
}

func validAlertDecisionCode(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func validLegacyAlertDeliveryPreference(value string) bool {
	switch value {
	case "unapproved", "record_only", "inbox", "digest", "page":
		return true
	default:
		return false
	}
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

func cloneAlertCoverage(in rpc.AlertCoverage) rpc.AlertCoverage {
	out := in
	out.ExpectedSources = make([]rpc.AlertSource, len(in.ExpectedSources))
	copy(out.ExpectedSources, in.ExpectedSources)
	out.CoveredSources = make([]rpc.AlertSource, len(in.CoveredSources))
	copy(out.CoveredSources, in.CoveredSources)
	return out
}

func cloneAlertSources(in []rpc.AlertSource) []rpc.AlertSource {
	out := make([]rpc.AlertSource, len(in))
	copy(out, in)
	return out
}

func findAlertEpisodeScope(scopes []alertEpisodeRegistryScopeDocument, authorityScope string) (alertEpisodeRegistryScopeDocument, bool) {
	for _, scopeDocument := range scopes {
		if scopeDocument.AuthorityScope == authorityScope {
			return scopeDocument, true
		}
	}
	return alertEpisodeRegistryScopeDocument{}, false
}

func cloneAlertEpisodeScopeDocument(in alertEpisodeRegistryScopeDocument) alertEpisodeRegistryScopeDocument {
	out := in
	out.Coverage = cloneAlertCoverage(in.Coverage)
	out.SourceStates = make([]alertEpisodeRegistrySourceState, len(in.SourceStates))
	copy(out.SourceStates, in.SourceStates)
	out.Metrics.Sources = make([]alertShadowDurableSourceMeasurements, len(in.Metrics.Sources))
	copy(out.Metrics.Sources, in.Metrics.Sources)
	out.Episodes = make([]alertEpisodeRegistryRecord, len(in.Episodes))
	copy(out.Episodes, in.Episodes)
	return out
}

func cloneAlertEpisodeRegistryDocument(in alertEpisodeRegistryDocument) alertEpisodeRegistryDocument {
	out := in
	out.Scopes = make([]alertEpisodeRegistryScopeDocument, len(in.Scopes))
	for i := range in.Scopes {
		out.Scopes[i] = cloneAlertEpisodeScopeDocument(in.Scopes[i])
	}
	if in.LegacyUnscoped != nil {
		legacy := *in.LegacyUnscoped
		legacy.Document = append(json.RawMessage(nil), in.LegacyUnscoped.Document...)
		out.LegacyUnscoped = &legacy
	}
	return out
}

func mergeAlertEpisodeSourceStates(scopeDocument *alertEpisodeRegistryScopeDocument, updates []alertEpisodeRegistrySourceState) {
	if len(updates) == 0 {
		if scopeDocument.SourceStates == nil {
			scopeDocument.SourceStates = []alertEpisodeRegistrySourceState{}
		}
		return
	}
	bySource := make(map[rpc.AlertSource]alertEpisodeRegistrySourceState, len(scopeDocument.SourceStates)+len(updates))
	for _, state := range scopeDocument.SourceStates {
		state.DuplicateCandidates = 0
		bySource[state.Source] = state
	}
	for _, state := range updates {
		persisted := state
		persisted.DuplicateCandidates = 0
		bySource[state.Source] = persisted
	}
	scopeDocument.SourceStates = scopeDocument.SourceStates[:0]
	for _, state := range bySource {
		scopeDocument.SourceStates = append(scopeDocument.SourceStates, state)
	}
	sort.Slice(scopeDocument.SourceStates, func(i, j int) bool {
		return scopeDocument.SourceStates[i].Source < scopeDocument.SourceStates[j].Source
	})
}

func alertEpisodeOpportunitySet(evaluation alertEpisodeEvaluation) map[rpc.AlertSource]struct{} {
	sources := evaluation.OpportunitySources
	if sources == nil {
		// Direct lifecycle tests and legacy internal callers predate explicit
		// producer opportunities. Treat their declared expected universe as the
		// opportunity boundary; production composer calls always pass a precise
		// non-nil set.
		sources = evaluation.Coverage.ExpectedSources
	}
	out := make(map[rpc.AlertSource]struct{}, len(sources))
	for _, source := range sources {
		out[source] = struct{}{}
	}
	return out
}

func newAlertShadowDurableSourceMeasurements() []alertShadowDurableSourceMeasurements {
	out := make([]alertShadowDurableSourceMeasurements, 0, len(alertShadowExpectedSources))
	for _, source := range alertShadowExpectedSources {
		out = append(out, alertShadowDurableSourceMeasurements{Source: source})
	}
	return out
}

func alertShadowDurableSourceMetric(metrics *alertShadowDurableMetrics, source rpc.AlertSource) *alertShadowSourceMetrics {
	for i := range metrics.Sources {
		if metrics.Sources[i].Source == source {
			return &metrics.Sources[i].Measurements
		}
	}
	return nil
}

func applyAlertShadowEvaluationMetrics(scopeDocument *alertEpisodeRegistryScopeDocument, before alertEpisodeRegistryScopeDocument, evaluation alertEpisodeEvaluation, decisions []alertEpisodeDecision) {
	metrics := &scopeDocument.Metrics
	if metrics.Sources == nil {
		metrics.Sources = newAlertShadowDurableSourceMeasurements()
	}
	metrics.AsOf = evaluation.AsOf.UTC()
	metrics.Evaluations++
	metrics.RegistryApplyFailures += evaluation.PendingApplyFailures
	metrics.LastErrorCode = ""

	covered := make(map[rpc.AlertSource]struct{}, len(evaluation.Coverage.CoveredSources))
	for _, source := range evaluation.Coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	activeCounts := make(map[rpc.AlertSource]uint64)
	for _, observation := range evaluation.Observations {
		if observation.Active {
			activeCounts[observation.Source]++
		}
	}
	stateBySource := make(map[rpc.AlertSource]alertEpisodeRegistrySourceState, len(evaluation.SourceStates))
	for _, state := range evaluation.SourceStates {
		stateBySource[state.Source] = state
	}
	for source := range alertEpisodeOpportunitySet(evaluation) {
		metric := alertShadowDurableSourceMetric(metrics, source)
		if metric == nil {
			continue
		}
		metric.Evaluations++
		if _, ok := covered[source]; ok {
			metric.CoveredEvaluations++
		} else {
			metric.CoverageFailures++
		}
		if count := activeCounts[source]; count > 0 {
			metric.ActiveEvaluations++
			metric.ActiveObservations += count
		}
		if state, ok := stateBySource[source]; ok {
			metric.DuplicateCandidates += state.DuplicateCandidates
			if !state.InputAsOf.IsZero() {
				delay := max(evaluation.AsOf.Sub(state.InputAsOf), 0)
				metric.TimeToObserveSamples++
				metric.TimeToObserveTotal += delay
				if delay > metric.TimeToObserveMax {
					metric.TimeToObserveMax = delay
				}
			}
			if state.Status == alertShadowStatusStale {
				metric.StaleSuppressions++
			}
		}
	}

	beforeRecords := make(map[string]alertEpisodeRegistryRecord, len(before.Episodes))
	for _, record := range before.Episodes {
		beforeRecords[record.EpisodeKey] = record
	}
	for _, decision := range decisions {
		metric := alertShadowDurableSourceMetric(metrics, decision.Source)
		if metric == nil {
			continue
		}
		switch decision.Action {
		case alertDecisionOpened:
			metric.EpisodesOpened++
		case alertDecisionReopened:
			metric.EpisodesReopened++
		case alertDecisionEscalated:
			metric.EpisodesEscalated++
		case alertDecisionRecovered:
			metric.EpisodesRecovered++
		case alertDecisionRefreshedActive:
			if prior, ok := beforeRecords[decision.EpisodeKey]; ok {
				if prior.EvidenceFingerprint == decision.EvidenceFingerprint {
					metric.RepeatedActive++
				} else {
					metric.ActiveEvidenceChurn++
				}
			}
		}
	}
}

func applyAlertShadowCursor(scopeDocument *alertEpisodeRegistryScopeDocument, kind string, cursor alertShadowInputCursor) {
	switch kind {
	case alertShadowCursorStress:
		scopeDocument.Cursors.Stress = cursor
	case alertShadowCursorNudges:
		scopeDocument.Cursors.Nudges = cursor
	case alertShadowCursorRegime:
		scopeDocument.Cursors.Regime = cursor
	case alertShadowCursorRulebook:
		scopeDocument.Cursors.Rulebook = cursor
	case alertShadowCursorProtection:
		scopeDocument.Cursors.Protection = cursor
	case alertShadowCursorOrderIntegrity:
		scopeDocument.Cursors.OrderIntegrity = cursor
	case alertShadowCursorDataHealth:
		scopeDocument.Cursors.DataHealth = cursor
	}
}

func validateAlertShadowInputCursor(cursor alertShadowInputCursor) error {
	if cursor.AsOf.IsZero() || !validAlertRegistryFingerprint(cursor.Fingerprint) {
		return errors.New("alert registry input cursor is invalid")
	}
	return nil
}

func validateAlertShadowInputCursorOptional(cursor alertShadowInputCursor) error {
	if cursor.AsOf.IsZero() && cursor.Fingerprint == "" {
		return nil
	}
	return validateAlertShadowInputCursor(cursor)
}

func validateAlertShadowDurableMetrics(metrics alertShadowDurableMetrics, documentAsOf time.Time) error {
	if metrics.Sources == nil || len(metrics.Sources) != len(alertShadowExpectedSources) {
		return errors.New("commissioning metrics require the fixed source universe")
	}
	if !metrics.AsOf.IsZero() && metrics.AsOf.After(documentAsOf) {
		return errors.New("commissioning metrics timestamp is after document")
	}
	if metrics.LastErrorCode != "" && !validAlertDecisionCode(metrics.LastErrorCode) {
		return errors.New("commissioning metrics last error code is invalid")
	}
	for i, row := range metrics.Sources {
		if row.Source != alertShadowExpectedSources[i] {
			return errors.New("commissioning metrics source universe is not canonical")
		}
		measurement := row.Measurements
		if measurement.TimeToObserveTotal < 0 || measurement.TimeToObserveMax < 0 {
			return errors.New("commissioning metrics contain negative latency")
		}
		if measurement.TimeToObserveSamples == 0 && (measurement.TimeToObserveTotal != 0 || measurement.TimeToObserveMax != 0) {
			return errors.New("commissioning metrics latency has no samples")
		}
		if measurement.TimeToObserveSamples > 0 && measurement.TimeToObserveMax > measurement.TimeToObserveTotal {
			return errors.New("commissioning metrics maximum latency exceeds total")
		}
	}
	return nil
}
