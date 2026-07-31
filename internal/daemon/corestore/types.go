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
	ErrLegacyImportConflict   = errors.New("corestore: legacy authority was already imported from a different source")
	ErrFreshAuthorityConflict = errors.New("corestore: fresh trading authority requires empty order and purge state")
	ErrProjectionConflict     = errors.New("corestore: immutable projection conflict")
	ErrUpgradeRequired        = errors.New("corestore: schema upgrade required")
)

// Options configures the authoritative store. Path is required; the daemon
// integration decides where daemon.db lives.
type Options struct {
	Path        string
	BusyTimeout time.Duration
	// MinimumHead, when non-nil, prevents opening an older copy of the same
	// authority. It is intended for restore/backup selection boundaries.
	MinimumHead *AuthorityHead
	// CommitObserver runs synchronously after every successful durable
	// mutation while the store's write lock is still held. Production uses it
	// to persist an external monotonic head; an observer failure is returned
	// to the caller and latches the store unhealthy without rolling back the
	// already-committed SQLite transaction.
	CommitObserver func(AuthorityHead) error
}

// AuthorityHead is the rollback-detection identity and monotonic write head
// carried inside every database and backup.
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
// serviceable by this build or requires an out-of-place upgrade.
type InspectionStatus string

// Inspection statuses returned by Inspect.
const (
	InspectionCurrent         InspectionStatus = "current"
	InspectionUpgradeRequired InspectionStatus = "upgrade_required"
)

// InspectOptions configures a non-mutating authority inspection. Path must
// already exist. MinimumHead, when non-nil, enforces the external monotonic
// watermark while the database is still opened read-only. TargetVersion
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

// UpgradeMaintenanceResult reports physical work required by pending
// migration metadata and the exact discard evidence produced while building
// the candidate. SourceBackupRetirementRequired is an instruction to the outer
// crash-recovery coordinator: the large old-head backup may be retired only
// after publication and independent target-head backup verification.
type UpgradeMaintenanceResult struct {
	Discards                       []ObservationDiscardSummary
	Compacted                      bool
	SourceBackupRetirementRequired bool
}

// UpgradeHeadTransition is the only authority-head effect an out-of-place
// schema upgrade may report. Ordinary pending migrations advance exactly once.
// A batch preserves the head only when every pending migration is an explicitly
// reviewed maintenance operation that leaves store_meta and event_log alone.
type UpgradeHeadTransition string

// Supported upgrade head transitions.
const (
	UpgradeHeadTransitionAdvanceOnce UpgradeHeadTransition = "advance_once"
	UpgradeHeadTransitionPreserve    UpgradeHeadTransition = "preserve"
)

// UpgradeOptions describes an out-of-place schema upgrade. BackupPath is an
// immutable exact-old-head snapshot. CandidatePath is an unpublished,
// independent database for the caller to atomically publish after any outer
// coordination state is durable. The target-head backup is deliberately a
// post-publication operation so source backup plus candidate stays within the
// large-authority space bound. TargetVersion zero means current.
// ResetUnboundArtifacts is only for a preparing intent whose candidate has not
// been durably fingerprint-bound: with ReplaceCandidate it revalidates the
// exact source, durably removes all deterministic candidate and source-backup
// artifacts, and rebuilds from the source.
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
// backup and frozen target plan. RecomputeUpgradeMaintenance derives the same
// discard evidence as candidate preparation without changing any file.
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
// TargetBackup remains nil during preparation. A maintenance coordinator calls
// PrepareUpgradeTargetBackup only after publishing and verifying Candidate;
// this ordering keeps the promised two-source-footprint space bound honest.
type UpgradeResult struct {
	Source         Inspection
	Backup         BackupInfo
	Candidate      Inspection
	TargetBackup   *BackupInfo
	Maintenance    UpgradeMaintenanceResult
	HeadTransition UpgradeHeadTransition
}

// QuiesceOptions identifies the exact old authority that may be physically
// checkpointed immediately before an atomic candidate replacement. The caller
// must hold the state-root persistence lock and must have closed every Store
// handle; SQLite cannot prove that process-level ownership from a pathname.
type QuiesceOptions struct {
	Path                  string
	ExpectedSchemaVersion int
	ExpectedHead          AuthorityHead
}

// Health is a process-lifetime latch. Critical mutation failures caused by a
// full, busy, corrupt, or I/O-failing SQLite store transition Ready to false;
// only closing and explicitly reopening the store can reset it.
type Health struct {
	Ready     bool
	Code      string
	BlockedAt time.Time
}

// StateDocument is the current revision of one scope- and kind-addressed JSON
// document. JSON contains verified stored bytes and UpdatedAt is the commit
// timestamp.
type StateDocument struct {
	ScopeKey  string
	Kind      string
	Revision  int64
	JSON      []byte
	UpdatedAt time.Time
}

// StateDocumentCAS requests a compare-and-swap update. ExpectedRevision zero
// creates a missing document at revision one; a positive value updates exactly
// that revision. UpdatedAtNotBefore can reject a commit whose clock would move
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
// compare-and-swap.
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
// payload digest recorded for it.
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
// are inclusive Unix milliseconds, AfterObservationID provides forward
// pagination, and a nil DecisionEligible includes both eligibility classes.
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
// ScopeKey.
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
// evidence.
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
// action.
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
// AfterEventSeq provides forward pagination; nil order-ID pointers omit those
// filters, while zero-valued time bounds are open.
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
	// frontier it validated. Nil leaves place/cancel/other actions unconditional.
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

// LegacyConsumedToken is a consumed canonical preview-token identifier and
// broker scope recovered during one-time legacy import.
type LegacyConsumedToken struct {
	Scope          BrokerScope
	PreviewTokenID string
	ConsumedAt     time.Time
}

// LegacyOrderFloor is a broker-scoped conservative order-ID floor recovered
// during one-time legacy import.
type LegacyOrderFloor struct {
	Scope BrokerScope
	Floor int64
}

// LegacyOrderImport is the complete one-time order-authority cutover input.
// SourceFingerprint makes replay of the same source idempotent and rejects a
// different source after import.
type LegacyOrderImport struct {
	SourceFingerprint string
	GlobalFloor       int64
	ScopedFloors      []LegacyOrderFloor
	ConsumedTokens    []LegacyConsumedToken
	Events            []OrderEventRecord
}

// LegacyOrderImportResult reports whether this call performed the import and
// the resulting event sequences and authority head.
type LegacyOrderImportResult struct {
	Imported  bool
	EventSeqs []int64
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
// hash mismatches are returned as errors rather than represented in this value.
type IntegrityReport struct {
	QuickCheckResults    []string
	ForeignKeyViolations []ForeignKeyViolation
}

// OK reports whether SQLite returned exactly one successful quick-check row
// and no foreign-key violations.
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
// The digest, rather than the file name or size, identifies a restatement.
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
// in the same transaction. PayloadJSON is retained byte-for-byte.
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
// head shared by its mutation batch.
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
// decision event.
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
// regime decision.
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
// transition event.
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
// event.
type CapitalEventProjection struct{ Kind, AmountBaseText, EffectiveAt, ReportID string }

// RiskPolicyEventProjection is the typed searchable projection of a risk-policy
// governance event.
type RiskPolicyEventProjection struct {
	Kind, PolicyID, PolicyFingerprint string
	PolicyVersion                     *int64
}

// ProposalOutcomeProjection is the typed searchable projection of a proposal
// outcome event.
type ProposalOutcomeProjection struct{ ProposalKey, Revision, Bucket, Symbol, SecType, Action, State string }
