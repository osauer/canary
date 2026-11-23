package daemon

import (
	"maps"
	"slices"

	"github.com/osauer/canary/v3/internal/rpc"
)

// orderReadState is the order journal's fold kept between reads, so an
// appended event folds into its one order instead of the whole journal
// being folded again. A broker order frame appends one event, and the next
// frame reads the journal: replaying 50,000 events on the broker's reader
// took seconds a frame and starved the open-order read of its deadline.
//
// An appended event folds in place only when it changes no identity the
// whole-journal fold decides: its keys are new or already name its order,
// it makes no order id ambiguous or prelinked, and neither it nor its order
// takes part in an entry-and-stop relationship (whose derived states read
// other orders). Anything else folds the whole journal again, as before.
type orderReadState struct {
	aliases    map[string]string
	ambiguous  map[string]bool
	prelinked  map[string]bool
	refsByID   map[string]map[string]bool
	firstRef   map[string]bool
	reflessPre map[string]bool
	folds      map[string]*orderViewFold
	byKey      map[string]int
	linked     map[string]bool
	views      []rpc.OrderView
	eventsBy   map[string][]rpc.OrderEvent
	// copied marks the folds an apply has copied for itself.
	copied map[string]bool
}

// newOrderReadState folds the whole journal, as buildOrderViews and
// buildOrderEventsByKey do, and keeps what an append needs.
func newOrderReadState(events []orderJournalEvent) *orderReadState {
	st := &orderReadState{
		aliases: orderJournalKeyAliases(events), ambiguous: ambiguousReservedOrderIDs(events), prelinked: reservedOrderIDsWithPrelinkedBrokerOnlyEvents(events),
		refsByID: map[string]map[string]bool{}, firstRef: map[string]bool{}, reflessPre: map[string]bool{},
		folds: map[string]*orderViewFold{}, linked: map[string]bool{},
	}
	for _, ev := range events {
		st.noteIdentity(ev)
		key := orderJournalCanonicalKey(ev, st.aliases)
		if key == "" {
			continue
		}
		fold := st.folds[key]
		if fold == nil {
			fold = &orderViewFold{attempts: make(map[string]*orderAttemptFold)}
			st.folds[key] = fold
		}
		fold.merge(ev)
	}
	st.views = make([]rpc.OrderView, 0, len(st.folds))
	for _, fold := range st.folds {
		st.views = append(st.views, finishOrderViewFold(fold))
	}
	deriveStockEntryStates(st.views, events)
	deriveRefusedStopPairs(st.views, events)
	sortOrderViews(st.views)
	st.index()
	st.eventsBy = buildOrderEventsByKey(events)
	return st
}

// finishOrderViewFold is a fold's view with its derived states, from a copy
// of the fold, so finishing it never changes what later events fold into.
func finishOrderViewFold(fold *orderViewFold) rpc.OrderView {
	f := *fold
	view, last, attemptUncertain := f.finish()
	view.LifecycleStatus = mapOrderViewLifecycleStatus(view, last, attemptUncertain)
	view.Open = orderViewIsOpen(view)
	view.ModifyEligible = orderViewModifyEligible(view)
	view.CancelEligible = orderViewCancelEligibleWithAttemptTruth(view, attemptUncertain)
	return view
}

// noteIdentity keeps the per-order-id facts the ambiguity and prelink sets
// are made of.
func (st *orderReadState) noteIdentity(ev orderJournalEvent) {
	if ev.ReservedOrderID == 0 {
		return
	}
	id := orderJournalScopedOrderIDKey(ev)
	if ev.OrderRef == "" {
		if !st.firstRef[id] {
			st.reflessPre[id] = true
		}
		return
	}
	refs := st.refsByID[id]
	if refs == nil {
		refs = map[string]bool{}
		st.refsByID[id] = refs
	}
	refs[ev.OrderRef] = true
	st.firstRef[id] = true
}

func (st *orderReadState) index() {
	st.byKey = make(map[string]int, len(st.views))
	st.linked = map[string]bool{}
	for i, v := range st.views {
		st.byKey[orderViewKey(v)] = i
		if v.EntryLink != nil {
			st.linked[v.EntryLink.ParentRef] = true
			st.linked[v.OrderRef] = true
		}
		if v.AttachedStop != nil {
			st.linked[v.OrderRef] = true
		}
	}
}

