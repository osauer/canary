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
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/publichttp"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	marketEventsRegSHOFreshFor      = 12 * time.Hour
	marketEventsRegSHOMaxAge        = 96 * time.Hour
	marketEventsHaltsFreshFor       = time.Minute
	marketEventsBorrowPollBudget    = 2500 * time.Millisecond
	marketEventsInventoryMaxAge     = 2 * time.Minute
	marketEventsInventoryCacheLimit = 512
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

	// marketEventsCanonicalScopeFor bounds how long the held-name scope the
	// daemon last derived classifies explicit-symbol reads. The proposal
	// refresh (30 s) and stress evaluation (1 min) re-derive it.
	marketEventsCanonicalScopeFor = 2 * time.Minute

	// marketEventsBorrowApplicabilityRetain bounds how long a "not_relevant"
	// borrow verdict from a current portfolio stream outlives that stream's
	// currency. It spans reconnects, resubscriptions and short-download
	// repairs so they do not flip the rows to required and back. A longer
	// outage returns them to required within one borrow-fee retry interval.
	marketEventsBorrowApplicabilityRetain = 15 * time.Minute

	// marketEventsShortableAbsentRetry bounds how long a "tick 236 never
	// absence must be re-tested once the tape can plausibly have changed;
	marketEventsShortableAbsentRetry = 30 * time.Minute

	// marketEventsFTPDialTimeout bounds each borrow-fee FTP connect, so a
	// filtered endpoint fails over instead of consuming the attempt.
	marketEventsFTPDialTimeout = 4 * time.Second
	// marketEventsFTPControlTimeout bounds each FTP control command and reply.
	marketEventsFTPControlTimeout = 10 * time.Second
	// marketEventsFTPTransferTimeout bounds the passive-mode file transfer
	// separately: the ~2 MB file alone takes several seconds from IBKR.
	marketEventsFTPTransferTimeout = 45 * time.Second
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

// fetchIBKRBorrowFees acquires the IBKR short-stock file. lastSourceURL names
// the endpoint that served the retained last-good file, which is tried first.
var fetchIBKRBorrowFees = fetchIBKRBorrowFeesFTP

// borrowFeeFTPSource is the IBKR short-stock FTP distribution: endpoints in
// preference order, the published anonymous login, the file and the time
// budgets of one attempt.
type borrowFeeFTPSource struct {
	endpoints       []string
	user, pass      string
	path            string
	dial            func(ctx context.Context, network, addr string) (net.Conn, error)
	controlTimeout  time.Duration
	transferTimeout time.Duration
}

// ibkrBorrowFeeFTP is the production source. ftp3 is the host IBKR documents
// for the file; ftp2 is an IBKR-operated mirror that serves the same files to
// the same published login. Tests replace the dialer and budgets.
var ibkrBorrowFeeFTP = borrowFeeFTPSource{
	endpoints:       []string{"ftp3.interactivebrokers.com", "ftp2.interactivebrokers.com"},
	user:            "shortstock",
	path:            "usa.txt",
	dial:            (&net.Dialer{Timeout: marketEventsFTPDialTimeout}).DialContext,
	controlTimeout:  marketEventsFTPControlTimeout,
	transferTimeout: marketEventsFTPTransferTimeout,
}

type marketEventCache struct {
	mu                        sync.Mutex
	borrowFeesRefreshGate     chan struct{}
	borrowInventoryGate       chan struct{}
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
	shortableAbsent   map[string]time.Time // symbol → when observed absent
	shortableBinding  ibkrlib.ConnectorSessionBinding
	shortableReceipts map[string]marketEventBorrowInventoryRecord

	// *FailedAt remember the last failed fetch per external source so
	// re-fetches. Zero value = no recent failure. Cleared on success.
	regSHOFailedAt time.Time
	haltsFailedAt  time.Time

	// authority is the sole durable runtime store after startup attachment.
	// Borrow-fee failure/backoff is durable; Reg SHO/halt retry timestamps and
	// shortableAbsent remain memory-only control state.
	authority *corestore.Store

	// logger reports borrow-fee failure-class changes and recovery; nil is
	// silent.
	logger *Logger

	// canonical is the held-name scope last derived from the daemon's own
	// positions read. Memory only; see marketEventCanonicalScope.
	canonical marketEventCanonicalScope

	// borrowIrrelevant is the last not-relevant borrow verdict; see
	// borrowApplicability.
	borrowIrrelevant borrowIrrelevance

	// openInventoryTransport, when set, replaces the connector-bound session
	// and tick-236 transport of borrowInventory; tests inject it.
	openInventoryTransport func() (borrowInventoryTransport, bool)
}

