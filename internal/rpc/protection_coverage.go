package rpc

import "time"

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
