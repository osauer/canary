package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The loss measure's inputs (spec TM-04.12; owner decision 2026-10-27
// 19:50–19:52 CET): every bought option counts at its premium at risk, the
// higher of the price paid and today's value as rule 2 measures a line
// (risk.PremiumAtRiskLeg), protection included (the owner: "Count it"); a
// share line counts at the cash loss to its stop while a stop or trail works
// for it at the broker, and is named as unstopped otherwise. The evaluator
// sums the figures per leg (mandate.Leg.LossPerUnitBase) and judges them
// against the loss room; nothing here reads the broker on the order path.

// mandateArmedStop is one working stop or trail on a share line as the
// open-order inventory shows it: the price it triggers at, in the line's
// currency, and the shares it covers.
type mandateArmedStop struct {
	StopPrice float64
	Quantity  float64
}

// mandateOptionPremiumInput maps an option row to the rulebook's leg for
// the premium at risk, as mapRuleNames does: the base value, substituted
// (and so unmeasurable) without an exchange rate, and the price paid, which
// IBKR reports multiplier-inclusive per contract.
func mandateOptionPremiumInput(row rpc.PositionView, base string) risk.LegInput {
	leg := risk.LegInput{Quantity: row.Quantity}
	if row.MarketValueBase != nil {
		leg.MarketValueBase = *row.MarketValueBase
	} else {
		leg.MarketValueBase = row.MarketValue
		leg.MarketValueBaseSource = risk.MarketValueBaseSourceSubstituted
	}
	var fx *float64
	if rate, ok := positionBaseRate(row, base); ok {
		fx = &rate
	} else if row.MarketValueBase != nil && row.MarketValue != 0 {
		fx = new(*row.MarketValueBase / row.MarketValue)
	}
	if fx != nil && row.AvgCost > 0 {
		leg.CostBasisBase = new(row.AvgCost * math.Abs(row.Quantity) * *fx)
	}
	return leg
}

// mandateOptionLoss fills a bought option's loss per contract from its row;
// a short leg carries none. An unmeasurable premium is named on the leg and
// makes the verdict unknown: nothing passes unmeasured.
func mandateOptionLoss(leg *mandate.Leg, row rpc.PositionView, base string) {
	if row.Quantity <= 0 {
		return
	}
	atRisk, _, ok, why := risk.PremiumAtRiskLeg(mandateOptionPremiumInput(row, base))
	if !ok {
		leg.LossUnknown = why
		return
	}
	leg.LossPerUnitBase = atRisk / row.Quantity
}

// mandateArmedStops reads the stops that bound the held share lines from
// the open-order inventory: every working STP, STP LMT, TRAIL or TRAIL LIMIT
// on the reducing side of a line, keyed by the leg's ID. It returns the
// reason instead when no complete current inventory exists for the scope;
// every share line then reads as unstopped (the measure counts nothing for
// it) and the verdict says so.
func (s *Server) mandateArmedStops(ctx context.Context, pos *rpc.PositionsResult, scope brokerStateScope) (map[string][]mandateArmedStop, string) {
	if s == nil || ctx == nil || pos == nil {
		return nil, "the open orders cannot be read"
	}
	snapshot, inventoryScope, err := s.brokerOpenOrderInventory(ctx, false)
	switch {
	case err != nil:
		return nil, "the open orders cannot be read (" + openOrderInventoryFailure(err) + ")"
	case !sameBrokerScope(inventoryScope, scope):
		return nil, "the open orders belong to another account than the selected one"
	}
	// Attached children require the durable parent-fill/child-activation
	// proof used by protection coverage. An unavailable journal proves no
	// child active; standalone broker stops can still be measured.
	views, _, _ := s.loadOrderViews()
	working := brokerWorkingOrders(snapshot, views, scope)
	// An OCA member on another contract, or a non-stop member, can remove
	// this line's protection too. Group the complete inventory before filtering.
	groups := map[string][]brokerWorkingOrder{}
	for _, candidate := range working {
		if group := strings.TrimSpace(candidate.Order.OcaGroup); group != "" {
			groups[group] = append(groups[group], candidate)
		}
	}
	out := map[string][]mandateArmedStop{}
	for _, row := range pos.Stocks {
		if row.Quantity == 0 || row.ConID <= 0 {
			continue
		}
		seen := map[string]bool{}
		for _, candidate := range working {
			var stop mandateArmedStop
			var ok bool
			if group := strings.TrimSpace(candidate.Order.OcaGroup); group != "" {
				if seen[group] {
					continue
				}
				seen[group] = true
				stop, ok = mandateOCAStop(groups[group], row)
			} else if candidate.Order.OcaType == 0 {
				stop, ok = mandateStopForPosition(candidate, row)
			}
			if ok {
				id := mandateLegID(row)
				out[id] = append(out[id], stop)
			}
		}
	}
	return out, ""
}

func mandateStopForPosition(candidate brokerWorkingOrder, row rpc.PositionView) (mandateArmedStop, bool) {
	order := candidate.Order
	if order.ConID != row.ConID || !mandateStopEffective(candidate) || !strings.EqualFold(order.Action, mandateAction(row)) || !mandateConditionalOrder(order.OrderType) {
		return mandateArmedStop{}, false
	}
	stop := mandateArmedStop{StopPrice: mandateStopPrice(order), Quantity: brokerOrderRemaining(order)}
	return stop, positiveFinite(stop.StopPrice) && positiveFinite(stop.Quantity)
}

