// Package daemon implements the long-running runtime authority for ibkr. It
// owns broker connectivity, durable and in-memory runtime state, background
// schedulers, policy execution, and the gated coordination of broker writes;
// other processes access those capabilities through typed daemon requests.
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"

	"github.com/osauer/ibkr/v2/internal/breadth/spx"
	"github.com/osauer/ibkr/v2/internal/config"
	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	"github.com/osauer/ibkr/v2/internal/daemon/history"
	"github.com/osauer/ibkr/v2/internal/discover"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// maxFrameBytes caps each newline-delimited JSON-RPC request the daemon will
// read from a single Unix-socket peer. Bound is generous (1 MiB) — every
// real CLI/MCP request is well under 10 KiB; the cap exists to prevent a
// hostile or buggy client from OOM'ing the daemon by sending a long
// newline-free byte stream that bufio.ReadBytes would otherwise grow into.
const maxFrameBytes = 1 << 20

// errFrameTooLarge is the sentinel returned by readBoundedLine when a peer
// sends more than maxFrameBytes without a terminating newline. The serve
// loop converts it to a CodeBadRequest response and closes the connection
// (we may be mid-frame and can't resync safely).
var errFrameTooLarge = fmt.Errorf("request frame exceeds %d bytes", maxFrameBytes)

// handshakeWatchdogDelay bounds how long the daemon waits for the IBKR
// handshake to complete before publishing a degraded-state hint to
// lastConnectError. pkg/ibkr's per-attempt budget is 10s; 12s is just past
// that so a healthy attempt always lands first, and a wedged gateway
// surfaces a verdict to `ibkr status` well inside its 25s budget.
const handshakeWatchdogDelay = 12 * time.Second

// perCandidateConnectBudget is the hard cap on one candidate's connect+
// handshake before the failover loop moves on. pkg/ibkr's plain-handshake
// path is bounded internally (~20s for 2 attempts), but its TLS-fallback
// retry uses tls.Conn.HandshakeContext — which only ends when ctx is
// cancelled. Against a host that accepts TCP but never replies to
// ClientHello (e.g. an IB Gateway that's stuck mid-startup, or any
// non-TLS listener), the retry hangs indefinitely and failover never
// advances. This deadline guarantees the loop reaches the alternate
// within a bounded time even in the worst case. 25s matches the
// `ibkr status` budget so a fresh status invocation triggers one full
// candidate attempt; the next status invocation sees the failover result.
//
// `var` (not const) so tests can drop it to milliseconds to exercise
// the deadline branch without sleeping for real.
var perCandidateConnectBudget = 25 * time.Second

