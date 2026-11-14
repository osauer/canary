package risk

import (
	"math"
	"testing"
)

// trimPut is one hedge-listed long put line: quantity contracts, each short
// perContract of delta in its own currency, quoted at fx to the base.
func trimPut(desc string, dte int, quantity, perContract, fx float64) LegInput {
	return LegInput{Desc: desc, Right: "P", Strike: 900, Expiry: etDate(2026, 7, 7).AddDate(0, 0, dte), DTE: dte,
		Quantity: quantity, Multiplier: 100, Mark: 5, Underlying: new(1000.0), Delta: new(-perContract / 100000),
		MarketValueBase: 500 * quantity * fx, ExtrinsicBase: new(500 * quantity * fx), CostBasisBase: new(500 * quantity * fx),
		FXToBase: new(fx), HedgeListed: true}
}

// trimBook is a long book of long base currency beside the given index puts.
func trimBook(long float64, puts ...LegInput) RuleInputs {
	in := limitsInputs()
	exposure := 0.0
	for _, p := range puts {
		exposure += *p.Delta * p.Quantity * p.Multiplier * *p.Underlying * *p.FXToBase
	}
	in.Names = []NameInput{{Symbol: "SPY", ExposureBase: exposure, ExposureBaseComplete: true, Legs: puts}}
	if long > 0 {
		in.Names = append(in.Names, limitsStock("SYNA", long))
	}
	return in
}

// Rule 12 divides the protection's short delta by the long book. The long
// book is in the base currency, so the puts must be too: a put quoted in
// another currency read larger or smaller than it is by the exchange rate,
// and crossed the over-hedge and directional lines at the wrong size.
func TestRule12MeasuresProtectionInTheBaseCurrency(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	// One put, short 40,000 in its own currency, worth half that in base:
	// 20% of a 100,000 long book, under the calm 25-35% range.
	in := trimBook(100000, trimPut("SPY synthetic P", 101, 1, 40000, 0.5))
	r := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity)
	if r.Observed == nil || math.Abs(*r.Observed-20) > 0.05 {
		t.Fatalf("want protection at 20%% of the long book in the base currency: %s", r.Evidence)
	}
	// Short 160,000 in its own currency is 80% in base: over the calm act
	// line of 70%, and far under the 140% line where a put stops being
	// protection.
	in = trimBook(100000, trimPut("SPY synthetic P", 101, 1, 160000, 0.5))
	r = rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity)
	if r.Status != RuleStatusAct || r.Observed == nil || math.Abs(*r.Observed-80) > 0.05 {
		t.Fatalf("an 80%% hedge read %s, want act at 80%%: %s", r.Status, r.Evidence)
	}
	if role := ClassifyIndexPutRoles(in, pol).Names[0].Legs[0].IndexPutRole; role != IndexPutRoleProtection {
		t.Fatalf("an 80%% hedge was classified %s, want protection", role)
	}
}

