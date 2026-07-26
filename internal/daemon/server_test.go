package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

// shortTempDir returns a tempdir under /tmp so Unix socket paths stay
// inside macOS's ~104-char SUN_LEN limit. t.TempDir() builds paths under
// /var/folders/... which routinely exceeds that.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ibkrd-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// openSocket must clean up a stale socket inode left by a crashed
// predecessor and bind a fresh listener. Setting SetUnlinkOnClose(false)
// before closing the staging listener forces the inode to survive, so
// the test deterministically exercises the stale-socket branch
// (previously a fallback to a regular-file path made either branch
// satisfy the assertion).
func TestOpenSocketRemovesStaleSocketFile(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	staleListener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	ul, ok := staleListener.(*net.UnixListener)
	if !ok {
		t.Fatalf("expected *net.UnixListener, got %T", staleListener)
	}
	ul.SetUnlinkOnClose(false)
	_ = staleListener.Close()

	fi, err := os.Stat(sockPath)
	if err != nil {
		t.Fatalf("staged stale socket missing after Close: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("staged path is not a socket inode: mode=%v", fi.Mode())
	}

	srv := &Server{socketPath: sockPath}
	if err := srv.openSocket(); err != nil {
		t.Fatalf("openSocket: %v", err)
	}
	defer func() {
		if srv.listener != nil {
			_ = srv.listener.Close()
		}
	}()
	if srv.listener == nil {
		t.Fatal("listener nil after openSocket")
	}
	// Round-trip dial verifies the fresh listener is actually serving on
	// the path; without this, an openSocket that misorders Listen→Chmod
	// could leave a half-initialised state that the listener-nil check
	// alone would miss.
	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial fresh listener: %v", err)
	}
	_ = conn.Close()
}

// A Server that never opened its listener (e.g. it lost the instance-lock
// race) must not delete the socket file on Stop(). Pre-fix, the loser's
// deferred srv.Stop() would unlink the winner's live socket out from
// underneath it, breaking the running daemon.
func TestStopDoesNotRemoveSocketWhenListenerNeverOpened(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	// Simulate the winner: a real socket file that should survive.
	winner, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed winner socket: %v", err)
	}
	defer winner.Close()

	// Simulate the loser: a Server constructed with the same socketPath
	// but no listener (because it never reached openSocket).
	loser := &Server{
		socketPath: sockPath,
		streams:    map[string]context.CancelFunc{},
	}
	loser.Stop()

	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("loser.Stop() removed the winner's socket: %v", err)
	}
}

// If a peer is actively serving on the socket, openSocket must refuse to
// evict it. This is belt-and-suspenders; the instance flock is the real
// guard, but a stuck flock + live socket should still be diagnosed clearly
// rather than ripping the socket out from under the live peer.
func TestOpenSocketRefusesToEvictLivePeer(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	livePeer, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("seed live peer: %v", err)
	}
	defer livePeer.Close()

	srv := &Server{socketPath: sockPath}
	err = srv.openSocket()
	if err == nil {
		t.Fatalf("expected openSocket to refuse evicting a live peer")
	}
	if !strings.Contains(err.Error(), "already serving") {
		t.Fatalf("expected 'already serving' diagnostic, got %v", err)
	}
}

// dispatch must report terminal=true for streaming RPCs. serveConn relies
// on this to return out of its read loop and let defer close the conn,
// which in turn unblocks the EOF watcher inside the streaming handler.
// Pre-fix, dispatch returned no terminal signal and serveConn would loop
// back into ReadBytes — but the streaming handler hadn't returned yet
// because it had no per-conn ctx tied to the read side, so the
// subscription leaked.
func TestDispatchQuoteSubscribeReportsTerminal(t *testing.T) {
	t.Parallel()
	srv := &Server{
		cfg:      &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4001), ClientID: new(15)}},
		endpoint: discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15},
		streams:  map[string]context.CancelFunc{},
		logger:   NewLogger(&bytes.Buffer{}, "error"),
	}
	srv.installSubs()

	// No connector wired → handleQuoteSubscribe takes the
	// gateway-unavailable early-exit, but the dispatch still has to
	// declare itself terminal so serveConn cleans up the conn.
	params, _ := json.Marshal(rpc.QuoteSubscribeParams{Contract: rpc.ContractParams{Symbol: "AAPL"}})
	req := &rpc.Request{ID: "test-1", Method: rpc.MethodQuoteSubscribe, Params: params}

	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	r := bufio.NewReader(strings.NewReader(""))

	terminal := srv.dispatch(context.Background(), req, enc, r)
	if !terminal {
		t.Fatalf("expected dispatch to report terminal=true for MethodQuoteSubscribe")
	}
}

// unaryDeadline picks a per-method budget that stays under the matching
// CLI per-invocation deadline (cmd/canary/main.go) so the daemon's classified
// error reaches the user before the socket times out, while leaving the
// streaming method (quote.subscribe) without a deadline. Locks two
// invariants: every unary method has d > 0, and d stays under the CLI's
// budget for that command. Most commands share the default 60s CLI budget;
// `scan`, `technical`, and `brief` have a 90s budget because cold-start
// off-hours scanner warmup, multi-symbol daily-history fan-out, and brief's
// multi-source composition are genuinely longer than other paths.
func TestUnaryDeadlineCoversAllUnaryMethods(t *testing.T) {
	t.Parallel()
	const cliDefault = 60 * time.Second
	const cliLong = 90 * time.Second

	cases := []struct {
		method    string
		cliBudget time.Duration
	}{
		{rpc.MethodAccountSummary, cliDefault},
		{rpc.MethodPositionsList, cliDefault},
		{rpc.MethodQuoteSnapshot, cliDefault},
		{rpc.MethodChainFetch, cliDefault},
		{rpc.MethodChainExpiries, cliDefault},
		{rpc.MethodScanRun, cliLong},
		{rpc.MethodScanList, cliDefault},
		{rpc.MethodScanParams, cliDefault},
		{rpc.MethodHistoryDaily, cliDefault},
		{rpc.MethodTechnical, cliLong},
		{rpc.MethodMarketCalendar, cliDefault},
		{rpc.MethodBreadthSPX, cliDefault},
		{rpc.MethodGammaZeroSPX, cliDefault},
		{rpc.MethodRegimeSnapshot, cliDefault},
		{rpc.MethodAlertCandidates, cliDefault},
		{rpc.MethodAlertStatus, cliDefault},
		{rpc.MethodBriefSnapshot, cliLong},
		{rpc.MethodBriefAck, cliLong},
		{rpc.MethodNudgesSnapshot, cliDefault},
		{rpc.MethodNudgesCutoverReview, cliDefault},
		{rpc.MethodReconStatus, cliDefault},
		{rpc.MethodReconCheck, cliDefault},
		{rpc.MethodStatusHealth, cliDefault},
		{rpc.MethodTradingStatus, cliDefault},
		{rpc.MethodSettingsGet, cliDefault},
		{rpc.MethodSettingsUpdate, cliDefault},
		{rpc.MethodOrdersOpen, cliDefault},
		{rpc.MethodOrdersHistory, cliDefault},
		{rpc.MethodOrderStatus, cliDefault},
		{rpc.MethodOrderPreview, cliDefault},
		{rpc.MethodCancel, cliDefault},
		{rpc.MethodOrderPlace, cliDefault},
		{rpc.MethodOrderModify, cliDefault},
		{rpc.MethodOrderCancel, cliDefault},
		{rpc.MethodPurgeStatus, cliDefault},
		{rpc.MethodPurgeExecute, cliDefault},
		{rpc.MethodPurgeRestorePreview, cliDefault},
		{rpc.MethodPurgeRestoreExecute, cliDefault},
	}
	for _, tc := range cases {
		d := unaryDeadline(tc.method)
		if d <= 0 {
			t.Errorf("unaryDeadline(%q) = %s, want >0 (every unary method needs a per-request timeout)", tc.method, d)
		}
		if d >= tc.cliBudget {
			t.Errorf("unaryDeadline(%q) = %s, must stay under CLI budget %s so daemon errors first", tc.method, d, tc.cliBudget)
		}
	}
	if d := unaryDeadline(rpc.MethodQuoteSubscribe); d != 0 {
		t.Errorf("unaryDeadline(%q) = %s, want 0 (streaming methods must not have a deadline)", rpc.MethodQuoteSubscribe, d)
	}
}

