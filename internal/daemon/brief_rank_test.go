package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func rankState(status string) rpc.BriefRowState {
	return rpc.BriefRowState{Status: status, Detail: "synthetic " + status}
}

// rankReady composes a Ready section through the production builder from
// synthetic row states, so Ranked is the one the wire would carry.
func rankReady(states map[string]string, events []string, pulse string) rpc.BriefReadySection {
	st := func(key string) rpc.BriefRowState { return rankState(states[key]) }
	var market rpc.BriefMarketSection
	market.Regime.BriefRowState = st(rpc.BriefReadyRowRegime)
	market.Breadth.BriefRowState = st(rpc.BriefReadyRowBreadth)
	market.Gamma.BriefRowState = st(rpc.BriefReadyRowGamma)
	market.Stress.BriefRowState = st(rpc.BriefReadyRowStress)
	var calendar rpc.BriefCalendarSection
	calendar.Session.BriefRowState = st(rpc.BriefReadyRowSession)
	for i, status := range events {
		calendar.MarketEvents = append(calendar.MarketEvents, rpc.BriefMarketEventRow{BriefRowState: rankState(status), Kind: []string{"earnings", "halt", "ssr", "borrow"}[i%4]})
	}
	var riskLimits rpc.BriefRiskSection
	riskLimits.Capital.BriefRowState = st(rpc.BriefReadyRowCapital)
	riskLimits.Latch.BriefRowState = st(rpc.BriefReadyRowLatch)
	riskLimits.PolicyDrift.BriefRowState = st(rpc.BriefReadyRowPolicyDrift)
	var portfolio rpc.BriefPortfolioSection
	portfolio.PremiumAtRisk.BriefRowState = st(rpc.BriefReadyRowPremiumAtRisk)
	portfolio.HedgeCost.BriefRowState = st(rpc.BriefReadyRowHedgeCost)
	var process rpc.BriefProcessSection
	if pulse != "" {
		process.MonthlyPulse = &rpc.BriefMonthlyPulseRow{Status: pulse}
	}
	proposals := rpc.BriefReadyProposalsRow{BriefRowState: st(rpc.BriefReadyRowProposals)}
	return composeBriefReady(market, calendar, riskLimits, portfolio, process, proposals)
}

func allReadyStates(status string) map[string]string {
	out := map[string]string{}
	for _, key := range []string{
		rpc.BriefReadyRowRegime, rpc.BriefReadyRowBreadth, rpc.BriefReadyRowGamma, rpc.BriefReadyRowStress,
		rpc.BriefReadyRowSession, rpc.BriefReadyRowCapital, rpc.BriefReadyRowLatch, rpc.BriefReadyRowPremiumAtRisk,
		rpc.BriefReadyRowHedgeCost, rpc.BriefReadyRowProposals, rpc.BriefReadyRowPolicyDrift,
	} {
		out[key] = status
	}
	return out
}

// Owner decision 2026-09-26: attention rows first with capital (the drawdown
// tier) and the latch leading, then degraded, unavailable and ok rows.
func TestBriefReadyRankedFollowsTheOwnersSeverityOrder(t *testing.T) {
	states := allReadyStates(rpc.BriefStatusOK)
	states[rpc.BriefReadyRowRegime] = rpc.BriefStatusDegraded
	states[rpc.BriefReadyRowGamma] = rpc.BriefStatusUnavailable
	states[rpc.BriefReadyRowStress] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowProposals] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowHedgeCost] = rpc.BriefStatusDegraded
	states[rpc.BriefReadyRowLatch] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowCapital] = rpc.BriefStatusAttention
	ready := rankReady(states, []string{rpc.BriefStatusOK, rpc.BriefStatusAttention}, rpc.BriefMonthlyPulseBlocked)

	want := []string{
		"capital", "latch", "stress", "market_events", "proposals", // attention
		"regime", "hedge_cost", "monthly_pulse", // degraded
		"gamma",                                                 // unavailable
		"breadth", "session", "premium_at_risk", "policy_drift", // ok
	}
	if !slices.Equal(ready.Ranked, want) {
		t.Fatalf("ranked = %v\nwant     %v", ready.Ranked, want)
	}
}

func TestBriefReadyRankedCapitalAndLatchLeadEveryGroup(t *testing.T) {
	states := allReadyStates(rpc.BriefStatusOK)
	states[rpc.BriefReadyRowStress] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowRegime] = rpc.BriefStatusDegraded
	states[rpc.BriefReadyRowCapital] = rpc.BriefStatusDegraded
	ready := rankReady(states, nil, "")
	want := []string{
		"stress",            // the only attention row
		"capital", "regime", // degraded: the drawdown tier leads
		"latch", "breadth", "gamma", "session", "market_events", "premium_at_risk", "hedge_cost", "proposals", "policy_drift",
	}
	if !slices.Equal(ready.Ranked, want) {
		t.Fatalf("ranked = %v\nwant     %v", ready.Ranked, want)
	}
}

