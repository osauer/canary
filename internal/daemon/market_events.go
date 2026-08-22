package daemon

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	marketEventsRegSHOFreshFor      = 12 * time.Hour
	marketEventsRegSHOMaxAge        = 96 * time.Hour
	marketEventsHaltsFreshFor       = time.Minute
	marketEventsBorrowPollBudget    = 2500 * time.Millisecond
	marketEventsBorrowFeeFreshFor   = 15 * time.Minute
	marketEventsBorrowFeeMaxAge     = 90 * time.Minute
	marketEventsBorrowFeeExtremePct = 50.0
	marketEventsRecentHaltWindow    = 24 * time.Hour
	marketEventsBorrowTightShares   = 10_000
	marketEventsBorrowExtremeShares = 1_000

	// marketEventsBorrowPollWorkers bounds the concurrent shortable-tick
	// marketEventsBorrowPollBudget for books whose names never deliver
	marketEventsBorrowPollWorkers = 8

	// marketEvents*RetryAfter gate re-fetch attempts after a source
	// failure. Without failure memory a blocked endpoint re-burns its
	// ftp3.interactivebrokers.com:21 filtered by the local network: a
	// transient failure shouldn't blind it for long — and one timeout
	marketEventsHaltsRetryAfter     = time.Minute
	marketEventsRegSHORetryAfter    = 15 * time.Minute
	marketEventsBorrowFeeRetryAfter = 15 * time.Minute

	// marketEventsShortableAbsentRetry bounds how long a "tick 236 never
	// absence must be re-tested once the tape can plausibly have changed;
	marketEventsShortableAbsentRetry = 30 * time.Minute

	// marketEventsFTPDialTimeout bounds the borrow-fee FTP connect. A
	marketEventsFTPDialTimeout = 4 * time.Second
)

var marketEventsHTTPClient = &http.Client{
	Timeout: 10 * time.Second,
	// Nasdaq's symdir endpoints 302-redirect to an HTML error page when
	// threshold symbols" for 12 h and never reaching the most recent
	CheckRedirect: marketEventsNoRedirect,
}

func marketEventsNoRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

var fetchIBKRBorrowFees = fetchIBKRBorrowFeesFTP

type marketEventCache struct {
	mu                        sync.Mutex
	borrowFeesRefreshMu       sync.Mutex
	borrowFeeFallbackMu       sync.Mutex
	regSHO                    marketEventRegSHOEntry
	halts                     marketEventHaltsEntry
	borrowFees                marketEventBorrowFeeEntry
	borrowFeesLastAttempt     *marketEventBorrowFeeAttempt
	borrowFeesRevision        int64
	borrowFeeFallback         marketEventFeeRateState
	borrowFeeFallbackRevision int64
	borrowFeeFallbackLoadedAt time.Time
	// borrowFeeFallbackCurrent binds runtime-only entitlement/failure reuse to
	// the exact connector socket session that observed it. A reconnect leaves
	// only the persisted 15-second identical-wire boundary in force.
	borrowFeeFallbackCurrent  map[string]ibkrlib.HistoricalSessionBinding
	fetchHistoricalFeeRates   func(context.Context, ibkrlib.Contract, int, time.Duration) ([]ibkrlib.HistoricalBar, error)
	resolveHistoricalFeeRoute func(context.Context, ibkrlib.Contract, time.Duration) (ibkrlib.Contract, error)
	readCachedPositions       func() ([]*ibkrlib.RawPosition, ibkrlib.PortfolioStreamHealth, error)
	regSHOFreshFor            time.Duration
	haltsFreshFor             time.Duration
	now                       func() time.Time

	// shortableAbsent remembers symbols whose shortable tick (236) did
	shortableAbsent map[string]time.Time // symbol → when observed absent

	// *FailedAt remember the last failed fetch per external source so
	// re-fetches. Zero value = no recent failure. Cleared on success.
	regSHOFailedAt time.Time
	haltsFailedAt  time.Time

	// authority is the sole durable runtime store after startup attachment.
	// Borrow-fee failure/backoff is durable; Reg SHO/halt retry timestamps and
	// shortableAbsent remain memory-only control state.
	authority *corestore.Store
}

// shortableAbsentRecently reports whether sym's shortable tick was
// observed absent within the last marketEventsShortableAbsentRetry.
func (c *marketEventCache) shortableAbsentRecently(sym string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.shortableAbsent[sym]
	return ok && now.Sub(at) < marketEventsShortableAbsentRetry
}

// rememberShortableAbsent records that sym ran a full poll budget at now
func (c *marketEventCache) rememberShortableAbsent(sym string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shortableAbsent == nil {
		c.shortableAbsent = make(map[string]time.Time)
	}
	c.shortableAbsent[sym] = now
}

// clearShortableAbsence drops all absence records. Called on gateway
func (c *marketEventCache) clearShortableAbsence() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shortableAbsent = nil
}

type marketEventRegSHOEntry struct {
	FetchedAt time.Time                          `json:"fetched_at"`
	AsOf      time.Time                          `json:"as_of"`
	SourceURL string                             `json:"source_url"`
	Symbols   map[string]marketEventRegSHORecord `json:"symbols"`
}

type marketEventRegSHORecord struct {
	Symbol         string `json:"symbol"`
	SecurityName   string `json:"security_name,omitempty"`
	MarketCategory string `json:"market_category,omitempty"`
	Rule3210       string `json:"rule_3210,omitempty"`
}

type marketEventHaltsEntry struct {
	FetchedAt time.Time               `json:"fetched_at"`
	AsOf      time.Time               `json:"as_of"`
	SourceURL string                  `json:"source_url"`
	Records   []marketEventHaltRecord `json:"records"`
}

type marketEventHaltRecord struct {
	Symbol              string    `json:"symbol"`
	IssueName           string    `json:"issue_name,omitempty"`
	Market              string    `json:"market,omitempty"`
	ReasonCode          string    `json:"reason_code"`
	HaltedAt            time.Time `json:"halted_at"`
	ResumptionQuoteAt   time.Time `json:"resumption_quote_at,omitzero"`
	ResumptionTradeAt   time.Time `json:"resumption_trade_at,omitzero"`
	PauseThresholdPrice string    `json:"pause_threshold_price,omitempty"`
}

type marketEventBorrowFeeEntry struct {
	FetchedAt time.Time                             `json:"fetched_at"`
	AsOf      time.Time                             `json:"as_of"`
	SourceURL string                                `json:"source_url"`
	Symbols   map[string]marketEventBorrowFeeRecord `json:"symbols"`
}

type marketEventBorrowFeeRecord struct {
	Symbol     string  `json:"symbol"`
	Currency   string  `json:"currency,omitempty"`
	Name       string  `json:"name,omitempty"`
	ConID      string  `json:"conid,omitempty"`
	ISIN       string  `json:"isin,omitempty"`
	RebateRate float64 `json:"rebate_rate"`
	FeeRate    float64 `json:"fee_rate"`
	Available  int64   `json:"available"`
}