func TestOrderPreviewUnaryDeadlineCoversBrokerWhatIf(t *testing.T) {
	t.Parallel()

	if got := unaryDeadline(rpc.MethodOrderPreview); got < 50*time.Second || got >= 60*time.Second {
		t.Fatalf("order.preview deadline = %s, want enough room for broker WhatIf but below the CLI 60s ceiling", got)
	}
}

func TestHistoryDailyUnaryDeadlineCoversInteractiveHMDS(t *testing.T) {
	t.Parallel()

	if got := unaryDeadline(rpc.MethodHistoryDaily); got < 50*time.Second || got >= 60*time.Second {
		t.Fatalf("history.daily deadline = %s, want room for cold contract details + HMDS below the CLI 60s ceiling", got)
	}
}

// requestCtx must derive a child context carrying the per-method unary
// deadline, and must NOT leak that deadline back into the caller's
// parent context. Tests the actual deadline-attachment behaviour
// dispatch relies on — previously this test asserted only that
// context.Background() has no deadline, which is true by construction.
func TestRequestCtxAppliesUnaryDeadline(t *testing.T) {
	t.Parallel()

	parent := context.Background()
	ctx, cancel := requestCtx(parent, rpc.MethodChainFetch)
	defer cancel()

	dl, has := ctx.Deadline()
	if !has {
		t.Fatal("requestCtx returned a ctx with no deadline for a unary method")
	}
	want := unaryDeadline(rpc.MethodChainFetch)
	if d := time.Until(dl); d <= 0 || d > want+time.Second {
		t.Fatalf("ctx deadline = %s from now, want ~%s", d, want)
	}
	if _, leaked := parent.Deadline(); leaked {
		t.Fatal("requestCtx leaked the deadline back into the caller's ctx")
	}
}

// Streaming methods (no unary deadline) must get the parent context
// through unchanged so the stream's own lifetime owns cancellation.
func TestRequestCtxNoDeadlineForStreamingMethod(t *testing.T) {
	t.Parallel()

	parent := t.Context()

	ctx, cancel := requestCtx(parent, rpc.MethodQuoteSubscribe)
	defer cancel()

	if _, has := ctx.Deadline(); has {
		t.Fatal("streaming method's ctx unexpectedly carries a deadline")
	}
}

