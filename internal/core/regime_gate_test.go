package core

import (
	"math"
	"testing"
	"time"
)

// gateFixture is a weekday calendar of n sessions ending 2026-10-30, SPY and
// HYG rising steadily and breadth at 60% on every session.
func gateFixture(n int) GateInput {
	var cal []string
	for d := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC); len(cal) < n; d = d.AddDate(0, 0, -1) {
		if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
			cal = append([]string{d.Format(time.DateOnly)}, cal...)
		}
	}
	in := GateInput{Calendar: cal, Breadth: map[string]float64{}}
	for i, d := range cal {
		in.SPY.Dates, in.SPY.Closes = append(in.SPY.Dates, d), append(in.SPY.Closes, 500+float64(i))
		in.HYG.Dates, in.HYG.Closes = append(in.HYG.Dates, d), append(in.HYG.Closes, 70+0.01*float64(i))
		in.Breadth[d] = 60
	}
	return in
}

var gateOwner = GateParams{BreadthMinPct: 50, HalfScalePct: 50, StaleAfterSessions: 1, ConfirmCloses: 2}

func reading(g Gate, id string) GateReading {
	for _, r := range g.Readings {
		if r.ID == id {
			return r
		}
	}
	return GateReading{}
}

func TestRegimeGateThreeOnIsFullScale(t *testing.T) {
	g := MeasureGate(gateFixture(260), gateOwner)
	if g.State != GateOn || g.ScalePct != 100 || g.Session != "2026-10-30" {
		t.Fatalf("gate = %+v, want on at 100 on 2026-10-30", g)
	}
}

func TestRegimeGateReadingFlipsOnlyAfterTwoCloses(t *testing.T) {
	in := gateFixture(260)
	last := len(in.Calendar) - 1
	in.Breadth[in.Calendar[last]] = 45
	g := MeasureGate(in, gateOwner)
	if b := reading(g, GateBreadth); !b.On || !b.Pending || b.Latest {
		t.Fatalf("one close under the bar: breadth = %+v, want on and pending", b)
	}
	if g.State != GateOn {
		t.Fatalf("one close under the bar: state %s, want on", g.State)
	}
	in.Breadth[in.Calendar[last-1]] = 49
	g = MeasureGate(in, gateOwner)
	if b := reading(g, GateBreadth); b.On || b.Pending || b.Since != in.Calendar[last] {
		t.Fatalf("two closes under the bar: breadth = %+v, want off since the second", b)
	}
	if g.State != GateHalf || g.ScalePct != 50 {
		t.Fatalf("two closes under the bar: gate %s at %g, want half at 50", g.State, g.ScalePct)
	}
	// A single close back above does not restore it.
	in.Breadth[in.Calendar[last]] = 55
	in.Breadth[in.Calendar[last-2]] = 40
	if b := reading(MeasureGate(in, gateOwner), GateBreadth); b.On || !b.Pending {
		t.Fatalf("one close back above: breadth = %+v, want still off, pending", b)
	}
	// With confirm_closes 1 the latest close decides.
	p := gateOwner
	p.ConfirmCloses = 1
	if b := reading(MeasureGate(in, p), GateBreadth); !b.On {
		t.Fatalf("confirm 1: breadth = %+v, want on", b)
	}
}

func TestRegimeGateOneOrNoneIsOff(t *testing.T) {
	in := gateFixture(260)
	n := len(in.Calendar)
	for i := n - 5; i < n; i++ {
		in.Breadth[in.Calendar[i]] = 30
		in.HYG.Closes[i] = 60
	}
	g := MeasureGate(in, gateOwner)
	if g.State != GateOff || g.ScalePct != 0 || !reading(g, GateTrend).On {
		t.Fatalf("gate = %+v, want off at 0 with only the trend on", g)
	}
}

func TestRegimeGateStaleOrMissingReadingCountsOff(t *testing.T) {
	in := gateFixture(260)
	n := len(in.Calendar)
	// HYG one session behind is still read; two behind is stale.
	in.HYG.Dates, in.HYG.Closes = in.HYG.Dates[:n-1], in.HYG.Closes[:n-1]
	if c := reading(MeasureGate(in, gateOwner), GateCredit); !c.On || c.AsOf != in.Calendar[n-2] {
		t.Fatalf("one session behind: credit = %+v, want on", c)
	}
	in.HYG.Dates, in.HYG.Closes = in.HYG.Dates[:n-2], in.HYG.Closes[:n-2]
	g := MeasureGate(in, gateOwner)
	if c := reading(g, GateCredit); c.On || c.Reason == "" {
		t.Fatalf("two sessions behind: credit = %+v, want off with a reason", c)
	}
	if g.State != GateHalf {
		t.Fatalf("stale credit: state %s, want half", g.State)
	}
	in.Breadth = nil
	if g := MeasureGate(in, gateOwner); g.State != GateOff || reading(g, GateBreadth).Reason == "" {
		t.Fatalf("no breadth: gate = %+v, want off with the breadth reason", g)
	}
	// A bar dated after the last completed session (today's partial bar)
	// is never read.
	in = gateFixture(260)
	in.Calendar = in.Calendar[:n-1]
	if tr := reading(MeasureGate(in, gateOwner), GateTrend); tr.AsOf != in.Calendar[n-2] {
		t.Fatalf("trend read %s, want the last completed session %s", tr.AsOf, in.Calendar[n-2])
	}
}

// At a month-end the daily trend reading is the core's own filter.
func TestRegimeGateTrendIsTheLibraryFilterAtMonthEnd(t *testing.T) {
	in := gateFixture(300)
	for i := range in.SPY.Closes {
		in.SPY.Closes[i] = 500 + 40*math.Sin(float64(i)/17)
	}
	sig, _ := MeasureWith(TrendSMA10, "gate", in.Calendar, in.SPY.Dates, in.SPY.Closes, "2026-10-30")
	if sig.Status != StatusOK {
		t.Fatalf("library signal %s: %s", sig.Status, sig.Reason)
	}
	tr := reading(MeasureGate(in, GateParams{BreadthMinPct: 50, StaleAfterSessions: 1, ConfirmCloses: 1}), GateTrend)
	if math.Abs(tr.Level-sig.SMA10) > 1e-9 || tr.Value != sig.Close || tr.Latest != sig.TrendOn {
		t.Fatalf("gate trend %+v, library close %g sma10 %g on %v", tr, sig.Close, sig.SMA10, sig.TrendOn)
	}
}
