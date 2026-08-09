package ibkr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const defaultOpenOrderSnapshotProtocolTimeout = 15 * time.Second

// ErrOpenOrderSnapshotPoisoned means a reqAllOpenOrders request was sent but
// its uncorrelated openOrderEnd terminator was not proven on the same socket.
// No second request is safe on that socket generation because late callbacks
// could otherwise complete the wrong snapshot.
var ErrOpenOrderSnapshotPoisoned = errors.New("open-order snapshot socket generation poisoned")

// OpenOrderSnapshot is a one-shot read of the API-created open orders the
// gateway reports across client IDs. It does not bind or include manual TWS
// orders merely because Complete is true; those require the separate client-0
// open-order binding flow, which this request does not perform.
//
// Complete is true only when openOrderEnd arrived on the exact Connection
// socket epoch that sent reqAllOpenOrders. When Complete is false, Orders may
// contain callbacks collected before the caller or protocol deadline ended,
// but that proves nothing about absent orders. A request failure returns no
// collected orders. AsOf is the local UTC completion or failure time, not a
// broker timestamp. Session is the opaque Connection socket generation that
// sent the request, and Generation is the Connector order-lifecycle
// frontier at the exact same-epoch openOrderEnd; a change to either invalidates
// this receipt.
type OpenOrderSnapshot struct {
	Complete   bool                    // Complete reports whether same-epoch openOrderEnd arrived.
	Orders     []OrderLifecycleEvent   // Orders contains collected openOrder events and may be empty.
	AsOf       time.Time               // AsOf is the local UTC time at which the evidence completed.
	Session    ConnectorSessionBinding // Session identifies the exact Connection socket generation.
	Generation uint64                  // Generation is the order-event frontier captured at completion.
}

type openOrderSnapshotBinding struct {
	conn  *Connection
	epoch uint64
}

func sameOpenOrderSnapshotBinding(a, b openOrderSnapshotBinding) bool {
	return a.conn != nil && a.conn == b.conn && a.epoch == b.epoch
}

// openOrderSnapshotCollector accumulates openOrder callbacks between one
// reqAllOpenOrders request and its same-epoch openOrderEnd terminator.
type openOrderSnapshotCollector struct {
	mu         sync.Mutex
	orders     []OrderLifecycleEvent
	end        chan struct{}
	closed     bool
	generation uint64
}

func (col *openOrderSnapshotCollector) add(ev OrderLifecycleEvent) {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.closed {
		return
	}
	col.orders = append(col.orders, ev)
}

func (col *openOrderSnapshotCollector) finish(generation uint64) {
	col.mu.Lock()
	defer col.mu.Unlock()
	if col.closed {
		return
	}
	col.closed = true
	col.generation = generation
	close(col.end)
}

func (col *openOrderSnapshotCollector) snapshot() ([]OrderLifecycleEvent, uint64) {
	col.mu.Lock()
	defer col.mu.Unlock()
	return append([]OrderLifecycleEvent{}, col.orders...), col.generation
}

type openOrderSnapshotFlight struct {
	binding   openOrderSnapshotBinding
	collector *openOrderSnapshotCollector
	done      chan struct{}
	result    OpenOrderSnapshot
	err       error
}

// RequestAllOpenOrders sends reqAllOpenOrders to the gateway. Results arrive
// asynchronously as openOrder and orderStatus callbacks followed by
// openOrderEnd; a nil error means only that the request frame was accepted for
// the socket write. This read request is available in both build modes.
func (c *Connection) RequestAllOpenOrders() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	epoch, err := c.captureBrokerInstructionEpoch()
	if err != nil {
		return err
	}
	return c.requestAllOpenOrdersForEpoch(epoch)
}

func (c *Connection) requestAllOpenOrdersForEpoch(epoch uint64) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	msg := c.encodeMsg(reqAllOpenOrders, "1")
	return c.sendMessageWithTypeContextForEpoch(context.Background(), msg, RequestTypeOrder, epoch, true)
}

