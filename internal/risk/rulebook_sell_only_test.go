package risk

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// premiumBook is a synthetic book at 100,000 NLV (limitsInputs): a losing
// call counted at its price paid (12,000 over an 8,000 value), a gaining call
// at its value (15,000 over a 10,000 cost), a short call and a protection put
// that stay outside the budget, and a stock line small enough that rule 15
// stays quiet. Premium at risk: 27,000 = 27% of NLV.
func premiumBook() []NameInput {
	losing := limitsLongCall("AAA", 60, 8000)
	losing.CostBasisBase = new(12000.0)
	gaining := limitsLongCall("BBB", 90, 15000)
	gaining.CostBasisBase = new(10000.0)
	short := limitsLongCall("BBB", 30, -2000)
	short.Quantity = -1
	names := limitsProtection(30, 5000)
	names[0] = limitsStock("AAA", 20000)
	names[0].Legs = []LegInput{losing}
	return append(names, NameInput{Symbol: "BBB", ExposureBaseComplete: true, ExposureBase: 5000, Legs: []LegInput{gaining, short}})
}

func premiumInputs(stage string, carried bool) RuleInputs {
	in := limitsInputs()
	in.Names = premiumBook()
	in.RegimeStage, in.RegimeStageCarried = stage, carried
	if carried {
		in.RegimeStageAsOf = in.AsOf.Add(-6 * time.Hour)
	}
	return in
}

// Rule 3 is the premium budget (amendment 17): premium at risk outside
// protection over NLV, the per-leg figure of rule 2, against the regime set's
// watch and act levels, with available funds as context only.
func TestPremiumBudgetMeasuresPremiumAtRiskOutsideProtection(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	ev := EvaluateRulebook(premiumInputs(RegimeBucketCalm, false), pol)
	r := rowByID(t, ev, RuleCashSellOnly)
	if r.Number != 3 || r.Title != "Premium budget" || r.Status != RuleStatusWatch || r.Observed == nil || *r.Observed != 27 ||
		*r.WatchThreshold != 25 || *r.ActThreshold != 35 || *r.Threshold != 25 || r.RegimeSet != RegimeBucketCalm {
		t.Fatalf("premium budget row = %+v", r)
	}
	if r.ImpactBase != 27000 {
		t.Fatalf("ranking impact = %v, want the offending premium in base currency, 27000", r.ImpactBase)
	}
	want := "Option premium at risk is 27.0% of NLV, at or above the calm set's 25% budget (act at 35%); sell-only until it is back under the budget. Available funds are 90.0% of NLV."
	if r.Evidence != want {
		t.Fatalf("evidence = %q\nwant       %q", r.Evidence, want)
	}
	if len(r.Offenders) != 2 || r.Offenders[0].Symbol != "BBB" || r.Offenders[1].Note != "counted at the price paid, above today's value" {
		t.Fatalf("offenders = %+v, want the two long calls, largest first, the loser at its price paid", r.Offenders)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "protection premium 5.0% of NLV excluded") }) {
		t.Fatalf("notes = %v, want the excluded protection premium disclosed", r.Notes)
	}
	if !r.SellOnly || !ev.SellOnly.Active || !slices.Equal(ev.SellOnly.Rules, []string{RuleCashSellOnly}) {
		t.Fatalf("sell-only = row %v, result %+v; want rule 3 alone", r.SellOnly, ev.SellOnly)
	}
}

