package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestMandateStopsRequireEffectiveProtection(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       string
		deactivated  bool
		child        bool
		parentFilled bool
		laterChild   bool
		want         bool
	}{
		{name: "standalone submitted", status: "Submitted", want: true},
		{name: "standalone presubmitted", status: "PreSubmitted", want: true},
		{name: "deactivated", status: "Submitted", deactivated: true},
		{name: "pending submission", status: "PendingSubmit"},
		{name: "held child", status: "PreSubmitted", child: true},
		{name: "activation pending child", status: "Submitted", child: true, parentFilled: true},
		{name: "active child", status: "Submitted", child: true, parentFilled: true, laterChild: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := brokerStateScope{Account: "synthetic", Mode: rpc.AccountModePaper}
			row := mandateReductionStock("SYNA", 9001, 100, 100)
			pos := mandateReductionBook(row)
			order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 101, ClientID: 41, ClientIDPresent: true, Account: scope.Account, ConID: row.ConID, Symbol: row.Symbol, SecType: "STK", Currency: "EUR", Action: "SELL", OrderType: "STP", AuxPrice: 90, TotalQuantity: 100, Remaining: 100, Status: tc.status, Deactivated: new(tc.deactivated)}
			s := &Server{orderJournal: newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders"))}
			if tc.child {
				order.ParentID = new(100)
				parent := orderJournalEvent{At: mandateTestNow, Type: orderJournalEventBrokerAcknowledged, OrderRef: "entry", PreviewTokenID: "pair", ReservedOrderID: 100, ClientID: 41, Account: scope.Account, Endpoint: "localhost:7497", Mode: scope.Mode, ConID: row.ConID, Symbol: row.Symbol, SecType: "STK", Currency: "EUR", Action: "BUY", OrderType: "LMT", Quantity: 100, Remaining: 100, Status: "Submitted", AttachedStop: &rpc.AttachedStockStop{Price: 90}}
				child := parent
				child.OrderRef, child.ReservedOrderID, child.Action, child.OrderType, child.Status = "child", 101, "SELL", "STP", "PreSubmitted"
				child.AttachedStop = nil
				child.EntryLink = &rpc.StockEntryLink{PairRef: "entry", ParentRef: "entry", ParentID: 100, EntryQuantity: 100}
				child.BrokerParentID, child.FixedStopPrice, child.TIF, child.TriggerMethod = new(100), 90, "GTC", 2
				events := []orderJournalEvent{parent, child}
				if tc.parentFilled {
					parent.Filled, parent.Remaining, parent.Status = 100, 0, "Filled"
					events = append(events, parent)
				}
				if tc.laterChild {
					child.Status = "Submitted"
					events = append(events, child)
				}
				for _, ev := range events {
					if err := s.orderJournal.Append(ev); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: mandateTestNow, Orders: []ibkrlib.OrderLifecycleEvent{order}}, scope, nil
			}
			stops, why := s.mandateArmedStops(t.Context(), pos, scope)
			if why != "" {
				t.Fatal(why)
			}
			if got := len(stops[mandateLegID(row)]) > 0; got != tc.want {
				t.Fatalf("armed=%t want=%t; stops=%+v", got, tc.want, stops)
			}
		})
	}
}
