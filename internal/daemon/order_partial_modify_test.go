package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestPartialModifyWhatIfUsesCumulativeQuantity(t *testing.T) {
	for _, secType := range []string{"STK", "OPT"} {
		for _, action := range []string{rpc.OrderActionBuy, rpc.OrderActionSell} {
			t.Run(secType+"/"+action, func(t *testing.T) {
				draft := rpc.OrderDraft{Action: action, Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYN", SecType: secType}, Quantity: 50, OrderType: rpc.OrderTypeLMT, LimitPrice: 10, TIF: rpc.OrderTIFDay}
				view := rpc.OrderView{ReservedOrderID: 1002, Quantity: 100, Filled: 20, Remaining: 80}
				got := 0
				s := &Server{orderPreviewWhatIf: func(_ context.Context, wire rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
					got = wire.Quantity
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
				}}
				if _, err := s.fetchModifyPreviewWhatIfBound(t.Context(), rpc.TradingStatus{}, view, draft, time.Second, nil); err != nil {
					t.Fatal(err)
				}
				if got != 70 {
					t.Fatalf("WhatIf cumulative quantity=%d; want70 for50 remaining plus20 filled", got)
				}
				if draft.Quantity != 50 {
					t.Fatal("signed remaining quantity was changed")
				}
			})
		}
	}
}

func TestPartialModifyWhatIfRejectsUnrepresentableFills(t *testing.T) {
	for _, filled := range []float64{-1, 0.5, math.NaN(), math.Inf(1)} {
		draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYN", SecType: "STK"}, Quantity: 50, OrderType: rpc.OrderTypeLMT, LimitPrice: 10}
		called := false
		s := &Server{orderPreviewWhatIf: func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
			called = true
			return rpc.OrderWhatIfResult{}, nil
		}}
		_, err := s.fetchModifyPreviewWhatIfBound(t.Context(), rpc.TradingStatus{}, rpc.OrderView{Filled: filled}, draft, time.Second, nil)
		if err == nil || called {
			t.Errorf("filled=%g reached WhatIf=%v err=%v", filled, called, err)
		}
	}
}
