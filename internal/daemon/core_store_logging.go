package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/logepisode"
)

// authorityIncident is the single warning owner for a fail-closed authority
// store. The store reports its Ready-to-blocked transition and its proof
// recovery through corestore.Options.HealthObserver; the incident announces
// the latch once with its code and cause, closes it with a bookend carrying
// the duration and the dependent failure count, and lets dependents that only
// see corestore.ErrBlocked join at debug through the Logger hook. A dependent
// can never open the incident. Status and health RPC surfaces keep reading the
// store's Health directly and are unaffected by this cadence.
type authorityIncident struct {
	server *Server
	state  logepisode.State
}

// armAuthorityIncident creates the incident for the live store about to be
// opened and points the daemon logger's blocked-store hook at it. Each live
// open starts with no incident; the most recent arm wins, which matches the
// single live Store the Server holds.
func (s *Server) armAuthorityIncident() *authorityIncident {
	incident := &authorityIncident{server: s}
	if s != nil {
		s.logger.SetBlockedIncidentHook(incident.join)
	}
	return incident
}

func (a *authorityIncident) join() bool { return a.state.Join() }

// observe is the corestore health observer. The store calls it outside its
// locks, once per transition, with the error the failing operation returned.
func (a *authorityIncident) observe(health corestore.Health, cause error) {
	now := a.server.gatewayLogClock()
	if health.Ready {
		a.recovered(now)
		return
	}
	warn, _, _ := a.state.Observe(now)
	if !warn || a.server.logger == nil {
		return
	}
	recovery := "restart required"
	if health.RecoveryEligible {
		recovery = "recovery-eligible: transient proof retries every " + coreStoreRecoveryPollInterval.String()
	}
	a.server.logger.Warnf(
		"daemon authority: persistence latched fail-closed (%s): %v; broker writes are blocked; %s; dependent persistence failures continue in debug logs",
		health.Code, cause, recovery,
	)
}

// recovered closes the incident. A zero count means none was open, so a
// Ready report without a preceding latch never claims a recovery.
func (a *authorityIncident) recovered(now time.Time) {
	count, age := a.state.Recover(now)
	if count == 0 || a.server.logger == nil {
		return
	}
	a.server.logger.Warnf(
		"daemon authority: persistence recovered after %s (%d dependent failures while blocked)",
		age.Round(time.Second), count-1,
	)
}
