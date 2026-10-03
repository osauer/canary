package daemon

import (
	"math"
	"testing"
	"time"

	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestSetupOptionListingsAreBoundedAndNeverInvented(t *testing.T) {
	now := time.Date(2026, 10, 2, 16, 0, 0, 0, time.UTC)
	listing := map[string][]ibkr.ExpiryClassedStrikes{}
	for i := -1; i < 70; i++ {
		listing[now.AddDate(0, 0, i).Format("2006-01-02")] = []ibkr.ExpiryClassedStrikes{{TradingClass: "SYNTH", Strikes: []float64{99, 100, 101}}}
	}
	expiries, truncated := setupOptionExpiries(listing, now)
	if len(expiries) != 64 || !truncated || expiries[0].Date != "20261002" {
		t.Fatal(expiries, truncated)
	}
	strikes, err := setupListedStrikes(listing, "SYNTH", "20261002")
	if err != nil || len(strikes) != 3 {
		t.Fatal(strikes, err)
	}
	if _, err := setupListedStrikes(nil, "SYNTH", "20261002"); err == nil {
		t.Fatal("missing listing invented a grid")
	}
	listing["2026-10-02"][0].TradingClass = "SYNTH1"
	if _, err := setupListedStrikes(listing, "SYNTH", "20261002"); err == nil {
		t.Fatal("adjusted class accepted")
	}
	listing["2026-10-02"] = []ibkr.ExpiryClassedStrikes{{TradingClass: "SYNTH", Strikes: []float64{math.NaN()}}}
	if _, err := setupListedStrikes(listing, "SYNTH", "20261002"); err == nil {
		t.Fatal("nonfinite strike accepted")
	}
}
