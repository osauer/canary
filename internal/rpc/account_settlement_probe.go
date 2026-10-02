package rpc

import "time"

// AccountSummaryParams opts into a receipt-only diagnostic. Empty/nil params
// preserve the ordinary account summary contract.
type AccountSummaryParams struct {
	SettlementProbe         bool `json:"settlement_probe,omitempty"`
	CurrencySettlementProbe bool `json:"currency_settlement_probe,omitempty"`
}

// AccountSettlementComparison compares the normal tag batch with an isolated
// SettledCash-only request. No amounts, accounts or authority are returned.
type AccountSettlementComparison struct {
	NormalStatus      string                        `json:"normal_status"`
	NormalObservation *AccountSettlementObservation `json:"normal_observation,omitempty"`
	Probe             AccountSettlementProbe        `json:"probe"`
	AsOf              time.Time                     `json:"as_of"`
}

// AccountSettlementProbe describes diagnostic transport, never cash authority.
type AccountSettlementProbe struct {
	Status          string                        `json:"status"`
	CancelStatus    string                        `json:"cancel_status"`
	BrokerErrorCode int                           `json:"broker_error_code,omitempty"`
	Observation     *AccountSettlementObservation `json:"observation,omitempty"`
}
