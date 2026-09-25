package daemon

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func testPushDeliveryProof(reportedAt time.Time) rpc.PushDeliveryProof {
	sent := reportedAt.Add(-2 * time.Minute)
	opened := reportedAt.Add(-time.Minute)
	return rpc.PushDeliveryProof{
		SchemaVersion: rpc.PushDeliveryProofVersion, ReportedAt: reportedAt,
		Mode: "act_only", Dispatcher: "healthy", ActiveSubscriptions: 1,
		SilentSince: &sent,
		LastSent: &rpc.PushSendFact{
			At: sent, Kind: rpc.PushNoticeKindDiagnostic, NoticeID: "diagnostic-0123456789abcdef",
			Class: "push_service_accepted", HTTPStatus: 201, Accepted: true,
		},
		LastDiagnosticSentAt: &sent,
		LastOpened: &rpc.PushAckFact{
			At: opened, Event: rpc.PushAckOpened, Kind: rpc.PushNoticeKindDiagnostic,
			NoticeID: "diagnostic-0123456789abcdef", Device: "iPhone", DeviceRef: "0123456789ab",
		},
		Witnessed: true,
		Recent: []rpc.PushNoticeFact{{
			NoticeID: "diagnostic-0123456789abcdef", Kind: rpc.PushNoticeKindDiagnostic, SentAt: sent,
			Class: "push_service_accepted", HTTPStatus: 201, Accepted: true, Targets: 1,
			OpenedAt: &opened, AcknowledgedAt: &opened, AcknowledgedBy: "iPhone",
		}},
	}
}

func TestPushDeliveryProofIsRetainedAcrossRestart(t *testing.T) {
	path := alertRegistryTestPath(t)
	core := openAlertRegistryTestStore(t, path)
	var authority pushDeliveryProofAuthority
	if err := authority.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if authority.current() != nil {
		t.Fatal("an unreported proof must read as absent")
	}
	reportedAt := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	receivedAt := reportedAt.Add(time.Second)
	res, err := authority.record(t.Context(), testPushDeliveryProof(reportedAt), receivedAt)
	if err != nil || !res.Accepted || !res.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("record = %+v, %v", res, err)
	}

	// An older report never rolls the evidence back.
	stale := testPushDeliveryProof(reportedAt.Add(-time.Hour))
	stale.Witnessed, stale.LastOpened, stale.Recent = false, nil, []rpc.PushNoticeFact{}
	if res, err := authority.record(t.Context(), stale, receivedAt.Add(time.Minute)); err != nil || res.Accepted {
		t.Fatalf("stale report = %+v, %v; want ignored", res, err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := openAlertRegistryTestStore(t, path)
	defer func() { _ = restarted.Close() }()
	var reloaded pushDeliveryProofAuthority
	if err := reloaded.bindCore(t.Context(), restarted); err != nil {
		t.Fatal(err)
	}
	status := reloaded.current()
	if status == nil || !status.Proof.Witnessed || !status.ReceivedAt.Equal(receivedAt) {
		t.Fatalf("restart lost the witnessed proof: %+v", status)
	}
	witness, ok := status.Proof.LastWitness()
	if !ok || witness.Event != rpc.PushAckOpened || witness.Device != "iPhone" {
		t.Fatalf("last witness = %+v, %v", witness, ok)
	}
}

func TestPushDeliveryProofRefusesMalformedEvidence(t *testing.T) {
	s := newTestServer(t)
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	for name, mutate := range map[string]func(*rpc.PushDeliveryProof){
		"schema":              func(p *rpc.PushDeliveryProof) { p.SchemaVersion = "push-delivery-proof-v0" },
		"acceptance witness":  func(p *rpc.PushDeliveryProof) { p.LastOpened = nil },
		"wrong ack event":     func(p *rpc.PushDeliveryProof) { p.LastOpened.Event = rpc.PushAckDisplayed },
		"notice id":           func(p *rpc.PushDeliveryProof) { p.LastSent.NoticeID = "../etc" },
		"device name control": func(p *rpc.PushDeliveryProof) { p.LastOpened.Device = "i\x00Phone" },
		"unacked by":          func(p *rpc.PushDeliveryProof) { p.Recent[0].AcknowledgedAt = nil },
	} {
		proof := testPushDeliveryProof(now)
		mutate(&proof)
		raw, err := json.Marshal(proof)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.handleAlertDeliveryProof(t.Context(), &rpc.Request{Method: rpc.MethodAlertDeliveryProof, Params: raw}); err == nil {
			t.Errorf("%s: malformed proof was accepted", name)
		}
	}
	raw, _ := json.Marshal(testPushDeliveryProof(now))
	res, err := s.handleAlertDeliveryProof(t.Context(), &rpc.Request{Method: rpc.MethodAlertDeliveryProof, Params: raw})
	if err != nil || !res.Accepted {
		t.Fatalf("valid proof = %+v, %v", res, err)
	}
	if got := s.pushDeliveryProof.current(); got == nil || !got.ReceivedAt.Equal(now) {
		t.Fatalf("retained proof = %+v", got)
	}
}

func TestPushDeliveryProofDiscardsCorruptDocumentWithoutBlockingStart(t *testing.T) {
	path := alertRegistryTestPath(t)
	core := openAlertRegistryTestStore(t, path)
	defer func() { _ = core.Close() }()
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: pushDeliveryProofStateKind, JSON: []byte(`{"version":1,"status":{"proof":{"schema_version":"bogus"},"received_at":"2026-09-26T07:00:00Z"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	var authority pushDeliveryProofAuthority
	if err := authority.bindCore(t.Context(), core); !errors.Is(err, errPushDeliveryProofDiscarded) {
		t.Fatalf("bind = %v, want a discarded proof", err)
	}
	if authority.current() != nil {
		t.Fatal("a discarded proof must read as absent, never as witnessed")
	}
	now := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	if res, err := authority.record(t.Context(), testPushDeliveryProof(now), now); err != nil || !res.Accepted {
		t.Fatalf("the next report must replace a discarded proof: %+v, %v", res, err)
	}
}
