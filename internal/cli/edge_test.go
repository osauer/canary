package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

type edgeCLIConn struct {
	method string
	params rpc.EdgeSnapshotParams
	result rpc.EdgeResult
}

func (c *edgeCLIConn) Call(_ context.Context, method string, params, out any) error {
	c.method = method
	c.params = params.(rpc.EdgeSnapshotParams)
	result := c.result
	result.Window = c.params.Window
	result.HorizonSessions = c.params.HorizonSessions
	for i := range result.Findings {
		result.Findings[i].HorizonSessions = c.params.HorizonSessions
	}
	*out.(*rpc.EdgeResult) = result
	return nil
}

func (*edgeCLIConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func TestEdgeCLIUsesBoundedTypedRPCParameters(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	args := []string{"--window", "365d", "--horizon", "5", "--limit", "2", "--change", "change_opaque", "--json"}
	if code := Run(t.Context(), env, "edge", args); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if conn.method != rpc.MethodEdgeSnapshot {
		t.Fatalf("method=%q", conn.method)
	}
	want := (rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 5, Limit: 2, ChangeID: "change_opaque"})
	if conn.params != want {
		t.Fatalf("params=%+v want %+v", conn.params, want)
	}
	if !bytes.Contains(stdout.Bytes(), []byte(`"not_execution": true`)) {
		t.Fatalf("missing not_execution marker: %s", stdout.String())
	}
}

func TestDefaultEdgeHumanOutputIsConciseAndComplete(t *testing.T) {
	result := edgeCLIResult()
	var stdout bytes.Buffer
	renderEdgeText(&stdout, result)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) > 12 {
		t.Fatalf("default output has %d lines, want at most 12:\n%s", len(lines), stdout.String())
	}
	for _, required := range []string{"Canary Edge", "Account P/L", "OPEN", "ADD", "TRIM", "EXIT", "Options · actual only", "Coverage"} {
		if !strings.Contains(stdout.String(), required) {
			t.Errorf("output missing %q:\n%s", required, stdout.String())
		}
	}
}

func TestEdgeSetupNamesProvenMissingQueryRequirements(t *testing.T) {
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v1", State: rpc.EdgeStateActionRequired, Reason: "flex_query_incomplete", Window: "90d", HorizonSessions: 20,
		Setup: &rpc.EdgeSetup{Steps: []string{"one", "two", "three"}, MissingRequirements: []string{"trades.ibOrderID", "open_positions.markPrice"}},
	}
	var stdout bytes.Buffer
	renderEdgeText(&stdout, result)
	for _, requirement := range result.Setup.MissingRequirements {
		if !strings.Contains(stdout.String(), requirement) {
			t.Fatalf("setup output omitted %q: %s", requirement, stdout.String())
		}
	}
}

func edgeCLIResult() rpc.EdgeResult {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	value := 125.5
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v1", State: rpc.EdgeStateCurrent, AsOf: now, Window: "90d", HorizonSessions: 20,
		Headline:    "Best observed result: ABC add, USD +125.50 after 20 sessions.",
		Account:     &rpc.EdgeAccountResult{BaseCurrency: "USD", RequestedFrom: now.AddDate(0, 0, -90), ActualFrom: now.AddDate(0, 0, -89), ActualTo: now, ProfitLossBase: 450, ExternalFlowsBase: 100, Definition: "Ending equity minus starting equity minus external flows."},
		Coverage:    rpc.EdgeCoverage{TradeChanges: 8, EligibleChanges: 6, ScoredByHorizon: map[int]int{1: 6, 5: 5, 20: 4}, ReasonCounts: map[string]int{}},
		Method:      rpc.EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Fingerprint: "edge_0123456789abcdef", LastFullRevalidation: now, NotExecution: true,
		Options: []rpc.EdgeOptionResult{{ID: "option_opaque", Grouping: "contract", Symbol: "ABC 100 C", LegCount: 1, ActualPNLBase: &value, ActualOnly: true}},
	}
	for _, action := range []string{"open", "add", "trim", "exit"} {
		row := rpc.EdgeActionRollup{Action: action}
		for _, sessions := range []int{1, 5, 20} {
			amount := float64(sessions)
			row.Horizons = append(row.Horizons, rpc.EdgeHorizonRollup{Sessions: sessions, SampleCount: 1, TotalBase: &amount, MedianBase: &amount})
		}
		result.ActionRollups = append(result.ActionRollups, row)
	}
	for i, symbol := range []string{"ABC", "DEF", "GHI"} {
		result.Findings = append(result.Findings, rpc.EdgeFinding{ChangeID: "change_" + strings.ToLower(symbol), Symbol: symbol, Action: "add", Direction: "long", ExecutedAt: now.Add(time.Duration(i) * time.Hour), HorizonSessions: 20, DecisionNotionalBase: 1_000, DecisionImpactBase: float64(100 - i), DecisionImpactPct: float64(10 - i)})
	}
	return result
}
