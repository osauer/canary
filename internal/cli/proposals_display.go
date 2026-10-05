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
	// Preserve producer order and keep the cash sweep distinct from protection.
	sweepCount := 0
	for _, p := range snap.Proposals {
		if p.Bucket == rpc.TradeProposalBucketCashSweep {
			sweepCount++
		}
	}
	for _, sweep := range []bool{false, true} {
		if sweep && (snap.CashSweep != nil || sweepCount > 0) {
			fmt.Fprintln(env.Stdout)
			displayLine(env, "Cash sweep · "+formatCashSweepStatus(snap.CashSweep, sweepCount), env.bold)
		}
		for _, p := range snap.Proposals {
			if (p.Bucket == rpc.TradeProposalBucketCashSweep) != sweep {
				continue
			}
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
