package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runHistory(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "history")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	days := fs.Int("days", 90, "calendar lookback in days")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fail(env, "history: usage: canary history SYM [--days N]")
	}
	params := rpc.HistoryDailyParams{
		Symbol: strings.ToUpper(rest[0]),
		Days:   *days,
	}
	var res rpc.HistoryDailyResult
	if err := env.Conn.Call(ctx, rpc.MethodHistoryDaily, params, &res); err != nil {
		return fail(env, "history: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderHistoryText(env, &res)
}

func renderHistoryText(env *Env, r *rpc.HistoryDailyResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%s daily bars  ·  %d-day lookback  ·  %d rows%s\n",
		r.Symbol, r.Days, len(r.Bars), env.suffixBadge(r.DataType))
	fmt.Fprintln(out)
	if len(r.Bars) == 0 {
		fmt.Fprintln(out, "  (no bars returned)")
		fmt.Fprintln(out)
		return 0
	}
	header := fmt.Sprintf("  %-10s  %10s  %10s  %10s  %10s  %s",
		"DATE", "OPEN", "HIGH", "LOW", "CLOSE", "VOLUME")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, b := range r.Bars {
		volPtr := b.Volume
		fmt.Fprintf(out, "  %-10s  %10.2f  %10.2f  %10.2f  %10.2f  %s\n",
			b.Date, b.Open, b.High, b.Low, b.Close, formatSize(&volPtr))
	}
	fmt.Fprintln(out)
	return 0
}