// Server is the daemon process state.
type Server struct {
	cfg        *config.Resolved
	socketPath string
	startedAt  time.Time
	version    string
	now        func() time.Time

	// brokerWriteMu serializes the check-then-act sections of every broker
	// write entry point (proposal submit, order place/modify, purge execute,
	// restore execute). These are seconds-long, low-frequency operations;
	// serialization is the correctness tool against double-submit TOCTOU
	// races, not a throughput concern. Cancel stays outside so a protective
	// cancel is never queued behind a long purge.
	brokerWriteMu sync.Mutex

	// reduceBasketMu guards reduceBasketDedupe, the short-TTL replay cache for
	// portfolio-reduce submits. A submit carrying a RequestRef already seen
	// within reduceBasketDedupeTTL replays the prior result and places nothing,
	// so a double-tap or client retry can never fan the basket out twice.
	// Entries are swept on access; the map is lazily allocated.
	reduceBasketMu     sync.Mutex
	reduceBasketDedupe map[string]reduceBasketDedupeEntry

	// paperSmokeMu admits one paper-smoke round-trip at a time; a second
	// concurrent run could overwrite fresh evidence with its own outcome
	// while two smoke orders ride. brokerWriteMu cannot serve here — the
	// smoke only holds it for the place call, not across its ack/cancel
	// waits.
	paperSmokeMu sync.Mutex
	// paperSmokeCancelBudgetOverride shortens the detached cancel budget.
	// Test hook only; zero means the production paperSmokeCancelBudget.
	paperSmokeCancelBudgetOverride time.Duration

	// minTickByConID caches broker-reported minimum price increments per
	// contract (see resolveContractMinTick).
	minTickMu      sync.Mutex
	minTickByConID map[int]float64

	// fxRates keeps last-known-good BASE-per-CCY exchange rates so one
	// failed FX snapshot quote cannot strip base-currency decoration from
	// a single positions/account response (see
	// repairCurrencyLedgerFXRatesCached in fx_cache.go).
	fxRates *fxRateCache

	// accountSnapshots owns the short-lived, request-authored account summary
	// shared by Rulebook, Canary, brief, app, and CLI reads. Its zero value is
	// ready for use; connector session and broker scope are part of every key.
	accountSnapshots accountSnapshotAuthority
	// dailyPnLObservations keeps an observed same-session feed failure visible
	// after the close until a newer valid frame proves recovery.
	dailyPnLObservations dailyPnLObservationAuthority

	listener net.Listener

	mu               sync.Mutex
	endpoint         discover.Endpoint // post-discovery, fully concrete; mutated on reconnect (issue: AUTO rediscover)
	connector        *ibkrlib.Connector
	streams          map[string]context.CancelFunc
	lastConnectError string
	// lastDiscoveryWarn remembers the most recent reconnect-discovery
	// WARN line so the reconnect loop logs each unique verdict once
	// instead of repeating the same line every poll cycle (~500ms while
	// `ibkr status` waits for handshake).
	lastDiscoveryWarn string

	// lastEndpointResolvedSig / lastGatewayUnreachable / lastNoEndpointUsable
	// dedupe the connect-retry log lines the same way lastDiscoveryWarn dedupes
	// discovery. While the gateway is down the daemon rebuilds the connector
	// every cycle (pinned port → discovery returns the same endpoint with
	// derr==nil each time), and without this each cycle re-emits the same
	// "endpoint resolved" INFO and "gateway not connected" / "no endpoint
	// usable" WARN lines — ~50k lines over a 13.5h off-hours window. Each holds
	// the last *surfaced* signature: a matching repeat drops to Debug, a
	// changed signature logs afresh. The two unreachable verdicts are cleared
	// on a successful handshake (resetConnectVerdicts) so the next outage logs
	// its transition again; they are deliberately NOT cleared at the
	// per-candidate / triggerReconnect lastConnectError resets, which fire
	// mid-outage and would defeat the dedupe. lastEndpointResolvedSig is
	// value-keyed on the endpoint and needs no reset.
	lastEndpointResolvedSig string
	lastGatewayUnreachable  string
	lastNoEndpointUsable    string

	// connectInFlight is true while a connect attempt (initial or reconnect)
	// is running its handshake. triggerReconnect refuses to fire while this
	// is set so a stream of `ibkr status` calls during a wedged-gateway
	// recovery doesn't pile up parallel connect goroutines.
	connectInFlight bool
	// initialAcceptLoopStartedForTest observes the exact startup boundary after
	// the RPC accept loop is exposed and before the initial connector goroutine
	// is launched. It lets the startup-order regression test assert that the
	// in-flight gate was already claimed at that boundary.
	initialAcceptLoopStartedForTest func()
	// reconnectFailStreak / lastReconnectAttemptAt drive the reconnect
	// backoff (reconnectAllowed / reconnectBackoff). The streak counts
	// consecutive failed reconnect cycles; it is bumped in reconnectFlow on
	// a failed cycle and reset to 0 by postConnectSetup on a successful
	// handshake. lastReconnectAttemptAt is stamped when triggerReconnect
	// claims the in-flight slot (start of attempt). Both guarded by s.mu.
	reconnectFailStreak    int
	lastReconnectAttemptAt time.Time
	// serverCtx is captured at Start time so handlers can launch
	// reconnect goroutines whose lifetime tracks the daemon, not the
	// short-lived request ctx that triggered the rediscover.
	serverCtx    context.Context
	serverCancel context.CancelFunc

	orderLifecycleHandlersMu sync.Mutex
	orderLifecycleHandlers   map[*ibkrlib.Connector]*orderLifecycleJournalBinding
	// orderLifecycleRegisterAfterCapture is a deterministic test seam for a
	// registration delayed across same-pointer Connector republication.
	orderLifecycleRegisterAfterCapture func()
	// orderLifecycleSessionCurrentForTest lets daemon tests isolate publication
	// identity from pkg/ibkr's intentionally opaque socket receipt.
	orderLifecycleSessionCurrentForTest func(*ibkrlib.Connector, ibkrlib.ConnectorSessionBinding) bool
	// orderLifecyclePersistenceFailures advances whenever a broker lifecycle
	// callback could not be committed to daemon authority. While uncertain is
	// set, journal-derived Protection and Order Integrity negatives fail
	// closed. Only a complete, stable broker reconciliation may clear it.
	orderLifecyclePersistenceFailures  atomic.Uint64
	orderLifecyclePersistenceUncertain atomic.Bool

	idleTimer   *time.Timer
	idleStop    chan struct{}
	activeConns int

	// subs owns refcounted market-data subscriptions shared between the
	// streaming quote handler, the snapshot quote handler, and any MCP
	// resource subscribers. Initialized in New (depends only on Server's
	// own pointer for the connector lookup).
	subs *subManager

	// expiryIVCache memoises per-(symbol, expiry) ATM IV so a fresh
	// `ibkr chain SYM` invocation reuses results from earlier calls
	// within the TTL window. The first call pays the per-expiry
	// subscribe cost; subsequent calls are near-instant.
	expiryIVs *expiryIVCache

	// quoteLiquidity memoises 20-day average volume / dollar volume derived
	// from daily bars. Quote/watch can poll frequently, so this keeps the
	// hard liquidity gate cheap after the first snapshot.
	quoteLiquidity *quoteLiquidityCache

	// prevCloses memoises per-symbol previous-session close (tick 9)
	// so the positions handler can render daily-change deltas without
	// re-subscribing on every invocation. The first `ibkr positions`
	// call after daemon startup pre-warms; later calls are instant.
	prevCloses *prevCloseCache

	// greeks memoises per-option model-computation Greeks so the
	// positions handler doesn't re-subscribe to each option leg on
	// every invocation. Short TTL (60 s) because Greeks shift with
	// spot, but long enough to make back-to-back calls free.
	greeks *greeksCache

	// zeroGamma holds the served dealer zero-gamma last-good plus any RTH
	// soft-TTL refresh running behind it, per scope and NY session key. The
	// compute is a multi-minute fan-out across hundreds of option legs; this
	// cache singleflights concurrent callers and outlives any single RPC ctx so
	// a client disconnect mid-compute doesn't kill work other pollers need.
	zeroGamma *gammaZeroCache
	// gammaOI persists per-contract option open interest observed by the gamma
	// collector. Missing OI refreshes never write through this store; only live
	// tick-101 observations update it, so pre-market/RTH recalcs can carry a
	// known OI state without zero-substituting unknowns.
	gammaOI *gammaOpenInterestStore
	// gammaGrids persists the last successful classed expiry/strike grid
	// per underlying so the gamma compute can ride out sec-def farm
	// outages on a recent prior grid instead of losing the SPX canonical
	// signal for the session (observed 2026-06-09, IBKR code 2157).
	gammaGrids *expiryGridStore
	// gammaStarted guards the boot-time prewarm so it only fires once
	// per Server lifetime. postConnectSetup runs once per successful
	// candidate (including post-reconnect), so without this Once a
	// reconnect-driven second postConnectSetup would re-kick a fresh
	// gamma compute on top of the cached one, doubling gateway slot
	// pressure for no benefit (the soft-TTL refresh in kickOrJoin
	// already keeps the cache from going stale).
	gammaStarted sync.Once
	// gammaRefreshStarted guards the active-session refresh loop. The loop
	// wakes at a short cadence but delegates actual compute decisions to the
	// gamma cache's session-aware soft TTL, so it does not duplicate in-flight
	// forced/user computations.
	gammaRefreshStarted sync.Once

	// breadth runs the SPX 50-DMA breadth compute. The engine owns
	// a persisted constituent-close cache, a once-daily scheduler
	// goroutine launched from postConnectSetup, and a rolling-history
	// file the handler reads for sparkline rendering. nil before
	// installBreadthEngine.
	breadth *spx.Engine
	// breadthStarted guards breadth.Run() so the scheduler goroutine
	// only spins up after the gateway completes its first handshake.
	// postConnectSetup runs once per successful candidate, including
	// post-reconnect — this Once ensures Run() launches exactly once
	// per Server lifetime so we don't end up with parallel daily-tick
	// loops competing on refreshMu.
	breadthStarted sync.Once
	// breadthConnector is a dedicated IBKR client connection (separate
	// clientID, separate handshake, separate rate-limit budget) that
	// backs the breadth refresh's historical-bar fan-out. Carved off
	// the primary connector so breadth's ~503 historical requests don't
	// compete with interactive RPCs and the gamma option-leg fan-out
	// for the primary's 40-msg/sec and 60-historical/10-min budgets.
	// nil before postConnectSetup completes the bulk handshake (or if
	// the bulk handshake fails — breadth gracefully reports
	// "no gateway" for that refresh and retries next tick).
	breadthConnector *ibkrlib.Connector
	// breadthClientStarted guards bulk-connector startup the same way
	// breadthStarted guards Run() — exactly once per Server lifetime
	// across the postConnectSetup re-runs that follow each reconnect.
	breadthClientStarted sync.Once

	// membersRefresher runs the daemon-internal SPX-constituent
	// refresh: daily 02:30 ET fetch from Wikipedia, startup catchup,
	// opportunistic post-rollover trigger from the breadth handler.
	// All three triggers converge on one singleflighted fetch
	// goroutine; the result is written to
	// `~/.cache/ibkr/spx-members/sp500-members.json` and pushed into
	// the breadth engine. nil when disabled by config / env, or when
	// the cache-path resolution failed at New (no $HOME / XDG).
	membersRefresher *spx.Refresher
	// membersRefresherStarted guards Run() launch so a reconnect-
	// driven second postConnectSetup doesn't spawn a duplicate
	// scheduler loop. Mirrors breadthStarted / breadthClientStarted.
	membersRefresherStarted sync.Once
	// membersCachePath is captured at install so handlers reading the
	// loaded file (status renderer) don't have to re-resolve the XDG
	// path on every call. Empty when membersRefresher is nil.
	membersCachePath string

	// contractStore persists symbol→conID resolutions across daemon
	// restarts. IBKR caps reqContractDetails at ~50 per 10 minutes
	// per ACCOUNT (the per-clientID isolation breadthConnector enables
	// doesn't help here — the cap is upstream); every restart that
	// re-resolves the 503 SPX members from scratch pays that bucket
	// over and over. The store loads at postConnectSetup and seeds
	// both connectors, saves every minute + on Stop. Reconstitution
	// is handled via a members-hash field on the file: stale members
	// get pruned at load when the current member list differs from
	// the cached one. nil only on the rare path where
	// DefaultContractStoreDir returns an error (no HOME / XDG_CACHE
	// set) — the daemon continues without persistence in that case.
	contractStore *ibkrlib.ContractStore
	// contractCacheLoaded records what Load() returned so both
	// connectors can be seeded from the same set without double disk
	// reads. Written once in postConnectSetup before the primary
	// connector ready signal, read by startBreadthConnector when the
	// bulk lane comes up. nil before load, possibly empty map after.
	contractCacheLoaded map[string]ibkrlib.ContractDetailsLite
	// contractCacheSaveStarted gates the background save loop to
	// exactly once per Server lifetime, mirroring breadthStarted /
	// breadthClientStarted. postConnectSetup re-runs on each
	// reconnect; without the Once a second postConnectSetup would
	// spawn a duplicate save goroutine racing the first.
	contractCacheSaveStarted sync.Once

	// streaks persists each regime indicator's consecutive-sessions-in-band
	// tally in daemon.db. It is loaded lazily on first Tick and keeps its own
	// invalidation rules: entries persist across days and counters change only
	// on band transitions. The path-backed form exists only for legacy import
	// and isolated unit tests.
	streaks *StreakStore

	// regimeDecisions appends typed regime lifecycle events to daemon.db for
	// threshold calibration. Its path-backed form exists only for legacy import
	// and isolated unit tests; journaling remains best-effort and never fails a
	// snapshot.
	regimeDecisions *regimeDecisionJournal

	// canaryDecisions is the daemon.db-backed portfolio-canary evidence corpus
	// written by the brief hook and the five-minute cadence loop. Its path-backed
	// form has the same legacy import/test-only contract as regimeDecisions.
	canaryDecisions *canaryDecisionJournal

	// rulesJournalMu serializes the legacy file-backed rule-transition seam;
	// daemon.db writes bypass it and commit transactionally in coreStore.
	rulesJournalMu sync.Mutex

	// historyIndex is the legacy history adapter serving regime.history and
	// rules.history. Runtime reads use coreStore; the history.db/file branches
	// remain only for explicit legacy import and isolated tests
	// (internal-docs/design/history-index.md).
	// Deliberately NOT named history: regimeHistory below already names
	// the unrelated HMDS daily-bars cache. Opened by the flock winner in
	// Start, published before the accept loop so handlers read it without
	// synchronization; nil means the legacy adapter is unavailable.
	// historyIndexOpts carries the paths resolved
	// at construction time — New must not touch the DB because every
	// autospawn race loser runs it before the flock decides.
	// The store is published once at Start and read by the ingest and
	// maintenance goroutines, so the pointer itself is atomic.
	historyIndex     atomic.Pointer[history.Store]
	historyIndexOpts *history.Options

	// coreStore is the daemon's sole live persistence authority. New resolves
	// its path but never opens it: only the Start winner may touch daemon.db,
	// after both the socket-specific instance lock and state-root persistence
	// lock have been acquired.
	coreStore        *corestore.Store
	coreStorePath    string
	coreStorePathErr error
	// importLegacyAuthority is true only for the production XDG database.
	// A custom database path is isolated and always starts as a fresh epoch;
	// it must never inspect the user's live legacy files.
	importLegacyAuthority bool
	persistenceLock       *persistenceLock
	authorityCloseOnce    sync.Once
	authorityCloseErr     error

	// tradingReadiness persists daemon-owned evidence for local write
	// gates, starting with the recent paper-smoke proof required before
	// live mode. It deliberately lives outside config so a TOML edit cannot
	// fake runtime readiness.
	tradingReadiness *tradingReadinessStore
	// orderJournal is the durable audit log for order intents and broker
	// lifecycle events. It is installed before order writes exist so status
	// and later handlers share one state primitive.
	orderJournal *orderJournalStore
	// orderSnapshotFn is the open-order snapshot seam for the reconcile
	// sweep; nil resolves to the live connector's SnapshotOpenOrders.
	orderSnapshotFn func(context.Context) (ibkrlib.OpenOrderSnapshot, error)
	// Test-only final-boundary seams. Production leaves these nil. They run
	// after evidence assembly and immediately before the stable commit/CAS.
	orderReconcileBeforeCommit     func()
	orderReconcileBeforeLatchClear func()
	protectionBeforeCommit         func()
	orderIntegrityBeforeCommit     func()
	stableBrokerEvidenceForTest    func(daemonBrokerEvidenceBinding, func() error) (bool, error)
	// protectionOrderSnapshot* retains only a short-lived, complete broker
	// inventory receipt for the 30-second Protection producer heartbeat. The
	// binding includes scope, connector epoch, and Connector order-lifecycle
	// generation; any later broker order event makes the receipt ineligible.
	protectionOrderSnapshotMu     sync.Mutex
	protectionOrderSnapshotCache  protectionOrderSnapshotCache
	protectionOrderSnapshotFlight *protectionOrderSnapshotFlight
	// orderReconcileLoopStarted gates the standing reconcile sweep to one
	// goroutine per Server lifetime across reconnect-driven
	// postConnectSetup re-runs.
	orderReconcileLoopStarted sync.Once
	// purgeLedger is the fill-backed purge/restore book. It is reduced from
	// broker lifecycle evidence, not from preview/send attempts, so restore
	// cannot double a position merely because a local file was edited.
	purgeLedger *purgeLedgerStore
	// proposalOutcomes is the append-only measurement book for protection
	// proposals. It records submitted/fill/mark evidence keyed to proposal
	// identity and order-journal refs without storing raw preview tokens.
	proposalOutcomes *proposalOutcomeStore
	// platformSettings persists daemon-owned runtime preferences. Gateway,
	// account, trading mode, and build capability stay config/build owned;
	// this store only carries settings ibkr may edit at runtime.
	platformSettings    *platformSettingsStore
	protectionPolicies  *protectionPolicyManager
	tradeProposals      *proposalEngine
	opportunityPolicies *opportunityPolicyManager
	opportunities       *opportunityEngine
	marketEvents        *marketEventCache
	// riskPolicies loads the operator's risk constitution
	// (risk-policy.toml); riskCapital owns the cash-flow-adjusted peak,
	// drawdown latch, capital events, overrides, and cadence records.
	// Advisory/shadow end to end in v1: neither may reach broker-write
	// authorization.
	riskPolicies *riskPolicyManager
	riskCapital  *riskCapitalStore
	// nudges owns only opaque governance occurrence state. Eligibility remains
	// a pure projection over risk/recon/capital/pin authorities.
	nudges *nudgeStateStore
	// nudgeWriteMu serializes the final compare-and-persist step for advisory
	// governance evidence. It does not freeze policy, pins, Flex, recon, or
	// capital; durable authority identities make a raced write inert.
	nudgeWriteMu          sync.Mutex
	monthlyRenderMu       sync.Mutex
	monthlyRenderReceipts map[string]monthlyRenderReceipt
	// Test-only deterministic seams. Production leaves all nil.
	nudgeBeforeCommit          func(string)
	nudgeAfterValidation       func(string)
	nudgeAfterPersist          func(string)
	nudgeScanCheckpoint        func(string)
	shadowBookkeepingHook      func()
	monthlyRenderBeforeIssue   func()
	monthlyRenderBeforePersist func()
	monthlyAckBeforeWriteLock  func()
	// briefState persists only human render-stamps and their rulebook delta
	// baselines. brief.snapshot reads it but never writes it.
	briefState *briefStateStore
	// reconMu serializes report-content mutations with report-backed human
	// and automatic reconcile appends so a report id cannot race a new
	// declaration or dismissal.
	reconMu sync.Mutex
	// flexFetch tracks the daily Flex statement ingestion for post-trade
	// reconciliation (internal-docs/design/post-trade-truth.md). Read-only toward
	// the broker; sanitized status only, never the token.
	flexFetch flexFetchState
	// Test-only seams for the broker fetch and retained-statement projection.
	// Production leaves both nil and uses the concrete implementations.
	flexFetchOnceFn  func(context.Context, time.Time) (flexFetchOutcome, error)
	flexProjectionFn func(context.Context) error
	// earnings backs the trading rulebook's catalyst rules (6-8); LKG cache,
	// async refresh only — never fetched on a snapshot or preview path.
	earnings *earningsCache
	// earningsTerminal is exact-contract, provenance-bound terminal issuer
	// evidence loaded from daemon.db. An optional operator file can update the
	// SQLite authority at startup; symbol text alone never grants an exemption.
	earningsTerminal *earningsTerminalStore
	// lastRules memoizes the most recent rulebook evaluation for advisory
	// preview causes (rulesPreviewTTL) and the transitions journal.
	rulesEvaluationMu       sync.Mutex
	rulesMu                 sync.Mutex
	lastRules               *rpc.RulesResult
	lastRulesAt             time.Time
	lastRulesScope          brokerStateScope
	lastRulesConnector      *ibkrlib.Connector
	lastRulesConnectorEpoch uint64
	lastRulesBroker         ibkrlib.BrokerEvidenceBinding
	lastRulesBrokerCaptured bool
	rulesRefreshWake        chan struct{}
	// connectorEpoch changes whenever the daemon publishes or removes the
	// primary connector. Rulebook cache/journal bindings use it to invalidate
	// evidence across reconnects even when account/mode text is unchanged.
	connectorEpoch uint64
	// rulesRegimeStage latches the bucketed regime lifecycle stage for the
	// rulebook's regime-conditional thresholds, persisted across restarts
	// (rules-regime-stage.json) so a bounce mid-stress cannot reset
	// thresholds to calm. The kick fields single-flight the async refresh.
	rulesRegimeStageMu     sync.Mutex
	rulesRegimeStage       rulesRegimeStageState
	rulesRegimeStageLoaded bool
	rulesRegimeKickAt      time.Time
	rulesRegimeKickBusy    atomic.Bool
	proposalsStarted       sync.Once
	opportunitiesStarted   sync.Once
	// orderTokens signs preview tokens. Tokens are local intent artifacts;
	// they are not broker orders and cannot submit anything until a separate
	// gated place handler exists.
	orderTokens *orderTokenSigner
	// orderPreview* hooks let tests exercise the full preview gate/token path
	// without a live gateway. Nil hooks use the production connector-backed
	// implementations.
	orderPreviewQuote            func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error)
	orderPreviewPositionImpact   func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error)
	orderRiskAuthorityForTest    func(context.Context, rpc.TradingStatus, rpc.ContractParams, string, int) (orderPositionAuthority, error)
	orderFXRateForTest           func(context.Context, string, string, time.Duration) (float64, time.Time, error)
	orderContractResolverForTest func(context.Context, rpc.ContractParams, time.Duration) (rpc.ContractParams, error)
	orderPreviewWhatIf           func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error)
	purgeRefreshPositions        func() ([]*ibkrlib.RawPosition, error)
	orderWritesEnabled           func() bool
	gatewayReadyForTrading       func() bool
	orderReserveBrokerID         func(context.Context) (int, error)
	orderPlaceBroker             func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error
	orderCancelBroker            func(context.Context, int) error
	optionExerciseBroker         func(context.Context, ibkrlib.OptionExerciseRequest) error
	// orderWriteBindingForTest and orderWriteBeforeBrokerSend are deterministic
	// transaction-boundary seams for trading-tag tests. Production leaves both
	// nil; broker writes then require a real ready Connector session.
	orderWriteBindingForTest        func(rpc.TradingStatus) (*ibkrlib.Connector, uint64, ibkrlib.ConnectorSessionBinding, brokerStateScope)
	orderWriteBeforeBrokerSend      func()
	orderWriteOriginBlockersForTest func(rpc.TradingStatus, string) []rpc.TradingBlocker

	// regimePrewarming is set while prewarmRegimeSymbols' fan-out is in
	// flight. Surfaces via backgroundTasks() so the idle watcher defers
	// shutdown and `ibkr status` reflects the work — same coherence
	// guarantee breadth-spx and gamma-zero ride. Up to ~30 s of
	// gateway-slot pressure during postConnectSetup; if the user
	// autospawns the daemon and walks away, the idle watcher could
	// previously fire mid-prewarm.
	regimePrewarming atomic.Bool
	// regimeSeries memoises official daily public-rate series used by
	// regime rows. These inputs change once per
	// business day, so persisting the last good CSV across daemon restarts
	// prevents routine HTTP flaps from making credit/funding rows vanish.
	regimeSeries *regimeSeriesCache
	// regimeHistory memoises daily HMDS bars used as slow baselines for
	// regime rows. These values rank HYG 50-DMA, SPY 52w high fallback,
	// and USD/JPY weekly change; transient HMDS failures must not make
	// the high-impact regime surface flap between ranked and unranked.
	regimeHistory *regimeHistoryCache
	// regimeSnapshots is the daemon-owned, daemon.db-backed last-good regime
	// authority. All RPC, brief, rulebook, proposal, Canary, and alert reads
	// converge here; only a complete fan-out may publish into it.
	regimeSnapshots          *regimeSnapshotCache
	regimeProjectionRepairMu sync.Mutex
	regimeRefreshLoopWG      sync.WaitGroup
	// Canary evaluation is daemon-owned and independent of app presence and
	// decision-journal retention. Regime publications and reconnects share the
	// buffered wake; the loop drains during daemon shutdown.
	canaryEvaluationWake   chan struct{}
	canaryEvaluationLoopWG sync.WaitGroup
	// canaryEvaluationSourceReaderForTest exercises the real evaluator against
	// typed source receipts without constructing a broker wire session.
	canaryEvaluationSourceReaderForTest canaryEvaluationSourceReader
	rulebookRefreshLoopWG               sync.WaitGroup
	regimeConsumerWakeMu                sync.Mutex
	regimeConsumerRevision              int64
	// Successful canonical gamma publications invalidate only Regime's refresh
	// schedule and signal this capacity-one daemon-owned wake. The prior Regime
	// authority remains visible until a complete replacement publishes.
	regimeRefreshWake chan struct{}
	// alertShadow is the daemon-owned source-neutral alert producer. It persists
	// lifecycle through alertEpisodes; the app dispatcher owns targets,
	// delivery attempts, badges, and service-worker transport.
	alertEpisodes *alertEpisodeRegistry
	alertShadow   *alertShadowComposer
	// dataHealthObserveMu keeps the hottest read-only status path independent
	// from alert registry persistence. Only one detached observation may run at
	// once;
	// failures back off in memory so an unhealthy SQLite store cannot turn
	// status polling into a write storm against itself. Adjacent identical
	// samples coalesce, but semantic transitions remain ordered so an outage
	// observed before its recovery cannot disappear while a write is in flight.
	dataHealthObserveMu      sync.Mutex
	dataHealthObserveRunning bool
	dataHealthObserveStopped bool
	dataHealthObservePending map[string][]alertShadowDataHealthPending
	dataHealthObserveRetryAt map[string]time.Time
	dataHealthObserveWake    chan struct{}
	dataHealthObserveWG      sync.WaitGroup
	dataHealthObserveTest    func(context.Context, alertShadowDataHealthInput) error
	dataHealthObserveBackoff time.Duration
	alertShadowLoopWG        sync.WaitGroup
	// postConnectSetupDone latches true at the end of the first
	// successful postConnectSetup. Gates the Connected field of
	// handleStatusHealth so an `ibkr status` that lands between
	// `c.ready=true` (set asynchronously by the connection read loop
	// in pkg/ibkr) and postConnectSetup finishing its synchronous
	// prewarm sentinel-setting reports Connected=false (the CLI keeps
	// polling) rather than Connected=true with empty BackgroundTasks.
	// Without this latch, the FIRST status after restart could miss
	// the imminent gamma-zero / breadth-spx / regime-prewarm work —
	// the daemon is technically handshaken but not yet ready to
	// publish its full background-task surface.
	//
	// Latched true once and not reset on reconnect: subsequent
	// reconnects reuse the same prewarm machinery (Once-guarded), so
	// the daemon never re-enters a "starting up" state from the
	// user's point of view. A daemon panic during postConnectSetup
	// crashes the process (no recover at that level), so the flag
	// never stays false forever in practice.
	postConnectSetupDone atomic.Bool

	lock *instanceLock

	logger *Logger

	// attempterFactory builds a connectAttempter for a candidate endpoint
	// during the failover loop. Production uses buildAttempter (which
	// wraps newConnector). Tests override this to inject a fake that
	// decides per-port whether the handshake "succeeds." Set by New;
	// callers that construct *Server directly (legacy tests) get a nil
	// factory and must either assign it themselves or only exercise
	// code paths that don't connect.
	attempterFactory func(discover.Endpoint) connectAttempter
}

// Options configures a Server.
type Options struct {
	Config     *config.Resolved
	SocketPath string
	Version    string
	Logger     *Logger
	// StateDatabasePath overrides daemon.db for isolated tests and offline
	// verification. Production leaves it empty and uses the XDG state root.
	StateDatabasePath string
}

// New constructs a Server with the supplied options.
func New(opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = NewLogger(os.Stderr, opts.Config.Daemon.LogLevel)
	}
	s := &Server{
		cfg:            opts.Config,
		socketPath:     opts.SocketPath,
		version:        opts.Version,
		now:            time.Now,
		streams:        map[string]context.CancelFunc{},
		idleStop:       make(chan struct{}),
		logger:         opts.Logger,
		expiryIVs:      newExpiryIVCache(),
		quoteLiquidity: newQuoteLiquidityCache(),
		prevCloses:     newPrevCloseCache(),
		greeks:         newGreeksCache(),
		zeroGamma:      newGammaZeroCache(),
		fxRates:        newFXRateCache(),
	}
	if opts.StateDatabasePath != "" {
		s.coreStorePath = opts.StateDatabasePath
	} else {
		s.coreStorePath, s.coreStorePathErr = defaultDaemonDatabasePath()
		s.importLegacyAuthority = true
	}
	s.attempterFactory = s.buildAttempter
	s.installSubs()
	s.installBreadthEngine()
	s.installMembersRefresher()
	s.installContractStore()
	s.installStreakStore()
	s.installCanaryDecisionJournal()
	s.installOrderJournalStore()
	s.installPurgeLedgerStore()
	s.installProposalOutcomeStore()
	s.installPlatformSettingsStore()
	s.installProtectionPolicyManager()
	s.installRiskPolicyManager()
	s.installRiskCapitalStore()
	s.installNudgeStateStore()
	s.installBriefStateStore()
	s.installProposalEngine()
	s.installOpportunityPolicyManager()
	s.installOpportunityEngine()
	s.installMarketEventCache()
	s.installRegimeSeriesCache()
	s.installRegimeHistoryCache()
	s.installGammaZeroCache()
	s.installFXRateCache()
	s.installEarningsCache()
	s.installEarningsTerminalStore()
	if s.zeroGamma != nil {
		s.zeroGamma.setPublicationCallback(s.handleGammaPublication)
	}
	return s
}

