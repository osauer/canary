package ibkr

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/osauer/canary/v2/pkg/ibkr/internal/logging"
)

var connectorLogger = logging.Component("IBKR Connector")
var marketDataLogger = logging.Component("IBKR MarketData")

// OptionSubscriptionGenericTicks is the generic-tick list used by
// SubscribeOption for per-contract option market data.
const OptionSubscriptionGenericTicks = "100,101,104,106"

// OptionOpenInterestGenericTick is IBKR's open-interest generic tick for
// option market-data subscriptions.
const OptionOpenInterestGenericTick = "101"

// ErrSymbolInactive indicates IBKR has reported the contract is unavailable (e.g., delisted).
var ErrSymbolInactive = errors.New("symbol marked inactive")

// ErrContractDetailsTimeout indicates that a contract-details request did not
// receive its end marker before the caller's timeout. A returned result slice
// may still contain details received before the timeout.
var ErrContractDetailsTimeout = errors.New("timeout waiting for contract details")

// ErrContractNoDefinition reports that IBKR answered a contract-details
// request with a definition rejection rather than falling silent. It is the
// broker's own verdict and must stay distinguishable from
// [ErrContractDetailsTimeout]: a timeout is "no answer yet", this is "there is
// no such contract". It alone is not proof a contract is dead — a wedged
// gateway answers this for everything — so it feeds the guarded inactive
// candidate rather than marking directly.
var ErrContractNoDefinition = errors.New("no security definition for contract")

// ErrBrokerIDNamespaceConflict reports that an explicit broker order/WhatIf
// ID is still owned by an open read-only request. The broker-adjacent
// operation is refused before local indexing or wire send and may be retried
// with a newly reserved ID.
var ErrBrokerIDNamespaceConflict = errors.New("broker id namespace conflict")

// Connector owns one broker connection together with its subscriptions and
// in-memory market, contract, account, and order caches. Construct a Connector
// with [NewConnector], call [Connector.Start] to begin its lifecycle, and call
// [Connector.Stop] when finished.
type Connector struct {
	name   string
	config *ConnectorConfig
	conn   *Connection

	fetchContractDetails    func(string, time.Duration) ([]ContractDetailsLite, error)
	contractTimingHook      func(string, time.Duration, bool)
	resolveWSHContract      func(context.Context, string, time.Duration) (*ContractDetailsLite, error)
	resolveWSHExactContract func(context.Context, string, int, time.Duration) (*ContractDetailsLite, error)
	wshNow                  func() time.Time
	wshGateMu               sync.Mutex
	wshGate                 chan struct{}
	wshMetadataConn         *Connection
	wshMetadataReadyAt      time.Time
	wshEarningsEventTag     string

	// Component state
	running   bool
	lastError error
	mu        sync.RWMutex
	ready     bool // true after handlers registered and startup completes

	// Market data subscriptions
	subscriptions map[string]*Subscription
	reqIDMap      map[int]string // Maps request IDs to symbols or routed subscription keys
	subMu         sync.RWMutex
	exactQuoteSeq atomic.Uint64

	// Order management
	openOrders       map[string]*trackedOrder
	brokerOrderIndex map[string]string // IB order ID -> internal order ID
	// orderIDHighWater is a bounded monotonic reservation frontier. Request
	// allocation jumps past it, avoiding an ever-growing set of consumed IDs.
	orderIDHighWater int
	orderMu          sync.RWMutex
	orderLifecycleMu sync.RWMutex
	orderLifecycle   []orderLifecycleHandlerEntry
	// handlerRegistration guards the one-time base-handler installation on a
	// concrete Connection. Installation happens before Connect starts the wire
	// reader, so lifecycle frames never need a lossy/reordering startup queue.
	handlerRegistrationMu sync.Mutex
	handlerRegistrations  map[*Connection]struct{}
	// orderLifecycleGeneration advances for every non-WhatIf order lifecycle
	// callback accepted by this Connector. Cached all-client inventory receipts
	// bind to this frontier so any later openOrder/orderStatus/exec/error event
	// invalidates the receipt before it can authorize a negative.
	orderLifecycleGeneration atomic.Uint64
	// evidenceBarrier linearizes socket/order callbacks and structural
	// portfolio publications against daemon commits that may clear an alert.
	// The owned Connection shares this exact lock.
	evidenceBarrier sync.RWMutex
	// publicationBarrier linearizes daemon connector publication against the
	// final transport section of a protected broker operation. It is separate
	// from evidenceBarrier and its read side is acquired only after pacing and
	// transport admission, so a paused sender cannot deadlock unpublication.
	publicationBarrier sync.RWMutex
	// brokerIDNamespaceMu makes Connector-level FEE request registration and
	// order/WhatIf reservation atomic around Connection's shared monotonic ID
	// frontier. Connection.reqIDMu remains the authority for allocation.
	brokerIDNamespaceMu sync.Mutex
	// whatIfBeforeBrokerIDClaim is a deterministic rollover seam. Production
	// leaves it nil; tests pause after the exact-session check and before the
	// allocator/claim boundary.
	whatIfBeforeBrokerIDClaim func()

	// Open-order snapshot plumbing is an epoch-bound single-flight because
	// reqAllOpenOrders has no request ID. A timed-out wire flight remains
	// authoritative until its same-socket terminator arrives; otherwise the
	// socket generation is poisoned rather than admitting ambiguous evidence.
	requestAllOpenOrders        func() error // test seam; nil uses the bound Connection
	openOrderSnapshotMu         sync.Mutex
	openOrderSnapshot           *openOrderSnapshotFlight
	openOrderSnapshotPoison     openOrderSnapshotBinding
	openOrderSnapshotTimeout    time.Duration
	openOrderSnapshotBeforeSend func()

	// orderStatusLogSig dedupes the high-frequency order-status log line.
	// IBKR re-sends orderStatus frames for unchanged working orders many
	// times per second (a resting PreSubmitted GTC trail can churn for
	// hours), and logging each at INFO grew ibkr-daemon.log into the
	// gigabytes. We remember the last-logged (status|filled|remaining)
	// signature per broker order id and demote verbatim repeats to Debug so
	// INFO carries only genuine transitions. Purely a logging concern —
	// order state tracking above is unaffected. Guarded by its own mutex so
	// the log path never widens the orderMu critical section.
	orderStatusLogMu  sync.Mutex
	orderStatusLogSig map[string]string

	// Lightweight contract details cache to improve routing during OOH sessions
	contractMu         sync.RWMutex
	contractCache      map[string]ContractDetailsLite
	inactiveMu         sync.RWMutex
	inactiveSymbols    map[string]inactiveSymbolState
	inactiveCandidates map[string]inactiveCandidateState

	// mktDataAbsent remembers subscription keys whose market-data request
	// the gateway terminally rejected as not-entitled (error 354), so the
	// next marketDataAbsenceRetry window skips the futile resubscribe
	// instead of re-burning a request + poll budget per caller cycle.
	// In-memory only — broker entitlements must never persist (see
	// docs/docs/internals/architecture.md) — and the daemon rebuilds the Connector on
	// every reconnect, which is the "cleared on reconnect" path.
	// absenceNow is a test seam, nil meaning time.Now (same pattern as
	// contractTimingHook).
	absenceMu     sync.Mutex
	mktDataAbsent map[string]marketDataAbsence
	absenceNow    func() time.Time

	// acctUpdatesMu guards the account-updates resubscribe throttle (see
	// maybeResubscribeAccountUpdates) and the account the stream is currently
	// subscribed with. acctUpdatesNow is a test seam, nil meaning time.Now
	// (same pattern as absenceNow).
	acctUpdatesMu      sync.Mutex
	acctUpdatesLastAt  time.Time
	acctUpdatesAccount string
	acctUpdatesNow     func() time.Time

	// pnlResubMu guards the daily-P&L resubscribe throttle (see
	// MaybeResubscribeStaleDailyPnL). pnlResubNow is a test seam, nil
	// meaning time.Now (same pattern as acctUpdatesNow).
	pnlResubMu     sync.Mutex
	pnlResubLastAt time.Time
	pnlResubNow    func() time.Time

	// backendConnMu guards the TWS<->IBKR backend-link state driven by
	// system notices 1100 (link lost) and 1101/1102 (link restored).
	// Distinct from the local API socket: the socket stays connected while
	// the gateway's own upstream is down, and an order transmitted in that
	// window is accepted locally but cannot reach the broker.
	backendConnMu   sync.Mutex
	backendConnDown bool
	backendConnAt   time.Time
	// mdReplayInFlight collapses concurrent 1101 recoveries so each
	// data-loss event replays every live shared subscription at most once.
	mdReplayInFlight atomic.Bool

	// Option IV tracking (by underlying symbol or per-contract key)
	optMu           sync.RWMutex
	optIV           map[string]float64 // last observed implied vol (fraction, e.g., 0.30)
	optReqIDs       map[int]string     // option reqID -> underlying or option market-data key
	optQuoteBid     map[string]float64 // last observed option bid per underlying
	optQuoteAsk     map[string]float64 // last observed option ask per underlying
	optPrevClose    map[string]float64 // tick 9 on the option contract itself (NOT the underlying)
	optGreeks       map[string]Greeks  // last observed model-computation greeks per option key
	optUnderlyingPx map[string]float64 // model-computation underlying price per option key

	// In-flight reqContractDetails requests, keyed by reqID. Without this the
	// notice path cannot see them at all: recoverFromSystemNotice searches
	// historicalReqs and subscriptionKeyForNotice searches reqIDMap, and a
	// contract-details reqID is in neither. A code-200 answering one then
	// failed to fail the pending wait (the caller burned its whole budget and
	// reported a timeout, disguising the broker's definitive answer as a
	// transient) and never reached inactive marking.
	contractDetailsMu   sync.Mutex
	contractDetailsReqs map[int]*contractDetailsRequest

	// Historical data requests (HMDS)
	historicalMu           sync.Mutex
	historicalReqs         map[int]*historicalRequest
	historicalBackoff      map[string]int
	historicalExactFlights map[string]*historicalExactFlight
	historicalRouteReqs    map[int]chan error
	historicalNow          func() time.Time

	// dataFarms records the latest IBKR data-farm notice per farm. The
	// status endpoint surfaces only unhealthy entries, but keeping the
	// healthy notices here lets a later "OK" clear an earlier break.
	// farmRecoveryAt stamps the most recent impaired->ok transition; see
	// farmRecoverySettleWindow.
	dataFarmMu     sync.RWMutex
	dataFarms      map[string]DataFarmStatus
	farmRecoveryAt time.Time

	// pnl holds account-level and per-conId Daily P&L subscription state.
	// The cache is on the Connector (not the Connection) so a Connection
	// rebuild (reconnect) restarts the subscription cleanly — the daemon's
	// post-connect setup re-issues reqPnL and per-position calls re-issue
	// reqPnLSingle as needed. Never nil after NewConnector.
	pnl *pnlCache
}

// DataFarmStatus describes the latest notice observed for one IBKR data farm.
// Type identifies the farm category, Status is one of "ok", "inactive",
// "disconnected", or "broken", and AsOf is the local observation time.
type DataFarmStatus struct {
	Name    string
	Type    string
	Status  string
	Code    int
	Message string
	AsOf    time.Time
}

// DataFarmStatuses returns a detached snapshot of the latest tracked farm
// notices, sorted by type and then name. It returns nil for a nil Connector;
// callers determine freshness from each entry's [DataFarmStatus.AsOf].
func (c *Connector) DataFarmStatuses() []DataFarmStatus {
	if c == nil {
		return nil
	}
	c.dataFarmMu.RLock()
	out := make([]DataFarmStatus, 0, len(c.dataFarms))
	for _, farm := range c.dataFarms {
		out = append(out, farm)
	}
	c.dataFarmMu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ConnectorConfig configures the single [Connection] owned by a [Connector].
// A nil BaseConfig uses [DefaultConfig]. PreferredClientID falls back first to
// BaseConfig.ClientID and then to 1. [NewConnector] copies both config values.
type ConnectorConfig struct {
	// Deprecated: ServiceName has no effect; nothing reads it since the
	// connection-pool removal. It will be removed in the next major version.
	ServiceName       string
	PreferredClientID int
	BaseConfig        *ConnectionConfig
}

// Subscription holds the latest values observed for one streaming market-data
// request. Zero-valued fields may mean either an observed zero or data not yet
// received; fields with an accompanying Observed flag distinguish those cases.
// LastTime is the subscription's liveness clock and LastTickAt is the tick
// observation instant; the two differ and are documented on the fields.
type Subscription struct {
	Symbol string
	// SessionEpoch is set for exact-session subscriptions. Zero identifies a
	// legacy/shared subscription that cannot satisfy broker-write preview
	// evidence.
	SessionEpoch uint64
	// Right is the normalized option right ("C" or "P") for option-leg
	// subscriptions. It is empty for non-option subscriptions.
	Right     string
	ReqID     int
	Fields    []string
	LastPrice float64
	Bid       float64
	Ask       float64
	// MarkPrice is tick 37 — the gateway's calculated "fair" price for
	// the symbol. For tradeable instruments (ETFs, equities) it is
	// usually redundant with last/(bid+ask)/2; for indices like VIX,
	// VIX3M, and SPX, IBKR delivers tick 37 as the ONLY price (indices
	// don't trade, so they have no bid/ask/last). Consumers use this
	// as a final fallback so an index symbol still yields a usable
	// scalar when bid/ask/last all stay zero.
	MarkPrice float64
	BidSize   int64
	AskSize   int64
	Volume    int64
	AvgVolume int64
	// OpenInt is the option open interest at this contract: tick 27
	// (callOpenInterest) for CALL legs, tick 28 (putOpenInterest) for
	// PUT legs. The gateway also emits a zero-valued companion tick for
	// the opposite right, so only the tick matching Right is committed.
	// OpenIntObserved distinguishes "gateway sent zero OI for this right"
	// from "gateway has not delivered the matching OI tick yet".
	OpenInt         int64
	OpenIntObserved bool
	// ShortableShares is wire tick 89 (a tickSize), delivered for the
	// generic-tick-236 request. ShortableObserved distinguishes "IBKR
	// observed zero shares available" from "this subscription has not
	// delivered borrow inventory".
	ShortableShares   int64
	ShortableObserved bool
	PrevClose         float64
	Open              float64
	High              float64
	Low               float64
	// Week-range highs/lows arrive via generic tick 165 (Misc Stats) as
	// tickPrice messages with tick types 15-20. Captured here so consumers
	// (notably scan-row enrichment, where 52w range is a standard column)
	// can read them without a separate market-data call.
	Week13Low  float64
	Week13High float64
	Week26Low  float64
	Week26High float64
	Week52Low  float64
	Week52High float64
	// LastTradeTime is IBKR tick-string type 45, a Unix timestamp for the
	// last trade/close print. It is distinct from LastTime, which records
	// when this process observed any tick on the subscription.
	LastTradeTime time.Time
	// LastTickAt is when this process last received a tick message from the
	// gateway on this subscription. Unlike LastTime it is never seeded at
	// subscribe time and never advanced by subscription bookkeeping, so a
	// zero value means "no tick has ever arrived" rather than "not observed
	// recently". It is the only field here that can distinguish a live
	// subscription that has gone quiet from one that is ticking.
	//
	// Two limits bind every reader. It records arrival, not the instant the
	// value was struck: under frozen mode the gateway re-sends the last
	// known value on request, so a frozen quote's LastTickAt is essentially
	// read time however old the value is. And it advances on any tick,
	// including size, volume and IV ticks, so a subscription delivering only
	// size ticks looks alive while its price is frozen.
	LastTickAt time.Time
	// IV is the option implied volatility tick (generic tick 106), present
	// only when the streaming subscribe requested it. Stored as a fraction
	// (0.234 == 23.4%); the gateway sometimes emits the percent form, which
	// the handler normalizes.
	IV float64
	// LastTime is the re-request staleness clock read by
	// EnsureMarketDataSubscription. It is seeded at subscribe time and
	// advanced by subscription bookkeeping as well as by ticks, so it is not
	// an observation instant — use LastTickAt for that.
	LastTime time.Time
	Observed bool // true once we receive any tick for this reqID
	// RejectCh receives a [SubscriptionRejection] when the gateway returns
	// a terminal error for this reqID (codes 200, 320, 321, 322, 354,
	// 10197) — "the subscription will never produce ticks" semantics.
	// Buffered 1; the producer drops on a full buffer so it never blocks
	// the error-handler goroutine. A nil channel means fast-abort is
	// disabled (used by test fixtures that bypass the Subscribe path).
	RejectCh chan SubscriptionRejection
	// replaySpec records the wire form of this subscription so the 1101
	// backend-recovery path can re-issue it: IBKR code 1101 means the
	// TWS<->IBKR link was restored with server-side subscriptions LOST,
	// so the old reqID will never tick again. Nil for exact-session
	// subscriptions and disconnected placeholders, which are not replayed.
	replaySpec *mdReplaySpec
	// rejectedReqID records the reqID the gateway reported dead via a
	// terminal entitlement/definition error (200/354): the server tears
	// the ticker down itself, so a wire CancelMarketData for that exact
	// reqID only draws error 300 "Can't find EId". Stored as the reqID
	// (not a bool) so a refresh that re-issues the subscription under a
	// new reqID naturally re-arms the cancel. See wireCancelNeeded.
	rejectedReqID int
}

// wireCancelNeeded reports whether sub still names a gateway-side
// subscription that needs a wire CancelMarketData on teardown. False when
// the gateway already killed this exact reqID via a terminal rejection —
// the rate-limiter slot is released separately (idempotently) in that
// case, never skipped (2026-05-21 slot-leak lesson).
func wireCancelNeeded(sub *Subscription) bool {
	return sub != nil && sub.ReqID != 0 && sub.ReqID != sub.rejectedReqID
}

// SubscriptionRejection records a terminal IBKR error for a market-data
// subscription. Code is the broker error code and Message is untrusted broker
// text. Receiving a value means that request will not produce further ticks.
type SubscriptionRejection struct {
	Code    int
	Message string
}

// terminalSubscriptionErrorCodes is the set of IBKR error codes that
// guarantee the subscription will never produce ticks. The error handler
// pushes a [SubscriptionRejection] on these codes so in-flight pollers
// can abort within milliseconds.
//
//   - 200   "No security definition has been found for the request"
//   - 320   "Server error when reading request" (bad ticker/exchange)
//   - 321   "Server error when validating request"
//   - 322   "Error processing request" (duplicate ticker ID)
//   - 354   "Requested market data is not subscribed" (entitlement gap)
//   - 10197 "Competing live session blocks live data" — handler also
//     forces delayed mode, but the original reqID is dead either way.
func isTerminalSubscriptionError(code int) bool {
	switch code {
	case 200, 320, 321, 322, 354, 10197:
		return true
	}
	return false
}

// marketDataAbsenceRetry bounds how long a terminal entitlement rejection
// (error 354) suppresses re-subscribing the same key. Mirrors the daemon's
// marketEventsShortableAbsentRetry: long enough that steady pollers (the
// app host quotes every held name each ~5 s) stop hammering a dead name,
// short enough that a subscription purchased mid-session starts flowing
// within half an hour. A gateway reconnect re-arms immediately because the
// daemon rebuilds the Connector per connect.
const marketDataAbsenceRetry = 30 * time.Minute

// inactiveMarkTTL bounds an inactive mark the same way marketDataAbsenceRetry
// bounds an entitlement rejection: suppression is a cache, not a verdict.
// Marks are in-memory only — a false mark formed while the gateway answered
// "no security definition" for everything (nightly-reset wedge, observed
// 2026-07-08 marking held AMD/BB/IBM and VIX) heals at min(TTL, reconnect
// rebuild) instead of persisting until a human edits state by hand. A
// genuinely dead name (delisting, ~one per weeks) pays two confirmation
// errors per TTL window per Connector — the same trade the absence memory
// accepted in writing.
const inactiveMarkTTL = 12 * time.Hour

type marketDataAbsence struct {
	code    int
	message string
	at      time.Time
}

// MarketDataAbsenceError reports that a recent terminal entitlement rejection
// is suppressing another request for the same route key. ObservedAt and RetryAt
// are local times; Message is untrusted broker text.
type MarketDataAbsenceError struct {
	Key        string
	Code       int
	Message    string
	ObservedAt time.Time
	RetryAt    time.Time
}

// Error returns a concise description of the suppressed market-data request.
func (e *MarketDataAbsenceError) Error() string {
	return fmt.Sprintf("market data for %s unavailable (IBKR %d at %s; retry after %s)",
		e.Key, e.Code, e.ObservedAt.Format("15:04:05"), e.RetryAt.Format("15:04:05"))
}

func (c *Connector) absenceClock() time.Time {
	if c.absenceNow != nil {
		return c.absenceNow()
	}
	return time.Now()
}

// rememberMarketDataAbsence records a terminal entitlement rejection for a
// subscription key. Logged at Info once per window so the log shows one
// honest probe per key per marketDataAbsenceRetry instead of a churn loop.
func (c *Connector) rememberMarketDataAbsence(key string, code int, message string) {
	now := c.absenceClock()
	c.absenceMu.Lock()
	if c.mktDataAbsent == nil {
		c.mktDataAbsent = make(map[string]marketDataAbsence)
	}
	prev, had := c.mktDataAbsent[key]
	c.mktDataAbsent[key] = marketDataAbsence{code: code, message: message, at: now}
	c.absenceMu.Unlock()
	if !had || now.Sub(prev.at) >= marketDataAbsenceRetry {
		c.logInfo("Market data for %s rejected (code %d); suppressing resubscribes for %s", key, code, marketDataAbsenceRetry)
	}
}

// marketDataAbsenceFor returns the active absence record for key, or nil
// when none is remembered or the retry window has elapsed (expired entries
// are dropped so the map cannot grow unbounded).
func (c *Connector) marketDataAbsenceFor(key string) *MarketDataAbsenceError {
	now := c.absenceClock()
	c.absenceMu.Lock()
	defer c.absenceMu.Unlock()
	entry, ok := c.mktDataAbsent[key]
	if !ok {
		return nil
	}
	if now.Sub(entry.at) >= marketDataAbsenceRetry {
		delete(c.mktDataAbsent, key)
		return nil
	}
	return &MarketDataAbsenceError{
		Key:        key,
		Code:       entry.code,
		Message:    entry.message,
		ObservedAt: entry.at,
		RetryAt:    entry.at.Add(marketDataAbsenceRetry),
	}
}

// farmRecoverySettleWindow keeps the impairment verdict standing after a
// farm reports OK. TWS flushes answers to requests queued during an outage
// as a burst around the status transition, and those answers carry
// outage-era verdicts: on 2026-07-29 the per-farm break notices for the
// 16:24 outage arrived 4s AFTER the queued code=200 burst they explain, and
// the 16:22 flap healed every farm within seconds of breaking it. A window
// after the last impaired->ok transition absorbs both edges; a genuinely
// dead symbol just confirms one settle window later.
const farmRecoverySettleWindow = time.Minute

func farmStatusImpaired(status string) bool {
	return status == "disconnected" || status == "broken"
}

// marketDataFarmImpaired reports whether any market-data (or TWS-server
// connectivity) farm is currently disconnected/broken, or recovered less
// than farmRecoverySettleWindow ago. Absence recording is gated on this: a
// 354 raised while a farm is bouncing says nothing about entitlement, and
// remembering it would blind an entitled name (the SPY/zero-gamma worst
// case) for a full retry window — a farm bounce does not rebuild the
// Connector, so the reconnect-clears-memory path would not save it.
// "inactive" farms stay eligible: that status is the normal off-hours idle
// state, not an outage.
func (c *Connector) marketDataFarmImpaired() bool {
	c.dataFarmMu.RLock()
	defer c.dataFarmMu.RUnlock()
	if !c.farmRecoveryAt.IsZero() && time.Since(c.farmRecoveryAt) < farmRecoverySettleWindow {
		return true
	}
	for _, farm := range c.dataFarms {
		switch farm.Type {
		case "market", "connectivity", "security_definition", "historical":
			if farmStatusImpaired(farm.Status) {
				return true
			}
		}
	}
	return false
}

const (
	contractHydrationWait  = 2 * time.Second
	contractHydrationPoll  = 25 * time.Millisecond
	contractHydrationGrace = 5 * time.Second
)

// HistoricalBar represents one OHLC bar returned by IBKR historical market
// data. Prices and Average use the contract's price units; Volume is the
// broker-reported volume. Time is parsed best-effort and is zero when parsing
// fails, while Date always retains the original broker value.
type HistoricalBar struct {
	Time     time.Time
	Date     string
	Open     float64
	High     float64
	Low      float64
	Close    float64
	Volume   int64
	Average  float64
	BarCount int
}

const minServerVerHistoricalDataEnd = 196

type historicalResult struct {
	start string
	end   string
	bars  []HistoricalBar
	err   error
}

type historicalRequest struct {
	symbol                     string
	result                     chan historicalResult
	strictDaily                bool
	waitForEnd                 bool
	requestOwnsNoticeCollision bool
	connection                 *Connection
	epoch                      uint64
	requireEpoch               bool
	bufferedBars               []HistoricalBar
	bufferedErr                error
}

type historicalExactFlight struct {
	done        chan struct{}
	connection  *Connection
	epoch       uint64
	completedAt time.Time
	expiresAt   time.Time
	bars        []HistoricalBar
	route       Contract
	err         error
}

const (
	historicalIdenticalRequestCooldown = 15 * time.Second
	historicalFeeRateBackendTimeout    = 45 * time.Second
	historicalExactRouteBackendTimeout = 5 * time.Second
	historicalExactRouteSuccessTTL     = 30 * time.Minute
)

// ConnectorSessionBinding is an opaque, process-local identity for the
// Connector's current Connection object and socket generation. Callers may
// retain it and ask the originating Connector whether it is still current,
// but cannot manufacture broker authority from its contents.
type ConnectorSessionBinding struct {
	connector  *Connector
	connection *Connection
	epoch      uint64
}

// HistoricalSessionBinding preserves the historical-data API name while all
// broker-adjacent cached receipts share the same socket-session identity.
type HistoricalSessionBinding = ConnectorSessionBinding

// Historical failure categories are connector-authored classifications. They
// intentionally contain no broker prose and let daemon callers map failures
// onto their typed cross-surface contract without stringifying an error.
const (
	HistoricalFailureNotEntitled         = "not_entitled"
	HistoricalFailureNoData              = "no_data"
	HistoricalFailurePacing              = "pacing"
	HistoricalFailureGatewayUnavailable  = "gateway_unavailable"
	HistoricalFailureContractUnavailable = "contract_unavailable"
	HistoricalFailureProtocolRejected    = "protocol_rejected"
	HistoricalFailureInvalidPayload      = "invalid_payload"
)

// HistoricalRequestError reports a broker error from a historical-data
// request. RetryAfter is zero when the connector has no retry delay to suggest,
// and Message is untrusted broker text.
type HistoricalRequestError struct {
	Code       int
	Message    string
	RetryAfter time.Duration
	Category   string
}

// Error returns the broker message when present, otherwise a code-based
// historical-data error description.
func (e *HistoricalRequestError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Category != "" {
		return "historical data " + e.Category
	}
	return fmt.Sprintf("historical data error %d", e.Code)
}

// HistoricalDataValidationError reports a connector-authored validation
// failure. Reason is an allowlisted token and never includes broker payload.
type HistoricalDataValidationError struct {
	Reason string
}

// Error returns a fixed connector-authored description and the allowlisted
// reason token; it never returns raw broker payload text.
func (e *HistoricalDataValidationError) Error() string {
	if e == nil || e.Reason == "" {
		return "historical data validation failed"
	}
	return "historical data validation failed: " + e.Reason
}

// NewConnector constructs a stopped Connector for one broker connection. It
// performs no network I/O and defensively copies config and BaseConfig. A nil
// config uses package defaults; call [Connector.Start] to connect.
func NewConnector(config *ConnectorConfig) *Connector {
	if config == nil {
		config = &ConnectorConfig{}
	} else {
		configCopy := *config
		config = &configCopy
	}
	if config.BaseConfig == nil {
		config.BaseConfig = DefaultConfig()
	} else {
		baseConfigCopy := *config.BaseConfig
		config.BaseConfig = &baseConfigCopy
	}
	if config.PreferredClientID == 0 {
		config.PreferredClientID = config.BaseConfig.ClientID
	}
	if config.PreferredClientID == 0 {
		config.PreferredClientID = 1
	}

	// Honour IBKR_PACKET_LOG_TEMPLATE if set and BaseConfig didn't already pin
	// a packet log path. Template tokens: trailing path-separator means
	// "treat as directory"; otherwise the "%d" placeholder gets the client ID.
	// docgen:env IBKR_PACKET_LOG_TEMPLATE | Template path for raw IBKR wire-packet logs. Trailing `/` treats as directory; `%d` placeholder gets the gateway client ID. Unset disables wire logging.
	if template := strings.TrimSpace(os.Getenv("IBKR_PACKET_LOG_TEMPLATE")); template != "" && config.BaseConfig.PacketLogPath == "" {
		if strings.HasSuffix(template, string(os.PathSeparator)) {
			template = filepath.Join(template, "ibkr_client_%d.log")
		} else if !strings.Contains(template, "%d") {
			template = template + "_%d.log"
		}
		config.BaseConfig.PacketLogPath = fmt.Sprintf(template, config.PreferredClientID)
	}

	connCfg := *config.BaseConfig
	connCfg.ClientID = config.PreferredClientID

	c := &Connector{
		name:                   "IBKRConnector",
		config:                 config,
		conn:                   NewConnection(&connCfg),
		subscriptions:          make(map[string]*Subscription),
		reqIDMap:               make(map[int]string),
		openOrders:             make(map[string]*trackedOrder),
		brokerOrderIndex:       make(map[string]string),
		orderStatusLogSig:      make(map[string]string),
		contractCache:          make(map[string]ContractDetailsLite),
		optIV:                  make(map[string]float64),
		optReqIDs:              make(map[int]string),
		optQuoteBid:            make(map[string]float64),
		optQuoteAsk:            make(map[string]float64),
		optPrevClose:           make(map[string]float64),
		optGreeks:              make(map[string]Greeks),
		optUnderlyingPx:        make(map[string]float64),
		contractDetailsReqs:    make(map[int]*contractDetailsRequest),
		historicalReqs:         make(map[int]*historicalRequest),
		historicalBackoff:      make(map[string]int),
		historicalExactFlights: make(map[string]*historicalExactFlight),
		historicalRouteReqs:    make(map[int]chan error),
		pnl:                    newPnLCache(),
	}
	c.conn.evidenceBarrier = &c.evidenceBarrier
	c.conn.publicationBarrier = &c.publicationBarrier
	c.fetchContractDetails = c.FetchContractDetails
	c.resolveWSHContract = c.resolveWSHStockContract
	c.resolveWSHExactContract = c.resolveWSHExactStockContract
	c.wshGate = make(chan struct{}, 1)
	c.wshGate <- struct{}{}
	return c
}

func (c *Connector) logInfo(format string, args ...any) {
	connectorLogger.Infof("%s: "+format, append([]any{c.name}, args...)...)
}

func (c *Connector) logWarn(format string, args ...any) {
	connectorLogger.Warnf("%s: "+format, append([]any{c.name}, args...)...)
}

func (c *Connector) logDebug(format string, args ...any) {
	connectorLogger.Debugf("%s: "+format, append([]any{c.name}, args...)...)
}

func (c *Connector) recordContractTiming(symbol string, elapsed time.Duration, resolved bool) {
	if symbol == "" || elapsed <= 0 {
		return
	}
	if c.contractTimingHook != nil {
		c.contractTimingHook(symbol, elapsed, resolved)
	}
	if c.conn != nil {
		c.conn.observeContractTiming(symbol, elapsed, resolved)
	}
}

func (c *Connector) inactiveReason(symbol string) (string, bool) {
	c.inactiveMu.RLock()
	state, ok := c.inactiveSymbols[symbol]
	c.inactiveMu.RUnlock()
	if !ok {
		return "", false
	}
	// Lazy TTL expiry, mirroring the absence memory: an expired mark is
	// deleted on read and the name earns a fresh probe; re-marking needs a
	// fresh 2-in-10-min confirmation.
	if time.Since(state.markedAt) > inactiveMarkTTL {
		c.inactiveMu.Lock()
		if cur, still := c.inactiveSymbols[symbol]; still && cur.markedAt.Equal(state.markedAt) {
			delete(c.inactiveSymbols, symbol)
		}
		c.inactiveMu.Unlock()
		return "", false
	}
	return state.reason, true
}

// InactiveReason reports an unexpired in-memory inactivity mark for symbol.
// It performs no broker request. The boolean is false when no mark exists or
// the mark has expired; the returned reason is untrusted broker text.
func (c *Connector) InactiveReason(symbol string) (string, bool) {
	if symbol == "" {
		return "", false
	}
	if reason, ok := c.inactiveReason(symbol); ok {
		return reason, true
	}
	upper := strings.ToUpper(symbol)
	if upper != symbol {
		return c.inactiveReason(upper)
	}
	return "", false
}

// IsSymbolInactive reports whether symbol has an unexpired in-memory
// inactivity mark. It performs no broker request.
func (c *Connector) IsSymbolInactive(symbol string) bool {
	_, inactive := c.InactiveReason(symbol)
	return inactive
}

func (c *Connector) hasActiveContract(symbol string) bool {
	symbol = strings.ToUpper(symbol)
	c.contractMu.RLock()
	detail, ok := c.contractCache[symbol]
	c.contractMu.RUnlock()
	return ok && detail.ConID != 0
}

func (c *Connector) clearInactiveCandidate(symbol string) {
	c.inactiveMu.Lock()
	if c.inactiveCandidates != nil {
		delete(c.inactiveCandidates, strings.ToUpper(symbol))
	}
	c.inactiveMu.Unlock()
}

// inactiveConfirmations and inactiveCandidateWindow gate the in-memory
// inactive mark: a definition error must repeat within the window before a
// key is suppressed. A single code-200/162 is routinely transient (gateway
// hiccup, contract-cache race) — observed 2026-06-11 on the currency-ledger
// FX repair path, where one transient 200 suppressed an IDEALPRO route for
// the connector's lifetime. Inactive means delisted/unknown contract
// (docs/docs/internals/architecture.md), and steady pollers re-request every cycle, so a
// genuinely dead name confirms within a couple of poll intervals.
const (
	inactiveConfirmations   = 2
	inactiveCandidateWindow = 10 * time.Minute
)

func joinPostActions(first, second func()) func() {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	default:
		return func() {
			first()
			second()
		}
	}
}

func (c *Connector) registerInactiveCandidate(symbol, reason string) bool {
	marked, post := c.registerInactiveCandidatePostAction(symbol, reason)
	if post != nil {
		post()
	}
	return marked
}

