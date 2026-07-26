package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// runScan dispatches the four shapes of `canary scan`:
//
//	canary scan <preset>                      → run a named preset
//	canary scan list                          → enumerate configured presets
//	canary scan params [--instrument STK]     → dump gateway catalog
//	canary scan --type X --exchange Y         → ad-hoc one-off scan
//
// Ad-hoc is the agent-facing path: avoids having to write a preset to the
// user's config.toml just to run a one-time scan. Preset stays as the
// friendly shorthand for human users and recurring agent workflows.
func runScan(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "scan")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	limit := fs.Int("limit", 0, "max rows (0 = preset default / 50 cap for ad-hoc)")
	adHocType := fs.String("type", "", "ad-hoc scanCode (e.g. TOP_PERC_GAIN) — required with --exchange when no preset is given")
	adHocExch := fs.String("exchange", "", "ad-hoc locationCode (e.g. STK.US.MAJOR) — required with --type when no preset is given")
	paramsInstrument := fs.String("instrument", "", "IBKR scanner instrument (e.g. STK, STOCK.EU); filters `scan params` and routes ad-hoc scans")
	minPrice := fs.Float64("min-price", 0, "drop enriched rows below this last price")
	minVolume := fs.Int64("min-volume", 0, "drop enriched rows below this share volume")
	minDollarVolume := fs.Float64("min-dollar-volume", 0, "drop enriched rows below this last×volume dollar-volume")
	requireLive := fs.Bool("require-live", false, "drop rows whose quote context is off-hours/stale/prev-close")
	excludePenny := fs.Bool("exclude-penny", false, "drop enriched rows below $5")
	paramsRaw := fs.Bool("raw", false, "include the gateway's raw XML payload in the params dump (~200 KB)")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	rest := fs.Args()

	switch {
	case len(rest) == 1 && rest[0] == "list":
		return runScanList(ctx, env, *jsonOut)
	case len(rest) >= 1 && rest[0] == "params":
		return runScanParams(ctx, env, *paramsInstrument, *paramsRaw, *jsonOut)
	case len(rest) == 0 && *adHocType != "" && *adHocExch != "":
		params := rpc.ScanRunParams{
			Type:            *adHocType,
			Exchange:        *adHocExch,
			Instrument:      *paramsInstrument,
			Limit:           *limit,
			MinPrice:        *minPrice,
			MinVolume:       *minVolume,
			MinDollarVolume: *minDollarVolume,
			RequireLive:     *requireLive,
			ExcludePenny:    *excludePenny,
		}
		return runScanCall(ctx, env, params, *jsonOut)
	case len(rest) == 0 && (*adHocType != "" || *adHocExch != ""):
		return fail(env, "scan: ad-hoc mode requires both --type and --exchange (see 'canary scan params' for valid values)")
	case len(rest) == 1:
		params := rpc.ScanRunParams{
			Preset:          rest[0],
			Limit:           *limit,
			MinPrice:        *minPrice,
			MinVolume:       *minVolume,
			MinDollarVolume: *minDollarVolume,
			RequireLive:     *requireLive,
			ExcludePenny:    *excludePenny,
		}
		return runScanCall(ctx, env, params, *jsonOut)
	default:
		return fail(env, "scan: usage: canary scan <preset> | canary scan list | canary scan params [--instrument STK] | canary scan --type X --exchange Y [--instrument STK|STOCK.EU]")
	}
}

