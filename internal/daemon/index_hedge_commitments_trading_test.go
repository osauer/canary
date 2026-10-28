//go:build trading

package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"testing"
)

func TestIndexHedgeCapCountsPendingOtherMonths(t *testing.T) {
	for _, conID := range []int{hedgeTestConID, hedgeTestConID + 1} {
		t.Run(string(rune('a'+conID-hedgeTestConID)), func(t *testing.T) {
			s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
			r := newHedgeSendRig(t, s)
			preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
			if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
				t.Fatalf("preview %+v", preview)
			}
			scope := s.currentBrokerStateScope()
			err := s.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: s.orderNow(), OrderRef: "synthetic-pending-hedge", PreviewTokenID: "synthetic-pending-token", ReservedOrderID: 301, Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002", ConID: conID, Symbol: "MES", SecType: "FUT", Currency: "USD", Multiplier: 5, Expiry: "20270319", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeLMT, LimitPrice: 6000, Quantity: 25, Remaining: 25, Status: "Submitted", Source: hedgeOrderSource})
			if err != nil {
				t.Fatal(err)
			}
			result, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}})
			if len(r.sends()) != 0 {
				t.Fatalf("cap25: existingpending25 oncon%d plus reviewed5 sent%d; result=%+v err=%v", conID, len(r.sends()), result, err)
			}
		})
	}
}

func TestIndexHedgeCapCountsBrokerPending(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
		t.Fatalf("preview %+v", preview)
	}
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		scope := s.currentBrokerStateScope()
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow(), Orders: []ibkr.OrderLifecycleEvent{{Type: ibkr.OrderLifecycleEventOpenOrder, OrderID: 1002, ClientID: 77, ClientIDPresent: true, Account: scope.Account, Symbol: "MES", SecType: "FUT", ConID: hedgeTestConID, Currency: "USD", Multiplier: 5, Action: "SELL", OrderType: "LMT", LimitPrice: 6000, TotalQuantity: 25, Remaining: 25, Status: "Submitted"}}}, scope, nil
	}
	result, err := s.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeHedge, PreviewTokens: []string{preview.Legs[0].PreviewToken}})
	if len(r.sends()) != 0 {
		t.Fatalf("brokerpending25 + reviewed5 exceeds max25 but sends%d, result=%+v err=%v", len(r.sends()), result, err)
	}
}
