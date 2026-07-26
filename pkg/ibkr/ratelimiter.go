package ibkr

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/pkg/ibkr/internal/logging"
)

var rateLimiterLogger = logging.Component("IBKR RateLimiter")

// RateLimiter manages rate limiting for IBKR API requests. The IBKR limits it
// enforces are recorded on the individual buckets and semaphores below.
type RateLimiter struct {
	// Token buckets for different rate limits
	messageRate    *TokenBucket // General messages: 40/sec (safe from 50/sec max)
	historicalRate *TokenBucket // Historical data: 60 requests per 10 minutes

	// Semaphores for concurrent limits
	historicalConcurrent *Semaphore // Max 50 concurrent historical requests
	marketDataSubs       *Semaphore // Max 100 concurrent market data subscriptions (retail)

	// Queue for pacing requests
	requestQueue chan *RateLimitedRequest

	// Metrics
	metrics   *RateLimiterMetrics
	metricsMu sync.RWMutex

	// Circuit breaker for repeated rate limit violations
	circuitMu        sync.Mutex
	circuitOpenUntil time.Time
	circuitThreshold int
	circuitCooldown  time.Duration

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// submitTimeoutFn is an internal test seam for proving that a request
	// canceled by the limiter's own completion deadline cannot run later.
	// Production leaves it nil and uses submitTimeout.
	submitTimeoutFn func(RequestType) time.Duration
}

// RateLimiterMetrics tracks rate limiting statistics
type RateLimiterMetrics struct {
	TotalRequests        uint64
	ThrottledRequests    uint64
	RejectedRequests     uint64
	CurrentQueueDepth    int
	MessageRatePerSec    float64
	HistoricalRatePerMin float64
	LastRateLimitError   time.Time
	ConsecutiveErrors    int
}

// RateLimitedRequest wraps a request with rate limiting metadata
type RateLimitedRequest struct {
	Type       RequestType
	Context    context.Context
	SendFunc   func(context.Context) error
	ResultChan chan error
	Timestamp  time.Time
	Retries    int
	MaxRetries int
}

// RequestType categorizes different IBKR request types for proper rate limiting
type RequestType int

// Request types select the limiter bucket and pacing policy used for a call.
const (
	RequestTypeGeneral RequestType = iota
	RequestTypeMarketData
	RequestTypeHistorical
	RequestTypeOrder
	RequestTypeHeartbeat
)

// TokenBucket implements token bucket algorithm for rate limiting
type TokenBucket struct {
	capacity   int     // Max tokens
	tokens     float64 // Current tokens (float for fractional refill)
	refillRate float64 // Tokens per second
	lastRefill time.Time
	mu         sync.Mutex
}

// NewTokenBucket creates a new token bucket
func NewTokenBucket(capacity int, refillRate float64) *TokenBucket {
	return &TokenBucket{
		capacity:   capacity,
		tokens:     float64(capacity), // Start full
		refillRate: refillRate,
		lastRefill: time.Now(),
	}
}

// TryAcquire attempts to acquire n tokens, returns true if successful
func (tb *TokenBucket) TryAcquire(n int) bool {
	if n <= 0 {
		return true
	}

	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(time.Now())
	if tb.tokens < float64(n) {
		return false
	}
	tb.tokens -= float64(n)
	return true
}

// WaitForTokens blocks until n tokens are available
func (tb *TokenBucket) WaitForTokens(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	if n > tb.capacity {
		return fmt.Errorf("token request %d exceeds bucket capacity %d", n, tb.capacity)
	}

	for {
		delay, reserved := tb.reserve(n, time.Now())
		if delay == 0 {
			return nil
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if reserved {
				tb.cancelReservation(n, time.Now())
			}
			return ctx.Err()
		case <-timer.C:
			if reserved {
				return nil
			}
		}
	}
}

func (tb *TokenBucket) reserve(n int, now time.Time) (time.Duration, bool) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(now)

	if tb.tokens >= float64(n) {
		tb.tokens -= float64(n)
		return 0, true
	}
	if tb.refillRate <= 0 {
		return time.Second, false
	}

	needed := float64(n) - tb.tokens
	delay := max(time.Duration(needed/tb.refillRate*float64(time.Second)), time.Millisecond)
	tb.tokens -= float64(n)
	return delay, true
}

