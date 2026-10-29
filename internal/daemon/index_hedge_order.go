package daemon

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/mandate"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The index hedge's order path (owner decisions 2026-10-28 13:17 and 20:07
// CET; internal-docs/design/index-hedge.md). A future reaches the broker in
// two shapes only, both through the one gated, device-confirmed path every
// order takes (preview, WhatIf, signed token, admission, wire guard):
//
//   - the hedge: a SELL of the Micro E-mini S&P 500 future the reduce plan
//     sized, previewed by the daemon with hedgeOrderSource, a limit at the
//     bid, good for the day, in US regular hours, with no stop order; it
//     may leave the contract at most [mandate.index_hedge] max_contracts
//     short, and is refused while that table is not written;
//   - its close: a BUY that closes or shrinks a held short of that future,
//     a percent reduce of the held line (`proposals reduce --con-id ID
//     --include-hedges`), priced as every reduce is.
//
// Every other future order is refused. What bounds cost and risk once a
// hedge sale passes the order cap (whose face-value notional a single Micro
// contract can exceed): max_contracts on the contract's position after the
// order, read at preview, admission and the wire guard; at submit the whole
// book's short Micro S&P contracts with Canary's working hedge sales; the
// size to neutral on the kept mandate book; a limit at the bid, never at
// market; DAY in regular hours only; the broker WhatIf; the owner's device
// confirmation of the exact terms; and the mandate gate, which judges any
// hedge that does not lower the fall's loss within the loss room as an
// addition. Its worst case is max_contracts sold short at the bid: a 10%
// rise then costs 10% of that face value.

// hedgeOrderSource tags a hedge sale's draft; the token path and the place
// boundary read it to apply the hedge's own checks.
const hedgeOrderSource = "index_hedge"

// isIndexFutureContract reports a Micro E-mini S&P 500 future in USD, the
// one future Canary trades.
func isIndexFutureContract(c rpc.ContractParams) bool {
	return strings.EqualFold(strings.TrimSpace(c.SecType), "FUT") && normSym(c.Symbol) == hedgeFutureSymbol && normCcy(c.Currency) == "USD"
}

// normalizePreviewFutureContract holds a future preview to an exact listed
// contract: its ConID, month, exchange and multiplier.
func normalizePreviewFutureContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	out := rpc.ContractParams{ConID: in.ConID, Symbol: normSym(in.Symbol), SecType: "FUT", Exchange: strings.ToUpper(strings.TrimSpace(in.Exchange)),
		Currency: normCcy(in.Currency), Expiry: strings.TrimSpace(in.Expiry), LocalSymbol: strings.TrimSpace(in.LocalSymbol),
		TradingClass: strings.TrimSpace(in.TradingClass), Multiplier: in.Multiplier}
	switch {
	case out.ConID <= 0 || out.Expiry == "" || out.Exchange == "" || out.Exchange == "SMART":
		return rpc.ContractParams{}, errBadRequest("a future preview names its exact contract: con_id, expiry and its exchange")
	case out.Multiplier <= 0:
		return rpc.ContractParams{}, errBadRequest("a future preview carries the contract's multiplier")
	case !isIndexFutureContract(out):
		return rpc.ContractParams{}, errBadRequest("the only future Canary trades is the Micro E-mini S&P 500 (MES, USD) index hedge")
	case in.Right != "" || in.Strike != 0:
		return rpc.ContractParams{}, errBadRequest("a future preview carries no right or strike")
	}
	return out, nil
}

// indexFutureSessionRefusal refuses a future order outside the US regular
// session (owner decision 2026-10-28 20:07 CET: the hedge is placed in US
// hours), and when the calendar cannot say: the futures trade nearly around
// the clock, so no quote would refuse it later.
func (s *Server) indexFutureSessionRefusal(now time.Time) error {
	session, ok := s.previewSession(marketcal.MarketUSEquity, now)
	message := ""
	switch {
	case !ok || session.State == marketcal.StateUnknown:
		message = "the index hedge trades in US regular hours only, and the official calendar cannot confirm the session now"
	case !session.IsOpen:
		message = "the index hedge trades in US regular hours only: " + sessionClosedPhrase(session)
	default:
		return nil
	}
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{Code: "outside_regular_hours", Message: message,
		Action: "Preview the hedge again in US regular hours; nothing was sent."})
}

