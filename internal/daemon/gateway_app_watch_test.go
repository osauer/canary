package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/discover"
)

// No daemon test shells out to ps: the package-wide process query reports
// no information unless a test installs its own (gatewayAppWatch.inspect).
func init() {
	gatewayAppInspector = func(context.Context) (discover.IBKRApp, bool) { return discover.IBKRApp{}, false }
}

// lockedBuffer is a log sink the poller goroutine may write concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// appWatchHarness is a test server whose process list, clock and poller
// ticks the test controls.
type appWatchHarness struct {
	s       *Server
	log     *lockedBuffer
	now     time.Time
	nowMu   sync.Mutex
	app     atomic.Value // discover.IBKRApp
	known   atomic.Bool
	lookups atomic.Int32
	ticks   chan time.Time
}

func newAppWatchHarness(t *testing.T) *appWatchHarness {
	t.Helper()
	h := &appWatchHarness{
		s:     newTestServer(t),
		log:   &lockedBuffer{},
		now:   time.Date(2026, 9, 30, 2, 0, 0, 0, time.UTC),
		ticks: make(chan time.Time),
	}
	h.app.Store(discover.IBKRApp{})
	h.known.Store(true)
	h.s.logger = NewLogger(h.log, "info")
	h.s.now = h.clock
	h.s.gatewayApp.inspect = func(context.Context) (discover.IBKRApp, bool) {
		h.lookups.Add(1)
		return h.app.Load().(discover.IBKRApp), h.known.Load()
	}
	h.s.gatewayApp.tick = func(time.Duration) (<-chan time.Time, func()) { return h.ticks, func() {} }
	// t.Context ends before cleanups run: every poller must be gone by then.
	t.Cleanup(func() { waitPollerGone(t, h.s) })
	return h
}

func waitPollerGone(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.gatewayApp.polling() {
		if time.Now().After(deadline) {
			t.Fatal("process poller still running")
		}
		time.Sleep(time.Millisecond)
	}
}

func (h *appWatchHarness) clock() time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	return h.now
}

func (h *appWatchHarness) advance(d time.Duration) time.Time {
	h.nowMu.Lock()
	defer h.nowMu.Unlock()
	h.now = h.now.Add(d)
	return h.now
}

// refuse runs one connect cycle against host:4001 whose dial is refused, the
// way pkg/ibkr reports it, and returns the published verdict.
func (h *appWatchHarness) refuse(t *testing.T, host string) string {
	t.Helper()
	return h.fail(t, host, refusedDialErr(host))
}

func (h *appWatchHarness) fail(t *testing.T, host string, err error) string {
	t.Helper()
	h.s.attempterFactory = func(discover.Endpoint) connectAttempter { return &fakeAttempter{startErr: err} }
	h.s.connectWithFailover(t.Context(), discover.Endpoint{Host: host, Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned})
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.reconnectFailStreak = 6
	h.s.lastReconnectAttemptAt = h.clock()
	return h.s.lastConnectError
}

func refusedDialErr(host string) error {
	return fmt.Errorf("failed to connect to IBKR at %s:4001: dial tcp %s:4001: connect: %w", host, host, syscall.ECONNREFUSED)
}

// nextDialAfter reports the first whole second after the last attempt at
// which the reconnect gate lets a dial through.
func (h *appWatchHarness) nextDialAfter() time.Duration {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	for d := time.Duration(0); d <= 5*time.Minute; d += time.Second {
		if h.s.reconnectAllowed(h.s.lastReconnectAttemptAt.Add(d)) {
			return d
		}
	}
	return -1
}

func (h *appWatchHarness) absent() (absent, watching, appeared bool) {
	w := &h.s.gatewayApp
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.absent, w.stop != nil, w.appeared
}

