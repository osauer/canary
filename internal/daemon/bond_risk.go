package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Bond risk of the held book (internal-docs/design/bond-risk.md, phase 1).
// Each classified bill or bond is measured at its mark: the issuer evidence
// a buy is admitted by (bond_evidence.go) supplies issuer, class, coupon and
// maturity; risk.MeasureBond derives yield, duration and the loss on a
// one-point rise; the book sums them in the account base. Measurement only:
// nothing here warns, blocks or changes a verdict.

// measureBondPositions fills each bond row's risk fields and returns the
// book, nil when none is held. Evidence is read through the sources' caches
// within bondPositionWait; a line still waiting says so and is measured on a
// later read.
func (s *Server) measureBondPositions(ctx context.Context, bonds []rpc.PositionBond, stocks []rpc.PositionView, nlvBase *float64, baseCcy string, now time.Time) *risk.BondBook {
	if len(bonds) == 0 {
		return nil
	}
	rows := map[int]rpc.PositionView{}
	for _, row := range stocks {
		if row.ConID > 0 {
			rows[row.ConID] = row
		}
	}
	src := s.bondEvidenceSupport()
	evCtx, cancel := context.WithTimeout(ctx, bondPositionWait)
	defer cancel()
	lines := make([]risk.BondBookLine, 0, len(bonds))
	for i := range bonds {
		b := &bonds[i]
		row, ok := rows[b.ConID]
		if ok && row.MarketValueBase != nil {
			b.MarketValueBase = new(*row.MarketValueBase)
		}
		measureHeldBond(evCtx, src, b, now)
		lines = append(lines, risk.BondBookLine{Symbol: b.Symbol, Currency: b.Currency, Issuer: nonEmptyString(b.EvidenceIssuer, b.Issuer),
			IssuerClass: b.IssuerClass, MarketValueBase: b.MarketValueBase, RateShockLossBase: b.RateShockLossBase, Unmeasured: b.RiskUnmeasured})
	}
	return risk.SummarizeBondBook(lines, nlvBase, baseCcy)
}

// measureHeldBond measures one row in place, or says in RiskUnmeasured why
// it cannot be.
func measureHeldBond(ctx context.Context, src *bondEvidenceSources, b *rpc.PositionBond, now time.Time) {
	unmeasured := func(reason string) { b.RiskUnmeasured = reason }
	if b.Class != rpc.BondClassBill && b.Class != rpc.BondClassBond {
		unmeasured(nonEmptyString("the line is not classified: "+b.Reason, "the line is not classified"))
		return
	}
	if src.factorPricedConID(b.ConID) {
		unmeasured("inflation-linked: IBKR prices it on a factored principal")
		return
	}
	id := nonEmptyString(b.ISIN, b.CUSIP)
	if id == "" || src == nil {
		unmeasured("IBKR names no ISIN or CUSIP, so no source can supply its coupon")
		return
	}
	line := ibkrlib.BondContractDetails{ConID: b.ConID, DescAppend: b.Issuer, Currency: b.Currency, SecIDs: map[string]string{}}
	if b.ISIN != "" {
		line.SecIDs[ibkrlib.BondIdentifierISIN] = b.ISIN
	}
	if b.CUSIP != "" {
		line.SecIDs[ibkrlib.BondIdentifierCUSIP] = b.CUSIP
	}
	maturity, coupon := time.Time{}, 0.0
	ev, err := src.bondBuyEvidence(ctx, line, id, now)
	switch {
	case err == nil:
		b.EvidenceIssuer, b.IssuerClass, b.EvidenceSource = ev.Issuer, ev.Class, ev.Source
		maturity, coupon = ev.Maturity, ev.Coupon
		b.Coupon = new(coupon)
		if held, parseErr := time.Parse(time.DateOnly, b.Maturity); parseErr == nil && !held.Equal(maturity) {
			unmeasured(fmt.Sprintf("its maturity %s differs from %s's %s", b.Maturity, ev.Source, maturity.Format(time.DateOnly)))
			return
		}
		b.Maturity = maturity.Format(time.DateOnly)
	case b.Class == rpc.BondClassBill && b.Maturity != "":
		// A bill pays no coupon: its classified maturity measures it; the
		// issuer class stays unknown and it counts as non-government.
		maturity, _ = time.Parse(time.DateOnly, b.Maturity)
	default:
		unmeasured("issuer evidence: " + err.Error())
		return
	}
	if !(b.Mark > 0) {
		unmeasured("IBKR sent no mark")
		return
	}
	m, err := risk.MeasureBond(b.Mark, coupon, maturity, now, risk.BondCouponsPerYear(b.Currency))
	if err != nil {
		unmeasured("its yield cannot be solved: " + err.Error())
		return
	}
	b.YieldPct, b.ModifiedDuration = new(m.YieldPct), new(m.ModifiedDuration)
	if b.MarketValueBase == nil {
		unmeasured("its value in the base currency is unknown")
		return
	}
	value := *b.MarketValueBase
	b.DV01Base = new(value * m.ModifiedDuration * 0.0001)
	b.RateShockLossBase = new(value * m.LossFraction(risk.BondShockPoints))
}

// bondBuyRateRisk states a bond buy's yield, duration and the loss on a
// one-point rise at its limit price, for the preview; nil terms or a bond
// that cannot be measured leave them unset.
func bondBuyRateRisk(terms *rpc.OrderBondTerms, price float64, currency string, now time.Time) {
	if terms == nil || terms.Coupon == nil || strings.TrimSpace(terms.Maturity) == "" {
		return
	}
	maturity, err := time.Parse(time.DateOnly, terms.Maturity)
	if err != nil {
		return
	}
	m, err := risk.MeasureBond(price, *terms.Coupon, maturity, now, risk.BondCouponsPerYear(currency))
	if err != nil {
		return
	}
	value := terms.FaceValue * price / 100
	terms.YieldPct, terms.ModifiedDuration = new(m.YieldPct), new(m.ModifiedDuration)
	terms.RateShockLoss = new(value * m.LossFraction(risk.BondShockPoints))
}
