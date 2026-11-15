//go:build trading

package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestSaleDoesNotCancelHeldAttachedChild(t *testing.T) {
	for _, filled := range []float64{0, 20} {
		t.Run(fmt.Sprintf("parent_filled_%g", filled), func(t *testing.T) {
			r := newCloseLinkRig(t, false, "")
			r.position += filled
			c := closeLinkContract(false)
			parent := orderJournalEvent{At: r.now, Type: orderJournalEventBrokerAcknowledged, OrderRef: "synthetic-entry", PreviewTokenID: "synthetic-pair", ReservedOrderID: 1001, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Exchange, Currency: c.Currency, Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 70, Remaining: 70, LimitPrice: 100, AttachedStop: &rpc.AttachedStockStop{Price: 90}, Status: "Submitted", SendState: orderSendStateBrokerAcknowledged}
			parent.Filled, parent.Remaining = filled, 70-filled
			if err := r.srv.orderJournal.Append(parent); err != nil {
				t.Fatal(err)
			}
			child := parent
			child.OrderRef = "ord-prot"
			child.ReservedOrderID = 1002
			child.AttachedStop = nil
			child.Filled, child.Remaining = 0, 70
			child.Action = "SELL"
			child.OrderType = "STP"
			child.TIF = "GTC"
			child.LimitPrice = 0
			child.FixedStopPrice = 90
			child.TriggerMethod = 2
			child.EntryLink = &rpc.StockEntryLink{PairRef: parent.OrderRef, ParentRef: parent.OrderRef, ParentID: 1001, EntryQuantity: 70}
			child.BrokerParentID = new(1001)
			child.Status = "PreSubmitted"
			if err := r.srv.orderJournal.Append(child); err != nil {
				t.Fatal(err)
			}
			original := r.srv.openOrderInventoryForTest
			r.srv.openOrderInventoryForTest = func(ctx context.Context, refresh bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
				snap, scope, err := original(ctx, refresh)
				for i := range snap.Orders {
					if snap.Orders[i].OrderID == 1002 {
						o := &snap.Orders[i]
						o.OrderType = "STP"
						o.Status = "PreSubmitted"
						o.ParentID = new(1001)
						o.AuxPrice = 90
						o.TIF = "GTC"
						o.TriggerMethod = 2
					}
				}
				return snap, scope, err
			}
			if got := r.stopView(t); got.EntryState != "held" || !got.CancelEligible {
				t.Fatalf("fixture invalid: entry_state=%s cancel_eligible=%v", got.EntryState, got.CancelEligible)
			}
			token := r.previewSell(t, int(r.position))
			result, err := r.place(t, token)
			t.Logf("synthetic sale accepted=%v error=%v sends=%v", result != nil && result.Accepted, err, r.sent())
			if err == nil || (result != nil && result.Accepted) || len(r.sent()) != 0 {
				t.Fatalf("sale overlapping pending protected entry must refuse before any broker write: result=%+v err=%v sends=%v", result, err, r.sent())
			}
			for _, id := range r.cancelled {
				if id == 1002 {
					t.Fatalf("independent sale cancelled the held attached stop while its parent entry remained working")
				}
			}
		})
	}
}
