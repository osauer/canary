package risk

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// Issuer concentration tests (amendment 15, owner decisions 2026-09-26).
// Every book is synthetic: NLV 100,000 in base currency, FX 1, so a loss in
// base reads directly as a percentage.

func concInputs() RuleInputs {
	in := limitsInputs()
	in.Earnings = map[string]EarningsInput{}
	return in
}

func stockLine(sym string, shares, mark float64) NameInput {
	return NameInput{Symbol: sym, HasStockLeg: true, StockQuantity: shares, StockMark: mark, StockFXToBase: new(1.0),
		ExposureBase: shares * mark, MarketValueBase: shares * mark, ExposureBaseComplete: true}
}

// optLeg is one option position; qty is signed contracts and mark is the
// per-share price, so its base value is qty × 100 × mark.
func optLeg(sym, right string, strike, qty, mark float64, dte int, spot float64) LegInput {
	expiry := etDate(2026, 7, 7).AddDate(0, 0, dte)
	return LegInput{Desc: fmt.Sprintf("%s %s %s %g", sym, expiry.Format("20060102"), right, strike), Right: right, Strike: strike,
		Expiry: expiry, DTE: dte, Quantity: qty, Multiplier: 100, Mark: mark, Underlying: new(spot),
		MarketValueBase: qty * 100 * mark, FXToBase: new(1.0)}
}

func withLegs(n NameInput, legs ...LegInput) NameInput {
	n.Legs = append(n.Legs, legs...)
	return n
}

func optionsOnly(sym string, legs ...LegInput) NameInput {
	return NameInput{Symbol: sym, ExposureBaseComplete: true, Legs: legs}
}

func knownEarnings(y int, m time.Month, d int) EarningsInput {
	return EarningsInput{Known: true, Date: etDate(y, m, d), TimeOfDay: "amc", SessionsUntil: new(30), Source: "fetched"}
}

func issuerOf(t *testing.T, in RuleInputs, pol RulebookPolicy, sym string) IssuerExposure {
	t.Helper()
	e, ok := newRuleContext(in, pol).issuerFor(sym)
	if !ok {
		t.Fatalf("issuer for %s not evaluated", sym)
	}
	return e.exposure
}

func legNamed(t *testing.T, x IssuerExposure, prefix string) IssuerLeg {
	t.Helper()
	for _, l := range x.Legs {
		if strings.HasPrefix(l.Leg, prefix) {
			return l
		}
	}
	t.Fatalf("leg %s missing from %+v", prefix, x.Legs)
	return IssuerLeg{}
}

// One deep call with about 100% delta exposure can lose only its premium:
// the cap reads the premium (10%) and passes, while the delta swing, which
// carries the old delta measure, watches.
func TestOneCallIsCappedAtItsPremiumAndWatchedForDelta(t *testing.T) {
	in := concInputs()
	call := optLeg("AAA", "C", 90, 10, 10, 60, 100) // 10,000 of premium on 1,000 share-equivalents
	call.Delta = new(1.0)
	n := optionsOnly("AAA", call)
	n.ExposureBase = 100000
	in.Names = []NameInput{n}
	in.Earnings["AAA"] = knownEarnings(2026, 8, 3)
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	cap := rowByID(t, ev, RuleSingleNameExposure)
	if cap.Status != RuleStatusPass || cap.Observed == nil || *cap.Observed != 10 || !strings.Contains(cap.Evidence, "10.0% of NLV") {
		t.Fatalf("rule 1 must cap one call at its premium: %+v", cap)
	}
	swing := rowByID(t, ev, RuleDeltaSwing)
	if swing.Status != RuleStatusWatch || swing.Observed == nil || *swing.Observed != 100 ||
		!strings.Contains(swing.Evidence, "a 10% fall costs about 10.0% of NLV") || swing.Number != 16 {
		t.Fatalf("delta swing must watch 100%% dollar delta: %+v", swing)
	}
}

