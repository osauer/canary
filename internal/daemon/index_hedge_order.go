package daemon

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The index hedge's order path (owner decisions 2026-10-28 13:17 and 20:07
// CET; internal-docs/design/index-hedge.md). A future reaches the broker in
// two shapes only, both through the one gated, device-confirmed path every
// order takes (preview, WhatIf, signed token, admission, wire guard):
//
//   - the hedge: a SELL of the Micro E-mini S&P 500 future the reduce plan
//     sized, previewed by the daemon with hedgeOrderSource, a limit at the
//     bid, good for the day, in US regular hours, with no stop order; it
//     may leave the contract at most [mandate.index_hedge] max_contracts
//     short, and is refused while that table is not written;
//   - its close: a BUY that closes or shrinks a held short of that future,
//     a percent reduce of the held line (`proposals reduce --con-id ID
//     --include-hedges`), priced as every reduce is.
//
// Every other future order is refused. What bounds cost and risk once a
// hedge sale passes the order cap (whose face-value notional a single Micro
// contract can exceed): max_contracts on the contract's position after the
// order, read at preview, admission and the wire guard; at submit the whole
// book's short Micro S&P contracts with Canary's working hedge sales; the
// size to neutral on the kept mandate book; a limit at the bid, never at
// market; DAY in regular hours only; the broker WhatIf; the owner's device
// confirmation of the exact terms; and the mandate gate, which judges any
// hedge that does not lower the fall's loss within the loss room as an
// addition. Its worst case is max_contracts sold short at the bid: a 10%
// rise then costs 10% of that face value.

// hedgeOrderSource tags a hedge sale's draft; the token path and the place
// boundary read it to apply the hedge's own checks.
const hedgeOrderSource = "index_hedge"

// isIndexFutureContract reports a Micro E-mini S&P 500 future in USD, the
// one future Canary trades.
func isIndexFutureContract(c rpc.ContractParams) bool {
	return strings.EqualFold(strings.TrimSpace(c.SecType), "FUT") && normSym(c.Symbol) == hedgeFutureSymbol && normCcy(c.Currency) == "USD"
}

// normalizePreviewFutureContract holds a future preview to an exact listed
// contract: its ConID, month, exchange and multiplier.
func normalizePreviewFutureContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	out := rpc.ContractParams{ConID: in.ConID, Symbol: normSym(in.Symbol), SecType: "FUT", Exchange: strings.ToUpper(strings.TrimSpace(in.Exchange)),
		Currency: normCcy(in.Currency), Expiry: strings.TrimSpace(in.Expiry), LocalSymbol: strings.TrimSpace(in.LocalSymbol),
		TradingClass: strings.TrimSpace(in.TradingClass), Multiplier: in.Multiplier}
	switch {
	case out.ConID <= 0 || out.Expiry == "" || out.Exchange == "" || out.Exchange == "SMART":
		return rpc.ContractParams{}, errBadRequest("a future preview names its exact contract: con_id, expiry and its exchange")
	case out.Multiplier <= 0:
		return rpc.ContractParams{}, errBadRequest("a future preview carries the contract's multiplier")
	case !isIndexFutureContract(out):
		return rpc.ContractParams{}, errBadRequest("the only future Canary trades is the Micro E-mini S&P 500 (MES, USD) index hedge")
	case in.Right != "" || in.Strike != 0:
		return rpc.ContractParams{}, errBadRequest("a future preview carries no right or strike")
	}
	return out, nil
}

// indexFutureSessionRefusal refuses a future order outside the US regular
// session (owner decision 2026-10-28 20:07 CET: the hedge is placed in US
// hours), and when the calendar cannot say: the futures trade nearly around
// the clock, so no quote would refuse it later.
func (s *Server) indexFutureSessionRefusal(now time.Time) error {
	session, ok := s.previewSession(marketcal.MarketUSEquity, now)
	message := ""
	switch {
	case !ok || session.State == marketcal.StateUnknown:
		message = "the index hedge trades in US regular hours only, and the official calendar cannot confirm the session now"
	case !session.IsOpen:
		message = "the index hedge trades in US regular hours only: " + sessionClosedPhrase(session)
	default:
		return nil
	}
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{Code: "outside_regular_hours", Message: message,
		Action: "Preview the hedge again in US regular hours; nothing was sent."})
}

// indexFuturePreviewRefusal holds a future preview to the two shapes the
// path admits. The order gate (indexFutureOrderRule) holds the sizes.
func indexFuturePreviewRefusal(p rpc.OrderPreviewParams, scope, action, orderType, tif string, position rpc.OrderPositionImpact) error {
	allowance := p.Allowance != nil && strings.EqualFold(strings.TrimSpace(p.Strategy), rpc.OrderStrategyAllowanceLimit)
	switch {
	case scope != rpc.OrderTokenScopePlace:
		return errBadRequest("a future order is placed new; it is never modified")
	case orderType != rpc.OrderTypeLMT || tif != rpc.OrderTIFDay || p.OutsideRTH || p.Trail != nil || p.AttachedStop != nil || p.AdaptivePriority != "":
		return errBadRequest("a future order is a plain limit, good for the day, in regular hours, with no stop")
	case !allowance || p.LimitPrice != nil:
		return errBadRequest("a future order is priced by the daemon from the touch; it takes no limit price")
	}
	switch strings.TrimSpace(p.Source) {
	case hedgeOrderSource:
		if action != rpc.OrderActionSell || p.Allowance.Pct != 0 {
			return errBadRequest("the index hedge sells at the bid")
		}
	case reduceOrderSource:
		if action != rpc.OrderActionBuy || !(position.Before < 0) {
			return errBadRequest("a future reduce only buys back a held short hedge")
		}
	default:
		return errBadRequest("a future is traded only as the index hedge (`proposals reduce --portfolio --mode hedge`) or its close (`proposals reduce --con-id ID --include-hedges`)")
	}
	return nil
}

