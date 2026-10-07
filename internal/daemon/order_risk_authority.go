package daemon

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

type orderPositionAuthority struct {
	Impact                 rpc.OrderPositionImpact
	Generation             uint64
	Health                 ibkrlib.PortfolioStreamHealth
	EvidenceAt             time.Time
	BaseCurrency           string
	BaseCurrencyProvenance ibkrlib.AccountBaseCurrencyProvenance
	TestOnly               bool
}

const (
	orderFXSourceIdentity          = "currency_identity"
	orderFXSourceExactSessionQuote = "ibkr.tws.exact_fx_quote"
	orderFXQuoteBudget             = 3 * time.Second
)

// orderFXBasePriority is the conventional major-FX base-currency ordering
// accepted by IBKR's CASH/IDEALPRO contracts. The lower-ranked currency is
// the wire symbol and the higher-ranked currency is the wire quote currency.
// pkg/ibkr.FxPair; an unknown pair fails closed instead of guessing a route.
var orderFXBasePriority = map[string]int{
	"EUR": 0,
	"GBP": 1,
	"AUD": 2,
	"NZD": 3,
	"USD": 4,
	"CAD": 5,
	"CHF": 6,
	"JPY": 7,
}

// orderNotionalAuthority is the typed measurement used by the account-base
// account-currency cap. Cross-currency evidence must come from an exact-session
// TWS CASH/IDEALPRO quote, never a streaming ledger inference or stale FX cache.
type orderNotionalAuthority struct {
	QuoteNotional    float64   `json:"quote_notional"`
	ContractCurrency string    `json:"contract_currency"`
	BaseNotional     float64   `json:"base_notional"`
	BaseCurrency     string    `json:"base_currency"`
	BasePerContract  float64   `json:"base_per_contract"`
	EvidenceAt       time.Time `json:"evidence_at"`
	DataType         string    `json:"data_type"`
	Source           string    `json:"source"`
}

// orderPreviewBrokerAuthority binds every production preview read to one
// daemon-published Connector and one physical broker socket generation. The
// same authority resolves the contract, reads the portfolio, runs WhatIf, and
// is revalidated immediately before the signed token is minted.
type orderPreviewBrokerAuthority struct {
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	session        ibkrlib.ConnectorSessionBinding
}

func (s *Server) hasOrderPreviewBrokerTestSeam() bool {
	return s.orderPreviewQuote != nil || s.orderPreviewPositionImpact != nil || s.orderRiskAuthorityForTest != nil ||
		s.orderContractResolverForTest != nil || s.orderPreviewWhatIf != nil || s.orderBondDetailsForTest != nil || s.orderBondLookupForTest != nil
}

func (s *Server) captureOrderPreviewBrokerAuthority() (*orderPreviewBrokerAuthority, error) {
	s.mu.Lock()
	connector, connectorEpoch := s.connector, s.connectorEpoch
	s.mu.Unlock()
	if connector == nil || !connector.IsReady() {
		if s.hasOrderPreviewBrokerTestSeam() {
			return nil, nil
		}
		s.triggerReconnect()
		return nil, s.gatewayUnavailableError()
	}
	session, ok := connector.CaptureSession()
	if !ok {
		return nil, fmt.Errorf("%w: broker session changed before contract resolution", ErrTradingDisabled)
	}
	authority := &orderPreviewBrokerAuthority{connector: connector, connectorEpoch: connectorEpoch, session: session}
	if !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return nil, fmt.Errorf("%w: broker session changed before contract resolution", ErrTradingDisabled)
	}
	return authority, nil
}

func (s *Server) orderPreviewBrokerAuthorityCurrent(authority *orderPreviewBrokerAuthority) bool {
	if s == nil || authority == nil || authority.connector == nil {
		return false
	}
	s.mu.Lock()
	current := s.connector == authority.connector && s.connectorEpoch == authority.connectorEpoch
	s.mu.Unlock()
	return current && authority.connector.SessionCurrent(authority.session)
}

func (s *Server) resolvePreviewOrderContract(ctx context.Context, authority *orderPreviewBrokerAuthority, contract rpc.ContractParams, timeout time.Duration) (rpc.ContractParams, error) {
	if s.orderContractResolverForTest != nil {
		resolved, err := s.orderContractResolverForTest(ctx, contract, timeout)
		if err != nil {
			return rpc.ContractParams{}, err
		}
		if resolved.ConID <= 0 {
			return rpc.ContractParams{}, fmt.Errorf("%w: contract resolver returned no positive ConID", ErrTradingDisabled)
		}
		return resolved, nil
	}
	// Existing unit tests may replace the complete quote/position/WhatIf
	// authority without constructing a socket. Production never takes this
	// path because no test seam is installed.
	if authority == nil {
		return contract, nil
	}
	resolved, err := authority.connector.ResolveOrderContractForSession(ctx, authority.session, *previewIBKRContract(contract), timeout)
	if err != nil {
		return rpc.ContractParams{}, fmt.Errorf("%w: exact contract resolution failed: %v", ErrTradingDisabled, err)
	}
	result := contract
	result.ConID = resolved.Contract.ConID
	result.Symbol = resolved.Contract.Symbol
	result.SecType = resolved.Contract.SecType
	result.Expiry = resolved.Contract.Expiry
	result.Strike = resolved.Contract.Strike
	result.Right = resolved.Contract.Right
	result.Multiplier = resolved.Contract.Multiplier
	result.Exchange = resolved.Contract.Exchange
	result.PrimaryExch = resolved.Contract.PrimaryExch
	result.Currency = resolved.Contract.Currency
	result.LocalSymbol = resolved.Contract.LocalSymbol
	result.TradingClass = resolved.Contract.TradingClass
	result.MinTick = resolved.MinTick
	if result.ConID <= 0 || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return rpc.ContractParams{}, fmt.Errorf("%w: broker session changed during contract resolution", ErrTradingDisabled)
	}
	return result, nil
}

