package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A daemon of this build starts on a store the build before closes-only
// wrote (owner decision 2026-11-24 18:26 CET). That build persisted a
// HYG/SPY snapshot with no hyg_close and derived the streak's last value and
// the decision journal's value and depth from hyg_price. Start-up reconcile
// re-derives both from the persisted snapshot and refuses the start on any
// difference, so this build must derive the same content from that
// snapshot. The 2026-11-24 19:12 CET install failed exactly here. Synthetic
// prices only.
func TestRegimeStartsOnAStoreWrittenBeforeClosesOnly(t *testing.T) {
	ny := newYorkLocation()
	asOf := time.Date(2026, 3, 18, 11, 0, 0, 0, ny)
	price, average := 41.20, 41.75
	store := openRegimeSnapshotTestStore(t)
	snapshot := regimeSnapshotCacheFixture(asOf.UTC(), "before-closes-only")
	snapshot.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{
		Status: rpc.RegimeStatusOK, HYGPrice: new(price), HYG50DMA: new(average), HYGDataType: "live",
		SPYPrice: new(470.0), SPY52WHigh: new(520.0),
		Streak: &rpc.StreakInfo{Band: "red", Sessions: 9, Since: "2026-03-06"},
		RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
			Band:        "red",
			Freshness:   &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh},
			Eligibility: &rpc.RegimeEligibility{Eligible: true, Latched: true},
		},
	}
	snapshot.Fingerprint = rpc.BuildRegimeFingerprint(snapshot)
	publication := regimeSnapshotPublication{Revision: 7, PublishedAt: asOf.UTC(), Fingerprint: snapshot.Fingerprint}

	// What the previous build stored: the streak's last value is hyg_price.
	stored := StreakEntry{
		LastBand: "red", SinceDate: "2026-03-06", LastSession: "2026-03-18",
		Sessions: 9, LastValue: price, EligibleLatched: true,
	}
	projectionRecoverySeedStreakStore(t, store, publication, map[string]StreakEntry{StreakKeyHYGSPY: stored})
	restarted := NewStreakStore("")
	if err := restarted.UseCoreStore(store); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcileRegimeProjection(t.Context(), snapshot, regimeProjectionPlan{publication: publication}); err != nil {
		t.Fatalf("start-up reconcile on the previous build's streak: %v", err)
	}
	restarted.mu.Lock()
	got := restarted.entries[StreakKeyHYGSPY]
	restarted.mu.Unlock()
	if !reflect.DeepEqual(got, stored) {
		t.Fatalf("streak after reconcile %+v, want the stored %+v (sessions and latch carried)", got, stored)
	}

	// The decision journal line is re-rendered byte for byte at start; the
	// previous build wrote hyg_price as the value and its depth below the
	// average.
	row := regimeDecisionIndicators(snapshot)[StreakKeyHYGSPY]
	wantDepth := (average - price) / average * 100
	if row.Value == nil || *row.Value != price || row.Depth == nil || *row.Depth != wantDepth {
		t.Fatalf("decision row value %v depth %v, want %v and %v as the previous build wrote", row.Value, row.Depth, price, wantDepth)
	}
}
