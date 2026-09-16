package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

// defaultCoalesceInterval is the cadence at which the per-symbol tick loop
// reads the IBKR market-data cache and (on change) emits a frame to its
// taps. Matches the value used by the pre-fan-out implementation; chosen
// to amortize render churn without lagging visibly behind the gateway.
const defaultCoalesceInterval = 150 * time.Millisecond

// defaultGenericTicks is the generic-tick list the daemon requests on every
// symbol subscription. Picked to cover the fields the CLI / MCP consumers
// expect to render: 100 = option volume, 101 = option open interest,
// 104 = historical volatility, 106 = option implied volatility (averaged
// across the chain — the "IV of the underlying" retail platforms display),
// 165 = Misc Stats (delivers 13w/26w/52w highs/lows as tickPrice msgs
// with tick types 15-20), and 236 = shortable shares. 106 and 165 enrich
// the technical and monitor reads; 236 is evidence for the
// market-event borrow-inventory flag.
var defaultGenericTicks = []string{"100", "101", "104", "106", "165", "236"}

// ibkrMarketConnector is the slice of *ibkrlib.Connector that subManager
// touches. Defining it as an interface lets unit tests drive the manager
// with a fake instead of a real gateway connection — required by the
// project's "no mocks for daemon-internal data, but transport seams are
// fair game" rule.
type ibkrMarketConnector interface {
	SubscribeMarketData(ctx context.Context, symbol string, fields []string) error
	SubscribeMarketDataWithContract(ctx context.Context, contract ibkrlib.Contract, fields []string) (string, error)
	UnsubscribeMarketData(symbol string) error
	MarketDataSnapshot() map[string]*ibkrlib.MarketData
	MarketDataTypeForSymbol(symbol string) int
}

type marketSubscribeFunc func(context.Context, ibkrMarketConnector) (string, error)

// subManager owns the daemon's per-symbol market-data subscriptions and
// + concurrent snapshot polls). At most one IBKR market-data line is held
//   - subsMu guards the `subs` map only — held briefly for lookup/insert/
//     IBKR Subscribe/Unsubscribe call for each symbol. Two cold-Subscribes
//     the same symbol serialise (only the first does the IBKR call).
type subManager struct {
	subsMu   sync.Mutex
	subs     map[string]*subEntry
	coalesce time.Duration

	initMu    sync.Mutex
	initLocks map[string]*sync.Mutex

	// connector is re-fetched on every tick so a daemon-side reconnect
	connector func() ibkrMarketConnector
}

type subEntry struct {
	cancelSource func(context.Context, string) error
	sym          string
	owner        ibkrMarketConnector
	current      func() bool
	stopOnce     sync.Once
	stop         chan struct{}

	// mu guards everything below. Per-symbol scope so fan-out and
	// release on one symbol don't block operations on another.
	mu       sync.Mutex
	refcount int
	taps     map[*frameTap]struct{}

	// Cached last-emitted state for change-detection.
	lastBid, lastAsk, lastLast float64
	lastBidSize, lastAskSize   int
	lastDataType               string
	emitted                    bool
}

type frameTap struct {
	ch chan rpc.Frame
}

func newSubManager(connector func() ibkrMarketConnector) *subManager {
	return &subManager{
		subs:      map[string]*subEntry{},
		coalesce:  defaultCoalesceInterval,
		initLocks: map[string]*sync.Mutex{},
		connector: connector,
	}
}

// symInitLock returns (creating on demand) the per-symbol init mutex.
// Callers hold it for the duration of an IBKR SubscribeMarketData /
// UnsubscribeMarketData call so the IBKR-side state for that symbol is
// serialised, without blocking operations on other symbols.
func (m *subManager) symInitLock(sym string) *sync.Mutex {
	m.initMu.Lock()
	defer m.initMu.Unlock()
	if m.initLocks == nil {
		m.initLocks = map[string]*sync.Mutex{}
	}
	if lock, ok := m.initLocks[sym]; ok {
		return lock
	}
	lock := &sync.Mutex{}
	m.initLocks[sym] = lock
	return lock
}

