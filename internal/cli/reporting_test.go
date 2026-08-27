package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

type reportingCLIConn struct {
	method string
	result rpc.ReportingStatusResult
}

func (c *reportingCLIConn) Call(_ context.Context, method string, _ any, out any) error {
	c.method = method
	*out.(*rpc.ReportingStatusResult) = c.result
	return nil
}

func (*reportingCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestReportingStatusUsesTypedRedactedRPC(t *testing.T) {
	result := reportingCLIResult()
	conn := &reportingCLIConn{result: result}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "reporting", []string{"status", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if conn.method != rpc.MethodReportingStatus {
		t.Fatalf("method = %q", conn.method)
	}
	for _, required := range []string{`"state": "action_required"`, `"broker_code": "1025"`, `"schema_fingerprint": "flex_schema_0123456789abcdef"`} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("JSON omitted %q: %s", required, stdout.String())
		}
	}
	for _, forbidden := range []string{"query_id", "token_path", "account_id", "statement.xml"} {
		if strings.Contains(stdout.String(), forbidden) {
			t.Fatalf("JSON exposed forbidden key/value %q: %s", forbidden, stdout.String())
		}
	}
}

func TestReportingStatusHumanOutputNamesEvidenceAndAction(t *testing.T) {
	result := reportingCLIResult()
	conn := &reportingCLIConn{result: result}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "reporting", nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, required := range []string{
		"Broker reporting — action_required", "reachability=reachable", "code=1025",
		"missing: trades.ibOrderID", "empty: transfers", "absent: corporate_actions", "contact IBKR",
	} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("human output omitted %q: %s", required, stdout.String())
		}
	}
}

func reportingCLIResult() rpc.ReportingStatusResult {
	requirements := make([]rpc.ReportingSectionRequirement, 0, len(flexstmt.CanonicalQueryManifest()))
	for _, section := range flexstmt.CanonicalQueryManifest() {
		status := flexstmt.QueryRequirementObserved
		missing := []string(nil)
		switch section.Key {
		case "trades":
			status, missing = flexstmt.QueryRequirementMissing, []string{"ibOrderID"}
		case "transfers":
			status = flexstmt.QueryRequirementEmpty
		case "corporate_actions":
			status = flexstmt.QueryRequirementAbsent
		}
		requirements = append(requirements, rpc.ReportingSectionRequirement{
			Key: section.Key, Label: section.Label, Status: status, LevelOfDetail: section.LevelOfDetail,
			Fields: append([]string(nil), section.RequiredFields...), MissingFields: missing,
		})
	}
	return rpc.ReportingStatusResult{
		SchemaVersion: rpc.ReportingSchemaVersion, State: rpc.ReportingStateActionRequired,
		Reason: rpc.ReportingReasonBrokerResponseUndocumented, ManifestVersion: flexstmt.ManifestVersion,
		Local: rpc.ReportingLocalStatus{Enabled: true, QueryConfigured: true, TokenFilePresent: true, TokenFilePrivate: true},
		Broker: rpc.ReportingBrokerStatus{
			State: rpc.ReconReportStateRetryScheduled, Reason: rpc.ReconReportReasonResponseInvalid,
			Reachability: rpc.ReportingReachabilityReachable, BrokerCode: "1025", RetryAutomatic: true,
		},
		Evidence:     rpc.ReportingEvidenceStatus{State: rpc.ReportingEvidenceObserved, SchemaFingerprint: "flex_schema_0123456789abcdef"},
		Requirements: requirements, MissingRequirements: []string{"trades.ibOrderID"}, UnprovedSections: []string{"transfers", "corporate_actions"},
		Action: "IBKR returned an undocumented Flex response; review configuration or contact IBKR.",
	}
}
