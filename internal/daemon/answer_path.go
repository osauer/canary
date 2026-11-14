package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A lane whose Gateway has held history unanswered for answerPathRedialAfter
// is dropped so the daemon redials it (owner decision #22, 2026-09-28:
// detect, notify, and redial after ten minutes). A redial that did not clear
// the stall is not repeated inside answerPathRedialQuiet: the Gateway then
// needs a restart, which the stall's own log line says.
const (
	answerPathRedialAfter = 10 * time.Minute
	answerPathRedialQuiet = time.Hour
	answerPathCheckEvery  = 30 * time.Second
)

// answerPathLane is the supervisor's memory of one lane across sessions.
type answerPathLane struct {
	redialedAt time.Time
	// quietWarned records that a stall outlasting a recent redial was logged.
	quietWarned bool
	// snapshotRedialedAt is the last redial for a poisoned open-order
	// snapshot; snapshotQuietWarned that a poison outlasting it was logged;
	// snapshotPoisonedAt the poisoning that redial answered, while the
	// recovery's outcome is still to be said.
	snapshotRedialedAt  time.Time
	snapshotQuietWarned bool
	// snapshotOutageSaid records that a poison during a lost TWS-to-IBKR
	// link was said.
	snapshotOutageSaid bool
	snapshotPoisonedAt time.Time
	snapshotDropped    *ibkrlib.Connector
}

type answerPathSupervisor struct {
	mu    sync.Mutex
	lanes map[string]*answerPathLane
}

func (a *answerPathSupervisor) lane(name string) *answerPathLane {
	if a.lanes == nil {
		a.lanes = map[string]*answerPathLane{}
	}
	if a.lanes[name] == nil {
		a.lanes[name] = &answerPathLane{}
	}
	return a.lanes[name]
}

// answerPathRedial decides for one lane whether a stall that began at since
// is due a redial now, and whether a stall that outlasted a recent redial
// should be reported once.
func answerPathRedial(lane *answerPathLane, since time.Time, stalled bool, now time.Time) (redial, warnQuiet bool) {
	if !stalled {
		lane.quietWarned = false
		return false, false
	}
	if now.Sub(since) < answerPathRedialAfter {
		return false, false
	}
	if !lane.redialedAt.IsZero() && now.Sub(lane.redialedAt) < answerPathRedialQuiet {
		warn := !lane.quietWarned
		lane.quietWarned = true
		return false, warn
	}
	lane.redialedAt, lane.quietWarned = now, false
	return true, false
}

// answerPathConnectors returns the lanes' current connectors.
func (s *Server) answerPathConnectors() (primary, breadth *ibkrlib.Connector) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connector, s.breadthConnector
}

// answerPaths projects both lanes for status.health.
func (s *Server) answerPaths(now time.Time) []rpc.ConnectionAnswerPath {
	primary, breadth := s.answerPathConnectors()
	out := make([]rpc.ConnectionAnswerPath, 0, 2)
	for _, lane := range []struct {
		name string
		c    *ibkrlib.Connector
	}{{rpc.AnswerPathLanePrimary, primary}, {rpc.AnswerPathLaneBreadth, breadth}} {
		if lane.name == rpc.AnswerPathLaneBreadth && lane.c == nil {
			continue
		}
		out = append(out, s.answerPathRow(lane.name, lane.c, now))
	}
	return out
}

func (s *Server) answerPathRow(name string, c *ibkrlib.Connector, now time.Time) rpc.ConnectionAnswerPath {
	row := rpc.ConnectionAnswerPath{Lane: name, State: rpc.AnswerPathNoSession}
	s.answerPath.mu.Lock()
	row.RedialedAt = s.answerPath.lane(name).redialedAt
	s.answerPath.mu.Unlock()
	if c == nil || !c.IsReady() {
		return row
	}
	ap := c.AnswerPath(now)
	row.ClientID, row.LastInboundAt, row.InFlight, row.OldestSentAt = ap.ClientID, ap.LastInboundAt, ap.InFlight, ap.OldestSentAt
	row.LastAnswerAt, row.Timeouts15m, row.LimiterQueue = ap.LastAnswerAt, ap.Timeouts, ap.LimiterQueue
	row.State = rpc.AnswerPathAnswering
	if !ap.HistoryStalledSince.IsZero() {
		row.State, row.StalledSince = rpc.AnswerPathStalled, ap.HistoryStalledSince
		row.RedialDue = ap.HistoryStalledSince.Add(answerPathRedialAfter)
		if !row.RedialedAt.IsZero() && now.Sub(row.RedialedAt) < answerPathRedialQuiet {
			row.RedialDue = time.Time{}
		}
	}
	return row
}

