package risk

import (
	"regexp"
	"strconv"
	"testing"
)

// limitsInputs is a quiet synthetic book at a round NLV so every percentage in
// the reported-limit contract reads directly off the fixture.
func limitsInputs() RuleInputs {
	now := etDate(2026, 7, 7)
	return RuleInputs{
		AsOf:               now,
		BaseCurrency:       "EUR",
		Positions:          SourceState{Healthy: true},
		Account:            SourceState{Healthy: true},
		NLVBase:            new(100000.0),
		CashBase:           new(90000.0),
		AvailableFundsBase: new(90000.0),
		DailyPnLBase:       new(-100.0),
		SessionOpen:        true,
		SPYDayChangePct:    new(0.1),
		Earnings:           map[string]EarningsInput{},
		RegimeStage:        RegimeBucketCalm,
		RegimeStageAsOf:    now,
		NonBaseNLVBase:     new(0.0),
	}
}

func limitsAllModes(mode string) RulebookPolicy {
	pol := DefaultRulebookPolicy()
	for _, id := range RuleIDs() {
		pol.Modes[id] = mode
	}
	return pol
}

func limitsStock(sym string, exposure float64) NameInput {
	return NameInput{Symbol: sym, ExposureBase: exposure, MarketValueBase: exposure, HasStockLeg: true, ExposureBaseComplete: true}
}

// limitsLongCall is a delta-known long call held on an otherwise empty name.
func limitsLongCall(sym string, dte int, value float64) LegInput {
	return LegInput{Desc: sym + " synthetic C", Right: "C", Strike: 100, Expiry: etDate(2026, 7, 7).AddDate(0, 0, dte), DTE: dte,
		Quantity: 1, Multiplier: 100, Mark: value / 100, Underlying: new(90.0), Delta: new(0.3),
		MarketValueBase: value, ExtrinsicBase: new(value), CostBasisBase: new(value), FXToBase: new(1.0)}
}

// limitsProtection is a long book of 100,000 beside one protection put whose
// short delta is shortPct of it.
func limitsProtection(shortPct, premium float64) []NameInput {
	put := LegInput{Desc: "SPY synthetic P", Right: "P", Strike: 900, Expiry: etDate(2026, 10, 16), DTE: 101,
		Quantity: 1, Multiplier: 100, Mark: premium / 100, Underlying: new(1000.0), Delta: new(-shortPct / 100),
		MarketValueBase: premium, ExtrinsicBase: new(premium), CostBasisBase: new(premium), FXToBase: new(1.0), HedgeListed: true}
	spy := NameInput{Symbol: "SPY", ExposureBase: -shortPct * 1000, MarketValueBase: premium, ExposureBaseComplete: true, Legs: []LegInput{put}}
	return []NameInput{limitsStock("AAA", 100000), spy}
}

type limitCase struct {
	name   string
	rule   string
	mutate func(*RuleInputs)
	status string
	// watch and act are the two bands of a two-band rule; limit is the one
	// limit of a single-limit rule. All nil means the row reports no limit.
	watch, act, limit *float64
}

// want is the band the row must report for its status: act reports the act
// band, every other status the watch band, and a single-limit rule its limit.
func (c limitCase) want() *float64 {
	switch {
	case c.status == RuleStatusAct && c.act != nil:
		return c.act
	case c.watch != nil:
		return c.watch
	default:
		return c.limit
	}
}

