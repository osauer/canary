package twssim

import (
	"fmt"
	"math"
)

// Combination (BAG) orders. A BAG is quoted from its legs: a BUY's net ask
// is what its bought legs ask less what its sold legs bid, per unit of the
// combination (ratio contracts per leg), and the net bid the reverse. A BUY
// limit at or above the net ask fills whole at it. The fill is reported as
// one execution per leg under the BAG's order id (each leg's contract, side,
// units x ratio contracts and price); TWS's exact combination execution
// shape is assumed. Positions move per leg; the commission is 0.65 per
// contract on every leg, at least feeMin.

// BAGConID is the contract id the simulator gives every combination, as IBKR
// gives USD combinations one shared id.
const BAGConID = 28812380

// bagSign is +1 for a leg the combination buys when the order buys.
func bagSign(orderAction, legAction string) float64 {
	s := 1.0
	if legAction == "SELL" {
		s = -1
	}
	if orderAction == "SELL" {
		s = -s
	}
	return s
}

// bagInstrumentLocked is a combination's contract, priced from its legs; it
// fails when a leg is not a listed option or stock.
func (b *Book) bagInstrumentLocked(o PlaceOrder) (Instrument, error) {
	in := Instrument{ConID: BAGConID, Symbol: o.Symbol, SecType: "BAG", Exchange: "SMART", Currency: o.Currency, LocalSymbol: o.Symbol, TradingClass: o.Symbol, MinTick: 0.01, Multiplier: 100}
	if len(o.Legs) == 0 {
		return in, fmt.Errorf("twssim: a BAG without legs")
	}
	bid, ask := 0.0, 0.0
	for _, leg := range o.Legs {
		l := b.instr[leg.ConID]
		if l == nil {
			return in, fmt.Errorf("twssim: combo leg %d is not listed", leg.ConID)
		}
		q := quoteOf(*l)
		lb, la := q.bid, q.ask
		if lb <= 0 || la <= 0 {
			lb, la = l.Price, l.Price
		}
		r := float64(max(leg.Ratio, 1))
		if leg.Action == "BUY" {
			ask += la * r
			bid += lb * r
		} else {
			ask -= lb * r
			bid -= la * r
		}
	}
	in.Bid, in.Ask = roundTo(bid, 0.01), roundTo(ask, 0.01)
	in.Price = roundTo((bid+ask)/2, 0.01)
	return in, nil
}

// bagWhatIfLocked is a combination's margin and commission preview: a
// debit combination margins its net premium.
func (b *Book) bagWhatIfLocked(o PlaceOrder, in Instrument) whatIf {
	f := b.figuresLocked()
	price := o.LimitPrice
	if price == 0 {
		price = in.Price
	}
	contracts := 0.0
	for _, leg := range o.Legs {
		contracts += o.Quantity * float64(max(leg.Ratio, 1))
	}
	fee := math.Round(max(b.feeMin, contracts*0.65)*100) / 100
	debit := math.Max(0, price) * o.Quantity * 100
	return whatIf{
		initBefore: f.initMargin, maintBefore: f.maintMargn, equityBefore: f.nlv,
		initAfter: f.initMargin + debit*marginRate, maintAfter: f.maintMargn + debit*marginRate, equityAfter: f.nlv - fee,
		commission: fee, commissionCcy: in.Currency, marginCcy: b.base,
	}
}

// fillBagLocked fills a combination's remaining units: each leg at its
// touch, positions and cash per leg.
func (b *Book) fillBagLocked(order *Order, units float64) []Execution {
	if units <= 0 || units > order.Remaining() {
		units = order.Remaining()
	}
	var out []Execution
	net := 0.0
	for _, leg := range order.Legs {
		l := *b.instr[leg.ConID]
		q := quoteOf(l)
		sign := bagSign(order.Action, leg.Action)
		price := q.ask
		if sign < 0 {
			price = q.bid
		}
		if price <= 0 {
			price = l.Price
		}
		qty := units * float64(max(leg.Ratio, 1))
		legOrder := &Order{PlaceOrder: PlaceOrder{OrderID: order.OrderID, ClientID: order.ClientID, ConID: leg.ConID, Action: map[bool]string{true: "BUY", false: "SELL"}[sign > 0], Quantity: qty, OrderRef: order.OrderRef}, PermID: order.PermID}
		ex := b.fillLocked(legOrder, l, qty, price, b.clock())
		net += sign * price * float64(max(leg.Ratio, 1))
		out = append(out, *ex)
	}
	order.AvgPrice = (order.AvgPrice*order.Filled + net*units) / (order.Filled + units)
	order.Filled += units
	if order.Remaining() == 0 {
		order.Status = "Filled"
	}
	return out
}
