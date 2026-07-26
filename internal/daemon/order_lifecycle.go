package daemon

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type orderLifecycleJournalBinding struct {
	connectorEpoch atomic.Uint64
}

func (s *Server) registerOrderLifecycleJournal(c *ibkrlib.Connector) {
	if s == nil || c == nil || s.orderJournal == nil {
		return
	}
	s.mu.Lock()
	if s.connector != c {
		s.mu.Unlock()
		return
	}
	connectorEpoch := s.connectorEpoch
	s.mu.Unlock()
	if s.orderLifecycleRegisterAfterCapture != nil {
		s.orderLifecycleRegisterAfterCapture()
	}

	s.orderLifecycleHandlersMu.Lock()
	s.mu.Lock()
	publicationStillCurrent := s.connector == c && s.connectorEpoch == connectorEpoch
	s.mu.Unlock()
	if !publicationStillCurrent {
		s.orderLifecycleHandlersMu.Unlock()
		return
	}
	if s.orderLifecycleHandlers == nil {
		s.orderLifecycleHandlers = make(map[*ibkrlib.Connector]*orderLifecycleJournalBinding)
	}
	if binding := s.orderLifecycleHandlers[c]; binding != nil {
		// Same-pointer republish reuses the one receipt handler. Updating its
		// daemon epoch prevents duplicate stale closures from latching every
		// current callback after a reconnect.
		storeOrderLifecycleConnectorEpoch(binding, connectorEpoch)
		s.orderLifecycleHandlersMu.Unlock()
		return
	}
	binding := &orderLifecycleJournalBinding{}
	binding.connectorEpoch.Store(connectorEpoch)
	c.RegisterOrderLifecycleReceiptHandler(s.boundOrderLifecycleHandler(c, binding))
	s.orderLifecycleHandlers[c] = binding
	s.orderLifecycleHandlersMu.Unlock()
}

func storeOrderLifecycleConnectorEpoch(binding *orderLifecycleJournalBinding, connectorEpoch uint64) {
	if binding == nil {
		return
	}
	for {
		current := binding.connectorEpoch.Load()
		if current >= connectorEpoch || binding.connectorEpoch.CompareAndSwap(current, connectorEpoch) {
			return
		}
	}
}

func (s *Server) boundOrderLifecycleHandler(c *ibkrlib.Connector, binding *orderLifecycleJournalBinding) func(ibkrlib.OrderLifecycleReceipt) {
	return func(receipt ibkrlib.OrderLifecycleReceipt) {
		// dispatchOrderLifecycle holds c's evidence-barrier read side for this
		// whole callback. Connector unpublication takes the exclusive side, so
		// the identity check and journal append are one publication interval.
		sessionCurrent := c.SessionReceiptCurrent(receipt.Session)
		if s.orderLifecycleSessionCurrentForTest != nil {
			sessionCurrent = s.orderLifecycleSessionCurrentForTest(c, receipt.Session)
		}
		s.mu.Lock()
		publicationCurrent := s.connector == c && binding != nil && s.connectorEpoch == binding.connectorEpoch.Load()
		s.mu.Unlock()
		if !sessionCurrent || !publicationCurrent || !s.appendOrderLifecycleEvent(receipt.Event) {
			s.markOrderLifecyclePersistenceUncertain()
		}
	}
}

func (s *Server) markOrderLifecyclePersistenceUncertain() {
	if s == nil {
		return
	}
	// Publish the generation before the latch. Reconciliation samples the
	// generation first and rechecks it after a clear so a racing failure wins.
	s.orderLifecyclePersistenceFailures.Add(1)
	s.orderLifecyclePersistenceUncertain.Store(true)
}

func (s *Server) forgetOrderLifecycleJournal(c *ibkrlib.Connector) {
	if s == nil || c == nil {
		return
	}
	// Stop has a bounded wait and is not proof that every decoded callback has
	// finished. Drop only the Server's strong reference after Stop; leave the
	// receipt handler on the retired Connector so any late frame still rejects
	// against publication identity and latches uncertainty. The one-way
	// Connector -> handler -> Server references do not keep Connector alive.
	s.orderLifecycleHandlersMu.Lock()
	delete(s.orderLifecycleHandlers, c)
	s.orderLifecycleHandlersMu.Unlock()
}

