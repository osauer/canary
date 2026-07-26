package daemon

import (
	"reflect"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestSelectSPXExpirationsClassedKeepsSPXandSPXWSeparate pins the
// per-class settlement-cutoff branch. On a third-Friday at 10:00 ET,
// the SPX-class AM-settled monthly is past its 09:30 settle and must
// be dropped, while the SPXW-class PM-settled weekly listed on the
// same date is still hours from settlement and must be kept.
func TestSelectSPXExpirationsClassedKeepsSPXandSPXWSeparate(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		// Third Friday — listed under both classes.
		"2026-06-19": {
			{TradingClass: "SPX", Strikes: []float64{5400, 5500}},
			{TradingClass: "SPXW", Strikes: []float64{5390, 5400, 5410}},
		},
		// SPXW-only Monday weekly.
		"2026-06-22": {
			{TradingClass: "SPXW", Strikes: []float64{5400}},
		},
	}

	// 10:00 ET on the third Friday — past 09:30 SPX-class settle, still
	// ~6 hours before 16:00 SPXW-class settle.
	now := time.Date(2026, 6, 19, 10, 0, 0, 0, loc)

	picked := selectSPXExpirationsClassed(classed, now, 6)

	// Expect 2 entries: the third-Friday SPXW-only entry (SPX-AM dropped),
	// plus the Monday weekly. Date ascending; SPXW class.
	if len(picked) != 2 {
		t.Fatalf("expected 2 picked entries, got %d: %+v", len(picked), picked)
	}
	want := []spxExpirySpec{
		{Date: "2026-06-19", TradingClass: "SPXW", Strikes: []float64{5390, 5400, 5410}},
		{Date: "2026-06-22", TradingClass: "SPXW", Strikes: []float64{5400}},
	}
	if !reflect.DeepEqual(picked, want) {
		t.Errorf("picked:\ngot  %+v\nwant %+v", picked, want)
	}
}

// TestSelectSPXExpirationsClassedKeepsBothPreSettle pins that an
// early-morning run BEFORE the AM-settle window keeps both classes
// on a third-Friday, treating each as a distinct expiration.
func TestSelectSPXExpirationsClassedKeepsBothPreSettle(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		"2026-06-19": {
			{TradingClass: "SPX", Strikes: []float64{5400}},
			{TradingClass: "SPXW", Strikes: []float64{5400}},
		},
	}

	// 08:30 ET — before both settlement cutoffs.
	now := time.Date(2026, 6, 19, 8, 30, 0, 0, loc)
	picked := selectSPXExpirationsClassed(classed, now, 6)
	if len(picked) != 2 {
		t.Fatalf("expected both classes pre-AM-settle, got %d entries: %+v", len(picked), picked)
	}
	// Sort tiebreak: SPX before SPXW.
	if picked[0].TradingClass != "SPX" || picked[1].TradingClass != "SPXW" {
		t.Errorf("class order: got [%s, %s], want [SPX, SPXW]",
			picked[0].TradingClass, picked[1].TradingClass)
	}
}

func TestSelectSPXExpirationsClassedDropsExpiringSPXWAtClose(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		"2026-06-03": {{TradingClass: "SPXW", Strikes: []float64{7550, 7600}}},
		"2026-06-04": {{TradingClass: "SPXW", Strikes: []float64{7550, 7600}}},
	}

	preClose := selectSPXExpirationsClassed(classed, time.Date(2026, 6, 3, 15, 59, 59, 0, loc), 6)
	if len(preClose) == 0 || preClose[0].Date != "2026-06-03" {
		t.Fatalf("pre-close SPXW 0DTE should be selectable, got %+v", preClose)
	}

	atClose := selectSPXExpirationsClassed(classed, time.Date(2026, 6, 3, 16, 0, 0, 0, loc), 6)
	if len(atClose) == 0 {
		t.Fatalf("expected next SPXW expiry after expiring close")
	}
	for _, spec := range atClose {
		if spec.Date == "2026-06-03" {
			t.Fatalf("expiring SPXW should be dropped at 16:00 ET, got %+v", atClose)
		}
	}

	postClose := selectSPXExpirationsClassed(classed, time.Date(2026, 6, 3, 16, 10, 0, 0, loc), 6)
	for _, spec := range postClose {
		if spec.Date == "2026-06-03" {
			t.Fatalf("expiring SPXW should be dropped after 16:00 ET, got %+v", postClose)
		}
	}
}

