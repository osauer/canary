package risk

import (
	"fmt"
	"maps"
	"math"
	"slices"
)

// StockAddAdmissionV1 names the complete, separately approved stock-entry contract.
const StockAddAdmissionV1 = "stock-entry-v1"

// StockAddPolicy holds optional additional stock allocation ceilings. Existing
// Rulebook and funding policy govern Add when this table is absent.
type StockAddPolicy struct {
	AdmissionContract        string   `toml:"admission_contract" json:"admission_contract"`
	MaxStockPctNLV           *float64 `toml:"max_stock_pct_nlv" json:"max_stock_pct_nlv"`
	MaxUnderlyingStockPctNLV *float64 `toml:"max_underlying_stock_pct_nlv" json:"max_underlying_stock_pct_nlv"`
}

func (p *StockAddPolicy) validate() error {
	if p == nil {
		return nil
	}
	if p.AdmissionContract != "" && p.AdmissionContract != StockAddAdmissionV1 {
		return fmt.Errorf("position_add.admission_contract must be %q", StockAddAdmissionV1)
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
	Manual                                                          bool
	Requested                                                       int
	BrokerMargin                                                    *StockAddMargin
	Cash                                                            *StockAddCash
	PendingCostBase                                                 float64
	StopQuantity, PendingQuantity                                   float64
	Rules                                                           RuleInputs
	Rulebook                                                        RulebookPolicy
}

// StockAddBlocker is an owning-layer explanation of unavailable capacity.
type StockAddBlocker struct {
	Kind    string `json:"kind"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// StockAddAllowance exposes an independent limit in account base currency.
type StockAddAllowance struct {
	Code      string  `json:"code"`
	Limit     float64 `json:"limit"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
	Shares    float64 `json:"shares"`
}

// StockAddCash explains native funding and the account reserve without pooling currencies.
type StockAddCash struct {
	Available               float64 `json:"available"`
	Committed               float64 `json:"committed"`
	CurrencyFloat           float64 `json:"currency_float"`
	ReserveInCurrency       float64 `json:"reserve_in_currency"`
	AccountReserveBase      float64 `json:"account_reserve_base"`
	OtherReserveFundingBase float64 `json:"other_reserve_funding_base"`
	Spendable               float64 `json:"spendable"`
}

// StockAddMargin is the exact candidate's broker-simulated headroom in base currency.
type StockAddMargin struct {
	Quantity              int      `json:"quantity"`
	ExcessLiquidityBase   float64  `json:"excess_liquidity_base"`
	LookAheadExcessBase   *float64 `json:"look_ahead_excess_base,omitempty"`
	InitialMarginBase     float64  `json:"initial_margin_base"`
	MaintenanceMarginBase float64  `json:"maintenance_margin_base"`
}

// StockAddRiskCheck explains the same admission measurement before and after an order.
type StockAddRiskCheck struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	BeforeStatus   string `json:"before_status"`
	BeforeEvidence string `json:"before_evidence"`
	AfterStatus    string `json:"after_status,omitempty"`
	AfterEvidence  string `json:"after_evidence,omitempty"`
}

// StockAddProtection counts working stop instructions, never guaranteed loss protection.
type StockAddProtection struct {
	StopQuantity    float64 `json:"stop_quantity"`
	UncoveredBefore float64 `json:"uncovered_before"`
	UncoveredAfter  float64 `json:"uncovered_after"`
	PendingQuantity float64 `json:"pending_quantity"`
}

// StockAddPlan is the common equity sizing result. It grants no order authority.
type StockAddPlan struct {
	MaximumKnown        bool                `json:"maximum_known"`
	OrderUpperBound     int                 `json:"order_upper_bound"`
	AllocationRoom      float64             `json:"allocation_room"`
	SupportedQuantity   int                 `json:"supported_quantity,omitempty"`
	RequestedQuantity   int                 `json:"requested_quantity,omitempty"`
	Sizing              string              `json:"sizing"`
	Allowances          []StockAddAllowance `json:"allowances,omitempty"`
	RiskChecks          []StockAddRiskCheck `json:"risk_checks,omitempty"`
	Cash                *StockAddCash       `json:"cash,omitempty"`
	Protection          *StockAddProtection `json:"protection,omitempty"`
	Margin              *StockAddMargin     `json:"margin,omitempty"`
	StockPctBefore      float64             `json:"stock_pct_before"`
	StockPctAfter       float64             `json:"stock_pct_after"`
	UnderlyingPctBefore float64             `json:"underlying_pct_before"`
	UnderlyingPctAfter  float64             `json:"underlying_pct_after"`
	MaxQuantity         int                 `json:"max_quantity"`
	Quantity            int                 `json:"quantity"`
	Before              float64             `json:"before"`
	After               float64             `json:"after"`
	Effect              string              `json:"effect"`
	Cost                float64             `json:"cost"`
	CashAfter           float64             `json:"cash_after_reserves"`
	Binding             string              `json:"binding"`
	Warnings            []StockAddBlocker   `json:"warnings,omitempty"`
	Blockers            []StockAddBlocker   `json:"blockers"`
}