// installFXRateCache installs the legacy codec path without reading it.
// Server.New runs before the persistence lock; only the unpublished cutover
// importer may read legacy JSON. Start later attaches daemon.db and loads its
// current FX projection.
func (s *Server) installFXRateCache() {
	dir, err := fxRateStoreDefaultDir()
	if err != nil {
		s.logger.Warnf("fx rate cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.fxRates = newFXRateCacheWithStoreCold(newFXRateStore(dir), time.Now, s.logger)
}

// installEarningsCache installs the legacy codec path cold for the same
// pre-lock reason as installFXRateCache. Start replaces it with daemon.db.
func (s *Server) installEarningsCache() {
	dir, err := fxRateStoreDefaultDir()
	if err != nil {
		s.logger.Warnf("earnings cache: resolve dir: %v (persistence disabled)", err)
		dir = ""
	}
	cache := newEarningsCacheCold(dir, s.logger.Warnf)
	if err := cache.setSecondaryProvider(earningsWSHProvider, s.fetchWSHEarningsProvider); err != nil {
		s.logger.Warnf("earnings cache: install IBKR WSH provider: %v", err)
	}
	if err := cache.setIdentityFetcher(s.fetchEarningsIdentity); err != nil {
		s.logger.Warnf("earnings cache: install broker identity reader: %v", err)
	}
	s.earnings = cache
}

func (s *Server) installEarningsTerminalStore() {
	path := ""
	if s.cfg != nil {
		path = s.cfg.Rulebook.TerminalEvidenceFile
	}
	s.earningsTerminal = newEarningsTerminalStore(path)
}

// installGammaZeroCache replaces the bootstrap in-memory gamma cache
// with a store-backed one. Best-effort: a missing XDG_CACHE_HOME /
// HOME pair leaves the in-memory cache in place — every restart pays
// the full ~5 min compute, but the daemon itself starts fine.
//
// When the store is attached, any persisted result from today's NY
// session is loaded and installed as `current`, so the first caller
// after restart skips the compute. See
// docs/docs/internals/gamma-cache.md for the cost/benefit
// rationale (cold combined runs can cross the 5-min threshold).
func (s *Server) installGammaZeroCache() {
	dir, err := gammaZeroStoreDefaultDir()
	if err != nil {
		s.logger.Warnf("gamma cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.zeroGamma = newGammaZeroCacheWithStore(newGammaZeroStore(dir), time.Now(), s.logger)
	if diagPath, diagErr := gammaSkewDiagDefaultPath(); diagErr == nil {
		s.zeroGamma.skewDiag = &gammaSkewDiagJournal{path: diagPath}
	} else {
		s.logger.Warnf("gamma skew diag: resolve path: %v (journaling disabled)", diagErr)
	}
	s.gammaOI = newGammaOpenInterestStore(dir)
	s.gammaGrids = newExpiryGridStore(dir)
}

// installStreakStore constructs the regime-streak persistence layer.
// Best-effort: a missing XDG_CACHE_HOME / HOME pair leaves streaks
// nil and the daemon runs without persistence (every restart resets
// the counters). The regime-snapshot path nil-guards before calling
// Tick so the rest of the row population continues unaffected.
func (s *Server) installStreakStore() {
	dir, err := DefaultStreakStoreDir()
	if err != nil {
		s.logger.Warnf("regime streaks: resolve cache dir: %v (counters disabled)", err)
		return
	}
	s.streaks = NewStreakStore(dir)
	if path, err := regimeDecisionsDefaultPath(); err != nil {
		s.logger.Warnf("regime decisions: resolve state path: %v (journal disabled)", err)
	} else {
		s.regimeDecisions = &regimeDecisionJournal{path: path}
	}
}

func (s *Server) installOrderJournalStore() {
	path, err := defaultOrderJournalPath()
	if err != nil {
		s.warnf("order journal: resolve state path: %v (order audit disabled)", err)
		return
	}
	s.orderJournal = newOrderJournalStore(path)
	s.installOrderIndexReads()
}

func (s *Server) installPurgeLedgerStore() {
	path, err := defaultPurgeLedgerPath()
	if err != nil {
		s.warnf("purge ledger: resolve state path: %v (purge ledger disabled)", err)
		return
	}
	s.purgeLedger = newPurgeLedgerStore(path, s.now)
}

func (s *Server) installPlatformSettingsStore() {
	path, err := defaultPlatformSettingsPath()
	if err != nil {
		s.warnf("platform settings: resolve state path: %v (runtime settings disabled)", err)
		return
	}
	// New runs before the instance/persistence locks are won. Resolve the
	// legacy path for the explicit cutover only; never read it here. The Start
	// winner imports it into an unpublished daemon.db, then bindCore publishes
	// the authoritative value.
	s.platformSettings = &platformSettingsStore{path: path, data: platformSettingsData{Version: platformSettingsDocVersion}}
}

func (s *Server) installRegimeSeriesCache() {
	dir, err := regimeSeriesCacheDefaultDir()
	if err != nil {
		s.logger.Warnf("regime series cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.regimeSeries = newRegimeSeriesCache(dir)
}

func (s *Server) installRegimeHistoryCache() {
	dir, err := regimeHistoryCacheDefaultDir()
	if err != nil {
		s.logger.Warnf("regime history cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.regimeHistory = newRegimeHistoryCache(dir)
}

// infof / warnf are nil-safe wrappers around s.logger. The tests that
// construct a zero-value Server directly (breadth_connector_test.go)
// reach installBreadthEngine / installMembersRefresher with logger=nil;
// these wrappers let those tests keep working without seeding a logger
// the test doesn't need.
func (s *Server) infof(format string, args ...any) {
	if s.logger != nil {
		s.logger.Infof(format, args...)
	}
}

func (s *Server) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warnf(format, args...)
	}
}

func (s *Server) debugf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Debugf(format, args...)
	}
}

// installContractStore constructs the legacy contract-cache codec used to
// locate cutover input. Runtime attachment switches it to daemon.db before any
// load; if legacy path resolution fails, attachCoreMarketAuthority installs a
// cold codec directly against SQLite.
func (s *Server) installContractStore() {
	dir, err := ibkrlib.DefaultContractStoreDir()
	if err != nil {
		s.logger.Warnf("contract cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.contractStore = ibkrlib.NewContractStore(dir)
}

// installBreadthEngine builds the SPX 50-DMA breadth engine and
// attaches it to s. Construction is best-effort: a failure to resolve
// the on-disk cache dir is logged but does not block daemon startup —
// the engine field stays nil and handleBreadthSPX surfaces
// status="unavailable" with a notes pointer.
//
// The fetcher closes over s.breadthGatewayConnector — the dedicated
// bulk-historical client — so breadth's ~500-name fan-out runs against
// its own IBKR clientID, separate from the primary connector that
// serves interactive RPCs and the gamma compute. Reading the live
// pointer on every call lets the engine survive a bulk-connector
// reconnect without re-instantiation, mirroring the primary-thunk
// pattern.
//
// Members seeding: resolveBreadthMembers, deferred via
// spx.Options.MembersFn to the engine's first actual members use —
// not read at construction. daemon.New runs before Server.Start
// acquires the single-instance flock, so every autospawn race loser
// builds a full Server; an eager read here made each loser re-read
// the members cache and append its own "loaded N members from cache"
// INFO line to the shared daemon log (2026-06-09: ~10 interleaved
// lines per spawn burst, same mechanism as the gamma persisted-cache
// fix). The winning daemon logs the source exactly once, on first
// breadth use, so a human triaging breadth values can still confirm
// which list is in play.
func (s *Server) installBreadthEngine() {
	dir, err := spx.DefaultDir()
	if err != nil {
		s.logger.Warnf("breadth: resolve cache dir: %v (engine disabled)", err)
		return
	}
	fetcher := newBreadthFetcher(s.breadthGatewayConnector)
	s.breadth = spx.New(spx.NewStore(dir), fetcher, spx.Options{
		Logger: s.logger, MembersFn: s.resolveBreadthMembers, DeferStoreLoad: true,
	})
}

// resolveBreadthMembers is the deferred members source for the breadth
// engine (see installBreadthEngine for why it must not run at
// construction). Prefers the runtime-refreshed daemon.db membership
// projection over the embedded list, so a daemon installed from a months-old
// release that has since persisted a fresher list serves current membership
// immediately. Falls back to the embedded list when no valid projection is
// available. Logs the chosen source — the engine's sync.Once gate
// guarantees at most one line per process lifetime.
func (s *Server) resolveBreadthMembers() []string {
	if path, perr := spx.MembersDefaultPath(); perr == nil {
		if loaded, asOf, ok := spx.LoadExternal(path); ok {
			s.infof("breadth: loaded %d members from cache (as_of %s)", len(loaded), asOf.Format("2006-01-02"))
			return loaded
		}
	}
	embedded, asOf := spx.MemberList()
	s.infof("breadth: using embedded members list (%d names, as_of %s)", len(embedded), asOf.Format("2006-01-02"))
	return embedded
}

// installMembersRefresher stands up the runtime SPX-members refresher.
// The refresher fetches Wikipedia's constituent list daily at 02:30 ET plus on
// startup catch-up, commits current state and an observation to daemon.db, and
// pushes new lists into the breadth engine.
//
// Construction is best-effort:
//   - breadth engine missing (cache-dir failure) → no refresher.
//   - members cache path unresolvable (HOME unset) → no refresher.
//
// The IBKR_SPX_MEMBERS_AUTO_REFRESH env var (symmetric override:
// "1" force-on, "0" force-off, unset defers) takes precedence over the
// TOML [spx] members_auto_refresh field. The status renderer surfaces
// which gate is active. Run() is launched separately in
// postConnectSetup so the refresher's goroutine lifetime tracks
// serverCtx.
func (s *Server) installMembersRefresher() {
	if s.breadth == nil {
		return
	}
	cachePath, err := spx.MembersDefaultPath()
	if err != nil {
		s.warnf("members refresh: resolve cache path: %v (refresher disabled)", err)
		return
	}

	// Resolve env+config to a single enabled/disabled decision plus
	// the reason. Env wins when present; otherwise TOML governs. The
	// reason flags drive the status row's `disabled (env)` /
	// `disabled (config)` suffix so a confused user knows which knob
	// to flip.
	envEnabled, envForced := config.SPXMembersAutoRefreshFromEnv()
	configEnabled := s.cfg.SPX.MembersAutoRefreshEnabled()

	var enabled, pinnedByEnv, pinnedByConfig bool
	switch {
	case envForced:
		enabled = envEnabled
		pinnedByEnv = !envEnabled
	default:
		enabled = configEnabled
		pinnedByConfig = !configEnabled
	}
	_ = enabled // refresher derives state from the Pinned* flags

	version := s.version
	fetch := func(ctx context.Context) ([]string, time.Time, error) {
		return spx.FetchAndParse(ctx, spx.WikipediaURL, version)
	}
	s.membersRefresher = spx.NewRefresher(spx.RefresherOptions{
		Engine:         s.breadth,
		CachePath:      cachePath,
		Fetch:          fetch,
		Logger:         s.logger,
		PinnedByConfig: pinnedByConfig,
		PinnedByEnv:    pinnedByEnv,
	})
	s.membersCachePath = cachePath
}

// breadthGatewayConnector returns the bulk-historical IBKR connector
// dedicated to the breadth refresh, or nil if it hasn't completed
// its handshake yet. Snapshot under s.mu mirrors gatewayConnector's
// read pattern so handlers reading the pointer never see it mid-Stop.
//
// Distinct from gatewayConnector: this one does NOT trigger a
// reconnect on nil. Bulk-connector failure is non-fatal — breadth
// surfaces "no gateway" for the affected refresh and the next 16:35
// ET tick retries from scratch. Auto-reconnect machinery for the
// bulk lane would double the lifecycle surface without changing the
// observable outcome (breadth still retries).
func (s *Server) breadthGatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.breadthConnector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		return nil
	}
	return c
}

// startBreadthConnector launches the bulk-historical IBKR client and
// blocks until handshake completes or breadthClientStartBudget elapses.
// Called once per Server lifetime from postConnectSetup — gated by
// breadthClientStarted so a reconnect-driven second postConnectSetup
// doesn't spin up a duplicate bulk connection.
//
// The handshake is intentionally synchronous: breadth.Run() launches
// shortly after on a sibling goroutine and would otherwise race a
// not-yet-ready bulk connector, dropping all 503 fetches against nil
// on the first refresh. 12 s mirrors the primary's per-candidate
// budget — long enough for a healthy local gateway, short enough to
// surface a misconfigured second cid promptly.
//
// On failure (collision past MaxClientIDRetries, gateway unreachable,
// handshake timeout) the function logs and returns without setting
// s.breadthConnector. Breadth's refresh sees a nil bulk connector
// and aborts gracefully; the daemon as a whole continues running.
func (s *Server) startBreadthConnector(ctx context.Context, primaryEp discover.Endpoint) {
	bulkEp := primaryEp
	bulkEp.ClientID = s.cfg.Gateway.BreadthClientIDOrDefault()

	attempter := s.newConnector(bulkEp)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		if err := attempter.Start(ctx); err != nil {
			s.logger.Warnf("breadth bulk connector start: %v", err)
		}
	}()

	select {
	case <-startDone:
	case <-time.After(breadthClientStartBudget):
		s.logger.Warnf("breadth bulk connector: handshake timeout after %s (cid=%d); refresh will use 'no gateway' fallback until next tick", breadthClientStartBudget, bulkEp.ClientID)
		_ = attempter.Stop()
		return
	case <-ctx.Done():
		_ = attempter.Stop()
		return
	}

	if !attempter.IsReady() {
		s.logger.Warnf("breadth bulk connector: not ready after Start (cid=%d); skipping", bulkEp.ClientID)
		_ = attempter.Stop()
		return
	}

	s.mu.Lock()
	s.breadthConnector = attempter
	s.mu.Unlock()
	s.logger.Infof("breadth bulk connector ready (cid=%d, separate 40-msg/sec budget from primary)", bulkEp.ClientID)

	// Seed the bulk lane from the same persisted contract cache that
	// the primary lane was seeded from in postConnectSetup. ConIDs are
	// globally unique so both lanes get the same wire identity for
	// every symbol — no contract resolution churn on the bulk side
	// across daemon restarts. seedFromContractStore is a no-op if the
	// store wasn't successfully loaded.
	s.seedConnectorFromCache(attempter)
}

// stopBreadthConnector tears down the bulk-historical connector if one
// was successfully started. Safe to call when nil. Mirror of
// stopConnector's lifecycle discipline: nil under mu so handlers can't
// observe a half-stopped pointer.
func (s *Server) stopBreadthConnector() {
	s.mu.Lock()
	c := s.breadthConnector
	s.breadthConnector = nil
	s.mu.Unlock()
	if c == nil {
		return
	}
	if err := c.Stop(); err != nil {
		s.logger.Warnf("breadth bulk connector Stop: %v", err)
	}
}

// breadthClientStartBudget bounds the bulk-historical handshake. Set
// to match the primary's perCandidateConnectBudget so a misconfigured
// second cid (collision, gateway rejecting client IDs, etc.) doesn't
// stretch postConnectSetup beyond a sensible boot delay.
const breadthClientStartBudget = 12 * time.Second

// contractCacheSaveInterval is how often the background loop reads
// both connectors' contractCache, merges the entries, and writes the
// result to ContractStore. 60s balances transaction cost against the "how much
// recent work would we lose on a crash?" risk; the worst case is one minute of
// resolutions which the next daemon restart re-fetches transparently.
const contractCacheSaveInterval = 60 * time.Second

// seedFromContractStore loads the persisted contract cache from daemon.db
// and seeds the supplied connector with each non-stale entry. Stale
// is defined relative to the current SPX members list (entries whose
// symbol isn't in the current sp500Members AND isn't one of the
// well-known seeds get pruned at load — they're delisted, renamed,
// or simply replaced by reconstitution).
//
// Also caches the loaded map in s.contractCacheLoaded for
// startBreadthConnector to seed the bulk lane without a second authority read.
// No-op when contractStore is nil or the SQLite document does not exist.
func (s *Server) seedFromContractStore(c *ibkrlib.Connector) {
	if s.contractStore == nil || c == nil {
		return
	}
	loaded, savedHash, err := s.contractStore.Load()
	if err != nil {
		s.logger.Warnf("contract cache: load: %v (will start cold)", err)
		return
	}
	if loaded == nil {
		s.logger.Infof("contract cache: no daemon.db state yet, starting cold")
		s.contractCacheLoaded = map[string]ibkrlib.ContractDetailsLite{}
		return
	}
	members, _ := spx.MemberList()
	currentHash := ibkrlib.MembersHash(members)
	if savedHash != "" && savedHash != currentHash {
		// SPX reconstituted since the last save. Prune entries whose
		// symbol isn't in the current list — keep the well-known
		// seeds (SPX, VIX, etc.) regardless since they aren't SPX
		// members but are still useful for regime / gamma paths.
		loaded = pruneNonMembers(loaded, members)
		s.logger.Infof("contract cache: SPX members hash changed (%s → %s); pruned to %d current-member entries", savedHash, currentHash, len(loaded))
	}
	s.contractCacheLoaded = loaded
	seeded := 0
	for sym, detail := range loaded {
		if c.SeedContractDetails(sym, detail) {
			seeded++
		}
	}
	s.logger.Infof("contract cache: seeded %d entries from daemon.db", seeded)

	// Option contracts (added in store v2). Expired entries are GC'd
	// at the store layer; whatever remains is still valid for at least
	// today's NY session. Pre-seeds the connector's optionContractCache
	// so the next gamma prewarm finds most strikes already resolved
	// and avoids the 40s-per-fan-out round-trip cost on warm restarts.
	if opts, err := s.contractStore.LoadOptions(); err != nil {
		s.logger.Warnf("contract cache: load options: %v", err)
	} else if seededOpts := c.SeedOptionContracts(opts); seededOpts > 0 {
		s.logger.Infof("contract cache: seeded %d option entries from daemon.db", seededOpts)
	}
}

// seedConnectorFromCache seeds c from the already-loaded
// s.contractCacheLoaded map. Called by startBreadthConnector when the
// bulk lane comes up; avoids a second disk read and guarantees both
// lanes see the same seed set. No-op if seedFromContractStore hasn't
// run yet (primary connector failed to come up) or if c is nil.
func (s *Server) seedConnectorFromCache(c *ibkrlib.Connector) {
	if c == nil || len(s.contractCacheLoaded) == 0 {
		return
	}
	seeded := 0
	for sym, detail := range s.contractCacheLoaded {
		if c.SeedContractDetails(sym, detail) {
			seeded++
		}
	}
	s.logger.Infof("contract cache: seeded %d entries into bulk connector", seeded)
}

// contractCacheSaveLoop runs for the daemon's lifetime, periodically
// snapshotting both connectors' contractCache and saving the merged
// result to ContractStore. The merge prefers entries from whichever
// connector has them — they're identical when both lanes have
// resolved the same symbol (conIDs are globally unique), and the
// merge picks any one. Returns when ctx is cancelled (daemon shutting
// down); Stop() runs a final saveContractCache after this to capture
// the last minute's work.
func (s *Server) contractCacheSaveLoop(ctx context.Context) {
	t := time.NewTicker(contractCacheSaveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.saveContractCache()
		}
	}
}

// saveContractCache snapshots both connectors' contractCaches, merges
// them, and writes the result. Errors are logged but not propagated —
// a transient I/O failure shouldn't take the daemon down; the next
// tick retries. Safe to call when ContractStore is nil or when
// neither connector is up (no-op).
func (s *Server) saveContractCache() {
	if s.contractStore == nil {
		return
	}
	merged := map[string]ibkrlib.ContractDetailsLite{}
	for _, c := range []*ibkrlib.Connector{s.gatewayConnector(), s.breadthGatewayConnector()} {
		if c == nil {
			continue
		}
		// Both connectors should resolve to the same ConID for a
		// given symbol; if they ever diverge (corrupted state on
		// one lane), the merge picks whichever was iterated last
		// — a benign tie-break that doesn't poison the file
		// because both values are valid IBKR contract identities.
		maps.Copy(merged, c.SnapshotContracts())
	}
	// Option contracts come from the primary connector only (gamma's
	// bulk prewarm runs against the primary's option cache). Empty when
	// no prewarm has fired this session — that's fine, an empty options
	// map persists as an empty JSON object.
	options := map[string]ibkrlib.ContractDetailsLite{}
	if primary := s.gatewayConnector(); primary != nil {
		options = primary.SnapshotOptionContracts()
	}
	if len(merged) == 0 && len(options) == 0 {
		return
	}
	members, _ := spx.MemberList()
	hash := ibkrlib.MembersHash(members)
	if err := s.contractStore.Save(merged, options, hash); err != nil {
		s.logger.Warnf("contract cache: save: %v", err)
	}
}

// pruneNonMembers returns a new map containing only entries whose
// symbol is in the current SPX members list OR is one of the
// well-known seeds (SPX, VIX, VIX3M, HYG, SPY, USD.JPY — the regime
// dashboard symbols with verified IBKR contracts). Caller uses this to strip delisted / renamed
// tickers from the persisted cache when SPX has reconstituted since
// the last save. The well-known seeds aren't SPX members but are
// still useful for the regime path, so they survive the prune.
func pruneNonMembers(loaded map[string]ibkrlib.ContractDetailsLite, members []string) map[string]ibkrlib.ContractDetailsLite {
	keep := make(map[string]struct{}, len(members)+8)
	for _, m := range members {
		keep[m] = struct{}{}
	}
	for _, sym := range []string{"SPX", "VIX", "VIX3M", "HYG", "SPY", "USD.JPY", "DXY", "NDX"} {
		keep[sym] = struct{}{}
	}
	out := make(map[string]ibkrlib.ContractDetailsLite, len(loaded))
	for sym, detail := range loaded {
		if _, ok := keep[sym]; ok {
			out[sym] = detail
		}
	}
	return out
}

// installSubs wires the per-symbol subscription manager onto s. Called by
// New (production path) and by test helpers that construct *Server directly
// without going through New. The connector closure re-fetches via
// gatewayConnector on each call so a daemon-side reconnect is observed
// without re-registering the manager.
func (s *Server) installSubs() {
	s.subs = newSubManager(func() ibkrMarketConnector {
		c := s.gatewayConnector()
		if c == nil {
			return nil
		}
		return c
	})
}

// Start runs discovery against the configured (possibly partial) gateway,
// opens the IB Gateway connection in the background, listens on the Unix
// socket, and blocks until ctx is cancelled or Stop is called. Returns
// the first fatal error encountered. Returns ErrAlreadyRunning (without
// touching the gateway) if another ibkrd holds the instance lock.
func (s *Server) Start(ctx context.Context) error {
	lock, err := acquireInstanceLock(s.socketPath)
	if err != nil {
		return err
	}
	s.lock = lock
	if err := s.openCoreStore(ctx); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	defer func() {
		if err := s.closeCoreStore(); err != nil {
			s.warnf("close daemon authority: %v", err)
		}
	}()

	// Discovery is fast: no probe at all when port+tls are pinned, and
	// 200ms-per-port TCP probes when not. We do this synchronously before
	// the socket comes up so handleStatusHealth can report the endpoint
	// the daemon will be talking to. Failure here only happens when the
	// user left port unpinned AND no IBKR ports respond — we still bring
	// the socket up so `ibkr status` can render the verdict.
	// Derive a cancellable child of the caller's ctx for background
	// goroutines (reconnect, watchdog). The deferred cancel below ensures
	// any reconnect goroutine launched late by a handler unwinds promptly
	// once Start returns — including the idle-shutdown path where the
	// caller ctx itself isn't cancelled.
	serverCtx, serverCancel := context.WithCancel(ctx)

	s.mu.Lock()
	s.serverCtx = serverCtx
	s.serverCancel = serverCancel
	s.mu.Unlock()
	// Registered after closeCoreStore's defer, so daemon cancellation and any
	// in-flight Regime publication always drain before SQLite is closed.
	defer s.stopServerContextAndWait()
	if err := s.attachRegimeSnapshotAuthority(ctx, serverCtx); err != nil {
		s.lock.Release()
		s.lock = nil
		return fmt.Errorf("attach regime snapshot authority: %w", err)
	}
	if err := s.attachAlertShadowAuthority(ctx); err != nil {
		s.lock.Release()
		s.lock = nil
		return fmt.Errorf("attach alert registry authority: %w", err)
	}

	ep, derr := discover.Resolve(serverCtx, partialFromConfig(s.cfg.Gateway))
	s.mu.Lock()
	s.endpoint = ep
	if derr != nil {
		s.lastConnectError = derr.Error()
	}
	s.mu.Unlock()
	if derr != nil {
		s.logger.Warnf("Endpoint discovery: %v (daemon will start anyway)", derr)
	} else {
		s.logger.Infof("Endpoint resolved: %s:%d (port=%s, tls=%v %s, alternates=%v)",
			ep.Host, ep.Port, ep.PortOrigin, ep.TLS, ep.TLSOrigin, ep.Alternates)
	}

	// With AUTO discovery, a startup with no IBKR running ends up with
	// derr != nil; we skip the connect attempt entirely. Triggering
	// reconnect later (via triggerReconnect) will run a fresh discovery
	// once the user starts IBKR. When discovery succeeded, the failover
	// loop below builds and publishes the connector itself — each
	// candidate endpoint gets its own attempter, and s.connector is set
	// to whichever one lands Connected first.
	defer s.stopConnector()
	defer s.stopBreadthConnector()

	if err := s.openSocket(); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	// closeListener is the load-bearing handoff for both clean and panic
	// shutdown paths: idempotent, mu-guarded, unlinks the socket file via
	// UnixListener.Close. The defer here is the safety net for a panic in
	// acceptLoop's spawn or the idle watcher itself.
	defer s.closeListener()

	s.startedAt = time.Now()
	s.logger.Infof("ibkr daemon %s listening on %s (gateway=%s:%d, clientID=%d)",
		s.version, s.socketPath, ep.Host, ep.Port, ep.ClientID)
	s.evaluateRiskPolicyV3Reconciliation()
	// Skip the connect goroutine when discovery already failed — there's
	// nothing to connect to. The socket is still up so `ibkr status`
	// renders the discovery error, and the next request will trigger a
	// rediscover. Claim the initial attempt before exposing the accept loop:
	// an immediately queued status request otherwise sees no ready connector
	// and no attempt in flight, starts reconnectFlow, and races the initial
	// handshake with a second socket using the same client ID.
	if derr == nil {
		s.mu.Lock()
		s.connectInFlight = true
		s.mu.Unlock()
	}
	// The canonical Rulebook refresh may immediately need the gateway. Start
	// all daemon-owned read loops only after the initial connect slot is claimed
	// so that read-side demand cannot launch reconnectFlow alongside cold start.
	s.startRegimeRefreshLoop(serverCtx)
	s.startRulebookCanonicalRefreshLoop(serverCtx)
	s.startAlertShadowObservationLoops(serverCtx)
	go s.runAccountPnLAuthorityLoop(serverCtx)
	go s.acceptLoop(ctx, s.listener)
	if s.initialAcceptLoopStartedForTest != nil {
		s.initialAcceptLoopStartedForTest()
	}
	if derr == nil {
		go s.runConnectAttempt(serverCtx, ep)
	}
	if s.protectionPolicies != nil {
		go s.protectionPolicies.Run(serverCtx, s.logger.Infof)
	}
	if s.opportunityPolicies != nil {
		go s.opportunityPolicies.Run(serverCtx, s.logger.Infof)
	}
	if s.riskPolicies != nil {
		go s.riskPolicies.Run(serverCtx, s.logger.Infof)
	}
	go s.runFlexFetchLoop(serverCtx)
	s.startCanaryEvaluationLoop(serverCtx)
	if s.tradeProposals != nil {
		s.proposalsStarted.Do(func() {
			go s.tradeProposals.Run(serverCtx)
		})
	}
	if s.opportunities != nil {
		s.opportunitiesStarted.Do(func() {
			go s.opportunities.Run(serverCtx)
		})
	}
	// Breadth scheduler launches from postConnectSetup behind a
	// sync.Once — the cold-start bootstrap fan-out depends on a live
	// gateway connector, and launching here would race against the
	// in-flight connect goroutine. Once Run() is up it survives
	// subsequent gateway disconnects via the fetcher's connector
	// thunk.
	s.runIdleWatcher(ctx)

	s.closeListener()
	return nil
}

// partialFromConfig translates a config.Gateway (pointer-fielded user
// input) into the minimal struct discover.Resolve consumes.
func partialFromConfig(g config.Gateway) discover.PartialGateway {
	return discover.PartialGateway{
		Host:     g.HostOrDefault(),
		Port:     g.Port,
		ClientID: g.ClientID,
		Account:  g.Account,
		TLS:      g.TLS,
	}
}

// closeListener closes and forgets the listener under mu. Idempotent.
// On UnixListener, Close also unlinks the socket file.
func (s *Server) closeListener() {
	s.mu.Lock()
	l := s.listener
	s.listener = nil
	s.mu.Unlock()
	if l != nil {
		_ = l.Close()
	}
}

// Stop closes the listener and IBKR connection. Safe to call multiple times.
// A Server that never reached openSocket (e.g. lock contention exit) must
// not touch the socket file — that would unlink the active peer's socket
// and break the running daemon.
func (s *Server) Stop() {
	// Notify any live streaming subscribers BEFORE we tear the listener
	// down: emits a daemon_shutdown error frame, lets the consumer render
	// a clean message, and unsubscribes the IBKR market-data lines so the
	// gateway doesn't carry zombie subs across daemon restarts.
	if s.subs != nil {
		s.subs.Close()
	}
	s.mu.Lock()
	for _, c := range s.streams {
		c()
	}
	s.streams = map[string]context.CancelFunc{}
	l := s.listener
	s.listener = nil
	s.mu.Unlock()
	if l != nil {
		_ = l.Close()
		_ = os.Remove(s.socketPath)
	}
	// Stop daemon-owned work before closing its persistence authority. Regime
	// publication is explicitly drained because it performs a SQLite CAS and
	// post-publish projections after an observing RPC may already have gone.
	s.stopServerContextAndWait()
	// Capture the last minute of contract resolutions before tearing
	// the connectors down. The save loop runs every 60s; without this
	// final flush, anything resolved in the trailing tick would be
	// lost across the restart. Best-effort: a save error here just
	// logs, since shutdown shouldn't fail because a disk write
	// couldn't complete.
	s.saveContractCache()

	s.stopConnector()
	s.stopBreadthConnector()
	if err := s.closeCoreStore(); err != nil {
		s.warnf("close daemon authority: %v", err)
	}
	if s.lock != nil {
		s.lock.Release()
		s.lock = nil
	}
}

// connectAttempter is the subset of *ibkrlib.Connector that the daemon's
// connect/handshake/failover path needs. *ibkrlib.Connector satisfies it
// structurally; defining the interface here lets the per-candidate
// handshake be unit-tested with a fake that decides per-port whether to
// return Connected, without needing a real TCP server.
type connectAttempter interface {
	Start(ctx context.Context) error
	Stop() error
	IsConnected() bool
	UsingTLS() bool
	SetMarketDataType(int) error
	RequestAccountUpdates(account string) error
	SubscribeAccountPnL(account string) error
}

type lastErrorReporter interface {
	LastError() string
}

// newConnector constructs (but does not start) the IBKR connector from
// the supplied endpoint. Returns immediately — no network I/O.
//
// Endpoint is passed in (not read from s.endpoint) because reconnect
// rebuilds the connector against a freshly-resolved endpoint that may not
// yet be the published one — the caller decides which endpoint applies.
//
// EnableTLSFallback comes from the discovery layer, not the raw config:
// pinned tls (true or false) → strict, no fallback (issue #3). Auto →
// fallback enabled so the SDK retries the alternate mode if the primary
// gets no handshake response.
func (s *Server) newConnector(ep discover.Endpoint) *ibkrlib.Connector {
	conn := ibkrlib.DefaultConfig()
	conn.Host = ep.Host
	conn.Port = ep.Port
	conn.ClientID = ep.ClientID
	conn.Account = ep.Account
	conn.UseTLS = ep.TLS
	conn.EnableTLSFallback = ep.EnableTLSFallback
	// The daemon owns reconnect/failover through triggerReconnect and
	// reconnectFlow. Keep the low-level connection from racing that owner with
	// a second reconnect loop on the same client ID.
	conn.AutoReconnect = false

	cc := &ibkrlib.ConnectorConfig{
		ServiceName:       "ibkrd",
		PreferredClientID: ep.ClientID,
		BaseConfig:        conn,
	}
	return ibkrlib.NewConnector(cc)
}

// buildAttempter is the production attempter factory. Tests replace
// s.attempterFactory to inject fakes without touching the live SDK.
func (s *Server) buildAttempter(ep discover.Endpoint) connectAttempter {
	return s.newConnector(ep)
}

// runConnectAttempt is the single entry point for "do one full connect
// flow (including handshake failover across alternates) and clear
// connectInFlight when done." Used by both Server.Start (initial attempt)
// and reconnectFlow (after a fresh discovery). Splitting this out is what
// makes triggerReconnect's in-flight gate authoritative — both code paths
// set/clear the same flag.
func (s *Server) runConnectAttempt(ctx context.Context, primary discover.Endpoint) {
	defer func() {
		s.mu.Lock()
		s.connectInFlight = false
		s.mu.Unlock()
	}()
	s.connectWithFailover(ctx, primary)
}

// connectWithFailover walks the primary endpoint then each alternate in
// preference order, building a fresh attempter for each and running its
// handshake under the watchdog. The first attempter to land Connected
// wins; it's published as s.connector and post-connect setup runs.
//
// Failover exists because the TCP probe in discover/ is coarse: a port
// can accept connections without its IBKR backend being ready to talk
// (e.g. Gateway is up but not logged in; API checkbox off; mid-startup).
// When both Gateway and TWS are running locally, discovery's preference
// order picks Gateway-live (4001) first, but if its handshake never
// completes, the daemon used to stay degraded forever even though TWS
// on 7496 was sitting right there in Endpoint.Alternates. This loop is
// what finally uses them.
//
// On a healthy primary the loop completes in one iteration. On an
// unhealthy primary, each failed candidate adds roughly pkg/ibkr's
// per-attempt budget (~20s with TLS fallback) before moving on — worst
// case can exceed `ibkr status`'s 25s budget, in which case the next
// status call surfaces the verdict.
func (s *Server) connectWithFailover(ctx context.Context, primary discover.Endpoint) {
	factory := s.attempterFactory
	if factory == nil {
		// Direct &Server{} construction (legacy tests) leaves the field
		// unset. Default to the production wrapper so the loop is
		// always callable.
		factory = s.buildAttempter
	}

	candidates := []discover.Endpoint{primary}
	for _, port := range primary.Alternates {
		alt := primary
		alt.Port = port
		alt.Alternates = nil
		candidates = append(candidates, alt)
	}

	for i, cand := range candidates {
		if ctx.Err() != nil {
			return
		}
		if i > 0 {
			s.logger.Infof("Failover: %s:%d did not handshake; trying alternate %s:%d (%d/%d)",
				candidates[i-1].Host, candidates[i-1].Port, cand.Host, cand.Port, i+1, len(candidates))
		}

		a := factory(cand)
		// Publish the candidate so handlers / status see the port the
		// daemon is currently talking to. The type assertion is safe in
		// production (buildAttempter returns *ibkrlib.Connector); test
		// fakes skip the s.connector publish but the loop still drives
		// the per-candidate handshake.
		if real, ok := a.(*ibkrlib.Connector); ok {
			for {
				s.mu.Lock()
				expected := s.connector
				s.mu.Unlock()
				if s.withConnectorEvidencePublication(expected, real, func() {
					s.endpoint = cand
					s.lastConnectError = ""
					s.connector = real
					s.connectorEpoch++
				}) {
					break
				}
			}
			s.registerOrderLifecycleJournal(real)
		} else {
			s.mu.Lock()
			s.endpoint = cand
			s.lastConnectError = ""
			s.mu.Unlock()
		}

		if s.tryOneHandshake(ctx, a, cand) {
			s.postConnectSetup(a, cand)
			return
		}

		// This candidate failed. Unpublish it under the exclusive evidence
		// boundary before Stop, so callbacks already in flight drain against the
		// still-current identity and any callback in the unpublish-to-Stop window
		// reaches the retained receipt handler and fails closed as stale.
		if real, ok := a.(*ibkrlib.Connector); ok {
			s.withConnectorEvidencePublication(real, nil, func() {
				s.connector = nil
				s.connectorEpoch++
			})
		}
		if err := a.Stop(); err != nil {
			s.logger.Warnf("Failover: stop failed candidate %s:%d: %v", cand.Host, cand.Port, err)
		}
		if real, ok := a.(*ibkrlib.Connector); ok {
			s.forgetOrderLifecycleJournal(real)
		}
	}

	if ctx.Err() != nil {
		return
	}
	// All candidates exhausted. Publish a verdict that names what we
	// tried so `ibkr status` shows the user the full picture (not just
	// the original probe winner).
	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, fmt.Sprintf("%s:%d", c.Host, c.Port))
	}
	hint := fmt.Sprintf(
		"none of %d discovered endpoint(s) completed TWS handshake (tried %s); confirm the IBKR app you intend to use has 'Enable ActiveX and Socket Clients' on and is logged in",
		len(candidates), strings.Join(names, ", "),
	)
	s.mu.Lock()
	s.lastConnectError = hint
	// Dedupe like the per-candidate verdict above: exhaustion recurs every
	// reconnect cycle while the gateway is down, so log once per changed
	// verdict and demote repeats to Debug.
	verdictChanged := s.lastNoEndpointUsable != hint
	s.lastNoEndpointUsable = hint
	s.mu.Unlock()
	const format = "Daemon up but no endpoint usable: %s"
	if verdictChanged {
		s.logger.Warnf(format, hint)
	} else {
		s.logger.Debugf(format, hint)
	}
}

