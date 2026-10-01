package daemon

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const cashSweepResolutionRequestBound = "request_bound"

type germanBillSource interface {
	germanBill(context.Context, string, time.Time) (germanBill, error)
}

func (src serverBillSource) germanBill(ctx context.Context, isin string, _ time.Time) (germanBill, error) {
	return fetchGermanBill(ctx, isin)
}

// cashSweepGermanCandidate binds an exact issuer factsheet to the single
// contract returned by the allowlisted ISIN query. IBCID is only the broker's
// contract identifier; it never becomes issuer identity or held-position proof.
func cashSweepGermanCandidate(ctx context.Context, src germanBillSource, lines []ibkrlib.BondContractDetails, line ibkrlib.BondContractDetails, isin string, now time.Time) (cashSweepBillCandidate, error) {
	fail := func(reason string) (cashSweepBillCandidate, error) {
		return cashSweepBillCandidate{}, fmt.Errorf("%s", reason)
	}
	if len(lines) == 0 || !ibkrlib.ValidISIN(isin) || !strings.HasPrefix(isin, "DE") || line.ConID <= 0 {
		return fail("German bill resolution needs an exact allowlisted ISIN and contract")
	}
	issuer, err := src.germanBill(ctx, isin, now)
	if err != nil {
		return cashSweepBillCandidate{}, err
	}
	if serverSrc, ok := src.(serverBillSource); ok {
		now = serverSrc.s.nowUTC()
	}
	today := cashSweepDay(now)
	if issuer.ISIN != isin || issuer.FetchedAt.IsZero() || issuer.FetchedAt.After(now) || now.Sub(issuer.FetchedAt) > treasuryBillUniverseMaxAge || issuer.IssueDate.IsZero() || issuer.IssueDate.After(today) || !issuer.IssueDate.Before(issuer.MaturityDate) || !issuer.MaturityDate.After(today) {
		return fail("German issuer evidence is not current, issued, and exact")
	}
	for _, sibling := range lines {
		if sibling.ConID != line.ConID {
			return fail("the ISIN query returned conflicting broker contracts")
		}
		if !sibling.Complete || sibling.SecType != ibkrlib.SecTypeBill || normCcy(sibling.Currency) != "EUR" || sibling.Coupon != 0 {
			return fail("the exact query did not resolve a complete zero-coupon EUR BILL")
		}
		for _, raw := range []string{sibling.CUSIPField, sibling.SecIDs[ibkrlib.BondIdentifierISIN], sibling.SecIDs[ibkrlib.BondIdentifierCUSIP]} {
			id := strings.ToUpper(strings.TrimSpace(raw))
			if id != "" && id != isin && id != "IBCID"+strconv.Itoa(line.ConID) {
				return fail("the broker's identifier contradicts the requested German bill")
			}
		}
		if strings.TrimSpace(sibling.Maturity) != "" {
			date, ok := sibling.MaturityDate()
			if !ok || !date.Equal(issuer.MaturityDate) {
				return fail("the broker maturity contradicts the German issuer")
			}
		}
		if strings.TrimSpace(sibling.IssueDate) != "" {
			date, ok := sibling.IssueDateValue()
			if !ok || date.After(today) || !date.Before(issuer.MaturityDate) {
				return fail("the broker issue date contradicts an issued German bill")
			}
		}
	}
	return cashSweepBillCandidate{idType: ibkrlib.BondIdentifierISIN, id: isin, instrument: cashSweepInstrumentDEBubill,
		source: rpc.CashSweepBillSourcePolicyISINs, maturity: issuer.MaturityDate, days: cashSweepDaysLeft(today, issuer.MaturityDate),
		maturitySource: rpc.CashSweepMaturitySourceGermanIssuer, publicFetchedAt: issuer.FetchedAt.UTC(), resolutionSource: cashSweepResolutionRequestBound, line: &line}, nil
}

// cashSweepCheckGermanPreview repeats the exact ISIN lookup on the preview's
// own broker session before accepting its request-bound ConID. No old lookup
// can substitute for this proof, and held positions cannot call this admission.
func cashSweepCheckGermanPreview(ctx context.Context, src germanBillSource, lines []ibkrlib.BondContractDetails, line ibkrlib.BondContractDetails, terms rpc.OrderBondTerms, now time.Time) error {
	candidate, err := cashSweepGermanCandidate(ctx, src, lines, line, terms.ISIN, now)
	if err != nil {
		return err
	}
	if candidate.maturity.Format(time.DateOnly) != terms.Maturity || terms.Instrument != cashSweepInstrumentDEBubill || terms.MaturitySource != rpc.CashSweepMaturitySourceGermanIssuer {
		return fmt.Errorf("the German bill's reviewed issuer maturity or instrument changed")
	}
	return nil
}