func newMarketEventCache(now func() time.Time) *marketEventCache {
	if now == nil {
		now = time.Now
	}
	return &marketEventCache{
		regSHOFreshFor: marketEventsRegSHOFreshFor,
		haltsFreshFor:  marketEventsHaltsFreshFor,
		now:            now,
	}
}

func (s *Server) installMarketEventCache() {
	s.marketEvents = newMarketEventCache(s.now)
}

func (s *Server) handleMarketEventsSnapshot(ctx context.Context, req *rpc.Request) (*rpc.MarketEventsResult, error) {
	var p rpc.MarketEventsParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	symbols := normalizeMarketEventSymbols(append(p.Symbols, p.Symbol))
	if len(symbols) == 0 {
		pos, err := s.handlePositionsList(ctx, &rpc.Request{})
		if err != nil {
			return nil, err
		}
		pos = s.analysisPositions(pos, s.now())
		symbols = marketEventSymbolsFromPositions(pos)
	}
	res := s.marketEventsForSymbols(ctx, symbols)
	return &res, nil
}

func (s *Server) marketEventsForSymbols(ctx context.Context, symbols []string) rpc.MarketEventsResult {
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	return s.marketEvents.snapshot(ctx, symbols, s.subs, s.gatewayConnector(), s.currentBrokerStateScope)
}

func (c *marketEventCache) snapshot(ctx context.Context, symbols []string, subs *subManager, connector *ibkrlib.Connector, scopeProviders ...func() brokerStateScope) rpc.MarketEventsResult {
	now := c.now().UTC()
	symbols = normalizeMarketEventSymbols(symbols)
	res := rpc.MarketEventsResult{
		Kind:          rpc.MarketEventsKind,
		SchemaVersion: rpc.MarketEventsSchemaVersion,
		AsOf:          now,
		Symbols:       symbols,
		BySymbol:      map[string][]rpc.MarketEventFlag{},
		NotExecution:  "Market-event flags are observed context and daemon safety gates; no orders are placed by Canary.",
	}
	if len(symbols) == 0 {
		res.WarningDetails = append(res.WarningDetails, rpc.DataWarning{
			Code:     "market_events_no_symbols",
			Severity: "data_quality",
			Message:  "No symbols were provided and no held underlyings were available.",
			Impact:   "No market-event flags can be evaluated.",
			Action:   "Pass --symbol or hold a stock/ETF position before relying on held-name tags.",
		})
		res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
		return res
	}

	regSHO, regSHOHealth, err := c.loadRegSHO(ctx, now)
	res.SourceHealth = append(res.SourceHealth, regSHOHealth)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("reg_sho_threshold", err))
	} else {
		for _, sym := range symbols {
			if rec, ok := regSHO.Symbols[sym]; ok {
				res.Flags = append(res.Flags, marketEventRegSHOFlag(sym, rec, regSHO, now))
			}
		}
	}

	halts, haltsHealth, err := c.loadHalts(ctx, now)
	res.SourceHealth = append(res.SourceHealth, haltsHealth)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("halts", err))
	} else {
		for _, sym := range symbols {
			for _, rec := range halts.Records {
				if rec.Symbol == sym {
					if flag, ok := marketEventHaltFlag(sym, rec, halts, now); ok {
						res.Flags = append(res.Flags, flag)
					}
				}
			}
		}
	}

	borrowHealth := c.borrowInventory(ctx, symbols, subs, connector, now, &res)
	res.SourceHealth = append(res.SourceHealth, borrowHealth)
	borrowFees, borrowFeeHealth, err := c.loadBorrowFees(ctx, now)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("borrow_fee", err))
	}
	var scopeProvider func() brokerStateScope
	if len(scopeProviders) > 0 {
		scopeProvider = scopeProviders[0]
	}
	bulkBorrowFeeUsable := borrowFeeFTPPolicyUsable(borrowFeeHealth)
	res.BorrowFeeCoverage, borrowFeeHealth = c.borrowFeeCoverage(ctx, symbols, connector, scopeProvider, now, borrowFees, borrowFeeHealth)
	res.SourceHealth = append(res.SourceHealth, borrowFeeHealth)
	if bulkBorrowFeeUsable {
		for _, row := range res.BorrowFeeCoverage {
			if !row.PolicyEligible || row.Source != rpc.BorrowFeeSourceBulkShortStock {
				continue
			}
			if rec, ok := borrowFees.Symbols[row.Symbol]; ok {
				if flag, ok := marketEventBorrowFeeFlag(row.Symbol, rec, borrowFees, now); ok {
					res.Flags = append(res.Flags, flag)
				}
			}
		}
	}

	slices.SortFunc(res.Flags, func(a, b rpc.MarketEventFlag) int {
		if c := cmpMarketEventSeverity(a.Severity, b.Severity); c != 0 {
			return c
		}
		if c := strings.Compare(a.Symbol, b.Symbol); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	for _, flag := range res.Flags {
		res.BySymbol[flag.Symbol] = append(res.BySymbol[flag.Symbol], flag)
	}
	if len(res.BySymbol) == 0 {
		res.BySymbol = nil
	}
	// Snapshot authority is the completion boundary, not request start. Broker
	res.AsOf = c.now().UTC()
	res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
	return res
}

func (c *marketEventCache) loadRegSHO(ctx context.Context, now time.Time) (marketEventRegSHOEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if !c.regSHO.FetchedAt.IsZero() && now.Sub(c.regSHO.FetchedAt) <= c.regSHOFreshFor {
		entry := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		return entry, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusOK, entry.AsOf, now, marketEventsRegSHOMaxAge, "high", regSHOSourceNotes()), nil
	}
	if !c.regSHOFailedAt.IsZero() && now.Sub(c.regSHOFailedAt) <= marketEventsRegSHORetryAfter {
		cached := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		return regSHOFallback(cached, now, errMarketEventRetrySuppressed)
	}
	c.mu.Unlock()

	entry, err := fetchLatestNasdaqRegSHO(ctx, now)
	if err != nil {
		c.mu.Lock()
		c.regSHOFailedAt = now
		cached := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		return regSHOFallback(cached, now, err)
	}
	entry.FetchedAt = now
	if err := c.persistRegSHO(ctx, entry); err != nil {
		c.mu.Lock()
		c.regSHOFailedAt = now
		cached := cloneRegSHOEntry(c.regSHO)
		c.mu.Unlock()
		return regSHOFallback(cached, now, fmt.Errorf("persist normalized Reg SHO snapshot: %w", err))
	}
	c.mu.Lock()
	c.regSHO = cloneRegSHOEntry(entry)
	c.regSHOFailedAt = time.Time{}
	c.mu.Unlock()
	return entry, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusOK, entry.AsOf, now, marketEventsRegSHOMaxAge, "high", regSHOSourceNotes()), nil
}

