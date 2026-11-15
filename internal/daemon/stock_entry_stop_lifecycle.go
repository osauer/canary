package daemon

import (
	"github.com/osauer/canary/v3/internal/rpc"
	"strings"
)

// Derive from the full durable event sequence, including terminal parents.
// Holdings, disappearance from open orders and local send returns prove neither
// a parent fill nor activation of its child.
func deriveStockEntryStates(views []rpc.OrderView, events []orderJournalEvent) {
	for i := range views {
		child := &views[i]
		link := child.EntryLink
		if link == nil {
			continue
		}
		child.ModifyEligible = false
		child.EntryState = "relationship_unknown"
		var parent *rpc.OrderView
		matches := 0
		for j := range views {
			p := &views[j]
			if p.OrderRef == link.ParentRef && p.ReservedOrderID == link.ParentID && p.Account == child.Account && p.ClientID == child.ClientID && p.Endpoint == child.Endpoint && p.Mode == child.Mode {
				matches++
				parent = p
			}
		}
		if matches != 1 || parent.AttachedStop == nil || link.PairRef != parent.OrderRef || parent.Action != "BUY" || parent.SecType != "STK" || parent.ConID != child.ConID || parent.Currency != child.Currency || parent.Quantity != float64(link.EntryQuantity) || (child.Quantity <= 0 || child.Quantity > float64(link.EntryQuantity)) || child.OrderType != "STP" || child.Action != "SELL" || child.FixedStopPrice != parent.AttachedStop.Price || child.TIF != "GTC" || child.OutsideRTH || child.TriggerMethod != 2 {
			continue
		}
		if child.BrokerParentID != nil && *child.BrokerParentID != link.ParentID {
			continue
		}
		if child.BrokerParentID == nil {
			if child.SendState == orderSendStateSendAttempted {
				child.EntryState = "awaiting_broker"
			}
			continue
		}
		fullAt, workingAt := -1, -1
		shrinkProven := child.Quantity == float64(link.EntryQuantity)
		var shrinkIntent *orderJournalEvent
		for n, e := range events {
			if orderJournalKey(e) == orderViewKey(*child) && e.Type == orderJournalEventModifyRequested && e.EntryLink != nil && stockEntryLinksEqual(e.EntryLink, link) && e.FixedStopPrice == parent.AttachedStop.Price && e.Quantity == child.Quantity {
				intent := e
				shrinkIntent = &intent
			}
			if shrinkIntent != nil && orderJournalKey(e) == orderViewKey(*child) && orderModifyAcknowledgementMatches(*shrinkIntent, e) {
				shrinkProven = true
			}
			broker := e.Type == orderJournalEventBrokerAcknowledged || e.Type == orderJournalEventStatusUpdated
			if !broker {
				continue
			}
			if orderJournalKey(e) == orderViewKey(*parent) && e.ReservedOrderID == parent.ReservedOrderID && e.Filled >= float64(link.EntryQuantity) {
				if fullAt < 0 {
					fullAt = n
				}
			}
			if orderJournalKey(e) == orderViewKey(*child) && e.ReservedOrderID == child.ReservedOrderID && e.Status != "Execution" {
				workingAt = -1
				if e.BrokerParentID != nil && *e.BrokerParentID == link.ParentID && (e.Status == "Submitted" || e.Status == "PreSubmitted") && e.WhyHeld == "" && (e.Deactivated == nil || !*e.Deactivated) {
					workingAt = n
				}
			}
		}
		if !shrinkProven {
			continue
		}
		if fullAt >= 0 && parent.Filled >= float64(link.EntryQuantity) {
			child.EntryState = "activation_pending"
			if workingAt > fullAt && child.Open && (child.LifecycleStatus == rpc.OrderLifecycleSubmitted || child.LifecycleStatus == rpc.OrderLifecyclePreSubmitted || child.LifecycleStatus == rpc.OrderLifecyclePartiallyFilled) && (child.Deactivated == nil || !*child.Deactivated) {
				child.EntryState = "active"
				child.ModifyEligible = orderViewModifyEligible(*child)
			}
			if !child.Open {
				child.EntryState = "terminal"
			}
		} else if parent.Open && (parent.LifecycleStatus == rpc.OrderLifecycleSubmitted || parent.LifecycleStatus == rpc.OrderLifecyclePreSubmitted || parent.LifecycleStatus == rpc.OrderLifecyclePartiallyFilled) && child.Open && strings.EqualFold(child.Status, "PreSubmitted") {
			child.EntryState = "held"
		}
	}
}
