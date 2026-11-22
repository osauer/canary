package risk

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Premium at risk is the Rulebook's measure of what a long option book has
// put at stake: per leg the higher of the price paid and today's value (rule 2's line
// figure), a spread at what the pair can lose (PremiumAtRiskLegs), summed per
// underlying and over the book. Protection legs (a hedge-listed long put rule
// 12 sizes as protection) are measured the same way and named beside the
// total, not counted in it: rule 2's hedge tier and rule 12 govern the
// hedge's own size, and buying protection must not read as breaking the
// premium budget. Rule 3 measures the book's total
// against the premium budget; since the owner's decision of 2026-10-23
// 18:22 CEST the same figures gate option additions (option_name_act_pct per
// name, the premium budget's act level for the book) and are served per name
// on the mandate's exposure view. From 2026-10-27 they were also the premium
// part of the mandate's loss measure; since the owner's decision of
// 2026-11-13 05:00 CET the loss measure counts an option at today's value
// and leaves protection out (daemon mandate_loss.go), while the budget and
// the buy gates keep these figures: a loss frees no room to buy.

// PremiumAtRiskLeg is one long leg's premium at risk in base currency and
// the note rule 3 carries for it. ok is false, with why, when the figure
// cannot be measured: a value substituted for want of an FX rate, or neither
// a price paid nor a value. A short or flat leg carries no premium at risk.
func PremiumAtRiskLeg(l LegInput) (atRisk float64, note string, ok bool, why string) {
	if l.Quantity <= 0 {
		return 0, "", true, ""
	}
	if l.MarketValueBaseSource == MarketValueBaseSourceSubstituted {
		return 0, "", false, "premium not convertible to base — no FX rate for the leg's currency"
	}
	if l.CostBasisBase == nil && l.MarketValueBase == 0 {
		return 0, "", false, "neither the price paid nor a current value is known"
	}
	atRisk = math.Abs(l.MarketValueBase)
	if l.CostBasisBase != nil && *l.CostBasisBase > atRisk {
		atRisk, note = *l.CostBasisBase, "counted at the price paid, above today's value"
	}
	return atRisk, note, true, ""
}

// PremiumAtRiskFigure is one leg's premium at risk as the legs of its
// underlying give it. Covered counts the contracts measured with a sold
// option against them; zero means the leg was measured alone.
type PremiumAtRiskFigure struct {
	AtRisk  float64
	Note    string
	OK      bool
	Why     string
	Covered float64
}

// PremiumAtRiskLegs measures every leg of one underlying, index-aligned with
// legs. A long leg alone counts as PremiumAtRiskLeg has it. A long leg with a
// sold option of the same kind, expiry and contract size further out of the
// money against it is a spread, and for the contracts the sold leg covers the
// pair can lose the premium paid less the premium received, or the pair's
// value today when that is higher, and never less than nothing. Contracts the
// sold leg does not cover count in full. A sold leg nearer the money, of
// another expiry, or without a known premium received offsets nothing: the
// figure may overstate, never understate. The sold leg's own risk is not
// premium and stays with the exposure and margin rules.
func PremiumAtRiskLegs(legs []LegInput) []PremiumAtRiskFigure {
	out := make([]PremiumAtRiskFigure, len(legs))
	open := make([]float64, len(legs))
	var longs []int
	for i, l := range legs {
		out[i].AtRisk, out[i].Note, out[i].OK, out[i].Why = PremiumAtRiskLeg(l)
		switch {
		case !spreadLegMeasured(l):
		case l.Quantity < 0:
			open[i] = -l.Quantity
		case l.Quantity > 0 && out[i].OK:
			longs = append(longs, i)
		}
	}
	// The bought leg nearest the money pairs first, each with the nearest
	// sold strike beyond it.
	sort.SliceStable(longs, func(a, b int) bool { return spreadMoneyRank(legs[longs[a]]) < spreadMoneyRank(legs[longs[b]]) })
	for _, i := range longs {
		long := legs[i]
		var sold []int
		for j, s := range legs {
			if open[j] > 0 && spreadCovers(long, s) {
				sold = append(sold, j)
			}
		}
		sort.SliceStable(sold, func(a, b int) bool {
			return math.Abs(legs[sold[a]].Strike-long.Strike) < math.Abs(legs[sold[b]].Strike-long.Strike)
		})
		covered, pairs := 0.0, 0.0
		for _, j := range sold {
			m := math.Min(long.Quantity-covered, open[j])
			if m <= 0 {
				break
			}
			s := legs[j]
			longShare, soldShare := m/long.Quantity, m/-s.Quantity
			paid := *long.CostBasisBase*longShare - *s.CostBasisBase*soldShare
			value := math.Abs(long.MarketValueBase)*longShare - math.Abs(s.MarketValueBase)*soldShare
			pairs += math.Max(math.Max(paid, value), 0)
			covered += m
			open[j] -= m
		}
		if covered <= 0 {
			continue
		}
		out[i].AtRisk = pairs + out[i].AtRisk*(long.Quantity-covered)/long.Quantity
		out[i].Covered = covered
		out[i].Note = "counted with the sold option against it, at what the pair can lose"
		if covered < long.Quantity {
			out[i].Note = fmt.Sprintf("%s of %s contracts counted with the sold option against them, at what the pair can lose",
				strconv.FormatFloat(covered, 'f', -1, 64), strconv.FormatFloat(long.Quantity, 'f', -1, 64))
		}
	}
	return out
}

