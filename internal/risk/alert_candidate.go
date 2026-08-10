package risk

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// AlertCandidateSnapshotVersion identifies the scope-bound source-neutral
// alert candidate wire contract. It does not approve routing or pageability.
const (
	AlertCandidateSnapshotVersion  = "alert-candidate-snapshot-v3"
	alertEpisodeKeyPrefix          = "alert-episode-v1:"
	alertOccurrenceKeyPrefix       = "alert-occurrence-v1:"
	alertEvidenceFingerprintPrefix = "sha256:"
	alertAuthorityScopePrefix      = "alert-authority-scope-v1:"
	alertEpisodeIdentityVersion    = "alert-episode-v1"
	alertOccurrenceIdentityVersion = "alert-occurrence-v1"
	alertAuthorityIdentityVersion  = "alert-authority-scope-v1"
)

// AlertSource names the daemon authority that produced a candidate. These are
// product subsystem names, never broker, account, symbol, order, or device
type AlertSource string

// AlertSourceStress and the related constants identify candidate-producing
// AlertSourceStress keeps the wire value "canary" for the same reason
// parts are raw account/mode values that no stored document carries, so no
// registry or ledger migration can recompute the new key from the old — every
const (
	AlertSourceStress         AlertSource = "canary"
	AlertSourceRegime         AlertSource = "regime"
	AlertSourceRulebook       AlertSource = "rulebook"
	AlertSourceRiskPolicy     AlertSource = "risk_policy"
	AlertSourceProtection     AlertSource = "protection"
	AlertSourceOrderIntegrity AlertSource = "order_integrity"
	AlertSourceReconciliation AlertSource = "reconciliation"
	AlertSourceGovernance     AlertSource = "governance"
	AlertSourceDataHealth     AlertSource = "data_health"
	AlertSourceDelivery       AlertSource = "delivery"
)

// AlertKind is the redacted semantic class of an operator condition. Source
type AlertKind string

// AlertKindMarketState and the related constants classify the root operator
const (
	AlertKindMarketState             AlertKind = "market_state"
	AlertKindPortfolioRisk           AlertKind = "portfolio_risk"
	AlertKindMarginSafety            AlertKind = "margin_safety"
	AlertKindDrawdown                AlertKind = "drawdown"
	AlertKindProtectionGap           AlertKind = "protection_gap"
	AlertKindOrderIntegrity          AlertKind = "order_integrity"
	AlertKindReconciliationException AlertKind = "reconciliation_exception"
	AlertKindGovernance              AlertKind = "governance"
	AlertKindPolicyDrift             AlertKind = "policy_drift"
	AlertKindDataHealth              AlertKind = "data_health"
	AlertKindDeliveryHealth          AlertKind = "delivery_health"
)

// AlertEpisodeState is the daemon producer's lifecycle state. The daemon owns
// dwell, escalation, recovery, and re-arm; the app only persists delivery and
// receipt state for the producer-authored occurrence. A new opening/reopen or
// only revisions and recovery do not.
type AlertEpisodeState string

// AlertEpisodeOpen and the related constants describe a producer-owned episode
const (
	AlertEpisodeOpen      AlertEpisodeState = "open"
	AlertEpisodeEscalated AlertEpisodeState = "escalated"
	AlertEpisodeRecovered AlertEpisodeState = "recovered"
)

// AlertSeverity expresses the producer-classified urgency of an alert
type AlertSeverity string

// AlertSeverityObserve and the related constants rank candidate urgency.
const (
	AlertSeverityObserve AlertSeverity = "observe"
	AlertSeverityWatch   AlertSeverity = "watch"
	AlertSeverityAct     AlertSeverity = "act"
	AlertSeverityUrgent  AlertSeverity = "urgent"
)

// AlertPresentationCode is a closed, redacted copy key. Adapters may map it to
// human text, but cannot receive or interpolate broker identifiers through it.
type AlertPresentationCode string

