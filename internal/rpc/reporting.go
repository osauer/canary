package rpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Reporting RPC methods, schema versions, states, and stable reasons.
const (
	MethodReportingStatus   = "reporting.status"
	MethodReportingValidate = "reporting.validate"

	ReportingSchemaVersion = "canary-reporting-v1"

	ReportingStateConfigured     = "configured"
	ReportingStateBackfilling    = "backfilling"
	ReportingStateCurrent        = "current"
	ReportingStateActionRequired = "action_required"
	ReportingStateUnavailable    = "unavailable"

	ReportingEvidenceNotReceived = "not_received"
	ReportingEvidenceObserved    = "observed"
	ReportingEvidenceDegraded    = "degraded"

	ReportingReachabilityUnknown     = "unknown"
	ReportingReachabilityReachable   = "reachable"
	ReportingReachabilityUnreachable = "unreachable"

	ReportingValidationReady          = "ready"
	ReportingValidationUnproved       = "unproved"
	ReportingValidationActionRequired = "action_required"

	ReportingReasonTokenPermissions           = "token_permissions"
	ReportingReasonBrokerResponseUndocumented = "broker_response_undocumented"
	ReportingReasonFlexQueryIncomplete        = "flex_query_incomplete"
	ReportingReasonReportNotReceived          = "report_not_received"
	ReportingReasonEmptySectionsUnproved      = "empty_sections_unproved"
	ReportingReasonAccountScopeInvalid        = "report_account_scope_invalid"
	ReportingReasonAccountScopeMismatch       = "report_account_scope_mismatch"
)

// ReportingValidateParams references candidate credentials without carrying
// the Flex token over RPC. The result never echoes either value.
type ReportingValidateParams struct {
	QueryID   string `json:"query_id"`
	TokenPath string `json:"token_path"`
}

// ReportingValidationResult is the redacted outcome of one explicit
// side-effect-free candidate query check.
type ReportingValidationResult struct {
	SchemaVersion       string                        `json:"schema_version"`
	Outcome             string                        `json:"outcome"`
	Reason              string                        `json:"reason,omitempty"`
	ManifestVersion     string                        `json:"manifest_version"`
	BrokerCode          string                        `json:"broker_code,omitempty"`
	SchemaFingerprint   string                        `json:"schema_fingerprint,omitempty"`
	ReadyForRotation    bool                          `json:"ready_for_rotation"`
	Requirements        []ReportingSectionRequirement `json:"requirements"`
	MissingRequirements []string                      `json:"missing_requirements,omitempty"`
	UnprovedSections    []string                      `json:"unproved_sections,omitempty"`
	Action              string                        `json:"action,omitempty"`
}

// ReportingStatusResult is the shared, redacted broker-reporting diagnostic.
// It contains no token, Query ID, account identity, report value, or filename.
type ReportingStatusResult struct {
	SchemaVersion       string                        `json:"schema_version"`
	State               string                        `json:"state"`
	Reason              string                        `json:"reason,omitempty"`
	ManifestVersion     string                        `json:"manifest_version"`
	Local               ReportingLocalStatus          `json:"local"`
	Broker              ReportingBrokerStatus         `json:"broker"`
	Evidence            ReportingEvidenceStatus       `json:"evidence"`
	Requirements        []ReportingSectionRequirement `json:"requirements"`
	MissingRequirements []string                      `json:"missing_requirements,omitempty"`
	UnprovedSections    []string                      `json:"unproved_sections,omitempty"`
	Action              string                        `json:"action,omitempty"`
}

// ReportingLocalStatus describes only whether required local configuration
// exists and is private; it never carries credential values or paths.
type ReportingLocalStatus struct {
	Enabled          bool `json:"enabled"`
	QueryConfigured  bool `json:"query_configured"`
	TokenFilePresent bool `json:"token_file_present"`
	TokenFilePrivate bool `json:"token_file_private"`
}

