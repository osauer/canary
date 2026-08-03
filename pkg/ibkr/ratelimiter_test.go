package ibkr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiterCircuitBreakerTriggers(t *testing.T) {
	rl := NewRateLimiter(context.Background())
	t.Cleanup(rl.Stop)

	rl.circuitThreshold = 2
	rl.circuitCooldown = 200 * time.Millisecond

	sendErr := fmt.Errorf("ERROR 100: rate limit exceeded")

	for i := 0; i < rl.circuitThreshold; i++ {
		err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return sendErr }, 0)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "error 100") {
			t.Fatalf("expected rate limit error, got %v", err)
		}
	}

	err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return sendErr }, 0)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "circuit") {
		t.Fatalf("expected circuit breaker error, got %v", err)
	}

	time.Sleep(rl.circuitCooldown + 50*time.Millisecond)

	if err := rl.SubmitWithRetries(RequestTypeGeneral, func() error { return nil }, 0); err != nil {
		t.Fatalf("expected successful request after cooldown, got %v", err)
	}

	metrics := rl.GetMetrics()
	if metrics.ConsecutiveErrors != 0 {
		t.Fatalf("expected consecutive errors reset, got %d", metrics.ConsecutiveErrors)
	}
}

func TestTokenBucketReservationsStaggerWaiters(t *testing.T) {
	tb := NewTokenBucket(10, 40)
	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	tb.mu.Lock()
	tb.tokens = 0
	tb.lastRefill = now
	tb.mu.Unlock()

	delay, reserved := tb.reserve(1, now)
	if !reserved {
		t.Fatalf("first waiter did not reserve a future token")
	}
	if delay < 24*time.Millisecond || delay > 26*time.Millisecond {
		t.Fatalf("first delay=%s, want about 25ms", delay)
	}

	delay, reserved = tb.reserve(1, now)
	if !reserved {
		t.Fatalf("second waiter did not reserve a future token")
	}
	if delay < 49*time.Millisecond || delay > 51*time.Millisecond {
		t.Fatalf("second delay=%s, want about 50ms", delay)
	}
}

func TestTokenBucketRejectsImpossibleWait(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := NewTokenBucket(1, 1).WaitForTokens(ctx, 2)
	if err == nil || !strings.Contains(err.Error(), "exceeds bucket capacity") {
		t.Fatalf("WaitForTokens err=%v, want capacity error", err)
	}
}

func TestRateLimiterSubmitContextCancelsHistoricalTokenWait(t *testing.T) {
	rl := NewRateLimiter(context.Background())
	t.Cleanup(rl.Stop)

	rl.historicalRate.mu.Lock()
	rl.historicalRate.tokens = 0
	rl.historicalRate.refillRate = 0.001 // one token about every 1000s unless ctx cancels first
	rl.historicalRate.lastRefill = time.Now()
	rl.historicalRate.mu.Unlock()

	var called atomic.Bool
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := rl.SubmitWithRetriesContext(ctx, RequestTypeHistorical, func() error {
		called.Store(true)
		return nil
	}, 3)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SubmitWithRetriesContext err=%v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("SubmitWithRetriesContext took %s, want prompt caller-context cancellation", elapsed)
	}

	time.Sleep(50 * time.Millisecond)
	if called.Load() {
		t.Fatal("historical send function ran after caller context expired")
	}
	if metrics := rl.GetMetrics(); metrics.ConsecutiveErrors != 0 {
		t.Fatalf("context cancellation opened/advanced rate-limit circuit errors: %+v", metrics)
	}
}

func TestEffectivePriorityLanes(t *testing.T) {
	bg := WithRequestPriority(context.Background(), PriorityBackground)
	if got := effectivePriority(context.Background(), RequestTypeGeneral); got != PriorityInteractive {
		t.Fatalf("untagged general lane = %v, want interactive", got)
	}
	if got := effectivePriority(bg, RequestTypeGeneral); got != PriorityBackground {
		t.Fatalf("tagged general lane = %v, want background", got)
	}
	if got := effectivePriority(bg, RequestTypeHistorical); got != PriorityBackground {
		t.Fatalf("tagged historical lane = %v, want background", got)
	}
	// Broker writes and heartbeats never ride the background lane, even
	// under a background-tagged context.
	if got := effectivePriority(bg, RequestTypeOrder); got != PriorityInteractive {
		t.Fatalf("order lane = %v, want interactive", got)
	}
	if got := effectivePriority(bg, RequestTypeHeartbeat); got != PriorityInteractive {
		t.Fatalf("heartbeat lane = %v, want interactive", got)
	}
}

