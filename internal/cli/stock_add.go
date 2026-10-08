package cli

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runAdd(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "add")
	price := fs.Float64("limit", 0, "maximum price per share in the stock currency")
	quantity := fs.Int("quantity", 0, "whole shares to add; 0 computes the maximum permitted")
	currency := fs.String("currency", "", "stock trading currency (required)")
	conID := fs.Int("con-id", 0, "exact broker stock identity when known")
	exchange := fs.String("exchange", "SMART", "broker venue")
	jsonOut := fs.Bool("json", false, "emit typed Add evidence")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || (fs.Arg(0) != "plan" && fs.Arg(0) != "preview") {
		return fail(env, "usage: canary add plan|preview SYMBOL --currency CCY --limit PRICE [--quantity N] [--con-id ID] [--json]")
	}
	p, err := rpc.NormalizeAddParams(rpc.AddParams{Contract: rpc.ContractParams{Symbol: fs.Arg(1), SecType: "STK", Currency: *currency, ConID: *conID, Exchange: *exchange}, LimitPrice: *price, Quantity: *quantity})
	if err != nil {
		return fail(env, "add: %v", err)
	}
	if fs.Arg(0) == "preview" {
		var out rpc.OrderPreviewResult
		if err := env.Conn.Call(ctx, rpc.MethodAddPreview, p, &out); err != nil {
			return fail(env, "add preview: %v", err)
		}
		if *jsonOut {
			return printJSON(env, out)
		}
		if out.Draft.Add != nil {
			renderAddPlan(env, rpc.AddPlanResult{StockAddPlan: out.Draft.Add.Plan, Contract: out.Draft.Contract, Currency: out.Draft.Contract.Currency, LimitPrice: out.Draft.LimitPrice})
		}
		renderOrderPreviewText(env, &out)
		return 0
	}
	var out rpc.AddPlanResult
	if err := env.Conn.Call(ctx, rpc.MethodAddPlan, p, &out); err != nil {
		return fail(env, "add plan: %v", err)
	}
	if *jsonOut {
		return printJSON(env, out)
	}
	renderAddPlan(env, out)
	return 0
}

func renderAddPlan(env *Env, p rpc.AddPlanResult) {
	riskReadLine(env, "Add", p.Contract.Symbol, p.Currency)
	if len(p.Blockers) > 0 {
		for _, b := range p.Blockers {
			riskReadLine(env, "Held", b.Message)
		}
		return
	}
	riskReadLine(env, "Position", fmt.Sprintf("%g → %g shares", p.Before, p.After))
	riskReadLine(env, "Add shares", fmt.Sprintf("%d (maximum %d)", p.Quantity, p.MaxQuantity))
	riskReadLine(env, "Limit", fmt.Sprintf("%.4f %s per share", p.LimitPrice, p.Currency))
	riskReadLine(env, "Cost incl. fee bound", fmt.Sprintf("%.2f %s", p.Cost, p.Currency))
	riskReadLine(env, "Cash after reserves", fmt.Sprintf("%.2f %s", p.CashAfter, p.Currency))
	label := map[string]string{"cash": "Available cash after reserves and fees", "stock_allocation": "Total stock allocation", "underlying_stock": "Stock allocation in this underlying", "order_limit": "Per-order limit", "risk_budget": "Portfolio risk budget"}[p.Binding]
	if label == "" {
		label = "Order quantity limit"
	}
	riskReadLine(env, "Sizing constraint", label)
	fmt.Fprintln(env.Stdout, "Planning evidence only. An exact preview and separate owner confirmation are required. Existing stops retain their reviewed quantity.")
}
