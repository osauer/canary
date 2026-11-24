package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A read answers within its budget however slow the history reads behind it
// are: what has not arrived is unavailable with a reason, and the slow reads
// go on without the caller. Synthetic broker; nothing is retained up front.
func TestMarketReadAnswersWithinItsBudgetWhileHistoryIsSlow(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	requested := make(chan rpc.MarketHistoryParams, 64)
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now },
		marketHistoryFetchForTest: func(ctx context.Context, p rpc.MarketHistoryParams, _ int, _ time.Time) (*rpc.MarketHistoryResult, error) {
			requested <- p
			select { // a paced broker that does not answer in time
			case <-release:
			case <-ctx.Done():
			}
			return nil, context.DeadlineExceeded
		}}
	start := time.Now()
	r, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > marketReadBudget+time.Second {
		t.Fatalf("the read took %s, over its %s budget", took, marketReadBudget)
	}
	if r.Index.PriorUnavailable == "" || r.Index.BarsUnavailable == "" || len(r.Leaders.Rows) == 0 || r.Leaders.Rows[0].PriorClose != nil {
		t.Errorf("missing inputs are not marked unavailable: %+v", r.Index)
	}
	// The slow reads were asked for, on the background lane, and a second
	// read inside the retry window does not ask again. The read records each
	// background request before it returns and sends it from its own
	// goroutine, so a first request can reach the broker after the read has
	// answered. Wait until every first request that can reach this broker
	// has: the bounded request lets as many through as it has slots, and
	// this broker never answers, so the rest wait for a slot. The baseline
	// seed, when the read starts one, reads outside the slots. Then drain,
	// then read again: a late first request is not a repeat.
	select {
	case <-requested: // the bounded request has made its slots by now
	case <-time.After(10 * time.Second):
		t.Fatal("no background history request was made")
	}
	s.marketRead.mu.Lock()
	recorded, seeded := len(s.marketRead.requested), s.marketRead.seeded != ""
	s.marketRead.mu.Unlock()
	s.marketData.mu.Lock()
	slots := cap(s.marketData.slots)
	s.marketData.mu.Unlock()
	expected := min(recorded, slots)
	if seeded {
		expected++
	}
	for seen := 1; seen < expected; seen++ {
		select {
		case <-requested:
		case <-time.After(10 * time.Second):
			t.Fatalf("%d of %d background history requests reached the broker", seen, expected)
		}
	}
	for len(requested) > 0 {
		<-requested
	}
	now = now.Add(marketReadCacheFor + time.Second)
	if _, err := s.handleMarketRead(t.Context(), &rpc.Request{}); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-requested:
		t.Errorf("a series already being read was asked for again: %s %s", p.Contract.Symbol, p.Range)
	case <-time.After(200 * time.Millisecond):
	}
}

// While a new read is in flight, callers get the last good read (its as_of
// says when) rather than waiting, as long as it is recent.
func TestMarketReadServesTheLastReadWhileAReadIsInFlight(t *testing.T) {
	now := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	first, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(marketReadCacheFor + time.Second)
	s.marketRead.mu.Lock()
	s.marketRead.flight = make(chan struct{}) // a read in flight
	s.marketRead.mu.Unlock()
	got, err := s.handleMarketRead(t.Context(), &rpc.Request{})
	if err != nil || got != first {
		t.Fatalf("did not serve the last read while one is in flight: %v", err)
	}
	now = now.Add(marketReadServeLastFor)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.handleMarketRead(ctx, &rpc.Request{}); err == nil {
		t.Error("a read older than the stated age was served")
	}
}

// Without a daily bar, a leader's prior close comes from the quote's previous
// close, and says so; its prior low stays unavailable.
func TestMarketReadLeaderPriorCloseFromTheQuote(t *testing.T) {
	m := newReadMarket(t)
	now := m.at(3)
	m.leader("AAA", new(102.0), new(101.8))
	m.src.Leaders[0].Daily = nil
	row := m.read(now).Leaders.Rows[0]
	if row.PriorClose == nil || *row.PriorClose != 100 || row.PriorCloseSource != "quote_previous_close" || row.PriorLow != nil {
		t.Fatalf("prior close from the quote: %+v", row)
	}
	m.leader("BBB", new(102.0), new(101.8))
	if row := m.read(now).Leaders.Rows[1]; row.PriorCloseSource != "daily_bar" {
		t.Errorf("prior close from the daily bar: %+v", row)
	}
}

// Before the open the index's last price is the prior close; it carries the
// prior session's close time, never the time it was read.
func TestMarketReadPriorCloseIsNotStampedWithTheReadTime(t *testing.T) {
	m := newReadMarket(t)
	before := m.session.Open.Add(-time.Hour)
	m.src.SPXQuote = &broker.BriefQuote{Last: 95, Close: 95, ObservedAt: before}
	r := m.read(before)
	if r.Index.Last == nil || !r.Index.LastAt.Equal(m.prior.Close) || r.Index.LastSource != "prior_close" {
		t.Errorf("pre-market index last: %v at %s (%s)", r.Index.Last, r.Index.LastAt, r.Index.LastSource)
	}
	// A trade time from the broker is carried as it is.
	traded := m.session.Open.Add(10 * time.Minute)
	m.src.SPXQuote = &broker.BriefQuote{Last: 96, Close: 95, ObservedAt: traded.Add(time.Minute), TradeAt: traded}
	if r := m.read(m.at(3)); !r.Index.LastAt.Equal(traded) || r.Index.LastSource != "trade" {
		t.Errorf("index last with a trade time: %s (%s)", r.Index.LastAt, r.Index.LastSource)
	}
}

// A short SPY record is seeded once per session from one full read of the
// padded window; nothing recorded is removed.
func TestMarketReadSeedsTheVolumeBaselineOnce(t *testing.T) {
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	ny, _ := time.LoadLocation("America/New_York")
	day := func(d, h, m int) time.Time { return time.Date(2026, 9, d, h, m, 0, 0, ny) }
	fetches := make(chan int, 8)
	s := &Server{coreStore: store, marketHistoryFetchForTest: func(_ context.Context, p rpc.MarketHistoryParams, tail int, now time.Time) (*rpc.MarketHistoryResult, error) {
		fetches <- tail
		r := &rpc.MarketHistoryResult{Contract: rpc.ContractParams{ConID: 7, Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
			Range: "1D", Interval: "5 mins", TimestampKind: "instant", PriceBasis: "TRADES", AsOf: now}
		for _, d := range []int{17, 18, 21, 22} {
			r.Points = append(r.Points, rpc.MarketHistoryPoint{At: day(d, 9, 30), Value: 500, Volume: new(int64(100))})
		}
		r.RequestedStart = r.Points[0].At
		r.Start, r.End = r.Points[0].At, r.Points[len(r.Points)-1].At
		return r, nil
	}}
	p := rpc.MarketHistoryParams{Contract: marketReadSPY, Range: "1D"}
	if err := s.seedIntradayHistory(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if tail := <-fetches; tail != 0 {
		t.Errorf("seed read the tail (%d days), not the whole window", tail)
	}
	got := s.retainedHistory(t.Context(), p)
	if got == nil || len(got.Points) != 4 {
		t.Fatalf("seeded record: %+v", got)
	}
	s.seedMarketReadBaseline("2026-09-23")
	s.seedMarketReadBaseline("2026-09-23")
	select {
	case <-fetches:
	case <-time.After(2 * time.Second):
		t.Fatal("no seed for the session")
	}
	select {
	case <-fetches:
		t.Error("seeded twice in one session")
	case <-time.After(200 * time.Millisecond):
	}
}
