package daemon

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// startRulebookCanonicalRefreshLoop owns the complete Rulebook workload. Its
// lifecycle is independent of alert observation, the Canary app, and decision
// retention so removing any adapter cannot silently stop evaluation.
func (s *Server) startRulebookCanonicalRefreshLoop(ctx context.Context) {
	if s == nil || ctx == nil {
		return
	}
	s.startRulebookCanonicalRefreshLoopWith(ctx, s.runRulebookCanonicalRefreshLoop)
}

type rulebookRefreshLoop func(context.Context)

func (s *Server) startRulebookCanonicalRefreshLoopWith(ctx context.Context, run rulebookRefreshLoop) {
	if s == nil || ctx == nil || run == nil {
		return
	}
	s.rulebookRefreshLoopWG.Go(func() {
		run(ctx)
	})
}

// runRulebookCanonicalRefreshLoop owns the same one-minute complete Rulebook
// workload previously dependent on an open Canary app. App/CLI reads reuse the
// cache, and the nonblocking evaluator yields whenever an interactive read is
// already active.
func (s *Server) runRulebookCanonicalRefreshLoop(ctx context.Context) {
	wake := s.rulebookRefreshWakeChannel()
	for {
		now := s.orderNow().UTC()
		due := s.rulebookNextRefreshDue(s.currentRulebookBinding(), now)
		wait := max(due.Sub(now), 0)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-wake:
			timer.Stop()
			continue
		case <-timer.C:
		}
		if !s.refreshRulebookCanonicalCache(ctx) {
			retry := time.NewTimer(rulebookRefreshRetryEvery)
			select {
			case <-ctx.Done():
				retry.Stop()
				return
			case <-wake:
				retry.Stop()
			case <-retry.C:
			}
		}
	}
}

func (s *Server) refreshRulebookCanonicalCache(ctx context.Context) bool {
	return s.refreshRulebookCanonicalCacheWith(ctx, s.evaluateRulesModeLocked)
}

type canonicalRulebookEvaluateLocked func(context.Context, bool, bool) *rpc.RulesResult

// refreshRulebookCanonicalCacheWith keeps evaluation and publication inside
// one single-flight critical section. The injected seam is already-locked by
// contract; tests can count/hold it without recreating the unlock/publish gap.
func (s *Server) refreshRulebookCanonicalCacheWith(ctx context.Context, evaluate canonicalRulebookEvaluateLocked) bool {
	if s == nil || ctx == nil || ctx.Err() != nil {
		return false
	}
	binding := s.currentRulebookBinding()
	now := s.orderNow().UTC()
	if s.rulebookNextRefreshDue(binding, now).After(now) {
		return true
	}
	if !s.rulesEvaluationMu.TryLock() {
		return false
	}
	defer s.rulesEvaluationMu.Unlock()
	// An interactive caller may have published while this refresh was waiting
	// to run. Recheck the last-success anchor under the single-flight.
	now = s.orderNow().UTC()
	if s.rulebookNextRefreshDue(binding, now).After(now) {
		return true
	}
	result := evaluate(ctx, true, true)
	if result == nil || ctx.Err() != nil || !sameRulebookBinding(binding, s.currentRulebookBinding()) {
		return false
	}
	// Publish/cache before releasing the single-flight so an interactive waiter
	// cannot issue a back-to-back duplicate evaluation.
	return s.publishCanonicalRulebookResult(ctx, result, binding)
}
