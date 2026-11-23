package daemon

import (
	"testing"

	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The broker's close tick is the previous regular session's close. Just after
// the bell it is a full session old, so it must never anchor a gamma profile:
// with no current price the spot is unavailable and the phase is refused.
func TestGammaSpotIsNeverThePreviousClose(t *testing.T) {
	if got := gammaSpotFromQuote(ibkrlib.MarketData{Close: 7175.83}); got != 0 {
		t.Fatalf("spot from a quote holding only the previous close = %v, want 0 (unavailable)", got)
	}
	cases := []struct {
		name  string
		quote ibkrlib.MarketData
		want  float64
	}{
		{"last wins", ibkrlib.MarketData{Last: 6918.45, Bid: 6917, Ask: 6919, MarkPrice: 6918, Close: 7175.83}, 6918.45},
		{"mid without last", ibkrlib.MarketData{Bid: 6917, Ask: 6919, Close: 7175.83}, 6918},
		{"mark for an index", ibkrlib.MarketData{MarkPrice: 6910.39, Close: 7175.83}, 6910.39},
	}
	for _, tc := range cases {
		if got := gammaSpotFromQuote(tc.quote); got != tc.want {
			t.Errorf("%s: spot = %v, want %v", tc.name, got, tc.want)
		}
	}
}
