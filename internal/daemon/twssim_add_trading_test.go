//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// startSimulatedDaemon runs the real Server.Start path in simulator mode
// against a TWS simulator, with its own XDG roots, socket and state database,
// and the synthetic owner's reviewed policies.
func startSimulatedDaemon(t *testing.T, sim *twssim.Server) (*Server, *lockedBuffer) {
	t.Helper()
	dir := shortTempDir(t)
	for key, sub := range map[string]string{"XDG_STATE_HOME": "state", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_DATA_HOME": "data", "HOME": "home"} {
		t.Setenv(key, filepath.Join(dir, sub))
	}
	if err := WriteSimulatorPolicies(config.PolicyDir(), sim.Book().Base()); err != nil {
		t.Fatal(err)
	}
	port := sim.Port()
	cfg, err := (&config.Config{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: &port, ClientID: new(15), Account: sim.Book().Account(), TLS: new(false)},
		Trading: config.Trading{Mode: "paper"},
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
	deadline := time.Now().Add(30 * time.Second)
	for {
		if c := srv.gatewayConnectorForTest(); c != nil && c.IsReady() && srv.postConnectSetupDone.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready session against the simulator:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return srv, out
}

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

// The owner's question for 2026-11-23: does adding to a position work end to
// end? The real daemon, its real IBKR connector and wire codec, against the
// TWS simulator: the session reaches ready, the regime publishes from the
// simulator's quotes and bars, and add.plan sizes the addition from the
// broker's own account, positions, settled cash, open orders, contract and a
// WhatIf per candidate quantity. The WhatIf placeOrder frames are read back by
// the simulator's own decoder, by the field numbers of IBKR's Order.proto.
func TestSimulatedTWSPlansAnAdditionFromBrokerEvidence(t *testing.T) {
	sim := twssim.Start(t, twssim.Options{})
	srv, logged := startSimulatedDaemon(t, sim)
	if mgr := srv.riskPolicies.snapshot(); mgr.policy == nil || mgr.review == rpc.PolicyReviewUnreviewed {
		t.Fatalf("risk policy %s at %s: %s", mgr.status, mgr.path, mgr.message)
	}
	regime := dispatchedRegime(t, srv)
	t.Logf("regime: %s; %d clusters ranked", regime.Composite.Verdict, regime.Composite.ClusterRankedCount)

	add := rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, LimitPrice: 50.10, Quantity: 10}
	plan, rerr := dispatchJSON[rpc.AddPlanResult](t, srv, rpc.MethodAddPlan, add)
	if rerr != nil {
		t.Fatalf("add.plan: %+v", rerr)
	}
	if len(plan.Blockers) > 0 || plan.Quantity != 10 || plan.Contract.ConID != 900001 || plan.Before != 100 {
		t.Fatalf("add.plan: quantity %d of conid %d, held %v before, blockers %+v\nlog:\n%s", plan.Quantity, plan.Contract.ConID, plan.Before, plan.Blockers, logged.String())
	}

	var whatIfs []twssim.PlaceOrder
	for _, f := range sim.Recorder().Frames() {
		if f.Order == nil {
			continue
		}
		if len(f.IgnoredByBroker) > 0 {
			t.Errorf("placeOrder fields the broker drops: %v", f.IgnoredByBroker)
		}
		whatIfs = append(whatIfs, *f.Order)
	}
	if len(whatIfs) == 0 {
		t.Fatal("add.plan sent no WhatIf placeOrder")
	}
	for _, o := range whatIfs {
		if !o.WhatIf || !o.Transmit || o.Action != "BUY" || o.Quantity != 10 || o.OrderType != "LMT" || o.LimitPrice != 50.10 || o.TIF != "DAY" || o.OutsideRTH ||
			o.Account != "DU1234567" || o.ConID != 900001 || o.Symbol != "SYNA" || o.SecType != "STK" || o.Currency != "USD" || o.ClientID != 15 {
			t.Fatalf("WhatIf placeOrder frame: %+v", o)
		}
	}
	if orders := sim.Book().Orders(); len(orders) != 0 {
		t.Fatalf("a WhatIf left an order at the broker: %+v", orders)
	}
	if !strings.Contains(logged.String(), "Connected to IB Gateway 127.0.0.1") {
		t.Fatalf("no connect line:\n%s", logged.String())
	}
}
