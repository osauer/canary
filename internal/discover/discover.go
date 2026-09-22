// Package discover finds an IB Gateway or TWS endpoint on the local host
// when the user hasn't pinned one in config. The probe is TCP-only with a
// short timeout — we do not exchange the IBKR handshake here. It does tell a
// port nobody listens on from one whose listener accepts the connection and
// ends it before reading a byte, because the two need different fixes. The
// actual handshake runs against the winner through the daemon's broker
// connector, whose bounded connect path reports non-responsive listeners
// explicitly.
package discover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// StandardPorts is the IBKR-default probe order.
//
//	7496  TWS live
//	7497  TWS paper
var StandardPorts = []int{4001, 4002, 7496, 7497}

// DefaultProbeTimeout is the per-port budget when PartialGateway.ProbeTimeout
// is unset: the dial and, once it completes, the wait for the listener's
// first move.
const DefaultProbeTimeout = 200 * time.Millisecond

// Origin records why a dimension has its current value: was it pinned in
// config (binding), discovered by probe, or filled from a built-in default.
type Origin string

// Origin values classify how endpoint settings were resolved.
const (
	OriginPinned     Origin = "pinned"
	OriginDiscovered Origin = "discovered"
	OriginDefault    Origin = "default"
)

// ProbeVerdict is what one TCP probe learned about a port.
type ProbeVerdict string

// Probe verdicts. A listener that accepts the connection and ends it before
// reading a byte is not "no listener": the app owns the socket, and its API
// is turning clients away before the handshake.
const (
	// ProbeNoListener: the dial was refused, timed out or found no route.
	ProbeNoListener ProbeVerdict = "no_listener"
	// ProbeListening: the listener accepted the connection and held it,
	// waiting for the client's first bytes as a ready TWS/Gateway API does.
	ProbeListening ProbeVerdict = "listening"
	// ProbeClosed: the listener accepted the connection and closed it (FIN)
	// before reading a byte.
	ProbeClosed ProbeVerdict = "closed"
	// ProbeReset: the listener accepted the connection and reset it (RST),
	// either before the dial completed or before reading a byte.
	ProbeReset ProbeVerdict = "reset"
)

// Rejecting reports whether the listener ended the connection before the
// API handshake.
func (v ProbeVerdict) Rejecting() bool { return v == ProbeClosed || v == ProbeReset }

// Rejection records a probed port whose listener accepted the TCP connection
// and ended it before reading a byte.
type Rejection struct {
	Port    int
	Verdict ProbeVerdict // ProbeClosed or ProbeReset
}

// Endpoint is the post-discovery, fully-concrete connection spec the
// daemon hands to pkg/ibkr.
type Endpoint struct {
	Host       string
	Port       int
	PortOrigin Origin

	// TLS is the mode the SDK should attempt first.
	TLS       bool
	TLSOrigin Origin

	// EnableTLSFallback flips the SDK's tlsAttempts to retry the alternate
	// TLS mode on failure. We set this true only when the user left TLS
	EnableTLSFallback bool

	ClientID int
	Account  string

	// Alternates lists other ports that responded during the probe but
	// lost the first-hit race. Surface them in `canary status` so the user
	// knows e.g. "I'm on Gateway live but TWS is also up." Empty when the
	// port was pinned (discovery skipped) or no other ports responded.
	Alternates []int

	// Rejections lists the probed ports whose listener accepted the TCP
	// connection and closed or reset it before reading a byte. Each stays a
	// candidate, in Port or Alternates, behind every listener that held its
	// connection: the app owns the socket but its API is turning clients
	// away before the handshake, which only the handshake can confirm.
	Rejections []Rejection
}

// Probe tests host:port within timeout and classifies what answered. A dial
// error other than a reset means no listener. A completed dial is followed
// by a read for the rest of the budget, because a ready TWS/Gateway API says
// nothing until it has the client's version descriptor, while one that is
// turning clients away closes or resets within microseconds. The error is
// the dial error behind a ProbeNoListener verdict; tests replace the whole
// function.
var Probe = dialTCP