// Each regime set carries its own budget for new buying (the watch level);
// the cap (the act level) is the same in every set by default, so a rise in
// volatility alone moves a book from pass to watch but never to act (reviewer
// decision 2026-09-30 10:12 CEST). A carried stage keeps the worse of its set
// and calm, and a never-seen stage reads calm. The row names the set.
func TestPremiumBudgetFollowsTheRegimeSet(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	for _, c := range []struct {
		name       string
		stage      string
		carried    bool
		status     string
		watch, act float64
		set        string
		words      string
	}{
		{"calm", RegimeBucketCalm, false, RuleStatusWatch, 25, 35, RegimeBucketCalm, "calm set's 25% budget"},
		{"early warning", RegimeBucketEarlyWarning, false, RuleStatusWatch, 20, 35, RegimeBucketEarlyWarning, "early-warning set's 20% budget"},
		{"confirmed", RegimeBucketConfirmed, false, RuleStatusWatch, 15, 35, RegimeBucketConfirmed, "confirmed-stress set's 15% budget"},
		{"carried confirmed keeps the worse", RegimeBucketConfirmed, true, RuleStatusWatch, 15, 35, RegimeBucketConfirmed, "confirmed-stress set's 15% budget"},
		{"never seen reads calm", "", false, RuleStatusWatch, 25, 35, RegimeBucketCalm, "calm set's 25% budget"},
	} {
		r := rowByID(t, EvaluateRulebook(premiumInputs(c.stage, c.carried), pol), RuleCashSellOnly)
		if r.Status != c.status || *r.WatchThreshold != c.watch || *r.ActThreshold != c.act || r.RegimeSet != c.set || !strings.Contains(r.Evidence, c.words) {
			t.Errorf("%s: %s watch %v act %v set %q evidence %q", c.name, r.Status, *r.WatchThreshold, *r.ActThreshold, r.RegimeSet, r.Evidence)
		}
	}

	// The same book at 22%: a pass in calm, a watch (no new buying) under
	// early warning and confirmed stress, and never an act.
	for stage, want := range map[string]string{RegimeBucketCalm: RuleStatusPass, RegimeBucketEarlyWarning: RuleStatusWatch, RegimeBucketConfirmed: RuleStatusWatch} {
		in := limitsInputs()
		in.RegimeStage = stage
		in.Names = []NameInput{{Symbol: "AAA", ExposureBaseComplete: true, Legs: []LegInput{limitsLongCall("AAA", 60, 22000)}}}
		if r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly); r.Status != want {
			t.Errorf("22%% under %s = %s, want %s", stage, r.Status, want)
		}
	}
	// The cap acts at 35% in every set by default.
	for _, stage := range []string{RegimeBucketCalm, RegimeBucketEarlyWarning, RegimeBucketConfirmed} {
		in := limitsInputs()
		in.RegimeStage = stage
		in.Names = []NameInput{{Symbol: "AAA", ExposureBaseComplete: true, Legs: []LegInput{limitsLongCall("AAA", 60, 34900), limitsLongCall("AAA", 90, 100)}}}
		if r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly); r.Status != RuleStatusAct || *r.ActThreshold != 35 {
			t.Errorf("35%% under %s = %s (act %v), want act at 35", stage, r.Status, *r.ActThreshold)
		}
	}
}

// Missing inputs never pass: no NLV, a leg without a base value, or a leg
// with neither a price paid nor a value is unknown, even beside a breach.
// No long option at all is not evaluated, and protection alone reads 0%.
func TestPremiumBudgetNeverPassesOnMissingInputs(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)

	in := premiumInputs(RegimeBucketCalm, false)
	in.NLVBase = nil
	if r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly); r.Status != RuleStatusUnknown || r.Observed != nil {
		t.Fatalf("no NLV: %+v", r)
	}

	in = premiumInputs(RegimeBucketConfirmed, false)
	leg := limitsLongCall("CCC", 60, 500)
	leg.MarketValueBaseSource = MarketValueBaseSourceSubstituted
	in.Names = append(in.Names, NameInput{Symbol: "CCC", ExposureBaseComplete: true, Legs: []LegInput{leg}})
	r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly)
	if r.Status != RuleStatusUnknown || r.Reason != "premium_unmeasured" || len(r.Offenders) != 1 || r.Offenders[0].Leg != leg.Desc || r.Threshold != nil {
		t.Fatalf("leg without a base value: %+v", r)
	}

	in = premiumInputs(RegimeBucketCalm, false)
	leg = limitsLongCall("CCC", 60, 0)
	leg.CostBasisBase = nil
	in.Names = append(in.Names, NameInput{Symbol: "CCC", ExposureBaseComplete: true, Legs: []LegInput{leg}})
	if r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly); r.Status != RuleStatusUnknown || r.Reason != "premium_unmeasured" {
		t.Fatalf("no price paid and no value: %+v", r)
	}

	in = limitsInputs()
	short := limitsLongCall("AAA", 30, -2000)
	short.Quantity = -1
	stock := limitsStock("AAA", 20000)
	stock.Legs = []LegInput{short}
	in.Names = []NameInput{stock}
	ev := EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleCashSellOnly)
	if r.Status != RuleStatusNotEvaluated || r.Reason != RuleReasonNoLongOptions || r.SellOnly || ev.SellOnly.Active {
		t.Fatalf("no long option: %+v (sell-only %+v)", r, ev.SellOnly)
	}

	in = limitsInputs()
	in.Names = limitsProtection(30, 20000)
	if r := rowByID(t, EvaluateRulebook(in, pol), RuleCashSellOnly); r.Status != RuleStatusPass || *r.Observed != 0 {
		t.Fatalf("protection alone: %+v, want a pass at 0%%", r)
	}
}

