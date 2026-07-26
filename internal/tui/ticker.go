package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/rpc"
)

type tickerCell struct {
	r    rune
	ansi string
}

func tickerLine(m *model, width int) string {
	if !tickerReady(m.snapshot) {
		return ""
	}
	items := tickerItems(m.snapshot)
	if len(items) == 0 {
		return ""
	}
	prefix := styleInfo("MKT ")
	available := width - visibleWidth(prefix)
	if available <= 0 {
		return prefix
	}
	cells := tickerTapeCells(items)
	if len(cells) == 0 {
		return prefix
	}
	return prefix + renderTickerCells(scrolledTickerCells(cells, m.tickerIndex, available))
}

func tickerReady(snap live.Snapshot) bool {
	if snap.Status == nil || !snap.Status.Connected {
		return false
	}
	if snap.Positions != nil && (len(snap.Positions.Stocks) > 0 || len(snap.Positions.Options) > 0 || len(snap.Positions.ByUnderlying) > 0) {
		return true
	}
	if hasPriceQuote(snap) {
		return true
	}
	if snap.Account != nil && len(snap.Account.CurrencyExposure) > 0 {
		return true
	}
	return false
}

func hasPriceQuote(snap live.Snapshot) bool {
	if snap.Quotes == nil {
		return false
	}
	for _, quote := range snap.Quotes.Quotes {
		if quoteTickerPrice(quote) != nil {
			return true
		}
	}
	return false
}

func tickerTapeCells(items []string) []tickerCell {
	cells := []tickerCell{}
	for i, item := range items {
		if i > 0 {
			cells = append(cells, cellsFromANSI(styleDim("   |   "))...)
		}
		cells = append(cells, cellsFromANSI(item)...)
	}
	cells = append(cells, cellsFromANSI(styleDim("          "))...)
	return cells
}

func cellsFromANSI(s string) []tickerCell {
	cells := []tickerCell{}
	active := ""
	for len(s) > 0 {
		if seq, n, ok := ansiSequence(s); ok {
			if seq != "" {
				if seq == ansiReset {
					active = ""
				} else {
					active = seq
				}
			}
			s = s[n:]
			continue
		}
		r, n := firstVisibleRune(s)
		s = s[n:]
		if r == 0 {
			continue
		}
		cells = append(cells, tickerCell{r: r, ansi: active})
	}
	return cells
}

func scrolledTickerCells(cells []tickerCell, offset, width int) []tickerCell {
	if width <= 0 || len(cells) == 0 {
		return nil
	}
	offset %= len(cells)
	out := make([]tickerCell, 0, width)
	for range width {
		out = append(out, cells[offset])
		offset = (offset + 1) % len(cells)
	}
	return out
}

func renderTickerCells(cells []tickerCell) string {
	var b strings.Builder
	active := ""
	for _, cell := range cells {
		if cell.ansi != active {
			if active != "" {
				b.WriteString(ansiReset)
			}
			if cell.ansi != "" {
				b.WriteString(cell.ansi)
			}
			active = cell.ansi
		}
		b.WriteRune(cell.r)
	}
	if active != "" {
		b.WriteString(ansiReset)
	}
	return b.String()
}

func tickerSymbols(snap live.Snapshot) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(sym string) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || seen[sym] {
			return
		}
		seen[sym] = true
		out = append(out, sym)
	}
	if snap.Positions != nil {
		for _, row := range snap.Positions.Stocks {
			add(row.Symbol)
		}
		for _, row := range snap.Positions.Options {
			add(row.Symbol)
		}
		for _, group := range snap.Positions.ByUnderlying {
			add(group.Underlying)
		}
	}
	if snap.Account != nil {
		base := strings.ToUpper(strings.TrimSpace(snap.Account.BaseCurrency))
		if base == "" {
			base = "USD"
		}
		for _, ex := range snap.Account.CurrencyExposure {
			ccy := strings.ToUpper(strings.TrimSpace(ex.Currency))
			if ccy != "" && ccy != base {
				add(base + "." + ccy)
			}
		}
	}
	for _, sym := range []string{"SPY", "QQQ", "VIX"} {
		add(sym)
	}
	return out
}

