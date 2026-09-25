package state

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func pushJournalTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	for _, device := range []DeviceGrant{{ID: "phone-grant", Name: "iPhone", CreatedAt: created}, {ID: "laptop-grant", Name: "Browser", CreatedAt: created}} {
		if err := store.AddDevice(device); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddPushSubscription(PushSubscription{ID: "sub-1", DeviceID: "phone-grant", Endpoint: "https://push.invalid/1", P256DH: "p", Auth: "a", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	return store, dir
}

func TestPushJournalRecordsEverySendAndOnlyDeviceReceiptsWitness(t *testing.T) {
	store, dir := pushJournalTestStore(t)
	sub := store.PushSubscriptions()[0]
	sentAt := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	const notice = "diagnostic-0123456789abcdef"
	if err := store.RecordPushAttempt(rpc.PushNoticeKindDiagnostic, notice, sub, PushAttempt{At: sentAt, OK: true, StatusCode: 201, Class: GovernanceTransportAccepted}); err != nil {
		t.Fatal(err)
	}

	// Push-service acceptance alone is not a witness.
	proof := store.PushDeliveryProof(sentAt.Add(time.Second))
	if err := rpc.ValidatePushDeliveryProof(proof); err != nil {
		t.Fatalf("proof does not validate: %v", err)
	}
	if proof.Witnessed || proof.LastSent == nil || !proof.LastSent.Accepted || proof.LastSent.HTTPStatus != 201 || proof.LastSent.Kind != rpc.PushNoticeKindDiagnostic {
		t.Fatalf("acceptance proof = %+v", proof)
	}
	if proof.SilentSince != nil || proof.LastAlertSentAt != nil {
		t.Fatalf("a diagnostic push counted as an alert: %+v", proof)
	}

	// Receipts bind to journaled notices and to an active paired device.
	if _, err := store.RecordPushAck("diagnostic-ffffffffffffffff", "phone-grant", rpc.PushAckOpened, time.Time{}, sentAt.Add(time.Minute)); !errors.Is(err, ErrPushNoticeUnknown) {
		t.Fatalf("receipt for an unsent notice = %v", err)
	}
	if _, err := store.RecordPushAck(notice, "never-paired", rpc.PushAckOpened, time.Time{}, sentAt.Add(time.Minute)); !errors.Is(err, ErrPushAckDeviceGone) {
		t.Fatalf("receipt from an unknown device = %v", err)
	}
	if _, err := store.RecordPushAck(notice, "phone-grant", "read", time.Time{}, sentAt.Add(time.Minute)); !errors.Is(err, ErrPushAckInvalid) {
		t.Fatalf("receipt with an unknown event = %v", err)
	}
	if _, err := store.RecordPushAck(notice, "phone-grant", rpc.PushAckDisplayed, time.Time{}, sentAt.Add(-time.Minute)); !errors.Is(err, ErrPushAckInvalid) {
		t.Fatalf("receipt before the send = %v", err)
	}

	deviceClock := sentAt.Add(2 * time.Second)
	displayed, err := store.RecordPushAck(notice, "phone-grant", rpc.PushAckDisplayed, deviceClock, sentAt.Add(3*time.Second))
	if err != nil || !displayed.Recorded || displayed.Kind != rpc.PushNoticeKindDiagnostic {
		t.Fatalf("displayed receipt = %+v, %v", displayed, err)
	}
	if again, err := store.RecordPushAck(notice, "phone-grant", rpc.PushAckDisplayed, deviceClock, sentAt.Add(time.Hour)); err != nil || again.Recorded || !again.ReceivedAt.Equal(displayed.ReceivedAt) {
		t.Fatalf("duplicate receipt = %+v, %v; want the first receipt unchanged", again, err)
	}
	openedAt := sentAt.Add(10 * time.Second)
	if opened, err := store.RecordPushAck(notice, "phone-grant", rpc.PushAckOpened, time.Time{}, openedAt); err != nil || !opened.Recorded {
		t.Fatalf("opened receipt = %+v, %v", opened, err)
	}

	proof = store.PushDeliveryProof(openedAt.Add(time.Second))
	if err := rpc.ValidatePushDeliveryProof(proof); err != nil {
		t.Fatalf("witnessed proof does not validate: %v", err)
	}
	witness, ok := proof.LastWitness()
	if !proof.Witnessed || !ok || witness.Event != rpc.PushAckOpened || witness.Device != "iPhone" || witness.DeviceRef != PushDeviceRef("phone-grant") {
		t.Fatalf("witness = %+v (proof %+v)", witness, proof)
	}
	if proof.LastDisplayed == nil || !proof.LastDisplayed.DeviceAt.Equal(deviceClock) {
		t.Fatalf("displayed receipt lost the device clock: %+v", proof.LastDisplayed)
	}
	if len(proof.Recent) != 1 || proof.Recent[0].AcknowledgedBy != "iPhone" || proof.Recent[0].OpenedAt == nil || proof.Recent[0].Targets != 1 {
		t.Fatalf("recent notice = %+v", proof.Recent)
	}
	raw, _ := json.Marshal(proof)
	for _, private := range []string{"phone-grant", "push.invalid", sub.ID} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("proof leaked private identity %q: %s", private, raw)
		}
	}

	// The journal is durable across a restart.
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.PushDeliveryProof(openedAt); !got.Witnessed || got.LastOpened == nil {
		t.Fatalf("restart lost the receipts: %+v", got)
	}
}

