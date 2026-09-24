package daemon

import (
	"slices"
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
	out := projectDisplay(snapshot, holds, rpc.AccountDataScope{}, nil)
	q := out.Quotes[0]
	if q.Price == nil || *q.Price != 100 || q.PriceSource != "prev_close" || q.DataType != rpc.MarketDataDelayedFrozen || !q.TradeAt.IsZero() || q.PriceReceivedAt != received {
		t.Fatalf("bad delayed display: %+v", q)
	}
	rows := statusMarketDataAccess([]ibkr.MarketDataAbsenceError{{Key: "SYNTH|IND|NASDAQ", Code: 354, FallbackDataType: 4, FallbackReceivedAt: received}})
	if len(rows) != 1 || rows[0].FallbackDataType != rpc.MarketDataDelayedFrozen || rows[0].FallbackReceivedAt != received {
		t.Fatalf("bad recovery status: %+v", rows)
	}
}

// TestDelayedFeedQuoteIsNeverFirm witnesses an unentitled index served from
// IBKR's delayed-frozen fallback: without a session calendar nothing else
// marks it, so it used to reach consumers as a firm quote.
func TestDelayedFeedQuoteIsNeverFirm(t *testing.T) {
	hasWarning := func(q *rpc.Quote, code string) bool {
		return slices.ContainsFunc(q.WarningDetails, func(w rpc.DataWarning) bool { return w.Code == code })
	}
	index := &rpc.Quote{
		Symbol:    "SYNTH",
		Contract:  rpc.ContractParams{Symbol: "SYNTH", SecType: "IND", Exchange: "NASDAQ", Currency: "USD"},
		Mark:      new(30470.29296875),
		PrevClose: new(30470.29),
		DataType:  rpc.MarketDataDelayedFrozen,
		AsOf:      time.Date(2026, 9, 24, 12, 10, 0, 0, time.UTC),
	}
	new(Server).decorateQuote(index, marketcal.Market(""))
	if index.QuoteQuality != "indicative" || !index.Indicative || !hasWarning(index, "delayed_feed") {
		t.Fatalf("delayed-frozen index served as quality=%q indicative=%t warnings=%+v", index.QuoteQuality, index.Indicative, index.WarningDetails)
	}
	if !index.PriceAt.IsZero() || !index.QuotePriceAt.IsZero() || index.Change == nil {
		t.Fatalf("delayed mark invented a price clock or lost its broker change: at=%s quote_at=%s change=%v", index.PriceAt, index.QuotePriceAt, index.Change)
	}

	open := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	for _, feed := range []string{rpc.MarketDataDelayed, rpc.MarketDataLive} {
		q := &rpc.Quote{
			Symbol:    "SYNTH",
			Contract:  rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Currency: "USD"},
			Last:      new(100.0),
			PrevClose: new(99.0),
			TradeAt:   open.Add(-5 * time.Minute),
			PriceAt:   open.Add(-5 * time.Minute),
			DataType:  feed,
			AsOf:      open,
		}
		new(Server).decorateQuote(q, marketcal.MarketUSEquity)
		delayed := feed == rpc.MarketDataDelayed
		want := "firm"
		if delayed {
			want = "indicative"
		}
		if q.QuoteQuality != want || q.Indicative != delayed || hasWarning(q, "delayed_feed") != delayed {
			t.Fatalf("%s in-session stock: quality=%q indicative=%t warnings=%+v, want %s", feed, q.QuoteQuality, q.Indicative, q.WarningDetails, want)
		}
		if hasWarning(q, "off_hours_quote") {
			t.Fatalf("%s in-session stock called off-hours", feed)
		}
	}
}
