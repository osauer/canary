package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// An Add's evidence is current while what it sized from still holds, not
// while nothing anywhere moved. In market hours the account summary ticks
// with every mark and every client's order frames advance the broker's
// order generation, so comparing those whole refused Adds while nothing the
// Add used had changed.

// stockAddAccountMark is what an Add sizes from in the account summary: net
// liquidation and excess liquidity in base, and each currency's cash and
// settled cash.
type stockAddAccountMark struct {
	NetLiquidation, ExcessLiquidity float64
	Cash                            map[string]float64
}

// stockAddAccountTolerance is how far a figure the Add sized from may move
// before the evidence is taken again: half a percent of net liquidation,
// never less than 250 in base. Marks move net liquidation and excess
// liquidity continuously; a fill or a transfer moves cash by more.
func stockAddAccountTolerance(nlv float64) float64 {
	return max(0.005*math.Abs(nlv), 250)
}

// stockAddAccountMarkFrom reads the mark from the raw account summary; ok is
// false without net liquidation and excess liquidity.
func stockAddAccountMarkFrom(raw map[string]string) (stockAddAccountMark, bool) {
	m := stockAddAccountMark{Cash: map[string]float64{}}
	var nlv, excess bool
	for key, value := range raw {
		if _, after, found := strings.Cut(key, ":"); found {
			key = after
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v == math.MaxFloat64 {
			continue
		}
		// The summary keys a figure by itself or by its currency
		// ("NetLiquidation", "NetLiquidation_EUR"); account-wide figures
		// come in the base currency only.
		field, ccy, _ := strings.Cut(key, "_")
		switch field {
		case "NetLiquidation":
			m.NetLiquidation, nlv = v, true
		case "ExcessLiquidity":
			m.ExcessLiquidity, excess = v, true
		case "CashBalance", "SettledCash":
			if ccy != "" && ccy != "BASE" {
				m.Cash[field+"_"+ccy] = v
			}
		}
	}
	return m, nlv && excess
}

// stockAddAccountMarkHolds reports whether now still carries what was
// sized from: the same cash rows, and every figure within the tolerance.
func stockAddAccountMarkHolds(was, now stockAddAccountMark) bool {
	tol := stockAddAccountTolerance(was.NetLiquidation)
	if math.Abs(now.NetLiquidation-was.NetLiquidation) > tol || math.Abs(now.ExcessLiquidity-was.ExcessLiquidity) > tol || len(now.Cash) != len(was.Cash) {
		return false
	}
	for k, v := range was.Cash {
		n, ok := now.Cash[k]
		if !ok || math.Abs(n-v) > tol {
			return false
		}
	}
	return true
}

// stockAddAccountMarkCurrent reports whether the broker's account summary
// still carries what an Add sized from.
func stockAddAccountMarkCurrent(broker *orderPreviewBrokerAuthority, was stockAddAccountMark) bool {
	var now stockAddAccountMark
	ok := false
	if broker != nil && broker.connector != nil {
		now, ok = stockAddAccountMarkFrom(broker.connector.AccountSummaryRaw())
	}
	if !ok {
		// No summary now: current only if the Add sized from none either
		// (a session without a broker summary sizes nothing from it).
		return was.Cash == nil && was.NetLiquidation == 0 && was.ExcessLiquidity == 0
	}
	return stockAddAccountMarkHolds(was, now)
}

// stockAddOrderMark names the working orders that can matter to an Add of
// conID: every buy (a buy spends cash the Add sizes from) and every order
// on this contract. Another client's sell of another contract changes
// nothing the Add reads.
func stockAddOrderMark(orders []ibkrlib.OrderLifecycleEvent, conID int, scope brokerStateScope) string {
	var marks []string
	for _, o := range brokerWorkingOrders(ibkrlib.OpenOrderSnapshot{Complete: true, Orders: orders}, nil, scope) {
		ev := o.Order
		if !strings.EqualFold(ev.Action, rpc.OrderActionBuy) && ev.ConID != conID {
			continue
		}
		marks = append(marks, fmt.Sprintf("%d|%d|%d|%s|%s|%g|%g|%g|%s", ev.PermID, ev.ClientID, ev.OrderID, strings.ToUpper(ev.Action), ev.Symbol,
			ev.TotalQuantity, optionExitSnapshotRemaining(ev), ev.LimitPrice, strings.ToUpper(ev.Currency)))
	}
	slices.Sort(marks)
	return strings.Join(marks, "\n")
}

// stockAddOrdersHold reports whether the orders that can matter to the Add
// are as they were. The broker's order generation answers at once when no
// order frame arrived; otherwise a fresh all-client read decides, and the
// evidence carries its generation on.
func (s *Server) stockAddOrdersHold(ctx context.Context, ev *stockAddEvidence) bool {
	b := s.currentProtectionOrderSnapshotBinding()
	if b.generation == ev.ordersGeneration {
		return true
	}
	if ctx == nil {
		return false
	}
	inventory, scope, err := s.brokerOpenOrderInventory(ctx, true)
	if err != nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) || stockAddOrderMark(inventory.Orders, ev.contract.ConID, scope) != ev.ordersMark {
		return false
	}
	ev.ordersGeneration = inventory.Generation
	return true
}

// stockAddFeeProblem names why a WhatIf gives no commission bound in the
// order's cash currency, or "".
func stockAddFeeProblem(w rpc.OrderWhatIfResult, currency string) string {
	switch {
	case w.Status != rpc.OrderWhatIfStatusAccepted || !w.Available:
		return fmt.Sprintf("the broker's WhatIf gave no accepted answer in time (%s)", nonEmptyString(w.Status, "no answer"))
	case w.Margin == nil:
		return "the broker's WhatIf carried no commission figures"
	case normCcy(w.Margin.CommissionCurrency) != normCcy(currency):
		return fmt.Sprintf("the broker quoted the commission in %s, not %s", nonEmptyString(normCcy(w.Margin.CommissionCurrency), "no currency"), normCcy(currency))
	default:
		return "the broker's commission figures carry no usable bound"
	}
}