// marketEventCanonicalScope is the held-name market-event scope the daemon
// derived from its own positions read (see rpc.MarketEventScope). Source
// health describes the held book, so only a read of exactly these symbols
// records events:* health; an ad-hoc read describes only its request. unquoted
// names expect no market data and are not expected to report shortable shares.
type marketEventCanonicalScope struct {
	symbols, unquoted []string
	broker            brokerStateScope
	derivedAt         time.Time
}

// rememberCanonicalScope replaces the canonical scope. Symbols must already be
// normalized.
func (c *marketEventCache) rememberCanonicalScope(symbols, unquoted []string, broker brokerStateScope) {
	now := c.now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canonical = marketEventCanonicalScope{symbols: slices.Clone(symbols), unquoted: slices.Clone(unquoted), broker: broker, derivedAt: now}
}

// scopeFor classifies a read of normalized symbols against the canonical
// scope, which applies only while it is recent and names the same broker
// scope. unquoted lists the requested names the scope knows expect no market
// data; canonical reports that symbols is exactly the canonical scope.
func (c *marketEventCache) scopeFor(symbols []string, broker brokerStateScope) (unquoted []string, canonical bool) {
	now := c.now().UTC()
	c.mu.Lock()
	scope := c.canonical
	c.mu.Unlock()
	if scope.derivedAt.IsZero() || scope.derivedAt.After(now) || now.Sub(scope.derivedAt) > marketEventsCanonicalScopeFor || !sameBrokerScope(scope.broker, broker) {
		return nil, false
	}
	for _, symbol := range scope.unquoted {
		if _, found := slices.BinarySearch(symbols, symbol); found {
			unquoted = append(unquoted, symbol)
		}
	}
	return unquoted, slices.Equal(symbols, scope.symbols)
}

// shortableAbsentRecently reports whether sym's shortable tick was
// observed absent within the last marketEventsShortableAbsentRetry.
func (c *marketEventCache) shortableAbsentRecently(sym string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	at, ok := c.shortableAbsent[sym]
	return ok && !at.After(now) && now.Sub(at) < marketEventsShortableAbsentRetry
}

// rememberShortableAbsent records that sym ran a full poll budget at now
func (c *marketEventCache) rememberShortableAbsent(sym string, now time.Time, binding ibkrlib.ConnectorSessionBinding) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shortableBinding != binding {
		return
	}
	if c.shortableAbsent == nil {
		c.shortableAbsent = make(map[string]time.Time)
	}
	if len(c.shortableAbsent) >= marketEventsInventoryCacheLimit {
		return
	}
	c.shortableAbsent[sym] = now
}

// clearShortableAbsence drops all absence records. Called on gateway
func (c *marketEventCache) clearShortableAbsence() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.shortableAbsent = nil
	c.shortableReceipts = nil
	c.shortableBinding = ibkrlib.ConnectorSessionBinding{}
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
	// SkippedRows counts malformed data rows the parser left out; their
	// symbols are absent, not observed.
	SkippedRows int `json:"skipped_rows,omitempty"`
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
	// AvailableLowerBound marks a published ">N": at least Available shares,
	// never a scarcity reading.
	AvailableLowerBound bool `json:"available_lower_bound,omitempty"`
	// FeeRateUnpublished and RebateRateUnpublished mark a published "NA": the
	// symbol is observed but that rate is not, and its zero value is no rate.
	FeeRateUnpublished    bool `json:"fee_rate_unpublished,omitempty"`
	RebateRateUnpublished bool `json:"rebate_rate_unpublished,omitempty"`
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
	s.marketEvents.logger = s.logger
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
		symbols = s.canonicalMarketEventSymbols(s.analysisPositions(pos, s.now()))
	}
	res := s.marketEventsForSymbols(ctx, symbols)
	return &res, nil
}

// canonicalMarketEventSymbols derives the held-name market-event scope of pos
// with rpc.MarketEventScope, remembers it as the scope whose reads record
// source health and returns its symbols. Callers pass the daemon's analysis
// positions.
func (s *Server) canonicalMarketEventSymbols(pos *rpc.PositionsResult) []string {
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	symbols, unquoted := rpc.MarketEventScope(pos)
	symbols = normalizeMarketEventSymbols(symbols)
	s.marketEvents.rememberCanonicalScope(symbols, normalizeMarketEventSymbols(unquoted), s.currentBrokerStateScope())
	return symbols
}

