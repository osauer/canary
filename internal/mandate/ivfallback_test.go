package mandate

import (
	"math"
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

// At the maximum loss the room is zero, so no width fits a share of it, and
// one option without its own volatility blinded the verdict, the reduction and
// the hedge at once. The book is over its room at both edges, so the band
// cannot change the answer and the verdict stands (owner, 2026-11-07 07:33 CET).
func TestFallbackVolatilityAtZeroRoomKeepsTheVerdictBothEdgesGive(t *testing.T) {
	in := fallbackBook(t, 5.0/365, IVSourceMark)
	in.Policy.IVFallback = proposedFallback()
	in.Capital = knownCapital(t, 200000, 70000, 300000, 0)
	v := Evaluate(in)
	r := v.IVFallback
	if v.Status != StatusReduce || v.Stresses == nil || r == nil || r.Tier != IVTierRange {
		t.Fatalf("zero room: %s %v %+v, want reduce on the range tier", v.Status, v.Reasons, r)
	}
	if r.UncertaintyBase <= 0 || r.RatioToRoom != nil {
		t.Fatalf("report %+v, want a real width and no share of a zero room", r)
	}
	if got := v.HedgeFallBase(); got != r.IndexLossLowBase || !(got < v.Stresses.IndexLossBase) {
		t.Fatalf("hedge fall %v, want the low edge %v under the base %v", got, r.IndexLossLowBase, v.Stresses.IndexLossBase)
	}

	// Inside the room the owner's table still decides the width.
	roomy := fallbackBook(t, 5.0/365, IVSourceMark)
	roomy.Legs[1].Quantity = 50
	tight := proposedFallback()
	tight.MeasuredPct, tight.RangePct = 0.001, 0.001
	roomy.Policy.IVFallback = tight
	if got := Evaluate(roomy); got.Status != StatusUnknown {
		t.Fatalf("too wide inside the room: %s, want unknown", got.Status)
	}
}

// The width decides inside the room; once the loss is over it or none is left,
// only the verdict at both edges does, and one that differs stays unknown.
func TestFallbackTierAtAndAboveZeroRoom(t *testing.T) {
	f := proposedFallback()
	inf := math.Inf(1)
	for _, c := range []struct {
		name         string
		widthDecides bool
		share        float64
		same         bool
		tier         string
		tooWide      bool
	}{
		{"within measured", true, 0.05, false, IVTierMeasured, false},
		{"within range, same", true, 0.2, true, IVTierRange, false},
		{"within range, differs", true, 0.2, false, IVTierUnknown, false},
		{"too wide, same", true, 0.21, true, IVTierUnknown, true},
		{"over the room, no width", false, 0, false, IVTierMeasured, false},
		{"over the room, wide, same", false, 5, true, IVTierRange, false},
		{"over the room, wide, differs", false, 5, false, IVTierUnknown, false},
		{"no room, same", false, inf, true, IVTierRange, false},
		{"no room, differs", false, inf, false, IVTierUnknown, false},
	} {
		if tier, tooWide := f.tier(c.widthDecides, c.share, c.same); tier != c.tier || tooWide != c.tooWide {
			t.Errorf("%s: %s tooWide=%v, want %s tooWide=%v", c.name, tier, tooWide, c.tier, c.tooWide)
		}
	}
}

// Just above zero the stall was the same: a tiny room read the width as many
// times the room and went unknown, so the hold would flicker as the book moved
// (owner, 2026-11-07 08:03 CET). A loss inside the room is still judged on width.
func TestFallbackVolatilityOverASmallRoomKeepsTheVerdict(t *testing.T) {
	in := fallbackBook(t, 5.0/365, IVSourceMark)
	in.Policy.IVFallback = proposedFallback()
	in.Legs[1].LossPerUnitBase = 1000
	v := Evaluate(in)
	if v.IVFallback == nil || v.Loss == nil || v.Capital.BudgetBase <= 0 {
		t.Fatalf("fixture unmeasured: %v", v.Reasons)
	}
	width, loss := v.IVFallback.UncertaintyBase, v.Loss.TotalBase
	if loss <= width {
		t.Fatalf("fixture must lose more (%v) than its half-width (%v)", loss, width)
	}
	roomFor := func(base float64) float64 { return base / (in.Policy.MaxLossPct / 100 * in.Capital.BudgetBase) }

	// Room under the loss, and the width many times over range_pct of it.
	in.Capital.Room = roomFor(width)
	v = Evaluate(in)
	r := v.IVFallback
	if v.Status != StatusReduce || v.Stresses == nil || r == nil || r.Tier != IVTierRange {
		t.Fatalf("over a small room: %s %v %+v, want reduce on the range tier", v.Status, v.Reasons, r)
	}
	if r.RatioToRoom == nil || *r.RatioToRoom*100 <= in.Policy.IVFallback.RangePct {
		t.Fatalf("report %+v, want the share of the room reported and past range_pct", r)
	}

	// The same width with the loss inside the room: the owner's table decides.
	in.Capital.Room = roomFor(loss * 1.5)
	tight := proposedFallback()
	tight.MeasuredPct, tight.RangePct = 0.001, 0.001
	in.Policy.IVFallback = tight
	if got := Evaluate(in); got.Status != StatusUnknown || got.IVFallback == nil || got.IVFallback.Tier != IVTierUnknown {
		t.Fatalf("too wide inside the room: %s %+v, want unknown", got.Status, got.IVFallback)
	}
}

// The hedge offsets the base figure with every volatility its own, and the edge
// that loses less while one is on its mark, inside the room as at zero, but
// never more than the base figure.
func TestHedgeFallIsTheSmallerFigure(t *testing.T) {
	v := Evaluate(fallbackBook(t, 0.3, ""))
	if v.Stresses == nil || v.HedgeFallBase() != v.Stresses.IndexLossBase {
		t.Fatalf("hedge fall on a live book: %+v", v.Stresses)
	}
	in := fallbackBook(t, 0.3, IVSourceMark)
	in.Policy.IVFallback = proposedFallback()
	v = Evaluate(in)
	if v.Stresses == nil || v.IVFallback == nil || v.Capital.Room <= 0 {
		t.Fatalf("fixture must be measured with room left: %s %v", v.Status, v.Reasons)
	}
	if got := v.HedgeFallBase(); got != v.IVFallback.IndexLossLowBase || !(got < v.Stresses.IndexLossBase) {
		t.Fatalf("hedge fall inside the room %v, want the low edge %v under the base %v", got, v.IVFallback.IndexLossLowBase, v.Stresses.IndexLossBase)
	}
	above := Verdict{Stresses: &Stresses{IndexLossBase: 5}, IVFallback: &IVFallbackReport{IndexLossLowBase: 7}}
	if got := above.HedgeFallBase(); got != 5 {
		t.Fatalf("hedge fall %v, want at most the base figure 5", got)
	}
}
