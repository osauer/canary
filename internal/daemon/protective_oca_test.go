package daemon

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

var ocaTestScope = brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}

func ocaTestStopView(ref string, orderID int, qty float64, group string, updated time.Time) rpc.OrderView {
	v := rpc.OrderView{OrderRef: ref, PreviewTokenID: "tok-" + ref, ReservedOrderID: orderID, ClientID: 31, Account: "DU1234567",
		Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD", Action: rpc.OrderActionSell,
		OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Quantity: qty, Remaining: qty, Status: "Submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted, SendState: orderSendStateBrokerAcknowledged, UpdatedAt: updated, Open: true, OCAGroup: group}
	if group != "" {
		v.OCAType = ibkrlib.OcaTypeReduceWithBlock
	}
	return v
}

func ocaTestStopOrder(orderID int, remaining float64, group string, typ int) ibkrlib.OrderLifecycleEvent {
	return ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: orderID, ClientID: 31, ClientIDPresent: true,
		Account: "DU1234567", Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
		TotalQuantity: remaining, Remaining: remaining, Status: "Submitted", OcaGroup: group, OcaType: typ}
}

func ocaTestSale(qty int) rpc.OrderDraft {
	return rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: qty, OrderType: rpc.OrderTypeLMT, LimitPrice: 100, OrderRef: "ord-sale",
		Contract: rpc.ContractParams{ConID: 910001, Symbol: "SYW", SecType: "STK", Exchange: "SMART", Currency: "USD"}}
}

// The planner: what a sale beside Canary's own stops does, from the
// journal rows and the broker's echo.
func TestPlanProtectiveOCA(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 15, 40, 0, 0, time.UTC)
	snapshotAt := now.Add(-time.Second)
	linked := ocaTestStopView("ord-prot", 1002, 70, "canary-oca-ord-prot", now.Add(-time.Hour))
	legacy := ocaTestStopView("ord-prot", 1002, 70, "", now.Add(-time.Hour))
	second := ocaTestStopView("ord-second", 1004, 20, "", now.Add(-time.Hour))
	stopDraft := ocaTestSale(70)
	stopDraft.OrderType, stopDraft.OrderRef = rpc.OrderTypeTRAIL, "ord-new-stop"
	cases := []struct {
		name      string
		draft     rpc.OrderDraft
		held      float64
		views     []rpc.OrderView
		snapshot  *ibkrlib.OpenOrderSnapshot
		wantGroup string
		wantSteps string
	}{
		{name: "no stop: a sale goes out plain", draft: ocaTestSale(70), held: 70,
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt}},
		{name: "no stop: a stop is born in its own group", draft: stopDraft, held: 70,
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt}, wantGroup: "canary-oca-ord-new-stop"},
		{name: "an echoed group is joined", draft: ocaTestSale(70), held: 70, views: []rpc.OrderView{linked},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "canary-oca-ord-prot", 2)}},
			wantGroup: "canary-oca-ord-prot"},
		{name: "a sale that fits beside the stop is plain", draft: ocaTestSale(30), held: 100, views: []rpc.OrderView{linked},
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "canary-oca-ord-prot", 2)}}},
		{name: "a stop without a group is cancelled before a full close", draft: ocaTestSale(70), held: 70, views: []rpc.OrderView{legacy},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "", 0)}},
			wantSteps: "cancel ord-prot"},
		{name: "a stop without a group is shrunk before a reduction", draft: ocaTestSale(30), held: 70, views: []rpc.OrderView{legacy},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "", 0)}},
			wantSteps: "shrink ord-prot to 40"},
		{name: "the broker's remaining counts, not the journal's", draft: ocaTestSale(30), held: 70, views: []rpc.OrderView{legacy},
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 40, "", 0)}}},
		{name: "a group of another type is not joined", draft: ocaTestSale(70), held: 70, views: []rpc.OrderView{linked},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "canary-oca-ord-prot", 1)}},
			wantSteps: "cancel ord-prot"},
		{name: "no inventory: the journal's own stop is shrunk, never joined", draft: ocaTestSale(30), held: 70, views: []rpc.OrderView{linked},
			wantSteps: "shrink ord-prot to 40"},
		{name: "a stop the complete inventory no longer shows is gone", draft: ocaTestSale(70), held: 70, views: []rpc.OrderView{legacy},
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt}},
		{name: "a stop sent inside the cache window counts from the journal", draft: ocaTestSale(70), held: 70,
			views:     []rpc.OrderView{ocaTestStopView("ord-prot", 1002, 70, "canary-oca-ord-prot", now)},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt},
			wantSteps: "cancel ord-prot"},
		{name: "two stops are cut in order until the sale fits", draft: ocaTestSale(50), held: 90, views: []rpc.OrderView{legacy, second},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "", 0), ocaTestStopOrder(1004, 20, "", 0)}},
			wantSteps: "shrink ord-prot to 20"},
		{name: "a new stop beside a legacy stop shrinks it and gets its own group", draft: stopDraft, held: 70, views: []rpc.OrderView{legacy},
			snapshot:  &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "", 0)}},
			wantGroup: "canary-oca-ord-new-stop", wantSteps: "cancel ord-prot"},
		{name: "a buy is never linked", draft: func() rpc.OrderDraft { d := ocaTestSale(70); d.Action = rpc.OrderActionBuy; return d }(), held: 70, views: []rpc.OrderView{linked},
			snapshot: &ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: snapshotAt, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "canary-oca-ord-prot", 2)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stops := canaryWorkingSellStops(tc.views, tc.snapshot, ocaTestScope, tc.draft)
			plan := planProtectiveOCA(tc.draft, tc.held, stops)
			var steps []string
			for _, step := range plan.Steps {
				if step.Kind == protectiveStopGuardCancel {
					steps = append(steps, "cancel "+step.View.OrderRef)
				} else {
					steps = append(steps, "shrink "+step.View.OrderRef+" to "+strconv.Itoa(step.Target))
				}
			}
			if plan.Group != tc.wantGroup || (plan.Group != "" && plan.Type != ibkrlib.OcaTypeReduceWithBlock) || strings.Join(steps, ", ") != tc.wantSteps {
				t.Fatalf("plan = group %q type %d steps %v, want group %q with type 2 and steps %q", plan.Group, plan.Type, steps, tc.wantGroup, tc.wantSteps)
			}
		})
	}
}