func (s *Server) capturePreviewOrderPositionAuthority(ctx context.Context, status rpc.TradingStatus, contract rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
	if s.orderRiskAuthorityForTest != nil {
		return s.orderRiskAuthorityForTest(ctx, status, contract, action, qty)
	}
	if s.orderPreviewPositionImpact != nil {
		impact, err := s.orderPreviewPositionImpact(ctx, contract, action, qty)
		if err != nil {
			return orderPositionAuthority{}, err
		}
		now := s.orderNow()
		return orderPositionAuthority{
			Impact: impact, Generation: 1, BaseCurrency: strings.ToUpper(strings.TrimSpace(contract.Currency)), BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag, TestOnly: true,
			Health: ibkrlib.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 1}, EvidenceAt: now,
		}, nil
	}
	c := s.gatewayConnector()
	if c == nil {
		return orderPositionAuthority{}, s.gatewayUnavailableError()
	}
	session, ok := c.CaptureSession()
	if !ok {
		return orderPositionAuthority{}, fmt.Errorf("%w: broker session changed before portfolio validation", ErrTradingDisabled)
	}
	return s.captureBoundOrderPositionAuthority(ctx, c, session, status, contract, action, qty)
}

func (s *Server) captureBoundOrderPositionAuthority(ctx context.Context, connector *ibkrlib.Connector, session ibkrlib.ConnectorSessionBinding, status rpc.TradingStatus, contract rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
	if s.orderRiskAuthorityForTest != nil {
		return s.orderRiskAuthorityForTest(ctx, status, contract, action, qty)
	}
	if s.orderPreviewPositionImpact != nil {
		impact, err := s.orderPreviewPositionImpact(ctx, contract, action, qty)
		if err != nil {
			return orderPositionAuthority{}, err
		}
		now := s.orderNow()
		return orderPositionAuthority{
			Impact: impact, Generation: 1, BaseCurrency: strings.ToUpper(strings.TrimSpace(contract.Currency)), BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag, TestOnly: true,
			Health: ibkrlib.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 1}, EvidenceAt: now,
		}, nil
	}
	if connector == nil {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	projection, ok := connector.CapturePortfolioProjectionForSession(session)
	if !ok {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	if health := classifyPortfolioStreamHealth(scope, projection.Health, s.orderNow()); health != orderIntegrityHealthCurrent {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account-scoped portfolio evidence is %s; refresh and preview again", ErrTradingDisabled, health)
	}
	if !cachedPositionsMatchBrokerScope(projection.Positions, scope) {
		return orderPositionAuthority{}, fmt.Errorf("%w: portfolio evidence belongs to another account; refresh and preview again", ErrTradingDisabled)
	}
	positionEvidence, err := exactRiskPositionEvidenceForContract(projection.Positions, contract)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: exact contract position evidence unavailable: %v", ErrTradingDisabled, err)
	}
	before := positionEvidence.Quantity
	delta := float64(qty)
	if strings.EqualFold(action, rpc.OrderActionSell) {
		delta = -delta
	}
	authority := orderPositionAuthority{
		Impact: rpc.OrderPositionImpact{
			Before: before, After: before + delta, Effect: classifyPositionEffect(before, before+delta),
			AverageCost: positionEvidence.AverageCost, Multiplier: positionEvidence.Multiplier,
		},
		Generation: projection.Generation, Health: projection.Health,
		EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
	}
	// A position-only close/reduce classification does not grant a risk
	// exemption: reqAllOpenOrders cannot prove future manual-TWS activity. Only
	// the narrow protective stock exit (protective_exit.go) is excused.
	// Account base currency is immutable for one concrete broker account and
	// ExchangeRate=1 inference is never authority for an order cap.
	account, provenance, err := connector.RequestAccountSummaryWithProvenance(ctx, 3*time.Second)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account base-currency evidence unavailable: %v", ErrTradingDisabled, err)
	}
	if provenance != ibkrlib.AccountSummaryProvenanceRequest || account == nil || !strings.EqualFold(strings.TrimSpace(account.AccountID), strings.TrimSpace(status.Account)) {
		return orderPositionAuthority{}, fmt.Errorf("%w: current exact-account base-currency evidence unavailable", ErrTradingDisabled)
	}
	base, ok := rulebookBaseCurrency(account.BaseCurrency)
	if !ok || !account.BaseCurrencyProvenance.Proven() {
		return orderPositionAuthority{}, fmt.Errorf("%w: explicit account base-currency evidence is unavailable (provenance %q)", ErrTradingDisabled, account.BaseCurrencyProvenance)
	}
	if !connector.SessionCurrent(session) {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	authority.BaseCurrency = base
	authority.BaseCurrencyProvenance = account.BaseCurrencyProvenance
	return authority, nil
}

