package strategy

import (
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
