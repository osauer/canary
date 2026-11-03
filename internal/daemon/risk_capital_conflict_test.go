package daemon

import (
	"strings"
	"testing"
	"time"
)

// A save the database does not hold leaves the store one revision ahead of
// it (live 2026-10-29 21:30 CET, after a daemon crash and a double start;
// again 2026-11-02 19:57 CET, ten hours before the file failed its startup
// check). Reloading and saving on, as b8964f6a did, kept writing into a
// store that had lost committed pages. A lost save now latches the
// authority fail-closed: the change is dropped, the failure warns once, and
// no later save lands until a restart re-checks the file.
func TestRiskCapitalLostSaveLatchesTheAuthority(t *testing.T) {
	st, c, now := recoveryStore(t)
	var warnings []string
	st.warnf = func(format string, args ...any) { warnings = append(warnings, format) }
	before := st.revision
	st.revision++ // the daemon remembers a save the database lost

	*now = now.Add(time.Minute)
	st.Observe(270000, *now, c, testLiveObserveScope, true)
	if st.state.AdjustedPeakBase == 270000 {
		t.Fatal("a failed save kept its change in memory")
	}
	if got := st.core.Health(); got.Ready || got.Code != "lost_write" {
		t.Fatalf("authority health after a lost save = %+v, want latched lost_write", got)
	}
	*now = now.Add(time.Minute)
	st.Observe(275000, *now, c, testLiveObserveScope, true)
	doc, ok, err := st.core.GetStateDocument(t.Context(), st.scopeKey, stateKindRiskCapital)
	if err != nil || !ok || doc.Revision != before {
		t.Fatalf("a save landed after the latch: doc revision %d (want %d) ok=%v err=%v", doc.Revision, before, ok, err)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "could not be saved") {
		t.Fatalf("want the failed save warned, got %q", warnings)
	}
}
