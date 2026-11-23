//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// Exit plan scenarios: the real daemon against the TWS simulator. The plan
// waits 20 minutes after the open; each scenario's session opened just under
// that long ago (the wall-clock workaround: the simulated daemon has no clock
// seam), so the plan starts seconds from now. SYNA trades at 50.00 a share
// (a cent either side); its options reprice from the stock whenever the stock
// moves. Every figure is invented.

const (
	planSYNA = 900001
	planWait = 20 // minutes after the open
)

// planStartIn opens the US equity and options sessions so a plan's wait ends
// in from now; the sessions close close after that start (zero: 6.5 hours
// after the open).
func planStartIn(t *testing.T, in, close time.Duration) time.Time {
	t.Helper()
	t.Cleanup(marketcal.ClearSimulatedSessions)
	now := time.Now()
	open := now.Add(in - planWait*time.Minute)
	end := open.Add(6*time.Hour + 30*time.Minute)
	if close > 0 {
		end = now.Add(in + close)
	}
	w := marketcal.Window{Open: open, Close: end}
	marketcal.SimulateSession(marketcal.MarketUSEquity, w)
	marketcal.SimulateSession(marketcal.MarketUSOptions, w)
	return now.Add(in)
}

// planDesk is a simulated desk on the option book holding qty of one SYNA
// call (strike, the fourth expiry), with the option's broker contract.
func planDesk(t *testing.T, strike float64, qty float64) (*simDesk, rpc.ContractParams) {
	t.Helper()
	now := time.Now()
	setSessions(t, true)
	book := twssim.OptionsBook(now)
	exps := twssim.OptionExpiries(now)
	id, ok := book.OptionConIDFor(planSYNA, exps[3], strike, "C")
	if !ok {
		t.Fatalf("no SYNA %s %v call listed", exps[3], strike)
	}
	in, _ := book.Contract(id)
	book.SetPosition(id, qty, in.Price*100*0.8)
	// A book long its stock, as an owner's is: a call sale lowers both the
	// stock's and the book's delta, so the exit passes the option cap as a
	// delta-reducing exit.
	book.SetPosition(planSYNA, 2000, 48)
	d := startSimDesk(t, twssim.Options{Book: book}, true)
	return d, rpc.ContractParams{ConID: id, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: exps[3], Strike: strike, Right: "C", Multiplier: 100, TradingClass: "SYNA"}
}

func (d *simDesk) optionPrice(conID int) float64 {
	in, _ := d.sim.Book().Contract(conID)
	return in.Price
}

// preparePlan is Desk's prepare over RPC.
func (d *simDesk) preparePlan(plan rpc.QueuedExitPlanParams) rpc.TradeProposalQueuePrepareResult {
	d.t.Helper()
	scope := d.srv.tradeProposals.currentScope()
	res, rerr := dispatchJSON[rpc.TradeProposalQueuePrepareResult](d.t, d.srv, rpc.MethodTradeProposalsQueuePrepare,
		rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode, ExitPlan: &plan})
	if rerr != nil {
		d.t.Fatalf("prepare: %+v", rerr)
	}
	return res
}

func (d *simDesk) armPlanWith(refs []string, digest string) rpc.TradeProposalQueueResult {
	d.t.Helper()
	res, rerr := dispatchJSON[rpc.TradeProposalQueueResult](d.t, d.srv, rpc.MethodTradeProposalsQueueArm,
		rpc.TradeProposalQueueArmParams{QueuedRefs: refs, PlanDigest: digest, DeskActionID: "desk-action-1", Credential: "passkey", Envelope: `{"signature":"synthetic"}`, Origin: rpc.OrderOriginAgent})
	if rerr != nil {
		d.t.Fatalf("arm: %+v", rerr)
	}
	return res
}

// queuePlan prepares and arms a plan and returns its records' ids, first
// sale (when present) then trail.
func (d *simDesk) queuePlan(plan rpc.QueuedExitPlanParams) []string {
	d.t.Helper()
	q := d.preparePlan(plan)
	if !q.Accepted || q.Plan == nil {
		d.t.Fatalf("plan prepare refused: %+v", q.Blockers)
	}
	res := d.armPlanWith(q.Plan.QueuedRefs, q.Plan.PlanDigest)
	if !res.Accepted || len(res.Queues) != len(q.Plan.Queues) {
		d.t.Fatalf("plan arm refused: %+v", res.Blockers)
	}
	var ids []string
	for _, v := range q.Plan.Queues {
		ids = append(ids, v.Terms.QueueID)
	}
	return ids
}

func planTrail(stop float64) *rpc.QueuedPlanTrail {
	return &rpc.QueuedPlanTrail{TrailPercent: 3, InitialStopPrice: stop}
}

// stock moves SYNA and wakes the executor, as the next tick would.
func (d *simDesk) stock(price float64) {
	d.sim.Book().SetPrice(planSYNA, price)
	d.srv.tradeProposals.Kick()
}

