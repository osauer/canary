// Package push validates app-owned subscriptions and sends redacted Web Push
// payloads. It classifies transport outcomes for the app's durable delivery
// ledgers but does not decide alert eligibility or retain delivery state.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
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

// SafeDiagnosticPayload returns a fixed notification test containing no
// account, position, order, occurrence, or subscription data.
func SafeDiagnosticPayload() Payload {
	return Payload{
		Title: "Canary notification test", Body: "Safe test notification. No account data is included.",
		Destination: rpc.NudgeDestinationAlerts, DisplayID: "diagnostic-safe-test",
	}
}

// Subscriber is the VAPID contact claim presented to push services. Apple
// rejects localhost and malformed contact claims with 403 BadJwtToken.
const Subscriber = "https://osauer.dev"

// HTTPClient is the narrow transport surface needed to send a push request.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// WebPushSender sends payloads through a Web Push service. A nil Client uses
// http.DefaultClient.
type WebPushSender struct {
	Subscriber string
	Client     HTTPClient
}

// GenerateVAPIDKeys creates a private/public VAPID key pair for app-owned push
// subscriptions.
func GenerateVAPIDKeys() (privateKey, publicKey string, err error) {
	return generateVAPIDKeys()
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
	resp, err := sendWebPush(ctx, s.Client, webPushRequest{
		Endpoint:        sub.Endpoint,
		Auth:            sub.Auth,
		P256DH:          sub.P256DH,
		Payload:         body,
		Subscriber:      s.Subscriber,
		TTL:             60,
		Urgency:         "high",
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
