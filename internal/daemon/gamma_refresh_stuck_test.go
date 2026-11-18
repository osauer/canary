package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A refresh that never returns must not hold the refresh slot for the rest of
// the session. 2026-11-18 (observed): no gamma log line and no recompute from
// the 16:13 CET result until the 18:16 restart, with the soft TTL due since
// about 16:28. (Inferred, not observed: that a refresh started then and never
// finished; the blocking call is not located and the evidence went with the
// restart.) The abandoned job is cancelled, counted as a failure (so the retry
// gate paces the next attempt, never a respawn storm), logs how far it got,
// and the next attempt runs.
func TestGammaStuckRefreshIsAbandonedAndRetried(t *testing.T) {
	r := newGammaRolloverRun(t, "2026-09-23 16:05", "2026-09-24 07:00")
	logs := &gammaCaptureLog{}
	r.cache.log = logs

	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	var calls atomic.Int32
	var firstCtx atomic.Pointer[context.Context]
	compute := func(ctx context.Context, p *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		if calls.Add(1) == 1 {
			firstCtx.Store(&ctx)
			<-hang // ignores ctx: models a wait the context cannot reach
			return nil, errors.New("late return of the abandoned compute")
		}
		return r.compute.fn(ctx, p)
	}
	kick := func(at time.Time) {
		t.Helper()
		r.cache.kickOrJoin(t.Context(), rpc.GammaZeroScopeCombined, at, 1, compute)
	}

	open := gammaRolloverNY(t, "2026-09-24 09:31")
	kick(open)
	stuck := r.refreshJob()
	if stuck == nil {
		t.Fatal("session open did not start a refresh")
	}
	for deadline := time.Now().Add(5 * time.Second); firstCtx.Load() == nil; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the refresh compute never started")
		}
	}

	kick(open.Add(gammaComputeBudget - time.Minute))
	if r.refreshJob() != stuck {
		t.Fatal("a refresh inside its time budget was abandoned")
	}

	over := open.Add(gammaComputeBudget + time.Minute)
	kick(over)
	if r.refreshJob() == stuck {
		t.Fatalf("a refresh stuck for more than %s still holds the refresh slot", gammaComputeBudget)
	}
	if got := logs.joined(); !strings.Contains(got, "last progress 0%") || !strings.Contains(got, "SPY phase") {
		t.Fatalf("abandon warning does not say how far the job got: %q", got)
	}
	if ctx := firstCtx.Load(); ctx == nil || (*ctx).Err() == nil {
		t.Fatal("the abandoned refresh was not cancelled")
	}
	kick(over.Add(10 * time.Second))
	if calls.Load() != 1 {
		t.Fatalf("abandoned refresh respawned inside the retry backoff (compute calls = %d)", calls.Load())
	}

	kick(over.Add(2 * time.Minute))
	retry := r.refreshJob()
	if retry == nil || retry == stuck {
		t.Fatal("no new refresh after the retry backoff following the abandoned one")
	}
	r.compute.awaitCalls(t, 1)

	publishedAt := gammaRolloverNY(t, "2026-09-24 10:50")
	r.compute.finish(t, gammaRolloverResult(publishedAt), nil)
	r.awaitPublication(t, retry)
}

type gammaCaptureLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *gammaCaptureLog) Infof(f string, a ...any) { l.add(f, a) }
func (l *gammaCaptureLog) Warnf(f string, a ...any) { l.add(f, a) }
func (l *gammaCaptureLog) add(f string, a []any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}
func (l *gammaCaptureLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
