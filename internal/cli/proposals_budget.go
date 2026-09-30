package cli

import (
	"fmt"
	"io"

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

// formatProposalBudgetGate names the advisory review state of the Rulebook
// limits a rulebook-basis governor sells against, when they are not a
// reviewed owner file, and a ranking made without a Rulebook result.
func formatProposalBudgetGate(st *rpc.TradeProposalBudgetStatus) []string {
	var parts []string
	if review := budgetRulebookReviewText(st.RulebookReview); review != "" {
		parts = append(parts, review)
	}
	if st.RankingWithoutRulebook {
		parts = append(parts, "ranked without a Rulebook result")
	}
	return parts
}

// budgetRulebookReviewText is the header's word for a review state; empty
// when the limits are a reviewed owner file or the basis reads none.
func budgetRulebookReviewText(review string) string {
	switch review {
	case rpc.BudgetRulebookUnreviewed:
		return "Rulebook limits: Canary's defaults, not yet reviewed"
	case rpc.BudgetRulebookNoFile:
		return "Rulebook limits: no Rulebook policy file; compiled defaults"
	case rpc.BudgetRulebookDrift:
		return "Rulebook limits: the file on disk is not the one in force"
	case rpc.BudgetRulebookError:
		return "Rulebook limits: the file could not be read"
	}
	return ""
}
