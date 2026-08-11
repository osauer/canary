package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Alert-delivery constants version the app-local ledger and classify durable
const (
	AlertDeliveryVersion = "alert-delivery-v4"

	AlertDeliveryAttemptReserved       = "reserved"
	AlertDeliveryAttemptConfirmed      = "confirmed_pending_outcome"
	AlertDeliveryAttemptAccepted       = "push_service_accepted"
	AlertDeliveryAttemptRetry          = "retryable_failure"
	AlertDeliveryAttemptRejected       = "rejected"
	AlertDeliveryAttemptInterrupted    = "interrupted_uncertain"
	AlertDeliveryAttemptRetired        = "target_retired"
	AlertDeliveryAttemptInactive       = "occurrence_inactive"
	AlertDeliveryAttemptExhausted      = "retry_exhausted"
	AlertDeliveryAttemptModeSuppressed = "mode_suppressed"

	AlertDeliveryHealthHealthy     = "healthy"
	AlertDeliveryHealthDegraded    = "degraded"
	AlertDeliveryHealthUnavailable = "unavailable"
	AlertDeliveryHealthOverflow    = "overflow"

	AlertDeliveryHealthClassRetry          = "retry_pending"
	AlertDeliveryHealthClassRejected       = "transport_rejected"
	AlertDeliveryHealthClassObservation    = "producer_observation_rejected"
	AlertDeliveryHealthClassInterrupted    = "interrupted_uncertain"
	AlertDeliveryHealthClassStateWrite     = "state_write_failure"
	AlertDeliveryHealthClassOverflow       = "capacity_overflow"
	AlertDeliveryHealthClassNoSubscription = "no_active_subscription"
	AlertDeliveryHealthClassSigningKeys    = "signing_keys_unavailable"
	AlertDeliveryHealthClassSender         = "sender_unavailable"
	AlertDeliveryHealthClassFuse           = "push_fuse_tripped"

	AlertDeliveryEndRecovered  = "recovered"
	AlertDeliveryEndOmitted    = "authoritative_omission"
	AlertDeliveryEndSuperseded = "qualified_escalation"
	// AlertDeliveryEndAuthorityScopeChanged labels an immutable public boundary
	// projection when the daemon moves to another opaque account/mode authority.
	AlertDeliveryEndAuthorityScopeChanged = "authority_scope_changed"

	AlertDeliveryCompletionAccepted  AlertDeliveryCompletion = "accepted"
	AlertDeliveryCompletionRetryable AlertDeliveryCompletion = "retryable_failure"
	AlertDeliveryCompletionRejected  AlertDeliveryCompletion = "rejected"

	AlertDeliveryCompletionApplied         AlertDeliveryCompletionDisposition = "applied"
	AlertDeliveryCompletionAlreadyComplete AlertDeliveryCompletionDisposition = "already_complete"
	AlertDeliveryCompletionInactive        AlertDeliveryCompletionDisposition = "occurrence_inactive"
	AlertDeliveryCompletionRetired         AlertDeliveryCompletionDisposition = "target_retired"

	// AlertDispositionEligible and the related occurrence dispositions are
	// occurrence may only move to terminal suppression at the final mode gate;
	AlertDispositionEligible        = "eligible"
	AlertDispositionModeSuppressed  = "mode_suppressed"
	AlertDispositionObserveOnly     = "observe_inbox_only"
	AlertDispositionCutoverExisting = "cutover_existing"
)

const (
	defaultAlertDeliveryMaxItems = 4096
	alertDeliveryRetention       = 90 * 24 * time.Hour
)

// Alert-delivery errors expose bounded, redacted failure classes while
// occurrence, target, attempt, and persisted artifact identities stay private.
var (
	ErrAlertDeliveryOverflow          = errors.New("alert delivery evidence overflow")
	ErrAlertDeliveryOldSnapshot       = errors.New("alert delivery snapshot is older than source authority")
	ErrAlertDeliveryUnknownOccurrence = errors.New("alert delivery occurrence not found")
	ErrAlertDeliveryInvalidTransition = errors.New("invalid alert delivery lifecycle transition")
	ErrAlertDeliveryAttentionRead     = errors.New("alert delivery attention read cursor is invalid")
	ErrAlertDeliveryUnavailable       = errors.New("alert delivery state is unavailable")
)

// alertDeliveryData is an optional, independently versioned section of the
// health, and Web Push authority.
type alertDeliveryData struct {
	Version                  string                                   `json:"version"`
	Generation               uint64                                   `json:"generation"`
	Snapshot                 rpc.AlertCandidateSnapshot               `json:"snapshot"`
	SourceWatermarks         map[rpc.AlertSource]time.Time            `json:"source_watermarks"`
	SourceWatermarksByScope  map[string]map[rpc.AlertSource]time.Time `json:"source_watermarks_by_scope"`
	Episodes                 []alertDeliveryEpisode                   `json:"episodes,omitempty"`
	Occurrences              []alertDeliveryOccurrence                `json:"occurrences,omitempty"`
	PreviousContexts         []alertDeliveryPreviousContext           `json:"previous_contexts,omitempty"`
	PreviousContextHighWater uint64                                   `json:"previous_context_high_water_seq"`
	Attempts                 []alertDeliveryAttempt                   `json:"attempts,omitempty"`
	Receipts                 []alertDeliveryReceipt                   `json:"receipts,omitempty"`
	RetiredTargets           map[string]time.Time                     `json:"retired_targets"`
	Baselines                map[string]alertDeliveryBaseline         `json:"baselines"`
	Health                   AlertDeliveryHealth                      `json:"delivery_health"`
	// ObservationRejectedAt stamps the first producer snapshot refused since the
	// last accepted one. Transport health cannot express this: nothing is ever
	// attempted while intake is refused, so an empty attempt ledger would
	// otherwise read as healthy for as long as the refusal lasts.
	ObservationRejectedAt   time.Time `json:"observation_rejected_at,omitzero"`
	AttentionHighWaterSeq   uint64    `json:"attention_high_water_seq"`
	AttentionReadThroughSeq uint64    `json:"attention_read_through_seq"`
}

// alertDeliveryBaseline records the first complete/current producer snapshot
// observed for one opaque account/mode scope. Until it exists, every active
type alertDeliveryBaseline struct {
	EstablishedAt time.Time `json:"established_at"`
	SnapshotAsOf  time.Time `json:"snapshot_as_of"`
}

type alertDeliveryEpisode struct {
	AuthorityScope       string                `json:"authority_scope"`
	EpisodeKey           string                `json:"episode_key"`
	Source               rpc.AlertSource       `json:"source"`
	Kind                 rpc.AlertKind         `json:"kind"`
	CurrentOccurrenceKey string                `json:"current_occurrence_key"`
	State                rpc.AlertEpisodeState `json:"state"`
	FirstSeenAt          time.Time             `json:"first_seen_at"`
	LastSeenAt           time.Time             `json:"last_seen_at"`
}

type alertDeliveryOccurrence struct {
	AuthorityScope      string                    `json:"authority_scope"`
	OccurrenceKey       string                    `json:"occurrence_key"`
	EpisodeKey          string                    `json:"episode_key"`
	EvidenceFingerprint string                    `json:"evidence_fingerprint"`
	DisplayID           string                    `json:"display_id"`
	Source              rpc.AlertSource           `json:"source"`
	Kind                rpc.AlertKind             `json:"kind"`
	PresentationCode    rpc.AlertPresentationCode `json:"presentation_code"`
	State               rpc.AlertEpisodeState     `json:"state"`
	Severity            rpc.AlertSeverity         `json:"severity"`
	EvidenceHealth      rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination         rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf        time.Time                 `json:"evidence_as_of"`
	StateChangedAt      time.Time                 `json:"state_changed_at"`
	ObservedAt          time.Time                 `json:"observed_at"`
	FirstSeenAt         time.Time                 `json:"first_seen_at"`
	LastSeenAt          time.Time                 `json:"last_seen_at"`
	EndedAt             time.Time                 `json:"ended_at,omitzero"`
	EndReason           string                    `json:"end_reason,omitempty"`
	AttentionSeq        uint64                    `json:"attention_seq"`
	Disposition         string                    `json:"disposition"`
}

// alertDeliveryPreviousContext is an immutable, allowlisted audit projection
// created when the app moves away from an account/mode authority. It records
type alertDeliveryPreviousContext struct {
	AuthorityScope    string                    `json:"authority_scope"`
	ArchiveSeq        uint64                    `json:"archive_seq"`
	DisplayID         string                    `json:"display_id"`
	PriorDisplayID    string                    `json:"prior_display_id"`
	Source            rpc.AlertSource           `json:"source"`
	Kind              rpc.AlertKind             `json:"kind"`
	PresentationCode  rpc.AlertPresentationCode `json:"presentation_code"`
	State             rpc.AlertEpisodeState     `json:"state"`
	Severity          rpc.AlertSeverity         `json:"severity"`
	EvidenceHealth    rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination       rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf      time.Time                 `json:"evidence_as_of"`
	StateChangedAt    time.Time                 `json:"state_changed_at"`
	FirstSeenAt       time.Time                 `json:"first_seen_at"`
	LastSeenAt        time.Time                 `json:"last_seen_at"`
	EndedAt           time.Time                 `json:"ended_at"`
	EndReason         string                    `json:"end_reason"`
	OriginalAttention uint64                    `json:"original_attention_v2_seq"`
	Disposition       string                    `json:"disposition"`
}

type alertDeliveryAttempt struct {
	AuthorityScope string                             `json:"authority_scope"`
	ID             string                             `json:"id"`
	OccurrenceKey  string                             `json:"occurrence_key"`
	TargetRef      string                             `json:"target_ref"`
	ReceiptKey     string                             `json:"receipt_key"`
	AttemptNumber  int                                `json:"attempt_number"`
	ReservedAt     time.Time                          `json:"reserved_at"`
	CompletedAt    time.Time                          `json:"completed_at,omitzero"`
	Class          string                             `json:"class"`
	Disposition    AlertDeliveryCompletionDisposition `json:"disposition,omitempty"`
	RetryAt        time.Time                          `json:"retry_at,omitzero"`
	RetiredAt      time.Time                          `json:"retired_at,omitzero"`
}

type alertDeliveryReceipt struct {
	AuthorityScope string    `json:"authority_scope"`
	OccurrenceKey  string    `json:"occurrence_key"`
	TargetRef      string    `json:"target_ref"`
	ReceiptKey     string    `json:"receipt_key"`
	AcceptedAt     time.Time `json:"accepted_at"`
	RetiredAt      time.Time `json:"retired_at,omitzero"`
}

// AlertDeliveryOccurrenceView is safe for HTTP/SSE projection. Producer keys,
// evidence fingerprints, target identities, attempt IDs, and receipt keys are
// deliberately absent.
type AlertDeliveryOccurrenceView struct {
	DisplayID        string                    `json:"display_id"`
	Source           rpc.AlertSource           `json:"source"`
	Kind             rpc.AlertKind             `json:"kind"`
	PresentationCode rpc.AlertPresentationCode `json:"presentation_code"`
	State            rpc.AlertEpisodeState     `json:"state"`
	Severity         rpc.AlertSeverity         `json:"severity"`
	EvidenceHealth   rpc.AlertEvidenceHealth   `json:"evidence_health"`
	Destination      rpc.AlertDestination      `json:"destination"`
	EvidenceAsOf     time.Time                 `json:"evidence_as_of"`
	StateChangedAt   time.Time                 `json:"state_changed_at"`
	FirstSeenAt      time.Time                 `json:"first_seen_at"`
	LastSeenAt       time.Time                 `json:"last_seen_at"`
	EndedAt          time.Time                 `json:"ended_at,omitzero"`
	EndReason        string                    `json:"end_reason,omitempty"`
	AttentionSeq     uint64                    `json:"attention_seq"`
	Disposition      string                    `json:"disposition"`
}

// AlertDeliveryAttentionRef identifies one redacted unread occurrence without
type AlertDeliveryAttentionRef struct {
	DisplayID string          `json:"display_id"`
	Source    rpc.AlertSource `json:"source"`
	Kind      rpc.AlertKind   `json:"kind"`
}

// AlertDeliveryAttention is the source-neutral ledger's durable unread cursor.
type AlertDeliveryAttention struct {
	UnreadCount    int                         `json:"unread_count"`
	HighWaterSeq   uint64                      `json:"high_water_seq"`
	ReadThroughSeq uint64                      `json:"read_through_seq"`
	UnreadRefs     []AlertDeliveryAttentionRef `json:"unread_refs"`
}

// AlertDeliveryAttemptTotals is a redacted projection of durable attempt
type AlertDeliveryAttemptTotals struct {
	Attempts       int `json:"attempts"`
	Confirmed      int `json:"confirmed_pending_outcome"`
	Accepted       int `json:"push_service_accepted"`
	RetryPending   int `json:"retry_pending"`
	Rejected       int `json:"rejected"`
	Interrupted    int `json:"interrupted_uncertain"`
	TargetRetired  int `json:"target_retired"`
	Inactive       int `json:"occurrence_inactive"`
	RetryExhausted int `json:"retry_exhausted"`
	ModeSuppressed int `json:"mode_suppressed"`
}