// cashSweepHeldGermanBinding accepts one fresh allowlisted ISIN mapping, never
// an IBCID label. An unresolved competing query or two aliases remain unknown.
func cashSweepHeldGermanBinding(ctx context.Context, src cashSweepBillSource, issuerSrc germanBillSource, cfg protectionCashSweepCurrency, conID int, now time.Time) (cashSweepBillCandidate, error) {
	var match cashSweepBillCandidate
	found := false
	for _, isin := range cfg.ISINs {
		if !strings.HasPrefix(isin, "DE") {
			continue
		}
		lines, err := src.lines(ctx, ibkrlib.BondIdentifierISIN, isin, "EUR", []string{ibkrlib.SecTypeBill})
		if err != nil {
			return cashSweepBillCandidate{}, fmt.Errorf("an allowlisted German bill identity could not be confirmed")
		}
		line, _, err := bondLineFor(lines, "EUR", 0)
		if err != nil {
			return cashSweepBillCandidate{}, err
		}
		if line.ConID != conID {
			continue
		}
		candidate, err := cashSweepGermanCandidate(ctx, issuerSrc, lines, line, isin, now)
		if err != nil {
			return cashSweepBillCandidate{}, err
		}
		if found {
			return cashSweepBillCandidate{}, fmt.Errorf("multiple allowlisted ISINs map to the held contract")
		}
		match, found = candidate, true
	}
	if !found {
		return cashSweepBillCandidate{}, fmt.Errorf("no exact allowlisted German ISIN maps to the held contract")
	}
	lines, err := src.held(ctx, conID, "EUR", []string{ibkrlib.SecTypeBill})
	if err != nil {
		return cashSweepBillCandidate{}, err
	}
	line, _, err := bondLineFor(lines, "EUR", conID)
	if err != nil {
		return cashSweepBillCandidate{}, err
	}
	if err := cashSweepCheckGermanPreview(ctx, issuerSrc, lines, line, rpc.OrderBondTerms{Instrument: cashSweepInstrumentDEBubill, ISIN: match.id, Maturity: match.maturity.Format(time.DateOnly), MaturitySource: rpc.CashSweepMaturitySourceGermanIssuer}, now); err != nil {
		return cashSweepBillCandidate{}, err
	}
	return match, nil
}

// sessionBillSource bypasses the directory cache for held issuer mappings.
// Every request shares the connector session and concrete account captured at
// construction; a late scope change invalidates all evidence from the pass.
type sessionBillSource struct {
	serverBillSource
	connector *ibkrlib.Connector
	session   ibkrlib.ConnectorSessionBinding
	scope     brokerStateScope
}

func (src sessionBillSource) current() bool {
	return src.connector != nil && src.connector.SessionCurrent(src.session) && src.s.gatewayConnector() == src.connector && sameBrokerScope(src.scope, src.s.currentBrokerStateScope())
}
func (src sessionBillSource) lines(ctx context.Context, idType, id, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error) {
	if !src.current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	lines, err := src.connector.BondContractDetailsForSession(ctx, src.session, ibkrlib.BondContractRequest{IDType: idType, ID: id, Currency: ccy, SecTypes: secTypes}, bondPositionWait)
	if !src.current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	return lines, err
}
func (src sessionBillSource) held(ctx context.Context, conID int, ccy string, secTypes []string) ([]ibkrlib.BondContractDetails, error) {
	if !src.current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	lines, err := src.connector.BondContractDetailsForSession(ctx, src.session, ibkrlib.BondContractRequest{ConID: conID, Currency: ccy, SecTypes: secTypes}, bondPositionWait)
	if !src.current() {
		return nil, ibkrlib.ErrIBKRUnavailable
	}
	return lines, err
}
func (s *Server) heldGermanBinding(ctx context.Context, row rpc.PositionView, now time.Time) (cashSweepBillCandidate, error) {
	if s.protectionPolicies == nil {
		return cashSweepBillCandidate{}, fmt.Errorf("no German bill policy is active")
	}
	policy, _ := s.protectionPolicies.Active()
	bucket := policy.Buckets.CashSweep
	if !bucket.enabled() {
		return cashSweepBillCandidate{}, fmt.Errorf("cash sweep is disabled")
	}
	connector := s.gatewayConnector()
	if connector == nil {
		return cashSweepBillCandidate{}, ibkrlib.ErrIBKRUnavailable
	}
	session, ok := connector.CaptureSession()
	scope := s.currentBrokerStateScope()
	if !ok || !brokerScopeConcrete(scope) {
		return cashSweepBillCandidate{}, ibkrlib.ErrIBKRUnavailable
	}
	src := sessionBillSource{serverBillSource: serverBillSource{s}, connector: connector, session: session, scope: scope}
	ctx, cancel := context.WithTimeout(ctx, cashSweepResolveBudget)
	defer cancel()
	candidate, err := cashSweepHeldGermanBinding(ctx, src, serverBillSource{s}, bucket.currency("EUR"), row.ConID, now)
	if !src.current() {
		return cashSweepBillCandidate{}, fmt.Errorf("broker session or account changed during held bill identity reads")
	}
	return candidate, err
}