func TestSelectSPXChainEntryUsesOnlyListedClass(t *testing.T) {
	t.Parallel()
	entries := normalisedSPXChainEntries([]ibkrlib.ExpiryClassedStrikes{{
		TradingClass: "SPXW",
		Strikes:      []float64{7585, 7575, 7580},
	}}, "SPX")

	entry, auto, err := selectSPXChainEntry(entries, "SPX", false, "2026-06-01", time.Now())
	if err != nil {
		t.Fatalf("selectSPXChainEntry: %v", err)
	}
	if auto {
		t.Fatalf("single listed class should not be marked auto")
	}
	if entry.TradingClass != "SPXW" {
		t.Fatalf("TradingClass = %q, want SPXW", entry.TradingClass)
	}

	rows := chainRowsFromListedStrikes(entry.Strikes, 7581, 1)
	got := make([]float64, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Strike)
	}
	want := []float64{7575, 7580, 7585}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed strike grid = %v, want %v", got, want)
	}
	if !rows[1].IsATM {
		t.Fatalf("nearest listed strike should be marked ATM: %+v", rows)
	}
}

func TestSelectSPXChainEntryRejectsMissingExplicitClass(t *testing.T) {
	t.Parallel()
	entries := normalisedSPXChainEntries([]ibkrlib.ExpiryClassedStrikes{{
		TradingClass: "SPXW",
		Strikes:      []float64{7580},
	}}, "SPX")

	_, _, err := selectSPXChainEntry(entries, "SPX", true, "2026-06-01", time.Now())
	if err == nil {
		t.Fatalf("expected missing explicit class to fail")
	}
	if !strings.Contains(err.Error(), "available classes: SPXW") {
		t.Fatalf("error should list available class, got %v", err)
	}
}

func TestSelectDefaultChainEntryPrefersSymbolClass(t *testing.T) {
	t.Parallel()
	entries := normalisedSPXChainEntries([]ibkrlib.ExpiryClassedStrikes{
		{TradingClass: "2SPY", Strikes: []float64{639}},
		{TradingClass: "SPY", Strikes: []float64{755, 756, 757, 758, 759, 760, 761, 762}},
	}, "SPY")

	entry, auto, err := selectDefaultChainEntry("SPY", entries, "SPY", false, "2026-06-01")
	if err != nil {
		t.Fatalf("selectDefaultChainEntry: %v", err)
	}
	if auto {
		t.Fatalf("symbol-class match should not be marked auto")
	}
	if entry.TradingClass != "SPY" {
		t.Fatalf("TradingClass = %q, want SPY", entry.TradingClass)
	}

	rows := chainRowsFromListedStrikes(entry.Strikes, 757.96, 2)
	got := make([]float64, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Strike)
	}
	want := []float64{756, 757, 758, 759, 760}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed SPY strike grid = %v, want %v", got, want)
	}
}

func TestPickDefaultClassedExpirationsDoesNotMergeSPYSiblingClasses(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	now := time.Date(2026, 6, 2, 10, 0, 0, 0, loc)
	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		"2026-06-02": {
			{TradingClass: "2SPY", Strikes: []float64{650, 651, 652}},
			{TradingClass: "SPY", Strikes: []float64{756, 757, 758, 759, 760}},
		},
		"2026-06-03": {
			{TradingClass: "SPY", Strikes: []float64{755, 756, 757, 758, 759}},
		},
	}

	picked := pickDefaultClassedExpirations("SPY", classed, now, 2)
	if len(picked) != 2 {
		t.Fatalf("picked %d expirations, want 2: %+v", len(picked), picked)
	}
	first := picked[0]
	if first.date != "2026-06-02" || first.tradingClass != "SPY" {
		t.Fatalf("first pick = %+v, want 2026-06-02 class SPY", first)
	}
	wantStrikes := []float64{756, 757, 758, 759, 760}
	if !reflect.DeepEqual(first.strikes, wantStrikes) {
		t.Fatalf("SPY strikes were merged with sibling class: got %v want %v", first.strikes, wantStrikes)
	}
}

func TestSelectDefaultChainEntryRejectsMissingExplicitClass(t *testing.T) {
	t.Parallel()
	entries := normalisedSPXChainEntries([]ibkrlib.ExpiryClassedStrikes{{
		TradingClass: "SPY",
		Strikes:      []float64{758},
	}}, "SPY")

	_, _, err := selectDefaultChainEntry("SPY", entries, "2SPY", true, "2026-06-01")
	if err == nil {
		t.Fatalf("expected missing explicit class to fail")
	}
	if !strings.Contains(err.Error(), "available classes: SPY") {
		t.Fatalf("error should list available class, got %v", err)
	}
}

