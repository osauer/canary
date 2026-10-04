package cli

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
)

func runShortInterest(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "short-interest")
	p := rpc.ShortInterestScreenParams{}
	fs.IntVar(&p.Limit, "limit", 50, "maximum ranked rows: 1-100")
	fs.StringVar(&p.SortBy, "sort-by", "short_interest_shares", "ranking column; missing values last")
	fs.StringVar(&p.SortDir, "sort-dir", "desc", "asc or desc")
	fs.Float64Var(&p.MinPrice, "min-price", 0, "minimum covered USD price")
	fs.Float64Var(&p.MinAvgDollarVolume20D, "min-avg-dollar-volume-20d", 0, "minimum covered 20-session average USD turnover")
	fs.Int64Var(&p.MinAverageVolume, "min-average-volume", 0, "minimum FINRA reporting-cycle average daily shares")
	fs.Float64Var(&p.MinDaysToCover, "min-days-to-cover", 0, "minimum FINRA days to cover (published values floor at 1)")
	fs.BoolVar(&p.ListedOnly, "listed-only", false, "exclude FINRA OTC market rows; includes listed funds")
	exclude := fs.String("exclude", "", "up to 100 comma-separated symbols to omit")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "screen" {
		return failUnexpectedArgs(env, fs)
	}
	if *exclude != "" {
		p.Exclude = strings.Split(*exclude, ",")
	}
	p, err := rpc.NormalizeShortInterestScreenParams(p)
	if err != nil {
		return fail(env, "short-interest screen: %v", err)
	}
	var r rpc.ShortInterestScreenResult
	if err := env.Conn.Call(ctx, rpc.MethodShortInterestScreen, p, &r); err != nil {
		return fail(env, "short-interest screen: %v", err)
	}
	if err := rpc.ValidateShortInterestScreenResult(r, p); err != nil {
		return fail(env, "short-interest screen: %v", err)
	}
	if *jsonOut {
		return printJSON(env, r)
	}
	fmt.Fprintf(env.Stdout, "FINRA short interest · %s · settlement %s · %d matches / %d securities · %d shown\n", r.Status, r.SettlementDate, r.Matching, r.Total, len(r.Rows))
	for _, row := range r.Rows {
		days := "unavailable"
		if row.DaysToCover != nil {
			days = fmt.Sprintf("%.2f", *row.DaysToCover)
		}
		fmt.Fprintf(env.Stdout, "  %s  %d short shares · %s days to cover · %s\n", sanitizeRunText(row.Symbol), row.ShortInterestShares, days, sanitizeRunText(row.Name))
	}
	fmt.Fprintf(env.Stdout, "Market coverage: %d/%d; %d pending, %d unavailable. Displayed prices load first. %s\n", r.Coverage.Covered, r.Coverage.Candidates, r.Coverage.Pending, r.Coverage.Unavailable, r.Detail)
	return 0
}
