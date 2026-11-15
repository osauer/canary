package chainrec

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
)

// TradingDays answers whether a US equity date is a trading session. known is
// false outside the embedded calendar's coverage.
type TradingDays interface {
	IsTradingDay(date string) (trading, known bool)
}

// USEquityDays is TradingDays over the embedded official calendar.
type USEquityDays struct {
	cal  *marketcal.Calendar
	memo map[string][2]bool
}

// NewUSEquityDays returns a calendar-backed TradingDays.
func NewUSEquityDays(cal *marketcal.Calendar) *USEquityDays {
	if cal == nil {
		cal = marketcal.New()
	}
	return &USEquityDays{cal: cal, memo: map[string][2]bool{}}
}

// IsTradingDay reports a regular or early-close session on date.
func (d *USEquityDays) IsTradingDay(date string) (bool, bool) {
	if v, ok := d.memo[date]; ok {
		return v[0], v[1]
	}
	res, err := d.cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: date, Days: 1})
	var v [2]bool
	if err == nil {
		switch res.Session.State {
		case marketcal.StateRegular, marketcal.StateEarlyClose:
			v = [2]bool{true, true}
		case marketcal.StateHoliday, marketcal.StateClosed:
			v = [2]bool{false, true}
		}
	}
	d.memo[date] = v
	return v[0], v[1]
}

func tradingDay(days TradingDays, t time.Time) bool {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return false
	}
	if days == nil {
		return true
	}
	trading, known := days.IsTradingDay(t.Format(time.DateOnly))
	return trading || !known
}

// standardMonthly is the month's standard expiration: the third Friday, or
// the trading day before it when that Friday is an exchange holiday.
func standardMonthly(year int, month time.Month, days TradingDays) time.Time {
	first := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(time.Friday) - int(first.Weekday()) + 7) % 7
	d := first.AddDate(0, 0, offset+14)
	for range 5 {
		if tradingDay(days, d) {
			return d
		}
		d = d.AddDate(0, 0, -1)
	}
	return d
}

func previousTradingDay(t time.Time, days TradingDays) time.Time {
	d := t.AddDate(0, 0, -1)
	for range 10 {
		if tradingDay(days, d) {
			return d
		}
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// SelectExpiries picks the two nearest standard monthly expiries and the
// nearest weekly after today (YYYY-MM-DD, New York). Expiries that end today
// are excluded: a panel tracks contracts across sessions.
//
// Monthlies use the symbol's own trading class (SPY, SPX): the third-Friday
// listing, or the listing on the trading day before it, which is how IBKR
// keys AM-settled index monthlies (SPX AM on Thursday). The weekly is the
// last listed expiry of its calendar week — a Friday, or a Thursday when the
// Friday is a holiday — that is not a standard monthly date, in the symbol's
// class when listed there, otherwise another class (SPXW for SPX).
func SelectExpiries(symbol string, listed []ListedExpiry, today string, days TradingDays) []SelectedExpiry {
	strikes := map[string]map[string][]float64{} // date → class → strikes
	classCount := map[string]int{}
	for _, l := range listed {
		if l.Date <= today || l.TradingClass == "" {
			continue
		}
		if _, err := time.Parse(time.DateOnly, l.Date); err != nil {
			continue
		}
		if strikes[l.Date] == nil {
			strikes[l.Date] = map[string][]float64{}
		}
		if strikes[l.Date][l.TradingClass] == nil {
			classCount[l.TradingClass]++
		}
		strikes[l.Date][l.TradingClass] = append(strikes[l.Date][l.TradingClass], l.Strikes...)
	}
	if len(strikes) == 0 {
		return nil
	}
	primary := symbol
	if classCount[primary] == 0 {
		primary = ""
		for _, class := range slices.Sorted(maps.Keys(classCount)) {
			if primary == "" || classCount[class] > classCount[primary] {
				primary = class
			}
		}
	}
	dates := slices.Sorted(maps.Keys(strikes))
	lists := func(date, class string) bool { _, ok := strikes[date][class]; return ok }

	var out []SelectedExpiry
	seenMonth := map[string]bool{}
	for _, date := range dates {
		if len(out) == 2 {
			break
		}
		t, _ := time.Parse(time.DateOnly, date)
		monthKey := t.Format("2006-01")
		if seenMonth[monthKey] {
			continue
		}
		m := standardMonthly(t.Year(), t.Month(), days)
		pick := ""
		for _, cand := range []string{m.Format(time.DateOnly), previousTradingDay(m, days).Format(time.DateOnly)} {
			if cand > today && lists(cand, primary) {
				pick = cand
				break
			}
		}
		if pick == "" {
			continue
		}
		seenMonth[monthKey] = true
		slot := SlotMonthly1
		if len(out) == 1 {
			slot = SlotMonthly2
		}
		out = append(out, SelectedExpiry{Date: pick, TradingClass: primary, Slot: slot, Strikes: normalizeStrikes(strikes[pick][primary])})
	}

	weekEnd := map[string]string{} // Monday → latest listed date that week
	for _, date := range dates {
		t, _ := time.Parse(time.DateOnly, date)
		monday := t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7)).Format(time.DateOnly)
		if date > weekEnd[monday] {
			weekEnd[monday] = date
		}
	}
	for _, monday := range slices.Sorted(maps.Keys(weekEnd)) {
		date := weekEnd[monday]
		t, _ := time.Parse(time.DateOnly, date)
		weekly := t.Weekday() == time.Friday || (t.Weekday() == time.Thursday && !tradingDay(days, t.AddDate(0, 0, 1)))
		if !weekly || date == standardMonthly(t.Year(), t.Month(), days).Format(time.DateOnly) {
			continue
		}
		if slices.ContainsFunc(out, func(s SelectedExpiry) bool { return s.Date == date }) {
			continue
		}
		class := primary
		if !lists(date, class) {
			class = slices.Sorted(maps.Keys(strikes[date]))[0]
		}
		out = append(out, SelectedExpiry{Date: date, TradingClass: class, Slot: SlotWeekly, Strikes: normalizeStrikes(strikes[date][class])})
		break
	}
	slices.SortFunc(out, func(a, b SelectedExpiry) int {
		return cmp.Or(cmp.Compare(a.Date, b.Date), cmp.Compare(a.TradingClass, b.TradingClass))
	})
	return out
}

