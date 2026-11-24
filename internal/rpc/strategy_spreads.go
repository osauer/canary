package rpc

import "time"

// MethodStrategySpreads is bounded, read-only pricing evidence for opening a
// debit vertical: for one exact bought option, the sold strikes of the same
// expiry beyond it, each leg's quote and the combination's net figures. It
// carries no order authority; `strategy.preview` with operation open judges
// the order.
const MethodStrategySpreads = "strategy.spreads"

// StrategySpreadsParams names the underlying stock, as `add.market` takes
// it, and the exact option to buy, as `add.market` returned it.
type StrategySpreadsParams struct {
	Underlying ContractParams `json:"underlying"`
	Long       ContractParams `json:"long"`
}

// StrategySpreadRule is the rule the default sold strike follows and where
// its numbers come from: the opportunity policy's [add].spread, or Canary's
// compiled default when the policy file does not write it (Source
// "canary_default"). Among the sold strikes that save at least MinSaving of
// the bought option's midpoint and sit at least CapExpectedMoves one-sigma
// moves to expiry beyond the underlying's price, the default is the one
// whose delta is nearest ShortDeltaRatio times the bought option's.
type StrategySpreadRule struct {
	ShortDeltaRatio  float64 `json:"short_delta_ratio"`
	MinSaving        float64 `json:"min_saving"`
	CapExpectedMoves float64 `json:"cap_expected_moves"`
	Source           string  `json:"source"` // policy | canary_default
}

// Strategy spread rule sources.
const (
	StrategySpreadRulePolicy  = "policy"
	StrategySpreadRuleDefault = "canary_default"
)

// StrategySpreadCandidate is one sold strike and the vertical it makes with
// the bought option. Prices are per share in the contract currency; the
// per-unit figures are for one spread (one bought and one sold contract,
// times the contract size) at the net midpoint. NetBid and NetAsk need a
// two-sided quote on both legs; NetMid falls back to planning prices while
// the options session is closed, and is then context, not an executable
// quote.
type StrategySpreadCandidate struct {
	Short          AddCallCandidate `json:"short"`
	Width          float64          `json:"width"`
	NetBid         *float64         `json:"net_bid,omitempty"`
	NetAsk         *float64         `json:"net_ask,omitempty"`
	NetMid         *float64         `json:"net_mid,omitempty"`
	MaxLossPerUnit *float64         `json:"max_loss_per_unit,omitempty"`
	MaxGainPerUnit *float64         `json:"max_gain_per_unit,omitempty"`
	Breakeven      *float64         `json:"breakeven,omitempty"`
	// Saving is the share of the bought option's midpoint the sold one
	// takes back, 0 to 1.
	Saving *float64 `json:"saving,omitempty"`
	// BeyondExpectedMove reports a sold strike at least the rule's
	// CapExpectedMoves one-sigma moves from the underlying's price.
	BeyondExpectedMove bool `json:"beyond_expected_move"`
	// Eligible reports a candidate the default rule may choose: priced,
	// a debit below the width, saving enough and capped beyond the move.
	Eligible bool `json:"eligible"`
}

// StrategySpreadsResult is the evidence for one bought option. Default is
// the index into Candidates of the proposed sold strike, -1 with
// DefaultReason when none qualifies.
type StrategySpreadsResult struct {
	Version    int              `json:"version"`
	Underlying ContractParams   `json:"underlying"`
	Spot       *float64         `json:"spot,omitempty"`
	Long       AddCallCandidate `json:"long"`
	DTE        int              `json:"dte,omitempty"`
	// ExpectedMove is one standard deviation of the underlying's price to
	// expiry at the bought option's implied volatility, in price.
	ExpectedMove  *float64                  `json:"expected_move,omitempty"`
	Rule          StrategySpreadRule        `json:"rule"`
	Candidates    []StrategySpreadCandidate `json:"candidates"`
	Default       int                       `json:"default"`
	DefaultReason string                    `json:"default_reason,omitempty"`
	OptionMode    string                    `json:"option_mode"` // live | planning | unavailable
	AsOf          time.Time                 `json:"as_of"`
}

// MethodStrategySpreadPlan sizes a debit vertical, as `add.call-plan` sizes
// one option: the most units whose net debit and broker fee fit the budget,
// held by the option-contract cap per leg, the order cap on the net premium
// and, for a bear put spread on a Rulebook hedge symbol, rule 12's
// over-hedge line measured with the spread's net delta. Each candidate size
// is an exact broker preview of the combination; nothing is placed.
const MethodStrategySpreadPlan = "strategy.spread_plan"

// StrategySpreadPlanParams are the two exact contracts (as strategy.spreads
// returns them), the net debit per spread and the budget in USD, fees
// included.
type StrategySpreadPlanParams struct {
	Long       ContractParams `json:"long"`
	Short      ContractParams `json:"short"`
	LimitPrice float64        `json:"limit_price"`
	Budget     float64        `json:"budget"`
}

// What held a spread plan's size.
const (
	SpreadPlanHeldByBudget    = "budget"
	SpreadPlanHeldByOptionCap = "option_cap"
	SpreadPlanHeldByOrderCap  = "order_cap"
	SpreadPlanHeldByHedgeRoom = "hedge_room"
)

// StrategySpreadPlanResult is the size and what held it. Debit is units ×
// limit × 100 plus FeeBound, the broker's fee bound for the combination.
// OptionCap, OrderCapUnits and HedgeRoomUnits are each limit in units, zero
// where it does not apply or is unknown; IndexProtection is rule 12's
// measurement a hedge-symbol spread was sized on, its Unmeasured the reason
// only the other limits held the size.
type StrategySpreadPlanResult struct {
	Long            ContractParams      `json:"long"`
	Short           ContractParams      `json:"short"`
	LimitPrice      float64             `json:"limit_price"`
	Budget          float64             `json:"budget"`
	Units           int                 `json:"units"`
	FeeBound        float64             `json:"fee_bound"`
	Debit           float64             `json:"debit"`
	HeldBy          string              `json:"held_by"`
	OptionCap       int                 `json:"option_cap,omitempty"`
	OrderCapUnits   int                 `json:"order_cap_units,omitempty"`
	HedgeRoomUnits  int                 `json:"hedge_room_units,omitempty"`
	NetDelta        *float64            `json:"net_delta,omitempty"`
	IndexProtection *AddIndexProtection `json:"index_protection,omitempty"`
	AsOf            time.Time           `json:"as_of"`
}
