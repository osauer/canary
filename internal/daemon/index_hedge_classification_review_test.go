package daemon

import (
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
	"time"
)

// The latched drawdown brake leaves a hedge to neutral open, though it spends
// loss room (owner, 2026-11-06 14:23 CET), and still refuses one past neutral.
func TestIndexHedgeToNeutralPassesTheDrawdownBrakeAndPastItDoesNot(t *testing.T) {
	s, st, _, _ := brakeTestServer(t)
	keepMandateEvaluationAt(t, s, mandateReductionBook(mandateReductionStock("SPY", 9101, 3000, 50)), 0.5)
	st.now = s.now
	c := s.riskPolicies.snapshot().policy
	reconcileNow(t, st)
	st.Observe(260000, mandateTestNow.Add(-3*time.Minute), c, testLiveObserveScope, true)
	st.Observe(240000, mandateTestNow.Add(-2*time.Minute), c, testLiveObserveScope, true)
	if !st.Report(c, nil, brokerStateScope{}).BlockLatched {
		t.Fatal("fixture brake not latched")
	}
	draft := hedgeSale(5)
	impact := rpc.OrderPositionImpact{Before: 0, After: -5, Effect: rpc.OrderPositionEffectOpenShort}
	in, _, why := s.mandateCurrentInput()
	if in == nil {
		t.Fatal(why)
	}
	leg, ok := mandateOrderLeg(draft, in)
	if !ok {
		t.Fatal("fixture hedge unpriced")
	}
	before := mandate.Evaluate(*in)
	after := mandate.Evaluate(mandateInputWithLegs(*in, []mandate.Leg{leg}))
	if after.Loss.TotalBase <= before.Loss.TotalBase || after.Loss.TotalBase > after.Loss.RoomBase {
		t.Fatal("fixture must add loss within room")
	}
	if blocker, _ := s.riskPolicyBrake(draft, impact); blocker != nil {
		t.Fatalf("the brake refused a hedge to neutral (loss %g -> %g): %+v", before.Loss.TotalBase, after.Loss.TotalBase, blocker)
	}
	past := hedgeSale(6)
	if blocker, _ := s.riskPolicyBrake(past, rpc.OrderPositionImpact{Before: 0, After: -6, Effect: rpc.OrderPositionEffectOpenShort}); blocker == nil || blocker.Code != drawdownBrakeCode {
		t.Fatalf("a hedge past neutral bypassed the active brake: %+v", blocker)
	}
	draft.Action = rpc.OrderActionBuy
	if blocker, _ := s.riskPolicyBrake(draft, rpc.OrderPositionImpact{Before: -5, After: 0, Effect: rpc.OrderPositionEffectClose}); blocker != nil {
		t.Fatalf("actual hedge close blocked: %+v", blocker)
	}
}
