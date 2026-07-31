package daemon

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// TestStreakStore_FirstCall starts the counter at 1 with today's session.
func TestStreakStore_FirstCall(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	info := s.Tick(StreakKeyVIXTerm, 0.85, "green", now)
	if info == nil {
		t.Fatal("nil info from first Tick")
	}
	if info.Sessions != 1 || info.Band != "green" || info.Since != "2026-05-20" {
		t.Errorf("got Sessions=%d Band=%q Since=%q, want 1/green/2026-05-20",
			info.Sessions, info.Band, info.Since)
	}
}

// TestStreakStore_SameSessionNoIncrement: multiple calls on the same NY
// session with the same band leave Sessions at 1.
func TestStreakStore_SameSessionNoIncrement(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", now)
	later := mustParseNY(t, "2026-05-20 14:00 EST")
	info := s.Tick(StreakKeyVIXTerm, 0.86, "green", later)
	if info.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 (same session, no increment)", info.Sessions)
	}
}

// TestStreakStore_NextSessionIncrements: a same-band call on a later NY
// session ticks Sessions up by 1.
func TestStreakStore_NextSessionIncrements(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0.86, "green", day2)
	if info.Sessions != 2 || info.Since != "2026-05-20" {
		t.Errorf("got Sessions=%d Since=%q, want 2/2026-05-20", info.Sessions, info.Since)
	}
}

// TestStreakStore_BandChangeResets: a different band on a later day
// resets Sessions to 1 and Since to today.
func TestStreakStore_BandChangeResets(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0.95, "yellow", day2)
	if info.Sessions != 1 || info.Band != "yellow" || info.Since != "2026-05-21" {
		t.Errorf("got Sessions=%d Band=%q Since=%q, want 1/yellow/2026-05-21",
			info.Sessions, info.Band, info.Since)
	}
}

// TestStreakStore_EmptyBandFreezes: an unavailable indicator (empty band)
// returns the previous state without mutating the counter.
func TestStreakStore_EmptyBandFreezes(t *testing.T) {
	s := NewStreakStore(t.TempDir())
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	info := s.Tick(StreakKeyVIXTerm, 0, "", day2)
	if info == nil || info.Sessions != 1 || info.Band != "green" {
		t.Errorf("freeze returned %+v, want green sessions=1", info)
	}
	// Now a real tick on day 2 should still see day1 as Since.
	info = s.Tick(StreakKeyVIXTerm, 0.86, "green", day2)
	if info.Sessions != 2 {
		t.Errorf("after freeze + real tick, Sessions = %d, want 2", info.Sessions)
	}
}

func TestPopulateStreaksDoesNotAttachFrozenStreakToUnrankedRow(t *testing.T) {
	store := NewStreakStore(t.TempDir())
	now := mustParseNY(t, "2026-05-20 10:00 EST")
	store.Tick(StreakKeyVIXTerm, 0.85, "green", now)

	srv := &Server{streaks: store}
	res := &rpc.RegimeSnapshotResult{
		VIXTermStructure: rpc.RegimeVIXTerm{Status: rpc.RegimeStatusError, ErrorMessage: "no tick"},
	}
	srv.populateStreaks(res)
	if res.VIXTermStructure.Streak != nil {
		t.Fatalf("unranked VIX row should not expose frozen prior streak, got %+v", res.VIXTermStructure.Streak)
	}
	if got := store.Get(StreakKeyVIXTerm); got == nil || got.Band != "green" {
		t.Fatalf("store should still retain the frozen streak internally, got %+v", got)
	}
}

// TestStreakStore_PersistAcrossInstances: a store written by one instance
// should be loaded by a fresh instance pointed at the same dir.
func TestStreakStore_PersistAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	s1 := NewStreakStore(dir)
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s1.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	s1.Tick(StreakKeyVIXTerm, 0.86, "green", day2)

	s2 := NewStreakStore(dir)
	info := s2.Get(StreakKeyVIXTerm)
	if info == nil || info.Sessions != 2 || info.Since != "2026-05-20" {
		t.Errorf("reload got %+v, want sessions=2 since=2026-05-20", info)
	}
}

