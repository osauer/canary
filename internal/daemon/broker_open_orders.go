package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// ordersOpenInventoryWait bounds how long orders.open waits for a broker
// open-order inventory refresh before it reports untracked orders
// unavailable. The refresh itself follows daemon lifetime and warms the
// shared cache for the next read.
const ordersOpenInventoryWait = 1500 * time.Millisecond

var errBrokerOpenOrderInventoryUnavailable = errors.New("complete current broker open-order inventory is unavailable")

// brokerOpenOrderInventory returns a complete, current all-client broker
// open-order snapshot for the connected session and the broker scope it
// belongs to. It reuses the Protection producer's single-flight cache unless
// fresh asks for a new reqAllOpenOrders round trip. An incomplete, stale or
// session-crossing snapshot is an error, never an empty inventory.
func (s *Server) brokerOpenOrderInventory(ctx context.Context, fresh bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
	if s == nil || ctx == nil {
		return ibkrlib.OpenOrderSnapshot{}, brokerStateScope{}, errBrokerOpenOrderInventoryUnavailable
	}
	if s.openOrderInventoryForTest != nil {
		return s.openOrderInventoryForTest(ctx, fresh)
	}
	binding := s.currentProtectionOrderSnapshotBinding()
	if binding.connector == nil || !brokerScopeConcrete(binding.scope) {
		return ibkrlib.OpenOrderSnapshot{}, binding.scope, fmt.Errorf("%w: no concrete connected account session", errBrokerOpenOrderInventoryUnavailable)
	}
	var snapshot ibkrlib.OpenOrderSnapshot
	var err error
	if fresh {
		snapshot, err = s.snapshotOpenOrdersFrom(ctx, binding.connector)
	} else {
		snapshot, err = s.protectionSnapshotOpenOrders(ctx, binding)
	}
	if err != nil {
		return ibkrlib.OpenOrderSnapshot{}, binding.scope, fmt.Errorf("%w: %v", errBrokerOpenOrderInventoryUnavailable, err)
	}
	if !protectionOrderSnapshotUsable(snapshot, s.orderNow()) {
		return ibkrlib.OpenOrderSnapshot{}, binding.scope, fmt.Errorf("%w: the snapshot is incomplete or stale", errBrokerOpenOrderInventoryUnavailable)
	}
	receipt := binding
	receipt.session = snapshot.Session
	receipt.generation = snapshot.Generation
	if s.orderSnapshotFn != nil && receipt.session == (ibkrlib.ConnectorSessionBinding{}) {
		// The in-process test seam has no socket token; production snapshots
		// always carry the exact session the Connector read them on.
		receipt.session = binding.session
	}
	if !s.protectionOrderSnapshotBindingCurrent(receipt) {
		return ibkrlib.OpenOrderSnapshot{}, binding.scope, fmt.Errorf("%w: the broker order session changed during the read", errBrokerOpenOrderInventoryUnavailable)
	}
	return snapshot, binding.scope, nil
}

// brokerWorkingOrder is one order a complete broker open-order snapshot shows
// working in a scope, paired with the journal row that tracks it.
type brokerWorkingOrder struct {
	Order ibkrlib.OrderLifecycleEvent
	// Journal is the journal row for the order; nil when the journal does
	// not track it (placed by hand in TWS or by another API client).
	Journal *rpc.OrderView
}

// brokerWorkingOrders lists the snapshot orders still working in scope,
// each matched to its journal row by the PermID-first identity rule the
// reconciliation sweep uses. What-if, terminal and fully filled orders are
// not working; an order that names another account is outside scope.
func brokerWorkingOrders(snapshot ibkrlib.OpenOrderSnapshot, views []rpc.OrderView, scope brokerStateScope) []brokerWorkingOrder {
	var out []brokerWorkingOrder
	for _, order := range snapshot.Orders {
		if !brokerOrderWorking(order) {
			continue
		}
		if account := strings.TrimSpace(order.Account); account != "" && !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
			continue
		}
		row := brokerWorkingOrder{Order: order}
		for i := range views {
			if orderViewMatchesBrokerScope(views[i], scope) && openOrderSnapshotEventMatches(order, views[i]) {
				row.Journal = &views[i]
				break
			}
		}
		out = append(out, row)
	}
	return out
}