// registerInactiveCandidatePostAction performs only local state mutation and
// returns any broker-side subscription cleanup to its caller. Socket readers
// run the action after releasing inbound/publication/evidence leases.
func (c *Connector) registerInactiveCandidatePostAction(symbol, reason string) (bool, func()) {
	if symbol == "" {
		return false, nil
	}
	// Choke-point farm guard: both write paths (subscription notices AND
	// historical failures) converge here. While any tracked farm is
	// impaired the gateway's definition errors are a session verdict, not
	// a contract verdict — counting them is how a nightly-reset wedge
	// marked held AMD/BB/IBM and VIX inactive (2026-07-08).
	if c.marketDataFarmImpaired() {
		return false, nil
	}
	symbol = strings.ToUpper(symbol)

	upperReason := strings.ToUpper(reason)
	definitionDead := strings.Contains(upperReason, "NO SECURITY DEFINITION") || strings.Contains(upperReason, "NO DATA")
	// An actively cached contract vetoes non-definition reasons outright.
	// Definition-grade errors still count toward the confirmation
	// threshold below — they no longer mark on first sight.
	if c.hasActiveContract(symbol) && !definitionDead {
		c.clearInactiveCandidate(symbol)
		return false, nil
	}

	reason = strings.TrimSpace(reason)
	now := time.Now()
	c.inactiveMu.Lock()
	if c.inactiveSymbols != nil {
		if _, exists := c.inactiveSymbols[symbol]; exists {
			c.inactiveMu.Unlock()
			return true, nil
		}
	}
	if c.inactiveCandidates == nil {
		c.inactiveCandidates = make(map[string]inactiveCandidateState)
	}
	state := c.inactiveCandidates[symbol]
	if !state.lastUpdated.IsZero() && now.Sub(state.lastUpdated) > inactiveCandidateWindow {
		// Occurrences far apart are independent transients, not a
		// confirmation.
		state = inactiveCandidateState{}
	}
	state.count++
	state.lastReason = reason
	state.lastUpdated = now
	shouldMark := state.count >= inactiveConfirmations
	if shouldMark {
		delete(c.inactiveCandidates, symbol)
	} else {
		c.inactiveCandidates[symbol] = state
	}
	c.inactiveMu.Unlock()

	if shouldMark {
		return true, c.markSymbolInactivePostAction(symbol, reason)
	}
	return false, nil
}

func (c *Connector) markSymbolInactive(symbol, reason string) {
	if post := c.markSymbolInactivePostAction(symbol, reason); post != nil {
		post()
	}
}

func (c *Connector) markSymbolInactivePostAction(symbol, reason string) func() {
	if symbol == "" {
		return nil
	}
	symbol = strings.ToUpper(symbol)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "inactive"
	}

	c.inactiveMu.Lock()
	if c.inactiveSymbols == nil {
		c.inactiveSymbols = make(map[string]inactiveSymbolState)
	}
	if c.inactiveCandidates != nil {
		delete(c.inactiveCandidates, symbol)
	}
	if _, exists := c.inactiveSymbols[symbol]; exists {
		c.inactiveMu.Unlock()
		return nil
	}
	state := inactiveSymbolState{
		reason:   reason,
		markedAt: time.Now(),
	}
	c.inactiveSymbols[symbol] = state
	c.inactiveMu.Unlock()

	post := c.detachSubscription(symbol)
	c.logInfo("Suppressing market data for %s (inactive: %s)", symbol, reason)
	return post
}

func (c *Connector) processSystemNotice(alias reqAliasEntry, note *systemNotification) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	binding := ConnectorSessionBinding{connector: c, connection: conn}
	if conn != nil {
		binding.epoch = conn.BrokerSessionEpoch()
	}
	if post := c.processSystemNoticeFrom(binding, alias, note); post != nil {
		post()
	}
}

func (c *Connector) processSystemNoticeFrom(origin ConnectorSessionBinding, alias reqAliasEntry, note *systemNotification) (postBarrier func()) {
	if note == nil {
		return nil
	}
	func() {
		c.publicationBarrier.RLock()
		defer c.publicationBarrier.RUnlock()
		c.evidenceBarrier.RLock()
		defer c.evidenceBarrier.RUnlock()
		if !c.SessionReceiptCurrent(origin) {
			if note.tickerID > 0 {
				c.notifyOrderErrorLifecycleUnderBarrier(origin, int(note.tickerID), note.code, note.message, note.advancedOrderRejectJSON)
			}
			return
		}
		// Backend-connectivity notices (1100/1101/1102) are session-global
		// and must be tracked on every current-session path, including the
		// historical-collision early returns below: during an outage TWS
		// also answers pending requests with per-reqID code-1100 notices.
		defer func() {
			if post := c.handleBackendConnectivityNotice(origin, note); post != nil {
				postBarrier = joinPostActions(postBarrier, post)
			}
		}()
		// Legacy sessions and already-consumed IDs may still surface delayed broker
		// errors, so active exact-historical ownership wins before any durable order
		// lifecycle callback. New allocations use one monotonic disjoint namespace.
		if note.tickerID > 0 && c.failPendingExactHistoricalRoute(int(note.tickerID), note.code, note.message) {
			c.recordDataFarmNotice(note.code, note.message, note.timestamp)
			return
		}
		if note.tickerID > 0 && c.isKnownBrokerOrderID(int(note.tickerID)) &&
			c.failNoticeCollisionHistorical(int(note.tickerID), note.code, note.message) {
			c.recordDataFarmNotice(note.code, note.message, note.timestamp)
			return
		}
		if note.tickerID > 0 {
			c.notifyOrderErrorLifecycleUnderBarrier(origin, int(note.tickerID), note.code, note.message, note.advancedOrderRejectJSON)
		}
		c.recordDataFarmNotice(note.code, note.message, note.timestamp)
		// msg-204 delivers order errors and request errors through the same id
		// field. When the id names an order this connector owns,
		// notifyOrderErrorLifecycle above owns the notice:
		// request-scoped recovery and inactive-candidate marking must not act
		// on an innocent historical request or live subscription that happens
		// to share the integer.
		if note.tickerID > 0 && c.isKnownBrokerOrderID(int(note.tickerID)) {
			return
		}
		// Request-scoped recovery must run before the alias-based inactive
		// logic below: historical reqIDs never register an alias, so the
		// inactiveKey early-return would skip them entirely. This is the only
		// live error path on current gateways — TWS API server ≥203 delivers
		// request errors as system notifications (msg 204), not msgErrMsg
		// frames, so handleIBKRError/handleErrorMessage never see them
		// (observed 2026-06-11: 779 system notices, zero msgErrMsg errors).
		postBarrier = c.recoverFromSystemNotice(origin, alias, note)

		upperMsg := strings.ToUpper(note.message)
		definitionDead := false
		switch note.code {
		case 200:
			definitionDead = strings.Contains(upperMsg, "NO SECURITY DEFINITION")
		case 162:
			definitionDead = strings.Contains(upperMsg, "NO DATA")
		case 366:
			definitionDead = true
		}
		if !definitionDead {
			return
		}
		// Record the inactive candidate under the connector's own subscription
		// or contract-details key — exactly what the subscribe and resolve
		// paths check before re-requesting. The former alias-derived route key
		// embedded gateway-hydrated localSymbol/tradingClass/primaryExch fields
		// that no check-time key contains, so marks never suppressed anything.
		key := c.resolutionKeyForNotice(int(note.tickerID), alias)
		if key == "" {
			c.logDebug("Ignoring definition error code %d for unowned or derivative request %s (%s): %s", note.code, alias.symbol, alias.localSymbol, note.message)
			return
		}
		_, inactivePost := c.registerInactiveCandidatePostAction(key, note.message)
		postBarrier = joinPostActions(postBarrier, inactivePost)
	}()
	return postBarrier
}

// historicalNoticeOwnsIDCollision is a closed code allowlist, never a broker
// prose classifier. Explicit order-lifecycle codes remain order-owned; these
// codes describe historical/market-data request rejection or validation and
// therefore belong to an already-open historical request on numeric overlap.
func historicalNoticeOwnsIDCollision(code int) bool {
	switch code {
	case 162, 200, 321, 354, 366,
		502, 504, 1100, 1300,
		10089, 10090, 10091, 10186, 10187:
		return true
	default:
		return false
	}
}

func (c *Connector) failNoticeCollisionHistorical(reqID, code int, message string) bool {
	if !historicalNoticeOwnsIDCollision(code) {
		return false
	}
	c.historicalMu.Lock()
	request := c.historicalReqs[reqID]
	requestOwns := request != nil && request.requestOwnsNoticeCollision
	c.historicalMu.Unlock()
	if !requestOwns {
		return false
	}
	return c.failPendingHistorical(reqID, code, message)
}

func (c *Connector) failPendingExactHistoricalRoute(reqID, code int, message string) bool {
	if !historicalNoticeOwnsIDCollision(code) {
		return false
	}
	c.historicalMu.Lock()
	failureCh := c.historicalRouteReqs[reqID]
	if failureCh != nil {
		delete(c.historicalRouteReqs, reqID)
	}
	c.historicalMu.Unlock()
	if failureCh == nil {
		return false
	}
	failureCh <- &HistoricalRequestError{Code: code, Message: message}
	return true
}

// recoverFromSystemNotice drives the request-scoped recovery that the
// legacy msgErrMsg path (handleIBKRError / handleErrorMessage) promised
// but never receives on current gateways:
//
//   - a pending historical request fails immediately instead of burning
//     its timeout and then wire-cancelling a query the server already
//     killed (the recurring error-366 source); the error — including 200
//     "no security definition" — propagates to the caller;
//   - in-flight market-data pollers get the RejectCh fast-abort;
//   - terminal entitlement/definition rejections (200/354) release the
//     rate-limiter slot, mark the exact reqID server-dead so teardown
//     skips the futile wire cancel (the recurring error-300 source), and
//     — for 354 — feed the absence memory so steady pollers stop
//     re-requesting a name with no data entitlement (the recurring
//     354+2129 source);
//   - 10197 keeps its force-delayed side effect.
//
// Deliberately NOT ported from handleIBKRError: refreshSubscription's
// blind alternate-routing resubscribe on 200/320/321/354 — re-requesting
// a terminally rejected subscription is exactly the churn loop this
// recovery exists to stop, and recovery-after-the-window is owned by the
// absence TTL.
func (c *Connector) recoverFromSystemNotice(origin ConnectorSessionBinding, alias reqAliasEntry, note *systemNotification) (postBarrier func()) {
	if note.tickerID <= 0 {
		return nil
	}
	reqID := int(note.tickerID)
	code := note.code

	if c.failPendingHistorical(reqID, code, note.message) {
		return nil
	}

	// Contract-details requests hold no market-data slot and no subscription,
	// so they exit here rather than falling through to the rejection and
	// slot-release handling below.
	if c.failPendingContractDetails(reqID, code, note.message) {
		return nil
	}

	c.pushSubscriptionRejection(reqID, code, note.message)

	switch code {
	case 200, 354:
		c.markSubscriptionRejected(reqID)
		if origin.connection != nil {
			origin.connection.releaseMarketDataSlot(reqID)
		}
	case 10197:
		if origin.connection != nil && origin.connection.markCompetingLiveSession(strconv.Itoa(reqID)) {
			postBarrier = func() {
				if err := origin.connection.setMarketDataTypeAtEpoch(3, origin.epoch); err != nil {
					ibkrLogger.Errorf("[cid=%d] Failed to request delayed market data after 10197: %v", origin.connection.config.ClientID, err)
				} else {
					ibkrLogger.Warnf("[cid=%d] Forced delayed market data after 10197 (%s)", origin.connection.config.ClientID, note.message)
				}
			}
		}
	}

	if code == 354 {
		c.maybeRememberAbsenceForReqID(reqID, alias, code, note.message)
	}
	return postBarrier
}

// contractDetailsRequest tracks one in-flight reqContractDetails so a system
// notice targeting its reqID can fail it immediately. resolutionKey is the
// inactive-mark key a definition rejection should count against, empty for
// derivative probes whose rejections are routine (option fan-outs request
// strike supersets; FX resolution quotes both pair directions).
type contractDetailsRequest struct {
	resolutionKey string
	fail          chan error
}

// registerContractDetailsRequest arms reqID for notice-driven failure and
// returns the request plus a release func the caller must defer.
func (c *Connector) registerContractDetailsRequest(reqID int, resolutionKey string) (*contractDetailsRequest, func()) {
	req := &contractDetailsRequest{
		resolutionKey: resolutionKey,
		fail:          make(chan error, 1),
	}
	c.contractDetailsMu.Lock()
	c.contractDetailsReqs[reqID] = req
	c.contractDetailsMu.Unlock()
	return req, func() {
		c.contractDetailsMu.Lock()
		delete(c.contractDetailsReqs, reqID)
		c.contractDetailsMu.Unlock()
	}
}

// pendingContractDetailsKey returns the inactive-mark key a definition
// rejection for reqID should count against, or "" when reqID is not a live
// contract-details request or is a derivative probe.
func (c *Connector) pendingContractDetailsKey(reqID int) string {
	c.contractDetailsMu.Lock()
	req, ok := c.contractDetailsReqs[reqID]
	c.contractDetailsMu.Unlock()
	if !ok {
		return ""
	}
	return req.resolutionKey
}

// failPendingContractDetails fails the contract-details request owning reqID,
// if any, so the caller returns on the broker's answer instead of burning its
// full budget and reporting a timeout. Informational codes leave the request
// running, matching failPendingHistorical. Returns true when reqID belonged to
// a pending contract-details request.
func (c *Connector) failPendingContractDetails(reqID, code int, message string) bool {
	if code == 0 || code == -1 || (code >= 2100 && code < 2200) {
		return false
	}
	c.contractDetailsMu.Lock()
	req, ok := c.contractDetailsReqs[reqID]
	c.contractDetailsMu.Unlock()
	if !ok {
		return false
	}
	err := fmt.Errorf("contract details request failed (IBKR %d)", code)
	if code == 200 && strings.Contains(strings.ToUpper(message), "NO SECURITY DEFINITION") {
		err = ErrContractNoDefinition
	}
	select {
	case req.fail <- err:
	default:
	}
	return true
}

// failPendingHistorical fails the historical request owning reqID, if
// any, mirroring handleIBKRError's histPending branch (162 keeps its
// pacing backoff, anything else resets it). Informational codes leave the
// request running. Returns true when reqID belonged to a pending
// historical request.
func (c *Connector) failPendingHistorical(reqID, code int, message string) bool {
	if code == 0 || code == -1 || (code >= 2100 && code < 2200) {
		return false
	}
	c.historicalMu.Lock()
	hr, ok := c.historicalReqs[reqID]
	c.historicalMu.Unlock()
	if !ok {
		return false
	}
	hErr := &HistoricalRequestError{Code: code, Message: message}
	switch code {
	case 162:
		if hErr.Message == "" {
			hErr.Message = "historical data pacing violation"
		}
		hErr.RetryAfter = c.nextHistoricalBackoff(hr.symbol)
	case 321:
		if hErr.Message == "" {
			hErr.Message = "historical data request failed validation"
		}
		c.resetHistoricalBackoff(hr.symbol)
	default:
		c.resetHistoricalBackoff(hr.symbol)
	}
	c.failHistoricalRequest(reqID, hErr)
	return true
}

// markSubscriptionRejected records that the gateway terminally killed
// reqID's subscription server-side. Guarded on sub.ReqID == reqID so a
// notice for a stale reqID can never poison a live replacement
// subscription that reused the key.
func (c *Connector) markSubscriptionRejected(reqID int) {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	key, ok := c.reqIDMap[reqID]
	if !ok {
		return
	}
	if sub, ok := c.subscriptions[key]; ok && sub != nil && sub.ReqID == reqID {
		sub.rejectedReqID = reqID
	}
}

// maybeRememberAbsenceForReqID feeds the market-data absence memory for a
// terminal 354, keyed by the connector's own subscription key (see
// subscriptionKeyForNotice) so the record key is identical to what the
// subscribe paths check.
func (c *Connector) maybeRememberAbsenceForReqID(reqID int, alias reqAliasEntry, code int, message string) {
	key := c.subscriptionKeyForNotice(reqID, alias)
	if key == "" {
		return
	}
	c.rememberMarketDataAbsence(key, code, message)
}

// subscriptionKeyForNotice resolves the connector-owned subscription key a
// request-scoped notice may act on: reqIDMap[reqID], i.e. exactly what the
// subscribe paths check before re-requesting — bare symbol for
// SubscribeMarketData, route key for SubscribeMarketDataWithContract. Never
// alias.symbol: for option-IV subscriptions that is the underlying and a
// record there would blind the stock. Returns "" when the notice must not
// feed symbol-level memory: reqIDs the connector does not own
// (contract-details probes, snapshots, historical), option reqIDs and
// derivative aliases, and notices arriving while a market-data farm is
// impaired (a bounce-window error is not a verdict on the contract).
func (c *Connector) subscriptionKeyForNotice(reqID int, alias reqAliasEntry) string {
	if reqID <= 0 {
		return ""
	}
	c.subMu.RLock()
	key := c.reqIDMap[reqID]
	c.subMu.RUnlock()
	if key == "" {
		return ""
	}
	c.optMu.RLock()
	_, isOptionReq := c.optReqIDs[reqID]
	c.optMu.RUnlock()
	if isOptionReq {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(alias.secType)) {
	case "OPT", "FOP", "WAR", "BAG":
		return ""
	}
	if c.marketDataFarmImpaired() {
		return ""
	}
	return key
}

// resolutionKeyForNotice returns the inactive-mark key a definition rejection
// for reqID should count against: the subscription key when the connector owns
// a live subscription, otherwise the key of a live contract-details request.
// Contract-details rejections are the only evidence a name that can no longer
// be resolved ever produces — it never reaches a market-data subscription — so
// excluding them left such a name re-probing forever. Both branches keep the
// farm-impaired guard: while any tracked farm is impaired the gateway's
// definition errors are a session verdict, not a contract verdict.
func (c *Connector) resolutionKeyForNotice(reqID int, alias reqAliasEntry) string {
	if key := c.subscriptionKeyForNotice(reqID, alias); key != "" {
		return key
	}
	if reqID <= 0 || c.marketDataFarmImpaired() {
		return ""
	}
	return c.pendingContractDetailsKey(reqID)
}

func (c *Connector) recordDataFarmNotice(code int, message string, asOf time.Time) {
	farm, ok := dataFarmStatusFromNotice(code, message, asOf)
	if !ok {
		return
	}
	c.dataFarmMu.Lock()
	if c.dataFarms == nil {
		c.dataFarms = make(map[string]DataFarmStatus)
	}
	// A farm-level OK says nothing about the TWS<->IBKR link. This used to
	// clear the connectivity mark on ANY ok notice, so a routine 2104
	// ("market data farm connection is OK:usfarm") erased a 1100/2110 break
	// — and because an impaired connectivity row short-circuits every farm
	// type to unavailable in the status surface, that turned a real outage
	// green. Only a connectivity notice writes the connectivity key now,
	// which 1101/1102 already do through the generic path below: they carry
	// type "connectivity" and the same forced "tws-server" name, so the
	// overwrite and its impaired->ok stamp land there.
	key := dataFarmKey(farm.Type, farm.Name)
	if prev, had := c.dataFarms[key]; had && farmStatusImpaired(prev.Status) && farm.Status == "ok" {
		c.farmRecoveryAt = time.Now()
	}
	c.dataFarms[key] = farm
	c.dataFarmMu.Unlock()
}

func dataFarmStatusFromNotice(code int, message string, asOf time.Time) (DataFarmStatus, bool) {
	farmType, ok := dataFarmTypeForCode(code, message)
	if !ok {
		return DataFarmStatus{}, false
	}
	status := dataFarmStatusForCode(code)
	if status == "" {
		return DataFarmStatus{}, false
	}
	name := dataFarmNameFromMessage(message)
	// Connectivity notices name the TWS<->IBKR link, never a farm. 1102's
	// trailing "All data farms are connected: a; b; ..." would otherwise
	// become the name and mint a key that 1100's broken mark never matches.
	if farmType == "connectivity" {
		name = "tws-server"
	} else if name == "" {
		switch farmType {
		case "security_definition":
			name = "secdef"
		default:
			name = farmType
		}
	}
	if asOf.IsZero() {
		asOf = time.Now()
	}
	return DataFarmStatus{
		Name:    name,
		Type:    farmType,
		Status:  status,
		Code:    code,
		Message: message,
		AsOf:    asOf,
	}, true
}

func dataFarmTypeForCode(code int, message string) (string, bool) {
	switch code {
	case 2103, 2104, 2108, 2119:
		return "market", true
	case 2105, 2106, 2107:
		return "historical", true
	case 2157, 2158:
		return "security_definition", true
	case 1100, 1101, 1102, 2110:
		// 1100/1101/1102 word the link as "between IBKR and Trader
		// Workstation", so the "connectivity between tws" fallback below
		// (2110's wording) never matches them; they must be code-mapped.
		// Observed 2026-07-29 16:24:17: an untracked 1100 arrived 1ms
		// before TWS flushed queued code=200 answers for pending
		// resubscriptions, and IWM/TLT/QQQ were marked inactive with the
		// per-farm break notices still 4s away.
		return "connectivity", true
	}
	msg := strings.ToLower(message)
	switch {
	case strings.Contains(msg, "hmds") || strings.Contains(msg, "historical data farm"):
		return "historical", true
	case strings.Contains(msg, "sec-def") || strings.Contains(msg, "security definition data farm"):
		return "security_definition", true
	case strings.Contains(msg, "market data farm"):
		return "market", true
	case strings.Contains(msg, "connectivity between tws"):
		return "connectivity", true
	default:
		return "", false
	}
}

func dataFarmStatusForCode(code int) string {
	switch code {
	case 1101, 1102, 2104, 2106, 2119, 2158:
		return "ok"
	case 2107, 2108:
		return "inactive"
	case 2103, 2105, 2157:
		return "disconnected"
	case 1100, 2110:
		return "broken"
	default:
		return ""
	}
}

func dataFarmNameFromMessage(message string) string {
	if idx := strings.LastIndex(message, ":"); idx >= 0 && idx+1 < len(message) {
		name := strings.TrimSpace(message[idx+1:])
		name = strings.Trim(name, ".")
		return name
	}
	return ""
}

func dataFarmKey(farmType, name string) string {
	return strings.ToLower(strings.TrimSpace(farmType)) + "\x00" + strings.ToLower(strings.TrimSpace(name))
}

// mdReplaySpec captures the exact wire-request shape of a shared market-data
// subscription. symbol=="" selects the routed-contract form; a non-empty
// primaryExch selects the primary-exchange form of the bare-symbol request.
type mdReplaySpec struct {
	contract     Contract
	genericTicks string
	symbol       string
	primaryExch  string
}

// handleBackendConnectivityNotice tracks the TWS<->IBKR backend link.
// 1100: link lost — refuse new order transmissions until a restore notice.
// 1102: restored with server-side state maintained — nothing to replay.
// 1101: restored with subscriptions LOST server-side — the gateway will never
// resume ticking the old request IDs, so the post action replays every live
// shared market-data subscription and force-rebuilds the account-updates and
// daily-P&L streams.
func (c *Connector) handleBackendConnectivityNotice(origin ConnectorSessionBinding, note *systemNotification) func() {
	switch note.code {
	case 1100:
		c.setBackendConnectivityDown(true, note.timestamp)
		return nil
	case 1102:
		c.setBackendConnectivityDown(false, note.timestamp)
		return nil
	case 1101:
		c.setBackendConnectivityDown(false, note.timestamp)
		return func() { go c.recoverFromBackendDataLoss(origin) }
	default:
		return nil
	}
}

func (c *Connector) setBackendConnectivityDown(down bool, at time.Time) {
	if at.IsZero() {
		at = time.Now()
	}
	c.backendConnMu.Lock()
	changed := c.backendConnDown != down
	c.backendConnDown = down
	c.backendConnAt = at
	c.backendConnMu.Unlock()
	if !changed {
		return
	}
	if down {
		c.logWarn("TWS lost connectivity to the IBKR backend (code 1100); refusing order transmission until a 1101/1102 restore notice")
	} else {
		c.logInfo("TWS restored connectivity to the IBKR backend")
	}
}

func (c *Connector) backendConnectivityDown() (bool, time.Time) {
	c.backendConnMu.Lock()
	defer c.backendConnMu.Unlock()
	return c.backendConnDown, c.backendConnAt
}

// recoverFromBackendDataLoss is the 1101 post action. Exact-session
// subscriptions (SessionEpoch != 0) are excluded: they are short-lived
// broker-write evidence bound to one request, and their owners fail and
// retry themselves. Runs off the read loop; the in-flight guard makes a
// duplicate 1101 a no-op instead of a double replay.
func (c *Connector) recoverFromBackendDataLoss(origin ConnectorSessionBinding) {
	if !c.mdReplayInFlight.CompareAndSwap(false, true) {
		c.logDebug("1101 subscription replay already in flight; skipping duplicate")
		return
	}
	defer c.mdReplayInFlight.Store(false)
	replayed, dropped := c.replayMarketDataSubscriptions(origin)
	if replayed > 0 || dropped > 0 {
		c.logInfo("Replayed %d market-data subscriptions after 1101 data loss (%d dropped for demand re-subscribe)", replayed, dropped)
	}
	c.acctUpdatesMu.Lock()
	hadAcctStream := !c.acctUpdatesLastAt.IsZero()
	c.acctUpdatesMu.Unlock()
	if hadAcctStream {
		_ = c.resubscribeAccountUpdates()
	}
	c.forceResubscribeDailyPnL()
}

func (c *Connector) replayMarketDataSubscriptions(origin ConnectorSessionBinding) (replayed, dropped int) {
	type replayEntry struct {
		key      string
		sub      *Subscription
		oldReqID int
		spec     mdReplaySpec
	}
	var entries []replayEntry
	c.subMu.RLock()
	for key, sub := range c.subscriptions {
		if sub == nil || sub.SessionEpoch != 0 || sub.ReqID == 0 || sub.replaySpec == nil {
			continue
		}
		if sub.rejectedReqID == sub.ReqID {
			// The gateway already tore this ticker down terminally; the
			// demand paths own any retry decision.
			continue
		}
		entries = append(entries, replayEntry{key: key, sub: sub, oldReqID: sub.ReqID, spec: *sub.replaySpec})
	}
	c.subMu.RUnlock()

	for _, e := range entries {
		if origin.connection == nil || !c.SessionCurrent(origin) {
			// Socket bounced mid-replay; the successor session rebuilds
			// everything from scratch via invalidateUnstampedConnectorObservations.
			return replayed, dropped
		}
		_ = origin.connection.CancelMarketData(e.oldReqID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		var (
			newReqID int
			err      error
		)
		switch {
		case e.spec.symbol == "":
			newReqID, err = origin.connection.RequestMarketDataWithContract(ctx, e.spec.contract, e.spec.genericTicks, false, false)
		case e.spec.primaryExch != "":
			newReqID, err = origin.connection.RequestMarketDataWithPrimary(ctx, e.spec.symbol, e.spec.primaryExch)
		default:
			newReqID, err = origin.connection.RequestMarketData(ctx, e.spec.symbol)
		}
		cancel()
		c.subMu.Lock()
		current := c.subscriptions[e.key]
		if current != e.sub || current.ReqID != e.oldReqID {
			// Raced with an unsubscribe or competing rebuild; do not adopt.
			c.subMu.Unlock()
			if err == nil && newReqID != 0 {
				_ = origin.connection.CancelMarketData(newReqID)
			}
			continue
		}
		if err != nil || newReqID == 0 {
			// Drop the entry so the demand paths re-create it instead of
			// leaving a forever-dead reqID mapping behind.
			delete(c.subscriptions, e.key)
			delete(c.reqIDMap, e.oldReqID)
			c.subMu.Unlock()
			dropped++
			c.logWarn("Failed to replay market data for %s after 1101 (%v); dropped for demand re-subscribe", e.key, err)
			continue
		}
		delete(c.reqIDMap, e.oldReqID)
		c.reqIDMap[newReqID] = e.key
		e.sub.ReqID = newReqID
		// LastTime only: re-issuing the request is not an observation, and
		// LastTickAt must keep pointing at the last real tick so a replay that
		// never resumes ticking stays visible as a growing gap.
		e.sub.LastTime = time.Now()
		c.subMu.Unlock()
		replayed++
	}
	return replayed, dropped
}

type retiredMarketDataSubscription struct {
	connection *Connection
	epoch      uint64
	reqID      int
	cancel     bool
}

func (retired retiredMarketDataSubscription) run() {
	if retired.connection == nil || retired.reqID <= 0 {
		return
	}
	if retired.cancel {
		// Epoch-bound cancellation either writes on the originating socket or
		// returns a definite stale refusal. It always releases only that epoch's
		// slot, never a successor's reused reqID.
		_ = retired.connection.cancelMarketDataForEpoch(context.Background(), retired.reqID, retired.epoch)
		return
	}
	retired.connection.releaseMarketDataSlotAtEpoch(retired.reqID, retired.epoch)
}

func (c *Connector) detachSubscription(symbol string) func() {
	if symbol == "" {
		return nil
	}
	upper := strings.ToUpper(symbol)

	// Lift the cancel target under the lock, then release before calling
	// CancelMarketData — that path goes through the rate limiter (up to
	// 30 s) and the connection's writeMu, neither of which should run
	// while subMu is held: every other subscription reader (handleTick,
	// MarketDataSnapshot, scan enrichment) blocks on subMu.
	c.subMu.Lock()
	var cancelReqID, releaseReqID int
	var subscriptionEpoch uint64
	if sub, ok := c.subscriptions[upper]; ok {
		subscriptionEpoch = sub.SessionEpoch
		// Same wire-cancel exception as UnsubscribeMarketData: a reqID
		// the gateway already reported dead only draws error 300 on
		// cancel — release its slot instead (idempotent).
		if wireCancelNeeded(sub) {
			cancelReqID = sub.ReqID
		} else {
			releaseReqID = sub.ReqID
		}
		delete(c.subscriptions, upper)
	}
	for reqID, sym := range c.reqIDMap {
		if strings.EqualFold(sym, upper) {
			delete(c.reqIDMap, reqID)
		}
	}
	conn := c.conn
	epoch := uint64(0)
	if conn != nil {
		epoch = conn.BrokerSessionEpoch()
	}
	if subscriptionEpoch != 0 {
		epoch = subscriptionEpoch
	}
	c.subMu.Unlock()

	c.optMu.Lock()
	for reqID, sym := range c.optReqIDs {
		if strings.EqualFold(sym, upper) {
			delete(c.optReqIDs, reqID)
		}
	}
	c.optMu.Unlock()

	retired := retiredMarketDataSubscription{connection: conn, epoch: epoch}
	switch {
	case cancelReqID != 0:
		retired.reqID, retired.cancel = cancelReqID, true
	case releaseReqID != 0:
		retired.reqID = releaseReqID
	default:
		return nil
	}
	return retired.run
}

// SetMarketDataType requests the market-data mode for subsequent requests:
// 1=live, 2=frozen, 3=delayed, and 4=delayed-frozen. A live request is reduced
// to delayed mode when the connection has detected a competing live session.
// It returns an error when no broker connection is active or the write fails.
func (c *Connector) SetMarketDataType(dataType int) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return fmt.Errorf("IBKR connection not available")
	}
	if dataType == 1 && conn.HasCompetingLiveSession() {
		connectorLogger.Warnf("%s: Live market data blocked by competing session; forcing delayed mode", c.name)
		dataType = 3
	}
	return conn.SetMarketDataType(dataType)
}

// Start attaches lifecycle handlers and attempts to open the Connector's
// broker connection. It returns an error when already started. An initial
// connection failure leaves the Connector running but not ready and is exposed
// through [Connector.LastError], so that failure does not make Start fail.
// Context cancellation bounds the connection attempt.
func (c *Connector) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return fmt.Errorf("connector already running")
	}
	c.mu.Unlock()

	// Connector start/stop narration and the degraded-connect lines log at
	// Debug: the daemon rebuilds the connector on every reconnect cycle while
	// the gateway is down, so at INFO/WARN this floods ibkr-daemon.log
	// (~4k×/night off-hours). A *successful* start still logs "started
	// successfully" at INFO below. The degraded-connect WARN is demoted too:
	// it fires 1:1 with the daemon's gateway-unreachable verdict (which is
	// deduped to one WARN per outage), and programmatic callers read the
	// failure via LastError() rather than this log line.
	c.logDebug("Starting IBKR connector (client_id: %d)", c.config.PreferredClientID)

	c.attachConnectionHooks(c.conn)

	if err := c.conn.Connect(ctx); err != nil {
		c.logDebug("Failed to connect to IBKR: %v", err)
		c.logDebug("Running in degraded mode without IBKR connection")
		c.mu.Lock()
		c.running = true
		c.lastError = err
		c.mu.Unlock()
		return nil
	}

	c.mu.Lock()
	c.running = true
	c.lastError = nil
	c.mu.Unlock()

	c.logInfo("IBKR connector started successfully (client_id: %d)", c.config.PreferredClientID)

	return nil
}

// LastError returns the most recent connector startup error that left the
// connector in degraded mode. Empty means either healthy or no concrete
// connector-level diagnosis is available.
func (c *Connector) LastError() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.lastError == nil {
		return ""
	}
	return c.lastError.Error()
}

func (c *Connector) attachConnectionHooks(conn *Connection) {
	c.ensureHandlersRegistered(conn)
	conn.SetOnConnect(func() {
		c.onConnectionEstablished(conn)
	})
	conn.SetOnDisconnect(func(err error) {
		c.onConnectionLost(conn)
	})

	// If the connection is already established (e.g., reused after a hot
	// reconfigure), publish it ready immediately. The handlers were installed
	// above before this publication.
	if conn.IsConnected() {
		c.onConnectionEstablished(conn)
	}
}

func (c *Connector) onConnectionEstablished(conn *Connection) {
	c.resetWSHMetadataReadiness()
	c.evidenceBarrier.RLock()
	c.invalidateUnstampedConnectorObservations(conn)
	c.mu.Lock()
	c.conn = conn
	c.ready = true
	c.lastError = nil
	c.mu.Unlock()
	c.evidenceBarrier.RUnlock()
}

func (c *Connector) onConnectionLost(conn *Connection) {
	c.resetWSHMetadataReadiness()
	c.evidenceBarrier.RLock()
	c.mu.Lock()
	if c.conn == conn {
		c.ready = false
	}
	c.mu.Unlock()
	c.invalidateUnstampedConnectorObservations(conn)
	c.evidenceBarrier.RUnlock()
	// Keep every epoch-aware inbound hook on the retired Connection. Stop has
	// a bounded drain, so a decoded late frame must still reach exact-session
	// rejection (and order-lifecycle uncertainty) instead of being dropped.
	// Forget the PnL reqIDs — they belong to a now-dead Connection. The
	// daemon's post-connect setup callback re-subscribes once the next
	// handshake completes; per-position subscriptions resume lazily on
	// the next positions call.
	if c.pnl != nil {
		c.pnl.mu.Lock()
		c.pnl.accountReqID = 0
		c.pnl.accountAcct = ""
		c.pnl.accountStartedAt = time.Time{}
		c.pnl.account = AccountDailyPnL{}
		c.pnl.positionReqIDs = make(map[int]int)
		c.pnl.positionByReqID = make(map[int]int)
		c.pnl.positionSnapshot = make(map[int]PositionDailyPnL)
		c.pnl.mu.Unlock()
	}
}

