package ibkr

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MarketSnapshot is the result of one [Connector.FetchMarketSnapshot] request.
// Nil price pointers mean the broker did not provide that field. The one-shot
// request does not request generic ticks, so IV is nil and IVStatus is
// "unavailable". AsOf is the local completion time, not a broker tick timestamp.
type MarketSnapshot struct {
	Symbol         string
	Bid            *float64
	Ask            *float64
	Last           *float64
	IV             *float64
	IVStatus       string // "real" when populated from tick 106, "unavailable" otherwise.
	AsOf           time.Time
	MarketDataType int // 1=live, 2=frozen, 3=delayed, 4=delayed-frozen, 0=unknown
}

const defaultMarketSnapshotTimeout = 5 * time.Second

// snapshotSession is the per-call state for a FetchMarketSnapshot
// invocation. Handlers receive a pointer to it via closure; this design
// also makes the handler methods directly testable without setting up a
// network connection.
type snapshotSession struct {
	targetReqID atomic.Int64
	snap        *MarketSnapshot
	gotEndCh    chan struct{}
	mu          sync.Mutex
	dataType    int
}

func newSnapshotSession(symbol string) *snapshotSession {
	s := &snapshotSession{
		snap: &MarketSnapshot{
			Symbol:   symbol,
			IVStatus: "unavailable",
		},
		gotEndCh: make(chan struct{}, 1),
	}
	s.targetReqID.Store(-1)
	return s
}

// FetchMarketSnapshot requests a one-shot bid, ask, and last-price snapshot and
// waits for the broker's end marker. ctx must be non-nil. A timeout of zero or
// less uses five seconds. Context cancellation returns ctx.Err; timeout returns
// [context.DeadlineExceeded]. Both paths best-effort cancel the request and
// release its market-data slot. Inactive and unavailable connectors return
// [ErrSymbolInactive] and [ErrIBKRUnavailable] without sending a request.
func (c *Connector) FetchMarketSnapshot(ctx context.Context, symbol string, timeout time.Duration) (*MarketSnapshot, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	if c.IsSymbolInactive(symbol) {
		return nil, ErrSymbolInactive
	}
	if !c.IsReady() {
		return nil, ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = defaultMarketSnapshotTimeout
	}

	contract, ready := c.prepareContract(symbol, 2*time.Second, true)
	contract, _ = c.waitForContractDetails(symbol, contract, ready)
	contract = c.routeListingForRead(contract)

	session := newSnapshotSession(symbol)

	priceHandID := c.conn.RegisterHandler(msgTickPrice, func(fields []string) {
		c.handleSnapshotTickPrice(session, fields)
	})
	genericHandID := c.conn.RegisterHandler(msgTickGeneric, func(fields []string) {
		c.handleSnapshotTickGeneric(session, fields)
	})
	dataTypeHandID := c.conn.RegisterHandler(msgMarketDataType, func(fields []string) {
		c.handleSnapshotMarketDataType(session, fields)
	})
	endHandID := c.conn.RegisterHandler(msgTickSnapshotEnd, func(fields []string) {
		c.handleSnapshotEnd(session, fields)
	})

	cleanup := func(reqID int) {
		c.conn.UnregisterHandler(msgTickPrice, priceHandID)
		c.conn.UnregisterHandler(msgTickGeneric, genericHandID)
		c.conn.UnregisterHandler(msgMarketDataType, dataTypeHandID)
		c.conn.UnregisterHandler(msgTickSnapshotEnd, endHandID)
		if reqID > 0 && c.isConnected() {
			if cancelErr := c.conn.CancelMarketData(reqID); cancelErr != nil {
				connectorLogger.Debugf("CancelMarketData(reqID=%d) failed: %v", reqID, cancelErr)
			}
		}
	}

	// IBKR (server >= 100) rejects generic-tick lists when snapshot=true with
	// error 321 ("Snapshot market data subscription is not applicable to
	// generic ticks"). Snapshot requests therefore omit the IV/RTH ticks; IV
	// stays unavailable in snapshot results, consistent with the no-fabrication
	// invariant. Streaming subscribers (SubscribeMarketData) still get the
	// generic-tick set since snapshot=false there.
	reqID, err := c.conn.RequestMarketDataWithContract(ctx, contract, "", true, false)
	if err != nil {
		cleanup(0)
		return nil, fmt.Errorf("request market snapshot: %w", err)
	}
	// Atomic guard: handlers compare each tick's reqID against this. Until
	// Store is called, target == -1 and any inbound tick is dropped. The
	// window between RequestMarketDataWithContract returning and Store is
	// microseconds; on a real network IBKR cannot respond that quickly.
	session.targetReqID.Store(int64(reqID))

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	select {
	case <-session.gotEndCh:
		cleanup(reqID)
	case <-deadline.C:
		cleanup(reqID)
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		cleanup(reqID)
		return nil, ctx.Err()
	}

	session.mu.Lock()
	session.snap.MarketDataType = session.dataType
	session.mu.Unlock()
	session.snap.AsOf = time.Now().UTC()
	return session.snap, nil
}

