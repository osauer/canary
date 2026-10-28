package mandate

import (
	"encoding/json"
	"math"
	"slices"
	"testing"
)

// The loss measure headlines the verdict (spec TM-04.12; owner decision
// 2026-10-27 19:50–19:52 CET): premium at risk on every bought option plus
// the cash loss to the stop on every share line with an armed stop, against
// the remaining loss room (max_loss_pct × budget base × room). The
// volatility is measured and reported beside it, no longer judged.

// lossTestInput is the evaluator fixture with a high-volatility stock
// series beside the shared ones, so a book can be far over the volatility
// budget while every gap stays under its cap.
func lossTestInput(t *testing.T, legs []Leg) Input {
	t.Helper()
	in := testInput(t, legs)
	in.Series["SYNC"] = syntheticSeries("SYNC", 280, 0.8, 21, driverReturns(280, 7), 0.4)
	return in
}

func TestLossMeasureHeadlinesTheVerdict(t *testing.T) {
	iv := 0.35
	room := 0.30 * 230000.0 // max_loss_pct × budget base at full room

	// A bought-option book whose premium at risk is over the loss room reads
	// reduce, however small its volatility.
	calls := []Leg{{ID: "1", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 20, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv, LossPerUnitBase: 4000}}
	v := Evaluate(lossTestInput(t, calls))
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	if v.Risk == nil || v.Risk.SDAnnualBase > v.Capital.BudgetAnnualSD {
		t.Fatalf("fixture: the calls' volatility %+v should sit inside the budget %g", v.Risk, v.Capital.BudgetAnnualSD)
	}
	if v.Loss == nil || math.Abs(v.Loss.TotalBase-80000) > 1e-6 || math.Abs(v.Loss.PremiumBase-80000) > 1e-6 || v.Loss.StopLossBase != 0 || math.Abs(v.Loss.RoomBase-room) > 1e-6 {
		t.Fatalf("loss %+v, want 80,000 of premium against the room %g", v.Loss, room)
	}
	if v.Status != StatusReduce || math.Abs(v.Loss.RatioToRoom-80000/room) > 1e-9 {
		t.Fatalf("status %s ratio %g, want reduce over the loss room", v.Status, v.Loss.RatioToRoom)
	}
	if len(v.Loss.Names) != 1 || v.Loss.Names[0].Underlying != "SYNA" || v.Loss.Names[0].PremiumBase != 80000 || len(v.Legs) != 1 || v.Legs[0].LossBase != 80000 {
		t.Fatalf("names %+v legs %+v", v.Loss.Names, v.Legs)
	}

	// A share book far over the volatility budget, with no premium and no
	// stop, reads within: its volatility is reported, not judged, and the
	// unstopped line is named.
	shares := []Leg{{ID: "2", Symbol: "SYNC", Underlying: "SYNC", Kind: KindStock, Quantity: 600, Multiplier: 1, Currency: "EUR", Spot: 100}}
	v = Evaluate(lossTestInput(t, shares))
	if v.Status == StatusUnknown {
		t.Fatalf("unknown: %v", v.Reasons)
	}
	if v.Risk.SDAnnualBase <= v.Capital.BudgetAnnualSD || len(v.Stresses.GapBreaches) != 0 {
		t.Fatalf("fixture: risk %g budget %g breaches %v; the shares should be over the budget with no gap breach", v.Risk.SDAnnualBase, v.Capital.BudgetAnnualSD, v.Stresses.GapBreaches)
	}
	if v.Status != StatusWithin || v.Loss.TotalBase != 0 || len(v.Loss.Unstopped) != 1 || v.Loss.Unstopped[0] != "SYNC" {
		t.Fatalf("status %s loss %+v, want within with SYNC named unstopped", v.Status, v.Loss)
	}

	// The same shares with an armed stop count the cash loss to it.
	stopped := shares
	stopped[0].StopArmed, stopped[0].LossPerUnitBase = true, 30
	v = Evaluate(lossTestInput(t, stopped))
	if v.Status != StatusWithin || math.Abs(v.Loss.StopLossBase-18000) > 1e-6 || math.Abs(v.Loss.TotalBase-18000) > 1e-6 || len(v.Loss.Unstopped) != 0 {
		t.Fatalf("stopped shares: %s %+v", v.Status, v.Loss)
	}

	// Each leg's report carries its own loss whatever order the simulation
	// ranks the legs in (the ranking is by contribution, not by input).
	mixed := []Leg{stopped[0], calls[0]}
	v = Evaluate(lossTestInput(t, mixed))
	if v.Status == StatusUnknown || len(v.Legs) != 2 {
		t.Fatalf("mixed book: %s %v", v.Status, v.Reasons)
	}
	for _, rep := range v.Legs {
		want := map[string]float64{"1": 80000, "2": 18000}[rep.ID]
		if math.Abs(rep.LossBase-want) > 1e-6 {
			t.Fatalf("leg %s reports %g of the loss, want %g: %+v", rep.ID, rep.LossBase, want, v.Legs)
		}
	}

	// A bought option whose premium cannot be measured makes the verdict
	// unknown, with the reason; nothing passes unmeasured.
	blind := []Leg{{ID: "3", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 1, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv, LossUnknown: "neither the price paid nor a current value is known"}}
	v = Evaluate(lossTestInput(t, blind))
	if v.Status != StatusUnknown || len(v.Reasons) != 1 || v.Reasons[0] != "SYNA 100 C: premium at risk unmeasured: neither the price paid nor a current value is known" {
		t.Fatalf("blind premium: %s %v", v.Status, v.Reasons)
	}

	// At the maximum loss the room is zero: any premium is over it, the
	// ratio is unbounded and encodes as null.
	noRoom := lossTestInput(t, calls)
	noRoom.Capital = knownCapital(t, 200000, 70000, 300000, 0)
	v = Evaluate(noRoom)
	if v.Status != StatusReduce || v.Loss.RoomBase != 0 || !math.IsInf(v.Loss.RatioToRoom, 1) {
		t.Fatalf("room 0: %s %+v", v.Status, v.Loss)
	}
	raw, err := json.Marshal(v.Loss)
	if err != nil {
		t.Fatal(err)
	}
	var back LossMeasure
	if err := json.Unmarshal(raw, &back); err != nil || !math.IsInf(back.RatioToRoom, 1) {
		t.Fatalf("round trip %s: %v %+v", raw, err, back)
	}
}