func (d *simDesk) waitRecord(what string, id string, budget time.Duration, ok func(queuedAuthRecord) bool) queuedAuthRecord {
	d.t.Helper()
	deadline := time.Now().Add(budget)
	for {
		rec := d.queuedRecord(id)
		if ok(rec) {
			return rec
		}
		if time.Now().After(deadline) {
			var lines []string
			for line := range strings.SplitSeq(d.log.String(), "\n") {
				if strings.Contains(line, "queued") || strings.Contains(line, "exit plan") || strings.Contains(line, "level=WARN") {
					lines = append(lines, line)
				}
			}
			var states []string
			for _, r := range d.srv.tradeProposals.queued.list() {
				states = append(states, fmt.Sprintf("%s %s %s hold=%s %q reason=%s %q watch=%+v", r.Terms.QueueID, r.Terms.Key, r.State, r.HoldCode, r.HoldReason, r.ReasonCode, r.Reason, r.Watch))
			}
			d.t.Fatalf("no %s within %s\nrecords:\n%s\nlog:\n%s", what, budget, strings.Join(states, "\n"), tail(strings.Join(lines, "\n"), 6000))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sells are the transmitted option sells, in order.
func (d *simDesk) sells(conID int) []twssim.PlaceOrder {
	var out []twssim.PlaceOrder
	for _, o := range d.realPlaces() {
		if o.ConID == conID && o.Action == "SELL" {
			out = append(out, o)
		}
	}
	return out
}

func (d *simDesk) brokerOrderFor(conID int, qty float64) twssim.Order {
	d.t.Helper()
	for _, o := range d.sim.Book().Orders() {
		if o.ConID == conID && o.Action == "SELL" && o.Quantity == qty {
			return o
		}
	}
	d.t.Fatalf("no broker sell of %g for %d", qty, conID)
	return twssim.Order{}
}

func requirePlainLimitSell(t *testing.T, o twssim.PlaceOrder, conID int, qty float64) {
	t.Helper()
	if o.ConID != conID || o.Action != "SELL" || o.Quantity != qty || o.OrderType != "LMT" || o.TIF != "DAY" || o.LimitPrice <= 0 || o.OutsideRTH {
		t.Fatalf("not a plain DAY limit sell of %g: %+v", qty, o)
	}
}

func (d *simDesk) watchView(id string) rpc.QueuedTrailWatch {
	d.t.Helper()
	st := d.srv.tradeProposals.QueueStatus(rpc.TradeProposalQueueStatusParams{QueueID: id})
	if st.Queue == nil || st.Queue.Watch == nil {
		d.t.Fatalf("no watch on %s: %+v", id, st)
	}
	return *st.Queue.Watch
}

// X1. Rally then give-back: the first sale sends 10 after the wait and
// fills; the stock rises and the line rises with it; the stock falls to the
// line and the trail sells the remaining 10, once.
func TestSimulatedTWSExitPlanRallyThenGiveBack(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	start := planStartIn(t, 8*time.Second, 0)
	minPrice := roundPrice(d.optionPrice(opt.ConID) * 0.9)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: minPrice}, Trail: planTrail(48.50)})
	first, trail := ids[0], ids[1]
	if w := d.watchView(trail); w.State != rpc.QueuedWatchWaiting || w.StopPrice != 48.50 {
		t.Fatalf("before the start: %+v", w)
	}
	d.waitRecord("the first sale to be sent", first, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	if time.Now().Before(start) {
		t.Fatal("sent before the wait ended")
	}
	sells := d.sells(opt.ConID)
	if len(sells) != 1 {
		t.Fatalf("first sale: %d sells", len(sells))
	}
	requirePlainLimitSell(t, sells[0], opt.ConID, 10)
	if sells[0].LimitPrice < minPrice {
		t.Fatalf("first sale below its minimum: %+v", sells[0])
	}
	if err := d.sim.Fill(d.brokerOrderFor(opt.ConID, 10).OrderID, 0, 0); err != nil {
		t.Fatal(err)
	}
	d.waitRecord("the first sale to fill", first, 30*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthFilled })
	d.waitRecord("the high to seed", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater >= 50 })
	d.stock(52)
	d.waitRecord("the stop to rise with the stock", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater == 52 && r.Watch.StopPrice == 50.44 })
	d.stock(53)
	rec := d.waitRecord("the stop to rise again", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.StopPrice == 51.41 })
	if w := d.watchView(trail); w.State != rpc.QueuedWatchWatching || w.HighWater != 53 || w.StockPrice != 53 {
		t.Fatalf("watching: %+v", w)
	}
	if n := len(d.sells(opt.ConID)); n != 1 || rec.State != rpc.QueuedAuthArmed {
		t.Fatalf("the rally sold: %d sells, %s", n, rec.State)
	}
	d.stock(51.40)
	rec = d.waitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	sells = d.sells(opt.ConID)
	if len(sells) != 2 {
		t.Fatalf("give-back: %d sells", len(sells))
	}
	requirePlainLimitSell(t, sells[1], opt.ConID, 10)
	if rec.Watch.TriggerPrice != 51.40 || rec.Watch.TriggeredAt.IsZero() || rec.QuantitySent != 10 {
		t.Fatalf("trigger: %+v sent %d", rec.Watch, rec.QuantitySent)
	}
	if w := d.watchView(trail); w.State != rpc.QueuedWatchTriggered || w.TriggerPrice != 51.40 {
		t.Fatalf("triggered view: %+v", w)
	}
	time.Sleep(12 * time.Second)
	if n := len(d.sells(opt.ConID)); n != 2 {
		t.Fatalf("the trail sold %d times", n-1)
	}
}