func (s *Server) captureBoundStrategyPositionAuthority(ctx context.Context, connector *ibkrlib.Connector, session ibkrlib.ConnectorSessionBinding, status rpc.TradingStatus, draft rpc.StrategyOrderDraft) (orderPositionAuthority, error) {
	if len(draft.Legs) < 2 || draft.Units <= 0 || draft.UnitsBefore <= 0 || draft.UnitsAfter != draft.UnitsBefore-draft.Units {
		return orderPositionAuthority{}, fmt.Errorf("%w: invalid strategy position binding", ErrTradingDisabled)
	}
	first := draft.Legs[0]
	base, err := s.captureBoundOrderPositionAuthority(ctx, connector, session, status, first.Contract, first.Action, first.Quantity)
	if err != nil {
		return orderPositionAuthority{}, err
	}
	projection, ok := connector.CapturePortfolioProjectionForSession(session)
	if !ok || projection.Generation != base.Generation {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	if err := verifyStrategyPositionProjection(projection.Positions, draft); err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: strategy position changed: %v", ErrTradingDisabled, err)
	}
	base.Impact = rpc.OrderPositionImpact{
		Before: float64(draft.UnitsBefore), After: float64(draft.UnitsAfter),
		Effect: classifyPositionEffect(float64(draft.UnitsBefore), float64(draft.UnitsAfter)),
	}
	return base, nil
}

// captureWireOrderPositionAuthority reads only already-bound, in-memory
// evidence. Base currency is the session-invariant value captured again at
// preview-token redemption; the first-byte guard deliberately reuses that
// binding instead of issuing a broker request or reparsing unstamped cache.
// The real transport calls this while WithBoundBrokerSession owns
// evidenceBarrier.W. It must never issue a broker request or recursively take
func (s *Server) captureWireOrderPositionAuthority(binding brokerWriteTransactionBinding, status rpc.TradingStatus, draft rpc.OrderDraft) (orderPositionAuthority, error) {
	if binding.testOnly && s.orderRiskAuthorityForTest == nil && s.orderPreviewPositionImpact == nil {
		return orderPositionAuthority{
			Impact: binding.riskPosition, Generation: binding.riskPortfolioGeneration,
			Health:       ibkrlib.PortfolioStreamHealth{Account: binding.riskPortfolioAccount, ProjectionGeneration: binding.riskPortfolioGeneration},
			BaseCurrency: binding.riskBaseCurrency, BaseCurrencyProvenance: binding.riskBaseCurrencyProvenance, TestOnly: true,
		}, nil
	}
	if s.orderRiskAuthorityForTest != nil || s.orderPreviewPositionImpact != nil {
		return s.captureBoundOrderPositionAuthority(context.Background(), binding.connector, binding.session, status, draft.Contract, draft.Action, draft.Quantity)
	}
	if binding.connector == nil {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	projection, ok := binding.connector.CapturePortfolioProjectionForBoundSession(binding.session)
	if !ok {
		return orderPositionAuthority{}, brokerWriteTransactionDriftError()
	}
	scope := brokerStateScope{Account: status.Account, Mode: status.Mode}
	if health := classifyPortfolioStreamHealth(scope, projection.Health, s.orderNow()); health != orderIntegrityHealthCurrent {
		return orderPositionAuthority{}, fmt.Errorf("%w: current account-scoped portfolio evidence is %s; refresh and preview again", ErrTradingDisabled, health)
	}
	if !cachedPositionsMatchBrokerScope(projection.Positions, scope) {
		return orderPositionAuthority{}, fmt.Errorf("%w: portfolio evidence belongs to another account; refresh and preview again", ErrTradingDisabled)
	}
	if draft.StrategyGroup != nil {
		if err := verifyStrategyPositionProjection(projection.Positions, *draft.StrategyGroup); err != nil {
			return orderPositionAuthority{}, fmt.Errorf("%w: strategy position changed: %v", ErrTradingDisabled, err)
		}
		return orderPositionAuthority{
			Impact:     rpc.OrderPositionImpact{Before: float64(draft.StrategyGroup.UnitsBefore), After: float64(draft.StrategyGroup.UnitsAfter), Effect: classifyPositionEffect(float64(draft.StrategyGroup.UnitsBefore), float64(draft.StrategyGroup.UnitsAfter))},
			Generation: projection.Generation, Health: projection.Health, EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
			BaseCurrency: binding.riskBaseCurrency, BaseCurrencyProvenance: binding.riskBaseCurrencyProvenance,
		}, nil
	}
	before, err := exactRiskPositionQuantity(projection.Positions, draft.Contract)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: exact contract position evidence unavailable: %v", ErrTradingDisabled, err)
	}
	delta := float64(draft.Quantity)
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) {
		delta = -delta
	}
	return orderPositionAuthority{
		Impact: rpc.OrderPositionImpact{
			Before: before, After: before + delta,
			Effect: classifyPositionEffect(before, before+delta),
		},
		Generation: projection.Generation,
		Health:     projection.Health, EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
		BaseCurrency: binding.riskBaseCurrency, BaseCurrencyProvenance: binding.riskBaseCurrencyProvenance,
	}, nil
}

func verifyStrategyPositionProjection(positions []*ibkrlib.RawPosition, draft rpc.StrategyOrderDraft) error {
	if !draft.GuaranteedCombo || len(draft.Legs) < 2 {
		return fmt.Errorf("strategy is not submit-eligible")
	}
	seen := make(map[int]struct{}, len(draft.Legs))
	for _, leg := range draft.Legs {
		if _, exists := seen[leg.Contract.ConID]; exists {
			return fmt.Errorf("duplicate strategy ConID %d", leg.Contract.ConID)
		}
		seen[leg.Contract.ConID] = struct{}{}
		before, err := exactRiskPositionQuantity(positions, leg.Contract)
		if err != nil {
			return err
		}
		if math.Abs(before-leg.Before) > 1e-9 {
			return fmt.Errorf("ConID %d quantity is %.10g, preview expected %.10g", leg.Contract.ConID, before, leg.Before)
		}
	}
	return nil
}

