package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

// fakeDeps captures pre-canned answers for the three quote+history
// regime fetchers (VIX, HYG/SPY, USD/JPY). Tests construct one with
// the bits they need and the closures pull from these maps —
// missing keys yield zeros, which mirrors the production "no tick"
// path the fetchers gate on.
//
// warnLog accumulates log lines a fetcher emits on partial failure
// (snapshot miss, history error, insufficient bars). Tests assert against
// it to verify the operator-visible diagnostic landed; nil here means the
// fetcher had no failures worth logging.
//
// historyCalls records the days argument the production fetcher
// requested per symbol per call. The plain closures used to silently
// discard this — meaning a regression that reverted HYG's
// holiday-clipping widening (90 → 70) or USD.JPY's (14 → 12) would
// pass every test in the package. The capture pins the production
// contract so the test fake can't drift away from what the gateway
// is actually being asked for.
type fakeDeps struct {
	// now pins the fetchers' clock (regimeDeps.now). Zero means
	// regimeTestNow — a mid-RTH Wednesday — so calendar-keyed branches
	// (the closed-date day-change pin, off-hours banding) behave
	// identically no matter when the suite runs. Closed-date tests set
	// an explicit weekend/holiday instant.
	now       time.Time
	snapshots map[string]fakeQuote
	// rich holds the Week52High value the snapshotWith52WHigh dep
	// returns for a given symbol. Tests that don't set an entry simulate
	// the "Misc Stats tick 165 didn't land in the budget" case.
	rich          map[string]fakeRichQuote
	bars          map[string]fakeHistory
	series        map[string][]regimeSeriesPoint
	seriesErr     map[string]error
	vvix          []regimeSeriesPoint
	vvixErr       error
	warnLog       *[]string
	historyCalls  map[string][]int
	snapshotCalls map[string]int
}

type fakeQuote struct {
	price     float64
	prevClose float64 // tick 9 (previous regular-session close); 0 = "didn't arrive"
	dataType  string
}

type fakeRichQuote struct {
	price      float64
	prevClose  float64 // tick 9 (previous regular-session close); 0 = "didn't arrive"
	week52High float64
	dataType   string
}

type fakeHistory struct {
	bars []ibkrlib.HistoricalBar
	err  error
}

func (f *fakeDeps) build() *regimeDeps {
	nw := f.now
	if nw.IsZero() {
		nw = regimeTestNow
	}
	return &regimeDeps{
		now: func() time.Time { return nw },
		snapshot: func(_ context.Context, sym string, _ time.Duration) (float64, float64, string) {
			if f.snapshotCalls == nil {
				f.snapshotCalls = make(map[string]int)
			}
			f.snapshotCalls[sym]++
			q := f.snapshots[sym]
			return q.price, q.prevClose, q.dataType
		},
		snapshotWith52WHigh: func(_ context.Context, sym string, _ time.Duration) (float64, float64, float64, string) {
			r := f.rich[sym]
			return r.price, r.prevClose, r.week52High, r.dataType
		},
		history: func(_ context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
			if f.historyCalls == nil {
				f.historyCalls = make(map[string][]int)
			}
			f.historyCalls[sym] = append(f.historyCalls[sym], days)
			h := f.bars[sym]
			return h.bars, h.err
		},
		officialSeries: func(_ context.Context, seriesID string) ([]regimeSeriesPoint, error) {
			if err := f.seriesErr[seriesID]; err != nil {
				return nil, err
			}
			return f.series[seriesID], nil
		},
		vvixSeries: func(_ context.Context) ([]regimeSeriesPoint, error) {
			return f.vvix, f.vvixErr
		},
		logWarnf: func(format string, args ...any) {
			if f.warnLog != nil {
				*f.warnLog = append(*f.warnLog, fmt.Sprintf(format, args...))
			}
		},
	}
}

// regimeTestNow pins fetcher clock reads to a mid-RTH Wednesday
// (2026-07-15 14:00 ET). Without a pinned clock the closed-date
// day-change pin and the off-hours banding branch key on the real
// calendar and the suite behaves differently on weekends — the same
// flake class the canary suite pinned with stressTestNow.
var regimeTestNow = time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)

// regimeTestClosedSunday is an official non-trading date (Sunday
// 2026-07-19 16:06 ET) whose last two completed sessions are Thu
// 2026-07-16 and Fri 2026-07-17 — the live incident's calendar.
var regimeTestClosedSunday = time.Date(2026, 7, 19, 20, 6, 0, 0, time.UTC)

// makeDatedBars synthesises daily bars carrying wire-format YYYYMMDD
// session labels (what HMDS returns), oldest-first, so the closed-date
// pin's exact-date matching has real labels to match against.
func makeDatedBars(points map[string]float64) []ibkrlib.HistoricalBar {
	dates := slices.Sorted(maps.Keys(points))
	out := make([]ibkrlib.HistoricalBar, 0, len(dates))
	for _, d := range dates {
		out = append(out, ibkrlib.HistoricalBar{Date: strings.ReplaceAll(d, "-", ""), Close: points[d]})
	}
	return out
}

// makeBars synthesises N daily bars with a constant close. Oldest-
// first, matching what FetchHistoricalDailyBars returns.
func makeBars(n int, closePrice float64) []ibkrlib.HistoricalBar {
	out := make([]ibkrlib.HistoricalBar, n)
	for i := range n {
		out[i] = ibkrlib.HistoricalBar{Close: closePrice}
	}
	return out
}

func makeSeries(n int, start float64) []regimeSeriesPoint {
	out := make([]regimeSeriesPoint, n)
	base := time.Now().UTC().AddDate(0, 0, -n)
	for i := range n {
		out[i] = regimeSeriesPoint{
			Date:  base.AddDate(0, 0, i+1),
			Value: start + float64(i)*0.01,
		}
	}
	return out
}