// A 20% stock line passes the cap, but against effective risk capital of 8%
// of NLV it is 250% of what may be lost: the loss budget watches.
func TestLossBudgetWatchesAnIssuerLargerThanRemainingRiskCapital(t *testing.T) {
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 200, 100)}
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(8000.0)}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	if cap := rowByID(t, ev, RuleSingleNameExposure); cap.Status != RuleStatusPass || *cap.Observed != 20 {
		t.Fatalf("rule 1 = %+v, want pass at 20%%", cap)
	}
	budget := rowByID(t, ev, RuleLossBudget)
	if budget.Status != RuleStatusWatch || budget.Observed == nil || *budget.Observed != 250 || budget.Number != 18 ||
		!strings.Contains(budget.Evidence, "250.0% of your effective risk capital") || !strings.Contains(budget.Evidence, "100%") {
		t.Fatalf("loss budget = %+v, want watch at 250%%", budget)
	}
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(40000.0)}
	if budget := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleLossBudget); budget.Status != RuleStatusPass {
		t.Fatalf("a loss inside the budget must pass: %+v", budget)
	}
}

// Without the constitution's number the loss budget is unknown and names the
// missing number; it never passes.
func TestLossBudgetWithoutRiskCapitalNamesTheMissingNumber(t *testing.T) {
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 10, 100)}
	budget := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleLossBudget)
	if budget.Status != RuleStatusUnknown || budget.Reason != RuleReasonRiskCapitalUnavailable || !strings.Contains(budget.Evidence, "needs your number") {
		t.Fatalf("absent risk capital = %+v, want unknown naming the number", budget)
	}
	in.RiskCapital = &RiskCapitalInput{Missing: "capital.declared_risk_capital in risk-policy.toml"}
	budget = rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleLossBudget)
	if budget.Status != RuleStatusUnknown || !strings.Contains(budget.Evidence, "capital.declared_risk_capital") {
		t.Fatalf("missing declared capital = %+v, want the key named", budget)
	}
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(-500.0)}
	budget = rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleLossBudget)
	if budget.Status != RuleStatusWatch || !strings.Contains(budget.Evidence, "used up") {
		t.Fatalf("exhausted risk capital = %+v, want watch", budget)
	}
}

// A protective put counts only when it outlives the next earnings and is at
// least hedge_min_days out; otherwise it adds its premium and protects nothing.
func TestProtectivePutCreditNeedsEarningsAndMinimumDays(t *testing.T) {
	pol := DefaultRulebookPolicy()
	book := func(dte int) RuleInputs {
		in := concInputs()
		in.Names = []NameInput{withLegs(stockLine("AAA", 400, 100), optLeg("AAA", "P", 90, 4, 2, dte, 100))}
		in.Earnings["AAA"] = knownEarnings(2026, 10, 21)
		return in
	}
	// 101 days out expires Oct 16, before the Oct 21 print: no credit.
	before := book(101)
	x := issuerOf(t, before, pol, "AAA")
	put := legNamed(t, x, "AAA 20261016 P")
	if x.WorstCaseLossBase != 40800 || put.Hedge != IssuerHedgeUncredited || !strings.Contains(put.Note, "before earnings Oct 21") {
		t.Fatalf("put expiring before earnings: loss %v, leg %+v", x.WorstCaseLossBase, put)
	}
	if row := rowByID(t, EvaluateRulebook(before, pol), RuleSingleNameExposure); row.Status != RuleStatusAct {
		t.Fatalf("an uncredited put must leave the stock at act: %+v", row)
	}
	// 136 days out expires Nov 20, after the print: the loss stops at the strike.
	after := book(136)
	x = issuerOf(t, after, pol, "AAA")
	put = legNamed(t, x, "AAA 20261120 P")
	if x.WorstCaseLossBase != 4800 || put.Hedge != IssuerHedgeCredited || !strings.Contains(put.Note, "after earnings Oct 21") {
		t.Fatalf("put expiring after earnings: loss %v, leg %+v", x.WorstCaseLossBase, put)
	}
	if row := rowByID(t, EvaluateRulebook(after, pol), RuleSingleNameExposure); row.Status != RuleStatusPass {
		t.Fatalf("a credited put must bring the issuer under the watch level: %+v", row)
	}
	// After the print but inside the 14-day minimum: no credit.
	soon := book(10)
	soon.Earnings["AAA"] = knownEarnings(2026, 7, 9)
	x = issuerOf(t, soon, pol, "AAA")
	if put := legNamed(t, x, "AAA 20260717 P"); put.Hedge != IssuerHedgeUncredited || !strings.Contains(put.Note, "inside the 14-day minimum") || x.WorstCaseLossBase != 40800 {
		t.Fatalf("put inside the minimum days: loss %v, leg %+v", x.WorstCaseLossBase, put)
	}
	// Unknown earnings: only the minimum applies, and the row says so.
	unknown := book(60)
	delete(unknown.Earnings, "AAA")
	x = issuerOf(t, unknown, pol, "AAA")
	if x.WorstCaseLossBase != 4800 || len(x.Notes) == 0 || !strings.Contains(strings.Join(x.Notes, " "), "hedge credit uses the 14-day minimum only") {
		t.Fatalf("unknown earnings: loss %v, notes %v", x.WorstCaseLossBase, x.Notes)
	}
}