// exactRiskPositionQuantity deliberately has no symbol/fallback matching.
// A broker-positive ConID is required to classify current effect truthfully,
// but position evidence alone never grants a close/reduce exemption. Same-
// zero IDs, duplicate rows, or conflicting secType/currency evidence fail
func exactRiskPositionQuantity(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) (float64, error) {
	evidence, err := exactRiskPositionEvidenceForContract(positions, contract)
	return evidence.Quantity, err
}

type exactRiskPositionEvidence struct {
	Quantity    float64
	AverageCost float64
	Multiplier  int
}

func exactRiskPositionEvidenceForContract(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) (exactRiskPositionEvidence, error) {
	if contract.ConID <= 0 {
		return exactRiskPositionEvidence{}, fmt.Errorf("contract ConID must be positive")
	}
	wantSecType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	wantCurrency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	if wantSecType == "" || wantCurrency == "" {
		return exactRiskPositionEvidence{}, fmt.Errorf("contract secType and currency are required")
	}
	wantSymbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	matched := false
	var evidence exactRiskPositionEvidence
	for _, position := range positions {
		if position == nil || position.Position == 0 {
			continue
		}
		posSecType := strings.ToUpper(strings.TrimSpace(position.Contract.SecType))
		posSymbol := strings.ToUpper(strings.TrimSpace(position.Contract.Symbol))
		if position.Contract.ConID <= 0 {
			if posSymbol != "" && posSymbol == wantSymbol && riskSecTypeConsistent(wantSecType, posSecType) {
				return exactRiskPositionEvidence{}, fmt.Errorf("same-symbol portfolio row has no positive ConID")
			}
			continue
		}
		if position.Contract.ConID != contract.ConID {
			continue
		}
		if !riskSecTypeConsistent(wantSecType, posSecType) {
			return exactRiskPositionEvidence{}, fmt.Errorf("ConID %d has conflicting secType %q", contract.ConID, position.Contract.SecType)
		}
		posCurrency := strings.ToUpper(strings.TrimSpace(position.Contract.Currency))
		if posCurrency == "" || posCurrency != wantCurrency {
			return exactRiskPositionEvidence{}, fmt.Errorf("ConID %d has conflicting currency %q", contract.ConID, position.Contract.Currency)
		}
		if wantSymbol != "" && posSymbol != "" && posSymbol != wantSymbol {
			return exactRiskPositionEvidence{}, fmt.Errorf("ConID %d has conflicting symbol %q", contract.ConID, position.Contract.Symbol)
		}
		if matched {
			return exactRiskPositionEvidence{}, fmt.Errorf("ConID %d appears in duplicate portfolio rows", contract.ConID)
		}
		matched = true
		if math.IsNaN(position.AverageCost) || math.IsInf(position.AverageCost, 0) {
			return exactRiskPositionEvidence{}, fmt.Errorf("ConID %d has non-finite average cost", contract.ConID)
		}
		evidence = exactRiskPositionEvidence{Quantity: position.Position, AverageCost: position.AverageCost, Multiplier: position.Contract.Multiplier}
	}
	if !matched {
		return exactRiskPositionEvidence{Quantity: 0}, nil
	}
	return evidence, nil
}

func riskSecTypeConsistent(want, got string) bool {
	want = strings.ToUpper(strings.TrimSpace(want))
	got = strings.ToUpper(strings.TrimSpace(got))
	if want == got {
		return true
	}
	// TWS reports exchange-traded funds as STK contracts even when the public
	return (want == "ETF" && got == "STK") || (want == "STK" && got == "ETF")
}

func (s *Server) captureOrderNotionalAuthority(ctx context.Context, authority *orderPreviewBrokerAuthority, quoteNotional float64, contractCurrency, baseCurrency string, timeout time.Duration) (orderNotionalAuthority, error) {
	contractCurrency = strings.ToUpper(strings.TrimSpace(contractCurrency))
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	if !positiveFinite(quoteNotional) || contractCurrency == "" || baseCurrency == "" {
		return orderNotionalAuthority{}, fmt.Errorf("%w: complete order notional currency evidence is unavailable", ErrTradingDisabled)
	}
	now := s.orderNow()
	if contractCurrency == baseCurrency {
		return orderNotionalAuthority{
			QuoteNotional: quoteNotional, ContractCurrency: contractCurrency,
			BaseNotional: quoteNotional, BaseCurrency: baseCurrency, BasePerContract: 1,
			EvidenceAt: now, Source: orderFXSourceIdentity,
		}, nil
	}

	budget := min(timeout, orderFXQuoteBudget)
	if budget <= 0 {
		budget = orderFXQuoteBudget
	}
	if s.orderFXRateForTest != nil {
		rate, at, err := s.orderFXRateForTest(ctx, baseCurrency, contractCurrency, budget)
		if err != nil {
			return orderNotionalAuthority{}, fmt.Errorf("%w: current typed FX evidence unavailable: %v", ErrTradingDisabled, err)
		}
		return newCrossCurrencyOrderNotionalAuthority(quoteNotional, contractCurrency, baseCurrency, rate, at, rpc.MarketDataLive)
	}
	if authority == nil || authority.connector == nil || !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return orderNotionalAuthority{}, fmt.Errorf("%w: current typed FX evidence unavailable for %s/%s", ErrTradingDisabled, baseCurrency, contractCurrency)
	}

	fxContract, inverted, err := canonicalOrderFXContract(contractCurrency, baseCurrency)
	if err != nil {
		return orderNotionalAuthority{}, fmt.Errorf("%w: current typed FX evidence unavailable for %s/%s: %v", ErrTradingDisabled, baseCurrency, contractCurrency, err)
	}
	quote, err := s.previewExactSessionFXQuote(ctx, authority, fxContract, budget)
	if err != nil {
		return orderNotionalAuthority{}, fmt.Errorf("%w: current exact-session FX quote unavailable for %s/%s: %v", ErrTradingDisabled, baseCurrency, contractCurrency, err)
	}
	rate := conservativeOrderFXRate(quote, inverted)
	result, err := newCrossCurrencyOrderNotionalAuthority(quoteNotional, contractCurrency, baseCurrency, rate, quote.AsOf, quote.DataType)
	if err != nil {
		return orderNotionalAuthority{}, err
	}
	if !s.orderPreviewBrokerAuthorityCurrent(authority) {
		return orderNotionalAuthority{}, brokerWriteTransactionDriftError()
	}
	return result, nil
}

