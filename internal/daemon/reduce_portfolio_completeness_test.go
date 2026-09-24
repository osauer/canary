package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestReduceSweepRequiresCompleteExposure(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*rpc.PositionsResult)
	}{
		{"stale_short", func(p *rpc.PositionsResult) { p.Stocks[1].Stale = true }},
		{"missing_mark", func(p *rpc.PositionsResult) { p.Stocks[1].Mark = 0 }},
		{"missing_fx", func(p *rpc.PositionsResult) { p.Stocks[1].Currency = "EUR" }},
		{"invalid_mark", func(p *rpc.PositionsResult) { p.Stocks[1].Mark = math.NaN() }},
		{"invalid_fx", func(p *rpc.PositionsResult) {
			p.Stocks[1].Currency, p.Stocks[1].FXRate = "EUR", new(math.Inf(1))
		}},
		{"missing_delta", func(p *rpc.PositionsResult) {
			p.Stocks = p.Stocks[:1]
			p.Options = []rpc.PositionView{{Symbol: "HEDGE", ConID: 3, SecType: "OPTION", Currency: "USD", Quantity: 4, Right: "P", Multiplier: 100, Underlying: new(100.0)}}
		}},
		{"missing_authority", func(p *rpc.PositionsResult) { p.Authority = nil }},
		{"partial_download", func(p *rpc.PositionsResult) { p.Authority.Availability = rpc.AccountDataUnavailable }},
		{"stale_download", func(p *rpc.PositionsResult) { p.Authority.Freshness = rpc.AccountDataFreshnessStale }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos := reduceTestPortfolio([]rpc.PositionView{
				{Symbol: "LONG", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 100},
				{Symbol: "SHORT", SecType: "STOCK", ConID: 2, Currency: "USD", Quantity: -200, Mark: 100},
			}, nil)
			tc.edit(pos)
			cands, net, complete, target, blockers := reduceSweepCandidates(pos, 50)
			if complete || len(cands) != 0 || target != 0 || len(blockers) != 1 || blockers[0].Code != "net_delta_incomplete" {
				t.Fatalf("partial book authorized a sweep: complete=%v candidates=%d target=%v blockers=%+v", complete, len(cands), target, blockers)
			}
			if math.IsNaN(net) || math.IsInf(net, 0) {
				t.Fatal("unavailable net exposure must remain JSON-serializable")
			}
		})
	}
}

func TestReduceSweepCompleteHedgedBookReducesNetExposure(t *testing.T) {
	pos := reduceTestPortfolio([]rpc.PositionView{
		{Symbol: "LONG", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 100},
		{Symbol: "SHORT", SecType: "STOCK", ConID: 2, Currency: "USD", Quantity: -200, Mark: 100},
	}, nil)
	cands, net, complete, target, blockers := reduceSweepCandidates(pos, 50)
	if net != -10000 || !complete || target != 5000 || len(blockers) != 0 {
		t.Fatalf("complete book blocked: net=%v complete=%v target=%v blockers=%+v", net, complete, target, blockers)
	}
	if len(cands) != 1 || cands[0].row.ConID != 2 || cands[0].qty != 50 {
		t.Fatalf("expected covering 50 short shares, got %+v", cands)
	}
}