func TestFetchRegimeVIXTerm(t *testing.T) {
	ctx := context.Background()
	t.Run("happy_live", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.Ratio == nil || *got.Ratio < 0.857 || *got.Ratio > 0.858 {
			t.Errorf("ratio=%v, want ≈0.857", got.Ratio)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("stale_data_type", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataFrozen},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Errorf("frozen tick: status=%q, want stale", got.Status)
		}
	})

	t.Run("vix3m_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX": {price: 18.0, dataType: rpc.MarketDataLive},
				// VIX3M absent → snapshot returns 0
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Fatalf("status=%q, want error", got.Status)
		}
		if got.VIX == nil || *got.VIX != 18.0 {
			t.Errorf("VIX should still be populated for partial-error case, got %v", got.VIX)
		}
		if got.Ratio != nil {
			t.Errorf("ratio must be nil when VIX3M missing, got %v", *got.Ratio)
		}
	})

	t.Run("vix_missing", func(t *testing.T) {
		deps := (&fakeDeps{}).build() // no snapshots
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Errorf("both missing: status=%q, want error", got.Status)
		}
		if got.VIX != nil || got.VIX3M != nil {
			t.Errorf("no measurements should be populated: got VIX=%v VIX3M=%v", got.VIX, got.VIX3M)
		}
	})

	// Mixed-freshness pair: the snapshot helper returns the previous
	// regular-session close for VIX3M (with dataType=frozen) when no
	// live tick lands. The row ranks at stale status — both legs are
	// usable for the ratio, but the UX must indicate that the value
	// isn't live so the renderer dims it.
	t.Run("vix3m_frozen_leg_downgrades_to_stale", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Fatalf("status=%q, want stale (live VIX + frozen VIX3M leg)", got.Status)
		}
		if got.Ratio == nil || *got.Ratio < 0.857 || *got.Ratio > 0.858 {
			t.Errorf("ratio=%v, want ≈0.857", got.Ratio)
		}
		if got.DataType != rpc.MarketDataFrozen {
			t.Errorf("data_type=%q, want %q (staler leg wins)", got.DataType, rpc.MarketDataFrozen)
		}
	})

	// Day-change anchor: when tick 9 lands alongside VIX, the fetcher
	// computes VIXChangePct as ((vix - prev) / prev) * 100. Dashboard
	// header reads this to color-code the VIX cell (red on up, green on
	// down — vol expanding is risk-off).
	t.Run("vix_prev_close_populates_change_pct", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, prevClose: 20.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.VIXPrevClose == nil || *got.VIXPrevClose != 20.0 {
			t.Fatalf("VIXPrevClose=%v, want 20.0", got.VIXPrevClose)
		}
		// (18 - 20) / 20 * 100 = -10
		if got.VIXChangePct == nil || *got.VIXChangePct < -10.01 || *got.VIXChangePct > -9.99 {
			t.Errorf("VIXChangePct=%v, want ≈-10", got.VIXChangePct)
		}
	})

	// The dashboard header is useful even when VIX3M fails — partial
	// envelope must still surface the VIX day-change anchor so the
	// reader sees "VIX 18.4 −10.0%" even with no term-structure ratio.
	t.Run("vix_prev_close_survives_vix3m_failure", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX": {price: 18.0, prevClose: 20.0, dataType: rpc.MarketDataLive},
				// VIX3M absent
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Fatalf("status=%q, want error", got.Status)
		}
		if got.VIXPrevClose == nil || *got.VIXPrevClose != 20.0 {
			t.Errorf("VIXPrevClose=%v, want 20.0 (header anchor must survive partial-error row)", got.VIXPrevClose)
		}
		if got.VIXChangePct == nil {
			t.Errorf("VIXChangePct must populate even when ratio leg fails")
		}
	})

	// Closed-date day-change pinning: on an official non-trading date the
	// change fields come from the official closes of the last two
	// completed sessions, never from the frozen print vs whatever the
	// tick-9 anchor drifted to. Live incident calendar: Sunday
	// 2026-07-19, Friday truly closed VIX +12.19% (18.77 vs 16.73). The
	// fake's tick-9 anchor equals the print — the observed weekend reset,
	// which the old math rendered as +0.00%.
	t.Run("closed_date_pins_change_to_official_closes", func(t *testing.T) {
		deps := (&fakeDeps{
			now: regimeTestClosedSunday,
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.77, prevClose: 18.77, dataType: rpc.MarketDataFrozen},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"VIX": {bars: makeDatedBars(map[string]float64{
					"2026-07-14": 17.1,
					"2026-07-15": 16.9,
					"2026-07-16": 16.73,
					"2026-07-17": 18.77,
				})},
			},
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.VIXPrevClose == nil || *got.VIXPrevClose != 16.73 {
			t.Fatalf("VIXPrevClose=%v, want Thursday official close 16.73", got.VIXPrevClose)
		}
		if got.VIXChangePct == nil || *got.VIXChangePct < 12.19 || *got.VIXChangePct > 12.20 {
			t.Errorf("VIXChangePct=%v, want ≈+12.19 (Friday close-to-close), not the reset anchor's 0.00", got.VIXChangePct)
		}
		if !strings.Contains(got.VIXChangeBasis, "official closes 2026-07-16 → 2026-07-17") || !strings.Contains(got.VIXChangeBasis, "weekend") {
			t.Errorf("VIXChangeBasis=%q, want official-close session span with the weekend reason", got.VIXChangeBasis)
		}
		if got.Ratio == nil {
			t.Errorf("ratio leg must be untouched by the day-change pin")
		}
		if slices.Contains(got.FieldsMissing, "vix_day_change") {
			t.Errorf("fields_missing=%v, must not flag vix_day_change on a successful pin", got.FieldsMissing)
		}
	})

	// A failed pin withholds the change fields entirely — a drifted tick
	// anchor must never backfill. Series here is stale: Friday never
	// arrived in the bars.
	t.Run("closed_date_without_usable_bars_withholds_change", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			now: regimeTestClosedSunday,
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.77, prevClose: 16.73, dataType: rpc.MarketDataFrozen},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"VIX": {bars: makeDatedBars(map[string]float64{
					"2026-07-15": 16.9,
					"2026-07-16": 16.73,
				})},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeVIXTerm(ctx, deps)
		if got.VIXChangePct != nil || got.VIXPrevClose != nil {
			t.Errorf("change=%v prev=%v, want both nil: tick anchors must not backfill a failed pin", got.VIXChangePct, got.VIXPrevClose)
		}
		if got.VIXChangeBasis != "" {
			t.Errorf("VIXChangeBasis=%q, want empty on a failed pin", got.VIXChangeBasis)
		}
		if !slices.Contains(got.FieldsMissing, "vix_day_change") {
			t.Errorf("fields_missing=%v, want vix_day_change", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "VIX closed-date day change") && strings.Contains(s, "withholding")
		}) {
			t.Errorf("expected a withhold warn naming the missing sessions, got %v", warns)
		}
	})
}

func TestFetchRegimeVolOfVol(t *testing.T) {
	ctx := context.Background()
	points := makeSeries(21, 80.0)
	points[0].Value = 70.0
	points[20].Value = 84.0
	deps := (&fakeDeps{vvix: points}).build()

	got := fetchRegimeVolOfVol(ctx, deps)
	if got.Status != rpc.RegimeStatusOK {
		t.Fatalf("status=%q, want ok (%s)", got.Status, got.ErrorMessage)
	}
	if got.Last == nil || *got.Last != 84.0 {
		t.Fatalf("last=%v, want 84", got.Last)
	}
	if got.Change20D == nil || *got.Change20D < 19.9 || *got.Change20D > 20.1 {
		t.Errorf("change20d=%v, want 20%%", got.Change20D)
	}
	if band := classifyVolOfVolBand(got.Last); band != "green" {
		t.Errorf("band=%q want green", band)
	}
	if got.Source == "" || got.ValueQuality == nil {
		t.Errorf("official source/quality should be populated: %+v", got)
	}
}

