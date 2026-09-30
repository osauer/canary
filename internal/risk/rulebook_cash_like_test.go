package risk

import (
	"slices"
	"testing"
)

// Rule 14's evidence gains one note per currency; the figure is untouched
// and a missing part reads unavailable, never zero.
func TestRuleFourteenCashLikeEvidence(t *testing.T) {
	cash, eq := 12000.0, 3000.0
	c := &ruleContext{in: RuleInputs{CashLike: []CurrencyCashLike{{Currency: "usd", Cash: &cash, Equivalents: &eq}, {Currency: "EUR", Cash: &cash}}}}
	observed := 30.0
	row := c.withCashLikeEvidence(RuleRow{ID: RuleFXExposure, Observed: &observed, Notes: []string{"existing"}})
	want := []string{"existing", "USD cash 12000 · cash equivalents 3000 · cash-like 15000", "EUR cash 12000 · cash equivalents unavailable · cash-like unavailable"}
	if !slices.Equal(row.Notes, want) || *row.Observed != 30 {
		t.Fatalf("notes = %q observed %v", row.Notes, *row.Observed)
	}
	if got := (&ruleContext{}).withCashLikeEvidence(RuleRow{ID: RuleFXExposure}); got.Notes != nil {
		t.Fatalf("notes without cash-like input: %q", got.Notes)
	}
}
