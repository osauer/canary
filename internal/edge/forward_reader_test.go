package edge

import (
	"testing"
	"time"
)

// Synthetic reader fixtures: a rule registered on a Monday morning, and
// decisions in the weeks after it. Five sessions are a calendar week here.
var (
	fwdZone       = time.UTC
	fwdRegistered = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC) // a Monday
)

func fwdAt(days int) time.Time {
	return fwdRegistered.Add(time.Duration(days)*24*time.Hour + 6*time.Hour)
}

func fwdInput(asOf time.Time, changes ...Change) ForwardCountInput {
	return ForwardCountInput{
		Changes: changes, After: fwdRegistered, AsOf: asOf, ProtectionKnown: true, Zone: fwdZone,
		FifthSessionClose: func(t time.Time) (time.Time, bool) { return t.AddDate(0, 0, 7), true },
	}
}

func fwdClose(source string) []HorizonScore {
	v := 10.0
	return []HorizonScore{{Sessions: 1, HorizonClose: &v, CloseSource: source}, {Sessions: 5, HorizonClose: &v, CloseSource: source}}
}

func fwdStock(id, symbol, action string, days int) Change {
	return Change{ID: id, Symbol: symbol, AssetClass: "STK", Action: action, Direction: DirectionLong, ExecutedAt: fwdAt(days), Scores: fwdClose(CloseSourceDailyBar)}
}

func fwdOption(id, underlying, action string, days int, position *PositionOutcome) Change {
	return Change{ID: id, Symbol: underlying + " 270115C00100000", Underlying: underlying, AssetClass: "OPT", Action: action, Direction: DirectionLong, ExecutedAt: fwdAt(days), Position: position, Scores: fwdClose(CloseSourceClosingQuote)}
}

func fwdPosition(id, state string) *PositionOutcome {
	return &PositionOutcome{State: state, PositionID: id, ClosedBy: map[string]string{OptionPositionClosed: "trade"}[state]}
}

func protected(c Change) Change {
	c.ProtectionContext = ProtectionContext{Status: "partial", ExecutionCount: 2, MatchedExecutions: 1}
	return c
}

type fwdWant struct {
	key, group, state, reason string
	protection                bool
}

func checkUnits(t *testing.T, got []ForwardUnit, want []fwdWant) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("units=%d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		u := got[i]
		if w.key != "" && u.Key != w.key || u.Group != w.group || u.Protection != w.protection || u.State != w.state || u.Reason != w.reason {
			t.Errorf("unit %d = %+v, want %+v", i, u, w)
		}
	}
}

// Every registered rule has its facts, and a rule without a checkable fact
// says so with an empty condition instead of an invented one.
func TestForwardFactsCoverEveryRule(t *testing.T) {
	for _, rule := range ForwardTests() {
		facts, ok := ForwardTestFacts(rule.ID)
		if !ok || facts.Question == "" || len(facts.Informs) == 0 || facts.Needs <= 0 || facts.Unit == "" {
			t.Fatalf("rule %q has incomplete facts: %+v", rule.ID, facts)
		}
		empty := rule.ID == "stock_sales_vs_nearby_day" || rule.ID == "entries_vs_nearby_day"
		if empty != (facts.Condition == "") {
			t.Errorf("rule %q condition=%q", rule.ID, facts.Condition)
		}
	}
}

