package rpc

import "time"

// MethodAddCallPlan sizes a long call, or a protective or index put, against an owner budget, without submission.
const MethodAddCallPlan = "add.call-plan"

// AddCallPlanParams contains exact intended terms and the owner's debit ceiling.
type AddCallPlanParams struct {
	RoundSize  bool           `json:"round_size,omitempty"`
	Contract   ContractParams `json:"contract"`
	LimitPrice float64        `json:"limit_price"`
	Budget     float64        `json:"budget"`
}

// AddCallPlanResult carries a checked lot, not a maximum-capacity claim or token.
type AddCallPlanResult struct {
	RoundSize  bool           `json:"round_size,omitempty"`
	OverBudget bool           `json:"over_budget,omitempty"`
	Contract   ContractParams `json:"contract"`
	LimitPrice float64        `json:"limit_price"`
	Budget     float64        `json:"budget"`
	Quantity   int            `json:"quantity"`
	FeeBound   float64        `json:"fee_bound"`
	Debit      float64        `json:"debit"`
	// HeldToCap says the option cap in force ([order_limits].
	// max_option_contracts, OptionCap) held the budget's size.
	HeldToCap bool `json:"held_to_cap,omitempty"`
	OptionCap int  `json:"option_cap,omitempty"`
	// CoverCapped says a put was held to the shares it may still protect
	// or, for an index put, to rule 12's over-hedge line
	// (AddPutCover.RoomContracts).
	CoverCapped bool `json:"cover_capped,omitempty"`
	// IndexProtection is rule 12's measurement an index put was sized on;
	// its Unmeasured is the warning that only the option cap and the budget
	// held the size.
	IndexProtection *AddIndexProtection `json:"index_protection,omitempty"`
	AsOf            time.Time           `json:"as_of"`
}
