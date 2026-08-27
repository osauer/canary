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
		SchemaVersion: "canary-edge-v3", State: EdgeStateBackfilling, Window: "90d", HorizonSessions: 20,
		HorizonSelection: edgeTestSelection(false, 0, 0, 0),
		ActionRollups:    []EdgeActionRollup{{Action: "open", Horizons: []EdgeHorizonRollup{{Sessions: 20, SampleCount: 0}}}},
		Findings:         []EdgeFinding{}, Coverage: EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true,
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
		SchemaVersion: "canary-edge-v3", State: EdgeStateCurrent, AsOf: now, Window: "90d", HorizonSessions: 20,
		HorizonSelection: edgeTestSelection(false, 0, 0, 0),
		ActionRollups:    []EdgeActionRollup{}, Findings: []EdgeFinding{},
		Coverage:    EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}},
		Method:      EdgeMethod{Metric: "Decision price impact", HeadlineSelection: "disclosed", FindingRanking: "disclosed", MaterialityGate: "disclosed", AutomaticHorizon: "disclosed", MarketContext: "disclosed", NoCausalClaim: true, NoPredictiveClaim: true, NotInvestmentAdvice: true},
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
		{"market context disclosure", func(v *EdgeResult) { v.MarketContextMissing = []string{"spy"} }},
		{"account amount", func(v *EdgeResult) {
			v.Account = &EdgeAccountResult{RequestedFrom: now.AddDate(0, 0, -90), ActualFrom: now.AddDate(0, 0, -89), ActualTo: now, StartingEquityBase: math.NaN(), Definition: "defined"}
		}},
		{"rollup amount", func(v *EdgeResult) {
			total, median := math.NaN(), 1.0
			v.ActionRollups = []EdgeActionRollup{{Action: "open", Horizons: []EdgeHorizonRollup{{Sessions: 20, SampleCount: 1, TotalBase: &total, MedianBase: &median}}}}
		}},
		{"option grouping", func(v *EdgeResult) {
			zero := 0.0
			v.Options.Realized = EdgeOptionRealizedReview{KnownPNLBase: &zero, FlatCount: 1, CompleteCount: 1, TotalCount: 1, Episodes: []EdgeOptionEpisodeSummary{{ID: "option_safe", Grouping: "inferred", Lifecycle: "closing", ActivityFrom: now, ActivityTo: now, RealizedPNLBase: &zero, PNLStatus: "complete", Legs: []EdgeOptionLegIdentity{{Symbol: "SYN", Expiry: "2026-09-18", Strike: new(float64(100)), PutCall: "call"}}}}}
		}},
		{"option counts", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Options.Realized.TotalCount++
		}},
		{"partial option without missing evidence", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Options.Realized.CompleteCount = 0
			v.Options.Realized.PartialCount = 1
			v.Options.Realized.Episodes[0].PNLStatus = "partial"
		}},
		{"option identity without evidence", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Options.Realized.Episodes[0].Legs[0].Expiry = ""
		}},
		{"open option snapshot date", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Options.Open.SnapshotDate = time.Time{}
		}},
		{"duplicate option episode", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Options.Realized.Episodes = append(v.Options.Realized.Episodes, v.Options.Realized.Episodes[0])
			v.Options.Realized.TotalCount = 2
			v.Options.Realized.CompleteCount = 2
			v.Options.Realized.PositiveCount = 2
		}},
		{"option detail union", func(v *EdgeResult) {
			v.Options = edgeTestOptionReview(now)
			v.Option = &EdgeOptionDetail{ID: "option_safe", Kind: "realized_episode", Episode: edgeTestOptionDetail(now), OpenPosition: &EdgeOptionOpenPositionDetail{}}
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
	validOptions := base
	validOptions.Options = edgeTestOptionReview(now)
	validOptions.Option = &EdgeOptionDetail{ID: "option_safe", Kind: "realized_episode", Episode: edgeTestOptionDetail(now)}
	if err := ValidateEdgeResult(validOptions); err != nil {
		t.Fatalf("valid option review and detail rejected: %v", err)
	}
}

func edgeTestOptionReview(now time.Time) EdgeOptionReview {
	realized, open, strike := 90.0, -25.0, 100.0
	return EdgeOptionReview{
		Coverage: EdgeOptionCoverage{ExecutionEpisodes: 1, ClosingEpisodes: 1},
		Realized: EdgeOptionRealizedReview{
			KnownPNLBase: &realized, PositiveCount: 1, CompleteCount: 1, TotalCount: 1,
			Episodes: []EdgeOptionEpisodeSummary{{
				ID: "option_safe", Grouping: "exact_order", Lifecycle: "closing", Underlying: "SYN", ActivityFrom: now.Add(-time.Minute), ActivityTo: now,
				RealizedPNLBase: &realized, PNLStatus: "complete", MissingEvidence: []string{},
				Legs: []EdgeOptionLegIdentity{{Symbol: "SYN CALL", Underlying: "SYN", Expiry: "2026-09-18", Strike: &strike, PutCall: "call"}},
			}},
		},
		Open: EdgeOptionOpenReview{
			SnapshotDate: now, KnownPNLBase: &open, NegativeCount: 1, CompleteCount: 1, TotalCount: 1,
			Positions: []EdgeOptionOpenPositionSummary{{ID: "option_open_safe", Symbol: "SYN PUT", Underlying: "SYN", SnapshotDate: now, Expiry: "2026-09-18", Strike: &strike, PutCall: "put", OpenPNLBase: &open, PNLStatus: "complete", MissingEvidence: []string{}}},
		},
	}
}

func edgeTestOptionDetail(now time.Time) *EdgeOptionEpisodeDetail {
	realized, strike, quantity, price := 90.0, 100.0, 1.0, 2.5
	return &EdgeOptionEpisodeDetail{
		ID: "option_safe", Grouping: "exact_order", Lifecycle: "closing", Underlying: "SYN", ActivityFrom: now.Add(-time.Minute), ActivityTo: now,
		RealizedPNLBase: &realized, PNLStatus: "complete", MissingEvidence: []string{},
		Legs: []EdgeOptionEpisodeLeg{{ID: "option-leg_safe", Symbol: "SYN CALL", Underlying: "SYN", Expiry: "2026-09-18", Strike: &strike, PutCall: "call", Side: "sell", OpenClose: "closing", Quantity: &quantity, ExecutionPrice: &price, RealizedPNLBase: &realized, MissingEvidence: []string{}}},
	}
}

func TestNormalizeEdgeSnapshotParamsIsTheSharedThreeFindingContract(t *testing.T) {
	got, err := NormalizeEdgeSnapshotParams(EdgeSnapshotParams{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Window != "365d" || got.HorizonSessions != 20 || !got.AutomaticHorizon || got.Limit != MaxEdgeFindings {
		t.Fatalf("defaults=%+v", got)
	}
	for _, input := range []EdgeSnapshotParams{{Limit: 4}, {Window: "all"}, {HorizonSessions: 10}, {ChangeID: "broker-id"}, {OptionID: "broker-id"}, {ChangeID: "change_safe", OptionID: "option_safe"}} {
		if _, err := NormalizeEdgeSnapshotParams(input); err == nil {
			t.Fatalf("invalid params accepted: %+v", input)
		}
	}
}

func TestEdgeSetupMissingRequirementsAreManifestAllowlisted(t *testing.T) {
	result := EdgeResult{
		SchemaVersion: "canary-edge-v3", State: EdgeStateActionRequired, Reason: "flex_query_incomplete",
		Window: "90d", HorizonSessions: 20, ActionRollups: []EdgeActionRollup{}, Findings: []EdgeFinding{},
		HorizonSelection: edgeTestSelection(false, 0, 0, 0),
		Coverage:         EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true,
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

func TestEdgeSetupMayExplainOnlyTheTypedUnprovedTradeState(t *testing.T) {
	result := EdgeResult{
		SchemaVersion: "canary-edge-v3", State: EdgeStateInsufficient, Reason: "trade_history_unproved",
		Window: "365d", HorizonSessions: 20, ActionRollups: []EdgeActionRollup{}, Findings: []EdgeFinding{},
		HorizonSelection: edgeTestSelection(false, 0, 0, 0),
		Coverage:         EdgeCoverage{ScoredByHorizon: map[int]int{}, ReasonCounts: map[string]int{}}, NotExecution: true,
		Setup: &EdgeSetup{
			ManifestVersion: "canary-reporting-flex-v1", Steps: []string{"one", "two", "three"},
			Sections: []EdgeSectionRequirement{{Key: "trades", Label: "Trades", Fields: []string{"tradePrice"}}},
		},
	}
	if err := ValidateEdgeResult(result); err != nil {
		t.Fatalf("typed unproved-trade setup rejected: %v", err)
	}
	result.Setup.MissingRequirements = []string{"trades"}
	if err := ValidateEdgeResult(result); err == nil {
		t.Fatal("unproved Trades was relabeled as a proven missing requirement")
	}
	result.Setup.MissingRequirements = nil
	result.Reason = "another_reason"
	if err := ValidateEdgeResult(result); err == nil {
		t.Fatal("setup escaped the typed unproved-trade state")
	}
}

func edgeTestSelection(automatic bool, eligible, scored, largest int) EdgeHorizonSelection {
	mode := "explicit"
	reason := "explicit_override"
	if automatic {
		mode = "automatic"
		reason = "best_available"
	}
	coverage := 0.0
	if eligible > 0 {
		coverage = float64(scored) / float64(eligible) * 100
	}
	adequate := scored >= 3 && largest >= 3 && coverage >= 25
	if automatic && adequate {
		reason = "longest_adequately_covered"
	}
	return EdgeHorizonSelection{Mode: mode, Reason: reason, EligibleChanges: eligible, ScoredChanges: scored, CoveragePct: coverage, LargestActionSample: largest, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: adequate}
}
