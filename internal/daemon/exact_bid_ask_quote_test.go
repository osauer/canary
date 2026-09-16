package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestExactBidAskQuoteDoesNotInheritOldTradeStaleness(t *testing.T) {
	now := optionExitTestTime()
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNTH", SecType: "OPT", Currency: "USD"}
	q := &rpc.Quote{Symbol: contract.Symbol, Contract: contract, Bid: new(1.0), Ask: new(1.05), Last: new(1.1), Mark: new(1.08), TradeAt: now.Add(-time.Hour), PriceAt: now.Add(-time.Hour), DataType: rpc.MarketDataLive, AsOf: now}
	got := (&Server{}).exactBidAskQuoteSnapshot(q, contract, now.Add(-3*time.Second), now.Add(-2*time.Second), now.Add(-time.Second))
	if got.DataType != rpc.MarketDataLive || got.Stale || !got.PriceAt.Equal(now.Add(-2*time.Second)) {
		t.Fatalf("current live sides inherited last-trade staleness: type=%s stale=%t at=%s reason=%s", got.DataType, got.Stale, got.PriceAt, got.StaleReason)
	}
	if got.Last != q.Last || got.Mark != q.Mark || got.Bid != q.Bid || got.Ask != q.Ask {
		t.Fatal("snapshot lost distinct trade or quote values")
	}
	if !q.PriceAt.Equal(now.Add(-time.Hour)) || q.Last == nil {
		t.Fatal("snapshot mutated source trade evidence")
	}
	if got.SessionContext != nil && !got.SessionContext.IsOpen {
		t.Fatal("quote lost session authority")
	}
}

func TestExactBidAskQuoteDoesNotInventFreshness(t *testing.T) {
	now := optionExitTestTime()
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNTH", SecType: "OPT", Currency: "USD"}
	for _, feed := range []string{rpc.MarketDataFrozen, rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen, rpc.MarketDataUnknown, ""} {
		t.Run(feed, func(t *testing.T) {
			q := &rpc.Quote{Symbol: contract.Symbol, Contract: contract, Bid: new(1.0), Ask: new(1.05), DataType: feed, AsOf: now}
			got := (&Server{}).exactBidAskQuoteSnapshot(q, contract, now, now, now)
			if rpc.IsLiveDataType(got.DataType) || !got.PriceAt.IsZero() || !got.Stale {
				t.Fatalf("receipt promoted unavailable source freshness: type=%s stale=%t at=%s", got.DataType, got.Stale, got.PriceAt)
			}
			if (feed == rpc.MarketDataDelayed || feed == rpc.MarketDataDelayedFrozen) && got.DataType != feed {
				t.Fatal("lost broker delayed classification")
			}
		})
	}
	for name, stamp := range map[string]time.Time{"missing": {}, "future": now.Add(time.Second), "old": now.Add(-time.Hour)} {
		t.Run(name, func(t *testing.T) {
			q := &rpc.Quote{Symbol: contract.Symbol, Contract: contract, Bid: new(1.0), Ask: new(1.05), DataType: rpc.MarketDataLive, AsOf: now}
			got := (&Server{}).exactBidAskQuoteSnapshot(q, contract, now.Add(-2*time.Hour), stamp, now)
			if !got.Stale || rpc.IsLiveDataType(got.DataType) {
				t.Fatal("invalid or stale side became current live market")
			}
			if name == "old" && !got.PriceAt.Equal(stamp) {
				t.Fatal("old bid timestamp was replaced with newer ask or receipt clock")
			}
		})
	}
}

func TestExactQuoteWaitsForFeedNotice(t *testing.T) {
	q := &rpc.Quote{Bid: new(1.0), Ask: new(1.05)}
	for _, feed := range []string{"", rpc.MarketDataUnknown, "invented"} {
		q.DataType = feed
		if exactQuoteReady(q, true) || exactQuoteReady(q, false) {
			t.Fatal("prices before an authoritative feed notice completed the request")
		}
	}
	for _, feed := range []string{rpc.MarketDataLive, rpc.MarketDataFrozen, rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen} {
		q.DataType = feed
		if !exactQuoteReady(q, true) {
			t.Fatal("known feed notice did not finish a two-sided request")
		}
	}
	q.Ask = nil
	if exactQuoteReady(q, true) {
		t.Fatal("one-sided quote completed required bid/ask request")
	}
}

func TestExactBidAskQuoteRejectsCachedSidesAcrossRequestBoundary(t *testing.T) {
	now := optionExitTestTime()
	contract := rpc.ContractParams{ConID: 900001, Symbol: "SYNTH", SecType: "OPT", Currency: "USD"}
	for _, bidCached := range []bool{true, false} {
		bidAt, askAt := now, now
		if bidCached {
			bidAt = now.Add(-time.Second)
		} else {
			askAt = now.Add(-time.Second)
		}
		q := &rpc.Quote{Symbol: contract.Symbol, Contract: contract, Bid: new(1.0), Ask: new(1.05), DataType: rpc.MarketDataLive, AsOf: now}
		got := (&Server{}).exactBidAskQuoteSnapshot(q, contract, now, bidAt, askAt)
		if !got.Stale || !got.PriceAt.IsZero() || rpc.IsLiveDataType(got.DataType) {
			t.Fatal("a cached side was promoted by the other side's new receipt or the request clock")
		}
	}
}
