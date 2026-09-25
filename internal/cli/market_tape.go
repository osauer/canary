package cli

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runMarketTape(ctx context.Context, env *Env, args []string) int {
	fs := describedFlagSet(env, "market tape", "Daily prices, stocks above their recent average, and trading activity", "canary market tape [--history] [--before YYYY-MM-DD] [--sessions 5] [--explain] [--json]")
	jsonOut := fs.Bool("json", false, "all measurements, source times and data limitations as JSON")
	explain := fs.Bool("explain", false, "briefly explain what this shows and when to trust it")
	sessions := fs.Int("sessions", 5, "completed US trading days; default 5, range 5–60")
	history := fs.Bool("history", false, "saved observations and price changes one, three and five trading days later")
	before := fs.String("before", "", "with --history: show trading days strictly before this YYYY-MM-DD date")
	usage := fs.Usage
	fs.Usage = func() {
		usage()
		fmt.Fprint(env.Stdout, "\nExamples:\n  canary market tape\n  canary market tape --explain\n  canary market tape --history\n  canary market tape --history --before 2026-09-21\n  canary market tape --sessions 20\n  canary market tape --json\n\nRead-only, after-close observations; no forecast. — means unavailable.\n")
	}
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	p, err := rpc.NormalizeMarketTapeParams(rpc.MarketTapeParams{Sessions: *sessions, History: *history, Before: *before})
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
	if *history {
		renderMarketTapeHistory(env, &result, *explain)
		return 0
	}
	renderMarketTape(env, &result, *explain)
	return 0
}

func renderMarketTape(env *Env, result *rpc.MarketTapeResult, explain bool) {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintln(out, env.bold("Market tape"))
	riskReadLine(env, "  Completed closes through "+result.LatestSession+"; no forecast.")
	if result.Archive != nil && result.Archive.Status == "collection_failed" {
		riskReadLine(env, "  Archive save failed; these observations are not confirmed retained.")
	}
	var reading *rpc.MarketTapeReading
	var latest *rpc.MarketTapeSession
	if len(result.Sessions) > 0 {
		latest = &result.Sessions[len(result.Sessions)-1]
		reading = latest.Reading
	}
	if reading != nil {
		fmt.Fprintln(out)
		for _, line := range wrapVisibleText(sanitizeRunText(reading.Headline), briefProseWidth(out)) {
			fmt.Fprintln(out, env.bold(line))
		}
		for _, e := range reading.Evidence {
			if e.Key == "giveback" {
				briefLabelLine(env, "Recent gain", sanitizeRunText(e.Value), briefProseWidth(out))
			}
		}
	}
	fmt.Fprintln(out)
	rows := make([][]string, 0, len(result.Sessions))
	missingComparison := false
	for _, row := range result.Sessions {
		var spx, breadth, volume *float64
		if row.SPX != nil {
			spx = row.SPX.ChangePct
		}
		if row.QQQ != nil {
			volume = row.QQQ.RelativeVolume20
		}
		if row.Breadth != nil {
			breadth = row.Breadth.PctAbove50DMA
		}
		share := marketTapeNumber(env, breadth, "%", false)
		if breadth != nil && row.Breadth.Change50PP == nil {
			share += "*"
			missingComparison = true
		}
		rows = append(rows, []string{sanitizeRunText(row.Date), marketTapeNumber(env, spx, "%", true), share, marketTapeNumber(env, volume, "×", false)})
	}
	cols := []positionTableColumn{{"DAY", positionAlignLeft}, {"S&P 500", positionAlignRight}, {"ABOVE 50-DAY AVG", positionAlignRight}, {"QQQ VOLUME", positionAlignRight}}
	renderPositionTable(env, out, cols, rows)
	fmt.Fprintln(out)
	riskReadLine(env, "  S&P 500: price change since the previous close.")
	riskReadLine(env, "  Above average: % of measured S&P 500 stocks at or above their own 50-day average closing price.")
	riskReadLine(env, "  QQQ volume: trading in one Nasdaq-100 fund; 1× = its previous 20-day average.")
	riskReadLine(env, "  Days here are trading days. — = unavailable.")
	if missingComparison {
		riskReadLine(env, "  * Stock measure cannot be compared with the previous day.")
	}
	fmt.Fprintln(out)
	if latest != nil && latest.Breadth != nil {
		b := latest.Breadth
		if b.PctAbove50DMA != nil {
			riskReadLine(env, fmt.Sprintf("  Latest 50-day measure: %d of %d stocks covered.", b.Coverage50, b.MemberCount))
		}
	}
	if latest != nil && latest.Breadth != nil && latest.Breadth.Participation != nil && latest.Breadth.Participation.CoverageAD > 0 {
		p := latest.Breadth.Participation
		riskReadLine(env, fmt.Sprintf("  Latest day: %d stocks rose / %d fell / %d unchanged (%d of %d covered).", p.Advancing, p.Declining, p.Unchanged, p.CoverageAD, latest.Breadth.MemberCount))
	} else {
		riskReadLine(env, "  Stocks rising/falling that day: unavailable.")
	}
	marketTapeSourceWarnings(env, result.Sources)
	renderMarketTapeLeaders(env, result)
	if explain {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.bold("What this tells you"))
		riskReadLine(env, "  Large stocks can lift the index while many others stay weak.")
		riskReadLine(env, "  Above average shows how widespread strength is, not how many rose today.")
		riskReadLine(env, "  More volume means more trading; it does not tell us what happens next.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.bold("When to trust it"))
		riskReadLine(env, "  This describes completed days using IBKR prices. Stock data can arrive later.")
		riskReadLine(env, "  It has not proved it can predict reversals or warn before the close.")
		riskReadLine(env, "  Next: check daily collection, then test against using price alone.")
	} else {
		fmt.Fprintln(out)
		fmt.Fprintln(out, env.dim("  Plain-language guide: canary market tape --explain"))
	}
	fmt.Fprintln(out, env.dim("  Full measurements and source times: canary market tape --json"))
	fmt.Fprintln(out)
}

