//go:build trading

package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// saleOptionsOpenIn installs a US options session whose send time (the open
// plus the options opening offset) is in from now, beside a US equity session
// that is open: the options session opened just under fifteen minutes ago, so
// a queue is still offered and its window starts soon. There is no clock seam
// in the simulated daemon; this is the wall-clock workaround the queued buy
// scenario uses, for the options market.
func saleOptionsOpenIn(t *testing.T, in time.Duration) time.Time {
	t.Helper()
	t.Cleanup(marketcal.ClearSimulatedSessions)
	now := time.Now()
	open := now.Add(in - readinessOptionsOpeningOffset)
	marketcal.SimulateSession(marketcal.MarketUSEquity, marketcal.Window{Open: now.Add(-time.Hour), Close: now.Add(6 * time.Hour)})
	marketcal.SimulateSession(marketcal.MarketUSOptions, marketcal.Window{Open: open, Close: now.Add(6 * time.Hour)})
	return open
}

// saleDesk is a simulated desk on the option book with qty long calls of
// SYNA at the expiry and strike pick chooses, the options session opening in
// sendIn. The SYNB bear put spread is closed, so the rest of the book is net
// long delta and selling SYNA calls lowers the whole book's absolute delta
// (saleDeskWithSpread keeps it). edit changes the book before the daemon
// starts. It returns the desk, the held call and the options session's open.
func saleDesk(t *testing.T, qty float64, sendIn time.Duration, pick func(b *twssim.Book, exps []string) int, edit func(b *twssim.Book)) (*simDesk, rpc.ContractParams, time.Time) {
	t.Helper()
	return saleDeskBook(t, qty, sendIn, pick, func(b *twssim.Book) {
		exps := twssim.OptionExpiries(time.Now())
		for _, k := range []float64{110, 120} {
			if id, ok := b.OptionConIDFor(900002, exps[3], k, "P"); ok {
				b.SetPosition(id, 0, 0)
			}
		}
		if edit != nil {
			edit(b)
		}
	})
}

// saleDeskWithSpread is saleDesk on the option book as it is, the SYNB bear
// put spread held.
func saleDeskWithSpread(t *testing.T, qty float64, sendIn time.Duration, pick func(b *twssim.Book, exps []string) int) (*simDesk, rpc.ContractParams, time.Time) {
	t.Helper()
	return saleDeskBook(t, qty, sendIn, pick, nil)
}

func saleDeskBook(t *testing.T, qty float64, sendIn time.Duration, pick func(b *twssim.Book, exps []string) int, edit func(b *twssim.Book)) (*simDesk, rpc.ContractParams, time.Time) {
	t.Helper()
	now := time.Now()
	book := twssim.OptionsBook(now)
	exps := twssim.OptionExpiries(now)
	id := pick(book, exps)
	in, ok := book.Contract(id)
	if !ok {
		t.Fatalf("no option %d", id)
	}
	book.SetPosition(id, qty, in.Price*100)
	if edit != nil {
		edit(book)
	}
	open := saleOptionsOpenIn(t, sendIn)
	d := startSimDesk(t, twssim.Options{Book: book}, true)
	c := rpc.ContractParams{ConID: id, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: in.Expiry, Strike: in.Strike, Right: in.Right, Multiplier: 100, TradingClass: "SYNA"}
	return d, c, open
}

// saleCall is the SYNA call at an expiry index and strike.
func saleCall(exp int, strike float64) func(*twssim.Book, []string) int {
	return func(b *twssim.Book, exps []string) int {
		id, _ := b.OptionConIDFor(900001, exps[exp], strike, "C")
		return id
	}
}

// saleCheapCall is the SYNA call more than seven days out whose model price
// is closest to target.
func saleCheapCall(target float64) func(*twssim.Book, []string) int {
	return func(b *twssim.Book, exps []string) int {
		best, gap := 0, math.Inf(1)
		for _, exp := range exps[1:] {
			for _, k := range []float64{40, 45, 50, 55, 60} {
				id, ok := b.OptionConIDFor(900001, exp, k, "C")
				if !ok {
					continue
				}
				if in, _ := b.Contract(id); math.Abs(in.Price-target) < gap {
					best, gap = id, math.Abs(in.Price-target)
				}
			}
		}
		return best
	}
}

