package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	alertShadowStatusNotObserved = "not_observed"
	alertShadowStatusCurrent     = "current"
	alertShadowStatusPartial     = "partial"
	alertShadowStatusStale       = "stale"
	alertShadowStatusUnavailable = "unavailable"
	alertShadowStatusError       = "error"

	alertShadowReasonNotObserved                   = "not_observed"
	alertShadowReasonCurrent                       = "current"
	alertShadowReasonMissingRelevanceStamp         = "missing_relevance_stamp"
	alertShadowReasonInputHealthNotOK              = "input_health_not_ok"
	alertShadowReasonSourceHealthMissing           = "source_health_missing"
	alertShadowReasonSourceHealthIncomplete        = "source_health_incomplete"
	alertShadowReasonSourceHealthStale             = "source_health_stale"
	alertShadowReasonSourceHealthUnavailable       = "source_health_unavailable"
	alertShadowReasonSourceHealthError             = "source_health_error"
	alertShadowReasonSourceTimeInvalid             = "source_time_invalid"
	alertShadowReasonEvidenceFingerprintInvalid    = "evidence_fingerprint_invalid"
	alertShadowReasonPolicyFingerprintInvalid      = "policy_fingerprint_invalid"
	alertShadowReasonCandidateInvalid              = "candidate_invalid"
	alertShadowReasonSnapshotInvalid               = "snapshot_invalid"
	alertShadowReasonHealthUnapproved              = "health_unapproved"
	alertShadowReasonHealthStale                   = "health_stale"
	alertShadowReasonHealthUnavailable             = "health_unavailable"
	alertShadowReasonHealthError                   = "health_error"
	alertShadowReasonHealthTimeInvalid             = "health_time_invalid"
	alertShadowReasonBrokerScopeInvalid            = "broker_scope_invalid"
	alertShadowReasonRegistryApplyFailed           = "registry_apply_failed"
	alertShadowReasonSourceDisabled                = "source_disabled"
	alertShadowReasonConfirmationPending           = "confirmation_pending"
	alertShadowReasonLifecycleDataQuality          = "lifecycle_data_quality"
	alertShadowReasonPortfolioEvidenceStale        = "portfolio_evidence_stale"
	alertShadowReasonPortfolioEvidenceUnavailable  = "portfolio_evidence_unavailable"
	alertShadowDecisionLegacyGateActive            = "legacy_gate_active"
	alertShadowDecisionNudgeActive                 = "nudge_active"
	alertShadowDecisionRulebookActive              = "rulebook_active"
	alertShadowDecisionOrderIntegrityActive        = "order_integrity_active"
	alertShadowDecisionRegimeActive                = "regime_active"
	alertShadowDecisionProtectionActive            = "protection_active"
	alertShadowDecisionDataHealthActive            = "data_health_active"
	alertShadowDecisionClassifiedClear             = "classified_clear"
	alertShadowDecisionClassifiedNegativeUntrusted = "classified_negative_untrusted"

	alertShadowHumanLabelUnlabelled = "unlabelled"
)

// alertShadowExpectedSources is deliberately fixed. Delivery is not a producer
// and therefore never belongs in the measurement universe.
var alertShadowExpectedSources = [...]rpc.AlertSource{
	rpc.AlertSourceStress,
	rpc.AlertSourceRegime,
	rpc.AlertSourceRulebook,
	rpc.AlertSourceRiskPolicy,
	rpc.AlertSourceProtection,
	rpc.AlertSourceOrderIntegrity,
	rpc.AlertSourceReconciliation,
	rpc.AlertSourceGovernance,
	rpc.AlertSourceDataHealth,
}

var alertShadowNudgeSources = [...]rpc.AlertSource{
	rpc.AlertSourceRiskPolicy,
	rpc.AlertSourceReconciliation,
	rpc.AlertSourceGovernance,
}

const (
	alertShadowCursorStress = "stress"

	// stressEpisodeIdentity is hashed into the portfolio-stress episode key, so
	// it is a persisted lifecycle identity rather than a name. It stays
	// "portfolio_canary" because BuildAlertEpisodeKey is a SHA-256 over its
	// parts: changing this value produces a different key for the same
	// condition, with no way to map an old key to a new one. Every open
	// portfolio-stress episode would be abandoned mid-flight and reopen as a
	// fresh occurrence, re-paging the operator for an alert they already have.
	// It is only ever compared against itself, never displayed.
	stressEpisodeIdentity               = "portfolio_canary"
	alertShadowCursorNudges             = "nudges"
	alertShadowCursorRegime             = "regime"
	alertShadowCursorRulebook           = "rulebook"
	alertShadowCursorProtection         = "protection"
	alertShadowCursorOrderIntegrity     = "order_integrity"
	alertShadowCursorDataHealth         = "data_health"
	alertShadowStressSilenceHorizon     = 5 * time.Minute
	alertShadowNudgeSilenceHorizon      = time.Minute
	alertShadowRegimeSilenceHorizon     = 2 * time.Minute
	alertShadowRulebookSilenceHorizon   = 2 * time.Minute
	alertShadowProtectionSilenceHorizon = time.Minute
	alertShadowOrderSilenceHorizon      = 2 * time.Minute
	alertShadowDataHealthSilenceHorizon = time.Minute
	alertShadowHotPollRefreshInterval   = 30 * time.Second
	alertShadowReasonProducerSilent     = "producer_silent"
	protectionOrderUniverseJournaledAPI = string(rpc.AlertAuthorityUniverseJournaledAPIOrders)
	protectionOrderSnapshotMaxAge       = time.Minute
)

var alertShadowCanonicalRulebookRows = [...]struct {
	ID     string
	Number int
}{
	{risk.RuleSingleNameExposure, 1},
	{risk.RuleOptionLinePremium, 2},
	{risk.RuleCashSellOnly, 3},
	{risk.RuleExtrinsicBudget, 4},
	{risk.RuleExpiryRunway, 5},
	{risk.RuleCatalystCoverage, 6},
	{risk.RuleOverwriteEarnings, 7},
	{risk.RuleEarningsSizeFreeze, 8},
	{risk.RuleRedOnGreen, 9},
	{risk.RuleWinnerTrim, 10},
	{risk.RuleGreenDayAction, 11},
	{risk.RuleHedgeIntegrity, 12},
	{risk.RuleExitDiscipline, 13},
	{risk.RuleFXExposure, 14},
}

var alertShadowCanonicalRulebookHealth = [...]string{
	"account",
	"positions",
	"earnings",
	"regime_stage",
	"tape",
}

// alertShadowComposer is the source-neutral candidate producer boundary. It
// has no sender, pageability, cooldown, dwell, or threshold policy. Producers
// submit already classified typed snapshots; the composer maintains
// source-scoped coverage and lets alertEpisodeRegistry own durable lifecycle
// identity.
type alertShadowComposer struct {
	mu       sync.Mutex
	registry *alertEpisodeRegistry
	now      func() time.Time
	scopes   map[string]*alertShadowScopeState
}

type alertShadowScopeState struct {
	scope                alertShadowBrokerScope
	sources              map[rpc.AlertSource]alertShadowSourceBatch
	lastStress           alertShadowInputCursor
	lastNudges           alertShadowInputCursor
	lastRegime           alertShadowInputCursor
	lastRulebook         alertShadowInputCursor
	lastProtection       alertShadowInputCursor
	lastOrderIntegrity   alertShadowInputCursor
	lastDataHealth       alertShadowInputCursor
	orderConfirmations   map[string]struct{}
	pendingApplyFailures uint64
	applied              bool
}

