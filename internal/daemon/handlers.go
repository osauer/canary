package daemon

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	positionstrategy "github.com/osauer/canary/v2/internal/strategy"
)

// handleAccountSummary issues a one-shot reqAccountSummary and converts the
// result into the wire shape exposed to the CLI.
// dailyPnLStaleGrace is how old the account daily-P&L frame may be before
// handleAccountSummary asks the connector to self-heal the reqPnL stream.
const dailyPnLStaleGrace = 90 * time.Second

func (s *Server) handleAccountSummary(ctx context.Context) (*rpc.AccountResult, error) {
	return s.buildAccountSummary(ctx, true)
}

type accountSummaryAuthority struct {
	Provenance              ibkrlib.AccountSummaryProvenance
	AsOf                    time.Time
	NetLiquidationAvailable bool
	TotalCashAvailable      bool
	AvailableFundsAvailable bool
	BaseCurrencyAvailable   bool
}

// buildAccountSummary shares one builder across daemon compositions; observe
// controls capital-state observation without changing the typed result.
func (s *Server) buildAccountSummary(ctx context.Context, observe bool) (*rpc.AccountResult, error) {
	result, _, err := s.buildAccountSummaryWithAuthority(ctx, observe)
	return result, err
}

// buildAccountSummaryWithAuthority preserves display fallback while returning
// typed provenance. Cached fallback is never fresh Rulebook evidence.
func (s *Server) buildAccountSummaryWithAuthority(ctx context.Context, observe bool) (*rpc.AccountResult, accountSummaryAuthority, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, accountSummaryAuthority{}, s.gatewayUnavailableError()
	}
	scope := s.currentBrokerStateScope()
	if !brokerScopeConcrete(scope) {
		return nil, accountSummaryAuthority{}, errors.New("account summary requires one selected account; configure an account pin for a multi-account login")
	}
	snapshot, err := s.readAccountSnapshot(ctx, c)
	raw := snapshot.raw
	provenance := snapshot.provenance
	observedAt := snapshot.observedAt
	if err != nil {
		if cached := c.CachedAccountSummary(); cached != nil {
			s.logger.Debugf("account summary one-shot failed; using cached account update snapshot: %v", err)
			raw = cached
			provenance = ibkrlib.AccountSummaryProvenanceCachedFallback
			observedAt = time.Time{}
		} else {
			return nil, accountSummaryAuthority{}, err
		}
	}
	if raw == nil {
		return nil, accountSummaryAuthority{}, errors.New("account summary unavailable")
	}
	if !strings.EqualFold(strings.TrimSpace(raw.AccountID), strings.TrimSpace(scope.Account)) {
		return nil, accountSummaryAuthority{}, errors.New("account summary account scope conflict")
	}
	authority := accountSummaryAuthority{
		Provenance: provenance, AsOf: observedAt.UTC(),
		NetLiquidationAvailable: raw.NetLiquidation != nil,
		TotalCashAvailable:      raw.TotalCashValue != nil,
		AvailableFundsAvailable: raw.AvailableFunds != nil,
		BaseCurrencyAvailable:   raw.BaseCurrencyProvenance.Proven() && strings.TrimSpace(raw.BaseCurrency) != "",
	}
	baseCurrency := ""
	if authority.BaseCurrencyAvailable {
		baseCurrency = normCcy(raw.BaseCurrency)
	}
	res := &rpc.AccountResult{
		AccountID:    raw.AccountID,
		AccountType:  raw.AccountType,
		BaseCurrency: baseCurrency,
		AsOf:         accountResultAuthorityAsOf(authority, time.Now().UTC()),
	}
	if raw.NetLiquidation != nil {
		res.NetLiquidation = *raw.NetLiquidation
	}
	if raw.BuyingPower != nil {
		res.BuyingPower = *raw.BuyingPower
	}
	if raw.AvailableFunds != nil {
		res.AvailableFunds = *raw.AvailableFunds
	}
	if raw.ExcessLiquidity != nil {
		res.ExcessLiquidity = *raw.ExcessLiquidity
	}
	if raw.TotalCashValue != nil {
		res.TotalCash = *raw.TotalCashValue
	}
	if raw.MaintenanceMargin != nil {
		res.MaintenanceMargin = *raw.MaintenanceMargin
	}
	if raw.InitMarginReq != nil {
		res.InitialMargin = *raw.InitMarginReq
	}
	if raw.GrossPositionValue != nil {
		res.GrossPositionValue = *raw.GrossPositionValue
	}
	if raw.UnrealizedPnL != nil {
		res.UnrealizedPnL = *raw.UnrealizedPnL
	}
	if raw.RealizedPnL != nil {
		res.RealizedPnL = *raw.RealizedPnL
	}
	if raw.Cushion != nil {
		res.Cushion = *raw.Cushion
	}
	if raw.LookAheadInitMargin != nil {
		res.LookAheadInitMargin = *raw.LookAheadInitMargin
	}
	if raw.LookAheadMaintMargin != nil {
		res.LookAheadMaintMargin = *raw.LookAheadMaintMargin
	}
	if raw.LookAheadAvailable != nil {
		res.LookAheadAvailable = *raw.LookAheadAvailable
	}
	if raw.LookAheadExcess != nil {
		res.LookAheadExcess = *raw.LookAheadExcess
	}
	if res.BaseCurrency != "" {
		ledger := s.repairCurrencyLedgerFXRatesCached(ctx, c, raw.CurrencyLedger, res.BaseCurrency)
		res.CurrencyExposure = buildCurrencyExposure(ledger, res.BaseCurrency)
	}
	// Read the latest reqPnL frame; absence starts the idempotent subscription.
	// connect setup skips the subscribe in auto-detect mode (ep.Account is
	// empty until the gateway emits managedAccounts after handshake), so
	// the first `account` call doubles as the kickoff. SubscribeAccountPnL
	// is idempotent, so later reads reuse the same account subscription.
	snap, ok := c.AccountDailyPnL()
	if !ok {
		if account := scope.Account; brokerScopeAccountConcrete(account) {
			if err := c.SubscribeAccountPnL(account); err != nil {
				s.logger.Debugf("SubscribeAccountPnL(%s) failed: %v", account, err)
			}
			snap, ok = waitForAccountDailyPnL(ctx, c, time.Now().Add(750*time.Millisecond))
		}
	}
	if ok {
		res.DailyPnL = snap.DailyPnL
		res.PnLUnrealizedTotal = snap.UnrealizedTotalPnL
		res.PnLRealizedTotal = snap.RealizedTotalPnL
		// Self-heal reqPnL when account updates remain live but P&L goes stale.
		if !snap.AsOf.IsZero() && time.Since(snap.AsOf) >= dailyPnLStaleGrace {
			marketOpen := false
			if ses, sErr := marketcal.New().SessionAt(marketcal.MarketUSEquity, time.Now()); sErr == nil {
				marketOpen = ses.IsOpen
			}
			if c.MaybeResubscribeStaleDailyPnL(marketOpen) {
				if fresh, freshOK := waitForAccountDailyPnL(ctx, c, time.Now().Add(750*time.Millisecond)); freshOK {
					snap = fresh
					ok = true
					res.DailyPnL = fresh.DailyPnL
					res.PnLUnrealizedTotal = fresh.UnrealizedTotalPnL
					res.PnLRealizedTotal = fresh.RealizedTotalPnL
				}
			}
		}
	}
	pnlNow := s.nowUTC()
	pnlDue := true
	pnlSessionKey := nySessionKey(pnlNow)
	if session, sessionErr := marketcal.New().SessionAt(marketcal.MarketUSEquity, pnlNow); sessionErr == nil && session.State != marketcal.StateUnknown {
		pnlDue = session.IsOpen
		pnlSessionKey = session.Date
	}
	pnlSource := dailyPnLScopeSource(scope)
	pnlObservation, pnlObservationErr := s.dailyPnLObservations.observe(context.Background(), pnlSource, pnlSessionKey, pnlNow, pnlDue, snap, ok || !snap.AsOf.IsZero())
	if pnlObservationErr != nil {
		s.logger.Warnf("Daily P&L observation authority degraded: %v", pnlObservationErr)
	}
	res.DailyPnLObservation = &pnlObservation
	res.Authority = accountResultDataAuthority(scope, raw, provenance, res)
	// Successful account reads feed the cash-flow-adjusted capital state.
	if observe && s.riskCapital != nil && res.NetLiquidation > 0 {
		var pol *risk.Constitution
		if s.riskPolicies != nil {
			pol = s.riskPolicies.snapshot().policy
		}
		if pol == nil || pol.Capital.BaseCurrency == "" || res.BaseCurrency == "" ||
			strings.EqualFold(pol.Capital.BaseCurrency, res.BaseCurrency) {
			capitalScope := s.currentBrokerStateScope()
			if firstDailyObservation := s.riskCapital.Observe(res.NetLiquidation, res.AsOf, pol, capitalScope); firstDailyObservation {
				// Re-evaluate when today's first runtime account observation arrives.
				s.evaluateRiskPolicyV3Reconciliation()
			}
			// Account observation is the authority that can open the latch. A
			// bounded post-latch Flex check belongs here, not on a read-only
			// policy or Brief request. The fetch state suppresses repeats.
			if open, _, latchedAt := s.riskCapital.NudgeLatchForScope(capitalScope); open {
				s.maybeFetchFlexForLatch(context.Background(), latchedAt)
			}
		}
	}
	return res, authority, nil
}

// accountResultDataAuthority projects the daemon's internal account summary
// evidence onto the shared RPC truth contract. The legacy scalar values remain
// float64 for compatibility; Fields distinguishes missing from a real zero.
func accountResultDataAuthority(scope brokerStateScope, raw *ibkrlib.RawAccountSummary, provenance ibkrlib.AccountSummaryProvenance, res *rpc.AccountResult) *rpc.AccountDataAuthority {
	authority := &rpc.AccountDataAuthority{
		Scope:        accountDataScope(scope),
		Availability: rpc.AccountDataAvailable,
		Freshness:    rpc.AccountDataFreshnessUnknown,
		Fields:       &rpc.AccountFieldAvailability{},
	}
	if raw == nil || res == nil {
		authority.Availability = rpc.AccountDataUnavailable
		authority.Reason = rpc.AccountDataReasonInvalidPayload
		return authority
	}
	authority.Fields = &rpc.AccountFieldAvailability{
		AccountType:          strings.TrimSpace(raw.AccountType) != "",
		BaseCurrency:         raw.BaseCurrencyProvenance.Proven() && strings.TrimSpace(raw.BaseCurrency) != "",
		NetLiquidation:       raw.NetLiquidation != nil,
		BuyingPower:          raw.BuyingPower != nil,
		AvailableFunds:       raw.AvailableFunds != nil,
		ExcessLiquidity:      raw.ExcessLiquidity != nil,
		TotalCash:            raw.TotalCashValue != nil,
		MaintenanceMargin:    raw.MaintenanceMargin != nil,
		InitialMargin:        raw.InitMarginReq != nil,
		GrossPositionValue:   raw.GrossPositionValue != nil,
		UnrealizedPnL:        raw.UnrealizedPnL != nil,
		RealizedPnL:          raw.RealizedPnL != nil,
		Cushion:              raw.Cushion != nil,
		LookAheadInitMargin:  raw.LookAheadInitMargin != nil,
		LookAheadMaintMargin: raw.LookAheadMaintMargin != nil,
		LookAheadAvailable:   raw.LookAheadAvailable != nil,
		LookAheadExcess:      raw.LookAheadExcess != nil,
		DailyPnL:             res.DailyPnL != nil,
		PnLUnrealizedTotal:   res.PnLUnrealizedTotal != nil,
		PnLRealizedTotal:     res.PnLRealizedTotal != nil,
		CurrencyExposure:     provenance == ibkrlib.AccountSummaryProvenanceRequest && res.BaseCurrency != "",
	}
	switch provenance {
	case ibkrlib.AccountSummaryProvenanceRequest:
		authority.Source = rpc.AccountDataSourceAccountSummaryRequest
		authority.AsOf = res.AsOf.UTC()
		if res.AsOf.IsZero() {
			authority.Availability = rpc.AccountDataUnavailable
			authority.Reason = rpc.AccountDataReasonClockInvalid
		} else {
			authority.Freshness = rpc.AccountDataFreshnessCurrent
		}
	case ibkrlib.AccountSummaryProvenanceCachedFallback:
		authority.Source = rpc.AccountDataSourceAccountUpdatesCache
		authority.Reason = rpc.AccountDataReasonUnstampedCache
	default:
		authority.Availability = rpc.AccountDataUnavailable
		authority.Reason = rpc.AccountDataReasonInvalidPayload
	}
	return authority
}

// accountResultAuthorityAsOf keeps display context separate from decision
// authority. Cached account-update rows are reparsed and stamped at read time,
// so only a completed one-shot request supplies an authoritative observation.
func accountResultAuthorityAsOf(authority accountSummaryAuthority, completedAt time.Time) time.Time {
	asOf := authority.AsOf.UTC()
	if authority.Provenance != ibkrlib.AccountSummaryProvenanceRequest ||
		asOf.IsZero() || asOf.After(completedAt.UTC()) {
		return time.Time{}
	}
	return asOf
}

type accountDailyPnLReader interface {
	AccountDailyPnL() (ibkrlib.AccountDailyPnL, bool)
}

func waitForAccountDailyPnL(ctx context.Context, reader accountDailyPnLReader, deadline time.Time) (ibkrlib.AccountDailyPnL, bool) {
	if reader == nil {
		return ibkrlib.AccountDailyPnL{}, false
	}
	var snap ibkrlib.AccountDailyPnL
	var ok bool
	_ = pollUntil(ctx, deadline, func() bool {
		snap, ok = reader.AccountDailyPnL()
		return ok && snap.DailyPnL != nil
	})
	return snap, ok && snap.DailyPnL != nil
}

// buildCurrencyExposure flattens RawAccountSummary.CurrencyLedger into the
// wire-shape CurrencyExposure rows, sorted by currency for stable output.
// It drops the base row and invalid exchange rates; when base is unknown, a
// rate of exactly one is conservatively treated as the likely base row.
func buildCurrencyExposure(ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) []rpc.CurrencyExposure {
	if len(ledger) == 0 {
		return nil
	}
	baseCcy = normCcy(baseCcy)
	out := make([]rpc.CurrencyExposure, 0, len(ledger))
	for ccy, row := range ledger {
		upper := normCcy(ccy)
		if upper == baseCcy {
			continue
		}
		if row.ExchangeRate <= 0 {
			continue
		}
		// ExchangeRate==1 is the conservative fallback when base is unknown.
		if baseCcy == "" && row.ExchangeRate == 1.0 {
			continue
		}
		nlBase := row.NetLiquidationByCurrency * row.ExchangeRate
		out = append(out, rpc.CurrencyExposure{
			Currency:             upper,
			NetLiquidationCcy:    row.NetLiquidationByCurrency,
			CashCcy:              row.CashBalance,
			StockMarketValueCcy:  row.StockMarketValue,
			OptionMarketValueCcy: row.OptionMarketValue,
			UnrealizedPnLCcy:     row.UnrealizedPnL,
			RealizedPnLCcy:       row.RealizedPnL,
			ExchangeRate:         row.ExchangeRate,
			NetLiquidationBase:   nlBase,
		})
	}
	slices.SortStableFunc(out, func(a, b rpc.CurrencyExposure) int { return cmp.Compare(a.Currency, b.Currency) })
	return out
}

// handlePositionsList fetches positions and applies the optional symbol/type filter.
func (s *Server) handlePositionsList(ctx context.Context, req *rpc.Request) (*rpc.PositionsResult, error) {
	return s.handlePositionsListCaptured(ctx, req, nil)
}

func (s *Server) handlePositionsListCaptured(ctx context.Context, req *rpc.Request, portfolioHealth *ibkrlib.PortfolioStreamHealth) (*rpc.PositionsResult, error) {
	return s.handlePositionsListCapturedForScope(ctx, req, portfolioHealth, s.currentBrokerStateScope())
}

