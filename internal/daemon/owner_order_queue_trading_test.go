//go:build trading

package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func prepareOwnerTrim(t *testing.T, rig *automaticTestRig, quantity int, price float64) rpc.TradeProposalQueuePrepareResult {
	t.Helper()
	p := rig.trimProposal(10)
	rev := rig.install(p)
	scope := rig.engine.currentScope()
	q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, Key: p.Key, Revision: rev, AccountID: scope.Account, AccountMode: scope.Mode, Quantity: quantity, WorstPrice: price})
	if err != nil || !q.Accepted {
		t.Fatalf("owner prepare: %+v %v", q, err)
	}
	return q
}

func TestOwnerQueueExactMidpointAndQuantity(t *testing.T) {
	rig, broker := newQueueTradingRig(t)
	q := prepareOwnerTrim(t, rig, 2, 24.99)
	if q.Queue.Terms.Version != 2 || q.Queue.Terms.Concession != 0 || q.Queue.Terms.MaxQuantity != 2 {
		t.Fatalf("wrong approved terms: %+v", q.Queue.Terms)
	}
	rig.queueArm(q)
	rig.now = queueWindowOpen
	rig.queueCycle()
	if broker.count() != 1 {
		t.Fatalf("send: %+v", rig.queued(q.Queue.Terms.QueueID))
	}
	order := broker.orders[0]
	if order.TotalQty != 2 || order.LmtPrice != 25 || order.AdaptivePriority != "Normal" {
		t.Fatalf("not exact midpoint order: %+v", order)
	}
	rig.restart()
	rig.queueCycle()
	if broker.count() != 1 {
		t.Fatal("restart resent an attempted order")
	}
}

func TestOwnerQueueWaitsForPriceBoundThenExpires(t *testing.T) {
	rig, broker := newQueueTradingRig(t)
	q := prepareOwnerTrim(t, rig, 2, 25.01)
	rig.queueArm(q)
	rig.now = queueWindowOpen
	rig.queueCycle()
	if broker.count() != 0 || rig.queued(q.Queue.Terms.QueueID).State != rpc.QueuedAuthHeld {
		t.Fatalf("price floor ignored: %+v", rig.queued(q.Queue.Terms.QueueID))
	}
	rig.now = q.Queue.Terms.NotAfter
	rig.queueCycle()
	if rig.queued(q.Queue.Terms.QueueID).State != rpc.QueuedAuthExpired {
		t.Fatal("finite order did not expire")
	}
}

func TestOwnerQueueWorkingExitBlocksAnother(t *testing.T) {
	rig, broker := newQueueTradingRig(t)
	q := prepareOwnerTrim(t, rig, 2, 24.99)
	rig.queueArm(q)
	rig.now = queueWindowOpen
	scope := rig.engine.currentScope()
	rig.server.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now, Orders: []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: scope.Account, ConID: 101, Symbol: "SYN", SecType: "STK", Action: "SELL", OrderType: "TRAIL", TotalQuantity: 40, Remaining: 40, Status: "Submitted"}}}, scope, nil
	}
	rig.queueCycle()
	if broker.count() != 0 || rig.queued(q.Queue.Terms.QueueID).State != rpc.QueuedAuthHeld {
		t.Fatal("sent beside a working stop")
	}
}

func TestOwnerProtectionQueueDoesNotExpireAndFlatCancels(t *testing.T) {
	rig, broker := newQueueTradingRig(t)
	p := rig.stopProposal()
	rev := rig.install(p)
	scope := rig.engine.currentScope()
	q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, FullPosition: true, Key: p.Key, Revision: rev, AccountID: scope.Account, AccountMode: scope.Mode})
	if err != nil || !q.Accepted {
		t.Fatalf("stop prepare: %+v %v", q, err)
	}
	if !q.Queue.Terms.NotAfter.IsZero() || !q.Queue.Terms.NotBefore.Equal(rig.now) {
		t.Fatal("stop gained a delay or expiry")
	}
	rig.queueArm(q)
	rig.server.gatewayReadyForTrading = func() bool { return false }
	rig.now = rig.now.Add(30 * 24 * time.Hour)
	rig.queueCycle()
	if !rig.queued(q.Queue.Terms.QueueID).waiting() {
		t.Fatal("stop queue expired")
	}
	rig.server.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) { return nil, true }
	rig.queueCycle()
	if broker.count() != 0 || rig.queued(q.Queue.Terms.QueueID).State != rpc.QueuedAuthCancelled {
		t.Fatalf("flat did not cancel: %+v", rig.queued(q.Queue.Terms.QueueID))
	}
}

