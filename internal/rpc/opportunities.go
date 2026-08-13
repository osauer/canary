package rpc

import (
	"time"
)

// Opportunity method names and allowlisted policy, snapshot, action, state,
// position-effect, and risk-change values form a stable wire vocabulary.
const (
	MethodOpportunitiesStatus          = "opportunities.status"
	MethodOpportunitiesSnapshot        = "opportunities.snapshot"
	MethodOpportunitiesRefresh         = "opportunities.refresh"
	MethodOpportunitiesPreviewExercise = "opportunities.preview_exercise"
	MethodOpportunitiesSubmitExercise  = "opportunities.submit_exercise"
	MethodOpportunitiesIgnore          = "opportunities.ignore"

	// OpportunityPolicyFingerprintVersion identifies a semantic fingerprint projection.
	OpportunityPolicyFingerprintVersion = "opportunity-policy-fp-v1"

	OpportunityPolicyStatusActive   = "active"
	OpportunityPolicyStatusDefault  = "default"
	OpportunityPolicyStatusDrift    = "drift"
	OpportunityPolicyStatusError    = "error"
	OpportunityPolicyStatusDisabled = "disabled"

	OpportunitySnapshotKind = "ibkr.opportunity_snapshot"
	// OpportunitySnapshotSchemaVersion identifies a stable wire schema.
	OpportunitySnapshotSchemaVersion = "opportunity-snapshot-v1"
	OpportunityStatusKind            = "ibkr.opportunity_status"

	OpportunityBucketOptionExercise = "option_exercise"

	OpportunityStateGenerated = "generated"
	OpportunityStateBlocked   = "blocked"

	OpportunityActionExercise = "EXERCISE"

	ExerciseActionExercise = 1
	ExerciseActionLapse    = 2

	ExercisePositionEffectClose    = "close"
	ExercisePositionEffectReduce   = "reduce"
	ExercisePositionEffectOpen     = "open"
	ExercisePositionEffectIncrease = "increase"
	ExercisePositionEffectFlip     = "flip"
	ExercisePositionEffectUnknown  = "unknown"

	ExerciseRiskChangeClosed    = "closed"
	ExerciseRiskChangeReduced   = "reduced"
	ExerciseRiskChangeOpened    = "opened"
	ExerciseRiskChangeIncreased = "increased"
	ExerciseRiskChangeFlipped   = "flipped"
	ExerciseRiskChangeUnknown   = "unknown"
)

// OpportunityPolicyStatus reports the loaded policy identity and blockers. A
// status of active is required before absence of blockers is meaningful.
type OpportunityPolicyStatus struct {
	Kind          string           `json:"kind,omitempty"`
	Status        string           `json:"status"`
	PolicyID      string           `json:"policy_id,omitempty"`
	PolicyVersion int              `json:"policy_version,omitempty"`
	Profile       string           `json:"profile,omitempty"`
	Fingerprint   Fingerprint      `json:"fingerprint,omitzero"`
	Source        string           `json:"source,omitempty"`
	Path          string           `json:"path,omitempty"`
	LoadedAt      time.Time        `json:"loaded_at,omitzero"`
	LastCheckedAt time.Time        `json:"last_checked_at,omitzero"`
	Message       string           `json:"message,omitempty"`
	Blockers      []TradingBlocker `json:"blockers,omitempty"`
}

// OpportunityStatus combines opportunity-policy and trading readiness. It is
// status evidence only and does not authorize exercise submission.
type OpportunityStatus struct {
	Kind           string                  `json:"kind,omitempty"`
	AsOf           time.Time               `json:"as_of,omitzero"`
	Enabled        bool                    `json:"enabled"`
	HotReload      bool                    `json:"hot_reload"`
	ReloadInterval string                  `json:"reload_interval,omitempty"`
	RefreshCadence string                  `json:"refresh_cadence,omitempty"`
	Policy         OpportunityPolicyStatus `json:"policy"`
	Trading        TradingStatus           `json:"trading"`
	Blocked        bool                    `json:"blocked"`
	Blockers       []TradingBlocker        `json:"blockers,omitempty"`
}

// OpportunitySourceFingerprints carries optional semantic identities for the
// account and position snapshots used to derive an opportunity revision.
type OpportunitySourceFingerprints struct {
	Account   *Fingerprint `json:"account,omitempty"`
	Positions *Fingerprint `json:"positions,omitempty"`
}