type alertShadowInputCursor struct {
	AsOf        time.Time `json:"as_of,omitzero"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// alertShadowBrokerScope is a validated, normalized account/mode authority
// value. Raw parts exist only in memory and enter BuildAlertEpisodeKey; only
// the helper's opaque digest is persisted.
type alertShadowBrokerScope struct {
	account   string
	mode      string
	authority string
}

// alertShadowNudgeInput binds candidates, the exact constitution authority,
// and durable nudge-store health captured by one daemon composition call.
// Callers must not assemble these fields via independent later reads.
type alertShadowNudgeInput struct {
	Snapshot          rpc.NudgesSnapshotResult
	PolicyFingerprint rpc.Fingerprint
	StoreHealth       rpc.NudgeInputHealth
	Scope             alertShadowBrokerScope
}

// alertShadowProtectionInput binds the complete, unfiltered protection
// ledger to the portfolio-stream receipt that makes a negative trustworthy.
// General partial or unprotected rows are context only under the v1 producer
// policy; only orphaned and reconciliation-required facts are active.
type alertShadowProtectionInput struct {
	AsOf                  time.Time
	EvidenceAsOf          time.Time
	Status                string
	Summary               rpc.ProtectionCoverageSummary
	Scope                 alertShadowBrokerScope
	OrderSnapshotAsOf     time.Time
	OrderSnapshotComplete bool
	OrderUniverse         string
	orderJournal          *orderJournalStore
	orderAuthorityHeadSeq int64
}

type alertShadowGatewayPhase string

const (
	alertShadowGatewayConnecting alertShadowGatewayPhase = "connecting"
	alertShadowGatewayFailed     alertShadowGatewayPhase = "failed"
	alertShadowGatewayReady      alertShadowGatewayPhase = "ready"
)

// alertShadowDataHealthInput is one complete status.health evaluation. AsOf
// is the daemon observation time, not an upstream data timestamp: absence and
// failed due work can therefore remain distinct without fabricating evidence.
type alertShadowDataHealthInput struct {
	AsOf                  time.Time
	Health                rpc.HealthResult
	Scope                 alertShadowBrokerScope
	GatewayPhase          alertShadowGatewayPhase
	ProposalsExpected     bool
	OpportunitiesExpected bool
}

type alertShadowSourceBatch struct {
	Source                      rpc.AlertSource
	Status                      string
	Reason                      string
	AuthorityUniverse           rpc.AlertAuthorityUniverse
	InputAsOf                   time.Time
	ObservedAt                  time.Time
	EvidenceAsOf                time.Time
	EvidenceHealth              rpc.AlertEvidenceHealth
	FreshUntil                  time.Time
	PolicyFingerprint           string
	NegativeEvidenceFingerprint string
	NegativeSeverity            rpc.AlertSeverity
	NegativeDestination         rpc.AlertDestination
	Scope                       alertShadowBrokerScope
	Covered                     bool
	NegativeReady               bool
	DuplicateCandidates         uint64
	Observations                []alertEpisodeObservation
}

// alertShadowStatusReport is the producer operational view. Counts are
// descriptive registry measurements only: human precision and recall stay
// explicitly unlabelled until an operator supplies outcome labels.
type alertShadowStatusReport struct {
	AsOf                  time.Time                 `json:"as_of,omitzero"`
	ExpectedSources       []rpc.AlertSource         `json:"expected_sources"`
	Evaluations           uint64                    `json:"evaluations"`
	RegistryApplyFailures uint64                    `json:"registry_apply_failures"`
	Equivocations         uint64                    `json:"equivocations"`
	LastErrorCode         string                    `json:"last_error_code,omitempty"`
	HumanPrecision        string                    `json:"human_precision"`
	HumanRecall           string                    `json:"human_recall"`
	Sources               []alertShadowSourceStatus `json:"sources"`
}

type alertShadowSourceStatus struct {
	Source            rpc.AlertSource            `json:"source"`
	Status            string                     `json:"status"`
	Reason            string                     `json:"reason"`
	AuthorityUniverse rpc.AlertAuthorityUniverse `json:"authority_universe,omitempty"`
	InputAsOf         time.Time                  `json:"input_as_of,omitzero"`
	ObservedAt        time.Time                  `json:"observed_at,omitzero"`
	Covered           bool                       `json:"covered"`
	Active            int                        `json:"active_candidates"`
	Measurements      alertShadowSourceMetrics   `json:"measurements"`
}

type alertShadowSourceMetrics struct {
	Evaluations          uint64        `json:"evaluations"`
	CoveredEvaluations   uint64        `json:"covered_evaluations"`
	ActiveEvaluations    uint64        `json:"active_evaluations"`
	ActiveObservations   uint64        `json:"active_observations"`
	EpisodesOpened       uint64        `json:"episodes_opened"`
	EpisodesEscalated    uint64        `json:"episodes_escalated"`
	EpisodesRecovered    uint64        `json:"episodes_recovered"`
	EpisodesReopened     uint64        `json:"episodes_reopened"`
	DuplicateInputs      uint64        `json:"duplicate_inputs"`
	DuplicateCandidates  uint64        `json:"duplicate_candidates"`
	RepeatedActive       uint64        `json:"repeated_active_observations"`
	ActiveEvidenceChurn  uint64        `json:"active_evidence_revisions"`
	Equivocations        uint64        `json:"equivocations"`
	StaleSuppressions    uint64        `json:"stale_suppressions"`
	CoverageFailures     uint64        `json:"coverage_failures"`
	TimeToObserveSamples uint64        `json:"time_to_observe_samples"`
	TimeToObserveTotal   time.Duration `json:"time_to_observe_total"`
	TimeToObserveMax     time.Duration `json:"time_to_observe_max"`
}

func newAlertShadowComposer(registry *alertEpisodeRegistry) *alertShadowComposer {
	return &alertShadowComposer{registry: registry, now: time.Now, scopes: make(map[string]*alertShadowScopeState)}
}

func defaultAlertShadowSources(scope alertShadowBrokerScope) map[rpc.AlertSource]alertShadowSourceBatch {
	out := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowExpectedSources))
	for _, source := range alertShadowExpectedSources {
		batch := alertShadowSourceBatch{
			Source: source, Status: alertShadowStatusNotObserved, Reason: alertShadowReasonNotObserved,
			EvidenceHealth: rpc.AlertEvidenceUnavailable, Scope: scope, Observations: []alertEpisodeObservation{},
		}
		if source == rpc.AlertSourceProtection {
			batch.AuthorityUniverse = rpc.AlertAuthorityUniverseJournaledAPIOrders
		}
		out[source] = batch
	}
	return out
}

func newAlertShadowBrokerScope(scope brokerStateScope) (alertShadowBrokerScope, error) {
	if !brokerScopeConcrete(scope) {
		return alertShadowBrokerScope{}, errors.New("alert producer broker scope is not concrete")
	}
	account := strings.ToUpper(strings.TrimSpace(scope.Account))
	mode := strings.ToLower(strings.TrimSpace(scope.Mode))
	authority, err := rpc.BuildAlertAuthorityScope(account, mode)
	if err != nil {
		return alertShadowBrokerScope{}, err
	}
	return alertShadowBrokerScope{account: account, mode: mode, authority: authority}, nil
}

func (scope alertShadowBrokerScope) valid() bool {
	return scope.account != "" && brokerScopeConcrete(brokerStateScope{Account: scope.account, Mode: scope.mode}) &&
		rpc.ValidateAlertAuthorityScope(scope.authority) == nil
}

func (c *alertShadowComposer) scopeStateLocked(scope alertShadowBrokerScope) (*alertShadowScopeState, error) {
	if state := c.scopes[scope.authority]; state != nil {
		return state, nil
	}
	state := &alertShadowScopeState{scope: scope, sources: defaultAlertShadowSources(scope), orderConfirmations: make(map[string]struct{})}
	durable, ok, err := c.registry.scopeState(scope.authority)
	if err != nil {
		return nil, err
	}
	if ok {
		state.lastStress, state.lastNudges = durable.Cursors.Stress, durable.Cursors.Nudges
		state.lastRegime, state.lastRulebook = durable.Cursors.Regime, durable.Cursors.Rulebook
		state.lastProtection, state.lastOrderIntegrity = durable.Cursors.Protection, durable.Cursors.OrderIntegrity
		state.lastDataHealth = durable.Cursors.DataHealth
		for _, sourceState := range durable.SourceStates {
			batch := state.sources[sourceState.Source]
			batch.Status, batch.Reason = sourceState.Status, sourceState.Reason
			batch.InputAsOf, batch.ObservedAt, batch.EvidenceAsOf = sourceState.InputAsOf, sourceState.ObservedAt, sourceState.EvidenceAsOf
			batch.EvidenceHealth, batch.FreshUntil = sourceState.EvidenceHealth, sourceState.FreshUntil
			// Coverage is current-process knowledge. Durable producer evidence
			// remains available for audit and active-episode retention, but restart
			// cannot resurrect a trustworthy negative or omission boundary before
			// this process observes the source again.
			batch.Covered = false
			if sourceState.Covered {
				batch.Status, batch.Reason = alertShadowStatusNotObserved, alertShadowReasonNotObserved
				batch.EvidenceHealth = rpc.AlertEvidenceUnavailable
			}
			state.sources[sourceState.Source] = batch
		}
	}
	c.scopes[scope.authority] = state
	return state, nil
}

// ObserveStress consumes the daemon-authored Stress result. Eligibility is the
// existing legacy occurrence gate exactly, including its nil-relevance
// fail-open for positives. A missing relevance stamp can never authorize a
// negative or source coverage.
func (c *alertShadowComposer) ObserveStress(ctx context.Context, scope alertShadowBrokerScope, result rpc.StressResult) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if result.AsOf.IsZero() || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowStressInputFingerprint(scope, result)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastStress, result.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceStress}, observedAt); handled {
		return snapshot, err
	}
	previous := state.sources[rpc.AlertSourceStress]
	state.sources[rpc.AlertSourceStress] = alertShadowMapStress(scope, result, observedAt)
	cursor := alertShadowInputCursor{AsOf: result.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceStress}, alertShadowCursorStress, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceStress] = previous
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastStress = cursor
	return snapshot, nil
}

// ObserveNudges splits the canonical Nudge snapshot into three non-overlapping
// producer owners. policyFingerprint must be the current constitution semantic
// fingerprint, not candidate copy or rendered text.
func (c *alertShadowComposer) ObserveNudges(ctx context.Context, input alertShadowNudgeInput) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	result := input.Snapshot
	if result.AsOf.IsZero() || !input.Scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowNudgeInputFingerprint(input)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(input.Scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastNudges, result.AsOf, inputFingerprint, alertShadowNudgeSources[:], observedAt); handled {
		return snapshot, err
	}
	batches, duplicateCandidates, mapErr := alertShadowMapNudges(input, observedAt)
	if mapErr != nil {
		if dispositionErr := c.registry.RecordInputDisposition(ctx, input.Scope.authority, observedAt, alertShadowNudgeSources[:], alertShadowDispositionEquivocation); dispositionErr != nil {
			return rpc.AlertCandidateSnapshot{}, errors.Join(mapErr, dispositionErr)
		}
		snapshot, snapshotErr := c.currentSnapshotLocked(input.Scope)
		return snapshot, errors.Join(mapErr, snapshotErr)
	}
	previous := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowNudgeSources))
	for _, source := range alertShadowNudgeSources {
		previous[source] = state.sources[source]
		batch := batches[source]
		batch.DuplicateCandidates = duplicateCandidates[source]
		state.sources[source] = batch
	}
	cursor := alertShadowInputCursor{AsOf: result.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, alertShadowNudgeSources[:], alertShadowCursorNudges, cursor)
	if err != nil {
		for _, source := range alertShadowNudgeSources {
			state.sources[source] = previous[source]
		}
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastNudges = cursor
	return snapshot, nil
}

// ObserveRegime consumes the validated daemon-served last-good snapshot. Its
// cursor uses the evaluation time rather than the retained market-data as_of:
// authority health can become stale while the same semantic last-good remains
// served, and that transition must not be misclassified as timestamp
// equivocation. Only current early warning, confirmed stress, and panic are
// active under the producer policy; data_quality is never a warning.
func (c *alertShadowComposer) ObserveRegime(ctx context.Context, scope alertShadowBrokerScope, result rpc.RegimeSnapshotResult) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if result.AsOf.IsZero() || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowRegimeInputFingerprint(scope, result)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleUnchangedInputThrottleLocked(state, &state.lastRegime, inputFingerprint,
		[]rpc.AlertSource{rpc.AlertSourceRegime}, observedAt, alertShadowHotPollRefreshInterval); handled {
		return snapshot, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastRegime, observedAt, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceRegime}, observedAt); handled {
		return snapshot, err
	}
	previousSeverity, err := c.activeSourceSeverityLocked(scope, rpc.AlertSourceRegime, observedAt)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	previous := state.sources[rpc.AlertSourceRegime]
	state.sources[rpc.AlertSourceRegime] = alertShadowMapRegime(scope, result, observedAt, previousSeverity)
	cursor := alertShadowInputCursor{AsOf: observedAt.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceRegime}, alertShadowCursorRegime, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceRegime] = previous
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastRegime = cursor
	return snapshot, nil
}

// ObserveRulebook consumes the complete, unfiltered daemon rulebook result.
// The rulebook already owns every threshold and row classification; this
// adapter only projects watch/act facts into the source-neutral lifecycle.
// Degraded inputs may retain an active breach but can never authorize a clear.
func (c *alertShadowComposer) ObserveRulebook(ctx context.Context, scope alertShadowBrokerScope, result rpc.RulesResult) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if result.AsOf.IsZero() || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowRulebookInputFingerprint(scope, result)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(result.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastRulebook, result.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceRulebook}, observedAt); handled {
		return snapshot, err
	}
	previous := state.sources[rpc.AlertSourceRulebook]
	state.sources[rpc.AlertSourceRulebook] = alertShadowMapRulebook(scope, result, observedAt)
	cursor := alertShadowInputCursor{AsOf: result.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceRulebook}, alertShadowCursorRulebook, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceRulebook] = previous
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastRulebook = cursor
	return snapshot, nil
}

// ObserveProtection consumes a complete, unfiltered protection ledger plus
// the matching portfolio-stream receipt. The v1 producer policy opens
// only orphaned-order and reconciliation-required episodes. Partial and
// unprotected rows remain visible context and are authoritative negatives for
// this deliberately narrow producer; no universal stop obligation is implied.
func (c *alertShadowComposer) ObserveProtection(ctx context.Context, input alertShadowProtectionInput) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if input.AsOf.IsZero() || !input.Scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowProtectionInputFingerprint(input)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(input.Scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(input.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleUnchangedInputThrottleLocked(state, &state.lastProtection, inputFingerprint,
		[]rpc.AlertSource{rpc.AlertSourceProtection}, observedAt, alertShadowHotPollRefreshInterval); handled {
		return snapshot, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastProtection, input.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceProtection}, observedAt); handled {
		return snapshot, err
	}
	previous := state.sources[rpc.AlertSourceProtection]
	state.sources[rpc.AlertSourceProtection] = alertShadowMapProtection(input, observedAt)
	cursor := alertShadowInputCursor{AsOf: input.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceProtection}, alertShadowCursorProtection, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceProtection] = previous
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastProtection = cursor
	return snapshot, nil
}

// ObserveOrderIntegrity mirrors the established two-consecutive-pass mismatch
// gate inside the source-neutral candidate path. The app dispatcher owns
// delivery. A negative is trusted only when the daemon's portfolio stream
// receipts and scoped order journal were current at the same read.
func (c *alertShadowComposer) ObserveOrderIntegrity(ctx context.Context, scope alertShadowBrokerScope, input orderIntegrityEvaluation) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if input.AsOf.IsZero() || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowOrderIntegrityInputFingerprint(scope, input)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(input.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastOrderIntegrity, input.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceOrderIntegrity}, observedAt); handled {
		return snapshot, err
	}
	previousBatch := state.sources[rpc.AlertSourceOrderIntegrity]
	previousConfirmations := state.orderConfirmations
	batch, confirmations := alertShadowMapOrderIntegrity(scope, input, observedAt, state.orderConfirmations)
	state.sources[rpc.AlertSourceOrderIntegrity] = batch
	state.orderConfirmations = confirmations
	cursor := alertShadowInputCursor{AsOf: input.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceOrderIntegrity}, alertShadowCursorOrderIntegrity, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceOrderIntegrity] = previousBatch
		state.orderConfirmations = previousConfirmations
		c.recordApplyFailureLocked(ctx, state, observedAt)
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastOrderIntegrity = cursor
	return snapshot, nil
}

// ObserveDataHealth consumes the complete typed status projection. It creates
// one source-neutral episode per failing root source in the approved allowlist;
// not_due, computing, and intentionally disabled states are not outages.
func (c *alertShadowComposer) ObserveDataHealth(ctx context.Context, input alertShadowDataHealthInput) (rpc.AlertCandidateSnapshot, error) {
	if c == nil || c.registry == nil || ctx == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer is unavailable")
	}
	if input.AsOf.IsZero() || !input.Scope.valid() {
		return rpc.AlertCandidateSnapshot{}, errors.New(alertShadowReasonBrokerScopeInvalid)
	}
	inputFingerprint, err := alertShadowDataHealthInputFingerprint(input)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(input.Scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observedAt, err := c.observationTimeLocked(input.AsOf)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	if snapshot, handled, err := c.handleUnchangedInputThrottleLocked(state, &state.lastDataHealth, inputFingerprint,
		[]rpc.AlertSource{rpc.AlertSourceDataHealth}, observedAt, alertShadowHotPollRefreshInterval); handled {
		return snapshot, err
	}
	if snapshot, handled, err := c.handleInputCursorLocked(ctx, state, &state.lastDataHealth, input.AsOf, inputFingerprint, []rpc.AlertSource{rpc.AlertSourceDataHealth}, observedAt); handled {
		return snapshot, err
	}
	previous := state.sources[rpc.AlertSourceDataHealth]
	state.sources[rpc.AlertSourceDataHealth] = alertShadowMapDataHealth(input, observedAt)
	cursor := alertShadowInputCursor{AsOf: input.AsOf.UTC(), Fingerprint: inputFingerprint}
	snapshot, err := c.applyLocked(ctx, state, observedAt, []rpc.AlertSource{rpc.AlertSourceDataHealth}, alertShadowCursorDataHealth, cursor)
	if err != nil {
		state.sources[rpc.AlertSourceDataHealth] = previous
		// Data Health diagnoses the same SQLite authority that persists this
		// registry. Do not synchronously retry a failed store from the status
		// path; retain the measurement in memory for the next successful apply.
		state.pendingApplyFailures++
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.lastDataHealth = cursor
	return snapshot, nil
}

// Snapshot returns only durable registry state. false means the composer has
// not committed an evaluation yet; callers must not manufacture a clear.
func (c *alertShadowComposer) Snapshot(scope alertShadowBrokerScope) (rpc.AlertCandidateSnapshot, bool, error) {
	if c == nil || c.registry == nil || !scope.valid() {
		return rpc.AlertCandidateSnapshot{}, false, errors.New("alert producer is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := c.scopeStateLocked(scope)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, false, err
	}
	now := c.nowLocked()
	snapshot, ok, err := c.registry.Snapshot(scope.authority, now)
	if err != nil || !ok {
		return snapshot, ok, err
	}
	snapshot, err = c.projectProcessSnapshotLocked(snapshot, state, now)
	return snapshot, true, err
}

func (c *alertShadowComposer) Status(scope alertShadowBrokerScope) alertShadowStatusReport {
	report := alertShadowStatusReport{ExpectedSources: alertShadowExpectedSourceSlice(), HumanPrecision: alertShadowHumanLabelUnlabelled,
		HumanRecall: alertShadowHumanLabelUnlabelled, Sources: make([]alertShadowSourceStatus, 0, len(alertShadowExpectedSources))}
	if c == nil || c.registry == nil || !scope.valid() {
		return report
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.nowLocked()
	state, _ := c.scopeStateLocked(scope)
	durable, ok, err := c.registry.scopeState(scope.authority)
	if err != nil {
		report.LastErrorCode = alertShadowReasonRegistryApplyFailed
		return report
	}
	metricsBySource := make(map[rpc.AlertSource]alertShadowSourceMetrics)
	if ok {
		report.AsOf, report.Evaluations = durable.Metrics.AsOf, durable.Metrics.Evaluations
		report.RegistryApplyFailures, report.Equivocations = durable.Metrics.RegistryApplyFailures, durable.Metrics.Equivocations
		report.LastErrorCode = durable.Metrics.LastErrorCode
		for _, row := range durable.Metrics.Sources {
			metricsBySource[row.Source] = row.Measurements
		}
	}
	clockInvalid := ok && !durable.AsOf.IsZero() && durable.AsOf.After(now)
	if clockInvalid {
		report.LastErrorCode = alertShadowReasonSourceTimeInvalid
	}
	if state != nil && state.pendingApplyFailures > 0 {
		report.RegistryApplyFailures += state.pendingApplyFailures
		report.LastErrorCode = alertShadowReasonRegistryApplyFailed
	}
	activeBySource := make(map[rpc.AlertSource]int)
	if snapshot, snapshotOK, snapshotErr := c.registry.Snapshot(scope.authority, now); snapshotErr == nil && snapshotOK {
		for _, candidate := range snapshot.Candidates {
			if candidate.State != rpc.AlertEpisodeRecovered {
				activeBySource[candidate.Source]++
			}
		}
	}
	defaults := defaultAlertShadowSources(scope)
	for _, source := range alertShadowExpectedSources {
		batch := defaults[source]
		if state != nil {
			batch = state.sources[source]
		}
		covered := batch.Covered
		if covered && !batch.FreshUntil.IsZero() && now.After(batch.FreshUntil) {
			covered = false
			batch.Status, batch.Reason = alertShadowStatusStale, alertShadowReasonProducerSilent
		}
		if clockInvalid || (!batch.InputAsOf.IsZero() && batch.InputAsOf.After(now)) || (!batch.ObservedAt.IsZero() && batch.ObservedAt.After(now)) {
			covered = false
			batch.Status, batch.Reason = alertShadowStatusError, alertShadowReasonSourceTimeInvalid
		}
		report.Sources = append(report.Sources, alertShadowSourceStatus{
			Source: source, Status: batch.Status, Reason: batch.Reason, AuthorityUniverse: batch.AuthorityUniverse, InputAsOf: batch.InputAsOf,
			ObservedAt: batch.ObservedAt, Covered: covered, Active: activeBySource[source], Measurements: metricsBySource[source],
		})
	}
	return report
}

func (c *alertShadowComposer) handleInputCursorLocked(ctx context.Context, state *alertShadowScopeState, cursor *alertShadowInputCursor, asOf time.Time, fingerprint string, sources []rpc.AlertSource, observedAt time.Time) (rpc.AlertCandidateSnapshot, bool, error) {
	asOf = asOf.UTC()
	disposition := ""
	var resultErr error
	if !cursor.AsOf.IsZero() && asOf.Before(cursor.AsOf) {
		disposition = alertShadowDispositionStale
	} else if asOf.Equal(cursor.AsOf) {
		if fingerprint != cursor.Fingerprint {
			disposition = alertShadowDispositionEquivocation
			resultErr = errors.New("alert producer input timestamp equivocation")
		} else if alertShadowSourcesNeedObservation(state, sources) {
			// The first identical producer read after restart is a real process
			// observation, not a duplicate. Re-evaluate it once so current-process
			// coverage can be re-established without waiting for semantic churn.
			return rpc.AlertCandidateSnapshot{}, false, nil
		} else {
			disposition = alertShadowDispositionDuplicate
		}
	}
	if disposition == "" {
		return rpc.AlertCandidateSnapshot{}, false, nil
	}
	if err := c.registry.RecordInputDisposition(ctx, state.scope.authority, observedAt, sources, disposition); err != nil {
		return rpc.AlertCandidateSnapshot{}, true, err
	}
	snapshot, snapshotErr := c.currentSnapshotLocked(state.scope)
	return snapshot, true, errors.Join(resultErr, snapshotErr)
}

// handleUnchangedInputThrottleLocked keeps five-second app polling from
// turning semantically identical Protection or Data Health reads into a
// SQLite write storm. It never suppresses the first observation after restart
// and the refresh interval remains comfortably inside each producer's silence
// horizon.
func (c *alertShadowComposer) handleUnchangedInputThrottleLocked(state *alertShadowScopeState, cursor *alertShadowInputCursor, fingerprint string, sources []rpc.AlertSource, observedAt time.Time, interval time.Duration) (rpc.AlertCandidateSnapshot, bool, error) {
	if state == nil || cursor == nil || cursor.AsOf.IsZero() || cursor.Fingerprint != fingerprint ||
		!observedAt.After(cursor.AsOf) || observedAt.Sub(cursor.AsOf) >= interval || alertShadowSourcesNeedObservation(state, sources) {
		return rpc.AlertCandidateSnapshot{}, false, nil
	}
	snapshot, err := c.currentSnapshotLocked(state.scope)
	return snapshot, true, err
}

func (c *alertShadowComposer) activeSourceSeverityLocked(scope alertShadowBrokerScope, source rpc.AlertSource, at time.Time) (rpc.AlertSeverity, error) {
	snapshot, ok, err := c.registry.Snapshot(scope.authority, at.UTC())
	if err != nil || !ok {
		return "", err
	}
	severity := rpc.AlertSeverity("")
	for _, candidate := range snapshot.Candidates {
		if candidate.Source != source || candidate.State == rpc.AlertEpisodeRecovered {
			continue
		}
		if severity != "" {
			return "", errors.New("alert producer source has multiple active singleton episodes")
		}
		severity = candidate.Severity
	}
	return severity, nil
}

func alertShadowSourcesNeedObservation(state *alertShadowScopeState, sources []rpc.AlertSource) bool {
	if state == nil {
		return true
	}
	for _, source := range sources {
		batch := state.sources[source]
		if !batch.Covered && batch.Status == alertShadowStatusNotObserved && batch.Reason == alertShadowReasonNotObserved {
			return true
		}
	}
	return false
}

func (c *alertShadowComposer) currentSnapshotLocked(scope alertShadowBrokerScope) (rpc.AlertCandidateSnapshot, error) {
	now := c.nowLocked()
	snapshot, ok, err := c.registry.Snapshot(scope.authority, now)
	if err != nil || !ok {
		return rpc.AlertCandidateSnapshot{}, err
	}
	state := c.scopes[scope.authority]
	if state == nil {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert producer process scope is unavailable")
	}
	return c.projectProcessSnapshotLocked(snapshot, state, now)
}

func (c *alertShadowComposer) projectProcessSnapshotLocked(snapshot rpc.AlertCandidateSnapshot, state *alertShadowScopeState, now time.Time) (rpc.AlertCandidateSnapshot, error) {
	coverage := alertShadowCoverage(now, state.sources)
	sources := alertSourceCoverageFromBatches(now, state.sources)
	if now.Before(snapshot.AsOf) {
		coverage = rpc.AlertCoverage{
			State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: snapshot.AsOf,
			ExpectedSources: append([]rpc.AlertSource(nil), coverage.ExpectedSources...), CoveredSources: []rpc.AlertSource{},
		}
		sources = unavailableAlertSourceCoverage(coverage.ExpectedSources)
	}
	covered := make(map[rpc.AlertSource]struct{}, len(coverage.CoveredSources))
	for _, source := range coverage.CoveredSources {
		covered[source] = struct{}{}
	}
	filtered := make([]rpc.AlertCandidate, 0, len(snapshot.Candidates))
	for _, candidate := range snapshot.Candidates {
		_, sourceCovered := covered[candidate.Source]
		if candidate.State == rpc.AlertEpisodeRecovered && (!state.applied || !sourceCovered) {
			continue
		}
		if candidate.EvidenceHealth == rpc.AlertEvidenceCurrent && !sourceCovered {
			health := state.sources[candidate.Source].EvidenceHealth
			if health == "" || health == rpc.AlertEvidenceCurrent {
				health = rpc.AlertEvidenceUnavailable
			}
			candidate.EvidenceHealth = health
		}
		filtered = append(filtered, candidate)
	}
	snapshot.Coverage = coverage
	snapshot.Sources = sources
	snapshot.Candidates = filtered
	snapshot.CurrentState = rpc.AlertSnapshotUnknown
	for _, candidate := range filtered {
		if candidate.State == rpc.AlertEpisodeOpen || candidate.State == rpc.AlertEpisodeEscalated {
			snapshot.CurrentState = rpc.AlertSnapshotActive
			break
		}
	}
	if snapshot.CurrentState != rpc.AlertSnapshotActive && coverage.State == rpc.AlertCoverageComplete && coverage.Freshness == rpc.AlertCoverageCurrent {
		snapshot.CurrentState = rpc.AlertSnapshotClear
	}
	if err := rpc.ValidateAlertCandidateSnapshot(snapshot); err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	return snapshot, nil
}

func alertSourceCoverageFromBatches(asOf time.Time, sources map[rpc.AlertSource]alertShadowSourceBatch) []rpc.AlertSourceCoverage {
	rows := make([]rpc.AlertSourceCoverage, 0, len(alertShadowExpectedSources))
	for _, source := range alertShadowExpectedSources {
		batch := sources[source]
		row := rpc.AlertSourceCoverage{
			Source: source, Status: batch.Status, Reason: batch.Reason, EvidenceHealth: batch.EvidenceHealth,
			InputAsOf: batch.InputAsOf, ObservedAt: batch.ObservedAt, EvidenceAsOf: batch.EvidenceAsOf,
			FreshUntil: batch.FreshUntil, Covered: batch.Covered,
		}
		if row.Status == "" {
			row.Status, row.Reason, row.EvidenceHealth = alertShadowStatusNotObserved, alertShadowReasonNotObserved, rpc.AlertEvidenceUnavailable
		}
		if row.Covered && !row.FreshUntil.IsZero() && asOf.After(row.FreshUntil) {
			row.Status, row.Reason = alertShadowStatusStale, alertShadowReasonProducerSilent
			if row.EvidenceHealth != rpc.AlertEvidenceError {
				row.EvidenceHealth = rpc.AlertEvidenceStale
			}
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Source < rows[j].Source })
	return rows
}

func (c *alertShadowComposer) recordApplyFailureLocked(ctx context.Context, state *alertShadowScopeState, at time.Time) {
	// The failed evaluation context may already be cancelled. A bounded,
	// cancellation-detached follow-up may still persist measurement evidence
	// without advancing producer cursors or lifecycle. If SQLite itself remains
	// unavailable, retain an in-memory pending count for the next successful
	// evaluation; there is no alternate file authority.
	persistContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := c.registry.RecordApplyFailure(persistContext, state.scope.authority, at); err != nil {
		state.pendingApplyFailures++
	}
}

func (c *alertShadowComposer) nowLocked() time.Time {
	now := time.Now().UTC()
	if c.now != nil {
		now = c.now().UTC()
	}
	return now
}

func (c *alertShadowComposer) observationTimeLocked(sourceAsOf time.Time) (time.Time, error) {
	now := c.nowLocked()
	if now.Before(sourceAsOf) {
		return time.Time{}, errors.New("alert producer as_of is in the future")
	}
	return now, nil
}

func (c *alertShadowComposer) applyLocked(ctx context.Context, state *alertShadowScopeState, evaluationAt time.Time, opportunitySources []rpc.AlertSource, cursorKind string, cursor alertShadowInputCursor) (rpc.AlertCandidateSnapshot, error) {
	evaluationAt = evaluationAt.UTC()
	durable, durableOK, err := c.registry.scopeState(state.scope.authority)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	} else if durableOK && evaluationAt.Before(durable.AsOf) {
		return rpc.AlertCandidateSnapshot{}, errors.New("alert registry authority clock is behind durable scoped state")
	}
	durablePolicyByEpisode := make(map[string]string, len(durable.Episodes))
	if durableOK {
		for _, record := range durable.Episodes {
			durablePolicyByEpisode[record.EpisodeKey] = record.PolicyFingerprint
		}
	}
	updated := make(map[rpc.AlertSource]struct{}, len(opportunitySources))
	for _, source := range opportunitySources {
		updated[source] = struct{}{}
		batch := state.sources[source]
		batch.ObservedAt = evaluationAt
		for i := range batch.Observations {
			batch.Observations[i].ObservedAt = evaluationAt
		}
		state.sources[source] = batch
	}
	before, beforeOK, err := c.registry.Snapshot(state.scope.authority, evaluationAt)
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	observations := make([]alertEpisodeObservation, 0)
	represented := make(map[string]struct{})
	for _, source := range opportunitySources {
		batch := state.sources[source]
		for _, observation := range batch.Observations {
			observations = append(observations, observation)
			if observation.Active {
				represented[observation.EpisodeKey] = struct{}{}
			}
		}
	}
	if beforeOK {
		for _, candidate := range before.Candidates {
			if candidate.State == rpc.AlertEpisodeRecovered {
				continue
			}
			if _, present := represented[candidate.EpisodeKey]; present {
				continue
			}
			if _, isUpdated := updated[candidate.Source]; !isUpdated {
				continue
			}
			batch := state.sources[candidate.Source]
			if !batch.NegativeReady || !alertShadowCandidateMatchesScope(candidate, batch.Scope) {
				continue
			}
			// The retired Protection classifier cannot be recovered by a successor
			// authority universe merely because the new classifier omitted it. Other
			// sources use stable episode identities across legitimate policy changes,
			// so their current negative is the authoritative recovery evidence.
			if policy := durablePolicyByEpisode[candidate.EpisodeKey]; !alertShadowPolicyMayRecover(candidate.Source, policy, batch.PolicyFingerprint) {
				continue
			}
			reason := alertShadowDecisionClassifiedNegativeUntrusted
			if batch.Covered && batch.EvidenceHealth == rpc.AlertEvidenceCurrent {
				reason = alertShadowDecisionClassifiedClear
			}
			evidenceFingerprint := batch.NegativeEvidenceFingerprint
			if evidenceFingerprint == "" && !batch.Covered {
				// An untrusted negative may update health/source time, but must
				// retain the producer identity needed to prove broker scope on a
				// later authoritative observation.
				evidenceFingerprint = candidate.EvidenceFingerprint
			}
			if evidenceFingerprint == "" {
				evidenceFingerprint, err = alertShadowFingerprint(struct {
					Decision          string          `json:"decision"`
					Source            rpc.AlertSource `json:"source"`
					EpisodeKey        string          `json:"episode_key"`
					PolicyFingerprint string          `json:"policy_fingerprint"`
				}{"negative", candidate.Source, candidate.EpisodeKey, batch.PolicyFingerprint})
				if err != nil {
					return rpc.AlertCandidateSnapshot{}, err
				}
			}
			severity := candidate.Severity
			if batch.NegativeSeverity != "" {
				severity = batch.NegativeSeverity
			}
			destination := candidate.Destination
			if batch.NegativeDestination != "" {
				destination = batch.NegativeDestination
			}
			observations = append(observations, alertEpisodeObservation{
				EpisodeKey: candidate.EpisodeKey, Source: candidate.Source, Kind: candidate.Kind,
				PresentationCode: candidate.PresentationCode, Active: false, Severity: severity,
				EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: batch.EvidenceHealth,
				Destination: destination, EvidenceAsOf: batch.EvidenceAsOf, ObservedAt: evaluationAt,
				PolicyFingerprint: batch.PolicyFingerprint, ProducerDecisionReason: reason,
			})
		}
	}
	coverage := alertShadowCoverage(evaluationAt, state.sources)
	sourceStates := make([]alertEpisodeRegistrySourceState, 0, len(opportunitySources))
	for _, source := range opportunitySources {
		sourceStates = append(sourceStates, alertShadowRegistrySourceState(state.sources[source]))
	}
	snapshot, err := c.registry.Apply(ctx, alertEpisodeEvaluation{AuthorityScope: state.scope.authority, AsOf: evaluationAt,
		Coverage: coverage, Observations: observations, OpportunitySources: append([]rpc.AlertSource(nil), opportunitySources...),
		SourceStates: sourceStates, CursorKind: cursorKind, Cursor: cursor, PendingApplyFailures: state.pendingApplyFailures})
	if err != nil {
		return rpc.AlertCandidateSnapshot{}, err
	}
	state.pendingApplyFailures = 0
	state.applied = true
	return snapshot, nil
}

func alertShadowPolicyMayRecover(source rpc.AlertSource, durablePolicy, currentPolicy string) bool {
	if durablePolicy == "" || currentPolicy == "" {
		return false
	}
	if source == rpc.AlertSourceProtection {
		return durablePolicy == currentPolicy
	}
	return true
}

func alertShadowRegistrySourceState(batch alertShadowSourceBatch) alertEpisodeRegistrySourceState {
	return alertEpisodeRegistrySourceState{Source: batch.Source, Status: batch.Status, Reason: batch.Reason,
		InputAsOf: batch.InputAsOf, ObservedAt: batch.ObservedAt, EvidenceAsOf: batch.EvidenceAsOf,
		EvidenceHealth: batch.EvidenceHealth, Covered: batch.Covered, FreshUntil: batch.FreshUntil,
		DuplicateCandidates: batch.DuplicateCandidates}
}

func alertShadowCoverage(asOf time.Time, sources map[rpc.AlertSource]alertShadowSourceBatch) rpc.AlertCoverage {
	covered := make([]rpc.AlertSource, 0, len(alertShadowExpectedSources))
	stale := false
	for _, source := range alertShadowExpectedSources {
		batch := sources[source]
		if batch.Covered {
			covered = append(covered, source)
			if !batch.FreshUntil.IsZero() && asOf.After(batch.FreshUntil) {
				stale = true
			}
		}
	}
	coverage := rpc.AlertCoverage{
		State: rpc.AlertCoverageUnavailable, Freshness: rpc.AlertCoverageUnknown, AsOf: asOf,
		ExpectedSources: alertShadowExpectedSourceSlice(), CoveredSources: covered,
	}
	if len(covered) > 0 {
		coverage.State = rpc.AlertCoveragePartial
		coverage.Freshness = rpc.AlertCoverageCurrent
		if len(covered) == len(coverage.ExpectedSources) {
			coverage.State = rpc.AlertCoverageComplete
		}
		if stale {
			coverage.Freshness = rpc.AlertCoverageStale
		}
	}
	return coverage
}

func alertShadowExpectedSourceSlice() []rpc.AlertSource {
	out := make([]rpc.AlertSource, len(alertShadowExpectedSources))
	copy(out, alertShadowExpectedSources[:])
	return out
}

var alertShadowRegimeRequiredSources = [...]string{"breadth", "credit", "funding", "fx", "gamma", "vol"}

func alertShadowMapRegime(scope alertShadowBrokerScope, result rpc.RegimeSnapshotResult, observedAt time.Time, previousSeverity rpc.AlertSeverity) alertShadowSourceBatch {
	policyFingerprint := opaqueIdentity("alert-shadow-regime-policy", "market-stress-v1")
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceRegime, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: observedAt.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: result.AsOf.UTC(),
		EvidenceHealth: rpc.AlertEvidenceUnavailable, FreshUntil: observedAt.UTC().Add(alertShadowRegimeSilenceHorizon),
		PolicyFingerprint: policyFingerprint, NegativeSeverity: rpc.AlertSeverityObserve,
		NegativeDestination: rpc.AlertDestinationAlerts, Scope: scope, Observations: []alertEpisodeObservation{},
	}
	valid, covered, health, reason, evidenceAsOf, lifecycle := alertShadowRegimeEvidence(result, observedAt)
	batch.EvidenceHealth, batch.Reason = health, reason
	batch.Status = alertShadowStatusForEvidence(health)
	if !evidenceAsOf.IsZero() {
		batch.EvidenceAsOf = evidenceAsOf
	}
	if valid && validAlertRegistryFingerprint(result.Fingerprint.Key) {
		batch.NegativeEvidenceFingerprint = result.Fingerprint.Key
		batch.NegativeReady = !batch.EvidenceAsOf.IsZero()
	}
	batch.Covered = covered
	if covered {
		batch.Status, batch.Reason = alertShadowStatusCurrent, alertShadowReasonCurrent
	}
	if !valid {
		return batch
	}

	severity, active, stageValid := alertShadowRegimeStagePolicy(lifecycle)
	if !stageValid {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
		return batch
	}
	if !active {
		return batch
	}
	if result.AuthorityHealth == nil || result.AuthorityHealth.Status != rpc.RegimeAuthorityFresh {
		return batch
	}
	// A broken early-warning input is undefined, never a warning. Confirmed
	// stress and panic may remain active with partial evidence only because the
	// lifecycle authority already proved an independently current co-signature.
	if lifecycle.Stage == rpc.LifecycleEarlyWarning && !covered {
		return batch
	}
	if !covered && lifecycle.Readiness != "degraded" {
		return batch
	}
	episodeKey, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceRegime, rpc.AlertKindMarketState,
		scope.account, scope.mode, "market_stress")
	if err != nil {
		batch.Covered = false
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
		return batch
	}
	escalation := ""
	if previousSeverity != "" && alertShadowSeverityRank(severity) > alertShadowSeverityRank(previousSeverity) {
		escalation = opaqueIdentity("alert-shadow-regime-escalation", string(severity))
	}
	batch.Observations = append(batch.Observations, alertEpisodeObservation{
		EpisodeKey: episodeKey, Source: rpc.AlertSourceRegime, Kind: rpc.AlertKindMarketState, Active: true,
		PresentationCode:      rpc.AlertPresentationRegimeMarketStress,
		EscalationFingerprint: escalation, Severity: severity,
		EvidenceFingerprint: result.Fingerprint.Key, EvidenceHealth: batch.EvidenceHealth,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: batch.EvidenceAsOf, ObservedAt: observedAt.UTC(),
		PolicyFingerprint: policyFingerprint, ProducerDecisionReason: alertShadowDecisionRegimeActive,
	})
	return batch
}

func alertShadowRegimeEvidence(result rpc.RegimeSnapshotResult, observedAt time.Time) (valid, covered bool, health rpc.AlertEvidenceHealth, reason string, evidenceAsOf time.Time, lifecycle rpc.LifecycleState) {
	if result.AsOf.IsZero() || result.AsOf.After(observedAt) || result.Fingerprint.Version != rpc.RegimeFingerprintVersion ||
		!validAlertRegistryFingerprint(result.Fingerprint.Key) || result.Fingerprint != rpc.BuildRegimeFingerprint(&result) ||
		result.Lifecycle.Fingerprint != rpc.BuildLifecycleFingerprint(result.Lifecycle) {
		return false, false, rpc.AlertEvidenceError, alertShadowReasonEvidenceFingerprintInvalid, time.Time{}, rpc.LifecycleState{}
	}
	classifiedAtPublication := result
	classifiedAtPublication.SourceHealth = slices.Clone(result.SourceHealth)
	for i := range classifiedAtPublication.SourceHealth {
		source := &classifiedAtPublication.SourceHealth[i]
		if !source.AsOf.IsZero() && !source.AsOf.After(result.AsOf) {
			source.AgeSeconds = int64(result.AsOf.Sub(source.AsOf).Seconds())
		}
	}
	expectedLifecycle := rpc.BuildRegimeLifecycle(&classifiedAtPublication)
	if expectedLifecycle.Fingerprint != result.Lifecycle.Fingerprint {
		return false, false, rpc.AlertEvidenceError, alertShadowReasonSnapshotInvalid, time.Time{}, rpc.LifecycleState{}
	}
	evidenceAsOf = result.AsOf.UTC()
	if result.AuthorityHealth == nil {
		return true, false, rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable, evidenceAsOf, result.Lifecycle
	}
	if err := rpc.ValidateRegimeAuthorityHealth(*result.AuthorityHealth); err != nil {
		return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
	}
	if result.AuthorityHealth.LastSuccessAt != nil {
		lastSuccess := result.AuthorityHealth.LastSuccessAt.UTC()
		if lastSuccess.After(observedAt) {
			return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceTimeInvalid, evidenceAsOf, rpc.LifecycleState{}
		}
		if lastSuccess.Before(evidenceAsOf) {
			evidenceAsOf = lastSuccess
		}
	}
	health, reason = rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	switch result.AuthorityHealth.Status {
	case rpc.RegimeAuthorityFresh:
	case rpc.RegimeAuthorityStale:
		health, reason = rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
	case rpc.RegimeAuthorityUnavailable:
		health, reason = rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable
	default:
		return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
	}

	classifiedNow := result
	classifiedNow.SourceHealth = slices.Clone(result.SourceHealth)
	seen := make(map[string]struct{}, len(result.SourceHealth))
	for i, source := range result.SourceHealth {
		name := strings.ToLower(strings.TrimSpace(source.Source))
		if name == "" {
			return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
		}
		if _, duplicate := seen[name]; duplicate {
			return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
		}
		seen[name] = struct{}{}
		if source.AsOf.IsZero() || source.AsOf.After(result.AsOf) || source.AsOf.After(observedAt) ||
			source.MaxAgeSeconds != rpc.RegimeSourceMaxAgeSeconds(name) {
			return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceTimeInvalid, evidenceAsOf, rpc.LifecycleState{}
		}
		classifiedNow.SourceHealth[i].AgeSeconds = int64(observedAt.Sub(source.AsOf).Seconds())
		if source.AsOf.Before(evidenceAsOf) {
			evidenceAsOf = source.AsOf.UTC()
		}
		candidateHealth, candidateReason, ok := alertShadowRegimeSourceEvidence(name, source, observedAt)
		if !ok {
			return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
		}
		if alertShadowEvidenceRank(candidateHealth) > alertShadowEvidenceRank(health) {
			health, reason = candidateHealth, candidateReason
		}
	}
	for _, required := range alertShadowRegimeRequiredSources {
		if _, ok := seen[required]; !ok {
			return true, false, rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthMissing, evidenceAsOf, result.Lifecycle
		}
	}
	if len(seen) != len(alertShadowRegimeRequiredSources) {
		return false, false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, evidenceAsOf, rpc.LifecycleState{}
	}
	lifecycle = rpc.BuildRegimeLifecycle(&classifiedNow)

	switch lifecycle.Readiness {
	case "ready":
	case "degraded":
		if alertShadowEvidenceRank(rpc.AlertEvidencePartial) > alertShadowEvidenceRank(health) {
			health, reason = rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
		}
	case "blocked":
		health, reason = rpc.AlertEvidencePartial, alertShadowReasonLifecycleDataQuality
	default:
		return false, false, rpc.AlertEvidenceError, alertShadowReasonSnapshotInvalid, evidenceAsOf, rpc.LifecycleState{}
	}
	if lifecycle.Stage == rpc.LifecycleDataQuality {
		health, reason = rpc.AlertEvidencePartial, alertShadowReasonLifecycleDataQuality
	}
	covered = result.AuthorityHealth.Status == rpc.RegimeAuthorityFresh && health == rpc.AlertEvidenceCurrent &&
		lifecycle.Readiness == "ready" && lifecycle.Stage != rpc.LifecycleDataQuality
	return true, covered, health, reason, evidenceAsOf, lifecycle
}

func alertShadowRegimeSourceEvidence(name string, source rpc.SourceHealth, observedAt time.Time) (rpc.AlertEvidenceHealth, string, bool) {
	switch source.RefreshState {
	case "", rpc.SourceRefreshCurrent:
	case rpc.SourceRefreshNotDue:
		if name == "vol" || name == "gamma" {
			switch source.Status {
			case rpc.SourceStatusOK, rpc.SourceStatusStale:
				return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, true
			}
		}
	case rpc.SourceRefreshFetchFailed, rpc.SourceRefreshFetchFailedBackoff:
		// A failed due refresh is a data-health fact. The retained source row
		// still decides whether Regime evidence is stale or unavailable.
	default:
		return rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, false
	}
	if observedAt.Sub(source.AsOf) >= time.Duration(source.MaxAgeSeconds)*time.Second {
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale, true
	}
	switch source.Status {
	case rpc.SourceStatusOK:
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, true
	case rpc.SourceStatusPartial, rpc.SourceStatusDegraded:
		return rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete, true
	case rpc.SourceStatusStale:
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale, true
	case rpc.SourceStatusUnknown:
		return rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable, true
	default:
		return rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, false
	}
}

func alertShadowRegimeStagePolicy(lifecycle rpc.LifecycleState) (rpc.AlertSeverity, bool, bool) {
	severity := rpc.AlertSeverity(strings.ToLower(strings.TrimSpace(lifecycle.Severity)))
	if alertShadowSeverityRank(severity) < 0 {
		return "", false, false
	}
	switch lifecycle.Stage {
	case rpc.LifecycleEarlyWarning:
		return severity, true, severity == rpc.AlertSeverityWatch
	case rpc.LifecycleConfirmedStress:
		return severity, true, severity == rpc.AlertSeverityWatch || severity == rpc.AlertSeverityAct
	case rpc.LifecyclePanic:
		return severity, true, severity == rpc.AlertSeverityWatch || severity == rpc.AlertSeverityAct || severity == rpc.AlertSeverityUrgent
	case rpc.LifecycleQuiet:
		return severity, false, severity == rpc.AlertSeverityObserve
	case rpc.LifecycleStabilization:
		return severity, false, severity == rpc.AlertSeverityObserve
	case rpc.LifecycleOpportunity:
		return severity, false, severity == rpc.AlertSeverityWatch
	case rpc.LifecycleDataQuality:
		return severity, false, severity == rpc.AlertSeverityWatch
	default:
		return "", false, false
	}
}

func alertShadowSeverityRank(severity rpc.AlertSeverity) int {
	switch severity {
	case rpc.AlertSeverityObserve:
		return 0
	case rpc.AlertSeverityWatch:
		return 1
	case rpc.AlertSeverityAct:
		return 2
	case rpc.AlertSeverityUrgent:
		return 3
	default:
		return -1
	}
}

type alertShadowProtectionOrderFact struct {
	OrderRef       string    `json:"order_ref,omitempty"`
	Symbol         string    `json:"symbol,omitempty"`
	Action         string    `json:"action,omitempty"`
	OrderType      string    `json:"order_type,omitempty"`
	Remaining      float64   `json:"remaining,omitempty"`
	Reconciliation string    `json:"reconciliation,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
}

