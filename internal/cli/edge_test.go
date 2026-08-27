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
	result.AutomaticHorizon = c.params.AutomaticHorizon
	result.HorizonSelection.Mode = "explicit"
	result.HorizonSelection.Reason = "explicit_override"
	if c.params.AutomaticHorizon {
		result.HorizonSelection.Mode = "automatic"
		result.HorizonSelection.Reason = "best_available"
	}
	result.HorizonSelection.ScoredChanges = result.Coverage.ScoredByHorizon[c.params.HorizonSessions]
	result.HorizonSelection.CoveragePct = float64(result.HorizonSelection.ScoredChanges) / float64(result.HorizonSelection.EligibleChanges) * 100
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

func TestEdgeCLIForwardsOpaqueOptionDetail(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", []string{"--option", "option_opaque", "--json"}); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings, OptionID: "option_opaque"}
	if conn.params != want {
		t.Fatalf("params=%+v want %+v", conn.params, want)
	}
}

func TestEdgeCLIDefaultsToTheAutomaticOneYearReview(t *testing.T) {
	conn := &edgeCLIConn{result: edgeCLIResult()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(t.Context(), env, "edge", nil); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	want := rpc.EdgeSnapshotParams{Window: "365d", HorizonSessions: 20, AutomaticHorizon: true, Limit: rpc.MaxEdgeFindings}
	if conn.params != want {
		t.Fatalf("default params=%+v want %+v", conn.params, want)
	}
	if !strings.Contains(stdout.String(), "automatic one-year decision review") {
		t.Fatalf("default output does not lead with the automatic review: %s", stdout.String())
	}
}

func TestDefaultEdgeHumanOutputIsConciseAndComplete(t *testing.T) {
	result := edgeCLIResult()
	var stdout bytes.Buffer
	renderEdgeText(&stdout, result)
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) > 18 {
		t.Fatalf("default output has %d lines, want at most 18:\n%s", len(lines), stdout.String())
	}
	for _, required := range []string{"Canary Edge", "Account P/L", "OPEN", "ADD", "TRIM", "EXIT", "Options · realized", "Options · open snapshot", "Coverage"} {
		if !strings.Contains(stdout.String(), required) {
			t.Errorf("output missing %q:\n%s", required, stdout.String())
		}
	}
}

func TestEdgeHumanOutputDistinguishesConfirmedEmptyFromMissingOptionSnapshot(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.Options = rpc.EdgeOptionReview{
		Realized: rpc.EdgeOptionRealizedReview{Episodes: []rpc.EdgeOptionEpisodeSummary{}},
		Open:     rpc.EdgeOptionOpenReview{SnapshotDate: result.AsOf, Positions: []rpc.EdgeOptionOpenPositionSummary{}},
	}

	var stdout bytes.Buffer
	renderEdgeText(&stdout, result)
	if !strings.Contains(stdout.String(), "open snapshot "+result.AsOf.Format(time.DateOnly)+"  0 position(s) · confirmed empty") || strings.Contains(stdout.String(), "no dated Flex") {
		t.Fatalf("confirmed-empty snapshot output=%s", stdout.String())
	}

	result.Options.Open.SnapshotDate = time.Time{}
	stdout.Reset()
	renderEdgeText(&stdout, result)
	if !strings.Contains(stdout.String(), "open snapshot  no dated Flex Open Positions snapshot available") || strings.Contains(stdout.String(), "confirmed empty") {
		t.Fatalf("missing snapshot output=%s", stdout.String())
	}
}

func TestAutomaticOneSessionOutputUsesSingularGrammar(t *testing.T) {
	t.Parallel()
	result := edgeCLIResult()
	result.AutomaticHorizon = true
	result.HorizonSessions = 1
	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 6, ScoredChanges: 6, CoveragePct: 100, LargestActionSample: 3, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true}
	var stdout bytes.Buffer
	renderEdgeText(&stdout, result)
	if strings.Contains(stdout.String(), "1 sessions") || !strings.Contains(stdout.String(), "selected 1 session") || !strings.Contains(stdout.String(), "at 1 session") {
		t.Fatalf("one-session grammar=%s", stdout.String())
	}
}

