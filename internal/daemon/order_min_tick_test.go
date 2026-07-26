package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func trailGuardDraft(action string, stop float64) rpc.OrderDraft {
	amount := 3.83
	return rpc.OrderDraft{
		Action:    action,
		Quantity:  1,
		OrderType: rpc.OrderTypeTRAIL,
		Contract:  rpc.ContractParams{Symbol: "MBG", SecType: "STK", ConID: 29622935, Currency: "EUR"},
		Trail: &rpc.OrderTrailSpec{
			OffsetType:       rpc.OrderTrailOffsetAmount,
			TrailingAmount:   &amount,
			InitialStopPrice: stop,
		},
	}
}

func TestTrailRedemptionGuard(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})

	t.Run("sell stop above current bid is refused", func(t *testing.T) {
		srv.orderPreviewQuote = fixedPreviewQuote(44.00, 44.02)
		msg := srv.trailRedemptionGuard(context.Background(), trailGuardDraft(rpc.OrderActionSell, 44.04))
		if !strings.Contains(msg, "trigger immediately") {
			t.Fatalf("guard = %q, want immediate-trigger refusal", msg)
		}
	})

	t.Run("sell stop safely below bid passes", func(t *testing.T) {
		srv.orderPreviewQuote = fixedPreviewQuote(47.75, 47.77)
		if msg := srv.trailRedemptionGuard(context.Background(), trailGuardDraft(rpc.OrderActionSell, 44.04)); msg != "" {
			t.Fatalf("guard = %q, want pass", msg)
		}
	})

	t.Run("buy stop below current ask is refused", func(t *testing.T) {
		srv.orderPreviewQuote = fixedPreviewQuote(47.75, 47.77)
		msg := srv.trailRedemptionGuard(context.Background(), trailGuardDraft(rpc.OrderActionBuy, 47.00))
		if !strings.Contains(msg, "trigger immediately") {
			t.Fatalf("guard = %q, want immediate-trigger refusal", msg)
		}
	})

	t.Run("no live quote stands aside for off-hours placement", func(t *testing.T) {
		srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
			return rpc.OrderQuoteSnapshot{}, errors.New("no live quote")
		}
		if msg := srv.trailRedemptionGuard(context.Background(), trailGuardDraft(rpc.OrderActionSell, 44.04)); msg != "" {
			t.Fatalf("guard = %q, want stand-aside on quote unavailability", msg)
		}
	})

	t.Run("non-trail drafts are untouched", func(t *testing.T) {
		srv.orderPreviewQuote = fixedPreviewQuote(44.00, 44.02)
		draft := trailGuardDraft(rpc.OrderActionSell, 44.04)
		draft.Trail = nil
		if msg := srv.trailRedemptionGuard(context.Background(), draft); msg != "" {
			t.Fatalf("guard = %q, want pass for non-trail draft", msg)
		}
	})
}
