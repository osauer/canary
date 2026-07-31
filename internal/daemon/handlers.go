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
)

// handleAccountSummary issues a one-shot reqAccountSummary and converts the
// result into the wire shape exposed to the CLI.
// dailyPnLStaleGrace is how old the account daily-P&L frame may be before
// handleAccountSummary asks the connector to self-heal the reqPnL stream (see
// Connector.MaybeResubscribeStaleDailyPnL). Matches the connector's own
// staleness window; the daemon pre-checks it to skip the calendar lookup on the
// common fresh path.
const dailyPnLStaleGrace = 90 * time.Second

func (s *Server) handleAccountSummary(ctx context.Context) (*rpc.AccountResult, error) {
	return s.buildAccountSummary(ctx, true)
}

type accountSummaryAuthority struct {
	Provenance              ibkrlib.AccountSummaryProvenance
	AsOf                    time.Time
	NetLiquidationAvailable bool
	TotalCashAvailable      bool
	BaseCurrencyAvailable   bool
}

// buildAccountSummary shares the account wire builder with read-only daemon
// compositions. observe=false suppresses the risk-capital observation write;
// the returned typed account result is otherwise identical.
func (s *Server) buildAccountSummary(ctx context.Context, observe bool) (*rpc.AccountResult, error) {
	result, _, err := s.buildAccountSummaryWithAuthority(ctx, observe)
	return result, err
}