// errMarketEventRetrySuppressed marks the "recent failure, retry window
var errMarketEventRetrySuppressed = errors.New("recent fetch failure; retry suppressed")

// regSHOFallback serves the stale cached list when one exists, the
func regSHOFallback(cached marketEventRegSHOEntry, now time.Time, cause error) (marketEventRegSHOEntry, rpc.SourceHealth, error) {
	if len(cached.Symbols) > 0 {
		health := marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusStale, cached.AsOf, now, marketEventsRegSHOMaxAge, "medium-low", []string{"using stale cached Nasdaq Reg SHO threshold list: " + cause.Error()})
		health.AgeSeconds = int64(now.Sub(cached.FetchedAt).Seconds())
		return cached, health, nil
	}
	return marketEventRegSHOEntry{}, marketEventSourceHealth("reg_sho_threshold", rpc.SourceStatusUnknown, now, now, marketEventsRegSHOMaxAge, "low", []string{cause.Error()}), cause
}

func (c *marketEventCache) loadHalts(ctx context.Context, now time.Time) (marketEventHaltsEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if !c.halts.FetchedAt.IsZero() && now.Sub(c.halts.FetchedAt) <= c.haltsFreshFor {
		entry := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		return entry, haltsOKHealth(entry, now, c.haltsFreshFor), nil
	}
	if !c.haltsFailedAt.IsZero() && now.Sub(c.haltsFailedAt) <= marketEventsHaltsRetryAfter {
		cached := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		return haltsFallback(cached, now, c.haltsFreshFor, errMarketEventRetrySuppressed)
	}
	c.mu.Unlock()

	entry, err := fetchNasdaqTradeHalts(ctx)
	if err != nil {
		c.mu.Lock()
		c.haltsFailedAt = now
		cached := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		return haltsFallback(cached, now, c.haltsFreshFor, err)
	}
	entry.FetchedAt = now
	if err := c.persistHalts(ctx, entry); err != nil {
		c.mu.Lock()
		c.haltsFailedAt = now
		cached := cloneHaltsEntry(c.halts)
		c.mu.Unlock()
		return haltsFallback(cached, now, c.haltsFreshFor, fmt.Errorf("persist normalized trading-halts snapshot: %w", err))
	}
	c.mu.Lock()
	c.halts = cloneHaltsEntry(entry)
	c.haltsFailedAt = time.Time{}
	c.mu.Unlock()
	return entry, haltsOKHealth(entry, now, c.haltsFreshFor), nil
}

// haltsOKHealth ages the successful row by the fetch, not the feed's own
func haltsOKHealth(entry marketEventHaltsEntry, now time.Time, freshFor time.Duration) rpc.SourceHealth {
	health := marketEventSourceHealth("trading_halts", rpc.SourceStatusOK, entry.AsOf, now, freshFor, "high", nil)
	if !entry.FetchedAt.IsZero() {
		health.AgeSeconds = int64(now.Sub(entry.FetchedAt).Seconds())
	}
	return health
}

// haltsFallback mirrors regSHOFallback for the trade-halts feed.
func haltsFallback(cached marketEventHaltsEntry, now time.Time, freshFor time.Duration, cause error) (marketEventHaltsEntry, rpc.SourceHealth, error) {
	if len(cached.Records) > 0 {
		health := marketEventSourceHealth("trading_halts", rpc.SourceStatusStale, cached.AsOf, now, freshFor, "medium-low", []string{"using stale cached Nasdaq trade-halt RSS feed: " + cause.Error()})
		health.AgeSeconds = int64(now.Sub(cached.FetchedAt).Seconds())
		return cached, health, nil
	}
	return marketEventHaltsEntry{}, marketEventSourceHealth("trading_halts", rpc.SourceStatusUnknown, now, now, freshFor, "low", []string{cause.Error()}), cause
}

func (c *marketEventCache) loadBorrowFees(ctx context.Context, now time.Time) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	c.borrowFeesRefreshMu.Lock()
	defer c.borrowFeesRefreshMu.Unlock()

	c.mu.Lock()
	cached := cloneBorrowFeeEntry(c.borrowFees)
	lastAttempt := cloneBorrowFeeAttempt(c.borrowFeesLastAttempt)
	if !borrowFeeSourceDue(now) {
		c.mu.Unlock()
		return borrowFeesNotDue(cached, lastAttempt, now)
	}
	if borrowFeeEntryFresh(cached, now) {
		c.mu.Unlock()
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusOK, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium", []string{"IBKR short-stock availability fee rate"})
		health.RefreshState = rpc.SourceRefreshCurrent
		return cached, health, nil
	}
	if lastAttempt != nil && lastAttempt.Outcome == marketEventBorrowFeeOutcomeFailure && lastAttempt.NextAttempt != nil && now.Before(*lastAttempt.NextAttempt) {
		c.mu.Unlock()
		entry, health, err := borrowFeesFallback(cached, now, lastAttempt.Failure)
		health.RefreshState = rpc.SourceRefreshFetchFailedBackoff
		health.NextAttempt = cloneBorrowFeeTimePtr(lastAttempt.NextAttempt)
		return entry, health, err
	}
	c.mu.Unlock()

	attemptedAt := now.UTC()
	entry, err := fetchIBKRBorrowFees(ctx)
	completedAt := attemptedAt
	if err != nil {
		failure := borrowFeeFailureFromError(err, completedAt)
		next := completedAt.Add(marketEventsBorrowFeeRetryAfter).UTC()
		attempt := marketEventBorrowFeeAttempt{
			Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: attemptedAt,
			CompletedAt: completedAt, NextAttempt: &next, Failure: &failure,
		}
		if persistErr := c.persistBorrowFeeFailure(ctx, cached, attempt); persistErr != nil {
			persistFailure := borrowFeeSourceFailure(rpc.SourceFailureAuthorityWriteFailed, rpc.SourceFailureStageAuthorityPersist, completedAt, false)
			entry, health, fallbackErr := borrowFeesFallback(cached, now, &persistFailure)
			health.RefreshState = rpc.SourceRefreshFetchFailed
			return entry, health, fallbackErr
		}
		entry, health, fallbackErr := borrowFeesFallback(cached, now, &failure)
		health.RefreshState = rpc.SourceRefreshFetchFailed
		health.NextAttempt = cloneBorrowFeeTimePtr(&next)
		return entry, health, fallbackErr
	}
	entry.FetchedAt = completedAt
	if err := c.persistBorrowFeeSuccess(ctx, entry, attemptedAt, completedAt); err != nil {
		persistFailure := borrowFeeSourceFailure(rpc.SourceFailureAuthorityWriteFailed, rpc.SourceFailureStageAuthorityPersist, completedAt, false)
		entry, health, fallbackErr := borrowFeesFallback(cached, now, &persistFailure)
		health.RefreshState = rpc.SourceRefreshFetchFailed
		return entry, health, fallbackErr
	}
	status := rpc.SourceStatusOK
	confidence := "medium"
	if !borrowFeeEntryFresh(entry, now) {
		status = rpc.SourceStatusStale
		confidence = "medium-low"
	}
	health := marketEventSourceHealth("borrow_fee", status, entry.AsOf, now, marketEventsBorrowFeeMaxAge, confidence, []string{"IBKR short-stock availability fee rate"})
	health.RefreshState = rpc.SourceRefreshCurrent
	return entry, health, nil
}

