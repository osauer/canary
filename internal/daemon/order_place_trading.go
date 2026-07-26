//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const orderWritesAvailable = true

func (s *Server) handleOrderPlace(ctx context.Context, req *rpc.Request) (*rpc.OrderPlaceResult, error) {
	var p rpc.OrderPlaceParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.placeOrder(ctx, p)
}

func (s *Server) handleOrderModify(ctx context.Context, req *rpc.Request) (*rpc.OrderModifyResult, error) {
	var p rpc.OrderModifyParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.modifyOrder(ctx, p)
}

func (s *Server) handleOrderCancel(ctx context.Context, req *rpc.Request) (*rpc.OrderCancelResult, error) {
	var p rpc.OrderCancelParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.cancelOrder(ctx, p)
}

func (s *Server) placeOrder(ctx context.Context, p rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, false)
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	payload, err := s.verifyPreviewTokenForPlace(p.PreviewToken)
	if err != nil {
		return nil, err
	}
	if msg := s.trailRedemptionGuard(ctx, payload.Draft); msg != "" {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, msg)
	}
	if bindingErr != nil {
		return nil, bindingErr
	}
	if err := s.bindPreviewOrderRiskAuthority(ctx, &binding, status, payload, payload.Draft); err != nil {
		return nil, err
	}
	reservedOrderID, err := s.reserveBoundBrokerOrderID(ctx, binding)
	if err != nil {
		return nil, err
	}
	now := s.orderNow()
	attemptID, err := randomTokenID()
	if err != nil {
		return nil, fmt.Errorf("prepare broker place attempt: %w", err)
	}
	confirm := previewTokenConfirmedEvent(payload, reservedOrderID, now, fmt.Sprintf("preview token confirmed for %s broker transmit", auth.Route))
	attempt := orderJournalEventForDraft(payload.Draft, orderJournalEventSendAttempted, status, payload.TokenID, reservedOrderID, now)
	attempt.AttemptID = attemptID
	attempt.ActionKind = corestore.ActionPlace
	attempt.SendState = orderSendStateSendAttempted
	attempt.Origin = normalizedWriteOrigin(p.Origin)
	attempt.Message = fmt.Sprintf("%s broker placeOrder transmit attempted", auth.Route)
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return nil, err
	}
	if err := s.orderJournal.StagePreTransmit(payload.TokenID, payload.AuthorityEpoch, payload.SignerGeneration, reservedOrderID, corestore.ActionPlace, coreOrderOrigin(p.Origin), []orderJournalEvent{confirm, attempt}); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}

	contract := previewIBKRContract(payload.Draft.Contract)
	order := previewIBKROrder(payload.Draft)
	order.OrderID = reservedOrderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitBoundConfiguredOrder(ctx, binding, status, contract, order); err != nil {
		if journalErr := s.appendOrderSendError(payload.Draft, status, payload.TokenID, reservedOrderID, attemptID, corestore.ActionPlace, err); journalErr != nil {
			err = errors.Join(err, journalErr)
		}
		return nil, fmt.Errorf("%s order transmit: %w", auth.Route, err)
	}
	if err := s.appendOrderSendCompleted(payload.Draft, status, payload.TokenID, reservedOrderID, attemptID, corestore.ActionPlace); err != nil {
		return nil, fmt.Errorf("%s order transmit outcome: %w", auth.Route, ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionMayHaveWritten))
	}

	return &rpc.OrderPlaceResult{
		Accepted:        true,
		Mode:            status.Mode,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		ClientID:        status.ClientID,
		OrderRef:        payload.Draft.OrderRef,
		PreviewTokenID:  payload.TokenID,
		ReservedOrderID: reservedOrderID,
		Draft:           payload.Draft,
		LifecycleStatus: rpc.OrderLifecyclePendingSubmit,
		SendState:       orderSendStateSendAttempted,
		Message:         fmt.Sprintf("%s broker placeOrder transmit attempted; waiting for broker lifecycle callback", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) modifyOrder(ctx context.Context, p rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, false)
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	view, err := s.openOrderViewForWrite(p.ID, status)
	if err != nil {
		return nil, err
	}
	if view.ReservedOrderID <= 0 {
		return nil, errBadRequest("order modify requires a broker order ID")
	}
	if view.ConID <= 0 {
		return nil, errBadRequest("order modify requires an exact positive contract identity")
	}
	if !view.ModifyEligible {
		return nil, errBadRequest("order is not modify-eligible")
	}
	payload, err := s.verifyPreviewTokenForModify(p.PreviewToken, view)
	if err != nil {
		return nil, err
	}
	if err := validateModifyDraft(view, payload.Draft); err != nil {
		return nil, err
	}

	now := s.orderNow()
	attemptID, err := randomTokenID()
	if err != nil {
		return nil, fmt.Errorf("prepare broker modify attempt: %w", err)
	}
	modifiedDraft := payload.Draft
	modifiedDraft.OrderRef = view.OrderRef
	modifiedDraft.Contract = modifyContractForView(view, modifiedDraft.Contract)
	modifiedDraft.OutsideRTH = view.OutsideRTH
	if bindingErr != nil {
		return nil, bindingErr
	}
	if err := s.bindPreviewOrderRiskAuthority(ctx, &binding, status, payload, modifiedDraft); err != nil {
		return nil, err
	}
	// A close-protective modify must remain exactly backed by the same current
	// position authority that is carried to the first-byte guard.
	if orderViewIsCloseProtective(view) {
		if err := validateProtectiveModifyQuantity(view, modifiedDraft, binding.riskPosition.Before); err != nil {
			return nil, err
		}
	}
	confirm := previewTokenConfirmedEvent(payload, view.ReservedOrderID, now, fmt.Sprintf("preview token confirmed for %s broker modify", auth.Route))
	confirm.OrderRef = view.OrderRef
	modify := orderJournalEventForDraft(modifiedDraft, orderJournalEventModifyRequested, status, payload.TokenID, view.ReservedOrderID, now)
	modify.AttemptID = attemptID
	modify.ActionKind = corestore.ActionModify
	modify.SendState = orderSendStateSendAttempted
	modify.Origin = normalizedWriteOrigin(p.Origin)
	modify.Message = fmt.Sprintf("%s broker modify attempted", auth.Route)
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return nil, err
	}
	expectedOrderEventSeq, err := s.orderJournal.LatestOrderEventSeq(modify)
	if err != nil {
		return nil, fmt.Errorf("%w: read modify order authority: %v", ErrTradingDisabled, err)
	}
	stagedOrderEventSeq, err := s.orderJournal.StageModifyPreTransmit(payload.TokenID, payload.AuthorityEpoch, payload.SignerGeneration, view.ReservedOrderID, coreOrderOrigin(p.Origin), expectedOrderEventSeq, []orderJournalEvent{confirm, modify})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}
	binding.orderID = view.ReservedOrderID
	binding.orderEventSeq = stagedOrderEventSeq

	contract := previewIBKRContract(modifiedDraft.Contract)
	order := previewIBKROrder(modifiedDraft)
	order.OrderID = view.ReservedOrderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitBoundConfiguredOrder(ctx, binding, status, contract, order); err != nil {
		if journalErr := s.appendOrderSendError(modifiedDraft, status, payload.TokenID, view.ReservedOrderID, attemptID, corestore.ActionModify, err); journalErr != nil {
			err = errors.Join(err, journalErr)
		}
		return nil, fmt.Errorf("%s order modify: %w", auth.Route, err)
	}
	if err := s.appendOrderSendCompleted(modifiedDraft, status, payload.TokenID, view.ReservedOrderID, attemptID, corestore.ActionModify); err != nil {
		return nil, fmt.Errorf("%s order modify outcome: %w", auth.Route, ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionMayHaveWritten))
	}

	return &rpc.OrderModifyResult{
		Accepted:        true,
		Mode:            status.Mode,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		ClientID:        status.ClientID,
		OrderRef:        view.OrderRef,
		PreviewTokenID:  payload.TokenID,
		ReservedOrderID: view.ReservedOrderID,
		Draft:           modifiedDraft,
		LifecycleStatus: rpc.OrderLifecycleSubmitted,
		SendState:       orderSendStateSendAttempted,
		Message:         fmt.Sprintf("%s broker modify attempted; waiting for broker lifecycle callback", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) cancelOrder(ctx context.Context, p rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return s.cancelOrderForAction(ctx, p, corestore.ActionCancel)
}

func (s *Server) cancelOrderForAction(ctx context.Context, p rpc.OrderCancelParams, action corestore.ActionKind) (*rpc.OrderCancelResult, error) {
	if action != corestore.ActionCancel && action != corestore.ActionSmokeCleanup {
		return nil, errBadRequest("order cancel action kind is invalid")
	}
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, true)
	if !auth.Allowed {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
	}
	status := auth.Status
	view, err := s.openOrderViewForWrite(p.ID, status)
	if err != nil {
		return nil, err
	}
	if view.ReservedOrderID <= 0 {
		return nil, errBadRequest("order cancel requires a broker order ID")
	}
	if !view.CancelEligible {
		return nil, errBadRequest("order is not cancel-eligible")
	}
	if bindingErr != nil {
		return nil, bindingErr
	}

	now := s.orderNow()
	attemptID, err := randomTokenID()
	if err != nil {
		return nil, fmt.Errorf("prepare broker cancel attempt: %w", err)
	}
	cancelEvent := orderJournalEventFromView(view, orderJournalEventCancelRequested, now)
	cancelEvent.AttemptID = attemptID
	cancelEvent.ActionKind = action
	// The cancel attempt must not regress the ORDER's transmit state: an
	// empty SendState leaves the merged view at broker_acknowledged, so a
	// missed cancel confirmation keeps the row cancel-retryable instead of
	// wedging it write-ineligible forever (the live PendingCancel/Cancelled
	// broker callbacks still land as usual and take over when they arrive).
	cancelEvent.SendState = ""
	cancelEvent.Origin = normalizedWriteOrigin(p.Origin)
	cancelEvent.Message = fmt.Sprintf("%s broker cancel attempted", auth.Route)
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return nil, err
	}
	if err := s.orderJournal.StagePreTransmit("", "", 0, view.ReservedOrderID, action, coreOrderOrigin(p.Origin), []orderJournalEvent{cancelEvent}); err != nil {
		return nil, fmt.Errorf("%w: append cancel journal: %v", ErrTradingDisabled, err)
	}
	if err := s.cancelBoundConfiguredOrder(ctx, binding, status, view.ReservedOrderID); err != nil {
		if journalErr := s.appendOrderSendError(orderDraftFromView(view), status, view.PreviewTokenID, view.ReservedOrderID, attemptID, action, err); journalErr != nil {
			err = errors.Join(err, journalErr)
		}
		return nil, fmt.Errorf("%s order cancel: %w", auth.Route, err)
	}
	if err := s.appendOrderSendCompleted(orderDraftFromView(view), status, view.PreviewTokenID, view.ReservedOrderID, attemptID, action); err != nil {
		return nil, fmt.Errorf("%s order cancel outcome: %w", auth.Route, ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionMayHaveWritten))
	}
	view.LastEvent = orderJournalEventCancelRequested
	view.LifecycleStatus = rpc.OrderLifecyclePendingCancel
	view.Open = true
	view.ModifyEligible = false
	view.CancelEligible = false
	view.LastMessage = cancelEvent.Message
	view.UpdatedAt = now
	return &rpc.OrderCancelResult{
		Accepted:        true,
		Order:           view,
		Status:          view.Status,
		LifecycleStatus: view.LifecycleStatus,
		SendState:       view.SendState,
		Message:         fmt.Sprintf("%s broker cancel attempted; waiting for broker cancellation status", auth.Route),
		AsOf:            s.orderNow(),
	}, nil
}

