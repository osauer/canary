package cli

import (
	"context"
	"encoding/csv"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

func runChains(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "chains")
	symbol := fs.String("symbol", "", "one underlying")
	date := fs.String("date", "", "session date YYYY-MM-DD")
	limit := fs.Int("limit", 0, "runs to show (default 10)")
	jsonOut := fs.Bool("json", false, "emit the typed runs and coverage")
	csvOut := fs.Bool("csv", false, "write the selected runs' contract rows as CSV")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 1 || fs.Arg(0) != "recorded" {
		return fail(env, "chains: usage is `canary chains recorded [--symbol SYM] [--date YYYY-MM-DD] [--limit N] [--json|--csv]`")
	}
	if *jsonOut && *csvOut {
		return fail(env, "chains recorded: choose --json or --csv")
	}
	p, err := rpc.NormalizeChainsRecordedParams(rpc.ChainsRecordedParams{Symbol: *symbol, Date: *date, Limit: *limit})
	if err != nil {
		return fail(env, "chains recorded: %v", err)
	}
	var res rpc.ChainsRecordedResult
	if err := rpc.Call(ctx, env.Conn, api.ChainsRecorded, p, &res); err != nil {
		return fail(env, "chains recorded: %v", err)
	}
	switch {
	case *jsonOut:
		return printJSON(env, res)
	case *csvOut:
		return exportChainRows(ctx, env, res, p.Symbol)
	}
	renderChainsRecorded(env, res)
	return 0
}

// chainCSVHeader is the export's column contract (data dictionary:
// internal-docs/design/option-chain-recorder.md). Empty cells were not
// observed; they are never zero-filled.
var chainCSVHeader = []string{"run_id", "session_date", "symbol", "trading_class", "expiry", "expiry_slot", "strike", "right",
	"status", "detail", "con_id", "bid", "ask", "last", "bid_size", "ask_size", "volume", "open_interest", "iv", "delta", "gamma",
	"vega", "theta", "underlying_price", "model_source", "feed", "requested_at", "observed_at", "quote_received_at", "last_trade_at"}

// exportChainRows writes the listed runs' rows, oldest run first, paging each
// run through chains.recorded_rows.
func exportChainRows(ctx context.Context, env *Env, res rpc.ChainsRecordedResult, symbol string) int {
	w := csv.NewWriter(env.Stdout)
	_ = w.Write(chainCSVHeader)
	for _, run := range slices.Backward(res.Runs) {
		after := ""
		for {
			var page rpc.ChainsRecordedRowsResult
			p := rpc.ChainsRecordedRowsParams{RunID: run.RunID, Symbol: symbol, After: after, Limit: rpc.ChainsRowsMaxLimit}
			if err := rpc.Call(ctx, env.Conn, api.ChainsRecordedRows, p, &page); err != nil {
				w.Flush()
				return fail(env, "chains recorded: rows of run %d: %v", run.RunID, err)
			}
			for _, r := range page.Rows {
				_ = w.Write(chainCSVRecord(r))
			}
			if page.Next == "" {
				break
			}
			after = page.Next
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fail(env, "chains recorded: write csv: %v", err)
	}
	return 0
}

func chainCSVRecord(r rpc.ChainRecordedRow) []string {
	f := func(v *float64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatFloat(*v, 'f', -1, 64)
	}
	i := func(v *int64) string {
		if v == nil {
			return ""
		}
		return strconv.FormatInt(*v, 10)
	}
	t := func(v time.Time) string {
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	conID := ""
	if r.ConID > 0 {
		conID = strconv.Itoa(r.ConID)
	}
	return []string{strconv.FormatInt(r.RunID, 10), r.SessionDate, r.Symbol, r.TradingClass, r.Expiry, r.ExpirySlot,
		strconv.FormatFloat(r.Strike, 'f', -1, 64), r.Right, r.Status, r.Detail, conID, f(r.Bid), f(r.Ask), f(r.Last),
		f(r.BidSize), f(r.AskSize), i(r.Volume), i(r.OpenInterest), f(r.IV), f(r.Delta), f(r.Gamma), f(r.Vega), f(r.Theta),
		f(r.UnderlyingPrice), r.ModelSource, r.Feed, t(r.RequestedAt), t(r.ObservedAt), t(r.QuoteReceivedAt), t(r.LastTradeAt)}
}

var chainsNewYork = func() *time.Location {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		return time.UTC
	}
	return loc
}()

func chainsClock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(chainsNewYork).Format("15:04") + " ET"
}