func TestFetchRegimeHYGSPY(t *testing.T) {
	ctx := context.Background()

	t.Run("happy_all_fields", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.HYG50DMA == nil || *got.HYG50DMA != 79.5 {
			t.Errorf("HYG50DMA=%v, want 79.5", got.HYG50DMA)
		}
		if got.SPY52WHigh == nil || *got.SPY52WHigh != 540.0 {
			t.Errorf("SPY52WHigh=%v, want 540", got.SPY52WHigh)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("spot_miss_serves_official_close_as_stale", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, prevClose: 525.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
			warnLog: &warns,
		}).build()

		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Fatalf("status=%q, want stale", got.Status)
		}
		if got.HYGPrice == nil || *got.HYGPrice != 79.5 {
			t.Errorf("HYGPrice=%v, want latest official close 79.5", got.HYGPrice)
		}
		if got.HYGDataType != "close" {
			t.Errorf("HYGDataType=%q, want close", got.HYGDataType)
		}
		if got.HYGQuality == nil || !strings.Contains(got.HYGQuality.Source, "official daily close") || !strings.Contains(got.HYGQuality.Source, "spot-miss banding input") {
			t.Errorf("HYGQuality=%+v, want spot-miss official-close provenance", got.HYGQuality)
		}
		if got.HYG50DMA == nil || *got.HYG50DMA != 79.5 {
			t.Errorf("HYG50DMA=%v, want 79.5", got.HYG50DMA)
		}
		if got.SPYPrice == nil || *got.SPYPrice != 530.0 || got.SPYPrevClose == nil || *got.SPYPrevClose != 525.0 || got.SPY52WHigh == nil || *got.SPY52WHigh != 540.0 {
			t.Errorf("SPY fields not populated: price=%v prev_close=%v 52w_high=%v", got.SPYPrice, got.SPYPrevClose, got.SPY52WHigh)
		}
		if !slices.Equal(got.FieldsMissing, []string{"hyg_spot_tick"}) {
			t.Errorf("fields_missing=%v, want exactly [hyg_spot_tick]", got.FieldsMissing)
		}
		if len(warns) != 1 || !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "HYG spot subscribe delivered no tick") && strings.Contains(s, "official close")
		}) {
			t.Errorf("expected exactly one spot-miss fallback warning, got %v", warns)
		}
	})

	t.Run("spot_miss_without_history_stays_error", func(t *testing.T) {
		deps := (&fakeDeps{
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
		}).build()

		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Fatalf("status=%q, want error", got.Status)
		}
		if got.ErrorMessage != "HYG or SPY spot missing" {
			t.Errorf("error_message=%q, want existing missing-spot error", got.ErrorMessage)
		}
	})

	// Day-change anchor: when tick 9 lands alongside SPY's price triple,
	// the fetcher derives both the dollar and percent change. Dashboard
	// header reads these to render "SPY 530.00 +5.00 (+0.95%)" above the
	// indicator rows.
	t.Run("spy_prev_close_populates_change_and_pct", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, prevClose: 525.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPYPrevClose == nil || *got.SPYPrevClose != 525.0 {
			t.Fatalf("SPYPrevClose=%v, want 525.0", got.SPYPrevClose)
		}
		if got.SPYChange == nil || *got.SPYChange < 4.99 || *got.SPYChange > 5.01 {
			t.Errorf("SPYChange=%v, want 5.00", got.SPYChange)
		}
		// (530 - 525) / 525 * 100 ≈ 0.9524
		if got.SPYChangePct == nil || *got.SPYChangePct < 0.95 || *got.SPYChangePct > 0.96 {
			t.Errorf("SPYChangePct=%v, want ≈0.95", got.SPYChangePct)
		}
	})

	// Closed-date pinning, SPY side. Sunday 2026-07-19 live incident: the
	// tick-9 anchor had rolled onto the last print (rendering +0.00%)
	// while Friday's true close-to-close was −0.99%. The pin must ignore
	// both weekend snapshot values, leave the price tick alone, and pull
	// the change from the official closes. week52High is set so the SPY
	// history call is uniquely the pin's (no 365d fallback fetch).
	t.Run("closed_date_pins_spy_change_to_official_closes", func(t *testing.T) {
		fake := &fakeDeps{
			now: regimeTestClosedSunday,
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 750.72, prevClose: 750.72, week52High: 760.0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: makeDatedBars(map[string]float64{
					"2026-07-16": 758.22,
					"2026-07-17": 750.72,
				})},
			},
		}
		got := fetchRegimeHYGSPY(ctx, fake.build())
		if got.SPYPrevClose == nil || *got.SPYPrevClose != 758.22 {
			t.Fatalf("SPYPrevClose=%v, want Thursday official close 758.22", got.SPYPrevClose)
		}
		if got.SPYChange == nil || *got.SPYChange < -7.51 || *got.SPYChange > -7.49 {
			t.Errorf("SPYChange=%v, want −7.50 (official closes)", got.SPYChange)
		}
		if got.SPYChangePct == nil || *got.SPYChangePct > -0.98 || *got.SPYChangePct < -1.0 {
			t.Errorf("SPYChangePct=%v, want ≈−0.99 (Friday close-to-close), not the reset anchor's 0.00", got.SPYChangePct)
		}
		if !strings.Contains(got.SPYChangeBasis, "official closes 2026-07-16 → 2026-07-17") {
			t.Errorf("SPYChangeBasis=%q, want the official-close session span", got.SPYChangeBasis)
		}
		if got.SPYPrice == nil || *got.SPYPrice != 750.72 {
			t.Errorf("SPYPrice=%v, want the tick untouched — only the change fields pin", got.SPYPrice)
		}
		if want := []int{regimeTapePinLookbackDays}; !slices.Equal(fake.historyCalls["SPY"], want) {
			t.Errorf("SPY history calls=%v, want %v (pin window only)", fake.historyCalls["SPY"], want)
		}
	})

	// Holiday flavor of the same calendar key (marketcal StateHoliday):
	// Labor Day Monday 2026-09-07 pins Thu 2026-09-03 → Fri 2026-09-04
	// and the basis names the holiday.
	t.Run("closed_date_holiday_pins_and_names_reason", func(t *testing.T) {
		deps := (&fakeDeps{
			now: time.Date(2026, 9, 7, 15, 0, 0, 0, time.UTC),
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 505.0, week52High: 540.0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: makeDatedBars(map[string]float64{
					"2026-09-03": 500.0,
					"2026-09-04": 505.0,
				})},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPYChangePct == nil || *got.SPYChangePct < 0.99 || *got.SPYChangePct > 1.01 {
			t.Fatalf("SPYChangePct=%v, want ≈+1.00", got.SPYChangePct)
		}
		if !strings.Contains(got.SPYChangeBasis, "official closes 2026-09-03 → 2026-09-04") || !strings.Contains(got.SPYChangeBasis, "Labor Day") {
			t.Errorf("SPYChangeBasis=%q, want session span + holiday name", got.SPYChangeBasis)
		}
	})

	// A failed SPY pin (history error on the closed date) withholds the
	// change fields instead of falling back to drifted weekend anchors.
	t.Run("closed_date_spy_history_error_withholds_change", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			now: regimeTestClosedSunday,
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 750.72, prevClose: 750.72, week52High: 760.0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {err: errors.New("HMDS unavailable")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPYChangePct != nil || got.SPYChange != nil || got.SPYPrevClose != nil {
			t.Errorf("change fields=%v/%v/%v, want all nil on a failed pin", got.SPYChange, got.SPYChangePct, got.SPYPrevClose)
		}
		if !slices.Contains(got.FieldsMissing, "spy_day_change") {
			t.Errorf("fields_missing=%v, want spy_day_change", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "SPY closed-date day change") && strings.Contains(s, "HMDS unavailable")
		}) {
			t.Errorf("expected history-error warn, got %v", warns)
		}
	})

	t.Run("52w_high_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				// SPY tick arrived but Misc-Stats 165 did not — the
				// cold-start case the new helper surfaces honestly
				// instead of returning the stale cache value.
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok (advisory missing field doesn't downgrade)", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, want to include spy_52w_high", got.FieldsMissing)
		}
		if slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, should NOT include hyg_50dma", got.FieldsMissing)
		}
	})

	t.Run("50dma_history_error", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {err: errors.New("history fetch failed")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, want to include hyg_50dma", got.FieldsMissing)
		}
		// Operator-visible diagnostic: the silent-swallow that hid this
		// failure pre-fix is now a warn-level log line carrying the
		// underlying error. Without this the renderer's "hyg_50dma
		// missing" is uninterpretable — was it a timeout? entitlement?
		// transient gateway hiccup? The log answers.
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "HYG 50DMA") && strings.Contains(s, "history fetch failed")
		}) {
			t.Errorf("expected warn log for HYG history error, got %v", warns)
		}
	})

	t.Run("50dma_insufficient_bars_logged", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(30, 79.5)}, // < 50
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.HYG50DMA != nil {
			t.Errorf("HYG50DMA must be nil when bars insufficient, got %v", *got.HYG50DMA)
		}
		if !slices.Contains(got.FieldsMissing, "hyg_50dma") {
			t.Errorf("fields_missing=%v, want hyg_50dma", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "insufficient bars")
		}) {
			t.Errorf("expected warn log for insufficient-bars case, got %v", warns)
		}
	})

	t.Run("spy_spot_missing", func(t *testing.T) {
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			// SPY's rich snapshot didn't land at all
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.Status != rpc.RegimeStatusError {
			t.Errorf("status=%q, want error when SPY spot missing", got.Status)
		}
	})

	t.Run("spy_52w_high_falls_back_to_daily_bars_when_tick_missing", func(t *testing.T) {
		// Off-hours / frozen-mode reproducer. The gateway delivers SPY's
		// bid/ask/last as a single snapshot then goes silent — tick 165
		// (Misc Stats) never lands no matter how long we wait, so
		// snapshotWith52WHigh returns week52High=0. The indicator must
		// derive 52w high from daily bars rather than surface a null
		// field and drop to a 2-state signal at any hour the market is
		// closed.
		bars := make([]ibkrlib.HistoricalBar, 252)
		for i := range bars {
			bars[i] = ibkrlib.HistoricalBar{Close: 500.0, High: 500.0 + float64(i)}
		}
		// The peak is at the last bar (251) by construction; the helper
		// must scan the full 252-bar window, not just the latest.
		bars[100].High = 999.0 // peak somewhere in the middle
		const wantMaxHigh = 999.0

		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh == nil {
			t.Fatalf("SPY52WHigh must fall back to daily-bar max(High) when tick 165 is absent, got nil")
		}
		if *got.SPY52WHigh != wantMaxHigh {
			t.Errorf("SPY52WHigh=%v, want %v (peak in synthetic bars)", *got.SPY52WHigh, wantMaxHigh)
		}
		if slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, must NOT include spy_52w_high once fallback supplied it", got.FieldsMissing)
		}
	})

	t.Run("spy_52w_high_live_tick_takes_precedence_over_fallback", func(t *testing.T) {
		// When the gateway *did* deliver tick 165, that value is the
		// truth — even if our daily-bar derivation would compute
		// something larger (e.g. an intraday spike not yet reflected in
		// the closing-day bar set). Don't second-guess the gateway.
		bars := make([]ibkrlib.HistoricalBar, 252)
		for i := range bars {
			bars[i] = ibkrlib.HistoricalBar{Close: 500.0, High: 999.0}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 600.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh == nil || *got.SPY52WHigh != 600.0 {
			t.Errorf("SPY52WHigh=%v, want 600 (live gateway value, not the 999 we'd derive)", got.SPY52WHigh)
		}
	})

	t.Run("spy_52w_high_fallback_history_error_logged", func(t *testing.T) {
		// Symmetric with the HYG 50DMA history-error case: if the
		// fallback's history fetch fails, the renderer learns *why*
		// spy_52w_high is missing from the daemon log rather than
		// staring at a silent null field. Operator-visible diagnostic,
		// not just an advisory missing-field note.
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataFrozen},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 0, dataType: rpc.MarketDataFrozen},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {err: errors.New("history fetch failed")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeHYGSPY(ctx, deps)
		if got.SPY52WHigh != nil {
			t.Errorf("SPY52WHigh must be nil when both live tick and fallback fail, got %v", *got.SPY52WHigh)
		}
		if !slices.Contains(got.FieldsMissing, "spy_52w_high") {
			t.Errorf("fields_missing=%v, want spy_52w_high", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "SPY") && strings.Contains(s, "52w") && strings.Contains(s, "history fetch failed")
		}) {
			t.Errorf("expected warn log for SPY 52w fallback history error, got %v", warns)
		}
	})

	// Pins the (days int) the production fetcher hands to deps.history
	// against HYGLookbackDays. Without this gate the test seam silently
	// discards the days argument, so a regression that reverts the
	// v0.23.0 holiday-clipping widening (90 → 70 calendar days, commit
	// 02aba13) would pass every other test in this file. The 90-day
	// window absorbs the 9-10 US market holidays per year — without it
	// the 50-bar HMDS fetch comes up short on holiday weeks and
	// hyg_50dma silently disappears from the dashboard.
	t.Run("history_call_pins_HYGLookbackDays", func(t *testing.T) {
		f := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}
		_ = fetchRegimeHYGSPY(ctx, f.build())
		if got := f.historyCalls["HYG"]; len(got) != 1 || got[0] != HYGLookbackDays {
			t.Errorf("HYG history called with days=%v, want exactly [%d] (HYGLookbackDays)", got, HYGLookbackDays)
		}
	})
}

