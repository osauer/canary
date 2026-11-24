package daemon

import (
	"strings"

	"github.com/osauer/canary/v3/internal/rpc"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// orderCombinationFromGroup is what a strategy order Canary placed trades,
// from the group journaled with it: the shape its legs form as held (a
// close of a bull call spread is still a bull_call_spread), the operation,
// units and every leg with its side in this order.
func orderCombinationFromGroup(g *rpc.StrategyOrderDraft) *rpc.OrderCombination {
	if g == nil || len(g.Legs) == 0 {
		return nil
	}
	out := &rpc.OrderCombination{Source: rpc.OrderCombinationCanary, Operation: g.Operation, StrategyID: g.StrategyID, Units: g.Units}
	rows := make([]rpc.PositionView, 0, len(g.Legs))
	for _, leg := range g.Legs {
		c := leg.Contract
		out.Legs = append(out.Legs, rpc.OrderCombinationLeg{Action: leg.Action, Ratio: absOrderRatio(leg.Ratio), Quantity: leg.Quantity, ConID: c.ConID,
			Symbol: c.Symbol, SecType: c.SecType, Expiry: c.Expiry, Strike: c.Strike, Right: c.Right, Multiplier: c.Multiplier, TradingClass: c.TradingClass, Exchange: c.Exchange})
		rows = append(rows, rpc.PositionView{Symbol: c.Symbol, SecType: "OPT", ConID: c.ConID, Currency: c.Currency, Quantity: float64(leg.Ratio),
			Multiplier: contractMultiplier(c), Expiry: c.Expiry, Strike: c.Strike, Right: c.Right, TradingClass: c.TradingClass})
	}
	if inferred, _ := positionstrategy.InferStrategies(nil, rows); len(inferred) == 1 {
		out.Kind = inferred[0].Kind
		out.Strategy = strings.ReplaceAll(nonEmptyString(inferred[0].Name, inferred[0].Kind), " ", "_")
	}
	return out
}

// brokerOrderCombination is a combination placed elsewhere, as the broker's
// open order reports its legs: each leg's ratio, contract ID and venue, and
// its side in this order (the broker states the combination's direction, so
// a sold combination sells each leg's stated side the other way). Nil for an
// order that is no combination.
func brokerOrderCombination(order ibkrlib.OrderLifecycleEvent) *rpc.OrderCombination {
	if !strings.EqualFold(order.SecType, "BAG") {
		return nil
	}
	out := &rpc.OrderCombination{Source: rpc.OrderCombinationBroker, Legs: []rpc.OrderCombinationLeg{}}
	sold := strings.EqualFold(order.Action, rpc.OrderActionSell)
	for _, leg := range order.ComboLegs {
		action := strings.ToUpper(strings.TrimSpace(leg.Action))
		if sold {
			switch action {
			case rpc.OrderActionBuy:
				action = rpc.OrderActionSell
			case rpc.OrderActionSell:
				action = rpc.OrderActionBuy
			}
		}
		out.Legs = append(out.Legs, rpc.OrderCombinationLeg{Action: action, Ratio: leg.Ratio, ConID: leg.ConID, Exchange: leg.Exchange})
	}
	return out
}
