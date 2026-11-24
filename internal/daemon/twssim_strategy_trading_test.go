//go:build trading

package daemon

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// heldVertical is the SYNB bear put spread as the daemon groups it.
func (d *simDesk) heldVertical() rpc.PositionStrategy {
	d.t.Helper()
	pos, rerr := dispatchJSON[rpc.PositionsResult](d.t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
	if rerr != nil {
		d.t.Fatalf("positions.list: %+v", rerr)
	}
	for _, st := range rpc.NewPositionStrategiesView(&pos).Strategies {
		if st.Underlying == "SYNB" && st.Kind == "vertical" {
			return st
		}
	}
	d.t.Fatal("the SYNB bear put spread is not grouped as a strategy")
	return rpc.PositionStrategy{}
}

// strategyUnits is a strategy's units in the daemon's grouping now, 0 when
// it is no longer grouped.
func (d *simDesk) strategyUnits(id string) int {
	d.t.Helper()
	pos, rerr := dispatchJSON[rpc.PositionsResult](d.t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
	if rerr != nil {
		d.t.Fatalf("positions.list: %+v", rerr)
	}
	for _, st := range rpc.NewPositionStrategiesView(&pos).Strategies {
		if st.ID == id {
			return st.Units
		}
	}
	return 0
}

// strategyPreview is `canary strategies OP ID REV [--units N] [--limit L]`
// as Desk builds it (execution_strategy.go).
func (d *simDesk) strategyPreview(st rpc.PositionStrategy, op string, units int, limit *float64) (rpc.OrderPreviewResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.OrderPreviewResult](d.t, d.srv, rpc.MethodStrategyPreview, rpc.StrategyPreviewParams{StrategyID: st.ID, ExpectedRevision: st.Revision, Operation: op, Units: units, LimitPrice: limit, Source: "strategy_cli"})
}

// The vertical is recognised as one strategy the owner can act on.
func TestSimulatedTWSStrategyIsRecognised(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	st := d.heldVertical()
	if st.Name != "bear put spread" || st.Units != 15 || !st.Actionable || len(st.Legs) != 2 || st.Revision <= 0 {
		t.Fatalf("strategy %+v", st)
	}
}

// Adding a unit sends ONE combination: BAG, legs with conId, ratio 1, the
// held actions, SMART, openClose 0 (Same), shortSalesSlot 0, exemptCode -1;
// BUY LMT DAY at a positive net debit, order openClose O, no smart combo
// routing; its sold leg is capped by its bought leg, so it is not gated as a
// naked sale. The fill raises the units to 2 and changes the revision; the
// old revision is then refused.
func TestSimulatedTWSStrategyAddsAUnit(t *testing.T) {
	d, _ := optionDesk(t, nil, true)
	st := d.heldVertical()
	limit := 8.0
	pv, rerr := d.strategyPreview(st, "add", 1, &limit)
	if rerr != nil || !pv.SubmitEligible || pv.PreviewToken == "" {
		t.Fatalf("strategy add preview: %+v", rerr)
	}
	placed, rerr := d.place(pv.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	sent := d.realPlaces()
	if len(sent) != 1 {
		t.Fatalf("transmitted frames %d, want one combination", len(sent))
	}
	o := sent[0]
	if o.SecType != "BAG" || o.Action != "BUY" || o.OrderType != "LMT" || o.TIF != "DAY" || o.LimitPrice <= 0 || o.OpenClose != "O" || len(o.SmartComboRouting) != 0 || len(o.Legs) != 2 {
		t.Fatalf("BAG frame %+v", o)
	}
	for _, l := range o.Legs {
		if l.ConID <= 0 || l.Ratio != 1 || l.Exchange != "SMART" || l.OpenClose != 0 || l.ShortSalesSlot != 0 || l.ExemptCode != -1 {
			t.Fatalf("combo leg %+v", l)
		}
	}
	var after rpc.PositionStrategy
	deadline := time.Now().Add(15 * time.Second)
	for {
		after = d.heldVertical()
		if after.Units == 16 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, l := range sent[0].Legs {
		p, _ := d.sim.Book().Position(l.ConID)
		t.Logf("broker leg %d %s: %v", l.ConID, l.Action, p.Quantity)
	}
	if after.Units != 16 || after.Revision == st.Revision {
		t.Fatalf("after the fill: units %d revision %d (was %d), source %s", after.Units, after.Revision, st.Revision, after.Source)
	}
	if _, rerr := d.strategyPreview(st, "add", 1, &limit); rerr == nil || !strings.Contains(rerr.Message, "strategy changed") {
		t.Fatalf("a stale revision: %+v", rerr)
	}
}

// A working order on the strategy refuses another; the broker rejecting the
// combination (201) ends the record rejected and nothing else is sent.
func TestSimulatedTWSStrategyAddRefusals(t *testing.T) {
	t.Run("another-open-order", func(t *testing.T) {
		d, _ := optionDesk(t, nil, true)
		st := d.heldVertical()
		limit := 0.5 // below the net ask: rests
		pv, rerr := d.strategyPreview(st, "add", 1, &limit)
		if rerr != nil {
			t.Fatalf("preview: %+v", rerr)
		}
		if _, rerr := d.place(pv.PreviewToken); rerr != nil {
			t.Fatalf("place: %+v", rerr)
		}
		time.Sleep(time.Second)
		second, rerr := d.strategyPreview(st, "add", 1, &limit)
		if rerr == nil {
			placed, perr := d.place(second.PreviewToken)
			t.Logf("second preview minted %t; second place: err %+v accepted %t", second.TokenMinted, perr, placed.Accepted)
			if perr == nil {
				t.Fatal("a second order on a strategy with one working was placed")
			}
		}
	})
	t.Run("broker-rejects-201", func(t *testing.T) {
		d, _ := optionDesk(t, nil, true)
		st := d.heldVertical()
		limit := 8.0
		pv, rerr := d.strategyPreview(st, "add", 1, &limit)
		if rerr != nil {
			t.Fatalf("preview: %+v", rerr)
		}
		d.sim.ArmFault(twssim.Fault{Kind: twssim.FaultReject, Code: 201, Message: "Order rejected - reason:combination not accepted"})
		placed, rerr := d.place(pv.PreviewToken)
		if rerr != nil {
			t.Fatalf("place: %+v", rerr)
		}
		v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return !v.Open })
		t.Logf("rejected combination: %s", orderLine(v))
		if len(d.realPlaces()) != 1 || d.heldVertical().Units != 15 {
			t.Fatal("a rejected combination moved the book or was resent")
		}
	})
	t.Run("automatic-limit-while-closed", func(t *testing.T) {
		d, _ := optionDesk(t, nil, false)
		st := d.heldVertical()
		if _, rerr := d.strategyPreview(st, "add", 1, nil); rerr == nil {
			t.Fatal("an automatic combination limit while the options session is closed")
		} else {
			t.Logf("closed: %s", rerr.Message)
		}
	})
}

// spreadDesk is the options desk with a chain listed on a name the book
// holds no option of: SYNC (a stock added here) or SPY, the Rulebook's
// hedge symbol, which the book holds no shares of.
func spreadDesk(t *testing.T, conID int, strikes []float64, held ...func(*twssim.Book, []string)) (*simDesk, []string) {
	t.Helper()
	now := time.Now()
	book := twssim.OptionsBook(now)
	if conID == 900003 {
		book.AddInstrument(twssim.Instrument{ConID: 900003, Symbol: "SYNC", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", TradingClass: "NMS", LongName: "SYNTHETIC C INC", MinTick: 0.01, Price: 100, Spread: 0.02})
	}
	if err := book.ListOptions(conID, twssim.OptionExpiries(now), strikes); err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		h(book, twssim.OptionExpiries(now))
	}
	return optionDesk(t, book, true)
}

// holdCalls sets the book's position in one listed SYNC call.
func holdCalls(expiry int, strike, qty float64) func(*twssim.Book, []string) {
	return func(b *twssim.Book, exps []string) {
		if id, ok := b.OptionConIDFor(900003, exps[expiry], strike, "C"); ok {
			b.SetPosition(id, qty, 300*qty)
		}
	}
}

func (d *simDesk) listedOption(under int, symbol, expiry string, strike float64, right string) rpc.ContractParams {
	d.t.Helper()
	id, ok := d.sim.Book().OptionConIDFor(under, expiry, strike, right)
	if !ok {
		d.t.Fatalf("no %s %s %v %s listed", symbol, expiry, strike, right)
	}
	return rpc.ContractParams{ConID: id, Symbol: symbol, SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: expiry, Strike: strike, Right: right, Multiplier: 100, TradingClass: symbol}
}

func (d *simDesk) spreads(symbol string, long rpc.ContractParams) rpc.StrategySpreadsResult {
	d.t.Helper()
	res, rerr := dispatchJSON[rpc.StrategySpreadsResult](d.t, d.srv, rpc.MethodStrategySpreads, rpc.StrategySpreadsParams{
		Underlying: rpc.ContractParams{Symbol: symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"}, Long: long})
	if rerr != nil {
		d.t.Fatalf("strategy.spreads: %+v", rerr)
	}
	return res
}

func (d *simDesk) openPreview(long, short rpc.ContractParams, units int, limit *float64) (rpc.OrderPreviewResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.OrderPreviewResult](d.t, d.srv, rpc.MethodStrategyPreview, rpc.StrategyPreviewParams{Operation: "open", Units: units, LimitPrice: limit, Source: "strategy_cli",
		Open: &rpc.StrategyOpenLegs{Long: long, Short: short}})
}

// strategyNamed waits for the daemon to group the strategy id with units.
func (d *simDesk) strategyNamed(id string, units int) rpc.PositionStrategy {
	d.t.Helper()
	var got rpc.PositionStrategy
	d.waitFor("strategy "+id+" grouped", 15*time.Second, func() bool {
		pos, rerr := dispatchJSON[rpc.PositionsResult](d.t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
		if rerr != nil {
			return false
		}
		for _, st := range rpc.NewPositionStrategiesView(&pos).Strategies {
			if st.ID == id && st.Units == units {
				got = st
				return true
			}
		}
		return false
	})
	return got
}

// openSpread runs the open as Desk would: price the candidates, preview the
// default sold strike at its net ask with a broker WhatIf, place, fill. It
// checks the ONE combination sent and returns the strategy the fill made.
func (d *simDesk) openSpread(symbol string, long rpc.ContractParams, units int, name string) (rpc.PositionStrategy, rpc.OrderPreviewResult) {
	d.t.Helper()
	ev := d.spreads(symbol, long)
	if ev.Default < 0 || len(ev.Candidates) == 0 {
		d.t.Fatalf("no default sold strike: %s (%d candidates, rule %+v)", ev.DefaultReason, len(ev.Candidates), ev.Rule)
	}
	pick := ev.Candidates[ev.Default]
	d.t.Logf("spreads %s %v%s: spot %v move %v rule %+v; default short %v width %v net %v/%v/%v max loss %v gain %v breakeven %v saving %v",
		symbol, long.Strike, long.Right, deref(ev.Spot), deref(ev.ExpectedMove), ev.Rule, pick.Short.Contract.Strike, pick.Width,
		deref(pick.NetBid), deref(pick.NetMid), deref(pick.NetAsk), deref(pick.MaxLossPerUnit), deref(pick.MaxGainPerUnit), deref(pick.Breakeven), deref(pick.Saving))
	if ev.Rule.Source != rpc.StrategySpreadRuleDefault || pick.NetAsk == nil || !(*pick.NetMid > 0) || !(*pick.NetMid < pick.Width) {
		d.t.Fatalf("default candidate %+v", pick)
	}
	limit := *pick.NetAsk
	pv, rerr := d.openPreview(long, *pick.Short.Contract, units, &limit)
	if rerr != nil || !pv.SubmitEligible || pv.PreviewToken == "" || pv.WhatIf.Status != rpc.OrderWhatIfStatusAccepted {
		d.t.Fatalf("open preview: %+v (whatif %+v)", rerr, pv.WhatIf)
	}
	// The order cap reads the debit at the limit, not the legs' gross.
	if want := float64(units) * limit * 100; math.Abs(pv.Notional-want) > 1e-6 {
		d.t.Fatalf("open notional %v, want the debit %v", pv.Notional, want)
	}
	placed, rerr := d.place(pv.PreviewToken)
	if rerr != nil || !placed.Accepted {
		d.t.Fatalf("place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	sent := d.realPlaces()
	if len(sent) != 1 {
		d.t.Fatalf("transmitted frames %d, want one combination", len(sent))
	}
	o := sent[0]
	if o.SecType != "BAG" || o.Action != "BUY" || o.OrderType != "LMT" || o.TIF != "DAY" || math.Abs(o.LimitPrice-limit) > 1e-9 || o.Quantity != float64(units) || len(o.SmartComboRouting) != 0 || len(o.Legs) != 2 {
		d.t.Fatalf("BAG frame %+v", o)
	}
	actions := map[int]string{}
	for _, l := range o.Legs {
		if l.Ratio != 1 || l.Exchange != "SMART" || l.OpenClose != 0 {
			d.t.Fatalf("combo leg %+v", l)
		}
		actions[l.ConID] = l.Action
	}
	if actions[long.ConID] != "BUY" || actions[pick.Short.Contract.ConID] != "SELL" {
		d.t.Fatalf("combo leg actions %v", actions)
	}
	st := d.strategyNamed(pv.Draft.StrategyGroup.StrategyID, units)
	if st.Name != name || !st.Actionable || len(st.Legs) != 2 {
		d.t.Fatalf("the opened spread groups as %+v, want an actionable %s", st, name)
	}
	for _, l := range pv.Draft.StrategyGroup.Legs {
		p, _ := d.sim.Book().Position(l.Contract.ConID)
		if p.Quantity != l.After {
			d.t.Fatalf("broker leg %d holds %v, want %v", l.Contract.ConID, p.Quantity, l.After)
		}
	}
	for _, l := range st.Legs {
		if l.Quantity != float64(units*l.Ratio) {
			d.t.Fatalf("strategy leg %+v, want %d units", l, units)
		}
	}
	return st, pv
}

func deref(v *float64) any {
	if v == nil {
		return "-"
	}
	return *v
}

// A bull call spread opened from flat: the candidates are priced, the
// default sold strike is previewed with WhatIf and sent as one guaranteed
// combination, the fill is grouped as the strategy the order named, and the
// existing operations then reduce and close it as one order each.
func TestSimulatedTWSStrategyOpensABullCallSpread(t *testing.T) {
	d, exps := spreadDesk(t, 900003, []float64{90, 95, 100, 105, 110, 115, 120})
	long := d.listedOption(900003, "SYNC", exps[3], 100, "C")
	st, _ := d.openSpread("SYNC", long, 2, "bull call spread")
	limit := 0.05
	pv, rerr := d.strategyPreview(st, "reduce", 1, &limit)
	if rerr != nil || !pv.SubmitEligible {
		t.Fatalf("reduce preview: %+v", rerr)
	}
	placed, rerr := d.place(pv.PreviewToken)
	if rerr != nil {
		t.Fatalf("reduce place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	st = d.strategyNamed(st.ID, 1)
	pv, rerr = d.strategyPreview(st, "close", 0, &limit)
	if rerr != nil || !pv.SubmitEligible {
		t.Fatalf("close preview: %+v", rerr)
	}
	placed, rerr = d.place(pv.PreviewToken)
	if rerr != nil {
		t.Fatalf("close place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	d.waitFor("the spread closed", 15*time.Second, func() bool { return d.strategyUnits(st.ID) == 0 })
	if sent := d.realPlaces(); len(sent) != 3 || sent[1].Action != "SELL" || sent[2].Action != "SELL" {
		t.Fatalf("frames after reduce and close: %+v", sent)
	}
}

// A SPY bear put spread on a book holding no SPY shares is a hedge entry,
// as the single SPY put is: the drawdown brake and the mandate leave it
// open, and the fill is the bear put spread the order named.
func TestSimulatedTWSStrategyOpensASPYBearPutSpreadAsAHedge(t *testing.T) {
	d, exps := spreadDesk(t, 756733, []float64{510, 540, 555, 570, 585, 600, 615})
	long := d.listedOption(756733, "SPY", exps[3], 600, "P")
	_, pv := d.openSpread("SPY", long, 1, "bear put spread")
	if !d.srv.strategyAddHedgeEntry(pv.Draft) {
		t.Fatal("the SPY bear put spread was not judged a hedge entry")
	}
}

// Refusals before anything is sent: a credit spread, a debit at the strike
// width, and an open beside options the book holds on the name.
func TestSimulatedTWSStrategyOpenRefusals(t *testing.T) {
	d, exps := spreadDesk(t, 900003, []float64{90, 95, 100, 105, 110})
	long := d.listedOption(900003, "SYNC", exps[3], 100, "C")
	lower := d.listedOption(900003, "SYNC", exps[3], 95, "C")
	higher := d.listedOption(900003, "SYNC", exps[3], 105, "C")
	limit := 1.0
	if _, rerr := d.openPreview(long, lower, 1, &limit); rerr == nil || !strings.Contains(rerr.Message, "credit spread") {
		t.Fatalf("a bear call credit spread: %+v", rerr)
	}
	wide := 5.0
	if _, rerr := d.openPreview(long, higher, 1, &wide); rerr == nil || !strings.Contains(rerr.Message, "cannot make money") {
		t.Fatalf("a debit at the width: %+v", rerr)
	}
	// The book holds two SYNA 55 calls: a spread may not sell them.
	a := d.optionContract(exps[3], 50, "C")
	b := d.optionContract(exps[3], 55, "C")
	if _, rerr := d.openPreview(a, b, 1, &limit); rerr == nil || !strings.Contains(rerr.Message, "sells already") {
		t.Fatalf("an open selling a held call: %+v", rerr)
	}
	if n := len(d.realPlaces()); n != 0 {
		t.Fatalf("%d orders sent by refused opens", n)
	}
}

// An open resting at the broker: a second open of the same spread is not
// sent beside it, and once it is cancelled unfilled the bought contract
// bought alone is a plain call, not a broken spread.
func TestSimulatedTWSStrategyOpenWhileWorking(t *testing.T) {
	d, exps := spreadDesk(t, 900003, []float64{90, 95, 100, 105, 110, 115})
	long := d.listedOption(900003, "SYNC", exps[3], 100, "C")
	short := d.listedOption(900003, "SYNC", exps[3], 110, "C")
	low := 0.05 // far below the net bid: rests
	pv, rerr := d.openPreview(long, short, 1, &low)
	if rerr != nil || !pv.SubmitEligible {
		t.Fatalf("open preview: %+v", rerr)
	}
	first, rerr := d.place(pv.PreviewToken)
	if rerr != nil || !first.Accepted {
		t.Fatalf("place: %+v", rerr)
	}
	time.Sleep(time.Second)
	// The working order reads as the spread it is, in the open-orders view
	// and in its status.
	open, rerr := dispatchJSON[rpc.OrdersOpenResult](t, d.srv, rpc.MethodOrdersOpen, rpc.OrdersOpenParams{})
	if rerr != nil {
		t.Fatalf("orders.open: %+v", rerr)
	}
	var row *rpc.OrderView
	for i := range open.Orders {
		if open.Orders[i].OrderRef == first.OrderRef {
			row = &open.Orders[i]
		}
	}
	wantSpread := func(where string, v *rpc.OrderView) {
		raw, _ := json.Marshal(v)
		t.Logf("%s row: %s", where, raw)
		c := v.Combination
		if c == nil || c.Source != rpc.OrderCombinationCanary || c.Strategy != "bull_call_spread" || c.Operation != "open" || c.Units != 1 || len(c.Legs) != 2 ||
			c.Legs[0].Action != "BUY" || c.Legs[0].ConID != long.ConID || c.Legs[0].Strike != 100 || c.Legs[0].Right != "C" || c.Legs[0].Expiry != long.Expiry ||
			c.Legs[1].Action != "SELL" || c.Legs[1].ConID != short.ConID || c.Legs[1].Strike != 110 {
			t.Fatalf("%s: the working spread reads as %+v", where, c)
		}
	}
	if row == nil {
		t.Fatalf("the working open is not in orders.open: %+v", open.Orders)
	}
	wantSpread("orders.open", row)
	status := d.status(first.OrderRef)
	wantSpread("order.status", &status.Order)
	history, rerr := dispatchJSON[rpc.OrdersHistoryResult](t, d.srv, rpc.MethodOrdersHistory, rpc.OrdersHistoryParams{})
	if rerr != nil {
		t.Fatalf("orders.history: %+v", rerr)
	}
	found := false
	for i := range history.Orders {
		if history.Orders[i].Order.OrderRef == first.OrderRef {
			wantSpread("orders.history", &history.Orders[i].Order)
			found = true
		}
	}
	if !found {
		t.Fatal("the working open is not in orders.history")
	}
	if second, rerr := d.openPreview(long, short, 1, &low); rerr == nil {
		placed, perr := d.place(second.PreviewToken)
		time.Sleep(time.Second)
		t.Logf("second open: preview minted %t, place err %+v accepted %t", second.TokenMinted, perr, placed.Accepted)
		if perr == nil && placed.Accepted {
			t.Fatal("a second open of a spread with one working was placed")
		}
	} else {
		t.Logf("second open refused: %s", rerr.Message)
	}
	if _, rerr := dispatchJSON[rpc.OrderCancelResult](t, d.srv, rpc.MethodOrderCancel, rpc.OrderCancelParams{ID: first.OrderRef, Origin: rpc.OrderOriginAgent}); rerr != nil {
		t.Fatalf("cancel: %+v", rerr)
	}
	d.waitStatus(first.OrderRef, func(v rpc.OrderView) bool { return !v.Open })
	single, rerr := d.optionPreview(long, 1, 20, "")
	if rerr != nil || !single.SubmitEligible {
		t.Fatalf("single call preview: %+v", rerr)
	}
	bought, rerr := d.place(single.PreviewToken)
	if rerr != nil {
		t.Fatalf("single call place: %+v", rerr)
	}
	d.waitStatus(bought.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	d.waitFor("the call held", 15*time.Second, func() bool { p, _ := d.sim.Book().Position(long.ConID); return p.Quantity == 1 })
	time.Sleep(time.Second)
	pos, perr := dispatchJSON[rpc.PositionsResult](t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
	if perr != nil {
		t.Fatalf("positions: %+v", perr)
	}
	view := rpc.NewPositionStrategiesView(&pos)
	for _, st := range view.Strategies {
		if st.Underlying == "SYNC" {
			t.Fatalf("a call bought alone after an unfilled open reads as strategy %+v", st)
		}
	}
	for _, is := range view.Issues {
		if is.Underlying == "SYNC" {
			t.Fatalf("a call bought alone after an unfilled open is flagged: %+v", is)
		}
	}
}

// The owner's case (2026-11-24): a call spread opened on a name whose book
// holds another call, and one opened over calls the book holds already. The
// spread is listed as one strategy beside the other line and closed as one
// order; the other calls are never touched.
func TestSimulatedTWSStrategyOpensBesideHeldCalls(t *testing.T) {
	for _, tc := range []struct {
		name       string
		held       func(*twssim.Book, []string)
		heldStrike float64
		heldExpiry int
		heldQty    float64
	}{
		{"another-call", holdCalls(2, 95, 1), 95, 2, 1},
		{"same-call-held", holdCalls(3, 100, 3), 100, 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, exps := spreadDesk(t, 900003, []float64{90, 95, 100, 105, 110, 115, 120}, tc.held)
			other := d.listedOption(900003, "SYNC", exps[tc.heldExpiry], tc.heldStrike, "C")
			long := d.listedOption(900003, "SYNC", exps[3], 100, "C")
			st, _ := d.openSpread("SYNC", long, 1, "bull call spread")
			pos, rerr := dispatchJSON[rpc.PositionsResult](t, d.srv, rpc.MethodPositionsList, rpc.PositionsListParams{})
			if rerr != nil {
				t.Fatalf("positions: %+v", rerr)
			}
			view := rpc.NewPositionStrategiesView(&pos)
			for _, g := range view.Strategies {
				t.Logf("listed: %s %s %d unit(s) %s", g.ID, g.Underlying, g.Units, g.Name)
				if g.Underlying == "SYNC" && g.ID != st.ID {
					t.Fatalf("the other call is grouped: %+v", g)
				}
			}
			for _, is := range view.Issues {
				if is.Underlying == "SYNC" {
					t.Fatalf("SYNC flagged beside the opened spread: %+v", is)
				}
			}
			limit := 0.05
			pv, rerr := d.strategyPreview(st, "close", 0, &limit)
			if rerr != nil || !pv.SubmitEligible {
				t.Fatalf("close preview: %+v", rerr)
			}
			placed, rerr := d.place(pv.PreviewToken)
			if rerr != nil {
				t.Fatalf("close place: %+v", rerr)
			}
			d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
			d.waitFor("the spread closed", 15*time.Second, func() bool { return d.strategyUnits(st.ID) == 0 })
			if p, _ := d.sim.Book().Position(other.ConID); p.Quantity != tc.heldQty {
				t.Fatalf("the held call moved: %v, want %v", p.Quantity, tc.heldQty)
			}
			if sent := d.realPlaces(); len(sent) != 2 || sent[1].SecType != "BAG" || sent[1].Action != "SELL" {
				t.Fatalf("frames: %+v", sent)
			}
		})
	}
}

// Reduce and close sell the combination as one order and lower the units.
func TestSimulatedTWSStrategyReduceAndClose(t *testing.T) {
	for _, op := range []string{"reduce", "close"} {
		t.Run(op, func(t *testing.T) {
			d, _ := optionDesk(t, nil, true)
			st := d.heldVertical()
			units := 0
			if op == "reduce" {
				units = 1
			}
			limit := 1.0
			pv, rerr := d.strategyPreview(st, op, units, &limit)
			if rerr != nil || !pv.SubmitEligible {
				t.Fatalf("%s preview: %+v", op, rerr)
			}
			placed, rerr := d.place(pv.PreviewToken)
			if rerr != nil {
				t.Fatalf("%s place: %+v", op, rerr)
			}
			d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
			if o := d.realPlaces(); len(o) != 1 || o[0].SecType != "BAG" || o[0].Action != "SELL" {
				t.Fatalf("%s frame %+v", op, o)
			}
			// The book after the fill: a reduce leaves 14 units, a close leaves
			// no strategy, and a second close of it is never sent.
			want := 14
			if op == "close" {
				want = 0
			}
			got := -1
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				got = d.strategyUnits(st.ID)
				if got == want {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			if got != want {
				t.Errorf("%s: the strategy shows %d units after the fill, want %d", op, got, want)
				if op == "close" {
					again, rerr := d.strategyPreview(d.heldVertical(), "close", 0, &limit)
					sent := 0
					if rerr == nil && again.SubmitEligible {
						_, perr := d.place(again.PreviewToken)
						time.Sleep(time.Second)
						sent = len(d.realPlaces())
						t.Errorf("a second close of a closed spread: preview minted %t, place err %+v, transmitted frames %d", again.TokenMinted, perr, sent)
					} else {
						t.Logf("a second close was refused: %+v", rerr)
					}
				}
			}
		})
	}
}

func (d *simDesk) spreadPlan(long, short rpc.ContractParams, limit, budget float64) (rpc.StrategySpreadPlanResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.StrategySpreadPlanResult](d.t, d.srv, rpc.MethodStrategySpreadPlan, rpc.StrategySpreadPlanParams{Long: long, Short: short, LimitPrice: limit, Budget: budget})
}

// A spread is sized like a single option: the budget with the broker's fee
// bound for the combination, the option cap per leg, and for a SPY bear put
// spread rule 12's over-hedge line read with the spread's net delta, each
// named when it holds the size. Nothing is sent.
func TestSimulatedTWSStrategySpreadPlan(t *testing.T) {
	t.Run("bull-call", func(t *testing.T) {
		d, exps := spreadDesk(t, 900003, []float64{90, 95, 100, 105, 110, 115})
		long := d.listedOption(900003, "SYNC", exps[3], 100, "C")
		short := d.listedOption(900003, "SYNC", exps[3], 110, "C")
		small, rerr := d.spreadPlan(long, short, 3.5, 800)
		if rerr != nil || small.Units != 2 || small.HeldBy != rpc.SpreadPlanHeldByBudget || small.Debit > 800 || !(small.FeeBound > 0) {
			t.Fatalf("an 800 USD budget at 3.50: %+v %+v", small, rerr)
		}
		big, rerr := d.spreadPlan(long, short, 3.5, 1e6)
		t.Logf("a large budget: %+v %+v", big, rerr)
		if rerr != nil || big.Units <= 0 || big.HeldBy == rpc.SpreadPlanHeldByBudget || (big.HeldBy == rpc.SpreadPlanHeldByOptionCap && big.Units != big.OptionCap) || (big.HeldBy == rpc.SpreadPlanHeldByOrderCap && big.Units != big.OrderCapUnits) {
			t.Fatalf("a large budget: %+v %+v", big, rerr)
		}
		if n := len(d.realPlaces()); n != 0 {
			t.Fatalf("%d orders sent by a plan", n)
		}
	})
	t.Run("spy-hedge", func(t *testing.T) {
		// 2,000 SYNA shares: a long book of about 100,000 USD for the puts
		// to protect.
		shares := func(b *twssim.Book, _ []string) { b.SetPosition(900001, 2000, 48) }
		d, exps := spreadDesk(t, 756733, []float64{510, 540, 555, 570, 585, 600, 615}, shares)
		long := d.listedOption(756733, "SPY", exps[3], 600, "P")
		short := d.listedOption(756733, "SPY", exps[3], 555, "P")
		plan, rerr := d.spreadPlan(long, short, 17, 1e6)
		t.Logf("SPY bear put spread plan: %+v %+v", plan, rerr)
		if rerr != nil || plan.Units <= 0 || plan.IndexProtection == nil {
			t.Fatalf("SPY plan: %+v %+v", plan, rerr)
		}
		if plan.IndexProtection.Unmeasured != "" || plan.NetDelta == nil || !(*plan.NetDelta < 0) || plan.HedgeRoomUnits <= 0 || plan.HeldBy != rpc.SpreadPlanHeldByHedgeRoom || plan.Units != plan.HedgeRoomUnits {
			t.Fatalf("a SPY spread held by its net-delta room: %+v", plan)
		}
	})
}