// X2. A red open: the stock is under the starting stop at the first check.
// The first sale is withdrawn unsent and one order sells all 20.
func TestSimulatedTWSExitPlanRedOpen(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 6*time.Second, 0)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(50.50)})
	first, trail := ids[0], ids[1]
	rec := d.waitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	if f := d.queuedRecord(first); f.State != rpc.QueuedAuthCancelled || f.ReasonCode != queuedReasonPlanStopTriggered || f.PreviewTokenID != "" {
		t.Fatalf("first sale: %s %s", f.State, f.ReasonCode)
	}
	sells := d.sells(opt.ConID)
	if len(sells) != 1 {
		t.Fatalf("red open: %d sells", len(sells))
	}
	requirePlainLimitSell(t, sells[0], opt.ConID, 20)
	if rec.Watch.TriggerPrice > 50.50 || rec.Watch.StopPrice != 50.50 {
		t.Fatalf("trigger %+v", rec.Watch)
	}
}

// X3. A steady rise to the close: the first sale sends; the trail never
// fires and expires at the window's end with the high recorded.
func TestSimulatedTWSExitPlanSteadyRiseToTheClose(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 6*time.Second, 40*time.Second)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(48.50)})
	first, trail := ids[0], ids[1]
	d.waitRecord("the first sale to be sent", first, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	for _, p := range []float64{50.5, 51, 51.5} {
		d.stock(p)
		d.waitRecord("the high to follow", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater == p })
	}
	rec := d.waitRecord("the trail to expire at the close", trail, 90*time.Second, func(r queuedAuthRecord) bool { return r.final() })
	if rec.State != rpc.QueuedAuthExpired || rec.ReasonCode != queuedReasonWindowEnded || rec.Watch.HighWater != 51.5 || !rec.Watch.TriggeredAt.IsZero() {
		t.Fatalf("at the close: %s %s %+v", rec.State, rec.ReasonCode, rec.Watch)
	}
	if n := len(d.sells(opt.ConID)); n != 1 {
		t.Fatalf("%d sells", n)
	}
	if w := d.watchView(trail); w.HighWater != 51.5 || w.State != rpc.QueuedWatchUnwatched || w.ReasonCode != queuedReasonWindowEnded {
		t.Fatalf("ended view %+v", w)
	}
}

