package daemon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// This client negotiates at most protocol 203 (connection.go). IBKR's
// configuration request is a newer protobuf capability, not an existing wire
// request we can safely send. No request, config update, or raw response is
// emitted/retained here. Adding discovery requires a separately tested protocol
// upgrade; an operator declaration must never masquerade as an API observation.
const restartDiscoveryUnsupported = "unsupported_client_protocol"

type gatewayRestartSchedule struct {
	clock, zone  string
	hour, minute int
	loc          *time.Location
	grace        time.Duration
	loadedAt     time.Time
	problem      string
}

func parseGatewayRestart(g config.Gateway, loadedAt time.Time) (gatewayRestartSchedule, error) {
	s := gatewayRestartSchedule{clock: g.RestartTime, zone: g.RestartTimezone, grace: 5 * time.Minute, loadedAt: loadedAt}
	if s.clock == "" {
		if s.zone != "" || g.RestartGrace != "" {
			return s, fmt.Errorf("restart_time is required when a restart timezone or grace is configured")
		}
		return s, nil
	}
	t, err := time.Parse("15:04", s.clock)
	if err != nil || t.Format("15:04") != s.clock {
		return s, fmt.Errorf("restart_time must use 24-hour HH:MM")
	}
	s.hour, s.minute = t.Hour(), t.Minute()
	if g.RestartGrace != "" {
		s.grace, err = time.ParseDuration(g.RestartGrace)
		if err != nil || s.grace < time.Second || s.grace > time.Hour {
			return s, fmt.Errorf("restart_grace must be between 1s and 1h")
		}
	}
	if s.zone == "" || strings.EqualFold(s.zone, "local") {
		s.problem = "Gateway timezone has not been confirmed; no restart window inferred"
		return s, nil
	}
	if s.zone != "UTC" && !strings.Contains(s.zone, "/") {
		return s, fmt.Errorf("restart_timezone must be an IANA timezone, not an abbreviation")
	}
	s.loc, err = time.LoadLocation(s.zone)
	if err != nil {
		return s, fmt.Errorf("restart_timezone is not a recognized IANA timezone")
	}
	return s, nil
}

type gatewayRestartMonitor struct {
	mu                                          sync.Mutex
	schedule                                    gatewayRestartSchedule
	lastAt                                      time.Time
	outageAt, expectedAt, deadline, recoveredAt time.Time
}

func (s *Server) configureGatewayRestart() {
	g := config.Gateway{}
	if s.cfg != nil {
		g = s.cfg.Gateway
	}
	parsed, err := parseGatewayRestart(g, s.now())
	if err != nil {
		parsed.problem = err.Error()
		parsed.loc = nil
		s.addConfigIssue(config.Issue{Key: "gateway.restart_time/restart_timezone/restart_grace", Problem: err.Error() + "; restart annotation unavailable"})
	}
	s.gatewayRestart.mu.Lock()
	s.gatewayRestart.schedule = parsed
	s.gatewayRestart.mu.Unlock()
}

// occurrence avoids guessing when a local wall-clock time is skipped or
// repeated at a DST transition. In those cases there is no expected window.
func (s gatewayRestartSchedule) occurrence(day time.Time) (time.Time, bool) {
	t := time.Date(day.Year(), day.Month(), day.Day(), s.hour, s.minute, 0, 0, s.loc)
	local := t.In(s.loc)
	if local.Year() != day.Year() || local.Month() != day.Month() || local.Day() != day.Day() || local.Hour() != s.hour || local.Minute() != s.minute {
		return time.Time{}, false
	}
	// IANA transitions need not be an hour (Lord Howe uses thirty minutes).
	for d := time.Minute; d <= 3*time.Hour; d += time.Minute {
		for _, other := range []time.Time{t.Add(-d), t.Add(d)} {
			v := other.In(s.loc)
			if v.Year() == local.Year() && v.YearDay() == local.YearDay() && v.Hour() == s.hour && v.Minute() == s.minute {
				return time.Time{}, false
			}
		}
	}
	return t, true
}

func (s gatewayRestartSchedule) window(now time.Time) (time.Time, time.Time, bool) {
	day := now.In(s.loc)
	for _, candidate := range []time.Time{day, day.AddDate(0, 0, -1)} {
		at, ok := s.occurrence(candidate)
		if ok && !now.Before(at) && now.Before(at.Add(s.grace)) {
			return at, at.Add(s.grace), true
		}
	}
	return time.Time{}, time.Time{}, false
}

func (m *gatewayRestartMonitor) snapshot(now time.Time, h *rpc.HealthResult) *rpc.GatewayRestartHealth {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.schedule
	r := &rpc.GatewayRestartHealth{State: "unknown", Reason: "Restart configuration cannot be read by this client", Source: "unknown", Freshness: "unverified", APIDiscovery: restartDiscoveryUnsupported, TimezoneSource: "unknown", AsOf: now}
	if s.clock == "" && s.problem == "" {
		return r
	}
	r.Source = "operator"
	r.Time = s.clock
	r.Timezone = s.zone
	r.ConfigLoadedAt = s.loadedAt
	if s.loc != nil {
		r.TimezoneSource = "operator"
	}
	if s.problem != "" {
		r.Reason = s.problem
		return r
	}
	if s.loc == nil {
		r.Reason = "Gateway timezone has not been confirmed"
		return r
	}
	// Concurrent status composition can deliver an older sample, as can a
	// backwards clock. Do not let either rewrite the episode or its deadline.
	if !m.lastAt.IsZero() && now.Before(m.lastAt) {
		r.Reason = "Older health sample or clock regression; restart timing is unverified"
		return r
	}
	m.lastAt = now
	localReady := h.Connected && (h.GatewayPhase == rpc.GatewayPhaseReady || h.GatewayPhase == rpc.GatewayPhaseBackendLinkDown)
	if !localReady {
		if m.outageAt.IsZero() || !m.recoveredAt.IsZero() {
			m.outageAt = now
			m.recoveredAt = time.Time{}
			m.expectedAt, m.deadline, _ = s.window(now)
		}
		r.State = "unexpected_outage"
		r.Reason = "Local API is unavailable; cause is unknown"
		if !m.expectedAt.IsZero() {
			r.State = "expected_restart"
			r.Reason = "Outage observed within the declared restart window; cause is unverified"
			if !now.Before(m.deadline) {
				r.State = "overrun"
				r.Reason = "Local API recovery exceeded the declared restart window; inspect Gateway"
			}
		}
	} else if !m.outageAt.IsZero() && m.recoveredAt.IsZero() {
		// A live socket or 1102 does not prove data recovery. Reuse the
		// existing authoritative verdict; the annotation never alters it.
		if authoritativeHealthVerdict(h).State == "READY" {
			m.recoveredAt = now
			r.State = "recovered"
			r.Reason = "API and existing data-health checks have recovered"
		} else {
			r.State = "recovering"
			r.Reason = "API is reachable; existing health checks have not recovered"
			if !m.deadline.IsZero() && !now.Before(m.deadline) {
				r.State = "overrun"
				r.Reason = "Recovery window expired; API is reachable but health checks still require attention"
			}
		}
	} else {
		r.State = "scheduled"
		r.Reason = "Operator schedule; Gateway settings have not been verified through the API"
		if !m.recoveredAt.IsZero() {
			r.State = "recovered"
			r.Reason = "Last observed API outage recovered; schedule remains unverified"
		}
	}
	r.OutageObservedAt = m.outageAt
	r.ScheduledAt = m.expectedAt
	r.Deadline = m.deadline
	r.RecoveredAt = m.recoveredAt
	return r
}
