// Package daemon implements Canary's long-running runtime authority. It
// owns broker connectivity, durable and in-memory runtime state, background
// schedulers, policy execution, and the gated coordination of broker writes;
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

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// maxFrameBytes caps each newline-delimited JSON-RPC request the daemon will
// read from a single Unix-socket peer. Bound is generous (1 MiB) — every
// real CLI/MCP request is well under 10 KiB; the cap exists to prevent a
// hostile or buggy client from OOM'ing the daemon by sending a long
// newline-free byte stream that bufio.ReadBytes would otherwise grow into.
const maxFrameBytes = 1 << 20

// errFrameTooLarge is the sentinel returned by readBoundedLine when a peer
var errFrameTooLarge = fmt.Errorf("request frame exceeds %d bytes", maxFrameBytes)

// handshakeWatchdogDelay bounds how long the daemon waits for the IBKR
// lastConnectError. pkg/ibkr's per-attempt budget is 10s; 12s is just past
const handshakeWatchdogDelay = 12 * time.Second

// perCandidateConnectBudget is the hard cap on one candidate's connect+
// handshake before the failover loop moves on. pkg/ibkr's plain-handshake
// retry uses tls.Conn.HandshakeContext — which only ends when ctx is
// cancelled. Against a host that accepts TCP but never replies to
// non-TLS listener), the retry hangs indefinitely and failover never
// candidate attempt; the next status invocation sees the failover result.
var perCandidateConnectBudget = 25 * time.Second

