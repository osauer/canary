package rpc

import "time"

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