func (s *Server) handlePositionsListCapturedForScope(ctx context.Context, req *rpc.Request, portfolioHealth *ibkrlib.PortfolioStreamHealth, expectedScope brokerStateScope) (*rpc.PositionsResult, error) {
	var p rpc.PositionsListParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	session, sessionOK := c.CaptureSession()
	var positions []*ibkrlib.RawPosition
	positions, health, err := c.CachedPositionsWithHealth()
	if err != nil {
		return nil, err
	}
	if !brokerScopeConcrete(expectedScope) {
		if portfolioHealth != nil {
			*portfolioHealth = health
		}
		return newPositionsResult(expectedScope, health, time.Now().UTC()), nil
	}
	if brokerScopeConcrete(expectedScope) {
		var scoped bool
		health, scoped = scopedPortfolioStreamHealth(positions, health, expectedScope, time.Now())
		if !scoped {
			if portfolioHealth != nil {
				*portfolioHealth = health
			}
			return newPositionsResult(expectedScope, health, time.Now().UTC()), nil
		}
	}
	if portfolioHealth != nil {
		*portfolioHealth = health
	}
	res := newPositionsResult(expectedScope, health, time.Now().UTC())
	wantSym := normSym(p.Symbol)
	wantType := strings.ToLower(strings.TrimSpace(p.Type))

	// Keep ConIDs beside the display DTO so daily-P&L lookup stays exact.
	conIDByPositionKey := map[string]int{}

	for _, pos := range positions {
		if pos == nil {
			continue
		}
		isOpt := pos.Contract.SecType == "OPT"
		if wantType == "stk" && isOpt {
			continue
		}
		if wantType == "opt" && !isOpt {
			continue
		}
		sym := pos.Contract.Symbol
		if wantSym != "" && wantSym != strings.ToUpper(sym) {
			continue
		}
		multiplier := positionViewMultiplier(pos.Contract.SecType, pos.Contract.Multiplier)
		view := rpc.PositionView{
			Symbol:        sym,
			SecType:       positionSecType(pos.Contract.SecType),
			ConID:         pos.Contract.ConID,
			Exchange:      pos.Contract.Exchange,
			Currency:      pos.Contract.Currency,
			LocalSymbol:   pos.Contract.LocalSymbol,
			TradingClass:  pos.Contract.TradingClass,
			Quantity:      pos.Position,
			Multiplier:    multiplier,
			AvgCost:       pos.AverageCost,
			Mark:          pos.MarketPrice,
			ValuationMark: pos.MarketPrice,
			// Use broker market value; price × quantity is wrong for bonds.
			MarketValue:   pos.MarketValue,
			UnrealizedPnL: pos.UnrealizedPNL,
			RealizedPnL:   pos.RealizedPNL,
		}
		if isOpt {
			view.Expiry = pos.Contract.Expiry
			view.Right = pos.Contract.Right
			view.Strike = pos.Contract.Strike
			res.Options = append(res.Options, view)
		} else {
			res.Stocks = append(res.Stocks, view)
		}
		if pos.Contract.ConID > 0 {
			conIDByPositionKey[positionViewKey(view)] = pos.Contract.ConID
		}
	}
	slices.SortStableFunc(res.Stocks, func(a, b rpc.PositionView) int { return cmp.Compare(a.Symbol, b.Symbol) })
	slices.SortStableFunc(res.Options, func(a, b rpc.PositionView) int {
		if c := cmp.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Expiry, b.Expiry); c != 0 {
			return c
		}
		return cmp.Compare(a.Strike, b.Strike)
	})

	// Pre-warm held-stock quotes before deriving daily change.
	s.prewarmStockQuoteSummaries(ctx, c, res.Stocks)
	flagZeroValueStockPositions(res.Stocks)
	s.fillDailyChange(res.Stocks)
	// Options reuse their underlying's cached previous close as context.
	s.fillOptionUnderlyingPrevClose(res.Options)
	// Briefly subscribe to each option leg to harvest model Greeks.
	s.prewarmOptionGreeks(ctx, c, res.Options)
	s.fillOptionGreeks(c, res.Options)
	// Compute option day change after previous-close enrichment.
	fillOptionDayChangeMoney(res.Options)

	// FX/base-currency decoration: prefer the per-currency snapshot
	// maintained by the daemon's reqAccountUpdates subscription. If that
	// startup cache lacks account-base, NLV, or a held non-base currency,
	// Use one bounded account refresh before filling incomplete FX rates.
	baseCcy := ""
	var netLiquidationBase *float64
	ledger := c.CurrencyLedgerSnapshot()
	if cachedAccount := c.CachedAccountSummary(); cachedAccount != nil &&
		strings.EqualFold(strings.TrimSpace(cachedAccount.AccountID), strings.TrimSpace(expectedScope.Account)) {
		if cachedAccount.BaseCurrencyProvenance.Proven() {
			baseCcy = normCcy(cachedAccount.BaseCurrency)
		}
		if baseCcy != "" {
			netLiquidationBase = cachedAccount.NetLiquidation
			ledger = mergeCurrencyLedgers(cachedAccount.CurrencyLedger, ledger)
		}
	}
	ledger = s.repairCurrencyLedgerFXRatesCached(ctx, c, ledger, baseCcy)
	missing := missingPositionFXCurrencies(res.Stocks, res.Options, ledger, baseCcy)
	if baseCcy == "" || netLiquidationBase == nil || len(missing) > 0 {
		// Reuse the shared account-snapshot flight across position polls.
		refreshCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		snapshot, err := s.readAccountSnapshot(refreshCtx, c)
		cancel()
		if raw := snapshot.raw; err == nil && raw != nil {
			if baseCcy == "" && raw.BaseCurrencyProvenance.Proven() {
				baseCcy = normCcy(raw.BaseCurrency)
			}
			if netLiquidationBase == nil && baseCcy != "" {
				netLiquidationBase = raw.NetLiquidation
			}
			if baseCcy != "" {
				freshLedger := s.repairCurrencyLedgerFXRatesCached(ctx, c, raw.CurrencyLedger, baseCcy)
				ledger = mergeCurrencyLedgers(freshLedger, ledger)
			}
		} else {
			s.logger.Debugf("positions FX ledger refresh failed for %v: %v", missing, err)
		}
	}
	fillFXRates(res.Stocks, ledger, baseCcy)
	fillFXRates(res.Options, ledger, baseCcy)

	// Daily P&L: kick off reqPnLSingle subscriptions (idempotent — the
	// "DBL_MAX sentinel" — never zero-substituted.
	s.fillDailyPnL(c, res.Stocks, conIDByPositionKey, expectedScope.Account)
	s.fillDailyPnL(c, res.Options, conIDByPositionKey, expectedScope.Account)
	fillBaseValues(res.Stocks, baseCcy)
	fillBaseValues(res.Options, baseCcy)
	flagClosedOptionSession(res.Options, res.AsOf)
	flagOptionMarkOutsideBidAsk(res.Options)

	res.ByUnderlying = groupByUnderlying(res.Stocks, res.Options, baseCcy, netLiquidationBase)
	res.Strategies, res.StrategyIssues = positionstrategy.InferPositionStrategies(res.Options)
	res.Strategies, res.StrategyIssues = s.reconcileJournaledStrategyGroups(res.Options, res.Strategies, res.StrategyIssues)
	res.Portfolio = buildPortfolioAggregatesWithBase(res.Stocks, res.Options, baseCcy)
	addPortfolioBaseContext(res.Portfolio, res.ByUnderlying, baseCcy, netLiquidationBase)
	addFXSensitivity(res.Portfolio, ledger, baseCcy)
	s.attachProtectionCoverage(ctx, res, wantSym, wantType, health)
	completedAt := time.Now().UTC()
	res.Authority = positionsResultDataAuthority(expectedScope, health, completedAt)
	authorityScope := expectedScope
	if !sessionOK || !c.SessionCurrent(session) || !sameBrokerScope(expectedScope, s.currentBrokerStateScope()) {
		authorityScope = brokerStateScope{}
		res.Authority.Availability = rpc.AccountDataUnavailable
		res.Authority.Freshness = rpc.AccountDataFreshnessUnknown
		res.Authority.Reason = rpc.AccountDataReasonSessionChanged
	}
	res.AsOf = positionsResultAuthorityAsOf(authorityScope, health, completedAt)
	return res, nil
}

func newPositionsResult(scope brokerStateScope, health ibkrlib.PortfolioStreamHealth, completedAt time.Time) *rpc.PositionsResult {
	result := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{},
		Authority: positionsResultDataAuthority(scope, health, completedAt.UTC()),
	}
	if brokerScopeAccountConcrete(scope.Account) {
		result.AccountID = strings.TrimSpace(scope.Account)
	}
	result.AsOf = positionsResultAuthorityAsOf(scope, health, completedAt.UTC())
	return result
}

func positionsResultDataAuthority(scope brokerStateScope, health ibkrlib.PortfolioStreamHealth, completedAt time.Time) *rpc.AccountDataAuthority {
	status, arm := classifyPortfolioStreamHealthArm(scope, health, completedAt.UTC())
	authority := &rpc.AccountDataAuthority{
		Scope:  accountDataScope(scope),
		Source: rpc.AccountDataSourcePortfolioStream,
		AsOf:   portfolioStreamEvidenceAsOf(health),
	}
	switch status {
	case orderIntegrityHealthCurrent:
		authority.Availability = rpc.AccountDataAvailable
		authority.Freshness = rpc.AccountDataFreshnessCurrent
	case orderIntegrityHealthStale:
		authority.Availability = rpc.AccountDataUnavailable
		authority.Freshness = rpc.AccountDataFreshnessStale
	default:
		authority.Availability = rpc.AccountDataUnavailable
		authority.Freshness = rpc.AccountDataFreshnessUnknown
	}
	authority.Reason = positionsAccountDataReason(arm)
	return authority
}

func accountDataScope(scope brokerStateScope) rpc.AccountDataScope {
	out := rpc.AccountDataScope{AccountMode: scope.Mode}
	if brokerScopeAccountConcrete(scope.Account) {
		out.AccountID = strings.TrimSpace(scope.Account)
	}
	return out
}

func positionsAccountDataReason(arm string) rpc.AccountDataReason {
	switch arm {
	case "":
		return ""
	case "stream_scope_conflict_latched":
		return rpc.AccountDataReasonScopeConflict
	case "stream_invalid_payload":
		return rpc.AccountDataReasonInvalidPayload
	case "daemon_scope_not_concrete":
		return rpc.AccountDataReasonScopeUnresolved
	case "stream_account_unbound":
		return rpc.AccountDataReasonAccountUnbound
	case "stream_account_mismatch":
		return rpc.AccountDataReasonAccountMismatch
	case "initial_download_incomplete":
		return rpc.AccountDataReasonUnprimed
	case "evidence_time_in_future":
		return rpc.AccountDataReasonClockInvalid
	case "receipt_stale":
		return rpc.AccountDataReasonReceiptStale
	default:
		return rpc.AccountDataReasonInvalidPayload
	}
}

// positionsResultAuthorityAsOf stamps only a complete, current, account-scoped
// a fresh empty-book receipt.
func positionsResultAuthorityAsOf(scope brokerStateScope, health ibkrlib.PortfolioStreamHealth, completedAt time.Time) time.Time {
	if classifyPortfolioStreamHealth(scope, health, completedAt.UTC()) != orderIntegrityHealthCurrent {
		return time.Time{}
	}
	return portfolioStreamEvidenceAsOf(health)
}

// positionStockQuoteBudget is the per-symbol wait for enriching held stock
const positionStockQuoteBudget = 1500 * time.Millisecond

// prewarmStockQuoteSummaries dispatches brief refcounted market-data holds
func (s *Server) prewarmStockQuoteSummaries(ctx context.Context, c *ibkrlib.Connector, stocks []rpc.PositionView) {
	if c == nil || len(stocks) == 0 {
		return
	}
	type job struct {
		index    int
		contract rpc.ContractParams
	}
	jobs := make([]job, 0, len(stocks))
	seen := map[string]bool{}
	for i := range stocks {
		// The non-option slice carries every secType that is not OPT — bonds,
		// with, and day-changed against, AT&T. Those rows keep the broker's
		if !positionQuotesAsStock(stocks[i]) {
			continue
		}
		sym := normSym(stocks[i].Symbol)
		if sym == "" || seen[sym] {
			continue
		}
		// Zero-value rows are probed too: the probe is the evidence that
		// either disproves inactivity (live quote) or reaches the broker's
		seen[sym] = true
		jobs = append(jobs, job{
			index: i,
			contract: rpc.ContractParams{
				Symbol:   sym,
				SecType:  "STK",
				Exchange: stocks[i].Exchange,
				Currency: stocks[i].Currency,
			},
		})
	}
	runBounded(jobs, positionsPrewarmWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		q, ok, terminal := s.snapshotHeldStockQuote(ctx, c, j.contract, positionStockQuoteBudget)
		if terminal {
			p := &stocks[j.index]
			p.QuoteExpectation = rpc.QuoteExpectationNone
			p.QuoteExpectationReason = rpc.QuoteExpectationReasonTerminal
		}
		if !ok {
			if s.prevCloses != nil {
				s.prevCloses.put(j.contract.Symbol, prevCloseEntry{}, time.Now())
			}
			return
		}
		closeAnchor := q.RegularClose
		if closeAnchor == nil {
			closeAnchor = q.PrevClose
		}
		if closeAnchor != nil && s.prevCloses != nil {
			s.prevCloses.put(j.contract.Symbol, prevCloseEntry{value: *closeAnchor}, time.Now())
		}
		p := &stocks[j.index]
		p.DataType = q.DataType
		p.PriceSource = "portfolio_mark"
		p.RegularClose = q.RegularClose
		p.RegularCloseAt = q.RegularCloseAt
		p.PriorRegularClose = q.PriorRegularClose
		p.RegularChange = q.RegularChange
		p.RegularChangePct = q.RegularChangePct
		p.QuotePrice = q.QuotePrice
		p.QuotePriceSource = q.QuotePriceSource
		p.QuotePriceAt = q.QuotePriceAt
		p.QuotePriceAsOf = q.QuotePriceAsOf
		p.QuoteChange = q.QuoteChange
		p.QuoteChangePct = q.QuoteChangePct
		p.PrevClose = closeAnchor
		p.Bid = q.Bid
		p.Ask = q.Ask
		p.DayHigh = q.DayHigh
		p.DayLow = q.DayLow
		p.Week52High = q.Week52High
		p.Week52Low = q.Week52Low
		p.Volume = q.Volume
		p.AvgVolume = q.AvgVolume
		p.FeedType = q.FeedType
		p.SpreadPct = q.SpreadPct
		p.QuoteQuality = q.QuoteQuality
		p.Indicative = q.Indicative
		p.VolumePhase = q.VolumePhase
		p.Stale = q.Stale
		p.StaleReason = q.StaleReason
		p.WarningDetails = q.WarningDetails
		p.SessionContext = q.SessionContext
	})
}

// snapshotHeldStockQuote probes one held stock for a quote summary.
// terminal reports the broker's own non-reporting verdict (the connector's
// guarded inactive mark, minted from confirmed "no security definition"
// answers) — the only evidence allowed to mint QuoteExpectationNone. A
// probe that merely times out or fails transiently is not terminal.
func (s *Server) snapshotHeldStockQuote(ctx context.Context, c *ibkrlib.Connector, contract rpc.ContractParams, timeout time.Duration) (q rpc.Quote, ok, terminal bool) {
	if s == nil || s.subs == nil {
		return rpc.Quote{}, false, false
	}
	routeContract, echoedContract, routedQuote, err := normaliseStockQuoteContract(contract)
	if err != nil {
		return rpc.Quote{}, false, false
	}
	sessionMarket, hasSessionMarket := quoteSessionMarketForContract(echoedContract)
	sym := echoedContract.Symbol

	pollKey := sym
	var releaseSub func()
	if routedQuote {
		key, err := c.SubscribeMarketDataWithContract(ctx, routeContract, defaultGenericTicks)
		if err != nil {
			return rpc.Quote{}, false, errors.Is(err, ibkrlib.ErrSymbolInactive)
		}
		pollKey = key
		releaseSub = func() { _ = c.UnsubscribeMarketData(key) }
	} else {
		release, err := s.subs.Hold(ctx, sym)
		if err != nil {
			return rpc.Quote{}, false, errors.Is(err, ibkrlib.ErrSymbolInactive)
		}
		releaseSub = release
	}
	defer releaseSub()

	q = rpc.Quote{
		Symbol:   sym,
		Contract: echoedContract,
		IVStatus: "unavailable",
		AsOf:     time.Now(),
	}
	var seen bool
	pollStarted := time.Now()
	_ = pollMarketData(ctx, c, pollKey, pollStarted.Add(timeout), func(d *ibkrlib.MarketData) bool {
		fillQuoteMarketData(&q, d)
		seen = true
		ready := q.Bid != nil || q.Ask != nil || q.Last != nil
		fallback := quoteFallbackReady(&q, pollStarted, timeout)
		if ready || fallback {
			q.DataType = quoteDataTypeName(c.MarketDataTypeForSymbol(pollKey), ready, fallback)
		}
		return ready || fallback
	})
	if !seen {
		return rpc.Quote{}, false, false
	}
	q.AsOf = time.Now()
	if quoteNeedsHistoryForSession(&q, sessionMarket, hasSessionMarket) {
		s.fillQuoteHistoricalFallback(ctx, c, &q, sessionMarket, timeout)
	}
	if hasSessionMarket {
		s.attachQuoteSessionContext(&q, sessionMarket)
	}
	s.decorateQuote(&q, sessionMarket)
	return q, true, false
}

