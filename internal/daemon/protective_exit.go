package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Protective stock exit exemption (owner decision 2026-10-05 17:36 CEST,
// "exempt with the guard"; see internal-docs/design/protective-stop-guard.md).
//
// A broker-side stop that sells at most the long stock position it protects
// passes both the order cap in force ([order_limits]) and the apparent-exit
// re-read as a short
// open, provided the complete current broker open-order inventory shows no
// other working sell for the same contract that, together with this one,
// would sell more than is held. A modify that only lowers the quantity of a
// working protective stop passes on the same terms, without the competing-sell
// condition: it can only shrink what the book already carries. Everything
// else keeps both gates. The protective stop guard keeps Canary's own working
// stops no larger than the position afterwards.

// Blocker codes a trailing-stop proposal row carries when its placement would
// be refused because the exemption cannot apply.
const (
	protectiveExitInventoryUnavailableCode = "protective_exit_inventory_unavailable"
	protectiveExitCompetingSellCode        = "protective_exit_competing_sell"
	protectiveExitExceedsPositionCode      = "protective_exit_exceeds_position"
)

// protectiveExitInventory is the broker open-order evidence the exemptions
// read: the protective stock exit, the bond sale and the delta-reducing exit
// (delta_reduction.go) each require that this order and every other working
// order in its direction together stay within the held line. The zero value
// is "unavailable" and never exempts.
type protectiveExitInventory struct {
	// Current is true only for a complete, current all-client open-order
	// snapshot of the order's own account and mode.
	Current bool
	// OtherWorkingSameSide is the remaining quantity of every other working
	// order for the exact contract in the draft's own direction (sells
	// beside a sell, buys beside a buy), hand orders in TWS included.
	OtherWorkingSameSide float64
	// OtherWorkingSameSideByLeg is the same count per leg of a strategy
	// combo, by the leg's ConID and in the leg's own direction; nil for a
	// single-contract draft.
	OtherWorkingSameSideByLeg map[int]float64
	// ReducesWorkingStop is true for a modify whose target is a working sell
	// stop on the same contract and whose new quantity is below the target's
	// remaining quantity.
	ReducesWorkingStop bool
}

// otherWorkingSameSide is the competing working quantity for one contract
// of the draft: the combo leg's count, or the single contract's.
func (inv protectiveExitInventory) otherWorkingSameSide(conID int) float64 {
	if inv.OtherWorkingSameSideByLeg != nil {
		return inv.OtherWorkingSameSideByLeg[conID]
	}
	return inv.OtherWorkingSameSide
}

// isProtectiveStopOrderType reports the stop kinds a protective exit may use.
// Canary previews TRAIL and TRAIL LIMIT; STP and STP LMT are named so a stop
// read from the broker classifies the same way.
func isProtectiveStopOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT, "STP", "STP LMT":
		return true
	default:
		return false
	}
}

// protectiveStockExitCandidate is the shape test that needs no broker
// evidence: a single-contract stock/ETF sell stop that closes or reduces a
// long position. Only a candidate reads the open-order inventory.
func protectiveStockExitCandidate(draft rpc.OrderDraft, position rpc.OrderPositionImpact) bool {
	if draft.StrategyGroup != nil || !isStockLikeRiskSecType(draft.Contract.SecType) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(draft.Action), rpc.OrderActionSell) || !isProtectiveStopOrderType(draft.OrderType) {
		return false
	}
	qty := float64(draft.Quantity)
	if draft.Quantity <= 0 || !positiveFinite(position.Before) || qty > position.Before+1e-9 {
		return false
	}
	if math.Abs(position.After-(position.Before-qty)) > 1e-9 || !isRiskReducing(position.Effect) {
		return false
	}
	return true
}

// protectiveStockExitExempt decides the exemption. Stale or incomplete
// inventory never exempts; today's refusal stands.
func protectiveStockExitExempt(draft rpc.OrderDraft, position rpc.OrderPositionImpact, inv protectiveExitInventory) bool {
	if !protectiveStockExitCandidate(draft, position) || !inv.Current {
		return false
	}
	if inv.ReducesWorkingStop {
		return true
	}
	if math.IsNaN(inv.OtherWorkingSameSide) || math.IsInf(inv.OtherWorkingSameSide, 0) || inv.OtherWorkingSameSide < 0 {
		return false
	}
	return inv.OtherWorkingSameSide+float64(draft.Quantity) <= position.Before+1e-9
}

