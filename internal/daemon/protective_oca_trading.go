//go:build trading

package daemon

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// protectiveOCALink is the one-cancels-all membership a sale is sent with.
type protectiveOCALink struct {
	Group string
	Type  int
}

// protectiveOCABeforePlace runs between the admission of a sale and the
// reservation of its broker order ID, under brokerWriteMu: it reads
// Canary's own stops on the contract, links the sale to the one stop whose
// group the broker echoes, or shrinks or cancels the stops that cannot be
// joined and waits for the broker's answer. Any failure refuses the sale
// with the reason; the stops are then as the broker last reported them.
func (s *Server) protectiveOCABeforePlace(ctx context.Context, status rpc.TradingStatus, held float64, draft rpc.OrderDraft) (protectiveOCALink, error) {
	if !protectiveOCACandidate(draft) {
		return protectiveOCALink{}, nil
	}
	views, _, err := s.loadOrderViews()
	if err != nil {
		return protectiveOCALink{}, fmt.Errorf("%w: the order journal cannot be read, so Canary cannot rule out its own stop on this line: %v", ErrTradingDisabled, err)
	}
	scope := recordScope(status.Broker, status.Account, status.Mode)
	var snapshot *ibkrlib.OpenOrderSnapshot
	readCtx, cancel := context.WithTimeout(ctx, ordersOpenInventoryWait)
	if snap, inventoryScope, err := s.brokerOpenOrderInventory(readCtx, false); err == nil && sameBrokerScope(inventoryScope, scope) {
		snapshot = &snap
	}
	cancel()
	plan := planProtectiveOCA(draft, held, canaryWorkingSellStops(views, snapshot, scope, draft))
	if plan.Refusal != "" {
		s.recordProtectiveOCADecision("refuse", decisionRefused, status, draft.OrderRef, plan.Refusal)
		return protectiveOCALink{}, fmt.Errorf("%w: %s", ErrTradingDisabled, plan.Refusal)
	}
	for _, step := range plan.Steps {
		if err := s.protectiveOCAStep(ctx, status, step, draft); err != nil {
			reason := fmt.Sprintf("Canary's own stop %s %v; the sale %s was not sent", step.View.OrderRef, err, draft.OrderRef)
			s.warnf("protective OCA: %s", reason)
			s.recordProtectiveOCADecision("refuse", decisionRefused, status, draft.OrderRef, reason)
			return protectiveOCALink{}, fmt.Errorf("%w: %s", ErrTradingDisabled, reason)
		}
	}
	link := protectiveOCALink{Group: plan.Group, Type: plan.Type}
	if link.Group != "" {
		reason := fmt.Sprintf("%s is born in its own one-cancels-all group %s", draft.OrderRef, link.Group)
		if len(plan.Stops) == 1 && plan.Stops[0].Group == link.Group {
			reason = fmt.Sprintf("the sale %s joins the one-cancels-all group %s of Canary's own stop %s: the first fill reduces the other at the broker", draft.OrderRef, link.Group, plan.Stops[0].View.OrderRef)
		}
		s.infof("protective OCA: %s", reason)
		s.recordProtectiveOCADecision("link", decisionSent, status, draft.OrderRef, reason)
	}
	return link, nil
}

