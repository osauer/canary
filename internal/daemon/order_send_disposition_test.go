package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func sendDispositionTestEvent(at time.Time, eventType, attemptID string, action corestore.ActionKind) orderJournalEvent {
	return orderJournalEvent{
		At: at, Type: eventType, AttemptID: attemptID, ActionKind: action,
		OrderRef: "ord-disposition", ReservedOrderID: 1001, ClientID: 31,
		Account: "DU123", Endpoint: "127.0.0.1:4002", Mode: "paper",
		Symbol: "AAPL", SecType: "STK", ConID: 123, Action: "BUY",
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 1,
	}
}

func sendDispositionBrokerAck(at time.Time) orderJournalEvent {
	ev := sendDispositionTestEvent(at, orderJournalEventBrokerAcknowledged, "", "")
	ev.Status = "Submitted"
	ev.SendState = orderSendStateBrokerAcknowledged
	return ev
}

func sendDispositionError(at time.Time, attemptID string, action corestore.ActionKind, disposition ibkrlib.SendDisposition) orderJournalEvent {
	ev := sendDispositionTestEvent(at, orderJournalEventSendError, attemptID, action)
	ev.SendDisposition = disposition
	if disposition != ibkrlib.SendDispositionDefinitelyUnsent {
		ev.SendState = orderSendStateUncertainSend
	}
	return ev
}

func TestOrderAttemptDispositionReducer(t *testing.T) {
	base := time.Date(2026, 7, 22, 9, 0, 0, 0, time.UTC)

	t.Run("definite place is terminal", func(t *testing.T) {
		attempt := sendDispositionTestEvent(base, orderJournalEventSendAttempted, "place-a", corestore.ActionPlace)
		attempt.SendState = orderSendStateSendAttempted
		view := buildOrderViews([]orderJournalEvent{attempt, sendDispositionError(base.Add(time.Second), "place-a", corestore.ActionPlace, ibkrlib.SendDispositionDefinitelyUnsent)})[0]
		if view.Open || view.LifecycleStatus != rpc.OrderLifecycleRejected || view.SendState != orderSendStateTerminal || view.ModifyEligible || view.CancelEligible {
			t.Fatalf("definitely-unsent place view = %+v", view)
		}
	})

	for _, action := range []corestore.ActionKind{corestore.ActionModify, corestore.ActionCancel} {
		t.Run("definite "+string(action)+" preserves working order", func(t *testing.T) {
			startType := orderJournalEventModifyRequested
			if action == corestore.ActionCancel {
				startType = orderJournalEventCancelRequested
			}
			attempt := sendDispositionTestEvent(base.Add(time.Second), startType, "definite-"+string(action), action)
			view := buildOrderViews([]orderJournalEvent{
				sendDispositionBrokerAck(base), attempt,
				sendDispositionError(base.Add(2*time.Second), attempt.AttemptID, action, ibkrlib.SendDispositionDefinitelyUnsent),
			})[0]
			if !view.Open || view.SendState != orderSendStateBrokerAcknowledged || view.LifecycleStatus != rpc.OrderLifecycleSubmitted || !view.ModifyEligible || !view.CancelEligible {
				t.Fatalf("definitely-unsent %s view = %+v", action, view)
			}
		})
	}

	t.Run("older uncertain cancel survives later definite refusal", func(t *testing.T) {
		first := sendDispositionTestEvent(base.Add(time.Second), orderJournalEventCancelRequested, "cancel-a", corestore.ActionCancel)
		second := sendDispositionTestEvent(base.Add(3*time.Second), orderJournalEventCancelRequested, "cancel-b", corestore.ActionCancel)
		view := buildOrderViews([]orderJournalEvent{
			sendDispositionBrokerAck(base), first,
			sendDispositionError(base.Add(2*time.Second), first.AttemptID, first.ActionKind, ibkrlib.SendDispositionMayHaveWritten),
			second,
			sendDispositionError(base.Add(4*time.Second), second.AttemptID, second.ActionKind, ibkrlib.SendDispositionDefinitelyUnsent),
		})[0]
		if !view.Open || view.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired || view.SendState != orderSendStateUncertainSend || !view.CancelEligible || view.ModifyEligible {
			t.Fatalf("mixed cancel attempts view = %+v", view)
		}
	})
}

