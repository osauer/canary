package rpc

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestReportingStatusRejectsUntrustedBrokerCode(t *testing.T) {
	t.Parallel()

	result := validReportingStatusFixture()
	result.Broker.BrokerCode = "1025 broker text"
	if err := ValidateReportingStatusResult(result); err == nil {
		t.Fatal("accepted broker text as a code")
	}
	result = validReportingStatusFixture()
	result.Broker.BrokerCode = "１２３４"
	if err := ValidateReportingStatusResult(result); err == nil {
		t.Fatal("accepted non-ASCII digits as a code")
	}
}

func TestReportingStatusRoundTripRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(validReportingStatusFixture())
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	object["token"] = "must-not-be-accepted"
	encoded, _ = json.Marshal(object)
	var decoded ReportingStatusResult
	if err := json.Unmarshal(encoded, &decoded); err == nil {
		t.Fatal("accepted unknown reporting field")
	}
}

func TestReportingValidationRequiresCoherentRotationEvidence(t *testing.T) {
	t.Parallel()

	result := ReportingValidationResult{
		SchemaVersion: ReportingSchemaVersion, Outcome: ReportingValidationUnproved,
		ManifestVersion: "canary-reporting-flex-v1", SchemaFingerprint: "flex_schema_1234567890abcdef",
		ReadyForRotation: true,
		Requirements:     []ReportingSectionRequirement{{Key: "trades", Label: "Trades", Status: "unproved", Fields: []string{"tradeID"}}},
		UnprovedSections: []string{"trades"},
	}
	if err := ValidateReportingValidationResult(result); err != nil {
		t.Fatal(err)
	}
	result.MissingRequirements = []string{"trades"}
	if err := ValidateReportingValidationResult(result); err == nil {
		t.Fatal("accepted missing requirement on an unproved section")
	}
}

func TestReportingValidationFetchFailureRoundTripsWithoutOptionalEvidence(t *testing.T) {
	t.Parallel()

	result := ReportingValidationResult{
		SchemaVersion: ReportingSchemaVersion, Outcome: ReportingValidationActionRequired,
		Reason: ReconReportReasonReportNotReady, ManifestVersion: "canary-reporting-flex-v1",
		BrokerCode: "1003",
		Requirements: []ReportingSectionRequirement{{
			Key: "trades", Label: "Trades", Status: "not_received", Fields: []string{"tradeID"},
		}},
		Action: "Keep the active query and retry candidate validation after the broker service recovers.",
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, omitted := range []string{"schema_fingerprint", "missing_requirements", "unproved_sections"} {
		if json.Valid(encoded) && bytes.Contains(encoded, []byte(`"`+omitted+`"`)) {
			t.Fatalf("optional field %q was not omitted: %s", omitted, encoded)
		}
	}
	var decoded ReportingValidationResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Outcome != ReportingValidationActionRequired || decoded.Reason != ReconReportReasonReportNotReady || decoded.BrokerCode != "1003" {
		t.Fatalf("decoded result = %+v", decoded)
	}
}

func TestReportingCurrentStatusRoundTripsWithoutOptionalDiagnostics(t *testing.T) {
	t.Parallel()

	result := ReportingStatusResult{
		SchemaVersion: ReportingSchemaVersion, State: ReportingStateCurrent,
		ManifestVersion: "canary-reporting-flex-v1",
		Local:           ReportingLocalStatus{Enabled: true, QueryConfigured: true, TokenFilePresent: true, TokenFilePrivate: true},
		Broker: ReportingBrokerStatus{
			State: ReconReportStateCurrent, Reachability: ReportingReachabilityReachable,
		},
		Evidence: ReportingEvidenceStatus{State: ReportingEvidenceObserved, SchemaFingerprint: "flex_schema_1234567890abcdef"},
		Requirements: []ReportingSectionRequirement{{
			Key: "trades", Label: "Trades", Status: "observed", Fields: []string{"tradeID"},
		}},
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReportingStatusResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != ReportingStateCurrent || decoded.Reason != "" || decoded.Action != "" {
		t.Fatalf("decoded result = %+v", decoded)
	}
}

func validReportingStatusFixture() ReportingStatusResult {
	return ReportingStatusResult{
		SchemaVersion: ReportingSchemaVersion, State: ReportingStateActionRequired,
		Reason: ReportingReasonBrokerResponseUndocumented, ManifestVersion: "canary-reporting-flex-v1",
		Local: ReportingLocalStatus{Enabled: true, QueryConfigured: true, TokenFilePresent: true, TokenFilePrivate: true},
		Broker: ReportingBrokerStatus{
			State: ReconReportStateRetryScheduled, Reason: ReconReportReasonResponseInvalid,
			Reachability: ReportingReachabilityReachable, BrokerCode: "1025", RetryAutomatic: true,
		},
		Evidence:            ReportingEvidenceStatus{State: ReportingEvidenceObserved, SchemaFingerprint: "flex_schema_1234567890abcdef"},
		Requirements:        []ReportingSectionRequirement{{Key: "trades", Label: "Trades", Status: "missing", Fields: []string{"tradeID"}}},
		MissingRequirements: []string{"trades"},
		Action:              "Review the broker configuration.",
	}
}
