package daemon

import (
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
	"time"
)

// Spending more loss room is an addition under the approved classification,
// even when the future improves the index fall and the added loss fits.
func TestIndexHedgeIncreasingLossCannotBypassDrawdownBrake(t *testing.T) {
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
	if blocker, _ := s.riskPolicyBrake(draft, impact); blocker == nil || blocker.Code != drawdownBrakeCode {
		t.Fatalf("loss %g -> %g bypassed active brake: %+v", before.Loss.TotalBase, after.Loss.TotalBase, blocker)
	}
	draft.Action = rpc.OrderActionBuy
	if blocker, _ := s.riskPolicyBrake(draft, rpc.OrderPositionImpact{Before: -5, After: 0, Effect: rpc.OrderPositionEffectClose}); blocker != nil {
		t.Fatalf("actual hedge close blocked: %+v", blocker)
	}
}