type alertShadowProtectionFact struct {
	Underlying string                           `json:"underlying"`
	States     []string                         `json:"states"`
	Orders     []alertShadowProtectionOrderFact `json:"orders"`
}

func alertShadowMapProtection(input alertShadowProtectionInput, observedAt time.Time) alertShadowSourceBatch {
	policyFingerprint := opaqueIdentity("alert-shadow-protection-policy", "api-order-orphan-reconcile-v2")
	evidenceAsOf := input.EvidenceAsOf.UTC()
	if evidenceAsOf.IsZero() {
		evidenceAsOf = input.AsOf.UTC()
	}
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceProtection, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonPortfolioEvidenceUnavailable,
		AuthorityUniverse: rpc.AlertAuthorityUniverseJournaledAPIOrders,
		InputAsOf:         input.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: evidenceAsOf,
		EvidenceHealth: rpc.AlertEvidenceUnavailable, FreshUntil: observedAt.UTC().Add(alertShadowProtectionSilenceHorizon),
		PolicyFingerprint: policyFingerprint, NegativeSeverity: rpc.AlertSeverityWatch,
		NegativeDestination: rpc.AlertDestinationAlerts, Scope: input.Scope, Observations: []alertEpisodeObservation{},
	}
	switch input.Status {
	case orderIntegrityHealthStale:
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusStale, alertShadowReasonPortfolioEvidenceStale, rpc.AlertEvidenceStale
		return batch
	case orderIntegrityHealthUnavailable:
		return batch
	case orderIntegrityHealthCurrent:
	case "":
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSnapshotInvalid, rpc.AlertEvidenceError
		return batch
	default:
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSnapshotInvalid, rpc.AlertEvidenceError
		return batch
	}
	if input.OrderUniverse != protectionOrderUniverseJournaledAPI || !input.OrderSnapshotComplete {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusUnavailable, alertShadowReasonSourceHealthUnavailable, rpc.AlertEvidenceUnavailable
		return batch
	}
	orderAsOf := input.OrderSnapshotAsOf.UTC()
	if orderAsOf.IsZero() || orderAsOf.After(input.AsOf.UTC()) || input.AsOf.UTC().Sub(orderAsOf) > protectionOrderSnapshotMaxAge {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusStale, alertShadowReasonSourceHealthStale, rpc.AlertEvidenceStale
		return batch
	}
	if evidenceAsOf.IsZero() || orderAsOf.Before(evidenceAsOf) {
		evidenceAsOf = orderAsOf
		batch.EvidenceAsOf = evidenceAsOf
	}
	if input.EvidenceAsOf.IsZero() || input.EvidenceAsOf.After(input.AsOf) || input.Summary.AsOf.IsZero() || input.Summary.AsOf.After(input.AsOf) {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSourceTimeInvalid, rpc.AlertEvidenceError
		return batch
	}
	facts, ok := alertShadowProtectionFacts(input.Summary)
	if !ok {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSnapshotInvalid, rpc.AlertEvidenceError
		return batch
	}
	if input.Summary.Status == rpc.ProtectionCoverageStateUnknown {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusPartial, alertShadowReasonSourceHealthIncomplete, rpc.AlertEvidencePartial
		return batch
	}
	negativeFingerprint, err := alertShadowFingerprint(struct {
		Policy string                      `json:"policy"`
		Facts  []alertShadowProtectionFact `json:"facts"`
	}{policyFingerprint, facts})
	if err != nil {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
		return batch
	}
	batch.NegativeEvidenceFingerprint = negativeFingerprint
	batch.Covered, batch.NegativeReady = true, true
	batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusCurrent, alertShadowReasonCurrent, rpc.AlertEvidenceCurrent
	for _, fact := range facts {
		episodeKey, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceProtection, rpc.AlertKindProtectionGap,
			input.Scope.account, input.Scope.mode, fact.Underlying)
		if err != nil {
			batch.Covered, batch.NegativeReady = false, false
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			return batch
		}
		evidenceFingerprint, err := alertShadowFingerprint(struct {
			Policy string                    `json:"policy"`
			Fact   alertShadowProtectionFact `json:"fact"`
		}{policyFingerprint, fact})
		if err != nil {
			batch.Covered, batch.NegativeReady = false, false
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			return batch
		}
		batch.Observations = append(batch.Observations, alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: rpc.AlertSourceProtection, Kind: rpc.AlertKindProtectionGap,
			PresentationCode: alertProtectionPresentationCode(fact), Active: true, Severity: rpc.AlertSeverityWatch,
			EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: rpc.AlertEvidenceCurrent,
			Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: evidenceAsOf, ObservedAt: observedAt.UTC(),
			PolicyFingerprint: policyFingerprint, ProducerDecisionReason: alertShadowDecisionProtectionActive,
		})
	}
	return batch
}