// Server.Start must open the Unix socket and start serving before the
// gateway handshake starts — otherwise a slow or unreachable gateway blocks
// `canary status` for 30-40s during the IBKR pool's TCP timeouts. The initial
// connection must also be marked in-flight before the accept loop is exposed;
// otherwise an immediate status request launches reconnectFlow alongside the
// initial handshake and IBKR rejects the duplicate client ID. The fake
// attempter checks the Unix socket inode synchronously on entry to Start and
// then blocks. Dial reachability is covered independently by the openSocket
// test; keeping these assertions at the publication boundary avoids a
// load-sensitive kernel dial in the parallel race suite.
func TestStartOpensSocketBeforeGatewayHandshake(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	tlsFalse := false
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99), TLS: &tlsFalse},
	}
	// Disable idle exit for this ordering test. A short idle timer makes the
	// fixture itself load-sensitive: under a parallel race run it can close the
	// listener before the deliberately blocked connect goroutine is scheduled.
	cfg.Daemon.SetIdleTimeout(0)

	srv := New(Options{
		Config:            cfg,
		SocketPath:        sockPath,
		Version:           "test",
		Logger:            NewLogger(&bytes.Buffer{}, "error"),
		StateDatabasePath: filepath.Join(dir, "daemon.db"),
	})
	// Hermetic journal: New() resolves the host's XDG state path, and a real
	// open order there would (correctly) veto idle shutdown via the
	// open-orders background task.
	srv.orderJournal = newOrderJournalStore(filepath.Join(dir, "order-journal.jsonl"))
	acceptCheck := make(chan error, 1)
	srv.initialAcceptLoopStartedForTest = func() {
		srv.mu.Lock()
		inFlight := srv.connectInFlight
		srv.mu.Unlock()
		if !inFlight {
			acceptCheck <- errors.New("RPC accept loop exposed before initial connection claimed the in-flight gate")
			return
		}
		acceptCheck <- nil
	}
	startCheck := make(chan error, 1)
	srv.attempterFactory = func(_ discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			blockUntilCtxDone: true,
			startCheck: func() error {
				fi, err := os.Stat(sockPath)
				if err != nil {
					return err
				}
				if fi.Mode()&os.ModeSocket == 0 {
					return errors.New("daemon socket was not published before gateway handshake")
				}
				return nil
			},
			startCheckResult: startCheck,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startReturned := make(chan error, 1)
	go func() {
		startReturned <- srv.Start(ctx)
	}()

	select {
	case err := <-acceptCheck:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-startReturned:
		t.Fatalf("Start returned before exposing the accept loop: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("accept loop was not exposed")
	}

	select {
	case err := <-startCheck:
		if err != nil {
			t.Fatalf("gateway handshake started before daemon socket was published: %v", err)
		}
	case err := <-startReturned:
		t.Fatalf("Start returned before gateway handshake: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("gateway handshake did not start")
	}

	// Release the deliberately blocked handshake and let Start return cleanly.
	cancel()
	select {
	case <-startReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s of cancellation")
	}
	srv.Stop()
}

// TestStartDoesNotLaunchBreadthBeforePostConnect pins the v0.27.0
// regression: the breadth engine's bootstrap fan-out must not start
// until the gateway handshake completes. Pre-v0.27.1 the engine's
// Run() was launched from Server.Start() in parallel with the async
// connector goroutine; every per-name FetchDaily returned "no gateway
// connector" instantly and the fan-out finalised in milliseconds with
// Coverage=0, which then poisoned the cache. v0.27.1 moved the launch
// to postConnectSetup behind a sync.Once. This test asserts the
// invariant directly: with a handshake that blocks indefinitely, the
// breadth fetcher must never be invoked.
func TestStartDoesNotLaunchBreadthBeforePostConnect(t *testing.T) {
	t.Parallel()

	// Find a closed TCP port so discovery resolves to something
	// concrete; the fake attempter intercepts the actual handshake.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := probe.Addr().(*net.TCPAddr)
	_ = probe.Close()

	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	tlsFalse := false
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(addr.Port), ClientID: new(99), TLS: &tlsFalse},
	}
	cfg.Daemon.SetIdleTimeout(2 * time.Second)

	srv := New(Options{
		Config:            cfg,
		SocketPath:        sockPath,
		Version:           "test",
		Logger:            NewLogger(&bytes.Buffer{}, "error"),
		StateDatabasePath: filepath.Join(dir, "daemon.db"),
	})
	// Hermetic journal — see TestStartOpensSocketBeforeGatewayHandshake.
	srv.orderJournal = newOrderJournalStore(filepath.Join(shortTempDir(t), "order-journal.jsonl"))

	// Replace the production breadth engine with a test one whose
	// fetcher records every invocation. If postConnectSetup correctly
	// gates breadth.Run() behind a successful handshake, this fetcher
	// stays at zero calls for the entire duration of the test (because
	// the handshake never completes). Any call here is a regression
	// of the v0.27.0 bootstrap race.
	fakeFetcher := &spx.FakeBarFetcher{
		Bars: map[string][]spx.Bar{},
	}
	srv.breadth = spx.New(spx.NewStore(t.TempDir()), fakeFetcher, spx.Options{Workers: 4})

	// Attempter blocks until ctx is cancelled — models the
	// "TCP accepted but handshake never completes" path the v0.27.0
	// race hid in. postConnectSetup is reached only after Start
	// returns true here, so the bootstrap launch must wait.
	srv.attempterFactory = func(_ discover.Endpoint) connectAttempter {
		return &fakeAttempter{blockUntilCtxDone: true}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startReturned := make(chan error, 1)
	go func() {
		startReturned <- srv.Start(ctx)
	}()

	// 300 ms is comfortably past the daemon's socket-open + initial
	// connect-goroutine launch phase, and well inside the
	// perCandidateConnectBudget (25 s default) — the fake attempter
	// is still blocking, so postConnectSetup hasn't run.
	time.Sleep(300 * time.Millisecond)

	if srv.breadth.IsRefreshing() {
		t.Error("breadth.IsRefreshing() == true before gateway handshake completed (regression of v0.27.0 bootstrap race)")
	}
	if n := fakeFetcher.CallCount(); n > 0 {
		t.Errorf("breadth fetcher invoked %d times before gateway handshake — any invocation is a v0.27.0 regression", n)
	}

	cancel()
	select {
	case <-startReturned:
		// good
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s after ctx cancel")
	}
	srv.Stop()
}

// runIdleWatcher must return when the idle timer fires with no active
// conns. Pre-fix, the idle case closed the listener directly and returned;
// the surrounding Start() then returned to cmd/canaryd's main, which blocked
// on <-ctx.Done() forever, leaking a zombie process that held the instance
// lock. The fix moves listener teardown into Start() (via closeListener)
// so the idle path can simply return and let the deferred Stop() finish
// cleanup. This test pins the new contract: idle fire returns promptly.
func TestRunIdleWatcherReturnsOnIdleFire(t *testing.T) {
	t.Parallel()
	cfg := &config.Resolved{}
	cfg.Daemon.SetIdleTimeout(50 * time.Millisecond)
	srv := &Server{
		cfg:      cfg,
		streams:  map[string]context.CancelFunc{},
		idleStop: make(chan struct{}),
		logger:   NewLogger(&bytes.Buffer{}, "error"),
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.runIdleWatcher(context.Background())
	}()

	select {
	case <-done:
		// pass — idle watcher returned
	case <-time.After(2 * time.Second):
		t.Fatal("runIdleWatcher did not return within 2s of idle timer firing")
	}
}

// runIdleWatcher must defer shutdown while s.isBusy() reports true. The
// breadth engine's cold-start fan-out takes ~60 min (IBKR's pacing limit
// caps us at 6 historical-data requests per minute sustained); the
// default 5-minute idle window would otherwise kill the daemon
// mid-bootstrap and the indicator would never land. Pinned by v0.27.2
// — v0.27.1 shipped without this check and a fresh autospawned daemon
// idled out at the 5-minute mark, ~55 minutes before the bootstrap
// could complete.
func TestRunIdleWatcherDefersShutdownWhileBreadthRefreshing(t *testing.T) {
	t.Parallel()

	cfg := &config.Resolved{}
	cfg.Daemon.SetIdleTimeout(40 * time.Millisecond)

	// Build a real spx.Engine wired to a fake fetcher whose latency
	// keeps Refresh() in flight long enough to observe the
	// idle-deferral. 400 ms is comfortably above the watcher's
	// 40 ms tick period — the test asserts the watcher reset its
	// timer instead of returning. Workers is set high so the full
	// 503-name baked-in fan-out completes in one parallel batch
	// (~400 ms wall time, not 503 × 400 ms / 6).
	fetcher := &spx.FakeBarFetcher{
		Bars:    map[string][]spx.Bar{"AAA": {{Date: "2026-05-18", Close: 100}}},
		Latency: 400 * time.Millisecond,
	}
	engine := spx.New(spx.NewStore(t.TempDir()), fetcher, spx.Options{Workers: 1024})

	// Kick Refresh in the background so IsRefreshing() returns true
	// for ~250 ms while the watcher's idle timer fires.
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_ = engine.Refresh(context.Background())
	}()
	// Give Refresh a moment to acquire refreshMu and set
	// e.refreshing = true. Without this, the watcher's first tick
	// could fire before the refresh is observably in flight.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !engine.IsRefreshing() {
		time.Sleep(2 * time.Millisecond)
	}
	if !engine.IsRefreshing() {
		t.Fatal("engine.IsRefreshing() never became true; test setup is broken")
	}

	srv := &Server{
		cfg:      cfg,
		streams:  map[string]context.CancelFunc{},
		idleStop: make(chan struct{}),
		logger:   NewLogger(&bytes.Buffer{}, "error"),
		breadth:  engine,
	}
	if !srv.isBusy() {
		t.Fatal("srv.isBusy() should be true while the engine refresh is in flight")
	}

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		srv.runIdleWatcher(context.Background())
	}()

	// During the busy window (Refresh still running), the watcher
	// must NOT return. Wait long enough for at least two idle-timer
	// firings (40 ms × 2 = 80 ms), well inside the 250 ms refresh
	// latency.
	select {
	case <-watcherDone:
		t.Fatal("runIdleWatcher returned while breadth refresh was in flight")
	case <-time.After(120 * time.Millisecond):
		// good — watcher is still ticking, deferring shutdown
	}

	// Once the refresh finishes (and IsRefreshing flips false), the
	// next idle-timer tick should observe isBusy()==false and return.
	<-refreshDone
	select {
	case <-watcherDone:
		// good — watcher returned promptly after the busy condition
		// cleared
	case <-time.After(2 * time.Second):
		t.Fatal("runIdleWatcher did not return within 2 s after the busy condition cleared")
	}
}

// runIdleWatcher must also defer shutdown between breadth bootstrap
// attempts. A below-threshold refresh persists partial windows, then Run
// sleeps for the IBKR bucket-refill delay before retrying. That sleep is
// active bootstrap work; if the daemon exits there, breadth may never
// converge on a fresh autospawn.
func TestRunIdleWatcherDefersShutdownWhileBreadthRetryPending(t *testing.T) {
	t.Parallel()

	cfg := &config.Resolved{}
	cfg.Daemon.SetIdleTimeout(40 * time.Millisecond)

	now := time.Date(2026, 5, 18, 21, 0, 0, 0, time.UTC)
	series := func(start float64) []spx.Bar {
		bars := make([]spx.Bar, spx.WindowSize)
		for i := range bars {
			date := now.AddDate(0, 0, -(spx.WindowSize - 1 - i))
			bars[i] = spx.Bar{Date: date.Format("2006-01-02"), Close: start + float64(i)}
		}
		return bars
	}
	members := []string{"OK1", "OK2", "OK3", "OK4", "OK5", "F1", "F2", "F3", "F4", "F5"}
	fetcher := &spx.FakeBarFetcher{
		Bars: map[string][]spx.Bar{
			"OK1": series(100),
			"OK2": series(110),
			"OK3": series(120),
			"OK4": series(130),
			"OK5": series(140),
		},
		Errors: map[string]error{
			"F1": errors.New("gateway: pacing"),
			"F2": errors.New("gateway: pacing"),
			"F3": errors.New("gateway: pacing"),
			"F4": errors.New("gateway: pacing"),
			"F5": errors.New("gateway: pacing"),
		},
	}
	engine := spx.New(spx.NewStore(t.TempDir()), fetcher, spx.Options{
		Clock:   func() time.Time { return now },
		Workers: 4,
	})
	engine.SetMembers(members)

	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		engine.Run(runCtx)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		cov, mc := engine.LastRefreshCoverage()
		if cov == 5 && mc == 10 && !engine.IsRefreshing() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cov, mc := engine.LastRefreshCoverage()
	if cov != 5 || mc != 10 {
		cancelRun()
		<-runDone
		t.Fatalf("bootstrap coverage: want (5, 10), got (%d, %d)", cov, mc)
	}
	if !engine.IsBusy() {
		cancelRun()
		<-runDone
		t.Fatal("engine should stay busy while below-threshold retry is pending")
	}

	srv := &Server{
		cfg:      cfg,
		streams:  map[string]context.CancelFunc{},
		idleStop: make(chan struct{}),
		logger:   NewLogger(&bytes.Buffer{}, "error"),
		breadth:  engine,
	}

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		srv.runIdleWatcher(context.Background())
	}()

	select {
	case <-watcherDone:
		cancelRun()
		<-runDone
		t.Fatal("runIdleWatcher returned while breadth retry was pending")
	case <-time.After(120 * time.Millisecond):
	}

	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("breadth Run did not exit after cancel")
	}
	select {
	case <-watcherDone:
	case <-time.After(2 * time.Second):
		t.Fatal("runIdleWatcher did not return after breadth retry state cleared")
	}
}

