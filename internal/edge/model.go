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
	SchemaVersion      = "canary-edge-v3"
	FingerprintVersion = "canary-edge-fp-v3"

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

	// MinimumPatternSample is the smallest repeated-action sample that may
	// receive a pattern label.
	MinimumPatternSample = 3
	// MinimumAutomaticCoveragePct is the eligible-change coverage required for
	// automatic horizon selection.
	MinimumAutomaticCoveragePct = 25.0
	// MinimumFindingNotionalEquityPct is the starting-equity notional floor for
	// a ranked finding.
	MinimumFindingNotionalEquityPct = 0.25
	// MinimumFindingImpactEquityPct is the starting-equity impact floor for a
	// ranked finding and a headline median.
	MinimumFindingImpactEquityPct = 0.02
	// MinimumPatternTotalImpactEquityPct is the starting-equity aggregate floor
	// for a headline pattern.
	MinimumPatternTotalImpactEquityPct = 0.10
)

// Horizons is the closed set of post-execution trading-session lenses.
var Horizons = [...]int{1, 5, 20}

// Market-context kind constants distinguish broad ETF proxies from the VIX
// volatility index without assigning either class causal authority.
const (
	// MarketContextKindProxy identifies an ETF used as a named market proxy.
	MarketContextKindProxy = "market_proxy"
	// MarketContextKindVolatility identifies a volatility-index context series.
	MarketContextKindVolatility = "volatility_index"

	OptionGroupingExactOrder        = "exact_order"
	OptionGroupingUnlinkedExecution = "unlinked_execution"
	OptionGroupingEvent             = "option_event"

	OptionLifecycleOpening = "opening"
	OptionLifecycleClosing = "closing"
	OptionLifecycleMixed   = "mixed"
	OptionLifecycleEvent   = "event"
	OptionLifecycleUnknown = "unknown"

	OptionPNLComplete    = "complete"
	OptionPNLPartial     = "partial"
	OptionPNLUnavailable = "unavailable"

	OptionMissingRealizedPNL = "realized_pnl"
	OptionMissingOpenPNL     = "open_pnl"
	OptionMissingFX          = "fx_conversion"
	OptionMissingInstrument  = "instrument_metadata"
)

// marketBenchmarks is the closed, informational context set. ETF symbols are
// deliberately named as proxies rather than presented as the cash indices.
var marketBenchmarks = [...]MarketBenchmark{
	{Key: "spy", Symbol: "SPY", Label: "S&P 500 proxy (SPY)", Kind: MarketContextKindProxy},
	{Key: "qqq", Symbol: "QQQ", Label: "Nasdaq-100 proxy (QQQ)", Kind: MarketContextKindProxy},
	{Key: "dia", Symbol: "DIA", Label: "Dow proxy (DIA)", Kind: MarketContextKindProxy},
	{Key: "vix", Symbol: "VIX", Label: "CBOE VIX", Kind: MarketContextKindVolatility},
}

// MarketBenchmarks returns the fixed context catalog without exposing mutable
// analytical authority to daemon or adapter packages.
func MarketBenchmarks() []MarketBenchmark {
	return append([]MarketBenchmark(nil), marketBenchmarks[:]...)
}

// MarketBenchmark identifies one fixed market-context series.
type MarketBenchmark struct {
	Key    string `json:"key"`
	Symbol string `json:"symbol"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`
}

// DailyBar is one exact-ConID IBKR TRADES daily bar. Day is interpreted as the
// trading session label; only Close participates in decision scoring or
// informational market context.
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
	ContextBars  map[string][]DailyBar
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
	Options       OptionReview   `json:"options"`
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
	Sessions             int             `json:"sessions"`
	HorizonDay           *time.Time      `json:"horizon_day,omitempty"`
	HorizonClose         *float64        `json:"horizon_close,omitempty"`
	HorizonFX            *float64        `json:"horizon_fx,omitempty"`
	DecisionNotionalBase *float64        `json:"decision_notional_base,omitempty"`
	DecisionImpactBase   *float64        `json:"decision_impact_base,omitempty"`
	DecisionImpactPct    *float64        `json:"decision_impact_pct,omitempty"`
	MarketContext        []MarketContext `json:"market_context,omitempty"`
	Reason               string          `json:"reason,omitempty"`
}

