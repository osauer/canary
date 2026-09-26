package rpc

import (
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
)

// The stress read's net exposure is a projection of Rulebook rule 15
// (amendment 16): its status, measure, lower-bound flag and bands, the side
// it flags, and why a reading is missing.
func TestStressNetExposureFromRulesProjectsRule15(t *testing.T) {
	watch := RulesResult{Enabled: true, Rules: []risk.RuleRow{
		{ID: risk.RuleSingleNameExposure, Status: risk.RuleStatusPass},
		{ID: risk.RuleNetExposure, Status: risk.RuleStatusWatch, Observed: new(121.5), ObservedIsLowerBound: true,
			WatchThreshold: new(100.0), ActThreshold: new(150.0),
			Offenders: []risk.RuleOffender{{Symbol: "BBB", Note: "delta missing"}, {Symbol: "AAA", Observed: -80}}},
	}}
	got := StressNetExposureFromRules(&watch)
	if got.Reason != "" || got.Status != risk.RuleStatusWatch || got.PctNLV == nil || *got.PctNLV != 121.5 || !got.IsLowerBound ||
		*got.WatchPct != 100 || *got.ActPct != 150 || got.Direction != "short" || got.RuleReason != "" {
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
