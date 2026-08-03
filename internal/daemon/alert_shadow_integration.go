package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	alertShadowGatewayStartupGrace          = 30 * time.Second
	alertShadowDataHealthPendingTransitions = 256
)

type alertShadowDataHealthPending struct {
	Input       alertShadowDataHealthInput
	Fingerprint string
}

func (s *Server) attachAlertShadowAuthority(ctx context.Context) error {
	if s == nil || s.coreStore == nil {
		return errors.New("alert registry SQLite authority is unavailable")
	}
	registry, err := newAlertEpisodeRegistry(ctx, s.coreStore)
	if err != nil {
		return fmt.Errorf("load alert episode registry: %w", err)
	}
	s.alertEpisodes = registry
	s.alertShadow = newAlertShadowComposer(registry)
	return nil
}

func (s *Server) handleAlertCandidates(ctx context.Context, req *rpc.Request) (*rpc.AlertCandidateSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Params) > 0 {
		var params rpc.AlertCandidatesParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	if s == nil || s.alertShadow == nil {
		return nil, errors.New("alert authority is unavailable")
	}
	scope, err := newAlertShadowBrokerScope(s.currentBrokerStateScope())
	if err != nil {
		return nil, errors.New("alert authority scope is unavailable")
	}
	snapshot, ok, err := s.alertShadow.Snapshot(scope)
	if err != nil {
		return nil, err
	}
	if !ok {
		now := time.Now().UTC()
		if s.now != nil {
			now = s.now().UTC()
		}
		snapshot = unavailableAlertCandidateSnapshot(now, scope.authority)
	}
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return nil, fmt.Errorf("validate alert candidate snapshot: %w", err)
	}
	return &snapshot, nil
}

func unavailableAlertCandidateSnapshot(asOf time.Time, authorityScope string) rpc.AlertCandidateSnapshot {
	asOf = asOf.UTC()
	return rpc.AlertCandidateSnapshot{
		SchemaVersion: rpc.AlertCandidateSnapshotVersion, AuthorityScope: authorityScope,
		AsOf: asOf, CurrentState: rpc.AlertSnapshotUnknown,
		Coverage: rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: asOf,
			ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: []rpc.AlertSource{},
		},
		Sources:    unavailableAlertSourceCoverage(alertShadowExpectedSourceSlice()),
		Candidates: []rpc.AlertCandidate{},
	}
}

func (s *Server) handleAlertStatus(ctx context.Context, req *rpc.Request) (*rpc.AlertStatusResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Params) > 0 {
		var params rpc.AlertStatusParams
		if err := decodeParams(req.Params, &params); err != nil {
			return nil, err
		}
	}
	if s == nil || s.alertShadow == nil {
		return nil, errors.New("alert authority is unavailable")
	}
	scope, err := newAlertShadowBrokerScope(s.currentBrokerStateScope())
	if err != nil {
		return nil, errors.New("alert authority scope is unavailable")
	}
	status := s.alertShadow.Status(scope)
	result := &rpc.AlertStatusResult{
		AsOf:            status.AsOf,
		ExpectedSources: append([]rpc.AlertSource(nil), status.ExpectedSources...),
		Evaluations:     status.Evaluations, RegistryApplyFailures: status.RegistryApplyFailures,
		Equivocations: status.Equivocations, LastErrorCode: status.LastErrorCode,
		Sources: make([]rpc.AlertSourceStatus, 0, len(status.Sources)),
	}
	for _, source := range status.Sources {
		m := source.Measurements
		result.Sources = append(result.Sources, rpc.AlertSourceStatus{
			Source: source.Source, Status: source.Status, Reason: source.Reason,
			AuthorityUniverse: source.AuthorityUniverse,
			InputAsOf:         source.InputAsOf, ObservedAt: source.ObservedAt, Covered: source.Covered,
			UncoveredRules: append([]string(nil), source.UncoveredRules...), Active: source.Active,
			Measurements: rpc.AlertMeasurements{
				Evaluations: m.Evaluations, CoveredEvaluations: m.CoveredEvaluations,
				ActiveEvaluations: m.ActiveEvaluations, ActiveObservations: m.ActiveObservations,
				EpisodesOpened: m.EpisodesOpened, EpisodesEscalated: m.EpisodesEscalated,
				EpisodesRecovered: m.EpisodesRecovered, EpisodesReopened: m.EpisodesReopened,
				DuplicateInputs: m.DuplicateInputs, DuplicateCandidates: m.DuplicateCandidates,
				RepeatedActive: m.RepeatedActive, ActiveEvidenceChurn: m.ActiveEvidenceChurn,
				Equivocations: m.Equivocations, StaleSuppressions: m.StaleSuppressions,
				CoverageFailures: m.CoverageFailures, TimeToObserveSamples: m.TimeToObserveSamples,
				TimeToObserveTotalSecond: m.TimeToObserveTotal.Seconds(),
				TimeToObserveMaxSecond:   m.TimeToObserveMax.Seconds(),
			},
		})
	}
	return result, nil
}

