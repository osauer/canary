package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/watchlist"
)

var watchlistDefaultPath = watchlist.DefaultPath

func runWatchlist(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "watch")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	add := fs.Bool("add", false, "add symbol(s) to the local watchlist")
	remove := fs.Bool("remove", false, "remove symbol(s) from the local watchlist")
	clear := fs.Bool("clear", false, "remove every symbol from the local watchlist")
	list := fs.Bool("list", false, "list saved symbols without quote context")
	quotes := fs.Bool("quotes", false, "show saved symbols with enriched quote context (default)")
	live := fs.Bool("watch", false, "quote saved symbols repeatedly until Ctrl-C")
	rate := fs.Duration("rate", time.Second, "poll interval for --watch")
	timeout := fs.Duration("timeout", 5*time.Second, "per-symbol quote timeout for --quotes/--watch")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}

	rest := fs.Args()
	symbols := watchlist.NormalizeSymbols(strings.Join(rest, ","))
	actions := boolCount(*add, *remove, *clear, *list, *quotes, *live)
	if actions == 0 {
		*quotes = true
	}
	if actions > 1 {
		return fail(env, "watch: choose one of --add, --remove, --clear, --list, --quotes, or --watch")
	}
	if (*add || *remove) && len(symbols) == 0 {
		return fail(env, "watch: symbol required with --add or --remove")
	}
	if (*clear || *list || *quotes || *live) && len(symbols) > 0 && !*add && !*remove {
		return fail(env, "watch: unexpected symbol(s) with --list, --clear, --quotes, or --watch")
	}
	if *live && *jsonOut {
		return fail(env, "watch: --watch and --json are mutually exclusive")
	}
	if *quotes && env.Conn == nil {
		return fail(env, "watch: quote view requires the canary daemon; use --list for the offline symbol list")
	}

	store, err := openWatchlistStore()
	if err != nil {
		return fail(env, "watch: %v", err)
	}

	switch {
	case *add:
		snap, err := store.Add(symbols)
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *remove:
		snap, err := store.Remove(symbols)
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *clear:
		snap, err := store.Clear()
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	case *live:
		if env.Conn == nil {
			return fail(env, "watch: --watch requires the canary daemon")
		}
		fetchAndRender := func(out io.Writer) int {
			snap, err := store.Snapshot()
			if err != nil {
				return fail(env, "watch: %v", err)
			}
			if len(snap.Symbols) == 0 {
				return renderWatchlistTextTo(env, out, snap)
			}
			return renderWatchlistQuotes(ctx, env, out, snap, *timeout, false)
		}
		return runWatch(ctx, env, *rate, "watch", fetchAndRender)
	case *quotes:
		snap, err := store.Snapshot()
		if err != nil {
			return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
		}
		if len(snap.Symbols) == 0 {
			if *jsonOut {
				return printJSON(env, rpc.WatchlistResult{Name: snap.Name, Symbols: []string{}, Rows: []rpc.WatchlistRow{}, AsOf: time.Now()})
			}
			return renderWatchlistResult(env, env.Stdout, snap, nil, *jsonOut)
		}
		return renderWatchlistQuotes(ctx, env, env.Stdout, snap, *timeout, *jsonOut)
	default:
		snap, err := store.Snapshot()
		return renderWatchlistResult(env, env.Stdout, snap, err, *jsonOut)
	}
}

func openWatchlistStore() (*watchlist.Store, error) {
	path, err := watchlistDefaultPath()
	if err != nil {
		return nil, err
	}
	return watchlist.New(path), nil
}

func renderWatchlistResult(env *Env, out io.Writer, snap *watchlist.Snapshot, err error, jsonOut bool) int {
	if err != nil {
		return fail(env, "watch: %v", err)
	}
	if jsonOut {
		return printJSONTo(env, out, snap)
	}
	return renderWatchlistTextTo(env, out, snap)
}

