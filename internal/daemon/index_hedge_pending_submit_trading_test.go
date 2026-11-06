//go:build trading

package daemon

import (
	"fmt"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
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

// Placement measures the book as it reads once a working sale of a held line
// fills. A hedge previewed to neutral on 3,000 shares is refused when 1,500 of
// them are being sold by the time it is sent; a stop on those shares is no sale
// until the market reaches it, so it holds nothing back.
func TestIndexHedgePlacementCountsAWorkingSaleOfAHeldLine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		orderType string
		quantity  float64
		sends     int
		says      string
	}{
		{"limit sale past neutral", "LMT", 1500, 0, "would pass neutral"},
		{"unreadable sale", "LMT", 0, 0, "has no remaining quantity"},
		{"protective stop", "STP", 1500, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
			s.riskCapital = newTestRiskCapitalStore(t)
			s.riskCapital.now = s.now
			reconcileNow(t, s.riskCapital)
			s.riskCapital.Observe(260000, mandateTestNow.Add(-time.Minute), s.riskPolicies.snapshot().policy, testLiveObserveScope, true)
			seedMandateHistory(t, s, 0)
			rig := newHedgeSendRig(t, s)
			preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
			if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" || preview.Legs[0].ReduceQuantity != 5 {
				t.Fatalf("preview %+v, want SELL 5 with a token", preview)
			}
			sale := workingSale("SPY", "STK", 9101, tc.quantity)
			sale.OrderType = tc.orderType
			workingOrdersAt(s, sale)
			res, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}})
			if len(rig.sends()) != tc.sends {
				t.Fatalf("sent %d sales, want %d; result %+v err %v", len(rig.sends()), tc.sends, res, err)
			}
			if got := fmt.Sprintf("%+v %v", res, err); !strings.Contains(got, tc.says) {
				t.Fatalf("refusal %s does not say %s", got, tc.says)
			}
		})
	}
}