// tryOneHandshake runs a single candidate's connect under the watchdog
// and returns true iff the attempter ended Connected. On failure it
// publishes a per-candidate hint to lastConnectError so a status poll
// mid-failover shows the truth.
//
// The candidate runs under a bounded ctx (perCandidateConnectBudget) so
// the failover loop can advance even when pkg/ibkr's TLS-handshake
// retry would otherwise hang against a black-hole peer.
func (s *Server) tryOneHandshake(ctx context.Context, a connectAttempter, ep discover.Endpoint) bool {
	candidateCtx, candidateCancel := context.WithTimeout(ctx, perCandidateConnectBudget)
	defer candidateCancel()

	watchdogCtx, watchdogCancel := context.WithCancel(candidateCtx)
	defer watchdogCancel()
	go s.handshakeWatchdog(watchdogCtx, a.IsConnected, handshakeWatchdogDelay, ep)

	err := a.Start(candidateCtx)
	// Outer (daemon) ctx cancelled → shutdown raced with us; exit silently
	// so the surrounding loop's ctx check stops the iteration too.
	if ctx.Err() != nil {
		return false
	}
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		// Per-candidate budget expired with the SDK still in handshake
		// retry. This is the black-hole-TLS path: TCP accepted, TLS
		// ClientHello sent, no ServerHello ever arrived. Publish a
		// candidate-specific timeout hint so the user sees which port
		// burned the budget.
		hint := fmt.Sprintf("gateway %s:%d did not handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port, perCandidateConnectBudget)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Candidate budget expired: %s", hint)
		return false
	case err != nil && candidateCtx.Err() != nil:
		// SDK returned a wrapped ctx error (e.g. "tls handshake failed:
		// context deadline exceeded"). Treat as candidate-budget expiry
		// for the same reason as above.
		hint := fmt.Sprintf("gateway %s:%d did not handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port, perCandidateConnectBudget)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Candidate budget expired: %s", hint)
		return false
	case err != nil:
		s.mu.Lock()
		s.lastConnectError = err.Error()
		s.mu.Unlock()
		s.logger.Errorf("connect to IB Gateway %s:%d: %v", ep.Host, ep.Port, err)
		return false
	}

	// pkg/ibkr's pool returns success even when the underlying TCP
	// handshake hasn't completed (e.g. gateway unreachable, API socket
	// disabled). Probe IsConnected so we log the truth.
	if !a.IsConnected() {
		hint := connectorLastError(a)
		if hint == "" {
			hint = fmt.Sprintf("gateway %s:%d did not complete TWS handshake; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
				ep.Host, ep.Port)
		}
		s.mu.Lock()
		s.lastConnectError = hint
		// Dedupe: the daemon rebuilds and re-fails against a down gateway every
		// reconnect cycle. Log the transition once at WARN and demote identical
		// repeats to Debug. Compare-and-set under the same lock as
		// lastConnectError so two racing status-driven reconnects can't both
		// decide "changed" (see resetConnectVerdicts for the recovery reset).
		verdictChanged := s.lastGatewayUnreachable != hint
		s.lastGatewayUnreachable = hint
		s.mu.Unlock()
		const format = "Daemon up but gateway not connected: %s"
		if verdictChanged {
			s.logger.Warnf(format, hint)
		} else {
			s.logger.Debugf(format, hint)
		}
		return false
	}
	return true
}

