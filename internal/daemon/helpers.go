package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// pollCadence is the shared 75 ms cadence at which short-lived snapshot
// polls re-read the IBKR market-data / option cache. Previously inlined
const pollCadence = 75 * time.Millisecond

// SubscriptionRejectedError is returned by pollMarketData (and helpers
// that thread the subscription's reject channel) when the IBKR gateway
type SubscriptionRejectedError struct {
	Key       string
	Rejection ibkrlib.SubscriptionRejection
}

// Error formats the subscription key and broker rejection details.
func (e *SubscriptionRejectedError) Error() string {
	return fmt.Sprintf("subscription %q rejected by gateway: code=%d msg=%s", e.Key, e.Rejection.Code, e.Rejection.Message)
}

// IsSubscriptionRejected reports whether err is a SubscriptionRejectedError.
func IsSubscriptionRejected(err error) bool {
	var rej *SubscriptionRejectedError
	return errors.As(err, &rej)
}

// pollUntil drives a polling loop on the standard cadence until fn signals
// The IBKR Subscribe/Unsubscribe call is the caller's responsibility — this
// helper only owns the loop. Use ptrIfPos to lift the scalar fields the
func pollUntil(ctx context.Context, deadline time.Time, fn func() (done bool)) error {
	return pollUntilWithReject(ctx, deadline, nil, "", fn)
}

// pollUntilWithReject is pollUntil that also selects on a subscription
// error frame arriving on the wire, instead of running out the deadline.
// key is only used to label the returned error so fan-out callers can
func pollUntilWithReject(ctx context.Context, deadline time.Time, rejectCh <-chan ibkrlib.SubscriptionRejection, key string, fn func() (done bool)) error {
	if fn() {
		return nil
	}
	poll := time.NewTicker(pollCadence)
	defer poll.Stop()
	for {
		if time.Now().After(deadline) {
			return context.DeadlineExceeded
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case rej := <-rejectCh:
			return &SubscriptionRejectedError{Key: key, Rejection: rej}
		case <-poll.C:
		}
		if fn() {
			return nil
		}
	}
}

// pollMarketData is the common case of pollUntil: poll
// only when the cache has an entry for key.
// ([ibkrlib.Connector.SubscriptionRejectCh]) so a terminal gateway error
func pollMarketData(ctx context.Context, c *ibkrlib.Connector, key string, deadline time.Time, predicate func(*ibkrlib.MarketData) bool) error {
	return pollUntilWithReject(ctx, deadline, c.SubscriptionRejectCh(key), key, func() bool {
		data, ok := c.MarketDataSnapshot()[key]
		if !ok {
			return false
		}
		return predicate(data)
	})
}

// ptrIfPos returns &v when v > 0 (using ordered comparison) and nil
func ptrIfPos[T int | int64 | float64](v T) *T {
	if v > 0 {
		x := v
		return &x
	}
	return nil
}

// normCcy normalises a currency code: uppercase, trimmed. Centralises the
func normCcy(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// normSym is normCcy aliased for symbol normalisation — same rule, but the
func normSym(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// runBounded runs fn(jobs[i]) concurrently with at most workers in flight.
// work; this helper only bounds parallelism.
func runBounded[T any](jobs []T, workers int, fn func(T)) {
	if len(jobs) == 0 {
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	ch := make(chan T, len(jobs))
	for _, j := range jobs {
		ch <- j
	}
	close(ch)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for j := range ch {
				fn(j)
			}
		})
	}
	wg.Wait()
}