// Leg results the owner fixed: a covered call credits only its premium, a
// short put counts its strike notional less its current liability, and short
// stock and uncovered short calls are unbounded, sized at the takeover gap.
func TestIssuerLegRulesNetFromCurrentMarks(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()

	in.Names = []NameInput{stockLine("AAA", 100, 100)}
	stockOnly := issuerOf(t, in, pol, "AAA")
	if stockOnly.WorstCaseLossBase != 10000 || *stockOnly.WorstMovePct != -100 {
		t.Fatalf("long stock loses its market value: %+v", stockOnly)
	}
	in.Names = []NameInput{withLegs(stockLine("AAA", 100, 100), optLeg("AAA", "C", 110, -1, 3, 60, 100))}
	covered := issuerOf(t, in, pol, "AAA")
	if covered.WorstCaseLossBase != 9700 || covered.Unbounded {
		t.Fatalf("covered call must credit only its 300 premium: %+v", covered)
	}

	in.Names = []NameInput{optionsOnly("AAA", optLeg("AAA", "P", 80, -5, 2, 60, 100))}
	shortPut := issuerOf(t, in, pol, "AAA")
	if shortPut.WorstCaseLossBase != 39000 {
		t.Fatalf("short put must count 40,000 of strike notional less its 1,000 liability: %+v", shortPut)
	}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusWatch {
		t.Fatalf("39%% short put = %+v", row)
	}

	in.Names = []NameInput{stockLine("AAA", -100, 100)}
	short := issuerOf(t, in, pol, "AAA")
	leg := legNamed(t, short, "AAA stock")
	if short.WorstCaseLossBase != 10000 || !short.Unbounded || !leg.Unbounded || !strings.Contains(leg.Note, "sized at the 100% takeover gap") || *short.WorstMovePct != 100 {
		t.Fatalf("short stock must be unbounded at the takeover gap: %+v", short)
	}
	half := DefaultRulebookPolicy()
	half.TakeoverGapPct = 50
	if x := issuerOf(t, in, half, "AAA"); x.WorstCaseLossBase != 5000 {
		t.Fatalf("takeover_gap_pct must size the unbounded leg: %+v", x)
	}

	in.Names = []NameInput{optionsOnly("AAA", optLeg("AAA", "C", 110, -2, 3, 60, 100))}
	naked := issuerOf(t, in, pol, "AAA")
	if naked.WorstCaseLossBase != 17400 || !naked.Unbounded || !legNamed(t, naked, "AAA").Unbounded {
		t.Fatalf("uncovered short call must be unbounded at the takeover gap: %+v", naked)
	}

	in.Names = []NameInput{withLegs(stockLine("AAA", 100, 100), optLeg("AAA", "C", 110, -2, 3, 60, 100))}
	partly := issuerOf(t, in, pol, "AAA")
	if !partly.Unbounded || !strings.Contains(legNamed(t, partly, "AAA 2026").Note, "about 50% uncovered") {
		t.Fatalf("one of two calls covered must say half is uncovered: %+v", partly)
	}
}

