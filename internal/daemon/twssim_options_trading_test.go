//go:build trading

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// setSessions sets the simulated US equity and options sessions in the
// daemon's calendar (the simulated exchange follows the twssim options).
func setSessions(t *testing.T, optionsOpen bool) {
	t.Helper()
	t.Cleanup(marketcal.ClearSimulatedSessions)
	now := time.Now()
	open := marketcal.Window{Open: now.Add(-time.Hour), Close: now.Add(6 * time.Hour)}
	marketcal.SimulateSession(marketcal.MarketUSEquity, open)
	if optionsOpen {
		marketcal.SimulateSession(marketcal.MarketUSOptions, open)
	} else {
		marketcal.SimulateSession(marketcal.MarketUSOptions, marketcal.Window{})
	}
}

// optionDesk is a simulated desk on the owner-like option book, sessions
// open unless closed.
func optionDesk(t *testing.T, book *twssim.Book, optionsOpen bool) (*simDesk, []string) {
	t.Helper()
	now := time.Now()
	if book == nil {
		book = twssim.OptionsBook(now)
	}
	setSessions(t, optionsOpen)
	opts := twssim.Options{Book: book}
	if !optionsOpen {
		opts.ClosedMarkets = []string{twssim.MarketUSOptions}
	}
	return startSimDesk(t, opts, true), twssim.OptionExpiries(now)
}

func (d *simDesk) optionContract(expiry string, strike float64, right string) rpc.ContractParams {
	d.t.Helper()
	id, ok := d.sim.Book().OptionConIDFor(900001, expiry, strike, right)
	if !ok {
		d.t.Fatalf("no SYNA %s %v %s listed", expiry, strike, right)
	}
	return rpc.ContractParams{ConID: id, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Strike: strike, Right: right, Multiplier: 100, TradingClass: "SYNA"}
}

func (d *simDesk) market(right string) rpc.AddMarketResult {
	d.t.Helper()
	res, rerr := dispatchJSON[rpc.AddMarketResult](d.t, d.srv, rpc.MethodAddMarket, rpc.AddMarketParams{Underlying: rpc.ContractParams{Symbol: "SYNA", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Right: right, Discover: true})
	if rerr != nil {
		d.t.Fatalf("add.market: %+v", rerr)
	}
	return res
}

func (d *simDesk) callPlan(c rpc.ContractParams, limit, budget float64) (rpc.AddCallPlanResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.AddCallPlanResult](d.t, d.srv, rpc.MethodAddCallPlan, rpc.AddCallPlanParams{Contract: c, LimitPrice: limit, Budget: budget})
}

// optionPreview is Desk's option entry preview (execution_canary.go:221):
// explicit-limit LMT DAY, with the Adaptive priority Desk sends unless
// adaptive is empty.
func (d *simDesk) optionPreview(c rpc.ContractParams, qty int, limit float64, adaptive string) (rpc.OrderPreviewResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.OrderPreviewResult](d.t, d.srv, rpc.MethodOrderPreview, rpc.OrderPreviewParams{Action: "BUY", Contract: c, Quantity: qty, OrderType: "LMT", LimitPrice: &limit, Strategy: "explicit-limit", TIF: "DAY", AdaptivePriority: adaptive})
}

// The normal gate's option smoke: add market discovers an eligible call from
// the simulated chain with a two-sided quote and model greeks.
func TestSimulatedTWSOptionCallEntrySmoke(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	found := false
	for _, c := range d.market("C").Calls {
		if c.Contract != nil && c.Quote != nil && c.Quote.Ask != nil && *c.Quote.Ask > 0 && c.Quote.Delta != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("add market discovered no quoted call")
	}
}

func (d *simDesk) call60() rpc.AddCallCandidate {
	d.t.Helper()
	for _, c := range d.market("C").Calls {
		if c.Horizon == 60 && c.Contract != nil && c.Quote != nil && c.Quote.Ask != nil {
			return c
		}
	}
	d.t.Fatal("no quoted 60-day call")
	return rpc.AddCallCandidate{}
}

