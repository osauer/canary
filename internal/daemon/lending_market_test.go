package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
	"time"
)

func lendingMarketFixture(t *testing.T) (rpc.ContractParams, *rpc.MarketHistoryResult, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	c := rpc.ContractParams{Symbol: "AAA", ConID: 91001, SecType: "STK", Currency: "USD", Exchange: "SMART"}
	h := &rpc.MarketHistoryResult{Contract: c, Interval: "1 day", TimestampKind: "session_date", PriceBasis: "TRADES", AsOf: now}
	h.Points = append(h.Points, rpc.MarketHistoryPoint{At: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), Value: 10, Volume: new(int64(1000000))})
	sessions, err := tapeArchiveCalendar(now.AddDate(0, 0, -370), 372)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.Close.After(now) {
			continue
		}
		at, _ := time.Parse(time.DateOnly, s.Date)
		v := int64(1000000)
		price := 10.0
		if at.Year() == 2026 {
			price = 20
		}
		h.Points = append(h.Points, rpc.MarketHistoryPoint{At: at, Value: price, Volume: &v})
	}
	return c, h, now
}
func TestLendingMarketCompletedDatesAndGaps(t *testing.T) {
	c, h, now := lendingMarketFixture(t)
	r := projectLendingMarket(c, h, nil, now)
	if r.Status != "available" || *r.AvgDollarVolume20D != 20000000 || *r.YTDChangePct != 100 || r.YTDBaseDate != "2025-12-31" || r.VolumeDate != "2026-10-02" || r.PriceKind != "close" {
		t.Fatalf("wrong projection: %+v", r)
	}
	if err := rpc.ValidateLendingMarketResult(rpc.LendingMarketResult{Kind: "lending_market", Symbols: []string{"AAA"}, Rows: []rpc.LendingMarketRow{r}}, []string{"AAA"}); err != nil {
		t.Fatal(err)
	}
	h.Points[len(h.Points)-3].Volume = nil
	if r = projectLendingMarket(c, h, nil, now); r.AvgDollarVolume20D != nil || r.Price == nil {
		t.Fatal("missing volume must withhold liquidity only")
	}
	h.Points = h.Points[30:]
	for i, p := range h.Points {
		if p.At.Format(time.DateOnly) == "2025-12-31" {
			h.Points = append(h.Points[:i], h.Points[i+1:]...)
			break
		}
	}
	if r = projectLendingMarket(c, h, nil, now); r.YTDChangePct != nil {
		t.Fatal("must not substitute first YTD price for year-end baseline")
	}
	h.Contract.ConID++
	if r = projectLendingMarket(c, h, nil, now); r.Price != nil {
		t.Fatal("wrong contract accepted")
	}
}
func TestLendingMarketIntradayAndFrozenFallback(t *testing.T) {
	c, h, now := lendingMarketFixture(t)
	q := &rpc.Quote{Contract: c, Last: new(21.0), PrevClose: new(20.0), TradeAt: now.Add(-time.Minute), DataType: rpc.MarketDataDelayed, Volume: new(int64(300000))}
	r := projectLendingMarket(c, h, q, now)
	if r.PriceKind != "delayed" || *r.Price != 21 || r.DayChangePct == nil || *r.DayChangePct < 4.99 || *r.YTDChangePct != 100 {
		t.Fatal("intraday context changed completed-close YTD")
	}
	q.TradeAt = now.Add(-time.Hour)
	if r = projectLendingMarket(c, h, q, now); r.PriceKind != "close" || *r.Volume != 1000000 {
		t.Fatal("old trade masqueraded as intraday")
	}
	q.TradeAt = now.Add(time.Minute)
	if r = projectLendingMarket(c, h, q, now); r.PriceKind != "close" {
		t.Fatal("future trade accepted")
	}
	h.Points = h.Points[:len(h.Points)-1]
	if r = projectLendingMarket(c, h, nil, now); r.Price != nil || r.AvgDollarVolume20D != nil {
		t.Fatal("missing final session must not become current data")
	}
}

func TestLendingMarketBoundedInterestIdentityAndExpiry(t *testing.T) {
	c, _, now := lendingMarketFixture(t)
	s := &Server{now: func() time.Time { return now }}
	s.marketEvents = offlineMarketEventCache(&now)
	s.marketEvents.borrowFees = marketEventBorrowFeeEntry{AsOf: now.Add(-38 * time.Hour), FetchedAt: now, Symbols: map[string]marketEventBorrowFeeRecord{"AAA": {Symbol: "AAA", ConID: "91001", Currency: "USD"}}}
	s.lendingMarket.entries = map[string]lendingMarketEntry{}
	for i := range lendingMarketCapacity {
		s.lendingMarket.entries[fmt.Sprint(i)] = lendingMarketEntry{until: now.Add(time.Minute)}
	}
	raw, _ := json.Marshal(rpc.LendingMarketParams{Symbols: []string{"AAA"}})
	read := func() rpc.LendingMarketRow {
		t.Helper()
		r, err := s.handleLendingMarket(t.Context(), &rpc.Request{Params: raw})
		if err != nil {
			t.Fatal(err)
		}
		return r.Rows[0]
	}
	if r := read(); r.Status != "pending" || len(s.lendingMarket.entries) != lendingMarketCapacity {
		t.Fatal("capacity was not bounded")
	}
	s.lendingMarket.entries["0"] = lendingMarketEntry{until: now.Add(-time.Second)}
	if r := read(); r.Status != "pending" || r.Contract != c || len(s.lendingMarket.entries) != lendingMarketCapacity {
		t.Fatal("expired interest was not reclaimed")
	}
	s.lendingMarket.entries["AAA"] = lendingMarketEntry{contract: c, until: now.Add(time.Minute), row: rpc.LendingMarketRow{Status: "available", Price: new(20.0), ValidUntil: now.Add(-time.Second)}}
	if r := read(); r.Price != nil || r.Status != "pending" {
		t.Fatal("expired financial fields served")
	}
	rec := s.marketEvents.borrowFees.Symbols["AAA"]
	rec.ConID = "91002"
	s.marketEvents.borrowFees.Symbols["AAA"] = rec
	if r := read(); r.Contract.ConID != 91002 || r.Price != nil {
		t.Fatal("contract identity change retained prior evidence")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.startLendingMarketRefresh(ctx)
	done := make(chan struct{})
	go func() { s.marketData.loopWG.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("market worker did not join on shutdown")
	}
}
