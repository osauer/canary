package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
)

const (
	// MethodNudgesSnapshot returns the current redacted advisory snapshot.
	MethodNudgesSnapshot = "nudges.snapshot"
	// MethodNudgesCutoverReview records authenticated paired-device review
	// evidence; it does not complete a pulse or authorize broker activity.
	MethodNudgesCutoverReview = "nudges.cutover_review"
)

// NudgeCutoverReviewOrigin identifies the authenticated surface supplying
// cutover-review evidence.
type NudgeCutoverReviewOrigin string

// NudgeCutoverReviewOriginPairedDevice is the only accepted review origin.
const NudgeCutoverReviewOriginPairedDevice NudgeCutoverReviewOrigin = "paired_device"

// NudgeCutoverReviewEvidence identifies the fixed evidence class recorded by
// a cutover review.
type NudgeCutoverReviewEvidence string

// NudgeCutoverReviewEvidencePairedDeviceForegroundRender means the paired
// device visibly rendered the review in the foreground.
const NudgeCutoverReviewEvidencePairedDeviceForegroundRender NudgeCutoverReviewEvidence = "paired_device_foreground_render_review"

// Nudge candidate kinds, states, severities, destinations, and the blocking
// drawdown tier mirror the pure risk contract.
const (
	NudgeKindReconcileDue       = risk.NudgeKindReconcileDue
	NudgeKindReconcileException = risk.NudgeKindReconcileException
	NudgeKindShadowWouldBlock   = risk.NudgeKindShadowWouldBlock
	NudgeKindDrawdownLatched    = risk.NudgeKindDrawdownLatched
	NudgeKindPolicyDrift        = risk.NudgeKindPolicyDrift
	NudgeKindConfirmedFlow      = risk.NudgeKindConfirmedFlow
	NudgeKindMonthlyPulse       = risk.NudgeKindMonthlyPulse

	NudgeStateDueSoon  = risk.NudgeStateDueSoon
	NudgeStateOverdue  = risk.NudgeStateOverdue
	NudgeStateOpen     = risk.NudgeStateOpen
	NudgeStateObserved = risk.NudgeStateObserved
	NudgeStateDue      = risk.NudgeStateDue

	NudgeSeverityWatch = risk.NudgeSeverityWatch
	NudgeSeverityAct   = risk.NudgeSeverityAct

	NudgeDestinationMonitor = risk.NudgeDestinationMonitor
	NudgeDestinationAlerts  = risk.NudgeDestinationAlerts
	NudgeDestinationBrief   = risk.NudgeDestinationBrief

	NudgeDrawdownTierBlock = risk.CapitalTierBlock
)

// Nudge input and aggregate health values distinguish ready evidence from
// inactive, suppressed, stale, unavailable, and erroneous inputs.
const (
	NudgeInputStatusOK          = "ok"
	NudgeInputStatusInactive    = "inactive"
	NudgeInputStatusUnapproved  = "unapproved"
	NudgeInputStatusStale       = "stale"
	NudgeInputStatusUnavailable = "unavailable"
	NudgeInputStatusError       = "error"

	NudgeAggregateReady      = "ready"
	NudgeAggregateSuppressed = "suppressed"
	NudgeAggregateDegraded   = "degraded"
)

// Nudge source-health reasons are allowlisted tokens. Raw errors, paths,
// upstream fingerprints, and broker text do not belong on this contract.
const (
	NudgeHealthReasonNone                       = ""
	NudgeHealthReasonPolicyUnapproved           = "policy_unapproved"
	NudgeHealthReasonCadenceUnapproved          = "cadence_unapproved"
	NudgeHealthReasonEvidenceStale              = "evidence_stale"
	NudgeHealthReasonSourceUnavailable          = "source_unavailable"
	NudgeHealthReasonEvaluationError            = "evaluation_error"
	NudgeHealthReasonCoverageUnavailable        = "coverage_unavailable"
	NudgeHealthReasonCutoverReviewRequired      = "cutover_review_required"
	NudgeHealthReasonProcessRemindersNotEnabled = "process_reminders_not_enabled"
	NudgeHealthReasonInvalid                    = "invalid_health"
)

// NudgesSnapshotParams is empty because nudges.snapshot is a
// gateway-independent, side-effect-free read.
type NudgesSnapshotParams struct{}

