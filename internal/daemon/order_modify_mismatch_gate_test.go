//go:build trading

package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func seedProtectiveTrailJournal(t *testing.T, srv *Server, now time.Time) {
	t.Helper()
	amount := 8.5
	if err := srv.orderJournal.Append(orderJournalEvent{
		At:              now.Add(-time.Minute),
		Type:            orderJournalEventBrokerAcknowledged,
		OrderRef:        "ord-prot",
		PreviewTokenID:  "tok-prot",
		ReservedOrderID: 1002,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Source:          proposalOrderSource,
		OpenClose:       "C",
		Symbol:          "AMD",
		SecType:         "STK",
		ConID:           4391,
		Exchange:        "SMART",
		Currency:        "USD",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        100,
		Remaining:       100,
		Trail:           &rpc.OrderTrailSpec{TrailingAmount: &amount, InitialStopPrice: 90},
		Status:          "Submitted",
		SendState:       orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}
}

func protectiveReduceDraft(quantity int) rpc.OrderDraft {
	amount := 8.5
	return rpc.OrderDraft{
		Action:    rpc.OrderActionSell,
		Contract:  rpc.ContractParams{ConID: 4391, Symbol: "AMD", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Quantity:  quantity,
		OrderType: rpc.OrderTypeTRAIL,
		TIF:       rpc.OrderTIFGTC,
		Strategy:  rpc.OrderStrategyBrokerTrail,
		OrderRef:  "ord-prot",
		OpenClose: "C",
		Source:    proposalOrderSource,
		Trail:     &rpc.OrderTrailSpec{TrailingAmount: &amount, InitialStopPrice: 90},
	}
}

func protectiveViewForToken(quantity float64) rpc.OrderView {
	return rpc.OrderView{
		OrderRef:        "ord-prot",
		ReservedOrderID: 1002,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Source:          proposalOrderSource,
		OpenClose:       "C",
		Symbol:          "AMD",
		SecType:         "STK",
		ConID:           4391,
		Exchange:        "SMART",
		Currency:        "USD",
		Action:          rpc.OrderActionSell,
		OrderType:       rpc.OrderTypeTRAIL,
		TIF:             rpc.OrderTIFGTC,
		Quantity:        quantity,
		Remaining:       quantity,
		Trail:           &rpc.OrderTrailSpec{TrailingAmount: new(8.5), InitialStopPrice: 90},
		Status:          "Submitted",
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       orderSendStateBrokerAcknowledged,
	}
}

func TestOrderModifyMismatchAcceptsExactReduceOnly(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, AllowStockShort: true})
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	seedProtectiveTrailJournal(t, srv, now)
	srv.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{Before: 50, Effect: rpc.OrderPositionEffectClose}, nil
	}
	var sent *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		copied := *order
		sent = &copied
		return nil
	}

	// Wrong quantity (full old size) is rejected at write time.
	token := mintModifyPreviewTokenForWriteTest(t, srv, protectiveViewForToken(100), protectiveReduceDraft(100))
	if _, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-prot", PreviewToken: token}); err == nil {
		t.Fatal("oversized modify on mismatched protective order must be rejected")
	} else if !strings.Contains(err.Error(), "exactly 50") {
		t.Fatalf("rejection should name the coverage, got %v", err)
	}
	if sent != nil {
		t.Fatalf("rejected modify still reached the broker: %+v", sent)
	}

	// Exact reduce-to-coverage passes.
	token = mintModifyPreviewTokenForWriteTest(t, srv, protectiveViewForToken(100), protectiveReduceDraft(50))
	res, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-prot", PreviewToken: token})
	if err != nil {
		t.Fatalf("reduce-to-coverage modify err = %v", err)
	}
	if !res.Accepted || sent == nil || sent.OrderID != 1002 || sent.TotalQty != 50 {
		t.Fatalf("modify result = %+v sent = %+v, want re-transmit of qty 50 on broker order 1002", res, sent)
	}
}

func TestOrderModifyMismatchFailsClosedWithoutPositions(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	seedProtectiveTrailJournal(t, srv, now)
	srv.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		return rpc.OrderPositionImpact{}, fmt.Errorf("positions unavailable")
	}
	called := false
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		called = true
		return nil
	}

	token := mintModifyPreviewTokenForWriteTest(t, srv, protectiveViewForToken(100), protectiveReduceDraft(50))
	if _, err := srv.modifyOrder(context.Background(), rpc.OrderModifyParams{ID: "ord-prot", PreviewToken: token}); err == nil {
		t.Fatal("protective modify without position evidence must fail closed")
	}
	if called {
		t.Fatal("failed-closed modify still reached the broker")
	}
}