func alertShadowProtectionFacts(summary rpc.ProtectionCoverageSummary) ([]alertShadowProtectionFact, bool) {
	if !alertShadowProtectionSummaryValid(summary) {
		return nil, false
	}
	switch summary.Status {
	case "ok", "review", rpc.ProtectionCoverageStateReconcileRequired, rpc.ProtectionCoverageStateUnknown:
	default:
		return nil, false
	}
	byUnderlying := make(map[string]*alertShadowProtectionFact)
	for _, row := range summary.ByUnderlying {
		underlying := strings.ToUpper(strings.TrimSpace(row.Underlying))
		if underlying == "" {
			return nil, false
		}
		switch row.State {
		case rpc.ProtectionCoverageStateCovered, rpc.ProtectionCoverageStatePartial, rpc.ProtectionCoverageStateUnprotected:
			continue
		case rpc.ProtectionCoverageStateUnknown:
			if summary.Status != rpc.ProtectionCoverageStateUnknown {
				return nil, false
			}
			continue
		case rpc.ProtectionCoverageStateOrphanedOrder, rpc.ProtectionCoverageStateReconcileRequired:
		default:
			return nil, false
		}
		fact := byUnderlying[underlying]
		if fact == nil {
			fact = &alertShadowProtectionFact{Underlying: underlying, States: []string{}, Orders: []alertShadowProtectionOrderFact{}}
			byUnderlying[underlying] = fact
		}
		if !slices.Contains(fact.States, row.State) {
			fact.States = append(fact.States, row.State)
		}
		for _, order := range row.Orders {
			fact.Orders = append(fact.Orders, alertShadowProtectionOrderFactFor(order))
		}
	}
	facts := make([]alertShadowProtectionFact, 0, len(byUnderlying))
	for _, fact := range byUnderlying {
		sort.Strings(fact.States)
		sortAlertShadowProtectionOrderFacts(fact.Orders)
		facts = append(facts, *fact)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Underlying < facts[j].Underlying })
	return facts, true
}

func alertProtectionPresentationCode(fact alertShadowProtectionFact) rpc.AlertPresentationCode {
	if slices.Contains(fact.States, rpc.ProtectionCoverageStateReconcileRequired) {
		return rpc.AlertPresentationProtectionReconciliationRequired
	}
	return rpc.AlertPresentationProtectionOrphanedOrder
}

func alertShadowProtectionSummaryValid(summary rpc.ProtectionCoverageSummary) bool {
	counts := rpc.ProtectionCoverageCounts{}
	orphanedFromRows := []alertShadowProtectionOrderFact{}
	reconcileFromRows := []alertShadowProtectionOrderFact{}
	for _, row := range summary.ByUnderlying {
		if strings.TrimSpace(row.Underlying) == "" {
			return false
		}
		switch row.State {
		case rpc.ProtectionCoverageStateCovered:
			counts.Covered++
		case rpc.ProtectionCoverageStatePartial:
			counts.Partial++
		case rpc.ProtectionCoverageStateUnprotected:
			counts.Unprotected++
		case rpc.ProtectionCoverageStateOrphanedOrder:
			counts.OrphanedOrder++
			if len(row.Orders) == 0 {
				return false
			}
			for _, order := range row.Orders {
				orphanedFromRows = append(orphanedFromRows, alertShadowProtectionOrderFactFor(order))
			}
		case rpc.ProtectionCoverageStateReconcileRequired:
			counts.ReconcileRequired++
			if len(row.Orders) == 0 {
				return false
			}
			for _, order := range row.Orders {
				reconcileFromRows = append(reconcileFromRows, alertShadowProtectionOrderFactFor(order))
			}
		case rpc.ProtectionCoverageStateUnknown:
			counts.Unknown++
		case rpc.ProtectionCoverageStateNotProtectable:
			counts.NotProtectable++
		default:
			return false
		}
	}
	if counts != summary.Counts {
		return false
	}
	wantStatus := "ok"
	switch {
	case counts.Unknown > 0:
		wantStatus = rpc.ProtectionCoverageStateUnknown
	case counts.OrphanedOrder > 0 || counts.ReconcileRequired > 0:
		wantStatus = rpc.ProtectionCoverageStateReconcileRequired
	case counts.Unprotected > 0 || counts.Partial > 0:
		wantStatus = "review"
	}
	if summary.Status != wantStatus {
		return false
	}
	orphaned := make([]alertShadowProtectionOrderFact, 0, len(summary.OrphanedOrders))
	for _, order := range summary.OrphanedOrders {
		orphaned = append(orphaned, alertShadowProtectionOrderFactFor(order))
	}
	reconcile := make([]alertShadowProtectionOrderFact, 0, len(summary.ReconcileRequiredOrders))
	for _, order := range summary.ReconcileRequiredOrders {
		reconcile = append(reconcile, alertShadowProtectionOrderFactFor(order))
	}
	sortAlertShadowProtectionOrderFacts(orphanedFromRows)
	sortAlertShadowProtectionOrderFacts(orphaned)
	sortAlertShadowProtectionOrderFacts(reconcileFromRows)
	sortAlertShadowProtectionOrderFacts(reconcile)
	return slices.Equal(orphanedFromRows, orphaned) && slices.Equal(reconcileFromRows, reconcile)
}

func alertShadowProtectionOrderFactFor(order rpc.ProtectionCoverageOrder) alertShadowProtectionOrderFact {
	return alertShadowProtectionOrderFact{
		OrderRef: strings.TrimSpace(order.OrderRef), Symbol: strings.ToUpper(strings.TrimSpace(order.Symbol)),
		Action: strings.ToUpper(strings.TrimSpace(order.Action)), OrderType: strings.ToUpper(strings.TrimSpace(order.OrderType)),
		Remaining: order.Remaining, Reconciliation: strings.TrimSpace(order.ReconciliationState), UpdatedAt: order.UpdatedAt.UTC(),
	}
}

func sortAlertShadowProtectionOrderFacts(facts []alertShadowProtectionOrderFact) {
	sort.Slice(facts, func(i, j int) bool {
		left, right := facts[i], facts[j]
		if left.OrderRef != right.OrderRef {
			return left.OrderRef < right.OrderRef
		}
		if left.Symbol != right.Symbol {
			return left.Symbol < right.Symbol
		}
		if left.Action != right.Action {
			return left.Action < right.Action
		}
		if left.OrderType != right.OrderType {
			return left.OrderType < right.OrderType
		}
		if left.Remaining != right.Remaining {
			return left.Remaining < right.Remaining
		}
		if left.Reconciliation != right.Reconciliation {
			return left.Reconciliation < right.Reconciliation
		}
		return left.UpdatedAt.Before(right.UpdatedAt)
	})
}

