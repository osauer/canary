package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
	"github.com/osauer/canary/v3/internal/marketcal"
)

// While the simulator holds a session open, the last completed session is
// still settled: its daily bar is kept, at any hour and on a weekend. The
// start check read "history stale" for eight minutes on 2026-11-28 because
// the simulated window answered for every earlier day too.
func TestTheLastCloseStaysSettledUnderASimulatedOpenSession(t *testing.T) {
	t.Cleanup(marketcal.ClearSimulatedSessions)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{
		time.Date(2026, 11, 28, 5, 56, 0, 0, ny), // a Saturday
		time.Date(2026, 11, 27, 14, 0, 0, 0, ny), // after an early close
		time.Date(2026, 11, 24, 11, 0, 0, 0, ny), // in an ordinary session
		time.Date(2026, 11, 30, 5, 0, 0, 0, ny),  // a Monday before the open
	} {
		marketcal.SimulateSession(marketcal.MarketUSEquity, marketcal.Window{Open: now.Add(-time.Hour), Close: now.Add(6*time.Hour + 30*time.Minute)})
		latest := spx.CompletedSessionKey(now)
		if settled := regimeGateSettledAt(latest, now); settled.IsZero() || now.Before(settled) {
			t.Errorf("%s: session %s reads as not settled until %s", now.Format("Mon 2006-01-02 15:04"), latest, settled.In(ny).Format("01-02 15:04"))
		}
	}
}
