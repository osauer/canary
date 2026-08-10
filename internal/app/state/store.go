// Package state owns the Canary app's private durable state, including paired
// devices, push subscriptions, redacted inbox records, attention cursors, and
// app-local delivery evidence. It serializes mutations to state.json; daemon
// runtime and policy state remain separate authorities.
package state

import (
	"cmp"
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
const (
	AlertModeNone        = "none"
	AlertModeActOnly     = "act_only"
	AlertModeWatchAndAct = "watch_and_act"
)

// Governance transport and delivery constants classify app-local Web Push
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
	ErrAlertHistoryOverflow          = errors.New("alert history overflow: unread retention limit reached")
	ErrAttentionReadRegression       = errors.New("attention read cursor cannot regress")
	ErrAttentionReadBeyondHighWater  = errors.New("attention read cursor exceeds high-water sequence")
	ErrAttentionReferencesIncomplete = errors.New("attention references are incomplete through requested sequence")
	ErrAttentionSequenceExhausted    = errors.New("attention sequence exhausted")
	ErrInvalidPersistedState         = errors.New("invalid persisted app state")
)

// AttentionKindStress identifies the single legacy inbox record family
const AttentionKindStress = "stress"

// legacyStressAlertIDPrefix is the ID prefix the retired portfolio-stress inbox
const legacyStressAlertIDPrefix = "canary-"

const (
	// alertPreviousContextRetention expires read alert records that stopped
	// Unread records and still-matching records never expire.
	alertPreviousContextRetention = 14 * 24 * time.Hour
	// alertMatchStampInterval bounds LastMatchedAt refresh writes: matching
	// 14-day retention only needs hourly stamp granularity.
	alertMatchStampInterval = time.Hour
)

