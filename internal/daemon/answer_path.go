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
		}
	}
}

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
