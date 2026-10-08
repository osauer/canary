package risk

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// StockAddPolicy contains the owner's stock allocation ceilings. Absent values
// are unapproved. Installing the feature writes no policy and supplies no defaults.
type StockAddPolicy struct {
	MaxStockPctNLV           *float64 `toml:"max_stock_pct_nlv" json:"max_stock_pct_nlv"`
	MaxUnderlyingStockPctNLV *float64 `toml:"max_underlying_stock_pct_nlv" json:"max_underlying_stock_pct_nlv"`
}

func (p *StockAddPolicy) validate() error {
	if p == nil {
		return nil
	}
	for key, v := range map[string]*float64{"max_stock_pct_nlv": p.MaxStockPctNLV, "max_underlying_stock_pct_nlv": p.MaxUnderlyingStockPctNLV} {
		if v != nil && (!finiteStockAdd(*v) || *v <= 0 || *v > 100) {
			return fmt.Errorf("position_add.%s must be finite and in (0,100]", key)
		}
	}
	return nil
}

// StockAddInput is daemon-assembled evidence, never accepted from a model or
// browser. Stock amounts include outstanding purchases; FreeCash already deducts
// native-currency reserves and all fee-inclusive commitments. Rules includes
// pending stock purchases and every existing option leg.
type StockAddInput struct {
	Policy                                                          *StockAddPolicy
	Symbol                                                          string
	ConID                                                           int
	Price, FX, CurrentQuantity, StockValueBase, UnderlyingStockBase float64
	FreeCash, Fee, OrderCapBase                                     float64
	Requested                                                       int
	Rules                                                           RuleInputs
	Rulebook                                                        RulebookPolicy
}

// StockAddBlocker is an owning-layer explanation of unavailable capacity.
type StockAddBlocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StockAddPlan is the common equity sizing result. It grants no order authority.
type StockAddPlan struct {
	MaxQuantity int               `json:"max_quantity"`
	Quantity    int               `json:"quantity"`
	Before      float64           `json:"before"`
	After       float64           `json:"after"`
	Effect      string            `json:"effect"`
	Cost        float64           `json:"cost"`
	CashAfter   float64           `json:"cash_after_reserves"`
	Binding     string            `json:"binding"`
	Blockers    []StockAddBlocker `json:"blockers"`
}

