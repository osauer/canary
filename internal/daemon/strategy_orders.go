package daemon

import (
	"context"
	"fmt"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleStrategyPreview(ctx context.Context, req *rpc.Request) (*rpc.OrderPreviewResult, error) {
	var p rpc.StrategyPreviewParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.previewStrategyOrder(ctx, p)
}

func (s *Server) previewStrategyOrder(ctx context.Context, p rpc.StrategyPreviewParams) (*rpc.OrderPreviewResult, error) {
	if s == nil {
		return nil, ErrTradingDisabled
	}
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	if status.Mode == config.TradingModeDisabled {
		return nil, fmt.Errorf("%w: set [trading].mode to paper or live before strategy preview", ErrTradingDisabled)
	}
	if status.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(status.Blockers))
	}
	if s.orderTokens == nil {
		return nil, fmt.Errorf("%w: order preview token signer is unavailable", ErrTradingDisabled)
	}

	positions, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return nil, err
	}
	group, err := currentStrategyForPreview(positions.Strategies, p.StrategyID, p.ExpectedRevision)
	if err != nil {
		return nil, err
	}
	operation, units, err := normalizeStrategyOperation(p.Operation, p.Units, group.Units)
	if err != nil {
		return nil, err
	}
	tif := strings.ToUpper(strings.TrimSpace(p.TIF))
	if tif == "" {
		tif = rpc.OrderTIFDay
	}
	if tif != rpc.OrderTIFDay {
		return nil, errBadRequest("strategy close/reduce supports DAY time-in-force only")
	}

	timeout := orderPreviewTimeout(p.TimeoutMs)
	previewAuthority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return nil, err
	}
	if previewAuthority == nil {
		return nil, fmt.Errorf("%w: strategy preview requires one current broker session", ErrTradingDisabled)
	}

	draftGroup := &rpc.StrategyOrderDraft{
		StrategyID: group.ID, StrategyRevision: group.Revision,
		PositionFingerprint: group.PositionFingerprint,
		Operation:           operation, Units: units, UnitsBefore: group.Units, UnitsAfter: group.Units - units,
		GuaranteedCombo: true,
		Legs:            make([]rpc.StrategyOrderLeg, 0, len(group.Legs)),
	}
	currency := ""
	var aggregateBid, aggregateAsk, grossNotional float64
	var quoteAt time.Time
	dataType := ""
	for _, held := range group.Legs {
		resolved, timeZone, err := s.resolveStrategyLeg(ctx, previewAuthority, held.Contract, min(timeout, previewMinTickTimeout))
		if err != nil {
			return nil, err
		}
		if !guaranteedUSOptionComboLeg(resolved, timeZone) {
			return nil, errBadRequest("this strategy has no broker-guaranteed combo route; Canary can describe it but will not submit it")
		}
		if currency == "" {
			currency = strings.ToUpper(strings.TrimSpace(resolved.Currency))
		} else if !strings.EqualFold(currency, resolved.Currency) {
			return nil, errBadRequest("strategy legs do not share one currency")
		}
		action := rpc.OrderActionSell
		if held.Ratio < 0 {
			action = rpc.OrderActionBuy
		}
		quantity := absOrderRatio(held.Ratio) * units
		after := held.Quantity
		if action == rpc.OrderActionSell {
			after -= float64(quantity)
		} else {
			after += float64(quantity)
		}
		draftGroup.Legs = append(draftGroup.Legs, rpc.StrategyOrderLeg{
			Contract: resolved, Ratio: held.Ratio, Action: action, Quantity: quantity,
			Before: held.Quantity, After: after,
		})

		quote, err := s.fetchPreviewQuoteBound(ctx, resolved, timeout, previewAuthority)
		if err != nil {
			return nil, err
		}
		if quote.Bid == nil || quote.Ask == nil || !positiveFinite(*quote.Bid) || !positiveFinite(*quote.Ask) || *quote.Ask < *quote.Bid {
			return nil, fmt.Errorf("%w: complete bid/ask evidence is unavailable for strategy leg %d", ErrTradingDisabled, resolved.ConID)
		}
		ratio := float64(absOrderRatio(held.Ratio))
		if held.Ratio > 0 {
			aggregateBid += ratio * *quote.Bid
			aggregateAsk += ratio * *quote.Ask
		} else {
			aggregateBid -= ratio * *quote.Ask
			aggregateAsk -= ratio * *quote.Bid
		}
		mid := (*quote.Bid + *quote.Ask) / 2
		grossNotional += ratio * float64(units) * mid * float64(contractMultiplier(resolved))
		if quote.AsOf.After(quoteAt) {
			quoteAt = quote.AsOf
		}
		if dataType == "" {
			dataType = quote.DataType
		} else if dataType != quote.DataType {
			dataType = "mixed"
		}
	}
	if len(draftGroup.Legs) < 2 || currency == "" || !positiveFinite(grossNotional) {
		return nil, errBadRequest("strategy has incomplete executable leg evidence")
	}

	limit := roundPrice((aggregateBid + aggregateAsk) / 2)
	if p.LimitPrice != nil {
		if math.IsNaN(*p.LimitPrice) || math.IsInf(*p.LimitPrice, 0) {
			return nil, errBadRequest("strategy limit price must be finite")
		}
		limit = roundPrice(*p.LimitPrice)
	}
	midpoint := roundPrice((aggregateBid + aggregateAsk) / 2)
	quote := rpc.OrderQuoteSnapshot{
		Symbol: group.Underlying, Bid: new(roundPrice(aggregateBid)), Ask: new(roundPrice(aggregateAsk)),
		Midpoint: &midpoint, DataType: dataType, QuoteQuality: "strategy_legs", AsOf: quoteAt,
	}

	now := s.orderNow()
	draft := rpc.OrderDraft{
		Action:   rpc.OrderActionSell,
		Contract: rpc.ContractParams{Symbol: group.Underlying, SecType: "BAG", Exchange: "SMART", Currency: currency},
		Quantity: units, OrderType: rpc.OrderTypeLMT, LimitPrice: limit, TIF: tif,
		Strategy: "group-close", OrderRef: previewOrderRef(now), OpenClose: "C",
		Source: strings.TrimSpace(p.Source), StrategyGroup: draftGroup,
	}
	positionAuthority, err := s.captureBoundStrategyPositionAuthority(ctx, previewAuthority.connector, previewAuthority.session, status, *draftGroup)
	if err != nil {
		return nil, err
	}
	position := positionAuthority.Impact
	cfg, tradingControlGeneration := s.effectiveTradingControlSnapshot()
	notionalAuthority, err := s.captureOrderNotionalAuthority(ctx, previewAuthority, grossNotional, currency, positionAuthority.BaseCurrency, timeout)
	if err != nil {
		return nil, err
	}
	if err := validateOrderRiskAuthority(cfg, draft, position, notionalAuthority, positionAuthority.BaseCurrency); err != nil {
		return nil, errBadRequest(err.Error())
	}
	whatIf, err := s.fetchPreviewWhatIfBound(ctx, status, draft, timeout, previewAuthority)
	if err != nil {
		return nil, err
	}
	if !s.orderPreviewBrokerAuthorityCurrent(previewAuthority) {
		return nil, brokerWriteTransactionDriftError()
	}
	if _, generation := s.effectiveTradingControlSnapshot(); generation != tradingControlGeneration {
		return nil, fmt.Errorf("%w: trading controls changed during preview; refresh and retry", ErrTradingDisabled)
	}
	token, tokenID, expiresAt, err := s.orderTokens.mint(orderPreviewTokenPayload{
		Scope: rpc.OrderTokenScopeStrategy, Mode: status.Mode, Account: status.Account, Endpoint: status.Endpoint, ClientID: status.ClientID,
		Draft: draft, Quote: quote, Position: position,
		PortfolioGeneration: positionAuthority.Generation, PortfolioAccount: positionAuthority.Health.Account,
		PortfolioEvidenceAt: positionAuthority.EvidenceAt, BaseCurrency: positionAuthority.BaseCurrency,
		BaseCurrencyProvenance:   positionAuthority.BaseCurrencyProvenance,
		TradingControlGeneration: tradingControlGeneration, Notional: grossNotional, NotionalAuthority: notionalAuthority,
		WhatIf: whatIf, WhatIfStatus: whatIf.Status,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTradingDisabled, err)
	}
	event := strategyPreviewJournalEvent(draft, status, tokenID, now, whatIf)
	if err := s.orderJournal.Append(event); err != nil {
		return nil, fmt.Errorf("%w: append preview journal: %v", ErrTradingDisabled, err)
	}
	warnings := previewWhatIfWarnings(whatIf)
	tokenMinted := token != "" && tokenID != ""
	submitEligible := tokenMinted && draftGroup.GuaranteedCombo && whatIf.Status == rpc.OrderWhatIfStatusAccepted && !whatIf.RequiredForSubmit
	return &rpc.OrderPreviewResult{
		PreviewToken: token, PreviewTokenID: tokenID, PreviewTokenScope: rpc.OrderTokenScopeStrategy,
		PreviewTokenExpiresAt: expiresAt, TokenMinted: tokenMinted, SubmitEligible: submitEligible, Executable: submitEligible,
		Mode: status.Mode, Account: status.Account, Endpoint: status.Endpoint, ClientID: status.ClientID,
		Draft: draft, Quote: quote, Position: position, Notional: grossNotional,
		NotionalCurrency: notionalAuthority.ContractCurrency, NotionalBase: notionalAuthority.BaseNotional,
		BaseCurrency: notionalAuthority.BaseCurrency, FXRate: notionalAuthority.BasePerContract,
		FXEvidenceAt: notionalAuthority.EvidenceAt, FXDataType: notionalAuthority.DataType, FXSource: notionalAuthority.Source,
		MaxNotional: cfg.MaxNotional, WhatIf: whatIf, Warnings: warnings, AsOf: now,
	}, nil
}

