package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Rule 20 is a canonical alert row: it rests on account, positions and
// earnings, and an act opens an episode with its own presentation code.
func TestAlertShadowRulebookCarriesEarningsPremiumFreeze(t *testing.T) {
	if n, ok := alertShadowCanonicalRulebookRow(risk.RuleEarningsPremiumFreeze); !ok || n != 20 {
		t.Fatalf("canonical row = %d %v", n, ok)
	}
	if got := alertShadowRulebookHealthRelevance[risk.RuleEarningsPremiumFreeze]; !slices.Equal(got, []string{"account", "positions", "earnings"}) {
		t.Fatalf("relevance = %v", got)
	}
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	result := rulebookModeTestResult(now)
	i := slices.IndexFunc(result.Rules, func(r risk.RuleRow) bool { return r.ID == risk.RuleEarningsPremiumFreeze })
	if i < 0 || len(result.Rules) != len(risk.RuleIDs()) {
		t.Fatalf("canonical rows %d, rule 20 at %d", len(result.Rules), i)
	}
	result.Rules[i].Status = risk.RuleStatusAct
	got := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if !got.Covered || len(got.Observations) != 1 || got.Observations[0].PresentationCode != rpc.AlertPresentationRulebookEarningsPremium ||
		got.Observations[0].Severity != rpc.AlertSeverityAct {
		t.Fatalf("batch = %+v", got)
	}
	// No long option open is a safe not-evaluated: nothing hides behind it.
	result.Rules[i] = risk.RuleRow{ID: risk.RuleEarningsPremiumFreeze, Number: 20, Mode: risk.RuleModeAlert,
		Status: risk.RuleStatusNotEvaluated, Reason: risk.RuleReasonNoLongOptions}
	if !alertShadowRulebookSafeNotEvaluated(result.Rules[i], result) {
		t.Fatal("no long option open must count as a safe not-evaluated row")
	}
	result.Rules[i].Offenders = []risk.RuleOffender{{Symbol: "SYNTH"}}
	if alertShadowRulebookSafeNotEvaluated(result.Rules[i], result) {
		t.Fatal("an offender behind a not-evaluated reason is never safe")
	}
}

// Rule 20 warns an option buy on a name it names (amendment 21): the buy
// adds premium into the print. Stock buys, sales, closes and other names
// stay quiet, and so does a pass or unknown row.
func TestPreviewWarnsOptionBuysInsideThePremiumFreeze(t *testing.T) {
	result := func(status string, offenders ...string) *rpc.RulesResult {
		row := risk.RuleRow{ID: risk.RuleEarningsPremiumFreeze, Number: 20, Title: "Premium at risk near earnings", Status: status}
		for _, sym := range offenders {
			row.Offenders = append(row.Offenders, risk.RuleOffender{Symbol: sym})
		}
		return &rpc.RulesResult{Enabled: true, Status: "ok", AsOf: time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC), Rules: []risk.RuleRow{row}}
	}
	option := func(action, sym string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{Symbol: sym, SecType: "OPT", Right: "C", Expiry: "20261218", Strike: 40}}
	}
	stock := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "AAA", SecType: "STK"}}
	open := rpc.OrderPositionImpact{Effect: "open"}
	code := "rule_" + risk.RuleEarningsPremiumFreeze

	w := previewCodes(rulebookPreviewWarnings(result(risk.RuleStatusAct, "AAA"), option("BUY", "AAA"), open))[code]
	if w.Severity != risk.RuleStatusAct || w.Scope != "rulebook" || !strings.Contains(w.Message, "AAA carries option premium at risk inside the pre-earnings window") ||
		!strings.Contains(w.Impact, "rule 20") {
		t.Fatalf("act warning = %+v", w)
	}
	for name, draft := range map[string]rpc.OrderDraft{"stock buy": stock, "sale": option("SELL", "AAA"), "other name": option("BUY", "BBB")} {
		if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, "AAA"), draft, open); len(ws) != 0 {
			t.Fatalf("%s warned: %+v", name, ws)
		}
	}
	if ws := rulebookPreviewWarnings(result(risk.RuleStatusAct, "AAA"), option("BUY", "AAA"), rpc.OrderPositionImpact{Effect: "close"}); len(ws) != 0 {
		t.Fatalf("a close warned: %+v", ws)
	}
	for _, status := range []string{risk.RuleStatusPass, risk.RuleStatusUnknown} {
		if ws := rulebookPreviewWarnings(result(status, "AAA"), option("BUY", "AAA"), open); len(ws) != 0 {
			t.Fatalf("rule 20 %s warned: %+v", status, ws)
		}
	}
}
