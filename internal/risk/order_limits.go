package risk

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Order limits (owner decision 2026-10-05 19:56 CEST). The four per-order
// gates that config.toml [trading] once held are risk limits and live in the
// constitution as [order_limits]; the notional cap scales with the book:
//
//	cap in force = min(ceiling, max(floor, pct/100 × NLV))
//
// Every key is read from the file only. A missing key is not defaulted: the
// daemon refuses every order preview with a blocker naming it. An NLV that
// cannot be read currently binds the floor, the smaller cap, and says so.

// Order-limit keys as the constitution file spells them.
const (
	OrderLimitMaxOrderFloorBase     = "max_order_floor_base"
	OrderLimitMaxOrderPctNLV        = "max_order_pct_nlv"
	OrderLimitMaxOrderCeilingBase   = "max_order_ceiling_base"
	OrderLimitMaxOptionContracts    = "max_option_contracts"
	OrderLimitAllowStockShort       = "allow_stock_short"
	OrderLimitAllowOptionSellToOpen = "allow_option_sell_to_open"
)

// OrderLimitsTable is the constitution table the order limits live in.
const OrderLimitsTable = "order_limits"

// OrderLimitFloorOverrideControl is the one order limit a one-shot policy
// override may name: while the override lasts the floor is lifted to the
// ceiling, so the cap in force is the ceiling. It replaces the retired
// runtime setting trading.limits.max_notional.
const OrderLimitFloorOverrideControl = OrderLimitsTable + "." + OrderLimitMaxOrderFloorBase

// Order cap bindings: which part of the formula sets the cap in force.
const (
	OrderCapBoundFloor    = "floor"
	OrderCapBoundPctNLV   = "pct_nlv"
	OrderCapBoundCeiling  = "ceiling"
	OrderCapBoundOverride = "override"
)

// ConstitutionOrderLimits is the [order_limits] table. Every field is a
// pointer: nil means the key is not written, and no code value replaces it.
type ConstitutionOrderLimits struct {
	// MaxOrderFloorBase is the smallest order cap in force, in base currency:
	// the cap never falls below it however small the book, and it is the cap
	// whenever NLV cannot be read.
	MaxOrderFloorBase *float64 `toml:"max_order_floor_base" json:"max_order_floor_base"`
	// MaxOrderPctNLV is the share of net liquidation value, in percent, that
	// sets the cap in force between the floor and the ceiling.
	MaxOrderPctNLV *float64 `toml:"max_order_pct_nlv" json:"max_order_pct_nlv"`
	// MaxOrderCeilingBase is the largest order cap in force, in base currency,
	// however large the book.
	MaxOrderCeilingBase *float64 `toml:"max_order_ceiling_base" json:"max_order_ceiling_base"`
	// MaxOptionContracts caps the contracts of every single-leg option order
	// and every leg of a strategy close.
	MaxOptionContracts *int `toml:"max_option_contracts" json:"max_option_contracts"`
	// AllowStockShort permits stock or ETF orders that open or flip a short.
	AllowStockShort *bool `toml:"allow_stock_short" json:"allow_stock_short"`
	// AllowOptionSellToOpen permits option sell-to-open orders.
	AllowOptionSellToOpen *bool `toml:"allow_option_sell_to_open" json:"allow_option_sell_to_open"`
}

// OrderLimitKeys lists the table's keys in file order.
func OrderLimitKeys() []string {
	return []string{OrderLimitMaxOrderFloorBase, OrderLimitMaxOrderPctNLV, OrderLimitMaxOrderCeilingBase,
		OrderLimitMaxOptionContracts, OrderLimitAllowStockShort, OrderLimitAllowOptionSellToOpen}
}

// MissingKeys names, as order_limits.KEY, every key the table does not write;
// a nil table misses all of them.
func (o *ConstitutionOrderLimits) MissingKeys() []string {
	var have ConstitutionOrderLimits
	if o != nil {
		have = *o
	}
	present := map[string]bool{
		OrderLimitMaxOrderFloorBase:     have.MaxOrderFloorBase != nil,
		OrderLimitMaxOrderPctNLV:        have.MaxOrderPctNLV != nil,
		OrderLimitMaxOrderCeilingBase:   have.MaxOrderCeilingBase != nil,
		OrderLimitMaxOptionContracts:    have.MaxOptionContracts != nil,
		OrderLimitAllowStockShort:       have.AllowStockShort != nil,
		OrderLimitAllowOptionSellToOpen: have.AllowOptionSellToOpen != nil,
	}
	var out []string
	for _, k := range OrderLimitKeys() {
		if !present[k] {
			out = append(out, OrderLimitsTable+"."+k)
		}
	}
	return out
}

