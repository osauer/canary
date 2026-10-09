package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runPortfolioPlan(ctx context.Context, env *Env, jsonOut bool) int {
	var out rpc.PortfolioPlanResult
	if err := env.Conn.Call(ctx, rpc.MethodPortfolioPlan, nil, &out); err != nil {
		return fail(env, "portfolio plan: %v", err)
	}
	if jsonOut {
		return printJSON(env, out)
	}
	fmt.Fprintln(env.Stdout, "Portfolio plan · review only")
	riskReadLine(env, "Decision", out.State)
	riskReadLine(env, "Reason", out.Reason)
	if !out.ValidUntil.IsZero() {
		riskReadLine(env, "Mandate expires", out.ValidUntil.Format("2006-01-02 15:04 MST"))
	}
	if out.Regime != "" {
		riskReadLine(env, "Regime", out.Regime)
	}
	for _, r := range out.Targets {
		allocation := "allocation unmeasured"
		if r.StockPctNLV != nil {
			allocation = fmt.Sprintf("%.2f%% of NLV", *r.StockPctNLV)
		}
		if r.TargetPctNLV != nil {
			allocation += fmt.Sprintf("; target %.2f%%", *r.TargetPctNLV)
		}
		riskReadLine(env, r.Symbol, strings.ReplaceAll(r.Decision, "_", " ")+" · "+allocation)
		riskReadLine(env, "", r.Reason)
		if r.DesiredQuantity > 0 {
			riskReadLine(env, "Desired addition", fmt.Sprintf("%d shares before order checks", r.DesiredQuantity))
		}
	}
	for _, p := range out.Reductions {
		riskReadLine(env, "Existing review", fmt.Sprintf("%s %s %d · %s · %s", p.Action, p.Symbol, p.Quantity, p.OrderType, p.Reason))
	}
	for _, c := range out.Unassigned {
		riskReadLine(env, "No stock target", fmt.Sprintf("%s %s · contract %d", c.Symbol, c.Currency, c.ConID))
	}
	if out.Next != nil {
		if p := out.Next.Add; p != nil {
			riskReadLine(env, "Next review", fmt.Sprintf("BUY %s %d · limit %.2f %s · cost incl. fee %.2f %s", p.Contract.Symbol, p.Quantity, p.LimitPrice, p.Currency, p.Cost, p.Currency))
		} else {
			riskReadLine(env, "Next review", out.Next.Kind+" · "+out.Next.ProposalKey+" · revision "+out.Next.ProposalRevision)
		}
	}
	for _, b := range out.Blockers {
		riskReadLine(env, "Held", b.Message)
	}
	fmt.Fprintln(env.Stdout, "No money reserved or order authorised. Replan after each outcome.")
	return 0
}
