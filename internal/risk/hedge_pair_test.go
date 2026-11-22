package risk

import (
	"math"
	"strings"
	"testing"
)

// soldPut is a put sold against trimPut lines: the same expiry, a lower
// strike, quantity contracts (given positive), each giving back perContract
// of short delta in its own currency.
func soldPut(desc string, dte int, quantity, perContract, fx float64) LegInput {
	l := trimPut(desc, dte, quantity, perContract, fx)
	l.Strike = 800
	l.Quantity = -quantity
	return l
}

// A put spread protects by the pair's short delta, not the bought put's
// alone. Ten bought puts short 150,000 against a 100,000 long book read as
// more than twice the widest band and so as a directional short; with ten
// sold puts giving 70,000 back the pair is short 80,000, which is protection
// rule 12 sizes at 80% of the long book.
func TestRule12SizesAPutSpreadByThePair(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	alone := trimBook(100000, trimPut("SPY bought P", 101, 10, 15000, 1))
	if r := rowByID(t, EvaluateRulebook(alone, pol), RuleHedgeIntegrity); !strings.Contains(r.Evidence, "directional") {
		t.Fatalf("bought puts alone at 150%% of the long book: %+v, want a directional short", r)
	}
	pair := trimBook(100000, trimPut("SPY bought P", 101, 10, 15000, 1), soldPut("SPY sold P", 101, 10, 7000, 1))
	r := rowByID(t, EvaluateRulebook(pair, pol), RuleHedgeIntegrity)
	if r.Observed == nil || math.Abs(*r.Observed-80) > 0.05 || strings.Contains(r.Evidence, "directional") {
		t.Fatalf("put spread = %+v, want protection sized at 80%% of the long book", r)
	}
	// A sold put of another expiry gives nothing back.
	other := trimBook(100000, trimPut("SPY bought P", 101, 10, 15000, 1), soldPut("SPY sold P", 31, 10, 7000, 1))
	if r := rowByID(t, EvaluateRulebook(other, pol), RuleHedgeIntegrity); !strings.Contains(r.Evidence, "directional") {
		t.Fatalf("sold put of another expiry offset the hedge: %+v", r)
	}
}

// The trim sizes its sale on the same pair measurement: four of ten sold
// puts cover four bought ones, so the line is short 150,000 - 28,000.
func TestHedgeTrimMeasuresThePair(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	in := trimBook(100000, trimPut("SPY bought P", 101, 10, 15000, 1), soldPut("SPY sold P", 101, 4, 7000, 1))
	a := AssessHedgeTrim(in, pol)
	if a.State != HedgeTrimPlanned || math.Abs(a.Plan.ShortBase-122000) > 0.5 {
		t.Fatalf("assessment = %+v, want a plan on a short delta of 122,000", a)
	}
}

// Protection premium is named beside the premium budget and the book's
// premium at risk, not counted in them.
func TestPremiumBudgetShowsProtectionBeside(t *testing.T) {
	in := premiumInputs(RegimeBucketCalm, false)
	r := rowByID(t, EvaluateRulebook(in, limitsAllModes(RuleModeAlert)), RuleCashSellOnly)
	if r.Observed == nil || *r.Observed != 27 || r.ImpactBase != 27000 {
		t.Fatalf("premium budget = %+v, want 27%% without the 5,000 protection put", r)
	}
	report := PremiumAtRisk(ClassifyNamePutRoles(in.Names, DefaultRulebookPolicy()))
	if report.TotalBase != 27000 || report.HedgeBase != 5000 {
		t.Fatalf("report total %.0f hedge %.0f, want 27,000 with 5,000 beside", report.TotalBase, report.HedgeBase)
	}
}

// A pair whose sold put gives back all of the bought put's short delta (both
// deep in the money after a fall) is measured at nothing to trim. It is not
// an unmeasured put: that state stops the mandate verdict and with it every
// addition and the forced reduction.
func TestHedgeTrimReadsAPairThatNetsToNothingAsMeasured(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	in := trimBook(100000, trimPut("SPY bought P", 101, 10, 15000, 1), soldPut("SPY sold P", 101, 10, 15000, 1))
	if a := AssessHedgeTrim(in, pol); a.State != HedgeTrimInside {
		t.Fatalf("assessment = %+v, want inside the band with nothing to trim", a)
	}
}
