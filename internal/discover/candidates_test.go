package discover

import (
	"context"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// startListener serves loopback connections with behave until the test ends
// and returns the port. behave owns each accepted connection.
func startListener(t *testing.T, behave func(net.Conn)) int {
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

// holdOpen keeps every accepted connection open until the test ends: a
// ready API waiting for the client's first bytes.
func holdOpen(t *testing.T) func(net.Conn) {
	t.Helper()
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return func(c net.Conn) {
		mu.Lock()
		held = append(held, c)
		mu.Unlock()
	}
}

// closeBeforeReading sends FIN without reading a byte.
func closeBeforeReading(c net.Conn) { _ = c.Close() }

// resetBeforeReading sends RST without reading a byte.
func resetBeforeReading(c net.Conn) {
	_ = c.(*net.TCPConn).SetLinger(0)
	_ = c.Close()
}

// refusedPort is a loopback port nobody listens on.
func refusedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// stubProcesses replaces the process lister for the test.
func stubProcesses(t *testing.T, lines ...string) {
	t.Helper()
	old := ProcessLister
	ProcessLister = func(context.Context) []string { return lines }
	t.Cleanup(func() { ProcessLister = old })
}

const (
	gatewayProcessLine = "17443 /Applications/IB Gateway 10.37/IB Gateway 10.37.app/Contents/MacOS/JavaApplicationStub"
	twsProcessLine     = "300 /Applications/Trader Workstation 10.37/Trader Workstation.app/Contents/MacOS/JavaApplicationStub"
)

// A listener that closes the probe's connection before reading a byte must
// not lead the candidates: a ready API holds the connection and waits for
// the client's version descriptor.
func TestResolvePrefersAListenerThatHoldsOverOneThatClosesFirst(t *testing.T) {
	closing := startListener(t, closeBeforeReading)
	holding := startListener(t, holdOpen(t))
	ep, err := Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", ProbePorts: []int{closing, holding}})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Port != holding || !reflect.DeepEqual(ep.Alternates, []int{closing}) {
		t.Fatalf("port %d alternates %v, want %d first and %d as the alternate", ep.Port, ep.Alternates, holding, closing)
	}
}

// A listener that resets the connection is a listener. The reset can land
// before the dial completes, which the kernel reports as a dial error, and
// that must not become "no IBKR listener found".
func TestResolveKeepsAResettingListenerInsteadOfReportingNoListener(t *testing.T) {
	resetting := startListener(t, resetBeforeReading)
	for i := range 30 {
		ep, err := Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", ProbePorts: []int{resetting}})
		if err != nil {
			t.Fatalf("attempt %d reported no listener: %v", i+1, err)
		}
		if ep.Port != resetting || ep.PortOrigin != OriginDiscovered {
			t.Fatalf("attempt %d resolved %d (%s), want %d discovered", i+1, ep.Port, ep.PortOrigin, resetting)
		}
	}
}

// IB Gateway has no 'Enable ActiveX and Socket Clients' setting. A closed
// port with the Gateway running is a Gateway still starting or on another
// Socket port, and the verdict must say so; the checkbox stays a TWS hint.
func TestNoListenerHintNamesTheGatewayWithoutTheTWSCheckbox(t *testing.T) {
	refused := refusedPort(t)
	stubProcesses(t, gatewayProcessLine)
	_, err := Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", ProbePorts: []int{refused}})
	if err == nil {
		t.Fatal("a refused port resolved")
	}
	msg := err.Error()
	for _, want := range []string{"no IBKR listener found", "IB Gateway is running (pid 17443)", "Socket port"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("gateway verdict lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "Enable ActiveX") {
		t.Fatalf("gateway verdict names a TWS-only checkbox: %s", msg)
	}

	stubProcesses(t, twsProcessLine)
	_, err = Resolve(t.Context(), PartialGateway{Host: "127.0.0.1", ProbePorts: []int{refused}})
	if err == nil || !strings.Contains(err.Error(), "TWS is running (pid 300)") || !strings.Contains(err.Error(), "Enable ActiveX and Socket Clients") {
		t.Fatalf("TWS verdict lost the checkbox hint: %v", err)
	}
}