func TestOwnerProtectionOutcomeUsesResizedBrokerQuantity(t *testing.T) {
	r := queuedAuthRecord{QuantitySent: 100, Terms: rpc.QueuedAuthTerms{Version: 2, OwnerOrder: &rpc.QueuedOwnerOrder{FullPosition: true}}}
	for _, tc := range []struct {
		quantity, filled float64
		lifecycle, want  string
	}{
		{60, 60, rpc.OrderLifecycleFilled, rpc.QueuedAuthFilled},
		{200, 120, rpc.OrderLifecycleCancelled, rpc.QueuedAuthPartiallyFilled},
		{0, 60, rpc.OrderLifecycleFilled, rpc.QueuedAuthFailed},
	} {
		_, state, _, _, done := queuedRecordOrderResolution(rpc.OrderView{Quantity: tc.quantity, Filled: tc.filled, LifecycleStatus: tc.lifecycle}, r)
		if !done || state != tc.want || r.QuantitySent != 100 {
			t.Fatalf("resized outcome = %s done=%v audit=%d", state, done, r.QuantitySent)
		}
	}
}

func TestOwnerProtectionCancelKeepsAmbiguousAttempt(t *testing.T) {
	for _, flat := range []bool{false, true} {
		rig, broker := newQueueTradingRig(t)
		p := rig.stopProposal()
		rev := rig.install(p)
		scope := rig.engine.currentScope()
		q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, FullPosition: true, Key: p.Key, Revision: rev, AccountID: scope.Account, AccountMode: scope.Mode})
		if err != nil || !q.Accepted {
			t.Fatalf("prepare: %+v %v", q, err)
		}
		rig.queueArm(q)
		id := q.Queue.Terms.QueueID
		if err := rig.engine.queued.update(t.Context(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
			r := records[id]
			r.State, r.OrderRef, r.PreviewTokenID = rpc.QueuedAuthHeld, "uncertain-owner-stop", "uncertain-token"
			return []queuedAuthEvent{r.note("fixture", r.State, rig.now)}
		}); err != nil {
			t.Fatal(err)
		}
		if flat {
			rig.server.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) { return nil, true }
			rig.engine.endFlatProtectionQueues(t.Context())
		} else {
			res, err := rig.engine.QueueCancel(t.Context(), rpc.TradeProposalQueueCancelParams{QueueID: id, Origin: rpc.OrderOriginHumanTTY})
			if err != nil || len(res.InFlight) != 1 || len(res.Queues) != 0 {
				t.Fatalf("uncertain cancellation: %+v %v", res, err)
			}
		}
		r := rig.queued(id)
		if r.State != rpc.QueuedAuthHeld || !r.CancelRequested || r.MandateEndedAt.IsZero() || r.OrderRef == "" || r.PreviewTokenID == "" || broker.count() != 0 {
			t.Fatalf("lost uncertain stop identity: %+v", r)
		}
	}
}

