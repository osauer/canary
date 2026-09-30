package daemon

import (
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Rule 19 (amendment 18): the template writes its two levels and its mode,
// and the upgrade migration materialises them in an owner file written
// before the rule existed, at Canary's defaults, without moving the policy
// in force or the owner's values.
func TestRulebookMarginHeadroomTemplateAndMigration(t *testing.T) {
	template := string(RulebookPolicyTemplate("v9.9.9"))
	for _, want := range []string{"# Rule 19 — margin headroom (broker excess liquidity)", "\nmargin_headroom_watch_pct = 30.0\n", "\nmargin_headroom_act_pct = 15.0\n",
		"\nmargin_headroom = \"alert\"\n", "Rule 19 reads downward"} {
		if !strings.Contains(template, want) {
			t.Fatalf("template lacks %q", want)
		}
	}
	// The owner's reviewed file as the previous release wrote it: no rule 19
	// key, no rule 19 mode, one limit of the owner's own.
	var lines []string
	for line := range strings.SplitSeq(template, "\n") {
		if strings.Contains(line, "margin_headroom") || strings.HasPrefix(line, "# Canary defaults, not yet reviewed.") {
			continue
		}
		if strings.HasPrefix(line, "fx_exposure_watch_pct = ") {
			line = "fx_exposure_watch_pct = 70.0  # mine"
		}
		lines = append(lines, line)
	}
	old := []byte(strings.Join(lines, "\n"))
	before, err := parseRulebookPolicy(old)
	if err != nil || !slices.Equal(slices.Sorted(slices.Values(before.missing)), []string{"margin_headroom_act_pct", "margin_headroom_watch_pct", "modes.margin_headroom"}) {
		t.Fatalf("older file: err %v missing %v", err, before.missing)
	}
	out, changes, _, err := migrateRulebookPolicyFile(old, "v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changes, []string{"added margin_headroom_act_pct at Canary's default", "added margin_headroom_watch_pct at Canary's default", "added modes.margin_headroom at Canary's default"}) {
		t.Fatalf("changes = %v", changes)
	}
	after, err := parseRulebookPolicy(out)
	if err != nil || len(after.missing) != 0 || after.policy.EffectiveFingerprintKey() != before.policy.EffectiveFingerprintKey() ||
		after.policy.MarginHeadroomWatchPct != 30 || after.policy.MarginHeadroomActPct != 15 || after.policy.ModeFor(risk.RuleMarginHeadroom) != risk.RuleModeAlert ||
		after.policy.FXExposureWatchPct != 70 || after.policy.Version != before.policy.Version || policyFileReview(out) != "" {
		t.Fatalf("migrated: err %v missing %v policy %+v", err, after.missing, after.policy)
	}
	if !strings.Contains(string(out), "fx_exposure_watch_pct = 70.0  # mine") {
		t.Fatalf("the owner's line was lost:\n%s", out)
	}
}

// canary rules policy set validates rule 19's levels as the daemon will:
// 0 ≤ act ≤ watch ≤ 100.
func TestEditRulebookPolicyValidatesMarginHeadroom(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies", "rulebook-policy.toml")
	for _, bad := range [][]string{{"margin_headroom_act_pct=35"}, {"margin_headroom_watch_pct=101"}, {"margin_headroom_act_pct=-1"}, {"margin_headroom_watch_pct=10"}} {
		if _, err := EditRulebookPolicy(path, bad, nil, false); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
	edit, err := EditRulebookPolicy(path, []string{"margin_headroom_watch_pct=40", "margin_headroom_act_pct=20"}, nil, false)
	if err != nil || !slices.Equal(edit.Differs, []string{"margin_headroom_act_pct", "margin_headroom_watch_pct"}) {
		t.Fatalf("edit = %+v err %v", edit, err)
	}
}

// The account summary feeds rule 19 only the fields the broker reported:
// an unreported or non-finite field stays nil, never a zero.
func TestRulebookMarginInputsReadOnlyReportedFields(t *testing.T) {
	acct := &rpc.AccountResult{ExcessLiquidity: 22000, InitialMargin: 55000, MaintenanceMargin: 40000}
	all := accountSummaryAuthority{ExcessLiquidityAvailable: true, InitialMarginAvailable: true, MaintenanceMarginAvailable: true}
	excess, initial, maintenance := rulebookMarginInputs(acct, all)
	if excess == nil || *excess != 22000 || initial == nil || *initial != 55000 || maintenance == nil || *maintenance != 40000 {
		t.Fatalf("reported fields: %v %v %v", excess, initial, maintenance)
	}
	if excess, initial, maintenance := rulebookMarginInputs(acct, accountSummaryAuthority{}); excess != nil || initial != nil || maintenance != nil {
		t.Fatalf("unreported fields read as numbers: %v %v %v", excess, initial, maintenance)
	}
	acct.ExcessLiquidity = math.Inf(1)
	if excess, _, _ := rulebookMarginInputs(acct, all); excess != nil {
		t.Fatalf("a non-finite excess liquidity was read: %v", *excess)
	}
	if excess, initial, maintenance := rulebookMarginInputs(nil, all); excess != nil || initial != nil || maintenance != nil {
		t.Fatal("no account summary produced inputs")
	}
}

// Rule 19 is a canonical alert row: an act reading opens an episode under
// rulebook_margin_headroom, and it rests on the account source alone.
func TestAlertShadowRulebookCarriesMarginHeadroom(t *testing.T) {
	if n, ok := alertShadowCanonicalRulebookRow(risk.RuleMarginHeadroom); !ok || n != 19 {
		t.Fatalf("canonical row = %d %v", n, ok)
	}
	if got := alertShadowRulebookHealthRelevance[risk.RuleMarginHeadroom]; !slices.Equal(got, []string{"account"}) {
		t.Fatalf("relevance = %v", got)
	}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	result := rulebookModeTestResult(now)
	i := slices.IndexFunc(result.Rules, func(r risk.RuleRow) bool { return r.ID == risk.RuleMarginHeadroom })
	if i < 0 || len(result.Rules) != len(risk.RuleIDs()) {
		t.Fatalf("canonical rows %d, rule 19 at %d", len(result.Rules), i)
	}
	result.Rules[i].Status = risk.RuleStatusAct
	got := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if !got.Covered || len(got.Observations) != 1 || got.Observations[0].PresentationCode != rpc.AlertPresentationRulebookMarginHeadroom ||
		got.Observations[0].Severity != rpc.AlertSeverityAct {
		t.Fatalf("batch = %+v", got)
	}
}

// Rule 19 warns buys (amendment 19): while margin headroom is at watch or
// act, every buy, stock or option, call or put, carries an advisory
// rule_margin_headroom warning quoting the band its status rests on. A sale
// never warns, a close or reduce stays exempt, and a pass or unknown row is
// quiet. Submit eligibility is untouched.
func TestPreviewWarnsBuysWhileMarginHeadroomIsLow(t *testing.T) {
	result := func(status string, observed, threshold float64) *rpc.RulesResult {
		return &rpc.RulesResult{Enabled: true, Status: "ok", AsOf: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC), Rules: []risk.RuleRow{
			{ID: risk.RuleMarginHeadroom, Number: 19, Title: "Margin headroom", Status: status, Observed: new(observed), Threshold: new(threshold),
				WatchThreshold: new(30.0), ActThreshold: new(15.0)},
		}}
	}
	stock := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK"}}
	option := func(action, right string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Right: right, Expiry: "20261218", Strike: 500}}
	}
	open := rpc.OrderPositionImpact{Effect: "open"}
	code := "rule_" + risk.RuleMarginHeadroom

	w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusWatch, 22, 30), stock, open))[code]
	if w.Severity != risk.RuleStatusWatch || w.Scope != "rulebook" ||
		w.Message != "Excess liquidity is 22% of NLV, below the margin-headroom 30% watch level; a buy consumes margin, so this order shrinks the headroom further." ||
		!strings.Contains(w.Impact, "rule 19") || !strings.Contains(w.Impact, "submit eligibility is unaffected") {
		t.Fatalf("watch warning = %+v", w)
	}
	for _, draft := range []rpc.OrderDraft{option("BUY", "C"), option("BUY", "P")} {
		w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), draft, open))[code]
		if w.Severity != risk.RuleStatusAct || !strings.Contains(w.Message, "Excess liquidity is 9% of NLV, below the margin-headroom 15% act level") {
			t.Fatalf("act warning on %s %s = %+v", draft.Action, draft.Contract.Right, w)
		}
	}
	// An increase is not a close: it warns too.
	if w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusWatch, 22, 30), stock, rpc.OrderPositionImpact{Effect: "increase"}))[code]; w.Code == "" {
		t.Fatal("a buy that increases a position did not warn")
	}
	for _, effect := range []string{"close", "reduce"} {
		if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), stock, rpc.OrderPositionImpact{Effect: effect}); len(ws) != 0 {
			t.Fatalf("%s warned: %+v", effect, ws)
		}
	}
	if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), option("SELL", "C"), open); len(ws) != 0 {
		t.Fatalf("a sale warned: %+v", ws)
	}
	for _, status := range []string{risk.RuleStatusPass, risk.RuleStatusUnknown, risk.RuleStatusNotEvaluated} {
		if ws := rulebookPreviewWarnings(result(status, 40, 30), stock, open); len(ws) != 0 {
			t.Fatalf("rule 19 %s warned: %+v", status, ws)
		}
	}
}