// Alert presentation codes cover every producer-owned candidate class. The
// generic condition codes exist only to preserve lifecycle identity while a
// version-2 registry is upgraded; the next producer observation replaces them
// with the precise code without opening a new occurrence.
const (
	AlertPresentationPortfolioStress                  AlertPresentationCode = "portfolio_stress"
	AlertPresentationMarginCushion                    AlertPresentationCode = "margin_cushion"
	AlertPresentationRegimeMarketStress               AlertPresentationCode = "regime_market_stress"
	AlertPresentationRulebookSingleNameExposure       AlertPresentationCode = "rulebook_single_name_exposure"
	AlertPresentationRulebookOptionLinePremium        AlertPresentationCode = "rulebook_option_line_premium"
	AlertPresentationRulebookCashSellOnly             AlertPresentationCode = "rulebook_cash_sell_only"
	AlertPresentationRulebookExtrinsicBudget          AlertPresentationCode = "rulebook_extrinsic_budget"
	AlertPresentationRulebookExpiryRunway             AlertPresentationCode = "rulebook_expiry_runway"
	AlertPresentationRulebookCatalystCoverage         AlertPresentationCode = "rulebook_catalyst_coverage"
	AlertPresentationRulebookOverwriteEarnings        AlertPresentationCode = "rulebook_overwrite_earnings"
	AlertPresentationRulebookEarningsSizeFreeze       AlertPresentationCode = "rulebook_earnings_size_freeze"
	AlertPresentationRulebookRedOnGreen               AlertPresentationCode = "rulebook_red_on_green"
	AlertPresentationRulebookWinnerTrim               AlertPresentationCode = "rulebook_winner_trim"
	AlertPresentationRulebookGreenDayAction           AlertPresentationCode = "rulebook_green_day_action"
	AlertPresentationRulebookHedgeIntegrity           AlertPresentationCode = "rulebook_hedge_integrity"
	AlertPresentationRulebookExitDiscipline           AlertPresentationCode = "rulebook_exit_discipline"
	AlertPresentationRulebookFXExposure               AlertPresentationCode = "rulebook_fx_exposure"
	AlertPresentationProtectionOrphanedOrder          AlertPresentationCode = "protection_orphaned_order"
	AlertPresentationProtectionReconciliationRequired AlertPresentationCode = "protection_reconciliation_required"
	AlertPresentationOrderIntegrityMismatch           AlertPresentationCode = "order_integrity_mismatch"
	AlertPresentationDataHealthGateway                AlertPresentationCode = "data_health_gateway"
	AlertPresentationDataHealthStorage                AlertPresentationCode = "data_health_storage"
	AlertPresentationDataHealthProposals              AlertPresentationCode = "data_health_proposals"
	AlertPresentationDataHealthOpportunities          AlertPresentationCode = "data_health_opportunities"
	AlertPresentationDataHealthDataFarms              AlertPresentationCode = "data_health_data_farms"
	AlertPresentationDataHealthRegime                 AlertPresentationCode = "data_health_regime"
	AlertPresentationDataHealthGamma                  AlertPresentationCode = "data_health_gamma"
	AlertPresentationDataHealthQuality                AlertPresentationCode = "data_health_quality"
	AlertPresentationRiskPolicyLimitWouldBlock        AlertPresentationCode = "risk_policy_limit_would_block"
	AlertPresentationRiskPolicyDrawdownLatched        AlertPresentationCode = "risk_policy_drawdown_latched"
	AlertPresentationRiskPolicyDrift                  AlertPresentationCode = "risk_policy_drift"
	AlertPresentationReconciliationDue                AlertPresentationCode = "reconciliation_due"
	AlertPresentationReconciliationException          AlertPresentationCode = "reconciliation_exception"
	AlertPresentationReconciliationConfirmedFlow      AlertPresentationCode = "reconciliation_confirmed_flow"
	AlertPresentationGovernanceMonthlyPulse           AlertPresentationCode = "governance_monthly_pulse"
	AlertPresentationDeliveryHealth                   AlertPresentationCode = "delivery_health"
	AlertPresentationRulebookLegacyCondition          AlertPresentationCode = "rulebook_condition"
	AlertPresentationRiskPolicyLegacyCondition        AlertPresentationCode = "risk_policy_condition"
	AlertPresentationReconciliationLegacyCondition    AlertPresentationCode = "reconciliation_condition"
	AlertPresentationGovernanceLegacyCondition        AlertPresentationCode = "governance_condition"
)

// AlertEvidenceHealth describes whether a candidate's supporting observation
type AlertEvidenceHealth string

// AlertEvidenceCurrent and the related constants classify the supporting
const (
	AlertEvidenceCurrent     AlertEvidenceHealth = "current"
	AlertEvidencePartial     AlertEvidenceHealth = "partial"
	AlertEvidenceStale       AlertEvidenceHealth = "stale"
	AlertEvidenceUnavailable AlertEvidenceHealth = "unavailable"
	AlertEvidenceError       AlertEvidenceHealth = "error"
)

// AlertDestination names a redacted product surface, not a device or target.
type AlertDestination string

// AlertDestinationMonitor and the related constants name redacted product
const (
	AlertDestinationMonitor AlertDestination = "monitor"
	AlertDestinationAlerts  AlertDestination = "alerts"
	AlertDestinationBrief   AlertDestination = "brief"
)

// AlertCoverageState describes how much of the expected source set was
// evaluated. The zero value is invalid.
type AlertCoverageState string

// AlertCoverageComplete and the related constants classify expected-source
const (
	AlertCoverageComplete    AlertCoverageState = "complete"
	AlertCoveragePartial     AlertCoverageState = "partial"
	AlertCoverageUnavailable AlertCoverageState = "unavailable"
)

// AlertCoverageFreshness describes the timeliness of the covered sources. An
// unavailable source set must use AlertCoverageUnknown.
type AlertCoverageFreshness string