func borrowFeeEntryFresh(entry marketEventBorrowFeeEntry, now time.Time) bool {
	if entry.AsOf.IsZero() || entry.AsOf.After(now) {
		return false
	}
	return now.Sub(entry.AsOf) <= marketEventsBorrowFeeFreshFor
}

func borrowFeeSourceDue(now time.Time) bool {
	session, err := marketcal.NewWithClock(func() time.Time { return now }).SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || session.State == marketcal.StateUnknown {
		return true
	}
	return session.IsOpen
}

func borrowFeesNotDue(cached marketEventBorrowFeeEntry, lastAttempt *marketEventBorrowFeeAttempt, now time.Time) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	if len(cached.Symbols) == 0 {
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusUnknown, now, now, marketEventsBorrowFeeMaxAge, "low", []string{"IBKR borrow-fee source is outside its official US-equity refresh window"})
		health.RefreshState = rpc.SourceRefreshNotDue
		applyBorrowFeeLastFailure(&health, lastAttempt)
		return marketEventBorrowFeeEntry{}, health, nil
	}
	status := rpc.SourceStatusStale
	if completedDate, _, ok := lastCompletedMarketSession(now, marketcal.MarketUSEquity); ok && !cached.AsOf.IsZero() && !cached.AsOf.After(now) {
		ny, err := time.LoadLocation("America/New_York")
		if err == nil && cached.AsOf.In(ny).Format("2006-01-02") == completedDate {
			status = rpc.SourceStatusOK
		}
	} else if !cached.AsOf.IsZero() && !cached.AsOf.After(now) && now.Sub(cached.AsOf) <= marketEventsBorrowFeeMaxAge {
		status = rpc.SourceStatusOK
	}
	health := marketEventSourceHealth("borrow_fee", status, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium-low", []string{"serving last-good IBKR borrow-fee data; no regular-session refresh is due"})
	health.RefreshState = rpc.SourceRefreshNotDue
	applyBorrowFeeLastFailure(&health, lastAttempt)
	return cached, health, nil
}

// borrowFeesFallback mirrors regSHOFallback for the IBKR short-stock
func borrowFeesFallback(cached marketEventBorrowFeeEntry, now time.Time, failure *rpc.SourceFailure) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	cause := borrowFeeFailureError(failure)
	if len(cached.Symbols) > 0 {
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusStale, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium-low", []string{"using stale cached IBKR short-stock availability; latest refresh " + cause.Error()})
		health.LastFailure = cloneBorrowFeeSourceFailure(failure)
		return cached, health, nil
	}
	health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusUnknown, now, now, marketEventsBorrowFeeMaxAge, "low", []string{"IBKR borrow-fee data is unavailable; latest refresh " + cause.Error()})
	health.LastFailure = cloneBorrowFeeSourceFailure(failure)
	return marketEventBorrowFeeEntry{}, health, cause
}

func fetchLatestNasdaqRegSHO(ctx context.Context, now time.Time) (marketEventRegSHOEntry, error) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	base := now.In(ny)
	var lastErr error
	for daysBack := range 8 {
		date := base.AddDate(0, 0, -daysBack)
		endpoint := "https://www.nasdaqtrader.com/dynamic/symdir/regsho/nasdaqth" + date.Format("20060102") + ".txt"
		entry, err := fetchNasdaqRegSHO(ctx, endpoint)
		if err == nil {
			if entry.AsOf.IsZero() {
				entry.AsOf = time.Date(date.Year(), date.Month(), date.Day(), 23, 0, 0, 0, ny).UTC()
			}
			return entry, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return marketEventRegSHOEntry{}, lastErr
	}
	return marketEventRegSHOEntry{}, fmt.Errorf("no Nasdaq Reg SHO threshold file found")
}

func fetchNasdaqRegSHO(ctx context.Context, endpoint string) (marketEventRegSHOEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := marketEventsHTTPClient.Do(req)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return marketEventRegSHOEntry{}, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	entry, err := parseNasdaqRegSHO(resp.Body)
	if err != nil {
		return marketEventRegSHOEntry{}, err
	}
	entry.SourceURL = endpoint
	return entry, nil
}

func parseNasdaqRegSHO(r io.Reader) (marketEventRegSHOEntry, error) {
	reader := csv.NewReader(r)
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	entry := marketEventRegSHOEntry{Symbols: map[string]marketEventRegSHORecord{}}
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return marketEventRegSHOEntry{}, fmt.Errorf("read Nasdaq Reg SHO row: %w", err)
		}
		if len(rec) == 1 {
			raw := strings.TrimSpace(rec[0])
			if len(raw) >= 14 {
				if ts, err := time.Parse("20060102150405", raw[:14]); err == nil {
					entry.AsOf = ts.UTC()
				}
			}
			continue
		}
		if len(rec) < 5 || strings.EqualFold(strings.TrimSpace(rec[0]), "Symbol") {
			continue
		}
		flag := strings.ToUpper(strings.TrimSpace(rec[3]))
		if flag != "Y" {
			continue
		}
		sym := normSym(rec[0])
		if sym == "" {
			continue
		}
		entry.Symbols[sym] = marketEventRegSHORecord{
			Symbol:         sym,
			SecurityName:   strings.TrimSpace(rec[1]),
			MarketCategory: strings.TrimSpace(rec[2]),
			Rule3210:       strings.TrimSpace(rec[4]),
		}
	}
	return entry, nil
}

func fetchIBKRBorrowFeesFTP(ctx context.Context) (marketEventBorrowFeeEntry, error) {
	const endpoint = "ftp://ftp3.interactivebrokers.com/usa.txt"
	body, err := fetchFTPFile(ctx, "ftp3.interactivebrokers.com:21", "shortstock", "", "usa.txt")
	if err != nil {
		return marketEventBorrowFeeEntry{}, err
	}
	return parseIBKRBorrowFeeDownload(body, endpoint)
}

func parseIBKRBorrowFeeDownload(body, endpoint string) (marketEventBorrowFeeEntry, error) {
	entry, err := parseIBKRBorrowFees(strings.NewReader(body))
	if err != nil {
		if _, ok := errors.AsType[*borrowFeeFetchError](err); ok {
			return marketEventBorrowFeeEntry{}, err
		}
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
	}
	entry.SourceURL = strings.TrimSpace(endpoint)
	if entry.SourceURL == "" {
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
	}
	return entry, nil
}

