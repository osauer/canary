package ibkr

import "testing"

// A bare RUT request classifies as the Russell 2000 index on its listing
// venue, not as a SMART-routed stock or a CBOE index (#46).
func TestClassifySymbolRoutesTheRussell2000Index(t *testing.T) {
	secType, exchange, currency, primary := classifySymbol("RUT")
	if secType != "IND" || exchange != "RUSSELL" || currency != "USD" || primary != "RUSSELL" {
		t.Fatalf("classifySymbol(RUT) = %s %s %s %s, want IND RUSSELL USD RUSSELL", secType, exchange, currency, primary)
	}
	if secType, exchange, _, _ := classifySymbol("NDX"); secType != "IND" || exchange != "NASDAQ" {
		t.Fatalf("classifySymbol(NDX) = %s %s, want IND NASDAQ", secType, exchange)
	}
}
