package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
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

// While the Rulebook reads sell-only (amendment 17), the preview keeps
// rule_cash_sell_only on every buy, now in the premium budget's words, and
// adds rule_net_exposure on a buy that adds to the side rule 15 flags. A
// close or reduce never warns; the warnings are advisory only.
func TestPreviewWarnsBuysWhileTheRulebookReadsSellOnly(t *testing.T) {
	stock := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK"}}
	option := func(action, right string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Right: right, Expiry: "20261218", Strike: 500}}
	}
	open := rpc.OrderPositionImpact{Effect: "open"}

	got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(120), stock, open))
	budget, net := got["rule_"+risk.RuleCashSellOnly], got["rule_"+risk.RuleNetExposure]
	if budget.Severity != risk.RuleStatusWatch || budget.Scope != "rulebook" ||
		budget.Message != "Option premium at risk is 27% of NLV, at or above the premium budget's 25% watch level; the Rulebook reads sell-only, so this buy works against it." {
		t.Fatalf("premium budget warning = %+v", budget)
	}
	if net.Severity != risk.RuleStatusAct || !strings.Contains(net.Message, "The book is net long 120% of NLV, at or above the net-exposure 100% act level") ||
		!strings.Contains(net.Impact, "submit eligibility is unaffected") {
		t.Fatalf("net exposure warning = %+v", net)
	}

	// A bought put hedges a net-long book: the premium budget still warns,
	// net exposure does not; on a net-short book it is the call that is quiet.
	if got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(120), option("BUY", "P"), open)); got["rule_"+risk.RuleNetExposure].Code != "" || got["rule_"+risk.RuleCashSellOnly].Code == "" {
		t.Fatalf("put on a net-long book: %+v", got)
	}
	if got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(-120), option("BUY", "P"), open)); !strings.Contains(got["rule_"+risk.RuleNetExposure].Message, "net short") {
		t.Fatalf("put on a net-short book: %+v", got)
	}
	if got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(-120), option("BUY", "C"), open)); got["rule_"+risk.RuleNetExposure].Code != "" {
		t.Fatalf("call on a net-short book: %+v", got)
	}

	for _, effect := range []string{"close", "reduce"} {
		if ws := rulebookPreviewWarnings(sellOnlyTestResult(120), stock, rpc.OrderPositionImpact{Effect: effect}); len(ws) != 0 {
			t.Fatalf("%s warned: %+v", effect, ws)
		}
	}
	if got := previewCodes(rulebookPreviewWarnings(sellOnlyTestResult(120), option("SELL", "C"), open)); len(got) != 0 {
		t.Fatalf("a sale warned: %+v", got)
	}
	quiet := sellOnlyTestResult(120)
	quiet.Rules[1].Status = risk.RuleStatusPass
	if got := previewCodes(rulebookPreviewWarnings(quiet, stock, open)); got["rule_"+risk.RuleNetExposure].Code != "" {
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