// indexFuturePreviewRefusal holds a future preview to the two shapes the
// path admits. The order gate (indexFutureOrderRule) holds the sizes.
func indexFuturePreviewRefusal(p rpc.OrderPreviewParams, scope, action, orderType, tif string, position rpc.OrderPositionImpact) error {
	allowance := p.Allowance != nil && strings.EqualFold(strings.TrimSpace(p.Strategy), rpc.OrderStrategyAllowanceLimit)
	switch {
	case scope != rpc.OrderTokenScopePlace:
		return errBadRequest("a future order is placed new; it is never modified")
	case orderType != rpc.OrderTypeLMT || tif != rpc.OrderTIFDay || p.OutsideRTH || p.Trail != nil || p.AttachedStop != nil || p.AdaptivePriority != "":
		return errBadRequest("a future order is a plain limit, good for the day, in regular hours, with no stop")
	case !allowance || p.LimitPrice != nil:
		return errBadRequest("a future order is priced by the daemon from the touch; it takes no limit price")
	}
	switch strings.TrimSpace(p.Source) {
	case hedgeOrderSource:
		if action != rpc.OrderActionSell || p.Allowance.Pct != 0 {
			return errBadRequest("the index hedge sells at the bid")
		}
	case reduceOrderSource:
		if action != rpc.OrderActionBuy || !(position.Before < 0) {
			return errBadRequest("a future reduce only buys back a held short hedge")
		}
	default:
		return errBadRequest("a future is traded only as the index hedge (`proposals reduce --portfolio --mode hedge`) or its close (`proposals reduce --con-id ID --include-hedges`)")
	}
	return nil
}

// indexFutureOrderRule is the order gate's rule for a future, read at
// preview, admission and the wire guard with the position read there: a
// hedge sale may leave the contract at most max_contracts short and needs
// the table; a buy only closes or shrinks a held short. A future that
// passes is exempt from the notional cap (index_hedge_order.go's header
// says what bounds it instead).
func indexFutureOrderRule(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, position rpc.OrderPositionImpact) error {
	if !isIndexFutureContract(draft.Contract) || draft.StrategyGroup != nil {
		return fmt.Errorf("the only future Canary trades is the Micro E-mini S&P 500 index hedge")
	}
	qty := float64(draft.Quantity)
	switch strings.ToUpper(strings.TrimSpace(draft.Action)) {
	case rpc.OrderActionSell:
		switch {
		case draft.Source != hedgeOrderSource:
			return fmt.Errorf("a future sale is placed only as the index hedge")
		case limits.IndexHedgeRefusal != "" || limits.IndexHedgeMaxContracts < 1:
			return fmt.Errorf("%s", nonEmptyString(limits.IndexHedgeRefusal, "placing an index hedge is refused until [mandate.index_hedge] max_contracts is written"))
		case position.Before > 0 || math.Abs(position.After-(position.Before-qty)) > 1e-9:
			return fmt.Errorf("the index hedge sells from no position or a held short, never from a long future")
		case -position.After > float64(limits.IndexHedgeMaxContracts)+1e-9:
			return fmt.Errorf("the hedge would leave %s short %g contracts, above [mandate.index_hedge] max_contracts %d", draft.Contract.LocalSymbol, -position.After, limits.IndexHedgeMaxContracts)
		}
	case rpc.OrderActionBuy:
		if !(position.Before < 0) || position.After > 1e-9 || math.Abs(position.After-(position.Before+qty)) > 1e-9 {
			return fmt.Errorf("a future buy only closes or shrinks a held short hedge")
		}
	default:
		return fmt.Errorf("action must be buy or sell")
	}
	return nil
}

