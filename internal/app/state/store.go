package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Alert delivery modes control app-side notification eligibility without
// changing daemon policy or the durable occurrence record.
const (
	AlertModeNone        = "none"
	AlertModeActOnly     = "act_only"
	AlertModeWatchAndAct = "watch_and_act"
)

// Governance transport and delivery constants classify app-local Web Push
// attempts and aggregate delivery health.
const (
	GovernanceTransportAccepted       = "push_service_accepted"
	GovernanceTransportPartial        = "partial_acceptance"
	GovernanceTransportAllFailed      = "all_failed"
	GovernanceTransportNoSubscription = "no_subscription"
	GovernanceTransportMissingKeys    = "missing_keys"
	GovernanceTransportSenderMissing  = "sender_unavailable"
	GovernanceTransportReserved       = "attempt_reserved"
	GovernanceTransportInterrupted    = "interrupted_uncertain"
	GovernanceTransportTargetRetired  = "target_retired"
	GovernanceTransportDeadlineRetry  = "deadline_retry"
	GovernanceTransportCanceledRetry  = "canceled_retry"
	GovernanceTransportNetworkRetry   = "transport_retry"
	GovernanceTransportHTTPRetry      = "http_retry"
	GovernanceTransportHTTPRejected   = "http_rejected"
	// GovernanceTransportTimeoutRetry and the following legacy classes remain
	// readable for state written by the first app implementation; new transport
	// code uses the specific classes above.
	GovernanceTransportTimeoutRetry = "timeout_retry"
	GovernanceTransportRejected     = "rejected"
	GovernanceTransportDead         = "dead_subscription"
	GovernanceTransportStateWrite   = "state_write_failure"
	GovernanceTransportRecovery     = "recovery"
	GovernanceTransportSuppressed   = "suppressed"
	GovernanceTransportOverflow     = "overflow"

	GovernanceDeliveryHealthy     = "healthy"
	GovernanceDeliverySuppressed  = "suppressed"
	GovernanceDeliveryDegraded    = "degraded"
	GovernanceDeliveryUnavailable = "unavailable"
	GovernanceDeliveryOverflow    = "overflow"
)

// App-state errors describe fail-closed capacity, cursor, and persisted-state
// validation failures without exposing private record identity.
var (
	ErrGovernanceOverflow            = errors.New("governance evidence overflow")
	ErrAlertHistoryOverflow          = errors.New("alert history overflow: unread retention limit reached")
	ErrAttentionReadRegression       = errors.New("attention read cursor cannot regress")
	ErrAttentionReadBeyondHighWater  = errors.New("attention read cursor exceeds high-water sequence")
	ErrAttentionReferencesIncomplete = errors.New("attention references are incomplete through requested sequence")
	ErrAttentionSequenceExhausted    = errors.New("attention sequence exhausted")
	ErrInvalidPersistedState         = errors.New("invalid persisted app state")
)

// Attention kinds identify the two legacy inbox record families sharing the
// app's durable read cursor.
const (
	AttentionKindCanary     = "canary"
	AttentionKindGovernance = "governance"
)

// Governance delivery dispositions freeze whether an occurrence was eligible
// when the app first persisted it.
const (
	GovernanceDispositionEligible             = "eligible"
	GovernanceDispositionSuppressedAtCreation = "suppressed_at_creation"
	GovernanceDispositionLegacyUnknown        = "legacy_unknown"
)

const (
	governanceRetention = 90 * 24 * time.Hour
	// alertPreviousContextRetention expires read alert records that stopped
	// matching the live context (operator decision 2026-07-20: 14 days).
	// Unread records and still-matching records never expire.
	alertPreviousContextRetention = 14 * 24 * time.Hour
	// alertMatchStampInterval bounds LastMatchedAt refresh writes: matching
	// is observed on the canary cadence (about once a minute), but a
	// 14-day retention only needs hourly stamp granularity.
	alertMatchStampInterval   = time.Hour
	defaultGovernanceMaxItems = 4096
)

// Store serializes access to the app's private state.json and returns copies or
// redacted projections at its public read boundaries. Its zero value is not
// usable; callers open a store with [Open].
type Store struct {
	path                            string
	mu                              sync.Mutex
	data                            Data
	governanceMaxItems              int
	governanceMaxAttempts           int
	volatileHealth                  *GovernanceDeliveryHealth
	governanceInFlight              map[string]bool
	saveHook                        func(string) error
	saveObserver                    func()
	alertDeliveryMaxItems           int
	alertDeliveryInFlight           map[string]bool
	alertDeliveryVolatile           *AlertDeliveryHealth
	alertDeliveryVolatileGeneration uint64
	alertDeliveryQuarantine         *alertDeliveryQuarantine
	loadedAlertDeliveryRaw          json.RawMessage
	loadedAlertDeliveryDecodeErr    error
}

// Data is the persisted app-state envelope. AlertDelivery remains an internal
// independently versioned section even though the surrounding legacy fields
// are exported for JSON persistence and tests.
type Data struct {
	Devices                 []DeviceGrant               `json:"devices,omitempty"`
	AlertSettings           AlertSettings               `json:"alert_settings"`
	PushSubscriptions       []PushSubscription          `json:"push_subscriptions,omitempty"`
	AlertHistory            []AlertRecord               `json:"alert_history,omitempty"`
	VAPID                   *VAPIDKeys                  `json:"vapid,omitempty"`
	LastPush                *PushAttempt                `json:"last_push,omitempty"`
	ProposalAudit           []ProposalAuditItem         `json:"proposal_audit,omitempty"`
	RelayRoute              *RelayRoute                 `json:"relay_route,omitempty"`
	GovernanceOccurrences   []GovernanceOccurrence      `json:"governance_occurrences,omitempty"`
	GovernanceAttempts      []GovernanceAttempt         `json:"governance_attempts,omitempty"`
	GovernanceReceipts      []GovernanceReceipt         `json:"governance_receipts,omitempty"`
	GovernanceHealth        GovernanceDeliveryHealth    `json:"governance_delivery_health"`
	GovernanceTotals        GovernanceAttemptTotals     `json:"governance_attempt_totals"`
	GovernanceHealthTotals  GovernanceHealthEventTotals `json:"governance_health_event_totals"`
	DiagnosticStatus        GovernanceDiagnosticStatus  `json:"diagnostic_status"`
	AttentionHighWaterSeq   uint64                      `json:"attention_high_water_seq"`
	AttentionReadThroughSeq uint64                      `json:"attention_read_through_seq"`
	AlertDelivery           *alertDeliveryData          `json:"alert_delivery,omitempty"`
}

// DeviceGrant is an app-owned paired-device identity. RevokedAt is terminal:
// re-pairing creates a new identity instead of reviving this one.
type DeviceGrant struct {
	ID               string `json:"id"`
	Name             string `json:"name,omitempty"`
	PublicKeyJWK     string `json:"public_key_jwk,omitempty"`
	DeviceSecretHash string `json:"device_secret_hash,omitempty"`
	// DeviceCookieHashes authenticate the long-lived HttpOnly device
	// cookie. Cookies are the only client storage that provably survives
	// the iOS home-screen web-app container split (localStorage/IndexedDB
	// written by Safari never reach the installed app), so session
	// continuity must not depend on script-visible storage. A capped list,
	// not a single value: Safari and the installed app hold twin copies of
	// the cookie jar, so issuing a fresh cookie to one twin must never
	// invalidate the other.
	DeviceCookieHashes []string  `json:"device_cookie_hashes,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	LastSeenAt         time.Time `json:"last_seen_at,omitzero"`
	RevokedAt          time.Time `json:"revoked_at,omitzero"`
}

// RelayRoute stores the app connector's resumable remote-relay registration.
// ExpiresAt is informational because a token-matched reconnect may revive it.
type RelayRoute struct {
	RemoteURL      string    `json:"remote_url"`
	RouteID        string    `json:"route_id"`
	ConnectorToken string    `json:"connector_token"`
	PublicURL      string    `json:"public_url,omitempty"`
	ConnectorURL   string    `json:"connector_url,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// AlertSettings holds the operator-selected app notification mode.
type AlertSettings struct {
	Mode string `json:"mode"`
}

// PushSubscription is an app-owned Web Push target bound to one paired device.
// Its endpoint and keys are private transport material.
type PushSubscription struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"device_id"`
	Endpoint   string    `json:"endpoint"`
	P256DH     string    `json:"p256dh"`
	Auth       string    `json:"auth"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at,omitzero"`
}

// AlertRecord is a redacted durable inbox row. AttentionSeq zero identifies a
// legacy row outside the shared unread cursor.
type AlertRecord struct {
	ID          string    `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	Action      string    `json:"action,omitempty"`
	Severity    string    `json:"severity,omitempty"`
	Account     string    `json:"account,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
	// LastMatchedAt is refreshed while an observed canary still matches this
	// record's context (fingerprint for canary-source records, account/mode
	// for all). Previous-context expiry keys on it; records from before the
	// stamp existed fall back to CreatedAt.
	LastMatchedAt time.Time `json:"last_matched_at,omitzero"`
	AttentionSeq  uint64    `json:"attention_seq"`
}

// PushAttempt records one classified Web Push transport result. OK means the
// push service accepted the request, not that a device displayed it.
type PushAttempt struct {
	At             time.Time `json:"at"`
	SubscriptionID string    `json:"subscription_id,omitempty"`
	AlertID        string    `json:"alert_id,omitempty"`
	OK             bool      `json:"ok"`
	Status         string    `json:"status,omitempty"`
	Error          string    `json:"error,omitempty"`
	Class          string    `json:"class,omitempty"`
}

// GovernanceOccurrence is durable app transport state. Fingerprint is the
// daemon's opaque semantic identity and is never exposed by Governance().
type GovernanceOccurrence struct {
	Fingerprint         string    `json:"fingerprint"`
	DisplayID           string    `json:"display_id"`
	Kind                string    `json:"kind"`
	State               string    `json:"state"`
	Severity            string    `json:"severity"`
	Title               string    `json:"title"`
	Body                string    `json:"body"`
	Destination         string    `json:"destination"`
	OccurredAt          time.Time `json:"occurred_at"`
	DueAt               time.Time `json:"due_at,omitzero"`
	ExpiresAt           time.Time `json:"expires_at,omitzero"`
	FirstSeenAt         time.Time `json:"first_seen_at"`
	LastSeenAt          time.Time `json:"last_seen_at"`
	ResolvedAt          time.Time `json:"resolved_at,omitzero"`
	AttentionSeq        uint64    `json:"attention_seq"`
	DeliveryDisposition string    `json:"delivery_disposition"`
}

// AttentionRef identifies one redacted legacy inbox row without exposing its
// private fingerprint or transport identity.
type AttentionRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Attention is the shared durable unread cursor for the legacy Alerts inbox.
// Rows with sequence zero are intentionally excluded from UnreadCount.
type Attention struct {
	UnreadCount    int            `json:"unread_count"`
	HighWaterSeq   uint64         `json:"high_water_seq"`
	ReadThroughSeq uint64         `json:"read_through_seq"`
	UnreadRefs     []AttentionRef `json:"unread_refs"`
}

type attentionEntry struct {
	seq uint64
	ref AttentionRef
}

