//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// startSimDeskSkewed is startSimDesk with the daemon's clock ahead of the
// wall clock by skew, set before Start, so a test can step past the
// reconcile grace without waiting it out.
func startSimDeskSkewed(t *testing.T, opts twssim.Options) (*simDesk, *atomic.Int64) {
	t.Helper()
	requireSimTier(t)
	skew := &atomic.Int64{}
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
	srv.now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
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
	d.waitFor("a ready session against the simulator", 30*time.Second, func() bool {
		c := srv.gatewayConnectorForTest()
		return c != nil && c.IsReady() && srv.postConnectSetupDone.Load()
	})
	dispatchedRegime(t, srv)
	return d, skew
}

// sweepPastGrace steps the daemon's clock past the reconcile grace and runs
// one sweep on a ready session.
func (d *simDesk) sweepPastGrace(skew *atomic.Int64) {
	d.t.Helper()
	skew.Add(int64(orderReconcileGrace + time.Minute))
	d.waitFor("a ready session", 30*time.Second, func() bool { c := d.srv.gatewayConnectorForTest(); return c != nil && c.IsReady() })
	d.srv.reconcileOrderJournalWithBroker(d.t.Context())
}

// E5b. Records that never got a broker acknowledgement end. The broker
// holding the order (it never answered) binds it from the complete
// open-order snapshot and its lifecycle continues; an order the complete
// snapshot does not contain (an error before any acknowledgement, a
// connection lost before the broker booked it) ends closed_reconciled,
// final state unknown. Nothing is sent again.
func TestSimulatedTWSReconcileEndsUnacknowledgedOrders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fault     twssim.Fault
		lifecycle string
	}{
		{"never-answered-bound", twssim.Fault{Kind: twssim.FaultSilent}, "submitted"},
		{"disconnect-after-send", twssim.Fault{Kind: twssim.FaultDrop}, "closed_reconciled"},
		{"no-definition-200", twssim.Fault{Kind: twssim.FaultError, Code: 200, Message: "No security definition has been found for the request"}, "closed_reconciled"},
		{"market-data-354", twssim.Fault{Kind: twssim.FaultError, Code: 354, Message: "Requested market data is not subscribed."}, "closed_reconciled"},
		{"market-data-10089", twssim.Fault{Kind: twssim.FaultError, Code: 10089, Message: "Requested market data requires additional subscription for API."}, "closed_reconciled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, skew := startSimDeskSkewed(t, twssim.Options{})
			preview, rerr := d.preview(synaAdd)
			if rerr != nil {
				t.Fatalf("preview %+v", rerr)
			}
			d.sim.ArmFault(tc.fault)
			placed, rerr := d.place(preview.PreviewToken)
			if rerr != nil || !placed.Accepted {
				t.Fatalf("place: %+v", rerr)
			}
			time.Sleep(time.Second)
			before := d.status(placed.OrderRef).Order
			if !before.Open || before.PermID != 0 {
				t.Fatalf("before the sweep: %s", orderLine(before))
			}
			d.sweepPastGrace(skew)
			v := d.status(placed.OrderRef).Order
			if v.LifecycleStatus != tc.lifecycle {
				t.Fatalf("after the sweep: %s", orderLine(v))
			}
			if tc.lifecycle == "closed_reconciled" && (v.Open || !strings.Contains(v.LastMessage, "never acknowledged")) {
				t.Fatalf("closed record: %s", orderLine(v))
			}
			if tc.lifecycle == "submitted" && (!v.Open || v.PermID == 0) {
				t.Fatalf("bound record: %s perm %d", orderLine(v), v.PermID)
			}
			if n := len(d.realPlaces()); n != 1 {
				t.Fatalf("transmitted placeOrder frames: %d, want exactly one", n)
			}
		})
	}
}

// E2b. The entry withdrawn after its stop was refused ends with the sweep:
// the broker's Cancelled for it carried no reference to bind.
func TestSimulatedTWSWithdrawnEntryEnds(t *testing.T) {
	d, skew := startSimDeskSkewed(t, twssim.Options{})
	d.sim.ArmFault(twssim.Fault{Kind: twssim.FaultRejectChild, Code: 201, Message: "Order rejected - reason:child order not supported"})
	placed := d.placeProtected(protectedAdd)
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.LifecycleStatus == "pending_cancel" })
	d.sweepPastGrace(skew)
	want := "The broker refused the attached stop (broker error 201: Order rejected - reason:child order not supported); the entry was withdrawn."
	parent, stop := d.status(placed.OrderRef).Order, d.status(placed.AttachedStopOrderRef).Order
	if parent.Open || stop.Open || parent.LifecycleStatus != "closed_reconciled" || parent.LastMessage != want || stop.LastMessage != want || d.cancels() != 1 {
		t.Fatalf("after the sweep:\nparent %s\nstop %s", orderLine(parent), orderLine(stop))
	}
}