func currentStrategyForPreview(groups []rpc.PositionStrategy, id string, expectedRevision int64) (rpc.PositionStrategy, error) {
	id = strings.TrimSpace(id)
	if id == "" || expectedRevision <= 0 {
		return rpc.PositionStrategy{}, errBadRequest("strategy id and expected revision are required")
	}
	for _, group := range groups {
		if group.ID != id {
			continue
		}
		if group.Revision != expectedRevision {
			return rpc.PositionStrategy{}, errBadRequest("strategy changed; list current strategies and preview again")
		}
		if group.Status != rpc.PositionStrategyStatusCurrent || !group.Actionable {
			return rpc.PositionStrategy{}, errBadRequest("strategy is not actionable")
		}
		return group, nil
	}
	return rpc.PositionStrategy{}, errBadRequest("strategy is not present in current positions")
}

func normalizeStrategyOperation(operation string, units, available int) (string, int, error) {
	operation = strings.ToLower(strings.TrimSpace(operation))
	switch operation {
	case rpc.StrategyOperationClose:
		if units != 0 && units != available {
			return "", 0, errBadRequest("close applies to every remaining strategy unit")
		}
		return operation, available, nil
	case rpc.StrategyOperationReduce:
		if units <= 0 || units >= available {
			return "", 0, errBadRequest("reduce units must be positive and leave at least one strategy unit")
		}
		return operation, units, nil
	default:
		return "", 0, errBadRequest("operation must be close or reduce")
	}
}