// Server is the daemon process state.
type Server struct {
	cfg        *config.Resolved
	socketPath string
	startedAt  time.Time
	version    string
	now        func() time.Time

	// brokerWriteMu serializes the check-then-act sections of every broker
	// races, not a throughput concern. Cancel stays outside so a protective
	// cancel is never queued behind a longer placement flow.
	brokerWriteMu sync.Mutex

	// reduceBasketMu guards reduceBasketDedupe, the short-TTL replay cache for
	// so a double-tap or client retry can never fan the basket out twice.
	reduceBasketMu     sync.Mutex
	reduceBasketDedupe map[string]reduceBasketDedupeEntry

	// minTickByConID caches broker-reported minimum price increments per
	// contract (see resolveContractMinTick).
	minTickMu      sync.Mutex
	minTickByConID map[int]float64

	// fxRates keeps last-known-good BASE-per-CCY exchange rates so one
	// failed FX snapshot quote cannot strip base-currency decoration from
	// a single positions/account response (see
	fxRates *fxRateCache

	// accountSnapshots owns the short-lived, request-authored account summary
	// ready for use; connector session and broker scope are part of every key.
	accountSnapshots accountSnapshotAuthority
	// dailyPnLObservations keeps an observed same-session feed failure visible
	dailyPnLObservations dailyPnLObservationAuthority
	// dailyPnLCloseCaptures pins each scope's account Daily P&L at the
	// official close, the only figure that may serve as the last completed
	dailyPnLCloseCaptures dailyPnLCloseCaptureAuthority

	listener net.Listener

	mu               sync.Mutex
	endpoint         discover.Endpoint // post-discovery, fully concrete; mutated on reconnect (issue: AUTO rediscover)
	connector        *ibkrlib.Connector
	streams          map[string]context.CancelFunc
	lastConnectError string
	// lastDiscoveryWarn remembers the most recent reconnect-discovery
	lastDiscoveryWarn string

	// lastEndpointResolvedSig / lastGatewayUnreachable / lastNoEndpointUsable
	lastEndpointResolvedSig string
	lastGatewayUnreachable  string
	lastNoEndpointUsable    string

	// connectInFlight is true while a connect attempt (initial or reconnect)
	connectInFlight bool
	// initialAcceptLoopStartedForTest observes the exact startup boundary after
	// the RPC accept loop is exposed and before the initial connector goroutine
	// is launched. It lets the startup-order regression test assert that the
	// in-flight gate was already claimed at that boundary.
	initialAcceptLoopStartedForTest func()
	// reconnectFailStreak / lastReconnectAttemptAt drive the reconnect
	// consecutive failed reconnect cycles; it is bumped in reconnectFlow on
	// a failed cycle and reset to 0 by postConnectSetup on a successful
	reconnectFailStreak    int
	lastReconnectAttemptAt time.Time
	// serverCtx is captured at Start time so handlers can launch
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

	// alertEvidenceArms tracks, per alert source, the last unavailable arm the
	// evidence heartbeat logged, so transitions log once rather than per tick.
	alertEvidenceArmMu sync.Mutex
	alertEvidenceArms  map[rpc.AlertSource]alertEvidenceArmState

	idleTimer   *time.Timer
	idleStop    chan struct{}
	activeConns int

	// subs owns refcounted market-data subscriptions shared between the
	// resource subscribers. Initialized in New (depends only on Server's
	subs *subManager

	// expiryIVCache memoises per-(symbol, expiry) ATM IV so a fresh
	expiryIVs *expiryIVCache

	// quoteLiquidity memoises 20-day average volume / dollar volume derived
	quoteLiquidity *quoteLiquidityCache
	// marketDataWitnessAt is the last successful broker price observation.
	// failed quote path without treating the witness as durable entitlement.
	marketDataWitnessAt atomic.Int64

	// prevCloses memoises per-symbol previous-session close (tick 9)
	prevCloses *prevCloseCache

	// greeks memoises per-option model-computation Greeks so the
	// positions handler doesn't re-subscribe to each option leg on
	// every invocation. Short TTL (60 s) because Greeks shift with
	// spot, but long enough to make back-to-back calls free.
	greeks *greeksCache

	// zeroGamma holds the served dealer zero-gamma last-good plus any RTH
	zeroGamma *gammaZeroCache
	// gammaOI persists per-contract option open interest observed by the gamma
	// collector. Missing OI refreshes never write through this store; only live
	gammaOI *gammaOpenInterestStore
	// gammaGrids persists the last successful classed expiry/strike grid
	// signal for the session (observed 2026-06-09, IBKR code 2157).
	gammaGrids *expiryGridStore
	// gammaStarted guards the boot-time prewarm so it only fires once
	gammaStarted sync.Once
	// gammaRefreshStarted guards the active-session refresh loop. The loop
	gammaRefreshStarted sync.Once

	// breadth runs the SPX 50-DMA breadth compute. The engine owns
	breadth *spx.Engine
	// breadthStarted guards breadth.Run() so the scheduler goroutine
	// only spins up after the gateway completes its first handshake.
	// post-reconnect — this Once ensures Run() launches exactly once
	breadthStarted sync.Once
	// breadthConnector is a dedicated IBKR client connection (separate
	// Read through breadthGatewayConnector, never directly: that
	breadthConnector *ibkrlib.Connector
	// maintenanceWindows is the parsed broker reset schedule applied to
	// every connector this server constructs; see resolveMaintenanceWindows.
	maintenanceWindows []ibkrlib.MaintenanceWindow
	// breadthConnectInFlight / breadthConnectFailStreak /
	// connectInFlight / reconnectFailStreak / lastReconnectAttemptAt,
	// connector a once-per-PROCESS resource: the 2026-08-03 23:45 TWS
	breadthConnectInFlight      bool
	breadthConnectFailStreak    int
	lastBreadthConnectAttemptAt time.Time

	// membersRefresher runs the daemon-internal SPX-constituent
	// `~/.cache/ibkr/spx-members/sp500-members.json` and pushed into
	// the cache-path resolution failed at New (no $HOME / XDG).
	membersRefresher *spx.Refresher
	// membersRefresherStarted guards Run() launch so a reconnect-
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
	contractCacheLoaded map[string]ibkrlib.ContractDetailsLite
	// contractCacheSaveStarted gates the background save loop to
	// exactly once per Server lifetime, mirroring breadthStarted /
	contractCacheSaveStarted sync.Once

	// streaks persists each regime indicator's consecutive-sessions-in-band
	// on band transitions. The path-backed form exists only for legacy import
	streaks *StreakStore

	// regimeDecisions appends typed regime lifecycle events to daemon.db for
	// threshold calibration. Its path-backed form exists only for legacy import
	// and isolated unit tests; journaling remains best-effort and never fails a
	// snapshot.
	regimeDecisions *regimeDecisionJournal

	// stressDecisions is the daemon.db-backed portfolio-stress evidence corpus
	// form has the same legacy import/test-only contract as regimeDecisions.
	stressDecisions *stressDecisionJournal

	// rulesJournalMu serializes the legacy file-backed rule-transition seam;
	rulesJournalMu sync.Mutex

	// coreStore is the daemon's sole live persistence authority. New resolves
	// its path but never opens it: only the Start winner may touch daemon.db,
	// after both the socket-specific instance lock and state-root persistence
	// lock have been acquired.
	coreStore        *corestore.Store
	coreStorePath    string
	coreStorePathErr error
	// productionStateDatabase distinguishes the XDG authority from isolated
	// test/offline databases. Only production enforces the v2-to-v3 bridge.
	productionStateDatabase bool
	persistenceLock         *persistenceLock
	authorityCloseOnce      sync.Once
	authorityCloseErr       error

	// orderJournal is the durable audit log for order intents and broker
	orderJournal *orderJournalStore
	// strategyLineage is a read-through cache of durable submitted group
	// drafts. Position polling uses it to detect broken leg ratios without
	// rereading the complete order journal on every refresh.
	strategyLineageMu     sync.Mutex
	strategyLineageLoaded bool
	strategyLineage       map[string]rpc.StrategyOrderDraft
	// orderSnapshotFn is the open-order snapshot seam for the reconcile
	orderSnapshotFn func(context.Context) (ibkrlib.OpenOrderSnapshot, error)
	// Test-only final-boundary seams. Production leaves these nil. They run
	orderReconcileBeforeCommit     func()
	orderReconcileBeforeLatchClear func()
	protectionBeforeCommit         func()
	orderIntegrityBeforeCommit     func()
	stableBrokerEvidenceForTest    func(daemonBrokerEvidenceBinding, func() error) (bool, error)
	// protectionOrderSnapshot* retains only a short-lived, complete broker
	// inventory receipt for the 30-second Protection producer heartbeat. The
	// generation; any later broker order event makes the receipt ineligible.
	protectionOrderSnapshotMu     sync.Mutex
	protectionOrderSnapshotCache  protectionOrderSnapshotCache
	protectionOrderSnapshotFlight *protectionOrderSnapshotFlight
	// orderReconcileLoopStarted gates the standing reconcile sweep to one
	orderReconcileLoopStarted sync.Once
	// proposalOutcomes is the append-only measurement book for protection
	// identity and order-journal refs without storing raw preview tokens.
	proposalOutcomes *proposalOutcomeStore
	// platformSettings persists daemon-owned runtime preferences. Gateway,
	// account, trading mode, and build capability stay config/build owned;
	// this store only carries settings the operator may edit at runtime.
	platformSettings    *platformSettingsStore
	protectionPolicies  *protectionPolicyManager
	tradeProposals      *proposalEngine
	opportunityPolicies *opportunityPolicyManager
	opportunities       *opportunityEngine
	marketEvents        *marketEventCache
	// riskPolicies loads the operator's risk constitution
	// Advisory/shadow end to end in v1: neither may reach broker-write
	riskPolicies *riskPolicyManager
	riskCapital  *riskCapitalStore
	// nudges owns only opaque governance occurrence state. Eligibility remains
	nudges *nudgeStateStore
	// Test-only deterministic seams. Production leaves all nil.
	nudgeScanCheckpoint   func(string)
	shadowBookkeepingHook func()
	// reconMu serializes report-content mutations with report-backed human
	// and automatic reconcile appends so a report id cannot race a new
	reconMu sync.Mutex
	// flexFetch tracks the daily Flex statement ingestion for post-trade
	// the broker; sanitized status only, never the token.
	flexFetch flexFetchState
	// Test-only seams for the broker fetch and retained-statement projection.
	flexFetchOnceFn  func(context.Context, time.Time) (flexFetchOutcome, error)
	flexProjectionFn func(context.Context) error
	// earnings backs the trading rulebook's catalyst rules (6-8); LKG cache,
	// async refresh only — never fetched on a snapshot or preview path.
	earnings *earningsCache
	// earningsTerminal is exact-contract, provenance-bound terminal issuer
	// SQLite authority at startup; symbol text alone never grants an exemption.
	earningsTerminal *earningsTerminalStore
	// lastRules memoizes the most recent rulebook evaluation for advisory
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
	orderTokens *orderTokenSigner
	// orderPreview* hooks let tests exercise the full preview gate/token path
	orderPreviewQuote            func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error)
	orderPreviewPositionImpact   func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error)
	orderRiskAuthorityForTest    func(context.Context, rpc.TradingStatus, rpc.ContractParams, string, int) (orderPositionAuthority, error)
	orderFXRateForTest           func(context.Context, string, string, time.Duration) (float64, time.Time, error)
	orderContractResolverForTest func(context.Context, rpc.ContractParams, time.Duration) (rpc.ContractParams, error)
	orderPreviewWhatIf           func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error)
	orderWritesEnabled           func() bool
	gatewayReadyForTrading       func() bool
	orderReserveBrokerID         func(context.Context) (int, error)
	orderPlaceBroker             func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error
	orderCancelBroker            func(context.Context, int) error
	optionExerciseBroker         func(context.Context, ibkrlib.OptionExerciseRequest) error
	// orderWriteBindingForTest and orderWriteBeforeBrokerSend are deterministic
	// nil; broker writes then require a real ready Connector session.
	orderWriteBindingForTest        func(rpc.TradingStatus) (*ibkrlib.Connector, uint64, ibkrlib.ConnectorSessionBinding, brokerStateScope)
	orderWriteBeforeBrokerSend      func()
	orderWriteOriginBlockersForTest func(rpc.TradingStatus, string) []rpc.TradingBlocker

	// regimePrewarming is set while prewarmRegimeSymbols' fan-out is in
	// flight. Surfaces via backgroundTasks() so the idle watcher defers
	// shutdown and `canary status` reflects the work — same coherence
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
	// and USD/JPY weekly change; transient HMDS failures must not make
	regimeHistory *regimeHistoryCache
	// regimeSnapshots is the daemon-owned, daemon.db-backed last-good regime
	// authority. All RPC, brief, rulebook, proposal, Stress, and alert reads
	// converge here; only a complete fan-out may publish into it.
	regimeSnapshots          *regimeSnapshotCache
	regimeProjectionRepairMu sync.Mutex
	regimeRefreshLoopWG      sync.WaitGroup
	// Stress evaluation is daemon-owned and independent of app presence and
	// decision-journal retention. Regime publications and reconnects share the
	// buffered wake; the loop drains during daemon shutdown.
	stressEvaluationWake   chan struct{}
	stressEvaluationLoopWG sync.WaitGroup
	// stressEvaluationSourceReaderForTest exercises the real evaluator against
	// typed source receipts without constructing a broker wire session.
	stressEvaluationSourceReaderForTest stressEvaluationSourceReader
	rulebookRefreshLoopWG               sync.WaitGroup
	regimeConsumerWakeMu                sync.Mutex
	regimeConsumerRevision              int64
	// Successful canonical gamma publications invalidate only Regime's refresh
	// authority remains visible until a complete replacement publishes.
	regimeRefreshWake chan struct{}
	// alertShadow is the daemon-owned source-neutral alert producer. It persists
	alertEpisodes *alertEpisodeRegistry
	alertShadow   *alertShadowComposer
	// dataHealthObserveMu keeps the hottest read-only status path independent
	// from alert registry persistence. Only one detached observation may run at
	// failures back off in memory so an unhealthy SQLite store cannot turn
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
	// in pkg/ibkr) and postConnectSetup finishing its synchronous
	// the daemon never re-enters a "starting up" state from the
	// never stays false forever in practice.
	postConnectSetupDone atomic.Bool

	lock *instanceLock

	logger *Logger

	// attempterFactory builds a connectAttempter for a candidate endpoint
	// during the failover loop. Production uses buildAttempter (which
	// callers that construct *Server directly (legacy tests) get a nil
	// factory and must either assign it themselves or only exercise
	attempterFactory func(discover.Endpoint) connectAttempter
}