// brokerOrderRemaining is the quantity a working broker order can still fill.
func brokerOrderRemaining(order ibkrlib.OrderLifecycleEvent) float64 {
	if order.Remaining > 0 {
		return order.Remaining
	}
	if order.TotalQuantity > order.Filled {
		return order.TotalQuantity - order.Filled
	}
	return 0
}

// workingOrderIdentity is what the competing-order count needs of a working
// order, from a broker snapshot row or from Canary's own journal row.
type workingOrderIdentity struct {
	SecType, Symbol, Action string
	ConID                   int
	Remaining               float64
	// Legs are the option contracts of a combo (BAG) order the journal
	// knows; nil for a hand combo, which then counts against every leg.
	Legs map[int]struct{}
}

// journalInventory is Canary's own order rows for the competing-order count:
// the journal's views and the legs of each combo it placed, by order
// reference and by session order id.
type journalInventory struct {
	views         []rpc.OrderView
	legsByRef     map[string]map[int]struct{}
	legsByOrderID map[int]map[int]struct{}
}

// legsOf is the combo legs the journal knows for a row, or nil.
func (j journalInventory) legsOf(view *rpc.OrderView) map[int]struct{} {
	if view == nil {
		return nil
	}
	if legs, ok := j.legsByRef[view.OrderRef]; ok && view.OrderRef != "" {
		return legs
	}
	if legs, ok := j.legsByOrderID[view.ReservedOrderID]; ok && view.ReservedOrderID > 0 {
		return legs
	}
	return nil
}

// journalInventoryMargin is how long before a snapshot's completion its
// request may have begun: the flight's own budget (orderReconcileSnapshotWait).
// An unpaired open journal row updated after that counts, so an order placed
// while the snapshot was in flight, absent from it because the broker had
// not acknowledged it yet, is not read as settled.
const journalInventoryMargin = orderReconcileSnapshotWait

func brokerOrderIdentity(order ibkrlib.OrderLifecycleEvent) workingOrderIdentity {
	return workingOrderIdentity{SecType: order.SecType, Symbol: order.Symbol, Action: order.Action, ConID: order.ConID, Remaining: brokerOrderRemaining(order)}
}

// journalOrderIdentity reads a journal row; one the broker has not yet
// acknowledged has no remaining quantity, so its whole quantity counts.
func journalOrderIdentity(view rpc.OrderView) workingOrderIdentity {
	remaining := view.Remaining
	if remaining <= 0 {
		remaining = view.Quantity
	}
	return workingOrderIdentity{SecType: view.SecType, Symbol: view.Symbol, Action: view.Action, ConID: view.ConID, Remaining: remaining}
}

// sameContract matches the order to the exact contract. An order without a
// ConID that names the same symbol in the contract's own class (a stock or
// ETF beside a stock, an option on the underlying beside an option, a bill
// or bond beside a bond) counts: doubt adds to the competing quantity, never
// removes from it.
func (w workingOrderIdentity) sameContract(contract rpc.ContractParams) bool {
	if w.ConID > 0 && contract.ConID > 0 {
		return w.ConID == contract.ConID
	}
	if !strings.EqualFold(strings.TrimSpace(w.Symbol), strings.TrimSpace(contract.Symbol)) {
		return false
	}
	secType := strings.TrimSpace(w.SecType)
	switch {
	case secType == "":
		return true
	case strings.EqualFold(contract.SecType, "OPT"):
		return strings.EqualFold(secType, "OPT") || strings.EqualFold(secType, rpc.SecTypeOption)
	case ibkrlib.IsBillOrBond(contract.SecType):
		return ibkrlib.IsBillOrBond(secType)
	default:
		return isStockLikeRiskSecType(secType)
	}
}

