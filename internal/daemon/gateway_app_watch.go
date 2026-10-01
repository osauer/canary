package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/discover"
)

// A refused dial to a local IBKR API port has two very different causes: no
// IBKR app is running at all, or one is running but its API port is not open
// yet (starting, logging in) or not open at all. The process list tells them
// apart. The verdict is advisory: it changes the wording and the dial pacing,
// never whether the daemon dials. A failed or empty process lookup, a remote
// gateway host, or any dial failure other than a refusal keeps the plain
// reconnect backoff and the raw dial error.
const (
	// gatewayAppVerdictTTL bounds how often a refused dial may consult the
	// process list; reconnect cycles inside it reuse the cached verdict.
	gatewayAppVerdictTTL = 30 * time.Second
	// gatewayAppAbsentRedial is the dial cadence while no IBKR app process
	// runs. It is also the hard cap: an app the process list fails to
	// recognise is still dialled at least this often.
	gatewayAppAbsentRedial = 60 * time.Second
	// gatewayAppPollInterval is how often the process list is checked while
	// no IBKR app runs; an app that appears is dialled at once.
	gatewayAppPollInterval = 10 * time.Second
	// gatewayAppLookupBudget bounds one process-list lookup.
	gatewayAppLookupBudget = 2 * time.Second
)

// gatewayAppWatch is the process-aware half of the reconnect pacing. Its own
// mutex orders after s.mu: code holding s.mu may take it, never the reverse.
type gatewayAppWatch struct {
	mu sync.Mutex
	// app/known/checkedAt cache the last process-list verdict.
	app       discover.IBKRApp
	known     bool
	checkedAt time.Time
	// absent: the current outage is classified "no IBKR app running", which
	// stretches the dial cadence to gatewayAppAbsentRedial.
	absent bool
	// appeared: an IBKR app process showed up during an absent outage; the
	// next reconnect gate passes at once, whatever the backoff.
	appeared bool
	// stop is non-nil while the process-list poller runs; closing it stops
	// the poller. Only clearAbsentLocked and the poller's own exit clear it.
	stop chan struct{}
	// inspect replaces gatewayAppInspector for one server in tests.
	inspect func(context.Context) (discover.IBKRApp, bool)
	// tick replaces the poller's ticker channel in tests.
	tick func(time.Duration) (<-chan time.Time, func())
}

// gatewayAppInspector is the process-list query used when a server sets no
// inspect hook. Tests in this package replace it with a stub that reports no
// information, so no test shells out to ps unless it opts in.
var gatewayAppInspector = discover.InspectIBKRApp

// clearAbsentLocked ends the "no IBKR app running" classification and stops
// the poller. Caller holds w.mu.
func (w *gatewayAppWatch) clearAbsentLocked() {
	w.absent = false
	if w.stop != nil {
		close(w.stop)
		w.stop = nil
	}
}

// polling reports whether the process-list poller is running.
func (w *gatewayAppWatch) polling() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stop != nil
}

// lookup runs one bounded process-list query and caches it. Caller must not
// hold w.mu.
func (w *gatewayAppWatch) lookup(ctx context.Context, now time.Time) (discover.IBKRApp, bool) {
	w.mu.Lock()
	inspect := w.inspect
	w.mu.Unlock()
	if inspect == nil {
		inspect = gatewayAppInspector
	}
	lookupCtx, cancel := context.WithTimeout(ctx, gatewayAppLookupBudget)
	defer cancel()
	app, known := inspect(lookupCtx)
	w.mu.Lock()
	w.app, w.known, w.checkedAt = app, known, now
	w.mu.Unlock()
	return app, known
}

// verdict returns the cached process-list verdict, refreshing it when older
// than gatewayAppVerdictTTL.
func (w *gatewayAppWatch) verdict(ctx context.Context, now time.Time) (discover.IBKRApp, bool) {
	w.mu.Lock()
	if !w.checkedAt.IsZero() && now.Sub(w.checkedAt) >= 0 && now.Sub(w.checkedAt) < gatewayAppVerdictTTL {
		app, known := w.app, w.known
		w.mu.Unlock()
		return app, known
	}
	w.mu.Unlock()
	return w.lookup(ctx, now)
}

// quietPeriod is the reconnect gate's wait after a failed attempt: the plain
// backoff, stretched to gatewayAppAbsentRedial while no IBKR app runs, and
// zero once one has appeared.
func (w *gatewayAppWatch) quietPeriod(streak int) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case w.appeared:
		return 0
	case w.absent:
		return gatewayAppAbsentRedial
	default:
		return reconnectBackoff(streak)
	}
}

// consumeAppeared clears the "dial at once" mark after an attempt starts.
func (w *gatewayAppWatch) consumeAppeared() {
	w.mu.Lock()
	w.appeared = false
	w.mu.Unlock()
}

// reset ends the outage classification after a completed handshake.
func (w *gatewayAppWatch) reset() {
	w.mu.Lock()
	w.clearAbsentLocked()
	w.appeared = false
	w.mu.Unlock()
}

