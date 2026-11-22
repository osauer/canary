package risk

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// The hedge trim (owner decision 2026-11-14 05:45 CET, "Trim card for my
// tap") is rule 12's own remedy. When the forced reduction has sold the calls,
// the index puts stay: the reduction never sells one. They can then be worth
// more than everything they protect. Once their short delta passes the
// over-hedge line, overhedge_multiple times the band top, the plan names the
// sale that returns it to the band top. It only ever proposes: the row waits
// for the owner's device, and no bucket that may be pre-authorised carries it.

// HedgeTrimPlan is the sale of index puts that returns protection to the
// top of rule 12's band.
type HedgeTrimPlan struct {
	// RatioPct is the puts' short delta as a percent of the long book.
	RatioPct float64
	// ActPct is the over-hedge line the ratio passed and TopPct the band top
	// the trim returns to, both the higher of the applicable regime sets.
	ActPct, TopPct float64
	// GrossLongBase, ShortBase and TargetBase are the long book, the puts'
	// short delta and the short delta the band top allows, in base currency.
	GrossLongBase, ShortBase, TargetBase float64
	// Lines are the sales, nearest expiry first.
	Lines []HedgeTrimLine
}

// HedgeTrimLine is one put line's part of the trim.
type HedgeTrimLine struct {
	Symbol, Leg string
	Expiry      time.Time
	// Held and Sell are whole contracts; ShortPerContractBase is one
	// contract's short delta in base currency.
	Held, Sell           float64
	ShortPerContractBase float64
}

// SoldBase is the short delta the plan's sales take off.
func (p HedgeTrimPlan) SoldBase() float64 {
	sold := 0.0
	for _, l := range p.Lines {
		sold += l.Sell * l.ShortPerContractBase
	}
	return sold
}

// indexPutShortBase is a hedge-listed long put's short delta in base
// currency, the unit the long book it is compared with is summed in. A leg
// without its rate is in the base currency already, or its name is marked
// incomplete and no ratio is built on it.
func indexPutShortBase(l LegInput) float64 {
	short := math.Abs(*l.Delta * l.Quantity * l.Multiplier * *l.Underlying)
	if l.FXToBase != nil {
		short *= math.Abs(*l.FXToBase)
	}
	return short
}

// indexPutShorts returns, index-aligned with one underlying's legs, the short
// delta in base currency of each hedge-eligible long put as rule 12 sizes
// it: its own short delta less what a sold put against it gives back. A sold
// put counts against a bought one of the same expiry, contract size and
// trading class with a higher strike, the nearest strike first, for the
// contracts it covers (the
// pairing premium at risk uses, spreadCovers). A bought put measured alone
// reads larger than the pair held, and a put spread then reads as more than
// the book it protects. A sold put without a delta or price, of another
// expiry, or nearer the money gives nothing back.
func indexPutShorts(legs []LegInput) []float64 {
	out := make([]float64, len(legs))
	open := make([]float64, len(legs))
	var longs []int
	for i, l := range legs {
		switch {
		case indexPutRoleEligible(l):
			out[i] = indexPutShortBase(l)
			if l.Multiplier > 0 && !l.Expiry.IsZero() {
				longs = append(longs, i)
			}
		case l.Quantity < 0 && l.HedgeListed && isPut(l.Right) && l.Delta != nil && l.Underlying != nil &&
			l.UnderlyingSource != UnderlyingSourceStockLegMark && l.Multiplier > 0 && !l.Expiry.IsZero():
			open[i] = -l.Quantity
		}
	}
	slices.SortStableFunc(longs, func(a, b int) int { return cmp.Compare(legs[b].Strike, legs[a].Strike) })
	for _, i := range longs {
		long := legs[i]
		var sold []int
		for j, s := range legs {
			if open[j] > 0 && spreadCovers(long, s) {
				sold = append(sold, j)
			}
		}
		slices.SortStableFunc(sold, func(a, b int) int { return cmp.Compare(legs[b].Strike, legs[a].Strike) })
		covered, back := 0.0, 0.0
		for _, j := range sold {
			m := math.Min(long.Quantity-covered, open[j])
			if m <= 0 {
				break
			}
			s := legs[j]
			back += indexPutShortBase(s) * m / -s.Quantity
			covered += m
			open[j] -= m
		}
		out[i] = math.Max(out[i]-back, 0)
	}
	return out
}

// PlanHedgeTrim sizes the trim on rule 12's own measurement. It reports
// false while protection is inside the over-hedge line, and whenever the book
// is not whole: an unhealthy positions read, a name whose exposure or deltas
// are incomplete, or a hedge-listed long put without its delta, price or
// exchange rate. A long line missing from the sum makes the puts read
// over-hedged (2026-11-04), so an unmeasured book sells no protection. For
// the same reason a book with no long exposure at all gets no plan, as rule
// 12 is not evaluated on one: a read that lost its long lines must not offer
// every put.
//
// A carried regime stage reads the higher line and the higher top of its set
// and the calm one, and a stage never observed the highest of all three:
// stale or missing regime data may keep more protection, never sell more.
// Whole contracts only, rounded down, so the hedge never ends under the band
// top.
func PlanHedgeTrim(in RuleInputs, pol RulebookPolicy) (HedgeTrimPlan, bool) {
	a := AssessHedgeTrim(in, pol)
	return a.Plan, a.State == HedgeTrimPlanned
}

// HedgeTrimState is what the trim's measurement found.
type HedgeTrimState string

