package ibkr

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The rate limiter paces every outbound frame under IBKR's 50 messages per
// second envelope (40/s here), books the slower historical budget, keeps
// broker writes and heartbeats off the background lane, retries a failed
// send with backoff, and lets a caller or Stop revoke a queued request
// before it can reach the socket. Timing assertions use lower bounds with
// generous margins only.

func ordTLimiter(t *testing.T) *RateLimiter {
	t.Helper()
	rl := NewRateLimiter(t.Context())
	t.Cleanup(rl.Stop)
	return rl
}

func TestTokenBucketReservationAndCancellation(t *testing.T) {
	tb := NewTokenBucket(5, 1000)
	if err := tb.WaitForTokens(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := tb.WaitForTokens(t.Context(), 6); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("over-capacity request: %v", err)
	}
	if !tb.TryAcquire(0) || !tb.TryAcquire(5) || tb.TryAcquire(1) {
		t.Fatal("TryAcquire accounting")
	}
	// A wait that is cancelled gives its reservation back; the bucket is not
	// left in debt for a send that never happened.
	slow := NewTokenBucket(1, 0.001)
	if !slow.TryAcquire(1) {
		t.Fatal("drain")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := slow.WaitForTokens(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled wait: %v", err)
	}
	slow.mu.Lock()
	tokens := slow.tokens
	slow.mu.Unlock()
	if tokens < -0.01 {
		t.Fatalf("reservation not returned: %.3f tokens", tokens)
	}
	// A bucket that never refills cannot reserve; the wait polls until ctx ends.
	frozen := NewTokenBucket(1, 0)
	frozen.TryAcquire(1)
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	if err := frozen.WaitForTokens(ctx2, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("frozen bucket: %v", err)
	}
	// A reservation into debt is honoured after the refill delay.
	fast := NewTokenBucket(1, 200)
	fast.TryAcquire(1)
	start := time.Now()
	if err := fast.WaitForTokens(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 2*time.Millisecond {
		t.Fatal("debt reservation returned before the refill")
	}
	// A clock that runs backwards never mints tokens.
	back := NewTokenBucket(2, 10)
	back.TryAcquire(2)
	back.mu.Lock()
	back.refillLocked(back.lastRefill.Add(-time.Hour))
	tokens = back.tokens
	back.mu.Unlock()
	if tokens != 0 {
		t.Fatalf("backwards clock minted %.3f tokens", tokens)
	}
}

func TestSemaphoreTryAcquireAndRelease(t *testing.T) {
	s := NewSemaphore(1)
	if !s.TryAcquire() || s.TryAcquire() || s.Count() != 1 {
		t.Fatal("TryAcquire")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("full semaphore wait: %v", err)
	}
	s.Release()
	if s.Count() != 0 {
		t.Fatal("release")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("over-release did not panic")
		}
	}()
	s.Release()
}

func TestRequestPriorityLanes(t *testing.T) {
	//lint:ignore SA1012 the nil-context refusal is the behaviour under test
	if RequestPriorityFrom(t.Context()) != PriorityInteractive || RequestPriorityFrom(nil) != PriorityInteractive {
		t.Fatal("default lane")
	}
	bg := WithRequestPriority(t.Context(), PriorityBackground)
	if RequestPriorityFrom(bg) != PriorityBackground {
		t.Fatal("background lane not carried")
	}
	for _, rt := range []RequestType{RequestTypeOrder, RequestTypeHeartbeat} {
		if effectivePriority(bg, rt) != PriorityInteractive {
			t.Fatalf("type %d queued behind the background pool", rt)
		}
	}
	for _, rt := range []RequestType{RequestTypeGeneral, RequestTypeMarketData, RequestTypeHistorical, RequestTypeMarketDataCancel} {
		if effectivePriority(bg, rt) != PriorityBackground {
			t.Fatalf("type %d lost its background lane", rt)
		}
	}

	rl := ordTLimiter(t)
	sent := 0
	if err := rl.SubmitContext(bg, RequestTypeGeneral, func() error { sent++; return nil }); err != nil || sent != 1 {
		t.Fatalf("background submit: %v sent=%d", err, sent)
	}
	// With every background slot held, a background read waits for a slot
	// and leaves with its caller; an order on the same context does not wait.
	for range backgroundMessageSlots {
		if !rl.backgroundMessage.TryAcquire() {
			t.Fatal("pool")
		}
	}
	defer func() {
		for range backgroundMessageSlots {
			rl.backgroundMessage.Release()
		}
	}()
	short, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	err := rl.SubmitContext(short, RequestTypeMarketData, func() error { t.Error("sent without a slot"); return nil })
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("background without a slot: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for rl.GetMetrics().ThrottledRequests == 0 {
		if time.Now().After(deadline) {
			t.Fatal("slot wait not counted as throttled")
		}
		time.Sleep(time.Millisecond)
	}
	sent = 0
	if err := rl.SubmitContext(bg, RequestTypeOrder, func() error { sent++; return nil }); err != nil || sent != 1 {
		t.Fatalf("order behind a full background pool: %v sent=%d", err, sent)
	}
}

func TestRateLimiterRetriesWithBackoffAndHonoursRevocation(t *testing.T) {
	rl := ordTLimiter(t)
	var calls atomic.Int32
	start := time.Now()
	err := rl.SubmitWithRetriesContextFunc(t.Context(), RequestTypeGeneral, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("transient")
		}
		return nil
	}, 1)
	if err != nil || calls.Load() != 2 {
		t.Fatalf("retry: %v calls=%d", err, calls.Load())
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("retry without backoff: %s", time.Since(start))
	}
	// Retries exhausted: the last error is returned.
	calls.Store(0)
	if err := rl.SubmitWithRetriesContextFunc(t.Context(), RequestTypeGeneral, func(context.Context) error { calls.Add(1); return errors.New("always") }, 0); err == nil || calls.Load() != 1 {
		t.Fatalf("no retries: %v calls=%d", err, calls.Load())
	}
	// A failure that is the caller's own cancellation, or the session
	// ending under the send, is never retried.
	for name, sendErr := range map[string]error{"cancelled": context.Canceled, "session ended": errOutboundSessionEnded} {
		calls.Store(0)
		err := rl.SubmitWithRetriesContextFunc(t.Context(), RequestTypeGeneral, func(context.Context) error { calls.Add(1); return sendErr }, 3)
		if !errors.Is(err, sendErr) || calls.Load() != 1 {
			t.Fatalf("%s retried: %v calls=%d", name, err, calls.Load())
		}
	}
	// The caller leaving during the backoff ends the request with its error.
	ctx, cancel := context.WithCancel(t.Context())
	calls.Store(0)
	done := make(chan error, 1)
	go func() {
		done <- rl.SubmitWithRetriesContextFunc(ctx, RequestTypeGeneral, func(context.Context) error { calls.Add(1); return errors.New("transient") }, 3)
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("cancel during backoff: %v calls=%d", err, calls.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancel during backoff did not return")
	}
	// Stop during the backoff ends it too, with the limiter's own refusal.
	stopped := NewRateLimiter(t.Context())
	calls.Store(0)
	go func() {
		done <- stopped.SubmitWithRetriesContextFunc(t.Context(), RequestTypeGeneral, func(context.Context) error { calls.Add(1); return errors.New("transient") }, 3)
	}()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	stopped.Stop()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("stop during backoff: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop during backoff did not return")
	}
	if err := stopped.Submit(RequestTypeGeneral, func() error { t.Error("sent after stop"); return nil }); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("submit after stop: %v", err)
	}
}

