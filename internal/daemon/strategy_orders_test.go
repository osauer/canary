package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestNormalizeStrategyOperationPreservesWholeUnits(t *testing.T) {
	op, units, err := normalizeStrategyOperation(rpc.StrategyOperationClose, 0, 3)
	if err != nil || op != rpc.StrategyOperationClose || units != 3 {
		t.Fatalf("close = %q, %d, %v", op, units, err)
	}
	op, units, err = normalizeStrategyOperation(rpc.StrategyOperationReduce, 1, 3)
	if err != nil || op != rpc.StrategyOperationReduce || units != 1 {
		t.Fatalf("reduce = %q, %d, %v", op, units, err)
	}
	if _, _, err := normalizeStrategyOperation(rpc.StrategyOperationReduce, 3, 3); err == nil {
		t.Fatal("reduce accepted a full close")
	}
	if _, _, err := normalizeStrategyOperation(rpc.StrategyOperationClose, 1, 3); err == nil {
		t.Fatal("close accepted a partial quantity")
	}
}

func TestGuaranteedUSOptionComboLegFailsClosed(t *testing.T) {
	leg := rpc.ContractParams{ConID: 101, SecType: "OPT", Exchange: "SMART", Currency: "USD", Multiplier: 100}
	if !guaranteedUSOptionComboLeg(leg, "US/Eastern") {
		t.Fatal("expected exact US SMART option leg to qualify")
	}
	leg.Exchange = "CBOE"
	if guaranteedUSOptionComboLeg(leg, "US/Eastern") {
		t.Fatal("direct route qualified as guaranteed SMART combo")
	}
	leg.Exchange = "SMART"
	if guaranteedUSOptionComboLeg(leg, "Europe/Berlin") {
		t.Fatal("non-US option qualified as supported guaranteed combo")
	}
}

func TestValidateStrategyReductionDraftRequiresEveryLegToReduce(t *testing.T) {
	draft := rpc.OrderDraft{
		Action: rpc.OrderActionSell, Contract: rpc.ContractParams{SecType: "BAG"}, Quantity: 1,
		StrategyGroup: &rpc.StrategyOrderDraft{
			GuaranteedCombo: true, Units: 1, UnitsBefore: 2, UnitsAfter: 1,
			Legs: []rpc.StrategyOrderLeg{
				{Contract: rpc.ContractParams{ConID: 11, SecType: "OPT"}, Ratio: 1, Action: rpc.OrderActionSell, Quantity: 1, Before: 2, After: 1},
				{Contract: rpc.ContractParams{ConID: 22, SecType: "OPT"}, Ratio: -1, Action: rpc.OrderActionBuy, Quantity: 1, Before: -2, After: -1},
			},
		},
	}
	position := rpc.OrderPositionImpact{Before: 2, After: 1, Effect: rpc.OrderPositionEffectReduce}
	if err := validateStrategyReductionDraft(draft, position, 10); err != nil {
		t.Fatalf("valid reduction rejected: %v", err)
	}
	draft.StrategyGroup.Legs[1].After = -3
	if err := validateStrategyReductionDraft(draft, position, 10); err == nil {
		t.Fatal("strategy accepted a leg that increased the position")
	}
}

func TestCurrentKnownStrategyUnitsDetectsBrokenRatio(t *testing.T) {
	known := rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{
		{Contract: rpc.ContractParams{ConID: 11}, Ratio: 1},
		{Contract: rpc.ContractParams{ConID: 22}, Ratio: -2},
	}}
	units, _, state := currentKnownStrategyUnits(known, map[int]float64{11: 2, 22: -4})
	if state != knownStrategyCurrent || units != 2 {
		t.Fatalf("proportional strategy = state %d, units %d", state, units)
	}
	if _, _, state := currentKnownStrategyUnits(known, map[int]float64{11: 2, 22: -3}); state != knownStrategyBroken {
		t.Fatalf("broken ratio state = %d", state)
	}
	if _, _, state := currentKnownStrategyUnits(known, map[int]float64{}); state != knownStrategyClosed {
		t.Fatalf("closed strategy state = %d", state)
	}
}
