package risk

import (
	"math"
	"slices"
	"strings"
	"testing"
)

func TestRuleIDsMatchEvaluationAndBaselineModes(t *testing.T) {
	ev := EvaluateRulebook(healthyInputs(), DefaultRulebookPolicy())
	var got []string
	for _, r := range ev.Rows {
		got = append(got, r.ID)
	}
	if !slices.Equal(got, RuleIDs()) || len(got) != 18 {
		t.Fatalf("rows %v, want %v", got, RuleIDs())
	}
	for _, id := range RuleIDs() {
		if _, ok := DefaultRulebookPolicy().Modes[id]; !ok {
			t.Fatalf("baseline has no mode for %s: a new rule would silently alert", id)
		}
	}
	if err := DefaultRulebookPolicy().Validate(); err != nil {
		t.Fatalf("baseline invalid: %v", err)
	}
}

// Net exposure counts every name with its sign, hedges included, so a large
// index put offsets long single-name exposure, which premium and cash do not
// show.
func TestNetExposureMeasuresTheWholeBookWithHedges(t *testing.T) {
	in := healthyInputs() // NLV 245,000: NOW +380,000, BB +45,000, MSFT +30,000, SPY −80,000
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleNetExposure)
	if row.Status != RuleStatusAct || row.Observed == nil || math.Abs(*row.Observed-153.1) > 0.05 || row.ObservedIsLowerBound ||
		!strings.Contains(row.Evidence, "net long") || row.Mode != RuleModeTrack || row.Number != 15 {
		t.Fatalf("net exposure row = %+v", row)
	}
	for _, o := range row.Offenders {
		if o.Symbol == "SPY" {
			t.Fatalf("the hedge was listed as a long contributor: %+v", row.Offenders)
		}
	}
	if len(row.Notes) == 0 || !strings.Contains(row.Notes[0], "gross long 185.7%") || !strings.Contains(row.Notes[0], "gross short 32.7%") {
		t.Fatalf("gross split missing: %v", row.Notes)
	}
	in.Names[3].ExposureBase = -300000
	if row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleNetExposure); row.Status != RuleStatusPass {
		t.Fatalf("a larger hedge did not bring the net under watch: %+v", row)
	}
}

// A fully invested stock book is exactly unlevered: it sits at the watch
// level, and nothing about it acts.
func TestNetExposureStockOnlyBookSitsAtWatch(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{Symbol: "AAA", ExposureBase: 245000, MarketValueBase: 245000, HasStockLeg: true, ExposureBaseComplete: true}}
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleNetExposure)
	if row.Status != RuleStatusWatch || *row.Observed != 100 {
		t.Fatalf("stock-only book = %+v", row)
	}
}

// Missing delta may indict, never acquit: a provable excess still acts, and
// an interval straddling the limit is unknown, not pass.
func TestNetExposureWithMissingDeltaIndictsNeverAcquits(t *testing.T) {
	in := healthyInputs()
	gapped := NameInput{Symbol: "GAP", ExposureBaseComplete: true, GreeksGapNotionalBase: 50000,
		Legs: []LegInput{{Desc: "GAP C", Right: "C", Strike: 50, Quantity: 10, Multiplier: 100, Underlying: new(60.0), FXToBase: new(1.0)}}}
	in.Names = []NameInput{{Symbol: "AAA", ExposureBase: 400000, ExposureBaseComplete: true, HasStockLeg: true}, gapped}
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleNetExposure)
	if row.Status != RuleStatusAct || !row.ObservedIsLowerBound || !strings.Contains(row.Evidence, "at least") {
		t.Fatalf("provable excess not reported as a lower bound: %+v", row)
	}
	gapped.Legs[0].Right = "P"
	in.Names = []NameInput{{Symbol: "AAA", ExposureBase: 200000, ExposureBaseComplete: true, HasStockLeg: true}, gapped}
	row = rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleNetExposure)
	if row.Status != RuleStatusUnknown || row.Observed != nil {
		t.Fatalf("an unmeasured put acquitted the book: %+v", row)
	}
}