// WithLeg carries the addition's loss figure, so the incremental check judges
// an order by the premium it adds.
func TestWithLegAddsTheOrdersLoss(t *testing.T) {
	iv := 0.35
	held := []Leg{{ID: "1", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 10, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv, LossPerUnitBase: 1000}}
	in := lossTestInput(t, held)
	add := held[0]
	add.Quantity, add.LossPerUnitBase = 5, 1000
	v := Evaluate(WithLeg(in, add))
	if v.Loss == nil || math.Abs(v.Loss.TotalBase-15000) > 1e-6 {
		t.Fatalf("with the order: %+v, want 15,000 of premium", v.Loss)
	}
}

// A closing leg leaves the line's loss per unit as it was: selling 4 of 10
// calls leaves 6 at their own premium, and selling 40 of 100 stopped shares
// leaves 60 at their own distance to the stop (found 2026-10-28: the merge
// weighted the sale's zero premium in, so the plan's after figure read low).
func TestClosingLegKeepsTheLinesLossPerUnit(t *testing.T) {
	iv := 0.35
	call := Leg{ID: "1", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 10, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv, LossPerUnitBase: 1000}
	shares := Leg{ID: "2", Symbol: "SYNB", Underlying: "SYNB", Kind: KindStock, Quantity: 100, Multiplier: 1, Currency: "USD", Spot: 50, LossPerUnitBase: 5, StopArmed: true}
	in := lossTestInput(t, []Leg{call, shares})
	sellCalls, sellShares := call, shares
	sellCalls.Quantity, sellCalls.LossPerUnitBase = -4, 0
	sellShares.Quantity, sellShares.LossPerUnitBase, sellShares.StopArmed = -40, 0, false
	m, reasons := MeasureLoss(WithLeg(WithLeg(in, sellCalls), sellShares))
	if len(reasons) > 0 || m == nil || math.Abs(m.PremiumBase-6000) > 1e-6 || math.Abs(m.StopLossBase-300) > 1e-6 || len(m.Unstopped) != 0 {
		t.Fatalf("after the sales: %+v %v, want 6,000 of premium and 300 to the stop, still stopped", m, reasons)
	}
	// Past flat, the new side's units carry the new leg's own figure.
	flip := call
	flip.Quantity, flip.LossPerUnitBase = -15, 0
	if m, _ := MeasureLoss(WithLeg(in, flip)); m == nil || m.PremiumBase != 0 {
		t.Fatalf("short after the flip: %+v, want no premium at risk", m)
	}
	// Adding to the line still weighs the two premiums.
	add := call
	add.Quantity, add.LossPerUnitBase = 10, 3000
	if m, _ := MeasureLoss(WithLeg(in, add)); m == nil || math.Abs(m.PremiumBase-40000) > 1e-6 {
		t.Fatalf("added: %+v, want 40,000 of premium", m)
	}
}

// MeasureLoss is the verdict's headline without the simulation, and unknown
// for every reason the verdict would give for it: an unmeasured premium, the
// daemon's own grounds, or capital without a loss room.
func TestMeasureLossMatchesTheVerdict(t *testing.T) {
	iv := 0.35
	legs := []Leg{{ID: "1", Symbol: "SYNA 100 C", Underlying: "SYNA", Kind: KindOption, Quantity: 10, Multiplier: 100, Currency: "USD", Spot: 100,
		Right: "C", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv, LossPerUnitBase: 1000}}
	in := lossTestInput(t, legs)
	m, reasons := MeasureLoss(in)
	v := Evaluate(in)
	if len(reasons) > 0 || m == nil || v.Loss == nil || m.TotalBase != v.Loss.TotalBase || m.RoomBase != v.Loss.RoomBase {
		t.Fatalf("measure %+v %v, verdict %+v", m, reasons, v.Loss)
	}
	stale := in
	stale.Reasons = []string{"positions read is stale"}
	if _, reasons := MeasureLoss(stale); len(reasons) != 1 || reasons[0] != "positions read is stale" {
		t.Fatalf("daemon grounds: %v", reasons)
	}
	noCapital := in
	noCapital.Capital = Capital{Reasons: []string{"no equity reading"}}
	if _, reasons := MeasureLoss(noCapital); len(reasons) == 0 || reasons[0] != "no equity reading" {
		t.Fatalf("unknown capital: %v", reasons)
	}
	unmeasured := in
	unmeasured.Legs = slices.Clone(legs)
	unmeasured.Legs[0].LossUnknown = "no price paid"
	if _, reasons := MeasureLoss(unmeasured); len(reasons) == 0 {
		t.Fatal("an unmeasured premium must make the measure unknown")
	}
}
