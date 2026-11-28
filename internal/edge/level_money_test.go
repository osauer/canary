package edge

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

// levelMoneyBook holds three TESTA 100 calls marked 9.00 at the prior close
// (the stock at 105), so the model has a volatility to price the look from.
func levelMoneyBook(t *testing.T, more func(*replayBook)) *LevelOptionBook {
	t.Helper()
	b := newReplayBook(t, "USD")
	b.option(1, "TESTA", "C", 100, "2026-06-19")
	b.held("2026-03-09", position(1, 3, 9, 2700))
	if more != nil {
		more(b)
	}
	return NewLevelOptionBook(b.statements(), b.chain)
}

// levelMoneyPrice is the model's price of the TESTA call at the look (97).
func levelMoneyPrice(t *testing.T) float64 {
	t.Helper()
	vol, ok := mandate.ImpliedVolatility(true, 9, 105, 100, levelYears("2026-03-09", "2026-06-19"))
	if !ok {
		t.Fatal("no volatility")
	}
	return mandate.OptionPrice(true, 97, 100, levelYears("2026-03-10", "2026-06-19"), vol)
}

func levelMoneyOne(t *testing.T, in LevelDayInput, book *LevelOptionBook, index []LevelBar) rpc.EdgeLevelsDay {
	t.Helper()
	out := LevelReplay(LevelReplayInput{Days: []LevelDayInput{in}, Index: map[string][]LevelBar{in.Day.Session.Date: index}, Book: book})
	if len(out.Days) != 1 {
		t.Fatalf("replayed %d days, excluded %v", len(out.Days), out.Excluded)
	}
	return out.Days[0]
}

func levelFlatIndex() []LevelBar {
	var index []LevelBar
	for i := range 78 {
		index = append(index, LevelBar{Start: levelTestSession.Open.Add(time.Duration(i) * 5 * time.Minute), Open: 501, High: 502, Low: 500, Close: 501})
	}
	return index
}

func TestLevelMoneyValuesEachResultOnTheCallsHeldAndSaysWhatIsRecorded(t *testing.T) {
	day := levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98)
	p0 := levelMoneyPrice(t)

	// No price of the call on the session: the close is modelled, and
	// holding a call through a 97 to 98 day gains more than the stock.
	d := levelMoneyOne(t, day, levelMoneyBook(t, nil), nil)
	held, _ := levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, "")
	if held.Option == nil || held.Option.CloseSource != rpc.EdgeLevelsModelled || held.Option.ClosePct <= held.ClosePct {
		t.Fatalf("held on the calls = %+v against %.3f on the stock", held.Option, held.ClosePct)
	}
	// The cut sold everything at 95: the option result is the sale's alone
	// and loses more than the stock's 2%.
	rule, _ := levelResult(&d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves)
	if rule.Option == nil || rule.Option.ClosePct >= rule.ClosePct || rule.Option.ClosePct <= -100 {
		t.Fatalf("rule on the calls = %+v against %.3f on the stock", rule.Option, rule.ClosePct)
	}

	// The statement marks the call 5.00 that session: held's close is
	// recorded, against the modelled price at the look.
	d = levelMoneyOne(t, day, levelMoneyBook(t, func(b *replayBook) { b.held("2026-03-10", position(1, 3, 5, 2700)) }), nil)
	held, _ = levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, "")
	if held.Option.CloseSource != rpc.EdgeLevelsRecorded {
		t.Fatalf("close source = %s", held.Option.CloseSource)
	}
	near(t, "held on the calls, recorded close", held.Option.ClosePct, (5/p0-1)*100)

	// A day whose call has no recorded price at the prior close has no
	// option result, and the share counts it.
	other := day
	other.Day.Symbol = "TESTB"
	d = levelMoneyOne(t, other, levelMoneyBook(t, nil), nil)
	if held, _ = levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, ""); held.Option != nil {
		t.Fatalf("a stock without held calls was priced: %+v", held.Option)
	}
	if share := LevelReplayOptionShare([]rpc.EdgeLevelsDay{d}); share.Unavailable != 1 || share.Days != 0 {
		t.Fatalf("share = %+v", share)
	}
}

