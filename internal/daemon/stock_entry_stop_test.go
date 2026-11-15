package daemon

import (
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
	"testing"
	"time"
)

func TestProtectedAddScopeCannotFallBackToOrdinaryBuy(t *testing.T) {
	d := rpc.OrderDraft{Contract: rpc.ContractParams{ConID: 10001, Symbol: "SYNX", SecType: "STK"}, Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 10, LimitPrice: 80, Add: &rpc.AddReview{}, AttachedStop: &rpc.AttachedStockStop{Price: 76}}
	if err := validateProtectedAddScope(rpc.OrderTokenScopeProtectedAdd, d); err != nil {
		t.Fatal(err)
	}
	if validateProtectedAddScope(rpc.OrderTokenScopePlace, d) == nil {
		t.Fatal("ordinary token dropped child")
	}
	d.AttachedStop = nil
	if validateProtectedAddScope(rpc.OrderTokenScopeProtectedAdd, d) == nil {
		t.Fatal("protected scope without child")
	}
	d.AttachedStop = &rpc.AttachedStockStop{Price: 81}
	if validateProtectedAddScope(rpc.OrderTokenScopeProtectedAdd, d) == nil {
		t.Fatal("stop above entry")
	}
}

func TestStockEntryStopLifecycleRequiresFullFillAndLaterChildEvidence(t *testing.T) {
	now := time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)
	entry := orderJournalEvent{At: now, Type: orderJournalEventSendAttempted, OrderRef: "entry", PreviewTokenID: "pair", ReservedOrderID: 100, ClientID: 41, Account: "synthetic", Endpoint: "localhost:7497", Mode: "paper", ConID: 10001, Symbol: "SYNX", SecType: "STK", Currency: "USD", Action: "BUY", OrderType: "LMT", Quantity: 10, LimitPrice: 80, TIF: "DAY", AttachedStop: &rpc.AttachedStockStop{Price: 76}}
	child := entry
	child.OrderRef = "child"
	child.ReservedOrderID = 101
	child.AttachedStop = nil
	child.EntryLink = &rpc.StockEntryLink{PairRef: "entry", ParentRef: "entry", ParentID: 100, EntryQuantity: 10}
	child.Action = "SELL"
	child.OrderType = "STP"
	child.LimitPrice = 0
	child.FixedStopPrice = 76
	child.TIF = "GTC"
	child.TriggerMethod = 2
	child.OpenClose = "C"
	ack := func(e orderJournalEvent, status string, filled float64) orderJournalEvent {
		e.Type = orderJournalEventBrokerAcknowledged
		e.SendState = orderSendStateBrokerAcknowledged
		e.Status = status
		e.Filled = filled
		e.Remaining = 10 - filled
		return e
	}
	parentAck := ack(entry, "Submitted", 0)
	childAck := ack(child, "PreSubmitted", 0)
	childAck.BrokerParentID = new(100)
	events := []orderJournalEvent{entry, child, parentAck, childAck}
	check := func(want string) {
		t.Helper()
		views := buildOrderViews(events)
		for _, v := range views {
			if v.OrderRef == "child" {
				if v.EntryState != want {
					t.Fatalf("state=%s want=%s view=%+v", v.EntryState, want, v)
				}
				if v.ModifyEligible != (want == "active") {
					t.Fatal("generic modify detached child")
				}
				row := rpc.PositionView{Quantity: 10}
				if protectionCoverageOrderCounts(v, row) != (want == "active") {
					t.Fatalf("coverage for %s", want)
				}
				return
			}
		}
		t.Fatal("missing child")
	}
	check("held")
	events = append(events, ack(entry, "Submitted", 4))
	check("held")
	events = append(events, ack(entry, "Filled", 10))
	check("activation_pending")
	events = append(events, ack(childAck, "Submitted", 0))
	check("active")
	execution := ack(childAck, "Execution", 2)
	execution.Type = orderJournalEventStatusUpdated
	execution.BrokerParentID = nil
	events = append(events, execution)
	check("active")
	heldAgain := ack(childAck, "PreSubmitted", 0)
	heldAgain.WhyHeld = "broker hold"
	events = append(events, heldAgain)
	check("activation_pending")
	events = append(events, ack(childAck, "Submitted", 0))
	check("active")
	wrong := ack(childAck, "Submitted", 0)
	wrong.BrokerParentID = new(99)
	events = append(events, wrong)
	check("relationship_unknown")
}

func TestStockStopModifyAcknowledgementRequiresExactBrokerTerms(t *testing.T) {
	intent := orderJournalEvent{Type: orderJournalEventModifyRequested, Action: "SELL", OrderType: "STP", TIF: "GTC", TriggerMethod: 2, Quantity: 8, FixedStopPrice: 76, EntryLink: &rpc.StockEntryLink{ParentID: 100}}
	ack := intent
	ack.Type = orderJournalEventBrokerAcknowledged
	ack.BrokerParentID = new(100)
	if !orderModifyAcknowledgementMatches(intent, ack) {
		t.Fatal("exact ack rejected")
	}
	ack.FixedStopPrice = 75
	if orderModifyAcknowledgementMatches(intent, ack) {
		t.Fatal("wrong stop accepted")
	}
	ack.FixedStopPrice = 76
	ack.BrokerParentID = new(99)
	if orderModifyAcknowledgementMatches(intent, ack) {
		t.Fatal("wrong parent accepted")
	}
	ack.BrokerParentID = nil
	if orderModifyAcknowledgementMatches(intent, ack) {
		t.Fatal("missing parent accepted")
	}
}
func TestStockStopOpenOrderDoesNotInventStopPrice(t *testing.T) {
	e := orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, OrderType: "STP"}
	copyOrderJournalIdentityFromView(&e, rpc.OrderView{FixedStopPrice: 76})
	if e.FixedStopPrice != 0 {
		t.Fatal("local price substituted for broker evidence")
	}
}

