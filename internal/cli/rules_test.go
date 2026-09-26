package cli

import (
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
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
		{"single limit", risk.RuleRow{Title: "Cash reserve", Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(50.0), Threshold: new(75.0)},
			"Cash reserve (observed 50.0 vs 75.0 % NLV)"},
		{"unmeasured", risk.RuleRow{Title: "Cash reserve", Status: risk.RuleStatusUnknown, Reason: "available_funds_unavailable"},
			"Cash reserve (available_funds_unavailable)"},
	} {
		if got := ruleHeadline(c.row); got != c.want {
			t.Errorf("%s: headline = %q, want %q", c.name, got, c.want)
		}
	}
}
