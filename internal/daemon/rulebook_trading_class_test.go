package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The Rulebook's legs carry each option's listed class, which the pair rules
// compare: a sold option of another class caps nothing.
func TestRuleLegsCarryTheTradingClass(t *testing.T) {
	weekly := rpc.PositionView{Symbol: "IDX", SecType: "OPT", ConID: 5, Currency: "USD", Quantity: 1, Multiplier: 100, Mark: 2, Right: "P", Strike: 520, Expiry: "20261218", TradingClass: "IDXW"}
	monthly := weekly
	monthly.ConID, monthly.Quantity, monthly.Strike, monthly.TradingClass = 6, -1, 470, "IDX"
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{weekly, monthly},
		ByUnderlying: []rpc.PositionGroup{{Underlying: "IDX", Options: []rpc.PositionView{weekly, monthly}}}}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "USD")
	if len(names) != 1 || len(names[0].Legs) != 2 {
		t.Fatalf("names = %+v", names)
	}
	got := map[string]bool{}
	for _, l := range names[0].Legs {
		got[l.TradingClass] = true
	}
	if !got["IDXW"] || !got["IDX"] {
		t.Fatalf("leg classes = %v, want IDXW and IDX", got)
	}
}