// indexFutureOrderRule is the order gate's rule for a future, read at
// preview, admission and the wire guard with the position read there: a
// hedge sale may leave the contract at most max_contracts short and needs
// the table; a buy only closes or shrinks a held short. A future that
// passes is exempt from the notional cap (index_hedge_order.go's header
// says what bounds it instead).
func indexFutureOrderRule(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, position rpc.OrderPositionImpact) error {
	if !isIndexFutureContract(draft.Contract) || draft.StrategyGroup != nil {
		return fmt.Errorf("the only future Canary trades is the Micro E-mini S&P 500 index hedge")
	}
	qty := float64(draft.Quantity)
	switch strings.ToUpper(strings.TrimSpace(draft.Action)) {
	case rpc.OrderActionSell:
		switch {
		case draft.Source != hedgeOrderSource:
			return fmt.Errorf("a future sale is placed only as the index hedge")
		case limits.IndexHedgeRefusal != "" || limits.IndexHedgeMaxContracts < 1:
			return fmt.Errorf("%s", nonEmptyString(limits.IndexHedgeRefusal, "placing an index hedge is refused until [mandate.index_hedge] max_contracts is written"))
		case position.Before > 0 || math.Abs(position.After-(position.Before-qty)) > 1e-9:
			return fmt.Errorf("the index hedge sells from no position or a held short, never from a long future")
		case -position.After > float64(limits.IndexHedgeMaxContracts)+1e-9:
			return fmt.Errorf("the hedge would leave %s short %g contracts, above [mandate.index_hedge] max_contracts %d", draft.Contract.LocalSymbol, -position.After, limits.IndexHedgeMaxContracts)
		}
	case rpc.OrderActionBuy:
		if !(position.Before < 0) || position.After > 1e-9 || math.Abs(position.After-(position.Before+qty)) > 1e-9 {
			return fmt.Errorf("a future buy only closes or shrinks a held short hedge")
		}
	default:
		return fmt.Errorf("action must be buy or sell")
	}
	return nil
}

// heldIndexHedge reports a held short of the index future, the line a
// single reduce may buy back.
func heldIndexHedge(row rpc.PositionView) bool {
	return positionIsIndexFuture(row) && row.Quantity < 0
}

// reduceSingleEligible is reduceEligible plus the held hedge: a percent
// reduce of one line may buy the hedge back; a portfolio sweep never
// selects it (closing a hedge is no risk cut: TM-05.18).
func reduceSingleEligible(row rpc.PositionView) bool {
	return reduceEligible(row) || heldIndexHedge(row)
}

// indexHedgeShortHeld is the whole book's short Micro S&P contracts, every
// month together.
func indexHedgeShortHeld(pos *rpc.PositionsResult) float64 {
	if pos == nil {
		return 0
	}
	short := 0.0
	for _, row := range pos.Stocks {
		if heldIndexHedge(row) {
			short -= row.Quantity
		}
	}
	return short
}

// indexHedgeCapRefusal is the submit's whole-book check: the book's short
// Micro S&P contracts, Canary's working hedge sales of this contract and
// the reviewed sale together stay within max_contracts. The order gate
// holds the contract's own position at every stage; this adds the other
// months and the working orders.
func (s *Server) indexHedgeCapRefusal(draft rpc.OrderDraft, heldShort float64) []rpc.TradingBlocker {
	var mandateTable *risk.ConstitutionMandate
	if c := s.riskPolicies.snapshot().policy; c != nil {
		mandateTable = c.Mandate
	}
	maxContracts, why := mandateTable.IndexHedgeMaxContracts()
	if why != "" {
		return []rpc.TradingBlocker{{Code: "index_hedge_unplaceable", Message: why, Action: "Write [mandate.index_hedge] max_contracts with a higher policy_version to place a hedge; nothing was sent."}}
	}
	committed, blockers := s.reduceCommittedSales(draft.Contract, rpc.OrderActionSell)
	if len(blockers) > 0 {
		return blockers
	}
	if total := heldShort + committed + float64(draft.Quantity); total > float64(maxContracts)+1e-9 {
		return []rpc.TradingBlocker{{Code: "index_hedge_over_max", Message: fmt.Sprintf("the book would hold %g Micro S&P contracts short (%g held, %g in Canary's working sales, %d reviewed), above [mandate.index_hedge] max_contracts %d",
			total, heldShort, committed, draft.Quantity, maxContracts), Action: "Preview the hedge again for the current book; nothing was sent."}}
	}
	return nil
}
