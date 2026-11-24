package mandate

import (
	"math"
	"strings"
	"testing"
)

// tail keeps the last n closes of a series: a name listed n sessions ago.
func tail(s Series, n int) Series {
	s.Dates, s.Closes = s.Dates[len(s.Dates)-n:], s.Closes[len(s.Closes)-n:]
	return s
}

// A newly listed name held long does not blind the mandate (owner decision
// 2026-11-24): on 2026-11-23 a 50-share line of a stock IBKR had three daily
// bars for, about 2% of the book, made the whole verdict unknown, which
// refused every stock addition and held the forced reduction. Its whole value
// is charged as lost in every scenario and both index stresses, the one-name
// gap applies as to any name, it is named on the verdict, and nothing about
// it reads as measured. A short line on such a name stays unknown.
func TestShortHistoryLongLineIsChargedNotBlinding(t *testing.T) {
	iv := 0.5
	base := []Leg{
		{ID: "1", Symbol: "SYNA", Underlying: "SYNA", Kind: KindStock, Quantity: 300, Multiplier: 1, Currency: "USD", Spot: 100},
		{ID: "2", Symbol: "SYNB", Underlying: "SYNB", Kind: KindStock, Quantity: 200, Multiplier: 1, Currency: "USD", Spot: 50},
	}
	young := Leg{ID: "3", Symbol: "NEWX", Underlying: "NEWX", Kind: KindStock, Quantity: 50, Multiplier: 1, Currency: "USD", Spot: 120}
	youngCall := Leg{ID: "4", Symbol: "NEWX C130", Underlying: "NEWX", Kind: KindOption, Quantity: 2, Multiplier: 100, Currency: "USD", Spot: 120,
		Right: "C", Strike: 130, TimeToExpiryYears: 0.25, IV: &iv, LossPerUnitBase: 900}
	input := func(t *testing.T, legs ...Leg) Input {
		in := testInput(t, legs)
		in.Series["NEWX"] = tail(syntheticSeries("NEWX", 280, 0.6, 21, nil, 0), 3)
		return in
	}
	without := Evaluate(testInput(t, base))
	if without.Status == StatusUnknown {
		t.Fatalf("setup: the book without the new name is unknown: %v", without.Reasons)
	}

	t.Run("long stock and a bought call are charged at their value", func(t *testing.T) {
		v := Evaluate(input(t, append(base, young, youngCall)...))
		if v.Status == StatusUnknown {
			t.Fatalf("a long line with three closes blinded the verdict: %v", v.Reasons)
		}
		call := youngCall.Quantity * youngCall.Multiplier * OptionPrice(true, 120, 130, 0.25, iv) * 0.9
		want := 50*120*0.9 + call
		if len(v.ShortHistory) != 1 {
			t.Fatalf("short_history = %+v, want the one young name", v.ShortHistory)
		}
		got := v.ShortHistory[0]
		if got.Underlying != "NEWX" || got.Closes != 3 || got.ClosesNeeded != Closes || len(got.Lines) != 2 || math.Abs(got.ChargedBase-want) > 1e-6 {
			t.Fatalf("short_history = %+v, want NEWX, 3 of %d closes, two lines, %.2f charged", got, Closes, want)
		}
		// The risk without the charge is the book's; every scenario loses the
		// whole value on top, so the expected shortfall and both index
		// stresses carry it in full and the volatility does not.
		if math.Abs(v.Risk.ES99Base-(without.Risk.ES99Base+want)) > 1e-6 {
			t.Fatalf("es99 = %.2f, want %.2f + %.2f", v.Risk.ES99Base, without.Risk.ES99Base, want)
		}
		if math.Abs(v.Risk.SDAnnualBase-without.Risk.SDAnnualBase) > 1e-6 {
			t.Fatalf("a charged line moved the simulated volatility: %.2f vs %.2f", v.Risk.SDAnnualBase, without.Risk.SDAnnualBase)
		}
		if math.Abs(v.Stresses.IndexLossBase-(without.Stresses.IndexLossBase+want)) > 1e-6 ||
			math.Abs(v.Stresses.IndexRiseLossBase-(without.Stresses.IndexRiseLossBase+want)) > 1e-6 {
			t.Fatalf("index stresses = %.2f / %.2f, want the book's %.2f / %.2f plus %.2f", v.Stresses.IndexLossBase, v.Stresses.IndexRiseLossBase, without.Stresses.IndexLossBase, without.Stresses.IndexRiseLossBase, want)
		}
		for _, u := range v.Underlyings {
			if u.Underlying != "NEWX" {
				continue
			}
			if !u.ShortHistory || u.ContributionPct != 0 || u.Beta != 0 {
				t.Fatalf("the charged name reads as measured: %+v", u)
			}
			if !(u.GapDownLossBase > 0) || u.GapShockPct != 25 {
				t.Fatalf("the charged name took no one-name gap: %+v", u)
			}
		}
		marked := 0
		for _, l := range v.Legs {
			if l.Underlying == "NEWX" {
				if !l.ShortHistory || l.ContributionPct != 0 || l.Unknown != "" {
					t.Fatalf("charged leg = %+v", l)
				}
				marked++
			}
		}
		if marked != 2 {
			t.Fatalf("%d charged legs reported, want 2", marked)
		}
		sum := 0.0
		for _, l := range v.Legs {
			sum += l.ContributionPct
		}
		if math.Abs(sum-100) > 1e-6 {
			t.Fatalf("contributions sum to %.6f%%, want 100", sum)
		}
	})

	for _, tc := range []struct {
		name string
		leg  Leg
	}{
		{"short stock", Leg{ID: "5", Symbol: "NEWX", Underlying: "NEWX", Kind: KindStock, Quantity: -50, Multiplier: 1, Currency: "USD", Spot: 120}},
		{"sold put", Leg{ID: "6", Symbol: "NEWX P100", Underlying: "NEWX", Kind: KindOption, Quantity: -1, Multiplier: 100, Currency: "USD", Spot: 120, Right: "P", Strike: 100, TimeToExpiryYears: 0.25, IV: &iv}},
		{"sold call beside the shares", Leg{ID: "7", Symbol: "NEWX C140", Underlying: "NEWX", Kind: KindOption, Quantity: -1, Multiplier: 100, Currency: "USD", Spot: 120, Right: "C", Strike: 140, TimeToExpiryYears: 0.25, IV: &iv}},
	} {
		t.Run(tc.name+" keeps the verdict unknown", func(t *testing.T) {
			legs := append(append([]Leg{}, base...), tc.leg)
			if tc.leg.Kind == KindOption && tc.leg.Right == "C" {
				legs = append(legs, young)
			}
			v := Evaluate(input(t, legs...))
			if v.Status != StatusUnknown || !strings.Contains(strings.Join(v.Reasons, "; "), "daily history for NEWX holds 3 closes; 253 are needed") {
				t.Fatalf("a short line on a three-close name was judged: %s %v", v.Status, v.Reasons)
			}
			if v.ShortHistory != nil {
				t.Fatalf("an unknown verdict named a charge: %+v", v.ShortHistory)
			}
		})
	}

	t.Run("the index never is charged", func(t *testing.T) {
		in := testInput(t, base)
		in.Series[IndexSymbol] = tail(in.Series[IndexSymbol], 3)
		if v := Evaluate(in); v.Status != StatusUnknown {
			t.Fatalf("a short index history was charged: %s", v.Status)
		}
	})

	t.Run("an option priced on a fallback volatility stays priceable", func(t *testing.T) {
		in := input(t, append(base, young, youngCall)...)
		in.Legs[len(in.Legs)-1].IVSource = IVSourceMark
		in.Policy.IVFallback = &IVFallback{ShortBandPct: 10, MonthBandPct: 10, LongBandPct: 10, MeasuredPct: 100, RangePct: 100}
		v := Evaluate(in)
		if v.Status == StatusUnknown || v.IVFallback == nil {
			t.Fatalf("fallback verdict = %s %v %+v", v.Status, v.Reasons, v.IVFallback)
		}
	})
}