// protectiveOCAStep shrinks or cancels one of Canary's own stops for the
// sale draft through the guard's gated path, then waits for the broker to
// confirm it. The error names what happened to the stop in plain words.
func (s *Server) protectiveOCAStep(ctx context.Context, status rpc.TradingStatus, step protectiveStopGuardStep, draft rpc.OrderDraft) error {
	view := step.View
	s.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: step.Kind, OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID,
		Quantity: step.Target, Trail: cloneTrailSpec(view.Trail), Filled: view.Filled})
	defer s.protectiveStopGuardGrant.Store(nil)
	var err error
	change := "a cancel"
	switch step.Kind {
	case protectiveStopGuardCancel:
		_, err = s.cancelOrderForAction(ctx, rpc.OrderCancelParams{ID: view.OrderRef, Origin: rpc.OrderOriginDaemonProtectiveGuard}, corestore.ActionCancel)
	case protectiveStopGuardShrink:
		change = fmt.Sprintf("a shrink to %d", step.Target)
		var preview *rpc.OrderPreviewResult
		preview, err = s.previewOrder(ctx, protectiveStopGuardShrinkParams(view, step.Target))
		if err == nil && (preview == nil || !preview.SubmitEligible || preview.Draft.Quantity != step.Target) {
			err = errors.New("the shrink preview is not submit-eligible")
		}
		if err == nil {
			_, err = s.modifyOrder(ctx, rpc.OrderModifyParams{ID: view.OrderRef, PreviewToken: preview.PreviewToken, Origin: rpc.OrderOriginDaemonProtectiveGuard})
		}
	default:
		return fmt.Errorf("has an unknown step %q", step.Kind)
	}
	if err != nil {
		s.recordProtectiveOCADecision(step.Kind, decisionFailed, status, view.OrderRef, step.Reason+"; failed: "+err.Error())
		if ibkrlib.SendDispositionOf(err) == ibkrlib.SendDispositionMayHaveWritten {
			return fmt.Errorf("was sent %s that may or may not have reached the broker (%v); wait for the broker's answer and preview again", change, err)
		}
		if step.Kind == protectiveStopGuardCancel {
			return fmt.Errorf("could not be cancelled: %v", err)
		}
		return fmt.Errorf("could not be shrunk to %d: %v", step.Target, err)
	}
	s.recordProtectiveOCADecision(step.Kind, decisionSent, status, view.OrderRef, step.Reason)
	wait := cmp.Or(s.protectiveOCAConfirmWaitForTest, protectiveOCAConfirmWait)
	if err := s.awaitProtectiveOCAStep(ctx, step, wait); err != nil {
		return err
	}
	s.infof("protective OCA: %s (order %s, for the sale %s)", step.Reason, view.OrderRef, draft.OrderRef)
	return nil
}

// awaitProtectiveOCAStep waits, up to wait, for the journal to show the
// broker's answer to the step: a cancelled or shrunk stop confirms; a stop
// that filled meanwhile, or no answer in time, refuses the sale.
func (s *Server) awaitProtectiveOCAStep(ctx context.Context, step protectiveStopGuardStep, wait time.Duration) error {
	change := "a cancel"
	if step.Kind == protectiveStopGuardShrink {
		change = fmt.Sprintf("a shrink to %d", step.Target)
	}
	deadline := time.Now().Add(wait)
	for {
		views, _, err := s.loadOrderViews()
		if err == nil {
			for _, view := range views {
				if view.OrderRef != step.View.OrderRef || view.ReservedOrderID != step.View.ReservedOrderID {
					continue
				}
				switch {
				case view.LifecycleStatus == rpc.OrderLifecycleFilled || (view.Quantity > 0 && view.Filled >= view.Quantity-1e-9):
					return errors.New("filled while the sale was being admitted; preview again from the position as it is now")
				case !view.Open:
					// Cancelled, or gone some other way: nothing competes.
					return nil
				case step.Kind == protectiveStopGuardShrink && view.SendState == orderSendStateBrokerAcknowledged &&
					orderViewRemainingQuantity(view) <= float64(step.Target)+1e-9:
					return nil
				}
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("was sent %s the broker has not confirmed within %s; wait for its answer and preview again", change, wait)
		}
		timer := time.NewTimer(protectiveOCAConfirmPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("was sent %s, but the request ended before the broker confirmed it (%v); preview again", change, ctx.Err())
		case <-timer.C:
		}
	}
}

// recordProtectiveOCADecision appends one linking decision to the decision
// log. kind is link, shrink, cancel or refuse; orderRef is the sale's for a
// link or a refusal and the stop's for a shrink or cancel.
func (s *Server) recordProtectiveOCADecision(kind, outcome string, status rpc.TradingStatus, orderRef, reason string) {
	if s == nil || s.decisions == nil {
		return
	}
	ev := decisionEvent{TS: s.orderNow(), Svc: "canary", Event: "protective_oca." + kind, Outcome: outcome,
		Mode: status.Mode, IDs: decisionIDs{OrderRef: orderRef}, Reason: boundedDecisionReason(reason)}
	if err := s.decisions.append(ev); err != nil {
		s.warnf("protective OCA: append %s: %v", decisionLogFile, err)
	}
}
