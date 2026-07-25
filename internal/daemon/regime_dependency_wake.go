package daemon

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

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
// repeated signals while Canary is already evaluating.
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
