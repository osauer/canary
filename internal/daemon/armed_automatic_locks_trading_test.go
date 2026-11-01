//go:build trading

package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestArmedAutomaticLockSafetyShrinkReachesBroker(t *testing.T) {
	t.Parallel()
	r := newStopGuardRig(t, config.Trading{})
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYW", ConID: 910001, AccountID: "DU1234567", AccountMode: "paper"}
	automaticLockUpdate(t, r.srv, 0, "locked-stop-shrink", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	r.set(50, true)
	r.pass(t)
	if modified, cancelled := r.writes(); modified != 1 || cancelled != 0 {
		t.Fatalf("locked stop did not safely follow holding100->50: modified=%d cancelled=%d", modified, cancelled)
	}
	got := r.modified[0]
	if got.OrderID != 1002 || got.TotalQty != 50 || got.OrderType != rpc.OrderTypeTRAIL || got.TIF != rpc.OrderTIFGTC || got.AuxPrice != 8.5 || got.TrailStopPrice != 90 {
		t.Fatalf("safety shrink changed existing protection terms: %+v", got)
	}
	if r.srv.protectiveStopGuardGrant.Load() != nil {
		t.Fatal("safety exception outlived its broker write")
	}
}

func TestArmedAutomaticLockFullPositionShrinkReachesBroker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ position, filled float64 }{{60, 0}, {60, 20}, {120, 0}} {
		t.Run(fmt.Sprintf("position_%g_filled_%g", tc.position, tc.filled), func(t *testing.T) {
			rig, broker := newQueueTradingRig(t)
			position := 100.0
			rig.server.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, qty int) (rpc.OrderPositionImpact, error) {
				return rpc.OrderPositionImpact{Before: position, After: position - float64(qty), Effect: classifyPositionEffect(position, position-float64(qty))}, nil
			}
			p := rig.stopProposal()
			p.Quantity, p.PositionQuantity = 100, 100
			revision := rig.install(p)
			scope := rig.engine.currentScope()
			q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, FullPosition: true, Key: p.Key, Revision: revision, AccountID: scope.Account, AccountMode: scope.Mode})
			if err != nil || !q.Accepted {
				t.Fatalf("synthetic owner stop prepare: %+v %v", q, err)
			}
			rig.queueArm(q)
			rig.now = queueWindowOpen
			rig.queueCycle()
			if broker.count() != 1 {
				t.Fatalf("initial stop not sent: %+v", rig.queued(q.Queue.Terms.QueueID))
			}
			rec := rig.queued(q.Queue.Terms.QueueID)
			var ack orderJournalEvent
			for _, event := range journalEventsForToken(t, rig.server, rec.PreviewTokenID) {
				if event.Type == orderJournalEventSendAttempted {
					ack = event
				}
			}
			ack.Type, ack.SendState, ack.Status = orderJournalEventBrokerAcknowledged, orderSendStateBrokerAcknowledged, "Submitted"
			ack.Remaining, ack.Filled, ack.At = 100-tc.filled, tc.filled, rig.now
			if err := rig.server.orderJournal.Append(ack); err != nil {
				t.Fatal(err)
			}
			original := broker.orders[0]
			ratchet := original.TrailStopPrice + 0.1
			rig.server.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now, Orders: []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder,
					OrderID: original.OrderID, ClientID: original.ClientID, ClientIDPresent: true, Account: scope.Account, ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD",
					Action: rpc.OrderActionSell, OrderType: original.OrderType, TIF: original.TIF, TriggerMethod: original.TriggerMethod, OutsideRth: original.OutsideRth,
					TotalQuantity: 100, Remaining: 100 - tc.filled, Filled: tc.filled, TrailStopPrice: ratchet, TrailingPercent: original.TrailingPercent, AuxPrice: original.AuxPrice, Status: "Submitted"}}}, scope, nil
			}
			position = tc.position
			rig.server.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
				return []*ibkrlib.RawPosition{{Account: scope.Account, Position: position, Contract: ibkrlib.Contract{ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD"}}}, true
			}
			lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYN", ConID: 101, AccountID: scope.Account, AccountMode: scope.Mode}
			automaticLockUpdate(t, rig.server, 0, "locked-mandated-stop", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
			steps, paused := rig.server.planProtectiveStopGuardPass(t.Context(), scope)
			if len(steps) != 1 || steps[0].Kind != protectiveStopGuardResize {
				t.Fatalf("missing full-position resize: %s %+v", paused, steps)
			}
			err = rig.server.protectiveStopGuardWrite(t.Context(), steps[0])
			if tc.position > 100-tc.filled {
				if err == nil || !strings.Contains(err.Error(), "locked") || broker.count() != 1 {
					t.Fatalf("automatic lock permitted increasing protection: err=%v calls=%d", err, broker.count())
				}
				return
			}
			if err != nil || broker.count() != 2 {
				t.Fatalf("automatic lock blocked safe full-position shrink: err=%v calls=%d", err, broker.count())
			}
			got := broker.orders[1]
			if got.OrderID != original.OrderID || got.OrderRef != original.OrderRef || got.TotalQty != int(tc.position+tc.filled) || got.TrailStopPrice != ratchet || got.TrailingPercent != original.TrailingPercent || got.AuxPrice != original.AuxPrice {
				t.Fatalf("safety shrink changed stop identity, fill accounting or ratchet: %+v", got)
			}
			if event, ok := stopGuardJournalOrigin(t, rig.server, orderJournalEventModifyRequested); !ok || event.OrderRef != original.OrderRef || event.ReservedOrderID != original.OrderID || event.Origin != rpc.OrderOriginDaemonProtectiveGuard {
				t.Fatalf("safety shrink was not journaled as an exact existing-order modification: %+v", event)
			}
		})
	}
}