// mandateOCAStop is the coverage the whole group can prove, not the sum of
// mutually exclusive orders. With block, whichever same-line stop fills can
// cancel or reduce its peers: only the smallest remaining quantity is common
// coverage, and the worst trigger bounds its measured loss. Partial fills do
// not grant the cancelled/reduced peers another full allocation of shares.
// Other contracts, non-stops, unconfirmed members or inconsistent types leave
// no provable stop coverage. Multiple non-blocking members can overfill and
// change the net position, so they cannot prove this line remains protected.
func mandateOCAStop(group []brokerWorkingOrder, row rpc.PositionView) (mandateArmedStop, bool) {
	if len(group) == 0 {
		return mandateArmedStop{}, false
	}
	typ := group[0].Order.OcaType
	if typ < ibkrlib.OcaTypeCancelWithBlock || typ > ibkrlib.OcaTypeReduceNonBlock || len(group) > 1 && typ == ibkrlib.OcaTypeReduceNonBlock {
		return mandateArmedStop{}, false
	}
	var combined mandateArmedStop
	for i, candidate := range group {
		stop, ok := mandateStopForPosition(candidate, row)
		if !ok || candidate.Order.OcaType != typ {
			return mandateArmedStop{}, false
		}
		if i == 0 {
			combined = stop
			continue
		}
		combined.Quantity = min(combined.Quantity, stop.Quantity)
		if row.Quantity > 0 {
			combined.StopPrice = min(combined.StopPrice, stop.StopPrice)
		} else {
			combined.StopPrice = max(combined.StopPrice, stop.StopPrice)
		}
	}
	return combined, true
}

// mandateStopEffective distinguishes executable protection from an order
// retained to block competing orders. A held child is not protection for an
// existing holding, even when its broker status reads PreSubmitted.
func mandateStopEffective(candidate brokerWorkingOrder) bool {
	order := candidate.Order
	if order.Deactivated != nil && *order.Deactivated || strings.TrimSpace(order.WhyHeld) != "" {
		return false
	}
	if order.Status != "Submitted" && order.Status != "PreSubmitted" {
		return false
	}
	view := candidate.Journal
	if order.ParentID != nil && *order.ParentID > 0 || view != nil && view.EntryLink != nil {
		return view != nil && view.EntryLink != nil && view.EntryState == "active" &&
			order.ParentID != nil && *order.ParentID == view.EntryLink.ParentID
	}
	return true
}

// mandateStopPrice is the price a working stop or trail triggers at: the
// trail's current stop price, or a stop's aux price. Zero when the broker
// reports none.
func mandateStopPrice(order ibkrlib.OrderLifecycleEvent) float64 {
	switch strings.ToUpper(strings.TrimSpace(order.OrderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return order.TrailStopPrice
	}
	return order.AuxPrice
}

// mandateApplyStops sets each share leg's loss per share from the stops
// that cover it: a long line loses the mark less the stop per covered share,
// a short line the stop less the mark, in base currency, and a stop above
// the mark (a long line in profit) loses nothing. A line every stop covers
// is armed; one covered in part counts the covered shares and is named as
// unstopped for the rest. With no inventory (reason set) nothing is armed,
// and the note says why every line reads unstopped.
func mandateApplyStops(legs []mandate.Leg, pos *rpc.PositionsResult, base string, stops map[string][]mandateArmedStop, reason string) (out []mandate.Leg, note string) {
	rows := map[string]rpc.PositionView{}
	for _, row := range pos.Stocks {
		rows[mandateLegID(row)] = row
	}
	out = make([]mandate.Leg, 0, len(legs))
	for _, l := range legs {
		if l.Kind != mandate.KindStock || l.Quantity == 0 {
			out = append(out, l)
			continue
		}
		row, held := rows[l.ID]
		rate, rateOK := positionBaseRate(row, base)
		covered, loss := 0.0, 0.0
		for _, st := range stops[l.ID] {
			if !held || !rateOK || !(row.Mark > 0) {
				break
			}
			perShare := row.Mark - st.StopPrice
			if row.Quantity < 0 {
				perShare = st.StopPrice - row.Mark
			}
			q := math.Min(st.Quantity, math.Abs(l.Quantity)-covered)
			if q <= 0 {
				break
			}
			covered += q
			loss += math.Max(0, perShare) * q * rate
		}
		if covered > 0 {
			// The covered shares' loss, spread over the line, so a cut of the
			// line takes its share of the loss off.
			l.LossPerUnitBase = loss / math.Abs(l.Quantity)
		}
		l.StopArmed = covered >= math.Abs(l.Quantity)-1e-9
		if !l.StopArmed && covered > 0 {
			l.Symbol = fmt.Sprintf("%s (%s of %s shares stopped)", l.Symbol, formatShares(covered), formatShares(math.Abs(l.Quantity)))
		}
		out = append(out, l)
	}
	if reason != "" {
		note = "share stops unread: " + reason + "; every share line counts as unstopped"
	}
	return out, note
}

// formatShares prints a share count without decimals.
func formatShares(v float64) string {
	return fmt.Sprintf("%.0f", v)
}