// GovernanceAttempt is durable app-local evidence for one occurrence-target
// transport decision. An incomplete reservation is uncertain after restart.
type GovernanceAttempt struct {
	ID             string    `json:"id"`
	OccurrenceID   string    `json:"occurrence_id,omitempty"`
	TargetRef      string    `json:"target_ref,omitempty"`
	ReceiptKey     string    `json:"receipt_key,omitempty"`
	At             time.Time `json:"at"`
	CompletedAt    time.Time `json:"completed_at,omitzero"`
	Class          string    `json:"class"`
	RetryAt        time.Time `json:"retry_at,omitzero"`
	RetiredAt      time.Time `json:"target_retired_at,omitzero"`
	TransportCount int       `json:"transport_count,omitempty"`
}

// GovernanceReceipt records push-service acceptance for one occurrence-target
// pair; it is not proof of device display or human attention.
type GovernanceReceipt struct {
	OccurrenceID string    `json:"occurrence_id"`
	TargetRef    string    `json:"target_ref"`
	ReceiptKey   string    `json:"receipt_key"`
	AcceptedAt   time.Time `json:"accepted_at"`
	ResolvedAt   time.Time `json:"resolved_at,omitzero"`
	RetiredAt    time.Time `json:"target_retired_at,omitzero"`
}

// GovernanceDeliveryHealth summarizes the legacy governance transport ledger.
// A zero LastAcceptedAt means no acceptance has been retained.
type GovernanceDeliveryHealth struct {
	State          string    `json:"state"`
	Class          string    `json:"class,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	LastAcceptedAt time.Time `json:"last_push_service_acceptance_at,omitzero"`
}

// GovernanceDiagnosticStatus stores the latest safe notification-test result.
type GovernanceDiagnosticStatus struct {
	State string    `json:"state,omitempty"`
	At    time.Time `json:"at,omitzero"`
}

// GovernanceCompletionDisposition reports whether a completed transport still
// belonged to its target or returned after target retirement.
type GovernanceCompletionDisposition string

// Governance completion dispositions distinguish an applied result from a
// late result for a retired target.
const (
	GovernanceCompletionApplied GovernanceCompletionDisposition = "applied"
	GovernanceCompletionRetired GovernanceCompletionDisposition = "retired"
)

// GovernanceCompletionOutcome reports how a completed governance reservation
// was reconciled with current durable target state.
type GovernanceCompletionOutcome struct {
	Disposition GovernanceCompletionDisposition
}

// GovernanceAttemptTotals contains cumulative durable transport dispositions;
// RetryPending is a current derived count rather than a persisted total.
type GovernanceAttemptTotals struct {
	CumulativeAttempts int `json:"cumulative_attempts"`
	Accepted           int `json:"push_service_accepted"`
	RetryableFailures  int `json:"retryable_failures"`
	Rejected           int `json:"rejected"`
	Dead               int `json:"dead_subscription"`
	Missed             int `json:"missed"`
	Suppressed         int `json:"suppressed"`
	Interrupted        int `json:"interrupted_uncertain"`
	TargetRetired      int `json:"target_retired"`
	RetryPending       int `json:"-"`

	// Legacy counters are accepted on load and migrated into the explicit
	// attempt/health split. They remain zero in newly written state.
	LegacyTotal         int `json:"total,omitempty"`
	LegacyPartial       int `json:"partial,omitempty"`
	LegacyRetryPending  int `json:"retry_pending,omitempty"`
	LegacyStateFailures int `json:"state_write_failure,omitempty"`
	LegacyRecovery      int `json:"recovery,omitempty"`
	LegacyOverflow      int `json:"overflow,omitempty"`
}

// GovernanceHealthEventTotals counts app-local delivery-health transitions
// separately from actual transport attempts.
type GovernanceHealthEventTotals struct {
	PartialEpisodes int `json:"partial_episodes"`
	StateFailures   int `json:"state_write_failures"`
	Recoveries      int `json:"recoveries"`
	Overflows       int `json:"overflows"`
}

// GovernanceOccurrenceView is the redacted operator projection of a durable
// governance occurrence; its producer fingerprint and attention sequence stay
// private.
type GovernanceOccurrenceView struct {
	DisplayID   string    `json:"display_id"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	Destination string    `json:"destination"`
	OccurredAt  time.Time `json:"occurred_at"`
	DueAt       time.Time `json:"due_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
	ResolvedAt  time.Time `json:"resolved_at,omitzero"`
}

// GovernanceAttemptView exposes classified timing without attempt IDs, receipt
// keys, subscription endpoints, or transport error text.
type GovernanceAttemptView struct {
	OccurrenceID   string    `json:"occurrence_id,omitempty"`
	TargetRef      string    `json:"target_ref,omitempty"`
	At             time.Time `json:"at"`
	CompletedAt    time.Time `json:"completed_at,omitzero"`
	Class          string    `json:"class"`
	RetryAt        time.Time `json:"retry_at,omitzero"`
	RetiredAt      time.Time `json:"target_retired_at,omitzero"`
	TransportCount int       `json:"transport_count,omitempty"`
}

// GovernanceReceiptView exposes acceptance timing without its private receipt
// key or subscription endpoint.
type GovernanceReceiptView struct {
	OccurrenceID string    `json:"occurrence_id"`
	TargetRef    string    `json:"target_ref"`
	AcceptedAt   time.Time `json:"accepted_at"`
	RetiredAt    time.Time `json:"target_retired_at,omitzero"`
}

// GovernanceView is the redacted app-local governance delivery projection.
type GovernanceView struct {
	Occurrences    []GovernanceOccurrenceView  `json:"occurrences"`
	Attempts       []GovernanceAttemptView     `json:"attempts"`
	Receipts       []GovernanceReceiptView     `json:"receipts"`
	DeliveryHealth GovernanceDeliveryHealth    `json:"delivery_health"`
	AttemptTotals  GovernanceAttemptTotals     `json:"attempt_totals"`
	HealthTotals   GovernanceHealthEventTotals `json:"health_event_totals"`
	Diagnostic     GovernanceDiagnosticStatus  `json:"diagnostic"`
}

// VAPIDKeys stores the app-owned signing key pair. PrivateKey must never cross
// an authenticated app response or logging boundary.
type VAPIDKeys struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

// ProposalAuditItem is a durable app-side audit row for paired-device proposal
// actions; Payload may contain private request data and is not a public DTO.
type ProposalAuditItem struct {
	ID        string          `json:"id"`
	DeviceID  string          `json:"device_id,omitempty"`
	Action    string          `json:"action,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Open loads or initializes the private app store under dir, validates its
// persisted invariants, and recovers interrupted delivery reservations. It
// quarantines an invalid optional alert-delivery ledger without fabricating a
// replacement authority.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state dir required")
	}
	s := &Store{path: filepath.Join(dir, "state.json"), governanceMaxItems: defaultGovernanceMaxItems, governanceMaxAttempts: defaultGovernanceMaxItems}
	s.initAlertDeliveryRuntime()
	if err := s.load(); err != nil {
		return nil, err
	}
	if s.data.AlertSettings.Mode == "" {
		s.data.AlertSettings.Mode = AlertModeWatchAndAct
	} else if !validAlertMode(s.data.AlertSettings.Mode) {
		return nil, fmt.Errorf("%w: invalid alert mode %q", ErrInvalidPersistedState, s.data.AlertSettings.Mode)
	}
	for i := range s.data.GovernanceOccurrences {
		disposition := s.data.GovernanceOccurrences[i].DeliveryDisposition
		if disposition == "" {
			s.data.GovernanceOccurrences[i].DeliveryDisposition = GovernanceDispositionLegacyUnknown
			continue
		}
		if !validGovernanceDisposition(disposition) {
			return nil, fmt.Errorf("%w: invalid governance delivery disposition %q", ErrInvalidPersistedState, disposition)
		}
	}
	if err := s.validateAttentionState(); err != nil {
		return nil, err
	}
	if s.loadedAlertDeliveryDecodeErr != nil {
		if err := s.quarantineLoadedAlertDelivery(s.loadedAlertDeliveryDecodeErr); err != nil {
			return nil, err
		}
	} else if err := s.validateAlertDeliveryState(); err != nil {
		if quarantineErr := s.quarantineLoadedAlertDelivery(err); quarantineErr != nil {
			return nil, quarantineErr
		}
	}
	if s.alertDeliveryQuarantinedLocked() {
		return s, nil
	}
	if s.data.AlertDelivery != nil && s.data.AlertDelivery.migratedLegacy {
		s.data.AlertDelivery.migratedLegacy = false
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("persist alert delivery ledger upgrade: %w", err)
		}
	}
	if err := s.RecoverAlertDeliveries(time.Now().UTC()); err != nil {
		if s.alertDeliveryStateWriteFailure() {
			return s, nil
		}
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read app state: %w", err)
	}
	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal(data, &topLevel); err != nil {
		return fmt.Errorf("decode app state: %w", err)
	}
	if topLevel == nil {
		return errors.New("decode app state: top-level JSON object required")
	}

	// Decode the top-level object a second time without alert_delivery. This
	// keeps a failure in the optional typed ledger from making the legacy
	// Canary authority unavailable, while every legacy field still uses its
	// normal typed decoder and remains fatal on corruption.
	rawAlertDelivery := append(json.RawMessage(nil), topLevel["alert_delivery"]...)
	delete(topLevel, "alert_delivery")
	legacyData, err := json.Marshal(topLevel)
	if err != nil {
		return fmt.Errorf("decode app state envelope: %w", err)
	}
	if err := json.Unmarshal(legacyData, &s.data); err != nil {
		return fmt.Errorf("decode app state: %w", err)
	}
	s.loadedAlertDeliveryRaw = rawAlertDelivery
	if len(rawAlertDelivery) > 0 && string(rawAlertDelivery) != "null" {
		var typed alertDeliveryData
		if err := json.Unmarshal(rawAlertDelivery, &typed); err != nil {
			s.loadedAlertDeliveryDecodeErr = fmt.Errorf("decode alert delivery state: %w", err)
		} else {
			s.data.AlertDelivery = &typed
		}
	}
	s.migrateGovernanceTotals()
	return nil
}

func (s *Store) migrateGovernanceTotals() {
	totals := &s.data.GovernanceTotals
	if totals.CumulativeAttempts == 0 && totals.LegacyTotal > 0 {
		healthEvents := totals.LegacyPartial + totals.LegacyStateFailures + totals.LegacyRecovery + totals.LegacyOverflow
		totals.CumulativeAttempts = max(0, totals.LegacyTotal-healthEvents)
	}
	if totals.RetryableFailures == 0 {
		totals.RetryableFailures = totals.LegacyRetryPending
	}
	s.data.GovernanceHealthTotals.PartialEpisodes += totals.LegacyPartial
	s.data.GovernanceHealthTotals.StateFailures += totals.LegacyStateFailures
	s.data.GovernanceHealthTotals.Recoveries += totals.LegacyRecovery
	s.data.GovernanceHealthTotals.Overflows += totals.LegacyOverflow
	totals.LegacyTotal = 0
	totals.LegacyPartial = 0
	totals.LegacyRetryPending = 0
	totals.LegacyStateFailures = 0
	totals.LegacyRecovery = 0
	totals.LegacyOverflow = 0
}

// AlertSettings returns the current app notification mode.
func (s *Store) AlertSettings() AlertSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AlertSettings
}

// Attention returns a snapshot of the legacy inbox's shared durable unread
// cursor and redacted unread references.
func (s *Store) Attention() Attention {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attentionLocked()
}

