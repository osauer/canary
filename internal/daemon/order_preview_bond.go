package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// BOND order previews (internal-docs/design/cash-sweep.md, Phase B order
// path). A bond is previewed only for a cash_sweep row: the proposal engine
// sets the daemon-internal OrderPreviewParams.Bond with the bill's
// conventions, so no RPC caller can preview one. The preview reads the
// line's grid and hours from its contract details on the preview's own
// broker session, refuses a closed session before any quote, prices a
// patient limit on the line's minimum tick from a live two-sided quote,
// values the order at face × price / 100, builds the order through
// ibkrlib.NewBondLimitOrder (which refuses anything off the grid) and then
// runs WhatIf and every gate an ordinary preview runs.

// previewBondOrderInvalidCode refuses a bond order the line's grid refuses.
const previewBondOrderInvalidCode = "bond_order_invalid"

// normalizePreviewBondContract keeps a BILL or BOND contract's identity: its
// type, a positive contract id, a three-letter currency, SMART unless named,
// no option or multiplier fields.
func normalizePreviewBondContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	out := rpc.ContractParams{ConID: in.ConID, Symbol: strings.ToUpper(strings.TrimSpace(in.Symbol)), SecType: ibkrlib.BillOrBondSecType(in.SecType),
		Exchange: strings.ToUpper(strings.TrimSpace(in.Exchange)), Currency: normCcy(in.Currency)}
	switch {
	case out.ConID <= 0:
		return rpc.ContractParams{}, errBadRequest("a BOND preview needs the line's contract id")
	case len(out.Currency) != 3:
		return rpc.ContractParams{}, errBadRequest("a BOND preview needs a three-letter currency")
	case in.Expiry != "" || in.Right != "" || in.Strike != 0 || in.Multiplier > 1:
		return rpc.ContractParams{}, errBadRequest("a BOND preview carries no expiry, right, strike or multiplier")
	}
	if out.Exchange == "" {
		out.Exchange = "SMART"
	}
	return out, nil
}

// validatePreviewBondParams admits a BOND preview only in the one shape a
// cash_sweep row asks for: the proposal engine's bill terms, a new LMT DAY
// order in regular hours priced as a patient limit.
func validatePreviewBondParams(p rpc.OrderPreviewParams, replace bool) error {
	switch {
	case p.Bond == nil:
		return errBadRequest("order preview admits BOND only for a cash_sweep proposal row; preview it with `canary proposals preview`")
	case replace:
		return errBadRequest("a working BOND order cannot be modified through preview; cancel it and preview the row again")
	case p.Bond.FacePerUnit <= 0 || strings.TrimSpace(p.Bond.Instrument) == "":
		return errBadRequest("the BOND preview's bill terms name no instrument or face per unit")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.OrderType), rpc.OrderTypeLMT), rpc.OrderTypeLMT):
		return errBadRequest("a BOND preview is a limit order")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.TIF), rpc.OrderTIFDay), rpc.OrderTIFDay):
		return errBadRequest("a BOND preview is a DAY order")
	case p.OutsideRTH || p.Trail != nil || p.LimitPrice != nil || p.Bounded != nil || p.ResolvedStrategy != nil:
		return errBadRequest("a BOND preview is a patient limit in the bill's session: no limit, trail, bound or outside_rth")
	case p.Strategy != "" && !strings.EqualFold(p.Strategy, rpc.OrderStrategyPatientLimit):
		return errBadRequest("a BOND preview is priced as a patient limit")
	}
	return nil
}

