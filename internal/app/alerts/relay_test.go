package alerts

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

type fakeProofReporter struct {
	mu     sync.Mutex
	proofs []rpc.PushDeliveryProof
	err    error
	calls  chan struct{}
}

func (f *fakeProofReporter) ReportPushDeliveryProof(_ context.Context, proof rpc.PushDeliveryProof) (*rpc.AlertDeliveryProofResult, error) {
	f.mu.Lock()
	f.proofs = append(f.proofs, proof)
	err := f.err
	f.mu.Unlock()
	f.calls <- struct{}{}
	if err != nil {
		return nil, err
	}
	return &rpc.AlertDeliveryProofResult{Accepted: true, ReceivedAt: proof.ReportedAt}, nil
}

func TestProofRelayReportsAtStartAndAfterEveryJournalChange(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	sender := &recordingSender{at: now}
	d, store := pushDeliveryTestDispatcher(t, sender, now)
	reporter := &fakeProofReporter{calls: make(chan struct{}, 8), err: errors.New("daemon unavailable")}
	relay := NewProofRelay(store, reporter)
	relay.Every = time.Hour
	relay.Now = func() time.Time { return now }
	d.OnJournal = relay.Notify
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		relay.Run(ctx)
		close(done)
	}()
	waitCall := func() {
		t.Helper()
		select {
		case <-reporter.calls:
		case <-time.After(5 * time.Second):
			t.Fatal("relay did not report")
		}
	}
	waitCall() // start-up report, refused by the daemon: logged, not fatal
	reporter.mu.Lock()
	reporter.err = nil
	reporter.mu.Unlock()
	if _, err := d.Observe(t.Context(), pushDeliveryTestSnapshot(t, now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Observe(t.Context(), pushDeliveryTestSnapshot(t, now, pushDeliveryTestCandidate(t, now))); err != nil {
		t.Fatal(err)
	}
	waitCall()
	cancel()
	<-done
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	last := reporter.proofs[len(reporter.proofs)-1]
	if err := rpc.ValidatePushDeliveryProof(last); err != nil || last.LastAlertSentAt == nil {
		t.Fatalf("relayed proof = %+v, %v", last, err)
	}
}

func TestRecordPushAckStoresTheReceiptAndNotifiesTheRelay(t *testing.T) {
	now := time.Date(2026, 9, 26, 7, 0, 0, 0, time.UTC)
	d, store := pushDeliveryTestDispatcher(t, &recordingSender{at: now}, now)
	notified := 0
	d.OnJournal = func() { notified++ }
	sub := store.ActivePushSubscriptionsForDevice("phone-grant")[0]
	const notice = "alert-0123456789abcdef"
	if err := store.RecordPushAttempt(rpc.PushNoticeKindAlert, notice, sub, state.PushAttempt{At: now, OK: true, StatusCode: 201, Class: state.GovernanceTransportAccepted}); err != nil {
		t.Fatal(err)
	}
	outcome, err := d.RecordPushAck(notice, "phone-grant", rpc.PushAckDisplayed, time.Time{})
	if err != nil || !outcome.Recorded || outcome.Kind != rpc.PushNoticeKindAlert || notified != 1 {
		t.Fatalf("receipt = %+v, %v, notified %d", outcome, err, notified)
	}
	if again, err := d.RecordPushAck(notice, "phone-grant", rpc.PushAckDisplayed, time.Time{}); err != nil || again.Recorded || notified != 1 {
		t.Fatalf("duplicate receipt = %+v, %v, notified %d; a duplicate must not report again", again, err, notified)
	}
	if proof := store.PushDeliveryProof(now); !proof.Witnessed || proof.LastDisplayed == nil || proof.LastDisplayed.Device != "iPhone" {
		t.Fatalf("proof after receipt = %+v", proof)
	}
}