// positionSecType maps IBKR's raw SecType codes ("STK", "OPT", "FUT", "IND")
// to the canonical wire values carried on PositionView.SecType.
// positionViewMultiplier normalises a raw wire multiplier to the per-row
// semantics PositionView reports. Stocks may carry 100 on the wire where the
// wire-shape contract is per-share, so that one case is coerced to 1. The
// coercion is STK-only because a future multiplier of 100 is meaningful.
func positionViewMultiplier(secType string, raw int) int {
	multiplier := max(raw, 1)
	if secType == "STK" && multiplier == 100 {
		return 1
	}
	return multiplier
}

func positionSecType(raw string) string {
	switch raw {
	case "STK":
		return rpc.SecTypeStock
	case "OPT":
		return rpc.SecTypeOption
	case "FUT":
		return rpc.SecTypeFuture
	case "IND":
		return rpc.SecTypeIndex
	}
	return raw
}

// positionViewKey produces a stable identifier for a PositionView,
// pointer) without threading those fields through the wire shape. Two
// Every identity field participates and SecType namespaces the key.
func positionViewKey(v rpc.PositionView) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%.4f", v.SecType, v.Symbol, v.ConID, v.Expiry, v.Right, v.Strike)
}

// maxDailyPnLSubscriptions caps the per-positions-call fan-out of
// reqPnLSingle subscriptions. IBKR doesn't document a hard limit, but
// accounts above the cap receive per-position P&L only for the first rows.
const maxDailyPnLSubscriptions = 50

// fillDailyPnL subscribes (if needed) to reqPnLSingle for each row's
//   - cache empty → subscribe (if we have an account and we're under
//
// Subscribing requires a concrete account; cached values remain readable without one.
func (s *Server) fillDailyPnL(c *ibkrlib.Connector, rows []rpc.PositionView, conIDs map[string]int, account string) {
	if c == nil || len(rows) == 0 {
		return
	}
	if !brokerScopeAccountConcrete(account) {
		account = ""
	}
	for i := range rows {
		view := &rows[i]
		conID, ok := conIDs[positionViewKey(*view)]
		if !ok || conID <= 0 {
			continue
		}
		if _, exists := c.PositionDailyPnL(conID); !exists && account != "" {
			if s.activeDailyPnLCount(c) >= maxDailyPnLSubscriptions {
				continue
			}
			if err := c.SubscribePositionDailyPnL(account, conID); err != nil {
				continue
			}
		}
		if snap, exists := c.PositionDailyPnL(conID); exists && snap.DailyPnL != nil {
			v := *snap.DailyPnL
			view.DailyPnL = &v
		}
	}
}

// activeDailyPnLCount is a thin probe of how many per-conId PnL
// without reaching into pkg/ibkr internals.
func (s *Server) activeDailyPnLCount(c *ibkrlib.Connector) int {
	return c.ActiveDailyPnLSubscriptions()
}

const fxRepairQuoteBudget = 1200 * time.Millisecond

type currencyRateResolver func(context.Context, string, string, time.Duration) (float64, bool)

// repairCurrencyLedgerFXRates repairs missing streaming-ledger FX rates.
func repairCurrencyLedgerFXRates(ctx context.Context, c *ibkrlib.Connector, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) map[string]ibkrlib.CurrencyLedger {
	return repairCurrencyLedgerFXRatesWithResolver(ctx, ledger, baseCcy, fxRepairQuoteBudget, func(ctx context.Context, baseCcy, ccy string, timeout time.Duration) (float64, bool) {
		return resolveBasePerCurrencyFXRate(ctx, c, baseCcy, ccy, timeout)
	})
}

func repairCurrencyLedgerFXRatesWithResolver(ctx context.Context, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string, timeout time.Duration, resolver currencyRateResolver) map[string]ibkrlib.CurrencyLedger {
	if len(ledger) == 0 {
		return nil
	}
	baseCcy = normCcy(baseCcy)
	out := make(map[string]ibkrlib.CurrencyLedger, len(ledger))
	needsRepair := make([]string, 0)
	for ccy, row := range ledger {
		ccy = normCcy(ccy)
		if ccy == "" {
			continue
		}
		if baseCcy != "" && ccy != baseCcy && (row.ExchangeRate <= 0 || row.ExchangeRate == 1.0) {
			needsRepair = append(needsRepair, ccy)
		}
		out[ccy] = row
	}
	if baseCcy == "" || resolver == nil || len(needsRepair) == 0 {
		return out
	}

	resolved := make(map[string]float64, len(needsRepair))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, ccy := range needsRepair {
		wg.Go(func() {
			rate, ok := resolver(ctx, baseCcy, ccy, timeout)
			if !ok || rate <= 0 {
				return
			}
			mu.Lock()
			resolved[ccy] = rate
			mu.Unlock()
		})
	}
	wg.Wait()

	for _, ccy := range needsRepair {
		row := out[ccy]
		if rate, ok := resolved[ccy]; ok {
			row.ExchangeRate = rate
		} else {
			row.ExchangeRate = 0
		}
		out[ccy] = row
	}
	return out
}

func resolveBasePerCurrencyFXRate(ctx context.Context, c *ibkrlib.Connector, baseCcy, ccy string, timeout time.Duration) (float64, bool) {
	if c == nil {
		return 0, false
	}
	baseCcy = normCcy(baseCcy)
	ccy = normCcy(ccy)
	if baseCcy == "" || ccy == "" {
		return 0, false
	}
	if baseCcy == ccy {
		return 1, true
	}
	if price, ok := snapshotFXPrice(ctx, c, baseCcy+"."+ccy, timeout); ok {
		return 1 / price, true
	}
	if price, ok := snapshotFXPrice(ctx, c, ccy+"."+baseCcy, timeout); ok {
		return price, true
	}
	return 0, false
}

func snapshotFXPrice(ctx context.Context, c *ibkrlib.Connector, pair string, timeout time.Duration) (float64, bool) {
	q := briefSnapshotPriceWithClose(ctx, c, pair, timeout, nil)
	if q.price <= 0 {
		return 0, false
	}
	return q.price, true
}

func mergeCurrencyLedgers(primary, fallback map[string]ibkrlib.CurrencyLedger) map[string]ibkrlib.CurrencyLedger {
	if len(primary) == 0 {
		return fallback
	}
	out := make(map[string]ibkrlib.CurrencyLedger, len(primary)+len(fallback))
	for ccy, row := range primary {
		if ccy = normCcy(ccy); ccy != "" {
			out[ccy] = row
		}
	}
	for ccy, row := range fallback {
		ccy = normCcy(ccy)
		if ccy == "" {
			continue
		}
		if _, ok := out[ccy]; !ok {
			out[ccy] = row
		}
	}
	return out
}

func missingPositionFXCurrencies(stocks, options []rpc.PositionView, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) []string {
	baseCcy = normCcy(baseCcy)
	missing := map[string]struct{}{}
	check := func(rows []rpc.PositionView) {
		for _, row := range rows {
			ccy := normCcy(row.Currency)
			if ccy == "" || ccy == baseCcy {
				continue
			}
			entry, ok := ledger[ccy]
			if !ok || entry.ExchangeRate <= 0 {
				missing[ccy] = struct{}{}
			}
		}
	}
	check(stocks)
	check(options)
	out := make([]string, 0, len(missing))
	for ccy := range missing {
		out = append(out, ccy)
	}
	slices.Sort(out)
	return out
}

// fillFXRates copies the per-currency ExchangeRate into each non-base
func fillFXRates(rows []rpc.PositionView, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) {
	for i := range rows {
		p := &rows[i]
		ccy := normCcy(p.Currency)
		if ccy == "" || ccy == baseCcy {
			continue
		}
		entry, ok := ledger[ccy]
		if !ok || entry.ExchangeRate <= 0 {
			continue
		}
		fx := entry.ExchangeRate
		p.FXRate = &fx
	}
}

func fillBaseValues(rows []rpc.PositionView, baseCcy string) {
	for i := range rows {
		p := &rows[i]
		rate, ok := positionBaseRate(*p, baseCcy)
		if !ok {
			continue
		}
		marketValueBase := p.MarketValue * rate
		p.MarketValueBase = &marketValueBase
		unrealizedBase := p.UnrealizedPnL * rate
		p.UnrealizedPnLBase = &unrealizedBase
		realizedBase := p.RealizedPnL * rate
		p.RealizedPnLBase = &realizedBase
		if p.DailyPnL != nil {
			dailyBase := *p.DailyPnL * rate
			p.DailyPnLBase = &dailyBase
		}
	}
}

func positionBaseRate(p rpc.PositionView, baseCcy string) (float64, bool) {
	baseCcy = normCcy(baseCcy)
	ccy := normCcy(p.Currency)
	if baseCcy == "" || ccy == "" {
		return 0, false
	}
	if ccy == baseCcy {
		return 1, true
	}
	if p.FXRate != nil && *p.FXRate > 0 {
		return *p.FXRate, true
	}
	return 0, false
}

func flagClosedOptionSession(options []rpc.PositionView, now time.Time) {
	if len(options) == 0 || optionSessionOpen(now) {
		return
	}
	for i := range options {
		p := &options[i]
		if positionWarningHasCode(p.WarningDetails, "options_closed") {
			continue
		}
		p.WarningDetails = append(p.WarningDetails, rpc.DataWarning{
			Code:     "options_closed",
			Scope:    optionWarningScope(*p),
			Severity: "info",
			Message:  "The regular U.S. listed-options data surface is outside its official session.",
			Impact:   "Option bid/ask, previous close, IV, and Greeks are closed-session context, not executable quotes, unless live fields landed; missing Greeks on a thinly quoted leg are expected here, not a data fault; SPX/VIX extended sessions do not guarantee a complete API surface.",
			Action:   "Use the account mark for held-position valuation; Greeks and the full quote/OI/IV surface normally return with the next options session.",
		})
	}
}

func flagOptionMarkOutsideBidAsk(options []rpc.PositionView) {
	for i := range options {
		p := &options[i]
		if p.OptionBid == nil || p.OptionAsk == nil {
			continue
		}
		bid, ask := *p.OptionBid, *p.OptionAsk
		if bid < 0 || ask <= 0 || bid > ask {
			continue
		}
		const eps = 1e-9
		if p.Mark+eps >= bid && p.Mark-eps <= ask {
			continue
		}
		p.MarkOutsideBidAsk = true
		scope := optionWarningScope(*p)
		p.WarningDetails = append(p.WarningDetails, rpc.DataWarning{
			Code:     "mark_outside_bid_ask",
			Scope:    scope,
			Severity: "data_quality",
			Message:  "Option valuation mark is outside the bid/ask range.",
			Impact:   "The account mark may be stale, model-derived, or not currently executable.",
			Action:   "Refresh during the regular option session and compare option_bid/option_ask before using the mark.",
		})
	}
}

// flagZeroValueStockPositions marks rows whose quote probe left them with
// authority and retains them until broker evidence resolves the zero value.
func flagZeroValueStockPositions(stocks []rpc.PositionView) {
	for i := range stocks {
		p := &stocks[i]
		if !stockPositionLooksInactive(*p) {
			continue
		}
		if positionWarningHasCode(p.WarningDetails, "zero_value_stock_position") {
			continue
		}
		p.Stale = true
		p.StaleReason = "zero-value portfolio row; likely inactive or defunct"
		p.WarningDetails = append(p.WarningDetails, rpc.DataWarning{
			Code:     "zero_value_stock_position",
			Scope:    p.Symbol,
			Severity: "data_quality",
			Message:  "Held stock position has nonzero quantity but zero mark and zero market value.",
			Impact:   "Unresolved exposure until a live quote or the broker's terminal non-reporting verdict resolves it; remains visible as account position truth.",
			Action:   "Reconcile the holding with broker records before using it in risk or protection workflows.",
		})
	}
}

func positionWarningHasCode(warnings []rpc.DataWarning, code string) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func optionWarningScope(p rpc.PositionView) string {
	parts := []string{normSym(p.Symbol)}
	if p.Expiry != "" {
		parts = append(parts, p.Expiry)
	}
	if p.Right != "" {
		parts = append(parts, strings.ToUpper(p.Right))
	}
	if p.Strike > 0 {
		parts = append(parts, strconv.FormatFloat(p.Strike, 'f', -1, 64))
	}
	return strings.Join(parts, " ")
}

// addFXSensitivity computes the portfolio-wide 1%-FX-move sensitivity
// — never fabricates a zero when the answer is "unknown".
func addFXSensitivity(p *rpc.PositionsPortfolio, ledger map[string]ibkrlib.CurrencyLedger, baseCcy string) {
	if p == nil || len(ledger) == 0 {
		return
	}
	baseCcy = normCcy(baseCcy)
	if baseCcy == "" {
		return
	}
	var sens float64
	any := false
	for ccy, row := range ledger {
		if normCcy(ccy) == baseCcy {
			continue
		}
		if row.NetLiquidationByCurrency == 0 || row.ExchangeRate <= 0 {
			continue
		}
		sens += row.NetLiquidationByCurrency * row.ExchangeRate * 0.01
		any = true
	}
	if !any {
		return
	}
	v := sens
	p.FXSensitivityPerPct = &v
	p.FXBaseCurrency = baseCcy
}

// fillDailyChange populates previous-close and daily-change fields.
func (s *Server) fillDailyChange(stocks []rpc.PositionView) {
	now := time.Now()
	for i := range stocks {
		p := &stocks[i]
		// prevCloses is keyed by bare symbol, so a held equity's close would
		// only equity rows may use the bare-symbol previous-close cache.
		if !positionQuotesAsStock(*p) {
			continue
		}
		anchor := 0.0
		if p.RegularClose != nil && *p.RegularClose > 0 {
			anchor = *p.RegularClose
		} else if s.prevCloses != nil {
			sym := normSym(p.Symbol)
			if e, ok := s.prevCloses.get(sym, now); ok && e.value > 0 {
				anchor = e.value
			}
		}
		if anchor <= 0 {
			continue
		}
		v := anchor
		if p.RegularClose == nil {
			p.RegularClose = &v
		}
		p.PrevClose = &v
		p.DayChange, p.DayChangePct = computePositionDayChange(p.Mark, anchor)
		if p.DayChange != nil {
			money := p.Quantity * *p.DayChange
			p.DayChangeMoney = &money
		}
	}
}

// fillOptionDayChangeMoney computes the position-level dollar move on
// standard option day move and defaults a missing multiplier to 100.
func fillOptionDayChangeMoney(options []rpc.PositionView) {
	for i := range options {
		p := &options[i]
		if p.OptionPrevClose == nil || p.Mark <= 0 || *p.OptionPrevClose <= 0 {
			continue
		}
		mult := p.Multiplier
		if mult <= 0 {
			mult = 100
		}
		money := p.Quantity * float64(mult) * (p.Mark - *p.OptionPrevClose)
		p.DayChangeMoney = &money
	}
}

// fillOptionUnderlyingPrevClose copies the cached underlying regular close.
func (s *Server) fillOptionUnderlyingPrevClose(options []rpc.PositionView) {
	if s.prevCloses == nil {
		return
	}
	now := time.Now()
	for i := range options {
		p := &options[i]
		under := normSym(p.Symbol)
		e, ok := s.prevCloses.get(under, now)
		if !ok || e.value <= 0 {
			continue
		}
		v := e.value
		p.PrevClose = &v
	}
}

// positionsPrewarmWorkers bounds per-call market-data enrichment concurrency.
const positionsPrewarmWorkers = 4

// optionGreeksBudget bounds each option-Greeks capture.
const optionGreeksBudget = 2500 * time.Millisecond

// prewarmOptionGreeks dispatches bounded concurrent option-Greeks captures.
func (s *Server) prewarmOptionGreeks(ctx context.Context, c *ibkrlib.Connector, options []rpc.PositionView) {
	if s.greeks == nil || c == nil || len(options) == 0 {
		return
	}
	now := time.Now()
	type job struct {
		key    string
		under  string
		expiry string
		strike float64
		right  string
	}
	var jobs []job
	seen := map[string]bool{}
	for _, p := range options {
		key := optionGreeksKey(p)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := s.greeks.get(key, now); ok {
			continue
		}
		jobs = append(jobs, job{
			key:    key,
			under:  strings.ToUpper(p.Symbol),
			expiry: p.Expiry,
			strike: p.Strike,
			right:  p.Right,
		})
	}
	runBounded(jobs, positionsPrewarmWorkers, func(j job) {
		if ctx.Err() != nil {
			return
		}
		entry := captureOptionGreeks(ctx, c, j.under, j.expiry, j.strike, j.right, optionGreeksBudget)
		s.greeks.put(j.key, entry, time.Now())
	})
}

