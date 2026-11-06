package daemon

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// hedgeLineSale is a working sale of a line other than the hedge's future:
// quantity units of the contract, or why it cannot be read.
type hedgeLineSale struct {
	conID    int
	symbol   string
	quantity float64
	unread   string
}

// workingLineSale reads one working order as a sale for the hedge's book, or
// reports false when it is no sale now: a buy, or a stop or trail, which sells
// only when the market reaches it and which every protected line carries.
func workingLineSale(conID int, symbol, action, orderType string, quantity float64, trail bool) (hedgeLineSale, bool) {
	sale := hedgeLineSale{conID: conID, symbol: normSym(symbol), quantity: quantity}
	name := nonEmptyString(sale.symbol, "an unnamed contract")
	switch {
	case strings.EqualFold(action, rpc.OrderActionBuy):
		return hedgeLineSale{}, false
	case !strings.EqualFold(action, rpc.OrderActionSell):
		sale.unread = "a working order on " + name + " has no direction"
	case strings.TrimSpace(orderType) == "":
		sale.unread = "a working sale of " + name + " has no order type"
	case trail || !orderTypeFillsNow(orderType):
		return hedgeLineSale{}, false
	case !positiveFinite(quantity):
		sale.unread = "a working sale of " + name + " has no remaining quantity"
	case conID <= 0:
		sale.unread = "a working sale of " + name + " does not name its contract"
	}
	return sale, true
}

// hedgeBookWithWorkingSales is the book the hedge is measured on: the held book
// as it reads once the working sales of its long lines fill, so a cut still
// working cannot leave the short past neutral when it fills (review of Desk
// 70929f97, 2026-11-06). A sale counts its remaining units, the fills being in
// the held quantity already, and at most what the line holds; the units left
// keep the line's own valuation. It counts only when it lowers the 10% fall,
// which adds up line by line: selling a bought put raises the fall, and a hedge
// sized for that stands past neutral until the put sells. So the size is safe
// whichever of the working sales fill; one that lapses leaves the hedge short
// of neutral, and the next plan sells the rest. It also returns how many lines
// it counted, and why a sale that may touch a long line cannot be read: the
// plan says that, and placement refuses. A sale of a line the book does not
// hold long opens or adds to a short; it is no cut and is not counted.
func hedgeBookWithWorkingSales(input mandate.Input, sales []hedgeLineSale) (mandate.Input, int, string) {
	if len(sales) == 0 {
		return input, 0, ""
	}
	held, long := map[string]float64{}, map[string]bool{}
	for _, l := range input.Legs {
		held[l.ID] += l.Quantity
	}
	for _, l := range input.Legs {
		if held[l.ID] > 0 {
			long[strings.ToUpper(l.Underlying)] = true
		}
	}
	sold, unread := map[string]float64{}, ""
	for _, sale := range sales {
		id := mandateConLegID(sale.conID)
		mayTouch := held[id] > 0
		if sale.conID <= 0 {
			mayTouch = sale.symbol == "" || long[sale.symbol]
		}
		switch {
		case !mayTouch:
		case sale.unread != "":
			if unread == "" {
				unread = sale.unread
			}
		default:
			sold[id] += sale.quantity
		}
	}
	base := mandate.Evaluate(input)
	if base.Stresses == nil {
		return input, 0, unread // the caller measures the book and says why it cannot
	}
	book, lines := input, 0
	for _, l := range input.Legs {
		quantity := min(sold[l.ID], held[l.ID])
		if !(quantity > 0) {
			continue
		}
		delete(sold, l.ID) // two rows of one contract close once
		l.Quantity = -quantity
		closed := mandate.Evaluate(mandateInputWithLegs(input, []mandate.Leg{l}))
		switch {
		case closed.Stresses == nil:
			if unread == "" {
				unread = "the mandate cannot measure the book with the working sale of " + l.Symbol
			}
		case closed.Stresses.IndexLossBase < base.Stresses.IndexLossBase-mandateReductionEpsilon:
			book, lines = mandateInputWithLegs(book, []mandate.Leg{l}), lines+1
		}
	}
	return book, lines, unread
}