// AlertCoverageCurrent and the related constants classify coverage freshness.
const (
	AlertCoverageCurrent AlertCoverageFreshness = "current"
	AlertCoverageStale   AlertCoverageFreshness = "stale"
	AlertCoverageUnknown AlertCoverageFreshness = "unknown"
)

// AlertSnapshotState is the state derived from validated candidates and
// coverage. Clear requires complete, current coverage.
type AlertSnapshotState string

// AlertSnapshotClear and the related constants are derived snapshot states.
const (
	AlertSnapshotClear   AlertSnapshotState = "clear"
	AlertSnapshotActive  AlertSnapshotState = "active"
	AlertSnapshotUnknown AlertSnapshotState = "unknown"
)

// AlertCandidate contains classified, redacted semantics only. It deliberately
// has no display copy, source subject, account, symbol, order, route, display
// ID, target ID, device ID, or delivery-attempt ID.
//
// EpisodeKey is stable for the same root problem. EvidenceFingerprint changes
// when classified supporting evidence changes. OccurrenceKey changes when the
// daemon opens/reopens the root problem or classifies an escalation as a new
// page-worthy occurrence; it stays stable for evidence-only revisions and the
// occurrence's recovery. App delivery records bind this private occurrence key
// and create their own per-target attempt IDs; they never infer re-arm,
// page-worthy escalation, or use a display ID as authority. StateChangedAt is
// the daemon's semantic transition time.
type AlertCandidate struct {
	EpisodeKey          string                `json:"episode_key"`
	OccurrenceKey       string                `json:"occurrence_key"`
	EvidenceFingerprint string                `json:"evidence_fingerprint"`
	Source              AlertSource           `json:"source"`
	Kind                AlertKind             `json:"kind"`
	PresentationCode    AlertPresentationCode `json:"presentation_code"`
	State               AlertEpisodeState     `json:"state"`
	Severity            AlertSeverity         `json:"severity"`
	EvidenceHealth      AlertEvidenceHealth   `json:"evidence_health"`
	Destination         AlertDestination      `json:"destination"`
	EvidenceAsOf        time.Time             `json:"evidence_as_of"`
	StateChangedAt      time.Time             `json:"state_changed_at"`
	ObservedAt          time.Time             `json:"observed_at"`
}

// AlertSourceCoverage names the health of one expected producer in the same
// snapshot as its candidates. Times are absent only before that producer has
// been observed in the current authority scope.
type AlertSourceCoverage struct {
	Source         AlertSource         `json:"source"`
	Status         string              `json:"status"`
	Reason         string              `json:"reason"`
	EvidenceHealth AlertEvidenceHealth `json:"evidence_health"`
	InputAsOf      time.Time           `json:"input_as_of"`
	ObservedAt     time.Time           `json:"observed_at"`
	EvidenceAsOf   time.Time           `json:"evidence_as_of"`
	FreshUntil     time.Time           `json:"fresh_until"`
	Covered        bool                `json:"covered"`
}

// AlertCoverage makes the universe behind an empty candidate list explicit.
// CoveredSources must be a subset of ExpectedSources. Complete means the two
type AlertCoverage struct {
	State           AlertCoverageState     `json:"state"`
	Freshness       AlertCoverageFreshness `json:"freshness"`
	AsOf            time.Time              `json:"as_of"`
	ExpectedSources []AlertSource          `json:"expected_sources"`
	CoveredSources  []AlertSource          `json:"covered_sources"`
}

// AlertCandidateSnapshot is the versioned daemon-side measurement contract.
// Clear only with complete, current coverage; otherwise it is Unknown.
type AlertCandidateSnapshot struct {
	SchemaVersion  string                `json:"schema_version"`
	AuthorityScope string                `json:"authority_scope"`
	AsOf           time.Time             `json:"as_of"`
	CurrentState   AlertSnapshotState    `json:"current_state"`
	Coverage       AlertCoverage         `json:"coverage"`
	Sources        []AlertSourceCoverage `json:"sources"`
	Candidates     []AlertCandidate      `json:"candidates"`
}

// BuildAlertAuthorityScope binds private alert state to one normalized broker
// account/mode context without exposing either raw value. The canonicalization
// cannot split one authority. Callers must still reject aggregate or unknown
// broker scopes before invoking it.
func BuildAlertAuthorityScope(account, mode string) (string, error) {
	account = strings.ToUpper(strings.TrimSpace(account))
	mode = strings.ToLower(strings.TrimSpace(mode))
	if account == "" || mode == "" {
		return "", errors.New("alert authority scope requires account and mode")
	}
	if len(account) > 256 || len(mode) > 32 {
		return "", errors.New("alert authority scope input is too long")
	}
	raw, err := json.Marshal(struct {
		Version string `json:"version"`
		Account string `json:"account"`
		Mode    string `json:"mode"`
	}{
		Version: alertAuthorityIdentityVersion,
		Account: account,
		Mode:    mode,
	})
	if err != nil {
		return "", fmt.Errorf("encode alert authority scope: %w", err)
	}
	sum := sha256.Sum256(raw)
	return alertAuthorityScopePrefix + hex.EncodeToString(sum[:]), nil
}