// Rule 1: one reading per name and week in which a stock or ETF was sold out
// of a long position; final once the week is over and each sale's
// five-session close exists. A sale Canary's protection sent counts like
// any other and only marks its week.
func TestForwardStockSalesWalkTheRule(t *testing.T) {
	short := fwdStock("s", "AAA", ActionExit, 1)
	short.Direction = DirectionShort
	option := fwdOption("o", "AAA", ActionTrim, 1, fwdPosition("p", OptionPositionOpen))
	noClose := fwdStock("n", "CCC", ActionExit, 8)
	noClose.Scores = []HorizonScore{{Sessions: 5, Reason: ReasonCorporateAction}}
	changes := []Change{
		fwdStock("before", "AAA", ActionTrim, -1), // before registration: hindsight
		fwdStock("a1", "AAA", ActionTrim, 1),
		fwdStock("a2", "AAA", ActionExit, 2), // same name, same week: one reading
		{ID: "etf", Symbol: "EEE", AssetClass: "ETF", Action: ActionTrim, Direction: DirectionLong, ExecutedAt: fwdAt(3), Scores: fwdClose(CloseSourceDailyBar)},
		protected(fwdStock("b", "BBB", ActionExit, 1)),
		fwdStock("buy", "AAA", ActionAdd, 1), // not a sale
		short, option,                        // a short cover and an option are not stock sales
		noClose,
		fwdStock("late", "DDD", ActionTrim, 15), // its five sessions are not over
	}
	units := CountForwardUnits("stock_sales_vs_nearby_day", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "AAA:2026-W10", group: ForwardGroupAll, state: "final"},
		{key: "BBB:2026-W10", group: ForwardGroupAll, protection: true, state: "final"},
		{key: "EEE:2026-W10", group: ForwardGroupAll, state: "final"},
		{group: ForwardGroupAll, state: "excluded", reason: ForwardExcludedNoClose},
		{group: ForwardGroupAll, state: "waiting"},
	})
	// A week still running is not final, even when its sales are scored.
	inWeek := CountForwardUnits("stock_sales_vs_nearby_day", fwdInput(fwdAt(4), fwdStock("a1", "AAA", ActionTrim, 1)))
	checkUnits(t, inWeek, []fwdWant{{group: ForwardGroupAll, state: "waiting"}})
	// Protection evidence withheld: the sales still count, nothing is marked
	// as protection's, and the sales of the year before still set a pace.
	in := fwdInput(fwdAt(20), protected(fwdStock("b", "BBB", ActionExit, 1)), fwdStock("a1", "AAA", ActionTrim, 1))
	in.ProtectionKnown = false
	facts, _ := ForwardTestFacts("stock_sales_vs_nearby_day")
	withheld := CountForwardUnits("stock_sales_vs_nearby_day", in)
	sample := SummarizeForward(facts, withheld, nil, fwdRegistered, fwdAt(20), fwdZone)
	if sample.Test.Final != 2 || sample.Protection.Final != 0 || sample.ToFill != 78 || !sample.CanFill {
		t.Fatalf("withheld protection: %+v", sample)
	}
	paced := SummarizeForward(facts, nil, withheld, fwdAt(20), fwdAt(20), fwdZone)
	if paced.Expected == nil || paced.NoDate != "" {
		t.Fatalf("withheld protection, pace only: %+v", paced)
	}
	// Every sale counts toward the 80; the protection's is also told apart.
	sample = SummarizeForward(facts, units, nil, fwdRegistered, fwdAt(20), fwdZone)
	if sample.Test.Final != 3 || sample.Protection.Final != 1 || sample.ToFill != 77 || sample.Excluded[ForwardExcludedNoClose] != 1 || sample.State != ForwardStateCollecting {
		t.Fatalf("sample = %+v", sample)
	}
}

// Rule 2: one reading per name and week with an entry into a long stock, ETF
// or single-name option position. A single name is any underlying but SPY
// and VIX, so an entry in the SPY fund counts and one in a SPY option does
// not. No protection split.
func TestForwardEntriesWalkTheRule(t *testing.T) {
	spyFund := fwdStock("spy", "SPY", ActionOpen, 1)
	changes := []Change{
		fwdStock("a", "AAA", ActionOpen, 1),
		fwdOption("ao", "AAA", ActionOpen, 2, fwdPosition("p1", OptionPositionOpen)), // same name, same week
		spyFund,
		fwdOption("spyo", "SPY", ActionOpen, 1, fwdPosition("p2", OptionPositionOpen)),
		fwdOption("vixo", "VIX", ActionAdd, 1, fwdPosition("p3", OptionPositionOpen)),
		fwdOption("unknown", "", ActionOpen, 1, fwdPosition("p4", OptionPositionOpen)),
		protected(fwdStock("b", "BBB", ActionAdd, 8)), // entries have no protection split
		fwdStock("sale", "CCC", ActionTrim, 1),
	}
	units := CountForwardUnits("entries_vs_nearby_day", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "AAA:2026-W10", group: ForwardGroupAll, state: "final"},
		{key: "SPY:2026-W10", group: ForwardGroupAll, state: "final"},
		{key: "BBB:2026-W11", group: ForwardGroupAll, state: "final"},
		// An option whose statement named no underlying cannot be told a
		// single name: it is said, not dropped.
		{key: "unknown", group: ForwardGroupCannotSort, state: "excluded", reason: ForwardExcludedUnderlyingUnset},
	})
	facts, _ := ForwardTestFacts("entries_vs_nearby_day")
	if s := SummarizeForward(facts, units, nil, fwdRegistered, fwdAt(20), fwdZone); s.Test.Final != 3 || s.ToFill != 164 || s.Protection != nil || s.Excluded[ForwardExcludedUnderlyingUnset] != 1 || len(s.Groups) != 1 {
		t.Fatalf("sample = %+v", s)
	}
}