// queueSale prepares and arms an owner-queued option sale as the console
// does, with the synthetic device envelope the queue tests use; floor is the
// signed minimum price.
func (d *simDesk) queueSale(contract rpc.ContractParams, qty int, floor float64) rpc.TradeProposalQueuePrepareResult {
	d.t.Helper()
	e := d.srv.tradeProposals
	scope := e.currentScope()
	q, err := e.QueuePrepare(d.t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode,
		Order: &rpc.OrderPreviewParams{Contract: contract, Action: "SELL", Quantity: qty, OrderType: "LMT", TIF: "DAY", LimitPrice: &floor}})
	if err != nil || !q.Accepted {
		d.t.Fatalf("queue prepare: %+v %v", q.Blockers, err)
	}
	res, err := e.QueueArm(d.t.Context(), rpc.TradeProposalQueueArmParams{QueuedRef: q.QueuedRef, TermsDigest: q.Queue.TermsDigest,
		DeskActionID: "desk-action-1", Credential: "companion", Envelope: `{"signature":"synthetic"}`, Origin: rpc.OrderOriginAgent})
	if err != nil || !res.Accepted {
		d.t.Fatalf("queue arm: %+v %v", res.Blockers, err)
	}
	return q
}

// saleWaiting is a record still waiting for its window to open.
func saleWaiting(rec queuedAuthRecord) bool {
	return rec.State == rpc.QueuedAuthArmed || rec.State == rpc.QueuedAuthSending ||
		rec.State == rpc.QueuedAuthHeld && (rec.HoldCode == rpc.ReadinessMarketClosed || rec.HoldCode == rpc.ReadinessOpeningWindow)
}

// saleSettled waits until the record leaves the opening wait (sent, cancelled
// or held on anything else) and returns it.
func (d *simDesk) saleSettled(id string, budget time.Duration) queuedAuthRecord {
	d.t.Helper()
	var rec queuedAuthRecord
	d.waitFor("the queued sale to leave the opening wait", budget, func() bool {
		rec = d.queuedRecord(id)
		return !saleWaiting(rec)
	})
	return rec
}

// saleFrame checks one transmitted sale frame against the signed terms: a
// plain option limit, DAY, regular hours, the exact quantity and con id, at
// or above the floor on the cent grid, nothing the broker drops.
func (d *simDesk) saleFrame(o twssim.PlaceOrder, c rpc.ContractParams, qty int, floor float64) {
	d.t.Helper()
	d.t.Logf("frame: %s %g %s %s @%g tif %s rth-only %t openClose %q algo %q %v con %d",
		o.Action, o.Quantity, o.SecType, o.OrderType, o.LimitPrice, o.TIF, !o.OutsideRTH, o.OpenClose, o.AlgoStrategy, o.AlgoParams, o.ConID)
	cents := math.Round(o.LimitPrice * 100)
	if o.Action != "SELL" || o.Quantity != float64(qty) || o.OrderType != "LMT" || o.TIF != "DAY" || o.OutsideRTH ||
		o.ConID != c.ConID || o.SecType != "OPT" || o.AlgoStrategy != "" || len(o.AlgoParams) != 0 ||
		o.LimitPrice < floor-1e-9 || math.Abs(cents-o.LimitPrice*100) > 1e-6 {
		d.t.Fatalf("sale frame %+v", o)
	}
	for _, f := range d.allPlaces() {
		if len(f.IgnoredByBroker) > 0 {
			d.t.Fatalf("fields the broker drops: %v", f.IgnoredByBroker)
		}
	}
}

