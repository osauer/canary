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
// an unreported or non-finite field stays nil, never a zero. The look-ahead
// excess liquidity (amendment 19 R3) follows the same rule.
func TestRulebookMarginInputsReadOnlyReportedFields(t *testing.T) {
	acct := &rpc.AccountResult{ExcessLiquidity: 22000, LookAheadExcess: 18000, InitialMargin: 55000, MaintenanceMargin: 40000}
	all := accountSummaryAuthority{ExcessLiquidityAvailable: true, LookAheadExcessLiquidityAvailable: true, InitialMarginAvailable: true, MaintenanceMarginAvailable: true}
	excess, lookAhead, initial, maintenance := rulebookMarginInputs(acct, all)
	if excess == nil || *excess != 22000 || lookAhead == nil || *lookAhead != 18000 || initial == nil || *initial != 55000 || maintenance == nil || *maintenance != 40000 {
		t.Fatalf("reported fields: %v %v %v %v", excess, lookAhead, initial, maintenance)
	}
	if excess, lookAhead, initial, maintenance := rulebookMarginInputs(acct, accountSummaryAuthority{}); excess != nil || lookAhead != nil || initial != nil || maintenance != nil {
		t.Fatalf("unreported fields read as numbers: %v %v %v %v", excess, lookAhead, initial, maintenance)
	}
	current := all
	current.LookAheadExcessLiquidityAvailable = false
	if excess, lookAhead, _, _ := rulebookMarginInputs(acct, current); excess == nil || lookAhead != nil {
		t.Fatalf("current only: excess %v look-ahead %v", excess, lookAhead)
	}
	acct.ExcessLiquidity, acct.LookAheadExcess = math.Inf(1), math.NaN()
	if excess, lookAhead, _, _ := rulebookMarginInputs(acct, all); excess != nil || lookAhead != nil {
		t.Fatalf("a non-finite excess liquidity was read: %v %v", excess, lookAhead)
	}
	if excess, lookAhead, initial, maintenance := rulebookMarginInputs(nil, all); excess != nil || lookAhead != nil || initial != nil || maintenance != nil {
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
// rule_margin_headroom warning quoting the band its status rests on. A close
// or reduce stays exempt, and a pass or unknown row is quiet. Submit
// eligibility is untouched. Opening sales are covered below.
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
	// "open" is a long opening; a sale never classifies as one, so it is quiet.
	if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), option("SELL", "C"), open); len(ws) != 0 {
		t.Fatalf("a sale classified open warned: %+v", ws)
	}
	for _, status := range []string{risk.RuleStatusPass, risk.RuleStatusUnknown, risk.RuleStatusNotEvaluated} {
		if ws := rulebookPreviewWarnings(result(status, 40, 30), stock, open); len(ws) != 0 {
			t.Fatalf("rule 19 %s warned: %+v", status, ws)
		}
	}
}

// Rule 19 warns opening sales (amendment 19 R4): while margin headroom is at
// watch or act, a SELL that opens or adds to a short stock or option position
// uses margin as a buy does and carries the same advisory warning, on the
// preview's own position-effect classification. A covered close or a reduce
// stays exempt, and so does a sale of any other security type.
func TestPreviewWarnsOpeningShortSalesWhileMarginHeadroomIsLow(t *testing.T) {
	result := func(status string, observed, threshold float64) *rpc.RulesResult {
		return &rpc.RulesResult{Enabled: true, Status: "ok", AsOf: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC), Rules: []risk.RuleRow{
			{ID: risk.RuleMarginHeadroom, Number: 19, Title: "Margin headroom", Status: status, Observed: new(observed), Threshold: new(threshold),
				WatchThreshold: new(30.0), ActThreshold: new(15.0)},
		}}
	}
	put := rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Right: "P", Expiry: "20261218", Strike: 450}}
	call := rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "BBB", SecType: "OPT", Right: "C", Expiry: "20261218", Strike: 120}}
	stock := rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK"}}
	code := "rule_" + risk.RuleMarginHeadroom
	const saleMessage = "Excess liquidity is 18% of NLV, below the margin-headroom 30% watch level; a sale that opens or adds to a short position consumes margin, so this order shrinks the headroom further."

	// An opening short put: no position before, a short one after.
	w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusWatch, 18, 30), put, rpc.OrderPositionImpact{Before: 0, After: -1, Effect: rpc.OrderPositionEffectOpenShort}))[code]
	if w.Severity != risk.RuleStatusWatch || w.Scope != "rulebook" || w.Message != saleMessage ||
		!strings.Contains(w.Impact, "rule 19") || !strings.Contains(w.Impact, "submit eligibility is unaffected") {
		t.Fatalf("opening short put = %+v", w)
	}
	for name, c := range map[string]struct {
		draft  rpc.OrderDraft
		impact rpc.OrderPositionImpact
	}{
		"a larger short put":            {put, rpc.OrderPositionImpact{Before: -1, After: -3, Effect: rpc.OrderPositionEffectIncrease}},
		"a short stock opening":         {stock, rpc.OrderPositionImpact{Before: 0, After: -100, Effect: rpc.OrderPositionEffectOpenShort}},
		"a stock sale through to short": {stock, rpc.OrderPositionImpact{Before: 50, After: -50, Effect: rpc.OrderPositionEffectFlip}},
		"an ETF short opening": {rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "CCC", SecType: "ETF"}},
			rpc.OrderPositionImpact{Before: 0, After: -10, Effect: rpc.OrderPositionEffectOpenShort}},
		"a naked call": {call, rpc.OrderPositionImpact{Before: 0, After: -1, Effect: rpc.OrderPositionEffectOpenShort}},
	} {
		w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), c.draft, c.impact))[code]
		if w.Severity != risk.RuleStatusAct || !strings.Contains(w.Message, "below the margin-headroom 15% act level; a sale that opens or adds to a short position consumes margin") {
			t.Fatalf("%s: warning = %+v", name, w)
		}
	}
	// A sale that closes or reduces a long position, a covered close among
	// them, stays exempt; so do an unclassified sale and a sale of another
	// security type.
	for name, c := range map[string]struct {
		draft  rpc.OrderDraft
		impact rpc.OrderPositionImpact
	}{
		"a covered close of the stock": {stock, rpc.OrderPositionImpact{Before: 100, After: 0, Effect: rpc.OrderPositionEffectClose}},
		"a long stock reduced":         {stock, rpc.OrderPositionImpact{Before: 100, After: 40, Effect: rpc.OrderPositionEffectReduce}},
		"a long put sold to close":     {put, rpc.OrderPositionImpact{Before: 2, After: 0, Effect: rpc.OrderPositionEffectClose}},
		"an unclassified sale":         {put, rpc.OrderPositionImpact{}},
		"a bond sold short": {rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "BOND"}},
			rpc.OrderPositionImpact{Before: 0, After: -1, Effect: rpc.OrderPositionEffectOpenShort}},
	} {
		if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, 9, 15), c.draft, c.impact); len(ws) != 0 {
			t.Fatalf("%s warned: %+v", name, ws)
		}
	}
	// A pass, unknown or off rule 19 is quiet for an opening sale too.
	for _, status := range []string{risk.RuleStatusPass, risk.RuleStatusUnknown, risk.RuleStatusNotEvaluated} {
		if ws := rulebookPreviewWarnings(result(status, 40, 30), put, rpc.OrderPositionImpact{Before: 0, After: -1, Effect: rpc.OrderPositionEffectOpenShort}); len(ws) != 0 {
			t.Fatalf("rule 19 %s warned an opening sale: %+v", status, ws)
		}
	}
}