// SizeStockAdd finds whole shares within cash, allocations, per-order controls
// and the canonical Rulebook entry bands. A zero holding needs complete inputs.
// It evaluates the post-purchase book; a protective stop is not a loss cap.
func SizeStockAdd(in StockAddInput) StockAddPlan {
	out := StockAddPlan{Before: in.CurrentQuantity, After: in.CurrentQuantity, Blockers: []StockAddBlocker{}, Effect: "increase_long"}
	if in.CurrentQuantity == 0 {
		out.Effect = "open_long"
	}
	hold := func(code, message string) StockAddPlan {
		out.Quantity = 0
		out.Blockers = append(out.Blockers, StockAddBlocker{code, message})
		return out
	}
	if in.Policy == nil || in.Policy.MaxStockPctNLV == nil || in.Policy.MaxUnderlyingStockPctNLV == nil {
		return hold("add_policy_unapproved", "Stock allocation and per-underlying stock limits need approved policy values.")
	}
	if err := in.Policy.validate(); err != nil {
		return hold("add_policy_invalid", err.Error())
	}
	if !in.Rules.Account.Healthy || !in.Rules.Positions.Healthy || in.Rules.NLVBase == nil || !finiteStockAdd(*in.Rules.NLVBase) || *in.Rules.NLVBase <= 0 {
		return hold("add_book_unavailable", "A complete current account and portfolio are required; missing holdings are not zero.")
	}
	for _, v := range []float64{in.Price, in.FX, in.Price * in.FX, in.OrderCapBase} {
		if !finiteStockAdd(v) || v <= 0 {
			return hold("add_input_unavailable", "Current price, currency conversion and order cap must be positive and finite.")
		}
	}
	for _, v := range []float64{in.CurrentQuantity, in.StockValueBase, in.UnderlyingStockBase, in.Fee} {
		if !finiteStockAdd(v) || v < 0 {
			return hold("add_input_unavailable", "An existing long or empty position, measured allocations and a fee bound are required.")
		}
	}
	if !finiteStockAdd(in.FreeCash) || in.Requested < 0 || in.ConID <= 0 || in.Symbol == "" {
		return hold("add_input_unavailable", "Exact stock identity, cash and a nonnegative requested quantity are required.")
	}
	unit := in.Price * in.FX
	capacities := []struct {
		key    string
		amount float64
	}{
		{"cash", (in.FreeCash - in.Fee) * in.FX},
		{"stock_allocation", *in.Rules.NLVBase**in.Policy.MaxStockPctNLV/100 - in.StockValueBase},
		{"underlying_stock", *in.Rules.NLVBase**in.Policy.MaxUnderlyingStockPctNLV/100 - in.UnderlyingStockBase},
		{"order_limit", in.OrderCapBase},
	}
	// The existing order DTO uses an int; one million is its structural bound,
	// not an investment-policy allowance.
	maxQ := 1000000
	for _, c := range capacities {
		if !finiteStockAdd(c.amount) {
			return hold("add_input_unavailable", "A capacity calculation is not finite.")
		}
		q := max(0, math.Floor(c.amount/unit))
		if q < float64(maxQ) {
			maxQ = int(q)
			out.Binding = c.key
		}
	}
	if maxQ == 0 {
		return hold("add_no_capacity", "The available allowance cannot fund one whole share.")
	}
	// Buying into a current entry-band breach is held; binary search then remains
	// on the admissible side even where existing option hedges make risk nonlinear.
	if reason := stockAddRiskReason(in, 0); reason != "" {
		return hold("add_risk_hold", reason)
	}
	lo, hi := 0, maxQ
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		if stockAddRiskReason(in, mid) == "" {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	out.MaxQuantity = lo
	if lo < maxQ {
		out.Binding = "risk_budget"
	}
	if lo == 0 {
		return hold("add_risk_hold", stockAddRiskReason(in, 1))
	}
	out.Quantity = lo
	if in.Requested > 0 {
		if in.Requested > lo {
			return hold("add_quantity_above_max", "The requested quantity exceeds the current permitted addition.")
		}
		out.Quantity = in.Requested
	}
	out.After = in.CurrentQuantity + float64(out.Quantity)
	out.Cost = float64(out.Quantity)*in.Price + in.Fee
	out.CashAfter = in.FreeCash - out.Cost
	if !finiteStockAdd(out.Cost) || !finiteStockAdd(out.After) || out.CashAfter < 0 {
		out.Quantity = 0
		return hold("add_input_unavailable", "The complete order does not fit the measured allowance.")
	}
	return out
}

func stockAddRiskReason(in StockAddInput, quantity int) string {
	r := in.Rules
	r.Names = slices.Clone(r.Names)
	increment := float64(quantity) * in.Price * in.FX
	if quantity > 0 {
		i := slices.IndexFunc(r.Names, func(n NameInput) bool { return n.Symbol == in.Symbol })
		if i < 0 {
			r.Names = append(r.Names, NameInput{Symbol: in.Symbol, StockConID: in.ConID, StockSecType: "STK", UnderlyingSecType: "STK", StockFXToBase: new(in.FX), StockMark: in.Price, ExposureBaseComplete: true})
			i = len(r.Names) - 1
		}
		n := &r.Names[i]
		// Existing exact stock identity/FX were checked by the daemon. Revalue the
		// combined stock leg conservatively at the greater of current mark and limit.
		mark := max(n.StockMark, in.Price)
		n.StockQuantity += float64(quantity)
		n.StockMark = mark
		n.StockFXToBase = new(in.FX)
		n.HasStockLeg = true
		n.StockConID = in.ConID
		n.StockSecType = "STK"
		n.UnderlyingSecType = "STK"
		n.ExposureBase += increment
		n.MarketValueBase += increment
	}
	if r.ExcessLiquidityBase != nil {
		r.ExcessLiquidityBase = new(*r.ExcessLiquidityBase - increment - in.Fee*in.FX)
	}
	if r.LookAheadExcessLiquidityBase != nil {
		r.LookAheadExcessLiquidityBase = new(*r.LookAheadExcessLiquidityBase - increment - in.Fee*in.FX)
	}
	p := in.Rulebook
	p.Modes = maps.Clone(p.Modes)
	if p.Modes == nil {
		p.Modes = map[string]string{}
	}
	// Display modes do not turn off admission measurements.
	for _, id := range []string{RuleSingleNameExposure, RuleCashSellOnly, RuleNetExposure, RuleLossBudget, RuleMarginHeadroom} {
		p.Modes[id] = RuleModeTrack
	}
	for _, row := range EvaluateRulebook(r, p).Rows {
		if !slices.Contains([]string{RuleSingleNameExposure, RuleCashSellOnly, RuleNetExposure, RuleLossBudget, RuleMarginHeadroom}, row.ID) {
			continue
		}
		if row.Status == RuleStatusPass || row.Reason == RuleReasonNoLongOptions {
			continue
		}
		return row.Title + ": " + row.Evidence
	}
	return ""
}

func finiteStockAdd(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