// SizeStockAdd calculates an upper bound under an assumed fixed fee. The daemon
// must validate each exact candidate against its own commission and broker margin;
// this bound alone is never a verified maximum or execution authority. A zero holding needs complete inputs.
// It evaluates the post-purchase book; a protective stop is not a loss cap.
func SizeStockAdd(in StockAddInput) StockAddPlan {
	out := StockAddPlan{Before: in.CurrentQuantity, After: in.CurrentQuantity, Blockers: []StockAddBlocker{}, Effect: "increase_long"}
	if in.CurrentQuantity == 0 {
		out.Effect = "open_long"
	}
	hold := func(code, message string) StockAddPlan {
		out.Quantity = 0
		out.Blockers = append(out.Blockers, StockAddBlocker{Kind: stockAddBlockerKind(code), Code: code, Message: message})
		return out
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
	out.Cash = in.Cash
	out.RequestedQuantity = in.Requested
	out.Protection = &StockAddProtection{StopQuantity: in.StopQuantity, PendingQuantity: in.PendingQuantity, UncoveredBefore: max(0, in.CurrentQuantity-in.StopQuantity), UncoveredAfter: max(0, in.CurrentQuantity-in.StopQuantity)}
	out.StockPctBefore = in.StockValueBase / *in.Rules.NLVBase * 100
	out.StockPctAfter = out.StockPctBefore
	out.UnderlyingPctBefore = in.UnderlyingStockBase / *in.Rules.NLVBase * 100
	out.UnderlyingPctAfter = out.UnderlyingPctBefore
	out.RiskChecks = stockAddChecks(in, 0)
	unit := in.Price * in.FX
	capacities := []struct {
		key    string
		amount float64
	}{
		{"cash", (in.FreeCash - in.Fee) * in.FX},
		{"order_limit", in.OrderCapBase},
	}
	if in.Policy != nil {
		if v := in.Policy.MaxStockPctNLV; v != nil {
			capacities = append(capacities, struct {
				key    string
				amount float64
			}{"stock_allocation", *in.Rules.NLVBase**v/100 - in.StockValueBase})
		}
		if v := in.Policy.MaxUnderlyingStockPctNLV; v != nil {
			capacities = append(capacities, struct {
				key    string
				amount float64
			}{"underlying_stock", *in.Rules.NLVBase**v/100 - in.UnderlyingStockBase})
		}
	}
	// The existing order DTO uses an int; one million is its structural bound,
	// not an investment-policy allowance.
	maxQ := 1000000
	out.AllocationRoom = math.MaxFloat64
	for _, c := range capacities {
		if !finiteStockAdd(c.amount) {
			return hold("add_input_unavailable", "A capacity calculation is not finite.")
		}
		q := max(0, math.Floor(c.amount/unit))
		if !finiteStockAdd(q) {
			return hold("add_input_unavailable", "The share allowance is not finite.")
		}
		used := 0.
		switch c.key {
		case "stock_allocation":
			used = in.StockValueBase
		case "underlying_stock":
			used = in.UnderlyingStockBase
		}
		out.Allowances = append(out.Allowances, StockAddAllowance{Code: c.key, Limit: c.amount + used, Used: used, Remaining: c.amount, Shares: q})
		if c.key == "stock_allocation" || c.key == "underlying_stock" {
			out.AllocationRoom = min(out.AllocationRoom, q)
		}
		if q < float64(maxQ) {
			maxQ = int(q)
			out.Binding = c.key
		}
	}
	if out.AllocationRoom == math.MaxFloat64 {
		out.AllocationRoom = float64(maxQ)
	}
	if maxQ == 0 {
		return hold("add_no_capacity", "The available allowance cannot fund one whole share.")
	}
	// Buying into a current entry-band breach is held; binary search then remains
	// on the admissible side even where existing option hedges make risk nonlinear.
	if reason := stockAddRiskReason(in, 0); reason != "" {
		out = hold("add_risk_hold", reason)
		out.Blockers[len(out.Blockers)-1].Kind = stockAddRiskKind(in, 0)
		return out
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
	out.OrderUpperBound = lo
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
	out.StockPctAfter = (in.StockValueBase + float64(out.Quantity)*unit) / *in.Rules.NLVBase * 100
	out.UnderlyingPctAfter = (in.UnderlyingStockBase + float64(out.Quantity)*unit) / *in.Rules.NLVBase * 100
	out.Protection.UncoveredAfter = max(0, out.After-in.StopQuantity)
	out.Cost = float64(out.Quantity)*in.Price + in.Fee
	out.CashAfter = in.FreeCash - out.Cost
	if !finiteStockAdd(out.Cost) || !finiteStockAdd(out.After) || out.CashAfter < 0 {
		out.Quantity = 0
		return hold("add_input_unavailable", "The complete order does not fit the measured allowance.")
	}
	return out
}

func stockAddRiskRows(in StockAddInput, quantity int) []RuleRow {
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
	if quantity > 0 && in.BrokerMargin != nil && in.BrokerMargin.Quantity == quantity {
		r.ExcessLiquidityBase = new(in.BrokerMargin.ExcessLiquidityBase)
		r.LookAheadExcessLiquidityBase = in.BrokerMargin.LookAheadExcessBase
		r.InitialMarginBase = new(in.BrokerMargin.InitialMarginBase)
		r.MaintenanceMarginBase = new(in.BrokerMargin.MaintenanceMarginBase)
	}
	p := in.Rulebook
	// Use the owner's modes and numbers. Issuer rows see the affected issuer;
	// global rows retain the complete book. Cluster evaluation keeps all members.
	issuer := func(symbol string) string {
		for group, members := range p.IssuerGroups {
			if slices.Contains(members, symbol) {
				return group
			}
		}
		return symbol
	}
	scoped := r
	scoped.Names = slices.DeleteFunc(slices.Clone(r.Names), func(n NameInput) bool { return issuer(n.Symbol) != issuer(in.Symbol) })
	local := EvaluateRulebook(scoped, p).Rows
	p.Clusters = maps.Clone(p.Clusters)
	for group, members := range p.Clusters {
		if !slices.ContainsFunc(members, func(symbol string) bool { return issuer(symbol) == issuer(in.Symbol) }) {
			delete(p.Clusters, group)
		}
	}
	rows := EvaluateRulebook(r, p).Rows
	for i, row := range rows {
		if slices.Contains([]string{RuleSingleNameExposure, RuleDeltaSwing, RuleLossBudget, RuleEarningsSizeFreeze}, row.ID) {
			for _, own := range local {
				if own.ID == row.ID {
					rows[i] = own
					break
				}
			}
		}
	}
	return slices.DeleteFunc(rows, func(row RuleRow) bool {
		return !slices.Contains([]string{RuleSingleNameExposure, RuleCashSellOnly, RuleNetExposure, RuleLossBudget, RuleMarginHeadroom, RuleDeltaSwing, RuleClusterStress, RuleHedgeIntegrity, RuleEarningsSizeFreeze, RuleFXExposure}, row.ID)
	})
}

func stockAddRiskReason(in StockAddInput, quantity int) string {
	for _, row := range stockAddRiskRows(in, quantity) {
		// FX and event findings retain their advisory semantics. The issuer loss
		// check already constrains earnings-related size; absent clusters are not
		// invented. An intentionally disabled rule grants no measurement.
		if row.ID == RuleFXExposure || row.ID == RuleEarningsSizeFreeze || row.Status == RuleStatusNotEvaluated || row.Status == RuleStatusInfo || row.Status == RuleStatusPass {
			continue
		}
		if in.Manual && (row.Status == RuleStatusWatch || row.Status == RuleStatusAct) {
			continue
		}
		return row.Title + ": " + row.Evidence
	}
	return ""
}

func stockAddWarnings(in StockAddInput, quantity int) []StockAddBlocker {
	var out []StockAddBlocker
	for _, row := range stockAddRiskRows(in, quantity) {
		if row.Status == RuleStatusWatch || row.Status == RuleStatusAct {
			out = append(out, StockAddBlocker{Kind: "warning", Code: row.ID, Message: stockAddWarningMessage(row)})
		}
	}
	return out
}

func stockAddWarningMessage(row RuleRow) string {
	switch row.ID {
	case RuleCashSellOnly:
		return "Option premium is above the policy warning level; new buying works against sell-only guidance."
	case RuleHedgeIntegrity:
		return "This purchase leaves index protection outside its policy range."
	case RuleMarginHeadroom:
		return "This purchase leaves margin headroom below the policy warning level."
	case RuleEarningsSizeFreeze:
		return "This issuer is already large and has approaching earnings."
	}
	limit := row.WatchThreshold
	if limit == nil {
		limit = row.Threshold
	}
	if row.Observed != nil && limit != nil {
		return fmt.Sprintf("%s: %.1f %s; warning level %.1f.", row.Title, *row.Observed, row.Unit, *limit)
	}
	return row.Title + ": policy warning applies."
}

func stockAddRiskKind(in StockAddInput, quantity int) string {
	for _, row := range stockAddRiskRows(in, quantity) {
		if row.Status == RuleStatusUnknown {
			return "evidence"
		}
	}
	return "capacity"
}

func stockAddChecks(in StockAddInput, quantity int) []StockAddRiskCheck {
	before := stockAddRiskRows(in, 0)
	var after []RuleRow
	if quantity > 0 {
		after = stockAddRiskRows(in, quantity)
	}
	var out []StockAddRiskCheck
	for _, b := range before {
		c := StockAddRiskCheck{ID: b.ID, Title: b.Title, BeforeStatus: b.Status, BeforeEvidence: b.Evidence}
		for _, a := range after {
			if a.ID == b.ID {
				c.AfterStatus, c.AfterEvidence = a.Status, a.Evidence
			}
		}
		out = append(out, c)
	}
	return out
}

// CheckStockAdd evaluates only the requested quantity, using its own broker evidence.
// It never extrapolates that quantity's fee or margin to an unquoted maximum.
func CheckStockAdd(in StockAddInput) StockAddPlan {
	margin := in.BrokerMargin
	capacity := in
	capacity.Requested, capacity.Fee, capacity.BrokerMargin = 0, 0, nil
	out := SizeStockAdd(capacity)
	out.OrderUpperBound, out.MaxQuantity = out.MaxQuantity, 0
	out.Quantity, out.After, out.Cost, out.CashAfter = 0, out.Before, 0, in.FreeCash
	out.RequestedQuantity = in.Requested
	out.Sizing = "quantity"
	out.StockPctAfter, out.UnderlyingPctAfter = out.StockPctBefore, out.UnderlyingPctBefore
	if out.Protection != nil {
		out.Protection.UncoveredAfter = out.Protection.UncoveredBefore
	}
	hold := func(kind, code, message string) StockAddPlan {
		out.Blockers = append(out.Blockers, StockAddBlocker{Kind: kind, Code: code, Message: message})
		return out
	}
	if len(out.Blockers) > 0 {
		return out
	}
	if in.Requested <= 0 || in.Requested > out.OrderUpperBound {
		return hold("capacity", "add_quantity_above_max", "The requested addition exceeds the cash, allocation, order or risk allowance before broker costs.")
	}
	if !finiteStockAdd(in.Fee) || in.Fee < 0 {
		return hold("evidence", "add_fee_unavailable", "The exact order has no valid commission bound.")
	}
	if margin == nil || margin.Quantity != in.Requested || !finiteStockAdd(margin.ExcessLiquidityBase) || !finiteStockAdd(margin.InitialMarginBase) || !finiteStockAdd(margin.MaintenanceMarginBase) || (margin.LookAheadExcessBase != nil && !finiteStockAdd(*margin.LookAheadExcessBase)) {
		return hold("evidence", "add_margin_unavailable", "The exact order needs complete broker margin evidence.")
	}
	if margin.ExcessLiquidityBase < 0 || margin.LookAheadExcessBase != nil && *margin.LookAheadExcessBase < 0 {
		return hold("capacity", "add_margin_shortfall", "The purchase would leave insufficient broker margin.")
	}
	out.RiskChecks = stockAddChecks(in, in.Requested)
	out.Margin = margin
	out.Cost = float64(in.Requested)*in.Price + in.Fee
	out.CashAfter = in.FreeCash - out.Cost
	if !finiteStockAdd(out.Cost) || out.CashAfter < 0 {
		return hold("capacity", "add_cash_limit", "The exact order, including its commission bound, exceeds spendable cash.")
	}
	if reason := stockAddRiskReason(in, in.Requested); reason != "" {
		return hold(stockAddRiskKind(in, in.Requested), "add_risk_hold", reason)
	}
	out.Warnings = stockAddWarnings(in, in.Requested)
	out.Quantity = in.Requested
	out.After = in.CurrentQuantity + float64(in.Requested)
	out.StockPctAfter = (in.StockValueBase + float64(in.Requested)*in.Price*in.FX) / *in.Rules.NLVBase * 100
	out.UnderlyingPctAfter = (in.UnderlyingStockBase + float64(in.Requested)*in.Price*in.FX) / *in.Rules.NLVBase * 100
	out.Protection.UncoveredAfter = max(0, out.After-in.StopQuantity)
	return out
}

func stockAddBlockerKind(code string) string {
	switch code {
	case "add_policy_unapproved", "add_policy_invalid":
		return "policy"
	case "add_no_capacity", "add_risk_hold", "add_quantity_above_max":
		return "capacity"
	default:
		return "evidence"
	}
}

func finiteStockAdd(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