// competesWith is the quantity the working order holds against an exit of
// contract in direction action: its remaining quantity when it is the same
// contract in the same direction, or when it is a combo (BAG) on the
// option's underlying whatever its direction, because a working combo close
// touches every leg of its unit, so its units are a lower bound on each leg.
// A combo Canary placed names its legs in the journal and counts only
// against those; a hand combo, whose legs the broker does not report, counts
// against every option leg of the underlying. Zero otherwise.
func (w workingOrderIdentity) competesWith(contract rpc.ContractParams, action string) float64 {
	if strings.EqualFold(contract.SecType, "OPT") && strings.EqualFold(strings.TrimSpace(w.SecType), "BAG") &&
		strings.EqualFold(strings.TrimSpace(w.Symbol), strings.TrimSpace(contract.Symbol)) {
		if w.Legs != nil {
			if _, leg := w.Legs[contract.ConID]; !leg {
				return 0
			}
		}
		return w.Remaining
	}
	if !w.sameContract(contract) || !strings.EqualFold(strings.TrimSpace(w.Action), strings.TrimSpace(action)) {
		return 0
	}
	return w.Remaining
}

// bondSaleCandidate is a bill or bond sale that reduces or closes a held
// line. Its admission needs the open-order inventory: a sale may never sell
// more than the held face less every other working sale of the line.
func bondSaleCandidate(draft rpc.OrderDraft, position rpc.OrderPositionImpact) bool {
	return draft.StrategyGroup == nil && ibkrlib.IsBillOrBond(draft.Contract.SecType) &&
		strings.EqualFold(strings.TrimSpace(draft.Action), rpc.OrderActionSell) && draft.Quantity > 0 && isRiskReducing(position.Effect)
}

// brokerOrderIsTarget reports whether order is the replace target itself.
func brokerOrderIsTarget(order ibkrlib.OrderLifecycleEvent, target orderPreviewReplaceTarget) bool {
	if target.PermID > 0 && order.PermID > 0 {
		return target.PermID == order.PermID
	}
	return target.ReservedOrderID > 0 && order.OrderID == target.ReservedOrderID
}

// protectiveExitInventoryFromSnapshot reads one complete snapshot, with
// Canary's own journal rows, for the exemptions: the other working orders
// in the draft's direction on its contract, or on each leg of a strategy
// combo in that leg's direction, a working combo on an option's underlying
// counting against every leg. target is the zero value for a new placement.
//
// The snapshot is served from a cache for up to
// protectionOrderSnapshotRefreshEvery, and nothing refreshes it when Canary
// places an order, so an order Canary sent after the snapshot completed is
// not in it. An open journal row in scope that no snapshot row pairs and that
// moved after the snapshot counts too, so two exits of one line admitted
// within that window cannot both pass; a row the snapshot shows is paired,
// never counted twice.
func protectiveExitInventoryFromSnapshot(snapshot ibkrlib.OpenOrderSnapshot, journal journalInventory, scope brokerStateScope, draft rpc.OrderDraft, target orderPreviewReplaceTarget) protectiveExitInventory {
	inv := protectiveExitInventory{Current: true}
	views := journal.views
	working := brokerWorkingOrders(snapshot, views, scope)
	hasTarget := target.ReservedOrderID > 0 || target.PermID > 0
	since := snapshot.AsOf.Add(-journalInventoryMargin)
	var unseen []workingOrderIdentity
	for i := range views {
		view := &views[i]
		if !view.Open || !orderViewMatchesBrokerScope(*view, scope) || !view.UpdatedAt.After(since) {
			continue
		}
		if hasTarget && ((target.ReservedOrderID > 0 && view.ReservedOrderID == target.ReservedOrderID) || (target.PermID > 0 && view.PermID == target.PermID)) {
			continue
		}
		if !slices.ContainsFunc(working, func(row brokerWorkingOrder) bool { return row.Journal == view }) {
			w := journalOrderIdentity(*view)
			w.Legs = journal.legsOf(view)
			unseen = append(unseen, w)
		}
	}
	competing := func(contract rpc.ContractParams, action string) float64 {
		total := 0.0
		for _, row := range working {
			if hasTarget && brokerOrderIsTarget(row.Order, target) {
				continue
			}
			w := brokerOrderIdentity(row.Order)
			w.Legs = journal.legsOf(row.Journal)
			total += w.competesWith(contract, action)
		}
		for _, w := range unseen {
			total += w.competesWith(contract, action)
		}
		return total
	}
	if group := draft.StrategyGroup; group != nil {
		inv.OtherWorkingSameSideByLeg = make(map[int]float64, len(group.Legs))
		for _, leg := range group.Legs {
			inv.OtherWorkingSameSideByLeg[leg.Contract.ConID] = competing(leg.Contract, leg.Action)
		}
		return inv
	}
	inv.OtherWorkingSameSide = competing(draft.Contract, draft.Action)
	if hasTarget {
		for _, row := range working {
			order := row.Order
			if brokerOrderIsTarget(order, target) && strings.EqualFold(strings.TrimSpace(order.Action), rpc.OrderActionSell) && isProtectiveStopOrderType(order.OrderType) {
				if remaining := brokerOrderRemaining(order); remaining > 0 && float64(draft.Quantity) < remaining-1e-9 {
					inv.ReducesWorkingStop = true
				}
			}
		}
	}
	return inv
}