func (s *Server) marketEventsForSymbols(ctx context.Context, symbols []string) rpc.MarketEventsResult {
	if s.marketEvents == nil {
		s.installMarketEventCache()
	}
	symbols = normalizeMarketEventSymbols(symbols)
	_, canonical := s.marketEvents.scopeFor(symbols, s.currentBrokerStateScope())
	connector := s.gatewayConnector()
	// Fence the whole acquisition. Capturing after it would attach a late
	// old-session inventory result to a newly connected broker session.
	binding, _ := connector.CaptureSession()
	result := s.marketEvents.snapshot(ctx, symbols, s.subs, connector, s.currentBrokerStateScope)
	if canonical {
		s.observeEventHealth(result, connector, binding)
	}
	return result
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

	var scopeProvider func() brokerStateScope
	var broker brokerStateScope
	if len(scopeProviders) > 0 {
		scopeProvider = scopeProviders[0]
		broker = scopeProvider()
	}
	unquoted, canonical := c.scopeFor(symbols, broker)
	// Portfolio applicability is independent of provider cadence, so it is
	// derived once here for both borrow sources rather than by the fallback.
	borrowApplicability := c.borrowApplicability(symbols, canonical, connector, scopeProvider)
	borrowHealth := c.borrowInventory(ctx, symbols, unquoted, subs, connector, now, &res)
	borrowHealth.Applicability = borrowApplicability
	res.SourceHealth = append(res.SourceHealth, borrowHealth)
	borrowFees, borrowFeeHealth, err := c.loadBorrowFees(ctx)
	if err != nil {
		res.WarningDetails = append(res.WarningDetails, marketEventSourceWarning("borrow_fee", err))
	}
	bulkBorrowFeeUsable := borrowFeeFTPPolicyUsable(borrowFeeHealth)
	res.BorrowFeeCoverage, borrowFeeHealth = c.borrowFeeCoverage(ctx, symbols, connector, scopeProvider, now, borrowFees, borrowFeeHealth)
	borrowFeeHealth.Applicability = borrowApplicability
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

func (c *marketEventCache) loadBorrowFees(ctx context.Context) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	if c.borrowFeesRefreshGate == nil {
		c.borrowFeesRefreshGate = make(chan struct{}, 1)
	}
	gate := c.borrowFeesRefreshGate
	c.mu.Unlock()
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return c.canceledBorrowFeeRefresh(ctx.Err(), c.now().UTC())
	}
	defer func() { <-gate }()
	now := c.now().UTC()
	if err := ctx.Err(); err != nil {
		return c.canceledBorrowFeeRefresh(err, now)
	}

	c.mu.Lock()
	cached := cloneBorrowFeeEntry(c.borrowFees)
	lastAttempt := cloneBorrowFeeAttempt(c.borrowFeesLastAttempt)
	if due, nextOpen := borrowFeeSourceDue(now); !due {
		c.mu.Unlock()
		return borrowFeesNotDue(cached, lastAttempt, now, nextOpen)
	}
	if borrowFeeEntryFresh(cached, now) {
		c.mu.Unlock()
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusOK, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium", borrowFeeEntryNotes(cached, "IBKR short-stock availability fee rate"))
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
	entry, err := fetchIBKRBorrowFees(ctx, cached.SourceURL)
	if ctx.Err() != nil {
		return c.canceledBorrowFeeRefresh(ctx.Err(), c.now().UTC())
	}
	completedAt := c.now().UTC()
	if completedAt.Before(attemptedAt) {
		completedAt = attemptedAt
	}
	now = completedAt
	if err != nil {
		failure := borrowFeeFailureFromError(err, completedAt)
		next := completedAt.Add(marketEventsBorrowFeeRetryAfter).UTC()
		attempt := marketEventBorrowFeeAttempt{
			Outcome: marketEventBorrowFeeOutcomeFailure, AttemptedAt: attemptedAt,
			CompletedAt: completedAt, NextAttempt: &next, Failure: &failure,
		}
		c.logBorrowFeeFailure(lastAttempt, failure, err, next)
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
	c.logBorrowFeeRecovery(lastAttempt, entry)
	status := rpc.SourceStatusOK
	confidence := "medium"
	if !borrowFeeEntryFresh(entry, now) {
		status = rpc.SourceStatusStale
		confidence = "medium-low"
	}
	health := marketEventSourceHealth("borrow_fee", status, entry.AsOf, now, marketEventsBorrowFeeMaxAge, confidence, borrowFeeEntryNotes(entry, "IBKR short-stock availability fee rate"))
	health.RefreshState = rpc.SourceRefreshCurrent
	return entry, health, nil
}

func (c *marketEventCache) canceledBorrowFeeRefresh(err error, now time.Time) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	c.mu.Lock()
	cached, attempt := cloneBorrowFeeEntry(c.borrowFees), cloneBorrowFeeAttempt(c.borrowFeesLastAttempt)
	c.mu.Unlock()
	status := rpc.SourceStatusUnknown
	if len(cached.Symbols) > 0 {
		status = rpc.SourceStatusStale
	}
	if borrowFeeEntryFresh(cached, now) {
		status = rpc.SourceStatusOK
	}
	health := marketEventSourceHealth("borrow_fee", status, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "low", borrowFeeEntryNotes(cached, "refresh canceled; prior source evidence is retained"))
	applyBorrowFeeLastFailure(&health, attempt)
	if attempt != nil && attempt.Outcome == marketEventBorrowFeeOutcomeFailure {
		health.NextAttempt = cloneBorrowFeeTimePtr(attempt.NextAttempt)
		health.RefreshState = rpc.SourceRefreshFetchFailed
		if health.NextAttempt != nil && now.Before(*health.NextAttempt) {
			health.RefreshState = rpc.SourceRefreshFetchFailedBackoff
		}
	}
	return cached, health, err
}