// Options configures a Server.
type Options struct {
	Config     *config.Resolved
	SocketPath string
	Version    string
	Logger     *Logger
	// StateDatabasePath overrides daemon.db for isolated tests and offline
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
		s.productionStateDatabase = true
	}
	s.attempterFactory = s.buildAttempter
	s.installSubs()
	s.installBreadthEngine()
	s.installMembersRefresher()
	s.installContractStore()
	s.installStreakStore()
	s.installStressDecisionJournal()
	s.installOrderJournalStore()
	s.installProposalOutcomeStore()
	s.installPlatformSettingsStore()
	s.installProtectionPolicyManager()
	s.installRiskPolicyManager()
	s.installRiskCapitalStore()
	s.installNudgeStateStore()
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
}

func (s *Server) installPlatformSettingsStore() {
	path, err := defaultPlatformSettingsPath()
	if err != nil {
		s.warnf("platform settings: resolve state path: %v (runtime settings disabled)", err)
		return
	}
	// New runs before the instance/persistence locks are won. Resolve the
	// legacy path for the explicit cutover only; never read it here. The Start
	s.platformSettings = &platformSettingsStore{path: path, data: platformSettingsData{Version: platformSettingsDocVersion}}
}

func (s *Server) installRegimeSeriesCache() {
	dir, err := regimeSeriesCacheDefaultDir()
	if err != nil {
		s.logger.Warnf("regime series cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.regimeSeries = newRegimeSeriesCache(dir, s.warnf)
}

func (s *Server) installRegimeHistoryCache() {
	dir, err := regimeHistoryCacheDefaultDir()
	if err != nil {
		s.logger.Warnf("regime history cache: resolve dir: %v (persistence disabled)", err)
		return
	}
	s.regimeHistory = newRegimeHistoryCache(dir, s.warnf)
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
// its own IBKR clientID, separate from the primary connector that
// acquires the single-instance flock, so every autospawn race loser
// fix). The winning daemon logs the source exactly once, on first
func (s *Server) installBreadthEngine() {
	dir, err := spx.DefaultDir()
	if err != nil {
		s.logger.Warnf("breadth: resolve cache dir: %v (engine disabled)", err)
		return
	}
	fetcher := newBreadthFetcher(s.breadthGatewayConnector)
	s.breadth = spx.New(spx.NewStore(dir), fetcher, spx.Options{
		Logger: s.logger, MembersFn: s.resolveBreadthMembers, DeferStoreLoad: true,
		HealthGate: s.breadthLaneHealth,
	})
}

// breadthLaneHealth is the engine's transport gate: nil when a fan-out is
// worth attempting, an error naming why not. It reads the bulk lane's OWN
// connector state — the lane has its own cid and its own farm notices — so a
// primary that looks healthy cannot green-light a sweep on a dead second
// lane. Called once per planned symbol during a sweep; everything here is an
// in-memory read.
func (s *Server) breadthLaneHealth() error {
	s.mu.Lock()
	c := s.breadthConnector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		s.triggerBreadthConnect()
		return fmt.Errorf("breadth bulk connector is not ready")
	}
	if farm, impaired := breadthLaneFarmImpaired(c); impaired {
		return fmt.Errorf("historical data farm %s is %s; deferring fan-out until it recovers", farm.Name, farm.Status)
	}
	return nil
}

// breadthLaneFarmImpaired reports an explicit broken/disconnected notice on
// the lane's historical or connectivity farm rows. Absence of any notice is
// NOT impairment — a gateway that never replayed its farm burst must not
// starve breadth — and "inactive" is routine off-hours idling.
func breadthLaneFarmImpaired(c *ibkrlib.Connector) (ibkrlib.DataFarmStatus, bool) {
	if c == nil {
		return ibkrlib.DataFarmStatus{}, false
	}
	for _, farm := range c.DataFarmStatuses() {
		farmType := strings.ToLower(strings.TrimSpace(farm.Type))
		if farmType != "historical" && farmType != "connectivity" {
			continue
		}
		if dataFarmNeedsAttention(farm.Status) {
			return farm, true
		}
	}
	return ibkrlib.DataFarmStatus{}, false
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
//   - breadth engine missing (cache-dir failure) → no refresher.
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
// read pattern so handlers reading the pointer never see it mid-Stop.
// historical sweep off the lane that serves interactive RPCs; failing
func (s *Server) breadthGatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.breadthConnector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		s.triggerBreadthConnect()
		return nil
	}
	return c
}

// claimBreadthConnect reserves the bulk lane's single dial slot,
// returning true iff the caller now owns it and must run
// breadthConnectFlow (which releases it). Mirrors the gate inside
// triggerReconnect, including the ordering: the cheap refusals come
// first so a zero-value Server — the shape used by unit tests and by
// autospawn race losers that never reach Start — returns before
// touching s.now.
func (s *Server) claimBreadthConnect() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.breadthConnectInFlight {
		return false
	}
	// IsReady, not a nil check: a connector whose socket died still
	// holds a non-nil pointer, and that state is exactly the one that
	// stranded breadth for 7 h.
	if s.breadthConnector != nil && s.breadthConnector.IsReady() {
		return false
	}
	if s.serverCtx == nil || s.serverCtx.Err() != nil {
		return false
	}
	now := s.now()
	if s.breadthConnectFailStreak > 0 &&
		now.Sub(s.lastBreadthConnectAttemptAt) < reconnectBackoff(s.breadthConnectFailStreak) {
		return false
	}
	s.breadthConnectInFlight = true
	s.lastBreadthConnectAttemptAt = now
	return true
}