// Any short leg can be assigned early. Assignment realizes exactly the leg's
// intrinsic value, so the netted worst case of a put spread equals the worst
// case of the book after an immediate assignment, less the time value that
// assignment gives up.
func TestNettingHoldsWhenAShortLegIsAssignedEarly(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	longPut := optLeg("AAA", "P", 90, 1, 1, 60, 100)
	in.Names = []NameInput{optionsOnly("AAA", optLeg("AAA", "P", 110, -1, 12, 60, 100), longPut)}
	spread := issuerOf(t, in, pol, "AAA")
	if spread.WorstCaseLossBase != 900 {
		t.Fatalf("put spread worst case = %v, want 2,000 of width less 1,100 net credit", spread.WorstCaseLossBase)
	}
	// Assigned now: the short put becomes 100 shares bought at 110 against a
	// 100 price, a realized 1,000 of intrinsic against its 1,200 mark.
	in.Names = []NameInput{withLegs(stockLine("AAA", 100, 100), longPut)}
	assigned := issuerOf(t, in, pol, "AAA")
	realizedAgainstMark := 1200.0 - 1000.0
	if got := assigned.WorstCaseLossBase - realizedAgainstMark; math.Abs(got-spread.WorstCaseLossBase) > 1e-9 {
		t.Fatalf("assignment broke the netting: assigned loss %v, spread loss %v", assigned.WorstCaseLossBase, spread.WorstCaseLossBase)
	}
}

// An index put protects the book, not an issuer: it sits on its own issuer and
// gives the stock no credit.
func TestIndexPutGivesNoIssuerCredit(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	spyPut := optLeg("SPY", "P", 600, 10, 5, 120, 650)
	spyPut.HedgeListed, spyPut.Delta = true, new(-0.3)
	spy := optionsOnly("SPY", spyPut)
	spy.ExposureBase = -0.3 * 10 * 100 * 650
	in.Names = []NameInput{stockLine("AAA", 400, 100), spy}
	if x := issuerOf(t, in, pol, "AAA"); x.WorstCaseLossBase != 40000 {
		t.Fatalf("an index put credited the issuer: %+v", x)
	}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusAct || row.Offenders[0].Symbol != "AAA" {
		t.Fatalf("rule 1 = %+v, want AAA at act", row)
	}
	if x := issuerOf(t, in, pol, "SPY"); x.WorstCaseLossBase != 5000 {
		t.Fatalf("a long index put can lose only its premium: %+v", x)
	}
}

// Share classes the owner groups are one issuer: their legs net and their
// losses add.
func TestIssuerGroupsJoinShareClasses(t *testing.T) {
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 200, 100), stockLine("AAB", 300, 50)}
	pol := DefaultRulebookPolicy()
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusPass || *row.Observed != 20 {
		t.Fatalf("ungrouped lines = %+v, want pass at 20%%", row)
	}
	pol.IssuerGroups = map[string][]string{"GroupA": {"aaa", " AAB "}}
	row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure)
	if row.Status != RuleStatusWatch || *row.Observed != 35 || len(row.Offenders) != 1 {
		t.Fatalf("grouped lines = %+v, want one watch offender at 35%%", row)
	}
	x := row.Offenders[0].Issuer
	if row.Offenders[0].Symbol != "GroupA" || x == nil || strings.Join(x.Lines, ",") != "AAA,AAB" || len(x.Legs) != 2 {
		t.Fatalf("grouped offender = %+v (%+v)", row.Offenders[0], x)
	}
	// A put on one class protects the issuer: both lines move together.
	in.Names[0] = withLegs(in.Names[0], optLeg("AAA", "P", 90, 4, 2, 60, 100))
	if x := issuerOf(t, in, pol, "AAB"); x.WorstCaseLossBase != 4300 {
		t.Fatalf("a put on one share class must net against the issuer: %+v", x)
	}
}

