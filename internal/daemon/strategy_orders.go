package daemon

import (
	"context"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"
	positionstrategy "github.com/osauer/canary/v3/internal/strategy"
)

func (s *Server) handleStrategyPreview(ctx context.Context, req *rpc.Request) (*rpc.OrderPreviewResult, error) {
	var p rpc.StrategyPreviewParams
	if err := decodeFor(api.StrategyPreview, req.Params, &p); err != nil {
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
	heldOptions := make(map[int]float64, len(positions.Options))
	for _, row := range positions.Options {
		if row.ConID > 0 {
			heldOptions[row.ConID] = row.Quantity
		}
	}
	// An open names its two contracts; nothing is held yet, so there is no
	// group to look up (strategy_open.go).
	opening := strings.EqualFold(strings.TrimSpace(p.Operation), rpc.StrategyOperationOpen)
	var group rpc.PositionStrategy
	if opening {
		group, err = strategyOpenGroup(positions, p)
	} else {
		group, err = currentStrategyForPreview(positions.Strategies, p.StrategyID, p.ExpectedRevision)
	}
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
		return nil, errBadRequest("a strategy order supports DAY time-in-force only")
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
	// An add opens every leg further in the held direction, an open opens
	// every leg from flat; a close or reduction trades every leg back
	// toward flat.
	adding := operation == rpc.StrategyOperationAdd || opening
	if adding {
		draftGroup.UnitsAfter = group.Units + units
	}
	currency := ""
	quotes := make([]rpc.OrderQuoteSnapshot, 0, len(group.Legs))
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
		if (held.Ratio < 0) != adding {
			action = rpc.OrderActionBuy
		}
		quantity := absOrderRatio(held.Ratio) * units
		// The contract as the book holds it: a recorded strategy's leg may
		// sit beside more of the same contract held on its own.
		before := held.Quantity
		if q, ok := heldOptions[held.Contract.ConID]; ok || opening {
			before = q
		}
		after := before
		if action == rpc.OrderActionSell {
			after -= float64(quantity)
		} else {
			after += float64(quantity)
		}
		draftGroup.Legs = append(draftGroup.Legs, rpc.StrategyOrderLeg{
			Contract: resolved, Ratio: held.Ratio, Action: action, Quantity: quantity,
			Before: before, After: after,
		})

		quote, err := s.fetchPreviewQuoteBound(ctx, resolved, timeout, previewAuthority)
		if err != nil {
			return nil, err
		}
		if opening && quote.Bid != nil && quote.Ask != nil && positiveFinite(*quote.Bid) && positiveFinite(*quote.Ask) {
			draftGroup.Legs[len(draftGroup.Legs)-1].Mark = roundPrice((*quote.Bid + *quote.Ask) / 2)
		}
		quotes = append(quotes, quote)
	}
	quote, limit, grossNotional, err := strategyPriceQuote(group.Underlying, draftGroup.Legs, quotes, p.LimitPrice)
	if err != nil {
		return nil, err
	}
	if opening {
		if err := validateStrategyOpenVertical(draftGroup, limit); err != nil {
			return nil, refusePreview(errBadRequest(err.Error()), rpc.TradingBlocker{Code: strategyOpenBlockerCode, Message: err.Error(), Action: "Choose a debit vertical: buy the option nearer the money and sell one further out, same expiry, for less than the strike width."})
		}
	}
	if currency == "" {
		return nil, errBadRequest("strategy has incomplete executable leg evidence")
	}

	now := s.orderNow()
	draft := rpc.OrderDraft{
		Action:   rpc.OrderActionSell,
		Contract: rpc.ContractParams{Symbol: group.Underlying, SecType: "BAG", Exchange: "SMART", Currency: currency},
		Quantity: units, OrderType: rpc.OrderTypeLMT, LimitPrice: limit, TIF: tif,
		Strategy: "group-close", OrderRef: previewOrderRef(now), OpenClose: "C",
		Source: strings.TrimSpace(p.Source), StrategyGroup: draftGroup,
	}
	if adding {
		// Bought as one combination, the limit is the most paid per unit
		// (negative: the least received). A covered add can lose that
		// premium and no more, so the order cap reads it at the limit; any
		// other add keeps the gross premium of its legs (strategy_add.go).
		draft.Action, draft.Strategy, draft.OpenClose = rpc.OrderActionBuy, "group-add", "O"
		if opening {
			draft.Strategy = "group-open"
		}
		grossNotional = strategyAddCapNotional(draft, grossNotional)
	}
	positionAuthority, err := s.captureBoundStrategyPositionAuthority(ctx, previewAuthority.connector, previewAuthority.session, status, *draftGroup)
	if err != nil {
		return nil, err
	}
	position := positionAuthority.Impact
	_, tradingControlGeneration := s.effectiveTradingControlSnapshot()
	notionalAuthority, err := s.captureOrderNotionalAuthority(ctx, previewAuthority, grossNotional, currency, positionAuthority.BaseCurrency, timeout)
	if err != nil {
		return nil, err
	}
	limits := s.orderLimitsInForceForPreview(ctx, positionAuthority.BaseCurrency)
	exitInventory := s.captureProtectiveExitInventory(ctx, status, draft, position, orderPreviewReplaceTarget{})
	deltaEvidence := s.captureDeltaReductionEvidence(ctx, status, draft, position, limits, notionalAuthority)
	if err := validateOrderRiskAuthority(limits, draft, position, notionalAuthority, positionAuthority.BaseCurrency, exitInventory, deltaEvidence); err != nil {
		return nil, refusePreview(errBadRequest(err.Error()), orderRiskLimitBlocker(limits, err))
	}
	// Read broker inventory and correlated fill quantities outside the wire
	// lock, as the single-order preview does; only an add needs them.
	if _, err := s.prepareMandatePremium(ctx, draft, position); err != nil {
		return nil, refusePreview(errBadRequest("Mandate premium: "+err.Error()), rpc.TradingBlocker{Code: rpc.MandatePremiumBlockerCode, Message: "Mandate premium: " + err.Error(), Action: "Refresh orders and positions before adding option premium."})
	}
	brake, gateWarnings := s.riskPolicyBrake(draft, position)
	if brake != nil {
		return nil, refusePreview(errBadRequest(brake.Message), *brake)
	}
	mandateBlocker, mandateWarning := s.mandateBudgetJudgement(draft, position)
	if mandateBlocker != nil {
		return nil, refusePreview(errBadRequest(mandateBlocker.Message), *mandateBlocker)
	}
	if mandateWarning != nil {
		gateWarnings = append(gateWarnings, *mandateWarning)
	}
	whatIf, err := s.fetchPreviewWhatIfBound(ctx, status, draft, timeout, previewAuthority)
	if err != nil {
		return nil, err
	}
	if !s.orderPreviewBrokerAuthorityCurrent(previewAuthority) {
		return nil, brokerWriteTransactionDriftError()
	}
	if adding {
		// An add opens options, so a recorded denial of the option class
		// refuses it as it refuses a single option bought to open.
		if b := s.brokerPermissionBlocker(draftGroup.Legs[0].Contract); b != nil {
			return nil, refusePreview(errBadRequest(b.Message), *b)
		}
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
	warnings := append(gateWarnings, previewWhatIfWarnings(whatIf)...)
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
		MaxNotional: limits.CapBase, WhatIf: whatIf, Warnings: warnings, AsOf: now,
	}, nil
}

