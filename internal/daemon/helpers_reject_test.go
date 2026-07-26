package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestPollUntilWithReject_FastAbortOnRejection asserts that the
// daemon-side poll helper returns a [SubscriptionRejectedError] within
// fastAbortBudget when a rejection arrives on the channel, instead of
// running out the budget. This is the consumer-side counterpart to the
// pkg/ibkr tests that verify the connector's handleIBKRError pushes
// rejections promptly.
func TestPollUntilWithReject_FastAbortOnRejection(t *testing.T) {
	const fastAbortBudget = 100 * time.Millisecond
	const pollBudget = 5 * time.Second

	rejectCh := make(chan ibkrlib.SubscriptionRejection, 1)

	// Push the rejection from a goroutine after a short stagger so the
	// poller is definitely already in its select. (A pre-queued message
	// would also pass, but staggering exercises the "arrive during
	// select" path that productionLegFetcher actually hits.)
	go func() {
		time.Sleep(5 * time.Millisecond)
		rejectCh <- ibkrlib.SubscriptionRejection{
			Code:    200,
			Message: "No security definition has been found for the request",
		}
	}()

	start := time.Now()
	err := pollUntilWithReject(
		context.Background(),
		time.Now().Add(pollBudget),
		rejectCh,
		"SPY_250620C500",
		func() bool { return false }, // predicate never satisfied
	)
	elapsed := time.Since(start)

	if elapsed > fastAbortBudget {
		t.Fatalf("poll took %s; ceiling is %s — the select did not honour the rejection channel", elapsed, fastAbortBudget)
	}
	if !IsSubscriptionRejected(err) {
		t.Fatalf("expected SubscriptionRejectedError, got %T: %v", err, err)
	}
	var rej *SubscriptionRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("errors.As(&SubscriptionRejectedError) failed for %v", err)
	}
	if rej.Rejection.Code != 200 {
		t.Fatalf("expected code 200, got %d", rej.Rejection.Code)
	}
	if rej.Key != "SPY_250620C500" {
		t.Fatalf("expected key SPY_250620C500, got %q", rej.Key)
	}
}

// TestPollUntilWithReject_NilChannelFallsThroughToDeadline guards the
// backward-compat path: passing a nil rejectCh disables fast-abort.
// pollUntil and existing call sites rely on this — a nil channel in a
// select blocks forever, leaving the existing ticker/ctx/deadline
// branches as the only ways to exit.
func TestPollUntilWithReject_NilChannelFallsThroughToDeadline(t *testing.T) {
	start := time.Now()
	err := pollUntilWithReject(
		context.Background(),
		time.Now().Add(150*time.Millisecond),
		nil, // no fast-abort
		"",
		func() bool { return false },
	)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned too early (%s); deadline should have been honoured", elapsed)
	}
}

// TestPollUntilWithReject_PredicateWinsOverRejection asserts that if
// the predicate is satisfiable, the poll returns nil even when a
// rejection is queued. This guards against a (silly) regression where
// the rejection branch is checked unconditionally before the predicate.
func TestPollUntilWithReject_PredicateWinsOverRejection(t *testing.T) {
	rejectCh := make(chan ibkrlib.SubscriptionRejection, 1)
	rejectCh <- ibkrlib.SubscriptionRejection{Code: 200, Message: "stale"}

	err := pollUntilWithReject(
		context.Background(),
		time.Now().Add(5*time.Second),
		rejectCh,
		"k",
		func() bool { return true }, // satisfied immediately
	)
	if err != nil {
		t.Fatalf("expected nil when predicate true on first call, got %v", err)
	}
}
