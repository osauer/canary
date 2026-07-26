package daemon

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

// fakeConnector implements ibkrMarketConnector for unit-testing the
// subManager. It does not mock daemon-internal data — it transports
// pre-fed cached ticks (the same shape the real connector publishes after
// a gateway response). Per project rule: transport seams are fair game
// for fakes; data is not.
type fakeConnector struct {
	mu             sync.Mutex
	cache          map[string]*ibkrlib.MarketData
	dataType       int
	subscribed     map[string]int // count of SubscribeMarketData calls per symbol
	contracts      map[string]ibkrlib.Contract
	unsubscribed   map[string]int // count of UnsubscribeMarketData calls per symbol
	subscribeError error
	// subscribeDelay simulates a slow IBKR Subscribe call so tests can
	// observe whether two cold-Subscribes for distinct symbols serialise
	// behind one another.
	subscribeDelay time.Duration
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{
		cache:        map[string]*ibkrlib.MarketData{},
		subscribed:   map[string]int{},
		contracts:    map[string]ibkrlib.Contract{},
		unsubscribed: map[string]int{},
		dataType:     1, // live
	}
}

func (f *fakeConnector) SubscribeMarketData(ctx context.Context, symbol string, _ []string) error {
	if f.subscribeError != nil {
		return f.subscribeError
	}
	if f.subscribeDelay > 0 {
		// Honour ctx so the F-26 contract test can pin the behaviour: a
		// 1 s ctx-deadline must unblock a slow subscribe even when the
		// fake's configured delay is much longer.
		select {
		case <-time.After(f.subscribeDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed[symbol]++
	return nil
}

func (f *fakeConnector) SubscribeMarketDataWithContract(ctx context.Context, contract ibkrlib.Contract, _ []string) (string, error) {
	if f.subscribeError != nil {
		return "", f.subscribeError
	}
	if f.subscribeDelay > 0 {
		select {
		case <-time.After(f.subscribeDelay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	key := ibkrlib.MarketDataKeyForContract(contract)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed[key]++
	f.contracts[key] = contract
	return key, nil
}

func (f *fakeConnector) UnsubscribeMarketData(symbol string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribed[symbol]++
	return nil
}

func (f *fakeConnector) MarketDataSnapshot() map[string]*ibkrlib.MarketData {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*ibkrlib.MarketData, len(f.cache))
	for k, v := range f.cache {
		copy := *v
		out[k] = &copy
	}
	return out
}

func (f *fakeConnector) MarketDataTypeForSymbol(_ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dataType
}

// putTick simulates the gateway delivering a tick by dropping a MarketData
// row into the connector's cache. The next tick-loop pass will pick it up
// and (on change) emit a Frame.
func (f *fakeConnector) putTick(symbol string, bid, ask, last float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[symbol] = &ibkrlib.MarketData{Bid: bid, Ask: ask, Last: last}
}

func (f *fakeConnector) subCount(symbol string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribed[symbol]
}

func (f *fakeConnector) unsubCount(symbol string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unsubscribed[symbol]
}

func (f *fakeConnector) subscribedContract(symbol string) (ibkrlib.Contract, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	contract, ok := f.contracts[symbol]
	return contract, ok
}

// testManager pairs a subManager with the atomic.Bool that gates its
// connector closure, so tests can simulate gateway loss without reaching
// into the manager's private fields.
type testManager struct {
	*subManager
	gatewayUp *atomic.Bool
}

// newTestManager builds a subManager wired to the supplied fake.  Coalesce
// is shrunk from the production 150ms default to 5ms so tests don't need
// to wait long for the first frame; the dedup logic is unaffected.
func newTestManager(fake *fakeConnector) *testManager {
	gatewayUp := &atomic.Bool{}
	gatewayUp.Store(true)
	m := &subManager{
		subs:     map[string]*subEntry{},
		coalesce: 5 * time.Millisecond,
		connector: func() ibkrMarketConnector {
			if !gatewayUp.Load() {
				return nil
			}
			return fake
		},
	}
	return &testManager{subManager: m, gatewayUp: gatewayUp}
}

// TestSubscribeReceivesFrames covers F3 / F9: a single subscriber gets
// frames after the gateway delivers ticks, dedup eliminates no-change
// ticks, and the IBKR sub is opened exactly once.
func TestSubscribeReceivesFrames(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	frames, release, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer release()

	if got := fake.subCount("AAPL"); got != 1 {
		t.Fatalf("SubscribeMarketData called %d times, want 1", got)
	}

	fake.putTick("AAPL", 207.86, 207.88, 207.87)
	f := receiveFrame(t, frames, 200*time.Millisecond)
	if f.Bid == nil || *f.Bid != 207.86 {
		t.Errorf("frame.Bid: got %v, want 207.86", f.Bid)
	}
	if f.Last == nil || *f.Last != 207.87 {
		t.Errorf("frame.Last: got %v, want 207.87", f.Last)
	}

	// A repeated unchanged tick must not produce a new frame (dedup).
	select {
	case f := <-frames:
		t.Errorf("expected no frame on no-change tick, got %+v", f)
	case <-time.After(40 * time.Millisecond):
		// pass
	}

	// A changed tick produces a new frame.
	fake.putTick("AAPL", 207.90, 207.92, 207.91)
	f = receiveFrame(t, frames, 200*time.Millisecond)
	if f.Bid == nil || *f.Bid != 207.90 {
		t.Errorf("after change, frame.Bid: got %v, want 207.90", f.Bid)
	}
}

// TestFanoutSharesIBKRLine covers F4 + P2: two concurrent subscribers to
// the same symbol must share one IBKR market-data line and receive the
// same frames.
func TestFanoutSharesIBKRLine(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	a, releaseA, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer releaseA()
	b, releaseB, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer releaseB()

	if got := fake.subCount("AAPL"); got != 1 {
		t.Fatalf("SubscribeMarketData called %d times, want 1 (refcount must collapse)", got)
	}
	if got := m.activeCount(); got != 1 {
		t.Fatalf("activeCount: got %d, want 1", got)
	}

	fake.putTick("AAPL", 100.0, 100.05, 100.02)
	fA := receiveFrame(t, a, 200*time.Millisecond)
	fB := receiveFrame(t, b, 200*time.Millisecond)
	if *fA.Bid != *fB.Bid || *fA.Ask != *fB.Ask || *fA.Last != *fB.Last {
		t.Errorf("subscribers got different frames: A=%+v B=%+v", fA, fB)
	}
}

func TestLateSubscriberReceivesCurrentFrame(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	a, releaseA, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	defer releaseA()

	fake.putTick("AAPL", 100.0, 100.05, 100.02)
	receiveFrame(t, a, 200*time.Millisecond)

	b, releaseB, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}
	defer releaseB()

	fB := receiveFrame(t, b, 200*time.Millisecond)
	if fB.Last == nil || *fB.Last != 100.02 {
		t.Fatalf("late subscriber frame.Last: got %v, want 100.02", fB.Last)
	}
	if got := fake.subCount("AAPL"); got != 1 {
		t.Fatalf("late subscriber opened %d IBKR lines, want 1", got)
	}
}

// TestRefcountReleasesLastSub covers F5: the IBKR line is released the
// moment the last subscriber releases.
func TestRefcountReleasesLastSub(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	_, releaseA, _ := m.Subscribe(context.Background(), "AAPL")
	_, releaseB, _ := m.Subscribe(context.Background(), "AAPL")

	releaseA()
	if got := fake.unsubCount("AAPL"); got != 0 {
		t.Errorf("after first release, UnsubscribeMarketData calls: got %d, want 0", got)
	}
	releaseB()
	if got := fake.unsubCount("AAPL"); got != 1 {
		t.Errorf("after last release, UnsubscribeMarketData calls: got %d, want 1", got)
	}
	if got := m.activeCount(); got != 0 {
		t.Errorf("activeCount after last release: got %d, want 0", got)
	}
}

// TestHoldAndSubscribeShareLine covers the snapshot-while-watching
// scenario from F4: a Hold (no frames) and a Subscribe (frames) on the
// same symbol must share one IBKR line.
func TestHoldAndSubscribeShareLine(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	releaseHold, err := m.Hold(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	defer releaseHold()

	frames, releaseSub, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer releaseSub()

	if got := fake.subCount("AAPL"); got != 1 {
		t.Errorf("Subscribe + Hold should share IBKR line: got %d Subscribe calls, want 1", got)
	}
	fake.putTick("AAPL", 1.0, 1.01, 1.005)
	receiveFrame(t, frames, 200*time.Millisecond)
}

func TestSubscribeContractUsesRouteKey(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	contract := ibkrlib.Contract{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", PrimaryExch: "CBOE", Currency: "USD"}
	key := ibkrlib.MarketDataKeyForContract(contract)
	frames, release, err := m.SubscribeContract(context.Background(), contract)
	if err != nil {
		t.Fatalf("SubscribeContract: %v", err)
	}
	defer release()

	if got := fake.subCount("VIX"); got != 0 {
		t.Fatalf("SubscribeContract used bare symbol path %d times, want 0", got)
	}
	if got := fake.subCount(key); got != 1 {
		t.Fatalf("SubscribeMarketDataWithContract called %d times for %s, want 1", got, key)
	}
	if got, ok := fake.subscribedContract(key); !ok || got.SecType != "IND" || got.Exchange != "CBOE" {
		t.Fatalf("routed contract not preserved: got %+v ok=%v", got, ok)
	}

	fake.putTick(key, 15.10, 15.20, 15.15)
	f := receiveFrame(t, frames, 200*time.Millisecond)
	if f.Last == nil || *f.Last != 15.15 {
		t.Fatalf("routed frame.Last: got %v, want 15.15", f.Last)
	}
	release()
	if got := fake.unsubCount(key); got != 1 {
		t.Fatalf("routed unsubscribe count: got %d, want 1", got)
	}
}

// TestGatewayLostEmitsTerminalFrame covers F7: a gateway disconnect
// mid-stream produces a terminal Frame with FrameErrGatewayLost, and the
// frame channel closes immediately after.
func TestGatewayLostEmitsTerminalFrame(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	frames, release, err := m.Subscribe(context.Background(), "AAPL")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer release()

	// Get one normal frame first so the test exercises mid-stream loss
	// rather than the very-first-tick path.
	fake.putTick("AAPL", 100.0, 100.1, 100.05)
	receiveFrame(t, frames, 200*time.Millisecond)

	// Pull the gateway out from under the manager. Next tick: connector()
	// returns nil, manager emits gateway_lost, closes taps, tears down.
	m.gatewayUp.Store(false)

	terminal := receiveFrame(t, frames, 200*time.Millisecond)
	if terminal.Error == nil {
		t.Fatalf("expected terminal error frame, got %+v", terminal)
	}
	if terminal.Error.Code != rpc.FrameErrGatewayLost {
		t.Errorf("error code: got %q, want %q", terminal.Error.Code, rpc.FrameErrGatewayLost)
	}

	// Channel must close after the terminal frame so consumers' range
	// loops exit.
	select {
	case _, ok := <-frames:
		if ok {
			t.Errorf("frame channel should be closed after gateway_lost")
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("frame channel did not close after gateway_lost")
	}
}

// TestCloseEmitsDaemonShutdown covers F8: Close emits daemon_shutdown to
// every active subscription.
func TestCloseEmitsDaemonShutdown(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)

	a, releaseA, _ := m.Subscribe(context.Background(), "AAPL")
	defer releaseA()
	b, releaseB, _ := m.Subscribe(context.Background(), "MSFT")
	defer releaseB()

	m.Close()

	for name, ch := range map[string]<-chan rpc.Frame{"AAPL": a, "MSFT": b} {
		f := receiveFrame(t, ch, 200*time.Millisecond)
		if f.Error == nil || f.Error.Code != rpc.FrameErrDaemonShutdown {
			t.Errorf("%s: expected daemon_shutdown frame, got %+v", name, f)
		}
	}
}

// TestColdSubscribesForDifferentSymbolsDoNotSerialise covers the
// per-symbol locking invariant: a slow IBKR Subscribe for symbol A
// must not block a concurrent first-Subscribe for symbol B.
//
// Before the per-symbol init lock, both cold-Subscribes serialised on
// the global subManager.mu, so the total elapsed time was ~2 × delay.
// After the fix they overlap and total time stays near 1 × delay.
func TestColdSubscribesForDifferentSymbolsDoNotSerialise(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	delay := 50 * time.Millisecond
	fake.subscribeDelay = delay
	m := newTestManager(fake)

	var wg sync.WaitGroup
	wg.Add(2)
	start := time.Now()
	for _, sym := range []string{"AAA", "BBB"} {
		go func() {
			defer wg.Done()
			_, release, err := m.Subscribe(context.Background(), sym)
			if err != nil {
				t.Errorf("subscribe %s: %v", sym, err)
				return
			}
			release()
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Allow generous headroom for CI jitter, but reject the
	// pathological serialised case (~2 × delay).
	if elapsed > 2*delay-10*time.Millisecond {
		t.Fatalf("cold subscribes serialised: elapsed=%v want < %v", elapsed, 2*delay-10*time.Millisecond)
	}
}

// TestSubscribeWithGatewayDownReturnsUnavailable covers the synchronous
// error path: subscribe-time gateway-unavailable surfaces as a typed
// error, not an error frame.
func TestSubscribeWithGatewayDownReturnsUnavailable(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	m.gatewayUp.Store(false)

	_, _, err := m.Subscribe(context.Background(), "AAPL")
	if err == nil {
		t.Fatalf("expected error when gateway unavailable, got nil")
	}
	if err != ibkrlib.ErrIBKRUnavailable {
		t.Errorf("expected ErrIBKRUnavailable, got %v", err)
	}
}

func receiveFrame(t *testing.T, ch <-chan rpc.Frame, timeout time.Duration) rpc.Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("frame channel closed prematurely")
		}
		return f
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for frame after %s", timeout)
		return rpc.Frame{}
	}
}
