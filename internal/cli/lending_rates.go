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

func runLendingScreen(ctx context.Context, env *Env, p rpc.LendingScreenParams, exclude string, jsonOut bool) int {
	if exclude != "" {
		p.Exclude = strings.Split(exclude, ",")
	}
	p, err := rpc.NormalizeLendingScreenParams(p)
	if err != nil {
		return fail(env, "lending screen: %v", err)
	}
	var result rpc.LendingScreenResult
	if err := env.Conn.Call(ctx, rpc.MethodLendingScreen, p, &result); err != nil {
		return fail(env, "lending screen: %v", err)
	}
	if err := rpc.ValidateLendingScreenResult(result, p); err != nil {
		return fail(env, "lending screen: %v", err)
	}
	if jsonOut {
		return printJSON(env, result)
	}
	fmt.Fprintf(env.Stdout, "IBKR US borrowing fees · %s · %d usable / %d USD records · %d matches · %d shown\n", result.Status, result.Usable, result.Total, result.Matching, len(result.Rows))
	fmt.Fprintf(env.Stdout, "Market coverage: %d/%d covered · %d pending · %d unavailable; filters before limit\n", result.Coverage.Covered, result.Coverage.Candidates, result.Coverage.Pending, result.Coverage.Unavailable)
	for _, row := range result.Rows {
		fmt.Fprintf(env.Stdout, "  %s  %.2f%% · %s · %s\n", sanitizeRunText(row.Symbol), *row.FeeRate, sanitizeRunText(row.Name), row.AsOf.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintln(env.Stdout, "Borrower costs, not lending yields. Listing, liquidity and lending allocation are unverified.")
	return 0
}

func runLendingMarket(ctx context.Context, env *Env, symbols string) int {
	p, err := rpc.NormalizeLendingRateSymbols(strings.Split(symbols, ","))
	if err != nil {
		return fail(env, "lending market: %v", err)
	}
	var result rpc.LendingMarketResult
	if err := env.Conn.Call(ctx, rpc.MethodLendingMarket, rpc.LendingMarketParams{Symbols: p}, &result); err != nil {
		return fail(env, "lending market: %v", err)
	}
	if err := rpc.ValidateLendingMarketResult(result, p); err != nil {
		return fail(env, "lending market: %v", err)
	}
	return printJSON(env, result)
}
