package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
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

// Through the preview: a bounded-limit draft reads the bid/ask quote, and a
// draft with its own limit the ordinary one.
func TestOrderPreviewRoutesABoundedLimitDraftToTheBidAskQuote(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	plain := fixedPreviewQuote(2.4, 2.6)
	var trade, sides int
	srv.orderPreviewQuote = func(ctx context.Context, c rpc.ContractParams, d time.Duration) (rpc.OrderQuoteSnapshot, error) {
		trade++
		return plain(ctx, c, d)
	}
	srv.orderPreviewSidesQuoteForTest = func(ctx context.Context, c rpc.ContractParams, d time.Duration) (rpc.OrderQuoteSnapshot, error) {
		sides++
		return plain(ctx, c, d)
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(4, 2, rpc.OrderPositionEffectReduce)
	contract := rpc.ContractParams{ConID: 700001, Symbol: "SYNTH", SecType: "OPT", Currency: "EUR", Expiry: "20261218", Right: "C", Strike: 50, Multiplier: 100}
	_, _ = srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Quantity: 2, Contract: contract, Strategy: rpc.OrderStrategyBoundedLimit,
		Bounded: &rpc.OrderBoundedLimit{Concession: 0.5, WorstPrice: 1, MaxSpreadPctOfMid: 50}})
	if sides != 1 || trade != 0 {
		t.Fatalf("bounded-limit draft read %d bid/ask and %d last-trade quotes, want 1 and 0", sides, trade)
	}
	limit := 2.5
	_, _ = srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Quantity: 2, Contract: contract, LimitPrice: &limit})
	if sides != 1 || trade != 1 {
		t.Fatalf("explicit-limit draft read %d bid/ask and %d last-trade quotes, want 1 and 1 in all", sides, trade)
	}
}