// acquire is the shared body of Subscribe and Hold. addTap=true attaches a
// frame channel; addTap=false (Hold) keeps the IBKR line open without
// ctx bounds the underlying pkg/ibkr.SubscribeMarketData call. Per-request
// same symbol skip the IBKR-side subscribe (refcount bump only), so ctx
func (m *subManager) acquire(ctx context.Context, sym string, addTap bool, subscribe marketSubscribeFunc) (*frameTap, string, *subEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c := m.connector()
	if c == nil {
		return nil, "", nil, ibkrlib.ErrIBKRUnavailable
	}

	var origin ibkrlib.ConnectorSessionBinding
	if real, ok := c.(*ibkrlib.Connector); ok {
		var ready bool
		origin, ready = real.CaptureSession()
		if !ready {
			return nil, "", nil, ibkrlib.ErrIBKRUnavailable
		}
	}
	initLock := m.symInitLock(sym)
	initLock.Lock()
	locked := true
	defer func() {
		if locked {
			initLock.Unlock()
		}
	}()

	m.subsMu.Lock()
	e, exists := m.subs[sym]
	m.subsMu.Unlock()
	if exists && !entryCurrent(e, c) {
		m.subsMu.Lock()
		delete(m.subs, sym)
		m.subsMu.Unlock()
		m.teardown(e, sym)
		exists = false
	}
	subKey := sym

	if !exists {
		// First reference for this symbol — open the IBKR line outside
		// any cross-symbol lock. pkg/ibkr's SubscribeMarketData is itself
		key, err := subscribe(ctx, c)
		if real, ok := c.(*ibkrlib.Connector); ok && !real.SessionCurrent(origin) {
			return nil, "", nil, ibkrlib.ErrIBKRUnavailable
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("subscribe %s: %w", sym, err)
		}
		if strings.TrimSpace(key) != "" {
			subKey = key
		}
		if subKey != sym {
			initLock.Unlock()
			locked = false
			initLock = m.symInitLock(subKey)
			initLock.Lock()
			locked = true
			m.subsMu.Lock()
			e, exists = m.subs[subKey]
			m.subsMu.Unlock()
		}
		if exists && !entryCurrent(e, c) {
			m.subsMu.Lock()
			delete(m.subs, subKey)
			m.subsMu.Unlock()
			m.teardown(e, subKey)
			exists = false
		}
		if !exists {
			e = &subEntry{
				sym:   subKey,
				owner: c,
				taps:  map[*frameTap]struct{}{},
				stop:  make(chan struct{}),
			}
			if real, ok := c.(*ibkrlib.Connector); ok {
				binding, ready := origin, real.SessionCurrent(origin)
				e.current = func() bool { return ready && real.SessionCurrent(binding) }
				e.cancelSource = func(ctx context.Context, key string) error {
					if !ready {
						return nil
					}
					return real.UnsubscribeSharedMarketDataForSession(ctx, binding, key)
				}
			}
			m.subsMu.Lock()
			m.subs[subKey] = e
			m.subsMu.Unlock()
			go m.tickLoop(e)
		}
	}

	var tap *frameTap
	if addTap {
		tap = &frameTap{ch: make(chan rpc.Frame, 16)}
	}
	e.mu.Lock()
	if tap != nil {
		e.taps[tap] = struct{}{}
	}
	e.refcount++
	seedExisting := tap != nil && e.emitted
	e.mu.Unlock()
	if seedExisting {
		if md, ok := c.MarketDataSnapshot()[subKey]; ok {
			frame := buildFrame(md, marketDataTypeName(md.FeedType))
			select {
			case tap.ch <- frame:
			default:
			}
		}
	}
	return tap, subKey, e, nil
}

// Subscribe acquires a market-data reference for sym and returns a frame
// channel that delivers coalesced ticks until release is called or a
// terminal error frame arrives. release is always safe to call exactly
// once (typically via defer).
//
// When this is the first reference to sym, an IBKR market-data line is
// opened and the per-symbol tick loop spins up. Subsequent Subscribe
// callers attach a new tap onto the existing fan-out.
//
// ctx bounds the cold-path slot-acquire on first reference; see acquire.
func (m *subManager) Subscribe(ctx context.Context, sym string) (<-chan rpc.Frame, func(), error) {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return nil, func() {}, errors.New("subscribe: symbol required")
	}
	tap, key, entry, err := m.acquire(ctx, sym, true, func(subCtx context.Context, c ibkrMarketConnector) (string, error) {
		return sym, c.SubscribeMarketData(subCtx, sym, defaultGenericTicks)
	})
	if err != nil {
		return nil, func() {}, err
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		m.release(key, tap, entry)
	}
	return tap.ch, release, nil
}

