package edge

import (
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A synthetic session of 78 five-minute bars, its first hour falling to a
// look price of 97 under a prior low of 100: day high 99, day low 96, so the
// cut level is 96, the trims 97.5 (the midpoint) and 100 (the prior low), and
// with an average range of 4 the stop level is 92.
var levelTestSession = LevelSession{Date: "2026-03-10", Open: time.Date(2026, 3, 10, 13, 30, 0, 0, time.UTC), Close: time.Date(2026, 3, 10, 20, 0, 0, 0, time.UTC), Prior: "2026-03-09"}

func levelTestBars(set map[int][4]float64, after [4]float64) []LevelBar {
	first := map[int][4]float64{0: {98, 99, 97.8, 98}, 6: {97, 97.5, 96, 97}, 11: {97.2, 97.4, 96.8, 97}}
	var bars []LevelBar
	for i := range 78 {
		ohlc := [4]float64{98, 98.4, 97.6, 98}
		if i >= 12 {
			ohlc = after
		}
		if v, ok := first[i]; ok {
			ohlc = v
		}
		if v, ok := set[i]; ok {
			ohlc = v
		}
		bars = append(bars, LevelBar{Start: levelTestSession.Open.Add(time.Duration(i) * 5 * time.Minute), Open: ohlc[0], High: ohlc[1], Low: ohlc[2], Close: ohlc[3]})
	}
	return bars
}

func levelTestDay(bars []LevelBar, prior rpc.MarketReadPrior, close float64) LevelDayInput {
	high, low := bars[0].High, bars[0].Low
	for _, b := range bars {
		high, low = max(high, b.High), min(low, b.Low)
	}
	return LevelDayInput{Day: LevelStockDay{Symbol: "TESTA", Currency: "USD", Session: levelTestSession, Contracts: 3}, Prior: prior, ATR14: 4, DayHigh: high, DayLow: low, Close: close, Bars: bars}
}

var levelTestPrior = rpc.MarketReadPrior{Date: "2026-03-09", High: 110, Low: 100, Close: 105}

var flatAfterLook = [4]float64{97, 97.3, 96.6, 97}

func levelOne(t *testing.T, in LevelDayInput, index []LevelBar) rpc.EdgeLevelsDay {
	t.Helper()
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{in}, Index: map[string][]LevelBar{in.Day.Session.Date: index}})
	if len(out.Days) != 1 {
		t.Fatalf("replayed %d days, excluded %v", len(out.Days), out.Excluded)
	}
	return out.Days[0]
}

func levelPct(t *testing.T, draft rpc.EdgeLevelsDraft, version, size string) float64 {
	t.Helper()
	r, ok := levelResult(&draft, version, size)
	if !ok {
		t.Fatalf("no %s %s result", version, size)
	}
	return r.ClosePct
}

// cutAndBack: two closes under 96 (95.5, then 95.0: the cut sells there),
// then two closes back above it (96.5, then 97.0: the buy back buys there).
var cutAndBack = map[int][4]float64{12: {96.5, 96.7, 95.4, 95.5}, 13: {95.5, 95.8, 94.9, 95.0}, 14: {95, 96.6, 94.9, 96.5}, 15: {96.5, 97.2, 96.3, 97.0}}

func TestLevelReplayDraftsALosingRowAtTheLookWithFrozenLevels(t *testing.T) {
	d := levelOne(t, levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98), nil)
	if d.Row != rpc.EdgeLevelsRowLosing || !d.DownOnDay || d.Later != nil {
		t.Fatalf("row %s down %v later %v", d.Row, d.DownOnDay, d.Later)
	}
	look := d.Look
	if !look.Moment.Equal(levelTestSession.Open.Add(time.Hour)) || look.MomentPrice != 97 {
		t.Fatalf("moment %s at %v", look.Moment, look.MomentPrice)
	}
	near(t, "cut", look.Cut, 96)
	near(t, "stop", look.Stop, 92)
	near(t, "mid", look.DayMid, 97.5)
	if !slices.Equal(look.Trims, []float64{97.5, 100}) {
		t.Fatalf("trims %v", look.Trims)
	}
}