func renderWatchlistTextTo(env *Env, out io.Writer, snap *watchlist.Snapshot) int {
	fmt.Fprintln(out)
	if len(snap.Symbols) == 0 {
		fmt.Fprintln(out, "No symbols in watchlist")
		fmt.Fprintf(out, "  as of %s\n", formatTimeShort(snap.AsOf))
		return 0
	}
	const wSymbol = 9
	fmt.Fprintln(out, "Watchlist")
	header := fmt.Sprintf("  %-*s", wSymbol, "SYMBOL")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, sym := range snap.Symbols {
		fmt.Fprintf(out, "  %-*s\n", wSymbol, sym)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %d symbols  ·  as of %s\n", len(snap.Symbols), formatTimeShort(snap.AsOf))
	return 0
}

func renderWatchlistQuotes(ctx context.Context, env *Env, out io.Writer, snap *watchlist.Snapshot, timeout time.Duration, jsonOut bool) int {
	res, err := fetchWatchlistQuotes(ctx, env, snap, timeout)
	if err != nil {
		return fail(env, "watch: %v", err)
	}
	if jsonOut {
		return printJSONTo(env, out, res)
	}
	wenv := *env
	wenv.Stdout = out
	return renderWatchlistQuoteText(&wenv, out, res)
}

func fetchWatchlistQuotes(ctx context.Context, env *Env, snap *watchlist.Snapshot, timeout time.Duration) (*rpc.WatchlistResult, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	res := &rpc.WatchlistResult{
		Name:    snap.Name,
		Symbols: append([]string(nil), snap.Symbols...),
		Rows:    make([]rpc.WatchlistRow, 0, len(snap.Symbols)),
		AsOf:    time.Now(),
	}
	holdings := fetchWatchlistStockHoldings(ctx, env)
	for _, sym := range snap.Symbols {
		var q rpc.Quote
		params := rpc.QuoteSnapshotParams{
			Contract:         watchlistQuoteContract(sym, holdings[sym]),
			TimeoutMs:        int(timeout.Milliseconds()),
			IncludeLiquidity: true,
		}
		row := rpc.WatchlistRow{}
		if err := env.Conn.Call(ctx, rpc.MethodQuoteSnapshot, params, &q); err != nil {
			if isGatewayUnavailable(err.Error()) {
				return nil, err
			}
			row.Quote = rpc.Quote{Symbol: sym}
			row.Error = err.Error()
		} else {
			row.Quote = q
		}
		if h, ok := holdings[sym]; ok {
			row.Holding = h
		}
		res.Rows = append(res.Rows, row)
	}
	return res, nil
}

func fetchWatchlistStockHoldings(ctx context.Context, env *Env) map[string]*rpc.WatchlistHolding {
	out := map[string]*rpc.WatchlistHolding{}
	if env == nil || env.Conn == nil {
		return out
	}
	var pos rpc.PositionsResult
	if err := env.Conn.Call(ctx, rpc.MethodPositionsList, rpc.PositionsListParams{Type: "stk"}, &pos); err != nil {
		return out
	}
	for _, p := range pos.Stocks {
		// The non-option slice carries every secType that is not OPT — bonds,
		// bills, funds, futures, cash. Keyed by bare symbol, a treasury
		// symbolled "T" would render as an AT&T holding and feed its currency
		// and exchange into the quote contract. Only rows a bare-symbol stock
		// quote actually describes may join the watchlist.
		if !rpc.PositionQuotesAsStock(p) {
			continue
		}
		out[strings.ToUpper(p.Symbol)] = &rpc.WatchlistHolding{
			Quantity:      p.Quantity,
			AvgCost:       p.AvgCost,
			Mark:          p.Mark,
			MarketValue:   p.MarketValue,
			UnrealizedPnL: p.UnrealizedPnL,
			DailyPnL:      p.DailyPnL,
			Exchange:      p.Exchange,
			Currency:      p.Currency,
		}
	}
	return out
}

func watchlistQuoteContract(sym string, h *rpc.WatchlistHolding) rpc.ContractParams {
	c := rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "USD"}
	if h == nil {
		return c
	}
	if h.Currency != "" {
		c.Currency = h.Currency
	}
	if h.Exchange != "" {
		if strings.EqualFold(h.Exchange, "IBIS") && strings.EqualFold(c.Currency, "EUR") {
			c.Market = "de"
		} else {
			c.Exchange = h.Exchange
		}
	}
	return c
}

