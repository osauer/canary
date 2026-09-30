package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A watch row on a two-band rule names both bands as the policy set them, so
// the reader sees how far the act level is; other rows leave the limit to
// their evidence line, and the renderer never picks a band itself.
func TestRuleLevelsNameBothBandsOnWatchRows(t *testing.T) {
	for _, c := range []struct {
		name string
		row  risk.RuleRow
		want string
	}{
		{"fractional band", risk.RuleRow{Unit: "% NLV", Status: risk.RuleStatusWatch,
			Observed: new(8.0), Threshold: new(7.5), WatchThreshold: new(7.5), ActThreshold: new(12.0)},
			"Levels: watch 7.5% NLV · act 12% NLV"},
		{"day unit", risk.RuleRow{Unit: "days", Status: risk.RuleStatusWatch,
			Observed: new(9.0), Threshold: new(14.0), WatchThreshold: new(14.0), ActThreshold: new(7.0)},
			"Levels: watch 14 days · act 7 days"},
		{"act row", risk.RuleRow{Unit: "% premium lost", Status: risk.RuleStatusAct,
			Observed: new(70.0), Threshold: new(60.0), WatchThreshold: new(40.0), ActThreshold: new(60.0)}, ""},
		{"hedge range", risk.RuleRow{ID: risk.RuleHedgeIntegrity, Unit: "% gross long", Status: risk.RuleStatusWatch,
			Observed: new(14.3), Threshold: new(30.0), WatchThreshold: new(30.0), ActThreshold: new(100.0)}, ""},
		{"single limit", risk.RuleRow{Unit: "% NLV", Status: risk.RuleStatusWatch, Observed: new(70.0), Threshold: new(60.0)}, ""},
	} {
		if got := ruleLevels(c.row); got != c.want {
			t.Errorf("%s: levels = %q, want %q", c.name, got, c.want)
		}
	}
}

// The header is one line in the brief's date style; the compiled policy id
// that only restates its version collapses, and status shows only when it
// is not ok.
func TestRulesHeaderIsOneCleanLine(t *testing.T) {
	res := &rpc.RulesResult{AsOf: time.Date(2026, 9, 30, 19, 20, 0, 0, time.UTC), Status: "ok",
		PolicyID: "rulebook-v5", PolicyVersion: 5, PolicyStatus: &rpc.RulebookPolicyStatus{Source: "file"}}
	got := rulesHeader(&Env{}, res)
	want := "Trading rulebook · " + res.AsOf.Local().Format("2 Jan 15:04 MST") + " · policy v5 (your file)"
	if got != want {
		t.Fatalf("header = %q, want %q", got, want)
	}
	res.Status, res.PolicyID = "degraded", "desk-policy"
	if got := rulesHeader(&Env{}, res); !strings.HasSuffix(got, " · policy desk-policy v5 (your file) · status degraded") {
		t.Fatalf("degraded header = %q", got)
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
// id trails its short title, a watch row names both levels, the regime note
// reads in local time, rules idle by configuration fold into one quiet line,
// and a footnote that only restates exempt lines on screen is dropped.
func TestRulesTextFitsTheTerminalAndFoldsIdleRules(t *testing.T) {
	t.Setenv("COLUMNS", "72")
	stageAt := time.Date(2026, 9, 30, 19, 8, 0, 0, time.UTC)
	res := &rpc.RulesResult{
		Enabled: true, Status: "ok", PolicyID: "rulebook-v5", PolicyVersion: 5,
		Rules: []risk.RuleRow{
			{ID: risk.RuleHedgeIntegrity, Number: 12, Title: "Index protection size", Status: risk.RuleStatusWatch, Unit: "% gross long",
				Observed: new(14.3), Threshold: new(30.0), WatchThreshold: new(30.0), ActThreshold: new(100.0),
				Evidence:  "Protection short delta is 14.3% of gross long exposure, below the 30–50% range.",
				Exempt:    []risk.RuleOffender{{Symbol: "SPY", Leg: "SPY 20261218 P 620", Note: "portfolio protection"}},
				RegimeSet: risk.RegimeBucketEarlyWarning,
				Notes:     []string{"thresholds: early_warning regime set (stage as of Sep 30 19:08 UTC)"}},
			{ID: risk.RuleRedOnGreen, Number: 9, Title: "Holding falls while the market rises", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonRuleOff, Evidence: "Turned off in the Rulebook policy."},
			{ID: risk.RuleWinnerTrim, Number: 10, Title: "Large winner today", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonRuleOff, Evidence: "Turned off in the Rulebook policy."},
			{ID: risk.RuleClusterStress, Number: 17, Title: "Cluster falling together", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonNoClusters, Evidence: "No cluster is declared in the Rulebook policy."},
		},
		BreachCounts: map[string]int{risk.RuleStatusWatch: 1, risk.RuleStatusNotEvaluated: 3},
		InputHealth:  []rpc.SourceHealth{{Source: "regime_stage", Status: "ok", AsOf: stageAt}},
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
		"WATCH 12  Index protection size  hedge_integrity\n",
		"          Protection short delta is 14.3% of gross long exposure, below\n          the 30–50% range.\n",
		"          Thresholds: early-warning set · stage as of " + stageAt.Local().Format("2 Jan 15:04 MST") + "\n",
		"          exempt: SPY 20261218 P 620 — portfolio protection\n",
		"--        rules 9, 10 are turned off in the Rulebook policy; rule 17 has\n          no clusters declared to test (--all lists them)\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	for _, reject := range []string{"Turned off in the Rulebook policy.", "broker-proven", "no_clusters", "(thresholds", "not applicable by security type", "Levels:"} {
		if strings.Contains(text, reject) {
			t.Errorf("unexpected %q in:\n%s", reject, text)
		}
	}

	// A security-type exemption not shown above keeps its footnote, which
	// never claims a broker identity proof.
	res.Earnings[0].Symbol = "QQQ"
	out.Reset()
	renderRulesText(&Env{Stdout: &out}, &out, res, false)
	if !strings.Contains(out.String(), "Issuer earnings not applicable by security type: QQQ (fund).") {
		t.Errorf("footnote for an exemption not on screen is missing:\n%s", out.String())
	}

	out.Reset()
	renderRulesText(&Env{Stdout: &out}, &out, res, true)
	if !strings.Contains(out.String(), "--     9  Holding falls while the market rises  red_on_green\n") {
		t.Errorf("--all must list a turned-off rule in full:\n%s", out.String())
	}
}
