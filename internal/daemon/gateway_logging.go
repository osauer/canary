package daemon

import (
	"time"

	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// logGatewayUnavailable is the single warning owner for failed connection
// attempts and their unavailable-dependent reads. Detailed current evidence
// remains in status; retries remain visible with debug logging.
func (s *Server) logGatewayUnavailable(detail string) {
	s.logGatewayFailure(detail)
}

// logGatewaySessionNotStarted reports, through the same incident, a Gateway
// that accepted the API connection and answered while the session did not
// start. It warns whatever the logging schedule declares: the Gateway is up
// and talking, so no quiet window explains the failure. The incident keeps
// that attention until it recovers, so a failover candidate that fails
// otherwise in the same cycle does not flip it back and re-emit. On
// 2026-10-19 the attempt's own error stayed in the debug log while the
// Gateway answered every attempt.
func (s *Server) logGatewaySessionNotStarted(detail string) {
	s.gatewaySessionNotStarted.Store(true)
	s.logGatewayFailure(detail)
}

func (s *Server) logGatewayFailure(detail string) {
	now := s.gatewayLogClock()
	required := s.gatewaySessionNotStarted.Load() || s.gatewayLogRequired(now, now)
	warn, count, age := s.gatewayLog.ObserveAttention(now, required)
	if s.logger == nil {
		return
	}
	if warn {
		log := s.logger.Warnf
		if !required {
			log = s.logger.Infof
		}
		log("Gateway unavailable: %s (observations=%d, duration=%s; repeated attempts remain in debug logs)", detail, count, age.Round(time.Second))
	} else if s.logger.debugEnabled() {
		s.logger.Debugf("Gateway unavailable: %s", detail)
	}
}

func (s *Server) logGatewayRecovered() {
	now := s.gatewayLogClock()
	count, age, attention := s.gatewayLog.RecoverAttention(now)
	s.gatewaySessionNotStarted.Store(false)
	if count > 0 && s.logger != nil {
		log := s.logger.Warnf
		if !attention && !s.gatewayLogRequired(now.Add(-age), now) {
			log = s.logger.Infof
		}
		log("Gateway connection recovered after %s (%d unavailable observations); dependent data recovery is reported separately", age.Round(time.Second), count)
	}
}

func (s *Server) gatewayLogClock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// logGatewayDependency suppresses duplicate symptoms only during an outage
// another owner reports: the daemon's own transport incident, a dial whose
// outcome is still pending, a set-up session that has ended and whose
// reconnect opens that incident (reconnectFlow), or the connector's
// TWS-to-IBKR backend-link loss, which the connector announces once (1100)
// and whose restore it reports itself (1101/1102). A pending dial reports its
// own failure through logGatewayUnavailable; without it, the chart reads of
// each daemon start warned "IBKR connection unavailable" in the instant
// before the first connect (24 lines over seven restarts on 2026-10-05). The
// ended session covers the reads cut by the loss before anything redials:
// two history warnings 51 ms after TWS's restart on 2026-10-07. The backend
// link counts for ibkrlib.BackendRestoreSettle after its restore, the flush
// in which TWS answers the requests the loss cut. It never infers an outage
// from arbitrary text.
//
// Owner instruction (Oliver, in chat, 2026-10-13 09:04 CEST): dependents of
// an owned outage log through the owner.
func (s *Server) logGatewayDependency(detail string) bool {
	switch {
	case s.gatewayLog.Join():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Gateway dependency unavailable: %s", detail)
		}
	case s.gatewayDialPending():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Gateway dependency awaiting dial: %s", detail)
		}
	case s.gatewaySessionEnded():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Gateway dependency awaiting redial: %s", detail)
		}
	case s.backendLinkUnsettled():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Backend link dependency unavailable: %s", detail)
		}
	default:
		return false
	}
	return true
}

// gatewayDialPending reports a connect or reconnect attempt in flight.
func (s *Server) gatewayDialPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectInFlight
}

// gatewaySessionEnded reports a published connector whose set-up session has
// ended and not yet been replaced: the state reconnectFlow turns into the
// incident. It reads the connector without gatewayConnector's reconnect side
// effect.
func (s *Server) gatewaySessionEnded() bool {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c != nil && s.postConnectSetupDone.Load() && !c.IsReady()
}

// gatewaySessionReady reports a published connector with a live API session,
// read without gatewayConnector's reconnect side effect.
func (s *Server) gatewaySessionReady() bool {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c != nil && c.IsReady()
}

// historicalBarFarmOutage reads the current connector's announced bar-farm
// outage without the reconnect side effect of gatewayConnector.
func (s *Server) historicalBarFarmOutage() bool {
	if s.historicalFarmOutage != nil {
		return s.historicalFarmOutage()
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c.HistoricalBarFarmOutageAnnounced()
}

// backendLinkUnsettled reads the current connector's backend-link latch
// without the reconnect side effect of gatewayConnector: down, or restored
// less than ibkrlib.BackendRestoreSettle ago.
func (s *Server) backendLinkUnsettled() bool {
	var link ibkrlib.BackendLinkReport
	if s.backendLink != nil {
		link = s.backendLink()
	} else {
		s.mu.Lock()
		c := s.connector
		s.mu.Unlock()
		link = c.BackendLink()
	}
	if link.Down {
		return true
	}
	return !link.ChangedAt.IsZero() && s.gatewayLogClock().Sub(link.ChangedAt) < ibkrlib.BackendRestoreSettle
}
