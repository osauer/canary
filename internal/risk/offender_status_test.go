package risk

import (
	"testing"
)

// offenderStatuses maps each offender (symbol or leg) to its own status.
func offenderStatuses(row RuleRow) map[string]string {
	out := map[string]string{}
	for _, o := range row.Offenders {
		key := o.Symbol
		if o.Leg != "" {
			key = o.Leg
		}
		out[key] = o.Status
	}
	return out
}

// Coordinator decision 2026-09-26: every offender of a rule with watch and
// act levels carries its own band, measured against its own limits, so a
// second offender between the levels reads watch while the row reads act.
func TestEachOffenderCarriesItsOwnBand(t *testing.T) {
	pol := DefaultRulebookPolicy()

	// Rule 1: AAA at 45% acts, BBB at 35% watches, and CCC at 25% watches
	// against its own illiquid 20/30 bands although 25% passes the normal
	// ones. DDD has no price and reads unknown.
	in := concInputs()
	illiquid := stockLine("CCC", 250, 100)
	illiquid.AvgDailyVolume = new(100.0)
	unpriced := stockLine("DDD", 100, 0)
	unpriced.StockMark, unpriced.ExposureBaseComplete = 0, false
	in.Names = []NameInput{stockLine("BBB", 350, 100), illiquid, stockLine("AAA", 450, 100), unpriced}
	row := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure)
	got := offenderStatuses(row)
	if row.Status != RuleStatusAct || got["AAA"] != RuleStatusAct || got["BBB"] != RuleStatusWatch || got["CCC"] != RuleStatusWatch {
		t.Fatalf("rule 1 row %s, offenders %v\n%s", row.Status, got, row.Evidence)
	}
	if row.Offenders[0].Symbol != "AAA" {
		t.Fatalf("the acting issuer is not listed first: %v", row.Offenders)
	}
	if got["DDD"] != RuleStatusUnknown {
		t.Fatalf("an unmeasured issuer reads %q: %+v", got["DDD"], row.Offenders)
	}

	// Rule 2: a 12% line acts, a 7% line watches, and a protection put at
	// 20% watches on its own 15/25 tier.
	in = concInputs()
	a := optLeg("AAA", "C", 100, 12, 10, 90, 100)  // 12,000 = 12%
	b := optLeg("BBB", "C", 100, 7, 10, 90, 100)   // 7,000 = 7%
	h := optLeg("SPY", "P", 400, 20, 10, 120, 450) // 20,000 = 20%
	h.HedgeListed, h.Delta = true, new(-0.01)      // 9,000 of short delta protects the long book
	in.Names = []NameInput{optionsOnly("AAA", a), optionsOnly("BBB", b), optionsOnly("SPY", h), stockLine("EEE", 500, 100)}
	row = rowByID(t, EvaluateRulebook(in, pol), RuleOptionLinePremium)
	got = offenderStatuses(row)
	if row.Status != RuleStatusAct || got[a.Desc] != RuleStatusAct || got[b.Desc] != RuleStatusWatch || got[h.Desc] != RuleStatusWatch {
		t.Fatalf("rule 2 row %s, offenders %v", row.Status, got)
	}

	// Rule 13: a line down 65% acts, one down 45% watches.
	in = concInputs()
	lost65 := optLeg("AAA", "C", 100, 1, 3.5, 90, 80)
	lost65.CostBasisBase = new(1000.0) // value 350
	lost45 := optLeg("BBB", "C", 100, 1, 5.5, 90, 80)
	lost45.CostBasisBase = new(1000.0) // value 550
	in.Names = []NameInput{optionsOnly("AAA", lost65), optionsOnly("BBB", lost45)}
	row = rowByID(t, EvaluateRulebook(in, pol), RuleExitDiscipline)
	got = offenderStatuses(row)
	if row.Status != RuleStatusAct || got[lost65.Desc] != RuleStatusAct || got[lost45.Desc] != RuleStatusWatch {
		t.Fatalf("rule 13 row %s, offenders %v", row.Status, got)
	}

	// Rules 16 and 18 never act, so their offenders read watch.
	in = concInputs()
	in.Names = []NameInput{stockLine("AAA", 450, 100)}
	in.RiskCapital = &RiskCapitalInput{EffectiveBase: new(40000.0)}
	for _, id := range []string{RuleDeltaSwing, RuleLossBudget} {
		row := rowByID(t, EvaluateRulebook(in, pol), id)
		if row.Status != RuleStatusWatch || len(row.Offenders) == 0 || row.Offenders[0].Status != RuleStatusWatch {
			t.Fatalf("%s row %s offenders %+v", id, row.Status, row.Offenders)
		}
	}
}