func (m *subManager) SubscribeContract(ctx context.Context, contract ibkrlib.Contract) (<-chan rpc.Frame, func(), error) {
	key := ibkrlib.MarketDataKeyForContract(contract)
	if key == "" {
		return nil, func() {}, errors.New("subscribe: contract symbol required")
	}
	tap, actualKey, entry, err := m.acquire(ctx, key, true, func(subCtx context.Context, c ibkrMarketConnector) (string, error) {
		return c.SubscribeMarketDataWithContract(subCtx, contract, defaultGenericTicks)
	})
	if err != nil {
		return nil, func() {}, err
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		m.release(actualKey, tap, entry)
	}
	return tap.ch, release, nil
}

// Hold acquires a market-data reference without subscribing to the frame
// fan-out. Used by the snapshot path: it wants the IBKR line to stay open
func (m *subManager) Hold(ctx context.Context, sym string) (func(), error) {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return func() {}, errors.New("hold: symbol required")
	}
	_, key, entry, err := m.acquire(ctx, sym, false, func(subCtx context.Context, c ibkrMarketConnector) (string, error) {
		return sym, c.SubscribeMarketData(subCtx, sym, defaultGenericTicks)
	})
	if err != nil {
		return func() {}, err
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		m.release(key, nil, entry)
	}, nil
}

// release drops a reference. If tap is non-nil it is also removed from the
// fan-out and its channel closed. On the last reference, the IBKR line is
// unsubscribed and the tick loop stopped.
func (m *subManager) release(sym string, tap *frameTap, expected *subEntry) {
	m.releaseContext(context.Background(), sym, tap, expected)
}
func (m *subManager) releaseContext(ctx context.Context, sym string, tap *frameTap, expected *subEntry) {
	initLock := m.symInitLock(sym)
	initLock.Lock()
	defer initLock.Unlock()

	m.subsMu.Lock()
	e, ok := m.subs[sym]
	m.subsMu.Unlock()
	if !ok || e != expected {
		return
	}

	e.mu.Lock()
	if tap != nil {
		if _, present := e.taps[tap]; present {
			delete(e.taps, tap)
			// Closing the tap's channel signals the consumer's range
			// before sending, so no concurrent send can race the close.
			close(tap.ch)
		}
	}
	e.refcount--
	teardown := e.refcount <= 0
	e.mu.Unlock()

	if teardown {
		m.subsMu.Lock()
		delete(m.subs, sym)
		m.subsMu.Unlock()
		m.teardownContext(ctx, e, sym)
	}
}

// teardown stops the tick loop and unsubscribes the IBKR line. The IBKR
func entryCurrent(e *subEntry, c ibkrMarketConnector) bool {
	return e.owner == c && (e.current == nil || e.current())
}
func (m *subManager) teardown(e *subEntry, sym string) {
	m.teardownContext(context.Background(), e, sym)
}
func (m *subManager) teardownContext(ctx context.Context, e *subEntry, sym string) {
	e.stopOnce.Do(func() {
		close(e.stop)
		e.mu.Lock()
		for tap := range e.taps {
			close(tap.ch)
			delete(e.taps, tap)
		}
		e.mu.Unlock()
		if e.current == nil || e.current() {
			if c := e.owner; c != nil {
				if real, ok := c.(*ibkrlib.Connector); ok {
					cleanup, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					if e.cancelSource != nil {
						_ = e.cancelSource(cleanup, sym)
					} else {
						_ = real.UnsubscribeMarketDataContext(cleanup, sym)
					}
				} else {
					_ = c.UnsubscribeMarketData(sym)
				}
			}
		}
	})
}

// tickLoop reads the IBKR market-data cache on the coalesce cadence and
// fans out a Frame to every tap whenever a tick field, the size, or the
// data-type-notice changes. Exits when sub.stop is closed.
//
// Gateway-loss detection is the connector() returning nil mid-stream: in
// that case the loop emits a terminal gateway_lost frame to every tap,
// closes them, and returns.
func (m *subManager) tickLoop(e *subEntry) {
	coalesce := m.coalesce
	if coalesce <= 0 {
		coalesce = defaultCoalesceInterval
	}
	t := time.NewTicker(coalesce)
	defer t.Stop()

	for {
		select {
		case <-e.stop:
			return
		case <-t.C:
			c := m.connector()
			if c == nil || !entryCurrent(e, c) {
				m.emitEntryError(e.sym, e, rpc.FrameErrGatewayLost,
					"IB Gateway connection dropped during streaming subscription")
				return
			}
			e.mu.Lock()
			noReaders := len(e.taps) == 0
			e.mu.Unlock()
			if noReaders {
				continue
			}
			data := c.MarketDataSnapshot()
			md, ok := data[e.sym]
			if !ok {
				continue
			}
			dt := marketDataTypeName(md.FeedType)

			e.mu.Lock()
			if e.emitted &&
				md.Bid == e.lastBid && md.Ask == e.lastAsk && md.Last == e.lastLast &&
				md.BidSize == e.lastBidSize && md.AskSize == e.lastAskSize &&
				dt == e.lastDataType {
				e.mu.Unlock()
				continue
			}
			frame := buildFrame(md, dt)
			for tap := range e.taps {
				select {
				case tap.ch <- frame:
				default:
					// Backpressure: tap is full (consumer not draining
				}
			}
			e.lastBid, e.lastAsk, e.lastLast = md.Bid, md.Ask, md.Last
			e.lastBidSize, e.lastAskSize = md.BidSize, md.AskSize
			e.lastDataType = dt
			e.emitted = true
			e.mu.Unlock()
		}
	}
}

