package chainrec

import (
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
)

func strikesRange(lo, hi, step float64) []float64 {
	var out []float64
	for k := lo; k <= hi+1e-9; k += step {
		out = append(out, k)
	}
	return out
}

func listing(class string, strikes []float64, dates ...string) []ListedExpiry {
	out := make([]ListedExpiry, 0, len(dates))
	for _, d := range dates {
		out = append(out, ListedExpiry{Date: d, TradingClass: class, Strikes: strikes})
	}
	return out
}

func picked(sel []SelectedExpiry) [][3]string {
	out := make([][3]string, 0, len(sel))
	for _, s := range sel {
		out = append(out, [3]string{s.Date, s.TradingClass, s.Slot})
	}
	return out
}

func days() TradingDays { return NewUSEquityDays(marketcal.New()) }

// Daily ETF listings: the two nearest third Fridays and the next Friday that
// is not one of them; today's expiry is never recorded.
func TestSelectExpiriesDailyListedETF(t *testing.T) {
	strikes := strikesRange(500, 700, 1)
	listed := listing("SPY", strikes, "2026-10-14", "2026-10-15", "2026-10-16", "2026-10-19", "2026-10-21", "2026-10-23",
		"2026-10-30", "2026-11-20", "2026-12-18", "2027-01-15")
	got := picked(SelectExpiries("SPY", listed, "2026-10-14", days()))
	want := [][3]string{{"2026-10-16", "SPY", SlotMonthly1}, {"2026-10-23", "SPY", SlotWeekly}, {"2026-11-20", "SPY", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("SPY picks = %v, want %v", got, want)
	}
}

// SPX monthlies are the AM-settled SPX class, keyed by IBKR on the Thursday
// before the third Friday; the weekly comes from SPXW.
func TestSelectExpiriesIndexAMMonthlyAndPMWeekly(t *testing.T) {
	strikes := strikesRange(5000, 7000, 5)
	listed := append(listing("SPX", strikes, "2026-10-15", "2026-11-19", "2026-12-17"),
		listing("SPXW", strikes, "2026-10-14", "2026-10-15", "2026-10-16", "2026-10-19", "2026-10-20", "2026-10-23", "2026-11-20")...)
	got := picked(SelectExpiries("SPX", listed, "2026-10-14", days()))
	want := [][3]string{{"2026-10-15", "SPX", SlotMonthly1}, {"2026-10-23", "SPXW", SlotWeekly}, {"2026-11-19", "SPX", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("SPX picks = %v, want %v", got, want)
	}
	// On the AM monthly's last trading day that monthly is today's expiry.
	got = picked(SelectExpiries("SPX", listed, "2026-10-15", days()))
	want = [][3]string{{"2026-10-23", "SPXW", SlotWeekly}, {"2026-11-19", "SPX", SlotMonthly1}, {"2026-12-17", "SPX", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("SPX picks on the AM last-trade day = %v, want %v", got, want)
	}
}

// Juneteenth 2026 falls on the third Friday: the standard monthly moves to
// Thursday and the AM index monthly to Wednesday.
func TestSelectExpiriesHolidayShiftedMonthly(t *testing.T) {
	strikes := strikesRange(500, 700, 1)
	etf := listing("SPY", strikes, "2026-06-12", "2026-06-18", "2026-06-26", "2026-07-17")
	got := picked(SelectExpiries("SPY", etf, "2026-06-10", days()))
	want := [][3]string{{"2026-06-12", "SPY", SlotWeekly}, {"2026-06-18", "SPY", SlotMonthly1}, {"2026-07-17", "SPY", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("holiday month SPY picks = %v, want %v", got, want)
	}
	index := append(listing("SPX", strikes, "2026-06-17", "2026-07-16"), listing("SPXW", strikes, "2026-06-12", "2026-06-18")...)
	got = picked(SelectExpiries("SPX", index, "2026-06-10", days()))
	want = [][3]string{{"2026-06-12", "SPXW", SlotWeekly}, {"2026-06-17", "SPX", SlotMonthly1}, {"2026-07-16", "SPX", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("holiday month SPX picks = %v, want %v", got, want)
	}
}

// A weekly whose Friday is Christmas expires on Thursday; Wednesday and
// month-end listings never stand in for a weekly.
func TestSelectExpiriesHolidayShiftedWeekly(t *testing.T) {
	strikes := strikesRange(500, 700, 1)
	listed := listing("SPY", strikes, "2026-12-22", "2026-12-23", "2026-12-24", "2026-12-31", "2027-01-15", "2027-02-19")
	got := picked(SelectExpiries("SPY", listed, "2026-12-21", days()))
	want := [][3]string{{"2026-12-24", "SPY", SlotWeekly}, {"2027-01-15", "SPY", SlotMonthly1}, {"2027-02-19", "SPY", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("Christmas week picks = %v, want %v", got, want)
	}
	monthEnd := listing("SYNA", strikes, "2026-10-28", "2026-11-20", "2026-12-18")
	got = picked(SelectExpiries("SYNA", monthEnd, "2026-10-14", days()))
	want = [][3]string{{"2026-11-20", "SYNA", SlotMonthly1}, {"2026-12-18", "SYNA", SlotMonthly2}}
	if !slices.Equal(got, want) {
		t.Fatalf("monthly-only picks = %v, want %v (no weekly)", got, want)
	}
}

// A symbol listing adjusted classes beside its own uses its own class.
func TestSelectExpiriesPrefersOwnClass(t *testing.T) {
	strikes := strikesRange(50, 150, 1)
	listed := append(listing("SYNA", strikes, "2026-10-16", "2026-10-23", "2026-11-20"), listing("2SYNA", strikes, "2026-10-16", "2026-11-20", "2026-12-18")...)
	for _, s := range SelectExpiries("SYNA", listed, "2026-10-14", days()) {
		if s.TradingClass != "SYNA" {
			t.Fatalf("picked %v from the adjusted class", s)
		}
	}
}

func TestSelectStrikesWindowAndCap(t *testing.T) {
	got, inWindow := SelectStrikes(strikesRange(50, 150, 1), 100, StrikeWindowPct, MaxStrikesPerExpiry)
	if inWindow != 41 || len(got) != MaxStrikesPerExpiry {
		t.Fatalf("window = %d selected = %d, want 41 and %d", inWindow, len(got), MaxStrikesPerExpiry)
	}
	if !slices.IsSorted(got) || len(slices.Compact(slices.Clone(got))) != len(got) {
		t.Fatalf("selection not sorted and unique: %v", got)
	}
	if got[0] != 80 || got[len(got)-1] != 120 || !slices.Contains(got, 100) {
		t.Fatalf("selection %v must reach both window edges and keep the money", got)
	}
	for k := 90.0; k <= 109; k++ {
		if !slices.Contains(got, k) {
			t.Fatalf("core strike %g missing from %v", k, got)
		}
	}

	// A 5-point index grid: 481 strikes in the window, half kept at the
	// money, half spread across the wings.
	got, inWindow = SelectStrikes(strikesRange(4000, 8000, 5), 6000, StrikeWindowPct, MaxStrikesPerExpiry)
	if inWindow != 481 || len(got) != MaxStrikesPerExpiry {
		t.Fatalf("index window = %d selected = %d", inWindow, len(got))
	}
	if got[0] != 4800 || got[len(got)-1] != 7200 || !slices.Contains(got, 6000) || !slices.Contains(got, 5955) || !slices.Contains(got, 6045) {
		t.Fatalf("index selection %v must span the window and keep the money", got)
	}

	got, inWindow = SelectStrikes([]float64{95, 100, 105, 200, 0, -5, 100}, 100, StrikeWindowPct, MaxStrikesPerExpiry)
	if inWindow != 3 || !slices.Equal(got, []float64{95, 100, 105}) {
		t.Fatalf("small listing = %v (%d), want 95 100 105", got, inWindow)
	}
	if got, _ := SelectStrikes(strikesRange(50, 150, 1), 0, StrikeWindowPct, MaxStrikesPerExpiry); got != nil {
		t.Fatalf("no spot must select nothing, got %v", got)
	}
}

// Held option underlyings come first (alphabetically), then the registered
// strategies' declared chains, then the research list; a symbol several
// sources name keeps every source in priority order; past the cap the
// research names go first (strategy-register.md, section 4).
func TestResolveSymbolsMergesAndCaps(t *testing.T) {
	declared := []SymbolChoice{{"SYNC", SourceStrategyPrefix + "s9"}, {"SPY", SourceStrategyPrefix + "s9"}}
	sel, skipped := ResolveSymbols([]string{"SYNB", "SPY", "SYNA"}, declared, []string{"spx", "SPY", "QQQ", "SYNC"})
	want := []SymbolChoice{{"SPY", SourceHeld + "+strategy:s9+" + SourceExplicit}, {"SYNA", SourceHeld}, {"SYNB", SourceHeld}, {"SYNC", "strategy:s9+" + SourceExplicit}, {"SPX", SourceExplicit}, {"QQQ", SourceExplicit}}
	if !slices.Equal(sel, want) || skipped != nil {
		t.Fatalf("resolved = %v skipped %v, want %v", sel, skipped, want)
	}
	held := []string{"SYNA", "SYNB", "SYNC", "SYND", "SYNE", "SYNF", "SYNG", "SYNH", "SYNI", "SYNJ", "SYNK"}
	sel, skipped = ResolveSymbols(held, []SymbolChoice{{"SYNL", "strategy:s9"}}, []string{"SPX", "SPY", "QQQ"})
	if len(sel) != MaxSymbols || len(skipped) != len(held)+1+3-MaxSymbols || skipped[0].Symbol != "SPX" || sel[MaxSymbols-1].Symbol != "SYNL" {
		t.Fatalf("cap: selected %v skipped %v", sel, skipped)
	}
	if sel, _ := ResolveSymbols(nil, nil, DefaultSymbols); len(sel) != 0 {
		t.Fatalf("the default research list records %v", sel)
	}
}

func TestNormalizeSymbols(t *testing.T) {
	got, err := NormalizeSymbols([]string{" spx", "SPY", "spy", "BRK.B"})
	if err != nil || !slices.Equal(got, []string{"SPX", "SPY", "BRK.B"}) {
		t.Fatalf("normalized = %v, %v", got, err)
	}
	for _, bad := range [][]string{{""}, {"SP X"}, {"TOOLONGSYMBOL1"}} {
		if _, err := NormalizeSymbols(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestOrderContractsPutsTheMoneyFirst(t *testing.T) {
	cs := []Contract{
		{Symbol: "SPY", Expiry: "2026-11-20", Strike: 90, Right: "C", rank: 2, sym: 1},
		{Symbol: "SPX", Expiry: "2026-11-19", Strike: 6000, Right: "P", rank: 0, sym: 0},
		{Symbol: "SPY", Expiry: "2026-11-20", Strike: 100, Right: "C", rank: 0, sym: 1},
		{Symbol: "SPX", Expiry: "2026-11-19", Strike: 6000, Right: "C", rank: 0, sym: 0},
	}
	orderContracts(cs)
	got := []string{}
	for _, c := range cs {
		got = append(got, c.Key())
	}
	want := []string{"SPX||2026-11-19|6000|C", "SPX||2026-11-19|6000|P", "SPY||2026-11-20|100|C", "SPY||2026-11-20|90|C"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestStandardMonthly(t *testing.T) {
	for _, tc := range []struct {
		year  int
		month time.Month
		want  string
	}{{2026, time.October, "2026-10-16"}, {2026, time.June, "2026-06-18"}, {2027, time.January, "2027-01-15"}} {
		if got := standardMonthly(tc.year, tc.month, days()).Format(time.DateOnly); got != tc.want {
			t.Errorf("%d-%02d monthly = %s, want %s", tc.year, tc.month, got, tc.want)
		}
	}
}
