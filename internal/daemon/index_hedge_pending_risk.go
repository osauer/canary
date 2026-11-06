package daemon

import (
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
)

// indexHedgeProjectedRiskRefusal measures the held book, every remaining
// broker hedge sale and the new sale together. The caller owns the complete
// account inventory and its binding through the first broker byte. Pending
// purchases never release hedge capacity before positions prove the close.
// A sale that passes is no risk addition for the drawdown brake and the mandate
// gate (riskAddingOrder reads the same rule); one that fails is refused here.
func indexHedgeProjectedRiskRefusal(input *mandate.Input, draft rpc.OrderDraft, pending []mandate.Leg) *rpc.TradingBlocker {
	refuse := func(code, message string) *rpc.TradingBlocker {
		return &rpc.TradingBlocker{Code: code, Message: message, Action: "Wait for working hedges to finish or cancel them, then preview the hedge again for the current book; nothing was sent."}
	}
	unknown := func(why string) *rpc.TradingBlocker {
		return refuse("index_hedge_risk_unmeasured", "The held book with working hedge sales cannot be measured: "+why)
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
		return refuse(bar.code, "The held book with working hedge sales and this sale would "+bar.leaves+".")
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
