package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// After the close IBKR's close tick on a US cash index is that day's own
// close, so the index read +0.00 against itself (VIX 18.39, 0.00%, on
// 2026-10-27). Its regular close and the close before it come from the
// daily bars, as an ETF's do.
func TestCashIndexAfterTheCloseCarriesItsSessionChange(t *testing.T) {
	last, sameDay := 7676.61, 7676.77
	closedAt := time.Date(2026, 10, 27, 5, 0, 0, 0, time.UTC)
	q := &rpc.Quote{Symbol: "SPX", Contract: rpc.ContractParams{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"},
		Last: &last, PrevClose: &sameDay, AsOf: closedAt}

	market, ok := quoteHistoryMarket(q, "", false)
	if !ok || market != marketcal.MarketUSEquity {
		t.Fatalf("a closed US cash index needs the US equity daily bars, got %q %v", market, ok)
	}
	applyQuoteHistoricalFallback(q, market, []ibkrlib.HistoricalBar{
		{Date: "20261023", Close: 7600}, {Date: "20261026", Close: 7676.61},
	})
	(&Server{}).decorateQuote(q, "")
	if q.RegularClose == nil || *q.RegularClose != 7676.61 || q.PriorRegularClose == nil || *q.PriorRegularClose != 7600 {
		t.Fatalf("regular close %v, prior %v; want 7676.61 after 7600", q.RegularClose, q.PriorRegularClose)
	}
	if q.RegularChange == nil || *q.RegularChange < 76.6 || *q.RegularChange > 76.62 {
		t.Fatalf("regular change %v, want +76.61 against the prior session", q.RegularChange)
	}
	if want := time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC); !q.RegularCloseAt.Equal(want) {
		t.Fatalf("regular close at %v, want the session's close %v", q.RegularCloseAt, want)
	}
}

func TestCashIndexHistoryOnlyWhenTheUSMarketIsClosed(t *testing.T) {
	index := rpc.ContractParams{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	last := 7676.61
	open := &rpc.Quote{Contract: index, Last: &last, AsOf: time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)}
	if _, ok := quoteHistoryMarket(open, "", false); ok {
		t.Fatal("an open session's index quote is current; no daily bars")
	}
	closed := &rpc.Quote{Contract: index, Last: &last, AsOf: time.Date(2026, 10, 27, 5, 0, 0, 0, time.UTC)}
	known := 7676.61
	closed.RegularClose = &known
	if _, ok := quoteHistoryMarket(closed, "", false); ok {
		t.Fatal("a regular close already supplied is not read again")
	}
	foreign := &rpc.Quote{Contract: rpc.ContractParams{Symbol: "DAX", SecType: "IND", Exchange: "EUREX", Currency: "EUR"}, Last: &last, AsOf: closed.AsOf}
	if _, ok := quoteHistoryMarket(foreign, "", false); ok {
		t.Fatal("a non-USD index has no US equity close")
	}
}