// ValidateAlertAuthorityScope accepts only the opaque value produced by
// BuildAlertAuthorityScope. Raw account or mode values never belong in a
// candidate snapshot, daemon registry document, or app inbox.
func ValidateAlertAuthorityScope(value string) error {
	if !validOpaqueSHA256(value, alertAuthorityScopePrefix) {
		return errors.New("invalid alert authority_scope")
	}
	return nil
}

// BuildAlertEpisodeKey hashes a producer-approved semantic identity into an
// opaque stable key. identityParts may contain sensitive source identities;
// callers must not persist or log them. Only the returned key belongs on the
// alert contract. Part order is significant.
func BuildAlertEpisodeKey(source AlertSource, kind AlertKind, identityParts ...string) (string, error) {
	if !validAlertSource(source) {
		return "", errors.New("invalid alert episode source")
	}
	if !validAlertKind(kind) {
		return "", errors.New("invalid alert episode kind")
	}
	if len(identityParts) == 0 || len(identityParts) > 16 {
		return "", errors.New("alert episode identity requires between 1 and 16 parts")
	}
	parts := make([]string, len(identityParts))
	for i, part := range identityParts {
		part = strings.TrimSpace(part)
		if part == "" || len(part) > 1024 {
			return "", fmt.Errorf("invalid alert episode identity part %d", i)
		}
		parts[i] = part
	}
	raw, err := json.Marshal(struct {
		Version string      `json:"version"`
		Source  AlertSource `json:"source"`
		Kind    AlertKind   `json:"kind"`
		Parts   []string    `json:"parts"`
	}{
		Version: alertEpisodeIdentityVersion,
		Source:  source,
		Kind:    kind,
		Parts:   parts,
	})
	if err != nil {
		return "", fmt.Errorf("encode alert episode identity: %w", err)
	}
	sum := sha256.Sum256(raw)
	return alertEpisodeKeyPrefix + hex.EncodeToString(sum[:]), nil
}

// BuildAlertOccurrenceKey hashes a daemon-authored occurrence identity for one
// escalation starts a new occurrence; this helper only makes that decision
// opaque and stable across transport. The identity parts must not be persisted
func BuildAlertOccurrenceKey(episodeKey string, identityParts ...string) (string, error) {
	if !validOpaqueSHA256(episodeKey, alertEpisodeKeyPrefix) {
		return "", errors.New("invalid alert occurrence episode_key")
	}
	if len(identityParts) == 0 || len(identityParts) > 16 {
		return "", errors.New("alert occurrence identity requires between 1 and 16 parts")
	}
	parts := make([]string, len(identityParts))
	for i, part := range identityParts {
		part = strings.TrimSpace(part)
		if part == "" || len(part) > 1024 {
			return "", fmt.Errorf("invalid alert occurrence identity part %d", i)
		}
		parts[i] = part
	}
	raw, err := json.Marshal(struct {
		Version    string   `json:"version"`
		EpisodeKey string   `json:"episode_key"`
		Parts      []string `json:"parts"`
	}{
		Version:    alertOccurrenceIdentityVersion,
		EpisodeKey: episodeKey,
		Parts:      parts,
	})
	if err != nil {
		return "", fmt.Errorf("encode alert occurrence identity: %w", err)
	}
	sum := sha256.Sum256(raw)
	return alertOccurrenceKeyPrefix + hex.EncodeToString(sum[:]), nil
}

// Validate checks the candidate's opaque identities, enum values, timestamps,
// and recovery coherence.
func (candidate AlertCandidate) Validate() error {
	if !validOpaqueSHA256(candidate.EpisodeKey, alertEpisodeKeyPrefix) {
		return errors.New("invalid alert candidate episode_key")
	}
	if !validOpaqueSHA256(candidate.OccurrenceKey, alertOccurrenceKeyPrefix) {
		return errors.New("invalid alert candidate occurrence_key")
	}
	if !validOpaqueSHA256(candidate.EvidenceFingerprint, alertEvidenceFingerprintPrefix) {
		return errors.New("invalid alert candidate evidence_fingerprint")
	}
	if !validAlertSource(candidate.Source) {
		return errors.New("invalid alert candidate source")
	}
	if !validAlertKind(candidate.Kind) {
		return errors.New("invalid alert candidate kind")
	}
	if !validAlertPresentationCode(candidate.Source, candidate.PresentationCode) {
		return errors.New("invalid alert candidate presentation_code")
	}
	if !validAlertEpisodeState(candidate.State) {
		return errors.New("invalid alert candidate state")
	}
	if !validAlertSeverity(candidate.Severity) {
		return errors.New("invalid alert candidate severity")
	}
	if !validAlertEvidenceHealth(candidate.EvidenceHealth) {
		return errors.New("invalid alert candidate evidence_health")
	}
	if !validAlertDestination(candidate.Destination) {
		return errors.New("invalid alert candidate destination")
	}
	if candidate.EvidenceAsOf.IsZero() || candidate.StateChangedAt.IsZero() || candidate.ObservedAt.IsZero() {
		return errors.New("alert candidate timestamps must be present")
	}
	if candidate.EvidenceAsOf.After(candidate.ObservedAt) {
		return errors.New("alert candidate evidence_as_of is after observed_at")
	}
	if candidate.StateChangedAt.After(candidate.ObservedAt) {
		return errors.New("alert candidate state_changed_at is after observed_at")
	}
	if candidate.State == AlertEpisodeRecovered && candidate.EvidenceHealth != AlertEvidenceCurrent {
		return errors.New("recovered alert candidate requires current evidence")
	}
	return nil
}

