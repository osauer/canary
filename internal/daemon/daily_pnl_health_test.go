package daemon

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDailyPnLObservationFailureSurvivesCloseUntilRecovery(t *testing.T) {
	authority := dailyPnLObservationAuthority{}
	sessionKey := "2026-07-21"
	duringRTH := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)

	failed, err := authority.observe(t.Context(), "paper|DU123", sessionKey, duringRTH, true, ibkrlib.AccountDailyPnL{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != rpc.DailyPnLObservationMissing {
		t.Fatalf("RTH observation = %+v, want missing", failed)
	}
	afterClose := duringRTH.Add(3 * time.Hour)
	stillFailed, err := authority.observe(t.Context(), "paper|DU123", sessionKey, afterClose, false, ibkrlib.AccountDailyPnL{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if stillFailed.Status != rpc.DailyPnLObservationMissing || !stillFailed.AsOf.Equal(failed.AsOf) {
		t.Fatalf("post-close observation = %+v, want retained failure %+v", stillFailed, failed)
	}

	daily := 12.5
	recoveredAt := afterClose.Add(time.Minute)
	recovered, err := authority.observe(t.Context(), "paper|DU123", sessionKey, recoveredAt, false, ibkrlib.AccountDailyPnL{
		DailyPnL: &daily, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: recoveredAt,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Status != rpc.DailyPnLObservationOK {
		t.Fatalf("recovered observation = %+v, want ok", recovered)
	}
}

func TestDailyPnLObservationCleanAfterHoursIsNotDue(t *testing.T) {
	authority := dailyPnLObservationAuthority{}
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	got, err := authority.observe(t.Context(), "paper|DU123", "2026-07-21", now, false, ibkrlib.AccountDailyPnL{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != rpc.DailyPnLObservationNotDue {
		t.Fatalf("observation = %+v, want not_due", got)
	}
}

func TestDailyPnLObservationMalformedFrameDegrades(t *testing.T) {
	authority := dailyPnLObservationAuthority{}
	now := time.Date(2026, 7, 21, 21, 0, 0, 0, time.UTC)
	got, err := authority.observe(t.Context(), "paper|DU123", "2026-07-21", now, false, ibkrlib.AccountDailyPnL{
		DailyPnLStatus: ibkrlib.DailyPnLFrameMalformed, AsOf: now,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != rpc.DailyPnLObservationInvalid {
		t.Fatalf("observation = %+v, want invalid", got)
	}
}

func TestDailyPnLObservationDoesNotCrossSourceOrSession(t *testing.T) {
	tests := []struct {
		name        string
		nextSource  string
		nextSession string
	}{
		{name: "source change", nextSource: "live|DU456", nextSession: "2026-07-21"},
		{name: "session change", nextSource: "paper|DU123", nextSession: "2026-07-22"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authority := dailyPnLObservationAuthority{}
			now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
			if got, err := authority.observe(t.Context(), "paper|DU123", "2026-07-21", now, true, ibkrlib.AccountDailyPnL{}, false); err != nil || got.Status != rpc.DailyPnLObservationMissing {
				t.Fatalf("seed failure = %+v err=%v", got, err)
			}
			got, err := authority.observe(t.Context(), test.nextSource, test.nextSession, now.Add(3*time.Hour), false, ibkrlib.AccountDailyPnL{}, false)
			if err != nil || got.Status != rpc.DailyPnLObservationNotDue {
				t.Fatalf("next scope/session = %+v err=%v, want not_due", got, err)
			}
		})
	}
}

func TestDailyPnLObservationFailureSurvivesDaemonStoreRestart(t *testing.T) {
	databasePath := filepath.Join(privateTestDir(t), "daemon.db")
	store, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	source := "paper|DU123"
	sessionKey := "2026-07-21"
	duringRTH := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	first := dailyPnLObservationAuthority{}
	if err := first.bindCore(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	failed, err := first.observe(t.Context(), source, sessionKey, duringRTH, true, ibkrlib.AccountDailyPnL{}, false)
	if err != nil || failed.Status != rpc.DailyPnLObservationMissing {
		t.Fatalf("persist failure = %+v err=%v", failed, err)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, dailyPnLObservationStateKind)
	if err != nil || !ok {
		t.Fatalf("persisted failure document missing: ok=%v err=%v", ok, err)
	}
	if bytes.Contains(doc.JSON, []byte("DU123")) {
		t.Fatalf("persisted health record exposed account identity: %s", doc.JSON)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := corestore.Open(t.Context(), corestore.Options{Path: databasePath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restartedStore.Close() })
	restarted := dailyPnLObservationAuthority{}
	if err := restarted.bindCore(t.Context(), restartedStore); err != nil {
		t.Fatal(err)
	}
	afterClose := duringRTH.Add(3 * time.Hour)
	stillFailed, err := restarted.observe(t.Context(), source, sessionKey, afterClose, false, ibkrlib.AccountDailyPnL{}, false)
	if err != nil || stillFailed.Status != rpc.DailyPnLObservationMissing || !stillFailed.AsOf.Equal(failed.AsOf) {
		t.Fatalf("restarted observation = %+v err=%v, want retained %+v", stillFailed, err, failed)
	}

	daily := 12.5
	recoveredAt := afterClose.Add(time.Minute)
	recovered, err := restarted.observe(t.Context(), source, sessionKey, recoveredAt, false, ibkrlib.AccountDailyPnL{
		DailyPnL: &daily, DailyPnLStatus: ibkrlib.DailyPnLFrameAvailable, AsOf: recoveredAt,
	}, true)
	if err != nil || recovered.Status != rpc.DailyPnLObservationOK {
		t.Fatalf("persist recovery = %+v err=%v", recovered, err)
	}
	secondRestart := dailyPnLObservationAuthority{}
	if err := secondRestart.bindCore(t.Context(), restartedStore); err != nil {
		t.Fatal(err)
	}
	clean, err := secondRestart.observe(t.Context(), source, sessionKey, recoveredAt.Add(time.Minute), false, ibkrlib.AccountDailyPnL{}, false)
	if err != nil || clean.Status != rpc.DailyPnLObservationNotDue {
		t.Fatalf("post-recovery restart = %+v err=%v, want not_due", clean, err)
	}
}
