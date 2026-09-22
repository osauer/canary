package discover

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// The probe tells a port nobody listens on from a listener that holds the
// connection, one that closes it before reading a byte, and one that resets
// it — the reset landing before or after the dial completes alike.
func TestProbeTellsAListenerThatHoldsFromOneThatEndsTheConnection(t *testing.T) {
	cases := []struct {
		name    string
		port    int
		want    ProbeVerdict
		wantErr bool
	}{
		{"holds", startListener(t, holdOpen(t)), ProbeListening, false},
		{"closes before reading", startListener(t, closeBeforeReading), ProbeClosed, false},
		{"resets before reading", startListener(t, resetBeforeReading), ProbeReset, false},
		{"nobody listens", refusedPort(t), ProbeNoListener, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range 10 {
				got, err := Probe(t.Context(), "127.0.0.1", tc.port, 200*time.Millisecond)
				if got != tc.want || (err != nil) != tc.wantErr {
					t.Fatalf("probe %d: verdict %q err %v, want %q err=%v", i+1, got, err, tc.want, tc.wantErr)
				}
			}
		})
	}
	if !ProbeClosed.Rejecting() || !ProbeReset.Rejecting() || ProbeListening.Rejecting() || ProbeNoListener.Rejecting() {
		t.Fatal("only closed and reset are rejections")
	}
}

// Rejecting listeners stay candidates, behind every listener that held the
// connection, and are recorded with how they ended it. A pinned port skips
// the probe and records nothing.
func TestResolveRecordsRejectingListenersBehindTheOnesThatHold(t *testing.T) {
	resetting := startListener(t, resetBeforeReading)
	closing := startListener(t, closeBeforeReading)
	holding := startListener(t, holdOpen(t))
	refused := refusedPort(t)
	ep, err := Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", ProbePorts: []int{resetting, refused, closing, holding}})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Port != holding || !reflect.DeepEqual(ep.Alternates, []int{resetting, closing}) {
		t.Fatalf("port %d alternates %v, want %d then %v", ep.Port, ep.Alternates, holding, []int{resetting, closing})
	}
	want := []Rejection{{Port: resetting, Verdict: ProbeReset}, {Port: closing, Verdict: ProbeClosed}}
	if !reflect.DeepEqual(ep.Rejections, want) {
		t.Fatalf("rejections %+v, want %+v", ep.Rejections, want)
	}

	pinned := resetting
	ep, err = Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", Port: &pinned})
	if err != nil || ep.Port != pinned || ep.PortOrigin != OriginPinned || ep.Rejections != nil {
		t.Fatalf("pinned port probed: %+v %v", ep, err)
	}
}

// The verdict names the app that owns the port and what to verify in it.
// IB Gateway has no API on/off switch, so its hint lists the login and
// prompt states, the API settings, and the OS firewall; TWS keeps its
// checkbox; an unknown listener gets the generic form. The hint carries no
// client-side detail, so repeats compare equal.
func TestRejectedListenerHintMatchesTheApp(t *testing.T) {
	stubProcesses(t, gatewayProcessLine)
	r := DescribeRejectedListener(t.Context(), "127.0.0.1", 4001, ProbeReset)
	if r.Host != "127.0.0.1" || r.Port != 4001 || r.Verdict != ProbeReset || r.App.Name != "IB Gateway" || r.App.PID != 17443 {
		t.Fatalf("verdict %+v", r)
	}
	for _, want := range []string{
		"IB Gateway (pid 17443) accepts connections on 127.0.0.1:4001 and resets them before the API handshake",
		"2FA", "daily auto-restart", "existing session", "incoming-connection prompt",
		"Trusted IPs", "127.0.0.1", "Socket port other than 4001",
		"application firewall",
	} {
		if !strings.Contains(r.Hint, want) {
			t.Fatalf("gateway hint lacks %q: %s", want, r.Hint)
		}
	}
	if strings.Contains(r.Hint, "Enable ActiveX") {
		t.Fatalf("gateway hint names a TWS-only checkbox: %s", r.Hint)
	}
	if again := DescribeRejectedListener(t.Context(), "127.0.0.1", 4001, ProbeReset); again.Hint != r.Hint {
		t.Fatal("the same state produced a different hint")
	}

	stubProcesses(t, twsProcessLine)
	r = DescribeRejectedListener(t.Context(), "127.0.0.1", 7496, ProbeClosed)
	for _, want := range []string{
		"TWS (pid 300) accepts connections on 127.0.0.1:7496 and closes them before the API handshake",
		"Enable ActiveX and Socket Clients", "Trusted IPs", "incoming-connection prompt",
	} {
		if !strings.Contains(r.Hint, want) {
			t.Fatalf("TWS hint lacks %q: %s", want, r.Hint)
		}
	}

	stubProcesses(t)
	r = DescribeRejectedListener(t.Context(), "127.0.0.1", 4001, ProbeReset)
	if r.App.Name != "" || !strings.Contains(r.Hint, "something on 127.0.0.1:4001 accepts connections and resets them") || !strings.Contains(r.Hint, "TWS or IB Gateway") {
		t.Fatalf("unknown-app hint: %+v", r)
	}
}
