package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The bucket cap uses the draft's bill units and limit, not a displayed
// notional or the account's separately configured trading limit.
func TestCashSweepPreviewCapUsesActualUnitsPriceAndPinnedFX(t *testing.T) {
	for _, side := range []string{rpc.CashSweepSideInvest, rpc.CashSweepSideRedeem} {
		t.Run(side, func(t *testing.T) {
			prop := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCashSweep, Contract: rpc.ContractParams{SecType: "BILL", Currency: "USD"}, CashSweep: &rpc.TradeProposalCashSweep{
				Side: side, Currency: "USD", Instrument: cashSweepInstrumentUSTBill, ExchangeRate: 0.9, MaxOrderNotionalBase: 1800,
			}}
			preview := &rpc.OrderPreviewResult{Notional: 1, NotionalBase: 1, Draft: rpc.OrderDraft{
				Contract: rpc.ContractParams{SecType: "BILL", Currency: "USD"}, Quantity: 2, LimitPrice: 100, Bond: cashSweepOrderTerms(prop),
			}}
			if got := cashSweepPreviewNotionalBlockers(prop, preview); len(got) != 0 {
				t.Fatalf("exact cap rejected: %v", got)
			}
			preview.Draft.LimitPrice = 100.01
			if got := cashSweepPreviewNotionalBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_above_max_order_notional") {
				t.Fatalf("fresh draft above cap accepted: %v", got)
			}
		})
	}
}

func TestCashSweepPreviewCapFailsClosedWithoutFiniteTerms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*rpc.TradeProposal, *rpc.OrderPreviewResult)
	}{
		{"missing cap", func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CashSweep.MaxOrderNotionalBase = 0 }},
		{"nonfinite cap", func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CashSweep.MaxOrderNotionalBase = math.Inf(1) }},
		{"missing FX", func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CashSweep.ExchangeRate = 0 }},
		{"nonfinite FX", func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CashSweep.ExchangeRate = math.NaN() }},
		{"nonfinite limit", func(_ *rpc.TradeProposal, v *rpc.OrderPreviewResult) { v.Draft.LimitPrice = math.Inf(1) }},
		{"wrong unit", func(_ *rpc.TradeProposal, v *rpc.OrderPreviewResult) { v.Draft.Bond.FacePerUnit = 1 }},
		{"wrong currency", func(_ *rpc.TradeProposal, v *rpc.OrderPreviewResult) { v.Draft.Contract.Currency = "EUR" }},
		{"overflow", func(p *rpc.TradeProposal, v *rpc.OrderPreviewResult) {
			p.CashSweep.ExchangeRate = 1e308
			v.Draft.Quantity = 2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prop := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCashSweep, Contract: rpc.ContractParams{SecType: "BILL", Currency: "USD"}, CashSweep: &rpc.TradeProposalCashSweep{
				Side: rpc.CashSweepSideRedeem, Currency: "USD", Instrument: cashSweepInstrumentUSTBill, ExchangeRate: 0.9, MaxOrderNotionalBase: 1800,
			}}
			preview := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Contract: rpc.ContractParams{SecType: "BILL", Currency: "USD"}, Quantity: 1,
				LimitPrice: 100, Bond: cashSweepOrderTerms(prop)}}
			tc.change(&prop, preview)
			if got := cashSweepPreviewNotionalBlockers(prop, preview); !hasTradingBlocker(got, "cash_sweep_notional_unknown") {
				t.Fatalf("unprovable cap accepted: %v", got)
			}
		})
	}
}
