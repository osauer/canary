// Package corestore owns the daemon's authoritative SQLite state in daemon.db.
//
// The store exposes typed transactions for mutable state, append-only evidence,
// broker-scoped order safety, retained observations, and statement projections.
// Every durable mutation advances a monotonic authority head. Opening,
// inspection, backup, and upgrade paths validate schema, integrity, content
// hashes, and any caller-supplied rollback floor; they never repair or recreate
// an existing authority after a validation failure.
//
// Store serializes mutations internally. Callers never receive the underlying
// SQL handle and must use the combined operations when state and evidence need
// to become visible atomically.
package corestore

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

// Store errors classify authority, concurrency, and durability failures that
// callers may handle without parsing error text.
var (
	ErrRevisionConflict       = errors.New("corestore: revision conflict")
	ErrPreviewTokenConsumed   = errors.New("corestore: preview token already consumed")
	ErrBrokerScopeCollision   = errors.New("corestore: broker scope collision")
	ErrAuthorityMismatch      = errors.New("corestore: authority mismatch")
	ErrRollback               = errors.New("corestore: authority rollback detected")
	ErrBlocked                = errors.New("corestore: health is blocked")
	ErrOrderIDFloor           = errors.New("corestore: reserved order id does not advance global floor")
	ErrOrderNotModifiable     = errors.New("corestore: order durable frontier is not modifiable")
	ErrCheckpointBusy         = errors.New("corestore: WAL checkpoint is busy")
	ErrFreshAuthorityConflict = errors.New("corestore: fresh trading authority requires empty order and purge state")
	ErrProjectionConflict     = errors.New("corestore: immutable projection conflict")
	ErrUpgradeRequired        = errors.New("corestore: schema upgrade required")
	ErrRecoveryNotEligible    = errors.New("corestore: transient recovery is not eligible")
)

// Options configures the authoritative store. Path is required; the daemon
type Options struct {
	Path        string
	BusyTimeout time.Duration
	// MinimumHead, when non-nil, prevents opening an older copy of the same
	// authority. It is intended for restore/backup selection boundaries.
	MinimumHead *AuthorityHead
	// CommitObserver runs synchronously after every successful durable
	// to persist an external monotonic head; an observer failure is returned
	CommitObserver func(AuthorityHead) error
}

// AuthorityHead is the rollback-detection identity and monotonic write head
type AuthorityHead struct {
	AuthorityEpoch   string
	HeadGeneration   int64
	LastEventSeq     int64
	SignerGeneration int64
}

// UpgradeRequiredError reports a valid, supported authority that must be
// upgraded out of place before this build can open it for service.
type UpgradeRequiredError struct {
	CurrentVersion int
	TargetVersion  int
}

// Error describes the current and target schema versions.
func (e *UpgradeRequiredError) Error() string {
	return fmt.Sprintf("%v: database version %d, target version %d", ErrUpgradeRequired, e.CurrentVersion, e.TargetVersion)
}

// Is reports whether the error matches ErrUpgradeRequired.
func (e *UpgradeRequiredError) Is(target error) bool { return target == ErrUpgradeRequired }

// InspectionStatus classifies whether a validated authority is directly
type InspectionStatus string

// Inspection statuses returned by Inspect.
const (
	InspectionCurrent         InspectionStatus = "current"
	InspectionUpgradeRequired InspectionStatus = "upgrade_required"
)

// InspectOptions configures a non-mutating authority inspection. Path must
// selects one frozen migration-plan prefix; zero means the current version.
type InspectOptions struct {
	Path          string
	MinimumHead   *AuthorityHead
	TargetVersion int
}

// Inspection is the validated identity, version, and write head of an
// authority database. TargetVersion is the selected frozen plan version.
type Inspection struct {
	Path           string
	SchemaVersion  int
	TargetVersion  int
	Status         InspectionStatus
	Head           AuthorityHead
	Integrity      IntegrityReport
	HeadTransition UpgradeHeadTransition
}

// ObservationDiscardSelector identifies one exact class of derived
// observations that a reviewed migration may discard. All three fields are
// required; this is not a general retention or expiry surface.
type ObservationDiscardSelector struct {
	ScopeKey string
	Source   string
	Kind     string
}

