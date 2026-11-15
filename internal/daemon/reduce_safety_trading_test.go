//go:build trading

package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// Two reductions of one line must never together sell more than is held
// (review 2026-10-22, finding 2; owner instruction 2026-10-22 17:45 CEST).
// A reduction sizes from what Canary's own working sales of the line leave
// unsold, and its send refuses when another of Canary's sales went out in
// between.
func TestReductionsNeverStackBeyondTheHeldLine(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	first := r.preview(reduceTestStockConID, 50)
	if res := r.submit(first.PreviewToken, "req-first"); !res.Accepted {
		t.Fatalf("first reduction = %+v, want it sent", res)
	}
	// 50 are working, unfilled. Close 100% now means the 50 not already
	// being sold.
	closing := r.preview(reduceTestStockConID, 100)
	if closing.ReduceQuantity != 50 || closing.Terms.Quantity != 50 || closing.CommittedQuantity != 50 || closing.PositionQuantity != 100 {
		t.Fatalf("Close 100%% beside a working sale of 50 sized %d, want the 50 left unsold", closing.ReduceQuantity)
	}
	// Two reviews prepared before either went out: the second send would
	// sell beyond the line, so it refuses and sends nothing.
	again := r.preview(reduceTestStockConID, 100)
	if res := r.submit(closing.PreviewToken, "req-close"); !res.Accepted {
		t.Fatalf("closing reduction = %+v, want it sent", res)
	}
	sent := len(r.sends())
	if res := r.submit(again.PreviewToken, "req-again"); res.Accepted || firstBlockerCode(res.Blockers) != "reduce_already_working" || len(r.sends()) != sent {
		t.Fatalf("a third reduction beside 100 working = %+v, want reduce_already_working and nothing sent", res)
	}
	// With the whole line already being sold, a new review says so.
	res, err := r.srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestStockConID, Percent: 25})
	if err != nil || res.Accepted || firstBlockerCode(res.Blockers) != "reduce_already_working" {
		t.Fatalf("a review with the line fully working = %+v %v, want reduce_already_working", res, err)
	}
}

// A send that may have reached the broker is never read as a refusal
// (finding 4): the leg says its outcome is unclear.
func TestReduceSendThatMayHaveReachedTheBrokerIsUnclear(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	r.srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		return ibkrlib.WithSendDisposition(errors.New("socket closed after write"), ibkrlib.SendDispositionMayHaveWritten)
	}
	preview := r.preview(reduceTestStockConID, 50)
	res := r.submit(preview.PreviewToken, "req-unclear")
	if res.Accepted || firstBlockerCode(res.Blockers) != "submit_unclear" {
		t.Fatalf("a transmit that may have written = %+v, want submit_unclear", res)
	}
	r.srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		return ibkrlib.WithSendDisposition(errors.New("not connected"), ibkrlib.SendDispositionDefinitelyUnsent)
	}
	preview = r.preview(reduceTestStockConID, 25)
	if res := r.submit(preview.PreviewToken, "req-failed"); res.Accepted || firstBlockerCode(res.Blockers) != "submit_failed" {
		t.Fatalf("a transmit that wrote nothing = %+v, want submit_failed", res)
	}
}