// Call entry: discover, preview the exact call at its ask as a plain LMT DAY
// (the overseer's rule: Adaptive only on a SMART stock), place, fill. The
// frame carries the option identity with multiplier 100 as a double, no
// algorithm and nothing the broker drops.
func TestSimulatedTWSOptionCallEntry(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	call := d.call60()
	limit := *call.Quote.Ask
	held, _ := d.sim.Book().Position(call.Contract.ConID)
	preview, rerr := d.optionPreview(*call.Contract, 2, limit, "")
	if rerr != nil || !preview.TokenMinted {
		t.Fatalf("order.preview: %+v", rerr)
	}
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("order.place: %+v", rerr)
	}
	v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	sent := d.realPlaces()
	if len(sent) != 1 || v.Filled != 2 {
		t.Fatalf("frames %d, record %s", len(sent), orderLine(v))
	}
	o := sent[0]
	if o.SecType != "OPT" || o.Right != "C" || o.Strike != call.Contract.Strike || o.Expiry != call.Contract.Expiry || o.Multiplier != 100 || o.ConID != call.Contract.ConID ||
		o.Action != "BUY" || o.OrderType != "LMT" || o.TIF != "DAY" || o.LimitPrice != limit || o.Quantity != 2 || o.OpenClose != "O" || o.AlgoStrategy != "" || len(o.AlgoParams) != 0 {
		t.Fatalf("option frame: %+v", o)
	}
	for _, f := range d.allPlaces() {
		if len(f.IgnoredByBroker) > 0 {
			t.Fatalf("fields the broker drops: %v", f.IgnoredByBroker)
		}
	}
	if p, ok := d.sim.Book().Position(call.Contract.ConID); !ok || p.Quantity != held.Quantity+2 {
		t.Fatalf("broker position: %+v", p)
	}
}

// Adaptive on an option is refused before the wire, as a bad request that
// says to leave it off (internal/daemon/adaptive.go).
func TestSimulatedTWSOptionAdaptiveRefused(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	call := d.call60()
	if _, rerr := d.optionPreview(*call.Contract, 1, *call.Quote.Ask, "Normal"); rerr == nil || rerr.Code != rpc.CodeBadRequest || !strings.Contains(rerr.Message, "leave Adaptive off") {
		t.Fatalf("Adaptive on an option: %+v", rerr)
	}
	if len(d.allPlaces()) != 0 {
		t.Fatal("a frame reached the wire")
	}
}

// add call-plan sizes a call against a 1,500 USD budget: debit = quantity x
// limit x 100 + the fee bound, inside the budget.
func TestSimulatedTWSOptionCallPlanSizes(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	call := d.call60()
	limit, budget := *call.Quote.Ask, 1500.0
	plan, rerr := d.callPlan(*call.Contract, limit, budget)
	if rerr != nil {
		t.Fatalf("add call-plan: %+v", rerr)
	}
	if plan.Quantity <= 0 || plan.FeeBound <= 0 || plan.Debit > budget || plan.Debit < float64(plan.Quantity)*limit*100+plan.FeeBound-0.01 {
		t.Fatalf("fee-bounded size: %+v", plan)
	}
	for _, f := range d.allPlaces() {
		if f.Order.AlgoStrategy != "" {
			t.Fatalf("a sizing WhatIf carried %s", f.Order.AlgoStrategy)
		}
	}
}

// putCoverUnread is add call-plan's refusal when the put's cover cannot be
// measured: the cover comes from the mandate's current verdict
// (mandate_verdict.go:765), which a book without a measured mandate (the
// simulated one) never has, held shares or not.
const putCoverUnread = "cannot be read to size a protective put"

// Put entry: a protective put on the 100 held shares is placed and fills;
// add call-plan cannot size it without a mandate verdict, and refuses a put
// with no shares (add_call_plan.go:33-39).
func TestSimulatedTWSOptionPutEntry(t *testing.T) {
	t.Run("covered", func(t *testing.T) {
		d, exps := optionDesk(t, nil, true)
		c := d.optionContract(exps[2], 45, "P")
		if _, rerr := d.callPlan(c, 1.5, 1500); rerr != nil {
			if !strings.Contains(rerr.Message, putCoverUnread) {
				t.Fatalf("add call-plan for a covered put: %+v", rerr)
			}
			t.Logf("add call-plan for a covered put: %s", rerr.Message)
		}
		preview, rerr := d.optionPreview(c, 1, 1.5, "")
		if rerr != nil {
			t.Fatalf("put preview: %+v", rerr)
		}
		placed, rerr := d.place(preview.PreviewToken)
		if rerr != nil {
			t.Fatalf("put place: %+v", rerr)
		}
		v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" || v.Status == "Submitted" })
		t.Logf("put record %s", orderLine(v))
		if o := d.realPlaces(); len(o) != 1 || o[0].Right != "P" || o[0].Action != "BUY" {
			t.Fatalf("put frame %+v", o)
		}
	})
	t.Run("uncovered", func(t *testing.T) {
		book := twssim.OptionsBook(time.Now())
		book.SetPosition(900001, 0, 0)
		d, exps := optionDesk(t, book, true)
		_, rerr := d.callPlan(d.optionContract(exps[2], 45, "P"), 1.5, 1500)
		if rerr == nil {
			t.Fatal("a put with no held shares was sized")
		}
		t.Logf("uncovered put: %s", rerr.Message)
		if len(d.realPlaces()) != 0 {
			t.Fatal("an order reached the wire")
		}
	})
}

