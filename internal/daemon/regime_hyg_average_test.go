package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// The credit row states the 50-day average it is judged against: its
// level, HYG's distance from it and since which close HYG has been on that
// side, counted on the same daily closes the average is taken from.
// Synthetic closes only.
func TestHYGRowStatesItsAverageAndSinceWhen(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 11, 3, 0, 0, 0, ny) // off-RTH: the latest daily close bands
	read := func(bars []ibkrlib.HistoricalBar) rpc.RegimeHYGSPYDivergence {
		var warns []string
		deps := hygSpotMissDeps(now, &warns)
		deps.history = func(context.Context, string, int) ([]ibkrlib.HistoricalBar, error) { return bars, nil }
		return fetchRegimeHYGSPY(context.Background(), deps)
	}
	series := func(n, below int) []ibkrlib.HistoricalBar {
		bars := make([]ibkrlib.HistoricalBar, 0, n)
		for i := range n {
			d := now.AddDate(0, 0, i-n)
			c := 40.0
			if i >= n-below {
				c = 39.0
			}
			bars = append(bars, ibkrlib.HistoricalBar{Date: d.Format("2006-01-02"), Time: d, Close: c, High: 41})
		}
		return bars
	}
	bars := series(200, 12)
	r := read(bars)
	wantAvg := (38*40.0 + 12*39.0) / 50
	if r.HYG50DMA == nil || *r.HYG50DMA != wantAvg {
		t.Fatalf("average %v, want %v", r.HYG50DMA, wantAvg)
	}
	wantPct := (39 - wantAvg) / wantAvg * 100
	if r.HYGVs50DMAPct == nil || *r.HYGVs50DMAPct != wantPct {
		t.Errorf("distance %v, want %v", r.HYGVs50DMAPct, wantPct)
	}
	first, last := bars[len(bars)-12].Date, bars[len(bars)-1].Date
	if run := r.HYG50DMARun; run == nil || run.Side != "below" || run.Since != first || run.Sessions != 12 || run.Through != last {
		t.Fatalf("run %+v, want below since %s for 12 sessions through %s", run, first, last)
	}
	reading, context := rpc.HYGSPYReading(r), rpc.HYGSPYContext(r)
	for _, want := range []string{
		"HYG 39.00 at close",
		"below 50-day avg 39.76 since " + readingDayOf(t, first),
	} {
		if !strings.Contains(reading, want) {
			t.Errorf("reading %q lacks %q", reading, want)
		}
	}
	for _, want := range []string{
		"50-day average of the closes to " + readingDayOf(t, last) + ", below it at 12 closes running",
		"SPY 530.00",
		"5.4% below its 52-week high",
	} {
		if !strings.Contains(context, want) {
			t.Errorf("context %q lacks %q", context, want)
		}
	}
	if strings.Contains(reading+context, "held") {
		t.Errorf("a row never held red speaks of a hold: %q / %q", reading, context)
	}

	// The cross is said only when the bars reach back to it.
	if all := read(series(200, 0)); all.HYG50DMARun != nil || strings.Contains(rpc.HYGSPYReading(all), "since") {
		t.Errorf("a run as old as the window has no since date: %+v, %q", all.HYG50DMARun, rpc.HYGSPYReading(all))
	}
}

// A live price on the other side of the average from the run of closes
// keeps the closes' since date out of the reading: it would say the wrong
// side.
func TestHYGReadingDropsTheSinceDateWhenTheLivePriceCrossed(t *testing.T) {
	r := rpc.RegimeHYGSPYDivergence{
		HYGPrice:    new(40.5),
		HYG50DMA:    new(40.0),
		HYG50DMARun: &rpc.RegimeAverageRun{Side: "below", Since: "2026-03-02", Sessions: 9, Through: "2026-03-12"},
	}
	got := rpc.HYGSPYReading(r)
	if !strings.Contains(got, "1.2% above 50-day avg 40.00") || strings.Contains(got, "since") {
		t.Errorf("reading %q", got)
	}
}