// indexHedgeProjectedRiskRefusal measures the book as its working sales leave
// it (hedgeBookWithWorkingSales, which the caller applied to input), every
// remaining broker hedge sale and the new sale together. The caller owns the
// complete account inventory and its binding through the first broker byte.
// Pending purchases never release hedge capacity before positions prove the close.
// A sale that passes is no risk addition for the drawdown brake and the mandate
// gate (riskAddingOrder reads the same rule); one that fails is refused here.
func indexHedgeProjectedRiskRefusal(input *mandate.Input, draft rpc.OrderDraft, pending []mandate.Leg) *rpc.TradingBlocker {
	refuse := func(code, message string) *rpc.TradingBlocker {
		return &rpc.TradingBlocker{Code: code, Message: message, Action: "Wait for working hedges to finish or cancel them, then preview the hedge again for the current book; nothing was sent."}
	}
	unknown := func(why string) *rpc.TradingBlocker {
		return refuse("index_hedge_risk_unmeasured", "The book with its working sales cannot be measured: "+why)
	}
	if input == nil || !input.Capital.Known {
		return unknown("the current mandate book or its capital is unavailable")
	}
	if !isIndexFutureContract(draft.Contract) || !strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		return unknown("the reviewed order is not an index hedge sale")
	}
	for _, leg := range pending {
		if !mandate.IsIndexFuture(leg) || !(leg.Quantity < 0) {
			return unknown("working hedge sales have incomplete terms")
		}
	}
	leg, ok := mandateOrderLeg(draft, input)
	if !ok {
		return unknown("the reviewed hedge sale has incomplete terms")
	}
	beforeInput := mandateInputWithLegs(*input, pending)
	before := mandate.Evaluate(beforeInput)
	after := mandate.Evaluate(mandateInputWithLegs(beforeInput, []mandate.Leg{leg}))
	for _, v := range []mandate.Verdict{before, after} {
		if v.Stresses == nil || v.Loss == nil || v.Status == mandate.StatusUnknown {
			why := "the mandate cannot measure the projected book"
			if len(v.Reasons) > 0 {
				why = v.Reasons[0]
			}
			return unknown(why)
		}
	}
	if bar := indexHedgeSaleRefusal(&before, after, input.BaseCurrency); bar != nil {
		return refuse(bar.code, "The book with its working sales and this sale would "+bar.leaves+".")
	}
	return nil
}

// hedgeSaleBar is why a hedge sale is refused: its code, and what it would do
// to the book, in words that follow "would".
type hedgeSaleBar struct{ code, leaves string }

// indexHedgeSaleRefusal is the one rule a hedge sale answers to, at the plan,
// the gate classification and placement (owner, 2026-11-06 14:23 CET: the hedge
// stays open at the maximum loss, when the book needs it most). The sale must
// lower the loss in the 10% fall without passing neutral, and must not turn the
// 10% rise into a loss or deepen one: what the book holds pays for the hedge
// when the index rises, and once it no longer does the short is a bet. Neither
// the loss room nor the gap cap reads here: the hedge's rise loss still counts
// in the loss measure and its index gap in the verdict, so a hedged book reads
// reduce until the hedge is bought back, but a limit at zero room would refuse
// every hedge, and a book of single-name calls has nothing in the index group
// to net the hedge against. What bounds it instead is at the head of
// index_hedge_order.go.
func indexHedgeSaleRefusal(before *mandate.Verdict, after mandate.Verdict, ccy string) *hedgeSaleBar {
	fall, rise := after.Stresses.IndexLossBase, after.Stresses.IndexRiseLossBase
	switch {
	case fall < -mandateReductionEpsilon || fall >= before.Stresses.IndexLossBase-mandateReductionEpsilon:
		return &hedgeSaleBar{"index_hedge_past_neutral", "pass neutral or not reduce the loss in the index fall"}
	case rise > max(0, before.Stresses.IndexRiseLossBase)+mandateReductionEpsilon:
		return &hedgeSaleBar{"index_hedge_rise_loss", fmt.Sprintf("lose %s in a %g%% index rise", mandateMoney(rise, ccy), mandate.IndexShockPct)}
	}
	return nil
}
