// Package edge is Canary Edge's pure analytical core. It accepts typed broker
// evidence and cached price series, performs no I/O, and returns a deterministic
// result suitable for replay, property tests, and daemon-owned publication.
package edge

import (
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

// Edge schema, action, direction, and exclusion-reason constants define the
// deterministic vocabulary emitted by the analytical core.
const (
	SchemaVersion      = "canary-edge-v1"
	FingerprintVersion = "canary-edge-fp-v1"

	ActionOpen = "open"
	ActionAdd  = "add"
	ActionTrim = "trim"
	ActionExit = "exit"

	DirectionLong  = "long"
	DirectionShort = "short"

	ReasonMissingHorizon         = "missing_horizon"
	ReasonInterveningChange      = "intervening_change"
	ReasonUnsupportedAsset       = "unsupported_asset"
	ReasonCorporateAction        = "corporate_action"
	ReasonMissingFX              = "missing_fx"
	ReasonMarketDataUnavailable  = "market_data_unavailable"
	ReasonQueryFieldMissing      = "query_field_missing"
	ReasonPositionPathUnbalanced = "position_path_unbalanced"
)

// Horizons is the closed set of post-execution trading-session lenses in v1.
var Horizons = [...]int{1, 5, 20}

// DailyBar is one exact-ConID IBKR TRADES daily bar. Day is interpreted as the
// trading session label; only Close participates in v1 scoring.
type DailyBar struct {
	ConID int64     `json:"conid"`
	Day   time.Time `json:"day"`
	Close float64   `json:"close"`
}

// Input contains only typed, already-retained broker evidence and regenerable
// daily bars. WindowDays is 90 or 365.
type Input struct {
	AsOf         time.Time
	WindowDays   int
	BaseCurrency string
	Statements   []flexstmt.Statement
	Bars         map[int64][]DailyBar
}

// Result is the complete deterministic core result for one window. Public
// adapters translate it into rpc.EdgeResult and never recalculate it.
type Result struct {
	SchemaVersion string         `json:"schema_version"`
	AsOf          time.Time      `json:"as_of"`
	WindowDays    int            `json:"window_days"`
	Account       *AccountResult `json:"account,omitempty"`
	Rollups       []ActionRollup `json:"rollups"`
	Findings      []Finding      `json:"findings"`
	Changes       []Change       `json:"changes"`
	Options       []OptionResult `json:"options"`
	Coverage      Coverage       `json:"coverage"`
	Method        Method         `json:"method"`
	Fingerprint   string         `json:"fingerprint"`
	NotExecution  bool           `json:"not_execution"`
}

// AccountResult is the complete base-currency equity change after confirmed
// external flows for the actual available statement boundaries.
type AccountResult struct {
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

// Change is one deterministically classified exact-contract position change.
type Change struct {
	ID              string         `json:"id"`
	ConID           int64          `json:"-"`
	Symbol          string         `json:"symbol"`
	AssetClass      string         `json:"asset_class"`
	Currency        string         `json:"currency,omitempty"`
	Action          string         `json:"action"`
	Direction       string         `json:"direction"`
	ExecutedAt      time.Time      `json:"executed_at"`
	DeltaQuantity   float64        `json:"delta_quantity"`
	PositionBefore  float64        `json:"position_before"`
	PositionAfter   float64        `json:"position_after"`
	ExecutionVWAP   *float64       `json:"execution_vwap,omitempty"`
	Multiplier      *float64       `json:"multiplier,omitempty"`
	DirectCostsBase *float64       `json:"direct_costs_base,omitempty"`
	Scores          []HorizonScore `json:"scores"`
}

// HorizonScore is one observed fixed-price-path comparison or its typed
// unavailability reason. DecisionNotionalBase is the absolute changed
// quantity valued at execution VWAP with the same multiplier and horizon FX
// used by the impact calculation; DecisionImpactPct is impact divided by that
// disclosed notional.
type HorizonScore struct {
	Sessions             int        `json:"sessions"`
	HorizonDay           *time.Time `json:"horizon_day,omitempty"`
	HorizonClose         *float64   `json:"horizon_close,omitempty"`
	HorizonFX            *float64   `json:"horizon_fx,omitempty"`
	DecisionNotionalBase *float64   `json:"decision_notional_base,omitempty"`
	DecisionImpactBase   *float64   `json:"decision_impact_base,omitempty"`
	DecisionImpactPct    *float64   `json:"decision_impact_pct,omitempty"`
	Reason               string     `json:"reason,omitempty"`
}

// ActionRollup aggregates observed horizon results for one action class.
type ActionRollup struct {
	Action   string          `json:"action"`
	Horizons []HorizonRollup `json:"horizons"`
}

// HorizonRollup carries count, total, and median for one action and horizon.
type HorizonRollup struct {
	Sessions    int      `json:"sessions"`
	SampleCount int      `json:"sample_count"`
	TotalBase   *float64 `json:"total_base,omitempty"`
	MedianBase  *float64 `json:"median_base,omitempty"`
}

// Finding is one ranked observed impact. Ranking uses disclosed impact as a
// percentage of execution notional before absolute dollars, so position size
// alone cannot determine the order.
type Finding struct {
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

// OptionResult carries broker-actual option P/L without a synthesized
// historical counterfactual.
type OptionResult struct {
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

// Coverage makes scored, excluded, and unavailable evidence explicit.
type Coverage struct {
	TradeChanges    int            `json:"trade_changes"`
	EligibleChanges int            `json:"eligible_changes"`
	ScoredByHorizon map[int]int    `json:"scored_by_horizon"`
	ReasonCounts    map[string]int `json:"reason_counts"`
	PresentSections []string       `json:"present_sections"`
	MissingSections []string       `json:"missing_sections"`
}

// Method is the disclosure contract that travels with every calculated result.
type Method struct {
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

func defaultMethod() Method {
	return Method{
		Metric:              "Decision price impact",
		Counterfactual:      "Leave the exact-contract pre-trade position unchanged.",
		HorizonDefinition:   "The 1st, 5th, and 20th available IBKR daily closes after the execution session; horizon FX is the latest broker conversion at or before that close, no more than seven calendar days old.",
		HeadlineSelection:   "The action with the most clean observations at the selected horizon; ties use open, add, trim, then exit.",
		FindingRanking:      "Absolute Decision price impact as a percentage of disclosed execution notional, then absolute base-currency impact, then opaque change ID.",
		AccountDefinition:   "Ending equity minus starting equity minus statement-confirmed external flows.",
		Exclusions:          "Decision price impact excludes distributions, financing and borrow, market impact, and effects outside the fixed price-path comparison.",
		OptionsMethod:       "Broker-actual realized and open P/L only; no historical option counterfactual is synthesized.",
		NoCausalClaim:       true,
		NoPredictiveClaim:   true,
		NotInvestmentAdvice: true,
	}
}
