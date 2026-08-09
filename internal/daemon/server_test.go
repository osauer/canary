package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ibkrd-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

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

	conn, err := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("dial fresh listener: %v", err)
	}
	_ = conn.Close()
}

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

func TestDispatchMethodsMatchRPCTimingCatalog(t *testing.T) {
	t.Parallel()

	constants := rpcMethodConstants(t)
	dispatched := dispatchMethodValues(t, constants)
	catalogued := make(map[string]bool)
	for _, timing := range rpc.MethodTimings() {
		catalogued[timing.Method] = true
	}
	for method := range dispatched {
		if !catalogued[method] {
			t.Errorf("dispatched method %q has no rpc timing entry", method)
		}
	}
	for method := range catalogued {
		if !dispatched[method] {
			t.Errorf("rpc timing entry %q has no daemon dispatch case", method)
		}
	}
}

func rpcMethodConstants(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join("..", "rpc", "*.go"))
	if err != nil {
		t.Fatalf("glob rpc files: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range values.Names {
					if !strings.HasPrefix(name.Name, "Method") || i >= len(values.Values) {
						continue
					}
					lit, ok := values.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil {
						out[name.Name] = value
					}
				}
			}
		}
	}
	return out
}

func dispatchMethodValues(t *testing.T, constants map[string]string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				sel, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "rpc" || !strings.HasPrefix(sel.Sel.Name, "Method") {
					continue
				}
				value, ok := constants[sel.Sel.Name]
				if !ok {
					t.Errorf("dispatch uses unresolved rpc.%s", sel.Sel.Name)
					continue
				}
				out[value] = true
			}
			return true
		})
	}
	return out
}

func TestUnaryDeadlineCoversAllUnaryMethods(t *testing.T) {
	t.Parallel()
	for _, timing := range rpc.MethodTimings() {
		got := unaryDeadline(timing.Method)
		if timing.Lifetime == rpc.MethodLifetimeStreaming {
			if got != 0 {
				t.Errorf("unaryDeadline(%q) = %s, want 0 for streaming method", timing.Method, got)
			}
			continue
		}
		if got != timing.DaemonTimeout {
			t.Errorf("unaryDeadline(%q) = %s, want catalog timeout %s", timing.Method, got, timing.DaemonTimeout)
		}
	}
}

func TestOrderPreviewUnaryDeadlineCoversBrokerWhatIf(t *testing.T) {
	t.Parallel()

	if got := unaryDeadline(rpc.MethodOrderPreview); got < 50*time.Second || got >= 60*time.Second {
		t.Fatalf("order.preview deadline = %s, want enough room for broker WhatIf but below the CLI 60s ceiling", got)
	}
}

func TestStartOpensSocketBeforeGatewayHandshake(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	tlsFalse := false
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99), TLS: &tlsFalse},
	}

	cfg.Daemon.SetIdleTimeout(0)

	srv := New(Options{
		Config:            cfg,
		SocketPath:        sockPath,
		Version:           "test",
		Logger:            NewLogger(&bytes.Buffer{}, "error"),
		StateDatabasePath: filepath.Join(dir, "daemon.db"),
	})

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

	cancel()
	select {
	case <-startReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s of cancellation")
	}
	srv.Stop()
}

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

	case <-time.After(2 * time.Second):
		t.Fatal("runIdleWatcher did not return within 2s of idle timer firing")
	}
}

func TestRunIdleWatcherDefersShutdownWhileBreadthRefreshing(t *testing.T) {
	t.Parallel()

	cfg := &config.Resolved{}
	cfg.Daemon.SetIdleTimeout(40 * time.Millisecond)

	fetcher := &spx.FakeBarFetcher{
		Bars:    map[string][]spx.Bar{"AAA": {{Date: "2026-05-18", Close: 100}}},
		Latency: 400 * time.Millisecond,
	}
	engine := spx.New(spx.NewStore(t.TempDir()), fetcher, spx.Options{Workers: 1024})

	refreshDone := make(chan struct{})
	go func() {
		defer close(refreshDone)
		_ = engine.Refresh(context.Background())
	}()

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

	select {
	case <-watcherDone:
		t.Fatal("runIdleWatcher returned while breadth refresh was in flight")
	case <-time.After(120 * time.Millisecond):

	}

	<-refreshDone
	select {
	case <-watcherDone:

	case <-time.After(2 * time.Second):
		t.Fatal("runIdleWatcher did not return within 2 s after the busy condition cleared")
	}
}

