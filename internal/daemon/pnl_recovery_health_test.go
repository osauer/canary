package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPnLRepairNeedsValidNewFrameToClearHealth(t *testing.T) {
	var health dailyPnLObservationAuthority
	now := time.Now().UTC()
	check := func(snap ibkrlib.AccountDailyPnL, has bool, want rpc.DailyPnLObservationStatus) {
		t.Helper()
		got, err := health.observe(t.Context(), "synthetic-scope", "synthetic-session", now, true, snap, has)
		if err != nil || got.Status != want {
			t.Fatalf("status=%s error=%v want=%s", got.Status, err, want)
		}
	}
	check(ibkrlib.AccountDailyPnL{}, false, rpc.DailyPnLObservationMissing)
	now = now.Add(time.Second)
	check(ibkrlib.AccountDailyPnL{AsOf: now, DailyPnLStatus: ibkrlib.DailyPnLFrameUnavailable}, true, rpc.DailyPnLObservationMissing)
	check(ibkrlib.AccountDailyPnL{AsOf: now, DailyPnLStatus: ibkrlib.DailyPnLFrameMalformed}, true, rpc.DailyPnLObservationInvalid)
	value := 0.0
	now = now.Add(time.Second)
	check(ibkrlib.AccountDailyPnL{AsOf: now, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, DailyPnL: &value}, true, rpc.DailyPnLObservationOK)
}