// breadthLaneDown reports whether the bulk lane is known dead, plus the
// failure streak and the last dial attempt so a status row can describe it
// without inventing a timestamp.
//
// The zero lastBreadthConnectAttemptAt is the gate that keeps daemon
// start-up out of the "down" state: before postConnectSetup has dialled the
// bulk lane once, a nil connector is the expected shape, not a fault. After
// that first attempt a non-ready connector is exactly the state that
// stranded breadth for 7 h, and saying so is the point.
func (s *Server) breadthLaneDown() (down bool, failStreak int, lastAttempt time.Time) {
	s.mu.Lock()
	c := s.breadthConnector
	failStreak = s.breadthConnectFailStreak
	lastAttempt = s.lastBreadthConnectAttemptAt
	s.mu.Unlock()
	if lastAttempt.IsZero() {
		return false, failStreak, lastAttempt
	}
	return c == nil || !c.IsReady(), failStreak, lastAttempt
}

// releaseBreadthConnect hands the dial slot back and folds the outcome
// reconnects immediately; failure widens the quiet period 1s→15s, the
func (s *Server) releaseBreadthConnect(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.breadthConnectInFlight = false
	if ok {
		s.breadthConnectFailStreak = 0
		return
	}
	s.breadthConnectFailStreak++
}

