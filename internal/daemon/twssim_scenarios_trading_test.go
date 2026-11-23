//go:build trading

package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

var protectedAdd = rpc.AddParams{Contract: rpc.ContractParams{Symbol: "SYNA", Currency: "USD"}, LimitPrice: 50.10, Quantity: 10, StopPrice: 47}

// placeProtected previews and places the protected add and returns the
// place result, failing the test on a refusal.
func (d *simDesk) placeProtected(p rpc.AddParams) rpc.OrderPlaceResult {
	d.t.Helper()
	preview, rerr := d.preview(p)
	if rerr != nil || !preview.TokenMinted {
		d.t.Fatalf("add.preview: %+v", rerr)
	}
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil || !placed.Accepted {
		d.t.Fatalf("order.place: %+v %+v", rerr, placed)
	}
	return placed
}

func orderLine(o rpc.OrderView) string {
	entry := 0
	if o.EntryLink != nil {
		entry = o.EntryLink.EntryQuantity
	}
	return fmt.Sprintf("%s %s %s qty %v filled %v remaining %v status %q lifecycle %q entry_state %q entry_qty %v send %q recon %q last %q", o.OrderRef, o.Action, o.OrderType, o.Quantity, o.Filled, o.Remaining, o.Status, o.LifecycleStatus, o.EntryState, entry, o.SendState, o.ReconciliationState, o.LastMessage)
}

// waitStatus polls the daemon's record until ok holds.
func (d *simDesk) waitStatus(ref string, ok func(rpc.OrderView) bool) rpc.OrderView {
	d.t.Helper()
	var v rpc.OrderView
	d.waitFor("order "+ref+" in the expected state", 15*time.Second, func() bool {
		v = d.status(ref).Order
		return ok(v)
	})
	return v
}

func brokerOrder(t *testing.T, sim *twssim.Server, id int) twssim.Order {
	t.Helper()
	for _, o := range sim.Book().Orders() {
		if o.OrderID == id {
			return o
		}
	}
	t.Fatalf("broker holds no order %d: %+v", id, sim.Book().Orders())
	return twssim.Order{}
}

// E1. A protected add sends the parent BUY LMT DAY untransmitted and the
// child SELL STP GTC (parentId, trigger 2, openClose C) transmitting the
// pair. A full fill activates the child. A parent that fills 4 of 10 and
// ends the day leaves the child cancelled or resized to 4, by the broker
// variant; the daemon's record of each is logged for the report
// (EntryState stays relationship_unknown, entry quantity 10).
func TestSimulatedTWSProtectedAdd(t *testing.T) {
	for _, tc := range []struct {
		name    string
		variant string
		partial float64
	}{{"full", "", 0}, {"partial-child-cancelled", twssim.ChildCancelled, 4}, {"partial-child-resized", twssim.ChildResized, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			d := startSimDesk(t, twssim.Options{ChildOnPartialExpiry: tc.variant}, true)
			if tc.partial > 0 {
				d.sim.ArmFault(twssim.Fault{Kind: twssim.FaultPartial, Quantity: tc.partial})
			}
			placed := d.placeProtected(protectedAdd)
			sent := d.realPlaces()
			if len(sent) != 2 {
				t.Fatalf("transmitted frames: %+v", sent)
			}
			parent, child := sent[0], sent[1]
			if parent.Transmit || parent.ParentID != 0 || parent.Action != "BUY" || parent.OrderType != "LMT" || parent.TIF != "DAY" || parent.Quantity != 10 || parent.LimitPrice != 50.10 ||
				!child.Transmit || child.ParentID != parent.OrderID || child.Action != "SELL" || child.OrderType != "STP" || child.TIF != "GTC" || child.AuxPrice != 47 || child.TriggerMethod != 2 || child.Quantity != 10 || child.OpenClose != "C" {
				t.Fatalf("pair frames:\nparent %+v\nchild %+v", parent, child)
			}
			if tc.partial == 0 {
				d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
				stop := d.waitStatus(placed.AttachedStopOrderRef, func(v rpc.OrderView) bool { return v.Status == "Submitted" })
				if stop.EntryState != "active" || stop.Quantity != 10 || brokerOrder(t, d.sim, child.OrderID).Status != "Submitted" {
					t.Fatalf("active stop: %s", orderLine(stop))
				}
				return
			}
			d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Filled == tc.partial })
			d.sim.EndDay()
			want, wantQty := "Cancelled", 10.0
			if tc.variant == twssim.ChildResized {
				want, wantQty = "Submitted", tc.partial
			}
			if b := brokerOrder(t, d.sim, child.OrderID); b.Status != want || b.Quantity != wantQty {
				t.Fatalf("broker child %+v", b)
			}
			p := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Cancelled" })
			stop := d.waitStatus(placed.AttachedStopOrderRef, func(v rpc.OrderView) bool { return v.Status == want && v.Quantity == wantQty })
			if p.Filled != tc.partial {
				t.Fatalf("parent record %s", orderLine(p))
			}
			t.Logf("daemon after the close: parent %s; stop %s", orderLine(p), orderLine(stop))
		})
	}
}

