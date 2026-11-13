package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// mandateReductionHedgePut is a hedge-listed long put priced at its mark,
// the price paid above it as a decayed hedge's is.
func mandateReductionHedgePut(conID int, contracts, mark, paid float64) rpc.PositionView {
	row := mandateReductionCall(risk.DefaultRulebookPolicy().HedgeSymbols[0], conID, contracts, 100, 100, mark, "20270115")
	row.Right, row.AvgCost = "P", paid*100
	return row
}

// The mandate's loss measure counts a bought option at what it is worth
// today, what the line can still lose (owner, 2026-11-13 05:00 CET). The
// price paid above that is a loss already taken: it sits in the drawdown
// that shrank the loss room, so counting it here counted it twice. The
// Rulebook's premium at risk keeps the higher of the two, and a row without
// a value falls back to the price paid, since a worthless line and an
// unmarked one read alike.
func TestMandateLossCountsAnOptionAtTodaysValue(t *testing.T) {
	t.Parallel()
	losing := mandateReductionCall("SYNA", 9101, 10, 150, 100, 2, "20270115") // worth 2,000 EUR
	losing.AvgCost = 5 * 100                                                  // paid 5,000 EUR
	var leg mandate.Leg
	mandateOptionLoss(&leg, losing, "EUR")
	if leg.LossUnknown != "" || math.Abs(leg.LossPerUnitBase-200) > 1e-9 {
		t.Fatalf("a losing call counts %.2f EUR a contract (%q), want its value of 200", leg.LossPerUnitBase, leg.LossUnknown)
	}
	if paid, _, ok, _ := risk.PremiumAtRiskLeg(mandateOptionPremiumInput(losing, "EUR")); !ok || math.Abs(paid-5000) > 1e-9 {
		t.Fatalf("the Rulebook's premium at risk reads %.0f, want the 5,000 EUR paid", paid)
	}
	unmarked, zero := losing, 0.0
	unmarked.Mark, unmarked.MarketValue, unmarked.MarketValueBase = 0, 0, &zero
	leg = mandate.Leg{}
	mandateOptionLoss(&leg, unmarked, "EUR")
	if leg.LossUnknown != "" || math.Abs(leg.LossPerUnitBase-500) > 1e-9 {
		t.Fatalf("a row without a value counts %.2f EUR a contract (%q), want the 500 paid", leg.LossPerUnitBase, leg.LossUnknown)
	}
}

// A hedge-listed long put is protection: the loss measure names its value
// beside it and leaves it out, the verdict's leg carries no loss, and a
// directional put on a name that is not hedge-listed still counts.
func TestMandateLossLeavesProtectionOut(t *testing.T) {
	t.Parallel()
	call := mandateReductionCall("SYNA", 9101, 10, 100, 100, 6, "20270115") // 6,000 EUR
	hedge := mandateReductionHedgePut(9201, 5, 8, 20)                       // worth 4,000 EUR, paid 10,000
	bet := mandateReductionCall("SYNB", 9102, 2, 100, 100, 5, "20270115")   // 1,000 EUR
	bet.Right = "P"
	pos := mandateReductionBook(call, hedge, bet, mandateReductionStock("IBCF", 9004, 100, 100))
	in := mandateReductionFixture(t, pos, 0.5, 1e6, 0)
	mandateMarkProtection(in.Input.Legs, risk.DefaultRulebookPolicy())
	v := mandate.Evaluate(*in.Input)
	if v.Loss == nil || math.Abs(v.Loss.PremiumBase-7000) > 1e-6 || math.Abs(v.Loss.ProtectionBase-4000) > 1e-6 {
		t.Fatalf("loss %+v, want 7,000 EUR of options with 4,000 EUR of protection beside it", v.Loss)
	}
	if math.Abs(v.Loss.TotalBase-v.Loss.PremiumBase-v.Loss.StopLossBase) > 1e-6 {
		t.Fatalf("the total %.0f carries more than the options and the stops", v.Loss.TotalBase)
	}
	for _, l := range v.Legs {
		if l.ID == mandateLegID(hedge) && l.LossBase != 0 {
			t.Fatalf("the hedge put carries %.0f EUR of the loss measure", l.LossBase)
		}
	}
}

// With protection worth more than the whole loss room, the forced reduction
// cuts the calls to the target and stops: it reaches the target, keeps part
// of the line and never touches the put. Before 2026-11-13 the put's
// premium stayed in the measure, no call sale could take it off, and the
// plan sold every call without reaching the target.
func TestMandateReductionStopsAtTheTargetBesideProtection(t *testing.T) {
	t.Parallel()
	calls := mandateReductionCall("SYNA", 9101, 40, 100, 100, 15, "20270115") // 60,000 EUR
	hedge := mandateReductionHedgePut(9201, 10, 40, 60)                       // worth 40,000 EUR: over the 30,000 EUR room
	pos := mandateReductionBook(calls, hedge, mandateReductionStock("IBCF", 9004, 100, 100))
	in := mandateReductionFixture(t, pos, 3.5, 1e5, 2)
	mandateMarkProtection(in.Input.Legs, risk.DefaultRulebookPolicy())
	in = mandateReductionVerdict(*in.Input, 2)
	if l := in.Result.Verdict.Loss; l == nil || l.ProtectionBase < l.RoomBase || l.TotalBase <= l.RoomBase {
		t.Fatalf("fixture: %+v", l)
	}
	rows, st := mandateReductionRun(t, in, pos)
	assertMandateRowsReduceOnly(t, rows, pos)
	if _, ok := mandateRowFor(rows, 9201); ok {
		t.Fatal("the reduction sells the protection put")
	}
	row, ok := mandateRowFor(rows, 9101)
	if !ok || row.Mandate.Units >= 40 {
		t.Fatalf("the plan sells %+v of the 40 calls, want a part of them (%s)", row.Mandate, st.Reason)
	}
	if !st.TargetReached || st.ProjectedLossBase == nil || *st.ProjectedLossBase > st.LossTargetBase+1 {
		t.Fatalf("the plan does not reach its target: %+v", st)
	}
}
