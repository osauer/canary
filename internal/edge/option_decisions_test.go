package edge

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

func optionDecisionResult(t *testing.T, deltas []float64, markers []string, prices []float64, final float64, mark *float64) Result {
	t.Helper()
	st := cycleTestStatement(deltas, markers, final)
	for i, price := range prices {
		st.Trades[i].Price = new(price)
	}
	st.Positions[0].MarkPrice = mark
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func nearPtr(got *float64, want float64) bool {
	return got != nil && math.Abs(*got-want) < 1e-6
}

// Every decision in a closed option position is read against that position:
// entries against what the contracts fetched, sales against entry cost and
// against the sales that came later. Both readings add up to the same money.
func TestOptionDecisionsAreReadAgainstTheirPosition(t *testing.T) {
	result := optionDecisionResult(t, []float64{2, 1, -1, -2}, []string{"O", "O", "C", "C"}, []float64{2, 4, 5, 1}, 0, nil)
	if len(result.Changes) != 4 {
		t.Fatalf("changes=%d", len(result.Changes))
	}
	open, add, trim, exit := result.Changes[0].Position, result.Changes[1].Position, result.Changes[2].Position, result.Changes[3].Position
	for i, outcome := range []*PositionOutcome{open, add, trim, exit} {
		if outcome == nil || outcome.State != OptionPositionClosed || outcome.ResultPct == nil || outcome.ResultBase == nil {
			t.Fatalf("change %d outcome=%+v", i, outcome)
		}
	}
	exitAverage, entryAverage := 7.0/3, 8.0/3
	if !nearPtr(open.ResultPct, (exitAverage/2-1)*100) || !nearPtr(add.ResultPct, (exitAverage/4-1)*100) || open.ReferenceKind != OptionReferenceExitAverage {
		t.Fatalf("entry readings open=%+v add=%+v", open, add)
	}
	if !nearPtr(trim.ResultPct, (5/entryAverage-1)*100) || !nearPtr(exit.ResultPct, (1/entryAverage-1)*100) {
		t.Fatalf("exit readings trim=%+v exit=%+v", trim, exit)
	}
	// The contracts kept after the trim were sold at 1 against 5.
	if !nearPtr(trim.LaterExitPct, -80) || !nearPtr(trim.LaterExitBase, -400) || exit.LaterExitPct != nil {
		t.Fatalf("later exit trim=%+v exit=%+v", trim, exit)
	}
	// Proceeds 700 against cost 800: both readings carry the same -100.
	if entries, exits := *open.ResultBase+*add.ResultBase, *trim.ResultBase+*exit.ResultBase; math.Abs(entries+100) > 1e-6 || math.Abs(exits+100) > 1e-6 {
		t.Fatalf("entries=%v exits=%v", entries, exits)
	}
	decisions := result.Options.Decisions
	if decisions.Changes != 4 || decisions.WithOutcome != 4 || len(decisions.Actions) != 4 {
		t.Fatalf("decisions=%+v", decisions)
	}
	for _, roll := range decisions.Actions {
		if roll.Action == ActionAdd && roll.Losses != 1 || roll.Action == ActionTrim && (roll.WithLaterExit != 1 || roll.LaterExitBetter != 0) || roll.MedianResultPct != nil {
			t.Fatalf("roll=%+v", roll)
		}
	}
}

// Contracts still held are valued at the latest statement mark, and the
// reading says so.
func TestOpenOptionPositionUsesTheStatementMark(t *testing.T) {
	result := optionDecisionResult(t, []float64{2, -1}, []string{"O", "C"}, []float64{2, 3}, 1, new(float64(5)))
	open, trim := result.Changes[0].Position, result.Changes[1].Position
	if open.State != OptionPositionOpen || open.ReferenceKind != OptionReferenceExitAverageAndMark || !nearPtr(open.ResultPct, 100) || open.PositionPNLBase != nil || open.ClosedAt != nil {
		t.Fatalf("open=%+v", open)
	}
	if !nearPtr(trim.LaterExitPct, (5.0/3-1)*100) || result.Options.Decisions.Actions[1].LaterExitBetter != 1 {
		t.Fatalf("trim=%+v", trim)
	}
	without := optionDecisionResult(t, []float64{2, -1}, []string{"O", "C"}, []float64{2, 3}, 1, nil)
	if got := without.Changes[0].Position; got.ResultPct != nil || got.Reason != OptionOutcomeMarkUnavailable {
		t.Fatalf("no mark=%+v", got)
	}
}

// A position without a proven opening gets a reason, never a figure, and the
// stock scoring is untouched by option outcomes.
func TestUnprovedOptionPositionCarriesAReason(t *testing.T) {
	result := optionDecisionResult(t, []float64{-2}, []string{"C"}, []float64{3}, 0, nil)
	got := result.Changes[0].Position
	if got == nil || got.State != OptionPositionUnproved || got.Reason != "opening_unproved" || got.ResultPct != nil {
		t.Fatalf("outcome=%+v", got)
	}
	if result.Options.Decisions.Reasons["opening_unproved"] != 1 || result.Options.Decisions.WithOutcome != 0 {
		t.Fatalf("decisions=%+v", result.Options.Decisions)
	}
	stock, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{edgeStatement()}})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range stock.Changes {
		if change.AssetClass != "OPT" && change.Position != nil {
			t.Fatalf("stock change carries a position outcome: %+v", change)
		}
	}
}