// apply folds appended events into a copy of the state. ok is false when
// one of them changes an identity or a relationship the whole fold decides;
// the caller then folds the whole journal.
func (st *orderReadState) apply(added []orderJournalEvent) (*orderReadState, bool) {
	next := *st
	next.copied = nil
	next.aliases = maps.Clone(st.aliases)
	next.views = slices.Clone(st.views)
	next.eventsBy = maps.Clone(st.eventsBy)
	next.byKey = maps.Clone(st.byKey)
	touched := map[string]bool{}
	for _, ev := range added {
		// An order in an entry-and-stop pair derives its state from its
		// partner; its events fold the whole journal. A broker parent id on
		// any other order is a field of that order alone.
		if ev.EntryLink != nil || ev.AttachedStop != nil || ev.BrokerParentID != nil && *ev.BrokerParentID != 0 {
			return nil, false
		}
		if ev.ReservedOrderID != 0 {
			id := orderJournalScopedOrderIDKey(ev)
			if ev.OrderRef != "" {
				refs := st.refsByID[id]
				if len(refs) > 0 && !refs[ev.OrderRef] || !st.firstRef[id] && st.reflessPre[id] {
					return nil, false // the order id becomes ambiguous or prelinked
				}
				if refs == nil {
					return nil, false // the first reference for this order id
				}
			} else if !st.firstRef[id] && !st.reflessPre[id] {
				return nil, false // the first reference-less event for this order id
			}
		}
		keys := orderJournalIdentityKeysForAliases(ev, st.ambiguous, st.prelinked)
		if len(keys) == 0 {
			continue
		}
		canonical := ""
		for _, k := range keys {
			if resolved := resolveOrderJournalAlias(k, next.aliases); resolved != "" {
				canonical = resolved
				break
			}
		}
		if ev.OrderRef != "" {
			canonical = orderJournalRouteIdentity(ev.Endpoint, ev.ClientID, ev.Account, ev.Mode) + "|ref:" + ev.OrderRef
		}
		if canonical == "" {
			canonical = keys[0]
		}
		for _, k := range keys {
			if to, ok := next.aliases[k]; ok && to != canonical {
				return nil, false // the event links keys of different orders
			}
		}
		for _, k := range keys {
			next.aliases[k] = canonical
		}
		key := orderJournalCanonicalKey(ev, next.aliases)
		if key == "" {
			continue
		}
		if i, ok := st.byKey[key]; ok && st.linked[st.views[i].OrderRef] {
			return nil, false
		}
		touched[key] = true
		next.eventsBy[key] = append(slices.Clip(next.eventsBy[key]), orderEventFromJournal(ev))
		pending := next.foldFor(key)
		pending.merge(ev)
	}
	if len(touched) == 0 {
		next.copied = nil
		return &next, true
	}
	for key := range touched {
		view := finishOrderViewFold(next.folds[key])
		if view.EntryLink != nil || view.AttachedStop != nil {
			return nil, false
		}
		if i, ok := next.byKey[key]; ok {
			next.views[i] = view
		} else {
			next.views = append(next.views, view)
		}
	}
	sortOrderViews(next.views)
	next.index()
	next.copied = nil
	return &next, true
}

// foldFor is the order's fold in next, copied on first use so st's folds
// are never changed by an append that may still be refused.
func (st *orderReadState) foldFor(key string) *orderViewFold {
	if st.copied == nil {
		st.copied = map[string]bool{}
		st.folds = maps.Clone(st.folds)
	}
	fold := st.folds[key]
	if fold == nil {
		fold = &orderViewFold{attempts: make(map[string]*orderAttemptFold)}
		st.folds[key] = fold
		st.copied[key] = true
		return fold
	}
	if !st.copied[key] {
		c := *fold
		c.attempts = make(map[string]*orderAttemptFold, len(fold.attempts))
		for id, a := range fold.attempts {
			copied := *a
			c.attempts[id] = &copied
		}
		fold = &c
		st.folds[key] = fold
		st.copied[key] = true
	}
	return fold
}