// heldIndexHedge reports a held short of the index future, the line a
// single reduce may buy back.
func heldIndexHedge(row rpc.PositionView) bool {
	return positionIsIndexFuture(row) && row.Quantity < 0
}

// reduceSingleEligible is reduceEligible plus the held hedge: a percent
// reduce of one line may buy the hedge back; a portfolio sweep never
// selects it (closing a hedge is no risk cut: TM-05.18).
func reduceSingleEligible(row rpc.PositionView) bool {
	return reduceEligible(row) || heldIndexHedge(row)
}

// indexHedgeShortHeld is the whole book's short Micro S&P contracts, every
// month together.
func indexHedgeShortHeld(pos *rpc.PositionsResult) float64 {
	if pos == nil {
		return 0
	}
	short := 0.0
	for _, row := range pos.Stocks {
		if heldIndexHedge(row) {
			short -= row.Quantity
		}
	}
	return short
}

// indexHedgeCapRefusal is the submit's whole-book check: the book's short
// Micro S&P contracts, all-client working sales across months and the
// reviewed sale together stay within max_contracts. Admission additionally
// binds the complete positions and commitment evidence through the wire.
func (s *Server) indexHedgeCapRefusal(draft rpc.OrderDraft, heldShort float64) []rpc.TradingBlocker {
	if b := s.brokerPermissionBlocker(draft.Contract); b != nil && draft.Action == rpc.OrderActionSell {
		return []rpc.TradingBlocker{*b}
	}
	var mandateTable *risk.ConstitutionMandate
	if c := s.riskPolicies.snapshot().policy; c != nil {
		mandateTable = c.Mandate
	}
	maxContracts, why := mandateTable.IndexHedgeMaxContracts()
	if why != "" {
		return []rpc.TradingBlocker{{Code: "index_hedge_unplaceable", Message: why, Action: "Write [mandate.index_hedge] max_contracts with a higher policy_version to place a hedge; nothing was sent."}}
	}
	pending, _, _, err := s.indexHedgeCommitments(context.Background(), draft)
	if err != nil {
		return []rpc.TradingBlocker{{Code: "index_hedge_unmeasured", Message: err.Error(), Action: "Refresh orders and preview the hedge again; nothing was sent."}}
	}
	committed := 0.0
	for _, leg := range pending {
		committed -= leg.Quantity
	}
	if total := heldShort + committed + float64(draft.Quantity); total > float64(maxContracts)+1e-9 {
		return []rpc.TradingBlocker{{Code: "index_hedge_over_max", Message: fmt.Sprintf("the book would hold %g Micro S&P contracts short (%g held, %g working sales, %d reviewed), above [mandate.index_hedge] max_contracts %d", total, heldShort, committed, draft.Quantity, maxContracts), Action: "Preview the hedge again for the current book; nothing was sent."}}
	}
	return nil
}

// indexHedgeAuthority binds all months and all clients to one admission.
// The request's broker-write mutex serializes local intents; the lifecycle
// frontier invalidates broker changes. The wire guard only reads this proof.
type indexHedgeAuthority struct {
	heldShort float64
	pending   []mandate.Leg
	orders    protectionOrderSnapshotBinding
}

