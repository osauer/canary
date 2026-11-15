package daemon

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The verdict says how long it has read as it does: the clock moves only
// when the status changes, holds across a restart, and a verdict read on a
// fresh daemon continues the kept clock instead of starting at the restart.
func TestMandateStatusClockHoldsAcrossRestarts(t *testing.T) {
	stateHome := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	daemon := func() *Server {
		return &Server{coreStore: core, logger: NewLogger(&bytes.Buffer{}, "error")}
	}
	at := time.Date(2026, 10, 16, 20, 0, 0, 0, time.UTC)
	s := daemon()
	if got := s.mandateStatusSince(t.Context(), mandate.StatusWithin, at); !got.Equal(at) {
		t.Fatalf("first reading since %s, want %s", got, at)
	}
	if got := s.mandateStatusSince(t.Context(), mandate.StatusWithin, at.Add(time.Hour)); !got.Equal(at) {
		t.Fatalf("an unchanged status moved the clock to %s", got)
	}
	turned := at.Add(8 * time.Hour)
	if got := s.mandateStatusSince(t.Context(), mandate.StatusUnknown, turned); !got.Equal(turned) {
		t.Fatalf("a changed status kept %s, want %s", got, turned)
	}
	// A restart: the new daemon reads unknown and continues the clock.
	restarted := daemon()
	if got := restarted.mandateStatusSince(t.Context(), mandate.StatusUnknown, turned.Add(36*time.Hour)); !got.Equal(turned) {
		t.Fatalf("after a restart since %s, want %s", got, turned)
	}
	if got := restarted.mandateStatusSince(t.Context(), mandate.StatusWithin, turned.Add(40*time.Hour)); !got.Equal(turned.Add(40 * time.Hour)) {
		t.Fatalf("the release kept %s", got)
	}
}

// The verdict result carries the clock, and a read served from the cache
// carries the same one.
func TestMandateVerdictCarriesStatusSince(t *testing.T) {
	srv, st := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(1000), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin || !res.StatusSince.Equal(mandateTestNow) {
		t.Fatalf("first verdict %s since %s", res.Verdict.Status, res.StatusSince)
	}
	later := mandateTestNow.Add(10 * time.Minute)
	srv.now = func() time.Time { return later }
	st.now = srv.now
	if res := srv.evaluateMandate(t.Context(), false); !res.StatusSince.Equal(mandateTestNow) {
		t.Fatalf("an unchanged status moved the clock to %s", res.StatusSince)
	}
	c := srv.riskPolicies.active
	session := mandateSessionDate(later)
	pnl := 100.0
	st.ObserveWithPnL(300000, mandatePnLReading{DailyBase: &pnl, Session: session}, later.Add(-90*time.Second), c, testLiveObserveScope, true)
	st.ObserveWithPnL(280000, mandatePnLReading{DailyBase: &pnl, Session: session}, later.Add(-time.Minute), c, testLiveObserveScope, true)
	res = srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusUnknown || !res.StatusSince.Equal(later) {
		t.Fatalf("unknown verdict since %s, want %s (%s)", res.StatusSince, later, res.Verdict.Status)
	}
}