// With no IBKR app process on this machine, a refused dial says so plainly,
// the status carries that verdict instead of the raw dial error, and the
// daemon dials once a minute rather than every 15s.
func TestRefusedDialWithNoIBKRAppWaitsForOneToStart(t *testing.T) {
	h := newAppWatchHarness(t)
	verdict := h.refuse(t, "127.0.0.1")

	const want = "no TWS / IB Gateway / IBKR Desktop process running; waiting for one to start (127.0.0.1:4001 refused the connection"
	if !strings.HasPrefix(verdict, want) {
		t.Fatalf("verdict %q, want prefix %q", verdict, want)
	}
	for _, raw := range []string{"connection refused", "dial tcp", "none of 1", "Enable ActiveX"} {
		if strings.Contains(verdict, raw) {
			t.Fatalf("verdict still carries %q: %s", raw, verdict)
		}
	}
	if res := h.s.statusHealthSnapshot(); res.LastError != verdict || res.GatewayPhase != "port_down" {
		t.Fatalf("status last_error %q phase %q", res.LastError, res.GatewayPhase)
	}
	if got := h.log.String(); !strings.Contains(got, "Gateway unavailable: "+want) || strings.Count(got, "Gateway unavailable") != 1 {
		t.Fatalf("log wants one classified WARN, got:\n%s", got)
	}
	if got := h.nextDialAfter(); got != gatewayAppAbsentRedial {
		t.Fatalf("next dial after %s, want %s", got, gatewayAppAbsentRedial)
	}
	if absent, watching, _ := h.absent(); !absent || !watching {
		t.Fatalf("absent=%v watching=%v, want the process poller running", absent, watching)
	}

	// The bulk lane follows the same cadence.
	h.s.mu.Lock()
	h.s.serverCtx = t.Context()
	h.s.breadthConnectFailStreak = 6
	h.s.lastBreadthConnectAttemptAt = h.clock()
	h.s.mu.Unlock()
	h.advance(30 * time.Second)
	if h.s.claimBreadthConnect() {
		t.Fatal("bulk lane dialled 30s into an outage with no IBKR app")
	}
	h.advance(30 * time.Second)
	if !h.s.claimBreadthConnect() {
		t.Fatal("bulk lane did not dial after the 60s cap")
	}
}

// An IBKR app that appears while the daemon waits is dialled at once, not
// at the next minute mark.
func TestAnIBKRAppThatAppearsIsDialledAtOnce(t *testing.T) {
	h := newAppWatchHarness(t)
	var dials atomic.Int32
	h.refuse(t, "127.0.0.1")
	h.s.attempterFactory = func(discover.Endpoint) connectAttempter {
		dials.Add(1)
		return &fakeAttempter{startErr: refusedDialErr("127.0.0.1")}
	}
	h.s.mu.Lock()
	h.s.serverCtx = t.Context()
	h.s.mu.Unlock()

	// A tick send only proves reception, not inspection completion. A second
	// tick could still be inspecting when the app changes, exit the poller,
	// and leave the appearance tick with no receiver. Acknowledge the captured
	// absent observation directly instead; no extra poll remains in flight.
	inspected := make(chan discover.IBKRApp, 2)
	h.s.gatewayApp.mu.Lock()
	inspect := h.s.gatewayApp.inspect
	h.s.gatewayApp.inspect = func(ctx context.Context) (discover.IBKRApp, bool) {
		app, known := inspect(ctx)
		select {
		case inspected <- app:
		case <-ctx.Done():
		}
		return app, known
	}
	h.s.gatewayApp.mu.Unlock()
	sendTick := func() {
		t.Helper()
		select {
		case h.ticks <- h.clock():
		case <-time.After(5 * time.Second):
			t.Fatal("process poller did not receive a tick")
		}
	}
	// Still nothing running: the poller keeps waiting and nothing dials.
	h.advance(10 * time.Second)
	sendTick()
	select {
	case observed := <-inspected:
		if observed.Name != "" {
			t.Fatalf("first poll unexpectedly observed an app: %+v", observed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("process poller did not inspect the absent app")
	}
	if absent, watching, _ := h.absent(); !absent || !watching || dials.Load() != 0 {
		t.Fatalf("absent=%v watching=%v dials=%d after a poll that found nothing", absent, watching, dials.Load())
	}

	h.app.Store(discover.IBKRApp{Name: "IB Gateway", PID: 4242})
	h.advance(10 * time.Second)
	sendTick()
	deadline := time.After(5 * time.Second)
	for dials.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no dial after the IBKR app appeared")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !strings.Contains(h.log.String(), "Gateway: IB Gateway started (pid 4242); dialling now") {
		t.Fatalf("log lacks the app-started line:\n%s", h.log.String())
	}
	waitConnectIdle(t, h.s)
	if absent, watching, _ := h.absent(); absent || watching {
		t.Fatalf("absent=%v watching=%v after the app appeared", absent, watching)
	}
	// The app is up but its port still refuses (it is logging in): back to
	// the ordinary backoff, with the app-specific hint.
	h.s.mu.Lock()
	verdict := h.s.lastConnectError
	h.s.mu.Unlock()
	if !strings.Contains(verdict, "IB Gateway is running (pid 4242) but its API port is closed") {
		t.Fatalf("verdict after the app appeared: %q", verdict)
	}
}

func waitConnectIdle(t *testing.T, s *Server) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		busy := s.connectInFlight
		s.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("reconnect still in flight")
}