func dialTCP(ctx context.Context, host string, port int, timeout time.Duration) (ProbeVerdict, error) {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	deadline := time.Now().Add(timeout)
	d := net.Dialer{Deadline: deadline}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		// A reset during the dial means the kernel completed the TCP
		// handshake and the peer tore the connection down before the dialer
		// looked at it: a listener exists. A refused dial is the kernel
		// answering for a port nobody listens on.
		if errors.Is(err, syscall.ECONNRESET) {
			return ProbeReset, nil
		}
		return ProbeNoListener, err
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(deadline)
	var first [1]byte
	_, err = conn.Read(first[:])
	switch {
	case err == nil:
		// Unsolicited bytes: not an IBKR API, but something is listening.
		return ProbeListening, nil
	case errors.Is(err, io.EOF):
		return ProbeClosed, nil
	case errors.Is(err, syscall.ECONNRESET):
		return ProbeReset, nil
	default:
		// The read timed out: the listener is holding the connection for
		// our first bytes.
		return ProbeListening, nil
	}
}

// PartialGateway is the minimal subset of config.Gateway this package needs.
type PartialGateway struct {
	Host         string
	Port         *int
	ClientID     *int
	Account      string
	TLS          *bool
	ProbePorts   []int         // override StandardPorts; empty → StandardPorts
	ProbeTimeout time.Duration // per-port; 0 → DefaultProbeTimeout
}

// Resolve produces a concrete Endpoint by combining pinned values from g
// with a probe of the standard ports for the rest. Listeners that held the
// probe's connection lead the candidates; those that ended it before
// reading a byte follow, recorded in Endpoint.Rejections. It returns an
// error only when nothing listens on any candidate port, or the context
// ends first.
func Resolve(ctx context.Context, g PartialGateway) (Endpoint, error) {
	host := g.Host
	if host == "" {
		host = "127.0.0.1"
	}
	clientID := 15
	if g.ClientID != nil {
		clientID = *g.ClientID
	}

	ep := Endpoint{
		Host:     host,
		ClientID: clientID,
		Account:  g.Account,
	}

	// TLS dimension: pinned → strict, no fallback. Auto → start with plain
	if g.TLS != nil {
		ep.TLS = *g.TLS
		ep.TLSOrigin = OriginPinned
		ep.EnableTLSFallback = false
	} else {
		ep.TLS = false
		ep.TLSOrigin = OriginDiscovered
		ep.EnableTLSFallback = true
	}

	// Port dimension: pinned → use as-is, skip probe entirely.
	if g.Port != nil {
		ep.Port = *g.Port
		ep.PortOrigin = OriginPinned
		return ep, nil
	}

	timeout := g.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	ports := g.ProbePorts
	if len(ports) == 0 {
		ports = StandardPorts
	}

	hits, rejections, err := probeAll(ctx, host, ports, timeout)
	if err != nil {
		return ep, err
	}
	if len(hits) == 0 {
		return ep, noListenerError(ctx, host, ports, timeout)
	}
	ep.Port = hits[0]
	ep.PortOrigin = OriginDiscovered
	if len(hits) > 1 {
		ep.Alternates = hits[1:]
	}
	ep.Rejections = rejections
	return ep, nil
}

// IBKR app names as DetectIBKRApp reports them.
const (
	appTWS     = "TWS"
	appGateway = "IB Gateway"
)

// noListenerError builds the verdict the daemon surfaces (via log + status
// LastError) when no canonical IBKR port responded. Combines the bare TCP
// fact with a DetectIBKRApp pre-flight so the user sees a specific next
// step for the app that is actually running:
//
//	no app running            → start TWS, Gateway, or IBKR Desktop
//	IB Gateway running        → still starting, or a non-default Socket port;
//	                            the Gateway has no API on/off switch
//	TWS / IBKR Desktop running → the API socket switch, an unfinished login,
//	                            or a non-default Socket port
func noListenerError(ctx context.Context, host string, ports []int, timeout time.Duration) error {
	base := fmt.Sprintf("no IBKR listener found on %s ports %v (probe timeout %s)", host, ports, timeout)
	app := DetectIBKRApp(ctx)
	switch app.Name {
	case "":
		return fmt.Errorf("%s; no TWS / IB Gateway / IBKR Desktop process found — start one and ibkr will reconnect automatically", base)
	case appGateway:
		return fmt.Errorf("%s; IB Gateway is running (pid %d) but its API port is closed — it is still starting (the port opens once its login has completed), or it listens on a non-default Socket port (Configure → Settings → API → Settings; pin it in ~/.config/ibkr/config.toml under [gateway])", base, app.PID)
	default:
		return fmt.Errorf("%s; %s is running (pid %d) but its API socket isn't open — most likely 'Enable ActiveX and Socket Clients' is unchecked (Global Configuration → API → Settings), login hasn't fully completed (2FA / day-end dialog), or you set a non-default Socket port (pin it in ~/.config/ibkr/config.toml under [gateway])", base, app.Name, app.PID)
	}
}