type alertShadowDataHealthFact struct {
	Root         string    `json:"root"`
	Status       string    `json:"status"`
	Cadence      string    `json:"cadence,omitempty"`
	Code         int       `json:"code,omitempty"`
	EvidenceAsOf time.Time `json:"evidence_as_of"`
}

type alertShadowDataHealthSemanticFact struct {
	Root    string `json:"root"`
	Status  string `json:"status"`
	Cadence string `json:"cadence,omitempty"`
	Code    int    `json:"code,omitempty"`
}

func alertShadowMapDataHealth(input alertShadowDataHealthInput, observedAt time.Time) alertShadowSourceBatch {
	policyFingerprint := opaqueIdentity("alert-shadow-data-health-policy", "root-source-v1")
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceDataHealth, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: input.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: input.AsOf.UTC(),
		EvidenceHealth: rpc.AlertEvidenceUnavailable, FreshUntil: observedAt.UTC().Add(alertShadowDataHealthSilenceHorizon),
		PolicyFingerprint: policyFingerprint, NegativeSeverity: rpc.AlertSeverityWatch,
		NegativeDestination: rpc.AlertDestinationAlerts, Scope: input.Scope, Observations: []alertEpisodeObservation{},
	}
	facts, reason, valid, current := alertShadowDataHealthFacts(input)
	if !valid {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, reason, rpc.AlertEvidenceError
		return batch
	}
	if !current {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusUnavailable, reason, rpc.AlertEvidenceUnavailable
		return batch
	}
	semanticFacts := alertShadowDataHealthSemanticFacts(facts)
	negativeFingerprint, err := alertShadowFingerprint(struct {
		Policy string                              `json:"policy"`
		Facts  []alertShadowDataHealthSemanticFact `json:"facts"`
	}{policyFingerprint, semanticFacts})
	if err != nil {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
		return batch
	}
	batch.NegativeEvidenceFingerprint = negativeFingerprint
	batch.Covered, batch.NegativeReady = true, true
	batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusCurrent, alertShadowReasonCurrent, rpc.AlertEvidenceCurrent
	for i, fact := range facts {
		episodeKey, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceDataHealth, rpc.AlertKindDataHealth,
			input.Scope.account, input.Scope.mode, fact.Root)
		if err != nil {
			batch.Covered, batch.NegativeReady = false, false
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			return batch
		}
		evidenceFingerprint, err := alertShadowFingerprint(struct {
			Policy string                            `json:"policy"`
			Fact   alertShadowDataHealthSemanticFact `json:"fact"`
		}{policyFingerprint, semanticFacts[i]})
		if err != nil {
			batch.Covered, batch.NegativeReady = false, false
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			return batch
		}
		batch.Observations = append(batch.Observations, alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: rpc.AlertSourceDataHealth, Kind: rpc.AlertKindDataHealth,
			PresentationCode: alertDataHealthPresentationCode(fact.Root), Active: true, Severity: rpc.AlertSeverityWatch,
			EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: rpc.AlertEvidenceCurrent,
			Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: fact.EvidenceAsOf, ObservedAt: observedAt.UTC(),
			PolicyFingerprint: policyFingerprint, ProducerDecisionReason: alertShadowDecisionDataHealthActive,
		})
	}
	return batch
}