func renderMarketTapeLeaders(env *Env, result *rpc.MarketTapeResult) {
	rows := make([][]string, 0, len(result.Sessions))
	for _, r := range result.Sessions {
		if r.Leaders == nil {
			continue
		}
		b := r.Leaders
		var change, volume *float64
		if b.Price != nil {
			change = b.Price.ChangePct
			volume = b.Price.RelativeVolume20
		}
		rows = append(rows, []string{sanitizeRunText(r.Date), marketTapeNumber(env, change, "%", true), marketTapeNumber(env, b.Companies.PctAbove50DMA, "%", false), marketTapeNumber(env, b.Companies.RisingPct, "%", false), marketTapeNumber(env, volume, "×", false)})
	}
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, env.bold("Large-company leaders"))
	renderPositionTable(env, env.Stdout, []positionTableColumn{{"DAY", positionAlignLeft}, {"PRICE", positionAlignRight}, {"ABOVE 50-DAY", positionAlignRight}, {"RISING", positionAlignRight}, {"ACTIVITY", positionAlignRight}}, rows)
	riskReadLine(env, "  Fixed SPY-weighted basket: 11 share lines, 10 companies; Alphabet counts once.")
	riskReadLine(env, "  Rising: % of companies up since the previous close, including unchanged in the total.")
	riskReadLine(env, "  Activity: weighted trading volume versus each stock's preceding 20-day average.")
	for _, source := range result.Sources {
		if strings.HasPrefix(source.Key, "leader:") && source.Cache != nil && (source.Cache.RefreshFailed || source.Cache.RefreshDue || source.Cache.Detail != "") {
			riskReadLine(env, "  Leader history refresh incomplete; showing retained observations.")
			break
		}
	}
	last := result.Sessions[len(result.Sessions)-1]
	if b := last.Leaders; b != nil {
		riskReadLine(env, fmt.Sprintf("  Latest coverage: average %d/10; daily direction %d/10; activity %d/11.", b.Companies.Coverage50, b.Companies.CoverageAD, b.VolumeCoverage))
	}
	if c := last.Companies; c != nil && c.PctAbove50DMA != nil {
		riskReadLine(env, fmt.Sprintf("  Wider S&P companies: %.2f%% above average (%d/%d measured).", *c.PctAbove50DMA, c.Coverage50, c.CompanyCount))
	}
	if last.SPY != nil && last.SPY.SessionMove != nil {
		m := last.SPY.SessionMove
		riskReadLine(env, "  SPY session: opened "+marketTapeNumber(env, m.OpenChangePct, "%", true)+"; low "+marketTapeNumber(env, m.LowChangePct, "%", true)+" versus the previous close; finished "+marketTapeNumber(env, last.SPY.ChangePct, "%", true)+".")
	}
	if last.VIX != nil {
		riskReadLine(env, fmt.Sprintf("  VIX close: %.2f; context only.", last.VIX.Close))
	}
}

func marketTapeSourceWarnings(env *Env, sources []rpc.MarketTapeSource) {
	for _, source := range sources {
		label := map[string]string{"spx": "S&P 500 prices", "qqq": "QQQ prices/volume", "breadth": "Stock measures", "SPY": "SPY prices", "VIX": "VIX"}[source.Key]
		if strings.HasPrefix(source.Key, "leader:") {
			continue
		} // Bounded basket coverage is shown together.
		if label == "" {
			label = "Unknown source"
		}
		switch {
		case source.Status == "unavailable":
			riskReadLine(env, "  Data: "+label+" unavailable.")
		case source.MissingSessions > 0:
			riskReadLine(env, "  Data: "+label, fmt.Sprintf("missing days: %d", source.MissingSessions), "last available "+source.CoveredThrough)
		case source.Status == "partial" && len(source.MissingMetrics) == 0 && source.Cache == nil:
			riskReadLine(env, "  Data: "+label+" incomplete.")
		}
		if c := source.Cache; c != nil {
			switch {
			case c.RefreshFailed:
				riskReadLine(env, "  Data: "+label+" refresh failed; showing saved history.")
			case c.RefreshDue || c.PreviousWindow || c.Detail != "":
				riskReadLine(env, "  Data: "+label+" uses saved history; refresh or date coverage incomplete.")
			}
		}
	}
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
