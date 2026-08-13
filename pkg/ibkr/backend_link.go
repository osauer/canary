package ibkr

import (
	"fmt"
	"strings"
	"time"
)

// Backend-link episode coalescing. Five weeks of observed history show the
// TWS↔IBKR upstream link flapping on every logged day, overwhelmingly in the
// US evening/overnight band, with a median outage of 13 seconds and every
// restore reporting server-side data maintained (code 1102). Logging each
// blip individually made the most operationally important sequence in the
// log unreadable, so the connector logs per disturbance instead: the first
// loss and first restore of an episode at WARN, follow-up blips within
// backendEpisodeGap at INFO, and one WARN summary when the episode ends.
// Two conditions stay loud on every single event because they are the
// actionable ones: a loss while the trading session is open (an order
// transmitted then would be refused) and an outage exceeding
// backendOutageAttention (that is a wedge, not a blip).
const (
	// backendEpisodeGap joins a new loss to the running episode when it
	// arrives within this much of the previous restore. Observed flapping
	// spaced losses ~7 minutes apart for hours.
	backendEpisodeGap = 10 * time.Minute
	// backendOutageAttention is the outage duration above which a restore is
	// always logged at WARN: the self-healing blips cluster far below it
	// (p90 42s), while everything observed above it needed a human.
	backendOutageAttention = 5 * time.Minute
)

// MaintenanceWindow is a recurring broker maintenance window: a start/end
// wall-clock range in Loc, active on the listed start days. End at or before
// Start means the window crosses midnight (the day filter applies to the
// start day). This models the broker's schedule, not any market calendar —
// resets also happen on days markets are closed.
type MaintenanceWindow struct {
	Days        [7]bool // indexed by time.Weekday of the window's START day in Loc
	StartMinute int     // minutes since midnight in Loc, inclusive
	EndMinute   int     // minutes since midnight in Loc, exclusive
	Loc         *time.Location
}

// Contains reports whether t falls inside the window.
func (w MaintenanceWindow) Contains(t time.Time) bool {
	if w.Loc == nil {
		return false
	}
	lt := t.In(w.Loc)
	m := lt.Hour()*60 + lt.Minute()
	if w.StartMinute < w.EndMinute {
		return w.Days[lt.Weekday()] && m >= w.StartMinute && m < w.EndMinute
	}
	// Crosses midnight: after the start on a listed day, or before the end
	// on the day after a listed day.
	if m >= w.StartMinute {
		return w.Days[lt.Weekday()]
	}
	if m < w.EndMinute {
		return w.Days[(lt.Weekday()+6)%7]
	}
	return false
}

func maintenanceWindowsContain(windows []MaintenanceWindow, t time.Time) bool {
	for _, w := range windows {
		if w.Contains(t) {
			return true
		}
	}
	return false
}

// DefaultIBKRMaintenanceWindows returns IBKR's documented North America
// nightly reset schedule: 23:45–00:45 ET Saturday through Thursday and
// 23:45–00:30 ET on Friday.
func DefaultIBKRMaintenanceWindows() ([]MaintenanceWindow, error) {
	return ParseMaintenanceWindows([]string{
		"Sat-Thu 23:45-00:45 America/New_York",
		"Fri 23:45-00:30 America/New_York",
	})
}

// ParseMaintenanceWindows parses window specs of the form
// "DAY[-DAY] HH:MM-HH:MM TZ", e.g. "Sat-Thu 23:45-00:45 America/New_York".
// A day range wraps the week; an end time at or before the start crosses
// midnight. Every spec must parse or the whole call fails.
func ParseMaintenanceWindows(specs []string) ([]MaintenanceWindow, error) {
	out := make([]MaintenanceWindow, 0, len(specs))
	for _, spec := range specs {
		w, err := parseMaintenanceWindow(spec)
		if err != nil {
			return nil, fmt.Errorf("maintenance window %q: %w", spec, err)
		}
		out = append(out, w)
	}
	return out, nil
}

var maintenanceDayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseMaintenanceWindow(spec string) (MaintenanceWindow, error) {
	fields := strings.Fields(spec)
	if len(fields) != 3 {
		return MaintenanceWindow{}, fmt.Errorf("want \"DAY[-DAY] HH:MM-HH:MM TZ\", got %d fields", len(fields))
	}
	var w MaintenanceWindow
	first, last, ok := strings.Cut(strings.ToLower(fields[0]), "-")
	from, okFrom := maintenanceDayNames[first]
	if !okFrom {
		return MaintenanceWindow{}, fmt.Errorf("unknown day %q", first)
	}
	if !ok {
		w.Days[from] = true
	} else {
		to, okTo := maintenanceDayNames[last]
		if !okTo {
			return MaintenanceWindow{}, fmt.Errorf("unknown day %q", last)
		}
		for d := from; ; d = (d + 1) % 7 {
			w.Days[d] = true
			if d == to {
				break
			}
		}
	}
	startRaw, endRaw, ok := strings.Cut(fields[1], "-")
	if !ok {
		return MaintenanceWindow{}, fmt.Errorf("want HH:MM-HH:MM, got %q", fields[1])
	}
	var err error
	if w.StartMinute, err = parseWallMinute(startRaw); err != nil {
		return MaintenanceWindow{}, err
	}
	if w.EndMinute, err = parseWallMinute(endRaw); err != nil {
		return MaintenanceWindow{}, err
	}
	if w.Loc, err = time.LoadLocation(fields[2]); err != nil {
		return MaintenanceWindow{}, fmt.Errorf("timezone %q: %w", fields[2], err)
	}
	return w, nil
}

func parseWallMinute(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("time %q: want HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// SetBackendMaintenanceWindows installs the broker maintenance schedule used
// to annotate backend-link losses. Call before Start.
func (c *Connector) SetBackendMaintenanceWindows(windows []MaintenanceWindow) {
	c.backendConnMu.Lock()
	c.maintWindows = windows
	c.backendConnMu.Unlock()
}

// SetBackendSessionOpen installs the predicate that decides whether a
// backend-link loss happened while the trading session was open — the case
// that always warns per event. Call before Start; nil disables the check.
func (c *Connector) SetBackendSessionOpen(open func(time.Time) bool) {
	c.backendConnMu.Lock()
	c.backendSessionOpen = open
	c.backendConnMu.Unlock()
}

// finalizeBackendEpisode closes the running flap episode after
// backendEpisodeGap of quiet and logs its one-line summary. Single-blip
// episodes are skipped: their loss and restore lines already told the whole
// story. gen guards against a fired timer that lost the race against a new
// loss extending the episode.
func (c *Connector) finalizeBackendEpisode(gen int) {
	c.backendConnMu.Lock()
	if !c.epActive || c.backendConnDown || gen != c.epGen {
		c.backendConnMu.Unlock()
		return
	}
	losses, inWindow, longest := c.epLosses, c.epLossesInWindow, c.epLongest
	span := c.epLastRestore.Sub(c.epStart)
	c.epActive = false
	c.epTimer = nil
	c.backendConnMu.Unlock()

	if losses <= 1 {
		return
	}
	annotation := ""
	switch {
	case inWindow == losses:
		annotation = " (all inside IBKR maintenance window)"
	case inWindow > 0:
		annotation = fmt.Sprintf(" (%d of %d inside IBKR maintenance window)", inWindow, losses)
	}
	c.logWarn("TWS backend link episode ended: %d losses over %s, longest outage %s%s",
		losses, span.Round(time.Second), longest.Round(time.Second), annotation)
}

func (c *Connector) stopBackendEpisodeTimer() {
	c.backendConnMu.Lock()
	if c.epTimer != nil {
		c.epTimer.Stop()
		c.epTimer = nil
	}
	c.backendConnMu.Unlock()
}
