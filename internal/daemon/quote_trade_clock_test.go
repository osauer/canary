package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestQuoteTradeClockCannotBecomeAReceiptClock(t *testing.T) {
	received := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	traded := received.Add(-16 * time.Hour)
	q := &rpc.Quote{AsOf: received, Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Currency: "USD"}}
	fillQuoteMarketData(q, &ibkrlib.MarketData{Last: 123, LastTradeTime: traded})
	new(Server).decorateQuote(q, marketcal.MarketUSEquity)
	if !q.TradeAt.Equal(traded) {
		t.Fatal("quote decoration replaced the broker trade clock with local time")
	}
	q.AsOf = received.Add(time.Hour)
	fillQuoteMarketData(q, &ibkrlib.MarketData{Last: 123})
	new(Server).decorateQuote(q, marketcal.MarketUSEquity)
	if !q.TradeAt.IsZero() {
		t.Fatal("missing broker trade time inherited an earlier trade or receipt clock")
	}
}

func TestQuoteTradePhaseBelongsToTradeNotWeekendReceipt(t *testing.T) {
	contract := rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "NYSE"}
	for _, tt := range []struct{ at, phase string }{
		{"2026-09-11T12:00:00Z", "pre_market"},
		{"2026-09-11T15:00:00Z", "regular"},
		{"2026-09-11T23:00:00Z", "post_market"},
		{"2026-11-27T18:30:00Z", "post_market"},
		{"2026-03-09T13:00:00Z", "pre_market"},
	} {
		at, err := time.Parse(time.RFC3339, tt.at)
		if err != nil {
			t.Fatal(err)
		}
		q := &rpc.Quote{Contract: contract, TradeAt: at, AsOf: time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)}
		new(Server).attachQuoteSessionContext(q, marketcal.MarketUSEquity)
		if q.TradePhase != tt.phase {
			t.Fatalf("trade %s: got %s, want %s", tt.at, q.TradePhase, tt.phase)
		}
		q.TradeAt = time.Time{}
		new(Server).attachQuoteSessionContext(q, marketcal.MarketUSEquity)
		if q.TradePhase != "" {
			t.Fatal("missing trade clock inherited a previous phase")
		}
	}
	contract.SecType = "FUT"
	if quoteTradePhase(contract, time.Now()) != "" {
		t.Fatal("futures inherited a cash-equity session")
	}
}