// Validate checks one source row without interpreting producer-specific status
func (source AlertSourceCoverage) Validate(snapshotAsOf time.Time) error {
	if !validAlertSource(source.Source) {
		return errors.New("invalid alert source coverage source")
	}
	if !validAlertCode(source.Status) || !validAlertCode(source.Reason) {
		return errors.New("invalid alert source coverage status or reason")
	}
	if !validAlertEvidenceHealth(source.EvidenceHealth) {
		return errors.New("invalid alert source coverage evidence_health")
	}
	if snapshotAsOf.IsZero() {
		return errors.New("alert source coverage requires snapshot as_of")
	}
	allZero := source.InputAsOf.IsZero() && source.ObservedAt.IsZero() && source.EvidenceAsOf.IsZero() && source.FreshUntil.IsZero()
	if allZero {
		if source.Covered || source.EvidenceHealth != AlertEvidenceUnavailable {
			return errors.New("unobserved alert source coverage must be unavailable and uncovered")
		}
		return nil
	}
	if source.InputAsOf.IsZero() || source.ObservedAt.IsZero() || source.EvidenceAsOf.IsZero() || source.FreshUntil.IsZero() {
		return errors.New("observed alert source coverage requires all timestamps")
	}
	if source.InputAsOf.After(source.ObservedAt) || source.ObservedAt.After(snapshotAsOf) || source.EvidenceAsOf.After(snapshotAsOf) {
		return errors.New("alert source coverage timestamps are out of order")
	}
	if source.FreshUntil.Before(source.ObservedAt) {
		return errors.New("alert source coverage fresh_until precedes observed_at")
	}
	if source.Covered && source.EvidenceHealth != AlertEvidenceCurrent && source.EvidenceHealth != AlertEvidenceStale {
		return errors.New("covered alert source requires current or stale evidence")
	}
	return nil
}

// Validate checks coverage set membership and the coherence of state and
// freshness. CoveredSources must be non-nil even when it is empty.
func (coverage AlertCoverage) Validate() error {
	if !validAlertCoverageState(coverage.State) {
		return errors.New("invalid alert coverage state")
	}
	if !validAlertCoverageFreshness(coverage.Freshness) {
		return errors.New("invalid alert coverage freshness")
	}
	if coverage.AsOf.IsZero() {
		return errors.New("alert coverage is missing as_of")
	}
	if len(coverage.ExpectedSources) == 0 {
		return errors.New("alert coverage requires expected_sources")
	}
	if coverage.CoveredSources == nil {
		return errors.New("alert coverage requires covered_sources")
	}
	expected, err := alertSourceSet("expected_sources", coverage.ExpectedSources)
	if err != nil {
		return err
	}
	covered, err := alertSourceSet("covered_sources", coverage.CoveredSources)
	if err != nil {
		return err
	}
	for source := range covered {
		if _, ok := expected[source]; !ok {
			return errors.New("alert coverage covered_sources is not a subset of expected_sources")
		}
	}

	switch coverage.State {
	case AlertCoverageComplete:
		if len(covered) != len(expected) {
			return errors.New("complete alert coverage does not cover every expected source")
		}
		if coverage.Freshness == AlertCoverageUnknown {
			return errors.New("complete alert coverage cannot have unknown freshness")
		}
	case AlertCoveragePartial:
		if len(covered) == 0 || len(covered) == len(expected) {
			return errors.New("partial alert coverage requires a non-empty proper subset")
		}
		if coverage.Freshness == AlertCoverageUnknown {
			return errors.New("partial alert coverage cannot have unknown freshness")
		}
	case AlertCoverageUnavailable:
		if len(covered) != 0 {
			return errors.New("unavailable alert coverage cannot cover a source")
		}
		if coverage.Freshness != AlertCoverageUnknown {
			return errors.New("unavailable alert coverage requires unknown freshness")
		}
	}
	return nil
}

