package apphttp

import (
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// AlertPushSendDTO is the browser projection of the last journaled push.
type AlertPushSendDTO struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Class      string    `json:"class"`
	HTTPStatus int       `json:"http_status"`
	Accepted   bool      `json:"accepted"`
}

// AlertPushAckDTO is the browser projection of one device receipt.
type AlertPushAckDTO struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Device string    `json:"device"`
}

// AlertPushDeliveryDTO answers "did the phone get it?" for the Alerts
// surface: the last push sent, the last device receipts, and how long alert
// pushes have been silent. Witnessed means a paired device reported a
// notice displayed or opened; push-service acceptance never counts.
type AlertPushDeliveryDTO struct {
	LastSent              *AlertPushSendDTO `json:"last_sent"`
	LastAlertSentAt       *time.Time        `json:"last_alert_sent_at"`
	SilentSince           *time.Time        `json:"silent_since"`
	LastDisplayed         *AlertPushAckDTO  `json:"last_displayed"`
	LastOpened            *AlertPushAckDTO  `json:"last_opened"`
	Witnessed             bool              `json:"witnessed"`
	IntakeRejectedSince   *time.Time        `json:"intake_rejected_since"`
	SubscriptionExpiredAt *time.Time        `json:"subscription_expired_at"`
	ActiveSubscriptions   int               `json:"active_subscriptions"`
}

func newAlertPushDeliveryDTO(proof rpc.PushDeliveryProof) AlertPushDeliveryDTO {
	dto := AlertPushDeliveryDTO{
		LastAlertSentAt:       utcPointer(proof.LastAlertSentAt),
		SilentSince:           utcPointer(proof.SilentSince),
		Witnessed:             proof.Witnessed,
		IntakeRejectedSince:   utcPointer(proof.IntakeRejectedSince),
		SubscriptionExpiredAt: utcPointer(proof.SubscriptionExpiredAt),
		ActiveSubscriptions:   proof.ActiveSubscriptions,
	}
	if sent := proof.LastSent; sent != nil {
		dto.LastSent = &AlertPushSendDTO{At: sent.At.UTC(), Kind: sent.Kind, Class: sent.Class, HTTPStatus: sent.HTTPStatus, Accepted: sent.Accepted}
	}
	if ack := proof.LastDisplayed; ack != nil {
		dto.LastDisplayed = &AlertPushAckDTO{At: ack.At.UTC(), Kind: ack.Kind, Device: ack.Device}
	}
	if ack := proof.LastOpened; ack != nil {
		dto.LastOpened = &AlertPushAckDTO{At: ack.At.UTC(), Kind: ack.Kind, Device: ack.Device}
	}
	return dto
}

func utcPointer(at *time.Time) *time.Time {
	if at == nil || at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}
