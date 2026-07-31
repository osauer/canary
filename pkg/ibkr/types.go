package ibkr

import "time"

// Greeks contains option sensitivities reported by the broker.
type Greeks struct {
	Delta float64 `json:"delta"`
	Gamma float64 `json:"gamma"`
	Theta float64 `json:"theta"`
	Vega  float64 `json:"vega"`
	Rho   float64 `json:"rho"`
}

// MarketData is the latest set of market-data observations for a symbol.
// Callers must use the accompanying observed, timestamp, and data-type fields
// where provided; a numeric zero alone does not always prove the broker
// reported a zero value.
type MarketData struct {
	Symbol    string    `json:"symbol"`
	Timestamp time.Time `json:"timestamp"`

	Last float64 `json:"last"`
	Bid  float64 `json:"bid"`
	Ask  float64 `json:"ask"`
	Mid  float64 `json:"mid"`
	// MarkPrice is tick 37 from IBKR — the gateway's calculated fair
	// price. Populated for every symbol, but only load-bearing for
	// indices (VIX, VIX3M, SPX), which don't emit bid/ask/last.
	MarkPrice float64 `json:"mark_price,omitempty"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	VWAP      float64 `json:"vwap"`

	// Week-range highs/lows from generic tick 165 (Misc Stats). Zero when
	// the gateway hasn't delivered the tick yet — caller must distinguish
	// "not arrived" from "exactly zero" via the timestamp / Observed state.
	Week13Low  float64 `json:"week_13_low,omitempty"`
	Week13High float64 `json:"week_13_high,omitempty"`
	Week26Low  float64 `json:"week_26_low,omitempty"`
	Week26High float64 `json:"week_26_high,omitempty"`
	Week52Low  float64 `json:"week_52_low,omitempty"`
	Week52High float64 `json:"week_52_high,omitempty"`

	Volume    int64 `json:"volume"`
	AvgVolume int64 `json:"avg_volume,omitempty"`
	// LastTickAt is when this process last received a tick on the
	// subscription, or zero when none has ever arrived. See
	// [Subscription.LastTickAt] for the two limits that bind every reader:
	// it is an arrival instant rather than the instant the value was struck,
	// and it advances on any tick, size and volume included.
	LastTickAt        time.Time `json:"last_tick_at,omitzero"`
	LastTradeTime     time.Time `json:"last_trade_time,omitzero"`
	BidSize           int       `json:"bid_size"`
	AskSize           int       `json:"ask_size"`
	OpenInt           int64     `json:"open_int"`
	OpenIntObserved   bool      `json:"open_int_observed,omitempty"`
	ShortableShares   int64     `json:"shortable_shares,omitempty"`
	ShortableObserved bool      `json:"shortable_observed,omitempty"`

	IV     float64 `json:"iv"`
	HV     float64 `json:"hv"`
	IVRank float64 `json:"iv_rank"`
	IVPerc float64 `json:"iv_perc"`

	Greeks *Greeks `json:"greeks,omitempty"`

	PutCallRatio  float64 `json:"put_call_ratio"`
	TickDirection string  `json:"tick_direction"`

	Session   string `json:"session,omitempty"`
	DataType  string `json:"data_type,omitempty"`
	IsDelayed bool   `json:"is_delayed,omitempty"`
}

// trackedOrder is the connector's in-memory record of an order it placed,
// keyed by local ID in Connector.openOrders. The broker-facing wire type is
// IBKROrder.
type trackedOrder struct {
	ID          string      `json:"id"`
	BrokerID    string      `json:"broker_id"`
	Symbol      string      `json:"symbol"`
	Side        OrderSide   `json:"side"`
	Quantity    float64     `json:"quantity"`
	OrderType   OrderType   `json:"order_type"`
	LimitPrice  float64     `json:"limit_price"`
	StopPrice   float64     `json:"stop_price"`
	TimeInForce TimeInForce `json:"time_in_force"`
	Status      OrderStatus `json:"status"`
	FilledQty   float64     `json:"filled_qty"`
	FilledPrice float64     `json:"filled_price"`
	Commission  float64     `json:"commission"`
	Reason      string      `json:"reason"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	FilledAt    *time.Time  `json:"filled_at,omitempty"`
	CancelledAt *time.Time  `json:"cancelled_at,omitempty"`

	AllowOutsideRth bool `json:"outside_rth,omitempty"`
}

// OrderSide identifies the buy or sell direction of an order.
type OrderSide string

const (
	// OrderSideBuy identifies a buy order.
	OrderSideBuy OrderSide = "BUY"
	// OrderSideSell identifies a sell order.
	OrderSideSell OrderSide = "SELL"
)

// OrderType identifies the requested broker execution instruction.
type OrderType string

const (
	// OrderTypeMarket identifies a market order.
	OrderTypeMarket OrderType = "MARKET"
	// OrderTypeLimit identifies a limit order.
	OrderTypeLimit OrderType = "LIMIT"
	// OrderTypeStop identifies a stop order.
	OrderTypeStop OrderType = "STOP"
	// OrderTypeStopLimit identifies a stop-limit order.
	OrderTypeStopLimit OrderType = "STOP_LIMIT"
	// OrderTypeMOC identifies a market-on-close order.
	OrderTypeMOC OrderType = "MOC"
	// OrderTypeLOC identifies a limit-on-close order.
	OrderTypeLOC OrderType = "LOC"
	// OrderTypePegMid identifies an order pegged to the midpoint.
	OrderTypePegMid OrderType = "PEG_MID"
)

// TimeInForce identifies how long an order remains eligible for execution.
type TimeInForce string

const (
	// TimeInForceDay keeps an order active for the current trading day.
	TimeInForceDay TimeInForce = "DAY"
	// TimeInForceGTC keeps an order active until it fills or is cancelled.
	TimeInForceGTC TimeInForce = "GTC"
	// TimeInForceIOC requests immediate execution and cancels any remainder.
	TimeInForceIOC TimeInForce = "IOC"
	// TimeInForceFOK requires immediate execution of the full quantity.
	TimeInForceFOK TimeInForce = "FOK"
	// TimeInForceGTD keeps an order active through its specified date.
	TimeInForceGTD TimeInForce = "GTD"
	// TimeInForceOPG requests execution at the market open.
	TimeInForceOPG TimeInForce = "OPG"
)

// OrderStatus identifies the package's normalized lifecycle state for an order.
type OrderStatus string

const (
	// OrderStatusNew means the order has been created locally.
	OrderStatusNew OrderStatus = "NEW"
	// OrderStatusPending means submission is in progress.
	OrderStatusPending OrderStatus = "PENDING"
	// OrderStatusSubmitted means the broker has received the order.
	OrderStatusSubmitted OrderStatus = "SUBMITTED"
	// OrderStatusAccepted means the broker has accepted the order.
	OrderStatusAccepted OrderStatus = "ACCEPTED"
	// OrderStatusPartial means part, but not all, of the quantity has filled.
	OrderStatusPartial OrderStatus = "PARTIAL"
	// OrderStatusFilled means the full quantity has filled.
	OrderStatusFilled OrderStatus = "FILLED"
	// OrderStatusCancelled means cancellation was requested locally or observed
	// from the broker; the value alone does not prove broker-final cancellation.
	OrderStatusCancelled OrderStatus = "CANCELLED"
	// OrderStatusRejected means the broker rejected the order.
	OrderStatusRejected OrderStatus = "REJECTED"
	// OrderStatusExpired means the order elapsed without filling.
	OrderStatusExpired OrderStatus = "EXPIRED"
)
