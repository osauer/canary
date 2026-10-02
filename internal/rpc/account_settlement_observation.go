package rpc

import "time"

// AccountSettlementObservation is sanitized completed-request diagnostics only.
// It conveys no balance or trading authority, and is absent for stale sources.
type AccountSettlementObservation struct {
	AsOf      time.Time              `json:"as_of"`
	Callbacks int                    `json:"callbacks"`
	Rows      []AccountSettlementRow `json:"rows"`
}

// AccountSettlementRow reports whether a SettledCash callback was finite and
// whether its wire label was account_total, aggregate_ambiguous, broker_ledger_label
// or invalid_currency. Unvalidated currency text is not echoed. Finite describes
// mathematical finiteness only; it does not imply available cash (IBKR unset
// sentinels can be finite).
type AccountSettlementRow struct {
	Currency string `json:"currency"`
	Source   string `json:"source"`
	Finite   bool   `json:"finite"`
}