// MarkAttentionRead durably advances the shared read cursor to application
// render state reported by a client. It is not proof of human attention or
// physical delivery.
func (s *Store) MarkAttentionRead(throughSeq uint64) (Attention, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if throughSeq < s.data.AttentionReadThroughSeq {
		return s.attentionLocked(), ErrAttentionReadRegression
	}
	if throughSeq > s.data.AttentionHighWaterSeq {
		return s.attentionLocked(), ErrAttentionReadBeyondHighWater
	}
	if throughSeq > s.data.AttentionReadThroughSeq && !s.attentionReferencesCompleteThroughLocked(throughSeq) {
		return s.attentionLocked(), ErrAttentionReferencesIncomplete
	}
	if throughSeq == s.data.AttentionReadThroughSeq {
		return s.attentionLocked(), nil
	}
	prior := s.data.AttentionReadThroughSeq
	s.data.AttentionReadThroughSeq = throughSeq
	if err := s.save(); err != nil {
		s.data.AttentionReadThroughSeq = prior
		return s.attentionLocked(), err
	}
	return s.attentionLocked(), nil
}

func (s *Store) attentionLocked() Attention {
	entries := make([]attentionEntry, 0)
	for _, record := range s.data.AlertHistory {
		if record.AttentionSeq > s.data.AttentionReadThroughSeq && record.AttentionSeq <= s.data.AttentionHighWaterSeq {
			entries = append(entries, attentionEntry{seq: record.AttentionSeq, ref: AttentionRef{Kind: AttentionKindCanary, ID: record.ID}})
		}
	}
	for _, occurrence := range s.data.GovernanceOccurrences {
		if occurrence.AttentionSeq > s.data.AttentionReadThroughSeq && occurrence.AttentionSeq <= s.data.AttentionHighWaterSeq {
			entries = append(entries, attentionEntry{seq: occurrence.AttentionSeq, ref: AttentionRef{Kind: AttentionKindGovernance, ID: occurrence.DisplayID}})
		}
	}
	slices.SortFunc(entries, func(a, b attentionEntry) int {
		if order := cmp.Compare(a.seq, b.seq); order != 0 {
			return order
		}
		if order := cmp.Compare(a.ref.Kind, b.ref.Kind); order != 0 {
			return order
		}
		return cmp.Compare(a.ref.ID, b.ref.ID)
	})
	refs := make([]AttentionRef, len(entries))
	for i, entry := range entries {
		refs[i] = entry.ref
	}
	return Attention{UnreadCount: len(refs), HighWaterSeq: s.data.AttentionHighWaterSeq, ReadThroughSeq: s.data.AttentionReadThroughSeq, UnreadRefs: refs}
}

func (s *Store) attentionReferencesCompleteThroughLocked(throughSeq uint64) bool {
	seen := make(map[uint64]struct{})
	add := func(seq uint64) bool {
		if seq <= s.data.AttentionReadThroughSeq || seq > throughSeq {
			return true
		}
		if _, duplicate := seen[seq]; duplicate {
			return false
		}
		seen[seq] = struct{}{}
		return true
	}
	for _, record := range s.data.AlertHistory {
		if !add(record.AttentionSeq) {
			return false
		}
	}
	for _, occurrence := range s.data.GovernanceOccurrences {
		if !add(occurrence.AttentionSeq) {
			return false
		}
	}
	return uint64(len(seen)) == throughSeq-s.data.AttentionReadThroughSeq
}

func (s *Store) validateAttentionState() error {
	readThrough := s.data.AttentionReadThroughSeq
	highWater := s.data.AttentionHighWaterSeq
	if readThrough > highWater {
		return fmt.Errorf("%w: attention read-through %d exceeds high-water %d", ErrInvalidPersistedState, readThrough, highWater)
	}
	sequences := make(map[uint64]struct{})
	unreadRefs := make(map[AttentionRef]struct{})
	validate := func(seq uint64, ref AttentionRef) error {
		if seq == 0 {
			return nil
		}
		if seq > highWater {
			return fmt.Errorf("%w: attention sequence %d exceeds high-water %d", ErrInvalidPersistedState, seq, highWater)
		}
		if _, duplicate := sequences[seq]; duplicate {
			return fmt.Errorf("%w: duplicate attention sequence %d", ErrInvalidPersistedState, seq)
		}
		sequences[seq] = struct{}{}
		if seq <= readThrough {
			return nil
		}
		if strings.TrimSpace(ref.ID) == "" {
			return fmt.Errorf("%w: empty unread %s attention id", ErrInvalidPersistedState, ref.Kind)
		}
		if _, duplicate := unreadRefs[ref]; duplicate {
			return fmt.Errorf("%w: duplicate unread attention reference %s/%s", ErrInvalidPersistedState, ref.Kind, ref.ID)
		}
		unreadRefs[ref] = struct{}{}
		return nil
	}
	for _, record := range s.data.AlertHistory {
		if err := validate(record.AttentionSeq, AttentionRef{Kind: AttentionKindCanary, ID: record.ID}); err != nil {
			return err
		}
	}
	for _, occurrence := range s.data.GovernanceOccurrences {
		if err := validate(occurrence.AttentionSeq, AttentionRef{Kind: AttentionKindGovernance, ID: occurrence.DisplayID}); err != nil {
			return err
		}
	}
	if uint64(len(unreadRefs)) != highWater-readThrough {
		return fmt.Errorf("%w: attention sequence gap between read-through %d and high-water %d", ErrInvalidPersistedState, readThrough, highWater)
	}
	return nil
}

func (s *Store) nextAttentionSeqLocked() (uint64, error) {
	if s.data.AttentionHighWaterSeq == ^uint64(0) {
		return 0, ErrAttentionSequenceExhausted
	}
	s.data.AttentionHighWaterSeq++
	return s.data.AttentionHighWaterSeq, nil
}

// SetAlertMode validates and durably replaces the app notification mode. It
// does not change daemon policy or retroactively alter stored occurrences.
func (s *Store) SetAlertMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validAlertMode(mode) {
		return fmt.Errorf("invalid alert mode %q", mode)
	}
	prior := s.data.AlertSettings.Mode
	s.data.AlertSettings.Mode = mode
	if err := s.save(); err != nil {
		s.data.AlertSettings.Mode = prior
		return err
	}
	return nil
}

// AddDevice durably inserts or updates a paired device. Revocation atomically
// retires that device's targets in both delivery ledgers and cannot be undone
// by updating the same identity.
func (s *Store) AddDevice(d DeviceGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID == d.ID {
			priorDevice := s.data.Devices[i]
			if !priorDevice.RevokedAt.IsZero() && d.RevokedAt.IsZero() {
				return errors.New("revoked device identity cannot be reactivated; pair a new device")
			}
			priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
			priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
			priorTotals := s.data.GovernanceTotals
			priorAlertDelivery := s.data.AlertDelivery
			var alertRelease []string
			alertChanged := false
			if priorDevice.RevokedAt.IsZero() && !d.RevokedAt.IsZero() {
				governanceTargets := map[string]bool{}
				alertTargets := map[string]bool{}
				for _, subscription := range s.data.PushSubscriptions {
					if subscription.DeviceID != d.ID {
						continue
					}
					governanceTargets[GovernanceTargetRef(subscription.DeviceID, subscription.ID)] = true
					alertTargets[AlertDeliveryTargetRef(subscription.DeviceID, subscription.ID)] = true
				}
				var err error
				alertRelease, alertChanged, err = s.retireAlertDeliveryTargetsLocked(alertTargets, d.RevokedAt.UTC())
				if errors.Is(err, ErrAlertDeliveryOverflow) {
					return s.setAlertDeliveryOverflowLocked(priorAlertDelivery, d.RevokedAt.UTC())
				}
				if err != nil {
					return err
				}
				s.retireGovernanceTargetsLocked(governanceTargets, d.RevokedAt.UTC())
			}
			s.data.Devices[i] = d
			if err := s.save(); err != nil {
				s.data.Devices[i] = priorDevice
				s.data.GovernanceAttempts = priorAttempts
				s.data.GovernanceReceipts = priorReceipts
				s.data.GovernanceTotals = priorTotals
				s.data.AlertDelivery = priorAlertDelivery
				if alertChanged {
					s.noteAlertDeliverySaveFailureLocked(d.RevokedAt.UTC())
				}
				return err
			}
			s.finishAlertDeliveryRetirementLocked(alertRelease, alertChanged)
			return nil
		}
	}
	priorDevices := append([]DeviceGrant(nil), s.data.Devices...)
	s.data.Devices = append(s.data.Devices, d)
	if err := s.save(); err != nil {
		s.data.Devices = priorDevices
		return err
	}
	return nil
}

// Device returns the active paired device with id. Revoked and unknown devices
// both return false.
func (s *Store) Device(id string) (DeviceGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.data.Devices {
		if d.ID == id && d.RevokedAt.IsZero() {
			return d, true
		}
	}
	return DeviceGrant{}, false
}

// maxDeviceCookieHashes bounds the valid cookie generations per device:
// enough for a few Safari/home-screen twins plus re-provisioned logins,
// small enough that a leaked state file exposes a bounded credential set.
const maxDeviceCookieHashes = 5

// AddDeviceCookieHash retains a bounded set of cookie generations for one
// paired device so Safari and installed-app cookie jars can coexist.
func (s *Store) AddDeviceCookieHash(id, hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID != id {
			continue
		}
		hashes := s.data.Devices[i].DeviceCookieHashes
		if slices.Contains(hashes, hash) {
			return nil
		}
		hashes = append(hashes, hash)
		if len(hashes) > maxDeviceCookieHashes {
			hashes = hashes[len(hashes)-maxDeviceCookieHashes:]
		}
		s.data.Devices[i].DeviceCookieHashes = hashes
		return s.save()
	}
	return fmt.Errorf("device %s not found", id)
}

// Devices returns a shallow copy of all paired-device records, including
// revoked devices retained as audit state.
func (s *Store) Devices() []DeviceGrant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeviceGrant, len(s.data.Devices))
	copy(out, s.data.Devices)
	return out
}

// PruneDevices removes device grants whose last activity predates cutoff,
// along with their push subscriptions. Activity is the later of creation
// and last-seen, so a freshly paired but not-yet-used device survives.
func (s *Store) PruneDevices(cutoff time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := map[string]bool{}
	kept := make([]DeviceGrant, 0, len(s.data.Devices))
	for _, d := range s.data.Devices {
		last := d.LastSeenAt
		if d.CreatedAt.After(last) {
			last = d.CreatedAt
		}
		if last.Before(cutoff) {
			removed[d.ID] = true
			continue
		}
		kept = append(kept, d)
	}
	if len(removed) == 0 {
		return 0, nil
	}
	priorDevices := append([]DeviceGrant(nil), s.data.Devices...)
	priorSubscriptions := append([]PushSubscription(nil), s.data.PushSubscriptions...)
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorTotals := s.data.GovernanceTotals
	priorAlertDelivery := s.data.AlertDelivery
	governanceTargets := map[string]bool{}
	alertTargets := map[string]bool{}
	for _, sub := range s.data.PushSubscriptions {
		if removed[sub.DeviceID] {
			governanceTargets[GovernanceTargetRef(sub.DeviceID, sub.ID)] = true
			alertTargets[AlertDeliveryTargetRef(sub.DeviceID, sub.ID)] = true
		}
	}
	retiredAt := time.Now().UTC()
	alertRelease, alertChanged, err := s.retireAlertDeliveryTargetsLocked(alertTargets, retiredAt)
	if errors.Is(err, ErrAlertDeliveryOverflow) {
		return 0, s.setAlertDeliveryOverflowLocked(priorAlertDelivery, retiredAt)
	}
	if err != nil {
		return 0, err
	}
	s.data.Devices = kept
	s.data.PushSubscriptions = slices.DeleteFunc(s.data.PushSubscriptions, func(sub PushSubscription) bool {
		return removed[sub.DeviceID]
	})
	s.retireGovernanceTargetsLocked(governanceTargets, retiredAt)
	if err := s.save(); err != nil {
		s.data.Devices = priorDevices
		s.data.PushSubscriptions = priorSubscriptions
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		s.data.GovernanceTotals = priorTotals
		s.data.AlertDelivery = priorAlertDelivery
		if alertChanged {
			s.noteAlertDeliverySaveFailureLocked(retiredAt)
		}
		return 0, err
	}
	s.finishAlertDeliveryRetirementLocked(alertRelease, alertChanged)
	return len(removed), nil
}