// The row's red holds after SPY leaves its high until HYG is back at or
// above its average, and the row says so only while that hold, not a
// fresh red, is what makes it red. The fingerprint does not move with the
// wording. Synthetic prices only.
func TestHYGHeldRedIsSaidOnlyWhileTheExitRuleHoldsIt(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	streaks := NewStreakStore(t.TempDir()).cloneForRegimeEvaluation()
	evaluate := func(day time.Time, hyg, spy float64) *rpc.RegimeSnapshotResult {
		res := &rpc.RegimeSnapshotResult{AsOf: day, HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusOK, HYGPrice: new(hyg), HYG50DMA: new(40.0),
			SPYPrice: new(spy), SPY52WHigh: new(100.0), HYGDataType: "live",
		}}
		annotateRegimeMetadata(res, (&Server{}).populateStreaksWithStore(res, streaks))
		return res
	}
	day := func(d int) time.Time { return time.Date(2026, 3, d, 12, 0, 0, 0, ny) }

	evaluate(day(2), 39.5, 99) // HYG below, SPY within 3% of its high
	evaluate(day(3), 39.5, 99) // second session: eligible, latched
	red := evaluate(day(4), 39.5, 98)
	if red.HYGSPYDivergence.Band != "red" || red.HYGSPYDivergence.RedHeld ||
		red.HYGSPYDivergence.Eligibility == nil || !red.HYGSPYDivergence.Eligibility.Latched {
		t.Fatalf("a red on its own terms: band %q, held %v, eligibility %+v", red.HYGSPYDivergence.Band, red.HYGSPYDivergence.RedHeld, red.HYGSPYDivergence.Eligibility)
	}
	if got := rpc.HYGSPYReading(red.HYGSPYDivergence) + red.HYGSPYDivergence.BandReason; strings.Contains(got, "held") {
		t.Errorf("a latched red the red condition still holds speaks of a hold: %q", got)
	}

	held := evaluate(day(5), 39.5, 90) // SPY 10% off its high: yellow by the rule, red by the exit rule
	h := held.HYGSPYDivergence
	if h.Band != "red" || !h.RedHeld || h.Streak == nil || h.Streak.Since != "2026-03-02" {
		t.Fatalf("held red: band %q, held %v, streak %+v", h.Band, h.RedHeld, h.Streak)
	}
	got := rpc.HYGSPYReading(h)
	full := h // with every clause the reading can carry
	full.HYGRange52W = &rpc.RegimeRange52W{Low: 39.45, High: 43.12, Pos: 1}
	full.HYG50DMARun = &rpc.RegimeAverageRun{Side: "below", Since: "2026-01-27", Sessions: 25, Through: "2026-03-04"}
	if long := rpc.HYGSPYReading(full); len([]rune(long)) > 140 {
		t.Errorf("a held row's reading runs %d characters, over one cell: %q", len([]rune(long)), long)
	}
	if !strings.Contains(got, "red held since 2 Mar until HYG regains the avg") {
		t.Errorf("held reading %q", got)
	}
	if !strings.Contains(h.BandReason, "SPY no longer near highs") || strings.Contains(h.BandReason, "hysteresis") ||
		!strings.Contains(h.BandReason, "red since 2 Mar, when HYG fell below its average with shares within 3% of their high; held until HYG is back at or above its average") {
		t.Errorf("held band reason %q", h.BandReason)
	}
	unworded := *held
	unworded.HYGSPYDivergence.RedHeld = false
	unworded.HYGSPYDivergence.BandReason = ""
	unworded.HYGSPYDivergence.HYGVs50DMAPct = nil
	unworded.HYGSPYDivergence.HYG50DMARun = nil
	if rpc.BuildRegimeFingerprint(held) != rpc.BuildRegimeFingerprint(&unworded) {
		t.Error("the wording fields moved the regime fingerprint")
	}

	released := evaluate(day(6), 40, 90) // back at its average
	if r := released.HYGSPYDivergence; r.Band == "red" || r.RedHeld || strings.Contains(rpc.HYGSPYReading(r)+r.BandReason, "held") {
		t.Errorf("released: band %q, held %v, reading %q", r.Band, r.RedHeld, rpc.HYGSPYReading(r))
	}
}

func readingDayOf(t *testing.T, date string) string {
	t.Helper()
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		t.Fatal(err)
	}
	return d.Format("2 Jan")
}