// ObservationDiscardSummary is deterministic evidence of the rows one
// maintenance migration removed from its disposable working snapshot.
// OrderedDigestSHA256 uses the domain "canary.observation-discard.v1\x00",
// then each selector string as an 8-byte big-endian length plus bytes, then
// each observation ID as 8-byte big-endian plus its stored 32-byte payload
// digest in ascending ID order. Payload bytes are never copied into
// coordination state.
type ObservationDiscardSummary struct {
	MigrationVersion    int
	MigrationName       string
	Selector            ObservationDiscardSelector
	RemovedRows         int64
	PayloadBytes        int64
	OrderedDigestSHA256 string
}

// EventDiscardSelector identifies the one reviewed class of event snapshots a
// migration may discard. Predicate is a frozen implementation identifier, not
type EventDiscardSelector struct {
	ScopeKey  string
	EventType string
	Predicate string
}

// EventDiscardSummary is deterministic evidence of the event rows one
// maintenance migration removed from its disposable working snapshot.
// ascending sequence order. Payload bytes are never copied into coordination
type EventDiscardSummary struct {
	MigrationVersion    int
	MigrationName       string
	Selector            EventDiscardSelector
	RemovedRows         int64
	PayloadBytes        int64
	OrderedDigestSHA256 string
}

// OperationalPruneSelector identifies the one reviewed beta-history reset.
// Predicate is a frozen implementation identifier; it is never caller input
type OperationalPruneSelector struct {
	Predicate string
}

// OperationalPruneSummary is the deterministic receipt for the v6
type OperationalPruneSummary struct {
	MigrationVersion int
	MigrationName    string
	Selector         OperationalPruneSelector

	RemovedObservationRows         int64
	RemovedObservationPayloadBytes int64
	ObservationDigestSHA256        string
	RemovedEventRows               int64
	RemovedEventPayloadBytes       int64
	EventDigestSHA256              string

	RemovedRegimeDecisionRows   int64
	RemovedRegimeIndicatorRows  int64
	RemovedRuleTransitionRows   int64
	RemovedStressTransitionRows int64
}

// UpgradeMaintenanceResult reports physical work required by pending
// migration metadata and the exact discard evidence produced while building
type UpgradeMaintenanceResult struct {
	Discards                       []ObservationDiscardSummary
	EventDiscards                  []EventDiscardSummary
	OperationalPrunes              []OperationalPruneSummary
	Compacted                      bool
	SourceBackupRetirementRequired bool
}

// UpgradeHeadTransition is the only authority-head effect an out-of-place
// schema upgrade may report. Ordinary pending migrations advance exactly once.
// A batch preserves the head only when every pending migration is an explicitly
type UpgradeHeadTransition string

// Supported upgrade head transitions.
const (
	UpgradeHeadTransitionAdvanceOnce UpgradeHeadTransition = "advance_once"
	UpgradeHeadTransitionPreserve    UpgradeHeadTransition = "preserve"
)

// UpgradeOptions describes an out-of-place schema upgrade. BackupPath is an
// immutable exact-old-head snapshot. CandidatePath is an unpublished,
// large-authority space bound. TargetVersion zero means current.
// ResetUnboundArtifacts is only for a preparing intent whose candidate has not
// exact source, durably removes all deterministic candidate and source-backup
type UpgradeOptions struct {
	SourcePath            string
	BackupPath            string
	CandidatePath         string
	MinimumHead           *AuthorityHead
	TargetVersion         int
	ReplaceCandidate      bool
	ResetUnboundArtifacts bool
}

// RecomputeUpgradeMaintenanceOptions identifies an exact immutable source
type RecomputeUpgradeMaintenanceOptions struct {
	SourcePath            string
	ExpectedSchemaVersion int
	TargetVersion         int
	ExpectedHead          AuthorityHead
}

// UpgradeTargetBackupOptions binds the narrow post-publication recovery copy
// to one exact frozen target schema and authority head. The source is opened
// read-only and is never migrated, checkpointed, or otherwise modified.
type UpgradeTargetBackupOptions struct {
	SourcePath            string
	BackupPath            string
	ExpectedSchemaVersion int
	ExpectedHead          AuthorityHead
}