// breadthConnectWarnf surfaces the first failure of a streak at Warn and
// drops the repeats to Debug. A gateway that stays down re-triggers this
// lane on every breadth tick (30 s after a transport error, 12 min after
// a below-threshold pass), and the primary lane has already learned what
// an unthrottled per-attempt WARN does to this log: ~50k identical lines
// over one 13.5 h overnight outage.
func (s *Server) breadthConnectWarnf(format string, args ...any) {
	s.mu.Lock()
	first := s.breadthConnectFailStreak == 0
	s.mu.Unlock()
	if first {
		s.warnf(format, args...)
		return
	}
	s.debugf(format, args...)
}

// triggerBreadthConnect starts a background rebuild of the bulk lane if
// resolved. Returns true iff a fresh attempt was started. Never blocks:
// discovery — when the primary migrates from IB Gateway on 4001 to TWS
func (s *Server) triggerBreadthConnect() bool {
	if s.breadth == nil {
		return false
	}
	if !s.claimBreadthConnect() {
		return false
	}
	s.mu.Lock()
	ctx, ep := s.serverCtx, s.endpoint
	s.mu.Unlock()
	go s.breadthConnectFlow(ctx, ep)
	return true
}

// breadthConnectFlow dials the bulk-historical IBKR client and blocks
// until the handshake completes or breadthClientStartBudget elapses.
// The caller must already own the dial slot via claimBreadthConnect;
// this function releases it. postConnectSetup calls it inline (so
// breadth.Run() starts against a settled view of the lane) and
// triggerBreadthConnect calls it on a goroutine.
//
// Any previous connector is torn down first. On a mid-session gateway
// drop the old pointer is non-nil but dead, and leaving it in place
// would leak both the object and its clientID registration — the next
// dial would then collide with itself on cid=16.
//
// The handshake is intentionally synchronous: on the postConnectSetup
// path breadth.Run() launches shortly after on a sibling goroutine and
// would otherwise race a not-yet-ready bulk connector, dropping all 503
// fetches against nil on the first refresh. 12 s mirrors the primary's
// per-candidate budget — long enough for a healthy local gateway, short
// enough to surface a misconfigured second cid promptly.
//
// On failure (collision past MaxClientIDRetries, gateway unreachable,
// handshake timeout) the function logs and returns without setting
// s.breadthConnector. Breadth's refresh sees a nil bulk connector, the
// fetch path re-triggers behind the backoff, and the daemon as a whole
// continues running.
func (s *Server) breadthConnectFlow(ctx context.Context, primaryEp discover.Endpoint) {
	ok := false
	defer func() { s.releaseBreadthConnect(ok) }()

	s.stopBreadthConnector()

	bulkEp := primaryEp
	bulkEp.ClientID = s.cfg.Gateway.BreadthClientIDOrDefault()

	attempter := s.newConnector(bulkEp)

	startDone := make(chan struct{})
	go func() {
		defer close(startDone)
		if err := attempter.Start(ctx); err != nil {
			s.breadthConnectWarnf("breadth bulk connector start: %v", err)
		}
	}()

	select {
	case <-startDone:
	case <-time.After(breadthClientStartBudget):
		s.breadthConnectWarnf("breadth bulk connector: handshake timeout after %s (cid=%d); refresh will use 'no gateway' fallback until next tick", breadthClientStartBudget, bulkEp.ClientID)
		_ = attempter.Stop()
		return
	case <-ctx.Done():
		_ = attempter.Stop()
		return
	}

	if !attempter.IsReady() {
		s.breadthConnectWarnf("breadth bulk connector: not ready after Start (cid=%d); skipping", bulkEp.ClientID)
		_ = attempter.Stop()
		return
	}

	ok = true
	s.mu.Lock()
	s.breadthConnector = attempter
	s.mu.Unlock()
	s.logger.Infof("breadth bulk connector ready (cid=%d, separate 40-msg/sec budget from primary)", bulkEp.ClientID)

	// Wake the scheduler: a rebuilt lane is exactly when a transport-blocked
	// or unconverged refresh should try again, not after its current delay.
	if s.breadth != nil {
		s.breadth.Kick()
	}

	// Seed the bulk lane from the same persisted contract cache that
	// the primary lane was seeded from in postConnectSetup. ConIDs are
	// globally unique so both lanes get the same wire identity for
	// every symbol — no contract resolution churn on the bulk side
	// across daemon restarts. seedFromContractStore is a no-op if the
	// store wasn't successfully loaded.
	s.seedConnectorFromCache(attempter)
}

// stopBreadthConnector tears down the bulk-historical connector if one
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
const breadthClientStartBudget = 12 * time.Second

// contractCacheSaveInterval is how often the background loop reads
const contractCacheSaveInterval = 60 * time.Second

// seedFromContractStore loads the persisted contract cache from daemon.db
// startBreadthConnector to seed the bulk lane without a second authority read.
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
	if opts, err := s.contractStore.LoadOptions(); err != nil {
		s.logger.Warnf("contract cache: load options: %v", err)
	} else if seededOpts := c.SeedOptionContracts(opts); seededOpts > 0 {
		s.logger.Infof("contract cache: seeded %d option entries from daemon.db", seededOpts)
	}
}