// validate rejects written values no cap can be built from; it never fills a
// missing key.
func (o *ConstitutionOrderLimits) validate() error {
	if o == nil {
		return nil
	}
	finite := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
	if v := o.MaxOrderFloorBase; v != nil && (!finite(*v) || *v <= 0) {
		return fmt.Errorf("order_limits.max_order_floor_base must be positive")
	}
	if v := o.MaxOrderPctNLV; v != nil && (!finite(*v) || *v <= 0 || *v > 100) {
		return fmt.Errorf("order_limits.max_order_pct_nlv must be in (0, 100]")
	}
	if v := o.MaxOrderCeilingBase; v != nil && (!finite(*v) || *v <= 0) {
		return fmt.Errorf("order_limits.max_order_ceiling_base must be positive")
	}
	if o.MaxOrderFloorBase != nil && o.MaxOrderCeilingBase != nil && *o.MaxOrderFloorBase > *o.MaxOrderCeilingBase {
		return fmt.Errorf("order_limits.max_order_floor_base must not exceed max_order_ceiling_base")
	}
	if v := o.MaxOptionContracts; v != nil && *v <= 0 {
		return fmt.Errorf("order_limits.max_option_contracts must be positive")
	}
	return nil
}

// OrderLimitsNLV is the net liquidation value the cap scales with: a
// current, same-account reading in the account base currency, or the reason
// none is available.
type OrderLimitsNLV struct {
	Base        float64
	AsOf        time.Time
	Unavailable string
}

// OrderLimitsOverride is an active one-shot override of the floor.
type OrderLimitsOverride struct {
	ID        string
	ExpiresAt time.Time
}

// OrderLimitsInForce is the order limits as one order is judged: the file's
// values, the NLV they scale with, the cap in force and the term that sets
// it. Complete is false while a key is missing; then Missing names each one
// and every order preview is refused.
type OrderLimitsInForce struct {
	Complete     bool     `json:"complete"`
	Missing      []string `json:"missing,omitempty"`
	Unavailable  string   `json:"unavailable,omitempty"`
	BaseCurrency string   `json:"base_currency,omitempty"`

	FloorBase   float64 `json:"max_order_floor_base,omitempty"`
	PctNLV      float64 `json:"max_order_pct_nlv,omitempty"`
	CeilingBase float64 `json:"max_order_ceiling_base,omitempty"`

	NLVBase        *float64  `json:"nlv_base,omitempty"`
	NLVAsOf        time.Time `json:"nlv_as_of,omitzero"`
	NLVUnavailable string    `json:"nlv_unavailable,omitempty"`
	// PctNLVBase is pct/100 × NLV, before the floor and the ceiling.
	PctNLVBase *float64 `json:"pct_nlv_base,omitempty"`

	CapBase  float64 `json:"cap_base,omitempty"`
	CapBound string  `json:"cap_bound,omitempty"`

	OverrideID        string    `json:"override_id,omitempty"`
	OverrideExpiresAt time.Time `json:"override_expires_at,omitzero"`

	MaxOptionContracts    int  `json:"max_option_contracts,omitempty"`
	AllowStockShort       bool `json:"allow_stock_short"`
	AllowOptionSellToOpen bool `json:"allow_option_sell_to_open"`

	// Summary says the cap in force and how it was bound, in plain words,
	// or why there is none.
	Summary string `json:"summary"`
}