// MarshalJSON emits the canonical empty object and never JSON null.
func (NudgesSnapshotParams) MarshalJSON() ([]byte, error) {
	return []byte("{}"), nil
}

// UnmarshalJSON accepts only an exact empty object.
func (params *NudgesSnapshotParams) UnmarshalJSON(data []byte) error {
	type wire NudgesSnapshotParams
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, nil, &decoded); err != nil {
		return err
	}
	*params = NudgesSnapshotParams(decoded)
	return nil
}

// NudgesCutoverReviewParams carries only the fixed paired-surface evidence
// labels. The daemon, not this DTO, authenticates the origin and authorizes
// the advisory evidence write against current broker-backed report health.
type NudgesCutoverReviewParams struct {
	Origin   NudgeCutoverReviewOrigin   `json:"origin"`
	Evidence NudgeCutoverReviewEvidence `json:"evidence"`
}

// MarshalJSON validates the fixed origin and evidence values before encoding.
func (params NudgesCutoverReviewParams) MarshalJSON() ([]byte, error) {
	if err := validateNudgesCutoverReviewParams(params); err != nil {
		return nil, err
	}
	type wire NudgesCutoverReviewParams
	return json.Marshal(wire(params))
}

// UnmarshalJSON rejects null, missing, duplicate, or unknown fields and
// validates the resulting review evidence.
func (params *NudgesCutoverReviewParams) UnmarshalJSON(data []byte) error {
	type wire NudgesCutoverReviewParams
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, []string{"origin", "evidence"}, &decoded); err != nil {
		return err
	}
	value := NudgesCutoverReviewParams(decoded)
	if err := validateNudgesCutoverReviewParams(value); err != nil {
		return err
	}
	*params = value
	return nil
}

func validateNudgesCutoverReviewParams(params NudgesCutoverReviewParams) error {
	if params.Origin != NudgeCutoverReviewOriginPairedDevice {
		return errors.New("invalid nudge cutover-review origin")
	}
	if params.Evidence != NudgeCutoverReviewEvidencePairedDeviceForegroundRender {
		return errors.New("invalid nudge cutover-review evidence")
	}
	return nil
}

// NudgesCutoverReviewResult reports only daemon-authored, redacted evidence.
// It is neither broker authority nor monthly-pulse completion.
type NudgesCutoverReviewResult struct {
	OK              bool                       `json:"ok"`
	AlreadyReviewed bool                       `json:"already_reviewed"`
	ReviewedAt      time.Time                  `json:"reviewed_at"`
	CoverageFrom    time.Time                  `json:"coverage_from"`
	Evidence        NudgeCutoverReviewEvidence `json:"evidence"`
}

// MarshalJSON validates success, timestamps, and evidence before encoding.
func (result NudgesCutoverReviewResult) MarshalJSON() ([]byte, error) {
	if err := validateNudgesCutoverReviewResult(result); err != nil {
		return nil, err
	}
	type wire NudgesCutoverReviewResult
	return json.Marshal(wire(result))
}

// UnmarshalJSON accepts only the exact validated result shape.
func (result *NudgesCutoverReviewResult) UnmarshalJSON(data []byte) error {
	type wire NudgesCutoverReviewResult
	var decoded wire
	if err := decodeExactNudgeJSONObject(data, []string{"ok", "already_reviewed", "reviewed_at", "coverage_from", "evidence"}, &decoded); err != nil {
		return err
	}
	value := NudgesCutoverReviewResult(decoded)
	if err := validateNudgesCutoverReviewResult(value); err != nil {
		return err
	}
	*result = value
	return nil
}

func validateNudgesCutoverReviewResult(result NudgesCutoverReviewResult) error {
	if !result.OK {
		return errors.New("nudge cutover-review result must be successful")
	}
	if result.ReviewedAt.IsZero() {
		return errors.New("nudge cutover-review result is missing reviewed_at")
	}
	if result.CoverageFrom.IsZero() {
		return errors.New("nudge cutover-review result is missing coverage_from")
	}
	if result.CoverageFrom.After(result.ReviewedAt) {
		return errors.New("nudge cutover-review coverage_from is after reviewed_at")
	}
	if result.Evidence != NudgeCutoverReviewEvidencePairedDeviceForegroundRender {
		return errors.New("invalid nudge cutover-review result evidence")
	}
	return nil
}