// invalidateUnstampedConnectorObservations drops in-memory broker facts whose
// public identity does not carry a socket epoch. It intentionally retains the
// order lifecycle/journal correlation maps so late retired-session receipts
// can still latch uncertainty; market, contract, farm, entitlement, and
// account-derived caches must be repopulated by the successor session.
func (c *Connector) invalidateUnstampedConnectorObservations(conn *Connection) {
	if c == nil || conn == nil {
		return
	}
	c.mu.RLock()
	owned := c.conn == conn
	c.mu.RUnlock()
	if !owned {
		return
	}
	c.subMu.Lock()
	clear(c.subscriptions)
	clear(c.reqIDMap)
	c.subMu.Unlock()
	c.backendConnMu.Lock()
	c.backendConnDown = false
	c.backendConnAt = time.Time{}
	c.backendConnMu.Unlock()
	c.contractMu.Lock()
	clear(c.contractCache)
	c.contractMu.Unlock()
	c.inactiveMu.Lock()
	clear(c.inactiveSymbols)
	clear(c.inactiveCandidates)
	c.inactiveMu.Unlock()
	c.absenceMu.Lock()
	clear(c.mktDataAbsent)
	c.absenceMu.Unlock()
	c.optMu.Lock()
	clear(c.optIV)
	clear(c.optReqIDs)
	clear(c.optQuoteBid)
	clear(c.optQuoteAsk)
	clear(c.optPrevClose)
	clear(c.optGreeks)
	clear(c.optUnderlyingPx)
	c.optMu.Unlock()
	c.dataFarmMu.Lock()
	clear(c.dataFarms)
	c.dataFarmMu.Unlock()
	c.acctUpdatesMu.Lock()
	c.acctUpdatesLastAt = time.Time{}
	c.acctUpdatesAccount = ""
	c.acctUpdatesMu.Unlock()
	c.pnlResubMu.Lock()
	c.pnlResubLastAt = time.Time{}
	c.pnlResubMu.Unlock()
}

func (c *Connector) ensureHandlersRegistered(conn *Connection) {
	if c == nil || conn == nil {
		return
	}
	c.handlerRegistrationMu.Lock()
	if c.handlerRegistrations == nil {
		c.handlerRegistrations = make(map[*Connection]struct{})
	}
	if _, ok := c.handlerRegistrations[conn]; ok {
		c.handlerRegistrationMu.Unlock()
		return
	}
	// Keep the install mutex through the full fixed handler set. A concurrent
	// attach must not observe the Connection as registered and start its reader
	// while only a prefix of lifecycle handlers exists.
	c.handlerRegistrations[conn] = struct{}{}
	c.registerHandlers(conn)
	c.handlerRegistrationMu.Unlock()
}

// MarketDataTypeForSymbol returns the latest gateway data-type notice for the
// symbol's active subscription: 1=live, 2=frozen, 3=delayed,
// 4=delayed-frozen, or 0 when the subscription or notice is absent.
func (c *Connector) MarketDataTypeForSymbol(symbol string) int {
	c.subMu.RLock()
	sub, ok := c.subscriptions[strings.ToUpper(symbol)]
	c.subMu.RUnlock()
	if !ok || sub.ReqID == 0 {
		return 0
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return 0
	}
	return conn.MarketDataType(sub.ReqID)
}

// ContractDetailsLite contains the routing, identity, schedule, and price-tick
// fields decoded from a broker contract-details response. Option-specific
// fields such as Expiry, Strike, and Right are empty for non-option contracts.
type ContractDetailsLite struct {
	ReqID        int
	Symbol       string
	SecType      string
	Expiry       string
	Strike       float64
	Right        string
	Exchange     string
	PrimaryExch  string
	Currency     string
	ConID        int
	LocalSymbol  string
	TradingClass string
	Multiplier   int
	Industry     string
	Category     string
	Subcategory  string
	StockType    string
	TimeZoneID   string
	TradingHours string
	LiquidHours  string
	// MinTick is the venue's minimum price increment for the contract.
	// Zero means the gateway did not report one.
	MinTick float64
}

// ResolvedOrderContract is an unambiguous broker contract-details identity
// captured on one exact Connector session. Contract always has a positive
// ConID; MinTick is zero only when the broker omitted it.
type ResolvedOrderContract struct {
	Contract Contract
	MinTick  float64
}

// ResolveOrderContractForSession resolves a symbol/option description to one
// exact positive-ConID identity. Epoch-aware handlers and an epoch-bound
// request prevent a callback from a retired socket completing a new preview.
func (c *Connector) ResolveOrderContractForSession(ctx context.Context, binding ConnectorSessionBinding, contract Contract, timeout time.Duration) (ResolvedOrderContract, error) {
	if c == nil || !c.SessionCurrent(binding) {
		return ResolvedOrderContract{}, fmt.Errorf("broker session changed before contract resolution")
	}
	contract = normalizeMarketDataContract(contract)
	contract.Symbol = strings.ToUpper(strings.TrimSpace(contract.Symbol))
	contract.SecType = strings.ToUpper(strings.TrimSpace(contract.SecType))
	contract.Currency = strings.ToUpper(strings.TrimSpace(contract.Currency))
	if contract.SecType == "ETF" {
		// ETF is a daemon/user classification. IBKR contract-details and order
		// wire identity use STK for exchange-traded funds.
		contract.SecType = "STK"
	}
	if contract.Symbol == "" || contract.SecType == "" || contract.Currency == "" {
		return ResolvedOrderContract{}, fmt.Errorf("contract symbol, secType, and currency are required")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn := binding.connection
	reqID, err := conn.reserveRequestID(nil)
	if err != nil {
		return ResolvedOrderContract{}, err
	}
	defer conn.discardRequestIDReservation(reqID)

	detailsCh := make(chan ContractDetailsLite, 64)
	doneCh := make(chan struct{}, 1)
	overflowCh := make(chan struct{}, 1)
	serverVersion := conn.serverVersion
	dataHandlerID := conn.RegisterHandlerAtEpoch(msgContractData, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch {
			return
		}
		if detail, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *detail:
			default:
				select {
				case overflowCh <- struct{}{}:
				default:
				}
			}
		}
	})
	endHandlerID := conn.RegisterHandlerAtEpoch(msgContractDataEnd, func(fields []string, receiptEpoch uint64) {
		if receiptEpoch != binding.epoch || len(fields) < 3 {
			return
		}
		id, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
		if id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})
	defer conn.UnregisterHandler(msgContractData, dataHandlerID)
	defer conn.UnregisterHandler(msgContractDataEnd, endHandlerID)

	if err := conn.sendContractDetailsRequestForEpoch(resolveCtx, contract, reqID, binding.epoch); err != nil {
		return ResolvedOrderContract{}, err
	}
	details := make([]ContractDetailsLite, 0, 2)
	for {
		select {
		case detail := <-detailsCh:
			details = append(details, detail)
		case <-overflowCh:
			return ResolvedOrderContract{}, fmt.Errorf("contract details overflow")
		case <-doneCh:
			for {
				select {
				case detail := <-detailsCh:
					details = append(details, detail)
				case <-overflowCh:
					return ResolvedOrderContract{}, fmt.Errorf("contract details overflow")
				default:
					resolved, err := exactOrderContract(contract, details)
					if err != nil {
						return ResolvedOrderContract{}, err
					}
					if !c.SessionCurrent(binding) {
						return ResolvedOrderContract{}, fmt.Errorf("broker session changed during contract resolution")
					}
					return resolved, nil
				}
			}
		case <-resolveCtx.Done():
			return ResolvedOrderContract{}, resolveCtx.Err()
		}
	}
}

func exactOrderContract(request Contract, details []ContractDetailsLite) (ResolvedOrderContract, error) {
	request.Symbol = strings.ToUpper(strings.TrimSpace(request.Symbol))
	request.SecType = strings.ToUpper(strings.TrimSpace(request.SecType))
	request.Currency = strings.ToUpper(strings.TrimSpace(request.Currency))
	request.Exchange = strings.ToUpper(strings.TrimSpace(request.Exchange))
	request.PrimaryExch = strings.ToUpper(strings.TrimSpace(request.PrimaryExch))
	request.LocalSymbol = strings.TrimSpace(request.LocalSymbol)
	request.TradingClass = strings.TrimSpace(request.TradingClass)
	var selected *ContractDetailsLite
	for i := range details {
		detail := details[i]
		detail.Symbol = strings.ToUpper(strings.TrimSpace(detail.Symbol))
		detail.SecType = strings.ToUpper(strings.TrimSpace(detail.SecType))
		detail.Currency = strings.ToUpper(strings.TrimSpace(detail.Currency))
		detail.Exchange = strings.ToUpper(strings.TrimSpace(detail.Exchange))
		detail.PrimaryExch = strings.ToUpper(strings.TrimSpace(detail.PrimaryExch))
		detail.LocalSymbol = strings.TrimSpace(detail.LocalSymbol)
		detail.TradingClass = strings.TrimSpace(detail.TradingClass)
		detail.Expiry = strings.TrimSpace(detail.Expiry)
		detail.Right = strings.ToUpper(strings.TrimSpace(detail.Right))
		if detail.ConID <= 0 || detail.Symbol != request.Symbol || detail.Currency != request.Currency ||
			(request.ConID > 0 && detail.ConID != request.ConID) || !orderContractSecTypeMatches(request.SecType, detail.SecType) ||
			!exactOrderContractRouteMatches(request, detail) {
			continue
		}
		if request.SecType == "OPT" && (detail.Expiry != strings.TrimSpace(request.Expiry) || detail.Right != strings.ToUpper(strings.TrimSpace(request.Right)) ||
			!sameResolvedStrike(detail.Strike, request.Strike) || detail.Multiplier <= 0 || (request.Multiplier > 0 && detail.Multiplier != request.Multiplier)) {
			continue
		}
		if request.LocalSymbol != "" && !strings.EqualFold(request.LocalSymbol, detail.LocalSymbol) {
			continue
		}
		if request.TradingClass != "" && !strings.EqualFold(request.TradingClass, detail.TradingClass) {
			continue
		}
		if selected != nil {
			if selected.ConID != detail.ConID || !strings.EqualFold(selected.Exchange, detail.Exchange) ||
				!strings.EqualFold(selected.PrimaryExch, detail.PrimaryExch) || !strings.EqualFold(selected.LocalSymbol, detail.LocalSymbol) ||
				!strings.EqualFold(selected.TradingClass, detail.TradingClass) {
				return ResolvedOrderContract{}, fmt.Errorf("contract details are ambiguous")
			}
			continue
		}
		copy := detail
		selected = &copy
	}
	if selected == nil {
		return ResolvedOrderContract{}, fmt.Errorf("no exact positive-ConID contract match")
	}
	resolved := request
	resolved.ConID = selected.ConID
	// Preserve the caller's execution route. Contract-details Exchange is an
	// identity filter above, not authority to silently reroute a SMART/direct
	// order. Only fill it when the request truly omitted one.
	if resolved.Exchange == "" && selected.Exchange != "" {
		resolved.Exchange = selected.Exchange
	}
	// For options PrimaryExch is an underlying discovery hint and is commonly
	// absent from option details; preserve the requested hint. Stock/ETF
	// listing identity was strictly filtered and can be enriched here.
	if resolved.PrimaryExch == "" && selected.PrimaryExch != "" {
		resolved.PrimaryExch = selected.PrimaryExch
	}
	resolved.SecType = selected.SecType
	if selected.Multiplier > 0 {
		resolved.Multiplier = selected.Multiplier
	}
	if selected.LocalSymbol != "" {
		resolved.LocalSymbol = selected.LocalSymbol
	}
	if selected.TradingClass != "" {
		resolved.TradingClass = selected.TradingClass
	}
	return ResolvedOrderContract{Contract: resolved, MinTick: selected.MinTick}, nil
}

// exactOrderContractRouteMatches applies caller-supplied routing as an
// identity filter. SMART is an order-routing instruction, not a listing, so it
// does not by itself narrow contract-details rows. A concrete exchange or
// primary exchange must match one of the broker's route identity fields; this
// covers both SMART+PrimaryExch and direct-exchange responses. If both caller
// fields are concrete, both constraints must hold.
func exactOrderContractRouteMatches(request Contract, detail ContractDetailsLite) bool {
	reqExchange := strings.ToUpper(strings.TrimSpace(request.Exchange))
	reqPrimary := strings.ToUpper(strings.TrimSpace(request.PrimaryExch))
	detailExchange := strings.ToUpper(strings.TrimSpace(detail.Exchange))
	detailPrimary := strings.ToUpper(strings.TrimSpace(detail.PrimaryExch))
	if reqExchange != "" && reqExchange != "SMART" && reqExchange != detailExchange {
		return false
	}
	if orderContractSecTypeMatches("STK", request.SecType) && reqPrimary != "" && reqPrimary != "SMART" && reqPrimary != detailPrimary {
		return false
	}
	return true
}

func orderContractSecTypeMatches(request, broker string) bool {
	request = strings.ToUpper(strings.TrimSpace(request))
	broker = strings.ToUpper(strings.TrimSpace(broker))
	return request == broker || (request == "ETF" && broker == "STK") || (request == "STK" && broker == "ETF")
}

func sameResolvedStrike(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// ContractDetailsFirst returns the first contract-details row the gateway
// emits for contract. It does not apply option-candidate preference. A timeout
// of zero or less uses five seconds; context cancellation and send or timeout
// failures are returned to the caller.
func (c *Connector) ContractDetailsFirst(ctx context.Context, contract Contract, timeout time.Duration) (*ContractDetailsLite, error) {
	if !c.isConnected() {
		return nil, fmt.Errorf("not connected to IBKR")
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, fmt.Errorf("no active connection")
	}
	return conn.fetchContractDetailFirst(ctx, contract, timeout)
}

func mergeContractDetailsLite(base, incoming ContractDetailsLite) ContractDetailsLite {
	if base.Symbol == "" {
		base.Symbol = incoming.Symbol
	}
	if base.ConID == 0 {
		base.ConID = incoming.ConID
	}
	if base.Exchange == "" && incoming.Exchange != "" {
		base.Exchange = incoming.Exchange
	}
	if base.PrimaryExch == "" && incoming.PrimaryExch != "" {
		base.PrimaryExch = incoming.PrimaryExch
	}
	if base.Currency == "" && incoming.Currency != "" {
		base.Currency = incoming.Currency
	}
	if base.LocalSymbol == "" && incoming.LocalSymbol != "" {
		base.LocalSymbol = incoming.LocalSymbol
	}
	if base.TradingClass == "" && incoming.TradingClass != "" {
		base.TradingClass = incoming.TradingClass
	}
	if base.Industry == "" && incoming.Industry != "" {
		base.Industry = incoming.Industry
	}
	if base.Category == "" && incoming.Category != "" {
		base.Category = incoming.Category
	}
	if base.Subcategory == "" && incoming.Subcategory != "" {
		base.Subcategory = incoming.Subcategory
	}
	if base.StockType == "" && incoming.StockType != "" {
		base.StockType = incoming.StockType
	}
	if base.TimeZoneID == "" && incoming.TimeZoneID != "" {
		base.TimeZoneID = incoming.TimeZoneID
	}
	if base.TradingHours == "" && incoming.TradingHours != "" {
		base.TradingHours = incoming.TradingHours
	}
	if base.LiquidHours == "" && incoming.LiquidHours != "" {
		base.LiquidHours = incoming.LiquidHours
	}
	if base.MinTick == 0 && incoming.MinTick > 0 {
		base.MinTick = incoming.MinTick
	}
	return base
}

type inactiveSymbolState struct {
	reason   string
	markedAt time.Time
}

type inactiveCandidateState struct {
	count       int
	lastReason  string
	lastUpdated time.Time
}

// MarketDataKeyForContract returns the normalized cache and subscription key
// for an explicitly routed market-data contract. An unrouted stock uses its
// upper-case symbol only when it has no positive ConID; routed or exact
// contracts join symbol, security type, exchange, primary exchange, currency,
// local symbol, and trading class with "|", followed by CONID for exact
// identities. It returns an empty string when Symbol is empty.
func MarketDataKeyForContract(contract Contract) string {
	symbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
	if symbol == "" {
		return ""
	}
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	if secType == "" {
		secType = "STK"
	}
	exchange := strings.ToUpper(strings.TrimSpace(contract.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	currency := strings.ToUpper(strings.TrimSpace(contract.Currency))
	localSymbol := strings.ToUpper(strings.TrimSpace(contract.LocalSymbol))
	tradingClass := strings.ToUpper(strings.TrimSpace(contract.TradingClass))
	if secType == "STK" &&
		contract.ConID <= 0 &&
		exchange == "" &&
		primary == "" &&
		currency == "" &&
		localSymbol == "" &&
		tradingClass == "" {
		return symbol
	}
	parts := []string{symbol, secType, exchange, primary, currency, localSymbol, tradingClass}
	if contract.ConID > 0 {
		parts = append(parts, "CONID:"+strconv.Itoa(contract.ConID))
	}
	return strings.Join(parts, "|")
}

// DefaultMarketDataKeyForSymbol returns the same normalized route key used by
// the symbol-only subscription path after applying package routing defaults.
// It returns an empty string for a blank symbol.
func DefaultMarketDataKeyForSymbol(symbol string) string {
	upper := strings.ToUpper(strings.TrimSpace(symbol))
	if upper == "" {
		return ""
	}
	secType, exchange, currency, primary := classifySymbol(upper)
	wireSymbol := dualClassWireSymbol(upper)
	if base, _, ok := FxPair(upper); ok {
		wireSymbol = base
	}
	return MarketDataKeyForContract(Contract{
		Symbol:      wireSymbol,
		SecType:     secType,
		Exchange:    exchange,
		PrimaryExch: primary,
		Currency:    currency,
	})
}

func normalizeMarketDataContract(contract Contract) Contract {
	contract.Symbol = strings.ToUpper(strings.TrimSpace(contract.Symbol))
	contract.SecType = strings.ToUpper(strings.TrimSpace(contract.SecType))
	if contract.SecType == "" {
		contract.SecType = "STK"
	}
	contract.Exchange = strings.ToUpper(strings.TrimSpace(contract.Exchange))
	contract.PrimaryExch = strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	contract.Currency = strings.ToUpper(strings.TrimSpace(contract.Currency))
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	contract.LocalSymbol = strings.TrimSpace(contract.LocalSymbol)
	contract.TradingClass = strings.TrimSpace(contract.TradingClass)
	if contract.Exchange == "" {
		contract.Exchange = "SMART"
	}
	return contract
}

func (c *Connector) applyContractDetail(detail ContractDetailsLite, contract *Contract) bool {
	if detail.Exchange != "" {
		contract.Exchange = detail.Exchange
	}
	if detail.PrimaryExch != "" {
		contract.PrimaryExch = detail.PrimaryExch
	}
	if detail.ConID != 0 {
		contract.ConID = detail.ConID
	}
	if detail.LocalSymbol != "" {
		contract.LocalSymbol = detail.LocalSymbol
	} else if detail.ConID != 0 {
		connectorLogger.Debugf("Contract detail for %s (conID=%d) missing local symbol", detail.Symbol, detail.ConID)
	}
	if detail.TradingClass != "" {
		contract.TradingClass = detail.TradingClass
	}
	return contract.ConID != 0
}

func normalizeEquityRouting(contract *Contract, fallbackPrimary string) {
	if contract == nil || contract.SecType != "STK" {
		return
	}

	if contract.PrimaryExch == "" {
		contract.PrimaryExch = fallbackPrimary
	}
	if contract.PrimaryExch == "" && contract.Exchange != "" && !strings.EqualFold(contract.Exchange, "SMART") {
		contract.PrimaryExch = contract.Exchange
	}
	if contract.PrimaryExch != "" && strings.EqualFold(contract.PrimaryExch, "SMART") {
		contract.PrimaryExch = ""
	}
	if contract.PrimaryExch != "" {
		if strings.EqualFold(contract.Exchange, contract.PrimaryExch) || contract.Exchange == "" {
			contract.Exchange = "SMART"
		}
	}
}

func (c *Connector) prepareContract(symbol string, fetchTimeout time.Duration, asyncWarm bool) (Contract, bool) {
	start := time.Now()
	upper := strings.ToUpper(symbol)
	secType, exchange, currency, primary := classifySymbol(upper)
	localSymbol, tradingClass := contractDisplayHints(upper, secType)

	// FX pairs split the user-supplied "USD.JPY" into Symbol=USD,
	// Currency=JPY on the wire; the dotted/slash string itself is not a
	// valid IBKR symbol field. Dual-class shares (BRK.B, BF.B) get
	// translated to IBKR's space-form for the same reason — see
	// dualClassWireSymbol.
	wireSymbol := dualClassWireSymbol(upper)
	if base, _, ok := FxPair(upper); ok {
		wireSymbol = base
	}

	contract := Contract{
		Symbol:       wireSymbol,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primary,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
	}

	if reason, inactive := c.inactiveReason(upper); inactive {
		c.logDebug("Skipping contract hydration for inactive symbol %s (%s)", upper, reason)
		return contract, false
	}

	var hasDetail bool

	c.contractMu.RLock()
	if detail, ok := c.contractCache[upper]; ok {
		hasDetail = c.applyContractDetail(detail, &contract)
	}
	c.contractMu.RUnlock()

	if !hasDetail && fetchTimeout > 0 && c.conn != nil && c.conn.IsConnected() {
		fetch := c.fetchContractDetails
		if fetch == nil {
			fetch = c.FetchContractDetails
		}
		if details, err := fetch(upper, fetchTimeout); err == nil && len(details) > 0 {
			detail := details[0]
			c.contractMu.Lock()
			c.contractCache[upper] = detail
			c.contractMu.Unlock()
			hasDetail = c.applyContractDetail(detail, &contract)
		}
	}

	if !hasDetail && asyncWarm {
		go c.asyncWarmContractDetails(upper, fetchTimeout)
	}

	elapsed := time.Since(start)
	c.recordContractTiming(symbol, elapsed, hasDetail && contract.ConID != 0)
	normalizeEquityRouting(&contract, primary)

	return contract, hasDetail
}

func (c *Connector) waitForContractDetails(symbol string, base Contract, detailsReady bool) (Contract, bool) {
	upper := strings.ToUpper(symbol)
	if (detailsReady && base.ConID != 0) || base.Symbol == "" {
		return base, detailsReady || base.ConID != 0
	}
	deadline := time.Now().Add(contractHydrationWait)
	contract := base
	for contract.ConID == 0 && time.Now().Before(deadline) {
		time.Sleep(contractHydrationPoll)
		c.contractMu.RLock()
		detail, ok := c.contractCache[upper]
		c.contractMu.RUnlock()
		if !ok {
			continue
		}
		contractCopy := contract
		if c.applyContractDetail(detail, &contractCopy) && contractCopy.ConID != 0 {
			normalizeEquityRouting(&contractCopy, contract.PrimaryExch)
			return contractCopy, true
		}
	}
	return contract, detailsReady || contract.ConID != 0
}

func (c *Connector) asyncWarmContractDetails(symbol string, timeout time.Duration) {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if details, err := c.FetchContractDetails(symbol, timeout); err == nil && len(details) > 0 {
		c.contractMu.Lock()
		c.contractCache[symbol] = details[0]
		c.contractMu.Unlock()
		c.clearInactiveCandidate(symbol)
		c.logInfo("Cached contract details for %s (PrimaryExch=%s)", symbol, details[0].PrimaryExch)
	}
}

const (
	minServerVerMdSizeMultiplier = 110
	minServerVerAggGroup         = 121
	minServerVerUnderlyingInfo   = 122
	minServerVerMarketRules      = 126
	minServerVerRealExpiration   = 134
	minServerVerStockType        = 152
	minServerVerFractionalSize   = 163
	minServerVerSizeRules        = 164
	minServerVerFundDataFields   = 179
	minServerVerLastTradeDate    = 182
	minServerVerIneligibility    = 186
)

// SeedContractDetails adds a caller-supplied contract to the Connector's
// in-memory cache when symbol is non-empty, detail has a non-zero ConID, and no
// resolved entry is already cached for that symbol. It never replaces a live
// resolved entry and performs no broker request. The result reports whether the
// seed was applied.
func (c *Connector) SeedContractDetails(symbol string, detail ContractDetailsLite) bool {
	if symbol == "" || detail.ConID == 0 {
		return false
	}
	key := strings.ToUpper(strings.TrimSpace(symbol))
	c.contractMu.Lock()
	defer c.contractMu.Unlock()
	if existing, ok := c.contractCache[key]; ok && existing.ConID != 0 {
		return false
	}
	c.contractCache[key] = detail
	return true
}

// FetchContractDetails returns cached contract details for symbol when a
// resolved entry exists; otherwise it requests all matching rows and waits for
// the broker's completion marker. Calls must be serialized because the request
// uses shared response handlers. On timeout it returns any rows already
// received together with [ErrContractDetailsTimeout].
func (c *Connector) FetchContractDetails(symbol string, timeout time.Duration) ([]ContractDetailsLite, error) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		c.logDebug("Contract details fetch skipped for inactive symbol %s", symbol)
		return nil, ErrSymbolInactive
	}
	if cached := c.cachedContractDetail(symbol); cached != nil && cached.ConID != 0 {
		c.logDebug("Contract details fetch satisfied from cache symbol=%s conID=%d", symbol, cached.ConID)
		return []ContractDetailsLite{*cached}, nil
	}
	if !c.isConnected() {
		return nil, fmt.Errorf("IBKR connection not available")
	}
	// Prepare contract using the same classification as market data.
	// Dual-class shares (BRK.B, BF.B) translate to IBKR's space-form
	// before going on the wire — see dualClassWireSymbol.
	secType, exchange, currency, primary := classifySymbol(symbol)
	wireSymbol := dualClassWireSymbol(symbol)
	if base, _, ok := FxPair(symbol); ok {
		wireSymbol = base
	}
	contract := Contract{Symbol: wireSymbol, SecType: secType, Exchange: exchange, Currency: currency}
	if primary != "" {
		contract.PrimaryExch = primary
	}
	detailsCh := make(chan ContractDetailsLite, 10)
	doneCh := make(chan struct{})
	serverVersion := c.conn.serverVersion
	reqID, err := c.conn.nextRequestID()
	if err != nil {
		return nil, err
	}

	c.logDebug("Contract details fetch start reqID=%d symbol=%s secType=%s exch=%s primary=%s currency=%s", reqID, symbol, contract.SecType, contract.Exchange, contract.PrimaryExch, contract.Currency)

	// Register temporary handlers
	dataHandlerID := c.conn.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			detailsCh <- *lite
		}
	})

	endHandlerID := c.conn.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		rid, _ := strconv.Atoi(safeGet(fields, 2))
		if rid == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})

	req, releaseReq := c.registerContractDetailsRequest(reqID, resolutionKeyForSecType(symbol, secType))
	defer releaseReq()

	if err := c.conn.sendContractDetailsRequest(contract, reqID); err != nil {
		c.conn.UnregisterHandler(msgContractData, dataHandlerID)
		c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
		return nil, err
	}

	// Wait for completion
	var results []ContractDetailsLite
	deadline := time.After(timeout)
	for {
		select {
		case d := <-detailsCh:
			results = append(results, d)
		case <-doneCh:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			if len(results) == 0 {
				c.logDebug("Contract details fetch complete reqID=%d symbol=%s (0 rows)", reqID, symbol)
			} else {
				c.clearInactiveCandidate(symbol)
				first := results[0]
				// Populate the cache so callers that discard the
				// returned slice (e.g. the daemon's prewarm
				// goroutine) still warm contractCache for the
				// next prepareContract / ensureContractDetails
				// lookup. The guard matches deferContractDetailsCleanup:
				// don't clobber an already-resolved entry that
				// another path may have raced to populate.
				c.contractMu.Lock()
				if existing, ok := c.contractCache[symbol]; !ok || existing.ConID == 0 {
					c.contractCache[symbol] = first
				}
				c.contractMu.Unlock()
				c.logDebug("Contract details fetch success reqID=%d symbol=%s count=%d conID=%d exch=%s primary=%s local=%s class=%s",
					reqID, symbol, len(results), first.ConID, first.Exchange, first.PrimaryExch, first.LocalSymbol, first.TradingClass)
			}
			return results, nil
		case err := <-req.fail:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			c.logDebug("Contract details fetch rejected reqID=%d symbol=%s received=%d: %v", reqID, symbol, len(results), err)
			return results, err
		case <-deadline:
			c.deferContractDetailsCleanup(symbol, reqID, detailsCh, doneCh, dataHandlerID, endHandlerID)
			c.logDebug("Contract details fetch timeout reqID=%d symbol=%s received=%d", reqID, symbol, len(results))
			return results, ErrContractDetailsTimeout
		}
	}
}

// resolutionKeyForSecType returns the key a definition rejection for this
// request should count against, or "" for derivative probes whose rejections
// are routine rather than evidence: option fan-outs request strike supersets
// where some triples are unlisted, and FX resolution quotes both pair
// directions when only one is listed.
func resolutionKeyForSecType(key, secType string) string {
	switch strings.ToUpper(strings.TrimSpace(secType)) {
	case "OPT", "FOP", "WAR", "BAG", "CASH":
		return ""
	}
	return key
}

func (c *Connector) fetchContractDetailsForContract(contract Contract, timeout time.Duration) ([]ContractDetailsLite, error) {
	contract = normalizeMarketDataContract(contract)
	if contract.Symbol == "" {
		return nil, fmt.Errorf("contract symbol is required")
	}
	key := MarketDataKeyForContract(contract)
	if _, inactive := c.inactiveReason(key); inactive {
		c.logDebug("Contract details fetch skipped for inactive routed contract %s", key)
		return nil, ErrSymbolInactive
	}
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	if !c.isConnected() {
		return nil, fmt.Errorf("IBKR connection not available")
	}

	lookup := contract
	if lookup.SecType == "STK" && lookup.ConID == 0 && lookup.PrimaryExch != "" &&
		(lookup.Exchange == "" || strings.EqualFold(lookup.Exchange, "SMART")) {
		lookup.Exchange = lookup.PrimaryExch
	}

	detailsCh := make(chan ContractDetailsLite, 10)
	doneCh := make(chan struct{})
	serverVersion := c.conn.serverVersion
	reqID, err := c.conn.nextRequestID()
	if err != nil {
		return nil, err
	}

	c.logDebug("Routed contract details fetch start reqID=%d key=%s secType=%s exch=%s primary=%s currency=%s",
		reqID, key, lookup.SecType, lookup.Exchange, lookup.PrimaryExch, lookup.Currency)

	dataHandlerID := c.conn.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			detailsCh <- *lite
		}
	})

	endHandlerID := c.conn.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		rid, _ := strconv.Atoi(safeGet(fields, 2))
		if rid == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})

	req, releaseReq := c.registerContractDetailsRequest(reqID, resolutionKeyForSecType(key, contract.SecType))
	defer releaseReq()

	if err := c.conn.sendContractDetailsRequest(lookup, reqID); err != nil {
		c.conn.UnregisterHandler(msgContractData, dataHandlerID)
		c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
		return nil, err
	}

	var results []ContractDetailsLite
	deadline := time.After(timeout)
	for {
		select {
		case d := <-detailsCh:
			results = append(results, d)
		case <-doneCh:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			if len(results) > 0 {
				c.clearInactiveCandidate(key)
			}
			c.logDebug("Routed contract details fetch complete reqID=%d key=%s count=%d", reqID, key, len(results))
			return results, nil
		case err := <-req.fail:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			c.logDebug("Routed contract details fetch rejected reqID=%d key=%s received=%d: %v", reqID, key, len(results), err)
			return results, err
		case <-deadline:
			c.conn.UnregisterHandler(msgContractData, dataHandlerID)
			c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
			c.logDebug("Routed contract details fetch timeout reqID=%d key=%s received=%d", reqID, key, len(results))
			return results, ErrContractDetailsTimeout
		}
	}
}

// contractDetailsLateGrace is how long the deferred cleanup goroutine
// keeps listening for ContractData frames after FetchContractDetails has
// already timed out and returned to its caller. A late frame within this
// window still populates the cache so the next prepareContract /
// ensureContractDetails call hits a warm entry without a fresh
// round-trip.
//
// Bumped from 3 s to 30 s after observing TWS gateways that respond to
// reqContractDetails between 15-45 s post-request — especially for thin
// CBOE indices (VIX3M) and FX (USD.JPY) in the first hour after a TWS
// cold start. Under 3 s of grace those late frames were always lost,
// which kept the regime row's hyg_50dma / weekly_change_pct fields
// missing for the entire daemon lifetime even though the underlying
// data was on its way. 30 s is short enough that a permanently
// unresponsive gateway still surfaces the failure within one regime
// interval, and the goroutine's handler-registration footprint is
// bounded by the number of distinct symbols (a handful).
const contractDetailsLateGrace = 30 * time.Second

func (c *Connector) deferContractDetailsCleanup(symbol string, reqID int, detailsCh <-chan ContractDetailsLite, doneCh <-chan struct{}, dataHandlerID, endHandlerID uint64) {
	go func() {
		timer := time.NewTimer(contractDetailsLateGrace)
		defer timer.Stop()

		var cachedDetail *ContractDetailsLite

	forLoop:
		for {
			select {
			case detail := <-detailsCh:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(contractDetailsLateGrace)

				if detail.Symbol != "" {
					d := detail // copy
					cachedDetail = &d
					key := strings.ToUpper(detail.Symbol)
					c.contractMu.Lock()
					if existing, ok := c.contractCache[key]; !ok || existing.ConID == 0 {
						c.contractCache[key] = detail
					}
					c.contractMu.Unlock()
				}
			case <-doneCh:
				break forLoop
			case <-timer.C:
				break forLoop
			}
		}

		c.conn.UnregisterHandler(msgContractData, dataHandlerID)
		c.conn.UnregisterHandler(msgContractDataEnd, endHandlerID)

		if cachedDetail != nil {
			connectorLogger.Infof("[INFO] Contract details for %s arrived after timeout (reqID=%d, conID=%d)", symbol, reqID, cachedDetail.ConID)
		}

		for {
			select {
			case <-detailsCh:
			default:
				return
			}
		}
	}()
}

func (c *Connector) ensureContractDetails(symbol string, timeout time.Duration) (*ContractDetailsLite, error) {
	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, ErrSymbolInactive
	}

	c.contractMu.RLock()
	if cached, ok := c.contractCache[symbol]; ok && cached.ConID != 0 {
		c.contractMu.RUnlock()
		return &cached, nil
	}
	c.contractMu.RUnlock()

	fetch := c.fetchContractDetails
	if fetch == nil {
		fetch = c.FetchContractDetails
	}
	details, err := fetch(symbol, timeout)
	if err != nil {
		return nil, err
	}
	if len(details) == 0 {
		return nil, fmt.Errorf("contract details unavailable for %s", symbol)
	}
	primary := details[0]
	c.contractMu.Lock()
	c.contractCache[symbol] = primary
	c.contractMu.Unlock()
	return &primary, nil
}