// EvaluateOrderLimits computes the limits in force from the constitution's
// table. A nil table, or a nil constitution (unavailable says why), misses
// every key. The floor override lifts the floor to the ceiling.
func EvaluateOrderLimits(o *ConstitutionOrderLimits, baseCurrency string, nlv OrderLimitsNLV, override *OrderLimitsOverride, unavailable string) OrderLimitsInForce {
	base := strings.ToUpper(strings.TrimSpace(baseCurrency))
	out := OrderLimitsInForce{BaseCurrency: base, Missing: o.MissingKeys(), Unavailable: strings.TrimSpace(unavailable)}
	if len(out.Missing) > 0 {
		if out.Unavailable != "" {
			out.Summary = out.Unavailable + "; every order preview is refused until risk-policy.toml writes [order_limits] (canary policy ensure)"
		} else {
			out.Summary = "risk-policy.toml [order_limits] does not write " + strings.Join(out.Missing, ", ") + "; every order preview is refused until it does (canary policy ensure)"
		}
		return out
	}
	out.Complete = true
	out.FloorBase, out.PctNLV, out.CeilingBase = *o.MaxOrderFloorBase, *o.MaxOrderPctNLV, *o.MaxOrderCeilingBase
	out.MaxOptionContracts = *o.MaxOptionContracts
	out.AllowStockShort, out.AllowOptionSellToOpen = *o.AllowStockShort, *o.AllowOptionSellToOpen

	pctText := strconv.FormatFloat(out.PctNLV, 'f', -1, 64) + "%"
	if nlv.Base > 0 && !math.IsNaN(nlv.Base) && !math.IsInf(nlv.Base, 0) && nlv.Unavailable == "" {
		out.NLVBase, out.NLVAsOf = new(nlv.Base), nlv.AsOf
		out.PctNLVBase = new(out.PctNLV / 100 * nlv.Base)
	} else {
		out.NLVUnavailable = strings.TrimSpace(nlv.Unavailable)
		if out.NLVUnavailable == "" {
			out.NLVUnavailable = "net liquidation value cannot be read"
		}
	}
	switch {
	case override != nil:
		out.CapBase, out.CapBound = out.CeilingBase, OrderCapBoundOverride
		out.OverrideID, out.OverrideExpiresAt = override.ID, override.ExpiresAt
		out.Summary = fmt.Sprintf("%s (override %s lifts the floor to the ceiling until %s; [order_limits])",
			FormatOrderMoney(out.CapBase, base), override.ID, override.ExpiresAt.UTC().Format(time.RFC3339))
	case out.PctNLVBase == nil:
		out.CapBase, out.CapBound = out.FloorBase, OrderCapBoundFloor
		out.Summary = fmt.Sprintf("%s (the floor: %s, so %s of NLV cannot apply and the smaller cap holds; [order_limits])",
			FormatOrderMoney(out.CapBase, base), out.NLVUnavailable, pctText)
	case *out.PctNLVBase >= out.CeilingBase:
		out.CapBase, out.CapBound = out.CeilingBase, OrderCapBoundCeiling
		out.Summary = fmt.Sprintf("%s (the ceiling; %s of NLV %s would be %s; [order_limits])",
			FormatOrderMoney(out.CapBase, base), pctText, FormatOrderMoney(nlv.Base, base), FormatOrderMoney(*out.PctNLVBase, base))
	case *out.PctNLVBase <= out.FloorBase:
		out.CapBase, out.CapBound = out.FloorBase, OrderCapBoundFloor
		out.Summary = fmt.Sprintf("%s (the floor; %s of NLV %s is %s; [order_limits])",
			FormatOrderMoney(out.CapBase, base), pctText, FormatOrderMoney(nlv.Base, base), FormatOrderMoney(*out.PctNLVBase, base))
	default:
		out.CapBase, out.CapBound = *out.PctNLVBase, OrderCapBoundPctNLV
		out.Summary = fmt.Sprintf("%s (%s of NLV %s; [order_limits])",
			FormatOrderMoney(out.CapBase, base), pctText, FormatOrderMoney(nlv.Base, base))
	}
	return out
}

// FormatOrderMoney renders an amount with thousands separators, whole units
// when the cents are zero: 12000 EUR reads "12,000 EUR".
func FormatOrderMoney(v float64, ccy string) string {
	neg := v < 0
	v = math.Abs(v)
	cents := int64(math.Round(v * 100))
	whole, frac := cents/100, cents%100
	digits := strconv.FormatInt(whole, 10)
	var b strings.Builder
	if neg && cents != 0 {
		b.WriteByte('-')
	}
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if frac != 0 {
		fmt.Fprintf(&b, ".%02d", frac)
	}
	if ccy = strings.TrimSpace(ccy); ccy != "" {
		b.WriteString(" " + ccy)
	}
	return b.String()
}