// TestIsBusyIncludesGammaCompute pins v0.27.4: the idle-watcher's busy
// predicate must observe gamma compute as well as breadth refresh. At
// v0.27.3 only breadth was checked; gamma is faster than the 5-min
// idle window in practice so the gap never bit in observable usage,
// but the architectural hole is the same shape — a long-running
// daemon-internal task can outlive the CLI invocation that triggered
// it, and the watcher must defer shutdown for ALL such tasks.
func TestIsBusyIncludesGammaCompute(t *testing.T) {
	t.Parallel()
	srv := &Server{
		zeroGamma: newGammaZeroCache(),
	}
	if srv.isBusy() {
		t.Error("isBusy() should be false with no gamma compute in flight")
	}
	// Inject a synthetic in-flight computation: open `done` channel
	// means isDone() returns false, which means IsComputing() returns
	// true. Slot-keyed cache: any in-flight job in any slot counts.
	srv.zeroGamma.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: &gammaComputation{
			sessionKey: "2026-05-19",
			scope:      rpc.GammaZeroScopeCombined,
			startedAt:  time.Now(),
			done:       make(chan struct{}),
		}},
	}
	if !srv.isBusy() {
		t.Error("isBusy() should be true with gamma compute in flight (regression: gamma was not in isBusy() at v0.27.3)")
	}
}

// TestHandleStatusHealthReportsBackgroundTasks pins v0.27.4: the
// HealthResult.BackgroundTasks list reflects daemon-internal long-
// running computes and is always emitted (possibly empty) so consumers
// can rely on `len() == 0` for idle without inferring from absence.
func TestHandleStatusHealthReportsBackgroundTasks(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		version:   "test",
		startedAt: time.Now(),
		zeroGamma: newGammaZeroCache(),
	}

	// Idle daemon → empty but non-nil list. The non-nil contract
	// matters because JSON consumers must be able to do
	// `len(background_tasks) == 0` without dereferencing nil.
	res := srv.handleStatusHealth()
	if res.BackgroundTasks == nil {
		t.Error("BackgroundTasks must be non-nil even when empty")
	}
	if len(res.BackgroundTasks) != 0 {
		t.Errorf("BackgroundTasks should be empty when idle, got %+v", res.BackgroundTasks)
	}

	// Gamma compute in flight → one entry. Synthetic job injected into
	// the combined slot (the canonical cache cell for dashboard callers).
	srv.zeroGamma.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: &gammaComputation{
			sessionKey: "2026-05-19",
			scope:      rpc.GammaZeroScopeCombined,
			startedAt:  time.Now(),
			done:       make(chan struct{}),
		}},
	}
	res = srv.handleStatusHealth()
	if len(res.BackgroundTasks) != 1 || res.BackgroundTasks[0].Name != "gamma-zero" {
		t.Errorf("BackgroundTasks should list gamma-zero only, got %+v", res.BackgroundTasks)
	}

	// Breadth refresh in flight alongside gamma → both listed, in
	// the documented stable order (breadth first, gamma second).
	fakeFetcher := &spx.FakeBarFetcher{
		Bars:    map[string][]spx.Bar{"AAA": {{Date: "2026-05-18", Close: 100}}},
		Latency: 500 * time.Millisecond,
	}
	srv.breadth = spx.New(spx.NewStore(t.TempDir()), fakeFetcher, spx.Options{Workers: 1024})
	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_ = srv.breadth.Refresh(context.Background())
	}()
	// Wait for the refresh to be observably in flight before snapping
	// the health envelope.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !srv.breadth.IsRefreshing() {
		time.Sleep(2 * time.Millisecond)
	}
	if !srv.breadth.IsRefreshing() {
		t.Fatal("breadth refresh never reported IsRefreshing — test setup is broken")
	}

	res = srv.handleStatusHealth()
	if len(res.BackgroundTasks) != 2 {
		t.Fatalf("BackgroundTasks should list both tasks, got %+v", res.BackgroundTasks)
	}
	if res.BackgroundTasks[0].Name != "breadth-spx" || res.BackgroundTasks[1].Name != "gamma-zero" {
		t.Errorf("BackgroundTasks order: want [breadth-spx, gamma-zero], got %+v", res.BackgroundTasks)
	}

	<-refreshDone
}

// TestBackgroundTasksRegistry_isBusyAndHandlerAgree pins the invariant
// that isBusy() and handleStatusHealth.BackgroundTasks never disagree —
// both derive from s.backgroundTasks(). Drift here was the next likely
// "v0.27.x lifecycle bug" before the registry consolidated them.
func TestBackgroundTasksRegistry_isBusyAndHandlerAgree(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		version:   "test",
		startedAt: time.Now(),
		zeroGamma: newGammaZeroCache(),
	}

	// 1. Idle daemon — both surfaces report idle.
	if srv.isBusy() {
		t.Error("idle daemon: isBusy() should be false")
	}
	if len(srv.handleStatusHealth().BackgroundTasks) != 0 {
		t.Error("idle daemon: BackgroundTasks should be empty")
	}

	// 2. Inject a gamma compute. Both surfaces flip. The synthetic job
	// lives in the combined slot — IsComputing iterates all slots, so
	// "any scope busy = cache busy" is the invariant under test.
	fakeJob := &gammaComputation{
		sessionKey: "2026-05-19",
		scope:      rpc.GammaZeroScopeCombined,
		startedAt:  time.Now(),
		done:       make(chan struct{}),
	}
	srv.zeroGamma.slots = map[string]*gammaSlot{
		rpc.GammaZeroScopeCombined: {current: fakeJob},
	}
	if !srv.isBusy() {
		t.Error("with gamma in flight: isBusy() should be true")
	}
	if got := srv.handleStatusHealth().BackgroundTasks; len(got) != 1 || got[0].Name != "gamma-zero" {
		t.Errorf("with gamma in flight: BackgroundTasks=%+v, want [gamma-zero]", got)
	}

	// 3. Mark gamma done. Both surfaces return to idle.
	close(fakeJob.done)
	if srv.isBusy() {
		t.Error("after gamma done: isBusy() should be false")
	}
	if got := srv.handleStatusHealth().BackgroundTasks; len(got) != 0 {
		t.Errorf("after gamma done: BackgroundTasks=%+v, want []", got)
	}

	// 4. Flag regime-prewarm in flight. Registry must surface it.
	srv.regimePrewarming.Store(true)
	if !srv.isBusy() {
		t.Error("with regime-prewarm in flight: isBusy() should be true")
	}
	if got := srv.handleStatusHealth().BackgroundTasks; len(got) != 1 || got[0].Name != "regime-prewarm" {
		t.Errorf("with regime-prewarm in flight: BackgroundTasks=%+v, want [regime-prewarm]", got)
	}
	srv.regimePrewarming.Store(false)
	if srv.isBusy() {
		t.Error("after regime-prewarm flag cleared: isBusy() should be false")
	}
}