// isLoopbackHost reports whether host names this machine, the only case in
// which the local process list says anything about the gateway.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// isRefusedDial reports whether err is the kernel refusing the dial: nobody
// listens on the port.
func isRefusedDial(err error) bool {
	return err != nil && errors.Is(err, syscall.ECONNREFUSED)
}

// connectorConnectError is the attempter's kept connect error with its
// wrapping intact, or nil when it keeps none.
func connectorConnectError(a connectAttempter) error {
	if reporter, ok := a.(connectErrorReporter); ok {
		return reporter.LastConnectError()
	}
	return nil
}

// gatewayAppAbsentHint is the outage verdict while no IBKR app process runs.
func gatewayAppAbsentHint(ep discover.Endpoint) string {
	return fmt.Sprintf("no TWS / IB Gateway / IBKR Desktop process running; waiting for one to start (%s refused the connection; the process list is checked every %s and the port is dialled at least every %s)",
		net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)), gatewayAppPollInterval, gatewayAppAbsentRedial)
}

// gatewayAppPresentHint is the outage verdict while an IBKR app runs but its
// API port refuses connections.
func gatewayAppPresentHint(ep discover.Endpoint, app discover.IBKRApp) string {
	return fmt.Sprintf("%s refused the connection; %s",
		net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)), discover.ClosedPortHint(app))
}

// classifyRefusedDial consults the process list for a dial to ep that failed
// with err and returns the operator verdict, or ok false when the plain error
// stands (not a refusal, a remote host, or no process information). It also
// sets the outage pacing and starts the process poller when no IBKR app runs.
// It may run ps, so it must stay off request paths; caller must not hold s.mu.
func (s *Server) classifyRefusedDial(ctx context.Context, ep discover.Endpoint, err error) (string, bool) {
	if !isRefusedDial(err) || !isLoopbackHost(ep.Host) {
		return "", false
	}
	return s.classifyClosedLocalPort(ctx, ep)
}

// classifyClosedLocalPort classifies a local outage in which nothing listens
// on the IBKR API port; see classifyRefusedDial.
func (s *Server) classifyClosedLocalPort(ctx context.Context, ep discover.Endpoint) (string, bool) {
	if !isLoopbackHost(ep.Host) || ctx.Err() != nil {
		return "", false
	}
	app, known := s.gatewayApp.verdict(ctx, s.gatewayLogClock())
	w := &s.gatewayApp
	w.mu.Lock()
	switch {
	case !known:
		w.clearAbsentLocked()
		w.mu.Unlock()
		return "", false
	case app.Name != "":
		w.clearAbsentLocked()
		w.mu.Unlock()
		return gatewayAppPresentHint(ep, app), true
	}
	w.absent = true
	var stop chan struct{}
	if w.stop == nil {
		stop = make(chan struct{})
		w.stop = stop
	}
	w.mu.Unlock()
	if stop != nil {
		go s.runGatewayAppPoller(ctx, stop)
	}
	return gatewayAppAbsentHint(ep), true
}

// runGatewayAppPoller checks the process list every gatewayAppPollInterval
// while the outage is classified "no IBKR app running", and dials as soon as
// one appears. It exits as soon as the classification ends (stop closes) or
// ctx, the daemon's server context, ends.
func (s *Server) runGatewayAppPoller(ctx context.Context, stop chan struct{}) {
	w := &s.gatewayApp
	defer func() {
		// Release the poller slot unless the classification already did,
		// or a later poller now owns it.
		w.mu.Lock()
		if w.stop == stop {
			close(stop)
			w.stop = nil
		}
		w.mu.Unlock()
	}()
	w.mu.Lock()
	tick := w.tick
	w.mu.Unlock()
	if tick == nil {
		tick = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	c, stopTicker := tick(gatewayAppPollInterval)
	defer stopTicker()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-c:
		}
		if !s.pollGatewayApp(ctx) {
			return
		}
	}
}

// pollGatewayApp is one poller step. It reports whether polling continues.
func (s *Server) pollGatewayApp(ctx context.Context) bool {
	w := &s.gatewayApp
	w.mu.Lock()
	if !w.absent {
		w.mu.Unlock()
		return false
	}
	w.mu.Unlock()
	app, known := w.lookup(ctx, s.gatewayLogClock())
	w.mu.Lock()
	switch {
	case !w.absent:
		// A reconnect cycle reclassified the outage meanwhile.
		w.mu.Unlock()
		return false
	case !known:
		// The process list stopped answering: back to the plain backoff.
		w.clearAbsentLocked()
		w.mu.Unlock()
		return false
	case app.Name == "":
		w.mu.Unlock()
		return true
	}
	w.clearAbsentLocked()
	w.appeared = true
	w.mu.Unlock()
	if s.logger != nil {
		s.logger.Infof("Gateway: %s started (pid %d); dialling now", app.Name, app.PID)
	}
	s.triggerReconnect()
	return false
}