// X4. The first sale rests unfilled when the line is crossed: its order is
// cancelled at the broker first, then one sale of what is held; two sells
// never work together.
func TestSimulatedTWSExitPlanCancelsTheRestingFirstSale(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 6*time.Second, 0)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(48.50)})
	first, trail := ids[0], ids[1]
	d.waitRecord("the first sale to be sent", first, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	resting := d.brokerOrderFor(opt.ConID, 10)
	d.stock(48)
	d.waitRecord("the trail to hold for the cancel", trail, 60*time.Second, func(r queuedAuthRecord) bool {
		return !r.Watch.TriggeredAt.IsZero() && (r.HoldCode == queuedHoldFirstSaleCancelling || r.State == rpc.QueuedAuthSent)
	})
	rec := d.waitRecord("the trail to sell", trail, 90*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	if f := d.queuedRecord(first); f.State != rpc.QueuedAuthExpiredUnfilled {
		t.Fatalf("first sale %s %s", f.State, f.ReasonCode)
	}
	if o := brokerOrder(t, d.sim, resting.OrderID); o.Status != "Cancelled" {
		t.Fatalf("resting first sale %s", o.Status)
	}
	sells := d.sells(opt.ConID)
	if len(sells) != 2 || d.cancels() != 1 || rec.QuantitySent != 20 {
		t.Fatalf("%d sells, %d cancels, sent %d", len(sells), d.cancels(), rec.QuantitySent)
	}
	requirePlainLimitSell(t, sells[1], opt.ConID, 20)
	// The cancel went out before the trail's sale.
	frames := d.sim.Recorder().Frames()
	cancelAt := slices.IndexFunc(frames, func(f twssim.Frame) bool { return f.Name == "cancelOrder" })
	saleAt := slices.IndexFunc(frames, func(f twssim.Frame) bool {
		return f.Order != nil && !f.Order.WhatIf && f.Order.ConID == opt.ConID && f.Order.Quantity == 20
	})
	if cancelAt < 0 || saleAt < cancelAt {
		t.Fatalf("cancel frame %d, sale frame %d", cancelAt, saleAt)
	}
}

// X5. The first sale's minimum is never met: it expires unsent, and the
// trail still covers all 20.
func TestSimulatedTWSExitPlanFirstSaleMinimumNeverMet(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 6*time.Second, 0)
	high := roundPrice(d.optionPrice(opt.ConID) * 3)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: high}, Trail: planTrail(48.50)})
	first, trail := ids[0], ids[1]
	d.waitRecord("the first sale to wait for its minimum", first, 60*time.Second, func(r queuedAuthRecord) bool { return r.HoldCode == queuedHoldWorstPrice })
	// Its signed window ends early in the fixture, as at its hour's end.
	end := time.Now().Add(3 * time.Second)
	if err := d.srv.tradeProposals.queued.update(t.Context(), time.Now(), func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		r := records[first]
		r.Terms.NotAfter = end.UTC()
		r.TermsDigest, _ = queuedTermsDigest(r.Terms)
		return []queuedAuthEvent{r.note("fixture", r.State, time.Now())}
	}); err != nil {
		t.Fatal(err)
	}
	d.waitRecord("the first sale to expire unsent", first, 40*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthExpired })
	if n := len(d.sells(opt.ConID)); n != 0 {
		t.Fatalf("%d sells before the trail", n)
	}
	d.stock(48)
	rec := d.waitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	sells := d.sells(opt.ConID)
	if len(sells) != 1 || rec.QuantitySent != 20 {
		t.Fatalf("%d sells, sent %d", len(sells), rec.QuantitySent)
	}
	requirePlainLimitSell(t, sells[0], opt.ConID, 20)
}

// X6. A trail-only plan in session starts at once. A stale, delayed,
// one-sided or missing stock quote leaves it unwatched: nothing is sent,
// even with the stock under the line, and it resumes when quotes return.
func TestSimulatedTWSExitPlanUnusableStockQuote(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, -10*time.Minute, 0)
	if err := d.sim.SetFeed(planSYNA, twssim.FeedNone); err != nil {
		t.Fatal(err)
	}
	d.sim.Book().SetPrice(planSYNA, 47)
	before := time.Now()
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})
	trail := ids[0]
	if rec := d.queuedRecord(trail); rec.Terms.NotBefore.Before(before.Add(-time.Second)) || rec.Terms.NotBefore.After(time.Now()) {
		t.Fatalf("a trail-only plan in session starts at once: %s", rec.Terms.NotBefore)
	}
	for _, feed := range []string{twssim.FeedNone, twssim.FeedDelayed, twssim.FeedOneSided} {
		if err := d.sim.SetFeed(planSYNA, feed); err != nil {
			t.Fatal(err)
		}
		d.srv.tradeProposals.Kick()
		rec := d.waitRecord("the trail held unwatched ("+feed+")", trail, 40*time.Second, func(r queuedAuthRecord) bool { return r.HoldCode == queuedHoldStockQuote })
		time.Sleep(12 * time.Second)
		rec = d.queuedRecord(trail)
		if w := d.watchView(trail); w.State != rpc.QueuedWatchUnwatched || w.ReasonCode != queuedHoldStockQuote || rec.State != rpc.QueuedAuthHeld || !rec.Watch.TriggeredAt.IsZero() {
			t.Fatalf("%s: %+v %s", feed, w, rec.State)
		}
		if n := len(d.sells(opt.ConID)); n != 0 {
			t.Fatalf("%s: sold on an unusable quote", feed)
		}
		live, _ := d.srv.tradeProposals.trailLiveFor(trail)
		t.Logf("%s: %s", feed, live.reason)
	}
	if err := d.sim.SetFeed(planSYNA, twssim.FeedLive); err != nil {
		t.Fatal(err)
	}
	d.srv.tradeProposals.Kick()
	rec := d.waitRecord("the trail to resume and sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	if rec.Watch.TriggerPrice != 47 || len(d.sells(opt.ConID)) != 1 {
		t.Fatalf("resumed: %+v, %d sells", rec.Watch, len(d.sells(opt.ConID)))
	}
}