func renderWatchlistQuoteText(env *Env, out io.Writer, r *rpc.WatchlistResult) int {
	fmt.Fprintln(out)
	if r == nil || len(r.Rows) == 0 {
		fmt.Fprintln(out, "No symbols in watchlist")
		if r != nil {
			fmt.Fprintf(out, "  as of %s\n", formatTimeShort(r.AsOf))
		}
		return 0
	}
	fmt.Fprintln(out, "Watchlist")
	const (
		wSymbol = 7
		wPos    = 8
		wCCY    = 3
		wPrice  = 9
		wChange = 8
		wPct    = 7
		wRange  = 15
		wVol    = 11
		wADV    = 9
		wData   = 7
		wAsOf   = 40
	)
	header := fmt.Sprintf("  %-*s %*s %*s %*s %*s %*s %*s %*s %-*s %-*s %*s %*s %-*s %-*s",
		wSymbol, "SYMBOL", wPos, "POS", wCCY, "CCY", wPrice, "CLOSE", wChange, "C-CHG", wPct, "C%",
		wPrice, "QUOTE", wPct, "Q%", wRange, "DAY", wRange, "52W", wVol, "VOL/AVG", wADV, "ADV$20", wData, "DATA", wAsOf, "AS OF")
	fmt.Fprintln(out, env.dim(header))
	fmt.Fprintln(out, env.dim(strings.Repeat("─", visibleLen(header))))
	for _, row := range r.Rows {
		if row.Error != "" {
			fmt.Fprintf(out, "  %-*s  %s\n", wSymbol, row.Symbol, env.red(row.Error))
			continue
		}
		ccy := row.Contract.Currency
		if ccy == "" {
			ccy = "USD"
		}
		fmt.Fprintf(out, "  %-*s %s %*s %s %s %s %s %s %s %s %s %s %s %s\n",
			wSymbol, row.Symbol,
			padLeftVisible(formatWatchlistHolding(row.Holding), wPos),
			wCCY, ccy,
			formatWatchlistNumber(watchlistRegularClose(row), wPrice),
			formatWatchlistChange(env, watchlistRegularChange(row), wChange),
			env.formatChangePct(watchlistRegularChangePct(row), wPct),
			formatWatchlistNumber(watchlistQuotePrice(row), wPrice),
			env.formatChangePct(watchlistQuoteChangePct(row), wPct),
			formatWatchlistRange(env, row.DayLow, row.DayHigh, wRange),
			formatWatchlistRange(env, row.Week52Low, row.Week52High, wRange),
			formatWatchlistVolume(row.Volume, row.AvgVolume, wVol),
			formatTechnicalDollarVolume(row.AvgDollarVolume20D, wADV),
			padRightVisible(formatWatchlistData(env, row), wData),
			padRightVisible(formatWatchlistAsOf(row), wAsOf),
		)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  %d symbols  ·  as of %s\n", len(r.Symbols), formatTimeShort(r.AsOf))
	return 0
}

func formatWatchlistHolding(h *rpc.WatchlistHolding) string {
	if h == nil || h.Quantity == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f sh", h.Quantity)
}

func formatWatchlistNumber(p *float64, w int) string {
	if p == nil {
		return padDash(w)
	}
	return fmt.Sprintf("%*.2f", w, *p)
}

func formatWatchlistRange(env *Env, lo, hi *float64, w int) string {
	loS := "—"
	if lo != nil {
		loS = fmt.Sprintf("%.2f", *lo)
	}
	hiS := "—"
	if hi != nil {
		hiS = fmt.Sprintf("%.2f", *hi)
	}
	return env.dim(fmt.Sprintf("%*s-%-*s", (w-1)/2, loS, w-(w-1)/2-1, hiS))
}

func formatWatchlistChange(env *Env, p *float64, w int) string {
	if p == nil {
		return padDash(w)
	}
	abs := *p
	sign := "+"
	if abs < 0 {
		sign = "-"
		abs = -abs
	}
	s := fmt.Sprintf("%*s", w, fmt.Sprintf("%s%.2f", sign, abs))
	switch {
	case *p > 0:
		return env.green(s)
	case *p < 0:
		return env.red(s)
	default:
		return env.dim(s)
	}
}

func formatWatchlistVolume(volume, avg *int64, w int) string {
	left := formatWatchlistVolumePart(volume)
	right := formatWatchlistVolumePart(avg)
	return fmt.Sprintf("%*s", w, left+"/"+right)
}

func formatWatchlistVolumePart(v *int64) string {
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

func watchlistRegularClose(row rpc.WatchlistRow) *float64 {
	if row.RegularClose != nil {
		return row.RegularClose
	}
	if row.PriceSource == "historical_close" || row.PriceSource == "prev_close" {
		return row.Price
	}
	return row.PrevClose
}

func watchlistRegularChange(row rpc.WatchlistRow) *float64 {
	if row.RegularChange != nil {
		return row.RegularChange
	}
	if row.PriceSource == "historical_close" {
		return row.Change
	}
	return nil
}

func watchlistRegularChangePct(row rpc.WatchlistRow) *float64 {
	if row.RegularChangePct != nil {
		return row.RegularChangePct
	}
	if row.PriceSource == "historical_close" {
		return row.ChangePct
	}
	return nil
}

func watchlistQuotePrice(row rpc.WatchlistRow) *float64 {
	if row.QuotePrice != nil {
		return row.QuotePrice
	}
	switch row.PriceSource {
	case "last", "mark", "mid", "bid", "ask":
		return row.Price
	default:
		return nil
	}
}

func watchlistQuoteChangePct(row rpc.WatchlistRow) *float64 {
	if row.QuoteChangePct != nil {
		return row.QuoteChangePct
	}
	quote := watchlistQuotePrice(row)
	close := watchlistRegularClose(row)
	if quote == nil || close == nil || *close == 0 {
		return nil
	}
	v := (*quote - *close) / *close * 100
	return &v
}

func formatWatchlistData(env *Env, row rpc.WatchlistRow) string {
	hasQuote := watchlistQuotePrice(row) != nil
	hasClose := watchlistRegularClose(row) != nil
	if !hasQuote && !hasClose {
		return env.yellow("no data")
	}
	if !hasQuote && hasClose {
		return "close"
	}
	switch row.QuoteQuality {
	case "wide":
		return env.yellow("wide")
	case "indicative":
		return env.yellow("indic")
	case "prev_close":
		return env.yellow("prev")
	case "stale":
		return env.yellow("stale")
	case "missing":
		return env.red("missing")
	}
	if row.Stale || row.StaleReason != "" {
		return env.yellow("stale")
	}
	switch row.QuotePriceSource {
	case "historical_close":
		return "hist"
	}
	switch row.PriceSource {
	case "historical_close":
		return "hist"
	}
	switch row.DataType {
	case "", rpc.MarketDataLive:
		return rpc.MarketDataLive
	case rpc.MarketDataDelayed:
		return env.yellow("delay")
	case rpc.MarketDataFrozen:
		return env.yellow("frozen")
	case rpc.MarketDataDelayedFrozen:
		return env.yellow("dly-frz")
	default:
		return env.yellow(row.DataType)
	}
}

func formatWatchlistAsOf(row rpc.WatchlistRow) string {
	loc := time.Local
	if row.SessionContext != nil && row.SessionContext.Timezone != "" {
		if l, err := time.LoadLocation(row.SessionContext.Timezone); err == nil {
			loc = l
		}
	}
	stamps := make([]string, 0, 2)
	closeAt := row.RegularCloseAt
	if closeAt.IsZero() && (row.PriceSource == "prev_close" || row.PriceSource == "historical_close") {
		closeAt = row.PriceAt
	}
	if !closeAt.IsZero() && watchlistRegularClose(row) != nil {
		stamps = append(stamps, "close "+closeAt.In(loc).Format("Jan02"))
	}
	quoteAt := row.QuotePriceAt
	if quoteAt.IsZero() && watchlistQuotePrice(row) != nil && row.PriceSource != "prev_close" && row.PriceSource != "historical_close" {
		quoteAt = row.PriceAt
	}
	if !quoteAt.IsZero() && watchlistQuotePrice(row) != nil {
		stamps = append(stamps, "quote "+quoteAt.In(loc).Format("Jan02 15:04 MST"))
	}
	if len(stamps) == 0 {
		return "—"
	}
	return joinWatchlistAsOf(watchlistMarketState(row), strings.Join(stamps, " / "))
}

func joinWatchlistAsOf(state, stamp string) string {
	if state == "" {
		return stamp
	}
	return state + " " + stamp
}

func watchlistMarketState(row rpc.WatchlistRow) string {
	s := row.SessionContext
	if s == nil {
		if (watchlistQuotePrice(row) != nil || watchlistRegularClose(row) != nil) && !row.Stale {
			return "open"
		}
		return ""
	}
	if s.IsOpen {
		return "open"
	}
	if watchlistIsPreMarket(row, *s) {
		return "pre-market"
	}
	return "closed"
}

func watchlistIsPreMarket(row rpc.WatchlistRow, s rpc.MarketSession) bool {
	if s.Open.IsZero() {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(s.State))
	if state != "regular" && state != "early_close" {
		return false
	}
	at := row.AsOf
	if at.IsZero() {
		at = time.Now()
	}
	return at.In(watchlistSessionLocation(s)).Before(s.Open.In(watchlistSessionLocation(s)))
}

func watchlistSessionLocation(s rpc.MarketSession) *time.Location {
	if s.Timezone != "" {
		if loc, err := time.LoadLocation(s.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

func boolCount(values ...bool) int {
	var n int
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}