func connectorLastError(a connectAttempter) string {
	reporter, ok := a.(lastErrorReporter)
	if !ok {
		return ""
	}
	return strings.TrimSpace(reporter.LastError())
}

func (s *Server) gatewayUnavailableError() error {
	s.mu.Lock()
	lastErr := strings.TrimSpace(s.lastConnectError)
	s.mu.Unlock()
	if lastErr == "" {
		return ibkrlib.ErrIBKRUnavailable
	}
	return fmt.Errorf("%w: %s", ibkrlib.ErrIBKRUnavailable, lastErr)
}

// resetConnectVerdicts clears the connect-retry verdict dedupe on a successful
// handshake so the next unreachable episode logs its transition afresh. It
// returns true iff the daemon was in a logged-unreachable episode, so the
// caller can emit the one-line recovery bookend. lastEndpointResolvedSig is
// intentionally left intact — it is value-keyed on the endpoint, not the
// episode, and re-logs only when the endpoint actually changes. Caller must not
// hold s.mu.
func (s *Server) resetConnectVerdicts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := s.lastGatewayUnreachable != "" || s.lastNoEndpointUsable != ""
	s.lastGatewayUnreachable = ""
	s.lastNoEndpointUsable = ""
	return was
}

// postConnectSetup runs the best-effort initialization that follows a
// successful handshake (market-data type + account-updates stream).
// Failures here are non-fatal: snapshot data still flows; only the
// streaming mark/value/P&L decoration on positions is degraded.
func (s *Server) postConnectSetup(a connectAttempter, ep discover.Endpoint) {
	s.mu.Lock()
	s.lastConnectError = ""
	// A completed handshake ends the outage: clear the reconnect backoff so a
	// later drop reconnects immediately instead of inheriting an escalated
	// quiet period (same reasoning as the gamma resetRetryBackoff below).
	s.reconnectFailStreak = 0
	s.mu.Unlock()
	// A successful handshake ends any unreachable episode: clear the verdict
	// dedupe so a later outage logs its transition afresh, and log a one-line
	// recovery bookend iff we were mid-episode (so a plain first connect stays
	// quiet).
	if s.resetConnectVerdicts() {
		s.logger.Infof("Gateway reachable again at %s:%d after retrying while it was down",
			ep.Host, ep.Port)
	}
	s.logger.Infof("Connected to IB Gateway %s:%d (clientID=%d, tls=%v)",
		ep.Host, ep.Port, ep.ClientID, a.UsingTLS())

	// Default to type 2 (frozen-aware): IBKR returns live ticks for
	// entitled symbols during market hours and the last-known close
	// otherwise. Snapshot requests reliably terminate with
	// tickSnapshotEnd this way; pure live (type 1) can leave snapshots
	// hanging when the market is closed.
	if err := a.SetMarketDataType(2); err != nil {
		s.logger.Warnf("SetMarketDataType(frozen) failed: %v", err)
	}
	// A fresh handshake is the one event after which a previously
	// silent shortable feed (tick 236) can plausibly start delivering —
	// drop the market-events absence memory so the next snapshot
	// re-probes every held symbol. Same reasoning for the gamma retry
	// backoff: a farm outage ends with a reconnect, so the first compute
	// after one shouldn't sit out an escalated quiet period.
	if s.marketEvents != nil {
		s.marketEvents.clearShortableAbsence()
	}
	if s.zeroGamma != nil {
		s.zeroGamma.resetRetryBackoff()
	}
	if s.regimeSnapshots != nil {
		s.regimeSnapshots.allowRefreshNow()
	}
	// Start the streaming account+portfolio subscription so position
	// rows carry live mark/value/P&L. The discovered session account can
	// be the aggregate "All" (or a multi-account list); those are not
	// account codes — TWS rejects them with error 321 and the portfolio
	// stream never starts, leaving positions empty for the entire daemon
	// lifetime (observed 2026-06-11). Prefer a concrete session account,
	// then the concrete scope account (config pin), then let the
	// connector resolve its bound code.
	account := strings.TrimSpace(ep.Account)
	if !brokerScopeAccountConcrete(account) {
		account = s.currentBrokerStateScope().Account
	}
	if !brokerScopeAccountConcrete(account) {
		account = ""
	}
	if err := a.RequestAccountUpdates(account); err != nil {
		s.logger.Warnf("RequestAccountUpdates failed (positions will lack marks): %v", err)
	}
	// Subscribe to the account-level Daily P&L stream (TWS msg 94). Failure
	// is non-fatal: account.summary keeps working without daily fields,
	// and per-position daily lookups still degrade gracefully because the
	// connector cache returns nil pointers on miss. Empty account means
	// neither the session nor the discover/config path yielded a concrete
	// one — skip the call entirely since reqPnL requires an account.
	if account != "" {
		if err := a.SubscribeAccountPnL(account); err != nil {
			s.logger.Warnf("SubscribeAccountPnL failed (account.summary will lack daily P&L): %v", err)
		}
	}

	// Load the persisted contract cache and seed the primary connector
	// BEFORE prewarm / breadth / gamma fire. Every cache hit here is
	// an IBKR rate-limit-bucket token saved: without persistence, each
	// daemon restart re-resolves all 503 SPX members + the 6 regime
	// seeds, draining IBKR's per-account reqContractDetails bucket
	// over and over. The members-hash check prunes stale entries when
	// SPX has reconstituted since the last save — added members fall
	// through to fresh resolution via the normal path.
	s.seedFromContractStore(s.connector)
	s.registerOrderLifecycleJournal(s.connector)

	// Order-journal broker reconcile: a settle-delayed one-shot on EVERY
	// successful (re)connect — each reconnect is exactly when a terminal
	// callback may have been missed — plus a Once-gated standing loop.
	// Both live off serverCtx so daemon shutdown stops them.
	if s.serverCtx != nil {
		go func(ctx context.Context) {
			select {
			case <-ctx.Done():
			case <-time.After(orderReconcileConnectDelay):
				s.reconcileOrderJournalWithBroker(ctx)
			}
		}(s.serverCtx)
		s.orderReconcileLoopStarted.Do(func() {
			go s.runOrderReconcileLoop(s.serverCtx)
		})
	}

	// Spawn the periodic save loop. Guarded by contractCacheSaveStarted
	// so reconnect-driven postConnectSetup re-runs don't multiply the
	// goroutine. The loop runs for the daemon's lifetime, picking up
	// new resolutions from both connectors every minute.
	if s.contractStore != nil && s.serverCtx != nil {
		s.contractCacheSaveStarted.Do(func() {
			go s.contractCacheSaveLoop(s.serverCtx)
		})
	}

	// Pre-warm contract-details cache for the regime-dashboard symbols
	// in the background. Without this the first regime call after startup
	// races five parallel goroutines against fresh contract resolution
	// — confirmed observed at v0.22.0: first cold call returned with
	// hyg_50dma and weekly_change_pct missing; second call (caches
	// warm) returned all fields. Pre-warming eliminates the variance
	// without changing the regime request path.
	//
	// Sentinel set HERE (before `go`), not inside the goroutine: a
	// status RPC arriving in the brief window between this function
	// returning and the goroutine being scheduled would otherwise see
	// Connected=true but no background tasks. The goroutine's deferred
	// Store(false) is the load-bearing clear.
	s.regimePrewarming.Store(true)
	go s.prewarmRegimeSymbols()

	// Stand up the dedicated bulk-historical IBKR client BEFORE
	// launching breadth.Run(). The scheduler's cold-start bootstrap
	// fan-outs 500 historical-bar fetches and would otherwise read
	// nil from breadthGatewayConnector on every leg until the bulk
	// handshake lands. Synchronous start (12-s ceiling) blocks
	// postConnectSetup briefly; in exchange, breadth.Run() launches
	// with a guaranteed-or-nil view of the bulk connector.
	if s.breadth != nil && s.serverCtx != nil {
		s.breadthClientStarted.Do(func() {
			s.startBreadthConnector(s.serverCtx, ep)
		})
	}

	// Launch the breadth scheduler now that the gateway has handshaken.
	// The cold-start bootstrap inside Run() fan-outs 500 historical-bar
	// fetches via the bulk connector started above and would fail-and-
	// poison the cache if either connector weren't ready yet (v0.27.0
	// regression — see engine.finalise's Coverage==0 guard). The Once
	// guard means a reconnect-driven second postConnectSetup is a no-op
	// rather than spawning a duplicate scheduler loop.
	//
	// MarkPendingBootstrap is called BEFORE `go` so a status RPC arriving
	// in the goroutine-spawn window sees breadth-spx in BackgroundTasks
	// when a bootstrap Refresh is about to fire. No-op when the cached
	// snapshot is fresh enough that Run() would skip straight to the
	// daily-tick wait — so the flag never sticks.
	if s.breadth != nil && s.serverCtx != nil {
		s.breadthStarted.Do(func() {
			s.breadth.MarkPendingBootstrap()
			go s.breadth.Run(s.serverCtx)
		})
	}

	// Launch the runtime members refresher alongside the breadth
	// scheduler. The refresher's Run() no-ops immediately when the
	// pin gates fire — keeps the start path uniform regardless of
	// config. Once-guarded so reconnect-driven second postConnectSetup
	// is a no-op rather than spawning a duplicate ticker loop.
	if s.membersRefresher != nil && s.serverCtx != nil {
		s.membersRefresherStarted.Do(func() {
			go s.membersRefresher.Run(s.serverCtx)
		})
	}

	// Prewarm dealer zero-gamma so the first `ibkr regime` / `ibkr
	// gamma` of the day doesn't block on a cold ~4-min compute. The
	// user sees "computing dealer zero-gamma" in `ibkr status`
	// immediately. No periodic loop here — kickOrJoin's soft-TTL
	// machinery handles staleness from RPC handlers thereafter, so we
	// only need the initial kick. The Once gates against
	// reconnect-driven duplicate kicks; later staleness is the soft
	// TTL's job.
	//
	// Called synchronously (no `go`): kickOrJoin only acquires the
	// cache mutex and assigns c.current under it; the multi-minute
	// fan-out runs on the goroutine spawnJob spawns internally. By the
	// time postConnectSetup returns, IsComputing() reflects the
	// in-flight compute — closing the race where the first `ibkr
	// status` after restart would see Connected=true but no background
	// tasks.
	if s.zeroGamma != nil && s.serverCtx != nil {
		s.gammaStarted.Do(func() {
			s.prewarmZeroGamma(s.serverCtx)
		})
		s.gammaRefreshStarted.Do(func() {
			go s.runGammaRefreshLoop(s.serverCtx)
		})
	}
	// Kick the proposal engine for an immediate refresh now that the
	// session is handshaken and RequestAccountUpdates (above) has started
	// the portfolio stream. Without this a reconnect after an outage
	// leaves the protection panel serving the stale snapshot until the
	// backed-off timer fires (observed 2026-06-12: gateway back at 10:53,
	// panel recovered 10:59:15). Ordered last so the kicked refresh races
	// as little of the post-handshake setup as possible; the engine's
	// positions_pending guard covers the residual window where the
	// portfolio burst hasn't landed yet.
	s.tradeProposals.Kick()
	if s.opportunities != nil {
		s.opportunities.Kick()
	}

	// Latch the postConnectSetup-done barrier. handleStatusHealth gates
	// its Connected field on this so a status RPC arriving in the brief
	// window between c.ready flipping true (set by the connection read
	// loop in pkg/ibkr) and the synchronous sentinel-setting above
	// finishing reports Connected=false — the CLI keeps polling and the
	// first user-visible response shows the full background-task list.
	s.postConnectSetupDone.Store(true)
	// The evaluator starts before the gateway handshake so daemon startup never
	// depends on an app process. Wake it now that account and portfolio streams
	// have been requested; a later Regime publication supplies another
	// coalesced wake after market authority advances.
	s.wakeCanaryEvaluation()
}