// captureOptionGreeks runs one option subscribe → poll → unsubscribe
func captureOptionGreeks(ctx context.Context, c *ibkrlib.Connector, under, expiryYMD string, strike float64, right string, budget time.Duration) greeksEntry {
	out := greeksEntry{}
	if under == "" || expiryYMD == "" || strike <= 0 || right == "" {
		return out
	}
	// Single-class default — captureOptionGreeks is called from the
	// chain-prewarm path which doesn't disambiguate SPX vs SPXW today.
	key, _, err := c.SubscribeOption(ctx, under, under, expiryYMD, strike, right)
	if err != nil {
		return out
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	_ = pollUntilWithReject(ctx, time.Now().Add(budget), c.SubscriptionRejectCh(key), key, func() bool {
		g, ok := c.OptionGreeks(key)
		if !ok {
			return false
		}
		out.value = g
		out.ok = true
		if u, uok := c.OptionUnderlyingPrice(key); uok && u > 0 {
			out.underlying = u
		}
		return true
	})
	return out
}

// fillOptionGreeks copies cached Greeks onto each option leg's
// never zero-substituted.
func (s *Server) fillOptionGreeks(c *ibkrlib.Connector, options []rpc.PositionView) {
	if s.greeks == nil {
		return
	}
	now := time.Now()
	for i := range options {
		p := &options[i]
		key := optionGreeksKey(*p)
		if key == "" {
			continue
		}
		e, ok := s.greeks.get(key, now)
		if ok && e.ok {
			// e.ok is the cache's "captured tick" gate; per the wire
			// straddle delta ≈ 0 — must surface as a non-nil pointer.
			g := e.value
			p.Delta = &g.Delta
			p.Gamma = &g.Gamma
			p.Theta = &g.Theta
			p.Vega = &g.Vega
			// Underlying spot from the same model-computation tick that
			// produced the Greeks. The aggregator pairs it with delta so
			// dollar delta is computed against the spot the delta was
			// modelled at — see rpc.PositionView.Underlying doc.
			if e.underlying > 0 {
				u := e.underlying
				p.Underlying = &u
			}
		}
		if c == nil {
			continue
		}
		if iv, ok := c.OptionIV(key); ok && iv > 0 {
			v := iv
			p.IV = &v
		}
		if pc, ok := c.OptionPrevClose(key); ok {
			v := pc
			p.OptionPrevClose = &v
		}
	}
}

// optionGreeksKey builds the same OPRA-style key that
// stamped by pkg/ibkr's convertIBKRPositions, the canonical wire value)
// and "OPT" (the IBKR API request-side short form, here as a defensive
// The original v0.10.0 release had only the "OPT" check and reported
// greeks_coverage 0/N for every option-bearing account, because
// belt-and-braces fix.
func optionGreeksKey(p rpc.PositionView) string {
	if p.SecType != rpc.SecTypeOption && p.SecType != "OPT" {
		return ""
	}
	under := normSym(p.Symbol)
	if under == "" || len(p.Expiry) < 8 || p.Strike <= 0 || p.Right == "" {
		return ""
	}
	return fmt.Sprintf("%s_%s%s%.0f", under, p.Expiry[2:], strings.ToUpper(p.Right), p.Strike)
}

// buildPortfolioAggregates rolls per-leg Greeks and currency exposure
// price IBKR sent alongside the Greeks (kept in lockstep so the dollar
// single contract currency only when every contributing option leg
func buildPortfolioAggregatesWithBase(stocks, options []rpc.PositionView, baseCcy string) *rpc.PositionsPortfolio {
	acc := newPortfolioAggregateAccumulator(baseCcy)
	for _, o := range options {
		acc.addOption(o)
	}
	for _, st := range stocks {
		acc.addStock(st)
	}
	return acc.portfolio()
}

type portfolioAggregateAccumulator struct {
	baseCcy string
	p       rpc.PositionsPortfolio

	effectiveDelta float64
	dollarDelta    float64
	dailyTheta     float64
	gamma          float64
	vega           float64

	dollarDeltaBase convertedSum
	dailyThetaBase  convertedSum

	haveEffectiveDelta bool
	haveDollarDelta    bool
	haveDailyTheta     bool
	haveGamma          bool
	haveVega           bool

	dollarDeltaCcy   string
	dollarDeltaMixed bool
	dailyThetaCcy    string
	dailyThetaMixed  bool
}

func newPortfolioAggregateAccumulator(baseCcy string) *portfolioAggregateAccumulator {
	return &portfolioAggregateAccumulator{baseCcy: normCcy(baseCcy)}
}

func (a *portfolioAggregateAccumulator) addOption(o rpc.PositionView) {
	a.p.GreeksTotal++
	mult := float64(optionMultiplier(o))
	if o.Delta != nil {
		a.effectiveDelta += *o.Delta * o.Quantity * mult
		a.haveEffectiveDelta = true
	}
	if localDollarDelta, ok := positionDollarDelta(o, true); ok {
		a.addDollarDelta(localDollarDelta, o)
	}
	if o.Theta != nil {
		a.addDailyTheta(*o.Theta*o.Quantity*mult, o)
	}
	if o.Gamma != nil {
		a.gamma += *o.Gamma * o.Quantity * mult
		a.haveGamma = true
	}
	if o.Vega != nil {
		a.vega += *o.Vega * o.Quantity * mult
		a.haveVega = true
	}
	if positionHasGreek(o) {
		a.p.GreeksCoverage++
	}
}

func (a *portfolioAggregateAccumulator) addStock(st rpc.PositionView) {
	localDollarDelta, ok := positionDollarDelta(st, false)
	if !ok {
		return
	}
	// Stock legs add raw share-equivalent exposure to effective + dollar
	a.effectiveDelta += st.Quantity
	a.haveEffectiveDelta = true
	a.addDollarDelta(localDollarDelta, st)
}

func (a *portfolioAggregateAccumulator) addDollarDelta(value float64, row rpc.PositionView) {
	a.dollarDelta += value
	a.haveDollarDelta = true
	a.dollarDeltaBase.add(value, row, a.baseCcy)
	trackAggregateCurrency(&a.dollarDeltaCcy, &a.dollarDeltaMixed, row.Currency)
}

func (a *portfolioAggregateAccumulator) addDailyTheta(value float64, row rpc.PositionView) {
	a.dailyTheta += value
	a.haveDailyTheta = true
	a.dailyThetaBase.add(value, row, a.baseCcy)
	trackAggregateCurrency(&a.dailyThetaCcy, &a.dailyThetaMixed, row.Currency)
}

func (a *portfolioAggregateAccumulator) portfolio() *rpc.PositionsPortfolio {
	if a.haveEffectiveDelta {
		v := a.effectiveDelta
		a.p.EffectiveDelta = &v
	}
	if a.haveDollarDelta {
		v := a.dollarDelta
		a.p.DollarDelta = &v
		if a.dollarDeltaMixed {
			a.p.DollarDeltaCurrency = "MIX"
		} else {
			a.p.DollarDeltaCurrency = a.dollarDeltaCcy
		}
		if v := a.dollarDeltaBase.ptr(); v != nil {
			a.p.DollarDeltaBase = v
			a.p.DollarDeltaBaseCurrency = a.baseCcy
		}
	}
	if a.haveDailyTheta {
		v := a.dailyTheta
		a.p.DailyTheta = &v
		if a.dailyThetaMixed {
			a.p.DailyThetaCurrency = "MIX"
		} else {
			a.p.DailyThetaCurrency = a.dailyThetaCcy
		}
		if v := a.dailyThetaBase.ptr(); v != nil {
			a.p.DailyThetaBase = v
			a.p.DailyThetaBaseCurrency = a.baseCcy
		}
	}
	if a.haveGamma {
		v := a.gamma
		a.p.Gamma = &v
	}
	if a.haveVega {
		v := a.vega
		a.p.Vega = &v
	}
	return &a.p
}

func positionHasGreek(row rpc.PositionView) bool {
	return row.Delta != nil || row.Theta != nil || row.Gamma != nil || row.Vega != nil
}

func trackAggregateCurrency(current *string, mixed *bool, ccy string) {
	ccy = normCcy(ccy)
	if *current == "" {
		*current = ccy
	} else if ccy != *current {
		*mixed = true
	}
}

// optionMultiplier returns the contract multiplier used to scale a per-
// is populated from the wire (msgPortfolioValue → pos.Asset.Multiplier),
// options (sometimes 50 or 1000). Falls back to 100 only when the wire
// Without the fallback a missing multiplier would erase option exposure.
func optionMultiplier(p rpc.PositionView) int {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 100
}

// groupByUnderlying produces one PositionGroup per underlying symbol present
// in stock and option rows; other security types do not anchor an equity group.
func groupByUnderlying(stocks, options []rpc.PositionView, baseCcy string, netLiquidationBase *float64) []rpc.PositionGroup {
	groups := map[string]*rpc.PositionGroup{}
	getOrInit := func(under string) *rpc.PositionGroup {
		g, ok := groups[under]
		if !ok {
			g = &rpc.PositionGroup{Underlying: under}
			groups[under] = g
		}
		return g
	}
	// A symbol can arrive as more than one equity row — a dual-listed share
	stockRows := map[string][]rpc.PositionView{}
	for i := range stocks {
		s := stocks[i]
		if !positionCanAnchorUnderlyingGroup(s) {
			continue
		}
		under := strings.ToUpper(s.Symbol)
		g := getOrInit(under)
		stk := s
		g.Stock = &stk
		stockRows[under] = append(stockRows[under], s)
		g.GroupMarketValue += s.MarketValue
		g.GroupUnrealizedPnL += s.UnrealizedPnL
	}
	for i := range options {
		o := options[i]
		g := getOrInit(strings.ToUpper(o.Symbol))
		g.Options = append(g.Options, o)
		g.GroupMarketValue += o.MarketValue
		g.GroupUnrealizedPnL += o.UnrealizedPnL
	}
	out := make([]rpc.PositionGroup, 0, len(groups))
	for under, g := range groups {
		finalizePositionGroup(g, stockRows[under], baseCcy, netLiquidationBase)
		out = append(out, *g)
	}
	slices.SortStableFunc(out, func(a, b rpc.PositionGroup) int { return cmp.Compare(a.Underlying, b.Underlying) })
	return out
}

// positionCanAnchorUnderlyingGroup accepts only the equity aliases carried by
// position DTOs; missing type is not stock authority.
func positionCanAnchorUnderlyingGroup(row rpc.PositionView) bool {
	return positionQuotesAsStock(row)
}

// positionQuotesAsStock reports whether a non-option row is an equity — the
// only type safe to quote through the bare-symbol stock route.
func positionQuotesAsStock(row rpc.PositionView) bool {
	return rpc.PositionQuotesAsStock(row)
}

type convertedSum struct {
	sum     float64
	any     bool
	missing bool
}

func (s *convertedSum) add(value float64, row rpc.PositionView, baseCcy string) {
	rate, ok := positionBaseRate(row, baseCcy)
	if !ok {
		s.missing = true
		return
	}
	s.sum += value * rate
	s.any = true
}

func (s convertedSum) ptr() *float64 {
	if !s.any || s.missing {
		return nil
	}
	v := s.sum
	return &v
}

// finalizePositionGroup derives the group's base-currency totals and delta.
// aggregates all exact rows even though the display retains one representative.
func finalizePositionGroup(g *rpc.PositionGroup, stockRows []rpc.PositionView, baseCcy string, netLiquidationBase *float64) {
	if g == nil {
		return
	}
	baseCcy = normCcy(baseCcy)
	var marketBase, unrealizedBase, dailyBase, dollarDeltaBase convertedSum
	var effectiveDelta, dollarDelta float64
	var haveEffectiveDelta, haveDollarDelta bool
	dollarCcy := ""
	dollarMixed := false

	visit := func(row rpc.PositionView, isOption bool) {
		marketBase.add(row.MarketValue, row, baseCcy)
		unrealizedBase.add(row.UnrealizedPnL, row, baseCcy)
		if row.DailyPnL != nil {
			dailyBase.add(*row.DailyPnL, row, baseCcy)
		}
		if isOption {
			if row.Delta != nil {
				effectiveDelta += *row.Delta * row.Quantity * float64(optionMultiplier(row))
				haveEffectiveDelta = true
			}
		} else if row.Mark > 0 {
			effectiveDelta += row.Quantity
			haveEffectiveDelta = true
		}
		localDollarDelta, ok := positionDollarDelta(row, isOption)
		if !ok {
			return
		}
		dollarDelta += localDollarDelta
		haveDollarDelta = true
		dollarDeltaBase.add(localDollarDelta, row, baseCcy)
		ccy := normCcy(row.Currency)
		if dollarCcy == "" {
			dollarCcy = ccy
		} else if ccy != dollarCcy {
			dollarMixed = true
		}
	}

	for _, row := range stockRows {
		visit(row, false)
	}
	for _, opt := range g.Options {
		visit(opt, true)
	}

	if v := marketBase.ptr(); v != nil {
		g.GroupMarketValueBase = v
		if netLiquidationBase != nil && *netLiquidationBase != 0 {
			pct := *v / *netLiquidationBase * 100
			g.GroupMarketValuePctNLV = &pct
		}
	}
	g.GroupUnrealizedPnLBase = unrealizedBase.ptr()
	g.GroupDailyPnLBase = dailyBase.ptr()
	if haveEffectiveDelta {
		v := effectiveDelta
		g.GroupEffectiveDelta = &v
	}
	if haveDollarDelta {
		v := dollarDelta
		g.GroupDollarDelta = &v
		if dollarMixed {
			g.GroupDollarDeltaCurrency = "MIX"
		} else {
			g.GroupDollarDeltaCurrency = dollarCcy
		}
		g.GroupDollarDeltaBase = dollarDeltaBase.ptr()
	}
}

func positionDollarDelta(row rpc.PositionView, isOption bool) (float64, bool) {
	if isOption {
		if row.Delta == nil {
			return 0, false
		}
		spot := 0.0
		if row.Underlying != nil && *row.Underlying > 0 {
			spot = *row.Underlying
		} else if row.PrevClose != nil && *row.PrevClose > 0 {
			spot = *row.PrevClose
		}
		if spot <= 0 {
			return 0, false
		}
		return *row.Delta * row.Quantity * float64(optionMultiplier(row)) * spot, true
	}
	if row.Mark <= 0 {
		return 0, false
	}
	return row.Quantity * row.Mark, true
}

func addPortfolioBaseContext(p *rpc.PositionsPortfolio, groups []rpc.PositionGroup, baseCcy string, netLiquidationBase *float64) {
	if p == nil {
		return
	}
	baseCcy = normCcy(baseCcy)
	p.BaseCurrency = baseCcy
	p.NetLiquidationBase = netLiquidationBase
	p.ExposureBase, p.ExposureUnmeasured = buildUnderlyingExposureBase(groups, baseCcy)
}

// buildUnderlyingExposureBase projects the groups the aggregator could value in
// the account base currency, and names the ones it could not. A group with no
// base market value is dropped rather than zeroed, so the names are the only
// evidence a consumer has that the returned rows are not the whole book.
func buildUnderlyingExposureBase(groups []rpc.PositionGroup, baseCcy string) ([]rpc.UnderlyingExposure, []string) {
	baseCcy = normCcy(baseCcy)
	out := make([]rpc.UnderlyingExposure, 0, len(groups))
	var unmeasured []string
	for _, g := range groups {
		if g.GroupMarketValueBase == nil {
			unmeasured = append(unmeasured, g.Underlying)
			continue
		}
		out = append(out, rpc.UnderlyingExposure{
			Underlying:        g.Underlying,
			MarketValueBase:   *g.GroupMarketValueBase,
			MarketValuePctNLV: g.GroupMarketValuePctNLV,
			EffectiveDelta:    g.GroupEffectiveDelta,
			DollarDeltaBase:   g.GroupDollarDeltaBase,
			UnrealizedPnLBase: g.GroupUnrealizedPnLBase,
			DailyPnLBase:      g.GroupDailyPnLBase,
			BaseCurrency:      baseCcy,
		})
	}
	slices.SortStableFunc(out, func(a, b rpc.UnderlyingExposure) int {
		if c := cmp.Compare(math.Abs(b.MarketValueBase), math.Abs(a.MarketValueBase)); c != 0 {
			return c
		}
		return cmp.Compare(a.Underlying, b.Underlying)
	})
	slices.Sort(unmeasured)
	return out, unmeasured
}

// handleQuoteSnapshot resolves a contract, briefly subscribes to streaming
// returns a snapshot. We avoid IBKR's true snapshot mode (snapshot=true)
func (s *Server) handleQuoteSnapshot(ctx context.Context, req *rpc.Request) (*rpc.Quote, error) {
	var p rpc.QuoteSnapshotParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	if p.Contract.Symbol == "" {
		return nil, errBadRequest("contract.symbol required")
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if isOptionQuoteContract(p.Contract) {
		q, err := s.handleOptionQuoteSnapshot(ctx, c, p, timeout)
		if err == nil && quoteCarriesMarketDataWitness(q) {
			s.observeMarketDataWitness(q.AsOf)
		}
		return q, err
	}

	routeContract, echoedContract, routedQuote, err := normaliseStockQuoteContract(p.Contract)
	if err != nil {
		return nil, err
	}
	sym := echoedContract.Symbol
	q := &rpc.Quote{
		Symbol:   sym,
		Contract: echoedContract,
		IVStatus: "unavailable",
		AsOf:     time.Now(),
	}
	// FX pairs (USD.JPY / USD/JPY) route through CASH/IDEALPRO regardless
	// IBKR subscription is driven by pkg/ibkr.classifySymbol(sym) inside
	if _, quote, ok := ibkrlib.FxPair(sym); ok {
		q.Contract.SecType = "CASH"
		q.Contract.Exchange = "IDEALPRO"
		q.Contract.Currency = quote
	}
	sessionMarket, hasSessionMarket := quoteSessionMarketForContract(q.Contract)

	pollKey := sym
	var releaseSub func()
	if routedQuote {
		key, err := c.SubscribeMarketDataWithContract(ctx, routeContract, defaultGenericTicks)
		if err != nil && !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			if shell := s.absentQuoteShell(q, err, sessionMarket, hasSessionMarket); shell != nil {
				return shell, nil
			}
			return nil, err
		}
		pollKey = key
		releaseSub = func() { _ = c.UnsubscribeMarketData(key) }
	} else {
		// Route through the daemon's subscription manager so a snapshot
		// running concurrently with `quote --watch` (or another snapshot, or
		// an MCP subscriber) shares the same IBKR market-data line via the
		// refcount. Without this, the snapshot's deferred unsubscribe used
		// to cancel the watcher's sub mid-stream.
		release, err := s.subs.Hold(ctx, sym)
		if err != nil && !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			if shell := s.absentQuoteShell(q, err, sessionMarket, hasSessionMarket); shell != nil {
				return shell, nil
			}
			return nil, err
		}
		releaseSub = release
	}
	defer releaseSub()

	pollStarted := time.Now()
	if err := pollMarketData(ctx, c, pollKey, pollStarted.Add(timeout), func(d *ibkrlib.MarketData) bool {
		fillQuoteMarketData(q, d)
		ready := q.Bid != nil || q.Ask != nil || q.Last != nil
		fallback := quoteFallbackReady(q, pollStarted, timeout)
		if ready || fallback {
			// Capture the gateway's feed state while the subscription is
			// still live — once the deferred unsubscribe fires, the
			// connector's symbol→reqID mapping is gone and the type would
			// always read "". When IBKR omits that notice but only
			// fallback ticks landed, label the row frozen so JSON consumers
			// don't mistake mark/close-only data for a live quote.
			q.DataType = quoteDataTypeName(c.MarketDataTypeForSymbol(pollKey), ready, fallback)
		}
		return ready || fallback
	}); err != nil {
		switch {
		case err == context.DeadlineExceeded:
			inactiveKey := pollKey
			if !routedQuote {
				inactiveKey = ibkrlib.DefaultMarketDataKeyForSymbol(sym)
			}
			if inactiveKey != "" {
				if _, inactive := c.InactiveReason(inactiveKey); inactive {
					return nil, ibkrlib.ErrSymbolInactive
				}
			}
		case IsSubscriptionRejected(err):
			// Terminal gateway rejection (354/200): this is the
			// once-per-absence-window honest probe. Fall through to the
			// same shell + historical-fallback shape a quiet timeout
			// produces — a hard error here would red-flag the app's
			// whole market_quotes source over one dead symbol.
			q.Stale = true
			q.StaleReason = err.Error()
		default:
			return nil, err
		}
	}
	q.AsOf = time.Now()
	var historicalBars []ibkrlib.HistoricalBar
	if quoteNeedsHistoryForSession(q, sessionMarket, hasSessionMarket) {
		historicalBars = s.fillQuoteHistoricalFallback(ctx, c, q, sessionMarket, timeout)
	}
	if p.IncludeLiquidity && strings.EqualFold(q.Contract.SecType, "STK") {
		s.fillQuoteLiquidity(ctx, c, q, sessionMarket, timeout, historicalBars)
	}
	if hasSessionMarket {
		s.attachQuoteSessionContext(q, sessionMarket)
	}
	s.decorateQuote(q, sessionMarket)
	if quoteCarriesMarketDataWitness(q) {
		s.observeMarketDataWitness(q.AsOf)
	}

	return q, nil
}

