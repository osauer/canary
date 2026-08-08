package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

func runPositions(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "positions")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	symbol := fs.String("symbol", "", "filter by symbol")
	typeF := fs.String("type", "", "filter by type: stk | opt")
	sortBy := fs.String("sort", "alpha", "sort: alpha | pnl | value")
	by := fs.String("by", "", "group view: underlying (default = flat stocks/options tables)")
	quotes := fs.Bool("quotes", false, "include quote-detail columns on stock rows: previous close, ranges, volume")
	view := fs.String("view", rpc.ViewFull, "JSON response view: full | risk")
	watch := fs.Bool("watch", false, "re-poll on a fixed interval; in-place redraw on a TTY")
	rate := fs.Duration("rate", time.Second, "poll interval for --watch")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if *by != "" && *by != "underlying" {
		return fail(env, "positions: --by must be 'underlying' (got %q)", *by)
	}
	if *view != rpc.ViewFull && *view != rpc.ViewRisk {
		return fail(env, "positions: --view must be %q or %q (got %q)", rpc.ViewFull, rpc.ViewRisk, *view)
	}
	if *view != rpc.ViewFull && !*jsonOut {
		return fail(env, "positions: --view requires --json")
	}

	fetchAndRender := func(out io.Writer) int {
		params := rpc.PositionsListParams{Symbol: *symbol, Type: *typeF}
		var res rpc.PositionsResult
		if err := env.Conn.Call(ctx, rpc.MethodPositionsList, params, &res); err != nil {
			return fail(env, "positions: %v", err)
		}
		applySort(res.Stocks, *sortBy)
		applySort(res.Options, *sortBy)
		if *jsonOut {
			if *view == rpc.ViewRisk {
				return printJSONTo(env, out, rpc.CompactPositionsRisk(&res, 5))
			}
			return printJSONTo(env, out, res)
		}
		if *by == "underlying" {
			return renderPositionsByUnderlyingTo(env, out, &res)
		}
		return renderPositionsTextTo(env, out, &res, *quotes)
	}

	if *watch {
		if *jsonOut {
			return fail(env, "positions: --watch and --json are mutually exclusive")
		}
		return runWatch(ctx, env, *rate, "positions", fetchAndRender)
	}
	return fetchAndRender(env.Stdout)
}

func applySort(rows []rpc.PositionView, by string) {
	switch by {
	case "pnl":
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(b.UnrealizedPnL, a.UnrealizedPnL) })
	case "value":
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(b.MarketValue, a.MarketValue) })
	default:
		slices.SortStableFunc(rows, func(a, b rpc.PositionView) int { return cmp.Compare(a.Symbol, b.Symbol) })
	}
}

// renderPositionsText is preserved as a thin wrapper for tests and
// preview that pass an Env holding the destination writer.
func renderPositionsText(env *Env, r *rpc.PositionsResult) int {
	return renderPositionsTextTo(env, env.Stdout, r, false)
}

func renderPositionsTextTo(env *Env, out io.Writer, r *rpc.PositionsResult, quoteDetails bool) int {
	fmt.Fprintln(out)
	unavailable := renderPositionsAuthority(env, out, r)
	if len(r.Stocks) == 0 && len(r.Options) == 0 {
		if unavailable {
			fmt.Fprintf(out, "Positions unavailable%s\n\n", env.suffixBadge(r.DataType))
			return 0
		}
		fmt.Fprintf(out, "No open positions%s\n\n", env.suffixBadge(r.DataType))
		return 0
	}
	// Realized P&L is only rendered when at least one row carries a non-zero
	// value — most accounts in a same-day snapshot have all zeros, and an
	// always-on column adds dead width to the table.
	showRealized := anyRealized(r.Stocks) || anyRealized(r.Options)
	renderPortfolioSummaryTo(env, out, r)
	if len(r.Stocks) > 0 {
		renderStocksTable(env, out, r.Stocks, r.DataType, showRealized, quoteDetails)
	}
	if len(r.Options) > 0 {
		renderOptionsTable(env, out, r.Options, r.DataType, showRealized)
	}
	fmt.Fprintf(out, "  %d positions  ·  as of %s\n",
		len(r.Stocks)+len(r.Options), formatTimeShort(positionsDisplayAsOf(r)))
	return 0
}

