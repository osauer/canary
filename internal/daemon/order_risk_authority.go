package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
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
		s.orderContractResolverForTest != nil || s.orderPreviewWhatIf != nil
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
	before, err := exactRiskPositionQuantity(projection.Positions, contract)
	if err != nil {
		return orderPositionAuthority{}, fmt.Errorf("%w: exact contract position evidence unavailable: %v", ErrTradingDisabled, err)
	}
	delta := float64(qty)
	if strings.EqualFold(action, rpc.OrderActionSell) {
		delta = -delta
	}
	authority := orderPositionAuthority{
		Impact:     rpc.OrderPositionImpact{Before: before, After: before + delta, Effect: classifyPositionEffect(before, before+delta)},
		Generation: projection.Generation, Health: projection.Health,
		EvidenceAt: portfolioStreamEvidenceAsOf(projection.Health),
	}
	// A position-only close/reduce classification does not grant a risk
	// exemption. reqAllOpenOrders cannot prove future manual-TWS activity for
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

// exactRiskPositionQuantity deliberately has no symbol/fallback matching.
// A broker-positive ConID is required to classify current effect truthfully,
// but position evidence alone never grants a close/reduce exemption. Same-
// zero IDs, duplicate rows, or conflicting secType/currency evidence fail
func exactRiskPositionQuantity(positions []*ibkrlib.RawPosition, contract rpc.ContractParams) (float64, error) {
	if contract.ConID <= 0 {
		return 0, fmt.Errorf("contract ConID must be positive")
	}
	wantSecType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	wantCurrency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	if wantSecType == "" || wantCurrency == "" {
		return 0, fmt.Errorf("contract secType and currency are required")
	}
	wantSymbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	matched := false
	var quantity float64
	for _, position := range positions {
		if position == nil || position.Position == 0 {
			continue
		}
		posSecType := strings.ToUpper(strings.TrimSpace(position.Contract.SecType))
		posSymbol := strings.ToUpper(strings.TrimSpace(position.Contract.Symbol))
		if position.Contract.ConID <= 0 {
			if posSymbol != "" && posSymbol == wantSymbol && riskSecTypeConsistent(wantSecType, posSecType) {
				return 0, fmt.Errorf("same-symbol portfolio row has no positive ConID")
			}
			continue
		}
		if position.Contract.ConID != contract.ConID {
			continue
		}
		if !riskSecTypeConsistent(wantSecType, posSecType) {
			return 0, fmt.Errorf("ConID %d has conflicting secType %q", contract.ConID, position.Contract.SecType)
		}
		posCurrency := strings.ToUpper(strings.TrimSpace(position.Contract.Currency))
		if posCurrency == "" || posCurrency != wantCurrency {
			return 0, fmt.Errorf("ConID %d has conflicting currency %q", contract.ConID, position.Contract.Currency)
		}
		if wantSymbol != "" && posSymbol != "" && posSymbol != wantSymbol {
			return 0, fmt.Errorf("ConID %d has conflicting symbol %q", contract.ConID, position.Contract.Symbol)
		}
		if matched {
			return 0, fmt.Errorf("ConID %d appears in duplicate portfolio rows", contract.ConID)
		}
		matched = true
		quantity = position.Position
	}
	return quantity, nil
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

func validateOrderRiskAuthority(cfg config.Trading, draft rpc.OrderDraft, position rpc.OrderPositionImpact, notional orderNotionalAuthority, baseCurrency string) error {
	cfg = cfg.WithDefaults()
	contractCurrency := strings.ToUpper(strings.TrimSpace(draft.Contract.Currency))
	baseCurrency = strings.ToUpper(strings.TrimSpace(baseCurrency))
	if contractCurrency == "" || baseCurrency == "" ||
		!strings.EqualFold(contractCurrency, notional.ContractCurrency) ||
		!strings.EqualFold(baseCurrency, notional.BaseCurrency) ||
		!positiveFinite(notional.QuoteNotional) || !positiveFinite(notional.BaseNotional) ||
		!positiveFinite(notional.BasePerContract) || notional.EvidenceAt.IsZero() {
		return fmt.Errorf("risk-increasing order currency %q cannot be compared with account-base max_notional currency %q without current typed FX evidence", contractCurrency, baseCurrency)
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
	if strings.EqualFold(draft.Contract.SecType, "OPT") && draft.Quantity > cfg.MaxOptionContracts {
		return fmt.Errorf("option quantity %d exceeds [trading].max_option_contracts %d", draft.Quantity, cfg.MaxOptionContracts)
	}
	if notional.BaseNotional > cfg.MaxNotional {
		return fmt.Errorf("order notional %.2f %s exceeds [trading].max_notional %.2f %s", notional.BaseNotional, baseCurrency, cfg.MaxNotional, baseCurrency)
	}
	riskEffect := position.Effect
	if strings.EqualFold(draft.Action, rpc.OrderActionSell) && isRiskReducing(riskEffect) {
		// Incomplete manual-order visibility means the apparent long exit may
		// arrive after another sell consumed that capacity. Apply the same
		// explicit short-opening permission as a zero-position sell.
		riskEffect = rpc.OrderPositionEffectOpenShort
	}
	switch {
	case isStockLikeRiskSecType(draft.Contract.SecType) && stockShortOrFlip(riskEffect) && !cfg.AllowStockShort:
		return fmt.Errorf("stock short/flip requires [trading].allow_stock_short = true")
	case strings.EqualFold(draft.Contract.SecType, "OPT") && optionSellToOpen(draft.Action, riskEffect) && !cfg.AllowOptionSellToOpen:
		return fmt.Errorf("option sell-to-open requires [trading].allow_option_sell_to_open = true")
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
	cfg, controlGeneration := s.effectiveTradingControlSnapshot()
	if controlGeneration != binding.tradingControlGeneration || controlGeneration != payload.TradingControlGeneration {
		return fmt.Errorf("%w: trading controls changed after preview; preview again", ErrTradingDisabled)
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
	signedNotional := payload.NotionalAuthority
	if current.TestOnly && signedNotional.QuoteNotional == 0 {
		signedNotional = orderNotionalAuthority{
			QuoteNotional: payload.Notional, ContractCurrency: draft.Contract.Currency,
			BaseNotional: payload.Notional, BaseCurrency: current.BaseCurrency, BasePerContract: 1,
			EvidenceAt: s.orderNow(), Source: orderFXSourceIdentity,
		}
	}
	if err := validateOrderRiskAuthority(cfg, draft, current.Impact, signedNotional, current.BaseCurrency); err != nil {
		return fmt.Errorf("%w: signed preview risk authority is invalid: %v", ErrTradingDisabled, err)
	}
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
	if err := validateOrderRiskAuthority(cfg, draft, current.Impact, currentNotional, current.BaseCurrency); err != nil {
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
	return nil
}