// resolvePreviewBondContract reads the line's contract details by contract
// id on the preview's broker session (the test seam replaces the read),
// checks the line is still the vocabulary bill the row names, and returns
// the contract with its minimum tick, the terms with the line's grid, the
// grid itself and the line's session.
func (s *Server) resolvePreviewBondContract(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, terms rpc.OrderBondTerms, timeout time.Duration) (rpc.ContractParams, rpc.OrderBondTerms, ibkrlib.BondOrderRules, *rpc.BondSession, error) {
	fail := func(err error) (rpc.ContractParams, rpc.OrderBondTerms, ibkrlib.BondOrderRules, *rpc.BondSession, error) {
		return rpc.ContractParams{}, rpc.OrderBondTerms{}, ibkrlib.BondOrderRules{}, nil, err
	}
	var lines []ibkrlib.BondContractDetails
	var err error
	switch {
	case s.orderBondDetailsForTest != nil:
		lines, err = s.orderBondDetailsForTest(ctx, contract.ConID, contract.Currency)
	case authority != nil:
		lines, err = authority.connector.BondContractDetailsForSession(ctx, authority.session,
			ibkrlib.BondContractRequest{ConID: contract.ConID, Currency: contract.Currency, Exchange: contract.Exchange,
				SecTypes: cashSweepHeldSecTypes(contract.SecType, terms.Instrument)}, timeout)
	default:
		return fail(fmt.Errorf("%w: bond contract details need the broker session", ErrTradingDisabled))
	}
	if err != nil {
		return fail(fmt.Errorf("%w: bond contract details: %s", ErrTradingDisabled, bondLookupReason(err)))
	}
	line, _, err := bondLineFor(lines, contract.Currency, contract.ConID)
	if err != nil {
		return fail(fmt.Errorf("%w: bond contract details: %v", ErrTradingDisabled, err))
	}
	requestBound := terms.ResolutionSource == cashSweepResolutionRequestBound
	if requestBound {
		var exactLines []ibkrlib.BondContractDetails
		if authority == nil {
			return fail(fmt.Errorf("%w: request-bound bill identity needs the preview broker session", ErrTradingDisabled))
		}
		exactLines, err = authority.connector.BondContractDetailsForSession(ctx, authority.session,
			ibkrlib.BondContractRequest{IDType: ibkrlib.BondIdentifierISIN, ID: terms.ISIN, Currency: contract.Currency,
				SecTypes: cashSweepInstrumentSecTypes(terms.Instrument)}, timeout)
		if err != nil {
			return fail(fmt.Errorf("%w: exact bill identity cannot be re-read", ErrTradingDisabled))
		}
		exactLine, _, err := bondLineFor(exactLines, contract.Currency, 0)
		if err != nil || exactLine.ConID != contract.ConID {
			return fail(fmt.Errorf("%w: exact bill identity changed", ErrTradingDisabled))
		}
		if err := cashSweepCheckGermanPreview(ctx, serverBillSource{s}, exactLines, exactLine, terms, s.orderNow()); err != nil {
			return fail(fmt.Errorf("%w: %v", ErrTradingDisabled, err))
		}
		if err := cashSweepCheckGermanPreview(ctx, serverBillSource{s}, lines, line, terms, s.orderNow()); err != nil {
			return fail(fmt.Errorf("%w: held contract details contradict the exact bill: %v", ErrTradingDisabled, err))
		}
	}
	if instrument := bondLineInstrument(line); !requestBound && instrument != terms.Instrument {
		return fail(fmt.Errorf("%w: contract %d is not a %s bill (its details read %q)", ErrTradingDisabled, contract.ConID, terms.Instrument, nonEmptyString(instrument, bondClassOf(line))))
	}
	if !requestBound {
		if err := cashSweepCheckReviewedMaturity(serverBillSource{s}, lines, line, terms, s.orderNow()); err != nil {
			return fail(fmt.Errorf("%w: bill maturity identity: %v", ErrTradingDisabled, err))
		}
	}
	rules, err := ibkrlib.BondOrderRulesFrom(line)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrTradingDisabled, err))
	}
	if authority != nil && !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return fail(fmt.Errorf("%w: broker session changed during bond contract details", ErrTradingDisabled))
	}
	contract.Symbol = nonEmptyString(contract.Symbol, strings.ToUpper(strings.TrimSpace(line.Symbol)))
	contract.MinTick = rules.MinTick
	terms.MinTick, terms.MinSize, terms.SizeIncrement = rules.MinTick, rules.MinSize, rules.SizeIncrement
	return contract, terms, rules, cashSweepBondSession(&line, terms.Instrument, s.orderNow()), nil
}

// bondSessionRefusal refuses, before any quote is requested, a bond preview
// while the line's session is closed. An unknown session (past the last
// window the hours name) leaves the live-quote requirement to decide.
func bondSessionRefusal(session *rpc.BondSession, now time.Time) error {
	sess, ok := bondSessionAt(session, now)
	if !ok || sess.IsOpen {
		return nil
	}
	message := "bond patient-limit requires an open session: " + sessionClosedPhrase(sess)
	return refusePreview(errBadRequest(message), rpc.TradingBlocker{
		Code: previewMarketClosedCode, Message: message,
		Action: "Preview again once the bill's session is open; no quote was requested.",
	})
}

// bondPatientLimitPrice prices a bond patient limit on the line's minimum
// tick from a live, two-sided quote read during this preview: a buy at the
// mid rounded down but never below the bid, a sell at the mid rounded up but
// never above the ask.
func bondPatientLimitPrice(action string, tick float64, quote rpc.OrderQuoteSnapshot) (float64, error) {
	if err := requireFreshPreviewQuote(quote, "bond patient-limit"); err != nil {
		return 0, err
	}
	if !rpc.IsLiveDataType(quote.DataType) {
		return 0, refusePreviewCode(previewQuoteNotLiveCode, errBadRequest("bond patient-limit requires a live bid and ask"))
	}
	if quote.PriceAt.IsZero() {
		return 0, refusePreviewCode(previewQuoteStaleCode, errBadRequest("bond patient-limit requires a bid or ask received during this preview"))
	}
	if quote.Bid == nil || quote.Ask == nil || *quote.Bid <= 0 || *quote.Ask <= *quote.Bid {
		return 0, refusePreviewCode(previewQuoteNotTwoSidedCode, errBadRequest("bond patient-limit requires a positive two-sided bid/ask"))
	}
	if !positiveFinite(tick) {
		return 0, refusePreviewCode(previewContractUnresolvedCode, errBadRequest("the bond line carries no minimum tick"))
	}
	bid, ask := *quote.Bid, *quote.Ask
	mid := (bid + ask) / 2
	switch action {
	case rpc.OrderActionBuy:
		return max(ibkrlib.BondTickFloor(mid, tick), ibkrlib.BondTickNearest(bid, tick)), nil
	case rpc.OrderActionSell:
		return min(ibkrlib.BondTickCeil(mid, tick), ibkrlib.BondTickNearest(ask, tick)), nil
	}
	return 0, errBadRequest("action must be buy or sell")
}

// bondOrderNotional is a bond order's value in its currency: quantity in
// order units × face per unit × price per 100 of face.
func bondOrderNotional(quantity int, terms *rpc.OrderBondTerms, price float64) float64 {
	if terms == nil {
		return 0
	}
	return float64(quantity) * terms.FacePerUnit * price / 100
}

// previewBondRules is the grid a BOND draft carries, for the WhatIf and
// place encoders to re-check.
func previewBondRules(draft rpc.OrderDraft) *ibkrlib.BondOrderRules {
	if draft.Bond == nil || !ibkrlib.IsBillOrBond(draft.Contract.SecType) {
		return nil
	}
	return &ibkrlib.BondOrderRules{MinTick: draft.Bond.MinTick, MinSize: draft.Bond.MinSize, SizeIncrement: draft.Bond.SizeIncrement}
}