// indexHedgeCommitments joins the shared all-client snapshot to durable intents.
// Unacknowledged/uncertain writes retain their capacity; a matched order counts
// once. Pending buys never release capacity for a new short.
func (s *Server) indexHedgeCommitments(ctx context.Context, draft rpc.OrderDraft) ([]mandate.Leg, map[int]rpc.ContractParams, protectionOrderSnapshotBinding, error) {
	before := s.reduceFillBinding()
	snapshot, scope, err := s.brokerOpenOrderInventory(ctx, false)
	if err != nil || !sameBrokerScope(scope, before.scope) || !protectionOrderSnapshotUsable(snapshot, s.orderNow()) {
		return nil, nil, before, fmt.Errorf("complete current hedge order inventory is unavailable")
	}
	var journal journalInventory
	loaded := false
	load := func() bool { var ok bool; journal, ok = s.journalOrderViewsForInventory(); return ok }
	if before.connector != nil {
		evidence, ok := before.connector.CaptureBrokerEvidence()
		if ok && evidence.Session == before.session && evidence.OrderLifecycleGeneration == before.generation {
			loaded = before.connector.WithStableBrokerEvidence(evidence, load)
		}
	} else if s.hasOrderPreviewBrokerTestSeam() {
		loaded = load()
	}
	if !loaded {
		return nil, nil, before, fmt.Errorf("durable hedge commitments changed or are unavailable")
	}

	var pending []mandate.Leg
	filled := map[int]rpc.ContractParams{}
	addContract := func(c rpc.ContractParams, action string, remaining, fill, price float64) error {
		if strings.TrimSpace(c.SecType) == "" && normSym(c.Symbol) == "MES" {
			return fmt.Errorf("a Micro S&P commitment has unknown security type")
		}
		if !strings.EqualFold(c.SecType, "FUT") {
			return nil
		}
		if normSym(c.Symbol) != "MES" {
			if normSym(c.Symbol) == "" {
				return fmt.Errorf("a future commitment has unknown identity")
			}
			return nil
		}
		if c.ConID <= 0 || !isIndexFutureContract(c) || c.Multiplier <= 0 {
			return fmt.Errorf("a Micro S&P commitment has incomplete contract identity")
		}
		if math.IsNaN(fill) || math.IsInf(fill, 0) || fill < 0 {
			return fmt.Errorf("a Micro S&P commitment has unknown fill quantity")
		}
		if fill > 0 {
			filled[c.ConID] = c
		}
		if remaining != 0 && !strings.EqualFold(action, rpc.OrderActionSell) && !strings.EqualFold(action, rpc.OrderActionBuy) {
			return fmt.Errorf("a Micro S&P commitment has unknown direction")
		}
		if !strings.EqualFold(action, rpc.OrderActionSell) || remaining == 0 {
			return nil
		}
		if !positiveFinite(remaining) || !positiveFinite(price) {
			return fmt.Errorf("a Micro S&P sale has unmeasured remaining quantity or price")
		}
		pending = append(pending, mandate.Leg{ID: mandateConLegID(c.ConID), Symbol: c.LocalSymbol, Underlying: mandate.MicroSP500Future, Kind: mandate.KindStock, Quantity: -remaining, Multiplier: float64(c.Multiplier), Currency: normCcy(c.Currency), Spot: price})
		return nil
	}
	add := func(view rpc.OrderView, remaining float64, legs map[int]rpc.StrategyOrderLeg) error {
		if view.OrderRef != "" && view.OrderRef == draft.OrderRef {
			return nil
		}
		relevant := strings.EqualFold(view.SecType, "BAG") || normSym(view.Symbol) == "MES" && (strings.EqualFold(view.SecType, "FUT") || strings.TrimSpace(view.SecType) == "")
		if relevant && strings.EqualFold(view.Status, "Filled") && !(view.Filled > 0) {
			return fmt.Errorf("a completed hedge commitment has unknown fill quantity")
		}
		if relevant && view.Open && !positiveFinite(remaining) && !(view.Quantity > 0 && view.Filled >= view.Quantity) {
			return fmt.Errorf("a working hedge commitment has unknown remaining quantity")
		}
		if strings.EqualFold(view.SecType, "BAG") {
			if len(legs) == 0 {
				return fmt.Errorf("a combination has unknown hedge commitments")
			}
			for _, leg := range legs {
				if strings.TrimSpace(leg.Contract.SecType) == "" {
					return fmt.Errorf("a combination has unknown leg identity")
				}
				if !strings.EqualFold(leg.Contract.SecType, "FUT") {
					continue
				}
				// A combo's net limit does not establish a future leg's
				// valuation. Refuse instead of inferring the leg's direction
				// from the BAG parent action or signed ratio.
				if remaining > 0 && (normSym(leg.Contract.Symbol) == "MES" || normSym(leg.Contract.Symbol) == "") {
					return fmt.Errorf("a working future combination has unmeasured hedge commitments")
				}
				if err := addContract(leg.Contract, leg.Action, 0, view.Filled, 0); err != nil {
					return err
				}
			}
			return nil
		}
		c := rpc.ContractParams{ConID: view.ConID, Symbol: view.Symbol, SecType: view.SecType, Currency: view.Currency, Multiplier: view.Multiplier, Exchange: view.Exchange, Expiry: view.Expiry, LocalSymbol: view.LocalSymbol, TradingClass: view.TradingClass}
		price := view.LimitPrice
		if view.OrderType != rpc.OrderTypeLMT && view.OrderType != "STP LMT" {
			price = 0
		}
		return addContract(c, view.Action, remaining, view.Filled, price)
	}
	// Broker order IDs are client-local. Only authenticated journal
	// lineage can supply a BAG's typed legs; an ID-only fallback could
	// borrow another client's combo and incorrectly prove it harmless.
	legsOf := func(view *rpc.OrderView) map[int]rpc.StrategyOrderLeg {
		if view == nil || view.OrderRef == "" || view.PreviewTokenID == "" {
			return nil
		}
		return journal.legsByRef[view.OrderRef]
	}
	seen := map[int]bool{}
	for _, row := range brokerWorkingOrders(snapshot, journal.views, scope) {
		view := untrackedOrderView(row.Order, scope, snapshot.AsOf)
		if row.Journal != nil {
			view.OrderRef = row.Journal.OrderRef
			view.Filled = max(view.Filled, row.Journal.Filled)
			if (strings.EqualFold(view.SecType, "FUT") || strings.EqualFold(view.SecType, "BAG")) && (view.Quantity != row.Journal.Quantity || view.Action != row.Journal.Action || view.LimitPrice != row.Journal.LimitPrice) {
				return nil, nil, before, fmt.Errorf("a modified future commitment is not reconciled")
			}
		}
		remaining := brokerOrderRemaining(row.Order)
		if view.Filled > 0 && view.Quantity > 0 {
			remaining = min(remaining, max(0, view.Quantity-view.Filled))
		}
		if err := add(view, remaining, legsOf(row.Journal)); err != nil {
			return nil, nil, before, err
		}
		for i := range journal.views {
			if row.Journal == &journal.views[i] {
				seen[i] = true
			}
		}
	}
	// Retained terminal broker rows can carry a fill not yet in positions.
	for _, row := range snapshot.Orders {
		if row.Filled <= 0 && !strings.EqualFold(row.Status, "Filled") || row.WhatIf || brokerOrderWorking(row) || (row.Account != "" && !strings.EqualFold(row.Account, scope.Account)) {
			continue
		}
		view := untrackedOrderView(row, scope, snapshot.AsOf)
		view.Open = false
		var matched *rpc.OrderView
		for i := range journal.views {
			if orderViewMatchesBrokerScope(journal.views[i], scope) && openOrderSnapshotEventMatches(row, journal.views[i]) {
				matched = &journal.views[i]
				break
			}
		}
		if err := add(view, 0, legsOf(matched)); err != nil {
			return nil, nil, before, err
		}
	}
	for i, view := range journal.views {
		if seen[i] || !orderViewMatchesBrokerScope(view, scope) || view.ReservedOrderID <= 0 && view.PermID <= 0 {
			continue
		}
		remaining := 0.0
		if view.Open {
			remaining = orderViewRemainingQuantity(view)
		} else if view.Filled <= 0 && !strings.EqualFold(view.Status, "Filled") {
			continue
		}
		if err := add(view, remaining, legsOf(&view)); err != nil {
			return nil, nil, before, err
		}
	}
	if !s.reduceFillBindingCurrent(before) {
		return nil, nil, before, fmt.Errorf("hedge order evidence changed during admission")
	}
	return pending, filled, before, nil
}

