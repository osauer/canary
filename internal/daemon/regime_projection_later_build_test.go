package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A daemon rolled back onto a store a later build wrote must start. A later
// build may measure a row on another input (HYG on its settled close, owner
// decision 2026-11-24 18:26 CET), so the streak's last value and the decision
// line it stored differ from what this build derives from the same snapshot.
// On 2026-11-27 the start check showed v3.46.2 refusing such a store with
// "regime decision projection content mismatch". Synthetic values only.

func laterBuildSnapshot(t *testing.T) (*rpc.RegimeSnapshotResult, regimeSnapshotPublication) {
	t.Helper()
	asOf := time.Date(2026, 3, 18, 15, 0, 0, 0, time.UTC)
	snapshot := regimeSnapshotCacheFixture(asOf, "later-build")
	snapshot.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{
		Status: rpc.RegimeStatusOK, HYGPrice: new(41.20), HYG50DMA: new(41.75), HYGDataType: "live",
		SPYPrice: new(470.0), SPY52WHigh: new(480.0),
		Streak:      &rpc.StreakInfo{Band: "red", Sessions: 9, Since: "2026-03-06"},
		Band:        "red",
		Freshness:   &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh},
		Eligibility: &rpc.RegimeEligibility{Eligible: true, Latched: true},
	}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	return snapshot, regimeSnapshotPublication{Revision: 7, PublishedAt: asOf, Fingerprint: snapshot.Fingerprint}
}

func TestStartReplacesAStreakValueALaterBuildMeasuredDifferently(t *testing.T) {
	snapshot, publication := laterBuildSnapshot(t)
	plan := regimeProjectionPlan{publication: publication}
	// The later build stored the settled close; this build measures the price.
	stored := StreakEntry{
		LastBand: "red", SinceDate: "2026-03-06", LastSession: "2026-03-18",
		Sessions: 9, LastValue: 41.55, EligibleLatched: true,
	}
	store := openRegimeSnapshotTestStore(t)
	projectionRecoverySeedStreakStore(t, store, publication, map[string]StreakEntry{StreakKeyHYGSPY: stored})

	restarted := NewStreakStore("")
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileRegimeProjection(t.Context(), snapshot, plan); err != nil {
		t.Fatalf("start on a later build's streak: %v", err)
	}
	want := stored
	want.LastValue = 41.20
	// The replacement is durable: a second start reads it back unchanged.
	again := NewStreakStore("")
	if err := again.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := again.reconcileRegimeProjection(t.Context(), snapshot, plan); err != nil {
		t.Fatalf("second start: %v", err)
	}
	again.mu.Lock()
	got := again.entries[StreakKeyHYGSPY]
	again.mu.Unlock()
	if got != want {
		t.Fatalf("streak after start %+v, want %+v (only the last value replaced)", got, want)
	}

	// Anything else that differs is still refused: here the stored streak
	// lost the latch the snapshot says it earned.
	unlatched := stored
	unlatched.EligibleLatched = false
	damaged := projectionRecoverySeedStreakStore(t, openRegimeSnapshotTestStore(t), publication,
		map[string]StreakEntry{StreakKeyHYGSPY: unlatched})
	err := damaged.reconcileRegimeProjection(t.Context(), snapshot, plan)
	if err == nil || !strings.Contains(err.Error(), "content mismatch") {
		t.Fatalf("stored streak without its latch err=%v, want a content mismatch", err)
	}
}

func TestStartAcceptsADecisionLineALaterBuildRendered(t *testing.T) {
	snapshot, publication := laterBuildSnapshot(t)
	event := func(edit func(*regimeDecisionLine)) (corestore.EventRecord, regimeDecisionLine) {
		line := buildRegimeDecisionLine(publication.PublishedAt, snapshot, publication)
		edit(&line)
		return corestore.EventRecord{
			EventKey:   fmt.Sprintf("%s:snapshot:%020d", coreEventRegimeDecision, publication.Revision),
			OccurredAt: publication.PublishedAt,
		}, line
	}
	closeMeasured := func(line *regimeDecisionLine) {
		row := line.Indicators[StreakKeyHYGSPY]
		row.Value = new(41.55)
		line.Indicators[StreakKeyHYGSPY] = row
	}

	record, line := event(func(line *regimeDecisionLine) {
		line.V = regimeDecisionLineVersion + 1
		closeMeasured(line)
	})
	if err := validateRegimeDecisionProjectionEvent(record, line, snapshot, publication); err != nil {
		t.Fatalf("a later version's line bound to this snapshot: %v", err)
	}

	// A line of this build's own version is still compared byte for byte.
	record, line = event(closeMeasured)
	if err := validateRegimeDecisionProjectionEvent(record, line, snapshot, publication); err == nil ||
		!strings.Contains(err.Error(), "content mismatch") {
		t.Fatalf("current-version line with another value err=%v, want a content mismatch", err)
	}

	// A later version's line for another snapshot is still refused.
	record, line = event(func(line *regimeDecisionLine) {
		line.V = regimeDecisionLineVersion + 1
		line.SnapshotFingerprint.Key = "another-snapshot"
		line.Fingerprint = "another-snapshot"
	})
	if err := validateRegimeDecisionProjectionEvent(record, line, snapshot, publication); err == nil ||
		!strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("later-version line of another snapshot err=%v, want a fingerprint mismatch", err)
	}
}