// X7. One bad print below the line for a single check sells nothing.
func TestSimulatedTWSExitPlanOneBadPrint(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, -10*time.Minute, 0)
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	d.waitRecord("the high to seed", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater == 50 })
	d.sim.Book().SetPrice(planSYNA, 47)
	d.srv.tradeProposals.Kick()
	d.waitFor("one read below the line", 30*time.Second, func() bool {
		live, ok := d.srv.tradeProposals.trailLiveFor(trail)
		return ok && !live.breachAt.IsZero()
	})
	d.sim.Book().SetPrice(planSYNA, 50)
	time.Sleep(15 * time.Second)
	rec := d.queuedRecord(trail)
	live, _ := d.srv.tradeProposals.trailLiveFor(trail)
	if n := len(d.sells(opt.ConID)); n != 0 || !rec.Watch.TriggeredAt.IsZero() || !live.breachAt.IsZero() || rec.State != rpc.QueuedAuthArmed {
		t.Fatalf("a single bad print: %d sells, %+v, breach %s, %s", n, rec.Watch, live.breachAt, rec.State)
	}
}

// restartSimDesk stops d's daemon and starts another on the same state, as
// a daemon restart does, against the same simulator.
func restartSimDesk(t *testing.T, d *simDesk, stop func(), db string) (*simDesk, func()) {
	t.Helper()
	stop()
	return startSimDaemon(t, d.sim, db)
}

// startSimDaemon starts a daemon on an existing simulator and state
// database, as startSimDeskPolicies does, and returns its stop.
func startSimDaemon(t *testing.T, sim *twssim.Server, db string) (*simDesk, func()) {
	t.Helper()
	port := sim.Port()
	cfg, err := (&config.Config{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: new(15), Account: SimulatorAccount, TLS: new(false)},
		Trading: config.Trading{Mode: "live"},
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg.CashLedger.URL = "http://127.0.0.1:1/never"
	cfg.Daemon.SetIdleTimeout(0)
	var out *lockedBuffer
	var srv *Server
	var cancel context.CancelFunc
	started := make(chan error, 1)
	// The stopped daemon gives its instance lock back as it winds down; the
	// next one starts once it has, never beside it.
	for attempt := 0; ; attempt++ {
		out = &lockedBuffer{}
		srv = New(Options{Config: cfg, SocketPath: filepath.Join(filepath.Dir(db), "ibkr.sock"), Version: "v3.44.1", Logger: NewLogger(out, "info"),
			StateDatabasePath: db, DisableMacroSources: true, Simulator: true})
		serving := make(chan struct{}, 1)
		srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		go func() { started <- srv.Start(ctx) }()
		select {
		case <-serving:
		case err := <-started:
			cancel()
			if errors.Is(err, ErrAlreadyRunning) && attempt < 40 {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			t.Fatalf("daemon stopped at start: %v\n%s", err, out.String())
		case <-time.After(60 * time.Second):
			cancel()
			t.Fatal("daemon did not start")
		}
		break
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	}
	t.Cleanup(stop)
	d := &simDesk{t: t, sim: sim, srv: srv, log: out}
	d.waitFor("a ready session against the simulator", 30*time.Second, func() bool {
		c := srv.gatewayConnectorForTest()
		return c != nil && c.IsReady() && srv.postConnectSetupDone.Load()
	})
	dispatchedRegime(t, srv)
	return d, stop
}

// X8. A daemon restart mid-watch: the high water and the stop survive, the
// stop is not lowered, and nothing is sent twice.
func TestSimulatedTWSExitPlanSurvivesARestart(t *testing.T) {
	requireSimTier(t)
	planStartIn(t, -10*time.Minute, 0)
	now := time.Now()
	book := twssim.OptionsBook(now)
	exps := twssim.OptionExpiries(now)
	id, _ := book.OptionConIDFor(planSYNA, exps[3], 50, "C")
	book.SetPosition(id, 20, 250)
	book.SetPosition(planSYNA, 2000, 48)
	sim := twssim.Start(t, twssim.Options{Book: book})
	dir := shortTempDir(t)
	for key, sub := range map[string]string{"XDG_STATE_HOME": "state", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "HOME": "home"} {
		t.Setenv(key, filepath.Join(dir, sub))
	}
	if err := WriteSimulatorPolicies(config.PolicyDir(), sim.Book().Base()); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "daemon.db")
	d, stop := startSimDaemon(t, sim, db)
	opt := rpc.ContractParams{ConID: id, Symbol: "SYNA", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: exps[3], Strike: 50, Right: "C", Multiplier: 100, TradingClass: "SYNA"}
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	d.stock(53)
	d.waitRecord("the stop to rise", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.StopPrice == 51.41 })
	d, stop = restartSimDesk(t, d, stop, db)
	if rec := d.queuedRecord(trail); rec.Watch.HighWater != 53 || rec.Watch.StopPrice != 51.41 || !rec.waiting() {
		t.Fatalf("after the restart: %+v %s", rec.Watch, rec.State)
	}
	// Below the high, above the line: the stop the high set stands, not one
	// from this lower price.
	d.stock(52)
	d.waitFor("a read after the restart", 30*time.Second, func() bool {
		live, ok := d.srv.tradeProposals.trailLiveFor(trail)
		return ok && live.price == 52
	})
	if rec := d.queuedRecord(trail); rec.Watch.StopPrice != 51.41 || rec.Watch.HighWater != 53 || len(d.sells(opt.ConID)) != 0 {
		t.Fatalf("after a lower read: %+v", rec.Watch)
	}
	d.stock(51)
	d.waitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	d, _ = restartSimDesk(t, d, stop, db)
	time.Sleep(15 * time.Second)
	if n := len(d.sells(opt.ConID)); n != 1 {
		t.Fatalf("sent %d times across restarts", n)
	}
	if rec := d.queuedRecord(trail); rec.State != rpc.QueuedAuthSent && rec.State != rpc.QueuedAuthFilled && rec.State != rpc.QueuedAuthExpiredUnfilled {
		t.Fatalf("after the second restart: %s %s", rec.State, rec.ReasonCode)
	}
}