func TestSelectSPXChainEntryPrefersSPXWhenAmbiguousBeforeSettle(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	entries := normalisedSPXChainEntries([]ibkrlib.ExpiryClassedStrikes{
		{TradingClass: "SPXW", Strikes: []float64{7580}},
		{TradingClass: "SPX", Strikes: []float64{7580}},
	}, "SPX")

	entry, auto, err := selectSPXChainEntry(entries, "SPX", false, "2026-06-17", time.Date(2026, 6, 1, 4, 0, 0, 0, loc))
	if err != nil {
		t.Fatalf("selectSPXChainEntry: %v", err)
	}
	if !auto {
		t.Fatalf("ambiguous classes should be marked auto")
	}
	if entry.TradingClass != "SPX" {
		t.Fatalf("TradingClass = %q, want SPX", entry.TradingClass)
	}
}

func TestSelectSPXExpirationsClassedIncludesAMMonthlyAndQuarterlyThursdayKeys(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		"2026-06-02": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		"2026-06-03": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		"2026-06-04": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		"2026-06-05": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		"2026-06-08": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		"2026-06-09": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
		// IBKR/TWS keys AM-settled standard SPX monthlies by Thursday
		// last-trade date, not by the third Friday settlement date.
		"2026-06-18": {{TradingClass: "SPX", Strikes: []float64{7600}}},
		"2026-07-16": {{TradingClass: "SPX", Strikes: []float64{7600}}},
		"2026-09-17": {{TradingClass: "SPX", Strikes: []float64{7600}}},
		// Adjacent PM-settled weekly must not satisfy the SPX AM monthly
		// or quarterly anchors.
		"2026-09-18": {{TradingClass: "SPXW", Strikes: []float64{7600}}},
	}
	now := time.Date(2026, 6, 2, 9, 30, 0, 0, loc)

	picked := selectSPXExpirationsClassed(classed, now, 6)
	got := make([]string, 0, len(picked))
	for _, spec := range picked {
		got = append(got, spec.Date+"|"+spec.TradingClass)
	}
	want := []string{
		"2026-06-02|SPXW",
		"2026-06-03|SPXW",
		"2026-06-04|SPXW",
		"2026-06-05|SPXW",
		"2026-06-18|SPX",
		"2026-09-17|SPX",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("picked SPX expiries:\ngot  %v\nwant %v", got, want)
	}
}

func TestClassSettlementInstantSPXAMThursdayKeySettlesFridayMorning(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}
	got := classSettlementInstant("SPX", 2026, time.September, 17, loc)
	want := time.Date(2026, time.September, 18, 9, 30, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("SPX AM settlement instant = %s, want %s", got, want)
	}
}

// TestSelectSPXExpirationsClassedRespectsCount pins the 6-cap budget
// across classes. Anchored monthly/quarterly slots may jump forward, but
// the final basket must still respect the requested count and stable order.
func TestSelectSPXExpirationsClassedRespectsCount(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("America/New_York: %v", err)
	}

	classed := map[string][]ibkrlib.ExpiryClassedStrikes{
		"2026-06-22": {{TradingClass: "SPXW", Strikes: []float64{5400}}},
		"2026-06-23": {{TradingClass: "SPXW", Strikes: []float64{5400}}},
		"2026-06-24": {{TradingClass: "SPXW", Strikes: []float64{5400}}},
		"2026-06-25": {{TradingClass: "SPXW", Strikes: []float64{5400}}},
		"2026-06-26": {{TradingClass: "SPXW", Strikes: []float64{5400}}},
		"2026-07-17": {
			{TradingClass: "SPX", Strikes: []float64{5400}},
			{TradingClass: "SPXW", Strikes: []float64{5400}},
		},
		"2026-08-21": {{TradingClass: "SPX", Strikes: []float64{5400}}},
	}
	now := time.Date(2026, 6, 22, 8, 30, 0, 0, loc)
	picked := selectSPXExpirationsClassed(classed, now, 6)
	if len(picked) != 6 {
		t.Fatalf("count cap not honored: got %d, want 6: %+v", len(picked), picked)
	}
	// Sanity-check sort order: dates ascending.
	for i := 1; i < len(picked); i++ {
		if picked[i].Date < picked[i-1].Date {
			t.Errorf("sort order broken at index %d: %+v", i, picked)
		}
	}
}

// TestPickedDatesFromPickedDedupes pins that the result envelope's
// Expirations field carries unique dates only — SPX third-Friday with
// both classes shows once, not twice.
func TestPickedDatesFromPickedDedupes(t *testing.T) {
	picked := []pickedExpiration{
		{date: "2026-06-19", tradingClass: "SPX"},
		{date: "2026-06-19", tradingClass: "SPXW"}, // same date, different class
		{date: "2026-06-22", tradingClass: "SPXW"},
	}
	got := pickedDatesFromPicked(picked)
	want := []string{"2026-06-19", "2026-06-22"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupe broken: got %v, want %v", got, want)
	}
}
