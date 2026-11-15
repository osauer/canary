package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// sellOnlyTestResult is a synthetic Rulebook result with rule 3 (premium
// budget) at watch and rule 15 (net exposure) at act on the long side.
func sellOnlyTestResult(netSide float64) *rpc.RulesResult {
	return &rpc.RulesResult{
		Enabled: true, Status: "ok", AsOf: time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC),
		SellOnly: risk.RuleSellOnly{Active: true, Rules: []string{risk.RuleCashSellOnly, risk.RuleNetExposure}},
		Rules: []risk.RuleRow{
			{ID: risk.RuleCashSellOnly, Number: 3, Title: "Premium budget", Status: risk.RuleStatusWatch, SellOnly: true,
				Observed: new(27.0), Threshold: new(25.0), WatchThreshold: new(25.0), ActThreshold: new(35.0), RegimeSet: risk.RegimeBucketCalm},
			{ID: risk.RuleNetExposure, Number: 15, Title: "Net market exposure", Status: risk.RuleStatusAct, SellOnly: true,
				Observed: new(120.0), Threshold: new(100.0), WatchThreshold: new(75.0), ActThreshold: new(100.0), RegimeSet: risk.RegimeBucketConfirmed,
				Offenders: []risk.RuleOffender{{Symbol: "AAA", Observed: netSide}}},
		},
	}
}

func previewCodes(ws []rpc.DataWarning) map[string]rpc.DataWarning {
	out := map[string]rpc.DataWarning{}
	for _, w := range ws {
		out[w.Code] = w
	}
	return out
}

// Rules 3 and 15 judge each order by what it adds (owner, 2026-10-27 17:24
// CET): rule 3 warns a buy that adds option premium, never a stock buy;
// rule 15 warns an opening or adding order that moves net exposure further
// from zero on the side it flags, a sale included, and never one that brings
// it back. A close or reduce never warns; the warnings are advisory only.
func TestPreviewJudgesEachOrderByItsDirection(t *testing.T) {
	stock := func(action string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK"}}
	}
	option := func(action, right string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Right: right, Expiry: "20261218", Strike: 500}}
	}
	open := rpc.OrderPositionImpact{Effect: "open"}
	warns := func(side float64, draft rpc.OrderDraft, position rpc.OrderPositionImpact) (budget, net rpc.DataWarning) {
		got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(side), draft, position))
		return got["rule_"+risk.RuleCashSellOnly], got["rule_"+risk.RuleNetExposure]
	}

	budget, net := warns(120, option("BUY", "C"), open)
	if budget.Severity != risk.RuleStatusWatch || budget.Scope != "rulebook" ||
		budget.Message != "Option premium at risk is 27% of NLV, at or above the premium budget's 25% watch level; this option buy adds premium." {
		t.Fatalf("premium budget warning = %+v", budget)
	}
	if net.Severity != risk.RuleStatusAct || net.Message != "The book is net long 120% of NLV, at or above the net-exposure 100% act level; this order adds to the net long." ||
		!strings.Contains(net.Impact, "submit eligibility is unaffected") {
		t.Fatalf("net exposure warning = %+v", net)
	}

	for _, c := range []struct {
		name        string
		side        float64
		draft       rpc.OrderDraft
		budget, net bool
	}{
		{"stock buy on a net-long book", 120, stock("BUY"), false, true},
		{"stock buy on a net-short book", -120, stock("BUY"), false, false},
		{"short sale on a net-short book", -120, stock("SELL"), false, true},
		{"short sale on a net-long book", 120, stock("SELL"), false, false},
		{"put buy on a net-long book", 120, option("BUY", "P"), true, false},
		{"put buy on a net-short book", -120, option("BUY", "P"), true, true},
		{"call buy on a net-short book", -120, option("BUY", "C"), true, false},
		{"call sale on a net-short book", -120, option("SELL", "C"), false, true},
		{"put sale on a net-long book", 120, option("SELL", "P"), false, true},
		{"put sale on a net-short book", -120, option("SELL", "P"), false, false},
	} {
		budget, net := warns(c.side, c.draft, open)
		if (budget.Code != "") != c.budget || (net.Code != "") != c.net {
			t.Errorf("%s: premium warned %v (want %v), net warned %v (want %v)", c.name, budget.Code != "", c.budget, net.Code != "", c.net)
		}
	}
	if _, net := warns(-120, stock("SELL"), open); !strings.HasSuffix(net.Message, "this order adds to the net short.") {
		t.Fatalf("short-side wording = %q", net.Message)
	}

	for _, effect := range []string{"close", "reduce"} {
		for _, draft := range []rpc.OrderDraft{stock("SELL"), option("BUY", "P"), option("SELL", "C")} {
			if ws := rulebookPreviewWarnings(sellOnlyTestResult(-120), draft, rpc.OrderPositionImpact{Effect: effect}); len(ws) != 0 {
				t.Fatalf("%s %s warned: %+v", effect, draft.Action, ws)
			}
		}
	}
	quiet := sellOnlyTestResult(120)
	quiet.Rules[1].Status = risk.RuleStatusPass
	if got := previewCodes(rulebookPreviewWarnings(quiet, stock("BUY"), open)); got["rule_"+risk.RuleNetExposure].Code != "" {
		t.Fatalf("rule 15 below watch warned: %+v", got)
	}
}

