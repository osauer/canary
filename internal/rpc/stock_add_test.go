package rpc

import (
	"math"
	"testing"
)

func TestStockAddParamsRequireSelectionNotClientHoldings(t *testing.T) {
	good := AddParams{Contract: ContractParams{Symbol: " syna ", Currency: " usd "}, LimitPrice: 100}
	p, err := NormalizeAddParams(good)
	if err != nil || p.Contract.Symbol != "SYNA" || p.Contract.Currency != "USD" || p.Contract.SecType != "STK" || p.Quantity != 0 {
		t.Fatalf("%+v %v", p, err)
	}
	for _, change := range []func(*AddParams){func(p *AddParams) { p.Contract.Currency = "" }, func(p *AddParams) { p.Contract.SecType = "OPT" }, func(p *AddParams) { p.Contract.SecType = "BOND" }, func(p *AddParams) { p.Contract.Multiplier = 100 }, func(p *AddParams) { p.Contract.Strike = 10 }, func(p *AddParams) { p.Quantity = -1 }, func(p *AddParams) { p.Quantity = 1000001 }, func(p *AddParams) { p.LimitPrice = math.Inf(1) }, func(p *AddParams) { p.LimitPrice = 0 }} {
		bad := good
		change(&bad)
		if _, err := NormalizeAddParams(bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
