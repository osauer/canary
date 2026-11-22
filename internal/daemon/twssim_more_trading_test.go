//go:build trading

package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// A working option order holds a stock add (stock_add.go:477-494): the plan
// is held with the reason the owner reads.
func TestSimulatedTWSWorkingOptionOrderHoldsAStockAdd(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	call := d.call60()
	preview, rerr := d.optionPreview(*call.Contract, 1, *call.Quote.Bid, "")
	if rerr != nil {
		t.Fatalf("option preview: %+v", rerr)
	}
	d.sim.ArmFault(twssim.Fault{Kind: twssim.FaultHold})
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil {
		t.Fatalf("option place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Submitted" })
	plan := d.plan(synaAdd)
	if len(plan.Blockers) == 0 {
		t.Fatalf("a stock add beside a working option order: %+v", plan)
	}
	t.Logf("the owner reads: %s (%s)", plan.Blockers[0].Message, plan.Blockers[0].Kind)
}

// A EUR line on Xetra and a GBP line on the LSE (quoted in pence, price
// magnifier 100): one plan and one preview each, recorded as Canary answers.
func TestSimulatedTWSEuropeanAdds(t *testing.T) {
	for _, tc := range []struct {
		name  string
		add   rpc.AddParams
		limit float64
	}{
		{"EUR-IBIS", rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNE", Currency: "EUR"}, LimitPrice: 40.10, Quantity: 10}, 40.10},
		{"GBP-LSE", rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNG", Currency: "GBP"}, LimitPrice: 2.51, Quantity: 10}, 2.51},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startSimDesk(t, twssim.Options{Book: twssim.EuropeanBook()}, true)
			plan := d.plan(tc.add)
			t.Logf("plan: quantity %d, blockers %+v, warnings %d", plan.Quantity, plan.Blockers, len(plan.Warnings))
			pv, rerr := d.preview(tc.add)
			msg := ""
			if rerr != nil {
				msg = rerr.Code + ": " + rerr.Message
			}
			t.Logf("preview: %q minted %t", msg, pv.TokenMinted)
			for _, f := range d.allPlaces() {
				t.Logf("frame %s %s %v at %v exchange %s/%s algo %q", f.Order.Action, f.Order.Symbol, f.Order.Quantity, f.Order.LimitPrice, f.Order.Exchange, f.Order.PrimaryExch, f.Order.AlgoStrategy)
			}
			if len(d.realPlaces()) != 0 {
				t.Fatal("a preview transmitted an order")
			}
		})
	}
}