func (c *Connector) cachedContractDetail(symbol string) *ContractDetailsLite {
	symbol = strings.ToUpper(symbol)
	c.contractMu.RLock()
	defer c.contractMu.RUnlock()
	if detail, ok := c.contractCache[symbol]; ok {
		d := detail
		return &d
	}
	return nil
}

func (c *Connector) awaitContractDetail(symbol string, wait time.Duration) *ContractDetailsLite {
	return c.awaitContractDetailCtx(context.Background(), symbol, wait)
}

func (c *Connector) awaitContractDetailCtx(ctx context.Context, symbol string, wait time.Duration) *ContractDetailsLite {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(contractHydrationPoll)
	defer ticker.Stop()
	for {
		if detail := c.cachedContractDetail(symbol); detail != nil && detail.ConID != 0 {
			return detail
		}
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

func historicalTimeoutWithinContext(ctx context.Context, timeout time.Duration) (time.Duration, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return 0, context.DeadlineExceeded
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return timeout, nil
}

func safeGet(a []string, i int) string {
	if i >= 0 && i < len(a) {
		return a[i]
	}
	return ""
}

func parseIntSafe(s string) int {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}

// normalizeWireLastTradeDate reduces a lastTradeDateOrContractMonth wire value
// to its leading date token. The gateway appends the settlement time and zone
// for options ("20260729 15:00:00 US/Central"); the official IB decoders split
// on whitespace or hyphen and keep only the first token, and everything
// downstream — optionContractKey, the persisted contract cache, and OPT
// order-contract matching — expects the bare YYYYMMDD.
func normalizeWireLastTradeDate(raw string) string {
	raw = strings.TrimSpace(raw)
	if i := strings.IndexAny(raw, " -"); i >= 0 {
		return raw[:i]
	}
	return raw
}

func parseContractDetailsLite(fields []string, expectedReqID int, serverVersion int) (*ContractDetailsLite, bool) {
	if len(fields) <= 1 {
		return nil, false
	}

	idx := 1
	version := 8
	if serverVersion < minServerVerSizeRules {
		version = parseIntSafe(safeGet(fields, idx))
		idx++
	}

	reqID := expectedReqID
	if version >= 3 {
		parsedReqID := parseIntSafe(safeGet(fields, idx))
		idx++
		if parsedReqID != 0 {
			reqID = parsedReqID
		}
	}
	if expectedReqID != 0 && reqID != expectedReqID {
		return nil, false
	}

	symbol := strings.TrimSpace(safeGet(fields, idx))
	idx++
	secType := strings.TrimSpace(safeGet(fields, idx))
	idx++

	// Last trade date / contract month — for OPT this is the expiry YYYYMMDD.
	expiry := normalizeWireLastTradeDate(safeGet(fields, idx))
	idx++
	if serverVersion >= minServerVerLastTradeDate {
		idx++
	}

	// Strike and right.
	strikeStr := safeGet(fields, idx)
	idx++
	right := strings.TrimSpace(safeGet(fields, idx))
	idx++
	strike := 0.0
	if v, err := strconv.ParseFloat(strikeStr, 64); err == nil {
		strike = v
	}

	exchange := strings.TrimSpace(safeGet(fields, idx))
	idx++
	currency := strings.TrimSpace(safeGet(fields, idx))
	idx++

	localSymbol := strings.TrimSpace(safeGet(fields, idx))
	idx++
	_ = safeGet(fields, idx) // market name
	idx++
	tradingClass := strings.TrimSpace(safeGet(fields, idx))
	idx++

	conID := parseIntSafe(safeGet(fields, idx))
	idx++
	minTick := 0.0
	if v, err := strconv.ParseFloat(strings.TrimSpace(safeGet(fields, idx)), 64); err == nil && v > 0 {
		minTick = v
	}
	idx++

	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		idx++ // md size multiplier (deprecated)
	}

	multiplier := parseIntSafe(safeGet(fields, idx))
	idx++
	_ = safeGet(fields, idx) // order types
	idx++
	_ = safeGet(fields, idx) // valid exchanges
	idx++

	if version >= 2 {
		idx++ // price magnifier
	}
	if version >= 4 {
		idx++ // underConId
	}

	primaryExch := ""
	if version >= 5 {
		idx++ // long name
		primaryExch = strings.TrimSpace(safeGet(fields, idx))
		idx++
	}

	timeZoneID := ""
	tradingHours := ""
	liquidHours := ""
	if version >= 6 {
		idx++                    // contractMonth
		_ = safeGet(fields, idx) // industry: strict full-frame decoder owns classification
		idx++
		_ = safeGet(fields, idx) // category: strict full-frame decoder owns classification
		idx++
		_ = safeGet(fields, idx) // subcategory: strict full-frame decoder owns classification
		idx++
		timeZoneID = safeGet(fields, idx)
		idx++
		tradingHours = safeGet(fields, idx)
		idx++
		liquidHours = safeGet(fields, idx)
		idx++
	}

	industry, category, subcategory, stockType := "", "", "", ""
	if classification, ok := parseContractDetailsClassification(fields, serverVersion); ok {
		industry = classification.industry
		category = classification.category
		subcategory = classification.subcategory
		stockType = classification.stockType
	}

	return &ContractDetailsLite{
		ReqID:        reqID,
		Symbol:       symbol,
		SecType:      secType,
		Expiry:       expiry,
		Strike:       strike,
		Right:        right,
		Exchange:     exchange,
		PrimaryExch:  primaryExch,
		Currency:     currency,
		ConID:        conID,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClass,
		Multiplier:   multiplier,
		Industry:     industry,
		Category:     category,
		Subcategory:  subcategory,
		StockType:    stockType,
		TimeZoneID:   timeZoneID,
		TradingHours: tradingHours,
		LiquidHours:  liquidHours,
		MinTick:      minTick,
	}, true
}

type contractDetailsClassification struct {
	industry    string
	category    string
	subcategory string
	stockType   string
}

// parseContractDetailsClassification follows the official contractData
// decoder from the message ID through every field after StockType. The
// classification tuple is authoritative only when the entire versioned frame
// is present, numeric/count/boolean fields have their declared wire types, and
// no unconsumed field remains other than the encoding's terminal empty field.
// The general contract parser remains tolerant so old partial fixtures can
// still supply routing fields, but a partial or shifted frame can never supply
// issuer classification.
func parseContractDetailsClassification(fields []string, serverVersion int) (contractDetailsClassification, bool) {
	cursor := contractDetailsWireCursor{fields: fields, ok: true}
	messageID, ok := cursor.integer()
	if !ok || messageID != msgContractData {
		return contractDetailsClassification{}, false
	}

	version := 8
	if serverVersion < minServerVerSizeRules {
		version, ok = cursor.integer()
		if !ok || version < 1 || version > 8 {
			return contractDetailsClassification{}, false
		}
	}
	if version >= 3 {
		if _, ok = cursor.integer(); !ok { // reqID
			return contractDetailsClassification{}, false
		}
	}

	_ = cursor.string() // symbol
	secType := cursor.string()
	_ = cursor.string() // lastTradeDateOrContractMonth
	if serverVersion >= minServerVerLastTradeDate {
		_ = cursor.string() // lastTradeDate
	}
	if !cursor.number() { // strike
		return contractDetailsClassification{}, false
	}
	_ = cursor.string()                // right
	_ = cursor.string()                // exchange
	_ = cursor.string()                // currency
	_ = cursor.string()                // localSymbol
	_ = cursor.string()                // marketName
	_ = cursor.string()                // tradingClass
	if _, ok = cursor.integer(); !ok { // conID
		return contractDetailsClassification{}, false
	}
	if !cursor.number() { // minTick
		return contractDetailsClassification{}, false
	}
	if serverVersion >= minServerVerMdSizeMultiplier && serverVersion < minServerVerSizeRules {
		if _, ok = cursor.integer(); !ok { // deprecated mdSizeMultiplier
			return contractDetailsClassification{}, false
		}
	}
	_ = cursor.string() // multiplier is a protocol string
	_ = cursor.string() // orderTypes
	_ = cursor.string() // validExchanges
	if version >= 2 {
		if _, ok = cursor.integer(); !ok { // priceMagnifier
			return contractDetailsClassification{}, false
		}
	}
	if version >= 4 {
		if _, ok = cursor.integer(); !ok { // underConID
			return contractDetailsClassification{}, false
		}
	}
	if version >= 5 {
		_ = cursor.string() // longName
		_ = cursor.string() // primaryExchange
	}

	classification := contractDetailsClassification{}
	if version >= 6 {
		_ = cursor.string() // contractMonth
		classification.industry = strings.TrimSpace(cursor.string())
		classification.category = strings.TrimSpace(cursor.string())
		classification.subcategory = strings.TrimSpace(cursor.string())
		_ = cursor.string() // timeZoneID
		_ = cursor.string() // tradingHours
		_ = cursor.string() // liquidHours
	}
	if version >= 8 {
		_ = cursor.string()   // evRule
		if !cursor.number() { // evMultiplier is double in current Java/C++ clients
			return contractDetailsClassification{}, false
		}
	}
	if version >= 7 && !cursor.stringPairs() { // secIdList
		return contractDetailsClassification{}, false
	}
	if serverVersion >= minServerVerAggGroup {
		if _, ok = cursor.integer(); !ok {
			return contractDetailsClassification{}, false
		}
	}
	if serverVersion >= minServerVerUnderlyingInfo {
		_ = cursor.string() // underSymbol
		_ = cursor.string() // underSecType
	}
	if serverVersion >= minServerVerMarketRules {
		_ = cursor.string() // marketRuleIDs
	}
	if serverVersion >= minServerVerRealExpiration {
		_ = cursor.string() // realExpirationDate
	}
	if serverVersion >= minServerVerStockType {
		classification.stockType = strings.TrimSpace(cursor.string())
	}
	if serverVersion >= minServerVerFractionalSize && serverVersion < minServerVerSizeRules {
		if !cursor.number() { // deprecated sizeMinTick
			return contractDetailsClassification{}, false
		}
	}
	if serverVersion >= minServerVerSizeRules {
		for range 3 { // minSize, sizeIncrement, suggestedSizeIncrement
			if !cursor.number() {
				return contractDetailsClassification{}, false
			}
		}
	}
	if serverVersion >= minServerVerFundDataFields && strings.EqualFold(strings.TrimSpace(secType), "FUND") {
		for range 7 { // name through management fee
			_ = cursor.string()
		}
		for range 3 { // closed flags
			if !cursor.boolean() {
				return contractDetailsClassification{}, false
			}
		}
		for range 7 { // amount through asset type
			_ = cursor.string()
		}
	}
	if serverVersion >= minServerVerIneligibility && !cursor.stringPairs() {
		return contractDetailsClassification{}, false
	}
	if !cursor.complete() {
		return contractDetailsClassification{}, false
	}
	return classification, true
}

type contractDetailsWireCursor struct {
	fields []string
	idx    int
	ok     bool
}

func (c *contractDetailsWireCursor) string() string {
	if !c.ok || c.idx >= len(c.fields) {
		c.ok = false
		return ""
	}
	value := c.fields[c.idx]
	c.idx++
	return value
}

func (c *contractDetailsWireCursor) integer() (int, bool) {
	value := strings.TrimSpace(c.string())
	if !c.ok {
		return 0, false
	}
	if value == "" {
		return 0, true
	}
	parsed, err := strconv.Atoi(value)
	return parsed, err == nil
}

func (c *contractDetailsWireCursor) number() bool {
	value := strings.TrimSpace(c.string())
	if !c.ok || value == "" {
		return c.ok
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func (c *contractDetailsWireCursor) boolean() bool {
	value := strings.TrimSpace(c.string())
	return c.ok && (value == "" || value == "0" || value == "1")
}

func (c *contractDetailsWireCursor) stringPairs() bool {
	count, ok := c.integer()
	if !ok || count < 0 || count > (len(c.fields)-c.idx)/2 {
		c.ok = false
		return false
	}
	for range count {
		_ = c.string()
		_ = c.string()
	}
	return c.ok
}

func (c *contractDetailsWireCursor) complete() bool {
	if !c.ok || c.idx == len(c.fields) {
		return c.ok
	}
	return c.idx+1 == len(c.fields) && c.fields[c.idx] == ""
}

// Stop marks the Connector stopped, cancels its P&L subscriptions, and closes
// the broker connection. It is idempotent. The Connector remains a valid value
// after Stop; later method calls report unavailable state or no-op as defined by
// each method.
func (c *Connector) Stop() error {
	// Drop c.mu BEFORE calling into conn.Disconnect — that path fires the
	// registered onDisconnect callback (attachConnectionHooks.func2), which
	// calls back into onConnectionLost and tries to acquire c.mu. Holding
	// mu across the callback would deadlock the shutdown path, hanging the
	// daemon process after idle timeout / SIGTERM.
	//
	// Marking running=false before releasing the lock is the right boundary:
	// any reentrant caller that re-checks c.running mid-shutdown sees a
	// stopped connector and no-ops.
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}
	conn := c.conn
	c.running = false
	c.mu.Unlock()

	// Debug, not Info: the daemon calls Stop on every reconnect-cycle teardown
	// while the gateway is down (see the demotion note in Start).
	c.logDebug("Stopping IBKR connector")

	// Cancel any live PnL subscriptions before the connection drops.
	// Best-effort: the gateway drops subscriptions on socket close anyway,
	// but explicit cancels keep the gateway-side reqID slot count tidy
	// across daemon restarts.
	c.cancelAllPnL()

	if conn != nil {
		if err := conn.Disconnect(); err != nil {
			c.logWarn("Error disconnecting: %v", err)
		}
	}

	c.logDebug("IBKR connector stopped")

	return nil
}

// sharedGenericTicks is the wire generic-tick set requested for shared
// streaming subscriptions (OI, vol/IV, misc stats, mark price, RTVolume,
// shortable).
const sharedGenericTicks = "100,101,104,106,165,221,233,236"

// SubscribeMarketData ensures a symbol-keyed streaming subscription exists.
// Repeating the call for the same normalized symbol is a no-op, including from
// concurrent callers. ctx must be non-nil and bounds acquisition of a
// market-data slot. fields is retained as subscription metadata; the wire tick
// set is selected by the Connector. The slice is not copied and must not be
// mutated while subscribed. When disconnected, the method records a local
// subscription with no live request. Use [Connector.UnsubscribeMarketData] for
// cleanup.
func (c *Connector) SubscribeMarketData(ctx context.Context, symbol string, fields []string) error {
	symbol = strings.ToUpper(symbol)
	if reason, inactive := c.inactiveReason(symbol); inactive {
		if reason == "" {
			reason = "inactive"
		}
		c.logDebug("Skipping SubscribeMarketData for inactive symbol %s (%s)", symbol, reason)
		return ErrSymbolInactive
	}
	defaultRouteKey := DefaultMarketDataKeyForSymbol(symbol)
	if defaultRouteKey != "" && defaultRouteKey != symbol {
		if reason, inactive := c.inactiveReason(defaultRouteKey); inactive {
			if reason == "" {
				reason = "inactive"
			}
			c.logDebug("Skipping SubscribeMarketData for inactive route %s (%s)", defaultRouteKey, reason)
			return ErrSymbolInactive
		}
	}
	if absErr := c.marketDataAbsenceFor(symbol); absErr != nil {
		c.logDebug("Skipping SubscribeMarketData for %s (%v)", symbol, absErr)
		return absErr
	}
	c.subMu.RLock()
	if sub, exists := c.subscriptions[symbol]; exists {
		c.subMu.RUnlock()
		marketDataLogger.Debugf("%s: SubscribeMarketData(%s) is a no-op; existing subscription reqID=%d", c.name, symbol, sub.ReqID)
		return nil
	}
	c.subMu.RUnlock()

	reqID := 0
	var spec *mdReplaySpec
	if c.conn != nil && c.conn.IsConnected() {
		contract, ready := c.prepareContract(symbol, 2*time.Second, true)
		contract, ready = c.waitForContractDetails(symbol, contract, ready)

		var err error
		switch {
		case ready:
			reqID, err = c.conn.RequestMarketDataWithContract(ctx, contract, sharedGenericTicks, false, false)
			spec = &mdReplaySpec{contract: contract, genericTicks: sharedGenericTicks}
		case contract.PrimaryExch != "":
			reqID, err = c.conn.RequestMarketDataWithPrimary(ctx, symbol, contract.PrimaryExch)
			spec = &mdReplaySpec{symbol: symbol, primaryExch: contract.PrimaryExch}
		default:
			reqID, err = c.conn.RequestMarketData(ctx, symbol)
			spec = &mdReplaySpec{symbol: symbol}
		}
		if err != nil {
			c.logWarn("Failed to request market data for %s: %v", symbol, err)
			reqID = 0
			spec = nil
		}
	}

	c.subMu.Lock()

	// Race protection: another goroutine may have raced past the first
	// idempotency check. If we issued a reqID to IBKR, cancel it so we
	// don't leak a gateway-side subscription — but release subMu first
	// so the cancel's rate-limited socket write doesn't block every
	// other subscription reader.
	if _, exists := c.subscriptions[symbol]; exists {
		raceReqID := reqID
		conn := c.conn
		c.subMu.Unlock()
		if raceReqID != 0 && conn != nil && conn.IsConnected() {
			_ = conn.CancelMarketData(raceReqID)
		}
		marketDataLogger.Debugf("%s: SubscribeMarketData(%s) raced; reusing existing subscription", c.name, symbol)
		return nil
	}

	if reqID != 0 {
		c.reqIDMap[reqID] = symbol
	}

	c.subscriptions[symbol] = &Subscription{
		Symbol:     symbol,
		ReqID:      reqID,
		Fields:     fields,
		LastTime:   time.Now(),
		RejectCh:   make(chan SubscriptionRejection, 1),
		replaySpec: spec,
	}
	c.subMu.Unlock()

	marketDataLogger.Debugf("%s: Subscribed to market data for %s (ReqID: %d)", c.name, symbol, reqID)

	return nil
}

// SubscribeMarketDataWithContract ensures a streaming subscription exists for
// an explicitly routed contract and returns its [MarketDataKeyForContract] key.
// Repeating the same route is a no-op. ctx must be non-nil and bounds slot
// acquisition. fields is retained as metadata; the Connector selects the wire
// tick set. The slice is not copied and must not be mutated while subscribed.
// When disconnected, it records a local subscription with ReqID zero.
func (c *Connector) SubscribeMarketDataWithContract(ctx context.Context, contract Contract, fields []string) (string, error) {
	contract = normalizeMarketDataContract(contract)
	contract = c.hydrateExplicitMarketDataContract(contract)
	key := MarketDataKeyForContract(contract)
	if key == "" {
		return "", fmt.Errorf("contract symbol is required for market data")
	}
	if reason, inactive := c.inactiveReason(key); inactive {
		if reason == "" {
			reason = "inactive"
		}
		c.logDebug("Skipping routed SubscribeMarketData for %s (%s)", key, reason)
		return key, ErrSymbolInactive
	}
	if absErr := c.marketDataAbsenceFor(key); absErr != nil {
		c.logDebug("Skipping routed SubscribeMarketData for %s (%v)", key, absErr)
		return key, absErr
	}

	c.subMu.RLock()
	if sub, exists := c.subscriptions[key]; exists {
		c.subMu.RUnlock()
		marketDataLogger.Debugf("%s: SubscribeMarketDataWithContract(%s) is a no-op; existing subscription reqID=%d", c.name, key, sub.ReqID)
		return key, nil
	}
	c.subMu.RUnlock()

	reqID := 0
	if c.conn != nil && c.conn.IsConnected() {
		var err error
		reqID, err = c.conn.RequestMarketDataWithContract(ctx, contract, sharedGenericTicks, false, false)
		if err != nil {
			c.logWarn("Failed to request market data for %s: %v", key, err)
			return key, err
		}
	}

	c.subMu.Lock()
	if _, exists := c.subscriptions[key]; exists {
		raceReqID := reqID
		conn := c.conn
		c.subMu.Unlock()
		if raceReqID != 0 && conn != nil && conn.IsConnected() {
			_ = conn.CancelMarketData(raceReqID)
		}
		marketDataLogger.Debugf("%s: SubscribeMarketDataWithContract(%s) raced; reusing existing subscription", c.name, key)
		return key, nil
	}
	if reqID != 0 {
		c.reqIDMap[reqID] = key
	}
	c.subscriptions[key] = &Subscription{
		Symbol:     key,
		ReqID:      reqID,
		Fields:     fields,
		LastTime:   time.Now(),
		RejectCh:   make(chan SubscriptionRejection, 1),
		replaySpec: &mdReplaySpec{contract: contract, genericTicks: sharedGenericTicks},
	}
	c.subMu.Unlock()

	marketDataLogger.Debugf("%s: Subscribed to routed market data for %s (ReqID: %d)", c.name, key, reqID)
	return key, nil
}

// SubscribeMarketDataWithContractForSession creates a short-lived,
// non-sharing subscription for one exact positive-ConID contract, or one
// explicit CASH/IDEALPRO currency pair, on binding's physical socket. The
// unique key prevents a symbol/route cache entry from a different contract or
// socket satisfying broker-write evidence.
func (c *Connector) SubscribeMarketDataWithContractForSession(ctx context.Context, binding ConnectorSessionBinding, contract Contract, fields []string) (string, error) {
	if c == nil || !c.SessionCurrent(binding) {
		return "", fmt.Errorf("broker session changed before exact quote request")
	}
	contract = normalizeMarketDataContract(contract)
	if contract.ConID <= 0 && !isExplicitSessionFXContract(contract) {
		return "", fmt.Errorf("exact quote contract requires positive ConID or explicit CASH/IDEALPRO pair")
	}
	baseKey := MarketDataKeyForContract(contract)
	if baseKey == "" {
		return "", fmt.Errorf("contract symbol is required for market data")
	}
	key := baseKey + "|EXACT:" + strconv.FormatUint(c.exactQuoteSeq.Add(1), 10)
	conn := binding.connection
	cleanupPrepared := func(reqID int) {
		c.subMu.Lock()
		if sub := c.subscriptions[key]; sub != nil && sub.ReqID == reqID && sub.SessionEpoch == binding.epoch {
			delete(c.subscriptions, key)
		}
		if c.reqIDMap[reqID] == key {
			delete(c.reqIDMap, reqID)
		}
		c.subMu.Unlock()
	}
	wireContract := contract
	normalizeResolvedOptionMarketDataContract(&wireContract)
	reqID, err := conn.requestMarketDataWithContractForEpoch(ctx, wireContract, OptionSubscriptionGenericTicks+",165,221,233,236", false, false, binding.epoch, func(reqID int) func() {
		c.subMu.Lock()
		c.reqIDMap[reqID] = key
		c.subscriptions[key] = &Subscription{
			Symbol: key, ReqID: reqID, Fields: fields, LastTime: time.Now(), SessionEpoch: binding.epoch,
			RejectCh: make(chan SubscriptionRejection, 1),
		}
		c.subMu.Unlock()
		return func() { cleanupPrepared(reqID) }
	})
	if err != nil {
		return "", err
	}
	if !c.SessionCurrent(binding) {
		cleanupPrepared(reqID)
		binding.connection.releaseMarketDataSlotAtEpoch(reqID, binding.epoch)
		return "", fmt.Errorf("broker session changed during exact quote request")
	}
	return key, nil
}

func isExplicitSessionFXContract(contract Contract) bool {
	return contract.ConID == 0 &&
		strings.EqualFold(strings.TrimSpace(contract.SecType), "CASH") &&
		strings.EqualFold(strings.TrimSpace(contract.Exchange), "IDEALPRO") &&
		strings.TrimSpace(contract.Symbol) != "" &&
		strings.TrimSpace(contract.Currency) != "" &&
		!strings.EqualFold(strings.TrimSpace(contract.Symbol), strings.TrimSpace(contract.Currency))
}

// UnsubscribeMarketDataForSession removes only the exact subscription created
// on binding and emits a cancel solely when that physical socket is still
// current. A retired cleanup can never cancel a successor subscription.
func (c *Connector) UnsubscribeMarketDataForSession(ctx context.Context, binding ConnectorSessionBinding, key string) error {
	c.subMu.Lock()
	sub := c.subscriptions[key]
	if sub == nil || sub.SessionEpoch != binding.epoch {
		c.subMu.Unlock()
		return nil
	}
	delete(c.subscriptions, key)
	if c.reqIDMap[sub.ReqID] == key {
		delete(c.reqIDMap, sub.ReqID)
	}
	c.subMu.Unlock()
	if !c.SessionCurrent(binding) {
		binding.connection.releaseMarketDataSlotAtEpoch(sub.ReqID, binding.epoch)
		return nil
	}
	return binding.connection.cancelMarketDataForEpoch(ctx, sub.ReqID, binding.epoch)
}

func (c *Connector) hydrateExplicitMarketDataContract(contract Contract) Contract {
	if contract.ConID != 0 || contract.Symbol == "" {
		return contract
	}
	detail := c.cachedContractDetail(contract.Symbol)
	if detail == nil || detail.ConID == 0 {
		return contract
	}
	candidate := contract
	if !c.applyContractDetail(*detail, &candidate) {
		return contract
	}
	normalizeEquityRouting(&candidate, contract.PrimaryExch)
	if explicitContractRouteMatches(contract, candidate) {
		return candidate
	}
	return contract
}

func explicitContractRouteMatches(requested, candidate Contract) bool {
	if requested.Currency != "" && candidate.Currency != "" && !strings.EqualFold(requested.Currency, candidate.Currency) {
		return false
	}
	reqExchange := strings.ToUpper(strings.TrimSpace(requested.Exchange))
	reqPrimary := strings.ToUpper(strings.TrimSpace(requested.PrimaryExch))
	candExchange := strings.ToUpper(strings.TrimSpace(candidate.Exchange))
	candPrimary := strings.ToUpper(strings.TrimSpace(candidate.PrimaryExch))
	if reqPrimary != "" {
		return reqPrimary == candPrimary || reqPrimary == candExchange
	}
	if reqExchange == "" || reqExchange == "SMART" {
		return true
	}
	return reqExchange == candExchange || reqExchange == candPrimary
}

// EnsureMarketDataSubscription creates a live symbol subscription or refreshes
// one whose last observed tick is at least staleAfter old. A staleAfter value
// of zero or less disables age-based refresh. The boolean reports whether a new
// wire request was sent. ctx must be non-nil and bounds market-data slot
// acquisition; unavailable, inactive, entitlement, and request failures are
// returned.
func (c *Connector) EnsureMarketDataSubscription(ctx context.Context, symbol string, fields []string, staleAfter time.Duration) (bool, error) {
	symbol = strings.ToUpper(symbol)
	if reason, inactive := c.inactiveReason(symbol); inactive {
		if reason == "" {
			reason = "inactive"
		}
		c.logDebug("Skipping EnsureMarketDataSubscription for inactive symbol %s (%s)", symbol, reason)
		return false, ErrSymbolInactive
	}
	if absErr := c.marketDataAbsenceFor(symbol); absErr != nil {
		c.logDebug("Skipping EnsureMarketDataSubscription for %s (%v)", symbol, absErr)
		return false, absErr
	}
	c.subMu.Lock()
	defer c.subMu.Unlock()

	// Helper to (re)request from IBKR, mapping reqID
	request := func() (int, error) {
		if !c.IsReady() {
			return 0, fmt.Errorf("IBKR connection not ready")
		}

		contract, hasDetail := c.prepareContract(symbol, 2*time.Second, true)
		contract, hasDetail = c.waitForContractDetails(symbol, contract, hasDetail)
		if contract.SecType == "STK" && !hasDetail && contract.ConID == 0 {
			contract.PrimaryExch = ""
		}
		hydrated := hasDetail || contract.ConID != 0
		if !hydrated {
			if late := c.awaitContractDetail(symbol, contractHydrationGrace); late != nil {
				if c.applyContractDetail(*late, &contract) && contract.ConID != 0 {
					hydrated = true
				}
			}
		}
		if !hydrated {
			return 0, fmt.Errorf("contract details pending for %s", symbol)
		}

		var (
			reqID int
			err   error
		)

		reqID, err = c.conn.RequestMarketDataWithContract(ctx, contract, "100,101,104,106,165,221,233,236", false, false)
		if err != nil {
			return 0, err
		}
		c.reqIDMap[reqID] = symbol
		return reqID, nil
	}

	if sub, exists := c.subscriptions[symbol]; exists {
		// Refresh if stale
		if staleAfter > 0 && time.Since(sub.LastTime) >= staleAfter {
			if sub.ReqID != 0 {
				if conn := c.conn; conn != nil && conn.IsConnected() && wireCancelNeeded(sub) {
					if err := conn.CancelMarketData(sub.ReqID); err != nil {
						marketDataLogger.Warnf("%s: Failed to cancel stale market data for %s (ReqID: %d): %v", c.name, symbol, sub.ReqID, err)
					}
				} else if conn != nil {
					// Server-side-rejected reqID or no live session: the wire
					// cancel would only draw error 300, but slot accounting
					// must stay in sync. The per-reqID release is idempotent
					// with the release the notice path may already have done —
					// the raw rateLimiter release this replaces could
					// double-release and panic the semaphore.
					conn.releaseMarketDataSlot(sub.ReqID)
				}
				// Reset subscription metadata so the new request can cleanly re-register
				sub.ReqID = 0
				sub.Observed = false
				// Drain any stale rejection left by the previous reqID so
				// the next poller doesn't fast-abort on it.
				if sub.RejectCh != nil {
					select {
					case <-sub.RejectCh:
					default:
					}
				} else {
					sub.RejectCh = make(chan SubscriptionRejection, 1)
				}
			}
			reqID, err := request()
			if err != nil {
				marketDataLogger.Warnf("%s: Failed to refresh market data for %s: %v", c.name, symbol, err)
				return false, err
			}
			sub.ReqID = reqID
			marketDataLogger.Debugf("%s: Refreshed market data subscription for %s (ReqID: %d)", c.name, symbol, reqID)
			return true, nil
		}
		// Already subscribed and fresh enough
		return false, nil
	}

	// No existing subscription: create and request
	reqID := 0
	if c.IsReady() {
		if rid, err := request(); err == nil {
			reqID = rid
		} else {
			marketDataLogger.Warnf("%s: Failed to request market data for %s: %v", c.name, symbol, err)
			return false, err
		}
	} else {
		return false, fmt.Errorf("IBKR connection not ready")
	}

	sub := &Subscription{
		Symbol:   symbol,
		ReqID:    reqID,
		Fields:   fields,
		LastTime: time.Now(),
		RejectCh: make(chan SubscriptionRejection, 1),
	}
	c.subscriptions[symbol] = sub
	marketDataLogger.Debugf("%s: Subscribed to market data for %s (ReqID: %d)", c.name, symbol, reqID)
	return true, nil
}

// UnsubscribeMarketData removes the normalized symbol or route key from the
// local subscription cache and best-effort cancels its live broker request. It
// is idempotent when no matching subscription exists. For routed subscriptions,
// pass the key returned by [Connector.SubscribeMarketDataWithContract].
func (c *Connector) UnsubscribeMarketData(symbol string) error {
	symbol = strings.ToUpper(symbol)
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		// Make this idempotent; no-op if not found
		marketDataLogger.Debugf("%s: Unsubscribe requested for %s but no active subscription found", c.name, symbol)
		return nil
	}

	delete(c.subscriptions, symbol)

	// Cancel on the wire and release the rate-limiter slot, regardless of
	// whether we ever saw a tick.
	//
	// The prior `&& sub.Observed` guard was there to avoid IBKR errorCode 300
	// ("Can't find EId with tickerId") on shutdown, where the gateway has
	// already torn down subscriptions. But Observed is only set by
	// handleTickPrice / handleTickSize, NOT by handleOptionComputation —
	// so OPT subscriptions that receive ONLY model-computation ticks (msg 21)
	// stay Observed=false and were being unsubscribed locally without
	// CancelMarketData firing. CancelMarketData is what calls
	// releaseMarketDataSlot, so every such leg leaked one slot in
	// rateLimiter.marketDataSubs. Off-hours gamma fan-outs hit the 100-slot
	// cap within ~120s, tripping the breaker and aborting the compute.
	//
	// Shutdown is still handled correctly: IsConnected returns false
	// post-disconnect, so the cancel still skips. The only remaining
	// downside is occasional 300 warnings when canceling a sub the gateway
	// never accepted — strictly cosmetic, vs slot-leak which is functional.
	//
	// One narrow exception (wireCancelNeeded): when the gateway itself
	// reported this exact reqID terminally dead (200/354 system notice),
	// the wire cancel is guaranteed to draw error 300 — skip it, but
	// still release the rate-limiter slot (idempotent with the release
	// done at notice time; never skipped, per the slot-leak lesson).
	if c.conn != nil && c.conn.IsConnected() && sub.ReqID != 0 {
		if wireCancelNeeded(sub) {
			if err := c.conn.CancelMarketData(sub.ReqID); err != nil {
				marketDataLogger.Warnf("%s: Failed to cancel market data %s (ReqID: %d): %v", c.name, symbol, sub.ReqID, err)
			}
		} else {
			c.conn.releaseMarketDataSlot(sub.ReqID)
			marketDataLogger.Debugf("%s: Skipping wire cancel for %s (ReqID %d already rejected server-side)", c.name, symbol, sub.ReqID)
		}
	}

	marketDataLogger.Debugf("%s: Unsubscribed from market data for %s", c.name, symbol)
	return nil
}

// RawOrder contains the broker-wire fields accepted by Connector order-write
// methods. Numeric price fields use the contract currency. Callers are
// responsible for supplying a broker-valid combination of order type, prices,
// quantity, time in force, account, and routing fields.
type RawOrder struct {
	OrderID         int
	ClientID        int
	PermID          int
	Action          string // BUY or SELL
	TotalQty        int
	OrderType       string // MKT, LMT, STP, etc.
	LmtPrice        float64
	AuxPrice        float64 // Stop price for stop orders
	TrailStopPrice  float64
	TrailingPercent float64
	LmtPriceOffset  float64
	TIF             string // Time in force: DAY, GTC, IOC, etc.
	TriggerMethod   int    // IBKR stop trigger method for stop/trailing orders
	Account         string
	OrderRef        string // Our internal order ID
	OutsideRth      bool   // Allow execution outside regular trading hours
	OpenClose       string // O=open, C=close
}

// SubmitOrder sends an unrestricted order through the active broker
// connection. Default builds return [ErrTradingDisabled] before transmission;
// builds with the "trading" tag enable the wire path. A successful return means
// the frame was sent, not that the broker accepted or filled the order.
func (c *Connector) SubmitOrder(contract *Contract, order *RawOrder) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	return c.SubmitOrderForSession(binding, contract, order)
}