func (s *Server) resolveStrategyLeg(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.ContractParams, string, error) {
	// The parent is deliberately a SMART guaranteed combo. A portfolio row may
	// name the venue that last valued the leg; that is identity evidence, not
	// permission to turn the group operation into a direct-routed leg.
	contract.Exchange = "SMART"
	resolved, err := authority.connector.ResolveOrderContractForSession(ctx, authority.session, *previewIBKRContract(contract), timeout)
	if err != nil {
		return rpc.ContractParams{}, "", fmt.Errorf("%w: exact strategy leg resolution failed: %v", ErrTradingDisabled, err)
	}
	out := contract
	out.ConID, out.Symbol, out.SecType = resolved.Contract.ConID, resolved.Contract.Symbol, resolved.Contract.SecType
	out.Expiry, out.Strike, out.Right = resolved.Contract.Expiry, resolved.Contract.Strike, resolved.Contract.Right
	out.Multiplier, out.Exchange, out.PrimaryExch = resolved.Contract.Multiplier, resolved.Contract.Exchange, resolved.Contract.PrimaryExch
	out.Currency, out.LocalSymbol, out.TradingClass, out.MinTick = resolved.Contract.Currency, resolved.Contract.LocalSymbol, resolved.Contract.TradingClass, resolved.MinTick
	if out.ConID <= 0 || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.ContractParams{}, "", brokerWriteTransactionDriftError()
	}
	return out, resolved.TimeZoneID, nil
}

