package core

import (
	"math"
	"strings"
	"testing"
	"time"
)

// strongFixture builds n weekday sessions ending 2026-10-30: SPY rising
// 0.05% a session and each name compounding at its own daily rate, with a
// volume of 2 million shares.
func strongFixture(n int, rates map[string]float64) StrongNamesInput {
	var dates []string
	for d := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC); len(dates) < n; d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			dates = append([]string{d.Format(time.DateOnly)}, dates...)
		}
	}
	in := StrongNamesInput{Names: map[string]NameSeries{}, GateState: GateOn, GateScale: 100, ATR: map[string]float64{}, RiskBase: 1_000_000, AllocationPct: 2}
	for i, d := range dates {
		in.SPY.Dates, in.SPY.Closes = append(in.SPY.Dates, d), append(in.SPY.Closes, 500*math.Pow(1.0005, float64(i)))
	}
	for sym, rate := range rates {
		s := NameSeries{Dates: dates}
		for i := range dates {
			s.Closes = append(s.Closes, 50*math.Pow(1+rate, float64(i)))
			s.Volumes = append(s.Volumes, 2e6)
		}
		in.Names[sym] = s
	}
	return in
}

// dip sets the last close k% under the ten-session high.
func dip(in StrongNamesInput, sym string, pct float64) {
	s := in.Names[sym]
	n := len(s.Closes)
	s.Closes[n-1] = s.Closes[n-2] * (1 - pct/100)
	in.Names[sym] = s
}

func entry(sc StrongNamesScreen, sym string) (EntryPlan, bool) {
	for _, e := range sc.Entries {
		if e.Symbol == sym {
			return e, true
		}
	}
	return EntryPlan{}, false
}

func TestStrongNamesEntersALeaderOnAShortDip(t *testing.T) {
	in := strongFixture(400, map[string]float64{"LEAD": 0.004, "WEAK": -0.001})
	dip(in, "LEAD", 5)
	in.ATR["LEAD"] = 2
	sc := StrongNamesV1.Screen(in)
	if len(sc.Watch) != 1 || sc.Watch[0].Symbol != "LEAD" || !sc.Watch[0].Leading {
		t.Fatalf("watch = %+v, want LEAD leading alone", sc.Watch)
	}
	p, ok := entry(sc, "LEAD")
	if !ok {
		t.Fatalf("no entry for LEAD: %+v", sc)
	}
	// Risk 0.5% of 1,000,000 at 2.5 × 2 = 1,000 shares; the 5% cap at the
	// close allows fewer.
	byCap := math.Floor(50000 / p.Ref)
	if want := int(math.Min(1000, byCap)); p.RuleUnits != want || p.Units != want {
		t.Fatalf("plan %+v, want %d units", p, want)
	}
	if p.Stop != p.Ref-5 || p.JudgeAfter != 5 {
		t.Fatalf("plan %+v", p)
	}
	// At half the size while the gate is half.
	in.GateState, in.GateScale = GateHalf, 50
	if h, _ := entry(StrongNamesV1.Screen(in), "LEAD"); h.RuleUnits != int(math.Floor(math.Min(1000, 50000/p.Ref)*0.5)) {
		t.Fatalf("half plan %+v", h)
	}
	// None while the gate is off.
	in.GateState, in.GateScale = GateOff, 0
	if o, _ := entry(StrongNamesV1.Screen(in), "LEAD"); o.Units != 0 || !strings.Contains(o.Why, "off") {
		t.Fatalf("off plan %+v", o)
	}
	// With no risk allocation the plan stands and buys nothing.
	in.GateState, in.GateScale, in.AllocationPct = GateOn, 100, 0
	if z, _ := entry(StrongNamesV1.Screen(in), "LEAD"); z.RuleUnits == 0 || z.Units != 0 || !strings.Contains(z.Why, "allocation") {
		t.Fatalf("zero-allocation plan %+v", z)
	}
}

func TestStrongNamesDipBandAndEligibility(t *testing.T) {
	// A steep leader, so an 11% dip still closes above its 50-session
	// average and the trend holds.
	for pct, want := range map[float64]bool{1: false, 3.5: true, 11: true, 15: false} {
		in := strongFixture(400, map[string]float64{"LEAD": 0.006})
		dip(in, "LEAD", pct)
		in.ATR["LEAD"] = 2
		if _, ok := entry(StrongNamesV1.Screen(in), "LEAD"); ok != want {
			t.Fatalf("dip %g%%: entry %v, want %v", pct, ok, want)
		}
	}
	in := strongFixture(400, map[string]float64{"THIN": 0.004, "CHEAP": 0.004, "WILD": 0.004})
	s := in.Names["THIN"]
	for i := range s.Volumes {
		s.Volumes[i] = 1000
	}
	s = in.Names["CHEAP"]
	for i := range s.Closes {
		s.Closes[i] /= 100
	}
	s = in.Names["WILD"]
	for i := range s.Closes {
		if i%2 == 1 {
			s.Closes[i] *= 1.06
		}
	}
	sc := StrongNamesV1.Screen(in)
	if sc.Eligible != 0 || len(sc.Watch) != 0 {
		t.Fatalf("thin, cheap and wild names were eligible: %+v", sc.Watch)
	}
}

// A watch-list name leaves on a trend break, and an open name sells after
// two closes under its 50-session average.
func TestStrongNamesTrendBreakLeavesAndExits(t *testing.T) {
	in := strongFixture(400, map[string]float64{"LEAD": 0.004})
	s := in.Names["LEAD"]
	n := len(s.Closes)
	s.Closes[n-2], s.Closes[n-1] = s.Closes[n-3]*0.8, s.Closes[n-3]*0.79
	in.Held = map[string]int{"LEAD": 1}
	sc := StrongNamesV1.Screen(in)
	if len(sc.Watch) != 0 || len(sc.Departures) != 1 || sc.Departures[0].Why != "trend broke" {
		t.Fatalf("watch %+v departures %+v", sc.Watch, sc.Departures)
	}
	if len(sc.Held) != 1 || sc.Held[0].Exit == "" {
		t.Fatalf("held %+v, want an exit", sc.Held)
	}
}

// An open name at a new 20-session high adds while the gate is on and the
// lots are under the maximum.
func TestStrongNamesAddsAtANewHighOnlyWhenOn(t *testing.T) {
	in := strongFixture(400, map[string]float64{"LEAD": 0.004})
	in.Held = map[string]int{"LEAD": 1}
	if h := StrongNamesV1.Screen(in).Held[0]; !h.Add {
		t.Fatalf("held %+v, want an add", h)
	}
	in.GateState = GateHalf
	if h := StrongNamesV1.Screen(in).Held[0]; h.Add {
		t.Fatalf("an add while the gate is half: %+v", h)
	}
	in.GateState, in.Held["LEAD"] = GateOn, 2
	if h := StrongNamesV1.Screen(in).Held[0]; h.Add {
		t.Fatalf("an add past the maximum of lots: %+v", h)
	}
}