// Refusals before any order: the options session closed, an expiry inside
// seven days, a WhatIf without a commission, a WhatIf the broker rejects
// for margin. Each records what Canary returns to the caller.
func TestSimulatedTWSOptionRefusals(t *testing.T) {
	t.Run("session-closed", func(t *testing.T) {
		d, exps := optionDesk(t, nil, false)
		_, rerr := d.callPlan(d.optionContract(exps[3], 55, "C"), 1, 1500)
		if rerr == nil {
			t.Fatal("call-plan sized while the options session is closed")
		}
		t.Logf("closed session: %s", rerr.Message)
	})
	t.Run("expiry-inside-seven-days", func(t *testing.T) {
		d, exps := optionDesk(t, nil, true)
		_, rerr := d.callPlan(d.optionContract(exps[0], 50, "C"), 1, 1500)
		if rerr == nil {
			t.Fatal("call-plan sized an expiry inside seven days")
		}
		t.Logf("short expiry: %s", rerr.Message)
	})
	for _, tc := range []struct {
		name  string
		fault twssim.Fault
	}{
		{"whatif-no-fee", twssim.Fault{Kind: twssim.FaultWhatIfNoFee}},
		{"whatif-margin-reject-201", twssim.Fault{Kind: twssim.FaultWhatIfReject, Code: 201}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, exps := optionDesk(t, nil, true)
			d.sim.ArmFault(tc.fault)
			preview, rerr := d.optionPreview(d.optionContract(exps[3], 55, "C"), 1, 1, "")
			if tc.fault.Kind == twssim.FaultWhatIfReject && rerr == nil && preview.Executable {
				t.Fatal("an executable preview after a margin rejection")
			}
			msg := ""
			if rerr != nil {
				msg = rerr.Code + ": " + rerr.Message
			}
			t.Logf("%s: caller sees %q (minted %t, executable %t)", tc.name, msg, preview.TokenMinted, preview.Executable)
			if len(d.realPlaces()) != 0 {
				t.Fatal("an order reached the wire")
			}
		})
	}
}

// Rule 12 passes with index protection inside its band, and then --max
// sizes a positive addition. The book is the one these control commands
// make of the default book (the Desk harness's recipe):
//
//	canary-sim ctl -dir D position SYNA 600 48
//	canary-sim ctl -dir D options SYNX 520 540 560
//	canary-sim ctl -dir D position 910603 1 700   (the SYNX 540 put, 57-63 days out)
//
// 600 SYNA is 30,000 USD of gross long, 20 percent of the account (rule 1
// watches at 30); one put on SYNX (the synthetic index fund at 600, a hedge
// symbol in the simulator's Rulebook) at 540 is about 0.17 delta, about 10,000 USD of
// index short: 33 percent, inside the calm band of 25-35 percent, so rule 12
// passes and --max sizes up to the band's bottom.
func TestSimulatedTWSMaxSizesWithIndexProtection(t *testing.T) {
	now := time.Now()
	book := twssim.Default()
	book.SetPosition(900001, 600, 48)
	exps := twssim.OptionExpiries(now)
	if err := book.ListOptions(900601, exps, []float64{520, 540, 560}); err != nil {
		t.Fatal(err)
	}
	put, ok := book.OptionConIDFor(900601, exps[3], 540, "P")
	if !ok || put != 910603 {
		t.Fatalf("SYNX 540 put: %d", put)
	}
	book.SetPosition(put, 1, 700)
	setSessions(t, true)
	d := startSimDesk(t, twssim.Options{Book: book}, true)
	plan := d.plan(rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, LimitPrice: 50.10, Max: true})
	if len(plan.Blockers) > 0 || plan.Quantity <= 0 {
		t.Fatalf("max plan with index protection: %d, %+v", plan.Quantity, plan.Blockers)
	}
	t.Logf("--max sizes %d SYNA (put con id %d)", plan.Quantity, put)
}
