package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The list reads a strategy by the daemon's name for the held position, and
// a covered call with the shares it is written against and why no unit order
// carries it.
func TestStrategiesListNamesTheHeldPosition(t *testing.T) {
	spread := goldenStrategy()
	spread.Kind, spread.Name = "vertical", "bull call spread"
	covered := rpc.PositionStrategy{ID: "strat-bbb-1", Revision: 1, Underlying: "BBB", Kind: "covered_call", Name: "covered call",
		Source: rpc.PositionStrategySourceInferred, Status: rpc.PositionStrategyStatusCurrent, Units: 2,
		Legs:   []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{Symbol: "BBB", SecType: "OPT", Expiry: "20261218", Right: "C", Strike: 50, TradingClass: "BBB", ConID: 900011}, Quantity: -2, Ratio: -1}},
		Stock:  &rpc.PositionStrategyLeg{Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK", ConID: 900010}, Quantity: 200, Ratio: 100},
		Reason: "the call trades on its own order; the shares it is written against stay"}
	res := strategiesPositions(rpc.AccountDataScope{AccountID: "U1234567", AccountMode: rpc.AccountModeLive}, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", []rpc.PositionStrategy{spread, covered})
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: goldenConn{rpc.MethodPositionsList: res}}, "strategies", []string{"list"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"strat-aaa-1  AAA  2 unit(s)  bull call spread  revision 3",
		"strat-bbb-1  BBB  2 unit(s)  covered call  revision 1",
		"short 1 × 20261218 C 50.00 BBB  conID 900011",
		"against 200 shares of BBB  conID 900010",
		"the call trades on its own order; the shares it is written against stay",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}
