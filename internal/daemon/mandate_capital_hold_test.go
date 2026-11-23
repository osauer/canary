package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/mandate"
)

// The capital hold is served typed: flow_suspected while it can still clear
// by itself, pnl_reset_unresolved once a restart left the P&L basis
// ambiguous, and nothing without a hold.
func TestMandateCapitalServesTheHoldTyped(t *testing.T) {
	c := testCoreConstitution()
	srv := coreTestServer(t, c, mandateTestPositions(100), 0.25)
	st := srv.riskCapital
	scope := srv.currentBrokerStateScope()
	if capital, _ := st.MandateCapitalForScope(c, scope, mandateTestNow); capital.Hold != nil {
		t.Fatalf("a hold without a suspicion: %+v", capital.Hold)
	}
	raised := mandateTestNow.Add(-time.Minute)
	st.mu.Lock()
	st.state.Mandate.FlowSuspect = &mandateFlowSuspect{At: raised, Base: mandateFlowBase{At: mandateTestNow.Add(-3 * time.Minute), EquityBase: 280000},
		Unexplained: 20000, ThresholdAt: 3000, MoveBase: 20000}
	st.mu.Unlock()
	capital, _ := st.MandateCapitalForScope(c, scope, mandateTestNow)
	if h := capital.Hold; h == nil || h.Code != mandate.CapitalHoldFlowSuspected || !h.RaisedAt.Equal(raised) || h.Reason == "" || h.Reason != capital.Reasons[0] {
		t.Fatalf("flow suspected: %+v reasons %v", h, capital.Reasons)
	}
	st.mu.Lock()
	st.state.Mandate.FlowSuspect.PnLResetAmbiguousAt = mandateTestNow.Add(-30 * time.Second)
	st.mu.Unlock()
	capital, _ = st.MandateCapitalForScope(c, scope, mandateTestNow)
	if h := capital.Hold; h == nil || h.Code != mandate.CapitalHoldPnLResetUnresolved || !h.RaisedAt.Equal(raised) || h.Reason != capital.Reasons[0] {
		t.Fatalf("reset unresolved: %+v reasons %v", h, capital.Reasons)
	}
}
