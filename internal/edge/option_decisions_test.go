package edge

import (
	"math"
	"testing"

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

// An option that expires worthless was sold at zero: a full loss on its
// entry cost, counted on the sale side like any other sale.
func TestExpiredOptionIsAZeroPriceSale(t *testing.T) {
	result := optionDecisionResult(t, []float64{1, -1}, []string{"O", "C"}, []float64{2, 0}, 0, nil)
	open, exit := result.Changes[0].Position, result.Changes[1].Position
	if !nearPtr(open.ResultPct, -100) || !nearPtr(open.ResultBase, -200) || !nearPtr(exit.ResultPct, -100) || !nearPtr(exit.ResultBase, -200) || exit.LaterExitPct != nil {
		t.Fatalf("open=%+v exit=%+v", open, exit)
	}
}

// An option decision is scored like a stock decision, with the broker's
// statement mark as the close on the Nth market session. A mark on any other
// day is not that horizon, and the verdict follows the figure.
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
	if one.DecisionImpactBase == nil || math.Abs(*one.DecisionImpactBase-(2*100*(3-2)-1)) > 1e-6 {
		t.Fatalf("one session=%+v", one)
	}
	// The fifth session is the 12th. The mark of the 9th is not its close.
	if five.DecisionImpactBase != nil || five.Reason != ReasonMissingHorizon {
		t.Fatalf("five sessions=%+v", five)
	}
	if v := change.Verdict; v.Label != VerdictHelped || v.Basis != VerdictBasisHorizon || v.Sessions != 1 || v.ImpactBase == nil {
		t.Fatalf("verdict=%+v", v)
	}
	if result.Coverage.EligibleChanges != 1 || result.Coverage.ScoredByHorizon[1] != 1 {
		t.Fatalf("coverage=%+v", result.Coverage)
	}
}

// A result no larger than the decision's own costs is no difference, and a
// decision with no figure says why.
func TestVerdictLabels(t *testing.T) {
	impact := func(v float64) []HorizonScore { return []HorizonScore{{Sessions: 5, DecisionImpactBase: &v}} }
	for _, tc := range []struct {
		change Change
		want   string
	}{
		{Change{DirectCostsBase: new(float64(5)), Scores: impact(4)}, VerdictNoDifference},
		{Change{DirectCostsBase: new(float64(5)), Scores: impact(40)}, VerdictHelped},
		{Change{DirectCostsBase: new(float64(5)), Scores: impact(-40)}, VerdictHurt},
		{Change{Scores: []HorizonScore{{Sessions: 5, Reason: ReasonInterveningChange}}, Position: &PositionOutcome{ResultBase: new(float64(-90))}}, VerdictHurt},
		{Change{Scores: []HorizonScore{{Sessions: 5, Reason: ReasonInterveningChange}}}, VerdictNone},
	} {
		if got := changeVerdict(tc.change); got.Label != tc.want || tc.want == VerdictNone && got.Reason != ReasonInterveningChange {
			t.Fatalf("verdict=%+v want %s", got, tc.want)
		}
	}
}

// A decision with no figure names the nearest cause: where an option's own
// position cannot be read either, its reason is the one served, not the
// horizon's. An exercised option used to read "query field missing".
func TestVerdictWithoutAFigureNamesThePositionReason(t *testing.T) {
	change := Change{AssetClass: "OPT", Scores: []HorizonScore{{Sessions: 1, Reason: ReasonQueryFieldMissing}, {Sessions: 5, Reason: ReasonQueryFieldMissing}},
		Position: &PositionOutcome{Reason: "closed_by_delivery"}}
	if got := changeVerdict(change); got.Label != VerdictNone || got.Reason != "closed_by_delivery" {
		t.Fatalf("verdict=%+v, want none with the position's reason", got)
	}
	change.Position = nil
	if got := changeVerdict(change); got.Label != VerdictNone || got.Reason != ReasonQueryFieldMissing {
		t.Fatalf("verdict=%+v, want the horizon's reason without a position", got)
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