func TestFetchRegimeCreditSpreads(t *testing.T) {
	ctx := context.Background()
	hy := makeSeries(21, 3.50)
	ig := makeSeries(21, 1.10)
	hy[0].Value = 3.20
	hy[20].Value = 3.80
	ig[20].Value = 1.25
	deps := (&fakeDeps{
		series: map[string][]regimeSeriesPoint{
			fredSeriesHYOAS: hy,
			fredSeriesIGOAS: ig,
		},
	}).build()

	got := fetchRegimeCreditSpreads(ctx, deps)
	if got.Status != rpc.RegimeStatusOK {
		t.Fatalf("status=%q, want ok (%s)", got.Status, got.ErrorMessage)
	}
	if got.HYOAS == nil || *got.HYOAS != 3.80 {
		t.Fatalf("hy_oas=%v, want 3.80", got.HYOAS)
	}
	if got.HYIGSpread == nil || *got.HYIGSpread < 2.54 || *got.HYIGSpread > 2.56 {
		t.Errorf("hy_ig_spread=%v, want 2.55", got.HYIGSpread)
	}
	if got.HY20DChange == nil || *got.HY20DChange < 0.59 || *got.HY20DChange > 0.61 {
		t.Errorf("hy_20d_change=%v, want 0.60", got.HY20DChange)
	}
	if band := classifyCreditSpreadsBand(got); band != "yellow" {
		t.Errorf("band=%q want yellow from 20d widening", band)
	}
	if got.Source == "" || got.HYOASQuality == nil || got.SpreadQuality == nil {
		t.Errorf("source/quality should be populated: %+v", got)
	}
}

func TestFetchRegimeCreditSpreadsFetchesFREDPairConcurrently(t *testing.T) {
	ctx := context.Background()
	oldBudget := regimeOfficialSeriesBudget
	regimeOfficialSeriesBudget = 30 * time.Millisecond
	t.Cleanup(func() { regimeOfficialSeriesBudget = oldBudget })

	hy := makeSeries(21, 3.50)
	ig := makeSeries(21, 1.10)
	deps := &regimeDeps{
		officialSeries: func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
			select {
			case <-time.After(20 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			switch seriesID {
			case fredSeriesHYOAS:
				return hy, nil
			case fredSeriesIGOAS:
				return ig, nil
			default:
				return nil, fmt.Errorf("unexpected series %s", seriesID)
			}
		},
	}

	got := fetchRegimeCreditSpreads(ctx, deps)
	if got.Status == rpc.RegimeStatusError {
		t.Fatalf("credit pair should fit per-series budget when fetched concurrently, got error: %s", got.ErrorMessage)
	}
	if got.HYOAS == nil || got.IGOAS == nil {
		t.Fatalf("credit pair missing values after concurrent fetch: %+v", got)
	}
}

func TestFetchRegimeOfficialDailyRowsUsePersistedLastGoodOnRefreshFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()

	seed := newRegimeSeriesCache(dir)
	seed.freshFor = time.Nanosecond
	hy := makeSeries(21, 3.50)
	ig := makeSeries(21, 1.10)
	cp := makeSeries(3, 5.30)
	tb := makeSeries(3, 5.10)
	hy[20].Value = 3.80
	ig[20].Value = 1.25
	cp[2].Value = 5.34
	tb[2].Value = 5.20
	seed.put(fredSeriesHYOAS, hy, now.Add(-13*time.Hour))
	seed.put(fredSeriesIGOAS, ig, now.Add(-13*time.Hour))
	seed.put(fredSeriesCP3M, cp, now.Add(-13*time.Hour))
	seed.put(fredSeriesTBill3M, tb, now.Add(-13*time.Hour))

	// New cache instance mirrors a restarted daemon: memory is empty, only
	// the persisted official daily files remain. The live official-series
	// fetcher then fails like the flapping first-call screenshot.
	cacheAfterRestart := newRegimeSeriesCache(dir)
	cacheAfterRestart.freshFor = time.Nanosecond
	deps := &regimeDeps{
		officialSeries: func(ctx context.Context, seriesID string) ([]regimeSeriesPoint, error) {
			return cacheAfterRestart.fetch(ctx, seriesID, func(context.Context, string) ([]regimeSeriesPoint, error) {
				return nil, errors.New("fetch timed out")
			})
		},
	}

	credit := fetchRegimeCreditSpreads(ctx, deps)
	if credit.Status == rpc.RegimeStatusError || credit.HYOAS == nil || credit.IGOAS == nil {
		t.Fatalf("credit row should use persisted last-good FRED OAS data, got %+v", credit)
	}
	if *credit.HYOAS != 3.80 || *credit.IGOAS != 1.25 {
		t.Fatalf("credit values came from wrong source: hy=%v ig=%v", *credit.HYOAS, *credit.IGOAS)
	}
	funding := fetchRegimeFundingStress(ctx, deps)
	if funding.Status == rpc.RegimeStatusError || funding.SpreadBps == nil {
		t.Fatalf("funding row should use persisted last-good funding data, got %+v", funding)
	}
	if *funding.SpreadBps < 13.9 || *funding.SpreadBps > 14.1 {
		t.Fatalf("funding spread=%v, want 14bp from cached CP/T-bill series", *funding.SpreadBps)
	}
}

