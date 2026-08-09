// Package discover finds an IB Gateway or TWS endpoint on the local host
// short timeout — we do not exchange the IBKR handshake here. The actual
// handshake runs against the winner through the daemon's broker connector,
package discover

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// StandardPorts is the IBKR-default probe order.
//
//	7496  TWS live
//	7497  TWS paper
var StandardPorts = []int{4001, 4002, 7496, 7497}

// Origin records why a dimension has its current value: was it pinned in
type Origin string

// Origin values classify how endpoint settings were resolved.
const (
	OriginPinned     Origin = "pinned"
	OriginDiscovered Origin = "discovered"
	OriginDefault    Origin = "default"
)

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
	Alternates []int
}

// Probe tests TCP connectivity to host:port with the given timeout. Returns
var Probe = dialTCP

func dialTCP(ctx context.Context, host string, port int, timeout time.Duration) error {
	d := net.Dialer{Timeout: timeout}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// PartialGateway is the minimal subset of config.Gateway this package needs.
type PartialGateway struct {
	Host         string
	Port         *int
	ClientID     *int
	Account      string
	TLS          *bool
	ProbePorts   []int         // override StandardPorts; empty → StandardPorts
	ProbeTimeout time.Duration // per-port; 0 → 200ms
}

// Resolve produces a concrete Endpoint by combining pinned values from g
// wins. Returns an error only if g.Host is unreachable for every candidate
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
		timeout = 200 * time.Millisecond
	}
	ports := g.ProbePorts
	if len(ports) == 0 {
		ports = StandardPorts
	}

	hits, err := probeAll(ctx, host, ports, timeout)
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
	return ep, nil
}

// noListenerError builds the verdict the daemon surfaces (via log + status
// LastError) when no canonical IBKR port responded. Combines the bare TCP
// fact with a DetectIBKRApp pre-flight so the user sees a specific next
//
//	no app running            → start TWS, Gateway, or IBKR Desktop
func noListenerError(ctx context.Context, host string, ports []int, timeout time.Duration) error {
	base := fmt.Sprintf("no IBKR listener found on %s ports %v (probe timeout %s)", host, ports, timeout)
	app := DetectIBKRApp(ctx)
	switch {
	case app.Name != "":
		return fmt.Errorf("%s; %s is running (pid %d) but its API socket isn't open — most likely 'Enable ActiveX and Socket Clients' is unchecked (Global Configuration → API → Settings), login hasn't fully completed (2FA / day-end dialog), or you set a non-default Socket port (pin it in ~/.config/ibkr/config.toml under [gateway])", base, app.Name, app.PID)
	default:
		return fmt.Errorf("%s; no TWS / IB Gateway / IBKR Desktop process found — start one and ibkr will reconnect automatically", base)
	}
}

// probeAll runs Probe in parallel against every candidate port and returns
func probeAll(ctx context.Context, host string, ports []int, perPortTimeout time.Duration) ([]int, error) {
	type result struct {
		idx int
		ok  bool
	}
	results := make([]result, len(ports))
	var wg sync.WaitGroup
	for i, p := range ports {
		wg.Go(func() {
			err := Probe(ctx, host, p, perPortTimeout)
			results[i] = result{idx: i, ok: err == nil}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hits := make([]int, 0, len(ports))
	for _, r := range results {
		if r.ok {
			hits = append(hits, ports[r.idx])
		}
	}
	return hits, nil
}