// Rule 3: option buys that open or add to a long position, one per contract
// and week, final when the position has closed; delivery gives no reading.
// Premium at risk at the prior statement is not computed, so every unit's
// group is not read and the read point cannot fill: no date is promised.
func TestForwardPremiumLineWalksTheRule(t *testing.T) {
	delivered := fwdPosition("p3", OptionPositionClosed)
	delivered.ClosedBy = "exercise"
	changes := []Change{
		fwdOption("b1", "SPY", ActionOpen, 1, fwdPosition("p1", OptionPositionClosed)),
		fwdOption("b2", "SPY", ActionAdd, 2, fwdPosition("p1", OptionPositionClosed)), // same contract, same week
		fwdOption("b3", "SPY", ActionAdd, 8, fwdPosition("p1", OptionPositionClosed)), // a new week
		fwdOption("b4", "AAA", ActionOpen, 1, fwdPosition("p2", OptionPositionOpen)),
		fwdOption("b5", "BBB", ActionOpen, 1, delivered),
		fwdOption("b6", "CCC", ActionOpen, 1, &PositionOutcome{State: OptionPositionUnproved}),
		fwdOption("sale", "SPY", ActionExit, 9, fwdPosition("p1", OptionPositionClosed)),
	}
	units := CountForwardUnits("option_buys_below_premium_line", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "p1:2026-W10", group: ForwardGroupNotSorted, state: "final"},
		{key: "p2:2026-W10", group: ForwardGroupNotSorted, state: "waiting"},
		{key: "p3:2026-W10", group: ForwardGroupNotSorted, state: "excluded", reason: ForwardExcludedDelivered},
		{group: ForwardGroupNotSorted, state: "excluded", reason: ForwardExcludedNotProved},
		{key: "p1:2026-W11", group: ForwardGroupNotSorted, state: "final"},
	})
	facts, _ := ForwardTestFacts("option_buys_below_premium_line")
	s := SummarizeForward(facts, units, units, fwdRegistered, fwdAt(20), fwdZone)
	if s.Test.Final != 2 || s.ToFill != 30 || s.Expected != nil || s.NoDate != ForwardNoDateNotWorkedOut || s.Groups[2].ID != ForwardGroupNotSorted || s.Groups[2].Final != 2 {
		t.Fatalf("sample = %+v", s)
	}
}

