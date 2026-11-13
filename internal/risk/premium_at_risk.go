package risk

import (
	"math"
	"sort"
)

// Premium at risk is the Rulebook's measure of what a long option book has
// put at stake: per leg the higher of the price paid and today's value (rule 2's line
// figure), summed per underlying and over the book, protection legs counted
// since the owner's decision of 2026-10-27 19:52 CET ("Count it")
// with their share named beside (rule 2's hedge tier and rule 12
// still govern the hedge's own band). Rule 3 measures the book's total
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

// PremiumAtRiskName is one underlying's premium at risk, protection
// included.
type PremiumAtRiskName struct {
	Symbol string
	// Base is the sum over the name's long legs, protection included; Legs
	// counts them. Unmeasured names each leg whose figure is unknown, with
	// why; while any is, Base understates and Known is false.
	Base       float64
	Legs       int
	Known      bool
	Unmeasured []string
	// HedgeBase is the protection premium within Base.
	HedgeBase float64
}

// PremiumAtRiskReport is the book's premium at risk by name and in total.
type PremiumAtRiskReport struct {
	// Names lists every underlying with a long option leg, largest first.
	Names []PremiumAtRiskName
	// TotalBase is the book's figure, protection included; Known is false
	// while any leg is unmeasured, and Unmeasured names each such leg.
	// HedgeBase is the protection premium within TotalBase.
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
		for _, l := range n.Legs {
			if l.Quantity <= 0 {
				continue
			}
			atRisk, _, ok, why := PremiumAtRiskLeg(l)
			row.Legs++
			if !ok {
				row.Known = false
				row.Unmeasured = append(row.Unmeasured, l.Desc+": "+why)
				continue
			}
			if rule12HedgeLeg(l) {
				row.HedgeBase += atRisk
			}
			row.Base += atRisk
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