func parseIBKRBorrowFees(r io.Reader) (marketEventBorrowFeeEntry, error) {
	reader := csv.NewReader(r)
	reader.Comma = '|'
	reader.FieldsPerRecord = -1
	entry := marketEventBorrowFeeEntry{Symbols: map[string]marketEventBorrowFeeRecord{}}
	seenBOF := false
	seenHeader := false
	for {
		rec, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
		}
		if len(rec) == 0 {
			continue
		}
		tag := strings.TrimSpace(rec[0])
		switch {
		case tag == "#BOF":
			if seenBOF || len(rec) < 3 {
				return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
			}
			entry.AsOf = parseIBKRBorrowFeeAsOf(rec[1], rec[2])
			if entry.AsOf.IsZero() {
				return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
			}
			seenBOF = true
			continue
		case tag == "#SYM":
			if seenHeader || !validIBKRBorrowFeeHeader(rec) {
				return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
			}
			seenHeader = true
			continue
		case strings.HasPrefix(tag, "#"):
			continue
		}
		if !seenBOF || !seenHeader {
			return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
		}
		if len(rec) < 8 {
			continue
		}
		sym := normSym(rec[0])
		if sym == "" {
			continue
		}
		feeRate, feeOK := parseFloatField(rec[6])
		if !feeOK {
			continue
		}
		rebateRate, rebateOK := parseFloatField(rec[5])
		if !rebateOK {
			continue
		}
		available, availableOK := parseIntField(rec[7])
		if !availableOK || available < 0 {
			continue
		}
		entry.Symbols[sym] = marketEventBorrowFeeRecord{
			Symbol:     sym,
			Currency:   strings.TrimSpace(rec[1]),
			Name:       strings.TrimSpace(rec[2]),
			ConID:      strings.TrimSpace(rec[3]),
			ISIN:       strings.TrimSpace(rec[4]),
			RebateRate: rebateRate,
			FeeRate:    feeRate,
			Available:  available,
		}
	}
	if !seenBOF || !seenHeader || entry.AsOf.IsZero() || len(entry.Symbols) == 0 {
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
	}
	return entry, nil
}

func validIBKRBorrowFeeHeader(rec []string) bool {
	want := []string{"#SYM", "CUR", "NAME", "CON", "ISIN", "REBATERATE", "FEERATE", "AVAILABLE"}
	if len(rec) < len(want) {
		return false
	}
	for i, field := range want {
		if strings.ToUpper(strings.TrimSpace(rec[i])) != field {
			return false
		}
	}
	return true
}

type borrowFeeFetchError struct {
	code      string
	stage     string
	retryable bool
}

func newBorrowFeeFetchError(code, stage string, retryable bool) error {
	return &borrowFeeFetchError{code: code, stage: stage, retryable: retryable}
}

func (e *borrowFeeFetchError) Error() string {
	if e == nil {
		return "failed at ftp_control_connect (transport_failed)"
	}
	return fmt.Sprintf("failed at %s (%s)", e.stage, e.code)
}

func borrowFeeTransportFetchError(stage string, err error) error {
	code := rpc.SourceFailureTransportFailed
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr):
		code = rpc.SourceFailureDNSFailed
	case errors.Is(err, context.DeadlineExceeded):
		code = rpc.SourceFailureTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		code = rpc.SourceFailureConnectionRefused
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			code = rpc.SourceFailureTimeout
		}
	}
	return newBorrowFeeFetchError(code, stage, true)
}

func borrowFeeFTPResponseFetchError(stage string, err error) error {
	var netErr net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) {
		return borrowFeeTransportFetchError(stage, err)
	}
	return newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, stage, true)
}

func borrowFeeFailureFromError(err error, failedAt time.Time) rpc.SourceFailure {
	if sourceErr, ok := errors.AsType[*borrowFeeFetchError](err); ok {
		return borrowFeeSourceFailure(sourceErr.code, sourceErr.stage, failedAt, sourceErr.retryable)
	}
	return borrowFeeSourceFailure(rpc.SourceFailureTransportFailed, rpc.SourceFailureStageFTPControlConnect, failedAt, true)
}

func borrowFeeSourceFailure(code, stage string, failedAt time.Time, retryable bool) rpc.SourceFailure {
	return rpc.SourceFailure{Code: code, Stage: stage, FailedAt: failedAt.UTC(), Retryable: retryable}
}

func borrowFeeFailureError(failure *rpc.SourceFailure) error {
	if failure == nil {
		return newBorrowFeeFetchError(rpc.SourceFailureTransportFailed, rpc.SourceFailureStageFTPControlConnect, true)
	}
	return newBorrowFeeFetchError(failure.Code, failure.Stage, failure.Retryable)
}

func validateBorrowFeeSourceFailure(failure rpc.SourceFailure) error {
	if !rpc.ValidSourceFailure(&failure) || failure.FailedAt.IsZero() {
		return errors.New("invalid borrow-fee source failure")
	}
	switch failure.Code {
	case rpc.SourceFailureTimeout, rpc.SourceFailureDNSFailed, rpc.SourceFailureConnectionRefused,
		rpc.SourceFailureTransportFailed, rpc.SourceFailureProtocolRejected,
		rpc.SourceFailureAuthenticationRejected, rpc.SourceFailureInvalidPayload,
		rpc.SourceFailureAuthorityWriteFailed:
	default:
		return errors.New("invalid borrow-fee source failure code")
	}
	switch failure.Stage {
	case rpc.SourceFailureStageFTPControlConnect, rpc.SourceFailureStageFTPGreeting,
		rpc.SourceFailureStageFTPAuthenticate, rpc.SourceFailureStageFTPPassiveNegotiate,
		rpc.SourceFailureStageFTPPassiveConnect, rpc.SourceFailureStageFTPRetrieve,
		rpc.SourceFailureStageBorrowParse, rpc.SourceFailureStageAuthorityPersist:
	default:
		return errors.New("invalid borrow-fee source failure stage")
	}
	if (failure.Code == rpc.SourceFailureAuthorityWriteFailed) != (failure.Stage == rpc.SourceFailureStageAuthorityPersist) {
		return errors.New("invalid borrow-fee authority failure pairing")
	}
	return nil
}

func applyBorrowFeeLastFailure(health *rpc.SourceHealth, attempt *marketEventBorrowFeeAttempt) {
	if health == nil || attempt == nil || attempt.Outcome != marketEventBorrowFeeOutcomeFailure {
		return
	}
	health.LastFailure = cloneBorrowFeeSourceFailure(attempt.Failure)
}

