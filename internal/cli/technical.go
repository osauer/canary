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
	out := env.Stdout
	fmt.Fprintln(out)
	if r == nil {
		fmt.Fprintln(out, "Technical screen")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  (no rows)")
		fmt.Fprintln(out)
		return 0
	}
	fmt.Fprintf(out, "Technical screen  ·  benchmark %s  ·  %d-day lookback\n", r.Benchmark, r.LookbackDays)
	fmt.Fprintln(out)
	if len(r.Rows) == 0 {
		fmt.Fprintln(out, "  (no rows)")
		fmt.Fprintln(out)
		return 0
	}
	header := fmt.Sprintf("  %-7s %9s %8s %8s %8s %8s %8s %9s %10s %10s %s",
		"SYMBOL", "PRICE", "50DMA", "200DMA", "200EXT", "RS63", "RS126", "ATR%", "ADV20", "ADV$20", "STATE")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, row := range r.Rows {
		state := row.TrendState
		if row.DataQuality != "" && row.DataQuality != "ok" {
			state = row.TrendState + "/" + row.DataQuality
		}
		if row.Error != "" {
			state = "error"
		}
		fmt.Fprintf(out, "  %-7s %9s %8s %8s %8s %8s %8s %9s %10s %10s %s\n",
			row.Symbol,
			formatTechnicalMoney(row.Price, 9),
			formatTechnicalMoney(row.SMA50, 8),
			formatTechnicalMoney(row.SMA200, 8),
			formatTechnicalPct(row.PctAbove200DMA, 8),
			formatTechnicalPct(row.RS63D, 8),
			formatTechnicalPct(row.RS126D, 8),
			formatTechnicalPct(row.ATRPct, 9),
			formatTechnicalVolume(row.AvgVolume20D, 10),
			formatTechnicalDollarVolume(row.AvgDollarVolume20D, 10),
			state,
		)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.dim("  RS = symbol return minus benchmark return; percentages are decimal-return fields in JSON."))
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