func TestStreakStoreUsesSQLiteWithoutLegacyFallback(t *testing.T) {
	legacyDir := t.TempDir()
	authority := openMarketTestCoreStore(t)
	s1 := NewStreakStore(legacyDir)
	if err := s1.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	day1 := mustParseNY(t, "2026-05-20 10:00 EST")
	day2 := mustParseNY(t, "2026-05-21 10:00 EST")
	s1.Tick(StreakKeyVIXTerm, 0.85, "green", day1)
	s1.Tick(StreakKeyVIXTerm, 0.86, "green", day2)
	entries, err := os.ReadDir(legacyDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy streak file was written: entries=%v err=%v", entries, err)
	}

	s2 := NewStreakStore(legacyDir)
	if err := s2.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	info := s2.Get(StreakKeyVIXTerm)
	if info == nil || info.Sessions != 2 || info.Since != "2026-05-20" {
		t.Fatalf("SQLite reload got %+v", info)
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: streakAuthorityScope, Source: streakSource, Kind: streakObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("observations=%d err=%v", len(observations), err)
	}
}

func TestVIXTermCadenceDistinguishesNotDueFromOverdue(t *testing.T) {
	ny := newYorkLocation()
	ratio := 1.06
	quality := func(at time.Time, class string) *rpc.Quality {
		return &rpc.Quality{AsOf: at, FreshnessClass: class, Confidence: rpc.ConfidenceFirm}
	}
	result := &rpc.RegimeSnapshotResult{
		AsOf: time.Date(2026, 7, 20, 1, 5, 0, 0, ny),
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale, Ratio: &ratio,
		},
	}
	result.VIXTermStructure.VIXQuality = quality(result.AsOf, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(result.AsOf, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, result.AsOf); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 01:05 ET cadence=%q, want not_due", got)
	}
	policy := (&Server{}).populateStreaksWithStore(result, nil)[rpc.RegimeIndicatorVIXTerm]
	if policy.freshness == nil || policy.freshness.Class != rpc.RegimeFreshnessNotDue || policy.eligibility == nil || policy.eligibility.Eligible ||
		len(policy.eligibility.Reasons) != 1 || policy.eligibility.Reasons[0] != "data_not_due" {
		t.Fatalf("Monday 01:05 ET VIX policy=%+v", policy)
	}

	// Pre-open, IBKR reports each Cboe index leg's subscription mode, which
	// flips between live and frozen on its own. No combination means a VIX3M
	// observation went missing, so none of them may raise a data-quality
	// defect — and none can reach confirmable freshness either.
	gth := time.Date(2026, 7, 20, 4, 0, 0, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(gth, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(gth, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, gth); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 04:00 ET frozen VIX cadence=%q, want not_due", got)
	}
	result.AsOf = gth
	policy = (&Server{}).populateStreaksWithStore(result, nil)[rpc.RegimeIndicatorVIXTerm]
	if policy.freshness == nil || policy.freshness.Class != rpc.RegimeFreshnessNotDue || policy.eligibility == nil ||
		len(policy.eligibility.Reasons) != 1 || policy.eligibility.Reasons[0] != "data_not_due" {
		t.Fatalf("Monday 04:00 ET frozen VIX policy=%+v", policy)
	}
	result.VIXTermStructure.VIXQuality = quality(gth, rpc.FreshnessLive)
	if got := vixTermCadenceClass(result, gth); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 04:00 ET live VIX/frozen VIX3M cadence=%q, want not_due", got)
	}
	result.VIXTermStructure.VIX3MQuality = quality(gth, rpc.FreshnessLive)
	if got := vixTermCadenceClass(result, gth); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 04:00 ET off-window live VIX3M cadence=%q, want not_due", got)
	}
	result.VIXTermStructure.Status = rpc.RegimeStatusOK
	if got := vixTermCadenceClass(result, gth); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 04:00 ET off-window live/live cadence=%q, want not_due (never fresh)", got)
	}
	result.VIXTermStructure.Status = rpc.RegimeStatusStale
	result.VIXTermStructure.VIX3MQuality = quality(gth, rpc.FreshnessFrozen)
	savedVIX3M := result.VIXTermStructure.VIX3MQuality
	result.VIXTermStructure.VIX3MQuality = nil
	if got := vixTermCadenceClass(result, gth); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("Monday 04:00 ET missing VIX3M cadence=%q, want overdue", got)
	}
	result.VIXTermStructure.VIX3MQuality = savedVIX3M

	beforePause := time.Date(2026, 7, 20, 9, 24, 59, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(beforePause, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(beforePause, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, beforePause); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 09:24 ET frozen VIX cadence=%q, want not_due", got)
	}
	pauseStart := time.Date(2026, 7, 20, 9, 25, 0, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(pauseStart, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(pauseStart, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, pauseStart); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 09:25 ET VIX pause cadence=%q, want not_due", got)
	}
	beforeWindow := time.Date(2026, 7, 20, 9, 30, 59, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(beforeWindow, rpc.FreshnessLive)
	result.VIXTermStructure.VIX3MQuality = quality(beforeWindow, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, beforeWindow); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Monday 09:30 ET cadence=%q, want not_due", got)
	}
	afterWindow := time.Date(2026, 7, 20, 9, 31, 0, 0, ny)
	if got := vixTermCadenceClass(result, afterWindow); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("Monday 09:31 ET cadence=%q, want overdue", got)
	}
	rth := time.Date(2026, 7, 20, 10, 0, 0, 0, ny)
	result.VIXTermStructure.Status = rpc.RegimeStatusOK
	result.VIXTermStructure.VIXQuality = quality(rth, rpc.FreshnessLive)
	result.VIXTermStructure.VIX3MQuality = quality(rth, rpc.FreshnessLive)
	if got := vixTermCadenceClass(result, rth); got != rpc.RegimeFreshnessFresh {
		t.Fatalf("live VIX cadence=%q, want fresh", got)
	}
	result.VIXTermStructure.Status = rpc.RegimeStatusStale
	weekend := time.Date(2026, 7, 19, 1, 5, 0, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(weekend, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(weekend, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, weekend); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("Sunday cadence=%q, want not_due", got)
	}

	earlyCloseBeforeEnd := time.Date(2026, 11, 27, 13, 14, 59, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(earlyCloseBeforeEnd, rpc.FreshnessFrozen)
	result.VIXTermStructure.VIX3MQuality = quality(earlyCloseBeforeEnd, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, earlyCloseBeforeEnd); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("early close before VIX3M end cadence=%q, want overdue", got)
	}
	earlyCloseEnded := time.Date(2026, 11, 27, 13, 15, 0, 0, ny)
	if got := vixTermCadenceClass(result, earlyCloseEnded); got != rpc.RegimeFreshnessNotDue {
		t.Fatalf("early close after VIX3M end cadence=%q, want not_due", got)
	}
	unknown := time.Date(2035, 7, 20, 7, 5, 0, 0, ny)
	result.VIXTermStructure.VIXQuality = quality(unknown, rpc.FreshnessLive)
	result.VIXTermStructure.VIX3MQuality = quality(unknown, rpc.FreshnessFrozen)
	if got := vixTermCadenceClass(result, unknown); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("unknown-calendar cadence=%q, want overdue", got)
	}
}

