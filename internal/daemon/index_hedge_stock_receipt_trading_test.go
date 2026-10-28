//go:build trading

package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"testing"
	"time"
)

func TestIndexHedgeAuthorityReceiptCannotUseOldStockQuantity(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	r := newHedgeSendRig(t, s)
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
		t.Fatalf("preview=%+v", preview)
	}
	s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkr.RawPosition, error) {
		return []*ibkr.RawPosition{{Account: s.currentBrokerStateScope().Account, Contract: ibkr.Contract{ConID: 9101, Symbol: "SPY", SecType: "STK", Currency: "EUR"}, Position: 1500}}, nil
	}
	_, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.Legs[0].PreviewToken})
	if err == nil || len(r.sends()) != 0 {
		t.Fatalf("old stock quantity sized hedge after half already sold: err=%v sends=%d", err, len(r.sends()))
	}
}