// appendOrderLifecycleEvent reports whether every lifecycle fact for which
// the daemon owns journal authority was durably committed. Unmatched
// all-client inventory callbacks are intentionally outside that authority and
// therefore count as successfully ignored, not as persistence loss.
func (s *Server) appendOrderLifecycleEvent(ev ibkrlib.OrderLifecycleEvent) bool {
	if s == nil || s.orderJournal == nil {
		return false
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	journalEvent, ok := orderJournalEventFromLifecycle(ev, now)
	if !ok {
		return true
	}
	// Bind broker callbacks to the currently connected write route before any
	// journal lookup. Broker order IDs and permanent IDs can legitimately recur
	// on another endpoint/client/account/mode and must never select that row.
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if journalEvent.Account == "" {
		journalEvent.Account = status.Account
	}
	if journalEvent.Endpoint == "" {
		journalEvent.Endpoint = status.Endpoint
	}
	if journalEvent.Mode == "" {
		journalEvent.Mode = status.Mode
	}
	if !journalEvent.clientIDPresent {
		// The current route supplies a client for scoping, but it cannot turn an
		// omitted legacy callback field into exact broker-order identity.
		journalEvent.ClientID = status.ClientID
	}
	matched, viewsLoaded := s.enrichOrderLifecycleEventFromJournal(&journalEvent)
	if viewsLoaded && !matched &&
		(ev.Type == ibkrlib.OrderLifecycleEventOpenOrder || ev.Type == ibkrlib.OrderLifecycleEventStatus) {
		// reqAllOpenOrders snapshots surface ALL of the account's open
		// orders, including manual TWS orders this journal has never
		// tracked. The journal's contract is daemon-placed orders only
		// ("may miss manual orders"), so unmatched openOrder/orderStatus
		// callbacks are observed, not adopted — journaling them would mint
		// phantom rows. Keyed on the broker event type: execDetails and
		// error events keep their existing pass-through behavior.
		s.debugf("order lifecycle: dropping unmatched %s callback for broker order %d (perm %d)", ev.Type, ev.OrderID, ev.PermID)
		return true
	}
	normalizeOrderLifecycleJournalEvent(&journalEvent)

	var persistErr error
	if s.purgeLedger != nil {
		persistErr = s.purgeLedger.CommitOrderLifecycle(s.orderJournal, journalEvent)
	} else {
		persistErr = s.orderJournal.Append(journalEvent)
	}
	if persistErr != nil {
		s.warnf("append order lifecycle event: %v", persistErr)
		return false
	}
	if journalEvent.Source == proposalOrderSource && s.proposalOutcomes != nil && journalEvent.Filled > 0 {
		var submitted proposalEvent
		if s.tradeProposals != nil && s.tradeProposals.store != nil {
			if ev, ok, err := s.tradeProposals.store.FindSubmittedEvent(journalEvent.OrderRef, journalEvent.PreviewTokenID); err != nil {
				s.warnf("lookup submitted proposal event: %v", err)
			} else if ok {
				submitted = ev
			}
		}
		if submitted.PolicyID == "" {
			s.warnf("skip proposal outcome fill: no submitted proposal event for order_ref=%s token_id=%s", journalEvent.OrderRef, journalEvent.PreviewTokenID)
			return true
		}
		if err := s.proposalOutcomes.AppendMark(proposalOutcomeFilledFromJournal(journalEvent, submitted, now)); err != nil {
			s.warnf("append proposal outcome fill: %v", err)
		}
	}
	return true
}

func normalizeOrderLifecycleJournalEvent(ev *orderJournalEvent) {
	if ev == nil {
		return
	}
	if ev.Type != orderJournalEventStatusUpdated || !strings.EqualFold(ev.Status, "Execution") {
		return
	}
	if ev.Quantity > 0 && ev.Filled >= ev.Quantity-1e-9 {
		ev.Status = "Filled"
		ev.Remaining = 0
		ev.SendState = orderSendStateTerminal
		return
	}
	if ev.Remaining > 0 {
		ev.SendState = orderSendStateBrokerAcknowledged
	}
}

// enrichOrderLifecycleEventFromJournal copies identity from the matching
// journal view. matched reports whether an existing row claimed the event;
// viewsLoaded distinguishes "no match" from "journal unreadable" so callers
// can fail open on load errors instead of dropping broker evidence.
func (s *Server) enrichOrderLifecycleEventFromJournal(ev *orderJournalEvent) (matched, viewsLoaded bool) {
	if s == nil || ev == nil || s.orderJournal == nil {
		return false, false
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return false, false
	}
	if view, ok := orderJournalViewForLifecycleEvent(*ev, views); ok {
		copyOrderJournalIdentityFromView(ev, view)
		return true, true
	}
	return false, true
}

func orderJournalViewForLifecycleEvent(ev orderJournalEvent, views []rpc.OrderView) (rpc.OrderView, bool) {
	if ev.PermID != 0 {
		for _, view := range views {
			if view.PermID == ev.PermID && orderJournalEventRouteMatchesView(ev, view) {
				return view, true
			}
		}
		// The first broker acknowledgement can introduce the PermID to a
		// pre-transmit row that already has an exact local reference. Permit
		// that one-way enrichment only while the journal row has no PermID;
		// a different known PermID is a hard identity conflict.
		matches := make([]rpc.OrderView, 0, 1)
		for _, view := range views {
			if view.PermID != 0 || !orderJournalEventRouteMatchesView(ev, view) {
				continue
			}
			if ev.OrderRef != "" && view.OrderRef == ev.OrderRef {
				matches = append(matches, view)
				continue
			}
			if ev.ReservedOrderID != 0 && ev.clientIDPresent && view.ReservedOrderID == ev.ReservedOrderID {
				matches = append(matches, view)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		// A known account-wide PermID is authoritative. Falling through to a
		// colliding client-local order ID would project another order's fields
		// into this journal row.
		return rpc.OrderView{}, false
	}
	if ev.OrderRef != "" {
		for _, view := range views {
			if view.OrderRef == ev.OrderRef && orderJournalEventRouteMatchesView(ev, view) {
				return view, true
			}
		}
	}
	if ev.ReservedOrderID == 0 || !ev.clientIDPresent {
		return rpc.OrderView{}, false
	}
	matches := make([]rpc.OrderView, 0, 1)
	openMatches := make([]rpc.OrderView, 0, 1)
	for _, view := range views {
		if view.ReservedOrderID != ev.ReservedOrderID || !orderJournalEventRouteMatchesView(ev, view) {
			continue
		}
		matches = append(matches, view)
		if view.Open {
			openMatches = append(openMatches, view)
		}
	}
	if len(openMatches) == 1 {
		return openMatches[0], true
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	return rpc.OrderView{}, false
}

func orderJournalEventRouteMatchesView(ev orderJournalEvent, view rpc.OrderView) bool {
	return strings.TrimSpace(ev.Endpoint) == strings.TrimSpace(view.Endpoint) &&
		ev.ClientID == view.ClientID &&
		strings.EqualFold(strings.TrimSpace(ev.Account), strings.TrimSpace(view.Account)) &&
		strings.EqualFold(strings.TrimSpace(ev.Mode), strings.TrimSpace(view.Mode))
}

func copyOrderJournalIdentityFromView(ev *orderJournalEvent, view rpc.OrderView) {
	if ev.OrderRef == "" {
		ev.OrderRef = view.OrderRef
	}
	if ev.PreviewTokenID == "" {
		ev.PreviewTokenID = view.PreviewTokenID
	}
	if ev.Source == "" {
		ev.Source = view.Source
	}
	if ev.PurgeID == "" {
		ev.PurgeID = view.PurgeID
	}
	if ev.LegID == "" {
		ev.LegID = view.LegID
	}
	if view.BypassPreview {
		ev.BypassPreview = true
	}
	if ev.Account == "" {
		ev.Account = view.Account
	}
	if ev.Endpoint == "" {
		ev.Endpoint = view.Endpoint
	}
	if ev.Mode == "" {
		ev.Mode = view.Mode
	}
	if !ev.clientIDPresent {
		ev.ClientID = view.ClientID
		ev.clientIDPresent = true
	}
	if ev.ConID == 0 {
		ev.ConID = view.ConID
	}
	if ev.Symbol == "" {
		ev.Symbol = view.Symbol
	}
	if ev.SecType == "" {
		ev.SecType = view.SecType
	}
	if ev.Exchange == "" {
		ev.Exchange = view.Exchange
	}
	if ev.PrimaryExch == "" {
		ev.PrimaryExch = view.PrimaryExch
	}
	if ev.Currency == "" {
		ev.Currency = view.Currency
	}
	if ev.LocalSymbol == "" {
		ev.LocalSymbol = view.LocalSymbol
	}
	if ev.TradingClass == "" {
		ev.TradingClass = view.TradingClass
	}
	if ev.Expiry == "" {
		ev.Expiry = view.Expiry
	}
	if ev.Strike == 0 {
		ev.Strike = view.Strike
	}
	if ev.Right == "" {
		ev.Right = view.Right
	}
	if ev.Multiplier == 0 {
		ev.Multiplier = view.Multiplier
	}
	if ev.Type == orderJournalEventBrokerAcknowledged {
		// openOrder carries the broker's actual mutable order snapshot. Filling
		// absent fields from the locally attempted view would let an incomplete or
		// stale callback falsely acknowledge a modify that IBKR has not reflected.
		return
	}
	if ev.Action == "" {
		ev.Action = view.Action
	}
	if ev.OrderType == "" {
		ev.OrderType = view.OrderType
	}
	if ev.TIF == "" {
		ev.TIF = view.TIF
	}
	if ev.TriggerMethod == 0 {
		ev.TriggerMethod = view.TriggerMethod
	}
	if view.OutsideRTH {
		ev.OutsideRTH = true
	}
	if ev.Quantity == 0 {
		ev.Quantity = view.Quantity
	}
	if ev.LimitPrice == 0 {
		ev.LimitPrice = view.LimitPrice
	}
	if ev.Trail == nil {
		ev.Trail = cloneTrailSpec(view.Trail)
	}
	if ev.OpenClose == "" {
		ev.OpenClose = view.OpenClose
	}
}