// RejectedListener is the verdict on a listener that accepted the TCP
// connection and ended it before the API handshake, with the hint for the
// IBKR app that owns the port.
type RejectedListener struct {
	Host    string
	Port    int
	Verdict ProbeVerdict // ProbeClosed or ProbeReset
	// App is the IBKR process found on the host; zero when none was.
	App IBKRApp
	// Hint is the operator-facing verdict: the fact, then what to check in
	// that app. It carries no client-side detail, so repeats compare equal
	// and the daemon logs the state once.
	Hint string
}

// DescribeRejectedListener names what accepted and dropped the connection on
// host:port and what to verify. Only the handshake can tell why an app turns
// a client away, so the hint lists the settings and dialogs that do it:
// IB Gateway has no API on/off switch, so the TWS checkbox is not among
// its causes.
func DescribeRejectedListener(ctx context.Context, host string, port int, verdict ProbeVerdict) RejectedListener {
	ended := "closes"
	if verdict == ProbeReset {
		ended = "resets"
	}
	app := DetectIBKRApp(ctx)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	r := RejectedListener{Host: host, Port: port, Verdict: verdict, App: app}
	switch app.Name {
	case appGateway:
		r.Hint = fmt.Sprintf("IB Gateway (pid %d) accepts connections on %s and %s them before the API handshake, so nothing reaches its API — either the Gateway turns clients away (login or initialisation not finished: 2FA, the daily auto-restart, an 'existing session' dialog; an incoming-connection prompt waiting in its window; under Configure → Settings → API → Settings and Precautions a Trusted IPs rule that does not cover %s or is set to reject, or a Socket port other than %d) or an OS application firewall in front of it resets accepted connections (macOS: System Settings → Network → Firewall)", app.PID, addr, ended, host, port)
	case appTWS:
		r.Hint = fmt.Sprintf("TWS (pid %d) accepts connections on %s and %s them before the API handshake — verify that 'Enable ActiveX and Socket Clients' is on (Global Configuration → API → Settings), that login has finished (2FA, an 'existing session' dialog), that no incoming-connection prompt is waiting in the TWS window, that Trusted IPs includes %s (or is empty), and that the OS application firewall allows incoming connections to TWS", app.PID, addr, ended, host)
	case "":
		r.Hint = fmt.Sprintf("something on %s accepts connections and %s them before the API handshake; if it is TWS or IB Gateway, finish its login and check its API settings (Trusted IPs, a waiting incoming-connection prompt, the Socket port) and the OS application firewall", addr, ended)
	default:
		r.Hint = fmt.Sprintf("%s (pid %d) accepts connections on %s and %s them before the API handshake — verify that its API is enabled, that login has finished, that no incoming-connection prompt is waiting, that Trusted IPs includes %s (or is empty), and that the OS application firewall allows incoming connections to it", app.Name, app.PID, addr, ended, host)
	}
	return r
}

// probeAll runs Probe in parallel against every candidate port. Listeners
// that held the connection come first, in probe order; those that ended it
// before reading a byte follow, so a dropping IB Gateway on 4001 no longer
// shadows a ready TWS on 7496.
func probeAll(ctx context.Context, host string, ports []int, perPortTimeout time.Duration) ([]int, []Rejection, error) {
	verdicts := make([]ProbeVerdict, len(ports))
	var wg sync.WaitGroup
	for i, p := range ports {
		wg.Go(func() {
			verdicts[i], _ = Probe(ctx, host, p, perPortTimeout)
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	hits := make([]int, 0, len(ports))
	for i, v := range verdicts {
		if v == ProbeListening {
			hits = append(hits, ports[i])
		}
	}
	var rejections []Rejection
	for i, v := range verdicts {
		if v.Rejecting() {
			hits = append(hits, ports[i])
			rejections = append(rejections, Rejection{Port: ports[i], Verdict: v})
		}
	}
	return hits, rejections, nil
}
