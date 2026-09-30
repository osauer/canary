package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runMarketBond is canary market --symbol <ISIN|CUSIP> --type BOND: a
// read-only check that the broker resolves the identifier to one bond line
// and quotes it. It never stages or sends an order.
func runMarketBond(ctx context.Context, env *Env, identifier, currency string, jsonOut bool) int {
	var res rpc.MarketBondResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketBond, rpc.MarketBondParams{Identifier: identifier, Currency: currency}, &res); err != nil {
		return fail(env, "market bond: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	renderMarketBondText(env.Stdout, &res)
	return 0
}

// renderMarketBondText prints the check: the line, its size rules and
// conventions, and the quote, with every gap named.
func renderMarketBondText(out io.Writer, res *rpc.MarketBondResult) {
	fmt.Fprintf(out, "\nBond  %s · %s · %s\n", res.Identifier, res.IdentifierType, res.Currency)
	if c := res.Contract; c != nil {
		parts := []string{fmt.Sprintf("con_id %d", c.ConID), c.Class}
		for _, id := range []struct{ label, v string }{{"ISIN", c.ISIN}, {"CUSIP", c.CUSIP}} {
			if id.v != "" {
				parts = append(parts, id.label+" "+id.v)
			}
		}
		if c.Maturity != "" {
			m := "matures " + c.Maturity
			if c.DaysToMaturity != nil {
				m += fmt.Sprintf(" (%d days)", *c.DaysToMaturity)
			}
			parts = append(parts, m)
		}
		if c.Coupon != nil {
			parts = append(parts, fmt.Sprintf("coupon %.3f", *c.Coupon))
		}
		if c.Issuer != "" {
			parts = append(parts, c.Issuer)
		}
		fmt.Fprintf(out, "  %-10s %s\n", "Resolved", strings.Join(parts, " · "))
		size := []string{bondFigure("min size", c.MinSize, "%g"), bondFigure("increment", c.SizeIncrement, "%g"), bondFigure("tick", c.MinTick, "%g")}
		if c.QuantityUnit != "" {
			size = append(size, "unit "+c.QuantityUnit+" (assumed)")
		}
		size = append(size, "price per 100 of face")
		fmt.Fprintf(out, "  %-10s %s\n", "Size", strings.Join(size, " · "))
	} else {
		fmt.Fprintf(out, "  %-10s no (%d lines)\n", "Resolved", res.Lines)
	}
	if q := res.Quote; q != nil {
		parts := []string{bondFigure("bid", q.Bid, "%.4f"), bondFigure("ask", q.Ask, "%.4f"), bondFigure("last", q.Last, "%.4f"), bondFigure("close", q.Close, "%.4f")}
		if q.BidYield != nil || q.AskYield != nil || q.LastYield != nil {
			parts = append(parts, "yield "+strings.Join([]string{bondFigure("bid", q.BidYield, "%.3f%%"), bondFigure("ask", q.AskYield, "%.3f%%"), bondFigure("last", q.LastYield, "%.3f%%")}, " / "))
		}
		state := "fresh"
		if !q.Fresh {
			state = "stale: " + q.StaleReason
		}
		parts = append(parts, nonEmpty(q.DataType, "unknown")+" feed, "+state)
		fmt.Fprintf(out, "  %-10s %s\n", "Quote", strings.Join(parts, " · "))
	}
	if res.Reason != "" {
		fmt.Fprintf(out, "  %-10s %s\n", "Gap", res.Reason)
	}
	fmt.Fprintln(out)
}

// bondFigure labels a figure; a missing one reads "—", never zero.
func bondFigure(label string, v *float64, format string) string {
	if v == nil {
		return label + " —"
	}
	return label + " " + fmt.Sprintf(format, *v)
}