func TestLevelMoneyActualIsThePositionAfterHisTradesAtTheirOwnPrices(t *testing.T) {
	day := levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98)
	day.Close5, day.Close5Day = 99, "2026-03-17"
	p0 := levelMoneyPrice(t)
	book := levelMoneyBook(t, func(b *replayBook) {
		// He sold one of three at 4.00 and bought one back at 4.50; the two
		// plus one he ends with are marked 5.00 that session, 6.00 a week on.
		b.trade(1, "2026-03-10", -1, 4, 0, "C", 1, 0)
		b.trade(1, "2026-03-10", 1, 4.5, 0, "O", 1, 0)
		b.held("2026-03-10", position(1, 3, 5, 2700))
		b.held("2026-03-17", position(1, 3, 6, 2700))
	})
	d := levelMoneyOne(t, day, book, nil)
	if d.Actual == nil || d.Actual.Sales != 1 || d.Actual.Buys != 1 || d.Actual.Result == nil {
		t.Fatalf("actual = %+v", d.Actual)
	}
	near(t, "actual at the close", d.Actual.Result.ClosePct, ((4-4.5+3*5)/(3*p0)-1)*100)
	// His add is judged on its own contract: 6.00 a week on against 4.50.
	var owner []rpc.EdgeLevelsEntry
	for _, e := range d.Adds {
		if e.Kind == rpc.EdgeLevelsEntryOwnerAdd {
			owner = append(owner, e)
		}
	}
	if len(owner) != 1 || owner[0].OptionH5Pct == nil || owner[0].OptionSource != rpc.EdgeLevelsRecorded {
		t.Fatalf("owner adds = %+v", owner)
	}
	near(t, "his add", *owner[0].OptionH5Pct, (6/4.5-1)*100)
}

func TestLevelReplayNearerTrimSellsAQuarterOfTheWayUpTheRange(t *testing.T) {
	// The look at 96.30 with a range of 96 to 99: the quarter is 96.75, the
	// midpoint 97.50. The stock then reaches 96.90 and no further.
	bars := levelTestBars(map[int][4]float64{11: {96.5, 96.6, 96.2, 96.3}}, [4]float64{96.5, 96.9, 96.3, 96.5})
	d := levelOne(t, levelTestDay(bars, levelTestPrior, 96.5), nil)
	near(t, "near trim level", d.Look.NearTrim, 96.75)
	near(t, "nearer trim", levelPct(t, d.Look, rpc.EdgeLevelsVersionNearTrim, rpc.EdgeLevelsSizeHalves), ((0.5*96.75+0.5*96.5)/96.3-1)*100)
	// The rule never reached its midpoint trim; it sold half in the last hour.
	near(t, "rule", levelPct(t, d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves), (96.5/96.3-1)*100)
}

func TestLevelLaterBuyBackIsTheFirstCloseBackAboveTheCutLevelWithinFiveSessions(t *testing.T) {
	closes := func(v ...float64) []LevelClose {
		var out []LevelClose
		for i, c := range v {
			out = append(out, LevelClose{Day: time.Date(2026, 3, 11+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), Close: c})
		}
		return out
	}
	day := levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 95.5)
	// The cut level is 96. Two closes under it, then 97: bought there, and
	// judged on the close five sessions after the buy.
	day.Next = closes(95, 95.5, 97, 98, 99, 100, 101, 101.85)
	d := levelOne(t, day, nil)
	e := d.Look.LaterBuyBack
	if e == nil || e.State != rpc.EdgeLevelsEntryBought || e.Day != "2026-03-13" || e.Price != 97 || e.H5Pct == nil {
		t.Fatalf("later buy back = %+v", e)
	}
	near(t, "buy back as an entry", *e.H5Pct, (101.85/97-1)*100)

	day.Next = closes(95, 95.5, 95, 94, 96, 99)
	if e = levelOne(t, day, nil).Look.LaterBuyBack; e == nil || e.State != rpc.EdgeLevelsEntryNever {
		t.Fatalf("five sessions at or under the cut level = %+v", e)
	}
	day.Next = closes(95, 95.5)
	if e = levelOne(t, day, nil).Look.LaterBuyBack; e == nil || e.State != rpc.EdgeLevelsEntryPending {
		t.Fatalf("two sessions closed so far = %+v", e)
	}
	// No cut, no buy back to judge.
	quiet := levelTestDay(levelTestBars(nil, flatAfterLook), levelTestPrior, 97)
	if e = levelOne(t, quiet, nil).Look.LaterBuyBack; e != nil {
		t.Fatalf("a day without a cut carries %+v", e)
	}
	sums := LevelReplayEntries([]rpc.EdgeLevelsDay{d})
	if s := sums[0]; s.Kind != rpc.EdgeLevelsEntryLaterBuyBack || s.Cuts != 1 || s.Bought != 1 || s.Judged != 1 || s.H5Wins != 1 {
		t.Fatalf("entries = %+v", s)
	}
}

