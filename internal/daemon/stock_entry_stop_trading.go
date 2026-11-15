//go:build trading

package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func (s *Server) placeProtectedStockAdd(ctx context.Context, p rpc.OrderPlaceParams, payload orderPreviewTokenPayload, binding brokerWriteTransactionBinding, status rpc.TradingStatus) (*rpc.OrderPlaceResult, error) {
	draft := payload.Draft
	if err := validateProtectedAddScope(payload.Scope, draft); err != nil {
		return nil, err
	}
	if s.protectedAddMandateConflict(draft.Contract) {
		return nil, errBadRequest("full-position protection now overlaps the attached stop")
	}
	parentID, err := s.reserveBoundBrokerOrderID(ctx, binding)
	if err != nil {
		return nil, err
	}
	childID, err := s.reserveBoundBrokerOrderID(ctx, binding)
	if err != nil {
		return nil, err
	}
	child := rpc.OrderDraft{Contract: draft.Contract, Action: "SELL", Quantity: draft.Quantity, OrderType: "STP", FixedStopPrice: draft.AttachedStop.Price, TIF: "GTC", TriggerMethod: 2, OrderRef: draft.OrderRef + "-stop", OpenClose: "C", Source: draft.Source, EntryLink: &rpc.StockEntryLink{PairRef: draft.OrderRef, ParentRef: draft.OrderRef, ParentID: parentID, EntryQuantity: draft.Quantity}}
	now := s.orderNow()
	attemptID, err := randomTokenID()
	if err != nil {
		return nil, err
	}
	confirm := previewTokenConfirmedEvent(payload, parentID, now, "entry and attached stop confirmed as one pair")
	confirm.AddWarningsAccepted = p.AcceptAddWarnings && len(draft.Add.Plan.Warnings) > 0
	legs := []rpc.OrderDraft{draft, child}
	ids := []int{parentID, childID}
	attempts := []string{attemptID + "-entry", attemptID + "-stop"}
	events := []orderJournalEvent{confirm}
	for i, d := range legs {
		ev := orderJournalEventForDraft(d, orderJournalEventSendAttempted, status, payload.TokenID, ids[i], now)
		ev.AttemptID = attempts[i]
		ev.ActionKind = corestore.ActionPlace
		ev.SendState = orderSendStateSendAttempted
		ev.Origin = normalizedWriteOrigin(p.Origin)
		ev.Message = "paired stock entry/stop transmit attempted"
		if i == 0 {
			attachOrderFeeBound(&ev, payload.WhatIf)
			ev.Quote = fillQuoteAtEvent(payload.Quote, now)
		}
		events = append(events, ev)
	}
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		return nil, err
	}
	// One consumed token and both identities become durable before either frame.
	if err := s.orderJournal.StagePreTransmit(payload.TokenID, payload.AuthorityEpoch, payload.SignerGeneration, childID, corestore.ActionPlace, coreOrderOrigin(p.Origin), events); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}
	order := previewIBKROrderForStatus(draft, status)
	order.OrderID = parentID
	sendErr := s.submitBoundStockEntryStop(ctx, binding, status, previewIBKRContract(draft.Contract), order, childID, child.OrderRef, child.FixedStopPrice)
	for i, d := range legs {
		var journalErr error
		if sendErr != nil {
			journalErr = s.appendOrderSendError(d, status, payload.TokenID, ids[i], attempts[i], corestore.ActionPlace, sendErr)
		} else {
			journalErr = s.appendOrderSendCompleted(d, status, payload.TokenID, ids[i], attempts[i], corestore.ActionPlace)
		}
		err = errors.Join(err, journalErr)
	}
	if sendErr != nil {
		return nil, errors.Join(sendErr, err)
	}
	if err != nil {
		return nil, ibkr.WithSendDisposition(err, ibkr.SendDispositionMayHaveWritten)
	}
	return &rpc.OrderPlaceResult{Accepted: true, Mode: status.Mode, Account: status.Account, Endpoint: status.Endpoint, ClientID: status.ClientID, OrderRef: draft.OrderRef, PreviewTokenID: payload.TokenID, ReservedOrderID: parentID, AttachedStopOrderID: childID, AttachedStopOrderRef: child.OrderRef, Draft: draft, LifecycleStatus: rpc.OrderLifecyclePendingSubmit, SendState: orderSendStateSendAttempted, Message: "Entry and attached stop sent; awaiting separate broker acknowledgements. Stop remains held until the full entry fills.", AsOf: s.orderNow()}, nil
}

func (s *Server) submitBoundStockEntryStop(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, c *ibkr.Contract, o *ibkr.RawOrder, childID int, childRef string, stop float64) error {
	unsent := func(err error) error { return ibkr.WithSendDisposition(err, ibkr.SendDispositionDefinitelyUnsent) }
	if ctx == nil {
		return unsent(fmt.Errorf("missing order context"))
	}
	if err := ctx.Err(); err != nil {
		return unsent(err)
	}
	if s.orderWriteBeforeBrokerSend != nil {
		s.orderWriteBeforeBrokerSend()
	}
	entered := false
	err := s.withBoundBrokerWriteTransaction(binding, func() error {
		entered = true
		auth := s.brokerWriteAuthorization(status)
		if !auth.Allowed {
			return unsent(tradingBlockersError(auth.Blockers))
		}
		if binding.connector == nil {
			return unsent(brokerWriteTransactionDriftError())
		}
		guard, release := s.brokerWireGuard(binding, status, false)
		defer release()
		pairGuard := func() error {
			if s.protectedAddMandateConflict(binding.riskDraft.Contract) {
				return fmt.Errorf("full-position mandate overlaps attached stop")
			}
			return guard()
		}
		switch auth.Route {
		case config.TradingModePaper:
			return binding.connector.SubmitPaperStockEntryStopForSessionGuarded(ctx, binding.session, paperGateFromStatus(status), c, o, childID, childRef, stop, pairGuard)
		case config.TradingModeLive:
			return binding.connector.SubmitStockEntryStopForSessionGuarded(ctx, binding.session, c, o, childID, childRef, stop, pairGuard)
		default:
			return unsent(fmt.Errorf("invalid trading route"))
		}
	})
	if err != nil && !entered {
		return unsent(err)
	}
	return err
}
