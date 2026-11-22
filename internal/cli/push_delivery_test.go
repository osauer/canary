package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestPushDeliveryLineNamesSilenceWitnessAndBlockers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	silentSince := time.Date(2026, 8, 11, 13, 42, 47, 0, time.UTC)
	rejected := time.Date(2026, 8, 15, 1, 53, 52, 0, time.UTC)
	silent := rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion, ReportedAt: now, Mode: "act_only",
		Dispatcher: "degraded", DispatcherClass: "producer_observation_rejected", IntakeRejectedSince: &rejected,
		ActiveSubscriptions: 3, SilentSince: &silentSince, Recent: []rpc.PushNoticeFact{},
	}
	line, ok := pushDeliveryLine(silent, now, time.UTC)
	want := "silent since 2026-08-11 13:42 (45 days) · never witnessed on a device · alert intake rejected since 2026-08-15 01:53"
	if ok || line != want {
		t.Fatalf("silent line = %q (ok %v)\nwant %q", line, ok, want)
	}

	opened := now.Add(-time.Minute)
	sent := now.Add(-2 * time.Minute)
	witnessed := rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion, ReportedAt: now, Mode: "act_only", Dispatcher: "healthy",
		ActiveSubscriptions: 1, SilentSince: &sent, Witnessed: true, Recent: []rpc.PushNoticeFact{},
		LastOpened: &rpc.PushAckFact{At: opened, Event: rpc.PushAckOpened, Kind: rpc.PushNoticeKindDiagnostic, NoticeID: "diagnostic-0123456789abcdef", Device: "iPhone", DeviceRef: "0123456789ab"},
	}
	line, ok = pushDeliveryLine(witnessed, now, time.UTC)
	if !ok || line != "last alert push 2026-09-25 17:58 · opened on iPhone 2026-09-25 17:59 (test)" {
		t.Fatalf("witnessed line = %q (ok %v)", line, ok)
	}
	tested := witnessed
	tested.LastSent = &rpc.PushSendFact{At: opened.Add(-30 * time.Second), Kind: rpc.PushNoticeKindDiagnostic, NoticeID: "diagnostic-0123456789abcdef", Class: "push_service_accepted", HTTPStatus: 201, Accepted: true}
	if line, _ = pushDeliveryLine(tested, now, time.UTC); !strings.Contains(line, "last push push_service_accepted 201 2026-09-25 17:58 (test)") {
		t.Fatalf("test push not named: %q", line)
	}

	expired := now.Add(-30 * time.Second)
	failing := witnessed
	failing.SubscriptionExpiredAt = &expired
	failing.ActiveSubscriptions = 0
	failing.LastSent = &rpc.PushSendFact{At: expired, Kind: rpc.PushNoticeKindAlert, NoticeID: "alert-0123456789abcdef", Class: "dead_subscription", HTTPStatus: 410}
	line, ok = pushDeliveryLine(failing, now, time.UTC)
	for _, part := range []string{"last push dead_subscription 410 2026-09-25 17:59", "a subscription expired 2026-09-25 17:59; enable notifications on the phone", "no push subscription"} {
		if !strings.Contains(line, part) {
			t.Fatalf("expired line %q lacks %q", line, part)
		}
	}
	if ok {
		t.Fatal("an expired, unsubscribed channel must not read as ok")
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	// The status row is one fact. An old report says the state is not known
	// and when the app last reported; it gives no setup instruction, because
	// the report cannot speak for the phone now.
	for _, proof := range []rpc.PushDeliveryProof{witnessed, failing} {
		got := formatPushDeliveryValueIn(env, rpc.PushDeliveryStatus{Proof: proof, ReceivedAt: now.Add(-time.Hour)}, now, time.UTC)
		if !strings.HasPrefix(got, "not known") || !strings.Contains(got, "2026-09-25 17:00") || strings.Contains(got, "enable notifications") || !strings.Contains(got, "canary app status") {
			t.Fatalf("an old report must read as not known, dated, with the place to look: %q", got)
		}
	}
	// A current report names the one thing in the way, or that it works.
	if got := formatPushDeliveryValueIn(env, rpc.PushDeliveryStatus{Proof: failing, ReceivedAt: now}, now, time.UTC); !strings.Contains(got, "no phone is subscribed") {
		t.Fatalf("unsubscribed phone not named: %q", got)
	}
	if got := formatPushDeliveryValueIn(env, rpc.PushDeliveryStatus{Proof: witnessed, ReceivedAt: now}, now, time.UTC); !strings.HasPrefix(got, "working") {
		t.Fatalf("a witnessed, healthy channel must read as working: %q", got)
	}
}

func TestRenderStatusPhonePushRowOnlyWhenTheAppReported(t *testing.T) {
	t.Parallel()
	base := func() *rpc.HealthResult {
		return &rpc.HealthResult{DaemonVersion: "v1.0.0", Account: "DU0000000", AccountMode: rpc.AccountModePaper,
			GatewayHost: "127.0.0.1", GatewayPort: 4002, PortOrigin: "discovered", ClientID: 17, Connected: true, ServerVersion: 178}
	}
	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, base(), nil)
	if strings.Contains(stdout.String(), "Phone push") {
		t.Fatalf("phone push row rendered without an app report:\n%s", stdout.String())
	}
	now := time.Now().UTC()
	res := base()
	res.PushDelivery = &rpc.PushDeliveryStatus{ReceivedAt: now, Proof: rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion, ReportedAt: now, Mode: "act_only", Dispatcher: "healthy", ActiveSubscriptions: 1, Recent: []rpc.PushNoticeFact{},
	}}
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, res, nil)
	if !strings.Contains(stdout.String(), "Phone push") || !strings.Contains(stdout.String(), "not yet seen on a phone") {
		t.Fatalf("phone push row missing:\n%s", stdout.String())
	}
}