// SubmitOrderForSession sends an unrestricted order only on the exact
// Connector socket generation named by binding. The binding must have been
// captured from this Connector; reconnect or disconnect drift is rejected at
// allocator claim and again at the transport boundary before any wire write.
func (c *Connector) SubmitOrderForSession(binding ConnectorSessionBinding, contract *Contract, order *RawOrder) error {
	return c.SubmitOrderForSessionGuarded(context.Background(), binding, contract, order, nil)
}

// SubmitOrderForSessionGuarded carries caller cancellation and a final
// authority guard to the exact socket write. guard runs under the connection
// transport lock after pacing and epoch checks, immediately before any byte.
func (c *Connector) SubmitOrderForSessionGuarded(ctx context.Context, binding ConnectorSessionBinding, contract *Contract, order *RawOrder, guard func() error) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	return c.submitOrderForSession(ctx, binding, contract, order, nil, guard)
}

// SubmitPaperOrder validates gate against the configured connection and sends
// an order to a paper account. It is available in default builds without
// enabling [Connector.SubmitOrder]. A successful return means the frame was
// sent, not that the broker accepted or filled the order.
func (c *Connector) SubmitPaperOrder(gate PaperOrderGate, contract *Contract, order *RawOrder) error {
	binding, ok := c.CaptureSession()
	if !ok {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	return c.SubmitPaperOrderForSession(binding, gate, contract, order)
}

// SubmitPaperOrderForSession validates gate and sends a paper order only on
// the exact Connector socket generation named by binding.
func (c *Connector) SubmitPaperOrderForSession(binding ConnectorSessionBinding, gate PaperOrderGate, contract *Contract, order *RawOrder) error {
	return c.SubmitPaperOrderForSessionGuarded(context.Background(), binding, gate, contract, order, nil)
}

// SubmitPaperOrderForSessionGuarded is the paper-gated counterpart to
// SubmitOrderForSessionGuarded.
func (c *Connector) SubmitPaperOrderForSessionGuarded(ctx context.Context, binding ConnectorSessionBinding, gate PaperOrderGate, contract *Contract, order *RawOrder, guard func() error) error {
	if err := gate.validate(); err != nil {
		return definitelyUnsent(err)
	}
	return c.submitOrderForSession(ctx, binding, contract, order, &gate, guard)
}

func (c *Connector) submitOrderForSession(ctx context.Context, binding ConnectorSessionBinding, contract *Contract, order *RawOrder, paperGate *PaperOrderGate, guard func() error) error {
	if c == nil {
		return definitelyUnsent(fmt.Errorf("broker Connector is nil"))
	}
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("broker order context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if contract == nil {
		return definitelyUnsent(fmt.Errorf("broker order contract is nil"))
	}
	if order == nil {
		return definitelyUnsent(fmt.Errorf("broker order is nil"))
	}
	if !c.SessionCurrent(binding) {
		return definitelyUnsent(fmt.Errorf("broker session binding is not current for this Connector"))
	}
	if down, at := c.backendConnectivityDown(); down {
		return definitelyUnsent(fmt.Errorf("TWS reported IBKR backend connectivity lost at %s (code 1100) with no restore notice yet; refusing to transmit a broker order into a dead link", at.Format(time.RFC3339)))
	}
	conn := binding.connection

	// Convert to IBKROrder for the connection
	ibkrOrder := &IBKROrder{
		OrderID:         order.OrderID,
		ClientID:        order.ClientID,
		PermID:          order.PermID,
		ConID:           contract.ConID,
		Symbol:          contract.Symbol,
		SecType:         contract.SecType,
		Expiry:          contract.Expiry,
		Strike:          contract.Strike,
		Right:           contract.Right,
		Multiplier:      multiplierToString(contract.Multiplier),
		Exchange:        contract.Exchange,
		PrimaryExch:     contract.PrimaryExch,
		Currency:        contract.Currency,
		LocalSymbol:     contract.LocalSymbol,
		TradingClass:    contract.TradingClass,
		Action:          order.Action,
		TotalQty:        order.TotalQty,
		OrderType:       order.OrderType,
		LmtPrice:        order.LmtPrice,
		AuxPrice:        order.AuxPrice,
		TrailStopPrice:  order.TrailStopPrice,
		TrailingPercent: order.TrailingPercent,
		LmtPriceOffset:  order.LmtPriceOffset,
		TIF:             order.TIF,
		TriggerMethod:   order.TriggerMethod,
		OrderRef:        order.OrderRef,
		OutsideRth:      order.OutsideRth,
		Account:         order.Account,
		Transmit:        true,
		OpenClose:       strings.ToUpper(strings.TrimSpace(order.OpenClose)),
		Origin:          0,
	}
	if ibkrOrder.OpenClose == "" {
		ibkrOrder.OpenClose = "O"
	}

	// Bind the order id under the same coordination boundary used by the exact
	// historical request. Read-only requests keep their IDs; a colliding or
	// stale caller-supplied order is refused before local indexing or wire send.
	var claimEpoch uint64
	c.brokerIDNamespaceMu.Lock()
	if ibkrOrder.OrderID <= 0 {
		var err error
		ibkrOrder.OrderID, claimEpoch, err = c.nextDisjointOrderIDLockedForSession(binding)
		if err != nil {
			c.brokerIDNamespaceMu.Unlock()
			return definitelyUnsent(err)
		}
	} else {
		if c.feeRequestOwnsID(ibkrOrder.OrderID) {
			c.brokerIDNamespaceMu.Unlock()
			return definitelyUnsent(fmt.Errorf("%w: explicit order ID is owned by an active read-only request", ErrBrokerIDNamespaceConflict))
		}
		owned := c.isKnownBrokerOrderID(ibkrOrder.OrderID)
		var err error
		claimEpoch, err = conn.claimOrderIDForForwardingAtEpoch(ibkrOrder.OrderID, owned, &binding.epoch)
		if err != nil {
			c.brokerIDNamespaceMu.Unlock()
			return definitelyUnsent(err)
		}
	}
	defer conn.discardOrderIDReservation(ibkrOrder.OrderID)
	c.orderIDHighWater = max(c.orderIDHighWater, ibkrOrder.OrderID)

	brokerID := strconv.Itoa(ibkrOrder.OrderID)
	localID := strings.TrimSpace(order.OrderRef)
	if localID == "" {
		localID = brokerID
	}
	now := time.Now()
	stopPrice := order.AuxPrice
	if order.TrailStopPrice != 0 {
		stopPrice = order.TrailStopPrice
	}
	coreOrder := &trackedOrder{
		ID:              localID,
		BrokerID:        brokerID,
		Symbol:          contract.Symbol,
		Side:            OrderSide(order.Action),
		Quantity:        float64(order.TotalQty),
		OrderType:       mapIBOrderType(order.OrderType),
		LimitPrice:      order.LmtPrice,
		StopPrice:       stopPrice,
		TimeInForce:     mapIBTimeInForce(order.TIF),
		Status:          OrderStatusPending,
		CreatedAt:       now,
		UpdatedAt:       now,
		AllowOutsideRth: order.OutsideRth,
	}

	c.orderMu.Lock()
	previousOrder, hadPreviousOrder := c.openOrders[localID]
	previousIndex, hadPreviousIndex := c.brokerOrderIndex[brokerID]
	c.openOrders[localID] = coreOrder
	c.brokerOrderIndex[brokerID] = localID
	c.orderMu.Unlock()
	c.brokerIDNamespaceMu.Unlock()

	// Place the order through the connection after local indexing so fast
	// broker callbacks and errors can be correlated with the journal.
	var err error
	if paperGate != nil {
		err = conn.placePaperOrderForEpochGuarded(ctx, *paperGate, ibkrOrder, claimEpoch, guard)
	} else {
		err = conn.placeOrderForEpochGuarded(ctx, ibkrOrder, claimEpoch, guard)
	}
	if err != nil {
		if SendDispositionOf(err) == SendDispositionDefinitelyUnsent {
			c.orderMu.Lock()
			if hadPreviousOrder {
				c.openOrders[localID] = previousOrder
			} else {
				delete(c.openOrders, localID)
			}
			if hadPreviousIndex {
				c.brokerOrderIndex[brokerID] = previousIndex
			} else {
				delete(c.brokerOrderIndex, brokerID)
			}
			c.orderMu.Unlock()
		}
		// A possibly written instruction retains its exact positive ID and local
		// correlation so callbacks and a later safety cancel can still find it.
		order.OrderID = ibkrOrder.OrderID
		order.ClientID = ibkrOrder.ClientID
		order.PermID = ibkrOrder.PermID
		return fmt.Errorf("failed to place order: %w", err)
	}
	order.OrderID = ibkrOrder.OrderID
	order.ClientID = ibkrOrder.ClientID
	order.PermID = ibkrOrder.PermID

	if newBrokerID := strconv.Itoa(ibkrOrder.OrderID); newBrokerID != brokerID {
		c.orderMu.Lock()
		delete(c.brokerOrderIndex, brokerID)
		coreOrder.BrokerID = newBrokerID
		c.brokerOrderIndex[newBrokerID] = localID
		c.orderMu.Unlock()
	}

	c.logInfo("Order submitted: ID=%d, %s %s %d @ %.2f (TIF=%s, OutsideRth=%v)",
		ibkrOrder.OrderID, order.Action, contract.Symbol, order.TotalQty,
		order.LmtPrice, order.TIF, order.OutsideRth)

	return nil
}

// ReserveOrderID claims the next broker order ID without submitting an order.
// Default builds return [ErrTradingDisabled]. The ID is consumed locally and
// should be passed to a later [Connector.SubmitOrder] call.
func (c *Connector) ReserveOrderID() (int, error) {
	if !tradingEnabled {
		return 0, ErrTradingDisabled
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	return c.ReserveOrderIDForSession(binding)
}

// ReserveOrderIDForSession claims the next broker order ID from the exact
// socket generation named by binding. The reservation remains tied to that
// epoch and cannot authorize a later-session submission.
func (c *Connector) ReserveOrderIDForSession(binding ConnectorSessionBinding) (int, error) {
	if !tradingEnabled {
		return 0, ErrTradingDisabled
	}
	if !c.SessionCurrent(binding) {
		return 0, fmt.Errorf("broker session binding is not current for this Connector")
	}
	c.brokerIDNamespaceMu.Lock()
	id, _, err := c.nextDisjointOrderIDLockedForSession(binding)
	if err != nil {
		c.brokerIDNamespaceMu.Unlock()
		return 0, err
	}
	c.orderIDHighWater = max(c.orderIDHighWater, id)
	c.brokerIDNamespaceMu.Unlock()
	return id, nil
}

func (c *Connector) nextDisjointOrderIDLockedForSession(binding ConnectorSessionBinding) (int, uint64, error) {
	for {
		id, epoch, err := binding.connection.reserveNextOrderIDForEpoch(binding.epoch)
		if err != nil {
			return 0, epoch, err
		}
		if !c.feeRequestOwnsID(id) {
			return id, epoch, nil
		}
		binding.connection.discardOrderIDReservation(id)
	}
}

func (c *Connector) nextDisjointOrderIDLocked(conn *Connection) (int, uint64, error) {
	for {
		id, epoch, err := conn.reserveNextOrderID()
		if err != nil {
			return 0, epoch, err
		}
		if !c.feeRequestOwnsID(id) {
			return id, epoch, nil
		}
		// The shared frontier remains consumed, but a skipped read-owned ID
		// must not retain order-reservation provenance that could authorize a
		// later explicit claim.
		conn.discardOrderIDReservation(id)
	}
}

func (c *Connector) feeRequestOwnsID(id int) bool {
	if id <= 0 {
		return false
	}
	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()
	request := c.historicalReqs[id]
	return c.historicalRouteReqs[id] != nil || (request != nil && request.requestOwnsNoticeCollision)
}

type orderLifecycleHandlerEntry struct {
	legacy  func(OrderLifecycleEvent)
	receipt func(OrderLifecycleReceipt)
}

// RegisterOrderLifecycleHandler appends a compatibility callback for broker
// order lifecycle messages. Callbacks run synchronously in registration order
// and must return quickly. A nil Connector or handler is ignored.
func (c *Connector) RegisterOrderLifecycleHandler(handler func(OrderLifecycleEvent)) {
	if c == nil || handler == nil {
		return
	}
	c.orderLifecycleMu.Lock()
	c.orderLifecycle = append(c.orderLifecycle, orderLifecycleHandlerEntry{legacy: handler})
	c.orderLifecycleMu.Unlock()
}

// RegisterOrderLifecycleReceiptHandler appends a callback that receives the
// exact socket-session receipt for every event.
func (c *Connector) RegisterOrderLifecycleReceiptHandler(handler func(OrderLifecycleReceipt)) {
	if c == nil || handler == nil {
		return
	}
	c.orderLifecycleMu.Lock()
	c.orderLifecycle = append(c.orderLifecycle, orderLifecycleHandlerEntry{receipt: handler})
	c.orderLifecycleMu.Unlock()
}

// OrderLifecycleGeneration returns the current connection-local order-event
// frontier without issuing a broker request. Zero means no accepted lifecycle
// callback has been observed by this Connector.
func (c *Connector) OrderLifecycleGeneration() uint64 {
	if c == nil {
		return 0
	}
	return c.orderLifecycleGeneration.Load()
}

// PortfolioProjectionGeneration returns the current structural portfolio
// frontier without issuing a broker request. It advances for scope,
// completeness, contract-set, or quantity changes, but not mark/PnL-only
// updates.
func (c *Connector) PortfolioProjectionGeneration() uint64 {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return 0
	}
	return conn.PortfolioProjectionGeneration()
}

// PortfolioProjectionBinding is a caller-owned, exact-session snapshot of the
// structural portfolio projection and its typed stream receipt.
type PortfolioProjectionBinding struct {
	Session    ConnectorSessionBinding
	Positions  []*RawPosition
	Health     PortfolioStreamHealth
	Generation uint64
}

// CapturePortfolioProjectionForSession snapshots positions, health, and the
// structural generation while portfolio/session mutations are excluded.
// False means binding is stale or the Connector is not ready.
func (c *Connector) CapturePortfolioProjectionForSession(binding ConnectorSessionBinding) (PortfolioProjectionBinding, bool) {
	if c == nil {
		return PortfolioProjectionBinding{}, false
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(binding) {
		return PortfolioProjectionBinding{}, false
	}
	positions, health := binding.connection.GetPositionsWithPortfolioHealth()
	return PortfolioProjectionBinding{
		Session: binding, Positions: c.filteredCachedPositions(positions),
		Health: health, Generation: health.ProjectionGeneration,
	}, true
}

// CapturePortfolioProjectionForBoundSession snapshots the structural
// portfolio authority without acquiring publicationBarrier or evidenceBarrier.
// It is only valid from a protected order wire guard while the transport owns
// publicationBarrier for reading and evidenceBarrier exclusively. Keeping this
// variant lock-free preserves the publication-then-evidence lock order.
func (c *Connector) CapturePortfolioProjectionForBoundSession(binding ConnectorSessionBinding) (PortfolioProjectionBinding, bool) {
	if c == nil || !c.SessionCurrent(binding) {
		return PortfolioProjectionBinding{}, false
	}
	positions, health := binding.connection.GetPositionsWithPortfolioHealth()
	if !c.SessionCurrent(binding) {
		return PortfolioProjectionBinding{}, false
	}
	return PortfolioProjectionBinding{
		Session: binding, Positions: c.filteredCachedPositions(positions),
		Health: health, Generation: health.ProjectionGeneration,
	}, true
}

// BrokerEvidenceBinding is a point-in-time identity for the Connector session,
// order callback frontier, and structural portfolio projection.
type BrokerEvidenceBinding struct {
	Session                       ConnectorSessionBinding
	OrderLifecycleGeneration      uint64
	PortfolioProjectionGeneration uint64
}

// CaptureBrokerEvidence returns one stable broker-evidence frontier. False
// means the Connector is not a ready current session.
func (c *Connector) CaptureBrokerEvidence() (BrokerEvidenceBinding, bool) {
	if c == nil {
		return BrokerEvidenceBinding{}, false
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.RLock()
	defer c.evidenceBarrier.RUnlock()
	session, ok := c.CaptureSession()
	if !ok {
		return BrokerEvidenceBinding{}, false
	}
	return BrokerEvidenceBinding{
		Session: session, OrderLifecycleGeneration: c.OrderLifecycleGeneration(),
		PortfolioProjectionGeneration: c.PortfolioProjectionGeneration(),
	}, true
}

// WithStableBrokerEvidence executes commit while structural portfolio/session
// writers and order lifecycle dispatch are excluded. It returns false without
// calling commit when binding is no longer exact.
func (c *Connector) WithStableBrokerEvidence(binding BrokerEvidenceBinding, commit func() bool) bool {
	if c == nil || commit == nil {
		return false
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	if !c.SessionCurrent(binding.Session) || c.OrderLifecycleGeneration() != binding.OrderLifecycleGeneration ||
		c.PortfolioProjectionGeneration() != binding.PortfolioProjectionGeneration {
		return false
	}
	return commit()
}

// WithBrokerEvidenceMutation serializes an external owner-published identity
// change after all in-flight Connector evidence dispatch and exact-session
// broker operations have drained. It exists for daemon connector publication
// only; it does not authorize broker activity.
func (c *Connector) WithBrokerEvidenceMutation(change func()) {
	if change == nil {
		return
	}
	if c == nil {
		change()
		return
	}
	c.publicationBarrier.Lock()
	defer c.publicationBarrier.Unlock()
	c.evidenceBarrier.Lock()
	defer c.evidenceBarrier.Unlock()
	change()
}

// WithBoundBrokerSession admits operation only when binding names the current
// Connector socket session. It deliberately does not hold publication while
// operation waits in pacing or on a paused transport. Protected transports
// acquire the publication read side only for their final guarded write, where
// the exact Connection epoch remains the final pre-wire authority.
// False means binding was not current and operation was not called.
func (c *Connector) WithBoundBrokerSession(binding ConnectorSessionBinding, operation func() error) (bool, error) {
	if c == nil || operation == nil {
		return false, nil
	}
	if !c.SessionCurrent(binding) {
		return false, nil
	}
	return true, operation()
}

func multiplierToString(mult int) string {
	if mult <= 0 {
		return ""
	}
	return strconv.Itoa(mult)
}

// CancelOrder sends a cancellation for broker orderID. Default builds return
// [ErrTradingDisabled]. A successful return means the cancellation frame was
// sent, not that the broker confirmed the order cancelled.
func (c *Connector) CancelOrder(orderID int) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	binding, ok := c.CaptureSession()
	if !ok {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	return c.CancelOrderForSession(binding, orderID)
}

// CancelOrderForSession sends a cancellation only on the exact Connector
// socket generation named by binding.
func (c *Connector) CancelOrderForSession(binding ConnectorSessionBinding, orderID int) error {
	return c.CancelOrderForSessionGuarded(context.Background(), binding, orderID, nil)
}

// CancelOrderForSessionGuarded carries caller cancellation and a final
// authority guard to the exact cancel frame.
func (c *Connector) CancelOrderForSessionGuarded(ctx context.Context, binding ConnectorSessionBinding, orderID int, guard func() error) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("broker cancel context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if !c.SessionCurrent(binding) {
		return definitelyUnsent(fmt.Errorf("broker session binding is not current for this Connector"))
	}
	err := binding.connection.cancelOrderForEpochGuarded(ctx, orderID, binding.epoch, guard)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	c.logInfo("Order cancel request sent for ID: %d", orderID)

	return nil
}

// CancelPaperOrder validates gate against the configured connection and sends
// a cancellation for broker orderID in a paper account. A successful return
// means the frame was sent, not that the broker confirmed cancellation.
func (c *Connector) CancelPaperOrder(gate PaperOrderGate, orderID int) error {
	binding, ok := c.CaptureSession()
	if !ok {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	return c.CancelPaperOrderForSession(binding, gate, orderID)
}

// CancelPaperOrderForSession validates gate and sends a paper cancellation
// only on the exact Connector socket generation named by binding.
func (c *Connector) CancelPaperOrderForSession(binding ConnectorSessionBinding, gate PaperOrderGate, orderID int) error {
	return c.CancelPaperOrderForSessionGuarded(context.Background(), binding, gate, orderID, nil)
}

// CancelPaperOrderForSessionGuarded is the paper-gated counterpart to
// CancelOrderForSessionGuarded.
func (c *Connector) CancelPaperOrderForSessionGuarded(ctx context.Context, binding ConnectorSessionBinding, gate PaperOrderGate, orderID int, guard func() error) error {
	if err := gate.validate(); err != nil {
		return definitelyUnsent(err)
	}
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("broker cancel context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if !c.SessionCurrent(binding) {
		return definitelyUnsent(fmt.Errorf("broker session binding is not current for this Connector"))
	}
	if err := binding.connection.cancelPaperOrderForEpochGuarded(ctx, gate, orderID, binding.epoch, guard); err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}
	c.logInfo("Paper order cancel request sent for ID: %d", orderID)
	return nil
}

func (c *Connector) seedContractCacheFromPositions(positions map[string]*RawPosition) {
	if len(positions) == 0 {
		return
	}

	hints := make(map[string]ContractDetailsLite, len(positions))
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		if isZeroValueStockPosition(pos) {
			continue
		}
		contract := pos.Contract
		if contract.ConID == 0 {
			continue
		}
		// Only seed bare-symbol cache entries from stock positions. The
		// cache is indexed by the underlying ticker (e.g. "SPY") and is
		// later read by prepareContract when resolving stock quote
		// requests; if a held option (`SPY 700P 2026-06-18`, secType=OPT)
		// is allowed to seed under the bare key, prepareContract picks
		// up the option's ConID and `quote SPY` returns the option's
		// pricing instead of the ETF's. Filter to STK only.
		if !strings.EqualFold(contract.SecType, "STK") {
			continue
		}
		symbol := strings.ToUpper(strings.TrimSpace(contract.Symbol))
		if symbol == "" {
			continue
		}

		detail := ContractDetailsLite{
			Symbol:       symbol,
			Exchange:     strings.TrimSpace(contract.Exchange),
			PrimaryExch:  strings.TrimSpace(contract.PrimaryExch),
			ConID:        contract.ConID,
			LocalSymbol:  strings.TrimSpace(contract.LocalSymbol),
			TradingClass: strings.TrimSpace(contract.TradingClass),
		}

		if existing, ok := hints[symbol]; ok {
			hints[symbol] = mergeContractDetailsLite(existing, detail)
		} else {
			hints[symbol] = detail
		}
	}

	if len(hints) == 0 {
		return
	}

	c.contractMu.Lock()
	for symbol, hint := range hints {
		if cached, ok := c.contractCache[symbol]; ok {
			c.contractCache[symbol] = mergeContractDetailsLite(cached, hint)
		} else {
			c.contractCache[symbol] = hint
		}
	}
	c.contractMu.Unlock()
}

// isConnected checks if we have an active IBKR connection. Reconnection on
// loss is the daemon's responsibility (server.triggerReconnect); the
// connector reports honest connectivity rather than masking it with retry
// state.
func (c *Connector) isConnected() bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	return conn.IsConnected()
}

// IsConnected reports whether the Connector currently has an active broker
// connection. It does not imply that response handlers are ready; use
// [Connector.IsReady] when issuing requests.
func (c *Connector) IsConnected() bool { return c.isConnected() }

// UsingTLS reports the TLS mode the active session negotiated. False when
// disconnected or when a non-TLS handshake succeeded (possibly via fallback).
func (c *Connector) UsingTLS() bool {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return false
	}
	return conn.UsingTLS()
}

// IsReady reports whether the broker connection is established and the
// Connector's response handlers are registered.
func (c *Connector) IsReady() bool {
	c.mu.RLock()
	rd := c.ready
	c.mu.RUnlock()
	return rd && c.isConnected()
}

// CaptureSession returns the exact ready Connection object and socket epoch
// that may own a new broker-adjacent read. The token is process-local and is
// evidence for a later equality check, not durable readiness or authority.
func (c *Connector) CaptureSession() (ConnectorSessionBinding, bool) {
	if c == nil {
		return ConnectorSessionBinding{}, false
	}
	c.mu.RLock()
	conn := c.conn
	ready := c.ready
	c.mu.RUnlock()
	if !ready || conn == nil || !conn.IsConnected() {
		return ConnectorSessionBinding{}, false
	}
	return ConnectorSessionBinding{connector: c, connection: conn, epoch: conn.BrokerSessionEpoch()}, true
}

// SessionCurrent reports whether binding still names this Connector's ready
// Connection and exact socket epoch.
func (c *Connector) SessionCurrent(binding ConnectorSessionBinding) bool {
	if c == nil || binding.connector != c || binding.connection == nil {
		return false
	}
	c.mu.RLock()
	conn := c.conn
	ready := c.ready
	c.mu.RUnlock()
	return ready && conn == binding.connection && conn.IsConnected() && conn.BrokerSessionEpoch() == binding.epoch
}

// SessionReceiptCurrent reports whether binding names the Connector's exact
// installed inbound socket generation. Connecting is accepted because
// startAPI synchronously processes a small frame burst before onConnect;
// disconnected/failed/reconnecting states are never current. This does not
// authorize outbound requests.
func (c *Connector) SessionReceiptCurrent(binding ConnectorSessionBinding) bool {
	if c == nil || binding.connector != c || binding.connection == nil {
		return false
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn != binding.connection || conn.BrokerSessionEpoch() != binding.epoch {
		return false
	}
	status := conn.Status()
	return status == StatusConnecting || status == StatusConnected
}

// CaptureHistoricalSession is the historical-data compatibility spelling for
// [Connector.CaptureSession].
func (c *Connector) CaptureHistoricalSession() (HistoricalSessionBinding, bool) {
	return c.CaptureSession()
}

// HistoricalSessionCurrent is the historical-data compatibility spelling for
// [Connector.SessionCurrent].
func (c *Connector) HistoricalSessionCurrent(binding HistoricalSessionBinding) bool {
	return c.SessionCurrent(binding)
}

// ServerVersion returns the IBKR server protocol version reported by the
// gateway during the handshake. Returns 0 when no connection is established.
func (c *Connector) ServerVersion() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil {
		return 0
	}
	return c.conn.ServerVersion()
}

// SubscribeOption ensures a streaming subscription exists for a fully
// specified option contract. expiryYMD uses YYYYMMDD and right uses C or P. An
// empty tradingClass defaults to the normalized underlying; callers handling
// multiple classes for one underlying must supply the class explicitly. The
// returned key addresses cached values in [Connector.MarketDataSnapshot], and
// the request ID identifies the live subscription. ctx bounds contract
// resolution and slot acquisition.
func (c *Connector) SubscribeOption(ctx context.Context, underlying, tradingClass, expiryYMD string, strike float64, right string) (string, int, error) {
	if !c.isConnected() {
		return "", 0, ErrIBKRUnavailable
	}
	upperUnderlying := strings.ToUpper(underlying)
	upperClass := strings.ToUpper(strings.TrimSpace(tradingClass))
	if upperClass == "" {
		upperClass = upperUnderlying
	}
	key := optionMarketDataKeyForClass(upperUnderlying, upperClass, expiryYMD, right, strike)

	c.subMu.RLock()
	if existing, ok := c.subscriptions[key]; ok {
		c.subMu.RUnlock()
		return key, existing.ReqID, nil
	}
	c.subMu.RUnlock()

	contract := Contract{
		Symbol:       upperUnderlying,
		SecType:      "OPT",
		Exchange:     "SMART",
		PrimaryExch:  optionUnderlyingPrimaryExchangeHint(upperUnderlying),
		Currency:     "USD",
		Expiry:       expiryYMD,
		Strike:       strike,
		Right:        strings.ToUpper(right),
		Multiplier:   100,
		TradingClass: upperClass,
	}

	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return "", 0, ErrIBKRUnavailable
	}

	if conn.applyCachedOptionContract(&contract) {
		normalizeResolvedOptionMarketDataContract(&contract)
	} else {
		// Index options and other non-direct paths still resolve ConID before
		// reqMktData. SPX/SPXW in particular needs this cache-backed path to
		// preserve its classed contract identity.
		if err := conn.resolveOptionContract(ctx, &contract, 5*time.Second); err != nil {
			return "", 0, fmt.Errorf("resolve option %s %s %.2f%s: %w",
				contract.Symbol, contract.Expiry, contract.Strike, contract.Right, err)
		}
		normalizeResolvedOptionMarketDataContract(&contract)
	}

	// Generic ticks mirror RequestOptionsMarketData: 100=opt volume,
	// 101=opt open interest, 104=hist vol, 106=opt implied vol. For
	// individual OPT subscriptions the canonical per-strike IV source
	// is the OPTION_COMPUTATION model tick (msg 21, tickType 13) which
	// handleOptionComputation routes into optIV[OPRA_key] — readable
	// via OptionIV; 106 is documented for STK/IND (the 30-day
	// chain-averaged IV of the underlying) and is not reliably
	// delivered for option contracts, so callers must not depend on
	// subscriptions[…].IV for per-strike values. We still ask for 106
	// because it's harmless and the gateway occasionally fills it on
	// recently-traded contracts, but OptionIV is the source of truth.
	// OI ticks can be one-shot and arrive immediately after reqMktData.
	// Register the reqID before the wire send so ticks 27/28 cannot race
	// ahead of the connector routing maps.
	reqID, err := conn.requestMarketDataWithContract(ctx, contract, OptionSubscriptionGenericTicks, false, false, func(reqID int) func() {
		subscriptionRight := strings.ToUpper(strings.TrimSpace(contract.Right))
		if subscriptionRight != "" {
			subscriptionRight = subscriptionRight[:1]
		}
		c.subMu.Lock()
		c.reqIDMap[reqID] = key
		c.subscriptions[key] = &Subscription{
			Symbol:   key,
			Right:    subscriptionRight,
			ReqID:    reqID,
			LastTime: time.Now(),
			RejectCh: make(chan SubscriptionRejection, 1),
		}
		c.subMu.Unlock()
		// Route option-computation ticks (msg 21, tick types 10/11/13) for this
		// reqID into optIV / optQuoteMid keyed by the OPRA chain key. This is the
		// same handler path SubscribeOptionIV uses for ATM IV; per-strike chain
		// renders just need a different key so multiple strikes coexist.
		c.optMu.Lock()
		c.optReqIDs[reqID] = key
		c.optMu.Unlock()
		return func() {
			c.removePreparedOptionSubscription(key, reqID)
		}
	})
	if err != nil {
		return "", 0, err
	}
	return key, reqID, nil
}

func (c *Connector) removePreparedOptionSubscription(key string, reqID int) {
	c.subMu.Lock()
	if c.reqIDMap[reqID] == key {
		delete(c.reqIDMap, reqID)
	}
	if sub, ok := c.subscriptions[key]; ok && sub.ReqID == reqID {
		delete(c.subscriptions, key)
	}
	c.subMu.Unlock()
	c.optMu.Lock()
	if c.optReqIDs[reqID] == key {
		delete(c.optReqIDs, reqID)
	}
	c.optMu.Unlock()
}

// OptionMarketDataKey returns the normalized in-memory cache key used for an
// option contract, formatted as UNDERLYING_YYMMDDC100. Hyphens are removed
// from expiryYMD, only its last six digits are retained, and strike is formatted
// with no fractional digits.
func OptionMarketDataKey(underlying, expiryYMD, right string, strike float64) string {
	upperUnderlying := strings.ToUpper(strings.TrimSpace(underlying))
	upperRight := strings.ToUpper(strings.TrimSpace(right))
	expiryKey := strings.ReplaceAll(strings.TrimSpace(expiryYMD), "-", "")
	if len(expiryKey) > 6 {
		expiryKey = expiryKey[len(expiryKey)-6:]
	}
	return fmt.Sprintf("%s_%s%s%.0f", upperUnderlying, expiryKey, upperRight, strike)
}

func optionMarketDataKeyForClass(underlying, tradingClass, expiryYMD, right string, strike float64) string {
	base := OptionMarketDataKey(underlying, expiryYMD, right, strike)
	upperUnderlying := strings.ToUpper(strings.TrimSpace(underlying))
	upperClass := strings.ToUpper(strings.TrimSpace(tradingClass))
	if upperClass == "" || upperClass == upperUnderlying {
		return base
	}
	return upperUnderlying + "_" + upperClass + strings.TrimPrefix(base, upperUnderlying)
}

// PrewarmOptionChain resolves and caches option contracts for each expiry in a
// symbol and trading-class pair. It returns one result per expiry with cache
// counts, duration, and any error. The call returns nil when disconnected; ctx
// and timeout bound the underlying bulk requests. Later [Connector.SubscribeOption]
// calls can reuse the resolved contract identities.
func (c *Connector) PrewarmOptionChain(
	ctx context.Context,
	symbol string,
	expiries []string,
	tradingClass string,
	timeout time.Duration,
) []PrewarmOptionChainResult {
	if !c.isConnected() {
		return nil
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil
	}
	return conn.PrewarmOptionChain(ctx, symbol, expiries, tradingClass, timeout)
}

// RequestAccountUpdates starts the singleton streaming account and portfolio
// subscription used by [Connector.CachedPositions]. Pass an empty or aggregate
// account value to resolve a concrete managed account from the connection.
//
// Aggregate values ("All", comma-separated managedAccounts lists) are not
// account codes — TWS rejects them with error 321 and the portfolio stream
// never starts. They are reduced to a concrete code (or to "", which TWS
// resolves itself for single-account logins) before hitting the wire.
func (c *Connector) RequestAccountUpdates(account string) error {
	if !c.isConnected() {
		return ErrIBKRUnavailable
	}
	if !accountCodeConcrete(account) {
		account = c.conn.GetAccountCode()
	}
	if !accountCodeConcrete(account) {
		account = firstConcreteAccountCode(account)
	}
	now := c.acctUpdatesClock()
	c.acctUpdatesMu.Lock()
	c.acctUpdatesLastAt = now
	c.acctUpdatesAccount = account
	c.acctUpdatesMu.Unlock()
	return c.conn.RequestAccountUpdates(account)
}

// resubscribeAccountUpdates re-issues the subscription with the account the
// stream is already bound to. The self-heal paths must not pass "": on a
// multi-account login that resolves through the raw managedAccounts list to
// its positionally first entry, which silently rebinds the stream away from
// the operator's pinned account and makes the pinned account's own frames
// look foreign (issue #14).
func (c *Connector) resubscribeAccountUpdates() error {
	c.acctUpdatesMu.Lock()
	account := c.acctUpdatesAccount
	c.acctUpdatesMu.Unlock()
	return c.RequestAccountUpdates(account)
}

// acctUpdatesResubscribeThrottle bounds the dead-stream self-heal below to
// one resubscribe attempt per window, counted from the last subscribe.
const acctUpdatesResubscribeThrottle = 30 * time.Second

func (c *Connector) acctUpdatesClock() time.Time {
	if c.acctUpdatesNow != nil {
		return c.acctUpdatesNow()
	}
	return time.Now()
}