// absentQuoteShell converts a MarketDataAbsenceError from a subscribe
func (s *Server) absentQuoteShell(q *rpc.Quote, err error, market marketcal.Market, hasSessionMarket bool) *rpc.Quote {
	var absent *ibkrlib.MarketDataAbsenceError
	if !errors.As(err, &absent) {
		return nil
	}
	q.AsOf = time.Now()
	if hasSessionMarket {
		s.attachQuoteSessionContext(q, market)
	}
	s.decorateQuote(q, market)
	q.Stale = true
	q.StaleReason = absent.Error()
	return q
}

func isOptionQuoteContract(c rpc.ContractParams) bool {
	return strings.EqualFold(strings.TrimSpace(c.SecType), "OPT") ||
		strings.TrimSpace(c.Expiry) != "" ||
		strings.TrimSpace(c.Right) != "" ||
		c.Strike > 0
}

func normaliseStockQuoteContract(in rpc.ContractParams) (ibkrlib.Contract, rpc.ContractParams, bool, error) {
	sym := normSym(in.Symbol)
	if sym == "" {
		return ibkrlib.Contract{}, rpc.ContractParams{}, false, errBadRequest("contract.symbol required")
	}
	secType := strings.ToUpper(strings.TrimSpace(in.SecType))
	if secType == "" {
		secType = "STK"
	}
	market := strings.ToLower(strings.TrimSpace(in.Market))
	exchange := strings.ToUpper(strings.TrimSpace(in.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(in.PrimaryExch))
	currency := normCcy(in.Currency)
	localSymbol := strings.TrimSpace(in.LocalSymbol)
	tradingClass := strings.TrimSpace(in.TradingClass)

	routed := market != "" && market != "us" ||
		exchange != "" ||
		primary != "" ||
		localSymbol != "" ||
		tradingClass != "" ||
		(currency != "" && currency != "USD")

	switch market {
	case "", "us":
		if currency == "" {
			currency = "USD"
		}
		if routed && exchange == "" {
			exchange = "SMART"
		}
	case "de", "germany", "xetra", "ibis":
		if currency == "" {
			currency = "EUR"
		}
		if exchange == "" && primary == "" {
			exchange = "SMART"
			primary = "IBIS"
		}
	default:
		return ibkrlib.Contract{}, rpc.ContractParams{}, false, errBadRequest(fmt.Sprintf("unsupported quote market %q (supported: us, de)", in.Market))
	}
	if routed && exchange == "" {
		exchange = "SMART"
	}

	echo := rpc.ContractParams{
		ConID:        in.ConID,
		Symbol:       sym,
		SecType:      secType,
		Market:       market,
		Exchange:     exchange,
		PrimaryExch:  primary,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
		Multiplier:   in.Multiplier,
	}
	if !routed {
		echo.Exchange = ""
		if strings.TrimSpace(in.Currency) == "" {
			echo.Currency = ""
		}
	}
	contract := ibkrlib.Contract{
		ConID:        in.ConID,
		Symbol:       sym,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primary,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
		Multiplier:   in.Multiplier,
	}
	return contract, echo, routed, nil
}

func (s *Server) handleOptionQuoteSnapshot(ctx context.Context, c *ibkrlib.Connector, p rpc.QuoteSnapshotParams, timeout time.Duration) (*rpc.Quote, error) {
	contract, err := normaliseOptionQuoteContract(p.Contract)
	if err != nil {
		return nil, err
	}
	sym := contract.Symbol

	// Hold the underlying while the option line is open. IBKR's model-
	releaseUnder := func() {}
	if release, err := s.subs.Hold(ctx, sym); err == nil {
		releaseUnder = release
	} else if errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		return nil, err
	} else {
		s.logger.Debugf("quote.option underlying hold %s failed: %v", sym, err)
	}
	defer releaseUnder()

	tradingClass := strings.TrimSpace(contract.TradingClass)
	if tradingClass == "" {
		tradingClass = sym
	}
	key, _, err := c.SubscribeOption(ctx, sym, tradingClass, contract.Expiry, contract.Strike, contract.Right)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.UnsubscribeMarketData(key) }()

	q := &rpc.Quote{
		Symbol:   key,
		Contract: contract,
		IVStatus: "unavailable",
		AsOf:     time.Now(),
	}
	if err := pollUntilWithReject(ctx, time.Now().Add(timeout), c.SubscriptionRejectCh(key), key, func() bool {
		if d, ok := c.MarketDataSnapshot()[key]; ok {
			fillQuoteMarketData(q, d)
		}
		if bid, ask, ok := c.OptionQuoteBidAsk(key); ok {
			q.Bid = ptrIfPos(bid)
			q.Ask = ptrIfPos(ask)
		}
		if prev, ok := c.OptionPrevClose(key); ok {
			q.PrevClose = ptrIfPos(prev)
		}
		if iv, ok := c.OptionIV(key); ok && iv > 0 {
			q.IV = &iv
			q.IVStatus = "model"
		}
		if q.DataType == "" {
			q.DataType = marketDataTypeName(c.MarketDataTypeForSymbol(key))
		}
		return q.Bid != nil || q.Ask != nil || q.Last != nil || q.PrevClose != nil || q.IV != nil
	}); err != nil && err != context.DeadlineExceeded {
		// A terminal gateway rejection degrades to the same decorated
		if !IsSubscriptionRejected(err) {
			return nil, err
		}
		q.Stale = true
		q.StaleReason = err.Error()
	}
	q.AsOf = time.Now()
	s.attachQuoteSessionContext(q, marketcal.MarketUSOptions)
	s.decorateQuote(q, marketcal.MarketUSOptions)
	return q, nil
}

func quoteMarketForStockContract(c rpc.ContractParams) marketcal.Market {
	market := strings.ToLower(strings.TrimSpace(c.Market))
	exchange := strings.ToUpper(strings.TrimSpace(c.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	switch {
	case market == "de" || market == "germany" || market == "xetra" || market == "ibis":
		return marketcal.MarketDEXetra
	case exchange == "IBIS" || primary == "IBIS":
		return marketcal.MarketDEXetra
	default:
		return marketcal.MarketUSEquity
	}
}

func quoteSessionMarketForContract(c rpc.ContractParams) (marketcal.Market, bool) {
	if strings.EqualFold(strings.TrimSpace(c.SecType), "OPT") {
		return marketcal.MarketUSOptions, true
	}
	if !quoteHasRegularSessionCalendar(c) {
		return "", false
	}
	return quoteMarketForStockContract(c), true
}

func quoteHasRegularSessionCalendar(c rpc.ContractParams) bool {
	secType := strings.ToUpper(strings.TrimSpace(c.SecType))
	return secType == "" || secType == "STK" || secType == "ETF"
}

func (s *Server) attachQuoteSessionContext(q *rpc.Quote, market marketcal.Market) {
	if q == nil {
		return
	}
	session, err := marketcal.New().SessionAt(market, q.AsOf)
	if err != nil {
		if s.logger != nil {
			s.logger.Debugf("quote session context: %v", err)
		}
		return
	}
	priceMissing := q.Bid == nil && q.Ask == nil && q.Last == nil
	if session.IsOpen && rpc.IsLiveDataType(q.DataType) && !priceMissing {
		return
	}
	converted := marketSessionToRPC(session)
	q.SessionContext = &converted
}

func normaliseOptionQuoteContract(in rpc.ContractParams) (rpc.ContractParams, error) {
	sym := normSym(in.Symbol)
	if sym == "" {
		return rpc.ContractParams{}, errBadRequest("contract.symbol required")
	}
	expiry := strings.TrimSpace(in.Expiry)
	if len(expiry) != 8 {
		return rpc.ContractParams{}, errBadRequest("option contract.expiry must be YYYYMMDD")
	}
	if _, err := time.Parse("20060102", expiry); err != nil {
		return rpc.ContractParams{}, errBadRequest("option contract.expiry must be YYYYMMDD")
	}
	right := strings.ToUpper(strings.TrimSpace(in.Right))
	if right != "C" && right != "P" {
		return rpc.ContractParams{}, errBadRequest("option contract.right must be C or P")
	}
	if in.Strike <= 0 {
		return rpc.ContractParams{}, errBadRequest("option contract.strike must be positive")
	}
	out := in
	out.Symbol = sym
	out.SecType = "OPT"
	out.Expiry = expiry
	out.Right = right
	if out.Multiplier == 0 {
		out.Multiplier = 100
	}
	if out.Exchange == "" {
		out.Exchange = "SMART"
	}
	if out.Currency == "" {
		out.Currency = "USD"
	}
	return out, nil
}

// handleQuoteSubscribe attaches a fan-out tap to the daemon's per-symbol
// subscription registry so callers share one IBKR market-data line per symbol.
func (s *Server) handleQuoteSubscribe(ctx context.Context, req *rpc.Request, enc *json.Encoder, r *bufio.Reader) {
	var p rpc.QuoteSubscribeParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeError(enc, req.ID, rpc.CodeBadRequest, err.Error())
		return
	}
	if p.Contract.Symbol == "" {
		writeError(enc, req.ID, rpc.CodeBadRequest, "contract.symbol required")
		return
	}

	routeContract, echoedContract, routedQuote, err := normaliseStockQuoteContract(p.Contract)
	if err != nil {
		writeError(enc, req.ID, rpc.CodeBadRequest, err.Error())
		return
	}
	var frames <-chan rpc.Frame
	var release func()
	if routedQuote {
		frames, release, err = s.subs.SubscribeContract(ctx, routeContract)
	} else {
		frames, release, err = s.subs.Subscribe(ctx, echoedContract.Symbol)
	}
	if err != nil {
		if errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
			writeError(enc, req.ID, rpc.CodeGatewayUnavailable, err.Error())
			return
		}
		writeError(enc, req.ID, rpc.CodeInternal, err.Error())
		return
	}
	defer release()

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	s.streams[req.ID] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, req.ID)
		s.mu.Unlock()
	}()

	// EOF watcher: streaming clients are silent after the initial subscribe
	go func() {
		_, _ = r.ReadByte()
		cancel()
	}()

	for {
		select {
		case <-streamCtx.Done():
			_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
			return
		case frame, ok := <-frames:
			if !ok {
				// Manager torn the tap down (daemon_shutdown, gateway_lost, etc).
				_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
				return
			}
			if frame.Error == nil && (frame.Bid != nil || frame.Ask != nil || frame.Last != nil) {
				s.observeMarketDataWitness(frame.T)
			}
			buf, err := json.Marshal(frame)
			if err != nil {
				writeError(enc, req.ID, rpc.CodeInternal, err.Error())
				return
			}
			if err := enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, Frame: buf}); err != nil {
				return
			}
		}
	}
}

// fillQuoteMarketData projects the connector's current tick cache onto the
// Quote wire shape. Pointer fields preserve the nil-vs-zero contract: absent
func fillQuoteMarketData(q *rpc.Quote, d *ibkrlib.MarketData) {
	if q == nil || d == nil {
		return
	}
	q.Bid = ptrIfPos(d.Bid)
	q.Ask = ptrIfPos(d.Ask)
	q.Last = ptrIfPos(d.Last)
	q.Mark = ptrIfPos(d.MarkPrice)
	q.PrevClose = ptrIfPos(d.Close)
	q.DayHigh = ptrIfPos(d.High)
	q.DayLow = ptrIfPos(d.Low)
	q.Week52High = ptrIfPos(d.Week52High)
	q.Week52Low = ptrIfPos(d.Week52Low)
	q.BidSize = ptrIfPos(d.BidSize)
	q.AskSize = ptrIfPos(d.AskSize)
	q.Volume = ptrIfPos(d.Volume)
	q.AvgVolume = ptrIfPos(d.AvgVolume)
	if d.IV > 0 {
		v := d.IV
		q.IV = &v
		q.IVStatus = "model"
	}
	if !d.LastTradeTime.IsZero() {
		q.PriceAt = d.LastTradeTime
		q.QuotePriceAt = d.LastTradeTime
	}
}