// Every issuer in a declared cluster falls together; the watch trips at
// exactly the watch level and the row never acts.
func TestClusterFallingTogetherWatchesAtFifteenPercent(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 300, 100), stockLine("BBB", 400, 50), stockLine("CCC", 100, 10)}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleClusterStress); row.Status != RuleStatusNotEvaluated || row.Reason != RuleReasonNoClusters {
		t.Fatalf("no cluster declared = %+v, want not_evaluated", row)
	}
	pol.Clusters = map[string][]string{"ClusterA": {"AAA", "BBB"}}
	row := rowByID(t, EvaluateRulebook(in, pol), RuleClusterStress)
	if row.Status != RuleStatusWatch || row.Observed == nil || *row.Observed != 15 || row.Number != 17 ||
		!strings.Contains(row.Evidence, "fall 30% together") || !strings.Contains(row.Evidence, "15%") {
		t.Fatalf("30%% fall of 50,000 = %+v, want watch at exactly 15%%", row)
	}
	in.Names[1].StockQuantity = 399
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleClusterStress); row.Status != RuleStatusPass {
		t.Fatalf("just under the watch level = %+v, want pass", row)
	}
	in.Names[0].StockQuantity = 900
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleClusterStress); row.Status != RuleStatusWatch {
		t.Fatalf("a large cluster loss must still only watch: %+v", row)
	}
	// A cluster member may name an issuer group.
	pol.IssuerGroups = map[string][]string{"GroupB": {"BBB", "CCC"}}
	pol.Clusters = map[string][]string{"ClusterB": {"groupb"}}
	in.Names[0].StockQuantity = 300
	in.Names[1].StockQuantity = 1000
	row = rowByID(t, EvaluateRulebook(in, pol), RuleClusterStress)
	if row.Status != RuleStatusWatch || *row.Observed != 15.3 {
		t.Fatalf("cluster via an issuer group = %+v, want 51,000 × 30%% = 15.3%%", row)
	}
}

// An issuer that takes longer than illiquid_days_to_exit to leave at the
// allowed share of its 20-day volume is measured against 20/30.
func TestIlliquidIssuerUsesTighterBands(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	line := stockLine("AAA", 250, 100)
	line.AvgDailyVolume = new(100.0) // 250 / (20% × 100) = 12.5 days
	in.Names = []NameInput{line}
	row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure)
	if row.Status != RuleStatusWatch || *row.WatchThreshold != 20 || *row.ActThreshold != 30 || *row.Threshold != 20 ||
		!strings.Contains(row.Evidence, "20% watch level for an illiquid issuer") || row.Offenders[0].Issuer.DaysToExit == nil || *row.Offenders[0].Issuer.DaysToExit != 12.5 {
		t.Fatalf("illiquid 25%% = %+v, want watch against 20/30", row)
	}
	in.Names[0].StockQuantity = 300 // exactly the illiquid act band
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusAct || *row.Threshold != 30 {
		t.Fatalf("illiquid 30%% = %+v, want act at exactly the illiquid cap", row)
	}
	in.Names[0].StockQuantity = 250
	in.Names[0].AvgDailyVolume = new(1e6)
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusPass || *row.Threshold != 30 {
		t.Fatalf("liquid 25%% = %+v, want pass against 30/40", row)
	}
	in.Names[0].AvgDailyVolume = nil
	x := issuerOf(t, in, pol, "AAA")
	if x.Illiquid || x.WatchPct != 30 || !strings.Contains(strings.Join(x.Notes, " "), "average volume unavailable") {
		t.Fatalf("missing volume must keep normal bands and say so: %+v", x)
	}
	// An option-only issuer uses share-equivalents: |delta| × contracts × 100.
	call := optLeg("BBB", "C", 50, 10, 25, 60, 70) // 25% of NLV in premium
	call.Delta = new(0.5)
	opt := optionsOnly("BBB", call)
	opt.AvgDailyVolume = new(200.0) // 500 / 40 = 12.5 days
	in.Names = []NameInput{opt}
	if x := issuerOf(t, in, pol, "BBB"); !x.Illiquid || x.DaysToExit == nil || *x.DaysToExit != 12.5 {
		t.Fatalf("option-only share-equivalents: %+v", x)
	}
}

