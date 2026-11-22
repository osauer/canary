//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// simSmoke are the simulator scenarios the normal gate runs (about 27 s);
// every other scenario runs under `make test-sim` (CANARY_TEST_SIM=1),
// because together they take minutes and the daemon's trading leg is in
// every `make check`. The full leg's plan covers the plan scenario.
var simSmoke = map[string]bool{
	"TestSimulatedTWSAddsToAPositionEndToEnd": true,
	"TestSimulatedTWSRefusesAnotherAccount":   true,
	"TestSimulatedTWSOptionCallEntrySmoke":    true,
}

// requireSimTier skips a simulator scenario outside `make test-sim` unless it
// is one of the smoke scenarios. Scenarios still compile in every build.
func requireSimTier(t *testing.T) {
	t.Helper()
	root, _, _ := strings.Cut(t.Name(), "/")
	if simSmoke[root] || os.Getenv("CANARY_TEST_SIM") == "1" {
		return
	}
	t.Skip("simulator scenario: run it with make test-sim")
}

// simDesk is the real daemon in simulator mode (live, the placeholder
// account) against a TWS simulator, with its own XDG roots, socket and state
// database and the synthetic owner's reviewed policies.
type simDesk struct {
	t   *testing.T
	sim *twssim.Server
	srv *Server
	log *lockedBuffer
}