// UpgradeResult contains independently verified artifacts. Source and Backup
// remain at the old version and exact old head. HeadTransition says whether
// Candidate preserves that head or advances HeadGeneration exactly once.
// PrepareUpgradeTargetBackup only after publishing and verifying Candidate;
type UpgradeResult struct {
	Source         Inspection
	Backup         BackupInfo
	Candidate      Inspection
	TargetBackup   *BackupInfo
	Maintenance    UpgradeMaintenanceResult
	HeadTransition UpgradeHeadTransition
}

// QuiesceOptions identifies the exact old authority that may be physically
// must hold the state-root persistence lock and must have closed every Store
type QuiesceOptions struct {
	Path                  string
	ExpectedSchemaVersion int
	ExpectedHead          AuthorityHead
}

// Health is fail-closed mutation health. Critical failures caused by a full,
// busy, readonly, corrupt, or I/O-failing SQLite store remain latched until an
// explicit reopen. RecoveryEligible is true only for the narrow case where a
// mutation committed but reading its post-commit head hit the bounded context
// deadline; the live store may clear that latch only after an integrity,
// identity, monotonic-head, and external-watermark proof succeeds.
type Health struct {
	Ready            bool
	Code             string
	BlockedAt        time.Time
	RecoveryEligible bool
}

// StateDocument is the current revision of one scope- and kind-addressed JSON
type StateDocument struct {
	ScopeKey  string
	Kind      string
	Revision  int64
	JSON      []byte
	UpdatedAt time.Time
}

// StateDocumentCAS requests a compare-and-swap update. ExpectedRevision zero
// creates a missing document at revision one; a positive value updates exactly
// behind a retained authority timestamp.
type StateDocumentCAS struct {
	ScopeKey         string
	Kind             string
	ExpectedRevision int64
	JSON             []byte
	// UpdatedAtNotBefore is an optional atomic commit-clock floor. The store
	// compares it with the exact timestamp it will persist inside the same
	// critical mutation, before touching the document or authority head. It is
	// zero for ordinary callers.
	UpdatedAtNotBefore time.Time
}

// RevisionConflictError reports the actual state observed after a failed
type RevisionConflictError struct {
	Expected int64
	Actual   int64
	Exists   bool
}

// Error describes the expected revision and the state observed in the store.
func (e *RevisionConflictError) Error() string {
	if !e.Exists {
		return fmt.Sprintf("%v: expected revision %d, document does not exist", ErrRevisionConflict, e.Expected)
	}
	return fmt.Sprintf("%v: expected revision %d, actual revision %d", ErrRevisionConflict, e.Expected, e.Actual)
}

// Is reports whether the error matches ErrRevisionConflict.
func (e *RevisionConflictError) Is(target error) bool { return target == ErrRevisionConflict }

// ObservationInput stores Payload byte-for-byte. ContentType and MetadataJSON
// describe it without interpreting untrusted source content as authority.
type ObservationInput struct {
	ScopeKey         string
	Source           string
	Kind             string
	ObservedAt       time.Time
	ContentType      string
	Payload          []byte
	MetadataJSON     []byte
	DecisionEligible bool
}

// ObservationReceipt identifies one immutable observation and the exact
type ObservationReceipt struct {
	ID            int64
	PayloadSHA256 [sha256.Size]byte
	RecordedAt    time.Time
}

// Observation is a retained source measurement. Payload is evidence, not
// trusted authority; only DecisionEligible rows may feed live decisions.
type Observation struct {
	ID            int64
	ScopeKey      string
	Source        string
	Kind          string
	ObservedAt    time.Time
	RecordedAt    time.Time
	ContentType   string
	Payload       []byte
	PayloadSHA256 [sha256.Size]byte
	MetadataJSON  []byte
	// DecisionEligible is a typed authority boundary. Imported legacy
	// observations are false and must never seed current runtime state.
	DecisionEligible bool
}

// ObservationQuery filters observations within one required scope. Time bounds
type ObservationQuery struct {
	ScopeKey           string
	Source             string
	Kind               string
	FromObservedAtMS   int64
	ToObservedAtMS     int64
	AfterObservationID int64
	DecisionEligible   *bool
	Limit              int
}

// BrokerScope binds an authority namespace to all broker identity pins. A
// ScopeKey can never be rebound, and one binding cannot be aliased by a second
type BrokerScope struct {
	ScopeKey string
	Endpoint string
	ClientID int
	Account  string
	Mode     string
}