func (s *Server) reserveBoundBrokerOrderID(ctx context.Context, binding brokerWriteTransactionBinding) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return 0, err
	}
	if s.orderReserveBrokerID != nil {
		var id int
		err := s.withBoundBrokerWriteTransaction(binding, func() error {
			var err error
			id, err = s.orderReserveBrokerID(ctx)
			return err
		})
		return id, err
	}
	if binding.connector == nil {
		return 0, brokerWriteTransactionDriftError()
	}
	floor, err := s.reservedBrokerOrderIDFloor()
	if err != nil {
		return 0, err
	}
	var id int
	err = s.withBoundBrokerWriteTransaction(binding, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var reserveErr error
		id, reserveErr = binding.connector.ReserveOrderIDForSession(binding.session)
		for reserveErr == nil && id <= floor {
			id, reserveErr = binding.connector.ReserveOrderIDForSession(binding.session)
		}
		return reserveErr
	})
	return id, err
}

func (s *Server) submitBoundConfiguredOrder(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, contract *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
	if ctx == nil {
		return ibkrlib.WithSendDisposition(fmt.Errorf("broker order context is nil"), ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if err := ctx.Err(); err != nil {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if s.orderWriteBeforeBrokerSend != nil {
		s.orderWriteBeforeBrokerSend()
	}
	operationEntered := false
	err := s.withBoundBrokerWriteTransaction(binding, func() error {
		operationEntered = true
		if err := ctx.Err(); err != nil {
			return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
		}
		auth := s.brokerWriteAuthorization(status)
		if !auth.Allowed {
			return ibkrlib.WithSendDisposition(fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers)), ibkrlib.SendDispositionDefinitelyUnsent)
		}
		wireGuard, releaseWireGuard := s.brokerWireGuard(binding, status, false)
		defer releaseWireGuard()
		if s.orderPlaceBroker != nil {
			if err := wireGuard(); err != nil {
				return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
			}
			return s.orderPlaceBroker(ctx, contract, order)
		}
		if binding.connector == nil {
			return ibkrlib.WithSendDisposition(brokerWriteTransactionDriftError(), ibkrlib.SendDispositionDefinitelyUnsent)
		}
		switch auth.Route {
		case config.TradingModePaper:
			return binding.connector.SubmitPaperOrderForSessionGuarded(ctx, binding.session, paperGateFromStatus(status), contract, order, wireGuard)
		case config.TradingModeLive:
			return binding.connector.SubmitOrderForSessionGuarded(ctx, binding.session, contract, order, wireGuard)
		default:
			return ibkrlib.WithSendDisposition(fmt.Errorf("%w: trading mode %q is invalid", ErrTradingDisabled, auth.Route), ibkrlib.SendDispositionDefinitelyUnsent)
		}
	})
	if err != nil && !operationEntered {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	return err
}

func (s *Server) cancelBoundConfiguredOrder(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, orderID int) error {
	if ctx == nil {
		return ibkrlib.WithSendDisposition(fmt.Errorf("broker cancel context is nil"), ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if err := ctx.Err(); err != nil {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	if s.orderWriteBeforeBrokerSend != nil {
		s.orderWriteBeforeBrokerSend()
	}
	operationEntered := false
	err := s.withBoundBrokerWriteTransaction(binding, func() error {
		operationEntered = true
		if err := ctx.Err(); err != nil {
			return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
		}
		auth := s.brokerWriteAuthorization(status).forCancel()
		if !auth.Allowed {
			return ibkrlib.WithSendDisposition(fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers)), ibkrlib.SendDispositionDefinitelyUnsent)
		}
		wireGuard, releaseWireGuard := s.brokerWireGuard(binding, status, true)
		defer releaseWireGuard()
		if s.orderCancelBroker != nil {
			if err := wireGuard(); err != nil {
				return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
			}
			return s.orderCancelBroker(ctx, orderID)
		}
		if binding.connector == nil {
			return ibkrlib.WithSendDisposition(brokerWriteTransactionDriftError(), ibkrlib.SendDispositionDefinitelyUnsent)
		}
		switch auth.Route {
		case config.TradingModePaper:
			return binding.connector.CancelPaperOrderForSessionGuarded(ctx, binding.session, paperGateFromStatus(status), orderID, wireGuard)
		case config.TradingModeLive:
			return binding.connector.CancelOrderForSessionGuarded(ctx, binding.session, orderID, wireGuard)
		default:
			return ibkrlib.WithSendDisposition(fmt.Errorf("%w: trading mode %q is invalid", ErrTradingDisabled, auth.Route), ibkrlib.SendDispositionDefinitelyUnsent)
		}
	})
	if err != nil && !operationEntered {
		return ibkrlib.WithSendDisposition(err, ibkrlib.SendDispositionDefinitelyUnsent)
	}
	return err
}

func (s *Server) verifyPreviewTokenForModify(token string, view rpc.OrderView) (orderPreviewTokenPayload, error) {
	payload, err := s.verifyPreviewTokenForCurrentGate(token)
	if err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if payload.Scope != rpc.OrderTokenScopeModify {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token scope %q cannot be used for modify", ErrTradingDisabled, payload.Scope)
	}
	if err := requireSubmitEligiblePreviewToken(payload); err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if err := validateModifyPreviewTarget(payload.Replace, view); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	return payload, nil
}

func validateModifyPreviewTarget(target orderPreviewReplaceTarget, view rpc.OrderView) error {
	if target.ReservedOrderID <= 0 {
		return fmt.Errorf("modify preview token is not bound to a broker order ID")
	}
	if target.ReservedOrderID != view.ReservedOrderID {
		return fmt.Errorf("modify preview token targets broker order ID %d, current order is %d", target.ReservedOrderID, view.ReservedOrderID)
	}
	if target.ConID <= 0 || view.ConID <= 0 || target.ConID != view.ConID {
		return fmt.Errorf("modify preview token contract identity changed from ConID %d to %d", target.ConID, view.ConID)
	}
	if !modifyTargetContractIdentityMatches(target, view) {
		return fmt.Errorf("modify preview token contract identity changed; preview the replacement again")
	}
	if target.OrderRef != "" && view.OrderRef != "" && target.OrderRef != view.OrderRef {
		return fmt.Errorf("modify preview token targets order ref %s, current order is %s", target.OrderRef, view.OrderRef)
	}
	if target.PermID != 0 && view.PermID != 0 && target.PermID != view.PermID {
		return fmt.Errorf("modify preview token targets permanent ID %d, current order is %d", target.PermID, view.PermID)
	}
	if target.ClientID != 0 && view.ClientID != 0 && target.ClientID != view.ClientID {
		return fmt.Errorf("modify preview token targets client ID %d, current order is %d", target.ClientID, view.ClientID)
	}
	if !strings.EqualFold(target.Account, view.Account) || target.Endpoint != view.Endpoint || !strings.EqualFold(target.Mode, view.Mode) {
		return fmt.Errorf("modify preview token target gate no longer matches current order")
	}
	if target.Status != view.Status || target.LifecycleStatus != view.LifecycleStatus {
		return fmt.Errorf("modify preview token target status changed from %s/%s to %s/%s", target.Status, target.LifecycleStatus, view.Status, view.LifecycleStatus)
	}
	if target.OrderType != "" && view.OrderType != "" && !strings.EqualFold(target.OrderType, view.OrderType) {
		return fmt.Errorf("modify preview token target order type changed from %s to %s", target.OrderType, view.OrderType)
	}
	if target.TIF != "" && view.TIF != "" && !strings.EqualFold(target.TIF, view.TIF) {
		return fmt.Errorf("modify preview token target time-in-force changed from %s to %s", target.TIF, view.TIF)
	}
	if !sameOrderFloat(target.Quantity, view.Quantity) || !sameOrderFloat(target.Filled, view.Filled) || !sameOrderFloat(target.Remaining, view.Remaining) || !sameOrderFloat(target.LimitPrice, view.LimitPrice) {
		return fmt.Errorf("modify preview token target quantities/prices changed; preview the replacement again")
	}
	if !sameOrderTrail(target.Trail, view.Trail) {
		return fmt.Errorf("modify preview token target trail fields changed; preview the replacement again")
	}
	if target.OutsideRTH != view.OutsideRTH {
		return fmt.Errorf("modify preview token target outside_rth changed; preview the replacement again")
	}
	return nil
}

func modifyTargetContractIdentityMatches(target orderPreviewReplaceTarget, view rpc.OrderView) bool {
	equalText := func(a, b string) bool {
		return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
	}
	return equalText(target.Symbol, view.Symbol) && equalText(target.SecType, view.SecType) &&
		equalText(target.Exchange, view.Exchange) && equalText(target.PrimaryExch, view.PrimaryExch) &&
		equalText(target.Currency, view.Currency) && equalText(target.LocalSymbol, view.LocalSymbol) &&
		equalText(target.TradingClass, view.TradingClass) && strings.TrimSpace(target.Expiry) == strings.TrimSpace(view.Expiry) &&
		sameOrderFloat(target.Strike, view.Strike) && equalText(target.Right, view.Right) && target.Multiplier == view.Multiplier
}

func sameOrderTrail(a, b *rpc.OrderTrailSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameOrderFloat(a.InitialStopPrice, b.InitialStopPrice) &&
		sameOrderFloatPtr(a.TrailingPercent, b.TrailingPercent) &&
		sameOrderFloatPtr(a.TrailingAmount, b.TrailingAmount) &&
		sameOrderFloatPtr(a.LimitOffset, b.LimitOffset)
}

func sameOrderFloatPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return sameOrderFloat(*a, *b)
}

func sameOrderFloat(a, b float64) bool {
	const epsilon = 1e-9
	if a > b {
		return a-b < epsilon
	}
	return b-a < epsilon
}

func paperGateFromStatus(status rpc.TradingStatus) ibkrlib.PaperOrderGate {
	return ibkrlib.PaperOrderGate{
		Mode:     status.Mode,
		Account:  status.Account,
		Endpoint: status.Endpoint,
		Host:     status.GatewayHost,
		Port:     status.GatewayPort,
		ClientID: status.ClientID,
	}
}

func orderJournalEventForDraft(draft rpc.OrderDraft, eventType string, status rpc.TradingStatus, tokenID string, orderID int, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            eventType,
		OrderRef:        draft.OrderRef,
		PreviewTokenID:  tokenID,
		ReservedOrderID: orderID,
		ClientID:        status.ClientID,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		Mode:            status.Mode,
		Symbol:          draft.Contract.Symbol,
		SecType:         draft.Contract.SecType,
		ConID:           draft.Contract.ConID,
		Exchange:        draft.Contract.Exchange,
		PrimaryExch:     draft.Contract.PrimaryExch,
		Currency:        draft.Contract.Currency,
		LocalSymbol:     draft.Contract.LocalSymbol,
		TradingClass:    draft.Contract.TradingClass,
		Expiry:          draft.Contract.Expiry,
		Strike:          draft.Contract.Strike,
		Right:           draft.Contract.Right,
		Multiplier:      draft.Contract.Multiplier,
		Action:          draft.Action,
		OrderType:       draft.OrderType,
		TIF:             draft.TIF,
		TriggerMethod:   draft.TriggerMethod,
		OutsideRTH:      draft.OutsideRTH,
		Quantity:        float64(draft.Quantity),
		LimitPrice:      draft.LimitPrice,
		Trail:           cloneTrailSpec(draft.Trail),
		OpenClose:       draft.OpenClose,
		Source:          draft.Source,
	}
}

func (s *Server) appendOrderSendError(draft rpc.OrderDraft, status rpc.TradingStatus, tokenID string, orderID int, attemptID string, action corestore.ActionKind, sendErr error) error {
	if s == nil || s.orderJournal == nil {
		return fmt.Errorf("order journal is unavailable after broker send error")
	}
	ev := orderJournalEventForDraft(draft, orderJournalEventSendError, status, tokenID, orderID, s.orderNow())
	ev.AttemptID = attemptID
	ev.ActionKind = action
	ev.SendDisposition = ibkrlib.SendDispositionOf(sendErr)
	if ev.SendDisposition == ibkrlib.SendDispositionDefinitelyUnsent {
		ev.SendState = ""
	} else {
		ev.SendState = orderSendStateUncertainSend
	}
	ev.Message = fmt.Sprintf("broker send returned error (%s): %v", ev.SendDisposition, sendErr)
	if err := s.orderJournal.Append(ev); err != nil {
		s.warnf("append order send error: %v", err)
		s.markOrderLifecyclePersistenceUncertain()
		return fmt.Errorf("append correlated broker send error: %w", err)
	}
	return nil
}

func (s *Server) appendOrderSendCompleted(draft rpc.OrderDraft, status rpc.TradingStatus, tokenID string, orderID int, attemptID string, action corestore.ActionKind) error {
	if s == nil || s.orderJournal == nil {
		return fmt.Errorf("order journal is unavailable after broker send return")
	}
	ev := orderJournalEventForDraft(draft, orderJournalEventSendCompleted, status, tokenID, orderID, s.orderNow())
	ev.AttemptID = attemptID
	ev.ActionKind = action
	ev.SendState = orderSendStateSendAttempted
	ev.Message = "broker transport returned after frame flush; waiting for broker lifecycle evidence"
	if err := s.orderJournal.Append(ev); err != nil {
		s.warnf("append order send completion: %v", err)
		s.markOrderLifecyclePersistenceUncertain()
		return fmt.Errorf("append correlated broker send completion: %w", err)
	}
	return nil
}

func orderDraftFromView(view rpc.OrderView) rpc.OrderDraft {
	return rpc.OrderDraft{
		Action: view.Action,
		Contract: rpc.ContractParams{
			ConID:        view.ConID,
			Symbol:       view.Symbol,
			SecType:      view.SecType,
			Exchange:     view.Exchange,
			PrimaryExch:  view.PrimaryExch,
			Currency:     view.Currency,
			LocalSymbol:  view.LocalSymbol,
			TradingClass: view.TradingClass,
			Expiry:       view.Expiry,
			Strike:       view.Strike,
			Right:        view.Right,
			Multiplier:   view.Multiplier,
		},
		Quantity:      int(view.Quantity),
		OrderType:     view.OrderType,
		LimitPrice:    view.LimitPrice,
		Trail:         cloneTrailSpec(view.Trail),
		TriggerMethod: view.TriggerMethod,
		TIF:           view.TIF,
		OutsideRTH:    view.OutsideRTH,
		OrderRef:      view.OrderRef,
		OpenClose:     view.OpenClose,
	}
}