// Validate checks the complete snapshot contract and verifies that
// CurrentState is the state implied by its candidates and coverage.
func (snapshot AlertCandidateSnapshot) Validate() error {
	if snapshot.SchemaVersion != AlertCandidateSnapshotVersion {
		return errors.New("invalid alert candidate snapshot schema_version")
	}
	if err := ValidateAlertAuthorityScope(snapshot.AuthorityScope); err != nil {
		return err
	}
	if snapshot.AsOf.IsZero() {
		return errors.New("alert candidate snapshot is missing as_of")
	}
	if snapshot.Candidates == nil {
		return errors.New("alert candidate snapshot requires candidates")
	}
	if snapshot.Sources == nil {
		return errors.New("alert candidate snapshot requires sources")
	}
	if err := snapshot.Coverage.Validate(); err != nil {
		return fmt.Errorf("invalid alert candidate snapshot coverage: %w", err)
	}
	if snapshot.Coverage.AsOf.After(snapshot.AsOf) {
		return errors.New("alert candidate snapshot coverage is after as_of")
	}
	if snapshot.Coverage.Freshness == AlertCoverageCurrent && !snapshot.Coverage.AsOf.Equal(snapshot.AsOf) {
		return errors.New("current alert candidate coverage must match snapshot as_of")
	}

	expected, _ := alertSourceSet("expected_sources", snapshot.Coverage.ExpectedSources)
	covered, _ := alertSourceSet("covered_sources", snapshot.Coverage.CoveredSources)
	if len(snapshot.Sources) != len(expected) {
		return errors.New("alert candidate snapshot requires one source row per expected source")
	}
	seenSources := make(map[AlertSource]struct{}, len(snapshot.Sources))
	previousSource := AlertSource("")
	for i, source := range snapshot.Sources {
		if err := source.Validate(snapshot.AsOf); err != nil {
			return fmt.Errorf("invalid alert candidate snapshot source at index %d: %w", i, err)
		}
		if _, ok := expected[source.Source]; !ok {
			return fmt.Errorf("invalid alert candidate snapshot source at index %d: source is not expected", i)
		}
		if _, duplicate := seenSources[source.Source]; duplicate {
			return fmt.Errorf("invalid alert candidate snapshot source at index %d: duplicate source", i)
		}
		if previousSource != "" && source.Source <= previousSource {
			return errors.New("alert candidate snapshot sources are not canonical")
		}
		previousSource = source.Source
		seenSources[source.Source] = struct{}{}
		_, aggregateCovered := covered[source.Source]
		if source.Covered != aggregateCovered {
			return fmt.Errorf("invalid alert candidate snapshot source at index %d: covered state disagrees with aggregate", i)
		}
	}
	seenEpisodes := make(map[string]struct{}, len(snapshot.Candidates))
	seenOccurrences := make(map[string]struct{}, len(snapshot.Candidates))
	hasActive := false
	for i, candidate := range snapshot.Candidates {
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("invalid alert candidate at index %d: %w", i, err)
		}
		if candidate.ObservedAt.After(snapshot.AsOf) {
			return fmt.Errorf("invalid alert candidate at index %d: observed_at is after snapshot as_of", i)
		}
		if _, ok := expected[candidate.Source]; !ok {
			return fmt.Errorf("invalid alert candidate at index %d: source is outside snapshot coverage", i)
		}
		if candidate.EvidenceHealth == AlertEvidenceCurrent {
			if _, ok := covered[candidate.Source]; !ok {
				return fmt.Errorf("invalid alert candidate at index %d: current evidence source is not covered", i)
			}
		}
		if _, duplicate := seenEpisodes[candidate.EpisodeKey]; duplicate {
			return fmt.Errorf("invalid alert candidate at index %d: duplicate episode_key", i)
		}
		seenEpisodes[candidate.EpisodeKey] = struct{}{}
		if _, duplicate := seenOccurrences[candidate.OccurrenceKey]; duplicate {
			return fmt.Errorf("invalid alert candidate at index %d: duplicate occurrence_key", i)
		}
		seenOccurrences[candidate.OccurrenceKey] = struct{}{}
		if candidate.State == AlertEpisodeOpen || candidate.State == AlertEpisodeEscalated {
			hasActive = true
		}
	}

	derived := AlertSnapshotUnknown
	if hasActive {
		derived = AlertSnapshotActive
	} else if snapshot.Coverage.State == AlertCoverageComplete && snapshot.Coverage.Freshness == AlertCoverageCurrent {
		derived = AlertSnapshotClear
	}
	if snapshot.CurrentState != derived {
		return fmt.Errorf("alert candidate snapshot current_state %q does not match derived state %q", snapshot.CurrentState, derived)
	}
	return nil
}

// IsClear reports a trustworthy clear only for a fully valid snapshot. It is
// intentionally false for empty snapshots with incomplete or stale coverage.
func (snapshot AlertCandidateSnapshot) IsClear() bool {
	return snapshot.CurrentState == AlertSnapshotClear && snapshot.Validate() == nil
}