// handleSnapshotTickPrice parses a tick-price message and updates the session
// snapshot. Tick types: 1=bid, 2=ask, 4=last. Non-positive prices are dropped
// — IBKR emits 0/-1 for "no quote available", which is surfaced as nil rather
// than a confusing zero. Messages whose reqID does not match the session
// target are ignored.
func (c *Connector) handleSnapshotTickPrice(s *snapshotSession, fields []string) {
	if len(fields) < 5 {
		return
	}
	target := s.targetReqID.Load()
	if target < 0 {
		return
	}
	rid, err := strconv.Atoi(fields[2])
	if err != nil || int64(rid) != target {
		return
	}
	tickType, err := strconv.Atoi(fields[3])
	if err != nil {
		return
	}
	priceStr := strings.TrimSpace(fields[4])
	if priceStr == "" {
		return
	}
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil || price <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch tickType {
	case 1:
		p := price
		s.snap.Bid = &p
	case 2:
		p := price
		s.snap.Ask = &p
	case 4:
		p := price
		s.snap.Last = &p
	}
}

// handleSnapshotTickGeneric parses generic-tick messages. Only tick 106
// (Option Implied Volatility) is captured — and only when positive. No
// fallback to historical vol; absence of tick 106 leaves IV nil and
// IVStatus="unavailable" per AGENTS.md.
func (c *Connector) handleSnapshotTickGeneric(s *snapshotSession, fields []string) {
	if len(fields) < 5 {
		return
	}
	target := s.targetReqID.Load()
	if target < 0 {
		return
	}
	rid, err := strconv.Atoi(fields[2])
	if err != nil || int64(rid) != target {
		return
	}
	tickType, err := strconv.Atoi(fields[3])
	if err != nil {
		return
	}
	if tickType != 106 {
		return
	}
	val, err := strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
	if err != nil || val <= 0 {
		return
	}
	if val > 1.5 { // IBKR sometimes sends percent-form; normalize to fraction.
		val = val / 100.0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap.IV = &val
	s.snap.IVStatus = "real"
}

// handleSnapshotMarketDataType records the data-type notification IBKR sends
// alongside ticks (1=live, 2=frozen, 3=delayed, 4=delayed-frozen).
func (c *Connector) handleSnapshotMarketDataType(s *snapshotSession, fields []string) {
	if len(fields) < 4 {
		return
	}
	target := s.targetReqID.Load()
	if target < 0 {
		return
	}
	rid, err := strconv.Atoi(fields[2])
	if err != nil || int64(rid) != target {
		return
	}
	dt, err := strconv.Atoi(fields[3])
	if err != nil {
		return
	}
	s.mu.Lock()
	s.dataType = dt
	s.mu.Unlock()
}

// handleSnapshotEnd signals the FetchMarketSnapshot caller that IBKR has
// emitted tickSnapshotEnd for the current request. The channel send is
// non-blocking (buffered with capacity 1) so a duplicate end-message from
// IBKR cannot deadlock.
func (c *Connector) handleSnapshotEnd(s *snapshotSession, fields []string) {
	if len(fields) < 3 {
		return
	}
	target := s.targetReqID.Load()
	if target < 0 {
		return
	}
	rid, err := strconv.Atoi(fields[2])
	if err != nil || int64(rid) != target {
		return
	}
	select {
	case s.gotEndCh <- struct{}{}:
	default:
	}
}