// borrowFeeEntryNotes discloses skipped provider rows beside a served entry.
func borrowFeeEntryNotes(entry marketEventBorrowFeeEntry, note string) []string {
	notes := []string{note}
	if entry.SkippedRows > 0 {
		notes = append(notes, fmt.Sprintf("%d malformed IBKR short-stock rows were skipped; their symbols are unobserved", entry.SkippedRows))
	}
	return notes
}

// logBorrowFeeFailure warns once per change of failure class (code and stage),
// so a persistent outage is visible without logging every retry.
func (c *marketEventCache) logBorrowFeeFailure(previous *marketEventBorrowFeeAttempt, failure rpc.SourceFailure, err error, next time.Time) {
	if c.logger == nil {
		return
	}
	if previous != nil && previous.Outcome == marketEventBorrowFeeOutcomeFailure && previous.Failure != nil &&
		previous.Failure.Code == failure.Code && previous.Failure.Stage == failure.Stage {
		return
	}
	endpoint := "unknown endpoint"
	if sourceErr, ok := errors.AsType[*borrowFeeFetchError](err); ok && sourceErr.endpoint != "" {
		endpoint = sourceErr.endpoint
	}
	c.logger.Warnf("borrow fees: IBKR short-stock refresh failed at %s (%s) on %s; retrying after %s",
		failure.Stage, failure.Code, endpoint, next.Format(time.RFC3339))
}

// logBorrowFeeRecovery reports the first success after a failed attempt.
func (c *marketEventCache) logBorrowFeeRecovery(previous *marketEventBorrowFeeAttempt, entry marketEventBorrowFeeEntry) {
	if c.logger == nil || previous == nil || previous.Outcome != marketEventBorrowFeeOutcomeFailure {
		return
	}
	c.logger.Infof("borrow fees: IBKR short-stock refresh recovered from %s: %d symbols as of %s",
		entry.SourceURL, len(entry.Symbols), entry.AsOf.Format(time.RFC3339))
}

func borrowFeeEntryFresh(entry marketEventBorrowFeeEntry, now time.Time) bool {
	if entry.AsOf.IsZero() || entry.AsOf.After(now) {
		return false
	}
	return now.Sub(entry.AsOf) <= marketEventsBorrowFeeFreshFor
}

// borrowFeeSourceDue reports whether the US regular session is open at now and,
// when it is not, the calendar's next regular open. An unverified calendar
// date is treated as due.
func borrowFeeSourceDue(now time.Time) (bool, *time.Time) {
	session, err := marketcal.NewWithClock(func() time.Time { return now }).SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || session.State == marketcal.StateUnknown {
		return true, nil
	}
	return session.IsOpen, session.NextOpen
}

