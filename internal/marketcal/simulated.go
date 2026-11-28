package marketcal

import (
	"slices"
	"sync"
	"time"
)

// simulatedSessions holds the TWS simulator's synthetic sessions by market;
// empty in every daemon but a simulated one.
var simulatedSessions struct {
	sync.RWMutex
	windows map[Market][]Window
}

// SimulateSession replaces the official calendar of one market with a single
// synthetic regular session [Open, Close) in wall-clock time; a zero window
// makes the market closed all day, and Clear removes the replacement. Only a
// daemon running against the TWS simulator calls it (cmd/canary-sim): the
// installed daemon never does, and a test holds every caller to that path.
func SimulateSession(market Market, w Window) {
	SimulateSessions(market, w)
}

// SimulateSessions is SimulateSession with consecutive synthetic sessions,
// in order: between two of them the market is closed and names the next
// open, and the k-th is dated k days after the first's open, so a plan can
// be carried over simulated nights within minutes of wall-clock time.
func SimulateSessions(market Market, ws ...Window) {
	simulatedSessions.Lock()
	defer simulatedSessions.Unlock()
	if simulatedSessions.windows == nil {
		simulatedSessions.windows = map[Market][]Window{}
	}
	simulatedSessions.windows[market] = slices.Clone(ws)
}

// ClearSimulatedSessions restores the official calendar for every market.
func ClearSimulatedSessions() {
	simulatedSessions.Lock()
	defer simulatedSessions.Unlock()
	simulatedSessions.windows = nil
}

// simulatedSession is the synthetic session of a market at an instant, if
// one is installed and answers for that instant's day in the market's zone.
func simulatedSession(spec calendarSpec, at time.Time) (Session, bool) {
	simulatedSessions.RLock()
	ws, ok := simulatedSessions.windows[spec.market]
	simulatedSessions.RUnlock()
	if !ok || len(ws) == 0 {
		return Session{}, false
	}
	// One session stands for the day it opens on. A day before it keeps its
	// official session: answered with this window, the last real close would
	// read as not yet settled for as long as the simulated session is open.
	if len(ws) == 1 && !ws[0].Open.IsZero() {
		if loc, err := time.LoadLocation(spec.timezone); err == nil && at.In(loc).Format(time.DateOnly) < ws[0].Open.In(loc).Format(time.DateOnly) {
			return Session{}, false
		}
	}
	// The session at hand is the window at or after at, else the last.
	k := len(ws) - 1
	for i, w := range ws {
		if at.Before(w.Close) {
			k = i
			break
		}
	}
	w := ws[k]
	date := at.In(time.UTC).Format("2006-01-02")
	if len(ws) > 1 {
		date = ws[0].Open.In(time.UTC).AddDate(0, 0, k).Format("2006-01-02")
	}
	s := Session{
		Market: spec.market, Label: spec.label, Date: date, Timezone: spec.timezone,
		Source: "twssim", Notes: "simulated session: the TWS simulator's exchange hours, not the official calendar",
	}
	if w.Open.IsZero() {
		s.State, s.Reason = StateClosed, "simulated: closed"
		return s, true
	}
	s.State, s.Open, s.Close, s.Windows = StateRegular, w.Open, w.Close, []Window{w}
	s.IsOpen = !at.Before(w.Open) && at.Before(w.Close)
	if at.Before(w.Open) {
		open, closeT := w.Open, w.Close
		s.NextOpen, s.NextClose = &open, &closeT
	}
	return s, true
}