// PreviewTokenDigest is the persisted SHA-256 identity of a canonical preview
// token identifier. Raw signed preview tokens do not belong in the store.
type PreviewTokenDigest [sha256.Size]byte

// HashPreviewTokenID hashes the canonical preview-token identifier. Callers
// must not pass the raw signed token; legacy state stores only this identifier.
func HashPreviewTokenID(previewTokenID string) PreviewTokenDigest {
	return sha256.Sum256([]byte(previewTokenID))
}

// ActionKind classifies the broker-side action represented by durable order
type ActionKind string

// Supported broker action kinds.
const (
	ActionPlace        ActionKind = "place"
	ActionModify       ActionKind = "modify"
	ActionCancel       ActionKind = "cancel"
	ActionPurge        ActionKind = "purge"
	ActionRestore      ActionKind = "restore"
	ActionExercise     ActionKind = "exercise"
	ActionSmokeCleanup ActionKind = "smoke_cleanup"
)

// TransmitOrigin identifies the allowlisted path that initiated a broker-side
type TransmitOrigin string

// Supported broker-write origins.
const (
	OriginAgentCLI TransmitOrigin = "agent_gated_cli"
	OriginHumanCLI TransmitOrigin = "human_cli"
	OriginDaemon   TransmitOrigin = "daemon_internal"
)

// OrderEventRecord is one append-only order lifecycle event bound to an exact
// broker scope. RawJSON is retained evidence and is not interpreted as
// authorization.
type OrderEventRecord struct {
	EventSeq        int64
	Scope           BrokerScope
	EventKey        string
	AtMS            int64
	Type            string
	Action          ActionKind
	Origin          TransmitOrigin
	OrderRef        string
	PreviewTokenID  string
	ReservedOrderID int64
	PermID          int64
	Status          string
	RawJSON         []byte
}

// OrderQuery filters order events in ascending event-sequence order.
type OrderQuery struct {
	ScopeKey        string
	FromAtMS        int64
	ToAtMS          int64
	AfterEventSeq   int64
	OrderRef        string
	ReservedOrderID *int64
	PermID          *int64
	PreviewTokenID  string
	Limit           int
}

// PreTransmitRequest is committed before a caller may transmit. Success is
// evidence of durable staging, not broker-submit authority.
type PreTransmitRequest struct {
	Scope                 BrokerScope
	TokenDigest           PreviewTokenDigest
	AuthorityEpoch        string
	SignerGeneration      int64
	RequestedOrderIDFloor int64
	ReservedOrderID       int64
	// ExpectedOrderEventSeq binds a modify to the exact durable per-order
	ExpectedOrderEventSeq *int64
	Action                ActionKind
	Origin                TransmitOrigin
	Events                []OrderEventRecord
}

// LifecycleCommit couples order lifecycle events with an optional state CAS in
// one transaction.
type LifecycleCommit struct {
	Scope  BrokerScope
	Events []OrderEventRecord
	State  *StateDocumentCAS
}

// LifecycleResult reports the committed event sequences, optional state
// revision, and resulting authority head.
type LifecycleResult struct {
	EventSeqs []int64
	State     *StateDocument
	Head      AuthorityHead
}

// PreTransmitResult is durable proof that the pre-transmit transaction
// succeeded. It does not itself authorize or confirm a broker transmission.
type PreTransmitResult struct {
	EffectiveOrderIDFloor int64
	EventSeqs             []int64
	Head                  AuthorityHead
}

// IntegrityReport combines SQLite structural and foreign-key results. Content
type IntegrityReport struct {
	QuickCheckResults    []string
	ForeignKeyViolations []ForeignKeyViolation
}

// OK reports whether SQLite returned exactly one successful quick-check row
func (r IntegrityReport) OK() bool {
	return len(r.QuickCheckResults) == 1 && r.QuickCheckResults[0] == "ok" && len(r.ForeignKeyViolations) == 0
}

// ForeignKeyViolation describes one row returned by SQLite foreign_key_check.
type ForeignKeyViolation struct {
	Table       string
	RowID       *int64
	ParentTable string
	ForeignKey  int64
}