func runScanCall(ctx context.Context, env *Env, params rpc.ScanRunParams, jsonOut bool) int {
	var res rpc.ScanResult
	if err := env.Conn.Call(ctx, rpc.MethodScanRun, params, &res); err != nil {
		return fail(env, "scan: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	return renderScanText(env, &res)
}

func runScanList(ctx context.Context, env *Env, jsonOut bool) int {
	var res rpc.ScanListResult
	if err := env.Conn.Call(ctx, rpc.MethodScanList, nil, &res); err != nil {
		return fail(env, "scan list: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	out := env.Stdout
	fmt.Fprintln(out)
	// Numeric LIMIT right-aligns in its column; text labels left-align so the
	// preset name reads as the row anchor. Built off the same widths as the
	// data row so labels stay glued to their columns.
	const (
		wPreset = 20
		wType   = 32
		wExch   = 18
		wInst   = 10
		wLimit  = 5
	)
	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %*s",
		wPreset, "PRESET", wType, "TYPE", wExch, "EXCHANGE", wInst, "INSTRUMENT", wLimit, "LIMIT")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, p := range res.Presets {
		inst := p.Instrument
		if inst == "" {
			inst = "STK"
		}
		fmt.Fprintf(out, "  %-*s  %-*s  %-*s  %-*s  %*d\n",
			wPreset, p.Name, wType, p.Type, wExch, p.Exchange, wInst, inst, wLimit, p.Limit)
	}
	fmt.Fprintln(out)
	return 0
}

func runScanParams(ctx context.Context, env *Env, instrument string, raw, jsonOut bool) int {
	params := rpc.ScanParamsParams{Instrument: instrument, IncludeRawXML: raw}
	var res rpc.ScanParamsResult
	if err := env.Conn.Call(ctx, rpc.MethodScanParams, params, &res); err != nil {
		return fail(env, "scan params: %v", err)
	}
	if jsonOut {
		return printJSON(env, res)
	}
	return renderScanParamsText(env, &res, instrument)
}

func renderScanText(env *Env, r *rpc.ScanResult) int {
	out := env.Stdout
	fmt.Fprintln(out)
	label := r.Preset
	if label == "" {
		label = "(ad-hoc)"
	}
	fmt.Fprintf(out, "Scan: %s (%s)  ·  %d rows  ·  as of %s\n",
		label, r.Type, len(r.Rows), formatTimeShort(r.AsOf))
	fmt.Fprintln(out)
	// Columns match `canary quote`'s width/colour conventions: signed change
	// in green/red, em-dash for nil. The 52w range is rendered as a single
	// "low..high" string so the eye can pick out whether the current price
	// is near the top or bottom of the range without scanning two columns.
	// Each value column is pre-formatted to its exact visible width before
	// ANSI colour wrapping so the column lines up whether colour is on or
	// off and whether the value is present or nil (em-dash).
	header := fmt.Sprintf("  %-4s  %-9s %12s  %8s  %9s  %7s  %20s  %s",
		"RANK", "SYMBOL", "LAST", "CHG%", "VOL", "IV", "52W RANGE", "NOTE")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, row := range r.Rows {
		fmt.Fprintf(out, "  %-4d  %-9s %s  %s  %s  %s  %s  %s\n",
			row.Rank,
			row.Symbol,
			formatPrice(row.Last, 12, row.Currency),
			env.formatChangePct(row.ChangePct, 8),
			formatVolumeCompact(row.Volume, 9),
			ivStatus(row.IV),
			formatRange52w(env, row.Week52Low, row.Week52High, row.Currency),
			row.Comment,
		)
	}
	fmt.Fprintln(out)
	return 0
}

// formatPrice renders a price right-aligned to width w with the row's
// currency symbol attached (dense style — "$192.50", not "$ 192.50" —
// so the scan grid stays narrow). Empty ccy falls back to "$" for
// back-compat with daemons that don't emit ScanRow.Currency yet.
// Nil → em-dash. The symbol is part of the visible width so the column
// aligns whether the price is present or not.
func formatPrice(p *float64, w int, ccy string) string {
	if p == nil {
		return padDash(w)
	}
	sym := strings.TrimSpace(moneyPrefix(ccy))
	return fmt.Sprintf("%*s", w, fmt.Sprintf("%s%.2f", sym, *p))
}

// formatVolumeCompact renders share volume right-aligned to width w with
// K/M/B suffixes — the convention TWS and finance terminals use, easier
// to scan than literal thousand-separated counts. Nil → em-dash.
func formatVolumeCompact(v *int64, w int) string {
	if v == nil || *v <= 0 {
		return padDash(w)
	}
	n := float64(*v)
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%*s", w, fmt.Sprintf("%.2fB", n/1e9))
	case n >= 1e6:
		return fmt.Sprintf("%*s", w, fmt.Sprintf("%.2fM", n/1e6))
	case n >= 1e3:
		return fmt.Sprintf("%*s", w, fmt.Sprintf("%.1fK", n/1e3))
	default:
		return fmt.Sprintf("%*d", w, *v)
	}
}

// formatRange52w renders the 52-week low..high pair in dim grey to keep
// the eye drawn to the live price and change columns. Either side nil →
// em-dash for that half. The "low..high" connector is two dots; uses no
// space so the column stays compact. Currency follows the row; empty
// ccy falls back to "$".
func formatRange52w(env *Env, lo, hi *float64, ccy string) string {
	sym := strings.TrimSpace(moneyPrefix(ccy))
	lowStr := "—"
	if lo != nil {
		lowStr = fmt.Sprintf("%s%.2f", sym, *lo)
	}
	highStr := "—"
	if hi != nil {
		highStr = fmt.Sprintf("%s%.2f", sym, *hi)
	}
	return env.dim(fmt.Sprintf("%9s..%-9s", lowStr, highStr))
}

func renderScanParamsText(env *Env, r *rpc.ScanParamsResult, instrument string) int {
	out := env.Stdout
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Scanner catalog  ·  %d instruments  ·  %d locations  ·  %d scan types",
		len(r.Instruments), len(r.Locations), len(r.ScanTypes))
	if instrument != "" {
		fmt.Fprintf(out, "  (instrument=%s)", instrument)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  as of %s\n\n", formatTimeShort(r.AsOf))

	// Two sub-tables: SCAN TYPES and LOCATIONS. Each gets a dim header +
	// rule beneath, matching the table convention used by the other
	// command renderers.
	const (
		wCode = 36
		wInst = 18
	)
	scanHeader := fmt.Sprintf("  %-*s  %-*s  %s", wCode, "CODE", wInst, "INSTRUMENTS", "DISPLAY NAME")
	fmt.Fprintln(out, "  SCAN TYPES (--type)")
	fmt.Fprintln(out, env.dim(scanHeader))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(scanHeader))))
	for _, st := range r.ScanTypes {
		fmt.Fprintf(out, "  %-*s  %-*s  %s\n",
			wCode, st.Code, wInst, strings.Join(st.Instruments, ","), st.DisplayName)
	}
	fmt.Fprintln(out)

	locHeader := fmt.Sprintf("  %-*s  %s", wCode, "CODE", "DISPLAY NAME")
	fmt.Fprintln(out, "  LOCATIONS (--exchange)")
	fmt.Fprintln(out, env.dim(locHeader))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(locHeader))))
	for _, loc := range r.Locations {
		fmt.Fprintf(out, "  %-*s  %s\n", wCode, loc.Code, loc.DisplayName)
	}
	fmt.Fprintln(out)

	if r.RawXML != "" {
		fmt.Fprintf(out, "  RAW XML (%d bytes follows)\n\n", len(r.RawXML))
		fmt.Fprintln(out, r.RawXML)
	}
	return 0
}
