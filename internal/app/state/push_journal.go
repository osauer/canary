package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The push journal is the app's durable per-notice record of every Web Push
// request and every device acknowledgement. It is separate from the alert
// ledger: a diagnostic notice travels the same transport and receipt path but
// is never an alert occurrence, never counts toward the runaway fuse, and
// never enters the inbox.
const (
	pushJournalVersion = "push-journal-v1"
	// The bounds keep state.json small (it is rewritten on every poll): 64
	// notices is weeks of real alerts, and 16 attempts covers several
	// subscriptions through the full retry schedule.
	pushJournalNoticeLimit  = 64
	pushJournalAttemptLimit = 16
	pushJournalAckLimit     = 16
	// pushAckClockSkew tolerates a receipt stamped a little before the send
	// clock (the notice and the receipt are both app-host clocks, so any real
	// gap is scheduling jitter, never a device clock).
	pushAckClockSkew = 5 * time.Second
)

// Push journal errors are typed so the HTTP adapter can map them to exact
// statuses without parsing text.
var (
	ErrPushNoticeUnknown  = errors.New("push notice is not in the journal")
	ErrPushAckInvalid     = errors.New("push acknowledgement is invalid")
	ErrPushAckDeviceGone  = errors.New("acknowledging device is not an active paired device")
	errPushJournalInvalid = errors.New("invalid push journal")
)

type pushJournal struct {
	Version string              `json:"version"`
	Notices []pushJournalNotice `json:"notices"`
}

type pushJournalNotice struct {
	NoticeID    string               `json:"notice_id"`
	Kind        string               `json:"kind"`
	FirstSentAt time.Time            `json:"first_sent_at"`
	Attempts    []pushJournalAttempt `json:"attempts"`
	Acks        []pushJournalAck     `json:"acks,omitempty"`
}