// maybeResubscribeAccountUpdates re-issues the account+portfolio stream
// subscription when the position cache is empty even though the account
// summary reports gross position value. The TWS account-updates stream
// occasionally fails to start after a rapid reconnect (observed
// 2026-06-11: one boot delivered no msgPortfolioValue at all while
// quotes and account summary flowed normally — positions stayed empty
// for the whole daemon lifetime). Throttled to one attempt per
// acctUpdatesResubscribeThrottle window, counted from the last subscribe;
// a genuinely flat account (no gross position value) never triggers.
func (c *Connector) maybeResubscribeAccountUpdates() {
	c.maybeResubscribeAccountUpdatesForReason(false)
}

// maybeResubscribeAccountUpdatesForScopeConflict repairs a rejected foreign
// account frame even when the retained cache is non-empty. The rows remain
// context only until a new, account-scoped subscription completes.
func (c *Connector) maybeResubscribeAccountUpdatesForScopeConflict() {
	c.maybeResubscribeAccountUpdatesForReason(true)
}

func (c *Connector) maybeResubscribeAccountUpdatesForReason(scopeConflict bool) {
	if !c.isConnected() {
		return
	}
	if !scopeConflict && !accountSummaryShowsPositions(c.conn.GetAccountSummary()) {
		return
	}
	now := c.acctUpdatesClock()
	c.acctUpdatesMu.Lock()
	stale := now.Sub(c.acctUpdatesLastAt) >= acctUpdatesResubscribeThrottle
	c.acctUpdatesMu.Unlock()
	if !stale {
		return
	}
	if scopeConflict {
		ibkrLogger.Warnf("portfolio stream account scope conflicted; resubscribing account updates")
	} else {
		ibkrLogger.Warnf("positions cache empty while account summary shows gross position value; resubscribing account updates")
	}
	_ = c.resubscribeAccountUpdates()
}

// accountSummaryShowsPositions reports whether any GrossPositionValue
// entry in the summary map (keys may carry a currency suffix, e.g.
// "GrossPositionValue_EUR") parses to a positive value.
func accountSummaryShowsPositions(summary map[string]string) bool {
	for key, raw := range summary {
		if key != "GrossPositionValue" && !strings.HasPrefix(key, "GrossPositionValue_") {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && v > 0 {
			return true
		}
	}
	return false
}

// CachedPositions returns the latest filtered portfolio cache without issuing
// a positions request. The returned slice is detached, but its [RawPosition]
// pointers refer to cached rows and must be treated as read-only. Zero-quantity
// rows and stock placeholders with ConID zero are omitted. A disconnected
// Connector returns nil, nil; freshness is not implied by a non-empty result.
func (c *Connector) CachedPositions() ([]*RawPosition, error) {
	if !c.isConnected() {
		return nil, nil
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, nil
	}
	ibkrPositions := conn.GetPositions()
	result := c.filteredCachedPositions(ibkrPositions)
	if len(result) == 0 {
		// Self-heal a dead portfolio stream behind the read: consumers
		// poll this path constantly, so a failed account-updates
		// subscription recovers within a poll cycle.
		c.maybeResubscribeAccountUpdates()
	}
	return result, nil
}

// CachedPositionsWithHealth returns the same read-only cached rows as
// [Connector.CachedPositions] together with the latest stream completion and
// heartbeat receipts. It performs no positions snapshot request; a typed
// account-scope conflict may trigger the throttled stream resubscribe behind
// the read. A disconnected Connector returns nil rows, zero health, and a nil
// error.
func (c *Connector) CachedPositionsWithHealth() ([]*RawPosition, PortfolioStreamHealth, error) {
	if !c.isConnected() {
		return nil, PortfolioStreamHealth{}, nil
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, PortfolioStreamHealth{}, nil
	}
	ibkrPositions, health := conn.GetPositionsWithPortfolioHealth()
	result := c.filteredCachedPositions(ibkrPositions)
	if !health.ScopeConflictAt.IsZero() || !health.InvalidPayloadAt.IsZero() {
		c.maybeResubscribeAccountUpdatesForScopeConflict()
		_, health = conn.GetPositionsWithPortfolioHealth()
	} else if len(result) == 0 && accountSummaryShowsPositions(conn.GetAccountSummary()) {
		c.maybeResubscribeAccountUpdates()
		_, health = conn.GetPositionsWithPortfolioHealth()
	}
	return result, health, nil
}

func (c *Connector) filteredCachedPositions(ibkrPositions map[string]*RawPosition) []*RawPosition {
	c.seedContractCacheFromPositions(ibkrPositions)
	result := make([]*RawPosition, 0, len(ibkrPositions))
	for _, pos := range ibkrPositions {
		if pos == nil {
			continue
		}
		if pos.Position == 0 {
			continue
		}
		if pos.Contract.SecType == "STK" && pos.Contract.ConID == 0 {
			continue
		}
		// Inactive marks never hide a held row: for a true delisting the
		// row is zero-value and stays visible anyway, so a mark-based skip
		// here fired almost exclusively on FALSE marks — silently hiding
		// healthy holdings during gateway-wide degradation (2026-07-08).
		// A possibly-stale price beats a vanished position.
		result = append(result, pos)
	}
	return result
}

func isZeroValueStockPosition(pos *RawPosition) bool {
	if pos == nil || !strings.EqualFold(pos.Contract.SecType, "STK") {
		return false
	}
	if pos.Position == 0 {
		return false
	}
	return pos.MarketPrice <= 0 && math.Abs(pos.MarketValue) < 1e-9
}

// RefreshPositions issues the broker's singleton positions request, waits up
// to timeout for its end marker, and returns the filtered cache shape described
// by [Connector.CachedPositions]. Because the protocol supplies no request ID,
// callers must serialize refreshes.
func (c *Connector) RefreshPositions(timeout time.Duration) ([]*RawPosition, error) {
	if !c.isConnected() {
		return nil, ErrIBKRUnavailable
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return nil, ErrIBKRUnavailable
	}
	if err := conn.RequestPositions(); err != nil {
		return nil, err
	}
	if err := conn.WaitForPositionsEnd(timeout); err != nil {
		return nil, err
	}
	return c.filteredCachedPositions(conn.GetPositionsSnapshot()), nil
}

// registerHandlers sets up message handlers for IBKR responses.
// We keep handlers simple and thread-safe, and register them before any
// subscriptions so early messages (e.g., farm notices, nextValidId) are handled.
func (c *Connector) registerHandlers(conn *Connection) {
	if conn == nil {
		return
	}

	// Debug, not info: this fires once per Connection object, and the daemon
	// builds one per connect attempt — 4,191 of them on 2026-07-30 against 12
	// connections that actually handshaked, which made this the largest single
	// message in the log. A session that matters announces itself through
	// "Starting handshake" and "Connection established"; count those, never
	// this line.
	c.logDebug("Registering message handlers")
	conn.setOpenOrderSnapshotObserver(func(msgID int, fields []string, epoch uint64) {
		switch msgID {
		case msgOpenOrder:
			ev, ok := ParseOrderLifecycleEvent(fields)
			if !ok || ev.WhatIf || ev.Type != OrderLifecycleEventOpenOrder || conn.IsWhatIfOrderID(ev.OrderID) {
				return
			}
			c.collectOpenOrderSnapshotFrom(conn, epoch, ev)
		case msgOpenOrderEnd:
			c.finishOpenOrderSnapshotFrom(conn, epoch)
		}
	})

	// Register tick price handler (msgID 1)
	conn.RegisterHandler(1, func(fields []string) {
		c.handleTickPrice(fields)
	})

	// Register tick size handler (msgID 2)
	conn.RegisterHandler(2, func(fields []string) {
		c.handleTickSize(fields)
	})

	// Register generic tick handler (msgID 45) for items like option IV (106)
	conn.RegisterHandler(45, func(fields []string) {
		c.handleTickGeneric(fields)
	})

	// Register string tick handler (msgID 46) for values such as last
	// trade timestamp (tick type 45), used by quote/watchlist as-of text.
	conn.RegisterHandler(msgTickString, func(fields []string) {
		c.handleTickString(fields)
	})

	// Register option computation handler (msgID 21) for greeks and model IV
	conn.RegisterHandler(msgTickOptionComputation, func(fields []string) {
		c.handleOptionComputation(fields)
	})

	// Register historical data handler (msgID 17) for HMDS backfill
	conn.RegisterHandler(msgHistoricalData, func(fields []string) {
		c.handleHistoricalData(fields)
	})

	// Register historical data end handler (msgID 108) to finalize empty results
	conn.RegisterHandler(msgHistoricalDataEnd, func(fields []string) {
		c.handleHistoricalDataEnd(fields)
	})

	// Register the Connector-owned error handler separately so any returned
	// outbound cleanup runs only after Connection releases its inbound epoch
	// lease. Legacy externally registered handlers still use msgID 4 below.
	conn.setErrorPostActionHandler(func(fields []string, epoch uint64) func() {
		return c.handleIBKRErrorFrom(ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}, fields)
	})
	conn.RegisterHandler(msgErrMsg, func([]string) {})

	// Register position handler (msgID 61)
	conn.RegisterHandler(61, func(fields []string) {
		c.handlePosition(fields)
	})

	// Register portfolio value handler (msgID 7)
	conn.RegisterHandler(7, func(fields []string) {
		c.handlePortfolioValue(fields)
	})

	// Register order lifecycle handlers (openOrder/orderStatus/execDetails).
	conn.RegisterHandlerAtEpoch(msgOpenOrder, func(fields []string, epoch uint64) {
		c.notifyOrderLifecycleFrom(conn, epoch, fields)
	})
	conn.RegisterHandlerAtEpoch(msgOrderStatus, func(fields []string, epoch uint64) {
		c.notifyOrderLifecycleFrom(conn, epoch, fields)
	})
	conn.RegisterHandlerAtEpoch(msgExecDetails, func(fields []string, epoch uint64) {
		c.notifyOrderLifecycleFrom(conn, epoch, fields)
	})
	conn.RegisterHandler(msgOpenOrderEnd, func(fields []string) {
		// The epoch-aware Connection observer owns snapshot completion.
	})

	// Register system notification handler (msgID 204) for farm status changes
	conn.RegisterHandler(204, func(fields []string) {})
	conn.SetSystemNoticeHandlerAtEpochWithPostAction(func(note *systemNotification, alias reqAliasEntry, epoch uint64) func() {
		return c.processSystemNoticeFrom(ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}, alias, note)
	})

	// Daily P&L streams: msgPnL (94) for account-level, msgPnLSingle (95)
	// for per-conId. Subscriptions are owned by Connector.SubscribeAccountPnL
	// / Connector.SubscribePositionDailyPnL; handlers update the connector's
	// pnl cache for non-blocking reads by AccountDailyPnL / PositionDailyPnL.
	conn.RegisterHandler(msgPnL, func(fields []string) {
		c.handlePnL(fields)
	})
	conn.RegisterHandler(msgPnLSingle, func(fields []string) {
		c.handlePnLSingle(fields)
	})
}

// handleTickPrice processes tick price updates.
// Accepts [msgID, version, reqID, tickType, price, ...] and updates the
// associated subscription (bid/ask/last) and freshness timestamp.
func (c *Connector) handleTickPrice(fields []string) {
	if len(fields) < 4 {
		return
	}

	// Format: [msgID, version, reqID, tickType, price, ...]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	priceStr := strings.TrimSpace(fields[4])
	// Parse reqID with validation. strconv.Atoi is ~10× cheaper than
	// fmt.Sscanf on this per-tick hot path (no reflection, no format
	// machinery) — the field is a small ASCII integer.
	reqID, err := strconv.Atoi(reqIDStr)
	if err != nil || reqID == 0 {
		marketDataLogger.Warnf("Invalid reqID in tick price: %q (error: %v)", reqIDStr, err)
		return
	}

	// Find the symbol for this request ID
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()

	// Parse tickType with validation
	tickType, err := strconv.Atoi(tickTypeStr)
	if err != nil {
		if exists {
			marketDataLogger.Warnf("Invalid tickType in tick price for reqID %d: %q (error: %v)", reqID, tickTypeStr, err)
		}
		return
	}

	// Handle empty price payload (IBKR sends blank string for stale ticks)
	if priceStr == "" {
		if exists {
			c.subMu.Lock()
			if sub, ok := c.subscriptions[symbol]; ok {
				sub.LastTime = time.Now()
				sub.LastTickAt = sub.LastTime
			}
			c.subMu.Unlock()
		}
		return
	}

	// Parse price with validation
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil {
		if exists {
			marketDataLogger.Warnf("Invalid price in tick price for reqID %d: %q (error: %v)", reqID, priceStr, err)
		}
		return
	}

	if !exists {
		// Unknown reqID - might be from previous session or automatic subscription
		// ReqID 6 appears to be an automatic subscription from IBKR for account positions
		if reqID != 6 {
			marketDataLogger.Debugf("Received tick for unknown reqID %d", reqID)
		}
		return
	}

	// Log all market data for debugging with comprehensive tick type mapping
	tickTypeName := "unknown"
	isImportantTick := false
	switch tickType {
	case 1:
		tickTypeName = "bid"
		isImportantTick = true
	case 2:
		tickTypeName = "ask"
		isImportantTick = true
	case 4:
		tickTypeName = "last"
		isImportantTick = true
	case 6:
		tickTypeName = "high"
	case 7:
		tickTypeName = "low"
	case 9:
		tickTypeName = "close"
	case 14:
		tickTypeName = "open"
	case 15:
		tickTypeName = "low_13_weeks"
	case 16:
		tickTypeName = "high_13_weeks"
	case 17:
		tickTypeName = "low_26_weeks"
	case 18:
		tickTypeName = "high_26_weeks"
	case 19:
		tickTypeName = "low_52_weeks"
	case 20:
		tickTypeName = "high_52_weeks"
	case 37:
		tickTypeName = "mark_price"
		isImportantTick = true
	case 66:
		tickTypeName = "delayed_bid"
		isImportantTick = true
	case 67:
		tickTypeName = "delayed_ask"
		isImportantTick = true
	case 68:
		tickTypeName = "delayed_last"
		isImportantTick = true
	case 72:
		tickTypeName = "delayed_high"
	case 73:
		tickTypeName = "delayed_low"
	case 75:
		tickTypeName = "delayed_close"
	case 76:
		tickTypeName = "delayed_open"
	case 221:
		tickTypeName = "mark_price_slow"
	case 225:
		tickTypeName = "auction_data"
		marketDataLogger.Infof("%s AUCTION DATA received (tick 225): %.2f", symbol, price)
	case 232:
		tickTypeName = "last_yield"
	case 233:
		tickTypeName = "rt_volume"
	default:
		tickTypeName = fmt.Sprintf("tick_%d", tickType)
	}

	// If this tick belongs to an option reqID, capture the option quote
	// caches and the option subscription snapshot. Do not fall through to
	// the regular path: gamma fan-outs can touch thousands of option legs,
	// and the regular path logs every important bid/ask tick.
	c.optMu.RLock()
	optSym, isOptionReq := c.optReqIDs[reqID]
	c.optMu.RUnlock()
	if isOptionReq {
		c.subMu.Lock()
		if sub, ok := c.subscriptions[optSym]; ok {
			sub.LastTime = time.Now()
			sub.LastTickAt = sub.LastTime
			if price > 0 {
				sub.Observed = true
				switch tickType {
				case 1, 66:
					sub.Bid = price
				case 2, 67:
					sub.Ask = price
				case 4, 68:
					sub.LastPrice = price
				case 9, 75:
					sub.PrevClose = price
				case 37:
					sub.MarkPrice = price
				}
			}
		}
		c.subMu.Unlock()

		if price > 0 {
			c.optMu.Lock()
			switch tickType {
			case 1, 66:
				c.optQuoteBid[optSym] = price
			case 2, 67:
				c.optQuoteAsk[optSym] = price
			case 9, 75:
				// Per-contract previous close (the option's own prior settle,
				// NOT the underlying's). Used for option-level daily P&L
				// attribution; without this, callers fall back to the
				// underlying's prev close which is meaningless for an option.
				c.optPrevClose[optSym] = price
			}
			c.optMu.Unlock()
		}
		return
	}

	// Log important ticks only if price > 0
	if (isImportantTick || tickType == 225) && price > 0 {
		marketDataLogger.Debugf("%s %s: %.2f", symbol, tickTypeName, price)
	}

	// Update subscription data based on tick type
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		return
	}

	// Validate price before updating: reject zero and negative prices to prevent
	// overwriting valid prices with "no quote available" indicators from IBKR.
	// Exception: Very small but positive prices (e.g., 0.0001 for penny stocks) are allowed.
	if price <= 0 {
		// Update LastTime to show we received a tick, but don't update the price
		sub.LastTime = time.Now()
		sub.LastTickAt = sub.LastTime
		return
	}

	// Mark subscription observed once we accept a valid price
	sub.Observed = true

	// Tick types: 1=bid, 2=ask, 4=last, 6=high, 7=low, 9=close, 14=open.
	// Delayed subscriptions use 66/67/68/72/73/75/76 for the same fields.
	// Close (9) is yesterday's regular-session close — the anchor for
	// change-vs-prev-close. IBKR sends it automatically once per reqMktData,
	// regardless of generic-tick flags.
	//
	// Tick types 15-20 (week-range highs/lows) only arrive when the
	// streaming subscribe asked for generic tick 165 (Misc Stats). They are
	// load-bearing for the scan-row 52w column; without them the column
	// would be silently blank for symbols that didn't have an explicit
	// historical-bars fetch.
	switch tickType {
	case 1, 66:
		sub.Bid = price
	case 2, 67:
		sub.Ask = price
	case 4, 68:
		sub.LastPrice = price
	case 6, 72:
		sub.High = price
	case 7, 73:
		sub.Low = price
	case 9, 75:
		sub.PrevClose = price
	case 14, 76:
		sub.Open = price
	case 15:
		sub.Week13Low = price
	case 16:
		sub.Week13High = price
	case 17:
		sub.Week26Low = price
	case 18:
		sub.Week26High = price
	case 19:
		sub.Week52Low = price
	case 20:
		sub.Week52High = price
	case 37:
		// Mark price — see Subscription.MarkPrice. For indices this is
		// often the only price tick the gateway sends.
		sub.MarkPrice = price
	}
	sub.LastTime = time.Now()
	sub.LastTickAt = sub.LastTime
}

// handleTickGeneric processes generic tick updates. The wire tick ids
// differ from the generic-tick REQUEST list ids: requesting generic tick
// 106 delivers wire tick 24 (chain-averaged option implied vol of the
// underlying), and requesting generic tick 236 delivers wire tick 46
// (shortable difficulty level) plus wire tick 89 (shortable share count,
// a tickSize — see handleTickSize). An earlier revision matched the
// request ids (106/236) here, which never appear as wire tick types, so
// both surfaces were silently dead: quote IV stayed null and borrow
// inventory reported "unknown" for every symbol (observed 2026-06-11).
// Wire tick 46 is deliberately not handled: its 0–3 difficulty float
// cannot feed the share-count thresholds, and storing it as a share
// count would fire false Borrow-scarce flags.
func (c *Connector) handleTickGeneric(fields []string) {
	// Expected format: [msgID, version, reqID, tickType, value]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	valueStr := fields[4]

	reqID, _ := strconv.Atoi(reqIDStr)
	tickType, _ := strconv.Atoi(tickTypeStr)
	val, _ := strconv.ParseFloat(valueStr, 64)

	// Map reqID to underlying symbol
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()
	if !exists {
		return
	}

	switch {
	case tickType == 24 && val > 0:
		// 24 = Option Implied Volatility (averaged across the chain — the
		// "IV of the underlying" that retail platforms display), delivered
		// for the generic-tick-106 request.
		iv := val
		if iv > 1.5 { // normalize percent inputs
			iv = iv / 100.0
		}
		c.optMu.Lock()
		c.optIV[symbol] = iv
		c.optMu.Unlock()
		// Also write to the per-symbol subscription so MarketDataSnapshot sees
		// it without having to consult the option-IV cache separately —
		// scan-row enrichment and `quote --json` both read from there.
		c.subMu.Lock()
		if sub, ok := c.subscriptions[symbol]; ok {
			sub.IV = iv
			sub.LastTime = time.Now()
			sub.LastTickAt = sub.LastTime
		}
		c.subMu.Unlock()
	}
}

// handleOptionComputation processes IBKR option computation ticks (msgID 21),
// which provide implied volatility, greeks, and theoretical prices for option
// contracts.
//
// Wire format for IBKR server version ≥ MIN_SERVER_VER_PRICE_BASED_VOLATILITY
// (= 165):
//
//	[msgID, reqID, tickType, tickAttrib, impliedVol, delta, optPrice,
//	 pvDividend, gamma, vega, theta, underlyingPrice]
//
// The connection enforces serverVersion ≥ 124 (`minServerVersionRequired`),
// and current TWS / IB Gateway builds report 200+, so callers always see
// this layout. fields[1]=reqID, fields[2]=tickType, fields[3]=tickAttrib
// (option-computation flags; not consumed yet).
func (c *Connector) handleOptionComputation(fields []string) {
	// New format has 12 fields (legacy had 12 too, but the meaning shifted).
	// The trailing space in IBKR's wire-encoded frame can produce a 13th
	// empty token after Split; accept ≥ 12.
	if len(fields) < 12 {
		return
	}

	reqID, err := strconv.Atoi(fields[1])
	if err != nil {
		return
	}
	tickType, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}

	parseFloat := func(s string) float64 {
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return math.NaN()
		}
		return v
	}

	// fields[3] is tickAttrib (option computation flags); not consumed yet.
	impliedVol := parseFloat(fields[4])
	delta := parseFloat(fields[5])
	optionPrice := parseFloat(fields[6])
	gamma := parseFloat(fields[8])
	vega := parseFloat(fields[9])
	theta := parseFloat(fields[10])
	underlyingPrice := parseFloat(fields[11])

	c.optMu.Lock()
	symbol, exists := c.optReqIDs[reqID]
	if !exists {
		c.optMu.Unlock()
		return
	}

	switch tickType {
	case 10: // bid computation
		if optionPrice > 0 {
			c.optQuoteBid[symbol] = optionPrice
		}
	case 11: // ask computation
		if optionPrice > 0 {
			c.optQuoteAsk[symbol] = optionPrice
		}
	case 13: // model computation — canonical source for greeks
		if impliedVol > 0 {
			if impliedVol > 1.5 {
				impliedVol /= 100.0
			}
			c.optIV[symbol] = impliedVol
		}
		// IBKR sends a NaN/sentinel-tagged Greeks row when the model hasn't
		// priced the contract yet (typical for far OTM / illiquid OOH). We
		// only commit Greeks once at least one component is sane — never
		// fabricate zeros where the model abstained.
		g, ok := c.optGreeks[symbol]
		if !ok {
			g = Greeks{}
		}
		if saneGreek(delta, 1.05) { // delta bounded by 1; tiny slack for binomial drift
			g.Delta = delta
		}
		if saneGreek(gamma, 10) {
			g.Gamma = gamma
		}
		if saneGreek(theta, 1e6) {
			g.Theta = theta
		}
		if saneGreek(vega, 1e6) {
			g.Vega = vega
		}
		if g != (Greeks{}) {
			c.optGreeks[symbol] = g
		}
		if !math.IsNaN(underlyingPrice) && underlyingPrice > 0 && underlyingPrice < 1e9 {
			c.optUnderlyingPx[symbol] = underlyingPrice
		}
	}

	c.optMu.Unlock()
}

// saneGreek rejects NaN and IBKR's MaxFloat-style sentinel values that fire
// when the gateway emits a Greeks row before the model has priced the
// contract. bound is an asset-class-aware upper magnitude.
func saneGreek(v, bound float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	if math.Abs(v) > bound {
		return false
	}
	return true
}

// SubscriptionRejectCh returns the terminal-rejection channel for a tracked
// subscription key. The channel may receive at most one pending rejection and
// is not closed when the subscription ends. It returns nil when the key is not
// tracked or rejection signaling is unavailable; a nil channel can be used
// directly in a select to disable that case.
func (c *Connector) SubscriptionRejectCh(key string) <-chan SubscriptionRejection {
	if key == "" {
		return nil
	}
	c.subMu.RLock()
	defer c.subMu.RUnlock()
	sub, ok := c.subscriptions[strings.ToUpper(key)]
	if !ok || sub == nil {
		return nil
	}
	return sub.RejectCh
}

// pushSubscriptionRejection signals fast-abort to any in-flight poller
// watching this reqID's subscription. Non-blocking: the channel is
// buffered 1 and we drop on a full buffer so the error-handler goroutine
// never stalls. The drop is benign — the consumer's first read already
// carries the abort signal, and the specific code/message of subsequent
// rejections does not matter to the poller (every terminal code means
// "this subscription will never produce ticks").
//
// Looked up via reqIDMap (the same lookup handleIBKRError already does
// for recovery routing). Subscriptions created without a channel (test
// fixtures, scaffolding) silently no-op.
func (c *Connector) pushSubscriptionRejection(reqID, code int, message string) {
	if reqID <= 0 || !isTerminalSubscriptionError(code) {
		return
	}
	c.subMu.RLock()
	var ch chan SubscriptionRejection
	if sym, ok := c.reqIDMap[reqID]; ok {
		if sub, ok := c.subscriptions[sym]; ok && sub != nil {
			ch = sub.RejectCh
		}
	}
	c.subMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- SubscriptionRejection{Code: code, Message: message}:
	default:
	}
}

// handleIBKRError receives raw IBKR error messages for proactive recovery.
// fields: [msgID=4, version, reqID, errorCode, errorMsg]
func (c *Connector) handleIBKRError(fields []string) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	origin := ConnectorSessionBinding{connector: c, connection: conn}
	if conn != nil {
		origin.epoch = conn.BrokerSessionEpoch()
	}
	if post := c.handleIBKRErrorFrom(origin, fields); post != nil {
		post()
	}
}

func (c *Connector) handleIBKRErrorFrom(origin ConnectorSessionBinding, fields []string) (postBarrier func()) {
	if len(fields) < 4 {
		return
	}
	// Parse reqID and code
	reqID := 0
	if len(fields) > 2 {
		if v, err := strconv.Atoi(fields[2]); err == nil {
			reqID = v
		}
	}
	code := 0
	if len(fields) > 3 {
		if v, err := strconv.Atoi(fields[3]); err == nil {
			code = v
		}
	}
	rawMsg := ""
	if len(fields) > 4 {
		rawMsg = fields[4]
	}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.RLock()
	defer c.evidenceBarrier.RUnlock()
	if !c.SessionReceiptCurrent(origin) {
		c.notifyOrderErrorLifecycleUnderBarrier(origin, reqID, code, rawMsg, "")
		return
	}
	if reqID > 0 && c.failPendingExactHistoricalRoute(reqID, code, rawMsg) {
		c.recordDataFarmNotice(code, rawMsg, time.Now())
		return
	}

	// Fast-abort signal for in-flight pollers. Sent first so a code-200
	// rejection on a per-leg market-data subscription unblocks the
	// consumer before any of the recovery branches below (some of which
	// take subMu in write mode or kick off background refreshes). The
	// Connection layer has already released this reqID's market-data
	// slot in handleErrorMessage; the signal here is the consumer-side
	// counterpart.
	rejectionMsg := ""
	if len(fields) > 4 {
		rejectionMsg = fields[4]
	}
	c.pushSubscriptionRejection(reqID, code, rejectionMsg)

	// Map to symbol if available (subscriptions or historical request)
	symbol := ""
	histPending := false
	histOwnsNoticeCollision := false
	if reqID > 0 {
		c.subMu.RLock()
		symbol = c.reqIDMap[reqID]
		c.subMu.RUnlock()
		if symbol == "" {
			c.historicalMu.Lock()
			if hr, ok := c.historicalReqs[reqID]; ok {
				symbol = hr.symbol
				histPending = true
				histOwnsNoticeCollision = hr.requestOwnsNoticeCollision
			}
			c.historicalMu.Unlock()
		}
		if symbol == "" {
			if alias, ok := c.conn.lookupReqAlias(reqID); ok && alias.symbol != "" {
				symbol = alias.symbol
			}
		}
	}

	// The legacy msgErrMsg path shares the same multiplexed order/request-id
	// collision as msg-204. A FEE_RATE request explicitly owns this closed set
	// of request codes, so do not fabricate an order error when the integers
	// overlap. Explicit order codes remain order-owned.
	if !(histPending && histOwnsNoticeCollision && historicalNoticeOwnsIDCollision(code)) {
		c.notifyOrderErrorLifecycleUnderBarrier(origin, reqID, code, rawMsg, "")
	}
	c.recordDataFarmNotice(code, rawMsg, time.Now())
	upperMsg := strings.ToUpper(rawMsg)
	upperSymbol := strings.ToUpper(symbol)
	if symbol != "" && symbol != upperSymbol {
		symbol = upperSymbol
	}
	parserMisalign := strings.Contains(upperMsg, "MART") ||
		strings.Contains(upperMsg, "'BOE") || strings.Contains(upperMsg, "\"BOE") || strings.Contains(upperMsg, " BOE")
	if parserMisalign {
		context := c.parserContext(symbol)
		if context != "" {
			ibkrLogger.Errorf("[IBKR] Parser misalignment detected (code=%d reqID=%d symbol=%s): %s | frame=%s", code, reqID, symbol, rawMsg, context)
		} else {
			ibkrLogger.Errorf("[IBKR] Parser misalignment detected (code=%d reqID=%d symbol=%s): %s", code, reqID, symbol, rawMsg)
		}
	}

	// If this error targets an outstanding historical request, fail it immediately
	if histPending {
		if code == 0 || code == -1 || (code >= 2100 && code < 2200) {
			return // informational notices
		}
		msg := rawMsg
		hErr := &HistoricalRequestError{Code: code, Message: msg}
		switch code {
		case 162:
			if hErr.Message == "" {
				hErr.Message = "historical data pacing violation"
			}
			hErr.RetryAfter = c.nextHistoricalBackoff(symbol)
		case 321:
			if hErr.Message == "" {
				hErr.Message = "historical data request failed validation"
			}
			c.resetHistoricalBackoff(symbol)
		default:
			c.resetHistoricalBackoff(symbol)
		}
		c.failHistoricalRequest(reqID, hErr)
		if symbol != "" {
			if code == 366 || (code == 162 && strings.Contains(upperMsg, "NO DATA")) {
				_, postBarrier = c.registerInactiveCandidatePostAction(symbol, rawMsg)
			}
		}
		return
	}

	if code == 200 && symbol != "" {
		if strings.Contains(upperMsg, "NO SECURITY DEFINITION HAS BEEN FOUND") {
			marked, post := c.registerInactiveCandidatePostAction(symbol, rawMsg)
			postBarrier = joinPostActions(postBarrier, post)
			if marked {
				return
			}
		}
	}

	switch code {
	case 2108: // Market data farm disconnected
		// Mark subs unobserved to force refresh path
		c.subMu.Lock()
		for _, sub := range c.subscriptions {
			sub.Observed = false
		}
		c.subMu.Unlock()
	case 2119, 2104, 200, 320, 321, 354:
		// Never launch an unbound recovery request from an inbound socket
		// callback. Ordinary pollers own resubscription after current-session
		// state has settled; a retired reader therefore cannot write on its
		// successor socket.
	}
	return postBarrier
}

func (c *Connector) parserContext(symbol string) string {
	conn := c.conn
	if conn == nil {
		return ""
	}
	return conn.parserContext(symbol)
}

func (c *Connector) handleHistoricalData(fields []string) {
	if len(fields) < 2 {
		return
	}

	// For serverVersion >= 124, no version field in historical data messages
	// (We require minimum serverVersion 124, so version field never present)
	idx := 1

	if idx >= len(fields) {
		return
	}

	reqID, err := strconv.Atoi(fields[idx])
	if err != nil {
		return
	}
	idx++

	req := c.getHistoricalRequest(reqID)
	if req == nil {
		return
	}
	if req.requireEpoch && !c.HistoricalSessionCurrent(HistoricalSessionBinding{connector: c, connection: req.connection, epoch: req.epoch}) {
		c.failHistoricalRequest(reqID, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable})
		return
	}

	serverVersion := 0
	if c.conn != nil {
		serverVersion = c.conn.ServerVersion()
	}
	legacyFormat := false
	if serverVersion > 0 {
		legacyFormat = serverVersion < minServerVerHistoricalDataEnd
	} else if idx < len(fields) {
		// Auto-detect: if next field is non-numeric treat as legacy header
		if _, err := strconv.Atoi(fields[idx]); err != nil {
			legacyFormat = true
		}
	}

	start := ""
	end := ""
	if legacyFormat {
		start = safeField(fields, &idx)
		end = safeField(fields, &idx)
	}

	count := 0
	var countErr error
	if idx < len(fields) {
		if v, err := strconv.Atoi(fields[idx]); err == nil {
			count = v
		} else {
			countErr = err
		}
		idx++
	}
	if req.strictDaily && (countErr != nil || count < 0) {
		c.failHistoricalRequest(reqID, &HistoricalDataValidationError{Reason: "invalid_bar_count"})
		return
	}
	bars, parseErr := parseHistoricalBars(fields, &idx, count, req.strictDaily)
	if parseErr != nil {
		c.failHistoricalRequest(reqID, parseErr)
		return
	}
	if req.strictDaily && idx != len(fields) {
		c.failHistoricalRequest(reqID, &HistoricalDataValidationError{Reason: "trailing_payload"})
		return
	}

	result := historicalResult{
		start: start,
		end:   end,
		bars:  bars,
	}
	if req.waitForEnd && !legacyFormat {
		if err := c.bufferHistoricalResult(reqID, result); err != nil {
			c.failHistoricalRequest(reqID, err)
		}
		return
	}
	c.completeHistoricalRequest(reqID, result)
}

func parseHistoricalBars(fields []string, idx *int, count int, strictDaily bool) ([]HistoricalBar, error) {
	bars := make([]HistoricalBar, 0, max(count, 0))
	for range count {
		if *idx >= len(fields) {
			if strictDaily {
				return nil, &HistoricalDataValidationError{Reason: "truncated_bar"}
			}
			break
		}

		dateStr := fields[*idx]
		*idx++
		// Require six scalar fields (open, high, low, close, volume, average)
		// plus barCount.
		if *idx+6 >= len(fields) {
			if strictDaily {
				return nil, &HistoricalDataValidationError{Reason: "truncated_bar"}
			}
			break
		}

		values := [6]float64{}
		for i := range values {
			if strictDaily {
				value, err := strconv.ParseFloat(strings.TrimSpace(fields[*idx]), 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
					return nil, &HistoricalDataValidationError{Reason: "invalid_numeric"}
				}
				if i < 4 && value < 0 {
					return nil, &HistoricalDataValidationError{Reason: "invalid_ohlc"}
				}
				values[i] = value
			} else {
				values[i] = parseFloat(fields[*idx])
			}
			*idx++
		}
		openVal, highVal, lowVal, closeVal := values[0], values[1], values[2], values[3]
		if strictDaily && (highVal < lowVal || highVal < openVal || highVal < closeVal || lowVal > openVal || lowVal > closeVal) {
			return nil, &HistoricalDataValidationError{Reason: "incoherent_ohlc"}
		}

		barCount := 0
		if *idx < len(fields) {
			if value, err := strconv.Atoi(fields[*idx]); err == nil {
				barCount = value
			} else if strictDaily {
				return nil, &HistoricalDataValidationError{Reason: "invalid_bar_count"}
			}
			*idx++
		}

		barTime, timeErr := parseHistoricalTimestamp(dateStr)
		if strictDaily && timeErr != nil {
			return nil, &HistoricalDataValidationError{Reason: "invalid_daily_as_of"}
		}
		bars = append(bars, HistoricalBar{
			Time:     barTime,
			Date:     dateStr,
			Open:     openVal,
			High:     highVal,
			Low:      lowVal,
			Close:    closeVal,
			Volume:   int64(values[4]),
			Average:  values[5],
			BarCount: barCount,
		})
	}
	return bars, nil
}

