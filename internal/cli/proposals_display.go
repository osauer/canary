package cli

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func renderDisplayBlockers(env *Env, blockers []rpc.TradingBlocker) {
	for _, b := range blockers {
		displayLine(env, "  Blocked: "+b.Code+" · "+b.Message, env.yellow)
		if b.Action != "" {
			displayLine(env, "  Next: "+b.Action, nil)
		}
	}
}

func renderProposalsSummary(env *Env, snap *rpc.TradeProposalSnapshot) {
	displayLine(env, "Protection proposals", env.bold)
	displayLine(env, fmt.Sprintf("%d actionable / %d total · %d queued for the open", snap.Counts.Actionable, snap.Counts.Total, snap.Counts.Queued), nil)
	renderDisplayBlockers(env, snap.Blockers)
	if budget := formatProposalBudgetStatus(snap.BudgetReduction); budget != "" {
		displayRow(env, env.Stdout, "Budget", budget)
	}
	// Preserve producer order and keep the cash sweep and currency leveling
	// distinct from protection: protection first, then each under its own
	// heading.
	group := func(p rpc.TradeProposal) int {
		switch p.Bucket {
		case rpc.TradeProposalBucketCashSweep:
			return 1
		case rpc.TradeProposalBucketCurrencyLeveling:
			return 2
		}
		return 0
	}
	var groupRows [3]int
	for _, p := range snap.Proposals {
		groupRows[group(p)]++
	}
	for g := range 3 {
		switch {
		case g == 1 && (snap.CashSweep != nil || groupRows[1] > 0):
			fmt.Fprintln(env.Stdout)
			displayLine(env, "Cash sweep · "+formatCashSweepStatus(snap.CashSweep, groupRows[1]), env.bold)
		case g == 2 && (snap.CurrencyLeveling != nil || groupRows[2] > 0):
			fmt.Fprintln(env.Stdout)
			displayLine(env, "Currency leveling · "+formatCurrencyLevelingStatus(snap.CurrencyLeveling, groupRows[2]), env.bold)
			if st := snap.CurrencyLeveling; st != nil {
				for _, c := range st.Currencies {
					displayLine(env, fmt.Sprintf("  %s %s · %s", c.Currency, strings.ReplaceAll(c.State, "_", " "), formatCurrencyLevelingCurrency(c)), nil)
				}
				for _, b := range st.Bundles {
					displayLine(env, "  "+formatCurrencyLevelingBundle(b, st.BaseCurrency), nil)
				}
			}
		}
		var rows []rpc.TradeProposal
		for _, p := range snap.Proposals {
			if group(p) == g {
				rows = append(rows, p)
			}
		}
		if g == 2 {
			rows = currencyLevelingSendOrder(snap.CurrencyLeveling, rows)
		}
		for _, p := range rows {
			state, style := "review", env.bold
			if len(p.Blockers) > 0 || p.State == rpc.TradeProposalStateBlocked {
				state, style = "blocked", env.yellow
			}
			if p.Queued != nil {
				state, style = "queued · executor only", env.dim
			}
			if p.Shadow {
				state, style = "shadow · observation only; preview and submit refuse", env.dim
			}
			fmt.Fprintln(env.Stdout)
			unit := "units"
			switch nonEmpty(p.SecType, p.Contract.SecType) {
			case "STK":
				unit = "shares"
			case "OPT":
				unit = "contracts"
			}
			if p.CashSweep != nil {
				unit = nonEmpty(p.CashSweep.QuantityUnit, unit)
			}
			if p.CurrencyLeveling != nil {
				unit = nonEmpty(p.CurrencyLeveling.PairSymbol, unit)
			}
			displayLine(env, fmt.Sprintf("%s · %s %d %s · %s", nonEmpty(queuedContractLabel(p.Contract), p.Symbol), p.Action, p.Quantity, unit, state), style)
			displayLine(env, "  "+p.Key+" · "+p.Bucket+" · "+p.OrderType, nil)
			if p.Queued != nil {
				displayLine(env, "  Queue: "+p.Queued.State+" · window "+displayTime(p.Queued.NotBefore)+" to "+displayTime(p.Queued.NotAfter), nil)
			}
			if unit := formatProposalUnit(p.Unit); unit != "" {
				displayLine(env, "  "+unit, nil)
			}
			if p.Trail != nil {
				displayLine(env, "  Trail: "+formatOrderTrail(p.Trail), nil)
			}
			if p.Reason != "" {
				displayLine(env, "  "+p.Reason, nil)
			}
			if automatic := strings.TrimSpace(formatProposalAutomaticColumn(p.Automatic)); automatic != "" {
				displayLine(env, "  "+automatic, nil)
			}
			if readiness := formatProposalReadiness(p.Readiness); readiness != "" {
				displayLine(env, "  "+readiness, env.yellow)
			}
			renderDisplayBlockers(env, p.Blockers)
		}
	}
	displayLine(env, "Full risk evidence, sizing and funding: canary proposals list --details", env.dim)
	displayLine(env, "Review list only; exact preview and submit eligibility remain separate.", env.dim)
}