func borrowFeesNotDue(cached marketEventBorrowFeeEntry, lastAttempt *marketEventBorrowFeeAttempt, now time.Time, nextOpen *time.Time) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	// The next attempt is the next regular open, unless a retained failure
	// backoff reaches past it.
	var nextAttempt *time.Time
	if nextOpen != nil {
		nextAttempt = new(nextOpen.UTC())
	}
	if lastAttempt != nil && lastAttempt.NextAttempt != nil && (nextAttempt == nil || lastAttempt.NextAttempt.After(*nextAttempt)) {
		nextAttempt = new(lastAttempt.NextAttempt.UTC())
	}
	if len(cached.Symbols) == 0 {
		// No source clock: nothing was ever delivered.
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusUnknown, time.Time{}, now, marketEventsBorrowFeeMaxAge, "low", []string{"IBKR borrow-fee source is outside its official US-equity refresh window"})
		health.RefreshState = rpc.SourceRefreshNotDue
		health.NextAttempt = nextAttempt
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
	health := marketEventSourceHealth("borrow_fee", status, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium-low", borrowFeeEntryNotes(cached, "serving last-good IBKR borrow-fee data; no regular-session refresh is due"))
	health.RefreshState = rpc.SourceRefreshNotDue
	health.NextAttempt = nextAttempt
	applyBorrowFeeLastFailure(&health, lastAttempt)
	return cached, health, nil
}

// borrowFeesFallback mirrors regSHOFallback for the IBKR short-stock
func borrowFeesFallback(cached marketEventBorrowFeeEntry, now time.Time, failure *rpc.SourceFailure) (marketEventBorrowFeeEntry, rpc.SourceHealth, error) {
	cause := borrowFeeFailureError(failure)
	if len(cached.Symbols) > 0 {
		health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusStale, cached.AsOf, now, marketEventsBorrowFeeMaxAge, "medium-low", borrowFeeEntryNotes(cached, "using stale cached IBKR short-stock availability; latest refresh "+cause.Error()))
		health.LastFailure = cloneBorrowFeeSourceFailure(failure)
		return cached, health, nil
	}
	health := marketEventSourceHealth("borrow_fee", rpc.SourceStatusUnknown, time.Time{}, now, marketEventsBorrowFeeMaxAge, "low", []string{"IBKR borrow-fee data is unavailable; latest refresh " + cause.Error()})
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
	publichttp.SetUserAgent(req)
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

func fetchIBKRBorrowFeesFTP(ctx context.Context, lastSourceURL string) (marketEventBorrowFeeEntry, error) {
	return ibkrBorrowFeeFTP.fetch(ctx, lastSourceURL)
}

// fetch tries each endpoint once, starting with the one that served
// lastSourceURL, and records the serving endpoint as the entry's source URL.
// When every endpoint fails it returns the failure that progressed furthest,
// so an always-dark endpoint cannot mask how a reachable one failed.
func (s borrowFeeFTPSource) fetch(ctx context.Context, lastSourceURL string) (marketEventBorrowFeeEntry, error) {
	var failure error
	for _, host := range s.endpointOrder(lastSourceURL) {
		entry, err := s.fetchEndpoint(ctx, host)
		if err == nil {
			return entry, nil
		}
		if ctx.Err() != nil {
			return marketEventBorrowFeeEntry{}, ctx.Err()
		}
		if failure == nil || borrowFeeFailureProgress(err) >= borrowFeeFailureProgress(failure) {
			failure = err
		}
	}
	if failure == nil {
		failure = newBorrowFeeFetchError(rpc.SourceFailureTransportFailed, rpc.SourceFailureStageFTPControlConnect, true)
	}
	return marketEventBorrowFeeEntry{}, failure
}

func (s borrowFeeFTPSource) endpointOrder(lastSourceURL string) []string {
	order := slices.Clone(s.endpoints)
	u, err := url.Parse(lastSourceURL)
	if err != nil {
		return order
	}
	if i := slices.Index(order, u.Hostname()); i > 0 {
		preferred := order[i]
		order = slices.Insert(slices.Delete(order, i, i+1), 0, preferred)
	}
	return order
}

// fetchEndpoint reconnects once when the greeting or login times out, which
// IBKR's mirror does intermittently, before the caller fails over.
func (s borrowFeeFTPSource) fetchEndpoint(ctx context.Context, host string) (marketEventBorrowFeeEntry, error) {
	addr := net.JoinHostPort(host, "21")
	body, err := s.retrieve(ctx, addr)
	if borrowFeeFTPReconnects(err) && ctx.Err() == nil {
		body, err = s.retrieve(ctx, addr)
	}
	var entry marketEventBorrowFeeEntry
	if err == nil {
		entry, err = parseIBKRBorrowFeeDownload(body, "ftp://"+host+"/"+s.path)
	}
	if sourceErr, ok := errors.AsType[*borrowFeeFetchError](err); ok {
		sourceErr.endpoint = host
	}
	return entry, err
}