// Rule 4: single-name option buys grouped by Regime's stage recorded at the
// close of the session before the buy; a session without a record is not
// read. Read at 40 final readings after a warning on at least 20 days.
func TestForwardRegimeWarningWalksTheRule(t *testing.T) {
	stages := map[int]string{0: "early_warning", 1: "quiet", 2: "panic"}
	in := fwdInput(fwdAt(30),
		fwdOption("w1", "AAA", ActionOpen, 1, fwdPosition("p1", OptionPositionClosed)),
		fwdOption("q1", "BBB", ActionOpen, 2, fwdPosition("p2", OptionPositionClosed)),
		fwdOption("w2", "CCC", ActionAdd, 3, fwdPosition("p3", OptionPositionClosed)),
		fwdOption("none", "DDD", ActionOpen, 4, fwdPosition("p4", OptionPositionClosed)),
		fwdOption("index", "SPY", ActionOpen, 1, fwdPosition("p5", OptionPositionClosed)), // not a single name
	)
	in.PriorSessionStage = func(t time.Time) (string, bool) {
		stage, ok := stages[int(t.Sub(fwdRegistered).Hours()/24)-1]
		return stage, ok
	}
	units := CountForwardUnits("option_buys_after_regime_warning", in)
	checkUnits(t, units, []fwdWant{
		{key: "p1:2026-W10", group: "after_warning", state: "final"},
		{key: "p2:2026-W10", group: "other", state: "final"},
		{key: "p3:2026-W10", group: "after_warning", state: "final"},
		{key: "p4:2026-W10", group: ForwardGroupCannotSort, state: "final"},
	})
	facts, _ := ForwardTestFacts("option_buys_after_regime_warning")
	// Before registration no close was recorded: the pace of the group read
	// cannot be measured, and that is the reason given.
	unrecorded := []ForwardUnit{{Group: ForwardGroupCannotSort, State: "final", Date: fwdAt(-30)}}
	if s := SummarizeForward(facts, nil, unrecorded, fwdRegistered, fwdAt(1), fwdZone); s.NoDate != ForwardNoDateNotRecorded {
		t.Fatalf("no date reason %q", s.NoDate)
	}
	// Three final since registration, none after a warning: no pace in the
	// group read, said as such.
	others := []ForwardUnit{{Group: "other", State: "final", Date: fwdAt(1)}, {Group: "other", State: "final", Date: fwdAt(2)}, {Group: "other", State: "final", Date: fwdAt(3)}}
	if s := SummarizeForward(facts, others, nil, fwdRegistered, fwdAt(10), fwdZone); s.NoDate != ForwardNoDateNoneInGroup {
		t.Fatalf("no date reason %q", s.NoDate)
	}
	s := SummarizeForward(facts, units, nil, fwdRegistered, fwdAt(30), fwdZone)
	if s.Groups[0].Final != 2 || s.Groups[0].Days != 2 || s.Groups[1].Final != 1 || s.Groups[2].ID != ForwardGroupCannotSort || s.ToFill != 38 {
		t.Fatalf("sample = %+v", s)
	}
	// 40 readings on fewer than 20 days are not due.
	var many []ForwardUnit
	for i := range 40 {
		many = append(many, ForwardUnit{Group: "after_warning", State: "final", Date: fwdAt(i % 10)})
	}
	if s := SummarizeForward(facts, many, nil, fwdRegistered, fwdAt(60), fwdZone); s.State != ForwardStateCollecting {
		t.Fatalf("40 readings on 10 days: state %s", s.State)
	}
	for i := range many {
		many[i].Date = fwdAt(i)
	}
	if s := SummarizeForward(facts, many, nil, fwdRegistered, fwdAt(60), fwdZone); s.State != ForwardStateFull {
		t.Fatalf("40 readings on 40 days: state %s", s.State)
	}
}

// Rule 5: one reading per closed single-name position, by the sale that
// closed it, grouped by the sale price against the position's average cost;
// counted only where a closing quote is stored five sessions later; a
// a position whose closing sale Canary's protection sent counts like any
// other and is marked.
func TestForwardFinalSalesWalkTheRule(t *testing.T) {
	result := func(pct float64) *PositionOutcome {
		p := fwdPosition("p", OptionPositionClosed)
		p.PositionID, p.ReferenceKind, p.ResultPct = "", OptionReferenceEntryAverage, &pct
		return p
	}
	withID := func(c Change, id string) Change { c.Position.PositionID = id; return c }
	noQuote := withID(fwdOption("nq", "EEE", ActionExit, 1, result(5)), "p5")
	noQuote.Scores = []HorizonScore{{Sessions: 5, Reason: ReasonMissingHorizon}}
	changes := []Change{
		withID(fwdOption("up", "AAA", ActionExit, 1, result(12)), "p1"),
		withID(fwdOption("down", "BBB", ActionExit, 2, result(-30)), "p2"),
		protected(withID(fwdOption("stop", "CCC", ActionExit, 3, result(-10)), "p3")),
		withID(fwdOption("even", "DDD", ActionExit, 1, result(0)), "p4"),
		noQuote,
		withID(fwdOption("trim", "FFF", ActionTrim, 1, result(4)), "p6"),  // not the closing sale
		withID(fwdOption("index", "SPY", ActionExit, 1, result(4)), "p7"), // not a single name
		withID(fwdOption("late", "GGG", ActionExit, 19, result(4)), "p8"),
	}
	units := CountForwardUnits("final_option_sales_vs_close_5_sessions", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "p1", group: "above_cost", state: "final"},
		{key: "p4", group: ForwardGroupCannotSort, state: "excluded", reason: ForwardExcludedSoldAtCost},
		{key: "p5", group: "above_cost", state: "excluded", reason: ForwardExcludedNoClosingQuote},
		{key: "p2", group: "below_cost", state: "final"},
		{key: "p3", group: "below_cost", protection: true, state: "final"},
		{key: "p8", group: "above_cost", state: "waiting"},
	})
	facts, _ := ForwardTestFacts("final_option_sales_vs_close_5_sessions")
	s := SummarizeForward(facts, units, nil, fwdRegistered, fwdAt(20), fwdZone)
	if s.Groups[0].Final != 1 || s.Groups[1].Final != 2 || s.Protection.Final != 1 || s.ToFill != 28 {
		t.Fatalf("sample = %+v", s)
	}
}