func cloneBorrowFeeSourceFailure(in *rpc.SourceFailure) *rpc.SourceFailure {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneBorrowFeeTimePtr(in *time.Time) *time.Time {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func parseIBKRBorrowFeeAsOf(rawDate, rawTime string) time.Time {
	raw := strings.TrimSpace(rawDate) + " " + strings.TrimSpace(rawTime)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	if t, err := time.ParseInLocation("2006.01.02 15:04:05", raw, ny); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func fetchFTPFile(ctx context.Context, addr, user, pass, path string) (string, error) {
	dialer := net.Dialer{Timeout: marketEventsFTPDialTimeout}
	control, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPControlConnect, err)
	}
	defer control.Close()
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = control.SetDeadline(deadline)
	reader := bufio.NewReader(control)
	if code, _, err := readFTPResponse(reader); err != nil {
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPGreeting, err)
	} else if code != 220 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPGreeting, true)
	}
	if err := writeFTPCommand(control, "USER "+user); err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPAuthenticate, err)
	}
	code, _, err := readFTPResponse(reader)
	if err != nil {
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPAuthenticate, err)
	}
	if code == 331 {
		if err := writeFTPCommand(control, "PASS "+pass); err != nil {
			return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPAuthenticate, err)
		}
		code, _, err = readFTPResponse(reader)
		if err != nil {
			return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPAuthenticate, err)
		}
	}
	if code != 230 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureAuthenticationRejected, rpc.SourceFailureStageFTPAuthenticate, true)
	}
	if err := writeFTPCommand(control, "TYPE I"); err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPPassiveNegotiate, err)
	}
	if code, _, err := readFTPResponse(reader); err != nil {
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPPassiveNegotiate, err)
	} else if code != 200 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPPassiveNegotiate, true)
	}
	if err := writeFTPCommand(control, "PASV"); err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPPassiveNegotiate, err)
	}
	code, line, err := readFTPResponse(reader)
	if err != nil {
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPPassiveNegotiate, err)
	}
	if code != 227 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPPassiveNegotiate, true)
	}
	dataAddr, err := ftpPassiveAddr(line)
	if err != nil {
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPPassiveNegotiate, true)
	}
	data, err := dialer.DialContext(ctx, "tcp", dataAddr)
	if err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPPassiveConnect, err)
	}
	_ = data.SetDeadline(deadline)
	if err := writeFTPCommand(control, "RETR "+path); err != nil {
		data.Close()
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPRetrieve, err)
	}
	code, _, err = readFTPResponse(reader)
	if err != nil {
		data.Close()
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPRetrieve, err)
	}
	if code != 125 && code != 150 {
		data.Close()
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPRetrieve, true)
	}
	body, readErr := io.ReadAll(data)
	closeErr := data.Close()
	if readErr != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPRetrieve, readErr)
	}
	if closeErr != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPRetrieve, closeErr)
	}
	code, _, err = readFTPResponse(reader)
	if err != nil {
		return "", borrowFeeFTPResponseFetchError(rpc.SourceFailureStageFTPRetrieve, err)
	}
	if code != 226 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, rpc.SourceFailureStageFTPRetrieve, true)
	}
	_ = writeFTPCommand(control, "QUIT")
	return string(body), nil
}

func readFTPResponse(reader *bufio.Reader) (int, string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 3 {
		return 0, line, fmt.Errorf("short FTP response")
	}
	code, err := strconv.Atoi(line[:3])
	if err != nil {
		return 0, line, err
	}
	if len(line) > 3 && line[3] == '-' {
		prefix := line[:3] + " "
		for {
			next, err := reader.ReadString('\n')
			if err != nil {
				return code, line, err
			}
			next = strings.TrimRight(next, "\r\n")
			line += "\n" + next
			if strings.HasPrefix(next, prefix) {
				break
			}
		}
	}
	return code, line, nil
}

func writeFTPCommand(conn net.Conn, cmd string) error {
	_, err := fmt.Fprintf(conn, "%s\r\n", cmd)
	return err
}

func ftpPassiveAddr(line string) (string, error) {
	match := regexp.MustCompile(`\((\d+),(\d+),(\d+),(\d+),(\d+),(\d+)\)`).FindStringSubmatch(line)
	if len(match) != 7 {
		return "", fmt.Errorf("parse PASV address from %q", line)
	}
	parts := make([]byte, 6)
	for i := 1; i < len(match); i++ {
		v, err := strconv.ParseUint(match[i], 10, 8)
		if err != nil {
			return "", fmt.Errorf("parse PASV address from %q: part %d out of byte range: %w", line, i, err)
		}
		parts[i-1] = byte(v)
	}
	host := net.IPv4(parts[0], parts[1], parts[2], parts[3]).String()
	port := int(parts[4])*256 + int(parts[5])
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func parseFloatField(raw string) (float64, bool) {
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "%"))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0)
}

func parseIntField(raw string) (int64, bool) {
	raw = strings.TrimSpace(strings.ReplaceAll(raw, ",", ""))
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	return v, err == nil
}

func regSHOSourceNotes() []string {
	return []string{"Nasdaq-listed threshold securities source; non-Nasdaq listing-exchange threshold feeds remain outside V1."}
}

func fetchNasdaqTradeHalts(ctx context.Context) (marketEventHaltsEntry, error) {
	const endpoint = "https://www.nasdaqtrader.com/rss.aspx?feed=tradehalts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	req.Header.Set("User-Agent", "Go-http-client/1.1")
	resp, err := marketEventsHTTPClient.Do(req)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return marketEventHaltsEntry{}, fmt.Errorf("GET %s: HTTP %s", endpoint, resp.Status)
	}
	entry, err := parseNasdaqTradeHalts(resp.Body)
	if err != nil {
		return marketEventHaltsEntry{}, err
	}
	entry.SourceURL = endpoint
	return entry, nil
}

type nasdaqTradeHaltsRSS struct {
	Channel nasdaqTradeHaltsChannel `xml:"channel"`
}

type nasdaqTradeHaltsChannel struct {
	PubDate string                 `xml:"pubDate"`
	Items   []nasdaqTradeHaltsItem `xml:"item"`
}

type nasdaqTradeHaltsItem struct {
	HaltDate            string `xml:"HaltDate"`
	HaltTime            string `xml:"HaltTime"`
	IssueSymbol         string `xml:"IssueSymbol"`
	IssueName           string `xml:"IssueName"`
	Market              string `xml:"Market"`
	ReasonCode          string `xml:"ReasonCode"`
	PauseThresholdPrice string `xml:"PauseThresholdPrice"`
	ResumptionDate      string `xml:"ResumptionDate"`
	ResumptionQuoteTime string `xml:"ResumptionQuoteTime"`
	ResumptionTradeTime string `xml:"ResumptionTradeTime"`
}