// Sell-only is a result-level fact: rule 3 or rule 15 at watch or act, in
// rulebook order, whatever each rule's alert mode; a rule turned off never
// contributes, and an inactive fact serializes as active false.
func TestSellOnlyIsARulebookResultFact(t *testing.T) {
	pol := DefaultRulebookPolicy() // rule 3 alerts, rule 15 tracks
	in := premiumInputs(RegimeBucketCalm, false)
	in.Names = append(in.Names, limitsStock("DDD", 110000)) // net 105% with the protection put
	ev := EvaluateRulebook(in, pol)
	if !ev.SellOnly.Active || !slices.Equal(ev.SellOnly.Rules, []string{RuleCashSellOnly, RuleNetExposure}) {
		t.Fatalf("both rules at watch: %+v", ev.SellOnly)
	}
	for _, id := range RuleIDs() {
		r := rowByID(t, ev, id)
		if want := id == RuleCashSellOnly || id == RuleNetExposure; r.SellOnly != want {
			t.Fatalf("%s sell_only = %v, want %v", id, r.SellOnly, want)
		}
	}

	pol.Modes[RuleNetExposure] = RuleModeOff
	ev = EvaluateRulebook(in, pol)
	if !slices.Equal(ev.SellOnly.Rules, []string{RuleCashSellOnly}) || rowByID(t, ev, RuleNetExposure).SellOnly {
		t.Fatalf("rule 15 off still contributes: %+v", ev.SellOnly)
	}

	quiet := limitsInputs()
	quiet.Names = []NameInput{limitsStock("AAA", 50000)}
	ev = EvaluateRulebook(quiet, DefaultRulebookPolicy())
	raw, err := json.Marshal(ev.SellOnly)
	if err != nil || ev.SellOnly.Active || string(raw) != `{"active":false}` {
		t.Fatalf("quiet book sell-only = %s (%v)", raw, err)
	}
}

// Rule 15's bands are regime-conditional (amendment 17): calm 100/150, early
// warning 100/130, confirmed 75/100; the row names the set it used.
func TestNetExposureBandsFollowTheRegimeSet(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	for _, c := range []struct {
		name    string
		stage   string
		carried bool
		net     float64
		status  string
		set     string
	}{
		{"calm below watch", RegimeBucketCalm, false, 90000, RuleStatusPass, RegimeBucketCalm},
		{"confirmed watch at 90", RegimeBucketConfirmed, false, 90000, RuleStatusWatch, RegimeBucketConfirmed},
		{"confirmed act at 100", RegimeBucketConfirmed, false, 100000, RuleStatusAct, RegimeBucketConfirmed},
		{"early warning act at 130", RegimeBucketEarlyWarning, false, 130000, RuleStatusAct, RegimeBucketEarlyWarning},
		{"calm watch at 130", RegimeBucketCalm, false, 130000, RuleStatusWatch, RegimeBucketCalm},
		{"carried confirmed keeps the worse", RegimeBucketConfirmed, true, 90000, RuleStatusWatch, RegimeBucketConfirmed},
		{"never seen reads calm", "", false, 90000, RuleStatusPass, RegimeBucketCalm},
	} {
		in := limitsInputs()
		in.RegimeStage, in.RegimeStageCarried = c.stage, c.carried
		in.Names = []NameInput{limitsStock("AAA", c.net)}
		r := rowByID(t, EvaluateRulebook(in, pol), RuleNetExposure)
		if r.Status != c.status || r.RegimeSet != c.set || !strings.Contains(r.Evidence, RegimeSetWords(c.set)+" set's") {
			t.Errorf("%s: %s set %q evidence %q", c.name, r.Status, r.RegimeSet, r.Evidence)
		}
	}
}

