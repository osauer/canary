package ibkr

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Bond orders (internal-docs/design/cash-sweep.md, Phase B order path). A
// BOND order is a DAY limit order on one contract id. Its quantity counts the
// instrument's order unit and its price is quoted per 100 of face; both must
// sit on the size and price grid the line's own contract details carry.
// ValidateOrder refuses any BOND order that does not, so neither encoder can
// send one, and NewBondLimitOrder refuses it at construction.

// BondOrderRules is the order grid of one bond line, read from its contract
// details: the minimum order size and the size increment in order units, and
// the minimum price increment in the quote's convention (per 100 of face).
type BondOrderRules struct {
	MinTick       float64
	MinSize       float64
	SizeIncrement float64
}

// BondOrderRulesFrom reads the grid from a line's contract details. The size
// rules arrive only on a complete frame; a line without them, or without a
// minimum tick, cannot be ordered.
func BondOrderRulesFrom(d BondContractDetails) (BondOrderRules, error) {
	r := BondOrderRules{MinTick: d.MinTick}
	if d.Complete {
		r.MinSize, r.SizeIncrement = d.MinSize, d.SizeIncrement
	}
	return r, r.Validate()
}

// Validate reports whether the grid can bound an order: a positive minimum
// tick and minimum size, and sizes in whole order units.
func (r BondOrderRules) Validate() error {
	switch {
	case !bondPositiveFinite(r.MinTick):
		return fmt.Errorf("bond contract details carry no minimum tick")
	case !bondPositiveFinite(r.MinSize):
		return fmt.Errorf("bond contract details carry no minimum size")
	case math.IsNaN(r.SizeIncrement) || math.IsInf(r.SizeIncrement, 0) || r.SizeIncrement < 0:
		return fmt.Errorf("bond contract details carry an invalid size increment")
	case !bondWholeUnits(r.MinSize) || (r.SizeIncrement > 0 && !bondWholeUnits(r.SizeIncrement)):
		return fmt.Errorf("bond size rules (minimum %s, increment %s) are not whole order units", formatBondSize(r.MinSize), formatBondSize(r.SizeIncrement))
	}
	return nil
}

// Step is the size grid in order units: the size increment, or the minimum
// size when the line names no increment.
func (r BondOrderRules) Step() int {
	if r.SizeIncrement > 0 {
		return int(math.Round(r.SizeIncrement))
	}
	return int(math.Round(r.MinSize))
}

// Minimum is the minimum size in whole order units.
func (r BondOrderRules) Minimum() int {
	return int(math.Round(r.MinSize))
}

// CheckQuantity refuses a quantity below the minimum size or off the size
// grid.
func (r BondOrderRules) CheckQuantity(quantity int) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if quantity < r.Minimum() {
		return fmt.Errorf("bond order quantity %d is below the contract's minimum size %d", quantity, r.Minimum())
	}
	if step := r.Step(); step > 0 && quantity%step != 0 {
		return fmt.Errorf("bond order quantity %d is not a multiple of the contract's size step %d", quantity, step)
	}
	return nil
}

// CheckPrice refuses a price that is not positive or not on the minimum tick.
func (r BondOrderRules) CheckPrice(price float64) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if !bondPositiveFinite(price) {
		return fmt.Errorf("bond order limit price must be positive")
	}
	if !OnBondTick(price, r.MinTick) {
		return fmt.Errorf("bond order limit price %s is not on the contract's minimum tick %s", strconv.FormatFloat(price, 'f', -1, 64), strconv.FormatFloat(r.MinTick, 'f', -1, 64))
	}
	return nil
}

// OnBondTick reports whether price is a whole number of ticks, allowing for
// the float noise of a decimal tick.
func OnBondTick(price, tick float64) bool {
	if !bondPositiveFinite(price) || !bondPositiveFinite(tick) {
		return false
	}
	n := price / tick
	return math.Abs(n-math.Round(n)) <= 1e-6
}

// BondTickFloor moves price down onto the tick grid: a whole number of
// ticks, cleaned of float noise (a price already on the grid stays).
func BondTickFloor(price, tick float64) float64 {
	return bondTickSnap(price, tick, math.Floor)
}

