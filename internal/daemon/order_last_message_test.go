package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A completed send's "waiting for broker lifecycle evidence" note ends with
// the broker's first lifecycle event; a broker reason is kept.
func TestSendWaitingNoteEndsWithBrokerEvidence(t *testing.T) {
	sent := orderJournalEvent{Type: orderJournalEventSendCompleted, Message: orderSendAwaitingBrokerMessage}
	for _, tc := range []struct {
		events []orderJournalEvent
		want   string
	}{
		{[]orderJournalEvent{sent, {Type: orderJournalEventStatusUpdated, Status: "Cancelled"}}, ""},
		{[]orderJournalEvent{sent, {Type: orderJournalEventBrokerAcknowledged, Status: "Submitted"}}, ""},
		{[]orderJournalEvent{sent, {Type: orderJournalEventBrokerError, ErrorCode: 201, Message: "broker error 201: no"}, {Type: orderJournalEventStatusUpdated, Status: "Inactive"}}, "broker error 201: no"},
	} {
		var v rpc.OrderView
		for _, ev := range tc.events {
			mergeOrderJournalEventIntoView(&v, ev)
		}
		if v.LastMessage != tc.want {
			t.Fatalf("last message %q, want %q", v.LastMessage, tc.want)
		}
	}
}

// The sweep takes a row without a PermID only when the broker saw the order
// (a clean flush, or its error with a code) and the grace has passed; a
// transport-uncertain or outcome-less send is never resolved by absence.
func TestReconcileTakesOnlyUnacknowledgedRowsTheBrokerSaw(t *testing.T) {
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	old := now.Add(-orderReconcileGrace - time.Second)
	for _, tc := range []struct {
		view rpc.OrderView
		want bool
	}{
		{rpc.OrderView{LastEvent: orderJournalEventSendCompleted, UpdatedAt: old}, true},
		{rpc.OrderView{LastEvent: orderJournalEventBrokerError, LastErrorCode: 200, UpdatedAt: old}, true},
		{rpc.OrderView{LastEvent: orderJournalEventSendCompleted, UpdatedAt: now.Add(-time.Minute)}, false},
		{rpc.OrderView{LastEvent: orderJournalEventSendError, UpdatedAt: old}, false},
		{rpc.OrderView{LastEvent: orderJournalEventSendAttempted, UpdatedAt: old}, false},
		{rpc.OrderView{LastEvent: orderJournalEventReconciledUnknown, UpdatedAt: old}, false},
		{rpc.OrderView{LastEvent: orderJournalEventBrokerError, UpdatedAt: old}, false},
	} {
		if got := orderReconcileUnacknowledgedCandidate(tc.view, now); got != tc.want {
			t.Errorf("%s code %d: candidate %v, want %v", tc.view.LastEvent, tc.view.LastErrorCode, got, tc.want)
		}
	}
}
