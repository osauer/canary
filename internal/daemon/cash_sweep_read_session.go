package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// cashSweepReadSession publishes only the original read's binding. It must
// never attach a newly sampled socket generation to an earlier cash receipt.
func (s *Server) cashSweepReadSession(c *ibkrlib.Connector, origin ibkrlib.ConnectorSessionBinding) rpc.BrokerReadSession {
	if s == nil || c == nil || origin.Epoch() == 0 || !c.SessionCurrent(origin) {
		return rpc.BrokerReadSession{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connector != c || s.connectorEpoch == 0 || s.startedAt.IsZero() {
		return rpc.BrokerReadSession{}
	}
	return rpc.BrokerReadSession{DaemonStartedAt: s.startedAt.UTC(), ConnectorGeneration: s.connectorEpoch, SocketEpoch: origin.Epoch()}
}

func (s *Server) cashSweepCommonReadSession(acct *rpc.AccountResult, pos *rpc.PositionsResult) bool {
	if s == nil || acct == nil || pos == nil || acct.Authority == nil || pos.Authority == nil {
		return false
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	if c == nil {
		return false
	}
	current, ok := c.CaptureSession()
	return ok && cashSweepReadSessionsMatch(acct.Authority.BrokerReadSession, pos.Authority.BrokerReadSession, s.cashSweepReadSession(c, current))
}

func cashSweepReadSessionsMatch(account, positions, current rpc.BrokerReadSession) bool {
	return !account.DaemonStartedAt.IsZero() && account.ConnectorGeneration != 0 && account.SocketEpoch != 0 && account == positions && account == current
}