// Rule 12 on a gross-long book with no protection-classified position reads
// 0% coverage: below the band, a watch like any under-hedged book, never an
// act (act is the over-hedge tier only). No long book is not evaluated.
func TestHedgeIntegrityWatchesAnUnhedgedLongBook(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	for stage, want := range map[string]string{
		RegimeBucketCalm:         "No index protection is open; the calm band asks for 25–35% of gross long exposure.",
		RegimeBucketEarlyWarning: "No index protection is open; the early-warning band asks for 30–50% of gross long exposure.",
		RegimeBucketConfirmed:    "No index protection is open; the confirmed-stress band asks for 40–70% of gross long exposure.",
	} {
		in := limitsInputs()
		in.RegimeStage = stage
		in.Names = []NameInput{limitsStock("AAA", 80000)}
		r := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity)
		if r.Status != RuleStatusWatch || r.Reason != RuleReasonUnhedged || r.Observed == nil || *r.Observed != 0 || r.Evidence != want || r.RegimeSet != stage {
			t.Errorf("%s: %s/%s observed %v set %q evidence %q", stage, r.Status, r.Reason, r.Observed, r.RegimeSet, r.Evidence)
		}
	}

	// Below-band protection was already a watch; zero protection matches it.
	in := limitsInputs()
	in.Names = limitsProtection(10, 1000)
	if r := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity); r.Status != RuleStatusWatch || r.Reason != "" {
		t.Fatalf("below-band protection = %s/%s, want the same watch", r.Status, r.Reason)
	}

	in = limitsInputs()
	short := limitsStock("AAA", -30000)
	short.StockQuantity = -300
	in.Names = []NameInput{short}
	if r := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity); r.Status != RuleStatusNotEvaluated || r.Reason != RuleReasonNoLongBook {
		t.Fatalf("no long book = %s/%s, want not_evaluated/no_long_book", r.Status, r.Reason)
	}
}

// The governor reads rule 3's levels through PremiumBudgetInForce: the
// latched set when fresh, calm when never seen, the lower of the carried set
// and calm when stale — the worse-of verdict rule 3 keeps.
func TestPremiumBudgetInForceMatchesRule3(t *testing.T) {
	pol := DefaultRulebookPolicy()
	for _, c := range []struct {
		stage      string
		carried    bool
		watch, act float64
	}{
		{"", false, 25, 35},
		{RegimeBucketCalm, false, 25, 35},
		{RegimeBucketEarlyWarning, false, 20, 35},
		{RegimeBucketConfirmed, false, 15, 35},
		{RegimeBucketConfirmed, true, 15, 35},
		{"unrecognized", false, 20, 35},
	} {
		watch, act, set := pol.PremiumBudgetInForce(c.stage, c.carried)
		if watch != c.watch || act != c.act || set == "" {
			t.Errorf("stage %q carried %v: %v/%v (%s), want %v/%v", c.stage, c.carried, watch, act, set, c.watch, c.act)
		}
	}
	// An owner set looser than calm cannot relax a carried stage.
	pol.RegimeConfirmed.PremiumBudgetWatchPct, pol.RegimeConfirmed.PremiumBudgetActPct = 40, 50
	if watch, act, _ := pol.PremiumBudgetInForce(RegimeBucketConfirmed, true); watch != 25 || act != 35 {
		t.Fatalf("carried looser set = %v/%v, want calm's 25/35", watch, act)
	}
}