// saleSentOnce asserts the sale went out exactly once, and still once 2 s on.
func (d *simDesk) saleSentOnce(rec queuedAuthRecord, open time.Time) twssim.PlaceOrder {
	d.t.Helper()
	sent := d.realPlaces()
	if rec.State != rpc.QueuedAuthSent && rec.State != rpc.QueuedAuthFilled || len(sent) != 1 {
		d.t.Fatalf("queued sale: state %s hold %q %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.HoldReason, rec.ReasonCode, rec.Reason, len(sent))
	}
	if got := rec.Terms.NotBefore.Sub(open); got != readinessOptionsOpeningOffset {
		d.t.Fatalf("window opens %s after the options open, want %s", got, readinessOptionsOpeningOffset)
	}
	if rec.SentAt.Before(rec.Terms.NotBefore) {
		d.t.Fatalf("sent at %s, before the window %s", rec.SentAt, rec.Terms.NotBefore)
	}
	time.Sleep(2 * time.Second)
	if n := len(d.realPlaces()); n != 1 {
		d.t.Fatalf("queued sale sent %d times", n)
	}
	return sent[0]
}

// mid is the simulated option's mid, the price the bounded limit prices off.
func (d *simDesk) saleMid(conID int) float64 {
	d.t.Helper()
	in, ok := d.sim.Book().Contract(conID)
	if !ok {
		d.t.Fatalf("no contract %d", conID)
	}
	return in.Price
}

// S1. 20 long calls, all 20 queued for sale before the options session
// opens, at a minimum below the mid. Twenty contracts are above the option
// contract cap, so the sale passes only as a delta-reducing exit. It goes
// once, after the options opening offset, as a plain limit.
func TestSimulatedTWSQueuedOptionSaleWholeLine(t *testing.T) {
	d, c, open := saleDesk(t, 20, 40*time.Second, saleCall(3, 50), nil)
	limits := d.srv.orderLimitsInForce("USD")
	if limits.MaxOptionContracts >= 20 {
		t.Fatalf("the option cap %d does not bind a 20-contract sale", limits.MaxOptionContracts)
	}
	floor := math.Floor(d.saleMid(c.ConID)*0.9*100) / 100
	q := d.queueSale(c, 20, floor)
	if q.Queue.Terms.PositionEffect != "close" {
		t.Logf("position effect %q", q.Queue.Terms.PositionEffect)
	}
	rec := d.saleSettled(q.Queue.Terms.QueueID, 90*time.Second)
	o := d.saleSentOnce(rec, open)
	d.saleFrame(o, c, 20, floor)
	if p, ok := d.sim.Book().Position(c.ConID); ok {
		t.Logf("broker position after the sale: %g", p.Quantity)
	}
}

// S1b. The same sale on a book whose other lines are net short delta (the
// SYNB bear put spread): selling the calls would raise the whole book's
// absolute delta, so the exemption does not apply, the contract cap binds and
// the record holds under order_risk_limit, retried inside its window,
// nothing on the wire.
func TestSimulatedTWSQueuedOptionSaleRaisesBookDelta(t *testing.T) {
	d, c, _ := saleDeskWithSpread(t, 20, 40*time.Second, saleCall(3, 50))
	floor := math.Floor(d.saleMid(c.ConID)*0.9*100) / 100
	q := d.queueSale(c, 20, floor)
	rec := d.saleSettled(q.Queue.Terms.QueueID, 90*time.Second)
	t.Logf("book delta rises: state %s hold %q %q", rec.State, rec.HoldCode, rec.HoldReason)
	if rec.State != rpc.QueuedAuthHeld || rec.HoldCode != "order_risk_limit" || len(d.realPlaces()) != 0 {
		t.Fatalf("a sale that raises the book's delta: state %s hold %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.ReasonCode, rec.Reason, len(d.realPlaces()))
	}
}

// S2. 400 long calls of a cheap option, 200 queued for sale: above the
// contract cap and the money cap at once (the simulator's money cap is above
// 100 contracts of this premium, so the sale is sized to bind it), passing
// both as a delta-reducing exit.
func TestSimulatedTWSQueuedOptionSaleAboveBothCaps(t *testing.T) {
	d, c, open := saleDesk(t, 400, 40*time.Second, saleCheapCall(0.60), nil)
	mid := d.saleMid(c.ConID)
	limits := d.srv.orderLimitsInForce("USD")
	notional := 200 * mid * 100
	t.Logf("cheap call %s %v at mid %g; 200 contracts are %g USD against the cap %g USD and %d contracts", c.Expiry, c.Strike, mid, notional, limits.CapBase, limits.MaxOptionContracts)
	if limits.MaxOptionContracts >= 200 || notional <= limits.CapBase {
		t.Fatalf("the caps do not both bind: %d contracts, %g USD cap, %g USD notional", limits.MaxOptionContracts, limits.CapBase, notional)
	}
	floor := math.Floor(mid*0.9*100) / 100
	q := d.queueSale(c, 200, floor)
	rec := d.saleSettled(q.Queue.Terms.QueueID, 90*time.Second)
	d.saleFrame(d.saleSentOnce(rec, open), c, 200, floor)
}

// S3. A minimum above the market: the record holds under worst_price and
// nothing reaches the wire; once the quote rises above the minimum inside the
// window, the next cycle sends it once.
func TestSimulatedTWSQueuedOptionSaleFloorAboveMarket(t *testing.T) {
	d, c, open := saleDesk(t, 20, 40*time.Second, saleCall(3, 50), nil)
	mid := d.saleMid(c.ConID)
	floor := math.Ceil(mid*1.2*100) / 100
	q := d.queueSale(c, 20, floor)
	id := q.Queue.Terms.QueueID
	rec := d.saleSettled(id, 90*time.Second)
	if rec.State != rpc.QueuedAuthHeld || rec.HoldCode != queuedHoldWorstPrice || len(d.realPlaces()) != 0 {
		t.Fatalf("floor above the market: state %s hold %q %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.HoldReason, rec.ReasonCode, rec.Reason, len(d.realPlaces()))
	}
	t.Logf("held: %s", rec.HoldReason)
	// The option's own price moves; the underlying stays, so nothing
	// reprices it back.
	d.sim.Book().SetPrice(c.ConID, math.Round(floor*1.15*100)/100)
	d.waitFor("the held sale to send", 75*time.Second, func() bool {
		rec = d.queuedRecord(id)
		return rec.State != rpc.QueuedAuthArmed && rec.State != rpc.QueuedAuthSending && (rec.State != rpc.QueuedAuthHeld || rec.HoldCode != queuedHoldWorstPrice)
	})
	d.saleFrame(d.saleSentOnce(rec, open), c, 20, floor)
}

// saleNoDeltaCall is a held SYNA call whose underlying the broker cannot
// name: its quote carries no underlying price, so Canary has no delta for
// the line, and any delta measurement of SYNA or the book is not current.
const saleNoDeltaCall = 919901

func saleNoDelta(underlying int) twssim.Instrument {
	return twssim.Instrument{ConID: saleNoDeltaCall, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", LocalSymbol: "SYNA  NODELTA",
		TradingClass: "SYNA", LongName: "SYNTHETIC SYNA NO DELTA", MinTick: 0.01, Price: 1.00, Spread: 0.04,
		Expiry: twssim.OptionExpiries(time.Now())[3], Strike: 65, Right: "C", Multiplier: 100, UnderConID: underlying}
}

// S4. The delta is unknown at send time (another SYNA line has no delta), so
// the 20-contract sale cannot pass the contract cap as a delta-reducing exit.
// The record must hold and retry inside its window, never cancel; once the
// delta reads again it is sent once.
func TestSimulatedTWSQueuedOptionSaleDeltaUnknown(t *testing.T) {
	d, c, open := saleDesk(t, 20, 40*time.Second, saleCall(3, 50), func(b *twssim.Book) {
		b.AddInstrument(saleNoDelta(999999))
		b.SetPosition(saleNoDeltaCall, 1, 100)
	})
	floor := math.Floor(d.saleMid(c.ConID)*0.9*100) / 100
	q := d.queueSale(c, 20, floor)
	id := q.Queue.Terms.QueueID
	rec := d.saleSettled(id, 90*time.Second)
	t.Logf("delta unknown at the send: state %s hold %q %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.HoldReason, rec.ReasonCode, rec.Reason, len(d.realPlaces()))
	if rec.State != rpc.QueuedAuthHeld || len(d.realPlaces()) != 0 {
		t.Fatalf("an unknown delta must hold the sale: state %s hold %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.ReasonCode, rec.Reason, len(d.realPlaces()))
	}
	// The line's underlying is known again: its delta reads, and the next
	// cycle sends the sale.
	d.sim.Book().AddInstrument(saleNoDelta(900001))
	held := rec.HoldCode
	d.waitFor("the held sale to send once the delta reads", 75*time.Second, func() bool {
		rec = d.queuedRecord(id)
		return rec.State != rpc.QueuedAuthArmed && rec.State != rpc.QueuedAuthSending && (rec.State != rpc.QueuedAuthHeld || rec.HoldCode != held)
	})
	d.saleFrame(d.saleSentOnce(rec, open), c, 20, floor)
}

// S5. A partial sale of a line (10 of 20) with the position unchanged is sent
// once at 10; when the position changed by hand after the signature, the
// record cancels under position_changed and nothing is sent.
func TestSimulatedTWSQueuedOptionSalePartial(t *testing.T) {
	t.Run("position-unchanged", func(t *testing.T) {
		d, c, open := saleDesk(t, 20, 40*time.Second, saleCall(3, 50), nil)
		floor := math.Floor(d.saleMid(c.ConID)*0.9*100) / 100
		q := d.queueSale(c, 10, floor)
		if q.Queue.Terms.PositionEffect != "reduce" || q.Queue.Terms.PositionQuantity != 20 {
			t.Fatalf("partial sale terms: effect %q position %g", q.Queue.Terms.PositionEffect, q.Queue.Terms.PositionQuantity)
		}
		rec := d.saleSettled(q.Queue.Terms.QueueID, 90*time.Second)
		d.saleFrame(d.saleSentOnce(rec, open), c, 10, floor)
	})
	t.Run("position-changed-by-hand", func(t *testing.T) {
		d, c, _ := saleDesk(t, 20, 40*time.Second, saleCall(3, 50), nil)
		floor := math.Floor(d.saleMid(c.ConID)*0.9*100) / 100
		q := d.queueSale(c, 10, floor)
		d.sim.Book().SetPosition(c.ConID, 15, d.saleMid(c.ConID)*100)
		d.sim.PushAccount()
		rec := d.saleSettled(q.Queue.Terms.QueueID, 90*time.Second)
		t.Logf("position changed by hand: state %s hold %q reason %q %q, %d frames", rec.State, rec.HoldCode, rec.ReasonCode, rec.Reason, len(d.realPlaces()))
		if rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonPositionChanged || len(d.realPlaces()) != 0 {
			t.Fatalf("a hand change after the signature: state %s hold %q reason %q, %d frames", rec.State, rec.HoldCode, rec.ReasonCode, len(d.realPlaces()))
		}
	})
}
