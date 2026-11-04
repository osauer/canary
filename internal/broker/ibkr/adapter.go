// Package ibkr adapts one *ibkr.Connector to the read seam in
// internal/broker. The adapter maps between the seam's neutral types and the
// client's own; it holds no state of its own beyond the side table of
// concrete session bindings, and the daemon builds one adapter per connector
// and discards it with the connector.
package ibkr

import (
	"sync"

	"github.com/osauer/canary/v2/internal/broker"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Adapter is one IBKR connector behind the broker seam. Generation is the
// daemon's count of connectors it has published; a neutral binding carries
// it so a binding captured on an earlier connector is never current on this
// one even when the new connection's epoch counter repeats an old value.
type Adapter struct {
	conn       *ibkrlib.Connector
	generation uint64
	// logf receives the adapter's own operational lines (the delayed-data
	// retry); nil keeps it silent.
	logf func(format string, args ...any)

	mu sync.Mutex
	// nextEpoch issues the neutral epoch numbers; the first is 1, so a zero
	// binding is never current.
	nextEpoch uint64
	// concrete maps a neutral epoch to the connector's own binding and
	// epochOf is its inverse. ibkrlib.ConnectorSessionBinding has unexported
	// fields and no constructor, so the concrete value is kept here and
	// handed back to the connector; a neutral binding is never rebuilt into
	// a concrete one.
	concrete map[uint64]ibkrlib.ConnectorSessionBinding
	epochOf  map[ibkrlib.ConnectorSessionBinding]uint64
}

// New wraps conn as the daemon's generation-th connector. logf may be nil.
func New(conn *ibkrlib.Connector, generation uint64, logf func(string, ...any)) *Adapter {
	return &Adapter{
		conn:       conn,
		generation: generation,
		logf:       logf,
		concrete:   map[uint64]ibkrlib.ConnectorSessionBinding{},
		epochOf:    map[ibkrlib.ConnectorSessionBinding]uint64{},
	}
}

// Kind is always IBKR.
func (a *Adapter) Kind() broker.Kind { return broker.KindIBKR }

// Connector is the wrapped client, for the IBKR-only paths that still call
// it directly (the order path, the P&L stream, option chains).
func (a *Adapter) Connector() *ibkrlib.Connector {
	if a == nil {
		return nil
	}
	return a.conn
}

// Generation is the daemon's generation this adapter was built for.
func (a *Adapter) Generation() uint64 {
	if a == nil {
		return 0
	}
	return a.generation
}

// Wraps reports whether this adapter is the one built for conn.
func (a *Adapter) Wraps(conn *ibkrlib.Connector) bool {
	return a != nil && conn != nil && a.conn == conn
}

// Capture returns the binding of the connector's current ready session.
func (a *Adapter) Capture() (broker.SessionBinding, bool) {
	if a == nil || a.conn == nil {
		return broker.SessionBinding{}, false
	}
	cb, ok := a.conn.CaptureSession()
	if !ok {
		return broker.SessionBinding{}, false
	}
	return a.register(cb), true
}

// BindingFor returns the neutral binding for a concrete binding the daemon
// captured on this adapter's connector. A zero concrete binding, or one of
// another connector, yields the zero neutral binding, which is never current.
func (a *Adapter) BindingFor(cb ibkrlib.ConnectorSessionBinding) broker.SessionBinding {
	if a == nil || a.conn == nil || cb == (ibkrlib.ConnectorSessionBinding{}) {
		return broker.SessionBinding{}
	}
	// SessionReceiptCurrent and SessionCurrent both refuse a binding of
	// another connector; the side table keeps only this connector's.
	if !a.conn.SessionCurrent(cb) && !a.conn.SessionReceiptCurrent(cb) {
		return broker.SessionBinding{}
	}
	return a.register(cb)
}

func (a *Adapter) register(cb ibkrlib.ConnectorSessionBinding) broker.SessionBinding {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, seen := a.epochOf[cb]; seen {
		return broker.SessionBinding{Kind: broker.KindIBKR, Generation: a.generation, Epoch: e}
	}
	// A new epoch means the connection moved on; drop the bindings that
	// are no longer current so the table stays the size of the live set.
	for e, old := range a.concrete {
		if !a.conn.SessionCurrent(old) && !a.conn.SessionReceiptCurrent(old) {
			delete(a.concrete, e)
			delete(a.epochOf, old)
		}
	}
	a.nextEpoch++
	a.concrete[a.nextEpoch] = cb
	a.epochOf[cb] = a.nextEpoch
	return broker.SessionBinding{Kind: broker.KindIBKR, Generation: a.generation, Epoch: a.nextEpoch}
}

// concreteFor returns the connector's own binding behind b, if b was
// captured on this adapter.
func (a *Adapter) concreteFor(b broker.SessionBinding) (ibkrlib.ConnectorSessionBinding, bool) {
	if a == nil || b.IsZero() || b.Kind.Normalized() != broker.KindIBKR || b.Generation != a.generation {
		return ibkrlib.ConnectorSessionBinding{}, false
	}
	a.mu.Lock()
	cb, ok := a.concrete[b.Epoch]
	a.mu.Unlock()
	return cb, ok
}

// Current reports whether b still names this connector's ready session.
func (a *Adapter) Current(b broker.SessionBinding) bool {
	cb, ok := a.concreteFor(b)
	return ok && a.conn.SessionCurrent(cb)
}

// Health describes the session for the status surface.
func (a *Adapter) Health() broker.SessionHealth {
	h := broker.SessionHealth{Kind: broker.KindIBKR}
	if a == nil || a.conn == nil {
		return h
	}
	h.Connected = a.conn.IsConnected()
	h.Ready = a.conn.IsReady()
	h.BackendDown = a.conn.BackendLink().Down
	h.Account = a.conn.AccountID()
	h.ServerVersion = a.conn.ServerVersion()
	h.LastError = a.conn.LastError()
	return h
}

func (a *Adapter) logfSafe(format string, args ...any) {
	if a != nil && a.logf != nil {
		a.logf(format, args...)
	}
}