func TestOrderAttemptBrokerEvidenceDominatesLateLocalOutcome(t *testing.T) {
	base := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	attempt := sendDispositionTestEvent(base, orderJournalEventSendAttempted, "race-a", corestore.ActionPlace)
	attempt.SendState = orderSendStateSendAttempted

	t.Run("ack beats late uncertainty and Submitted status alone does not", func(t *testing.T) {
		status := sendDispositionTestEvent(base.Add(time.Second), orderJournalEventStatusUpdated, "", "")
		status.Status = "Submitted"
		status.SendState = orderSendStateBrokerAcknowledged
		late := sendDispositionError(base.Add(2*time.Second), attempt.AttemptID, attempt.ActionKind, ibkrlib.SendDispositionUnknown)
		noiseView := buildOrderViews([]orderJournalEvent{attempt, status, late})[0]
		if noiseView.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired {
			t.Fatalf("Submitted status resolved attempt: %+v", noiseView)
		}
		ackView := buildOrderViews([]orderJournalEvent{attempt, sendDispositionBrokerAck(base.Add(time.Second)), late})[0]
		if ackView.LifecycleStatus != rpc.OrderLifecycleSubmitted || ackView.SendState != orderSendStateBrokerAcknowledged || ackView.LastEvent != orderJournalEventBrokerAcknowledged {
			t.Fatalf("ack did not dominate late error: %+v", ackView)
		}
	})

	t.Run("terminal callback is sticky", func(t *testing.T) {
		terminal := sendDispositionTestEvent(base.Add(time.Second), orderJournalEventStatusUpdated, "", "")
		terminal.Status, terminal.Filled, terminal.Remaining, terminal.SendState = "Filled", 1, 0, orderSendStateTerminal
		view := buildOrderViews([]orderJournalEvent{attempt, terminal, sendDispositionError(base.Add(2*time.Second), attempt.AttemptID, attempt.ActionKind, ibkrlib.SendDispositionUnknown)})[0]
		if view.Open || view.LifecycleStatus != rpc.OrderLifecycleFilled || view.Status != "Filled" || view.LastEvent != orderJournalEventStatusUpdated {
			t.Fatalf("late local outcome resurrected terminal order: %+v", view)
		}
	})
}

func TestAmbiguousModifyRequiresMatchingBrokerAcknowledgement(t *testing.T) {
	base := time.Date(2026, 7, 22, 10, 30, 0, 0, time.UTC)
	working := sendDispositionBrokerAck(base)
	working.LimitPrice = 100
	modify := sendDispositionTestEvent(base.Add(time.Second), orderJournalEventModifyRequested, "modify-price", corestore.ActionModify)
	modify.LimitPrice = 99
	uncertain := sendDispositionError(base.Add(2*time.Second), modify.AttemptID, modify.ActionKind, ibkrlib.SendDispositionMayHaveWritten)

	t.Run("stale pre-modify callback does not resolve attempt", func(t *testing.T) {
		stale := sendDispositionBrokerAck(base.Add(3 * time.Second))
		stale.LimitPrice = working.LimitPrice
		view := buildOrderViews([]orderJournalEvent{working, modify, uncertain, stale})[0]
		if view.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired || view.SendState != orderSendStateUncertainSend || view.ModifyEligible || !view.CancelEligible {
			t.Fatalf("stale acknowledgement resolved ambiguous modify: %+v", view)
		}
	})

	t.Run("matching modified snapshot resolves exact attempt", func(t *testing.T) {
		matching := sendDispositionBrokerAck(base.Add(3 * time.Second))
		matching.LimitPrice = modify.LimitPrice
		view := buildOrderViews([]orderJournalEvent{working, modify, uncertain, matching})[0]
		if view.LifecycleStatus != rpc.OrderLifecycleSubmitted || view.SendState != orderSendStateBrokerAcknowledged || view.LimitPrice != modify.LimitPrice || !view.ModifyEligible || !view.CancelEligible {
			t.Fatalf("matching acknowledgement did not resolve modify: %+v", view)
		}
	})

	t.Run("journal enrichment does not fabricate mutable ack fields", func(t *testing.T) {
		ack := sendDispositionBrokerAck(base.Add(3 * time.Second))
		ack.Action, ack.OrderType, ack.TIF, ack.Quantity, ack.LimitPrice = "", "", "", 0, 0
		copyOrderJournalIdentityFromView(&ack, rpc.OrderView{
			OrderRef: "ord-disposition", Action: "BUY", OrderType: rpc.OrderTypeLMT,
			TIF: rpc.OrderTIFDay, Quantity: 1, LimitPrice: modify.LimitPrice,
		})
		if ack.Action != "" || ack.OrderType != "" || ack.TIF != "" || ack.Quantity != 0 || ack.LimitPrice != 0 {
			t.Fatalf("broker acknowledgement mutable fields were projected from local view: %+v", ack)
		}
	})
}

func TestOrderAttemptRestartReconstructsUncertainCancelablePlace(t *testing.T) {
	authorityDir := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(authorityDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(authorityDir, "authority.db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	journal := newOrderJournalStore(filepath.Join(filepath.Dir(dbPath), "legacy.jsonl"))
	if err := journal.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	attempt := sendDispositionTestEvent(time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC), orderJournalEventSendAttempted, "restart-a", corestore.ActionPlace)
	attempt.SendState = orderSendStateSendAttempted
	if err := journal.StagePreTransmit("", "", 0, attempt.ReservedOrderID, corestore.ActionPlace, corestore.OriginAgentCLI, []orderJournalEvent{attempt}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := corestore.Open(context.Background(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := newOrderJournalStore(journal.Path)
	if err := restarted.UseCoreStore(reopened); err != nil {
		t.Fatal(err)
	}
	events, err := restarted.LoadEvents(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AttemptID != attempt.AttemptID || events[0].ActionKind != corestore.ActionPlace {
		t.Fatalf("restarted attempt provenance = %+v", events)
	}
	view := buildOrderViews(events)[0]
	if view.LifecycleStatus != rpc.OrderLifecycleUnknownReconcileRequired || view.SendState != orderSendStateUncertainSend || !view.Open || !view.CancelEligible || view.ModifyEligible {
		t.Fatalf("restart view = %+v", view)
	}
}