func TestLevelReplayLookAtHalfAnHourFreezesTheLevelsThen(t *testing.T) {
	in := levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98)
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{in}, LookMinutes: 30})
	if len(out.Days) != 1 {
		t.Fatalf("excluded %v", out.Excluded)
	}
	look := out.Days[0].Look
	// The six bars to 14:00 trade 97.6 to 99, the look price is 98: under
	// the prior low of 100, so losing, with the cut at the half-hour low.
	if !look.Moment.Equal(levelTestSession.Open.Add(30*time.Minute)) || look.MomentPrice != 98 || out.Days[0].Row != rpc.EdgeLevelsRowLosing {
		t.Fatalf("moment %s at %v row %s", look.Moment, look.MomentPrice, out.Days[0].Row)
	}
	near(t, "cut", look.Cut, 97.6)
	near(t, "stop", look.Stop, 93.6)
	if !slices.Equal(look.Trims, []float64{98.3, 100}) {
		t.Fatalf("trims %v", look.Trims)
	}
}

func TestLevelReplayCutSellsOnTheSecondCloseAndTheBuyBackOnTheSecondCloseBack(t *testing.T) {
	d := levelOne(t, levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98), nil)
	near(t, "held", levelPct(t, d.Look, rpc.EdgeLevelsVersionHeld, ""), (98.0/97-1)*100)
	near(t, "at once", levelPct(t, d.Look, rpc.EdgeLevelsVersionAtOnce, rpc.EdgeLevelsSizeHalves), ((0.5*97+0.5*98)/97-1)*100)
	// The cut sells everything at 95.0, the second close, not at the touch.
	near(t, "rule", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves), (95.0/97-1)*100)
	// Sold at 95.0, bought back at 97.0, held to the close at 98.
	near(t, "buy back", levelPct(t, d.Look, rpc.EdgeLevelsVersionBuyBack, rpc.EdgeLevelsSizeHalves), ((95.0-97.0+98)/97-1)*100)
	// No index bars: the cut never has the index's agreement, and with no
	// sale by the last hour the rule sells half at that bar's open.
	near(t, "index cut", levelPct(t, d.Look, rpc.EdgeLevelsVersionIndex, rpc.EdgeLevelsSizeHalves), ((0.5*97+0.5*98)/97-1)*100)
}

func TestLevelReplayIndexCutSellsWhenTheIndexIsUnderItsFirstHourLow(t *testing.T) {
	var index []LevelBar
	for i := range 78 {
		b := LevelBar{Start: levelTestSession.Open.Add(time.Duration(i) * 5 * time.Minute), Open: 501, High: 502, Low: 500, Close: 501}
		if i == 13 {
			b.Low, b.Close = 498, 499
		}
		index = append(index, b)
	}
	d := levelOne(t, levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98), index)
	near(t, "index cut", levelPct(t, d.Look, rpc.EdgeLevelsVersionIndex, rpc.EdgeLevelsSizeHalves), (95.0/97-1)*100)
}

func TestLevelReplayTrimsThenStopInHalvesAndThirds(t *testing.T) {
	bars := levelTestBars(map[int][4]float64{12: {97.2, 97.6, 97.0, 97.3}, 13: {97.3, 100.2, 97.2, 100.0}, 14: {93, 93.1, 91.0, 91.5}}, [4]float64{91.5, 91.8, 91.2, 91.5})
	d := levelOne(t, levelTestDay(bars, levelTestPrior, 91.5), nil)
	// Halves: half at the nearer trim, 97.5; the stop sells the rest at 92,
	// the bar having opened above it.
	near(t, "halves", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves), ((0.5*97.5+0.5*92)/97-1)*100)
	// Thirds: a third at each trim, the last third at the stop.
	near(t, "thirds", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeThirds), ((97.5+100+92)/3/97-1)*100)
	// The stop's sale is not bought back: no two closes above the cut follow.
	near(t, "buy back", levelPct(t, d.Look, rpc.EdgeLevelsVersionBuyBack, rpc.EdgeLevelsSizeHalves), ((0.5*97.5+0.5*92)/97-1)*100)
}

