// Stress contract tests live beside the current Stress wire authority.
package rpc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
)

func TestEstablishedAlertProjectionStrictRoundTrip(t *testing.T) {
	t.Parallel()
	projection := validEstablishedAlertProjection()
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded EstablishedAlertProjection
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded != projection {
		t.Fatalf("round trip=%+v, want %+v", decoded, projection)
	}
}

func TestEstablishedAlertProjectionRejectsMalformedPresentData(t *testing.T) {
	t.Parallel()
	valid := validEstablishedAlertProjection()
	for _, test := range []struct {
		name   string
		mutate func(*EstablishedAlertProjection)
	}{
		{name: "schema", mutate: func(value *EstablishedAlertProjection) { value.SchemaVersion = "future" }},
		{name: "fingerprint_version", mutate: func(value *EstablishedAlertProjection) { value.CanonicalFingerprint.Version = RegimeFingerprintVersion }},
		{name: "fingerprint_key", mutate: func(value *EstablishedAlertProjection) { value.CanonicalFingerprint.Key = "sha256:short" }},
		{name: "action", mutate: func(value *EstablishedAlertProjection) { value.Action = "page" }},
		{name: "market_confirmation", mutate: func(value *EstablishedAlertProjection) { value.MarketConfirmation = "unknown" }},
		{name: "severity", mutate: func(value *EstablishedAlertProjection) { value.Severity = "critical" }},
		{name: "occurrence_inconsistent", mutate: func(value *EstablishedAlertProjection) { value.OccurrenceEligible = false }},
		{name: "act_only_inconsistent", mutate: func(value *EstablishedAlertProjection) { value.ActOnlyEligible = false }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			value := valid
			test.mutate(&value)
			if err := ValidateEstablishedAlertProjection(value); err == nil {
				t.Fatalf("ValidateEstablishedAlertProjection(%+v) unexpectedly passed", value)
			}
			if _, err := json.Marshal(value); err == nil {
				t.Fatalf("json.Marshal(%+v) unexpectedly passed", value)
			}
		})
	}
}

func TestEstablishedAlertProjectionDecoderRequiresExactPresentContract(t *testing.T) {
	t.Parallel()
	valid := validEstablishedAlertProjection()
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	missing := strings.Replace(string(encoded), `"market_confirmation":"blocked",`, "", 1)
	unknown := strings.TrimSuffix(string(encoded), "}") + `,"pageable":true}`
	for _, wire := range []string{missing, unknown, string(encoded) + `{}`} {
		var decoded EstablishedAlertProjection
		if err := json.Unmarshal([]byte(wire), &decoded); err == nil {
			t.Fatalf("malformed projection unexpectedly decoded: %s", wire)
		}
	}
}

func TestStressResultAllowsAbsentEstablishedProjectionForOlderDaemonSkew(t *testing.T) {
	t.Parallel()
	for _, wire := range []string{`{}`, `{"established_alert_projection":null}`} {
		var result StressResult
		if err := json.Unmarshal([]byte(wire), &result); err != nil {
			t.Fatalf("decode older-daemon shape %s: %v", wire, err)
		}
		if result.EstablishedAlertProjection != nil {
			t.Fatalf("older-daemon shape %s produced projection %+v", wire, result.EstablishedAlertProjection)
		}
	}
}

func TestEstablishedAlertProjectionKeepsStressWireLabels(t *testing.T) {
	t.Parallel()
	if EstablishedAlertProjectionSchemaVersion != "stress-established-alert-v1" {
		t.Fatalf("schema version=%q", EstablishedAlertProjectionSchemaVersion)
	}
	if EstablishedStressFingerprintVersion != "stress-fp-v1" {
		t.Fatalf("fingerprint version=%q", EstablishedStressFingerprintVersion)
	}
}

func validEstablishedAlertProjection() EstablishedAlertProjection {
	return EstablishedAlertProjection{
		SchemaVersion:        EstablishedAlertProjectionSchemaVersion,
		CanonicalFingerprint: Fingerprint{Version: EstablishedStressFingerprintVersion, Key: "sha256:" + strings.Repeat("a", 64)},
		OccurrenceEligible:   true,
		ActOnlyEligible:      true,
		Action:               "confirm_inputs",
		MarketConfirmation:   "blocked",
		Severity:             risk.SeverityObserve,
		PortfolioRelevant:    true,
	}
}
