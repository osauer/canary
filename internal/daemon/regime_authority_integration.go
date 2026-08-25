package daemon

import (
	"context"
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"maps"
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
	s.edgeWorkerWG.Wait()
}

// regimeRefreshWakeChannel returns the daemon-lifetime, capacity-one wake
// channel used by successful direct-dependency publications.
func (s *Server) regimeRefreshWakeChannel() <-chan struct{} {
	return s.regimeRefreshWakeSender()
}

func (s *Server) regimeRefreshWakeSender() chan struct{} {
	if s == nil {
		return nil
	}
	s.regimeConsumerWakeMu.Lock()
	if s.regimeRefreshWake == nil {
		s.regimeRefreshWake = make(chan struct{}, 1)
	}
	wake := s.regimeRefreshWake
	s.regimeConsumerWakeMu.Unlock()
	return wake
}

// handleGammaPublication links the canonical combined gamma publication to
// Regime without making Gamma own Regime scheduling. Failed, canceled, and
// superseded jobs never call this hook; diagnostic single-index publications
// cannot invalidate the combined Regime input.
func (s *Server) handleGammaPublication(scope string) {
	if s == nil || scope != rpc.GammaZeroScopeCombined || s.regimeSnapshots == nil ||
		!s.regimeSnapshots.invalidateForDependencyPublication() {
		return
	}
	wake := s.regimeRefreshWakeSender()
	select {
	case wake <- struct{}{}:
	default:
	}
}

// stressEvaluationWakeChannel returns the daemon-lifetime, capacity-one wake
// channel. A buffered wake survives startup ordering and naturally coalesces
// repeated signals while Stress is already evaluating.
func (s *Server) stressEvaluationWakeChannel() <-chan struct{} {
	return s.stressEvaluationWakeSender()
}

func (s *Server) stressEvaluationWakeSender() chan struct{} {
	if s == nil {
		return nil
	}
	s.regimeConsumerWakeMu.Lock()
	if s.stressEvaluationWake == nil {
		s.stressEvaluationWake = make(chan struct{}, 1)
	}
	wake := s.stressEvaluationWake
	s.regimeConsumerWakeMu.Unlock()
	return wake
}

func (s *Server) wakeStressEvaluation() {
	wake := s.stressEvaluationWakeSender()
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

// publishRulesRegimeStageState makes the new Regime stage and Rulebook cache
// boundary atomic with respect to a complete Rulebook evaluation. An
// evaluation already in flight finishes first and is then invalidated; one
// starting later sees the new stage. Interactive reads therefore cannot serve
// an older cached Rulebook result after the Regime stage becomes visible.
func (s *Server) publishRulesRegimeStageState(state rulesRegimeStageState, publication regimeSnapshotPublication) {
	if s == nil {
		return
	}
	s.rulesEvaluationMu.Lock()
	s.rulesRegimeStageMu.Lock()
	s.rulesRegimeStage = state
	s.rulesRegimeStageLoaded = true
	s.rulesRegimeStageMu.Unlock()

	notify := s.claimRegimeConsumerPublication(publication)
	var rulebookWake chan struct{}
	if notify {
		s.rulesMu.Lock()
		// Retain the prior result as the transition-journal baseline, but make it
		// ineligible for every read and immediately due for replacement.
		s.lastRulesAt = time.Time{}
		if s.rulesRefreshWake == nil {
			s.rulesRefreshWake = make(chan struct{}, 1)
		}
		rulebookWake = s.rulesRefreshWake
		s.rulesMu.Unlock()
	}
	s.rulesEvaluationMu.Unlock()

	if !notify {
		return
	}
	select {
	case rulebookWake <- struct{}{}:
	default:
	}
	s.wakeStressEvaluation()
}

// claimRegimeConsumerPublication admits each monotonic publication revision at
// most once. Consumers always read the latest immutable snapshot, so a burst
// of newer publications may share one buffered wake without losing state.
func (s *Server) claimRegimeConsumerPublication(publication regimeSnapshotPublication) bool {
	if s == nil || publication.Revision <= 0 {
		return false
	}
	s.regimeConsumerWakeMu.Lock()
	defer s.regimeConsumerWakeMu.Unlock()
	if publication.Revision <= s.regimeConsumerRevision {
		return false
	}
	s.regimeConsumerRevision = publication.Revision
	return true
}

// startRegimeRefreshLoop starts the daemon-owned Regime freshness scheduler.
// It waits for gateway readiness without consuming refresh backoff and is
// deliberately independent of the alert registry, Stress journaling, and app
// polling.
func (s *Server) startRegimeRefreshLoop(ctx context.Context) {
	if s == nil || ctx == nil || ctx.Err() != nil || s.regimeSnapshots == nil {
		return
	}
	s.regimeRefreshLoopWG.Go(func() {
		runRegimeRefreshLoop(
			ctx,
			s.regimeSnapshots,
			regimeSnapshotRefreshPoll,
			regimeSnapshotRefreshAhead,
			s.regimeRefreshWakeChannel(),
			s.regimeRefreshGatewayReady,
			s.acquireRegimeSnapshot,
		)
	})
}

// regimeRefreshGatewayReady is a non-triggering readiness read. The scheduler
// must not race cold-start discovery or create a reconnect loop of its own;
// normal connection ownership will make the next poll eligible.
func (s *Server) regimeRefreshGatewayReady() bool {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	return c != nil && c.IsReady()
}

func runRegimeRefreshLoop(
	ctx context.Context,
	cache *regimeSnapshotCache,
	pollEvery time.Duration,
	refreshAhead time.Duration,
	wake <-chan struct{},
	ready func() bool,
	refresh regimeSnapshotRefreshFunc,
) {
	if ctx == nil || cache == nil || ready == nil || refresh == nil || pollEvery <= 0 || refreshAhead <= 0 {
		return
	}
	kick := func() {
		if ready() {
			cache.startRefreshAhead(refresh, refreshAhead)
		}
	}
	kick()

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			kick()
		case <-ticker.C:
			kick()
		}
	}
}

