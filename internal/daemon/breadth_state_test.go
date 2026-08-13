package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

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