func guaranteedUSOptionComboLeg(contract rpc.ContractParams, timeZone string) bool {
	zone := strings.ToUpper(strings.TrimSpace(timeZone))
	return strings.EqualFold(contract.SecType, "OPT") && strings.EqualFold(contract.Exchange, "SMART") &&
		strings.EqualFold(contract.Currency, "USD") && contract.ConID > 0 && contract.Multiplier == 100 &&
		(zone == "US/EASTERN" || zone == "AMERICA/NEW_YORK")
}

func (s *Server) reconcileJournaledStrategyGroups(options []rpc.PositionView, groups []rpc.PositionStrategy, issues []rpc.StrategyGroupingIssue) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
	if s == nil || s.orderJournal == nil {
		return groups, issues
	}
	latest, err := s.strategyLineages()
	if err != nil {
		for i := range groups {
			groups[i].Actionable = false
			groups[i].Status = rpc.PositionStrategyStatusReview
			groups[i].Reason = "Close and reduce are paused until Canary can read its order history."
		}
		return groups, issues
	}
	if len(latest) == 0 {
		return groups, issues
	}
	quantityByConID := make(map[int]float64, len(options))
	for _, option := range options {
		if option.ConID > 0 {
			quantityByConID[option.ConID] = option.Quantity
		}
	}
	for id, known := range latest {
		units, currentLegs, state := currentKnownStrategyUnits(known, quantityByConID)
		if state == knownStrategyClosed {
			continue
		}
		index := -1
		for i := range groups {
			if groups[i].ID == id {
				index = i
				break
			}
		}
		if state == knownStrategyBroken {
			if index >= 0 {
				groups = append(groups[:index], groups[index+1:]...)
			}
			groups = append(groups, rpc.PositionStrategy{
				ID: id, Revision: known.StrategyRevision + 1, Underlying: known.Legs[0].Contract.Symbol,
				Source: rpc.PositionStrategySourceCanary, Status: rpc.PositionStrategyStatusReview,
				Legs: currentLegs, Actionable: false,
				Reason: "The leg quantities no longer match the strategy ratio. Review the broker fills before taking another action.",
			})
			issues = append(issues, rpc.StrategyGroupingIssue{
				Underlying: known.Legs[0].Contract.Symbol, LegCount: len(known.Legs),
				Reason: "leg quantities no longer match the recorded strategy ratio",
			})
			continue
		}
		if index >= 0 {
			groups[index].Source = rpc.PositionStrategySourceCanary
			groups[index].Units = units
			if !sameKnownStrategyBefore(known, quantityByConID) {
				groups[index].Revision = known.StrategyRevision + 1
			}
			groups[index].Reason = ""
		}
	}
	return groups, issues
}