func canonicalOrderFXContract(contractCurrency, baseCurrency string) (rpc.ContractParams, bool, error) {
	contractCurrency = strings.ToUpper(strings.TrimSpace(contractCurrency))
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	contractRank, contractOK := orderFXBasePriority[contractCurrency]
	baseRank, baseOK := orderFXBasePriority[baseCurrency]
	if !contractOK || !baseOK || contractCurrency == baseCurrency {
		return rpc.ContractParams{}, false, fmt.Errorf("unsupported major-currency pair")
	}
	inverted := contractRank > baseRank
	symbol, currency := contractCurrency, baseCurrency
	if inverted {
		symbol, currency = baseCurrency, contractCurrency
	}
	return rpc.ContractParams{
		Symbol: symbol, SecType: "CASH", Exchange: "IDEALPRO",
		PrimaryExch: "IDEALPRO", Currency: currency,
	}, inverted, nil
}

func newCrossCurrencyOrderNotionalAuthority(quoteNotional float64, contractCurrency, baseCurrency string, rate float64, at time.Time, dataType string) (orderNotionalAuthority, error) {
	if !positiveFinite(rate) || at.IsZero() || !validOrderFXDataType(dataType) {
		return orderNotionalAuthority{}, fmt.Errorf("%w: current typed FX evidence unavailable for %s/%s", ErrTradingDisabled, baseCurrency, contractCurrency)
	}
	return orderNotionalAuthority{
		QuoteNotional: quoteNotional, ContractCurrency: contractCurrency,
		BaseNotional: quoteNotional * rate, BaseCurrency: baseCurrency, BasePerContract: rate,
		EvidenceAt: at.UTC(), DataType: dataType, Source: orderFXSourceExactSessionQuote,
	}, nil
}

func validOrderFXDataType(dataType string) bool {
	switch dataType {
	case rpc.MarketDataLive, rpc.MarketDataFrozen, rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen:
		return true
	default:
		return false
	}
}

// conservativeOrderFXRate converts one unit of contract currency into account
func conservativeOrderFXRate(quote rpc.OrderQuoteSnapshot, inverted bool) float64 {
	if inverted {
		if quote.Bid == nil || !positiveFinite(*quote.Bid) {
			return 0
		}
		return 1 / *quote.Bid
	}
	if quote.Ask == nil || !positiveFinite(*quote.Ask) {
		return 0
	}
	return *quote.Ask
}