// journalOrderViewsForInventory reads Canary's own order rows for the
// competing-order count, with the legs of each combo it placed; false when
// the journal cannot be read. It folds the whole journal, as every other
// order read does (there is no bounded open-orders read model to serve it).
func (s *Server) journalOrderViewsForInventory() (journalInventory, bool) {
	if s == nil || s.orderJournal == nil {
		return journalInventory{}, false
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return journalInventory{}, false
	}
	j := journalInventory{views: buildOrderViews(events), legsByRef: map[string]map[int]struct{}{}, legsByOrderID: map[int]map[int]struct{}{}}
	for _, event := range events {
		if event.StrategyGroup == nil || len(event.StrategyGroup.Legs) == 0 {
			continue
		}
		legs := make(map[int]struct{}, len(event.StrategyGroup.Legs))
		for _, leg := range event.StrategyGroup.Legs {
			legs[leg.Contract.ConID] = struct{}{}
		}
		if event.OrderRef != "" {
			j.legsByRef[event.OrderRef] = legs
		}
		if event.ReservedOrderID > 0 {
			j.legsByOrderID[event.ReservedOrderID] = legs
		}
	}
	return j, true
}

// captureProtectiveExitInventory reads the complete broker open-order
// inventory (the cached protection snapshot, completed with Canary's own
// journal rows; no new broker request) for a protective-exit, bond-sale or
// delta-reducing-exit candidate. Only when the journal cannot be read is the
// inventory read from the broker afresh. A non-candidate, an unavailable
// inventory or one for another account/mode returns the unavailable value.
func (s *Server) captureProtectiveExitInventory(ctx context.Context, status rpc.TradingStatus, draft rpc.OrderDraft, position rpc.OrderPositionImpact, target orderPreviewReplaceTarget) protectiveExitInventory {
	if s == nil || ctx == nil || (!protectiveStockExitCandidate(draft, position) && !bondSaleCandidate(draft, position) && !deltaReductionCandidate(draft, position)) {
		return protectiveExitInventory{}
	}
	want := brokerStateScope{Account: status.Account, Mode: status.Mode}
	journal, journaled := s.journalOrderViewsForInventory()
	snapshot, scope, err := s.brokerOpenOrderInventory(ctx, !journaled)
	if err != nil || !sameBrokerScope(scope, want) {
		return protectiveExitInventory{}
	}
	return protectiveExitInventoryFromSnapshot(snapshot, journal, scope, draft, target)
}

