package rpc

import "time"

// MethodAlertCandidates exposes the daemon-authored, source-neutral alert
// candidate snapshot. The method is observational: it has no delivery target,
// acknowledgement, policy-change, or broker-write authority.
const MethodAlertCandidates = "alerts.candidates"

// MethodAlertStatus exposes redacted coverage and lifecycle measurements. It
// deliberately carries no candidate, account, order, or delivery-target identity.
const MethodAlertStatus = "alerts.status"

// AlertCandidatesParams is intentionally empty. Producers and their coverage
// universe are daemon-owned; callers cannot select sources or weaken evidence
// requirements through request parameters.
type AlertCandidatesParams struct{}

// AlertStatusParams is intentionally empty because scope and source coverage
// are daemon-owned.
type AlertStatusParams struct{}

// AlertStatusResult is the redacted, read-only operational view of the daemon
// alert registry. Measurements describe lifecycle behavior, not send policy.
type AlertStatusResult struct {
	AsOf                  time.Time           `json:"as_of,omitzero"`
	ExpectedSources       []AlertSource       `json:"expected_sources"`
	Evaluations           uint64              `json:"evaluations"`
	RegistryApplyFailures uint64              `json:"registry_apply_failures"`
	Equivocations         uint64              `json:"equivocations"`
	LastErrorCode         string              `json:"last_error_code,omitempty"`
	Sources               []AlertSourceStatus `json:"sources"`
}

// AlertSourceStatus reports coverage and lifecycle health for one
// allowlisted source without exposing candidate or account identities.
type AlertSourceStatus struct {
	Source            AlertSource            `json:"source"`
	Status            string                 `json:"status"`
	Reason            string                 `json:"reason"`
	AuthorityUniverse AlertAuthorityUniverse `json:"authority_universe,omitempty"`
	InputAsOf         time.Time              `json:"input_as_of,omitzero"`
	ObservedAt        time.Time              `json:"observed_at,omitzero"`
	Covered           bool                   `json:"covered"`
	// UncoveredRules lists canonical rule IDs whose per-rule coverage failed in
	// the source's latest evaluation while the source itself stayed covered.
	// Rule IDs are policy vocabulary — never candidate or account identity.
	UncoveredRules []string          `json:"uncovered_rules,omitempty"`
	Active         int               `json:"active_candidates"`
	Measurements   AlertMeasurements `json:"measurements"`
}

// AlertAuthorityUniverse names the exact evidence population over which a
// source may claim coverage. An empty value means the source does not expose a
// narrower population than its source contract.
type AlertAuthorityUniverse string

const (
	// AlertAuthorityUniverseJournaledAPIOrders limits Protection coverage to
	// daemon-journaled API orders checked against the all-client broker
	// inventory. It does not claim coverage over manual or unjournaled orders.
	AlertAuthorityUniverseJournaledAPIOrders AlertAuthorityUniverse = "daemon_journaled_api_orders_checked_against_all_client_inventory"
)

// AlertMeasurements contains cumulative, redacted lifecycle counts.
// Zero values mean no recorded observation, not successful coverage.
type AlertMeasurements struct {
	Evaluations              uint64  `json:"evaluations"`
	CoveredEvaluations       uint64  `json:"covered_evaluations"`
	ActiveEvaluations        uint64  `json:"active_evaluations"`
	ActiveObservations       uint64  `json:"active_observations"`
	EpisodesOpened           uint64  `json:"episodes_opened"`
	EpisodesEscalated        uint64  `json:"episodes_escalated"`
	EpisodesRecovered        uint64  `json:"episodes_recovered"`
	EpisodesReopened         uint64  `json:"episodes_reopened"`
	DuplicateInputs          uint64  `json:"duplicate_inputs"`
	DuplicateCandidates      uint64  `json:"duplicate_candidates"`
	RepeatedActive           uint64  `json:"repeated_active_observations"`
	ActiveEvidenceChurn      uint64  `json:"active_evidence_revisions"`
	Equivocations            uint64  `json:"equivocations"`
	StaleSuppressions        uint64  `json:"stale_suppressions"`
	CoverageFailures         uint64  `json:"coverage_failures"`
	TimeToObserveSamples     uint64  `json:"time_to_observe_samples"`
	TimeToObserveTotalSecond float64 `json:"time_to_observe_total_seconds"`
	TimeToObserveMaxSecond   float64 `json:"time_to_observe_max_seconds"`
}
