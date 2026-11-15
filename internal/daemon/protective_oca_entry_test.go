package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestProtectiveOCAPreservesHeldEntryStop(t *testing.T) {
	t.Parallel()
	now := time.Now()
	child := ocaTestStopView("entry-stop", 1002, 70, "", now)
	child.OrderType = "STP"
	child.EntryLink = &rpc.StockEntryLink{PairRef: "entry", ParentRef: "entry", ParentID: 1001, EntryQuantity: 70}
	child.EntryState = "held"
	child.Status = "PreSubmitted"
	child.LifecycleStatus = rpc.OrderLifecyclePreSubmitted
	for _, snapshot := range []*ibkrlib.OpenOrderSnapshot{nil, {Complete: true, AsOf: now, Orders: []ibkrlib.OrderLifecycleEvent{ocaTestStopOrder(1002, 70, "", 0)}}} {
		for _, quantity := range []int{30, 70} {
			draft := ocaTestSale(quantity)
			plan := planProtectiveOCA(draft, 70, canaryWorkingSellStops([]rpc.OrderView{child}, snapshot, ocaTestScope, draft))
			if plan.Refusal == "" || len(plan.Steps) != 0 || plan.Group != "" {
				t.Fatalf("sale of existing shares must preserve pending entry protection: quantity=%d steps=%+v group=%q", quantity, plan.Steps, plan.Group)
			}
		}
	}
}

func TestProtectiveGuardCannotChangeUnactivatedEntryStop(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"held", "awaiting_broker", "relationship_unknown", "activation_pending", ""} {
		t.Run(state, func(t *testing.T) {
			s := &Server{}
			child := ocaTestStopView("entry-stop", 1002, 70, "", time.Now())
			child.EntryLink = &rpc.StockEntryLink{PairRef: "entry", ParentRef: "entry", ParentID: 1001, EntryQuantity: 70}
			child.EntryState = state
			for _, kind := range []string{protectiveStopGuardCancel, protectiveStopGuardShrink} {
				s.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: kind, OrderRef: child.OrderRef, ReservedOrderID: child.ReservedOrderID, Quantity: 30})
				if err := s.protectiveStopGuardGrantCovers(rpc.OrderOriginDaemonProtectiveGuard, kind, child, 30); err == nil {
					t.Fatalf("guard allowed %s of entry stop whose activation is %q", kind, state)
				}
				if err := s.protectiveStopGuardGrantCovers(rpc.OrderOriginHumanTTY, kind, child, 30); err != nil {
					t.Fatalf("explicit owner operation must retain its existing authority: %v", err)
				}
			}
		})
	}
}

func TestProtectiveOCAUnconfirmedEntryBlocksBeforeChangingOtherStops(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"held", "awaiting_broker", "relationship_unknown", "activation_pending", ""} {
		t.Run(state, func(t *testing.T) {
			now := time.Now()
			ordinary := ocaTestStopView("a-existing-stop", 1000, 70, "", now)
			child := ocaTestStopView("z-entry-stop", 1002, 70, "", now)
			child.EntryLink = &rpc.StockEntryLink{ParentRef: "entry", ParentID: 1001, EntryQuantity: 70}
			child.EntryState = state
			draft := ocaTestSale(70)
			plan := planProtectiveOCA(draft, 70, canaryWorkingSellStops([]rpc.OrderView{ordinary, child}, nil, ocaTestScope, draft))
			if plan.Refusal == "" || len(plan.Steps) != 0 || plan.Group != "" {
				t.Fatalf("uncertain attachment must refuse before changing any stop: %+v", plan)
			}
		})
	}
}

func TestProtectiveOCAConfirmedActiveEntryStillManagesCapacity(t *testing.T) {
	t.Parallel()
	child := ocaTestStopView("entry-stop", 1002, 70, "", time.Now())
	child.EntryLink = &rpc.StockEntryLink{ParentRef: "entry", ParentID: 1001, EntryQuantity: 70}
	child.EntryState = "active"
	draft := ocaTestSale(70)
	plan := planProtectiveOCA(draft, 70, canaryWorkingSellStops([]rpc.OrderView{child}, nil, ocaTestScope, draft))
	if plan.Refusal != "" || len(plan.Steps) != 1 || plan.Steps[0].Kind != protectiveStopGuardCancel {
		t.Fatalf("confirmed active stop must retain ordinary capacity management: %+v", plan)
	}
	s := &Server{}
	s.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: protectiveStopGuardCancel, OrderRef: child.OrderRef, ReservedOrderID: child.ReservedOrderID})
	if err := s.protectiveStopGuardGrantCovers(rpc.OrderOriginDaemonProtectiveGuard, protectiveStopGuardCancel, child, 0); err != nil {
		t.Fatalf("active child capacity management refused: %v", err)
	}
}
