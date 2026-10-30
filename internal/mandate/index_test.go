package mandate

import (
	"maps"
	"math"
	"slices"
	"testing"
)

// A broad index takes the owner's index gap (D4, 2026-10-14: 12%), a single
// name keeps the one-name gap, and without index_gap_shock_pct every
// underlying takes gap_shock_pct as before.
func TestBroadIndexTakesTheIndexGap(t *testing.T) {
	spy := Leg{ID: "con:1", Symbol: IndexSymbol, Underlying: IndexSymbol, Kind: KindStock, Quantity: 1500, Multiplier: 1, Currency: "USD", Spot: 100}
	syna := Leg{ID: "con:2", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 1500, Multiplier: 1, Currency: "USD", Spot: 100}
	in := testInput(t, []Leg{spy, syna})
	capBase := GapCapBase(in.Policy, in.Capital)
	value := 1500 * 100 * 0.9

	// Absent: both take 25% and both breach the cap.
	v := Evaluate(in)
	if v.Status != StatusReduce || !slices.Equal(v.Stresses.GapBreaches, []string{IndexSymbol, "SYNA"}) && !slices.Equal(v.Stresses.GapBreaches, []string{"SYNA", IndexSymbol}) {
		t.Fatalf("without the index gap: %s breaches %v (cap %g, gap loss %g)", v.Status, v.Stresses.GapBreaches, capBase, value*0.25)
	}
	if v.Stresses.IndexGapShockPct != 25 {
		t.Fatalf("index gap without the key %g, want the one-name 25", v.Stresses.IndexGapShockPct)
	}

	// Written: the index takes 12% and fits; the name keeps 25%.
	in.Policy.IndexGapShockPct = 12
	v = Evaluate(in)
	if !slices.Equal(v.Stresses.GapBreaches, []string{"SYNA"}) || v.Stresses.IndexGapShockPct != 12 {
		t.Fatalf("with the index gap: breaches %v stresses %+v", v.Stresses.GapBreaches, v.Stresses)
	}
	for _, u := range v.Underlyings {
		switch u.Underlying {
		case IndexSymbol:
			if !u.BroadIndex || u.GapShockPct != 12 || math.Abs(u.GapDownLossBase-value*0.12) > 1e-6 || u.GapBreach {
				t.Fatalf("index underlying %+v", u)
			}
		case "SYNA":
			if u.BroadIndex || u.GapShockPct != 25 || math.Abs(u.GapDownLossBase-value*0.25) > 1e-6 || !u.GapBreach {
				t.Fatalf("single name %+v", u)
			}
		}
	}
}

// The list matches symbol and currency together, case-insensitively; a
// symbol on the list in another currency, or a name off it, is not a broad
// index.
func TestIsBroadIndexMatchesSymbolAndCurrency(t *testing.T) {
	for _, c := range []struct {
		symbol, currency string
		want             bool
	}{
		{"IBCF", "EUR", true}, {" ibcf ", "eur", true}, {"IBCF", "USD", false},
		{"SPY", "USD", true}, {"SPY", "EUR", false}, {"SPX", "USD", true}, {"XSP", "USD", true}, {"NDX", "USD", true},
		{"QQQ", "USD", true}, {"IWM", "USD", true}, {"RUT", "USD", true}, {"DIA", "USD", true},
		{"SXR8", "EUR", true}, {"CSPX", "USD", true}, {"EUNL", "EUR", true}, {"VWCE", "EUR", true},
		{"AAPL", "USD", false}, {"SYNA", "USD", false}, {"", "", false},
	} {
		if got := IsBroadIndex(c.symbol, c.currency); got != c.want {
			t.Errorf("IsBroadIndex(%q, %q) = %v, want %v", c.symbol, c.currency, got, c.want)
		}
	}
	for _, b := range BroadIndexInstruments {
		if b.ISIN != "" && (len(b.ISIN) != 12 || b.ISIN[:2] != "IE") {
			t.Errorf("%s: ISIN %q is not an Irish UCITS ISIN", b.Symbol, b.ISIN)
		}
		if len(b.Currencies) == 0 || b.Name == "" {
			t.Errorf("%s: incomplete entry %+v", b.Symbol, b)
		}
	}
}

