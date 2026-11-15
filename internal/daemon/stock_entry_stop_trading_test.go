//go:build trading

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestAttachedStopGuardShrinksPartialFillWithoutChangingProtection(t *testing.T) {
	testAttachedStopGuardShrink(t, 100, 20, 50, 0)
}
func TestAttachedStopGuardShrinksBelowWholePositionForExistingProtection(t *testing.T) {
	testAttachedStopGuardShrink(t, 200, 0, 150, 100)
}
func testAttachedStopGuardShrink(t *testing.T, original, filled, held, existing float64) {
	r := newStopGuardRigIn(t, config.Trading{}, t.TempDir(), "127.0.0.1:4002", 4002, false)
	r.position = held
	parent := orderJournalEvent{At: r.now.Add(-time.Minute), Type: orderJournalEventBrokerAcknowledged, OrderRef: "entry-fixed", PreviewTokenID: "pair-fixed", ReservedOrderID: 1001, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: "SYW", SecType: "STK", ConID: 910001, Exchange: "SMART", Currency: "USD", Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: original, Filled: original, LimitPrice: 100, Status: "Filled", SendState: orderSendStateBrokerAcknowledged, AttachedStop: &rpc.AttachedStockStop{Price: 90}}
	child := parent
	child.OrderRef = "child-fixed"
	child.ReservedOrderID = 1002
	child.AttachedStop = nil
	child.EntryLink = &rpc.StockEntryLink{PairRef: parent.OrderRef, ParentRef: parent.OrderRef, ParentID: 1001, EntryQuantity: int(original)}
	child.BrokerParentID = new(1001)
	child.Action = "SELL"
	child.OrderType = "STP"
	child.TIF = "GTC"
	child.TriggerMethod = 2
	child.FixedStopPrice = 90
	child.LimitPrice = 0
	child.Filled = filled
	child.Remaining = original - filled
	child.Status = "Submitted"
	child.OpenClose = "C"
	events := []orderJournalEvent{parent, child}
	if existing > 0 {
		old := child
		old.OrderRef = "existing-stop"
		old.ReservedOrderID = 500
		old.EntryLink = nil
		old.BrokerParentID = nil
		old.OrderType = "TRAIL"
		old.Trail = &rpc.OrderTrailSpec{TrailingAmount: new(8.5), InitialStopPrice: 90}
		old.Quantity = existing
		old.Filled = 0
		old.Remaining = existing
		events = append(events, old)
	}
	for _, e := range events {
		if err := r.srv.orderJournal.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	r.srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		rows := []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 1002, ParentID: new(1001), ClientID: 31, ClientIDPresent: true, Account: "DU1234567", Symbol: "SYW", SecType: "STK", ConID: 910001, Exchange: "SMART", Currency: "USD", Action: "SELL", OrderType: "STP", TIF: "GTC", TriggerMethod: 2, AuxPrice: 90, TotalQuantity: original, Filled: filled, Remaining: original - filled, Status: "Submitted"}}
		if existing > 0 {
			old := rows[0]
			old.OrderID = 500
			old.ParentID = nil
			old.OrderType = "TRAIL"
			old.TotalQuantity = existing
			old.Remaining = existing
			old.Filled = 0
			old.TrailStopPrice = 90
			old.AuxPrice = 8.5
			rows = append(rows, old)
		}
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.now, Orders: rows}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}

	r.pass(t)
	if len(r.modified) != 1 {
		t.Fatalf("wanted one shrink: modified=%d decisions=%+v", len(r.modified), readDecisionLines(t, r.srv.decisions.path))
	}
	o := r.modified[0]
	if o.TotalQty != int(50+filled) || o.ParentID != 1001 || o.AuxPrice != 90 || o.OrderType != "STP" || o.TIF != "GTC" || o.TriggerMethod != 2 || o.OutsideRth {
		t.Fatalf("changed protection: %+v", o)
	}
	// Restart folding must retain original parent-fill proof and accept only an
	// exact acknowledgement of the new total (remaining50 + filled20).
	child.Quantity = 50 + filled
	child.Remaining = 50
	child.At = r.now
	if err := r.srv.orderJournal.Append(child); err != nil {
		t.Fatal(err)
	}
	views, _, err := r.srv.loadOrderViews()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		if v.OrderRef == child.OrderRef {
			if v.EntryState != "active" || !v.ModifyEligible || v.Quantity != 50+filled {
				t.Fatalf("shrunk child lost lineage: %+v", v)
			}
			return
		}
	}
	t.Fatal("missing child")
}