func TestFetchRegimeFundingStress(t *testing.T) {
	ctx := context.Background()
	cp := makeSeries(3, 5.30)
	tb := makeSeries(3, 5.10)
	cp[2].Value = 5.34
	tb[2].Value = 5.20
	deps := (&fakeDeps{
		series: map[string][]regimeSeriesPoint{
			fredSeriesCP3M:    cp,
			fredSeriesTBill3M: tb,
		},
	}).build()

	got := fetchRegimeFundingStress(ctx, deps)
	if got.Status != rpc.RegimeStatusOK {
		t.Fatalf("status=%q, want ok (%s)", got.Status, got.ErrorMessage)
	}
	if got.SpreadBps == nil || *got.SpreadBps < 13.9 || *got.SpreadBps > 14.1 {
		t.Fatalf("spread_bps=%v, want 14", got.SpreadBps)
	}
	if band := classifyFundingStressBand(got.SpreadBps); band != "green" {
		t.Errorf("band=%q want green", band)
	}
	if got.Source == "" || got.CP3MQuality == nil || got.SpreadQuality == nil {
		t.Errorf("source/quality should be populated: %+v", got)
	}
}

func TestFetchRegimeFundingStressUsesOfficialFedAndTreasurySources(t *testing.T) {
	ctx := context.Background()
	obsDate := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fed-cp":
			fmt.Fprintf(w, "Header row ignored\nTime Period,RIFSPPFAAD90_N.B\n%s,ND\n%s,3.70\n", time.Now().UTC().AddDate(0, 0, -2).Format("2006-01-02"), obsDate)
		case "/treasury-bills":
			fmt.Fprintf(w, `<feed>
  <entry>
    <content>
      <m:properties xmlns:m="http://schemas.microsoft.com/ado/2007/08/dataservices/metadata" xmlns:d="http://schemas.microsoft.com/ado/2007/08/dataservices">
        <d:INDEX_DATE>%sT00:00:00</d:INDEX_DATE>
        <d:ROUND_B1_CLOSE_13WK_2>3.63</d:ROUND_B1_CLOSE_13WK_2>
      </m:properties>
    </content>
  </entry>
</feed>`, obsDate)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	oldCPURL := fedCommercialPaperRatesURL
	oldTreasuryURL := treasuryBillRatesXMLURL
	fedCommercialPaperRatesURL = srv.URL + "/fed-cp"
	treasuryBillRatesXMLURL = func(string) string { return srv.URL + "/treasury-bills" }
	t.Cleanup(func() {
		fedCommercialPaperRatesURL = oldCPURL
		treasuryBillRatesXMLURL = oldTreasuryURL
	})

	deps := &regimeDeps{officialSeries: fetchOfficialRegimeSeries}
	got := fetchRegimeFundingStress(ctx, deps)
	if got.Status != rpc.RegimeStatusOK {
		t.Fatalf("status=%q, want ok (%s)", got.Status, got.ErrorMessage)
	}
	if got.CP3M == nil || *got.CP3M != 3.70 {
		t.Fatalf("CP3M=%v, want 3.70 from Federal Reserve DDP", got.CP3M)
	}
	if got.TBill3M == nil || *got.TBill3M != 3.63 {
		t.Fatalf("TBill3M=%v, want 3.63 from Treasury bill XML", got.TBill3M)
	}
	if got.SpreadBps == nil || *got.SpreadBps < 6.9 || *got.SpreadBps > 7.1 {
		t.Fatalf("SpreadBps=%v, want 7bp", got.SpreadBps)
	}
	if got.Source != "Federal Reserve Commercial Paper DDP + U.S. Treasury Daily Treasury Bill Rates" {
		t.Fatalf("Source=%q, want official Fed/Treasury source label", got.Source)
	}
	if got.CP3MQuality == nil || !strings.Contains(got.CP3MQuality.Source, "Federal Reserve") {
		t.Fatalf("CP quality source=%+v, want Federal Reserve provenance", got.CP3MQuality)
	}
	if got.TBill3MQuality == nil || !strings.Contains(got.TBill3MQuality.Source, "Treasury") {
		t.Fatalf("T-bill quality source=%+v, want Treasury provenance", got.TBill3MQuality)
	}
}

func TestFetchRegimeUSDJPY(t *testing.T) {
	ctx := context.Background()

	t.Run("happy_live", func(t *testing.T) {
		// Construct 10 bars with closes 150..159 — most-recent is 159,
		// 7-trading-days-ago bar is index len-8 = 2, close 152.
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Close: float64(150 + i)}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Fatalf("status=%q, want ok", got.Status)
		}
		if got.Close7DAgo == nil || *got.Close7DAgo != 152.0 {
			t.Errorf("Close7DAgo=%v, want 152", got.Close7DAgo)
		}
		if got.WeeklyChange == nil {
			t.Fatalf("WeeklyChange should be populated")
		}
		// (160 - 152) / 152 * 100 ≈ 5.263
		if *got.WeeklyChange < 5.26 || *got.WeeklyChange > 5.27 {
			t.Errorf("WeeklyChange=%v, want ≈5.263", *got.WeeklyChange)
		}
		if len(got.FieldsMissing) != 0 {
			t.Errorf("fields_missing=%v, want empty", got.FieldsMissing)
		}
	})

	t.Run("thin_history", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: makeBars(3, 158.0)}, // < 8 bars
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusOK {
			t.Errorf("status=%q, want ok (live FX, advisory history miss)", got.Status)
		}
		if !slices.Contains(got.FieldsMissing, "close_7d_ago") {
			t.Errorf("fields_missing=%v, want close_7d_ago", got.FieldsMissing)
		}
		if !slices.Contains(got.FieldsMissing, "weekly_change_pct") {
			t.Errorf("fields_missing=%v, want weekly_change_pct", got.FieldsMissing)
		}
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "USD.JPY") && strings.Contains(s, "insufficient")
		}) {
			t.Errorf("expected warn log for thin USD.JPY history, got %v", warns)
		}
	})

	t.Run("fx_history_error_logged", func(t *testing.T) {
		var warns []string
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {err: errors.New("contract details unresolved")},
			},
			warnLog: &warns,
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Close7DAgo != nil {
			t.Errorf("Close7DAgo must be nil on history error")
		}
		// Operator-visible: this is the FX-race case that bit us in
		// review — pre-fix the daemon silently swallowed the
		// contract-resolution timeout, leaving users to guess at the
		// missing weekly_change_pct.
		if !slices.ContainsFunc(warns, func(s string) bool {
			return strings.Contains(s, "USD.JPY") && strings.Contains(s, "contract details")
		}) {
			t.Errorf("expected warn log for FX history error, got %v", warns)
		}
	})

	t.Run("fx_missing", func(t *testing.T) {
		deps := (&fakeDeps{}).build() // no snapshots
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusUnavailable {
			t.Errorf("status=%q, want unavailable when FX tick absent", got.Status)
		}
	})

	t.Run("fx_tick_missing_uses_midpoint_history_fallback", func(t *testing.T) {
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Date: fmt.Sprintf("202605%02d", 18+i), Close: float64(150 + i)}
		}
		deps := (&fakeDeps{
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Fatalf("status=%q, want stale midpoint fallback", got.Status)
		}
		if got.Last == nil || *got.Last != 159.0 {
			t.Fatalf("Last=%v, want latest midpoint close 159", got.Last)
		}
		if got.Close7DAgo == nil || *got.Close7DAgo != 152.0 {
			t.Fatalf("Close7DAgo=%v, want 152", got.Close7DAgo)
		}
		if got.WeeklyChange == nil || *got.WeeklyChange < 4.60 || *got.WeeklyChange > 4.61 {
			t.Fatalf("WeeklyChange=%v, want ≈4.605", got.WeeklyChange)
		}
		if got.DataType != regimeFreshnessDailyClose {
			t.Fatalf("DataType=%q, want %q", got.DataType, regimeFreshnessDailyClose)
		}
		if got.LastQuality == nil || !strings.Contains(got.LastQuality.Source, "fallback") {
			t.Fatalf("LastQuality=%+v, want fallback provenance", got.LastQuality)
		}
	})

	t.Run("stale_data_type", func(t *testing.T) {
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Close: float64(150 + i)}
		}
		deps := (&fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataDelayed},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}).build()
		got := fetchRegimeUSDJPY(ctx, deps)
		if got.Status != rpc.RegimeStatusStale {
			t.Errorf("delayed tick: status=%q, want stale", got.Status)
		}
	})

	// Pins the (days int) the production fetcher hands to deps.history
	// against USDJPYLookbackDays. Without this gate the test seam
	// silently discards the days argument, so a regression that reverts
	// the v0.23.0 holiday-clipping widening (14 → 12 calendar days,
	// commit 02aba13) would pass every other test in this file. The
	// 14-day window absorbs Monday US bank holidays that clip one
	// US-tradable FX day each — without it the 7-trading-day close
	// silently disappears the week of MLK Day / Memorial Day / Labor
	// Day / Thanksgiving and weekly_change_pct goes missing.
	t.Run("history_call_pins_USDJPYLookbackDays", func(t *testing.T) {
		bars := make([]ibkrlib.HistoricalBar, 10)
		for i := range 10 {
			bars[i] = ibkrlib.HistoricalBar{Close: float64(150 + i)}
		}
		f := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 160.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: bars},
			},
		}
		_ = fetchRegimeUSDJPY(ctx, f.build())
		if got := f.historyCalls["USD.JPY"]; len(got) != 1 || got[0] != USDJPYLookbackDays {
			t.Errorf("USD.JPY history called with days=%v, want exactly [%d] (USDJPYLookbackDays)", got, USDJPYLookbackDays)
		}
	})
}