// BondTickCeil moves price up onto the tick grid, like BondTickFloor.
func BondTickCeil(price, tick float64) float64 {
	return bondTickSnap(price, tick, math.Ceil)
}

// BondTickNearest rounds price to the nearest tick, like BondTickFloor.
func BondTickNearest(price, tick float64) float64 {
	return bondTickSnap(price, tick, math.Round)
}

func bondTickSnap(price, tick float64, round func(float64) float64) float64 {
	if !bondPositiveFinite(tick) {
		return price
	}
	n := price / tick
	if math.Abs(n-math.Round(n)) <= 1e-6 {
		n = math.Round(n)
	} else {
		n = round(n)
	}
	// Ten decimals keep 1/256-of-a-point ticks exact and drop binary noise.
	return math.Round(n*tick*1e10) / 1e10
}

// NewBondLimitOrder builds the one order shape Canary sends for a bill or
// bond (secType BILL stays BILL, anything else goes as BOND): a DAY
// limit order for a contract id, with the line's grid attached so the
// encoders re-check it. It refuses a quantity off the size grid and a price
// off the minimum tick.
func NewBondLimitOrder(contract Contract, rules BondOrderRules, action string, quantity int, price float64) (*Contract, *RawOrder, error) {
	c := contract
	c.SecType = BillOrBondSecType(c.SecType)
	c.Symbol = strings.ToUpper(strings.TrimSpace(c.Symbol))
	c.Currency = strings.ToUpper(strings.TrimSpace(c.Currency))
	c.Exchange = strings.ToUpper(strings.TrimSpace(c.Exchange))
	if c.Exchange == "" {
		c.Exchange = "SMART"
	}
	c.BondRules = &rules
	order := &RawOrder{Action: strings.ToUpper(strings.TrimSpace(action)), TotalQty: quantity, OrderType: "LMT", LmtPrice: price, LmtPriceSet: true, TIF: "DAY"}
	probe := &IBKROrder{ConID: c.ConID, Symbol: c.Symbol, SecType: c.SecType, Exchange: c.Exchange, Currency: c.Currency,
		Expiry: c.Expiry, Right: c.Right, Strike: c.Strike, Multiplier: multiplierToString(c.Multiplier), ComboLegs: c.ComboLegs,
		Action: order.Action, TotalQty: order.TotalQty, OrderType: order.OrderType, LmtPrice: order.LmtPrice, LmtPriceSet: true, TIF: order.TIF,
		BondRules: c.BondRules}
	if err := ValidateOrder(probe); err != nil {
		return nil, nil, err
	}
	return &c, order, nil
}

// validateBondOrder is ValidateOrder's BOND branch: a limit order for a
// positive contract id and one currency, DAY only, with no option or combo
// fields and a quantity and price on the line's grid.
func validateBondOrder(order *IBKROrder) error {
	switch {
	case order.ConID <= 0:
		return fmt.Errorf("bond orders require a positive contract id")
	case !bondCurrencyCode(strings.ToUpper(strings.TrimSpace(order.Currency))):
		return fmt.Errorf("bond orders require a three-letter currency")
	case !strings.EqualFold(strings.TrimSpace(order.OrderType), "LMT"):
		return fmt.Errorf("bond orders must be limit orders, not %q", order.OrderType)
	case order.TIF != "" && !strings.EqualFold(strings.TrimSpace(order.TIF), "DAY"):
		return fmt.Errorf("bond orders must be DAY orders, not %q", order.TIF)
	case order.Expiry != "" || order.Right != "" || order.Strike != 0 || order.Multiplier != "" || len(order.ComboLegs) != 0:
		return fmt.Errorf("bond orders carry no expiry, right, strike, multiplier or combo legs")
	case order.AuxPrice != 0 || order.TrailStopPrice != 0 || order.TrailingPercent != 0 || order.LmtPriceOffset != 0:
		return fmt.Errorf("bond limit orders carry no stop or trail prices")
	case order.BondRules == nil:
		return fmt.Errorf("bond orders require the line's size and price rules from its contract details")
	}
	if err := order.BondRules.CheckQuantity(order.TotalQty); err != nil {
		return err
	}
	return order.BondRules.CheckPrice(order.LmtPrice)
}

