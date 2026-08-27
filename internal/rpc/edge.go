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
	MaxEdgeOptionResults    = 20
)

// EdgeSnapshotParams selects one deterministic lens over the daemon-published
// Edge snapshot. Reads never start Flex or market-data work.
type EdgeSnapshotParams struct {
	Window           string `json:"window,omitempty"`
	HorizonSessions  int    `json:"horizon_sessions,omitempty"`
	AutomaticHorizon bool   `json:"automatic_horizon,omitempty"`
	Limit            int    `json:"limit,omitempty"`
	ChangeID         string `json:"change_id,omitempty"`
	OptionID         string `json:"option_id,omitempty"`
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
		out.AutomaticHorizon = true
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
	out.OptionID = strings.TrimSpace(out.OptionID)
	if len(out.OptionID) > 128 || out.OptionID != "" && !strings.HasPrefix(out.OptionID, "option_") {
		return EdgeSnapshotParams{}, fmt.Errorf("edge option id is invalid")
	}
	if out.ChangeID != "" && out.OptionID != "" {
		return EdgeSnapshotParams{}, fmt.Errorf("edge change and option detail are mutually exclusive")
	}
	return out, nil
}

// EdgeResult is the sanitized public Broker-Truth Decision Review. It contains
// no account, query, order, execution, statement-file, or filesystem identity.
type EdgeResult struct {
	SchemaVersion        string                    `json:"schema_version"`
	State                string                    `json:"state"`
	Reason               string                    `json:"reason,omitempty"`
	AsOf                 time.Time                 `json:"as_of,omitzero"`
	Window               string                    `json:"window"`
	HorizonSessions      int                       `json:"horizon_sessions"`
	AutomaticHorizon     bool                      `json:"automatic_horizon"`
	HorizonSelection     EdgeHorizonSelection      `json:"horizon_selection"`
	Headline             string                    `json:"headline,omitempty"`
	MarketContext        []EdgeMarketContextRollup `json:"market_context"`
	MarketContextMissing []string                  `json:"market_context_missing"`
	Account              *EdgeAccountResult        `json:"account,omitempty"`
	ActionRollups        []EdgeActionRollup        `json:"action_rollups"`
	Findings             []EdgeFinding             `json:"findings"`
	Options              EdgeOptionReview          `json:"options"`
	Coverage             EdgeCoverage              `json:"coverage"`
	Method               EdgeMethod                `json:"method"`
	Setup                *EdgeSetup                `json:"setup,omitempty"`
	Change               *EdgeChangeDetail         `json:"change,omitempty"`
	Option               *EdgeOptionDetail         `json:"option,omitempty"`
	Fingerprint          string                    `json:"fingerprint,omitempty"`
	LastFullRevalidation time.Time                 `json:"last_full_revalidation,omitzero"`
	NotExecution         bool                      `json:"not_execution"`
}

// EdgeHorizonSelection explains whether the selected lens was explicit or the
// daemon's deterministic best available automatic horizon.
type EdgeHorizonSelection struct {
	Mode                string  `json:"mode"`
	Reason              string  `json:"reason"`
	EligibleChanges     int     `json:"eligible_changes"`
	ScoredChanges       int     `json:"scored_changes"`
	CoveragePct         float64 `json:"coverage_pct"`
	LargestActionSample int     `json:"largest_action_sample"`
	MinimumSample       int     `json:"minimum_sample"`
	MinimumCoveragePct  float64 `json:"minimum_coverage_pct"`
	Adequate            bool    `json:"adequate"`
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
	Sessions      int                       `json:"sessions"`
	SampleCount   int                       `json:"sample_count"`
	TotalBase     *float64                  `json:"total_base,omitempty"`
	MedianBase    *float64                  `json:"median_base,omitempty"`
	MarketContext []EdgeMarketContextRollup `json:"market_context,omitempty"`
}