// prewarmZeroGamma kicks the first dealer zero-gamma compute of a
// daemon's lifetime so the first `ibkr regime` / `ibkr gamma` call
// doesn't block on the cold compute. Mirrors handleGammaZeroSPX's
// compute closure construction (connector + normalized default params
// + production leg fetcher) so the prewarm result and a subsequent
// user-triggered compute are interchangeable from the cache's
// perspective.
//
// No sequencing against breadth: with breadth running on a dedicated
// bulk-historical client (separate clientID, separate rate-limit
// budget), the two fan-outs no longer share the primary's 40-msg/sec
// or 60-historical/10-min budgets. Gamma can prewarm concurrently
// with breadth's cold-start refresh and still finish in its normal
// ~20 s wall-clock.
//
// Returns silently on connector unavailability — postConnectSetup
// runs after handshake so the connector is normally ready, but a
// race against immediate disconnect shouldn't crash startup. The
// next user-triggered kickOrJoin will kick the compute instead.
func (s *Server) prewarmZeroGamma(ctx context.Context) {
	s.kickZeroGamma(ctx, "startup")
}

const gammaRefreshPollInterval = time.Minute

func (s *Server) runGammaRefreshLoop(ctx context.Context) {
	ticker := time.NewTicker(gammaRefreshPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if gammaClassifySession(time.Now()) == rpc.SessionClosed {
			continue
		}
		s.kickZeroGamma(ctx, "scheduler")
	}
}

func (s *Server) kickZeroGamma(ctx context.Context, caller string) {
	c := s.gatewayConnector()
	if c == nil {
		s.logger.Warnf("gamma %s: gateway connector unavailable, skipping compute", caller)
		return
	}
	params := normalizeGammaParams(rpc.GammaZeroParams{})
	// Startup prewarm builds the canonical combined cache so the first
	// user-driven `ibkr gamma` (default scope) hits ready instead of
	// kicking another 7-minute fan-out. computeGammaCombined holds
	// SPY then SPX serially via runUnderlyingPhase, and degrades to
	// SPY-only with a structured warning when SPX is unreachable —
	// the same path the handler uses for scope="spy+spx". Refcounted
	// Holds via subManager so a concurrent regime snapshot on either
	// symbol is safe.
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return computeGammaCombined(bgCtx, s, c, params, prog)
	}
	if _, fresh := s.zeroGamma.kickOrJoin(ctx, rpc.GammaZeroScopeCombined, time.Now(), computeETA, compute); fresh {
		s.logger.Infof("gamma %s: kicked compute (scope=combined)", caller)
	}
}

// regimeSymbolSeed is the static fallback used by prewarmRegimeSymbols
// when the gateway's reqContractDetails is unresponsive (per-account
// rate limit, post-restart cold state, or any other transient
// silence). These conIDs identify the spot instruments by their IBKR
// contract identity, which is stable across years for spot ETFs/FX/CBOE
// indices — observed unchanged in the daemon log over weeks. A live
// gateway response always wins over a seeded value (the
// SeedContractDetails guard refuses to overwrite a non-zero entry), so
// the seed is purely a fallback for the broken-gateway case. If IBKR
// ever renumbers one of these (extremely rare for spot instruments),
// the only symptom is the regime row's historical-derived field going
// stale until someone updates the constant.
var regimeSymbolSeed = map[string]ibkrlib.ContractDetailsLite{
	"VIX":     {Symbol: "VIX", ConID: 13455763, Exchange: "CBOE", PrimaryExch: "CBOE"},
	"VIX3M":   {Symbol: "VIX3M", ConID: 47511905, Exchange: "CBOE", PrimaryExch: "CBOE"},
	"HYG":     {Symbol: "HYG", ConID: 43652089, Exchange: "SMART", PrimaryExch: "ARCA"},
	"SPY":     {Symbol: "SPY", ConID: 756733, Exchange: "SMART", PrimaryExch: "ARCA"},
	"USD.JPY": {Symbol: "USD", ConID: 15016059, Exchange: "IDEALPRO", PrimaryExch: "IDEALPRO"},
	// SPX anchors the zero-gamma compute (Indicator 4). Without a
	// seeded underlying conID, the compute's first step — fetching
	// the option chain — can stall on the same reqContractDetails
	// silence that blocks the historical fetchers above.
	"SPX": {Symbol: "SPX", ConID: 416904, Exchange: "CBOE", PrimaryExch: "CBOE"},
}

// prewarmRegimeSymbols populates the connector's contract-details
// cache for the verified regime-dashboard symbols so the first
// user-initiated regime call's historical fetches don't depend on a
// fresh reqContractDetails round-trip.
//
// Two layers, applied in order:
//
//  1. Seed each symbol from regimeSymbolSeed (above) — fast and works
//     even when the gateway's reqContractDetails is silent (rate
//     limit, cold state). Without this the seed, a slow / wedged
//     reqContractDetails leaves hyg_50dma and weekly_change_pct
//     missing for the entire daemon lifetime.
//
//  2. Fire FetchContractDetails in parallel anyway. On a healthy
//     gateway the live response overwrites the seed in
//     SeedContractDetails' caller (the cleanup goroutine's "ConID==0
//     can be overwritten" guard ensures live wins over seed). On a
//     degraded gateway the live request times out at 30 s but the
//     seed remains in place, and the regime row still ranks.
//
// USD.JPY normalises to the IDEALPRO FX-pair contract via the same
// classifySymbol path the regime USD/JPY fetcher uses; the seed entry
// is keyed on the dotted-pair form so the look-up matches.
func (s *Server) prewarmRegimeSymbols() {
	// Clear the sentinel on every exit path. The matching Set is in the
	// caller (postConnectSetup) BEFORE the `go` so a status RPC arriving
	// during the goroutine-spawn window sees the in-flight prewarm.
	// The connector nil-return below depends on this defer running.
	defer s.regimePrewarming.Store(false)
	c := s.gatewayConnector()
	if c == nil {
		return
	}
	syms := []string{"VIX", "VIX3M", "HYG", "SPY", "USD.JPY", "SPX"}
	for _, sym := range syms {
		if seed, ok := regimeSymbolSeed[sym]; ok {
			c.SeedContractDetails(sym, seed)
		}
	}
	var wg sync.WaitGroup
	for _, sym := range syms {
		wg.Go(func() {
			// 30 s budget per symbol. Empirically a healthy
			// weekday gateway resolves contract details in well
			// under a second; weekend/frozen state can stretch
			// past 20 s for some symbols (observed: HYG at 21 s
			// post-connect). The pre-warm goroutine is
			// fire-and-forget and doesn't gate any user-facing
			// path, so a generous budget here is cheap and the
			// payoff (first regime call lands without 5-way
			// contract-resolve contention) is real.
			if _, err := c.FetchContractDetails(sym, 30*time.Second); err != nil {
				s.logger.Warnf("regime pre-warm: %s contract details: %v (using seeded conID)", sym, err)
			}
		})
	}
	wg.Wait()
	s.logger.Infof("regime pre-warm: contract cache primed for %v", syms)
}

// handshakeWatchdog publishes a degraded-state hint to lastConnectError if
// the gateway hasn't connected by `delay`. Takes isConnected as a function
// pointer so tests can drive this directly without a real *Connector.
//
// The gate (s.lastConnectError == "") avoids clobbering a real error that
// the main path may have already set. The success branch in
// connectGatewayBackground clears the hint when the connect eventually
// lands (e.g. via the SDK's TLS fallback retry) — so the watchdog is
// informational only, not authoritative.
func (s *Server) handshakeWatchdog(ctx context.Context, isConnected func() bool, delay time.Duration, ep discover.Endpoint) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	if isConnected() {
		return
	}
	hint := fmt.Sprintf(
		"gateway %s:%d not responding to TWS handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
		ep.Host, ep.Port, delay,
	)
	s.mu.Lock()
	if s.lastConnectError == "" {
		s.lastConnectError = hint
	}
	s.mu.Unlock()
	s.logger.Warnf("Handshake watchdog: %s", hint)
}

// reconnectBackoffBase / reconnectBackoffMax bound the quiet period between
// reconnect attempts. The cap is deliberately below the CLI's
// handshakeWaitBudget (25s) so a user moving IBKR from Gateway to TWS still
// recovers within a single `ibkr status` invocation.
const (
	reconnectBackoffBase = time.Second
	reconnectBackoffMax  = 15 * time.Second
)

// reconnectBackoff converts a consecutive-failed-reconnect count into the
// minimum spacing before the next attempt may start: 1s, 2s, 4s, 8s, then
// capped at reconnectBackoffMax. Reconnection is demand-driven — every read
// handler that finds the connector down calls triggerReconnect, and a dead
// gateway refuses the dial instantly, so without this gate a multi-hour
// outage retries as fast as callers poll (~2.6/s → 66,900 attempts over a 7h
// outage, observed 2026-07-08, each emitting an identical connect-failure
// line). Mirrors gammaRetryBackoff. The d <= 0 branch guards shift overflow
// on absurd streaks.
func reconnectBackoff(streak int) time.Duration {
	if streak <= 1 {
		return reconnectBackoffBase
	}
	d := reconnectBackoffBase << (streak - 1)
	if d <= 0 || d > reconnectBackoffMax {
		return reconnectBackoffMax
	}
	return d
}

// reconnectAllowed reports whether enough quiet time has elapsed since the
// last reconnect attempt to start another, given the current consecutive-
// failure streak. streak 0 is always allowed so a fresh gateway drop
// reconnects immediately; the spacing is measured from attempt start, so a
// slow handshake already counts as its own quiet period. Caller must hold
// s.mu. Mirrors gammaSlot.retryAllowed.
func (s *Server) reconnectAllowed(now time.Time) bool {
	if s.reconnectFailStreak == 0 {
		return true
	}
	return now.Sub(s.lastReconnectAttemptAt) >= reconnectBackoff(s.reconnectFailStreak)
}

// triggerReconnect launches a rediscover+reconnect attempt in a
// background goroutine if (a) no connect attempt is already in flight and
// (b) the daemon is not already healthy. Returns true iff a fresh attempt
// was started.
//
// The motivation is the AUTO discovery flow: discovery only ran once at
// daemon startup, and after a failed handshake the endpoint stayed pinned
// to the original probe winner forever. If the user closes IB Gateway
// (4001) and starts TWS (7496) instead, the daemon would render
// "degraded — gateway not connected" against 4001 indefinitely. Calling
// this from `gatewayConnector()` and from `handleStatusHealth` means each
// new request kicks off a rediscover, picks up the new listener, and
// recovers without a daemon restart.
//
// We clear lastConnectError when claiming the in-flight slot so the
// existing CLI status loop (which polls until either Connected or
// LastError != "") treats the recovery attempt as "in flight" and waits
// for a verdict — exactly the same UX as the initial connect path.
func (s *Server) triggerReconnect() bool {
	s.mu.Lock()
	if s.connectInFlight {
		s.mu.Unlock()
		return false
	}
	// IsReady, not IsConnected: TCP-up is not enough. If the connector
	// landed in the {ready=false, conn=true} state (handlers cleared
	// during a transient gateway hiccup), we must re-establish — and the
	// only way out is to tear the old TCP socket down in reconnectFlow.
	if s.connector != nil && s.connector.IsReady() {
		s.mu.Unlock()
		return false
	}
	if s.serverCtx == nil || s.serverCtx.Err() != nil {
		// Server.Start hasn't run yet, the daemon has already begun
		// shutting down, or this Server was constructed outside the
		// normal lifecycle (e.g. in a test that doesn't go through
		// Start). Nothing to do.
		s.mu.Unlock()
		return false
	}
	// Backoff gate: while the gateway is down every read handler that funnels
	// through gatewayConnector() calls this, and a refused dial returns
	// instantly, so absent a throttle the retries flood the log. Skip (never
	// sleep — this runs on the request hot path) while inside the current
	// failure streak's quiet period; the next poll after it elapses starts the
	// real attempt.
	now := s.now()
	if !s.reconnectAllowed(now) {
		s.mu.Unlock()
		return false
	}
	s.connectInFlight = true
	s.lastReconnectAttemptAt = now
	s.lastConnectError = ""
	ctx := s.serverCtx
	s.mu.Unlock()

	go func() {
		defer func() {
			s.mu.Lock()
			s.connectInFlight = false
			s.mu.Unlock()
		}()
		s.reconnectFlow(ctx)
	}()
	return true
}

