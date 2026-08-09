package rpc

import (
	"time"
)

// AccountDataAuthority is the shared truth envelope for account-scoped reads.
// Scope names the one account and paper/live mode the values belong to. Source
// describes the producer, Availability says whether the payload can stand as
// account truth, and Freshness says whether its receipt is current. A payload
// may retain stale or incomplete rows as context while Availability is
// unavailable; consumers must not turn those rows into a clean-book claim.
//
// Fields is present on AccountResult. Its booleans preserve the distinction
// between an observed zero and a legacy float64 zero produced because IBKR did
// not send the field. PositionsResult leaves Fields nil because the portfolio
// stream's completeness is described by Availability, Freshness, and Reason.
type AccountDataAuthority struct {
	Scope        AccountDataScope          `json:"scope"`
	Source       AccountDataSource         `json:"source"`
	Availability AccountDataAvailability   `json:"availability"`
	Freshness    AccountDataFreshness      `json:"freshness"`
	Reason       AccountDataReason         `json:"reason,omitempty"`
	AsOf         time.Time                 `json:"as_of,omitzero"`
	Fields       *AccountFieldAvailability `json:"fields,omitempty"`
}

// AccountDataScope names one concrete broker account and its paper/live mode.
// An aggregate managedAccounts inventory and the literal "All" are not valid
// account IDs and must never be published here as a selected account.
type AccountDataScope struct {
	AccountID   string `json:"account_id"`
	AccountMode string `json:"account_mode"`
}

// AccountDataSource is the closed producer vocabulary for account-scoped
// account and positions results.
type AccountDataSource string

// Account data sources identify the producer behind an account-scoped result.
const (
	AccountDataSourceAccountSummaryRequest AccountDataSource = "account_summary_request"
	AccountDataSourceAccountUpdatesCache   AccountDataSource = "account_updates_cache"
	AccountDataSourcePortfolioStream       AccountDataSource = "portfolio_stream"
)

// AccountDataAvailability says whether the payload may stand as account truth.
// Unavailable payloads may still carry context, but cannot prove a genuine
// zero, an empty book, or any other negative.
type AccountDataAvailability string

// Account data availability values say whether a result can stand as truth.
const (
	AccountDataAvailable   AccountDataAvailability = "available"
	AccountDataUnavailable AccountDataAvailability = "unavailable"
)

// AccountDataFreshness separates a current receipt from stale retained context
// and from data with no trustworthy observation time.
type AccountDataFreshness string

// Account data freshness values describe the age of the observed result.
const (
	AccountDataFreshnessCurrent AccountDataFreshness = "current"
	AccountDataFreshnessStale   AccountDataFreshness = "stale"
	AccountDataFreshnessUnknown AccountDataFreshness = "unknown"
)

// AccountDataReason is a stable, redacted explanation of why an account-scoped
// payload is stale or unavailable. Raw broker and transport errors stay out of
// this public contract.
type AccountDataReason string

// Account data reasons are the closed public explanations for unavailable data.
const (
	AccountDataReasonUnstampedCache  AccountDataReason = "unstamped_cache"
	AccountDataReasonScopeUnresolved AccountDataReason = "scope_unresolved"
	AccountDataReasonScopeConflict   AccountDataReason = "scope_conflict"
	AccountDataReasonAccountUnbound  AccountDataReason = "account_unbound"
	AccountDataReasonAccountMismatch AccountDataReason = "account_mismatch"
	AccountDataReasonUnprimed        AccountDataReason = "unprimed"
	AccountDataReasonInvalidPayload  AccountDataReason = "invalid_payload"
	AccountDataReasonClockInvalid    AccountDataReason = "clock_invalid"
	AccountDataReasonReceiptStale    AccountDataReason = "receipt_stale"
	AccountDataReasonSessionChanged  AccountDataReason = "session_changed"
)

// AccountFieldAvailability mirrors AccountResult's account-summary fields.
// Every key is emitted when Fields is present: false is evidence that the
// producer did not supply that field, not a default a consumer may ignore.
type AccountFieldAvailability struct {
	AccountType          bool `json:"account_type"`
	BaseCurrency         bool `json:"base_currency"`
	NetLiquidation       bool `json:"net_liquidation"`
	BuyingPower          bool `json:"buying_power"`
	AvailableFunds       bool `json:"available_funds"`
	ExcessLiquidity      bool `json:"excess_liquidity"`
	TotalCash            bool `json:"total_cash"`
	MaintenanceMargin    bool `json:"maintenance_margin"`
	InitialMargin        bool `json:"initial_margin"`
	GrossPositionValue   bool `json:"gross_position_value"`
	UnrealizedPnL        bool `json:"unrealized_pnl"`
	RealizedPnL          bool `json:"realized_pnl"`
	Cushion              bool `json:"cushion"`
	LookAheadInitMargin  bool `json:"look_ahead_init_margin"`
	LookAheadMaintMargin bool `json:"look_ahead_maint_margin"`
	LookAheadAvailable   bool `json:"look_ahead_available_funds"`
	LookAheadExcess      bool `json:"look_ahead_excess_liquidity"`
	DailyPnL             bool `json:"daily_pnl"`
	PnLUnrealizedTotal   bool `json:"pnl_unrealized_total"`
	PnLRealizedTotal     bool `json:"pnl_realized_total"`
	CurrencyExposure     bool `json:"currency_exposure"`
}

// A data-quality signal claims that reality is unobserved. Where reality is
// observed to be nothing — a cancelled equity, a defunct issuer — that is a
// fact, not a gap. QuoteExpectationNone carries that distinction across every
// surface so each consumer does not have to re-derive it from warning codes.
// Only the broker's terminal non-reporting verdict may mint it; numeric zeros
// in account rows are a data-quality warning, never expectation authority.
const (
	QuoteExpectationNone = "none"

	QuoteExpectationReasonTerminal = "terminal_non_reporting"
)

