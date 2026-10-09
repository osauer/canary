package cli

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runAdd(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "add")
	price := fs.Float64("limit", 0, "maximum price per share in the stock currency")
	quantity := fs.Int("quantity", 0, "exact additional whole shares; choose this or --max")
	maximum := fs.Bool("max", false, "find the maximum supported addition for one order")
	currency := fs.String("currency", "", "stock trading currency (required)")
	conID := fs.Int("con-id", 0, "exact broker stock identity when known")
	exchange := fs.String("exchange", "SMART", "broker venue")
	jsonOut := fs.Bool("json", false, "emit typed Add evidence")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || (fs.Arg(0) != "plan" && fs.Arg(0) != "preview") {
		return fail(env, "usage: canary add plan|preview SYMBOL --currency CCY --limit PRICE (--quantity N | --max) [--con-id ID] [--json]")
	}
	p, err := rpc.NormalizeAddParams(rpc.AddParams{Contract: rpc.ContractParams{Symbol: fs.Arg(1), SecType: "STK", Currency: *currency, ConID: *conID, Exchange: *exchange}, LimitPrice: *price, Quantity: *quantity, Max: *maximum})
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
	riskReadLine(env, "Limit", fmt.Sprintf("%.4f %s per share", p.LimitPrice, p.Currency))
	for _, w := range p.Warnings {
		riskReadLine(env, "Warning · confirmation required", w.Message)
	}
	for _, b := range p.Blockers {
		riskReadLine(env, "Held · "+addBlockerKindLabel(b.Kind), b.Message)
	}
	if p.Quantity > 0 && len(p.Blockers) == 0 {
		riskReadLine(env, "Add shares", fmt.Sprintf("%d", p.Quantity))
		if p.MaximumKnown {
			riskReadLine(env, "Maximum for this order", fmt.Sprintf("%d shares; larger candidates ruled out", p.MaxQuantity))
		}
		riskReadLine(env, "Cost incl. fee bound", fmt.Sprintf("%.2f %s", p.Cost, p.Currency))
		riskReadLine(env, "Cash after reserves", fmt.Sprintf("%.2f %s", p.CashAfter, p.Currency))
	}
	if p.SupportedQuantity > 0 {
		riskReadLine(env, "Exact quantity checked", fmt.Sprintf("%d shares; maximum not established", p.SupportedQuantity))
	}
	if len(p.Allowances) > 0 {
		riskReadLine(env, "Position", fmt.Sprintf("%g → %g shares", p.Before, p.After))
		if p.Sizing == "max" {
			riskReadLine(env, "Sizing", "Explicit maximum for one order")
		} else {
			riskReadLine(env, "Requested addition", fmt.Sprintf("%d shares", p.RequestedQuantity))
		}
		riskReadLine(env, "Stock allocation room", fmt.Sprintf("%.0f additional shares before cash, risk and order checks", p.AllocationRoom))
		riskReadLine(env, "Order allowance", fmt.Sprintf("up to %d shares before exact broker costs and margin", p.OrderUpperBound))
		for _, a := range p.Allowances {
			riskReadLine(env, addAllowanceLabel(a.Code), fmt.Sprintf("limit %.2f; used %.2f; remaining %.2f %s (%.0f shares)", a.Limit, a.Used, a.Remaining, p.BaseCurrency, a.Shares))
		}
	}
	if p.Cash != nil {
		c := p.Cash
		riskReadLine(env, "Cash calculation", fmt.Sprintf("%.2f available − %.2f committed − %.2f currency float − %.2f account reserve = %.2f %s spendable", c.Available, c.Committed, c.CurrencyFloat, c.ReserveInCurrency, c.Spendable, p.Currency))
		riskReadLine(env, "Account reserve", fmt.Sprintf("%.2f %s required; %.2f %s available outside this currency after floats and commitments", c.AccountReserveBase, p.BaseCurrency, c.OtherReserveFundingBase, p.BaseCurrency))
	}
	if p.Quantity > 0 && len(p.Blockers) == 0 {
		riskReadLine(env, "Total stock allocation", fmt.Sprintf("%.2f%% → %.2f%% of account value", p.StockPctBefore, p.StockPctAfter))
		riskReadLine(env, "Underlying stock allocation", fmt.Sprintf("%.2f%% → %.2f%% of account value", p.UnderlyingPctBefore, p.UnderlyingPctAfter))
	}
	if p.Binding != "" {
		riskReadLine(env, "Sizing constraint before broker simulation", addAllowanceLabel(p.Binding))
	}
	for _, c := range p.RiskChecks {
		riskReadLine(env, c.Title+" before", c.BeforeEvidence)
		if c.AfterEvidence != "" {
			riskReadLine(env, c.Title+" after", c.AfterEvidence)
		}
	}
	if p.Protection != nil {
		c := p.Protection
		riskReadLine(env, "Working stop instructions", fmt.Sprintf("%g shares; shares without these instructions %g → %g", c.StopQuantity, c.UncoveredBefore, c.UncoveredAfter))
		if c.PendingQuantity > 0 {
			riskReadLine(env, "Other pending purchases", fmt.Sprintf("%g shares; not included in the filled holding or stop coverage", c.PendingQuantity))
		}
		riskReadLine(env, "Protection review", "Stop instructions do not guarantee an exit or loss limit. Existing stops retain their reviewed quantity; changing protection requires a separate review.")
	}
	riskReadLine(env, "Order review", "Planning evidence only. An exact preview and separate owner confirmation are required.")
}

func addAllowanceLabel(code string) string {
	label := map[string]string{"cash": "Cash before this order's fee", "stock_allocation": "Total stock allowance", "underlying_stock": "Underlying stock allowance", "order_limit": "Per-order limit", "risk_budget": "Portfolio risk budget"}[code]
	if label == "" {
		return "Order quantity limit"
	}
	return label
}

func addBlockerKindLabel(kind string) string {
	switch kind {
	case "policy":
		return "Policy approval"
	case "capacity":
		return "Limit reached"
	case "unsupported":
		return "Not supported in this stage"
	default:
		return "More evidence needed"
	}
}