// reconnectFlow tears down any existing connector, re-runs port
// discovery, builds a fresh connector against whichever endpoint the
// probe winner produces, and runs the handshake. Caller must have
// already claimed connectInFlight; this function does not manage the
// flag itself (the goroutine wrapper in triggerReconnect does).
//
// Tearing down the old connector is required: pkg/ibkr's pool can't be
// "restarted" against a different host/port, and we may genuinely need
// to switch (e.g. GW on 4001 → TWS on 7496). Stop is idempotent and
// safe even when the old connector never finished its first handshake.
func (s *Server) reconnectFlow(ctx context.Context) {
	s.mu.Lock()
	old := s.connector
	s.mu.Unlock()
	s.withConnectorEvidencePublication(old, nil, func() {
		s.connector = nil
		s.connectorEpoch++
	})
	if old != nil {
		if err := old.Stop(); err != nil {
			s.logger.Warnf("Reconnect: stop old connector: %v", err)
		}
		s.forgetOrderLifecycleJournal(old)
	}

	ep, derr := discover.Resolve(ctx, partialFromConfig(s.cfg.Gateway))
	endpointSig := fmt.Sprintf("%s:%d tls=%v", ep.Host, ep.Port, ep.TLS)
	s.mu.Lock()
	s.endpoint = ep
	if derr != nil {
		s.lastConnectError = derr.Error()
	}
	prevWarn := s.lastDiscoveryWarn
	switch {
	case derr != nil:
		s.lastDiscoveryWarn = derr.Error()
	default:
		s.lastDiscoveryWarn = ""
	}
	curWarn := s.lastDiscoveryWarn
	// Dedupe the "endpoint resolved" INFO: with a pinned port this resolves to
	// the same endpoint every reconnect cycle, so log once per changed endpoint
	// and demote identical repeats to Debug. Captured under the lock as a local
	// so the log decision can't race a concurrent reconnect.
	endpointChanged := false
	if derr == nil {
		endpointChanged = s.lastEndpointResolvedSig != endpointSig
		s.lastEndpointResolvedSig = endpointSig
	}
	s.mu.Unlock()
	if derr != nil {
		// Same verdict as the previous attempt → already logged; stay quiet.
		// A changed verdict (or first failure) logs once. This keeps the
		// reconnect-during-status-poll loop from emitting the same WARN line
		// every 500ms while the user is waiting for the handshake.
		if curWarn != prevWarn {
			s.logger.Warnf("Reconnect: discovery: %v", derr)
		}
		s.noteReconnectOutcome(ctx, false)
		return
	}
	const endpointFormat = "Reconnect: endpoint resolved: %s:%d (port=%s, tls=%v %s, alternates=%v)"
	if endpointChanged {
		s.logger.Infof(endpointFormat, ep.Host, ep.Port, ep.PortOrigin, ep.TLS, ep.TLSOrigin, ep.Alternates)
	} else {
		s.logger.Debugf(endpointFormat, ep.Host, ep.Port, ep.PortOrigin, ep.TLS, ep.TLSOrigin, ep.Alternates)
	}

	s.connectWithFailover(ctx, ep)
	// connectWithFailover publishes a ready connector on success (and
	// postConnectSetup zeroes the streak); if we are still not ready this
	// cycle failed — bump the streak so triggerReconnect's backoff paces the
	// next attempt.
	s.mu.Lock()
	connected := s.connector != nil && s.connector.IsReady()
	s.mu.Unlock()
	s.noteReconnectOutcome(ctx, connected)
}

// noteReconnectOutcome records the result of a reconnect cycle for the backoff
// gate: a failed cycle bumps reconnectFailStreak so triggerReconnect paces the
// next attempt. A successful handshake is handled by postConnectSetup, which
// zeroes the streak; a cycle cut short by daemon shutdown is ignored. Caller
// must not hold s.mu.
func (s *Server) noteReconnectOutcome(ctx context.Context, connected bool) {
	if connected || ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	s.reconnectFailStreak++
	s.mu.Unlock()
}

// stopConnector tears down the IBKR connector and forgets it. Idempotent.
// Holds s.mu across the entire lifecycle transition so handlers reading
// s.connector via gatewayConnector() never observe a value mid-Stop.
func (s *Server) stopConnector() {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	s.withConnectorEvidencePublication(c, nil, func() {
		s.connector = nil
		s.connectorEpoch++
	})
	if c == nil {
		return
	}
	if err := c.Stop(); err != nil {
		s.logger.Warnf("Connector.Stop: %v", err)
	}
	s.forgetOrderLifecycleJournal(c)
}

// gatewayConnector returns the live IBKR connector if one is constructed
// AND fully connected, or nil otherwise. All read handlers go through
// this — taking the snapshot under mu prevents the race where a handler
// reads s.connector as non-nil, stopConnector nils it, then the handler
// dereferences it for a method call.
//
// When the daemon is degraded (no connector, or connector not connected),
// this kicks off a background rediscover+reconnect via triggerReconnect.
// The current handler still returns ErrIBKRUnavailable to its caller —
// the next call to the daemon will see the result of the reconnect. This
// is what closes the AUTO-discovery hole where the daemon would stay
// pinned to a stale port after the user moved between Gateway and TWS.
func (s *Server) gatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	// IsReady, not IsConnected: handlers also need to be armed. A connector
	// in {ready=false, conn=true} can't serve data — return nil so the
	// caller surfaces ErrIBKRUnavailable, while triggerReconnect rebuilds.
	if c == nil || !c.IsReady() {
		s.triggerReconnect()
		return nil
	}
	return c
}

func (s *Server) openSocket() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// We hold the instance flock; any peer holding the socket is by
	// definition stale (its lock would be released). Dial-probe first to
	// distinguish "stale file from a crashed predecessor" (safe to remove)
	// from "live peer that beat us to flock acquisition" (impossible, but
	// surface clearly if it ever happens).
	if fi, err := os.Stat(s.socketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
		if c, err := net.DialTimeout("unix", s.socketPath, 200*time.Millisecond); err == nil {
			_ = c.Close()
			return fmt.Errorf("socket %s already serving despite holding lock; refusing to evict", s.socketPath)
		}
		if err := os.Remove(s.socketPath); err != nil {
			return fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen unix: %w", err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.listener = l
	return nil
}

// acceptLoop runs against a stable listener reference captured at Start
// time. Mutating s.listener (closeListener / Stop) only affects future
// Start calls; the live loop sees Accept return net.ErrClosed once that
// listener is closed and exits.
func (s *Server) acceptLoop(ctx context.Context, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if ctx.Err() != nil {
				return
			}
			s.logger.Warnf("accept: %v", err)
			continue
		}
		s.bumpActive(+1)
		go func() {
			defer s.bumpActive(-1)
			s.serveConn(ctx, conn)
		}()
	}
}

func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	r := bufio.NewReaderSize(conn, 64<<10)
	enc := json.NewEncoder(conn)
	for {
		line, err := readBoundedLine(r, maxFrameBytes)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET) {
				return
			}
			if errors.Is(err, errFrameTooLarge) {
				// Frame too big to dispatch and we may be mid-frame
				// (no newline seen) — drop the connection after a
				// classified error so a buggy client gets a clear
				// signal instead of a silent close.
				_ = enc.Encode(rpc.Response{ID: "", Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
				return
			}
			s.logger.Debugf("conn read: %v", err)
			return
		}
		var req rpc.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpc.Response{ID: "", Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
			continue
		}
		if terminal := s.dispatch(connCtx, &req, enc, r); terminal {
			return
		}
	}
}

// readBoundedLine reads from r up to and including the next '\n', returning
// the line bytes including the trailing newline. Returns errFrameTooLarge
// if no newline appears within maxBytes — the caller should treat this as
// terminal and close the connection (mid-frame state is unrecoverable
// without an out-of-band resync token, which the protocol does not have).
//
// Uses bufio.ReadSlice in a loop so the per-iteration allocation stays
// bounded by bufio's internal buffer; the accumulated len(buf) check
// short-circuits before append can grow without bound.
func readBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > maxBytes {
			return nil, errFrameTooLarge
		}
		buf = append(buf, chunk...)
		if err == nil {
			return buf, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Got a partial chunk that filled bufio's internal buffer
			// without finding '\n'; keep reading.
			continue
		}
		return buf, err
	}
}

// recoverHandler is the defer target the dispatcher uses to convert a
// handler panic into an internal_error response on the *same* JSON-RPC id
// instead of letting it unwind through serveConn and kill the listener
// goroutine. The stack trace lands in the daemon log so the panic is
// debuggable; the misbehaving client gets a classified error and the
// other connected clients keep working.
func recoverHandler(logger *Logger, enc *json.Encoder, req *rpc.Request) {
	rec := recover()
	if rec == nil {
		return
	}
	method := ""
	id := ""
	if req != nil {
		method = req.Method
		id = req.ID
	}
	logger.Errorf("panic in handler method=%s id=%s: %v\n%s", method, id, rec, debug.Stack())
	writeError(enc, id, rpc.CodeInternal, fmt.Sprintf("internal panic: %v", rec))
}

func (s *Server) dispatch(ctx context.Context, req *rpc.Request, enc *json.Encoder, r *bufio.Reader) (terminal bool) {
	// A handler panic must not unwind through serveConn — that would kill
	// the per-connection goroutine and disconnect *other* clients sharing
	// the listener. Convert to a classified error on the request's id so
	// the panic is debuggable from the log and the misbehaving caller
	// gets a clean error.
	defer recoverHandler(s.logger, enc, req)
	// Per-request deadline. Streaming methods get no deadline (the stream
	// itself owns its lifetime). Unary deadlines are bounded below the
	// CLI's 60s per-invocation budget so the daemon errors first with a
	// classified message instead of the CLI surfacing a generic socket
	// timeout. Without this, a slow handler (e.g. chain.fetch's
	// per-strike contract resolution) could outlive the CLI cancellation
	// and leak gateway market-data slots.
	var cancel context.CancelFunc
	ctx, cancel = requestCtx(ctx, req.Method)
	defer cancel()
	switch req.Method {
	case rpc.MethodAccountSummary:
		s.unary(req, enc, func() (any, error) { return s.handleAccountSummary(ctx) })
	case rpc.MethodPositionsList:
		s.unary(req, enc, func() (any, error) { return s.handlePositionsList(ctx, req) })
	case rpc.MethodQuoteSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleQuoteSnapshot(ctx, req) })
	case rpc.MethodChainFetch:
		s.unary(req, enc, func() (any, error) { return s.handleChainFetch(ctx, req) })
	case rpc.MethodChainExpiries:
		s.unary(req, enc, func() (any, error) { return s.handleChainExpiries(ctx, req) })
	case rpc.MethodScanRun:
		s.unary(req, enc, func() (any, error) { return s.handleScanRun(ctx, req) })
	case rpc.MethodScanList:
		s.unary(req, enc, func() (any, error) { return s.handleScanList(), nil })
	case rpc.MethodScanParams:
		s.unary(req, enc, func() (any, error) { return s.handleScanParams(ctx, req) })
	case rpc.MethodHistoryDaily:
		s.unary(req, enc, func() (any, error) { return s.handleHistoryDaily(ctx, req) })
	case rpc.MethodTechnical:
		s.unary(req, enc, func() (any, error) { return s.handleTechnical(ctx, req) })
	case rpc.MethodMarketCalendar:
		s.unary(req, enc, func() (any, error) { return s.handleMarketCalendar(req) })
	case rpc.MethodBreadthSPX:
		s.unary(req, enc, func() (any, error) { return s.handleBreadthSPX(ctx, req) })
	case rpc.MethodGammaZeroSPX:
		s.unary(req, enc, func() (any, error) { return s.handleGammaZeroSPX(ctx, req) })
	case rpc.MethodRegimeSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleRegimeSnapshot(ctx, req) })
	case rpc.MethodAlertCandidates:
		s.unary(req, enc, func() (any, error) { return s.handleAlertCandidates(ctx, req) })
	case rpc.MethodAlertStatus:
		s.unary(req, enc, func() (any, error) { return s.handleAlertStatus(ctx, req) })
	case rpc.MethodRegimeHistory:
		s.unary(req, enc, func() (any, error) { return s.handleRegimeHistory(req) })
	case rpc.MethodRulesHistory:
		s.unary(req, enc, func() (any, error) { return s.handleRulesHistory(req) })
	case rpc.MethodStressHistory:
		s.unary(req, enc, func() (any, error) { return s.handleStressHistory(req) })
	case rpc.MethodReconEquity:
		s.unary(req, enc, func() (any, error) { return s.handleReconEquity(req) })
	case rpc.MethodMarketEventsSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleMarketEventsSnapshot(ctx, req) })
	case rpc.MethodStatusHealth:
		s.unary(req, enc, func() (any, error) { return s.handleStatusHealth(), nil })
	case rpc.MethodTradingStatus:
		s.unary(req, enc, func() (any, error) { return s.handleTradingStatus(), nil })
	case rpc.MethodTradingPaperSmoke:
		s.unary(req, enc, func() (any, error) { return s.handleTradingPaperSmoke(ctx, req) })
	case rpc.MethodAutoTradeStatus:
		s.unary(req, enc, func() (any, error) { return s.handleAutoTradeStatus(), nil })
	case rpc.MethodRulesSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleRulesSnapshot(ctx, req) })
	case rpc.MethodBriefSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleBriefSnapshot(ctx, req) })
	case rpc.MethodBriefAck:
		s.unary(req, enc, func() (any, error) { return s.handleBriefAck(ctx, req) })
	case rpc.MethodNudgesSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleNudgesSnapshot(ctx, req) })
	case rpc.MethodNudgesCutoverReview:
		s.unary(req, enc, func() (any, error) { return s.handleNudgesCutoverReview(ctx, req) })
	case rpc.MethodRiskPolicySnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicySnapshot(ctx, req) })
	case rpc.MethodRiskPolicyCapitalEvent:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicyCapitalEvent(ctx, req) })
	case rpc.MethodRiskPolicyOverride:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicyOverride(ctx, req) })
	case rpc.MethodRiskPolicyResetDrawdown:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicyResetDrawdown(ctx, req) })
	case rpc.MethodRiskPolicyCorrectPeak:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicyCorrectPeak(ctx, req) })
	case rpc.MethodRiskPolicyArtefact:
		s.unary(req, enc, func() (any, error) { return s.handleRiskPolicyArtefact(ctx, req) })
	case rpc.MethodReconSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleReconSnapshot(ctx, req) })
	case rpc.MethodReconStatus:
		s.unary(req, enc, func() (any, error) { return s.handleReconStatus(ctx, req) })
	case rpc.MethodReconCheck:
		s.unary(req, enc, func() (any, error) { return s.handleReconCheck(ctx, req) })
	case rpc.MethodReconBacktest:
		s.unary(req, enc, func() (any, error) { return s.handleReconBacktest(ctx, req) })
	case rpc.MethodReconDismiss:
		s.unary(req, enc, func() (any, error) { return s.handleReconDismiss(ctx, req) })
	case rpc.MethodTradeProposalsSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsSnapshot(req), nil })
	case rpc.MethodTradeProposalsRefresh:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsRefresh(ctx, req) })
	case rpc.MethodTradeProposalsPreview:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsPreview(ctx, req) })
	case rpc.MethodTradeProposalsSubmit:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsSubmit(ctx, req) })
	case rpc.MethodTradeProposalsIgnore:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsIgnore(req), nil })
	case rpc.MethodTradeProposalsReducePreview:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsReducePreview(ctx, req) })
	case rpc.MethodTradeProposalsReduceSubmit:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsReduceSubmit(ctx, req) })
	case rpc.MethodTradeProposalsReducePortfolioPreview:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsReducePortfolioPreview(ctx, req) })
	case rpc.MethodTradeProposalsReducePortfolioSubmit:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsReducePortfolioSubmit(ctx, req) })
	case rpc.MethodOpportunitiesStatus:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesStatus(), nil })
	case rpc.MethodOpportunitiesSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesSnapshot(req), nil })
	case rpc.MethodOpportunitiesRefresh:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesRefresh(ctx, req) })
	case rpc.MethodOpportunitiesPreviewExercise:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesPreviewExercise(ctx, req) })
	case rpc.MethodOpportunitiesSubmitExercise:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesSubmitExercise(ctx, req) })
	case rpc.MethodOpportunitiesIgnore:
		s.unary(req, enc, func() (any, error) { return s.handleOpportunitiesIgnore(req), nil })
	case rpc.MethodSettingsGet:
		s.unary(req, enc, func() (any, error) { return s.handleSettingsGet() })
	case rpc.MethodSettingsUpdate:
		s.unary(req, enc, func() (any, error) { return s.handleSettingsUpdate(ctx, req) })
	case rpc.MethodOrdersOpen:
		s.unary(req, enc, func() (any, error) { return s.handleOrdersOpen(ctx, req) })
	case rpc.MethodOrdersHistory:
		s.unary(req, enc, func() (any, error) { return s.handleOrdersHistory(ctx, req) })
	case rpc.MethodOrderStatus:
		s.unary(req, enc, func() (any, error) { return s.handleOrderStatus(ctx, req) })
	case rpc.MethodOrderPreview:
		s.unary(req, enc, func() (any, error) { return s.handleOrderPreview(ctx, req) })
	case rpc.MethodQuoteSubscribe:
		s.handleQuoteSubscribe(ctx, req, enc, r)
		return true
	case rpc.MethodCancel:
		s.unary(req, enc, func() (any, error) { return s.handleCancel(req) })
	case rpc.MethodOrderPlace:
		s.unary(req, enc, func() (any, error) { return s.handleOrderPlace(ctx, req) })
	case rpc.MethodOrderModify:
		s.unary(req, enc, func() (any, error) { return s.handleOrderModify(ctx, req) })
	case rpc.MethodOrderCancel:
		s.unary(req, enc, func() (any, error) { return s.handleOrderCancel(ctx, req) })
	case rpc.MethodPurgeStatus:
		s.unary(req, enc, func() (any, error) { return s.handlePurgeStatus(ctx, req) })
	case rpc.MethodPurgeExecute:
		s.unary(req, enc, func() (any, error) { return s.handlePurgeExecute(ctx, req) })
	case rpc.MethodPurgeRestorePreview:
		s.unary(req, enc, func() (any, error) { return s.handlePurgeRestorePreview(ctx, req) })
	case rpc.MethodPurgeRestoreExecute:
		s.unary(req, enc, func() (any, error) { return s.handlePurgeRestoreExecute(ctx, req) })
	default:
		writeError(enc, req.ID, rpc.CodeUnknownMethod, "unknown method: "+req.Method)
	}
	return false
}