// brokerOrderWorking reports whether an open-order snapshot entry is still
// working at the broker. Unknown broker state counts as working: the
// callers use it to prove an absence, so doubt never reads as settled.
func brokerOrderWorking(order ibkrlib.OrderLifecycleEvent) bool {
	if order.Type != ibkrlib.OrderLifecycleEventOpenOrder || order.WhatIf {
		return false
	}
	if order.Remaining <= 0 && order.TotalQuantity > 0 && order.TotalQuantity-order.Filled <= 1e-9 {
		return false
	}
	return !orderLifecycleStatusIsTerminal(mapBrokerOrderLifecycleStatus(order.Status, order.Filled, order.Remaining))
}

// untrackedBrokerOrders lists the orders working at the broker in scope that
// the journal does not track. It reads the complete all-client inventory the
// Protection producer keeps current, waiting at most ordersOpenInventoryWait
// for a refresh, and reports unavailable, never an empty list, when no
// complete current inventory exists for scope.
func (s *Server) untrackedBrokerOrders(ctx context.Context, views []rpc.OrderView, scope brokerStateScope) ([]rpc.OrderView, string, time.Time) {
	if ctx == nil || !brokerScopeConcrete(scope) {
		return nil, rpc.OrdersUntrackedUnavailable, time.Time{}
	}
	waitCtx, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
	defer cancel()
	snapshot, inventoryScope, err := s.brokerOpenOrderInventory(waitCtx, false)
	if err != nil || !sameBrokerScope(inventoryScope, scope) {
		return nil, rpc.OrdersUntrackedUnavailable, time.Time{}
	}
	asOf := snapshot.AsOf.UTC()
	var untracked []rpc.OrderView
	for _, row := range brokerWorkingOrders(snapshot, views, scope) {
		if row.Journal == nil {
			untracked = append(untracked, untrackedOrderView(row.Order, scope, asOf))
		}
	}
	slices.SortStableFunc(untracked, func(a, b rpc.OrderView) int {
		if c := cmp.Compare(a.PermID, b.PermID); c != 0 {
			return c
		}
		return cmp.Compare(a.ReservedOrderID, b.ReservedOrderID)
	})
	return untracked, rpc.OrdersUntrackedCurrent, asOf
}

// untrackedOrderView projects a broker-working order the journal does not
// track onto the read row shape. It carries typed identity, contract and
// working state only: broker free text (the order reference a human typed,
// why-held, messages) stays out, the row has no origin, and nothing marks it
// modify- or cancel-eligible, because the daemon never tracked it.
func untrackedOrderView(order ibkrlib.OrderLifecycleEvent, scope brokerStateScope, asOf time.Time) rpc.OrderView {
	view := rpc.OrderView{
		ReservedOrderID: order.OrderID, PermID: order.PermID,
		Account: scope.Account, Mode: scope.Mode,
		Symbol: order.Symbol, SecType: order.SecType, ConID: order.ConID,
		Exchange: order.Exchange, Currency: order.Currency, LocalSymbol: order.LocalSymbol,
		TradingClass: order.TradingClass, Expiry: order.Expiry, Strike: order.Strike,
		Right: order.Right, Multiplier: order.Multiplier,
		Action: order.Action, OrderType: order.OrderType, TIF: order.TIF,
		TriggerMethod: order.TriggerMethod, OutsideRTH: order.OutsideRth,
		Quantity: order.TotalQuantity, LimitPrice: order.LimitPrice, Trail: trailSpecFromLifecycle(order),
		Status: order.Status, Filled: order.Filled, Remaining: order.Remaining,
		LifecycleStatus: mapBrokerOrderLifecycleStatus(order.Status, order.Filled, order.Remaining),
		UpdatedAt:       asOf, Open: true,
	}
	if order.ClientIDPresent {
		view.ClientID = order.ClientID
	}
	return view
}
