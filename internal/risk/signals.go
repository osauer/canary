// Package risk provides pure risk-policy evaluation and related domain
// contracts. It performs no I/O and owns no broker or runtime state; callers
// supply typed observations and policies, and retain responsibility for state
// mutation, persistence, transport, and enforcement.
//
// Evaluators preserve missing, stale, and unapproved inputs as explicit
// non-passing outcomes. Fingerprint helpers identify the complete normalized
// policy projections used by callers.
package risk

// SignalDirection describes whether a signal supports defense, deployment,
// rebalancing, mixed action, or data-quality confirmation.
type SignalDirection string

// DirectionDefensive and the related constants enumerate signal directions.
const (
	DirectionDefensive    SignalDirection = "defensive"
	DirectionConstructive SignalDirection = "constructive"
	DirectionRebalance    SignalDirection = "rebalance"
	DirectionMixed        SignalDirection = "mixed"
	DirectionDataQuality  SignalDirection = "data_quality"
)

// PortfolioPosture summarizes the portfolio response implied by a set of
// signals. The zero value is unspecified.
type PortfolioPosture string

// PortfolioPostureNeutral and the related constants enumerate portfolio
// response summaries.
const (
	PortfolioPostureNeutral           PortfolioPosture = "neutral"
	PortfolioPostureThreat            PortfolioPosture = "threat"
	PortfolioPostureRebalance         PortfolioPosture = "rebalance"
	PortfolioPostureOpportunity       PortfolioPosture = "opportunity"
	PortfolioPostureThreatOpportunity PortfolioPosture = "threat_opportunity"
	PortfolioPostureConfirmData       PortfolioPosture = "confirm_data"
)

// SignalSeverity ranks the urgency of a risk signal. The zero value is
// unspecified.
type SignalSeverity string

// SeverityObserve and the related constants enumerate signal urgency.
const (
	SeverityObserve SignalSeverity = "observe"
	SeverityWatch   SignalSeverity = "watch"
	SeverityAct     SignalSeverity = "act"
	SeverityUrgent  SignalSeverity = "urgent"
)

// PlannerMode identifies the action family a downstream planner may prepare.
// It conveys no broker-write authority.
type PlannerMode string

// PlannerModeNone and the related constants enumerate planner action modes.
const (
	PlannerModeNone        PlannerMode = "none"
	PlannerModeStage       PlannerMode = "stage"
	PlannerModeDefend      PlannerMode = "defend"
	PlannerModeRebalance   PlannerMode = "rebalance"
	PlannerModeDeploy      PlannerMode = "deploy"
	PlannerModeConfirmData PlannerMode = "confirm_data"
)

// PlannerReadiness describes how close a planner is to presenting an action.
// Ready still conveys no broker-write authority.
type PlannerReadiness string

// PlannerReadinessNone and the related constants enumerate preparation states.
const (
	PlannerReadinessNone     PlannerReadiness = "none"
	PlannerReadinessWatch    PlannerReadiness = "watch"
	PlannerReadinessPrestage PlannerReadiness = "prestage"
	PlannerReadinessReady    PlannerReadiness = "ready"
	PlannerReadinessBlocked  PlannerReadiness = "blocked"
)

// SignalID identifies a stable risk or data-quality condition.
type SignalID string

// SignalMarginCushionLow and the related constants identify supported risk and
// data-quality conditions.
const (
	SignalMarginCushionLow              SignalID = "margin_cushion_low"
	SignalLookAheadCushionLow           SignalID = "lookahead_cushion_low"
	SignalMarketSelloffViolent          SignalID = "market_selloff_violent"
	SignalVolSpikeConfirmed             SignalID = "vol_spike_confirmed"
	SignalMarketRallyViolent            SignalID = "market_rally_violent"
	SignalVolCrushConfirmed             SignalID = "vol_crush_confirmed"
	SignalRegimeStressConfirmed         SignalID = "regime_stress_confirmed"
	SignalRegimeStressEarly             SignalID = "regime_stress_early"
	SignalFXCarryUnwind                 SignalID = "fx_carry_unwind"
	SignalGammaRed                      SignalID = "gamma_red"
	SignalGrossExposureHigh             SignalID = "gross_exposure_high"
	SignalNetDeltaHigh                  SignalID = "net_delta_high"
	SignalGrossDeltaHigh                SignalID = "gross_delta_high"
	SignalSingleNameExposureHigh        SignalID = "single_name_exposure_high"
	SignalSingleNameDeltaHigh           SignalID = "single_name_delta_high"
	SignalHeldUnderlyingPnLShock        SignalID = "held_underlying_pnl_shock"
	SignalHeldOptionExpiryConcentration SignalID = "held_option_expiry_concentration"
	SignalHeldLiquidityDegraded         SignalID = "held_liquidity_degraded"
	SignalOptionGreeksDegraded          SignalID = "option_greeks_degraded"
	SignalShortConvexityHigh            SignalID = "short_convexity_high"
	SignalPortfolioPnLShock             SignalID = "portfolio_pnl_shock"
	SignalRiskDataDegraded              SignalID = "risk_data_degraded"
	SignalMarketDataStale               SignalID = "market_data_stale"
)

// Signal is a typed, serializable risk observation. Optional numeric fields
// remain nil when unavailable; callers must not interpret their absence as
// zero. BlockedBy explains which missing or degraded inputs limited the
// signal.
type Signal struct {
	ID               SignalID         `json:"id"`
	Direction        SignalDirection  `json:"direction"`
	Posture          PortfolioPosture `json:"posture,omitempty"`
	Severity         SignalSeverity   `json:"severity"`
	Subject          string           `json:"subject,omitempty"`
	Metric           string           `json:"metric,omitempty"`
	Observed         *float64         `json:"observed,omitempty"`
	Threshold        *float64         `json:"threshold,omitempty"`
	Target           *float64         `json:"target,omitempty"`
	Unit             string           `json:"unit,omitempty"`
	Evidence         string           `json:"evidence,omitempty"`
	Confidence       string           `json:"confidence,omitempty"`
	ConfidenceImpact string           `json:"confidence_impact,omitempty"`
	BlockedBy        []string         `json:"blocked_by,omitempty"`
}