// One leg off the list keeps the whole underlying on the one-name gap.
func TestBroadIndexNeedsEveryLeg(t *testing.T) {
	legs := []Leg{{Underlying: "SPY", Currency: "USD"}, {Underlying: "SPY", Currency: "EUR"}}
	if broadIndexUnderlying(legs, "SPY") {
		t.Fatal("a mixed underlying read as a broad index")
	}
	if !broadIndexUnderlying(legs[:1], "SPY") || broadIndexUnderlying(legs, "QQQ") {
		t.Fatal("broad index by underlying")
	}
}

// IBCF, the EUR-hedged S&P 500 fund, moves with SPY's local-currency
// returns on the US close (review S5): measured without a series of its
// own, at beta 1, with no currency leg in a EUR book, and its own Xetra
// series, stale or missing, never makes the verdict unknown.
func TestIBCFMovesWithTheIndexReturns(t *testing.T) {
	ibcf := Leg{ID: "con:3", Symbol: "IBCF", Underlying: "IBCF", Kind: KindStock, Quantity: 1000, Multiplier: 1, Currency: "EUR", Spot: 50}
	in := testInput(t, []Leg{ibcf})
	if _, ok := in.Series["IBCF"]; ok {
		t.Fatal("fixture carries an IBCF series")
	}
	v := Evaluate(in)
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	if len(v.Underlyings) != 1 || math.Abs(v.Underlyings[0].Beta-1) > 1e-12 || v.Risk.FXContributionPct != 0 {
		t.Fatalf("IBCF %+v risk %+v", v.Underlyings, v.Risk)
	}
	// The same notional in SPY measured in EUR (no FX leg) has the same risk.
	spy := Leg{ID: "con:4", Symbol: IndexSymbol, Underlying: IndexSymbol, Kind: KindStock, Quantity: 1000, Multiplier: 1, Currency: "EUR", Spot: 50}
	if sv := Evaluate(testInput(t, []Leg{spy})); math.Abs(sv.Risk.SDAnnualBase-v.Risk.SDAnnualBase) > 1e-9 {
		t.Fatalf("IBCF risk %g, SPY at the same notional %g", v.Risk.SDAnnualBase, sv.Risk.SDAnnualBase)
	}
	stale := in
	stale.Series = maps.Clone(in.Series)
	stale.Series["IBCF"] = Series{Key: "IBCF", Dates: []string{"2026-01-02"}, Closes: []float64{50}, Stale: true}
	if sv := Evaluate(stale); sv.Status == StatusUnknown {
		t.Fatalf("a stale IBCF series made the verdict unknown: %v", sv.Reasons)
	}
	if ReturnsSeries("ibcf", "eur") != IndexSymbol || ReturnsSeries("IBCF", "USD") != "IBCF" || ReturnsSeries("SYNA", "USD") != "SYNA" {
		t.Fatal("ReturnsSeries")
	}
}

