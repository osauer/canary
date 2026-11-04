package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runReportingCosts(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "reporting costs")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	month := fs.String("month", "", "one calendar month, YYYY-MM (UTC); default every retained fill")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return fail(env, "reporting costs: usage is `canary reporting costs [--month YYYY-MM] [--json]`")
	}
	if *month != "" && !rpc.ValidCostsMonth(*month) {
		return fail(env, "reporting costs: --month must be YYYY-MM")
	}
	var result rpc.ReportingCostsResult
	if err := env.Conn.Call(ctx, rpc.MethodReportingCosts, rpc.ReportingCostsParams{Month: *month}, &result); err != nil {
		return fail(env, "reporting costs: %v", err)
	}
	if err := rpc.ValidateReportingCostsResult(result); err != nil {
		return fail(env, "reporting costs: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderReportingCosts(env, &result)
	return 0
}

// renderReportingCosts prints the half-spread each fill paid, the totals and
// how far the daemon can see the account's executions.
func renderReportingCosts(env *Env, result *rpc.ReportingCostsResult) {
	out := env.Stdout
	scope := "all months"
	if result.Month != "" {
		scope = result.Month
	}
	fmt.Fprintf(out, "Execution costs  %s\n", scope)
	coverage := "no fills retained"
	if result.Coverage.Ratio != nil {
		coverage = fmt.Sprintf("fills %d · quoted %d (%.0f%%) · unknown %d", result.Coverage.Fills, result.Coverage.Quoted, *result.Coverage.Ratio*100, result.Coverage.Unknown)
	}
	fmt.Fprintf(out, "  mode %s · base %s · %s\n", emptyDash(result.Mode), emptyDash(result.BaseCurrency), coverage)
	displayWrapped(env, "  Half-spread paid = (fill − mid) × side × quantity × multiplier at the quote retained for the fill; positive is cost, negative is price improvement. Base amounts use each fill's own FX rate.")

	for _, section := range []struct {
		title  string
		totals []rpc.CostTotal
	}{{"By month", result.ByMonth}, {"By source", result.BySource}, {"By class", result.ByClass}} {
		if len(section.totals) == 0 {
			continue
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, section.title)
		for _, total := range section.totals {
			fmt.Fprintf(out, "  %-9s fills %3d  measured %3d  unknown %3d  %s\n", total.Key, total.Fills, total.Measured, total.Unknown, costTotalText(total, result.BaseCurrency))
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Fills")
	if len(result.Fills) == 0 {
		fmt.Fprintln(out, "  none retained for this scope")
	}
	for _, fill := range result.Fills {
		fmt.Fprintf(out, "  %s  %s %s %s @ %.4f %s (%s)\n", fill.At.Local().Format("2006-01-02 15:04:05 MST"), fill.Side, formatCostQuantity(fill.Quantity), costFillName(fill), fill.Price, emptyDash(fill.Currency), fill.Source)
		displayWrapped(env, "    "+costFillDetail(fill, result.BaseCurrency))
	}

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Executions from the account")
	obs := result.Observation
	switch {
	case !obs.Polling:
		fmt.Fprintln(out, "  not polling: the daemon store is not bound")
	case obs.LastPollAt.IsZero():
		fmt.Fprintf(out, "  poll every %d s · no poll yet\n", obs.IntervalSeconds)
	default:
		state := "complete"
		if !obs.LastPollComplete {
			state = "incomplete"
		}
		line := fmt.Sprintf("  poll every %d s · last %s %s · %d executions", obs.IntervalSeconds, obs.LastPollAt.Local().Format("15:04:05 MST"), state, obs.LastPollExecutions)
		if obs.LastPollError != "" {
			line += " · " + obs.LastPollError
		}
		fmt.Fprintln(out, line)
	}
	visibility := obs.Visibility
	if !obs.ProvenAt.IsZero() {
		visibility += " (proven " + obs.ProvenAt.Local().Format("15:04:05 MST") + ")"
	}
	fmt.Fprintf(out, "  visibility %s\n", visibility)
	if obs.Action != "" {
		displayWrapped(env, "  "+obs.Action)
	}
}

func costTotalText(total rpc.CostTotal, base string) string {
	if len(total.ByCurrency) > 0 {
		parts := make([]string, 0, len(total.ByCurrency))
		for _, amount := range total.ByCurrency {
			parts = append(parts, costMoney(amount.HalfSpreadBase, amount.Currency))
		}
		text := strings.Join(parts, " · ")
		if total.Unconverted > 0 {
			text += fmt.Sprintf(" (+%d without FX or base currency)", total.Unconverted)
		}
		return text
	}
	if total.HalfSpreadBase == nil {
		if total.Measured > 0 {
			return fmt.Sprintf("— (%d measured, FX or base currency unavailable)", total.Unconverted)
		}
		return "—"
	}
	text := costMoney(*total.HalfSpreadBase, base)
	if total.Unconverted > 0 {
		text += fmt.Sprintf(" (+%d without FX)", total.Unconverted)
	}
	return text
}

// costMoney is a signed amount with two decimals and its currency code: a
// cost report reads +0.50 USD and +1,250.00 EUR, never +1,250 EUR.
func costMoney(v float64, ccy string) string {
	return strings.TrimSpace(formatSignedGrouped(v, 2) + " " + ccy)
}

func costFillName(fill rpc.CostFill) string {
	switch fill.Class {
	case rpc.CostClassOption, rpc.CostClassBond:
		return nonEmpty(strings.Join(strings.Fields(fill.LocalSymbol), " "), fill.Symbol)
	default:
		return fill.Symbol
	}
}

func formatCostQuantity(q float64) string {
	if q == float64(int64(q)) {
		return fmt.Sprintf("%d", int64(q))
	}
	return fmt.Sprintf("%.4g", q)
}

// costFillDetail is the second line of a fill: the quote and the half-spread,
// or why neither could be measured.
func costFillDetail(fill rpc.CostFill, base string) string {
	q := fill.Quote
	parts := []string{"feed " + nonEmpty(q.DataType, "unknown")}
	if q.Bid != nil || q.Ask != nil {
		parts = append(parts, fmt.Sprintf("quote %s / %s", displayNumber(q.Bid, 1, "%.4f"), displayNumber(q.Ask, 1, "%.4f")))
	}
	if q.Mid != nil {
		parts = append(parts, fmt.Sprintf("mid %.4f", *q.Mid))
	}
	if q.GapMS != nil {
		seconds := float64(*q.GapMS) / 1000
		if seconds >= 0 {
			parts = append(parts, fmt.Sprintf("read %.1f s after the fill", seconds))
		} else {
			parts = append(parts, fmt.Sprintf("read %.1f s before the fill", -seconds))
		}
	}
	if fill.Status == rpc.CostFillMeasured {
		cost := "half-spread " + costMoney(*fill.HalfSpread, fill.Currency)
		if fill.HalfSpreadBase != nil {
			cost += " = " + costMoney(*fill.HalfSpreadBase, fill.BaseCurrency)
		} else {
			cost += " (FX or base currency unavailable)"
		}
		parts = append(parts, cost)
	} else if fill.Reason == "backfilled" {
		parts = append(parts, "no quote: found after its day by the reconnect backfill")
	} else {
		parts = append(parts, "unknown: "+fill.Reason)
	}
	return strings.Join(parts, " · ")
}