// runAnswerPathSupervisor checks both lanes every answerPathCheckEvery.
func (s *Server) runAnswerPathSupervisor(ctx context.Context) {
	ticker := time.NewTicker(answerPathCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.superviseAnswerPaths(s.orderNow())
		}
	}
}

// superviseAnswerPaths redials a lane whose history stall is due. The primary
// lane is dropped only while no broker write holds brokerWriteMu; a write that
// starts after the drop sees the session change and fails closed.
func (s *Server) superviseAnswerPaths(now time.Time) {
	primary, breadth := s.answerPathConnectors()
	for _, lane := range []struct {
		name string
		c    *ibkrlib.Connector
	}{{rpc.AnswerPathLanePrimary, primary}, {rpc.AnswerPathLaneBreadth, breadth}} {
		if lane.c == nil || !lane.c.IsReady() {
			continue
		}
		since, stalled := lane.c.HistoricalServiceStalled()
		s.answerPath.mu.Lock()
		redial, warnQuiet := answerPathRedial(s.answerPath.lane(lane.name), since, stalled, now)
		s.answerPath.mu.Unlock()
		if warnQuiet {
			s.warnf("IBKR Gateway still answers no historical data on the %s connection since %s, although it was redialled within the last hour; the Gateway needs a restart", lane.name, since.Format(time.TimeOnly))
		}
		if redial {
			s.redialStalledLane(lane.name, lane.c, since, now)
			continue
		}
		if lane.name == rpc.AnswerPathLanePrimary {
			s.redialPoisonedOpenOrderSnapshot(lane.c, now)
			s.noteOpenOrderSnapshotRecovered(lane.c, now)
		}
	}
}

// redialPoisonedOpenOrderSnapshot drops the primary session whose open-order
// snapshot is poisoned, so open orders are read again on a new socket.
// reqAllOpenOrders has no request ID, so a snapshot that missed its
// openOrderEnd, or whose send may have been written, poisons its socket
// generation and nothing on that socket clears it. On 2026-10-20 a late
// answer during IBKR's nightly reset left the order reconcile and the
// protection alerts without open orders from before 06:56 CEST until a manual
// restart at 08:25 CEST. The poison never clears by itself, so the redial
// does not wait; like a history redial (owner decision #22) it happens at
// most once per answerPathRedialQuiet, says once when a redialled session is
// poisoned again, never drops the session under brokerWriteMu, and logs
// through the logging schedule.
//
// While TWS itself has no connection to IBKR (code 1100) it answers no
// open-order read on any socket, so a redial cannot help: the session is left
// alone and the line says what is true. Once the link is back the read is
// redialled at once, whatever the last redial's age. On 2026-11-09, 10 and 13
// TWS lost that link for hours inside the US session; the one redial was
// spent in the first minute, the log said the Gateway needed a restart, and
// after the link returned the open orders stayed unread for up to an hour,
// with every reduction row blocked on them.
func (s *Server) redialPoisonedOpenOrderSnapshot(c *ibkrlib.Connector, now time.Time) {
	poisonedFn := (*ibkrlib.Connector).OpenOrderSnapshotPoisoned
	if s.snapshotPoisonedFn != nil {
		poisonedFn = s.snapshotPoisonedFn
	}
	since, cause, poisoned := poisonedFn(c)
	if !poisoned {
		return
	}
	logf := s.infof
	if s.gatewayLogRequired(since, now) {
		logf = s.warnf
	}
	linkFn := (*ibkrlib.Connector).BackendLink
	if s.backendLinkFn != nil {
		linkFn = s.backendLinkFn
	}
	link := linkFn(c)
	s.answerPath.mu.Lock()
	lane := s.answerPath.lane(rpc.AnswerPathLanePrimary)
	if c == lane.snapshotDropped {
		// A tick that read the session before the redial dropped it: the
		// redialled connection is a different one, so nothing is said.
		s.answerPath.mu.Unlock()
		return
	}
	if link.Down {
		say := !lane.snapshotOutageSaid
		lane.snapshotOutageSaid = true
		s.answerPath.mu.Unlock()
		if say {
			logf("open orders cannot be read while TWS has no connection to IBKR (lost at %s); Canary reads them again when it returns", localTimeOnly(link.ChangedAt))
		}
		return
	}
	lane.snapshotOutageSaid = false
	// The link came back since the last redial: what poisoned the read has
	// changed, so the hour's quiet does not hold this one.
	restored := !link.ChangedAt.IsZero() && link.ChangedAt.After(lane.snapshotRedialedAt)
	if !restored && !lane.snapshotRedialedAt.IsZero() && now.Sub(lane.snapshotRedialedAt) < answerPathRedialQuiet {
		warn := !lane.snapshotQuietWarned
		lane.snapshotQuietWarned = true
		redialedAt := lane.snapshotRedialedAt
		s.answerPath.mu.Unlock()
		if warn {
			logf("IBKR Gateway's open-order snapshot is poisoned again on the connection redialled at %s; open orders cannot be read until the Gateway restarts", localTimeOnly(redialedAt))
		}
		return
	}
	s.answerPath.mu.Unlock()
	if !s.brokerWriteMu.TryLock() {
		return // a broker write is in progress; the next check tries again
	}
	dropped := c.DropSession(fmt.Errorf("%w: open-order snapshot poisoned since %s", errAnswerPathRedial, since.Format(time.TimeOnly)))
	s.brokerWriteMu.Unlock()
	if !dropped {
		return
	}
	s.answerPath.mu.Lock()
	lane.snapshotRedialedAt, lane.snapshotQuietWarned, lane.snapshotPoisonedAt, lane.snapshotDropped = now, false, since, c
	s.answerPath.mu.Unlock()
	logf("IBKR open-order snapshot poisoned at %s (%v); open orders cannot be read on this connection, so it is redialled", localTimeOnly(since), cause)
	s.triggerReconnect()
}