// The trim returns protection to the band top once it passes the over-hedge
// line (owner decision 2026-11-14 05:45 CET, "Trim card for my tap").
func TestPlanHedgeTrim(t *testing.T) {
	pol := DefaultRulebookPolicy()
	near := trimPut("SPY near P", 30, 2, 10000, 1)
	far := trimPut("SPY far P", 60, 20, 10000, 1)

	t.Run("inside the over-hedge line nothing is offered", func(t *testing.T) {
		// 220,000 short against 400,000 long is 55%: over the calm range,
		// under its 70% act line.
		if plan, ok := PlanHedgeTrim(trimBook(400000, near, far), pol); ok {
			t.Fatalf("a hedge under the over-hedge line was trimmed: %+v", plan)
		}
	})
	t.Run("past the line it sells back to the band top, nearest expiry first", func(t *testing.T) {
		// 220,000 short against 100,000 long is 220%. The calm top is 35%,
		// so 35,000 stays: 18 contracts go and 4 stay, never fewer than the
		// top needs.
		plan, ok := PlanHedgeTrim(trimBook(100000, far, near), pol)
		if !ok {
			t.Fatal("an over-hedged book got no trim")
		}
		if math.Abs(plan.RatioPct-220) > 0.01 || plan.TopPct != 35 || plan.ActPct != 70 || math.Abs(plan.TargetBase-35000) > 0.01 {
			t.Fatalf("plan figures: %+v", plan)
		}
		if len(plan.Lines) != 2 || plan.Lines[0].Leg != "SPY near P" || plan.Lines[0].Sell != 2 || plan.Lines[1].Leg != "SPY far P" || plan.Lines[1].Sell != 16 {
			t.Fatalf("lines: %+v", plan.Lines)
		}
		if after := plan.ShortBase - plan.SoldBase(); after < plan.TargetBase-0.01 || after-plan.TargetBase >= 10000 {
			t.Fatalf("the trim leaves %.0f short against a %.0f target", after, plan.TargetBase)
		}
	})
	t.Run("a carried regime keeps the larger hedge", func(t *testing.T) {
		// Stale early-warning data: the line and the top are the higher of
		// that set and the calm one, so stale data never sells more.
		in := trimBook(100000, far, near)
		in.RegimeStage, in.RegimeStageCarried = RegimeBucketEarlyWarning, true
		plan, ok := PlanHedgeTrim(in, pol)
		if !ok || plan.TopPct != 50 || plan.ActPct != 100 {
			t.Fatalf("carried regime plan: %+v ok=%v", plan, ok)
		}
		in = trimBook(250000, far, near) // 88%: over the calm line, under the carried one
		in.RegimeStage, in.RegimeStageCarried = RegimeBucketEarlyWarning, true
		if plan, ok := PlanHedgeTrim(in, pol); ok {
			t.Fatalf("a hedge under the carried set's line was trimmed: %+v", plan)
		}
	})
	t.Run("a regime never observed keeps the largest hedge", func(t *testing.T) {
		in := trimBook(100000, far, near)
		in.RegimeStage = ""
		plan, ok := PlanHedgeTrim(in, pol)
		if !ok || plan.TopPct != 70 || plan.ActPct != 140 {
			t.Fatalf("unknown regime plan: %+v ok=%v", plan, ok)
		}
	})
	t.Run("with no long book nothing is offered", func(t *testing.T) {
		// Rule 12 is not evaluated without a long book, and a read that lost
		// its long lines looks exactly like this one.
		if plan, ok := PlanHedgeTrim(trimBook(0, far, near), pol); ok {
			t.Fatalf("a book with no long exposure trimmed its puts: %+v", plan)
		}
	})
	t.Run("an unmeasured book offers nothing", func(t *testing.T) {
		// The failure of 2026-11-04: a long line dropped out of the sum and
		// the puts read as over-hedged. A book that is not whole sells no
		// protection.
		in := trimBook(100000, far, near)
		in.Names[1].ExposureBaseComplete = false
		if plan, ok := PlanHedgeTrim(in, pol); ok {
			t.Fatalf("an incomplete long book trimmed the hedge: %+v", plan)
		}
		in = trimBook(100000, far, near)
		in.Positions.Healthy = false
		if plan, ok := PlanHedgeTrim(in, pol); ok {
			t.Fatalf("an unhealthy positions read trimmed the hedge: %+v", plan)
		}
		in = trimBook(100000, far, near)
		in.Names[0].Legs[0].Delta = nil
		if plan, ok := PlanHedgeTrim(in, pol); ok {
			t.Fatalf("a put without its delta trimmed the hedge: %+v", plan)
		}
		// Read as base currency, a put without its rate would look larger
		// than it is and sell more.
		in = trimBook(100000, far, near)
		in.Names[0].Legs[0].FXToBase = nil
		if plan, ok := PlanHedgeTrim(in, pol); ok {
			t.Fatalf("a put without its exchange rate trimmed the hedge: %+v", plan)
		}
	})
	t.Run("a put in another currency is measured in the base", func(t *testing.T) {
		// 220,000 short at 0.25 is 55,000, 55% of the long book: under the
		// line. Unconverted it would read 220% and sell 18 contracts.
		usdNear, usdFar := trimPut("SPY near P", 30, 2, 10000, 0.25), trimPut("SPY far P", 60, 20, 10000, 0.25)
		if plan, ok := PlanHedgeTrim(trimBook(100000, usdFar, usdNear), pol); ok {
			t.Fatalf("a hedge of 55%% in base was trimmed: %+v", plan)
		}
	})
}