// X9. The option's spread is too wide at the trigger: the trail holds, stays
// latched while the stock recovers, and sells when the spread narrows.
func TestSimulatedTWSExitPlanWideSpreadAtTheTrigger(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, -10*time.Minute, 0)
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	if err := d.sim.SetSpread(opt.ConID, 60); err != nil {
		t.Fatal(err)
	}
	d.stock(48)
	d.waitRecord("the trail to hold on the spread", trail, 60*time.Second, func(r queuedAuthRecord) bool {
		return !r.Watch.TriggeredAt.IsZero() && r.HoldCode == rpc.ReadinessSpreadTooWide
	})
	d.stock(51)
	time.Sleep(12 * time.Second)
	rec := d.queuedRecord(trail)
	if len(d.sells(opt.ConID)) != 0 || rec.Watch.TriggeredAt.IsZero() || !rec.waiting() {
		t.Fatalf("recovered stock: %+v %s, %d sells", rec.Watch, rec.State, len(d.sells(opt.ConID)))
	}
	if err := d.sim.SetSpread(opt.ConID, 0); err != nil {
		t.Fatal(err)
	}
	d.srv.tradeProposals.Kick()
	rec = d.waitRecord("the trail to sell once the spread narrows", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	sells := d.sells(opt.ConID)
	if len(sells) != 1 || rec.QuantitySent != 20 {
		t.Fatalf("%d sells, sent %d", len(sells), rec.QuantitySent)
	}
	requirePlainLimitSell(t, sells[0], opt.ConID, 20)
}

// X10. The owner cancels before the trigger: cancelling one record leaves
// the other as it is, and nothing is sent once both are cancelled.
func TestSimulatedTWSExitPlanOwnerCancels(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 8*time.Second, 0)
	ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(50.50)})
	first, trail := ids[0], ids[1]
	e := d.srv.tradeProposals
	if res, err := e.QueueCancel(t.Context(), rpc.TradeProposalQueueCancelParams{QueueID: first, Origin: rpc.OrderOriginAgent}); err != nil || !res.Accepted {
		t.Fatalf("cancel first sale: %+v %v", res, err)
	}
	if rec := d.queuedRecord(trail); rec.State != rpc.QueuedAuthArmed {
		t.Fatalf("the trail after its first sale was cancelled: %s", rec.State)
	}
	if res, err := e.QueueCancel(t.Context(), rpc.TradeProposalQueueCancelParams{QueueID: trail, Origin: rpc.OrderOriginAgent}); err != nil || !res.Accepted {
		t.Fatalf("cancel trail: %+v %v", res, err)
	}
	time.Sleep(25 * time.Second)
	if n := len(d.realPlaces()); n != 0 {
		t.Fatalf("sent %d after the owner cancelled", n)
	}
	for _, id := range ids {
		if rec := d.queuedRecord(id); rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonOwnerCancelled {
			t.Fatalf("%s: %s %s", id, rec.State, rec.ReasonCode)
		}
	}
}

// X11. A hand order working on the same contract holds the trail as today;
// once it is cancelled the trail sells what is held.
func TestSimulatedTWSExitPlanHandOrderHolds(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, -10*time.Minute, 0)
	high := roundPrice(d.optionPrice(opt.ConID) * 3)
	prev, rerr := dispatchJSON[rpc.OrderPreviewResult](t, d.srv, rpc.MethodOrderPreview, rpc.OrderPreviewParams{Action: "SELL", Contract: opt, Quantity: 5, OrderType: "LMT", LimitPrice: &high, Strategy: "explicit-limit", TIF: "DAY"})
	if rerr != nil || !prev.SubmitEligible {
		t.Fatalf("hand preview: %+v %+v", rerr, prev)
	}
	placed, rerr := d.place(prev.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("hand place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.LifecycleStatus == rpc.OrderLifecycleSubmitted })
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	d.stock(48)
	d.waitRecord("the trail to hold behind the hand order", trail, 60*time.Second, func(r queuedAuthRecord) bool {
		return !r.Watch.TriggeredAt.IsZero() && r.HoldCode == queuedHoldHandOrder
	})
	if n := len(d.sells(opt.ConID)); n != 1 {
		t.Fatalf("%d sells beside the hand order", n)
	}
	if _, rerr := dispatchJSON[rpc.OrderCancelResult](t, d.srv, rpc.MethodOrderCancel, rpc.OrderCancelParams{ID: placed.OrderRef, Origin: rpc.OrderOriginAgent}); rerr != nil {
		t.Fatalf("hand cancel: %+v", rerr)
	}
	rec := d.waitRecord("the trail to sell", trail, 90*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent })
	sells := d.sells(opt.ConID)
	if len(sells) != 2 || rec.QuantitySent != 20 {
		t.Fatalf("%d sells, sent %d", len(sells), rec.QuantitySent)
	}
	requirePlainLimitSell(t, sells[1], opt.ConID, 20)
}

