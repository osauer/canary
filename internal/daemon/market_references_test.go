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
