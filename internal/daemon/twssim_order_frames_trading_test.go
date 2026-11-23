//go:build trading

package daemon

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// A large order journal must not slow the broker reader: an order frame
// pairs with its journal row in milliseconds, so a fresh all-client
// open-order read completes inside its deadline during a burst of order
// frames, and a fired exit-plan trail's whole price ladder sells over the
// same book. The journal holds 50,000 synthetic events of finished orders;
// 16 orders work, 8 placed by the daemon and 8 outside it.
func TestSimulatedTWSOrderFramesStayFastOnALargeJournal(t *testing.T) {
	d, opt := planDesk(t, 50, 20)
	d.sim.SetMidFill(opt.ConID, false)
	planStartIn(t, -10*time.Minute, 0)
	st := d.srv.currentTradingStatus()
	base := time.Now().Add(-48 * time.Hour).UTC()
	var batch []orderJournalEvent
	for i := range 50000 {
		order := i / 10
		ev := orderJournalEvent{At: base.Add(time.Duration(i) * time.Second), Type: orderJournalEventStatusUpdated, OrderRef: fmt.Sprintf("seed%05d", order),
			ReservedOrderID: 500000 + order, ClientID: st.ClientID, Account: st.Account, Endpoint: st.Endpoint, Mode: st.Mode,
			Symbol: "SYNB", SecType: "STK", ConID: 900002, Action: "BUY", OrderType: "LMT", Quantity: 1, Status: "Submitted", PermID: 800000 + order}
		switch i % 10 {
		case 0:
			ev.Type = orderJournalEventSendAttempted
		case 9:
			ev.Status, ev.Filled, ev.SendState = "Filled", 1, orderSendStateTerminal
		}
		batch = append(batch, ev)
		if len(batch) == 1000 {
			if err := d.srv.orderJournal.AppendAll(batch); err != nil {
				t.Fatal(err)
			}
			batch = nil
		}
	}
	// Eight working buys placed through the daemon, resting far below the
	// market, and eight placed outside it.
	exps := twssim.OptionExpiries(time.Now())
	for i, strike := range []float64{40, 45, 50, 55, 60, 40, 45, 50} {
		c := d.optionContract(exps[1+i/5], strike, "C")
		prev, rerr := d.optionPreview(c, 1, 0.01, "")
		if rerr != nil || !prev.SubmitEligible {
			t.Fatalf("working order %d preview: %+v", i, rerr)
		}
		if placed, rerr := d.place(prev.PreviewToken); rerr != nil || !placed.Accepted {
			t.Fatalf("working order %d place: %+v", i, rerr)
		}
	}
	for i := range 8 {
		if _, err := d.sim.AddOutsideOrder(900002, "BUY", 1, 60+float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(3 * time.Second)
	// The sixteen working orders the scenario set up, by broker identity.
	placed := map[[2]int]bool{}
	for _, o := range d.sim.Book().Orders() {
		if o.Working() {
			placed[[2]int{o.ClientID, o.OrderID}] = true
		}
	}
	if len(placed) != 16 {
		t.Fatalf("%d working orders set up, want 16", len(placed))
	}
	c := d.srv.gatewayConnectorForTest()
	// A burst of order frames: the reader pairs each with its journal row.
	frames := d.sim.Churn(20)
	d.waitFor("the burst to be read", 120*time.Second, func() bool {
		load := c.ReaderLoad()
		return load.Running == 0 && load.Frames >= frames
	})
	load := c.ReaderLoad()
	for _, m := range load.Top {
		if (m.ID == 3 || m.ID == 5) && m.Frames > 0 {
			if per := m.Busy / time.Duration(m.Frames); per > 50*time.Millisecond {
				t.Fatalf("message %d took %s a frame (%d frames, %s): %s", m.ID, per, m.Frames, m.Busy, load)
			}
		}
	}
	// A fresh all-client read completes with margin inside the protocol
	// deadline (under 60% of it, whatever else loads the machine), poisons
	// nothing, and holds exactly the sixteen working orders set up. The
	// snapshot carries an event per frame it collected, so orders are counted
	// by identity.
	deadline := d.srv.openOrderSnapshotProtocolTimeout
	if deadline == 0 {
		deadline = ibkrlib.DefaultOpenOrderSnapshotTimeout
	}
	started := time.Now()
	snap, scope, err := d.srv.brokerOpenOrderInventory(t.Context(), true)
	took := time.Since(started)
	seen := map[[2]int]bool{}
	for _, o := range snap.Orders {
		if brokerOrderWorking(o) {
			seen[[2]int{o.ClientID, o.OrderID}] = true
		}
	}
	if err != nil || !snap.Complete || !sameBrokerScope(scope, d.srv.currentBrokerStateScope()) || took > deadline*6/10 || !maps.Equal(seen, placed) {
		t.Fatalf("a fresh open-order read took %s of a %s deadline: %v (complete %v, %d working orders, want the 16 set up)", took, deadline, err, snap.Complete, len(seen))
	}
	if _, cause, poisoned := c.OpenOrderSnapshotPoisoned(); poisoned {
		t.Fatalf("the open-order read was poisoned: %v", cause)
	}
	t.Logf("%d order frames read; a fresh read took %s; %s", frames, took, load)
	// A fired trail's whole ladder over the same book.
	trail := d.queuePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})[0]
	d.planWaitRecord("the high to seed", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater == 50 })
	d.planStock(48)
	rec := d.planWaitRecord("the trail's ladder to sell", trail, 180*time.Second, func(r queuedAuthRecord) bool { return r.final() })
	if rec.State != rpc.QueuedAuthFilled || d.planBrokerSold(opt.ConID) != 20 || rec.Watch.SaleAttempts != 3 {
		t.Fatalf("%s %s: sold %g in %d attempts", rec.State, rec.Reason, d.planBrokerSold(opt.ConID), rec.Watch.SaleAttempts)
	}
	if _, cause, poisoned := c.OpenOrderSnapshotPoisoned(); poisoned {
		t.Fatalf("the ladder poisoned the open-order read: %v", cause)
	}
}