// An app that runs but refuses on its port is starting or logging in: keep
// the 15s cadence and say which app and what to check.
func TestRefusedDialWithAnIBKRAppRunningKeepsTheNormalBackoff(t *testing.T) {
	h := newAppWatchHarness(t)
	h.app.Store(discover.IBKRApp{Name: "TWS", PID: 300})
	verdict := h.refuse(t, "localhost")
	for _, want := range []string{"localhost:4001 refused the connection; TWS is running (pid 300)", "Enable ActiveX and Socket Clients"} {
		if !strings.Contains(verdict, want) {
			t.Fatalf("verdict lacks %q: %s", want, verdict)
		}
	}
	if got := h.nextDialAfter(); got != reconnectBackoffMax {
		t.Fatalf("next dial after %s, want %s", got, reconnectBackoffMax)
	}
	if _, watching, _ := h.absent(); watching {
		t.Fatal("process poller started although an IBKR app runs")
	}
}

// Without process information — a remote gateway host, a failed process
// lookup, or a failure other than a refused dial — reconnect behaves exactly
// as before: raw error, 15s cadence, no poller.
func TestReconnectWithoutProcessInformationIsUnchanged(t *testing.T) {
	timeout := fmt.Errorf("failed to connect to IBKR at 127.0.0.1:4001: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name        string
		host        string
		err         error
		psFails     bool
		wantLookups int32
	}{
		{name: "remote host", host: "10.0.0.5", err: refusedDialErr("10.0.0.5")},
		{name: "process lookup fails", host: "127.0.0.1", err: refusedDialErr("127.0.0.1"), psFails: true, wantLookups: 1},
		{name: "not a refusal", host: "127.0.0.1", err: errors.New("failed to connect to IBKR at 127.0.0.1:4001: dial tcp 127.0.0.1:4001: connect: no route to host")},
		{name: "context error", host: "127.0.0.1", err: timeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAppWatchHarness(t)
			h.known.Store(!tc.psFails)
			verdict := h.fail(t, tc.host, tc.err)
			if strings.Contains(verdict, "process running") || strings.Contains(verdict, "is running (pid") {
				t.Fatalf("verdict classified without process information: %s", verdict)
			}
			if got := h.lookups.Load(); got != tc.wantLookups {
				t.Fatalf("process lookups %d, want %d", got, tc.wantLookups)
			}
			if got := h.nextDialAfter(); got != reconnectBackoffMax {
				t.Fatalf("next dial after %s, want %s", got, reconnectBackoffMax)
			}
			if absent, watching, _ := h.absent(); absent || watching {
				t.Fatalf("absent=%v watching=%v without process information", absent, watching)
			}
		})
	}
}

