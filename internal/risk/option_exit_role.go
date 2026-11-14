package risk

import "math"

// ClassifyCompleteIndexPutRoles applies the existing Rulebook economic-role
// bands to a complete measured book. The caller must establish exact broker
// identity, scope and freshness. Unlike advisory partial-book evaluation,
// this entry point cannot interpret missing exposure as no long book.
// Both the short-put numerator and name exposures are in account base currency.
func ClassifyCompleteIndexPutRoles(in RuleInputs, pol RulebookPolicy) (RuleInputs, bool) {
	if !in.Positions.Healthy || in.BaseCurrency == "" || len(in.Names) == 0 {
		return RuleInputs{}, false
	}
	gross := 0.0
	for _, n := range in.Names {
		if !n.ExposureBaseComplete || !optionExitFinite(n.ExposureBase) {
			return RuleInputs{}, false
		}
		gross += math.Max(n.ExposureBase, 0)
		shortPut := 0.0
		for _, l := range n.Legs {
			if !optionExitFinite(l.Quantity) || l.Quantity == 0 || math.Trunc(l.Quantity) != l.Quantity ||
				!optionExitFinite(l.Multiplier) || l.Multiplier <= 0 || l.Delta == nil || !optionExitFinite(*l.Delta) || math.Abs(*l.Delta) > 1.05 ||
				l.Underlying == nil || !optionExitFinite(*l.Underlying) || *l.Underlying <= 0 || l.UnderlyingSource == UnderlyingSourceStockLegMark ||
				l.FXToBase == nil || !optionExitFinite(*l.FXToBase) || *l.FXToBase <= 0 ||
				(!isPut(l.Right) && !isCall(l.Right)) || (isPut(l.Right) && *l.Delta > 0) || (isCall(l.Right) && *l.Delta < 0) {
				return RuleInputs{}, false
			}
			if !optionExitFinite(*l.Underlying * *l.FXToBase * l.Multiplier * l.Quantity) {
				return RuleInputs{}, false
			}
			if indexPutRoleEligible(l) {
				shortPut += indexPutShortBase(l)
				if !optionExitFinite(shortPut) {
					return RuleInputs{}, false
				}
			}
		}
	}
	if !optionExitFinite(gross) {
		return RuleInputs{}, false
	}
	// classifyIndexPutRoles converts each put with its own rate
	// (indexPutShortBase), so the inputs go in as they are.
	return classifyIndexPutRoles(in, pol), true
}
