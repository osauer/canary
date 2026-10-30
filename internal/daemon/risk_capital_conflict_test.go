package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A save the database does not hold leaves the store one revision ahead of
// it (live 2026-10-29 21:30 CET, after a daemon crash and a double start).
// The store reloads the saved state on the conflict, so the next change
// saves instead of failing the same way until a restart; the failure warns
// once and its recovery once.
func TestRiskCapitalRevisionConflictReloadsAndSavesAgain(t *testing.T) {
	st, c, now := recoveryStore(t)
	var warnings []string
	st.warnf = func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) }
	st.revision++ // the daemon remembers a save the database lost

	*now = now.Add(time.Minute)
	st.Observe(270000, *now, c, testLiveObserveScope, true)
	if st.state.AdjustedPeakBase == 270000 {
		t.Fatal("a failed save kept its change in memory")
	}
	*now = now.Add(time.Minute)
	st.Observe(275000, *now, c, testLiveObserveScope, true)
	doc, ok, err := st.core.GetStateDocument(t.Context(), st.scopeKey, stateKindRiskCapital)
	if err != nil || !ok {
		t.Fatalf("saved state unreadable: ok=%v err=%v", ok, err)
	}
	var saved riskCapitalSQLiteDocument
	if err := json.Unmarshal(doc.JSON, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State.AdjustedPeakBase != 275000 || st.revision != doc.Revision {
		t.Fatalf("the store stayed stuck after the conflict: saved peak %v, revision memory %d database %d", saved.State.AdjustedPeakBase, st.revision, doc.Revision)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "could not be saved") || !strings.Contains(warnings[1], "saves again") {
		t.Fatalf("want one failure warning and one recovery warning, got %q", warnings)
	}
}
