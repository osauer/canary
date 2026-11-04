package daemon

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

// An OpenD daemon takes no broker write in this build, whatever its trading mode
// and whatever authority stands: a paper-mode daemon on a simulate account clears
// every other check, so this refusal alone must stop each origin, the daemon's own
// automatic and protective sends included.
func TestOpenDDaemonAuthorisesNoBrokerWrite(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServerInMode(t, sim, opendTestAccount, config.TradingModePaper)
	defer stop()
	waitOpenDReady(t, srv)

	status := srv.handleTradingStatus()
	if status.Blocked {
		t.Fatalf("the test needs a daemon that clears the other checks: %+v", status.Blockers)
	}
	if status.CanWrite {
		t.Fatal("trading status offers writes under OpenD")
	}
	for _, origin := range []string{
		rpc.OrderOriginAgent, rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice,
		rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginDaemonOwnerQueued,
		rpc.OrderOriginDaemonProtectiveGuard, rpc.OrderOriginDeskAutomatic,
	} {
		if auth := srv.brokerWriteAuthorizationForRequest(origin); auth.Allowed || !hasBlocker(auth.Blockers, brokerWritesNotServedCode) {
			t.Errorf("%s: authorised %v, blockers %+v", origin, auth.Allowed, auth.Blockers)
		}
		for _, cancel := range []bool{false, true} {
			if auth, _, _ := srv.authorizeBrokerWriteTransaction(origin, cancel); auth.Allowed || !hasBlocker(auth.Blockers, brokerWritesNotServedCode) {
				t.Errorf("%s cancel=%v: authorised %v, blockers %+v", origin, cancel, auth.Allowed, auth.Blockers)
			}
		}
	}
}

// The client has no way to send one: no method on its connector places, changes or
// cancels an order or unlocks trading. Phase D adds them on purpose and changes this.
func TestFutuConnectorHasNoWriteMethod(t *testing.T) {
	connector := reflect.TypeFor[*futu.Connector]()
	if connector.NumMethod() == 0 {
		t.Fatal("no methods to check")
	}
	for method := range connector.Methods() {
		name := method.Name
		if slices.ContainsFunc([]string{"place", "modify", "cancel", "unlock"}, func(verb string) bool {
			return strings.Contains(strings.ToLower(name), verb)
		}) {
			t.Errorf("the connector gained a write: %s", name)
		}
	}
}