// A losing option line counts at the price paid: its fall in value must not
// free room under the per-position limit to buy more of it.
func TestOptionLinePremiumCountsALosingLineAtThePricePaid(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{Symbol: "LOSS", ExposureBase: 20000, ExposureBaseComplete: true,
		Legs: []LegInput{{Desc: "LOSS 20261120 C 100", Right: "C", Strike: 100, Quantity: 10, Multiplier: 100,
			MarketValueBase: 10000, CostBasisBase: new(30000.0), Underlying: new(90.0), Delta: new(0.2), FXToBase: new(1.0)}}}}
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleOptionLinePremium)
	if row.Status != RuleStatusAct || row.Observed == nil || math.Abs(*row.Observed-12.2) > 0.05 ||
		len(row.Offenders) == 0 || !strings.Contains(row.Offenders[0].Note, "price paid") {
		t.Fatalf("losing line measured at its value: %+v", row)
	}
	in.Names[0].Legs[0].CostBasisBase = new(5000.0)
	if row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleOptionLinePremium); row.Status != RuleStatusPass || *row.Observed != 4.1 {
		t.Fatalf("a gaining line is measured at its value: %+v", row)
	}
}

func TestRulebookPolicyValidateRejectsUnusableLimits(t *testing.T) {
	for name, mutate := range map[string]func(*RulebookPolicy){
		"watch above act":     func(p *RulebookPolicy) { p.NetExposureWatchPct = 200 },
		"negative reserve":    func(p *RulebookPolicy) { p.CashReserveMinPct = -1 },
		"reserve above 100":   func(p *RulebookPolicy) { p.CashReserveMinPct = 101 },
		"NaN":                 func(p *RulebookPolicy) { p.SingleNameActPct = math.NaN() },
		"unknown mode":        func(p *RulebookPolicy) { p.Modes[RuleNetExposure] = "loud" },
		"unknown rule":        func(p *RulebookPolicy) { p.Modes["no_such_rule"] = RuleModeAlert },
		"runway act > watch":  func(p *RulebookPolicy) { p.RunwayActDTE = 30 },
		"empty hedge symbol":  func(p *RulebookPolicy) { p.HedgeSymbols = append(p.HedgeSymbols, " ") },
		"no identity":         func(p *RulebookPolicy) { p.ID = "" },
		"wrong kind":          func(p *RulebookPolicy) { p.Kind = "ibkr.protection_policy" },
		"regime band min>max": func(p *RulebookPolicy) { p.RegimeCalm.HedgeBandMinPct = 50 },
		"overhedge below 1":   func(p *RulebookPolicy) { p.OverhedgeMultiple = 0.5 },
		"overhedge NaN":       func(p *RulebookPolicy) { p.OverhedgeMultiple = math.NaN() },
	} {
		p := DefaultRulebookPolicy()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestRulebookFingerprintCoversNetExposureLimits(t *testing.T) {
	base := DefaultRulebookPolicy()
	changed := DefaultRulebookPolicy()
	changed.NetExposureActPct = 175
	if base.FingerprintKey() == changed.FingerprintKey() {
		t.Fatal("a net exposure limit change left the fingerprint unchanged")
	}
	changed = DefaultRulebookPolicy()
	changed.OverhedgeMultiple = 1.5
	if base.FingerprintKey() == changed.FingerprintKey() {
		t.Fatal("an over-hedge multiple change left the fingerprint unchanged")
	}
}

// The over-hedge multiple is the owner's: it moves the level where rule 12
// acts and the level where index puts stop counting as protection.
func TestOverhedgeMultipleMovesRule12AndTheProtectionBoundary(t *testing.T) {
	in := healthyInputs() // gross long 455,000; one SPY put line of 40 contracts on 752
	in.RegimeStage, in.RegimeStageAsOf = RegimeBucketCalm, in.AsOf
	in.Names[3].Legs[0].Delta = new(-0.10) // about 60–66% of gross long against the calm 25–35% band
	pol := DefaultRulebookPolicy()
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity); row.Status != RuleStatusWatch {
		t.Fatalf("default multiple: %s (%s)", row.Status, row.Evidence)
	}
	pol.OverhedgeMultiple = 1.5
	if row := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity); row.Status != RuleStatusAct {
		t.Fatalf("multiple 1.5 left an over-hedge at %s (%s)", row.Status, row.Evidence)
	}

	in.Names[3].Legs[0].Delta = new(-0.15) // 99% of gross long
	if role := classifyIndexPutRoles(in, DefaultRulebookPolicy()).Names[3].Legs[0].IndexPutRole; role != IndexPutRoleProtection {
		t.Fatalf("default multiple: role %s", role)
	}
	pol.OverhedgeMultiple = 1.4 // boundary 98% of gross long
	if role := classifyIndexPutRoles(in, pol).Names[3].Legs[0].IndexPutRole; role != IndexPutRoleDirectional {
		t.Fatalf("multiple 1.4 still treats a put at 99%% of gross long as protection: %s", role)
	}
}
