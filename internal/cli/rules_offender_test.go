package cli

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
)

// `canary rules` shows each offender's own band and, for rule 1, what was
// netted: the lines of a grouped issuer and its legs, largest move first,
// with credited and uncredited hedges and unbounded legs flagged.
func TestRulesOffenderLinesShowOwnBandAndIssuerLegs(t *testing.T) {
	row := risk.RuleRow{ID: risk.RuleSingleNameExposure, Number: 1, Status: risk.RuleStatusAct, Unit: "% NLV"}
	second := risk.RuleOffender{Symbol: "BBB", Observed: 35, Status: risk.RuleStatusWatch, Note: "worst at a fall to zero"}
	if got := offenderLine(row, second); got != "BBB — watch at 35.0% NLV: worst at a fall to zero" {
		t.Fatalf("second offender = %q", got)
	}
	unknown := risk.RuleOffender{Symbol: "DDD", Status: risk.RuleStatusUnknown, Note: "not measured: DDD stock price unavailable"}
	if got := offenderLine(row, unknown); got != "DDD — not measured: DDD stock price unavailable" {
		t.Fatalf("unknown offender = %q", got)
	}
	x := &risk.IssuerExposure{Issuer: "GroupA", Lines: []string{"AAA", "AAB"}, Legs: []risk.IssuerLeg{
		{Leg: "AAA 20261120 P 90", Kind: risk.IssuerLegPut, Quantity: 2, LossBase: -8000, Hedge: risk.IssuerHedgeCredited},
		{Leg: "AAA stock", Kind: risk.IssuerLegStock, Quantity: 450, LossBase: 45000},
		{Leg: "AAB 20261016 C 120", Kind: risk.IssuerLegCall, Quantity: -3, LossBase: 12000, Unbounded: true},
		{Leg: "AAB 20261016 P 80", Kind: risk.IssuerLegPut, Quantity: 1, LossBase: 300, Hedge: risk.IssuerHedgeUncredited},
		{Leg: "AAB stock", Kind: risk.IssuerLegStock, Quantity: 10, LossBase: 100},
	}}
	lines := issuerDetailLines(x, "EUR")
	want := []string{
		"lines AAA, AAB",
		"AAA stock  450 sh  loses € 45,000.00",
		"AAB 20261016 C 120  -3  loses € 12,000.00  (unbounded, sized at the takeover gap)",
		"AAA 20261120 P 90  +2  gains € 8,000.00  (hedge credited)",
		"AAB 20261016 P 80  +1  loses € 300.00  (hedge not credited)",
		"… 1 more legs (--json has all)",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("issuer detail:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if issuerDetailLines(&risk.IssuerExposure{Issuer: "CCC", Lines: []string{"CCC"}}, "EUR") != nil {
		t.Fatal("a one-line issuer without legs needs no detail")
	}
}
