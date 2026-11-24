package daemon

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The exposure view (owner decision 2026-10-23 18:22 CEST): the book through
// the greeks for the operator to watch, and the premium at risk per name and
// for the book, which the premium gate judges option additions against. It
// reads the same positions as the verdict and the betas the verdict
// measured. A figure a leg cannot supply stays absent with its reason: a
// missing greek, a stale quote, no spot, no exchange rate. Nothing here is
// zero for want of data.

// mandateExposureFigure sums one greek over a row's legs; a leg that cannot
// supply it makes the sum unknown.
type mandateExposureFigure struct {
	value float64
	known bool
}

func (f *mandateExposureFigure) add(v float64) {
	if f.known {
		f.value += v
	}
}

func (f mandateExposureFigure) ptr() *float64 {
	if !f.known {
		return nil
	}
	v := f.value
	return &v
}

// mandateExposure builds the exposure view from a current positions read,
// the verdict's betas and shares, the rulebook's premium limits and the
// order limits' current net liquidation reading. verdict may be unknown:
// the greeks and the premium are measured anyway, and the shares and betas
// are absent.
func (s *Server) mandateExposure(pos *rpc.PositionsResult, verdict *mandate.Verdict, base string, now time.Time) *rpc.MandateExposureView {
	if pos == nil {
		return nil
	}
	pol := s.rulebookPolicy()
	out := &rpc.MandateExposureView{AsOf: pos.AsOf, BaseCurrency: base, NamePremiumCapPct: pol.OptionNameActPct}
	stage, carried := s.rulebookRegimeStage(pol, now)
	out.BookPremiumWatchPct, out.BookPremiumCapPct, out.BookPremiumSet = pol.PremiumBudgetInForce(stage.Bucket, carried)
	nlv := s.currentOrderLimitsNLV(base, now)
	if nlv.Unavailable == "" {
		v := nlv.Base
		out.NLVBase = &v
	} else {
		out.NLVReason = nlv.Unavailable
	}
	premium := risk.PremiumAtRisk(risk.ClassifyNamePutRoles(mapRuleNames(pos, pol, base), pol))
	betas := map[string]mandate.UnderlyingReport{}
	if verdict != nil && verdict.Status != mandate.StatusUnknown {
		for _, u := range verdict.Underlyings {
			betas[u.Underlying] = u
		}
	}
	book := rpc.MandateExposureRow{}
	bookDelta, bookBeta, bookGamma, bookTheta, bookVega := mandateExposureFigure{known: true}, mandateExposureFigure{known: true}, mandateExposureFigure{known: true}, mandateExposureFigure{known: true}, mandateExposureFigure{known: true}
	for _, g := range pos.ByUnderlying {
		row, delta, deltaBeta, gamma, theta, vega := mandateExposureName(g, base, betas, verdict)
		if row.Legs == 0 {
			continue
		}
		mandateExposurePremium(&row, premium, out.NLVBase, out.NamePremiumCapPct, row.Underlying)
		out.Names = append(out.Names, row)
		book.Legs += row.Legs
		book.Options += row.Options
		for _, f := range []struct {
			sum  *mandateExposureFigure
			part mandateExposureFigure
			what string
		}{{&bookDelta, delta, "delta"}, {&bookBeta, deltaBeta, "beta-adjusted delta"}, {&bookGamma, gamma, "gamma"}, {&bookTheta, theta, "theta"}, {&bookVega, vega, "vega"}} {
			if !f.part.known {
				if f.sum.known {
					book.Unknown = append(book.Unknown, f.what+" unknown for "+row.Underlying)
				}
				f.sum.known = false
				continue
			}
			f.sum.add(f.part.value)
		}
	}
	slices.SortStableFunc(out.Names, func(a, b rpc.MandateExposureRow) int {
		switch {
		case a.DeltaBase == nil && b.DeltaBase == nil:
			return cmp.Compare(a.Underlying, b.Underlying)
		case a.DeltaBase == nil:
			return 1
		case b.DeltaBase == nil:
			return -1
		}
		if c := cmp.Compare(math.Abs(*b.DeltaBase), math.Abs(*a.DeltaBase)); c != 0 {
			return c
		}
		return cmp.Compare(a.Underlying, b.Underlying)
	})
	book.DeltaBase, book.DeltaBetaBase, book.GammaBase, book.ThetaBase, book.VegaBase = bookDelta.ptr(), bookBeta.ptr(), bookGamma.ptr(), bookTheta.ptr(), bookVega.ptr()
	if premium.Known {
		v := premium.TotalBase
		book.PremiumAtRiskBase = &v
	} else {
		book.Unknown = append(book.Unknown, "premium at risk unmeasured: "+strings.Join(premium.Unmeasured, "; "))
	}
	if premium.HedgeBase > 0 {
		v := premium.HedgeBase
		book.HedgePremiumBase = &v
	}
	mandateExposureCap(&book, out.NLVBase, out.BookPremiumCapPct)
	out.Book = book
	return out
}