func parseNasdaqTradeHalts(r io.Reader) (marketEventHaltsEntry, error) {
	var feed nasdaqTradeHaltsRSS
	decoder := xml.NewDecoder(r)
	if err := decoder.Decode(&feed); err != nil {
		return marketEventHaltsEntry{}, fmt.Errorf("decode Nasdaq trade halt RSS: %w", err)
	}
	entry := marketEventHaltsEntry{}
	if pubDate := strings.TrimSpace(feed.Channel.PubDate); pubDate != "" {
		if t, err := time.Parse(time.RFC1123, pubDate); err == nil {
			entry.AsOf = t.UTC()
		} else if t, err := time.Parse(time.RFC1123Z, pubDate); err == nil {
			entry.AsOf = t.UTC()
		}
	}
	for _, item := range feed.Channel.Items {
		sym := normSym(item.IssueSymbol)
		if sym == "" {
			continue
		}
		rec := marketEventHaltRecord{
			Symbol:              sym,
			IssueName:           strings.TrimSpace(item.IssueName),
			Market:              strings.TrimSpace(item.Market),
			ReasonCode:          strings.ToUpper(strings.TrimSpace(item.ReasonCode)),
			PauseThresholdPrice: strings.TrimSpace(item.PauseThresholdPrice),

			HaltedAt:          parseNasdaqHaltTime(item.HaltDate, item.HaltTime),
			ResumptionQuoteAt: parseNasdaqHaltTime(item.ResumptionDate, item.ResumptionQuoteTime),
			ResumptionTradeAt: parseNasdaqHaltTime(item.ResumptionDate, item.ResumptionTradeTime)}
		entry.Records = append(entry.Records, rec)
	}
	return entry, nil
}

func parseNasdaqHaltTime(rawDate, rawTime string) time.Time {
	rawDate = strings.TrimSpace(rawDate)
	rawTime = strings.TrimSpace(rawTime)
	if rawDate == "" || rawTime == "" {
		return time.Time{}
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		ny = time.UTC
	}
	for _, layout := range []string{"01/02/2006 15:04:05.000", "01/02/2006 15:04:05"} {
		if t, err := time.ParseInLocation(layout, rawDate+" "+rawTime, ny); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func marketEventRegSHOFlag(sym string, rec marketEventRegSHORecord, source marketEventRegSHOEntry, now time.Time) rpc.MarketEventFlag {
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventRegSHOThreshold,
		Symbol:     sym,
		Label:      "Reg SHO",
		Status:     rpc.MarketEventStatusActive,
		Severity:   rpc.MarketEventSeverityWatch,
		Role:       rpc.MarketEventRoleContext,
		Source:     "Nasdaq Reg SHO threshold list",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Details: compactNonEmptyStrings(
			"threshold security",
			"market_category="+rec.MarketCategory,
			"rule_3210="+rec.Rule3210,
			rec.SecurityName,
		),
	}
}

func marketEventHaltFlag(sym string, rec marketEventHaltRecord, source marketEventHaltsEntry, now time.Time) (rpc.MarketEventFlag, bool) {
	status := rpc.MarketEventStatusActive
	if !rec.ResumptionTradeAt.IsZero() {
		if now.Sub(rec.ResumptionTradeAt) > marketEventsRecentHaltWindow {
			return rpc.MarketEventFlag{}, false
		}
		status = rpc.MarketEventStatusRecent
	}
	id := rpc.MarketEventHaltRegulatoryOrNews
	label := "Halt"
	severity := rpc.MarketEventSeverityBlock
	role := rpc.MarketEventRoleHardBlocker
	if status == rpc.MarketEventStatusRecent {
		severity = rpc.MarketEventSeverityWatch
		role = rpc.MarketEventRoleProposalModifier
	}
	if marketEventLULDReason(rec.ReasonCode) {
		id = rpc.MarketEventLULDRecent
		label = "LULD"
		if status == rpc.MarketEventStatusActive {
			label = "LULD active"
		} else {
			label = "LULD recent"
		}
	}
	flag := rpc.MarketEventFlag{
		ID:         id,
		Symbol:     sym,
		Label:      label,
		Status:     status,
		Severity:   severity,
		Role:       role,
		Source:     "Nasdaq trade halt RSS",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Details: compactNonEmptyStrings(
			"reason_code="+rec.ReasonCode,
			rec.IssueName,
			rec.Market,
			"pause_threshold="+rec.PauseThresholdPrice,
		),
	}
	if status == rpc.MarketEventStatusActive {
		flag.ExpiresAt = rec.ResumptionTradeAt
	}
	return flag, true
}

func marketEventLULDReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "M", "T7":
		return true
	default:
		return false
	}
}

func (c *marketEventCache) borrowInventory(ctx context.Context, symbols []string, subs *subManager, connector *ibkrlib.Connector, now time.Time, res *rpc.MarketEventsResult) rpc.SourceHealth {
	if connector == nil || subs == nil {
		return marketEventSourceHealth("borrow_inventory", rpc.SourceStatusUnknown, now, now, 2*time.Minute, "low", []string{"IBKR gateway is unavailable; shortable-share inventory is unknown"})
	}
	// Per-symbol probe results land in index-addressed slots so the
	// bounded workers never share mutable state; flags are merged after
	// the fan-out (res.Flags gets a global sort downstream anyway).
	type borrowProbe struct {
		observed bool
		hasFlag  bool
		flag     rpc.MarketEventFlag
		record   *marketEventBorrowInventoryRecord
	}
	probes := make([]borrowProbe, len(symbols))
	var jobs []int
	skipped := 0
	for i, sym := range symbols {
		if c.shortableAbsentRecently(sym, now) {
			skipped++
			continue
		}
		jobs = append(jobs, i)
	}
	runBounded(jobs, marketEventsBorrowPollWorkers, func(i int) {
		sym := symbols[i]
		holdCtx, cancel := context.WithTimeout(ctx, marketEventsBorrowPollBudget)
		defer cancel()
		release, err := subs.Hold(holdCtx, sym)
		if err != nil {
			return
		}
		defer release()
		pollErr := pollMarketData(holdCtx, connector, sym, time.Now().Add(marketEventsBorrowPollBudget), func(md *ibkrlib.MarketData) bool {
			return md.ShortableObserved
		})
		if md := connector.MarketDataSnapshot()[sym]; md != nil && md.ShortableObserved {
			probes[i].observed = true
			record := marketEventBorrowInventoryRecord{
				Symbol: sym, ShortableShares: md.ShortableShares, AsOf: md.ShortableTickAt,
				DataType: md.DataType, Delayed: md.IsDelayed,
			}
			probes[i].record = &record
			if flag, ok := marketEventBorrowInventoryFlag(sym, *md, now); ok {
				probes[i].hasFlag = true
				probes[i].flag = flag
			}
			return
		}
		// Tick absent. Record the absence only when this probe genuinely
		if ctx.Err() == nil && pollErr != nil {
			c.rememberShortableAbsent(sym, now)
		}
	})

	var observed, tight int
	observations := make(map[string]marketEventBorrowInventoryRecord)
	for i := range probes {
		if probes[i].observed {
			observed++
		}
		if probes[i].record != nil {
			observations[probes[i].record.Symbol] = *probes[i].record
		}
		if probes[i].hasFlag {
			tight++
			res.Flags = append(res.Flags, probes[i].flag)
		}
	}
	status := rpc.SourceStatusUnknown
	confidence := "low"
	notes := []string{"shortable-share tick did not arrive for requested symbols"}
	if observed > 0 {
		status = rpc.SourceStatusOK
		confidence = "medium"
		notes = []string{fmt.Sprintf("observed shortable-share inventory for %d/%d symbols", observed, len(symbols))}
		if tight == 0 {
			notes = append(notes, "no tight borrow-inventory flags crossed V1 thresholds")
		}
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("skipped %d symbols whose shortable tick was recently absent; re-probing every %s", skipped, marketEventsShortableAbsentRetry))
	}
	if err := c.persistBorrowInventory(ctx, now, observations); err != nil {
		status = rpc.SourceStatusUnknown
		confidence = "low"
		notes = append(notes, "normalized shortable-share observations were not durably recorded: "+err.Error())
	}
	return marketEventSourceHealth("borrow_inventory", status, now, now, 2*time.Minute, confidence, notes)
}

