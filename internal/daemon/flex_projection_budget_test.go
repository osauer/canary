package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The statement check's local re-projection is local work, never an HTTP
// round trip: on 2026-11-24 05:33 UTC it ran out of the 30-second HTTP
// timeout it borrowed, failed as projection_failed with no line in the log
// (the daemon logs at WARN, the failure at INFO) and waited the download's
// 30 minutes to try again. The projection now has the whole check's budget;
// a failure is a warning naming its class and time, and a local failure is
// retried on the check loop's own cadence.
func TestStatementProjectionHasALocalBudgetAndAFailureIsSeen(t *testing.T) {
	now := berlinTestTime(t, 2026, 11, 24, 6, 33)
	s, _ := newFlexScheduleTestServer(t, &now)
	var logs bytes.Buffer
	s.logger = NewLogger(&logs, "warn")
	s.flexFetchOnceFn = func(context.Context, time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{Path: "flex-test.xml", CoverageTo: utcDate(2026, 11, 23)}, nil
	}
	var budget time.Duration
	projection := func(ctx context.Context) error {
		if deadline, ok := ctx.Deadline(); ok {
			budget = time.Until(deadline)
		}
		return context.DeadlineExceeded
	}
	s.flexProjectionFn = func(ctx context.Context) error { return projection(ctx) }
	if !s.startFlexFetch(t.Context(), false) {
		t.Fatal("setup: the statement check did not start")
	}
	s.flexFetch.wg.Wait()
	if budget <= flexHTTPTimeout {
		t.Fatalf("the projection ran under %s, no more than the HTTP timeout %s", budget, flexHTTPTimeout)
	}
	state := s.flexFetch.state
	if state.LastReason != rpc.ReconReportReasonProjectionFailed || !state.LastRetryable {
		t.Fatalf("state = %+v, want a retryable projection failure", state)
	}
	if next := state.NextAttempt.Sub(now); next != flexCheckInterval {
		t.Fatalf("a local projection failure retries in %s, want the check loop's %s", next, flexCheckInterval)
	}
	line := logs.String()
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "Flex report check failed: projection_failed") || !strings.Contains(line, "ran out of time") {
		t.Fatalf("the failure left no warning with its detail: %q", line)
	}

	// The local retry is due on the check loop's cadence, not the download's.
	now = now.Add(flexCheckInterval)
	if got := s.flexFetchStatusAt(now); got.State != rpc.ReconReportStateDue {
		t.Fatalf("status after the local retry interval = %+v, want due", got)
	}
}

// The FX backfill spaces its retries while one failure lasts and warns when
// it starts, when its reason changes and when it recovers, never on every
// attempt: a Flex outage on 2026-11-24 wrote 870 identical warnings a
// minute apart.
func TestFXBackfillBacksOffAndWarnsOnChange(t *testing.T) {
	var b fxBackoff
	var delays []time.Duration
	warned := 0
	for range 8 {
		d, changed := b.fail("backfill_network_unavailable")
		delays = append(delays, d)
		if changed {
			warned++
		}
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, flexRetryAfterFail, flexRetryAfterFail, flexRetryAfterFail}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("retry delays %v, want %v", delays, want)
		}
	}
	if warned != 1 {
		t.Fatalf("one outage warned %d times, want once", warned)
	}
	if d, changed := b.fail("backfill_response_invalid"); !changed || d != time.Minute {
		t.Fatalf("a new reason: delay %s changed %v, want a minute and a warning", d, changed)
	}
	if n, reason := b.succeed(); n != 1 || reason != "backfill_response_invalid" {
		t.Fatalf("recovery = %d %q", n, reason)
	}
	if d, changed := b.fail("backfill_network_unavailable"); !changed || d != time.Minute {
		t.Fatalf("after recovery: delay %s changed %v, want a fresh start", d, changed)
	}
}