// SetDeviceSeen durably records the supplied last-seen time for a known device.
func (s *Store) SetDeviceSeen(id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID == id {
			s.data.Devices[i].LastSeenAt = at
			return s.save()
		}
	}
	return fmt.Errorf("device %s not found", id)
}

// AddPushSubscription durably inserts or refreshes an app-owned push target.
// Moving an endpoint between devices requires a fresh, never-retired target
// identity and atomically retires the prior target's delivery evidence.
func (s *Store) AddPushSubscription(sub PushSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.PushSubscriptions {
		if s.data.PushSubscriptions[i].Endpoint == sub.Endpoint {
			priorSub := s.data.PushSubscriptions[i]
			transferring := priorSub.DeviceID != sub.DeviceID
			if transferring {
				if strings.TrimSpace(sub.DeviceID) == "" || strings.TrimSpace(sub.ID) == "" || sub.ID == priorSub.ID {
					return errors.New("cross-device endpoint transfer requires a fresh subscription identity")
				}
				if s.pushTargetIdentityInUseLocked(sub.DeviceID, sub.ID, i) {
					return errors.New("subscription target identity is already active")
				}
				if s.pushTargetIdentityRetiredLocked(sub.DeviceID, sub.ID) {
					return errors.New("subscription target identity was retired; create a fresh subscription")
				}
			} else {
				sub.ID = priorSub.ID
				sub.CreatedAt = priorSub.CreatedAt
			}
			priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
			priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
			priorTotals := s.data.GovernanceTotals
			priorAlertDelivery := s.data.AlertDelivery
			var alertRelease []string
			alertChanged := false
			retiredAt := sub.LastSeenAt.UTC()
			if retiredAt.IsZero() {
				retiredAt = time.Now().UTC()
			}
			if transferring {
				var err error
				alertRelease, alertChanged, err = s.retireAlertDeliveryTargetsLocked(map[string]bool{AlertDeliveryTargetRef(priorSub.DeviceID, priorSub.ID): true}, retiredAt)
				if errors.Is(err, ErrAlertDeliveryOverflow) {
					return s.setAlertDeliveryOverflowLocked(priorAlertDelivery, retiredAt)
				}
				if err != nil {
					return err
				}
				s.retireGovernanceTargetsLocked(map[string]bool{GovernanceTargetRef(priorSub.DeviceID, priorSub.ID): true}, retiredAt)
			}
			s.data.PushSubscriptions[i] = sub
			if err := s.save(); err != nil {
				s.data.PushSubscriptions[i] = priorSub
				s.data.GovernanceAttempts = priorAttempts
				s.data.GovernanceReceipts = priorReceipts
				s.data.GovernanceTotals = priorTotals
				s.data.AlertDelivery = priorAlertDelivery
				if alertChanged {
					s.noteAlertDeliverySaveFailureLocked(retiredAt)
				}
				return err
			}
			s.finishAlertDeliveryRetirementLocked(alertRelease, alertChanged)
			return nil
		}
	}
	if strings.TrimSpace(sub.DeviceID) == "" || strings.TrimSpace(sub.ID) == "" {
		return errors.New("push subscription device and identity required")
	}
	if s.pushTargetIdentityInUseLocked(sub.DeviceID, sub.ID, -1) {
		return errors.New("subscription target identity is already active")
	}
	if s.pushTargetIdentityRetiredLocked(sub.DeviceID, sub.ID) {
		return errors.New("subscription target identity was retired; create a fresh subscription")
	}
	priorSubscriptions := append([]PushSubscription(nil), s.data.PushSubscriptions...)
	s.data.PushSubscriptions = append(s.data.PushSubscriptions, sub)
	if err := s.save(); err != nil {
		s.data.PushSubscriptions = priorSubscriptions
		return err
	}
	return nil
}

func (s *Store) pushTargetIdentityInUseLocked(deviceID, subscriptionID string, except int) bool {
	for i, subscription := range s.data.PushSubscriptions {
		if i != except && subscription.DeviceID == deviceID && subscription.ID == subscriptionID {
			return true
		}
	}
	return false
}

func (s *Store) pushTargetIdentityRetiredLocked(deviceID, subscriptionID string) bool {
	if data := s.data.AlertDelivery; data != nil && !data.RetiredTargets[AlertDeliveryTargetRef(deviceID, subscriptionID)].IsZero() {
		return true
	}
	target := GovernanceTargetRef(deviceID, subscriptionID)
	for _, attempt := range s.data.GovernanceAttempts {
		if attempt.TargetRef == target && !attempt.RetiredAt.IsZero() {
			return true
		}
	}
	for _, receipt := range s.data.GovernanceReceipts {
		if receipt.TargetRef == target && !receipt.RetiredAt.IsZero() {
			return true
		}
	}
	return false
}

// PushSubscriptions returns a shallow copy of all retained subscriptions,
// including targets whose device activity is not checked by this legacy read.
func (s *Store) PushSubscriptions() []PushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]PushSubscription, len(s.data.PushSubscriptions))
	copy(out, s.data.PushSubscriptions)
	return out
}

// ActivePushSubscriptions returns subscriptions only for current, non-revoked
// paired devices. Governance delivery deliberately does not inherit Canary's
// looser historical subscription iteration.
func (s *Store) ActivePushSubscriptions() []PushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := make(map[string]bool, len(s.data.Devices))
	for _, device := range s.data.Devices {
		if device.RevokedAt.IsZero() {
			active[device.ID] = true
		}
	}
	out := make([]PushSubscription, 0, len(s.data.PushSubscriptions))
	for _, sub := range s.data.PushSubscriptions {
		if active[sub.DeviceID] {
			out = append(out, sub)
		}
	}
	return out
}

// ActivePushSubscriptionsForDevice returns subscriptions only when deviceID is
// a current, non-revoked paired device; otherwise it returns nil.
func (s *Store) ActivePushSubscriptionsForDevice(deviceID string) []PushSubscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := false
	for _, device := range s.data.Devices {
		if device.ID == deviceID && device.RevokedAt.IsZero() {
			active = true
			break
		}
	}
	if !active {
		return nil
	}
	out := make([]PushSubscription, 0)
	for _, sub := range s.data.PushSubscriptions {
		if sub.DeviceID == deviceID {
			out = append(out, sub)
		}
	}
	return out
}

// RemovePushSubscription retires a subscription at the current UTC time.
func (s *Store) RemovePushSubscription(id string) error {
	return s.RemovePushSubscriptionAt(id, time.Now().UTC())
}

// RemovePushSubscriptionAt atomically removes a subscription selected by ID or
// endpoint and retires its targets in both delivery ledgers. A zero retiredAt
// uses the current UTC time.
func (s *Store) RemovePushSubscriptionAt(id string, retiredAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	priorSubscriptions := append([]PushSubscription(nil), s.data.PushSubscriptions...)
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorTotals := s.data.GovernanceTotals
	priorAlertDelivery := s.data.AlertDelivery
	governanceTargets := map[string]bool{}
	alertTargets := map[string]bool{}
	for _, sub := range s.data.PushSubscriptions {
		if sub.ID == id || sub.Endpoint == id {
			governanceTargets[GovernanceTargetRef(sub.DeviceID, sub.ID)] = true
			alertTargets[AlertDeliveryTargetRef(sub.DeviceID, sub.ID)] = true
		}
	}
	if len(governanceTargets) == 0 {
		return nil
	}
	retiredAt = retiredAt.UTC()
	if retiredAt.IsZero() {
		retiredAt = time.Now().UTC()
	}
	alertRelease, alertChanged, err := s.retireAlertDeliveryTargetsLocked(alertTargets, retiredAt)
	if errors.Is(err, ErrAlertDeliveryOverflow) {
		return s.setAlertDeliveryOverflowLocked(priorAlertDelivery, retiredAt)
	}
	if err != nil {
		return err
	}
	s.data.PushSubscriptions = slices.DeleteFunc(s.data.PushSubscriptions, func(sub PushSubscription) bool {
		return sub.ID == id || sub.Endpoint == id
	})
	s.retireGovernanceTargetsLocked(governanceTargets, retiredAt)
	if err := s.save(); err != nil {
		s.data.PushSubscriptions = priorSubscriptions
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		s.data.GovernanceTotals = priorTotals
		s.data.AlertDelivery = priorAlertDelivery
		if alertChanged {
			s.noteAlertDeliverySaveFailureLocked(retiredAt)
		}
		return err
	}
	s.finishAlertDeliveryRetirementLocked(alertRelease, alertChanged)
	return nil
}

func (s *Store) retireGovernanceTargetsLocked(targets map[string]bool, retiredAt time.Time) {
	if retiredAt.IsZero() {
		retiredAt = time.Now().UTC()
	}
	for i := range s.data.GovernanceAttempts {
		attempt := &s.data.GovernanceAttempts[i]
		if !targets[attempt.TargetRef] || !attempt.RetiredAt.IsZero() {
			continue
		}
		attempt.RetiredAt = retiredAt
		attempt.RetryAt = time.Time{}
		if attempt.Class == GovernanceTransportReserved {
			attempt.Class = GovernanceTransportTargetRetired
			attempt.CompletedAt = retiredAt
			addGovernanceAttemptTotal(&s.data.GovernanceTotals, attempt.Class)
		}
	}
	for i := range s.data.GovernanceReceipts {
		receipt := &s.data.GovernanceReceipts[i]
		if targets[receipt.TargetRef] && receipt.RetiredAt.IsZero() {
			receipt.RetiredAt = retiredAt
		}
	}
}

// GovernanceTargetRef derives the opaque stable identity of an app-owned
// device/subscription pair.
func GovernanceTargetRef(deviceID, subscriptionID string) string {
	return governanceHash("target", strings.TrimSpace(deviceID), strings.TrimSpace(subscriptionID))
}

// GovernanceReceiptKey derives the opaque deduplication key for one legacy
// governance occurrence-target pair.
func GovernanceReceiptKey(occurrenceID, targetRef string) string {
	return governanceHash("receipt", strings.TrimSpace(occurrenceID), strings.TrimSpace(targetRef))
}

func governanceHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil))
}

func governanceDisplayID(fingerprint string, episode int, at time.Time) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", fingerprint, episode, at.UTC().Format(time.RFC3339Nano)))
	return fmt.Sprintf("gov-%x", sum[:8])
}

