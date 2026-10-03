package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Rates adapts the existing daemon observation. It does not quote a lender's
// yield, rank investments or acquire an independent data source.
func runLendingRates(ctx context.Context, env *Env, symbols string, jsonOut bool) int {
	list, err := rpc.NormalizeLendingRateSymbols(strings.Split(symbols, ","))
	if err != nil {
		return fail(env, "lending rates: %v", err)
	}
	var result rpc.MarketEventsResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketEventsSnapshot, rpc.MarketEventsParams{Symbols: list}, &result); err != nil {
		return fail(env, "lending rates: %v", err)
	}
	if err := rpc.ValidateLendingRateScope(result, list); err != nil {
		return fail(env, "lending rates: %v", err)
	}
	if jsonOut {
		return printJSON(env, result)
	}
	fmt.Fprintln(env.Stdout, "Indicative borrowing rates · not your lending yield")
	for _, row := range result.BorrowFeeCoverage {
		value := "Unavailable"
		if row.FeeRate != nil {
			value = fmt.Sprintf("%.2f%% annualized", *row.FeeRate)
		}
		fmt.Fprintf(env.Stdout, "  %s  %s · %s · %s\n", sanitizeRunText(row.Symbol), value, row.Status, row.AsOf.Format("2006-01-02 15:04 MST"))
	}
	return 0
}
