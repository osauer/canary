package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runAdd(ctx context.Context, env *Env, args []string) int {
	// Route before parsing so --help belongs to the selected action. Skip
	// value-taking flags: a stock currency or venue is not an action name.
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && isValueFlag(arg) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		switch arg {
		case "market":
			return runAddMarket(ctx, env, append(append([]string{}, args[:i]...), args[i+1:]...))
		case "call-plan":
			return runAddCallPlan(ctx, env, append(append([]string{}, args[:i]...), args[i+1:]...))
		}
		break
	}
	fs := flagSet(env, "add")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		helpLine(env, "", "plan checks a stock addition or opening; preview creates an exact order review for separate owner confirmation. Neither places an order.", nil)
		helpLine(env, "", "market reads stock quotes and discovers or refreshes calls; call-plan sizes an exact call using broker fee evidence. Both read one JSON object from stdin. Run canary add market --help or canary add call-plan --help for request fields and examples.", nil)
	}
	stop := fs.Float64("stop-price", 0, "fixed attached stop below entry; added shares only, GTC regular session")
	price := fs.Float64("limit", 0, "maximum price per share in the stock currency")
	quantity := fs.Int("quantity", 0, "exact additional whole shares; choose this or --max")
	maximum := fs.Bool("max", false, "find the maximum supported addition for one order")
	currency := fs.String("currency", "", "stock trading currency (required)")
	conID := fs.Int("con-id", 0, "exact broker stock identity when known")
	exchange := fs.String("exchange", "SMART", "broker venue")
	adaptive := fs.String("adaptive-priority", "", "IBKR Adaptive priority for the limit: Normal, Patient, or Urgent")
	jsonOut := fs.Bool("json", false, "emit typed Add evidence")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 2 || (fs.Arg(0) != "plan" && fs.Arg(0) != "preview") {
		return fail(env, "usage: canary add plan|preview SYMBOL --currency CCY --limit PRICE (--quantity N | --max) [--con-id ID] [--json]")
	}
	p, err := rpc.NormalizeAddParams(rpc.AddParams{StopPrice: *stop, AdaptivePriority: *adaptive, Contract: rpc.ContractParams{Symbol: fs.Arg(1), SecType: "STK", Currency: *currency, ConID: *conID, Exchange: *exchange}, LimitPrice: *price, Quantity: *quantity, Max: *maximum})
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
			renderAddPlan(env, rpc.AddPlanResult{StockAddPlan: out.Draft.Add.Plan, Contract: out.Draft.Contract, Currency: out.Draft.Contract.Currency, LimitPrice: out.Draft.LimitPrice, StopPrice: *stop})
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
	if p.StopPrice > 0 {
		riskReadLine(env, "Attached fixed stop", fmt.Sprintf("%.4f %s; added shares only; GTC regular session; held until full entry fill", p.StopPrice, p.Currency))
	}
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

// runAddMarket adapts one bounded read-only request; stdin carries no authority.
func runAddMarket(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "add market", "Read stock quotes and discover or refresh exact long calls or protective puts; no orders placed", "canary add market [--json] < request.json")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		renderAddRequestHelp(env, []string{
			"Stdin: one JSON object; unknown fields and extra objects are rejected. Output is always JSON, with or without --json.",
			"underlying (required): stock contract with symbol, sec_type STK, currency and exchange SMART; con_id is optional when known.",
			"right (optional): C (default) for calls, P for protective puts. Puts come back in puts with put_cover: the shares held, the shares long puts cover and the contracts that still fit.",
			"discover (optional boolean): discover candidates for USD stocks, one per horizon of the opportunity policy's [add] table. selected (optional contract): refresh one exact contract returned by discovery.",
			"refresh (optional array): {horizon_days, contract} objects, one per horizon of that side at most, with unique horizons from the [add] table (calls 14, 30, 60 and 120 by default; puts 30, 60, 90 and 120). Do not combine refresh with discover. selected and refresh contracts must be exact USD SMART options for the underlying with the request's right, con_id, expiry, strike, multiplier 100 and trading_class.",
			"Options require USD. Read option_mode and each row's planning or unavailable evidence: planning prices are not executable quotes. A row's delta_band is written without the sign; a put's quote delta keeps it. Use the returned exact contract for call-plan.",
		}, `canary add market --json <<'JSON'
{
  "underlying": {
    "symbol": "SYNA",
    "sec_type": "STK",
    "currency": "USD",
    "exchange": "SMART"
  },
  "discover": true
}
JSON`)
	}
	_ = fs.Bool("json", false, "emit typed market evidence")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || env.Stdin == nil {
		return fail(env, "add market reads one JSON request from stdin")
	}
	d := json.NewDecoder(io.LimitReader(env.Stdin, 8193))
	d.DisallowUnknownFields()
	var p rpc.AddMarketParams
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return fail(env, "invalid Add market request")
	}
	var out rpc.AddMarketResult
	if err := env.Conn.Call(ctx, rpc.MethodAddMarket, p, &out); err != nil {
		return fail(env, "add market: %v", err)
	}
	return printJSON(env, out)
}

func runAddCallPlan(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "add call-plan", "Size an exact long call or protective put using broker fee evidence; no orders placed", "canary add call-plan [--json] < request.json")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		renderAddRequestHelp(env, []string{
			"Stdin: one JSON object; unknown fields and extra objects are rejected. Output is always JSON, with or without --json.",
			"contract (required): copy the exact contract returned by add market. It must be a USD SMART option: sec_type OPT, right C or P, multiplier 100, positive con_id, trading_class and expiry YYYYMMDD more than seven days away; retain symbol and strike.",
			"A put is sized within the held shares it may still protect; cover_capped says that held the size.",
			"limit_price (required): positive premium per underlying share in USD. budget (required): positive total USD debit including the broker fee bound, at most 1000000000.",
			"round_size (optional boolean, default false): round to the nearest five contracts, retaining one or two. Rounding can exceed budget; inspect over_budget, debit, quantity and fee_bound in the result.",
			"The options session must be open. Sizing returns no submission token and reserves no money. An exact order preview and separate owner confirmation are required to place an order.",
		}, `canary add call-plan --json <<'JSON'
{
  "contract": {
    "symbol": "SYNA",
    "con_id": 10001,
    "sec_type": "OPT",
    "currency": "USD",
    "exchange": "SMART",
    "expiry": "20300118",
    "strike": 100,
    "right": "C",
    "multiplier": 100,
    "trading_class": "SYNA"
  },
  "limit_price": 5.5,
  "budget": 5000,
  "round_size": false
}
JSON`)
	}
	_ = fs.Bool("json", false, "emit checked call size")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 || env.Stdin == nil {
		return fail(env, "add call-plan reads one JSON request from stdin")
	}
	d := json.NewDecoder(io.LimitReader(env.Stdin, 8193))
	d.DisallowUnknownFields()
	var p rpc.AddCallPlanParams
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return fail(env, "invalid call sizing request")
	}
	var out rpc.AddCallPlanResult
	if err := env.Conn.Call(ctx, rpc.MethodAddCallPlan, p, &out); err != nil {
		return fail(env, "add call-plan: %v", err)
	}
	return printJSON(env, out)
}

func renderAddRequestHelp(env *Env, paragraphs []string, example string) {
	for _, paragraph := range paragraphs {
		helpLine(env, "", paragraph, nil)
		fmt.Fprintln(env.Stdout)
	}
	helpLine(env, "", "Example (synthetic identity; replace with your resolved contract):", nil)
	fmt.Fprintln(env.Stdout, example)
}
