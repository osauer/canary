package rpc

import "time"

// AccountCurrencySettlementProbe is a one-shot currency-only Multi experiment.
// Completed means initial callbacks ended; it grants no financial authority.
type AccountCurrencySettlementProbe struct {
	Status          string             `json:"status"`
	CancelStatus    string             `json:"cancel_status"`
	ClockSource     string             `json:"clock_source"`
	BrokerErrorCode int                `json:"broker_error_code,omitempty"`
	SocketEpoch     uint64             `json:"socket_epoch"`
	RequestedAt     time.Time          `json:"requested_at,omitzero"`
	CompletedAt     time.Time          `json:"completed_at,omitzero"`
	LastCallbackAt  time.Time          `json:"last_callback_at,omitzero"`
	ReadAt          time.Time          `json:"read_at,omitzero"`
	Callbacks       int                `json:"callbacks"`
	Rows            []AccountStreamRow `json:"rows,omitempty"`
	RowsTruncated   bool               `json:"rows_truncated"`
}
