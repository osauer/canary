package cli

import (
	"context"
	"fmt"
	"math"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runMarketTape(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "market tape", "Completed-session price, participation and volume with a descriptive reading", "canary market tape [--sessions 20] [--explain] [--json]")
	jsonOut := fs.Bool("json", false, "emit the full typed tape; text formatting does not alter JSON")
	explain := fs.Bool("explain", false, "include measurement meanings, limitations and source clocks")
	sessions := fs.Int("sessions", 20, "completed US equity sessions; default 20, range 5–60")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		fmt.Fprint(env.Stdout, "\nExamples:\n  canary market tape --sessions 5\n  canary market tape --explain\n  canary market tape --sessions 20 --json\n\nRead-only observations; no forecast. Missing measurements remain unavailable.\n")
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	p, err := rpc.NormalizeMarketTapeParams(rpc.MarketTapeParams{Sessions: *sessions})
	if err != nil {
		return fail(env, "market tape: %v", err)
	}
	var result rpc.MarketTapeResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketTape, p, &result); err != nil {
		return fail(env, "market tape: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderMarketTape(env, &result, *explain)
	return 0
}

func renderMarketTape(env *Env, result *rpc.MarketTapeResult, explain bool) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.bold("Market tape"))
	riskReadLine(env, "  Through", result.LatestSession, result.CoverageStatus+" coverage")
	riskReadLine(env, "  Observed daily sessions; no forecast. Historical availability unknown.")
	var reading *rpc.MarketTapeReading
	if len(result.Sessions) > 0 {
		reading = result.Sessions[len(result.Sessions)-1].Reading
	}
	if reading != nil {
		fmt.Fprintln(out)
		for _, line := range wrapVisibleText(sanitizeRunText(reading.Headline), briefProseWidth(out)) {
			fmt.Fprintln(out, env.bold(line))
		}
		riskReadLine(env, "  "+reading.Summary)
		for _, e := range reading.Evidence {
			if e.Key == "giveback" {
				marketTapeEvidenceLine(env, e)
			}
		}
	}
	fmt.Fprintln(out)
	rows := make([][]string, 0, len(result.Sessions))
	for _, row := range result.Sessions {
		var spx, qqq, breadth, change, volume *float64
		if row.SPX != nil {
			spx = row.SPX.ChangePct
		}
		if row.QQQ != nil {
			qqq, volume = row.QQQ.ChangePct, row.QQQ.RelativeVolume20
		}
		if row.Breadth != nil {
			breadth, change = row.Breadth.PctAbove50DMA, row.Breadth.Change50PP
		}
		rows = append(rows, []string{sanitizeRunText(row.Date), marketTapeNumber(env, spx, "%", true), marketTapeNumber(env, qqq, "%", true), marketTapeNumber(env, breadth, "%", false), marketTapeNumber(env, change, "", true), marketTapeNumber(env, volume, "×", false)})
	}
	cols := []positionTableColumn{{"SESSION", positionAlignLeft}, {"SPX DAY", positionAlignRight}, {"QQQ DAY", positionAlignRight}, {"ABOVE50D", positionAlignRight}, {"Δ PP", positionAlignRight}, {"QQQ RVOL", positionAlignRight}}
	renderPositionTable(env, out, cols, rows)
	fmt.Fprintln(out)
	riskReadLine(env, "  — = unavailable; Δ pp = change in percentage points.")
	riskReadLine(env, "  ABOVE50D = measured S&P 500 stocks above their 50-day average.")
	riskReadLine(env, "  QQQ RVOL = ETF volume / prior 20-session mean; not signed market flow.")
	if reading != nil {
		fmt.Fprintln(out)
		if explain {
			fmt.Fprintln(out, env.bold("Measurements · latest session"))
			for _, e := range reading.Evidence {
				marketTapeEvidenceLine(env, e)
				riskReadLine(env, "    "+e.Meaning)
			}
		} else {
			fmt.Fprintln(out, env.bold("Participation · latest session"))
			for _, e := range reading.Evidence {
				switch e.Key {
				case "breadth_20", "breadth_200", "highs_lows", "advance_decline", "constituent_volume":
					marketTapeEvidenceLine(env, e)
				}
			}
		}
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.bold("Next-session checks"))
		for _, check := range reading.WatchFor {
			riskReadLine(env, "  "+check)
		}
		if explain {
			fmt.Fprintln(out)
			fmt.Fprintln(out, env.bold("Reading limits"))
			for _, limit := range reading.Limits {
				riskReadLine(env, "  "+limit)
			}
		}
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.bold("Source coverage"))
	for _, source := range result.Sources {
		label := map[string]string{"spx": "SPX price", "qqq": "QQQ price / volume", "breadth": "S&P 500 breadth"}[source.Key]
		if label == "" {
			label = "Unknown source"
		}
		riskReadLine(env, "  "+label, source.Status, fmt.Sprintf("%d missing sessions", source.MissingSessions))
		if !explain {
			continue
		}
		acquired := "unavailable"
		if !source.AsOf.IsZero() {
			acquired = source.AsOf.Local().Format("2 Jan 2006 15:04 MST")
		}
		riskReadLine(env, "    Acquired", acquired, source.Detail)
		for _, metric := range []struct{ key, label string }{{"volume", "ETF volume"}, {"relative_volume_20", "Relative volume"}, {"pct_above_20dma", "20-day participation"}, {"pct_above_200dma", "200-day participation"}, {"highs_lows", "Closing highs / lows"}, {"advance_decline", "Daily participation"}, {"constituent_volume", "Directional share volume"}} {
			if n := source.MissingMetrics[metric.key]; n > 0 {
				riskReadLine(env, "    "+metric.label, fmt.Sprintf("unavailable for %d sessions", n))
			}
		}
	}
	if explain {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.bold("Method and timing"))
		for _, note := range result.Notes {
			riskReadLine(env, "  "+note)
		}
	} else {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Details: canary market tape --explain"))
	}
	fmt.Fprintln(out)
}

func marketTapeEvidenceLine(env *Env, e rpc.MarketTapeEvidence) {
	briefLabelLine(env, sanitizeRunText(e.Label), sanitizeRunText(e.Value), briefProseWidth(env.Stdout))
}

func marketTapeNumber(env *Env, v *float64, suffix string, signed bool) string {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return env.dim("—")
	}
	// Match the displayed precision, so a nearly flat session is not -0.00%.
	value := *v
	if math.Abs(value) < 0.005 {
		value = 0
	}
	text := fmt.Sprintf("%.2f%s", value, suffix)
	if signed {
		if value > 0 {
			text = "+" + text
		}
		return env.colorBySign(value, text, signPnL)
	}
	return text
}
