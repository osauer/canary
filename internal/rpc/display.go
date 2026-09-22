package rpc

import "time"

// DisplaySnapshot is complete volatile display state, never trading authority.
// A new generation replaces the prior scope; absent rows/fields are removals.
// EmittedAt is a transport clock. Every value retains its own source/receive clock.
type DisplaySnapshot struct {
	Version     int               `json:"version"`
	Generation  string            `json:"generation"`
	Sequence    uint64            `json:"sequence"`
	EmittedAt   time.Time         `json:"emitted_at"`
	Available   bool              `json:"available"`
	Scope       AccountDataScope  `json:"scope"`
	Account     DisplayAccount    `json:"account"`
	PositionsAt time.Time         `json:"positions_at,omitzero"`
	Positions   []DisplayPosition `json:"positions"`
	Quotes      []DisplayQuote    `json:"quotes"`
	Truncated   bool              `json:"truncated"`
}

// DisplayAccount contains broker-base monetary values with separate receive clocks.
type DisplayAccount struct {
	Currency         string    `json:"currency"`
	NetLiquidation   *float64  `json:"net_liquidation"`
	NetLiquidationAt time.Time `json:"net_liquidation_at,omitzero"`
	DailyPnL         *float64  `json:"daily_pnl"`
	UnrealizedPnL    *float64  `json:"unrealized_pnl"`
	RealizedPnL      *float64  `json:"realized_pnl"`
	PnLAt            time.Time `json:"pnl_at,omitzero"`
}

// DisplayPosition holds one exact portfolio contract. DailyPnL is in the
// contract currency: account-base reqPnLSingle money divided by a recent cached
// BASE-per-CCY rate. Missing currency, rate or P&L leaves it unavailable.
// PnLAt remains the broker P&L receipt time, never an FX refresh or read time.
type DisplayPosition struct {
	UnrealizedAt  time.Time      `json:"unrealized_at,omitzero"`
	ValuationAt   time.Time      `json:"valuation_at,omitzero"`
	Contract      ContractParams `json:"contract"`
	Quantity      float64        `json:"quantity"`
	AverageCost   *float64       `json:"average_cost"`
	Mark          *float64       `json:"mark"`
	MarketValue   *float64       `json:"market_value"`
	UnrealizedPnL *float64       `json:"unrealized_pnl"`
	DailyPnL      *float64       `json:"daily_pnl"`
	PnLAt         time.Time      `json:"pnl_at,omitzero"`
}

// DisplayQuote separates exchange trade time from local price/volume receipts.
// DataType and source clocks must be checked before describing a value as live.
// Volume is cumulative session volume, never a historical bar's volume.
type DisplayQuote struct {
	Price         *float64       `json:"price"`
	PriceSource   string         `json:"price_source"`
	Contract      ContractParams `json:"contract"`
	Kind          string         `json:"kind"`
	Key           string         `json:"key"`
	DataType      string         `json:"data_type"`
	Last          *float64       `json:"last"`
	Bid           *float64       `json:"bid"`
	Ask           *float64       `json:"ask"`
	PreviousClose *float64       `json:"previous_close"`
	TradeAt       time.Time      `json:"trade_at,omitzero"`
	// TradePhase describes the original trade session, never receipt time.
	// Values match Quote.TradePhase; empty means the session is unknown.
	TradePhase      string    `json:"trade_phase,omitempty"`
	PriceReceivedAt time.Time `json:"price_received_at,omitzero"`
	Volume          *int64    `json:"volume"`
	VolumeAt        time.Time `json:"volume_at,omitzero"`
}
