package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A quote read waits for the rest of a new line's opening messages, and only
// then: a complete quote, a quiet line and an instrument that never sends the
// missing field must not pay for it.
func TestQuoteReadWaitsOnlyForAnOpeningBurst(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 0, 0, 0, time.UTC)
	price := func(v float64) *float64 { return &v }
	const budget = 3 * time.Second
	for _, tc := range []struct {
		name           string
		secType        string
		bid, ask, last *float64
		tickAge        time.Duration
		want           bool
	}{
		{"no price yet", "STK", nil, nil, nil, 0, false},
		{"stock with bid, ask and last", "STK", price(109.99), price(110.01), price(110), 0, true},
		{"stock with a lone bid just received", "STK", price(109.99), nil, nil, 10 * time.Millisecond, false},
		{"stock with both sides just received", "STK", price(109.99), price(110.01), nil, 10 * time.Millisecond, false},
		{"stock with a lone last just received", "STK", nil, nil, price(110), 10 * time.Millisecond, false},
		{"stock with a lone bid on a quiet line", "STK", price(109.99), nil, nil, time.Minute, true},
		{"currency pair with both sides just received", "CASH", price(1.0841), price(1.0842), nil, 10 * time.Millisecond, true},
		{"currency pair with a lone bid just received", "CASH", price(1.0841), nil, nil, 10 * time.Millisecond, false},
		{"index with a last just received", "IND", nil, nil, price(5900), 10 * time.Millisecond, true},
	} {
		q := &rpc.Quote{Contract: rpc.ContractParams{SecType: tc.secType}, Bid: tc.bid, Ask: tc.ask, Last: tc.last}
		d := &ibkrlib.MarketData{LastPriceTickAt: now.Add(-tc.tickAge)}
		var burst quoteBurst
		if got := burst.settled(q, d, now, budget); got != tc.want {
			t.Errorf("%s: settled=%v, want %v", tc.name, got, tc.want)
		}
	}

	// The wait is counted from the first price seen and ends: later ticks on a
	// busy line do not extend it, and a short read keeps half its budget.
	q := &rpc.Quote{Contract: rpc.ContractParams{SecType: "STK"}, Bid: price(109.99)}
	var burst quoteBurst
	if burst.settled(q, &ibkrlib.MarketData{LastPriceTickAt: now}, now, budget) {
		t.Fatal("a lone bid settled the moment it arrived")
	}
	later := now.Add(quoteBurstSettle)
	if !burst.settled(q, &ibkrlib.MarketData{LastPriceTickAt: later}, later, budget) {
		t.Fatal("a line that kept ticking extended the wait past its bound")
	}
	var short quoteBurst
	if !short.settled(q, &ibkrlib.MarketData{LastPriceTickAt: now}, now.Add(100*time.Millisecond), 200*time.Millisecond) {
		t.Fatal("a 200 ms read waited more than half its budget for the rest of the quote")
	}
}
