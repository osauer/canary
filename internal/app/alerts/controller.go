package alerts

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

// SetAlertMode serializes a notification-mode change with confirmation and
// transport. Once this call commits, no already-confirmed send can still be in
// flight behind it.
func (d *Dispatcher) SetAlertMode(mode string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return errors.New("alert delivery store unavailable")
	}
	if err := d.Store.SetAlertMode(mode); err != nil {
		return err
	}
	_, err := d.refreshTransportReadinessLocked(d.now())
	return err
}

// AddDevice serializes device creation and terminal device revocation with
// alert transport. Store remains the durable device authority.
func (d *Dispatcher) AddDevice(device state.DeviceGrant) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return errors.New("alert delivery store unavailable")
	}
	if err := d.Store.AddDevice(device); err != nil {
		return err
	}
	_, err := d.refreshTransportReadinessLocked(d.now())
	return err
}

// PruneDevices serializes device and target retirement with alert transport.
func (d *Dispatcher) PruneDevices(cutoff time.Time) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return 0, errors.New("alert delivery store unavailable")
	}
	removed, err := d.Store.PruneDevices(cutoff)
	if err != nil {
		return removed, err
	}
	_, err = d.refreshTransportReadinessLocked(d.now())
	return removed, err
}

// AddPushSubscription serializes subscription creation, refresh, and endpoint
// transfer with alert transport.
func (d *Dispatcher) AddPushSubscription(subscription state.PushSubscription) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return errors.New("alert delivery store unavailable")
	}
	if err := d.Store.AddPushSubscription(subscription); err != nil {
		return err
	}
	_, err := d.refreshTransportReadinessLocked(d.now())
	return err
}

// RemovePushSubscription serializes target retirement with alert transport.
func (d *Dispatcher) RemovePushSubscription(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return errors.New("alert delivery store unavailable")
	}
	if err := d.Store.RemovePushSubscription(id); err != nil {
		return err
	}
	_, err := d.refreshTransportReadinessLocked(d.now())
	return err
}

// DiagnosticTarget is one subscription's redacted transport result for a
// diagnostic push.
type DiagnosticTarget struct {
	Device     string `json:"device"`
	DeviceRef  string `json:"device_ref"`
	Class      string `json:"class"`
	HTTPStatus int    `json:"http_status,omitempty"`
	Accepted   bool   `json:"push_service_accepted"`
}

// DiagnosticResult reports one diagnostic push. NoticeID is the journal
// identity a device acknowledges when it displays or opens the notification.
// Acceptance proves only that the push service took the request.
type DiagnosticResult struct {
	NoticeID string             `json:"notice_id,omitempty"`
	State    string             `json:"state"`
	Accepted bool               `json:"push_service_accepted"`
	Targets  []DiagnosticTarget `json:"targets"`
}

// SendSafeDiagnostic sends the fixed diagnostic notification to the active
// subscriptions for deviceID (the phone's own "send test" button). See
// SendDiagnostic for the transport and journal contract.
func (d *Dispatcher) SendSafeDiagnostic(ctx context.Context, deviceID string) (DiagnosticResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return DiagnosticResult{}, errors.New("alert delivery store unavailable")
	}
	return d.sendDiagnosticLocked(ctx, func() []state.PushSubscription { return d.Store.ActivePushSubscriptionsForDevice(deviceID) })
}

// SendDiagnostic sends the fixed diagnostic notification to every active
// subscription (the local `canary app push-test`). Each attempt is journaled
// as a diagnostic notice under one fresh notice id and travels the same
// transport and acknowledgement path as an alert, but it never enters the
// alert ledger, the inbox, or the runaway fuse count. The dispatcher lock
// serializes diagnostic transport and dead-target retirement with mode and
// topology changes; store methods release their own mutex before the
// external sender is called.
func (d *Dispatcher) SendDiagnostic(ctx context.Context) (DiagnosticResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return DiagnosticResult{}, errors.New("alert delivery store unavailable")
	}
	return d.sendDiagnosticLocked(ctx, d.Store.ActivePushSubscriptions)
}

func (d *Dispatcher) sendDiagnosticLocked(ctx context.Context, targets func() []state.PushSubscription) (DiagnosticResult, error) {
	now := d.now()
	readiness, err := d.refreshTransportReadinessLocked(now)
	if err != nil {
		return DiagnosticResult{}, err
	}
	result := DiagnosticResult{Targets: []DiagnosticTarget{}}
	if !readiness.enabled {
		result.State = state.GovernanceTransportSuppressed
		return result, d.Store.RecordDiagnosticStatus(state.GovernanceDiagnosticStatus{State: result.State, At: now})
	}
	subscriptions := targets()
	stateClass := state.GovernanceTransportNoSubscription
	if len(subscriptions) > 0 {
		noticeID, err := d.newNoticeID()
		if err != nil {
			return DiagnosticResult{}, err
		}
		result.NoticeID = noticeID
	}
	accepted, failed := 0, 0
	keys, hasKeys := d.Store.VAPID()
	removedDead := false
	for _, subscription := range subscriptions {
		attempt := state.PushAttempt{Class: state.GovernanceTransportSenderMissing}
		if !hasKeys {
			attempt.Class = state.GovernanceTransportMissingKeys
		} else if d.Sender != nil {
			sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout())
			attempt = d.Sender.Send(sendCtx, subscription, keys, push.SafeDiagnosticPayload(result.NoticeID))
			cancel()
		}
		class := attempt.Class
		if attempt.OK {
			class = state.GovernanceTransportAccepted
		}
		if class == "" || class == state.GovernanceTransportAccepted && !attempt.OK {
			class = state.GovernanceTransportHTTPRejected
		}
		attempt.Class = class
		d.journal(rpc.PushNoticeKindDiagnostic, result.NoticeID, subscription, attempt, d.now())
		device := d.Store.DeviceName(subscription.DeviceID)
		result.Targets = append(result.Targets, DiagnosticTarget{
			Device: device, DeviceRef: state.PushDeviceRef(subscription.DeviceID),
			Class: class, HTTPStatus: attempt.StatusCode, Accepted: class == state.GovernanceTransportAccepted,
		})
		if class == state.GovernanceTransportAccepted {
			accepted++
		} else {
			failed++
			stateClass = class
		}
		if class == state.GovernanceTransportDead {
			if err := d.Store.RemovePushSubscriptionAt(subscription.ID, d.now()); err != nil {
				return result, err
			}
			removedDead = true
		}
	}
	if removedDead {
		if _, err := d.refreshTransportReadinessLocked(d.now()); err != nil {
			return result, err
		}
	}
	if accepted > 0 && failed == 0 {
		stateClass = state.GovernanceTransportAccepted
	} else if accepted > 0 {
		stateClass = state.GovernanceTransportPartial
	} else if len(subscriptions) > 1 && failed > 0 {
		stateClass = state.GovernanceTransportAllFailed
	}
	result.State = stateClass
	result.Accepted = accepted > 0
	if err := d.Store.RecordDiagnosticStatus(state.GovernanceDiagnosticStatus{State: stateClass, At: now}); err != nil {
		return result, err
	}
	return result, nil
}

func (d *Dispatcher) newNoticeID() (string, error) {
	if d.NewNoticeID != nil {
		return d.NewNoticeID()
	}
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return state.NewPushNoticeID(random), nil
}