// requestCtx returns a derived context with the per-method unary
// deadline applied, plus its cancel func. Streaming methods (deadline
// == 0) pass parent through with a no-op cancel so callers can defer
// uniformly.
func requestCtx(parent context.Context, method string) (context.Context, context.CancelFunc) {
	if d := unaryDeadline(method); d > 0 {
		return context.WithTimeout(parent, d)
	}
	return parent, func() {}
}

// unaryDeadline returns the per-request deadline for a method, or 0 for
// methods that own their own lifetime (streaming). Values are picked to
// stay below the CLI's 60s per-invocation budget in cmd/ibkr/main.go so
// the daemon's classified error reaches the user instead of a raw socket
// timeout, while still being generous enough for the slowest legitimate
// path: chain.fetch's sequential per-strike contract resolution.
//
// MethodPositionsList gets a long budget because the first call after
// daemon start has to resolve every held option's contract details
// (cold cache, 4-way parallel × up to 5 s per leg) before the Greeks
// model-computation tick can be polled. After the option-contract
// cache is warm, subsequent positions calls return in under 5 s; the
// 30 s ceiling is for the first-call worst case.
func unaryDeadline(method string) time.Duration {
	switch method {
	case rpc.MethodChainFetch, rpc.MethodChainExpiries:
		// 50 s = 5 s spot snapshot + 4 waves × ~10 s/wave cold-start. The
		// cold path on a fresh-client-ID TWS session pays per-leg contract-
		// details warmup (2-3 s × first-wave timeouts → retries) on top of
		// the per-leg market-data tick window; observed cold cost on a
		// 14-leg AAPL fetch is 25-30 s and at 25 s the handler is cut off
		// mid-leg-fanout. CLI ceiling is 60 s ([cmd/ibkr/main.go:125]) so
		// 50 s leaves room for the classified daemon error to surface
		// before the socket times out.
		return 50 * time.Second
	case rpc.MethodHistoryDaily:
		// 55 s — interactive daily-history reads may need a cold
		// contract-details round trip plus an HMDS request admitted through
		// the paced historical bucket. Keep this below the default 60 s CLI
		// ceiling so timeout errors remain daemon-classified.
		return 55 * time.Second
	case rpc.MethodPositionsList:
		return 30 * time.Second
	case rpc.MethodTechnical:
		return 75 * time.Second
	case rpc.MethodMarketCalendar, rpc.MethodBreadthSPX, rpc.MethodAlertCandidates, rpc.MethodAlertStatus:
		// 2 s — both handlers are pure projections of in-process data.
		// handleMarketCalendar reads embedded official schedules;
		// handleBreadthSPX reads in-memory engine state (Get() +
		// History()) while the long-running compute sits on the engine
		// scheduler goroutine. A handler taking >2 s here is a sign of
		// mutex contention or a stuck scheduler, not legitimate work.
		return 2 * time.Second
	case rpc.MethodGammaZeroSPX:
		// 55 s — under the CLI's 60 s ceiling but generous enough to
		// support meaningful WaitMs values. The actual compute runs
		// on a daemon-internal goroutine outside this budget; the
		// only thing under the deadline is the snapshot + (optional)
		// wait for the cached result. A caller that sets WaitMs >
		// 50000 ms will get a clean "computing" envelope back once
		// the deadline ticks, not a socket timeout.
		return 55 * time.Second
	case rpc.MethodRegimeSnapshot:
		// 50 s — the daemon-owned acquisition retains its established 45 s
		// upper bound. Five seconds of response headroom lets a cold caller
		// receive the cache's typed regime_unavailable classification after a
		// timed-out/incomplete refresh instead of racing into a generic socket
		// timeout. Still below the CLI's 60 s ceiling.
		return 50 * time.Second
	case rpc.MethodBriefSnapshot, rpc.MethodBriefAck:
		// Brief composition fans out across the same gateway-heavy account,
		// positions, regime, market-event, and rulebook reads as canary.
		return 75 * time.Second
	case rpc.MethodNudgesSnapshot, rpc.MethodNudgesCutoverReview, rpc.MethodReconStatus, rpc.MethodReconCheck:
		// Local policy, retained statements, and daemon state only.
		return 5 * time.Second
	case rpc.MethodMarketEventsSnapshot:
		return 20 * time.Second
	case rpc.MethodScanRun:
		// Up to 35 s scanner-subscription budget (off-hours cold-start
		// for HIGH_OPEN_GAP / TOP_PERC_GAIN / HIGH_OPT_IMP_VOLAT_OVER_HIST
		// can take 25-45 s) + per-row enrichment in waves of
		// scanEnrichConcurrency × scanEnrichWindow each. Typical RTH scan:
		// ~15 s. Worst case off-hours: 35 s scan + 20 s enrichment + slack.
		// The CLI per-invocation deadline at cmd/ibkr/main.go is bumped to
		// 90 s in lockstep so the daemon's classified error reaches the
		// user instead of a raw socket timeout.
		return 75 * time.Second
	case rpc.MethodScanParams:
		// ~200 KB XML payload, single round trip — usually <2 s. Generous
		// budget to absorb a degraded gateway without timing out before
		// the connection error surfaces.
		return 15 * time.Second
	case rpc.MethodOrderPreview:
		// 55 s — order preview does a quote snapshot, position-impact
		// read, then broker WhatIf. The WhatIf leg can take longer than
		// a fast quote path on a v203 TWS session; leave enough room for
		// it while still beating the CLI's default 60 s unary ceiling.
		return 55 * time.Second
	case rpc.MethodPurgeExecute, rpc.MethodPurgeRestorePreview, rpc.MethodPurgeRestoreExecute, rpc.MethodTradeProposalsRefresh, rpc.MethodTradeProposalsPreview, rpc.MethodTradeProposalsSubmit, rpc.MethodTradeProposalsReducePreview, rpc.MethodTradeProposalsReduceSubmit, rpc.MethodOpportunitiesRefresh, rpc.MethodOpportunitiesPreviewExercise, rpc.MethodOpportunitiesSubmitExercise:
		return 55 * time.Second
	case rpc.MethodTradeProposalsReducePortfolioPreview, rpc.MethodTradeProposalsReducePortfolioSubmit:
		// A sweep previews/places each eligible leg sequentially; the budget
		// scales past the single-order 55s bucket. Per-leg deadlines (legContext)
		// keep one stuck leg from consuming the whole window.
		return 120 * time.Second
	case rpc.MethodTradingPaperSmoke:
		// Quote + WhatIf preview, place, ≤60 s ack wait, 15 s detached
		// cancel budget. The CLI's paper-smoke budget is 120 s in lockstep
		// so the daemon's classified error reaches the user.
		return 100 * time.Second
	case rpc.MethodAccountSummary, rpc.MethodQuoteSnapshot:
		return 10 * time.Second
	case rpc.MethodStatusHealth, rpc.MethodTradingStatus, rpc.MethodAutoTradeStatus, rpc.MethodOpportunitiesStatus, rpc.MethodSettingsGet, rpc.MethodSettingsUpdate, rpc.MethodOrdersOpen, rpc.MethodOrdersHistory, rpc.MethodOrderStatus, rpc.MethodPurgeStatus, rpc.MethodTradeProposalsSnapshot, rpc.MethodTradeProposalsIgnore, rpc.MethodOpportunitiesSnapshot, rpc.MethodOpportunitiesIgnore, rpc.MethodScanList:
		return 5 * time.Second
	case rpc.MethodQuoteSubscribe:
		return 0
	default:
		return 15 * time.Second
	}
}

// unary wraps a handler so result/error envelopes are uniform.
func (s *Server) unary(req *rpc.Request, enc *json.Encoder, fn func() (any, error)) {
	res, err := fn()
	if err != nil {
		code, msg := classifyError(err)
		writeError(enc, req.ID, code, msg)
		return
	}
	buf, err := json.Marshal(res)
	if err != nil {
		writeError(enc, req.ID, rpc.CodeInternal, "marshal result: "+err.Error())
		return
	}
	_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Result: buf})
}

func writeError(enc *json.Encoder, id, code, message string) {
	_ = enc.Encode(rpc.Response{ID: id, Ok: false, Error: &rpc.Error{Code: code, Message: message}})
}

func classifyError(err error) (string, string) {
	var bad *badRequestError
	var contractTimeout *chainContractTimeoutError
	var mdAbsent *ibkrlib.MarketDataAbsenceError
	var regimeUnavailable *regimeSnapshotCacheUnavailableError
	switch {
	case errors.As(err, &regimeUnavailable):
		return rpc.CodeRegimeUnavailable, regimeUnavailable.Error()
	case errors.As(err, &bad):
		return rpc.CodeBadRequest, err.Error()
	case errors.As(err, &contractTimeout):
		return rpc.CodeTimeout, err.Error()
	case errors.As(err, &mdAbsent):
		// Entitlement-absence suppression (recent IBKR 354). Reuses the
		// symbol_inactive wire code — same caller semantics ("this symbol
		// will not produce data right now; do not hot-retry") — while the
		// message carries the precise rejection and retry time.
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrSymbolInactive):
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrIBKRUnavailable):
		return rpc.CodeGatewayUnavailable, err.Error()
	case errors.Is(err, ErrTradingDisabled):
		return rpc.CodeTradingDisabled, err.Error()
	case errors.Is(err, ibkrlib.ErrContractDetailsTimeout):
		return rpc.CodeTimeout, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return rpc.CodeTimeout, err.Error()
	default:
		return rpc.CodeInternal, err.Error()
	}
}

func (s *Server) bumpActive(delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeConns += delta
	s.resetIdleLocked()
}

func (s *Server) resetIdleLocked() {
	if s.idleTimer == nil {
		return
	}
	if s.activeConns > 0 {
		s.idleTimer.Stop()
		return
	}
	s.idleTimer.Reset(s.cfg.Daemon.IdleTimeout.Std())
}

func (s *Server) runIdleWatcher(ctx context.Context) {
	timeout := s.cfg.Daemon.IdleTimeout.Std()
	if timeout <= 0 {
		<-ctx.Done()
		return
	}
	s.mu.Lock()
	s.idleTimer = time.NewTimer(timeout)
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			s.idleTimer.Stop()
			return
		case <-s.idleStop:
			s.idleTimer.Stop()
			return
		case <-s.idleTimer.C:
			s.mu.Lock()
			active := s.activeConns
			s.mu.Unlock()
			if active == 0 && !s.isBusy() {
				s.logger.Infof("Idle timeout reached (%s); shutting down", timeout)
				return
			}
			s.mu.Lock()
			s.idleTimer.Reset(timeout)
			s.mu.Unlock()
		}
	}
}

// backgroundTasks returns the set of daemon-internal long-running
// computes that are running or waiting for a scheduled retry. Always
// returns a non-nil slice (possibly empty) so consumers can rely on
// len()==0 to mean idle.
//
// This is the single source of truth for "what background work is in
// flight." Three callers ride it: isBusy() (idle-watcher gate),
// handleStatusHealth (wire-emitted BackgroundTasks list), and the
// regime partial-envelope path (names the contending task in the
// ErrorMessage). Adding a new long-running task means adding one
// clause here; the three caller surfaces stay coherent automatically.
//
// Distinct from activeConns: those track open client sockets, but the
// engine-style background tasks run without any client connection held
// open. The breadth bootstrap can span multiple retry waits while
// IBKR's contract-details bucket refills, and gamma compute runs
// ~1–2 min — both can outlive a CLI invocation that triggered them.
func (s *Server) backgroundTasks() []rpc.BackgroundTaskStatus {
	tasks := []rpc.BackgroundTaskStatus{}
	if s.breadth != nil && s.breadth.IsBusy() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "breadth-spx", Status: "computing"})
	}
	if s.zeroGamma != nil && s.zeroGamma.IsComputing() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "gamma-zero", Status: "computing"})
	}
	if s.regimePrewarming.Load() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "regime-prewarm", Status: "computing"})
	}
	if s.regimeSnapshots != nil && s.regimeSnapshots.refreshing() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "regime-refresh", Status: "computing"})
	}
	if s.flexFetch.isBusy() {
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "flex-report", Status: "checking"})
	}
	if scoped, total := s.openBrokerOrderCounts(); total > 0 {
		// A daemon that idle-exits while protective stops are working goes
		// dark on fills, cancels, and the order journal exactly when they
		// matter; stay up while any non-terminal order is on the books —
		// including other-scope rows. The status string leads with the
		// current-scope count so it agrees with the orders tab, and
		// discloses the off-scope remainder instead of silently differing.
		status := fmt.Sprintf("%d working", scoped)
		if rest := total - scoped; rest > 0 {
			status = fmt.Sprintf("%d working (+%d other scope)", scoped, rest)
		}
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "open-orders", Status: status})
	}
	return tasks
}

// openBrokerOrderCounts reports non-terminal journaled orders: scoped counts
// only the connected account/mode (what the orders tab shows), total spans
// all scopes (what idle shutdown must respect). Zero on any journal problem:
// idle shutdown must not be blocked by a broken state directory.
func (s *Server) openBrokerOrderCounts() (scoped, total int) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return 0, 0
	}
	scope := s.currentBrokerStateScope()
	for _, v := range views {
		if !v.Open {
			continue
		}
		total++
		if orderViewMatchesBrokerScope(v, scope) {
			scoped++
		}
	}
	return scoped, total
}

// isBusy reports whether the daemon has daemon-internal background work
// that should defer idle shutdown. Derived from backgroundTasks() so
// the idle watcher and the status surface never disagree about what's
// running.
func (s *Server) isBusy() bool {
	return len(s.backgroundTasks()) > 0
}