// Store serializes access to the app's private state.json and returns copies or
// redacted projections at its public read boundaries. Its zero value is not
// usable; callers open a store with [Open].
type Store struct {
	path                            string
	mu                              sync.Mutex
	data                            Data
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
type Data struct {
	Devices           []DeviceGrant       `json:"devices,omitempty"`
	AlertSettings     AlertSettings       `json:"alert_settings"`
	PushSubscriptions []PushSubscription  `json:"push_subscriptions,omitempty"`
	AlertHistory      []AlertRecord       `json:"alert_history,omitempty"`
	VAPID             *VAPIDKeys          `json:"vapid,omitempty"`
	LastPush          *PushAttempt        `json:"last_push,omitempty"`
	ProposalAudit     []ProposalAuditItem `json:"proposal_audit,omitempty"`
	RelayRoute        *RelayRoute         `json:"relay_route,omitempty"`
	// LegacyGovernanceOccurrences decodes the retired governance ledger's
	// occurrence rows for exactly one purpose: compacting their attention
	// migration and never persisted again.
	LegacyGovernanceOccurrences []legacyGovernanceOccurrence `json:"governance_occurrences,omitempty"`
	DiagnosticStatus            GovernanceDiagnosticStatus   `json:"diagnostic_status"`
	AttentionHighWaterSeq       uint64                       `json:"attention_high_water_seq"`
	AttentionReadThroughSeq     uint64                       `json:"attention_read_through_seq"`
	AlertDelivery               *alertDeliveryData           `json:"alert_delivery,omitempty"`
}

// legacyGovernanceOccurrence is the one-way decode shape for rows the retired
// governance ledger persisted. Only the attention sequence matters: the
// migration renumbers the shared cursor space without them.
type legacyGovernanceOccurrence struct {
	AttentionSeq uint64 `json:"attention_seq"`
}

// DeviceGrant is an app-owned paired-device identity. RevokedAt is terminal:
type DeviceGrant struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	PublicKeyJWK string `json:"public_key_jwk,omitempty"`
	// DeviceCookieHashes authenticate the long-lived HttpOnly device
	// cookie. Cookies are the only client storage that provably survives
	// written by Safari never reach the installed app), so session
	// continuity must not depend on script-visible storage. A capped list,
	// the cookie jar, so issuing a fresh cookie to one twin must never
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
	// LastMatchedAt is refreshed while an observed stress result still matches this
	// record's context (fingerprint for stress-source records, account/mode
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

// AttentionRef identifies one redacted legacy inbox row without exposing its
// private fingerprint or transport identity.
type AttentionRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Attention is the shared durable unread cursor for the legacy Alerts inbox.
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

// GovernanceDiagnosticStatus stores the latest safe notification-test result.
type GovernanceDiagnosticStatus struct {
	State string    `json:"state,omitempty"`
	At    time.Time `json:"at,omitzero"`
}

// VAPIDKeys stores the app-owned signing key pair. PrivateKey must never cross
type VAPIDKeys struct {
	PublicKey  string    `json:"public_key"`
	PrivateKey string    `json:"private_key"`
	CreatedAt  time.Time `json:"created_at"`
}

// ProposalAuditItem is a durable app-side audit row for paired-device proposal
type ProposalAuditItem struct {
	ID        string          `json:"id"`
	DeviceID  string          `json:"device_id,omitempty"`
	Action    string          `json:"action,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Open loads or initializes the private app store under dir, validates its
// persisted invariants, and recovers interrupted delivery reservations. It
// replacement authority.
func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state dir required")
	}
	s := &Store{path: filepath.Join(dir, "state.json")}
	s.initAlertDeliveryRuntime()
	if err := s.load(); err != nil {
		return nil, err
	}
	if s.data.AlertSettings.Mode == "" {
		s.data.AlertSettings.Mode = AlertModeWatchAndAct
	} else if !validAlertMode(s.data.AlertSettings.Mode) {
		return nil, fmt.Errorf("%w: invalid alert mode %q", ErrInvalidPersistedState, s.data.AlertSettings.Mode)
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
	// stress authority unavailable, while every legacy field still uses its
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
	s.migrateLegacyGovernanceAttention()
	return nil
}

// migrateLegacyGovernanceAttention is the one-way decoder for the retired
// rejects. The migration renumbers the remaining alert-history sequences
// nils the legacy rows so the next save drops them permanently. Unread
func (s *Store) migrateLegacyGovernanceAttention() {
	if len(s.data.LegacyGovernanceOccurrences) == 0 {
		return
	}
	s.data.LegacyGovernanceOccurrences = nil
	type seqRef struct {
		seq uint64
		idx int
	}
	rows := make([]seqRef, 0, len(s.data.AlertHistory))
	for i, rec := range s.data.AlertHistory {
		if rec.AttentionSeq != 0 {
			rows = append(rows, seqRef{seq: rec.AttentionSeq, idx: i})
		}
	}
	slices.SortFunc(rows, func(a, b seqRef) int { return cmp.Compare(a.seq, b.seq) })
	var next, readThrough uint64
	for _, row := range rows {
		next++
		if row.seq <= s.data.AttentionReadThroughSeq {
			readThrough = next
		}
		s.data.AlertHistory[row.idx].AttentionSeq = next
	}
	s.data.AttentionReadThroughSeq = readThrough
	s.data.AttentionHighWaterSeq = next
}

// AlertSettings returns the current app notification mode.
func (s *Store) AlertSettings() AlertSettings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AlertSettings
}

// Attention returns a snapshot of the legacy inbox's shared durable unread
func (s *Store) Attention() Attention {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attentionLocked()
}

// MarkAttentionRead durably advances the shared read cursor to application
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
			entries = append(entries, attentionEntry{seq: record.AttentionSeq, ref: AttentionRef{Kind: AttentionKindStress, ID: record.ID}})
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
		if err := validate(record.AttentionSeq, AttentionRef{Kind: AttentionKindStress, ID: record.ID}); err != nil {
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
func (s *Store) AddDevice(d DeviceGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.Devices {
		if s.data.Devices[i].ID == d.ID {
			priorDevice := s.data.Devices[i]
			if !priorDevice.RevokedAt.IsZero() && d.RevokedAt.IsZero() {
				return errors.New("revoked device identity cannot be reactivated; pair a new device")
			}
			priorAlertDelivery := s.data.AlertDelivery
			var alertRelease []string
			alertChanged := false
			if priorDevice.RevokedAt.IsZero() && !d.RevokedAt.IsZero() {
				alertTargets := map[string]bool{}
				for _, subscription := range s.data.PushSubscriptions {
					if subscription.DeviceID != d.ID {
						continue
					}
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
			}
			s.data.Devices[i] = d
			if err := s.save(); err != nil {
				s.data.Devices[i] = priorDevice
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
const maxDeviceCookieHashes = 5

// AddDeviceCookieHash retains a bounded set of cookie generations for one
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
	priorAlertDelivery := s.data.AlertDelivery
	alertTargets := map[string]bool{}
	for _, sub := range s.data.PushSubscriptions {
		if removed[sub.DeviceID] {
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
	if err := s.save(); err != nil {
		s.data.Devices = priorDevices
		s.data.PushSubscriptions = priorSubscriptions
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
			}
			s.data.PushSubscriptions[i] = sub
			if err := s.save(); err != nil {
				s.data.PushSubscriptions[i] = priorSub
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
// paired devices. Governance delivery deliberately does not inherit the legacy stress inbox's
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
	priorAlertDelivery := s.data.AlertDelivery
	alertTargets := map[string]bool{}
	for _, sub := range s.data.PushSubscriptions {
		if sub.ID == id || sub.Endpoint == id {
			alertTargets[AlertDeliveryTargetRef(sub.DeviceID, sub.ID)] = true
		}
	}
	if len(alertTargets) == 0 {
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
	if err := s.save(); err != nil {
		s.data.PushSubscriptions = priorSubscriptions
		s.data.AlertDelivery = priorAlertDelivery
		if alertChanged {
			s.noteAlertDeliverySaveFailureLocked(retiredAt)
		}
		return err
	}
	s.finishAlertDeliveryRetirementLocked(alertRelease, alertChanged)
	return nil
}

// RecordDiagnosticStatus validates and persists the latest safe notification
func (s *Store) RecordDiagnosticStatus(status GovernanceDiagnosticStatus) error {
	if status.At.IsZero() || !validDiagnosticState(status.State) {
		return errors.New("invalid diagnostic status")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.data.DiagnosticStatus
	s.data.DiagnosticStatus = status
	if err := s.save(); err != nil {
		s.data.DiagnosticStatus = prior
		return err
	}
	return nil
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

// GovernanceDiagnostic returns the latest safe notification-test result.
func (s *Store) GovernanceDiagnostic() GovernanceDiagnosticStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.DiagnosticStatus
}

// RecordAlert appends one redacted legacy inbox record and assigns its durable
func (s *Store) RecordAlert(rec AlertRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordAlertLocked(rec)
}

// RecordAlertIfNew atomically deduplicates a semantic portfolio-stress occurrence and
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
// only a positive mismatch (a different live stress fingerprint for a
// stress-source record, or a different stated account/mode) marks a record
// previous-context; unknown context never expires anything. Unread records
// never expire — the operator sees evidence before the store forgets it.
func (s *Store) CompactAlertHistory(stressFingerprint, account, mode string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now = now.UTC()
	cutoff := now.Add(-alertPreviousContextRetention)
	changed := false
	retained := make([]AlertRecord, 0, len(s.data.AlertHistory))
	for _, rec := range s.data.AlertHistory {
		if alertRecordMatchesContext(rec, stressFingerprint, account, mode) {
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

func alertRecordMatchesContext(rec AlertRecord, stressFingerprint, account, mode string) bool {
	if strings.HasPrefix(rec.ID, legacyStressAlertIDPrefix) && rec.Fingerprint != "" && stressFingerprint != "" && rec.Fingerprint != stressFingerprint {
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
	return route, true
}

// SetRelayRoute validates and durably stores a relay registration, preserving
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
