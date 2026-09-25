package rpc

import (
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode"
)

// MethodAlertDeliveryProof records the app host's redacted Web Push delivery
// proof: what it last sent and what a paired device last acknowledged. The
// daemon keeps the latest report for status and for gating pre-authorised
// automation on a witnessed phone channel. The method grants no delivery,
// alert, policy, or broker authority.
const MethodAlertDeliveryProof = "alerts.delivery_proof"

// PushDeliveryProofVersion identifies the relayed proof schema.
const PushDeliveryProofVersion = "push-delivery-proof-v1"

// Push notice kinds. A diagnostic notice travels the same transport, journal,
// and acknowledgement path as an alert but never counts as an alert.
const (
	PushNoticeKindAlert      = "alert"
	PushNoticeKindDiagnostic = "diagnostic"
)

// Push acknowledgement events a paired device reports for a notice. A notice
// is witnessed only by one of these receipts from a device; push-service
// acceptance proves transport, never display or reading.
const (
	PushAckDisplayed = "displayed"
	PushAckOpened    = "opened"
)

// PushDeliveryRecentLimit bounds the per-notice facts carried in one proof.
const PushDeliveryRecentLimit = 16

// PushSendFact is one journaled Web Push request as the push service answered
// it. Accepted means the push service took the request, nothing more.
type PushSendFact struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	NoticeID   string    `json:"notice_id"`
	Class      string    `json:"class"`
	HTTPStatus int       `json:"http_status,omitempty"`
	Accepted   bool      `json:"accepted"`
}

// PushAckFact is one acknowledgement a paired device reported for a notice.
// At is the app host's receipt clock; DeviceAt is the device's own claim.
type PushAckFact struct {
	At        time.Time `json:"at"`
	DeviceAt  time.Time `json:"device_at,omitzero"`
	Event     string    `json:"event"`
	Kind      string    `json:"kind"`
	NoticeID  string    `json:"notice_id"`
	Device    string    `json:"device"`
	DeviceRef string    `json:"device_ref"`
}

// PushNoticeFact summarizes one recent notice: its latest send and the first
// time a device reported it displayed or opened.
type PushNoticeFact struct {
	NoticeID       string     `json:"notice_id"`
	Kind           string     `json:"kind"`
	SentAt         time.Time  `json:"sent_at"`
	Class          string     `json:"class"`
	HTTPStatus     int        `json:"http_status,omitempty"`
	Accepted       bool       `json:"accepted"`
	Targets        int        `json:"targets"`
	DisplayedAt    *time.Time `json:"displayed_at,omitempty"`
	OpenedAt       *time.Time `json:"opened_at,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	AcknowledgedBy string     `json:"acknowledged_by,omitempty"`
}

// PushDeliveryProof is the app host's redacted delivery evidence. It carries
// no endpoint, key, account, position, or alert-evidence data.
type PushDeliveryProof struct {
	SchemaVersion       string     `json:"schema_version"`
	ReportedAt          time.Time  `json:"reported_at"`
	Mode                string     `json:"mode"`
	Dispatcher          string     `json:"dispatcher"`
	DispatcherClass     string     `json:"dispatcher_class,omitempty"`
	IntakeRejectedSince *time.Time `json:"intake_rejected_since,omitempty"`
	ActiveSubscriptions int        `json:"active_subscriptions"`
	// SilentSince is the last push-service acceptance of an alert push: no
	// alert can have reached a phone after it. Nil means none on record.
	SilentSince          *time.Time    `json:"silent_since,omitempty"`
	LastSent             *PushSendFact `json:"last_sent,omitempty"`
	LastAlertSentAt      *time.Time    `json:"last_alert_sent_at,omitempty"`
	LastDiagnosticSentAt *time.Time    `json:"last_diagnostic_sent_at,omitempty"`
	// SubscriptionExpiredAt is the latest push the push service answered
	// with 404/410: that subscription is gone and was removed, so the device
	// must enable notifications again to be reachable.
	SubscriptionExpiredAt *time.Time       `json:"subscription_expired_at,omitempty"`
	LastDisplayed         *PushAckFact     `json:"last_displayed,omitempty"`
	LastOpened            *PushAckFact     `json:"last_opened,omitempty"`
	Witnessed             bool             `json:"witnessed"`
	Recent                []PushNoticeFact `json:"recent"`
}

// LastWitness returns the most recent displayed or opened receipt.
func (p PushDeliveryProof) LastWitness() (PushAckFact, bool) {
	switch {
	case p.LastDisplayed == nil && p.LastOpened == nil:
		return PushAckFact{}, false
	case p.LastDisplayed == nil:
		return *p.LastOpened, true
	case p.LastOpened == nil || p.LastDisplayed.At.After(p.LastOpened.At):
		return *p.LastDisplayed, true
	default:
		return *p.LastOpened, true
	}
}

// AlertDeliveryProofResult reports whether the daemon kept the report. A
// report older than the retained one is ignored, not an error.
type AlertDeliveryProofResult struct {
	Accepted   bool      `json:"accepted"`
	ReceivedAt time.Time `json:"received_at"`
}

// PushDeliveryStatus is the daemon's retained copy of the latest proof and
// the daemon clock at which it arrived; an old ReceivedAt means the app host
// has stopped reporting.
type PushDeliveryStatus struct {
	Proof      PushDeliveryProof `json:"proof"`
	ReceivedAt time.Time         `json:"received_at"`
}

var (
	pushCodePattern      = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	pushNoticeIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{2,95}$`)
	pushDeviceRefPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// ValidatePushNoticeID accepts the opaque notice identities the app mints:
// an alert display id or a diagnostic notice id.
func ValidatePushNoticeID(id string) error {
	if !pushNoticeIDPattern.MatchString(id) {
		return errors.New("invalid push notice id")
	}
	return nil
}

// ValidatePushDeliveryProof checks the relayed proof's shape and vocabulary.
// It does not judge delivery health; it only refuses malformed evidence.
func ValidatePushDeliveryProof(p PushDeliveryProof) error {
	if p.SchemaVersion != PushDeliveryProofVersion {
		return fmt.Errorf("unsupported push delivery proof schema %q", p.SchemaVersion)
	}
	if p.ReportedAt.IsZero() {
		return errors.New("push delivery proof requires reported_at")
	}
	switch p.Mode {
	case "none", "act_only", "watch_and_act":
	default:
		return fmt.Errorf("invalid push delivery mode %q", p.Mode)
	}
	if !pushCodePattern.MatchString(p.Dispatcher) {
		return errors.New("invalid push dispatcher state")
	}
	if p.DispatcherClass != "" && !pushCodePattern.MatchString(p.DispatcherClass) {
		return errors.New("invalid push dispatcher class")
	}
	if p.ActiveSubscriptions < 0 || p.ActiveSubscriptions > 4096 {
		return errors.New("invalid active push subscription count")
	}
	for _, clock := range []*time.Time{p.IntakeRejectedSince, p.SilentSince, p.LastAlertSentAt, p.LastDiagnosticSentAt, p.SubscriptionExpiredAt} {
		if clock != nil && clock.IsZero() {
			return errors.New("push delivery proof clocks must be absent or set")
		}
	}
	if p.LastSent != nil {
		if err := validatePushSendFact(*p.LastSent); err != nil {
			return err
		}
	}
	for _, ack := range []*PushAckFact{p.LastDisplayed, p.LastOpened} {
		if ack != nil {
			if err := validatePushAckFact(*ack); err != nil {
				return err
			}
		}
	}
	if p.LastDisplayed != nil && p.LastDisplayed.Event != PushAckDisplayed {
		return errors.New("last_displayed must carry a displayed receipt")
	}
	if p.LastOpened != nil && p.LastOpened.Event != PushAckOpened {
		return errors.New("last_opened must carry an opened receipt")
	}
	if p.Witnessed != (p.LastDisplayed != nil || p.LastOpened != nil) {
		return errors.New("witnessed must reflect a displayed or opened receipt")
	}
	if len(p.Recent) > PushDeliveryRecentLimit {
		return errors.New("too many recent push notices")
	}
	for _, notice := range p.Recent {
		if err := validatePushNoticeFact(notice); err != nil {
			return err
		}
	}
	return nil
}

func validatePushKind(kind string) error {
	if kind != PushNoticeKindAlert && kind != PushNoticeKindDiagnostic {
		return fmt.Errorf("invalid push notice kind %q", kind)
	}
	return nil
}

func validatePushHTTPStatus(status int) error {
	if status != 0 && (status < 100 || status > 599) {
		return errors.New("invalid push http status")
	}
	return nil
}

func validatePushSendFact(f PushSendFact) error {
	if f.At.IsZero() {
		return errors.New("push send fact requires at")
	}
	if err := validatePushKind(f.Kind); err != nil {
		return err
	}
	if err := ValidatePushNoticeID(f.NoticeID); err != nil {
		return err
	}
	if !pushCodePattern.MatchString(f.Class) {
		return errors.New("invalid push transport class")
	}
	return validatePushHTTPStatus(f.HTTPStatus)
}

func validatePushAckFact(f PushAckFact) error {
	if f.At.IsZero() {
		return errors.New("push acknowledgement requires at")
	}
	if f.Event != PushAckDisplayed && f.Event != PushAckOpened {
		return fmt.Errorf("invalid push acknowledgement event %q", f.Event)
	}
	if err := validatePushKind(f.Kind); err != nil {
		return err
	}
	if err := ValidatePushNoticeID(f.NoticeID); err != nil {
		return err
	}
	if !pushDeviceRefPattern.MatchString(f.DeviceRef) {
		return errors.New("invalid push acknowledgement device ref")
	}
	return validatePushDeviceName(f.Device)
}

func validatePushNoticeFact(f PushNoticeFact) error {
	if err := ValidatePushNoticeID(f.NoticeID); err != nil {
		return err
	}
	if err := validatePushKind(f.Kind); err != nil {
		return err
	}
	if f.SentAt.IsZero() || !pushCodePattern.MatchString(f.Class) || f.Targets < 1 || f.Targets > 4096 {
		return errors.New("invalid recent push notice")
	}
	if err := validatePushHTTPStatus(f.HTTPStatus); err != nil {
		return err
	}
	for _, clock := range []*time.Time{f.DisplayedAt, f.OpenedAt, f.AcknowledgedAt} {
		if clock != nil && clock.IsZero() {
			return errors.New("recent push notice clocks must be absent or set")
		}
	}
	if (f.AcknowledgedAt == nil) != (f.DisplayedAt == nil && f.OpenedAt == nil) {
		return errors.New("acknowledged_at must reflect a displayed or opened receipt")
	}
	if f.AcknowledgedAt == nil && f.AcknowledgedBy != "" {
		return errors.New("acknowledged_by requires acknowledged_at")
	}
	if f.AcknowledgedBy != "" {
		return validatePushDeviceName(f.AcknowledgedBy)
	}
	return nil
}

// validatePushDeviceName bounds the operator-chosen paired-device label that
// names a witness ("iPhone"); it must be short printable text.
func validatePushDeviceName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("invalid push device name")
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return errors.New("invalid push device name")
		}
	}
	return nil
}