// A one-cancels-all membership is journaled with the send and survives the
// replay of the journal: later status and execution callbacks, and a legacy
// openOrder that does not report it, never clear it; a broker openOrder
// that reports it carries it into the journal.
func TestOCAMembershipSurvivesJournalReplay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 12, 15, 40, 0, 0, time.UTC)
	base := orderJournalEvent{OrderRef: "ord-close", PreviewTokenID: "tok-close", ReservedOrderID: 1003, ClientID: 31,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: "SYW", SecType: "STK", ConID: 910001,
		Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 70, LimitPrice: 100}
	sent := base
	sent.At, sent.Type, sent.SendState = now, orderJournalEventSendAttempted, orderSendStateSendAttempted
	sent.OCAGroup, sent.OCAType = "canary-oca-ord-prot", ibkrlib.OcaTypeReduceWithBlock
	legacyAck := base
	legacyAck.At, legacyAck.Type, legacyAck.SendState, legacyAck.Status = now.Add(time.Second), orderJournalEventBrokerAcknowledged, orderSendStateBrokerAcknowledged, "Submitted"
	status := base
	status.At, status.Type, status.Status, status.Remaining = now.Add(2*time.Second), orderJournalEventStatusUpdated, "Submitted", 70
	views := buildOrderViews([]orderJournalEvent{sent, legacyAck, status})
	if len(views) != 1 || views[0].OCAGroup != "canary-oca-ord-prot" || views[0].OCAType != ibkrlib.OcaTypeReduceWithBlock || !views[0].Open {
		t.Fatalf("views = %+v, want one open row still in canary-oca-ord-prot with type 2", views)
	}

	echo, ok := orderJournalEventFromLifecycle(ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1003, ClientID: 31,
		ClientIDPresent: true, Symbol: "SYW", SecType: "STK", ConID: 910001, Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT,
		TotalQuantity: 70, LimitPrice: 100, TIF: rpc.OrderTIFDay, Status: "Submitted", OcaGroup: "canary-oca-ord-prot", OcaType: 2}, now)
	if !ok || echo.OCAGroup != "canary-oca-ord-prot" || echo.OCAType != 2 {
		t.Fatalf("journal event from the broker's openOrder = %+v (ok %v), want the echoed membership", echo, ok)
	}
	if view := untrackedOrderView(ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 9, Symbol: "SYW", SecType: "STK",
		ConID: 910001, Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TotalQuantity: 70, Remaining: 70, Status: "Submitted",
		OcaGroup: "owner-group", OcaType: 1}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, now); view.OCAGroup != "owner-group" || view.OCAType != 1 {
		t.Fatalf("untracked view = %+v, want the hand order's group reported", view)
	}
}

