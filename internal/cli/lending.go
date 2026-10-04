package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runLending(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "lending")
	minRate := fs.Float64("min-rate", 50, "discovery minimum annualized borrower percentage; not a risk rule")
	minPrice := fs.Float64("min-price", 0, "minimum known USD price; applies before result limit")
	minVolume := fs.Float64("min-avg-dollar-volume-20d", 0, "minimum known 20-session average USD turnover")
	minDates := fs.Int("min-high-dates", 0, "0-7 distinct high-fee provider dates in seven days, reset by lower rates")
	sortBy := fs.String("sort-by", "fee_rate", "symbol, price, day_change_pct, volume, avg_dollar_volume_20d, ytd_change_pct, fee_rate or high_dates")
	sortDir := fs.String("sort-dir", "desc", "asc or desc; absent values always last")
	exclude := fs.String("exclude", "", "discovery: up to 100 comma-separated symbols to omit")
	symbols := fs.String("symbols", "", "1-100 comma-separated US stock symbols")
	window := fs.String("window", "365d", "retained Edge period: 90d or 365d")
	from := fs.String("from", "", "exclusive opening equity date; pair with --to")
	to := fs.String("to", "", "inclusive closing equity date; pair with --from")
	conID := fs.Int64("con-id", 0, "optional exact stock contract ID")
	limit := fs.Int("limit", 25, "maximum fee or discovery rows: 1-100")
	cursor := fs.String("cursor", "", "opaque next cursor from the previous page")
	fingerprint := fs.String("fingerprint", "", "optional prior summary fingerprint; rejects changed evidence")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() == 1 && fs.Arg(0) == "screen" {
		invalid := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "min-rate" && f.Name != "exclude" && f.Name != "limit" && f.Name != "json" && f.Name != "min-price" && f.Name != "min-avg-dollar-volume-20d" && f.Name != "min-high-dates" && f.Name != "sort-by" && f.Name != "sort-dir" {
				invalid = true
			}
		})
		if invalid {
			return fail(env, "lending screen: unsupported option; use price, turnover, source-date, rate, sorting, exclude and limit options")
		}
		return runLendingScreen(ctx, env, rpc.LendingScreenParams{MinRate: *minRate, Limit: *limit, MinPrice: *minPrice, MinAvgDollarVolume20D: *minVolume, MinHighDates: *minDates, SortBy: *sortBy, SortDir: *sortDir}, *exclude, *jsonOut)
	}
	if fs.NArg() == 1 && (fs.Arg(0) == "rates" || fs.Arg(0) == "market") {
		invalid := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "symbols" && f.Name != "json" {
				invalid = true
			}
		})
		if invalid {
			return fail(env, "lending %s: only --symbols and --json are supported", fs.Arg(0))
		}
		if fs.Arg(0) == "market" {
			return runLendingMarket(ctx, env, *symbols)
		}
		return runLendingRates(ctx, env, *symbols, *jsonOut)
	}
	screenFlag := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "min-rate" || f.Name == "exclude" || f.Name == "min-price" || f.Name == "min-avg-dollar-volume-20d" || f.Name == "min-high-dates" || f.Name == "sort-by" || f.Name == "sort-dir" {
			screenFlag = true
		}
	})
	if screenFlag {
		return fail(env, "discovery filters and sorting require lending screen")
	}
	if *symbols != "" {
		return fail(env, "lending fees: --symbols requires rates or market")
	}
	if fs.NArg() != 0 && (fs.NArg() != 1 || fs.Arg(0) != "fees") {
		return failUnexpectedArgs(env, fs)
	}
	params, err := rpc.NormalizeFinancingFeesParams(rpc.FinancingFeesParams{Window: *window, From: *from, To: *to, ConID: *conID, Limit: *limit, Cursor: *cursor, Fingerprint: *fingerprint})
	if err != nil {
		return fail(env, "lending fees: %v", err)
	}
	var result rpc.FinancingFeesResult
	if err := env.Conn.Call(ctx, rpc.MethodFinancingFees, params, &result); err != nil {
		return fail(env, "lending fees: %v", err)
	}
	if err := rpc.ValidateFinancingFeesResponse(result, params); err != nil {
		return fail(env, "lending fees: invalid daemon result: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderLendingSummary(env, result.Summary)
	for _, fee := range result.Fees {
		amount := "Unavailable"
		if fee.NetFee != nil {
			amount = edgeSignedMoney(env, *fee.NetFee, fee.Currency)
		}
		fmt.Fprintf(env.Stdout, "  %s  %-12s %s\n", fee.ValueDate.Format(time.DateOnly), sanitizeRunText(fee.Symbol), amount)
	}
	if result.FilteredCount == 0 && result.Summary.State != rpc.FinancingUnavailable {
		fmt.Fprintln(env.Stdout, "  No reported earned fees in this period.")
	}
	if result.NextCursor != "" {
		fmt.Fprintf(env.Stdout, "  More fees: canary lending fees --cursor %s --con-id %d --from %s --to %s\n", result.NextCursor, result.ConID, result.Summary.From.Format(time.DateOnly), result.Summary.To.Format(time.DateOnly))
	}
	return 0
}

func renderLendingSummary(env *Env, summary rpc.FinancingSummary) {
	amount := "Unavailable"
	if summary.EarnedBase != nil {
		amount = edgeSignedMoney(env, *summary.EarnedBase, summary.BaseCurrency)
	} else if summary.KnownEarnedBase != nil {
		amount = edgeSignedMoney(env, *summary.KnownEarnedBase, summary.BaseCurrency) + " known"
	}
	fmt.Fprintf(env.Stdout, "Stock lending income: %s · %s to %s · %s\n", amount, summary.From.Format(time.DateOnly), summary.To.Format(time.DateOnly), summary.State)
	for _, native := range summary.Native {
		fmt.Fprintf(env.Stdout, "  Earned %s\n", edgeSignedMoney(env, native.Amount, native.Currency))
	}
	if summary.Reason != "" {
		fmt.Fprintf(env.Stdout, "  Evidence: %s (%d/%d days covered)\n", summary.Reason, summary.CoveredDays, summary.ExpectedDays)
	}
	fmt.Fprintln(env.Stdout, "  P/L reconciliation and cash-payment linkage unavailable; do not add these fees to broker P/L.")
}
