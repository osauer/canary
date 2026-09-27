package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRenderStatusRestartDoesNotHideOffline(t *testing.T) {
	var out bytes.Buffer
	h := &rpc.HealthResult{Verdict: rpc.HealthVerdict{State: "OFFLINE", Reason: "Broker data connection unavailable"}, GatewayPhase: rpc.GatewayPhasePortDown, GatewayRestart: &rpc.GatewayRestartHealth{Source: "operator", Time: "23:45", Timezone: "UTC", State: "overrun", Reason: "Local API recovery exceeded the declared restart window"}}
	renderStatusText(&Env{Stdout: &out, Stderr: &bytes.Buffer{}}, h, nil)
	for _, want := range []string{"IBKR Gateway  OFFLINE", "GW restart", "23:45 UTC", "overrun", "Next concern"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
}