func (c *Connector) handleHistoricalDataEnd(fields []string) {
	if len(fields) < 3 {
		return
	}

	idx := 1
	if len(fields) > idx {
		if _, err := strconv.Atoi(fields[idx]); err == nil {
			idx++
		}
	}
	if idx >= len(fields) {
		return
	}

	reqID, err := strconv.Atoi(fields[idx])
	if err != nil {
		return
	}
	idx++
	if req := c.getHistoricalRequest(reqID); req != nil && req.requireEpoch &&
		!c.HistoricalSessionCurrent(HistoricalSessionBinding{connector: c, connection: req.connection, epoch: req.epoch}) {
		c.failHistoricalRequest(reqID, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable})
		return
	}

	start := ""
	if idx < len(fields) {
		start = fields[idx]
		idx++
	}
	end := ""
	if idx < len(fields) {
		end = fields[idx]
		idx++
	}
	if req := c.getHistoricalRequest(reqID); req != nil && req.strictDaily && idx != len(fields) {
		c.failHistoricalRequest(reqID, &HistoricalDataValidationError{Reason: "trailing_payload"})
		return
	}

	c.historicalMu.Lock()
	req := c.historicalReqs[reqID]
	result := historicalResult{start: start, end: end}
	if req != nil {
		result.bars = slices.Clone(req.bufferedBars)
		result.err = req.bufferedErr
	}
	c.historicalMu.Unlock()
	c.completeHistoricalRequest(reqID, result)
}

func safeField(fields []string, idx *int) string {
	if *idx >= len(fields) {
		return ""
	}
	val := fields[*idx]
	*idx = *idx + 1
	return val
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseHistoricalTimestamp(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("empty historical timestamp")
	}

	normalized := strings.ReplaceAll(raw, "  ", " ")

	layouts := []string{
		"20060102 15:04:05",
		"20060102",
	}

	for _, layout := range layouts {
		if ts, err := time.ParseInLocation(layout, normalized, time.UTC); err == nil {
			return ts, nil
		}
	}
	if epoch, err := strconv.ParseInt(normalized, 10, 64); err == nil && epoch > 0 {
		return time.Unix(epoch, 0).UTC(), nil
	}

	return time.Time{}, fmt.Errorf("unable to parse historical timestamp: %s", raw)
}

func (c *Connector) getHistoricalRequest(reqID int) *historicalRequest {
	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()
	return c.historicalReqs[reqID]
}

func (c *Connector) createHistoricalRequest(reqID int, symbol string) *historicalRequest {
	return c.createHistoricalRequestWithOptions(reqID, symbol, historicalRequestOptions{})
}

type historicalRequestOptions struct {
	strictDaily                bool
	waitForEnd                 bool
	requestOwnsNoticeCollision bool
	formatDate                 int
	session                    HistoricalSessionBinding
	requireEpoch               bool
}

func (c *Connector) createHistoricalRequestWithOptions(reqID int, symbol string, options historicalRequestOptions) *historicalRequest {
	req := &historicalRequest{
		symbol:                     symbol,
		result:                     make(chan historicalResult, 1),
		strictDaily:                options.strictDaily,
		waitForEnd:                 options.waitForEnd,
		requestOwnsNoticeCollision: options.requestOwnsNoticeCollision,
		connection:                 options.session.connection,
		epoch:                      options.session.epoch,
		requireEpoch:               options.requireEpoch,
	}
	c.historicalMu.Lock()
	c.historicalReqs[reqID] = req
	c.historicalMu.Unlock()
	return req
}

func (c *Connector) bufferHistoricalResult(reqID int, res historicalResult) error {
	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()
	req := c.historicalReqs[reqID]
	if req == nil {
		return nil
	}
	if req.strictDaily {
		seen := make(map[string]struct{}, len(req.bufferedBars)+len(res.bars))
		for _, bar := range req.bufferedBars {
			seen[bar.Time.UTC().Format("2006-01-02")] = struct{}{}
		}
		for _, bar := range res.bars {
			date := bar.Time.UTC().Format("2006-01-02")
			if _, duplicate := seen[date]; duplicate {
				return &HistoricalDataValidationError{Reason: "duplicate_session_date"}
			}
			seen[date] = struct{}{}
		}
	}
	req.bufferedBars = append(req.bufferedBars, res.bars...)
	if res.err != nil {
		req.bufferedErr = res.err
	}
	return nil
}

func (c *Connector) completeHistoricalRequest(reqID int, res historicalResult) {
	c.historicalMu.Lock()
	req, ok := c.historicalReqs[reqID]
	if ok {
		delete(c.historicalReqs, reqID)
	}
	c.historicalMu.Unlock()
	if !ok {
		return
	}
	req.result <- res
	close(req.result)
	if len(res.bars) > 0 {
		c.resetHistoricalBackoff(req.symbol)
	}
}

func (c *Connector) failHistoricalRequest(reqID int, err error) {
	c.completeHistoricalRequest(reqID, historicalResult{err: err})
}

func (c *Connector) nextHistoricalBackoff(symbol string) time.Duration {
	const base = 30 * time.Second
	const maxDelay = 5 * time.Minute

	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()

	count := min(c.historicalBackoff[symbol]+1, 10)
	c.historicalBackoff[symbol] = count

	delay := min(base*time.Duration(1<<(count-1)), maxDelay)
	return delay
}

func (c *Connector) resetHistoricalBackoff(symbol string) {
	c.historicalMu.Lock()
	delete(c.historicalBackoff, symbol)
	c.historicalMu.Unlock()
}

func formatHistoricalDuration(lookbackDays int) string {
	if lookbackDays <= 0 {
		return "1 D"
	}
	if lookbackDays <= 365 {
		return fmt.Sprintf("%d D", lookbackDays)
	}
	years := (lookbackDays + 364) / 365
	if years == 1 {
		return "1 Y"
	}
	return fmt.Sprintf("%d Y", years)
}

// FetchHistoricalDailyBars requests daily bars for symbol and waits for the
// historical-data response. A lookbackDays value of zero or less uses 400 days;
// a timeout of zero or less uses 45 seconds. The earlier of timeout and the ctx
// deadline bounds both paced transmission and response waiting. Cancellation
// best-effort cancels an already-sent broker request.
func (c *Connector) FetchHistoricalDailyBars(ctx context.Context, symbol string, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	return c.fetchHistoricalDailyBars(ctx, symbol, lookbackDays, timeout, "")
}

// FetchHistoricalDailyBarsWhatToShow requests daily bars using the normalized
// IBKR whatToShow value supplied by the caller. It does not fall back to another
// feed, so returned bars retain the requested feed provenance. Context,
// lookback, and timeout semantics match [Connector.FetchHistoricalDailyBars].
func (c *Connector) FetchHistoricalDailyBarsWhatToShow(ctx context.Context, symbol string, lookbackDays int, whatToShow string, timeout time.Duration) ([]HistoricalBar, error) {
	cleanWhat, err := normalizeHistoricalWhatToShow(whatToShow)
	if err != nil {
		return nil, err
	}
	return c.fetchHistoricalDailyBars(ctx, symbol, lookbackDays, timeout, cleanWhat)
}

func (c *Connector) fetchHistoricalDailyBars(ctx context.Context, symbol string, lookbackDays int, timeout time.Duration, forceWhatToShow string) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.IsReady() {
		return nil, fmt.Errorf("IBKR connection not ready")
	}

	symbol = strings.ToUpper(symbol)
	if _, inactive := c.inactiveReason(symbol); inactive {
		return nil, ErrSymbolInactive
	}

	if lookbackDays <= 0 {
		lookbackDays = 400
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	timeout, err := historicalTimeoutWithinContext(ctx, timeout)
	if err != nil {
		return nil, err
	}

	secType, exchange, currency, primary := classifySymbol(symbol)
	// Dual-class shares (BRK.B, BF.B) translate to IBKR's space-form
	// before going on the wire — see dualClassWireSymbol. Without this
	// translation IBKR returns code 200 "No security definition has been
	// found for the request" and the breadth fan-out silently drops
	// Berkshire-B (a top-10 SPX member by weight).
	wireSymbol := dualClassWireSymbol(symbol)
	if base, _, ok := FxPair(symbol); ok {
		wireSymbol = base
	}
	baseContract := Contract{
		Symbol:      wireSymbol,
		SecType:     secType,
		Exchange:    exchange,
		PrimaryExch: primary,
		Currency:    currency,
	}

	return c.fetchHistoricalDailyBarsWithBase(ctx, symbol, baseContract, primary, lookbackDays, timeout, true, forceWhatToShow)
}

// FetchHistoricalDailyBarsWithContract requests daily bars using the routing
// fields already present on contract, including exchange, currency, local
// symbol, or ConID. Context, lookback, and timeout semantics match
// [Connector.FetchHistoricalDailyBars].
func (c *Connector) FetchHistoricalDailyBarsWithContract(ctx context.Context, contract Contract, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	return c.fetchHistoricalDailyBarsWithContract(ctx, contract, lookbackDays, timeout)
}

// FetchHistoricalDailyFeeRates requests daily stock-borrow fee-rate bars for
// an exact broker contract. It is intentionally narrower than the general
// historical APIs: FEE_RATE is pinned and ConID is required. A missing
// executable exchange may be completed only by a bounded exact-ConID contract
// details read whose identity and route are strictly checked; the method never
// resolves by symbol, substitutes another ConID, or fabricates SMART.
// Concurrent identical requests share one open HMDS request, and its detached
// typed result is reused for IBKR's 15-second identical-request cooldown.
func (c *Connector) FetchHistoricalDailyFeeRates(ctx context.Context, contract Contract, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	contract = normalizeExactHistoricalContract(contract)
	if contract.ConID <= 0 {
		return nil, &HistoricalDataValidationError{Reason: "missing_contract_id"}
	}
	if contract.Symbol == "" {
		return nil, &HistoricalDataValidationError{Reason: "missing_symbol"}
	}
	if contract.SecType != "STK" {
		return nil, &HistoricalDataValidationError{Reason: "unsupported_security_type"}
	}
	if contract.Currency == "" {
		return nil, &HistoricalDataValidationError{Reason: "incomplete_exact_route"}
	}
	if !HistoricalFeeRateUSRouteSupported(contract, contract.Exchange != "") {
		return nil, &HistoricalDataValidationError{Reason: "unsupported_market_calendar"}
	}
	if lookbackDays <= 0 {
		lookbackDays = 7
	}
	if timeout <= 0 {
		timeout = historicalFeeRateBackendTimeout
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return nil, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}

	key := "fee-rate\x00" + historicalDailyFeeRateKey(contract, lookbackDays)
	flight, leader := c.acquireHistoricalExactFlight(key, binding)
	if leader {
		go c.runHistoricalDailyFeeRateFlight(flight, binding, contract, lookbackDays)
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-flight.done:
		return slices.Clone(flight.bars), cloneSanitizedHistoricalError(flight.err)
	case <-waitCtx.Done():
		return nil, waitCtx.Err()
	}
}

// ResolveExactHistoricalStockRoute completes a missing executable exchange
// only through an exact positive-ConID contract-details request. It rejects
// wrong, missing, or ambiguous broker details and never resolves by symbol or
// substitutes a default route. Callers may retain their original position
// identity separately from the returned route used on the wire.
func (c *Connector) ResolveExactHistoricalStockRoute(ctx context.Context, contract Contract, timeout time.Duration) (Contract, error) {
	return c.resolveExactHistoricalStockRoute(ctx, contract, timeout)
}

func normalizeExactHistoricalContract(contract Contract) Contract {
	contract.Symbol = strings.ToUpper(strings.TrimSpace(contract.Symbol))
	contract.SecType = strings.ToUpper(strings.TrimSpace(contract.SecType))
	contract.Exchange = strings.ToUpper(strings.TrimSpace(contract.Exchange))
	contract.PrimaryExch = strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	contract.Currency = strings.ToUpper(strings.TrimSpace(contract.Currency))
	contract.LocalSymbol = strings.TrimSpace(contract.LocalSymbol)
	contract.TradingClass = strings.TrimSpace(contract.TradingClass)
	contract.SecIDType = strings.TrimSpace(contract.SecIDType)
	contract.SecID = strings.TrimSpace(contract.SecID)
	return contract
}

var historicalFeeRateUSExchanges = map[string]struct{}{
	"AMEX": {}, "ARCA": {}, "NASDAQ": {}, "NYSE": {}, "SMART": {},
}

// HistoricalFeeRateUSRouteSupported restricts FEE_RATE to the embedded U.S.
// cash-equity calendar and a closed set of exact IBKR stock routes. When
// requireExchange is false, an allowlisted primary exchange may be used only
// as a route-resolution hint; the resolved executable exchange is validated
// again before HMDS admission.
func HistoricalFeeRateUSRouteSupported(contract Contract, requireExchange bool) bool {
	contract = normalizeExactHistoricalContract(contract)
	if contract.SecType != "STK" || contract.Currency != "USD" {
		return false
	}
	if contract.PrimaryExch != "" {
		if _, ok := historicalFeeRateUSExchanges[contract.PrimaryExch]; !ok {
			return false
		}
	}
	if contract.Exchange == "" {
		return !requireExchange && contract.PrimaryExch != ""
	}
	_, ok := historicalFeeRateUSExchanges[contract.Exchange]
	return ok
}

// sendExactHistoricalStockRouteRequest is the positive-ConID contract-details
// encoder used only by the FEE_RATE fallback. The socket epoch is checked by
// sendMessageWithTypeContextForEpoch while transportMu is held, immediately
// before writer access; a reconnect can therefore never redirect this request
// onto a new socket.
func (c *Connection) sendExactHistoricalStockRouteRequest(ctx context.Context, contract Contract, reqID int, epoch uint64) error {
	c.registerReqAlias(reqID, contract)
	fields := []any{
		reqContractData,
		8,
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		"",
		contract.Right,
		"",
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass,
		0,
		contract.SecIDType,
		contract.SecID,
		"",
	}
	return c.sendMessageWithTypeContextForEpoch(ctx, c.encodeMsg(fields...), RequestTypeGeneral, epoch, true)
}

// requestHistoricalDailyFeeRateForEpoch is a deliberately closed HMDS
// encoder. It accepts no caller-selected whatToShow, calendar, bar size, or
// timestamp format and uses Connection's shared request/order allocator.
func (c *Connection) requestHistoricalDailyFeeRateForEpoch(
	ctx context.Context,
	contract Contract,
	lookbackDays int,
	epoch uint64,
	beforeSend func(int),
) (int, error) {
	if !c.IsConnected() || c.BrokerSessionEpoch() != epoch {
		return 0, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	if err := c.requireServerVersion("RequestHistoricalData"); err != nil {
		return 0, err
	}
	if contract.ConID <= 0 || !HistoricalFeeRateUSRouteSupported(contract, true) {
		return 0, &HistoricalDataValidationError{Reason: "unsupported_market_calendar"}
	}
	reqID, err := c.reserveRequestID(nil)
	if err != nil {
		return 0, err
	}
	multiplier := ""
	if contract.Multiplier != 0 {
		multiplier = strconv.Itoa(contract.Multiplier)
	}
	fields := []any{
		reqHistoricalData,
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		"",
		contract.Right,
		multiplier,
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass,
		false,
	}
	if contract.SecIDType != "" || contract.SecID != "" {
		fields = append(fields, contract.SecIDType, contract.SecID)
	}
	fields = append(fields,
		"",
		"1 day",
		formatHistoricalDuration(lookbackDays),
		true,
		"FEE_RATE",
		2,
		false,
		"",
	)
	if beforeSend != nil {
		beforeSend(reqID)
	}
	if err := c.sendMessageWithTypeContextForEpoch(ctx, c.encodeMsg(fields...), RequestTypeHistorical, epoch, true); err != nil {
		return 0, fmt.Errorf("failed to request historical FEE_RATE data: %w", err)
	}
	return reqID, nil
}

func (c *Connection) cancelHistoricalDataForEpoch(ctx context.Context, reqID int, epoch uint64) error {
	if reqID <= 0 {
		return nil
	}
	msg := c.encodeMsg(cancelHistoricalData, 1, reqID)
	return c.sendMessageWithTypeContextForEpoch(ctx, msg, RequestTypeHistorical, epoch, true)
}

func historicalDailyFeeRateKey(contract Contract, lookbackDays int) string {
	return strings.Join([]string{
		strconv.Itoa(contract.ConID),
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strconv.FormatFloat(contract.Strike, 'g', -1, 64),
		contract.Right,
		strconv.Itoa(contract.Multiplier),
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass,
		contract.SecIDType,
		contract.SecID,
		formatHistoricalDuration(lookbackDays),
		"1 day",
		"FEE_RATE",
		"useRTH=1",
		"formatDate=2",
	}, "\x00")
}

func (c *Connector) historicalClock() time.Time {
	if c.historicalNow != nil {
		return c.historicalNow()
	}
	return time.Now()
}

func (c *Connector) acquireHistoricalExactFlight(key string, binding HistoricalSessionBinding) (*historicalExactFlight, bool) {
	now := c.historicalClock()
	c.historicalMu.Lock()
	defer c.historicalMu.Unlock()
	for candidateKey, candidate := range c.historicalExactFlights {
		if !candidate.expiresAt.IsZero() && !now.Before(candidate.expiresAt) {
			delete(c.historicalExactFlights, candidateKey)
		}
	}
	if flight := c.historicalExactFlights[key]; flight != nil {
		if flight.connection == binding.connection && flight.epoch == binding.epoch {
			return flight, false
		}
		delete(c.historicalExactFlights, key)
	}
	flight := &historicalExactFlight{done: make(chan struct{}), connection: binding.connection, epoch: binding.epoch}
	c.historicalExactFlights[key] = flight
	return flight, true
}

func (c *Connector) runHistoricalDailyFeeRateFlight(flight *historicalExactFlight, binding HistoricalSessionBinding, contract Contract, lookbackDays int) {
	flightCtx, cancel := context.WithTimeout(context.Background(), historicalFeeRateBackendTimeout)
	defer cancel()
	var err error
	if contract.Exchange == "" {
		contract, err = c.resolveExactHistoricalStockRoute(flightCtx, contract, historicalExactRouteBackendTimeout)
	}
	var bars []HistoricalBar
	if err == nil && c.HistoricalSessionCurrent(binding) {
		bars, err = c.fetchHistoricalWithContractOptions(
			flightCtx,
			contract.Symbol,
			contract,
			lookbackDays,
			historicalFeeRateBackendTimeout,
			"FEE_RATE",
			historicalRequestOptions{
				strictDaily: true, waitForEnd: true, requestOwnsNoticeCollision: true, formatDate: 2,
				session: binding, requireEpoch: true,
			},
		)
	} else if err == nil {
		err = &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	if err == nil && len(bars) == 0 {
		err = &HistoricalRequestError{Category: HistoricalFailureNoData}
	}
	if err == nil && !c.HistoricalSessionCurrent(binding) {
		bars = nil
		err = &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	if err != nil {
		bars = nil
		err = sanitizeExactHistoricalError(err)
	}

	c.historicalMu.Lock()
	flight.bars = slices.Clone(bars)
	flight.err = err
	flight.completedAt = c.historicalClock()
	flight.expiresAt = flight.completedAt.Add(historicalIdenticalRequestCooldown)
	close(flight.done)
	c.historicalMu.Unlock()
}

func (c *Connector) resolveExactHistoricalStockRoute(ctx context.Context, contract Contract, timeout time.Duration) (Contract, error) {
	contract = normalizeExactHistoricalContract(contract)
	if contract.ConID <= 0 || contract.Symbol == "" || contract.SecType != "STK" || contract.Currency == "" {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureContractUnavailable}
	}
	if !HistoricalFeeRateUSRouteSupported(contract, contract.Exchange != "") {
		return Contract{}, &HistoricalDataValidationError{Reason: "unsupported_market_calendar"}
	}
	if contract.Exchange != "" {
		return contract, nil
	}
	if timeout <= 0 {
		timeout = historicalExactRouteBackendTimeout
	}
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	key := "fee-route\x00" + historicalDailyFeeRateKey(contract, 0)
	flight, leader := c.acquireHistoricalExactFlight(key, binding)
	if leader {
		go c.runExactHistoricalStockRouteFlight(flight, binding, contract)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case <-flight.done:
		return flight.route, cloneSanitizedHistoricalError(flight.err)
	case <-waitCtx.Done():
		return Contract{}, waitCtx.Err()
	}
}

func (c *Connector) runExactHistoricalStockRouteFlight(flight *historicalExactFlight, binding HistoricalSessionBinding, contract Contract) {
	ctx, cancel := context.WithTimeout(context.Background(), historicalExactRouteBackendTimeout)
	defer cancel()
	route, err := c.resolveExactHistoricalStockRouteWire(ctx, binding, contract)
	if err == nil && !c.HistoricalSessionCurrent(binding) {
		route = Contract{}
		err = &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	if err != nil {
		route = Contract{}
		err = sanitizeExactHistoricalError(err)
	}
	now := c.historicalClock()
	expiresAt := now.Add(historicalIdenticalRequestCooldown)
	if err == nil {
		expiresAt = now.Add(historicalExactRouteSuccessTTL)
	}
	c.historicalMu.Lock()
	flight.route = route
	flight.err = err
	flight.completedAt = now
	flight.expiresAt = expiresAt
	close(flight.done)
	c.historicalMu.Unlock()
}

func (c *Connector) resolveExactHistoricalStockRouteWire(ctx context.Context, binding HistoricalSessionBinding, contract Contract) (Contract, error) {
	if !c.HistoricalSessionCurrent(binding) {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	conn := binding.connection

	reqID, err := conn.reserveRequestID(nil)
	if err != nil {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	detailsCh := make(chan ContractDetailsLite, 8)
	doneCh := make(chan struct{}, 1)
	failureCh := make(chan error, 1)
	overflowCh := make(chan struct{}, 1)
	serverVersion := conn.serverVersion
	dataHandlerID := conn.RegisterHandler(msgContractData, func(fields []string) {
		if conn.BrokerSessionEpoch() != binding.epoch {
			return
		}
		if detail, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *detail:
			default:
				select {
				case overflowCh <- struct{}{}:
				default:
				}
			}
		}
	})
	endHandlerID := conn.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if conn.BrokerSessionEpoch() != binding.epoch {
			return
		}
		if len(fields) < 3 {
			return
		}
		id, _ := strconv.Atoi(fields[2])
		if id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})
	c.historicalMu.Lock()
	c.historicalRouteReqs[reqID] = failureCh
	c.historicalMu.Unlock()
	defer func() {
		conn.UnregisterHandler(msgContractData, dataHandlerID)
		conn.UnregisterHandler(msgContractDataEnd, endHandlerID)
		c.historicalMu.Lock()
		delete(c.historicalRouteReqs, reqID)
		c.historicalMu.Unlock()
	}()

	if !c.HistoricalSessionCurrent(binding) {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	if err := conn.sendExactHistoricalStockRouteRequest(ctx, contract, reqID, binding.epoch); err != nil {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
	}
	details := make([]ContractDetailsLite, 0, 1)
	for {
		select {
		case detail := <-detailsCh:
			details = append(details, detail)
		case <-overflowCh:
			return Contract{}, &HistoricalDataValidationError{Reason: "contract_details_overflow"}
		case <-doneCh:
			for {
				select {
				case detail := <-detailsCh:
					details = append(details, detail)
				case <-overflowCh:
					return Contract{}, &HistoricalDataValidationError{Reason: "contract_details_overflow"}
				default:
					return exactHistoricalStockRoute(contract, details)
				}
			}
		case routeErr := <-failureCh:
			return Contract{}, routeErr
		case <-ctx.Done():
			return Contract{}, ctx.Err()
		}
	}
}

func exactHistoricalStockRoute(contract Contract, details []ContractDetailsLite) (Contract, error) {
	var selected ContractDetailsLite
	for _, detail := range details {
		detail.Symbol = strings.ToUpper(strings.TrimSpace(detail.Symbol))
		detail.SecType = strings.ToUpper(strings.TrimSpace(detail.SecType))
		detail.Exchange = strings.ToUpper(strings.TrimSpace(detail.Exchange))
		detail.PrimaryExch = strings.ToUpper(strings.TrimSpace(detail.PrimaryExch))
		detail.Currency = strings.ToUpper(strings.TrimSpace(detail.Currency))
		detail.LocalSymbol = strings.TrimSpace(detail.LocalSymbol)
		detail.TradingClass = strings.TrimSpace(detail.TradingClass)
		if detail.ConID != contract.ConID || detail.Symbol != contract.Symbol || detail.SecType != contract.SecType ||
			detail.Currency != contract.Currency || detail.Exchange == "" ||
			(contract.PrimaryExch != "" && detail.PrimaryExch != "" && detail.PrimaryExch != contract.PrimaryExch) ||
			(contract.LocalSymbol != "" && detail.LocalSymbol != "" && detail.LocalSymbol != contract.LocalSymbol) ||
			(contract.TradingClass != "" && detail.TradingClass != "" && detail.TradingClass != contract.TradingClass) {
			return Contract{}, &HistoricalRequestError{Category: HistoricalFailureContractUnavailable}
		}
		if selected.ConID != 0 && (detail.Exchange != selected.Exchange || detail.PrimaryExch != selected.PrimaryExch ||
			detail.LocalSymbol != selected.LocalSymbol || detail.TradingClass != selected.TradingClass) {
			return Contract{}, &HistoricalRequestError{Category: HistoricalFailureContractUnavailable}
		}
		selected = detail
	}
	if selected.ConID == 0 {
		return Contract{}, &HistoricalRequestError{Category: HistoricalFailureContractUnavailable}
	}
	resolved := contract
	resolved.Exchange = selected.Exchange
	if selected.PrimaryExch != "" {
		resolved.PrimaryExch = selected.PrimaryExch
	}
	if resolved.LocalSymbol == "" {
		resolved.LocalSymbol = selected.LocalSymbol
	}
	if resolved.TradingClass == "" {
		resolved.TradingClass = selected.TradingClass
	}
	if !HistoricalFeeRateUSRouteSupported(resolved, true) {
		return Contract{}, &HistoricalDataValidationError{Reason: "unsupported_market_calendar"}
	}
	return resolved, nil
}

func sanitizeExactHistoricalError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*HistoricalDataValidationError](err); ok {
		return &HistoricalRequestError{Category: HistoricalFailureInvalidPayload}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if requestErr, ok := errors.AsType[*HistoricalRequestError](err); ok {
		category := classifyHistoricalRequestFailure(requestErr)
		return &HistoricalRequestError{
			Code:       requestErr.Code,
			RetryAfter: requestErr.RetryAfter,
			Category:   category,
		}
	}
	return &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
}

func classifyHistoricalRequestFailure(err *HistoricalRequestError) string {
	if err == nil {
		return HistoricalFailureProtocolRejected
	}
	switch err.Category {
	case HistoricalFailureNotEntitled,
		HistoricalFailureNoData,
		HistoricalFailurePacing,
		HistoricalFailureGatewayUnavailable,
		HistoricalFailureContractUnavailable,
		HistoricalFailureProtocolRejected,
		HistoricalFailureInvalidPayload:
		return err.Category
	}

	upperMessage := strings.ToUpper(err.Message)
	switch err.Code {
	case 354, 10089, 10090, 10091, 10186, 10187:
		return HistoricalFailureNotEntitled
	case 502, 504, 1100, 1300:
		return HistoricalFailureGatewayUnavailable
	case 200:
		return HistoricalFailureContractUnavailable
	case 321, 366:
		return HistoricalFailureProtocolRejected
	}
	if err.Code == 162 {
		switch {
		case strings.Contains(upperMessage, "NO MARKET DATA PERMISSION"),
			strings.Contains(upperMessage, "NOT SUBSCRIBED"),
			strings.Contains(upperMessage, "MARKET DATA SUBSCRIPTION"),
			strings.Contains(upperMessage, "PERMISSION TO USE"):
			return HistoricalFailureNotEntitled
		case strings.Contains(upperMessage, "NO DATA"):
			return HistoricalFailureNoData
		case strings.Contains(upperMessage, "PACING"):
			return HistoricalFailurePacing
		}
	}
	return HistoricalFailureProtocolRejected
}

func cloneSanitizedHistoricalError(err error) error {
	if err == nil {
		return nil
	}
	if requestErr, ok := errors.AsType[*HistoricalRequestError](err); ok {
		return &HistoricalRequestError{
			Code:       requestErr.Code,
			RetryAfter: requestErr.RetryAfter,
			Category:   requestErr.Category,
		}
	}
	if validation, ok := errors.AsType[*HistoricalDataValidationError](err); ok {
		return &HistoricalDataValidationError{Reason: validation.Reason}
	}
	return err
}

func (c *Connector) fetchHistoricalDailyBarsWithContract(ctx context.Context, contract Contract, lookbackDays int, timeout time.Duration) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !c.IsReady() {
		return nil, fmt.Errorf("IBKR connection not ready")
	}
	contract = normalizeMarketDataContract(contract)
	if contract.Symbol == "" {
		return nil, fmt.Errorf("contract symbol is required")
	}
	symbol := strings.ToUpper(contract.Symbol)
	if _, inactive := c.inactiveReason(MarketDataKeyForContract(contract)); inactive {
		return nil, ErrSymbolInactive
	}
	if lookbackDays <= 0 {
		lookbackDays = 400
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	timeout, err := historicalTimeoutWithinContext(ctx, timeout)
	if err != nil {
		return nil, err
	}
	fallbackPrimary := contract.PrimaryExch
	if detail := c.cachedContractDetail(symbol); detail != nil && detail.ConID != 0 {
		candidate := contract
		if c.applyContractDetail(*detail, &candidate) {
			normalizeEquityRouting(&candidate, fallbackPrimary)
			if explicitContractRouteMatches(contract, candidate) {
				contract = candidate
			}
		}
	}
	if contract.ConID == 0 {
		resolveTimeout := min(timeout, 12*time.Second)
		details, err := c.fetchContractDetailsForContract(contract, resolveTimeout)
		if len(details) > 0 {
			for _, detail := range details {
				candidate := contract
				if !c.applyContractDetail(detail, &candidate) {
					continue
				}
				normalizeEquityRouting(&candidate, fallbackPrimary)
				if explicitContractRouteMatches(contract, candidate) {
					contract = candidate
					break
				}
			}
			if contract.ConID == 0 {
				c.logWarn("Routed contract details for %s returned no route match (exchange=%s primary=%s currency=%s)",
					symbol, contract.Exchange, contract.PrimaryExch, contract.Currency)
			}
		} else if err != nil {
			c.logDebug("Routed contract details for %s unavailable (%v)", symbol, err)
		}
	}
	return c.fetchHistoricalDailyBarsWithBase(ctx, symbol, contract, fallbackPrimary, lookbackDays, timeout, false, "")
}

func (c *Connector) fetchHistoricalDailyBarsWithBase(ctx context.Context, symbol string, baseContract Contract, primary string, lookbackDays int, timeout time.Duration, requireConID bool, forceWhatToShow string) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	timeout, err = historicalTimeoutWithinContext(ctx, timeout)
	if err != nil {
		return nil, err
	}
	requestedContract := baseContract
	graceWindow := contractDetailsLateGrace
	if timeout > 0 {
		if half := timeout / 2; half > 0 && half < graceWindow {
			graceWindow = half
		}
	}

	// ensureContractDetails budget: 30 s. A historical-data fan-out
	// (breadth-spx, 500+ names) puts dozens of reqContractData on the
	// wire alongside reqHistoricalData; even with the rate limiter's
	// per-request dispatcher (no HoL blocking) IBKR can take several
	// seconds to respond per contract under load. The previous 5 s
	// budget tripped on liquid names (DIS, ACN, ISRG, …) whenever the
	// gateway was busy and the awaitContractDetail grace window
	// couldn't recover the late frames in time, surfacing as
	// "contract details unresolved" with primary='' in the daemon log.
	// Up to 30 s aligns with the prewarm path (server.go:749), but
	// shorter caller budgets stay shorter for prompt-facing screens.
	if baseContract.ConID == 0 && requireConID {
		var fetchErr error
		resolveTimeout := 30 * time.Second
		if timeout > 0 {
			resolveTimeout = min(resolveTimeout, timeout)
		}
		if detail, err := c.ensureContractDetails(symbol, resolveTimeout); err == nil && detail != nil {
			candidate := baseContract
			if c.applyContractDetail(*detail, &candidate) {
				normalizeEquityRouting(&candidate, primary)
				if requireConID || explicitContractRouteMatches(requestedContract, candidate) {
					baseContract = candidate
				}
			}
		} else {
			fetchErr = err
			late := c.awaitContractDetailCtx(ctx, symbol, graceWindow)
			candidate := baseContract
			if late != nil && c.applyContractDetail(*late, &candidate) {
				normalizeEquityRouting(&candidate, primary)
				if requireConID || explicitContractRouteMatches(requestedContract, candidate) {
					baseContract = candidate
				}
				c.logInfo("Contract details for %s arrived during grace window (conID=%d)", symbol, late.ConID)
			} else if fetchErr != nil {
				c.logDebug("Contract details for %s unavailable (%v); using static classification hints only", symbol, fetchErr)
			}
		}
	}

	// The only WARN in this chain. A quote-history fallback runs the routed
	// contract and then the bare symbol, and each leg used to warn on both its
	// resolve step and its request step — one unresolvable name produced four
	// identical-in-substance WARN lines per attempt. The intermediate steps are
	// debug; this abort is the caller-visible outcome, and every other leg
	// returns its error for the caller to report.
	if requireConID && baseContract.ConID == 0 {
		c.logWarn("Historical data request aborted for %s: contract ID unresolved (exchange=%s primary=%s)", symbol, baseContract.Exchange, baseContract.PrimaryExch)
		return nil, fmt.Errorf("contract details unresolved for %s (exchange=%s primary=%s)", symbol, baseContract.Exchange, baseContract.PrimaryExch)
	}

	type attempt struct {
		contract   Contract
		whatToShow string
		label      string
	}

	var seq []string
	if strings.TrimSpace(forceWhatToShow) != "" {
		cleanWhat, err := normalizeHistoricalWhatToShow(forceWhatToShow)
		if err != nil {
			return nil, err
		}
		seq = []string{cleanWhat}
	} else {
		baseWhat := defaultHistoricalWhat(baseContract.SecType)
		altWhat := alternateHistoricalWhat(baseWhat)
		seq = historicalWhatSequence(symbol, baseContract.SecType, baseWhat, altWhat)
	}
	attempts := make([]attempt, 0, len(seq)*2)
	seen := make(map[string]struct{})
	appendAttempt := func(contract Contract, what string) {
		if what == "" {
			return
		}
		key := strings.ToUpper(contract.Exchange) + "|" + strings.ToUpper(what)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		attempts = append(attempts, attempt{
			contract:   contract,
			whatToShow: what,
			label:      fmt.Sprintf("%s/%s", contract.Exchange, what),
		})
	}

	for _, what := range seq {
		appendAttempt(baseContract, what)
	}

	if primary != "" && strings.EqualFold(baseContract.Exchange, "SMART") {
		altContract := baseContract
		altContract.Exchange = primary
		altContract.PrimaryExch = ""
		for _, what := range seq {
			appendAttempt(altContract, what)
		}
	}

	var lastBars []HistoricalBar
	var lastErr error
	for idx, att := range attempts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		bars, err := c.fetchHistoricalWithContract(ctx, symbol, att.contract, lookbackDays, timeout, att.whatToShow)
		if err != nil {
			if shouldRetryHistorical(err) && idx < len(attempts)-1 {
				c.logWarn("Historical data attempt %s for %s failed (%v); retrying with alternate route", att.label, symbol, err)
				lastErr = err
				continue
			}
			return nil, err
		}
		if len(bars) > 0 {
			if idx > 0 {
				c.logInfo("Historical data for %s recovered via %s", symbol, att.label)
			}
			return bars, nil
		}
		c.logWarn("Historical data for %s returned no rows via %s", symbol, att.label)
		lastBars = bars
	}

	if len(lastBars) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("historical data unavailable for %s", symbol)
	}
	return lastBars, nil
}