// seedConnectorFromCache seeds c from the already-loaded
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
// a transient I/O failure shouldn't take the daemon down; the next
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
		// because both values are valid IBKR contract identities.
		maps.Copy(merged, c.SnapshotContracts())
	}
	// Option contracts come from the primary connector only (gamma's
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
// dashboard symbols with verified IBKR contracts). Caller uses this to strip delisted / renamed
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
// touching the gateway) if another Canary daemon holds the instance lock.
func (s *Server) Start(ctx context.Context) error {
	// Fail on a malformed [gateway] maintenance_windows before anything else
	// starts: a schedule typo silently ignored would misclassify every
	// backend-link loss for the whole session.
	if err := s.resolveMaintenanceWindows(); err != nil {
		return err
	}
	lock, err := acquireInstanceLock(s.socketPath)
	if err != nil {
		return err
	}
	s.lock = lock
	// Authority verification (quick_check, foreign keys, a re-hash of every
	// history, so on a large authority it is effectively the whole pre-socket
	authorityStartedAt := time.Now()
	if info, err := os.Stat(s.coreStorePath); err == nil {
		s.logger.Infof("daemon authority: verifying %d MiB before opening the socket", info.Size()>>20)
	}
	if err := s.openCoreStore(ctx); err != nil {
		s.lock.Release()
		s.lock = nil
		return err
	}
	s.logger.Infof("daemon authority: verified in %s", time.Since(authorityStartedAt).Round(time.Millisecond))
	defer func() {
		if err := s.closeCoreStore(); err != nil {
			s.warnf("close daemon authority: %v", err)
		}
	}()

	// Discovery is fast: no probe at all when port+tls are pinned, and
	// the daemon will be talking to. Failure here only happens when the
	// user left port unpinned AND no IBKR ports respond — we still bring
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
	// once the user starts IBKR. When discovery succeeded, the failover
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
	s.logger.Infof("canary daemon %s listening on %s (gateway=%s:%d, clientID=%d)",
		s.version, s.socketPath, ep.Host, ep.Port, ep.ClientID)
	s.evaluateRiskPolicyV3Reconciliation()
	// Skip the connect goroutine when discovery already failed — there's
	// and no attempt in flight, starts reconnectFlow, and races the initial
	if derr == nil {
		s.mu.Lock()
		s.connectInFlight = true
		s.mu.Unlock()
	}
	// The canonical Rulebook refresh may immediately need the gateway. Start
	// all daemon-owned read loops only after the initial connect slot is claimed
	s.startRegimeRefreshLoop(serverCtx)
	s.startRulebookCanonicalRefreshLoop(serverCtx)
	s.startAlertShadowObservationLoops(serverCtx)
	go s.runCoreStoreRecoveryLoop(serverCtx)
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
	s.startStressEvaluationLoop(serverCtx)
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
	s.stopServerContextAndWait()
	// Capture the last minute of contract resolutions before tearing
	// logs, since shutdown shouldn't fail because a disk write
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
		PreferredClientID: ep.ClientID,
		BaseConfig:        conn,
	}
	connector := ibkrlib.NewConnector(cc)
	connector.SetBackendMaintenanceWindows(s.maintenanceWindows)
	connector.SetBackendSessionOpen(anySupportedMarketOpen)
	return connector
}

// anySupportedMarketOpen reports whether any market this system trades is in
// its regular session at t — the union over every marketcal calendar (US
// equities, US options, Xetra today). A backend-link loss during any of them
// is an order-transmission hole worth a per-event warning; there is no
// global quiet hour to special-case, only the union of what we trade.
func anySupportedMarketOpen(t time.Time) bool {
	cal := marketcal.New()
	for _, market := range marketcal.AllMarkets() {
		if s, err := cal.SessionAt(market, t); err == nil && s.IsOpen {
			return true
		}
	}
	return false
}

// resolveMaintenanceWindows parses [gateway] maintenance_windows once for
// the daemon's lifetime. nil config means IBKR's documented North America
// defaults; an explicit empty list disables the classification.
func (s *Server) resolveMaintenanceWindows() error {
	if s.cfg == nil || s.cfg.Gateway.MaintenanceWindows == nil {
		windows, err := ibkrlib.DefaultIBKRMaintenanceWindows()
		if err != nil {
			return fmt.Errorf("default broker maintenance windows: %w", err)
		}
		s.maintenanceWindows = windows
		return nil
	}
	windows, err := ibkrlib.ParseMaintenanceWindows(s.cfg.Gateway.MaintenanceWindows)
	if err != nil {
		return fmt.Errorf("[gateway] maintenance_windows: %w", err)
	}
	s.maintenanceWindows = windows
	return nil
}

// buildAttempter is the production attempter factory. Tests replace
func (s *Server) buildAttempter(ep discover.Endpoint) connectAttempter {
	return s.newConnector(ep)
}

// runConnectAttempt is the single entry point for "do one full connect
// flow (including handshake failover across alternates) and clear
func (s *Server) runConnectAttempt(ctx context.Context, primary discover.Endpoint) {
	defer func() {
		s.mu.Lock()
		s.connectInFlight = false
		s.mu.Unlock()
	}()
	s.connectWithFailover(ctx, primary)
}

