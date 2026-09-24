package ibkr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func frontFutureTestConnector(t *testing.T) *Connector {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	c.conn, c.running, c.ready = conn, true, true
	return c
}

func TestFrontFutureExpiryFailureAndRecovery(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	now := time.Date(2026, 9, 24, 23, 59, 0, 0, time.UTC)
	var calls atomic.Int32
	fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
		if calls.Add(1) == 1 {
			return rows, nil
		}
		return nil, ErrContractDetailsTimeout
	}
	if _, err := c.frontFuture(t.Context(), want, now, time.Second, fetch); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	for range 2 {
		got, err := c.frontFuture(t.Context(), want, now, time.Second, fetch)
		if !errors.Is(err, ErrContractDetailsTimeout) || got.ConID != 0 {
			t.Fatalf("failed refresh reused expired future: %+v %v", got, err)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("failed resolution ignored backoff")
	}
	rows[0].Expiry = "20261218"
	fetch = func(Contract, time.Duration) ([]ContractDetailsLite, error) { calls.Add(1); return rows, nil }
	got, err := c.frontFuture(t.Context(), want, now.Add(time.Minute), time.Second, fetch)
	if err != nil || got.Expiry != "20261218" || calls.Load() != 3 {
		t.Fatalf("recovery: %+v %v calls=%d", got, err, calls.Load())
	}
}

func TestFrontFutureConcurrentWaitersAndReconnect(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-finish
		}
		return rows, nil
	}
	old := make(chan error, 1)
	go func() { _, err := c.frontFuture(t.Context(), want, now, time.Second, fetch); old <- err }()
	<-started
	c.conn.brokerSessionEpoch.Add(1)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := c.frontFuture(t.Context(), want, now, time.Second, fetch)
			if err != nil || got.ConID != 91 {
				t.Errorf("current session failed: %+v %v", got, err)
			}
		})
	}
	wg.Wait()
	close(finish)
	if err := <-old; err == nil {
		t.Fatal("old session flight published an identity")
	}
	if _, err := c.frontFuture(t.Context(), want, now, time.Second, fetch); err != nil || calls.Load() != 2 {
		t.Fatalf("late old flight replaced new cache: calls=%d err=%v", calls.Load(), err)
	}
}

func TestFrontFutureAcquisitionBoundAndTimeout(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	finish := make(chan struct{})
	var active atomic.Int32
	fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
		active.Add(1)
		<-finish
		return rows, nil
	}
	for i := range frontFutureCacheLimit {
		want.Currency = fmt.Sprintf("SYNTH%d", i)
		if _, err := c.frontFuture(t.Context(), want, now, time.Millisecond, fetch); !errors.Is(err, ErrContractDetailsTimeout) {
			t.Fatalf("short waiter %d: %v", i, err)
		}
	}
	c.conn.brokerSessionEpoch.Add(1)
	want.Currency = "USD"
	if _, err := c.frontFuture(t.Context(), want, now, time.Millisecond, fetch); err == nil {
		t.Fatal("unbounded pending work across reconnect")
	}
	if active.Load() > frontFutureCacheLimit {
		t.Fatal("acquisition limit exceeded")
	}
	close(finish)
	// Drain every bounded worker before the connection cleanup.
	deadline := time.Now().Add(time.Second)
	for {
		c.frontFutureMu.Lock()
		pending := c.frontFutureActive
		c.frontFutureMu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed workers retained")
		}
		time.Sleep(time.Millisecond)
	}
}

func futureTestContract() (Contract, []ContractDetailsLite) {
	want := Contract{Symbol: "SYNTH", SecType: "FUT", Currency: "USD", Exchange: "CME"}
	return want, []ContractDetailsLite{{ConID: 91, Symbol: want.Symbol, SecType: want.SecType, Currency: want.Currency, Exchange: want.Exchange, TradingClass: want.Symbol, Expiry: "20260925"}}
}

func TestFrontFutureReusesCompletedResolution(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
		if calls.Add(1) > 1 {
			return nil, ErrContractDetailsTimeout
		}
		return rows, nil
	}
	for range 2 {
		got, err := c.frontFuture(t.Context(), want, now, time.Second, fetch)
		if err != nil || got.ConID != 91 {
			t.Fatalf("completed broker resolution was lost: conID=%d err=%v", got.ConID, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("completed resolution was re-requested")
	}
}

func TestFrontFutureWireFlightCannotCrossSocketEpoch(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	oldBinding, _ := c.CaptureSession()
	started, finish := make(chan struct{}), make(chan struct{})
	oldDone := make(chan struct{})
	go func() {
		defer close(oldDone)
		_, _ = c.fetchFrontFutureDetails(oldBinding, want, time.Second, func(time.Duration, func([]ContractDetailsLite)) ([]ContractDetailsLite, error) {
			close(started)
			<-finish
			return rows, nil
		})
	}()
	<-started
	c.conn.brokerSessionEpoch.Add(1)
	newBinding, _ := c.CaptureSession()
	newRows := append([]ContractDetailsLite(nil), rows...)
	newRows[0].ConID = 92
	got, err := c.fetchFrontFutureDetails(newBinding, want, 100*time.Millisecond, func(time.Duration, func([]ContractDetailsLite)) ([]ContractDetailsLite, error) { return newRows, nil })
	close(finish)
	<-oldDone
	if err != nil || len(got) != 1 || got[0].ConID != 92 {
		t.Fatalf("new session joined the old contract wire: %+v %v", got, err)
	}
}

func TestFrontFutureLateCompletionSurvivesShortWaiter(t *testing.T) {
	c := frontFutureTestConnector(t)
	want, rows := futureTestContract()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	fetch := func(Contract, time.Duration) ([]ContractDetailsLite, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-finish
		return rows, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := c.frontFuture(ctx, want, now, time.Second, fetch); done <- err }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller: %v", err)
		}
	case <-time.After(time.Second):
		close(finish)
		t.Fatal("cancellation remained blocked on the contract-details wire")
	}
	close(finish)
	got, err := c.frontFuture(t.Context(), want, now, time.Second, fetch)
	if err != nil || got.ConID != 91 || calls.Load() != 1 {
		t.Fatalf("late shared result lost: conID=%d calls=%d err=%v", got.ConID, calls.Load(), err)
	}
}