// cloneForRegimeEvaluation snapshots the current streak authority into a
// write-isolated in-memory evaluator. The caller may run the normal Tick and
// Latch methods against the clone; no SQLite write occurs until commit below.
func (s *StreakStore) cloneForRegimeEvaluation() *StreakStore {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	return &StreakStore{
		entries:  cloneStreakEntries(s.entries),
		loaded:   true,
		volatile: true,
		asOf:     s.asOf,
	}
}

// commitRegimeEvaluation publishes one already-classified evaluator after the
// enclosing regime snapshot has committed as last-good. Regime refresh is
// single-flight, so this is the only production writer in the evaluation
// interval. Persistence is part of the projection barrier: failure withholds
// the committed snapshot until exact recovery succeeds.
func (s *StreakStore) commitRegimeEvaluation(ctx context.Context, evaluated *StreakStore, plan regimeProjectionPlan) error {
	if s == nil || evaluated == nil {
		return nil
	}
	publication := plan.publication
	evaluated.mu.Lock()
	entries := cloneStreakEntries(evaluated.entries)
	evaluated.mu.Unlock()

	s.mu.Lock()
	s.loadLocked()
	position, err := s.regimeProjectionPosition(plan)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if position == regimeProjectionCurrent {
		if !maps.Equal(s.entries, entries) {
			s.mu.Unlock()
			return fmt.Errorf("regime streak projection content mismatch at snapshot revision %d", publication.Revision)
		}
		s.mu.Unlock()
		return nil
	}
	beforeEntries := cloneStreakEntries(s.entries)
	beforeAsOf := s.asOf
	beforePublication := s.publication
	beforeExists := s.stateExists
	s.entries = entries
	s.loaded = true
	err = s.saveLockedContextPublication(ctx, publication)
	if err != nil {
		s.entries = beforeEntries
		s.asOf = beforeAsOf
		s.publication = beforePublication
		s.stateExists = beforeExists
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("commit regime streak projection: %w", err)
	}
	return nil
}

func (s *StreakStore) commitLegacyRegimeEvaluation(ctx context.Context, evaluated *StreakStore, publishedAt time.Time) error {
	if s == nil || evaluated == nil {
		return nil
	}
	evaluated.mu.Lock()
	entries := cloneStreakEntries(evaluated.entries)
	evaluated.mu.Unlock()
	s.mu.Lock()
	beforeEntries := cloneStreakEntries(s.entries)
	beforeAsOf := s.asOf
	s.entries = entries
	s.loaded = true
	err := s.saveLockedContextAt(ctx, publishedAt)
	if err != nil {
		s.entries = beforeEntries
		s.asOf = beforeAsOf
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("commit legacy regime streak projection: %w", err)
	}
	return nil
}

func cloneStreakEntries(in map[string]StreakEntry) map[string]StreakEntry {
	out := make(map[string]StreakEntry, len(in))
	maps.Copy(out, in)
	return out
}
