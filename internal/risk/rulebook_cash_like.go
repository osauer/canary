package risk

import (
	"fmt"
	"strings"
)

// CurrencyCashLike is one currency's cash and classified cash equivalents,
// in that currency's own unit (internal-docs/design/cash-sweep.md). Nil is
// unavailable, never zero.
type CurrencyCashLike struct {
	Currency    string
	Cash        *float64
	Equivalents *float64
}

// withCashLikeEvidence appends one note per currency to rule 14's row. A
// sweep moves cash into bills of the same currency, so the rule's figure is
// unchanged by it; the notes show where the currency's value sits.
func (c *ruleContext) withCashLikeEvidence(row RuleRow) RuleRow {
	for _, cl := range c.in.CashLike {
		row.Notes = append(row.Notes, cashLikeNote(cl))
	}
	return row
}

func cashLikeNote(cl CurrencyCashLike) string {
	figure := func(v *float64) string {
		if v == nil {
			return "unavailable"
		}
		return fmt.Sprintf("%.0f", *v)
	}
	sum := "unavailable"
	if cl.Cash != nil && cl.Equivalents != nil {
		sum = fmt.Sprintf("%.0f", *cl.Cash+*cl.Equivalents)
	}
	return fmt.Sprintf("%s cash %s · cash equivalents %s · cash-like %s", strings.ToUpper(cl.Currency), figure(cl.Cash), figure(cl.Equivalents), sum)
}