func renderPositionsAuthority(env *Env, out io.Writer, r *rpc.PositionsResult) bool {
	if r == nil || r.Authority == nil {
		return false
	}
	scope := r.Authority.Scope
	if scope.AccountID != "" || scope.AccountMode != "" {
		fmt.Fprintf(out, "Positions  %s", nonEmpty(scope.AccountID, "—"))
		if scope.AccountMode != "" {
			fmt.Fprintf(out, " · %s", scope.AccountMode)
		}
		fmt.Fprintln(out)
	}
	writeAccountAuthorityNotice(env, out, r.Authority)
	return r.Authority.Availability != rpc.AccountDataAvailable
}

func positionsDisplayAsOf(r *rpc.PositionsResult) time.Time {
	if r == nil {
		return time.Time{}
	}
	if r.Authority != nil && !r.Authority.AsOf.IsZero() {
		return r.Authority.AsOf
	}
	return r.AsOf
}

// renderStocksTable prints the stocks block. The DAY column carries IBKR's
// per-conId start-of-trading-day P&L from reqPnLSingle (TWS msg 95) — the
// same metric across stocks and options. Always rendered; nil rows show
// the "subscribing" placeholder on the first call after a daemon restart
// and a value on the next refresh.
//
// Column widths are sized from the rows being rendered, with the header as
// the floor. That keeps small books compact while preserving right-aligned
// money and quantity columns when a larger account needs more room.
func renderStocksTable(env *Env, out io.Writer, rows []rpc.PositionView, dataType string, showRealized bool, quoteDetails bool) {
	fmt.Fprintf(out, "Stocks & ETFs%s\n", env.suffixBadge(dataType))
	cols := []positionTableColumn{
		{header: "SYMBOL", align: positionAlignLeft},
		{header: "POS", align: positionAlignRight},
		{header: "CCY", align: positionAlignLeft},
		{header: "MARK", align: positionAlignRight},
		{header: "CLOSE", align: positionAlignRight},
		{header: "CHG", align: positionAlignRight},
		{header: "CHG%", align: positionAlignRight},
		{header: "QUOTE", align: positionAlignRight},
	}
	if quoteDetails {
		cols = append(cols,
			positionTableColumn{header: "PRIOR", align: positionAlignRight},
			positionTableColumn{header: "DAY", align: positionAlignRight},
			positionTableColumn{header: "52W", align: positionAlignRight},
			positionTableColumn{header: "VOL/AVG", align: positionAlignRight},
		)
	}
	cols = append(cols,
		positionTableColumn{header: "AVG", align: positionAlignRight},
		positionTableColumn{header: "VALUE", align: positionAlignRight},
		positionTableColumn{header: "DAY P&L", align: positionAlignRight},
		positionTableColumn{header: "UNREAL", align: positionAlignRight},
	)
	if showRealized {
		cols = append(cols, positionTableColumn{header: "REAL", align: positionAlignRight})
	}
	cols = append(cols,
		positionTableColumn{header: "DATA", align: positionAlignLeft},
		positionTableColumn{header: "AS OF", align: positionAlignLeft},
	)
	tableRows := make([][]string, 0, len(rows))
	for _, p := range rows {
		row := []string{
			p.Symbol,
			formatPositionQuantity(p.Quantity, "sh"),
			formatPositionCurrency(p.Currency),
			formatPositionPrice(p.Mark),
			formatPositionPricePtr(positionRegularClose(p)),
			formatWatchlistChange(env, p.DayChange, 8),
			env.formatChangePct(p.DayChangePct, 7),
			formatPositionPricePtr(p.QuotePrice),
		}
		if quoteDetails {
			row = append(row,
				formatWatchlistNumber(p.PriorRegularClose, 9),
				formatWatchlistRange(env, p.DayLow, p.DayHigh, 15),
				formatWatchlistRange(env, p.Week52Low, p.Week52High, 15),
				formatWatchlistVolume(p.Volume, p.AvgVolume, 11),
			)
		}
		row = append(row,
			formatPositionPrice(avgCostPerShare(p)),
			formatPositionValue(p),
			env.formatPositionPnLPtr(p.DailyPnL),
			env.formatPositionPnL(p.UnrealizedPnL),
		)
		if showRealized {
			row = append(row, env.formatPositionPnL(p.RealizedPnL))
		}
		row = append(row, formatPositionData(env, p), formatPositionAsOf(p))
		tableRows = append(tableRows, row)
	}
	renderPositionTable(env, out, cols, tableRows)
	fmt.Fprintln(out)
}

