package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A bounded-limit order prices from the bid and ask alone, so its preview
// reads a quote judged by when those sides arrived, not by the last trade: a
// thinly traded option with a live market and an hour-old trade was refused
// quote_stale at every send (2026-11-03 19:22 CET).
func TestBoundedLimitPreviewJudgesTheSidesNotTheLastTrade(t *testing.T) {
	bound := &rpc.OrderBoundedLimit{Concession: 0.5, WorstPrice: 0.5, MaxSpreadPctOfMid: 50}
	if !previewPricesFromSides(rpc.OrderPreviewParams{Strategy: rpc.OrderStrategyBoundedLimit, Bounded: bound}) {
		t.Fatal("a bounded-limit preview reads the last-trade quote")
	}
	if previewPricesFromSides(rpc.OrderPreviewParams{Strategy: rpc.OrderStrategyExplicitLimit}) {
		t.Fatal("an explicit-limit preview waits for both sides")
	}
	now := optionExitTestTime()
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNTH", SecType: "OPT", Currency: "USD", Multiplier: 100}
	quote := func() *rpc.Quote {
		return &rpc.Quote{Symbol: contract.Symbol, Contract: contract, Bid: new(1.0), Ask: new(1.05), Last: new(1.1),
			TradeAt: now.Add(-time.Hour), PriceAt: now.Add(-time.Hour), DataType: rpc.MarketDataLive, AsOf: now}
	}
	s := &Server{}
	byTrade := quote()
	s.decorateExactPreviewQuote(byTrade, contract)
	if _, err := boundedLimitPrice(rpc.OrderActionSell, bound, nil, contract, orderQuoteSnapshotFromQuote(byTrade)); !previewRefusedWith(err, previewQuoteStaleCode) {
		t.Fatalf("premise: the last-trade snapshot of an hour-old trade is not refused quote_stale: %v", err)
	}
	sides := s.exactBidAskQuoteSnapshot(quote(), contract, now.Add(-3*time.Second), now.Add(-2*time.Second), now.Add(-time.Second))
	if _, err := boundedLimitPrice(rpc.OrderActionSell, bound, nil, contract, sides); err != nil {
		t.Fatalf("live sides with an hour-old trade: %v", err)
	}
}
