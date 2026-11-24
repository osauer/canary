package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/regimerows"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A HYG price at the bottom of its trailing year is said in the credit row,
// and the row's band is the one the 50-day rule gives either way. Synthetic
// closes only.
func TestHYGAtItsYearLowIsSaidWithoutChangingTheBand(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 3, 0, 0, 0, ny) // off-RTH: the latest daily close bands
	read := func(last float64) rpc.RegimeHYGSPYDivergence {
		bars := make([]ibkrlib.HistoricalBar, 0, 360)
		for i := range 360 {
			d := now.AddDate(0, 0, i-360)
			bars = append(bars, ibkrlib.HistoricalBar{Date: d.Format("2006-01-02"), Time: d, Close: 80 - float64(i)/100, High: 81})
		}
		bars[len(bars)-1].Close = last
		var warns []string
		deps := hygSpotMissDeps(now, &warns)
		deps.history = func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) { return bars, nil }
		return fetchRegimeHYGSPY(context.Background(), deps)
	}
	low := read(75)
	if low.HYGRange52W == nil || low.HYGRange52W.Pos != 0 || !low.HYGAt52WLow {
		t.Fatalf("HYG at the year's lowest close: range %+v, at low %v", low.HYGRange52W, low.HYGAt52WLow)
	}
	if row := regimerows.HYGSPY(now, low); !strings.Contains(row.Value, "at 52w low") {
		t.Errorf("credit row does not say HYG is at its 52-week low: %q", row.Value)
	}
	above := read(79)
	if above.HYGAt52WLow || above.HYGRange52W == nil || above.HYGRange52W.Pos == 0 {
		t.Fatalf("HYG above its low read as at the low: %+v", above.HYGRange52W)
	}
	if row := regimerows.HYGSPY(now, above); strings.Contains(row.Value, "at 52w low") {
		t.Errorf("credit row claims a 52-week low: %q", row.Value)
	}
	unflagged := low
	unflagged.HYGAt52WLow = false
	if got, want := regimerows.HYGSPY(now, low).Band, regimerows.HYGSPY(now, unflagged).Band; got != want {
		t.Errorf("the 52-week low changed the band: %v, want %v", got, want)
	}
	monitor := func(r rpc.RegimeHYGSPYDivergence) string {
		m := rpc.CompactRegimeMonitor(&rpc.RegimeSnapshotResult{HYGSPYDivergence: r})
		for _, row := range m.Indicators {
			if row.Name == "HYG/SPY" {
				return row.Reading
			}
		}
		t.Fatal("no HYG/SPY monitor row")
		return ""
	}
	if !strings.Contains(monitor(low), "at 52w low") || strings.Contains(monitor(above), "at 52w low") {
		t.Errorf("monitor reading: at low %q, above %q", monitor(low), monitor(above))
	}
}