// renderOptionsTable prints the options block in the same column language
// as renderStocksTable. The DAY P&L column matches stocks — same source
// (reqPnLSingle), same alignment — so a reader can scan one column down
// to see "today's P&L" across the whole book.
func renderOptionsTable(env *Env, out io.Writer, rows []rpc.PositionView, dataType string, showRealized bool) {
	fmt.Fprintf(out, "Options%s\n", env.suffixBadge(dataType))
	cols := []positionTableColumn{
		{header: "UNDERLYING", align: positionAlignLeft},
		{header: "SIDE", align: positionAlignLeft},
		{header: "EXPIRY", align: positionAlignLeft},
		{header: "STRIKE", align: positionAlignRight},
		{header: "POS", align: positionAlignRight},
		{header: "AVG", align: positionAlignRight},
		{header: "MARK", align: positionAlignRight},
		{header: "BID/ASK", align: positionAlignRight},
		{header: "DAY P&L", align: positionAlignRight},
		{header: "UNREAL", align: positionAlignRight},
	}
	if showRealized {
		cols = append(cols, positionTableColumn{header: "REAL", align: positionAlignRight})
	}
	tableRows := make([][]string, 0, len(rows))
	for _, p := range rows {
		row := []string{
			p.Symbol,
			p.Right,
			formatExpiry(p.Expiry),
			fmt.Sprintf("%.2f", p.Strike),
			formatPositionQuantity(p.Quantity, "ct"),
			formatPositionPrice(avgCostPerShare(p)),
			formatPositionPrice(p.Mark),
			formatOptionBidAsk(p),
			env.formatPositionPnLPtr(p.DailyPnL),
			env.formatPositionPnL(p.UnrealizedPnL),
		}
		if showRealized {
			row = append(row, env.formatPositionPnL(p.RealizedPnL))
		}
		tableRows = append(tableRows, row)
	}
	renderPositionTable(env, out, cols, tableRows)
	fmt.Fprintln(out)
}

type positionColumnAlign int

const (
	positionAlignLeft positionColumnAlign = iota
	positionAlignRight
)

type positionTableColumn struct {
	header string
	align  positionColumnAlign
}

func renderPositionTable(env *Env, out io.Writer, cols []positionTableColumn, rows [][]string) {
	widths := make([]int, len(cols))
	headers := make([]string, len(cols))
	for i, col := range cols {
		headers[i] = col.header
		widths[i] = visibleLen(col.header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			widths[i] = max(widths[i], visibleLen(cell))
		}
	}
	header := formatPositionTableRow(cols, widths, headers)
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, row := range rows {
		fmt.Fprintln(out, formatPositionTableRow(cols, widths, row))
	}
}

func formatPositionTableRow(cols []positionTableColumn, widths []int, cells []string) string {
	var b strings.Builder
	b.WriteString("  ")
	for i, col := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		cell := ""
		if i < len(cells) {
			cell = cells[i]
		}
		if col.align == positionAlignRight {
			b.WriteString(padLeftVisible(cell, widths[i]))
		} else {
			b.WriteString(padRightVisible(cell, widths[i]))
		}
	}
	return b.String()
}

func formatPositionCurrency(ccy string) string {
	ccy = strings.ToUpper(strings.TrimSpace(ccy))
	if ccy == "" {
		return "—"
	}
	return ccy
}

func formatPositionQuantity(qty float64, unit string) string {
	if unit == "" {
		return fmt.Sprintf("%.0f", qty)
	}
	return fmt.Sprintf("%.0f %s", qty, unit)
}

func formatPositionPrice(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

func formatPositionPricePtr(v *float64) string {
	if v == nil || *v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v)
}

func positionRegularClose(p rpc.PositionView) *float64 {
	if p.RegularClose != nil {
		return p.RegularClose
	}
	return p.PrevClose
}