func alertShadowDataHealthFacts(input alertShadowDataHealthInput) ([]alertShadowDataHealthFact, string, bool, bool) {
	if input.AsOf.IsZero() {
		return nil, alertShadowReasonSourceTimeInvalid, false, false
	}
	facts := []alertShadowDataHealthFact{}
	switch input.GatewayPhase {
	case alertShadowGatewayConnecting:
		if input.Health.Connected {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		return nil, alertShadowReasonConfirmationPending, true, false
	case alertShadowGatewayFailed:
		if input.Health.Connected {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		facts = append(facts, alertShadowDataHealthFact{Root: "gateway", Status: "unavailable", EvidenceAsOf: input.AsOf.UTC()})
	case alertShadowGatewayReady:
		if !input.Health.Connected {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
	default:
		return nil, alertShadowReasonSourceHealthError, false, false
	}
	seenSubsystems := make(map[string]struct{}, len(input.Health.Subsystems))
	subsystems := make(map[string]rpc.SubsystemHealth, len(input.Health.Subsystems))
	storageSeen := false
	for _, subsystem := range input.Health.Subsystems {
		name := strings.ToLower(strings.TrimSpace(subsystem.Name))
		status := strings.ToLower(strings.TrimSpace(subsystem.Status))
		if name == "" || status == "" {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		if _, duplicate := seenSubsystems[name]; duplicate {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		seenSubsystems[name] = struct{}{}
		subsystems[name] = subsystem
		if !subsystem.LastErrorAt.IsZero() && subsystem.LastErrorAt.After(input.AsOf) {
			return nil, alertShadowReasonSourceTimeInvalid, false, false
		}
		if name == "storage" {
			storageSeen = true
		}
		// v1 keeps only independent root capabilities here. Gateway-derived
		// quote/history/chain rows and normal computing/disabled work would
		// multiply one outage into several noisy episodes.
		if name != "storage" && name != "proposals" && name != "opportunities" {
			continue
		}
		switch status {
		case "ready":
			continue
		case "computing", "disabled":
			if name != "storage" {
				continue
			}
			return nil, alertShadowReasonSourceHealthError, false, false
		case "degraded", "unavailable", "error":
			evidenceAt := subsystem.LastErrorAt.UTC()
			if evidenceAt.IsZero() {
				evidenceAt = input.AsOf.UTC()
			}
			facts = append(facts, alertShadowDataHealthFact{Root: "subsystem:" + name, Status: status, EvidenceAsOf: evidenceAt})
		default:
			return nil, alertShadowReasonSourceHealthError, false, false
		}
	}
	if !storageSeen {
		return nil, alertShadowReasonSourceHealthMissing, false, false
	}
	for _, expected := range []struct {
		name    string
		enabled bool
	}{{"proposals", input.ProposalsExpected}, {"opportunities", input.OpportunitiesExpected}} {
		if !expected.enabled {
			continue
		}
		row, ok := subsystems[expected.name]
		if !ok {
			facts = append(facts, alertShadowDataHealthFact{Root: "subsystem:" + expected.name, Status: "unavailable", EvidenceAsOf: input.AsOf.UTC()})
			continue
		}
		if strings.EqualFold(strings.TrimSpace(row.Status), "disabled") {
			facts = append(facts, alertShadowDataHealthFact{Root: "subsystem:" + expected.name, Status: "unavailable", EvidenceAsOf: input.AsOf.UTC()})
		}
	}
	if input.Health.Connected {
		farmStatus, farmEvidenceAt, ok := alertShadowDataFarmAggregate(input, subsystems)
		if !ok {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		if farmStatus != "ready" {
			facts = append(facts, alertShadowDataHealthFact{Root: "data_farms", Status: farmStatus, EvidenceAsOf: farmEvidenceAt})
		}
	}
	seenQuality := make(map[string]struct{}, len(input.Health.DataQuality))
	for _, quality := range input.Health.DataQuality {
		surface := strings.ToLower(strings.TrimSpace(quality.Surface))
		status := strings.ToLower(strings.TrimSpace(quality.Status))
		cadence := strings.ToLower(strings.TrimSpace(quality.CadenceState))
		if surface == "" || status == "" {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		if _, duplicate := seenQuality[surface]; duplicate {
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		seenQuality[surface] = struct{}{}
		if !quality.AsOf.IsZero() && quality.AsOf.After(input.AsOf) {
			return nil, alertShadowReasonSourceTimeInvalid, false, false
		}
		switch cadence {
		case "", rpc.DataCadenceCurrent, rpc.DataCadenceNotDue, rpc.DataCadenceMissedSession, rpc.DataCadenceNoLastGood, rpc.DataCadenceUnknown:
		default:
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		switch status {
		case "ok", "ready", "partial", "stale", "degraded", "unavailable", "error":
		default:
			return nil, alertShadowReasonSourceHealthError, false, false
		}
		if cadence == rpc.DataCadenceNotDue && (status == "ok" || status == "ready" || status == "partial" || status == "stale" || status == "degraded") {
			continue
		}
		if (status == "ok" || status == "ready") && (cadence == "" || cadence == rpc.DataCadenceCurrent) {
			continue
		}
		evidenceAt := quality.AsOf.UTC()
		if evidenceAt.IsZero() {
			evidenceAt = input.AsOf.UTC()
		}
		facts = append(facts, alertShadowDataHealthFact{Root: "quality:" + surface, Status: status, Cadence: cadence, EvidenceAsOf: evidenceAt})
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Root < facts[j].Root })
	return facts, alertShadowReasonCurrent, true, true
}

func alertShadowDataFarmAggregate(input alertShadowDataHealthInput, subsystems map[string]rpc.SubsystemHealth) (string, time.Time, bool) {
	status := "ready"
	evidenceAt := input.AsOf.UTC()
	for _, name := range []string{"quote", "history", "chain"} {
		subsystem, ok := subsystems[name]
		if !ok {
			return "", time.Time{}, false
		}
		rowStatus := strings.ToLower(strings.TrimSpace(subsystem.Status))
		switch rowStatus {
		case "ready":
		case "degraded":
			if status == "ready" {
				status = "degraded"
			}
		case "unavailable", "error":
			status = "unavailable"
		default:
			return "", time.Time{}, false
		}
		if !subsystem.LastErrorAt.IsZero() && subsystem.LastErrorAt.Before(evidenceAt) {
			evidenceAt = subsystem.LastErrorAt.UTC()
		}
	}
	for _, farm := range input.Health.DataFarms {
		farmType := strings.ToLower(strings.TrimSpace(farm.Type))
		farmStatus := strings.ToLower(strings.TrimSpace(farm.Status))
		switch farmType {
		case "market", "historical", "security_definition", "connectivity":
		default:
			return "", time.Time{}, false
		}
		if farmStatus != "disconnected" && farmStatus != "broken" {
			return "", time.Time{}, false
		}
		if !farm.AsOf.IsZero() && farm.AsOf.After(input.AsOf) {
			return "", time.Time{}, false
		}
		if !farm.AsOf.IsZero() && farm.AsOf.Before(evidenceAt) {
			evidenceAt = farm.AsOf.UTC()
		}
		if farmStatus == "broken" {
			status = "unavailable"
		} else if status == "ready" {
			status = "degraded"
		}
	}
	return status, evidenceAt, true
}

func alertShadowDataHealthSemanticFacts(facts []alertShadowDataHealthFact) []alertShadowDataHealthSemanticFact {
	out := make([]alertShadowDataHealthSemanticFact, 0, len(facts))
	for _, fact := range facts {
		out = append(out, alertShadowDataHealthSemanticFact{Root: fact.Root, Status: fact.Status, Cadence: fact.Cadence, Code: fact.Code})
	}
	return out
}

func alertDataHealthPresentationCode(root string) rpc.AlertPresentationCode {
	switch root {
	case "gateway":
		return rpc.AlertPresentationDataHealthGateway
	case "subsystem:storage":
		return rpc.AlertPresentationDataHealthStorage
	case "subsystem:proposals":
		return rpc.AlertPresentationDataHealthProposals
	case "subsystem:opportunities":
		return rpc.AlertPresentationDataHealthOpportunities
	case "data_farms":
		return rpc.AlertPresentationDataHealthDataFarms
	case "quality:regime":
		return rpc.AlertPresentationDataHealthRegime
	case "quality:gamma":
		return rpc.AlertPresentationDataHealthGamma
	default:
		return rpc.AlertPresentationDataHealthQuality
	}
}

func alertShadowMapStress(scope alertShadowBrokerScope, result rpc.StressResult, observedAt time.Time) alertShadowSourceBatch {
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceStress, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: result.AsOf.UTC(),
		EvidenceHealth: rpc.AlertEvidenceUnavailable, PolicyFingerprint: result.PolicyFingerprint.Key,
		NegativeEvidenceFingerprint: result.Fingerprint.Key, NegativeDestination: rpc.AlertDestinationAlerts,
		FreshUntil:   alertShadowStressFreshUntil(result, observedAt),
		Scope:        scope,
		Observations: []alertEpisodeObservation{},
	}
	covered, health, reason, evidenceAsOf := alertShadowStressHealth(result)
	batch.Covered = covered
	batch.EvidenceHealth = health
	batch.Reason = reason
	if !evidenceAsOf.IsZero() {
		batch.EvidenceAsOf = evidenceAsOf
	}
	batch.Status = alertShadowStatusForEvidence(health)
	if covered {
		batch.Status = alertShadowStatusCurrent
		batch.Reason = alertShadowReasonCurrent
	}
	if result.Fingerprint.Version != rpc.StressFingerprintVersion || !validAlertRegistryFingerprint(result.Fingerprint.Key) {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonEvidenceFingerprintInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	if result.PolicyFingerprint.Version != risk.StressPolicyFingerprintVersion || !validAlertRegistryFingerprint(result.PolicyFingerprint.Key) {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonPolicyFingerprintInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	severity, ok := alertShadowStressSeverity(result.Severity)
	if !ok {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonCandidateInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	batch.NegativeSeverity = severity
	batch.NegativeReady = !batch.EvidenceAsOf.IsZero()
	if !alertShadowStressOccurrenceEligible(result) {
		return batch
	}
	episodeKey, err := rpc.BuildAlertEpisodeKey(
		rpc.AlertSourceStress, rpc.AlertKindPortfolioRisk,
		scope.account, scope.mode, stressEpisodeIdentity,
	)
	if err != nil {
		batch.Covered = false
		batch.NegativeReady = false
		batch.Status = alertShadowStatusError
		batch.Reason = alertShadowReasonCandidateInvalid
		batch.EvidenceHealth = rpc.AlertEvidenceError
		return batch
	}
	batch.Observations = append(batch.Observations, alertEpisodeObservation{
		EpisodeKey: episodeKey, Source: rpc.AlertSourceStress, Kind: rpc.AlertKindPortfolioRisk,
		PresentationCode: rpc.AlertPresentationPortfolioStress, Active: true, Severity: severity,
		EvidenceFingerprint: result.Fingerprint.Key, EvidenceHealth: batch.EvidenceHealth,
		Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: batch.EvidenceAsOf, ObservedAt: observedAt.UTC(),
		PolicyFingerprint: result.PolicyFingerprint.Key, ProducerDecisionReason: alertShadowDecisionLegacyGateActive,
	})
	return batch
}

func alertShadowStressFreshUntil(result rpc.StressResult, observedAt time.Time) time.Time {
	deadline := observedAt.UTC().Add(alertShadowStressSilenceHorizon)
	for _, source := range result.SourceHealth {
		if source.AsOf.IsZero() || source.MaxAgeSeconds <= 0 {
			continue
		}
		candidate := source.AsOf.UTC().Add(time.Duration(source.MaxAgeSeconds) * time.Second)
		if candidate.Before(deadline) {
			deadline = candidate
		}
	}
	if deadline.Before(observedAt) {
		return observedAt.UTC()
	}
	return deadline
}

func alertShadowStressHealth(result rpc.StressResult) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	if result.PortfolioAlertRelevant == nil {
		return false, rpc.AlertEvidencePartial, alertShadowReasonMissingRelevanceStamp, alertShadowOldestStressSourceTime(result)
	}
	if strings.TrimSpace(result.InputHealth) != "ok" {
		return false, rpc.AlertEvidencePartial, alertShadowReasonInputHealthNotOK, alertShadowOldestStressSourceTime(result)
	}
	if len(result.SourceHealth) == 0 {
		return false, rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthMissing, result.AsOf.UTC()
	}
	required := map[string]bool{"account": false, "positions": false, "regime": false}
	seen := make(map[string]struct{}, len(result.SourceHealth))
	evidenceAsOf := time.Time{}
	worstHealth := rpc.AlertEvidenceCurrent
	worstReason := alertShadowReasonCurrent
	for _, source := range result.SourceHealth {
		name := strings.TrimSpace(source.Source)
		if _, duplicate := seen[name]; duplicate || name == "" {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, alertShadowOldestStressSourceTime(result)
		}
		seen[name] = struct{}{}
		if _, ok := required[name]; ok {
			required[name] = true
		} else if name != "market_events" {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError, alertShadowOldestStressSourceTime(result)
		}
		if source.AsOf.IsZero() || source.AsOf.After(result.AsOf) {
			return false, rpc.AlertEvidenceError, alertShadowReasonSourceTimeInvalid, alertShadowOldestStressSourceTime(result)
		}
		if source.Fingerprint == nil || strings.TrimSpace(source.Fingerprint.Version) == "" || !validAlertRegistryFingerprint(source.Fingerprint.Key) {
			return false, rpc.AlertEvidenceError, alertShadowReasonEvidenceFingerprintInvalid, alertShadowOldestStressSourceTime(result)
		}
		if evidenceAsOf.IsZero() || source.AsOf.Before(evidenceAsOf) {
			evidenceAsOf = source.AsOf.UTC()
		}
		rowHealth, rowReason := alertShadowStressSourceHealth(source)
		if alertShadowEvidenceRank(rowHealth) > alertShadowEvidenceRank(worstHealth) {
			worstHealth, worstReason = rowHealth, rowReason
		}
	}
	for _, present := range required {
		if !present {
			return false, rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete, evidenceAsOf
		}
	}
	if worstHealth != rpc.AlertEvidenceCurrent {
		return false, worstHealth, worstReason, evidenceAsOf
	}
	return true, rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, evidenceAsOf
}

func alertShadowStressSourceHealth(source rpc.SourceHealth) (rpc.AlertEvidenceHealth, string) {
	if source.MaxAgeSeconds > 0 && source.AgeSeconds > source.MaxAgeSeconds {
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
	}
	switch source.Status {
	case rpc.SourceStatusOK:
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	case rpc.SourceStatusStale:
		return rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
	case rpc.SourceStatusPartial, rpc.SourceStatusDegraded:
		return rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
	case rpc.SourceStatusUnknown:
		return rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable
	default:
		return rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
	}
}

func alertShadowOldestStressSourceTime(result rpc.StressResult) time.Time {
	oldest := time.Time{}
	for _, source := range result.SourceHealth {
		if source.AsOf.IsZero() {
			continue
		}
		if oldest.IsZero() || source.AsOf.Before(oldest) {
			oldest = source.AsOf.UTC()
		}
	}
	if oldest.IsZero() {
		return result.AsOf.UTC()
	}
	return oldest
}

func alertShadowStressOccurrenceEligible(result rpc.StressResult) bool {
	// This is intentionally identical to internal/app/alerts' approved legacy
	// occurrence policy. Nil relevance fails open for the positive gate only.
	relevant := result.PortfolioAlertRelevant == nil || *result.PortfolioAlertRelevant
	return relevant && (alertShadowStressSeverityAtLeast(result.Severity, risk.SeverityWatch) ||
		alertShadowStressSeverityAtLeast(result.Severity, risk.SeverityAct) ||
		result.Action == "defend" || result.Action == "rebalance" || result.Action == "confirm_inputs")
}

func alertShadowStressSeverityAtLeast(got, want risk.SignalSeverity) bool {
	rank := map[risk.SignalSeverity]int{
		risk.SeverityObserve: 0, risk.SeverityWatch: 1, risk.SeverityAct: 2, risk.SeverityUrgent: 3,
	}
	return rank[got] >= rank[want]
}

func alertShadowStressSeverity(severity risk.SignalSeverity) (rpc.AlertSeverity, bool) {
	switch severity {
	case risk.SeverityObserve:
		return rpc.AlertSeverityObserve, true
	case risk.SeverityWatch:
		return rpc.AlertSeverityWatch, true
	case risk.SeverityAct:
		return rpc.AlertSeverityAct, true
	case risk.SeverityUrgent:
		return rpc.AlertSeverityUrgent, true
	default:
		return "", false
	}
}

func alertShadowMapRulebook(scope alertShadowBrokerScope, result rpc.RulesResult, observedAt time.Time) alertShadowSourceBatch {
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceRulebook, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: result.AsOf.UTC(),
		EvidenceHealth: rpc.AlertEvidenceUnavailable, FreshUntil: observedAt.UTC().Add(alertShadowRulebookSilenceHorizon),
		Scope: scope, NegativeDestination: rpc.AlertDestinationMonitor, NegativeSeverity: rpc.AlertSeverityWatch,
		Observations: []alertEpisodeObservation{},
	}
	if !result.Enabled || result.Status == "disabled" {
		batch.Reason = alertShadowReasonSourceDisabled
		return batch
	}
	if result.PolicyFingerprint == nil || result.PolicyFingerprint.Version != rpc.RulebookPolicyFingerprintVersion ||
		!validAlertRegistryFingerprint(result.PolicyFingerprint.Key) {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonPolicyFingerprintInvalid, rpc.AlertEvidenceError
		return batch
	}
	batch.PolicyFingerprint = result.PolicyFingerprint.Key
	batch.NegativeReady = true
	covered := result.Status == "ok"
	worst := rpc.AlertEvidenceCurrent
	reason := alertShadowReasonCurrent
	if result.Status != "ok" && result.Status != "degraded" {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSnapshotInvalid, rpc.AlertEvidenceError
		return batch
	}
	if result.Status == "degraded" {
		covered, worst, reason = false, rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
	}
	required := make(map[string]bool, len(alertShadowCanonicalRulebookHealth))
	for _, source := range alertShadowCanonicalRulebookHealth {
		required[source] = false
	}
	accountHealthStatus := ""
	seenHealth := make(map[string]struct{}, len(result.InputHealth))
	for _, health := range result.InputHealth {
		name := strings.TrimSpace(health.Source)
		if name == "" || name != health.Source {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
			continue
		}
		if _, duplicate := seenHealth[name]; duplicate {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
			continue
		}
		seenHealth[name] = struct{}{}
		if _, ok := required[name]; !ok {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
			continue
		}
		required[name] = true
		healthStatus := strings.ToLower(strings.TrimSpace(health.Status))
		if name == "account" {
			accountHealthStatus = healthStatus
		}
		switch healthStatus {
		case rpc.SourceStatusOK:
			if health.AsOf.IsZero() || health.AsOf.After(result.AsOf) {
				covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonSourceTimeInvalid
				continue
			}
			if health.MaxAgeSeconds > 0 {
				expiresAt := health.AsOf.UTC().Add(time.Duration(health.MaxAgeSeconds) * time.Second)
				if expiresAt.Before(batch.FreshUntil) {
					batch.FreshUntil = expiresAt
				}
				if observedAt.UTC().After(expiresAt) {
					covered = false
					if alertShadowEvidenceRank(rpc.AlertEvidenceStale) > alertShadowEvidenceRank(worst) {
						worst, reason = rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
					}
				}
			}
		case rpc.SourceStatusStale:
			covered = false
			if alertShadowEvidenceRank(rpc.AlertEvidenceStale) > alertShadowEvidenceRank(worst) {
				worst, reason = rpc.AlertEvidenceStale, alertShadowReasonSourceHealthStale
			}
		case rpc.SourceStatusPartial, rpc.SourceStatusDegraded, "pending":
			covered = false
			if alertShadowEvidenceRank(rpc.AlertEvidencePartial) > alertShadowEvidenceRank(worst) {
				worst, reason = rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
			}
		case rpc.SourceStatusUnknown, "unavailable":
			covered = false
			if alertShadowEvidenceRank(rpc.AlertEvidenceUnavailable) > alertShadowEvidenceRank(worst) {
				worst, reason = rpc.AlertEvidenceUnavailable, alertShadowReasonSourceHealthUnavailable
			}
		default:
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonSourceHealthError
		}
	}
	for _, present := range required {
		if !present {
			covered = false
			if alertShadowEvidenceRank(rpc.AlertEvidencePartial) > alertShadowEvidenceRank(worst) {
				worst, reason = rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
			}
		}
	}
	seenRules := make(map[string]struct{}, len(result.Rules))
	for _, row := range result.Rules {
		id := strings.TrimSpace(row.ID)
		number, canonical := alertShadowCanonicalRulebookRow(id)
		if id == "" || id != row.ID || !canonical || row.Number != number {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			continue
		}
		if _, duplicate := seenRules[id]; duplicate {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			continue
		}
		seenRules[id] = struct{}{}
		var severity rpc.AlertSeverity
		switch row.Status {
		case risk.RuleStatusWatch:
			severity = rpc.AlertSeverityWatch
		case risk.RuleStatusAct:
			severity = rpc.AlertSeverityAct
		case risk.RuleStatusUnknown:
			covered = false
			if alertShadowEvidenceRank(rpc.AlertEvidencePartial) > alertShadowEvidenceRank(worst) {
				worst, reason = rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
			}
			continue
		case risk.RuleStatusPass, risk.RuleStatusInfo:
			continue
		case risk.RuleStatusNotEvaluated:
			if !alertShadowRulebookSafeNotEvaluated(row, result, accountHealthStatus) {
				covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			}
			continue
		default:
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			continue
		}
		episodeKey, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceRulebook, rpc.AlertKindGovernance, scope.account, scope.mode, id)
		if err != nil {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			continue
		}
		evidenceFingerprint, err := alertShadowFingerprint(struct {
			Policy string       `json:"policy"`
			Rule   risk.RuleRow `json:"rule"`
		}{result.PolicyFingerprint.Key, row})
		if err != nil {
			covered, worst, reason = false, rpc.AlertEvidenceError, alertShadowReasonCandidateInvalid
			continue
		}
		batch.Observations = append(batch.Observations, alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: rpc.AlertSourceRulebook, Kind: rpc.AlertKindGovernance,
			PresentationCode: alertRulebookPresentationCode(id), Active: true, Severity: severity,
			EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: worst, Destination: rpc.AlertDestinationMonitor,
			EvidenceAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), PolicyFingerprint: result.PolicyFingerprint.Key,
			ProducerDecisionReason: alertShadowDecisionRulebookActive,
		})
	}
	if len(seenRules) != len(alertShadowCanonicalRulebookRows) {
		covered = false
		if alertShadowEvidenceRank(rpc.AlertEvidencePartial) > alertShadowEvidenceRank(worst) {
			worst, reason = rpc.AlertEvidencePartial, alertShadowReasonSourceHealthIncomplete
		}
	}
	batch.Covered, batch.EvidenceHealth, batch.Reason = covered && worst == rpc.AlertEvidenceCurrent, worst, reason
	batch.Status = alertShadowStatusForEvidence(worst)
	if batch.Covered {
		batch.Status, batch.Reason = alertShadowStatusCurrent, alertShadowReasonCurrent
	}
	for i := range batch.Observations {
		batch.Observations[i].EvidenceHealth = batch.EvidenceHealth
	}
	sort.Slice(batch.Observations, func(i, j int) bool { return batch.Observations[i].EpisodeKey < batch.Observations[j].EpisodeKey })
	return batch
}

func alertShadowCanonicalRulebookRow(id string) (int, bool) {
	for _, row := range alertShadowCanonicalRulebookRows {
		if row.ID == id {
			return row.Number, true
		}
	}
	return 0, false
}

func alertRulebookPresentationCode(id string) rpc.AlertPresentationCode {
	switch id {
	case risk.RuleSingleNameExposure:
		return rpc.AlertPresentationRulebookSingleNameExposure
	case risk.RuleOptionLinePremium:
		return rpc.AlertPresentationRulebookOptionLinePremium
	case risk.RuleCashSellOnly:
		return rpc.AlertPresentationRulebookCashSellOnly
	case risk.RuleExtrinsicBudget:
		return rpc.AlertPresentationRulebookExtrinsicBudget
	case risk.RuleExpiryRunway:
		return rpc.AlertPresentationRulebookExpiryRunway
	case risk.RuleCatalystCoverage:
		return rpc.AlertPresentationRulebookCatalystCoverage
	case risk.RuleOverwriteEarnings:
		return rpc.AlertPresentationRulebookOverwriteEarnings
	case risk.RuleEarningsSizeFreeze:
		return rpc.AlertPresentationRulebookEarningsSizeFreeze
	case risk.RuleRedOnGreen:
		return rpc.AlertPresentationRulebookRedOnGreen
	case risk.RuleWinnerTrim:
		return rpc.AlertPresentationRulebookWinnerTrim
	case risk.RuleGreenDayAction:
		return rpc.AlertPresentationRulebookGreenDayAction
	case risk.RuleHedgeIntegrity:
		return rpc.AlertPresentationRulebookHedgeIntegrity
	case risk.RuleExitDiscipline:
		return rpc.AlertPresentationRulebookExitDiscipline
	case risk.RuleFXExposure:
		return rpc.AlertPresentationRulebookFXExposure
	default:
		return ""
	}
}

func alertShadowRulebookSafeNotEvaluated(row risk.RuleRow, result rpc.RulesResult, accountHealthStatus string) bool {
	switch row.ID {
	case risk.RuleCatalystCoverage, risk.RuleOverwriteEarnings, risk.RuleEarningsSizeFreeze:
		return alertShadowRulebookSafeEarningsNotEvaluated(row, result)
	case risk.RuleRedOnGreen, risk.RuleWinnerTrim:
		return row.Reason == risk.RuleReasonOffSession
	case risk.RuleGreenDayAction:
		return row.Reason == risk.RuleReasonPnLUnavailable &&
			result.Status == "degraded" && accountHealthStatus == rpc.SourceStatusDegraded
	case risk.RuleHedgeIntegrity:
		return row.Reason == risk.RuleReasonNoLongBook
	default:
		return false
	}
}

type alertShadowEarningsAuthority uint8

const (
	alertShadowEarningsAuthorityInvalid alertShadowEarningsAuthority = iota
	alertShadowEarningsAuthorityTerminal
	alertShadowEarningsAuthorityBroker
)

// alertShadowRulebookSafeEarningsNotEvaluated verifies the same-result typed
// authority behind every exempt name. A row reason is disclosure, not proof,
// and cannot by itself recover an active alert episode.
func alertShadowRulebookSafeEarningsNotEvaluated(row risk.RuleRow, result rpc.RulesResult) bool {
	if result.AsOf.IsZero() || len(row.Exempt) == 0 || len(row.Offenders) != 0 {
		return false
	}
	infos := make(map[string]rpc.EarningsInfo, len(result.Earnings))
	duplicates := make(map[string]struct{})
	for _, info := range result.Earnings {
		symbol := strings.TrimSpace(info.Symbol)
		if symbol == "" || symbol != info.Symbol {
			continue
		}
		if _, exists := infos[symbol]; exists {
			duplicates[symbol] = struct{}{}
			continue
		}
		infos[symbol] = info
	}

	seen := make(map[string]struct{}, len(row.Exempt))
	seenBrokerReceipts := make(map[string]struct{})
	seenTerminalContracts := make(map[int]struct{})
	hasTerminal, hasBroker := false, false
	for _, exempt := range row.Exempt {
		symbol := strings.TrimSpace(exempt.Symbol)
		if symbol == "" || symbol != exempt.Symbol {
			return false
		}
		if _, duplicate := seen[symbol]; duplicate {
			return false
		}
		seen[symbol] = struct{}{}
		if _, duplicate := duplicates[symbol]; duplicate {
			return false
		}
		info, found := infos[symbol]
		if !found {
			return false
		}
		authority := alertShadowRulebookEarningsAuthorityFor(info, result.AsOf.UTC())
		if authority == alertShadowEarningsAuthorityBroker {
			if _, duplicate := seenBrokerReceipts[info.Identity.ObservationID]; duplicate {
				return false
			}
			seenBrokerReceipts[info.Identity.ObservationID] = struct{}{}
		} else if authority == alertShadowEarningsAuthorityTerminal {
			if _, duplicate := seenTerminalContracts[info.Terminal.ContractConID]; duplicate {
				return false
			}
			seenTerminalContracts[info.Terminal.ContractConID] = struct{}{}
		}
		switch row.Reason {
		case risk.EarningsReasonTerminalNonReporting:
			if authority != alertShadowEarningsAuthorityTerminal {
				return false
			}
		case risk.EarningsReasonBrokerNonIssuer:
			if authority != alertShadowEarningsAuthorityBroker {
				return false
			}
		case risk.EarningsReasonNotApplicable:
			if authority != alertShadowEarningsAuthorityTerminal && authority != alertShadowEarningsAuthorityBroker {
				return false
			}
		default:
			return false
		}
		hasTerminal = hasTerminal || authority == alertShadowEarningsAuthorityTerminal
		hasBroker = hasBroker || authority == alertShadowEarningsAuthorityBroker
	}

	switch row.Reason {
	case risk.EarningsReasonTerminalNonReporting:
		return hasTerminal && !hasBroker
	case risk.EarningsReasonBrokerNonIssuer:
		return hasBroker && !hasTerminal
	case risk.EarningsReasonNotApplicable:
		return hasTerminal && hasBroker
	default:
		return false
	}
}

func alertShadowRulebookEarningsAuthorityFor(info rpc.EarningsInfo, asOf time.Time) alertShadowEarningsAuthority {
	if info.Stale || asOf.IsZero() {
		return alertShadowEarningsAuthorityInvalid
	}
	if validRulebookTerminalEarningsAuthority(info, asOf) {
		return alertShadowEarningsAuthorityTerminal
	}
	if validRulebookBrokerEarningsAuthority(info, asOf) {
		return alertShadowEarningsAuthorityBroker
	}
	return alertShadowEarningsAuthorityInvalid
}

func alertShadowMapOrderIntegrity(scope alertShadowBrokerScope, input orderIntegrityEvaluation, observedAt time.Time, previous map[string]struct{}) (alertShadowSourceBatch, map[string]struct{}) {
	evidenceAsOf := input.EvidenceAsOf.UTC()
	if evidenceAsOf.IsZero() {
		evidenceAsOf = input.AsOf.UTC()
	}
	policyFingerprint := opaqueIdentity("order-integrity-policy", "order-integrity-classifier-v1")
	batch := alertShadowSourceBatch{
		Source: rpc.AlertSourceOrderIntegrity, Status: alertShadowStatusUnavailable, Reason: alertShadowReasonSourceHealthUnavailable,
		InputAsOf: input.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: evidenceAsOf,
		EvidenceHealth: rpc.AlertEvidenceUnavailable, FreshUntil: observedAt.UTC().Add(alertShadowOrderSilenceHorizon),
		PolicyFingerprint: policyFingerprint, NegativeSeverity: rpc.AlertSeverityUrgent,
		NegativeDestination: rpc.AlertDestinationAlerts, Scope: scope, Observations: []alertEpisodeObservation{},
	}
	next := make(map[string]struct{})
	switch input.Status {
	case orderIntegrityHealthStale:
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusStale, alertShadowReasonSourceHealthStale, rpc.AlertEvidenceStale
		return batch, next
	case orderIntegrityHealthUnavailable:
		return batch, next
	case orderIntegrityHealthCurrent:
		if input.EvidenceAsOf.IsZero() || input.EvidenceAsOf.After(input.AsOf) {
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSourceTimeInvalid, rpc.AlertEvidenceError
			return batch, next
		}
	default:
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonSnapshotInvalid, rpc.AlertEvidenceError
		return batch, next
	}

	pending := false
	for _, order := range input.Orders {
		if !order.Open || order.ReconciliationKind == "" {
			continue
		}
		routeKey := orderViewKey(order)
		if routeKey == "" || order.ReconciliationSeverity != rpc.OrderReconciliationSeverityCritical {
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			batch.Covered = false
			return batch, make(map[string]struct{})
		}
		confirmationKey := opaqueIdentity("order-integrity-confirmation", routeKey, order.ReconciliationKind,
			strconv.FormatFloat(order.Remaining, 'g', -1, 64), strconv.FormatFloat(order.ReduceToQuantity, 'g', -1, 64))
		next[confirmationKey] = struct{}{}
		if _, confirmed := previous[confirmationKey]; !confirmed {
			pending = true
			continue
		}
		episodeKey, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceOrderIntegrity, rpc.AlertKindOrderIntegrity,
			scope.account, scope.mode, routeKey)
		if err != nil {
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			batch.Covered = false
			return batch, make(map[string]struct{})
		}
		evidenceFingerprint, err := alertShadowFingerprint(struct {
			Route       string    `json:"route"`
			Kind        string    `json:"kind"`
			Severity    string    `json:"severity"`
			Remaining   float64   `json:"remaining"`
			ShortRisk   float64   `json:"short_risk"`
			ReduceTo    float64   `json:"reduce_to"`
			BrokerTruth time.Time `json:"broker_truth"`
		}{routeKey, order.ReconciliationKind, order.ReconciliationSeverity, order.Remaining,
			order.ShortRiskQuantity, order.ReduceToQuantity, order.BrokerTruthAsOf.UTC()})
		if err != nil {
			batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
			batch.Covered = false
			return batch, make(map[string]struct{})
		}
		batch.Observations = append(batch.Observations, alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: rpc.AlertSourceOrderIntegrity, Kind: rpc.AlertKindOrderIntegrity,
			PresentationCode: rpc.AlertPresentationOrderIntegrityMismatch, Active: true, Severity: rpc.AlertSeverityUrgent,
			EvidenceFingerprint: evidenceFingerprint, EvidenceHealth: rpc.AlertEvidenceCurrent,
			Destination: rpc.AlertDestinationAlerts, EvidenceAsOf: evidenceAsOf, ObservedAt: observedAt.UTC(),
			PolicyFingerprint: policyFingerprint, ProducerDecisionReason: alertShadowDecisionOrderIntegrityActive,
		})
	}
	negativeFingerprint, err := alertShadowFingerprint(struct {
		Policy string   `json:"policy"`
		Open   []string `json:"open_confirmations"`
	}{policyFingerprint, sortedAlertShadowKeys(next)})
	if err != nil {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusError, alertShadowReasonCandidateInvalid, rpc.AlertEvidenceError
		return batch, make(map[string]struct{})
	}
	batch.NegativeEvidenceFingerprint = negativeFingerprint
	if pending {
		batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusPartial, alertShadowReasonConfirmationPending, rpc.AlertEvidencePartial
		for i := range batch.Observations {
			batch.Observations[i].EvidenceHealth = rpc.AlertEvidencePartial
		}
		return batch, next
	}
	batch.Covered, batch.NegativeReady = true, true
	batch.Status, batch.Reason, batch.EvidenceHealth = alertShadowStatusCurrent, alertShadowReasonCurrent, rpc.AlertEvidenceCurrent
	sort.Slice(batch.Observations, func(i, j int) bool { return batch.Observations[i].EpisodeKey < batch.Observations[j].EpisodeKey })
	return batch, next
}

func sortedAlertShadowKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func alertShadowMapNudges(input alertShadowNudgeInput, observedAt time.Time) (map[rpc.AlertSource]alertShadowSourceBatch, map[rpc.AlertSource]uint64, error) {
	result := input.Snapshot
	policyFingerprint := input.PolicyFingerprint
	batches := make(map[rpc.AlertSource]alertShadowSourceBatch, len(alertShadowNudgeSources))
	duplicates := make(map[rpc.AlertSource]uint64, len(alertShadowNudgeSources))
	health := rpc.NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	validSnapshot := true
	if _, err := json.Marshal(result); err != nil {
		validSnapshot = false
	}
	policyValid := policyFingerprint.Version == rpc.RiskConstitutionFingerprintVersion && validAlertRegistryFingerprint(policyFingerprint.Key)
	storeValid, storeReason := alertShadowNudgeStoreHealthValid(input.StoreHealth, result.AsOf)
	for _, source := range alertShadowNudgeSources {
		covered, evidenceHealth, reason, evidenceAsOf := alertShadowNudgeHealth(source, result.AsOf, health, input.StoreHealth)
		batch := alertShadowSourceBatch{
			Source: source, Status: alertShadowStatusForEvidence(evidenceHealth), Reason: reason,
			InputAsOf: result.AsOf.UTC(), ObservedAt: observedAt.UTC(), EvidenceAsOf: evidenceAsOf,
			EvidenceHealth: evidenceHealth, PolicyFingerprint: policyFingerprint.Key,
			FreshUntil: observedAt.UTC().Add(alertShadowNudgeSilenceHorizon),
			Scope:      input.Scope,
			Covered:    covered, NegativeReady: policyValid && !evidenceAsOf.IsZero(), Observations: []alertEpisodeObservation{},
		}
		if covered {
			batch.Status = alertShadowStatusCurrent
			batch.Reason = alertShadowReasonCurrent
		}
		if !policyValid {
			batch.Covered = false
			batch.NegativeReady = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonPolicyFingerprintInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		if !validSnapshot {
			batch.Covered = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonSnapshotInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		if !storeValid {
			batch.Covered = false
			batch.NegativeReady = false
			batch.Status = alertShadowStatusError
			batch.Reason = storeReason
			batch.EvidenceHealth = rpc.AlertEvidenceError
		}
		batches[source] = batch
	}
	if !validSnapshot || !policyValid {
		return batches, duplicates, nil
	}

	seen := make(map[string]alertEpisodeObservation)
	invalidSources := make(map[rpc.AlertSource]bool, len(alertShadowNudgeSources))
	for _, wireCandidate := range result.Candidates {
		canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
			Fingerprint: wireCandidate.Fingerprint, Kind: wireCandidate.Kind, State: wireCandidate.State,
			Severity: wireCandidate.Severity, Title: wireCandidate.Title, Body: wireCandidate.Body,
			OccurredAt: wireCandidate.OccurredAt, DueAt: wireCandidate.DueAt,
			ExpiresAt: wireCandidate.ExpiresAt, Destination: wireCandidate.Destination,
		})
		if err != nil || canonical.OccurredAt.After(result.AsOf) {
			for _, source := range alertShadowNudgeSources {
				batch := batches[source]
				batch.Covered = false
				batch.Status = alertShadowStatusError
				batch.Reason = alertShadowReasonCandidateInvalid
				batch.EvidenceHealth = rpc.AlertEvidenceError
				batches[source] = batch
				invalidSources[source] = true
			}
			continue
		}
		source, kind, ok := alertShadowNudgeOwner(canonical.Kind)
		if !ok {
			for _, owner := range alertShadowNudgeSources {
				batch := batches[owner]
				batch.Covered = false
				batch.Status = alertShadowStatusError
				batch.Reason = alertShadowReasonCandidateInvalid
				batch.EvidenceHealth = rpc.AlertEvidenceError
				batches[owner] = batch
				invalidSources[owner] = true
			}
			continue
		}
		severity, severityOK := alertShadowNudgeSeverity(canonical.Severity)
		destination, destinationOK := alertShadowNudgeDestination(canonical.Destination)
		if !severityOK || !destinationOK || !validAlertRegistryFingerprint(canonical.Fingerprint) {
			batch := batches[source]
			batch.Covered = false
			batch.Status = alertShadowStatusError
			batch.Reason = alertShadowReasonCandidateInvalid
			batch.EvidenceHealth = rpc.AlertEvidenceError
			batches[source] = batch
			invalidSources[source] = true
			continue
		}
		_, candidateEvidenceHealth, _, _ := alertShadowNudgeCandidateHealth(canonical.Kind, result.AsOf, health, input.StoreHealth)
		episodeKey, err := rpc.BuildAlertEpisodeKey(
			source, kind, input.Scope.account, input.Scope.mode, canonical.Fingerprint,
		)
		if err != nil {
			return nil, nil, err
		}
		batch := batches[source]
		observation := alertEpisodeObservation{
			EpisodeKey: episodeKey, Source: source, Kind: kind, Active: true, Severity: severity,
			PresentationCode: alertNudgePresentationCode(canonical.Kind), EvidenceFingerprint: canonical.Fingerprint,
			EvidenceHealth: candidateEvidenceHealth, Destination: destination, EvidenceAsOf: canonical.OccurredAt.UTC(),
			ObservedAt: observedAt.UTC(), PolicyFingerprint: policyFingerprint.Key,
			ProducerDecisionReason: alertShadowDecisionNudgeActive,
		}
		if previous, duplicate := seen[episodeKey]; duplicate {
			if !alertShadowObservationsEquivalent(previous, observation) {
				return nil, nil, errors.New("alert producer nudge candidate equivocation")
			}
			duplicates[source]++
			continue
		}
		seen[episodeKey] = observation
		batch.Observations = append(batch.Observations, observation)
		batches[source] = batch
	}
	for _, source := range alertShadowNudgeSources {
		batch := batches[source]
		if invalidSources[source] {
			batch.Covered = false
			for i := range batch.Observations {
				batch.Observations[i].EvidenceHealth = rpc.AlertEvidenceError
			}
		}
		sort.Slice(batch.Observations, func(i, j int) bool {
			return batch.Observations[i].EpisodeKey < batch.Observations[j].EpisodeKey
		})
		batches[source] = batch
	}
	return batches, duplicates, nil
}

func alertShadowNudgeHealth(source rpc.AlertSource, asOf time.Time, health rpc.NudgeSourceHealth, storeHealth rpc.NudgeInputHealth) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	return alertShadowNudgeHealthInputs(asOf, alertShadowNudgeRequiredHealth(source, health, storeHealth))
}

func alertShadowNudgeCandidateHealth(kind string, asOf time.Time, health rpc.NudgeSourceHealth, storeHealth rpc.NudgeInputHealth) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	var inputs []rpc.NudgeInputHealth
	switch kind {
	case rpc.NudgeKindPolicyDrift:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Pins}
	case rpc.NudgeKindDrawdownLatched:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Capital}
	case rpc.NudgeKindShadowWouldBlock:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Capital, storeHealth}
	case rpc.NudgeKindReconcileException:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Reconciliation}
	case rpc.NudgeKindReconcileDue:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Capital, health.Cadence}
	case rpc.NudgeKindConfirmedFlow:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Reconciliation, health.ConfirmedFlow, storeHealth}
	case rpc.NudgeKindMonthlyPulse:
		inputs = []rpc.NudgeInputHealth{health.Policy, health.Pins, health.Cadence, storeHealth, health.Reconciliation}
	default:
		inputs = []rpc.NudgeInputHealth{{Status: rpc.NudgeInputStatusError, Reason: rpc.NudgeHealthReasonInvalid}}
	}
	return alertShadowNudgeHealthInputs(asOf, inputs)
}