// validateOrderRiskAuthority applies the order limits in force (risk-policy
// [order_limits]) to one order. exit is the broker open-order evidence for
// the protective stock exit exemption (protective_exit.go); delta is the
// underlying's delta measurement for the delta-reducing exit exemption
// (delta_reduction.go). Either zero value never exempts. Limits that are not
// complete refuse every order.
func validateOrderRiskAuthority(limits risk.OrderLimitsInForce, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional orderNotionalAuthority, baseCurrency string, exit protectiveExitInventory, delta deltaReductionEvidence) error {
	if !limits.Complete {
		return orderLimitsIncompleteError(limits)
	}
	contractCurrency := strings.ToUpper(strings.TrimSpace(draft.Contract.Currency))
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	if contractCurrency == "" || baseCurrency == "" ||
		!strings.EqualFold(contractCurrency, notional.ContractCurrency) ||
		!strings.EqualFold(baseCurrency, notional.BaseCurrency) ||
		!positiveFinite(notional.QuoteNotional) || !positiveFinite(notional.BaseNotional) ||
		!positiveFinite(notional.BasePerContract) || notional.EvidenceAt.IsZero() {
		return fmt.Errorf("risk-increasing order currency %q cannot be compared with the account-base order cap currency %q without current typed FX evidence", contractCurrency, baseCurrency)
	}
	if limits.BaseCurrency != "" && !strings.EqualFold(limits.BaseCurrency, baseCurrency) {
		return fmt.Errorf("the order cap in force is stated in %s but the order's account base currency is %s", limits.BaseCurrency, baseCurrency)
	}
	if contractCurrency == baseCurrency {
		if notional.Source != orderFXSourceIdentity || math.Abs(notional.BasePerContract-1) > 1e-12 || notional.DataType != "" {
			return fmt.Errorf("same-currency order notional lacks identity FX evidence")
		}
	} else if notional.Source != orderFXSourceExactSessionQuote || !validOrderFXDataType(notional.DataType) {
		return fmt.Errorf("cross-currency order notional lacks exact-session FX evidence")
	}
	wantBase := notional.QuoteNotional * notional.BasePerContract
	if math.Abs(wantBase-notional.BaseNotional) > max(1e-8, math.Abs(wantBase)*1e-9) {
		return fmt.Errorf("order base notional does not match typed FX evidence")
	}
	// A close or reduction that lowers the absolute net delta of its
	// underlying passes the option-contract cap and the notional cap (owner
	// decision 2026-10-07 08:13 CEST; the rule, its measurement and what
	// still bounds it: delta_reduction.go). An unknown or stale delta never
	// exempts, nor does an order that with the other working orders in its
	// direction would exceed the held line, and the refusal then says why.
	deltaExempt, deltaWhy := deltaReducingExit(draft, position, delta, exit)
	if strings.EqualFold(draft.Contract.SecType, "OPT") && draft.Quantity > limits.MaxOptionContracts && !deltaExempt {
		return fmt.Errorf("option quantity %d exceeds the option cap in force of %d contracts ([order_limits].max_option_contracts)%s", draft.Quantity, limits.MaxOptionContracts, deltaWhy)
	}
	// A protective stock exit that sells at most the long position, with no
	// competing working sell, passes both the notional cap and the
	// apparent-exit short re-read (owner decision 2026-10-05 17:36 CEST).
	protectiveExit := protectiveStockExitExempt(draft, position, exit)
	// A same-currency cash sweep bill order passes the notional cap only up
	// to the sweep's own cap in force, when the protection policy writes
	// bills_exempt_from_trading_max_notional (owner decision 2026-10-05
	// 18:35 CEST); every other order keeps the order cap in force.
	sweepBill := cashSweepTradingCapExempt(draft, position, notional)
	if notional.BaseNotional > limits.CapBase && !protectiveExit && !sweepBill && !deltaExempt {
		return fmt.Errorf("order notional %s exceeds the order cap in force %s%s", risk.FormatOrderMoney(notional.BaseNotional, baseCurrency), limits.Summary, deltaWhy)
	}
	if draft.StrategyGroup != nil {
		if err := validateStrategyReductionDraft(draft, position, limits.MaxOptionContracts, deltaExempt); err != nil {
			return err
		}
		return nil
	}
	if ibkrlib.IsBillOrBond(draft.Contract.SecType) && stockShortOrFlip(position.Effect) {
		// A bond sale only ever reduces or closes a held line.
		return fmt.Errorf("a bond order that opens or flips a short position is not supported")
	}
	if bondSaleCandidate(draft, position) {
		// Two sales of the same held face, each judged against the unfilled
		// position, would together sell short. The complete open-order list
		// must show room for this sale beside every other working one.
		switch {
		case !exit.Current:
			return fmt.Errorf("a bond sale needs the broker's complete, current open-order list to show no other working sale already sells the held face; preview again")
		case math.IsNaN(exit.OtherWorkingSameSide) || math.IsInf(exit.OtherWorkingSameSide, 0) || exit.OtherWorkingSameSide < 0 ||
			exit.OtherWorkingSameSide+float64(draft.Quantity) > position.Before+1e-9:
			return fmt.Errorf("this bond sale of %d units with %s already working to sell exceeds the %s held; cancel a working sale first",
				draft.Quantity, strconv.FormatFloat(exit.OtherWorkingSameSide, 'f', -1, 64), strconv.FormatFloat(position.Before, 'f', -1, 64))
		}
	}
	if ibkrlib.IsBillOrBond(draft.Contract.SecType) && strings.EqualFold(draft.Action, rpc.OrderActionBuy) {
		// A bond buy matures within [order_limits].max_bond_maturity_years
		// (owner decision 2026-10-06 20:17 CEST); a sale is never limited.
		if draft.Bond == nil {
			return fmt.Errorf("a bond buy carries no bond terms, so its maturity cannot be checked")
		}
		asOf := limits.AsOf
		if asOf.IsZero() {
			asOf = time.Now().UTC()
		}
		if err := limits.CheckBondMaturity(draft.Bond.Maturity, asOf); err != nil {
			return err
		}
	}
	riskEffect := position.Effect
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) && isRiskReducing(riskEffect) && !protectiveExit {
		// Incomplete manual-order visibility means the apparent long exit may
		// arrive after another sell consumed that capacity. Apply the same
		// explicit short-opening permission as a zero-position sell. The
		// protective exit is excused because the complete inventory shows no
		// competing sell now and the stop guard shrinks or cancels the stop
		// when a later sale reduces the position.
		riskEffect = rpc.OrderPositionEffectOpenShort
	}
	switch {
	case isStockLikeRiskSecType(draft.Contract.SecType) && stockShortOrFlip(riskEffect) && !limits.AllowStockShort:
		return fmt.Errorf("stock short/flip requires [order_limits].allow_stock_short = true")
	case strings.EqualFold(draft.Contract.SecType, "OPT") && optionSellToOpen(draft.Action, riskEffect) && !limits.AllowOptionSellToOpen:
		return fmt.Errorf("option sell-to-open requires [order_limits].allow_option_sell_to_open = true")
	}
	return nil
}

