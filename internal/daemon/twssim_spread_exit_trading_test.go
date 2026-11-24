//go:build trading

package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The owner holds 3 SYNA 50 calls: 1 the bought leg of a bull call spread
// Canary opened (50/60), 2 held beside it. An exit plan on the call covers
// the 2: when the line is crossed it sells exactly 2, and the spread, its
// bought call and its sold call, stays held and actionable as one strategy.
func TestSimulatedTWSExitPlanBesideASpreadSellsOnlyTheLine(t *testing.T) {
	d, opt := planDeskOn(t, 3, 50, 2)
	planStartIn(t, -10*time.Minute, 0)
	short := d.optionContract(opt.Expiry, 60, "C")
	limit := roundPrice(d.optionPrice(opt.ConID) - d.optionPrice(short.ConID) + 0.5)
	pv, rerr := d.openPreview(opt, short, 1, &limit)
	if rerr != nil || !pv.SubmitEligible {
		t.Fatalf("open preview: %+v", rerr)
	}
	placed, rerr := d.place(pv.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("open place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	spread := d.strategyNamed(pv.Draft.StrategyGroup.StrategyID, 1)
	if p, _ := d.sim.Book().Position(opt.ConID); p.Quantity != 3 {
		t.Fatalf("calls held after the open: %v, want 3", p.Quantity)
	}

	q := d.preparePlan(rpc.QueuedExitPlanParams{Contract: opt, WaitMinutes: planWait, Trail: planTrail(48.50)})
	if !q.Accepted || q.Plan == nil || len(q.Plan.Queues) != 1 {
		t.Fatalf("plan prepare: %+v", q.Blockers)
	}
	if terms := q.Plan.Queues[0].Terms; terms.MaxQuantity != 2 || terms.PositionQuantity != 2 {
		t.Fatalf("the plan covers %d of %v, want the 2 beside the spread", terms.MaxQuantity, terms.PositionQuantity)
	}
	res := d.armPlanWith(q.Plan.QueuedRefs, q.Plan.PlanDigest)
	if !res.Accepted {
		t.Fatalf("arm: %+v", res.Blockers)
	}
	trail := q.Plan.Queues[0].Terms.QueueID
	d.planWaitRecord("the high to seed", trail, 30*time.Second, func(r queuedAuthRecord) bool { return r.Watch.HighWater == 50 })
	d.planStock(48)
	d.planWaitRecord("the trail to sell", trail, 60*time.Second, func(r queuedAuthRecord) bool {
		return r.Watch.SaleFilled+int(r.FilledQuantity) >= 2 || r.State == rpc.QueuedAuthFilled
	})
	d.waitFor("2 calls sold", 30*time.Second, func() bool { return d.planBrokerSold(opt.ConID) == 2 })
	time.Sleep(2 * time.Second)
	sells := d.planSells(opt.ConID)
	total := 0.0
	for _, o := range sells {
		total += o.Quantity
		if o.SecType == "BAG" || o.Quantity > 2 {
			t.Fatalf("a sell past the line: %+v", o)
		}
	}
	if sold := d.planBrokerSold(opt.ConID); sold != 2 {
		t.Fatalf("sold %v calls, want 2 (orders %+v)", sold, sells)
	}
	if p, _ := d.sim.Book().Position(opt.ConID); p.Quantity != 1 {
		t.Fatalf("calls left: %v, want the spread's 1", p.Quantity)
	}
	if p, _ := d.sim.Book().Position(short.ConID); p.Quantity != -1 {
		t.Fatalf("the sold call: %v, want -1", p.Quantity)
	}
	after := d.strategyNamed(spread.ID, 1)
	if !after.Actionable || after.Status != rpc.PositionStrategyStatusCurrent || after.Name != "bull call spread" {
		t.Fatalf("the spread after the exit: %+v", after)
	}
}
