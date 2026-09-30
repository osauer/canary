package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Shadow rows sit under their own heading after the ordinary rows, the
// heading says a preview refuses them, the budget line names cap, measured
// value, excess and order, and the header carries the governor's status.
func TestRenderProposalsListsShadowRowsUnderTheirOwnHeading(t *testing.T) {
	lineExcess, totalExcess, measured := 4500.0, 18000.0, 56.0
	snap := &rpc.TradeProposalSnapshot{
		Revision: "rev-1", PolicyID: "protection-mvp", PolicyVersion: 9,
		Counts: rpc.TradeProposalCounts{Total: 2, Actionable: 1, BudgetReduction: 1, BudgetReductionShadow: 1},
		BudgetReduction: &rpc.TradeProposalBudgetStatus{Mode: rpc.BudgetReductionModeShadow, Shadow: true, State: rpc.BudgetStateOverBudget,
			PremiumAtRiskPctOfRiskCapital: 40, PerLinePctOfRiskCapital: 15, MeasuredPctOfRiskCapital: &measured, ProtectionLegs: 2},
		Proposals: []rpc.TradeProposal{
			{Key: "budget_reduction:1", Bucket: rpc.TradeProposalBucketBudgetReduction, Action: "SELL", Quantity: 4, Symbol: "AAA", OrderType: "LMT",
				Reason: "premium line is 24.0% of declared risk capital", Shadow: true, NeverSkipVeto: true,
				Budget: &rpc.TradeProposalBudget{Cap: "per_line+total", PerLinePctOfRiskCapital: 15, PremiumAtRiskPctOfRiskCapital: 40, LinePctOfRiskCapital: 24,
					LineExcessBase: &lineExcess, TotalPctOfRiskCapital: 56, TotalExcessBase: &totalExcess, Order: 2, ContractsPerLine: 3, ContractsTotal: 1, BaseCurrency: "EUR"},
				Blockers: []rpc.TradingBlocker{{Code: "shadow_mode", Message: "budget reduction runs in shadow mode", Action: "Set mode = \"active\"."}}},
			{Key: "trailing_stop:2", Bucket: rpc.TradeProposalBucketTrailingStop, Action: "SELL", Quantity: 100, Symbol: "TEST", OrderType: "TRAIL", Reason: "protective stop"},
		},
	}
	var buf bytes.Buffer
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	out := buf.String()
	ordinary := strings.Index(out, "trailing_stop:2")
	heading := strings.Index(out, "Shadow (budget reduction)  1 row listed for observation; preview and submit refuse them")
	shadow := strings.Index(out, "budget_reduction:1")
	if ordinary < 0 || heading < 0 || shadow < 0 || !(ordinary < heading && heading < shadow) {
		t.Fatalf("rows out of order or heading missing:\n%s", out)
	}
	for _, want := range []string{
		"[shadow]",
		"Budget:      line 24.0% > 15% cap (excess € 4,500.00, 3 ct) · total 56.0% > 40% cap (excess € 18,000.00), 2nd in order, 1 ct",
		"shadow · over budget · premium 56.0% of risk capital (caps 40% total, 15% per line) · 2 protection legs left out",
		"shadow_mode",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// No shadow rows: no heading, and the header stays as it was.
	snap.Proposals = snap.Proposals[1:]
	snap.BudgetReduction = nil
	buf.Reset()
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	if strings.Contains(buf.String(), "Shadow (") || strings.Contains(buf.String(), "Budget ") {
		t.Fatalf("heading or status rendered without shadow rows:\n%s", buf.String())
	}
	// The gated states read as plain words with their reason.
	gated := formatProposalBudgetStatus(&rpc.TradeProposalBudgetStatus{Mode: "shadow", State: rpc.BudgetStateNotLatched, Reason: "the drawdown block tier is neither latched nor breached"})
	if gated != "shadow · not latched · the drawdown block tier is neither latched nor breached" {
		t.Fatalf("gated status = %q", gated)
	}
}

// Under the Budget header the text names the advisory review state of the
// Rulebook limits, the ranked candidates with their why, and every order of
// the plan with its cycle.
func TestRenderProposalsPrintsTheBudgetCandidatesAndPlan(t *testing.T) {
	aaa := rpc.ContractParams{ConID: 701, Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C"}
	bbb := rpc.ContractParams{ConID: 702, Symbol: "BBB", SecType: "OPT", Expiry: "20261218", Strike: 100, Right: "C"}
	snap := &rpc.TradeProposalSnapshot{
		Revision: "rev-1", PolicyID: "protection-mvp", PolicyVersion: 9,
		BudgetReduction: &rpc.TradeProposalBudgetStatus{Mode: rpc.BudgetReductionModeActive, RulebookReview: rpc.BudgetRulebookUnreviewed,
			State: rpc.BudgetStateOverBudget, Basis: rpc.BudgetBasisRulebook, BaseCurrency: "EUR",
			Candidates: []rpc.TradeProposalBudgetCandidate{
				{Rank: 1, Contract: aaa, Contracts: 4, UnitValueBase: 2000, Relief: []string{"option_line_premium", "exit_discipline"}, Why: "offends 2 open rules; 50% time value; unrealised −500"},
				{Rank: 2, Contract: bbb, Contracts: 4, UnitValueBase: 2000, Relief: []string{"extrinsic_budget"}, Why: "offends 1 open rule; 100% time value; unrealised −100"},
			},
			Plan: []rpc.TradeProposalBudgetPlanOrder{
				{Rank: 1, Contract: aaa, Contracts: 2, RaisesBase: 4000, Cycle: 1},
				{Rank: 1, Contract: aaa, Contracts: 2, RaisesBase: 4000, Cycle: 2},
				{Rank: 2, Contract: bbb, Contracts: 2, RaisesBase: 4000, Cycle: 1},
			}},
	}
	var buf bytes.Buffer
	renderProposalsText(&Env{Stdout: &buf, Stderr: &buf}, snap)
	out := buf.String()
	for _, want := range []string{
		"active · Rulebook limits: Canary's defaults, not yet reviewed · over budget",
		"               candidates 1. AAA 20261218 100 C  4 ct × € 2,000.00  offends 2 open rules; 50% time value; unrealised −500\n",
		"                          2. BBB 20261218 100 C  4 ct × € 2,000.00  offends 1 open rule; 100% time value; unrealised −100\n",
		"               plan       1. sell 2 AAA 20261218 100 C · raises € 4,000.00 · cycle 1 · rank 1\n",
		"                          2. sell 2 AAA 20261218 100 C · raises € 4,000.00 · cycle 2 · rank 1\n",
		"                          3. sell 2 BBB 20261218 100 C · raises € 4,000.00 · cycle 1 · rank 2\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	// A ranking made without a Rulebook result says so in the header, and a
	// reviewed owner file adds nothing.
	snap.BudgetReduction.RulebookReview, snap.BudgetReduction.RankingWithoutRulebook = rpc.BudgetRulebookReviewed, true
	if got := formatProposalBudgetStatus(snap.BudgetReduction); got != "active · ranked without a Rulebook result · over budget" {
		t.Fatalf("header = %q", got)
	}
	for review, want := range map[string]string{
		rpc.BudgetRulebookNoFile: "no Rulebook policy file; compiled defaults",
		rpc.BudgetRulebookDrift:  "the file on disk is not the one in force",
		rpc.BudgetRulebookError:  "the file could not be read",
	} {
		snap.BudgetReduction.RulebookReview = review
		if got := formatProposalBudgetStatus(snap.BudgetReduction); !strings.Contains(got, "active · Rulebook limits: "+want+" · ") {
			t.Fatalf("%s header = %q", review, got)
		}
	}
}