func (s *Server) observeStressAlertShadow(result *rpc.StressResult, brokerScope brokerStateScope) {
	if s == nil || s.alertShadow == nil || result == nil {
		return
	}
	scope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		s.warnf("alert producer: Stress observation skipped: %v", err)
		return
	}
	ctx := context.Background()
	s.mu.Lock()
	if s.serverCtx != nil {
		ctx = s.serverCtx
	}
	s.mu.Unlock()
	if _, err := s.alertShadow.ObserveStress(ctx, scope, *result); err != nil {
		s.warnf("alert producer: Stress observation failed: %v", err)
	}
}

func (s *Server) observeNudgesAlertShadow(ctx context.Context, input alertShadowNudgeInput) {
	if s == nil || s.alertShadow == nil {
		return
	}
	if _, err := s.alertShadow.ObserveNudges(ctx, input); err != nil {
		s.warnf("alert producer: Nudge observation failed: %v", err)
	}
}

func (s *Server) observeRegimeAlertShadow(ctx context.Context, result *rpc.RegimeSnapshotResult, brokerScope brokerStateScope) {
	if s == nil || s.alertShadow == nil || result == nil {
		return
	}
	scope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		s.warnf("alert producer: Regime observation skipped: %v", err)
		return
	}
	if _, err := s.alertShadow.ObserveRegime(ctx, scope, *result); err != nil {
		s.warnf("alert producer: Regime observation failed: %v", err)
	}
}

func (s *Server) observeRulebookAlertShadow(ctx context.Context, result *rpc.RulesResult, brokerScope brokerStateScope) {
	if err := s.commitRulebookAlertShadow(ctx, result, brokerScope); err != nil {
		s.warnf("alert producer: Rulebook observation failed: %v", err)
	}
}

func (s *Server) commitRulebookAlertShadow(ctx context.Context, result *rpc.RulesResult, brokerScope brokerStateScope) error {
	if s == nil || s.alertShadow == nil || result == nil {
		return nil
	}
	scope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		return err
	}
	if _, err := s.alertShadow.ObserveRulebook(ctx, scope, *result); err != nil {
		return err
	}
	return nil
}

func (s *Server) observeProtectionAlertShadow(ctx context.Context, input alertShadowProtectionInput) {
	if s == nil || s.alertShadow == nil {
		return
	}
	if err := s.commitProtectionAlertShadow(ctx, input); err != nil {
		s.warnf("alert producer: Protection observation failed: %v", err)
	}
}

func (s *Server) commitProtectionAlertShadow(ctx context.Context, input alertShadowProtectionInput) error {
	if s == nil || s.alertShadow == nil {
		return nil
	}
	_, err := s.alertShadow.ObserveProtection(ctx, input)
	return err
}

func (s *Server) observeProtectionAlertShadowStable(ctx context.Context, binding daemonBrokerEvidenceBinding, input alertShadowProtectionInput) {
	if s == nil || s.alertShadow == nil {
		return
	}
	if s.protectionBeforeCommit != nil {
		s.protectionBeforeCommit()
	}
	committed, err := s.withStableBrokerAndOrderEvidence(binding, input.orderJournal, input.orderAuthorityHeadSeq, func() error {
		return s.commitProtectionAlertShadow(ctx, input)
	})
	if err != nil {
		s.warnf("alert producer: Protection observation failed: %v", err)
		return
	}
	if !committed {
		s.debugf("alert producer: Protection broker, portfolio, or journal evidence changed before final commit")
	}
}

func (s *Server) observeOrderIntegrityAlertShadow(ctx context.Context, input orderIntegrityEvaluation) {
	if s == nil || s.alertShadow == nil {
		return
	}
	scope, err := newAlertShadowBrokerScope(input.Scope)
	if err != nil {
		s.warnf("alert producer: Order Integrity observation skipped: %v", err)
		return
	}
	if input.connector == nil {
		// Injected heartbeat seams use unbound evaluations to exercise mapper
		// policy. Production load paths force unbound evidence unavailable, so
		// this branch can update health but cannot authorize a recovery.
		if err := s.commitOrderIntegrityAlertShadow(ctx, scope, input); err != nil {
			s.warnf("alert producer: Order Integrity observation failed: %v", err)
		}
		return
	}
	if s.orderIntegrityBeforeCommit != nil {
		s.orderIntegrityBeforeCommit()
	}
	committed, err := s.withStableBrokerAndOrderEvidence(daemonBrokerEvidenceBinding{
		scope: input.Scope, connector: input.connector, connectorEpoch: input.connectorEpoch,
		broker: input.brokerEvidence,
	}, input.orderJournal, input.orderAuthorityHeadSeq, func() error {
		return s.commitOrderIntegrityAlertShadow(ctx, scope, input)
	})
	if err != nil {
		s.warnf("alert producer: Order Integrity observation failed: %v", err)
		return
	}
	if !committed {
		s.debugf("alert producer: Order Integrity broker or journal evidence changed before final commit")
	}
}

