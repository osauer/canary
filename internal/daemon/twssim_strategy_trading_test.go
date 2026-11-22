//go:build trading

package daemon

import (
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
		})
	}
}