// closeListener must be idempotent and safe to call when the listener was
// never opened (loser of the lock race) or has already been closed by an
// idle-shutdown path.
func TestCloseListenerIdempotent(t *testing.T) {
	t.Parallel()
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	srv.closeListener() // never opened — must not panic
	srv.closeListener() // still nil — must not panic

	dir := shortTempDir(t)
	srv.socketPath = filepath.Join(dir, "ibkrd.sock")
	if err := srv.openSocket(); err != nil {
		t.Fatalf("openSocket: %v", err)
	}
	srv.closeListener() // real close
	srv.closeListener() // already nil — must not panic

	if _, err := os.Stat(srv.socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file should be unlinked after closeListener; stat err=%v", err)
	}
}

// End-to-end check that idle-fire → Start returns → Stop releases all
// resources. Bypasses startConnector/lock acquisition (which need a real
// gateway / process layer) and exercises just the listener+idle plumbing
// that the zombie bug lived in.
func TestServerIdleShutdownReleasesListenerAndSocket(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	cfg := &config.Resolved{}
	cfg.Daemon.SetIdleTimeout(50 * time.Millisecond)
	srv := &Server{
		cfg:        cfg,
		socketPath: filepath.Join(dir, "ibkrd.sock"),
		streams:    map[string]context.CancelFunc{},
		idleStop:   make(chan struct{}),
		logger:     NewLogger(&bytes.Buffer{}, "error"),
	}
	if err := srv.openSocket(); err != nil {
		t.Fatalf("openSocket: %v", err)
	}

	// Drive the same sequence Start runs after wiring is up.
	go srv.acceptLoop(context.Background(), srv.listener)
	srv.runIdleWatcher(context.Background())
	srv.closeListener() // matches Start's post-watcher cleanup

	// Allow acceptLoop a moment to observe net.ErrClosed and exit.
	time.Sleep(50 * time.Millisecond)

	srv.Stop() // matches cmd/canaryd's deferred Stop

	if _, err := os.Stat(srv.socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after idle shutdown + Stop; stat err=%v", err)
	}
	if srv.listener != nil {
		t.Fatal("listener should be nil after Stop")
	}
}

// handshakeWatchdog publishes a hint to lastConnectError when isConnected
// is still false after the delay. Function-pointer abstraction lets us
// drive this with no real Connector — exactly the testability story the
// #2 issue brief specified.
func TestHandshakeWatchdog_PublishesHintAfterDelay(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 4001}
	srv.handshakeWatchdog(context.Background(), func() bool { return false }, 1*time.Millisecond, ep)
	srv.mu.Lock()
	got := srv.lastConnectError
	srv.mu.Unlock()
	if !strings.Contains(got, "127.0.0.1:4001") || !strings.Contains(got, "TWS handshake") {
		t.Fatalf("watchdog hint missing or malformed: %q", got)
	}
}

// When isConnected returns true after the delay, the watchdog must stay
// silent — no spurious hint to clobber a successful state.
func TestHandshakeWatchdog_SilentWhenConnected(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 4001}
	srv.handshakeWatchdog(context.Background(), func() bool { return true }, 1*time.Millisecond, ep)
	srv.mu.Lock()
	got := srv.lastConnectError
	srv.mu.Unlock()
	if got != "" {
		t.Fatalf("watchdog set lastConnectError despite connected=true: %q", got)
	}
}

// ctx cancellation before the delay fires must abort the watchdog
// silently. Used during shutdown where the surrounding goroutine is
// already winding down.
func TestHandshakeWatchdog_RespectsContextCancel(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger: NewLogger(&bytes.Buffer{}, "error"),
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 4001}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	srv.handshakeWatchdog(ctx, func() bool {
		calls.Add(1)
		return false
	}, 50*time.Millisecond, ep)
	if calls.Load() != 0 {
		t.Fatalf("watchdog called isConnected after ctx cancel")
	}
	srv.mu.Lock()
	got := srv.lastConnectError
	srv.mu.Unlock()
	if got != "" {
		t.Fatalf("watchdog set lastConnectError after ctx cancel: %q", got)
	}
}

// The watchdog must not clobber a real error already published by the
// main connect path — the gate is the lastConnectError == "" check.
func TestHandshakeWatchdog_DoesNotClobberExistingError(t *testing.T) {
	t.Parallel()
	srv := &Server{
		lastConnectError: "real error from connector.Start",
		logger:           NewLogger(&bytes.Buffer{}, "error"),
	}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 4001}
	srv.handshakeWatchdog(context.Background(), func() bool { return false }, 1*time.Millisecond, ep)
	srv.mu.Lock()
	got := srv.lastConnectError
	srv.mu.Unlock()
	if got != "real error from connector.Start" {
		t.Fatalf("watchdog clobbered real error: got %q", got)
	}
}

// triggerReconnect is the gate that prevents stacked connect attempts.
// While a connect is already running, calling it must return false
// immediately — otherwise the same `canary status` poll loop that drives
// the recovery would pile up parallel connect goroutines for the same
// endpoint while the first one is still running its handshake.
func TestTriggerReconnect_InFlightGateBlocksNewAttempt(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger:          NewLogger(&bytes.Buffer{}, "error"),
		connectInFlight: true,
		serverCtx:       context.Background(),
	}
	if srv.triggerReconnect() {
		t.Fatal("triggerReconnect must return false when connectInFlight=true")
	}
}

// Without a serverCtx (Server.Start hasn't run yet, or already returned
// and cancelled it), there's no long-lived context to attach the
// reconnect goroutine to. triggerReconnect must bail out cleanly.
func TestTriggerReconnect_NoServerCtxBails(t *testing.T) {
	t.Parallel()
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	if srv.triggerReconnect() {
		t.Fatal("triggerReconnect must return false when serverCtx is nil")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	srv.serverCtx = cancelled
	if srv.triggerReconnect() {
		t.Fatal("triggerReconnect must return false when serverCtx is already cancelled")
	}
}

// reconnectFlow re-runs discovery and re-publishes s.endpoint when the
// probe winner has changed. This is the fix for the AUTO-mode degradation
// detection bug: GW closed (4001), TWS opened (7496), the daemon was
// pinned to 4001 forever; calling reconnectFlow updates the endpoint to
// the new probe winner. We exercise the discovery+endpoint-republish
// half here; the connector.Start tail will fail because no real IBKR is
// listening, but s.endpoint is updated before that.
func TestReconnectFlow_RepublishesEndpointOnNewProbeWinner(t *testing.T) {
	// Not parallel: stubs the package-level discover.Probe.
	saved := discover.Probe
	discover.Probe = func(_ context.Context, _ string, port int, _ time.Duration) error {
		if port == 7496 {
			return nil
		}
		return errors.New("refused")
	}
	t.Cleanup(func() { discover.Probe = saved })

	srv := &Server{
		cfg:      &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", ClientID: new(15)}},
		endpoint: discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginDiscovered},
		streams:  map[string]context.CancelFunc{},
		logger:   NewLogger(&bytes.Buffer{}, "error"),
	}

	// Use a context tight enough that the connector.Start tail unwinds quickly.
	// We don't need it to succeed; we only need s.endpoint updated before the
	// connect attempt.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	srv.reconnectFlow(ctx)

	srv.mu.Lock()
	got := srv.endpoint
	srv.mu.Unlock()
	if got.Port != 7496 {
		t.Fatalf("after reconnectFlow, endpoint.Port = %d, want 7496 (the new probe winner)", got.Port)
	}
	if got.PortOrigin != discover.OriginDiscovered {
		t.Fatalf("after reconnectFlow, endpoint.PortOrigin = %q, want discovered", got.PortOrigin)
	}
}