func tickerItems(snap live.Snapshot) []string {
	seen := map[string]bool{}
	items := []string{}
	addSeen := func(sym string) bool {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || seen[sym] {
			return false
		}
		seen[sym] = true
		return true
	}
	if snap.Positions != nil {
		for _, row := range snap.Positions.Stocks {
			if addSeen(row.Symbol) {
				items = append(items, formatPositionRowTicker(row))
			}
		}
		for _, row := range snap.Positions.Options {
			if addSeen(row.Symbol) {
				items = append(items, formatPositionRowTicker(row))
			}
		}
		for _, group := range snap.Positions.ByUnderlying {
			if group.Stock == nil && addSeen(group.Underlying) {
				items = append(items, formatPositionGroupTicker(group))
			}
		}
	}
	for _, sym := range tickerSymbols(snap) {
		if !addSeen(sym) {
			continue
		}
		if q, ok := quoteFor(snap, sym); ok {
			items = append(items, formatQuoteTicker(sym, q))
			continue
		}
		if p, ok := positionFor(snap, sym); ok {
			items = append(items, formatPositionTicker(sym, p))
			continue
		}
		items = append(items, styleStrong(sym))
	}
	return items
}

func tickerSignature(snap live.Snapshot) string {
	seen := map[string]bool{}
	keys := []string{}
	add := func(kind, sym string) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || seen[kind+":"+sym] {
			return
		}
		seen[kind+":"+sym] = true
		keys = append(keys, kind+":"+sym)
	}
	if snap.Positions != nil {
		for _, row := range snap.Positions.Stocks {
			add("position", row.Symbol)
		}
		for _, row := range snap.Positions.Options {
			add("position", row.Symbol)
		}
		for _, group := range snap.Positions.ByUnderlying {
			if group.Stock == nil {
				add("position", group.Underlying)
			}
		}
	}
	for _, sym := range tickerSymbols(snap) {
		add("symbol", sym)
	}
	return strings.Join(keys, "\x00")
}

func quoteFor(snap live.Snapshot, sym string) (rpc.Quote, bool) {
	if snap.Quotes == nil {
		return rpc.Quote{}, false
	}
	q, ok := snap.Quotes.Quotes[sym]
	return q, ok
}

func positionFor(snap live.Snapshot, sym string) (rpc.UnderlyingExposure, bool) {
	if snap.Positions == nil || snap.Positions.Portfolio == nil {
		return rpc.UnderlyingExposure{}, false
	}
	for _, row := range snap.Positions.Portfolio.ExposureBase {
		if strings.EqualFold(row.Underlying, sym) {
			return row, true
		}
	}
	return rpc.UnderlyingExposure{}, false
}

func formatQuoteTicker(sym string, q rpc.Quote) string {
	price := quoteTickerPrice(q)
	if price == nil {
		return styleStrong(sym)
	}
	out := fmt.Sprintf("%s %.2f", styleStrong(sym), *price)
	if changePct := quoteTickerChangePct(q); changePct != nil {
		out += " " + signedTickerValue(*changePct, fmt.Sprintf("%+.2f%%", *changePct))
	}
	if q.DataType != "" && q.DataType != rpc.MarketDataLive {
		out += " " + styleDim(q.DataType)
	}
	return out
}

func quoteTickerPrice(q rpc.Quote) *float64 {
	if q.Price != nil {
		return q.Price
	}
	if q.QuotePrice != nil {
		return q.QuotePrice
	}
	return q.Last
}

func quoteTickerChangePct(q rpc.Quote) *float64 {
	if q.QuoteChangePct != nil {
		return q.QuoteChangePct
	}
	if q.ChangePct != nil {
		return q.ChangePct
	}
	if q.RegularChangePct != nil {
		return q.RegularChangePct
	}
	price := quoteTickerPrice(q)
	if price != nil && q.PrevClose != nil && *q.PrevClose != 0 {
		changePct := (*price - *q.PrevClose) / *q.PrevClose * 100
		return &changePct
	}
	return nil
}