func (s *Server) strategyLineages() (map[string]rpc.StrategyOrderDraft, error) {
	s.strategyLineageMu.Lock()
	defer s.strategyLineageMu.Unlock()
	if !s.strategyLineageLoaded {
		events, err := s.orderJournal.LoadEvents(0)
		if err != nil {
			return nil, err
		}
		s.strategyLineage = make(map[string]rpc.StrategyOrderDraft)
		for _, event := range events {
			if event.Type == orderJournalEventSendAttempted && event.StrategyGroup != nil && event.StrategyGroup.StrategyID != "" && len(event.StrategyGroup.Legs) >= 2 {
				s.strategyLineage[event.StrategyGroup.StrategyID] = *event.StrategyGroup
			}
		}
		s.strategyLineageLoaded = true
	}
	out := make(map[string]rpc.StrategyOrderDraft, len(s.strategyLineage))
	maps.Copy(out, s.strategyLineage)
	return out, nil
}

func (s *Server) rememberStrategyLineage(group *rpc.StrategyOrderDraft) {
	if s == nil || group == nil || group.StrategyID == "" || len(group.Legs) < 2 {
		return
	}
	s.strategyLineageMu.Lock()
	defer s.strategyLineageMu.Unlock()
	if s.strategyLineage == nil {
		s.strategyLineage = make(map[string]rpc.StrategyOrderDraft)
	}
	s.strategyLineage[group.StrategyID] = *group
	// If the cache was not loaded yet, leave the flag false so restart history
	// is merged before this process serves the first position reconciliation.
}

const (
	knownStrategyCurrent = iota
	knownStrategyClosed
	knownStrategyBroken
)

func currentKnownStrategyUnits(known rpc.StrategyOrderDraft, quantities map[int]float64) (int, []rpc.PositionStrategyLeg, int) {
	legs := make([]rpc.PositionStrategyLeg, 0, len(known.Legs))
	currentUnits := -1
	allZero := true
	for _, leg := range known.Legs {
		quantity := quantities[leg.Contract.ConID]
		if math.Abs(quantity) > 1e-9 {
			allZero = false
		}
		legs = append(legs, rpc.PositionStrategyLeg{Contract: leg.Contract, Quantity: quantity, Ratio: leg.Ratio})
		if leg.Ratio == 0 {
			return 0, legs, knownStrategyBroken
		}
		units := quantity / float64(leg.Ratio)
		rounded := math.Round(units)
		if units < -1e-9 || math.Abs(units-rounded) > 1e-9 {
			return 0, legs, knownStrategyBroken
		}
		if currentUnits < 0 {
			currentUnits = int(rounded)
		} else if currentUnits != int(rounded) {
			return 0, legs, knownStrategyBroken
		}
	}
	if allZero {
		return 0, legs, knownStrategyClosed
	}
	return currentUnits, legs, knownStrategyCurrent
}

func sameKnownStrategyBefore(known rpc.StrategyOrderDraft, quantities map[int]float64) bool {
	for _, leg := range known.Legs {
		if math.Abs(quantities[leg.Contract.ConID]-leg.Before) > 1e-9 {
			return false
		}
	}
	return true
}

func strategyPreviewJournalEvent(draft rpc.OrderDraft, status rpc.TradingStatus, tokenID string, at time.Time, whatIf rpc.OrderWhatIfResult) orderJournalEvent {
	return orderJournalEvent{
		At: at, Type: orderJournalEventPreviewed, OrderRef: draft.OrderRef, PreviewTokenID: tokenID,
		ClientID: status.ClientID, Account: status.Account, Endpoint: status.Endpoint, Mode: status.Mode,
		Symbol: draft.Contract.Symbol, SecType: draft.Contract.SecType, Exchange: draft.Contract.Exchange,
		Currency: draft.Contract.Currency, Action: draft.Action, OrderType: draft.OrderType, TIF: draft.TIF,
		Quantity: float64(draft.Quantity), LimitPrice: draft.LimitPrice, OpenClose: draft.OpenClose,
		Source: draft.Source, StrategyGroup: draft.StrategyGroup, Message: previewWhatIfJournalMessage(whatIf),
	}
}