// spreadLegMeasured reports whether a leg carries what a spread needs: a
// converted value, the premium paid or received, a contract size and an
// expiry.
func spreadLegMeasured(l LegInput) bool {
	return l.MarketValueBaseSource != MarketValueBaseSourceSubstituted && l.CostBasisBase != nil && l.Multiplier > 0 && !l.Expiry.IsZero()
}

// spreadMoneyRank orders bought legs of one kind nearest the money first: a
// put by falling strike, a call by rising strike.
func spreadMoneyRank(l LegInput) float64 {
	if l.Right == "P" {
		return -l.Strike
	}
	return l.Strike
}

// spreadCovers reports whether sold leg s caps bought leg long: the same
// kind, expiry day and contract size, and a strike further out of the money.
func spreadCovers(long, s LegInput) bool {
	if s.Quantity >= 0 || s.Right != long.Right || s.Multiplier != long.Multiplier {
		return false
	}
	ly, lm, ld := long.Expiry.Date()
	sy, sm, sd := s.Expiry.Date()
	if ly != sy || lm != sm || ld != sd {
		return false
	}
	switch long.Right {
	case "P":
		return s.Strike < long.Strike
	case "C":
		return s.Strike > long.Strike
	}
	return false
}

// PremiumAtRiskName is one underlying's premium at risk, with its
// protection premium beside.
type PremiumAtRiskName struct {
	Symbol string
	// Base is the sum over the name's long legs outside protection; Legs
	// counts every long leg, protection too. Unmeasured names each leg whose figure is unknown, with
	// why; while any is, Base understates and Known is false.
	Base       float64
	Legs       int
	Known      bool
	Unmeasured []string
	// HedgeBase is the protection premium, beside Base and not in it.
	HedgeBase float64
}

// PremiumAtRiskReport is the book's premium at risk by name and in total.
type PremiumAtRiskReport struct {
	// Names lists every underlying with a long option leg, largest first.
	Names []PremiumAtRiskName
	// TotalBase is the book's figure outside protection; Known is false
	// while any leg is unmeasured, and Unmeasured names each such leg.
	// HedgeBase is the protection premium beside TotalBase.
	TotalBase  float64
	Known      bool
	Unmeasured []string
	HedgeBase  float64
}

// Name returns the underlying's row, or false while it holds no long option.
func (r PremiumAtRiskReport) Name(symbol string) (PremiumAtRiskName, bool) {
	for _, n := range r.Names {
		if n.Symbol == symbol {
			return n, true
		}
	}
	return PremiumAtRiskName{}, false
}

// PremiumAtRisk measures the book from names whose index-put roles are
// classified (ClassifyIndexPutRoles, as EvaluateRulebook classifies them).
func PremiumAtRisk(names []NameInput) PremiumAtRiskReport {
	out := PremiumAtRiskReport{Known: true}
	for _, n := range names {
		row := PremiumAtRiskName{Symbol: n.Symbol, Known: true}
		figures := PremiumAtRiskLegs(n.Legs)
		for i, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			fig := figures[i]
			row.Legs++
			if !fig.OK {
				row.Known = false
				row.Unmeasured = append(row.Unmeasured, l.Desc+": "+fig.Why)
				continue
			}
			if rule12HedgeLeg(l) {
				row.HedgeBase += fig.AtRisk
				continue
			}
			row.Base += fig.AtRisk
		}
		if row.Legs == 0 {
			continue
		}
		out.TotalBase += row.Base
		out.HedgeBase += row.HedgeBase
		if !row.Known {
			out.Known = false
			out.Unmeasured = append(out.Unmeasured, row.Unmeasured...)
		}
		out.Names = append(out.Names, row)
	}
	sort.SliceStable(out.Names, func(i, j int) bool { return out.Names[i].Base > out.Names[j].Base })
	return out
}

// ClassifyNamePutRoles classifies every leg's index-put role as
// EvaluateRulebook does (ClassifyIndexPutRoles on the names alone), so a
// reader outside the rulebook (the premium gate, the exposure view) names
// the same protection share of the premium.
func ClassifyNamePutRoles(names []NameInput, pol RulebookPolicy) []NameInput {
	return ClassifyIndexPutRoles(RuleInputs{Names: names}, pol).Names
}