func decodeExactNudgeJSONObject(data []byte, allowedKeys []string, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("nudge JSON value must be an object")
	}

	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(allowedKeys))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("nudge JSON object contains a non-string key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("nudge JSON object contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("nudge JSON object contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("nudge JSON object key %q must not be null", key)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("nudge JSON object is not closed")
	}
	for _, key := range allowedKeys {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("nudge JSON object is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing nudge JSON value")
		}
		return err
	}
	return json.Unmarshal(data, destination)
}

// NudgeCandidate is intentionally lockscreen-safe. Its title and body are
// daemon-authored enum templates; Fingerprint is an opaque semantic identity.
type NudgeCandidate struct {
	Fingerprint string    `json:"fingerprint"`
	Kind        string    `json:"kind"`
	State       string    `json:"state"`
	Severity    string    `json:"severity"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	OccurredAt  time.Time `json:"occurred_at,omitzero"`
	DueAt       time.Time `json:"due_at,omitzero"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	Destination string    `json:"destination"`
}

// NudgeInputHealth reports one input's allowlisted status and reason. A zero
// AsOf is invalid and is normalized to error rather than treated as current.
type NudgeInputHealth struct {
	Status string    `json:"status"` // ok | unapproved | stale | unavailable | error
	Reason string    `json:"reason,omitempty"`
	AsOf   time.Time `json:"as_of,omitzero"`
}

// NudgeSourceHealth is separate from app polling/relay health. Fixed fields
// prevent generic notes, raw fingerprints, or unknown source names from
// widening the wire contract.
type NudgeSourceHealth struct {
	Aggregate      string           `json:"aggregate"` // ready | suppressed | degraded
	Policy         NudgeInputHealth `json:"policy"`
	Reconciliation NudgeInputHealth `json:"reconciliation"`
	Capital        NudgeInputHealth `json:"capital"`
	Pins           NudgeInputHealth `json:"pins"`
	Cadence        NudgeInputHealth `json:"cadence"`
	ConfirmedFlow  NudgeInputHealth `json:"confirmed_flow"`
}

type nudgeHealthSource uint8

const (
	nudgeHealthSourcePolicy nudgeHealthSource = iota
	nudgeHealthSourceReconciliation
	nudgeHealthSourceCapital
	nudgeHealthSourcePins
	nudgeHealthSourceCadence
	nudgeHealthSourceConfirmedFlow
)

// AggregateNudgeSourceHealth makes an empty result reassuring only when every
// relevant authority is ready. Partial coverage with safe candidates is
// degraded; the same coverage gap with no candidates is suppressed.
func AggregateNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) string {
	return NormalizeNudgeSourceHealth(health, candidateCount).Aggregate
}

// NormalizeNudgeSourceHealth is the mandatory wire boundary. It removes raw
// or incoherent status/reason values, preserves missing timestamps as missing,
// and derives Aggregate rather than trusting caller-provided state.
func NormalizeNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) NudgeSourceHealth {
	health.Policy = normalizeNudgeInputHealth(health.Policy, nudgeHealthSourcePolicy)
	health.Reconciliation = normalizeNudgeInputHealth(health.Reconciliation, nudgeHealthSourceReconciliation)
	health.Capital = normalizeNudgeInputHealth(health.Capital, nudgeHealthSourceCapital)
	health.Pins = normalizeNudgeInputHealth(health.Pins, nudgeHealthSourcePins)
	health.Cadence = normalizeNudgeInputHealth(health.Cadence, nudgeHealthSourceCadence)
	health.ConfirmedFlow = normalizeNudgeInputHealth(health.ConfirmedFlow, nudgeHealthSourceConfirmedFlow)
	health.Aggregate = aggregateNormalizedNudgeSourceHealth(health, candidateCount)
	return health
}