// ReportingBrokerStatus describes the typed Flex acquisition state without
// request material, broker prose, or identifiers.
type ReportingBrokerStatus struct {
	State          string    `json:"state"`
	Reason         string    `json:"reason,omitempty"`
	Reachability   string    `json:"reachability"`
	BrokerCode     string    `json:"broker_code,omitempty"`
	LastSuccess    time.Time `json:"last_success,omitzero"`
	LastAttempt    time.Time `json:"last_attempt,omitzero"`
	NextAttempt    time.Time `json:"next_attempt,omitzero"`
	RetryAutomatic bool      `json:"retry_automatic"`
	Busy           bool      `json:"busy"`
}

// ReportingEvidenceStatus summarizes retained active-query evidence without
// statement values, account identity, or filenames.
type ReportingEvidenceStatus struct {
	State             string    `json:"state"`
	SchemaFingerprint string    `json:"schema_fingerprint,omitempty"`
	CoverageTo        time.Time `json:"coverage_to,omitzero"`
}

// ReportingSectionRequirement compares one canonical manifest section with
// observed broker XML using only allowlisted section and field names.
type ReportingSectionRequirement struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	Status        string   `json:"status"`
	LevelOfDetail string   `json:"level_of_detail,omitempty"`
	Fields        []string `json:"fields"`
	MissingFields []string `json:"missing_fields,omitempty"`
}

// MarshalJSON validates the reporting status contract before encoding it.
func (result ReportingStatusResult) MarshalJSON() ([]byte, error) {
	if err := ValidateReportingStatusResult(result); err != nil {
		return nil, err
	}
	type wire ReportingStatusResult
	return json.Marshal(wire(result))
}

// UnmarshalJSON rejects unknown top-level fields and invalid reporting status
// combinations.
func (result *ReportingStatusResult) UnmarshalJSON(data []byte) error {
	type wire ReportingStatusResult
	var decoded wire
	if err := decodeReportingJSONObject(data,
		[]string{"schema_version", "state", "manifest_version", "local", "broker", "evidence", "requirements"},
		[]string{"reason", "missing_requirements", "unproved_sections", "action"},
		&decoded,
	); err != nil {
		return err
	}
	value := ReportingStatusResult(decoded)
	if err := ValidateReportingStatusResult(value); err != nil {
		return err
	}
	*result = value
	return nil
}

// MarshalJSON validates the candidate result contract before encoding it.
func (result ReportingValidationResult) MarshalJSON() ([]byte, error) {
	if err := ValidateReportingValidationResult(result); err != nil {
		return nil, err
	}
	type wire ReportingValidationResult
	return json.Marshal(wire(result))
}

// UnmarshalJSON rejects unknown top-level fields and invalid candidate result
// combinations.
func (result *ReportingValidationResult) UnmarshalJSON(data []byte) error {
	type wire ReportingValidationResult
	var decoded wire
	if err := decodeReportingJSONObject(data,
		[]string{"schema_version", "outcome", "manifest_version", "ready_for_rotation", "requirements"},
		[]string{"reason", "broker_code", "schema_fingerprint", "missing_requirements", "unproved_sections", "action"},
		&decoded,
	); err != nil {
		return err
	}
	value := ReportingValidationResult(decoded)
	if err := ValidateReportingValidationResult(value); err != nil {
		return err
	}
	*result = value
	return nil
}

// decodeReportingJSONObject keeps the reporting wire closed to unknown,
// duplicate, and null fields while allowing fields tagged omitempty to be
// absent. Required fields remain mandatory so a partial result cannot become
// valid merely because its Go zero values happen to pass later checks.
func decodeReportingJSONObject(data []byte, requiredKeys, optionalKeys []string, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return errors.New("reporting JSON value must be an object")
	}

	allowed := make(map[string]struct{}, len(requiredKeys)+len(optionalKeys))
	for _, key := range requiredKeys {
		allowed[key] = struct{}{}
	}
	for _, key := range optionalKeys {
		allowed[key] = struct{}{}
	}
	seen := make(map[string]struct{}, len(allowed))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok {
			return errors.New("reporting JSON object contains a non-string key")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("reporting JSON object contains unknown key %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("reporting JSON object contains duplicate key %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("reporting JSON object key %q must not be null", key)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return errors.New("reporting JSON object is not closed")
	}
	for _, key := range requiredKeys {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("reporting JSON object is missing key %q", key)
		}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing reporting JSON value")
		}
		return err
	}
	return json.Unmarshal(data, destination)
}