// The index gap moves every broad-index line together, each by the index
// gap times its beta, and the gap cap applies to the group's net loss
// (owner decision "1. A", 2026-10-30 09:41 CET): a short Micro S&P hedge
// nets against the SPY it hedges, cures SPY's breach at neutral, adds none
// of its own below neutral, and still breaches when it hedges past neutral.
func TestIndexGapNetsTheHedgeAgainstTheIndex(t *testing.T) {
	spy := func(shares float64) Leg {
		return Leg{ID: "con:1", Symbol: IndexSymbol, Underlying: IndexSymbol, Kind: KindStock, Quantity: shares, Multiplier: 1, Currency: "USD", Spot: 600}
	}
	hedge := func(n float64) Leg {
		return Leg{ID: "con:9", Symbol: "MESZ6", Underlying: MicroSP500Future, Kind: KindStock, Quantity: -n, Multiplier: 5, Currency: "USD", Spot: 6000}
	}
	syna := Leg{ID: "con:2", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 380, Multiplier: 1, Currency: "USD", Spot: 100}
	synb := Leg{ID: "con:3", Symbol: "SYNB", Underlying: "SYNB", Kind: KindStock, Quantity: 380, Multiplier: 1, Currency: "USD", Spot: 100}
	breaches := func(legs ...Leg) ([]string, Verdict) {
		t.Helper()
		in := testInput(t, legs)
		in.Policy.IndexGapShockPct = 12
		v := Evaluate(in)
		if v.Stresses == nil {
			t.Fatalf("unknown: %v", v.Reasons)
		}
		if c := v.Stresses.GapCapBase; math.Abs(c-22977) > 1 {
			t.Fatalf("gap cap %g, want ≈ 22,977", c)
		}
		return v.Stresses.GapBreaches, v
	}

	if got, _ := breaches(spy(400)); !slices.Equal(got, []string{IndexSymbol}) {
		t.Fatalf("400 SPY: breaches %v, want [SPY]", got)
	}
	got, v := breaches(spy(400), hedge(8))
	if len(got) != 0 {
		t.Fatalf("400 SPY hedged to neutral with 8 MES: breaches %v, want none", got)
	}
	for _, u := range v.Underlyings {
		if !u.BroadIndex || math.Abs(u.GapLossBase) > 1e-6 {
			t.Fatalf("hedged index line %+v, want the group's net gap loss ≈ 0", u)
		}
	}
	if g := v.Stresses.IndexGap; g == nil || !slices.Equal(g.Members, []string{IndexSymbol, MicroSP500Future}) || g.ShockPct != 12 || g.Breach {
		t.Fatalf("index gap group %+v, want SPY and MES FUT at 12%% within the cap", g)
	}

	if got, _ := breaches(spy(298), syna, synb); len(got) != 0 {
		t.Fatalf("298 SPY with SYNA, SYNB: breaches %v, want none", got)
	}
	for _, n := range []float64{6, 8} {
		if got, _ := breaches(spy(298), syna, synb, hedge(n)); len(got) != 0 {
			t.Fatalf("298 SPY short %g MES: breaches %v, want none below the over-hedge", n, got)
		}
	}
	// 30 contracts are 900,000 USD short against 178,800 USD of SPY: past
	// neutral, a 12% rise loses ≈ 77,900 EUR on the net short.
	got, v = breaches(spy(298), syna, synb, hedge(30))
	if !slices.Equal(got, []string{MicroSP500Future}) {
		t.Fatalf("298 SPY over-hedged with 30 MES: breaches %v, want [MES FUT]", got)
	}
	want := (30*6000*5 - 298*600) * 0.9 * 0.12
	if g := v.Stresses.IndexGap; g == nil || !g.Breach || g.UpLossBase == nil || math.Abs(*g.UpLossBase-want) > 1e-6 || math.Abs(g.LossBase-want) > 1e-6 {
		t.Fatalf("over-hedged group %+v, want the net short's rise loss %.2f breaching", g, want)
	}
}

// Every broad-index line moves by the full index gap whatever its beta
// (owner decision 2026-10-30 09:58 CET): one with beta above 1 no further,
// one with beta below 1 no less.
func TestIndexGapMovesEveryLineByTheFullGap(t *testing.T) {
	in := testInput(t, []Leg{
		{ID: "con:1", Symbol: IndexSymbol, Underlying: IndexSymbol, Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 600},
		{ID: "con:4", Symbol: "QQQ", Underlying: "QQQ", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 500},
		{ID: "con:5", Symbol: "IWM", Underlying: "IWM", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 200},
	})
	drv := driverReturns(280, 7)
	in.Series["QQQ"] = syntheticSeries("QQQ", 280, 0.22, 15, drv, 0.9)
	in.Series["IWM"] = syntheticSeries("IWM", 280, 0.10, 16, drv, 0.3)
	in.Policy.IndexGapShockPct = 12
	v := Evaluate(in)
	betas := map[string]float64{}
	for _, u := range v.Underlyings {
		betas[u.Underlying] = u.Beta
	}
	if betas["QQQ"] <= 1 || betas["IWM"] >= 1 {
		t.Fatalf("fixture betas %v, want QQQ above 1 and IWM below", betas)
	}
	want := (100*600 + 100*500 + 100*200) * 0.9 * 0.12
	if g := v.Stresses.IndexGap; g == nil || len(g.Members) != 3 || math.Abs(g.DownLossBase-want) > 1e-6 {
		t.Fatalf("group %+v, want down loss %.2f at the full gap", g, want)
	}
}