func TestRateLimiterSubmitTimeoutRevokesTheQueuedSend(t *testing.T) {
	rl := ordTLimiter(t)
	rl.submitTimeoutFn = func(RequestType) time.Duration { return 50 * time.Millisecond }
	var sawCancel atomic.Bool
	err := rl.SubmitWithRetriesContextFunc(t.Context(), RequestTypeGeneral, func(ctx context.Context) error {
		<-ctx.Done()
		sawCancel.Store(true)
		return ctx.Err()
	}, 0)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("timed-out submit: %v", err)
	}
	for !sawCancel.Load() {
		time.Sleep(time.Millisecond)
	}
	if rl.GetMetrics().RejectedRequests != 1 {
		t.Fatalf("rejected count %d", rl.GetMetrics().RejectedRequests)
	}
	if submitTimeout(RequestTypeHistorical) <= submitTimeout(RequestTypeGeneral) {
		t.Fatal("historical submissions need the longer completion budget")
	}
	// A caller that is already gone never queues.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := rl.SubmitContext(cancelled, RequestTypeGeneral, func() error { t.Error("sent"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled submit: %v", err)
	}
	// An open circuit refuses reads and orders but lets heartbeats and
	// subscription cancels through, and closes itself after the cooldown.
	rl.openCircuit(time.Now().Add(time.Hour))
	if err := rl.Submit(RequestTypeGeneral, func() error { return nil }); err == nil || !strings.Contains(err.Error(), "circuit") {
		t.Fatalf("open circuit admitted a read: %v", err)
	}
	if err := rl.checkCircuit(RequestTypeOrder); err == nil {
		t.Fatal("open circuit admitted an order")
	}
	for _, rt := range []RequestType{RequestTypeHeartbeat, RequestTypeMarketDataCancel} {
		if err := rl.checkCircuit(rt); err != nil {
			t.Fatalf("type %d refused by the circuit: %v", rt, err)
		}
	}
	rl.openCircuit(time.Now().Add(-time.Second))
	if err := rl.checkCircuit(RequestTypeGeneral); err == nil {
		t.Fatal("an earlier close did not shorten the open circuit")
	}
	rl.circuitMu.Lock()
	rl.circuitOpenUntil = time.Now().Add(-time.Second)
	rl.circuitMu.Unlock()
	if err := rl.checkCircuit(RequestTypeGeneral); err != nil {
		t.Fatalf("expired circuit still open: %v", err)
	}
	// Five pacing errors inside the cooldown open the circuit; a sixth
	// after the cooldown starts a fresh count.
	rl.recordRateLimitError()
	rl.metricsMu.Lock()
	rl.metrics.LastRateLimitError = time.Now().Add(-2 * rl.circuitCooldown)
	rl.metricsMu.Unlock()
	rl.recordRateLimitError()
	if rl.GetMetrics().ConsecutiveErrors != 1 {
		t.Fatalf("stale error count kept: %d", rl.GetMetrics().ConsecutiveErrors)
	}
	for range rl.circuitThreshold - 1 {
		rl.recordRateLimitError()
	}
	if err := rl.checkCircuit(RequestTypeGeneral); err == nil {
		t.Fatal("threshold did not open the circuit")
	}
}

