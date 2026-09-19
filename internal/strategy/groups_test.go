package strategy

import (
	"reflect"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestInferPositionStrategiesVerticalUsesWholeUnits(t *testing.T) {
	strategies, issues := InferPositionStrategies([]rpc.PositionView{
		{Symbol: "SNOW", SecType: "OPT", ConID: 101, Currency: "USD", Expiry: "20260918", Right: "P", Strike: 180, Multiplier: 100, Quantity: 3},
		{Symbol: "SNOW", SecType: "OPT", ConID: 102, Currency: "USD", Expiry: "20260918", Right: "P", Strike: 170, Multiplier: 100, Quantity: -6},
	})
	if len(issues) != 0 || len(strategies) != 1 {
		t.Fatalf("strategies=%+v issues=%+v", strategies, issues)
	}
	got := strategies[0]
	if got.Kind != "vertical" || got.Units != 3 || len(got.Legs) != 2 {
		t.Fatalf("strategy=%+v", got)
	}
	if got.Legs[0].Ratio != 1 || got.Legs[1].Ratio != -2 {
		t.Fatalf("ratios=%d,%d", got.Legs[0].Ratio, got.Legs[1].Ratio)
	}
	if got.PositionFingerprint == "" || got.ID == "" || !got.Actionable {
		t.Fatalf("strategy identity/actionability missing: %+v", got)
	}
}

func TestInferPositionStrategiesLeavesAmbiguousBookStandalone(t *testing.T) {
	strategies, issues := InferPositionStrategies([]rpc.PositionView{
		{Symbol: "SPY", SecType: "OPT", ConID: 1, Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", ConID: 2, Quantity: -1},
		{Symbol: "SPY", SecType: "OPT", ConID: 3, Quantity: 1},
	})
	if len(strategies) != 0 || len(issues) != 1 {
		t.Fatalf("strategies=%+v issues=%+v", strategies, issues)
	}
	if issues[0].Reason != "multiple strategy decompositions are possible" {
		t.Fatalf("reason=%q", issues[0].Reason)
	}
}

func TestInferPositionStrategiesRequiresExactWholeContracts(t *testing.T) {
	strategies, issues := InferPositionStrategies([]rpc.PositionView{
		{Symbol: "SPY", SecType: "OPT", ConID: 0, Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", ConID: 2, Quantity: -1},
	})
	if len(strategies) != 0 || len(issues) != 1 {
		t.Fatalf("strategies=%+v issues=%+v", strategies, issues)
	}
}

func TestOptionSecurityTypeAliasesPreserveExactStrategyAndRejectIncompleteLegs(t *testing.T) {
	rows := []rpc.PositionView{
		{Symbol: "SYNTH", SecType: "OPT", ConID: 9001, Currency: "USD", Expiry: "20261218", Right: "P", Strike: 50, Multiplier: 100, Quantity: 3},
		{Symbol: "SYNTH", SecType: "OPT", ConID: 9002, Currency: "USD", Expiry: "20261218", Right: "P", Strike: 40, Multiplier: 100, Quantity: -6},
	}
	expected, issues := InferPositionStrategies(rows)
	if len(expected) != 1 || len(issues) != 0 {
		t.Fatal("invalid fixture")
	}
	for _, kind := range []string{"OPTION", " option ", "Opt"} {
		input := append([]rpc.PositionView(nil), rows...)
		for i := range input {
			input[i].SecType = kind
		}
		got, issues := InferPositionStrategies(input)
		if len(issues) != 0 || !reflect.DeepEqual(got, expected) {
			t.Fatalf("alias %q lost exact grouping: groups=%v issues=%v", kind, got, issues)
		}
	}
	for _, defect := range []string{"missing identity", "fractional quantity", "not an option", "ambiguous decomposition"} {
		t.Run(defect, func(t *testing.T) {
			input := append([]rpc.PositionView(nil), rows...)
			for i := range input {
				input[i].SecType = "OPTION"
			}
			switch defect {
			case "missing identity":
				input[0].ConID = 0
			case "fractional quantity":
				input[0].Quantity = 1.5
			case "not an option":
				input[0].SecType = "STK"
			case "ambiguous decomposition":
				input = append(input, rpc.PositionView{Symbol: "SYNTH", SecType: "OPTION", ConID: 9003, Quantity: 1})
			}
			got, issues := InferPositionStrategies(input)
			if len(got) != 0 || len(issues) != 1 {
				t.Fatal("alias normalization relaxed strategy evidence", got, issues)
			}
		})
	}
}

// Two long calls at different strikes and expiries were read as a "diagonal",
// which then demanded per-leg operator declarations before either could be
// exit-managed. Nothing in a same-direction stack depends on another leg.
func TestInferPositionStrategiesLeavesSameDirectionStacksStandalone(t *testing.T) {
	leg := func(symbol string, conID int, expiry, right string, strike, quantity float64) rpc.PositionView {
		return rpc.PositionView{Symbol: symbol, SecType: "OPT", ConID: conID, Currency: "USD", Expiry: expiry, Right: right, Strike: strike, Multiplier: 100, Quantity: quantity}
	}
	for name, rows := range map[string][]rpc.PositionView{
		"two long calls":    {leg("IBM", 1, "20261016", "C", 255, 20), leg("IBM", 2, "20261120", "C", 245, 25)},
		"two long puts":     {leg("SPY", 3, "20261016", "P", 710, 10), leg("SPY", 4, "20261120", "P", 720, 10)},
		"same expiry longs": {leg("IBM", 5, "20261016", "C", 255, 1), leg("IBM", 6, "20261016", "C", 260, 1)},
		"two short calls":   {leg("IBM", 7, "20261016", "C", 255, -1), leg("IBM", 8, "20261120", "C", 245, -1)},
		"three long calls":  {leg("IBM", 9, "20261016", "C", 255, 1), leg("IBM", 10, "20261120", "C", 245, 1), leg("IBM", 11, "20261218", "C", 240, 1)},
	} {
		t.Run(name, func(t *testing.T) {
			strategies, issues := InferPositionStrategies(rows)
			if len(strategies) != 0 || len(issues) != 0 {
				t.Fatalf("a same-direction stack became a strategy or an ambiguity: strategies=%+v issues=%+v", strategies, issues)
			}
		})
	}
	// Opposite signs of one right remain a spread; opposite rights keep their combos.
	spread, issues := InferPositionStrategies([]rpc.PositionView{leg("IBM", 1, "20261016", "C", 255, 20), leg("IBM", 2, "20261120", "C", 245, -25)})
	if len(issues) != 0 || len(spread) != 1 || spread[0].Kind != "diagonal" {
		t.Fatalf("a long/short diagonal lost its grouping: %+v %+v", spread, issues)
	}
	straddle, issues := InferPositionStrategies([]rpc.PositionView{leg("IBM", 1, "20261016", "C", 255, 1), leg("IBM", 2, "20261016", "P", 255, 1)})
	if len(issues) != 0 || len(straddle) != 1 || straddle[0].Kind != "straddle" {
		t.Fatalf("a long straddle lost its grouping: %+v %+v", straddle, issues)
	}
}
