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
// This check supplements the ordinary gates; it does not exempt the order
// from the drawdown brake, owner stop or sleeve allocation.
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
	if after.Stresses.IndexLossBase < -mandateReductionEpsilon || after.Stresses.IndexLossBase >= before.Stresses.IndexLossBase-mandateReductionEpsilon {
		return refuse("index_hedge_past_neutral", "The held book with working hedge sales and this sale would pass neutral or would not reduce the loss in the index fall.")
	}
	if after.Loss.TotalBase > after.Loss.RoomBase+mandateReductionEpsilon {
		return refuse("index_hedge_over_loss_room", fmt.Sprintf("The held book with working hedge sales and this sale would measure %s of loss, above the mandate's loss room %s.", mandateMoney(after.Loss.TotalBase, input.BaseCurrency), mandateMoney(after.Loss.RoomBase, input.BaseCurrency)))
	}
	// A breach the held book already carries is no reason to refuse a hedge
	// that lessens it: the index gap nets the sale against the funds it
	// hedges (owner decision "1. A", 2026-10-30 09:41 CET).
	if u, added := mandateGapBreachAdded(&before, after); added {
		return refuse("index_hedge_gap", "The held book with working hedge sales and this sale would breach the mandate's gap cap for "+u.Underlying+".")
	}
	return nil
}
