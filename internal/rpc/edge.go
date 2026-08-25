package rpc

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Edge RPC method and state constants define the bounded public snapshot
// lifecycle.
const (
	MethodEdgeSnapshot = "edge.snapshot"

	EdgeStateActionRequired = "action_required"
	EdgeStateBackfilling    = "backfilling"
	EdgeStateCurrent        = "current"
	EdgeStateDegraded       = "degraded"
	EdgeStateInsufficient   = "insufficient_evidence"
	EdgeStateUnavailable    = "unavailable"
	MaxEdgeFindings         = 3
)

// EdgeSnapshotParams selects one deterministic lens over the daemon-published
// Edge snapshot. Reads never start Flex or market-data work.
type EdgeSnapshotParams struct {
	Window          string `json:"window,omitempty"`
	HorizonSessions int    `json:"horizon_sessions,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	ChangeID        string `json:"change_id,omitempty"`
}

// NormalizeEdgeSnapshotParams is the single public input contract shared by
// CLI, MCP, HTTP, and daemon adapters.
func NormalizeEdgeSnapshotParams(in EdgeSnapshotParams) (EdgeSnapshotParams, error) {
	out := in
	out.Window = strings.ToLower(strings.TrimSpace(out.Window))
	if out.Window == "" {
		out.Window = "365d"
	}
	if out.Window != "90d" && out.Window != "365d" {
		return EdgeSnapshotParams{}, fmt.Errorf("edge window must be 90d or 365d")
	}
	if out.HorizonSessions == 0 {
		out.HorizonSessions = 20
	}
	if !validEdgeHorizon(out.HorizonSessions) {
		return EdgeSnapshotParams{}, fmt.Errorf("edge horizon must be 1, 5, or 20 sessions")
	}
	if out.Limit == 0 {
		out.Limit = MaxEdgeFindings
	}
	if out.Limit < 1 || out.Limit > MaxEdgeFindings {
		return EdgeSnapshotParams{}, fmt.Errorf("edge limit must be between 1 and %d", MaxEdgeFindings)
	}
	out.ChangeID = strings.TrimSpace(out.ChangeID)
	if len(out.ChangeID) > 128 || out.ChangeID != "" && !strings.HasPrefix(out.ChangeID, "change_") {
		return EdgeSnapshotParams{}, fmt.Errorf("edge change id is invalid")
	}
	return out, nil
}

// EdgeResult is the sanitized public Broker-Truth Decision Review. It contains
// no account, query, order, execution, statement-file, or filesystem identity.
type EdgeResult struct {
	SchemaVersion        string             `json:"schema_version"`
	State                string             `json:"state"`
	Reason               string             `json:"reason,omitempty"`
	AsOf                 time.Time          `json:"as_of,omitzero"`
	Window               string             `json:"window"`
	HorizonSessions      int                `json:"horizon_sessions"`
	Headline             string             `json:"headline,omitempty"`
	Account              *EdgeAccountResult `json:"account,omitempty"`
	ActionRollups        []EdgeActionRollup `json:"action_rollups"`
	Findings             []EdgeFinding      `json:"findings"`
	Options              []EdgeOptionResult `json:"options"`
	Coverage             EdgeCoverage       `json:"coverage"`
	Method               EdgeMethod         `json:"method"`
	Setup                *EdgeSetup         `json:"setup,omitempty"`
	Change               *EdgeChangeDetail  `json:"change,omitempty"`
	Fingerprint          string             `json:"fingerprint,omitempty"`
	LastFullRevalidation time.Time          `json:"last_full_revalidation,omitzero"`
	NotExecution         bool               `json:"not_execution"`
}

// EdgeAccountResult is the sanitized account-level result for exact statement
// boundaries.
type EdgeAccountResult struct {
	BaseCurrency       string    `json:"base_currency,omitempty"`
	RequestedFrom      time.Time `json:"requested_from"`
	ActualFrom         time.Time `json:"actual_from"`
	ActualTo           time.Time `json:"actual_to"`
	StartingEquityBase float64   `json:"starting_equity_base"`
	EndingEquityBase   float64   `json:"ending_equity_base"`
	ExternalFlowsBase  float64   `json:"external_flows_base"`
	ProfitLossBase     float64   `json:"profit_loss_base"`
	Definition         string    `json:"definition"`
}

// EdgeActionRollup is one public action-matrix row.
type EdgeActionRollup struct {
	Action   string              `json:"action"`
	Horizons []EdgeHorizonRollup `json:"horizons"`
}

// EdgeHorizonRollup is one count, total, and median matrix cell.
type EdgeHorizonRollup struct {
	Sessions    int      `json:"sessions"`
	SampleCount int      `json:"sample_count"`
	TotalBase   *float64 `json:"total_base,omitempty"`
	MedianBase  *float64 `json:"median_base,omitempty"`
}

// EdgeFinding is one bounded, opaque-ID retrospective observation.
type EdgeFinding struct {
	ChangeID             string    `json:"change_id"`
	Symbol               string    `json:"symbol"`
	Action               string    `json:"action"`
	Direction            string    `json:"direction"`
	ExecutedAt           time.Time `json:"executed_at"`
	HorizonSessions      int       `json:"horizon_sessions"`
	DecisionNotionalBase float64   `json:"decision_notional_base"`
	DecisionImpactBase   float64   `json:"decision_impact_base"`
	DecisionImpactPct    float64   `json:"decision_impact_pct"`
}

// EdgeChangeDetail expands one opaque change without exposing broker identity.
type EdgeChangeDetail struct {
	ID              string             `json:"id"`
	Symbol          string             `json:"symbol"`
	AssetClass      string             `json:"asset_class"`
	Currency        string             `json:"currency,omitempty"`
	Action          string             `json:"action"`
	Direction       string             `json:"direction"`
	ExecutedAt      time.Time          `json:"executed_at"`
	DeltaQuantity   float64            `json:"delta_quantity"`
	PositionBefore  float64            `json:"position_before"`
	PositionAfter   float64            `json:"position_after"`
	ExecutionVWAP   *float64           `json:"execution_vwap,omitempty"`
	Multiplier      *float64           `json:"multiplier,omitempty"`
	DirectCostsBase *float64           `json:"direct_costs_base,omitempty"`
	Scores          []EdgeHorizonScore `json:"scores"`
}

// EdgeHorizonScore exposes either one calculated impact or a typed reason.
type EdgeHorizonScore struct {
	Sessions             int        `json:"sessions"`
	HorizonDay           *time.Time `json:"horizon_day,omitempty"`
	HorizonClose         *float64   `json:"horizon_close,omitempty"`
	HorizonFX            *float64   `json:"horizon_fx,omitempty"`
	DecisionNotionalBase *float64   `json:"decision_notional_base,omitempty"`
	DecisionImpactBase   *float64   `json:"decision_impact_base,omitempty"`
	DecisionImpactPct    *float64   `json:"decision_impact_pct,omitempty"`
	Reason               string     `json:"reason,omitempty"`
}

// EdgeOptionResult exposes broker-actual option P/L only.
type EdgeOptionResult struct {
	ID              string   `json:"id"`
	Grouping        string   `json:"grouping"`
	Symbol          string   `json:"symbol"`
	Underlying      string   `json:"underlying,omitempty"`
	LegCount        int      `json:"leg_count"`
	RealizedPNLBase *float64 `json:"realized_pnl_base,omitempty"`
	OpenPNLBase     *float64 `json:"open_pnl_base,omitempty"`
	ActualPNLBase   *float64 `json:"actual_pnl_base,omitempty"`
	ActualOnly      bool     `json:"actual_only"`
}

// EdgeCoverage reports the sample and every typed exclusion count.
type EdgeCoverage struct {
	TradeChanges    int            `json:"trade_changes"`
	EligibleChanges int            `json:"eligible_changes"`
	ScoredByHorizon map[int]int    `json:"scored_by_horizon"`
	ReasonCounts    map[string]int `json:"reason_counts"`
	PresentSections []string       `json:"present_sections"`
	MissingSections []string       `json:"missing_sections"`
}

// EdgeMethod discloses the fixed counterfactual, inputs, exclusions, and claim
// boundaries.
type EdgeMethod struct {
	Metric              string `json:"metric"`
	Counterfactual      string `json:"counterfactual"`
	HorizonDefinition   string `json:"horizon_definition"`
	HeadlineSelection   string `json:"headline_selection"`
	FindingRanking      string `json:"finding_ranking"`
	AccountDefinition   string `json:"account_definition"`
	Exclusions          string `json:"exclusions"`
	OptionsMethod       string `json:"options_method"`
	NoCausalClaim       bool   `json:"no_causal_claim"`
	NoPredictiveClaim   bool   `json:"no_predictive_claim"`
	NotInvestmentAdvice bool   `json:"not_investment_advice"`
}

// EdgeSetup is the manifest-generated three-step Flex onboarding contract.
type EdgeSetup struct {
	ManifestVersion     string                   `json:"manifest_version"`
	Steps               []string                 `json:"steps"`
	Sections            []EdgeSectionRequirement `json:"sections"`
	MissingRequirements []string                 `json:"missing_requirements,omitempty"`
}

// EdgeSectionRequirement is one generated canonical Flex section.
type EdgeSectionRequirement struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	LevelOfDetail string   `json:"level_of_detail,omitempty"`
	Fields        []string `json:"fields"`
}

// ValidateEdgeResult rejects malformed or accidentally execution-capable wire
// results before adapters render them.
func ValidateEdgeResult(result EdgeResult) error {
	if result.SchemaVersion != "canary-edge-v1" {
		return fmt.Errorf("invalid Edge schema version %q", result.SchemaVersion)
	}
	switch result.State {
	case EdgeStateActionRequired, EdgeStateBackfilling, EdgeStateCurrent, EdgeStateDegraded, EdgeStateInsufficient, EdgeStateUnavailable:
	default:
		return fmt.Errorf("invalid Edge state %q", result.State)
	}
	if result.Window != "90d" && result.Window != "365d" {
		return fmt.Errorf("invalid Edge window %q", result.Window)
	}
	if result.HorizonSessions != 1 && result.HorizonSessions != 5 && result.HorizonSessions != 20 {
		return fmt.Errorf("invalid Edge horizon %d", result.HorizonSessions)
	}
	if !result.NotExecution {
		return fmt.Errorf("edge result must be marked not_execution")
	}
	if len(result.Findings) > MaxEdgeFindings {
		return fmt.Errorf("edge findings exceed contract maximum")
	}
	if result.Fingerprint != "" && !strings.HasPrefix(result.Fingerprint, "edge_") {
		return fmt.Errorf("invalid Edge fingerprint")
	}
	if result.State == EdgeStateCurrent && result.Fingerprint == "" {
		return fmt.Errorf("current Edge result requires a fingerprint")
	}
	if result.Fingerprint != "" && (result.AsOf.IsZero() || result.Method.Metric != "Decision price impact" || strings.TrimSpace(result.Method.HeadlineSelection) == "" || strings.TrimSpace(result.Method.FindingRanking) == "" || !result.Method.NoCausalClaim || !result.Method.NoPredictiveClaim || !result.Method.NotInvestmentAdvice) {
		return fmt.Errorf("published Edge result has an invalid method contract")
	}
	if result.Account != nil {
		amounts := [...]float64{result.Account.StartingEquityBase, result.Account.EndingEquityBase, result.Account.ExternalFlowsBase, result.Account.ProfitLossBase}
		for _, amount := range amounts {
			if !finite(amount) {
				return fmt.Errorf("invalid Edge account amount")
			}
		}
		if result.Account.RequestedFrom.IsZero() || result.Account.ActualFrom.IsZero() || result.Account.ActualTo.IsZero() || result.Account.ActualFrom.After(result.Account.ActualTo) || strings.TrimSpace(result.Account.Definition) == "" {
			return fmt.Errorf("invalid Edge account period")
		}
	}
	actions := map[string]bool{}
	for _, rollup := range result.ActionRollups {
		if !validEdgeAction(rollup.Action) || actions[rollup.Action] {
			return fmt.Errorf("invalid Edge action rollup")
		}
		actions[rollup.Action] = true
		horizons := map[int]bool{}
		for _, horizon := range rollup.Horizons {
			if !validEdgeHorizon(horizon.Sessions) || horizons[horizon.Sessions] || horizon.SampleCount < 0 {
				return fmt.Errorf("invalid Edge horizon rollup")
			}
			horizons[horizon.Sessions] = true
			if (horizon.TotalBase == nil) != (horizon.MedianBase == nil) || horizon.SampleCount == 0 && horizon.TotalBase != nil || horizon.SampleCount > 0 && horizon.TotalBase == nil {
				return fmt.Errorf("invalid Edge rollup amount presence")
			}
			for _, amount := range []*float64{horizon.TotalBase, horizon.MedianBase} {
				if amount != nil && !finite(*amount) {
					return fmt.Errorf("invalid Edge rollup amount")
				}
			}
		}
	}
	for _, finding := range result.Findings {
		if !strings.HasPrefix(finding.ChangeID, "change_") || !validEdgeAction(finding.Action) || !validEdgeDirection(finding.Direction) || finding.HorizonSessions != result.HorizonSessions || finding.ExecutedAt.IsZero() || !finite(finding.DecisionNotionalBase) || finding.DecisionNotionalBase <= 0 || !finite(finding.DecisionImpactBase) || !finite(finding.DecisionImpactPct) {
			return fmt.Errorf("invalid Edge finding")
		}
	}
	for _, option := range result.Options {
		if !strings.HasPrefix(option.ID, "option_") || (option.Grouping != "contract" && option.Grouping != "exact_order") || option.LegCount < 1 || !option.ActualOnly {
			return fmt.Errorf("invalid Edge option result")
		}
		for _, value := range []*float64{option.RealizedPNLBase, option.OpenPNLBase, option.ActualPNLBase} {
			if value != nil && !finite(*value) {
				return fmt.Errorf("invalid Edge option amount")
			}
		}
	}
	if result.Coverage.TradeChanges < 0 || result.Coverage.EligibleChanges < 0 || result.Coverage.EligibleChanges > result.Coverage.TradeChanges {
		return fmt.Errorf("invalid Edge coverage counts")
	}
	for horizon, count := range result.Coverage.ScoredByHorizon {
		if !validEdgeHorizon(horizon) || count < 0 || count > result.Coverage.TradeChanges {
			return fmt.Errorf("invalid Edge scored coverage")
		}
	}
	for reason, count := range result.Coverage.ReasonCounts {
		if strings.TrimSpace(reason) == "" || count < 0 {
			return fmt.Errorf("invalid Edge reason coverage")
		}
	}
	if result.Setup != nil {
		setupAllowed := result.State == EdgeStateActionRequired || result.State == EdgeStateInsufficient && result.Reason == "trade_history_unproved"
		unprovedTradesClaimedMissing := result.State == EdgeStateInsufficient && len(result.Setup.MissingRequirements) > 0
		if !setupAllowed || unprovedTradesClaimedMissing || len(result.Setup.ManifestVersion) == 0 || len(result.Setup.ManifestVersion) > 128 || len(result.Setup.Steps) != 3 || len(result.Setup.Sections) == 0 || len(result.Setup.Sections) > 32 || len(result.Setup.MissingRequirements) > 256 {
			return fmt.Errorf("invalid Edge setup contract")
		}
		allowed := make(map[string]bool)
		sectionKeys := make(map[string]bool)
		for _, section := range result.Setup.Sections {
			if !safeEdgeManifestToken(section.Key) || sectionKeys[section.Key] || strings.TrimSpace(section.Label) == "" || len(section.Fields) == 0 || len(section.Fields) > 128 {
				return fmt.Errorf("invalid Edge setup section")
			}
			sectionKeys[section.Key] = true
			allowed[section.Key] = true
			fields := make(map[string]bool)
			for _, field := range section.Fields {
				if !safeEdgeManifestToken(field) || fields[field] {
					return fmt.Errorf("invalid Edge setup field")
				}
				fields[field] = true
				allowed[section.Key+"."+field] = true
			}
		}
		seen := make(map[string]bool)
		for _, requirement := range result.Setup.MissingRequirements {
			if !allowed[requirement] || seen[requirement] {
				return fmt.Errorf("invalid Edge missing query requirement")
			}
			seen[requirement] = true
		}
	}
	if result.Change != nil {
		if !strings.HasPrefix(result.Change.ID, "change_") || !validEdgeAction(result.Change.Action) || !validEdgeDirection(result.Change.Direction) || result.Change.ExecutedAt.IsZero() || !finite(result.Change.DeltaQuantity) || !finite(result.Change.PositionBefore) || !finite(result.Change.PositionAfter) {
			return fmt.Errorf("invalid Edge change detail")
		}
		for _, amount := range []*float64{result.Change.ExecutionVWAP, result.Change.Multiplier, result.Change.DirectCostsBase} {
			if amount != nil && !finite(*amount) {
				return fmt.Errorf("invalid Edge change amount")
			}
		}
		for _, score := range result.Change.Scores {
			if !validEdgeHorizon(score.Sessions) || score.DecisionImpactBase != nil && score.Reason != "" {
				return fmt.Errorf("invalid Edge change score")
			}
			if score.DecisionImpactBase != nil && (score.DecisionNotionalBase == nil || score.DecisionImpactPct == nil || *score.DecisionNotionalBase <= 0) {
				return fmt.Errorf("invalid Edge score ranking inputs")
			}
			for _, amount := range []*float64{score.HorizonClose, score.HorizonFX, score.DecisionNotionalBase, score.DecisionImpactBase, score.DecisionImpactPct} {
				if amount != nil && !finite(*amount) {
					return fmt.Errorf("invalid Edge score amount")
				}
			}
		}
	}
	return nil
}

func validEdgeHorizon(value int) bool { return value == 1 || value == 5 || value == 20 }

func validEdgeAction(value string) bool {
	return value == "open" || value == "add" || value == "trim" || value == "exit"
}

func validEdgeDirection(value string) bool { return value == "long" || value == "short" }

func safeEdgeManifestToken(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
