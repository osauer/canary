package daemon

import (
	"context"
	"fmt"
	"time"
)

const (
	// Regime's operational last-good window follows the existing five-minute
	// evidence cadence. This is cache scheduling, not a market threshold: row
	// cadence and confirmation eligibility remain owned by the typed Regime
	// policy already carried in each snapshot.
	regimeSnapshotFreshFor       = stressJournalEvery
	regimeSnapshotRefreshTimeout = 45 * time.Second
	// Start a normal refresh one full timeout before the hard freshness
	// ceiling, with a small scheduler cushion. The hard five-minute limit is
	// unchanged: if this work cannot finish, consumers still see stale state.
	regimeSnapshotRefreshAhead = regimeSnapshotRefreshTimeout + 15*time.Second
	regimeSnapshotRefreshPoll  = 5 * time.Second
	// A failed early refresh should not suppress recovery for another complete
	// five-minute window. The single-flight and 45-second timeout still bound
	// pressure while a source is unhealthy.
	regimeSnapshotFailureRetry = 30 * time.Second
)

// attachRegimeSnapshotAuthority strictly hydrates the one daemon.db document
// after the daemon-lifetime context exists and before the RPC socket is
// published. Missing state is a valid cold start; malformed state fails
// startup instead of silently falling back to a file or history projection.
func (s *Server) attachRegimeSnapshotAuthority(startupContext, daemonContext context.Context) error {
	if s == nil || s.coreStore == nil {
		return fmt.Errorf("regime snapshot SQLite authority is unavailable")
	}
	cache, err := loadRegimeSnapshotCache(startupContext, daemonContext, s.coreStore, regimeSnapshotCacheOptions{
		FreshFor:          regimeSnapshotFreshFor,
		RefreshTimeout:    regimeSnapshotRefreshTimeout,
		FailureRetryAfter: regimeSnapshotFailureRetry,
	})
	if err != nil {
		return err
	}
	if err := s.reconcileRegimeSnapshotProjections(startupContext, cache); err != nil {
		return fmt.Errorf("reconcile regime snapshot projections: %w", err)
	}
	s.regimeSnapshots = cache
	return nil
}

// stopServerContextAndWait is the shutdown barrier for daemon-owned Regime
// work. It is idempotent so both Start's deferred cleanup and Stop may call it.
// The refresh itself is already bounded by regimeSnapshotRefreshTimeout; once
// cancellation is observed, wait cannot admit a replacement refresh.
func (s *Server) stopServerContextAndWait() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel := s.serverCancel
	s.serverCancel = nil
	cache := s.regimeSnapshots
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.regimeRefreshLoopWG.Wait()
	s.rulebookRefreshLoopWG.Wait()
	s.alertShadowLoopWG.Wait()
	s.stressEvaluationLoopWG.Wait()
	s.stopDataHealthAlertShadowWorker()
	if cache != nil {
		cache.wait()
	}
	s.flexFetch.stopAndWait()
}
