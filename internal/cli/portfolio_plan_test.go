package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func goldenPortfolioPlan() rpc.PortfolioPlanResult {
	return rpc.PortfolioPlanResult{Kind: "ibkr.portfolio_plan", AsOf: goldenAt, ValidUntil: goldenAt.AddDate(0, 0, 1), State: "review", Reason: "One addition is checked for review. Calculate again after its outcome before sizing another candidate.", Regime: risk.RegimeBucketCalm,
		Targets:    []risk.PortfolioIntent{{Symbol: "SYNA", Decision: "add", Reason: "Owner target", StockPctNLV: new(2.), TargetPctNLV: new(3.), DesiredQuantity: 10}, {Symbol: "SYNB", Decision: "wait_for_replan", Reason: "Calculate again after the first outcome.", StockPctNLV: new(0.), TargetPctNLV: new(1.), DesiredQuantity: 5}},
		Unassigned: []rpc.ContractParams{{Symbol: "SYNC", Currency: "USD", ConID: 303}}, Next: &rpc.PortfolioPlanAction{Kind: "add", Add: &rpc.AddPlanResult{Quantity: 10, Cost: 1002, Contract: rpc.ContractParams{Symbol: "SYNA"}, LimitPrice: 100, Currency: "USD"}}}
}

func TestPortfolioPlanCLIIsReadOnlyAndRejectsExtraArguments(t *testing.T) {
	for _, args := range [][]string{{"plan", "--json"}, {"plan", "SYNA"}, {"plan", "--submit"}, {"plan", "--quantity", "5"}, {"unknown"}} {
		conn := &stockAddConn{}
		var out, errs bytes.Buffer
		code := Run(t.Context(), &Env{Conn: conn, Stdout: &out, Stderr: &errs}, "portfolio", args)
		if len(args) == 2 && args[1] == "--json" {
			if code != 0 || conn.method != rpc.MethodPortfolioPlan {
				t.Fatalf("%v %d %s", args, code, errs.String())
			}
			var result rpc.PortfolioPlanResult
			if json.Unmarshal(out.Bytes(), &result) != nil {
				t.Fatal("JSON lost")
			}
		} else if code == 0 || conn.method != "" {
			t.Fatalf("unexpected read/action for %v", args)
		}
	}
}