func borrowFeeFTPReconnects(err error) bool {
	sourceErr, ok := errors.AsType[*borrowFeeFetchError](err)
	return ok && sourceErr.code == rpc.SourceFailureTimeout &&
		(sourceErr.stage == rpc.SourceFailureStageFTPGreeting || sourceErr.stage == rpc.SourceFailureStageFTPAuthenticate)
}

// borrowFeeFailureProgress ranks a fetch failure by how far the session got.
func borrowFeeFailureProgress(err error) int {
	sourceErr, ok := errors.AsType[*borrowFeeFetchError](err)
	if !ok {
		return -1
	}
	return slices.Index([]string{
		rpc.SourceFailureStageFTPControlConnect, rpc.SourceFailureStageFTPGreeting,
		rpc.SourceFailureStageFTPAuthenticate, rpc.SourceFailureStageFTPPassiveNegotiate,
		rpc.SourceFailureStageFTPPassiveConnect, rpc.SourceFailureStageFTPRetrieve,
		rpc.SourceFailureStageBorrowParse,
	}, sourceErr.stage)
}

func parseIBKRBorrowFeeDownload(body, endpoint string) (marketEventBorrowFeeEntry, error) {
	entry, err := parseIBKRBorrowFees(body)
	if err != nil {
		return marketEventBorrowFeeEntry{}, err
	}
	entry.SourceURL = strings.TrimSpace(endpoint)
	if entry.SourceURL == "" {
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
	}
	return entry, nil
}

// parseIBKRBorrowFees parses IBKR's pipe-delimited short-stock file. The format
// has no quoting, so a quote inside a security name is literal text. The
// #BOF/#SYM envelope is strict and a published #EOF row count must equal the
// data lines; a malformed data row is skipped and counted instead of rejecting
// the file.
func parseIBKRBorrowFees(body string) (marketEventBorrowFeeEntry, error) {
	invalid := newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageBorrowParse, true)
	entry := marketEventBorrowFeeEntry{Symbols: map[string]marketEventBorrowFeeRecord{}}
	seenBOF, seenHeader, seenEOF := false, false, false
	dataRows := 0
	for line := range strings.Lines(body) {
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if seenEOF {
			return marketEventBorrowFeeEntry{}, invalid
		}
		fields := strings.Split(line, "|")
		switch tag := strings.TrimSpace(fields[0]); {
		case tag == "#BOF":
			if seenBOF || len(fields) < 3 {
				return marketEventBorrowFeeEntry{}, invalid
			}
			entry.AsOf = parseIBKRBorrowFeeAsOf(fields[1], fields[2])
			if entry.AsOf.IsZero() {
				return marketEventBorrowFeeEntry{}, invalid
			}
			seenBOF = true
			continue
		case tag == "#SYM":
			if seenHeader || !validIBKRBorrowFeeHeader(fields) {
				return marketEventBorrowFeeEntry{}, invalid
			}
			seenHeader = true
			continue
		case tag == "#EOF":
			if len(fields) > 1 && strings.TrimSpace(fields[1]) != "" {
				count, err := strconv.Atoi(strings.TrimSpace(fields[1]))
				if err != nil || count != dataRows {
					return marketEventBorrowFeeEntry{}, invalid
				}
			}
			seenEOF = true
			continue
		case strings.HasPrefix(tag, "#"):
			continue
		}
		if !seenBOF || !seenHeader {
			return marketEventBorrowFeeEntry{}, invalid
		}
		dataRows++
		record, ok := parseIBKRBorrowFeeRow(fields)
		if !ok {
			entry.SkippedRows++
			continue
		}
		entry.Symbols[record.Symbol] = record
	}
	if !seenBOF || !seenHeader || entry.AsOf.IsZero() || len(entry.Symbols) == 0 {
		return marketEventBorrowFeeEntry{}, invalid
	}
	return entry, nil
}

func parseIBKRBorrowFeeRow(fields []string) (marketEventBorrowFeeRecord, bool) {
	if len(fields) < 8 {
		return marketEventBorrowFeeRecord{}, false
	}
	record := marketEventBorrowFeeRecord{
		Symbol:   normSym(fields[0]),
		Currency: strings.TrimSpace(fields[1]),
		Name:     strings.TrimSpace(fields[2]),
		ConID:    strings.TrimSpace(fields[3]),
		ISIN:     strings.TrimSpace(fields[4]),
	}
	var rebateOK, feeOK, availableOK bool
	record.RebateRate, record.RebateRateUnpublished, rebateOK = parseIBKRBorrowFeeRate(fields[5])
	record.FeeRate, record.FeeRateUnpublished, feeOK = parseIBKRBorrowFeeRate(fields[6])
	record.Available, record.AvailableLowerBound, availableOK = parseIBKRBorrowAvailable(fields[7])
	return record, record.Symbol != "" && rebateOK && feeOK && availableOK
}

