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