// When discovery itself fails (no IBKR listener), reconnectFlow must
// record the discovery error so `canary status` renders a verdict instead
// of leaving the user staring at "starting…" forever.
func TestReconnectFlow_RecordsDiscoveryFailure(t *testing.T) {
	// Not parallel: stubs the package-level discover.Probe.
	saved := discover.Probe
	discover.Probe = func(_ context.Context, _ string, _ int, _ time.Duration) error {
		return errors.New("refused (test)")
	}
	t.Cleanup(func() { discover.Probe = saved })

	srv := &Server{
		cfg:      &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", ClientID: new(15)}},
		endpoint: discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginDiscovered},
		streams:  map[string]context.CancelFunc{},
		logger:   NewLogger(&bytes.Buffer{}, "error"),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	srv.reconnectFlow(ctx)

	srv.mu.Lock()
	lastErr := srv.lastConnectError
	srv.mu.Unlock()
	if !strings.Contains(lastErr, "no IBKR listener") {
		t.Fatalf("lastConnectError = %q, want it to mention discovery failure", lastErr)
	}
}

// handleStatusHealth must trigger a rediscover+reconnect when the
// daemon is currently degraded. This is the user-visible recovery path:
// running `canary status` after moving from Gateway to TWS picks up the
// new port without a daemon restart. We check the trigger fired by
// observing connectInFlight transitioning from "claimed" back to
// "cleared" inside the goroutine.
func TestHandleStatusHealth_TriggersReconnectWhenDegraded(t *testing.T) {
	// Not parallel: stubs the package-level discover.Probe.
	saved := discover.Probe
	discover.Probe = func(_ context.Context, _ string, _ int, _ time.Duration) error {
		return errors.New("refused (test)")
	}
	t.Cleanup(func() { discover.Probe = saved })

	srv := &Server{
		cfg:              &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", ClientID: new(15)}},
		endpoint:         discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15},
		version:          "test",
		streams:          map[string]context.CancelFunc{},
		logger:           NewLogger(&bytes.Buffer{}, "error"),
		lastConnectError: "test: gateway 127.0.0.1:4001 did not complete handshake",
		serverCtx:        context.Background(),
		now:              time.Now,
	}

	res := srv.handleStatusHealth()
	if res.Connected {
		t.Fatal("expected Connected=false")
	}

	// Trigger fired synchronously up to "claim slot, launch goroutine."
	// Wait for the goroutine to complete (discovery fails fast under our
	// stub) and verify the post-recovery state: lastConnectError replaced
	// with the discovery failure, in-flight flag cleared.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		inFlight := srv.connectInFlight
		lastErr := srv.lastConnectError
		srv.mu.Unlock()
		if !inFlight {
			if !strings.Contains(lastErr, "no IBKR listener") {
				t.Fatalf("after rediscovery, lastConnectError = %q, want discovery failure", lastErr)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("connectInFlight did not clear within 2s — reconnect goroutine never ran")
}

// gatewayConnector returning nil for a degraded daemon must also
// trigger a reconnect — every read handler funnels through it, so this
// is what makes commands other than `status` recover automatically.
func TestGatewayConnector_TriggersReconnectWhenDegraded(t *testing.T) {
	// Not parallel: stubs the package-level discover.Probe.
	saved := discover.Probe
	discover.Probe = func(_ context.Context, _ string, _ int, _ time.Duration) error {
		return errors.New("refused (test)")
	}
	t.Cleanup(func() { discover.Probe = saved })

	srv := &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", ClientID: new(15)}},
		endpoint:  discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15},
		streams:   map[string]context.CancelFunc{},
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		serverCtx: context.Background(),
		now:       time.Now,
	}

	if got := srv.gatewayConnector(); got != nil {
		t.Fatalf("gatewayConnector with no connector should return nil, got %v", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		inFlight := srv.connectInFlight
		lastErr := srv.lastConnectError
		srv.mu.Unlock()
		if !inFlight && lastErr != "" {
			// Goroutine ran and recorded the discovery failure.
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("gatewayConnector did not trigger a reconnect goroutine within 2s")
}

// serveConn must return when the client closes its socket end while a
// streaming RPC is in flight. The fix wires this via per-conn context
// and an EOF watcher inside the streaming handler. With no connector,
// the handler takes the gateway-unavailable early exit and returns;
// dispatch reports terminal=true; serveConn returns. The whole sequence
// must complete within a tight deadline.
func TestServeConnExitsCleanlyAfterStreamingRequest(t *testing.T) {
	t.Parallel()
	srv := &Server{
		cfg:      &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4001), ClientID: new(15)}},
		endpoint: discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15},
		streams:  map[string]context.CancelFunc{},
		logger:   NewLogger(&bytes.Buffer{}, "error"),
	}
	srv.installSubs()

	clientSide, daemonSide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close(); _ = daemonSide.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveConn(context.Background(), daemonSide)
	}()

	params, _ := json.Marshal(rpc.QuoteSubscribeParams{Contract: rpc.ContractParams{Symbol: "AAPL"}})
	req := &rpc.Request{ID: "test-1", Method: rpc.MethodQuoteSubscribe, Params: params}
	if err := json.NewEncoder(clientSide).Encode(req); err != nil {
		t.Fatalf("encode subscribe request: %v", err)
	}

	// Read the streaming handler's gateway-unavailable error response so
	// the daemon-side write doesn't block on a backed-up pipe. Then close
	// the client side; serveConn should exit promptly.
	if _, err := bufio.NewReader(clientSide).ReadBytes('\n'); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = clientSide.Close()

	select {
	case <-done:
		// pass
	case <-time.After(2 * time.Second):
		t.Fatalf("serveConn did not return within 2s after client disconnect")
	}
}

// fakeAttempter implements connectAttempter for the failover loop tests.
// Each instance is built for one candidate endpoint; connectOk decides
// whether IsConnected returns true after Start runs. blockUntilCtxDone
// makes Start sit blocked on ctx (modelling pkg/ibkr's TLS-handshake-on-
// black-hole hang) so the per-candidate budget can be exercised.
type fakeAttempter struct {
	port              int
	connectOk         bool
	startErr          error
	lastError         string
	blockUntilCtxDone bool
	startCheck        func() error
	startCheckResult  chan<- error
	connected         atomic.Bool
	stopCalls         atomic.Int32

	// observed runtime info — checked by tests
	setMarketDataType atomic.Int32
	requestedAccount  atomic.Value // string
}