// Fifty sends through the limiter take at least the time the 40/s bucket
// needs to refill the ten beyond its capacity. Every send happens exactly
// once and in arrival order.
func TestRateLimiterPacesSendsUnderTheMessageEnvelope(t *testing.T) {
	rl := ordTLimiter(t)
	var order []int
	var mu sync.Mutex
	start := time.Now()
	for i := range 50 {
		if err := rl.Submit(RequestTypeGeneral, func() error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 150*time.Millisecond {
		t.Fatalf("50 sends in %s: the 40/s envelope was not applied", elapsed)
	}
	if len(order) != 50 {
		t.Fatalf("%d sends", len(order))
	}
	for i, v := range order {
		if v != i {
			t.Fatalf("sends out of order at %d: %v", i, order)
		}
	}
	m := rl.GetMetrics()
	if m.TotalRequests != 50 || m.CurrentQueueDepth != 0 {
		t.Fatalf("metrics %+v", m)
	}
	// Historical requests also book the slow bucket and a concurrency slot.
	if err := rl.Submit(RequestTypeHistorical, func() error {
		if rl.historicalConcurrent.Count() != 1 {
			t.Error("historical slot not held during the send")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if rl.historicalConcurrent.Count() != 0 {
		t.Fatal("historical slot not released")
	}
	rl.historicalRate.mu.Lock()
	historical := rl.historicalRate.tokens
	rl.historicalRate.mu.Unlock()
	if historical > 59.5 {
		t.Fatalf("historical budget not debited: %.2f", historical)
	}
	// Market-data slots are a separate pool, released by the connector.
	if err := rl.AcquireMarketDataSlot(t.Context()); err != nil {
		t.Fatal(err)
	}
	rl.ReleaseMarketDataSlot()
	if rl.marketDataSubs.Count() != 0 {
		t.Fatal("market data slot")
	}
}

// A request still queued when Stop runs ends with the limiter's refusal;
// a caller whose context ends while queued gets its own error.
func TestRateLimiterStopAndCallerCancellationDrainTheQueue(t *testing.T) {
	rl := NewRateLimiter(t.Context())
	// Block the single dispatcher behind a send that waits on the message
	// bucket by draining it first.
	rl.messageRate.mu.Lock()
	rl.messageRate.tokens = -2000
	rl.messageRate.lastRefill = time.Now()
	rl.messageRate.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 2)
	go func() {
		results <- rl.SubmitContext(ctx, RequestTypeGeneral, func() error { t.Error("sent"); return nil })
	}()
	go func() { results <- rl.Submit(RequestTypeGeneral, func() error { t.Error("sent"); return nil }) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	first := <-results
	if !errors.Is(first, context.Canceled) {
		t.Fatalf("caller cancellation: %v", first)
	}
	rl.Stop()
	// Stop races the dispatcher's own cancellation of the admitted send:
	// either the limiter's refusal or the cancelled wait comes back, never
	// a send.
	second := <-results
	if second == nil || (!strings.Contains(second.Error(), "stopped") && !errors.Is(second, context.Canceled)) {
		t.Fatalf("stop: %v", second)
	}
	// Nothing is left queued and the dispatcher goroutines have exited.
	if rl.GetMetrics().CurrentQueueDepth != 0 {
		t.Fatal("queue not drained")
	}
}
