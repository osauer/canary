package rpc

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/core"
)

// MethodStrategyOne serves strategy one's screen (rule
// strong-names-v1): the watch list, the leading set, the names that left,
// the entry plans and the open names' adds and exits, on the latest
// settled US close. Read-only: the strategy holds no risk allocation until
// the owner writes one, so its plans buy nothing and no row is served.
const MethodStrategyOne = "strategy.one"

// StrategyOneResult is the screen with what it was read on.
type StrategyOneResult struct {
	core.StrongNamesScreen
	EvaluatedAt time.Time `json:"evaluated_at"`
	// Status is ready, or hold with Reason when the screen could not run.
	Status string `json:"status"`
	// GateState and GateScale are the regime gate the plans were sized at.
	GateState string  `json:"gate_state"`
	GateScale float64 `json:"gate_scale_pct"`
	// RiskBase is the mandate's risk base in BaseCurrency and RiskBaseUSD
	// the same in the names' currency at USDRate (base per USD).
	RiskBase     float64 `json:"risk_base"`
	BaseCurrency string  `json:"base_currency,omitempty"`
	RiskBaseUSD  float64 `json:"risk_base_usd"`
	USDRate      float64 `json:"usd_rate,omitempty"`
	// AllocationPct is the register's risk allocation for the strategy:
	// zero while no entry is written.
	AllocationPct float64 `json:"allocation_pct"`
	// Undated names are constituents whose closes could not be laid on
	// the US calendar against their dated bars; they are left out.
	Undated  int  `json:"undated,omitempty"`
	ReadOnly bool `json:"read_only"`
	// Notes say what the screen does not yet read.
	Notes []string `json:"notes,omitempty"`
}

// FetchStrategyOne reads strategy one's screen.
func FetchStrategyOne(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}) (StrategyOneResult, error) {
	var out StrategyOneResult
	if err := conn.Call(ctx, MethodStrategyOne, struct{}{}, &out); err != nil {
		return StrategyOneResult{}, err
	}
	return out, nil
}