// MarketContext is one benchmark move over the same observed interval as a
// scored decision. It is explanatory evidence only and never changes Decision
// price impact.
type MarketContext struct {
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

// MarketContextRollup summarizes the benchmark path accompanying one action
// and horizon without treating that path as an explanatory or causal factor.
type MarketContextRollup struct {
	Key                string   `json:"key"`
	Label              string   `json:"label"`
	Kind               string   `json:"kind"`
	SampleCount        int      `json:"sample_count"`
	MedianChangePct    *float64 `json:"median_change_pct,omitempty"`
	MedianChangePoints *float64 `json:"median_change_points,omitempty"`
}

// ActionRollup aggregates observed horizon results for one action class.
type ActionRollup struct {
	Action   string          `json:"action"`
	Horizons []HorizonRollup `json:"horizons"`
}

// HorizonRollup carries count, total, and median for one action and horizon.
type HorizonRollup struct {
	Sessions      int                   `json:"sessions"`
	SampleCount   int                   `json:"sample_count"`
	TotalBase     *float64              `json:"total_base,omitempty"`
	MedianBase    *float64              `json:"median_base,omitempty"`
	MarketContext []MarketContextRollup `json:"market_context,omitempty"`
}

// Finding is one materially gated, ranked observed impact. After the fixed
// account-relative floors, ranking uses disclosed impact as a percentage of
// execution notional before absolute dollars, so position size alone cannot
// determine the order.
type Finding struct {
	ChangeID             string          `json:"change_id"`
	Symbol               string          `json:"symbol"`
	Action               string          `json:"action"`
	Direction            string          `json:"direction"`
	ExecutedAt           time.Time       `json:"executed_at"`
	HorizonSessions      int             `json:"horizon_sessions"`
	DecisionNotionalBase float64         `json:"decision_notional_base"`
	DecisionImpactBase   float64         `json:"decision_impact_base"`
	DecisionImpactPct    float64         `json:"decision_impact_pct"`
	MarketContext        []MarketContext `json:"market_context,omitempty"`
}

// OptionReview keeps windowed realized activity separate from the latest
// point-in-time open-position snapshot. The two scopes deliberately have no
// combined P/L field.
type OptionReview struct {
	Coverage OptionCoverage       `json:"coverage"`
	Realized OptionRealizedReview `json:"realized"`
	Open     OptionOpenReview     `json:"open"`
}

// OptionCoverage accounts for option execution and lifecycle evidence that is
// not itself a realized result. In particular, opening-only zero-P/L episodes
// remain visible here instead of inflating the ranked result set.
type OptionCoverage struct {
	ExecutionEpisodes       int `json:"execution_episodes"`
	OpeningEpisodes         int `json:"opening_episodes"`
	OpeningOnlyZeroEpisodes int `json:"opening_only_zero_episodes"`
	ClosingEpisodes         int `json:"closing_episodes"`
	MixedEpisodes           int `json:"mixed_episodes"`
	UnknownEpisodes         int `json:"unknown_episodes"`
	EventEpisodes           int `json:"event_episodes"`
}

// OptionRealizedReview summarizes broker-reported realized P/L for bounded
// episodes. KnownPNLBase is explicitly only the sum of available evidence when
// PartialCount or UnavailableCount is non-zero.
type OptionRealizedReview struct {
	KnownPNLBase     *float64        `json:"known_pnl_base,omitempty"`
	PositiveCount    int             `json:"positive_count"`
	NegativeCount    int             `json:"negative_count"`
	FlatCount        int             `json:"flat_count"`
	CompleteCount    int             `json:"complete_count"`
	PartialCount     int             `json:"partial_count"`
	UnavailableCount int             `json:"unavailable_count"`
	Episodes         []OptionEpisode `json:"episodes"`
}

// OptionOpenReview summarizes the latest authoritative Flex Open Positions
// snapshot. SnapshotDate is zero only when no open option row exists.
type OptionOpenReview struct {
	SnapshotDate     time.Time            `json:"snapshot_date,omitzero"`
	KnownPNLBase     *float64             `json:"known_pnl_base,omitempty"`
	PositiveCount    int                  `json:"positive_count"`
	NegativeCount    int                  `json:"negative_count"`
	FlatCount        int                  `json:"flat_count"`
	CompleteCount    int                  `json:"complete_count"`
	UnavailableCount int                  `json:"unavailable_count"`
	Positions        []OptionOpenPosition `json:"positions"`
}

// OptionEpisode is one exact-order execution group, one unlinked execution,
// or one exact OptionEAE lifecycle event. It never claims cross-order strategy
// identity.
type OptionEpisode struct {
	ID              string             `json:"id"`
	Grouping        string             `json:"grouping"`
	Lifecycle       string             `json:"lifecycle"`
	EventType       string             `json:"event_type,omitempty"`
	Underlying      string             `json:"underlying,omitempty"`
	ActivityFrom    time.Time          `json:"activity_from"`
	ActivityTo      time.Time          `json:"activity_to"`
	RealizedPNLBase *float64           `json:"realized_pnl_base,omitempty"`
	PNLStatus       string             `json:"pnl_status"`
	MissingEvidence []string           `json:"missing_evidence"`
	Legs            []OptionEpisodeLeg `json:"legs"`
}

// OptionEpisodeLeg carries the broker facts needed to identify and inspect an
// episode without exposing ConID, order, execution, trade, or account identity.
type OptionEpisodeLeg struct {
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

// OptionOpenPosition is one exact-contract row from the latest authoritative
// Flex open-position snapshot.
type OptionOpenPosition struct {
	ID              string    `json:"id"`
	Symbol          string    `json:"symbol"`
	Underlying      string    `json:"underlying,omitempty"`
	SnapshotDate    time.Time `json:"snapshot_date"`
	Expiry          string    `json:"expiry,omitempty"`
	Strike          *float64  `json:"strike,omitempty"`
	PutCall         string    `json:"put_call,omitempty"`
	Multiplier      *float64  `json:"multiplier,omitempty"`
	Side            string    `json:"side,omitempty"`
	Quantity        *float64  `json:"quantity,omitempty"`
	MarkPrice       *float64  `json:"mark_price,omitempty"`
	CostBasisMoney  *float64  `json:"cost_basis_money,omitempty"`
	Currency        string    `json:"currency,omitempty"`
	OpenPNLBase     *float64  `json:"open_pnl_base,omitempty"`
	PNLStatus       string    `json:"pnl_status"`
	MissingEvidence []string  `json:"missing_evidence"`
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

func defaultMethod() Method {
	return Method{
		Metric:              "Decision price impact",
		Counterfactual:      "Leave the exact-contract pre-trade position unchanged.",
		HorizonDefinition:   "The 1st, 5th, and 20th available IBKR daily closes after the execution session; horizon FX is the latest broker conversion at or before that close, no more than seven calendar days old.",
		HeadlineSelection:   "Among actions that clear the evidence and account-materiality gates, select the one with the most clean observations at the selected horizon; ties use open, add, trim, then exit. Strength or drag requires at least 3 observations, absolute total impact of at least 0.10% of starting equity, and an absolute median impact of at least 0.02% of starting equity.",
		FindingRanking:      "After account-relative materiality gates, absolute Decision price impact as a percentage of disclosed execution notional, then absolute base-currency impact, then opaque change ID.",
		MaterialityGate:     "A ranked finding requires decision notional of at least 0.25% of starting equity and absolute Decision price impact of at least 0.02% of starting equity.",
		AutomaticHorizon:    "Choose the longest of 20, 5, and 1 sessions with at least 3 clean observations, one action represented at least 3 times, and at least 25% of eligible changes scored; otherwise show the best-covered horizon without labeling strength or drag.",
		MarketContext:       "For SPY, QQQ, DIA, and VIX, compare the last daily close before the execution session with the close on the decision horizon day. QQQ and DIA are ETF proxies. Context is informational and never changes Decision price impact.",
		AccountDefinition:   "Ending equity minus starting equity minus statement-confirmed external flows.",
		Exclusions:          "Decision price impact excludes distributions, financing and borrow, market impact, and effects outside the fixed price-path comparison.",
		OptionsMethod:       "Broker-reported realized option episodes and the latest dated open-position P/L snapshot are separate. Opening-only zero-P/L executions remain coverage, gains and losses are seated before magnitude ranking, missing evidence is never zero-filled, and no historical option counterfactual or cross-order strategy identity is synthesized.",
		NoCausalClaim:       true,
		NoPredictiveClaim:   true,
		NotInvestmentAdvice: true,
	}
}