type pushJournalAttempt struct {
	At         time.Time `json:"at"`
	TargetRef  string    `json:"target_ref"`
	DeviceID   string    `json:"device_id"`
	Class      string    `json:"class"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Accepted   bool      `json:"accepted"`
}

type pushJournalAck struct {
	Event      string    `json:"event"`
	DeviceID   string    `json:"device_id"`
	DeviceAt   time.Time `json:"device_at,omitzero"`
	ReceivedAt time.Time `json:"received_at"`
}

// PushJournalDiscarded reports a persisted journal that failed validation at
// open and was dropped; nil when the journal loaded or was absent.
func (s *Store) PushJournalDiscarded() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadedPushJournalDiscarded
}

// PushAckOutcome reports a stored or already-known acknowledgement. Recorded
// is false for an exact duplicate (same notice, device, and event).
type PushAckOutcome struct {
	NoticeID   string    `json:"notice_id"`
	Kind       string    `json:"kind"`
	Event      string    `json:"event"`
	Recorded   bool      `json:"recorded"`
	ReceivedAt time.Time `json:"received_at"`
}

// NewPushNoticeID mints an opaque notice identity for a diagnostic push from
// random bytes supplied by the caller.
func NewPushNoticeID(random [8]byte) string {
	return rpc.PushNoticeKindDiagnostic + "-" + hex.EncodeToString(random[:])
}

// PushDeviceRef is the redacted device identity carried off this host: a
// short hash, never the paired-device grant id.
func PushDeviceRef(deviceID string) string {
	sum := sha256.Sum256([]byte("push-device-ref-v1\x00" + deviceID))
	return hex.EncodeToString(sum[:6])
}

// RecordPushAttempt durably journals one classified Web Push request for a
// notice. It never grants or blocks transport; a failure to persist leaves
// the in-memory journal unchanged.
func (s *Store) RecordPushAttempt(kind, noticeID string, sub PushSubscription, attempt PushAttempt) error {
	if kind != rpc.PushNoticeKindAlert && kind != rpc.PushNoticeKindDiagnostic {
		return fmt.Errorf("%w: kind %q", errPushJournalInvalid, kind)
	}
	if err := rpc.ValidatePushNoticeID(noticeID); err != nil {
		return err
	}
	at := attempt.At.UTC()
	if at.IsZero() {
		return fmt.Errorf("%w: attempt time required", errPushJournalInvalid)
	}
	class := strings.TrimSpace(attempt.Class)
	if class == "" {
		class = GovernanceTransportHTTPRejected
	}
	entry := pushJournalAttempt{
		At: at, TargetRef: AlertDeliveryTargetRef(sub.DeviceID, sub.ID), DeviceID: sub.DeviceID,
		Class: class, HTTPStatus: attempt.StatusCode, Accepted: attempt.OK && class == GovernanceTransportAccepted,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.data.PushJournal
	next := clonePushJournal(prior)
	index := slices.IndexFunc(next.Notices, func(n pushJournalNotice) bool { return n.NoticeID == noticeID })
	if index < 0 {
		next.Notices = append(next.Notices, pushJournalNotice{NoticeID: noticeID, Kind: kind, FirstSentAt: at})
		index = len(next.Notices) - 1
	} else if next.Notices[index].Kind != kind {
		return fmt.Errorf("%w: notice kind changed", errPushJournalInvalid)
	}
	notice := &next.Notices[index]
	notice.Attempts = append(notice.Attempts, entry)
	if len(notice.Attempts) > pushJournalAttemptLimit {
		notice.Attempts = notice.Attempts[len(notice.Attempts)-pushJournalAttemptLimit:]
	}
	if len(next.Notices) > pushJournalNoticeLimit {
		slices.SortStableFunc(next.Notices, func(a, b pushJournalNotice) int { return a.FirstSentAt.Compare(b.FirstSentAt) })
		next.Notices = next.Notices[len(next.Notices)-pushJournalNoticeLimit:]
	}
	s.data.PushJournal = next
	if err := s.save(); err != nil {
		s.data.PushJournal = prior
		return err
	}
	return nil
}

// RecordPushAck durably records a displayed or opened receipt that the
// authenticated paired device deviceID reported for a journaled notice. The
// app-host clock at receipt is authoritative; deviceAt is kept as the
// device's own claim. An exact duplicate is acknowledged without a write.
func (s *Store) RecordPushAck(noticeID, deviceID, event string, deviceAt, receivedAt time.Time) (PushAckOutcome, error) {
	if err := rpc.ValidatePushNoticeID(noticeID); err != nil {
		return PushAckOutcome{}, fmt.Errorf("%w: %v", ErrPushAckInvalid, err)
	}
	if event != rpc.PushAckDisplayed && event != rpc.PushAckOpened {
		return PushAckOutcome{}, fmt.Errorf("%w: event %q", ErrPushAckInvalid, event)
	}
	receivedAt = receivedAt.UTC()
	if receivedAt.IsZero() {
		return PushAckOutcome{}, fmt.Errorf("%w: receipt time required", ErrPushAckInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.activeDeviceLocked(deviceID) {
		return PushAckOutcome{}, ErrPushAckDeviceGone
	}
	prior := s.data.PushJournal
	if prior == nil {
		return PushAckOutcome{}, ErrPushNoticeUnknown
	}
	index := slices.IndexFunc(prior.Notices, func(n pushJournalNotice) bool { return n.NoticeID == noticeID })
	if index < 0 {
		return PushAckOutcome{}, ErrPushNoticeUnknown
	}
	notice := prior.Notices[index]
	outcome := PushAckOutcome{NoticeID: noticeID, Kind: notice.Kind, Event: event}
	for _, ack := range notice.Acks {
		if ack.DeviceID == deviceID && ack.Event == event {
			outcome.ReceivedAt = ack.ReceivedAt
			return outcome, nil
		}
	}
	if receivedAt.Add(pushAckClockSkew).Before(notice.FirstSentAt) {
		return PushAckOutcome{}, fmt.Errorf("%w: receipt precedes the send", ErrPushAckInvalid)
	}
	if len(notice.Acks) >= pushJournalAckLimit {
		return PushAckOutcome{}, fmt.Errorf("%w: acknowledgement limit reached", ErrPushAckInvalid)
	}
	next := clonePushJournal(prior)
	next.Notices[index].Acks = append(next.Notices[index].Acks, pushJournalAck{
		Event: event, DeviceID: deviceID, DeviceAt: deviceAt.UTC(), ReceivedAt: receivedAt,
	})
	s.data.PushJournal = next
	if err := s.save(); err != nil {
		s.data.PushJournal = prior
		return PushAckOutcome{}, err
	}
	outcome.Recorded = true
	outcome.ReceivedAt = receivedAt
	return outcome, nil
}

func (s *Store) activeDeviceLocked(deviceID string) bool {
	if strings.TrimSpace(deviceID) == "" {
		return false
	}
	for _, device := range s.data.Devices {
		if device.ID == deviceID && device.RevokedAt.IsZero() {
			return true
		}
	}
	return false
}

// DeviceName returns the paired-device label for deviceID ("iPhone"), or a
// neutral label when the grant is unknown or its name is unusable.
func (s *Store) DeviceName(deviceID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deviceNameLocked(deviceID)
}

func (s *Store) deviceNameLocked(deviceID string) string {
	for _, device := range s.data.Devices {
		if device.ID == deviceID {
			name := strings.TrimSpace(device.Name)
			if name != "" && len(name) <= 64 && !strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) {
				return name
			}
			break
		}
	}
	return "paired device"
}

// PushDeliveryProof projects the journal, the alert ledger's pre-journal
// history, and delivery readiness into the redacted proof relayed to the
// daemon and served by the app's status surfaces.
func (s *Store) PushDeliveryProof(now time.Time) rpc.PushDeliveryProof {
	now = now.UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	proof := rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion,
		ReportedAt:    now,
		Mode:          s.data.AlertSettings.Mode,
		Recent:        []rpc.PushNoticeFact{},
	}
	// Health mirrors the ledger view (persisted health, overridden by a
	// volatile write or quarantine failure) without building the whole view:
	// the alerts stream projects this proof several times a second.
	var health AlertDeliveryHealth
	if s.data.AlertDelivery != nil {
		health = s.data.AlertDelivery.Health
	}
	if s.alertDeliveryVolatile != nil {
		health = *s.alertDeliveryVolatile
	}
	proof.Dispatcher, proof.DispatcherClass = health.State, health.Class
	if proof.Dispatcher == "" {
		proof.Dispatcher, proof.DispatcherClass = AlertDeliveryHealthUnavailable, "not_initialized"
	}
	if data := s.data.AlertDelivery; data != nil && !data.ObservationRejectedAt.IsZero() {
		proof.IntakeRejectedSince = timePointer(data.ObservationRejectedAt)
	}
	active := make(map[string]bool, len(s.data.Devices))
	for _, device := range s.data.Devices {
		if device.RevokedAt.IsZero() {
			active[device.ID] = true
		}
	}
	for _, sub := range s.data.PushSubscriptions {
		if active[sub.DeviceID] {
			proof.ActiveSubscriptions++
		}
	}

	var lastSent *rpc.PushSendFact
	consider := func(fact rpc.PushSendFact) {
		if lastSent == nil || fact.At.After(lastSent.At) {
			copied := fact
			lastSent = &copied
		}
	}
	later := func(current *time.Time, at time.Time) *time.Time {
		if at.IsZero() || (current != nil && !at.After(*current)) {
			return current
		}
		return timePointer(at)
	}
	var expiredAt *time.Time
	journaled := map[string]bool{}
	if journal := s.data.PushJournal; journal != nil {
		for _, notice := range journal.Notices {
			journaled[notice.NoticeID] = true
			for _, attempt := range notice.Attempts {
				consider(rpc.PushSendFact{At: attempt.At, Kind: notice.Kind, NoticeID: notice.NoticeID, Class: attempt.Class, HTTPStatus: attempt.HTTPStatus, Accepted: attempt.Accepted})
				switch notice.Kind {
				case rpc.PushNoticeKindAlert:
					proof.LastAlertSentAt = later(proof.LastAlertSentAt, attempt.At)
					if attempt.Accepted {
						proof.SilentSince = later(proof.SilentSince, attempt.At)
					}
				case rpc.PushNoticeKindDiagnostic:
					proof.LastDiagnosticSentAt = later(proof.LastDiagnosticSentAt, attempt.At)
				}
				if attempt.Class == GovernanceTransportDead {
					expiredAt = later(expiredAt, attempt.At)
				}
			}
			for _, ack := range notice.Acks {
				fact := rpc.PushAckFact{
					At: ack.ReceivedAt, DeviceAt: ack.DeviceAt, Event: ack.Event, Kind: notice.Kind,
					NoticeID: notice.NoticeID, Device: s.deviceNameLocked(ack.DeviceID), DeviceRef: PushDeviceRef(ack.DeviceID),
				}
				switch ack.Event {
				case rpc.PushAckDisplayed:
					if proof.LastDisplayed == nil || fact.At.After(proof.LastDisplayed.At) {
						proof.LastDisplayed = &fact
					}
				case rpc.PushAckOpened:
					if proof.LastOpened == nil || fact.At.After(proof.LastOpened.At) {
						proof.LastOpened = &fact
					}
				}
			}
		}
		proof.Recent = s.recentPushNoticesLocked(journal)
	}
	// Alert pushes sent before the journal existed live only in the alert
	// ledger (and, before that, the legacy last-push record). A notice the
	// journal holds is described by the journal, which also has its status.
	if data := s.data.AlertDelivery; data != nil {
		for _, attempt := range data.Attempts {
			if attempt.CompletedAt.IsZero() || !pushLedgerTransportClass(attempt.Class) {
				continue
			}
			noticeID := alertDeliveryDisplayID(attempt.AuthorityScope, attempt.OccurrenceKey)
			if journaled[noticeID] {
				continue
			}
			consider(rpc.PushSendFact{
				At: attempt.CompletedAt, Kind: rpc.PushNoticeKindAlert, NoticeID: noticeID,
				Class: attempt.Class, Accepted: attempt.Class == AlertDeliveryAttemptAccepted,
			})
			proof.LastAlertSentAt = later(proof.LastAlertSentAt, attempt.CompletedAt)
		}
		for _, receipt := range data.Receipts {
			proof.SilentSince = later(proof.SilentSince, receipt.AcceptedAt)
		}
		proof.SilentSince = later(proof.SilentSince, data.Health.LastAcceptedAt)
	}
	if legacy := s.data.LastPush; legacy != nil && !legacy.At.IsZero() {
		if legacyID := strings.TrimSpace(legacy.AlertID); rpc.ValidatePushNoticeID(legacyID) == nil {
			class := legacy.Class
			if class == "" {
				class = GovernanceTransportHTTPRejected
				if legacy.OK {
					class = GovernanceTransportAccepted
				}
			}
			consider(rpc.PushSendFact{At: legacy.At.UTC(), Kind: rpc.PushNoticeKindAlert, NoticeID: legacyID, Class: class, HTTPStatus: legacy.StatusCode, Accepted: legacy.OK})
		}
		proof.LastAlertSentAt = later(proof.LastAlertSentAt, legacy.At.UTC())
		if legacy.OK {
			proof.SilentSince = later(proof.SilentSince, legacy.At.UTC())
		}
	}
	proof.LastSent = lastSent
	proof.SubscriptionExpiredAt = expiredAt
	proof.Witnessed = proof.LastDisplayed != nil || proof.LastOpened != nil
	return proof
}

// pushLedgerTransportClass keeps only ledger attempts that reached the push
// service: a request was made and answered (or failed in transport).
func pushLedgerTransportClass(class string) bool {
	switch class {
	case AlertDeliveryAttemptAccepted, AlertDeliveryAttemptRetry, AlertDeliveryAttemptRejected, AlertDeliveryAttemptExhausted:
		return true
	default:
		return false
	}
}

func (s *Store) recentPushNoticesLocked(journal *pushJournal) []rpc.PushNoticeFact {
	notices := slices.Clone(journal.Notices)
	slices.SortStableFunc(notices, func(a, b pushJournalNotice) int {
		return cmp.Or(b.FirstSentAt.Compare(a.FirstSentAt), strings.Compare(a.NoticeID, b.NoticeID))
	})
	if len(notices) > rpc.PushDeliveryRecentLimit {
		notices = notices[:rpc.PushDeliveryRecentLimit]
	}
	out := make([]rpc.PushNoticeFact, 0, len(notices))
	for _, notice := range notices {
		if len(notice.Attempts) == 0 {
			continue
		}
		fact := rpc.PushNoticeFact{NoticeID: notice.NoticeID, Kind: notice.Kind, SentAt: notice.FirstSentAt}
		representative := notice.Attempts[len(notice.Attempts)-1]
		targets := map[string]bool{}
		for _, attempt := range notice.Attempts {
			targets[attempt.TargetRef] = true
			if attempt.Accepted && !representative.Accepted {
				representative = attempt
			}
		}
		fact.Class, fact.HTTPStatus, fact.Accepted, fact.Targets = representative.Class, representative.HTTPStatus, representative.Accepted, len(targets)
		var acknowledged *pushJournalAck
		for i, ack := range notice.Acks {
			at := timePointer(ack.ReceivedAt)
			switch ack.Event {
			case rpc.PushAckDisplayed:
				if fact.DisplayedAt == nil || ack.ReceivedAt.Before(*fact.DisplayedAt) {
					fact.DisplayedAt = at
				}
			case rpc.PushAckOpened:
				if fact.OpenedAt == nil || ack.ReceivedAt.Before(*fact.OpenedAt) {
					fact.OpenedAt = at
				}
			}
			if acknowledged == nil || ack.ReceivedAt.Before(acknowledged.ReceivedAt) {
				acknowledged = &notice.Acks[i]
			}
		}
		if acknowledged != nil {
			fact.AcknowledgedAt = timePointer(acknowledged.ReceivedAt)
			fact.AcknowledgedBy = s.deviceNameLocked(acknowledged.DeviceID)
		}
		out = append(out, fact)
	}
	return out
}

func timePointer(at time.Time) *time.Time {
	utc := at.UTC()
	return &utc
}

func clonePushJournal(in *pushJournal) *pushJournal {
	out := &pushJournal{Version: pushJournalVersion}
	if in == nil {
		return out
	}
	out.Notices = make([]pushJournalNotice, len(in.Notices))
	for i, notice := range in.Notices {
		notice.Attempts = slices.Clone(notice.Attempts)
		notice.Acks = slices.Clone(notice.Acks)
		out.Notices[i] = notice
	}
	return out
}

// decodePushJournal validates a persisted journal. The journal is evidence,
// not authority: an unreadable one is discarded (reading as "never
// witnessed") rather than keeping the app host from starting.
func decodePushJournal(raw json.RawMessage) (*pushJournal, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var journal pushJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		return nil, fmt.Errorf("%w: %v", errPushJournalInvalid, err)
	}
	if journal.Version != pushJournalVersion || len(journal.Notices) > pushJournalNoticeLimit {
		return nil, fmt.Errorf("%w: version or size", errPushJournalInvalid)
	}
	seen := map[string]bool{}
	for _, notice := range journal.Notices {
		if rpc.ValidatePushNoticeID(notice.NoticeID) != nil || seen[notice.NoticeID] || notice.FirstSentAt.IsZero() ||
			(notice.Kind != rpc.PushNoticeKindAlert && notice.Kind != rpc.PushNoticeKindDiagnostic) ||
			len(notice.Attempts) == 0 || len(notice.Attempts) > pushJournalAttemptLimit || len(notice.Acks) > pushJournalAckLimit {
			return nil, fmt.Errorf("%w: notice", errPushJournalInvalid)
		}
		seen[notice.NoticeID] = true
		for _, attempt := range notice.Attempts {
			if attempt.At.IsZero() || !validAlertHash(attempt.TargetRef) || strings.TrimSpace(attempt.Class) == "" ||
				(attempt.HTTPStatus != 0 && (attempt.HTTPStatus < 100 || attempt.HTTPStatus > 599)) {
				return nil, fmt.Errorf("%w: attempt", errPushJournalInvalid)
			}
		}
		for _, ack := range notice.Acks {
			if ack.ReceivedAt.IsZero() || strings.TrimSpace(ack.DeviceID) == "" || (ack.Event != rpc.PushAckDisplayed && ack.Event != rpc.PushAckOpened) {
				return nil, fmt.Errorf("%w: acknowledgement", errPushJournalInvalid)
			}
		}
	}
	return &journal, nil
}
