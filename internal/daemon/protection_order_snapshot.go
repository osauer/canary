package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	// The Protection producer ticks every 30 seconds. Reusing one successful
	// receipt for 45 seconds removes phase-offset duplicate reqAllOpenOrders
	// calls while still forcing a refresh at least every other heartbeat.
	protectionOrderSnapshotRefreshEvery = 45 * time.Second
)

var errProtectionOrderSnapshotBindingChanged = errors.New("protection open-order snapshot binding changed")

type protectionOrderSnapshotBinding struct {
	scope          brokerStateScope
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	session        ibkrlib.ConnectorSessionBinding
	generation     uint64
}

func sameProtectionOrderSnapshotSession(a, b protectionOrderSnapshotBinding) bool {
	return sameBrokerScope(a.scope, b.scope) && a.connector != nil && a.connector == b.connector &&
		a.connectorEpoch == b.connectorEpoch && a.session == b.session
}

func sameProtectionOrderSnapshotBinding(a, b protectionOrderSnapshotBinding) bool {
	return sameProtectionOrderSnapshotSession(a, b) && a.generation == b.generation
}

func (s *Server) protectionOrderSnapshotBindingCurrent(binding protectionOrderSnapshotBinding) bool {
	current := s.currentProtectionOrderSnapshotBinding()
	if !sameProtectionOrderSnapshotBinding(binding, current) {
		return false
	}
	// orderSnapshotFn is an in-process test seam and has no live socket token.
	// Production always requires the Connector itself to validate the opaque
	// Connection+epoch binding.
	if s.orderSnapshotFn != nil && binding.session == (ibkrlib.ConnectorSessionBinding{}) {
		return true
	}
	return binding.connector != nil && binding.connector.SessionCurrent(binding.session)
}

type protectionOrderSnapshotCache struct {
	binding     protectionOrderSnapshotBinding
	snapshot    ibkrlib.OpenOrderSnapshot
	succeededAt time.Time
}

type protectionOrderSnapshotFlight struct {
	binding  protectionOrderSnapshotBinding
	done     chan struct{}
	snapshot ibkrlib.OpenOrderSnapshot
	err      error
}

func (s *Server) currentProtectionOrderSnapshotBinding() protectionOrderSnapshotBinding {
	if s == nil {
		return protectionOrderSnapshotBinding{}
	}
	s.mu.Lock()
	ep, connector, connectorEpoch := s.endpoint, s.connector, s.connectorEpoch
	s.mu.Unlock()
	configuredAccount := ""
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
	}
	connectedAccount := ""
	if connector != nil {
		connectedAccount = connector.AccountID()
	}
	port := ep.Port
	if port == 0 && s.cfg != nil && s.cfg.Gateway.Port != nil {
		port = *s.cfg.Gateway.Port
	}
	binding := protectionOrderSnapshotBinding{
		scope:     brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount),
		connector: connector, connectorEpoch: connectorEpoch,
	}
	if connector != nil {
		binding.session, _ = connector.CaptureSession()
		binding.generation = connector.OrderLifecycleGeneration()
	}
	return binding
}

func protectionOrderSnapshotUsable(snapshot ibkrlib.OpenOrderSnapshot, now time.Time) bool {
	if !snapshot.Complete || snapshot.AsOf.IsZero() {
		return false
	}
	now = now.UTC()
	asOf := snapshot.AsOf.UTC()
	return !asOf.After(now) && now.Sub(asOf) <= protectionOrderSnapshotMaxAge
}

func cloneProtectionOrderSnapshot(snapshot ibkrlib.OpenOrderSnapshot) ibkrlib.OpenOrderSnapshot {
	copySnapshot := snapshot
	copySnapshot.Orders = append([]ibkrlib.OrderLifecycleEvent(nil), snapshot.Orders...)
	return copySnapshot
}

func (s *Server) cachedProtectionOrderSnapshot(binding protectionOrderSnapshotBinding, now time.Time) (ibkrlib.OpenOrderSnapshot, bool) {
	if s == nil || !brokerScopeConcrete(binding.scope) || binding.connector == nil {
		return ibkrlib.OpenOrderSnapshot{}, false
	}
	s.protectionOrderSnapshotMu.Lock()
	cache := s.protectionOrderSnapshotCache
	s.protectionOrderSnapshotMu.Unlock()
	now = now.UTC()
	if cache.succeededAt.IsZero() || cache.succeededAt.After(now) || now.Sub(cache.succeededAt.UTC()) > protectionOrderSnapshotRefreshEvery ||
		!sameProtectionOrderSnapshotBinding(cache.binding, binding) || !protectionOrderSnapshotUsable(cache.snapshot, now) {
		return ibkrlib.OpenOrderSnapshot{}, false
	}
	return cloneProtectionOrderSnapshot(cache.snapshot), true
}