// ValidateReportingStatusResult enforces the redacted reporting status wire
// contract.
func ValidateReportingStatusResult(result ReportingStatusResult) error {
	if result.SchemaVersion != ReportingSchemaVersion || !safeReportingToken(result.ManifestVersion) {
		return errors.New("invalid reporting schema contract")
	}
	switch result.State {
	case ReportingStateConfigured, ReportingStateBackfilling, ReportingStateCurrent,
		ReportingStateActionRequired, ReportingStateUnavailable:
	default:
		return errors.New("invalid reporting state")
	}
	if !validReportingReason(result.Reason) {
		return errors.New("invalid reporting reason")
	}
	if result.Local.TokenFilePrivate && !result.Local.TokenFilePresent {
		return errors.New("private reporting token file is not present")
	}
	if !validReportingBrokerState(result.Broker.State) || !validReportingBrokerReason(result.Broker.Reason) {
		return errors.New("invalid reporting broker status")
	}
	switch result.Broker.Reachability {
	case ReportingReachabilityUnknown, ReportingReachabilityReachable, ReportingReachabilityUnreachable:
	default:
		return errors.New("invalid reporting broker reachability")
	}
	if result.Broker.BrokerCode != "" && !validFlexBrokerCode(result.Broker.BrokerCode) {
		return errors.New("invalid reporting broker code")
	}
	if result.Broker.Busy != (result.Broker.State == ReconReportStateChecking) {
		return errors.New("reporting broker busy flag and state disagree")
	}
	switch result.Evidence.State {
	case ReportingEvidenceNotReceived:
		if result.Evidence.SchemaFingerprint != "" || !result.Evidence.CoverageTo.IsZero() {
			return errors.New("not-received reporting evidence carries observations")
		}
	case ReportingEvidenceObserved, ReportingEvidenceDegraded:
		if !strings.HasPrefix(result.Evidence.SchemaFingerprint, "flex_schema_") {
			return errors.New("observed reporting evidence lacks a schema fingerprint")
		}
	default:
		return errors.New("invalid reporting evidence state")
	}

	if err := validateReportingSectionEvidence(result.Requirements, result.MissingRequirements, result.UnprovedSections); err != nil {
		return err
	}
	if len(result.Action) > 500 || strings.ContainsAny(result.Action, "\r\n") {
		return errors.New("invalid reporting action")
	}
	return nil
}

// ValidateReportingValidationResult enforces the redacted candidate
// validation wire contract.
func ValidateReportingValidationResult(result ReportingValidationResult) error {
	if result.SchemaVersion != ReportingSchemaVersion || !safeReportingToken(result.ManifestVersion) {
		return errors.New("invalid reporting validation schema contract")
	}
	switch result.Outcome {
	case ReportingValidationReady, ReportingValidationUnproved, ReportingValidationActionRequired:
	default:
		return errors.New("invalid reporting validation outcome")
	}
	if !validReportingReason(result.Reason) {
		return errors.New("invalid reporting validation reason")
	}
	if result.BrokerCode != "" && !validFlexBrokerCode(result.BrokerCode) {
		return errors.New("invalid reporting validation broker code")
	}
	if result.SchemaFingerprint != "" && !strings.HasPrefix(result.SchemaFingerprint, "flex_schema_") {
		return errors.New("invalid reporting validation schema fingerprint")
	}
	if err := validateReportingSectionEvidence(result.Requirements, result.MissingRequirements, result.UnprovedSections); err != nil {
		return err
	}
	if result.ReadyForRotation != (result.Outcome == ReportingValidationReady || result.Outcome == ReportingValidationUnproved) {
		return errors.New("reporting validation rotation flag and outcome disagree")
	}
	if result.ReadyForRotation && (result.SchemaFingerprint == "" || len(result.MissingRequirements) > 0) {
		return errors.New("reporting validation marked an incomplete schema ready")
	}
	if result.Outcome == ReportingValidationReady && len(result.UnprovedSections) > 0 {
		return errors.New("ready reporting validation carries unproved sections")
	}
	if result.Outcome == ReportingValidationUnproved && len(result.UnprovedSections) == 0 {
		return errors.New("unproved reporting validation lacks unproved sections")
	}
	if len(result.Action) > 500 || strings.ContainsAny(result.Action, "\r\n") {
		return errors.New("invalid reporting validation action")
	}
	return nil
}