func alertShadowNudgeHealthInputs(asOf time.Time, inputs []rpc.NudgeInputHealth) (bool, rpc.AlertEvidenceHealth, string, time.Time) {
	oldest := time.Time{}
	worst := rpc.AlertEvidenceCurrent
	reason := alertShadowReasonCurrent
	for _, input := range inputs {
		if input.AsOf.IsZero() || input.AsOf.After(asOf) {
			return false, rpc.AlertEvidenceError, alertShadowReasonHealthTimeInvalid, oldest
		}
		if oldest.IsZero() || input.AsOf.Before(oldest) {
			oldest = input.AsOf.UTC()
		}
		candidateHealth, candidateReason := alertShadowNudgeInputEvidence(input)
		if alertShadowEvidenceRank(candidateHealth) > alertShadowEvidenceRank(worst) {
			worst, reason = candidateHealth, candidateReason
		}
	}
	if worst != rpc.AlertEvidenceCurrent {
		return false, worst, reason, oldest
	}
	return true, rpc.AlertEvidenceCurrent, alertShadowReasonCurrent, oldest
}

func alertShadowNudgeRequiredHealth(source rpc.AlertSource, health rpc.NudgeSourceHealth, storeHealth rpc.NudgeInputHealth) []rpc.NudgeInputHealth {
	switch source {
	case rpc.AlertSourceRiskPolicy:
		return []rpc.NudgeInputHealth{health.Policy, health.Capital, health.Pins, storeHealth}
	case rpc.AlertSourceReconciliation:
		return []rpc.NudgeInputHealth{health.Policy, health.Reconciliation, health.Capital, health.Cadence, health.ConfirmedFlow, storeHealth}
	case rpc.AlertSourceGovernance:
		return []rpc.NudgeInputHealth{health.Policy, health.Pins, health.Cadence, storeHealth, health.Reconciliation}
	default:
		return []rpc.NudgeInputHealth{{Status: rpc.NudgeInputStatusError, Reason: rpc.NudgeHealthReasonInvalid}}
	}
}

func alertShadowNudgeInputEvidence(input rpc.NudgeInputHealth) (rpc.AlertEvidenceHealth, string) {
	switch input.Status {
	case rpc.NudgeInputStatusOK:
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	case rpc.NudgeInputStatusInactive:
		// An explicitly inactive reminder dependency is authoritative negative
		// evidence, not missing evidence. This is how a valid v3 policy says that
		// v4 cadence and confirmed-flow producers are outside its candidate
		// universe while independent policy, drawdown, and reconciliation facts
		// remain evaluable.
		return rpc.AlertEvidenceCurrent, alertShadowReasonCurrent
	case rpc.NudgeInputStatusUnapproved:
		return rpc.AlertEvidencePartial, alertShadowReasonHealthUnapproved
	case rpc.NudgeInputStatusStale:
		return rpc.AlertEvidenceStale, alertShadowReasonHealthStale
	case rpc.NudgeInputStatusUnavailable:
		return rpc.AlertEvidenceUnavailable, alertShadowReasonHealthUnavailable
	case rpc.NudgeInputStatusError:
		return rpc.AlertEvidenceError, alertShadowReasonHealthError
	default:
		return rpc.AlertEvidenceError, alertShadowReasonHealthError
	}
}

func alertShadowNudgeStoreHealthValid(health rpc.NudgeInputHealth, asOf time.Time) (bool, string) {
	if health.AsOf.IsZero() || !health.AsOf.Equal(asOf) {
		return false, alertShadowReasonHealthTimeInvalid
	}
	switch health.Status {
	case rpc.NudgeInputStatusOK:
		return health.Reason == rpc.NudgeHealthReasonNone, alertShadowReasonHealthError
	case rpc.NudgeInputStatusStale:
		return health.Reason == rpc.NudgeHealthReasonEvidenceStale, alertShadowReasonHealthError
	case rpc.NudgeInputStatusUnavailable:
		return health.Reason == rpc.NudgeHealthReasonSourceUnavailable, alertShadowReasonHealthError
	case rpc.NudgeInputStatusError:
		return health.Reason == rpc.NudgeHealthReasonEvaluationError || health.Reason == rpc.NudgeHealthReasonInvalid, alertShadowReasonHealthError
	default:
		return false, alertShadowReasonHealthError
	}
}

func alertShadowNudgeOwner(kind string) (rpc.AlertSource, rpc.AlertKind, bool) {
	switch kind {
	case rpc.NudgeKindShadowWouldBlock:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindPortfolioRisk, true
	case rpc.NudgeKindDrawdownLatched:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindDrawdown, true
	case rpc.NudgeKindPolicyDrift:
		return rpc.AlertSourceRiskPolicy, rpc.AlertKindPolicyDrift, true
	case rpc.NudgeKindReconcileDue:
		return rpc.AlertSourceReconciliation, rpc.AlertKindGovernance, true
	case rpc.NudgeKindReconcileException:
		return rpc.AlertSourceReconciliation, rpc.AlertKindReconciliationException, true
	case rpc.NudgeKindConfirmedFlow:
		return rpc.AlertSourceReconciliation, rpc.AlertKindGovernance, true
	case rpc.NudgeKindMonthlyPulse:
		return rpc.AlertSourceGovernance, rpc.AlertKindGovernance, true
	default:
		return "", "", false
	}
}