// connectWithFailover walks the primary endpoint then each alternate in
// Failover exists because the TCP probe in discover/ is coarse: a port
// can accept connections without its IBKR backend being ready to talk
// When both Gateway and TWS are running locally, discovery's preference
// completes, the daemon used to stay degraded forever even though TWS
// unhealthy primary, each failed candidate adds roughly pkg/ibkr's
func (s *Server) connectWithFailover(ctx context.Context, primary discover.Endpoint) {
	factory := s.attempterFactory
	if factory == nil {
		// Direct &Server{} construction (legacy tests) leaves the field
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
		// production (buildAttempter returns *ibkrlib.Connector); test
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
	// tried so `canary status` shows the user the full picture (not just
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
// mid-failover shows the truth.
// the failover loop can advance even when pkg/ibkr's TLS-handshake
func (s *Server) tryOneHandshake(ctx context.Context, a connectAttempter, ep discover.Endpoint) bool {
	candidateCtx, candidateCancel := context.WithTimeout(ctx, perCandidateConnectBudget)
	defer candidateCancel()

	watchdogCtx, watchdogCancel := context.WithCancel(candidateCtx)
	defer watchdogCancel()
	go s.handshakeWatchdog(watchdogCtx, a.IsConnected, handshakeWatchdogDelay, ep)

	err := a.Start(candidateCtx)
	// Outer (daemon) ctx cancelled → shutdown raced with us; exit silently
	if ctx.Err() != nil {
		return false
	}
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		// Per-candidate budget expired with the SDK still in handshake
		hint := fmt.Sprintf("gateway %s:%d did not handshake within %s; check IB Gateway is running and 'Enable ActiveX and Socket Clients' is on",
			ep.Host, ep.Port, perCandidateConnectBudget)
		s.mu.Lock()
		s.lastConnectError = hint
		s.mu.Unlock()
		s.logger.Warnf("Candidate budget expired: %s", hint)
		return false
	case err != nil && candidateCtx.Err() != nil:
		// SDK returned a wrapped ctx error (e.g. "tls handshake failed:
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
	// one — skip the call entirely since reqPnL requires an account.
	if account != "" {
		if err := a.SubscribeAccountPnL(account); err != nil {
			s.logger.Warnf("SubscribeAccountPnL failed (account.summary will lack daily P&L): %v", err)
		}
	}

	// Load the persisted contract cache and seed the primary connector
	// an IBKR rate-limit-bucket token saved: without persistence, each
	// seeds, draining IBKR's per-account reqContractDetails bucket
	s.seedFromContractStore(s.connector)
	s.registerOrderLifecycleJournal(s.connector)

	// Order-journal broker reconcile: a settle-delayed one-shot on EVERY
	// successful (re)connect — each reconnect is exactly when a terminal
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
	if s.contractStore != nil && s.serverCtx != nil {
		s.contractCacheSaveStarted.Do(func() {
			go s.contractCacheSaveLoop(s.serverCtx)
		})
	}

	// Pre-warm contract-details cache for the regime-dashboard symbols
	// races five parallel goroutines against fresh contract resolution
	s.regimePrewarming.Store(true)
	go s.prewarmRegimeSymbols()

	// Stand up the dedicated bulk-historical IBKR client BEFORE
	// launching breadth.Run(). The scheduler's cold-start bootstrap
	// fan-outs 500 historical-bar fetches and would otherwise read
	// nil from breadthGatewayConnector on every leg until the bulk
	// handshake lands. Synchronous start (12-s ceiling) blocks
	// postConnectSetup briefly; in exchange, breadth.Run() launches
	// with a guaranteed-or-nil view of the bulk connector.
	//
	// Runs on EVERY successful primary handshake, not once per process.
	// A reconnect is the moment the bulk lane most needs rebuilding:
	// whatever killed the primary's socket almost always killed cid=16
	// alongside it. claimBreadthConnect makes the repeat a no-op when
	// the lane is already healthy, which is what the old sync.Once was
	// really there to guarantee.
	//
	// The 12-s ceiling therefore now bounds reconnects too, not just
	// the first connect. It only binds when the second cid cannot be
	// seated at all; a healthy local gateway seats it in about a
	// second, which is what the reconnect path actually costs.
	if s.breadth != nil && s.serverCtx != nil && s.claimBreadthConnect() {
		s.breadthConnectFlow(s.serverCtx, ep)
	}

	// Launch the breadth scheduler now that the gateway has handshaken.
	// fetches via the bulk connector started above and would fail-and-
	// daily-tick wait — so the flag never sticks.
	if s.breadth != nil && s.serverCtx != nil {
		s.breadthStarted.Do(func() {
			s.breadth.MarkPendingBootstrap()
			go s.breadth.Run(s.serverCtx)
		})
	}

	// Launch the runtime members refresher alongside the breadth
	if s.membersRefresher != nil && s.serverCtx != nil {
		s.membersRefresherStarted.Do(func() {
			go s.membersRefresher.Run(s.serverCtx)
		})
	}

	// Prewarm dealer zero-gamma for the first brief/rulebook/app consumer.
	// The Once gates the initial kick; kickOrJoin only acquires the in-flight
	// compute synchronously and does not wait for completion.
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
	// panel recovered 10:59:15). Ordered last so the kicked refresh races
	s.tradeProposals.Kick()
	if s.opportunities != nil {
		s.opportunities.Kick()
	}

	// Latch the postConnectSetup-done barrier. handleStatusHealth gates
	// loop in pkg/ibkr) and the synchronous sentinel-setting above
	s.postConnectSetupDone.Store(true)
	// The evaluator starts before the gateway handshake so daemon startup never
	// depends on an app process. Wake it now that account and portfolio streams
	// coalesced wake after market authority advances.
	s.wakeStressEvaluation()
}

// prewarmZeroGamma kicks the first dealer zero-gamma compute of a
// race against immediate disconnect shouldn't crash startup. The
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
	compute := func(bgCtx context.Context, prog *atomic.Int32) (*rpc.GammaZeroComputed, error) {
		return computeGammaCombined(bgCtx, s, c, params, prog)
	}
	if _, fresh := s.zeroGamma.kickOrJoin(ctx, rpc.GammaZeroScopeCombined, time.Now(), computeETA, compute); fresh {
		s.logger.Infof("gamma %s: kicked compute (scope=combined)", caller)
	}
}

// regimeSymbolSeed is the static fallback used by prewarmRegimeSymbols
// when the gateway's reqContractDetails is unresponsive (per-account
// silence). These conIDs identify the spot instruments by their IBKR
// the seed is purely a fallback for the broken-gateway case. If IBKR
// the only symptom is the regime row's historical-derived field going
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
// handshakeWaitBudget (25s) so a user moving IBKR from Gateway to TWS still
const (
	reconnectBackoffBase = time.Second
	reconnectBackoffMax  = 15 * time.Second
)

// reconnectBackoff converts a consecutive-failed-reconnect count into the
// outage, observed 2026-07-08, each emitting an identical connect-failure
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
// failure streak. streak 0 is always allowed so a fresh gateway drop
// slow handshake already counts as its own quiet period. Caller must hold
func (s *Server) reconnectAllowed(now time.Time) bool {
	if s.reconnectFailStreak == 0 {
		return true
	}
	return now.Sub(s.lastReconnectAttemptAt) >= reconnectBackoff(s.reconnectFailStreak)
}

// triggerReconnect launches a rediscover+reconnect attempt in a
// The motivation is the AUTO discovery flow: discovery only ran once at
// daemon startup, and after a failed handshake the endpoint stayed pinned
// (4001) and starts TWS (7496) instead, the daemon would render
// for a verdict — exactly the same UX as the initial connect path.
func (s *Server) triggerReconnect() bool {
	s.mu.Lock()
	if s.connectInFlight {
		s.mu.Unlock()
		return false
	}
	// IsReady, not IsConnected: TCP-up is not enough. If the connector
	// during a transient gateway hiccup), we must re-establish — and the
	// only way out is to tear the old TCP socket down in reconnectFlow.
	if s.connector != nil && s.connector.IsReady() {
		s.mu.Unlock()
		return false
	}
	if s.serverCtx == nil || s.serverCtx.Err() != nil {
		// Server.Start hasn't run yet, the daemon has already begun
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
// probe winner produces, and runs the handshake. Caller must have
// Tearing down the old connector is required: pkg/ibkr's pool can't be
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
// this — taking the snapshot under mu prevents the race where a handler
// The current handler still returns ErrIBKRUnavailable to its caller —
// pinned to a stale port after the user moved between Gateway and TWS.
func (s *Server) gatewayConnector() *ibkrlib.Connector {
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	// IsReady, not IsConnected: handlers also need to be armed. A connector
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
// without an out-of-band resync token, which the protocol does not have).
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
	defer recoverHandler(s.logger, enc, req)
	// Per-request deadline. Streaming methods get no deadline (the stream
	if _, ok := rpc.LookupMethodTiming(req.Method); !ok {
		writeError(enc, req.ID, rpc.CodeUnknownMethod, "unknown method: "+req.Method)
		return false
	}
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
	case rpc.MethodRulesHistory:
		s.unary(req, enc, func() (any, error) { return s.handleRulesHistory(ctx, req) })
	case rpc.MethodReconEquity:
		s.unary(req, enc, func() (any, error) { return s.handleReconEquity(ctx, req) })
	case rpc.MethodMarketEventsSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleMarketEventsSnapshot(ctx, req) })
	case rpc.MethodStatusHealth:
		s.unary(req, enc, func() (any, error) { return s.handleStatusHealth(), nil })
	case rpc.MethodTradingStatus:
		s.unary(req, enc, func() (any, error) { return s.handleTradingStatus(), nil })
	case rpc.MethodAutoTradeStatus:
		s.unary(req, enc, func() (any, error) { return s.handleAutoTradeStatus(), nil })
	case rpc.MethodRulesSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleRulesSnapshot(ctx, req) })
	case rpc.MethodBriefSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleBriefSnapshot(ctx, req) })
	case rpc.MethodNudgesSnapshot:
		s.unary(req, enc, func() (any, error) { return s.handleNudgesSnapshot(ctx, req) })
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
	case rpc.MethodTradeProposalsRequestStop:
		s.unary(req, enc, func() (any, error) { return s.handleTradeProposalsRequestStop(ctx, req) })
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
	case rpc.MethodStrategyPreview:
		s.unary(req, enc, func() (any, error) { return s.handleStrategyPreview(ctx, req) })
	case rpc.MethodQuoteSubscribe:
		s.handleQuoteSubscribe(ctx, req, enc, r)
		return true
	case rpc.MethodOrderPlace:
		s.unary(req, enc, func() (any, error) { return s.handleOrderPlace(ctx, req) })
	case rpc.MethodOrderModify:
		s.unary(req, enc, func() (any, error) { return s.handleOrderModify(ctx, req) })
	case rpc.MethodOrderCancel:
		s.unary(req, enc, func() (any, error) { return s.handleOrderCancel(ctx, req) })
	default:
		writeError(enc, req.ID, rpc.CodeUnknownMethod, "unknown method: "+req.Method)
	}
	return false
}

