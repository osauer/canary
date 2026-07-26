package discover

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"
)

// stubProbe lets a test pretend specific (host, port) pairs are listening.
// All probes ignore ctx and the timeout — tests don't need that detail.
func stubProbe(open map[int]bool) func(context.Context, string, int, time.Duration) error {
	return func(_ context.Context, _ string, port int, _ time.Duration) error {
		if open[port] {
			return nil
		}
		return errors.New("connection refused")
	}
}

func withProbe(t *testing.T, fn func(context.Context, string, int, time.Duration) error, body func()) {
	t.Helper()
	saved := Probe
	Probe = fn
	defer func() { Probe = saved }()
	body()
}

func TestResolve_PinnedPortSkipsProbe(t *testing.T) {
	called := false
	withProbe(t, func(_ context.Context, _ string, _ int, _ time.Duration) error {
		called = true
		return nil
	}, func() {
		port := 4001
		ep, err := Resolve(context.Background(), PartialGateway{Port: &port})
		if err != nil {
			t.Fatal(err)
		}
		if called {
			t.Error("Probe must not run when port is pinned")
		}
		if ep.PortOrigin != OriginPinned {
			t.Errorf("PortOrigin = %v, want pinned", ep.PortOrigin)
		}
		if ep.Port != 4001 {
			t.Errorf("Port = %d, want 4001", ep.Port)
		}
	})
}

func TestResolve_DiscoversFirstResponder(t *testing.T) {
	withProbe(t, stubProbe(map[int]bool{4002: true, 7496: true}), func() {
		ep, err := Resolve(context.Background(), PartialGateway{})
		if err != nil {
			t.Fatal(err)
		}
		if ep.Port != 4002 {
			t.Errorf("Port = %d, want 4002 (first hit in StandardPorts order)", ep.Port)
		}
		if ep.PortOrigin != OriginDiscovered {
			t.Errorf("PortOrigin = %v, want discovered", ep.PortOrigin)
		}
		if !reflect.DeepEqual(ep.Alternates, []int{7496}) {
			t.Errorf("Alternates = %v, want [7496]", ep.Alternates)
		}
	})
}

func TestResolve_PreferenceOrderRespected(t *testing.T) {
	// 4001 is open AND 4002 is open. 4001 wins because it's first in
	// StandardPorts, regardless of which goroutine completes first.
	withProbe(t, stubProbe(map[int]bool{4001: true, 4002: true, 7496: true}), func() {
		ep, err := Resolve(context.Background(), PartialGateway{})
		if err != nil {
			t.Fatal(err)
		}
		if ep.Port != 4001 {
			t.Errorf("Port = %d, want 4001 (first in StandardPorts)", ep.Port)
		}
		if !reflect.DeepEqual(ep.Alternates, []int{4002, 7496}) {
			t.Errorf("Alternates = %v, want [4002, 7496]", ep.Alternates)
		}
	})
}

func TestResolve_NoListenersReturnsError(t *testing.T) {
	withProbe(t, stubProbe(nil), func() {
		withProcessLister(t, stubProcessLister( /* no processes */ ), func() {
			_, err := Resolve(context.Background(), PartialGateway{})
			if err == nil {
				t.Fatal("expected error when no port responds")
			}
		})
	})
}

// TestResolve_NoListeners_HintsAppRunning guards the v0.8.1 UX: when a
// canonical IBKR app is running but no API port responds, the discovery
// error must name the app and PID so `canary status` can surface a specific
// next action ("Enable ActiveX..." / login pending / custom port) instead
// of the generic timeout users hit pre-v0.8.1.
func TestResolve_NoListeners_HintsAppRunning(t *testing.T) {
	withProbe(t, stubProbe(nil), func() {
		withProcessLister(t, stubProcessLister(
			"48176 /Applications/Trader Workstation/Trader Workstation.app/Contents/MacOS/JavaApplicationStub",
		), func() {
			_, err := Resolve(context.Background(), PartialGateway{})
			if err == nil {
				t.Fatal("expected error")
			}
			msg := err.Error()
			for _, want := range []string{"TWS is running", "pid 48176", "Enable ActiveX"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q must mention %q", msg, want)
				}
			}
		})
	})
}

// TestResolve_NoListeners_HintsNoApp covers the other branch: nothing
// IBKR-shaped is running, hint should tell the user to start one.
func TestResolve_NoListeners_HintsNoApp(t *testing.T) {
	withProbe(t, stubProbe(nil), func() {
		withProcessLister(t, stubProcessLister("789 /usr/bin/zsh"), func() {
			_, err := Resolve(context.Background(), PartialGateway{})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), "no TWS / IB Gateway / IBKR Desktop process found") {
				t.Errorf("error %q must hint at starting an IBKR app", err.Error())
			}
		})
	})
}

func TestResolve_PinnedTLSDisablesFallback(t *testing.T) {
	port := 4001
	tlsTrue := true
	tlsFalse := false

	for _, tc := range []struct {
		name    string
		gateway PartialGateway
		wantTLS bool
		wantFB  bool
		wantOri Origin
	}{
		{"tls=true pinned, no fallback", PartialGateway{Port: &port, TLS: &tlsTrue}, true, false, OriginPinned},
		{"tls=false pinned, no fallback", PartialGateway{Port: &port, TLS: &tlsFalse}, false, false, OriginPinned},
		{"tls auto, fallback on", PartialGateway{Port: &port}, false, true, OriginDiscovered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ep, err := Resolve(context.Background(), tc.gateway)
			if err != nil {
				t.Fatal(err)
			}
			if ep.TLS != tc.wantTLS {
				t.Errorf("TLS = %v, want %v", ep.TLS, tc.wantTLS)
			}
			if ep.EnableTLSFallback != tc.wantFB {
				t.Errorf("EnableTLSFallback = %v, want %v", ep.EnableTLSFallback, tc.wantFB)
			}
			if ep.TLSOrigin != tc.wantOri {
				t.Errorf("TLSOrigin = %v, want %v", ep.TLSOrigin, tc.wantOri)
			}
		})
	}
}

func TestResolve_DefaultsForUnpinnedFields(t *testing.T) {
	port := 4001
	ep, err := Resolve(context.Background(), PartialGateway{Port: &port})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1 (default for empty Host)", ep.Host)
	}
	if ep.ClientID != 15 {
		t.Errorf("ClientID = %d, want 15 (default)", ep.ClientID)
	}
}

// TestProbe_RealLoopback exercises the production probe (dialTCP) against
// a real ephemeral listener — proves the probe correctly distinguishes
// "listening" from "refused" on actual TCP. No IBKR involvement; this is
// pure Go net behavior.
func TestProbe_RealLoopback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if err := dialTCP(context.Background(), "127.0.0.1", port, 200*time.Millisecond); err != nil {
		t.Errorf("dialTCP open port: %v", err)
	}
	// Pick an arbitrary high port that's almost certainly closed locally.
	// Loopback gives RST immediately, so this completes well under timeout.
	if err := dialTCP(context.Background(), "127.0.0.1", 1, 200*time.Millisecond); err == nil {
		t.Error("dialTCP closed port: expected error, got nil")
	}
}