// buildAccountSummaryWithAuthority preserves the public account fallback but
// also returns the typed availability and source provenance strict consumers
// need. A cached fallback is usable display context, never fresh Rulebook
// evidence.
func (s *Server) buildAccountSummaryWithAuthority(ctx context.Context, observe bool) (*rpc.AccountResult, accountSummaryAuthority, error) {
	c := s.gatewayConnector()
	if c == nil {
		return nil, accountSummaryAuthority{}, s.gatewayUnavailableError()
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
	authority := accountSummaryAuthority{
		Provenance: provenance, AsOf: observedAt.UTC(),
		NetLiquidationAvailable: raw.NetLiquidation != nil,
		TotalCashAvailable:      raw.TotalCashValue != nil,
		BaseCurrencyAvailable:   strings.TrimSpace(raw.Currency) != "",
	}
	res := &rpc.AccountResult{
		AccountID:    raw.AccountID,
		AccountType:  raw.AccountType,
		BaseCurrency: raw.Currency,
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
	ledger := s.repairCurrencyLedgerFXRatesCached(ctx, c, raw.CurrencyLedger, res.BaseCurrency)
	res.CurrencyExposure = buildCurrencyExposure(ledger, res.BaseCurrency)
	// Daily P&L: read the connector's most-recent reqPnL frame. ok=false
	// before the first frame arrives — leave pointers nil (no fabrication).
	// Subscribe lazily on the first call when the cache is empty: post-
	// connect setup skips the subscribe in auto-detect mode (ep.Account is
	// empty until the gateway emits managedAccounts after handshake), so
	// the first `account` call doubles as the kickoff. SubscribeAccountPnL
	// is idempotent — subsequent calls for the same account are no-ops. After
	// kicking the subscription, wait briefly so cold-start Stress does not
	// falsely report missing P&L while the first frame is in flight.
	snap, ok := c.AccountDailyPnL()
	if !ok {
		if account := s.cachedAccount(); account != "" {
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
		// Self-heal a silently-stalled reqPnL stream. The gateway can stop
		// feeding the daily-P&L subscription (observed after a pre-market
		// subscribe that never resumed post-open) while account-updates keep
		// flowing — NLV stays live but daily P&L freezes. SubscribeAccountPnL is
		// idempotent and cannot revive it, so the connector force-rebuilds, gated
		// on market hours (off-hours the stream idles legitimately) and throttled.
		// When it fires, wait briefly for the fresh frame before returning.
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
	scope := s.currentBrokerStateScope()
	pnlSource := string(scope.Mode) + "|" + strings.TrimSpace(scope.Account)
	pnlObservation, pnlObservationErr := s.dailyPnLObservations.observe(context.Background(), pnlSource, pnlSessionKey, pnlNow, pnlDue, snap, ok || !snap.AsOf.IsZero())
	if pnlObservationErr != nil {
		s.logger.Warnf("Daily P&L observation authority degraded: %v", pnlObservationErr)
	}
	res.DailyPnLObservation = &pnlObservation
	// Feed the risk-constitution capital state: observation cadence is
	// usage cadence — every successful account read updates the cash-flow-
	// adjusted peak and drawdown tier; there is deliberately no scheduler
	// in v1. Skipped on a base-currency mismatch: capital math in the
	// wrong currency is worse than none.
	if observe && s.riskCapital != nil && res.NetLiquidation > 0 {
		var pol *risk.Constitution
		if s.riskPolicies != nil {
			pol = s.riskPolicies.snapshot().policy
		}
		if pol == nil || pol.Capital.BaseCurrency == "" || res.BaseCurrency == "" ||
			strings.EqualFold(pol.Capital.BaseCurrency, res.BaseCurrency) {
			if firstDailyObservation := s.riskCapital.Observe(res.NetLiquidation, res.AsOf, pol, s.currentBrokerStateScope()); firstDailyObservation {
				// The broker report can arrive before today's runtime account
				// value. Re-evaluate once when that missing daily counterpart
				// first appears; the report-id gate keeps automatic extension
				// idempotent.
				s.evaluateRiskPolicyV3Reconciliation()
			}
		}
	}
	return res, authority, nil
}

// accountResultAuthorityAsOf keeps display context separate from decision
// authority. Cached account-update rows are reparsed and stamped at read time,
// so only a completed one-shot request has an observation time that consumers
// may use as current evidence.
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
// Drops the row whose currency matches the account base (it duplicates
// the top-level totals and exposure is by definition "non-base") and
// also drops rows with missing/invalid exchange rates. When the caller
// didn't supply a base, rows whose ExchangeRate is exactly 1.0 are dropped
// as a defense-in-depth fallback because the row may be the base currency.
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
		// ExchangeRate==1 fallback for accounts where the base
		// currency couldn't be resolved upstream.
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

// handlePositionsList fetches all positions, splits stocks vs options, and
// applies the optional symbol/type filter.
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
	if brokerScopeConcrete(expectedScope) {
		var scoped bool
		health, scoped = scopedPortfolioStreamHealth(positions, health, expectedScope, time.Now())
		if !scoped {
			if portfolioHealth != nil {
				*portfolioHealth = health
			}
			return nil, errors.New("portfolio stream account scope conflict")
		}
	}
	if portfolioHealth != nil {
		*portfolioHealth = health
	}
	if err != nil {
		return nil, err
	}

	res := &rpc.PositionsResult{
		AsOf:    time.Now(),
		Stocks:  []rpc.PositionView{},
		Options: []rpc.PositionView{},
	}
	wantSym := normSym(p.Symbol)
	wantType := strings.ToLower(strings.TrimSpace(p.Type))

	// conIDByPositionKey lets fillDailyPnL look up the IBKR conId for
	// each rendered view without threading it through PositionView (which
	// stays focused on the user-visible wire shape). Key is built by
	// positionViewKey so it survives sort + group passes.
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
		multiplier := max(pos.Contract.Multiplier, 1)
		// Stocks may carry a multiplier of 100 in the raw wire row; the
		// wire-shape contract on PositionView reports per-share semantics
		// for stocks (multiplier 1).
		if !isOpt && multiplier == 100 {
			multiplier = 1
		}
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
			MarketValue:   pos.MarketPrice * pos.Position * float64(multiplier),
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

	// Pre-warm quote summaries for held stock underlyings, then fill
	// DayChange/DayChangePct on each row. This supersedes the older
	// prev-close-only probe for stock rows while still feeding the same
	// prev-close cache options use as their underlying anchor.
	s.prewarmStockQuoteSummaries(ctx, c, res.Stocks)
	flagZeroValueStockPositions(res.Stocks)
	s.fillDailyChange(res.Stocks)
	// Options group with their underlying so stock prev close feeds the
	// option's underlying field too — useful as a contextual anchor even
	// though we don't compute per-option DayChange yet.
	s.fillOptionUnderlyingPrevClose(res.Options)
	// Greeks: brief subscribe to each option leg, harvest model-
	// computation tick within budget, fill per-leg Delta/Gamma/Theta/
	// Vega. Same bounded fan-out and TTL-cached pattern as prev close.
	s.prewarmOptionGreeks(ctx, c, res.Options)
	s.fillOptionGreeks(c, res.Options)
	// Option day-change-money runs after Greeks because fillOptionGreeks
	// is where OptionPrevClose is read from the per-leg tick stream.
	fillOptionDayChangeMoney(res.Options)

	// FX/base-currency decoration: prefer the per-currency snapshot
	// maintained by the daemon's reqAccountUpdates subscription. If that
	// startup cache lacks account-base, NLV, or a held non-base currency,
	// take one bounded account-summary refresh before filling FXRate /
	// *_base fields; otherwise downstream math would use a partial ledger.
	rawAccount := c.AccountSummaryRaw()
	baseCcy := normCcy(baseCurrencyFromRaw(rawAccount))
	netLiquidationBase := netLiquidationBaseFromRaw(rawAccount, baseCcy)
	ledger := s.repairCurrencyLedgerFXRatesCached(ctx, c, c.CurrencyLedgerSnapshot(), baseCcy)
	missing := missingPositionFXCurrencies(res.Stocks, res.Options, ledger, baseCcy)
	if baseCcy == "" || netLiquidationBase == nil || len(missing) > 0 {
		if raw, err := c.RequestAccountSummary(ctx, 3*time.Second); err == nil {
			if baseCcy == "" {
				baseCcy = normCcy(raw.Currency)
			}
			if baseCcy == "" {
				baseCcy = normCcy(baseCurrencyFromRaw(raw.Raw))
			}
			if netLiquidationBase == nil {
				netLiquidationBase = raw.NetLiquidation
			}
			freshLedger := s.repairCurrencyLedgerFXRatesCached(ctx, c, raw.CurrencyLedger, baseCcy)
			ledger = mergeCurrencyLedgers(freshLedger, ledger)
		} else {
			s.logger.Debugf("positions FX ledger refresh failed for %v: %v", missing, err)
		}
	}
	fillFXRates(res.Stocks, ledger, baseCcy)
	fillFXRates(res.Options, ledger, baseCcy)

	// Daily P&L: kick off reqPnLSingle subscriptions (idempotent — the
	// connector cache shorts repeated calls) and fill view.DailyPnL from
	// whatever the connector has cached so far. First call after daemon
	// startup pre-warms; subsequent calls within a few seconds read fresh
	// values. Nil pointer means "no frame yet" / "no entitlement" /
	// "DBL_MAX sentinel" — never zero-substituted.
	s.fillDailyPnL(c, res.Stocks, conIDByPositionKey)
	s.fillDailyPnL(c, res.Options, conIDByPositionKey)
	fillBaseValues(res.Stocks, baseCcy)
	fillBaseValues(res.Options, baseCcy)
	flagClosedOptionSession(res.Options, res.AsOf)
	flagOptionMarkOutsideBidAsk(res.Options)

	res.ByUnderlying = groupByUnderlying(res.Stocks, res.Options, baseCcy, netLiquidationBase)
	res.Portfolio = buildPortfolioAggregatesWithBase(res.Stocks, res.Options, baseCcy)
	addPortfolioBaseContext(res.Portfolio, res.ByUnderlying, baseCcy, netLiquidationBase)
	addFXSensitivity(res.Portfolio, ledger, baseCcy)
	s.attachProtectionCoverage(ctx, res, wantSym, wantType, health)
	authorityScope := expectedScope
	if !sessionOK || !c.SessionCurrent(session) || !sameBrokerScope(expectedScope, s.currentBrokerStateScope()) {
		authorityScope = brokerStateScope{}
	}
	res.AsOf = positionsResultAuthorityAsOf(authorityScope, health, time.Now().UTC())
	return res, nil
}

// positionsResultAuthorityAsOf stamps only a complete, current, account-scoped
// portfolio projection. Cached rows remain useful context when the stream is
// unprimed or stale, but a zero timestamp makes that uncertainty explicit to
// Stress and every other consumer instead of laundering the handler time into
// a fresh empty-book receipt.
func positionsResultAuthorityAsOf(scope brokerStateScope, health ibkrlib.PortfolioStreamHealth, completedAt time.Time) time.Time {
	if classifyPortfolioStreamHealth(scope, health, completedAt.UTC()) != orderIntegrityHealthCurrent {
		return time.Time{}
	}
	return portfolioStreamEvidenceAsOf(health)
}

// positionStockQuoteBudget is the per-symbol wait for enriching held stock
// positions with the same quote summary fields watchlist/quote render.
// Short by design: positions already have portfolio marks, so this path is
// opportunistic context, not the source of truth for holdings.
const positionStockQuoteBudget = 1500 * time.Millisecond

// prewarmStockQuoteSummaries dispatches brief refcounted market-data holds
// for held stock rows and copies quote-context fields onto the position
// views. It also writes the latest completed regular-session close into the
// existing cache so fillDailyChange and option-underlying context reuse the
// same value.
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
		sym := normSym(stocks[i].Symbol)
		if sym == "" || seen[sym] {
			continue
		}
		// Zero-value rows are probed too: the probe is the evidence that
		// either disproves inactivity (live quote) or reaches the broker's
		// terminal verdict; the connector's inactive marks keep repeat
		// probes of genuinely dead names cheap.
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
// usable as a map key to associate auxiliary state (conId, daily P&L
// pointer) without threading those fields through the wire shape. Two
// views built from the same underlying position produce the same key.
//
// Every identity field participates. SecType namespaces the security
// types against each other, and ConID separates contracts that share a
// Symbol: IB repeats a base symbol across treasury issues, and futures
// repeat one across expiries, so Symbol alone is not an identity.
func positionViewKey(v rpc.PositionView) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%.4f", v.SecType, v.Symbol, v.ConID, v.Expiry, v.Right, v.Strike)
}

// maxDailyPnLSubscriptions caps the per-positions-call fan-out of
// reqPnLSingle subscriptions. IBKR doesn't document a hard limit, but
// community reporting puts the gateway's tolerance around 50 streams;
// accounts with more positions than that get daily P&L on the first 50
// and nil on the rest. Honest, not silent-zero. Renders as em-dash.
const maxDailyPnLSubscriptions = 50

// fillDailyPnL subscribes (if needed) to reqPnLSingle for each row's
// conId and copies the connector's most-recent cached value into
// view.DailyPnL. Idempotent — repeat invocations within a single
// positions call (stocks then options) build on the same cache.
//
// Two branches per row:
//   - cache already populated → just copy the value
//   - cache empty → subscribe (if we have an account and we're under
//     maxDailyPnLSubscriptions), copy nil
//
// Subscribing requires an account; if account is unknown the
// subscribe branch is skipped, but the read branch still fires —
// which matters for unit tests that seed the cache directly.
func (s *Server) fillDailyPnL(c *ibkrlib.Connector, rows []rpc.PositionView, conIDs map[string]int) {
	if c == nil || len(rows) == 0 {
		return
	}
	account := s.cachedAccount()
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
// subscriptions the connector currently holds. Exposed via the
// connector's cache; the daemon uses it to honor maxDailyPnLSubscriptions
// without reaching into pkg/ibkr internals.
func (s *Server) activeDailyPnLCount(c *ibkrlib.Connector) int {
	return c.ActiveDailyPnLSubscriptions()
}

// cachedAccount returns the account code the daemon believes is active.
// Pulled from the connector's continuously-fresh accountSummary stream;
// empty when pre-handshake.
func (s *Server) cachedAccount() string {
	c := s.gatewayConnector()
	if c == nil {
		return ""
	}
	raw := c.AccountSummaryRaw()
	if id, ok := raw["AccountCode"]; ok && id != "" {
		return id
	}
	// Some gateways emit the account code only on managedAccounts; the
	// connector's account field is the canonical source.
	if id := c.AccountID(); id != "" {
		return id
	}
	return ""
}

// baseCurrencyFromRaw resolves the account's base currency by scanning
// the raw accountSummary map. The bare "Currency" tag IBKR emits carries
// the literal string "BASE" (the pseudo-currency name, not the actual
// base currency), so it is useless on its own — we only return it when
// the value is something other than "BASE". Prefer account-level value
// suffixes (`NetLiquidation_EUR`) because the streaming `$LEDGER:ALL`
// ExchangeRate rows can all be 1.0 on some accounts; use the exchange-rate
// fallback only when exactly one real currency has a unit rate.
func baseCurrencyFromRaw(raw map[string]string) string {
	if v, ok := raw["Currency"]; ok {
		ccy := normCcy(v)
		if ccy != "" && ccy != "BASE" {
			return ccy
		}
	}
	for _, tag := range []string{
		"NetLiquidation",
		"BuyingPower",
		"AvailableFunds",
		"ExcessLiquidity",
		"TotalCashValue",
		"MaintMarginReq",
		"MaintenanceMarginReq",
		"InitMarginReq",
		"GrossPositionValue",
		"UnrealizedPnL",
		"RealizedPnL",
	} {
		if ccy := accountValueCurrencySuffix(raw, tag); ccy != "" {
			return ccy
		}
	}
	const erPrefix = "ExchangeRate_"
	const eps = 1e-6
	match := ""
	for k, v := range raw {
		ccy, ok := strings.CutPrefix(k, erPrefix)
		if !ok {
			continue
		}
		ccy = normCcy(ccy)
		if ccy == "" || ccy == "BASE" {
			continue
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.Abs(rate-1.0) > eps {
			continue
		}
		if match != "" && ccy != match {
			return ""
		}
		match = ccy
	}
	return match
}

func accountValueCurrencySuffix(raw map[string]string, tag string) string {
	prefix := tag + "_"
	best := ""
	for k := range raw {
		ccy, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		ccy = normCcy(ccy)
		if ccy == "" || ccy == "BASE" {
			continue
		}
		if best == "" || ccy < best {
			best = ccy
		}
	}
	return best
}

func netLiquidationBaseFromRaw(raw map[string]string, baseCcy string) *float64 {
	if v, ok := parseRawFloat(raw, "NetLiquidation"); ok {
		return &v
	}
	baseCcy = normCcy(baseCcy)
	if baseCcy != "" {
		if v, ok := parseRawFloat(raw, "NetLiquidation_"+baseCcy); ok {
			return &v
		}
		return nil
	}
	var out *float64
	const prefix = "NetLiquidation_"
	for k := range raw {
		ccy, ok := strings.CutPrefix(k, prefix)
		if !ok || normCcy(ccy) == "BASE" {
			continue
		}
		v, ok := parseRawFloat(raw, k)
		if !ok {
			continue
		}
		if out != nil {
			return nil
		}
		vv := v
		out = &vv
	}
	return out
}

func parseRawFloat(raw map[string]string, key string) (float64, bool) {
	v, ok := raw[key]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

const fxRepairQuoteBudget = 1200 * time.Millisecond

type currencyRateResolver func(context.Context, string, string, time.Duration) (float64, bool)

// repairCurrencyLedgerFXRates fixes a live-gateway quirk where streaming
// $LEDGER rows sometimes report ExchangeRate=1 for non-base currencies.
// Downstream exposure math treats ExchangeRate as BASE per CCY, so a fake
// unit rate is worse than missing data. Keep valid non-unit rates, resolve
// missing or suspicious unit rates through bounded FX snapshots, and mark
// unresolved rates as unavailable (0) so consumers skip them.
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
// position's FXRate field. Same-currency rows keep it nil because the
// conversion is implicitly 1.0.
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
	if len(options) == 0 || rpc.IsOptionRTH(now) {
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
			Message:  "The regular U.S. listed-options data surface is outside RTH.",
			Impact:   "Option bid/ask, previous close, IV, and Greeks are closed-session context, not executable quotes, unless live fields landed; SPX/VIX extended sessions do not guarantee a complete API surface.",
			Action:   "Use the account mark for held-position valuation; retry during 09:30-16:00 ET for the most complete quote/OI/IV surface.",
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
// zero marks and no quote fields. Numeric zeros are a data-quality flag, not
// authority: only the broker's terminal non-reporting verdict (stamped by
// prewarmStockQuoteSummaries from the connector's guarded inactive marks) may
// mint QuoteExpectationNone, so a transiently zeroed real holding stays
// visible as unresolved exposure instead of being sealed as expected-silent.
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
// in base currency: Σ (non-base NetLiquidation × ExchangeRate × 0.01).
// Skips when the ledger is empty (single-currency book or pre-handshake)
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

// fillDailyChange populates PrevClose / DayChange / DayChangePct /
// DayChangeMoney on each stock row from the row's regular-close field or
// the cache. Rows whose underlying has no positive close anchor (cache
// miss, dead stream) are left untouched — pointers stay nil and the renderer
// shows an em-dash.
//
// DayChangeMoney is qty × DayChange (stocks have multiplier 1; the
// dollar impact on the position equals the per-share move times shares
// held). Computed inline rather than in computePositionDayChange so the
// option path can supply its own (Mark − OptionPrevClose) inputs without
// duplicating the price-level math.
func (s *Server) fillDailyChange(stocks []rpc.PositionView) {
	now := time.Now()
	for i := range stocks {
		p := &stocks[i]
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
// each option leg using the contract's own prev close (OptionPrevClose,
// populated by fillOptionGreeks from the per-leg tick stream — not the
// underlying's PrevClose, which would give the wrong answer). Skips legs
// where either input is missing; pointers stay nil and the renderer
// shows an em-dash.
//
// Formula: qty × multiplier × (Mark − OptionPrevClose). Multiplier
// defaults to 100 when the wire value is zero — matches the convention
// in avgCostPerShare and IBKR's per-contract pricing for standard equity
// options (a real zero would mean a non-standard contract spec we can't
// price honestly).
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

// fillOptionUnderlyingPrevClose copies the cached underlying regular close
// onto each option leg's PrevClose field — useful as a contextual anchor
// when the renderer groups by underlying. The option's own DayChange
// stays nil because we don't track contract-level prev close.
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

// positionsPrewarmWorkers bounds the per-positions-call market-data
// fan-out. 4 mirrors handleChainFetch — the gateway throttles
// aggressive subscribe churn beyond that.
const positionsPrewarmWorkers = 4

// optionGreeksBudget is the per-leg deadline for capturing the IBKR
// model-computation tick. Long enough for the gateway's typical 200-
// 800 ms latency between subscribe and the first tick-21 row, short
// enough that a 15-leg book cold-fetch stays under ~6 s wall time at
// 4-way parallelism. Negative-cache means subsequent calls within the
// TTL pay zero for legs that already returned empty.
const optionGreeksBudget = 2500 * time.Millisecond

// prewarmOptionGreeks dispatches up to positionsPrewarmWorkers concurrent
// brief subscribes for each option leg, harvests the model-computation
// Greeks (msg 21 tickType 13), then unsubscribes. Caches into s.greeks
// keyed by the OPRA-style option key. Skips legs whose Greeks are
// already cached (positive or negative entry) within the TTL.
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
// cycle and returns a cache entry. The entry is negative (ok=false)
// when no valid model-computation tick arrived before the deadline.
// Honors ctx cancellation so a daemon-shutdown mid-prewarm tears the
// subscription down promptly.
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
// Delta/Gamma/Theta/Vega fields, plus the option-contract-level bid/ask,
// IV, and prev_close pulled from the connector's tick maps (populated by
// the prewarm subscription). Legs whose data is absent keep nil pointers —
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
			// contract on PositionView.Delta etc. ("never zero-substituted"),
			// a genuine zero from the model — deep-ITM theta ≈ 0, ATM-
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
		if bid, ask, ok := c.OptionQuoteBidAsk(key); ok {
			if bid > 0 {
				b := bid
				p.OptionBid = &b
			}
			if ask > 0 {
				a := ask
				p.OptionAsk = &a
			}
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
// Connector.SubscribeOption returns. Mirrors:
//
//	fmt.Sprintf("%s_%s%s%.0f", upper(under), expiryYMD[2:], right, strike)
//
// Returns "" when any required field is missing (e.g. a malformed
// position string we couldn't parse).
//
// Accepts both rpc.SecTypeOption ("OPTION" — the AssetType enum value
// stamped by pkg/ibkr's convertIBKRPositions, the canonical wire value)
// and "OPT" (the IBKR API request-side short form, here as a defensive
// fallback for any code path that still threads the short form through).
// The original v0.10.0 release had only the "OPT" check and reported
// greeks_coverage 0/N for every option-bearing account, because
// positions came through as "OPTION"; this dual-tolerance is the
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
// into one PositionsPortfolio block.
//
// Sign convention: Quantity carries the position sign (long calls +qty,
// short puts -qty). EffectiveDelta sums per-leg delta × qty × multiplier
// plus stock qty. DollarDelta uses the model-computation underlying
// price IBKR sent alongside the Greeks (kept in lockstep so the dollar
// figure is consistent with the delta it was computed against).
//
// Currency mixing is honest: we report DollarDeltaCurrency as the
// single contract currency only when every contributing option leg
// agrees. A truly mixed-currency option book gets "MIX" so the
// caller knows not to compare to a single FX rate.
//
// Always returns a non-nil result (the renderer relies on this), but
// individual pointer fields stay nil when their inputs were absent.
func buildPortfolioAggregates(stocks, options []rpc.PositionView) *rpc.PositionsPortfolio {
	return buildPortfolioAggregatesWithBase(stocks, options, "")
}

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
	// delta (delta=1 for stock by definition). positionDollarDelta rejects
	// mark=0 zombie rows so this aggregate stays stable across calls.
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
// option Greek into a share-equivalent quantity. PositionView.Multiplier
// is populated from the wire (msgPortfolioValue → pos.Asset.Multiplier),
// reliable across standard equity options (100), minis (10), and index
// options (sometimes 50 or 1000). Falls back to 100 only when the wire
// didn't carry a value — the safe default for retail equity options.
// Without this fallback an account that never received a multiplier tick
// would zero out every option contribution to effective_delta /
// dollar_delta / daily_theta.
func optionMultiplier(p rpc.PositionView) int {
	if p.Multiplier > 0 {
		return p.Multiplier
	}
	return 100
}

// groupByUnderlying produces one PositionGroup per underlying symbol present
// in either the equity-stock or options projection. PositionsResult.Stocks is
// the historical name of the flat non-option slice and can also contain bonds,
// futures, and indices; those exact contracts remain authoritative there but
// cannot masquerade as the singular equity leg in this ticker-level view.
// Stock + option totals contribute to GroupMarketValue /
// GroupUnrealizedPnL; the stock leg is optional.
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
	for i := range stocks {
		s := stocks[i]
		if !positionCanAnchorUnderlyingGroup(s) {
			continue
		}
		g := getOrInit(strings.ToUpper(s.Symbol))
		stk := s
		g.Stock = &stk
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
	for _, g := range groups {
		finalizePositionGroup(g, baseCcy, netLiquidationBase)
		out = append(out, *g)
	}
	slices.SortStableFunc(out, func(a, b rpc.PositionGroup) int { return cmp.Compare(a.Underlying, b.Underlying) })
	return out
}

// positionCanAnchorUnderlyingGroup accepts only the equity aliases carried by
// current PositionView output. Unknown and explicit non-equity types always
// remain flat-only; absence of a type is not stock authority.
func positionCanAnchorUnderlyingGroup(row rpc.PositionView) bool {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case rpc.SecTypeStock, "STK", "ETF":
		return true
	default:
		return false
	}
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

func finalizePositionGroup(g *rpc.PositionGroup, baseCcy string, netLiquidationBase *float64) {
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

	if g.Stock != nil {
		visit(*g.Stock, false)
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
	p.ExposureBase = buildUnderlyingExposureBase(groups, baseCcy)
}

func buildUnderlyingExposureBase(groups []rpc.PositionGroup, baseCcy string) []rpc.UnderlyingExposure {
	baseCcy = normCcy(baseCcy)
	out := make([]rpc.UnderlyingExposure, 0, len(groups))
	for _, g := range groups {
		if g.GroupMarketValueBase == nil {
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
	return out
}

// handleQuoteSnapshot resolves a contract, briefly subscribes to streaming
// market data, harvests whatever ticks arrive within the timeout window, and
// returns a snapshot. We avoid IBKR's true snapshot mode (snapshot=true)
// because it does not reliably emit tickSnapshotEnd for frozen/closed-market
// requests, leaving snapshot calls hanging until the deadline.
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
		return s.handleOptionQuoteSnapshot(ctx, c, p, timeout)
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
	// of what the caller stamped on the request. Override the echoed
	// Contract so JSON consumers see the canonical routing — the actual
	// IBKR subscription is driven by pkg/ibkr.classifySymbol(sym) inside
	// the connector and is correct either way.
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

	return q, nil
}

// absentQuoteShell converts a MarketDataAbsenceError from a subscribe
// path into the honest shell quote callers received before the absence
// memory existed (then: after burning the full poll budget): symbol and
// contract echo, no prices, stale with the stored gateway rejection as
// the reason. Returns nil when err is not an absence error, letting the
// caller propagate normally. A hard RPC error would be dishonest in the
// other direction: the app folds any per-symbol quote error into a red
// market_quotes source-health state for the whole snapshot.
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
	// computation ticks are more reliable when the underlier is also
	// subscribed, and the hold shares any concurrent stock quote/watch line.
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
		// shell a quiet timeout produces (no fabricated values, stale
		// reason carries the rejection) — mirrors the stock path.
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

// handleCancel terminates a streaming subscription previously started via
// MethodQuoteSubscribe. The wire contract is intentionally strict: an
// unknown id returns CodeBadRequest because callers only ever cancel ids
// the daemon handed them, and "silent success" would mask client-side
// programming errors. Cancel is idempotent against itself only via the
// underlying context cancel — re-cancelling the same id after it has
// already been released returns the same bad_request, which is fine.
//
// Returning an empty result keeps the JSON-RPC shape uniform with other
// unary methods (Ok: true, Result: {}); callers that don't care about
// the body can ignore it.
func (s *Server) handleCancel(req *rpc.Request) (struct{}, error) {
	var p rpc.CancelParams
	if err := decodeParams(req.Params, &p); err != nil {
		return struct{}{}, err
	}
	if p.ID == "" {
		return struct{}{}, &badRequestError{msg: "id required"}
	}
	s.mu.Lock()
	cancel, ok := s.streams[p.ID]
	s.mu.Unlock()
	if !ok {
		return struct{}{}, &badRequestError{msg: "no active stream with id " + p.ID}
	}
	cancel()
	return struct{}{}, nil
}

// handleQuoteSubscribe attaches a fan-out tap to the daemon's per-symbol
// market-data subscription and streams coalesced frames to the caller
// until the client disconnects, the daemon shuts down, or a terminal
// error frame arrives from the manager.
//
// Client disconnect is detected by an EOF watcher reading from r: any
// read result (a stray byte or EOF) cancels streamCtx. Multiple concurrent
// subscribers to the same symbol share one IBKR market-data line via the
// subManager refcount; the line is released when the last subscriber
// releases its tap.
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
	// request, so any read result on r means either a stray byte (rare) or
	// connection close (the common case). Either way cancel the stream so
	// release() runs and the refcount drops.
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
				// The terminal error frame, if any, was the last frame delivered
				// before close. Signal stream end to the client envelope.
				_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Stream: true, End: true})
				return
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
// ticks stay nil, genuine positive gateway values are surfaced.
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
// price and prevClose. Both stay nil unless price and prevClose are present
// and prevClose is strictly positive — no fabrication, no divide-by-zero.
// Centralised here so quote, watchlist, scan, and position callers share
// one formula.
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

// adHocScanLimitCap is the maximum number of rows an ad-hoc scan
// (Preset == "") may request. Presets carry their own limit and bypass
// this cap; the cap is here to keep a careless agent from asking the
// gateway for thousands of rows on an ad-hoc call. The TWS Market
// Scanner UI itself ranks to 50 by default.
const adHocScanLimitCap = 50

// defaultScanSubscriptionTimeout is how long the daemon waits for the
// gateway's first scannerData frame before giving up. 20 s is enough
// during regular trading hours but cold-starts off-hours (especially for
// scanCodes that depend on a current-session open or live option flow
// — HIGH_OPEN_GAP, TOP_PERC_GAIN, HIGH_OPT_IMP_VOLAT_OVER_HIST,
// HOT_BY_OPT_VOLUME) routinely need 25-45 s for the scanner subsystem
// to warm up. 35 s is the empirical sweet spot — long enough to absorb
// the warmup, short enough that a genuinely dead gateway still fails
// fast rather than hanging the user.
const defaultScanSubscriptionTimeout = 35 * time.Second

// handleScanRun runs a scanner. Two modes:
//
//  1. Preset (p.Preset != ""): looks up [scans.<name>] in config and runs
//     it. Limit override honored; preset.Timeout applies. Returns
//     bad_request if the preset is unknown.
//  2. Ad-hoc (p.Preset == ""): runs scanCode = p.Type / locationCode =
//     p.Exchange directly, with optional p.Instrument for non-US markets.
//     Both Type and Exchange are required; missing either → bad_request.
//     Limit clamped to adHocScanLimitCap. Fixed default timeout.
func (s *Server) handleScanRun(ctx context.Context, req *rpc.Request) (*rpc.ScanResult, error) {
	var p rpc.ScanRunParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}

	var (
		scanType    string
		scanExch    string
		scanInst    string
		scanLimit   int
		scanTimeout time.Duration
		presetName  string
	)
	switch {
	case p.Preset != "":
		preset, ok := s.cfg.Scans[p.Preset]
		if !ok {
			return nil, errBadRequest(fmt.Sprintf("unknown preset %q (run 'canary scan list' for available)", p.Preset))
		}
		scanType = preset.Type
		scanExch = preset.Exchange
		scanInst = preset.Instrument
		scanLimit = p.Limit
		if scanLimit <= 0 {
			scanLimit = preset.Limit
		}
		scanTimeout = preset.Timeout.Std()
		if scanTimeout <= 0 {
			scanTimeout = defaultScanSubscriptionTimeout
		}
		presetName = p.Preset
	default:
		// Ad-hoc.
		if strings.TrimSpace(p.Type) == "" {
			return nil, errBadRequest("ad-hoc scan requires either preset or type (scanCode); see 'canary scan params' for available scanCodes")
		}
		if strings.TrimSpace(p.Exchange) == "" {
			return nil, errBadRequest("ad-hoc scan requires exchange (locationCode); see 'canary scan params' for available locationCodes")
		}
		scanType = p.Type
		scanExch = p.Exchange
		scanInst = p.Instrument
		scanLimit = p.Limit
		if scanLimit <= 0 || scanLimit > adHocScanLimitCap {
			scanLimit = adHocScanLimitCap
		}
		scanTimeout = defaultScanSubscriptionTimeout
	}

	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	res := &rpc.ScanResult{
		Preset: presetName,
		Type:   scanType,
		AsOf:   time.Now(),
	}
	rows, err := c.RunScannerSubscription(ctx, ibkrlib.ScannerSubscription{
		Type:       scanType,
		Exchange:   scanExch,
		Instrument: scanInst,
		Limit:      scanLimit,
	}, scanTimeout)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		row := rpc.ScanRow{
			Rank:         r.Rank,
			Symbol:       r.Symbol,
			SecType:      strings.ToUpper(strings.TrimSpace(r.SecType)),
			Exchange:     strings.ToUpper(strings.TrimSpace(r.Exchange)),
			Currency:     normCcy(r.Currency),
			LocalSymbol:  strings.TrimSpace(r.LocalSymbol),
			TradingClass: strings.TrimSpace(r.TradingClass),
			Comment:      r.Comment,
		}
		row.InstrumentTags = scanInstrumentTags(row)
		res.Rows = append(res.Rows, row)
	}
	s.enrichScanRows(ctx, c, res.Rows, p)
	res.Rows = filterScanRows(res.Rows, p)
	return res, nil
}

func filterScanRows(rows []rpc.ScanRow, p rpc.ScanRunParams) []rpc.ScanRow {
	if !scanFiltersActive(p) || len(rows) == 0 {
		return rows
	}
	out := rows[:0]
	for _, row := range rows {
		if scanRowPassesFilters(row, p) {
			out = append(out, row)
		}
	}
	return out
}

func scanFiltersActive(p rpc.ScanRunParams) bool {
	return p.MinPrice > 0 || p.MinVolume > 0 || p.MinDollarVolume > 0 || p.RequireLive || p.ExcludePenny
}

func scanRowPassesFilters(row rpc.ScanRow, p rpc.ScanRunParams) bool {
	minPrice := p.MinPrice
	if p.ExcludePenny {
		minPrice = max(minPrice, 5)
	}
	if minPrice > 0 {
		if row.Last == nil || *row.Last < minPrice {
			return false
		}
	}
	if p.MinVolume > 0 {
		if row.Volume == nil || *row.Volume < p.MinVolume {
			return false
		}
	}
	if p.MinDollarVolume > 0 {
		if scanRowDollarVolume(row) < p.MinDollarVolume {
			return false
		}
	}
	if p.RequireLive && !scanRowHasUsableLiveQuote(row) {
		return false
	}
	return true
}

func scanRowDollarVolume(row rpc.ScanRow) float64 {
	if row.Last == nil {
		return 0
	}
	if row.Volume != nil {
		return *row.Last * float64(*row.Volume)
	}
	if row.AvgDollarVolume20D != nil {
		return *row.AvgDollarVolume20D
	}
	if row.AvgVolume20D != nil {
		return *row.Last * float64(*row.AvgVolume20D)
	}
	return 0
}

func scanRowHasUsableLiveQuote(row rpc.ScanRow) bool {
	if row.Last == nil || !rpc.IsLiveDataType(row.DataType) {
		return false
	}
	for _, w := range row.WarningDetails {
		switch w.Code {
		case "off_hours_quote", "selected_price_prev_close", "selected_price_closed_session", "stale_quote", "missing_quote":
			return false
		}
	}
	return true
}

// scanEnrichWindow is the per-row deadline for collecting market-data ticks
// after subscribing. Most US stocks deliver bid/ask/last + prev-close + IV
// + 52w within 2-4 s during RTH; the slowest generic-tick set (tick 165
// Misc Stats) tail-arrives up to ~6 s. Off-hours, ticks often don't arrive
// at all; rows then surface with whatever made it (typically prev-close
// only) and the other fields nil — the honest read.
const scanEnrichWindow = 6 * time.Second

// scanEnrichConcurrency bounds the in-flight enrichment subscriptions.
// IBKR Pro accounts typically have a 100-slot market-data cap; the daemon
// holds a few for `quote --watch`, positions Greeks, MCP subscribers, etc.
// 20 leaves comfortable headroom and reduces a 50-row scan to 2-3 waves
// at scanEnrichWindow each (~12-18 s wall clock, well under the
// MethodScanRun unary deadline).
const scanEnrichConcurrency = 20

// enrichScanRows fans out one Hold-based subscribe per row symbol,
// collects last/prev-close/change/volume/IV/52w from the daemon's tick
// cache, and writes the result back into rows in place. Bounded by
// scanEnrichConcurrency goroutines. Per-row failures are silent: the row
// keeps its existing rank+symbol+comment, the numeric fields stay nil,
// and the renderer shows "—" — never a fabricated value.
//
// Ctx cancellation propagates: a CLI Ctrl-C during enrichment aborts
// in-flight subscriptions and lets the result return with whatever data
// arrived first, again with no fabrication.
func (s *Server) enrichScanRows(ctx context.Context, c *ibkrlib.Connector, rows []rpc.ScanRow, filters rpc.ScanRunParams) {
	if len(rows) == 0 || c == nil {
		return
	}
	sem := make(chan struct{}, scanEnrichConcurrency)
	var wg sync.WaitGroup
	for i := range rows {
		if rows[i].Symbol == "" {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			s.enrichOneScanRow(ctx, c, &rows[i], filters)
		})
	}
	wg.Wait()
}

// enrichOneScanRow holds a market-data subscription on the row's symbol,
// polls the connector's tick cache until the row has at least a last
// price (the minimum signal worth rendering) or the per-row window
// elapses, then writes whatever arrived back into the row.
//
// The shape of "good enough" is intentionally loose: we keep polling
// even after `last` arrives because IV and 52w typically lag bid/ask/last
// by 1-2 s, and the row is more useful with them than without.
func (s *Server) enrichOneScanRow(ctx context.Context, c *ibkrlib.Connector, row *rpc.ScanRow, filters rpc.ScanRunParams) {
	pollKey := row.Symbol
	var releaseSub func()
	if scanRowNeedsRoutedQuote(row) {
		contract := ibkrlib.Contract{
			Symbol:       normSym(row.Symbol),
			SecType:      strings.ToUpper(strings.TrimSpace(row.SecType)),
			Exchange:     strings.ToUpper(strings.TrimSpace(row.Exchange)),
			Currency:     normCcy(row.Currency),
			LocalSymbol:  strings.TrimSpace(row.LocalSymbol),
			TradingClass: strings.TrimSpace(row.TradingClass),
		}
		key, err := c.SubscribeMarketDataWithContract(ctx, contract, defaultGenericTicks)
		if err != nil {
			return
		}
		pollKey = key
		releaseSub = func() { _ = c.UnsubscribeMarketData(key) }
	} else {
		release, err := s.subs.Hold(ctx, row.Symbol)
		if err != nil {
			// Hold can only fail with ErrIBKRUnavailable (gateway dropped
			// mid-scan) or an internal subscribe error. Either way, the
			// row stays bare — no fabrication.
			return
		}
		releaseSub = release
	}
	defer releaseSub()

	deadline := time.Now().Add(scanEnrichWindow)
	poll := time.NewTicker(75 * time.Millisecond)
	defer poll.Stop()
	var snap *ibkrlib.MarketData
	for {
		md := c.MarketDataSnapshot()
		if data, ok := md[pollKey]; ok {
			snap = data
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
	}
	if snap == nil {
		return
	}
	if snap.Last > 0 {
		v := snap.Last
		row.Last = &v
	}
	if snap.Close > 0 {
		v := snap.Close
		row.PrevClose = &v
	}
	if row.Last != nil && row.PrevClose != nil {
		ch, pct := computeQuoteChange(row.Last, row.PrevClose)
		row.Change = ch
		row.ChangePct = pct
	}
	if snap.Volume > 0 {
		v := snap.Volume
		row.Volume = &v
	}
	if snap.IV > 0 {
		v := snap.IV
		row.IV = &v
	}
	if snap.Week52High > 0 {
		v := snap.Week52High
		row.Week52High = &v
	}
	if snap.Week52Low > 0 {
		v := snap.Week52Low
		row.Week52Low = &v
	}
	row.AsOf = time.Now()
	row.DataType = marketDataTypeName(c.MarketDataTypeForSymbol(pollKey))
	q := rpc.Quote{
		Symbol:    row.Symbol,
		Last:      row.Last,
		PrevClose: row.PrevClose,
		Volume:    row.Volume,
		IV:        row.IV,
		IVStatus:  "unavailable",
		DataType:  row.DataType,
		AsOf:      row.AsOf,
	}
	if !snap.LastTradeTime.IsZero() {
		q.PriceAt = snap.LastTradeTime
	}
	market := marketcal.MarketUSEquity
	if normCcy(row.Currency) == "EUR" {
		market = marketcal.MarketDEXetra
	}
	if filters.MinDollarVolume > 0 {
		s.fillQuoteLiquidity(ctx, c, &q, market, scanEnrichWindow, nil)
		row.AvgVolume20D = q.AvgVolume20D
		row.AvgDollarVolume20D = q.AvgDollarVolume20D
	}
	s.attachQuoteSessionContext(&q, market)
	s.decorateQuote(&q, market)
	row.DataType = q.DataType
	row.FeedType = q.FeedType
	row.PriceAt = q.PriceAt
	row.PriceAsOf = q.PriceAsOf
	row.VolumePhase = q.VolumePhase
	row.WarningDetails = q.WarningDetails
}

func scanRowNeedsRoutedQuote(row *rpc.ScanRow) bool {
	if row == nil {
		return false
	}
	ccy := normCcy(row.Currency)
	return ccy != "" && ccy != "USD"
}

// handleScanParams fetches the gateway's scanner catalog (scanCodes,
// locationCodes, instruments) so agents can discover what's available
// without guessing at the magic strings. Result includes the raw XML
// only when explicitly requested — the payload is ~200 KB on a US Pro
// gateway and overwhelms typical agent context budgets if always sent.
func (s *Server) handleScanParams(ctx context.Context, req *rpc.Request) (*rpc.ScanParamsResult, error) {
	var p rpc.ScanParamsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	params, err := c.RunScannerParameters(ctx, 10*time.Second)
	if err != nil {
		return nil, err
	}
	out := &rpc.ScanParamsResult{AsOf: time.Now()}
	for _, in := range params.Instruments {
		out.Instruments = append(out.Instruments, rpc.ScanParamInstrument{Name: in.Name, Type: in.Type})
	}
	for _, loc := range params.Locations {
		out.Locations = append(out.Locations, rpc.ScanParamLocation{Code: loc.Code, DisplayName: loc.DisplayName})
	}
	scans := params.ScanTypes
	if p.Instrument != "" {
		scans = params.FilterByInstrument(p.Instrument)
	}
	for _, st := range scans {
		out.ScanTypes = append(out.ScanTypes, rpc.ScanParamScanType{
			Code:        st.Code,
			DisplayName: st.DisplayName,
			Instruments: st.Instruments,
		})
	}
	if p.IncludeRawXML {
		out.RawXML = params.RawXML
	}
	return out, nil
}

// handleScanList enumerates the configured presets.
func (s *Server) handleScanList() *rpc.ScanListResult {
	out := &rpc.ScanListResult{}
	for name, preset := range s.cfg.Scans {
		out.Presets = append(out.Presets, rpc.ScanPresetSummary{
			Name:       name,
			Type:       preset.Type,
			Exchange:   preset.Exchange,
			Instrument: preset.Instrument,
			Limit:      preset.Limit,
		})
	}
	slices.SortStableFunc(out.Presets, func(a, b rpc.ScanPresetSummary) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// handleStatusHealth describes daemon + gateway state for status command.
// Takes connector + endpoint snapshots under mu so all IBKR-side fields
// describe the same point in time even if reconnectFlow races with this
// call (reconnect rewrites s.endpoint and s.connector).
//
// PortOrigin / TLSOrigin / Alternates come from the discovery layer and
// let `canary status` show whether the endpoint was pinned in config or
// found by probe — the user-visible contract for the AUTO-by-default
// design.
//
// When the daemon is currently degraded, kick off a background
// rediscover+reconnect (triggerReconnect throttles itself via the
// in-flight gate). Clearing lastConnectError as part of that turns this
// status response into "handshake in flight," which prompts the CLI's
// 25s status wait loop to keep polling — so a user who just moved IBKR
// from Gateway (4001) to TWS (7496) gets recovery in a single `ibkr
// status` invocation instead of having to restart the daemon.
func (s *Server) handleStatusHealth() *rpc.HealthResult {
	s.triggerReconnect()
	return s.statusHealthSnapshot()
}

// statusHealthSnapshot composes only daemon-owned cached state. Alert
// heartbeats use it so observation cannot trigger discovery, reconnect, or any
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
	if c != nil {
		res.ConnectedAccount = strings.TrimSpace(c.AccountID())
		if res.ConnectedAccount != "" {
			accountForMode = res.ConnectedAccount
		}
		// Report IsReady, not IsConnected: the gateway being TCP-reachable
		// is not enough — handlers must be armed (post-handshake) for any
		// data verb to succeed. Reporting IsConnected here while every
		// other verb gates on IsReady made `status` lie when the connector
		// got stuck in the {ready=false, conn=true} state (overnight TWS
		// hiccups, market-data farm reconnects). User-invoked status triggers
		// recovery before entering this pure projection; heartbeats do not.
		//
		// AND gated on postConnectSetupDone: c.ready is flipped by the
		// connection read-loop goroutine in pkg/ibkr independently of
		// postConnectSetup's synchronous prewarm sentinel-setting. A
		// status RPC landing in the brief window between c.ready=true
		// and postConnectSetup's tail would otherwise see Connected=true
		// with an empty BackgroundTasks list — the symptom the user
		// reported on 2026-05-21 (gamma + regime missing on the first
		// status, present on the second). The latch is one-way: once
		// postConnectSetup completes once, the daemon never re-enters
		// "starting up" state from the user's point of view, even
		// across reconnects (the prewarm Once guards mean reconnect
		// doesn't re-fire them anyway).
		res.Connected = c.IsReady() && setupComplete
		res.ServerVersion = c.ServerVersion()
		res.NegotiatedTLS = c.UsingTLS()
		farmStatuses = c.DataFarmStatuses()
		res.DataFarms = statusDataFarms(farmStatuses)
	}
	res.AccountMode = accountModeForStatus(ep.Port, accountForMode)

	// BackgroundTasks lists daemon-internal long-running computes
	// running RIGHT NOW. Presence-as-state: a task appears here iff
	// its accessor returns busy; idle/ready/cold tasks are omitted.
	// Always emitted as a (possibly empty) slice so consumers can
	// rely on `len() == 0` to mean idle. Read from s.backgroundTasks
	// — the same source isBusy() and the regime partial-envelope
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
	shadowScope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, res.ConnectedAccount)
	gatewayPhase := alertShadowGatewayPhaseForHealth(res.Connected, setupComplete, connectInFlight, lastErr, time.Since(s.startedAt))
	observedAt := time.Now().UTC()
	if s.now != nil {
		observedAt = s.now().UTC()
	}
	s.observeDataHealthAlertShadow(res, shadowScope, gatewayPhase, observedAt)
	return res
}

func (s *Server) subsystemHealth(connected bool, farms []ibkrlib.DataFarmStatus) []rpc.SubsystemHealth {
	gatewayStatus := "ready"
	if !connected {
		gatewayStatus = "unavailable"
	}
	out := []rpc.SubsystemHealth{
		s.authoritySubsystemHealth(),
		{Name: "watchlist", Status: "ready", Message: "list-only path is local; quote enrichment requires gateway"},
		statusSubsystemFromReadiness("quote", marketDataFarmReadiness(connected, farms, "quotes may time out")),
		statusSubsystemFromReadiness("scanner", marketDataFarmReadiness(connected, farms, "scanner requests may time out")),
		statusSubsystemFromReadiness("history", historicalDataFarmReadiness(connected, farms)),
		chainSubsystemHealth(connected, farms),
	}
	gamma := rpc.SubsystemHealth{Name: "gamma", Status: gatewayStatus}
	if s.zeroGamma != nil && s.zeroGamma.IsComputing() {
		gamma.Status = "computing"
		gamma.Message = "dealer gamma compute is fanning out option legs"
	}
	out = append(out, gamma)
	breadth := rpc.SubsystemHealth{Name: "breadth", Status: gatewayStatus}
	if s.breadth != nil && s.breadth.IsBusy() {
		breadth.Status = "computing"
		breadth.Message = "S&P 500 breadth refresh is running or waiting to retry"
	}
	out = append(out, breadth)
	if sub, ok := s.proposalSubsystemHealth(); ok {
		out = append(out, sub)
	}
	if sub, ok := s.opportunitySubsystemHealth(); ok {
		out = append(out, sub)
	}
	return out
}

// proposalSubsystemHealth reports the protection-proposal engine's refresh
// health. The engine preserves the last-good snapshot through transient
// refresh failures (err == nil, served as_of frozen), so the gateway can
// look healthy while the protection panel serves stale data — this row
// makes that state diagnosable from `canary status` alone. It stays "ready"
// below proposalRefreshWarnStreak because the first refreshes after a
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

// handleHistoryDaily returns N days of daily OHLCV bars for a symbol.
// Calendar lookback (matching IBKR HMDS): the gateway returns whatever
// trading days fall inside the window, so an N=90 request typically yields
// ~63 bars. Days defaults to 90.
func (s *Server) handleHistoryDaily(ctx context.Context, req *rpc.Request) (*rpc.HistoryDailyResult, error) {
	var p rpc.HistoryDailyParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	sym := normSym(p.Symbol)
	if sym == "" {
		return nil, errBadRequest("symbol required")
	}
	days := p.Days
	if days <= 0 {
		days = 90
	}
	whatToShow, err := normalizeHistoryWhatToShowParam(p.WhatToShow)
	if err != nil {
		return nil, err
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	var bars []ibkrlib.HistoricalBar
	if whatToShow != "" {
		bars, err = c.FetchHistoricalDailyBarsWhatToShow(ctx, sym, days, whatToShow, 0)
	} else {
		bars, err = c.FetchHistoricalDailyBars(ctx, sym, days, 0)
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("historical data deadline exceeded for %s (HMDS pacing or response exceeded request budget): %w", sym, err)
		}
		return nil, err
	}
	res := &rpc.HistoryDailyResult{
		Symbol:     sym,
		Days:       days,
		WhatToShow: whatToShow,
		AsOf:       time.Now(),
		Bars:       make([]rpc.HistoryBar, 0, len(bars)),
	}
	for _, b := range bars {
		res.Bars = append(res.Bars, rpc.HistoryBar{
			Date:   barDate(b),
			Open:   b.Open,
			High:   b.High,
			Low:    b.Low,
			Close:  b.Close,
			Volume: b.Volume,
		})
	}
	return res, nil
}

func normalizeHistoryWhatToShowParam(value string) (string, error) {
	clean := strings.ToUpper(strings.TrimSpace(value))
	switch clean {
	case "":
		return "", nil
	case "TRADES", "MIDPOINT", "ADJUSTED_LAST":
		return clean, nil
	default:
		return "", errBadRequest("what_to_show must be TRADES, MIDPOINT, or ADJUSTED_LAST")
	}
}

// handleBreadthSPX returns the current S&P 500 stocks-above-50DMA reading
// plus a trailing daily series for sparkline rendering. The headline
// number is the percentage of S&P 500 constituents trading above their
// own 50-day SMA, in [0, 100]. The dashboard generator uses this as
// Indicator 5 of the risk-regime panel.
//
// Methodology — spx.MethodConstituentFanout: we compute S5FI locally
// from constituent daily closes pulled via IBKR's HMDS feed. IBKR
// does not redistribute S&P DJI's S5FI index on retail subscriptions
// (verified via reqContractDetails — see pkg/ibkr/symbols.go), so the
// daemon reproduces the math from data it already has access to. The
// engine runs a once-daily refresh post-close (16:35 ET) and serves
// the cached snapshot to readers.
//
// The handler is a thin projection of the engine state onto the wire
// envelope: the long-running fetch happens off this code path entirely.
// Cold-start callers receive an empty envelope (Value=0, History=[]);
// the fetchRegimeBreadth wrapper checks IsRefreshing to map that to
// status="computing" rather than "unavailable".
//
// Threshold derivation (green / yellow / red) is intentionally not on
// this result. The spec itself flags those bands as user-tunable, so
// the daemon stays out of policy and the renderer applies whatever
// cuts the user has configured.
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
	res.State = classifyBreadthState(ok, active)
	res.Refreshing = ok && active
	if progress, exists := s.breadth.Progress(); exists {
		res.Refresh = &rpc.BreadthRefreshProgress{
			SessionKey:  progress.SessionKey,
			StartedAt:   progress.StartedAt,
			Processed:   progress.Processed,
			Total:       progress.Total,
			Deadline:    progress.Deadline,
			LastFailure: rpc.BreadthRefreshFailure(progress.LastFailure),
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
// active) pair onto the wire-visible BreadthState. This is the
// single source of truth — handleBreadthSPX and any future consumer
// must derive State the same way. The four states:
//
//   - ready: snapshot exists; Refreshing reports whether a newer refresh
//     is active
//   - computing: no snapshot exists and a refresh is in flight or waiting
//     to retry
//   - cold: no snapshot AND no active refresh/retry — rare; only seen
//     briefly between daemon Start and postConnectSetup launching the
//     engine, or after a coverage-threshold-failed refresh exhausts
//     its retry budget
//   - degraded: reserved; v0.27.3 engine refuses to persist below
//     threshold so this state isn't currently produced. The enum
//     defines it so a future schema can adopt it without a contract
//     bump.
func classifyBreadthState(snapshotExists, active bool) rpc.BreadthState {
	switch {
	case snapshotExists:
		return rpc.BreadthStateReady
	case active:
		return rpc.BreadthStateComputing
	default:
		return rpc.BreadthStateCold
	}
}

// barDate returns the bar's date as YYYY-MM-DD. IBKR's daily bar dates arrive
// as YYYYMMDD strings; the parsed Time field is best-effort.
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
// instead of falling through to the catch-all internal classification.
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

func errBadRequest(msg string) error { return &badRequestError{msg: msg} }

// decodeParams unmarshals req.Params into dst and tags failures as bad-request
// errors so classifyError surfaces them as CodeBadRequest instead of internal.
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
