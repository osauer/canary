package daemon

import (
	"github.com/osauer/canary/v3/internal/marketcal"
	"testing"
	"time"
)

func TestMandateHKLastCoveredDayAfterClose(t *testing.T) {
	now, _ := time.Parse(time.RFC3339, "2026-12-31T14:00:00+08:00")
	session, e := marketcal.NewWithClock(func() time.Time { return now }).SessionAt(marketcal.MarketHKHKEX, now)
	if e != nil {
		t.Fatal(e)
	}
	last, previous, ok := mandateLastSessionsIn(now, marketcal.MarketHKHKEX)
	t.Logf("current official calendar session=%s state=%s close=%s now=%s; mandate last=%s previous=%s covered=%v", session.Date, session.State, session.Close, now, last, previous, ok)
	if !ok || last != "2026-12-31" {
		t.Fatal("completed covered HK session not found")
	}
}
