package daemon

import (
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestMandateOCACoverageUsesWholeGroup(t *testing.T) {
	for _, tc := range []struct {
		name              string
		typ               int
		short             bool
		change            func([]ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent
		wantQty, wantLoss float64
	}{
		{name: "cancel group", typ: 1, wantQty: 50, wantLoss: 500},
		{name: "reduce group", typ: 2, wantQty: 50, wantLoss: 500},
		{name: "full alternative stops", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			for i := range o {
				o[i].Remaining, o[i].TotalQuantity = 100, 100
			}
			return o
		}, wantQty: 100, wantLoss: 1000},
		{name: "separate groups", typ: 2, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[1].OcaGroup = "another-group"
			return o
		}, wantQty: 100, wantLoss: 1000},
		{name: "unpriced sibling", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent { o[1].AuxPrice = 0; return o }},
		{name: "unblocked group", typ: 3},
		{name: "unknown type", typ: 99},
		{name: "unequal quantities and prices", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[1].Remaining = 80
			o[1].TotalQuantity = 80
			o[1].AuxPrice = 80
			return o
		}, wantQty: 50, wantLoss: 1000},
		{name: "short group worst price", typ: 2, short: true, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent { o[1].AuxPrice = 120; return o }, wantQty: 50, wantLoss: 1000},
		{name: "partial fill remaining", typ: 2, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[0].Remaining = 30
			o[0].Filled = 20
			return o
		}, wantQty: 30, wantLoss: 300},
		{name: "cross contract sibling", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[1].ConID = 9002
			o[1].Symbol = "SYNB"
			return o
		}},
		{name: "limit sibling", typ: 2, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[1].OrderType = "LMT"
			o[1].LimitPrice = 110
			return o
		}},
		{name: "opposite side sibling", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent { o[1].Action = "BUY"; return o }},
		{name: "deactivated sibling", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent { o[1].Deactivated = new(true); return o }},
		{name: "mixed types", typ: 1, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent { o[1].OcaType = 2; return o }},
		{name: "standalone OCA", typ: 2, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[0].Remaining = 100
			o[0].TotalQuantity = 100
			return o[:1]
		}, wantQty: 100, wantLoss: 1000},
		{name: "standalone nonblock OCA", typ: 3, change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			o[0].Remaining = 100
			o[0].TotalQuantity = 100
			return o[:1]
		}, wantQty: 100, wantLoss: 1000},
		{name: "independent stops", change: func(o []ibkr.OrderLifecycleEvent) []ibkr.OrderLifecycleEvent {
			for i := range o {
				o[i].OcaGroup = ""
			}
			return o
		}, wantQty: 100, wantLoss: 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			qty, action, price := 100.0, "SELL", 90.0
			if tc.short {
				qty, action, price = -100, "BUY", 110
			}
			row := mandateReductionStock("SYNA", 9001, qty, 100)
			pos := mandateReductionBook(row)
			scope := brokerStateScope{Account: "synthetic", Mode: rpc.AccountModePaper}
			var orders []ibkr.OrderLifecycleEvent
			for id := range 2 {
				orders = append(orders, ibkr.OrderLifecycleEvent{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 100 + id, ConID: 9001, Account: scope.Account, SecType: "STK", Symbol: "SYNA", Action: action, OrderType: "STP", Status: "Submitted", Remaining: 50, TotalQuantity: 50, AuxPrice: price, OcaGroup: "synthetic-group", OcaType: tc.typ})
			}
			if tc.change != nil {
				orders = tc.change(orders)
			}
			// Inventory order cannot choose the group's quantity or loss.
			for range 2 {
				s := &Server{openOrderInventoryForTest: func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
					return ibkr.OpenOrderSnapshot{Complete: true, AsOf: mandateTestNow, Orders: orders}, scope, nil
				}}
				stops, why := s.mandateArmedStops(t.Context(), pos, scope)
				if why != "" {
					t.Fatal(why)
				}
				covered := 0.0
				for _, stop := range stops[mandateLegID(row)] {
					covered += stop.Quantity
				}
				if covered != tc.wantQty {
					t.Fatalf("proven coverage=%g want=%g", covered, tc.wantQty)
				}
				legs, _ := mandateApplyStops([]mandate.Leg{{ID: mandateLegID(row), Symbol: "SYNA", Kind: mandate.KindStock, Quantity: qty}}, pos, "EUR", stops, "")
				// A partly covered line carries its loss per covered share.
				units := 100.0
				if legs[0].StoppedUnits > 0 {
					units = legs[0].StoppedUnits
				}
				if legs[0].StopArmed != (tc.wantQty == 100) || math.Abs(legs[0].LossPerUnitBase*units-tc.wantLoss) > 1e-8 {
					t.Fatalf("loss/armed mismatch: %+v; want loss=%g covered=%g", legs[0], tc.wantLoss, tc.wantQty)
				}
				if tc.wantQty > 0 && tc.wantQty < 100 && !strings.Contains(legs[0].Symbol, "of 100 shares stopped") {
					t.Fatal("partial group coverage omitted residual")
				}
				slices.Reverse(orders)
			}
		})
	}
}