func normalizeNudgeInputHealth(health NudgeInputHealth, source nudgeHealthSource) NudgeInputHealth {
	validPair := false
	if !health.AsOf.IsZero() {
		switch health.Status {
		case NudgeInputStatusOK:
			validPair = health.Reason == NudgeHealthReasonNone
		case NudgeInputStatusInactive:
			validPair = (source == nudgeHealthSourcePolicy || source == nudgeHealthSourceCadence || source == nudgeHealthSourceConfirmedFlow) &&
				health.Reason == NudgeHealthReasonProcessRemindersNotEnabled
		case NudgeInputStatusUnapproved:
			validPair = health.Reason == NudgeHealthReasonPolicyUnapproved ||
				health.Reason == NudgeHealthReasonCadenceUnapproved ||
				(source == nudgeHealthSourceConfirmedFlow && health.Reason == NudgeHealthReasonCutoverReviewRequired)
		case NudgeInputStatusStale:
			validPair = health.Reason == NudgeHealthReasonEvidenceStale
		case NudgeInputStatusUnavailable:
			validPair = health.Reason == NudgeHealthReasonSourceUnavailable ||
				health.Reason == NudgeHealthReasonCoverageUnavailable
		case NudgeInputStatusError:
			validPair = health.Reason == NudgeHealthReasonEvaluationError || health.Reason == NudgeHealthReasonInvalid
		}
	}
	if !validPair {
		health.Status = NudgeInputStatusError
		health.Reason = NudgeHealthReasonInvalid
	}
	return health
}

func aggregateNormalizedNudgeSourceHealth(health NudgeSourceHealth, candidateCount int) string {
	inputs := [...]NudgeInputHealth{
		health.Policy,
		health.Reconciliation,
		health.Capital,
		health.Pins,
		health.Cadence,
		health.ConfirmedFlow,
	}
	allReady := true
	for _, input := range inputs {
		if input.Status != NudgeInputStatusOK &&
			!(input.Status == NudgeInputStatusInactive && input.Reason == NudgeHealthReasonProcessRemindersNotEnabled) {
			allReady = false
			break
		}
	}
	if allReady {
		return NudgeAggregateReady
	}
	if candidateCount <= 0 {
		return NudgeAggregateSuppressed
	}
	return NudgeAggregateDegraded
}

type nudgeSourceHealthWire NudgeSourceHealth

// MarshalJSON prevents standalone source-health values from carrying a false
// ready aggregate. Without result candidate context, partial health is
// conservatively suppressed.
func (health NudgeSourceHealth) MarshalJSON() ([]byte, error) {
	normalized := NormalizeNudgeSourceHealth(health, 0)
	return json.Marshal(nudgeSourceHealthWire(normalized))
}

// NudgesSnapshotResult is the daemon-authored advisory nudge snapshot. An
// empty Candidates slice is reassuring only when SourceHealth is ready.
type NudgesSnapshotResult struct {
	AsOf                  time.Time                   `json:"as_of"`
	Candidates            []NudgeCandidate            `json:"candidates"`
	SourceHealth          NudgeSourceHealth           `json:"source_health"`
	Reconciliation        *ReconAutomationStatus      `json:"reconciliation,omitempty"`
	ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
	Context               *NudgeSnapshotContext       `json:"context,omitempty"`
}

// NudgeSnapshotContext is visible snapshot detail, never candidate or push
// copy. Its concrete summaries deliberately admit no arbitrary display text.
type NudgeSnapshotContext struct {
	Shadow   *NudgeShadowSummary   `json:"shadow,omitempty"`
	Drawdown *NudgeDrawdownSummary `json:"drawdown,omitempty"`
}

// NudgeShadowSummary reports the redacted number of shadow findings.
type NudgeShadowSummary struct {
	Count int `json:"count"`
}

// NudgeDrawdownSummary reports the active tier and optional consumption. A nil
// percentage means the value is unavailable, not zero.
type NudgeDrawdownSummary struct {
	Tier        string   `json:"tier"`
	ConsumedPct *float64 `json:"consumed_pct"`
}

// NudgeConfirmedFlowCoverage discloses only the redacted cutover boundary and
// whether flows before that boundary still require review.
type NudgeConfirmedFlowCoverage struct {
	CoverageFrom              time.Time `json:"coverage_from"`
	PreCutoverFlowsUnreviewed bool      `json:"pre_cutover_flows_unreviewed"`
}

// MarshalJSON validates and canonicalizes health and candidates before
// encoding the snapshot.
func (result NudgesSnapshotResult) MarshalJSON() ([]byte, error) {
	normalizedHealth, candidates, err := validateNudgeSnapshot(result)
	if err != nil {
		return nil, err
	}
	wire := struct {
		AsOf                  time.Time                   `json:"as_of"`
		Candidates            []NudgeCandidate            `json:"candidates"`
		SourceHealth          nudgeSourceHealthWire       `json:"source_health"`
		Reconciliation        *ReconAutomationStatus      `json:"reconciliation,omitempty"`
		ConfirmedFlowCoverage *NudgeConfirmedFlowCoverage `json:"confirmed_flow_coverage,omitempty"`
		Context               *NudgeSnapshotContext       `json:"context,omitempty"`
	}{
		AsOf:                  result.AsOf,
		Candidates:            candidates,
		SourceHealth:          nudgeSourceHealthWire(normalizedHealth),
		Reconciliation:        result.Reconciliation,
		ConfirmedFlowCoverage: result.ConfirmedFlowCoverage,
		Context:               result.Context,
	}
	return json.Marshal(wire)
}

