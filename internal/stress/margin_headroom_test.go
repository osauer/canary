package stress

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// rule19 builds a measured rule 19 reading with the default bands: watch
// strictly below 30, act strictly below 15.
func rule19(status string, pct float64) *rpc.StressMarginHeadroom {
	return &rpc.StressMarginHeadroom{Status: status, PctNLV: new(pct), WatchPct: new(30.0), ActPct: new(15.0)}
}

// marginBook is a calm, fully current book whose broker cushion reads 7%,
// below every retired stress level (watch 35, act 20, urgent 10), with a
// look-ahead headroom of 5%. The stress read takes its margin verdict from
// the rule 19 reading it is given, never from these figures.
func marginBook(h *rpc.StressMarginHeadroom) StressInput {
	acct := baseStressAccount()
	acct.AsOf = stressTestNow
	acct.Cushion = 0.07
	acct.ExcessLiquidity = 7_000
	acct.LookAheadExcess = 5_000
	return StressInput{Now: stressTestNow, Account: acct, Positions: freshStressPositions(), Regime: healthyStressRegime(), MarginHeadroom: h}
}

// The stress read's margin headroom is rule 19's (amendment 19): its figure,
// its bands and its verdict. A broker cushion the retired levels called
// urgent is a pass when rule 19 passes, and the cushion figure and its trip
// are rule 19's measure and watch band. A measured reading already judges the
// broker's look-ahead figure (R3), so the row does not restate the stress
// read's own; the portfolio still carries it.
func TestStressMarginReadsRule19NotTheBrokerCushion(t *testing.T) {
	t.Parallel()
	res := ComputeStress(marginBook(rule19(risk.RuleStatusPass, 42)))
	if hasSignal(res.Signals, risk.SignalMarginCushionLow) || hasSignal(res.Signals, risk.SignalLookAheadCushionLow) {
		t.Fatalf("raised a margin signal on a rule 19 pass: %+v", res.Signals)
	}
	row := stressRowByTitle(res.Rows, "Immediate margin safety")
	if row == nil || row.Severity != risk.SeverityObserve ||
		row.Evidence != "margin headroom 42.0% NLV (Rulebook watch below 30%, act below 15%)" {
		t.Fatalf("margin row = %+v", row)
	}
	p := res.Portfolio
	if p.CushionPct == nil || *p.CushionPct != 42 || p.CushionTripPct == nil || *p.CushionTripPct != 30 ||
		p.LookAheadCushionPct == nil || *p.LookAheadCushionPct != 5 {
		t.Fatalf("cushion %v trip %v look-ahead %v, want rule 19's 42 and 30 beside the broker's look-ahead 5", p.CushionPct, p.CushionTripPct, p.LookAheadCushionPct)
	}
	if h := p.MarginHeadroom; h == nil || h.Status != risk.RuleStatusPass || *h.WatchPct != 30 || *h.ActPct != 15 {
		t.Fatalf("portfolio.margin_headroom = %+v", h)
	}
}

// Rule 19 at watch is a stress watch and at act a stress act, both
// defensive; an act targets rule 19's watch band. The retired urgent tier has
// no replacement: a margin deficit is an act.
func TestStressMarginTakesRule19sOwnTier(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		h         *rpc.StressMarginHeadroom
		severity  risk.SignalSeverity
		threshold float64
		target    *float64
		guidance  string
	}{
		{name: "watch", h: rule19(risk.RuleStatusWatch, 22), severity: risk.SeverityWatch, threshold: 30, guidance: "falls below the Rulebook's 15% act level"},
		{name: "act", h: rule19(risk.RuleStatusAct, 12), severity: risk.SeverityAct, threshold: 15, target: new(30.0), guidance: "back at the Rulebook's 30% watch level"},
		{name: "deficit", h: rule19(risk.RuleStatusAct, -5), severity: risk.SeverityAct, threshold: 15, target: new(30.0), guidance: "back at the Rulebook's 30% watch level"},
		{name: "owner bands", h: &rpc.StressMarginHeadroom{Status: risk.RuleStatusWatch, PctNLV: new(35.0), WatchPct: new(40.0), ActPct: new(20.0)},
			severity: risk.SeverityWatch, threshold: 40, guidance: "falls below the Rulebook's 20% act level"},
	} {
		res := ComputeStress(marginBook(c.h))
		sig, raised := findSignal(res.Signals, risk.SignalMarginCushionLow)
		if !raised || sig.Severity != c.severity || sig.Direction != risk.DirectionDefensive || sig.Threshold == nil || *sig.Threshold != c.threshold ||
			sig.Observed == nil || *sig.Observed != *c.h.PctNLV || (c.target == nil) != (sig.Target == nil) || c.target != nil && *sig.Target != *c.target {
			t.Errorf("%s: margin_cushion_low = %+v (raised %v)", c.name, sig, raised)
		}
		if hasSignal(res.Signals, risk.SignalLookAheadCushionLow) {
			t.Errorf("%s: raised lookahead_cushion_low; the look-ahead figure is context only", c.name)
		}
		row := stressRowByTitle(res.Rows, "Immediate margin safety")
		if row == nil || row.Severity != c.severity || row.Direction != risk.DirectionDefensive || !strings.Contains(row.Guidance, c.guidance) {
			t.Errorf("%s: margin row = %+v", c.name, row)
		}
	}
}

// Without a rule 19 measurement the margin row is a data-quality watch, never
// a pass, whatever the broker's cushion shows; no margin signal is raised and
// the cushion figure and its trip are absent. Rule 19 turned off is no
// measurement either.
func TestStressMarginWithoutARule19ReadingIsNotAPass(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		h        *rpc.StressMarginHeadroom
		evidence string
	}{
		"absent":       {nil, "margin headroom unavailable (no Rulebook reading was supplied)"},
		"rulebook off": {&rpc.StressMarginHeadroom{Reason: "the Rulebook is turned off"}, "margin headroom unavailable (the Rulebook is turned off)"},
		"no row":       {&rpc.StressMarginHeadroom{Reason: "the Rulebook result has no rule 19 row"}, "margin headroom unavailable (the Rulebook result has no rule 19 row)"},
		"unknown": {&rpc.StressMarginHeadroom{Status: risk.RuleStatusUnknown, RuleReason: risk.RuleReasonExcessLiquidityUnavailable, WatchPct: new(30.0), ActPct: new(15.0)},
			"margin headroom unknown (Rulebook rule 19: excess_liquidity_unavailable)"},
		"rule off": {&rpc.StressMarginHeadroom{Status: risk.RuleStatusNotEvaluated, RuleReason: risk.RuleReasonRuleOff}, "margin headroom not assessed (Rulebook rule 19 is off)"},
	} {
		res := ComputeStress(marginBook(c.h))
		if hasSignal(res.Signals, risk.SignalMarginCushionLow) || hasSignal(res.Signals, risk.SignalLookAheadCushionLow) {
			t.Errorf("%s: raised a margin signal without a rule 19 measurement", name)
		}
		if res.Portfolio.CushionPct != nil || res.Portfolio.CushionTripPct != nil {
			t.Errorf("%s: cushion %v trip %v, want both absent", name, res.Portfolio.CushionPct, res.Portfolio.CushionTripPct)
		}
		row := stressRowByTitle(res.Rows, "Immediate margin safety")
		if row == nil || row.Direction != risk.DirectionDataQuality || row.Severity != risk.SeverityWatch ||
			row.Evidence != c.evidence+"; look-ahead 5.0% NLV (context)" {
			t.Errorf("%s: margin row = %+v, want a data-quality watch", name, row)
		}
	}
}
