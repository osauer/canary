package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func appendReduceComboReservation(t *testing.T, srv *Server, quantity, remaining float64, legs []rpc.StrategyOrderLeg) {
	t.Helper()
	scope := srv.currentBrokerStateScope()
	var group *rpc.StrategyOrderDraft
	if legs != nil {
		group = &rpc.StrategyOrderDraft{Units: int(quantity), GuaranteedCombo: true, Legs: legs}
	}
	if err := srv.orderJournal.Append(orderJournalEvent{
		Type: orderJournalEventSendAttempted, At: srv.now(),
		OrderRef: "synthetic-combo-reservation", PreviewTokenID: "synthetic-combo-token", ReservedOrderID: 301,
		Account: scope.Account, Mode: scope.Mode, ClientID: 31, Endpoint: "127.0.0.1:4002",
		Symbol: "SYN", SecType: "BAG", Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT,
		Quantity: quantity, Remaining: remaining, Filled: quantity - remaining, Status: "Submitted", StrategyGroup: group,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReduceReservesWorkingComboLegs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		quantity, remaining float64
		leg                 *rpc.StrategyOrderLeg
		wantCommitted       float64
		wantBlocker         string
	}{
		{name: "opposite parent action still sells held calls", quantity: 5, remaining: 5,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: -1, Action: rpc.OrderActionSell}, wantBlocker: "reduce_already_working"},
		{name: "partial fill counts remaining times ratio", quantity: 3, remaining: 1,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: -2, Action: rpc.OrderActionSell}, wantCommitted: 2},
		{name: "opposite leg direction leaves capacity", quantity: 5, remaining: 5,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: 1, Action: rpc.OrderActionBuy}},
		{name: "other contract leaves capacity", quantity: 5, remaining: 5,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID + 1}, Ratio: -1, Action: rpc.OrderActionSell}},
		{name: "unknown legs block", quantity: 5, remaining: 5, wantBlocker: "orders_unavailable"},
		{name: "unknown ratio blocks", quantity: 5, remaining: 5,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Action: rpc.OrderActionSell}, wantBlocker: "orders_unavailable"},
		{name: "unknown leg action blocks", quantity: 5, remaining: 5,
			leg: &rpc.StrategyOrderLeg{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: 1}, wantBlocker: "orders_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newReducePreviewTestServer(t, reduceTestOptionRow(5))
			srv.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
				return []*ibkrlib.RawPosition{{Account: srv.currentBrokerStateScope().Account, Contract: ibkrlib.Contract{ConID: reduceTestOptionConID}, Position: 5}}, nil
			}
			var legs []rpc.StrategyOrderLeg
			if tc.leg != nil {
				legs = []rpc.StrategyOrderLeg{*tc.leg}
			}
			appendReduceComboReservation(t, srv, tc.quantity, tc.remaining, legs)
			result, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestOptionConID, Percent: 100})
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if len(result.Blockers) > 0 {
				got = result.Blockers[0].Code
			}
			if got != tc.wantBlocker {
				t.Fatalf("blocker = %q, want %q; accepted=%t quantity=%d", got, tc.wantBlocker, result.Accepted, result.ReduceQuantity)
			}
			if tc.wantBlocker != "" {
				if result.Accepted || result.PreviewToken != "" {
					t.Fatal("unknown or fully committed option capacity minted a reduction token")
				}
			} else if !result.Accepted || result.CommittedQuantity != tc.wantCommitted || result.ReduceQuantity != 5-int(tc.wantCommitted) {
				t.Fatalf("accepted=%t committed=%g quantity=%d, want committed=%g quantity=%d", result.Accepted, result.CommittedQuantity, result.ReduceQuantity, tc.wantCommitted, 5-int(tc.wantCommitted))
			}
		})
	}
}

func TestReduceTokenRechecksWorkingComboLegs(t *testing.T) {
	t.Parallel()
	srv, pos := newReducePreviewTestServer(t, reduceTestOptionRow(5))
	preview, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestOptionConID, Percent: 100})
	if err != nil || !preview.Accepted {
		t.Fatalf("preview accepted=%t err=%v", preview.Accepted, err)
	}
	appendReduceComboReservation(t, srv, 5, 5, []rpc.StrategyOrderLeg{{
		Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: -1, Action: rpc.OrderActionSell,
	}})
	out := srv.reduceTokenLeg(t.Context(), preview.PreviewToken, pos, 0, "", nil)
	if len(out.blockers) == 0 || out.blockers[0].Code != "reduce_already_working" {
		t.Fatalf("submit blockers = %+v, want reduce_already_working", out.blockers)
	}
}