// IsCleanEmpty reports whether the snapshot is valid, fully covered, and has
// neither candidates nor contextual findings.
func (result NudgesSnapshotResult) IsCleanEmpty() bool {
	normalized, candidates, err := validateNudgeSnapshot(result)
	if err != nil {
		return false
	}
	return len(candidates) == 0 && result.Context == nil && normalized.Aggregate == NudgeAggregateReady
}

func validateNudgeSnapshot(result NudgesSnapshotResult) (NudgeSourceHealth, []NudgeCandidate, error) {
	normalizedHealth := NormalizeNudgeSourceHealth(result.SourceHealth, len(result.Candidates))
	if result.Reconciliation != nil {
		if err := ValidateReconAutomationStatus(*result.Reconciliation); err != nil {
			return NudgeSourceHealth{}, nil, err
		}
	}
	if err := validateNudgeSnapshotConfirmedFlowCoherence(result.AsOf, result.ConfirmedFlowCoverage, normalizedHealth.ConfirmedFlow); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	if err := validateNudgeSnapshotSourceHealthTimestamps(result.AsOf, normalizedHealth); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	candidates := make([]NudgeCandidate, len(result.Candidates))
	for i, candidate := range result.Candidates {
		canonical, err := canonicalizeRPCNudgeCandidate(candidate)
		if err != nil {
			return NudgeSourceHealth{}, nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		if err := validateNudgeCandidateSnapshotTime(result.AsOf, canonical); err != nil {
			return NudgeSourceHealth{}, nil, fmt.Errorf("invalid nudge candidate at index %d: %w", i, err)
		}
		candidates[i] = canonical
	}
	if err := validateNudgeSnapshotContext(result.Context, candidates); err != nil {
		return NudgeSourceHealth{}, nil, err
	}
	return normalizedHealth, candidates, nil
}

func validateNudgeCandidateSnapshotTime(asOf time.Time, candidate NudgeCandidate) error {
	if candidate.OccurredAt.After(asOf) {
		return errors.New("nudge candidate occurrence time is after snapshot as_of")
	}
	switch {
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateDueSoon:
		if candidate.DueAt.Before(asOf) {
			return errors.New("reconcile due-soon deadline is before snapshot as_of")
		}
	case candidate.Kind == NudgeKindReconcileDue && candidate.State == NudgeStateOverdue:
		if candidate.DueAt.After(asOf) {
			return errors.New("reconcile overdue deadline is after snapshot as_of")
		}
	case candidate.Kind == NudgeKindMonthlyPulse && candidate.State == NudgeStateDue:
		if candidate.DueAt.After(asOf) {
			return errors.New("monthly pulse deadline is after snapshot as_of")
		}
	}
	return nil
}

func validateNudgeSnapshotContext(context *NudgeSnapshotContext, candidates []NudgeCandidate) error {
	if context != nil && context.Shadow == nil && context.Drawdown == nil {
		return errors.New("nudge snapshot context is empty")
	}
	shadowCandidates := 0
	drawdownCandidates := 0
	for _, candidate := range candidates {
		switch {
		case candidate.Kind == NudgeKindShadowWouldBlock && candidate.State == NudgeStateObserved:
			shadowCandidates++
		case candidate.Kind == NudgeKindDrawdownLatched && candidate.State == NudgeStateOpen:
			drawdownCandidates++
		}
	}
	if shadowCandidates > 1 {
		return errors.New("nudge snapshot has duplicate shadow context candidates")
	}
	if drawdownCandidates > 1 {
		return errors.New("nudge snapshot has duplicate drawdown context candidates")
	}

	var shadow *NudgeShadowSummary
	var drawdown *NudgeDrawdownSummary
	if context != nil {
		shadow = context.Shadow
		drawdown = context.Drawdown
	}
	if (shadowCandidates == 1) != (shadow != nil) {
		return errors.New("nudge snapshot shadow summary and candidate are incoherent")
	}
	if shadow != nil && shadow.Count < 1 {
		return errors.New("nudge snapshot shadow count must be positive")
	}
	if (drawdownCandidates == 1) != (drawdown != nil) {
		return errors.New("nudge snapshot drawdown summary and candidate are incoherent")
	}
	if drawdown != nil {
		if drawdown.Tier != NudgeDrawdownTierBlock {
			return errors.New("nudge snapshot drawdown tier must be block")
		}
		if drawdown.ConsumedPct != nil && (math.IsNaN(*drawdown.ConsumedPct) || math.IsInf(*drawdown.ConsumedPct, 0) || *drawdown.ConsumedPct < 0) {
			return errors.New("nudge snapshot drawdown consumed_pct must be finite and non-negative")
		}
	}
	return nil
}

func validateNudgeSnapshotSourceHealthTimestamps(asOf time.Time, health NudgeSourceHealth) error {
	inputs := [...]struct {
		name string
		asOf time.Time
	}{
		{name: "policy", asOf: health.Policy.AsOf},
		{name: "reconciliation", asOf: health.Reconciliation.AsOf},
		{name: "capital", asOf: health.Capital.AsOf},
		{name: "pins", asOf: health.Pins.AsOf},
		{name: "cadence", asOf: health.Cadence.AsOf},
		{name: "confirmed_flow", asOf: health.ConfirmedFlow.AsOf},
	}
	for _, input := range inputs {
		if !input.asOf.IsZero() && input.asOf.After(asOf) {
			return fmt.Errorf("nudge snapshot %s source health is after as_of", input.name)
		}
	}
	return nil
}

func validateNudgeSnapshotConfirmedFlowCoherence(
	asOf time.Time,
	coverage *NudgeConfirmedFlowCoverage,
	confirmedFlowHealth NudgeInputHealth,
) error {
	if asOf.IsZero() {
		return errors.New("nudge snapshot is missing as_of")
	}
	if coverage == nil {
		if confirmedFlowHealth.Status == NudgeInputStatusOK {
			return errors.New("nudge snapshot has ready confirmed-flow health without coverage")
		}
		return nil
	}
	if coverage.CoverageFrom.IsZero() {
		return errors.New("nudge snapshot confirmed-flow coverage is missing coverage_from")
	}
	if coverage.CoverageFrom.After(asOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is after as_of")
	}
	if !confirmedFlowHealth.AsOf.IsZero() && coverage.CoverageFrom.After(confirmedFlowHealth.AsOf) {
		return errors.New("nudge snapshot confirmed-flow coverage is newer than source health")
	}
	if coverage.PreCutoverFlowsUnreviewed {
		if confirmedFlowHealth.Status != NudgeInputStatusUnapproved || confirmedFlowHealth.Reason != NudgeHealthReasonCutoverReviewRequired {
			return errors.New("nudge snapshot unreviewed confirmed-flow coverage has incoherent source health")
		}
		return nil
	}
	if confirmedFlowHealth.Reason == NudgeHealthReasonCutoverReviewRequired {
		return errors.New("nudge snapshot reviewed confirmed-flow coverage still requires cutover review")
	}
	return nil
}

func canonicalizeRPCNudgeCandidate(candidate NudgeCandidate) (NudgeCandidate, error) {
	canonical, err := risk.CanonicalizeNudgeCandidate(risk.NudgeCandidate{
		Fingerprint: candidate.Fingerprint,
		Kind:        candidate.Kind,
		State:       candidate.State,
		Severity:    candidate.Severity,
		Title:       candidate.Title,
		Body:        candidate.Body,
		OccurredAt:  candidate.OccurredAt,
		DueAt:       candidate.DueAt,
		ExpiresAt:   candidate.ExpiresAt,
		Destination: candidate.Destination,
	})
	if err != nil {
		return NudgeCandidate{}, err
	}
	return NudgeCandidate{
		Fingerprint: canonical.Fingerprint,
		Kind:        canonical.Kind,
		State:       canonical.State,
		Severity:    canonical.Severity,
		Title:       canonical.Title,
		Body:        canonical.Body,
		OccurredAt:  canonical.OccurredAt,
		DueAt:       canonical.DueAt,
		ExpiresAt:   canonical.ExpiresAt,
		Destination: canonical.Destination,
	}, nil
}