// BackupInfo describes a validated standalone authority backup.
type BackupInfo struct {
	Path          string
	SchemaVersion int
	Head          AuthorityHead
	Integrity     IntegrityReport
}

// StatementFileRecord is one file in the current retained-statement inventory.
type StatementFileRecord struct {
	ScopeKey             string
	FileKey              string
	SizeBytes            int64
	SHA256               [sha256.Size]byte
	Status               string
	StatementGeneratedAt *time.Time
	IngestedAt           *time.Time
	UpdatedAt            time.Time
}

// StatementEquityDayRecord is the current statement-derived winner for one
// account and day, linked to the exact retained statement digest.
type StatementEquityDayRecord struct {
	ID                  int64
	ScopeKey            string
	AccountKey          string
	Day                 string
	EquityBaseText      string
	StatementFileKey    string
	StatementFileSHA256 [sha256.Size]byte
	GeneratedAt         time.Time
	RawJSON             []byte
}

// CheckpointResult reports SQLite WAL checkpoint progress. A nonzero Busy value
// means the authority was not fully quiesced.
type CheckpointResult struct {
	Busy               int
	LogFrames          int
	CheckpointedFrames int
}

// EventInput is one append-only event and an optional typed projection written
type EventInput struct {
	ScopeKey    string
	EventKey    string
	Type        string
	Action      string
	Origin      string
	OccurredAt  time.Time
	PayloadJSON []byte
	Projection  EventProjection
}

// EventReceipt identifies a committed event and the single resulting authority
type EventReceipt struct {
	EventSeq   int64
	RecordedAt time.Time
	Head       AuthorityHead
}

// EventRecord is one retained append-only event in event-sequence order.
type EventRecord struct {
	EventSeq    int64
	ScopeKey    string
	EventKey    string
	Type        string
	Action      string
	Origin      string
	OccurredAt  time.Time
	RecordedAt  time.Time
	PayloadJSON []byte
}

// EventQuery filters append-only events. Zero-valued filters are open and
// AfterEventSeq provides forward pagination.
type EventQuery struct {
	ScopeKey      string
	Type          string
	FromAtMS      int64
	ToAtMS        int64
	AfterEventSeq int64
	Limit         int
}

// EventProjection is a typed tagged union; zero values append only the
// canonical event_log row. At most one member may be non-nil.
type EventProjection struct {
	RegimeDecision   *RegimeDecisionProjection
	RuleTransition   *RuleTransitionProjection
	StressTransition *StressTransitionProjection
	CapitalEvent     *CapitalEventProjection
	RiskPolicyEvent  *RiskPolicyEventProjection
	ProposalOutcome  *ProposalOutcomeProjection
}

// RegimeDecisionProjection is the typed searchable projection of a regime
type RegimeDecisionProjection struct {
	DecisionKey string
	Stage       string
	Severity    string
	Readiness   string
	Confidence  string
	Verdict     string
	Fingerprint string
	Indicators  []RegimeIndicatorProjection
}

// RegimeIndicatorProjection is one indicator row attached to a projected
type RegimeIndicatorProjection struct {
	Indicator       string
	Status          string
	Band            string
	Value           *float64
	Depth           *float64
	StreakSessions  *int64
	Freshness       string
	Eligible        *bool
	Latched         bool
	ThresholdsLabel string
}

// RuleTransitionProjection is the typed searchable projection of a rule
type RuleTransitionProjection struct {
	RuleID, Status, PreviousStatus, PolicyID, PolicyFingerprint string
	PolicyVersion                                               *int64
}

// StressTransitionProjection is the typed searchable projection of a
// portfolio-stress transition event.
type StressTransitionProjection struct {
	Action, Severity, Direction, MarketStage, InputHealth string
	PortfolioAlertRelevant                                *bool
}

// CapitalEventProjection is the typed searchable projection of a capital
type CapitalEventProjection struct{ Kind, AmountBaseText, EffectiveAt, ReportID string }

// RiskPolicyEventProjection is the typed searchable projection of a risk-policy
type RiskPolicyEventProjection struct {
	Kind, PolicyID, PolicyFingerprint string
	PolicyVersion                     *int64
}

// ProposalOutcomeProjection is the typed searchable projection of a proposal
type ProposalOutcomeProjection struct{ ProposalKey, Revision, Bucket, Symbol, SecType, Action, State string }