func (s *Server) prepareIndexHedgeAuthority(ctx context.Context, binding brokerWriteTransactionBinding, draft rpc.OrderDraft, generation uint64) (*indexHedgeAuthority, error) {
	pending, filled, orders, err := s.indexHedgeCommitments(ctx, draft)
	if err != nil {
		return nil, err
	}
	held := map[int]reduceHeldPosition{}
	book := map[int]reduceHeldPosition{}
	addHeld := func(c rpc.ContractParams, q float64) error {
		if strings.EqualFold(strings.TrimSpace(c.SecType), "CASH") {
			return nil
		}
		c.SecType = positionWireSecType(c.SecType)
		// These classes are explicitly outside the equity mandate model;
		// broker bond face units must not be compared to RPC face amounts.
		if c.SecType == "BOND" || c.SecType == "BILL" || c.SecType == "CASH" {
			return nil
		}
		if c.ConID <= 0 || math.IsNaN(q) || math.IsInf(q, 0) {
			return fmt.Errorf("the hedge's portfolio quantity identity is unknown")
		}
		if _, ok := book[c.ConID]; ok {
			return fmt.Errorf("the hedge's portfolio contract is duplicated")
		}
		book[c.ConID] = reduceHeldPosition{c, q}
		if !strings.EqualFold(c.SecType, "FUT") || normSym(c.Symbol) != "MES" {
			return nil
		}

		if c.ConID <= 0 || !isIndexFutureContract(c) || c.Multiplier <= 0 || math.IsNaN(q) || math.IsInf(q, 0) {
			return fmt.Errorf("held Micro S&P identity or quantity is unknown")
		}
		if _, ok := held[c.ConID]; ok {
			return fmt.Errorf("held Micro S&P contract is duplicated")
		}
		held[c.ConID] = reduceHeldPosition{c, q}
		return nil
	}
	if binding.testOnly && s.reducePositionsForTest != nil {
		pos, err := s.reducePositionsForTest(ctx)
		if err != nil || pos == nil {
			return nil, fmt.Errorf("whole-book hedge positions unavailable")
		}
		for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
			for _, row := range rows {
				if err := addHeld(proposalContractFromPosition(row, row.SecType), row.Quantity); err != nil {
					return nil, err
				}
			}
		}
	} else {
		if binding.connector == nil {
			return nil, brokerWriteTransactionDriftError()
		}
		projection, ok := binding.connector.CapturePortfolioProjectionForSession(binding.session)
		if !ok || projection.Generation != generation {
			return nil, brokerWriteTransactionDriftError()
		}
		if err := confirmedPositionProjection(binding.scope, projection, s.orderNow()); err != nil {
			return nil, err
		}
		for _, row := range projection.Positions {
			if row == nil {
				continue
			}
			c := row.Contract
			if err := addHeld(rpc.ContractParams{ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Currency: c.Currency, Multiplier: c.Multiplier, Exchange: c.Exchange, Expiry: c.Expiry, LocalSymbol: c.LocalSymbol, TradingClass: c.TradingClass}, row.Position); err != nil {
				return nil, err
			}
		}
	}
	out := &indexHedgeAuthority{pending: pending, orders: orders}
	for _, row := range held {
		if row.quantity < 0 {
			out.heldShort -= row.quantity
		}
	}
	for id, c := range filled {
		if _, ok := held[id]; !ok {
			held[id] = reduceHeldPosition{c, 0}
		}
	}
	input, _, why := s.mandateCurrentInput()
	if input == nil {
		return nil, fmt.Errorf("the hedge's mandate book is unavailable: %s", why)
	}
	modeled := map[int]float64{}
	for _, leg := range input.Legs {
		id, ok := strings.CutPrefix(leg.ID, "con:")
		conID, err := strconv.Atoi(id)
		if !ok || err != nil || conID <= 0 {
			return nil, fmt.Errorf("a modeled hedge-book line has no exact contract identity")
		}
		modeled[conID] += leg.Quantity
	}
	for id, q := range modeled {
		if math.Abs(book[id].quantity-q) > 1e-9 {
			return nil, fmt.Errorf("the hedge's modeled holdings differ from the current portfolio")
		}
	}
	receipt, err := s.orderFillPositionsReceipt(ctx, orders, 0)
	if err != nil {
		return nil, err
	}
	received := map[int]float64{}
	for _, row := range receipt {
		if row == nil {
			continue
		}
		if strings.TrimSpace(row.Account) == "" && row.Position != 0 {
			return nil, fmt.Errorf("the hedge's positions receipt has unknown account scope")
		}
		if !strings.EqualFold(row.Account, orders.scope.Account) {
			continue
		}
		c := row.Contract
		if strings.EqualFold(strings.TrimSpace(c.SecType), "CASH") {
			continue
		}
		sec := positionWireSecType(c.SecType)
		if sec == "BOND" || sec == "BILL" || sec == "CASH" {
			continue
		}
		if c.ConID <= 0 || math.IsNaN(row.Position) || math.IsInf(row.Position, 0) {
			return nil, fmt.Errorf("the hedge's broker positions receipt has unknown identity or quantity")
		}
		if _, ok := received[c.ConID]; ok {
			return nil, fmt.Errorf("the hedge's broker positions receipt has duplicate contracts")
		}
		if prior, ok := book[c.ConID]; ok && sec != "" && !riskSecTypeConsistent(prior.contract.SecType, sec) {
			return nil, fmt.Errorf("the hedge's broker positions receipt has conflicting identity")
		}
		received[c.ConID] = row.Position
		if math.Abs(book[c.ConID].quantity-row.Position) > 1e-9 {
			return nil, fmt.Errorf("the hedge's held book has not caught up with broker fills; refresh positions")
		}
	}
	for id, row := range book {
		if math.Abs(received[id]-row.quantity) > 1e-9 {
			return nil, fmt.Errorf("the hedge's held book has not caught up with broker fills; refresh positions")
		}
	}
	for id := range filled {
		if math.Abs(received[id]-held[id].quantity) > 1e-9 {
			return nil, fmt.Errorf("the hedge's held quantity has not caught up with broker fills; refresh positions")
		}
	}

	if err := s.validateIndexHedgeAuthority(out, draft, s.orderLimitsInForce(binding.riskBaseCurrency)); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) validateIndexHedgeAuthority(a *indexHedgeAuthority, draft rpc.OrderDraft, limits risk.OrderLimitsInForce) error {
	if a == nil || !s.reduceFillBindingCurrent(a.orders) {
		return fmt.Errorf("%w: hedge order evidence changed; preview again", ErrTradingDisabled)
	}
	if limits.IndexHedgeRefusal != "" || limits.IndexHedgeMaxContracts < 1 {
		return fmt.Errorf("%w: hedge permission unavailable", ErrTradingDisabled)
	}
	total := a.heldShort + float64(draft.Quantity)
	for _, leg := range a.pending {
		total -= leg.Quantity
	}
	if total > float64(limits.IndexHedgeMaxContracts)+1e-9 {
		return fmt.Errorf("%w: whole-book Micro S&P short commitments %g exceed max_contracts %d", ErrTradingDisabled, total, limits.IndexHedgeMaxContracts)
	}
	input, _, why := s.mandateCurrentInput()
	if input == nil {
		return fmt.Errorf("%w: hedge book is unmeasured: %s", ErrTradingDisabled, why)
	}
	if blocker := indexHedgeProjectedRiskRefusal(input, draft, a.pending); blocker != nil {
		return tradingBlockersError([]rpc.TradingBlocker{*blocker})
	}
	return nil
}
