package alerts

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestDiagnosticPushTravelsTheJournalButNeverCountsAsAnAlert(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	sender := &recordingSender{at: now}
	d, store := pushDeliveryTestDispatcher(t, sender, now)
	result, err := d.SendDiagnostic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.State != state.GovernanceTransportAccepted || len(result.Targets) != 2 || !strings.HasPrefix(result.NoticeID, "diagnostic-") {
		t.Fatalf("diagnostic result = %+v", result)
	}
	for _, target := range result.Targets {
		if target.HTTPStatus != 201 || target.DeviceRef == "" || strings.Contains(target.DeviceRef, "grant") {
			t.Fatalf("target = %+v", target)
		}
	}
	for _, payload := range sender.payloads {
		if payload.NoticeID != result.NoticeID || payload.Destination != rpc.NudgeDestinationAlerts || payload.Title != "Canary notification test" {
			t.Fatalf("diagnostic payload = %+v", payload)
		}
	}
	if got := store.AcceptedAlertPushesSince(now.Add(-time.Hour)); got != 0 {
		t.Fatalf("diagnostic pushes counted toward the runaway fuse: %d", got)
	}
	if view := store.AlertDelivery(now); len(view.Occurrences) != 0 || view.Attention.UnreadCount != 0 {
		t.Fatalf("diagnostic entered the alert ledger: %+v", view)
	}
	proof := store.PushDeliveryProof(now)
	if proof.SilentSince != nil || proof.LastAlertSentAt != nil || proof.LastDiagnosticSentAt == nil || proof.LastSent.Kind != rpc.PushNoticeKindDiagnostic {
		t.Fatalf("diagnostic counted as an alert: %+v", proof)
	}

	// The phone's own button targets only its device.
	own, err := d.SendSafeDiagnostic(t.Context(), "phone-grant")
	if err != nil || len(own.Targets) != 1 || own.Targets[0].Device != "iPhone" || own.NoticeID == result.NoticeID {
		t.Fatalf("device diagnostic = %+v, %v", own, err)
	}
	none, err := d.SendSafeDiagnostic(t.Context(), "unsubscribed-grant")
	if err != nil || none.State != state.GovernanceTransportNoSubscription || none.NoticeID != "" || none.Accepted {
		t.Fatalf("unsubscribed device diagnostic = %+v, %v", none, err)
	}
}

func TestDiagnosticPushReportsAnExpiredSubscription(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	sender := &recordingSender{at: now, status: 410, class: state.GovernanceTransportDead}
	d, store := pushDeliveryTestDispatcher(t, sender, now)
	result, err := d.SendDiagnostic(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted || result.State != state.GovernanceTransportAllFailed || len(store.ActivePushSubscriptions()) != 0 {
		t.Fatalf("expired result = %+v, subscriptions left %d", result, len(store.ActivePushSubscriptions()))
	}
	proof := store.PushDeliveryProof(now)
	if proof.SubscriptionExpiredAt == nil || !proof.SubscriptionExpiredAt.Equal(now) || proof.ActiveSubscriptions != 0 || proof.LastSent.HTTPStatus != 410 {
		t.Fatalf("expired subscription is not named: %+v", proof)
	}
}
