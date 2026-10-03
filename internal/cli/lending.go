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
			if f.Name != "min-rate" && f.Name != "exclude" && f.Name != "limit" && f.Name != "json" {
				invalid = true
			}
		})
		if invalid {
			return fail(env, "lending screen: only --min-rate, --exclude, --limit and --json are supported")
		}
		return runLendingScreen(ctx, env, *minRate, *limit, *exclude, *jsonOut)
	}
	if fs.NArg() == 1 && fs.Arg(0) == "rates" {
		invalid := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name != "symbols" && f.Name != "json" {
				invalid = true
			}
		})
		if invalid {
			return fail(env, "lending rates: only --symbols and --json are supported")
		}
		return runLendingRates(ctx, env, *symbols, *jsonOut)
	}
	screenFlag := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "min-rate" || f.Name == "exclude" {
			screenFlag = true
		}
	})
	if screenFlag {
		return fail(env, "--min-rate and --exclude require lending screen")
	}
	if *symbols != "" {
		return fail(env, "lending fees: --symbols requires rates")
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
