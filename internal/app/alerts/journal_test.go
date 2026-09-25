package alerts

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/push"
	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

type recordingSender struct {
	mu       sync.Mutex
	payloads []push.Payload
	status   int
	class    string
	at       time.Time
}

func (s *recordingSender) Send(_ context.Context, sub state.PushSubscription, _ state.VAPIDKeys, payload push.Payload) state.PushAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payloads = append(s.payloads, payload)
	ok := s.class == "" || s.class == state.GovernanceTransportAccepted
	class := s.class
	if class == "" {
		class = state.GovernanceTransportAccepted
	}
	status := s.status
	if status == 0 {
		status = 201
	}
	return state.PushAttempt{At: s.at, SubscriptionID: sub.ID, OK: ok, StatusCode: status, Class: class}
}

func pushDeliveryTestDispatcher(t *testing.T, sender push.Sender, now time.Time) (*Dispatcher, *state.Store) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureVAPID(now, func() (string, string, error) { return "private", "public", nil }); err != nil {
		t.Fatal(err)
	}
	for _, device := range []state.DeviceGrant{{ID: "phone-grant", Name: "iPhone", CreatedAt: now}, {ID: "laptop-grant", Name: "Browser", CreatedAt: now}} {
		if err := store.AddDevice(device); err != nil {
			t.Fatal(err)
		}
	}
	for i, device := range []string{"phone-grant", "laptop-grant"} {
		if err := store.AddPushSubscription(state.PushSubscription{
			ID: "sub-" + device, DeviceID: device, Endpoint: "https://push.invalid/" + string(rune('a'+i)), P256DH: "p", Auth: "a", CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetAlertMode(state.AlertModeWatchAndAct); err != nil {
		t.Fatal(err)
	}
	counter := 0
	d := &Dispatcher{
		Store: store, Sender: sender, URL: "https://relay.example", Now: func() time.Time { return now },
		NewNoticeID: func() (string, error) {
			counter++
			return state.NewPushNoticeID([8]byte{byte(counter), 0xab, 0xcd, 0xef, 1, 2, 3, 4}), nil
		},
	}
	return d, store
}

func pushDeliveryTestSnapshot(t *testing.T, at time.Time, candidates ...rpc.AlertCandidate) rpc.AlertCandidateSnapshot {
	t.Helper()
	scope, err := rpc.BuildAlertAuthorityScope("SYNTHETIC", "paper")
	if err != nil {
		t.Fatal(err)
	}
	current := rpc.AlertSnapshotClear
	if len(candidates) > 0 {
		current = rpc.AlertSnapshotActive
	}
	if candidates == nil {
		candidates = []rpc.AlertCandidate{}
	}
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: scope, AsOf: at, CurrentState: current,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageComplete, Freshness: rpc.AlertCoverageCurrent, AsOf: at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceRiskPolicy}, CoveredSources: []rpc.AlertSource{rpc.AlertSourceRiskPolicy},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source: rpc.AlertSourceRiskPolicy, Status: "current", Reason: "current", EvidenceHealth: rpc.AlertEvidenceCurrent, Covered: true,
			InputAsOf: at, ObservedAt: at, EvidenceAsOf: at, FreshUntil: at.Add(time.Hour),
		}},
		Candidates: candidates,
	}
}

func pushDeliveryTestCandidate(t *testing.T, at time.Time) rpc.AlertCandidate {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceRiskPolicy, rpc.AlertKindDrawdown, "latch")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "sequence:1")
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence, EvidenceFingerprint: "sha256:" + strings.Repeat("b", 64),
		Source: rpc.AlertSourceRiskPolicy, Kind: rpc.AlertKindDrawdown, PresentationCode: rpc.AlertPresentationRiskPolicyDrawdownLatched,
		State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityAct, EvidenceHealth: rpc.AlertEvidenceCurrent,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}
}

func TestAlertDispatchJournalsEachSendUnderItsNoticeID(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	sender := &recordingSender{at: now}
	d, store := pushDeliveryTestDispatcher(t, sender, now)
	notified := 0
	d.OnJournal = func() { notified++ }
	if _, err := d.Observe(t.Context(), pushDeliveryTestSnapshot(t, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	view, err := d.Observe(t.Context(), pushDeliveryTestSnapshot(t, now, pushDeliveryTestCandidate(t, now)))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Occurrences) != 1 || len(sender.payloads) != 2 {
		t.Fatalf("occurrences=%d sends=%d, want one alert to two subscriptions", len(view.Occurrences), len(sender.payloads))
	}
	displayID := view.Occurrences[0].DisplayID
	for _, payload := range sender.payloads {
		if payload.NoticeID != displayID || payload.DisplayID != displayID {
			t.Fatalf("alert payload notice id = %q, display id %q, want %q", payload.NoticeID, payload.DisplayID, displayID)
		}
	}
	proof := store.PushDeliveryProof(now)
	if err := rpc.ValidatePushDeliveryProof(proof); err != nil {
		t.Fatal(err)
	}
	if proof.LastSent == nil || proof.LastSent.NoticeID != displayID || proof.LastSent.Kind != rpc.PushNoticeKindAlert || proof.LastSent.HTTPStatus != 201 {
		t.Fatalf("last send = %+v", proof.LastSent)
	}
	if proof.SilentSince == nil || !proof.SilentSince.Equal(now) || len(proof.Recent) != 1 || proof.Recent[0].Targets != 2 {
		t.Fatalf("journal = %+v", proof)
	}
	if notified != 2 {
		t.Fatalf("journal notifications = %d, want one per send", notified)
	}
}