// The cap compares at or above both bands (bandStatus): exactly 30 watches
// and exactly 40 acts.
func TestIssuerCapEqualityAtThirtyAndForty(t *testing.T) {
	pol := DefaultRulebookPolicy()
	for _, c := range []struct {
		shares float64
		status string
	}{{299.99, RuleStatusPass}, {300, RuleStatusWatch}, {399.99, RuleStatusWatch}, {400, RuleStatusAct}} {
		in := concInputs()
		in.Names = []NameInput{stockLine("AAA", c.shares, 100)}
		if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != c.status {
			t.Errorf("%v%% of NLV = %s, want %s (%s)", c.shares/10, row.Status, c.status, row.Evidence)
		}
	}
}

// The risk-reduction trim starts at act and goes back to the watch band on
// the same measure.
func TestIssuerTrimReturnsToTheWatchBand(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 450, 100)}
	plan, ok := PlanIssuerTrim(in, pol, "AAA")
	if !ok || !plan.Reachable || !plan.Stock || plan.Quantity != 150 || plan.LossAfterBase != 30000 || plan.TargetBase != 30000 {
		t.Fatalf("45%% stock plan = %+v, want sell 150 shares to 30%%", plan)
	}
	// At watch there is nothing to trim.
	in.Names = []NameInput{stockLine("AAA", 350, 100)}
	if _, ok := PlanIssuerTrim(in, pol, "AAA"); ok {
		t.Fatal("a trim was planned below the act band")
	}
	// A covered call keeps its premium credit, so fewer shares go.
	in.Names = []NameInput{withLegs(stockLine("AAA", 450, 100), optLeg("AAA", "C", 110, -3, 3, 60, 100))}
	plan, ok = PlanIssuerTrim(in, pol, "AAA")
	if !ok || !plan.Reachable || plan.Quantity != 141 || plan.LossAfterBase > 30000 {
		t.Fatalf("covered call plan = %+v, want sell 141 shares", plan)
	}
	// A short put that dominates is bought back rather than the stock sold:
	// 49,600 at a fall to zero, each put 9,900 of it, so two go.
	in.Names = []NameInput{withLegs(stockLine("AAA", 100, 100), optLeg("AAA", "P", 100, -4, 1, 60, 100))}
	plan, ok = PlanIssuerTrim(in, pol, "AAA")
	if !ok || plan.Stock || plan.Quantity != 2 || plan.LossAfterBase != 29800 || plan.Held != -4 {
		t.Fatalf("short put plan = %+v, want two puts bought back", plan)
	}
	// Illiquid issuers trim to their own watch band.
	line := stockLine("AAA", 350, 100)
	line.AvgDailyVolume = new(100.0)
	in.Names = []NameInput{line}
	plan, ok = PlanIssuerTrim(in, pol, "AAA")
	if !ok || plan.Quantity != 150 || plan.WatchPct != 20 {
		t.Fatalf("illiquid plan = %+v, want sell 150 shares to 20%%", plan)
	}
}

// Rule 8's size test is rule 1's own verdict on the issuer.
func TestEarningsFreezeReadsTheIssuerVerdict(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := concInputs()
	call := optLeg("AAA", "C", 90, 10, 10, 60, 100)
	call.Delta = new(1.0)
	n := optionsOnly("AAA", call)
	n.ExposureBase = 100000 // 100% in delta, 10% in worst-case loss
	in.Names = []NameInput{n}
	in.Earnings["AAA"] = EarningsInput{Known: true, Date: etDate(2026, 7, 9), TimeOfDay: "amc", SessionsUntil: new(2), Source: "fetched"}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleEarningsSizeFreeze); row.Status != RuleStatusPass {
		t.Fatalf("an issuer under rule 1's watch level is not oversized: %+v", row)
	}
	in.Names = []NameInput{stockLine("AAA", 350, 100)}
	row := rowByID(t, EvaluateRulebook(in, pol), RuleEarningsSizeFreeze)
	if row.Status != RuleStatusAct || !strings.Contains(row.Offenders[0].Note, "can lose 35.0% of NLV") {
		t.Fatalf("an issuer at rule 1's watch level inside the window = %+v", row)
	}
}

