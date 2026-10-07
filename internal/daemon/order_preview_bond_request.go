package daemon

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bond orders named by identifier (internal-docs/design/bond-orders.md). The
// owner names a bill or bond by ISIN or CUSIP and a face amount. The daemon
// resolves the line on the preview's broker session, admits a buy only on
// issuer evidence (bond_evidence.go), derives the order quantity from the
// face and the currency's unit, and builds the terms a cash_sweep row
// carries plus that evidence. From there the preview is the cash sweep's
// bond path: the line's session, a patient limit on its tick, its grid, the
// order cap, WhatIf and the token.

const (
	// bondRequestMinDays refuses a buy of a line maturing so soon that its
	// settlement and its redemption overlap.
	bondRequestMinDays = 7
	// bondEvidenceWait bounds the issuer sources one preview waits for.
	bondEvidenceWait = 8 * time.Second

	previewBondIssuerUnprovenCode = "bond_issuer_unproven"
	previewBondUnitMismatchCode   = "bond_unit_mismatch"
)

// previewBondRequest is a validated BondOrder.
type previewBondRequest struct {
	identifier string
	idType     string
	face       float64
	secType    string
	currency   string
}

// validatePreviewBondRequest admits a BondOrder only in the one shape the
// cash sweep's bond path prices: a new LMT DAY order, priced as a patient
// limit in the line's session, named by identifier and face alone.
func validatePreviewBondRequest(p rpc.OrderPreviewParams, replace bool) (previewBondRequest, error) {
	r := p.BondOrder
	req := previewBondRequest{identifier: strings.ToUpper(strings.TrimSpace(r.Identifier)), face: r.Face,
		secType: strings.ToUpper(strings.TrimSpace(p.Contract.SecType)), currency: normCcy(p.Contract.Currency)}
	switch {
	case ibkrlib.ValidISIN(req.identifier):
		req.idType = ibkrlib.BondIdentifierISIN
	case ibkrlib.ValidCUSIP(req.identifier):
		req.idType = ibkrlib.BondIdentifierCUSIP
	default:
		return previewBondRequest{}, errBadRequest(fmt.Sprintf("%q is not a valid ISIN or CUSIP", req.identifier))
	}
	switch {
	case replace:
		return previewBondRequest{}, errBadRequest("a working bond order cannot be modified through preview; cancel it and preview again")
	case req.secType != ibkrlib.SecTypeBond && req.secType != ibkrlib.SecTypeBill:
		return previewBondRequest{}, errBadRequest("a bond order's security type is BOND or BILL")
	case !positiveFinite(r.Face):
		return previewBondRequest{}, errBadRequest("a bond order's face amount must be positive")
	case p.Quantity != 0:
		return previewBondRequest{}, errBadRequest("a bond order names its face amount; the daemon derives the quantity")
	case len(req.currency) != 3:
		return previewBondRequest{}, errBadRequest("a bond order needs the bond's currency")
	case p.Contract.ConID != 0 || p.Contract.Expiry != "" || p.Contract.Right != "" || p.Contract.Strike != 0 || p.Contract.Multiplier > 1:
		return previewBondRequest{}, errBadRequest("a bond order names the bond by identifier only")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.OrderType), rpc.OrderTypeLMT), rpc.OrderTypeLMT):
		return previewBondRequest{}, errBadRequest("a bond order is a limit order")
	case !strings.EqualFold(nonEmptyString(strings.TrimSpace(p.TIF), rpc.OrderTIFDay), rpc.OrderTIFDay):
		return previewBondRequest{}, errBadRequest("a bond order is a DAY order")
	case p.OutsideRTH || p.Trail != nil || p.LimitPrice != nil || p.Bounded != nil || p.ResolvedStrategy != nil || p.FX != nil || p.Bond != nil:
		return previewBondRequest{}, errBadRequest("a bond order is a patient limit in the bond's session: no limit price, trail or outside_rth")
	case p.Strategy != "" && !strings.EqualFold(p.Strategy, rpc.OrderStrategyPatientLimit):
		return previewBondRequest{}, errBadRequest("a bond order is priced as a patient limit")
	}
	if _, ok := cashSweepBondConvention(req.currency); !ok {
		return previewBondRequest{}, errBadRequest(fmt.Sprintf("Canary knows the order unit of USD, EUR, GBP and CAD bonds only, not %s", req.currency))
	}
	return req, nil
}