// ExpectsMarketData reports whether a quote, mark, or market-event flag should
// exist for this position. Absence is a defect only when this returns true.
func ExpectsMarketData(p PositionView) bool {
	return p.QuoteExpectation != QuoteExpectationNone
}

// ExpectsMarketDataGroup reports whether an underlying group should be
// subscribed for market data. Only a stock-only group whose stock expects no
// data is skipped: an option leg on a defunct underlying still needs its own
// quote, and the group's other rows are unaffected.
func ExpectsMarketDataGroup(g PositionGroup) bool {
	if len(g.Options) > 0 {
		return true
	}
	if g.Stock == nil {
		return true
	}
	return ExpectsMarketData(*g.Stock)
}

// Protection-coverage states distinguish reconciled coverage from partial,
// absent, orphaned, uncertain, reconciliation-required, and unprotectable
// observations.
const (
	ProtectionCoverageStateCovered           = "covered"
	ProtectionCoverageStatePartial           = "partial"
	ProtectionCoverageStateUnprotected       = "unprotected"
	ProtectionCoverageStateOrphanedOrder     = "orphaned_order"
	ProtectionCoverageStateReconcileRequired = "reconcile_required"
	ProtectionCoverageStateUnknown           = "unknown"
	// ProtectionCoverageStateNotProtectable marks a defunct or unquoted
	// holding: it has no mark to stop out against, so the proposal engine
	// already declines to propose for it. Counting such a row as unprotected
	// raises an alarm the protection panel cannot answer.
	ProtectionCoverageStateNotProtectable = "not_protectable"
)

// ProtectionCoverageSummary is the read-only coverage ledger for stock/ETF
// protection. Quantities count only open close-protective orders that still
// reconcile with the current position; stale/orphaned orders are surfaced but
// never counted as protection.
type ProtectionCoverageSummary struct {
	AsOf                            time.Time                 `json:"as_of,omitzero"`
	Status                          string                    `json:"status,omitempty"`
	ByUnderlying                    []ProtectionCoverageRow   `json:"by_underlying,omitempty"`
	Counts                          ProtectionCoverageCounts  `json:"counts,omitzero"`
	UnprotectedNotionalBase         *float64                  `json:"unprotected_notional_base,omitempty"`
	UnprotectedNotionalBaseCurrency string                    `json:"unprotected_notional_base_currency,omitempty"`
	LargestUnprotected              []ProtectionCoverageRow   `json:"largest_unprotected,omitempty"`
	OrphanedOrders                  []ProtectionCoverageOrder `json:"orphaned_orders,omitempty"`
	ReconcileRequiredOrders         []ProtectionCoverageOrder `json:"reconcile_required_orders,omitempty"`
	WarningCodes                    []string                  `json:"warning_codes,omitempty"`
	Message                         string                    `json:"message,omitempty"`
}

// ProtectionCoverageCounts summarizes the mutually exclusive coverage rows.
type ProtectionCoverageCounts struct {
	Covered           int `json:"covered,omitempty"`
	Partial           int `json:"partial,omitempty"`
	Unprotected       int `json:"unprotected,omitempty"`
	OrphanedOrder     int `json:"orphaned_order,omitempty"`
	ReconcileRequired int `json:"reconcile_required,omitempty"`
	Unknown           int `json:"unknown,omitempty"`
	NotProtectable    int `json:"not_protectable,omitempty"`
}

// ProtectionCoverageRow reports reconciled coverage for one held underlying.
// Pointer notionals are nil when base-currency conversion is unavailable.
type ProtectionCoverageRow struct {
	Underlying                      string                    `json:"underlying"`
	State                           string                    `json:"state"`
	PositionQuantity                float64                   `json:"position_quantity,omitempty"`
	ProtectedQuantity               float64                   `json:"protected_quantity,omitempty"`
	UnprotectedQuantity             float64                   `json:"unprotected_quantity,omitempty"`
	MarketValueBase                 *float64                  `json:"market_value_base,omitempty"`
	MarketValuePctNLV               *float64                  `json:"market_value_pct_nlv,omitempty"`
	UnprotectedNotionalBase         *float64                  `json:"unprotected_notional_base,omitempty"`
	UnprotectedNotionalBaseCurrency string                    `json:"unprotected_notional_base_currency,omitempty"`
	Orders                          []ProtectionCoverageOrder `json:"orders,omitempty"`
	WarningCodes                    []string                  `json:"warning_codes,omitempty"`
	Message                         string                    `json:"message,omitempty"`
}

// ProtectionCoverageOrder is a redacted protective-order observation. Its
// coverage and reconciliation flags are daemon-derived, not broker authority.
type ProtectionCoverageOrder struct {
	OrderRef            string    `json:"order_ref,omitempty"`
	Symbol              string    `json:"symbol,omitempty"`
	SecType             string    `json:"sec_type,omitempty"`
	Action              string    `json:"action,omitempty"`
	OrderType           string    `json:"order_type,omitempty"`
	TIF                 string    `json:"tif,omitempty"`
	Remaining           float64   `json:"remaining,omitempty"`
	Quantity            float64   `json:"quantity,omitempty"`
	StopPrice           *float64  `json:"stop_price,omitempty"`
	LimitPrice          *float64  `json:"limit_price,omitempty"`
	LifecycleStatus     string    `json:"lifecycle_status,omitempty"`
	ReconciliationState string    `json:"reconciliation_state,omitempty"`
	UpdatedAt           time.Time `json:"updated_at,omitzero"`
	LastMessage         string    `json:"last_message,omitempty"`
}
