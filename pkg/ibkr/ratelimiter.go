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
type RateLimiter struct {
	// Token buckets for different rate limits
	messageRate    *TokenBucket // General messages: 40/sec (safe from 50/sec max)
	historicalRate *TokenBucket // Historical data: 60 requests per 10 minutes

	// Semaphores for concurrent limits
	historicalConcurrent *Semaphore // Max 50 concurrent historical requests
	marketDataSubs       *Semaphore // Max 100 concurrent market data subscriptions (retail)

	// Background-lane admission pools (see backgroundMessageSlots). One per
	// token bucket a background request can pre-book.
	backgroundMessage    *Semaphore
	backgroundHistorical *Semaphore

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

// RateLimitedRequest wraps a scheduler-owned request with rate-limit metadata.
// It remains exported for v2 source compatibility.
type RateLimitedRequest struct {
	Type       RequestType
	Priority   RequestPriority
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

// RequestPriority selects the pacing lane for a submitted request. The
type RequestPriority int

const (
	// PriorityInteractive requests reserve pacing tokens immediately, in
	PriorityInteractive RequestPriority = iota
	// PriorityBackground requests must hold one of a small pool of
	// in-flight slots before reserving pacing tokens. The pool bounds how
	// many token reservations a fan-out can book ahead of an interactive
	// not the whole fan-out. Token reservations stay FIFO across lanes and
	PriorityBackground
)

// Background-lane pool sizes. A background request's token debt is capped
// background work alone (slots turn over as fast as tokens refill).
const (
	backgroundMessageSlots    = 8
	backgroundHistoricalSlots = 2
)

type requestPriorityContextKey struct{}

// WithRequestPriority returns a context whose connector requests submit on
func WithRequestPriority(ctx context.Context, p RequestPriority) context.Context {
	return context.WithValue(ctx, requestPriorityContextKey{}, p)
}

func requestPriorityFrom(ctx context.Context) RequestPriority {
	if ctx == nil {
		return PriorityInteractive
	}
	if p, ok := ctx.Value(requestPriorityContextKey{}).(RequestPriority); ok {
		return p
	}
	return PriorityInteractive
}

// effectivePriority reads the context's pacing lane. Broker writes and
// heartbeats never queue behind the background pool, whatever their
func effectivePriority(ctx context.Context, reqType RequestType) RequestPriority {
	switch reqType {
	case RequestTypeOrder, RequestTypeHeartbeat:
		return PriorityInteractive
	}
	return requestPriorityFrom(ctx)
}

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

		// Background pacing lane
		backgroundMessage:    NewSemaphore(backgroundMessageSlots),
		backgroundHistorical: NewSemaphore(backgroundHistoricalSlots),

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
func (rl *RateLimiter) Stop() {
	rl.cancel()
	rl.wg.Wait()
}

// Submit submits a request for rate-limited execution with the default
// retry count (3). For one-shot requests where any failure should bubble
func (rl *RateLimiter) Submit(reqType RequestType, sendFunc func() error) error {
	return rl.SubmitWithRetries(reqType, sendFunc, 3)
}

// SubmitContext submits a request for rate-limited execution and cancels the
// queue/token wait when ctx is done. The send function should also check ctx
func (rl *RateLimiter) SubmitContext(ctx context.Context, reqType RequestType, sendFunc func() error) error {
	return rl.SubmitWithRetriesContext(ctx, reqType, sendFunc, 3)
}

// submitTimeout caps how long Submit waits for a queued request to
// must accommodate the slow historicalRate bucket (60 tokens, refills at
// 0.1/sec — IBKR's 60-per-10-min pacing window). When a fan-out empties
func submitTimeout(reqType RequestType) time.Duration {
	switch reqType {
	case RequestTypeHistorical:
		return 12 * time.Minute
	default:
		return 30 * time.Second
	}
}

// SubmitWithRetries submits a request with a custom retry count. Requests
// dispatch in arrival order; the only scheduling distinction is the
// many token reservations PriorityBackground work may hold at once. A
// "queue jump" parameter existed before v0.16.0 but was never wired and
func (rl *RateLimiter) SubmitWithRetries(reqType RequestType, sendFunc func() error, maxRetries int) error {
	return rl.SubmitWithRetriesContext(context.Background(), reqType, sendFunc, maxRetries)
}

// SubmitWithRetriesContext is SubmitWithRetries plus caller-owned
// request with a 60 s budget must leave that queue promptly when its caller is
func (rl *RateLimiter) SubmitWithRetriesContext(ctx context.Context, reqType RequestType, sendFunc func() error, maxRetries int) error {
	return rl.SubmitWithRetriesContextFunc(ctx, reqType, func(context.Context) error {
		return sendFunc()
	}, maxRetries)
}

// SubmitWithRetriesContextFunc passes the limiter-owned request context to the
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
		Priority:   effectivePriority(ctx, reqType),
		Context:    requestCtx,
		SendFunc:   sendFunc,
		ResultChan: make(chan error, 1),
		Timestamp:  time.Now(),
		MaxRetries: maxRetries,
	}

	// Try to queue the request. Check ctx first so a shutdown in progress
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
// stuck in WaitForTokens (e.g. a historical-data request waiting on the
// behind historical waits and failed their 5 s caller timeouts —
// themselves: messageRate (40 tokens/sec) for every type, historicalRate
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
func (rl *RateLimiter) dispatch(req *RateLimitedRequest) {
	defer rl.wg.Done()

	err := rl.executeRequest(req)
	if err != nil && rl.shouldRetry(req, err) {
		req.Retries++
		// Re-queue with exponential backoff. Honor rl.ctx so a
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

	// Background-lane admission: hold a pool slot before booking any
	// tokens, so a fan-out's pending reservations can never queue more
	if req.Priority == PriorityBackground {
		pool := rl.backgroundMessage
		if req.Type == RequestTypeHistorical {
			pool = rl.backgroundHistorical
		}
		if err := pool.Acquire(ctx); err != nil {
			rl.incrementThrottled()
			return fmt.Errorf("background pacing lane: %w", err)
		}
		defer pool.Release()
	}

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