// TestRateLimiterInteractiveOvertakesBackgroundFanout reproduces the
// cold-boot priority inversion: a large background fan-out pre-books the
// message bucket, and an interactive request arriving mid-flight used to
// queue behind every pending reservation. With the background lane, the
// interactive arrival waits behind at most backgroundMessageSlots
// reservations plus whatever is already past token acquisition.
func TestRateLimiterInteractiveOvertakesBackgroundFanout(t *testing.T) {
	rl := NewRateLimiter(context.Background())
	t.Cleanup(rl.Stop)

	// Slow, small bucket: 1-token capacity refilling at 50/s keeps a
	// 100-request fan-out about 2 s deep in token debt without the pool.
	rl.messageRate = NewTokenBucket(1, 50)

	const fanout = 100
	bgCtx := WithRequestPriority(context.Background(), PriorityBackground)
	var completed atomic.Int32
	var wg sync.WaitGroup
	for range fanout {
		wg.Go(func() {
			_ = rl.SubmitWithRetriesContext(bgCtx, RequestTypeGeneral, func() error {
				completed.Add(1)
				return nil
			}, 0)
		})
	}

	// Let the fan-out saturate the pool, then measure how many background
	// sends complete between the interactive arrival and its completion.
	time.Sleep(100 * time.Millisecond)
	before := completed.Load()
	if err := rl.SubmitWithRetriesContext(context.Background(), RequestTypeGeneral, func() error { return nil }, 0); err != nil {
		t.Fatalf("interactive submit: %v", err)
	}
	overtaken := completed.Load() - before
	wg.Wait()

	if got := completed.Load(); got != fanout {
		t.Fatalf("background fan-out incomplete: %d of %d sends ran", got, fanout)
	}
	// Bound: the pool's reservations plus generous scheduling slack.
	// Without the background lane this reads ~90 (the fan-out's tail).
	if limit := int32(backgroundMessageSlots + 6); overtaken > limit {
		t.Fatalf("interactive request waited behind %d background sends, want <= %d", overtaken, limit)
	}
}

// TestRateLimiterBackgroundPoolHonorsCancellation parks background
// requests behind a drained, never-refilling bucket so the pool fills,
// then proves a background arrival waiting for a pool slot leaves
// promptly when its caller context is canceled.
func TestRateLimiterBackgroundPoolHonorsCancellation(t *testing.T) {
	rl := NewRateLimiter(context.Background())
	t.Cleanup(rl.Stop)

	rl.messageRate.mu.Lock()
	rl.messageRate.tokens = 0
	rl.messageRate.refillRate = 0
	rl.messageRate.mu.Unlock()

	bgCtx := WithRequestPriority(context.Background(), PriorityBackground)
	for range backgroundMessageSlots + 2 {
		go func() {
			_ = rl.SubmitWithRetriesContext(bgCtx, RequestTypeGeneral, func() error { return nil }, 0)
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	for rl.backgroundMessage.Count() < backgroundMessageSlots {
		if time.Now().After(deadline) {
			t.Fatalf("background pool never filled: %d of %d slots held", rl.backgroundMessage.Count(), backgroundMessageSlots)
		}
		time.Sleep(5 * time.Millisecond)
	}

	waitCtx, cancel := context.WithCancel(bgCtx)
	errCh := make(chan error, 1)
	go func() {
		errCh <- rl.SubmitWithRetriesContext(waitCtx, RequestTypeGeneral, func() error { return nil }, 0)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled background submit err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background submit did not return after caller cancellation")
	}
}

// TestRateLimiterStopRace exercises concurrent Submit/Stop. Before the fix,
// Stop closed requestQueue while Submit and the retry goroutine could still
// send to it, occasionally panicking with "send on closed channel".
func TestRateLimiterStopRace(t *testing.T) {
	for range 25 {
		rl := NewRateLimiter(context.Background())

		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				for range 50 {
					_ = rl.SubmitWithRetries(RequestTypeGeneral, func() error { return nil }, 1)
				}
			})
		}

		time.Sleep(2 * time.Millisecond)
		rl.Stop()
		wg.Wait()
	}
}