func TestEdgeSetupNamesProvenMissingQueryRequirements(t *testing.T) {
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v3", State: rpc.EdgeStateActionRequired, Reason: "flex_query_incomplete", Window: "90d", HorizonSessions: 20,
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
	gain, loss, open, realizedTotal := 125.5, -50.0, -40.0, 75.5
	result := rpc.EdgeResult{
		SchemaVersion: "canary-edge-v3", State: rpc.EdgeStateCurrent, AsOf: now, Window: "90d", HorizonSessions: 20,
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "explicit", Reason: "explicit_override", EligibleChanges: 6, ScoredChanges: 4, CoveragePct: 4.0 / 6 * 100, LargestActionSample: 1, MinimumSample: 3, MinimumCoveragePct: 25},
		Headline:         "Best observed result: ABC add, USD +125.50 after 20 sessions.",
		Account:          &rpc.EdgeAccountResult{BaseCurrency: "USD", RequestedFrom: now.AddDate(0, 0, -90), ActualFrom: now.AddDate(0, 0, -89), ActualTo: now, ProfitLossBase: 450, ExternalFlowsBase: 100, Definition: "Ending equity minus starting equity minus external flows."},
		Coverage:         rpc.EdgeCoverage{TradeChanges: 8, EligibleChanges: 6, ScoredByHorizon: map[int]int{1: 6, 5: 5, 20: 4}, ReasonCounts: map[string]int{}},
		Method:           rpc.EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", MaterialityGate: "disclosed", AutomaticHorizon: "disclosed", MarketContext: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Fingerprint:      "edge_0123456789abcdef", LastFullRevalidation: now, NotExecution: true,
		Options: rpc.EdgeOptionReview{
			Coverage: rpc.EdgeOptionCoverage{ExecutionEpisodes: 2, ClosingEpisodes: 2},
			Realized: rpc.EdgeOptionRealizedReview{KnownPNLBase: &realizedTotal, PositiveCount: 1, NegativeCount: 1, CompleteCount: 2, TotalCount: 2, Episodes: []rpc.EdgeOptionEpisodeSummary{
				{ID: "option_gain", Grouping: "exact_order", Lifecycle: "closing", Underlying: "ABC", ActivityFrom: now.AddDate(0, 0, -5), ActivityTo: now.AddDate(0, 0, -5), RealizedPNLBase: &gain, PNLStatus: "complete", Legs: []rpc.EdgeOptionLegIdentity{{Symbol: "ABC CALL", Underlying: "ABC", Expiry: "2026-09-18", Strike: new(float64(100)), PutCall: "call"}}},
				{ID: "option_loss", Grouping: "unlinked_execution", Lifecycle: "closing", Underlying: "XYZ", ActivityFrom: now.AddDate(0, 0, -3), ActivityTo: now.AddDate(0, 0, -3), RealizedPNLBase: &loss, PNLStatus: "complete", Legs: []rpc.EdgeOptionLegIdentity{{Symbol: "XYZ PUT", Underlying: "XYZ", Expiry: "2026-10-16", Strike: new(float64(90)), PutCall: "put"}}},
			}},
			Open: rpc.EdgeOptionOpenReview{SnapshotDate: now, KnownPNLBase: &open, NegativeCount: 1, CompleteCount: 1, TotalCount: 1, Positions: []rpc.EdgeOptionOpenPositionSummary{{ID: "option_open", Symbol: "ABC CALL", Underlying: "ABC", SnapshotDate: now, Expiry: "2026-09-18", Strike: new(float64(100)), PutCall: "call", OpenPNLBase: &open, PNLStatus: "complete"}}},
		},
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