func defaultHistoricalWhat(secType string) string {
	switch strings.ToUpper(secType) {
	case "IND", "CMDTY", "CASH":
		// CASH has no consolidated trade tape on IBKR; reqHistoricalData
		// requires MIDPOINT for FX pairs. IND/CMDTY follow the same rule
		// because they're also non-trading reference series.
		return "MIDPOINT"
	default:
		return "TRADES"
	}
}

func alternateHistoricalWhat(current string) string {
	if strings.EqualFold(current, "TRADES") {
		return "MIDPOINT"
	}
	if strings.EqualFold(current, "MIDPOINT") {
		return "TRADES"
	}
	return current
}

func historicalWhatSequence(symbol, secType, baseWhat, altWhat string) []string {
	seq := make([]string, 0, 5)
	appendWhat := func(value string) {
		if value == "" {
			return
		}
		for _, existing := range seq {
			if strings.EqualFold(existing, value) {
				return
			}
		}
		seq = append(seq, value)
	}

	switch strings.ToUpper(strings.TrimSpace(symbol)) {
	case "VIX":
		appendWhat("TRADES")
		appendWhat("MIDPOINT")
	default:
		appendWhat(baseWhat)
		switch strings.ToUpper(strings.TrimSpace(secType)) {
		case "STK":
			appendWhat("ADJUSTED_LAST")
		case "CASH":
			// FX has no trade tape — don't bother probing TRADES;
			// IBKR rejects with code 162 and the retry would just
			// burn the deadline.
			return seq
		}
		if !strings.EqualFold(baseWhat, altWhat) {
			appendWhat(altWhat)
		}
	}

	return seq
}

func normalizeHistoricalWhatToShow(value string) (string, error) {
	clean := strings.ToUpper(strings.TrimSpace(value))
	switch clean {
	case "TRADES", "MIDPOINT", "ADJUSTED_LAST":
		return clean, nil
	default:
		return "", fmt.Errorf("unsupported historical whatToShow %q", value)
	}
}

func shouldRetryHistorical(err error) bool {
	if hErr, ok := errors.AsType[*HistoricalRequestError](err); ok {
		switch hErr.Code {
		case 162:
			return true
		}
	}
	return false
}

func (c *Connector) fetchHistoricalWithContract(ctx context.Context, symbol string, contract Contract, lookbackDays int, timeout time.Duration, whatToShow string) ([]HistoricalBar, error) {
	return c.fetchHistoricalWithContractOptions(ctx, symbol, contract, lookbackDays, timeout, whatToShow, historicalRequestOptions{})
}

func (c *Connector) fetchHistoricalWithContractOptions(ctx context.Context, symbol string, contract Contract, lookbackDays int, timeout time.Duration, whatToShow string, options historicalRequestOptions) ([]HistoricalBar, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	timeout, err := historicalTimeoutWithinContext(ctx, timeout)
	if err != nil {
		return nil, err
	}
	if contract.ConID == 0 {
		c.logDebug("Skipping historical data request for %s: unresolved contract ID (exchange=%s primary=%s)", symbol, contract.Exchange, contract.PrimaryExch)
		return nil, fmt.Errorf("contract ID unresolved for %s", symbol)
	}
	var req *historicalRequest
	var registeredReqID int
	duration := formatHistoricalDuration(lookbackDays)
	formatDate := options.formatDate
	if formatDate == 0 {
		formatDate = 1
	}
	register := func(id int) {
		registeredReqID = id
		req = c.createHistoricalRequestWithOptions(id, symbol, options)
	}
	var reqID int
	if options.requireEpoch {
		if whatToShow != "FEE_RATE" || formatDate != 2 || !c.HistoricalSessionCurrent(options.session) {
			return nil, &HistoricalRequestError{Category: HistoricalFailureGatewayUnavailable}
		}
		reqID, err = options.session.connection.requestHistoricalDailyFeeRateForEpoch(ctx, contract, lookbackDays, options.session.epoch, register)
	} else {
		// Connection's shared monotonic broker namespace is the sole allocator
		// for both requests and orders. The request-local ownership flag below
		// affects only delayed-notice routing; it must not create a second
		// allocator.
		reqID, err = c.conn.requestHistoricalDataWithIDGuard(ctx, contract, "", duration, "1 day", whatToShow, true, false, formatDate, false, nil, register)
	}
	if err != nil {
		if registeredReqID != 0 {
			c.failHistoricalRequest(registeredReqID, err)
		}
		return nil, err
	}
	if req == nil {
		req = c.createHistoricalRequestWithOptions(reqID, symbol, options)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-req.result:
		if res.err != nil {
			return nil, res.err
		}
		return res.bars, nil
	case <-ctx.Done():
		c.cancelHistoricalDataBestEffortWithOptions(reqID, options)
		c.failHistoricalRequest(reqID, ctx.Err())
		return nil, ctx.Err()
	case <-timer.C:
		c.cancelHistoricalDataBestEffortWithOptions(reqID, options)
		timeoutErr := fmt.Errorf("historical data timeout for %s after %s: %w", symbol, timeout, context.DeadlineExceeded)
		c.failHistoricalRequest(reqID, timeoutErr)
		return nil, timeoutErr
	}
}

func (c *Connector) cancelHistoricalDataBestEffortWithOptions(reqID int, options historicalRequestOptions) {
	if reqID == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var err error
		if options.requireEpoch {
			err = options.session.connection.cancelHistoricalDataForEpoch(ctx, reqID, options.session.epoch)
		} else {
			err = c.conn.CancelHistoricalData(ctx, reqID)
		}
		if err != nil {
			c.logDebug("Historical cancel reqID=%d skipped/failed: %v", reqID, err)
		}
	}()
}

// OptionIV returns the last valid implied-volatility observation for key as a
// fraction, such as 0.30 for 30 percent. The boolean is false when no valid
// observation has been cached; the method performs no broker request.
func (c *Connector) OptionIV(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optIV[symbol]
	return v, ok
}

// OptionGreeks returns the last valid model-computation Greeks for an option
// key returned by [Connector.SubscribeOption]. The boolean is false until at
// least one field has been observed; callers must not interpret absence as a
// zero-valued Greek. The returned value is a copy.
func (c *Connector) OptionGreeks(symbol string) (Greeks, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	g, ok := c.optGreeks[symbol]
	return g, ok
}

// OptionUnderlyingPrice returns the underlying price embedded in the latest
// model-computation tick for an option key. The boolean is false when no valid
// price has been observed. This is the price the broker used for the associated
// Greeks.
func (c *Connector) OptionUnderlyingPrice(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optUnderlyingPx[symbol]
	return v, ok
}

// OptionQuoteBidAsk returns the last observed bid and ask for an option key.
// It returns (0, 0, false) when neither side has been observed; one-sided
// results return true with the absent side left at zero. The method performs no
// broker request and does not itself determine freshness.
func (c *Connector) OptionQuoteBidAsk(symbol string) (bid, ask float64, ok bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	b, hasB := c.optQuoteBid[symbol]
	a, hasA := c.optQuoteAsk[symbol]
	if !hasB && !hasA {
		return 0, 0, false
	}
	return b, a, true
}

// OptionPrevClose returns the option contract's own previous regular-session
// close, not the underlying's close. The boolean is false when no positive
// value has been observed.
func (c *Connector) OptionPrevClose(symbol string) (float64, bool) {
	c.optMu.RLock()
	defer c.optMu.RUnlock()
	v, ok := c.optPrevClose[symbol]
	if !ok || v <= 0 {
		return 0, false
	}
	return v, true
}

// CancelOptionIV cancels an option-IV subscription previously returned by
// SubscribeOptionIV. Idempotent: zero reqID or an unknown reqID is a
// no-op. Best-effort on the wire — a failed CancelMarketData is logged
// but not returned, since the typical caller is a deferred cleanup.
//
// Use this instead of UnsubscribeMarketData(symbol) for option-IV
// subscriptions: SubscribeOptionIV does not install a subscriptions[symbol]
// entry, so the symbol-scoped unsubscribe either no-ops or — worse —
// tears down an unrelated streaming-quote subscription for the same
// underlier.
func (c *Connector) CancelOptionIV(reqID int) {
	if reqID == 0 {
		return
	}
	c.optMu.Lock()
	_, isOption := c.optReqIDs[reqID]
	delete(c.optReqIDs, reqID)
	c.optMu.Unlock()
	if !isOption {
		return
	}
	c.subMu.Lock()
	delete(c.reqIDMap, reqID)
	conn := c.conn
	c.subMu.Unlock()
	if conn != nil && conn.IsConnected() {
		if err := conn.CancelMarketData(reqID); err != nil {
			marketDataLogger.Debugf("%s: CancelOptionIV(reqID=%d): %v", c.name, reqID, err)
		}
	}
}

// SubscribeOptionIV starts an option market-data subscription and routes
// model-computation IV to the normalized underlying key read by
// [Connector.OptionIV]. expiry is converted to a UTC YYYYMMDD date and right is
// normalized to upper case. ctx bounds contract resolution and slot
// acquisition. Pair the returned request ID with [Connector.CancelOptionIV].
func (c *Connector) SubscribeOptionIV(ctx context.Context, symbol string, expiry time.Time, strike float64, right string) (int, error) {
	return c.subscribeOptionIV(ctx, symbol, expiry, strike, right, strings.ToUpper(symbol))
}

// SubscribeOptionIVKeyed starts one option market-data subscription and routes
// model IV to the returned per-contract key. Use the key with [Connector.OptionIV]
// and the request ID with [Connector.CancelOptionIV]. Unlike
// [Connector.SubscribeOptionIV], concurrent legs for one underlying do not
// overwrite one shared underlying-keyed value.
func (c *Connector) SubscribeOptionIVKeyed(ctx context.Context, symbol string, expiry time.Time, strike float64, right string) (int, string, error) {
	expStr := expiry.UTC().Format("20060102")
	key := OptionMarketDataKey(symbol, expStr, right, strike)
	reqID, err := c.subscribeOptionIV(ctx, symbol, expiry, strike, right, key)
	if err != nil {
		return 0, "", err
	}
	return reqID, key, nil
}

func (c *Connector) subscribeOptionIV(ctx context.Context, symbol string, expiry time.Time, strike float64, right, routeKey string) (int, error) {
	symbol = strings.ToUpper(symbol)
	if routeKey == "" {
		routeKey = symbol
	}
	if _, inactive := c.inactiveReason(symbol); inactive {
		c.logDebug("Skipping option IV subscription for inactive symbol %s", symbol)
		return 0, ErrSymbolInactive
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return 0, fmt.Errorf("IBKR connection not available")
	}

	// Format expiry as YYYYMMDD
	expStr := expiry.UTC().Format("20060102")
	reqID, err := conn.RequestOptionsMarketData(ctx, symbol, expStr, strike, strings.ToUpper(right))
	if err != nil {
		return 0, err
	}

	// Map reqID to the requested route key so we can attribute IV updates.
	c.subMu.Lock()
	c.reqIDMap[reqID] = routeKey
	c.subMu.Unlock()
	c.optMu.Lock()
	c.optReqIDs[reqID] = routeKey
	c.optMu.Unlock()

	c.logInfo("Subscribed option IV for %s %s %.2f %s (ReqID: %d, key: %s)", symbol, expStr, strike, right, reqID, routeKey)
	return reqID, nil
}

// handleTickSize processes tick size updates
func (c *Connector) handleTickSize(fields []string) {
	if len(fields) < 4 {
		return
	}

	// Format: [msgID, version, reqID, tickType, size]
	if len(fields) < 5 {
		return
	}
	reqIDStr := fields[2]
	tickTypeStr := fields[3]
	sizeStr := fields[4]

	reqID, _ := strconv.Atoi(reqIDStr)
	tickType, _ := strconv.Atoi(tickTypeStr)
	size, ok := parseTickSize(c.ServerVersion(), tickType, sizeStr)
	if !ok {
		return
	}

	// Find the symbol for this request ID
	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()

	if !exists {
		return
	}

	// Update subscription data based on tick type
	c.subMu.Lock()
	defer c.subMu.Unlock()

	sub, exists := c.subscriptions[symbol]
	if !exists {
		return
	}

	// Mark observed on any size tick
	sub.Observed = true

	// IBKR tick types: 0=BID_SIZE, 3=ASK_SIZE, 8=VOLUME (cumulative day total).
	// 21=average volume, delivered by the Misc Stats generic-tick bundle (165).
	// Delayed subscriptions use 69/70/74 for bid size / ask size / volume.
	// 5=LAST_SIZE is intentionally dropped — too noisy and not surfaced.
	// 27=callOpenInterest, 28=putOpenInterest. On option-leg subscriptions,
	// the gateway emits a zero-valued companion tick for the opposite right,
	// so only the tick matching Subscription.Right may commit OpenInt. On
	// right-less stock/ETF subscriptions these ticks are aggregate option OI;
	// no consumer reads that value from this slot, so dropping it is deliberate.
	switch tickType {
	case 0, 69:
		sub.BidSize = size
	case 3, 70:
		sub.AskSize = size
	case 8, 74:
		sub.Volume = size
	case 21:
		sub.AvgVolume = size
	case 27:
		if sub.Right == "C" {
			sub.OpenInt = size
			sub.OpenIntObserved = true
		}
	case 28:
		if sub.Right == "P" {
			sub.OpenInt = size
			sub.OpenIntObserved = true
		}
	case 89:
		// Shortable share count, delivered for the generic-tick-236
		// request (TWS build 974+). Feeds the borrow-inventory market-
		// event flags. ShortableObserved means "a share COUNT landed" —
		// never the tick-46 difficulty level, whose 0–3 float would read
		// as ≤1000 shares and fire false Borrow-scarce flags.
		sub.ShortableShares = size
		sub.ShortableObserved = true
	}
	sub.LastTime = time.Now()
	sub.LastTickAt = sub.LastTime
}

// handleTickString processes IBKR tick-string updates. Tick type 45 carries
// the last timestamp as Unix seconds; tick type 233 (RTVolume) carries a
// semicolon-delimited real-time volume payload whose cumulative-volume field
// is the most reliable intraday volume source for some live subscriptions.
func (c *Connector) handleTickString(fields []string) {
	if len(fields) < 5 {
		return
	}
	reqID, err := strconv.Atoi(fields[2])
	if err != nil {
		return
	}
	tickType, err := strconv.Atoi(fields[3])
	if err != nil {
		return
	}
	value := strings.TrimSpace(fields[4])
	if value == "" {
		return
	}

	c.subMu.RLock()
	symbol, exists := c.reqIDMap[reqID]
	c.subMu.RUnlock()
	if !exists {
		return
	}

	c.subMu.Lock()
	defer c.subMu.Unlock()
	sub, ok := c.subscriptions[symbol]
	if !ok {
		return
	}
	switch tickType {
	case 45:
		sec, err := strconv.ParseInt(value, 10, 64)
		if err != nil || sec <= 0 {
			return
		}
		sub.LastTradeTime = time.Unix(sec, 0)
	case 233:
		last, volume, ts, ok := parseRTVolumeTick(value, c.ServerVersion())
		if !ok {
			return
		}
		if last > 0 {
			sub.LastPrice = last
		}
		if volume > 0 {
			sub.Volume = volume
		}
		if !ts.IsZero() {
			sub.LastTradeTime = ts
		}
	default:
		return
	}
	sub.LastTime = time.Now()
	sub.LastTickAt = sub.LastTime
	sub.Observed = true
}

func parseRTVolumeTick(value string, serverVersion int) (last float64, volume int64, ts time.Time, ok bool) {
	parts := strings.Split(value, ";")
	if len(parts) < 4 {
		return 0, 0, time.Time{}, false
	}
	last, _ = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if v, parsed := parseTickSize(serverVersion, 8, strings.TrimSpace(parts[3])); parsed {
		volume = v
	}
	rawTime := strings.TrimSpace(parts[2])
	if rawTime != "" {
		if n, err := strconv.ParseInt(rawTime, 10, 64); err == nil && n > 0 {
			if n > 10_000_000_000 {
				ts = time.UnixMilli(n)
			} else {
				ts = time.Unix(n, 0)
			}
		}
	}
	return last, volume, ts, last > 0 || volume > 0 || !ts.IsZero()
}

// parseTickSize normalises IBKR tickSize payloads.
//
// Recent TWS/Gateway builds expose size values as IBKR Decimal payloads.
// For stock volume (tick type 8) the wire field can arrive as a fixed-scale
// integer where 41,762,007 shares is encoded as 41762007966821. Treating that
// as a plain int produces trillion-share volume in quote/scanner output. Small
// values still arrive as ordinary integers for some instruments, so only
// normalise obviously-scaled volume payloads and leave bid/ask sizes and option
// open-interest ticks untouched.
func parseTickSize(serverVersion, tickType int, raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	size, err := strconv.ParseInt(raw, 10, 64)
	if err == nil {
		if serverVersion >= minServerVerSizeRules && (tickType == 8 || tickType == 74) && size >= 1_000_000 {
			return size / 1_000_000, true
		}
		return size, true
	}
	decimal, decimalErr := strconv.ParseFloat(raw, 64)
	if decimalErr != nil || math.IsNaN(decimal) || math.IsInf(decimal, 0) || decimal < 0 {
		marketDataLogger.Warnf("Invalid tick size for tickType %d: %q (error: %v)", tickType, raw, err)
		return 0, false
	}
	return int64(decimal), true
}

// handlePosition processes position updates
func (c *Connector) handlePosition(fields []string) {
	if len(fields) < 8 {
		return
	}

	// Fields: version, account, symbol, secType, expiry, strike, right, multiplier, exchange, currency, localSymbol, tradingClass, position, avgCost
	symbol := fields[2]
	positionStr := fields[12]
	avgCostStr := fields[13]

	c.logDebug("Position update - Symbol: %s, Position: %s, AvgCost: %s",
		symbol, positionStr, avgCostStr)
}

// handlePortfolioValue processes portfolio updates
func (c *Connector) handlePortfolioValue(fields []string) {
	if len(fields) < 18 {
		return
	}

	// Extract relevant fields
	symbol := fields[1]
	position := fields[6]
	marketPrice := fields[7]
	marketValue := fields[8]
	avgCost := fields[9]
	unrealizedPnL := fields[10]
	realizedPnL := fields[11]

	c.logDebug("Portfolio update - Symbol: %s, Position: %s, Price: %s, Value: %s, UnrealizedPnL: %s, RealizedPnL: %s, AvgCost: %s",
		symbol, position, marketPrice, marketValue, unrealizedPnL, realizedPnL, avgCost)
}

// handleOrderStatus processes order status updates
func (c *Connector) handleOrderStatus(fields []string) {
	if len(fields) < 3 {
		return
	}

	start := 1
	if len(fields) > 3 && isNumeric(fields[1]) && isNumeric(fields[2]) {
		start = 2
	}
	if len(fields) <= start+3 {
		return
	}

	orderID := ""
	status := ""
	filled := "0"
	remaining := "0"
	avgFillPrice := "0"
	lastFillPrice := "0"
	whyHeld := ""
	if len(fields) > 1 && fields[1] == "protobuf" {
		orderID = summaryFieldValue(fields, "orderId=")
		status = summaryFieldValue(fields, "status=")
		filled = summaryFieldValue(fields, "filled=")
		remaining = summaryFieldValue(fields, "remaining=")
		avgFillPrice = summaryFieldValue(fields, "avgFillPrice=")
		lastFillPrice = summaryFieldValue(fields, "lastFillPrice=")
		whyHeld = summaryFieldValue(fields, "whyHeld=")
	} else {
		orderID = fields[start]
		status = fields[start+1]
		filled = fields[start+2]
		remaining = fields[start+3]
		if len(fields) > start+4 {
			avgFillPrice = fields[start+4]
		}
		if len(fields) > start+6 {
			lastFillPrice = fields[start+6]
		}
		if len(fields) > start+9 {
			whyHeld = fields[start+9]
		}
	}
	if orderID == "" || status == "" {
		return
	}

	filledQty, _ := strconv.ParseFloat(filled, 64)
	remainingQty, _ := strconv.ParseFloat(remaining, 64)
	avgPx, _ := strconv.ParseFloat(avgFillPrice, 64)
	lastPx, _ := strconv.ParseFloat(lastFillPrice, 64)

	c.logOrderStatus(orderID, status, filledQty, remainingQty, avgPx)

	c.orderMu.Lock()
	internalID, ok := c.brokerOrderIndex[orderID]
	if !ok {
		// Fallback: try direct lookup using broker ID as key (some tests store that way)
		internalID = orderID
	}
	order, exists := c.openOrders[internalID]
	if !exists {
		c.orderMu.Unlock()
		return
	}

	order.BrokerID = orderID
	order.FilledQty = filledQty
	if avgPx > 0 {
		order.FilledPrice = avgPx
	} else if lastPx > 0 {
		order.FilledPrice = lastPx
	}
	order.UpdatedAt = time.Now()
	order.Status = mapIBOrderStatus(status, filledQty, remainingQty)
	if whyHeld != "" {
		order.Reason = whyHeld
	}

	if order.Status == OrderStatusFilled && order.FilledAt == nil {
		now := time.Now()
		order.FilledAt = &now
	}
	if order.Status == OrderStatusCancelled && order.CancelledAt == nil {
		now := time.Now()
		order.CancelledAt = &now
	}

	// Remove from open orders once terminal
	terminal := isTerminalOrderStatus(order.Status)
	if terminal {
		delete(c.openOrders, internalID)
		delete(c.brokerOrderIndex, orderID)
	}

	c.orderMu.Unlock()

	// Drop the log-dedupe signature outside orderMu so a later frame or a
	// reused broker id logs fresh at INFO instead of being swallowed as a
	// repeat.
	if terminal {
		c.forgetOrderStatusLog(orderID)
	}
}

// logOrderStatus emits the order-status line, demoting verbatim repeats to
// Debug. IBKR re-sends orderStatus frames for unchanged working orders many
// times per second, so only a change in status/filled/remaining reaches INFO;
// the steady-state churn stays at Debug, which the default level drops. This
// governs the log line alone — state tracking in handleOrderStatus is
// unaffected.
func (c *Connector) logOrderStatus(orderID, status string, filled, remaining, avgPx float64) {
	const format = "Order status - ID: %s, Status: %s, Filled: %.4f, Remaining: %.4f, AvgPrice: %.4f"
	if c.orderStatusChanged(orderID, orderStatusLogSignature(status, filled, remaining)) {
		c.logInfo(format, orderID, status, filled, remaining, avgPx)
		return
	}
	c.logDebug(format, orderID, status, filled, remaining, avgPx)
}

// orderStatusLogSignature is the dedupe key for the order-status log line:
// only a change in one of these fields is worth an INFO line. avgFillPrice is
// omitted deliberately — it moves only alongside filled/remaining.
func orderStatusLogSignature(status string, filled, remaining float64) string {
	return fmt.Sprintf("%s|%.4f|%.4f", status, filled, remaining)
}

// orderStatusChanged reports whether this order-status frame differs from the
// last one logged for orderID, recording the new signature when it does. True
// means the frame earns an INFO line; false means a verbatim repeat that
// belongs at Debug.
func (c *Connector) orderStatusChanged(orderID, sig string) bool {
	c.orderStatusLogMu.Lock()
	defer c.orderStatusLogMu.Unlock()
	if c.orderStatusLogSig == nil {
		c.orderStatusLogSig = make(map[string]string)
	} else if c.orderStatusLogSig[orderID] == sig {
		return false
	}
	c.orderStatusLogSig[orderID] = sig
	return true
}

// forgetOrderStatusLog drops the dedupe signature for a terminal or removed
// order so its slot does not linger in the map and a later reused broker id
// logs fresh.
func (c *Connector) forgetOrderStatusLog(orderID string) {
	c.orderStatusLogMu.Lock()
	delete(c.orderStatusLogSig, orderID)
	c.orderStatusLogMu.Unlock()
}

func (c *Connector) notifyOrderLifecycleFrom(conn *Connection, epoch uint64, fields []string) {
	ev, ok := ParseOrderLifecycleEvent(fields)
	if !ok {
		return
	}
	if ev.WhatIf {
		return
	}
	if ev.Type == OrderLifecycleEventStatus && c.isWhatIfOrderID(ev.OrderID) {
		return
	}
	origin := ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.RLock()
	defer c.evidenceBarrier.RUnlock()
	current := c.SessionReceiptCurrent(origin)
	if current && ev.Type == OrderLifecycleEventStatus {
		c.handleOrderStatus(fields)
	}
	c.dispatchOrderLifecycleUnderBarrier(origin, ev, current)
}

func (c *Connector) notifyOrderLifecycle(fields []string) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	epoch := uint64(0)
	if conn != nil {
		epoch = conn.BrokerSessionEpoch()
	}
	c.notifyOrderLifecycleFrom(conn, epoch, fields)
}

func (c *Connector) isWhatIfOrderID(orderID int) bool {
	if c == nil || orderID <= 0 {
		return false
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	return conn != nil && conn.IsWhatIfOrderID(orderID)
}

// isKnownBrokerOrderID reports whether id names a broker order this
// connector owns: a journaled/open order or an in-flight WhatIf preview.
// Used to keep request-scoped notice recovery off order-scoped msg-204
// errors when the two integer id spaces collide.
func (c *Connector) isKnownBrokerOrderID(id int) bool {
	if c == nil || id <= 0 {
		return false
	}
	brokerID := strconv.Itoa(id)
	c.orderMu.RLock()
	_, indexed := c.brokerOrderIndex[brokerID]
	_, direct := c.openOrders[brokerID]
	c.orderMu.RUnlock()
	return indexed || direct || c.isWhatIfOrderID(id)
}

func (c *Connector) notifyOrderErrorLifecycleFrom(origin ConnectorSessionBinding, orderID, code int, message, advancedRejectJSON string) {
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.RLock()
	defer c.evidenceBarrier.RUnlock()
	c.notifyOrderErrorLifecycleUnderBarrier(origin, orderID, code, message, advancedRejectJSON)
}

func (c *Connector) notifyOrderErrorLifecycleUnderBarrier(origin ConnectorSessionBinding, orderID, code int, message, advancedRejectJSON string) {
	if orderID <= 0 || code == 0 || orderWhatIfInformationalError(code) {
		return
	}
	brokerID := strconv.Itoa(orderID)
	c.orderMu.RLock()
	_, indexed := c.brokerOrderIndex[brokerID]
	_, direct := c.openOrders[brokerID]
	c.orderMu.RUnlock()
	if !indexed && !direct {
		return
	}
	message = orderBrokerErrorMessage(code, message, advancedRejectJSON)
	ev := OrderLifecycleEvent{
		Type:      OrderLifecycleEventError,
		OrderID:   orderID,
		ErrorCode: code,
		Status:    orderBrokerErrorStatus(code),
		Message:   message,
	}
	c.dispatchOrderLifecycleUnderBarrier(origin, ev, c.SessionReceiptCurrent(origin))
}

func (c *Connector) notifyOrderErrorLifecycle(orderID, code int, message, advancedRejectJSON string) {
	binding, _ := c.CaptureSession()
	c.notifyOrderErrorLifecycleFrom(binding, orderID, code, message, advancedRejectJSON)
}

func (c *Connector) dispatchOrderLifecycle(ev OrderLifecycleEvent) {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	epoch := uint64(0)
	if conn != nil {
		epoch = conn.BrokerSessionEpoch()
	}
	c.dispatchOrderLifecycleFrom(ConnectorSessionBinding{connector: c, connection: conn, epoch: epoch}, ev)
}

func (c *Connector) dispatchOrderLifecycleFrom(origin ConnectorSessionBinding, ev OrderLifecycleEvent) {
	c.publicationBarrier.RLock()
	defer c.publicationBarrier.RUnlock()
	c.evidenceBarrier.RLock()
	defer c.evidenceBarrier.RUnlock()
	c.dispatchOrderLifecycleUnderBarrier(origin, ev, c.SessionReceiptCurrent(origin))
}

func (c *Connector) dispatchOrderLifecycleUnderBarrier(origin ConnectorSessionBinding, ev OrderLifecycleEvent, current bool) {
	c.orderLifecycleGeneration.Add(1)
	c.orderLifecycleMu.RLock()
	handlers := append([]orderLifecycleHandlerEntry(nil), c.orderLifecycle...)
	c.orderLifecycleMu.RUnlock()
	receipt := OrderLifecycleReceipt{Session: origin, Event: ev}
	for _, handler := range handlers {
		if handler.receipt != nil {
			handler.receipt(receipt)
		} else if current && handler.legacy != nil {
			handler.legacy(ev)
		}
	}
}

func orderBrokerErrorStatus(code int) string {
	switch code {
	case 103, 110, 201:
		return "Rejected"
	case 202:
		return "Cancelled"
	default:
		return ""
	}
}

func orderBrokerErrorMessage(code int, message, advancedRejectJSON string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = getErrorDescription(code)
	}
	if advancedRejectJSON = strings.TrimSpace(advancedRejectJSON); advancedRejectJSON != "" {
		if message != "" {
			message += "; "
		}
		message += "advanced_reject_json=" + advancedRejectJSON
	}
	if message == "" {
		return fmt.Sprintf("broker error %d", code)
	}
	return fmt.Sprintf("broker error %d: %s", code, message)
}

// MarketDataSnapshot returns a detached point-in-time copy of all locally
// tracked streaming subscriptions. The returned map and MarketData values may
// be mutated by the caller. Zero fields can represent data not yet observed.
// Timestamp carries the subscription's liveness clock, which is seeded at
// subscribe time; LastTickAt is the tick observation instant and is zero until
// a tick actually arrives. Neither guarantees broker-source freshness.
func (c *Connector) MarketDataSnapshot() map[string]*MarketData {
	c.subMu.RLock()
	defer c.subMu.RUnlock()

	data := make(map[string]*MarketData)

	for symbol, sub := range c.subscriptions {
		data[symbol] = &MarketData{
			Symbol:            symbol,
			Bid:               sub.Bid,
			Ask:               sub.Ask,
			Last:              sub.LastPrice,
			MarkPrice:         sub.MarkPrice,
			BidSize:           int(sub.BidSize),
			AskSize:           int(sub.AskSize),
			Volume:            sub.Volume,
			AvgVolume:         sub.AvgVolume,
			LastTickAt:        sub.LastTickAt,
			LastTradeTime:     sub.LastTradeTime,
			OpenInt:           sub.OpenInt,
			OpenIntObserved:   sub.OpenIntObserved,
			ShortableShares:   sub.ShortableShares,
			ShortableObserved: sub.ShortableObserved,
			Close:             sub.PrevClose,
			Open:              sub.Open,
			High:              sub.High,
			Low:               sub.Low,
			Week13Low:         sub.Week13Low,
			Week13High:        sub.Week13High,
			Week26Low:         sub.Week26Low,
			Week26High:        sub.Week26High,
			Week52Low:         sub.Week52Low,
			Week52High:        sub.Week52High,
			IV:                sub.IV,
			Timestamp:         sub.LastTime,
		}
	}

	return data
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return true
	}
	return false
}

func mapIBOrderType(orderType string) OrderType {
	switch strings.ToUpper(orderType) {
	case "MKT":
		return OrderTypeMarket
	case "LMT":
		return OrderTypeLimit
	case "STP":
		return OrderTypeStop
	case "STP LMT", "STPLMT":
		return OrderTypeStopLimit
	case "MOC":
		return OrderTypeMOC
	case "LOC":
		return OrderTypeLOC
	case "PEG MID", "PEGMID", "PEGMIDPT":
		return OrderTypePegMid
	default:
		return OrderType(strings.ToUpper(orderType))
	}
}

func mapIBTimeInForce(tif string) TimeInForce {
	switch strings.ToUpper(tif) {
	case "DAY":
		return TimeInForceDay
	case "GTC":
		return TimeInForceGTC
	case "IOC":
		return TimeInForceIOC
	case "FOK":
		return TimeInForceFOK
	case "GTD":
		return TimeInForceGTD
	case "OPG":
		return TimeInForceOPG
	default:
		return TimeInForce(strings.ToUpper(tif))
	}
}

func mapIBOrderStatus(status string, filled, remaining float64) OrderStatus {
	s := strings.ToLower(status)
	switch s {
	case "pendingsubmit", "apipending":
		return OrderStatusPending
	case "presubmitted":
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		return OrderStatusSubmitted
	case "submitted", "pendingcancel":
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		if remaining == 0 && filled > 0 {
			return OrderStatusFilled
		}
		return OrderStatusSubmitted
	case "partiallyfilled":
		return OrderStatusPartial
	case "filled":
		return OrderStatusFilled
	case "cancelled", "apicancelled":
		return OrderStatusCancelled
	case "inactive", "rejected", "error":
		return OrderStatusRejected
	case "expired":
		return OrderStatusExpired
	case "completed":
		return OrderStatusFilled
	default:
		if remaining == 0 && filled > 0 {
			return OrderStatusFilled
		}
		if filled > 0 && remaining > 0 {
			return OrderStatusPartial
		}
		return OrderStatus(strings.ToUpper(status))
	}
}

func isTerminalOrderStatus(status OrderStatus) bool {
	switch status {
	case OrderStatusFilled, OrderStatusCancelled, OrderStatusRejected, OrderStatusExpired:
		return true
	default:
		return false
	}
}