// X12. A 200-call line above both order caps passes as a delta-reducing
// exit. While the exemption fails closed (here the stock is held short
// beside the calls, so the sale would raise the line's absolute delta) the
// trail holds under the cap and
// retries each cycle; it is never cancelled, and it sells the whole line
// once the exemption passes.
func TestSimulatedTWSExitPlanLargeLine(t *testing.T) {
	d, opt := planDesk(t, 50, 200)
	planStartIn(t, -10*time.Minute, 0)
	limits := d.srv.orderLimitsInForce("USD")
	notional := 200 * d.optionPrice(opt.ConID) * 100
	t.Logf("caps: %d contracts, %g notional; the line: 200 at %g (%g)", limits.MaxOptionContracts, limits.CapBase, d.optionPrice(opt.ConID), notional)
	if limits.MaxOptionContracts >= 200 || limits.CapBase >= notional {
		t.Fatalf("the line is not above both caps: %+v", limits)
	}
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	d.sim.Book().SetPosition(planSYNA, -12000, 48)
	d.sim.PushAccount()
	d.stock(48)
	rec := d.waitRecord("the trail to hold under the cap", trail, 60*time.Second, func(r queuedAuthRecord) bool {
		return !r.Watch.TriggeredAt.IsZero() && r.HoldCode == "order_risk_limit"
	})
	t.Logf("held: %s", rec.HoldReason)
	time.Sleep(15 * time.Second)
	if rec = d.queuedRecord(trail); !rec.waiting() || len(d.sells(opt.ConID)) != 0 {
		t.Fatalf("under the cap: %s %s, %d sells", rec.State, rec.ReasonCode, len(d.sells(opt.ConID)))
	}
	d.sim.Book().SetPosition(planSYNA, 2000, 48)
	d.sim.PushAccount()
	d.srv.tradeProposals.Kick()
	rec = d.waitRecord("the trail to sell the line", trail, 120*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent || r.final() })
	sells := d.sells(opt.ConID)
	if rec.State != rpc.QueuedAuthSent || len(sells) != 1 || rec.QuantitySent != 200 {
		t.Fatalf("%s %s %s: %d sells, sent %d", rec.State, rec.ReasonCode, rec.Reason, len(sells), rec.QuantitySent)
	}
	requirePlainLimitSell(t, sells[0], opt.ConID, 200)
}

// X13. The position is reduced by hand before the trigger: the trail sells
// only what is held. A flat line ends the plan.
func TestSimulatedTWSExitPlanPositionChangedByHand(t *testing.T) {
	t.Run("reduced", func(t *testing.T) {
		d, opt := planDesk(t, 50, 20)
		planStartIn(t, -10*time.Minute, 0)
		trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
		d.sim.Book().SetPosition(opt.ConID, 12, 250)
		d.sim.PushAccount()
		time.Sleep(2 * time.Second)
		d.stock(48)
		rec := d.waitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool { return r.State == rpc.QueuedAuthSent || r.final() })
		sells := d.sells(opt.ConID)
		if rec.State != rpc.QueuedAuthSent || len(sells) != 1 || rec.QuantitySent != 12 {
			t.Fatalf("%s %s: %d sells, sent %d", rec.State, rec.ReasonCode, len(sells), rec.QuantitySent)
		}
		requirePlainLimitSell(t, sells[0], opt.ConID, 12)
	})
	t.Run("flat", func(t *testing.T) {
		d, opt := planDesk(t, 50, 20)
		planStartIn(t, 30*time.Second, 0)
		ids := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 9999}, Trail: planTrail(48.50)})
		d.sim.Book().ClosePosition(opt.ConID)
		d.sim.PushAccount()
		for _, id := range ids {
			rec := d.waitRecord("the plan to end", id, 90*time.Second, func(r queuedAuthRecord) bool { return r.final() })
			if rec.State != rpc.QueuedAuthCancelled || rec.ReasonCode != queuedReasonPositionChanged {
				t.Fatalf("%s: %s %s", id, rec.State, rec.ReasonCode)
			}
		}
		if n := len(d.realPlaces()); n != 0 {
			t.Fatalf("%d sent for a closed line", n)
		}
	})
}