func TestPushJournalBoundsNoticesAndDiscardsACorruptJournal(t *testing.T) {
	store, dir := pushJournalTestStore(t)
	sub := store.PushSubscriptions()[0]
	base := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	for i := range pushJournalNoticeLimit + 5 {
		id := NewPushNoticeID([8]byte{byte(i), 1, 2, 3, 4, 5, 6, 7})
		if err := store.RecordPushAttempt(rpc.PushNoticeKindDiagnostic, id, sub, PushAttempt{At: base.Add(time.Duration(i) * time.Minute), Class: GovernanceTransportHTTPRejected, StatusCode: 403}); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	kept := len(store.data.PushJournal.Notices)
	oldest := store.data.PushJournal.Notices[0].FirstSentAt
	store.mu.Unlock()
	if kept != pushJournalNoticeLimit || !oldest.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("journal kept %d notices from %s", kept, oldest)
	}
	if last := store.PushDeliveryProof(base.Add(3 * time.Hour)).LastSent; last == nil || last.Accepted || last.HTTPStatus != 403 {
		t.Fatalf("a refused push must stay visible as the last send: %+v", last)
	}

	path := dir + "/state.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	top["push_journal"] = json.RawMessage(`{"version":"push-journal-v1","notices":[{"notice_id":"../x"}]}`)
	corrupt, _ := json.Marshal(top)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("a corrupt push journal must not keep the store from opening: %v", err)
	}
	if !errors.Is(reopened.PushJournalDiscarded(), errPushJournalInvalid) {
		t.Fatalf("discard not reported: %v", reopened.PushJournalDiscarded())
	}
	if proof := reopened.PushDeliveryProof(base); proof.Witnessed || proof.LastSent != nil {
		t.Fatalf("discarded journal still produced evidence: %+v", proof)
	}
	if len(reopened.Devices()) != 2 {
		t.Fatal("discarding the journal touched device grants")
	}
}

// Before the journal existed, alert pushes lived only in the delivery ledger.
// The proof names that history so the silence has a date on day one.
func TestPushDeliveryProofSeedsSilenceFromTheAlertLedger(t *testing.T) {
	store, _ := pushJournalTestStore(t)
	acceptedAt := time.Date(2026, 8, 11, 13, 42, 47, 0, time.UTC)
	rejectedSince := time.Date(2026, 8, 15, 1, 53, 52, 0, time.UTC)
	store.mu.Lock()
	store.data.AlertDelivery = newAlertDeliveryData()
	store.data.AlertDelivery.Receipts = []alertDeliveryReceipt{{AuthorityScope: defaultTestAlertAuthorityScope, ReceiptKey: "r", AcceptedAt: acceptedAt}}
	store.data.AlertDelivery.Attempts = []alertDeliveryAttempt{{
		AuthorityScope: defaultTestAlertAuthorityScope, OccurrenceKey: "o", Class: AlertDeliveryAttemptAccepted,
		ReservedAt: acceptedAt.Add(-time.Second), CompletedAt: acceptedAt,
	}}
	store.data.AlertDelivery.Health = AlertDeliveryHealth{State: AlertDeliveryHealthDegraded, Class: AlertDeliveryHealthClassObservation, UpdatedAt: rejectedSince, LastAcceptedAt: acceptedAt}
	store.data.AlertDelivery.ObservationRejectedAt = rejectedSince
	store.mu.Unlock()

	proof := store.PushDeliveryProof(time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC))
	if err := rpc.ValidatePushDeliveryProof(proof); err != nil {
		t.Fatalf("proof does not validate: %v", err)
	}
	if proof.SilentSince == nil || !proof.SilentSince.Equal(acceptedAt) || proof.LastAlertSentAt == nil || !proof.LastAlertSentAt.Equal(acceptedAt) {
		t.Fatalf("silence not seeded from the ledger: %+v", proof)
	}
	if proof.LastSent == nil || proof.LastSent.Kind != rpc.PushNoticeKindAlert || !proof.LastSent.Accepted {
		t.Fatalf("last alert send = %+v", proof.LastSent)
	}
	if proof.IntakeRejectedSince == nil || !proof.IntakeRejectedSince.Equal(rejectedSince) || proof.DispatcherClass != AlertDeliveryHealthClassObservation {
		t.Fatalf("intake refusal not named: %+v", proof)
	}
	if proof.Witnessed || proof.ActiveSubscriptions != 1 || proof.Mode != AlertModeWatchAndAct {
		t.Fatalf("proof = %+v", proof)
	}
}