// MarshalJSON validates candidate before encoding it.
func (candidate AlertCandidate) MarshalJSON() ([]byte, error) {
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	type wire AlertCandidate
	return json.Marshal(wire(candidate))
}

// UnmarshalJSON rejects unknown or missing fields and validates the decoded
func (candidate *AlertCandidate) UnmarshalJSON(data []byte) error {
	type wire AlertCandidate
	var decoded wire
	if err := decodeExactAlertJSONObject(data, []string{
		"episode_key", "occurrence_key", "evidence_fingerprint", "source", "kind", "presentation_code", "state", "severity",
		"evidence_health", "destination", "evidence_as_of",
		"state_changed_at", "observed_at",
	}, &decoded); err != nil {
		return err
	}
	value := AlertCandidate(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*candidate = value
	return nil
}

// MarshalJSON validates source coverage before encoding it.
func (source AlertSourceCoverage) MarshalJSON() ([]byte, error) {
	// Snapshot validation supplies the authoritative upper time bound. Use the
	// row's latest known time here so standalone encoding still rejects shape
	// errors without inventing a later observation.
	latest := source.ObservedAt
	if source.EvidenceAsOf.After(latest) {
		latest = source.EvidenceAsOf
	}
	if latest.IsZero() {
		latest = time.Unix(0, 0).UTC()
	}
	if err := source.Validate(latest); err != nil {
		return nil, err
	}
	type wire AlertSourceCoverage
	return json.Marshal(wire(source))
}

// UnmarshalJSON rejects unknown or missing fields. Snapshot validation applies
func (source *AlertSourceCoverage) UnmarshalJSON(data []byte) error {
	type wire AlertSourceCoverage
	var decoded wire
	if err := decodeExactAlertJSONObject(data, []string{
		"source", "status", "reason", "evidence_health", "input_as_of", "observed_at", "evidence_as_of", "fresh_until", "covered",
	}, &decoded); err != nil {
		return err
	}
	value := AlertSourceCoverage(decoded)
	latest := value.ObservedAt
	if value.EvidenceAsOf.After(latest) {
		latest = value.EvidenceAsOf
	}
	if latest.IsZero() {
		latest = time.Unix(0, 0).UTC()
	}
	if err := value.Validate(latest); err != nil {
		return err
	}
	*source = value
	return nil
}

// MarshalJSON validates coverage before encoding it.
func (coverage AlertCoverage) MarshalJSON() ([]byte, error) {
	if err := coverage.Validate(); err != nil {
		return nil, err
	}
	type wire AlertCoverage
	return json.Marshal(wire(coverage))
}

// UnmarshalJSON rejects unknown or missing fields and validates the decoded
func (coverage *AlertCoverage) UnmarshalJSON(data []byte) error {
	type wire AlertCoverage
	var decoded wire
	if err := decodeExactAlertJSONObject(data, []string{
		"state", "freshness", "as_of", "expected_sources", "covered_sources",
	}, &decoded); err != nil {
		return err
	}
	value := AlertCoverage(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*coverage = value
	return nil
}

// MarshalJSON validates snapshot before encoding it.
func (snapshot AlertCandidateSnapshot) MarshalJSON() ([]byte, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, err
	}
	type wire AlertCandidateSnapshot
	return json.Marshal(wire(snapshot))
}

// UnmarshalJSON rejects unknown or missing fields and validates the decoded
func (snapshot *AlertCandidateSnapshot) UnmarshalJSON(data []byte) error {
	type wire AlertCandidateSnapshot
	var decoded wire
	if err := decodeExactAlertJSONObject(data, []string{
		"schema_version", "authority_scope", "as_of", "current_state", "coverage", "sources", "candidates",
	}, &decoded); err != nil {
		return err
	}
	value := AlertCandidateSnapshot(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*snapshot = value
	return nil
}

func alertSourceSet(field string, sources []AlertSource) (map[AlertSource]struct{}, error) {
	set := make(map[AlertSource]struct{}, len(sources))
	for _, source := range sources {
		if !validAlertSource(source) {
			return nil, fmt.Errorf("alert coverage %s contains invalid source", field)
		}
		if _, duplicate := set[source]; duplicate {
			return nil, fmt.Errorf("alert coverage %s contains duplicate source", field)
		}
		set[source] = struct{}{}
	}
	return set, nil
}

func validOpaqueSHA256(value, prefix string) bool {
	if len(value) != len(prefix)+sha256.Size*2 || !strings.HasPrefix(value, prefix) {
		return false
	}
	for i := len(prefix); i < len(value); i++ {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

func validAlertSource(value AlertSource) bool {
	switch value {
	case AlertSourceStress, AlertSourceRegime, AlertSourceRulebook, AlertSourceRiskPolicy,
		AlertSourceProtection, AlertSourceOrderIntegrity, AlertSourceReconciliation,
		AlertSourceGovernance, AlertSourceDataHealth, AlertSourceDelivery:
		return true
	default:
		return false
	}
}

func validAlertKind(value AlertKind) bool {
	switch value {
	case AlertKindMarketState, AlertKindPortfolioRisk, AlertKindMarginSafety, AlertKindDrawdown,
		AlertKindProtectionGap, AlertKindOrderIntegrity, AlertKindReconciliationException,
		AlertKindGovernance, AlertKindPolicyDrift, AlertKindDataHealth, AlertKindDeliveryHealth:
		return true
	default:
		return false
	}
}

func validAlertEpisodeState(value AlertEpisodeState) bool {
	return value == AlertEpisodeOpen || value == AlertEpisodeEscalated || value == AlertEpisodeRecovered
}

func validAlertSeverity(value AlertSeverity) bool {
	return value == AlertSeverityObserve || value == AlertSeverityWatch || value == AlertSeverityAct || value == AlertSeverityUrgent
}

func validAlertPresentationCode(source AlertSource, value AlertPresentationCode) bool {
	switch source {
	case AlertSourceStress:
		return value == AlertPresentationPortfolioStress || value == AlertPresentationMarginCushion
	case AlertSourceRegime:
		return value == AlertPresentationRegimeMarketStress
	case AlertSourceRulebook:
		switch value {
		case AlertPresentationRulebookSingleNameExposure, AlertPresentationRulebookOptionLinePremium,
			AlertPresentationRulebookCashSellOnly, AlertPresentationRulebookExtrinsicBudget,
			AlertPresentationRulebookExpiryRunway, AlertPresentationRulebookCatalystCoverage,
			AlertPresentationRulebookOverwriteEarnings, AlertPresentationRulebookEarningsSizeFreeze,
			AlertPresentationRulebookRedOnGreen, AlertPresentationRulebookWinnerTrim,
			AlertPresentationRulebookGreenDayAction, AlertPresentationRulebookHedgeIntegrity,
			AlertPresentationRulebookExitDiscipline, AlertPresentationRulebookFXExposure,
			AlertPresentationRulebookLegacyCondition:
			return true
		}
	case AlertSourceRiskPolicy:
		return value == AlertPresentationRiskPolicyLimitWouldBlock || value == AlertPresentationRiskPolicyDrawdownLatched ||
			value == AlertPresentationRiskPolicyDrift || value == AlertPresentationRiskPolicyLegacyCondition
	case AlertSourceProtection:
		return value == AlertPresentationProtectionOrphanedOrder || value == AlertPresentationProtectionReconciliationRequired
	case AlertSourceOrderIntegrity:
		return value == AlertPresentationOrderIntegrityMismatch
	case AlertSourceReconciliation:
		return value == AlertPresentationReconciliationDue || value == AlertPresentationReconciliationException ||
			value == AlertPresentationReconciliationConfirmedFlow || value == AlertPresentationReconciliationLegacyCondition
	case AlertSourceGovernance:
		return value == AlertPresentationGovernanceMonthlyPulse || value == AlertPresentationGovernanceLegacyCondition
	case AlertSourceDataHealth:
		switch value {
		case AlertPresentationDataHealthGateway, AlertPresentationDataHealthStorage,
			AlertPresentationDataHealthProposals, AlertPresentationDataHealthOpportunities,
			AlertPresentationDataHealthDataFarms, AlertPresentationDataHealthRegime,
			AlertPresentationDataHealthGamma, AlertPresentationDataHealthQuality:
			return true
		}
	case AlertSourceDelivery:
		return value == AlertPresentationDeliveryHealth
	}
	return false
}

func validAlertEvidenceHealth(value AlertEvidenceHealth) bool {
	return value == AlertEvidenceCurrent || value == AlertEvidencePartial || value == AlertEvidenceStale ||
		value == AlertEvidenceUnavailable || value == AlertEvidenceError
}

func validAlertDestination(value AlertDestination) bool {
	return value == AlertDestinationMonitor || value == AlertDestinationAlerts || value == AlertDestinationBrief
}

func validAlertCoverageState(value AlertCoverageState) bool {
	return value == AlertCoverageComplete || value == AlertCoveragePartial || value == AlertCoverageUnavailable
}

func validAlertCoverageFreshness(value AlertCoverageFreshness) bool {
	return value == AlertCoverageCurrent || value == AlertCoverageStale || value == AlertCoverageUnknown
}

func validAlertCode(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for i := 1; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func decodeExactAlertJSONObject(data []byte, requiredKeys []string, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("alert JSON value must be an object")
	}
	required := make(map[string]struct{}, len(requiredKeys))
	for _, key := range requiredKeys {
		required[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(requiredKeys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("alert JSON object contains a non-string key")
		}
		if _, ok := required[key]; !ok {
			return fmt.Errorf("alert JSON object contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("alert JSON object contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("alert JSON object key %q must not be null", key)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("alert JSON object is not closed")
	}
	for _, key := range requiredKeys {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("alert JSON object is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing alert JSON value")
		}
		return err
	}
	return json.Unmarshal(data, destination)
}