// TestRegime_CallSequence_HonestUnavailable pins the contract for what
// happens when a regime fetcher's data lands on call N and then doesn't
// land on call N+1. The daemon does NOT keep a last-known-good cache;
// the affected field becomes nil and the row's status honestly reports
// error or unavailable. A renderer can detect the transition by diffing
// the wire shape between two calls — exactly what release-verify.sh's
// regime-twice step does against a live gateway.
//
// The five sub-cases cover the live-gateway risk surface called out by
// the senior verification review: VIX3M timing out (RegimeStatusError),
// USD.JPY FX tick missing (history fallback, RegimeStatusStale), HYG spot
// missing (official-close fallback, RegimeStatusStale), SPY tick 165 missing
// (row stays ok, field moves into FieldsMissing), and the SPY 52w-high
// fallback collapsing when history thins (advisory field stays missing
// rather than holding the prior bar).
func TestRegime_CallSequence_HonestUnavailable(t *testing.T) {
	ctx := context.Background()

	t.Run("vix3m_drops_on_call_2_flips_ok_to_error", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"VIX":   {price: 18.0, dataType: rpc.MarketDataLive},
				"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive},
			},
		}
		built := deps.build()

		first := fetchRegimeVIXTerm(ctx, built)
		if first.Status != rpc.RegimeStatusOK {
			t.Fatalf("call 1 status=%q, want ok (precondition)", first.Status)
		}

		// VIX3M tick stops arriving on call 2 — simulate the thin-CBOE
		// off-hours case the daemon already handles inline.
		delete(deps.snapshots, "VIX3M")

		second := fetchRegimeVIXTerm(ctx, built)
		if second.Status != rpc.RegimeStatusError {
			t.Errorf("call 2 status=%q, want error (rule 12: honest report when tick drops, no stale-cache layer)", second.Status)
		}
		if second.Ratio != nil {
			t.Errorf("call 2 ratio=%v, want nil (rule 12: must not carry over the call-1 value)", *second.Ratio)
		}
		if second.VIX == nil || *second.VIX != 18.0 {
			t.Errorf("call 2 VIX=%v, want 18.0 (the surviving leg still surfaces)", second.VIX)
		}
	})

	t.Run("usdjpy_drops_on_call_2_uses_history_fallback_without_carryover", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"USD.JPY": {price: 149.5, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"USD.JPY": {bars: makeBars(14, 149.0)},
			},
		}
		built := deps.build()

		first := fetchRegimeUSDJPY(ctx, built)
		if first.Status != rpc.RegimeStatusOK {
			t.Fatalf("call 1 status=%q, want ok (precondition)", first.Status)
		}

		// FX tick stops on call 2 — the IDEALPRO entitlement case.
		delete(deps.snapshots, "USD.JPY")

		second := fetchRegimeUSDJPY(ctx, built)
		if second.Status != rpc.RegimeStatusStale {
			t.Errorf("call 2 status=%q, want stale HMDS fallback", second.Status)
		}
		if second.Last == nil || *second.Last != 149.0 {
			t.Errorf("call 2 last=%v, want latest midpoint close 149.0 (no carry-over from live tick)", second.Last)
		}
	})

	t.Run("hyg_spot_drops_on_call_2_uses_official_close_without_carryover", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}
		built := deps.build()

		first := fetchRegimeHYGSPY(ctx, built)
		if first.Status != rpc.RegimeStatusOK {
			t.Fatalf("call 1 status=%q, want ok (precondition)", first.Status)
		}

		delete(deps.snapshots, "HYG")

		second := fetchRegimeHYGSPY(ctx, built)
		if second.Status != rpc.RegimeStatusStale {
			t.Errorf("call 2 status=%q, want stale official-close fallback", second.Status)
		}
		if second.HYGPrice == nil || *second.HYGPrice != 79.5 {
			t.Errorf("call 2 HYGPrice=%v, want official close 79.5 (no carry-over from live tick 80.0)", second.HYGPrice)
		}
		if second.HYGDataType != "close" {
			t.Errorf("call 2 HYGDataType=%q, want close", second.HYGDataType)
		}
	})

	t.Run("spy_tick165_drops_on_call_2_field_moves_to_missing", func(t *testing.T) {
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, week52High: 540.0, dataType: rpc.MarketDataLive},
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
			},
		}
		built := deps.build()

		first := fetchRegimeHYGSPY(ctx, built)
		if first.Status != rpc.RegimeStatusOK || first.SPY52WHigh == nil {
			t.Fatalf("call 1: want ok with SPY52WHigh populated, got status=%q SPY52WHigh=%v", first.Status, first.SPY52WHigh)
		}

		// Tick 165 (Misc Stats) stops landing on call 2; price still
		// lands, but the 52w-high reads zero. With no history fallback
		// configured, the advisory field stays missing.
		deps.rich["SPY"] = fakeRichQuote{price: 530.0, dataType: rpc.MarketDataLive}

		second := fetchRegimeHYGSPY(ctx, built)
		if second.Status != rpc.RegimeStatusOK {
			t.Errorf("call 2 status=%q, want ok (primary measurements landed; advisory missing doesn't downgrade row)", second.Status)
		}
		if second.SPY52WHigh != nil {
			t.Errorf("call 2 SPY52WHigh=%v, want nil (rule 12: no carry-over from call 1)", *second.SPY52WHigh)
		}
		if !slices.Contains(second.FieldsMissing, "spy_52w_high") {
			t.Errorf("call 2 fields_missing=%v, want to include spy_52w_high", second.FieldsMissing)
		}
	})

	t.Run("spy_52w_fallback_collapses_when_history_thins", func(t *testing.T) {
		// Call 1: tick 165 missing but the daily-bars fallback supplies
		// the 52w high. Call 2: history returns <50 bars, fallback bails,
		// the advisory field stays missing rather than echoing call 1.
		//
		// The fallback reads HistoricalBar.High (see maxHigh in regime.go);
		// makeBars sets only Close, so bars are populated by hand here.
		spyBars := make([]ibkrlib.HistoricalBar, 200)
		for i := range spyBars {
			spyBars[i] = ibkrlib.HistoricalBar{Close: 530.0, High: 540.0}
		}
		deps := &fakeDeps{
			snapshots: map[string]fakeQuote{
				"HYG": {price: 80.0, dataType: rpc.MarketDataLive},
			},
			rich: map[string]fakeRichQuote{
				"SPY": {price: 530.0, dataType: rpc.MarketDataLive}, // tick 165 absent
			},
			bars: map[string]fakeHistory{
				"HYG": {bars: makeBars(60, 79.5)},
				"SPY": {bars: spyBars}, // fallback succeeds
			},
		}
		built := deps.build()

		first := fetchRegimeHYGSPY(ctx, built)
		if first.SPY52WHigh == nil {
			t.Fatalf("call 1: SPY 252d max(High) fallback should have populated SPY52WHigh")
		}

		// History fallback collapses on call 2 (gateway returns a
		// truncated bar set). Both primary and fallback paths fail.
		deps.bars["SPY"] = fakeHistory{bars: spyBars[:30]}

		second := fetchRegimeHYGSPY(ctx, built)
		if second.SPY52WHigh != nil {
			t.Errorf("call 2 SPY52WHigh=%v, want nil (rule 12: when both primary and fallback drop, the field disappears honestly)", *second.SPY52WHigh)
		}
		if !slices.Contains(second.FieldsMissing, "spy_52w_high") {
			t.Errorf("call 2 fields_missing=%v, want spy_52w_high", second.FieldsMissing)
		}
	})
}

// TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope pins the
// v0.27.6 fix for the regression where a single stuck fetcher hung
// the whole regime call past the daemon's deadline. The contract:
// when ctx fires, the fan-out returns within a few milliseconds with
// a fully-populated envelope where any not-yet-completed row carries
// TestBoundedSnapshot_ReturnsWithinBudgetUnderInnerBlock pins the
// per-fetcher wall-time bound. Without this, a deps.snapshot that
// blocks past its declared budget (e.g. inner SubscribeMarketData
// stalls on a saturated market-data semaphore using Connection.ctx
// instead of the caller's ctx) hangs the fetcher past the
// orchestrator's 45 s deadline and the partial-envelope path fires
// for rows that should have errored cleanly at 5 s. Production
// signature from 2026-05-19: "regime fan-out exceeded handler
// deadline" for three rows that historically returned cleanly.
func TestBoundedSnapshot_ReturnsWithinBudgetUnderInnerBlock(t *testing.T) {
	savedSlack := boundedSnapshotSlack
	boundedSnapshotSlack = 20 * time.Millisecond
	t.Cleanup(func() { boundedSnapshotSlack = savedSlack })

	// A snapshot dep that blocks forever — simulates the inner code
	// not honouring ctx. The bounded wrapper must STILL return at
	// budget + slack, with zero values.
	block := make(chan struct{})
	defer close(block)
	deps := &regimeDeps{
		snapshot: func(_ context.Context, _ string, _ time.Duration) (float64, float64, string) {
			<-block
			return 99.99, 99.99, rpc.MarketDataLive
		},
		snapshotWith52WHigh: func(_ context.Context, _ string, _ time.Duration) (float64, float64, float64, string) {
			<-block
			return 99.99, 99.99, 99.99, rpc.MarketDataLive
		},
	}

	start := time.Now()
	price, prev, dt := boundedSnapshot(context.Background(), deps, "VIX", 100*time.Millisecond)
	elapsed := time.Since(start)

	// budget 100ms + test slack = 120ms; assert <500ms to leave room for
	// scheduler jitter without being so loose the test loses its
	// meaning.
	if elapsed > 500*time.Millisecond {
		t.Errorf("boundedSnapshot took %v with inner block; want <500ms (the helper must bound regardless of inner-code behaviour)", elapsed)
	}
	// Zero values returned when budget fires before the inner returns.
	if price != 0 || prev != 0 || dt != "" {
		t.Errorf("boundedSnapshot returned (%v, %v, %q); want zeros on budget timeout", price, prev, dt)
	}

	// Same contract for snapshotWith52WHigh.
	start = time.Now()
	p2, pc2, w52, dt2 := boundedSnapshotWith52WHigh(context.Background(), deps, "SPY", 100*time.Millisecond)
	elapsed = time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Errorf("boundedSnapshotWith52WHigh took %v with inner block; want <500ms", elapsed)
	}
	if p2 != 0 || pc2 != 0 || w52 != 0 || dt2 != "" {
		t.Errorf("boundedSnapshotWith52WHigh returned (%v, %v, %v, %q); want zeros on budget timeout", p2, pc2, w52, dt2)
	}
}

// TestBoundedSnapshot_PassesThroughOnFastReturn confirms the helper
// adds no observable latency or value mutation when the inner snapshot
// returns inside the budget — guards against accidentally introducing
// budget-slack lag on the warm path.
func TestBoundedSnapshot_PassesThroughOnFastReturn(t *testing.T) {
	t.Parallel()
	deps := &regimeDeps{
		snapshot: func(_ context.Context, sym string, _ time.Duration) (float64, float64, string) {
			if sym == "VIX" {
				return 18.5, 19.2, rpc.MarketDataLive
			}
			return 0, 0, ""
		},
	}
	start := time.Now()
	price, prev, dt := boundedSnapshot(context.Background(), deps, "VIX", 5*time.Second)
	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("fast snapshot took %v; want <100ms (the helper must not introduce slack on the warm path)", elapsed)
	}
	if price != 18.5 || prev != 19.2 || dt != rpc.MarketDataLive {
		t.Errorf("boundedSnapshot returned (%v, %v, %q); want (18.5, 19.2, %q)", price, prev, dt, rpc.MarketDataLive)
	}
}

// Status=error and an explanatory ErrorMessage.
//
// This is the gate the v0.27.5 test suite was missing — Rule 12
// covered "individual indicator drops" but didn't drive the
// orchestration layer where wg.Wait used to block.
func TestRunRegimeFanout_ReturnsOnCtxDoneWithPartialEnvelope(t *testing.T) {
	// HYG is the stuck fetcher; it blocks until the test closes the
	// channel. The other four return immediately so they should land
	// before the 200 ms deadline.
	block := make(chan struct{})
	defer close(block) // unblock the stuck goroutine on test exit so it doesn't leak

	stuckHYG := func(_ context.Context) rpc.RegimeHYGSPYDivergence {
		<-block
		return rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusOK}
	}
	fastVIX := func(_ context.Context) rpc.RegimeVIXTerm {
		return rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK}
	}
	fastVolOfVol := func(_ context.Context) rpc.RegimeVolOfVol {
		return rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Symbol: "VVIX"}
	}
	fastCreditSpreads := func(_ context.Context) rpc.RegimeCreditSpreads {
		return rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK}
	}
	fastFundingStress := func(_ context.Context) rpc.RegimeFundingStress {
		return rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK}
	}
	fastUSDJPY := func(_ context.Context) rpc.RegimeUSDJPY {
		return rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Symbol: "USD.JPY"}
	}
	fastGamma := func(_ context.Context) rpc.RegimeGammaZero {
		return rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK}
	}
	fastBreadth := func(_ context.Context) rpc.RegimeBreadth {
		return rpc.RegimeBreadth{Status: rpc.RegimeStatusOK}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	contentionMsg := func() string {
		return "regime fan-out exceeded handler deadline (test)"
	}
	start := time.Now()
	res := runRegimeFanout(ctx,
		fastVIX, fastVolOfVol, stuckHYG,
		fastCreditSpreads, fastFundingStress,
		fastUSDJPY, fastGamma, fastBreadth,
		contentionMsg)
	elapsed := time.Since(start)

	// The whole point of the v0.27.6 fix: the handler returns when its
	// ctx fires, not when the stuck fetcher unblocks. 400 ms is a
	// generous ceiling (200 ms ctx + slack); pre-v0.27.6 this would
	// have blocked indefinitely on wg.Wait.
	if elapsed > 400*time.Millisecond {
		t.Errorf("runRegimeFanout returned after %v; want <400ms (must respect ctx deadline, not wait for stuck fetcher)", elapsed)
	}

	// The stuck row must surface as Status=error with the partial-envelope
	// message, NOT as a zero-valued struct (which would let the renderer
	// mistake "stuck" for "no data ever requested").
	if res.HYGSPYDivergence.Status != rpc.RegimeStatusError {
		t.Errorf("HYG row Status=%q, want error (stuck fetcher must surface as error after ctx fires)", res.HYGSPYDivergence.Status)
	}
	if !strings.Contains(res.HYGSPYDivergence.ErrorMessage, "fan-out exceeded handler deadline") {
		t.Errorf("HYG row ErrorMessage=%q, want substring 'fan-out exceeded handler deadline'", res.HYGSPYDivergence.ErrorMessage)
	}
	if res.HYGSPYDivergence.Notes == "" {
		t.Errorf("HYG row Notes must be populated even on partial-envelope path so renderer has spec context")
	}

	// The four fast fetchers landed before the deadline; their status
	// must come through unchanged.
	if res.VIXTermStructure.Status != rpc.RegimeStatusOK {
		t.Errorf("VIX row Status=%q, want ok (fetcher returned before deadline)", res.VIXTermStructure.Status)
	}
	if res.USDJPY.Status != rpc.RegimeStatusOK {
		t.Errorf("USD.JPY row Status=%q, want ok", res.USDJPY.Status)
	}
	if res.GammaZero.Status != rpc.RegimeStatusOK {
		t.Errorf("gamma row Status=%q, want ok", res.GammaZero.Status)
	}
	if res.Breadth.Status != rpc.RegimeStatusOK {
		t.Errorf("breadth row Status=%q, want ok", res.Breadth.Status)
	}
}