func quoteNeedsHistoricalFallback(q *rpc.Quote) bool {
	if q == nil {
		return false
	}
	return q.Price == nil &&
		q.Bid == nil &&
		q.Ask == nil &&
		q.Last == nil &&
		q.Mark == nil
}

func quoteNeedsClosedMarketHistoricalContext(q *rpc.Quote, market marketcal.Market) bool {
	if q == nil || quoteMarketIsOpen(q, market) {
		return false
	}
	if q.Last != nil {
		return false
	}
	return q.Mark != nil || q.PrevClose != nil || q.Bid != nil || q.Ask != nil
}

func quoteNeedsHistoricalContext(q *rpc.Quote, market marketcal.Market) bool {
	if q == nil {
		return false
	}
	if quoteNeedsHistoricalFallback(q) || quoteNeedsClosedMarketHistoricalContext(q, market) {
		return true
	}
	if quoteMarketIsOpen(q, market) {
		return false
	}
	return q.RegularClose == nil ||
		q.DayHigh == nil ||
		q.DayLow == nil ||
		q.Week52High == nil ||
		q.Week52Low == nil ||
		q.AvgVolume == nil
}

func quoteNeedsHistoryForSession(q *rpc.Quote, market marketcal.Market, hasSessionMarket bool) bool {
	if !hasSessionMarket {
		return quoteNeedsHistoricalFallback(q)
	}
	return quoteNeedsHistoricalContext(q, market)
}

func (s *Server) fillQuoteHistoricalFallback(ctx context.Context, c *ibkrlib.Connector, q *rpc.Quote, market marketcal.Market, timeout time.Duration) []ibkrlib.HistoricalBar {
	if c == nil || q == nil || q.Symbol == "" {
		return nil
	}
	bars, err := s.fetchQuoteHistoricalBars(ctx, c, q, timeout, 400)
	if err != nil {
		if s.logger != nil {
			s.logger.Debugf("quote historical fallback %s: %v", q.Symbol, err)
		}
		return nil
	}
	applyQuoteHistoricalFallback(q, market, bars)
	return bars
}

func (s *Server) fetchQuoteHistoricalBars(ctx context.Context, c *ibkrlib.Connector, q *rpc.Quote, timeout time.Duration, lookbackDays int) ([]ibkrlib.HistoricalBar, error) {
	if c == nil || q == nil || q.Symbol == "" {
		return nil, fmt.Errorf("quote missing symbol")
	}
	fallbackCtx, cancel := context.WithTimeout(ctx, quoteHistoricalFallbackTimeout(timeout))
	defer cancel()
	bars, err := c.FetchHistoricalDailyBarsWithContract(fallbackCtx, quoteHistoricalContract(q), lookbackDays, 0)
	if err != nil {
		bars, err = c.FetchHistoricalDailyBars(fallbackCtx, q.Symbol, lookbackDays, 0)
	}
	return bars, err
}

func (s *Server) fillQuoteLiquidity(ctx context.Context, c *ibkrlib.Connector, q *rpc.Quote, market marketcal.Market, timeout time.Duration, bars []ibkrlib.HistoricalBar) {
	if q == nil {
		return
	}
	key := quoteLiquidityCacheKey(q)
	if cached, ok := s.quoteLiquidity.get(key, time.Now()); ok {
		applyQuoteLiquidityEntry(q, cached)
		return
	}
	if len(bars) == 0 {
		var err error
		bars, err = s.fetchQuoteHistoricalBars(ctx, c, q, timeout, 45)
		if err != nil {
			entry := quoteLiquidityEntry{status: "unavailable"}
			s.quoteLiquidity.put(key, entry, time.Now())
			applyQuoteLiquidityEntry(q, entry)
			if s.logger != nil {
				s.logger.Debugf("quote liquidity %s: %v", q.Symbol, err)
			}
			return
		}
	}
	liq := computeHistoricalLiquidity20D(bars)
	if liq.sampleDays == 0 {
		entry := quoteLiquidityEntry{status: "unavailable"}
		s.quoteLiquidity.put(key, entry, time.Now())
		applyQuoteLiquidityEntry(q, entry)
		return
	}
	entry := quoteLiquidityEntry{
		status:     "ok",
		source:     "daily_bars",
		sampleDays: liq.sampleDays,
		asOf:       liq.asOf,
	}
	if liq.avgVolume != nil {
		entry.avgVolume = *liq.avgVolume
	}
	if liq.avgDollarVolume != nil {
		entry.avgDollarVolume = *liq.avgDollarVolume
	}
	if liq.sampleDays < 20 {
		entry.status = "partial"
	}
	if entry.asOf.IsZero() {
		if last, ok := latestTechnicalBar(bars); ok {
			entry.asOf = marketCloseForHistoricalBar(market, last, q.AsOf)
		}
	}
	s.quoteLiquidity.put(key, entry, time.Now())
	applyQuoteLiquidityEntry(q, entry)
}

func quoteLiquidityCacheKey(q *rpc.Quote) quoteLiquidityKey {
	if q == nil {
		return quoteLiquidityKey{}
	}
	symbol := normSym(q.Contract.Symbol)
	if symbol == "" {
		symbol = normSym(q.Symbol)
	}
	return quoteLiquidityKey{
		symbol:   symbol,
		market:   strings.ToLower(strings.TrimSpace(q.Contract.Market)),
		exchange: normSym(q.Contract.Exchange),
		primary:  normSym(q.Contract.PrimaryExch),
		currency: normCcy(q.Contract.Currency),
	}
}

func applyQuoteLiquidityEntry(q *rpc.Quote, e quoteLiquidityEntry) {
	if q == nil {
		return
	}
	q.LiquidityStatus = e.status
	q.LiquiditySource = e.source
	q.LiquiditySampleDays = e.sampleDays
	q.LiquidityAsOf = e.asOf
	q.AvgVolume20D = ptrIfPos(e.avgVolume)
	q.AvgDollarVolume20D = ptrIfPos(e.avgDollarVolume)
}

func quoteHistoricalContract(q *rpc.Quote) ibkrlib.Contract {
	if q == nil {
		return ibkrlib.Contract{}
	}
	c := ibkrlib.Contract{
		Symbol:       q.Contract.Symbol,
		SecType:      q.Contract.SecType,
		Exchange:     q.Contract.Exchange,
		PrimaryExch:  q.Contract.PrimaryExch,
		Currency:     q.Contract.Currency,
		LocalSymbol:  q.Contract.LocalSymbol,
		TradingClass: q.Contract.TradingClass,
	}
	if c.Symbol == "" {
		c.Symbol = q.Symbol
	}
	if c.SecType == "" {
		c.SecType = "STK"
	}
	switch strings.ToLower(strings.TrimSpace(q.Contract.Market)) {
	case "de", "germany", "xetra", "ibis":
		if c.Exchange == "" {
			c.Exchange = "SMART"
		}
		if c.PrimaryExch == "" {
			c.PrimaryExch = "IBIS"
		}
		if c.Currency == "" {
			c.Currency = "EUR"
		}
	}
	return c
}

func quoteHistoricalFallbackTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 5 * time.Second
	}
	if timeout > 5*time.Second {
		return 5 * time.Second
	}
	return timeout
}

func applyQuoteHistoricalFallback(q *rpc.Quote, market marketcal.Market, bars []ibkrlib.HistoricalBar) {
	if q == nil || len(bars) == 0 {
		return
	}
	last := bars[len(bars)-1]
	if last.Close <= 0 {
		return
	}
	regularClose := last.Close
	q.RegularClose = &regularClose
	if t := marketCloseForHistoricalBar(market, last, q.AsOf); !t.IsZero() {
		q.RegularCloseAt = t
	}
	hasQuote := q.Last != nil || q.Mark != nil || q.Bid != nil || q.Ask != nil
	if hasQuote {
		q.PrevClose = &regularClose
	}
	if q.Last == nil && q.Bid == nil && q.Ask == nil {
		price := regularClose
		q.Price = &price
		q.PriceSource = "historical_close"
		q.DataType = rpc.MarketDataFrozen
		q.PriceAt = q.RegularCloseAt
	}
	if last.High > 0 && q.DayHigh == nil {
		v := last.High
		q.DayHigh = &v
	}
	if last.Low > 0 && q.DayLow == nil {
		v := last.Low
		q.DayLow = &v
	}
	if last.Volume > 0 && q.Volume == nil {
		v := last.Volume
		q.Volume = &v
	}
	if len(bars) >= 2 {
		prev := bars[len(bars)-2].Close
		if prev > 0 {
			q.PriorRegularClose = &prev
			if !hasQuote {
				q.PrevClose = &prev
			}
		}
	}
	if lo, hi := historicalRange(bars, 252); lo > 0 && hi > 0 && (q.Week52Low == nil || q.Week52High == nil) {
		q.Week52Low = &lo
		q.Week52High = &hi
	}
	if avg := averageHistoricalVolume(bars, 30); avg > 0 && q.AvgVolume == nil {
		q.AvgVolume = &avg
	}
}

func marketCloseForDate(market marketcal.Market, date string, at time.Time) time.Time {
	date = normalizeHistoricalDate(date)
	if date == "" && !at.IsZero() {
		date = at.Format("2006-01-02")
	}
	if date == "" {
		return time.Time{}
	}
	res, err := marketcal.New().Query(marketcal.Query{Market: market, Date: date, Days: 1})
	if err != nil || res.Session.Close.IsZero() {
		return time.Time{}
	}
	return res.Session.Close
}

func marketCloseForHistoricalBar(market marketcal.Market, bar ibkrlib.HistoricalBar, at time.Time) time.Time {
	if t := marketCloseForDate(market, bar.Date, at); !t.IsZero() {
		return t
	}
	if !bar.Time.IsZero() {
		if t := marketCloseForDate(market, bar.Time.Format("2006-01-02"), at); !t.IsZero() {
			return t
		}
		return bar.Time
	}
	return time.Time{}
}

func normalizeHistoricalDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if fields := strings.Fields(raw); len(fields) > 0 {
		raw = fields[0]
	}
	for _, layout := range []string{"2006-01-02", "20060102"} {
		if t, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return raw
}

func historicalRange(bars []ibkrlib.HistoricalBar, n int) (float64, float64) {
	if n <= 0 || len(bars) < n {
		n = len(bars)
	}
	start := len(bars) - n
	var lo, hi float64
	for _, b := range bars[start:] {
		if b.Low > 0 && (lo == 0 || b.Low < lo) {
			lo = b.Low
		}
		if b.High > hi {
			hi = b.High
		}
	}
	return lo, hi
}

func averageHistoricalVolume(bars []ibkrlib.HistoricalBar, n int) int64 {
	if n <= 0 || len(bars) < n {
		n = len(bars)
	}
	start := len(bars) - n
	var sum int64
	var count int64
	for _, b := range bars[start:] {
		if b.Volume <= 0 {
			continue
		}
		sum += b.Volume
		count++
	}
	if count == 0 {
		return 0
	}
	return sum / count
}

func (s *Server) decorateQuote(q *rpc.Quote, market marketcal.Market) {
	if q == nil {
		return
	}
	feedType := q.DataType
	if q.RegularClose == nil && q.PrevClose != nil && quoteStockLike(q) && quoteMarketIsOpen(q, market) {
		q.RegularClose = q.PrevClose
		q.RegularCloseAt = previousMarketCloseTime(market, q.AsOf)
	}
	q.QuotePrice, q.QuotePriceSource = quoteCurrentQuotePrice(q)
	q.QuotePriceAt = quotePriceTimeForSource(q, q.QuotePriceSource, q.QuotePrice, market)
	if q.RegularClose != nil && q.PriorRegularClose != nil {
		q.RegularChange, q.RegularChangePct = computeQuoteChange(q.RegularClose, q.PriorRegularClose)
	}
	if q.RegularClose != nil && q.QuotePrice != nil {
		q.QuoteChange, q.QuoteChangePct = computeQuoteChange(q.QuotePrice, q.RegularClose)
	}
	q.Price, q.PriceSource = quoteCurrentPrice(q)
	if q.QuotePrice != nil && q.RegularClose != nil {
		q.PrevClose = q.RegularClose
	} else if q.PriceSource == "historical_close" && q.PriorRegularClose != nil {
		q.PrevClose = q.PriorRegularClose
	}
	q.Change, q.ChangePct = computeQuoteChange(q.Price, q.PrevClose)
	q.PriceAt = quotePriceTime(q, market)
	if stale, reason := quoteStaleness(q, market); stale {
		q.Stale = true
		q.StaleReason = reason
	}
	q.DataType = quoteEffectiveDataType(q, market, feedType)
	if feedType != "" && feedType != q.DataType {
		q.FeedType = feedType
	} else {
		q.FeedType = ""
	}
	q.PriceAsOf = quotePriceAsOf(q, market)
	q.QuotePriceAsOf = quoteAsOfLabel(q, market, q.QuotePriceAt, q.QuotePriceSource, q.DataType)
	q.SpreadPct = quoteSpreadPct(q)
	q.VolumePhase = quoteVolumePhase(q, market)
	q.QuoteQuality = quoteQuality(q, market)
	q.Indicative = quoteIndicative(q, market)
	q.WarningDetails = quoteWarningDetails(q, market)
}

func quoteStockLike(q *rpc.Quote) bool {
	if q == nil {
		return false
	}
	secType := strings.ToUpper(strings.TrimSpace(q.Contract.SecType))
	return secType == "" || secType == "STK" || secType == "ETF"
}

func quoteCurrentPrice(q *rpc.Quote) (*float64, string) {
	if q == nil {
		return nil, ""
	}
	if q.Price != nil && q.PriceSource != "" {
		return q.Price, q.PriceSource
	}
	if q.QuotePrice != nil {
		return q.QuotePrice, q.QuotePriceSource
	}
	if q.RegularClose != nil {
		return q.RegularClose, "historical_close"
	}
	if q.PrevClose != nil {
		return q.PrevClose, "prev_close"
	}
	return nil, ""
}

func quoteCurrentQuotePrice(q *rpc.Quote) (*float64, string) {
	if q == nil {
		return nil, ""
	}
	if q.Last != nil {
		return q.Last, "last"
	}
	if q.Mark != nil {
		return q.Mark, "mark"
	}
	if q.Bid != nil && q.Ask != nil {
		v := (*q.Bid + *q.Ask) / 2
		return &v, "mid"
	}
	if q.Bid != nil {
		return q.Bid, "bid"
	}
	if q.Ask != nil {
		return q.Ask, "ask"
	}
	return nil, ""
}

func quoteSpreadPct(q *rpc.Quote) *float64 {
	if q == nil || q.Bid == nil || q.Ask == nil || *q.Bid <= 0 || *q.Ask <= 0 || *q.Ask < *q.Bid {
		return nil
	}
	mid := (*q.Bid + *q.Ask) / 2
	if mid <= 0 {
		return nil
	}
	v := (*q.Ask - *q.Bid) / mid * 100
	return &v
}

func quoteEffectiveDataType(q *rpc.Quote, market marketcal.Market, feedType string) string {
	if q == nil || q.Price == nil {
		return feedType
	}
	if q.PriceSource == "prev_close" || q.PriceSource == "historical_close" {
		return rpc.MarketDataPrevClose
	}
	session := quoteSessionFor(q, market)
	if session != nil {
		if quotePriceAtSessionClose(q, *session) && !session.IsOpen {
			return rpc.MarketDataFrozen
		}
	}
	if q.Stale {
		return rpc.MarketDataFrozen
	}
	if feedType != "" {
		return feedType
	}
	return rpc.MarketDataLive
}

func quotePriceBeforeSessionDate(q *rpc.Quote, session rpc.MarketSession) bool {
	if q == nil || q.PriceAt.IsZero() || session.Date == "" || session.Timezone == "" {
		return false
	}
	loc, err := time.LoadLocation(session.Timezone)
	if err != nil {
		return false
	}
	return q.PriceAt.In(loc).Format("2006-01-02") < session.Date
}

func quotePriceAtSessionClose(q *rpc.Quote, session rpc.MarketSession) bool {
	if q == nil || q.PriceAt.IsZero() || session.Close.IsZero() {
		return false
	}
	return q.PriceAt.Equal(session.Close)
}

func quoteQuality(q *rpc.Quote, market marketcal.Market) string {
	if q == nil || q.Price == nil {
		return "missing"
	}
	if q.DataType == rpc.MarketDataPrevClose {
		return "prev_close"
	}
	if q.Stale {
		return "stale"
	}
	if quoteSpreadIsWide(q) {
		return "wide"
	}
	if quoteOffHours(q, market) {
		return "indicative"
	}
	return "firm"
}

