package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The typed evidence behind the phase travels with the status while the
// rejection is the published verdict, and drops out the moment a verdict of
// another kind supersedes it.
func TestPortRejectionEvidenceTravelsWithTheStatus(t *testing.T) {
	port := serveLoopback(t, resetWithoutReading)
	stubIBKRProcess(t, gatewayProcess)
	s := newTestServer(t)
	s.cfg.Gateway.Port = nil
	s.connectWithFailover(t.Context(), discover.Endpoint{Host: "127.0.0.1", Port: port, ClientID: 93, PortOrigin: discover.OriginDiscovered})

	res := s.statusHealthSnapshot()
	want := &rpc.PortRejectionHealth{Host: "127.0.0.1", Port: port, Ended: "reset", App: "IB Gateway", PID: 4242}
	if res.GatewayPhase != rpc.GatewayPhasePortRejecting || !reflect.DeepEqual(res.PortRejection, want) {
		t.Fatalf("phase %q evidence %+v, want %+v", res.GatewayPhase, res.PortRejection, want)
	}

	s.mu.Lock()
	s.lastConnectError = "gateway 127.0.0.1:4001 did not handshake within 25s"
	s.mu.Unlock()
	res = s.statusHealthSnapshot()
	if res.GatewayPhase != rpc.GatewayPhasePortDown || res.PortRejection != nil {
		t.Fatalf("superseded rejection still published: phase %q evidence %+v", res.GatewayPhase, res.PortRejection)
	}

	// An attempt in flight, or a socket that is up, outranks the rejection.
	if got := statusGatewayPhase(false, false, false, true, false, true, "", time.Hour); got != rpc.GatewayPhaseConnecting {
		t.Fatalf("in-flight phase %q", got)
	}
	if got := statusGatewayPhase(true, false, false, false, false, true, "", time.Hour); got != rpc.GatewayPhaseAPINotReady {
		t.Fatalf("connected phase %q", got)
	}
	if got := statusGatewayPhase(false, false, false, false, false, true, "x", time.Hour); got != rpc.GatewayPhasePortRejecting {
		t.Fatalf("rejecting phase %q", got)
	}
}
