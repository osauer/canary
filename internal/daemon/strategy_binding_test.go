package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// strategyBindingDraft is a held put vertical (long the higher strike, short
// the lower, same expiry) at two units, in the shape previewStrategyOrder
// builds for op over units.
func strategyBindingDraft(op string, units int) rpc.StrategyOrderDraft {
	adding := op == rpc.StrategyOperationAdd
	after := 2 - units
	if adding {
		after = 2 + units
	}
	leg := func(conID int, strike float64, ratio int) rpc.StrategyOrderLeg {
		action := rpc.OrderActionSell
		if (ratio < 0) != adding {
			action = rpc.OrderActionBuy
		}
		before := float64(2 * ratio)
		q := absOrderRatio(ratio) * units
		legAfter := before + float64(q)
		if action == rpc.OrderActionSell {
			legAfter = before - float64(q)
		}
		return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: conID, Symbol: "SYNA", SecType: "OPT", Right: "P", Strike: strike, Expiry: "20270115", Multiplier: 100, Exchange: "SMART", Currency: "USD"},
			Ratio: ratio, Action: action, Quantity: q, Before: before, After: legAfter}
	}
	return rpc.StrategyOrderDraft{StrategyID: "strat-binding", StrategyRevision: 1, Operation: op, Units: units, UnitsBefore: 2, UnitsAfter: after, GuaranteedCombo: true,
		Legs: []rpc.StrategyOrderLeg{leg(7401, 145, 1), leg(7402, 135, -1)}}
}

// The strategy position binding holds the units arithmetic of the operation:
// an add raises the held units by the units bought, a close or reduction
// lowers them by the units sold, and nothing else binds. Past the binding the
// capture reads the broker projection; with no connector it stops there with
// the drift error, which is how this test sees the binding admit a draft.
func TestStrategyPositionBindingFollowsTheOperation(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderPreviewPositionImpact = fixedPreviewPosition(2, 2, rpc.OrderPositionEffectIncrease)
	status := rpc.TradingStatus{Account: "DU1234567", Mode: config.TradingModePaper}
	capture := func(d rpc.StrategyOrderDraft) error {
		_, err := srv.captureBoundStrategyPositionAuthority(t.Context(), nil, ibkrlib.ConnectorSessionBinding{}, status, d)
		return err
	}
	admitted := func(err error) bool {
		return err != nil && !strings.Contains(err.Error(), "invalid strategy position binding")
	}
	for _, tc := range []struct {
		name  string
		draft rpc.StrategyOrderDraft
		want  bool
	}{
		{"add one unit", strategyBindingDraft(rpc.StrategyOperationAdd, 1), true},
		{"reduce one unit", strategyBindingDraft(rpc.StrategyOperationReduce, 1), true},
		{"close two units", strategyBindingDraft(rpc.StrategyOperationClose, 2), true},
		{"add bound as a reduction", func() rpc.StrategyOrderDraft {
			d := strategyBindingDraft(rpc.StrategyOperationAdd, 1)
			d.UnitsAfter = 1
			return d
		}(), false},
		{"reduction bound as an add", func() rpc.StrategyOrderDraft {
			d := strategyBindingDraft(rpc.StrategyOperationReduce, 1)
			d.UnitsAfter = 3
			return d
		}(), false},
		{"add with no units held", func() rpc.StrategyOrderDraft {
			d := strategyBindingDraft(rpc.StrategyOperationAdd, 1)
			d.UnitsBefore, d.UnitsAfter = 0, 1
			return d
		}(), false},
	} {
		if got := admitted(capture(tc.draft)); got != tc.want {
			t.Errorf("%s: binding admitted = %v (%v), want %v", tc.name, got, capture(tc.draft), tc.want)
		}
	}
}