// protectionSnapshotOpenOrders returns a short-lived complete broker inventory
// receipt for the Protection producer heartbeat. The actual reqAllOpenOrders
// flight follows daemon lifetime rather than any one waiter: a short caller
// cancellation cannot tear down the uncorrelated protocol collector for other
// waiters. Cache publication requires an exact final scope, connector epoch,
// and order-lifecycle frontier match.
func (s *Server) protectionSnapshotOpenOrders(ctx context.Context, binding protectionOrderSnapshotBinding) (ibkrlib.OpenOrderSnapshot, error) {
	if s == nil || ctx == nil {
		return ibkrlib.OpenOrderSnapshot{}, fmt.Errorf("protection open-order snapshot unavailable")
	}
	if err := ctx.Err(); err != nil {
		return ibkrlib.OpenOrderSnapshot{}, err
	}
	if !brokerScopeConcrete(binding.scope) || binding.connector == nil || !s.protectionOrderSnapshotBindingCurrent(binding) {
		return ibkrlib.OpenOrderSnapshot{}, errProtectionOrderSnapshotBindingChanged
	}
	now := s.orderNow()
	if cached, ok := s.cachedProtectionOrderSnapshot(binding, now); ok {
		return cached, nil
	}

	s.protectionOrderSnapshotMu.Lock()
	// Recheck after taking the single-flight lock so a concurrently completed
	// flight is reused without launching a duplicate wire request.
	cache := s.protectionOrderSnapshotCache
	if !cache.succeededAt.IsZero() && !cache.succeededAt.After(now) && now.Sub(cache.succeededAt.UTC()) <= protectionOrderSnapshotRefreshEvery &&
		sameProtectionOrderSnapshotBinding(cache.binding, binding) && protectionOrderSnapshotUsable(cache.snapshot, now) {
		snapshot := cloneProtectionOrderSnapshot(cache.snapshot)
		s.protectionOrderSnapshotMu.Unlock()
		return snapshot, nil
	}
	flight := s.protectionOrderSnapshotFlight
	if flight == nil || !sameProtectionOrderSnapshotSession(flight.binding, binding) {
		flight = &protectionOrderSnapshotFlight{binding: binding, done: make(chan struct{})}
		s.protectionOrderSnapshotFlight = flight
		go s.runProtectionOrderSnapshotFlight(flight)
	}
	s.protectionOrderSnapshotMu.Unlock()

	select {
	case <-flight.done:
		return cloneProtectionOrderSnapshot(flight.snapshot), flight.err
	case <-ctx.Done():
		return ibkrlib.OpenOrderSnapshot{}, ctx.Err()
	}
}

func (s *Server) runProtectionOrderSnapshotFlight(flight *protectionOrderSnapshotFlight) {
	base := context.Background()
	if s.serverCtx != nil {
		base = s.serverCtx
	}
	flightCtx, cancel := context.WithTimeout(base, orderReconcileSnapshotWait)
	defer cancel()

	var snapshot ibkrlib.OpenOrderSnapshot
	var err error
	if s.orderSnapshotFn != nil {
		snapshot, err = s.orderSnapshotFn(flightCtx)
	} else {
		snapshot, err = flight.binding.connector.SnapshotOpenOrders(flightCtx)
	}
	now := s.orderNow()
	completionBinding := flight.binding
	completionBinding.session = snapshot.Session
	completionBinding.generation = snapshot.Generation
	if err == nil && !protectionOrderSnapshotUsable(snapshot, now) {
		err = fmt.Errorf("protection open-order snapshot is incomplete or stale")
	}
	if err == nil && !s.protectionOrderSnapshotBindingCurrent(completionBinding) {
		err = errProtectionOrderSnapshotBindingChanged
	}

	s.protectionOrderSnapshotMu.Lock()
	if err == nil {
		s.protectionOrderSnapshotCache = protectionOrderSnapshotCache{
			binding: completionBinding, snapshot: cloneProtectionOrderSnapshot(snapshot), succeededAt: now.UTC(),
		}
	}
	flight.snapshot = cloneProtectionOrderSnapshot(snapshot)
	flight.err = err
	if s.protectionOrderSnapshotFlight == flight {
		s.protectionOrderSnapshotFlight = nil
	}
	close(flight.done)
	s.protectionOrderSnapshotMu.Unlock()
}
