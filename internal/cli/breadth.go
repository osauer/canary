package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runBreadth(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "breadth")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	days := fs.Int("days", 30, "trailing daily-series length (1-90)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() > 0 {
		return failUnexpectedArgs(env, fs)
	}
	params := rpc.BreadthSPXParams{HistoryDays: *days}
	var res rpc.BreadthSPXResult
	if err := env.Conn.Call(ctx, rpc.MethodBreadthSPX, params, &res); err != nil {
		return fail(env, "breadth: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	return renderBreadthText(env, &res)
}

func renderBreadthText(env *Env, r *rpc.BreadthSPXResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "S&P 500 Breadth%s\n", env.suffixBadge(r.DataType))
	fmt.Fprintln(out)

	// A zero reading only means something once State is ready: every other
	// state carries 0 as the "no data yet" sentinel. Printing it anyway read
	// as "0% of the index is above its 50-day average", which is an extreme
	// market signal rather than an empty cache.
	if r.State != "" && r.State != rpc.BreadthStateReady && r.State != rpc.BreadthStateDegraded {
		fmt.Fprintf(out, "  No reading yet (%s)\n", r.State)
		if p := r.Refresh; p != nil && p.Total > 0 {
			fmt.Fprintf(out, "  Progress       %d / %d constituents\n", p.Processed, p.Total)
		}
		if p := r.Refresh; p != nil && p.LastFailure != "" {
			fmt.Fprintf(out, "  Last failure   %s\n", p.LastFailure)
		}
		fmt.Fprintln(out)
		return 0
	}

	fmt.Fprintf(out, "  Above 50-DMA   %.1f %%\n", r.PctAbove50DMA)
	fmt.Fprintf(out, "  Above 200-DMA  %.1f %%\n", r.PctAbove200DMA)
	// New-highs/lows on the sub-line: raw counts plus the net
	// percentage. "Net" is signed; positive means more names making
	// new highs than new lows, which is what you want when SPX is
	// at highs. The narrow-rally pattern reads as SPX near highs +
	// NetNewHighsPct ≈ 0 (or negative).
	fmt.Fprintf(out, "  52w highs/lows %d / %d  (net %+.1f %%)\n",
		r.NewHighsToday, r.NewLowsToday, r.NetNewHighsPct)
	// The reading above is a specific trading session's close, and the daemon
	// keeps serving the last good one when a refresh cannot converge. Without
	// a date on screen there was no way to tell today's breadth from a
	// snapshot the lane stopped updating days ago.
	if r.SessionKey != "" {
		session := r.SessionKey
		if r.Stale {
			session += env.yellow("  (stale — not the latest completed session)")
		}
		fmt.Fprintf(out, "  Session        %s\n", session)
	}
	if !r.AsOf.IsZero() {
		fmt.Fprintf(out, "  Computed       %s\n", r.AsOf.Format("2006-01-02 15:04 MST"))
	}
	if !r.SpotAt.IsZero() {
		fmt.Fprintf(out, "  Observed       %s\n", r.SpotAt.Format("2006-01-02 15:04 MST"))
	}
	fmt.Fprintf(out, "  Source         %s\n", r.Source)
	fmt.Fprintf(out, "  Method         %s\n", r.Method)

	if len(r.History) == 0 {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "  (no history bars returned)")
		fmt.Fprintln(out)
		return 0
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "  50-DMA  %s\n", breadthSparkline(r.History, breadthFieldPct50))
	fmt.Fprintf(out, "  200-DMA %s\n", breadthSparkline(r.History, breadthFieldPct200))
	fmt.Fprintln(out)

	header := fmt.Sprintf("  %-12s  %8s  %8s  %5s  %5s", "DATE", "%50D", "%200D", "HIGH", "LOW")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, h := range r.History {
		fmt.Fprintf(out, "  %-12s  %7.1f%%  %7.1f%%  %5d  %5d\n",
			h.Date, h.PctAbove50DMA, h.PctAbove200DMA, h.NewHighs, h.NewLows)
	}
	fmt.Fprintln(out)
	return 0
}

// breadthField selects which series breadthSparkline draws from a
// history point. Keeps the renderer one function instead of two,
// since the sparkline math is identical across the two SMA readings.
type breadthField int

const (
	breadthFieldPct50 breadthField = iota
	breadthFieldPct200
)

// breadthSparkline renders the trailing series as Unicode block glyphs.
// Eight-step granularity matches the typical 0-100 % breadth range
// without inventing precision the data doesn't have.
func breadthSparkline(h []rpc.BreadthDailyValue, field breadthField) string {
	if len(h) == 0 {
		return ""
	}
	const glyphs = "▁▂▃▄▅▆▇█"
	runes := []rune(glyphs)
	// Map [0, 100] linearly onto the 8 glyph bins. The breadth domain is
	// fixed by construction; no min/max normalization needed.
	var b strings.Builder
	b.Grow(len(h) * 3)
	for _, v := range h {
		var x float64
		switch field {
		case breadthFieldPct200:
			x = v.PctAbove200DMA
		default:
			x = v.PctAbove50DMA
		}
		if x < 0 {
			x = 0
		}
		if x > 100 {
			x = 100
		}
		idx := int(x / 12.5)
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		b.WriteRune(runes[idx])
	}
	return b.String()
}