// validateStrategyReductionDraft holds a strategy close or reduction to its
// shape and each leg to the option-contract cap; capExempt lifts the per-leg
// cap for a delta-reducing exit (delta_reduction.go), nothing else.
func validateStrategyReductionDraft(draft rpc.OrderDraft, position rpc.OrderPositionImpact, maxOptionContracts int, capExempt bool) error {
	group := draft.StrategyGroup
	if group == nil || !group.GuaranteedCombo || len(group.Legs) < 2 || draft.Contract.SecType != "BAG" || draft.Action != rpc.OrderActionSell || draft.Quantity != group.Units {
		return fmt.Errorf("strategy order is not one guaranteed proportional combo")
	}
	if position.Before != float64(group.UnitsBefore) || position.After != float64(group.UnitsAfter) || group.UnitsAfter < 0 || group.UnitsAfter >= group.UnitsBefore {
		return fmt.Errorf("strategy order does not reduce the current group")
	}
	for _, leg := range group.Legs {
		wantQuantity := absOrderRatio(leg.Ratio) * group.Units
		if wantQuantity <= 0 || leg.Quantity != wantQuantity || (leg.Quantity > maxOptionContracts && !capExempt) || !strings.EqualFold(leg.Contract.SecType, "OPT") {
			return fmt.Errorf("strategy leg %d violates the proportional option quantity limit", leg.Contract.ConID)
		}
		if math.Abs(leg.After) > math.Abs(leg.Before)+1e-9 || (leg.Before != 0 && leg.After != 0 && math.Signbit(leg.After) != math.Signbit(leg.Before)) {
			return fmt.Errorf("strategy leg %d would not reduce its current position", leg.Contract.ConID)
		}
		expectedAfter := leg.Before
		if leg.Action == rpc.OrderActionSell {
			expectedAfter -= float64(leg.Quantity)
		} else if leg.Action == rpc.OrderActionBuy {
			expectedAfter += float64(leg.Quantity)
		} else {
			return fmt.Errorf("strategy leg %d has an invalid action", leg.Contract.ConID)
		}
		if math.Abs(expectedAfter-leg.After) > 1e-9 {
			return fmt.Errorf("strategy leg %d has inconsistent before/after quantities", leg.Contract.ConID)
		}
	}
	return nil
}

func isStockLikeRiskSecType(secType string) bool {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "STK", "STOCK", "ETF":
		return true
	default:
		return false
	}
}

func sameOrderPositionImpact(a, b rpc.OrderPositionImpact) bool {
	return a.Before == b.Before && a.After == b.After && a.Effect == b.Effect
}