func marketEventBorrowInventoryFlag(sym string, md ibkrlib.MarketData, now time.Time) (rpc.MarketEventFlag, bool) {
	if !md.ShortableObserved || md.ShortableShares > marketEventsBorrowTightShares {
		return rpc.MarketEventFlag{}, false
	}
	severity := rpc.MarketEventSeverityWatch
	label := "Borrow tight"
	if md.ShortableShares <= marketEventsBorrowExtremeShares {
		severity = rpc.MarketEventSeverityAct
		label = "Borrow scarce"
	}
	value := float64(md.ShortableShares)
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventBorrowInventoryTight,
		Symbol:     sym,
		Label:      label,
		Status:     rpc.MarketEventStatusActive,
		Severity:   severity,
		Role:       rpc.MarketEventRoleProposalModifier,
		Source:     "IBKR generic tick 236",
		AsOf:       md.ShortableTickAt,
		ObservedAt: now,
		Value:      &value,
		Unit:       "shares",
		Details:    []string{"shortable_shares=" + strconv.FormatInt(md.ShortableShares, 10)},
	}, true
}

func marketEventBorrowFeeFlag(sym string, rec marketEventBorrowFeeRecord, source marketEventBorrowFeeEntry, now time.Time) (rpc.MarketEventFlag, bool) {
	if rec.FeeRate < marketEventsBorrowFeeExtremePct {
		return rpc.MarketEventFlag{}, false
	}
	value := rec.FeeRate
	return rpc.MarketEventFlag{
		ID:         rpc.MarketEventBorrowFeeExtreme,
		Symbol:     sym,
		Label:      "Fee extreme",
		Status:     rpc.MarketEventStatusActive,
		Severity:   rpc.MarketEventSeverityAct,
		Role:       rpc.MarketEventRoleProposalModifier,
		Source:     "IBKR short stock availability",
		SourceURL:  source.SourceURL,
		AsOf:       source.AsOf,
		ObservedAt: now,
		Value:      &value,
		Unit:       "pct_annualized",
		Details: compactNonEmptyStrings(
			fmt.Sprintf("fee_rate=%.4f%%", rec.FeeRate),
			fmt.Sprintf("rebate_rate=%.4f%%", rec.RebateRate),
			"available="+strconv.FormatInt(rec.Available, 10),
			rec.Currency,
			rec.Name,
		),
	}, true
}

func marketEventSourceHealth(source, status string, asOf, now time.Time, maxAge time.Duration, confidence string, notes []string) rpc.SourceHealth {
	health := rpc.SourceHealth{
		Source:               source,
		Status:               status,
		AsOf:                 asOf,
		MaxAgeSeconds:        int64(maxAge.Seconds()),
		Confidence:           confidence,
		FingerprintStability: rpc.FingerprintStabilitySemanticBuckets,
		Notes:                notes,
	}
	if !asOf.IsZero() && !now.IsZero() {
		health.AgeSeconds = int64(now.Sub(asOf).Seconds())
	}
	return health
}

func marketEventSourceWarning(scope string, err error) rpc.DataWarning {
	return rpc.DataWarning{
		Code:     scope + "_unavailable",
		Scope:    scope,
		Severity: "data_quality",
		Message:  "Market-event source is unavailable: " + err.Error(),
		Impact:   "The corresponding flag remains unknown, not inactive.",
		Action:   "Retry later or inspect source health before relying on absence of this flag.",
	}
}

func normalizeMarketEventSymbols(raw []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, token := range raw {
		for part := range strings.SplitSeq(token, ",") {
			sym := normSym(part)
			if sym == "" || seen[sym] {
				continue
			}
			seen[sym] = true
			out = append(out, sym)
		}
	}
	slices.Sort(out)
	return out
}

func marketEventSymbolsFromPositions(pos *rpc.PositionsResult) []string {
	if pos == nil {
		return nil
	}
	var raw []string
	for _, stock := range pos.Stocks {
		raw = append(raw, stock.Symbol)
	}
	for _, group := range pos.ByUnderlying {
		raw = append(raw, group.Underlying)
	}
	return normalizeMarketEventSymbols(raw)
}

func cloneRegSHOEntry(in marketEventRegSHOEntry) marketEventRegSHOEntry {
	out := in
	if in.Symbols != nil {
		out.Symbols = make(map[string]marketEventRegSHORecord, len(in.Symbols))
		maps.Copy(out.Symbols, in.Symbols)
	}
	return out
}

func cloneHaltsEntry(in marketEventHaltsEntry) marketEventHaltsEntry {
	out := in
	out.Records = slices.Clone(in.Records)
	return out
}

func cloneBorrowFeeEntry(in marketEventBorrowFeeEntry) marketEventBorrowFeeEntry {
	out := in
	if in.Symbols != nil {
		out.Symbols = make(map[string]marketEventBorrowFeeRecord, len(in.Symbols))
		maps.Copy(out.Symbols, in.Symbols)
	}
	return out
}

func compactNonEmptyStrings(values ...string) []string {
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || strings.HasSuffix(value, "=") {
			continue
		}
		out = append(out, value)
	}
	return out
}

func cmpMarketEventSeverity(a, b string) int {
	rank := func(v string) int {
		switch v {
		case rpc.MarketEventSeverityBlock:
			return 0
		case rpc.MarketEventSeverityAct:
			return 1
		case rpc.MarketEventSeverityWatch:
			return 2
		default:
			return 3
		}
	}
	return rank(a) - rank(b)
}
