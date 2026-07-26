package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runMarketEvents(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "market-events")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	symbol := fs.String("symbol", "", "symbol or comma-separated symbols; omit to use held underlyings")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	symbols := []string{}
	if strings.TrimSpace(*symbol) != "" {
		symbols = append(symbols, *symbol)
	}
	symbols = append(symbols, fs.Args()...)
	var res rpc.MarketEventsResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketEventsSnapshot, rpc.MarketEventsParams{Symbols: symbols}, &res); err != nil {
		return fail(env, "market-events: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderMarketEventsText(env, &res)
	return 0
}

func renderMarketEventsText(env *Env, res *rpc.MarketEventsResult) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Canary Market Events  %d active/recent flags\n", len(res.Flags))
	if len(res.Symbols) > 0 {
		statusRow(env, out, "Symbols", strings.Join(res.Symbols, ", "))
	}
	statusRow(env, out, "Fingerprint", res.Fingerprint.Key)
	for _, health := range res.SourceHealth {
		value := health.Status
		if len(health.Notes) > 0 {
			value += " — " + strings.Join(health.Notes, "; ")
		}
		statusRow(env, out, health.Source, value)
	}
	if len(res.WarningDetails) > 0 {
		fmt.Fprintln(out, "Warnings:")
		for _, warning := range res.WarningDetails {
			fmt.Fprintf(out, "  - %s: %s\n", warning.Code, warning.Message)
		}
	}
	if len(res.Flags) == 0 {
		fmt.Fprintln(out, "No active or recent market-event flags found. Unknown sources are shown above, not treated as inactive.")
		fmt.Fprintln(out)
		return
	}
	fmt.Fprintln(out, "Flags:")
	for _, flag := range res.Flags {
		detail := ""
		if len(flag.Details) > 0 {
			detail = " — " + strings.Join(flag.Details, "; ")
		}
		fmt.Fprintf(out, "  %s  %s  %s/%s  %s%s\n", flag.Symbol, flag.Label, flag.Status, flag.Severity, flag.Source, detail)
	}
	fmt.Fprintln(out)
}
