package marketcal

import (
	"sync"
	"time"
)

// simulatedSessions holds the TWS simulator's synthetic sessions by market;
// empty in every daemon but a simulated one.
var simulatedSessions struct {
	sync.RWMutex
	windows map[Market]Window
}

// SimulateSession replaces the official calendar of one market with a single
// synthetic regular session [Open, Close) in wall-clock time; a zero window
// makes the market closed all day, and Clear removes the replacement. Only a
// daemon running against the TWS simulator calls it (cmd/canary-sim): the
// installed daemon never does, and a test holds every caller to that path.
func SimulateSession(market Market, w Window) {
	simulatedSessions.Lock()
	defer simulatedSessions.Unlock()
	if simulatedSessions.windows == nil {
		simulatedSessions.windows = map[Market]Window{}
	}
	simulatedSessions.windows[market] = w
}

// ClearSimulatedSessions restores the official calendar for every market.
func ClearSimulatedSessions() {
	simulatedSessions.Lock()
	defer simulatedSessions.Unlock()
	simulatedSessions.windows = nil
}

// simulatedSession is the synthetic session of a market at an instant, if
// one is installed.
func simulatedSession(spec calendarSpec, at time.Time) (Session, bool) {
	simulatedSessions.RLock()
	w, ok := simulatedSessions.windows[spec.market]
	simulatedSessions.RUnlock()
	if !ok {
		return Session{}, false
	}
	s := Session{
		Market: spec.market, Label: spec.label, Date: at.In(time.UTC).Format("2006-01-02"), Timezone: spec.timezone,
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