// OpportunitySnapshot is one daemon-authored, account-and-mode-scoped revision.
// LoadedFromState identifies retained output; callers must still honor status
// and blockers rather than treating persistence as freshness.
type OpportunitySnapshot struct {
	Kind               string                        `json:"kind"`
	SchemaVersion      string                        `json:"schema_version"`
	AsOf               time.Time                     `json:"as_of"`
	Revision           string                        `json:"revision"`
	AccountID          string                        `json:"account_id,omitempty"`
	AccountMode        string                        `json:"account_mode,omitempty"`
	PolicyID           string                        `json:"policy_id,omitempty"`
	PolicyVersion      int                           `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint                   `json:"policy_fingerprint,omitzero"`
	PolicyStatus       OpportunityPolicyStatus       `json:"policy_status"`
	Status             OpportunityStatus             `json:"status"`
	Trading            TradingStatus                 `json:"trading"`
	SourceFingerprints OpportunitySourceFingerprints `json:"source_fingerprints,omitzero"`
	Opportunities      []Opportunity                 `json:"opportunities"`
	Counts             OpportunityCounts             `json:"counts"`
	Blockers           []TradingBlocker              `json:"blockers,omitempty"`
	LoadedFromState    bool                          `json:"loaded_from_state,omitempty"`
}

// OpportunityCounts summarizes the opportunities in the enclosing revision.
type OpportunityCounts struct {
	Total                int     `json:"total"`
	Actionable           int     `json:"actionable"`
	Blocked              int     `json:"blocked"`
	OptionExercise       int     `json:"option_exercise"`
	ExpectedGain         float64 `json:"expected_gain,omitempty"`
	ExpectedGainCurrency string  `json:"expected_gain_currency,omitempty"`
}

// Opportunity is an advisory exercise candidate bound to its key and revision.
// It is not a preview token or exercise authorization.
type Opportunity struct {
	Key                      string                        `json:"key"`
	Revision                 string                        `json:"revision"`
	State                    string                        `json:"state"`
	Bucket                   string                        `json:"bucket"`
	Rank                     int                           `json:"rank"`
	Symbol                   string                        `json:"symbol"`
	SecType                  string                        `json:"sec_type"`
	Action                   string                        `json:"action"`
	ExerciseAction           int                           `json:"exercise_action"`
	Quantity                 int                           `json:"quantity"`
	MaxQuantity              int                           `json:"max_quantity"`
	PositionQuantity         float64                       `json:"position_quantity"`
	PositionEffect           string                        `json:"position_effect"`
	UnderlyingQuantityBefore float64                       `json:"underlying_quantity_before"`
	UnderlyingQuantityAfter  float64                       `json:"underlying_quantity_after"`
	UnderlyingShareChange    float64                       `json:"underlying_share_change"`
	PostExerciseRisk         *OpportunityPostExerciseRisk  `json:"post_exercise_risk,omitempty"`
	Contract                 ContractParams                `json:"contract"`
	UnderlyingContract       ContractParams                `json:"underlying_contract"`
	ExpectedGain             float64                       `json:"expected_gain,omitempty"`
	ExpectedGainCurrency     string                        `json:"expected_gain_currency,omitempty"`
	RequiredCash             float64                       `json:"required_cash,omitempty"`
	RequiredCashCurrency     string                        `json:"required_cash_currency,omitempty"`
	IntrinsicValue           float64                       `json:"intrinsic_value,omitempty"`
	CloseValue               float64                       `json:"close_value,omitempty"`
	OptionBid                *float64                      `json:"option_bid,omitempty"`
	UnderlyingBid            *float64                      `json:"underlying_bid,omitempty"`
	UnderlyingAsk            *float64                      `json:"underlying_ask,omitempty"`
	Reason                   string                        `json:"reason"`
	Details                  []string                      `json:"details,omitempty"`
	Score                    float64                       `json:"score,omitempty"`
	PolicyID                 string                        `json:"policy_id,omitempty"`
	PolicyVersion            int                           `json:"policy_version,omitempty"`
	PolicyFingerprint        Fingerprint                   `json:"policy_fingerprint,omitzero"`
	SourceFingerprints       OpportunitySourceFingerprints `json:"source_fingerprints,omitzero"`
	Blockers                 []TradingBlocker              `json:"blockers,omitempty"`
	CreatedAt                time.Time                     `json:"created_at,omitzero"`
	// PortfolioGeneration and PortfolioAccount are daemon-only execution
	// authority. They are never serialized into advisory snapshots.
	PortfolioGeneration uint64 `json:"-"`
	PortfolioAccount    string `json:"-"`
}

// OpportunityPostExerciseRisk is advisory context for what exercising a long
// option would do to the underlying stock/ETF exposure. It does not authorize
// or block submit; preview/submit gates remain daemon-owned and broker-gated.
type OpportunityPostExerciseRisk struct {
	Underlying                      string   `json:"underlying,omitempty"`
	BeforeQuantity                  float64  `json:"before_quantity"`
	AfterQuantity                   float64  `json:"after_quantity"`
	ShareChange                     float64  `json:"share_change"`
	PositionEffect                  string   `json:"position_effect,omitempty"`
	RiskChange                      string   `json:"risk_change,omitempty"`
	RiskOpened                      bool     `json:"risk_opened,omitempty"`
	RiskIncreased                   bool     `json:"risk_increased,omitempty"`
	RiskFlipped                     bool     `json:"risk_flipped,omitempty"`
	ProtectionReviewNeeded          bool     `json:"protection_review_needed"`
	ProtectionReviewReason          string   `json:"protection_review_reason,omitempty"`
	ProtectionCoverageState         string   `json:"protection_coverage_state,omitempty"`
	CurrentProtectedQuantity        float64  `json:"current_protected_quantity,omitempty"`
	CurrentUnprotectedQuantity      float64  `json:"current_unprotected_quantity,omitempty"`
	CurrentUnprotectedNotionalBase  *float64 `json:"current_unprotected_notional_base,omitempty"`
	UnprotectedNotionalBaseCurrency string   `json:"unprotected_notional_base_currency,omitempty"`
	WarningCodes                    []string `json:"warning_codes,omitempty"`
}

// OpportunitySnapshotParams controls adapter rendering only; Show does not
// expand daemon authority or eligibility.
type OpportunitySnapshotParams struct {
	Show bool `json:"show,omitempty"`
}

// OpportunityRefreshParams requests recomputation; Show controls rendering.
type OpportunityRefreshParams struct {
	Show bool `json:"show,omitempty"`
}

// OpportunityExercisePreviewParams identifies an exact candidate revision for
// gated broker preview. Origin is audit evidence, not authority by itself.
type OpportunityExercisePreviewParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

// OpportunityExercisePreviewResult reports daemon and broker eligibility. The
// raw one-shot token is returned only to the requesting client; TokenID is its
// non-authorizing audit identifier.
type OpportunityExercisePreviewResult struct {
	Accepted              bool             `json:"accepted"`
	Opportunity           Opportunity      `json:"opportunity"`
	PreviewToken          string           `json:"preview_token,omitempty"`
	PreviewTokenID        string           `json:"preview_token_id,omitempty"`
	TokenMinted           bool             `json:"token_minted"`
	PreviewTokenExpiresAt time.Time        `json:"preview_token_expires_at,omitzero"`
	SubmitEligible        bool             `json:"submit_eligible"`
	Blockers              []TradingBlocker `json:"blockers,omitempty"`
	AsOf                  time.Time        `json:"as_of"`
}

// OpportunityExerciseSubmitParams requests gated exercise of an exact revision.
// The daemon revalidates current authority; prior preview is not submit consent.
type OpportunityExerciseSubmitParams struct {
	Key          string `json:"key"`
	Revision     string `json:"revision"`
	Quantity     int    `json:"quantity,omitempty"`
	PreviewToken string `json:"preview_token"`
	TimeoutMs    int    `json:"timeout_ms,omitempty"`
	Origin       string `json:"origin,omitempty"`
}

// OpportunityExerciseSubmitResult reports the terminal outcome of a gated
// submission request. Accepted is false when blockers prevented submission.
type OpportunityExerciseSubmitResult struct {
	Accepted       bool                              `json:"accepted"`
	Opportunity    Opportunity                       `json:"opportunity"`
	Preview        *OpportunityExercisePreviewResult `json:"preview,omitempty"`
	PreviewTokenID string                            `json:"preview_token_id,omitempty"`
	OrderRef       string                            `json:"order_ref,omitempty"`
	Blockers       []TradingBlocker                  `json:"blockers,omitempty"`
	Message        string                            `json:"message,omitempty"`
	AsOf           time.Time                         `json:"as_of"`
}

// OpportunityIgnoreParams dismisses an advisory candidate revision; it does
// not alter positions or broker orders.
type OpportunityIgnoreParams struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// OpportunityIgnoreResult reports whether the advisory dismissal was accepted.
type OpportunityIgnoreResult struct {
	Accepted bool      `json:"accepted"`
	Key      string    `json:"key"`
	Revision string    `json:"revision,omitempty"`
	Message  string    `json:"message,omitempty"`
	AsOf     time.Time `json:"as_of"`
}

// Trade-proposal method names and allowlisted policy, snapshot, bucket, and
// proposal-state values form a stable daemon wire vocabulary.
const (
	MethodAutoTradeStatus        = "auto_trade.status"
	MethodTradeProposalsSnapshot = "trade_proposals.snapshot"
	MethodTradeProposalsRefresh  = "trade_proposals.refresh"
	MethodTradeProposalsPreview  = "trade_proposals.preview"
	MethodTradeProposalsSubmit   = "trade_proposals.submit"
	MethodTradeProposalsIgnore   = "trade_proposals.ignore"
	// MethodTradeProposalsRequestStop asks the proposal engine to generate a
	// protective trailing-stop proposal for one named position now. It is a
	// generation verb: the result flows into the ordinary preview/submit gates.
	MethodTradeProposalsRequestStop = "trade_proposals.request_stop"
	// MethodTradeProposalsReducePreview starts a discretionary, user-initiated
	MethodTradeProposalsReducePreview = "trade_proposals.reduce_preview"
	MethodTradeProposalsReduceSubmit  = "trade_proposals.reduce_submit"
	// MethodTradeProposalsReducePortfolioPreview starts the one-tap risk-off
	MethodTradeProposalsReducePortfolioPreview = "trade_proposals.reduce_portfolio_preview"
	MethodTradeProposalsReducePortfolioSubmit  = "trade_proposals.reduce_portfolio_submit"

	// ProtectionPolicyFingerprintVersion identifies a semantic fingerprint projection.
	ProtectionPolicyFingerprintVersion = "protection-policy-fp-v1"

	ProtectionPolicyStatusActive   = "active"
	ProtectionPolicyStatusDefault  = "default"
	ProtectionPolicyStatusDrift    = "drift"
	ProtectionPolicyStatusError    = "error"
	ProtectionPolicyStatusDisabled = "disabled"

	TradeProposalSnapshotKind = "ibkr.trade_proposal_snapshot"
	// TradeProposalSnapshotSchemaVersion identifies the account-and-mode-scoped
	// snapshot schema. Persisted snapshots without concrete scope fail closed.
	TradeProposalSnapshotSchemaVersion = "trade-proposal-snapshot-v2"

	TradeProposalBucketThetaHygiene     = "theta_hygiene"
	TradeProposalBucketRiskReduction    = "risk_reduction"
	TradeProposalBucketTrailingStop     = "trailing_stop"
	TradeProposalBucketOptionLossExit   = "option_loss_exit"
	TradeProposalBucketOptionExitReview = "option_exit_review"

	TradeProposalStateGenerated = "generated"
	TradeProposalStateBlocked   = "blocked"
)

// ProtectionPolicyStatus reports the loaded policy identity and blockers.
type ProtectionPolicyStatus struct {
	Kind          string           `json:"kind,omitempty"`
	Status        string           `json:"status"`
	PolicyID      string           `json:"policy_id,omitempty"`
	PolicyVersion int              `json:"policy_version,omitempty"`
	Profile       string           `json:"profile,omitempty"`
	Fingerprint   Fingerprint      `json:"fingerprint,omitzero"`
	Source        string           `json:"source,omitempty"`
	Path          string           `json:"path,omitempty"`
	LoadedAt      time.Time        `json:"loaded_at,omitzero"`
	LastCheckedAt time.Time        `json:"last_checked_at,omitzero"`
	Message       string           `json:"message,omitempty"`
	Blockers      []TradingBlocker `json:"blockers,omitempty"`
}

// AutoTradeStatus combines proposal generation and trading readiness. It is
// observational and does not itself authorize a broker write.
type AutoTradeStatus struct {
	Kind             string                 `json:"kind,omitempty"`
	AsOf             time.Time              `json:"as_of,omitzero"`
	Trading          TradingStatus          `json:"trading"`
	ProposalsEnabled bool                   `json:"proposals_enabled"`
	FastPathEnabled  bool                   `json:"fast_path_enabled"`
	HotReload        bool                   `json:"hot_reload"`
	ReloadInterval   string                 `json:"reload_interval,omitempty"`
	ProposalCadence  string                 `json:"proposal_cadence,omitempty"`
	Policy           ProtectionPolicyStatus `json:"policy"`
	Blocked          bool                   `json:"blocked"`
	Blockers         []TradingBlocker       `json:"blockers,omitempty"`
}

// TradeProposalSourceFingerprints identifies the snapshots used to derive a
// proposal revision. Nil members mean that source supplied no identity.
type TradeProposalSourceFingerprints struct {
	Account      *Fingerprint `json:"account,omitempty"`
	Positions    *Fingerprint `json:"positions,omitempty"`
	Rulebook     *Fingerprint `json:"rulebook,omitempty"`
	Regime       *Fingerprint `json:"regime,omitempty"`
	MarketEvents *Fingerprint `json:"market_events,omitempty"`
}

// TradeProposalSnapshot is one daemon-authored, account-and-mode-scoped
type TradeProposalSnapshot struct {
	Kind               string                          `json:"kind"`
	SchemaVersion      string                          `json:"schema_version"`
	AsOf               time.Time                       `json:"as_of"`
	Revision           string                          `json:"revision"`
	AccountID          string                          `json:"account_id,omitempty"`
	AccountMode        string                          `json:"account_mode,omitempty"`
	PolicyID           string                          `json:"policy_id,omitempty"`
	PolicyVersion      int                             `json:"policy_version,omitempty"`
	PolicyFingerprint  Fingerprint                     `json:"policy_fingerprint,omitzero"`
	PolicyStatus       ProtectionPolicyStatus          `json:"policy_status"`
	AutoTrade          AutoTradeStatus                 `json:"auto_trade"`
	Trading            TradingStatus                   `json:"trading"`
	SourceFingerprints TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
	MarketEvents       *MarketEventsResult             `json:"market_events,omitempty"`
	Proposals          []TradeProposal                 `json:"proposals"`
	Counts             TradeProposalCounts             `json:"counts"`
	Blockers           []TradingBlocker                `json:"blockers,omitempty"`
	LoadedFromState    bool                            `json:"loaded_from_state,omitempty"`
}

// TradeProposalCounts summarizes proposals and their currency-qualified money
type TradeProposalCounts struct {
	Total                       int     `json:"total"`
	Actionable                  int     `json:"actionable"`
	ThetaHygiene                int     `json:"theta_hygiene"`
	RiskReduction               int     `json:"risk_reduction"`
	TrailingStop                int     `json:"trailing_stop"`
	OptionLossExit              int     `json:"option_loss_exit"`
	OptionExitReview            int     `json:"option_exit_review"`
	MarketFlags                 int     `json:"market_flags,omitempty"`
	ThetaPerDay                 float64 `json:"theta_per_day"`
	RiskReductionExcessNotional float64 `json:"risk_reduction_excess_notional,omitempty"`
	RiskReductionExcessCurrency string  `json:"risk_reduction_excess_currency,omitempty"`
	// ThetaPerDayCurrency labels the ThetaPerDay sum. Omitted when the
	// sum is kept for legacy renderers, but a currency label would lie.
	ThetaPerDayCurrency string `json:"theta_per_day_currency,omitempty"`
	// Base-currency twins of the money aggregates, converted per proposal
	// rate), never zero. BaseCurrency labels both.
	ThetaPerDayBase                 *float64 `json:"theta_per_day_base,omitempty"`
	RiskReductionExcessNotionalBase *float64 `json:"risk_reduction_excess_notional_base,omitempty"`
	BaseCurrency                    string   `json:"base_currency,omitempty"`
}

// TradeProposal is an advisory action bound to a key and revision. It is not a
// preview token or submit authorization.
type TradeProposal struct {
	Key                string                           `json:"key"`
	Revision           string                           `json:"revision"`
	State              string                           `json:"state"`
	Bucket             string                           `json:"bucket"`
	Rank               int                              `json:"rank"`
	Symbol             string                           `json:"symbol"`
	SecType            string                           `json:"sec_type"`
	Action             string                           `json:"action"`
	Quantity           int                              `json:"quantity"`
	MaxQuantity        int                              `json:"max_quantity"`
	PositionQuantity   float64                          `json:"position_quantity"`
	PositionEffect     string                           `json:"position_effect"`
	OrderType          string                           `json:"order_type"`
	Trail              *OrderTrailSpec                  `json:"trail,omitempty"`
	TrailSizing        *TradeProposalTrailSizing        `json:"trail_sizing,omitempty"`
	OptionExit         *TradeProposalOptionExit         `json:"option_exit,omitempty"`
	ExecutionSemantics *TradeProposalExecutionSemantics `json:"execution_semantics,omitempty"`
	StopRisk           *TradeProposalStopRisk           `json:"stop_risk,omitempty"`
	StopLadder         []TradeProposalStopLadderStep    `json:"stop_ladder,omitempty"`
	TriggerMethod      int                              `json:"trigger_method,omitempty"`
	TIF                string                           `json:"tif"`
	OutsideRTH         bool                             `json:"outside_rth"`
	Contract           ContractParams                   `json:"contract"`
	Reason             string                           `json:"reason"`
	Details            []string                         `json:"details,omitempty"`
	Score              float64                          `json:"score,omitempty"`
	ThetaPerDay        float64                          `json:"theta_per_day,omitempty"`
	Notional           float64                          `json:"notional,omitempty"`
	RiskExcessNotional float64                          `json:"risk_excess_notional,omitempty"`
	RiskExcessCurrency string                           `json:"risk_excess_currency,omitempty"`
	// Base-currency twins of ThetaPerDay / RiskExcessNotional, converted
	// was unavailable, never zero.
	ThetaPerDayBase        *float64 `json:"theta_per_day_base,omitempty"`
	RiskExcessNotionalBase *float64 `json:"risk_excess_notional_base,omitempty"`
	MarketValuePctNLV      *float64 `json:"market_value_pct_nlv,omitempty"`
	// Holding-level decision context: the full exposure being acted on, not
	PositionMarketValue       float64                         `json:"position_market_value,omitempty"`
	PositionDayChangeMoney    *float64                        `json:"position_day_change_money,omitempty"`
	PositionDayChangeCurrency string                          `json:"position_day_change_currency,omitempty"`
	PositionDayChangePct      *float64                        `json:"position_day_change_pct,omitempty"`
	MarketFlags               []MarketEventFlag               `json:"market_flags,omitempty"`
	LimitPrice                *float64                        `json:"limit_price,omitempty"`
	PolicyID                  string                          `json:"policy_id,omitempty"`
	PolicyVersion             int                             `json:"policy_version,omitempty"`
	PolicyFingerprint         Fingerprint                     `json:"policy_fingerprint,omitzero"`
	SourceFingerprints        TradeProposalSourceFingerprints `json:"source_fingerprints,omitzero"`
	Blockers                  []TradingBlocker                `json:"blockers,omitempty"`
	CreatedAt                 time.Time                       `json:"created_at,omitzero"`
}

// TradeProposalOptionExit explains an approved exact-contract directional
// option exit. Values are daemon-authored from cost basis and a fresh
// executable bid; adapters render them without re-evaluating thresholds.
type TradeProposalOptionExit struct {
	Kind                 string   `json:"kind"`
	Intent               string   `json:"intent"`
	EconomicRole         string   `json:"economic_role,omitempty"`
	DTE                  int      `json:"dte"`
	MinDTE               int      `json:"min_dte,omitempty"`
	CostBasisPremium     *float64 `json:"cost_basis_premium,omitempty"`
	ReferencePrice       *float64 `json:"reference_price,omitempty"`
	ReturnPct            *float64 `json:"return_pct,omitempty"`
	LossExitPct          float64  `json:"loss_exit_pct,omitempty"`
	ProfitArmGainPct     float64  `json:"profit_arm_gain_pct,omitempty"`
	LockedGainPct        float64  `json:"locked_gain_pct,omitempty"`
	InitialLockedGainPct *float64 `json:"initial_locked_gain_pct,omitempty"`
	ProfitTrailPct       float64  `json:"profit_trail_pct,omitempty"`
	MinTrailPct          float64  `json:"min_trail_pct,omitempty"`
	MaxTrailPct          float64  `json:"max_trail_pct,omitempty"`
	MaxSpreadPctOfMid    float64  `json:"max_spread_pct_of_mid,omitempty"`
	MinTrailAbs          float64  `json:"min_trail_abs,omitempty"`
	SpreadMultiple       float64  `json:"spread_multiple,omitempty"`
	Method               string   `json:"method,omitempty"`
}

// TradeProposalTrailSizing is the daemon-owned explanation for a protective
// protection policy TOML and OrderTrailSpec's broker percent convention.
type TradeProposalTrailSizing struct {
	Method            string    `json:"method,omitempty"`
	Version           string    `json:"version,omitempty"`
	DataQuality       string    `json:"data_quality,omitempty"`
	SelectedBy        string    `json:"selected_by,omitempty"`
	Fallback          bool      `json:"fallback,omitempty"`
	Capped            bool      `json:"capped,omitempty"`
	ReferencePrice    *float64  `json:"reference_price,omitempty"`
	ReferenceSource   string    `json:"reference_source,omitempty"`
	ReferenceAsOf     time.Time `json:"reference_as_of,omitzero"`
	PolicyMinPct      float64   `json:"policy_min_pct,omitempty"`
	PolicyDefaultPct  float64   `json:"policy_default_pct,omitempty"`
	PolicyFallbackPct float64   `json:"policy_fallback_pct,omitempty"`
	PolicyMaxPct      float64   `json:"policy_max_pct,omitempty"`
	ChosenPct         float64   `json:"chosen_pct,omitempty"`
	ChosenAmount      *float64  `json:"chosen_amount,omitempty"`
	InitialStopPrice  *float64  `json:"initial_stop_price,omitempty"`
	ATR14             *float64  `json:"atr_14,omitempty"`
	ATRPct            *float64  `json:"atr_pct,omitempty"`
	ATRMultiplier     *float64  `json:"atr_multiplier,omitempty"`
	ATRCandidatePct   *float64  `json:"atr_candidate_pct,omitempty"`
	SpreadPct         *float64  `json:"spread_pct,omitempty"`
	SpreadMultiplier  *float64  `json:"spread_multiplier,omitempty"`
	SpreadFloorPct    *float64  `json:"spread_floor_pct,omitempty"`
	MissingReasons    []string  `json:"missing_reasons,omitempty"`
	AsOf              time.Time `json:"as_of,omitzero"`
}

// TradeProposalExecutionSemantics explains how a protective stop is expected
// to behave at the broker. It is disclosure only; broker WhatIf/order status
type TradeProposalExecutionSemantics struct {
	ReferenceSide      string    `json:"reference_side,omitempty"`
	ReferencePrice     *float64  `json:"reference_price,omitempty"`
	ReferenceAsOf      time.Time `json:"reference_as_of,omitzero"`
	TriggerMethod      int       `json:"trigger_method,omitempty"`
	TriggerMethodLabel string    `json:"trigger_method_label,omitempty"`
	TriggerSource      string    `json:"trigger_source,omitempty"`
	TriggerEffect      string    `json:"trigger_effect,omitempty"`
	PriceGuarantee     string    `json:"price_guarantee,omitempty"`
}

// TradeProposalStopRisk estimates the near-stop account impact from the
// proposal's current reference price. It is not a fill guarantee and must not
// be treated as a broker promise: stop orders can gap or slip.
type TradeProposalStopRisk struct {
	ReferencePrice      *float64                  `json:"reference_price,omitempty"`
	StopPrice           *float64                  `json:"stop_price,omitempty"`
	Distance            *float64                  `json:"distance,omitempty"`
	DistancePct         *float64                  `json:"distance_pct,omitempty"`
	Quantity            int                       `json:"quantity,omitempty"`
	Multiplier          int                       `json:"multiplier,omitempty"`
	EstimatedLoss       *float64                  `json:"estimated_loss_ccy,omitempty"`
	Currency            string                    `json:"currency,omitempty"`
	EstimatedLossBase   *float64                  `json:"estimated_loss_base,omitempty"`
	BaseCurrency        string                    `json:"base_currency,omitempty"`
	EstimatedLossPctNLV *float64                  `json:"estimated_loss_pct_nlv,omitempty"`
	GapScenario         *TradeProposalStopRiskGap `json:"gap_scenario,omitempty"`
	WarningCodes        []string                  `json:"warning_codes,omitempty"`
}

// TradeProposalStopRiskGap describes one modeled gap scenario. Pointer values
type TradeProposalStopRiskGap struct {
	Label                 string   `json:"label,omitempty"`
	GapPct                float64  `json:"gap_pct,omitempty"`
	AssumedExecutionPrice *float64 `json:"assumed_execution_price,omitempty"`
	EstimatedLoss         *float64 `json:"estimated_loss_ccy,omitempty"`
	EstimatedLossBase     *float64 `json:"estimated_loss_base,omitempty"`
	EstimatedLossPctNLV   *float64 `json:"estimated_loss_pct_nlv,omitempty"`
}

// TradeProposalStopLadderStep is one modeled stop-distance scenario and is not
// a broker fill guarantee.
type TradeProposalStopLadderStep struct {
	Label               string   `json:"label"`
	Kind                string   `json:"kind,omitempty"`
	Percent             *float64 `json:"percent,omitempty"`
	StopPrice           *float64 `json:"stop_price,omitempty"`
	EstimatedLoss       *float64 `json:"estimated_loss_ccy,omitempty"`
	EstimatedLossBase   *float64 `json:"estimated_loss_base,omitempty"`
	EstimatedLossPctNLV *float64 `json:"estimated_loss_pct_nlv,omitempty"`
	ReferencePrice      *float64 `json:"reference_price,omitempty"`
}

// TradeProposalSnapshotParams controls rendering of the current revision.
type TradeProposalSnapshotParams struct {
	Show bool `json:"show,omitempty"`
}

// TradeProposalRefreshParams requests daemon recomputation; Show affects only
// the returned presentation.
type TradeProposalRefreshParams struct {
	Show bool `json:"show,omitempty"`
}

// TradeProposalPreviewParams identifies an exact candidate revision for gated
// broker preview.
type TradeProposalPreviewParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	FastPath  bool   `json:"fast_path,omitempty"`
}

// TradeProposalPreviewResult reports broker and daemon eligibility. A token ID
// is an audit identifier; the raw authorizing token remains private.
type TradeProposalPreviewResult struct {
	Accepted              bool                       `json:"accepted"`
	Proposal              TradeProposal              `json:"proposal"`
	PreviewTokenID        string                     `json:"preview_token_id,omitempty"`
	PreviewTokenExpiresAt time.Time                  `json:"preview_token_expires_at,omitzero"`
	SubmitEligible        bool                       `json:"submit_eligible"`
	Preview               *TradeProposalOrderPreview `json:"preview,omitempty"`
	Blockers              []TradingBlocker           `json:"blockers,omitempty"`
	AsOf                  time.Time                  `json:"as_of"`
}

// TradeProposalOrderPreview is the sanitized broker WhatIf and order preview.
type TradeProposalOrderPreview struct {
	PreviewTokenID        string                           `json:"preview_token_id,omitempty"`
	PreviewTokenScope     string                           `json:"preview_token_scope,omitempty"`
	PreviewTokenExpiresAt time.Time                        `json:"preview_token_expires_at,omitzero"`
	TokenMinted           bool                             `json:"token_minted"`
	SubmitEligible        bool                             `json:"submit_eligible"`
	Mode                  string                           `json:"mode"`
	Account               string                           `json:"account"`
	Endpoint              string                           `json:"endpoint"`
	ClientID              int                              `json:"client_id"`
	Draft                 OrderDraft                       `json:"draft"`
	Quote                 OrderQuoteSnapshot               `json:"quote"`
	Position              OrderPositionImpact              `json:"position"`
	ExecutionSemantics    *TradeProposalExecutionSemantics `json:"execution_semantics,omitempty"`
	StopRisk              *TradeProposalStopRisk           `json:"stop_risk,omitempty"`
	Notional              float64                          `json:"notional"`
	MaxNotional           float64                          `json:"max_notional,omitempty"`
	WhatIf                OrderWhatIfResult                `json:"what_if"`
	Warnings              []DataWarning                    `json:"warnings,omitempty"`
	AsOf                  time.Time                        `json:"as_of"`
}

// TradeProposalSubmitParams requests gated submission of an exact revision.
// Origin and any earlier preview are evidence, not submit authority by themselves.
type TradeProposalSubmitParams struct {
	Key       string `json:"key"`
	Revision  string `json:"revision"`
	Quantity  int    `json:"quantity,omitempty"`
	FastPath  bool   `json:"fast_path,omitempty"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

// TradeProposalSubmitResult reports the outcome of a gated submission request.
type TradeProposalSubmitResult struct {
	Accepted       bool                       `json:"accepted"`
	Proposal       TradeProposal              `json:"proposal"`
	Preview        *TradeProposalOrderPreview `json:"preview,omitempty"`
	Place          *OrderPlaceResult          `json:"place,omitempty"`
	PreviewTokenID string                     `json:"preview_token_id,omitempty"`
	OrderRef       string                     `json:"order_ref,omitempty"`
	Blockers       []TradingBlocker           `json:"blockers,omitempty"`
	Message        string                     `json:"message,omitempty"`
	AsOf           time.Time                  `json:"as_of"`
}

// TradeProposalIgnoreParams dismisses an advisory revision without touching
// broker orders or positions.
type TradeProposalIgnoreParams struct {
	Key      string `json:"key"`
	Revision string `json:"revision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// TradeProposalIgnoreResult reports whether the advisory dismissal was accepted.
type TradeProposalIgnoreResult struct {
	Accepted bool      `json:"accepted"`
	Key      string    `json:"key"`
	Revision string    `json:"revision,omitempty"`
	Message  string    `json:"message,omitempty"`
	AsOf     time.Time `json:"as_of"`
}

// TradeProposalRequestStopParams names a held stock/ETF position for an
// on-demand protective trailing-stop proposal. ConID is the unambiguous key;
// a bare Symbol is accepted only when it matches exactly one stock position.
// The verb regenerates the advisory snapshot and clears a prior advisory
// dismissal for that position's stop; it never places, previews, or
// authorizes a broker order.
type TradeProposalRequestStopParams struct {
	ConID  int    `json:"con_id,omitempty"`
	Symbol string `json:"symbol,omitempty"`
	Show   bool   `json:"show,omitempty"`
}

// TradeProposalRequestStopResult reports the outcome of an on-demand
// protective-stop generation. Accepted means an unblocked trailing-stop
// proposal for the named position exists in the returned snapshot revision;
// it is not a preview token or submit eligibility. The proposal flows into
// the ordinary trade_proposals.preview / trade_proposals.submit gates.
type TradeProposalRequestStopResult struct {
	Accepted bool   `json:"accepted"`
	ConID    int    `json:"con_id,omitempty"`
	Symbol   string `json:"symbol,omitempty"`
	SecType  string `json:"sec_type,omitempty"`
	// ProposalKey/Revision address the generated proposal for preview/submit.
	ProposalKey string         `json:"proposal_key,omitempty"`
	Revision    string         `json:"revision,omitempty"`
	Proposal    *TradeProposal `json:"proposal,omitempty"`
	// Snapshot is the full refreshed proposal snapshot the key/revision are
	// bound to, so callers can render the current list without a racing
	// second refresh.
	Snapshot *TradeProposalSnapshot `json:"snapshot,omitempty"`
	// IgnoreCleared discloses that a prior advisory ignore for this stop was
	// cleared by this explicit request.
	IgnoreCleared bool             `json:"ignore_cleared,omitempty"`
	Blockers      []TradingBlocker `json:"blockers,omitempty"`
	Message       string           `json:"message,omitempty"`
	AsOf          time.Time        `json:"as_of"`
}

// TradeProposalReduceParams is a discretionary partial reduce of an existing
// size, so the order is close/reduce-only and can never flip or open exposure.
type TradeProposalReduceParams struct {
	ConID         int    `json:"con_id,omitempty"`
	Symbol        string `json:"symbol,omitempty"`
	Percent       int    `json:"percent"`
	IncludeHedges bool   `json:"include_hedges,omitempty"`
	TimeoutMs     int    `json:"timeout_ms,omitempty"`
	// Origin identifies who is asking (OrderOrigin*) for audit and the
	// live-origin write gate; submit only.
	Origin string `json:"origin,omitempty"`
}

// TradeProposalReduceResult is returned by both reduce_preview and
// reduce_submit. Preview carries the sanitized order preview (the raw token
// never leaves the daemon; PreviewTokenID is for audit only) and SubmitEligible.
// opted into hedges, Blockers carries hedge_excluded and no token is minted.
type TradeProposalReduceResult struct {
	Accepted              bool                       `json:"accepted"`
	ConID                 int                        `json:"con_id,omitempty"`
	Symbol                string                     `json:"symbol,omitempty"`
	SecType               string                     `json:"sec_type,omitempty"`
	Action                string                     `json:"action,omitempty"`
	Percent               int                        `json:"percent"`
	PositionQuantity      float64                    `json:"position_quantity"`
	ReduceQuantity        int                        `json:"reduce_quantity"`
	HedgeLike             bool                       `json:"hedge_like,omitempty"`
	PreviewTokenID        string                     `json:"preview_token_id,omitempty"`
	PreviewTokenExpiresAt time.Time                  `json:"preview_token_expires_at,omitzero"`
	SubmitEligible        bool                       `json:"submit_eligible"`
	Preview               *TradeProposalOrderPreview `json:"preview,omitempty"`
	Place                 *OrderPlaceResult          `json:"place,omitempty"`
	OrderRef              string                     `json:"order_ref,omitempty"`
	Blockers              []TradingBlocker           `json:"blockers,omitempty"`
	Message               string                     `json:"message,omitempty"`
	AsOf                  time.Time                  `json:"as_of"`
}

// TradeProposalReducePortfolioParams is the one-tap portfolio risk-off sweep:
// never selected: trimming them would increase net risk, not reduce it, so
type TradeProposalReducePortfolioParams struct {
	Percent    int    `json:"percent"`
	TimeoutMs  int    `json:"timeout_ms,omitempty"`
	Origin     string `json:"origin,omitempty"`
	RequestRef string `json:"request_ref,omitempty"`
}

// TradeProposalReduceLeg is one position's slice of a portfolio sweep. On
// is basis context only — annotation, never an input to sizing or selection.
type TradeProposalReduceLeg struct {
	ConID                     int                        `json:"con_id,omitempty"`
	Symbol                    string                     `json:"symbol,omitempty"`
	SecType                   string                     `json:"sec_type,omitempty"`
	Action                    string                     `json:"action,omitempty"`
	PositionQuantity          float64                    `json:"position_quantity"`
	ReduceQuantity            int                        `json:"reduce_quantity"`
	DollarDelta               float64                    `json:"dollar_delta,omitempty"`
	RiskContributionCut       float64                    `json:"risk_contribution_cut,omitempty"`
	Notional                  float64                    `json:"notional,omitempty"`
	NotionalCurrency          string                     `json:"notional_currency,omitempty"`
	NotionalBase              *float64                   `json:"notional_base,omitempty"`
	PositionUnrealizedPnL     float64                    `json:"position_unrealized_pnl_ccy,omitempty"`
	PositionUnrealizedPnLBase *float64                   `json:"position_unrealized_pnl_base,omitempty"`
	PreviewTokenID            string                     `json:"preview_token_id,omitempty"`
	SubmitEligible            bool                       `json:"submit_eligible"`
	Preview                   *TradeProposalOrderPreview `json:"preview,omitempty"`
	Place                     *OrderPlaceResult          `json:"place,omitempty"`
	Placed                    bool                       `json:"placed,omitempty"`
	OrderRef                  string                     `json:"order_ref,omitempty"`
	Blockers                  []TradingBlocker           `json:"blockers,omitempty"`
	Message                   string                     `json:"message,omitempty"`
}

// TradeProposalReducePortfolioResult is the basket preview/submit envelope.
type TradeProposalReducePortfolioResult struct {
	Accepted             bool                     `json:"accepted"`
	Percent              int                      `json:"percent"`
	NetDollarDeltaBefore float64                  `json:"net_dollar_delta_before,omitempty"`
	NetDeltaIncomplete   bool                     `json:"net_delta_incomplete,omitempty"`
	TargetDollarDelta    float64                  `json:"target_dollar_delta,omitempty"`
	AchievedDollarDelta  float64                  `json:"achieved_dollar_delta,omitempty"`
	AchievedPctOfTarget  *float64                 `json:"achieved_pct_of_target,omitempty"`
	Legs                 []TradeProposalReduceLeg `json:"legs"`
	LegCount             int                      `json:"leg_count"`
	EligibleCount        int                      `json:"eligible_count"`
	BlockedCount         int                      `json:"blocked_count"`
	TotalNotional        float64                  `json:"total_notional,omitempty"`
	BaseCurrency         string                   `json:"base_currency,omitempty"`
	FXIncomplete         bool                     `json:"fx_incomplete,omitempty"`
	Replayed             bool                     `json:"replayed,omitempty"`
	Blockers             []TradingBlocker         `json:"blockers,omitempty"`
	Message              string                   `json:"message,omitempty"`
	AsOf                 time.Time                `json:"as_of"`
}
