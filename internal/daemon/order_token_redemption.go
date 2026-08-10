package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) confirmPreviewTokenForPlace(token string) (orderPreviewTokenPayload, error) {
	return s.confirmPreviewTokenForPlaceWithOrderID(token, 0, "preview token confirmed for broker place flow")
}

func (s *Server) confirmPreviewTokenForPlaceWithOrderID(token string, reservedOrderID int, message string) (orderPreviewTokenPayload, error) {
	payload, err := s.verifyPreviewTokenForPlace(token)
	if err != nil {
		return orderPreviewTokenPayload{}, err
	}

	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	if err := s.orderJournal.StagePreTransmit(payload.TokenID, payload.AuthorityEpoch, payload.SignerGeneration, reservedOrderID, corestore.ActionPlace, corestore.OriginDaemon, []orderJournalEvent{previewTokenConfirmedEvent(payload, reservedOrderID, now, message)}); err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %w", ErrTradingDisabled, err)
	}
	return payload, nil
}

func (s *Server) verifyPreviewTokenForPlace(token string) (orderPreviewTokenPayload, error) {
	payload, err := s.verifyPreviewTokenForCurrentGate(token)
	if err != nil {
		return orderPreviewTokenPayload{}, err
	}
	if payload.Scope != rpc.OrderTokenScopePlace && payload.Scope != rpc.OrderTokenScopeStrategy {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token scope %q cannot be used for place", ErrTradingDisabled, payload.Scope)
	}
	if err := requireSubmitEligiblePreviewToken(payload); err != nil {
		return orderPreviewTokenPayload{}, err
	}
	return payload, nil
}

func (s *Server) verifyPreviewTokenForCurrentGate(token string) (orderPreviewTokenPayload, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token is required", ErrTradingDisabled)
	}
	if s == nil || s.orderTokens == nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}
	payload, err := s.orderTokens.verify(token)
	if err != nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}

	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if status.Mode == config.TradingModeDisabled {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: set [trading].mode to paper or live before confirming order preview token", ErrTradingDisabled)
	}
	if status.Blocked {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if payload.Mode != status.Mode || payload.Account != status.Account || payload.Endpoint != status.Endpoint || payload.ClientID != status.ClientID {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: preview token was minted for %s/%s client %d account %s, current gate is %s/%s client %d account %s",
			ErrTradingDisabled,
			payload.Mode, payload.Endpoint, payload.ClientID, payload.Account,
			status.Mode, status.Endpoint, status.ClientID, status.Account)
	}
	if s.orderJournal == nil {
		return orderPreviewTokenPayload{}, fmt.Errorf("%w: order journal is unavailable", ErrTradingDisabled)
	}
	return payload, nil
}

func requireSubmitEligiblePreviewToken(payload orderPreviewTokenPayload) error {
	if payload.WhatIf.Status != rpc.OrderWhatIfStatusAccepted {
		return fmt.Errorf("%w: preview token is not backed by an accepted broker WhatIf result", ErrTradingDisabled)
	}
	if payload.WhatIf.RequiredForSubmit {
		return fmt.Errorf("%w: accepted broker WhatIf result still requires a submit gate", ErrTradingDisabled)
	}
	return nil
}

func previewTokenConfirmedEvent(payload orderPreviewTokenPayload, reservedOrderID int, at time.Time, message string) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            orderJournalEventTokenConfirmed,
		OrderRef:        payload.Draft.OrderRef,
		PreviewTokenID:  payload.TokenID,
		ReservedOrderID: reservedOrderID,
		ClientID:        payload.ClientID,
		Account:         payload.Account,
		Endpoint:        payload.Endpoint,
		Mode:            payload.Mode,
		Symbol:          payload.Draft.Contract.Symbol,
		SecType:         payload.Draft.Contract.SecType,
		ConID:           payload.Draft.Contract.ConID,
		Exchange:        payload.Draft.Contract.Exchange,
		PrimaryExch:     payload.Draft.Contract.PrimaryExch,
		Currency:        payload.Draft.Contract.Currency,
		LocalSymbol:     payload.Draft.Contract.LocalSymbol,
		TradingClass:    payload.Draft.Contract.TradingClass,
		Expiry:          payload.Draft.Contract.Expiry,
		Strike:          payload.Draft.Contract.Strike,
		Right:           payload.Draft.Contract.Right,
		Multiplier:      payload.Draft.Contract.Multiplier,
		Action:          payload.Draft.Action,
		OrderType:       payload.Draft.OrderType,
		TIF:             payload.Draft.TIF,
		OutsideRTH:      payload.Draft.OutsideRTH,
		Quantity:        float64(payload.Draft.Quantity),
		LimitPrice:      payload.Draft.LimitPrice,
		Trail:           cloneTrailSpec(payload.Draft.Trail),
		OpenClose:       payload.Draft.OpenClose,
		Source:          payload.Draft.Source,
		Message:         strings.TrimSpace(message),
	}
}
