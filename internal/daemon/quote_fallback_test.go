package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDelayedQuoteKeepsProvenanceAndSourceTime(t *testing.T) {
	now := time.Date(2026, 9, 15, 16, 0, 0, 0, time.UTC)
	price := 100.0
	for _, kind := range []string{rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen} {
		q := &rpc.Quote{Price: &price, Last: &price, PriceSource: "last", DataType: kind, Stale: true, AsOf: now}
		if got := quoteEffectiveDataType(q, marketcal.Market(""), kind); got != kind {
			t.Fatalf("delayed origin became %q", got)
		}
		if at := quotePriceTimeForSource(q, "last", &price, marketcal.Market("")); !at.IsZero() {
			t.Fatal("receipt time became broker price time")
		}
		q.TradeAt = now.Add(-20 * time.Minute)
		if at := quotePriceTimeForSource(q, "last", &price, marketcal.Market("")); !at.Equal(q.TradeAt) {
			t.Fatal("lost delayed broker timestamp")
		}
		if at := quotePriceTimeForSource(q, "bid", &price, marketcal.Market("")); !at.IsZero() {
			t.Fatal("delayed bid invented source time")
		}
	}
	if got := quoteDataTypeName(0, true, false); got != rpc.MarketDataUnknown || rpc.IsLiveDataType(got) {
		t.Fatalf("unknown quote called executable live: %q", got)
	}
}
func TestDelayedCloseSuppliesDisplayAndRecoveryStatus(t *testing.T) {
	received := time.Date(2026, 9, 15, 5, 34, 0, 0, time.UTC)
	md := &ibkr.MarketData{Close: 100, CloseAt: received, FeedType: 4}
	snapshot := ibkr.DisplaySnapshot{Quotes: map[string]*ibkr.MarketData{"key": md}, DataTypes: map[string]int{"key": 4}}
	holds := []displayHold{{item: displayInstrument{contract: ibkr.Contract{Symbol: "SYNTH", SecType: "IND", Currency: "USD"}}, cacheKey: "key"}}
	out := projectDisplay(snapshot, holds, rpc.AccountDataScope{})
	q := out.Quotes[0]
	if q.Price == nil || *q.Price != 100 || q.PriceSource != "prev_close" || q.DataType != rpc.MarketDataDelayedFrozen || !q.TradeAt.IsZero() || q.PriceReceivedAt != received {
		t.Fatalf("bad delayed display: %+v", q)
	}
	rows := statusMarketDataAccess([]ibkr.MarketDataAbsenceError{{Key: "SYNTH|IND|NASDAQ", Code: 354, FallbackDataType: 4, FallbackReceivedAt: received}})
	if len(rows) != 1 || rows[0].FallbackDataType != rpc.MarketDataDelayedFrozen || rows[0].FallbackReceivedAt != received {
		t.Fatalf("bad recovery status: %+v", rows)
	}
}
