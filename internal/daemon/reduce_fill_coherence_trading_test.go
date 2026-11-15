//go:build trading

package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestReduceHoldsCapacityUntilFilledPositionCatchesUp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status      string
		filled, remaining float64
	}{
		{"filled", "Filled", 50, 0},
		{"filled_stop", "Filled", 50, 0},
		{"partial", "Submitted", 20, 30},
		{"cancelled_after_partial", "Cancelled", 20, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReduceTokenRig(t, reduceTestStockRow(100))
			first := r.preview(reduceTestStockConID, 50)
			direct := r.preview(reduceTestStockConID, 100)
			if result := r.submit(first.PreviewToken, "first"); !result.Accepted {
				t.Fatal("first order refused")
			}
			views, _, err := r.srv.loadOrderViews()
			index := slices.IndexFunc(views, func(view rpc.OrderView) bool { return view.PreviewTokenID == first.PreviewTokenID })
			if err != nil || index < 0 {
				t.Fatalf("first order missing from views: err=%v", err)
			}
			event := orderJournalEventFromView(views[index], orderJournalEventStatusUpdated, r.clock())
			event.Status, event.Filled, event.Remaining = tc.status, tc.filled, tc.remaining
			if tc.name == "filled_stop" {
				event.OrderType = "STP"
			}
			if err := r.srv.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			r.srv.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
				return []*ibkrlib.RawPosition{{Account: r.srv.currentBrokerStateScope().Account, Contract: ibkrlib.Contract{ConID: reduceTestStockConID}, Position: 100 - tc.filled}}, nil
			}
			preview, err := r.srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestStockConID, Percent: 100})
			if err == nil && preview.Accepted {
				t.Fatalf("stale held100 after %g filled accepted another reduction of %d", tc.filled, preview.ReduceQuantity)
			}
			placed, placeErr := r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: direct.PreviewToken})
			if placeErr == nil || (placed != nil && placed.Accepted) || len(r.sends()) != 1 {
				t.Fatalf("generic placement bypassed fill coherence: err=%v sends=%d", placeErr, len(r.sends()))
			}
			// The same exact quantity receipt releases capacity after the stream
			// catches up; a partial cancellation releases only its unfilled part.
			r.hold(reduceTestStockConID, 100-tc.filled)
			preview, err = r.srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestStockConID, Percent: 100})
			want := int(100 - tc.filled - tc.remaining)
			if err != nil || !preview.Accepted || preview.ReduceQuantity != want {
				t.Fatalf("coherent position: err=%v result=%+v, want %d available", err, preview, want)
			}
			placed, placeErr = r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if placeErr != nil || placed == nil || !placed.Accepted || len(r.sends()) != 2 || r.sends()[1].TotalQty != want {
				t.Fatalf("caught-up reduction: err=%v sends=%d, want %d sent", placeErr, len(r.sends()), want)
			}
		})
	}
}

func TestReduceFillDuringSnapshotOrBeforeWireRefuses(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"snapshot", "wire_without_prior_fill"} {
		t.Run(phase, func(t *testing.T) {
			r := newReduceTokenRig(t, reduceTestStockRow(100))
			generation := uint64(1)
			r.srv.reduceFillGenerationForTest = func() uint64 { return generation }
			r.srv.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
				generation++
				return []*ibkrlib.RawPosition{{Account: r.srv.currentBrokerStateScope().Account, Contract: ibkrlib.Contract{ConID: reduceTestStockConID}, Position: 100}}, nil
			}
			preview := r.preview(reduceTestStockConID, 50)
			if phase == "snapshot" {
				scope := r.srv.currentBrokerStateScope()
				if err := r.srv.orderJournal.Append(orderJournalEvent{Type: orderJournalEventSendAttempted, At: r.clock(), OrderRef: "synthetic-prior-fill", PreviewTokenID: "synthetic-prior-token", ReservedOrderID: 301, ClientID: 31, Endpoint: "127.0.0.1:4002", Account: scope.Account, Mode: scope.Mode, ConID: reduceTestStockConID, SecType: "STK", Action: "SELL", OrderType: "LMT", Quantity: 1, Filled: 1, Status: "Filled"}); err != nil {
					t.Fatal(err)
				}
			} else {
				r.srv.orderWriteBeforeBrokerSend = func() { generation++ }
			}
			res, err := r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if err == nil || (res != nil && res.Accepted) || len(r.sends()) != 0 {
				t.Fatalf("changed lifecycle frontier: err=%v sends=%d", err, len(r.sends()))
			}
		})
	}
}

func TestReduceFilledComboRequiresExactLegPosition(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestOptionRow(5))
	appendReduceComboReservation(t, r.srv, 3, 1, []rpc.StrategyOrderLeg{{Contract: rpc.ContractParams{ConID: reduceTestOptionConID}, Ratio: -2, Action: rpc.OrderActionSell}})
	r.srv.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{Account: r.srv.currentBrokerStateScope().Account, Contract: ibkrlib.Contract{ConID: reduceTestOptionConID}, Position: 1}}, nil
	}
	res, err := r.srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestOptionConID, Percent: 100})
	if err == nil && res.Accepted {
		t.Fatalf("combo fill against old held quantity accepted %d more contracts", res.ReduceQuantity)
	}
}
