//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// submitOptionExercise consumes one signed preview token, durably stages the
// exact reduce-only instruction, and sends it through the captured broker
// session. A nil return means only that the frame was flushed; exercise has no
// WhatIf or final-status callback, so the operator must verify broker status.
func (s *Server) submitOptionExercise(ctx context.Context, payload orderPreviewTokenPayload, opp rpc.Opportunity, qty int, origin string) error {
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(origin, false)
	if !auth.Allowed {
		return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	if bindingErr != nil {
		return bindingErr
	}
	if payload.Scope != rpc.OrderTokenScopeExercise || payload.TokenID == "" ||
		payload.Draft.OrderRef == "" || payload.Draft.Quantity != qty || payload.Draft.Contract.ConID != opp.Contract.ConID {
		return fmt.Errorf("%w: exercise preview token does not match the requested instruction", ErrTradingDisabled)
	}
	if payload.Mode != auth.Status.Mode || payload.Account != auth.Status.Account ||
		payload.Endpoint != auth.Status.Endpoint || payload.ClientID != auth.Status.ClientID {
		return brokerWriteTransactionDriftError()
	}
	if err := s.bindExercisePositionAuthority(ctx, &binding, auth.Status, payload, opp, qty); err != nil {
		return err
	}
	now := s.orderNow()
	attemptID, err := randomTokenID()
	if err != nil {
		return fmt.Errorf("prepare option exercise attempt: %w", err)
	}
	confirm := previewTokenConfirmedEvent(payload, 0, now, "exercise preview token confirmed for broker transmit")
	attempt := orderJournalEventForDraft(payload.Draft, orderJournalEventSendAttempted, auth.Status, payload.TokenID, 0, now)
	attempt.AttemptID = attemptID
	attempt.ActionKind = corestore.ActionExercise
	attempt.SendState = orderSendStateSendAttempted
	attempt.Origin = normalizedWriteOrigin(origin)
	attempt.Message = "option exercise broker transmit attempted"
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return err
	}
	if err := s.orderJournal.StagePreTransmit(payload.TokenID, payload.AuthorityEpoch, payload.SignerGeneration, 0, corestore.ActionExercise, coreOrderOrigin(origin), []orderJournalEvent{confirm, attempt}); err != nil {
		return fmt.Errorf("%w: stage option exercise: %w", ErrTradingDisabled, err)
	}
	req := ibkrlib.OptionExerciseRequest{
		Contract:         previewIBKRContract(opp.Contract),
		ExerciseAction:   ibkrlib.OptionExerciseActionExercise,
		ExerciseQuantity: qty,
		Account:          auth.Status.Account,
		Override:         0,
	}
	if err := s.submitBoundOptionExercise(ctx, binding, auth.Status, req); err != nil {
		if journalErr := s.appendOrderSendError(payload.Draft, auth.Status, payload.TokenID, 0, attemptID, corestore.ActionExercise, err); journalErr != nil {
			err = errors.Join(err, journalErr)
		}
		return err
	}
	if err := s.appendOrderSendCompleted(payload.Draft, auth.Status, payload.TokenID, 0, attemptID, corestore.ActionExercise); err != nil {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionMayHaveWritten)
	}
	return nil
}

func (s *Server) bindExercisePositionAuthority(ctx context.Context, binding *brokerWriteTransactionBinding, status rpc.TradingStatus, payload orderPreviewTokenPayload, opp rpc.Opportunity, qty int) error {
	if binding == nil || payload.PortfolioGeneration == 0 || strings.TrimSpace(payload.PortfolioAccount) == "" {
		return fmt.Errorf("%w: exercise preview lacks exact portfolio authority", ErrTradingDisabled)
	}
	draft, err := exerciseUnderlyingRiskDraft(opp, qty)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	var current orderPositionAuthority
	if binding.testOnly && s.orderRiskAuthorityForTest == nil && s.orderPreviewPositionImpact == nil {
		current = orderPositionAuthority{
			Impact: payload.Position, Generation: payload.PortfolioGeneration,
			Health:   ibkrlib.PortfolioStreamHealth{Account: payload.PortfolioAccount, ProjectionGeneration: payload.PortfolioGeneration},
			TestOnly: true,
		}
	} else {
		current, err = s.captureBoundOrderPositionAuthority(ctx, binding.connector, binding.session, status, draft.Contract, draft.Action, draft.Quantity)
		if err != nil {
			return err
		}
	}
	if current.Generation != payload.PortfolioGeneration ||
		!strings.EqualFold(strings.TrimSpace(current.Health.Account), strings.TrimSpace(payload.PortfolioAccount)) ||
		!sameOrderPositionImpact(current.Impact, payload.Position) {
		return fmt.Errorf("%w: portfolio position authority changed after exercise preview; preview again", ErrTradingDisabled)
	}
	binding.exerciseBound = true
	binding.exerciseDraft = draft
	binding.riskPosition = current.Impact
	binding.riskPortfolioGeneration = current.Generation
	binding.riskPortfolioAccount = current.Health.Account
	return nil
}

func exerciseUnderlyingRiskDraft(opp rpc.Opportunity, qty int) (rpc.OrderDraft, error) {
	multiplier := opp.Contract.Multiplier
	if qty <= 0 || multiplier <= 0 || qty > int(^uint(0)>>1)/multiplier {
		return rpc.OrderDraft{}, fmt.Errorf("exercise quantity or multiplier is invalid")
	}
	if opp.UnderlyingContract.ConID <= 0 {
		return rpc.OrderDraft{}, fmt.Errorf("exact underlying contract identity is unavailable")
	}
	action := rpc.OrderActionBuy
	if strings.EqualFold(opp.Contract.Right, "P") {
		action = rpc.OrderActionSell
	} else if !strings.EqualFold(opp.Contract.Right, "C") {
		return rpc.OrderDraft{}, fmt.Errorf("option right is invalid")
	}
	return rpc.OrderDraft{Action: action, Contract: opp.UnderlyingContract, Quantity: qty * multiplier, Source: "option_exercise_risk"}, nil
}

func (s *Server) submitBoundOptionExercise(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, req ibkrlib.OptionExerciseRequest) error {
	if ctx == nil {
		return ibkrlib.WithSendDisposition(fmt.Errorf("broker exercise context is nil"), ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if err := ctx.Err(); err != nil {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if s.orderWriteBeforeBrokerSend != nil {
		s.orderWriteBeforeBrokerSend()
	}
	entered := false
	err := s.withBoundBrokerWriteTransaction(binding, func() error {
		entered = true
		if err := ctx.Err(); err != nil {
			return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
		}
		wireGuard, release := s.brokerWireGuard(binding, status, false)
		defer release()
		if s.optionExerciseBroker != nil {
			if err := wireGuard(); err != nil {
				return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
			}
			return s.optionExerciseBroker(ctx, req)
		}
		if binding.connector == nil {
			return ibkrlib.WithSendDisposition(brokerWriteTransactionDriftError(), ibkrlib.SendDispositionDefinitelyUnsent)
		}
		return binding.connector.ExerciseOptionsForSessionGuarded(ctx, binding.session, req, wireGuard)
	})
	if err != nil && !entered {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	return err
}