// limitCases drive every reachable status of every rule with synthetic
// inputs under the default policy (all rules evaluated).
func limitCases() []limitCase {
	f := func(v float64) *float64 { return &v }
	name := func(ns ...NameInput) func(*RuleInputs) {
		return func(in *RuleInputs) { in.Names = append(in.Names, ns...) }
	}
	optionName := func(sym string, legs ...LegInput) NameInput {
		return NameInput{Symbol: sym, ExposureBaseComplete: true, Legs: legs}
	}
	known := func(sessions int) EarningsInput {
		return EarningsInput{Known: true, Date: etDate(2026, 7, 7).AddDate(0, 0, sessions+1), TimeOfDay: "amc", SessionsUntil: new(sessions), Source: "fetched"}
	}
	positionsDown := func(in *RuleInputs) { in.Positions = SourceState{Healthy: false, Reason: "positions_pending"} }
	return []limitCase{
		// 1 — exposure to one underlying: watch 30, act 40.
		{name: "pass", rule: RuleSingleNameExposure, mutate: name(limitsStock("AAA", 20000)), status: RuleStatusPass, watch: f(30), act: f(40)},
		{name: "watch", rule: RuleSingleNameExposure, mutate: name(limitsStock("AAA", 35000)), status: RuleStatusWatch, watch: f(30), act: f(40)},
		{name: "act", rule: RuleSingleNameExposure, mutate: name(limitsStock("AAA", 45000)), status: RuleStatusAct, watch: f(30), act: f(40)},
		{name: "unknown", rule: RuleSingleNameExposure, mutate: name(NameInput{Symbol: "AAA", ExposureBase: 1000, HasStockLeg: true}), status: RuleStatusUnknown, watch: f(30), act: f(40)},
		{name: "gate", rule: RuleSingleNameExposure, mutate: positionsDown, status: RuleStatusUnknown},

		// 2 — premium at risk in one option position: watch 5, act 10;
		// protection tier 15/25 when it drives the verdict.
		{name: "pass", rule: RuleOptionLinePremium, status: RuleStatusPass, watch: f(5), act: f(10)},
		{name: "watch", rule: RuleOptionLinePremium, mutate: name(optionName("AAA", limitsLongCall("AAA", 60, 7000))), status: RuleStatusWatch, watch: f(5), act: f(10)},
		{name: "act", rule: RuleOptionLinePremium, mutate: name(optionName("AAA", limitsLongCall("AAA", 60, 12000))), status: RuleStatusAct, watch: f(5), act: f(10)},
		{name: "protection watch", rule: RuleOptionLinePremium, mutate: name(limitsProtection(30, 18000)...), status: RuleStatusWatch, watch: f(15), act: f(25)},
		{name: "protection act", rule: RuleOptionLinePremium, mutate: name(limitsProtection(30, 26000)...), status: RuleStatusAct, watch: f(15), act: f(25)},
		{name: "unknown", rule: RuleOptionLinePremium, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 1000)
			leg.MarketValueBaseSource = MarketValueBaseSourceSubstituted
			in.Names = append(in.Names, optionName("AAA", leg))
		}, status: RuleStatusUnknown, watch: f(5), act: f(10)},

		// 3 — cash reserve: one limit, 75.
		{name: "pass", rule: RuleCashSellOnly, status: RuleStatusPass, limit: f(75)},
		{name: "watch", rule: RuleCashSellOnly, mutate: func(in *RuleInputs) { in.AvailableFundsBase = new(50000.0) }, status: RuleStatusWatch, limit: f(75)},
		{name: "unknown", rule: RuleCashSellOnly, mutate: func(in *RuleInputs) { in.AvailableFundsBase = nil }, status: RuleStatusUnknown},

		// 4 — option time value: calm 10/15, early warning 7.5/12.
		{name: "pass", rule: RuleExtrinsicBudget, status: RuleStatusPass, watch: f(10), act: f(15)},
		{name: "watch", rule: RuleExtrinsicBudget, mutate: name(optionName("AAA", limitsLongCall("AAA", 60, 11000))), status: RuleStatusWatch, watch: f(10), act: f(15)},
		{name: "act", rule: RuleExtrinsicBudget, mutate: name(optionName("AAA", limitsLongCall("AAA", 60, 16000))), status: RuleStatusAct, watch: f(10), act: f(15)},
		{name: "early warning watch", rule: RuleExtrinsicBudget, mutate: func(in *RuleInputs) {
			in.RegimeStage = RegimeBucketEarlyWarning
			in.Names = append(in.Names, optionName("AAA", limitsLongCall("AAA", 60, 8000)))
		}, status: RuleStatusWatch, watch: f(7.5), act: f(12)},
		// An uncomputable time value stops before a regime set is chosen, so
		// the row names the gap and reports no limit.
		{name: "unknown", rule: RuleExtrinsicBudget, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 5000)
			leg.ExtrinsicBase = nil
			in.Names = append(in.Names, optionName("AAA", leg))
		}, status: RuleStatusUnknown},

		// 5 — options nearing expiry: watch inside 14 DTE, act inside 7.
		{name: "pass", rule: RuleExpiryRunway, status: RuleStatusPass, watch: f(14), act: f(7)},
		{name: "watch", rule: RuleExpiryRunway, mutate: name(optionName("AAA", limitsLongCall("AAA", 10, 1000))), status: RuleStatusWatch, watch: f(14), act: f(7)},
		{name: "act", rule: RuleExpiryRunway, mutate: name(optionName("AAA", limitsLongCall("AAA", 10, 1000), limitsLongCall("AAA", 3, 500))), status: RuleStatusAct, watch: f(14), act: f(7)},
		{name: "gate", rule: RuleExpiryRunway, mutate: positionsDown, status: RuleStatusUnknown},

		// 6 — earnings timing: no numeric limit.
		{name: "pass", rule: RuleCatalystCoverage, status: RuleStatusPass},
		{name: "watch", rule: RuleCatalystCoverage, mutate: func(in *RuleInputs) {
			in.Names = append(in.Names, optionName("AAA", limitsLongCall("AAA", 10, 1000)))
			in.Earnings["AAA"] = known(12)
		}, status: RuleStatusWatch},
		{name: "unknown", rule: RuleCatalystCoverage, mutate: name(optionName("AAA", limitsLongCall("AAA", 10, 1000))), status: RuleStatusUnknown},

		// 7 — short options through earnings: no single numeric limit.
		{name: "pass", rule: RuleOverwriteEarnings, status: RuleStatusPass},
		{name: "act", rule: RuleOverwriteEarnings, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 30, -1000)
			leg.Quantity = -1
			in.Names = append(in.Names, optionName("AAA", leg))
			in.Earnings["AAA"] = known(5)
		}, status: RuleStatusAct},
		{name: "unknown", rule: RuleOverwriteEarnings, mutate: name(limitsStock("AAA", 1000)), status: RuleStatusUnknown},

		// 8 — position size near earnings: one limit, 3 sessions.
		{name: "pass", rule: RuleEarningsSizeFreeze, status: RuleStatusPass, limit: f(3)},
		{name: "act", rule: RuleEarningsSizeFreeze, mutate: func(in *RuleInputs) {
			in.Names = append(in.Names, limitsStock("AAA", 35000))
			in.Earnings["AAA"] = known(2)
		}, status: RuleStatusAct, limit: f(3)},
		{name: "unknown", rule: RuleEarningsSizeFreeze, mutate: name(limitsStock("AAA", 35000)), status: RuleStatusUnknown, limit: f(3)},

		// 9 — holding falls while the market rises: one limit, -1.5.
		{name: "tape not green", rule: RuleRedOnGreen, status: RuleStatusPass, limit: f(-1.5)},
		{name: "pass", rule: RuleRedOnGreen, mutate: func(in *RuleInputs) { in.SPYDayChangePct = new(1.0) }, status: RuleStatusPass, limit: f(-1.5)},
		{name: "watch", rule: RuleRedOnGreen, mutate: func(in *RuleInputs) {
			in.SPYDayChangePct = new(1.0)
			n := limitsStock("AAA", 1000)
			n.StockDayChangePct = new(-2.0)
			in.Names = append(in.Names, n)
		}, status: RuleStatusWatch, limit: f(-1.5)},
		{name: "unknown", rule: RuleRedOnGreen, mutate: func(in *RuleInputs) { in.SPYDayChangePct = nil }, status: RuleStatusUnknown},
		{name: "off session", rule: RuleRedOnGreen, mutate: func(in *RuleInputs) { in.SessionOpen = false }, status: RuleStatusNotEvaluated},

		// 10 — large winner today: one limit, 4.
		{name: "pass", rule: RuleWinnerTrim, status: RuleStatusPass, limit: f(4)},
		{name: "watch", rule: RuleWinnerTrim, mutate: func(in *RuleInputs) {
			n := limitsStock("AAA", 20000)
			n.StockDayChangePct = new(5.0)
			in.Names = append(in.Names, n)
		}, status: RuleStatusWatch, limit: f(4)},
		{name: "unknown", rule: RuleWinnerTrim, mutate: func(in *RuleInputs) {
			n := limitsStock("AAA", 20000)
			n.ExposureBaseComplete = false
			n.StockDayChangePct = new(5.0)
			in.Names = append(in.Names, n)
		}, status: RuleStatusUnknown, limit: f(4)},
		{name: "off session", rule: RuleWinnerTrim, mutate: func(in *RuleInputs) { in.SessionOpen = false }, status: RuleStatusNotEvaluated},

		// 11 — green-day nudge: informational, no limit.
		{name: "pass", rule: RuleGreenDayAction, status: RuleStatusPass},
		{name: "info", rule: RuleGreenDayAction, mutate: func(in *RuleInputs) {
			in.DailyPnLBase = new(100.0)
			in.Names = append(in.Names, limitsStock("AAA", 45000))
		}, status: RuleStatusInfo},

		// 12 — index protection size: calm range 25–35, act above 2×35.
		{name: "pass", rule: RuleHedgeIntegrity, mutate: name(limitsProtection(30, 1000)...), status: RuleStatusPass, watch: f(25), act: f(70)},
		{name: "watch below", rule: RuleHedgeIntegrity, mutate: name(limitsProtection(20, 1000)...), status: RuleStatusWatch, watch: f(25), act: f(70)},
		{name: "watch above", rule: RuleHedgeIntegrity, mutate: name(limitsProtection(40, 1000)...), status: RuleStatusWatch, watch: f(35), act: f(70)},
		{name: "act", rule: RuleHedgeIntegrity, mutate: name(limitsProtection(80, 1000)...), status: RuleStatusAct, watch: f(35), act: f(70)},
		{name: "no protection", rule: RuleHedgeIntegrity, mutate: name(limitsStock("AAA", 1000)), status: RuleStatusNotEvaluated},

		// 13 — long option loss limit: watch 40, act 60.
		{name: "pass", rule: RuleExitDiscipline, status: RuleStatusPass, watch: f(40), act: f(60)},
		{name: "watch", rule: RuleExitDiscipline, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 5000)
			leg.CostBasisBase = new(10000.0)
			in.Names = append(in.Names, optionName("AAA", leg))
		}, status: RuleStatusWatch, watch: f(40), act: f(60)},
		{name: "act", rule: RuleExitDiscipline, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 3000)
			leg.CostBasisBase = new(10000.0)
			in.Names = append(in.Names, optionName("AAA", leg))
		}, status: RuleStatusAct, watch: f(40), act: f(60)},
		{name: "unknown", rule: RuleExitDiscipline, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 5000)
			leg.CostBasisBase = nil
			in.Names = append(in.Names, optionName("AAA", leg))
		}, status: RuleStatusUnknown, watch: f(40), act: f(60)},

		// 14 — foreign-currency exposure: one limit, 60.
		{name: "pass", rule: RuleFXExposure, status: RuleStatusPass, limit: f(60)},
		{name: "watch", rule: RuleFXExposure, mutate: func(in *RuleInputs) { in.NonBaseNLVBase = new(70000.0) }, status: RuleStatusWatch, limit: f(60)},
		{name: "unknown", rule: RuleFXExposure, mutate: func(in *RuleInputs) { in.NonBaseNLVBase = nil }, status: RuleStatusUnknown},

		// 15 — net market exposure: watch 100, act 150.
		{name: "flat", rule: RuleNetExposure, status: RuleStatusPass, watch: f(100), act: f(150)},
		{name: "pass", rule: RuleNetExposure, mutate: name(limitsStock("AAA", 50000)), status: RuleStatusPass, watch: f(100), act: f(150)},
		{name: "watch", rule: RuleNetExposure, mutate: name(limitsStock("AAA", 120000)), status: RuleStatusWatch, watch: f(100), act: f(150)},
		{name: "act", rule: RuleNetExposure, mutate: name(limitsStock("AAA", 160000)), status: RuleStatusAct, watch: f(100), act: f(150)},
		{name: "unknown", rule: RuleNetExposure, mutate: func(in *RuleInputs) {
			leg := limitsLongCall("AAA", 60, 1000)
			leg.Delta, leg.Underlying = nil, nil
			n := optionName("AAA", leg)
			n.GreeksGapNotionalBase = 9000
			in.Names = append(in.Names, n)
		}, status: RuleStatusUnknown, watch: f(100), act: f(150)},
	}
}