// protectiveExitProposalBlocker is the readiness side of the exemption for a
// stock/ETF trailing-stop row: when its placement would be refused because
// the exemption cannot apply, the row carries one typed blocker in plain
// words instead of reading ready. A row the gates would pass anyway, or one
// that is not a long-stock sell stop, returns false.
func protectiveExitProposalBlocker(allowStockShort bool, p rpc.TradeProposal, inv protectiveExitInventory) (rpc.TradingBlocker, bool) {
	if !strings.EqualFold(strings.TrimSpace(p.Action), rpc.OrderActionSell) || !isProtectiveStopOrderType(p.OrderType) ||
		!positiveFinite(p.PositionQuantity) || p.Quantity <= 0 {
		return rpc.TradingBlocker{}, false
	}
	contract := p.Contract
	if strings.TrimSpace(contract.SecType) == "" {
		contract.SecType = positionWireSecType(p.SecType)
	}
	if !isStockLikeRiskSecType(contract.SecType) {
		return rpc.TradingBlocker{}, false
	}
	before := p.PositionQuantity
	after := before - float64(p.Quantity)
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: contract, Quantity: p.Quantity, OrderType: p.OrderType}
	position := rpc.OrderPositionImpact{Before: before, After: after, Effect: classifyPositionEffect(before, after)}
	if protectiveStockExitExempt(draft, position, inv) || allowStockShort {
		// With allow_stock_short the order cap in force still decides at
		// preview, from exact-session FX evidence this row does not carry.
		return rpc.TradingBlocker{}, false
	}
	switch {
	case float64(p.Quantity) > before+1e-9:
		return rpc.TradingBlocker{
			Code:    protectiveExitExceedsPositionCode,
			Message: fmt.Sprintf("this stop would sell %d shares but %.4g are held, so it would open a short position", p.Quantity, before),
			Action:  "Wait for the next proposal refresh to size the stop to the position.",
		}, true
	case !inv.Current:
		return rpc.TradingBlocker{
			Code:    protectiveExitInventoryUnavailableCode,
			Message: "Canary cannot read the broker's open orders right now, so it cannot rule out another sale of these shares; the stop waits until it can",
			Action:  "Wait for the broker connection to recover; the row becomes ready once the open orders can be read.",
		}, true
	default:
		return rpc.TradingBlocker{
			Code:    protectiveExitCompetingSellCode,
			Message: fmt.Sprintf("another working sell order already covers %.4g of the %.4g shares held, so this stop would sell more than you hold", inv.OtherWorkingSameSide, before),
			Action:  "Cancel or finish the other sell order first; the stop then becomes ready.",
		}, true
	}
}

// protectiveExitBook is one proposal refresh's read of the broker open-order
// inventory, taken at most once and only when a row needs it.
type protectiveExitBook struct {
	read     bool
	ok       bool
	snapshot ibkrlib.OpenOrderSnapshot
	scope    brokerStateScope
}

// protectiveExitRowBlocker is protectiveExitProposalBlocker for one generated
// stock/ETF trailing-stop row, reading the inventory lazily into book. A row
// already blocked, or a shadow row, reads nothing.
func (e *proposalEngine) protectiveExitRowBlocker(ctx context.Context, p rpc.TradeProposal, book *protectiveExitBook) (rpc.TradingBlocker, bool) {
	if e == nil || e.server == nil || book == nil || p.Shadow || p.State == rpc.TradeProposalStateBlocked {
		return rpc.TradingBlocker{}, false
	}
	limits := e.server.orderLimitsInForce("")
	allowStockShort := limits.Complete && limits.AllowStockShort
	if allowStockShort || !strings.EqualFold(strings.TrimSpace(p.Action), rpc.OrderActionSell) {
		return protectiveExitProposalBlocker(allowStockShort, p, protectiveExitInventory{})
	}
	if !book.read {
		book.read = true
		if ctx == nil {
			ctx = context.Background()
		}
		waitCtx, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
		snapshot, scope, err := e.server.brokerOpenOrderInventory(waitCtx, false)
		cancel()
		if err == nil && sameBrokerScope(scope, e.currentScope()) {
			book.ok, book.snapshot, book.scope = true, snapshot, scope
		}
	}
	inv := protectiveExitInventory{}
	if book.ok {
		contract := p.Contract
		if strings.TrimSpace(contract.SecType) == "" {
			contract.SecType = positionWireSecType(p.SecType)
		}
		inv = protectiveExitInventoryFromSnapshot(book.snapshot, journalInventory{}, book.scope, rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: contract, Quantity: p.Quantity, OrderType: p.OrderType}, orderPreviewReplaceTarget{})
	}
	return protectiveExitProposalBlocker(allowStockShort, p, inv)
}
