package spx

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type recoveryFetcherFunc func(context.Context, string, int) ([]Bar, error)

func (f recoveryFetcherFunc) FetchDaily(ctx context.Context, symbol string, days int) ([]Bar, error) {
	return f(ctx, symbol, days)
}

func TestRecoveryGateProbesSeriallyAndBacksOffFailures(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC)
	anchor := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	gateErr := &RecoveryGateError{Cause: errors.New("historical farm disconnected")}
	fake := &FakeBarFetcher{}
	e := newTestEngine(t, fake, func() time.Time { return now }, []string{"SYNTA", "SYNTB", "SYNTC"})
	e.healthGate = func() error { return gateErr }
	var active, calls atomic.Int32
	failed := true
	e.fetcher = recoveryFetcherFunc(func(ctx context.Context, symbol string, days int) ([]Bar, error) {
		if active.Add(1) != 1 {
			t.Error("recovery used parallel fan-out")
		}
		defer active.Add(-1)
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > recoveryReadBudget {
			t.Error("recovery request lacks total budget")
		}
		if failed {
			return nil, errors.New("synthetic response timeout")
		}
		return makeSeries(100, 1, WindowSize, anchor), nil
	})
	if err := e.Refresh(t.Context()); !errors.Is(err, gateErr) || calls.Load() != 1 {
		t.Fatalf("failed probe fanned out: calls=%d err=%v", calls.Load(), err)
	}
	for range 20 {
		_ = e.Refresh(t.Context())
	}
	if calls.Load() != 1 {
		t.Fatal("manual retries bypassed cooldown")
	}
	now = now.Add(time.Minute)
	_ = e.Refresh(t.Context())
	if calls.Load() != 2 {
		t.Fatal("no second recovery probe")
	}
	now = now.Add(time.Minute)
	_ = e.Refresh(t.Context())
	if calls.Load() != 2 {
		t.Fatal("second failure did not increase backoff")
	}
	failed = false
	now = now.Add(time.Minute)
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 5 {
		t.Fatalf("recovery did not advance each constituent: calls=%d", calls.Load())
	}
	if snap, ok := e.Get(); !ok || snap.Coverage != 3 {
		t.Fatalf("missing observed coverage: %+v %v", snap, ok)
	}
	if e.healthGate() != gateErr {
		t.Fatal("successful requests erased the warning")
	}
}

func TestRecoveryGateStopsOnLaterFailureAndKeepsProgress(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC)
	fake := &FakeBarFetcher{Bars: map[string][]Bar{"SYNTA": makeSeries(100, 1, WindowSize, now.Add(-21*time.Hour))}, Errors: map[string]error{"SYNTB": errors.New("synthetic timeout")}}
	e := newTestEngine(t, fake, frozenClock(now), []string{"SYNTA", "SYNTB", "SYNTC"})
	e.healthGate = func() error { return &RecoveryGateError{Cause: errors.New("farm warning")} }
	if err := e.Refresh(t.Context()); err == nil || fake.CallCount() != 2 {
		t.Fatalf("failure did not stop pass: calls=%d err=%v", fake.CallCount(), err)
	}
	if _, ok := e.Get(); ok {
		t.Fatal("partial recovery was published")
	}
	if e.windows["SYNTA"].LastBarAt != "2026-09-25" {
		t.Fatal("successful progress lost")
	}
	if reloaded := New(e.store, fake, Options{}); reloaded.windows["SYNTA"].LastBarAt != "2026-09-25" {
		t.Fatal("progress not checkpointed")
	}
}

func TestRecoveryGateDoesNotTreatEmptyBarsAsRecovery(t *testing.T) {
	now := time.Now()
	fake := &FakeBarFetcher{Bars: map[string][]Bar{"SYNTA": {}}}
	e := newTestEngine(t, fake, frozenClock(now), []string{"SYNTA", "SYNTB"})
	e.healthGate = func() error { return &RecoveryGateError{Cause: errors.New("farm warning")} }
	if err := e.Refresh(t.Context()); err == nil || fake.CallCount() != 1 {
		t.Fatalf("empty probe unlocked next read: %v", err)
	}
}

func TestRecoveryGateHardDisconnectStillStopsAfterSuccess(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC)
	fake := &FakeBarFetcher{Bars: map[string][]Bar{"SYNTA": makeSeries(100, 1, WindowSize, now.Add(-21*time.Hour))}}
	e := newTestEngine(t, fake, frozenClock(now), []string{"SYNTA", "SYNTB"})
	e.healthGate = func() error {
		if fake.CallCount() > 0 {
			return errors.New("TWS/server disconnected")
		}
		return &RecoveryGateError{Cause: errors.New("farm warning")}
	}
	if err := e.Refresh(t.Context()); err == nil || fake.CallCount() != 1 {
		t.Fatalf("hard disconnect was probed: %v", err)
	}
}

func TestRecoveryGateRotatesBadSymbolAndPublishesOnlyObservedCoverage(t *testing.T) {
	now := time.Date(2026, 9, 25, 21, 0, 0, 0, time.UTC)
	anchor := now.Add(-21 * time.Hour)
	members := []string{"SYNTA", "SYNTB", "SYNTC", "SYNTD", "SYNTE"}
	fake := &FakeBarFetcher{Bars: map[string][]Bar{}, Errors: map[string]error{"SYNTA": errors.New("symbol-specific timeout")}}
	for _, symbol := range members[1:] {
		fake.Bars[symbol] = makeSeries(100, 1, WindowSize, anchor)
	}
	e := newTestEngine(t, fake, func() time.Time { return now }, members)
	gateErr := &RecoveryGateError{Cause: errors.New("farm warning")}
	e.healthGate = func() error { return gateErr }
	if err := e.Refresh(t.Context()); !errors.Is(err, gateErr) || fake.CallCount() != 1 {
		t.Fatalf("first failure must stop: calls=%d err=%v", fake.CallCount(), err)
	}
	if _, ok := e.Get(); ok {
		t.Fatal("failed probe published unobserved coverage")
	}
	now = now.Add(time.Minute)
	if err := e.Refresh(t.Context()); !errors.Is(err, gateErr) {
		t.Fatal("warning disappeared", err)
	}
	if fake.Calls[1].Symbol != "SYNTB" || fake.CallCount() != 6 {
		t.Fatalf("unavailable symbol monopolised retries: %+v", fake.Calls)
	}
	if snap, ok := e.Get(); !ok || snap.Coverage != 4 || snap.MemberCount != 5 {
		t.Fatalf("observed 80-percent coverage must publish under the existing threshold: %+v %v", snap, ok)
	}
}