// A close the broker books at zero is a full loss on the entry cost, read on
// the closing side like any other close. (An expiry reported with its
// lifecycle row is no decision at all:
// TestLifecycleClosedOptionCyclesAreCompleted.)
func TestACloseBookedAtZeroIsAFullLossOnEntryCost(t *testing.T) {
	result := optionDecisionResult(t, []float64{1, -1}, []string{"O", "C"}, []float64{2, 0}, 0, nil)
	open, exit := result.Changes[0].Position, result.Changes[1].Position
	if !nearPtr(open.ResultPct, -100) || !nearPtr(open.ResultBase, -200) || !nearPtr(exit.ResultPct, -100) || !nearPtr(exit.ResultBase, -200) || exit.LaterExitPct != nil {
		t.Fatalf("open=%+v exit=%+v", open, exit)
	}
}

// An option decision is scored like a stock decision, with the broker's
// statement mark as the close on the Nth market session, and the score says
// it rests on a mark. A mark on any other day is not that horizon. The
// figure is no verdict: an option's position is graded, not its fills.
func TestOptionDecisionIsScoredFromStatementMarks(t *testing.T) {
	st := cycleTestStatement([]float64{2}, []string{"O"}, 2)
	st.Positions[0].MarkPrice = new(float64(9))
	// Sessions after the buy on 2026-01-05: the 6th, 7th, 8th, 9th, 12th.
	st.Positions = append(st.Positions,
		flexstmt.OpenPosition{RecordID: "m1", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-01-06"), Quantity: new(float64(2)), MarkPrice: new(float64(3))},
		flexstmt.OpenPosition{RecordID: "m4", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-01-09"), Quantity: new(float64(2)), MarkPrice: new(float64(7))},
	)
	var calendar []DailyBar
	for _, d := range []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09", "2026-01-12"} {
		calendar = append(calendar, DailyBar{Day: day(d), Close: 100})
	}
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, ContextBars: map[string][]DailyBar{"SPY": calendar}})
	if err != nil {
		t.Fatal(err)
	}
	change := result.Changes[0]
	one, five := change.Scores[0], change.Scores[1]
	// Two contracts bought at 2, marked 3 one session later, one of costs.
	if one.DecisionImpactBase == nil || math.Abs(*one.DecisionImpactBase-(2*100*(3-2)-1)) > 1e-6 || one.CloseSource != CloseSourceStatementMark {
		t.Fatalf("one session=%+v", one)
	}
	// The fifth session is the 12th. The mark of the 9th is not its close.
	if five.DecisionImpactBase != nil || five.Reason != ReasonMissingHorizon {
		t.Fatalf("five sessions=%+v", five)
	}
	if v := change.Verdict; v.Label != VerdictNone || v.Reason != VerdictGradedByPosition || v.ImpactBase != nil {
		t.Fatalf("verdict=%+v, want none: the position is graded", v)
	}
	if result.Coverage.EligibleChanges != 1 || result.Coverage.ScoredByHorizon[1] != 1 {
		t.Fatalf("coverage=%+v", result.Coverage)
	}
}