// Delta swing mentions gamma only when it bends a 10% move by at least the
// materiality floor.
func TestDeltaSwingMentionsMaterialGamma(t *testing.T) {
	in := concInputs()
	call := optLeg("AAA", "C", 100, -20, 5, 60, 100)
	call.Delta, call.Gamma = new(-0.5), new(0.04)
	n := withLegs(stockLine("AAA", 2000, 100), call)
	n.ExposureBase = 2000*100 - 0.5*20*100*100
	in.Names = []NameInput{n}
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleDeltaSwing)
	// short gamma: 0.5 × 0.04 × 10² × (−2,000) = −4,000 = 4% of NLV
	if row.Status != RuleStatusWatch || !strings.Contains(row.Evidence, "short gamma adds about 4.0% of NLV") {
		t.Fatalf("material short gamma = %+v", row)
	}
	call.Gamma = new(0.001)
	n = withLegs(stockLine("AAA", 2000, 100), call)
	n.ExposureBase = 2000*100 - 0.5*20*100*100
	in.Names = []NameInput{n}
	if row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleDeltaSwing); strings.Contains(row.Evidence, "gamma") {
		t.Fatalf("immaterial gamma was mentioned: %s", row.Evidence)
	}
}

// Partial data may indict, never acquit: an unmeasured line keeps rule 1 and
// its watches unknown unless a measured issuer already breaches.
func TestIssuerGapsNeverAcquit(t *testing.T) {
	pol := DefaultRulebookPolicy()
	pol.Clusters = map[string][]string{"ClusterAll": {"AAA", "BBB"}}
	in := concInputs()
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(50000.0)}
	gap := stockLine("BBB", 10, 100)
	gap.StockFXToBase = nil
	in.Names = []NameInput{stockLine("AAA", 100, 100), gap}
	ev := EvaluateRulebook(in, pol)
	for _, id := range []string{RuleSingleNameExposure, RuleClusterStress, RuleLossBudget} {
		if row := rowByID(t, ev, id); row.Status != RuleStatusUnknown {
			t.Errorf("%s = %s over an unmeasured line, want unknown (%s)", id, row.Status, row.Evidence)
		}
	}
	// A spotless option-only short call can be proven to lose at least its
	// value at the highest strike, never acquitted.
	naked := optLeg("CCC", "C", 50, -100, 1, 60, 0)
	naked.Underlying = nil
	in.Names = []NameInput{optionsOnly("CCC", naked)}
	x := issuerOf(t, in, pol, "CCC")
	if !x.LowerBound || !x.Unbounded {
		t.Fatalf("spotless naked call = %+v, want a lower bound flagged unbounded", x)
	}
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure); row.Status != RuleStatusUnknown {
		t.Fatalf("an unsizable unbounded leg acquitted rule 1: %+v", row)
	}
}

// The three watches never act, whatever the reading.
func TestConcentrationWatchesNeverAct(t *testing.T) {
	pol := DefaultRulebookPolicy()
	pol.Clusters = map[string][]string{"ClusterAll": {"AAA"}}
	in := concInputs()
	in.Names = []NameInput{stockLine("AAA", 5000, 100)}
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(1000.0)}
	for _, row := range EvaluateRulebook(in, pol).Rows {
		if WatchOnlyRule(row.ID) && row.Status != RuleStatusWatch {
			t.Errorf("%s = %s at an extreme reading, want watch", row.ID, row.Status)
		}
		if WatchOnlyRule(row.ID) && row.ActThreshold != nil {
			t.Errorf("%s carries an act band", row.ID)
		}
	}
}

// Owner decision 2026-09-26: rule 18 alerts by default, so a worst-case loss
// beyond the remaining risk capital can open an alert episode; rules 16 and
// 17 are tracked. All three stay watch-only.
func TestConcentrationWatchDefaultModes(t *testing.T) {
	p := DefaultRulebookPolicy()
	if p.ModeFor(RuleLossBudget) != RuleModeAlert || p.ModeFor(RuleDeltaSwing) != RuleModeTrack || p.ModeFor(RuleClusterStress) != RuleModeTrack {
		t.Fatalf("default modes: loss_budget %s, delta_swing %s, cluster_stress %s; want alert, track, track",
			p.ModeFor(RuleLossBudget), p.ModeFor(RuleDeltaSwing), p.ModeFor(RuleClusterStress))
	}
}
