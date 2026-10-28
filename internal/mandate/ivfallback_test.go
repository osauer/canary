package mandate

import (
	"strings"
	"testing"
)

// proposedFallback is the table proposed on 2026-10-28: bands 65, 15 and 8,
// tiers 5 and 20 percent of the loss room.
func proposedFallback() *IVFallback {
	return &IVFallback{ShortBandPct: 65, MonthBandPct: 15, LongBandPct: 8, MeasuredPct: 5, RangePct: 20}
}

func fallbackBook(t *testing.T, years float64, source string) Input {
	t.Helper()
	iv := 0.3
	stock := Leg{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 100}
	option := Leg{ID: "2", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 1, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: years, IV: &iv, IVSource: source}
	return testInput(t, []Leg{stock, option})
}

// One option priced on its mark's volatility no longer blinds the book once
// the owner has written [mandate.iv_fallback] (owner, 2026-10-28 08:51
// CET); without the table it reads unknown, as before.
func TestFallbackVolatilityCountsOnlyUnderTheOwnersTable(t *testing.T) {
	live := Evaluate(fallbackBook(t, 0.3, ""))
	if live.Status == StatusUnknown || live.Stresses == nil || live.IVFallback != nil {
		t.Fatalf("precondition: the live book is measured: %s %v", live.Status, live.Reasons)
	}

	v := Evaluate(fallbackBook(t, 0.3, IVSourceMark))
	if v.Status != StatusUnknown || v.Stresses != nil || !strings.Contains(strings.Join(v.Reasons, " "), "implied volatility") {
		t.Fatalf("without the table: %s %v, want unknown on the missing volatility", v.Status, v.Reasons)
	}

	in := fallbackBook(t, 0.3, IVSourceMark)
	in.Policy.IVFallback = proposedFallback()
	v = Evaluate(in)
	r := v.IVFallback
	if v.Status != live.Status || v.Stresses == nil || r == nil || r.Tier != IVTierMeasured {
		t.Fatalf("with the table: %s %+v, want the live verdict, measured", v.Status, r)
	}
	if v.Stresses.IndexLossBase != live.Stresses.IndexLossBase {
		t.Fatalf("base figure %v, live %v: the fallback volatility is the leg's own", v.Stresses.IndexLossBase, live.Stresses.IndexLossBase)
	}
	if !(r.IndexLossLowBase < r.IndexLossHighBase) || r.IndexLossLowBase > v.Stresses.IndexLossBase+1e-6 || r.IndexLossHighBase < v.Stresses.IndexLossBase-1e-6 {
		t.Fatalf("range %v..%v must hold the base %v", r.IndexLossLowBase, r.IndexLossHighBase, v.Stresses.IndexLossBase)
	}
	if len(r.Legs) != 1 || r.Legs[0].BandPct != 8 || r.RatioToRoom == nil || r.UncertaintyBase <= 0 {
		t.Fatalf("report = %+v, want one leg in the long band and a measured width", r)
	}
}

// The band follows the time to expiry, and a range wider than the owner's
// tier leaves the verdict unknown with the reason, never a figure.
func TestFallbackVolatilityBandAndTiers(t *testing.T) {
	for _, c := range []struct {
		years float64
		band  float64
	}{{5.0 / 365, 65}, {20.0 / 365, 15}, {90.0 / 365, 8}} {
		in := fallbackBook(t, c.years, IVSourceMark)
		in.Policy.IVFallback = proposedFallback()
		r := Evaluate(in).IVFallback
		if r == nil || r.Legs[0].BandPct != c.band {
			t.Fatalf("%v years: report %+v, want band %v", c.years, r, c.band)
		}
	}

	in := fallbackBook(t, 5.0/365, IVSourceMark)
	in.Legs[1].Quantity = 50
	tight := proposedFallback()
	tight.MeasuredPct, tight.RangePct = 0.001, 0.001
	in.Policy.IVFallback = tight
	v := Evaluate(in)
	if v.Status != StatusUnknown || v.Stresses != nil || v.Risk != nil || v.IVFallback == nil || v.IVFallback.Tier != IVTierUnknown {
		t.Fatalf("too wide: %s %+v, want unknown with the range kept", v.Status, v.IVFallback)
	}
	if !strings.Contains(strings.Join(v.Reasons, " "), "uncertain by more than") {
		t.Fatalf("reasons %v, want the width named", v.Reasons)
	}

	wide := proposedFallback()
	wide.MeasuredPct, wide.RangePct = 0.001, 100
	in.Policy.IVFallback = wide
	v = Evaluate(in)
	if v.IVFallback == nil || (v.IVFallback.Tier != IVTierRange && v.IVFallback.Tier != IVTierUnknown) {
		t.Fatalf("between the tiers: %+v, want range or, where the verdict differs at an edge, unknown", v.IVFallback)
	}
	if v.IVFallback.Tier == IVTierRange && (v.Status == StatusUnknown || v.Stresses == nil) {
		t.Fatalf("range tier must keep the verdict: %s", v.Status)
	}
}
