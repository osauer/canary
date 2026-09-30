package rpc

import (
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
)

// The stress read's net exposure is a projection of Rulebook rule 15
// (amendments 16 and 17): its status, measure, lower-bound flag, bands and
// the regime set behind them, the side it flags, and why a reading is
// missing.
func TestStressNetExposureFromRulesProjectsRule15(t *testing.T) {
	watch := RulesResult{Enabled: true, Rules: []risk.RuleRow{
		{ID: risk.RuleSingleNameExposure, Status: risk.RuleStatusPass},
		{ID: risk.RuleNetExposure, Status: risk.RuleStatusWatch, Observed: new(121.5), ObservedIsLowerBound: true,
			WatchThreshold: new(100.0), ActThreshold: new(150.0), RegimeSet: risk.RegimeBucketEarlyWarning,
			Offenders: []risk.RuleOffender{{Symbol: "BBB", Note: "delta missing"}, {Symbol: "AAA", Observed: -80}}},
	}}
	got := StressNetExposureFromRules(&watch)
	if got.Reason != "" || got.Status != risk.RuleStatusWatch || got.PctNLV == nil || *got.PctNLV != 121.5 || !got.IsLowerBound ||
		*got.WatchPct != 100 || *got.ActPct != 150 || got.Direction != "short" || got.RuleReason != "" || got.RegimeSet != risk.RegimeBucketEarlyWarning {
		t.Fatalf("watch projection = %+v", got)
	}
	// The projection is a copy: the Rulebook result stays untouched.
	*got.PctNLV = 0
	if *watch.Rules[1].Observed != 121.5 {
		t.Fatal("projection aliases the Rulebook row")
	}

	unknown := RulesResult{Enabled: true, Rules: []risk.RuleRow{{ID: risk.RuleNetExposure, Status: risk.RuleStatusUnknown, Reason: "greeks_gap",
		WatchThreshold: new(100.0), ActThreshold: new(150.0)}}}
	if got := StressNetExposureFromRules(&unknown); got.Status != risk.RuleStatusUnknown || got.RuleReason != "greeks_gap" || got.PctNLV != nil || got.Direction != "" {
		t.Fatalf("unknown projection = %+v", got)
	}
	for name, res := range map[string]*RulesResult{
		"nil":      nil,
		"disabled": {Enabled: false},
		"no row":   {Enabled: true, Rules: []risk.RuleRow{{ID: risk.RuleSingleNameExposure, Status: risk.RuleStatusPass}}},
	} {
		if got := StressNetExposureFromRules(res); got.Reason == "" || got.Status != "" {
			t.Errorf("%s: projection = %+v, want unavailable with a reason", name, got)
		}
	}
}

// The stress read's margin headroom is a projection of Rulebook rule 19
// (amendment 19): its status, measure and bands, and why a reading is
// missing.
func TestStressMarginHeadroomFromRulesProjectsRule19(t *testing.T) {
	act := RulesResult{Enabled: true, Rules: []risk.RuleRow{
		{ID: risk.RuleNetExposure, Status: risk.RuleStatusPass},
		{ID: risk.RuleMarginHeadroom, Status: risk.RuleStatusAct, Observed: new(12.5), Threshold: new(15.0), WatchThreshold: new(30.0), ActThreshold: new(15.0)},
	}}
	got := StressMarginHeadroomFromRules(&act)
	if got.Reason != "" || got.Status != risk.RuleStatusAct || got.PctNLV == nil || *got.PctNLV != 12.5 || *got.WatchPct != 30 || *got.ActPct != 15 || got.RuleReason != "" {
		t.Fatalf("act projection = %+v", got)
	}
	// The projection is a copy: the Rulebook result stays untouched.
	*got.PctNLV = 0
	if *act.Rules[1].Observed != 12.5 {
		t.Fatal("projection aliases the Rulebook row")
	}

	for status, reason := range map[string]string{risk.RuleStatusUnknown: risk.RuleReasonExcessLiquidityUnavailable, risk.RuleStatusNotEvaluated: risk.RuleReasonRuleOff} {
		res := RulesResult{Enabled: true, Rules: []risk.RuleRow{{ID: risk.RuleMarginHeadroom, Status: status, Reason: reason}}}
		if got := StressMarginHeadroomFromRules(&res); got.Status != status || got.RuleReason != reason || got.PctNLV != nil || got.Reason != "" {
			t.Fatalf("%s projection = %+v", status, got)
		}
	}
	for name, res := range map[string]*RulesResult{
		"nil":      nil,
		"disabled": {Enabled: false},
		"no row":   {Enabled: true, Rules: []risk.RuleRow{{ID: risk.RuleNetExposure, Status: risk.RuleStatusPass}}},
	} {
		if got := StressMarginHeadroomFromRules(res); got.Reason == "" || got.Status != "" {
			t.Errorf("%s: projection = %+v, want unavailable with a reason", name, got)
		}
	}
}