func TestIsBusyIncludesGammaCompute(t *testing.T) {
	t.Parallel()
	srv := &Server{
		zeroGamma: newGammaZeroCache(),
	}
	if srv.isBusy() {
		t.Error("isBusy() should be false with no gamma compute in flight")
	}

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

func TestBackgroundTasksRegistry_isBusyAndHandlerAgree(t *testing.T) {
	t.Parallel()
	srv := &Server{
		logger:    NewLogger(&bytes.Buffer{}, "error"),
		version:   "test",
		startedAt: time.Now(),
		zeroGamma: newGammaZeroCache(),
	}

	if srv.isBusy() {
		t.Error("idle daemon: isBusy() should be false")
	}
	if len(srv.handleStatusHealth().BackgroundTasks) != 0 {
		t.Error("idle daemon: BackgroundTasks should be empty")
	}

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

	close(fakeJob.done)
	if srv.isBusy() {
		t.Error("after gamma done: isBusy() should be false")
	}
	if got := srv.handleStatusHealth().BackgroundTasks; len(got) != 0 {
		t.Errorf("after gamma done: BackgroundTasks=%+v, want []", got)
	}

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

	go srv.acceptLoop(context.Background(), srv.listener)
	srv.runIdleWatcher(context.Background())
	srv.closeListener()

	time.Sleep(50 * time.Millisecond)

	srv.Stop()

	if _, err := os.Stat(srv.socketPath); !os.IsNotExist(err) {
		t.Fatalf("socket file should be removed after idle shutdown + Stop; stat err=%v", err)
	}
	if srv.listener != nil {
		t.Fatal("listener should be nil after Stop")
	}
}

func TestReconnectFlow_RepublishesEndpointOnNewProbeWinner(t *testing.T) {

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

	if _, err := bufio.NewReader(clientSide).ReadBytes('\n'); err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = clientSide.Close()

	select {
	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatalf("serveConn did not return within 2s after client disconnect")
	}
}

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

	setMarketDataType atomic.Int32
	requestedAccount  atomic.Value
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

	return nil
}

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
			connectOk: ep.Port == 7496,
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

	if len(built) != 2 || built[0] != 4001 || built[1] != 7496 {
		t.Fatalf("built ports = %v, want [4001 7496]", built)
	}

	if got := attempters[0].stopCalls.Load(); got != 1 {
		t.Fatalf("primary stopCalls = %d, want 1", got)
	}
	if got := attempters[1].stopCalls.Load(); got != 0 {
		t.Fatalf("alternate stopCalls = %d, want 0 (still live)", got)
	}

	if got := attempters[1].setMarketDataType.Load(); got != 2 {
		t.Fatalf("alternate.SetMarketDataType arg = %d, want 2 (frozen-aware)", got)
	}

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

func TestRecoverHandlerWritesErrorAndDoesNotPropagate(t *testing.T) {
	t.Parallel()
	var encOut bytes.Buffer
	enc := json.NewEncoder(&encOut)
	req := &rpc.Request{ID: "req-1", Method: "fake.panic"}
	logger := NewLogger(&bytes.Buffer{}, "error")

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

func TestReadBoundedLineRejectsOversize(t *testing.T) {
	t.Parallel()

	const cap = 1024
	big := bytes.Repeat([]byte{'x'}, cap+16)
	r := bufio.NewReaderSize(bytes.NewReader(big), 256)
	_, err := readBoundedLine(r, cap)
	if !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("expected errFrameTooLarge, got %v", err)
	}
}

func TestReadBoundedLineAcceptsAtCap(t *testing.T) {
	t.Parallel()
	const cap = 64

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