// IDEALPRO trades one continuous weekly session, so a shut market is an
// expected gap and not a source defect — including the whole weekend, which
// previously read overdue and blocked the dashboard every Saturday.
func TestUSDJPYCadenceFollowsIDEALPROSession(t *testing.T) {
	ny := newYorkLocation()
	last, weekly := 150.0, -2.4
	result := &rpc.RegimeSnapshotResult{
		USDJPY: rpc.RegimeUSDJPY{Last: &last, WeeklyChange: &weekly},
	}
	// 2026-07-31 is a Friday; 08-01 Saturday; 08-02 Sunday; 08-03 Monday.
	for _, tc := range []struct {
		name   string
		at     time.Time
		status string
		want   string
	}{
		{"friday mid-session live tick", time.Date(2026, 7, 31, 12, 0, 0, 0, ny), rpc.RegimeStatusOK, rpc.RegimeFreshnessFresh},
		{"friday mid-session frozen tick", time.Date(2026, 7, 31, 12, 0, 0, 0, ny), rpc.RegimeStatusStale, rpc.RegimeFreshnessOverdue},
		{"friday weekly close", time.Date(2026, 7, 31, 17, 0, 0, 0, ny), rpc.RegimeStatusStale, rpc.RegimeFreshnessNotDue},
		{"saturday", time.Date(2026, 8, 1, 12, 0, 0, 0, ny), rpc.RegimeStatusStale, rpc.RegimeFreshnessNotDue},
		{"sunday before reopen", time.Date(2026, 8, 2, 17, 14, 59, 0, ny), rpc.RegimeStatusStale, rpc.RegimeFreshnessNotDue},
		{"sunday reopen", time.Date(2026, 8, 2, 17, 15, 0, 0, ny), rpc.RegimeStatusOK, rpc.RegimeFreshnessFresh},
		{"weekday changeover break", time.Date(2026, 8, 3, 17, 5, 0, 0, ny), rpc.RegimeStatusStale, rpc.RegimeFreshnessNotDue},
		{"weekday after changeover", time.Date(2026, 8, 3, 17, 15, 0, 0, ny), rpc.RegimeStatusOK, rpc.RegimeFreshnessFresh},
	} {
		result.USDJPY.Status = tc.status
		if got := usdJpyCadenceClass(result, tc.at); got != tc.want {
			t.Fatalf("%s cadence=%q, want %q", tc.name, got, tc.want)
		}
	}

	saturday := time.Date(2026, 8, 1, 12, 0, 0, 0, ny)
	result.USDJPY.Status = rpc.RegimeStatusUnavailable
	if got := usdJpyCadenceClass(result, saturday); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("saturday unavailable row cadence=%q, want overdue", got)
	}

	// The red row stays visible over the weekend as context, and the cluster
	// earns the expected-not-due exemption that keeps readiness off blocked.
	result.USDJPY.Status = rpc.RegimeStatusStale
	result.AsOf = saturday
	policies := (&Server{}).populateStreaksWithStore(result, nil)
	policy := policies[rpc.RegimeIndicatorUSDJPY]
	if policy.band != "red" || policy.freshness == nil || policy.freshness.Class != rpc.RegimeFreshnessNotDue ||
		policy.eligibility == nil || len(policy.eligibility.Reasons) != 1 || policy.eligibility.Reasons[0] != "data_not_due" {
		t.Fatalf("saturday USD/JPY policy=%+v", policy)
	}
	annotateRegimeMetadata(result, policies)
	if !rpc.RegimeClusterExpectedNotDue(*result, "fx") {
		t.Fatalf("saturday fx cluster is not expected-not-due: %+v", result.USDJPY.RegimeIndicatorMeta)
	}
	for _, health := range rpc.BuildRegimeSourceHealth(result, saturday) {
		if health.Source != "fx" {
			continue
		}
		if health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue {
			t.Fatalf("saturday fx source health=%+v", health)
		}
	}

	// A frozen tick while IDEALPRO is trading is still a real defect.
	result.AsOf = time.Date(2026, 7, 31, 12, 0, 0, 0, ny)
	annotateRegimeMetadata(result, (&Server{}).populateStreaksWithStore(result, nil))
	if rpc.RegimeClusterExpectedNotDue(*result, "fx") {
		t.Fatal("mid-session frozen USD/JPY tick was exempted as expected-not-due")
	}
}