// quotesLimit reports whether evidence states the limit as a standalone
// number, so 5 is not found inside 15 or 7 inside 7.5.
func quotesLimit(evidence string, limit float64) bool {
	text := regexp.QuoteMeta(strconv.FormatFloat(limit, 'f', -1, 64))
	return regexp.MustCompile(`(^|[^0-9.])` + text + `([^0-9.]|\.([^0-9]|$)|$)`).MatchString(evidence)
}

// TestRuleRowReportsTheLimitOfItsStatus is the reported-limit contract (owner
// decisions 2026-09-26): a row's Threshold is the band its status sits in, and
// its evidence line quotes that same number. Unknown and not-evaluated rows
// name what is missing; they may omit the limit but never quote another band.
func TestRuleRowReportsTheLimitOfItsStatus(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	covered := map[string]map[string]bool{}
	for _, c := range limitCases() {
		t.Run(c.rule+"/"+c.name, func(t *testing.T) {
			in := limitsInputs()
			if c.mutate != nil {
				c.mutate(&in)
			}
			r := rowByID(t, EvaluateRulebook(in, pol), c.rule)
			if r.Status != c.status {
				t.Fatalf("status = %s, want %s — the fixture must drive the status under test (evidence: %s)", r.Status, c.status, r.Evidence)
			}
			for _, band := range []struct {
				name      string
				got, want *float64
			}{{"watch_threshold", r.WatchThreshold, c.watch}, {"act_threshold", r.ActThreshold, c.act}} {
				switch {
				case band.want == nil && band.got != nil:
					t.Fatalf("%s = %v, want none", band.name, *band.got)
				case band.want != nil && (band.got == nil || *band.got != *band.want):
					t.Fatalf("%s = %v, want %v", band.name, band.got, *band.want)
				}
			}
			want := c.want()
			switch {
			case want == nil && r.Threshold != nil:
				t.Fatalf("threshold = %v, want none", *r.Threshold)
			case want != nil && r.Threshold == nil:
				t.Fatalf("threshold missing, want %v", *want)
			case want != nil && *r.Threshold != *want:
				t.Fatalf("%s row reports threshold %v, want the %s band %v (evidence: %s)", r.Status, *r.Threshold, c.status, *want, r.Evidence)
			}
			if want == nil {
				return
			}
			switch r.Status {
			case RuleStatusPass, RuleStatusWatch, RuleStatusAct:
				if !quotesLimit(r.Evidence, *want) {
					t.Fatalf("evidence does not quote the reported threshold %v: %q", *want, r.Evidence)
				}
			default:
				for _, other := range []*float64{c.watch, c.act} {
					if other != nil && *other != *want && quotesLimit(r.Evidence, *other) && !quotesLimit(r.Evidence, *want) {
						t.Fatalf("evidence quotes band %v, not the reported threshold %v: %q", *other, *want, r.Evidence)
					}
				}
			}
		})
		if covered[c.rule] == nil {
			covered[c.rule] = map[string]bool{}
		}
		covered[c.rule][c.status] = true
	}
	for _, id := range RuleIDs() {
		if len(covered[id]) == 0 {
			t.Errorf("rule %s has no reported-limit case", id)
		}
	}
}