// E2. The broker rejects the Adaptive protected pair in three assumed
// shapes. order.place answers accepted in each (the answer is the send, not
// the broker's verdict); the record and the broker's book are asserted.
// child-only: the broker refuses the stop and holds the entry
// untransmitted; the daemon cancels the entry at the broker once, and both
// records say so with the broker's reason. The broker's Cancelled for an
// order it never acknowledged carries no reference, so the entry record
// waits pending_cancel for the reconcile sweep (E5b ends it).
func TestSimulatedTWSBrokerRejectsTheProtectedPair(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		fault                     twssim.Fault
		brokerParent, brokerChild string
		parentState               string
	}{
		{"parent-201", twssim.Fault{Kind: twssim.FaultReject, Code: 201, Message: "Order rejected - reason:a bracket with an IB algo is not supported"}, "Inactive", "Cancelled", "rejected"},
		{"child-only-201", twssim.Fault{Kind: twssim.FaultRejectChild, Code: 201, Message: "Order rejected - reason:child order not supported"}, "Cancelled", "Inactive", "pending_cancel"},
		{"parent-442", twssim.Fault{Kind: twssim.FaultReject, Code: 442, Message: "Specified algorithm is not allowed for this order"}, "Inactive", "Cancelled", "inactive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startSimDesk(t, twssim.Options{}, true)
			d.sim.ArmFault(tc.fault)
			p := protectedAdd
			p.AdaptivePriority = "Normal"
			placed := d.placeProtected(p)
			sent := d.realPlaces()
			if len(sent) != 2 || sent[0].AlgoStrategy != "Adaptive" || sent[0].AlgoParams["adaptivePriority"] != "Normal" {
				t.Fatalf("frames %+v", sent)
			}
			var parent rpc.OrderView
			d.waitFor("the entry record "+tc.parentState, 90*time.Second, func() bool {
				parent = d.status(placed.OrderRef).Order
				return parent.LifecycleStatus == tc.parentState
			})
			stop := d.waitStatus(placed.AttachedStopOrderRef, func(v rpc.OrderView) bool { return !v.Open })
			if b := brokerOrder(t, d.sim, sent[0].OrderID); b.Status != tc.brokerParent {
				t.Fatalf("broker parent %+v", b)
			}
			if b := brokerOrder(t, d.sim, sent[1].OrderID); b.Status != tc.brokerChild {
				t.Fatalf("broker child %+v", b)
			}
			if tc.fault.Kind == twssim.FaultRejectChild {
				want := "The broker refused the attached stop (broker error 201: " + tc.fault.Message + "); the entry's withdrawal was sent to the broker."
				if stop.Open || parent.LastMessage != want || stop.LastMessage != want || d.cancels() != 1 {
					t.Fatalf("withdrawn pair:\nparent %s\nstop %s", orderLine(parent), orderLine(stop))
				}
				if n := len(d.realPlaces()); n != 2 {
					t.Fatalf("withdrawal sent %d placeOrder frames, want the pair's two", n)
				}
			}
			t.Logf("daemon: parent %s; stop %s", orderLine(parent), orderLine(stop))
		})
	}
}

// cancels counts the cancelOrder frames the daemon sent.
func (d *simDesk) cancels() int {
	n := 0
	for _, f := range d.sim.Recorder().Frames() {
		if f.Name == "cancelOrder" {
			n++
		}
	}
	return n
}