func (s *Server) commitOrderIntegrityAlertShadow(ctx context.Context, scope alertShadowBrokerScope, input orderIntegrityEvaluation) error {
	if s == nil || s.alertShadow == nil {
		return nil
	}
	_, err := s.alertShadow.ObserveOrderIntegrity(ctx, scope, input)
	return err
}

func (s *Server) observeDataHealthAlertShadow(result *rpc.HealthResult, brokerScope brokerStateScope, gatewayPhase alertShadowGatewayPhase, observedAt time.Time) {
	if s == nil || s.alertShadow == nil || result == nil {
		return
	}
	scope, err := newAlertShadowBrokerScope(brokerScope)
	if err != nil {
		s.warnf("alert producer: Data Health observation skipped: %v", err)
		return
	}
	if observedAt.IsZero() {
		observedAt = s.dataHealthObservationNow()
	}
	proposalsExpected, opportunitiesExpected := false, false
	if s.cfg != nil {
		proposalsExpected = s.cfg.AutoTrade.WithDefaults().ProposalsEnabledResolved()
		opportunitiesExpected = s.cfg.Opportunities.WithDefaults().EnabledResolved()
	}
	input := alertShadowDataHealthInput{
		AsOf: observedAt.UTC(), Health: *result, Scope: scope, GatewayPhase: gatewayPhase,
		ProposalsExpected: proposalsExpected, OpportunitiesExpected: opportunitiesExpected,
	}
	s.enqueueDataHealthAlertShadow(input)
}

func alertShadowGatewayPhaseForHealth(connected, setupComplete, connectInFlight bool, lastError string, uptime time.Duration) alertShadowGatewayPhase {
	if connected {
		return alertShadowGatewayReady
	}
	if setupComplete || strings.TrimSpace(lastError) != "" || (!connectInFlight && uptime >= alertShadowGatewayStartupGrace) {
		return alertShadowGatewayFailed
	}
	return alertShadowGatewayConnecting
}

func (s *Server) enqueueDataHealthAlertShadow(input alertShadowDataHealthInput) {
	if s == nil {
		return
	}
	fingerprint, err := alertShadowDataHealthInputFingerprint(input)
	if err != nil {
		s.warnf("alert producer: Data Health queue fingerprint failed: %v", err)
		return
	}
	pending := alertShadowDataHealthPending{Input: input, Fingerprint: fingerprint}
	ctx := context.Background()
	s.mu.Lock()
	if s.serverCtx != nil {
		ctx = s.serverCtx
	}
	s.mu.Unlock()
	s.dataHealthObserveMu.Lock()
	if s.dataHealthObserveStopped {
		s.dataHealthObserveMu.Unlock()
		return
	}
	if s.dataHealthObservePending == nil {
		s.dataHealthObservePending = make(map[string][]alertShadowDataHealthPending)
	}
	if s.dataHealthObserveRetryAt == nil {
		s.dataHealthObserveRetryAt = make(map[string]time.Time)
	}
	if s.dataHealthObserveWake == nil {
		s.dataHealthObserveWake = make(chan struct{}, 1)
	}
	authority := input.Scope.authority
	queue := s.dataHealthObservePending[authority]
	if len(queue) > 0 && queue[len(queue)-1].Fingerprint == fingerprint {
		// Repeated polling of the same semantic state only advances its
		// receipt time. A changed state is always appended, preserving an
		// observed outage before a later recovery.
		queue[len(queue)-1] = pending
	} else if len(queue) < alertShadowDataHealthPendingTransitions {
		queue = append(queue, pending)
	} else {
		// SQLite is already unable to keep up with 256 distinct transitions.
		// Retain the older ordered evidence rather than replacing it with a
		// newer clear. The 30-second heartbeat will re-observe current state
		// after the queue begins draining.
		s.dataHealthObserveMu.Unlock()
		s.warnf("alert producer: Data Health transition queue saturated; retaining older evidence")
		return
	}
	s.dataHealthObservePending[authority] = queue
	select {
	case s.dataHealthObserveWake <- struct{}{}:
	default:
	}
	if s.dataHealthObserveRunning {
		s.dataHealthObserveMu.Unlock()
		return
	}
	s.dataHealthObserveRunning = true
	s.dataHealthObserveWG.Add(1)
	s.dataHealthObserveMu.Unlock()
	go s.runDataHealthAlertShadowWorker(ctx)
}

