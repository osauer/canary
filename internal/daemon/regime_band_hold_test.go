package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// hygSpyRedSnapshot is a credit row that bands red from live inputs: HYG under
// its 50-day SMA with SPY inside 3% of its 52-week high.
func hygSpyRedSnapshot(at time.Time) *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{
		AsOf: at,
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:     rpc.RegimeStatusOK,
			HYGPrice:   new(78.0),
			HYG50DMA:   new(80.0),
			SPYPrice:   new(757.67),
			SPY52WHigh: new(760.40),
			HYGQuality: &rpc.Quality{AsOf: at, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm},
		},
	}
}

// hygSpyBlindSnapshot replays 2026-08-04 08:28:37 CEST: the SPY subscribe
// returned nothing, so the fetcher errors the whole row and no band can be
// computed — even though HYG's own inputs were present.
func hygSpyBlindSnapshot(at time.Time) *rpc.RegimeSnapshotResult {
	return &rpc.RegimeSnapshotResult{
		AsOf: at,
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:       rpc.RegimeStatusError,
			ErrorMessage: "HYG or SPY spot missing",
		},
	}
}

func TestRegimeBandHeldWhenInputsUnavailable(t *testing.T) {
	ny := newYorkLocation()
	store := NewStreakStore(t.TempDir())
	measured := time.Date(2026, 8, 4, 2, 24, 0, 0, ny)

	// One live pass banks red and dates it. Whatever confirmation state that
	// earns is legitimate; the hold must simply not advance it.
	if p := (&Server{}).populateStreaksWithStore(hygSpyRedSnapshot(measured), store)[rpc.RegimeIndicatorHYGSPY]; p.band != "red" || p.held {
		t.Fatalf("live pass: band=%q held=%v, want a computed red", p.band, p.held)
	}
	latchedBefore := store.Latched(StreakKeyHYGSPY)
	sessionsBefore := 0
	if info := store.Get(StreakKeyHYGSPY); info != nil {
		sessionsBefore = info.Sessions
	}

	// Four minutes later the SPY tick is gone. The market has not moved —
	// only our sight of it — so the band must not move either.
	blind := hygSpyBlindSnapshot(measured.Add(4 * time.Minute))
	policy := (&Server{}).populateStreaksWithStore(blind, store)[rpc.RegimeIndicatorHYGSPY]

	if policy.band != "red" {
		t.Fatalf("band=%q, want the last measured red held", policy.band)
	}
	if !policy.held {
		t.Fatal("band must be marked held, or a consumer cannot tell memory from measurement")
	}
	if !policy.heldAt.Equal(measured) {
		t.Errorf("heldAt=%v, want the time of the last live measurement %v", policy.heldAt, measured)
	}
	// A held band is memory. It may not confirm stress.
	if policy.eligibility != nil {
		t.Errorf("held band carried eligibility %+v, want nil — memory must never confirm", policy.eligibility)
	}
	if store.Latched(StreakKeyHYGSPY) != latchedBefore {
		t.Errorf("hold changed the eligibility latch (%v -> %v); memory must not advance confirmation state",
			latchedBefore, store.Latched(StreakKeyHYGSPY))
	}
	if info := store.Get(StreakKeyHYGSPY); info != nil && info.Sessions != sessionsBefore {
		t.Errorf("hold banked a persistence session (%d -> %d); an outage is not evidence of persistence",
			sessionsBefore, info.Sessions)
	}

	// The row says so on its face.
	annotateRegimeMetadata(blind, map[string]regimeRowPolicy{rpc.RegimeIndicatorHYGSPY: policy})
	got := blind.HYGSPYDivergence.BandReason
	if !strings.Contains(got, "held from the last measured reading") {
		t.Errorf("band_reason=%q, want it to disclose the band is held", got)
	}
	if !strings.Contains(got, measured.UTC().Format("2006-01-02 15:04Z")) {
		t.Errorf("band_reason=%q, want it to date the held reading", got)
	}

	// Holding the band must not hide the outage: the row keeps its error
	// status, so data_quality still names the credit lane.
	if partial := partialRegimeClusters(blind); !slices.Contains(partial, "credit") {
		t.Errorf("partial clusters=%v, want credit still reported while the band is held", partial)
	}
}

// Nothing measured, nothing to hold: the row stays honestly unranked rather
// than inventing a band.
func TestRegimeBandNotHeldWithoutPriorMeasurement(t *testing.T) {
	ny := newYorkLocation()
	store := NewStreakStore(t.TempDir())
	blind := hygSpyBlindSnapshot(time.Date(2026, 8, 4, 2, 28, 0, 0, ny))

	policy := (&Server{}).populateStreaksWithStore(blind, store)[rpc.RegimeIndicatorHYGSPY]
	if policy.band != "" || policy.held {
		t.Fatalf("band=%q held=%v, want unranked with no prior band to hold", policy.band, policy.held)
	}
	annotateRegimeMetadata(blind, map[string]regimeRowPolicy{rpc.RegimeIndicatorHYGSPY: policy})
	if b := blind.HYGSPYDivergence.Band; b != "unranked" {
		t.Errorf("band=%q, want unranked", b)
	}
}

// Memory never overrides measurement: once the inputs return, the freshly
// computed band wins and the row stops reporting itself as held.
func TestRegimeMeasuredBandOverridesHeldBand(t *testing.T) {
	ny := newYorkLocation()
	store := NewStreakStore(t.TempDir())
	measured := time.Date(2026, 8, 4, 2, 24, 0, 0, ny)
	(&Server{}).populateStreaksWithStore(hygSpyRedSnapshot(measured), store)
	(&Server{}).populateStreaksWithStore(hygSpyBlindSnapshot(measured.Add(4*time.Minute)), store)

	// Inputs return, and credit has genuinely improved: HYG back above its
	// 50-day SMA is green, and the held red must not survive it.
	recovered := hygSpyRedSnapshot(measured.Add(8 * time.Minute))
	recovered.HYGSPYDivergence.HYGPrice = new(81.0)
	policy := (&Server{}).populateStreaksWithStore(recovered, store)[rpc.RegimeIndicatorHYGSPY]
	if policy.held {
		t.Error("row still reports held after its inputs returned")
	}
	if policy.band != "green" {
		t.Fatalf("band=%q, want the freshly measured green", policy.band)
	}
}
