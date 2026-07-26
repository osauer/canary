package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Sender transports one caller-selected, redacted payload and returns a
// classified attempt for the caller to persist. It does not grant eligibility.
type Sender interface {
	Send(context.Context, state.PushSubscription, state.VAPIDKeys, Payload) state.PushAttempt
}

// Payload is the allowlisted lock-screen surface. Callers must construct it
// from redacted presentation data rather than producer evidence or account
// state.
type Payload struct {
	Title       string `json:"title"`
	Body        string `json:"body"`
	Severity    string `json:"severity,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Destination string `json:"destination,omitempty"`
	DisplayID   string `json:"display_id,omitempty"`
	URL         string `json:"url,omitempty"`
	AlertID     string `json:"alert_id,omitempty"`
	Action      string `json:"action,omitempty"`
}

// GovernancePayload is the only constructor for governance Web Push copy.
// Canonicalization replaces every caller-supplied display field with the
// shared enum template, while the returned struct populates only the explicit
// lock-screen allowlist.
func GovernancePayload(candidate rpc.NudgeCandidate, displayID string) (Payload, error) {
	canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
		Fingerprint: candidate.Fingerprint, Kind: candidate.Kind, State: candidate.State,
		Severity: candidate.Severity, Title: candidate.Title, Body: candidate.Body,
		OccurredAt: candidate.OccurredAt, DueAt: candidate.DueAt, ExpiresAt: candidate.ExpiresAt,
		Destination: candidate.Destination,
	})
	if err != nil {
		return Payload{}, err
	}
	if !validGovernanceDisplayID(displayID) {
		return Payload{}, errors.New("invalid governance display id")
	}
	return Payload{
		Title: canonical.Title, Body: canonical.Body, Severity: canonical.Severity,
		Kind: canonical.Kind, Destination: canonical.Destination, DisplayID: displayID,
	}, nil
}

func validGovernanceDisplayID(displayID string) bool {
	if len(displayID) != len("gov-")+16 || !strings.HasPrefix(displayID, "gov-") {
		return false
	}
	for _, char := range displayID[len("gov-"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// SafeDiagnosticPayload returns a fixed notification test containing no
// account, position, order, occurrence, or subscription data.
func SafeDiagnosticPayload() Payload {
	return Payload{
		Title: "Canary notification test", Body: "Safe test notification. No account data is included.",
		Destination: rpc.NudgeDestinationAlerts, DisplayID: "diagnostic-safe-test",
	}
}

// Subscriber is the VAPID contact claim presented to push services. It must
// be an "https:" URL (webpush-go passes those through unchanged) or a bare
// email without the "mailto:" prefix (the library prepends exactly one).
// Never a "mailto:"-prefixed value — webpush-go v1.4.0 would double it into
// "mailto:mailto:…" — and never an @localhost address: Apple rejects both
// with 403 BadJwtToken, surfacing as http_rejected on every delivery.
const Subscriber = "https://osauer.dev"

// WebPushSender sends payloads through a Web Push service. A nil Client uses
// the webpush library's default HTTP client.
type WebPushSender struct {
	Subscriber string
	Client     webpush.HTTPClient
}

// GenerateVAPIDKeys creates a private/public VAPID key pair for app-owned push
// subscriptions.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return webpush.GenerateVAPIDKeys()
}

// Send validates sub, sends payload once, and classifies the response without
// persisting it. OK means the push service accepted the request, not that a
// device displayed or a human read the notification.
func (s WebPushSender) Send(ctx context.Context, sub state.PushSubscription, keys state.VAPIDKeys, payload Payload) state.PushAttempt {
	attempt := state.PushAttempt{At: time.Now().UTC(), SubscriptionID: sub.ID, AlertID: payload.AlertID}
	if err := ValidateSubscription(sub); err != nil {
		attempt.Error = err.Error()
		attempt.Class = state.GovernanceTransportMissingKeys
		return attempt
	}
	body, err := json.Marshal(payload)
	if err != nil {
		attempt.Error = err.Error()
		attempt.Class = state.GovernanceTransportHTTPRejected
		return attempt
	}
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{Auth: sub.Auth, P256dh: sub.P256DH},
	}, &webpush.Options{
		HTTPClient:      s.Client,
		Subscriber:      s.Subscriber,
		TTL:             60,
		Urgency:         webpush.UrgencyHigh,
		VAPIDPublicKey:  keys.PublicKey,
		VAPIDPrivateKey: keys.PrivateKey,
	})
	if err != nil {
		attempt.Error = err.Error()
		attempt.Class = classifyTransport(err, 0)
		return attempt
	}
	defer resp.Body.Close()
	attempt.Status = resp.Status
	attempt.OK = resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	attempt.Class = classifyTransport(nil, resp.StatusCode)
	if !attempt.OK {
		attempt.Error = fmt.Sprintf("push service returned %s", resp.Status)
	}
	return attempt
}

func classifyTransport(err error, status int) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return state.GovernanceTransportDeadlineRetry
	case errors.Is(err, context.Canceled):
		return state.GovernanceTransportCanceledRetry
	case err != nil:
		return state.GovernanceTransportNetworkRetry
	case status >= http.StatusOK && status < http.StatusMultipleChoices:
		return state.GovernanceTransportAccepted
	case status == http.StatusNotFound || status == http.StatusGone:
		return state.GovernanceTransportDead
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError:
		return state.GovernanceTransportHTTPRetry
	default:
		return state.GovernanceTransportHTTPRejected
	}
}

// ValidateSubscription requires the endpoint and both Web Push key fields;
// it does not establish that the subscription's paired device is active.
func ValidateSubscription(sub state.PushSubscription) error {
	if sub.Endpoint == "" {
		return fmt.Errorf("endpoint required")
	}
	if sub.P256DH == "" {
		return fmt.Errorf("p256dh key required")
	}
	if sub.Auth == "" {
		return fmt.Errorf("auth key required")
	}
	return nil
}