// resolvePreviewBondRequest resolves the named line on the preview's broker
// session and returns its contract, the order's terms, the line's grid and
// session, and the order quantity in units. A buy carries the issuer
// evidence that admitted it; a sale needs none, since it only reduces a held
// line (the order risk authority refuses a short).
func (s *Server) resolvePreviewBondRequest(ctx context.Context, authority *orderPreviewBrokerAuthority, action string, req previewBondRequest, timeout time.Duration) (rpc.ContractParams, rpc.OrderBondTerms, ibkrlib.BondOrderRules, *rpc.BondSession, int, error) {
	fail := func(err error) (rpc.ContractParams, rpc.OrderBondTerms, ibkrlib.BondOrderRules, *rpc.BondSession, int, error) {
		return rpc.ContractParams{}, rpc.OrderBondTerms{}, ibkrlib.BondOrderRules{}, nil, 0, err
	}
	secTypes := []string{ibkrlib.SecTypeBond, ibkrlib.SecTypeBill}
	if req.secType == ibkrlib.SecTypeBill {
		secTypes = []string{ibkrlib.SecTypeBill, ibkrlib.SecTypeBond}
	}
	lookup := ibkrlib.BondContractRequest{IDType: req.idType, ID: req.identifier, Currency: req.currency, SecTypes: secTypes}
	var lines []ibkrlib.BondContractDetails
	var err error
	switch {
	case s.orderBondLookupForTest != nil:
		lines, err = s.orderBondLookupForTest(ctx, lookup)
	case authority != nil:
		lines, err = authority.connector.BondContractDetailsForSession(ctx, authority.session, lookup, timeout)
	default:
		return fail(fmt.Errorf("%w: bond contract details need the broker session", ErrTradingDisabled))
	}
	if err != nil {
		return fail(fmt.Errorf("%w: bond contract details for %s: %s", ErrTradingDisabled, req.identifier, bondLookupReason(err)))
	}
	s.bondEvidenceSupport().noteFactorPriced(lines)
	line, _, err := bondLineFor(lines, req.currency, 0)
	if err != nil {
		return fail(fmt.Errorf("%w: bond contract details for %s: %v", ErrTradingDisabled, req.identifier, err))
	}
	if err := bondRequestIdentity(req, line); err != nil {
		return fail(fmt.Errorf("%w: %v", ErrTradingDisabled, err))
	}
	rules, err := ibkrlib.BondOrderRulesFrom(line)
	if err != nil {
		return fail(fmt.Errorf("%w: %v", ErrTradingDisabled, err))
	}
	conv, _ := cashSweepBondConvention(req.currency)
	quantity, err := bondRequestQuantity(req, conv, rules)
	if err != nil {
		return fail(refusePreviewCode(previewBondOrderInvalidCode, errBadRequest(err.Error())))
	}
	terms := rpc.OrderBondTerms{Instrument: rpc.OrderBondInstrumentByIdentifier, QuantityUnit: conv.QuantityUnit, FacePerUnit: conv.FacePerUnit,
		PriceConvention: conv.PriceConvention, ISIN: line.ISIN(), CUSIP: line.CUSIP(),
		MinTick: rules.MinTick, MinSize: rules.MinSize, SizeIncrement: rules.SizeIncrement}
	brokerMaturity, hasBrokerMaturity := line.MaturityDate()
	if hasBrokerMaturity {
		terms.Maturity, terms.MaturitySource = brokerMaturity.Format(time.DateOnly), rpc.CashSweepMaturitySourceBroker
	}
	now := s.orderNow()
	if action == rpc.OrderActionBuy {
		if err := s.admitBondBuy(ctx, req, line, brokerMaturity, hasBrokerMaturity, now, &terms); err != nil {
			return fail(err)
		}
	}
	if authority != nil && !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return fail(fmt.Errorf("%w: broker session changed during bond contract details", ErrTradingDisabled))
	}
	// IBKR's bond frames carry no symbol, and a quote needs one: fall back to
	// the CUSIP, then the ISIN, as a sweep row does.
	symbol := nonEmptyString(strings.ToUpper(strings.TrimSpace(line.Symbol)), nonEmptyString(line.CUSIP(), nonEmptyString(line.ISIN(), req.identifier)))
	contract := rpc.ContractParams{ConID: line.ConID, Symbol: symbol, SecType: ibkrlib.BillOrBondSecType(line.SecType),
		Exchange: "SMART", Currency: req.currency, MinTick: rules.MinTick}
	return contract, terms, rules, cashSweepBondSession(&line, "", now), quantity, nil
}