const (
	// HedgeTrimPlanned: the puts are past the over-hedge line; Plan names
	// the sale.
	HedgeTrimPlanned HedgeTrimState = "planned"
	// HedgeTrimInside: no hedge-listed long put, or the puts are inside the
	// over-hedge line.
	HedgeTrimInside HedgeTrimState = "inside"
	// HedgeTrimNoLongBook: hedge-listed long puts and no long exposure left
	// for them to protect.
	HedgeTrimNoLongBook HedgeTrimState = "no_long_book"
	// HedgeTrimPutUnmeasured: a hedge-listed long put lacks its delta,
	// price or exchange rate; Reason names it.
	HedgeTrimPutUnmeasured HedgeTrimState = "put_unmeasured"
	// HedgeTrimBookIncomplete: the positions read is unhealthy or another
	// name's exposure or deltas are incomplete, so no ratio is built.
	HedgeTrimBookIncomplete HedgeTrimState = "book_incomplete"
)

// HedgeTrimAssessment is the trim's measurement with what it found.
type HedgeTrimAssessment struct {
	State  HedgeTrimState
	Reason string
	Plan   HedgeTrimPlan
}

// AssessHedgeTrim is PlanHedgeTrim with the reason there is no plan, which
// the mandate's loss measure needs (owner decision 2026-11-14 09:45 CET,
// "Count them, fail closed"): puts with no long book left count whole, and a
// put the trim cannot measure makes the verdict unknown.
func AssessHedgeTrim(in RuleInputs, pol RulebookPolicy) HedgeTrimAssessment {
	c := newRuleContext(in, pol)
	if !c.in.Positions.Healthy {
		return HedgeTrimAssessment{State: HedgeTrimBookIncomplete, Reason: "the positions read is not healthy"}
	}
	var plan HedgeTrimPlan
	incomplete, unmeasured := "", ""
	for _, n := range c.in.Names {
		if (c.greeksGapMaterial(n) || !n.ExposureBaseComplete) && incomplete == "" {
			incomplete = n.Symbol + "'s exposure is incomplete"
		}
		if n.ExposureBase > 0 {
			plan.GrossLongBase += n.ExposureBase
		}
		shorts := indexPutShorts(n.Legs)
		for li, l := range n.Legs {
			if !l.HedgeListed || !isPut(l.Right) || l.Quantity <= 0 {
				continue
			}
			held := math.Floor(l.Quantity + 1e-9)
			if !indexPutRoleEligible(l) || held <= 0 || l.FXToBase == nil || !(*l.FXToBase > 0) {
				if unmeasured == "" {
					unmeasured = n.Symbol + " " + l.Desc + " has no delta, price or exchange rate"
				}
				continue
			}
			per := shorts[li] / l.Quantity
			if own := indexPutShortBase(l); shorts[li] == 0 && own > 0 && !math.IsInf(own, 0) {
				// The sold put against it gives all of its short delta
				// back: measured, with nothing to trim.
				continue
			}
			if !(per > 0) || math.IsInf(per, 0) {
				if unmeasured == "" {
					unmeasured = n.Symbol + " " + l.Desc + " has no short delta"
				}
				continue
			}
			plan.ShortBase += per * l.Quantity
			plan.Lines = append(plan.Lines, HedgeTrimLine{Symbol: n.Symbol, Leg: l.Desc, Expiry: l.Expiry, Held: held, ShortPerContractBase: per})
		}
	}
	switch {
	case unmeasured != "":
		return HedgeTrimAssessment{State: HedgeTrimPutUnmeasured, Reason: unmeasured}
	case incomplete != "":
		return HedgeTrimAssessment{State: HedgeTrimBookIncomplete, Reason: incomplete}
	case len(plan.Lines) == 0:
		return HedgeTrimAssessment{State: HedgeTrimInside}
	case !(plan.GrossLongBase > 0):
		return HedgeTrimAssessment{State: HedgeTrimNoLongBook}
	}
	plan, ok := sizeHedgeTrim(c, plan)
	if !ok {
		return HedgeTrimAssessment{State: HedgeTrimInside}
	}
	return HedgeTrimAssessment{State: HedgeTrimPlanned, Plan: plan}
}

// sizeHedgeTrim applies the band to a measured book.
func sizeHedgeTrim(c *ruleContext, plan HedgeTrimPlan) (HedgeTrimPlan, bool) {
	sets := []string{RegimeBucketCalm, RegimeBucketEarlyWarning, RegimeBucketConfirmed}
	if stage := c.in.RegimeStage; stage != "" {
		sets = []string{regimeSetBucket(stage)}
		if c.in.RegimeStageCarried {
			sets = append(sets, RegimeBucketCalm)
		}
	}
	for _, set := range sets {
		plan.TopPct = math.Max(plan.TopPct, c.pol.SetForBucket(set).HedgeBandMaxPct)
	}
	plan.ActPct = c.pol.OverhedgeMultiple * plan.TopPct
	plan.RatioPct = pct(plan.ShortBase, plan.GrossLongBase)
	if plan.RatioPct <= plan.ActPct {
		return HedgeTrimPlan{}, false
	}
	plan.TargetBase = plan.TopPct / 100 * plan.GrossLongBase
	slices.SortStableFunc(plan.Lines, func(a, b HedgeTrimLine) int {
		if order := a.Expiry.Compare(b.Expiry); order != 0 {
			return order
		}
		return cmp.Compare(a.Leg, b.Leg)
	})
	excess := plan.ShortBase - plan.TargetBase
	sold := 0.0
	for i := range plan.Lines {
		l := &plan.Lines[i]
		l.Sell = math.Min(l.Held, math.Floor(excess/l.ShortPerContractBase+1e-9))
		excess -= l.Sell * l.ShortPerContractBase
		sold += l.Sell
	}
	plan.Lines = slices.DeleteFunc(plan.Lines, func(l HedgeTrimLine) bool { return l.Sell <= 0 })
	return plan, sold > 0
}