func quoteIndicative(q *rpc.Quote, market marketcal.Market) bool {
	if q == nil {
		return false
	}
	return quoteOffHours(q, market) || quoteSpreadIsWide(q) || q.DataType == rpc.MarketDataPrevClose
}

func quoteSpreadIsWide(q *rpc.Quote) bool {
	return q != nil && q.SpreadPct != nil && *q.SpreadPct > 2
}

func quoteOffHours(q *rpc.Quote, market marketcal.Market) bool {
	session := quoteSessionFor(q, market)
	return session != nil && !session.IsOpen
}

func quoteVolumePhase(q *rpc.Quote, market marketcal.Market) string {
	session := quoteSessionFor(q, market)
	if session == nil {
		return ""
	}
	if session.IsOpen {
		return "regular_session"
	}
	at := time.Now()
	if q != nil && !q.AsOf.IsZero() {
		at = q.AsOf
	}
	loc, err := time.LoadLocation(session.Timezone)
	if err == nil {
		local := at.In(loc)
		switch {
		case !session.Open.IsZero() && local.Before(session.Open.In(loc)):
			return "pre_market_or_prior_session"
		case !session.Close.IsZero() && !local.Before(session.Close.In(loc)):
			return "post_market_or_regular_session"
		}
	}
	return "closed_or_prior_session"
}

func quoteWarningDetails(q *rpc.Quote, market marketcal.Market) []rpc.DataWarning {
	if q == nil {
		return nil
	}
	var out []rpc.DataWarning
	scope := q.Symbol
	if scope == "" {
		scope = q.Contract.Symbol
	}
	switch q.DataType {
	case rpc.MarketDataPrevClose:
		out = append(out, rpc.DataWarning{
			Code:     "selected_price_prev_close",
			Scope:    scope,
			Severity: "data_quality",
			Message:  "Selected price is from a prior regular-session close.",
			Impact:   "Do not treat bid/ask/last context as a fresh regular-session trade signal.",
			Action:   "Retry during the regular session or use quote_quality/spread_pct as a gate.",
		})
	case rpc.MarketDataFrozen, rpc.MarketDataDelayedFrozen:
		if quoteOffHours(q, market) {
			out = append(out, rpc.DataWarning{
				Code:     "selected_price_closed_session",
				Scope:    scope,
				Severity: "info",
				Message:  "Selected price is from a closed regular session.",
				Impact:   "The value is suitable as stale context, not as an executable quote.",
			})
		}
	}
	if quoteSpreadIsWide(q) {
		out = append(out, rpc.DataWarning{
			Code:     "wide_spread",
			Scope:    scope,
			Severity: "data_quality",
			Message:  "Bid/ask spread is wide.",
			Impact:   "Liquidity gates should treat this as indicative until quotes tighten.",
			Action:   "Check spread_pct and retry during regular trading hours.",
		})
	}
	if quoteOffHours(q, market) {
		out = append(out, rpc.DataWarning{
			Code:     "off_hours_quote",
			Scope:    scope,
			Severity: "info",
			Message:  "Market is outside its regular session.",
			Impact:   "Quotes and volume may be thin, partial, or carried from the prior session.",
		})
	}
	return out
}

func quoteSessionFor(q *rpc.Quote, market marketcal.Market) *rpc.MarketSession {
	if q != nil && q.SessionContext != nil {
		return q.SessionContext
	}
	at := time.Now()
	if q != nil && !q.AsOf.IsZero() {
		at = q.AsOf
	}
	session, err := marketcal.New().SessionAt(market, at)
	if err != nil {
		return nil
	}
	converted := marketSessionToRPC(session)
	return &converted
}

func quoteFallbackReady(q *rpc.Quote, pollStarted time.Time, timeout time.Duration) bool {
	if q == nil || (q.Mark == nil && q.PrevClose == nil) {
		return false
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	grace := 750 * time.Millisecond
	if timeout < grace {
		grace = timeout / 2
	}
	if grace <= 0 {
		return true
	}
	return time.Since(pollStarted) >= grace
}

func quotePriceTime(q *rpc.Quote, market marketcal.Market) time.Time {
	if q == nil || q.Price == nil {
		return time.Time{}
	}
	return quotePriceTimeForSource(q, q.PriceSource, q.Price, market)
}

func quotePriceTimeForSource(q *rpc.Quote, source string, price *float64, market marketcal.Market) time.Time {
	if q == nil || price == nil {
		return time.Time{}
	}
	switch source {
	case "last":
		if quoteTickTimeUsable(q, price, q.PriceAt, market) {
			return q.PriceAt
		}
	case "mark":
		if quoteTickTimeUsable(q, price, q.PriceAt, market) {
			return q.PriceAt
		}
	case "mid", "bid", "ask":
		if !q.AsOf.IsZero() {
			return q.AsOf
		}
	case "prev_close":
		if t := previousMarketCloseTime(market, q.AsOf); !t.IsZero() {
			return t
		}
	case "historical_close":
		if !q.RegularCloseAt.IsZero() {
			return q.RegularCloseAt
		}
	}
	if !q.AsOf.IsZero() {
		return q.AsOf
	}
	return time.Now()
}

func quoteTickTimeUsable(q *rpc.Quote, price *float64, tickAt time.Time, market marketcal.Market) bool {
	if q == nil || price == nil || tickAt.IsZero() {
		return false
	}
	if q.RegularClose != nil && !q.RegularCloseAt.IsZero() && tickAt.Equal(q.RegularCloseAt) && !floatClose(*price, *q.RegularClose) {
		return false
	}
	session := quoteSessionFor(q, market)
	if session == nil || session.Timezone == "" {
		return true
	}
	if q.RegularClose != nil && quotePriceBeforeSessionDate(&rpc.Quote{PriceAt: tickAt}, *session) && !floatClose(*price, *q.RegularClose) {
		return false
	}
	return true
}

func floatClose(a, b float64) bool {
	return math.Abs(a-b) < 0.005
}

func previousMarketCloseTime(market marketcal.Market, at time.Time) time.Time {
	if at.IsZero() {
		at = time.Now()
	}
	cal := marketcal.New()
	session, err := cal.SessionAt(market, at)
	if err != nil {
		return time.Time{}
	}
	loc, err := time.LoadLocation(session.Timezone)
	if err != nil {
		return time.Time{}
	}
	local := at.In(loc)
	if (session.State == marketcal.StateRegular || session.State == marketcal.StateEarlyClose) &&
		!session.Close.IsZero() && !local.Before(session.Close) {
		return session.Close
	}
	for i := 1; i <= 14; i++ {
		day := local.AddDate(0, 0, -i).Format("2006-01-02")
		res, err := cal.Query(marketcal.Query{Market: market, Date: day, Days: 1})
		if err != nil {
			continue
		}
		s := res.Session
		if (s.State == marketcal.StateRegular || s.State == marketcal.StateEarlyClose) && !s.Close.IsZero() {
			return s.Close
		}
	}
	return time.Time{}
}

func quotePriceAsOf(q *rpc.Quote, market marketcal.Market) string {
	if q == nil || q.PriceAt.IsZero() {
		return ""
	}
	return quoteAsOfLabel(q, market, q.PriceAt, q.PriceSource, q.DataType)
}

func quoteAsOfLabel(q *rpc.Quote, market marketcal.Market, at time.Time, source, dataType string) string {
	if q == nil || at.IsZero() {
		return ""
	}
	loc := quoteMarketLocation(q, market)
	t := at
	if loc != nil {
		t = t.In(loc)
	}
	label := "As of"
	if source == "prev_close" || source == "historical_close" || dataType == rpc.MarketDataPrevClose {
		label = "At close"
	} else if dataType == rpc.MarketDataDelayedFrozen {
		if quoteMarketIsOpen(q, market) {
			label = "Delayed frozen"
		}
	} else if dataType == rpc.MarketDataFrozen {
		if quoteMarketIsOpen(q, market) {
			label = "Frozen"
		}
	} else if dataType == rpc.MarketDataDelayed {
		label = "Delayed"
	}
	return fmt.Sprintf("%s: %s", label, t.Format("Jan 2 at 03:04:05 PM MST"))
}

func quoteMarketLocation(q *rpc.Quote, market marketcal.Market) *time.Location {
	if q != nil && q.SessionContext != nil && q.SessionContext.Timezone != "" {
		if loc, err := time.LoadLocation(q.SessionContext.Timezone); err == nil {
			return loc
		}
	}
	at := time.Now()
	if q != nil && !q.AsOf.IsZero() {
		at = q.AsOf
	}
	session, err := marketcal.New().SessionAt(market, at)
	if err != nil || session.Timezone == "" {
		return nil
	}
	loc, err := time.LoadLocation(session.Timezone)
	if err != nil {
		return nil
	}
	return loc
}

func quoteStaleness(q *rpc.Quote, market marketcal.Market) (bool, string) {
	if q == nil || !quoteMarketIsOpen(q, market) {
		return false, ""
	}
	if q.PriceSource == "prev_close" {
		return true, "market is open but only previous close is available"
	}
	if q.DataType == rpc.MarketDataFrozen || q.DataType == rpc.MarketDataDelayedFrozen {
		return true, "market is open but quote data is frozen"
	}
	if q.PriceAt.IsZero() || q.AsOf.IsZero() {
		return false, ""
	}
	if age := q.AsOf.Sub(q.PriceAt); age > 15*time.Minute {
		return true, fmt.Sprintf("price timestamp is %s old during market hours", formatQuoteAge(age))
	}
	return false, ""
}

func quoteMarketIsOpen(q *rpc.Quote, market marketcal.Market) bool {
	if q != nil && q.SessionContext != nil {
		return q.SessionContext.IsOpen
	}
	at := time.Now()
	if q != nil && !q.AsOf.IsZero() {
		at = q.AsOf
	}
	session, err := marketcal.New().SessionAt(market, at)
	return err == nil && session.IsOpen
}

func formatQuoteAge(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	d = d.Round(time.Minute)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	if h > 0 && m > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if h > 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dm", m)
}

// computeQuoteChange returns (change, change_pct) pointers given a current
func computeQuoteChange(price, prevClose *float64) (*float64, *float64) {
	if price == nil || prevClose == nil || *prevClose <= 0 {
		return nil, nil
	}
	chg := *price - *prevClose
	pct := chg / *prevClose * 100
	return &chg, &pct
}

// marketDataTypeName maps the gateway's numeric data-type notice
// (1=RealTime, 2=Frozen, 3=Delayed, 4=DelayedFrozen) to a stable
// lower-case string used on the wire and in the CLI badge. Empty for
// unknown so callers can omit the field via omitempty.
func marketDataTypeName(t int) string {
	switch t {
	case 1:
		return rpc.MarketDataLive
	case 2:
		return rpc.MarketDataFrozen
	case 3:
		return rpc.MarketDataDelayed
	case 4:
		return rpc.MarketDataDelayedFrozen
	default:
		return ""
	}
}

func quoteDataTypeName(notice int, hasCurrentPrice, hasFallbackPrice bool) string {
	dt := marketDataTypeName(notice)
	if hasCurrentPrice {
		if dt != "" {
			return dt
		}
		return rpc.MarketDataLive
	}
	if hasFallbackPrice {
		switch dt {
		case rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen:
			return dt
		default:
			return rpc.MarketDataFrozen
		}
	}
	return dt
}

// handleStatusHealth describes daemon + gateway state for status command.
// Takes connector + endpoint snapshots under mu so all IBKR-side fields
// describe the same point in time even if reconnectFlow races with this
// 25s status wait loop to keep polling — so a user who just moved IBKR
// from Gateway (4001) to TWS (7496) gets recovery in a single `ibkr
func (s *Server) handleStatusHealth() *rpc.HealthResult {
	s.triggerReconnect()
	return s.statusHealthSnapshot()
}

// statusHealthSnapshot composes only daemon-owned cached state. Alert
// broker request. User-invoked status retains self-heal in handleStatusHealth.
func (s *Server) statusHealthSnapshot() *rpc.HealthResult {
	s.mu.Lock()
	ep := s.endpoint
	c := s.connector
	lastErr := s.lastConnectError
	connectInFlight := s.connectInFlight
	s.mu.Unlock()

	res := &rpc.HealthResult{
		DaemonVersion: s.version,
		DaemonStarted: s.startedAt,
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
		Account:       ep.Account,
		GatewayHost:   ep.Host,
		GatewayPort:   ep.Port,
		GatewayTLS:    ep.TLS,
		PortOrigin:    string(ep.PortOrigin),
		TLSOrigin:     string(ep.TLSOrigin),
		Alternates:    ep.Alternates,
		ClientID:      ep.ClientID,
		LastError:     lastErr,
	}
	accountForMode := ep.Account
	var farmStatuses []ibkrlib.DataFarmStatus
	setupComplete := s.postConnectSetupDone.Load()
	// The raw advertised value, kept local. Scope resolution below reasons about
	// the session's whole inventory and must keep seeing it; only the serialized
	// field is projected to one account, further down where the pin is known.
	advertisedAccount := ""
	if c != nil {
		advertisedAccount = strings.TrimSpace(c.AccountID())
		res.ConnectedAccount = advertisedAccount
		if advertisedAccount != "" {
			accountForMode = advertisedAccount
		}
		// Report IsReady, not IsConnected: the gateway being TCP-reachable
		// is not enough — handlers must be armed (post-handshake) for any
		// got stuck in the {ready=false, conn=true} state (overnight TWS
		// connection read-loop goroutine in pkg/ibkr independently of
		// postConnectSetup completes once, the daemon never re-enters
		res.Connected = c.IsReady() && setupComplete
		res.ServerVersion = c.ServerVersion()
		res.NegotiatedTLS = c.UsingTLS()
		farmStatuses = c.DataFarmStatuses()
		res.DataFarms = statusDataFarms(farmStatuses)
		res.MarketDataAccess = statusMarketDataAccess(c.MarketDataAbsences())
	}
	localConnected, apiReady, backendDown := false, false, false
	if c != nil {
		localConnected = c.IsConnected()
		apiReady = c.IsReady()
		link := c.BackendLink()
		backendDown = link.Down
		if backendDown {
			res.GatewayPhaseAt = link.ChangedAt
		}
		if link.Losses > 0 || link.Down {
			res.BackendLink = &rpc.BackendLinkHealth{
				Down:                      link.Down,
				ChangedAt:                 link.ChangedAt,
				Losses:                    link.Losses,
				LossesInMaintenanceWindow: link.LossesInMaintenance,
				LastOutageSeconds:         int64(link.LastOutage / time.Second),
				LongestOutageSeconds:      int64(link.LongestOutage / time.Second),
			}
		}
	}
	res.GatewayPhase = statusGatewayPhase(
		localConnected,
		apiReady,
		setupComplete,
		connectInFlight,
		backendDown,
		lastErr,
		time.Since(s.startedAt),
	)
	res.AccountMode = accountModeForStatus(ep.Port, accountForMode)

	// BackgroundTasks lists daemon-internal long-running computes
	// contention message ride, so the three surfaces never diverge.
	res.BackgroundTasks = s.backgroundTasks()
	res.Subsystems = s.subsystemHealth(res.Connected, farmStatuses)
	res.DataQuality = s.statusDataQuality()
	res.Members = s.membersHealth()
	res.Trading = *s.handleTradingStatus()
	configuredAccount := ""
	port := ep.Port
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
	}
	shadowScope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, advertisedAccount)
	// connected_account names one account or nothing. A login holding several
	// advertises the comma-joined managedAccounts list, which is a session
	// inventory rather than an account code, and every consumer that reads this
	// field wants the account the session is scoped to: the CLI already
	// resolution above keeps the raw value. A single-account login is
	// byte-identical; an unpinned multi-account login serves nothing, which is
	// honest — there is no account to name. Naming the inventory as well needs a
	// second field, and nothing on the wire wants one yet.
	if !brokerScopeAccountConcrete(res.ConnectedAccount) {
		res.ConnectedAccount = ""
		if pin := strings.TrimSpace(configuredAccount); brokerScopeAccountConcrete(pin) {
			res.ConnectedAccount = pin
		}
	}
	gatewayPhase := alertShadowGatewayPhaseForHealth(res.GatewayPhase, setupComplete, connectInFlight, time.Since(s.startedAt))
	observedAt := time.Now().UTC()
	if s.now != nil {
		observedAt = s.now().UTC()
	}
	s.observeDataHealthAlertShadow(res, shadowScope, gatewayPhase, observedAt)
	return res
}

