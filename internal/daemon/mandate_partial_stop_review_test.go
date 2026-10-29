package daemon

import (
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
	"testing"
)

func TestMandatePartialStopsCountCoveredLoss(t *testing.T) {
	for _, tc := range []struct {
		name           string
		quantity, stop float64
	}{{"long", 100, 90}, {"short", -100, 110}} {
		t.Run(tc.name, func(t *testing.T) {
			row := mandateReductionStock("SYNA", 9001, tc.quantity, 100)
			pos := mandateReductionBook(row)
			in := *mandateReductionFixture(t, pos, 0.5, 100000, 0).Input
			in.Legs, _ = mandateApplyStops(in.Legs, pos, "EUR", map[string][]mandateArmedStop{mandateLegID(row): {{StopPrice: tc.stop, Quantity: 50}}}, "")
			if in.Legs[0].StopArmed {
				t.Fatal("partial cover must not read fully armed")
			}
			v := mandate.Evaluate(in)
			if v.Loss == nil || v.Loss.StopLossBase != 500 || v.Loss.TotalBase != 500 {
				t.Fatalf("partial stop loss = %+v, want 500", v.Loss)
			}
			if len(v.Loss.Unstopped) != 1 || !strings.Contains(v.Loss.Unstopped[0], "50 of 100 shares stopped") {
				t.Fatalf("uncovered residual not disclosed: %+v", v.Loss.Unstopped)
			}
			if len(v.Legs) != 1 || v.Legs[0].LossBase != 500 {
				t.Fatalf("leg loss = %+v", v.Legs)
			}
		})
	}
}

// A plan that sells part of a partly stopped line counts the loss the line
// rebuilt after the fill counts: the sale takes the uncovered shares first,
// so selling them leaves the stop's loss whole. Spread over the line, the
// covered loss fell with every share sold, and the plan and the forced
// reduction thought 50 unstopped shares sold took 500 off (review of
// 2026-10-29).
func TestMandatePartialStopsSellTheUncoveredSharesFirst(t *testing.T) {
	for _, tc := range []struct {
		name           string
		quantity, stop float64
	}{{"long", 100, 80}, {"short", -100, 120}} {
		t.Run(tc.name, func(t *testing.T) {
			sign := tc.quantity / 100
			stops := func(row rpc.PositionView) map[string][]mandateArmedStop {
				return map[string][]mandateArmedStop{mandateLegID(row): {{StopPrice: tc.stop, Quantity: 50}}}
			}
			held := func(qty float64) mandate.Input {
				row := mandateReductionStock("SYNA", 9001, qty, 100)
				pos := mandateReductionBook(row)
				in := *mandateReductionFixture(t, pos, 0.5, 100000, 0).Input
				in.Legs, _ = mandateApplyStops(in.Legs, pos, "EUR", stops(row), "")
				return in
			}
			loss := func(in mandate.Input) float64 {
				t.Helper()
				v := mandate.Evaluate(in)
				if v.Loss == nil {
					t.Fatalf("unmeasured: %v", v.Reasons)
				}
				return v.Loss.StopLossBase
			}
			in := held(tc.quantity)
			if got := loss(in); got != 1000 {
				t.Fatalf("held loss %g, want 1,000", got)
			}
			for _, cut := range []struct{ sold, want float64 }{{50, 1000}, {70, 600}, {100, 0}} {
				planned := mandateInputWithTrades(in, map[int]float64{9001: -sign * cut.sold})
				if got := loss(planned); got != cut.want {
					t.Fatalf("selling %g: plan counts %g, want %g", cut.sold, got, cut.want)
				}
				if left := tc.quantity - sign*cut.sold; left != 0 {
					if got := loss(held(left)); got != cut.want {
						t.Fatalf("selling %g: the line after the fill counts %g, want %g", cut.sold, got, cut.want)
					}
				}
				closing := mandateInputWithLegs(in, []mandate.Leg{{ID: in.Legs[0].ID, Symbol: "SYNA", Underlying: "SYNA", Kind: mandate.KindStock, Quantity: -sign * cut.sold, Multiplier: 1, Currency: in.Legs[0].Currency, Spot: 100}})
				if got := loss(closing); got != cut.want {
					t.Fatalf("a closing line of %g counts %g, want %g", cut.sold, got, cut.want)
				}
			}
			if v := mandate.Evaluate(mandateInputWithTrades(in, map[int]float64{9001: -sign * 50})); len(v.Loss.Unstopped) != 0 {
				t.Fatalf("the stopped remainder reads unstopped: %v", v.Loss.Unstopped)
			}
			added := mandateInputWithLegs(in, []mandate.Leg{{ID: in.Legs[0].ID, Symbol: "SYNA", Underlying: "SYNA", Kind: mandate.KindStock, Quantity: sign * 50, Multiplier: 1, Currency: in.Legs[0].Currency, Spot: 100}})
			if got := loss(added); got != 1000 {
				t.Fatalf("adding 50 unstopped shares counts %g, want the stop's 1,000", got)
			}
		})
	}
}