// A process lookup that starts failing mid-outage hands pacing back to the
// ordinary backoff instead of leaving the daemon on the stretched cadence.
func TestAFailingProcessLookupEndsTheStretchedCadence(t *testing.T) {
	h := newAppWatchHarness(t)
	h.refuse(t, "127.0.0.1")
	h.known.Store(false)
	h.advance(10 * time.Second)
	if h.s.pollGatewayApp(t.Context()) {
		t.Fatal("poller kept polling after the process lookup failed")
	}
	if absent, _, appeared := h.absent(); absent || appeared {
		t.Fatalf("absent=%v appeared=%v after a failed lookup", absent, appeared)
	}
	if got := h.nextDialAfter(); got != reconnectBackoffMax {
		t.Fatalf("next dial after %s, want %s", got, reconnectBackoffMax)
	}
}

// Refused dials consult ps at most once per verdict TTL.
func TestRefusedDialsReuseTheProcessVerdict(t *testing.T) {
	h := newAppWatchHarness(t)
	h.app.Store(discover.IBKRApp{Name: "IB Gateway", PID: 4242})
	for range 5 {
		h.refuse(t, "127.0.0.1")
		h.advance(5 * time.Second)
	}
	if got := h.lookups.Load(); got != 1 {
		t.Fatalf("process lookups within the TTL: %d, want 1", got)
	}
	h.advance(gatewayAppVerdictTTL)
	h.refuse(t, "127.0.0.1")
	if got := h.lookups.Load(); got != 2 {
		t.Fatalf("process lookups after the TTL: %d, want 2", got)
	}
}

// A completed handshake ends the classification.
func TestAConnectedGatewayEndsTheNoAppClassification(t *testing.T) {
	h := newAppWatchHarness(t)
	h.refuse(t, "127.0.0.1")
	h.s.attempterFactory = func(discover.Endpoint) connectAttempter { return &fakeAttempter{connectOk: true} }
	h.s.connectWithFailover(t.Context(), discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned})
	if absent, _, _ := h.absent(); absent {
		t.Fatal("still classified as no IBKR app after a completed handshake")
	}
	if h.s.pollGatewayApp(t.Context()) {
		t.Fatal("poller kept polling after the gateway connected")
	}
}

// The poller stops without waiting for its next tick when the outage is
// reclassified, and when the server context ends.
func TestTheProcessPollerStopsWithTheClassificationAndTheServer(t *testing.T) {
	h := newAppWatchHarness(t)
	h.refuse(t, "127.0.0.1")
	if !h.s.gatewayApp.polling() {
		t.Fatal("no poller during an outage with no IBKR app")
	}
	h.s.gatewayApp.reset()
	waitPollerGone(t, h.s)

	ctx, cancel := context.WithCancel(t.Context())
	h.s.gatewayApp.mu.Lock()
	h.s.gatewayApp.checkedAt = time.Time{}
	h.s.gatewayApp.mu.Unlock()
	if _, ok := h.s.classifyRefusedDial(ctx, discover.Endpoint{Host: "127.0.0.1", Port: 4001}, refusedDialErr("127.0.0.1")); !ok || !h.s.gatewayApp.polling() {
		t.Fatal("no poller after the second classification")
	}
	cancel()
	waitPollerGone(t, h.s)
}

// A later cycle whose failure is not an explained refusal — here a listener
// that never handshakes — drops the stretched cadence an earlier cycle set.
func TestAnUnexplainedFailureEndsTheNoAppCadence(t *testing.T) {
	h := newAppWatchHarness(t)
	h.refuse(t, "127.0.0.1")
	h.fail(t, "127.0.0.1", errors.New("failed to connect to IBKR (client 15)"))
	if absent, polling, _ := h.absent(); absent || polling {
		t.Fatalf("absent=%v polling=%v after an unexplained failure", absent, polling)
	}
	if got := h.nextDialAfter(); got != reconnectBackoffMax {
		t.Fatalf("next dial after %s, want %s", got, reconnectBackoffMax)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "[::1]": true, "localhost": true, "LocalHost": true, "127.0.0.2": true,
		"10.0.0.5": false, "gateway.lan": false, "": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}