// SelectStrikes keeps the listed strikes within spot ± windowPct and, above
// maxCount, keeps half nearest the money and spreads the rest evenly across
// the remaining window, so the panel sees both the spread at the money and
// the wings of the smile. It returns the selection ascending and how many
// strikes the window held.
func SelectStrikes(strikes []float64, spot, windowPct float64, maxCount int) ([]float64, int) {
	if spot <= 0 || windowPct <= 0 || maxCount <= 0 {
		return nil, 0
	}
	lo, hi := spot*(1-windowPct), spot*(1+windowPct)
	var window []float64
	for _, k := range normalizeStrikes(strikes) {
		if k >= lo && k <= hi {
			window = append(window, k)
		}
	}
	if len(window) <= maxCount {
		return window, len(window)
	}
	byDistance := slices.Clone(window)
	sort.SliceStable(byDistance, func(i, j int) bool {
		return math.Abs(byDistance[i]-spot) < math.Abs(byDistance[j]-spot)
	})
	coreCount := (maxCount + 1) / 2
	core := byDistance[:coreCount]
	var rest []float64
	for _, k := range window {
		if !slices.Contains(core, k) {
			rest = append(rest, k)
		}
	}
	// Evenly spaced by listed-strike index, both window edges included.
	wings := maxCount - coreCount
	out := slices.Clone(core)
	for i := range wings {
		idx := len(rest) / 2
		if wings > 1 {
			idx = (i*(len(rest)-1) + (wings-1)/2) / (wings - 1)
		}
		out = append(out, rest[idx])
	}
	slices.Sort(out)
	return out, len(window)
}

// atmRanks maps each strike to its rank by distance from spot (0 = nearest).
func atmRanks(strikes []float64, spot float64) map[float64]int {
	byDistance := slices.Clone(strikes)
	sort.SliceStable(byDistance, func(i, j int) bool {
		return math.Abs(byDistance[i]-spot) < math.Abs(byDistance[j]-spot)
	})
	out := make(map[float64]int, len(byDistance))
	for i, k := range byDistance {
		out[k] = i
	}
	return out
}

func normalizeStrikes(in []float64) []float64 {
	out := make([]float64, 0, len(in))
	for _, k := range in {
		if k > 0 && !math.IsNaN(k) && !math.IsInf(k, 0) {
			out = append(out, math.Round(k*1e4)/1e4)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SymbolChoice is one underlying the run covers and where it came from.
type SymbolChoice struct {
	Symbol string
	Source string
}

// ResolveSymbols merges, in priority order, held option underlyings
// (alphabetically), the registered strategies' declared chains (in their
// order) and the research list (in order), de-duplicated with every source
// named, and splits off everything past MaxSymbols.
//
// Before 2026-10-18 it merged the explicit list (in order) with held option
// underlyings (alphabetically), de-duplicated, and splits off everything past
// MaxSymbols.
func ResolveSymbols(held []string, declared []SymbolChoice, explicit []string) (selected, skipped []SymbolChoice) {
	var all []SymbolChoice
	index := map[string]int{}
	add := func(raw, source string) {
		s, err := NormalizeSymbol(raw)
		if err != nil {
			return
		}
		if i, ok := index[s]; ok {
			if !slices.Contains(strings.Split(all[i].Source, "+"), source) {
				all[i].Source += "+" + source
			}
			return
		}
		index[s] = len(all)
		all = append(all, SymbolChoice{Symbol: s, Source: source})
	}
	heldSorted := slices.Clone(held)
	slices.Sort(heldSorted)
	for _, s := range heldSorted {
		add(s, SourceHeld)
	}
	for _, d := range declared {
		add(d.Symbol, d.Source)
	}
	for _, s := range explicit {
		add(s, SourceExplicit)
	}
	if len(all) <= MaxSymbols {
		return all, nil
	}
	return all[:MaxSymbols], all[MaxSymbols:]
}

// orderContracts sorts the run's work so the nearest-the-money strikes of
// every symbol and expiry come first; a run cut short loses wings evenly.
func orderContracts(cs []Contract) {
	slices.SortStableFunc(cs, func(a, b Contract) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.sym, b.sym), cmp.Compare(a.Expiry, b.Expiry),
			cmp.Compare(a.TradingClass, b.TradingClass), cmp.Compare(a.Right, b.Right))
	})
}
