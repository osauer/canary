package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
)

const (
	daemonStateScope          = "daemon"
	stateKindPlatformSettings = "platform_settings"
	stateKindRiskCapital      = "risk_capital"
	stateKindNudges           = "governance_nudges"
	stateKindBrief            = "brief_baselines"
	stateKindRulesRegimeStage = "rules_regime_stage"
	stateKindProposalOutcomes = "proposal_outcomes"

	coreEventCapital          = "capital_event"
	coreEventRiskPolicy       = "risk_policy_event"
	coreEventRegimeDecision   = "regime_decision"
	coreEventStressDecision   = "stress_decision"
	coreEventRuleTransition   = "rule_transition"
	coreEventProposalOutcome  = "proposal_outcome"
	coreEventPlatformSettings = "platform_settings_update"
	coreEventActionRecord     = "record"
	coreEventActionUpdate     = "update"
	coreEventOriginDaemon     = "daemon_internal"
)

func coreEventKey(prefix string, at time.Time, payload []byte, ordinal int) string {
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%s:%020d:%06d:%s", prefix, at.UTC().UnixNano(), ordinal, hex.EncodeToString(digest[:8]))
}

func coreStoreEventKey(ctx context.Context, core *corestore.Store, prefix string, at time.Time, payload []byte, offset int) (string, error) {
	head, err := core.AuthorityHead(ctx)
	if err != nil {
		return "", err
	}
	return coreEventKey(prefix, at, payload, int(head.LastEventSeq)+offset+1), nil
}

func loadAllCoreEvents(ctx context.Context, core *corestore.Store, eventType string) ([]corestore.EventRecord, error) {
	var out []corestore.EventRecord
	var after int64
	for {
		page, err := core.LoadEvents(ctx, corestore.EventQuery{
			ScopeKey: daemonStateScope, Type: eventType, AfterEventSeq: after, Limit: 10000,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < 10000 {
			return out, nil
		}
		after = page[len(page)-1].EventSeq
	}
}

// bindAuthoritativeDaemonState is called by Start after the daemon owns the
// process/state locks and before it publishes a socket or touches the broker.
// Every attached store replaces any value loaded by a legacy test/import
// helper and from this point onward has no file fallback.
func (s *Server) bindAuthoritativeDaemonState(ctx context.Context, core *corestore.Store) error {
	if s == nil || core == nil {
		return fmt.Errorf("daemon SQLite authority is unavailable")
	}
	s.coreStore = core
	if s.platformSettings == nil {
		s.platformSettings = &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion}}
	}
	if err := s.platformSettings.bindCore(ctx, core); err != nil {
		return err
	}
	if s.riskCapital == nil {
		s.riskCapital = &riskCapitalStore{now: s.now}
	}
	if err := s.riskCapital.bindCore(ctx, core); err != nil {
		return err
	}
	if s.nudges == nil {
		s.nudges = &nudgeStateStore{now: s.now}
	}
	if err := s.nudges.bindCore(ctx, core); err != nil {
		return err
	}
	s.riskCapital.nudges = s.nudges
	s.riskCapital.observeConfirmedFlows = s.observeConfirmedFlows
	if s.briefState == nil {
		s.briefState = &briefStateStore{}
	}
	if err := s.briefState.bindCore(ctx, core); err != nil {
		return err
	}
	if err := s.flexFetch.bindCore(ctx, core); err != nil {
		return err
	}
	if err := s.dailyPnLObservations.bindCore(ctx, core); err != nil {
		return err
	}
	if err := s.bindRulesRegimeStage(ctx, core); err != nil {
		return err
	}
	if err := s.bindDecisionStores(ctx, core); err != nil {
		return err
	}
	// Policy source remains operator TOML, but its status transitions are
	// journaled only after the SQLite risk-capital authority is installed.
	if s.riskPolicies != nil {
		s.riskPolicies.reload()
	}
	return nil
}

func (s *Server) bindDecisionStores(ctx context.Context, core *corestore.Store) error {
	if s.regimeDecisions == nil {
		s.regimeDecisions = &regimeDecisionJournal{}
	}
	if s.canaryDecisions == nil {
		s.canaryDecisions = &canaryDecisionJournal{}
	}
	if s.proposalOutcomes == nil {
		s.proposalOutcomes = &proposalOutcomeStore{}
	}
	s.regimeDecisions.core = core
	s.canaryDecisions.core = core
	s.proposalOutcomes.core = core
	s.proposalOutcomes.mu.Lock()
	keys, err := s.proposalOutcomes.loadOutcomeKeysLocked()
	if err == nil {
		s.proposalOutcomes.outcomeKeys = keys
	}
	s.proposalOutcomes.mu.Unlock()
	if err != nil {
		return fmt.Errorf("load proposal outcomes from SQLite: %w", err)
	}
	// Force an early read of each fresh-epoch decision stream so corrupt
	// payloads fail startup rather than appearing as a partial history later.
	for _, kind := range []string{coreEventRegimeDecision, coreEventRuleTransition, coreEventStressDecision} {
		if _, err := loadAllCoreEvents(ctx, core, kind); err != nil {
			return fmt.Errorf("load %s events from SQLite: %w", kind, err)
		}
	}
	return nil
}