// X14. Plan arm is all-or-nothing: part of the plan, a wrong plan digest or
// the single-record form arms nothing; a third record for the contract and
// side is refused.
func TestSimulatedTWSExitPlanArmIsAllOrNothing(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	planStartIn(t, 10*time.Minute, 0)
	q := d.preparePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(48.50)})
	if !q.Accepted || q.Plan == nil || len(q.Plan.Queues) != 2 || len(q.Plan.QueuedRefs) != 2 || q.Queue != nil || q.QueuedRef != "" {
		t.Fatalf("prepare: %+v", q)
	}
	digests := []string{q.Plan.Queues[0].TermsDigest, q.Plan.Queues[1].TermsDigest}
	if q.Plan.PlanDigest != exitPlanDigest(digests) || q.Plan.Queues[0].Terms.OwnerOrder.Plan.Role != rpc.QueuedPlanRoleFirstSale {
		t.Fatalf("plan digest or order: %+v", q.Plan)
	}
	ft, tt := q.Plan.Queues[0].Terms, q.Plan.Queues[1].Terms
	st := tt.OwnerOrder.StockTrail
	if ft.Version != 3 || tt.Version != 3 || ft.Style != "midpoint_bounded" || ft.MaxQuantity != 10 || ft.WorstPrice != 0.05 || tt.Style != rpc.QueuedAuthStyleStockTrail ||
		tt.MaxQuantity != 20 || tt.PositionQuantity != 20 || tt.WorstPrice != 0 || st == nil || st.Underlying.ConID != planSYNA || st.ReferencePrice != 50 ||
		!ft.NotBefore.Equal(tt.NotBefore) || !ft.NotAfter.Equal(ft.NotBefore.Add(time.Hour)) || tt.NotAfter.Before(ft.NotAfter) {
		t.Fatalf("terms: %+v\n%+v", ft, tt)
	}
	for name, res := range map[string]rpc.TradeProposalQueueResult{
		"one record":   d.armPlanWith(q.Plan.QueuedRefs[1:], exitPlanDigest(digests[1:])),
		"wrong digest": d.armPlanWith(q.Plan.QueuedRefs, exitPlanDigest([]string{digests[1], digests[0]})),
		"single form": func() rpc.TradeProposalQueueResult {
			res, _ := d.srv.tradeProposals.QueueArm(t.Context(), rpc.TradeProposalQueueArmParams{QueuedRef: q.Plan.QueuedRefs[1], TermsDigest: digests[1]})
			return res
		}(),
	} {
		if res.Accepted || len(res.Blockers) == 0 {
			t.Fatalf("%s armed: %+v", name, res)
		}
		for _, v := range q.Plan.Queues {
			if rec := d.queuedRecord(v.Terms.QueueID); rec.State != rpc.QueuedAuthPrepared {
				t.Fatalf("%s: %s left %s", name, v.Terms.QueueID, rec.State)
			}
		}
		t.Logf("%s: %s", name, res.Blockers[0].Code)
	}
	res := d.armPlanWith(q.Plan.QueuedRefs, q.Plan.PlanDigest)
	if !res.Accepted || len(res.Queues) != 2 {
		t.Fatalf("arm: %+v", res.Blockers)
	}
	for _, v := range res.Queues {
		if v.State != rpc.QueuedAuthArmed {
			t.Fatalf("armed view %s", v.State)
		}
	}
	// A third record for the contract and side: another plan, or an owner
	// order.
	again := d.preparePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})
	if again.Accepted || len(again.Blockers) == 0 || again.Blockers[0].Code != queuedIntentExistsCode {
		t.Fatalf("a second plan: %+v", again)
	}
	scope := d.srv.tradeProposals.currentScope()
	limit := 0.05
	order, err := d.srv.tradeProposals.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode,
		Order: &rpc.OrderPreviewParams{Contract: opt, Action: "SELL", Quantity: 5, OrderType: "LMT", TIF: "DAY", LimitPrice: &limit}})
	if err != nil || order.Accepted || len(order.Blockers) == 0 || order.Blockers[0].Code != queuedIntentExistsCode {
		t.Fatalf("an owner order beside the plan: %+v %v", order, err)
	}
	// A first sale while the session is past the wait is refused.
	planStartIn(t, -time.Minute, 0)
	late := d.preparePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, FirstSale: &rpc.QueuedPlanFirstSale{Quantity: 10, MinPrice: 0.05}, Trail: planTrail(48.50)})
	if late.Accepted || late.Blockers[0].Code != "queue_not_offered" {
		t.Fatalf("a first sale past the wait: %+v", late)
	}
	short := d.preparePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: 10, Trail: planTrail(48.50)})
	if short.Accepted || short.Blockers[0].Code != "queue_wait_invalid" {
		t.Fatalf("a wait below the opening offset: %+v", short)
	}
}