// UpsertGovernanceOccurrence records one non-authoritative observation and
// reports the active durable occurrence plus whether a new episode was created.
func (s *Store) UpsertGovernanceOccurrence(rec GovernanceOccurrence, now time.Time) (GovernanceOccurrence, bool, error) {
	if strings.TrimSpace(rec.Fingerprint) == "" {
		return GovernanceOccurrence{}, false, errors.New("governance occurrence fingerprint required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observed, created, err := s.observeGovernanceOccurrencesLocked([]GovernanceOccurrence{rec}, false, now.UTC())
	if err != nil {
		return GovernanceOccurrence{}, false, err
	}
	if len(observed) != 1 {
		return GovernanceOccurrence{}, false, errors.New("governance occurrence was not active at observation time")
	}
	return observed[0], created[0], nil
}

// ObserveGovernanceOccurrences applies one daemon observation in a single
// durable update. Identical active rows do not churn LastSeenAt; a fingerprint
// that was previously resolved starts a distinct app delivery episode.
func (s *Store) ObserveGovernanceOccurrences(records []GovernanceOccurrence, authoritative bool, now time.Time) ([]GovernanceOccurrence, error) {
	now = now.UTC()
	for _, rec := range records {
		if strings.TrimSpace(rec.Fingerprint) == "" {
			return nil, errors.New("governance occurrence fingerprint required")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observed, _, err := s.observeGovernanceOccurrencesLocked(records, authoritative, now)
	return observed, err
}

func (s *Store) observeGovernanceOccurrencesLocked(records []GovernanceOccurrence, authoritative bool, now time.Time) ([]GovernanceOccurrence, []bool, error) {
	priorOccurrences := append([]GovernanceOccurrence(nil), s.data.GovernanceOccurrences...)
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorHighWater := s.data.AttentionHighWaterSeq
	priorReadThrough := s.data.AttentionReadThroughSeq
	changed := false
	active := make(map[string]bool, len(records))
	resolvedIDs := map[string]bool{}
	result := make([]GovernanceOccurrence, 0, len(records))
	created := make([]bool, 0, len(records))
	for i := range s.data.GovernanceOccurrences {
		occurrence := &s.data.GovernanceOccurrences[i]
		if occurrence.ResolvedAt.IsZero() && !occurrence.ExpiresAt.IsZero() && !now.Before(occurrence.ExpiresAt) {
			occurrence.ResolvedAt = now
			resolvedIDs[occurrence.DisplayID] = true
			changed = true
		}
	}
	for _, incoming := range records {
		expired := !incoming.ExpiresAt.IsZero() && !now.Before(incoming.ExpiresAt)
		if !expired {
			active[incoming.Fingerprint] = true
		}
		found := -1
		episodes := 0
		for i := range s.data.GovernanceOccurrences {
			if s.data.GovernanceOccurrences[i].Fingerprint != incoming.Fingerprint {
				continue
			}
			episodes++
			if s.data.GovernanceOccurrences[i].ResolvedAt.IsZero() {
				found = i
			}
		}
		if found >= 0 {
			prior := s.data.GovernanceOccurrences[found]
			incoming.AttentionSeq = prior.AttentionSeq
			incoming.DeliveryDisposition = prior.DeliveryDisposition
			incoming.DisplayID = prior.DisplayID
			incoming.FirstSeenAt = prior.FirstSeenAt
			incoming.LastSeenAt = prior.LastSeenAt
			incoming.ResolvedAt = time.Time{}
			if expired {
				incoming.ResolvedAt = now
				resolvedIDs[incoming.DisplayID] = true
			}
			if !sameGovernanceOccurrenceSemantics(prior, incoming) {
				incoming.LastSeenAt = now
				s.data.GovernanceOccurrences[found] = incoming
				changed = true
			} else {
				incoming = prior
			}
			if !expired {
				result = append(result, incoming)
				created = append(created, false)
			}
			continue
		}
		if expired {
			continue
		}
		if s.governanceMaxItems <= 0 {
			s.governanceMaxItems = defaultGovernanceMaxItems
		}
		if len(s.data.GovernanceOccurrences) >= s.governanceMaxItems {
			s.data.GovernanceOccurrences = priorOccurrences
			s.data.GovernanceAttempts = priorAttempts
			s.data.GovernanceReceipts = priorReceipts
			s.data.AttentionHighWaterSeq = priorHighWater
			s.data.AttentionReadThroughSeq = priorReadThrough
			return nil, nil, s.setGovernanceOverflowLocked(now)
		}
		attentionSeq, err := s.nextAttentionSeqLocked()
		if err != nil {
			s.data.GovernanceOccurrences = priorOccurrences
			s.data.GovernanceAttempts = priorAttempts
			s.data.GovernanceReceipts = priorReceipts
			s.data.AttentionHighWaterSeq = priorHighWater
			s.data.AttentionReadThroughSeq = priorReadThrough
			return nil, nil, err
		}
		incoming.AttentionSeq = attentionSeq
		incoming.DeliveryDisposition = GovernanceDispositionEligible
		if s.data.AlertSettings.Mode == AlertModeNone {
			incoming.DeliveryDisposition = GovernanceDispositionSuppressedAtCreation
		}
		incoming.DisplayID = governanceDisplayID(incoming.Fingerprint, episodes+1, now)
		incoming.FirstSeenAt = now
		incoming.LastSeenAt = now
		incoming.ResolvedAt = time.Time{}
		s.data.GovernanceOccurrences = append(s.data.GovernanceOccurrences, incoming)
		result = append(result, incoming)
		created = append(created, true)
		changed = true
	}
	if authoritative {
		for i := range s.data.GovernanceOccurrences {
			occurrence := &s.data.GovernanceOccurrences[i]
			if !active[occurrence.Fingerprint] && occurrence.ResolvedAt.IsZero() {
				occurrence.ResolvedAt = now
				resolvedIDs[occurrence.DisplayID] = true
				changed = true
			}
		}
	}
	for i := range s.data.GovernanceReceipts {
		if resolvedIDs[s.data.GovernanceReceipts[i].OccurrenceID] && s.data.GovernanceReceipts[i].ResolvedAt.IsZero() {
			s.data.GovernanceReceipts[i].ResolvedAt = now
			changed = true
		}
	}
	for i := range s.data.GovernanceAttempts {
		attempt := &s.data.GovernanceAttempts[i]
		if resolvedIDs[attempt.OccurrenceID] && !attempt.RetryAt.IsZero() {
			attempt.RetryAt = time.Time{}
			changed = true
		}
	}
	if !changed {
		if s.hasVolatileGovernanceWriteFailureLocked() {
			if err := s.saveGovernanceLocked(now); err != nil {
				return nil, nil, err
			}
		}
		return result, created, nil
	}
	if err := s.saveGovernanceLocked(now); err != nil {
		s.data.GovernanceOccurrences = priorOccurrences
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		s.data.AttentionHighWaterSeq = priorHighWater
		s.data.AttentionReadThroughSeq = priorReadThrough
		return nil, nil, err
	}
	return result, created, nil
}

func sameGovernanceOccurrenceSemantics(a, b GovernanceOccurrence) bool {
	return a.Fingerprint == b.Fingerprint && a.DisplayID == b.DisplayID && a.Kind == b.Kind && a.State == b.State &&
		a.Severity == b.Severity && a.Title == b.Title && a.Body == b.Body && a.Destination == b.Destination &&
		a.OccurredAt.Equal(b.OccurredAt) && a.DueAt.Equal(b.DueAt) && a.ExpiresAt.Equal(b.ExpiresAt) && a.FirstSeenAt.Equal(b.FirstSeenAt) &&
		a.ResolvedAt.Equal(b.ResolvedAt) && a.AttentionSeq == b.AttentionSeq &&
		a.DeliveryDisposition == b.DeliveryDisposition
}

// ResolveGovernanceOccurrences ends active occurrences omitted from the given
// fingerprint set and stops their pending retries. The fingerprints remain
// private and never enter [GovernanceView].
func (s *Store) ResolveGovernanceOccurrences(activeFingerprints []string, now time.Time) error {
	now = now.UTC()
	active := make(map[string]bool, len(activeFingerprints))
	for _, fingerprint := range activeFingerprints {
		active[fingerprint] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	priorOccurrences := append([]GovernanceOccurrence(nil), s.data.GovernanceOccurrences...)
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	resolvedIDs := map[string]bool{}
	for i := range s.data.GovernanceOccurrences {
		occurrence := &s.data.GovernanceOccurrences[i]
		if !active[occurrence.Fingerprint] && occurrence.ResolvedAt.IsZero() {
			occurrence.ResolvedAt = now
			resolvedIDs[occurrence.DisplayID] = true
		}
	}
	for i := range s.data.GovernanceReceipts {
		if resolvedIDs[s.data.GovernanceReceipts[i].OccurrenceID] && s.data.GovernanceReceipts[i].ResolvedAt.IsZero() {
			s.data.GovernanceReceipts[i].ResolvedAt = now
		}
	}
	for i := range s.data.GovernanceAttempts {
		if resolvedIDs[s.data.GovernanceAttempts[i].OccurrenceID] {
			s.data.GovernanceAttempts[i].RetryAt = time.Time{}
		}
	}
	if len(resolvedIDs) == 0 {
		if s.hasVolatileGovernanceWriteFailureLocked() {
			return s.saveGovernanceLocked(now)
		}
		return nil
	}
	if err := s.saveGovernanceLocked(now); err != nil {
		s.data.GovernanceOccurrences = priorOccurrences
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		return err
	}
	return nil
}

// ReserveGovernanceAttempt durably reserves both the attempt row and the
// possible acceptance receipt before any external push transport is called.
func (s *Store) ReserveGovernanceAttempt(occurrenceID, targetRef string, now time.Time) (GovernanceAttempt, bool, error) {
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	active := false
	for _, occurrence := range s.data.GovernanceOccurrences {
		if occurrence.DisplayID == occurrenceID && occurrence.ResolvedAt.IsZero() && (occurrence.ExpiresAt.IsZero() || now.Before(occurrence.ExpiresAt)) {
			active = true
			break
		}
	}
	if !active {
		return GovernanceAttempt{}, false, nil
	}
	receiptKey := GovernanceReceiptKey(occurrenceID, targetRef)
	for _, receipt := range s.data.GovernanceReceipts {
		if receipt.ReceiptKey == receiptKey && receipt.ResolvedAt.IsZero() && receipt.RetiredAt.IsZero() {
			return GovernanceAttempt{}, false, nil
		}
	}
	latest := -1
	for i := range s.data.GovernanceAttempts {
		if s.data.GovernanceAttempts[i].ReceiptKey == receiptKey && s.data.GovernanceAttempts[i].RetiredAt.IsZero() {
			latest = i
		}
	}
	if latest >= 0 {
		attempt := s.data.GovernanceAttempts[latest]
		if !isGovernanceRetryable(attempt.Class) || attempt.RetryAt.IsZero() || now.Before(attempt.RetryAt) {
			return attempt, false, nil
		}
		if attempt.Class == GovernanceTransportReserved {
			if s.governanceMaxAttempts <= 0 {
				s.governanceMaxAttempts = defaultGovernanceMaxItems
			}
			if len(s.data.GovernanceAttempts) >= s.governanceMaxAttempts {
				return GovernanceAttempt{}, false, s.setGovernanceOverflowLocked(now)
			}
			priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
			priorTotals := s.data.GovernanceTotals
			attempt.Class = GovernanceTransportInterrupted
			attempt.CompletedAt = now
			attempt.RetryAt = time.Time{}
			s.data.GovernanceAttempts[latest] = attempt
			addGovernanceAttemptTotal(&s.data.GovernanceTotals, attempt.Class)
			fresh := newGovernanceReservation(s.data.GovernanceAttempts, occurrenceID, targetRef, receiptKey, now)
			s.data.GovernanceAttempts = append(s.data.GovernanceAttempts, fresh)
			if err := s.saveGovernanceLocked(now); err != nil {
				s.data.GovernanceAttempts = priorAttempts
				s.data.GovernanceTotals = priorTotals
				return GovernanceAttempt{}, false, err
			}
			return fresh, true, nil
		}
	}
	if s.governanceMaxAttempts <= 0 {
		s.governanceMaxAttempts = defaultGovernanceMaxItems
	}
	if s.governanceMaxItems <= 0 {
		s.governanceMaxItems = defaultGovernanceMaxItems
	}
	pendingReceipts := s.bindingGovernanceReceiptReservationsLocked(now)
	if len(s.data.GovernanceAttempts) >= s.governanceMaxAttempts || len(s.data.GovernanceReceipts)+pendingReceipts >= s.governanceMaxItems {
		return GovernanceAttempt{}, false, s.setGovernanceOverflowLocked(now)
	}
	attempt := newGovernanceReservation(s.data.GovernanceAttempts, occurrenceID, targetRef, receiptKey, now)
	s.data.GovernanceAttempts = append(s.data.GovernanceAttempts, attempt)
	if err := s.saveGovernanceLocked(now); err != nil {
		s.data.GovernanceAttempts = s.data.GovernanceAttempts[:len(s.data.GovernanceAttempts)-1]
		return GovernanceAttempt{}, false, err
	}
	return attempt, true, nil
}

func newGovernanceReservation(attempts []GovernanceAttempt, occurrenceID, targetRef, receiptKey string, now time.Time) GovernanceAttempt {
	return GovernanceAttempt{
		ID:           governanceHash("attempt", occurrenceID, targetRef, now.Format(time.RFC3339Nano), fmt.Sprint(len(attempts)+1)),
		OccurrenceID: occurrenceID, TargetRef: targetRef, ReceiptKey: receiptKey, At: now,
		Class: GovernanceTransportReserved, RetryAt: now.Add(governanceRetryBackoff(attempts, receiptKey)), TransportCount: 1,
	}
}

// BeginGovernanceTransport marks the short volatile interval in which an
// external sender may still return after its durable target is retired. A
// process restart clears this marker, so abandoned reservations do not retain
// receipt capacity merely because their audit row remains.
func (s *Store) BeginGovernanceTransport(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, attempt := range s.data.GovernanceAttempts {
		if attempt.ID != id {
			continue
		}
		if attempt.Class != GovernanceTransportReserved || !attempt.RetiredAt.IsZero() {
			return false
		}
		if s.governanceInFlight == nil {
			s.governanceInFlight = map[string]bool{}
		}
		s.governanceInFlight[id] = true
		return true
	}
	return false
}

func (s *Store) bindingGovernanceReceiptReservationsLocked(now time.Time) int {
	active := map[string]bool{}
	for _, occurrence := range s.data.GovernanceOccurrences {
		if occurrence.ResolvedAt.IsZero() && (occurrence.ExpiresAt.IsZero() || now.Before(occurrence.ExpiresAt)) {
			active[occurrence.DisplayID] = true
		}
	}
	pending := 0
	for _, attempt := range s.data.GovernanceAttempts {
		switch {
		case attempt.Class == GovernanceTransportReserved && attempt.RetiredAt.IsZero() && active[attempt.OccurrenceID]:
			pending++
		case attempt.Class == GovernanceTransportTargetRetired && s.governanceInFlight[attempt.ID]:
			pending++
		}
	}
	return pending
}

// CompleteGovernanceAttempt updates an existing reservation, so completion can
// never fail merely because a new evidence row no longer fits.
func (s *Store) CompleteGovernanceAttempt(id, class string, accepted bool, now time.Time) (GovernanceCompletionOutcome, error) {
	now = now.UTC()
	if !validGovernanceTransportClass(class) || class == GovernanceTransportReserved {
		return GovernanceCompletionOutcome{}, errors.New("invalid governance completion class")
	}
	if accepted != (class == GovernanceTransportAccepted) {
		return GovernanceCompletionOutcome{}, errors.New("governance acceptance/class mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index := -1
	for i := range s.data.GovernanceAttempts {
		if s.data.GovernanceAttempts[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return GovernanceCompletionOutcome{}, errors.New("governance reservation not found")
	}
	defer delete(s.governanceInFlight, id)
	priorAttempt := s.data.GovernanceAttempts[index]
	disposition := GovernanceCompletionApplied
	if !priorAttempt.RetiredAt.IsZero() {
		disposition = GovernanceCompletionRetired
	}
	if priorAttempt.Class != GovernanceTransportReserved && priorAttempt.Class != GovernanceTransportTargetRetired {
		return GovernanceCompletionOutcome{Disposition: disposition}, nil
	}
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorTotals := s.data.GovernanceTotals
	attempt := priorAttempt
	attempt.Class = class
	attempt.CompletedAt = now
	attempt.RetryAt = time.Time{}
	if isGovernanceRetryable(class) && disposition == GovernanceCompletionApplied {
		attempt.RetryAt = now.Add(governanceRetryBackoff(s.data.GovernanceAttempts, attempt.ReceiptKey))
	}
	s.data.GovernanceAttempts[index] = attempt
	if priorAttempt.Class == GovernanceTransportTargetRetired {
		removeGovernanceAttemptTotal(&s.data.GovernanceTotals, GovernanceTransportTargetRetired)
	}
	addGovernanceAttemptTotal(&s.data.GovernanceTotals, class)
	if accepted {
		s.data.GovernanceReceipts = append(s.data.GovernanceReceipts, GovernanceReceipt{
			OccurrenceID: attempt.OccurrenceID, TargetRef: attempt.TargetRef, ReceiptKey: attempt.ReceiptKey, AcceptedAt: now, RetiredAt: attempt.RetiredAt,
		})
	}
	if err := s.saveGovernanceLocked(now); err != nil {
		s.data.GovernanceAttempts[index] = priorAttempt
		s.data.GovernanceReceipts = priorReceipts
		s.data.GovernanceTotals = priorTotals
		return GovernanceCompletionOutcome{}, err
	}
	return GovernanceCompletionOutcome{Disposition: disposition}, nil
}

// RecordGovernanceAttempt appends a completed legacy transport decision and,
// when accepted is true, its deduplicating receipt in the same durable write.
func (s *Store) RecordGovernanceAttempt(attempt GovernanceAttempt, accepted bool) error {
	if attempt.At.IsZero() || !validGovernanceTransportClass(attempt.Class) {
		return errors.New("invalid governance attempt")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if accepted && attempt.Class != GovernanceTransportAccepted {
		return errors.New("accepted governance attempt must use push_service_accepted")
	}
	if accepted {
		for _, receipt := range s.data.GovernanceReceipts {
			if receipt.ReceiptKey == attempt.ReceiptKey {
				return nil
			}
		}
	}
	if s.governanceMaxAttempts <= 0 {
		s.governanceMaxAttempts = defaultGovernanceMaxItems
	}
	if len(s.data.GovernanceAttempts) >= s.governanceMaxAttempts {
		return s.setGovernanceOverflowLocked(attempt.At)
	}
	if accepted && len(s.data.GovernanceReceipts) >= s.governanceMaxItems {
		return s.setGovernanceOverflowLocked(attempt.At)
	}
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorTotals := s.data.GovernanceTotals
	if attempt.ID == "" {
		attempt.ID = governanceHash("attempt", attempt.OccurrenceID, attempt.TargetRef, attempt.At.UTC().Format(time.RFC3339Nano), fmt.Sprint(len(s.data.GovernanceAttempts)+1))
	}
	attempt.CompletedAt = attempt.At.UTC()
	if isGovernanceRetryable(attempt.Class) {
		attempt.RetryAt = attempt.At.Add(governanceRetryBackoff(s.data.GovernanceAttempts, attempt.ReceiptKey))
	}
	s.data.GovernanceAttempts = append(s.data.GovernanceAttempts, attempt)
	addGovernanceAttemptTotal(&s.data.GovernanceTotals, attempt.Class)
	if accepted {
		s.data.GovernanceReceipts = append(s.data.GovernanceReceipts, GovernanceReceipt{
			OccurrenceID: attempt.OccurrenceID, TargetRef: attempt.TargetRef, ReceiptKey: attempt.ReceiptKey, AcceptedAt: attempt.At,
		})
	}
	if err := s.saveGovernanceLocked(attempt.At); err != nil {
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		s.data.GovernanceTotals = priorTotals
		return err
	}
	return nil
}

func governanceRetryBackoff(attempts []GovernanceAttempt, receiptKey string) time.Duration {
	count := 0
	for _, attempt := range attempts {
		if attempt.ReceiptKey == receiptKey && attempt.Class != GovernanceTransportReserved && attempt.RetiredAt.IsZero() {
			count++
		}
	}
	backoffs := [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	if count >= len(backoffs) {
		return backoffs[len(backoffs)-1]
	}
	return backoffs[count]
}

// GovernanceAttemptDue reports whether an occurrence-target pair lacks an
// active receipt and has no attempt or a retry whose deadline has arrived.
func (s *Store) GovernanceAttemptDue(receiptKey string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, receipt := range s.data.GovernanceReceipts {
		if receipt.ReceiptKey == receiptKey && receipt.ResolvedAt.IsZero() && receipt.RetiredAt.IsZero() {
			return false
		}
	}
	var latest *GovernanceAttempt
	for i := range s.data.GovernanceAttempts {
		if s.data.GovernanceAttempts[i].ReceiptKey == receiptKey && s.data.GovernanceAttempts[i].RetiredAt.IsZero() {
			latest = &s.data.GovernanceAttempts[i]
		}
	}
	if latest == nil {
		return true
	}
	return isGovernanceRetryable(latest.Class) && !latest.RetryAt.IsZero() && !now.Before(latest.RetryAt)
}

func isGovernanceRetryable(class string) bool {
	switch class {
	case GovernanceTransportReserved, GovernanceTransportDeadlineRetry, GovernanceTransportCanceledRetry,
		GovernanceTransportNetworkRetry, GovernanceTransportHTTPRetry,
		GovernanceTransportMissingKeys, GovernanceTransportSenderMissing,
		GovernanceTransportTimeoutRetry:
		return true
	default:
		return false
	}
}

// HasGovernanceReceipt reports whether receiptKey has a current, non-retired
// push-service acceptance receipt.
func (s *Store) HasGovernanceReceipt(receiptKey string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, receipt := range s.data.GovernanceReceipts {
		if receipt.ReceiptKey == receiptKey && receipt.ResolvedAt.IsZero() && receipt.RetiredAt.IsZero() {
			return true
		}
	}
	return false
}

// SetGovernanceDeliveryHealth validates and persists aggregate app-local
// governance transport health while preserving the last acceptance timestamp
// when the caller leaves it zero.
func (s *Store) SetGovernanceDeliveryHealth(health GovernanceDeliveryHealth) error {
	if !validGovernanceDeliveryHealth(health) {
		return errors.New("invalid governance delivery health")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.data.GovernanceHealth
	priorHealthTotals := s.data.GovernanceHealthTotals
	if health.LastAcceptedAt.IsZero() {
		health.LastAcceptedAt = prior.LastAcceptedAt
	}
	if s.volatileHealth == nil && health.State == prior.State && health.Class == prior.Class && health.LastAcceptedAt.Equal(prior.LastAcceptedAt) {
		return nil
	}
	if health.Class == GovernanceTransportPartial {
		s.data.GovernanceHealthTotals.PartialEpisodes++
	}
	s.data.GovernanceHealth = health
	if err := s.saveGovernanceLocked(health.UpdatedAt); err != nil {
		s.data.GovernanceHealth = prior
		s.data.GovernanceHealthTotals = priorHealthTotals
		return err
	}
	s.volatileHealth = nil
	return nil
}

// RecordDiagnosticStatus validates and persists the latest safe notification
// test result.
func (s *Store) RecordDiagnosticStatus(status GovernanceDiagnosticStatus) error {
	if status.At.IsZero() || !validDiagnosticState(status.State) {
		return errors.New("invalid diagnostic status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.data.DiagnosticStatus
	s.data.DiagnosticStatus = status
	if err := s.saveGovernanceLocked(status.At); err != nil {
		s.data.DiagnosticStatus = prior
		return err
	}
	return nil
}

// Governance returns a redacted snapshot of app-local legacy governance
// occurrences and delivery evidence. RetryPending is derived at now.
func (s *Store) Governance(now time.Time) GovernanceView {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := GovernanceView{
		Occurrences:    make([]GovernanceOccurrenceView, 0, len(s.data.GovernanceOccurrences)),
		Attempts:       make([]GovernanceAttemptView, 0, len(s.data.GovernanceAttempts)),
		Receipts:       make([]GovernanceReceiptView, 0, len(s.data.GovernanceReceipts)),
		DeliveryHealth: s.data.GovernanceHealth,
		AttemptTotals:  s.data.GovernanceTotals,
		HealthTotals:   s.data.GovernanceHealthTotals,
		Diagnostic:     s.data.DiagnosticStatus,
	}
	if s.volatileHealth != nil {
		view.DeliveryHealth = *s.volatileHealth
	}
	for _, occurrence := range s.data.GovernanceOccurrences {
		view.Occurrences = append(view.Occurrences, GovernanceOccurrenceView{
			DisplayID: occurrence.DisplayID, Kind: occurrence.Kind, State: occurrence.State, Severity: occurrence.Severity,
			Title: occurrence.Title, Body: occurrence.Body, Destination: occurrence.Destination, OccurredAt: occurrence.OccurredAt,
			DueAt: occurrence.DueAt, ExpiresAt: occurrence.ExpiresAt, FirstSeenAt: occurrence.FirstSeenAt, LastSeenAt: occurrence.LastSeenAt, ResolvedAt: occurrence.ResolvedAt,
		})
	}
	for _, attempt := range s.data.GovernanceAttempts {
		view.Attempts = append(view.Attempts, GovernanceAttemptView{
			OccurrenceID: attempt.OccurrenceID, TargetRef: attempt.TargetRef, At: attempt.At, CompletedAt: attempt.CompletedAt,
			Class: attempt.Class, RetryAt: attempt.RetryAt, RetiredAt: attempt.RetiredAt, TransportCount: attempt.TransportCount,
		})
	}
	for _, receipt := range s.data.GovernanceReceipts {
		view.Receipts = append(view.Receipts, GovernanceReceiptView{OccurrenceID: receipt.OccurrenceID, TargetRef: receipt.TargetRef, AcceptedAt: receipt.AcceptedAt, RetiredAt: receipt.RetiredAt})
	}
	view.AttemptTotals.RetryPending = s.currentGovernanceRetryPendingLocked(now.UTC())
	return view
}

func (s *Store) currentGovernanceRetryPendingLocked(now time.Time) int {
	activeOccurrences := map[string]bool{}
	for _, occurrence := range s.data.GovernanceOccurrences {
		if occurrence.ResolvedAt.IsZero() && (occurrence.ExpiresAt.IsZero() || now.Before(occurrence.ExpiresAt)) {
			activeOccurrences[occurrence.DisplayID] = true
		}
	}
	accepted := map[string]bool{}
	for _, receipt := range s.data.GovernanceReceipts {
		if receipt.ResolvedAt.IsZero() && receipt.RetiredAt.IsZero() {
			accepted[receipt.ReceiptKey] = true
		}
	}
	latest := map[string]GovernanceAttempt{}
	for _, attempt := range s.data.GovernanceAttempts {
		if attempt.RetiredAt.IsZero() {
			latest[attempt.ReceiptKey] = attempt
		}
	}
	pending := 0
	for receiptKey, attempt := range latest {
		if activeOccurrences[attempt.OccurrenceID] && !accepted[receiptKey] && isGovernanceRetryable(attempt.Class) && !attempt.RetryAt.IsZero() {
			pending++
		}
	}
	return pending
}

// CompactGovernance removes read, resolved occurrences and retired transport
// evidence older than the retention window while retaining unread evidence.
func (s *Store) CompactGovernance(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := now.UTC().Add(-governanceRetention)
	removed := map[string]bool{}
	unreadOccurrences := map[string]bool{}
	occurrences := make([]GovernanceOccurrence, 0, len(s.data.GovernanceOccurrences))
	for _, occurrence := range s.data.GovernanceOccurrences {
		attentionRead := occurrence.AttentionSeq == 0 || occurrence.AttentionSeq <= s.data.AttentionReadThroughSeq
		if attentionRead && !occurrence.ResolvedAt.IsZero() && occurrence.ResolvedAt.Before(cutoff) {
			removed[occurrence.DisplayID] = true
			continue
		}
		if !attentionRead {
			unreadOccurrences[occurrence.DisplayID] = true
		}
		occurrences = append(occurrences, occurrence)
	}
	priorOccurrences := append([]GovernanceOccurrence(nil), s.data.GovernanceOccurrences...)
	priorAttempts := append([]GovernanceAttempt(nil), s.data.GovernanceAttempts...)
	priorReceipts := append([]GovernanceReceipt(nil), s.data.GovernanceReceipts...)
	priorHighWater := s.data.AttentionHighWaterSeq
	priorReadThrough := s.data.AttentionReadThroughSeq
	s.data.GovernanceOccurrences = occurrences
	s.data.GovernanceAttempts = slices.DeleteFunc(s.data.GovernanceAttempts, func(attempt GovernanceAttempt) bool {
		return removed[attempt.OccurrenceID] || (!unreadOccurrences[attempt.OccurrenceID] && !attempt.RetiredAt.IsZero() && attempt.RetiredAt.Before(cutoff))
	})
	s.data.GovernanceReceipts = slices.DeleteFunc(s.data.GovernanceReceipts, func(receipt GovernanceReceipt) bool {
		return removed[receipt.OccurrenceID] || (!unreadOccurrences[receipt.OccurrenceID] && !receipt.RetiredAt.IsZero() && receipt.RetiredAt.Before(cutoff))
	})
	if len(removed) == 0 && len(s.data.GovernanceAttempts) == len(priorAttempts) && len(s.data.GovernanceReceipts) == len(priorReceipts) {
		if s.hasVolatileGovernanceWriteFailureLocked() {
			return s.saveGovernanceLocked(now)
		}
		return nil
	}
	if err := s.saveGovernanceLocked(now); err != nil {
		s.data.GovernanceOccurrences = priorOccurrences
		s.data.GovernanceAttempts = priorAttempts
		s.data.GovernanceReceipts = priorReceipts
		s.data.AttentionHighWaterSeq = priorHighWater
		s.data.AttentionReadThroughSeq = priorReadThrough
		return err
	}
	return nil
}

func (s *Store) setGovernanceOverflowLocked(now time.Time) error {
	if s.volatileHealth == nil && s.data.GovernanceHealth.State == GovernanceDeliveryOverflow && s.data.GovernanceHealth.Class == GovernanceTransportOverflow {
		return ErrGovernanceOverflow
	}
	health := GovernanceDeliveryHealth{State: GovernanceDeliveryOverflow, Class: GovernanceTransportOverflow, UpdatedAt: now.UTC(), LastAcceptedAt: s.data.GovernanceHealth.LastAcceptedAt}
	s.data.GovernanceHealth = health
	s.data.GovernanceHealthTotals.Overflows++
	_ = s.saveGovernanceLocked(now)
	return ErrGovernanceOverflow
}

func addGovernanceAttemptTotal(totals *GovernanceAttemptTotals, class string) {
	switch class {
	case GovernanceTransportAccepted:
		totals.CumulativeAttempts++
		totals.Accepted++
	case GovernanceTransportRejected, GovernanceTransportHTTPRejected:
		totals.CumulativeAttempts++
		totals.Rejected++
	case GovernanceTransportDeadlineRetry, GovernanceTransportCanceledRetry,
		GovernanceTransportNetworkRetry, GovernanceTransportHTTPRetry, GovernanceTransportTimeoutRetry,
		GovernanceTransportMissingKeys, GovernanceTransportSenderMissing:
		totals.CumulativeAttempts++
		totals.RetryableFailures++
	case GovernanceTransportDead:
		totals.CumulativeAttempts++
		totals.Dead++
	case GovernanceTransportNoSubscription:
		totals.CumulativeAttempts++
		totals.Missed++
	case GovernanceTransportSuppressed:
		totals.CumulativeAttempts++
		totals.Suppressed++
	case GovernanceTransportInterrupted:
		totals.CumulativeAttempts++
		totals.Interrupted++
	case GovernanceTransportTargetRetired:
		totals.CumulativeAttempts++
		totals.TargetRetired++
	}
}

func removeGovernanceAttemptTotal(totals *GovernanceAttemptTotals, class string) {
	if totals.CumulativeAttempts > 0 {
		totals.CumulativeAttempts--
	}
	if class == GovernanceTransportTargetRetired && totals.TargetRetired > 0 {
		totals.TargetRetired--
	}
}

func (s *Store) saveGovernanceLocked(now time.Time) error {
	priorHealthTotals := s.data.GovernanceHealthTotals
	recoveringWrite := s.hasVolatileGovernanceWriteFailureLocked()
	if recoveringWrite {
		s.data.GovernanceHealthTotals.StateFailures++
		s.data.GovernanceHealthTotals.Recoveries++
	}
	if err := s.save(); err != nil {
		s.data.GovernanceHealthTotals = priorHealthTotals
		health := GovernanceDeliveryHealth{State: GovernanceDeliveryUnavailable, Class: GovernanceTransportStateWrite, UpdatedAt: now.UTC(), LastAcceptedAt: s.data.GovernanceHealth.LastAcceptedAt}
		s.volatileHealth = &health
		return err
	}
	if recoveringWrite {
		s.volatileHealth = nil
	}
	return nil
}

func (s *Store) hasVolatileGovernanceWriteFailureLocked() bool {
	return s.volatileHealth != nil && s.volatileHealth.Class == GovernanceTransportStateWrite
}

func validGovernanceTransportClass(class string) bool {
	switch class {
	case GovernanceTransportAccepted, GovernanceTransportPartial, GovernanceTransportAllFailed,
		GovernanceTransportNoSubscription, GovernanceTransportMissingKeys, GovernanceTransportSenderMissing,
		GovernanceTransportReserved, GovernanceTransportInterrupted, GovernanceTransportTargetRetired,
		GovernanceTransportDeadlineRetry, GovernanceTransportCanceledRetry,
		GovernanceTransportNetworkRetry, GovernanceTransportHTTPRetry, GovernanceTransportHTTPRejected,
		GovernanceTransportTimeoutRetry, GovernanceTransportRejected, GovernanceTransportDead,
		GovernanceTransportStateWrite, GovernanceTransportRecovery, GovernanceTransportSuppressed, GovernanceTransportOverflow:
		return true
	default:
		return false
	}
}

func validGovernanceDeliveryHealth(health GovernanceDeliveryHealth) bool {
	if health.UpdatedAt.IsZero() || !validGovernanceTransportClass(health.Class) {
		return false
	}
	switch health.State {
	case GovernanceDeliveryHealthy, GovernanceDeliverySuppressed, GovernanceDeliveryDegraded, GovernanceDeliveryUnavailable, GovernanceDeliveryOverflow:
		return true
	default:
		return false
	}
}

func validDiagnosticState(state string) bool {
	switch state {
	case GovernanceTransportAccepted, GovernanceTransportPartial, GovernanceTransportAllFailed,
		GovernanceTransportNoSubscription, GovernanceTransportMissingKeys, GovernanceTransportSenderMissing,
		GovernanceTransportDeadlineRetry, GovernanceTransportCanceledRetry, GovernanceTransportNetworkRetry,
		GovernanceTransportHTTPRetry, GovernanceTransportHTTPRejected, GovernanceTransportTimeoutRetry,
		GovernanceTransportRejected, GovernanceTransportDead, GovernanceTransportStateWrite, GovernanceTransportSuppressed:
		return true
	default:
		return false
	}
}

// RecordAlert appends one redacted legacy inbox record and assigns its durable
// attention sequence. It rejects duplicate IDs and refuses to evict unread
// history when the bounded store is full.
func (s *Store) RecordAlert(rec AlertRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordAlertLocked(rec)
}

// RecordAlertIfNew atomically deduplicates a semantic Canary occurrence and
// records its durable inbox row under the same store transaction.
func (s *Store) RecordAlertIfNew(rec AlertRecord) (bool, error) {
	if strings.TrimSpace(rec.Fingerprint) == "" {
		return false, errors.New("alert fingerprint required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.data.AlertHistory {
		if existing.Fingerprint == rec.Fingerprint {
			return false, nil
		}
	}
	if err := s.recordAlertLocked(rec); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) recordAlertLocked(rec AlertRecord) error {
	if strings.TrimSpace(rec.ID) == "" {
		return errors.New("alert id required")
	}
	for _, existing := range s.data.AlertHistory {
		if existing.ID == rec.ID {
			return fmt.Errorf("alert id %q already exists", rec.ID)
		}
	}
	priorHistory := append([]AlertRecord(nil), s.data.AlertHistory...)
	priorHighWater := s.data.AttentionHighWaterSeq
	priorReadThrough := s.data.AttentionReadThroughSeq
	for len(s.data.AlertHistory) >= 100 {
		evict := -1
		for i, record := range slices.Backward(s.data.AlertHistory) {
			seq := record.AttentionSeq
			if seq == 0 || seq <= s.data.AttentionReadThroughSeq {
				evict = i
				break
			}
		}
		if evict < 0 {
			s.data.AlertHistory = priorHistory
			return ErrAlertHistoryOverflow
		}
		s.data.AlertHistory = slices.Delete(s.data.AlertHistory, evict, evict+1)
	}
	attentionSeq, err := s.nextAttentionSeqLocked()
	if err != nil {
		s.data.AlertHistory = priorHistory
		return err
	}
	rec.AttentionSeq = attentionSeq
	s.data.AlertHistory = append([]AlertRecord{rec}, s.data.AlertHistory...)
	if err := s.save(); err != nil {
		s.data.AlertHistory = priorHistory
		s.data.AttentionHighWaterSeq = priorHighWater
		s.data.AttentionReadThroughSeq = priorReadThrough
		return err
	}
	return nil
}

// AlertHistory returns a copy of the newest legacy inbox rows. A non-positive
// limit returns all retained rows.
func (s *Store) AlertHistory(limit int) []AlertRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > len(s.data.AlertHistory) {
		limit = len(s.data.AlertHistory)
	}
	out := make([]AlertRecord, limit)
	copy(out, s.data.AlertHistory[:limit])
	return out
}

// ClearAlertHistory removes only rows already covered by the durable read
// cursor and returns the number removed; unread rows are always retained.
func (s *Store) ClearAlertHistory() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := append([]AlertRecord(nil), s.data.AlertHistory...)
	retained := make([]AlertRecord, 0, len(s.data.AlertHistory))
	for _, record := range s.data.AlertHistory {
		if record.AttentionSeq != 0 && record.AttentionSeq > s.data.AttentionReadThroughSeq {
			retained = append(retained, record)
		}
	}
	cleared := len(s.data.AlertHistory) - len(retained)
	if cleared == 0 {
		return 0, nil
	}
	s.data.AlertHistory = retained
	if err := s.save(); err != nil {
		s.data.AlertHistory = prior
		return 0, err
	}
	return cleared, nil
}

// CompactAlertHistory refreshes the last-matched stamp on records that still
// match the observed context and drops read records whose context died more
// than the retention window ago. Matching mirrors the SPA's staleness rule:
// only a positive mismatch (a different live canary fingerprint for a
// canary-source record, or a different stated account/mode) marks a record
// previous-context; unknown context never expires anything. Unread records
// never expire — the operator sees evidence before the store forgets it.
func (s *Store) CompactAlertHistory(canaryFingerprint, account, mode string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	cutoff := now.Add(-alertPreviousContextRetention)
	changed := false
	retained := make([]AlertRecord, 0, len(s.data.AlertHistory))
	for _, rec := range s.data.AlertHistory {
		if alertRecordMatchesContext(rec, canaryFingerprint, account, mode) {
			if rec.LastMatchedAt.IsZero() || now.Sub(rec.LastMatchedAt) >= alertMatchStampInterval {
				rec.LastMatchedAt = now
				changed = true
			}
			retained = append(retained, rec)
			continue
		}
		read := rec.AttentionSeq == 0 || rec.AttentionSeq <= s.data.AttentionReadThroughSeq
		lastCurrent := rec.LastMatchedAt
		if lastCurrent.IsZero() {
			lastCurrent = rec.CreatedAt
		}
		if read && lastCurrent.Before(cutoff) {
			changed = true
			continue
		}
		retained = append(retained, rec)
	}
	if !changed {
		return nil
	}
	prior := s.data.AlertHistory
	s.data.AlertHistory = retained
	if err := s.save(); err != nil {
		s.data.AlertHistory = prior
		return err
	}
	return nil
}

func alertRecordMatchesContext(rec AlertRecord, canaryFingerprint, account, mode string) bool {
	if strings.HasPrefix(rec.ID, "canary-") && rec.Fingerprint != "" && canaryFingerprint != "" && rec.Fingerprint != canaryFingerprint {
		return false
	}
	if rec.Account != "" && account != "" && rec.Account != account {
		return false
	}
	if rec.Mode != "" && mode != "" && rec.Mode != mode {
		return false
	}
	return true
}

// HasAlertFingerprint reports whether the legacy inbox retains a record with
// the private semantic fingerprint fp.
func (s *Store) HasAlertFingerprint(fp string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.data.AlertHistory {
		if rec.Fingerprint == fp {
			return true
		}
	}
	return false
}

// RecordPush replaces the legacy last-attempt diagnostic with attempt.
func (s *Store) RecordPush(attempt PushAttempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.LastPush = &attempt
	return s.save()
}

// LastPush returns a copy of the legacy last-attempt diagnostic, or nil when
// no attempt has been recorded.
func (s *Store) LastPush() *PushAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.LastPush == nil {
		return nil
	}
	cp := *s.data.LastPush
	return &cp
}

// EnsureVAPID returns the retained app signing keys or generates and durably
// stores one pair. gen is called while the store is locked and only when a
// complete retained pair is unavailable.
func (s *Store) EnsureVAPID(now time.Time, gen func() (privateKey, publicKey string, err error)) (VAPIDKeys, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.VAPID != nil && s.data.VAPID.PublicKey != "" && s.data.VAPID.PrivateKey != "" {
		return *s.data.VAPID, nil
	}
	priv, pub, err := gen()
	if err != nil {
		return VAPIDKeys{}, err
	}
	keys := VAPIDKeys{PublicKey: pub, PrivateKey: priv, CreatedAt: now}
	s.data.VAPID = &keys
	if err := s.save(); err != nil {
		return VAPIDKeys{}, err
	}
	return keys, nil
}

// VAPID returns a copy of the app's retained signing keys. False means no key
// record exists; callers must also validate non-empty key material as needed.
func (s *Store) VAPID() (VAPIDKeys, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.VAPID == nil {
		return VAPIDKeys{}, false
	}
	return *s.data.VAPID, true
}

// RelayRoute returns the resumable route only when remoteURL and its required
// credentials match. An expired route is still returned for token-matched
// revival.
func (s *Store) RelayRoute(remoteURL string) (RelayRoute, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.RelayRoute == nil {
		return RelayRoute{}, false
	}
	route := *s.data.RelayRoute
	if route.RemoteURL != remoteURL || route.RouteID == "" || route.ConnectorToken == "" {
		return RelayRoute{}, false
	}
	// An expired route is still returned: the relay revives a token-matched
	// resume, and abandoning the route id here would orphan every paired
	// phone. ExpiresAt is informational.
	return route, true
}

// SetRelayRoute validates and durably stores a relay registration, preserving
// CreatedAt when the same route identity is refreshed.
func (s *Store) SetRelayRoute(route RelayRoute) error {
	if route.RemoteURL == "" {
		return errors.New("relay remote URL required")
	}
	if route.RouteID == "" {
		return errors.New("relay route id required")
	}
	if route.ConnectorToken == "" {
		return errors.New("relay connector token required")
	}
	now := time.Now().UTC()
	route.UpdatedAt = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if route.CreatedAt.IsZero() {
		// Route extensions re-persist the same route id; keep its birth
		// time so route age stays observable.
		if prev := s.data.RelayRoute; prev != nil && prev.RouteID == route.RouteID {
			route.CreatedAt = prev.CreatedAt
		}
		if route.CreatedAt.IsZero() {
			route.CreatedAt = now
		}
	}
	s.data.RelayRoute = &route
	return s.save()
}

func (s *Store) save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	b, err := s.marshalStateForSave()
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if s.saveHook != nil {
		if err := s.saveHook("write"); err != nil {
			return err
		}
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	closed = true
	if s.saveHook != nil {
		if err := s.saveHook("rename"); err != nil {
			return err
		}
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	if s.saveObserver != nil {
		s.saveObserver()
	}
	return nil
}

func validAlertMode(mode string) bool {
	switch mode {
	case AlertModeNone, AlertModeActOnly, AlertModeWatchAndAct:
		return true
	default:
		return false
	}
}

func validGovernanceDisposition(disposition string) bool {
	switch disposition {
	case GovernanceDispositionEligible, GovernanceDispositionSuppressedAtCreation, GovernanceDispositionLegacyUnknown:
		return true
	default:
		return false
	}
}