// emitError sends a terminal error frame to every tap on sym, closes the
// tap channels, removes the entry, and unsubscribes the IBKR line. Idempotent
func (m *subManager) emitError(sym string, code, message string) {
	m.emitEntryError(sym, nil, code, message)
}
func (m *subManager) emitEntryError(sym string, expected *subEntry, code, message string) {
	initLock := m.symInitLock(sym)
	initLock.Lock()
	defer initLock.Unlock()

	frame := rpc.Frame{
		T:     time.Now(),
		Error: &rpc.FrameError{Code: code, Message: message},
	}
	m.subsMu.Lock()
	e, ok := m.subs[sym]
	if ok && expected != nil && e != expected {
		m.subsMu.Unlock()
		return
	}
	if ok {
		delete(m.subs, sym)
	}
	m.subsMu.Unlock()
	if !ok {
		return
	}

	e.mu.Lock()
	for tap := range e.taps {
		select {
		case tap.ch <- frame:
		default:
			// Consumer hasn't drained the buffer — fall back to closing
		}
		close(tap.ch)
	}
	e.taps = map[*frameTap]struct{}{}
	e.mu.Unlock()
	m.teardown(e, sym)
}

// Close emits a daemon_shutdown frame to every active subscription and
// tears them down. Called from Server.Stop so MCP clients and CLI watchers
// see a structured terminal frame instead of an opaque socket close.
func (m *subManager) Close() {
	m.subsMu.Lock()
	syms := make([]string, 0, len(m.subs))
	for sym := range m.subs {
		syms = append(syms, sym)
	}
	m.subsMu.Unlock()
	for _, sym := range syms {
		m.emitError(sym, rpc.FrameErrDaemonShutdown, "canary daemon shutting down")
	}
}

// activeCount reports the number of distinct symbols currently held by
// the manager. Used by tests; safe to call concurrently.
func (m *subManager) activeCount() int {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	return len(m.subs)
}

// buildFrame projects an *ibkrlib.MarketData snapshot into the wire frame
// shape, using nil pointers for fields the gateway hasn't delivered yet.
// Note: the original code lifted on `!= 0` for all fields, but bid/ask/last
// only carry positive prices and negative values would be a protocol bug,
// so ptrIfPos is the safer semantic.
func buildFrame(md *ibkrlib.MarketData, dt string) rpc.Frame {
	if dt == "" && (md.Last > 0 || md.Bid > 0 || md.Ask > 0) {
		dt = rpc.MarketDataUnknown
	}
	return rpc.Frame{
		T:        time.Now(),
		DataType: dt,
		Bid:      ptrIfPos(md.Bid),
		Ask:      ptrIfPos(md.Ask),
		Last:     ptrIfPos(md.Last),
		BidSize:  ptrIfPos(md.BidSize),
		AskSize:  ptrIfPos(md.AskSize),
	}
}

// HoldContract retains a shared routed line without a polling frame tap.
func (m *subManager) HoldContract(ctx context.Context, contract ibkrlib.Contract) (string, func(context.Context), error) {
	key := ibkrlib.MarketDataKeyForContract(contract)
	if key == "" {
		return "", func(context.Context) {}, errors.New("contract symbol required")
	}
	_, actual, entry, err := m.acquire(ctx, key, false, func(ctx context.Context, c ibkrMarketConnector) (string, error) {
		return c.SubscribeMarketDataWithContract(ctx, contract, defaultGenericTicks)
	})
	if err != nil {
		return "", func(context.Context) {}, err
	}
	var once sync.Once
	return actual, func(ctx context.Context) { once.Do(func() { m.releaseContext(ctx, actual, nil, entry) }) }, nil
}
