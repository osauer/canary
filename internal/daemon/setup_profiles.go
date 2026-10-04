package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/setups"
)

// setupProfileCache holds complete, comparable prior sessions per contract.
// It is disposable market evidence, not a candidate or execution ledger, and
// a restart discards it.
//
// A live profile is keyed by exact contract, not by date: when the session
// moves on, the sessions still inside the new 20-session window are kept and
// only the missing ones are read, so a watched name costs one historical read
// per new session instead of a full rebuild. A replay (--at) on another
// session date keeps its own row so it never rewrites the live window.
//
// mu guards the rows; gate serializes cold acquisition only, so a cached
// profile never waits behind another contract's broker reads.
type setupProfileCache struct {
	mu   sync.Mutex
	gate chan struct{}
	rows map[string]*setupProfileRow
	// live is the session date of the latest current (non --at) evaluation.
	live string
	// waitForTest observes a request blocking on a busy gate.
	waitForTest func()
}

// setupProfileRow holds one contract's complete prior sessions by date.
type setupProfileRow struct {
	sessions map[string]setupProfileSession
}

// setupProfileSession is one complete comparable prior session and the daemon
// time the read that returned its bars completed.
type setupProfileSession struct {
	session  setups.Session
	acquired time.Time
}

func (p *setupProfileCache) lock(ctx context.Context) error {
	p.mu.Lock()
	if p.gate == nil {
		p.gate = make(chan struct{}, 1)
	}
	gate, wait := p.gate, p.waitForTest
	p.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return nil
	default:
	}
	if wait != nil {
		wait()
	}
	select {
	case gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *setupProfileCache) unlock() { <-p.gate }

// rowKey names the row serving an evaluation of contract on session. Current
// evaluations advance the live session; a replay of that same session shares
// the live row because its window is identical.
func (p *setupProfileCache) rowKey(contract, session string, historical bool) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !historical && session > p.live {
		p.live = session
	}
	if session == p.live {
		return contract
	}
	return contract + "@" + session
}

// assemble returns the window's sessions in order with their oldest
// acquisition, or the window sessions the row does not hold.
func (p *setupProfileCache) assemble(key string, window []setups.Session) ([]setups.Session, time.Time, []setups.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.assembleLocked(key, window)
}

func (p *setupProfileCache) assembleLocked(key string, window []setups.Session) ([]setups.Session, time.Time, []setups.Session) {
	row := p.rows[key]
	prior := make([]setups.Session, 0, len(window))
	var observed time.Time
	var missing []setups.Session
	for _, w := range window {
		if row != nil {
			held, ok := row.sessions[w.Date]
			if ok && held.session.Open.Equal(w.Open) && held.session.Close.Equal(w.Close) {
				prior = append(prior, held.session)
				// BaselineObservedAt is the oldest acquisition still in use: every
				// baseline bar was read at or after it. The newest acquisition
				// would claim corrections were checked for sessions read on
				// earlier days. For a single read this is that read's completion
				// time, the value a full rebuild always reported.
				if observed.IsZero() || held.acquired.Before(observed) {
					observed = held.acquired
				}
				continue
			}
		}
		missing = append(missing, w)
	}
	if len(missing) > 0 {
		return nil, time.Time{}, missing
	}
	return prior, observed, nil
}

// fill stores newly read complete sessions, drops sessions that fell out of
// the window, and returns the window as assemble would.
func (p *setupProfileCache) fill(key string, window []setups.Session, complete []setupProfileSession) ([]setups.Session, time.Time, []setups.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]*setupProfileRow{}
	}
	row := p.rows[key]
	if row == nil && len(complete) == 0 {
		return p.assembleLocked(key, window)
	}
	if row == nil {
		if len(p.rows) >= 20 {
			for k := range p.rows {
				delete(p.rows, k)
				break
			}
		}
		row = &setupProfileRow{sessions: map[string]setupProfileSession{}}
		p.rows[key] = row
	}
	for _, c := range complete {
		row.sessions[c.session.Date] = c
	}
	inWindow := make(map[string]bool, len(window))
	for _, w := range window {
		inWindow[w.Date] = true
	}
	for date := range row.sessions {
		if !inWindow[date] {
			delete(row.sessions, date)
		}
	}
	return p.assembleLocked(key, window)
}

// setupFetchRanges groups sessions (ordered by open) into reads spanning at
// most seven days, so each range is one paced historical request. A range may
// cover sessions already held; their bars are not re-attached.
func setupFetchRanges(missing []setups.Session) [][2]time.Time {
	var out [][2]time.Time
	for _, m := range missing {
		if n := len(out); n > 0 && !m.Close.After(out[n-1][0].Add(7*24*time.Hour)) {
			out[n-1][1] = m.Close
			continue
		}
		out = append(out, [2]time.Time{m.Open, m.Close})
	}
	return out
}