func formatOptionBidAsk(p rpc.PositionView) string {
	bid := "—"
	if p.OptionBid != nil && *p.OptionBid > 0 {
		bid = fmt.Sprintf("%.2f", *p.OptionBid)
	}
	ask := "—"
	if p.OptionAsk != nil && *p.OptionAsk > 0 {
		ask = fmt.Sprintf("%.2f", *p.OptionAsk)
	}
	return bid + "/" + ask
}

func formatPositionValue(p rpc.PositionView) string {
	return formatMoneyCcy(p.MarketValue, p.Currency)
}

func formatPositionData(env *Env, p rpc.PositionView) string {
	if p.DataType == "" && p.QuotePriceAt.IsZero() && p.RegularCloseAt.IsZero() && p.QuotePrice == nil && p.RegularClose == nil {
		return env.dim("pos")
	}
	if p.Stale || p.StaleReason != "" {
		return env.yellow("stale")
	}
	switch p.QuoteQuality {
	case "wide", "indicative", "prev_close", "missing":
		return env.yellow(p.QuoteQuality)
	}
	if p.DataType != "" {
		return p.DataType
	}
	return "quote"
}

func formatPositionAsOf(p rpc.PositionView) string {
	for _, at := range []time.Time{p.QuotePriceAt, p.PriceAt, p.RegularCloseAt} {
		if !at.IsZero() {
			return formatTimeShort(at)
		}
	}
	return "—"
}

func (e *Env) formatChangePct(p *float64, width int) string {
	if p == nil {
		return padDash(width)
	}
	s := fmt.Sprintf("%+*.2f%%", width-1, *p)
	return e.colorBySign(*p, s, signPnL)
}

func formatWatchlistNumber(p *float64, width int) string {
	if p == nil {
		return padDash(width)
	}
	return fmt.Sprintf("%*.2f", width, *p)
}

func formatWatchlistRange(env *Env, low, high *float64, width int) string {
	lo, hi := "—", "—"
	if low != nil {
		lo = fmt.Sprintf("%.2f", *low)
	}
	if high != nil {
		hi = fmt.Sprintf("%.2f", *high)
	}
	return env.dim(fmt.Sprintf("%*s-%-*s", (width-1)/2, lo, width-(width-1)/2-1, hi))
}

func formatWatchlistChange(env *Env, p *float64, width int) string {
	if p == nil {
		return padDash(width)
	}
	s := fmt.Sprintf("%+*.2f", width, *p)
	return env.colorBySign(*p, s, signPnL)
}

func formatWatchlistVolume(volume, average *int64, width int) string {
	return fmt.Sprintf("%*s", width, formatVolumePart(volume)+"/"+formatVolumePart(average))
}

func formatVolumePart(v *int64) string {
	if v == nil || *v <= 0 {
		return "—"
	}
	n := float64(*v)
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", n/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", n/1e6)
	case n >= 1e3:
		return fmt.Sprintf("%.0fK", n/1e3)
	default:
		return fmt.Sprintf("%d", *v)
	}
}

func (e *Env) formatPositionPnL(v float64) string {
	if v == 0 {
		return e.dim("—")
	}
	return e.colorBySign(v, formatMoney(v), signPnL)
}

func (e *Env) formatPositionPnLPtr(v *float64) string {
	if v == nil {
		return "—"
	}
	return e.formatPositionPnL(*v)
}