func alertNudgePresentationCode(kind string) rpc.AlertPresentationCode {
	switch kind {
	case rpc.NudgeKindShadowWouldBlock:
		return rpc.AlertPresentationRiskPolicyLimitWouldBlock
	case rpc.NudgeKindDrawdownLatched:
		return rpc.AlertPresentationRiskPolicyDrawdownLatched
	case rpc.NudgeKindPolicyDrift:
		return rpc.AlertPresentationRiskPolicyDrift
	case rpc.NudgeKindReconcileDue:
		return rpc.AlertPresentationReconciliationDue
	case rpc.NudgeKindReconcileException:
		return rpc.AlertPresentationReconciliationException
	case rpc.NudgeKindConfirmedFlow:
		return rpc.AlertPresentationReconciliationConfirmedFlow
	case rpc.NudgeKindMonthlyPulse:
		return rpc.AlertPresentationGovernanceMonthlyPulse
	default:
		return ""
	}
}

func alertShadowCandidateMatchesScope(candidate rpc.AlertCandidate, scope alertShadowBrokerScope) bool {
	if !scope.valid() {
		return false
	}
	// Rulebook and order-integrity observations use stable producer identities
	// (rule ID and journal route) rather than the evidence fingerprint as the
	// final episode-key component. The registry snapshot was already selected by
	// this exact opaque account/mode authority, and a covered batch is a complete
	// source-wide read for that authority, so omission is an explicit negative
	// for every prior episode owned by the same source.
	if candidate.Source == rpc.AlertSourceRegime || candidate.Source == rpc.AlertSourceRulebook ||
		candidate.Source == rpc.AlertSourceProtection || candidate.Source == rpc.AlertSourceOrderIntegrity ||
		candidate.Source == rpc.AlertSourceDataHealth {
		return true
	}
	identity := candidate.EvidenceFingerprint
	if candidate.Source == rpc.AlertSourceStress {
		identity = stressEpisodeIdentity
	}
	key, err := rpc.BuildAlertEpisodeKey(
		candidate.Source, candidate.Kind, scope.account, scope.mode, identity,
	)
	return err == nil && key == candidate.EpisodeKey
}

func alertShadowNudgeSeverity(severity string) (rpc.AlertSeverity, bool) {
	switch severity {
	case rpc.NudgeSeverityWatch:
		return rpc.AlertSeverityWatch, true
	case rpc.NudgeSeverityAct:
		return rpc.AlertSeverityAct, true
	default:
		return "", false
	}
}

func alertShadowNudgeDestination(destination string) (rpc.AlertDestination, bool) {
	switch destination {
	case rpc.NudgeDestinationMonitor:
		return rpc.AlertDestinationMonitor, true
	case rpc.NudgeDestinationAlerts:
		return rpc.AlertDestinationAlerts, true
	case rpc.NudgeDestinationBrief:
		return rpc.AlertDestinationBrief, true
	default:
		return "", false
	}
}

func alertShadowStatusForEvidence(health rpc.AlertEvidenceHealth) string {
	switch health {
	case rpc.AlertEvidenceCurrent:
		return alertShadowStatusCurrent
	case rpc.AlertEvidencePartial:
		return alertShadowStatusPartial
	case rpc.AlertEvidenceStale:
		return alertShadowStatusStale
	case rpc.AlertEvidenceUnavailable:
		return alertShadowStatusUnavailable
	default:
		return alertShadowStatusError
	}
}

func alertShadowEvidenceRank(health rpc.AlertEvidenceHealth) int {
	switch health {
	case rpc.AlertEvidenceCurrent:
		return 0
	case rpc.AlertEvidencePartial:
		return 1
	case rpc.AlertEvidenceUnavailable:
		return 2
	case rpc.AlertEvidenceStale:
		return 3
	case rpc.AlertEvidenceError:
		return 4
	default:
		return 5
	}
}

func alertShadowStressInputFingerprint(scope alertShadowBrokerScope, result rpc.StressResult) (string, error) {
	type sourceHealth struct {
		Source      string           `json:"source"`
		Status      string           `json:"status"`
		AsOf        time.Time        `json:"as_of"`
		Age         int64            `json:"age"`
		MaxAge      int64            `json:"max_age"`
		Fingerprint *rpc.Fingerprint `json:"fingerprint,omitempty"`
	}
	health := make([]sourceHealth, 0, len(result.SourceHealth))
	for _, source := range result.SourceHealth {
		health = append(health, sourceHealth{
			Source: source.Source, Status: source.Status, AsOf: source.AsOf, Age: source.AgeSeconds,
			MaxAge: source.MaxAgeSeconds, Fingerprint: source.Fingerprint,
		})
	}
	sort.Slice(health, func(i, j int) bool { return health[i].Source < health[j].Source })
	return alertShadowFingerprint(struct {
		Account           string              `json:"account"`
		Mode              string              `json:"mode"`
		AsOf              time.Time           `json:"as_of"`
		Fingerprint       rpc.Fingerprint     `json:"fingerprint"`
		PolicyFingerprint rpc.Fingerprint     `json:"policy_fingerprint"`
		Action            string              `json:"action"`
		Severity          risk.SignalSeverity `json:"severity"`
		Relevance         *bool               `json:"relevance,omitempty"`
		InputHealth       string              `json:"input_health"`
		SourceHealth      []sourceHealth      `json:"source_health"`
	}{
		Account: scope.account, Mode: scope.mode, AsOf: result.AsOf.UTC(),
		Fingerprint: result.Fingerprint, PolicyFingerprint: result.PolicyFingerprint,
		Action: result.Action, Severity: result.Severity, Relevance: result.PortfolioAlertRelevant,
		InputHealth: result.InputHealth, SourceHealth: health,
	})
}

func alertShadowRegimeInputFingerprint(scope alertShadowBrokerScope, result rpc.RegimeSnapshotResult) (string, error) {
	type sourceHealth struct {
		Source       string    `json:"source"`
		Status       string    `json:"status"`
		AsOf         time.Time `json:"as_of"`
		MaxAge       int64     `json:"max_age"`
		RefreshState string    `json:"refresh_state,omitempty"`
	}
	health := make([]sourceHealth, 0, len(result.SourceHealth))
	for _, source := range result.SourceHealth {
		health = append(health, sourceHealth{Source: strings.ToLower(strings.TrimSpace(source.Source)),
			Status: strings.ToLower(strings.TrimSpace(source.Status)), AsOf: source.AsOf.UTC(),
			MaxAge: source.MaxAgeSeconds, RefreshState: strings.ToLower(strings.TrimSpace(source.RefreshState))})
	}
	sort.Slice(health, func(i, j int) bool { return health[i].Source < health[j].Source })
	type authorityHealth struct {
		Status        rpc.RegimeAuthorityStatus      `json:"status,omitempty"`
		Refreshing    bool                           `json:"refreshing"`
		LastSuccessAt *time.Time                     `json:"last_success_at,omitempty"`
		FailureCode   rpc.RegimeAuthorityFailureCode `json:"failure_code,omitempty"`
	}
	var authority *authorityHealth
	if result.AuthorityHealth != nil {
		authority = &authorityHealth{Status: result.AuthorityHealth.Status, Refreshing: result.AuthorityHealth.Refreshing,
			LastSuccessAt: result.AuthorityHealth.LastSuccessAt, FailureCode: result.AuthorityHealth.FailureCode}
	}
	return alertShadowFingerprint(struct {
		Account     string             `json:"account"`
		Mode        string             `json:"mode"`
		AsOf        time.Time          `json:"as_of"`
		Fingerprint rpc.Fingerprint    `json:"fingerprint"`
		Lifecycle   rpc.LifecycleState `json:"lifecycle"`
		Authority   *authorityHealth   `json:"authority,omitempty"`
		Health      []sourceHealth     `json:"health"`
	}{scope.account, scope.mode, result.AsOf.UTC(), result.Fingerprint, result.Lifecycle, authority, health})
}

func alertShadowProtectionInputFingerprint(input alertShadowProtectionInput) (string, error) {
	type rowState struct {
		Underlying string `json:"underlying"`
		State      string `json:"state"`
	}
	rows := make([]rowState, 0, len(input.Summary.ByUnderlying))
	for _, row := range input.Summary.ByUnderlying {
		rows = append(rows, rowState{Underlying: strings.ToUpper(strings.TrimSpace(row.Underlying)), State: strings.TrimSpace(row.State)})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Underlying != rows[j].Underlying {
			return rows[i].Underlying < rows[j].Underlying
		}
		return rows[i].State < rows[j].State
	})
	facts, valid := alertShadowProtectionFacts(input.Summary)
	return alertShadowFingerprint(struct {
		Account               string                      `json:"account"`
		Mode                  string                      `json:"mode"`
		Status                string                      `json:"status"`
		SummaryStatus         string                      `json:"summary_status"`
		EvidenceAsOf          time.Time                   `json:"evidence_as_of"`
		OrderSnapshotAsOf     time.Time                   `json:"order_snapshot_as_of"`
		SummaryAsOf           time.Time                   `json:"summary_as_of"`
		OrderUniverse         string                      `json:"order_universe"`
		OrderSnapshotComplete bool                        `json:"order_snapshot_complete"`
		Rows                  []rowState                  `json:"rows"`
		Facts                 []alertShadowProtectionFact `json:"facts"`
		Valid                 bool                        `json:"valid"`
	}{input.Scope.account, input.Scope.mode, input.Status, input.Summary.Status,
		input.EvidenceAsOf.UTC(), input.OrderSnapshotAsOf.UTC(), input.Summary.AsOf.UTC(), input.OrderUniverse,
		input.OrderSnapshotComplete, rows, facts, valid})
}

func alertShadowDataHealthInputFingerprint(input alertShadowDataHealthInput) (string, error) {
	type subsystem struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	type quality struct {
		Surface string `json:"surface"`
		Status  string `json:"status"`
		Cadence string `json:"cadence,omitempty"`
	}
	type farm struct {
		Type   string `json:"type"`
		Status string `json:"status"`
		Code   int    `json:"code,omitempty"`
	}
	subsystems := make([]subsystem, 0, len(input.Health.Subsystems))
	for _, row := range input.Health.Subsystems {
		subsystems = append(subsystems, subsystem{Name: strings.ToLower(strings.TrimSpace(row.Name)),
			Status: strings.ToLower(strings.TrimSpace(row.Status))})
	}
	sort.Slice(subsystems, func(i, j int) bool { return subsystems[i].Name < subsystems[j].Name })
	qualities := make([]quality, 0, len(input.Health.DataQuality))
	for _, row := range input.Health.DataQuality {
		qualities = append(qualities, quality{Surface: strings.ToLower(strings.TrimSpace(row.Surface)),
			Status: strings.ToLower(strings.TrimSpace(row.Status)), Cadence: strings.ToLower(strings.TrimSpace(row.CadenceState))})
	}
	sort.Slice(qualities, func(i, j int) bool { return qualities[i].Surface < qualities[j].Surface })
	farms := make([]farm, 0, len(input.Health.DataFarms))
	for _, row := range input.Health.DataFarms {
		farms = append(farms, farm{Type: strings.ToLower(strings.TrimSpace(row.Type)),
			Status: strings.ToLower(strings.TrimSpace(row.Status)), Code: row.Code})
	}
	sort.Slice(farms, func(i, j int) bool {
		if farms[i].Type != farms[j].Type {
			return farms[i].Type < farms[j].Type
		}
		if farms[i].Code != farms[j].Code {
			return farms[i].Code < farms[j].Code
		}
		return farms[i].Status < farms[j].Status
	})
	facts, reason, valid, current := alertShadowDataHealthFacts(input)
	return alertShadowFingerprint(struct {
		Account               string                              `json:"account"`
		Mode                  string                              `json:"mode"`
		Connected             bool                                `json:"connected"`
		GatewayPhase          alertShadowGatewayPhase             `json:"gateway_phase"`
		ProposalsExpected     bool                                `json:"proposals_expected"`
		OpportunitiesExpected bool                                `json:"opportunities_expected"`
		Subsystems            []subsystem                         `json:"subsystems"`
		Quality               []quality                           `json:"quality"`
		Farms                 []farm                              `json:"farms"`
		Facts                 []alertShadowDataHealthSemanticFact `json:"facts"`
		Reason                string                              `json:"reason"`
		Valid                 bool                                `json:"valid"`
		Current               bool                                `json:"current"`
	}{input.Scope.account, input.Scope.mode, input.Health.Connected, input.GatewayPhase, input.ProposalsExpected,
		input.OpportunitiesExpected, subsystems, qualities, farms, alertShadowDataHealthSemanticFacts(facts), reason, valid, current})
}

func alertShadowNudgeInputFingerprint(input alertShadowNudgeInput) (string, error) {
	result := input.Snapshot
	type candidate struct {
		Fingerprint string    `json:"fingerprint"`
		Kind        string    `json:"kind"`
		State       string    `json:"state"`
		OccurredAt  time.Time `json:"occurred_at"`
		DueAt       time.Time `json:"due_at"`
	}
	candidates := make([]candidate, 0, len(result.Candidates))
	for _, item := range result.Candidates {
		candidates = append(candidates, candidate{
			Fingerprint: item.Fingerprint, Kind: item.Kind, State: item.State,
			OccurredAt: item.OccurredAt, DueAt: item.DueAt,
		})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Kind != candidates[j].Kind {
			return candidates[i].Kind < candidates[j].Kind
		}
		return candidates[i].Fingerprint < candidates[j].Fingerprint
	})
	return alertShadowFingerprint(struct {
		Account               string                          `json:"account"`
		Mode                  string                          `json:"mode"`
		AsOf                  time.Time                       `json:"as_of"`
		PolicyFingerprint     rpc.Fingerprint                 `json:"policy_fingerprint"`
		Candidates            []candidate                     `json:"candidates"`
		SourceHealth          rpc.NudgeSourceHealth           `json:"source_health"`
		StoreHealth           rpc.NudgeInputHealth            `json:"store_health"`
		ConfirmedFlowCoverage *rpc.NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	}{
		input.Scope.account, input.Scope.mode, result.AsOf.UTC(), input.PolicyFingerprint, candidates,
		rpc.NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates)), input.StoreHealth, result.ConfirmedFlowCoverage,
	})
}

func alertShadowRulebookInputFingerprint(scope alertShadowBrokerScope, result rpc.RulesResult) (string, error) {
	type healthRow struct {
		Source string    `json:"source"`
		Status string    `json:"status"`
		AsOf   time.Time `json:"as_of"`
	}
	health := make([]healthRow, 0, len(result.InputHealth))
	for _, row := range result.InputHealth {
		health = append(health, healthRow{Source: row.Source, Status: row.Status, AsOf: row.AsOf.UTC()})
	}
	sort.Slice(health, func(i, j int) bool { return health[i].Source < health[j].Source })
	return alertShadowFingerprint(struct {
		Account           string           `json:"account"`
		Mode              string           `json:"mode"`
		AsOf              time.Time        `json:"as_of"`
		Enabled           bool             `json:"enabled"`
		Status            string           `json:"status"`
		Rules             []risk.RuleRow   `json:"rules"`
		Health            []healthRow      `json:"health"`
		PolicyFingerprint *rpc.Fingerprint `json:"policy_fingerprint,omitempty"`
	}{
		Account: scope.account, Mode: scope.mode, AsOf: result.AsOf.UTC(), Enabled: result.Enabled,
		Status: result.Status, Rules: result.Rules, Health: health, PolicyFingerprint: result.PolicyFingerprint,
	})
}

func alertShadowOrderIntegrityInputFingerprint(scope alertShadowBrokerScope, input orderIntegrityEvaluation) (string, error) {
	type orderFact struct {
		Route       string    `json:"route"`
		Kind        string    `json:"kind"`
		Severity    string    `json:"severity"`
		Remaining   float64   `json:"remaining"`
		ShortRisk   float64   `json:"short_risk"`
		ReduceTo    float64   `json:"reduce_to"`
		BrokerTruth time.Time `json:"broker_truth"`
	}
	facts := make([]orderFact, 0, len(input.Orders))
	for _, order := range input.Orders {
		if !order.Open || order.ReconciliationKind == "" {
			continue
		}
		facts = append(facts, orderFact{
			Route: orderViewKey(order), Kind: order.ReconciliationKind, Severity: order.ReconciliationSeverity,
			Remaining: order.Remaining, ShortRisk: order.ShortRiskQuantity, ReduceTo: order.ReduceToQuantity,
			BrokerTruth: order.BrokerTruthAsOf.UTC(),
		})
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Route < facts[j].Route })
	return alertShadowFingerprint(struct {
		Account      string      `json:"account"`
		Mode         string      `json:"mode"`
		AsOf         time.Time   `json:"as_of"`
		Status       string      `json:"status"`
		EvidenceAsOf time.Time   `json:"evidence_as_of"`
		Facts        []orderFact `json:"facts"`
	}{scope.account, scope.mode, input.AsOf.UTC(), input.Status, input.EvidenceAsOf.UTC(), facts})
}

func alertShadowObservationsEquivalent(left, right alertEpisodeObservation) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func alertShadowFingerprint(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint alert producer value: %w", err)
	}
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