// strategyPriceQuote combines the exact leg observations used for the combo's
// price and risk notional, before a broker WhatIf or preview token is requested.
func strategyPriceQuote(symbol string, legs []rpc.StrategyOrderLeg, quotes []rpc.OrderQuoteSnapshot, explicit *float64) (rpc.OrderQuoteSnapshot, float64, float64, error) {
	var aggregateBid, aggregateAsk, grossNotional float64
	aggregate := rpc.OrderQuoteSnapshot{Symbol: symbol}
	var staleReasons []string
	if len(legs) < 2 || len(legs) != len(quotes) {
		return rpc.OrderQuoteSnapshot{}, 0, 0, errBadRequest("strategy has incomplete executable leg evidence")
	}
	for i, leg := range legs {
		quote := quotes[i]
		if explicit == nil {
			if err := requireFreshPreviewQuote(quote, "strategy automatic limit"); err != nil {
				return rpc.OrderQuoteSnapshot{}, 0, 0, err
			}
			if quote.DataType != rpc.MarketDataLive {
				return rpc.OrderQuoteSnapshot{}, 0, 0, refusePreviewCode(previewQuoteNotLiveCode, errBadRequest("strategy automatic limit requires live bid/ask data for every leg; use --limit for an explicit price"))
			}
		}
		if quote.Bid == nil || quote.Ask == nil || !positiveFinite(*quote.Bid) || !positiveFinite(*quote.Ask) || *quote.Ask < *quote.Bid {
			return rpc.OrderQuoteSnapshot{}, 0, 0, fmt.Errorf("%w: complete bid/ask evidence is unavailable for strategy leg %d", ErrTradingDisabled, leg.Contract.ConID)
		}
		ratio := float64(absOrderRatio(leg.Ratio))
		if leg.Ratio > 0 {
			aggregateBid += ratio * *quote.Bid
			aggregateAsk += ratio * *quote.Ask
		} else {
			aggregateBid -= ratio * *quote.Ask
			aggregateAsk -= ratio * *quote.Bid
		}
		mid := (*quote.Bid + *quote.Ask) / 2
		grossNotional += float64(leg.Quantity) * mid * float64(contractMultiplier(leg.Contract))
		// The oldest observation bounds a multi-leg quote; one fresh leg must
		// never make an older or qualified leg look current.
		if i == 0 || quote.AsOf.Before(aggregate.AsOf) {
			aggregate.AsOf = quote.AsOf
		}
		if i == 0 || quote.PriceAt.Before(aggregate.PriceAt) {
			aggregate.PriceAt, aggregate.PriceAsOf = quote.PriceAt, quote.PriceAsOf
		}
		if i == 0 {
			aggregate.DataType, aggregate.QuoteQuality = quote.DataType, quote.QuoteQuality
			aggregate.SessionContext = quote.SessionContext
		} else {
			if aggregate.DataType != quote.DataType {
				aggregate.DataType = "mixed"
			}
			if aggregate.QuoteQuality != quote.QuoteQuality {
				aggregate.QuoteQuality = "mixed"
			}
			if quote.SessionContext == nil || aggregate.SessionContext == nil {
				aggregate.SessionContext = nil
			} else if !quote.SessionContext.IsOpen {
				aggregate.SessionContext = quote.SessionContext
			}
		}
		if quote.Stale {
			aggregate.Stale = true
			staleReasons = append(staleReasons, fmt.Sprintf("leg %d: %s", leg.Contract.ConID, quote.StaleReason))
		}
		aggregate.Warnings = append(aggregate.Warnings, quote.Warnings...)
	}
	if !positiveFinite(grossNotional) {
		return rpc.OrderQuoteSnapshot{}, 0, 0, errBadRequest("strategy has incomplete executable leg evidence")
	}
	limit := roundPrice((aggregateBid + aggregateAsk) / 2)
	if explicit != nil {
		if math.IsNaN(*explicit) || math.IsInf(*explicit, 0) {
			return rpc.OrderQuoteSnapshot{}, 0, 0, errBadRequest("strategy limit price must be finite")
		}
		limit = roundPrice(*explicit)
	}
	midpoint := roundPrice((aggregateBid + aggregateAsk) / 2)
	aggregate.StaleReason = strings.Join(staleReasons, "; ")
	aggregate.Bid, aggregate.Ask, aggregate.Midpoint = new(roundPrice(aggregateBid)), new(roundPrice(aggregateAsk)), &midpoint
	return aggregate, limit, grossNotional, nil
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
	case rpc.StrategyOperationAdd, rpc.StrategyOperationOpen:
		if units <= 0 {
			return "", 0, errBadRequest(operation + " units must be positive")
		}
		return operation, units, nil
	default:
		return "", 0, errBadRequest("operation must be close, reduce, add or open")
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

// reconcileJournaledStrategyGroups lays Canary's own order records over the
// grouping inferred from the book. A strategy a Canary combination opened or
// traded is the group its record says, whatever else the name holds: a
// spread opened beside another option of the same underlying, or over a
// contract the book held already, is listed as that spread, and what the
// book holds beyond it is grouped again on its own (an extra bought contract
// a line of its own). A record whose legs no longer form whole units is a
// strategy for review; an open that never filled holds nothing sold and is
// closed (currentKnownVerticalUnits).
func (s *Server) reconcileJournaledStrategyGroups(stocks, options []rpc.PositionView, groups []rpc.PositionStrategy, issues []rpc.StrategyGroupingIssue) ([]rpc.PositionStrategy, []rpc.StrategyGroupingIssue) {
	if s == nil || s.orderJournal == nil {
		return groups, issues
	}
	latest, err := s.strategyLineages()
	if err != nil {
		for i := range groups {
			groups[i].Actionable = false
			groups[i].Status = rpc.PositionStrategyStatusReview
			groups[i].Reason = "Strategy orders are paused until Canary can read its order history."
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
	ids := slices.Sorted(maps.Keys(latest))
	owned := map[string][]rpc.PositionStrategy{}
	claimed := map[int]float64{}
	for _, id := range ids {
		known := latest[id]
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
		changed := !sameKnownStrategyBefore(known, quantityByConID)
		// The shape is the record's at its units; the revision is the
		// grouping's while the book is as the record left it.
		group := strategyFromLineage(stocks, id, known, units)
		group.Revision = max(known.StrategyRevision, 1)
		if index >= 0 {
			group.Revision = groups[index].Revision
		}
		if changed {
			group.Revision = known.StrategyRevision + 1
		}
		group.Source, group.Units, group.Reason = rpc.PositionStrategySourceCanary, units, ""
		group.Legs = make([]rpc.PositionStrategyLeg, len(known.Legs))
		for i, leg := range known.Legs {
			group.Legs[i] = rpc.PositionStrategyLeg{Contract: leg.Contract, Quantity: float64(units * leg.Ratio), Ratio: leg.Ratio}
			claimed[leg.Contract.ConID] += float64(units * leg.Ratio)
		}
		underlying := normSym(group.Underlying)
		owned[underlying] = append(owned[underlying], group)
	}
	// Every name a record owns a strategy on is grouped again: the records'
	// strategies, then what the book holds beyond them, inferred as before.
	for _, underlying := range slices.Sorted(maps.Keys(owned)) {
		kept := groups[:0:0]
		for _, g := range groups {
			if normSym(g.Underlying) != underlying || g.Status == rpc.PositionStrategyStatusReview {
				kept = append(kept, g)
			}
		}
		keptIssues := issues[:0:0]
		for _, is := range issues {
			if normSym(is.Underlying) != underlying {
				keptIssues = append(keptIssues, is)
			}
		}
		var rest []rpc.PositionView
		for _, option := range options {
			if normSym(option.Symbol) != underlying {
				continue
			}
			option.Quantity -= claimed[option.ConID]
			if math.Abs(option.Quantity) > 1e-9 {
				rest = append(rest, option)
			}
		}
		inferred, inferredIssues := positionstrategy.InferStrategies(stocks, rest)
		groups = append(append(kept, owned[underlying]...), inferred...)
		issues = append(keptIssues, inferredIssues...)
	}
	return groups, issues
}

// strategyFromLineage is the group a record's legs form at units, named as
// the grouping names that shape.
func strategyFromLineage(stocks []rpc.PositionView, id string, known rpc.StrategyOrderDraft, units int) rpc.PositionStrategy {
	symbol := normSym(known.Legs[0].Contract.Symbol)
	rows := make([]rpc.PositionView, 0, len(known.Legs))
	for _, leg := range known.Legs {
		c := leg.Contract
		rows = append(rows, rpc.PositionView{Symbol: symbol, SecType: "OPT", ConID: c.ConID, Currency: c.Currency, Exchange: c.Exchange, Quantity: float64(units * leg.Ratio),
			Multiplier: contractMultiplier(c), Expiry: c.Expiry, Strike: c.Strike, Right: c.Right, TradingClass: c.TradingClass, LocalSymbol: c.LocalSymbol})
	}
	group := rpc.PositionStrategy{ID: id, Underlying: symbol, Status: rpc.PositionStrategyStatusCurrent, Actionable: true}
	if inferred, _ := positionstrategy.InferStrategies(stocks, rows); len(inferred) == 1 {
		group.Kind, group.Name, group.PositionFingerprint, group.Stock = inferred[0].Kind, inferred[0].Name, inferred[0].PositionFingerprint, inferred[0].Stock
	}
	return group
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
	if units, legs, state, ok := currentKnownVerticalUnits(known, quantities); ok {
		return units, legs, state
	}
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

// currentKnownVerticalUnits reads a recorded debit vertical (one bought and
// one sold option, 1:1, the sold one capped by the bought: strategyAddCovered)
// by its sold leg: its units are the contracts sold, and the bought leg must
// hold at least as many, any more being a line of its own (an open over a
// held contract, a contract bought later alone). A sold leg the bought one
// no longer covers is for review; with none sold the spread is closed. ok is
// false for every other record.
func currentKnownVerticalUnits(known rpc.StrategyOrderDraft, quantities map[int]float64) (int, []rpc.PositionStrategyLeg, int, bool) {
	if len(known.Legs) != 2 || !strategyAddCovered(&known) {
		return 0, nil, 0, false
	}
	var long, short rpc.StrategyOrderLeg
	for _, leg := range known.Legs {
		switch leg.Ratio {
		case 1:
			long = leg
		case -1:
			short = leg
		default:
			return 0, nil, 0, false
		}
	}
	held := func(leg rpc.StrategyOrderLeg) float64 { return quantities[leg.Contract.ConID] }
	legs := make([]rpc.PositionStrategyLeg, 0, 2)
	for _, leg := range known.Legs {
		legs = append(legs, rpc.PositionStrategyLeg{Contract: leg.Contract, Quantity: held(leg), Ratio: leg.Ratio})
	}
	sold, ok := wholeUnits(-held(short))
	switch {
	case !ok || sold < 0:
		return 0, legs, knownStrategyBroken, true
	case sold == 0:
		return 0, legs, knownStrategyClosed, true
	case held(long) < float64(sold)-1e-9:
		return 0, legs, knownStrategyBroken, true
	}
	return sold, legs, knownStrategyCurrent, true
}

func wholeUnits(v float64) (int, bool) {
	r := math.Round(v)
	return int(r), math.Abs(v-r) <= 1e-9
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