// E3. --max is held in the simulated book: it carries no index protection
// and a Rulebook watch holds automated sizing, while a stated quantity only
// warns (risk/stock_add.go:236-241, 355-360). A quantity plan whose account
// ticks inside the tolerance during its WhatIf returns its size; one whose
// cash moves beyond it, or gains a cash row, is told to calculate again.
func TestSimulatedTWSSizingAndAccountChanges(t *testing.T) {
	d := startSimDesk(t, twssim.Options{WhatIfDelay: 300 * time.Millisecond}, true)
	held := d.plan(rpc.AddParams{Contract: synaAdd.Contract, LimitPrice: 50.10, Max: true})
	if len(held.Blockers) == 0 || held.Blockers[0].Code != "add_risk_hold" {
		t.Fatalf("max plan: %+v", held.Blockers)
	}
	start := time.Now()
	if plan := d.plan(synaAdd); len(plan.Blockers) != 0 {
		t.Fatalf("quantity plan: %+v", plan.Blockers)
	}
	t.Logf("a quantity plan with one 300 ms WhatIf took %s", time.Since(start))
	during := func(change func()) rpc.AddPlanResult {
		go func() {
			time.Sleep(100 * time.Millisecond)
			change()
			d.sim.PushAccount()
		}()
		return d.plan(synaAdd)
	}
	// An account tick inside the tolerance during the WhatIf changes nothing
	// the plan sized from: it returns its size.
	if plan := during(func() { d.sim.Book().SetCash("USD", d.sim.Book().Cash("USD")+0.01) }); len(plan.Blockers) != 0 || plan.Quantity != synaAdd.Quantity {
		t.Fatalf("plan across an account tick: %+v (quantity %d)", plan.Blockers, plan.Quantity)
	}
	// Cash moving beyond the tolerance, or a cash row appearing, during the
	// WhatIf is what the plan sized from changing: it is told to calculate
	// again.
	for name, change := range map[string]func(){
		"cash spent":          func() { d.sim.Book().SetCash("USD", d.sim.Book().Cash("USD")-40000) },
		"a cash row appeared": func() { d.sim.Book().SetCash("GBP", 1500) },
	} {
		plan := during(change)
		if len(plan.Blockers) == 0 || !strings.Contains(plan.Blockers[0].Message, "changed while sizing") {
			t.Fatalf("plan across %s: %+v", name, plan.Blockers)
		}
		t.Logf("%s: %s %s", name, plan.Blockers[0].Code, plan.Blockers[0].Message)
	}
}