func TestAttachedStopsShareOnePositionCapacity(t *testing.T) {
	scope := brokerStateScope{Account: "synthetic", Mode: "paper"}
	row := func(ref string, id int, linked bool) brokerWorkingOrder {
		v := rpc.OrderView{OrderRef: ref, ReservedOrderID: id, Account: scope.Account, Mode: scope.Mode, PreviewTokenID: ref, Open: true, ConID: 10001, SecType: "STK", Symbol: "SYNX", Currency: "USD", Action: "SELL", OrderType: "STP", Quantity: 100, Remaining: 100}
		if linked {
			v.EntryLink = &rpc.StockEntryLink{ParentID: id - 1, EntryQuantity: 100}
			v.EntryState = "active"
		}
		return brokerWorkingOrder{Journal: &v, Order: ibkrlib.OrderLifecycleEvent{OrderID: id, ConID: 10001, SecType: "STK", Symbol: "SYNX", Currency: "USD", Action: "SELL", OrderType: "STP", TotalQuantity: 100, Remaining: 100}}
	}
	old, added := row("old", 100, false), row("added", 200, true)
	pos := []*ibkrlib.RawPosition{{Position: 150, Contract: ibkrlib.Contract{ConID: 10001, SecType: "STK", Symbol: "SYNX", Currency: "USD"}}}
	for _, rows := range [][]brokerWorkingOrder{{old, added}, {added, old}} {
		steps := planProtectiveStopGuard(rows, pos, scope)
		if len(steps) != 1 || steps[0].View.OrderRef != "added" || steps[0].Target != 50 {
			t.Fatalf("want only new stop shrunk to50: %+v", steps)
		}
	}
}

func TestAttachedStopWhatIfAndSubmitUseCumulativeTotal(t *testing.T) {
	link := &rpc.StockEntryLink{ParentID: 100, EntryQuantity: 100}
	view := rpc.OrderView{OrderRef: "child", ReservedOrderID: 101, ConID: 10001, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD", Action: "SELL", OrderType: "STP", TIF: "GTC", TriggerMethod: 2, Quantity: 100, Filled: 20, Remaining: 80, EntryLink: link, EntryState: "active", FixedStopPrice: 76}
	d := rpc.OrderDraft{Contract: rpc.ContractParams{ConID: 10001, Symbol: "SYNX", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Action: "SELL", OrderType: "STP", TIF: "GTC", TriggerMethod: 2, Quantity: 50, EntryLink: link, FixedStopPrice: 76}
	o, err := attachedStockStopModifyOrder(view, d, rpc.TradingStatus{})
	if err != nil {
		t.Fatal(err)
	}
	if o.TotalQty != 70 || o.ParentID != 100 || o.AuxPrice != 76 {
		t.Fatalf("wire total/parent/price: %+v", o)
	}
}

func TestAttachedStopCapacityReservesUnseenAndUnprovenOCA(t *testing.T) {
	c := rpc.ContractParams{ConID: 10001, SecType: "STK", Symbol: "SYNX", Currency: "USD"}
	scope := brokerStateScope{Account: "synthetic", Mode: "paper"}
	makeRow := func(id int) brokerWorkingOrder {
		return brokerWorkingOrder{Order: ibkrlib.OrderLifecycleEvent{OrderID: id, OrderRef: "synthetic", ConID: c.ConID, SecType: c.SecType, Symbol: c.Symbol, Currency: c.Currency, Action: "SELL", OrderType: "STP", Remaining: 100, TotalQuantity: 100}, Journal: &rpc.OrderView{Open: true, ReservedOrderID: id, OrderRef: "synthetic", PreviewTokenID: "synthetic", Account: scope.Account, Mode: scope.Mode, EntryState: "active"}}
	}
	old, newer := makeRow(100), makeRow(200)
	newer.Journal.EntryLink = &rpc.StockEntryLink{ParentID: 199}
	old.Unseen = true
	cap, ok := attachedStopCapacity([]brokerWorkingOrder{old, newer}, newer, c, scope, 150)
	if !ok || cap != 50 {
		t.Fatalf("unseen capacity %v %v", cap, ok)
	}
	old.Order.OcaGroup, newer.Order.OcaGroup = "synthetic-unseen-oca", "synthetic-unseen-oca"
	old.Order.OcaType, newer.Order.OcaType = 2, 2
	cap, _ = attachedStopCapacity([]brokerWorkingOrder{old, newer}, newer, c, scope, 150)
	if cap != 50 {
		t.Fatalf("unseen OCA intent shared capacity: %g", cap)
	}
	old.Unseen = false
	old.Order.OcaGroup = "synthetic-oca"
	newer.Order.OcaGroup = old.Order.OcaGroup
	for _, typ := range []int{0, 3, 2} {
		old.Order.OcaType = typ
		newer.Order.OcaType = typ
		cap, _ = attachedStopCapacity([]brokerWorkingOrder{old, newer}, newer, c, scope, 150)
		want := 50.0
		if typ == 2 {
			want = 100
		}
		if cap != want {
			t.Fatalf("OCA type%d capacity%g want%g", typ, cap, want)
		}
	}
}
