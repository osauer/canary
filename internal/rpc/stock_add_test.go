package rpc

import (
	"github.com/osauer/canary/v2/internal/risk"
	"math"
	"testing"
)

func TestStockAddParamsRequireSelectionNotClientHoldings(t *testing.T) {
	good := AddParams{Contract: ContractParams{Symbol: " syna ", Currency: " usd "}, LimitPrice: 100, Max: true}
	p, err := NormalizeAddParams(good)
	if err != nil || p.Contract.Symbol != "SYNA" || p.Contract.Currency != "USD" || p.Contract.SecType != "STK" || p.Quantity != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, change := range []func(*AddParams){func(p *AddParams) { p.Max = false }, func(p *AddParams) { p.Quantity = 1 }, func(p *AddParams) { p.Contract.Currency = "" }, func(p *AddParams) { p.Contract.SecType = "OPT" }, func(p *AddParams) { p.Contract.SecType = "BOND" }, func(p *AddParams) { p.Contract.Multiplier = 100 }, func(p *AddParams) { p.Contract.Strike = 10 }, func(p *AddParams) { p.Quantity = -1 }, func(p *AddParams) { p.Quantity = 1000001 }, func(p *AddParams) { p.LimitPrice = math.Inf(1) }, func(p *AddParams) { p.LimitPrice = 0 }} {
		bad := good
		change(&bad)
		if _, err := NormalizeAddParams(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestStockAddReviewCloneDetachesAllEvidence(t *testing.T) {
	in := &AddReview{Plan: risk.StockAddPlan{
		Allowances: []risk.StockAddAllowance{{Limit: 100}},
		RiskChecks: []risk.StockAddRiskCheck{{BeforeEvidence: "before"}},
		Cash:       &risk.StockAddCash{Available: 100},
		Protection: &risk.StockAddProtection{StopQuantity: 2},
		Margin:     &risk.StockAddMargin{Quantity: 3, LookAheadExcessBase: new(50.)},
	}}
	out := CloneAddReview(in)
	out.Plan.Allowances[0].Limit = 0
	out.Plan.RiskChecks[0].BeforeEvidence = "changed"
	out.Plan.Cash.Available = 0
	out.Plan.Protection.StopQuantity = 0
	*out.Plan.Margin.LookAheadExcessBase = 0
	if in.Plan.Allowances[0].Limit != 100 || in.Plan.RiskChecks[0].BeforeEvidence != "before" || in.Plan.Cash.Available != 100 || in.Plan.Protection.StopQuantity != 2 || *in.Plan.Margin.LookAheadExcessBase != 50 {
		t.Fatal("review shares mutable planning evidence")
	}
}
