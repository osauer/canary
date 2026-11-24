package rpc

// OrderCombination is what a combination (BAG) order trades, so a working
// spread never reads as a single-instrument order. Source is "canary" for a
// strategy order Canary placed (open, add, reduce or close of a strategy),
// with Strategy the held shape's name (bull_call_spread, bear_put_spread,
// iron_condor ...), Operation, StrategyID and Units; "broker" for one placed
// elsewhere (TWS), with only the legs the broker's open order reports.
// Each leg's Action is the leg's own side in this order: a parent SELL that
// closes a bull call spread sells its long call and buys back its short
// call.
type OrderCombination struct {
	Source     string                `json:"source"` // canary | broker
	Strategy   string                `json:"strategy,omitempty"`
	Kind       string                `json:"kind,omitempty"`
	Operation  string                `json:"operation,omitempty"`
	StrategyID string                `json:"strategy_id,omitempty"`
	Units      int                   `json:"units,omitempty"`
	Legs       []OrderCombinationLeg `json:"legs"`
}

// Order combination sources.
const (
	OrderCombinationCanary = "canary"
	OrderCombinationBroker = "broker"
)

// OrderCombinationLeg is one leg: its side in this order, its ratio per
// unit and its exact contract. A broker-reported leg carries the contract
// ID, ratio, side and venue; the rest is known only for legs Canary placed.
type OrderCombinationLeg struct {
	Action       string  `json:"action"`
	Ratio        int     `json:"ratio"`
	Quantity     int     `json:"quantity,omitempty"`
	ConID        int     `json:"con_id"`
	Symbol       string  `json:"symbol,omitempty"`
	SecType      string  `json:"sec_type,omitempty"`
	Expiry       string  `json:"expiry,omitempty"`
	Strike       float64 `json:"strike,omitempty"`
	Right        string  `json:"right,omitempty"`
	Multiplier   int     `json:"multiplier,omitempty"`
	TradingClass string  `json:"trading_class,omitempty"`
	Exchange     string  `json:"exchange,omitempty"`
}
