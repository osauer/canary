package daemon

import (
	"testing"
	"time"
)

// Every embedded fund split, generated or hand-kept, carries a real date and
// source and spreads the fund over GICS sectors without exceeding the fund.
func TestFundLookThroughSplitsAreDatedGICSAndBounded(t *testing.T) {
	gics := map[string]bool{
		"Communication Services": true, "Consumer Discretionary": true, "Consumer Staples": true,
		"Energy": true, "Financials": true, "Health Care": true, "Industrials": true,
		"Information Technology": true, "Materials": true, "Real Estate": true, "Utilities": true,
	}
	for _, fund := range []string{"SPY", "QQQ", "IWM"} {
		if _, ok := fundLookThrough[fund]; !ok {
			t.Fatalf("%s: no look-through entry", fund)
		}
	}
	for fund, w := range fundLookThrough {
		if _, err := time.Parse(time.DateOnly, w.AsOf); err != nil {
			t.Errorf("%s: as-of %q is not a date: %v", fund, w.AsOf, err)
		}
		if w.Source == "" {
			t.Errorf("%s: source missing", fund)
		}
		var sum float64
		for sector, v := range w.Weights {
			if !gics[sector] {
				t.Errorf("%s: %q is not a GICS sector", fund, sector)
			}
			if v <= 0 {
				t.Errorf("%s: %s weight %v", fund, sector, v)
			}
			sum += v
		}
		// The residual below 100 is cash or derivatives; more than rounding
		// above 100 is a broken table. The floor mirrors the generator's guard.
		if sum < 99 || sum > 100.1 {
			t.Errorf("%s: sector weights sum to %.2f", fund, sum)
		}
	}
}
