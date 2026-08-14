package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The rpc breadth-failure vocabulary is filled by a type conversion from the
// spx values, so the two constant families are load-bearing string twins:
// drift in either would ship an undeclared wire value silently.
func TestBreadthRefreshFailureMirrorsSPXValues(t *testing.T) {
	pairs := map[rpc.BreadthRefreshFailure]spx.RefreshFailure{
		rpc.BreadthRefreshFailureFetch:     spx.RefreshFailureFetch,
		rpc.BreadthRefreshFailurePersist:   spx.RefreshFailurePersist,
		rpc.BreadthRefreshFailureCancelled: spx.RefreshFailureCancelled,
		rpc.BreadthRefreshFailureTransport: spx.RefreshFailureTransport,
	}
	for wire, engine := range pairs {
		if string(wire) != string(engine) {
			t.Errorf("wire %q != engine %q", wire, engine)
		}
	}
}

// A served snapshot whose latest refresh pass stayed below the publication
// coverage threshold is degraded, not ready: the wire must say the reading
// is last-good while convergence is still being retried.
func TestClassifyBreadthState(t *testing.T) {
	cases := []struct {
		name                         string
		snapshot, active, shortCover bool
		want                         rpc.BreadthState
	}{
		{"healthy", true, false, false, rpc.BreadthStateReady},
		{"serving last-good during retries", true, true, true, rpc.BreadthStateDegraded},
		{"serving last-good after retries stopped", true, false, true, rpc.BreadthStateDegraded},
		{"cold refresh in flight", false, true, false, rpc.BreadthStateComputing},
		{"nothing", false, false, false, rpc.BreadthStateCold},
	}
	for _, tc := range cases {
		if got := classifyBreadthState(tc.snapshot, tc.active, tc.shortCover); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
	}
}