// EdgeMarketContextRollup is informational benchmark context for the selected
// action/horizon. It does not participate in the Edge impact calculation.
type EdgeMarketContextRollup struct {
	Key                string   `json:"key"`
	Label              string   `json:"label"`
	Kind               string   `json:"kind"`
	SampleCount        int      `json:"sample_count"`
	MedianChangePct    *float64 `json:"median_change_pct,omitempty"`
	MedianChangePoints *float64 `json:"median_change_points,omitempty"`
}

// EdgeMarketContext is one exact benchmark interval attached to a finding.
type EdgeMarketContext struct {
	Key          string    `json:"key"`
	Label        string    `json:"label"`
	Kind         string    `json:"kind"`
	StartDay     time.Time `json:"start_day"`
	EndDay       time.Time `json:"end_day"`
	StartClose   float64   `json:"start_close"`
	EndClose     float64   `json:"end_close"`
	ChangePct    float64   `json:"change_pct"`
	ChangePoints *float64  `json:"change_points,omitempty"`
}

// EdgeFinding is one bounded, opaque-ID retrospective observation.
type EdgeFinding struct {
	ChangeID             string              `json:"change_id"`
	Symbol               string              `json:"symbol"`
	Action               string              `json:"action"`
	Direction            string              `json:"direction"`
	ExecutedAt           time.Time           `json:"executed_at"`
	HorizonSessions      int                 `json:"horizon_sessions"`
	DecisionNotionalBase float64             `json:"decision_notional_base"`
	DecisionImpactBase   float64             `json:"decision_impact_base"`
	DecisionImpactPct    float64             `json:"decision_impact_pct"`
	MarketContext        []EdgeMarketContext `json:"market_context,omitempty"`
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
	Sessions             int                 `json:"sessions"`
	HorizonDay           *time.Time          `json:"horizon_day,omitempty"`
	HorizonClose         *float64            `json:"horizon_close,omitempty"`
	HorizonFX            *float64            `json:"horizon_fx,omitempty"`
	DecisionNotionalBase *float64            `json:"decision_notional_base,omitempty"`
	DecisionImpactBase   *float64            `json:"decision_impact_base,omitempty"`
	DecisionImpactPct    *float64            `json:"decision_impact_pct,omitempty"`
	MarketContext        []EdgeMarketContext `json:"market_context,omitempty"`
	Reason               string              `json:"reason,omitempty"`
}

// EdgeOptionReview keeps realized option episodes and the dated open-position
// snapshot as separate broker-truth scopes.
type EdgeOptionReview struct {
	Coverage EdgeOptionCoverage       `json:"coverage"`
	Realized EdgeOptionRealizedReview `json:"realized"`
	Open     EdgeOptionOpenReview     `json:"open"`
}

// EdgeOptionCoverage accounts for lifecycle evidence that is not necessarily
// a realized result.
type EdgeOptionCoverage struct {
	ExecutionEpisodes       int `json:"execution_episodes"`
	OpeningEpisodes         int `json:"opening_episodes"`
	OpeningOnlyZeroEpisodes int `json:"opening_only_zero_episodes"`
	ClosingEpisodes         int `json:"closing_episodes"`
	MixedEpisodes           int `json:"mixed_episodes"`
	UnknownEpisodes         int `json:"unknown_episodes"`
	EventEpisodes           int `json:"event_episodes"`
}

// EdgeOptionRealizedReview is a bounded public view over all realized
// episodes in the selected window.
type EdgeOptionRealizedReview struct {
	KnownPNLBase     *float64                   `json:"known_pnl_base,omitempty"`
	PositiveCount    int                        `json:"positive_count"`
	NegativeCount    int                        `json:"negative_count"`
	FlatCount        int                        `json:"flat_count"`
	CompleteCount    int                        `json:"complete_count"`
	PartialCount     int                        `json:"partial_count"`
	UnavailableCount int                        `json:"unavailable_count"`
	TotalCount       int                        `json:"total_count"`
	Truncated        bool                       `json:"truncated"`
	Episodes         []EdgeOptionEpisodeSummary `json:"episodes"`
}

