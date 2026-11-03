package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func regimeGateTestInputs(fetched time.Time) regimeGateInputs {
	in := regimeGateInputs{Now: time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC), Latest: "2026-10-30", SPYFetched: fetched, HYGFetched: fetched,
		Breadth: &rpc.BreadthSPXResult{State: rpc.BreadthStateReady, SessionKey: "2026-10-30", PctAbove50DMA: 58, Coverage50: 500}}
	i := 0
	for d := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC); !d.After(time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		date := d.Format(time.DateOnly)
		in.SPY = append(in.SPY, ibkrlib.HistoricalBar{Date: date, Close: 500 + float64(i)})
		in.HYG = append(in.HYG, ibkrlib.HistoricalBar{Date: date, Close: 70 + 0.01*float64(i)})
		in.Breadth.History = append(in.Breadth.History, rpc.BreadthDailyValue{Date: date, PctAbove50DMA: 60, Coverage50: 500})
		i++
	}
	// A partial bar for the next session is never read.
	in.SPY = append(in.SPY, ibkrlib.HistoricalBar{Date: "2026-11-02", Close: 1})
	return in
}

var regimeGateTestParams = core.GateParams{BreadthMinPct: 50, HalfScalePct: 50, StaleAfterSessions: 1, ConfirmCloses: 2}

func gateReading(g *rpc.RegimeGateView, id string) core.GateReading {
	for _, r := range g.Readings {
		if r.ID == id {
			return r
		}
	}
	return core.GateReading{}
}

func TestRegimeGateReadsSettledBarsOnly(t *testing.T) {
	after := regimeGateFrom(regimeGateTestInputs(time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC)), regimeGateTestParams)
	if after.State != core.GateOn || after.Session != "2026-10-30" {
		t.Fatalf("gate = %+v, want on for 2026-10-30", after.Gate)
	}
	if tr := gateReading(after, core.GateTrend); tr.AsOf != "2026-10-30" {
		t.Fatalf("trend read %s, want the settled 2026-10-30 close", tr.AsOf)
	}
	// Fetched during the session: that session's bar is partial and dropped;
	// the previous close, one session old, still counts.
	during := regimeGateFrom(regimeGateTestInputs(time.Date(2026, 10, 30, 18, 0, 0, 0, time.UTC)), regimeGateTestParams)
	for _, id := range []string{core.GateTrend, core.GateCredit} {
		if r := gateReading(during, id); r.AsOf != "2026-10-29" || !r.On {
			t.Fatalf("%s = %+v, want on from 2026-10-29", id, r)
		}
	}
	if b := gateReading(during, core.GateBreadth); b.AsOf != "2026-10-30" {
		t.Fatalf("breadth read %s, want 2026-10-30", b.AsOf)
	}
}

func TestRegimeGateBreadthNotReadyCountsOff(t *testing.T) {
	in := regimeGateTestInputs(time.Date(2026, 10, 30, 21, 0, 0, 0, time.UTC))
	in.Breadth.State = rpc.BreadthStateComputing
	g := regimeGateFrom(in, regimeGateTestParams)
	if b := gateReading(g, core.GateBreadth); b.On || b.Reason == "" {
		t.Fatalf("breadth = %+v, want off with a reason", b)
	}
	if g.State != core.GateHalf || g.ScalePct != 50 {
		t.Fatalf("gate %s at %g, want half at 50", g.State, g.ScalePct)
	}
	in.SPY, in.HYG = nil, nil
	if g := regimeGateFrom(in, regimeGateTestParams); g.State != core.GateOff || g.Reason == "" {
		t.Fatalf("no history: gate = %+v, want off with a reason", g)
	}
}

func TestRegimeGateUnwrittenPolicyReadsOff(t *testing.T) {
	s := &Server{}
	g := s.measureRegimeGate(t.Context(), nil, time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC))
	if g.State != core.GateOff || g.Reason == "" {
		t.Fatalf("gate = %+v, want off with the unwritten table named", g)
	}
}
