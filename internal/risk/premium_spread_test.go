package risk

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// spreadPut is one put leg of a synthetic index name at a mark of 100 in
// base currency: quantity is signed, paid is the premium paid (long) or
// received (short) for the whole leg, value its unsigned value today.
func spreadPut(strike, quantity, paid, value float64, expiry time.Time) LegInput {
	sign := 1.0
	if quantity < 0 {
		sign = -1
	}
	return LegInput{Desc: fmt.Sprintf("IDX synthetic P %g", strike), Right: "P", Strike: strike, Expiry: expiry, DTE: 30,
		Quantity: quantity, Multiplier: 100, Mark: value / math.Abs(quantity) / 100, Underlying: new(700.0), Delta: new(-0.3 * sign),
		MarketValueBase: sign * value, CostBasisBase: new(paid), FXToBase: new(1.0)}
}

func spreadName(legs ...LegInput) []NameInput {
	return []NameInput{{Symbol: "IDX", ExposureBaseComplete: true, ExposureBase: -10000, Legs: legs}}
}

// A bought option with a sold option of the same kind and expiry further out
// of the money against it can lose the net premium of the pair, not the
// bought leg's whole price. Ten 520 puts bought for 20,000 against ten 470
// puts sold for 8,000 put 12,000 at risk.
func TestPremiumAtRiskCountsASpreadAtWhatThePairCanLose(t *testing.T) {
	exp := etDate(2026, 12, 18)
	later := etDate(2027, 1, 15)
	unknownCredit := spreadPut(470, -10, 8000, 7000, exp)
	unknownCredit.CostBasisBase = nil
	otherRight := spreadPut(470, -10, 8000, 7000, exp)
	otherRight.Right = "C"
	for name, tc := range map[string]struct {
		legs []LegInput
		want float64
	}{
		"bought leg alone":                       {[]LegInput{spreadPut(520, 10, 20000, 18000, exp)}, 20000},
		"whole spread at its net price paid":     {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), spreadPut(470, -10, 8000, 7000, exp)}, 12000},
		"spread worth more than paid, its value": {[]LegInput{spreadPut(520, 10, 20000, 30000, exp), spreadPut(470, -10, 8000, 9000, exp)}, 21000},
		"four of ten covered, six in full":       {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), spreadPut(470, -4, 3200, 2800, exp)}, 16800},
		"more sold than bought covers ten":       {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), spreadPut(470, -20, 16000, 14000, exp)}, 12000},
		"net credit never goes below zero":       {[]LegInput{spreadPut(520, 10, 5000, 4000, exp), spreadPut(470, -10, 8000, 7000, exp)}, 0},
		"sold leg of another expiry":             {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), spreadPut(470, -10, 8000, 7000, later)}, 20000},
		"sold leg nearer the money":              {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), spreadPut(700, -10, 26000, 24000, exp)}, 20000},
		"sold leg of the other kind":             {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), otherRight}, 20000},
		"sold leg without a known credit":        {[]LegInput{spreadPut(520, 10, 20000, 18000, exp), unknownCredit}, 20000},
		"two bought strikes share one sold leg": {[]LegInput{spreadPut(520, 5, 10000, 9000, exp), spreadPut(680, 5, 8000, 7000, exp),
			spreadPut(470, -10, 8000, 7000, exp)}, 10000},
	} {
		t.Run(name, func(t *testing.T) {
			got := PremiumAtRisk(spreadName(tc.legs...))
			if !got.Known || math.Abs(got.TotalBase-tc.want) > 0.01 {
				t.Fatalf("premium at risk = %.2f (known %v), want %.2f", got.TotalBase, got.Known, tc.want)
			}
		})
	}
}

// Rules 2 and 3 read the same figure: the spread counts once, at its net
// premium, in the position rule and in the budget, and the row says so.
func TestRulebookCountsASpreadAtItsNetPremium(t *testing.T) {
	exp := etDate(2026, 12, 18)
	in := limitsInputs()
	in.Names = append([]NameInput{limitsStock("AAA", 20000)},
		spreadName(spreadPut(520, 10, 20000, 18000, exp), spreadPut(470, -10, 8000, 7000, exp))...)
	ev := EvaluateRulebook(in, limitsAllModes(RuleModeAlert))
	budget := rowByID(t, ev, RuleCashSellOnly)
	if budget.Observed == nil || *budget.Observed != 12 || budget.ImpactBase != 12000 {
		t.Fatalf("premium budget = %+v, want 12%% of NLV for a spread that can lose 12,000", budget)
	}
	if len(budget.Offenders) != 1 || !strings.Contains(budget.Offenders[0].Note, "sold") {
		t.Fatalf("offenders = %+v, want the bought leg with a note naming the sold option against it", budget.Offenders)
	}
	line := rowByID(t, ev, RuleOptionLinePremium)
	if line.Observed == nil || *line.Observed != 12 {
		t.Fatalf("position premium = %+v, want the same 12%%", line)
	}
}

// Two classes of one index can expire on one date and settle on different
// prices (a monthly on the opening print, a weekly on the close), so a sold
// option of another class caps nothing: the bought leg counts in full and
// gives no short delta back. One class written in two cases is one class.
func TestASoldOptionOfAnotherClassCapsNothing(t *testing.T) {
	exp := etDate(2026, 12, 18)
	long, short := spreadPut(520, 10, 20000, 15000, exp), spreadPut(470, -10, 8000, 7000, exp)
	long.TradingClass, short.TradingClass = "IDXW", "IDX"
	figures := PremiumAtRiskLegs([]LegInput{long, short})
	if figures[0].Covered != 0 || figures[0].AtRisk != 20000 {
		t.Fatalf("bought weekly against a sold monthly = %+v, want the bought leg in full at 20,000", figures[0])
	}
	if shorts := indexPutShorts([]LegInput{withHedge(long), withHedge(short)}); shorts[0] != indexPutShortBase(withHedge(long)) {
		t.Fatalf("short delta %v, want the bought put's own %v", shorts[0], indexPutShortBase(withHedge(long)))
	}
	long.TradingClass, short.TradingClass = "idx", " IDX "
	if figures := PremiumAtRiskLegs([]LegInput{long, short}); figures[0].Covered != 10 {
		t.Fatalf("one class in two spellings = %+v, want the pair counted", figures[0])
	}
}

func withHedge(l LegInput) LegInput {
	l.HedgeListed = true
	return l
}
