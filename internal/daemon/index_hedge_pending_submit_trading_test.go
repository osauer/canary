//go:build trading

package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
	"time"
)

func TestIndexHedgeAuthorityPendingSalesCannotPassNeutral(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	s.riskCapital = newTestRiskCapitalStore(t)
	s.riskCapital.now = s.now
	reconcileNow(t, s.riskCapital)
	s.riskCapital.Observe(260000, mandateTestNow.Add(-time.Minute), s.riskPolicies.snapshot().policy, testLiveObserveScope, true)
	seedMandateHistory(t, s, 0)
	rig := newHedgeSendRig(t, s)
	for i := range 2 {
		preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
		if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
			if i == 1 {
				return
			}
			t.Fatalf("first preview failed: %+v", preview)
		}
		res, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}})
		if i == 0 && (err != nil || len(res.Legs) != 1 || !res.Legs[0].Placed) {
			t.Fatalf("first submit %v %+v", err, res)
		}
	}
	sent := rig.sends()
	qty := 0
	for _, o := range sent {
		qty += o.TotalQty
	}
	if qty > 5 {
		t.Fatalf("sent %d contracts in %d sales against a five-contract neutral budget with no fill; both classified from unchanged held book", qty, len(sent))
	}
}