func TestLevelReplayStopFillsAtTheOpenOfABarThatOpensThroughIt(t *testing.T) {
	bars := levelTestBars(map[int][4]float64{12: {90, 90.5, 89, 90}}, [4]float64{90, 90.3, 89.8, 90})
	d := levelOne(t, levelTestDay(bars, levelTestPrior, 90), nil)
	near(t, "rule", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves), (90.0/97-1)*100)
}

func TestLevelReplayLastHourSellsTheTrimSizeWhenNoLevelWasHit(t *testing.T) {
	d := levelOne(t, levelTestDay(levelTestBars(nil, flatAfterLook), levelTestPrior, 98), nil)
	near(t, "halves", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves), ((0.5*97+0.5*98)/97-1)*100)
	near(t, "thirds", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeThirds), ((97.0/3+2*98.0/3)/97-1)*100)
	r, _ := levelResult(&d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves)
	if len(r.Events) != 1 || r.Events[0].Kind != "last_hour" || !r.Events[0].At.Equal(levelTestSession.Close.Add(-time.Hour)) {
		t.Fatalf("events %+v", r.Events)
	}
}

func TestLevelReplayWatchedRowDraftedWhenItBreaksThePriorLowLater(t *testing.T) {
	prior := rpc.MarketReadPrior{Date: "2026-03-09", High: 110, Low: 95.2, Close: 105}
	bars := levelTestBars(map[int][4]float64{12: {96.5, 96.7, 95.4, 95.5}, 13: {95.5, 95.8, 94.9, 95.0}}, flatAfterLook)
	d := levelOne(t, levelTestDay(bars, prior, 98), nil)
	if d.Row != rpc.EdgeLevelsRowWatched || !d.DownOnDay || d.Later == nil {
		t.Fatalf("row %s down %v later %v", d.Row, d.DownOnDay, d.Later)
	}
	// At the look the second reading drafts it: one trim, the midpoint.
	if !slices.Equal(d.Look.Trims, []float64{97.5}) {
		t.Fatalf("look trims %v", d.Look.Trims)
	}
	// Drafted again at the close under the prior low, its levels from the
	// day so far then.
	near(t, "later cut", d.Later.Cut, 94.9)
	if !slices.Equal(d.Later.Trims, []float64{95.2, 96.95}) || d.Later.MomentPrice != 95 {
		t.Fatalf("later trims %v at %v", d.Later.Trims, d.Later.MomentPrice)
	}
}

func TestLevelReplayWorkingRowIsNotDrafted(t *testing.T) {
	prior := rpc.MarketReadPrior{Date: "2026-03-09", High: 96.5, Low: 90, Close: 95}
	d := levelOne(t, levelTestDay(levelTestBars(nil, flatAfterLook), prior, 98), nil)
	if d.Row != rpc.EdgeLevelsRowWorking || d.DownOnDay || len(d.Look.Results) != 1 {
		t.Fatalf("row %s down %v results %d", d.Row, d.DownOnDay, len(d.Look.Results))
	}
}

func TestLevelReplayLeavesOutADayWhoseDailyBarDisagreesWithItsBars(t *testing.T) {
	in := levelTestDay(levelTestBars(nil, flatAfterLook), levelTestPrior, 98)
	in.DayHigh = 150
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{in}})
	if len(out.Days) != 0 || out.Excluded[LevelExcludedBasis] != 1 {
		t.Fatalf("days %d excluded %v", len(out.Days), out.Excluded)
	}
	in = levelTestDay(levelTestBars(nil, flatAfterLook)[13:], levelTestPrior, 98)
	out = LevelReplay(LevelReplayInput{Days: []LevelDayInput{in}})
	if out.Excluded[LevelExcludedFirstHour] != 1 {
		t.Fatalf("excluded %v", out.Excluded)
	}
}