// Rule 6: long single-name option positions with a partial sale after
// registration, final when the position has closed; a position on which
// Canary's protection sent any sale counts like any other and is marked.
func TestForwardPartialSalesWalkTheRule(t *testing.T) {
	changes := []Change{
		fwdOption("before", "AAA", ActionTrim, -2, fwdPosition("p1", OptionPositionClosed)), // hindsight alone
		fwdOption("t1", "BBB", ActionTrim, 1, fwdPosition("p2", OptionPositionClosed)),
		fwdOption("t2", "BBB", ActionTrim, 3, fwdPosition("p2", OptionPositionClosed)), // same position
		fwdOption("t3", "CCC", ActionTrim, 1, fwdPosition("p3", OptionPositionOpen)),
		fwdOption("t4", "DDD", ActionTrim, 1, fwdPosition("p4", OptionPositionClosed)),
		protected(fwdOption("x4", "DDD", ActionExit, 4, fwdPosition("p4", OptionPositionClosed))), // a protection sale on p4
		fwdOption("x5", "EEE", ActionExit, 1, fwdPosition("p5", OptionPositionClosed)),            // no partial sale
		fwdOption("t6", "SPY", ActionTrim, 1, fwdPosition("p6", OptionPositionClosed)),            // not a single name
	}
	units := CountForwardUnits("floor_after_partial_sale", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "p2", group: ForwardGroupAll, state: "final"},
		{key: "p3", group: ForwardGroupAll, state: "waiting"},
		{key: "p4", group: ForwardGroupAll, protection: true, state: "final"},
	})
}

// Rule 7: one reading per name and week with a long option sale, VIX left
// out and SPY kept; counted only where a closing quote is stored for the
// fifth session, so a trim whose contract is still held (a statement value)
// gives none. The adverse-move group is not computed: not read.
func TestForwardAdverseMoveWalksTheRule(t *testing.T) {
	held := fwdOption("held", "BBB", ActionTrim, 1, fwdPosition("p2", OptionPositionOpen))
	held.Scores = fwdClose(CloseSourceStatementMark)
	changes := []Change{
		fwdOption("a", "AAA", ActionTrim, 1, fwdPosition("p1", OptionPositionClosed)),
		fwdOption("a2", "AAA", ActionExit, 2, fwdPosition("p1", OptionPositionClosed)),
		held,
		fwdOption("spy", "SPY", ActionExit, 1, fwdPosition("p3", OptionPositionClosed)),
		fwdOption("vix", "VIX", ActionExit, 1, fwdPosition("p4", OptionPositionClosed)),
		protected(fwdOption("stop", "CCC", ActionExit, 1, fwdPosition("p5", OptionPositionClosed))),
	}
	units := CountForwardUnits("option_sales_after_adverse_move", fwdInput(fwdAt(20), changes...))
	checkUnits(t, units, []fwdWant{
		{key: "AAA:2026-W10", group: ForwardGroupNotSorted, state: "final"},
		{key: "BBB:2026-W10", group: ForwardGroupNotSorted, state: "excluded", reason: ForwardExcludedNoClosingQuote},
		{key: "SPY:2026-W10", group: ForwardGroupNotSorted, state: "final"},
		{key: "CCC:2026-W10", group: ForwardGroupNotSorted, protection: true, state: "final"},
	})
	facts, _ := ForwardTestFacts("option_sales_after_adverse_move")
	s := SummarizeForward(facts, units, units, fwdRegistered, fwdAt(20), fwdZone)
	if s.Test.Final != 3 || s.ToFill != 50 || s.Expected != nil || s.NoDate != ForwardNoDateNotWorkedOut {
		t.Fatalf("sample = %+v", s)
	}
}

