package daemon

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
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
		if receipt.Uncertain || !sessionCurrent || !publicationCurrent || !s.appendOrderLifecycleEvent(receipt.Event) {
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

	persistErr := s.orderJournal.Append(journalEvent)
	if persistErr != nil {
		s.warnf("append order lifecycle event: %v", persistErr)
		return false
	}
	if (journalEvent.Type == orderJournalEventBrokerError || strings.EqualFold(journalEvent.Status, "Rejected") || strings.EqualFold(journalEvent.Status, "Inactive")) && s.serverCtx != nil {
		// A refused attached stop may leave its entry untransmitted at the
		// broker. A broker error often carries no order reference, so any
		// refusal triggers the check. The withdrawal runs outside this
		// callback's barrier, under brokerWriteMu, and rechecks the journal.
		go s.withdrawUnprotectedEntries(s.serverCtx)
	}
	s.offerSetupMarkoutFill(journalEvent)
	s.offerFillQuoteCapture(journalEvent)
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
		// A callback arrives keyed to the current socket. Key it to the
		// order's own route, the socket it was placed through, as a write's
		// events are (orderJournalStatusFor): one order, one journal row.
		ev.Endpoint = view.Endpoint
		copyOrderJournalIdentityFromView(ev, view)
		return true, true
	}
	return false, true
}

// orderJournalViewForLifecycleEvent pairs a broker callback with its journal
// row. Rows outside the callback's scope (client ID, account, mode) never
// pair. Within the scope every candidate passes one shared check
// (orderJournalPairingContradicted) before it is accepted, on the callback's
// own socket as across sockets:
//   - With a permanent ID, the row that holds it is the order. More than one
//     holder pairs only with the single one Canary placed (a duplicate born
//     from callbacks alone, as builds before v3.26.1 minted under the new
//     socket, loses to it); otherwise none. A holder on another socket also
//     needs the callback to name its client ID itself.
//   - With a permanent ID no row holds yet (the first acknowledgement), only
//     an openOrder introduces it, to the one open row without a permanent ID
//     in the whole scope that it names by reference (and, across sockets, by
//     order ID with an explicit client ID too); a row it names by order ID
//     alone, or a second candidate, refuses. An orderStatus or execution
//     never introduces a permanent ID (the refuter's second round).
//   - Without a permanent ID only the callback's own socket pairs: by
//     reference, else by order ID with an explicit client ID, never a row
//     whose reference differs.
//
// Error callbacks carry no client ID or permanent ID and never pair across
// sockets; hand orders and other clients' orders are not in the journal.
func orderJournalViewForLifecycleEvent(ev orderJournalEvent, views []rpc.OrderView) (rpc.OrderView, bool) {
	scope := make([]rpc.OrderView, 0, len(views))
	for _, view := range views {
		if orderJournalEventScopeMatchesView(ev, view) {
			scope = append(scope, view)
		}
	}
	var view rpc.OrderView
	var ok bool
	switch {
	case ev.PermID != 0:
		view, ok = orderJournalViewForPermID(ev, scope)
	default:
		view, ok = orderJournalViewWithoutPermID(ev, scope)
	}
	if !ok || orderJournalPairingContradicted(ev, view, scope) {
		return rpc.OrderView{}, false
	}
	return view, true
}

func orderJournalViewForPermID(ev orderJournalEvent, scope []rpc.OrderView) (rpc.OrderView, bool) {
	var holders, placed []rpc.OrderView
	for _, view := range scope {
		if view.PermID != ev.PermID {
			continue
		}
		holders = append(holders, view)
		if orderViewPlacedByCanary(view) {
			placed = append(placed, view)
		}
	}
	if len(holders) > 0 {
		var holder rpc.OrderView
		switch {
		case len(holders) == 1:
			holder = holders[0]
		case len(placed) == 1:
			holder = placed[0]
		default:
			return rpc.OrderView{}, false
		}
		if !orderJournalEventOnViewSocket(ev, holder) && !ev.clientIDPresent {
			return rpc.OrderView{}, false
		}
		return holder, true
	}
	// No row holds the permanent ID yet. Only the broker's openOrder, which
	// carries the order reference, introduces it, and only to the one open
	// row without a permanent ID that it names by reference: an orderStatus
	// or execution without the reference could be another order of the same
	// client ID behind the same order ID (another app on our client ID, an ID
	// reset), and its fills and cancels would land on Canary's row.
	if ev.Type != orderJournalEventBrokerAcknowledged || ev.OrderRef == "" {
		return rpc.OrderView{}, false
	}
	var candidates []rpc.OrderView
	for _, view := range scope {
		if view.PermID != 0 || !view.Open || orderJournalRefsDiffer(ev, view) {
			continue
		}
		if orderJournalRefNames(ev, view) || orderJournalOrderIDNames(ev, view) {
			candidates = append(candidates, view)
		}
	}
	if len(candidates) != 1 || !orderJournalRefNames(ev, candidates[0]) {
		return rpc.OrderView{}, false
	}
	view := candidates[0]
	if orderJournalEventOnViewSocket(ev, view) {
		return view, true
	}
	// Across sockets the order ID must agree too, named by the callback's
	// own client ID.
	return view, ev.clientIDPresent && orderJournalOrderIDNames(ev, view)
}