func renderChainsRecorded(env *Env, res rpc.ChainsRecordedResult) {
	rec := res.Recorder
	badge := statusConcern{Text: "OFF", Level: statusConcernNotice}
	if rec.Enabled {
		badge = statusConcern{Text: "ON", Level: statusConcernNone}
	}
	if rec.Running {
		badge = statusConcern{Text: "RECORDING", Level: statusConcernNone}
	}
	fmt.Fprintln(env.Stdout)
	fmt.Fprintf(env.Stdout, "Option-chain recorder  %s\n", env.statusBadge(badge))
	fmt.Fprintln(env.Stdout)
	if !rec.Enabled {
		displayRow(env, env.Stdout, "Recorder", "off: you switched it off; canary settings set chains.recorder.enabled=null records what holdings and strategies declare again")
	}
	declared := "none"
	if len(rec.Declared) > 0 {
		parts := make([]string, len(rec.Declared))
		for i, d := range rec.Declared {
			parts[i] = d.Symbol + " (" + strings.TrimPrefix(d.Source, "strategy:") + ")"
		}
		declared = strings.Join(parts, ", ")
	}
	research := strings.Join(rec.Symbols, ", ")
	if len(rec.Symbols) == 0 {
		research = "none"
	}
	if rec.SymbolsDefault {
		research += " (default)"
	}
	displayRow(env, env.Stdout, "Symbols", fmt.Sprintf("held option underlyings, then strategies' chains: %s, then research names: %s; at most %d", declared, research, rec.MaxSymbols))
	displayRow(env, env.Stdout, "Plan", fmt.Sprintf("%s; strikes within ±%.0f%% of spot, at most %d per expiry, calls and puts; %d min before the close",
		rec.Expiries, rec.StrikeWindowPct*100, rec.MaxStrikesPerExpiry, rec.LeadMinutes))
	displayRow(env, env.Stdout, "Lines", fmt.Sprintf("at most %d at once; %d kept free for the live display and orders", rec.MaxLines, rec.LineReserve))
	if rec.NextRun != nil {
		displayRow(env, env.Stdout, "Next run", rec.NextRun.In(chainsNewYork).Format("2006-01-02")+" "+chainsClock(*rec.NextRun))
	}
	if res.Store.Present {
		displayRow(env, env.Stdout, "Store", fmt.Sprintf("option-chains.db · %d runs · %s rows · %s", res.Store.Runs,
			groupThousands(strconv.FormatInt(res.Store.Rows, 10)), chainsBytes(res.Store.Bytes)))
	} else {
		displayRow(env, env.Stdout, "Store", "none yet; created by the first run")
	}
	fmt.Fprintln(env.Stdout)
	if len(res.Runs) == 0 {
		displayLine(env, "No runs recorded for this selection.", env.dim)
	}
	for _, run := range res.Runs {
		renderChainRun(env, run)
	}
	fmt.Fprintln(env.Stdout)
	displayLine(env, "Market-data reads only; nothing here is an order. --csv exports the rows; empty cells were not observed.", env.dim)
}

func renderChainRun(env *Env, run rpc.ChainRecorderRun) {
	style := env.green
	switch run.Status {
	case "partial", "interrupted", "running":
		style = env.yellow
	case "failed":
		style = env.red
	}
	head := fmt.Sprintf("%s · run %d · %s", run.SessionDate, run.RunID, style(strings.ReplaceAll(run.Status, "_", " ")))
	if run.StopReason != "" {
		head += " (" + strings.ReplaceAll(run.StopReason, "_", " ") + ")"
	}
	if run.EarlyClose {
		head += " · early close"
	}
	fmt.Fprintln(env.Stdout, env.bold(head))
	timing := "Started " + chainsClock(run.StartedAt)
	if !run.FinishedAt.IsZero() {
		timing += " · finished " + chainsClock(run.FinishedAt)
	}
	timing += " · close " + chainsClock(run.SessionClose)
	displayLine(env, "  "+timing, env.dim)
	c := run.Coverage
	displayLine(env, fmt.Sprintf("  Quoted %d of %d%s · two-sided %d · IV %d · greeks %d · OI %d",
		c.Quoted, c.Planned, chainsPct(c.Quoted, c.Planned), c.TwoSided, c.WithIV, c.WithGreeks, c.WithOI), nil)
	if run.HeldUnavailable {
		displayLine(env, "  Held underlyings were unreadable; only the explicit list ran.", env.yellow)
	}
	for _, s := range run.Symbols {
		line := "  " + s.Symbol + " · " + s.Source
		if s.Status == "planned" {
			line += fmt.Sprintf(" · spot %s · %d expiries · %d of %d quoted", chainsPrice(s.Spot), len(s.Expiries), s.Coverage.Quoted, s.Coverage.Planned)
			displayLine(env, line, nil)
			continue
		}
		line += " · " + strings.ReplaceAll(s.Status, "_", " ")
		if s.Detail != "" {
			line += ": " + s.Detail
		}
		displayLine(env, line, env.yellow)
	}
	missing := []string{}
	for _, status := range slices.Sorted(maps.Keys(c.ByStatus)) {
		if status != "quoted" {
			missing = append(missing, fmt.Sprintf("%s %d", strings.ReplaceAll(status, "_", " "), c.ByStatus[status]))
		}
	}
	// An interrupted run never wrote rows for the contracts it did not reach.
	if extra := c.NotAttempted - c.ByStatus["not_attempted"]; extra > 0 {
		missing = append(missing, fmt.Sprintf("never reached %d", extra))
	}
	if len(missing) > 0 {
		displayLine(env, "  Not quoted: "+strings.Join(missing, " · "), env.dim)
	}
}

func chainsPct(n, of int) string {
	if of <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d%%)", n*100/of)
}

func chainsPrice(v float64) string {
	if v <= 0 {
		return "—"
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(v, 'f', 2, 64), ".")
	return groupThousands(whole) + "." + frac
}

func chainsBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}