// The expected date comes from the past year's pace while fewer than three
// units since registration are final, and from the pace since then after;
// a date further out than the review window after registration says the
// rule cannot fill without stored units.
func TestForwardExpectedDate(t *testing.T) {
	facts, _ := ForwardTestFacts("entries_vs_nearby_day")
	asOf := fwdRegistered.AddDate(0, 0, 10)
	var past []ForwardUnit
	for i := range 73 { // 73 final units over the year before: one in five days
		past = append(past, ForwardUnit{Group: ForwardGroupAll, State: "final", Date: fwdRegistered.AddDate(0, 0, -5*i-1)})
	}
	s := SummarizeForward(facts, nil, past, fwdRegistered, asOf, fwdZone)
	if s.Expected == nil || s.Expected.Basis != ForwardPacePastYear || !s.Expected.BeyondWindow {
		t.Fatalf("expected = %+v (%s)", s.Expected, s.NoDate)
	}
	if days := s.Expected.Date.Sub(asOf).Hours() / 24; days < 805 || days > 820 { // 167 units at 73 in the 355 days before registration
		t.Fatalf("expected in %.0f days", days)
	}
	since := []ForwardUnit{}
	for i := range 5 {
		since = append(since, ForwardUnit{Group: ForwardGroupAll, State: "final", Date: fwdRegistered.AddDate(0, 0, i)})
	}
	small := facts
	small.Needs = 10
	s = SummarizeForward(small, since, past, fwdRegistered, asOf, fwdZone)
	if s.Expected == nil || s.Expected.Basis != ForwardPaceSinceRegistration || s.Expected.BeyondWindow {
		t.Fatalf("expected = %+v", s.Expected)
	}
	if days := s.Expected.Date.Sub(asOf).Hours() / 24; days < 9.9 || days > 10.1 { // 5 more at one in two days
		t.Fatalf("expected in %.1f days", days)
	}
	if s := SummarizeForward(facts, nil, nil, fwdRegistered, asOf, fwdZone); s.Expected != nil || s.NoDate != ForwardNoDateNoPace {
		t.Fatalf("no pace: %+v", s)
	}
	if len(s.Months) != 1 || s.Months[0].Month != "2026-03" {
		t.Fatalf("months = %+v", s.Months)
	}
}

// The months count only units that count toward the number needed; a test
// whose groups this version cannot sort says it cannot fill, as does one
// whose date lies beyond the window; waiting and not-counted units are named.
func TestForwardSampleServesWhatAReaderFilters(t *testing.T) {
	facts, _ := ForwardTestFacts("option_buys_after_regime_warning")
	units := []ForwardUnit{
		{Key: "w", Label: "AAA C", Group: "after_warning", State: "final", Date: fwdAt(1)},
		{Key: "o", Label: "BBB C", Group: "other", State: "final", Date: fwdAt(2)},
		{Key: "x", Label: "CCC C", Group: ForwardGroupCannotSort, State: "final", Date: fwdAt(3)},
		{Key: "p", Label: "DDD C", Group: "other", State: "waiting", Date: fwdAt(4)},
		{Key: "d", Label: "EEE C", Group: "after_warning", State: "excluded", Reason: ForwardExcludedDelivered, Date: fwdAt(5)},
	}
	s := SummarizeForward(facts, units, nil, fwdRegistered, fwdAt(10), fwdZone)
	if len(s.Months) != 1 || s.Months[0].Final != 1 {
		t.Fatalf("months = %+v, want only the after-warning unit", s.Months)
	}
	if len(s.WaitingUnits) != 1 || s.WaitingUnits[0].Label != "DDD C" || len(s.NotCounted) != 1 || s.NotCounted[0].Reason != ForwardExcludedDelivered {
		t.Fatalf("waiting=%+v not counted=%+v", s.WaitingUnits, s.NotCounted)
	}
	// One buy after a warning in nine days: the other 39 lie past the window.
	if s.CanFill || s.CannotFill != ForwardCannotFillBeyondWindow || s.Expected == nil || !s.Expected.BeyondWindow {
		t.Fatalf("can fill=%v reason=%q expected=%+v", s.CanFill, s.CannotFill, s.Expected)
	}
	premium, _ := ForwardTestFacts("option_buys_below_premium_line")
	if s := SummarizeForward(premium, nil, nil, fwdRegistered, fwdAt(10), fwdZone); s.CanFill || s.CannotFill != ForwardCannotFillNotWorkedOut {
		t.Fatalf("premium line can fill: %+v", s)
	}
}
