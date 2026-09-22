package daemon

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/discover"
)

// serveLoopback serves loopback connections with behave until the test ends
// and returns the port.
func serveLoopback(t *testing.T, behave func(net.Conn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			behave(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func closeWithoutReading(c net.Conn) { _ = c.Close() }

func resetWithoutReading(c net.Conn) {
	_ = c.(*net.TCPConn).SetLinger(0)
	_ = c.Close()
}

// stubIBKRProcess makes discovery see exactly these process-list lines.
func stubIBKRProcess(t *testing.T, lines ...string) {
	t.Helper()
	old := discover.ProcessLister
	discover.ProcessLister = func(context.Context) []string { return lines }
	t.Cleanup(func() { discover.ProcessLister = old })
}

const gatewayProcess = "4242 /Applications/IB Gateway 10.37/IB Gateway 10.37.app/Contents/MacOS/JavaApplicationStub"

// A listener that accepts the TCP connection and ends it before the API
// handshake is neither "no listener" nor a raw socket error. Whether it
// closes or resets, and whichever client step the reset lands on, the
// daemon publishes one stable verdict that names the app owning the port
// and what to verify in it, and the status carries it as its own phase.
func TestAListenerThatDropsConnectionsBeforeTheHandshakeIsItsOwnState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		behave func(net.Conn)
	}{
		{"closes before reading", closeWithoutReading},
		{"resets before reading", resetWithoutReading},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := serveLoopback(t, tc.behave)
			stubIBKRProcess(t, gatewayProcess)
			s := newTestServer(t)
			s.cfg.Gateway.Port = nil
			ep := discover.Endpoint{Host: "127.0.0.1", Port: port, ClientID: 93, PortOrigin: discover.OriginDiscovered}
			s.connectWithFailover(t.Context(), ep)

			s.mu.Lock()
			verdict := s.lastConnectError
			s.mu.Unlock()
			for _, want := range []string{
				fmt.Sprintf("IB Gateway (pid 4242) accepts connections on 127.0.0.1:%d", port),
				"before the API handshake", "Trusted IPs", "application firewall",
			} {
				if !strings.Contains(verdict, want) {
					t.Fatalf("verdict lacks %q: %s", want, verdict)
				}
			}
			for _, raw := range []string{"Enable ActiveX", "broken pipe", "invalid argument", "reset by peer", "EOF", "none of 1 discovered"} {
				if strings.Contains(verdict, raw) {
					t.Fatalf("verdict still carries %q: %s", raw, verdict)
				}
			}

			res := s.statusHealthSnapshot()
			if res.GatewayPhase != "port_rejecting" {
				t.Fatalf("phase %q, want port_rejecting", res.GatewayPhase)
			}
			if res.Connected || res.LastError != verdict {
				t.Fatalf("status connected=%v last_error=%q", res.Connected, res.LastError)
			}
			if res.Verdict.State != "OFFLINE" || !strings.Contains(res.Verdict.Reason, "before the handshake") {
				t.Fatalf("verdict %+v", res.Verdict)
			}
		})
	}
}

// A port nobody listens on keeps the port-down reading and the generic
// exhaustion verdict.
func TestARefusedPortStaysPortDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	stubIBKRProcess(t, gatewayProcess)
	s := newTestServer(t)
	s.cfg.Gateway.Port = nil
	s.connectWithFailover(t.Context(), discover.Endpoint{Host: "127.0.0.1", Port: port, ClientID: 93, PortOrigin: discover.OriginDiscovered})
	res := s.statusHealthSnapshot()
	if res.GatewayPhase != "port_down" || strings.Contains(res.LastError, "before the API handshake") {
		t.Fatalf("refused port: phase %q last_error %q", res.GatewayPhase, res.LastError)
	}
}