// statusGatewayPhase classifies the exact connectivity boundary without
// parsing broker or dial error text. Startup grace preserves the existing
func statusGatewayPhase(localConnected, apiReady, setupComplete, connectInFlight, backendDown bool, lastError string, uptime time.Duration) string {
	if localConnected {
		if backendDown {
			return rpc.GatewayPhaseBackendLinkDown
		}
		if apiReady && setupComplete {
			return rpc.GatewayPhaseReady
		}
		return rpc.GatewayPhaseAPINotReady
	}
	if connectInFlight || (!setupComplete && strings.TrimSpace(lastError) == "" && uptime < alertShadowGatewayStartupGrace) {
		return rpc.GatewayPhaseConnecting
	}
	return rpc.GatewayPhasePortDown
}

func (s *Server) subsystemHealth(connected bool, farms []ibkrlib.DataFarmStatus) []rpc.SubsystemHealth {
	gatewayStatus := "ready"
	if !connected {
		gatewayStatus = "unavailable"
	}
	now := time.Now().UTC()
	if s.now != nil {
		now = s.now().UTC()
	}
	quoteWitnessCurrent := s.marketDataWitnessCurrent(now)
	out := []rpc.SubsystemHealth{
		s.authoritySubsystemHealth(),
		{Name: "watchlist", Status: "ready", Message: "list-only path is local; quote enrichment requires gateway"},
		statusSubsystemFromReadiness("quote", marketDataFarmReadiness(connected, farms, quoteWitnessCurrent, "quotes may time out")),
		statusSubsystemFromReadiness("history", historicalDataFarmReadiness(connected, farms)),
		s.edgeSubsystemHealth(),
		chainSubsystemHealth(connected, farms, quoteWitnessCurrent),
	}
	gamma := rpc.SubsystemHealth{Name: "gamma", Status: gatewayStatus}
	if s.zeroGamma != nil && s.zeroGamma.IsComputing() {
		gamma.Status = "computing"
		gamma.Message = "dealer gamma compute is fanning out option legs"
	}
	out = append(out, gamma)
	out = append(out, s.breadthSubsystemHealth(gatewayStatus))
	if sub, ok := s.proposalSubsystemHealth(); ok {
		out = append(out, sub)
	}
	if sub, ok := s.opportunitySubsystemHealth(); ok {
		out = append(out, sub)
	}
	return out
}

// breadthSubsystemHealth reports the breadth lane against the connection
func (s *Server) breadthSubsystemHealth(gatewayStatus string) rpc.SubsystemHealth {
	sub := rpc.SubsystemHealth{Name: "breadth", Status: gatewayStatus}
	if gatewayStatus == "ready" {
		if down, streak, lastAttempt := s.breadthLaneDown(); down {
			sub.Status = "degraded"
			sub.Message = "S&P 500 breadth runs on a second gateway connection and that connection is down; the primary connection is unaffected"
			if streak > 0 {
				sub.Message += fmt.Sprintf(" (%d redial attempts failed)", streak)
			}
			sub.LastError = "breadth_bulk_connector_down"
			// The dial attempt, never the next-retry time: a LastErrorAt
			// ahead of the health snapshot's as_of fails the whole
			sub.LastErrorAt = lastAttempt
			return sub
		}
	}
	if gatewayStatus == "ready" {
		s.mu.Lock()
		lane := s.breadthConnector
		s.mu.Unlock()
		if farm, impaired := breadthLaneFarmImpaired(lane); impaired {
			sub.Status = "degraded"
			sub.Message = fmt.Sprintf("S&P 500 breadth is deferred: historical data farm %s is %s on the breadth connection", farm.Name, farm.Status)
			sub.LastError = "breadth_hmds_farm_impaired"
			sub.LastErrorAt = farm.AsOf
			return sub
		}
	}
	if s.breadth != nil && s.breadth.IsBusy() {
		sub.Status = "computing"
		sub.Message = "S&P 500 breadth refresh is running or waiting to retry"
		if s.breadth.CoverageShort() {
			cov, mc := s.breadth.LastRefreshCoverage()
			sub.Message = fmt.Sprintf("S&P 500 breadth refresh is retrying: last pass covered %d/%d constituents, below the publication threshold", cov, mc)
		}
	}
	return sub
}

// proposalSubsystemHealth reports the protection-proposal engine's refresh
// refresh failures (err == nil, served as_of frozen), so the gateway can
// daemon start race the gateway connect by design.
func (s *Server) proposalSubsystemHealth() (rpc.SubsystemHealth, bool) {
	if s.tradeProposals == nil {
		return rpc.SubsystemHealth{}, false
	}
	if s.cfg != nil && !s.cfg.AutoTrade.WithDefaults().ProposalsEnabledResolved() {
		return rpc.SubsystemHealth{Name: "proposals", Status: "disabled", Message: "manual protection proposals are disabled by config"}, true
	}
	sub := rpc.SubsystemHealth{Name: "proposals", Status: "ready"}
	h := s.tradeProposals.RefreshHealth()
	if h.Streak >= proposalRefreshWarnStreak {
		sub.Status = "degraded"
		sub.Message = fmt.Sprintf("refresh blocked %d consecutive times since %s; serving snapshot as_of %s",
			h.Streak, h.Since.Format(time.RFC3339), h.ServedAsOf.Format(time.RFC3339))
		sub.LastError = strings.Join(h.Codes, ",")
		sub.LastErrorAt = h.Since
	}
	return sub, true
}

func (s *Server) opportunitySubsystemHealth() (rpc.SubsystemHealth, bool) {
	if s.opportunities == nil {
		return rpc.SubsystemHealth{}, false
	}
	if s.cfg != nil && !s.cfg.Opportunities.WithDefaults().EnabledResolved() {
		return rpc.SubsystemHealth{Name: "opportunities", Status: "disabled", Message: "opportunities are disabled by config"}, true
	}
	sub := rpc.SubsystemHealth{Name: "opportunities", Status: "ready"}
	if s.opportunityPolicies != nil {
		policy := s.opportunityPolicies.Status()
		if policy.PolicyID != "" {
			sub.Message = fmt.Sprintf("policy %s v%d %s %s", policy.PolicyID, policy.PolicyVersion, policy.Status, policy.Fingerprint.Key)
		}
		if policy.Status == rpc.OpportunityPolicyStatusDrift || policy.Status == rpc.OpportunityPolicyStatusError {
			sub.Status = "degraded"
			sub.LastError = policy.Message
			sub.LastErrorAt = policy.LastCheckedAt
		}
	}
	h := s.opportunities.RefreshHealth()
	if h.Streak >= proposalRefreshWarnStreak {
		sub.Status = "degraded"
		refreshMessage := fmt.Sprintf("refresh blocked %d consecutive times since %s; serving snapshot as_of %s",
			h.Streak, h.Since.Format(time.RFC3339), h.ServedAsOf.Format(time.RFC3339))
		if sub.Message != "" {
			sub.Message += "; " + refreshMessage
		} else {
			sub.Message = refreshMessage
		}
		sub.LastError = strings.Join(h.Codes, ",")
		sub.LastErrorAt = h.Since
	}
	return sub, true
}

// membersHealth assembles the rpc.MembersHealth wire shape for the
// status response. Source is "cache" when the engine loaded from the
// runtime-refreshed file, "embedded" otherwise. RefreshState reflects
// the refresher's current health, or empty when the refresher is
// disabled / nil (the CLI uses Source alone to render the row).
func (s *Server) membersHealth() rpc.MembersHealth {
	if s.breadth == nil {
		return rpc.MembersHealth{}
	}
	current := s.breadth.Members()
	mh := rpc.MembersHealth{
		Source: "embedded",
		AsOf:   sp500EmbeddedAsOf(),
		Count:  len(current),
	}
	// Prefer the runtime-refreshed file as the source signal when it
	// exists and parses cleanly. A stale file (older than the embedded
	// baseline) still counts as "cache" — the user sees the date and
	// can decide if it's stale; we don't second-guess them.
	if s.membersCachePath != "" {
		if _, asOf, ok := spx.LoadExternal(s.membersCachePath); ok {
			mh.Source = "cache"
			mh.AsOf = asOf
		}
	}
	if s.membersRefresher != nil {
		mh.RefreshState = string(s.membersRefresher.State())
	}
	return mh
}

// sp500EmbeddedAsOf returns the asOf of the embedded list. Wrapped in
// a helper so the per-call type-cast stays out of the status hot path.
func sp500EmbeddedAsOf() time.Time {
	_, asOf := spx.MemberList()
	return asOf
}

// handleMarketCalendar returns official exchange-session context for the
// supported first-release markets: U.S. cash equities, U.S. listed options,
// and Xetra cash equities.
func (s *Server) handleMarketCalendar(req *rpc.Request) (*rpc.MarketCalendarResult, error) {
	var p rpc.MarketCalendarParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	market, ok := marketcal.NormalizeMarket(p.Market)
	if !ok {
		return nil, errBadRequest(fmt.Sprintf("unsupported market %q (supported: us, us-options, de)", p.Market))
	}
	res, err := marketcal.New().Query(marketcal.Query{
		Market: market,
		Date:   p.Date,
		At:     p.At,
		Days:   p.Days,
	})
	if err != nil {
		return nil, errBadRequest(err.Error())
	}
	out := &rpc.MarketCalendarResult{
		Market:        string(res.Market),
		Label:         res.Label,
		Timezone:      res.Timezone,
		AsOf:          res.AsOf,
		CoverageStart: res.CoverageStart,
		CoverageEnd:   res.CoverageEnd,
		Source:        res.Source,
		SourceURL:     res.SourceURL,
		Session:       marketSessionToRPC(res.Session),
		Sessions:      make([]rpc.MarketSession, 0, len(res.Sessions)),
	}
	for _, s := range res.Sessions {
		out.Sessions = append(out.Sessions, marketSessionToRPC(s))
	}
	return out, nil
}

func marketSessionToRPC(s marketcal.Session) rpc.MarketSession {
	return rpc.MarketSession{
		Market:        string(s.Market),
		Label:         s.Label,
		Date:          s.Date,
		Timezone:      s.Timezone,
		State:         string(s.State),
		IsOpen:        s.IsOpen,
		Reason:        s.Reason,
		Open:          s.Open,
		Close:         s.Close,
		NextOpen:      s.NextOpen,
		NextClose:     s.NextClose,
		Source:        s.Source,
		SourceURL:     s.SourceURL,
		CoverageStart: s.CoverageStart,
		CoverageEnd:   s.CoverageEnd,
		Notes:         s.Notes,
	}
}

// handleBreadthSPX returns the current S&P 500 stocks-above-50DMA reading
// plus a trailing daily series for sparkline rendering. The headline
// number is the percentage of S&P 500 constituents trading above their
// own 50-day SMA, in [0, 100].
//
// Methodology — spx.MethodConstituentFanout: we compute S5FI locally
// from constituent daily closes pulled via IBKR's HMDS feed. IBKR
// does not redistribute S&P DJI's S5FI index on retail subscriptions
// (verified via reqContractDetails — see pkg/ibkr/symbols.go), so the
// daemon reproduces the math from data it already has access to.
// The handler is a thin projection of the engine state onto the wire.
func (s *Server) handleBreadthSPX(_ context.Context, req *rpc.Request) (*rpc.BreadthSPXResult, error) {
	return s.buildBreadthSPX(req, true)
}

func (s *Server) buildBreadthSPX(req *rpc.Request, allowRefresh bool) (*rpc.BreadthSPXResult, error) {
	var p rpc.BreadthSPXParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	historyDays := p.HistoryDays
	if historyDays <= 0 {
		historyDays = 30
	}
	if historyDays > 90 {
		historyDays = 90
	}

	if s.breadth == nil {
		// Engine construction failed at New (e.g. unresolvable cache
		// dir). Match the pre-engine wire contract: surface as
		// gateway-unavailable so clients render a consistent "daemon
		// I/O dependency missing" state.
		return nil, ibkrlib.ErrIBKRUnavailable
	}

	// Opportunistic refresh trigger: on the first breadth call after
	// the NY-date rolls over, kick the members refresher if its
	// on-disk file is stale. Belt-and-suspenders against the 02:30
	// ET ticker missing (network outage, daemon paused). No-op when
	// the refresher is pinned off, or when the loaded file is
	// already from today, or when a fetch is already in flight
	// (singleflighted by the refresher).
	if allowRefresh && s.membersRefresher != nil && s.serverCtx != nil {
		s.membersRefresher.TriggerIfRolledOver(s.serverCtx)
	}

	res := &rpc.BreadthSPXResult{
		Source: "Computed from S&P-500 constituent daily bars (IBKR HMDS)",
		Method: spx.MethodConstituentFanout,
		AsOf:   time.Now(),
	}

	snap, ok := s.breadth.Get()
	active := s.breadth.IsBusy()
	res.State = classifyBreadthState(ok, active, s.breadth.CoverageShort())
	res.Refreshing = ok && active
	if progress, exists := s.breadth.Progress(); exists {
		res.Refresh = &rpc.BreadthRefreshProgress{
			SessionKey:  progress.SessionKey,
			StartedAt:   progress.StartedAt,
			Processed:   progress.Processed,
			Failed:      progress.Failed,
			Total:       progress.Total,
			Deadline:    progress.Deadline,
			LastFailure: rpc.BreadthRefreshFailure(progress.LastFailure),
		}
		if next, exists := s.breadth.NextAttempt(); exists {
			res.Refresh.NextAttempt = next
		}
	}

	if ok {
		res.PctAbove50DMA = snap.PctAbove50DMA
		res.PctAbove200DMA = snap.PctAbove200DMA
		res.NewHighsToday = snap.NewHighsToday
		res.NewLowsToday = snap.NewLowsToday
		res.NetNewHighsPct = snap.NetNewHighsPct
		res.AsOf = snap.AsOf
		res.SessionKey = snap.SessionKey
		res.Stale = breadthEnvelopeStale(res, time.Now())

		history := s.breadth.History(historyDays)
		res.History = make([]rpc.BreadthDailyValue, 0, len(history))
		for _, h := range history {
			res.History = append(res.History, rpc.BreadthDailyValue{
				Date:           h.Date,
				PctAbove50DMA:  h.PctAbove50DMA,
				PctAbove200DMA: h.PctAbove200DMA,
				NewHighs:       h.NewHighs,
				NewLows:        h.NewLows,
			})
		}
	}
	return res, nil
}

// classifyBreadthState projects the engine's (snapshot exists, refresh
// active, coverage-short) triple onto the wire-visible BreadthState.
//   - degraded: a snapshot is being served but the most recent completed
//     refresh pass stayed below the publication coverage threshold — the
//     served reading is last-good, not the current session's convergence.
//   - ready: a snapshot is served and the last pass met coverage.
//   - computing: no snapshot yet, refresh in flight or waiting to retry.
//   - cold: no snapshot AND no active refresh/retry.
func classifyBreadthState(snapshotExists, active, coverageShort bool) rpc.BreadthState {
	switch {
	case snapshotExists && coverageShort:
		return rpc.BreadthStateDegraded
	case snapshotExists:
		return rpc.BreadthStateReady
	case active:
		return rpc.BreadthStateComputing
	default:
		return rpc.BreadthStateCold
	}
}

// barDate returns the bar's date as YYYY-MM-DD. IBKR's daily bar dates arrive
func barDate(b ibkrlib.HistoricalBar) string {
	if !b.Time.IsZero() {
		return b.Time.Format("2006-01-02")
	}
	if len(b.Date) == 8 {
		return b.Date[:4] + "-" + b.Date[4:6] + "-" + b.Date[6:8]
	}
	return b.Date
}

// errBadRequest tags a typed error so dispatch can map it to CodeBadRequest
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &badRequestError{msg: msg} }

// decodeParams unmarshals req.Params into dst and tags failures as bad-request
func decodeParams[T any](raw json.RawMessage, dst *T) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return errBadRequest("decode params: " + err.Error())
	}
	return nil
}

// strikeStep picks a sensible strike interval based on spot. Mirrors common
// IBKR option spacings; refined chains use whatever IBKR returns.
func strikeStep(spot float64) float64 {
	switch {
	case spot < 25:
		return 1
	case spot < 100:
		return 2.5
	case spot < 250:
		return 5
	default:
		return 10
	}
}

func normalizeExpiry(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch len(s) {
	case 8: // YYYYMMDD
		return s, nil
	case 10: // YYYY-MM-DD
		return s[:4] + s[5:7] + s[8:], nil
	default:
		return "", fmt.Errorf("expiry must be YYYY-MM-DD or YYYYMMDD")
	}
}

func daysUntil(expiryYMD string) int {
	return daysUntilFrom(expiryYMD, time.Now())
}

func daysUntilFrom(expiryYMD string, now time.Time) int {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	t, err := time.ParseInLocation("20060102", expiryYMD, ny)
	if err != nil {
		return 0
	}
	y, m, d := now.In(ny).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	expiry := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return int(expiry.Sub(today).Hours() / 24)
}

func validChainSide(side string) bool {
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "calls", "puts", "both":
		return true
	default:
		return false
	}
}
