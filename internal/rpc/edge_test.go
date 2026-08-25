package rpc

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestEdgeResultPreservesMissingFinancialValues(t *testing.T) {
	result := EdgeResult{
		SchemaVersion: "canary-edge-v1", State: EdgeStateBackfilling, Window: "90d", HorizonSessions: 20,
		ActionRollups: []EdgeActionRollup{{Action: "open", Horizons: []EdgeHorizonRollup{{Sessions: 20, SampleCount: 0}}}},
		Findings:      []EdgeFinding{}, Options: []EdgeOptionResult{}, Coverage: EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true,
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"total_base"`) || strings.Contains(string(raw), `"median_base"`) || strings.Contains(string(raw), `"account":`) {
		t.Fatalf("missing financial values were zero-filled: %s", raw)
	}
	if err := ValidateEdgeResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeResultValidationFailsClosed(t *testing.T) {
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	base := EdgeResult{
		SchemaVersion: "canary-edge-v1", State: EdgeStateCurrent, AsOf: now, Window: "90d", HorizonSessions: 20,
		ActionRollups: []EdgeActionRollup{}, Findings: []EdgeFinding{}, Options: []EdgeOptionResult{},
		Coverage:    EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}},
		Method:      EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
		Fingerprint: "edge_safe", NotExecution: true,
	}
	if err := ValidateEdgeResult(base); err != nil {
		t.Fatalf("valid Edge result rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*EdgeResult)
	}{
		{"schema", func(v *EdgeResult) { v.SchemaVersion = "edge-v0" }},
		{"state", func(v *EdgeResult) { v.State = "ready" }},
		{"window", func(v *EdgeResult) { v.Window = "all" }},
		{"horizon", func(v *EdgeResult) { v.HorizonSessions = 10 }},
		{"execution", func(v *EdgeResult) { v.NotExecution = false }},
		{"fingerprint", func(v *EdgeResult) { v.Fingerprint = "raw" }},
		{"change id", func(v *EdgeResult) { v.Findings = []EdgeFinding{{ChangeID: "raw-id"}} }},
		{"method claims", func(v *EdgeResult) { v.Method.NoCausalClaim = false }},
		{"finding ranking", func(v *EdgeResult) { v.Method.FindingRanking = "" }},
		{"account amount", func(v *EdgeResult) {
			v.Account = &EdgeAccountResult{RequestedFrom: now.AddDate(0, 0, -90), ActualFrom: now.AddDate(0, 0, -89), ActualTo: now, StartingEquityBase: math.NaN(), Definition: "defined"}
		}},
		{"rollup amount", func(v *EdgeResult) {
			total, median := math.NaN(), 1.0
			v.ActionRollups = []EdgeActionRollup{{Action: "open", Horizons: []EdgeHorizonRollup{{Sessions: 20, SampleCount: 1, TotalBase: &total, MedianBase: &median}}}}
		}},
		{"option grouping", func(v *EdgeResult) {
			v.Options = []EdgeOptionResult{{ID: "option_safe", Grouping: "inferred", LegCount: 1, ActualOnly: true}}
		}},
		{"coverage", func(v *EdgeResult) { v.Coverage.TradeChanges = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			tc.mutate(&got)
			if err := ValidateEdgeResult(got); err == nil {
				t.Fatal("invalid Edge result was accepted")
			}
		})
	}
}

func TestNormalizeEdgeSnapshotParamsIsTheSharedThreeFindingContract(t *testing.T) {
	got, err := NormalizeEdgeSnapshotParams(EdgeSnapshotParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Window != "365d" || got.HorizonSessions != 20 || got.Limit != MaxEdgeFindings {
		t.Fatalf("defaults=%+v", got)
	}
	for _, input := range []EdgeSnapshotParams{{Limit: 4}, {Window: "all"}, {HorizonSessions: 10}, {ChangeID: "broker-id"}} {
		if _, err := NormalizeEdgeSnapshotParams(input); err == nil {
			t.Fatalf("invalid params accepted: %+v", input)
		}
	}
}

func TestEdgeSetupMissingRequirementsAreManifestAllowlisted(t *testing.T) {
	result := EdgeResult{
		SchemaVersion: "canary-edge-v1", State: EdgeStateActionRequired, Reason: "flex_query_incomplete",
		Window: "90d", HorizonSessions: 20, ActionRollups: []EdgeActionRollup{}, Findings: []EdgeFinding{}, Options: []EdgeOptionResult{},
		Coverage: EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true,
		Setup: &EdgeSetup{
			ManifestVersion: "canary-reporting-flex-v1", Steps: []string{"one", "two", "three"},
			Sections:            []EdgeSectionRequirement{{Key: "trades", Label: "Trades", Fields: []string{"ibOrderID", "tradePrice"}}},
			MissingRequirements: []string{"trades.ibOrderID"},
		},
	}
	if err := ValidateEdgeResult(result); err != nil {
		t.Fatalf("valid missing requirement rejected: %v", err)
	}
	result.Setup.MissingRequirements = []string{"broker says upload token"}
	if err := ValidateEdgeResult(result); err == nil {
		t.Fatal("non-manifest missing requirement was accepted")
	}
}
