package rpc

import "time"

// AccountStreamObservation is passive, value-free existing-subscription
// diagnostics. Its local clocks and initial completion grant no cash authority.
type AccountStreamObservation struct {
	Status              string             `json:"status"`
	ClockSource         string             `json:"clock_source"`
	SocketEpoch         uint64             `json:"socket_epoch"`
	ReadAt              time.Time          `json:"read_at"`
	RequestedAt         time.Time          `json:"requested_at,omitzero"`
	DownloadEndAt       time.Time          `json:"download_end_at,omitzero"`
	CompletedAt         time.Time          `json:"initial_completed_at,omitzero"`
	LastCallbackAt      time.Time          `json:"last_callback_at,omitzero"`
	AccountReady        string             `json:"account_ready"`
	AccountReadyAt      time.Time          `json:"account_ready_at,omitzero"`
	TradingType         string             `json:"trading_type"`
	TradingTypeObserved bool               `json:"trading_type_observed"`
	TradingTypeAt       time.Time          `json:"trading_type_at,omitzero"`
	Rows                []AccountStreamRow `json:"rows"`
	RowsTruncated       bool               `json:"rows_truncated"`
}

// AccountStreamRow preserves an allowlisted wire key and native currency.
type AccountStreamRow struct {
	Key             string    `json:"key"`
	Currency        string    `json:"currency"`
	Source          string    `json:"source"`
	ValueStatus     string    `json:"value_status"`
	Callbacks       int       `json:"callbacks"`
	FirstReceivedAt time.Time `json:"first_received_at"`
	LastReceivedAt  time.Time `json:"last_received_at"`
}