// mandateExposureName measures one underlying's greeks in base currency from
// its stock and option rows: delta as shares plus delta × contracts ×
// multiplier, times the spot; gamma as the change of that delta for a 1%
// move; theta a day and vega a point as the broker's model reports them,
// times contracts and multiplier. A stale option quote, a missing greek,
// spot or rate leaves that figure unknown, named.
func mandateExposureName(g rpc.PositionGroup, base string, betas map[string]mandate.UnderlyingReport, verdict *mandate.Verdict) (row rpc.MandateExposureRow, delta, deltaBeta, gamma, theta, vega mandateExposureFigure) {
	row = rpc.MandateExposureRow{Underlying: g.Underlying}
	delta, gamma, theta, vega = mandateExposureFigure{known: true}, mandateExposureFigure{known: true}, mandateExposureFigure{known: true}, mandateExposureFigure{known: true}
	unknown := func(what, why string, figures ...*mandateExposureFigure) {
		for _, f := range figures {
			f.known = false
		}
		row.Unknown = append(row.Unknown, what+": "+why)
	}
	var stockSpot float64
	// A holding the broker reports as terminal and values at zero carries no
	// exposure (defunctHolding, the shared verdict): it is not a row and does
	// not blank the book (owner review 2026-10-23 22:15 CEST).
	if st := g.Stock; st != nil && st.Quantity != 0 && !defunctHolding(*st) {
		row.Legs++
		desc := st.Symbol + " shares"
		switch {
		case !(st.Mark > 0):
			unknown(desc, "no mark", &delta)
		default:
			if rate, ok := positionBaseRate(*st, base); ok {
				delta.add(st.Quantity * st.Mark * rate)
				stockSpot = st.Mark
			} else {
				unknown(desc, "no exchange rate for "+st.Currency, &delta)
			}
		}
		// Shares carry no gamma, theta or vega.
	}
	for _, o := range g.Options {
		if o.Quantity == 0 {
			continue
		}
		row.Legs++
		row.Options++
		desc := legDesc(o)
		rate, rateOK := positionBaseRate(o, base)
		if !rateOK {
			unknown(desc, "no exchange rate for "+o.Currency, &delta, &gamma, &theta, &vega)
			continue
		}
		if o.Stale {
			why := "quote stale"
			if o.StaleReason != "" {
				why += " (" + o.StaleReason + ")"
			}
			unknown(desc, why, &delta, &gamma, &theta, &vega)
			continue
		}
		units := o.Quantity * float64(optionMultiplier(o))
		spot := 0.0
		if o.Underlying != nil && *o.Underlying > 0 {
			spot = *o.Underlying
		} else if stockSpot > 0 {
			spot = stockSpot
		}
		switch {
		case o.Delta == nil:
			unknown(desc, "no delta from the broker's model", &delta)
		case spot <= 0:
			unknown(desc, "no underlying spot beside its delta", &delta)
		default:
			delta.add(*o.Delta * units * spot * rate)
		}
		switch {
		case o.Gamma == nil:
			unknown(desc, "no gamma from the broker's model", &gamma)
		case spot <= 0:
			unknown(desc, "no underlying spot beside its gamma", &gamma)
		default:
			// The delta changes by gamma × (1% of spot) shares a contract; in
			// money that change is worth the spot.
			gamma.add(*o.Gamma * 0.01 * spot * units * spot * rate)
		}
		if o.Theta == nil {
			unknown(desc, "no theta from the broker's model", &theta)
		} else {
			theta.add(*o.Theta * units * rate)
		}
		if o.Vega == nil {
			unknown(desc, "no vega from the broker's model", &vega)
		} else {
			vega.add(*o.Vega * units * rate)
		}
	}
	deltaBeta = mandateExposureFigure{}
	if u, ok := betas[g.Underlying]; ok && u.ShortHistory {
		// Charged at its whole value, not simulated: no share or beta was
		// measured, so none is shown as if it were.
		row.Unknown = append(row.Unknown, "share of risk and beta: too little daily history; its whole value is charged as lost")
	} else if ok {
		c, b := u.ContributionPct, u.Beta
		row.ContributionPct, row.Beta = &c, &b
		if delta.known {
			deltaBeta = mandateExposureFigure{value: delta.value * b, known: true}
		}
	} else if row.Legs > 0 {
		why := "the verdict prices no " + g.Underlying
		if verdict == nil || verdict.Status == mandate.StatusUnknown {
			why = "the verdict is unknown"
		}
		row.Unknown = append(row.Unknown, "share of risk and beta: "+why)
	}
	row.DeltaBase, row.DeltaBetaBase, row.GammaBase, row.ThetaBase, row.VegaBase = delta.ptr(), deltaBeta.ptr(), gamma.ptr(), theta.ptr(), vega.ptr()
	return row, delta, deltaBeta, gamma, theta, vega
}

