package rpc

import "time"

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
