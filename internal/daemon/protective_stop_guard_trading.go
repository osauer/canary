//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// protectiveStopGuardWrite performs one settled guard step through the
// ordinary cancel or modify path with the daemon-protective-guard origin.
// Under brokerWriteMu it reads the evidence again and acts only when the same
// step is still planned, so a sale or fill between the pass and the lock
// never turns into a stale write.
func (s *Server) protectiveStopGuardWrite(ctx context.Context, step protectiveStopGuardStep) error {
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	steps, paused := s.planProtectiveStopGuardPass(ctx, s.currentBrokerStateScope())
	if paused != "" {
		return errors.New(paused)
	}
	still := false
	for _, current := range steps {
		if current.key() == step.key() {
			still = true
			break
		}
	}
	if !still {
		return errors.New("the step no longer applies to current evidence")
	}
	view := step.View
	s.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: step.Kind, OrderRef: view.OrderRef, ReservedOrderID: view.ReservedOrderID, Quantity: step.Target})
	defer s.protectiveStopGuardGrant.Store(nil)
	switch step.Kind {
	case protectiveStopGuardCancel:
		_, err := s.cancelOrderForAction(ctx, rpc.OrderCancelParams{ID: view.OrderRef, Origin: rpc.OrderOriginDaemonProtectiveGuard}, corestore.ActionCancel)
		return err
	case protectiveStopGuardShrink:
		preview, err := s.previewOrder(ctx, protectiveStopGuardShrinkParams(view, step.Target))
		if err != nil {
			return err
		}
		if preview == nil || !preview.SubmitEligible || preview.Draft.Quantity != step.Target {
			return errors.New("the shrink preview is not submit-eligible")
		}
		_, err = s.modifyOrder(ctx, rpc.OrderModifyParams{ID: view.OrderRef, PreviewToken: preview.PreviewToken, Origin: rpc.OrderOriginDaemonProtectiveGuard})
		return err
	default:
		return fmt.Errorf("unknown protective stop guard step %q", step.Kind)
	}
}

// protectiveStopGuardShrinkParams is the replacement preview for a shrink:
// the journaled order with only the quantity lowered. Trail amount or
// percent, the current stop price, limit offset, trigger method, TIF and
// outside-RTH flag are the order's own.
func protectiveStopGuardShrinkParams(view rpc.OrderView, target int) rpc.OrderPreviewParams {
	return rpc.OrderPreviewParams{
		Action: rpc.OrderActionSell,
		Contract: rpc.ContractParams{
			ConID: view.ConID, Symbol: view.Symbol, SecType: strings.ToUpper(strings.TrimSpace(view.SecType)),
			Exchange: view.Exchange, PrimaryExch: view.PrimaryExch, Currency: view.Currency,
			LocalSymbol: view.LocalSymbol, TradingClass: view.TradingClass,
		},
		Quantity:      target,
		OrderType:     view.OrderType,
		Trail:         cloneTrailSpec(view.Trail),
		TriggerMethod: view.TriggerMethod,
		Strategy:      rpc.OrderStrategyBrokerTrail,
		TIF:           view.TIF,
		OutsideRTH:    view.OutsideRTH,
		ReplaceID:     view.OrderRef,
		TimeoutMs:     int(protectiveStopGuardWriteTimeout.Milliseconds()),
		Source:        view.Source,
	}
}
