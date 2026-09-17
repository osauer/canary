package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/discover"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Brief broker resets should recover on their existing socket. An Auto
// session that stays down may instead have moved from Gateway to TWS.
const autoBackendRediscoveryDelay = 30 * time.Second

func autoBackendRediscoveryDue(pinned bool, link ibkrlib.BackendLinkReport, now time.Time) bool {
	return !pinned && link.Down && !link.ChangedAt.IsZero() && now.Sub(link.ChangedAt) >= autoBackendRediscoveryDelay
}

func preferAlternateEndpoint(ep discover.Endpoint, failedPort int) (discover.Endpoint, bool) {
	if ep.PortOrigin != discover.OriginDiscovered || ep.Port == 0 {
		return ep, false
	}
	if ep.Port != failedPort {
		return ep, true
	}
	if len(ep.Alternates) == 0 {
		return ep, false
	}
	ep.Port, ep.Alternates = ep.Alternates[0], append(append([]int(nil), ep.Alternates[1:]...), failedPort)
	return ep, true
}
