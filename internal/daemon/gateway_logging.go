package daemon

import "time"

// logGatewayUnavailable is the single warning owner for failed connection
// attempts and their unavailable-dependent reads. Detailed current evidence
// remains in status; retries remain visible with debug logging.
func (s *Server) logGatewayUnavailable(detail string) {
	now := s.gatewayLogClock()
	required := s.gatewayLogRequired(now, now)
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

// logGatewayDependency suppresses duplicate symptoms only during an already
// reported outage: the daemon's own transport incident, or the connector's
// TWS-to-IBKR backend-link loss, which the connector announces once (1100)
// and whose restore it reports itself (1101/1102). It never infers an outage
// from arbitrary text.
func (s *Server) logGatewayDependency(detail string) bool {
	switch {
	case s.gatewayLog.Join():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Gateway dependency unavailable: %s", detail)
		}
	case s.backendLinkDown():
		if s.logger != nil && s.logger.debugEnabled() {
			s.logger.Debugf("Backend link dependency unavailable: %s", detail)
		}
	default:
		return false
	}
	return true
}

// backendLinkDown reads the current connector's backend-link latch without
// the reconnect side effect of gatewayConnector.
func (s *Server) backendLinkDown() bool {
	if s.backendLink != nil {
		return s.backendLink().Down
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c != nil && c.BackendLink().Down
}