func TestBriefReadyRankedListsEveryPresentRowOnce(t *testing.T) {
	states := allReadyStates(rpc.BriefStatusOK)
	// A status this build does not know still needs eyes: it ranks with
	// degraded, never with ok and never above a risk condition.
	states[rpc.BriefReadyRowBreadth] = "surprise"
	states[rpc.BriefReadyRowSession] = rpc.BriefStatusAttention
	withPulse := rankReady(states, nil, rpc.BriefMonthlyPulseNotDue)
	if got := withPulse.Ranked[:2]; !slices.Equal(got, []string{"session", "breadth"}) {
		t.Fatalf("ranked head = %v, want [session breadth]", got)
	}
	if len(withPulse.Ranked) != 13 || withPulse.Ranked[12] != rpc.BriefReadyRowMonthlyPulse {
		t.Fatalf("ranked = %v, want 13 keys with the current monthly pulse last", withPulse.Ranked)
	}
	seen := map[string]bool{}
	for _, key := range withPulse.Ranked {
		if seen[key] {
			t.Fatalf("ranked repeats %q: %v", key, withPulse.Ranked)
		}
		seen[key] = true
	}
	// No monthly pulse row on the section: no key for it either. An empty
	// event list still names the always-present market_events field.
	without := rankReady(states, nil, "")
	if slices.Contains(without.Ranked, rpc.BriefReadyRowMonthlyPulse) || !slices.Contains(without.Ranked, rpc.BriefReadyRowMarketEvents) {
		t.Fatalf("ranked = %v, want market_events and no monthly_pulse", without.Ranked)
	}
}

func rankRules() *rpc.RulesResult {
	return &rpc.RulesResult{
		Rules: []risk.RuleRow{
			{ID: risk.RuleSingleNameExposure, Mode: risk.RuleModeAlert, Status: risk.RuleStatusAct},
			{ID: risk.RuleCashSellOnly, Mode: risk.RuleModeAlert, Status: risk.RuleStatusWatch},
			{ID: risk.RuleNetExposure, Mode: risk.RuleModeTrack, Status: risk.RuleStatusAct},
			{ID: risk.RuleExitDiscipline, Status: risk.RuleStatusAct}, // empty mode reads as alert
			{ID: risk.RuleWinnerTrim, Mode: risk.RuleModeOff, Status: risk.RuleStatusNotEvaluated},
		},
		// The Rulebook ranked exit discipline hardest.
		Ranked: []int{3, 0, 1, 2, 4},
	}
}

// The unified order: drawdown tier, latch, alert rules at act in the
// Rulebook's ranked order, then the other attention rows.
func TestBriefAttentionOrderInterleavesRulesAtAct(t *testing.T) {
	states := allReadyStates(rpc.BriefStatusOK)
	states[rpc.BriefReadyRowProposals] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowStress] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowLatch] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowCapital] = rpc.BriefStatusAttention
	states[rpc.BriefReadyRowRegime] = rpc.BriefStatusDegraded
	ready := rankReady(states, nil, "")

	want := []string{"ready.capital", "ready.latch", "rules.exit_discipline", "rules.single_name_exposure", "ready.stress", "ready.proposals"}
	if got := briefAttentionOrder(ready, rankRules()); !slices.Equal(got, want) {
		t.Fatalf("attention order = %v\nwant              %v", got, want)
	}

	// Without the latch and capital at attention the rules lead.
	states[rpc.BriefReadyRowCapital] = rpc.BriefStatusOK
	states[rpc.BriefReadyRowLatch] = rpc.BriefStatusDegraded
	ready = rankReady(states, nil, "")
	want = []string{"rules.exit_discipline", "rules.single_name_exposure", "ready.stress", "ready.proposals"}
	if got := briefAttentionOrder(ready, rankRules()); !slices.Equal(got, want) {
		t.Fatalf("attention order = %v\nwant              %v", got, want)
	}
}

func TestBriefAttentionOrderDegradesWithoutARanking(t *testing.T) {
	ready := rankReady(allReadyStates(rpc.BriefStatusOK), nil, "")

	// Nothing needs attention: an empty list on the wire, never null.
	got := briefAttentionOrder(ready, nil)
	raw, _ := json.Marshal(rpc.BriefResult{Ready: ready, AttentionOrder: got})
	if got == nil || !strings.Contains(string(raw), `"attention_order":[]`) || !strings.Contains(string(raw), `"ranked":["capital","latch",`) {
		t.Fatalf("quiet brief wire = %s", raw)
	}

	// A ranking that does not cover every row falls back to rulebook order.
	rules := rankRules()
	rules.Ranked = []int{3}
	want := []string{"rules.single_name_exposure", "rules.exit_discipline"}
	if got := briefAttentionOrder(ready, rules); !slices.Equal(got, want) {
		t.Fatalf("fallback order = %v, want %v", got, want)
	}
}