// SnapshotOpenOrders joins or starts one epoch-bound reqAllOpenOrders flight.
// Caller cancellation only stops that waiter; after the wire request begins,
// the collector remains installed until same-epoch openOrderEnd or the
// internal protocol deadline. This is required because the protocol has no
// request ID and a late terminator could otherwise bless a later flight.
//
// A canceled waiter issues no late request. A protocol timeout or uncertain
// send poisons only the exact Connection epoch; callers fail closed until a
// reconnect advances that epoch.
func (c *Connector) SnapshotOpenOrders(ctx context.Context) (OpenOrderSnapshot, error) {
	if c == nil {
		return OpenOrderSnapshot{}, fmt.Errorf("connector is nil")
	}
	if ctx == nil {
		return OpenOrderSnapshot{}, fmt.Errorf("context is nil")
	}
	if err := ctx.Err(); err != nil {
		return OpenOrderSnapshot{AsOf: time.Now().UTC()}, err
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return OpenOrderSnapshot{}, fmt.Errorf("no active connection")
	}
	binding := openOrderSnapshotBinding{conn: conn, epoch: conn.BrokerSessionEpoch()}

	c.openOrderSnapshotMu.Lock()
	if sameOpenOrderSnapshotBinding(c.openOrderSnapshotPoison, binding) {
		c.openOrderSnapshotMu.Unlock()
		return OpenOrderSnapshot{AsOf: time.Now().UTC(), Session: ConnectorSessionBinding{connector: c, connection: binding.conn, epoch: binding.epoch}}, ErrOpenOrderSnapshotPoisoned
	}
	flight := c.openOrderSnapshot
	if flight == nil || !sameOpenOrderSnapshotBinding(flight.binding, binding) {
		flight = &openOrderSnapshotFlight{
			binding:   binding,
			collector: &openOrderSnapshotCollector{end: make(chan struct{})},
			done:      make(chan struct{}),
		}
		c.openOrderSnapshot = flight
		go c.runOpenOrderSnapshotFlight(flight)
	}
	c.openOrderSnapshotMu.Unlock()

	select {
	case <-flight.done:
		return cloneOpenOrderSnapshot(flight.result), flight.err
	case <-ctx.Done():
		orders, generation := flight.collector.snapshot()
		return OpenOrderSnapshot{
			Complete:   false,
			Orders:     orders,
			AsOf:       time.Now().UTC(),
			Session:    ConnectorSessionBinding{connector: c, connection: flight.binding.conn, epoch: flight.binding.epoch},
			Generation: generation,
		}, ctx.Err()
	}
}

func (c *Connector) runOpenOrderSnapshotFlight(flight *openOrderSnapshotFlight) {
	if beforeSend := c.openOrderSnapshotBeforeSend; beforeSend != nil {
		beforeSend()
	}
	request := c.requestAllOpenOrders
	if request == nil {
		request = func() error { return flight.binding.conn.requestAllOpenOrdersForEpoch(flight.binding.epoch) }
	}
	if err := request(); err != nil {
		c.completeOpenOrderSnapshotFlight(flight, OpenOrderSnapshot{AsOf: time.Now().UTC(), Session: ConnectorSessionBinding{connector: c, connection: flight.binding.conn, epoch: flight.binding.epoch}}, err, brokerSendMayHaveBeenWritten(err))
		return
	}
	timeout := c.openOrderSnapshotTimeout
	if timeout <= 0 {
		timeout = defaultOpenOrderSnapshotProtocolTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-flight.collector.end:
		orders, generation := flight.collector.snapshot()
		c.completeOpenOrderSnapshotFlight(flight, OpenOrderSnapshot{
			Complete:   true,
			Orders:     orders,
			AsOf:       time.Now().UTC(),
			Session:    ConnectorSessionBinding{connector: c, connection: flight.binding.conn, epoch: flight.binding.epoch},
			Generation: generation,
		}, nil, false)
	case <-timer.C:
		err := fmt.Errorf("%w: no same-epoch openOrderEnd before protocol deadline", ErrOpenOrderSnapshotPoisoned)
		orders, generation := flight.collector.snapshot()
		c.completeOpenOrderSnapshotFlight(flight, OpenOrderSnapshot{
			Complete:   false,
			Orders:     orders,
			AsOf:       time.Now().UTC(),
			Session:    ConnectorSessionBinding{connector: c, connection: flight.binding.conn, epoch: flight.binding.epoch},
			Generation: generation,
		}, err, true)
	}
}

func (c *Connector) completeOpenOrderSnapshotFlight(flight *openOrderSnapshotFlight, result OpenOrderSnapshot, err error, poison bool) {
	flight.result = cloneOpenOrderSnapshot(result)
	flight.err = err
	c.openOrderSnapshotMu.Lock()
	if poison {
		c.openOrderSnapshotPoison = flight.binding
	}
	if c.openOrderSnapshot == flight {
		c.openOrderSnapshot = nil
	}
	close(flight.done)
	c.openOrderSnapshotMu.Unlock()
}

func cloneOpenOrderSnapshot(in OpenOrderSnapshot) OpenOrderSnapshot {
	out := in
	out.Orders = append([]OrderLifecycleEvent{}, in.Orders...)
	return out
}

func (c *Connector) collectOpenOrderSnapshotFrom(conn *Connection, epoch uint64, ev OrderLifecycleEvent) {
	c.openOrderSnapshotMu.Lock()
	flight := c.openOrderSnapshot
	if flight == nil || !sameOpenOrderSnapshotBinding(flight.binding, openOrderSnapshotBinding{conn: conn, epoch: epoch}) {
		c.openOrderSnapshotMu.Unlock()
		return
	}
	collector := flight.collector
	c.openOrderSnapshotMu.Unlock()
	collector.add(ev)
}

func (c *Connector) finishOpenOrderSnapshotFrom(conn *Connection, epoch uint64) {
	c.openOrderSnapshotMu.Lock()
	flight := c.openOrderSnapshot
	if flight == nil || !sameOpenOrderSnapshotBinding(flight.binding, openOrderSnapshotBinding{conn: conn, epoch: epoch}) {
		c.openOrderSnapshotMu.Unlock()
		return
	}
	collector := flight.collector
	c.openOrderSnapshotMu.Unlock()
	collector.finish(c.OrderLifecycleGeneration())
}