// renderPortfolioSummaryTo prints the daemon-computed aggregate block when
// at least one component is populated. Empty when there are no options
// (Greeks coverage zero AND no FX rollup) so single-stock accounts don't
// see an empty header. Lines render only when their pointer is non-nil
// — never zero-substituted.
func renderPortfolioSummaryTo(env *Env, out io.Writer, r *rpc.PositionsResult) {
	if r.Portfolio == nil {
		return
	}
	p := r.Portfolio
	hasGreeks := p.EffectiveDelta != nil || p.DailyTheta != nil || p.Gamma != nil || p.Vega != nil
	hasFX := p.FXSensitivityPerPct != nil
	hasProtection := r.ProtectionCoverage != nil
	if !hasGreeks && !hasFX && !hasProtection {
		return
	}
	fmt.Fprintln(out, heroSummaryStyle(env, "Summary"))
	// All numeric values right-align to col (labelStart + labelWidth +
	// space + valueWidth) so the unit text (share-equivalents, USD,
	// / day, etc.) lands at a single column regardless of value
	// magnitude. labelWidth covers the widest label ("FX sensitivity /
	// 1%" = 19); valueWidth covers a 5-digit gamma with commas and a
	// sign (e.g. "+12,584.6938" = 12).
	const labelWidth = 22
	const valueWidth = 14
	if p.EffectiveDelta != nil {
		val := padLeftVisible(formatSignedGrouped(*p.EffectiveDelta, 1), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  share-equivalents\n", labelWidth, "Effective delta", env.bold(val))
	}
	if p.DollarDelta != nil {
		ccy := p.DollarDeltaCurrency
		if ccy == "" {
			ccy = "?"
		}
		val := padLeftVisible(formatMoneyBare(*p.DollarDelta), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  %s\n", labelWidth, "Dollar delta", env.bold(val), ccy)
	}
	if p.DailyTheta != nil {
		// Match the Dollar delta line's pattern: render bare (no symbol)
		// and name the currency to the right. MIX → render bare with an
		// explicit "(mixed currencies)" tail so a reader knows the sum
		// spans multiple ccys and can't be stamped with a single symbol.
		ccy := p.DailyThetaCurrency
		val := padLeftVisible(formatMoneyBare(*p.DailyTheta), valueWidth)
		switch {
		case *p.DailyTheta > 0:
			val = env.green(val)
		case *p.DailyTheta < 0:
			val = env.red(val)
		default:
			val = env.dim(val)
		}
		switch ccy {
		case "":
			fmt.Fprintf(out, "  %-*s  %s  / day\n", labelWidth, "Daily theta", val)
		case "MIX":
			fmt.Fprintf(out, "  %-*s  %s  / day (mixed currencies)\n", labelWidth, "Daily theta", val)
		default:
			fmt.Fprintf(out, "  %-*s  %s  %s / day\n", labelWidth, "Daily theta", val, ccy)
		}
	}
	if p.Gamma != nil {
		val := padLeftVisible(formatSignedGrouped(*p.Gamma, 4), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s\n", labelWidth, "Gamma", val)
	}
	if p.Vega != nil {
		val := padLeftVisible(formatSignedGrouped(*p.Vega, 2), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  / 1 vol pt\n", labelWidth, "Vega", val)
	}
	if p.GreeksTotal > 0 {
		cov := padLeftVisible(fmt.Sprintf("%d / %d", p.GreeksCoverage, p.GreeksTotal), valueWidth)
		if p.GreeksCoverage < p.GreeksTotal {
			fmt.Fprintf(out, "  %-*s  %s  legs (partial — model abstained or OOH)\n",
				labelWidth, "Greeks coverage", cov)
		} else {
			fmt.Fprintf(out, "  %-*s  %s  legs  %s\n",
				labelWidth, "Greeks coverage", cov, env.green("✓"))
		}
	}
	if p.FXSensitivityPerPct != nil {
		base := p.FXBaseCurrency
		if base == "" {
			base = "base"
		}
		val := padLeftVisible(formatMoneyBare(*p.FXSensitivityPerPct), valueWidth)
		fmt.Fprintf(out, "  %-*s  %s  %s per +1%% FX\n",
			labelWidth, "FX sensitivity", val, base)
	}
	if r.ProtectionCoverage != nil {
		fmt.Fprintf(out, "  %-*s  %s\n", labelWidth, "Protection coverage", stress.FormatProtectionCoverageEvidence(r.ProtectionCoverage))
	}
	fmt.Fprintln(out)
}

func anyRealized(rows []rpc.PositionView) bool {
	for _, p := range rows {
		if p.RealizedPnL != 0 {
			return true
		}
	}
	return false
}

// renderPositionsByUnderlyingTo prints one block per underlying with the
// stock leg (if any), the option legs (with inline Greeks), and a
// per-underlying Total row when there's more than one leg.
//
// Every row uses the same column layout — LEG, QTY, AVG, MARK,
// CHANGE/GREEKS, MKT VALUE, UNREAL P&L — so the eye reads down each
// column instead of zigzagging across row-type-specific layouts. Money
// columns right-align so decimal points line up; sign-coloured cells
// (day change, unrealised P&L, Δ) pad before colour wrap so visible
// widths stay correct under ANSI escapes.
func renderPositionsByUnderlyingTo(env *Env, out io.Writer, r *rpc.PositionsResult) int {
	fmt.Fprintln(out)
	unavailable := renderPositionsAuthority(env, out, r)
	if len(r.ByUnderlying) == 0 {
		if unavailable {
			fmt.Fprintf(out, "Positions unavailable%s\n\n", env.suffixBadge(r.DataType))
			return 0
		}
		fmt.Fprintf(out, "No open positions%s\n\n", env.suffixBadge(r.DataType))
		return 0
	}
	fmt.Fprintf(out, "Positions by underlying%s\n", env.suffixBadge(r.DataType))
	fmt.Fprintln(out)

	// Column header.  Widths chosen to fit realistic data: identifier
	// holds "2026-06-18 C 1191.67" (~20); change/greeks holds the
	// compact greek tuple "Δ+0.62 Γ+0.31 Θ-0.01 ν+0.01" (27 cells) or
	// the stock day-change cell "+1.32 (+0.64%)" (~14 cells).
	const (
		wLeg    = 22
		wQty    = 9
		wAvg    = 10
		wMark   = 10
		wMkt    = 13
		wUnreal = 13
	)
	// wChange sizes itself to the widest CHANGE/GREEKS cell actually
	// rendered in this call: header width floor ("CHANGE / GREEKS" =
	// 15 cells), full greek tuple at the ceiling (27 cells). When no
	// option carries captured Greeks — the gateway pipeline goes silent
	// OOH, or the model-computation tick never landed — every greek
	// cell is the 15-cell placeholder, so the column shrinks to 15 and
	// the eye stops reading the trailing pad as an empty column.
	wChange := len("CHANGE / GREEKS")
	for _, g := range r.ByUnderlying {
		if g.Stock != nil {
			if v := visibleLen(env.formatDayChange(g.Stock.DayChangeMoney, g.Stock.DayChangePct, g.Stock.Currency, 0)); v > wChange {
				wChange = v
			}
		}
		for _, o := range g.Options {
			if v := visibleLen(env.formatGreeksLine(o)); v > wChange {
				wChange = v
			}
		}
	}
	header := fmt.Sprintf("  %-*s  %*s  %*s  %*s  %-*s  %*s  %*s",
		wLeg, "LEG", wQty, "QTY", wAvg, "AVG", wMark, "MARK",
		wChange, "CHANGE / GREEKS", wMkt, "MKT VALUE", wUnreal, "UNREAL P&L")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))

	writeRow := func(leg, qty, avg, mark, change, mkt, unreal string) {
		fmt.Fprintf(out, "  %s  %s  %s  %s  %s  %s  %s\n",
			padRightVisible(leg, wLeg),
			padLeftVisible(qty, wQty),
			padLeftVisible(avg, wAvg),
			padLeftVisible(mark, wMark),
			padRightVisible(change, wChange),
			padLeftVisible(mkt, wMkt),
			padLeftVisible(unreal, wUnreal))
	}

	for _, g := range r.ByUnderlying {
		fmt.Fprintln(out, "  "+env.bold(g.Underlying))

		if g.Stock != nil {
			s := g.Stock
			writeRow(
				"Stock",
				fmt.Sprintf("%.0f sh", s.Quantity),
				formatMoney(avgCostPerShare(*s)),
				formatMoney(s.Mark),
				env.formatDayChange(s.DayChangeMoney, s.DayChangePct, s.Currency, 0),
				formatMoney(s.MarketValue),
				env.formatPnLRight(s.UnrealizedPnL, wUnreal))
		}
		for _, o := range g.Options {
			writeRow(
				fmt.Sprintf("%s %s %.2f", formatExpiry(o.Expiry), o.Right, o.Strike),
				fmt.Sprintf("%.0f ct", o.Quantity),
				formatMoney(avgCostPerShare(o)),
				formatMoney(o.Mark),
				env.formatGreeksLine(o),
				formatMoney(o.MarketValue),
				env.formatPnLRight(o.UnrealizedPnL, wUnreal))
		}

		// Total row only when there's more than one leg — for a single
		// stock or a single option the per-leg row already carries the
		// values that would otherwise be duplicated.
		legs := 0
		if g.Stock != nil {
			legs++
		}
		legs += len(g.Options)
		if legs > 1 {
			writeRow(
				env.dim("─── Total"),
				"", "", "", "",
				formatMoney(g.GroupMarketValue),
				env.formatPnLRight(g.GroupUnrealizedPnL, wUnreal))
		}
	}
	fmt.Fprintln(out)
	renderPortfolioSummaryTo(env, out, r)
	fmt.Fprintf(out, "  %d underlyings  ·  as of %s\n",
		len(r.ByUnderlying), formatTimeShort(positionsDisplayAsOf(r)))
	return 0
}

// formatDayChange renders the combined "+$132.00 (+0.64%)" cell — the
// dollar impact on the position (qty × per-share move for stocks; qty ×
// multiplier × move for options) followed by the underlying's percent.
// Money leads because it's the answer most traders are scanning for;
// percent stays for cross-symbol comparability. Both painted by sign as
// a unit. Em-dash placeholder of width w when money is unavailable —
// per-share change without size is misleading on options (a $0.10 leg
// move on 10 contracts is $100, not $0.10).
//
// money carries position currency for the money side; pct is unitless.
// Padding lives outside the ANSI wrap so visible width matches w
// regardless of color state.
func (e *Env) formatDayChange(money, pct *float64, ccy string, w int) string {
	if money == nil || pct == nil {
		return padDash(w)
	}
	moneyStr := formatMoneyCcy(*money, ccy)
	if *money > 0 {
		moneyStr = "+" + moneyStr
	}
	s := fmt.Sprintf("%s (%+.2f%%)", moneyStr, *pct)
	if pad := w - len(s); pad > 0 {
		s += strings.Repeat(" ", pad)
	}
	return e.colorBySign(*money, s, signPnL)
}

// formatGreeksLine renders a per-leg Greeks tuple in the most compact
// form that stays readable: no space between symbol and number, single
// space between greeks, 2 decimals everywhere ("Δ+0.62 Γ+0.31 Θ-0.01
// ν+0.01" — 27 cells). Delta carries sign coloring (the headline risk
// component); gamma / theta / vega print with sign but no color so the
// eye is drawn to delta first.
//
// Greeks that the daemon didn't capture render as a dim "Δ  —" / "Γ  —"
// / "Θ  —" / "ν  —" placeholder so the column shape stays consistent
// across rows — a blank cell would otherwise read as "Greeks don't
// apply here" rather than the truth ("not yet captured, retry").
func (e *Env) formatGreeksLine(o rpc.PositionView) string {
	delta := e.dim("Δ —")
	if o.Delta != nil {
		s := fmt.Sprintf("Δ%+0.2f", *o.Delta)
		switch {
		case *o.Delta > 0:
			s = e.green(s)
		case *o.Delta < 0:
			s = e.red(s)
		}
		delta = s
	}
	gamma := e.dim("Γ —")
	if o.Gamma != nil {
		gamma = fmt.Sprintf("Γ%+0.2f", *o.Gamma)
	}
	theta := e.dim("Θ —")
	if o.Theta != nil {
		theta = fmt.Sprintf("Θ%+0.2f", *o.Theta)
	}
	vega := e.dim("ν —")
	if o.Vega != nil {
		vega = fmt.Sprintf("ν%+0.2f", *o.Vega)
	}
	return delta + " " + gamma + " " + theta + " " + vega
}

// avgCostPerShare normalises AvgCost to per-share units for visual
// comparison with Mark in the same row. IBKR's averageCost is per-share
// for stocks but per-contract (multiplier-inclusive) for options — so a
// $3.00 premium call comes off the wire as $300, which reads like a typo
// next to a $3 mark. Dividing by multiplier on OPT restores symmetry.
// JSON output stays IBKR-faithful; only the rendered column normalises.
//
// The SecType comparison uses rpc.SecTypeOption as the single source of truth
// for the wire value.
func avgCostPerShare(p rpc.PositionView) float64 {
	if p.SecType == rpc.SecTypeOption && p.Multiplier > 0 {
		return p.AvgCost / float64(p.Multiplier)
	}
	return p.AvgCost
}

func formatExpiry(s string) string {
	// IBKR returns YYYYMMDD; render YYYY-MM-DD if length matches
	if len(s) == 8 {
		return s[:4] + "-" + s[4:6] + "-" + s[6:8]
	}
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}
