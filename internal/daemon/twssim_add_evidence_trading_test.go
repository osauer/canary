//go:build trading

package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// An Add plan is sized while other clients' order frames for other contracts
// and account value ticks arrive: nothing it sized from changed, so it
// returns its size. A buy that spends the same cash, arriving between the
// collection and the check, still makes the evidence stale.
func TestSimulatedTWSAddEvidenceIgnoresUnrelatedMoves(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	// A protective stop entered in TWS on the held stock: an order the Add
	// allows, re-sent by the churn below.
	if _, err := d.sim.AddOutsideStop(planSYNA, 50, 40); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
				d.sim.Churn(1)
				base := d.sim.Book().Base()
				d.sim.Book().SetCash(base, d.sim.Book().Cash(base)+0.01)
				d.sim.PushAccount()
			}
		}
	}()
	plan := d.plan(synaAdd)
	close(stop)
	<-done
	for _, b := range plan.Blockers {
		if b.Code == "add_evidence_unavailable" || b.Code == "add_evidence_changed" {
			t.Fatalf("an unrelated move refused the Add: %+v", plan.Blockers)
		}
	}
	if plan.Quantity != synaAdd.Quantity {
		t.Fatalf("plan %+v", plan)
	}
	ev, err := d.srv.stockAddEvidence(t.Context(), synaAdd)
	if err != nil {
		t.Fatal(err)
	}
	d.sim.Churn(3)
	time.Sleep(time.Second)
	if !d.srv.stockAddEvidenceCurrent(t.Context(), &ev) {
		t.Fatal("re-sent order frames made the evidence stale")
	}
	if _, err := d.sim.AddOutsideOrder(900002, "BUY", 50, 100); err != nil {
		t.Fatal(err)
	}
	d.sim.Churn(1)
	time.Sleep(time.Second)
	if d.srv.stockAddEvidenceCurrent(t.Context(), &ev) {
		t.Fatal("a buy on the same cash left the evidence current")
	}
}
