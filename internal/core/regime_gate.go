package core

import (
	"fmt"
	"slices"
)

// The regime gate (owner decisions 2026-11-03 13:29 CET): three readings from
// daily closes Canary already keeps, and three states from how many hold.
// The index's trend is the library's trend-sma10m read on every close, not
// only at month-end: SPY's close above the mean of the current close and the
// nine month-end closes before it, which at a month-end is the core's own
// filter. Breadth is the share of S&P 500 constituents above their 50-day
// average, above the owner's breadth_min_pct. Credit is HYG's close above
// the mean of its last 50 closes. All three on: on, full scale. Two: half,
// at half_scale_pct. One or none: off, scale 0, no new entries and no adds.
//
// A reading flips only when confirm_closes consecutive observed closes
// agree (owner: two), so the state is a function of the history alone
// and no restart can lose it. A reading with no close within
// stale_after_sessions sessions of the latest completed session counts as
// off. The ten months and the 50 days are the rule's, compiled.

// Gate reading ids.
const (
	GateTrend   = "trend"
	GateBreadth = "breadth"
	GateCredit  = "credit"
)

// Gate states.
const (
	GateOn   = "on"
	GateHalf = "half"
	GateOff  = "off"
)

const (
	// GateCreditDays is the credit reading's average: 50 closes.
	GateCreditDays = 50
	// GateWalkSessions is how many sessions the confirmation walk reads.
	GateWalkSessions = 30
)

// GateParams are the owner's numbers from [mandate.regime_gate].
type GateParams struct {
	BreadthMinPct      float64 `json:"breadth_min_pct"`
	HalfScalePct       float64 `json:"half_scale_pct"`
	StaleAfterSessions int     `json:"stale_after_sessions"`
	ConfirmCloses      int     `json:"confirm_closes"`
}

// GateInput is what the gate reads: the US equity sessions to the latest
// completed one (ascending, inclusive), SPY's and HYG's daily closes and
// the breadth engine's daily share above the 50-day average by date. Bars
// dated after the last session are ignored.
type GateInput struct {
	Calendar []string
	SPY      Series
	HYG      Series
	Breadth  map[string]float64
}

// GateReading is one reading's confirmed state and the close it rests on.
type GateReading struct {
	ID string `json:"id"`
	// On is the confirmed reading the gate counts.
	On bool `json:"on"`
	// Latest is the latest close's own reading, before confirmation.
	Latest bool `json:"latest"`
	// AsOf is the session of the latest close read.
	AsOf string `json:"as_of,omitempty"`
	// Value and Level are what was compared on AsOf: SPY's close and its
	// ten-month average, the breadth share and breadth_min_pct, HYG's
	// close and its 50-day average.
	Value float64 `json:"value,omitempty"`
	Level float64 `json:"level,omitempty"`
	// Since is the session the confirmed reading took hold, when the walk
	// saw it change.
	Since string `json:"since,omitempty"`
	// Pending is set when the latest close disagrees with the confirmed
	// reading and has not yet held confirm_closes closes.
	Pending bool `json:"pending,omitempty"`
	// Reason says why the reading is off for want of data.
	Reason string `json:"reason,omitempty"`
}

// Gate is the measured regime gate.
type Gate struct {
	State    string        `json:"state"`
	ScalePct float64       `json:"scale_pct"`
	Session  string        `json:"session,omitempty"`
	Readings []GateReading `json:"readings"`
	Params   GateParams    `json:"params"`
}

// MeasureGate computes the gate. It never errs: a reading it cannot make
// counts as off, with the reason.
func MeasureGate(in GateInput, p GateParams) Gate {
	g := Gate{Params: p}
	cal := in.Calendar
	if len(cal) > 0 {
		g.Session = cal[len(cal)-1]
	}
	walk := cal
	if len(walk) > GateWalkSessions {
		walk = walk[len(walk)-GateWalkSessions:]
	}
	g.Readings = []GateReading{
		gateWalk(GateTrend, cal, walk, p, trendReader(in.SPY, g.Session)),
		gateWalk(GateBreadth, cal, walk, p, func(d string) (bool, float64, float64, string) {
			v, ok := in.Breadth[d]
			if !ok {
				return false, 0, 0, "no breadth reading on " + d
			}
			return v > p.BreadthMinPct, v, p.BreadthMinPct, ""
		}),
		gateWalk(GateCredit, cal, walk, p, averageReader(in.HYG, g.Session, GateCreditDays)),
	}
	on := 0
	for _, r := range g.Readings {
		if r.On {
			on++
		}
	}
	switch {
	case on == 3:
		g.State, g.ScalePct = GateOn, 100
	case on == 2:
		g.State, g.ScalePct = GateHalf, p.HalfScalePct
	default:
		g.State, g.ScalePct = GateOff, 0
	}
	return g
}

