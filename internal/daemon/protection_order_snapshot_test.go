package daemon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func newProtectionOrderSnapshotTestServer(now *time.Time) (*Server, *ibkrlib.Connector) {
	port := 4002
	connector := ibkrlib.NewConnector(&ibkrlib.ConnectorConfig{})
	server := &Server{
		cfg:            &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, Account: "DU-PROTECTION"}},
		endpoint:       discover.Endpoint{Host: "127.0.0.1", Port: port, Account: "DU-PROTECTION"},
		connector:      connector,
		connectorEpoch: 1,
		now:            func() time.Time { return now.UTC() },
		serverCtx:      context.Background(),
	}
	return server, connector
}

func TestProtectionOrderSnapshotCacheAvoidsPhaseOffsetRequests(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server, connector := newProtectionOrderSnapshotTestServer(&now)
	var calls atomic.Int32
	server.orderSnapshotFn = func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		calls.Add(1)
		return ibkrlib.OpenOrderSnapshot{
			Complete: true, AsOf: now, Generation: connector.OrderLifecycleGeneration(),
		}, nil
	}

	binding := server.currentProtectionOrderSnapshotBinding()
	first, err := server.protectionSnapshotOpenOrders(t.Context(), binding)
	if err != nil || !first.Complete {
		t.Fatalf("first snapshot=%+v err=%v", first, err)
	}
	now = now.Add(30 * time.Second)
	binding = server.currentProtectionOrderSnapshotBinding()
	second, err := server.protectionSnapshotOpenOrders(t.Context(), binding)
	if err != nil || !second.AsOf.Equal(first.AsOf) {
		t.Fatalf("phase-offset cache read=%+v err=%v, want first receipt", second, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("30-second heartbeats issued %d broker snapshots, want one", got)
	}

	now = now.Add(16 * time.Second)
	binding = server.currentProtectionOrderSnapshotBinding()
	third, err := server.protectionSnapshotOpenOrders(t.Context(), binding)
	if err != nil || !third.AsOf.Equal(now) {
		t.Fatalf("due refresh=%+v err=%v, want receipt at %s", third, err, now)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("refresh after 46 seconds calls=%d, want two", got)
	}
}

func TestProtectionOrderSnapshotSingleflightOutlivesCanceledWaiter(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server, connector := newProtectionOrderSnapshotTestServer(&now)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	server.orderSnapshotFn = func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return ibkrlib.OpenOrderSnapshot{
			Complete: true, AsOf: now, Generation: connector.OrderLifecycleGeneration(),
		}, nil
	}
	binding := server.currentProtectionOrderSnapshotBinding()
	type outcome struct {
		snapshot ibkrlib.OpenOrderSnapshot
		err      error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		snapshot, err := server.protectionSnapshotOpenOrders(t.Context(), binding)
		firstDone <- outcome{snapshot: snapshot, err: err}
	}()
	<-entered

	waiterCtx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := server.protectionSnapshotOpenOrders(waiterCtx, binding); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled waiter err=%v, want deadline exceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("canceled waiter launched %d flights, want one", got)
	}
	close(release)
	select {
	case got := <-firstDone:
		if got.err != nil || !got.snapshot.Complete {
			t.Fatalf("surviving flight=%+v err=%v", got.snapshot, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("surviving flight did not complete")
	}
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatalf("cached receipt after canceled waiter: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached read after surviving flight calls=%d, want one", got)
	}
}

func TestProtectionOrderSnapshotCacheRejectsBindingAndAgeDrift(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server, connector := newProtectionOrderSnapshotTestServer(&now)
	var calls atomic.Int32
	server.orderSnapshotFn = func(context.Context) (ibkrlib.OpenOrderSnapshot, error) {
		calls.Add(1)
		return ibkrlib.OpenOrderSnapshot{
			Complete: true, AsOf: now, Generation: connector.OrderLifecycleGeneration(),
		}, nil
	}

	binding := server.currentProtectionOrderSnapshotBinding()
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	server.protectionOrderSnapshotMu.Lock()
	server.protectionOrderSnapshotCache.binding.generation++
	server.protectionOrderSnapshotMu.Unlock()
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatalf("generation mismatch refresh: %v", err)
	}

	server.mu.Lock()
	server.connectorEpoch++
	server.mu.Unlock()
	binding = server.currentProtectionOrderSnapshotBinding()
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatalf("connector-epoch refresh: %v", err)
	}

	server.cfg.Gateway.Account = "U-PROTECTION"
	server.mu.Lock()
	server.endpoint.Account = "U-PROTECTION"
	server.endpoint.Port = 4001
	server.mu.Unlock()
	binding = server.currentProtectionOrderSnapshotBinding()
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatalf("scope refresh: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("binding refresh calls=%d, want four", got)
	}

	server.protectionOrderSnapshotMu.Lock()
	server.protectionOrderSnapshotCache.succeededAt = now
	server.protectionOrderSnapshotCache.snapshot.AsOf = now.Add(-protectionOrderSnapshotMaxAge - time.Second)
	server.protectionOrderSnapshotMu.Unlock()
	if _, err := server.protectionSnapshotOpenOrders(t.Context(), binding); err != nil {
		t.Fatalf("stale receipt refresh: %v", err)
	}
	if got := calls.Load(); got != 5 {
		t.Fatalf("stale receipt calls=%d, want five", got)
	}
}