func (s *Server) runDataHealthAlertShadowWorker(ctx context.Context) {
	defer s.dataHealthObserveWG.Done()
	for {
		pending, waitFor, wake, ok := s.nextDataHealthAlertShadowInput()
		if !ok {
			return
		}
		if waitFor > 0 {
			timer := time.NewTimer(waitFor)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				s.finishDataHealthAlertShadowWorker()
				return
			case <-wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				continue
			case <-timer.C:
				continue
			}
		}
		observeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		observeErr := s.applyDataHealthAlertShadow(observeCtx, pending.Input)
		cancel()
		finishedAt := s.dataHealthObservationNow()
		s.dataHealthObserveMu.Lock()
		if s.dataHealthObserveStopped {
			s.dataHealthObserveRunning = false
			s.dataHealthObserveMu.Unlock()
			return
		}
		if observeErr != nil {
			backoff := s.dataHealthObserveBackoff
			if backoff <= 0 {
				backoff = alertShadowHotPollRefreshInterval
			}
			authority := pending.Input.Scope.authority
			s.dataHealthObserveRetryAt[authority] = finishedAt.Add(backoff)
			queue := s.dataHealthObservePending[authority]
			if len(queue) == 0 || queue[0].Fingerprint != pending.Fingerprint {
				if len(queue) >= alertShadowDataHealthPendingTransitions {
					queue = queue[:alertShadowDataHealthPendingTransitions-1]
				}
				queue = append([]alertShadowDataHealthPending{pending}, queue...)
			}
			s.dataHealthObservePending[authority] = queue
		} else {
			delete(s.dataHealthObserveRetryAt, pending.Input.Scope.authority)
		}
		s.dataHealthObserveMu.Unlock()
		if observeErr != nil {
			s.warnf("alert producer: Data Health observation failed: %v", observeErr)
		}
	}
}

func (s *Server) nextDataHealthAlertShadowInput() (alertShadowDataHealthPending, time.Duration, <-chan struct{}, bool) {
	s.dataHealthObserveMu.Lock()
	defer s.dataHealthObserveMu.Unlock()
	if s.dataHealthObserveStopped {
		s.dataHealthObserveRunning = false
		return alertShadowDataHealthPending{}, 0, nil, false
	}
	now := s.dataHealthObservationNow()
	var selected string
	var waitUntil time.Time
	for authority, queue := range s.dataHealthObservePending {
		if len(queue) == 0 {
			delete(s.dataHealthObservePending, authority)
			continue
		}
		retryAt := s.dataHealthObserveRetryAt[authority]
		if retryAt.IsZero() || !now.Before(retryAt) {
			selected = authority
			break
		}
		if waitUntil.IsZero() || retryAt.Before(waitUntil) {
			waitUntil = retryAt
		}
	}
	if selected != "" {
		queue := s.dataHealthObservePending[selected]
		pending := queue[0]
		if len(queue) == 1 {
			delete(s.dataHealthObservePending, selected)
		} else {
			s.dataHealthObservePending[selected] = queue[1:]
		}
		return pending, 0, s.dataHealthObserveWake, true
	}
	if !waitUntil.IsZero() {
		return alertShadowDataHealthPending{}, max(waitUntil.Sub(now), time.Millisecond), s.dataHealthObserveWake, true
	}
	s.dataHealthObserveRunning = false
	return alertShadowDataHealthPending{}, 0, nil, false
}

func (s *Server) finishDataHealthAlertShadowWorker() {
	s.dataHealthObserveMu.Lock()
	s.dataHealthObserveRunning = false
	s.dataHealthObserveMu.Unlock()
}

func (s *Server) applyDataHealthAlertShadow(ctx context.Context, input alertShadowDataHealthInput) error {
	if s.dataHealthObserveTest != nil {
		return s.dataHealthObserveTest(ctx, input)
	}
	_, err := s.alertShadow.ObserveDataHealth(ctx, input)
	return err
}

func (s *Server) dataHealthObservationNow() time.Time {
	now := time.Now().UTC()
	if s != nil && s.now != nil {
		now = s.now().UTC()
	}
	return now
}

func (s *Server) stopDataHealthAlertShadowWorker() {
	if s == nil {
		return
	}
	s.dataHealthObserveMu.Lock()
	s.dataHealthObserveStopped = true
	if s.dataHealthObserveWake != nil {
		select {
		case s.dataHealthObserveWake <- struct{}{}:
		default:
		}
	}
	s.dataHealthObserveMu.Unlock()
	s.dataHealthObserveWG.Wait()
	s.dataHealthObserveMu.Lock()
	s.dataHealthObservePending = nil
	s.dataHealthObserveRetryAt = nil
	s.dataHealthObserveRunning = false
	s.dataHealthObserveMu.Unlock()
}