// Rule 3 without a long option is a trusted negative for the alert authority
// (no premium to budget, no offender to hide); rule 12's retired
// no_index_protection is not, because since amendment 17 a long book without
// protection is a watch. A book with no long exposure stays not evaluated.
func TestAlertComposerNotEvaluatedReasonsFollowAmendment17(t *testing.T) {
	for _, c := range []struct {
		row  risk.RuleRow
		safe bool
	}{
		{risk.RuleRow{ID: risk.RuleCashSellOnly, Status: risk.RuleStatusNotEvaluated, Reason: risk.RuleReasonNoLongOptions}, true},
		{risk.RuleRow{ID: risk.RuleCashSellOnly, Status: risk.RuleStatusNotEvaluated, Reason: risk.RuleReasonNoLongOptions, Offenders: []risk.RuleOffender{{Symbol: "AAA"}}}, false},
		{risk.RuleRow{ID: risk.RuleCashSellOnly, Status: risk.RuleStatusNotEvaluated, Reason: "available_funds_unavailable"}, false},
		{risk.RuleRow{ID: risk.RuleHedgeIntegrity, Status: risk.RuleStatusNotEvaluated, Reason: risk.RuleReasonNoLongBook}, true},
		{risk.RuleRow{ID: risk.RuleHedgeIntegrity, Status: risk.RuleStatusNotEvaluated, Reason: "no_index_protection"}, false},
	} {
		if got := alertShadowRulebookSafeNotEvaluated(c.row, rpc.RulesResult{}); got != c.safe {
			t.Errorf("%s/%s (offenders %d): safe = %v, want %v", c.row.ID, c.row.Reason, len(c.row.Offenders), got, c.safe)
		}
	}

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	result := rulebookModeTestResult(now)
	for i := range result.Rules {
		switch result.Rules[i].ID {
		case risk.RuleCashSellOnly:
			result.Rules[i].Status, result.Rules[i].Reason = risk.RuleStatusNotEvaluated, risk.RuleReasonNoLongOptions
		case risk.RuleHedgeIntegrity:
			result.Rules[i].Status, result.Rules[i].Reason = risk.RuleStatusWatch, risk.RuleReasonUnhedged
		}
	}
	got := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if !got.Covered || got.EvidenceHealth != rpc.AlertEvidenceCurrent || len(got.Observations) != 1 ||
		got.Observations[0].PresentationCode != rpc.AlertPresentationRulebookHedgeIntegrity || got.Observations[0].Severity != rpc.AlertSeverityWatch {
		t.Fatalf("batch = %+v, want a covered batch with one unhedged watch", got)
	}

	// Rules 3 and 15 read regime-banded levels now, so like rules 4 and 12
	// they rest on the regime stage: a stale stage holds their episodes.
	for _, id := range []string{risk.RuleCashSellOnly, risk.RuleNetExposure, risk.RuleExtrinsicBudget, risk.RuleHedgeIntegrity} {
		if !slices.Contains(alertShadowRulebookHealthRelevance[id], "regime_stage") {
			t.Errorf("%s does not rest on the regime stage: %v", id, alertShadowRulebookHealthRelevance[id])
		}
	}
	result.Status = "degraded" // the producer stamps degraded with any source not ok
	for i := range result.Rules {
		if result.Rules[i].ID == risk.RuleCashSellOnly {
			result.Rules[i].Status, result.Rules[i].Reason = risk.RuleStatusPass, ""
		}
	}
	for i := range result.InputHealth {
		if result.InputHealth[i].Source == "regime_stage" {
			result.InputHealth[i].Status = rpc.SourceStatusStale
		}
	}
	stale := alertShadowMapRulebook(alertShadowBrokerScope{account: "redacted", mode: "paper"}, result, now)
	if !slices.Contains(stale.UncoveredRules, risk.RuleCashSellOnly) || !slices.Contains(stale.UncoveredRules, risk.RuleNetExposure) {
		t.Fatalf("a stale regime stage left rules 3 and 15 covered: %+v", stale.UncoveredRules)
	}
}

// The cached result is shared with every reader: its sell-only rules are a
// copy, not the canonical slice.
func TestCloneRulesResultCopiesTheSellOnlyFact(t *testing.T) {
	in := sellOnlyTestResult(120)
	out := cloneRulesResult(in)
	out.SellOnly.Rules[0] = "mutated"
	if !out.SellOnly.Active || in.SellOnly.Rules[0] != risk.RuleCashSellOnly || !slices.Equal(in.SellOnly.Rules, []string{risk.RuleCashSellOnly, risk.RuleNetExposure}) {
		t.Fatalf("clone shares the sell-only rules: %+v", in.SellOnly)
	}
}

// Rule 15 measures equity and option exposure only; a bill or bond never
// enters it, so buying one does not add to the side it flags, and it adds
// no option premium for rule 3 either.
func TestPreviewDoesNotCountABondBuyAsNetExposure(t *testing.T) {
	open := rpc.OrderPositionImpact{Effect: "open"}
	for _, secType := range []string{"BOND", "BILL"} {
		bond := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "SYNB", SecType: secType}}
		got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(120), bond, open))
		if got["rule_"+risk.RuleNetExposure].Code != "" || got["rule_"+risk.RuleCashSellOnly].Code != "" {
			t.Fatalf("%s buy warnings = %+v", secType, got)
		}
	}
}