// EdgeOptionOpenReview is a bounded public view over the latest authoritative
// Flex open-position snapshot.
type EdgeOptionOpenReview struct {
	SnapshotDate     time.Time                       `json:"snapshot_date,omitzero"`
	KnownPNLBase     *float64                        `json:"known_pnl_base,omitempty"`
	PositiveCount    int                             `json:"positive_count"`
	NegativeCount    int                             `json:"negative_count"`
	FlatCount        int                             `json:"flat_count"`
	CompleteCount    int                             `json:"complete_count"`
	UnavailableCount int                             `json:"unavailable_count"`
	TotalCount       int                             `json:"total_count"`
	Truncated        bool                            `json:"truncated"`
	Positions        []EdgeOptionOpenPositionSummary `json:"positions"`
}

// EdgeOptionEpisodeSummary identifies one realized episode without carrying
// its execution-size details.
type EdgeOptionEpisodeSummary struct {
	ID              string                  `json:"id"`
	Grouping        string                  `json:"grouping"`
	Lifecycle       string                  `json:"lifecycle"`
	EventType       string                  `json:"event_type,omitempty"`
	Underlying      string                  `json:"underlying,omitempty"`
	ActivityFrom    time.Time               `json:"activity_from"`
	ActivityTo      time.Time               `json:"activity_to"`
	RealizedPNLBase *float64                `json:"realized_pnl_base,omitempty"`
	PNLStatus       string                  `json:"pnl_status"`
	MissingEvidence []string                `json:"missing_evidence"`
	Legs            []EdgeOptionLegIdentity `json:"legs"`
}

// EdgeOptionLegIdentity carries only the contract description required to
// distinguish a compact result row.
type EdgeOptionLegIdentity struct {
	Symbol     string   `json:"symbol"`
	Underlying string   `json:"underlying,omitempty"`
	Expiry     string   `json:"expiry,omitempty"`
	Strike     *float64 `json:"strike,omitempty"`
	PutCall    string   `json:"put_call,omitempty"`
}

// EdgeOptionOpenPositionSummary identifies one dated open contract without
// carrying its size and mark details.
type EdgeOptionOpenPositionSummary struct {
	ID              string    `json:"id"`
	Symbol          string    `json:"symbol"`
	Underlying      string    `json:"underlying,omitempty"`
	SnapshotDate    time.Time `json:"snapshot_date"`
	Expiry          string    `json:"expiry,omitempty"`
	Strike          *float64  `json:"strike,omitempty"`
	PutCall         string    `json:"put_call,omitempty"`
	OpenPNLBase     *float64  `json:"open_pnl_base,omitempty"`
	PNLStatus       string    `json:"pnl_status"`
	MissingEvidence []string  `json:"missing_evidence"`
}

// EdgeOptionDetail is one on-demand broker-evidence expansion. Exactly one of
// Episode or OpenPosition is populated.
type EdgeOptionDetail struct {
	ID           string                        `json:"id"`
	Kind         string                        `json:"kind"`
	Episode      *EdgeOptionEpisodeDetail      `json:"episode,omitempty"`
	OpenPosition *EdgeOptionOpenPositionDetail `json:"open_position,omitempty"`
}

// EdgeOptionEpisodeDetail carries the execution facts behind one episode.
type EdgeOptionEpisodeDetail struct {
	ID              string                 `json:"id"`
	Grouping        string                 `json:"grouping"`
	Lifecycle       string                 `json:"lifecycle"`
	EventType       string                 `json:"event_type,omitempty"`
	Underlying      string                 `json:"underlying,omitempty"`
	ActivityFrom    time.Time              `json:"activity_from"`
	ActivityTo      time.Time              `json:"activity_to"`
	RealizedPNLBase *float64               `json:"realized_pnl_base,omitempty"`
	PNLStatus       string                 `json:"pnl_status"`
	MissingEvidence []string               `json:"missing_evidence"`
	Legs            []EdgeOptionEpisodeLeg `json:"legs"`
}