// noteOpenOrderSnapshotRecovered says once that a snapshot completed on a
// session after a poison redial, closing the record the redial opened. The
// dropped connector never counts, nor a session poisoned again.
func (s *Server) noteOpenOrderSnapshotRecovered(c *ibkrlib.Connector, now time.Time) {
	s.answerPath.mu.Lock()
	defer s.answerPath.mu.Unlock()
	lane := s.answerPath.lane(rpc.AnswerPathLanePrimary)
	poisonedAt, redialedAt := lane.snapshotPoisonedAt, lane.snapshotRedialedAt
	if poisonedAt.IsZero() || c == nil || c == lane.snapshotDropped {
		return
	}
	if _, _, poisoned := c.OpenOrderSnapshotPoisoned(); poisoned {
		return
	}
	completed := c.OpenOrderSnapshotCompletedAt()
	if completed.IsZero() {
		return
	}
	lane.snapshotPoisonedAt, lane.snapshotDropped = time.Time{}, nil
	logf := s.infof
	if s.gatewayLogRequired(poisonedAt, now) {
		logf = s.warnf
	}
	logf("IBKR open-order snapshot complete again at %s on the connection redialled at %s (poisoned at %s)", localTimeOnly(completed), localTimeOnly(redialedAt), localTimeOnly(poisonedAt))
}

// localTimeOnly is the clock time a log line names, on the machine's clock:
// broker receipts are UTC, local stamps are not.
func localTimeOnly(t time.Time) string { return t.Local().Format(time.TimeOnly) }

// errAnswerPathRedial marks a session the supervisor ended on purpose. The
// supervisor has warned already, so the reconnect that follows opens no
// gateway incident (see reconnectFlow).
var errAnswerPathRedial = errors.New("redialled by the daemon")

func (s *Server) redialStalledLane(name string, c *ibkrlib.Connector, since, now time.Time) {
	reason := fmt.Errorf("%w: history unanswered since %s", errAnswerPathRedial, since.Format(time.TimeOnly))
	switch name {
	case rpc.AnswerPathLanePrimary:
		if !s.brokerWriteMu.TryLock() {
			// A broker write is in progress; the next check tries again.
			s.answerPath.mu.Lock()
			s.answerPath.lane(name).redialedAt = time.Time{}
			s.answerPath.mu.Unlock()
			return
		}
		s.warnf("IBKR Gateway has answered no historical data on the %s connection for %s; redialling that connection", name, now.Sub(since).Round(time.Second))
		dropped := c.DropSession(reason)
		s.brokerWriteMu.Unlock()
		if dropped {
			s.triggerReconnect()
		}
	case rpc.AnswerPathLaneBreadth:
		s.warnf("IBKR Gateway has answered no historical data on the %s connection for %s; redialling that connection", name, now.Sub(since).Round(time.Second))
		// breadthConnectFlow stops the stalled connector before it dials.
		s.triggerBreadthConnect()
	}
}
