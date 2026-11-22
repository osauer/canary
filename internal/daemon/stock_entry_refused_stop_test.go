package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A refused attached stop withdraws only an entry the broker never
// acknowledged, and only once; an acknowledged or filled entry is never
// cancelled. Each record says what the refusal means for the entry, and
// "withdrawn" only when the journal holds Canary's cancel for it.
func TestRefusedStopWithdrawsOnlyAnUnacknowledgedEntryOnce(t *testing.T) {
	pair := func() []rpc.OrderView {
		parent := rpc.OrderView{OrderRef: "e1", ReservedOrderID: 10, ClientID: 15, Account: "U1", Endpoint: "x", Mode: "live", Action: "BUY", SecType: "STK",
			AttachedStop: &rpc.AttachedStockStop{Price: 47}, Quantity: 10, Open: true, LifecycleStatus: rpc.OrderLifecyclePendingSubmit, SendState: orderSendStateSendAttempted, LastMessage: "parent note"}
		child := rpc.OrderView{OrderRef: "e1-stop", ReservedOrderID: 11, ClientID: 15, Account: "U1", Endpoint: "x", Mode: "live", Action: "SELL", OrderType: "STP",
			EntryLink: &rpc.StockEntryLink{PairRef: "e1", ParentRef: "e1", ParentID: 10, EntryQuantity: 10}, Quantity: 10,
			LifecycleStatus: rpc.OrderLifecycleRejected, LastMessage: "broker error 201: no"}
		return []rpc.OrderView{parent, child}
	}
	cancel := []orderJournalEvent{{Type: orderJournalEventCancelRequested, OrderRef: "e1", ReservedOrderID: 10, ClientID: 15, Account: "U1", Endpoint: "x", Mode: "live"}}
	for _, tc := range []struct {
		name     string
		edit     func(p *rpc.OrderView)
		events   []orderJournalEvent
		withdraw bool
		says     string // "" keeps each record's own message
	}{
		{"never acknowledged", func(*rpc.OrderView) {}, nil, true, "the entry is being withdrawn"},
		{"cancel answered with an error", func(p *rpc.OrderView) {
			p.LifecycleStatus, p.LastErrorCode = rpc.OrderLifecycleUnknownReconcileRequired, 10147
		}, cancel, false, "the entry's withdrawal was sent to the broker"},
		{"acknowledged and working", func(p *rpc.OrderView) {
			p.PermID, p.SendState, p.LifecycleStatus = 7, orderSendStateBrokerAcknowledged, rpc.OrderLifecycleSubmitted
		}, nil, false, "the entry is working without it"},
		{"filled", func(p *rpc.OrderView) {
			p.PermID, p.Filled, p.Open, p.LifecycleStatus = 7, 10, false, rpc.OrderLifecycleFilled
		}, nil, false, "the entry filled 10 shares that this stop does not protect"},
		{"withdrawn", func(p *rpc.OrderView) { p.Open, p.LifecycleStatus = false, rpc.OrderLifecycleClosedReconciled }, cancel, false, "the entry was withdrawn"},
		{"closed without a cancel", func(p *rpc.OrderView) { p.Open, p.LifecycleStatus = false, rpc.OrderLifecycleClosedReconciled }, nil, false, ""},
		{"entry rejected by the broker", func(p *rpc.OrderView) { p.Open, p.LifecycleStatus = false, rpc.OrderLifecycleRejected }, cancel, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := pair()
			tc.edit(&views[0])
			if got := stockEntryAwaitingWithdrawal(views, views[0], len(tc.events) > 0); got != tc.withdraw {
				t.Fatalf("withdraw = %v, want %v", got, tc.withdraw)
			}
			deriveRefusedStopPairs(views, tc.events)
			if tc.says == "" {
				if views[0].LastMessage != "parent note" || views[1].LastMessage != "broker error 201: no" {
					t.Fatalf("messages rewritten: %q / %q", views[0].LastMessage, views[1].LastMessage)
				}
				return
			}
			for _, v := range views {
				if !strings.Contains(v.LastMessage, "(broker error 201: no)") || !strings.Contains(v.LastMessage, tc.says) {
					t.Fatalf("%s says %q", v.OrderRef, v.LastMessage)
				}
			}
			if views[0].CancelEligible != tc.withdraw {
				t.Fatalf("cancel eligible %v", views[0].CancelEligible)
			}
		})
	}
	views := pair()
	views[1].LifecycleStatus, views[1].Open = rpc.OrderLifecyclePreSubmitted, true
	if stockEntryAwaitingWithdrawal(views, views[0], false) {
		t.Fatal("a held stop withdrew its entry")
	}
}