// parseIBKRBorrowFeeRate reads a percentage rate; "NA" is a published absence.
func parseIBKRBorrowFeeRate(raw string) (rate float64, unpublished, ok bool) {
	if strings.EqualFold(strings.TrimSpace(raw), "NA") {
		return 0, true, true
	}
	rate, ok = parseFloatField(raw)
	return rate, false, ok
}

// parseIBKRBorrowAvailable reads a share count; ">N" publishes only a lower bound.
func parseIBKRBorrowAvailable(raw string) (available int64, lowerBound, ok bool) {
	raw, lowerBound = strings.CutPrefix(strings.TrimSpace(raw), ">")
	available, ok = parseIntField(raw)
	return available, lowerBound, ok && available >= 0
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
	// endpoint names the failing host for the operator log only; the typed
	// failure never carries it.
	endpoint string
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

// retrieve downloads s.path over one FTP session at addr. Every control
// command and reply has its own deadline and the passive transfer has a
// separate budget; ctx bounds both, and cancellation closes the sockets.
func (s borrowFeeFTPSource) retrieve(ctx context.Context, addr string) (string, error) {
	control, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPControlConnect, err)
	}
	defer control.Close()
	stopControl := context.AfterFunc(ctx, func() { _ = control.Close() })
	defer stopControl()
	reader := bufio.NewReader(control)
	// exchange sends cmd, if any, and reads its reply within one control deadline.
	exchange := func(stage, cmd string) (int, string, error) {
		_ = control.SetDeadline(ftpDeadline(ctx, s.controlTimeout))
		if cmd != "" {
			if err := writeFTPCommand(control, cmd); err != nil {
				return 0, "", borrowFeeTransportFetchError(stage, err)
			}
		}
		code, line, err := readFTPResponse(reader)
		if err != nil {
			return 0, "", borrowFeeFTPResponseFetchError(stage, err)
		}
		return code, line, nil
	}
	rejected := func(stage string) error {
		return newBorrowFeeFetchError(rpc.SourceFailureProtocolRejected, stage, true)
	}

	if code, _, err := exchange(rpc.SourceFailureStageFTPGreeting, ""); err != nil {
		return "", err
	} else if code != 220 {
		return "", rejected(rpc.SourceFailureStageFTPGreeting)
	}
	code, _, err := exchange(rpc.SourceFailureStageFTPAuthenticate, "USER "+s.user)
	if err != nil {
		return "", err
	}
	if code == 331 {
		if code, _, err = exchange(rpc.SourceFailureStageFTPAuthenticate, "PASS "+s.pass); err != nil {
			return "", err
		}
	}
	if code != 230 {
		return "", newBorrowFeeFetchError(rpc.SourceFailureAuthenticationRejected, rpc.SourceFailureStageFTPAuthenticate, true)
	}
	if code, _, err := exchange(rpc.SourceFailureStageFTPPassiveNegotiate, "TYPE I"); err != nil {
		return "", err
	} else if code != 200 {
		return "", rejected(rpc.SourceFailureStageFTPPassiveNegotiate)
	}
	code, line, err := exchange(rpc.SourceFailureStageFTPPassiveNegotiate, "PASV")
	if err != nil {
		return "", err
	}
	if code != 227 {
		return "", rejected(rpc.SourceFailureStageFTPPassiveNegotiate)
	}
	dataAddr, err := ftpPassiveAddr(line)
	if err != nil {
		return "", rejected(rpc.SourceFailureStageFTPPassiveNegotiate)
	}
	data, err := s.dial(ctx, "tcp", dataAddr)
	if err != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPPassiveConnect, err)
	}
	defer data.Close()
	stopData := context.AfterFunc(ctx, func() { _ = data.Close() })
	defer stopData()
	if code, _, err := exchange(rpc.SourceFailureStageFTPRetrieve, "RETR "+s.path); err != nil {
		return "", err
	} else if code != 125 && code != 150 {
		return "", rejected(rpc.SourceFailureStageFTPRetrieve)
	}
	_ = data.SetDeadline(ftpDeadline(ctx, s.transferTimeout))
	const maxBorrowFeeBytes = 16 << 20
	body, readErr := io.ReadAll(io.LimitReader(data, maxBorrowFeeBytes+1))
	closeErr := data.Close()
	if readErr != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPRetrieve, readErr)
	}
	if len(body) > maxBorrowFeeBytes {
		return "", newBorrowFeeFetchError(rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageFTPRetrieve, true)
	}
	if closeErr != nil {
		return "", borrowFeeTransportFetchError(rpc.SourceFailureStageFTPRetrieve, closeErr)
	}
	if code, _, err := exchange(rpc.SourceFailureStageFTPRetrieve, ""); err != nil {
		return "", err
	} else if code != 226 {
		return "", rejected(rpc.SourceFailureStageFTPRetrieve)
	}
	_ = writeFTPCommand(control, "QUIT")
	return string(body), nil
}

