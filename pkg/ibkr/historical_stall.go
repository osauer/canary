package ibkr

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrHistoricalServiceStalled reports a history request refused before the
// wire because the Gateway has stopped answering history on this session.
var ErrHistoricalServiceStalled = errors.New("the Gateway is not answering historical data requests")

// Stall thresholds, set by the owner on 2026-09-28.
const (
	// historicalStallRequests unanswered requests spanning at least
	// historicalStallSpan from the first one's send declare a stall.
	historicalStallRequests = 3
	historicalStallSpan     = time.Minute
	// historicalStallMinWait is the shortest wait that counts: a tighter
	// caller budget proves nothing about the Gateway.
	historicalStallMinWait = 10 * time.Second
	// historicalStallProbeEvery admits one request while stalled.
	historicalStallProbeEvery = 5 * time.Minute
)

// historicalStall tracks whether the Gateway still answers history. On
// 2026-09-28 the Mini's Gateway accepted every history request from 06:31
// and answered none; each cancel drew "query cancelled", so it held the
// queries. Every chart series kept sending its own doomed request, about 130
// an hour, and nothing said the Gateway needed a restart. Earlier episodes
// on this Mac's TWS ended only when TWS itself restarted.
type historicalStall struct {
	mu         sync.Mutex
	session    HistoricalSessionBinding
	unanswered int
	firstSent  time.Time
	since      time.Time // zero unless stalled
	probeAt    time.Time
}

// resetForSessionLocked starts over on a new broker session: a reconnected
// Gateway earns fresh requests, and a stall still there is declared anew.
func (s *historicalStall) resetForSessionLocked(session HistoricalSessionBinding) {
	if s.session == session {
		return
	}
	s.session, s.unanswered, s.firstSent, s.since, s.probeAt = session, 0, time.Time{}, time.Time{}, time.Time{}
}

// A queued result can outlive teardown of the request that produced it.
// Compare the exact connection as well as its socket epoch before publishing
// that result into the current session's stall/admission state. Callers hold
// historicalStall.mu, so an old completion cannot replace a newer verdict.
func (c *Connector) historicalRequestSessionCurrent(session HistoricalSessionBinding) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return session.connector == c && session.connection != nil && c.conn == session.connection && session.connection.BrokerSessionEpoch() == session.epoch
}

// admitHistoricalRequest refuses history while stalled, except for one probe
// per historicalStallProbeEvery.
func (c *Connector) admitHistoricalRequest(session HistoricalSessionBinding, now time.Time) error {
	s := &c.historicalStall
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.historicalRequestSessionCurrent(session) {
		return errHistorySessionEnded
	}
	s.resetForSessionLocked(session)
	if s.since.IsZero() {
		return nil
	}
	if now.Sub(s.probeAt) >= historicalStallProbeEvery {
		s.probeAt = now
		return nil
	}
	return fmt.Errorf("%w since %s; next probe after %s", ErrHistoricalServiceStalled,
		s.since.Format(time.TimeOnly), s.probeAt.Add(historicalStallProbeEvery).Format(time.TimeOnly))
}

// noteHistoricalAnswer records any broker answer to a history request: bars,
// an end marker, or a broker error such as a pacing refusal.
func (c *Connector) noteHistoricalAnswer(session HistoricalSessionBinding, now time.Time) {
	s := &c.historicalStall
	s.mu.Lock()
	if !c.historicalRequestSessionCurrent(session) {
		s.mu.Unlock()
		return
	}
	s.resetForSessionLocked(session)
	since := s.since
	s.unanswered, s.firstSent, s.since, s.probeAt = 0, time.Time{}, time.Time{}, time.Time{}
	s.mu.Unlock()
	c.answers.noteAnswer(now)
	if !since.IsZero() {
		c.logWarn("IBKR Gateway answers historical data requests again after %s", now.Sub(since).Round(time.Second))
	}
}

// noteHistoricalTimeout records a sent history request that waited out its
// budget without any answer.
func (c *Connector) noteHistoricalTimeout(session HistoricalSessionBinding, waited time.Duration, now time.Time) {
	s := &c.historicalStall
	s.mu.Lock()
	if !c.historicalRequestSessionCurrent(session) {
		s.mu.Unlock()
		return
	}
	if waited < historicalStallMinWait {
		s.mu.Unlock()
		c.answers.noteTimeout(now)
		return
	}
	s.resetForSessionLocked(session)
	if s.unanswered == 0 {
		s.firstSent = now.Add(-waited)
	}
	s.unanswered++
	declare := s.since.IsZero() && s.unanswered >= historicalStallRequests && now.Sub(s.firstSent) >= historicalStallSpan
	if declare {
		s.since, s.probeAt = now, now
	}
	count, span := s.unanswered, now.Sub(s.firstSent)
	s.mu.Unlock()
	c.answers.noteTimeout(now)
	if declare {
		c.logWarn("IBKR Gateway has answered none of the last %d historical data requests over %s; holding further history requests except one probe every %s until it answers, and serving recorded history meanwhile (restarting the Gateway has cleared this before)",
			count, span.Round(time.Second), historicalStallProbeEvery)
	}
}

// HistoricalServiceStalled reports whether the Gateway has stopped answering
// history on the current broker session, and since when.
func (c *Connector) HistoricalServiceStalled() (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	s := &c.historicalStall
	s.mu.Lock()
	defer s.mu.Unlock()
	if !c.historicalRequestSessionCurrent(s.session) || s.since.IsZero() {
		return time.Time{}, false
	}
	return s.since, true
}

// historicalAnswered reports whether a history result came from the broker:
// bars or an end marker (nil), a coded broker error, or a payload that failed
// validation. Connector-authored failures, such as a changed session, do not.
func historicalAnswered(err error) bool {
	if err == nil {
		return true
	}
	if hErr, ok := errors.AsType[*HistoricalRequestError](err); ok {
		return hErr.Code != 0
	}
	_, ok := errors.AsType[*HistoricalDataValidationError](err)
	return ok
}
