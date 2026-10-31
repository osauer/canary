package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

// offlineHealth is a disconnected daemon snapshot with the daemon's own
// verdict and its last connect verdict, as `status.health` serves them.
func offlineHealth(phase, lastError string) rpc.HealthResult {
	return rpc.HealthResult{
		Verdict:       rpc.HealthVerdict{State: "OFFLINE", Reason: "Broker data connection unavailable"},
		DaemonVersion: "v0.0.0-golden",
		GatewayHost:   "127.0.0.1",
		GatewayPort:   4002,
		PortOrigin:    "configured",
		ClientID:      15,
		GatewayPhase:  phase,
		LastError:     lastError,
	}
}

// statusOfflineCases are the first-run outages the v3.25.4 review named: the
// daemon already knows the cause and the next step, so `canary status` must
// say them instead of only "Broker data connection unavailable".
func statusOfflineCases() []struct {
	name string
	res  rpc.HealthResult
} {
	noGateway := "no IBKR listener found on 127.0.0.1 ports [4001 4002 7496 7497] (probe timeout 200ms); " + discover.NoAppRunningHint
	refusedPinned := "127.0.0.1:4002 refused the connection; " + discover.ClosedPortHint(discover.IBKRApp{Name: "IB Gateway", PID: 4242})
	refusedNoApp := "no TWS / IB Gateway / IBKR Desktop process running; waiting for one to start (127.0.0.1:4002 refused the connection; the process list is checked every 5s and the port is dialled at least every 1m0s)"
	handshake := "gateway 127.0.0.1:4002 did not handshake within 12s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on"
	rejecting := offlineHealth(rpc.GatewayPhasePortRejecting, "IB Gateway (pid 4242) accepts connections on 127.0.0.1:4002 and resets them before the API handshake, so nothing reaches its API — verify the login, Trusted IPs and the OS application firewall")
	rejecting.Verdict.Reason = "Broker API port accepts connections and drops them before the handshake"
	rejecting.PortRejection = &rpc.PortRejectionHealth{Host: "127.0.0.1", Port: 4002, Ended: "reset", App: "IB Gateway", PID: 4242}
	// A clean profile pins nothing: discovery found no port to name.
	fresh := offlineHealth(rpc.GatewayPhasePortDown, noGateway)
	fresh.GatewayPort, fresh.PortOrigin = 0, ""
	return []struct {
		name string
		res  rpc.HealthResult
	}{
		{"no gateway", fresh},
		{"refused pinned port, app running", offlineHealth(rpc.GatewayPhasePortDown, refusedPinned)},
		{"refused pinned port, no app", offlineHealth(rpc.GatewayPhasePortDown, refusedNoApp)},
		{"incomplete handshake", offlineHealth(rpc.GatewayPhasePortDown, handshake)},
		{"rejecting port", rejecting},
	}
}

func normalizedText(s string) string { return strings.Join(strings.Fields(s), " ") }

func TestRenderStatusOfflineShowsTheDaemonsCauseAndNextStep(t *testing.T) {
	for _, tc := range statusOfflineCases() {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, &tc.res, nil)
			got := normalizedText(stdout.String())
			cause := normalizedText(tc.res.LastError)
			if n := strings.Count(got, cause); n != 1 {
				t.Fatalf("daemon cause printed %d times, want once:\n%s", n, stdout.String())
			}
			for _, want := range []string{
				"Canary OFFLINE",
				"Last attempt " + cause,
				"Reason " + tc.res.Verdict.Reason,
				"Daemon log:",
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("status missing %q:\n%s", want, stdout.String())
				}
			}
			for _, stale := range []string{"Handshake did not complete", "handshake in progress", "Gateway offline:"} {
				if strings.Contains(got, stale) {
					t.Fatalf("status shows %q:\n%s", stale, stdout.String())
				}
			}
		})
	}
}

// A handshake still in flight after the wait shows the daemon's verdict on
// its last attempt in place of the generic checklist, so the same advice is
// not printed twice; with no verdict yet the checklist stays.
func TestRenderStatusHandshakeInFlightPrefersTheDaemonsVerdict(t *testing.T) {
	watchdog := "gateway 127.0.0.1:4002 not responding to TWS handshake within 12s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on"
	res := offlineHealth(rpc.GatewayPhaseConnecting, watchdog)
	res.Verdict = rpc.HealthVerdict{State: "STARTING", Reason: "Broker API handshake is incomplete"}
	var stdout bytes.Buffer
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, &res, nil)
	got := normalizedText(stdout.String())
	for _, want := range []string{"Canary STARTING", "handshake in progress", "Last attempt " + normalizedText(watchdog), "Handshake did not complete within 25s.", "Daemon log:"} {
		if !strings.Contains(got, want) {
			t.Fatalf("status missing %q:\n%s", want, stdout.String())
		}
	}
	if n := strings.Count(got, "Enable ActiveX and Socket Clients"); n != 1 {
		t.Fatalf("handshake advice printed %d times, want once:\n%s", n, stdout.String())
	}

	res.LastError = ""
	stdout.Reset()
	renderStatusText(&Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}, &res, nil)
	got = normalizedText(stdout.String())
	if strings.Contains(got, "Last attempt") || !strings.Contains(got, "Check the daemon log for the underlying error") || !strings.Contains(got, "Enable ActiveX and Socket Clients") {
		t.Fatalf("handshake without a verdict lost the checklist:\n%s", stdout.String())
	}
}

// Exit codes and the JSON contract do not move: a disconnected daemon exits 1
// on both paths and the JSON keeps the daemon's verdict and last error.
func TestStatusOfflineExitCodesAndJSONPreserved(t *testing.T) {
	for _, tc := range statusOfflineCases() {
		t.Run(tc.name, func(t *testing.T) {
			conn := goldenConn{rpc.MethodStatusHealth: tc.res, rpc.MethodAlertCandidates: rpc.AlertCandidateSnapshot{}}
			var stdout, stderr bytes.Buffer
			if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}, "status", nil); code != 1 {
				t.Fatalf("text exit %d, want 1; stderr %s", code, stderr.String())
			}
			stdout.Reset()
			if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}, "status", []string{"--json"}); code != 1 {
				t.Fatalf("json exit %d, want 1; stderr %s", code, stderr.String())
			}
			var got rpc.HealthResult
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.LastError != tc.res.LastError || got.Verdict != tc.res.Verdict || got.GatewayPhase != tc.res.GatewayPhase {
				t.Fatalf("JSON changed the daemon's evidence: %+v", got)
			}
		})
	}
}