// A stock or ETF verdict rests on the five-session figure alone. A price move
// within the decision's own costs is no difference in either direction; the
// figures below are after costs of 5, so the move is the figure plus 5.
func TestVerdictLabels(t *testing.T) {
	five := func(v float64) []HorizonScore { return []HorizonScore{{Sessions: 5, DecisionImpactBase: &v}} }
	for _, tc := range []struct {
		name   string
		change Change
		want   string
	}{
		{"a gain beyond costs", Change{DirectCostsBase: new(float64(5)), Scores: five(40)}, VerdictHelped},
		{"a loss beyond costs", Change{DirectCostsBase: new(float64(5)), Scores: five(-40)}, VerdictHurt},
		// A move of +6 and of -6: the old band read the first as no
		// difference and the second as hurt.
		{"a gain just beyond costs", Change{DirectCostsBase: new(float64(5)), Scores: five(1)}, VerdictHelped},
		{"a loss just beyond costs", Change{DirectCostsBase: new(float64(5)), Scores: five(-11)}, VerdictHurt},
		// A move of +2 and of -3.
		{"a gain within costs", Change{DirectCostsBase: new(float64(5)), Scores: five(-3)}, VerdictNoDifference},
		{"a loss within costs", Change{DirectCostsBase: new(float64(5)), Scores: five(-8)}, VerdictNoDifference},
	} {
		if got := changeVerdict(tc.change); got.Label != tc.want || got.Sessions != VerdictSessions || got.Basis != VerdictBasisHorizon {
			t.Fatalf("%s: verdict=%+v want %s at five sessions", tc.name, got, tc.want)
		}
	}
}

// A verdict never changes its measure. With no five-session figure a stock
// decision has no verdict and gives that horizon's reason, whatever the one
// session figure says: the fallback to it called a decision helped one day
// and hurt four days later.
func TestStockVerdictDoesNotFallBackToOneSession(t *testing.T) {
	change := Change{AssetClass: "STK", DirectCostsBase: new(float64(1)), Scores: []HorizonScore{
		{Sessions: 1, DecisionImpactBase: new(float64(300))},
		{Sessions: 5, Reason: ReasonMissingHorizon},
	}}
	if got := changeVerdict(change); got.Label != VerdictNone || got.Reason != ReasonMissingHorizon || got.ImpactBase != nil {
		t.Fatalf("verdict=%+v, want none with the five-session reason", got)
	}
}