// admitBondBuy refuses a buy that no source vouches for, that is priced on a
// factored principal, or whose evidence contradicts the request or the
// broker; it writes the evidence and the accrued interest bound into terms.
func (s *Server) admitBondBuy(ctx context.Context, req previewBondRequest, line ibkrlib.BondContractDetails, brokerMaturity time.Time, hasBrokerMaturity bool, now time.Time, terms *rpc.OrderBondTerms) error {
	refuse := func(message, action string) error {
		return refusePreview(errBadRequest(message), rpc.TradingBlocker{Code: previewBondIssuerUnprovenCode, Message: message, Action: action})
	}
	src := s.bondEvidenceSupport()
	if line.FactorPriced || src.factorPricedConID(line.ConID) {
		return refuse(fmt.Sprintf("IBKR prices %s on a factored principal, as an inflation-linked bond; its value cannot be bounded from face and price", req.identifier),
			"Canary buys only nominal bonds with a fixed or zero coupon.")
	}
	if src == nil {
		return refuse("no issuer source is available", "")
	}
	evCtx, cancel := context.WithTimeout(ctx, bondEvidenceWait)
	defer cancel()
	ev, err := src.bondBuyEvidence(evCtx, line, req.identifier, now)
	if err != nil {
		return refuse(fmt.Sprintf("%s cannot be bought: %v", req.identifier, err),
			"Canary buys US Treasuries, UK gilts, Government of Canada bonds and bonds on the ECB's list of eligible assets (government or investment grade).")
	}
	today := cashSweepDay(now)
	switch {
	case ev.Currency != req.currency:
		return refuse(fmt.Sprintf("%s is a %s bond by %s, not %s", req.identifier, ev.Currency, ev.Source, req.currency), "Preview again with the bond's own currency.")
	case line.Coupon != 0 && math.Abs(line.Coupon-ev.Coupon) > 1e-6:
		return refuse(fmt.Sprintf("IBKR's coupon %s for %s differs from %s's %s", strconv.FormatFloat(line.Coupon, 'f', -1, 64), req.identifier, ev.Source, strconv.FormatFloat(ev.Coupon, 'f', -1, 64)), "")
	case hasBrokerMaturity && !brokerMaturity.Equal(ev.Maturity):
		return refuse(fmt.Sprintf("IBKR's maturity %s for %s differs from %s's %s", brokerMaturity.Format(time.DateOnly), req.identifier, ev.Source, ev.Maturity.Format(time.DateOnly)), "")
	case !ev.Maturity.After(today.AddDate(0, 0, bondRequestMinDays)):
		return refuse(fmt.Sprintf("%s matures %s, within %d days; its settlement and redemption would overlap", req.identifier, ev.Maturity.Format(time.DateOnly), bondRequestMinDays), "")
	}
	terms.Maturity, terms.MaturitySource = ev.Maturity.Format(time.DateOnly), ev.Source
	terms.IssuerClass, terms.Issuer, terms.EvidenceSource, terms.EvidenceAsOf = ev.Class, ev.Issuer, ev.Source, ev.AsOf
	terms.Coupon = new(ev.Coupon)
	terms.AccruedBound = req.face * ev.Coupon / 100
	return nil
}

// bondRequestIdentity binds the named identifier to the line: the line's
// ISIN or CUSIP must be the one asked, and no identifier channel the broker
// sent may name another.
func bondRequestIdentity(req previewBondRequest, line ibkrlib.BondContractDetails) error {
	wantISIN, wantCUSIP := "", ""
	switch req.idType {
	case ibkrlib.BondIdentifierISIN:
		wantISIN = req.identifier
		if strings.HasPrefix(wantISIN, "US") || strings.HasPrefix(wantISIN, "CA") {
			wantCUSIP = wantISIN[2:11]
		}
	case ibkrlib.BondIdentifierCUSIP:
		wantCUSIP = req.identifier
	}
	if (wantISIN == "" || line.ISIN() != wantISIN) && (wantCUSIP == "" || line.CUSIP() != wantCUSIP) {
		return fmt.Errorf("IBKR's line %d does not carry %s (it names ISIN %q, CUSIP %q)", line.ConID, req.identifier, line.ISIN(), line.CUSIP())
	}
	for _, raw := range []string{line.CUSIPField, line.SecIDs[ibkrlib.BondIdentifierCUSIP], line.SecIDs[ibkrlib.BondIdentifierISIN]} {
		id := strings.ToUpper(strings.TrimSpace(raw))
		switch {
		case wantISIN != "" && ibkrlib.ValidISIN(id) && id != wantISIN,
			wantCUSIP != "" && ibkrlib.ValidCUSIP(id) && id != wantCUSIP,
			wantCUSIP != "" && ibkrlib.ValidISIN(id) && (strings.HasPrefix(id, "US") || strings.HasPrefix(id, "CA")) && id[2:11] != wantCUSIP:
			return fmt.Errorf("IBKR's line %d names %s besides %s; the identity is contradictory", line.ConID, id, req.identifier)
		}
	}
	return nil
}

