package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
)

// runEdgeLevels is `canary edge levels`: the daemon replays the level rule
// for a losing call on the five-minute bars of every past session the owner
// held a call on a stock, and this view prints the result.
func runEdgeLevels(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "edge")
	from := fs.String("from", "", "first session YYYY-MM-DD (default: the first the calendar and statements cover)")
	to := fs.String("to", "", "last session YYYY-MM-DD (default: the last closed session)")
	every := fs.Bool("every-session", false, "replay the held names on every session in the bounds, as if held throughout; --from may then lie before Canary's calendar")
	days := fs.Bool("days", false, "also print one line per stock-day")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	params, err := rpc.NormalizeEdgeLevelsParams(rpc.EdgeLevelsParams{From: *from, To: *to, EverySession: *every})
	if err != nil {
		return fail(env, "edge levels: %v", err)
	}
	var result rpc.EdgeLevelsResult
	if err := rpc.Call(ctx, env.Conn, api.EdgeLevels, params, &result); err != nil {
		return fail(env, "edge levels: %v", err)
	}
	if err := rpc.ValidateEdgeLevelsResult(result); err != nil {
		return fail(env, "edge levels: daemon returned an invalid result: %v", err)
	}
	if *jsonOut {
		return printJSON(env, result)
	}
	renderEdgeLevels(env, result, *days)
	return 0
}

var edgeLevelsVersionLabels = map[string]string{
	rpc.EdgeLevelsVersionHeld: "Held", rpc.EdgeLevelsVersionAtOnce: "Reduced at once", rpc.EdgeLevelsVersionRule: "The rule",
	rpc.EdgeLevelsVersionBuyBack: "With its buy back", rpc.EdgeLevelsVersionIndex: "Cut with the index",
}

var edgeLevelsGroupTitles = map[string]string{
	rpc.EdgeLevelsReadingPriorLow + "/" + rpc.EdgeLevelsRowLosing:       "Under the prior low at the look (the default reading)",
	rpc.EdgeLevelsReadingPriorLow + "/" + rpc.EdgeLevelsRowWatchedLater: "Watched at the look, under the prior low later",
	rpc.EdgeLevelsReadingDownOnDay + "/" + rpc.EdgeLevelsRowLosing:      "Down on the day at the look (the other reading)",
	rpc.EdgeLevelsReadingPriorLow + "/" + rpc.EdgeLevelsRowWatched:      "Watched: inside the prior range, held only",
	rpc.EdgeLevelsReadingPriorLow + "/" + rpc.EdgeLevelsRowWorking:      "Working: above the prior high, held only",
}

func renderEdgeLevels(env *Env, r rpc.EdgeLevelsResult, days bool) {
	out := env.Stdout
	title := []string{"Canary Edge", "level replay"}
	if r.From != "" {
		title = append(title, r.From+" to "+r.To)
	}
	if r.State != rpc.EdgeStateCurrent {
		title = append(title, env.yellow(r.State))
	}
	fmt.Fprintln(out, strings.Join(title, " · "))
	if r.Reason != "" {
		fmt.Fprintf(out, "%s\n", sanitizeRunText(r.Reason))
		return
	}
	if r.Incomplete {
		fmt.Fprintln(out, env.yellow(fmt.Sprintf("%d stock-days are still being read from the broker; run again for them.", r.Pending)))
	}
	fmt.Fprintln(out, "Stock prices only. The moment's price and the closes are recorded; every sale and buy back is modelled.")
	if r.EverySession {
		fmt.Fprintln(out, "Every session in the period for the names held since, as if a call had been held throughout: not the days one was held.")
	}
	for _, look := range r.Looks {
		fmt.Fprintf(out, "\n%s\n", env.bold(fmt.Sprintf("Look %d minutes after the open", look.LookMinutes)))
		renderEdgeLevelsLook(env, look, days)
	}
	fmt.Fprintln(out, "\nMethod: canary edge levels --json · the rule, the prices and every stock-day")
}

// edgeLevelsClock is a moment in New York time, the session's own clock.
func edgeLevelsClock(at time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		at = at.In(loc)
	}
	return at.Format("15:04")
}

func edgeLevelsCell(m rpc.EdgeLevelsResultMean) string {
	h5 := "not yet"
	if m.H5MeanPct != nil {
		h5 = fmt.Sprintf("%+.2f", *m.H5MeanPct)
	}
	if m.Version == rpc.EdgeLevelsVersionHeld {
		return fmt.Sprintf("%+.2f / %s", m.CloseMeanPct, h5)
	}
	return fmt.Sprintf("%+.2f / %s  %d/%d", m.CloseMeanPct, h5, m.CloseBeatsHeld, m.CloseDays)
}

func edgeLevelsDayCell(d rpc.EdgeLevelsDraft, version string) string {
	for _, r := range d.Results {
		if r.Version == version && (version == rpc.EdgeLevelsVersionHeld || r.Size == rpc.EdgeLevelsSizeHalves) {
			return fmt.Sprintf("%+6.2f", r.ClosePct)
		}
	}
	return fmt.Sprintf("%6s", "")
}

