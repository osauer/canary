package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// budgetPlanIndent aligns the governor's candidates and plan under the value
// column of the Budget header row.
const budgetPlanIndent = "               "

// renderProposalBudgetPlan prints the premium budget governor's ranked
// candidates and its whole plan under the Budget header (amendment
// 2026-09-30): what it would sell first and why, and every order the
// measured excess needs, with the cycle each can go in.
func renderProposalBudgetPlan(out io.Writer, st *rpc.TradeProposalBudgetStatus) {
	if st == nil {
		return
	}
	for i, c := range st.Candidates {
		fmt.Fprintf(out, "%s%-11s%d. %s  %d ct × %s  %s\n", budgetPlanIndent, budgetPlanLabel(i, "candidates"), c.Rank,
			queuedContractLabel(c.Contract), c.Contracts, formatProposalMoney(c.UnitValueBase, st.BaseCurrency), c.Why)
	}
	for i, o := range st.Plan {
		fmt.Fprintf(out, "%s%-11s%d. sell %d %s · raises %s · cycle %d · rank %d\n", budgetPlanIndent, budgetPlanLabel(i, "plan"), i+1,
			o.Contracts, queuedContractLabel(o.Contract), formatProposalMoney(o.RaisesBase, st.BaseCurrency), o.Cycle, o.Rank)
	}
}

func budgetPlanLabel(i int, label string) string {
	if i > 0 {
		return ""
	}
	return label
}

// formatProposalBudgetGate names why the governor runs in shadow when its
// mode does not, and a ranking made without a Rulebook result.
func formatProposalBudgetGate(st *rpc.TradeProposalBudgetStatus) []string {
	var parts []string
	if st.ShadowReason != "" {
		parts = append(parts, "shadow: "+strings.ReplaceAll(st.ShadowReason, "_", " "))
	}
	if st.RankingWithoutRulebook {
		parts = append(parts, "ranked without a Rulebook result")
	}
	return parts
}
