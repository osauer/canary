package daemon

import "time"

// logGatewayUnavailable is the single warning owner for failed connection
// attempts and their unavailable-dependent reads. Detailed current evidence
// remains in status; retries remain visible with debug logging.
func (s *Server) logGatewayUnavailable(detail string) {
	warn, count, age := s.gatewayLog.Observe(s.gatewayLogClock())
	if s.logger == nil {
		return
	}
	if warn {
		s.logger.Warnf("Gateway unavailable: %s (observations=%d, duration=%s; repeated attempts remain in debug logs)", detail, count, age.Round(time.Second))
	} else if s.logger.debugEnabled() {
		s.logger.Debugf("Gateway unavailable: %s", detail)
	}
}

func (s *Server) logGatewayRecovered() {
	count, age := s.gatewayLog.Recover(s.gatewayLogClock())
	if count > 0 && s.logger != nil {
		s.logger.Warnf("Gateway connection recovered after %s (%d unavailable observations); dependent data recovery is reported separately", age.Round(time.Second), count)
	}
}

func (s *Server) gatewayLogClock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// logGatewayDependency suppresses duplicate symptoms only during an already
// reported transport outage. It never infers an outage from arbitrary text.
func (s *Server) logGatewayDependency(detail string) bool {
	if !s.gatewayLog.Join() {
		return false
	}
	if s.logger != nil && s.logger.debugEnabled() {
		s.logger.Debugf("Gateway dependency unavailable: %s", detail)
	}
	return true
}