// renderEdgeLevelsLook prints one look: its sample, the results per group
// and half, the counts and, asked for, one line per stock-day.
func renderEdgeLevelsLook(env *Env, r rpc.EdgeLevelsLook, days bool) {
	out := env.Stdout
	fmt.Fprintf(out, "Sample  %d stock-days · %d sessions · %d names · second half from %s\n", r.Sample.StockDays, r.Sample.Sessions, r.Sample.Names, r.Split)
	fmt.Fprintf(out, "Rows at the look  losing %d · watched %d · working %d · watched, then under the prior low %d\n",
		r.Sample.Rows[rpc.EdgeLevelsRowLosing], r.Sample.Rows[rpc.EdgeLevelsRowWatched], r.Sample.Rows[rpc.EdgeLevelsRowWorking], r.Sample.Rows[rpc.EdgeLevelsRowWatchedLater])
	if len(r.Sample.Excluded) > 0 {
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(r.Sample.Excluded)) {
			parts = append(parts, fmt.Sprintf("%s %d", strings.ReplaceAll(k, "_", " "), r.Sample.Excluded[k]))
		}
		fmt.Fprintf(out, "Left out  %s\n", strings.Join(parts, " · "))
	}
	fmt.Fprintln(out, "\nEach cell: mean result in % of the position at the moment, at the close / five sessions on, and on how many days it beat holding at the close.")
	type key struct{ reading, row, size string }
	groups := map[key]map[string]rpc.EdgeLevelsSum{}
	var order []key
	for _, s := range r.Summary {
		k := key{s.Reading, s.Row, s.Size}
		if groups[k] == nil {
			groups[k] = map[string]rpc.EdgeLevelsSum{}
			order = append(order, k)
		}
		groups[k][s.Half] = s
	}
	halves := []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond}
	for _, k := range order {
		g := groups[k]
		heading := edgeLevelsGroupTitles[k.reading+"/"+k.row]
		if k.size != "" {
			heading += ", " + k.size
		}
		fmt.Fprintf(out, "\n%s\n", heading)
		fmt.Fprintf(out, "  %-20s", "")
		for _, h := range halves {
			s := g[h]
			fmt.Fprintf(out, "  %-24s", fmt.Sprintf("%s: %d days, %d sess.", map[string]string{rpc.EdgeLevelsHalfAll: "all", rpc.EdgeLevelsHalfFirst: "first", rpc.EdgeLevelsHalfSecond: "second"}[h], s.StockDays, s.Sessions))
		}
		fmt.Fprintln(out)
		for i, m := range g[rpc.EdgeLevelsHalfAll].Results {
			fmt.Fprintf(out, "  %-20s", edgeLevelsVersionLabels[m.Version])
			for _, h := range halves {
				if i >= len(g[h].Results) {
					fmt.Fprintf(out, "  %-24s", "")
					continue
				}
				fmt.Fprintf(out, "  %-24s", edgeLevelsCell(g[h].Results[i]))
			}
			fmt.Fprintln(out)
		}
	}
	for _, reading := range []string{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsReadingDownOnDay} {
		fmt.Fprintf(out, "\nCounts, %s, the rule in halves\n", strings.ToLower(strings.SplitN(edgeLevelsGroupTitles[reading+"/"+rpc.EdgeLevelsRowLosing], " (", 2)[0]))
		for _, h := range halves {
			for _, t := range r.Counts {
				if t.Reading != reading || t.Half != h {
					continue
				}
				fmt.Fprintf(out, "  %-6s %d days. Cuts %d, closed above the sale %d (close vs sale %+.2f%%). Crossings %d: held %d (second close vs level %+.2f%%), came back %d.\n",
					h, t.Days, t.Cuts, t.CutsClosedAbove, t.CutCloseVsSalePct, t.Crossings, t.CrossingsHeld, t.WaitVsTouchPct, t.CrossingsNotHeld)
				fmt.Fprintf(out, "         Stop touched %d: closed above the cut %d, under the stop %d. Low broke %d: bounce stalled short of the trim %d (by %.2f%%). Buy backs %d (bought vs sold %+.2f%%). Trims %d, last-hour sales %d.\n",
					t.StopTouches, t.StopClosedAboveCut, t.StopClosedBelowStop, t.LowBreaks, t.BouncesStalled, t.StallShortPct, t.RoundTrips, t.RoundTripCostPct, t.Trims, t.TimeSales)
			}
		}
	}
	if days {
		fmt.Fprintln(out, "\nSession     Name   Calls  Row        Down  Moment  Price     Cut      Stop     Held    Rule    Buy back  Index   (halves, at the close)")
		for _, d := range r.Days {
			drafts := []rpc.EdgeLevelsDraft{d.Look}
			if d.Later != nil {
				drafts = append(drafts, *d.Later)
			}
			for i, draft := range drafts {
				row := d.Row
				if i == 1 {
					row = "later"
				}
				down := ""
				if d.DownOnDay {
					down = "yes"
				}
				cells := []string{}
				for _, v := range []string{rpc.EdgeLevelsVersionHeld, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsVersionBuyBack, rpc.EdgeLevelsVersionIndex} {
					cells = append(cells, edgeLevelsDayCell(draft, v))
				}
				fmt.Fprintf(out, "%-11s %-6s %5g  %-10s %-5s %-7s %-9.2f %-8.2f %-8.2f %s\n", d.Session, sanitizeRunText(d.Symbol), d.Contracts, row, down, edgeLevelsClock(draft.Moment), draft.MomentPrice, draft.Cut, draft.Stop, strings.Join(cells, "  "))
			}
		}
	}
}