func TestOwnerQueueFinalAuthorityChecksSignedWindowAndCancellation(t *testing.T) {
	rig, broker := newQueueTradingRig(t)
	q := prepareOwnerTrim(t, rig, 2, 24.99)
	rig.queueArm(q)
	r := rig.queued(q.Queue.Terms.QueueID)
	rig.server.queuedGrant.Store(&queuedWriteGrant{QueueID: r.Terms.QueueID, Key: r.Terms.Key, Bucket: r.Terms.Bucket})
	defer rig.server.queuedGrant.Store(nil)
	for _, at := range []time.Time{r.Terms.NotBefore.Add(-time.Nanosecond), r.Terms.NotAfter, r.Terms.NotAfter.Add(time.Second)} {
		rig.now = at
		if len(rig.server.daemonOwnerQueuedOriginBlockers()) == 0 {
			t.Fatal("expired or early signed authority remained valid")
		}
	}
	rig.now = r.Terms.NotBefore.Add(time.Second)
	if b := rig.server.daemonOwnerQueuedOriginBlockers(); len(b) != 0 {
		t.Fatalf("valid signed window blocked: %+v", b)
	}
	auth, binding, err := rig.server.authorizeBrokerWriteTransaction(rpc.OrderOriginDaemonOwnerQueued, false)
	if err != nil || !auth.Allowed {
		t.Fatalf("admission: %+v %v", auth, err)
	}
	guard, release := rig.server.brokerWireGuard(binding, auth.Status, false)
	defer release()
	rig.now = r.Terms.NotAfter
	if err := guard(); err == nil || !strings.Contains(err.Error(), "signed queue send window") {
		t.Fatalf("first byte crossed the signed window: %v", err)
	}
	rig.now = r.Terms.NotBefore.Add(time.Second)
	_, err = rig.engine.QueueCancel(t.Context(), rpc.TradeProposalQueueCancelParams{QueueID: r.Terms.QueueID, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || len(rig.server.daemonOwnerQueuedOriginBlockers()) == 0 || broker.count() != 0 {
		t.Fatal("cancelled grant retained authority")
	}
}

func TestFullPositionStopPlanTracksSizeAndRatchet(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	view := rpc.OrderView{OrderRef: "owner-stop", PreviewTokenID: "signed", ReservedOrderID: 100, Account: scope.Account, Mode: scope.Mode, Open: true, ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD", Action: "SELL", OrderType: "TRAIL", Quantity: 100, Remaining: 100, TIF: "GTC", Trail: &rpc.OrderTrailSpec{TrailingPercent: new(2.0), InitialStopPrice: 95}}
	order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD", Action: "SELL", OrderType: "TRAIL", TIF: "GTC", TrailingPercent: 2, TotalQuantity: 100, Remaining: 100, TrailStopPrice: 98, Status: "Submitted"}
	for _, size := range []float64{60, 120} {
		positions := []*ibkrlib.RawPosition{{Account: scope.Account, Position: size, Contract: ibkrlib.Contract{ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD"}}}
		steps := planProtectiveStopGuard([]brokerWorkingOrder{{Order: order, Journal: &view}}, positions, scope, map[string]bool{view.OrderRef: true})
		if len(steps) != 1 || steps[0].Kind != protectiveStopGuardResize || steps[0].Target != int(size) || steps[0].View.Trail.InitialStopPrice != 98 || *steps[0].View.Trail.TrailingPercent != 2 {
			t.Fatalf("size %v: %+v", size, steps)
		}
	}
	positions := []*ibkrlib.RawPosition{{Account: scope.Account, Position: 120, Contract: ibkrlib.Contract{ConID: 101, Symbol: "SYN", SecType: "STK"}}}
	if steps := planProtectiveStopGuard([]brokerWorkingOrder{{Order: order, Journal: &view}}, positions, scope); len(steps) != 0 {
		t.Fatal("legacy stop gained growth authority")
	}
	order.TrailStopPrice = 0
	if steps := planProtectiveStopGuard([]brokerWorkingOrder{{Order: order, Journal: &view}}, positions, scope, map[string]bool{view.OrderRef: true}); len(steps) != 0 {
		t.Fatal("resized without a broker ratchet")
	}
	if total, err := protectionModifyTotal(120, 20); err != nil || total != 140 {
		t.Fatalf("partial-fill total=%d %v", total, err)
	}
}

func TestOwnerQueueRecoveryRequiresDefiniteUnsentEvidence(t *testing.T) {
	rec := queuedAuthRecord{Terms: rpc.QueuedAuthTerms{Version: 2}, PreviewTokenID: "token"}
	base := []orderJournalEvent{{Type: orderJournalEventSendAttempted, PreviewTokenID: "token", OrderRef: "order"}}
	for _, tc := range []struct {
		disposition string
		ack         bool
		want        orderSendOutcome
	}{
		{"", false, orderSendUnclear}, {"definitely_unsent", false, orderSendFailedUnsent},
		{"definitely_unsent", true, orderSendReached}, {"may_have_written", false, orderSendUnclear},
	} {
		events := append(append([]orderJournalEvent{}, base...), orderJournalEvent{Type: orderJournalEventSendError, PreviewTokenID: "token", SendDisposition: ibkrlib.SendDisposition(tc.disposition)})
		if tc.ack {
			events = append(events, orderJournalEvent{Type: orderJournalEventBrokerAcknowledged, PreviewTokenID: "token", OrderRef: "order"})
		}
		if got, _ := queuedJournalSendOutcome(events, rec); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func TestOwnerQueueBuyAndAddUseFreshCapacity(t *testing.T) {
	for _, before := range []float64{0, 40} {
		for _, cash := range []float64{10000, 1} {
			t.Run(fmt.Sprintf("before_%g_cash_%g", before, cash), func(t *testing.T) {
				rig, broker := newQueueTradingRig(t)
				s := rig.server
				in := stockAddTestInput()
				in.CurrentQuantity = before
				s.rulebookPolicies = &rulebookPolicyManager{active: in.Rulebook, status: rpc.RulebookPolicyStatus{Status: "active"}}
				s.stockAddEvidenceForTest = func(_ context.Context, p rpc.AddParams) (stockAddEvidence, error) {
					copy := in
					copy.Requested = p.Quantity
					copy.Price = p.LimitPrice
					return stockAddEvidence{input: copy, contract: p.Contract, review: rpc.AddReview{PolicyFingerprint: s.riskPolicies.active.FingerprintKey(), RulebookFingerprint: in.Rulebook.FingerprintKey(), CashPolicyFingerprint: fingerprintProtectionPolicy(s.protectionPolicies.active).Key, AsOf: s.orderNow()}}, nil
				}
				s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
					return stockAddTestWhatIf(5), nil
				}
				s.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, q int) (rpc.OrderPositionImpact, error) {
					return rpc.OrderPositionImpact{Before: before, After: before + float64(q), Effect: classifyPositionEffect(before, before+float64(q))}, nil
				}
				scope := rig.engine.currentScope()
				price := 25.01
				q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode, Order: &rpc.OrderPreviewParams{Contract: stockAddTestParams().Contract, Action: "BUY", Quantity: 2, OrderType: "LMT", TIF: "DAY", LimitPrice: &price}})
				if err != nil || !q.Accepted {
					t.Fatalf("prepare %+v %v", q, err)
				}
				rig.queueArm(q)
				in.FreeCash = cash
				rig.now = queueWindowOpen
				rig.queueCycle()
				if cash < price {
					if broker.count() != 0 {
						t.Fatal("spent cash still sent")
					}
					return
				}
				if broker.count() != 1 {
					t.Fatalf("valid buy held %+v", rig.queued(q.Queue.Terms.QueueID))
				}
				if broker.orders[0].TotalQty != 2 || broker.orders[0].LmtPrice != 25 {
					t.Fatalf("quantity/price changed %+v", broker.orders[0])
				}
			})
		}
	}
}

func TestOwnerQueueCapacityExemptionIsScopedAndKeepsUncertainty(t *testing.T) {
	rig, _ := newQueueTradingRig(t)
	s := rig.server
	scope := rig.engine.currentScope()
	price := 25.01
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 2, rpc.OrderPositionEffectOpen)
	q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, AccountID: scope.Account, AccountMode: scope.Mode, Order: &rpc.OrderPreviewParams{Contract: stockAddTestParams().Contract, Action: "BUY", Quantity: 2, LimitPrice: &price}})
	if err != nil || !q.Accepted {
		t.Fatalf("prepare %+v %v", q, err)
	}
	rig.queueArm(q)
	r := rig.queued(q.Queue.Terms.QueueID)
	a := &ownerQueueAdmission{QueueID: r.Terms.QueueID, ConID: 101, Quantity: 2, Cap: price}
	s.queuedGrant.Store(&queuedWriteGrant{QueueID: r.Terms.QueueID, Key: r.Terms.Key, Bucket: r.Terms.Bucket})
	defer s.queuedGrant.Store(nil)
	if !s.stockAddQueuedInstruction(scope) {
		t.Fatal("manual sizing inherited executor grant")
	}
	if s.stockAddQueuedInstruction(scope, a) {
		t.Fatal("executing queue blocks itself")
	}
	other := r
	other.Terms.QueueID = "SyntheticOtherQueue0001"
	other.Terms.Contract.ConID = 202
	err = rig.engine.queued.update(t.Context(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		records[other.Terms.QueueID] = &other
		return []queuedAuthEvent{other.note("fixture", other.State, rig.now)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.stockAddQueuedInstruction(scope, a) {
		t.Fatal("two future intents deadlock")
	}
	err = rig.engine.queued.update(t.Context(), rig.now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		v := records[other.Terms.QueueID]
		v.OrderRef = "uncertain"
		v.PreviewTokenID = "spent"
		v.State = rpc.QueuedAuthHeld
		return []queuedAuthEvent{v.note("fixture", v.State, rig.now)}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !s.stockAddQueuedInstruction(scope, a) {
		t.Fatal("uncertain earlier exposure excluded")
	}
	wrong := *a
	wrong.Quantity++
	if s.ownerQueueAdmissionCurrent(scope, &wrong) {
		t.Fatal("changed quantity inherited grant")
	}
	_, err = rig.engine.QueueCancel(t.Context(), rpc.TradeProposalQueueCancelParams{QueueID: r.Terms.QueueID, Origin: rpc.OrderOriginHumanTTY})
	if err != nil {
		t.Fatal(err)
	}
	if s.ownerQueueAdmissionCurrent(scope, a) {
		t.Fatal("cancelled admission remained usable")
	}
}

func TestFullPositionStopResizesThroughBrokerPath(t *testing.T) {
	for _, tc := range []struct{ size, filled float64 }{{60, 0}, {120, 0}, {120, 20}} {
		size, filled := tc.size, tc.filled
		t.Run(fmt.Sprintf("size_%g_filled_%g", size, filled), func(t *testing.T) {
			rig, broker := newQueueTradingRig(t)
			position := 100.0
			rig.server.orderPreviewPositionImpact = func(_ context.Context, _ rpc.ContractParams, _ string, qty int) (rpc.OrderPositionImpact, error) {
				return rpc.OrderPositionImpact{Before: position, After: position - float64(qty), Effect: classifyPositionEffect(position, position-float64(qty))}, nil
			}
			p := rig.stopProposal()
			p.Quantity = 100
			p.PositionQuantity = 100
			rev := rig.install(p)
			scope := rig.engine.currentScope()
			q, err := rig.engine.QueuePrepare(t.Context(), rpc.TradeProposalQueuePrepareParams{OwnerOrder: true, FullPosition: true, Key: p.Key, Revision: rev, AccountID: scope.Account, AccountMode: scope.Mode})
			if err != nil || !q.Accepted {
				t.Fatalf("prepare %+v %v", q, err)
			}
			rig.queueArm(q)
			rig.now = queueWindowOpen
			rig.queueCycle()
			if broker.count() != 1 {
				t.Fatalf("stop not sent: %+v", rig.queued(q.Queue.Terms.QueueID))
			}
			rec := rig.queued(q.Queue.Terms.QueueID)
			events := journalEventsForToken(t, rig.server, rec.PreviewTokenID)
			var ack orderJournalEvent
			for _, ev := range events {
				if ev.Type == orderJournalEventSendAttempted {
					ack = ev
				}
			}
			ack.Type = orderJournalEventBrokerAcknowledged
			ack.SendState = orderSendStateBrokerAcknowledged
			ack.Status = "Submitted"
			ack.Remaining = 100 - filled
			ack.Filled = filled
			ack.At = rig.now
			if err := rig.server.orderJournal.Append(ack); err != nil {
				t.Fatal(err)
			}
			o := broker.orders[0]
			ratchet := o.TrailStopPrice + 0.1
			rig.server.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now, Orders: []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: o.OrderID, ClientID: o.ClientID, ClientIDPresent: true, Account: scope.Account, ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD", Action: "SELL", OrderType: o.OrderType, TIF: o.TIF, TriggerMethod: o.TriggerMethod, OutsideRth: o.OutsideRth, TotalQuantity: 100, Remaining: 100 - filled, Filled: filled, TrailStopPrice: ratchet, TrailingPercent: o.TrailingPercent, AuxPrice: o.AuxPrice, Status: "Submitted"}}}, scope, nil
			}
			position = size
			rig.server.protectiveStopGuardPositionsForTest = func(context.Context, brokerStateScope) ([]*ibkrlib.RawPosition, bool) {
				return []*ibkrlib.RawPosition{{Account: scope.Account, Position: position, Contract: ibkrlib.Contract{ConID: 101, Symbol: "SYN", SecType: "STK", Currency: "USD"}}}, true
			}
			steps, paused := rig.server.planProtectiveStopGuardPass(t.Context(), scope)
			if len(steps) != 1 {
				t.Fatalf("no resize %s %+v mandate %+v", paused, steps, rec)
			}
			if err := rig.server.protectiveStopGuardWrite(t.Context(), steps[0]); err != nil {
				t.Fatal(err)
			}
			if broker.count() != 2 {
				t.Fatal("no modify")
			}
			got := broker.orders[1]
			if got.TotalQty != int(size+filled) || got.TrailStopPrice != ratchet || got.TrailingPercent != o.TrailingPercent || got.AuxPrice != o.AuxPrice || got.OrderID != o.OrderID {
				t.Fatalf("trail changed %+v", got)
			}
		})
	}
}