// TestRunRegimeFanout_PartialEnvelopeUsesContentionMessage pins the
// commit-1 contract: when ctx fires before all rows return, the
// orchestrator calls contentionMsg() fresh and writes its return
// string verbatim onto the not-received rows' ErrorMessage. Closure
// invocation (vs constant message capture) is what lets the message
// reflect daemon state at deadline time, not handler-entry time.
func TestRunRegimeFanout_PartialEnvelopeUsesContentionMessage(t *testing.T) {
	block := make(chan struct{})
	defer close(block)

	stuckHYG := func(_ context.Context) rpc.RegimeHYGSPYDivergence {
		<-block
		return rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusOK}
	}
	fastVIX := func(_ context.Context) rpc.RegimeVIXTerm {
		return rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK}
	}
	fastVolOfVol := func(_ context.Context) rpc.RegimeVolOfVol {
		return rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Symbol: "VVIX"}
	}
	fastCreditSpreads := func(_ context.Context) rpc.RegimeCreditSpreads {
		return rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK}
	}
	fastFundingStress := func(_ context.Context) rpc.RegimeFundingStress {
		return rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK}
	}
	fastUSDJPY := func(_ context.Context) rpc.RegimeUSDJPY {
		return rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Symbol: "USD.JPY"}
	}
	fastGamma := func(_ context.Context) rpc.RegimeGammaZero {
		return rpc.RegimeGammaZero{Status: rpc.RegimeStatusOK}
	}
	fastBreadth := func(_ context.Context) rpc.RegimeBreadth {
		return rpc.RegimeBreadth{Status: rpc.RegimeStatusOK}
	}

	const fingerprint = "test-contention-fingerprint-Δ" // Δ proves the closure result is written verbatim
	contentionMsg := func() string {
		return fingerprint
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res := runRegimeFanout(ctx,
		fastVIX, fastVolOfVol, stuckHYG,
		fastCreditSpreads, fastFundingStress,
		fastUSDJPY, fastGamma, fastBreadth,
		contentionMsg)

	if res.HYGSPYDivergence.ErrorMessage != fingerprint {
		t.Errorf("HYG ErrorMessage=%q, want %q (the orchestrator must call contentionMsg() fresh at deadline time and write its return verbatim)", res.HYGSPYDivergence.ErrorMessage, fingerprint)
	}
}

// TestRegimeContentionMessage_NamesRunningTasks pins commit-1's
// expansion side: a Server with breadth-spx and/or gamma-zero in
// flight produces a contention message that names each running task
// by its BackgroundTaskStatus name. The empty-list case falls
// through to a gateway-side hedge so the message is always
// actionable.
func TestRegimeContentionMessage_NamesRunningTasks(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		zeroGamma: newGammaZeroCache(),
	}

	// 1. No tasks running → gateway-side hedge.
	msg := srv.regimeContentionMessage()
	if !strings.Contains(msg, "no daemon-internal contention detected") {
		t.Errorf("idle daemon: regimeContentionMessage()=%q; want a 'no daemon-internal contention' hedge", msg)
	}

	// The acquisition advertises itself to the idle watcher, but must not
	// diagnose itself as contention when its own fan-out times out.
	srv.regimeSnapshots = &regimeSnapshotCache{refresh: &regimeSnapshotRefresh{done: make(chan struct{})}}
	msg = srv.regimeContentionMessage()
	if strings.Contains(msg, "regime-refresh") || !strings.Contains(msg, "no daemon-internal contention detected") {
		t.Errorf("self-only refresh: regimeContentionMessage()=%q", msg)
	}

	// 2. Gamma in flight → message names gamma-zero. Synthetic job
	// injected via the combined slot (the canonical cache cell for
	// dashboard / regime callers).
	fakeJob := &gammaComputation{
		sessionKey: "2026-05-19",
		scope:      rpc.GammaZeroScopeCombined,
		startedAt:  time.Now(),
		done:       make(chan struct{}),
	}
	srv.zeroGamma.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: fakeJob},
	}
	msg = srv.regimeContentionMessage()
	if !strings.Contains(msg, "gamma-zero") {
		t.Errorf("gamma in flight: regimeContentionMessage()=%q; want substring 'gamma-zero'", msg)
	}
	if strings.Contains(msg, "breadth-spx") {
		t.Errorf("gamma in flight, breadth idle: message=%q; should NOT name breadth-spx", msg)
	}
	if strings.Contains(msg, "regime-refresh") {
		t.Errorf("gamma plus own refresh: message=%q; should NOT name regime-refresh", msg)
	}
	close(fakeJob.done) // tidy up the synthetic computation
}

// TestClassifyHorizonAgreement_ThreeBucketSplit pins the v3 enum-set
// the classifier returns over the new 0DTE / 1-7 / term horizon
// split. The canonical actionable label is "diverge:0dte_vs_term"
// (short-fuse flow disagrees with monthly positioning); the rest
// describe agreement and partial-data cases.
func TestClassifyHorizonAgreement_ThreeBucketSplit(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }
	const spot = 5000.0
	cases := []struct {
		name string
		zero *float64
		one  *float64
		term *float64
		want string
	}{
		{"no_data_all_nil", nil, nil, nil, ""},
		{"0dte_only", ptr(4900), nil, nil, "0dte_only"},
		{"1to7_only", nil, ptr(4900), nil, "1to7_only"},
		{"term_only", nil, nil, ptr(4900), "term_only"},
		{"all_long", ptr(4800), ptr(4850), ptr(4900), "all_long"},
		{"all_short", ptr(5200), ptr(5250), ptr(5300), "all_short"},
		{"diverge_0dte_vs_term", ptr(4900), ptr(4950), ptr(5100), "diverge:0dte_vs_term"},
		{"diverge_term_vs_0dte", ptr(5100), ptr(5050), ptr(4900), "diverge:0dte_vs_term"},
		{"diverge_partial_1to7_alone_disagrees", ptr(4900), ptr(5100), ptr(4900), "diverge:partial"},
		{"diverge_partial_two_only", ptr(4900), nil, ptr(5100), "diverge:0dte_vs_term"},
		{"diverge_partial_1to7_term_only", nil, ptr(4900), ptr(5100), "diverge:partial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &rpc.GammaZeroComputed{
				SpotUnderlying: spot,
				ZeroGamma0DTE:  tc.zero,
				ZeroGamma1to7:  tc.one,
				ZeroGammaTerm:  tc.term,
			}
			if got := classifyHorizonAgreement(c); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestClassifyHorizonAgreement_SignOnlyBucketsCanDiverge(t *testing.T) {
	c := &rpc.GammaZeroComputed{
		SpotUnderlying: 5000,
		GammaSign0DTE:  "negative",
		GammaSignTerm:  "positive",
	}
	if got := classifyHorizonAgreement(c); got != "diverge:0dte_vs_term" {
		t.Errorf("got %q, want diverge:0dte_vs_term", got)
	}
}