func formatPositionTicker(sym string, p rpc.UnderlyingExposure) string {
	out := fmt.Sprintf("%s %.0f", styleStrong(sym), p.MarketValueBase)
	if p.MarketValuePctNLV != nil {
		out += fmt.Sprintf(" %.1f%%", *p.MarketValuePctNLV)
	}
	if p.DailyPnLBase != nil {
		out += " " + signedTickerValue(*p.DailyPnLBase, fmt.Sprintf("%+.0f", *p.DailyPnLBase))
	}
	return out
}

func formatPositionRowTicker(row rpc.PositionView) string {
	out := styleStrong(strings.ToUpper(row.Symbol))
	if price := positionTickerPrice(row); price != nil {
		out += fmt.Sprintf(" %.2f", *price)
	}
	switch changePct := positionTickerChangePct(row); {
	case changePct != nil:
		out += " " + signedTickerValue(*changePct, fmt.Sprintf("%+.2f%%", *changePct))
	case row.DailyPnLBase != nil:
		out += " " + signedTickerValue(*row.DailyPnLBase, fmt.Sprintf("day %+.0f", *row.DailyPnLBase))
	case row.DailyPnL != nil:
		out += " " + signedTickerValue(*row.DailyPnL, fmt.Sprintf("day %+.0f", *row.DailyPnL))
	}
	return out
}

func positionTickerChangePct(row rpc.PositionView) *float64 {
	if row.DayChangePct != nil {
		return row.DayChangePct
	}
	if row.QuoteChangePct != nil {
		return row.QuoteChangePct
	}
	if row.RegularChangePct != nil {
		return row.RegularChangePct
	}
	price := positionTickerPrice(row)
	if price != nil && row.PrevClose != nil && *row.PrevClose != 0 {
		changePct := (*price - *row.PrevClose) / *row.PrevClose * 100
		return &changePct
	}
	if row.Mark != 0 && row.RegularClose != nil && *row.RegularClose != 0 {
		changePct := (row.Mark - *row.RegularClose) / *row.RegularClose * 100
		return &changePct
	}
	if row.QuotePrice != nil && row.RegularClose != nil && *row.RegularClose != 0 {
		changePct := (*row.QuotePrice - *row.RegularClose) / *row.RegularClose * 100
		return &changePct
	}
	return nil
}

func formatPositionGroupTicker(group rpc.PositionGroup) string {
	out := styleStrong(strings.ToUpper(group.Underlying))
	value := group.GroupMarketValue
	if group.GroupMarketValueBase != nil {
		value = *group.GroupMarketValueBase
	}
	out += fmt.Sprintf(" %.0f", value)
	if group.GroupDailyPnLBase != nil {
		out += " " + signedTickerValue(*group.GroupDailyPnLBase, fmt.Sprintf("day %+.0f", *group.GroupDailyPnLBase))
		return out
	}
	pnl := group.GroupUnrealizedPnL
	if group.GroupUnrealizedPnLBase != nil {
		pnl = *group.GroupUnrealizedPnLBase
	}
	out += " " + signedTickerValue(pnl, fmt.Sprintf("%+.0f", pnl))
	return out
}

func positionTickerPrice(row rpc.PositionView) *float64 {
	if row.QuotePrice != nil {
		return row.QuotePrice
	}
	if row.Mark != 0 {
		return &row.Mark
	}
	if row.ValuationMark != 0 {
		return &row.ValuationMark
	}
	return row.RegularClose
}

func signedTickerValue(v float64, s string) string {
	switch {
	case v > 0:
		return styleOK(s)
	case v < 0:
		return styleDanger(s)
	default:
		return styleDim(s)
	}
}

func dynamicSymbols(snap live.Snapshot) []string {
	out := tickerSymbols(snap)
	slices.Sort(out)
	return out
}