// A sale that joins Canary's own stop's group does not count that stop
// against the held line for the cap exemptions (owner decision 2026-10-22
// 15:26 CEST): the broker reduces the stop by the sale's fill. An
// ungrouped stop, a second stop and a hand sale still count.
func TestProtectiveExitInventoryLeavesOutTheLinkedStop(t *testing.T) {
	t.Parallel()
	now := time.Now()
	group := "canary-oca-ord-prot"
	snap := func(orders ...ibkrlib.OrderLifecycleEvent) ibkrlib.OpenOrderSnapshot {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: now, Orders: orders}
	}
	sale := ocaTestSale(35)
	position := rpc.OrderPositionImpact{Before: 70, After: 35, Effect: classifyPositionEffect(70, 35)}
	journal := journalInventory{views: []rpc.OrderView{ocaTestStopView("ord-prot", 1002, 70, group, now.Add(-time.Hour))}}
	stop := ocaTestStopOrder(1002, 70, group, ibkrlib.OcaTypeReduceWithBlock)

	inv := protectiveExitInventoryFromSnapshot(snap(stop), journal, ocaTestScope, sale, orderPreviewReplaceTarget{})
	if !inv.Current || inv.OtherWorkingSameSide != 0 || inv.LinkedStop != "ord-prot" || inv.otherWorkingSameSide(910001)+35 > position.Before {
		t.Fatalf("beside a grouped full stop: %+v, want the stop left out, so the sale stays within the held line", inv)
	}
	hand := ocaTestStopOrder(0, 30, "", 0)
	hand.ClientID, hand.OrderType, hand.PermID = 0, rpc.OrderTypeLMT, 8001
	if inv := protectiveExitInventoryFromSnapshot(snap(stop, hand), journal, ocaTestScope, sale, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 30 {
		t.Fatalf("with a hand sale of 30 beside it: %+v, want the hand sale's 30 competing", inv)
	}
	plain := journalInventory{views: []rpc.OrderView{ocaTestStopView("ord-prot", 1002, 70, "", now.Add(-time.Hour))}}
	if inv := protectiveExitInventoryFromSnapshot(snap(ocaTestStopOrder(1002, 70, "", 0)), plain, ocaTestScope, sale, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 70 || inv.LinkedStop != "" {
		t.Fatalf("beside an ungrouped stop: %+v, want the stop counted", inv)
	}
	two := journalInventory{views: []rpc.OrderView{journal.views[0], ocaTestStopView("ord-second", 1004, 20, "canary-oca-ord-second", now.Add(-time.Hour))}}
	second := ocaTestStopOrder(1004, 20, "canary-oca-ord-second", ibkrlib.OcaTypeReduceWithBlock)
	if inv := protectiveExitInventoryFromSnapshot(snap(stop, second), two, ocaTestScope, sale, orderPreviewReplaceTarget{}); inv.OtherWorkingSameSide != 90 || inv.LinkedStop != "" {
		t.Fatalf("beside two stops: %+v, want both counted; the sale joins no group", inv)
	}
}