func validateReportingSectionEvidence(requirements []ReportingSectionRequirement, missingRequirements, unprovedSections []string) error {
	allowedMissing := make(map[string]bool)
	allowedSections := make(map[string]string)
	seenSections := make(map[string]bool)
	for _, section := range requirements {
		if !safeReportingToken(section.Key) || seenSections[section.Key] || strings.TrimSpace(section.Label) == "" || len(section.Fields) == 0 {
			return errors.New("invalid reporting section requirement")
		}
		seenSections[section.Key] = true
		allowedSections[section.Key] = section.Status
		switch section.Status {
		case "observed", "missing", "unproved", "not_received":
		default:
			return errors.New("invalid reporting section evidence")
		}
		fields := make(map[string]bool)
		for _, field := range section.Fields {
			if !safeReportingToken(field) || fields[field] {
				return errors.New("invalid reporting section field")
			}
			fields[field] = true
		}
		missingFields := make(map[string]bool)
		for _, field := range section.MissingFields {
			if !fields[field] || missingFields[field] || section.Status != "missing" {
				return errors.New("invalid reporting missing field")
			}
			missingFields[field] = true
		}
		if section.Status == "missing" {
			if len(section.MissingFields) == 0 {
				allowedMissing[section.Key] = true
			} else {
				for _, field := range section.MissingFields {
					allowedMissing[section.Key+"."+field] = true
				}
			}
		}
	}
	if len(requirements) == 0 {
		return errors.New("reporting requirements are empty")
	}
	seen := make(map[string]bool)
	for _, requirement := range missingRequirements {
		if !allowedMissing[requirement] || seen[requirement] {
			return errors.New("invalid reporting missing requirement")
		}
		seen[requirement] = true
	}
	seen = make(map[string]bool)
	for _, section := range unprovedSections {
		if allowedSections[section] != "unproved" || seen[section] {
			return errors.New("invalid reporting unproved section")
		}
		seen[section] = true
	}
	return nil
}

func validReportingBrokerState(state string) bool {
	switch state {
	case ReconReportStateWaiting, ReconReportStateDue, ReconReportStateChecking,
		ReconReportStateCurrent, ReconReportStateRetryScheduled,
		ReconReportStateActionRequired, ReconReportStateUnavailable:
		return true
	default:
		return false
	}
}

func validReportingBrokerReason(reason string) bool {
	switch reason {
	case ReconReportReasonNone, ReconReportReasonBeforeDailyWindow,
		ReconReportReasonCoveragePending, ReconReportReasonReportNotReady,
		ReconReportReasonServiceBusy, ReconReportReasonRateLimited,
		ReconReportReasonNetworkUnavailable, ReconReportReasonFlexDisabled,
		ReconReportReasonQueryMissing, ReconReportReasonTokenMissing,
		ReconReportReasonTokenInvalid, ReconReportReasonTokenExpired,
		ReconReportReasonQueryInvalid, ReconReportReasonIPRestricted,
		ReconReportReasonServiceInactive, ReconReportReasonResponseInvalid,
		ReconReportReasonReportInvalid, ReconReportReasonStorageFailed,
		ReconReportReasonProjectionFailed, ReconReportReasonAuthorityUnavailable:
		return true
	default:
		return false
	}
}

func validReportingReason(reason string) bool {
	if reason == "" || validReportingBrokerReason(reason) {
		return true
	}
	switch reason {
	case ReportingReasonTokenPermissions, ReportingReasonBrokerResponseUndocumented,
		ReportingReasonFlexQueryIncomplete, ReportingReasonReportNotReceived,
		ReportingReasonEmptySectionsUnproved, ReportingReasonAccountScopeInvalid,
		ReportingReasonAccountScopeMismatch:
		return true
	default:
		return false
	}
}

func safeReportingToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i := range len(value) {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}
