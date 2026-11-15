package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestDeactivatedOrdersStayVisibleWithoutProtectionOrWriteEligibility(t *testing.T) {
	now := time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)
	broker := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 101, PermID: 7001,
		OrderRef: "synthetic-deactivated", Symbol: "SYN", SecType: "STK", Action: "SELL", OrderType: "TRAIL", TIF: "GTC",
		TotalQuantity: 10, Remaining: 10, Status: "Inactive", Deactivated: new(true)}
	ack, ok := orderJournalEventFromLifecycle(broker, now)
	if !ok {
		t.Fatal("missing journal event")
	}
	events := []orderJournalEvent{ack}
	assertPaused := func() {
		t.Helper()
		views := buildOrderViews(events)
		if len(views) != 1 {
			t.Fatalf("%+v", views)
		}
		v := views[0]
		if !v.Open || v.LifecycleStatus != rpc.OrderLifecycleDeactivated || v.ModifyEligible || v.CancelEligible {
			t.Fatalf("unsafe deactivated projection: %+v", v)
		}
		if protectionCoverageOrderCounts(v, rpc.PositionView{Quantity: 10}) {
			t.Fatal("paused stop counted as protection")
		}
	}
	assertPaused()
	status, _ := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventStatus, OrderID: 101, PermID: 7001, Status: "Inactive", Remaining: 10}, now.Add(time.Second))
	status.OrderRef = broker.OrderRef
	events = append(events, status)
	assertPaused()
	missingFlag := ack
	missingFlag.Deactivated = nil
	events = append(events, missingFlag)
	assertPaused()
	stale := buildOrderViews(events)[0]
	stale.LifecycleStatus = rpc.OrderLifecycleSubmitted
	if protectionCoverageOrderCounts(stale, rpc.PositionView{Quantity: 10}) {
		t.Fatal("explicit deactivation lost to an older working status")
	}
	projected := protectionOrderViewFromSnapshot(stale, ibkrlib.OrderLifecycleEvent{Deactivated: new(true)}, now)
	if projected.LifecycleStatus != rpc.OrderLifecycleDeactivated {
		t.Fatal("snapshot flag without status did not deactivate protection")
	}
	if !brokerOrderWorking(broker) {
		t.Fatal("retained order no longer blocks competing orders")
	}
	// A later explicit active openOrder resumes the same identity.
	broker.Deactivated = new(false)
	broker.Status = "Submitted"
	resumed, _ := orderJournalEventFromLifecycle(broker, now.Add(2*time.Second))
	events = append(events, resumed)
	v := buildOrderViews(events)[0]
	if !v.Open || v.LifecycleStatus != rpc.OrderLifecycleSubmitted || !v.ModifyEligible {
		t.Fatalf("failed to resume: %+v", v)
	}
	for _, terminal := range []string{"Filled", "Cancelled", "Rejected"} {
		t.Run(terminal, func(t *testing.T) {
			end := status
			end.Status = terminal
			seq := append(append([]orderJournalEvent{}, events...), end, status, ack)
			v := buildOrderViews(seq)[0]
			if v.Open || v.LifecycleStatus == rpc.OrderLifecycleDeactivated {
				t.Fatalf("terminal order resurrected: %+v", v)
			}
		})
	}
	brokerError := status
	brokerError.Type = orderJournalEventBrokerError
	brokerError.ErrorCode = 135
	closed := buildOrderViews([]orderJournalEvent{ack, brokerError, status, ack})[0]
	if closed.Open {
		t.Fatal("retention resurrected definitive broker error through a late Inactive callback")
	}
	// An older Inactive-only callback may be corrected by explicit retention,
	// but it stays terminal in the absence of that new evidence.
	if buildOrderViews([]orderJournalEvent{status})[0].Open {
		t.Fatal("changed ordinary Inactive semantics")
	}
	events = []orderJournalEvent{status, ack}
	assertPaused()
}