func (tb *TokenBucket) refillLocked(now time.Time) {
	elapsed := now.Sub(tb.lastRefill).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	tb.tokens = min(float64(tb.capacity), tb.tokens+elapsed*tb.refillRate)
	tb.lastRefill = now
}

func (tb *TokenBucket) cancelReservation(n int, now time.Time) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.refillLocked(now)
	tb.tokens = min(float64(tb.capacity), tb.tokens+float64(n))
}

// Semaphore limits concurrent operations
type Semaphore struct {
	ch chan struct{}
}

// NewSemaphore creates a semaphore with given capacity
func NewSemaphore(capacity int) *Semaphore {
	return &Semaphore{
		ch: make(chan struct{}, capacity),
	}
}

// Acquire blocks until a slot is available
func (s *Semaphore) Acquire(ctx context.Context) error {
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire attempts to acquire without blocking
func (s *Semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot. Panics if the semaphore is empty — an over-release
// is always a bookkeeping bug at the caller (mismatched Acquire/Release pair),
// and silently absorbing it would mask the root cause.
func (s *Semaphore) Release() {
	select {
	case <-s.ch:
	default:
		panic("ibkr: Semaphore.Release called without matching Acquire")
	}
}

// Count returns current number of acquired slots
func (s *Semaphore) Count() int {
	return len(s.ch)
}

// NewRateLimiter creates a rate limiter with IBKR-compliant limits
func NewRateLimiter(ctx context.Context) *RateLimiter {
	ctx, cancel := context.WithCancel(ctx)

	rl := &RateLimiter{
		// Message rate: 40/sec (safe from 50/sec max)
		messageRate: NewTokenBucket(40, 40),

		// Historical: 60 requests per 10 minutes = 0.1 requests/sec
		historicalRate: NewTokenBucket(60, 0.1),

		// Concurrent limits
		historicalConcurrent: NewSemaphore(50),  // Max 50 concurrent historical
		marketDataSubs:       NewSemaphore(100), // Max 100 market data subscriptions

		// Request queue with buffer
		requestQueue: make(chan *RateLimitedRequest, 1000),

		metrics:          &RateLimiterMetrics{},
		ctx:              ctx,
		cancel:           cancel,
		circuitThreshold: 5,
		circuitCooldown:  10 * time.Second,
	}

	// Start request processor
	rl.wg.Add(1)
	go rl.processRequests()

	// Start metrics updater
	rl.wg.Add(1)
	go rl.updateMetrics()

	return rl
}

// Stop gracefully shuts down the rate limiter. The request queue is not
// closed: producers (Submit and the retry goroutine) race with shutdown and
// would panic on send to a closed channel. Instead, ctx cancellation signals
// both producers and consumers to exit, and the queue is GC'd once unreferenced.
func (rl *RateLimiter) Stop() {
	rl.cancel()
	rl.wg.Wait()
}

// Submit submits a request for rate-limited execution with the default
// retry count (3). For one-shot requests where any failure should bubble
// straight back to the caller (heartbeat path, test fixtures), use
// SubmitWithRetries(reqType, sendFunc, 0).
func (rl *RateLimiter) Submit(reqType RequestType, sendFunc func() error) error {
	return rl.SubmitWithRetries(reqType, sendFunc, 3)
}

// SubmitContext submits a request for rate-limited execution and cancels the
// queue/token wait when ctx is done. The send function should also check ctx
// if it may block after the limiter admits it.
func (rl *RateLimiter) SubmitContext(ctx context.Context, reqType RequestType, sendFunc func() error) error {
	return rl.SubmitWithRetriesContext(ctx, reqType, sendFunc, 3)
}

// submitTimeout caps how long Submit waits for a queued request to
// complete before giving up. The cap is type-aware: RequestTypeHistorical
// must accommodate the slow historicalRate bucket (60 tokens, refills at
// 0.1/sec — IBKR's 60-per-10-min pacing window). When a fan-out empties
// the bucket — common during the breadth-spx 500-name cold-start — the
// next refill arrives 10 s later, and with multiple workers competing
// the back-of-queue wait can approach the bucket's full drain time
// (~10 min). 12 min gives one minute of headroom past that worst case.
//
// The previous unconditional 30 s cap silently rejected most historical
// requests in any large fan-out: the engine at internal/breadth/spx/engine.go
// documented a ~60 min cold-start expectation that the 30 s cap made
// unachievable. General and market-data requests retain the original 30 s
// budget — those buckets refill fast enough that a longer wait would
// hide a genuine stall.
func submitTimeout(reqType RequestType) time.Duration {
	switch reqType {
	case RequestTypeHistorical:
		return 12 * time.Minute
	default:
		return 30 * time.Second
	}
}

// SubmitWithRetries submits a request with a custom retry count. The queue
// is strictly FIFO — a higher-priority "queue jump" parameter existed
// before v0.16.0 but processRequests never read it (no priority queue was
// ever wired); removing it killed the dead API in favour of an honest one.
func (rl *RateLimiter) SubmitWithRetries(reqType RequestType, sendFunc func() error, maxRetries int) error {
	return rl.SubmitWithRetriesContext(context.Background(), reqType, sendFunc, maxRetries)
}

// SubmitWithRetriesContext is SubmitWithRetries plus caller-owned
// cancellation. This matters for interactive historical reads: the historical
// bucket can legitimately wait minutes during breadth fan-out, but a CLI/RPC
// request with a 60 s budget must leave that queue promptly when its caller is
// gone.
func (rl *RateLimiter) SubmitWithRetriesContext(ctx context.Context, reqType RequestType, sendFunc func() error, maxRetries int) error {
	return rl.SubmitWithRetriesContextFunc(ctx, reqType, func(context.Context) error {
		return sendFunc()
	}, maxRetries)
}

// SubmitWithRetriesContextFunc passes the limiter-owned request context to the
// admitted callback. In addition to caller cancellation, that context is
// canceled synchronously by the limiter's own completion timeout, so work
// already admitted but parked behind another transport cannot run late.
func (rl *RateLimiter) SubmitWithRetriesContextFunc(ctx context.Context, reqType RequestType, sendFunc func(context.Context) error, maxRetries int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rl.checkCircuit(reqType); err != nil {
		return err
	}

	requestCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	req := &RateLimitedRequest{
		Type:       reqType,
		Context:    requestCtx,
		SendFunc:   sendFunc,
		ResultChan: make(chan error, 1),
		Timestamp:  time.Now(),
		MaxRetries: maxRetries,
	}

	// Try to queue the request. Check ctx first so a shutdown in progress
	// returns immediately instead of racing the send.
	select {
	case <-rl.ctx.Done():
		return fmt.Errorf("rate limiter stopped")
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	timeout := submitTimeout(reqType)
	if rl.submitTimeoutFn != nil {
		timeout = rl.submitTimeoutFn(reqType)
	}
	select {
	case rl.requestQueue <- req:
		// Wait for result with timeout
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-req.ResultChan:
			return err
		case <-timer.C:
			// Revoke the queued dispatch synchronously before returning. This
			// closes the scheduler gap where the caller could observe a timeout
			// and the request could still acquire a token and reach SendFunc.
			cancelRequest()
			rl.incrementRejected()
			return fmt.Errorf("request timeout after %s", timeout)
		case <-ctx.Done():
			cancelRequest()
			return ctx.Err()
		case <-rl.ctx.Done():
			cancelRequest()
			return fmt.Errorf("rate limiter stopped")
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-rl.ctx.Done():
		return fmt.Errorf("rate limiter stopped")
	default:
		// Queue is full
		rl.incrementRejected()
		rl.recordRateLimitError()
		return fmt.Errorf("request queue full (1000 pending)")
	}
}

// processRequests dispatches each queued request to its own goroutine.
// Per-request goroutines remove head-of-line blocking: a slow request
// stuck in WaitForTokens (e.g. a historical-data request waiting on the
// 0.1/sec refill of historicalRate) no longer blocks unrelated requests
// behind it in the FIFO. Before this dispatcher change, a 500-name
// breadth-spx fan-out would jam the queue so badly that contract-detail
// requests (RequestTypeGeneral, expected to clear in milliseconds) sat
// behind historical waits and failed their 5 s caller timeouts —
// surfacing as "contract details unresolved" in the daemon log.
//
// Per-request concurrency is bounded by the rate-limit primitives
// themselves: messageRate (40 tokens/sec) for every type, historicalRate
// (0.1/sec) and historicalConcurrent (50 slots) for historical, and the
// queue's own 1000-deep buffer for the dispatcher itself. All three are
// thread-safe, so spawning is the simplest correct change here.
func (rl *RateLimiter) processRequests() {
	defer rl.wg.Done()

	for {
		select {
		case <-rl.ctx.Done():
			return
		case req := <-rl.requestQueue:
			rl.wg.Add(1)
			go rl.dispatch(req)
		}
	}
}

// dispatch runs one request's rate-limited execution. Extracted from the
// processRequests loop body so per-request goroutines have a clean
// completion-and-retry boundary. Always closes the request out via
// req.ResultChan unless the request is re-queued for retry.
func (rl *RateLimiter) dispatch(req *RateLimitedRequest) {
	defer rl.wg.Done()

	err := rl.executeRequest(req)
	if err != nil && rl.shouldRetry(req, err) {
		req.Retries++
		// Re-queue with exponential backoff. Honor rl.ctx so a
		// shutdown mid-backoff doesn't leak a goroutine sleeping
		// out the remainder of the delay.
		rl.wg.Add(1)
		go func(backoff time.Duration) {
			defer rl.wg.Done()
			ctx := req.Context
			if ctx == nil {
				ctx = context.Background()
			}
			timer := time.NewTimer(backoff)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				req.ResultChan <- ctx.Err()
				return
			case <-rl.ctx.Done():
				req.ResultChan <- fmt.Errorf("rate limiter stopped")
				return
			}
			select {
			case rl.requestQueue <- req:
				// Re-queued successfully
			case <-ctx.Done():
				req.ResultChan <- ctx.Err()
			case <-rl.ctx.Done():
				req.ResultChan <- fmt.Errorf("rate limiter stopped")
			default:
				// Queue full, give up
				req.ResultChan <- fmt.Errorf("retry failed: queue full")
			}
		}(time.Duration(req.Retries) * time.Second)
		return
	}
	// Send result back
	req.ResultChan <- err
}

// executeRequest executes a single request with appropriate rate limiting
func (rl *RateLimiter) executeRequest(req *RateLimitedRequest) error {
	rl.incrementTotal()

	ctx, cancel := rl.executionContext(req)
	defer cancel()

	// Wait for general message rate limit (all requests)
	if err := rl.messageRate.WaitForTokens(ctx, 1); err != nil {
		rl.incrementThrottled()
		if !isContextDone(err) {
			rl.recordRateLimitError()
		}
		return fmt.Errorf("rate limit cancelled: %w", err)
	}

	// Apply type-specific limits
	switch req.Type {
	case RequestTypeHistorical:
		// Wait for historical rate limit
		if err := rl.historicalRate.WaitForTokens(ctx, 1); err != nil {
			rl.incrementThrottled()
			if !isContextDone(err) {
				rl.recordRateLimitError()
			}
			return fmt.Errorf("historical rate limit: %w", err)
		}

		// Acquire concurrent slot
		if err := rl.historicalConcurrent.Acquire(ctx); err != nil {
			rl.incrementThrottled()
			if !isContextDone(err) {
				rl.recordRateLimitError()
			}
			return fmt.Errorf("historical concurrent limit: %w", err)
		}
		defer rl.historicalConcurrent.Release()

	case RequestTypeMarketData:
		// Check market data subscription limit
		if rl.marketDataSubs.Count() >= 100 {
			rl.incrementThrottled()
			rl.recordRateLimitError()
			return fmt.Errorf("market data subscription limit reached (100)")
		}
		// Note: Caller must manage subscription lifecycle with AcquireMarketDataSlot/ReleaseMarketDataSlot
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	// Execute the actual request
	err := req.SendFunc(ctx)
	if err != nil {
		if isContextDone(err) {
			return err
		}
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "error 100") || strings.Contains(lower, "rate limit") {
			rl.recordRateLimitError()
		} else {
			rl.resetRateLimitErrors()
		}
	} else {
		rl.resetRateLimitErrors()
	}

	return err
}

func (rl *RateLimiter) executionContext(req *RateLimitedRequest) (context.Context, context.CancelFunc) {
	base := req.Context
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithCancel(base)
	stop := context.AfterFunc(rl.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

func (rl *RateLimiter) shouldRetry(req *RateLimitedRequest, err error) bool {
	if err == nil || req.Retries >= req.MaxRetries {
		return false
	}
	if isContextDone(err) || rl.ctx.Err() != nil {
		return false
	}
	if req.Context != nil && req.Context.Err() != nil {
		return false
	}
	return true
}

func isContextDone(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// AcquireMarketDataSlot acquires a market data subscription slot
func (rl *RateLimiter) AcquireMarketDataSlot(ctx context.Context) error {
	return rl.marketDataSubs.Acquire(ctx)
}

// ReleaseMarketDataSlot releases a market data subscription slot
func (rl *RateLimiter) ReleaseMarketDataSlot() {
	rl.marketDataSubs.Release()
}

// GetMetrics returns current rate limiter metrics
func (rl *RateLimiter) GetMetrics() RateLimiterMetrics {
	rl.metricsMu.RLock()
	defer rl.metricsMu.RUnlock()

	metrics := *rl.metrics
	metrics.CurrentQueueDepth = len(rl.requestQueue)
	return metrics
}

// updateMetrics periodically updates rate metrics
func (rl *RateLimiter) updateMetrics() {
	defer rl.wg.Done()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastTotal uint64
	var lastHistorical uint64

	for {
		select {
		case <-rl.ctx.Done():
			return
		case <-ticker.C:
			rl.metricsMu.Lock()

			// Calculate rates
			currentTotal := rl.metrics.TotalRequests
			rl.metrics.MessageRatePerSec = float64(currentTotal - lastTotal)
			lastTotal = currentTotal

			// Historical rate (per minute)
			if time.Now().Unix()%60 == 0 {
				rl.metrics.HistoricalRatePerMin = float64(rl.metrics.TotalRequests - lastHistorical)
				lastHistorical = rl.metrics.TotalRequests
			}

			rl.metricsMu.Unlock()
		}
	}
}

// Helper methods for metrics
func (rl *RateLimiter) incrementTotal() {
	rl.metricsMu.Lock()
	rl.metrics.TotalRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) incrementThrottled() {
	rl.metricsMu.Lock()
	rl.metrics.ThrottledRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) incrementRejected() {
	rl.metricsMu.Lock()
	rl.metrics.RejectedRequests++
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) recordRateLimitError() {
	now := time.Now()
	rl.metricsMu.Lock()
	rl.metrics.LastRateLimitError = now
	rl.metrics.ConsecutiveErrors++
	count := rl.metrics.ConsecutiveErrors
	rl.metricsMu.Unlock()

	rateLimiterLogger.Warnf("IBKR rate limit error detected (consecutive: %d)", count)

	if rl.circuitThreshold > 0 && count >= rl.circuitThreshold {
		rl.openCircuit(now.Add(rl.circuitCooldown))
	}
}

func (rl *RateLimiter) resetRateLimitErrors() {
	rl.metricsMu.Lock()
	rl.metrics.ConsecutiveErrors = 0
	rl.metricsMu.Unlock()
}

func (rl *RateLimiter) openCircuit(openUntil time.Time) {
	rl.circuitMu.Lock()
	if rl.circuitOpenUntil.Before(openUntil) {
		rl.circuitOpenUntil = openUntil
		rateLimiterLogger.Warnf("Circuit breaker open until %s", openUntil.Format(time.RFC3339))
	}
	rl.circuitMu.Unlock()
}

func (rl *RateLimiter) checkCircuit(reqType RequestType) error {
	rl.circuitMu.Lock()
	defer rl.circuitMu.Unlock()

	if rl.circuitOpenUntil.IsZero() {
		return nil
	}

	now := time.Now()
	if now.After(rl.circuitOpenUntil) {
		rl.circuitOpenUntil = time.Time{}
		return nil
	}

	if reqType == RequestTypeHeartbeat {
		return nil
	}

	return fmt.Errorf("rate limiter circuit breaker open until %s", rl.circuitOpenUntil.Format(time.RFC3339))
}
