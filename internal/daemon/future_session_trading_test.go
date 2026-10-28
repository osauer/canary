//go:build trading

package daemon

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"testing"
)

func TestFutureSessionMustHoldAtSend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close bool
		late  bool
	}{{"sale closes at wire", false, true}, {"buyback closed before submit", true, false}, {"buyback closes at wire", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []rpc.PositionView{mandateReductionStock("SPY", 9101, 3000, 50)}
			if tc.close {
				rows = append(rows, mandateReductionFuture(hedgeTestConID, -2, 6000))
			}
			s := hedgeOrderServer(t, 25, rows...)
			r := newHedgeSendRig(t, s)
			token := ""
			if tc.close {
				preview, err := s.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: hedgeTestConID, Percent: 100, IncludeHedges: true})
				if err != nil || preview.PreviewToken == "" {
					t.Fatalf("preview=%+v err=%v", preview, err)
				}
				token = preview.PreviewToken
			} else {
				preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
				if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
					t.Fatalf("preview=%+v", preview)
				}
				token = preview.Legs[0].PreviewToken
			}
			if tc.late {
				s.orderWriteBeforeBrokerSend = func() { hedgeClosedSession(s) }
			} else {
				hedgeClosedSession(s)
			}
			res, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
			if err == nil || len(r.sends()) != 0 {
				t.Fatalf("US session closed: accepted=%v err=%v sends=%d", res != nil && res.Accepted, err, len(r.sends()))
			}
		})
	}
}

func TestFutureSessionDoesNotBlockCancellation(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	newHedgeSendRig(t, s)
	auth, binding, err := s.authorizeBrokerWriteTransaction("", true)
	if err != nil || !auth.Allowed {
		t.Fatalf("cancel authority: %v %+v", err, auth.Blockers)
	}
	binding.riskBound = true
	binding.riskDraft = rpc.OrderDraft{Contract: rpc.ContractParams{SecType: "FUT"}}
	hedgeClosedSession(s)
	guard, release := s.brokerWireGuard(binding, auth.Status, true)
	defer release()
	if err := guard(); err != nil {
		t.Fatalf("closed session blocked cancellation: %v", err)
	}
}