func orderJournalViewWithoutPermID(ev orderJournalEvent, scope []rpc.OrderView) (rpc.OrderView, bool) {
	if ev.OrderRef != "" {
		for _, view := range scope {
			if view.OrderRef == ev.OrderRef && orderJournalEventOnViewSocket(ev, view) {
				return view, true
			}
		}
	}
	if ev.ReservedOrderID == 0 || !ev.clientIDPresent {
		return rpc.OrderView{}, false
	}
	matches := make([]rpc.OrderView, 0, 1)
	openMatches := make([]rpc.OrderView, 0, 1)
	for _, view := range scope {
		if view.ReservedOrderID != ev.ReservedOrderID || !orderJournalEventOnViewSocket(ev, view) || orderJournalRefsDiffer(ev, view) {
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

// orderJournalPairingContradicted is the check every pairing passes: the
// row's reference, order ID and permanent ID agree with the callback's where
// both carry one, and no other row of the scope carries the callback's
// reference under another permanent ID. That no other row holds the
// callback's permanent ID is structural: orderJournalViewForPermID pairs a
// holder whenever one exists and enriches a row only when none does.
func orderJournalPairingContradicted(ev orderJournalEvent, view rpc.OrderView, scope []rpc.OrderView) bool {
	if orderJournalRefsDiffer(ev, view) {
		return true
	}
	if ev.PermID != 0 && view.PermID != 0 && ev.PermID != view.PermID {
		return true
	}
	if ev.PermID != 0 && view.PermID == ev.PermID && ev.ReservedOrderID != 0 && view.ReservedOrderID != 0 && ev.ReservedOrderID != view.ReservedOrderID {
		return true
	}
	if ev.PermID == 0 {
		return false
	}
	key := orderViewKey(view)
	for _, other := range scope {
		if orderViewKey(other) == key {
			continue
		}
		if ev.OrderRef != "" && other.OrderRef == ev.OrderRef && other.PermID != 0 && other.PermID != ev.PermID {
			return true
		}
	}
	return false
}

func orderJournalEventOnViewSocket(ev orderJournalEvent, view rpc.OrderView) bool {
	return strings.TrimSpace(ev.Endpoint) == strings.TrimSpace(view.Endpoint)
}

func orderJournalRefsDiffer(ev orderJournalEvent, view rpc.OrderView) bool {
	return ev.OrderRef != "" && view.OrderRef != "" && ev.OrderRef != view.OrderRef
}

func orderJournalRefNames(ev orderJournalEvent, view rpc.OrderView) bool {
	return ev.OrderRef != "" && view.OrderRef == ev.OrderRef
}

func orderJournalOrderIDNames(ev orderJournalEvent, view rpc.OrderView) bool {
	return ev.ReservedOrderID > 0 && view.ReservedOrderID == ev.ReservedOrderID
}

// orderViewPlacedByCanary reports whether the row carries Canary's own
// placement record (a preview token or a bypass-preview flow), not only
// broker callbacks.
func orderViewPlacedByCanary(view rpc.OrderView) bool {
	return strings.TrimSpace(view.PreviewTokenID) != "" || view.BypassPreview
}

// orderJournalEventScopeMatchesView is the order's identity without the
// socket: client ID, account and mode. Since 2026-10-20 08:00 CEST (owner)
// the socket an order was placed through is not part of its identity for a
// write (validateOrderViewMatchesGate); these three are.
func orderJournalEventScopeMatchesView(ev orderJournalEvent, view rpc.OrderView) bool {
	return ev.ClientID == view.ClientID &&
		strings.EqualFold(strings.TrimSpace(ev.Account), strings.TrimSpace(view.Account)) &&
		strings.EqualFold(strings.TrimSpace(ev.Mode), strings.TrimSpace(view.Mode))
}

func copyOrderJournalIdentityFromView(ev *orderJournalEvent, view rpc.OrderView) {
	if ev.EntryLink == nil {
		ev.EntryLink = view.EntryLink
	}
	if ev.AttachedStop == nil {
		ev.AttachedStop = view.AttachedStop
	}
	if ev.FixedStopPrice == 0 && ev.Type != orderJournalEventBrokerAcknowledged {
		ev.FixedStopPrice = view.FixedStopPrice
	}
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
	if ev.OCAGroup == "" {
		// Membership is identity Canary set at send time; a callback that
		// does not report it (status, execution, a legacy openOrder) keeps it.
		ev.OCAGroup, ev.OCAType = view.OCAGroup, view.OCAType
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
