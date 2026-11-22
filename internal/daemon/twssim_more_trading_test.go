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

// The example book (owner's asset mix, with a defunct zero-value line on
// DOLLR4LOT and option rows reporting AMEX): the vertical is grouped at 15
// units and a unit is previewed.
func TestSimulatedTWSOwnerMixBook(t *testing.T) {
	book, err := twssim.LoadBook("../../pkg/ibkr/twssim/testdata/owner-mix-book.json")
	if err != nil {
		t.Fatal(err)
	}
	setSessions(t, true)
	d := startSimDesk(t, twssim.Options{Book: book}, true)
	st := d.heldVertical()
	if st.Units != 15 || !st.Actionable {
		t.Fatalf("vertical %+v", st)
	}
	for _, l := range st.Legs {
		t.Logf("leg %s %s %v exchange %q", l.Contract.Right, l.Contract.Expiry, l.Contract.Strike, l.Contract.Exchange)
	}
	// The defunct line is answered "No security definition" on every probe.
	// The connector marks a symbol inactive only after two such answers in
	// ten minutes (pkg/ibkr/connector.go:1327-1328), and a fresh session
	// probes it once, so here it is not yet classified terminal
	// (handlers.go:813) and the add is held on it; the live book's row is
	// classified. The answer is recorded for the report.
	plan := d.plan(synaAdd)
	t.Logf("stock add on the owner mix: quantity %d, blockers %+v", plan.Quantity, plan.Blockers)
	pv, rerr := d.strategyPreview(st, "add", 1, new(8.0))
	if rerr != nil || !pv.TokenMinted {
		t.Fatalf("strategy add on the owner mix: %+v", rerr)
	}
}

// A held stock that lost its quote without the broker calling it dead holds
// every stock add with the sentence the owner reads.
func TestSimulatedTWSUnquotedHoldingHoldsAStockAdd(t *testing.T) {
	book := twssim.Default()
	book.AddInstrument(twssim.Instrument{ConID: 940002, Symbol: "SYNY", SecType: "STK", Exchange: "SMART", PrimaryExch: "NYSE", Currency: "USD", MinTick: 0.01})
	book.SetPosition(940002, 50, 10)
	d := startSimDesk(t, twssim.Options{Book: book}, true)
	plan := d.plan(synaAdd)
	const want = "held SYNY has no current price or currency conversion, so the book's stock exposure cannot be measured; refresh, or reconcile that holding"
	if len(plan.Blockers) == 0 || plan.Blockers[0].Message != want {
		t.Fatalf("plan beside an unquoted holding: %+v", plan.Blockers)
	}
}
