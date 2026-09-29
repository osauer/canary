package daemon

import "testing"

// The Russell 2000 reference is an index on the RUSSELL venue; described on
// CBOE it never resolved at the broker (#46).
func TestMarketReferencesRouteTheRussell2000ToItsListingVenue(t *testing.T) {
	found := false
	for _, ref := range marketReferences() {
		if ref.Quote == nil || ref.Quote.Contract.Symbol != "RUT" {
			continue
		}
		found = true
		if c := ref.Quote.Contract; c.SecType != "IND" || c.Exchange != "RUSSELL" {
			t.Fatalf("RUT reference = %+v, want IND on RUSSELL", c)
		}
	}
	if !found {
		t.Fatal("no RUT market reference")
	}
}

// SPY and QQQ quote pre-market while SPX and NDX are frozen; they carry their
// own keys so no index row is ever replaced by its ETF.
func TestMarketReferencesQuoteSPYAndQQQUnderTheirOwnKeys(t *testing.T) {
	want := map[string]string{"SPY": "spy", "QQQ": "qqq"}
	for _, ref := range marketReferences() {
		key, ok := want[ref.Quote.Contract.Symbol]
		if !ok {
			continue
		}
		if c := ref.Quote.Contract; ref.Key != key || ref.Kind != "etf" || c.SecType != "STK" || c.Exchange != "SMART" || c.Currency != "USD" {
			t.Fatalf("%s reference = %+v, want key %s, STK on SMART", c.Symbol, ref, key)
		}
		delete(want, ref.Quote.Contract.Symbol)
	}
	if len(want) != 0 {
		t.Fatalf("missing market references: %v", want)
	}
}
