//go:build trading

package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestPartialModifyKeepsReviewedRemainingQuantity(t *testing.T) {
	r := newCloseLinkRig(t, false, "")
	r.silent = true
	r.position = 50
	r.stopRemaining = 80
	view := r.stopView(t)
	e := orderJournalEventFromView(view, orderJournalEventStatusUpdated, r.now)
	e.Quantity = 100
	e.Filled = 20
	e.Remaining = 80
	e.Status = "Submitted"
	if err := r.srv.orderJournal.Append(e); err != nil {
		t.Fatal(err)
	}
	original := r.srv.openOrderInventoryForTest
	r.srv.openOrderInventoryForTest = func(ctx context.Context, refresh bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		snap, scope, err := original(ctx, refresh)
		for i := range snap.Orders {
			if snap.Orders[i].OrderID == 1002 {
				o := &snap.Orders[i]
				o.TotalQuantity = 100
				o.Filled = 20
				o.Remaining = 80
			}
		}
		return snap, scope, err
	}
	view = r.stopView(t)
	preview, err := r.srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "SELL", Contract: closeLinkContract(false), Quantity: 50, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Strategy: rpc.OrderStrategyBrokerTrail, Trail: cloneTrailSpec(view.Trail), ReplaceID: view.OrderRef})
	if err != nil {
		t.Fatalf("preview failed: %v", err)
	}
	if !preview.SubmitEligible {
		t.Fatal("preview not submit eligible")
	}
	result, err := r.srv.modifyOrder(t.Context(), rpc.OrderModifyParams{ID: view.OrderRef, PreviewToken: preview.PreviewToken})
	if err != nil {
		t.Fatalf("modify failed: %v", err)
	}
	if result == nil || !result.Accepted || len(r.modified) != 1 {
		t.Fatal("modify did not reach broker seam")
	}
	t.Logf("reviewed_quantity=%d position_before=%g position_after=%g filled=%g broker_total=%d broker_remaining=%g", preview.Draft.Quantity, preview.Position.Before, preview.Position.After, view.Filled, r.modified[0].TotalQty, float64(r.modified[0].TotalQty)-view.Filled)
	if result.Draft.Quantity != 50 || preview.Draft.Quantity != 50 || preview.Position.After != 0 {
		t.Fatal("result or preview changed the reviewed remaining intent")
	}
	staged, completed := false, false
	for _, event := range journalEventsForToken(t, r.srv, result.PreviewTokenID) {
		if event.Type == orderJournalEventModifyRequested || event.Type == orderJournalEventSendCompleted {
			if event.Quantity != 70 {
				t.Fatalf("journal %s quantity=%g, want cumulative70", event.Type, event.Quantity)
			}
			staged = staged || event.Type == orderJournalEventModifyRequested
			completed = completed || event.Type == orderJournalEventSendCompleted
		}
	}
	if !staged || !completed {
		t.Fatal("replacement lacks staged or completed journal evidence")
	}
	if r.modified[0].TotalQty != 70 {
		t.Fatalf("reviewed remaining50 but sent cumulative%d with20 already filled; expected70", r.modified[0].TotalQty)
	}
}

// A fill callback can precede the corresponding portfolio callback while
// risk authority is being captured. The signed target must still be current.
func TestPartialModifyRejectsFillDuringRiskBinding(t *testing.T) {
	r := newCloseLinkRig(t, false, "")
	r.silent = true
	view := r.stopView(t)
	preview, err := r.srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "SELL", Contract: closeLinkContract(false), Quantity: 50, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Strategy: rpc.OrderStrategyBrokerTrail, Trail: cloneTrailSpec(view.Trail), ReplaceID: view.OrderRef})
	if err != nil || !preview.SubmitEligible {
		t.Fatalf("preview: %v %+v", err, preview)
	}
	original := r.srv.orderPreviewPositionImpact
	changed := false
	r.srv.orderPreviewPositionImpact = func(ctx context.Context, contract rpc.ContractParams, action string, quantity int) (rpc.OrderPositionImpact, error) {
		if !changed {
			changed = true
			event := orderJournalEventFromView(view, orderJournalEventStatusUpdated, r.now.Add(time.Millisecond))
			event.Filled = 1
			event.Remaining = view.Quantity - 1
			if err := r.srv.orderJournal.Append(event); err != nil {
				return rpc.OrderPositionImpact{}, err
			}
		}
		return original(ctx, contract, action, quantity)
	}
	result, err := r.srv.modifyOrder(t.Context(), rpc.OrderModifyParams{ID: view.OrderRef, PreviewToken: preview.PreviewToken})
	if !changed {
		t.Fatal("fixture did not deliver fill during risk binding")
	}
	if _, ok := errors.AsType[*corestore.RevisionConflictError](err); !ok {
		t.Fatalf("fill did not invalidate captured order sequence: %v", err)
	}
	if err == nil || len(r.modified) != 0 {
		t.Fatalf("stale target accepted after fill: result=%+v err=%v sends=%d", result, err, len(r.modified))
	}
}
