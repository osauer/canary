package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runTechnical(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "technical")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	benchmark := fs.String("benchmark", "SPY", "relative-strength benchmark")
	lookback := fs.Int("lookback-days", 420, "calendar-day history lookback")
	market := fs.String("market", "", "stock routing shortcut for symbols: us | de")
	exchange := fs.String("exchange", "", "IBKR exchange override for symbols, e.g. SMART or IBIS")
	primary := fs.String("primary", "", "IBKR primary exchange hint for symbols, e.g. ARCA or IBIS")
	currency := fs.String("currency", "", "currency override for symbols, e.g. USD or EUR")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "technical: usage: canary technical SYM[,SYM...] [--benchmark SPY] [--market us|de]")
	}
	params := rpc.TechnicalParams{
		Symbols:      splitSymbols(rest[0]),
		Benchmark:    strings.ToUpper(strings.TrimSpace(*benchmark)),
		LookbackDays: *lookback,
		Market:       strings.TrimSpace(*market),
		Exchange:     strings.ToUpper(strings.TrimSpace(*exchange)),
		PrimaryExch:  strings.ToUpper(strings.TrimSpace(*primary)),
		Currency:     strings.ToUpper(strings.TrimSpace(*currency)),
	}
	var res rpc.TechnicalResult
	if err := env.Conn.Call(ctx, rpc.MethodTechnical, params, &res); err != nil {
		return fail(env, "technical: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderTechnicalText(env, &res)
}

func splitSymbols(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if symbol := strings.ToUpper(strings.TrimSpace(part)); symbol != "" {
			out = append(out, symbol)
		}
	}
	return out
}

func renderTechnicalText(env *Env, r *rpc.TechnicalResult) int {
	if r == nil {
		displayLine(env, "Technical screen · no rows", nil)
		return 0
	}
	displayLine(env, fmt.Sprintf("Technical screen · %s benchmark · %d-day lookback", r.Benchmark, r.LookbackDays), env.bold)
	for _, warning := range r.WarningDetails {
		displayLine(env, "Data: "+warning.Message+" · "+warning.Impact+" · "+warning.Action, env.yellow)
	}
	if len(r.Rows) == 0 {
		displayLine(env, "No rows available.", env.dim)
	}
	for _, row := range r.Rows {
		fmt.Fprintln(env.Stdout)
		currency := nonEmpty(row.Currency, nonEmpty(r.Currency, "currency unavailable"))
		state := nonEmpty(row.TrendState, "unrated")
		style := env.dim
		if row.Error != "" || row.DataQuality == "error" {
			state, style = "error", env.red
		} else if row.DataQuality != "ok" {
			state, style = nonEmpty(row.DataQuality, "unverified")+" · recorded "+state, env.yellow
		} else {
			switch state {
			case "uptrend":
				style = env.green
			case "broken":
				style = env.red
			case "extended", "recovering":
				style = env.yellow
			}
		}
		displayLine(env, row.Symbol+" · "+strings.ReplaceAll(state, "_", " "), style)
		displayRow(env, env.Stdout, "Price", strings.TrimSpace(formatTechnicalMoney(row.Price, 0))+" "+currency+" · as of "+nonEmpty(row.PriceAsOf, "unavailable"))
		displayRow(env, env.Stdout, "Trend", "50-day avg "+strings.TrimSpace(formatTechnicalMoney(row.SMA50, 0))+" · 200-day avg "+strings.TrimSpace(formatTechnicalMoney(row.SMA200, 0)))
		displayRow(env, env.Stdout, "vs 200-day", strings.TrimSpace(formatTechnicalPct(row.PctAbove200DMA, 0))+" from the long-term average")
		displayRow(env, env.Stdout, "vs benchmark", displayNumber(row.RS63D, 100, "%+.1f pp")+" / 63 days · "+displayNumber(row.RS126D, 100, "%+.1f pp")+" / 126 days")
		displayRow(env, env.Stdout, "Daily range", strings.TrimSpace(formatTechnicalPct(row.ATRPct, 0))+" · 14-day average true range / price")
		displayRow(env, env.Stdout, "Liquidity", strings.TrimSpace(formatTechnicalVolume(row.AvgVolume20D, 0))+" shares/day · "+strings.TrimSpace(formatTechnicalDollarVolume(row.AvgDollarVolume20D, 0))+" "+currency+"/day · 20-day average")
		if row.Error != "" {
			displayLine(env, "  Error: "+row.Error, env.red)
		}
		for _, reason := range row.MissingReasons {
			displayLine(env, "  Missing: "+strings.ReplaceAll(reason, "_", " "), env.yellow)
		}
	}
	displayLine(env, "pp = percentage points of return relative to the benchmark. — = unavailable.", env.dim)
	return 0
}

func formatTechnicalMoney(v *float64, width int) string {
	if v == nil || *v <= 0 {
		return padDash(width)
	}
	return fmt.Sprintf("%*.2f", width, *v)
}

func formatTechnicalPct(v *float64, width int) string {
	if v == nil {
		return padDash(width)
	}
	return fmt.Sprintf("%*.1f%%", width-1, *v*100)
}

func formatTechnicalVolume(v *int64, width int) string {
	if v == nil || *v <= 0 {
		return padDash(width)
	}
	return fmt.Sprintf("%*s", width, formatVolumePart(v))
}

func formatTechnicalDollarVolume(v *float64, width int) string {
	if v == nil || *v <= 0 {
		return padDash(width)
	}
	n := *v
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%*.1fB", width-1, n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%*.1fM", width-1, n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%*.0fK", width-1, n/1e3)
	default:
		return fmt.Sprintf("%*.0f", width, n)
	}
}
