package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
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

// stockEntryRefusedStop is the attached stop of parent that the broker
// refused before any share of it filled, if there is exactly one.
func stockEntryRefusedStop(views []rpc.OrderView, parent rpc.OrderView) (rpc.OrderView, bool) {
	if parent.AttachedStop == nil {
		return rpc.OrderView{}, false
	}
	var found []rpc.OrderView
	for _, child := range views {
		link := child.EntryLink
		if link == nil || link.ParentRef != parent.OrderRef || link.ParentID != parent.ReservedOrderID || child.Account != parent.Account ||
			child.ClientID != parent.ClientID || child.Endpoint != parent.Endpoint || child.Mode != parent.Mode {
			continue
		}
		found = append(found, child)
	}
	if len(found) != 1 {
		return rpc.OrderView{}, false
	}
	child := found[0]
	refused := !child.Open && child.Filled == 0 && (child.LifecycleStatus == rpc.OrderLifecycleRejected || child.LifecycleStatus == rpc.OrderLifecycleInactive)
	return child, refused
}

// stockEntryAwaitingWithdrawal reports an entry whose attached stop the
// broker refused while the broker has never acknowledged the entry itself:
// sent with transmit=false, it can sit untransmitted at the broker, where a
// human could transmit it by hand without its stop. The daemon withdraws
// it, once: an entry the journal already holds a cancel for (cancelSent),
// whatever the broker answered, is never cancelled again. An entry the
// broker acknowledged (working or filled) is not touched.
func stockEntryAwaitingWithdrawal(views []rpc.OrderView, parent rpc.OrderView, cancelSent bool) bool {
	if cancelSent || !parent.Open || parent.PermID != 0 || parent.SendState == orderSendStateBrokerAcknowledged || parent.Filled > 0 ||
		parent.ReservedOrderID <= 0 || parent.LifecycleStatus == rpc.OrderLifecyclePendingCancel {
		return false
	}
	_, refused := stockEntryRefusedStop(views, parent)
	return refused
}

// orderCancelRequested reports whether the journal holds a cancel request
// for the order.
func orderCancelRequested(events []orderJournalEvent, view rpc.OrderView) bool {
	key := orderViewKey(view)
	return slices.ContainsFunc(events, func(e orderJournalEvent) bool {
		return e.Type == orderJournalEventCancelRequested && orderJournalKey(e) == key
	})
}

// deriveRefusedStopPairs says on both records of a pair whose stop the
// broker refused what that means for the entry, with the broker's reason:
// withdrawn (only when the journal holds Canary's cancel for it), its
// withdrawal sent, about to be withdrawn, working without the stop, or
// filled without it. An entry with its own broker verdict (rejected,
// inactive) keeps it. A later broker event's text never hides the sentence,
// since it is derived from the pair each time.
func deriveRefusedStopPairs(views []rpc.OrderView, events []orderJournalEvent) {
	for i := range views {
		parent := &views[i]
		child, refused := stockEntryRefusedStop(views, *parent)
		if !refused || parent.LifecycleStatus == rpc.OrderLifecycleRejected || parent.LifecycleStatus == rpc.OrderLifecycleInactive {
			continue
		}
		reason := strings.TrimSpace(child.LastMessage)
		if reason == "" {
			reason = "no reason given"
		}
		cancelSent := orderCancelRequested(events, *parent)
		var msg string
		switch {
		case parent.Filled > 0:
			msg = fmt.Sprintf("The broker refused the attached stop (%s); the entry filled %g shares that this stop does not protect.", reason, parent.Filled)
		case !parent.Open && cancelSent:
			msg = fmt.Sprintf("The broker refused the attached stop (%s); the entry was withdrawn.", reason)
		case !parent.Open:
			continue
		case cancelSent:
			msg = fmt.Sprintf("The broker refused the attached stop (%s); the entry's withdrawal was sent to the broker.", reason)
		case stockEntryAwaitingWithdrawal(views, *parent, false):
			parent.CancelEligible = true
			msg = fmt.Sprintf("The broker refused the attached stop (%s); the entry is being withdrawn.", reason)
		default:
			msg = fmt.Sprintf("The broker refused the attached stop (%s); the entry is working without it.", reason)
		}
		parent.LastMessage = msg
		for j := range views {
			if orderViewKey(views[j]) == orderViewKey(child) {
				views[j].LastMessage = msg
			}
		}
	}
}