// TestClassifyBands sanity-checks each classifier against the spec.
func TestClassifyBands(t *testing.T) {
	mkPtr := func(v float64) *float64 { return &v }

	t.Run("vix term", func(t *testing.T) {
		cases := []struct {
			ratio float64
			want  string
		}{
			{0.85, "green"},
			{0.92, "yellow"},
			{0.95, "yellow"},
			{1.00, "red"},
			{1.20, "red"},
		}
		for _, c := range cases {
			if got := classifyVIXTermBand(mkPtr(c.ratio)); got != c.want {
				t.Errorf("classifyVIXTermBand(%v) = %q, want %q", c.ratio, got, c.want)
			}
		}
		if got := classifyVIXTermBand(nil); got != "" {
			t.Errorf("nil ratio returned %q, want empty (freeze)", got)
		}
	})

	t.Run("usdjpy", func(t *testing.T) {
		cases := []struct {
			weeklyPct float64
			want      string
		}{
			{0.0, "green"},
			{0.5, "green"},  // yen weakening — green
			{-0.5, "green"}, // yen weakening little — green
			{-1.0, "yellow"},
			{-1.5, "yellow"},
			{-2.0, "red"},
			{-3.5, "red"},
		}
		for _, c := range cases {
			if got := classifyUSDJPYBand(mkPtr(c.weeklyPct)); got != c.want {
				t.Errorf("classifyUSDJPYBand(%v) = %q, want %q", c.weeklyPct, got, c.want)
			}
		}
	})

	t.Run("hyg spy", func(t *testing.T) {
		hyg := 79.0
		hyg50 := 80.0
		spy := 737.0
		nearHigh := 749.0
		farHigh := 780.0
		if got := classifyHYGSPYBand(rpc.RegimeHYGSPYDivergence{
			HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &nearHigh,
		}); got != "red" {
			t.Errorf("HYG below 50dma + SPY near highs = %q, want red", got)
		}
		if got := classifyHYGSPYBand(rpc.RegimeHYGSPYDivergence{
			HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &farHigh,
		}); got != "yellow" {
			t.Errorf("HYG below 50dma away from highs = %q, want yellow", got)
		}
	})

	t.Run("gamma", func(t *testing.T) {
		// With a crossing, band on gap_pct.
		cases := []struct {
			gap  float64
			want string
		}{
			{3.0, "green"},
			{2.5, "green"},
			{1.0, "yellow"},
			{-1.5, "yellow"},
			{-2.5, "red"},
		}
		for _, c := range cases {
			if got := classifyGammaBand(mkPtr(c.gap), ""); got != c.want {
				t.Errorf("classifyGammaBand gap=%v = %q, want %q", c.gap, got, c.want)
			}
		}
		// Without a crossing, band on sign.
		if got := classifyGammaBand(nil, "positive"); got != "green" {
			t.Errorf("no crossing + positive = %q, want green", got)
		}
		if got := classifyGammaBand(nil, "negative"); got != "red" {
			t.Errorf("no crossing + negative = %q, want red", got)
		}
		if got := classifyGammaBand(nil, "no_data"); got != "" {
			t.Errorf("no crossing + no_data = %q, want empty", got)
		}
		combined := &rpc.GammaZeroComputed{
			Scope:   rpc.GammaZeroScopeCombined,
			Quality: rankableGammaQuality(),
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive", Quality: rankableGammaQuality()},
				"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "negative", Quality: rankableGammaQuality()},
			},
		}
		if got := classifyGammaComputedBand(combined); got != "red" {
			t.Errorf("SPX-dominant mixed combined gamma bands = %q, want red", got)
		}
	})

	t.Run("breadth", func(t *testing.T) {
		cases := []struct {
			v    float64
			want string
		}{
			{20, "red"},
			{39.9, "red"},
			{40, "yellow"},
			{50, "yellow"},
			{55, "yellow"},
			{55.1, "green"},
			{75, "green"},
		}
		for _, c := range cases {
			if got := classifyBreadthBand(c.v); got != c.want {
				t.Errorf("classifyBreadthBand(%v) = %q, want %q", c.v, got, c.want)
			}
		}
	})
}

func TestGammaStreaksRequireExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		quality *rpc.GammaSignalQuality
	}{
		{name: "nil_quality"},
		{name: "blocked_quality", quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gap := 5.0
			res := &rpc.RegimeSnapshotResult{
				GammaZero: rpc.RegimeGammaZero{
					Status: rpc.RegimeStatusOK,
					Envelope: rpc.GammaZeroSPXResult{
						Status: rpc.GammaZeroStatusReady,
						Result: &rpc.GammaZeroComputed{
							ZeroGamma: new(580.0),
							GapPct:    &gap,
							Quality:   tc.quality,
						},
					},
				},
			}

			band, _ := gammaZeroStreaks{}.bandAndValue(res)
			if band != "" {
				t.Fatalf("bandAndValue band = %q, want frozen/unranked", band)
			}
		})
	}
}

func mustParseNY(t *testing.T, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY tz: %v", err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04 MST", s, loc)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}