func TestLevelReplayCountsCrossingsStallsAndRoundTrips(t *testing.T) {
	// A crossing that comes back (95.9 traded, closed 96.4), then one that
	// holds and is bought back.
	set := map[int][4]float64{12: {96.5, 96.8, 95.9, 96.4}, 20: {96.5, 96.7, 95.4, 95.5}, 21: {95.5, 95.8, 94.9, 95.0}, 22: {95, 96.6, 94.9, 96.5}, 23: {96.5, 97.2, 96.3, 97.0}}
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{levelTestDay(levelTestBars(set, flatAfterLook), levelTestPrior, 98)}})
	_, tallies, split := LevelReplaySummary(out)
	if split != "2026-03-10" {
		t.Fatalf("split %s", split)
	}
	var tally rpc.EdgeLevelsTally
	for _, row := range tallies {
		if row.Reading == rpc.EdgeLevelsReadingPriorLow && row.Half == rpc.EdgeLevelsHalfAll {
			tally = row
		}
	}
	if tally.Days != 1 || tally.Crossings != 2 || tally.CrossingsHeld != 1 || tally.CrossingsNotHeld != 1 || tally.Cuts != 1 || tally.CutsClosedAbove != 1 || tally.RoundTrips != 1 || tally.LowBreaks != 1 || tally.BouncesStalled != 1 {
		t.Fatalf("tally %+v", tally)
	}
	near(t, "wait vs touch", tally.WaitVsTouchPct, (95.0-96)/96*100)
	near(t, "round trip", tally.RoundTripCostPct, (97.0-95)/95*100)
	near(t, "stall", tally.StallShortPct, (97.5-97.0)/97.5*100)
}

func TestLevelReplaySummarySplitsTheSampleInHalves(t *testing.T) {
	early := levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98)
	late := early
	late.Day.Session.Date = "2026-06-10"
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{early, late}})
	sums, _, split := LevelReplaySummary(out)
	if split != "2026-06-10" || out.Days[0].Half != rpc.EdgeLevelsHalfFirst || out.Days[1].Half != rpc.EdgeLevelsHalfSecond {
		t.Fatalf("split %s halves %s %s", split, out.Days[0].Half, out.Days[1].Half)
	}
	for _, s := range sums {
		if s.Reading == rpc.EdgeLevelsReadingPriorLow && s.Row == rpc.EdgeLevelsRowLosing && s.Size == rpc.EdgeLevelsSizeHalves {
			want := map[string]int{rpc.EdgeLevelsHalfAll: 2, rpc.EdgeLevelsHalfFirst: 1, rpc.EdgeLevelsHalfSecond: 1}[s.Half]
			if s.StockDays != want || s.Results[0].Version != rpc.EdgeLevelsVersionHeld || s.Results[2].CloseBeatsHeld != 0 {
				t.Fatalf("%s: %+v", s.Half, s)
			}
		}
	}
}

func TestLevelReplaySampleReadsThePriorSessionsStatement(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(1, "TESTA", "C", 100, "2026-06-19")
	b.option(2, "SPX", "C", 6000, "2026-06-19")
	b.option(3, "TESTB", "C", 50, "2026-06-19")
	b.option(4, "TESTB", "C", 60, "2026-06-19")
	b.option(5, "TESTC", "P", 40, "2026-06-19")
	b.held("2026-03-09", position(1, 3, 5, 1500), position(2, 1, 50, 5000), position(3, 2, 4, 800), position(4, -2, 2, -400), position(5, 1, 2, 200))
	sessions := []LevelSession{
		{Date: "2026-03-10", Prior: "2026-03-09"},
		{Date: "2026-03-11", Prior: "2026-03-10"},
	}
	days, excluded := LevelReplaySample(b.statements(), sessions, func(s string) bool { return s == "SPX" })
	if len(days) != 1 || days[0].Symbol != "TESTA" || days[0].Contracts != 3 || days[0].Session.Date != "2026-03-10" {
		t.Fatalf("days %+v", days)
	}
	if excluded[LevelExcludedIndex] != 1 || excluded[LevelExcludedSpread] != 1 || excluded[LevelExcludedNoPriorStatement] != 1 || len(excluded) != 3 {
		t.Fatalf("excluded %v", excluded)
	}
}

func TestMarketReadHeldPositionCountsTheBoundariesAsInside(t *testing.T) {
	prior := rpc.MarketReadPrior{High: 110, Low: 100}
	for last, want := range map[float64]string{110: rpc.MarketReadInsidePrior, 100: rpc.MarketReadInsidePrior, 110.01: rpc.MarketReadAbovePriorHigh, 99.99: rpc.MarketReadBelowPriorLow} {
		if got := rpc.MarketReadHeldPosition(last, prior); got != want {
			t.Fatalf("%v: %s, want %s", last, got, want)
		}
	}
}
