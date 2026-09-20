package rpc

import "testing"

// A benchmark index is described on the venue IBKR lists the index itself
// on, not where its options trade: a RUT index described on CBOE drew "no
// security definition" from the broker on every read (#46).
func TestIndexUnderlyingContractsUseTheListingVenue(t *testing.T) {
	for _, tc := range []struct{ symbol, exchange string }{
		{"SPX", "CBOE"}, {"NDX", "NASDAQ"}, {"RUT", "RUSSELL"}, {"VIX", "CBOE"},
	} {
		c := fallbackUnderlyingQuoteContract(tc.symbol, "USD")
		if c.SecType != "IND" || c.Exchange != tc.exchange || c.PrimaryExch != tc.exchange || c.Currency != "USD" {
			t.Errorf("%s = %+v, want IND on %s", tc.symbol, c, tc.exchange)
		}
	}
	if c := fallbackUnderlyingQuoteContract("AAPL", "USD"); c.SecType != "STK" || c.Exchange != "SMART" {
		t.Errorf("a stock still routes SMART: %+v", c)
	}
}