func TestLevelAddsBuyOnTwoClosesAboveTheLevelOnlyWhileTheIndexHolds(t *testing.T) {
	kinds := func(d rpc.EdgeLevelsDay) map[string]rpc.EdgeLevelsEntry {
		out := map[string]rpc.EdgeLevelsEntry{}
		for _, e := range d.Adds {
			out[e.Kind] = e
		}
		return out
	}
	// Yesterday's range was 95 to 96.50: the stock holds 97 after the look,
	// two closes above the high, and the add buys at the second.
	above := rpc.MarketReadPrior{Date: "2026-03-09", High: 96.5, Low: 95, Close: 96}
	day := levelTestDay(levelTestBars(nil, flatAfterLook), above, 97)
	day.Close5, day.Close5Day = 100, "2026-03-17"
	got := kinds(levelMoneyOne(t, day, nil, levelFlatIndex()))
	add, ok := got[rpc.EdgeLevelsEntryAddAboveHigh]
	if !ok || add.Price != 97 || !add.At.Equal(levelTestSession.Open.Add(70*time.Minute)) || add.H5Pct == nil || len(got) != 1 {
		t.Fatalf("adds = %+v", got)
	}
	near(t, "add above the prior high", *add.H5Pct, (100.0/97-1)*100)
	// Without the index's bars nothing says it holds, and nothing is added.
	if got = kinds(levelMoneyOne(t, day, nil, nil)); len(got) != 0 {
		t.Fatalf("adds without the index = %+v", got)
	}
	// The index under its own first-hour low at those closes: no add.
	weak := levelFlatIndex()
	for i := 12; i < len(weak); i++ {
		weak[i].Close = 499
	}
	if got = kinds(levelMoneyOne(t, day, nil, weak)); len(got) != 0 {
		t.Fatalf("adds with the index under its low = %+v", got)
	}
	// The reclaim: opened at 98 under yesterday's low of 100, then two
	// closes back above it late in the morning.
	bars := levelTestBars(map[int][4]float64{20: {99.5, 100.6, 99.4, 100.4}, 21: {100.4, 100.7, 100.2, 100.5}}, flatAfterLook)
	got = kinds(levelMoneyOne(t, levelTestDay(bars, levelTestPrior, 97), nil, levelFlatIndex()))
	if e, ok := got[rpc.EdgeLevelsEntryAddReclaim]; !ok || e.Price != 100.5 || e.State != rpc.EdgeLevelsEntryPending || len(got) != 1 {
		t.Fatalf("reclaim = %+v", got)
	}
}

func TestLevelInsuranceSplitsTheCutsIntoFallsThatTurnedAndFallsThatKeptGoing(t *testing.T) {
	turned := levelOne(t, levelTestDay(levelTestBars(cutAndBack, flatAfterLook), levelTestPrior, 98), nil)
	// The same cut on a day that closes at 94, under the cut level of 96.
	kept := levelOne(t, levelTestDay(levelTestBars(map[int][4]float64{12: cutAndBack[12], 13: cutAndBack[13]}, [4]float64{95, 95.2, 93.9, 94}), levelTestPrior, 94), nil)
	kept.Session = "2026-03-11"
	var row rpc.EdgeLevelsInsurance
	for _, r := range LevelReplayInsurance([]rpc.EdgeLevelsDay{turned, kept}) {
		if r.Reading == rpc.EdgeLevelsReadingPriorLow && r.Size == rpc.EdgeLevelsSizeHalves && r.Half == rpc.EdgeLevelsHalfAll && r.Version == rpc.EdgeLevelsVersionRule && r.Basis == rpc.EdgeLevelsBasisStock {
			row = r
		}
	}
	if row.Days != 2 || row.Cuts != 2 || row.Turned != 1 || row.KeptGoing != 1 {
		t.Fatalf("insurance = %+v", row)
	}
	// Sold at 95 from 97: against holding to 98 it cost, against 94 it saved.
	near(t, "cost when the fall turned", row.TurnedCostClosePct, (95.0-98)/97*100)
	near(t, "saving when it kept going", row.KeptSavingClosePct, (95.0-94)/97*100)
	near(t, "worst held", row.WorstHeldClosePct, (94.0/97-1)*100)
	near(t, "worst with the plan", row.WorstPlanClosePct, (95.0/97-1)*100)
}
