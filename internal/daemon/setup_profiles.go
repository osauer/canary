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
// The cache holds max(20, distinct contracts evaluated in the live session)
// rows, never more than 40. A new row at that size evicts one row, least
// recently used first among rows whose latest session is not the live one
// (replays and names not yet evaluated today), then among the rest. A replay
// row never evicts a live row; it is served without being kept instead.
//
// mu guards the rows; gate serializes cold acquisition only, so a cached
// profile never waits behind another contract's broker reads.
type setupProfileCache struct {
	mu   sync.Mutex
	gate chan struct{}
	rows map[string]*setupProfileRow
	// misses remembers incomplete or failed baseline reads per contract and
	// session date, so a symbol with a gap in its history is not re-read on
	// every poll.
	misses map[setupMissKey]setupProfileMiss
	// live is the session date of the latest current (non --at) evaluation;
	// today holds the distinct contracts evaluated on it.
	live  string
	today map[string]bool
	// tick orders row use for least-recently-used eviction.
	tick uint64
	// waitForTest observes a request blocking on a busy gate.
	waitForTest func()
}

// Row bounds: at least setupProfileRowsMin rows, growing with the distinct
// contracts evaluated in the live session up to setupProfileRowsMax.
const (
	setupProfileRowsMin = 20
	setupProfileRowsMax = 40
)

// setupProfileRow holds one contract's complete prior sessions by date.
type setupProfileRow struct {
	sessions map[string]setupProfileSession
	// session is the latest session date this row served; used is its last
	// use on the cache tick.
	session string
	used    uint64
}

// setupProfileSession is one complete comparable prior session and the daemon
// time the read that returned its bars completed.
type setupProfileSession struct {
	session  setups.Session
	acquired time.Time
}

// setupProfileMissTTL is how long an incomplete or failed baseline read is
// answered from memory before the next evaluation reads again.
const setupProfileMissTTL = 15 * time.Minute

// setupProfileMissLimit bounds remembered misses; expired ones go first.
const setupProfileMissLimit = 256

type setupMissKey struct{ contract, session string }

// setupProfileMiss is why a baseline window could not be completed, and when.
type setupProfileMiss struct {
	reason string
	at     time.Time
}

// text is the miss as a reason detail naming when the next read may happen,
// on the daemon clock rounded up to the minute.
func (m setupProfileMiss) text() string {
	retry := m.at.Add(setupProfileMissTTL)
	if minute := retry.Truncate(time.Minute); !minute.Equal(retry) {
		retry = minute.Add(time.Minute)
	}
	return m.reason + " (retry after " + retry.Format("15:04") + ")"
}

// miss returns the remembered reason for contract and session while it is
// younger than setupProfileMissTTL.
func (p *setupProfileCache) miss(contract, session string, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.misses[setupMissKey{contract, session}]
	if !ok || !now.Before(m.at.Add(setupProfileMissTTL)) {
		return "", false
	}
	return m.text(), true
}

// rememberMiss records a failed acquisition and returns its reason detail.
func (p *setupProfileCache) rememberMiss(contract, session, reason string, at time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.misses == nil {
		p.misses = map[setupMissKey]setupProfileMiss{}
	}
	for k, m := range p.misses {
		if !at.Before(m.at.Add(setupProfileMissTTL)) {
			delete(p.misses, k)
		}
	}
	key := setupMissKey{contract, session}
	if _, ok := p.misses[key]; !ok && len(p.misses) >= setupProfileMissLimit {
		var oldest setupMissKey
		var oldestAt time.Time
		for k, m := range p.misses {
			if oldestAt.IsZero() || m.at.Before(oldestAt) {
				oldest, oldestAt = k, m.at
			}
		}
		delete(p.misses, oldest)
	}
	m := setupProfileMiss{reason: reason, at: at}
	p.misses[key] = m
	return m.text()
}

func (p *setupProfileCache) clearMiss(contract, session string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.misses, setupMissKey{contract, session})
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
		p.live, p.today = session, nil
	}
	if session == p.live {
		if p.today == nil {
			p.today = map[string]bool{}
		}
		p.today[contract] = true
		return contract
	}
	return contract + "@" + session
}

// capacity is the row bound for the live session.
func (p *setupProfileCache) capacity() int {
	return min(setupProfileRowsMax, max(setupProfileRowsMin, len(p.today)))
}

// admit makes room for a new row serving session and reports whether it may
// be kept. At capacity one row is evicted: the least recently used row not
// serving the live session, else, for a live row only, the least recently
// used row overall.
func (p *setupProfileCache) admit(session string) bool {
	if len(p.rows) < p.capacity() {
		return true
	}
	victim, victimLive := "", false
	var victimUsed uint64
	for k, row := range p.rows {
		live := row.session == p.live
		if victim == "" || (victimLive && !live) || (victimLive == live && row.used < victimUsed) {
			victim, victimLive, victimUsed = k, live, row.used
		}
	}
	if victim == "" || (victimLive && session != p.live) {
		return false
	}
	delete(p.rows, victim)
	return true
}

// assemble returns the window's sessions in order with their oldest
// acquisition, or the window sessions the row does not hold. It marks the
// row used for session.
func (p *setupProfileCache) assemble(key, session string, window []setups.Session) ([]setups.Session, time.Time, []setups.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	row := p.rows[key]
	if row != nil {
		p.touch(row, session)
	}
	return assembleSetupRow(row, window)
}

func (p *setupProfileCache) touch(row *setupProfileRow, session string) {
	p.tick++
	row.used = p.tick
	if session > row.session {
		row.session = session
	}
}

func assembleSetupRow(row *setupProfileRow, window []setups.Session) ([]setups.Session, time.Time, []setups.Session) {
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
// the window, and returns the window as assemble would. A row that may not be
// kept still serves this evaluation.
func (p *setupProfileCache) fill(key, session string, window []setups.Session, complete []setupProfileSession) ([]setups.Session, time.Time, []setups.Session) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rows == nil {
		p.rows = map[string]*setupProfileRow{}
	}
	row := p.rows[key]
	if row == nil {
		row = &setupProfileRow{sessions: map[string]setupProfileSession{}}
		if len(complete) > 0 && p.admit(session) {
			p.rows[key] = row
		}
	}
	p.touch(row, session)
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
	return assembleSetupRow(row, window)
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