// TestRuleRowOffModeReportsNoLimit keeps the off-mode contract: a rule the
// policy turned off was never compared, so it carries no limit of any kind.
func TestRuleRowOffModeReportsNoLimit(t *testing.T) {
	pol := limitsAllModes(RuleModeOff)
	for _, c := range limitCases() {
		in := limitsInputs()
		if c.mutate != nil {
			c.mutate(&in)
		}
		r := rowByID(t, EvaluateRulebook(in, pol), c.rule)
		if r.Status != RuleStatusNotEvaluated || r.Threshold != nil || r.WatchThreshold != nil || r.ActThreshold != nil {
			t.Fatalf("%s/%s off: status %s threshold %v watch %v act %v, want not_evaluated with no limit", c.rule, c.name, r.Status, r.Threshold, r.WatchThreshold, r.ActThreshold)
		}
	}
}

// TestTwoBandRulesClassifyAtOrAbove pins the single comparison (owner
// decision 2026-09-26): a reading exactly at a band's limit is in that band.
// Rules 1, 2, 4 and 15 moved from watch to act at exact equality; rule 13
// already classified this way.
func TestTwoBandRulesClassifyAtOrAbove(t *testing.T) {
	pol := limitsAllModes(RuleModeAlert)
	optionName := func(legs ...LegInput) NameInput {
		return NameInput{Symbol: "AAA", ExposureBaseComplete: true, Legs: legs}
	}
	lossLeg := func(value float64) LegInput {
		leg := limitsLongCall("AAA", 60, value)
		leg.CostBasisBase = new(10000.0)
		return leg
	}
	for _, c := range []struct {
		name   string
		rule   string
		names  []NameInput
		status string
	}{
		{"single name at watch", RuleSingleNameExposure, []NameInput{limitsStock("AAA", 30000)}, RuleStatusWatch},
		{"single name at act", RuleSingleNameExposure, []NameInput{limitsStock("AAA", 40000)}, RuleStatusAct},
		{"option line at watch", RuleOptionLinePremium, []NameInput{optionName(limitsLongCall("AAA", 60, 5000))}, RuleStatusWatch},
		{"option line at act", RuleOptionLinePremium, []NameInput{optionName(limitsLongCall("AAA", 60, 10000))}, RuleStatusAct},
		{"protection premium at act", RuleOptionLinePremium, limitsProtection(30, 25000), RuleStatusAct},
		{"time value at watch", RuleExtrinsicBudget, []NameInput{optionName(limitsLongCall("AAA", 60, 10000))}, RuleStatusWatch},
		{"time value at act", RuleExtrinsicBudget, []NameInput{optionName(limitsLongCall("AAA", 60, 15000))}, RuleStatusAct},
		{"loss at watch", RuleExitDiscipline, []NameInput{optionName(lossLeg(6000))}, RuleStatusWatch},
		{"loss at act", RuleExitDiscipline, []NameInput{optionName(lossLeg(4000))}, RuleStatusAct},
		{"net exposure at watch", RuleNetExposure, []NameInput{limitsStock("AAA", 100000)}, RuleStatusWatch},
		{"net exposure at act", RuleNetExposure, []NameInput{limitsStock("AAA", 150000)}, RuleStatusAct},
	} {
		in := limitsInputs()
		in.Names = c.names
		r := rowByID(t, EvaluateRulebook(in, pol), c.rule)
		if r.Status != c.status {
			t.Errorf("%s: status = %s, want %s (evidence: %s)", c.name, r.Status, c.status, r.Evidence)
		}
		if r.Threshold == nil || r.Observed == nil || *r.Observed != *r.Threshold {
			t.Errorf("%s: observed %v, threshold %v — the fixture must sit exactly on the reported limit", c.name, r.Observed, r.Threshold)
		}
	}
}
