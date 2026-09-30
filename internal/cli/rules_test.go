package cli

import (
	"bytes"
	"strings"
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
			"Long option loss limit (observed 70.0 vs 60% premium lost; watch 40, act 60)"},
		{"fractional band", risk.RuleRow{Title: "Option time value at risk", Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(8.0), Threshold: new(7.5), WatchThreshold: new(7.5), ActThreshold: new(12.0)},
			"Option time value at risk (observed 8.0 vs 7.5% NLV; watch 7.5, act 12)"},
		{"single limit", risk.RuleRow{Title: "Foreign-currency exposure", Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(70.0), Threshold: new(60.0)},
			"Foreign-currency exposure (observed 70.0 vs 60.0% NLV)"},
		{"day unit", risk.RuleRow{Title: "Options nearing expiry", Unit: "days", Status: risk.RuleStatusWatch,
			Observed: new(9.0), Threshold: new(14.0)},
			"Options nearing expiry (observed 9.0 vs 14.0 days)"},
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

// The text screen fits the terminal: every line wraps inside COLUMNS and
// hangs under the gutter, the status reads in the summary's words, the rule
// id trails its headline, rules the policy turned off fold into one line,
// and a security-type exemption never claims a broker identity proof.
func TestRulesTextFitsTheTerminalAndFoldsTurnedOffRules(t *testing.T) {
	t.Setenv("COLUMNS", "72")
	res := &rpc.RulesResult{
		Enabled: true, Status: "ok", PolicyID: "rulebook-v5", PolicyVersion: 5,
		Rules: []risk.RuleRow{
			{ID: risk.RuleHedgeIntegrity, Number: 12, Title: "Index protection size", Status: risk.RuleStatusWatch, Unit: "% gross long",
				Observed: new(14.3), Threshold: new(30.0), WatchThreshold: new(30.0), ActThreshold: new(100.0),
				Evidence: "Protection short delta is 14.3% of gross long exposure, below the 30–50% range.",
				Exempt:   []risk.RuleOffender{{Symbol: "SPY", Leg: "SPY 20261218 P 620", Note: "portfolio protection"}}},
			{ID: risk.RuleRedOnGreen, Number: 9, Title: "Holding falls while the market rises", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonRuleOff, Evidence: "Turned off in the Rulebook policy."},
			{ID: risk.RuleWinnerTrim, Number: 10, Title: "Large winner today", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonRuleOff, Evidence: "Turned off in the Rulebook policy."},
		},
		BreachCounts: map[string]int{risk.RuleStatusWatch: 1, risk.RuleStatusNotEvaluated: 2},
		Earnings: []rpc.EarningsInfo{{Symbol: "SPY", Source: "security_type", Status: rpc.EarningsStatusNotApplicable,
			Reason: risk.EarningsReasonNonIssuerSecurity, SecurityType: "FUND"}},
	}
	var out bytes.Buffer
	renderRulesText(&Env{Stdout: &out}, &out, res, false)
	text := out.String()
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if visibleLen(line) > 72 {
			t.Errorf("line wider than the terminal (%d): %q", visibleLen(line), line)
		}
	}
	for _, want := range []string{
		"WATCH 12  Index protection size (observed 14.3 vs 30% gross long; watch\n          30, act 100)  hedge_integrity\n",
		"          Protection short delta is 14.3% of gross long exposure, below\n          the 30–50% range.\n",
		"          exempt: SPY 20261218 P 620 — portfolio protection\n",
		"--        rules 9, 10 are turned off in the Rulebook policy (--all lists\n          them)\n",
		"Issuer earnings not applicable by security type: SPY (fund).",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, reject := range []string{"Turned off in the Rulebook policy.", "broker-proven"} {
		if strings.Contains(text, reject) {
			t.Errorf("unexpected %q in:\n%s", reject, text)
		}
	}

	out.Reset()
	renderRulesText(&Env{Stdout: &out}, &out, res, true)
	if !strings.Contains(out.String(), "--     9  Holding falls while the market rises (rule_off)  red_on_green\n") {
		t.Errorf("--all must list a turned-off rule in full:\n%s", out.String())
	}
}