// EdgeOptionEpisodeLeg carries one aggregated exact-contract execution leg.
type EdgeOptionEpisodeLeg struct {
	ID              string   `json:"id"`
	Symbol          string   `json:"symbol"`
	Underlying      string   `json:"underlying,omitempty"`
	Expiry          string   `json:"expiry,omitempty"`
	Strike          *float64 `json:"strike,omitempty"`
	PutCall         string   `json:"put_call,omitempty"`
	Multiplier      *float64 `json:"multiplier,omitempty"`
	Side            string   `json:"side,omitempty"`
	OpenClose       string   `json:"open_close,omitempty"`
	Quantity        *float64 `json:"quantity,omitempty"`
	ExecutionPrice  *float64 `json:"execution_price,omitempty"`
	Currency        string   `json:"currency,omitempty"`
	RealizedPNLBase *float64 `json:"realized_pnl_base,omitempty"`
	DirectCostsBase *float64 `json:"direct_costs_base,omitempty"`
	MissingEvidence []string `json:"missing_evidence"`
}

// EdgeOptionOpenPositionDetail carries the size, mark, and cost basis behind
// one dated open-position summary.
type EdgeOptionOpenPositionDetail struct {
	EdgeOptionOpenPositionSummary
	Multiplier     *float64 `json:"multiplier,omitempty"`
	Side           string   `json:"side,omitempty"`
	Quantity       *float64 `json:"quantity,omitempty"`
	MarkPrice      *float64 `json:"mark_price,omitempty"`
	CostBasisMoney *float64 `json:"cost_basis_money,omitempty"`
	Currency       string   `json:"currency,omitempty"`
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
	MaterialityGate     string `json:"materiality_gate"`
	AutomaticHorizon    string `json:"automatic_horizon"`
	MarketContext       string `json:"market_context"`
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
	if result.SchemaVersion != "canary-edge-v3" {
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
	if err := validateEdgeHorizonSelection(result); err != nil {
		return err
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
	if result.Fingerprint != "" && (result.AsOf.IsZero() || result.Method.Metric != "Decision price impact" || strings.TrimSpace(result.Method.HeadlineSelection) == "" || strings.TrimSpace(result.Method.FindingRanking) == "" || strings.TrimSpace(result.Method.MaterialityGate) == "" || strings.TrimSpace(result.Method.AutomaticHorizon) == "" || strings.TrimSpace(result.Method.MarketContext) == "" || !result.Method.NoCausalClaim || !result.Method.NoPredictiveClaim || !result.Method.NotInvestmentAdvice) {
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
			if err := validateEdgeMarketContextRollups(horizon.MarketContext); err != nil {
				return err
			}
			for _, context := range horizon.MarketContext {
				if context.SampleCount > horizon.SampleCount {
					return fmt.Errorf("invalid Edge market context sample")
				}
			}
		}
	}
	if err := validateEdgeMarketContextRollups(result.MarketContext); err != nil {
		return err
	}
	presentContext := map[string]bool{}
	for _, context := range result.MarketContext {
		presentContext[context.Key] = true
		if context.SampleCount > result.HorizonSelection.LargestActionSample {
			return fmt.Errorf("invalid selected Edge market context sample")
		}
	}
	missingContext := map[string]bool{}
	for _, key := range result.MarketContextMissing {
		if !validEdgeMarketContextKey(key) || presentContext[key] || missingContext[key] {
			return fmt.Errorf("invalid missing Edge market context")
		}
		missingContext[key] = true
	}
	if len(presentContext)+len(missingContext) > 0 && len(presentContext)+len(missingContext) != 4 {
		return fmt.Errorf("incomplete Edge market context disclosure")
	}
	for _, finding := range result.Findings {
		if !strings.HasPrefix(finding.ChangeID, "change_") || !validEdgeAction(finding.Action) || !validEdgeDirection(finding.Direction) || finding.HorizonSessions != result.HorizonSessions || finding.ExecutedAt.IsZero() || !finite(finding.DecisionNotionalBase) || finding.DecisionNotionalBase <= 0 || !finite(finding.DecisionImpactBase) || !finite(finding.DecisionImpactPct) {
			return fmt.Errorf("invalid Edge finding")
		}
		if err := validateEdgeMarketContext(finding.MarketContext); err != nil {
			return err
		}
	}
	if err := validateEdgeOptionReview(result.Options); err != nil {
		return err
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
			if score.DecisionImpactBase == nil && len(score.MarketContext) > 0 {
				return fmt.Errorf("unscored Edge change carries market context")
			}
			if score.DecisionImpactBase != nil && (score.DecisionNotionalBase == nil || score.DecisionImpactPct == nil || *score.DecisionNotionalBase <= 0) {
				return fmt.Errorf("invalid Edge score ranking inputs")
			}
			for _, amount := range []*float64{score.HorizonClose, score.HorizonFX, score.DecisionNotionalBase, score.DecisionImpactBase, score.DecisionImpactPct} {
				if amount != nil && !finite(*amount) {
					return fmt.Errorf("invalid Edge score amount")
				}
			}
			if err := validateEdgeMarketContext(score.MarketContext); err != nil {
				return err
			}
		}
	}
	if result.Change != nil && result.Option != nil {
		return fmt.Errorf("edge result cannot carry change and option detail together")
	}
	if result.Option != nil {
		if err := validateEdgeOptionDetail(*result.Option); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeOptionReview(review EdgeOptionReview) error {
	coverage := review.Coverage
	counts := [...]int{coverage.ExecutionEpisodes, coverage.OpeningEpisodes, coverage.OpeningOnlyZeroEpisodes, coverage.ClosingEpisodes, coverage.MixedEpisodes, coverage.UnknownEpisodes, coverage.EventEpisodes}
	for _, count := range counts {
		if count < 0 {
			return fmt.Errorf("invalid Edge option coverage")
		}
	}
	if coverage.ExecutionEpisodes != coverage.OpeningEpisodes+coverage.ClosingEpisodes+coverage.MixedEpisodes+coverage.UnknownEpisodes || coverage.OpeningOnlyZeroEpisodes > coverage.OpeningEpisodes {
		return fmt.Errorf("inconsistent Edge option coverage")
	}
	realized := review.Realized
	if realized.TotalCount < 0 || realized.TotalCount != realized.CompleteCount+realized.PartialCount+realized.UnavailableCount || realized.PositiveCount+realized.NegativeCount+realized.FlatCount != realized.CompleteCount+realized.PartialCount || len(realized.Episodes) > MaxEdgeOptionResults || realized.TotalCount < len(realized.Episodes) || realized.Truncated != (realized.TotalCount > len(realized.Episodes)) {
		return fmt.Errorf("invalid Edge realized option counts")
	}
	if (realized.KnownPNLBase == nil) != (realized.CompleteCount+realized.PartialCount == 0) || realized.KnownPNLBase != nil && !finite(*realized.KnownPNLBase) {
		return fmt.Errorf("invalid Edge realized option total")
	}
	seen := map[string]bool{}
	for _, episode := range realized.Episodes {
		if seen[episode.ID] {
			return fmt.Errorf("duplicate Edge realized option episode")
		}
		seen[episode.ID] = true
		if err := validateEdgeOptionEpisodeSummary(episode); err != nil {
			return err
		}
	}
	open := review.Open
	if open.TotalCount < 0 || open.TotalCount != open.CompleteCount+open.UnavailableCount || open.PositiveCount+open.NegativeCount+open.FlatCount != open.CompleteCount || len(open.Positions) > MaxEdgeOptionResults || open.TotalCount < len(open.Positions) || open.Truncated != (open.TotalCount > len(open.Positions)) {
		return fmt.Errorf("invalid Edge open option counts")
	}
	if (open.KnownPNLBase == nil) != (open.CompleteCount == 0) || open.KnownPNLBase != nil && !finite(*open.KnownPNLBase) {
		return fmt.Errorf("invalid Edge open option total")
	}
	if open.TotalCount > 0 && open.SnapshotDate.IsZero() {
		return fmt.Errorf("invalid Edge open option snapshot date")
	}
	seen = map[string]bool{}
	for _, position := range open.Positions {
		if seen[position.ID] {
			return fmt.Errorf("duplicate Edge open option position")
		}
		seen[position.ID] = true
		if err := validateEdgeOptionOpenSummary(position); err != nil {
			return err
		}
	}
	return nil
}

func validateEdgeOptionEpisodeSummary(episode EdgeOptionEpisodeSummary) error {
	if !strings.HasPrefix(episode.ID, "option_") || !validEdgeOptionGrouping(episode.Grouping) || !validEdgeOptionLifecycle(episode.Lifecycle) || episode.ActivityFrom.IsZero() || episode.ActivityTo.IsZero() || episode.ActivityFrom.After(episode.ActivityTo) || len(episode.Legs) == 0 {
		return fmt.Errorf("invalid Edge realized option episode")
	}
	if episode.Grouping == "option_event" {
		if episode.Lifecycle != "event" || !validEdgeOptionEventType(episode.EventType) {
			return fmt.Errorf("invalid Edge option lifecycle event")
		}
	} else if episode.EventType != "" || episode.Lifecycle == "event" {
		return fmt.Errorf("invalid Edge option execution lifecycle")
	}
	if err := validateEdgeOptionPNL(episode.PNLStatus, episode.RealizedPNLBase, episode.MissingEvidence); err != nil {
		return err
	}
	for _, leg := range episode.Legs {
		if strings.TrimSpace(leg.Symbol) == "" || !validEdgeOptionIdentity(leg.Expiry, leg.Strike, leg.PutCall, episode.MissingEvidence) {
			return fmt.Errorf("invalid Edge option leg identity")
		}
	}
	return nil
}

func validateEdgeOptionOpenSummary(position EdgeOptionOpenPositionSummary) error {
	if !strings.HasPrefix(position.ID, "option_") || strings.TrimSpace(position.Symbol) == "" || position.SnapshotDate.IsZero() || !validEdgeOptionIdentity(position.Expiry, position.Strike, position.PutCall, position.MissingEvidence) {
		return fmt.Errorf("invalid Edge open option position")
	}
	return validateEdgeOptionPNL(position.PNLStatus, position.OpenPNLBase, position.MissingEvidence)
}

func validateEdgeOptionDetail(detail EdgeOptionDetail) error {
	if !strings.HasPrefix(detail.ID, "option_") {
		return fmt.Errorf("invalid Edge option detail id")
	}
	switch detail.Kind {
	case "realized_episode":
		if detail.Episode == nil || detail.OpenPosition != nil || detail.Episode.ID != detail.ID {
			return fmt.Errorf("invalid Edge realized option detail")
		}
		episode := detail.Episode
		identities := make([]EdgeOptionLegIdentity, 0, len(episode.Legs))
		for _, leg := range episode.Legs {
			identities = append(identities, EdgeOptionLegIdentity{Symbol: leg.Symbol, Underlying: leg.Underlying, Expiry: leg.Expiry, Strike: leg.Strike, PutCall: leg.PutCall})
			if !strings.HasPrefix(leg.ID, "option-leg_") || !validEdgeOptionIdentity(leg.Expiry, leg.Strike, leg.PutCall, leg.MissingEvidence) || !validEdgeOptionTradeSide(leg.Side) || !validEdgeOptionOpenClose(leg.OpenClose) {
				return fmt.Errorf("invalid Edge option execution leg")
			}
			for _, amount := range []*float64{leg.Strike, leg.Multiplier, leg.Quantity, leg.ExecutionPrice, leg.RealizedPNLBase, leg.DirectCostsBase} {
				if amount != nil && !finite(*amount) {
					return fmt.Errorf("invalid Edge option execution amount")
				}
			}
		}
		return validateEdgeOptionEpisodeSummary(EdgeOptionEpisodeSummary{ID: episode.ID, Grouping: episode.Grouping, Lifecycle: episode.Lifecycle, EventType: episode.EventType, Underlying: episode.Underlying, ActivityFrom: episode.ActivityFrom, ActivityTo: episode.ActivityTo, RealizedPNLBase: episode.RealizedPNLBase, PNLStatus: episode.PNLStatus, MissingEvidence: episode.MissingEvidence, Legs: identities})
	case "open_position":
		if detail.OpenPosition == nil || detail.Episode != nil || detail.OpenPosition.ID != detail.ID {
			return fmt.Errorf("invalid Edge open option detail")
		}
		position := detail.OpenPosition
		if err := validateEdgeOptionOpenSummary(position.EdgeOptionOpenPositionSummary); err != nil {
			return err
		}
		if !validEdgeOptionPositionSide(position.Side) {
			return fmt.Errorf("invalid Edge open option side")
		}
		for _, amount := range []*float64{position.Strike, position.Multiplier, position.Quantity, position.MarkPrice, position.CostBasisMoney, position.OpenPNLBase} {
			if amount != nil && !finite(*amount) {
				return fmt.Errorf("invalid Edge open option amount")
			}
		}
		return nil
	default:
		return fmt.Errorf("invalid Edge option detail kind")
	}
}

func validateEdgeOptionPNL(status string, amount *float64, missing []string) error {
	switch status {
	case "complete":
		if amount == nil {
			return fmt.Errorf("complete Edge option P/L is unavailable")
		}
	case "partial":
		if amount == nil || len(missing) == 0 {
			return fmt.Errorf("partial Edge option P/L lacks evidence state")
		}
	case "unavailable":
		if amount != nil || len(missing) == 0 {
			return fmt.Errorf("unavailable Edge option P/L has an amount")
		}
	default:
		return fmt.Errorf("invalid Edge option P/L status")
	}
	if amount != nil && !finite(*amount) {
		return fmt.Errorf("invalid Edge option P/L amount")
	}
	seen := map[string]bool{}
	for _, reason := range missing {
		if seen[reason] || reason != "realized_pnl" && reason != "open_pnl" && reason != "fx_conversion" && reason != "instrument_metadata" {
			return fmt.Errorf("invalid Edge option missing evidence")
		}
		seen[reason] = true
	}
	return nil
}

func validEdgeOptionIdentity(expiry string, strike *float64, putCall string, missing []string) bool {
	missingInstrument := false
	for _, reason := range missing {
		missingInstrument = missingInstrument || reason == "instrument_metadata"
	}
	if missingInstrument {
		return (expiry == "" || validEdgeDate(expiry)) && (strike == nil || finite(*strike)) && (putCall == "" || putCall == "call" || putCall == "put")
	}
	return validEdgeDate(expiry) && strike != nil && finite(*strike) && (putCall == "call" || putCall == "put")
}

func validEdgeDate(value string) bool {
	parsed, err := time.Parse(time.DateOnly, value)
	return err == nil && parsed.Format(time.DateOnly) == value
}

func validEdgeOptionGrouping(value string) bool {
	return value == "exact_order" || value == "unlinked_execution" || value == "option_event"
}

func validEdgeOptionLifecycle(value string) bool {
	return value == "opening" || value == "closing" || value == "mixed" || value == "event" || value == "unknown"
}

func validEdgeOptionEventType(value string) bool {
	return value == "exercise" || value == "assignment" || value == "expiration" || value == "other"
}

func validEdgeOptionTradeSide(value string) bool {
	return value == "buy" || value == "sell" || value == "unknown"
}

func validEdgeOptionOpenClose(value string) bool {
	return value == "opening" || value == "closing" || value == "unknown"
}

func validEdgeOptionPositionSide(value string) bool {
	return value == "long" || value == "short" || value == "unknown"
}

func validateEdgeHorizonSelection(result EdgeResult) error {
	selection := result.HorizonSelection
	if selection.Mode != "automatic" && selection.Mode != "explicit" {
		return fmt.Errorf("invalid Edge horizon selection mode")
	}
	if result.AutomaticHorizon != (selection.Mode == "automatic") || selection.EligibleChanges < 0 || selection.ScoredChanges < 0 || selection.ScoredChanges > selection.EligibleChanges || selection.LargestActionSample < 0 || selection.LargestActionSample > selection.ScoredChanges || selection.MinimumSample != 3 || !finite(selection.CoveragePct) || selection.CoveragePct < 0 || selection.CoveragePct > 100 || selection.MinimumCoveragePct != 25 || strings.TrimSpace(selection.Reason) == "" {
		return fmt.Errorf("invalid Edge horizon selection")
	}
	wantCoverage := 0.0
	if selection.EligibleChanges > 0 {
		wantCoverage = float64(selection.ScoredChanges) / float64(selection.EligibleChanges) * 100
	}
	wantAdequate := selection.ScoredChanges >= selection.MinimumSample && selection.LargestActionSample >= selection.MinimumSample && wantCoverage >= selection.MinimumCoveragePct
	if math.Abs(selection.CoveragePct-wantCoverage) > 1e-9 || selection.Adequate != wantAdequate || selection.EligibleChanges != result.Coverage.EligibleChanges || selection.ScoredChanges != result.Coverage.ScoredByHorizon[result.HorizonSessions] {
		return fmt.Errorf("inconsistent Edge horizon selection")
	}
	if selection.Mode == "automatic" {
		validReason := selection.Reason == "snapshot_unavailable" || !selection.Adequate && selection.Reason == "best_available" || selection.Adequate && selection.Reason == "longest_adequately_covered"
		if !validReason {
			return fmt.Errorf("invalid automatic Edge horizon reason")
		}
	} else if selection.Reason != "snapshot_unavailable" && selection.Reason != "explicit_override" {
		return fmt.Errorf("invalid explicit Edge horizon reason")
	}
	return nil
}

func validateEdgeMarketContextRollups(rows []EdgeMarketContextRollup) error {
	seen := map[string]bool{}
	for _, row := range rows {
		if !validEdgeMarketContextIdentity(row.Key, row.Label, row.Kind) || seen[row.Key] || row.SampleCount < 1 || row.MedianChangePct == nil || !finite(*row.MedianChangePct) {
			return fmt.Errorf("invalid Edge market context rollup")
		}
		seen[row.Key] = true
		if row.Kind == "volatility_index" {
			if row.MedianChangePoints == nil || !finite(*row.MedianChangePoints) {
				return fmt.Errorf("invalid Edge volatility context rollup")
			}
		} else if row.MedianChangePoints != nil {
			return fmt.Errorf("invalid Edge proxy context rollup")
		}
	}
	return nil
}

func validateEdgeMarketContext(rows []EdgeMarketContext) error {
	seen := map[string]bool{}
	for _, row := range rows {
		if !validEdgeMarketContextIdentity(row.Key, row.Label, row.Kind) || seen[row.Key] || row.StartDay.IsZero() || row.EndDay.IsZero() || !row.StartDay.Before(row.EndDay) || !finite(row.StartClose) || row.StartClose <= 0 || !finite(row.EndClose) || row.EndClose <= 0 || !finite(row.ChangePct) {
			return fmt.Errorf("invalid Edge market context")
		}
		seen[row.Key] = true
		if row.Kind == "volatility_index" {
			if row.ChangePoints == nil || !finite(*row.ChangePoints) {
				return fmt.Errorf("invalid Edge volatility context")
			}
		} else if row.ChangePoints != nil {
			return fmt.Errorf("invalid Edge proxy context")
		}
	}
	return nil
}

func validEdgeMarketContextIdentity(key, label, kind string) bool {
	switch key {
	case "spy":
		return label == "S&P 500 proxy (SPY)" && kind == "market_proxy"
	case "qqq":
		return label == "Nasdaq-100 proxy (QQQ)" && kind == "market_proxy"
	case "dia":
		return label == "Dow proxy (DIA)" && kind == "market_proxy"
	case "vix":
		return label == "CBOE VIX" && kind == "volatility_index"
	default:
		return false
	}
}

func validEdgeMarketContextKey(key string) bool {
	return key == "spy" || key == "qqq" || key == "dia" || key == "vix"
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