// E4. An add placed while the exchange is closed rests PreSubmitted at the
// broker and in the daemon's record, and fills when the session opens.
func TestSimulatedTWSAddWhileClosedFillsAtTheOpen(t *testing.T) {
	t.Cleanup(marketcal.ClearSimulatedSessions)
	marketcal.SimulateSession(marketcal.MarketUSEquity, marketcal.Window{})
	d := startSimDesk(t, twssim.Options{ClosedMarkets: []string{twssim.MarketUSEquity}}, true)
	preview, rerr := d.preview(synaAdd)
	if rerr != nil {
		t.Fatalf("preview while closed: %+v", rerr)
	}
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil || !placed.Accepted {
		t.Fatalf("place while closed: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "PreSubmitted" })
	now := time.Now()
	marketcal.SimulateSession(marketcal.MarketUSEquity, marketcal.Window{Open: now.Add(time.Second), Close: now.Add(6 * time.Hour)})
	time.AfterFunc(time.Second, func() { d.sim.SetSession(twssim.MarketUSEquity, true) })
	v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Filled" })
	if v.Filled != 10 || len(d.realPlaces()) != 1 {
		t.Fatalf("after the open: %s, %d frames", orderLine(v), len(d.realPlaces()))
	}
}

// E5. Broker answers to one plain add. Each sends exactly one transmitted
// placeOrder (never a resend) and ends in the recorded lifecycle. Errors the
// broker sends without ever acknowledging the order (200, 354, 10089) and an
// order the broker never answers leave the record open, and a sweep inside
// the reconcile grace leaves it so; E5b steps past the grace and ends them.
func TestSimulatedTWSBrokerAnswersToAnAdd(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fault     twssim.Fault
		after     func(d *simDesk, id int)
		lifecycle string
		filled    float64
	}{
		{"presubmitted", twssim.Fault{Kind: twssim.FaultPreSubmitted}, nil, "pre_submitted", 0},
		{"partial-then-full", twssim.Fault{Kind: twssim.FaultPartial, Quantity: 3}, func(d *simDesk, id int) {
			d.waitStatus(d.realPlaces()[0].OrderRef, func(v rpc.OrderView) bool { return v.Filled == 3 })
			_ = d.sim.Fill(id, 0, 0)
		}, "filled", 10},
		{"reject-after-ack-201", twssim.Fault{Kind: twssim.FaultRejectAfterAck, Code: 201, Message: "Order rejected - reason:after acknowledging"}, nil, "inactive", 0},
		{"price-increment-110", twssim.Fault{Kind: twssim.FaultError, Code: 110, Message: "The price does not conform to the minimum price variation for this contract."}, nil, "rejected", 0},
		{"duplicate-id-103", twssim.Fault{Kind: twssim.FaultError, Code: 103, Message: "Duplicate order id"}, nil, "rejected", 0},
		{"no-definition-200", twssim.Fault{Kind: twssim.FaultError, Code: 200, Message: "No security definition has been found for the request"}, nil, "unknown_reconcile_required", 0},
		{"market-data-354", twssim.Fault{Kind: twssim.FaultError, Code: 354, Message: "Requested market data is not subscribed."}, nil, "unknown_reconcile_required", 0},
		{"market-data-10089", twssim.Fault{Kind: twssim.FaultError, Code: 10089, Message: "Requested market data requires additional subscription for API."}, nil, "unknown_reconcile_required", 0},
		{"broker-cancel-202", twssim.Fault{Kind: twssim.FaultHold}, func(d *simDesk, _ int) { d.sim.EndDay() }, "cancelled", 0},
		{"never-answered", twssim.Fault{Kind: twssim.FaultSilent}, nil, "pending_submit", 0},
		{"disconnect-after-send", twssim.Fault{Kind: twssim.FaultDrop}, nil, "pending_submit", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := startSimDesk(t, twssim.Options{}, true)
			preview, rerr := d.preview(synaAdd)
			if rerr != nil {
				t.Fatalf("preview %+v", rerr)
			}
			d.sim.ArmFault(tc.fault)
			placed, rerr := d.place(preview.PreviewToken)
			if rerr != nil || !placed.Accepted {
				t.Fatalf("place: %+v", rerr)
			}
			if tc.after != nil {
				time.Sleep(200 * time.Millisecond)
				tc.after(d, placed.ReservedOrderID)
			}
			v := d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.LifecycleStatus == tc.lifecycle })
			if v.Filled != tc.filled {
				t.Fatalf("record %s", orderLine(v))
			}
			if tc.lifecycle == "pending_submit" || tc.lifecycle == "unknown_reconcile_required" {
				d.waitFor("a ready session", 30*time.Second, func() bool { c := d.srv.gatewayConnectorForTest(); return c != nil && c.IsReady() })
				d.srv.reconcileOrderJournalWithBroker(t.Context())
				if after := d.status(placed.OrderRef).Order; after.LifecycleStatus != tc.lifecycle {
					t.Fatalf("the reconcile changed the record: %s", orderLine(after))
				}
			}
			if n := len(d.realPlaces()); n != 1 {
				t.Fatalf("transmitted placeOrder frames: %d, want exactly one", n)
			}
		})
	}
}

// E6. Futures, currencies and bonds are refused by add before any broker
// request; an add beside a pending buy of the same stock is still sized.
func TestSimulatedTWSAddRefusesOtherInstruments(t *testing.T) {
	d := startSimDesk(t, twssim.Options{}, true)
	for _, secType := range []string{"FUT", "CASH", "BOND"} {
		res := dispatchRaw(t, d.srv, rpc.MethodAddPlan, fmt.Sprintf(`{"contract":{"symbol":"SYNX","sec_type":%q,"currency":"USD"},"limit_price":10,"quantity":1}`, secType))
		if res.Ok || res.Error == nil || !strings.Contains(res.Error.Message, "exact stock identity") {
			t.Fatalf("%s add: %+v", secType, res.Error)
		}
	}
	preview, _ := d.preview(synaAdd)
	d.sim.ArmFault(twssim.Fault{Kind: twssim.FaultHold})
	placed, rerr := d.place(preview.PreviewToken)
	if rerr != nil {
		t.Fatalf("place: %+v", rerr)
	}
	d.waitStatus(placed.OrderRef, func(v rpc.OrderView) bool { return v.Status == "Submitted" })
	if plan := d.plan(synaAdd); len(plan.Blockers) != 0 || plan.Quantity != 10 {
		t.Fatalf("plan beside a pending buy: %+v", plan)
	}
}
