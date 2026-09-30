package cli

import (
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The headline sets the observed value beside the limit the daemon reported
// for the row's status and, for a two-band rule, names both bands; it never
// chooses a band itself.
func TestRuleHeadlineShowsTheReportedLimitAndBothBands(t *testing.T) {
	for _, c := range []struct {
		name string
		row  risk.RuleRow
		want string
	}{
		{"act row", risk.RuleRow{Title: "Long option loss limit", Unit: "% premium lost", Status: risk.RuleStatusAct,
			Observed: new(70.0), Threshold: new(60.0), WatchThreshold: new(40.0), ActThreshold: new(60.0)},
			"Long option loss limit (observed 70.0 vs 60 % premium lost; watch 40, act 60)"},
		{"fractional band", risk.RuleRow{Title: "Option time value at risk", Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(8.0), Threshold: new(7.5), WatchThreshold: new(7.5), ActThreshold: new(12.0)},
			"Option time value at risk (observed 8.0 vs 7.5 % NLV; watch 7.5, act 12)"},
		{"single limit", risk.RuleRow{Title: "Foreign-currency exposure", Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(70.0), Threshold: new(60.0)},
			"Foreign-currency exposure (observed 70.0 vs 60.0 % NLV)"},
		{"unmeasured", risk.RuleRow{Title: "Premium budget", Status: risk.RuleStatusUnknown, Reason: "premium_unmeasured"},
			"Premium budget (premium_unmeasured)"},
	} {
		if got := ruleHeadline(c.row); got != c.want {
			t.Errorf("%s: headline = %q, want %q", c.name, got, c.want)
		}
	}
}

// The text screen states the sell-only fact under the header while it is
// active, naming the rules behind it, and says nothing while it is not.
func TestSellOnlyLineNamesTheRules(t *testing.T) {
	res := rpc.RulesResult{
		SellOnly: risk.RuleSellOnly{Active: true, Rules: []string{risk.RuleCashSellOnly, risk.RuleNetExposure}},
		Rules: []risk.RuleRow{
			{ID: risk.RuleCashSellOnly, Number: 3, Title: "Premium budget"},
			{ID: risk.RuleNetExposure, Number: 15, Title: "Net market exposure"},
		},
	}
	want := "  sell-only  rule 3 premium budget and rule 15 net market exposure at watch or act: buys work against the Rulebook (advisory)"
	if got := sellOnlyLine(res); got != want {
		t.Fatalf("sell-only line = %q, want %q", got, want)
	}
	res.SellOnly = risk.RuleSellOnly{}
	if got := sellOnlyLine(res); got != "" {
		t.Fatalf("inactive sell-only printed %q", got)
	}
}
