package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A port that accepts connections and drops them before the API handshake
// is rendered as its own state: the row states the fact and the app that
// owns the port, the daemon's verdict is printed in full, and the generic
// TWS checklist stays out of the way.
func TestRenderStatusNamesAPortThatDropsConnections(t *testing.T) {
	t.Parallel()
	hint := "IB Gateway (pid 17443) accepts connections on 127.0.0.1:4001 and resets them before the API handshake, so nothing reaches its API — verify the login, Trusted IPs and the OS application firewall"
	res := &rpc.HealthResult{
		Verdict:       rpc.HealthVerdict{State: "OFFLINE", Reason: "Broker API port accepts connections and drops them before the handshake"},
		DaemonVersion: "v1.0.0",
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4001,
		ClientID:      15,
		GatewayPhase:  rpc.GatewayPhasePortRejecting,
		LastError:     hint,
		PortRejection: &rpc.PortRejectionHealth{Host: "127.0.0.1", Port: 4001, Ended: "reset", App: "IB Gateway", PID: 17443},
	}
	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, res, nil)
	got := stdout.String()
	for _, want := range []string{
		"IBKR Gateway  OFFLINE",
		"port 4001 accepts connections and resets them before the API handshake (IB Gateway pid 17443)",
		"Next concern   Broker API port accepts connections and drops them before the handshake",
		"  " + hint,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, got)
		}
	}
	for _, stale := range []string{"Enable ActiveX", "handshake in progress", "not connected"} {
		if strings.Contains(got, stale) {
			t.Fatalf("status still shows %q:\n%s", stale, got)
		}
	}
	if isHandshakeInFlight(*res) {
		t.Fatal("a rejecting port is a verdict, not a handshake in flight")
	}
	if verdict := statusVerdict(*res, ""); verdict.Text != "OFFLINE" {
		t.Fatalf("verdict %q", verdict.Text)
	}

	// Without the typed evidence the row still states the fact.
	res.PortRejection = nil
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, res, nil)
	if !strings.Contains(stdout.String(), "port accepts connections and drops them before the API handshake") {
		t.Fatalf("row without evidence:\n%s", stdout.String())
	}
}