// gateReader reads one session: the reading, the compared value and level,
// or why it cannot.
type gateReader func(date string) (on bool, value, level float64, missing string)

// gateWalk confirms one reading over the walk's sessions with data, then
// applies the staleness rule against the whole calendar.
func gateWalk(id string, cal, walk []string, p GateParams, read gateReader) GateReading {
	out := GateReading{ID: id}
	confirm := max(p.ConfirmCloses, 1)
	var seen []bool
	var held bool
	lastMissing := ""
	for _, d := range walk {
		on, v, lvl, missing := read(d)
		if missing != "" {
			lastMissing = missing
			continue
		}
		seen = append(seen, on)
		out.AsOf, out.Value, out.Level, out.Latest = d, v, lvl, on
		switch {
		case len(seen) == 1:
			held = on
		case on != held && len(seen) >= confirm && allEqual(seen[len(seen)-confirm:], on):
			held, out.Since = on, d
		}
	}
	if out.AsOf == "" {
		out.Reason = "no reading in the last " + fmt.Sprint(len(walk)) + " sessions"
		if lastMissing != "" {
			out.Reason = lastMissing
		}
		return out
	}
	out.On, out.Pending = held, out.Latest != held
	if age := len(cal) - 1 - slices.Index(cal, out.AsOf); age > p.StaleAfterSessions {
		out.On, out.Pending = false, false
		out.Reason = fmt.Sprintf("the latest reading is from %s, %d sessions old", out.AsOf, age)
	}
	return out
}

func allEqual(v []bool, want bool) bool {
	for _, x := range v {
		if x != want {
			return false
		}
	}
	return true
}

// seriesTo returns the series' dates and closes on or before last, with a
// date-to-index map.
func seriesTo(s Series, last string) ([]string, []float64, map[string]int) {
	var dates []string
	var closes []float64
	for i, d := range s.Dates {
		if i >= len(s.Closes) || (last != "" && d > last) || !(s.Closes[i] > 0) {
			continue
		}
		dates, closes = append(dates, d), append(closes, s.Closes[i])
	}
	at := make(map[string]int, len(dates))
	for i, d := range dates {
		at[d] = i
	}
	return dates, closes, at
}

// trendReader reads trend-sma10m on any close: the close above the mean of
// itself and the nine month-end closes before it.
func trendReader(spy Series, last string) gateReader {
	dates, closes, at := seriesTo(spy, last)
	return func(d string) (bool, float64, float64, string) {
		i, ok := at[d]
		if !ok {
			return false, 0, 0, "no SPY close on " + d
		}
		ends := MonthEnds(dates[:i+1])
		if len(ends) < TrendSMA10.Months {
			return false, 0, 0, fmt.Sprintf("SPY needs %d month-ends to %s, has %d", TrendSMA10.Months, d, len(ends))
		}
		sum := 0.0
		for _, e := range ends[len(ends)-TrendSMA10.Months:] {
			sum += closes[at[e]]
		}
		avg := sum / float64(TrendSMA10.Months)
		return closes[i] > avg, closes[i], avg, ""
	}
}

// averageReader reads a close above the mean of its last n closes, its own
// included.
func averageReader(s Series, last string, n int) gateReader {
	_, closes, at := seriesTo(s, last)
	return func(d string) (bool, float64, float64, string) {
		i, ok := at[d]
		if !ok {
			return false, 0, 0, "no HYG close on " + d
		}
		if i+1 < n {
			return false, 0, 0, fmt.Sprintf("HYG needs %d closes to %s, has %d", n, d, i+1)
		}
		sum := 0.0
		for _, c := range closes[i+1-n : i+1] {
			sum += c
		}
		avg := sum / float64(n)
		return closes[i] > avg, closes[i], avg, ""
	}
}