// bindPreviewOrderRiskAuthority revalidates the signed preview against one
// exact broker session and the current controls before any broker ID is
// reserved or token is consumed. Drift always asks for a new preview/WhatIf;
// it never silently rewrites OpenClose or adapts the signed quantity.
func (s *Server) bindPreviewOrderRiskAuthority(ctx context.Context, binding *brokerWriteTransactionBinding, status rpc.TradingStatus, payload orderPreviewTokenPayload, draft rpc.OrderDraft) error {
	if binding == nil {
		return brokerWriteTransactionDriftError()
	}
	_, controlGeneration := s.effectiveTradingControlSnapshot()
	if controlGeneration != binding.tradingControlGeneration || controlGeneration != payload.TradingControlGeneration {
		return fmt.Errorf("%w: trading controls changed after preview; preview again", ErrTradingDisabled)
	}
	if draft.StrategyGroup != nil {
		if err := s.ensureNoOtherOpenStrategyOrder(status, draft.StrategyGroup.StrategyID, draft.OrderRef); err != nil {
			return err
		}
	}
	var current orderPositionAuthority
	var err error
	if binding.testOnly && s.orderRiskAuthorityForTest == nil && s.orderPreviewPositionImpact == nil {
		current = orderPositionAuthority{
			Impact: payload.Position, Generation: payload.PortfolioGeneration,
			Health:       ibkrlib.PortfolioStreamHealth{Account: payload.PortfolioAccount, ProjectionGeneration: payload.PortfolioGeneration},
			EvidenceAt:   payload.PortfolioEvidenceAt,
			BaseCurrency: payload.BaseCurrency, TestOnly: true,
			BaseCurrencyProvenance: payload.BaseCurrencyProvenance,
		}
		if current.Generation == 0 {
			current.Generation = 1
			current.Health.ProjectionGeneration = 1
		}
		if current.Health.Account == "" {
			current.Health.Account = status.Account
		}
		if current.BaseCurrency == "" {
			current.BaseCurrency = strings.ToUpper(strings.TrimSpace(draft.Contract.Currency))
			current.BaseCurrencyProvenance = ibkrlib.AccountBaseCurrencyExplicitTag
		}
	} else if draft.StrategyGroup != nil {
		current, err = s.captureBoundStrategyPositionAuthority(ctx, binding.connector, binding.session, status, *draft.StrategyGroup)
		if err != nil {
			return err
		}
	} else {
		current, err = s.captureBoundOrderPositionAuthority(ctx, binding.connector, binding.session, status, draft.Contract, draft.Action, draft.Quantity)
		if err != nil {
			return err
		}
	}
	expectedGeneration := payload.PortfolioGeneration
	expectedAccount := payload.PortfolioAccount
	expectedBase := payload.BaseCurrency
	expectedBaseProvenance := payload.BaseCurrencyProvenance
	expectedImpact := payload.Position
	// Focused test fixtures that mint payloads directly predate the v4
	// authority fields. Production v4 tokens are minted only by previewOrder
	// and always carry all three; this compatibility branch is reachable only
	// through an explicit in-process position seam.
	if current.TestOnly && expectedGeneration == 0 && expectedAccount == "" && expectedBase == "" {
		expectedGeneration = current.Generation
		expectedAccount = current.Health.Account
		expectedBase = current.BaseCurrency
		expectedBaseProvenance = current.BaseCurrencyProvenance
		expectedImpact = current.Impact
	}
	if current.Generation != expectedGeneration ||
		!strings.EqualFold(strings.TrimSpace(current.Health.Account), strings.TrimSpace(expectedAccount)) ||
		!sameOrderPositionImpact(current.Impact, expectedImpact) ||
		!strings.EqualFold(strings.TrimSpace(current.BaseCurrency), strings.TrimSpace(expectedBase)) ||
		current.BaseCurrencyProvenance != expectedBaseProvenance {
		return fmt.Errorf("%w: portfolio risk authority changed after preview; preview again", ErrTradingDisabled)
	}
	if payload.OptionExitEconomics != nil {
		if binding.connector == nil {
			return brokerWriteTransactionDriftError()
		}
		projection, ok := binding.connector.CapturePortfolioProjectionForSession(binding.session)
		if !ok || projection.Generation != current.Generation {
			return brokerWriteTransactionDriftError()
		}
		terminal := s.optionExitTerminalEvidence(projection.Positions, s.orderNow())
		if optionExitEvidenceHash(terminal) != payload.OptionExitEconomics.TerminalFingerprint {
			return fmt.Errorf("%w: terminal exposure authority changed", ErrTradingDisabled)
		}
		scope := optionExitBookScope{Scope: binding.scope,
			Session:    fmt.Sprintf("%p/%d/%v", binding.connector, binding.connectorEpoch, binding.session),
			Generation: current.Generation, BaseCurrency: current.BaseCurrency, Terminal: terminal}
		if err := validateOptionExitTokenEvidence(payload.OptionExitEconomics, optionExitScopeHash(scope), current.Generation, s.orderNow()); err != nil {
			return err
		}
		binding.optionExitExpiresAt = payload.OptionExitEconomics.AsOf.Add(optionExitEvidenceBudget)
		binding.optionExitTerminalFingerprint = payload.OptionExitEconomics.TerminalFingerprint
	}
	signedNotional := payload.NotionalAuthority
	if current.TestOnly && signedNotional.QuoteNotional == 0 {
		signedNotional = orderNotionalAuthority{
			QuoteNotional: payload.Notional, ContractCurrency: draft.Contract.Currency,
			BaseNotional: payload.Notional, BaseCurrency: current.BaseCurrency, BasePerContract: 1,
			EvidenceAt: s.orderNow(), Source: orderFXSourceIdentity,
		}
	}
	// The exemptions read their evidence again at admission: a hand order
	// entered after the preview withdraws the protective exit or the
	// delta-reducing exit, and the underlying's delta is measured from the
	// positions as they are now. A modify excludes its own target order.
	// The delta is read against the larger of the signed and the current
	// notional, so a cap that binds only after FX drift still finds its
	// measurement.
	exitInventory := s.captureProtectiveExitInventory(ctx, status, draft, current.Impact, payload.Replace)
	limits := s.orderLimitsInForce(current.BaseCurrency)
	var fxAuthority *orderPreviewBrokerAuthority
	if !binding.testOnly {
		fxAuthority = &orderPreviewBrokerAuthority{
			connector: binding.connector, connectorEpoch: binding.connectorEpoch, session: binding.session,
		}
	}
	currentNotional, err := s.captureOrderNotionalAuthority(ctx, fxAuthority, signedNotional.QuoteNotional, draft.Contract.Currency, current.BaseCurrency, orderFXQuoteBudget)
	if err != nil {
		return err
	}
	bindNotional := signedNotional
	if currentNotional.BaseNotional > signedNotional.BaseNotional {
		bindNotional = currentNotional
	}
	deltaEvidence := s.captureDeltaReductionEvidence(ctx, status, draft, current.Impact, limits, bindNotional)
	if err := validateOrderRiskAuthority(limits, draft, current.Impact, signedNotional, current.BaseCurrency, exitInventory, deltaEvidence); err != nil {
		return fmt.Errorf("%w: signed preview risk authority is invalid: %v", ErrTradingDisabled, err)
	}
	if err := validateOrderRiskAuthority(limits, draft, current.Impact, currentNotional, current.BaseCurrency, exitInventory, deltaEvidence); err != nil {
		return fmt.Errorf("%w: current trading controls reject the order: %v", ErrTradingDisabled, err)
	}
	binding.riskBound = true
	binding.riskDraft = draft
	binding.riskPosition = current.Impact
	binding.riskPortfolioGeneration = current.Generation
	binding.riskPortfolioAccount = current.Health.Account
	binding.riskBaseCurrency = current.BaseCurrency
	binding.riskBaseCurrencyProvenance = current.BaseCurrencyProvenance
	binding.riskNotional = currentNotional
	binding.riskProtectiveExit = exitInventory
	binding.riskDeltaReduction = deltaEvidence
	return nil
}

func (s *Server) ensureNoOtherOpenStrategyOrder(status rpc.TradingStatus, strategyID, currentOrderRef string) error {
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return fmt.Errorf("%w: current order history is unavailable: %v", ErrTradingDisabled, err)
	}
	strategyByRef := make(map[string]string)
	strategyByOrderID := make(map[int]string)
	for _, event := range events {
		if event.StrategyGroup == nil || event.StrategyGroup.StrategyID == "" {
			continue
		}
		if event.OrderRef != "" {
			strategyByRef[event.OrderRef] = event.StrategyGroup.StrategyID
		}
		if event.ReservedOrderID > 0 {
			strategyByOrderID[event.ReservedOrderID] = event.StrategyGroup.StrategyID
		}
	}
	for _, view := range buildOrderViews(events) {
		if !view.Open || !strings.EqualFold(strings.TrimSpace(view.Account), strings.TrimSpace(status.Account)) || !strings.EqualFold(strings.TrimSpace(view.Mode), strings.TrimSpace(status.Mode)) {
			continue
		}
		groupID := strategyByRef[view.OrderRef]
		if groupID == "" && view.ReservedOrderID > 0 {
			groupID = strategyByOrderID[view.ReservedOrderID]
		}
		if groupID == strategyID && view.OrderRef != currentOrderRef {
			return fmt.Errorf("%w: another order for this strategy is still open; wait for it to finish or cancel it before previewing again", ErrTradingDisabled)
		}
	}
	return nil
}