// requestCtx returns a derived context with the per-method unary
func requestCtx(parent context.Context, method string) (context.Context, context.CancelFunc) {
	if d := unaryDeadline(method); d > 0 {
		return context.WithTimeout(parent, d)
	}
	return parent, func() {}
}

// unaryDeadline reads the shared RPC timing authority. Adapters add their own
func unaryDeadline(method string) time.Duration {
	timing, ok := rpc.LookupMethodTiming(method)
	if !ok || timing.Lifetime == rpc.MethodLifetimeStreaming {
		return 0
	}
	return timing.DaemonTimeout
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
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrSymbolInactive):
		return rpc.CodeSymbolInactive, err.Error()
	case errors.Is(err, ibkrlib.ErrContractNoDefinition):
		// The broker answered that no such contract exists. Same caller
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
// handleStatusHealth (wire-emitted BackgroundTasks list), and the
// IBKR's contract-details bucket refills, and gamma compute runs
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
	if scoped, total, err := s.openBrokerOrderCounts(); err != nil && !errors.Is(err, ErrTradingDisabled) {
		// A configured journal that cannot be read is unknown order
		// authority, not zero: broker-side protective orders may still be
		// is broken — is the visible failure; going dark on fills would be
		// (ErrTradingDisabled) has no broker-write capability to go dark
		tasks = append(tasks, rpc.BackgroundTaskStatus{Name: "open-orders", Status: "journal unreadable; count unknown, idle exit deferred"})
	} else if err == nil && total > 0 {
		// A daemon that idle-exits while protective stops are working goes
		// dark on fills, cancels, and the order journal exactly when they
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
// all scopes (what idle shutdown must respect). A journal read error is
// returned as such — never flattened to zero counts.
func (s *Server) openBrokerOrderCounts() (scoped, total int, err error) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return 0, 0, err
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
	return scoped, total, nil
}

// isBusy reports whether the daemon has daemon-internal background work
// the idle watcher and the status surface never disagree about what's
func (s *Server) isBusy() bool {
	return len(s.backgroundTasks()) > 0
}