// bondRequestQuantity converts the face amount into order units of the
// currency's convention and checks it against the line's size grid.
func bondRequestQuantity(req previewBondRequest, conv cashSweepInstrumentConvention, rules ibkrlib.BondOrderRules) (int, error) {
	unit := risk.FormatOrderMoney(conv.FacePerUnit, req.currency)
	units := req.face / conv.FacePerUnit
	quantity := int(math.Round(units))
	if quantity < 1 || math.Abs(units-float64(quantity)) > 1e-9 {
		return 0, fmt.Errorf("face %s is not a whole number of order units of %s", risk.FormatOrderMoney(req.face, req.currency), unit)
	}
	if err := rules.CheckQuantity(quantity); err != nil {
		return 0, fmt.Errorf("face %s on a line whose minimum is %s of face and whose step is %s: %v",
			risk.FormatOrderMoney(req.face, req.currency), risk.FormatOrderMoney(float64(rules.Minimum())*conv.FacePerUnit, req.currency),
			risk.FormatOrderMoney(float64(rules.Step())*conv.FacePerUnit, req.currency), err)
	}
	return quantity, nil
}

// bondWhatIfUnitCheck compares the broker's WhatIf initial-margin change of
// a bond buy with the order's value at the assumed unit (A5): a ratio outside
// the band means one order unit is not what Canary assumes. ok is false with
// the reason when it cannot be checked or does not hold.
func bondWhatIfUnitCheck(whatIf rpc.OrderWhatIfResult, terms *rpc.OrderBondTerms, notional, notionalBase float64, contractCcy, baseCcy string) (string, bool) {
	unit := fmt.Sprintf("%s (%s of face per unit)", terms.QuantityUnit, formatBudgetMoney(terms.FacePerUnit, contractCcy))
	m := whatIf.Margin
	if m == nil || m.InitialMarginBefore == nil || m.InitialMarginAfter == nil ||
		math.IsNaN(*m.InitialMarginBefore) || math.IsNaN(*m.InitialMarginAfter) || math.IsInf(*m.InitialMarginBefore, 0) || math.IsInf(*m.InitialMarginAfter, 0) {
		return fmt.Sprintf("the broker's WhatIf carried no initial-margin change, so the assumed unit %s cannot be checked against the broker's own figures", unit), false
	}
	broker := math.Abs(*m.InitialMarginAfter - *m.InitialMarginBefore)
	marginCcy := normCcy(m.Currency)
	var expected float64
	var ccy string
	switch {
	case marginCcy == "" || marginCcy == normCcy(baseCcy):
		expected, ccy = notionalBase, nonEmptyString(normCcy(baseCcy), marginCcy)
	case marginCcy == normCcy(contractCcy):
		expected, ccy = notional, marginCcy
	default:
		return fmt.Sprintf("the broker's WhatIf reports its initial-margin change in %s, neither the account base nor the bond's currency, so the assumed unit %s cannot be checked", marginCcy, unit), false
	}
	if ratio := broker / expected; !positiveFinite(expected) || !positiveFinite(broker) || ratio < cashSweepUnitRatioMin || ratio > cashSweepUnitRatioMax {
		return fmt.Sprintf("the broker's WhatIf initial-margin change %s is %s of the order's expected value %s at the assumed unit %s, outside the band %s to %s; the unit may be wrong",
			formatBudgetMoney(broker, ccy), strconv.FormatFloat(ratio, 'g', 3, 64), formatBudgetMoney(expected, ccy), unit,
			strconv.FormatFloat(cashSweepUnitRatioMin, 'f', -1, 64), strconv.FormatFloat(cashSweepUnitRatioMax, 'f', -1, 64)), false
	}
	return "", true
}
