package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/pkg/futu/futusim"
)

// The session helpers read the session seam, so they answer for OpenD from
// OpenD's own session: its account is the connected one, a healthy session is
// neither ended nor unsettled, and a ready OpenD is still not ready for
// trading, because orders leave through IBKR only.
func TestSessionHelpersReadTheSeamUnderOpenD(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)

	if health, held := srv.sessionHealth(); !held || !health.Ready {
		t.Fatalf("session health = %+v, held %v", health, held)
	}
	if got := srv.connectedGatewayAccount(); got != opendTestAccount {
		t.Errorf("connected account = %q, want %q", got, opendTestAccount)
	}
	if srv.tradingGatewayReady() {
		t.Error("a ready OpenD session read as ready for trading")
	}
	if srv.gatewaySessionEnded() || srv.backendLinkUnsettled() || srv.historicalBarFarmOutage() {
		t.Error("a healthy OpenD session read as ended, unsettled or in a history outage")
	}
}

// With no connector published there is no session to ask, and every helper
// answers its quiet value without arming a reconnect.
func TestSessionHelpersAnswerQuietlyWithNoSession(t *testing.T) {
	srv := &Server{}
	if _, held := srv.sessionHealth(); held {
		t.Fatal("a server with no connector holds a session")
	}
	if srv.connectedGatewayAccount() != "" || srv.gatewaySessionEnded() || srv.backendLinkUnsettled() || srv.historicalBarFarmOutage() {
		t.Error("a helper answered for a session that does not exist")
	}
}
