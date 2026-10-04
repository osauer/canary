package daemon

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/setups"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// setupCurrentBars is the latest current-session read of one contract. A
// live evaluation reuses it until the next five-minute bar should have
// completed, and then reproduces the evaluation at the read's own decision
// and acquisition clocks: an evaluation on retained bars never claims a
// fresher observation than the read. Replays (--at) neither read nor feed it.
type setupCurrentBars struct {
	binding  ibkrlib.HistoricalSessionBinding
	session  setups.Session
	at       time.Time // decision clock that bounded the read
	acquired time.Time // when the read completed
	next     time.Time // latest completed bar end plus five minutes
	used     uint64
}

// setupCurrentFlight is one outstanding current-session read. Other live
// evaluations of the contract wait for it instead of repeating it; they share
// its failure unless the reader's own caller cancelled it.
type setupCurrentFlight struct {
	done   chan struct{}
	err    error
	shared bool
}

// setupCurrentLimit bounds retained current-session reads, one per contract.
const setupCurrentLimit = setupProfileRowsMax

// servable reports whether a live evaluation at now may reuse m: same broker
// session and market session, no new bar due yet, and a read no older than
// the baseline it is paired with.
func (m *setupCurrentBars) servable(binding ibkrlib.HistoricalSessionBinding, session string, now, baseline time.Time) bool {
	return m != nil && m.binding == binding && m.session.Date == session && !now.Before(m.at) && now.Before(m.next) && !m.acquired.Before(baseline)
}

func newSetupCurrentBars(binding ibkrlib.HistoricalSessionBinding, current setups.Session, bars []ibkrlib.HistoricalBar, at, acquired time.Time) setupCurrentBars {
	session := attachSetupBars([]setups.Session{current}, bars)[0]
	last := current.Open
	for _, b := range session.Bars {
		if !b.End.After(at) && b.End.After(last) {
			last = b.End
		}
	}
	return setupCurrentBars{binding: binding, session: session, at: at, acquired: acquired, next: last.Add(5 * time.Minute)}
}

// liveCurrentBars returns the current session's bars for a live evaluation
// decided at at, reading at most once per contract per completed bar. read
// reports whether this call performed the broker read.
func (s *Server) liveCurrentBars(ctx context.Context, c setupBarSource, binding ibkrlib.HistoricalSessionBinding, contract ibkrlib.Contract, key string, current setups.Session, at, baseline time.Time) (bars setupCurrentBars, read bool, err error) {
	p := &s.setupProfiles
	for {
		now := s.setupClock()
		p.mu.Lock()
		if m := p.current[key]; m.servable(binding, current.Date, now, baseline) {
			p.tick++
			m.used = p.tick
			out := *m
			p.mu.Unlock()
			return out, false, nil
		}
		if f := p.flights[key]; f != nil {
			wait := p.currentWaitForTest
			p.mu.Unlock()
			if wait != nil {
				wait()
			}
			select {
			case <-f.done:
				if f.err != nil && f.shared {
					return setupCurrentBars{}, false, f.err
				}
				continue
			case <-ctx.Done():
				return setupCurrentBars{}, false, ctx.Err()
			}
		}
		f := &setupCurrentFlight{done: make(chan struct{})}
		if p.flights == nil {
			p.flights = map[string]*setupCurrentFlight{}
		}
		p.flights[key] = f
		p.mu.Unlock()

		raw, err := c.FetchSetupBars(ctx, contract, current.Open, at, 20*time.Second)
		if err == nil {
			bars = newSetupCurrentBars(binding, current, raw, at, s.setupClock())
		}
		p.mu.Lock()
		delete(p.flights, key)
		if err == nil {
			p.storeCurrentLocked(key, bars)
		}
		f.err, f.shared = err, err != nil && ctx.Err() == nil
		close(f.done)
		p.mu.Unlock()
		return bars, true, err
	}
}

// storeCurrentLocked keeps m for key. Reads of earlier sessions are dropped;
// at the bound the least recently used read goes.
func (p *setupProfileCache) storeCurrentLocked(key string, m setupCurrentBars) {
	if p.current == nil {
		p.current = map[string]*setupCurrentBars{}
	}
	for k, old := range p.current {
		if old.session.Date < m.session.Date {
			delete(p.current, k)
		}
	}
	if _, ok := p.current[key]; !ok && len(p.current) >= setupCurrentLimit {
		victim := ""
		for k, old := range p.current {
			if victim == "" || old.used < p.current[victim].used {
				victim = k
			}
		}
		delete(p.current, victim)
	}
	p.tick++
	m.used = p.tick
	p.current[key] = &m
}
