package ibkr

import (
	"errors"
	"sync"
	"time"
)

// answerTimeoutWindow is how far back AnswerPath counts request timeouts.
const answerTimeoutWindow = 15 * time.Minute

// answerTimeoutsKept bounds the timeout clocks one connector remembers.
const answerTimeoutsKept = 256

// AnswerPath is one connection's request-answer health: whether the Gateway
// answers what this connection asks, which a connected socket alone does not
// establish. It is a diagnostic snapshot; the history stall it reports is the
// one admitHistoricalRequest enforces.
type AnswerPath struct {
	ClientID int
	// LastInboundAt is the newest frame of any kind on the current socket.
	LastInboundAt time.Time
	// InFlight counts history and contract-details requests awaiting an
	// answer; OldestSentAt is when the oldest of them was sent.
	InFlight     int
	OldestSentAt time.Time
	// LastAnswerAt is the newest answer to a history or contract-details
	// request: data, an end marker or a coded broker refusal.
	LastAnswerAt time.Time
	// Timeouts counts requests of either kind that waited out their budget
	// without an answer during the last 15 minutes.
	Timeouts int
	// LimiterQueue is the pacing queue's current depth.
	LimiterQueue int
	// HistoryStalledSince is set while the Gateway holds history requests
	// unanswered on this session (see historical_stall.go).
	HistoryStalledSince time.Time
}

// answerClock keeps the answer and timeout clocks behind AnswerPath.
type answerClock struct {
	mu       sync.Mutex
	answered time.Time
	timeouts []time.Time
}

func (a *answerClock) noteAnswer(now time.Time) {
	a.mu.Lock()
	if now.After(a.answered) {
		a.answered = now
	}
	a.mu.Unlock()
}

func (a *answerClock) noteTimeout(now time.Time) {
	a.mu.Lock()
	a.timeouts = append(a.timeouts, now)
	if over := len(a.timeouts) - answerTimeoutsKept; over > 0 {
		a.timeouts = append(a.timeouts[:0], a.timeouts[over:]...)
	}
	a.mu.Unlock()
}

func (a *answerClock) read(now time.Time) (time.Time, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	count := 0
	for _, at := range a.timeouts {
		if now.Sub(at) <= answerTimeoutWindow {
			count++
		}
	}
	return a.answered, count
}

// noteContractDetailsOutcome records how one contract-details wait ended: an
// end marker or a coded refusal is an answer, a spent budget a timeout.
func (c *Connector) noteContractDetailsOutcome(err error, now time.Time) {
	switch {
	case errors.Is(err, ErrContractDetailsTimeout):
		c.answers.noteTimeout(now)
	default:
		c.answers.noteAnswer(now)
	}
}

// AnswerPath reports this connector's current request-answer health.
func (c *Connector) AnswerPath(now time.Time) AnswerPath {
	if c == nil {
		return AnswerPath{}
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	out := AnswerPath{}
	if conn != nil {
		out.ClientID = conn.config.ClientID
		if nano := conn.lastHeartbeatNano.Load(); nano > 0 {
			out.LastInboundAt = time.Unix(0, nano)
		}
		if conn.rateLimiter != nil {
			out.LimiterQueue = conn.rateLimiter.GetMetrics().CurrentQueueDepth
		}
	}
	oldest := func(at time.Time) {
		out.InFlight++
		if !at.IsZero() && (out.OldestSentAt.IsZero() || at.Before(out.OldestSentAt)) {
			out.OldestSentAt = at
		}
	}
	c.historicalMu.Lock()
	for _, req := range c.historicalReqs {
		oldest(req.sentAt)
	}
	c.historicalMu.Unlock()
	c.contractDetailsMu.Lock()
	for _, req := range c.contractDetailsReqs {
		oldest(req.sentAt)
	}
	c.contractDetailsMu.Unlock()
	out.LastAnswerAt, out.Timeouts = c.answers.read(now)
	if since, stalled := c.HistoricalServiceStalled(); stalled {
		out.HistoryStalledSince = since
	}
	return out
}

// DropSession ends the current broker session so that its owner redials it.
// It is for a session that is connected but no longer answering: requests in
// flight fail with the session change and nothing is resent. It reports
// whether a connected session was dropped.
func (c *Connector) DropSession(reason error) bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return false
	}
	conn.handleDisconnection(reason)
	return true
}

// SessionLossCause returns why the Connector's broker session ended: the
// transport error (EOF, reset), a heartbeat timeout, or the reason its owner
// passed to DropSession, unchanged so errors.Is works on it. It is nil while
// the session is connected. With ManagedConnectionLogging the loss itself
// logs at debug, and the owner reports it with this cause.
func (c *Connector) SessionLossCause() error {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || conn.IsConnected() {
		return nil
	}
	conn.statusMu.RLock()
	defer conn.statusMu.RUnlock()
	return conn.lastError
}
