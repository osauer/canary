package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func strategyQuoteReviewFixture() ([]rpc.StrategyOrderLeg, []rpc.OrderQuoteSnapshot) {
	at := time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC)
	legs := []rpc.StrategyOrderLeg{
		{Contract: rpc.ContractParams{ConID: 101, SecType: "OPT", Multiplier: 100}, Ratio: 1, Quantity: 2},
		{Contract: rpc.ContractParams{ConID: 102, SecType: "OPT", Multiplier: 100}, Ratio: -1, Quantity: 2},
	}
	quotes := []rpc.OrderQuoteSnapshot{
		{Bid: new(5.), Ask: new(5.2), DataType: rpc.MarketDataLive, QuoteQuality: "firm", AsOf: at, PriceAt: at, SessionContext: &rpc.MarketSession{IsOpen: true}},
		{Bid: new(2.), Ask: new(2.2), DataType: rpc.MarketDataLive, QuoteQuality: "firm", AsOf: at.Add(time.Second), PriceAt: at.Add(time.Second), SessionContext: &rpc.MarketSession{IsOpen: true}},
	}
	return legs, quotes
}

func TestStrategyAutomaticPriceRequiresEveryLegFreshLiveAndOpen(t *testing.T) {
	for _, change := range []struct {
		name  string
		apply func(*rpc.OrderQuoteSnapshot)
	}{
		{"delayed", func(q *rpc.OrderQuoteSnapshot) { q.DataType = rpc.MarketDataDelayed }},
		{"frozen", func(q *rpc.OrderQuoteSnapshot) { q.DataType = rpc.MarketDataFrozen }},
		{"delayed frozen", func(q *rpc.OrderQuoteSnapshot) { q.DataType = rpc.MarketDataDelayedFrozen }},
		{"unclassified", func(q *rpc.OrderQuoteSnapshot) { q.DataType = "" }},
		{"stale", func(q *rpc.OrderQuoteSnapshot) { q.Stale = true; q.StaleReason = "old leg" }},
		{"closed", func(q *rpc.OrderQuoteSnapshot) { q.SessionContext = &rpc.MarketSession{IsOpen: false, State: "closed"} }},
	} {
		for _, badLeg := range []int{0, 1} {
			t.Run(change.name+string(rune('0'+badLeg)), func(t *testing.T) {
				legs, quotes := strategyQuoteReviewFixture()
				change.apply(&quotes[badLeg])
				if _, _, _, err := strategyPriceQuote("SYNTH", legs, quotes, nil); err == nil {
					t.Fatal("unusable leg produced an automatic executable combo price")
				}
				explicit := -1.25
				if _, limit, _, err := strategyPriceQuote("SYNTH", legs, quotes, &explicit); err != nil || limit != explicit {
					t.Fatalf("owner price exception lost: limit=%v err=%v", limit, err)
				}
			})
		}
	}
}

func TestStrategyFreshPriceAndExplicitEvidence(t *testing.T) {
	legs, quotes := strategyQuoteReviewFixture()
	q, limit, notional, err := strategyPriceQuote("SYNTH", legs, quotes, nil)
	if err != nil || limit != 3 || notional != 1440 {
		t.Fatalf("live price: quote=%+v limit=%v notional=%v err=%v", q, limit, notional, err)
	}
	quotes[0].Stale = true
	quotes[0].StaleReason = "old first leg"
	quotes[0].DataType = rpc.MarketDataFrozen
	quotes[0].QuoteQuality = "indicative"
	quotes[0].SessionContext = &rpc.MarketSession{IsOpen: false, State: "closed"}
	quotes[0].Warnings = []rpc.DataWarning{{Code: "frozen_quote", Message: "Frozen first leg"}}
	quotes[1].Warnings = []rpc.DataWarning{{Code: "second_leg_warning", Message: "Second leg evidence"}}
	q, limit, _, err = strategyPriceQuote("SYNTH", legs, quotes, new(-1.25))
	if err != nil || limit != -1.25 || !q.Stale || q.StaleReason == "" || q.DataType != "mixed" || q.QuoteQuality == "firm" || !q.AsOf.Equal(quotes[0].AsOf) || !q.PriceAt.Equal(quotes[0].PriceAt) || q.SessionContext == nil || q.SessionContext.IsOpen || len(q.Warnings) != 2 {
		t.Fatalf("explicit price erased old or qualified leg evidence: quote=%+v limit=%v err=%v", q, limit, err)
	}
}