func cloneBondOrderRules(in *BondOrderRules) *BondOrderRules {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func bondPositiveFinite(v float64) bool {
	return v > 0 && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func bondWholeUnits(v float64) bool {
	return math.Abs(v-math.Round(v)) <= 1e-9 && v < math.MaxInt32
}

func formatBondSize(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// TradingWindow is one interval a line trades in: open inclusive, close
// exclusive, in UTC.
type TradingWindow struct {
	Open  time.Time
	Close time.Time
}

// Bond session sources: which of the frame's hour lists the windows came
// from.
const (
	BondHoursLiquid  = "liquid_hours"
	BondHoursTrading = "trading_hours"
)

// SessionWindows reads the line's liquid hours, else its trading hours, in
// the line's time zone. ok is false when a complete frame carried neither,
// the zone is unknown, or the list does not parse; a list that says CLOSED
// for every day it names parses to no windows.
func (d BondContractDetails) SessionWindows() (windows []TradingWindow, source string, ok bool) {
	if !d.Complete || strings.TrimSpace(d.TimeZoneID) == "" {
		return nil, "", false
	}
	for _, candidate := range []struct{ source, hours string }{{BondHoursLiquid, d.LiquidHours}, {BondHoursTrading, d.TradingHours}} {
		if strings.TrimSpace(candidate.hours) == "" {
			continue
		}
		windows, err := ParseTradingHours(d.TimeZoneID, candidate.hours)
		if err != nil {
			return nil, "", false
		}
		return windows, candidate.source, true
	}
	return nil, "", false
}

// ParseTradingHours reads IBKR's hours list in zone tz: segments separated
// by ";", each "YYYYMMDD:CLOSED", "YYYYMMDD:HHMM-HHMM[,HHMM-HHMM]" or
// "YYYYMMDD:HHMM-YYYYMMDD:HHMM". A close at or before its open without a
// date of its own falls on the next day. Any segment that does not parse
// refuses the whole list.
func ParseTradingHours(tz, hours string) ([]TradingWindow, error) {
	loc, err := time.LoadLocation(strings.TrimSpace(tz))
	if err != nil {
		return nil, fmt.Errorf("trading hours time zone %q: %w", tz, err)
	}
	var out []TradingWindow
	for segment := range strings.SplitSeq(hours, ";") {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		date, rest, found := strings.Cut(segment, ":")
		if !found {
			return nil, fmt.Errorf("trading hours segment %q has no date", segment)
		}
		day, err := time.ParseInLocation("20060102", date, loc)
		if err != nil {
			return nil, fmt.Errorf("trading hours segment %q: %w", segment, err)
		}
		if strings.EqualFold(strings.TrimSpace(rest), "CLOSED") {
			continue
		}
		for span := range strings.SplitSeq(rest, ",") {
			from, to, found := strings.Cut(strings.TrimSpace(span), "-")
			if !found {
				return nil, fmt.Errorf("trading hours span %q has no end", span)
			}
			open, err := tradingHoursClock(day, from, loc)
			if err != nil {
				return nil, err
			}
			closeDay := day
			d, t, dated := strings.Cut(to, ":")
			if dated {
				if closeDay, err = time.ParseInLocation("20060102", d, loc); err != nil {
					return nil, fmt.Errorf("trading hours span %q: %w", span, err)
				}
				to = t
			}
			closeAt, err := tradingHoursClock(closeDay, to, loc)
			if err != nil {
				return nil, err
			}
			if !closeAt.After(open) && !dated {
				closeAt = closeAt.AddDate(0, 0, 1)
			}
			if !closeAt.After(open) {
				return nil, fmt.Errorf("trading hours span %q closes before it opens", span)
			}
			out = append(out, TradingWindow{Open: open.UTC(), Close: closeAt.UTC()})
		}
	}
	return out, nil
}

func tradingHoursClock(day time.Time, hhmm string, loc *time.Location) (time.Time, error) {
	hhmm = strings.TrimSpace(hhmm)
	if len(hhmm) != 4 {
		return time.Time{}, fmt.Errorf("trading hours time %q is not HHMM", hhmm)
	}
	h, errH := strconv.Atoi(hhmm[:2])
	m, errM := strconv.Atoi(hhmm[2:])
	if errH != nil || errM != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return time.Time{}, fmt.Errorf("trading hours time %q is not HHMM", hhmm)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc), nil
}