// mandateExposurePremium sets a name's premium at risk and its cap.
func mandateExposurePremium(row *rpc.MandateExposureRow, premium risk.PremiumAtRiskReport, nlv *float64, capPct float64, symbol string) {
	name, ok := premium.Name(symbol)
	switch {
	case !ok:
		// No long option on the name: nothing to lose a premium on, and the
		// whole cap is room for one.
		v := 0.0
		row.PremiumAtRiskBase = &v
	case !name.Known:
		row.Unknown = append(row.Unknown, "premium at risk unmeasured: "+strings.Join(name.Unmeasured, "; "))
		return
	default:
		v := name.Base
		row.PremiumAtRiskBase = &v
		if name.HedgeBase > 0 {
			h := name.HedgeBase
			row.HedgePremiumBase = &h
		}
	}
	mandateExposureCap(row, nlv, capPct)
}

// mandateExposureCap sets the premium's share of NLV, the cap in money and
// the room under it, once NLV is known.
func mandateExposureCap(row *rpc.MandateExposureRow, nlv *float64, capPct float64) {
	if row.PremiumAtRiskBase == nil {
		return
	}
	if nlv == nil || !(*nlv > 0) {
		return
	}
	pct := *row.PremiumAtRiskBase / *nlv * 100
	capBase := capPct / 100 * *nlv
	room := math.Max(0, capBase-*row.PremiumAtRiskBase)
	row.PremiumPctNLV, row.PremiumCapBase, row.PremiumRoomBase = &pct, &capBase, &room
	row.PremiumOver = *row.PremiumAtRiskBase >= capBase-mandateReductionEpsilon
}

// mandatePremiumPct words a premium share for a refusal.
func mandatePremiumPct(v float64) string {
	return fmt.Sprintf("%.1f%%", v)
}