func (f *fakeAttempter) Start(ctx context.Context) error {
	if f.startCheck != nil && f.startCheckResult != nil {
		f.startCheckResult <- f.startCheck()
	}
	if f.blockUntilCtxDone {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.startErr != nil {
		return f.startErr
	}
	if f.connectOk {
		f.connected.Store(true)
	}
	return nil
}
func (f *fakeAttempter) Stop() error {
	f.stopCalls.Add(1)
	f.connected.Store(false)
	return nil
}
func (f *fakeAttempter) IsConnected() bool { return f.connected.Load() }
func (f *fakeAttempter) UsingTLS() bool    { return false }
func (f *fakeAttempter) LastError() string { return f.lastError }
func (f *fakeAttempter) SetMarketDataType(t int) error {
	f.setMarketDataType.Store(int32(t))
	return nil
}
func (f *fakeAttempter) RequestAccountUpdates(account string) error {
	f.requestedAccount.Store(account)
	return nil
}
func (f *fakeAttempter) SubscribeAccountPnL(account string) error {
	// Subscription kickoff has the same lifecycle as RequestAccountUpdates
	// from the daemon's perspective: best-effort, fire-and-forget. The
	// fake records nothing extra — the failover tests don't assert PnL
	// subscription state and adding tracking would just be noise.
	return nil
}

func TestTryOneHandshakePublishesConnectorLastError(t *testing.T) {
	t.Parallel()
	srv := &Server{logger: NewLogger(&bytes.Buffer{}, "error")}
	ep := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	cause := "IBKR: client ID already in use: gateway client ID 15 is already in use; stop the stale IBKR API client or choose a free [gateway].client_id"

	if srv.tryOneHandshake(context.Background(), &fakeAttempter{lastError: cause}, ep) {
		t.Fatal("tryOneHandshake succeeded for disconnected attempter")
	}
	if srv.lastConnectError != cause {
		t.Fatalf("lastConnectError = %q, want connector cause %q", srv.lastConnectError, cause)
	}
}

// The failover bug fix: when discovery picks a port that completes the
// TCP probe but never handshakes (e.g. IB Gateway up but not logged in),
// the daemon used to stay degraded forever even though TWS on an
// alternate port was sitting in Endpoint.Alternates. With failover, the
// loop walks alternates in preference order and the first one to
// handshake wins. This test pins that contract: primary (4001) fails,
// alternate (7496) succeeds, the daemon ends up Connected against the
// alternate with lastConnectError cleared.
func TestConnectWithFailover_AlternateWinsWhenPrimaryFails(t *testing.T) {
	t.Parallel()

	built := make([]int, 0, 2)
	var attempters []*fakeAttempter

	srv := &Server{
		logger:  NewLogger(&bytes.Buffer{}, "error"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		built = append(built, ep.Port)
		a := &fakeAttempter{
			port:      ep.Port,
			connectOk: ep.Port == 7496, // only the alternate handshakes
		}
		attempters = append(attempters, a)
		return a
	}

	primary := discover.Endpoint{
		Host:       "127.0.0.1",
		Port:       4001,
		ClientID:   15,
		Alternates: []int{7496},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.connectWithFailover(ctx, primary)

	// Both candidates were built, in preference order.
	if len(built) != 2 || built[0] != 4001 || built[1] != 7496 {
		t.Fatalf("built ports = %v, want [4001 7496]", built)
	}

	// Primary was Stopped after failing handshake; alternate was not
	// Stopped (it's the live connector).
	if got := attempters[0].stopCalls.Load(); got != 1 {
		t.Fatalf("primary stopCalls = %d, want 1", got)
	}
	if got := attempters[1].stopCalls.Load(); got != 0 {
		t.Fatalf("alternate stopCalls = %d, want 0 (still live)", got)
	}

	// Post-connect setup ran on the winning attempter.
	if got := attempters[1].setMarketDataType.Load(); got != 2 {
		t.Fatalf("alternate.SetMarketDataType arg = %d, want 2 (frozen-aware)", got)
	}

	// Endpoint published reflects the winning candidate; the daemon's
	// recorded error is cleared.
	srv.mu.Lock()
	gotEp := srv.endpoint
	gotErr := srv.lastConnectError
	srv.mu.Unlock()
	if gotEp.Port != 7496 {
		t.Fatalf("endpoint.Port = %d after failover, want 7496", gotEp.Port)
	}
	if gotErr != "" {
		t.Fatalf("lastConnectError = %q, want empty after success", gotErr)
	}
}

// When every candidate's handshake fails, the daemon publishes a verdict
// that names what was tried so `canary status` shows the user the full
// picture. This is the "all candidates exhausted" branch — the user must
// fix something upstream (login, API checkbox, port config) before the
// daemon can recover.
func TestConnectWithFailover_ExhaustionPublishesNamedVerdict(t *testing.T) {
	t.Parallel()

	srv := &Server{
		logger:  NewLogger(&bytes.Buffer{}, "error"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		return &fakeAttempter{port: ep.Port, connectOk: false}
	}

	primary := discover.Endpoint{
		Host:       "127.0.0.1",
		Port:       4001,
		ClientID:   15,
		Alternates: []int{7496},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.connectWithFailover(ctx, primary)

	srv.mu.Lock()
	gotErr := srv.lastConnectError
	srv.mu.Unlock()
	if !strings.Contains(gotErr, "none of 2 discovered endpoint(s)") {
		t.Fatalf("lastConnectError = %q, want exhaustion verdict naming 2 endpoints", gotErr)
	}
	if !strings.Contains(gotErr, "127.0.0.1:4001") || !strings.Contains(gotErr, "127.0.0.1:7496") {
		t.Fatalf("lastConnectError = %q, want it to name both 4001 and 7496", gotErr)
	}
}

// A candidate whose handshake hangs forever (the black-hole-TLS
// scenario: TCP accepted, no ServerHello ever sent) must not block
// failover. The per-candidate budget caps the wait; the loop then
// advances to the alternate, which lands Connected. This is the live
// reproduction that revealed the bug: an IB Gateway stuck mid-login
// would freeze the daemon against 4001 even though TWS on 7496 was
// healthy and right there in the alternates list.
func TestConnectWithFailover_HangingPrimaryYieldsToAlternateAfterBudget(t *testing.T) {
	// Not parallel: mutates the package-level budget var.
	saved := perCandidateConnectBudget
	perCandidateConnectBudget = 50 * time.Millisecond
	t.Cleanup(func() { perCandidateConnectBudget = saved })

	var built []int
	srv := &Server{
		logger:  NewLogger(&bytes.Buffer{}, "error"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		built = append(built, ep.Port)
		switch ep.Port {
		case 4001:
			return &fakeAttempter{port: ep.Port, blockUntilCtxDone: true}
		case 7496:
			return &fakeAttempter{port: ep.Port, connectOk: true}
		default:
			t.Fatalf("unexpected candidate port %d", ep.Port)
			return nil
		}
	}

	primary := discover.Endpoint{
		Host:       "127.0.0.1",
		Port:       4001,
		ClientID:   15,
		Alternates: []int{7496},
	}

	// Give the loop enough wall-clock for one full candidate budget
	// (50ms) plus alternate (effectively instant).
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	start := time.Now()
	srv.connectWithFailover(ctx, primary)
	elapsed := time.Since(start)

	if len(built) != 2 || built[0] != 4001 || built[1] != 7496 {
		t.Fatalf("built ports = %v, want [4001 7496] (loop must advance past hung primary)", built)
	}
	// The loop should have spent ~one budget on the primary, not the
	// full test ctx timeout. Allow generous slack for scheduler jitter.
	if elapsed > 1*time.Second {
		t.Fatalf("failover took %s, expected ~50ms primary + ~0ms alt; per-candidate budget did not fire", elapsed)
	}
	srv.mu.Lock()
	gotPort := srv.endpoint.Port
	gotErr := srv.lastConnectError
	srv.mu.Unlock()
	if gotPort != 7496 {
		t.Fatalf("endpoint.Port = %d after failover, want 7496", gotPort)
	}
	if gotErr != "" {
		t.Fatalf("lastConnectError = %q, want empty after alternate success", gotErr)
	}
}

// No alternates → single candidate → degenerate failover that just runs
// the primary once. Preserves the pre-failover semantics for the common
// case where discovery saw exactly one IBKR endpoint.
func TestConnectWithFailover_SingleCandidateNoAlternates(t *testing.T) {
	t.Parallel()

	var built []int
	srv := &Server{
		logger:  NewLogger(&bytes.Buffer{}, "error"),
		streams: map[string]context.CancelFunc{},
	}
	srv.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		built = append(built, ep.Port)
		return &fakeAttempter{port: ep.Port, connectOk: true}
	}

	primary := discover.Endpoint{Host: "127.0.0.1", Port: 7496, ClientID: 15}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.connectWithFailover(ctx, primary)

	if len(built) != 1 || built[0] != 7496 {
		t.Fatalf("built ports = %v, want [7496] (single candidate)", built)
	}
	srv.mu.Lock()
	gotErr := srv.lastConnectError
	srv.mu.Unlock()
	if gotErr != "" {
		t.Fatalf("lastConnectError = %q, want empty after single-candidate success", gotErr)
	}
}

// MethodCancel terminates a previously registered stream by id. The dispatcher
// used to omit a case for it entirely, so any caller using the documented
// "cancel" wire op got unknown_method back and the stream's refcount stayed
// held until the underlying socket EOFed. This test pins the wiring.
func TestDispatchMethodCancelCancelsRegisteredStream(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	cancelled := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	srv.mu.Lock()
	srv.streams["stream-id"] = func() {
		cancel()
		cancelled <- struct{}{}
	}
	srv.mu.Unlock()

	params, _ := json.Marshal(rpc.CancelParams{ID: "stream-id"})
	req := &rpc.Request{ID: "req-1", Method: rpc.MethodCancel, Params: params}

	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	r := bufio.NewReader(strings.NewReader(""))

	terminal := srv.dispatch(context.Background(), req, enc, r)
	if terminal {
		t.Fatalf("cancel should not be terminal — it's a unary op on the same connection")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatalf("registered cancel func was never invoked")
	}
	if ctx.Err() == nil {
		t.Fatalf("expected stream context to be cancelled")
	}

	var resp rpc.Response
	if err := json.Unmarshal(encOut.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw %q)", err, encOut.String())
	}
	if !resp.Ok || resp.Error != nil || resp.ID != "req-1" {
		t.Fatalf("unexpected response: %+v err=%+v", resp, resp.Error)
	}
}

// Cancelling an id the daemon never handed out is a programming error —
// returning silent success would mask client bugs. The dispatcher maps it
// to CodeBadRequest via the badRequestError sentinel that classifyError
// already understands.
func TestDispatchMethodCancelUnknownIDReturnsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	params, _ := json.Marshal(rpc.CancelParams{ID: "never-existed"})
	req := &rpc.Request{ID: "req-1", Method: rpc.MethodCancel, Params: params}

	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	r := bufio.NewReader(strings.NewReader(""))

	if terminal := srv.dispatch(context.Background(), req, enc, r); terminal {
		t.Fatalf("cancel should not be terminal")
	}

	var resp rpc.Response
	if err := json.Unmarshal(encOut.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Ok || resp.Error == nil || resp.Error.Code != rpc.CodeBadRequest {
		t.Fatalf("expected bad_request, got %+v", resp)
	}
}

// A panic in a handler must not unwind through serveConn (which would
// kill the per-connection goroutine and disconnect *other* clients).
// recoverHandler converts the panic into an internal_error response on
// the same JSON-RPC id and lets the dispatcher continue.
func TestRecoverHandlerWritesErrorAndDoesNotPropagate(t *testing.T) {
	t.Parallel()
	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	req := &rpc.Request{ID: "req-1", Method: "fake.panic"}
	logger := NewLogger(&bytes.Buffer{}, "error")

	// Use a helper closure so the defer scope matches the dispatcher's.
	func() {
		defer recoverHandler(logger, enc, req)
		panic("simulated handler panic")
	}()

	var resp rpc.Response
	if err := json.Unmarshal(encOut.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v raw=%q", err, encOut.String())
	}
	if resp.Ok || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if resp.Error.Code != rpc.CodeInternal {
		t.Fatalf("expected CodeInternal, got %q", resp.Error.Code)
	}
	if resp.ID != "req-1" {
		t.Fatalf("expected id to echo request, got %q", resp.ID)
	}
}

// recoverHandler must tolerate a nil request (a panic that fires before
// req is populated). It writes an error with id="" rather than nil-deref.
func TestRecoverHandlerHandlesNilRequest(t *testing.T) {
	t.Parallel()
	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	logger := NewLogger(&bytes.Buffer{}, "error")

	func() {
		defer recoverHandler(logger, enc, nil)
		panic("nil-req panic")
	}()

	var resp rpc.Response
	if err := json.Unmarshal(encOut.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Ok || resp.Error == nil || resp.Error.Code != rpc.CodeInternal {
		t.Fatalf("expected internal error, got %+v", resp)
	}
}

// readBoundedLine returns errFrameTooLarge before the input can OOM the
// daemon. Without this cap, a peer sending a newline-free byte stream
// would grow bufio's accumulator until process memory exhaustion.
func TestReadBoundedLineRejectsOversize(t *testing.T) {
	t.Parallel()
	// Make a payload bigger than the cap. The reader must reject before
	// returning a line.
	const cap = 1024
	big := bytes.Repeat([]byte{'x'}, cap+16)
	r := bufio.NewReaderSize(bytes.NewReader(big), 256)
	_, err := readBoundedLine(r, cap)
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("expected errFrameTooLarge, got %v", err)
	}
}

// readBoundedLine returns the line including the trailing newline for any
// payload up to and including maxBytes. Regression: an off-by-one earlier
// in the implementation would reject lines exactly maxBytes long.
func TestReadBoundedLineAcceptsAtCap(t *testing.T) {
	t.Parallel()
	const cap = 64
	// 63 'x' + '\n' = 64 bytes total — exactly at cap.
	line := append(bytes.Repeat([]byte{'x'}, cap-1), '\n')
	r := bufio.NewReaderSize(bytes.NewReader(line), 32)
	got, err := readBoundedLine(r, cap)
	if err != nil {
		t.Fatalf("unexpected error at cap: %v", err)
	}
	if !bytes.Equal(got, line) {
		t.Fatalf("got %q, want %q", got, line)
	}
}

// End-to-end: serveConn must reject an oversize newline-free blob with a
// classified bad_request response and close cleanly — not OOM and not
// hang forever waiting for a newline.
func TestServeConnRejectsOversizedFrame(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	serverSide, clientSide := net.Pipe()
	t.Cleanup(func() { _ = clientSide.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.serveConn(context.Background(), serverSide)
	}()

	// Push more bytes than the cap with no newline. The serve loop should
	// detect the breach mid-stream and respond before we send everything.
	// Two subtleties:
	//   - writer must NOT Close clientSide on its own — that races with
	//     the server's in-flight error-response Write through the
	//     synchronous pipe and turns a clean reject into a "closed pipe"
	//     on Decode. Test cleanup handles the close.
	//   - payload must comfortably exceed maxFrameBytes so the bounded
	//     reader's bufio refill at the cap boundary is satisfied by
	//     still-pending writer data (otherwise bufio.ReadSlice blocks
	//     waiting for a refill the writer can't supply).
	big := bytes.Repeat([]byte{'x'}, 2*maxFrameBytes)
	go func() { _, _ = clientSide.Write(big) }()

	dec := json.NewDecoder(clientSide)
	var resp rpc.Response
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("expected bad_request response, decode error: %v", err)
	}
	if resp.Ok || resp.Error == nil || resp.Error.Code != rpc.CodeBadRequest {
		t.Fatalf("expected bad_request, got %+v", resp)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("serveConn did not return after oversize-frame rejection")
	}
}
