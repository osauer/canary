package alerts

import (
	"context"
	"errors"
	"time"

	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/state"
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

// SendSafeDiagnostic sends the fixed diagnostic notification to the active
// subscriptions for deviceID and durably records its redacted result. The
// dispatcher lock also serializes diagnostic transport and dead-target
// retirement with mode and topology changes. Store methods release their own
// mutex before the external sender is called.
func (d *Dispatcher) SendSafeDiagnostic(ctx context.Context, deviceID string) (state.GovernanceDiagnosticStatus, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Store == nil {
		return state.GovernanceDiagnosticStatus{}, false, errors.New("alert delivery store unavailable")
	}

	now := d.now()
	readiness, err := d.refreshTransportReadinessLocked(now)
	if err != nil {
		return state.GovernanceDiagnosticStatus{}, false, err
	}
	if !readiness.enabled {
		status := state.GovernanceDiagnosticStatus{State: state.GovernanceTransportSuppressed, At: now}
		return status, false, d.Store.RecordDiagnosticStatus(status)
	}

	subscriptions := d.Store.ActivePushSubscriptionsForDevice(deviceID)
	stateClass := state.GovernanceTransportNoSubscription
	accepted, failed := 0, 0
	keys, hasKeys := d.Store.VAPID()
	removedDead := false
	for _, subscription := range subscriptions {
		result := state.PushAttempt{Class: state.GovernanceTransportSenderMissing}
		if !hasKeys {
			result.Class = state.GovernanceTransportMissingKeys
		} else if d.Sender != nil {
			sendCtx, cancel := context.WithTimeout(ctx, d.sendTimeout())
			result = d.Sender.Send(sendCtx, subscription, keys, push.SafeDiagnosticPayload())
			cancel()
		}
		class := result.Class
		if result.OK {
			class = state.GovernanceTransportAccepted
		}
		if class == "" || class == state.GovernanceTransportAccepted && !result.OK {
			class = state.GovernanceTransportHTTPRejected
		}
		if class == state.GovernanceTransportAccepted {
			accepted++
		} else {
			failed++
			stateClass = class
		}
		if class == state.GovernanceTransportDead {
			if err := d.Store.RemovePushSubscriptionAt(subscription.ID, d.now()); err != nil {
				return state.GovernanceDiagnosticStatus{}, accepted > 0, err
			}
			removedDead = true
		}
	}
	if removedDead {
		if _, err := d.refreshTransportReadinessLocked(d.now()); err != nil {
			return state.GovernanceDiagnosticStatus{}, accepted > 0, err
		}
	}
	if accepted > 0 && failed == 0 {
		stateClass = state.GovernanceTransportAccepted
	} else if accepted > 0 {
		stateClass = state.GovernanceTransportPartial
	} else if len(subscriptions) > 1 && failed > 0 {
		stateClass = state.GovernanceTransportAllFailed
	}
	status := state.GovernanceDiagnosticStatus{State: stateClass, At: now}
	if err := d.Store.RecordDiagnosticStatus(status); err != nil {
		return state.GovernanceDiagnosticStatus{}, accepted > 0, err
	}
	return status, accepted > 0, nil
}