// AlertDeliveryHealth summarizes app-local delivery readiness and outcomes.
type AlertDeliveryHealth struct {
	State          string    `json:"state"`
	Class          string    `json:"class,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	LastAcceptedAt time.Time `json:"last_push_service_acceptance_at,omitzero"`
}

// AlertDeliveryView is the redacted app projection of the source-neutral
// delivery ledger. Private authority scope, producer keys, target identities,
// attempts, and receipts are excluded from JSON.
type AlertDeliveryView struct {
	Initialized      bool                          `json:"initialized"`
	Version          string                        `json:"version,omitempty"`
	Generation       uint64                        `json:"generation"`
	AsOf             time.Time                     `json:"as_of,omitzero"`
	CurrentState     rpc.AlertSnapshotState        `json:"current_state,omitempty"`
	Coverage         rpc.AlertCoverage             `json:"coverage,omitzero"`
	Sources          []rpc.AlertSourceCoverage     `json:"sources,omitzero"`
	SourceWatermarks map[rpc.AlertSource]time.Time `json:"-"`
	Occurrences      []AlertDeliveryOccurrenceView `json:"occurrences"`
	Attention        AlertDeliveryAttention        `json:"attention"`
	AttemptTotals    AlertDeliveryAttemptTotals    `json:"-"`
	DeliveryHealth   AlertDeliveryHealth           `json:"delivery_health"`
	// AuthorityScope is private app coordination state. HTTP/SSE DTOs map the
	// view explicitly and JSON must never expose this opaque broker context.
	AuthorityScope string `json:"-"`
}

// AlertDeliveryReservation is the durable-before-send handoff for one
// occurrence-target attempt. Candidate is populated only after confirmation
type AlertDeliveryReservation struct {
	AttemptID     string    `json:"-"`
	DisplayID     string    `json:"display_id"`
	AttemptNumber int       `json:"attempt_number"`
	ReservedAt    time.Time `json:"reserved_at"`
	RetryAt       time.Time `json:"retry_at,omitzero"`
	// Candidate is populated only by a successful ConfirmAlertTransport.
	// It is the exact current candidate checked under the same store lock as
	// build transport copy from this value, never from a prior due-work scan.
	Candidate rpc.AlertCandidate `json:"-"`
}

// AlertDeliveryCompletion classifies the transport result supplied when a
// confirmed reservation is completed.
type AlertDeliveryCompletion string

// AlertDeliveryCompletionDisposition reports whether completion was applied,
// already known, inactive, or reconciled after target retirement.
type AlertDeliveryCompletionDisposition string

// AlertDeliveryCompletionOutcome reports the durable disposition and retry
type AlertDeliveryCompletionOutcome struct {
	Disposition AlertDeliveryCompletionDisposition `json:"disposition"`
	Class       string                             `json:"class"`
	RetryAt     time.Time                          `json:"retry_at,omitzero"`
}

// AlertDeliveryDueWork is an app-internal dispatch record. The private
// are excluded from JSON; DisplayID is the only public identity.
type AlertDeliveryDueWork struct {
	OccurrenceKey string             `json:"-"`
	Candidate     rpc.AlertCandidate `json:"-"`
	DisplayID     string             `json:"display_id"`
}

func (s *Store) initAlertDeliveryRuntime() {
	if s.alertDeliveryMaxItems <= 0 {
		s.alertDeliveryMaxItems = defaultAlertDeliveryMaxItems
	}
	if s.alertDeliveryInFlight == nil {
		s.alertDeliveryInFlight = make(map[string]bool)
	}
}

func newAlertDeliveryData() *alertDeliveryData {
	return &alertDeliveryData{
		Version:                 AlertDeliveryVersion,
		SourceWatermarks:        make(map[rpc.AlertSource]time.Time),
		SourceWatermarksByScope: make(map[string]map[rpc.AlertSource]time.Time),
		RetiredTargets:          make(map[string]time.Time),
		Baselines:               make(map[string]alertDeliveryBaseline),
	}
}

func bumpAlertDeliveryGeneration(data *alertDeliveryData) error {
	if data.Generation == ^uint64(0) {
		return ErrAttentionSequenceExhausted
	}
	data.Generation++
	return nil
}

func (s *Store) bumpAlertDeliveryGenerationLocked(data *alertDeliveryData) error {
	if err := bumpAlertDeliveryGeneration(data); err != nil {
		return err
	}
	// A volatile state-write failure is itself a public health generation.
	if s.alertDeliveryVolatile != nil {
		return bumpAlertDeliveryGeneration(data)
	}
	return nil
}

// noteAlertObservationRejectedLocked stamps the first producer snapshot refused
func (s *Store) noteAlertObservationRejectedLocked(prior *alertDeliveryData, at time.Time) {
	if prior == nil || !prior.ObservationRejectedAt.IsZero() {
		return
	}
	next := cloneAlertDeliveryData(prior)
	next.ObservationRejectedAt = at.UTC()
	s.recomputeAlertDeliveryHealthLocked(next, at)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(at)
		return
	}
	s.clearAlertDeliveryVolatileLocked()
}

func (s *Store) noteAlertDeliverySaveFailureLocked(at time.Time) {
	// Preserve one stable public outage generation until a successful write
	if s.alertDeliveryVolatile != nil {
		return
	}
	lastAccepted := time.Time{}
	if s.data.AlertDelivery != nil {
		lastAccepted = s.data.AlertDelivery.Health.LastAcceptedAt
	}
	health := AlertDeliveryHealth{State: AlertDeliveryHealthUnavailable, Class: AlertDeliveryHealthClassStateWrite, UpdatedAt: at.UTC(), LastAcceptedAt: lastAccepted}
	s.alertDeliveryVolatile = &health
	if s.data.AlertDelivery == nil {
		s.alertDeliveryVolatileGeneration = 1
	} else if s.data.AlertDelivery.Generation < ^uint64(0) {
		s.alertDeliveryVolatileGeneration = s.data.AlertDelivery.Generation + 1
	}
}

func (s *Store) clearAlertDeliveryVolatileLocked() {
	if s.alertDeliveryQuarantinedLocked() {
		return
	}
	s.alertDeliveryVolatile = nil
	s.alertDeliveryVolatileGeneration = 0
}

func (s *Store) setAlertDeliveryOverflowLocked(prior *alertDeliveryData, at time.Time) error {
	rollback := s.data.AlertDelivery
	next := cloneAlertDeliveryData(prior)
	if next == nil {
		next = newAlertDeliveryData()
	}
	if next.SourceWatermarksByScope == nil {
		next.SourceWatermarksByScope = make(map[string]map[rpc.AlertSource]time.Time)
	}
	if next.Snapshot.AuthorityScope != "" {
		next.SourceWatermarksByScope[next.Snapshot.AuthorityScope] = cloneAlertSourceWatermarks(next.SourceWatermarks)
	}
	next.Version = AlertDeliveryVersion
	next.Health = AlertDeliveryHealth{State: AlertDeliveryHealthOverflow, Class: AlertDeliveryHealthClassOverflow, UpdatedAt: at.UTC(), LastAcceptedAt: next.Health.LastAcceptedAt}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return ErrAlertDeliveryOverflow
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = rollback
		s.noteAlertDeliverySaveFailureLocked(at)
		return fmt.Errorf("%w: persist overflow health: %v", ErrAlertDeliveryOverflow, err)
	}
	s.clearAlertDeliveryVolatileLocked()
	return ErrAlertDeliveryOverflow
}

// ObserveAlertSnapshot validates and commits one complete producer contract.
// All lifecycle, authority-watermark, attention, and snapshot-view changes are
// persisted even when they only advance generation/coverage.
func (s *Store) ObserveAlertSnapshot(snapshot rpc.AlertCandidateSnapshot) (AlertDeliveryView, error) {
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return AlertDeliveryView{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return s.alertDeliveryViewLocked(nil, time.Now().UTC()), err
	}
	prior := s.data.AlertDelivery
	scopeChanged := prior != nil && prior.Generation > 0 && prior.Snapshot.AuthorityScope != snapshot.AuthorityScope
	allowBaselineRefresh := prior != nil && !alertDeliveryBaselineEstablished(prior, snapshot.AuthorityScope) &&
		alertSnapshotCanEstablishBaseline(snapshot) && prior.Snapshot.Coverage.State == rpc.AlertCoverageUnavailable
	if !scopeChanged && prior != nil && prior.Generation > 0 && snapshot.AsOf.Equal(prior.Snapshot.AsOf) && !allowBaselineRefresh {
		if !equalAlertCandidateSnapshot(prior.Snapshot, snapshot) {
			return AlertDeliveryView{}, fmt.Errorf("%w: snapshot %s conflicts with the persisted authority generation", ErrAlertDeliveryOldSnapshot, snapshot.AsOf.UTC().Format(time.RFC3339Nano))
		}
		return s.alertDeliveryViewLocked(prior, time.Now().UTC()), nil
	}
	next := cloneAlertDeliveryData(prior)
	if next == nil {
		next = newAlertDeliveryData()
	}
	if scopeChanged {
		if err := s.archiveAlertDeliveryAuthorityScopeLocked(next, snapshot); err != nil {
			if errors.Is(err, ErrAlertDeliveryOverflow) {
				return AlertDeliveryView{}, s.setAlertDeliveryOverflowLocked(prior, snapshot.AsOf)
			}
			return AlertDeliveryView{}, err
		}
		if err := rejectAlertSourceWatermarks(next, snapshot); err != nil {
			return AlertDeliveryView{}, err
		}
	} else {
		if err := rejectOldAlertSnapshot(next, snapshot); err != nil {
			return AlertDeliveryView{}, err
		}
	}
	if err := s.applyAlertSnapshotLocked(next, snapshot); err != nil {
		if errors.Is(err, ErrAlertDeliveryOverflow) {
			overflowBase := prior
			if overflowBase == nil {
				overflowBase = newAlertDeliveryData()
				overflowBase.Version = AlertDeliveryVersion
				overflowBase.Snapshot = unavailableAlertSnapshot(snapshot)
			}
			return AlertDeliveryView{}, s.setAlertDeliveryOverflowLocked(overflowBase, snapshot.AsOf)
		}
		if errors.Is(err, ErrAlertDeliveryInvalidTransition) {
			s.noteAlertObservationRejectedLocked(prior, snapshot.AsOf)
		}
		return AlertDeliveryView{}, err
	}
	// Only a snapshot carrying producer coverage can attest that intake works
	if snapshot.Coverage.State != rpc.AlertCoverageUnavailable {
		next.ObservationRejectedAt = time.Time{}
	}
	if alertSnapshotCanEstablishBaseline(snapshot) && !alertDeliveryBaselineEstablished(next, snapshot.AuthorityScope) {
		if next.Baselines == nil {
			next.Baselines = make(map[string]alertDeliveryBaseline)
		}
		if len(next.Baselines) >= s.alertDeliveryMaxItems {
			return AlertDeliveryView{}, s.setAlertDeliveryOverflowLocked(prior, snapshot.AsOf)
		}
		next.Baselines[snapshot.AuthorityScope] = alertDeliveryBaseline{
			EstablishedAt: snapshot.AsOf.UTC(), SnapshotAsOf: snapshot.AsOf.UTC(),
		}
	}
	s.recomputeAlertDeliveryHealthLocked(next, snapshot.AsOf)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return AlertDeliveryView{}, err
	}
	next.Snapshot = cloneAlertSnapshot(snapshot)
	next.Version = AlertDeliveryVersion
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(snapshot.AsOf)
		return AlertDeliveryView{}, err
	}
	s.clearAlertDeliveryVolatileLocked()
	return s.alertDeliveryViewLocked(next, time.Now().UTC()), nil
}

func unavailableAlertSnapshot(snapshot rpc.AlertCandidateSnapshot) rpc.AlertCandidateSnapshot {
	return rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AuthorityScope: snapshot.AuthorityScope,
		AsOf:           snapshot.AsOf,
		CurrentState:   rpc.AlertSnapshotUnknown,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: snapshot.AsOf,
			ExpectedSources: append([]rpc.AlertSource{}, snapshot.Coverage.ExpectedSources...),
			CoveredSources:  []rpc.AlertSource{},
		},
		Sources:    unavailableAlertSourceRows(snapshot.Coverage.ExpectedSources, "snapshot_unavailable"),
		Candidates: []rpc.AlertCandidate{},
	}
}

func (s *Store) archiveAlertDeliveryAuthorityScopeLocked(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	if data == nil {
		return nil
	}
	priorScope := data.Snapshot.AuthorityScope
	endedAt := snapshot.AsOf.UTC()
	if data.Snapshot.AsOf.After(endedAt) {
		endedAt = data.Snapshot.AsOf.UTC()
	}
	for i := range data.Episodes {
		episode := &data.Episodes[i]
		if episode.AuthorityScope != priorScope {
			continue
		}
		occurrence, _, ok := findAlertDeliveryOccurrence(data, priorScope, episode.CurrentOccurrenceKey)
		if !ok || !alertDeliveryOccurrenceActive(occurrence, episode) {
			continue
		}
		if len(data.PreviousContexts) >= s.alertDeliveryMaxItems {
			return ErrAlertDeliveryOverflow
		}
		if data.PreviousContextHighWater == ^uint64(0) {
			return ErrAlertDeliveryOverflow
		}
		data.PreviousContextHighWater++
		data.PreviousContexts = append(data.PreviousContexts, alertDeliveryPreviousContext{
			AuthorityScope: priorScope,
			ArchiveSeq:     data.PreviousContextHighWater,
			DisplayID:      alertPreviousContextDisplayID(priorScope, occurrence.DisplayID, endedAt, data.PreviousContextHighWater),
			PriorDisplayID: occurrence.DisplayID,
			Source:         occurrence.Source, Kind: occurrence.Kind, PresentationCode: occurrence.PresentationCode, State: occurrence.State,
			Severity:       occurrence.Severity,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt,
			FirstSeenAt: occurrence.FirstSeenAt, LastSeenAt: occurrence.LastSeenAt,
			EndedAt: endedAt, EndReason: AlertDeliveryEndAuthorityScopeChanged,
			OriginalAttention: occurrence.AttentionSeq, Disposition: occurrence.Disposition,
		})
	}
	if data.SourceWatermarksByScope == nil {
		data.SourceWatermarksByScope = make(map[string]map[rpc.AlertSource]time.Time)
	}
	data.SourceWatermarksByScope[priorScope] = cloneAlertSourceWatermarks(data.SourceWatermarks)
	data.SourceWatermarks = cloneAlertSourceWatermarks(data.SourceWatermarksByScope[snapshot.AuthorityScope])
	if data.Health.State == AlertDeliveryHealthOverflow || data.Health.State == AlertDeliveryHealthUnavailable ||
		(data.Health.State == AlertDeliveryHealthDegraded && data.Health.Class == AlertDeliveryHealthClassInterrupted) {
		data.Health.LastAcceptedAt = time.Time{}
	} else {
		data.Health = AlertDeliveryHealth{}
	}
	return nil
}

func rejectOldAlertSnapshot(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	if data == nil {
		return nil
	}
	if data.Generation > 0 && snapshot.AsOf.Before(data.Snapshot.AsOf) {
		return fmt.Errorf("%w: snapshot %s precedes current view %s", ErrAlertDeliveryOldSnapshot, snapshot.AsOf.UTC().Format(time.RFC3339Nano), data.Snapshot.AsOf.UTC().Format(time.RFC3339Nano))
	}
	return rejectAlertSourceWatermarks(data, snapshot)
}

func rejectAlertSourceWatermarks(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	if data == nil {
		return nil
	}
	for _, candidate := range snapshot.Candidates {
		if watermark := data.SourceWatermarks[candidate.Source]; !watermark.IsZero() && candidate.ObservedAt.Before(watermark) {
			return fmt.Errorf("%w: source %s observed_at %s precedes %s", ErrAlertDeliveryOldSnapshot, candidate.Source, candidate.ObservedAt.UTC().Format(time.RFC3339Nano), watermark.UTC().Format(time.RFC3339Nano))
		}
	}
	if snapshot.Coverage.Freshness == rpc.AlertCoverageCurrent {
		for _, source := range snapshot.Coverage.CoveredSources {
			if watermark := data.SourceWatermarks[source]; !watermark.IsZero() && snapshot.Coverage.AsOf.Before(watermark) {
				return fmt.Errorf("%w: source %s coverage %s precedes %s", ErrAlertDeliveryOldSnapshot, source, snapshot.Coverage.AsOf.UTC().Format(time.RFC3339Nano), watermark.UTC().Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

func equalAlertCandidateSnapshot(a, b rpc.AlertCandidateSnapshot) bool {
	if a.SchemaVersion != b.SchemaVersion || a.AuthorityScope != b.AuthorityScope || !a.AsOf.Equal(b.AsOf) || a.CurrentState != b.CurrentState ||
		a.Coverage.State != b.Coverage.State || a.Coverage.Freshness != b.Coverage.Freshness || !a.Coverage.AsOf.Equal(b.Coverage.AsOf) ||
		!slices.Equal(a.Coverage.ExpectedSources, b.Coverage.ExpectedSources) || !slices.Equal(a.Coverage.CoveredSources, b.Coverage.CoveredSources) ||
		!slices.EqualFunc(a.Sources, b.Sources, equalAlertSourceCoverage) ||
		len(a.Candidates) != len(b.Candidates) {
		return false
	}
	for i := range a.Candidates {
		left, right := a.Candidates[i], b.Candidates[i]
		if left.EpisodeKey != right.EpisodeKey || left.OccurrenceKey != right.OccurrenceKey || left.EvidenceFingerprint != right.EvidenceFingerprint ||
			left.Source != right.Source || left.Kind != right.Kind || left.PresentationCode != right.PresentationCode || left.State != right.State || left.Severity != right.Severity ||
			left.EvidenceHealth != right.EvidenceHealth || left.Destination != right.Destination ||
			!left.EvidenceAsOf.Equal(right.EvidenceAsOf) || !left.StateChangedAt.Equal(right.StateChangedAt) || !left.ObservedAt.Equal(right.ObservedAt) {
			return false
		}
	}
	return true
}

func equalAlertSourceCoverage(left, right rpc.AlertSourceCoverage) bool {
	return left.Source == right.Source && left.Status == right.Status && left.Reason == right.Reason &&
		left.EvidenceHealth == right.EvidenceHealth && left.Covered == right.Covered &&
		left.InputAsOf.Equal(right.InputAsOf) && left.ObservedAt.Equal(right.ObservedAt) &&
		left.EvidenceAsOf.Equal(right.EvidenceAsOf) && left.FreshUntil.Equal(right.FreshUntil)
}

func (s *Store) applyAlertSnapshotLocked(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	episodes := make(map[string]int, len(data.Episodes))
	occurrences := make(map[string]int, len(data.Occurrences))
	for i := range data.Episodes {
		episodes[alertDeliveryScopedKey(data.Episodes[i].AuthorityScope, data.Episodes[i].EpisodeKey)] = i
	}
	for i := range data.Occurrences {
		occurrences[alertDeliveryScopedKey(data.Occurrences[i].AuthorityScope, data.Occurrences[i].OccurrenceKey)] = i
	}

	seenEpisodes := make(map[string]struct{}, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		if candidate.State == rpc.AlertEpisodeRecovered && !alertCandidateSourceCurrent(snapshot, candidate, snapshot.AsOf) {
			return fmt.Errorf("%w: recovery source evidence is not current", ErrAlertDeliveryInvalidTransition)
		}
		episodeMapKey := alertDeliveryScopedKey(snapshot.AuthorityScope, candidate.EpisodeKey)
		occurrenceMapKey := alertDeliveryScopedKey(snapshot.AuthorityScope, candidate.OccurrenceKey)
		seenEpisodes[episodeMapKey] = struct{}{}
		episodeIndex, exists := episodes[episodeMapKey]
		if !exists {
			if candidate.State == rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: recovery for unknown episode", ErrAlertDeliveryInvalidTransition)
			}
			if _, reused := occurrences[occurrenceMapKey]; reused {
				return fmt.Errorf("%w: occurrence key reused by another episode", ErrAlertDeliveryInvalidTransition)
			}
			occurrence, err := s.newAlertDeliveryOccurrenceLocked(data, snapshot.AuthorityScope, candidate)
			if err != nil {
				return err
			}
			data.Episodes = append(data.Episodes, alertDeliveryEpisode{
				AuthorityScope: snapshot.AuthorityScope,
				EpisodeKey:     candidate.EpisodeKey, Source: candidate.Source, Kind: candidate.Kind,
				CurrentOccurrenceKey: candidate.OccurrenceKey, State: candidate.State,
				FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt,
			})
			data.Occurrences = append(data.Occurrences, occurrence)
			episodes[episodeMapKey] = len(data.Episodes) - 1
			occurrences[occurrenceMapKey] = len(data.Occurrences) - 1
			continue
		}

		episode := &data.Episodes[episodeIndex]
		if episode.AuthorityScope != snapshot.AuthorityScope || episode.Source != candidate.Source || episode.Kind != candidate.Kind {
			return fmt.Errorf("%w: episode source or kind changed", ErrAlertDeliveryInvalidTransition)
		}
		currentIndex, currentExists := occurrences[alertDeliveryScopedKey(snapshot.AuthorityScope, episode.CurrentOccurrenceKey)]
		if !currentExists {
			return fmt.Errorf("%w: episode current occurrence is missing", ErrInvalidPersistedState)
		}
		current := &data.Occurrences[currentIndex]

		if candidate.OccurrenceKey == episode.CurrentOccurrenceKey {
			if !current.EndedAt.IsZero() && candidate.State != rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: ended occurrence was reused", ErrAlertDeliveryInvalidTransition)
			}
			if err := validateAlertCandidateAdvance(*current, candidate); err != nil {
				return err
			}
			s.applyAlertCandidate(current, candidate)
			if candidate.State == rpc.AlertEpisodeRecovered {
				current.EndedAt = candidate.StateChangedAt
				current.EndReason = AlertDeliveryEndRecovered
			}
			episode.State = candidate.State
			episode.LastSeenAt = candidate.ObservedAt
			continue
		}

		if _, reused := occurrences[occurrenceMapKey]; reused {
			return fmt.Errorf("%w: old occurrence replayed as current", ErrAlertDeliveryInvalidTransition)
		}
		if candidate.State == rpc.AlertEpisodeRecovered {
			return fmt.Errorf("%w: recovery occurrence key did not match", ErrAlertDeliveryInvalidTransition)
		}
		if candidate.ObservedAt.Before(current.ObservedAt) || !candidate.StateChangedAt.After(current.StateChangedAt) {
			return fmt.Errorf("%w: new occurrence regressed lifecycle time", ErrAlertDeliveryInvalidTransition)
		}
		switch {
		case current.State == rpc.AlertEpisodeRecovered || !current.EndedAt.IsZero():
			// A new daemon occurrence after exact recovery is a reopen.
		case candidate.State == rpc.AlertEpisodeEscalated:
			// Rotating the occurrence key while active is allowed only for a
			// producer-qualified escalation.
			current.EndedAt = candidate.StateChangedAt
			current.EndReason = AlertDeliveryEndSuperseded
		default:
			return fmt.Errorf("%w: active opening changed occurrence key without escalation", ErrAlertDeliveryInvalidTransition)
		}
		occurrence, err := s.newAlertDeliveryOccurrenceLocked(data, snapshot.AuthorityScope, candidate)
		if err != nil {
			return err
		}
		data.Occurrences = append(data.Occurrences, occurrence)
		occurrences[occurrenceMapKey] = len(data.Occurrences) - 1
		episode.CurrentOccurrenceKey = candidate.OccurrenceKey
		episode.State = candidate.State
		episode.LastSeenAt = candidate.ObservedAt
	}

	authoritative := make(map[rpc.AlertSource]struct{})
	if snapshot.Coverage.Freshness == rpc.AlertCoverageCurrent {
		for _, row := range snapshot.Sources {
			if !row.Covered || row.EvidenceHealth != rpc.AlertEvidenceCurrent || snapshot.AsOf.After(row.FreshUntil) {
				continue
			}
			authoritative[row.Source] = struct{}{}
			data.SourceWatermarks[row.Source] = snapshot.Coverage.AsOf
		}
	}
	// Absence resolves only the source slices this observation proves current.
	// Partial or stale coverage can never clear an unrelated active episode.
	for i := range data.Episodes {
		episode := &data.Episodes[i]
		if episode.AuthorityScope != snapshot.AuthorityScope {
			continue
		}
		if episode.State == rpc.AlertEpisodeRecovered {
			continue
		}
		if _, covered := authoritative[episode.Source]; !covered {
			continue
		}
		if _, present := seenEpisodes[alertDeliveryScopedKey(snapshot.AuthorityScope, episode.EpisodeKey)]; present {
			continue
		}
		occurrenceIndex, ok := occurrences[alertDeliveryScopedKey(snapshot.AuthorityScope, episode.CurrentOccurrenceKey)]
		if !ok {
			return fmt.Errorf("%w: omitted episode current occurrence is missing", ErrInvalidPersistedState)
		}
		occurrence := &data.Occurrences[occurrenceIndex]
		if !occurrence.EndedAt.IsZero() {
			// A previous-scope or qualified-escalation occurrence is historical
			continue
		}
		occurrence.State = rpc.AlertEpisodeRecovered
		occurrence.EvidenceHealth = rpc.AlertEvidenceCurrent
		occurrence.EvidenceAsOf = snapshot.Coverage.AsOf
		occurrence.StateChangedAt = snapshot.AsOf
		occurrence.ObservedAt = snapshot.AsOf
		occurrence.LastSeenAt = snapshot.AsOf
		occurrence.EndedAt = snapshot.AsOf
		occurrence.EndReason = AlertDeliveryEndOmitted
		episode.State = rpc.AlertEpisodeRecovered
		episode.LastSeenAt = snapshot.AsOf
	}
	if data.SourceWatermarksByScope == nil {
		data.SourceWatermarksByScope = make(map[string]map[rpc.AlertSource]time.Time)
	}
	if _, exists := data.SourceWatermarksByScope[snapshot.AuthorityScope]; !exists && len(data.SourceWatermarksByScope) >= s.alertDeliveryMaxItems {
		return ErrAlertDeliveryOverflow
	}
	data.SourceWatermarksByScope[snapshot.AuthorityScope] = cloneAlertSourceWatermarks(data.SourceWatermarks)
	return nil
}

func (s *Store) newAlertDeliveryOccurrenceLocked(data *alertDeliveryData, authorityScope string, candidate rpc.AlertCandidate) (alertDeliveryOccurrence, error) {
	if s.alertDeliveryMaxItems <= 0 {
		s.alertDeliveryMaxItems = defaultAlertDeliveryMaxItems
	}
	if len(data.Occurrences) >= s.alertDeliveryMaxItems || len(data.Episodes) >= s.alertDeliveryMaxItems {
		return alertDeliveryOccurrence{}, ErrAlertDeliveryOverflow
	}
	if data.AttentionHighWaterSeq == ^uint64(0) {
		return alertDeliveryOccurrence{}, ErrAttentionSequenceExhausted
	}
	data.AttentionHighWaterSeq++
	disposition := s.newAlertDeliveryDispositionLocked(data, authorityScope, candidate)
	return alertDeliveryOccurrence{
		AuthorityScope: authorityScope,
		OccurrenceKey:  candidate.OccurrenceKey, EpisodeKey: candidate.EpisodeKey,
		EvidenceFingerprint: candidate.EvidenceFingerprint, DisplayID: alertDeliveryDisplayID(authorityScope, candidate.OccurrenceKey),
		Source: candidate.Source, Kind: candidate.Kind, PresentationCode: candidate.PresentationCode, State: candidate.State, Severity: candidate.Severity,
		EvidenceHealth: candidate.EvidenceHealth, Destination: candidate.Destination,
		EvidenceAsOf: candidate.EvidenceAsOf, StateChangedAt: candidate.StateChangedAt, ObservedAt: candidate.ObservedAt,
		FirstSeenAt: candidate.ObservedAt, LastSeenAt: candidate.ObservedAt, AttentionSeq: data.AttentionHighWaterSeq,
		Disposition: disposition,
	}, nil
}

func (s *Store) applyAlertCandidate(occurrence *alertDeliveryOccurrence, candidate rpc.AlertCandidate) {
	occurrence.EvidenceFingerprint = candidate.EvidenceFingerprint
	occurrence.State = candidate.State
	occurrence.Severity = candidate.Severity
	occurrence.PresentationCode = candidate.PresentationCode
	occurrence.EvidenceHealth = candidate.EvidenceHealth
	occurrence.Destination = candidate.Destination
	occurrence.EvidenceAsOf = candidate.EvidenceAsOf
	occurrence.StateChangedAt = candidate.StateChangedAt
	occurrence.ObservedAt = candidate.ObservedAt
	occurrence.LastSeenAt = candidate.ObservedAt
}

func validateAlertCandidateAdvance(current alertDeliveryOccurrence, candidate rpc.AlertCandidate) error {
	if candidate.ObservedAt.Before(current.ObservedAt) || candidate.StateChangedAt.Before(current.StateChangedAt) {
		return fmt.Errorf("%w: occurrence timestamps regressed", ErrAlertDeliveryInvalidTransition)
	}
	// A cutover occurrence carries evidence times seeded from the retired
	// pre-baseline producer, which stamped evaluation time where the current
	// contract stamps the condition's own start. The two scales are not
	// comparable, so an earlier evidence time on a row that can never be
	// delivered reads as a re-statement rather than a replay. Observation and
	// lifecycle order still bind here, and every post-baseline occurrence keeps
	// the full rule.
	if candidate.EvidenceAsOf.Before(current.EvidenceAsOf) && current.Disposition != AlertDispositionCutoverExisting {
		return fmt.Errorf("%w: occurrence timestamps regressed", ErrAlertDeliveryInvalidTransition)
	}
	if candidate.State == current.State {
		if !candidate.StateChangedAt.Equal(current.StateChangedAt) {
			return fmt.Errorf("%w: unchanged state changed its transition time", ErrAlertDeliveryInvalidTransition)
		}
		return nil
	}
	if !candidate.StateChangedAt.After(current.StateChangedAt) {
		return fmt.Errorf("%w: lifecycle transition did not advance state_changed_at", ErrAlertDeliveryInvalidTransition)
	}
	allowed := (current.State == rpc.AlertEpisodeOpen && (candidate.State == rpc.AlertEpisodeEscalated || candidate.State == rpc.AlertEpisodeRecovered)) ||
		(current.State == rpc.AlertEpisodeEscalated && candidate.State == rpc.AlertEpisodeRecovered)
	if !allowed {
		return fmt.Errorf("%w: state transition %s to %s", ErrAlertDeliveryInvalidTransition, current.State, candidate.State)
	}
	return nil
}

func (s *Store) newAlertDeliveryDispositionLocked(data *alertDeliveryData, authorityScope string, candidate rpc.AlertCandidate) string {
	if !alertDeliveryBaselineEstablished(data, authorityScope) {
		return AlertDispositionCutoverExisting
	}
	if candidate.State == rpc.AlertEpisodeRecovered || candidate.Severity == rpc.AlertSeverityObserve {
		return AlertDispositionObserveOnly
	}
	if alertModeAllowsSeverity(s.data.AlertSettings.Mode, candidate.Severity) {
		return AlertDispositionEligible
	}
	return AlertDispositionModeSuppressed
}

func alertDeliveryBaselineEstablished(data *alertDeliveryData, authorityScope string) bool {
	if data == nil || data.Baselines == nil {
		return false
	}
	baseline, ok := data.Baselines[authorityScope]
	return ok && !baseline.EstablishedAt.IsZero() && !baseline.SnapshotAsOf.IsZero()
}

func alertSnapshotCanEstablishBaseline(snapshot rpc.AlertCandidateSnapshot) bool {
	return snapshot.Coverage.State == rpc.AlertCoverageComplete && snapshot.Coverage.Freshness == rpc.AlertCoverageCurrent
}

func alertModeAllowsSeverity(mode string, severity rpc.AlertSeverity) bool {
	switch mode {
	case AlertModeActOnly:
		return severity == rpc.AlertSeverityAct || severity == rpc.AlertSeverityUrgent
	case AlertModeWatchAndAct:
		return severity == rpc.AlertSeverityWatch || severity == rpc.AlertSeverityAct || severity == rpc.AlertSeverityUrgent
	default:
		return false
	}
}

func alertCandidateSourceCurrent(snapshot rpc.AlertCandidateSnapshot, candidate rpc.AlertCandidate, now time.Time) bool {
	if candidate.EvidenceHealth != rpc.AlertEvidenceCurrent || snapshot.Coverage.Freshness != rpc.AlertCoverageCurrent {
		return false
	}
	for _, row := range snapshot.Sources {
		if row.Source != candidate.Source {
			continue
		}
		return row.Covered && row.EvidenceHealth == rpc.AlertEvidenceCurrent && !row.FreshUntil.IsZero() && !now.After(row.FreshUntil)
	}
	return false
}

func alertDeliveryOccurrenceDispatchable(data *alertDeliveryData, occurrence *alertDeliveryOccurrence, candidate rpc.AlertCandidate, mode string, now time.Time) bool {
	if data == nil || occurrence == nil || occurrence.Disposition != AlertDispositionEligible ||
		(candidate.State != rpc.AlertEpisodeOpen && candidate.State != rpc.AlertEpisodeEscalated) ||
		!alertModeAllowsSeverity(mode, candidate.Severity) || !alertCandidateSourceCurrent(data.Snapshot, candidate, now) {
		return false
	}
	for _, current := range data.Snapshot.Candidates {
		if current.OccurrenceKey == candidate.OccurrenceKey && current.EpisodeKey == candidate.EpisodeKey &&
			current.EvidenceFingerprint == candidate.EvidenceFingerprint && current.State == candidate.State {
			return true
		}
	}
	return false
}

func alertCandidateFromOccurrence(occurrence *alertDeliveryOccurrence) rpc.AlertCandidate {
	return rpc.AlertCandidate{
		EpisodeKey: occurrence.EpisodeKey, OccurrenceKey: occurrence.OccurrenceKey,
		EvidenceFingerprint: occurrence.EvidenceFingerprint, Source: occurrence.Source, Kind: occurrence.Kind,
		PresentationCode: occurrence.PresentationCode, State: occurrence.State, Severity: occurrence.Severity,
		EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
		EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt, ObservedAt: occurrence.ObservedAt,
	}
}

// AlertDelivery returns one atomic generation of coverage, current state,
// source authority, redacted occurrence history, attention, and delivery
// totals. It contains no producer, target, attempt, or receipt identity.
func (s *Store) AlertDelivery(now time.Time) AlertDeliveryView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alertDeliveryViewLocked(s.data.AlertDelivery, now.UTC())
}

func (s *Store) alertDeliveryViewLocked(data *alertDeliveryData, now time.Time) AlertDeliveryView {
	view := alertDeliveryViewLocked(data, now)
	if s.alertDeliveryVolatile != nil {
		view.DeliveryHealth = *s.alertDeliveryVolatile
		if s.alertDeliveryVolatileGeneration > view.Generation {
			view.Generation = s.alertDeliveryVolatileGeneration
		}
	}
	return view
}

// AlertDeliveriesDue reconstructs active transport-eligible work from durable
// receipt/retry dedupe remains authoritative in BeginAlertDelivery.
func (s *Store) AlertDeliveriesDue(now time.Time) []AlertDeliveryDueWork {
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.alertDeliveryQuarantinedLocked() {
		return nil
	}
	data := s.data.AlertDelivery
	if data == nil || s.alertDeliveryVolatile != nil || data.Health.State == AlertDeliveryHealthOverflow ||
		(data.Health.State == AlertDeliveryHealthUnavailable && !validAlertDeliveryPrerequisiteClass(data.Health.Class)) {
		return nil
	}
	out := make([]AlertDeliveryDueWork, 0)
	for i := range data.Occurrences {
		occurrence := &data.Occurrences[i]
		_, episode, ok := findAlertDeliveryOccurrence(data, occurrence.AuthorityScope, occurrence.OccurrenceKey)
		if !ok || !alertDeliveryOccurrenceCurrent(data, occurrence, episode) {
			continue
		}
		candidate := alertCandidateFromOccurrence(occurrence)
		if rpc.ValidateAlertCandidate(candidate) != nil || !alertDeliveryOccurrenceDispatchable(data, occurrence, candidate, s.data.AlertSettings.Mode, now) {
			continue
		}
		out = append(out, AlertDeliveryDueWork{OccurrenceKey: occurrence.OccurrenceKey, Candidate: candidate, DisplayID: occurrence.DisplayID})
	}
	slices.SortFunc(out, func(a, b AlertDeliveryDueWork) int { return cmp.Compare(a.DisplayID, b.DisplayID) })
	return out
}

// SetAlertDeliveryPrerequisiteHealth persists the current dispatcher's
// allowlisted prerequisite posture. An empty class clears only a prior
// clear overflow, a state-write failure, or interrupted transport evidence.
func (s *Store) SetAlertDeliveryPrerequisiteHealth(class string, now time.Time) error {
	if class != "" && !validAlertDeliveryPrerequisiteClass(class) {
		return errors.New("invalid alert delivery prerequisite health class")
	}
	now = now.UTC()
	if now.IsZero() {
		return errors.New("alert delivery prerequisite health time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	if s.alertDeliveryVolatile != nil {
		return ErrAlertDeliveryUnavailable
	}
	if prior.Health.State == AlertDeliveryHealthOverflow ||
		(prior.Health.State == AlertDeliveryHealthUnavailable && prior.Health.Class == AlertDeliveryHealthClassStateWrite) {
		return ErrAlertDeliveryUnavailable
	}
	if class == "" && !validAlertDeliveryPrerequisiteClass(prior.Health.Class) {
		return nil
	}
	if class != "" && prior.Health.State == AlertDeliveryHealthUnavailable && prior.Health.Class == class {
		return nil
	}

	next := cloneAlertDeliveryData(prior)
	if class == "" {
		next.Health = AlertDeliveryHealth{}
		s.recomputeAlertDeliveryHealthLocked(next, now)
	} else {
		next.Health = AlertDeliveryHealth{
			State: AlertDeliveryHealthUnavailable, Class: class, UpdatedAt: now,
			LastAcceptedAt: prior.Health.LastAcceptedAt,
		}
	}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

func alertDeliveryViewLocked(data *alertDeliveryData, now time.Time) AlertDeliveryView {
	view := AlertDeliveryView{Occurrences: []AlertDeliveryOccurrenceView{}, Attention: AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}}
	if data == nil {
		return view
	}
	view.Initialized = true
	view.Version = data.Version
	view.Generation = data.Generation
	view.AsOf = data.Snapshot.AsOf
	view.CurrentState = data.Snapshot.CurrentState
	view.Coverage = cloneAlertCoverage(data.Snapshot.Coverage)
	view.Sources = cloneAlertSourceCoverageRows(data.Snapshot.Sources)
	view.AuthorityScope = data.Snapshot.AuthorityScope
	view.SourceWatermarks = cloneAlertSourceWatermarks(data.SourceWatermarks)
	for _, occurrence := range data.Occurrences {
		if occurrence.AuthorityScope != data.Snapshot.AuthorityScope && occurrence.EndedAt.IsZero() {
			// Active occurrences in another account/mode remain privately live so
			// projection below is the only public representation while dormant.
			continue
		}
		view.Occurrences = append(view.Occurrences, AlertDeliveryOccurrenceView{
			DisplayID: occurrence.DisplayID, Source: occurrence.Source, Kind: occurrence.Kind, PresentationCode: occurrence.PresentationCode, State: occurrence.State,
			Severity:       occurrence.Severity,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt,
			FirstSeenAt: occurrence.FirstSeenAt, LastSeenAt: occurrence.LastSeenAt,
			EndedAt: occurrence.EndedAt, EndReason: occurrence.EndReason,
			AttentionSeq: occurrence.AttentionSeq, Disposition: occurrence.Disposition,
		})
	}
	for _, previous := range data.PreviousContexts {
		view.Occurrences = append(view.Occurrences, AlertDeliveryOccurrenceView{
			DisplayID: previous.DisplayID, Source: previous.Source, Kind: previous.Kind, PresentationCode: previous.PresentationCode, State: previous.State,
			Severity:       previous.Severity,
			EvidenceHealth: previous.EvidenceHealth, Destination: previous.Destination,
			EvidenceAsOf: previous.EvidenceAsOf, StateChangedAt: previous.StateChangedAt,
			FirstSeenAt: previous.FirstSeenAt, LastSeenAt: previous.LastSeenAt,
			EndedAt: previous.EndedAt, EndReason: previous.EndReason,
			AttentionSeq: previous.OriginalAttention, Disposition: previous.Disposition,
		})
	}
	view.Attention = alertDeliveryAttentionLocked(data)
	view.AttemptTotals = alertDeliveryAttemptTotals(data, now)
	view.DeliveryHealth = data.Health
	ageAlertDeliveryEvidence(&view, now)
	return view
}

func ageAlertDeliveryEvidence(view *AlertDeliveryView, now time.Time) {
	if view == nil {
		return
	}
	expiredSources := make(map[rpc.AlertSource]struct{})
	for i := range view.Sources {
		row := &view.Sources[i]
		if !row.Covered || row.EvidenceHealth != rpc.AlertEvidenceCurrent || row.FreshUntil.IsZero() || !now.After(row.FreshUntil) {
			continue
		}
		row.Status = "stale"
		row.Reason = "freshness_expired"
		row.EvidenceHealth = rpc.AlertEvidenceStale
		expiredSources[row.Source] = struct{}{}
	}
	if len(expiredSources) == 0 {
		return
	}
	for i := range view.Occurrences {
		if _, expired := expiredSources[view.Occurrences[i].Source]; expired && view.Occurrences[i].EvidenceHealth == rpc.AlertEvidenceCurrent {
			view.Occurrences[i].EvidenceHealth = rpc.AlertEvidenceStale
		}
	}
	view.Coverage.Freshness = rpc.AlertCoverageStale
	if view.CurrentState == rpc.AlertSnapshotClear {
		view.CurrentState = rpc.AlertSnapshotUnknown
	}
}

func alertDeliveryAttentionLocked(data *alertDeliveryData) AlertDeliveryAttention {
	if data == nil {
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}
	}
	type entry struct {
		seq uint64
		ref AlertDeliveryAttentionRef
	}
	entries := make([]entry, 0)
	for _, occurrence := range data.Occurrences {
		if occurrence.AttentionSeq > data.AttentionReadThroughSeq && occurrence.AttentionSeq <= data.AttentionHighWaterSeq {
			displayID := occurrence.DisplayID
			if occurrence.AuthorityScope != data.Snapshot.AuthorityScope && occurrence.EndedAt.IsZero() {
				if previous, ok := latestAlertDeliveryPreviousContext(data, occurrence.AuthorityScope, occurrence.DisplayID); ok {
					displayID = previous.DisplayID
				} else {
					// Corrupt states are rejected at open. Keep this defensive path
					continue
				}
			}
			entries = append(entries, entry{seq: occurrence.AttentionSeq, ref: AlertDeliveryAttentionRef{DisplayID: displayID, Source: occurrence.Source, Kind: occurrence.Kind}})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return cmp.Compare(a.seq, b.seq) })
	refs := make([]AlertDeliveryAttentionRef, len(entries))
	for i := range entries {
		refs[i] = entries[i].ref
	}
	return AlertDeliveryAttention{UnreadCount: len(refs), HighWaterSeq: data.AttentionHighWaterSeq, ReadThroughSeq: data.AttentionReadThroughSeq, UnreadRefs: refs}
}

func latestAlertDeliveryPreviousContext(data *alertDeliveryData, authorityScope, priorDisplayID string) (alertDeliveryPreviousContext, bool) {
	if data == nil {
		return alertDeliveryPreviousContext{}, false
	}
	for _, previous := range slices.Backward(data.PreviousContexts) {
		if previous.AuthorityScope == authorityScope && previous.PriorDisplayID == priorDisplayID {
			return previous, true
		}
	}
	return alertDeliveryPreviousContext{}, false
}

// MarkAlertDeliveryAttentionRead durably advances the source-neutral inbox
// cursor only across a complete contiguous set of retained references. It is
// evidence of rendered app state, not human attention or physical delivery.
func (s *Store) MarkAlertDeliveryAttentionRead(throughSeq uint64) (AlertDeliveryAttention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, err
	}
	data := s.data.AlertDelivery
	if data == nil {
		if throughSeq == 0 {
			return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, nil
		}
		return AlertDeliveryAttention{UnreadRefs: []AlertDeliveryAttentionRef{}}, ErrAlertDeliveryAttentionRead
	}
	if throughSeq < data.AttentionReadThroughSeq || throughSeq > data.AttentionHighWaterSeq || !alertDeliveryAttentionCompleteThrough(data, throughSeq) {
		return alertDeliveryAttentionLocked(data), ErrAlertDeliveryAttentionRead
	}
	if throughSeq == data.AttentionReadThroughSeq {
		return alertDeliveryAttentionLocked(data), nil
	}
	prior := s.data.AlertDelivery
	next := cloneAlertDeliveryData(prior)
	next.AttentionReadThroughSeq = throughSeq
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return alertDeliveryAttentionLocked(prior), err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(time.Now().UTC())
		return alertDeliveryAttentionLocked(prior), err
	}
	s.clearAlertDeliveryVolatileLocked()
	return alertDeliveryAttentionLocked(next), nil
}

func alertDeliveryAttentionCompleteThrough(data *alertDeliveryData, throughSeq uint64) bool {
	seen := make(map[uint64]struct{})
	for _, occurrence := range data.Occurrences {
		seq := occurrence.AttentionSeq
		if seq <= data.AttentionReadThroughSeq || seq > throughSeq {
			continue
		}
		if _, duplicate := seen[seq]; duplicate {
			return false
		}
		seen[seq] = struct{}{}
	}
	return uint64(len(seen)) == throughSeq-data.AttentionReadThroughSeq
}

// AlertDeliveryTargetRef hides device/subscription identities before they
// enter delivery state. A retired subscription must receive a new target ref.
func AlertDeliveryTargetRef(deviceID, subscriptionID string) string {
	return alertDeliveryHash("target", strings.TrimSpace(deviceID), strings.TrimSpace(subscriptionID))
}

// BeginAlertDelivery durably reserves one private occurrence+target attempt
// before a transport caller is allowed to send. DisplayID is never accepted as
// authority and receipt identity is constructed internally.
func (s *Store) BeginAlertDelivery(occurrenceKey, targetRef string, now time.Time) (AlertDeliveryReservation, bool, error) {
	if !validAlertPrivateKey(occurrenceKey, "alert-occurrence-v1:") {
		return AlertDeliveryReservation{}, false, errors.New("invalid private alert occurrence key")
	}
	if !validAlertHash(targetRef) {
		return AlertDeliveryReservation{}, false, errors.New("invalid alert delivery target ref")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery reservation time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnknownOccurrence
	}
	if prior.Health.State == AlertDeliveryHealthOverflow {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryOverflow
	}
	if prior.Health.State == AlertDeliveryHealthUnavailable {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	}
	if s.alertDeliveryVolatile != nil {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	}
	currentScope := prior.Snapshot.AuthorityScope
	occurrence, episode, ok := findAlertDeliveryOccurrence(prior, currentScope, occurrenceKey)
	if !ok {
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnknownOccurrence
	}
	if !alertDeliveryOccurrenceCurrent(prior, occurrence, episode) ||
		!alertDeliveryOccurrenceDispatchable(prior, occurrence, alertCandidateFromOccurrence(occurrence), s.data.AlertSettings.Mode, now) {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	if _, retired := prior.RetiredTargets[targetRef]; retired {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	receiptKey := alertDeliveryReceiptKey(currentScope, occurrenceKey, targetRef)
	if alertDeliveryHasReceipt(prior, receiptKey) {
		return AlertDeliveryReservation{DisplayID: occurrence.DisplayID}, false, nil
	}
	// Reserve-before-send must also reserve the possibility of an accepted
	// receipt. Letting transport run at receipt capacity would discover the
	// overflow only after an external acceptance that cannot be persisted.
	if len(prior.Receipts) >= s.alertDeliveryMaxItems {
		return AlertDeliveryReservation{}, false, s.setAlertDeliveryOverflowLocked(prior, now)
	}
	latest, hasLatest := latestAlertDeliveryAttempt(prior, receiptKey)
	if hasLatest {
		if latest.Class == AlertDeliveryAttemptReserved || latest.Class == AlertDeliveryAttemptConfirmed || latest.Class == AlertDeliveryAttemptAccepted || latest.Class == AlertDeliveryAttemptRejected ||
			latest.Class == AlertDeliveryAttemptRetired || latest.Class == AlertDeliveryAttemptInactive || latest.Class == AlertDeliveryAttemptExhausted ||
			latest.Class == AlertDeliveryAttemptModeSuppressed ||
			(latest.Class == AlertDeliveryAttemptInterrupted && latest.RetryAt.IsZero()) {
			return alertDeliveryReservationView(latest, occurrence.DisplayID), false, nil
		}
		if !latest.RetryAt.IsZero() && now.Before(latest.RetryAt) {
			return alertDeliveryReservationView(latest, occurrence.DisplayID), false, nil
		}
	}
	if len(prior.Attempts) >= s.alertDeliveryMaxItems {
		return AlertDeliveryReservation{}, false, s.setAlertDeliveryOverflowLocked(prior, now)
	}
	next := cloneAlertDeliveryData(prior)
	attemptNumber := 1
	if hasLatest {
		attemptNumber = latest.AttemptNumber + 1
	}
	attempt := alertDeliveryAttempt{
		AuthorityScope: currentScope,
		ID:             alertDeliveryAttemptID(receiptKey, attemptNumber, now), OccurrenceKey: occurrenceKey,
		TargetRef: targetRef, ReceiptKey: receiptKey, AttemptNumber: attemptNumber,
		ReservedAt: now, Class: AlertDeliveryAttemptReserved,
	}
	next.Attempts = append(next.Attempts, attempt)
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryReservation{}, false, err
	}
	// A successful durable reservation is owned by this process until Confirm
	// and Complete finish or a persistence failure explicitly releases it.
	// In-process orphan recovery must not rewrite a genuinely active caller.
	s.alertDeliveryInFlight[attempt.ID] = true
	s.clearAlertDeliveryVolatileLocked()
	return alertDeliveryReservationView(attempt, occurrence.DisplayID), true, nil
}

func alertDeliveryReservationView(attempt alertDeliveryAttempt, displayID string) AlertDeliveryReservation {
	return AlertDeliveryReservation{AttemptID: attempt.ID, DisplayID: displayID, AttemptNumber: attempt.AttemptNumber, ReservedAt: attempt.ReservedAt, RetryAt: attempt.RetryAt}
}

// ConfirmAlertTransport is the last durable-authority check immediately before
// an external Sender.Send call. Reservation alone never grants transport: a
func (s *Store) ConfirmAlertTransport(attemptID string, now time.Time) (AlertDeliveryReservation, bool, error) {
	if !validAlertAttemptID(attemptID) {
		return AlertDeliveryReservation{}, false, errors.New("invalid alert delivery attempt id")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery confirmation time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryReservation{}, false, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery attempt not found")
	}
	attemptIndex := -1
	for i := range prior.Attempts {
		if prior.Attempts[i].ID == attemptID {
			attemptIndex = i
			break
		}
	}
	if attemptIndex < 0 {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery attempt not found")
	}
	stored := prior.Attempts[attemptIndex]
	if now.Before(stored.ReservedAt) {
		return AlertDeliveryReservation{}, false, errors.New("alert delivery confirmation precedes reservation")
	}
	occurrence, episode, found := findAlertDeliveryOccurrence(prior, stored.AuthorityScope, stored.OccurrenceKey)
	displayID := ""
	if found {
		displayID = occurrence.DisplayID
	}
	if stored.Class != AlertDeliveryAttemptReserved || !s.alertDeliveryInFlight[attemptID] {
		return alertDeliveryReservationView(stored, displayID), false, nil
	}
	class := ""
	retiredAt := time.Time{}
	switch {
	case s.alertDeliveryVolatile != nil:
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryReservation{}, false, ErrAlertDeliveryUnavailable
	case !found || !alertDeliveryOccurrenceCurrent(prior, occurrence, episode):
		class = AlertDeliveryAttemptInactive
	case !prior.RetiredTargets[stored.TargetRef].IsZero():
		class = AlertDeliveryAttemptRetired
		retiredAt = prior.RetiredTargets[stored.TargetRef]
	case !alertModeAllowsSeverity(s.data.AlertSettings.Mode, occurrence.Severity):
		class = AlertDeliveryAttemptModeSuppressed
	case !alertDeliveryOccurrenceDispatchable(prior, occurrence, alertCandidateFromOccurrence(occurrence), s.data.AlertSettings.Mode, now):
		class = AlertDeliveryAttemptRetry
	}
	if class != "" {
		next := cloneAlertDeliveryData(prior)
		attempt := &next.Attempts[attemptIndex]
		attempt.Class = class
		attempt.CompletedAt = now
		attempt.RetiredAt = retiredAt
		if class == AlertDeliveryAttemptRetry {
			if delay, retry := alertDeliveryRetryDelay(attempt.AttemptNumber); retry {
				attempt.RetryAt = now.Add(delay)
			} else {
				attempt.Class = AlertDeliveryAttemptExhausted
			}
		}
		if class == AlertDeliveryAttemptInactive {
			attempt.Disposition = AlertDeliveryCompletionInactive
		} else if class == AlertDeliveryAttemptRetired {
			attempt.Disposition = AlertDeliveryCompletionRetired
		} else if class == AlertDeliveryAttemptModeSuppressed {
			if current, _, ok := findAlertDeliveryOccurrence(next, stored.AuthorityScope, stored.OccurrenceKey); ok {
				current.Disposition = AlertDispositionModeSuppressed
			}
		}
		s.recomputeAlertDeliveryHealthLocked(next, now)
		if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
			delete(s.alertDeliveryInFlight, attemptID)
			return AlertDeliveryReservation{}, false, err
		}
		s.data.AlertDelivery = next
		if err := s.save(); err != nil {
			s.data.AlertDelivery = prior
			delete(s.alertDeliveryInFlight, attemptID)
			s.noteAlertDeliverySaveFailureLocked(now)
			return AlertDeliveryReservation{}, false, err
		}
		delete(s.alertDeliveryInFlight, attemptID)
		s.clearAlertDeliveryVolatileLocked()
		return alertDeliveryReservationView(*attempt, displayID), false, nil
	}
	// Persist the narrow confirmed-pending-outcome window. A crash or a
	// caller must use the returned DisplayID as the transport collapse/tag key.
	next := cloneAlertDeliveryData(prior)
	attempt := &next.Attempts[attemptIndex]
	attempt.Class = AlertDeliveryAttemptConfirmed
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryReservation{}, false, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		delete(s.alertDeliveryInFlight, attemptID)
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryReservation{}, false, err
	}
	s.clearAlertDeliveryVolatileLocked()
	confirmed := alertDeliveryReservationView(*attempt, displayID)
	confirmed.Candidate = alertCandidateFromOccurrence(occurrence)
	return confirmed, true, nil
}

// CompleteAlertDelivery completes only a persisted reservation. It rechecks
// occurrence and target authority under the store lock before recording a
// receipt. The caller supplies neither occurrence, target, receipt key nor
func (s *Store) CompleteAlertDelivery(attemptID string, completion AlertDeliveryCompletion, now time.Time) (AlertDeliveryCompletionOutcome, error) {
	if !validAlertAttemptID(attemptID) {
		return AlertDeliveryCompletionOutcome{}, errors.New("invalid alert delivery attempt id")
	}
	if completion != AlertDeliveryCompletionAccepted && completion != AlertDeliveryCompletionRetryable && completion != AlertDeliveryCompletionRejected {
		return AlertDeliveryCompletionOutcome{}, errors.New("invalid alert delivery completion")
	}
	now = now.UTC()
	if now.IsZero() {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery completion time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return AlertDeliveryCompletionOutcome{}, err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery attempt not found")
	}
	attemptIndex := -1
	for i := range prior.Attempts {
		if prior.Attempts[i].ID == attemptID {
			attemptIndex = i
			break
		}
	}
	if attemptIndex < 0 {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery attempt not found")
	}
	stored := prior.Attempts[attemptIndex]
	if now.Before(stored.ReservedAt) {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery completion precedes reservation")
	}
	if stored.Class != AlertDeliveryAttemptConfirmed {
		return AlertDeliveryCompletionOutcome{Disposition: AlertDeliveryCompletionAlreadyComplete, Class: stored.Class, RetryAt: stored.RetryAt}, nil
	}
	if !s.alertDeliveryInFlight[attemptID] {
		return AlertDeliveryCompletionOutcome{}, errors.New("alert delivery transport was not confirmed")
	}
	next := cloneAlertDeliveryData(prior)
	attempt := &next.Attempts[attemptIndex]
	occurrence, episode, active := findAlertDeliveryOccurrence(next, attempt.AuthorityScope, attempt.OccurrenceKey)
	disposition := AlertDeliveryCompletionApplied
	if retiredAt := next.RetiredTargets[attempt.TargetRef]; !retiredAt.IsZero() {
		disposition = AlertDeliveryCompletionRetired
		attempt.RetiredAt = retiredAt
	} else if !active || !alertDeliveryOccurrenceCurrent(next, occurrence, episode) {
		disposition = AlertDeliveryCompletionInactive
	}
	attempt.Disposition = disposition
	attempt.CompletedAt = now
	switch completion {
	case AlertDeliveryCompletionAccepted:
		if !alertDeliveryHasAnyReceipt(next, attempt.ReceiptKey) {
			if len(next.Receipts) >= s.alertDeliveryMaxItems {
				err := s.setAlertDeliveryOverflowLocked(prior, now)
				delete(s.alertDeliveryInFlight, attemptID)
				return AlertDeliveryCompletionOutcome{}, err
			}
			next.Receipts = append(next.Receipts, alertDeliveryReceipt{
				AuthorityScope: attempt.AuthorityScope,
				OccurrenceKey:  attempt.OccurrenceKey, TargetRef: attempt.TargetRef,
				ReceiptKey: attempt.ReceiptKey, AcceptedAt: now, RetiredAt: attempt.RetiredAt,
			})
		}
		attempt.Class = AlertDeliveryAttemptAccepted
	case AlertDeliveryCompletionRetryable:
		if disposition == AlertDeliveryCompletionApplied {
			if delay, ok := alertDeliveryRetryDelay(attempt.AttemptNumber); ok {
				attempt.Class = AlertDeliveryAttemptRetry
				attempt.RetryAt = now.Add(delay)
			} else {
				attempt.Class = AlertDeliveryAttemptExhausted
			}
		} else {
			attempt.Class = AlertDeliveryAttemptRetry
		}
	case AlertDeliveryCompletionRejected:
		attempt.Class = AlertDeliveryAttemptRejected
	}
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		delete(s.alertDeliveryInFlight, attemptID)
		return AlertDeliveryCompletionOutcome{}, err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		delete(s.alertDeliveryInFlight, attemptID)
		s.noteAlertDeliverySaveFailureLocked(now)
		return AlertDeliveryCompletionOutcome{}, err
	}
	delete(s.alertDeliveryInFlight, attemptID)
	s.clearAlertDeliveryVolatileLocked()
	return AlertDeliveryCompletionOutcome{Disposition: disposition, Class: attempt.Class, RetryAt: attempt.RetryAt}, nil
}

func alertDeliveryRetryDelay(attemptNumber int) (time.Duration, bool) {
	delays := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	if attemptNumber < 1 || attemptNumber > len(delays) {
		return 0, false
	}
	return delays[attemptNumber-1], true
}

// RetireAlertDeliveryTarget permanently closes delivery evidence for one
// private target ref. A re-paired/re-subscribed endpoint must use a new ref.
func (s *Store) RetireAlertDeliveryTarget(targetRef string, at time.Time) error {
	if !validAlertHash(targetRef) {
		return errors.New("invalid alert delivery target ref")
	}
	at = at.UTC()
	if at.IsZero() {
		return errors.New("alert delivery target retirement time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	release, changed, err := s.retireAlertDeliveryTargetsLocked(map[string]bool{targetRef: true}, at)
	if errors.Is(err, ErrAlertDeliveryOverflow) {
		return s.setAlertDeliveryOverflowLocked(prior, at)
	}
	if err != nil || !changed {
		return err
	}
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(at)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	for _, attemptID := range release {
		delete(s.alertDeliveryInFlight, attemptID)
	}
	return nil
}

// retireAlertDeliveryTargetsLocked stages one atomic ledger retirement without
// saving. Callers already hold s.mu and must either persist every surrounding
func (s *Store) retireAlertDeliveryTargetsLocked(targets map[string]bool, at time.Time) ([]string, bool, error) {
	s.initAlertDeliveryRuntime()
	if s.alertDeliveryQuarantinedLocked() {
		// Device/subscription lifecycle remains available to legacy Canary.
		return nil, false, nil
	}
	prior := s.data.AlertDelivery
	if prior == nil || len(targets) == 0 {
		return nil, false, nil
	}
	if prior.Health.State == AlertDeliveryHealthOverflow {
		return nil, false, ErrAlertDeliveryOverflow
	}
	newTargets := 0
	for target := range targets {
		if !validAlertHash(target) {
			return nil, false, errors.New("invalid alert delivery target ref")
		}
		if prior.RetiredTargets[target].IsZero() {
			newTargets++
		}
	}
	if newTargets == 0 {
		return nil, false, nil
	}
	if len(prior.RetiredTargets)+newTargets > s.alertDeliveryMaxItems {
		return nil, false, ErrAlertDeliveryOverflow
	}
	for _, attempt := range prior.Attempts {
		if targets[attempt.TargetRef] && attempt.RetiredAt.IsZero() &&
			(attempt.Class == AlertDeliveryAttemptReserved || attempt.Class == AlertDeliveryAttemptRetry) && at.Before(attempt.ReservedAt) {
			return nil, false, errors.New("alert delivery retirement precedes reservation")
		}
	}

	next := cloneAlertDeliveryData(prior)
	for target := range targets {
		if next.RetiredTargets[target].IsZero() {
			next.RetiredTargets[target] = at
		}
	}
	release := make([]string, 0)
	for i := range next.Attempts {
		attempt := &next.Attempts[i]
		if !targets[attempt.TargetRef] || !attempt.RetiredAt.IsZero() {
			continue
		}
		attempt.RetiredAt = at
		attempt.RetryAt = time.Time{}
		switch attempt.Class {
		case AlertDeliveryAttemptConfirmed:
			// Sender.Send may already be running. Keep the confirmed attempt
			// completable so known transport truth is never replaced by inference.
			attempt.Disposition = AlertDeliveryCompletionRetired
		case AlertDeliveryAttemptInterrupted:
			// An unknown confirmed outcome remains operationally relevant even
			attempt.Disposition = AlertDeliveryCompletionRetired
			release = append(release, attempt.ID)
		case AlertDeliveryAttemptReserved, AlertDeliveryAttemptRetry:
			attempt.Class = AlertDeliveryAttemptRetired
			attempt.CompletedAt = at
			attempt.Disposition = AlertDeliveryCompletionRetired
			release = append(release, attempt.ID)
		}
	}
	for i := range next.Receipts {
		if targets[next.Receipts[i].TargetRef] && next.Receipts[i].RetiredAt.IsZero() {
			next.Receipts[i].RetiredAt = at
		}
	}
	s.recomputeAlertDeliveryHealthLocked(next, at)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return nil, false, err
	}
	s.data.AlertDelivery = next
	return release, true, nil
}

func (s *Store) finishAlertDeliveryRetirementLocked(release []string, changed bool) {
	if !changed {
		return
	}
	s.clearAlertDeliveryVolatileLocked()
	for _, attemptID := range release {
		delete(s.alertDeliveryInFlight, attemptID)
	}
}

// RecoverAlertDeliveries converts unowned reserve-before-send records into
// definite no-send retries. Only confirmed-pending-outcome records become
// after Confirm or Complete persistence failures release their reservation.
// Genuinely owned work is never rewritten under an active dispatcher.
func (s *Store) RecoverAlertDeliveries(now time.Time) error {
	now = now.UTC()
	if now.IsZero() {
		return errors.New("alert delivery recovery time required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initAlertDeliveryRuntime()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	next := cloneAlertDeliveryData(prior)
	changed := false
	for i := range next.Attempts {
		attempt := &next.Attempts[i]
		if (attempt.Class != AlertDeliveryAttemptReserved && attempt.Class != AlertDeliveryAttemptConfirmed) || !attempt.CompletedAt.IsZero() {
			continue
		}
		if s.alertDeliveryInFlight[attempt.ID] {
			continue
		}
		if now.Before(attempt.ReservedAt) {
			return errors.New("alert delivery recovery precedes reservation")
		}
		occurrence, episode, active := findAlertDeliveryOccurrence(next, attempt.AuthorityScope, attempt.OccurrenceKey)
		switch {
		case !active || !alertDeliveryOccurrenceCurrent(next, occurrence, episode):
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
			} else {
				attempt.Class = AlertDeliveryAttemptInactive
			}
			attempt.Disposition = AlertDeliveryCompletionInactive
		case !next.RetiredTargets[attempt.TargetRef].IsZero():
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
			} else {
				attempt.Class = AlertDeliveryAttemptRetired
			}
			attempt.RetiredAt = next.RetiredTargets[attempt.TargetRef]
			attempt.Disposition = AlertDeliveryCompletionRetired
		default:
			delay, retry := alertDeliveryRetryDelay(attempt.AttemptNumber)
			if attempt.Class == AlertDeliveryAttemptConfirmed {
				attempt.Class = AlertDeliveryAttemptInterrupted
				if retry {
					attempt.RetryAt = now.Add(delay)
				}
			} else if retry {
				attempt.Class = AlertDeliveryAttemptRetry
				attempt.RetryAt = now.Add(delay)
			} else {
				attempt.Class = AlertDeliveryAttemptExhausted
			}
		}
		attempt.CompletedAt = now
		changed = true
	}
	probeVolatileRecovery := s.alertDeliveryVolatile != nil
	if !changed && !probeVolatileRecovery {
		return nil
	}
	if changed {
		s.recomputeAlertDeliveryHealthLocked(next, now)
	}
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

// CompactAlertDelivery removes read, ended evidence older than the retention
// retained attempts. Unread occurrences are never compacted.
func (s *Store) CompactAlertDelivery(now time.Time) error {
	now = now.UTC()
	cutoff := now.Add(-alertDeliveryRetention)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.alertDeliveryQuarantineGuardLocked(); err != nil {
		return err
	}
	prior := s.data.AlertDelivery
	if prior == nil {
		return nil
	}
	next := cloneAlertDeliveryData(prior)
	removedOccurrences := make(map[string]struct{})
	next.Occurrences = slices.DeleteFunc(next.Occurrences, func(occurrence alertDeliveryOccurrence) bool {
		read := occurrence.AttentionSeq == 0 || occurrence.AttentionSeq <= next.AttentionReadThroughSeq
		remove := read && !occurrence.EndedAt.IsZero() && occurrence.EndedAt.Before(cutoff)
		if remove {
			removedOccurrences[alertDeliveryScopedKey(occurrence.AuthorityScope, occurrence.OccurrenceKey)] = struct{}{}
		}
		return remove
	})
	if len(removedOccurrences) > 0 {
		next.Attempts = slices.DeleteFunc(next.Attempts, func(attempt alertDeliveryAttempt) bool {
			_, remove := removedOccurrences[alertDeliveryScopedKey(attempt.AuthorityScope, attempt.OccurrenceKey)]
			return remove
		})
		next.Receipts = slices.DeleteFunc(next.Receipts, func(receipt alertDeliveryReceipt) bool {
			_, remove := removedOccurrences[alertDeliveryScopedKey(receipt.AuthorityScope, receipt.OccurrenceKey)]
			return remove
		})
	}
	previousBefore := len(next.PreviousContexts)
	next.PreviousContexts = slices.DeleteFunc(next.PreviousContexts, func(previous alertDeliveryPreviousContext) bool {
		read := previous.OriginalAttention == 0 || previous.OriginalAttention <= next.AttentionReadThroughSeq
		return read && previous.EndedAt.Before(cutoff)
	})
	activeTargets := s.activeAlertDeliveryTargetsLocked()
	retainedTargets := make(map[string]bool, len(next.Attempts)+len(next.Receipts))
	for _, attempt := range next.Attempts {
		retainedTargets[attempt.TargetRef] = true
	}
	for _, receipt := range next.Receipts {
		retainedTargets[receipt.TargetRef] = true
	}
	removedTargets := 0
	for target, retiredAt := range next.RetiredTargets {
		if retiredAt.Before(cutoff) && !activeTargets[target] && !retainedTargets[target] {
			delete(next.RetiredTargets, target)
			removedTargets++
		}
	}
	remainingOccurrences := make(map[string]struct{}, len(next.Occurrences))
	for _, occurrence := range next.Occurrences {
		remainingOccurrences[alertDeliveryScopedKey(occurrence.AuthorityScope, occurrence.OccurrenceKey)] = struct{}{}
	}
	next.Episodes = slices.DeleteFunc(next.Episodes, func(episode alertDeliveryEpisode) bool {
		_, remains := remainingOccurrences[alertDeliveryScopedKey(episode.AuthorityScope, episode.CurrentOccurrenceKey)]
		return !remains && episode.State == rpc.AlertEpisodeRecovered
	})
	retainedScopes := map[string]bool{next.Snapshot.AuthorityScope: true}
	for _, episode := range next.Episodes {
		retainedScopes[episode.AuthorityScope] = true
	}
	removedScopes := 0
	for scope, watermarks := range next.SourceWatermarksByScope {
		if retainedScopes[scope] {
			continue
		}
		old := len(watermarks) == 0
		if !old {
			old = true
			for _, watermark := range watermarks {
				if !watermark.Before(cutoff) {
					old = false
					break
				}
			}
		}
		if old {
			delete(next.SourceWatermarksByScope, scope)
			removedScopes++
		}
	}
	removedBaselines := 0
	for scope, baseline := range next.Baselines {
		if retainedScopes[scope] || !baseline.EstablishedAt.Before(cutoff) {
			continue
		}
		delete(next.Baselines, scope)
		removedBaselines++
	}
	recoveredCapacity := prior.Health.State == AlertDeliveryHealthOverflow && alertDeliveryBelowCapacity(next, s.alertDeliveryMaxItems)
	if len(removedOccurrences) == 0 && previousBefore == len(next.PreviousContexts) && len(next.Episodes) == len(prior.Episodes) && removedTargets == 0 && removedScopes == 0 && removedBaselines == 0 && !recoveredCapacity {
		return nil
	}
	if recoveredCapacity {
		// Overflow is fail-loud but not a permanent human ritual. Only the
		// retention compactor may clear it, and only after every capped ledger
		next.Health = AlertDeliveryHealth{}
	}
	s.recomputeAlertDeliveryHealthLocked(next, now)
	if err := s.bumpAlertDeliveryGenerationLocked(next); err != nil {
		return err
	}
	s.data.AlertDelivery = next
	if err := s.save(); err != nil {
		s.data.AlertDelivery = prior
		s.noteAlertDeliverySaveFailureLocked(now)
		return err
	}
	s.clearAlertDeliveryVolatileLocked()
	return nil
}

func (s *Store) activeAlertDeliveryTargetsLocked() map[string]bool {
	activeDevices := make(map[string]bool, len(s.data.Devices))
	for _, device := range s.data.Devices {
		if device.RevokedAt.IsZero() {
			activeDevices[device.ID] = true
		}
	}
	targets := make(map[string]bool, len(s.data.PushSubscriptions))
	for _, subscription := range s.data.PushSubscriptions {
		if activeDevices[subscription.DeviceID] {
			targets[AlertDeliveryTargetRef(subscription.DeviceID, subscription.ID)] = true
		}
	}
	return targets
}

func alertDeliveryBelowCapacity(data *alertDeliveryData, maximum int) bool {
	return data != nil && maximum > 0 && data.Snapshot.Coverage.State != rpc.AlertCoverageUnavailable && len(data.Episodes) < maximum && len(data.Occurrences) < maximum && len(data.PreviousContexts) < maximum && len(data.Attempts) < maximum && len(data.Receipts) < maximum && len(data.RetiredTargets) < maximum && len(data.Baselines) <= maximum
}

func alertDeliveryAttemptTotals(data *alertDeliveryData, now time.Time) AlertDeliveryAttemptTotals {
	_ = now
	var totals AlertDeliveryAttemptTotals
	latest := make(map[string]alertDeliveryAttempt)
	for _, attempt := range data.Attempts {
		totals.Attempts++
		switch attempt.Class {
		case AlertDeliveryAttemptAccepted:
			totals.Accepted++
		case AlertDeliveryAttemptConfirmed:
			totals.Confirmed++
		case AlertDeliveryAttemptRejected:
			totals.Rejected++
		case AlertDeliveryAttemptInterrupted:
			totals.Interrupted++
		case AlertDeliveryAttemptRetired:
			totals.TargetRetired++
		case AlertDeliveryAttemptInactive:
			totals.Inactive++
		case AlertDeliveryAttemptExhausted:
			totals.RetryExhausted++
		case AlertDeliveryAttemptModeSuppressed:
			totals.ModeSuppressed++
		}
		latest[attempt.ReceiptKey] = attempt
	}
	for receiptKey, attempt := range latest {
		if alertDeliveryHasReceipt(data, receiptKey) || attempt.RetryAt.IsZero() {
			continue
		}
		occurrence, episode, ok := findAlertDeliveryOccurrence(data, attempt.AuthorityScope, attempt.OccurrenceKey)
		if ok && alertDeliveryOccurrenceCurrent(data, occurrence, episode) {
			totals.RetryPending++
		}
	}
	return totals
}

func findAlertDeliveryOccurrence(data *alertDeliveryData, authorityScope, occurrenceKey string) (*alertDeliveryOccurrence, *alertDeliveryEpisode, bool) {
	if data == nil {
		return nil, nil, false
	}
	occurrenceIndex := -1
	for i := range data.Occurrences {
		if data.Occurrences[i].AuthorityScope == authorityScope && data.Occurrences[i].OccurrenceKey == occurrenceKey {
			occurrenceIndex = i
			break
		}
	}
	if occurrenceIndex < 0 {
		return nil, nil, false
	}
	episodeIndex := -1
	for i := range data.Episodes {
		if data.Episodes[i].AuthorityScope == authorityScope && data.Episodes[i].EpisodeKey == data.Occurrences[occurrenceIndex].EpisodeKey {
			episodeIndex = i
			break
		}
	}
	if episodeIndex < 0 {
		return &data.Occurrences[occurrenceIndex], nil, false
	}
	return &data.Occurrences[occurrenceIndex], &data.Episodes[episodeIndex], true
}

func alertDeliveryOccurrenceActive(occurrence *alertDeliveryOccurrence, episode *alertDeliveryEpisode) bool {
	return occurrence != nil && episode != nil && occurrence.AuthorityScope == episode.AuthorityScope && occurrence.OccurrenceKey == episode.CurrentOccurrenceKey &&
		episode.State != rpc.AlertEpisodeRecovered && occurrence.EndedAt.IsZero()
}

func alertDeliveryOccurrenceCurrent(data *alertDeliveryData, occurrence *alertDeliveryOccurrence, episode *alertDeliveryEpisode) bool {
	return data != nil && occurrence != nil && episode != nil && occurrence.AuthorityScope == data.Snapshot.AuthorityScope &&
		episode.AuthorityScope == data.Snapshot.AuthorityScope && alertDeliveryOccurrenceActive(occurrence, episode)
}

func alertDeliveryHasReceipt(data *alertDeliveryData, receiptKey string) bool {
	for _, receipt := range data.Receipts {
		if receipt.ReceiptKey == receiptKey && receipt.RetiredAt.IsZero() {
			return true
		}
	}
	return false
}

func alertDeliveryHasAnyReceipt(data *alertDeliveryData, receiptKey string) bool {
	for _, receipt := range data.Receipts {
		if receipt.ReceiptKey == receiptKey {
			return true
		}
	}
	return false
}

// recomputeAlertDeliveryHealthLocked derives current transport posture across
// fail-loud states remain sticky until their typed recovery path succeeds.
func (s *Store) recomputeAlertDeliveryHealthLocked(data *alertDeliveryData, now time.Time) {
	if data == nil {
		return
	}
	lastAccepted := data.Health.LastAcceptedAt
	for _, receipt := range data.Receipts {
		if receipt.AuthorityScope == data.Snapshot.AuthorityScope && receipt.AcceptedAt.After(lastAccepted) {
			lastAccepted = receipt.AcceptedAt
		}
	}
	if data.Health.State == AlertDeliveryHealthOverflow || data.Health.State == AlertDeliveryHealthUnavailable {
		data.Health.LastAcceptedAt = lastAccepted
		return
	}
	interrupted := false
	latest := make(map[string]alertDeliveryAttempt)
	for _, attempt := range data.Attempts {
		// Confirmed transport with an unknown outcome remains operationally
		// relevant even if recovery, retirement, or an authority-scope change
		if attempt.Class == AlertDeliveryAttemptInterrupted {
			interrupted = true
			continue
		}
		if attempt.AuthorityScope != data.Snapshot.AuthorityScope {
			continue
		}
		// Retired targets are no longer part of current transport posture.
		// target map also covers callback races while retirement is persisted.
		if !attempt.RetiredAt.IsZero() || !data.RetiredTargets[attempt.TargetRef].IsZero() {
			continue
		}
		occurrence, episode, active := findAlertDeliveryOccurrence(data, attempt.AuthorityScope, attempt.OccurrenceKey)
		if !active || !alertDeliveryOccurrenceCurrent(data, occurrence, episode) {
			// A completed retry/rejection/exhaustion for an inactive occurrence is
			// retained as history but can no longer describe current delivery.
			continue
		}
		latest[attempt.ReceiptKey] = attempt
	}
	class := ""
	if interrupted {
		class = AlertDeliveryHealthClassInterrupted
	} else {
		for _, attempt := range latest {
			switch attempt.Class {
			case AlertDeliveryAttemptExhausted:
				class = AlertDeliveryAttemptExhausted
			case AlertDeliveryAttemptRejected:
				if class == "" || class == AlertDeliveryHealthClassRetry {
					class = AlertDeliveryHealthClassRejected
				}
			case AlertDeliveryAttemptRetry:
				if class == "" {
					class = AlertDeliveryHealthClassRetry
				}
			}
		}
	}
	if class == "" && !data.ObservationRejectedAt.IsZero() {
		class = AlertDeliveryHealthClassObservation
	}
	if class != "" {
		data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthDegraded, Class: class, UpdatedAt: now, LastAcceptedAt: lastAccepted}
		return
	}
	data.Health = AlertDeliveryHealth{State: AlertDeliveryHealthHealthy, UpdatedAt: now, LastAcceptedAt: lastAccepted}
}

func latestAlertDeliveryAttempt(data *alertDeliveryData, receiptKey string) (alertDeliveryAttempt, bool) {
	for _, attempt := range slices.Backward(data.Attempts) {
		if attempt.ReceiptKey == receiptKey {
			return attempt, true
		}
	}
	return alertDeliveryAttempt{}, false
}

func alertDeliveryDisplayID(authorityScope, occurrenceKey string) string {
	sum := sha256.Sum256([]byte("alert-display-v2\x00" + authorityScope + "\x00" + occurrenceKey))
	return fmt.Sprintf("alert-%x", sum[:8])
}

func alertPreviousContextDisplayID(authorityScope, priorDisplayID string, endedAt time.Time, archiveSeq uint64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "alert-previous-context-v1\x00%s\x00%s\x00%s\x00%d", authorityScope, priorDisplayID, endedAt.UTC().Format(time.RFC3339Nano), archiveSeq))
	return fmt.Sprintf("alert-previous-%x", sum[:8])
}

func alertDeliveryScopedKey(authorityScope, privateKey string) string {
	return authorityScope + "\x00" + privateKey
}

func alertDeliveryAttemptID(receiptKey string, number int, at time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "alert-attempt-v1\x00%s\x00%d\x00%s", receiptKey, number, at.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("alert-attempt-%x", sum[:8])
}

func alertDeliveryReceiptKey(authorityScope, occurrenceKey, targetRef string) string {
	return alertDeliveryHash("receipt-v2", authorityScope, occurrenceKey, targetRef)
}

func alertDeliveryHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func validAlertHash(value string) bool {
	return validAlertPrivateKey(value, "sha256:")
}

func validAlertPrivateKey(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil && value == strings.ToLower(value)
}

func validAlertAttemptID(value string) bool {
	if !strings.HasPrefix(value, "alert-attempt-") || len(value) != len("alert-attempt-")+16 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "alert-attempt-"))
	return err == nil
}

func cloneAlertDeliveryData(in *alertDeliveryData) *alertDeliveryData {
	if in == nil {
		return nil
	}
	out := *in
	out.Snapshot = cloneAlertSnapshot(in.Snapshot)
	out.SourceWatermarks = cloneAlertSourceWatermarks(in.SourceWatermarks)
	out.SourceWatermarksByScope = cloneAlertSourceWatermarksByScope(in.SourceWatermarksByScope)
	out.Episodes = append([]alertDeliveryEpisode(nil), in.Episodes...)
	out.Occurrences = append([]alertDeliveryOccurrence(nil), in.Occurrences...)
	out.PreviousContexts = append([]alertDeliveryPreviousContext(nil), in.PreviousContexts...)
	out.Attempts = append([]alertDeliveryAttempt(nil), in.Attempts...)
	out.Receipts = append([]alertDeliveryReceipt(nil), in.Receipts...)
	out.RetiredTargets = make(map[string]time.Time, len(in.RetiredTargets))
	maps.Copy(out.RetiredTargets, in.RetiredTargets)
	out.Baselines = make(map[string]alertDeliveryBaseline, len(in.Baselines))
	maps.Copy(out.Baselines, in.Baselines)
	return &out
}

func cloneAlertSnapshot(in rpc.AlertCandidateSnapshot) rpc.AlertCandidateSnapshot {
	out := in
	if in.Candidates != nil {
		out.Candidates = make([]rpc.AlertCandidate, len(in.Candidates))
		copy(out.Candidates, in.Candidates)
	}
	out.Coverage = cloneAlertCoverage(in.Coverage)
	out.Sources = cloneAlertSourceCoverageRows(in.Sources)
	return out
}

func cloneAlertSourceCoverageRows(in []rpc.AlertSourceCoverage) []rpc.AlertSourceCoverage {
	if in == nil {
		return nil
	}
	out := make([]rpc.AlertSourceCoverage, len(in))
	copy(out, in)
	return out
}

func cloneAlertCoverage(in rpc.AlertCoverage) rpc.AlertCoverage {
	out := in
	if in.ExpectedSources != nil {
		out.ExpectedSources = make([]rpc.AlertSource, len(in.ExpectedSources))
		copy(out.ExpectedSources, in.ExpectedSources)
	}
	if in.CoveredSources != nil {
		out.CoveredSources = make([]rpc.AlertSource, len(in.CoveredSources))
		copy(out.CoveredSources, in.CoveredSources)
	}
	return out
}

func cloneAlertSourceWatermarks(in map[rpc.AlertSource]time.Time) map[rpc.AlertSource]time.Time {
	out := make(map[rpc.AlertSource]time.Time, len(in))
	maps.Copy(out, in)
	return out
}

func unavailableAlertSourceRows(expected []rpc.AlertSource, reason string) []rpc.AlertSourceCoverage {
	rows := make([]rpc.AlertSourceCoverage, 0, len(expected))
	for _, source := range expected {
		rows = append(rows, rpc.AlertSourceCoverage{
			Source: source, Status: "unavailable", Reason: reason, EvidenceHealth: rpc.AlertEvidenceUnavailable,
		})
	}
	slices.SortFunc(rows, func(a, b rpc.AlertSourceCoverage) int {
		return cmp.Compare(a.Source, b.Source)
	})
	return rows
}

func cloneAlertSourceWatermarksByScope(in map[string]map[rpc.AlertSource]time.Time) map[string]map[rpc.AlertSource]time.Time {
	out := make(map[string]map[rpc.AlertSource]time.Time, len(in))
	for scope, watermarks := range in {
		out[scope] = cloneAlertSourceWatermarks(watermarks)
	}
	return out
}

func (s *Store) validateAlertDeliveryState() error {
	data := s.data.AlertDelivery
	if data == nil {
		return nil
	}
	s.initAlertDeliveryRuntime()
	if data.Version != AlertDeliveryVersion || data.Generation == 0 {
		return fmt.Errorf("%w: invalid alert delivery version or generation", ErrInvalidPersistedState)
	}
	if err := rpc.ValidateAlertCandidateSnapshot(data.Snapshot); err != nil {
		return fmt.Errorf("%w: invalid alert delivery snapshot: %v", ErrInvalidPersistedState, err)
	}
	// JSON written before retired_targets became non-omitempty can decode an
	// empty map as nil. Empty is unambiguous and safe to normalize.
	if data.SourceWatermarks == nil {
		data.SourceWatermarks = make(map[rpc.AlertSource]time.Time)
	}
	if data.SourceWatermarksByScope == nil {
		return fmt.Errorf("%w: missing scoped alert delivery watermarks", ErrInvalidPersistedState)
	}
	if data.RetiredTargets == nil {
		data.RetiredTargets = make(map[string]time.Time)
	}
	if data.Baselines == nil {
		return fmt.Errorf("%w: missing alert delivery baselines", ErrInvalidPersistedState)
	}
	if len(data.Episodes) > s.alertDeliveryMaxItems || len(data.Occurrences) > s.alertDeliveryMaxItems ||
		len(data.PreviousContexts) > s.alertDeliveryMaxItems || len(data.Attempts) > s.alertDeliveryMaxItems || len(data.Receipts) > s.alertDeliveryMaxItems || len(data.RetiredTargets) > s.alertDeliveryMaxItems || len(data.SourceWatermarksByScope) > s.alertDeliveryMaxItems || len(data.Baselines) > s.alertDeliveryMaxItems {
		return fmt.Errorf("%w: alert delivery evidence exceeds capacity", ErrInvalidPersistedState)
	}
	if !validAlertDeliveryHealth(data.Health) {
		return fmt.Errorf("%w: invalid alert delivery health", ErrInvalidPersistedState)
	}
	episodes := make(map[string]alertDeliveryEpisode, len(data.Episodes))
	for _, episode := range data.Episodes {
		key := alertDeliveryScopedKey(episode.AuthorityScope, episode.EpisodeKey)
		if err := rpc.ValidateAlertAuthorityScope(episode.AuthorityScope); err != nil {
			return fmt.Errorf("%w: invalid alert delivery episode scope", ErrInvalidPersistedState)
		}
		if _, duplicate := episodes[key]; duplicate || episode.CurrentOccurrenceKey == "" || episode.FirstSeenAt.IsZero() || episode.LastSeenAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery episode", ErrInvalidPersistedState)
		}
		episodes[key] = episode
	}
	displays := map[string]struct{}{}
	occurrences := make(map[string]alertDeliveryOccurrence, len(data.Occurrences))
	occurrencesByDisplay := make(map[string]alertDeliveryOccurrence, len(data.Occurrences))
	unreadSeqs := make(map[uint64]struct{})
	for _, occurrence := range data.Occurrences {
		if err := rpc.ValidateAlertAuthorityScope(occurrence.AuthorityScope); err != nil {
			return fmt.Errorf("%w: invalid alert delivery occurrence scope", ErrInvalidPersistedState)
		}
		candidate := rpc.AlertCandidate{
			EpisodeKey: occurrence.EpisodeKey, OccurrenceKey: occurrence.OccurrenceKey,
			EvidenceFingerprint: occurrence.EvidenceFingerprint, Source: occurrence.Source, Kind: occurrence.Kind,
			PresentationCode: occurrence.PresentationCode, State: occurrence.State, Severity: occurrence.Severity,
			EvidenceHealth: occurrence.EvidenceHealth, Destination: occurrence.Destination,
			EvidenceAsOf: occurrence.EvidenceAsOf, StateChangedAt: occurrence.StateChangedAt, ObservedAt: occurrence.ObservedAt,
		}
		if err := rpc.ValidateAlertCandidate(candidate); err != nil {
			return fmt.Errorf("%w: invalid alert delivery occurrence: %v", ErrInvalidPersistedState, err)
		}
		expectedDisplayID := alertDeliveryDisplayID(occurrence.AuthorityScope, occurrence.OccurrenceKey)
		if occurrence.DisplayID != expectedDisplayID || occurrence.FirstSeenAt.IsZero() || occurrence.LastSeenAt.IsZero() || occurrence.AttentionSeq == 0 || !validAlertDisposition(occurrence.Disposition) {
			return fmt.Errorf("%w: invalid alert delivery occurrence metadata", ErrInvalidPersistedState)
		}
		if occurrence.FirstSeenAt.After(occurrence.LastSeenAt) || !occurrence.ObservedAt.Equal(occurrence.LastSeenAt) {
			return fmt.Errorf("%w: invalid alert delivery occurrence observation interval", ErrInvalidPersistedState)
		}
		if occurrence.EndedAt.IsZero() {
			if occurrence.EndReason != "" || occurrence.State == rpc.AlertEpisodeRecovered {
				return fmt.Errorf("%w: invalid alert delivery occurrence end state", ErrInvalidPersistedState)
			}
		} else {
			if occurrence.EndedAt.Before(occurrence.StateChangedAt) {
				return fmt.Errorf("%w: invalid alert delivery occurrence end time", ErrInvalidPersistedState)
			}
			coherent := (occurrence.State == rpc.AlertEpisodeRecovered && (occurrence.EndReason == AlertDeliveryEndRecovered || occurrence.EndReason == AlertDeliveryEndOmitted)) ||
				(occurrence.State != rpc.AlertEpisodeRecovered && occurrence.EndReason == AlertDeliveryEndSuperseded)
			if !coherent {
				return fmt.Errorf("%w: invalid alert delivery occurrence end reason", ErrInvalidPersistedState)
			}
		}
		occurrenceMapKey := alertDeliveryScopedKey(occurrence.AuthorityScope, occurrence.OccurrenceKey)
		if _, duplicate := occurrences[occurrenceMapKey]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery occurrence", ErrInvalidPersistedState)
		}
		if _, duplicate := displays[occurrence.DisplayID]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery display id", ErrInvalidPersistedState)
		}
		if occurrence.AttentionSeq > data.AttentionHighWaterSeq {
			return fmt.Errorf("%w: alert delivery attention exceeds high-water", ErrInvalidPersistedState)
		}
		if occurrence.AttentionSeq > data.AttentionReadThroughSeq {
			if _, duplicate := unreadSeqs[occurrence.AttentionSeq]; duplicate {
				return fmt.Errorf("%w: duplicate alert delivery attention sequence", ErrInvalidPersistedState)
			}
			unreadSeqs[occurrence.AttentionSeq] = struct{}{}
		}
		occurrences[occurrenceMapKey] = occurrence
		occurrencesByDisplay[alertDeliveryScopedKey(occurrence.AuthorityScope, occurrence.DisplayID)] = occurrence
		displays[occurrence.DisplayID] = struct{}{}
	}
	archiveSeqs := make(map[uint64]struct{}, len(data.PreviousContexts))
	for _, previous := range data.PreviousContexts {
		if err := rpc.ValidateAlertAuthorityScope(previous.AuthorityScope); err != nil {
			return fmt.Errorf("%w: invalid previous alert context scope", ErrInvalidPersistedState)
		}
		origin, ok := occurrencesByDisplay[alertDeliveryScopedKey(previous.AuthorityScope, previous.PriorDisplayID)]
		if !ok || origin.Source != previous.Source || origin.Kind != previous.Kind || !origin.FirstSeenAt.Equal(previous.FirstSeenAt) || origin.AttentionSeq != previous.OriginalAttention {
			return fmt.Errorf("%w: previous alert context origin mismatch", ErrInvalidPersistedState)
		}
		if previous.State == rpc.AlertEpisodeRecovered || previous.EndReason != AlertDeliveryEndAuthorityScopeChanged || previous.EndedAt.IsZero() ||
			previous.EndedAt.Before(previous.StateChangedAt) || previous.EndedAt.Before(previous.LastSeenAt) || previous.FirstSeenAt.After(previous.LastSeenAt) ||
			previous.ArchiveSeq == 0 || previous.ArchiveSeq > data.PreviousContextHighWater ||
			previous.DisplayID != alertPreviousContextDisplayID(previous.AuthorityScope, previous.PriorDisplayID, previous.EndedAt, previous.ArchiveSeq) || previous.OriginalAttention == 0 || previous.OriginalAttention > data.AttentionHighWaterSeq {
			return fmt.Errorf("%w: invalid previous alert context lifecycle", ErrInvalidPersistedState)
		}
		if _, duplicate := archiveSeqs[previous.ArchiveSeq]; duplicate {
			return fmt.Errorf("%w: duplicate previous alert context sequence", ErrInvalidPersistedState)
		}
		archiveSeqs[previous.ArchiveSeq] = struct{}{}
		candidate := rpc.AlertCandidate{
			EpisodeKey: origin.EpisodeKey, OccurrenceKey: origin.OccurrenceKey, EvidenceFingerprint: origin.EvidenceFingerprint,
			Source: previous.Source, Kind: previous.Kind, PresentationCode: previous.PresentationCode, State: previous.State, Severity: previous.Severity,
			EvidenceHealth: previous.EvidenceHealth, Destination: previous.Destination,
			EvidenceAsOf: previous.EvidenceAsOf, StateChangedAt: previous.StateChangedAt, ObservedAt: previous.LastSeenAt,
		}
		if err := rpc.ValidateAlertCandidate(candidate); err != nil {
			return fmt.Errorf("%w: invalid previous alert context projection: %v", ErrInvalidPersistedState, err)
		}
		if !validAlertDisposition(previous.Disposition) {
			return fmt.Errorf("%w: invalid previous alert context disposition", ErrInvalidPersistedState)
		}
		if _, duplicate := displays[previous.DisplayID]; duplicate {
			return fmt.Errorf("%w: duplicate previous alert context display id", ErrInvalidPersistedState)
		}
		displays[previous.DisplayID] = struct{}{}
	}
	for _, occurrence := range data.Occurrences {
		if occurrence.AuthorityScope == data.Snapshot.AuthorityScope || !occurrence.EndedAt.IsZero() || occurrence.AttentionSeq <= data.AttentionReadThroughSeq {
			continue
		}
		if _, ok := latestAlertDeliveryPreviousContext(data, occurrence.AuthorityScope, occurrence.DisplayID); !ok {
			return fmt.Errorf("%w: dormant alert occurrence lacks previous-context projection", ErrInvalidPersistedState)
		}
	}
	if data.AttentionReadThroughSeq > data.AttentionHighWaterSeq || uint64(len(unreadSeqs)) != data.AttentionHighWaterSeq-data.AttentionReadThroughSeq {
		return fmt.Errorf("%w: invalid alert delivery attention cursor", ErrInvalidPersistedState)
	}
	for _, episode := range episodes {
		occurrence, ok := occurrences[alertDeliveryScopedKey(episode.AuthorityScope, episode.CurrentOccurrenceKey)]
		if !ok || occurrence.AuthorityScope != episode.AuthorityScope || occurrence.EpisodeKey != episode.EpisodeKey || occurrence.Source != episode.Source || occurrence.Kind != episode.Kind || occurrence.State != episode.State {
			return fmt.Errorf("%w: alert delivery episode current state mismatch", ErrInvalidPersistedState)
		}
	}
	attemptIDs := map[string]struct{}{}
	nextAttemptNumber := map[string]int{}
	previousByReceipt := map[string]alertDeliveryAttempt{}
	acceptedByReceipt := map[string]time.Time{}
	latestByReceipt := map[string]alertDeliveryAttempt{}
	for _, attempt := range data.Attempts {
		if err := rpc.ValidateAlertAuthorityScope(attempt.AuthorityScope); err != nil {
			return fmt.Errorf("%w: invalid alert delivery attempt scope", ErrInvalidPersistedState)
		}
		expectedReceiptKey := alertDeliveryReceiptKey(attempt.AuthorityScope, attempt.OccurrenceKey, attempt.TargetRef)
		if !validAlertAttemptID(attempt.ID) || !validAlertHash(attempt.TargetRef) || attempt.ReceiptKey != expectedReceiptKey || attempt.AttemptNumber < 1 || attempt.AttemptNumber > 4 || attempt.ReservedAt.IsZero() || !validAlertDeliveryAttemptClass(attempt.Class) {
			return fmt.Errorf("%w: invalid alert delivery attempt", ErrInvalidPersistedState)
		}
		expectedNumber := nextAttemptNumber[attempt.ReceiptKey] + 1
		if attempt.AttemptNumber != expectedNumber || attempt.ID != alertDeliveryAttemptID(attempt.ReceiptKey, attempt.AttemptNumber, attempt.ReservedAt) {
			return fmt.Errorf("%w: invalid alert delivery attempt sequence or identity", ErrInvalidPersistedState)
		}
		if !validAlertDeliveryAttemptLifecycle(attempt) {
			return fmt.Errorf("%w: invalid alert delivery attempt lifecycle", ErrInvalidPersistedState)
		}
		retiredAt := data.RetiredTargets[attempt.TargetRef]
		if !attempt.RetiredAt.IsZero() && (retiredAt.IsZero() || !attempt.RetiredAt.Equal(retiredAt)) {
			return fmt.Errorf("%w: alert delivery attempt retirement mismatches target tombstone", ErrInvalidPersistedState)
		}
		if previous, ok := previousByReceipt[attempt.ReceiptKey]; ok {
			if !validAlertDeliveryAttemptTransition(previous, attempt, data.RetiredTargets) {
				return fmt.Errorf("%w: invalid alert delivery attempt transition chain", ErrInvalidPersistedState)
			}
		}
		if _, ok := occurrences[alertDeliveryScopedKey(attempt.AuthorityScope, attempt.OccurrenceKey)]; !ok {
			return fmt.Errorf("%w: alert delivery attempt occurrence missing", ErrInvalidPersistedState)
		}
		if _, duplicate := attemptIDs[attempt.ID]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery attempt id", ErrInvalidPersistedState)
		}
		attemptIDs[attempt.ID] = struct{}{}
		nextAttemptNumber[attempt.ReceiptKey] = attempt.AttemptNumber
		previousByReceipt[attempt.ReceiptKey] = attempt
		latestByReceipt[attempt.ReceiptKey] = attempt
		if attempt.Class == AlertDeliveryAttemptAccepted {
			if attempt.CompletedAt.IsZero() {
				return fmt.Errorf("%w: accepted alert delivery attempt is incomplete", ErrInvalidPersistedState)
			}
			if _, duplicate := acceptedByReceipt[attempt.ReceiptKey]; duplicate {
				return fmt.Errorf("%w: duplicate accepted alert delivery attempt", ErrInvalidPersistedState)
			}
			acceptedByReceipt[attempt.ReceiptKey] = attempt.CompletedAt
		}
	}
	receiptKeys := map[string]struct{}{}
	for _, receipt := range data.Receipts {
		if err := rpc.ValidateAlertAuthorityScope(receipt.AuthorityScope); err != nil {
			return fmt.Errorf("%w: invalid alert delivery receipt scope", ErrInvalidPersistedState)
		}
		expectedReceiptKey := alertDeliveryReceiptKey(receipt.AuthorityScope, receipt.OccurrenceKey, receipt.TargetRef)
		if !validAlertHash(receipt.TargetRef) || receipt.ReceiptKey != expectedReceiptKey || receipt.AcceptedAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery receipt", ErrInvalidPersistedState)
		}
		if _, ok := occurrences[alertDeliveryScopedKey(receipt.AuthorityScope, receipt.OccurrenceKey)]; !ok {
			return fmt.Errorf("%w: alert delivery receipt occurrence missing", ErrInvalidPersistedState)
		}
		if _, duplicate := receiptKeys[receipt.ReceiptKey]; duplicate {
			return fmt.Errorf("%w: duplicate alert delivery receipt", ErrInvalidPersistedState)
		}
		latest, ok := latestByReceipt[receipt.ReceiptKey]
		if !ok || latest.Class != AlertDeliveryAttemptAccepted || !latest.CompletedAt.Equal(receipt.AcceptedAt) {
			return fmt.Errorf("%w: alert delivery receipt lacks matching latest acceptance", ErrInvalidPersistedState)
		}
		retiredAt := data.RetiredTargets[receipt.TargetRef]
		if (!receipt.RetiredAt.IsZero() && (retiredAt.IsZero() || !receipt.RetiredAt.Equal(retiredAt))) || !receipt.RetiredAt.Equal(latest.RetiredAt) {
			return fmt.Errorf("%w: alert delivery receipt retirement mismatches latest acceptance", ErrInvalidPersistedState)
		}
		receiptKeys[receipt.ReceiptKey] = struct{}{}
	}
	for receiptKey, acceptedAt := range acceptedByReceipt {
		if _, ok := receiptKeys[receiptKey]; !ok || acceptedAt.IsZero() {
			return fmt.Errorf("%w: accepted alert delivery attempt lacks receipt", ErrInvalidPersistedState)
		}
	}
	for target, retiredAt := range data.RetiredTargets {
		if !validAlertHash(target) || retiredAt.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery retired target", ErrInvalidPersistedState)
		}
	}
	for scope, baseline := range data.Baselines {
		if err := rpc.ValidateAlertAuthorityScope(scope); err != nil || baseline.EstablishedAt.IsZero() || baseline.SnapshotAsOf.IsZero() || baseline.SnapshotAsOf.After(baseline.EstablishedAt) {
			return fmt.Errorf("%w: invalid alert delivery baseline", ErrInvalidPersistedState)
		}
	}
	for source, watermark := range data.SourceWatermarks {
		if !validAlertDeliverySource(source) || watermark.IsZero() {
			return fmt.Errorf("%w: invalid alert delivery source watermark", ErrInvalidPersistedState)
		}
	}
	for scope, watermarks := range data.SourceWatermarksByScope {
		if err := rpc.ValidateAlertAuthorityScope(scope); err != nil || watermarks == nil {
			return fmt.Errorf("%w: invalid scoped alert delivery watermarks", ErrInvalidPersistedState)
		}
		for source, watermark := range watermarks {
			if !validAlertDeliverySource(source) || watermark.IsZero() {
				return fmt.Errorf("%w: invalid scoped alert delivery source watermark", ErrInvalidPersistedState)
			}
		}
	}
	currentWatermarks, ok := data.SourceWatermarksByScope[data.Snapshot.AuthorityScope]
	if !ok || !maps.Equal(data.SourceWatermarks, currentWatermarks) {
		return fmt.Errorf("%w: current alert delivery watermarks mismatch authority scope", ErrInvalidPersistedState)
	}
	return nil
}

func validAlertDeliveryAttemptTransition(previous, current alertDeliveryAttempt, retiredTargets map[string]time.Time) bool {
	if (previous.Class == AlertDeliveryAttemptRetry || previous.Class == AlertDeliveryAttemptInterrupted) &&
		!previous.RetryAt.IsZero() && !current.ReservedAt.Before(previous.RetryAt) {
		return true
	}

	// Retirement closes all attempts for a target, including historical retry
	// that otherwise-terminal predecessor only when the exact target tombstone
	retiredAt := retiredTargets[previous.TargetRef]
	if retiredAt.IsZero() || !previous.RetiredAt.Equal(retiredAt) || !current.RetiredAt.Equal(retiredAt) ||
		previous.Disposition != AlertDeliveryCompletionRetired || !previous.RetryAt.IsZero() || current.ReservedAt.After(retiredAt) {
		return false
	}
	return previous.Class == AlertDeliveryAttemptRetired || previous.Class == AlertDeliveryAttemptInterrupted
}

func validAlertDeliveryAttemptLifecycle(attempt alertDeliveryAttempt) bool {
	completed := !attempt.CompletedAt.IsZero()
	retrying := !attempt.RetryAt.IsZero()
	retired := !attempt.RetiredAt.IsZero()
	if completed && attempt.CompletedAt.Before(attempt.ReservedAt) {
		return false
	}

	scheduledRetry := func() bool {
		delay, ok := alertDeliveryRetryDelay(attempt.AttemptNumber)
		return ok && retrying && attempt.RetryAt.Equal(attempt.CompletedAt.Add(delay))
	}

	switch attempt.Class {
	case AlertDeliveryAttemptReserved:
		return !completed && !retrying && !retired && attempt.Disposition == ""
	case AlertDeliveryAttemptConfirmed:
		return !completed && !retrying && ((!retired && attempt.Disposition == "") || (retired && attempt.Disposition == AlertDeliveryCompletionRetired))
	case AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRejected:
		if !completed || retrying {
			return false
		}
		switch attempt.Disposition {
		case AlertDeliveryCompletionApplied, AlertDeliveryCompletionInactive:
			// A later target retirement stamps RetiredAt while preserving the
			// transport-time disposition, so retirement is optional here.
			return true
		case AlertDeliveryCompletionRetired:
			return retired
		default:
			return false
		}
	case AlertDeliveryAttemptRetry:
		if !completed {
			return false
		}
		switch attempt.Disposition {
		case "", AlertDeliveryCompletionApplied:
			return !retired && scheduledRetry()
		case AlertDeliveryCompletionInactive:
			return !retired && !retrying
		case AlertDeliveryCompletionRetired:
			return retired && !retrying
		default:
			return false
		}
	case AlertDeliveryAttemptInterrupted:
		if !completed {
			return false
		}
		switch attempt.Disposition {
		case "":
			if retired {
				return false
			}
			if _, ok := alertDeliveryRetryDelay(attempt.AttemptNumber); ok {
				return scheduledRetry()
			}
			return !retrying
		case AlertDeliveryCompletionInactive:
			return !retrying
		case AlertDeliveryCompletionRetired:
			return retired && !retrying
		default:
			return false
		}
	case AlertDeliveryAttemptRetired:
		return completed && !retrying && retired && attempt.Disposition == AlertDeliveryCompletionRetired
	case AlertDeliveryAttemptInactive:
		return completed && !retrying && attempt.Disposition == AlertDeliveryCompletionInactive
	case AlertDeliveryAttemptExhausted:
		return attempt.AttemptNumber == 4 && completed && !retrying && (attempt.Disposition == "" || attempt.Disposition == AlertDeliveryCompletionApplied)
	case AlertDeliveryAttemptModeSuppressed:
		return completed && !retrying && attempt.Disposition == ""
	default:
		return false
	}
}

func validAlertDeliveryAttemptClass(class string) bool {
	switch class {
	case AlertDeliveryAttemptReserved, AlertDeliveryAttemptConfirmed, AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRetry,
		AlertDeliveryAttemptRejected, AlertDeliveryAttemptInterrupted, AlertDeliveryAttemptRetired,
		AlertDeliveryAttemptInactive, AlertDeliveryAttemptExhausted, AlertDeliveryAttemptModeSuppressed:
		return true
	default:
		return false
	}
}

func validAlertDeliverySource(source rpc.AlertSource) bool {
	switch source {
	case rpc.AlertSourceStress, rpc.AlertSourceRegime, rpc.AlertSourceRulebook, rpc.AlertSourceRiskPolicy,
		rpc.AlertSourceProtection, rpc.AlertSourceOrderIntegrity, rpc.AlertSourceReconciliation,
		rpc.AlertSourceGovernance, rpc.AlertSourceDataHealth, rpc.AlertSourceDelivery:
		return true
	default:
		return false
	}
}

func validAlertDisposition(disposition string) bool {
	switch disposition {
	case AlertDispositionEligible, AlertDispositionModeSuppressed, AlertDispositionObserveOnly, AlertDispositionCutoverExisting:
		return true
	default:
		return false
	}
}

func validAlertDeliveryHealth(health AlertDeliveryHealth) bool {
	if health.UpdatedAt.IsZero() {
		return false
	}
	switch health.State {
	case AlertDeliveryHealthHealthy:
		return health.Class == ""
	case AlertDeliveryHealthDegraded:
		switch health.Class {
		case AlertDeliveryHealthClassRetry, AlertDeliveryHealthClassRejected, AlertDeliveryHealthClassInterrupted, AlertDeliveryHealthClassObservation, AlertDeliveryAttemptExhausted:
			return true
		default:
			return false
		}
	case AlertDeliveryHealthUnavailable:
		return health.Class == AlertDeliveryHealthClassStateWrite || validAlertDeliveryPrerequisiteClass(health.Class)
	case AlertDeliveryHealthOverflow:
		return health.Class == AlertDeliveryHealthClassOverflow
	default:
		return false
	}
}

func validAlertDeliveryPrerequisiteClass(class string) bool {
	switch class {
	case AlertDeliveryHealthClassNoSubscription, AlertDeliveryHealthClassSigningKeys, AlertDeliveryHealthClassSender, AlertDeliveryHealthClassFuse:
		return true
	default:
		return false
	}
}

// AcceptedAlertPushesSince counts push-service-accepted deliveries since the
// given instant. It backs the runaway fuse: a guard against software
// malfunction, never an alarm-policy cap — distinct real events are always
// deliverable below the fuse rate.
func (s *Store) AcceptedAlertPushesSince(since time.Time) int {
	since = since.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	data := s.data.AlertDelivery
	if data == nil {
		return 0
	}
	n := 0
	for _, receipt := range data.Receipts {
		if !receipt.AcceptedAt.IsZero() && !receipt.AcceptedAt.Before(since) {
			n++
		}
	}
	return n
}