// ftpDeadline is budget from now, capped by ctx's deadline.
func ftpDeadline(ctx context.Context, budget time.Duration) time.Time {
	deadline := time.Now().Add(budget)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		return dl
	}
	return deadline
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
	publichttp.SetUserAgent(req)
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

func (c *marketEventCache) borrowInventory(ctx context.Context, symbols, unquoted []string, subs *subManager, connector *ibkrlib.Connector, now time.Time, res *rpc.MarketEventsResult) rpc.SourceHealth {
	open := c.openInventoryTransport
	if open == nil {
		open = func() (borrowInventoryTransport, bool) { return c.connectorInventoryTransport(subs, connector) }
	}
	transport, ok := open()
	if !ok {
		return marketEventSourceHealth("borrow_inventory", rpc.SourceStatusUnknown, time.Time{}, now, marketEventsInventoryMaxAge, "low", []string{"IBKR gateway is unavailable; shortable-share inventory is unknown"})
	}
	return c.readBorrowInventory(ctx, symbols, unquoted, transport.binding, res, transport.current, transport.peek, transport.probe)
}

// borrowInventoryTransport is the broker session a shortable-share read is
// fenced to and the tick-236 reads it borrows; see readBorrowInventory.
type borrowInventoryTransport struct {
	binding ibkrlib.ConnectorSessionBinding
	current func() bool
	peek    func(string) *ibkrlib.MarketData
	probe   func(context.Context, string) (*ibkrlib.MarketData, error)
}

// connectorInventoryTransport binds the shortable-share read to connector's
// ready session; false means no usable session.
func (c *marketEventCache) connectorInventoryTransport(subs *subManager, connector *ibkrlib.Connector) (borrowInventoryTransport, bool) {
	binding, ready := connector.CaptureSession()
	if !ready || subs == nil || connector.BackendLink().Down {
		return borrowInventoryTransport{}, false
	}
	peek := func(sym string) *ibkrlib.MarketData { return connector.MarketDataSnapshot()[sym] }
	return borrowInventoryTransport{
		binding: binding,
		current: func() bool { return connector.SessionCurrent(binding) && !connector.BackendLink().Down },
		peek:    peek,
		probe: func(probeCtx context.Context, sym string) (*ibkrlib.MarketData, error) {
			release, err := subs.Hold(probeCtx, sym)
			if err != nil {
				return nil, err
			}
			defer release()
			err = pollMarketData(probeCtx, connector, sym, time.Now().Add(marketEventsBorrowPollBudget), func(md *ibkrlib.MarketData) bool {
				return marketEventInventoryReceiptCurrent(md, c.now().UTC())
			})
			return peek(sym), err
		},
	}, true
}

func marketEventBorrowInventoryFlag(sym string, md ibkrlib.MarketData, now time.Time) (rpc.MarketEventFlag, bool) {
	if !marketEventInventoryReceiptCurrent(&md, now) || md.ShortableShares > marketEventsBorrowTightShares {
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
	if rec.FeeRateUnpublished || rec.FeeRate < marketEventsBorrowFeeExtremePct {
		return rpc.MarketEventFlag{}, false
	}
	value := rec.FeeRate
	rebate := fmt.Sprintf("rebate_rate=%.4f%%", rec.RebateRate)
	if rec.RebateRateUnpublished {
		rebate = "rebate_rate=unpublished"
	}
	available := "available=" + strconv.FormatInt(rec.Available, 10)
	if rec.AvailableLowerBound {
		available = "available>=" + strconv.FormatInt(rec.Available, 10)
	}
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
			rebate,
			available,
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
