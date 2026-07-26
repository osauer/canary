package cli

import (
	"context"
	"fmt"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func runSize(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "size")
	symbol := fs.String("symbol", "", "underlying symbol (required)")
	entry := fs.Float64("entry", 0, "planned entry price per share (required)")
	stop := fs.Float64("stop", 0, "planned stop price per share (required)")
	target := fs.Float64("target", 0, "optional take-profit price; enables R-multiple and breakeven win-rate output")
	riskPct := fs.Float64("risk-pct", 1.0, "percent of NLV to risk on this trade")
	side := fs.String("side", "long", "trade direction: long | short")
	lot := fs.Int("lot", 1, "round shares down to this multiple")
	fx := fs.Float64("fx", 1.0, "quote-currency units per 1 base-currency unit (e.g. 1.085 for EUR→USD)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	plan := risk.SizeInput{
		Symbol:  *symbol,
		Side:    *side,
		Entry:   *entry,
		Stop:    *stop,
		Target:  *target,
		RiskPct: *riskPct,
		Lot:     *lot,
		FX:      *fx,
	}
	if err := risk.ValidateSizePlan(plan); err != nil {
		return fail(env, "size: %v", err)
	}

	var acct rpc.AccountResult
	if err := env.Conn.Call(ctx, rpc.MethodAccountSummary, nil, &acct); err != nil {
		return fail(env, "size: %v", err)
	}

	in := plan
	in.NLV = acct.NetLiquidation
	in.BuyingPower = acct.BuyingPower
	in.Currency = acct.BaseCurrency
	res, err := risk.ComputeSize(in)
	if err != nil {
		return fail(env, "size: %v", err)
	}

	if *jsonOut {
		return printJSON(env, res)
	}
	return renderSizeText(env, &res)
}

func renderSizeText(env *Env, r *risk.SizeResult) int {
	out := env.Stdout
	ccyBase := nonEmpty(r.BaseCurrency, "USD")
	fmt.Fprintln(out)
	header := fmt.Sprintf("Size  %s · %s · entry %.2f · stop %.2f", r.Symbol, r.Side, r.Entry, r.Stop)
	if r.Target != nil {
		header += fmt.Sprintf(" · target %.2f", *r.Target)
	}
	fmt.Fprintln(out, header)
	fmt.Fprintln(out)

	// labelWidth covers the widest label ("Risk in quote ccy" = 17,
	// "Max gain at target" = 18, "Breakeven win rate" = 18); valueWidth
	// fits a 7-digit notional with grouping ("$ 9,999,999.99" = 14).
	const (
		labelWidth = 20
		valueWidth = 14
	)
	label := func(s string) string {
		return fmt.Sprintf("  %-*s", labelWidth, s)
	}
	// moneyBase stamps the base-currency symbol; right for NLV, RiskBase,
	// RewardBase. moneyQuote is for values denominated in the trade's
	// quote currency (RiskQuote, PerShareRisk, Notional, MaxLoss,
	// RewardQuote): when FX = 1 the quote currency IS the base currency so
	// the symbol is correct; when FX ≠ 1 the symbol would lie (the value
	// is in some other currency the daemon doesn't carry on SizeResult),
	// so we drop it and let the "Risk in quote ccy (fx …)" header do the
	// labelling.
	moneyBase := func(v float64) string {
		return env.formatMoneyNegCcyRight(v, ccyBase, valueWidth)
	}
	moneyQuote := func(v float64) string {
		if r.FX == 1.0 {
			return moneyBase(v)
		}
		return padLeftVisible(formatMoneyBare(v), valueWidth)
	}

	fmt.Fprintf(out, "%s  %s  (%s)\n", label("Net liquidation"), moneyBase(r.NLV), ccyBase)
	fmt.Fprintf(out, "%s  %s  (%.2f%% of NLV)\n", label("Risk budget"), moneyBase(r.RiskBase), r.RiskPct)
	if r.FX != 1.0 {
		fmt.Fprintf(out, "%s  %s  (fx %.4f)\n", label("Risk in quote ccy"), moneyQuote(r.RiskQuote), r.FX)
	}
	fmt.Fprintf(out, "%s  %s\n", label("Per-share risk"), moneyQuote(r.PerShareRisk))
	fmt.Fprintln(out)

	// Shares is the hero — the sizing tool's whole purpose is to tell the
	// user how many to buy. Bold it; the lot suffix stays plain so the
	// hero doesn't get a competing visual sibling on the same line.
	shares := padLeftVisible(fmt.Sprintf("%d", r.Shares), valueWidth)
	fmt.Fprintf(out, "%s  %s  (lot %d)\n", label("Shares"), env.bold(shares), r.Lot)
	fmt.Fprintf(out, "%s  %s\n", label("Notional"), moneyQuote(r.Notional))
	fmt.Fprintf(out, "%s  %s\n", label("Max loss at stop"), moneyQuote(r.MaxLoss))

	// Reward / R block: only when --target was supplied. R-multiple is the
	// canonical "is this trade worth taking" filter — ≥2R is the common
	// discretionary threshold. Breakeven win rate is the dual reading:
	// what hit-rate the strategy needs to be flat at this R.
	if r.R != nil && r.RewardQuote != nil && r.BreakevenWinRate != nil {
		fmt.Fprintln(out)
		fmt.Fprintf(out, "%s  %s\n", label("Max gain at target"), moneyQuote(*r.RewardQuote))
		if r.FX != 1.0 && r.RewardBase != nil {
			fmt.Fprintf(out, "%s  %s  (in %s)\n", label("Max gain in base ccy"), moneyBase(*r.RewardBase), ccyBase)
		}
		rStr := padLeftVisible(fmt.Sprintf("%.2fR", *r.R), valueWidth)
		fmt.Fprintf(out, "%s  %s\n", label("Reward:risk"), rStr)
		beStr := padLeftVisible(fmt.Sprintf("%.1f%%", *r.BreakevenWinRate*100), valueWidth)
		fmt.Fprintf(out, "%s  %s\n", label("Breakeven win rate"), beStr)
	}

	if r.Status != "ok" {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.yellow(fmt.Sprintf("  ⚠ status: %s", r.Status)))
		switch r.Status {
		case "tight_risk":
			fmt.Fprintln(out, "    risk budget < per-share risk × lot — widen the stop, raise --risk-pct, or lower --lot.")
		case "exceeds_buying_power":
			fmt.Fprintln(out, "    notional exceeds available buying power; trim --risk-pct or revisit the entry.")
		}
	}
	fmt.Fprintln(out)
	return 0
}