// Profit grades the position, and a price read after a trade grades
// nothing. The position below was bought at 2 and sold out at 3; the broker
// booked +20 for it. Closing quotes read after the sale put the contract at
// 9, which until 2026-11-19 turned the final sale from helped to hurt. The
// quotes change no grade and no verdict, and the scores they fill say they
// rest on a quote.
func TestFetchedClosesChangeNoOptionGrade(t *testing.T) {
	var calendar []DailyBar
	for i := range 12 {
		calendar = append(calendar, DailyBar{Day: day("2026-01-05").AddDate(0, 0, i), Close: 100})
	}
	run := func(bars map[int64][]DailyBar) Result {
		st := cycleTestStatement([]float64{2, -2}, []string{"O", "C"}, 0)
		st.Trades[0].Price, st.Trades[1].Price = new(float64(2)), new(float64(3))
		result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: bars, ContextBars: map[string][]DailyBar{"SPY": calendar}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var quotes []DailyBar
	for i := 1; i < 12; i++ {
		quotes = append(quotes, DailyBar{ConID: 456, Day: day("2026-01-05").AddDate(0, 0, i), Close: 9})
	}
	before, after := run(nil), run(map[int64][]DailyBar{456: quotes})
	for _, result := range []Result{before, after} {
		cycles := result.Options.Cycles
		if len(cycles.Cycles) != 1 || cycles.MadeMoney != 1 || cycles.LostMoney+cycles.Flat+cycles.Ungraded != 0 {
			t.Fatalf("cycles=%+v, want one position that made money", cycles)
		}
		// The grade is the sign of the broker's realized result, 10 on the
		// one closing execution, and of nothing else.
		if row := cycles.Cycles[0]; row.Grade != OptionGradeMadeMoney || row.RealizedPNLBase == nil || *row.RealizedPNLBase != 10 {
			t.Fatalf("cycle=%+v, want made_money on the broker's 10", row)
		}
		if len(result.Changes) != 2 || len(result.Verdicts) != 0 {
			t.Fatalf("changes=%d verdict counts=%+v, want two option fills and no fill counted", len(result.Changes), result.Verdicts)
		}
		for _, change := range result.Changes {
			if change.Verdict.Label != VerdictNone || change.Verdict.Reason != VerdictGradedByPosition || change.Position.PositionGrade != OptionGradeMadeMoney {
				t.Fatalf("change=%+v, want no verdict and the position's grade", change)
			}
		}
	}
	sale := after.Changes[1].Scores[1]
	if sale.Sessions != 5 || sale.DecisionImpactBase == nil || *sale.DecisionImpactBase >= 0 || sale.CloseSource != CloseSourceClosingQuote {
		t.Fatalf("five sessions after the sale=%+v, want a negative figure resting on a closing quote", sale)
	}
	if got := before.Changes[1].Scores[1]; got.DecisionImpactBase != nil {
		t.Fatalf("without quotes the sale has a five-session figure: %+v", got)
	}
}

// A change whose statement row carried no asset class is counted under a
// name: a blank class fails the served result's validation and took the
// whole Edge read down with it.
func TestVerdictCountsNameABlankAssetClass(t *testing.T) {
	counts := buildVerdictCounts([]Change{{AssetClass: " ", Verdict: Verdict{Label: VerdictNone}}, {AssetClass: "STK", Verdict: Verdict{Label: VerdictHelped}}})
	if len(counts) != 2 || counts[0].AssetClass != "STK" || counts[1].AssetClass != "unknown" || counts[1].None != 1 {
		t.Fatalf("counts=%+v", counts)
	}
}

// On a day that has both, the statement mark is the option's close and the
// fetched last trade is not: the mark is the broker's own valuation and what
// every option score so far rests on, and a last trade can be hours older.
// A fetched close counts on the days the statement has no mark, which is
// every day after the contract left the account.
func TestStatementMarkOutranksAFetchedCloseOnTheSameDay(t *testing.T) {
	held, sold := day("2026-01-06"), day("2026-01-07")
	input := Input{AsOf: day("2026-01-20"), Bars: map[int64][]DailyBar{456: {{ConID: 456, Day: held, Close: 5}, {ConID: 456, Day: sold, Close: 6}}}}
	ev := evidence{positions: []flexstmt.OpenPosition{{ConID: 456, AssetClass: "OPT", ReportDate: held, MarkPrice: new(float64(4))}}}
	bars := buildScoringIndex(input, nil, ev).barsByConID[456]
	if len(bars) != 2 || bars[0].Close != 4 || bars[1].Close != 6 {
		t.Fatalf("bars=%+v, want the mark 4 on the held day and the fetched 6 after it", bars)
	}
}

// holidayWeekStatement is one synthetic option position, bought on Monday
// 2026-11-16 and sold on Friday 2026-11-20, the week before US Thanksgiving
// (Thursday 2026-11-26, no US session). venue is the option's listing
// exchange; an empty venue leaves the contract without an instrument row.
func holidayWeekStatement(venue string) flexstmt.Statement {
	st := edgeStatement()
	st.FromDate, st.ToDate, st.WhenGenerated = day("2026-09-01"), day("2026-12-10"), dayTime("2026-12-11", 1)
	st.Equity = []flexstmt.EquityRow{{ReportDate: day("2026-09-01"), TotalBase: 10000}, {ReportDate: day("2026-12-10"), TotalBase: 10000}}
	st.Cash, st.FXRates = nil, nil
	for i := range 45 {
		st.FXRates = append(st.FXRates, flexstmt.FXRate{RecordID: fmt.Sprint("fx", i), Date: day("2026-11-01").AddDate(0, 0, i), FromCurrency: "USD", ToCurrency: "EUR", Rate: new(.9)})
	}
	opt := func(id, executed, side, marker string, pnl float64) flexstmt.Trade {
		return flexstmt.Trade{RecordID: id, AccountID: "U", ConID: 456, Symbol: "SYN CALL", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: id, ExecutionID: id, ExecutedAt: dayTime(executed, 16), Side: side, OpenClose: marker, Quantity: new(float64(2)), Price: new(float64(5)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(pnl), LevelOfDetail: "EXECUTION"}
	}
	st.Trades = []flexstmt.Trade{opt("buy", "2026-11-16", "BUY", "O", 0), opt("sale", "2026-11-20", "SELL", "C", 10)}
	st.Positions = []flexstmt.OpenPosition{{RecordID: "end", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-12-10"), Quantity: new(float64(0))}}
	if venue != "" {
		st.Instruments = []flexstmt.Instrument{{RecordID: "i456", ConID: 456, Symbol: "SYN CALL", UnderlyingSymbol: "SYN", AssetClass: "OPT", Currency: "USD", ListingExchange: venue, ReportDate: day("2026-12-10")}}
	}
	return st
}

// holidayWeekBars is what the daemon would hold for that week: SPY with a
// close on every US session and none on Thanksgiving, a stock listed in
// Europe (contract 789) with a close on every weekday including
// Thanksgiving, and the option's closing quotes on Friday the 27th (9) and
// Monday the 30th (4).
func holidayWeekBars() (bars map[int64][]DailyBar, context map[string][]DailyBar) {
	var us, europe []DailyBar
	for d := day("2026-11-09"); !d.After(day("2026-12-04")); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		europe = append(europe, DailyBar{ConID: 789, Day: d, Close: 50})
		if !d.Equal(day("2026-11-26")) {
			us = append(us, DailyBar{Day: d, Close: 100})
		}
	}
	quotes := []DailyBar{{ConID: 456, Day: day("2026-11-27"), Close: 9}, {ConID: 456, Day: day("2026-11-30"), Close: 4}}
	return map[int64][]DailyBar{456: quotes, 789: europe}, map[string][]DailyBar{"SPY": us}
}

// Five sessions after a US option's sale are five US trading days. The sale
// is on Friday 2026-11-20; the US sessions after it are the 23rd, 24th, 25th,
// 27th and 30th, because Thanksgiving (the 26th) has none. A stock listed in
// Europe traded on the 26th, and until 2026-11-20 its close made that day a
// session for every option of the account: the fifth session landed on the
// 27th and the score read that day's quote.
func TestOptionHorizonCountsSessionsOfTheOptionsOwnMarket(t *testing.T) {
	bars, context := holidayWeekBars()
	result, err := Analyze(Input{AsOf: day("2026-12-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{holidayWeekStatement("CBOE")}, Bars: bars, ContextBars: context})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 2 {
		t.Fatalf("changes=%d, want the buy and the sale", len(result.Changes))
	}
	five := result.Changes[1].Scores[1]
	if five.Sessions != 5 || five.HorizonDay == nil || five.HorizonClose == nil {
		t.Fatalf("five sessions after the sale=%+v, want a figure", five)
	}
	if !five.HorizonDay.Equal(day("2026-11-30")) || *five.HorizonClose != 4 || five.CloseSource != CloseSourceClosingQuote {
		t.Fatalf("fifth session=%s close=%v source=%q, want 2026-11-30 (the fifth US session; the 26th is a US holiday), close 4, a closing quote", five.HorizonDay.Format("2006-01-02"), *five.HorizonClose, five.CloseSource)
	}
}

// Only the US calendar can be derived from what Edge holds. An option listed
// on another exchange, one whose statement names no listing, and any option
// while no US benchmark close is on record get no horizon figure and say why:
// a figure counted on another market's days would look like one and be wrong.
func TestOptionWithoutAKnownSessionCalendarHasNoHorizonFigure(t *testing.T) {
	bars, context := holidayWeekBars()
	for _, tc := range []struct {
		name    string
		venue   string
		context map[string][]DailyBar
	}{
		{"listed outside the US", "EUREX", context},
		{"no instrument row", "", context},
		{"no US benchmark close on record", "CBOE", map[string][]DailyBar{"VIX": context["SPY"]}},
	} {
		result, err := Analyze(Input{AsOf: day("2026-12-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{holidayWeekStatement(tc.venue)}, Bars: bars, ContextBars: tc.context})
		if err != nil {
			t.Fatal(err)
		}
		for _, score := range result.Changes[1].Scores {
			if score.DecisionImpactBase != nil || score.HorizonDay != nil || score.Reason != ReasonSessionCalendarUnavailable {
				t.Fatalf("%s: %d sessions=%+v, want no figure and reason %s", tc.name, score.Sessions, score, ReasonSessionCalendarUnavailable)
			}
		}
		// The position's grade rests on the broker's realized result and
		// needs no calendar.
		if cycles := result.Options.Cycles; len(cycles.Cycles) != 1 || cycles.Cycles[0].Grade != OptionGradeMadeMoney {
			t.Fatalf("%s: cycles=%+v, want the position still graded", tc.name, cycles)
		}
	}
}

// An option change names its underlying: the trade row's, else the
// instrument record's. The forward tests key on it and keep single names.
func TestOptionChangeNamesItsUnderlying(t *testing.T) {
	st := cycleTestStatement([]float64{1, -1}, []string{"O", "C"}, 0)
	st.Instruments[0].UnderlyingSymbol = "syn"
	result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
	if err != nil || len(result.Changes) != 2 {
		t.Fatalf("changes=%d err=%v", len(result.Changes), err)
	}
	for _, change := range result.Changes {
		if change.Underlying != "SYN" {
			t.Fatalf("underlying=%q, want the instrument's", change.Underlying)
		}
	}
	st.Trades[0].UnderlyingSymbol = "SYNT"
	result, err = Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
	if err != nil || result.Changes[0].Underlying != "SYNT" {
		t.Fatalf("underlying=%q err=%v, want the trade row's", result.Changes[0].Underlying, err)
	}
}