// startSimDesk starts the daemon pinned to SimulatorAccount. With ready it
// waits for a ready session and publishes the regime once, as a client's
// first regime read does.
func startSimDesk(t *testing.T, opts twssim.Options, ready bool) *simDesk {
	t.Helper()
	requireSimTier(t)
	sim := twssim.Start(t, opts)
	dir := shortTempDir(t)
	for key, sub := range map[string]string{"XDG_STATE_HOME": "state", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "HOME": "home"} {
		t.Setenv(key, filepath.Join(dir, sub))
	}
	if err := WriteSimulatorPolicies(config.PolicyDir(), sim.Book().Base()); err != nil {
		t.Fatal(err)
	}
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
	out := &lockedBuffer{}
	srv := New(Options{
		Config: cfg, SocketPath: filepath.Join(dir, "ibkr.sock"), Version: "v3.44.1",
		Logger: NewLogger(out, "info"), StateDatabasePath: filepath.Join(dir, "daemon.db"),
		DisableMacroSources: true, Simulator: true,
	})
	serving := make(chan struct{}, 1)
	srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan error, 1)
	go func() { started <- srv.Start(ctx) }()
	select {
	case <-serving:
	case err := <-started:
		cancel()
		t.Fatalf("daemon stopped at start: %v\n%s", err, out.String())
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatal("daemon did not start")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-started:
		case <-time.After(30 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	d := &simDesk{t: t, sim: sim, srv: srv, log: out}
	if !ready {
		return d
	}
	d.waitFor("a ready session against the simulator", 30*time.Second, func() bool {
		c := srv.gatewayConnectorForTest()
		return c != nil && c.IsReady() && srv.postConnectSetupDone.Load()
	})
	dispatchedRegime(t, srv)
	return d
}

func (d *simDesk) waitFor(what string, budget time.Duration, ok func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(budget)
	for !ok() {
		if time.Now().After(deadline) {
			d.t.Fatalf("no %s within %s\nlog tail:\n%s", what, budget, tail(d.log.String(), 4000))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func tail(s string, n int) string { return s[max(0, len(s)-n):] }

func dispatchJSON[T any](t *testing.T, srv *Server, method string, params any) (T, *rpc.Error) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	res := dispatchRaw(t, srv, method, string(raw))
	var out T
	if res.Ok {
		if err := json.Unmarshal(res.Result, &out); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	return out, res.Error
}

func (d *simDesk) plan(p rpc.AddParams) rpc.AddPlanResult {
	d.t.Helper()
	plan, rerr := dispatchJSON[rpc.AddPlanResult](d.t, d.srv, rpc.MethodAddPlan, p)
	if rerr != nil {
		d.t.Fatalf("add.plan: %+v", rerr)
	}
	return plan
}

func (d *simDesk) preview(p rpc.AddParams) (rpc.OrderPreviewResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.OrderPreviewResult](d.t, d.srv, rpc.MethodAddPreview, p)
}

// place is Desk's production call: an agent-origin order.place with the
// preview token and the reviewed Add warnings accepted.
func (d *simDesk) place(token string) (rpc.OrderPlaceResult, *rpc.Error) {
	d.t.Helper()
	return dispatchJSON[rpc.OrderPlaceResult](d.t, d.srv, rpc.MethodOrderPlace, rpc.OrderPlaceParams{PreviewToken: token, AcceptAddWarnings: true, Origin: rpc.OrderOriginAgent})
}

func (d *simDesk) status(ref string) rpc.OrderStatusResult {
	d.t.Helper()
	st, rerr := dispatchJSON[rpc.OrderStatusResult](d.t, d.srv, rpc.MethodOrderStatus, rpc.OrderStatusParams{ID: ref})
	if rerr != nil {
		d.t.Fatalf("order.status %s: %+v", ref, rerr)
	}
	return st
}

// realPlaces are the transmitted (non-WhatIf) placeOrder frames the daemon sent.
func (d *simDesk) realPlaces() []twssim.PlaceOrder {
	var out []twssim.PlaceOrder
	for _, f := range d.sim.Recorder().Frames() {
		if f.Order != nil && !f.Order.WhatIf {
			out = append(out, *f.Order)
		}
	}
	return out
}

func (d *simDesk) allPlaces() []twssim.Frame {
	var out []twssim.Frame
	for _, f := range d.sim.Recorder().Frames() {
		if f.Order != nil {
			out = append(out, f)
		}
	}
	return out
}

var synaAdd = rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, LimitPrice: 50.10, Quantity: 10}

// add.plan sizes the addition from the broker's own account, positions,
// settled cash, open orders, contract and a WhatIf per candidate quantity;
// the WhatIf frames are read back by the broker's own field table.
func TestSimulatedTWSPlansAnAdditionFromBrokerEvidence(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	if mgr := d.srv.riskPolicies.snapshot(); mgr.policy == nil || mgr.review == rpc.PolicyReviewUnreviewed {
		t.Fatalf("risk policy %s at %s: %s", mgr.status, mgr.path, mgr.message)
	}
	plan := d.plan(synaAdd)
	if len(plan.Blockers) > 0 || plan.Quantity != 10 || plan.Contract.ConID != 900001 || plan.Before != 100 {
		t.Fatalf("add.plan: quantity %d of conid %d, held %v before, blockers %+v", plan.Quantity, plan.Contract.ConID, plan.Before, plan.Blockers)
	}
	frames := d.allPlaces()
	if len(frames) == 0 {
		t.Fatal("add.plan sent no WhatIf placeOrder")
	}
	for _, f := range frames {
		o := *f.Order
		if len(f.IgnoredByBroker) > 0 {
			t.Errorf("placeOrder fields the broker drops: %v", f.IgnoredByBroker)
		}
		if !o.WhatIf || !o.Transmit || o.Action != "BUY" || o.Quantity != 10 || o.OrderType != "LMT" || o.LimitPrice != 50.10 || o.TIF != "DAY" || o.OutsideRTH ||
			o.Account != SimulatorAccount || o.ConID != 900001 || o.Symbol != "SYNA" || o.SecType != "STK" || o.Currency != "USD" || o.ClientID != 15 {
			t.Fatalf("WhatIf placeOrder frame: %+v", o)
		}
	}
	if orders := d.sim.Book().Orders(); len(orders) != 0 {
		t.Fatalf("a WhatIf left an order at the broker: %+v", orders)
	}
}

// The owner's question for 2026-11-23, end to end: plan, preview (with the
// broker's WhatIf), Desk's place call, the broker's acknowledgement and fill,
// and the daemon's order record filled. The one transmitted placeOrder frame
// is read back by the broker's own field table.
func TestSimulatedTWSAddsToAPositionEndToEnd(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	cash := d.sim.Book().Cash("USD")
	preview, rerr := d.preview(synaAdd)
	if rerr != nil || !preview.TokenMinted || !preview.Executable {
		t.Fatalf("add.preview: %+v minted=%t executable=%t", rerr, preview.TokenMinted, preview.Executable)
	}
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil || !placed.Accepted || placed.Mode != "live" || placed.Account != SimulatorAccount {
		t.Fatalf("order.place: %+v %+v", rerr, placed)
	}
	var st rpc.OrderStatusResult
	d.waitFor("a filled order record", 15*time.Second, func() bool {
		st = d.status(placed.OrderRef)
		return st.Found && st.Order.Status == "Filled"
	})
	if st.Order.Filled != 10 || st.Order.AvgFillPrice != 50.01 || st.Order.Remaining != 0 {
		t.Fatalf("order record: %+v", st.Order)
	}
	sent := d.realPlaces()
	if len(sent) != 1 {
		t.Fatalf("transmitted placeOrder frames: %d, want one", len(sent))
	}
	o := sent[0]
	if o.WhatIf || !o.Transmit || o.Action != "BUY" || o.Quantity != 10 || o.OrderType != "LMT" || o.LimitPrice != 50.10 || o.TIF != "DAY" || o.OutsideRTH ||
		o.Account != SimulatorAccount || o.ConID != 900001 || o.Symbol != "SYNA" || o.SecType != "STK" || o.Currency != "USD" || o.ClientID != 15 ||
		o.OrderRef != placed.OrderRef || o.OrderID != placed.ReservedOrderID || o.ParentID != 0 || o.AttachedOrders {
		t.Fatalf("the transmitted placeOrder frame: %+v", o)
	}
	if p, ok := d.sim.Book().Position(900001); !ok || p.Quantity != 110 {
		t.Fatalf("broker position after the fill: %+v", p)
	}
	ex := d.sim.Book().Executions()
	if len(ex) != 1 || d.sim.Book().Cash("USD") != cash-10*50.01-ex[0].Fee {
		t.Fatalf("broker cash %v after %+v", d.sim.Book().Cash("USD"), ex)
	}
	hist, rerr := dispatchJSON[rpc.OrdersHistoryResult](t, d.srv, rpc.MethodOrdersHistory, rpc.OrdersHistoryParams{Limit: 10})
	if rerr != nil || len(hist.Orders) != 1 || hist.Orders[0].Order.OrderRef != placed.OrderRef || hist.Orders[0].Order.Status != "Filled" {
		t.Fatalf("orders.history: %+v %+v", rerr, hist.Orders)
	}
}

// Safety: the config pins the placeholder account and the broker announces
// another. Nothing may reach the wire: plan, preview and place are refused
// and the simulator records no placeOrder frame at all, WhatIf or real.
func TestSimulatedTWSRefusesAnotherAccount(t *testing.T) {
	book := twssim.Default()
	book.SetAccount("U7654321")
	d := startSimDesk(t, twssim.Options{Book: book}, false)
	time.Sleep(3 * time.Second) // let the connect and its setup run
	if plan := d.plan(synaAdd); len(plan.Blockers) == 0 {
		t.Fatalf("add.plan passed on another account: %+v", plan)
	}
	if _, rerr := d.preview(synaAdd); rerr == nil {
		t.Fatal("add.preview passed on another account")
	}
	if _, rerr := d.place("not-a-token"); rerr == nil {
		t.Fatal("order.place passed on another account")
	}
	if frames := d.allPlaces(); len(frames) != 0 {
		t.Fatalf("placeOrder reached the wire for another account: %+v", frames)
	}
}
