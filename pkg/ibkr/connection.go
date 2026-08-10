package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"math/rand"
	"net"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/osauer/canary/v2/pkg/ibkr/internal/logging"
)

// ConnectionStatus identifies the lifecycle state of a [Connection].
type ConnectionStatus int

const (
	// StatusDisconnected means no protocol session is established.
	StatusDisconnected ConnectionStatus = iota
	// StatusConnecting means a connection or handshake is in progress.
	StatusConnecting
	// StatusConnected means the protocol session is ready.
	StatusConnected
	// StatusReconnecting means recovery of a lost session is in progress.
	StatusReconnecting
	// StatusFailed means the most recent connection attempt failed.
	StatusFailed
)

var (
	errStartAPIFailed  = errors.New("ibkr start api failure")
	errHandshakeNoData = errors.New("ibkr handshake: no response")
	// errClientIDInUse marks the IBKR code-326 client-ID collision.
	errClientIDInUse = errors.New("IBKR: client ID already in use")
	ibkrLogger       = logging.Component("IBKR")
	connectLogger    = logging.Component("IBKR Connect")
	wireLogger       = logging.Component("IBKR Wire")
	handshakeLogger  = logging.Component("IBKR Handshake")
	portfolioLogger  = logging.Component("IBKR Portfolio")
	marketLogger     = logging.Component("IBKR MarketData")
)

func clientIDInUseError(clientID int, gatewayMsg string) error {
	msg := fmt.Sprintf("gateway client ID %d is already in use; stop the stale IBKR API client or choose a free [gateway].client_id", clientID)
	if strings.TrimSpace(gatewayMsg) != "" {
		msg += ": " + strings.TrimSpace(gatewayMsg)
	}
	return fmt.Errorf("%w: %s", errClientIDInUse, msg)
}

// String returns the uppercase name of s, or "UNKNOWN" for an unrecognized value.
func (s ConnectionStatus) String() string {
	switch s {
	case StatusDisconnected:
		return "DISCONNECTED"
	case StatusConnecting:
		return "CONNECTING"
	case StatusConnected:
		return "CONNECTED"
	case StatusReconnecting:
		return "RECONNECTING"
	case StatusFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

// ConnectionConfig configures one TWS or IB Gateway protocol session.
type ConnectionConfig struct {
	Host     string
	Port     int
	ClientID int
	Account  string

	// PacketLogPath enables sensitive packet logging; %d expands to the client ID.
	PacketLogPath string
	LogWireHex    bool // LogWireHex emits account-sensitive raw protocol frames.

	// WireInterceptor records frames; nil uses the environment configuration.
	WireInterceptor *WireInterceptor

	// startAPI retry settings for the configured client ID.
	MaxClientIDRetries int // Max attempts for transient startAPI failures (default 5)

	// Reconnection settings (from hedge patterns)
	AutoReconnect     bool
	MaxRetries        int
	InitialDelay      time.Duration // Initial reconnect delay (5s)
	MaxDelay          time.Duration // Max reconnect delay (60s)
	BackoffMultiplier float64       // Exponential backoff multiplier (2.0)
	Jitter            bool          // Add random jitter to delays

	// Connection timeouts
	ConnectTimeout    time.Duration
	HeartbeatInterval time.Duration

	// TLS options
	UseTLS                bool
	EnableTLSFallback     bool
	TLSInsecureSkipVerify bool
	TLSServerName         string
}

// RawPosition contains the latest broker-reported values for one position.
type RawPosition struct {
	Account       string
	Contract      Contract
	Position      float64
	MarketPrice   float64
	MarketValue   float64
	AverageCost   float64
	UnrealizedPNL float64
	RealizedPNL   float64
}

// Contract identifies an instrument in TWS wire requests.
type Contract struct {
	ConID        int
	Symbol       string
	SecType      string  // STK, OPT, FUT, etc.
	Expiry       string  // For options/futures
	Strike       float64 // For options
	Right        string  // P or C for options
	Multiplier   int
	Exchange     string
	PrimaryExch  string // Primary exchange for routing
	Currency     string
	LocalSymbol  string
	TradingClass string
	SecIDType    string
	SecID        string
	ComboLegs    []ComboLeg
}

// ComboLeg is one exact contract inside an IBKR BAG contract. Ratio is a
// positive integer; Action describes the BAG's held direction. A parent SELL
// order reverses these actions to close or reduce the strategy.
type ComboLeg struct {
	ConID     int
	Ratio     int
	Action    string
	Exchange  string
	OpenClose int
}

// DefaultConfig returns a new connection configuration with package defaults.
func DefaultConfig() *ConnectionConfig {
	return &ConnectionConfig{
		Host:                  "127.0.0.1",
		Port:                  4001, // IB Gateway port
		ClientID:              1,
		MaxClientIDRetries:    5,
		AutoReconnect:         true,
		MaxRetries:            10,
		InitialDelay:          5 * time.Second,
		MaxDelay:              60 * time.Second,
		BackoffMultiplier:     2.0,
		Jitter:                true,
		ConnectTimeout:        10 * time.Second,
		HeartbeatInterval:     30 * time.Second,
		UseTLS:                false,
		EnableTLSFallback:     true,
		TLSInsecureSkipVerify: true,
		TLSServerName:         "",
		LogWireHex:            false,
	}
}

func lookupEnvBool(key string) (bool, bool) {
	if val, ok := os.LookupEnv(key); ok {
		s := strings.TrimSpace(strings.ToLower(val))
		switch s {
		case "1", "true", "yes", "on":
			return true, true
		case "0", "false", "no", "off", "":
			return false, true
		default:
			return false, true
		}
	}
	return false, false
}

func (c *Connection) tlsAttempts() []bool {
	if c == nil || c.config == nil {
		return []bool{false}
	}
	base := c.config.UseTLS
	seq := []bool{base}
	if c.config.EnableTLSFallback {
		seq = append(seq, !base)
	}
	return seq
}

// Connection owns one TWS protocol session and its request and receipt state.
type Connection struct {
	config   *ConnectionConfig
	status   ConnectionStatus
	statusMu sync.RWMutex

	// Connection state
	connectedAt time.Time
	// lastHeartbeatNano holds the most recent heartbeat time as Unix
	lastHeartbeatNano atomic.Int64
	errorCount        int
	lastError         error

	// Reconnection control
	reconnectChan chan struct{}
	stopChan      chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup

	// TCP connection
	conn    net.Conn
	reader  *bufio.Reader
	writer  *bufio.Writer
	scanner *bufio.Scanner

	// Protocol state
	serverVersion      int
	connTime           string
	reqIDSeq           int
	reqIDMu            sync.Mutex
	nextOrderID        int
	haveNextValidID    bool
	brokerIDExhausted  bool
	reservedOrderIDs   map[int]struct{}
	reservedRequestIDs map[int]struct{}
	brokerSessionEpoch atomic.Uint64
	// inboundEpochMu keeps late receipts attributable after outbound invalidation.
	inboundEpochMu sync.RWMutex
	// account holds the raw msgManagedAccts value, which is a
	// comma-separated list for a login that carries several accounts.
	// managedAccounts is that value split into concrete codes. Guarded by
	// accountMu.
	account         string
	managedAccounts []string
	handshakeMu     sync.RWMutex
	handshakeReady  chan struct{}
	useTLS          bool

	// Outbound sequencing to guarantee single-writer semantics per client ID
	transportMu     sync.Mutex
	transportCond   *sync.Cond
	transportPaused bool
	// outboundSessionState is revoked at disconnect while brokerSessionEpoch and
	// the inbound epoch remain available to attribute late decoded receipts.
	outboundSessionState atomic.Uint64
	// evidenceBarrier is installed by Connector and shared with receipt publication.
	evidenceBarrier *sync.RWMutex

	packetLogger       PacketLogger
	packetLoggerMu     sync.RWMutex
	packetLoggerCloser func() error

	wireTap *WireInterceptor

	// Order tracking (scaffold for tests and local state)
	ordersMu    sync.RWMutex
	openOrders  map[int]*IBKROrder
	orderStatus map[int]string

	// Request aliasing (reqID -> metadata) for logging/system notices
	aliasMu  sync.RWMutex
	reqAlias map[int]reqAliasEntry

	logWireHex bool

	// Ensure write path runs serially
	writeMu         sync.Mutex
	writeInProgress atomic.Bool
	// publicationBarrier keeps protected sends inside one account publication.
	publicationBarrier *sync.RWMutex

	// Guard against repeated suspicious logs per symbol/payload.
	suspectMu        sync.Mutex
	suspectFlags     map[string]struct{}
	suspectSummaries map[string]string
	contractTimingMu sync.Mutex
	contractTimings  map[string]time.Duration

	// Read loop coordination so outbound requests wait until reader is ready
	readStartMu sync.Mutex
	readStartCh chan struct{}

	// Callbacks for status changes
	onConnect    func()
	onDisconnect func(error)

	// Message handling
	msgHandlers         map[int][]handlerEntry
	handlersMu          sync.RWMutex
	handlerSeq          uint64
	whatIfOrdersMu      sync.Mutex
	whatIfOrderIDs      map[int]struct{}
	openOrderObserverMu sync.RWMutex
	openOrderObserver   func(msgID int, fields []string, epoch uint64)

	// Market data type per reqID (1=RealTime,2=Frozen,3=Delayed,4=DelayedFrozen)
	mktDataType   map[int]int
	mktDataTypeMu sync.RWMutex

	optionContractMu    sync.RWMutex
	optionContractCache map[string]ContractDetailsLite

	systemNoticeMu      sync.RWMutex
	systemNoticeHandler func(note *systemNotification, alias reqAliasEntry, epoch uint64) func()
	errorPostActionMu   sync.RWMutex
	errorPostAction     func(fields []string, epoch uint64) func()

	// Competing live session detection (error 10197)
	competingMu          sync.RWMutex
	competingLiveSession bool

	// Portfolio data storage
	positions   map[string]*RawPosition
	positionsMu sync.RWMutex
	// portfolioStaging is the current reqAccountUpdates initial generation.
	// Published positions remain visible only with incomplete health until the
	// matching accountDownloadEnd atomically replaces them.
	portfolioStaging       map[string]*RawPosition
	portfolioStagingActive bool
	// positionsSnapshot is isolated reqPositions state. It must never mutate
	positionsSnapshot       map[string]*RawPosition
	positionsSnapshotResult map[string]*RawPosition
	positionsSnapshotActive bool
	// portfolioProjectionMu makes cached rows and their receipt health atomic.
	portfolioProjectionMu sync.RWMutex
	portfolioHealthMu     sync.RWMutex
	portfolioHealth       PortfolioStreamHealth
	accountSummary        map[string]string
	// summarySnapshots accumulates account-summary rows per reqID so a
	// synchronous reqAccountSummary read cannot be clobbered by the
	// streaming reqAccountUpdates subscription, which writes the shared
	// accountSummary map (issue #12). Guarded by accountMu.
	summarySnapshots map[int]*summarySnapshot
	accountMu        sync.RWMutex

	// Completion signals for async operations
	positionsEndChan   chan struct{} // Signals when position sync is complete
	acctSummaryEndChan chan struct{} // Signals when account summary is complete

	// Rate limiting
	rateLimiter *RateLimiter
	ctx         context.Context
	cancel      context.CancelFunc

	// Tracks which reqIDs currently hold a market data slot. The error
	marketDataSlotsMu sync.Mutex
	marketDataSlots   map[int]uint64

	// Start API failure tracking for adaptive backoff
	startAPIMu          sync.Mutex
	startAPIFailures    int
	lastStartAPIFailure time.Time

	// errorMessageAfterInitialEpochCheck is a deterministic receipt-race test seam.
	errorMessageAfterInitialEpochCheck func()
	// systemNoticeAfterInitialEpochCheck is the msg-204 equivalent; production leaves it nil.
	systemNoticeAfterInitialEpochCheck func()
	// messageAfterInitialEpochCheck covers ordinary authority frames.
	messageAfterInitialEpochCheck func(msgID int)
}

type handlerEntry struct {
	id        uint64
	fn        func([]string)
	fnAtEpoch func([]string, uint64)
}

// PortfolioStreamHealth is receipt metadata for the streaming
// reqAccountUpdates portfolio cache. It contains no positions or balances;
// callers use it only to decide whether a cached projection is current enough
type PortfolioStreamHealth struct {
	Account            string
	RequestedAt        time.Time
	InitialCompletedAt time.Time
	LastUpdateAt       time.Time
	// ProjectionGeneration advances only when the structural portfolio
	// authority changes: scope/completeness/invalidity, contract set, or held
	ProjectionGeneration uint64
	// ScopeConflictAt is set when the stream emits a portfolio or completion
	// frame for a blank or foreign account. Rows are retained as context, but
	// no receipt is trustworthy until reqAccountUpdates is resubscribed.
	ScopeConflictAt time.Time
	// InvalidPayloadAt is set when a portfolio generation contains a malformed
	InvalidPayloadAt time.Time
}

type reqAliasEntry struct {
	symbol       string
	secType      string
	exchange     string
	primaryExch  string
	currency     string
	localSymbol  string
	tradingClass string
}

func (c *Connection) registerReqAlias(reqID int, contract Contract) {
	if reqID <= 0 || contract.Symbol == "" {
		return
	}
	entry := reqAliasEntry{
		symbol:       strings.ToUpper(contract.Symbol),
		secType:      strings.ToUpper(contract.SecType),
		exchange:     strings.ToUpper(contract.Exchange),
		primaryExch:  strings.ToUpper(contract.PrimaryExch),
		currency:     strings.ToUpper(contract.Currency),
		localSymbol:  contract.LocalSymbol,
		tradingClass: contract.TradingClass,
	}
	c.aliasMu.Lock()
	c.reqAlias[reqID] = entry
	c.aliasMu.Unlock()
}

func (c *Connection) lookupReqAlias(reqID int) (reqAliasEntry, bool) {
	if reqID <= 0 {
		return reqAliasEntry{}, false
	}
	c.aliasMu.RLock()
	alias, ok := c.reqAlias[reqID]
	c.aliasMu.RUnlock()
	return alias, ok
}

// SetSystemNoticeHandler is retained for v2 source compatibility. Its private
// parameter types mean callers outside this package can only clear the handler.
func (c *Connection) SetSystemNoticeHandler(handler func(note *systemNotification, alias reqAliasEntry)) {
	if handler == nil {
		c.SetSystemNoticeHandlerAtEpoch(nil)
		return
	}
	c.SetSystemNoticeHandlerAtEpoch(func(note *systemNotification, alias reqAliasEntry, _ uint64) {
		handler(note, alias)
	})
}

// SetSystemNoticeHandlerAtEpoch is retained for v2 source compatibility. Its
// private parameter types mean callers outside this package can only clear it.
func (c *Connection) SetSystemNoticeHandlerAtEpoch(handler func(note *systemNotification, alias reqAliasEntry, epoch uint64)) {
	if handler == nil {
		c.SetSystemNoticeHandlerAtEpochWithPostAction(nil)
		return
	}
	c.SetSystemNoticeHandlerAtEpochWithPostAction(func(note *systemNotification, alias reqAliasEntry, epoch uint64) func() {
		handler(note, alias, epoch)
		return nil
	})
}

// SetSystemNoticeHandlerAtEpochWithPostAction installs the Connector-owned
// callback. Its action runs after the inbound lock is released, avoiding a
// reconnect deadlock. External callers can only pass nil because its parameter
// types are private; the export remains for v2 source compatibility.
func (c *Connection) SetSystemNoticeHandlerAtEpochWithPostAction(handler func(note *systemNotification, alias reqAliasEntry, epoch uint64) func()) {
	c.systemNoticeMu.Lock()
	c.systemNoticeHandler = handler
	c.systemNoticeMu.Unlock()
}

func (c *Connection) dispatchSystemNotice(note *systemNotification, alias reqAliasEntry, epochs ...uint64) func() {
	epoch := c.BrokerSessionEpoch()
	if len(epochs) > 0 {
		epoch = epochs[0]
	}
	c.systemNoticeMu.RLock()
	handler := c.systemNoticeHandler
	c.systemNoticeMu.RUnlock()
	if handler != nil {
		return handler(note, alias, epoch)
	}
	return nil
}

// setErrorPostActionHandler installs the Connector-owned msgErr handler whose
// returned action may perform outbound recovery only after the inbound socket-
func (c *Connection) setErrorPostActionHandler(handler func(fields []string, epoch uint64) func()) {
	c.errorPostActionMu.Lock()
	c.errorPostAction = handler
	c.errorPostActionMu.Unlock()
}

func (c *Connection) dispatchErrorPostAction(fields []string, epoch uint64) func() {
	c.errorPostActionMu.RLock()
	handler := c.errorPostAction
	c.errorPostActionMu.RUnlock()
	if handler == nil {
		return nil
	}
	return handler(fields, epoch)
}

func (c *Connection) resetReadStartCh() {
	c.readStartMu.Lock()
	c.readStartCh = make(chan struct{})
	c.readStartMu.Unlock()
}

func (c *Connection) signalReadStarted() {
	c.readStartMu.Lock()
	ch := c.readStartCh
	c.readStartMu.Unlock()
	if ch != nil {
		select {
		case <-ch:
			// already closed
		default:
			close(ch)
		}
	}
}

func (c *Connection) waitForReadStart(timeout time.Duration) {
	c.readStartMu.Lock()
	ch := c.readStartCh
	c.readStartMu.Unlock()
	if ch == nil {
		return
	}
	if timeout <= 0 {
		<-ch
		return
	}
	select {
	case <-ch:
	case <-time.After(timeout):
		connectLogger.Warnf("Client %d: read loop start wait timed out after %s", c.config.ClientID, timeout)
	}
}

// NewConnection constructs a disconnected session; nil uses [DefaultConfig].
func NewConnection(config *ConnectionConfig) *Connection {
	if config == nil {
		config = DefaultConfig()
	} else {
		configCopy := *config
		config = &configCopy

		// Fill in missing timeouts/intervals with safe defaults to avoid zero-value panics
		def := DefaultConfig()
		if config.ConnectTimeout == 0 {
			config.ConnectTimeout = def.ConnectTimeout
		}
		if config.HeartbeatInterval == 0 {
			config.HeartbeatInterval = def.HeartbeatInterval
		}
		if config.MaxClientIDRetries == 0 {
			config.MaxClientIDRetries = def.MaxClientIDRetries
		}
		if config.MaxRetries == 0 {
			config.MaxRetries = def.MaxRetries
		}
		if config.InitialDelay == 0 {
			config.InitialDelay = def.InitialDelay
		}
		if config.MaxDelay == 0 {
			config.MaxDelay = def.MaxDelay
		}
		if config.BackoffMultiplier == 0 {
			config.BackoffMultiplier = def.BackoffMultiplier
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	conn := &Connection{
		config:              config,
		status:              StatusDisconnected,
		reconnectChan:       make(chan struct{}, 1),
		stopChan:            make(chan struct{}),
		msgHandlers:         make(map[int][]handlerEntry),
		mktDataType:         make(map[int]int),
		positions:           make(map[string]*RawPosition),
		accountSummary:      make(map[string]string),
		summarySnapshots:    make(map[int]*summarySnapshot),
		reqIDSeq:            1,
		reservedOrderIDs:    make(map[int]struct{}),
		reservedRequestIDs:  make(map[int]struct{}),
		openOrders:          make(map[int]*IBKROrder),
		orderStatus:         make(map[int]string),
		reqAlias:            make(map[int]reqAliasEntry),
		logWireHex:          config.LogWireHex,
		suspectFlags:        make(map[string]struct{}),
		suspectSummaries:    make(map[string]string),
		contractTimings:     make(map[string]time.Duration),
		optionContractCache: make(map[string]ContractDetailsLite),
		readStartCh:         make(chan struct{}),
		ctx:                 ctx,
		cancel:              cancel,
		rateLimiter:         NewRateLimiter(ctx),
		marketDataSlots:     make(map[int]uint64),
		positionsEndChan:    make(chan struct{}, 1),
		acctSummaryEndChan:  make(chan struct{}, 1),
		serverVersion:       0,
		useTLS:              config.UseTLS,
	}

	// Use shared wire interceptor if provided, otherwise create per-connection (legacy)
	if config.WireInterceptor != nil {
		conn.wireTap = config.WireInterceptor
	} else if interceptor, err := NewWireInterceptorFromEnv(config.ClientID); err != nil {
		wireLogger.Warnf("Client %d: failed to initialize wire interceptor: %v", config.ClientID, err)
		ibkrLogger.Warnf("[WIRE] Client %d: failed to initialize wire interceptor: %v", config.ClientID, err)
	} else {
		conn.wireTap = interceptor
	}

	conn.transportCond = sync.NewCond(&conn.transportMu)
	conn.resetHandshakeReady()

	if config.PacketLogPath != "" {
		path := config.PacketLogPath
		if strings.Contains(path, "%d") {
			path = fmt.Sprintf(path, config.ClientID)
		}
		if logger, err := NewHexPacketLogger(path); err != nil {
			ibkrLogger.Warnf("Client %d: failed to initialize packet logger: %v", config.ClientID, err)
		} else {
			conn.SetPacketLogger(logger)
			config.PacketLogPath = path
		}
	}

	return conn
}

func (c *Connection) dialEndpoint(ctx context.Context, useTLS bool) (net.Conn, error) {
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	dialer := net.Dialer{Timeout: c.config.ConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to IBKR at %s: %w", addr, err)
	}

	// Disable Nagle's algorithm so buffered protocol frames transmit immediately.
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetNoDelay(true); err != nil {
			conn.Close()
			return nil, fmt.Errorf("failed to set TCP_NODELAY: %w", err)
		}
	}

	if !useTLS {
		return conn, nil
	}
	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.config.TLSInsecureSkipVerify,
	}
	serverName := c.config.TLSServerName
	if serverName == "" && !c.config.TLSInsecureSkipVerify {
		serverName = c.config.Host
	}
	if serverName != "" {
		tlsCfg.ServerName = serverName
	}
	tlsConn := tls.Client(conn, tlsCfg)
	// HandshakeContext bounds a server that accepts TCP but never completes TLS.
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("tls handshake failed: %w", err)
	}
	return tlsConn, nil
}

// closeConnection drops transport fields after the reader goroutine has exited.
func (c *Connection) closeConnection() {
	c.closeSocket()
	c.conn = nil
	c.reader = nil
	c.writer = nil
	c.scanner = nil
}

// closeSocket unblocks a reader without clearing fields it may still access.
func (c *Connection) closeSocket() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

// SetPacketLogger installs a packet logger invoked for every outbound frame.
// Passing nil disables logging. Frames may contain account IDs, order
// references, and order details; callers must protect the sink as sensitive
// data and use it only for short-lived debugging.
func (c *Connection) SetPacketLogger(logger PacketLogger) {
	c.packetLoggerMu.Lock()
	if c.packetLoggerCloser != nil {
		if err := c.packetLoggerCloser(); err != nil {
			ibkrLogger.Warnf("packet logger close error: %v", err)
		}
		c.packetLoggerCloser = nil
	}
	c.packetLogger = logger
	if logger != nil {
		if closer, ok := logger.(interface{ Close() error }); ok {
			c.packetLoggerCloser = closer.Close
		}
	}
	c.packetLoggerMu.Unlock()
}

func (c *Connection) ensurePacketLogger() {
	if c.config == nil || c.config.PacketLogPath == "" {
		return
	}
	c.packetLoggerMu.RLock()
	loggerPresent := c.packetLogger != nil
	c.packetLoggerMu.RUnlock()
	if loggerPresent {
		return
	}
	logger, err := NewHexPacketLogger(c.config.PacketLogPath)
	if err != nil {
		ibkrLogger.Warnf("Client %d: unable to open packet logger: %v", c.config.ClientID, err)
		return
	}
	c.SetPacketLogger(logger)
}

// isClientIDInUseError reports whether err wraps the client-ID collision sentinel.
func isClientIDInUseError(err error) bool {
	return errors.Is(err, errClientIDInUse)
}

// Connect establishes and handshakes the configured TWS or Gateway session.
func (c *Connection) Connect(ctx context.Context) error {
	clientID := c.config.ClientID

	// Limit to 5 retries max. Retries are for transient startAPI/handshake
	// failures on the same configured ID; a 326 collision is terminal because
	// neighboring IDs are reserved for other ibkr lanes.
	maxRetries := max(min(c.config.MaxClientIDRetries, 5), 1)

	// Attempt narration stays at debug so an unavailable gateway does not flood logs.
	connectLogger.Debugf("Starting connection process with Client ID %d, MaxRetries=%d",
		clientID, maxRetries)

	for attempt := range maxRetries {
		c.config.ClientID = clientID
		connectLogger.Debugf("Attempting connection with Client ID %d (attempt %d/%d)",
			clientID, attempt+1, maxRetries)

		err := c.connectWithClientID(ctx)
		if err == nil {
			return nil
		}

		if errors.Is(err, errClientIDInUse) {
			connectLogger.Errorf("Client ID %d already in use; not auto-walking to another reserved client ID", clientID)
			return err
		}

		if errors.Is(err, errStartAPIFailed) {
			connectLogger.Warnf("startAPI failed for Client ID %d; retrying", clientID)
			continue
		}

		// Non-client ID error (dial refused / TLS handshake / net error) —
		// return immediately. Logs at Debug, not Error: the daemon owns the
		// deduped connect verdict (see the connect-narration note above and
		// server.connectWithFailover), so at Error this single line floods
		// ibkr-daemon.log on every demand-driven reconnect cycle while the
		// gateway is down — 66,900 identical "connection refused" lines over a
		// 7h outage, observed 2026-07-08. The real 326 collision above stays at
		// Error; the daemon paces the retries themselves via reconnectBackoff.
		connectLogger.Debugf("Connection failed with non-client ID error: %v", err)
		return err
	}

	return fmt.Errorf("failed to connect after %d attempts with client ID %d", maxRetries, clientID)
}

// connectWithClientID attempts connection with specific client ID
func (c *Connection) connectWithClientID(ctx context.Context) error {
	c.setStatus(StatusConnecting)
	c.ensurePacketLogger()

	attempts := c.tlsAttempts()
	var lastErr error

	for idx, useTLS := range attempts {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if idx > 0 {
			connectLogger.Warnf("Client %d: retrying with tls=%v after error: %v", c.config.ClientID, useTLS, lastErr)
		}

		outboundEpoch := c.beginOutboundSession()
		c.resetHandshakeReady()
		c.resetOrderIDReadiness()

		if err := c.connectAttempt(ctx, useTLS, outboundEpoch); err != nil {
			lastErr = err
			c.closeConnection()
			c.resumeTransport()
			if ctx != nil {
				if cerr := ctx.Err(); cerr != nil {
					return cerr
				}
			}
			if errors.Is(err, errHandshakeNoData) && idx+1 < len(attempts) {
				connectLogger.Warnf("Client %d: handshake returned no data (tls=%v); attempting fallback", c.config.ClientID, useTLS)
				continue
			}
			return err
		}

		return nil
	}

	if lastErr != nil {
		return lastErr
	}

	return fmt.Errorf("failed to connect to IBKR (client %d)", c.config.ClientID)
}

func (c *Connection) connectAttempt(ctx context.Context, useTLS bool, outboundEpoch uint64) error {
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	connectLogger.Debugf("Client %d: Connecting to %s (tls=%v)...", c.config.ClientID, addr, useTLS)

	netConn, err := c.dialEndpoint(ctx, useTLS)
	if err != nil {
		c.setStatus(StatusDisconnected)
		return err
	}

	var cancelOnce sync.Once
	var cancelWatcherDone chan struct{}
	if ctx != nil {
		if cerr := ctx.Err(); cerr != nil {
			_ = netConn.Close()
			return cerr
		}
		cancelWatcherDone = make(chan struct{})
		go func(conn net.Conn, done <-chan struct{}, watchCtx context.Context) {
			select {
			case <-watchCtx.Done():
				cancelOnce.Do(func() { _ = conn.Close() })
			case <-done:
			}
		}(netConn, cancelWatcherDone, ctx)
	}
	defer func() {
		if cancelWatcherDone != nil {
			close(cancelWatcherDone)
		}
	}()

	c.conn = netConn
	c.reader = bufio.NewReader(netConn)
	c.writer = bufio.NewWriter(netConn)

	c.scanner = bufio.NewScanner(netConn)
	c.scanner.Split(c.scanMessages)
	c.scanner.Buffer(make([]byte, 4096), 1024*1024) // 1MB max message

	connectLogger.Infof("Client %d: Starting handshake...", c.config.ClientID)
	if err := c.handshake(); err != nil {
		connectLogger.Errorf("Client %d: Handshake failed: %v", c.config.ClientID, err)
		c.setStatus(StatusDisconnected)
		cancelOnce.Do(func() { _ = netConn.Close() })
		return fmt.Errorf("handshake failed: %w", err)
	}
	connectLogger.Infof("Client %d: Handshake successful (serverVersion=%d)", c.config.ClientID, c.serverVersion)

	connectLogger.Infof("Client %d: Starting API...", c.config.ClientID)
	if err := c.startAPI(); err != nil {
		if isClientIDInUseError(err) {
			connectLogger.Warnf("Client %d: startAPI rejected client ID in use: %v", c.config.ClientID, err)
			c.setStatus(StatusDisconnected)
			return err
		}
		delay := c.registerStartAPIFailure()
		connectLogger.Warnf("Client %d: Failed to start API: %v (backing off %s)", c.config.ClientID, err, delay)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("%w: context cancelled during startAPI backoff: %w", errStartAPIFailed, ctx.Err())
			}
		}
		return fmt.Errorf("%w: %v", errStartAPIFailed, err)
	}
	connectLogger.Infof("Client %d: API started successfully", c.config.ClientID)
	c.resetStartAPIFailure()
	c.lastHeartbeatNano.Store(time.Now().UnixNano())
	c.statusMu.Lock()
	c.status = StatusConnected
	c.connectedAt = time.Now()
	c.errorCount = 0
	c.lastError = nil
	c.statusMu.Unlock()
	if !c.activateOutboundSession(outboundEpoch) {
		c.setStatus(StatusDisconnected)
		return fmt.Errorf("connection invalidated before outbound session publication")
	}

	c.useTLS = useTLS

	connectLogger.Infof("Connection established (Client ID: %d, Server Version: %d, tls=%v)", c.config.ClientID, c.serverVersion, c.useTLS)

	c.resetReadStartCh()
	c.wg.Add(2)
	go c.heartbeatMonitor()
	go c.readMessages()
	c.waitForReadStart(500 * time.Millisecond)
	c.signalHandshakeReady()

	if c.onConnect != nil {
		c.onConnect()
	}

	return nil
}

// Disconnect closes the protocol session and stops its background work.
func (c *Connection) Disconnect() error {
	// Invalidate queued protected sends before publishing the disconnect.
	c.invalidateOutboundSession(false)
	c.statusMu.Lock()
	wasConnected := c.status == StatusConnected
	c.status = StatusDisconnected
	c.statusMu.Unlock()

	// Signal shutdown first - this stops new requests from being queued
	c.stopOnce.Do(func() {
		close(c.stopChan)
	})

	// Stop the rate limiter - this drains the queue and waits for in-flight requests
	if c.rateLimiter != nil {
		c.rateLimiter.Stop()
	}

	// Do not flush: the buffer may hold a partial frame that IBKR would reject.

	// Cancel context
	if c.cancel != nil {
		c.cancel()
	}

	// Close the socket before waiting, but retain fields until the reader exits.
	c.closeSocket()

	// Bound the wait in case a platform socket close fails to unblock Read.
	waitDone := make(chan struct{})
	go func() {
		c.wg.Wait()
		close(waitDone)
	}()
	select {
	case <-waitDone:
		c.closeConnection()
	case <-time.After(2 * time.Second):
		// The parked reader may still dereference the transport fields, so
		connectLogger.Warnf("Disconnect: goroutines still running after 2s; closing socket to unblock (Client ID: %d)", c.config.ClientID)
	}

	// Log only a real connected-to-disconnected transition at info.
	if wasConnected {
		connectLogger.Infof("Connection closed (Client ID: %d)", c.config.ClientID)
	} else {
		connectLogger.Debugf("Connection closed (Client ID: %d)", c.config.ClientID)
	}

	if wasConnected && c.onDisconnect != nil {
		c.onDisconnect(nil)
	}

	if c.wireTap != nil {
		_ = c.wireTap.Close()
	}

	c.SetPacketLogger(nil)

	return nil
}

// reconnectWithBackoff retries a lost session using configured exponential backoff.
func (c *Connection) reconnectWithBackoff(ctx context.Context) {
	defer c.wg.Done()

	attempt := 0

	for {
		select {
		case <-c.stopChan:
			return
		case <-c.reconnectChan:
			// Reset attempt counter on new reconnect request
			attempt = 0
		case <-time.After(c.calculateBackoff(attempt)):
			if attempt >= c.config.MaxRetries {
				c.setStatus(StatusFailed)
				connectLogger.Errorf("Reconnection failed after %d attempts", attempt)
				return
			}

			attempt++
			connectLogger.Warnf("Reconnection attempt %d/%d (Client ID: %d)",
				attempt, c.config.MaxRetries, c.config.ClientID)

			c.setStatus(StatusReconnecting)

			connectCtx, cancel := context.WithTimeout(ctx, c.config.ConnectTimeout)
			err := c.Connect(connectCtx)
			cancel()

			if err == nil {
				connectLogger.Infof("Reconnection successful (Client ID: %d)", c.config.ClientID)
				return
			}

			c.statusMu.Lock()
			c.errorCount++
			c.lastError = err
			c.statusMu.Unlock()
		}
	}
}

// calculateBackoff calculates delay with exponential backoff and optional jitter
func (c *Connection) calculateBackoff(attempt int) time.Duration {
	if attempt == 0 {
		return 0
	}

	// Calculate exponential backoff
	delay := float64(c.config.InitialDelay) * math.Pow(c.config.BackoffMultiplier, float64(attempt-1))

	// Cap at max delay
	if delay > float64(c.config.MaxDelay) {
		delay = float64(c.config.MaxDelay)
	}

	// Add jitter if enabled (±10% randomization)
	if c.config.Jitter {
		jitter := delay * 0.1 * (rand.Float64()*2 - 1)
		delay += jitter
	}

	return time.Duration(delay)
}

// heartbeatMonitor checks connection health periodically
func (c *Connection) heartbeatMonitor() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ticker.C:
			c.statusMu.RLock()
			status := c.status
			c.statusMu.RUnlock()
			lastHeartbeat := time.Unix(0, c.lastHeartbeatNano.Load())

			if status != StatusConnected {
				continue
			}

			// Check if heartbeat is stale (no response for 2x interval)
			if time.Since(lastHeartbeat) > c.config.HeartbeatInterval*2 {
				connectLogger.Warnf("Heartbeat timeout (Client ID: %d)", c.config.ClientID)
				c.handleDisconnection(fmt.Errorf("heartbeat timeout"))
				return
			}

			// Send heartbeat request to IBKR
			if err := c.RequestCurrentTime(); err != nil {
				connectLogger.Warnf("Failed to send heartbeat: %v", err)
				// Don't disconnect immediately on heartbeat failure,
				// let the timeout mechanism handle it
			} else {
				c.lastHeartbeatNano.Store(time.Now().UnixNano())
			}
		}
	}
}

// handleDisconnection triggers reconnection if auto-reconnect is enabled
func (c *Connection) handleDisconnection(err error) {
	// Preserve brokerSessionEpoch for late inbound receipt attribution, but
	// revoke all outbound authority before any reconnect can reuse the object.
	c.invalidateOutboundSession(true)
	c.statusMu.Lock()
	if c.status != StatusConnected {
		c.statusMu.Unlock()
		return
	}
	c.status = StatusDisconnected
	c.lastError = err
	c.statusMu.Unlock()
	c.pauseTransport()

	connectLogger.Warnf("Disconnection detected (Client ID: %d): %v", c.config.ClientID, err)

	if c.onDisconnect != nil {
		c.onDisconnect(err)
	}

	if c.config.AutoReconnect {
		select {
		case c.reconnectChan <- struct{}{}:
			c.wg.Add(1)
			go c.reconnectWithBackoff(context.Background())
		default:
			// Reconnection already in progress
		}
	}
}

// Status returns the current connection lifecycle state.
func (c *Connection) Status() ConnectionStatus {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.status
}

// IsConnected reports whether the protocol session is connected.
func (c *Connection) IsConnected() bool {
	return c.Status() == StatusConnected
}

// setStatus updates connection status safely
func (c *Connection) setStatus(status ConnectionStatus) {
	c.statusMu.Lock()
	c.status = status
	c.statusMu.Unlock()
}

// SetOnConnect replaces the callback invoked after a successful connection.
func (c *Connection) SetOnConnect(fn func()) {
	c.onConnect = fn
}

// SetOnDisconnect replaces the callback invoked after a connection is lost.
func (c *Connection) SetOnDisconnect(fn func(error)) {
	c.onDisconnect = fn
}

// GetConnectionInfo returns a detached snapshot of connection diagnostics.
func (c *Connection) GetConnectionInfo() map[string]any {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()

	info := map[string]any{
		"client_id":      c.config.ClientID,
		"host":           c.config.Host,
		"port":           c.config.Port,
		"status":         c.status.String(),
		"error_count":    c.errorCount,
		"connected_at":   c.connectedAt,
		"last_heartbeat": time.Unix(0, c.lastHeartbeatNano.Load()),
		"server_version": c.serverVersion,
	}

	if c.lastError != nil {
		info["last_error"] = c.lastError.Error()
	}

	return info
}

// ServerVersion returns the protocol version negotiated with TWS or IB Gateway.
func (c *Connection) ServerVersion() int {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.serverVersion
}

// UsingTLS reports whether the established session negotiated TLS.
func (c *Connection) UsingTLS() bool {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	return c.useTLS
}

// Protocol constants aligned with IBKR CLIENT_VERSION 66
const (
	// Handshake advertises compatibility starting at client version 100
	minClientVersion = 100

	// Minimum server version we accept: 124 = MIN_SERVER_VER_SYNT_REALTIME_BARS
	minServerVersionRequired = 124

	// Maximum tested version (TWS API Gateway v10.30+)
	maxClientVersion = 203

	// Version 203 = MIN_SERVER_VER_PROTOBUF_PLACE_ORDER
	minServerVerProtoBufPlaceOrder = 203
	protoBufMsgID                  = 200

	minServerVerManualOrderTime  = 169
	minServerVerRFQFields        = 187
	minServerVerUndoRFQFields    = 190
	minServerVerCMETaggingFields = 192

	// Required for startApi optional capabilities
	minServerVerStartAPICapab = 72

	// Message IDs from IBKR protocol
	msgTickPrice                              = 1
	msgTickSize                               = 2
	msgOrderStatus                            = 3
	msgErrMsg                                 = 4
	msgOpenOrder                              = 5
	msgAcctValue                              = 6
	msgPortfolioValue                         = 7
	msgAcctUpdateTime                         = 8
	msgNextValidID                            = 9
	msgContractData                           = 10
	msgExecDetails                            = 11
	msgMarketDepth                            = 12
	msgMarketDepthL2                          = 13
	msgNewsBulletins                          = 14
	msgManagedAccts                           = 15
	msgReceiveFA                              = 16
	msgHistoricalData                         = 17
	msgHistoricalDataEnd                      = 108
	msgCurrentTimeMillis                      = 109
	msgBondContractData                       = 18
	msgTickOptionComputation                  = 21
	msgTickGeneric                            = 45
	msgTickString                             = 46
	msgTickEFP                                = 47
	msgCurrentTime                            = 49
	msgRealTimeBars                           = 50
	msgFundamentalData                        = 51
	msgContractDataEnd                        = 52
	msgOpenOrderEnd                           = 53
	msgAcctDownloadEnd                        = 54
	msgDeltaNeutralValidation                 = 56
	msgTickSnapshotEnd                        = 57
	msgMarketDataType                         = 58
	msgPosition                               = 61
	msgPositionEnd                            = 62
	msgAccountSummary                         = 63
	msgAccountSummaryEnd                      = 64
	msgVerifyMessageAPI                       = 65
	msgVerifyCompleted                        = 66
	msgDisplayGroupList                       = 67
	msgDisplayGroupUpdated                    = 68
	msgVerifyAndAuthMessageAPI                = 69
	msgVerifyAndAuthCompleted                 = 70
	msgPositionMulti                          = 71
	msgPositionMultiEnd                       = 72
	msgAccountUpdateMulti                     = 73
	msgAccountUpdateMultiEnd                  = 74
	msgSecurityDefinitionOptionalParameter    = 75
	msgSecurityDefinitionOptionalParameterEnd = 76
	msgSoftDollarTiers                        = 77
	msgFamilyCodes                            = 78
	msgSymbolSamples                          = 79
	msgMktDepthExchanges                      = 80
	msgTickNews                               = 81
	msgSmartComponents                        = 82
	msgTickReqParams                          = 83
	msgNewsProviders                          = 84
	msgNewsArticle                            = 85
	msgHistoricalNews                         = 86
	msgHistoricalNewsEnd                      = 87
	msgHeadTimestamp                          = 88
	msgHistogramData                          = 89
	msgHistoricalDataUpdate                   = 90
	msgRerouteMktDataReq                      = 91
	msgRerouteMktDepthReq                     = 92
	msgMarketRule                             = 93
	msgPnL                                    = 94
	msgPnLSingle                              = 95
	msgHistoricalTicks                        = 96
	msgHistoricalTicksBidAsk                  = 97
	msgHistoricalTicksLast                    = 98
	msgTickByTick                             = 99
	msgOrderBound                             = 100
	msgWSHMetaData                            = 104
	msgWSHEventData                           = 105
	msgSystemNotification                     = 204

	// Outgoing message IDs
	reqMktData                  = 1
	cancelMktData               = 2
	placeOrder                  = 3
	cancelOrder                 = 4
	reqOpenOrders               = 5
	reqAcctData                 = 6
	reqIds                      = 8
	reqContractData             = 9
	reqMktDepth                 = 10
	cancelMktDepth              = 11
	reqNewsBulletins            = 12
	cancelNewsBulletins         = 13
	setServerLogLevel           = 14
	reqAutoOpenOrders           = 15
	reqAllOpenOrders            = 16
	reqManagedAccts             = 17
	reqFA                       = 18
	replaceFA                   = 19
	reqHistoricalData           = 20
	exerciseOptions             = 21
	cancelHistoricalData        = 25
	reqCurrentTime              = 49
	reqRealTimeBars             = 50
	cancelRealTimeBars          = 51
	reqFundamentalData          = 52
	cancelFundamentalData       = 53
	reqCalcImpliedVolatility    = 54
	reqCalcOptionPrice          = 55
	cancelCalcImpliedVolatility = 56
	cancelCalcOptionPrice       = 57
	reqGlobalCancel             = 58
	reqMarketDataType           = 59
	reqPositions                = 61
	reqAccountSummary           = 62
	cancelAccountSummary        = 63
	cancelPositions             = 64
	verifyRequest               = 65
	verifyMessage               = 66
	queryDisplayGroups          = 67
	subscribeToGroupEvents      = 68
	updateDisplayGroup          = 69
	unsubscribeFromGroupEvents  = 70
	startAPI                    = 71
	reqSecDefOptParams          = 78
	reqWSHMetaData              = 100
	cancelWSHMetaData           = 101
	reqWSHEventData             = 102
	cancelWSHEventData          = 103
	// PnL subscription opcodes (TWS API EClient: REQ_PNL / CANCEL_PNL /
	// TWS protocol's outbound and inbound id spaces are separate.
	reqPnL          = 92
	cancelPnL       = 93
	reqPnLSingle    = 94
	cancelPnLSingle = 95
)

// suppressedMessageLogIDs keeps high-volume price, size, and computation
// frames out of debug logs during regular trading hours.
var suppressedMessageLogIDs = map[int]bool{
	msgTickPrice:         true, // Tick price updates (1)
	msgTickSize:          true, // Tick size updates (2)
	msgTickString:        true, // Tick string updates (46)
	msgTickGeneric:       true, // Generic tick updates (45)
	msgMarketDataType:    true, // Market data type (58)
	msgTickNews:          true, // Tick news (81)
	msgAccountSummary:    true, // Account summary (63)
	msgAccountSummaryEnd: true, // Account summary end (64)
	msgPosition:          true, // Position updates (61)
	msgPositionEnd:       true, // Position sync complete (62)
	15:                   true, // Managed accounts
	9:                    true, // Next valid ID
	4:                    true, // Error messages (handled separately)
	msgCurrentTimeMillis: true, // Heartbeat variant with ms precision (109)
}

var placeOrderBaseFields = []string{
	"3", "0", "0", "", "", "", "0.0", "", "", "SMART", "", "USD", "", "", "", "", "BUY", "0", "LMT", "0", "", "DAY", "", "", "", "0", "", "1", "0", "0", "0", "0", "0", "0", "0", "", "0", "", "", "", "", "", "", "", "0", "", "-1", "0", "", "", "0", "", "", "1", "1", "", "0", "", "", "", "", "", "0", "", "", "", "", "0", "", "", "", "", "", "", "", "", "", "", "0", "", "", "0", "0", "", "", "0", "", "0", "0", "0", "0", "", "", "", "", "", "", "0", "", "", "", "", "0", "0", "0", "", ""}

// handshake performs the initial IBKR protocol handshake
func (c *Connection) handshake() error {
	attemptPayloads := []string{
		fmt.Sprintf("v%d..%d", minClientVersion, maxClientVersion),
		fmt.Sprintf("v%d", maxClientVersion),
	}

	var sawNoData bool

	for idx, payload := range attemptPayloads {
		if err := c.sendHandshakePayload(payload); err != nil {
			return fmt.Errorf("failed to send handshake payload %q: %w", payload, err)
		}

		err := c.readHandshakeResponse()
		if err == nil {
			return nil
		}
		if errors.Is(err, errHandshakeNoData) {
			sawNoData = true
			handshakeLogger.Warnf("Client %d: no response to payload %q (attempt %d/%d)", c.config.ClientID, payload, idx+1, len(attemptPayloads))
			continue
		}
		return err
	}

	if sawNoData {
		return fmt.Errorf("%w: no response from IBKR gateway after %d attempts", errHandshakeNoData, len(attemptPayloads))
	}

	return fmt.Errorf("handshake failed: no valid response format detected")
}

func (c *Connection) sendHandshakePayload(versionDescriptor string) error {
	descriptorBytes := append([]byte(versionDescriptor), '\x00')
	var lengthBuf [4]byte
	binary.BigEndian.PutUint32(lengthBuf[:], uint32(len(descriptorBytes)))

	var frame bytes.Buffer
	frame.Grow(4 + len(lengthBuf) + len(descriptorBytes))
	frame.WriteString("API\x00")
	frame.Write(lengthBuf[:])
	frame.Write(descriptorBytes)

	handshakeLogger.Infof("Client %d: sending descriptor %q", c.config.ClientID, versionDescriptor)
	return c.withTransport(true, func() error {
		_, err := c.conn.Write(frame.Bytes())
		return err
	})
}

func (c *Connection) readHandshakeResponse() error {
	const handshakeDeadline = 10 * time.Second
	if err := c.conn.SetReadDeadline(time.Now().Add(handshakeDeadline)); err != nil {
		return fmt.Errorf("handshake set deadline: %w", err)
	}
	defer c.conn.SetReadDeadline(time.Time{})

	head, err := c.reader.Peek(4)
	if err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake peek failed: %w", err)
	}

	first := head[0]
	if first == '-' || (first >= '0' && first <= '9') {
		return c.readAsciiHandshake()
	}
	return c.readLengthPrefixedHandshake()
}

func (c *Connection) readLengthPrefixedHandshake() error {
	var lengthBuf [4]byte
	if _, err := io.ReadFull(c.reader, lengthBuf[:]); err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read frame length: %w", err)
	}

	frameLen := int(binary.BigEndian.Uint32(lengthBuf[:]))
	if frameLen == 0 {
		return errHandshakeNoData
	}
	if frameLen < 0 || frameLen > 4096 {
		return fmt.Errorf("handshake frame length out of bounds: %d", frameLen)
	}

	payload := make([]byte, frameLen)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read frame payload: %w", err)
	}

	fieldsRaw := bytes.Split(payload, []byte{0})
	fields := make([]string, 0, len(fieldsRaw))
	for i, raw := range fieldsRaw {
		// Drop a trailing empty field if the payload ended with a null delimiter
		if i == len(fieldsRaw)-1 && len(raw) == 0 {
			continue
		}
		fields = append(fields, string(raw))
	}

	if len(fields) == 0 || fields[0] == "" {
		return errHandshakeNoData
	}

	serverVersion, err := strconv.Atoi(fields[0])
	if err != nil {
		return fmt.Errorf("invalid server version string %q: %w", fields[0], err)
	}

	if serverVersion == -1 {
		if len(fields) < 2 {
			return fmt.Errorf("handshake redirect requested but no target provided")
		}
		return fmt.Errorf("handshake redirect requested: %s", fields[1])
	}

	connTime := ""
	if serverVersion >= 20 {
		if len(fields) >= 2 {
			connTime = fields[1]
		} else {
			handshakeLogger.Warnf("Client %d: server version %d provided no time string", c.config.ClientID, serverVersion)
		}
	}

	if serverVersion < minServerVersionRequired {
		return fmt.Errorf("server version %d is too old (minimum: %d)", serverVersion, minServerVersionRequired)
	}

	c.serverVersion = serverVersion
	c.connTime = connTime
	handshakeLogger.Infof("Client %d: Server Version %d, Time %s (v100 frame)", c.config.ClientID, c.serverVersion, c.connTime)
	return nil
}

func (c *Connection) readAsciiHandshake() error {
	verStr, err := c.readHandshakeCString()
	if err != nil {
		if isHandshakeNoDataErr(err) {
			return errHandshakeNoData
		}
		return fmt.Errorf("handshake read version string: %w", err)
	}
	if verStr == "" {
		return errHandshakeNoData
	}

	serverVersion, err := strconv.Atoi(verStr)
	if err != nil {
		return fmt.Errorf("invalid server version string %q: %w", verStr, err)
	}

	if serverVersion == -1 {
		redirect, err := c.readHandshakeCString()
		if err != nil {
			return fmt.Errorf("handshake read redirect target: %w", err)
		}
		return fmt.Errorf("handshake redirect requested: %s", redirect)
	}

	connTime := ""
	if serverVersion >= 20 {
		timeStr, err := c.readHandshakeCString()
		if err != nil {
			if !isHandshakeNoDataErr(err) {
				return fmt.Errorf("handshake read time: %w", err)
			}
		} else {
			connTime = timeStr
		}
	}

	if serverVersion < minServerVersionRequired {
		return fmt.Errorf("server version %d is too old (minimum: %d)", serverVersion, minServerVersionRequired)
	}

	c.serverVersion = serverVersion
	c.connTime = connTime
	handshakeLogger.Infof("Client %d: Server Version %d, Time %s (ascii)", c.config.ClientID, c.serverVersion, c.connTime)
	return nil
}

func (c *Connection) readHandshakeCString() (string, error) {
	data, err := c.reader.ReadString('\x00')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(data, "\x00"), nil
}

// isHandshakeNoDataErr reports whether err means "the peer hung up before
//   - io.EOF / io.ErrUnexpectedEOF: graceful close after we sent the request.
//   - net.Error with Timeout(): the server accepted but did not reply in time.
func isHandshakeNoDataErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	if netErr, ok := errors.AsType[net.Error](err); ok {
		return netErr.Timeout()
	}
	return false
}

// startAPI sends the start API message to initialize the connection
func (c *Connection) startAPI() error {
	fields := []any{startAPI, 2, c.config.ClientID}
	if c.serverVersion >= minServerVerStartAPICapab {
		// Optional capabilities placeholder – currently unused but must be omitted
		fields = append(fields, "")
	}

	msg := c.encodeMsg(fields...)
	lengthBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

	// Debug: hex dump START_API message
	c.logOutgoingMessageHex(msg)

	sendErr := c.withTransport(true, func() error {
		if c.writer == nil {
			return fmt.Errorf("%w: buffered writer not initialized before startAPI", errStartAPIFailed)
		}
		if _, err := c.writer.Write(lengthBytes); err != nil {
			return fmt.Errorf("%w: failed to send startAPI length: %w", errStartAPIFailed, err)
		}
		if _, err := c.writer.Write(msg); err != nil {
			return fmt.Errorf("%w: failed to send startAPI payload: %w", errStartAPIFailed, err)
		}
		c.logPacketOutbound(msg)
		if err := c.writer.Flush(); err != nil {
			return fmt.Errorf("%w: failed to flush startAPI payload: %w", errStartAPIFailed, err)
		}
		return nil
	})
	if sendErr != nil {
		return sendErr
	}

	// Sent startAPI message

	// Wait for initial responses (managed accounts, next valid ID, etc.)
	c.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{}) // Clear deadline

	// Track if we get error 326 (client ID already in use)
	var clientIDError error

	// Read initial responses
	for range 10 { // Try to read up to 10 initial messages
		msgBytes, err := c.readMessage()
		if err != nil {
			// Capture a client-ID-collision lastError observed mid-read so
			// the caller's retry loop branches on errClientIDInUse rather
			// than the read error itself.
			c.statusMu.RLock()
			if errors.Is(c.lastError, errClientIDInUse) {
				clientIDError = c.lastError
			}
			c.statusMu.RUnlock()

			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				break // Timeout is expected after initial messages
			}
			if errors.Is(err, io.EOF) {
				// EOF after startAPI: if the gateway told us our client ID
				c.statusMu.RLock()
				lastErr := c.lastError
				c.statusMu.RUnlock()
				if errors.Is(lastErr, errClientIDInUse) {
					return lastErr
				}
				if clientIDError != nil {
					return clientIDError
				}
				return fmt.Errorf("%w: connection closed by server after startAPI", errStartAPIFailed)
			}
			// Log but don't fail on read errors during initialization
			connectLogger.Errorf("Error reading initial message: %v", err)
			break
		}

		// Process the initial message
		c.processMessage(msgBytes)

		c.statusMu.RLock()
		if errors.Is(c.lastError, errClientIDInUse) {
			clientIDError = c.lastError
		}
		c.statusMu.RUnlock()
	}

	// If we detected client ID error, return it
	if clientIDError != nil {
		return clientIDError
	}
	if !c.BrokerIDNamespaceReady() {
		return fmt.Errorf("%w: nextValidId not received during startup", errStartAPIFailed)
	}

	return nil
}

// readMessages continuously reads messages from the connection.
// Wrapped in a panic guard because this goroutine is the only consumer of
// malformed wire data without letting the only receipt reader die silently.
func (c *Connection) readMessages() {
	defer c.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			connectLogger.Errorf("readMessages panic recovered (Client ID: %d): %v\n%s",
				c.config.ClientID, r, debug.Stack())
			c.handleDisconnection(fmt.Errorf("reader panic: %v", r))
		}
	}()
	c.signalReadStarted()
	epoch := c.BrokerSessionEpoch()

	for {
		select {
		case <-c.stopChan:
			return
		default:
			if c.conn == nil {
				c.handleDisconnection(io.EOF)
				return
			}
			// Read message with timeout
			c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			msgBytes, err := c.readMessage()
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected, continue
					continue
				}
				if err == io.EOF {
					ibkrLogger.Warnf("Connection closed by server")
					c.handleDisconnection(err)
					return
				}
				// Any other error means stream alignment is uncertain —
				// errors before disconnect). Fail fast: log, signal
				ibkrLogger.Errorf("Error reading message: %v", err)
				c.handleDisconnection(err)
				return
			}

			// Process the message
			c.processMessageAtEpoch(msgBytes, epoch)

			// Receipt freshness belongs to the exact socket that delivered the
			// frame. A retired reader must not make its successor look healthy.
			c.recordHeartbeatAtEpoch(epoch, time.Now().UnixNano())
		}
	}
}

// processMessage handles incoming messages from IBKR
func (c *Connection) processMessage(msgBytes []byte) {
	c.processMessageAtEpoch(msgBytes, c.BrokerSessionEpoch())
}

func (c *Connection) processMessageAtEpoch(msgBytes []byte, epoch uint64) {
	fields := c.decodeMessage(msgBytes)
	if len(fields) == 0 {
		return
	}

	// First field is always the message ID
	msgID, err := strconv.Atoi(fields[0])
	if err != nil {
		ibkrLogger.Warnf("[WARNING] Invalid message ID: %v", err)
		return
	}

	if c.wireTap != nil {
		c.wireTap.RecordInbound(msgID, msgBytes, fields)
	}
	if epoch != c.BrokerSessionEpoch() {
		// Only epoch-aware order handlers may observe a stale frame, solely so
		// their owner can fail closed. Never run legacy handlers or mutate the
		// current session's request, farm, portfolio, or local-order state.
		c.dispatchStaleInboundMessage(msgID, fields, epoch)
		return
	}

	// The first atomic check above is an inexpensive fast path. Every ordinary
	// and legacy callbacks, so a retired reader cannot contaminate the next
	// socket's portfolio/account/request authority after reconnect begins.
	// they must run outbound recovery only after release. Broker-scope frames
	// cannot deadlock behind a reader that is itself waiting for publication.
	// The two small typed receipt handlers below take their own leases around
	switch msgID {
	case msgErrMsg, msgSystemNotification, msgCurrentTimeMillis, msgMarketDataType:
	case msgManagedAccts, msgAccountSummary:
		if c.publicationBarrier != nil {
			c.publicationBarrier.Lock()
			defer c.publicationBarrier.Unlock()
		}
		c.inboundEpochMu.RLock()
		defer c.inboundEpochMu.RUnlock()
		if epoch != c.BrokerSessionEpoch() {
			return
		}
		if c.evidenceBarrier != nil {
			c.evidenceBarrier.RLock()
			defer c.evidenceBarrier.RUnlock()
		}
	default:
		if c.messageAfterInitialEpochCheck != nil {
			c.messageAfterInitialEpochCheck(msgID)
		}
		c.inboundEpochMu.RLock()
		defer c.inboundEpochMu.RUnlock()
		if epoch != c.BrokerSessionEpoch() {
			c.dispatchStaleInboundMessage(msgID, fields, epoch)
			return
		}
	}

	// Only log unusual messages for debugging
	if msgID != 0 && !suppressedMessageLogIDs[msgID] && msgID != msgCurrentTime {
		ibkrLogger.Debugf("Received message ID %d with %d fields", msgID, len(fields))
	}
	if orderID, ok := inboundOrderID(msgID, fields); ok {
		c.observeInboundOrderIDAtEpoch(orderID, epoch)
	}
	if msgID == msgOpenOrder || msgID == msgOpenOrderEnd {
		c.openOrderObserverMu.RLock()
		observer := c.openOrderObserver
		c.openOrderObserverMu.RUnlock()
		if observer != nil {
			observer(msgID, fields, epoch)
		}
	}

	// Handle common messages
	switch msgID {
	case msgNextValidID:
		if id, ok := parseNextValidOrderID(fields); ok {
			c.observeNextValidOrderIDAtEpoch(id, epoch)
			if c.config.ClientID == 1 {
				ibkrLogger.Infof("Next Valid Order ID: %d", id)
			}
		}
	case msgCurrentTimeMillis:
		if len(fields) > 1 {
			if ms, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				c.recordHeartbeatAtEpoch(epoch, ms*int64(time.Millisecond))
			}
		}
	case msgManagedAccts:
		if acct := managedAccountsField(fields); acct != "" {
			c.accountMu.Lock()
			c.account = acct
			c.managedAccounts = parseManagedAccounts(acct)
			c.accountMu.Unlock()
			// The canonical primary client logs this account-wide notice; auxiliary
			if c.config.ClientID == 1 {
				ibkrLogger.Infof("Managed Accounts: %s", acct)
			}
		}
	case msgErrMsg:
		if c.errorMessageAfterInitialEpochCheck != nil {
			c.errorMessageAfterInitialEpochCheck()
		}
		c.processErrorMessageAtEpoch(fields, epoch)
	case msgCurrentTime:
		// IBKR heartbeat - silently process to maintain connection
	case msgPosition:
		c.handlePosition(fields)
	case msgPositionEnd:
		portfolioLogger.Infof("Position sync complete")
		c.completePositionsSnapshot()
		// Signal that positions are complete
		select {
		case c.positionsEndChan <- struct{}{}:
		default:
			// Channel already has a signal
		}
	case msgAccountSummary:
		c.handleAccountSummaryUnderBrokerScopeLease(fields)
	case msgAccountSummaryEnd:
		// Wire-cadence event: the daemon re-polls the summary every few
		portfolioLogger.Debugf("Account summary sync complete")
		c.signalSummaryEnd(fields)
		// Legacy shared signal for WaitForAccountSummaryEnd callers
		select {
		case c.acctSummaryEndChan <- struct{}{}:
		default:
			// Channel already has a signal
		}
	case msgPortfolioValue:
		c.handlePortfolioValue(fields)
	case msgAcctValue:
		c.handleAccountValue(fields)
	case msgAcctUpdateTime:
		// fields: [msgID, version, HH:MM]. fields[1] is the protocol
		if len(fields) > 2 {
			// Wire-cadence event, streamed continuously while account
			portfolioLogger.Debugf("Account update time: %s", fields[2])
		}
		c.portfolioProjectionMu.Lock()
		c.portfolioHealthMu.Lock()
		if c.portfolioHealth.ScopeConflictAt.IsZero() && c.portfolioHealth.InvalidPayloadAt.IsZero() {
			c.portfolioHealth.LastUpdateAt = time.Now().UTC()
		}
		c.portfolioHealthMu.Unlock()
		c.portfolioProjectionMu.Unlock()
	case msgAcctDownloadEnd:
		portfolioLogger.Infof("Account download complete")
		account := ""
		if len(fields) > 2 {
			account = fields[2]
		}
		c.completePortfolioDownload(account, time.Now().UTC())
	case msgMarketDataType:
		c.processMarketDataTypeAtEpoch(fields, epoch)
	case msgSystemNotification:
		if c.systemNoticeAfterInitialEpochCheck != nil {
			c.systemNoticeAfterInitialEpochCheck()
		}
		c.processSystemNoticeMessageAtEpoch(fields, epoch)
	case msgTickNews:
		// News tick - handle silently for now
	case msgTickString:
		// String tick data (e.g., last timestamp, bid/ask exchange)
	case msgTickGeneric:
		// Generic tick data (e.g., 106 = Option Implied Volatility)
		if c.dispatchHandlers(msgTickGeneric, fields, epoch) {
			return
		}
	case msgSecurityDefinitionOptionalParameter, msgSecurityDefinitionOptionalParameterEnd:
		// reqSecDefOptParams (78) responses arrive once per exchange (75) before
		c.dispatchHandlers(msgID, fields, epoch)
		return
	default:
		// Check for registered handler
		if c.dispatchHandlers(msgID, fields, epoch) {
		} else if isBenignUnhandledMessage(msgID) {
			// Contract details may arrive even when the connector did not register a handler.
			return
		} else {
			ibkrLogger.Warnf("[WARNING] Unhandled message ID %d: %v", msgID, fields)
		}
	}
}

func (c *Connection) dispatchStaleInboundMessage(msgID int, fields []string, epoch uint64) {
	switch msgID {
	case msgOpenOrder, msgOrderStatus, msgExecDetails:
		c.dispatchEpochHandlers(msgID, fields, epoch)
	case msgErrMsg:
		// The Connector-owned handler publishes only an exact stale receipt;
		_ = c.dispatchErrorPostAction(fields, epoch)
		c.dispatchEpochHandlers(msgID, fields, epoch)
	case msgSystemNotification:
		c.dispatchStaleSystemNotice(fields, epoch)
	}
}

// processErrorMessageAtEpoch keeps the final epoch check, all current-session
// side effects, and legacy handler delivery in one inbound-generation read
// section. The scoped defer is intentional: a panicking handler must not leave
// receipt handlers run only after the lock is released and never reach the
// current-session legacy surface.
func (c *Connection) processErrorMessageAtEpoch(fields []string, epoch uint64) {
	current := false
	var postLease []func()
	func() {
		c.inboundEpochMu.RLock()
		defer c.inboundEpochMu.RUnlock()
		if epoch != c.BrokerSessionEpoch() {
			return
		}
		current = true
		if post := c.handleErrorMessage(fields, epoch); post != nil {
			postLease = append(postLease, post)
		}
		if post := c.dispatchErrorPostAction(fields, epoch); post != nil {
			postLease = append(postLease, post)
		}
		// Also forward to registered error handlers for higher-level recovery.
		c.dispatchHandlers(msgErrMsg, fields, epoch)
	}()
	if !current {
		c.dispatchEpochHandlers(msgErrMsg, fields, epoch)
		return
	}
	for _, post := range postLease {
		post()
	}
}

// processSystemNoticeMessageAtEpoch gives msg-204 the same final socket-
// mutations, and legacy handler delivery complete under inboundEpochMu.R.
// Stale receipts reach only the epoch-aware owner; outbound actions run later.
func (c *Connection) processSystemNoticeMessageAtEpoch(fields []string, epoch uint64) {
	current := false
	var postLease func()
	func() {
		c.inboundEpochMu.RLock()
		defer c.inboundEpochMu.RUnlock()
		if epoch != c.BrokerSessionEpoch() {
			return
		}
		current = true
		postLease = c.handleSystemNotificationAtEpoch(fields, epoch)
		c.dispatchHandlers(msgSystemNotification, fields, epoch)
	}()
	if !current {
		c.dispatchStaleSystemNotice(fields, epoch)
		return
	}
	if postLease != nil {
		postLease()
	}
}

func isBenignUnhandledMessage(msgID int) bool {
	switch msgID {
	case msgContractData, msgContractDataEnd:
		return true
	default:
		return false
	}
}

// getErrorDescription returns a human-readable description for IBKR error codes
func getErrorDescription(code int) string {
	switch code {
	// Low-numbered error codes (1-99)
	case 1:
		return "Requested market data is not available"
	case 2:
		return "Requested market data is not subscribed"
	case 3:
		return "Requested market data cannot be retrieved"
	case 4:
		return "Market data request error"

	// Connection and System Status (2100-2199)
	case 2104:
		return "Market data farm connected"
	case 2106:
		return "Historical data farm connected"
	case 2107:
		return "Historical data farm connected (inactive)"
	case 2108:
		return "Market data farm disconnected"
	case 2110:
		return "Connectivity between TWS and server is broken"
	case 2119:
		return "Market data farm connection is OK"
	case 2158:
		return "Security definition data farm connected"

	// Market Data Errors (300-399)
	case 320:
		return "Reading request error - Invalid ticker or exchange"
	case 321:
		return "Error validating request"
	case 322:
		return "Error processing request - Duplicate ticker ID"
	case 326:
		return "Unable to connect as client id is already in use. Retry with unique client id"
	case 354:
		return "Requested market data is not subscribed"

	// Order and Trading Errors (100-199)
	case 110:
		return "Price does not conform to minimum price variation"
	case 161:
		return "Cancel attempted when order is not in a cancellable state"
	case 162:
		return "Historical market data service error"

	// Connection Errors (500-599)
	case 502:
		return "Couldn't connect to TWS"
	case 503:
		return "The TWS is out of date and must be upgraded"
	case 504:
		return "Not connected to TWS"

	// Account and Position Errors (400-449)
	case 430:
		return "The account code is required for this operation"
	case 431:
		return "Invalid account code"

	default:
		return ""
	}
}

// handleErrorMessage processes error messages from IBKR
func (c *Connection) handleErrorMessage(fields []string, epoch uint64) (postLease func()) {
	if len(fields) < 3 {
		ibkrLogger.Warnf("[WARNING] Invalid error message: %v", fields)
		return
	}

	// Expected layout: [msgId(4), version, reqId, errorCode, errorMsg]
	reqID := ""
	errorCode := ""
	errorMsg := ""
	if len(fields) > 2 {
		reqID = fields[2]
	}
	if len(fields) > 3 {
		errorCode = fields[3]
	}
	if len(fields) > 4 {
		errorMsg = fields[4]
	} else if len(fields) > 3 {
		errorMsg = fields[3]
	}

	// Check the error code to determine if it's informational or an actual error
	code, _ := strconv.Atoi(errorCode)

	// Log important errors for debugging
	if code >= 300 && code < 400 {
		ibkrLogger.Debugf("[cid=%d] Market data error for reqID %s: code=%s, msg=%s", c.config.ClientID, reqID, errorCode, errorMsg)
	} else if code == 200 || code == 162 || code == 10197 {
		// Market data subscription errors
		ibkrLogger.Debugf("[cid=%d] Market data subscription error for reqID %s: code=%s, msg=%s", c.config.ClientID, reqID, errorCode, errorMsg)
	}

	// Sometimes IBKR sends the error code in the message field
	if msgCode, err := strconv.Atoi(errorMsg); err == nil {
		// The message is an error code, look up its description
		if desc := getErrorDescription(msgCode); desc != "" {
			errorMsg = desc
		} else {
			errorMsg = fmt.Sprintf("Error code %d", msgCode)
		}
	}

	// Get human-readable description for the main error code
	description := getErrorDescription(code)
	if description != "" && errorMsg == errorCode {
		// Only replace if errorMsg is just the code repeated
		errorMsg = description
	}

	// IBKR informational codes (not actual errors)
	switch code {
	case 2104, 2106, 2107, 2158, 2119, 2169:
		// These are normal connection confirmations, suppress them
		return
	}

	// Warning level codes
	if code >= 2100 && code < 2200 {
		ibkrLogger.Warnf("[cid=%d] %s", c.config.ClientID, errorMsg)
	} else if code == 502 || code == 503 || code == 504 {
		// Connection errors - these are critical
		ibkrLogger.Errorf("[cid=%d] Critical Error: %s (Code %d)", c.config.ClientID, errorMsg, code)
		c.handleDisconnection(fmt.Errorf("connection error %d: %s", code, errorMsg))
	} else if code == 326 {
		// Client ID already in use — surface as the sentinel so the
		// matching this exact format string.
		ibkrLogger.Infof("[cid=%d] System notice: %s", c.config.ClientID, errorMsg)
		c.statusMu.Lock()
		c.lastError = clientIDInUseError(c.config.ClientID, errorMsg)
		c.statusMu.Unlock()
	} else if code == 200 {
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		if rid, err := strconv.Atoi(reqID); err == nil {
			c.releaseMarketDataSlotAtEpoch(rid, epoch)
		}
	} else if code == 320 || code == 321 || code == 322 || code == 354 {
		// Market data errors
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		if code == 354 {
			if rid, err := strconv.Atoi(reqID); err == nil {
				c.releaseMarketDataSlotAtEpoch(rid, epoch)
			}
		}
	} else if code == 10197 {
		// Competing live session blocks real-time data; switch to delayed
		ibkrLogger.Warnf("[cid=%d] Market Data Error (ReqID %s): %s", c.config.ClientID, reqID, errorMsg)
		if c.markCompetingLiveSession(reqID) {
			// Never enter outbound pacing/transport while holding inboundEpochMu.R:
			// reconnect pauses transport before taking inboundEpochMu.W. The exact
			// epoch-bound send below runs only after the inbound lease is released.
			postLease = func() {
				if err := c.setMarketDataTypeAtEpoch(3, epoch); err != nil {
					ibkrLogger.Errorf("[cid=%d] Failed to request delayed market data after 10197: %v", c.config.ClientID, err)
				} else {
					ibkrLogger.Warnf("[cid=%d] Forced delayed market data after 10197 (%s)", c.config.ClientID, errorMsg)
				}
			}
		}
	} else if code < 0 || code == 0 {
		// Codes -1 or 0 often contain system messages
		if errorMsg != "" && errorMsg != "0" {
			// Check if this is one of the informational messages we want to suppress
			if errorMsg == "Market data farm connected" ||
				errorMsg == "Historical data farm connected" ||
				errorMsg == "Historical data farm connected (inactive)" ||
				errorMsg == "Security definition data farm connected" {
				// Suppress these repetitive connection confirmations
				return
			}
			// Only log if it's not just the code number
			if _, err := strconv.Atoi(errorMsg); err != nil {
				ibkrLogger.Infof("[cid=%d] System: %s", c.config.ClientID, errorMsg)
				// Some gateway builds emit the client-ID-in-use notice
				errLower := strings.ToLower(errorMsg)
				if strings.Contains(errLower, "unable to connect as client id") ||
					strings.Contains(errLower, "client id is already in use") ||
					strings.Contains(errLower, "client id already in use") {
					c.statusMu.Lock()
					c.lastError = fmt.Errorf("%w: %s", errClientIDInUse, errorMsg)
					c.statusMu.Unlock()
				}
			}
		}
	} else {
		// Other errors
		ibkrLogger.Warnf("[cid=%d] Error (ReqID %s): %s (Code %d)", c.config.ClientID, reqID, errorMsg, code)
	}
	return postLease
}

// HasCompetingLiveSession returns true if IBKR reported code 10197 for this connection.
func (c *Connection) HasCompetingLiveSession() bool {
	c.competingMu.RLock()
	defer c.competingMu.RUnlock()
	return c.competingLiveSession
}

func (c *Connection) markCompetingLiveSession(reqID string) bool {
	c.competingMu.Lock()
	already := c.competingLiveSession
	c.competingLiveSession = true
	c.competingMu.Unlock()

	if already {
		return false
	}

	ibkrLogger.Warnf("[cid=%d] 10197 competing live session detected (reqID=%s) – requesting delayed market data", c.config.ClientID, reqID)
	return true
}

// acquireMarketDataSlot acquires a market data slot and records the holding
func (c *Connection) acquireMarketDataSlot(ctx context.Context, reqID int) error {
	if c.rateLimiter == nil {
		return nil
	}
	if err := c.rateLimiter.AcquireMarketDataSlot(ctx); err != nil {
		return err
	}
	c.marketDataSlotsMu.Lock()
	c.marketDataSlots[reqID] = c.BrokerSessionEpoch()
	c.marketDataSlotsMu.Unlock()
	return nil
}

// releaseMarketDataSlot releases a market data slot iff this reqID currently
// never over-releases.
func (c *Connection) releaseMarketDataSlot(reqID int) {
	c.releaseMarketDataSlotAtEpoch(reqID, 0)
}

// releaseMarketDataSlotAtEpoch releases reqID only when its slot belongs to
// epoch. A zero epoch is the compatibility form for a current synchronous
// caller. Exact stale cleanup uses a nonzero epoch so a retired subscription
// can never release a successor socket's reused request ID.
func (c *Connection) releaseMarketDataSlotAtEpoch(reqID int, epoch uint64) {
	if c.rateLimiter == nil {
		return
	}
	c.marketDataSlotsMu.Lock()
	heldEpoch, held := c.marketDataSlots[reqID]
	if held && (epoch == 0 || heldEpoch == epoch) {
		delete(c.marketDataSlots, reqID)
	} else {
		held = false
	}
	c.marketDataSlotsMu.Unlock()
	if held {
		c.rateLimiter.ReleaseMarketDataSlot()
	}
}

func (c *Connection) registerStartAPIFailure() time.Duration {
	c.startAPIMu.Lock()
	defer c.startAPIMu.Unlock()

	c.startAPIFailures++
	c.lastStartAPIFailure = time.Now()

	switch {
	case c.startAPIFailures == 1:
		return 2 * time.Second
	case c.startAPIFailures == 2:
		return 5 * time.Second
	case c.startAPIFailures <= 4:
		return 15 * time.Second
	case c.startAPIFailures <= 6:
		return 30 * time.Second
	default:
		return time.Minute
	}
}

func (c *Connection) resetStartAPIFailure() {
	c.startAPIMu.Lock()
	c.startAPIFailures = 0
	c.lastStartAPIFailure = time.Time{}
	c.startAPIMu.Unlock()
}

// handlePosition processes position updates from IBKR
func (c *Connection) handlePosition(fields []string) {
	// IBKR Position message format (msgID 61, version 3):
	// 2: account

	// The actual position message format might vary based on version
	if len(fields) < 13 {
		portfolioLogger.Errorf("Position message too short: %d fields", len(fields))
		return
	}

	// Parse contract details
	conID, _ := strconv.Atoi(fields[3])

	var positionSize, avgCost float64
	var contract Contract

	if len(fields) == 13 {
		// Stock format (13 fields)
		multiplier, _ := strconv.ParseFloat(fields[6], 64)
		if multiplier == 0 {
			multiplier = 1
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Multiplier:   int(multiplier),
			Exchange:     fields[7],
			Currency:     fields[8],
			LocalSymbol:  fields[9],
			TradingClass: fields[10],
		}

		positionSize, _ = strconv.ParseFloat(fields[11], 64)
		avgCost, _ = strconv.ParseFloat(fields[12], 64)

	} else if len(fields) == 15 {
		// Options format (15 fields) - includes expiry, strike, right
		strike, _ := strconv.ParseFloat(fields[7], 64)
		multiplier, _ := strconv.Atoi(fields[9])
		if multiplier == 0 {
			multiplier = 100 // Default for options
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Expiry:       fields[6],
			Strike:       strike,
			Right:        fields[8],
			Multiplier:   multiplier,
			Exchange:     fields[10],
			Currency:     fields[11],
			LocalSymbol:  fields[12],
			TradingClass: fields[13],
		}

		// Position at field 14, avgCost might be missing
		if fields[14] != "" {
			positionSize, _ = strconv.ParseFloat(fields[14], 64)
		}

	} else if len(fields) >= 16 {
		// Full options format with avgCost
		strike, _ := strconv.ParseFloat(fields[7], 64)
		multiplier, _ := strconv.Atoi(fields[9])
		if multiplier == 0 {
			multiplier = 100
		}

		contract = Contract{
			ConID:        conID,
			Symbol:       fields[4],
			SecType:      fields[5],
			Expiry:       fields[6],
			Strike:       strike,
			Right:        fields[8],
			Multiplier:   multiplier,
			Exchange:     fields[10],
			Currency:     fields[11],
			LocalSymbol:  fields[12],
			TradingClass: fields[13],
		}

		positionSize, _ = strconv.ParseFloat(fields[14], 64)
		avgCost, _ = strconv.ParseFloat(fields[15], 64)

	} else {
		portfolioLogger.Errorf("Unexpected position message format with %d fields", len(fields))
		return
	}

	c.portfolioProjectionMu.Lock()
	defer c.portfolioProjectionMu.Unlock()
	if !c.positionsSnapshotActive {
		// msgPosition belongs only to the singleton reqPositions stream. An
		// unsolicited/late row cannot mutate the streaming account-updates
		// authority or a completed one-shot result.
		return
	}
	key := cachedPositionKey(fields[2], contract)
	if positionSize == 0 {
		delete(c.positionsSnapshot, key)
		portfolioLogger.Debugf("Position closed: %s %s", fields[2], key)
		return
	}

	next := &RawPosition{
		Account:     fields[2],
		Contract:    contract,
		Position:    positionSize,
		AverageCost: avgCost,
	}
	if existing := c.positionsSnapshot[key]; existing != nil && existing.Contract.ConID == contract.ConID {
		next.Contract = mergeCachedPositionContract(existing.Contract, next.Contract)
		next.MarketPrice = existing.MarketPrice
		next.MarketValue = existing.MarketValue
		next.UnrealizedPNL = existing.UnrealizedPNL
		next.RealizedPNL = existing.RealizedPNL
	}
	c.positionsSnapshot[key] = next

	portfolioLogger.Debugf("Position: %s %s %.2f @ %.2f",
		fields[2], key, positionSize, avgCost)
}

// cachedPositionKey preserves exact account+contract identity whenever the
// broker supplies a positive ConID. Symbol-only keys caused same-symbol stock
// own scope checks. Descriptive fallbacks remain only for legacy zero-ConID rows.
func cachedPositionKey(account string, contract Contract) string {
	account = strings.ToUpper(strings.TrimSpace(account))
	secType := strings.ToUpper(strings.TrimSpace(contract.SecType))
	if contract.ConID > 0 {
		return fmt.Sprintf("%s|%s:%d", account, secType, contract.ConID)
	}
	if secType == "OPT" || secType == "FOP" {
		return fmt.Sprintf("%s|%s:%s_%s_%s%.0f", account, secType, contract.Symbol, contract.Expiry, contract.Right, contract.Strike)
	}
	return fmt.Sprintf("%s|%s:%s", account, secType, contract.Symbol)
}

func mergeCachedPositionContract(existing, incoming Contract) Contract {
	if incoming.Exchange == "" {
		incoming.Exchange = existing.Exchange
	}
	if incoming.PrimaryExch == "" {
		incoming.PrimaryExch = existing.PrimaryExch
	}
	if incoming.Currency == "" {
		incoming.Currency = existing.Currency
	}
	if incoming.LocalSymbol == "" {
		incoming.LocalSymbol = existing.LocalSymbol
	}
	if incoming.TradingClass == "" {
		incoming.TradingClass = existing.TradingClass
	}
	return incoming
}

func normalizedContractIdentity(contract Contract) Contract {
	contract.Symbol = strings.ToUpper(strings.TrimSpace(contract.Symbol))
	contract.SecType = strings.ToUpper(strings.TrimSpace(contract.SecType))
	contract.Expiry = strings.TrimSpace(contract.Expiry)
	contract.Right = strings.ToUpper(strings.TrimSpace(contract.Right))
	contract.Exchange = strings.ToUpper(strings.TrimSpace(contract.Exchange))
	contract.PrimaryExch = strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	contract.Currency = strings.ToUpper(strings.TrimSpace(contract.Currency))
	contract.LocalSymbol = strings.ToUpper(strings.TrimSpace(contract.LocalSymbol))
	contract.TradingClass = strings.ToUpper(strings.TrimSpace(contract.TradingClass))
	contract.SecIDType = strings.ToUpper(strings.TrimSpace(contract.SecIDType))
	contract.SecID = strings.TrimSpace(contract.SecID)
	return contract
}

// samePortfolioStructure intentionally excludes prices, cost basis, and PnL.
// position the daemon is evaluating. Exact contract identity, route, account,
// and quantity are structural and must invalidate a captured evidence binding.
func samePortfolioStructure(existing, next *RawPosition) bool {
	if existing == nil || next == nil {
		return existing == next
	}
	return strings.EqualFold(strings.TrimSpace(existing.Account), strings.TrimSpace(next.Account)) &&
		existing.Position == next.Position &&
		sameContractIdentity(existing.Contract, next.Contract)
}

func sameContractIdentity(a, b Contract) bool {
	a = normalizedContractIdentity(a)
	b = normalizedContractIdentity(b)
	return reflect.DeepEqual(a, b)
}

func (c *Connection) lockEvidenceChange() func() {
	if c == nil || c.evidenceBarrier == nil {
		return func() {}
	}
	c.evidenceBarrier.RLock()
	return c.evidenceBarrier.RUnlock
}

func (c *Connection) lockBrokerScopeChange() func() {
	if c == nil {
		return func() {}
	}
	if c.publicationBarrier != nil {
		c.publicationBarrier.Lock()
	}
	if c.evidenceBarrier != nil {
		c.evidenceBarrier.RLock()
	}
	return func() {
		if c.evidenceBarrier != nil {
			c.evidenceBarrier.RUnlock()
		}
		if c.publicationBarrier != nil {
			c.publicationBarrier.Unlock()
		}
	}
}

// handleAccountSummary processes account summary updates from IBKR
func (c *Connection) handleAccountSummary(fields []string) {
	unlockEvidence := c.lockBrokerScopeChange()
	defer unlockEvidence()
	c.handleAccountSummaryUnderBrokerScopeLease(fields)
}

// handleAccountSummaryUnderBrokerScopeLease applies one summary row while the
// owns the exact-generation read lease in publication-before-inbound order.
func (c *Connection) handleAccountSummaryUnderBrokerScopeLease(fields []string) {

	// Expected fields:
	// 3: account

	if len(fields) < 7 {
		ibkrLogger.Warnf("[WARNING] Invalid account summary message: expected at least 7 fields, got %d", len(fields))
		return
	}

	tag := fields[4]
	value := fields[5]
	currency := fields[6]
	account := strings.TrimSpace(fields[3])

	reqID, reqIDErr := strconv.Atoi(strings.TrimSpace(fields[2]))

	// A registered one-shot is an account-scoped authority read. Keep it
	// if any concrete row is outside the expected account. IBKR labels every
	// $LEDGER:ALL row as Account=All, including fields this client does not
	// model. Admit only the typed currency fields and ignore the other aggregate
	// rows; none of them may enter or invalidate the account-scoped snapshot.
	c.accountMu.Lock()
	key := tag
	if currency != "" && currency != "BASE" {
		key = fmt.Sprintf("%s_%s", tag, currency)
	}
	if reqIDErr == nil {
		if snap := c.summarySnapshots[reqID]; snap != nil {
			disposition := accountSummaryRequestRowDisposition(account, tag, currency, snap.expectedAccount, c.managedAccounts)
			if snap.scopeConflict || disposition == accountSummaryRowReject {
				snap.scopeConflict = true
				c.accountMu.Unlock()
				return
			}
			if disposition == accountSummaryRowIgnore {
				c.accountMu.Unlock()
				return
			}
			if disposition == accountSummaryRowAcceptLedger {
				// Ledger rows live in their own key namespace so a base-currency
				// slice can never overwrite the same-named account-level total
				// (or be overwritten by it) depending on wire arrival order.
				// Account=All inference and 10.47's own "$LEDGER-" wire label —
				field, _ := splitGatewayLedgerTag(tag)
				key = fmt.Sprintf("%s%s_%s", accountSummaryLedgerKeyPrefix, field, currency)
			}
			snap.observedRows++
			snap.values[key] = value
			c.accountMu.Unlock()
			return
		}
	}

	// Unregistered account-summary traffic is context only. It may seed a
	// single-account session, but a foreign concrete row can never overwrite
	// the account-bound shared cache.
	// A login carrying several accounts is never seeded from this path. Its
	// c.account is the managedAccounts aggregate, which is not a concrete code,
	// so the guard below used to fall through and adopt whichever account the
	// row named — routinely the unpinned sibling, because reqAccountSummary is
	// issued with group "All" and awaitAccountSummarySnapshot deregisters on
	// accountMismatchesConnected then read as a configured-vs-connected
	// divergence and refused every broker write. The aggregate is the state
	incoming, incomingOK := newAccountCode(account)
	bound, boundOK := newAccountCode(c.account)
	if !incomingOK || (boundOK && !bound.equal(incoming)) {
		c.accountMu.Unlock()
		return
	}
	if !boundOK {
		if newManagedAccountSet(c.managedAccounts).multiAccount() {
			c.accountMu.Unlock()
			return
		}
		c.account = account
	}
	c.accountSummary[key] = value
	c.accountMu.Unlock()

	// Log important values
	switch tag {
	case "NetLiquidation", "BuyingPower", "TotalCashValue", "GrossPositionValue":
		portfolioLogger.Debugf("%s: %s %s", tag, value, currency)
	}
}

type accountSummaryRowDisposition uint8

const (
	accountSummaryRowReject accountSummaryRowDisposition = iota
	accountSummaryRowAccept
	// accountSummaryRowAcceptLedger admits a row through the Account=All
	// reuses account-level tag names (UnrealizedPnL, RealizedPnL) for every
	// held currency's ledger slice, so under a shared key the account total
	// and the base-currency slice would overwrite each other in wire-arrival
	accountSummaryRowAcceptLedger
	accountSummaryRowIgnore
)

// accountSummaryRequestRowDisposition admits ordinary rows only from the
// expected concrete account. managed is the login's managedAccounts list, which
// decides what the other rows mean. For Account=All, it accepts only typed
// ledger fields and ignores every unmodeled aggregate field. Unregistered
// traffic never calls this helper and cannot seed the streaming cache through
// it.
func accountSummaryRequestRowDisposition(account, tag, currency, expectedAccount string, managed []string) accountSummaryRowDisposition {
	expectedAccount = strings.TrimSpace(expectedAccount)
	if !accountCodeConcrete(expectedAccount) {
		return accountSummaryRowReject
	}
	// Gateway 10.47's wire prefix is the gateway's own statement of ledger
	// round, every prefixed ledger row fails the allowlist and is dropped
	field, wirePrefixed := splitGatewayLedgerTag(tag)
	ledgerRow := currencyLedgerField(field) && concreteAccountSummaryLedgerCurrency(currency)
	account = strings.TrimSpace(account)
	if accountCodeConcrete(account) {
		if strings.EqualFold(account, expectedAccount) {
			if wirePrefixed && ledgerRow {
				// The gateway labeled the row a ledger slice and named the
				// pinned account: ledger namespace, fully attributed.
				return accountSummaryRowAcceptLedger
			}
			return accountSummaryRowAccept
		}
		// reqAccountSummary is issued with group "All", so a login carrying
		// several accounts answers with every one of them. A recognized
		// managedAccounts sibling is that expected traffic, not evidence the
		// read was misrouted: drop the row and let the pinned account's own
		// rows complete the snapshot. An account the login does not manage
		if code, ok := newAccountCode(account); ok && newManagedAccountSet(managed).contains(code) {
			return accountSummaryRowIgnore
		}
		return accountSummaryRowReject
	}
	if !strings.EqualFold(account, "All") {
		return accountSummaryRowReject
	}
	// An aggregate-labeled ledger row carries no account of its own. On a
	// single-account login that is unambiguous; on a multi-account login it
	// cannot be attributed to the pinned account, and admitting it would report
	// a sibling's currency exposure as the pinned account's — wire-prefixed
	// rows are withheld exactly like their bare twins.
	if ledgerRow && !newManagedAccountSet(managed).multiAccount() {
		return accountSummaryRowAcceptLedger
	}
	return accountSummaryRowIgnore
}

func concreteAccountSummaryLedgerCurrency(currency string) bool {
	if len(currency) != 3 || currency == "BASE" {
		return false
	}
	for i := range len(currency) {
		if currency[i] < 'A' || currency[i] > 'Z' {
			return false
		}
	}
	return true
}

// handlePortfolioValue handles portfolio position updates (from reqAccountUpdates)
func (c *Connection) handlePortfolioValue(fields []string) {
	unlockEvidence := c.lockEvidenceChange()
	defer unlockEvidence()
	// Expected fields for msgPortfolioValue (7):
	// 19: accountName

	if len(fields) < 20 {
		ibkrLogger.Warnf("[WARNING] Invalid portfolio value message: expected at least 20 fields, got %d", len(fields))
		c.portfolioProjectionMu.Lock()
		c.invalidatePortfolioGeneration(time.Now().UTC())
		c.portfolioProjectionMu.Unlock()
		return
	}
	c.portfolioProjectionMu.Lock()
	defer c.portfolioProjectionMu.Unlock()
	if !c.acceptPortfolioAccountFrame(fields[19], time.Now().UTC()) {
		return
	}

	// A malformed row invalidates the whole staged generation. Treating a bad
	// false all-clear at accountDownloadEnd.
	conID, conIDErr := strconv.Atoi(strings.TrimSpace(fields[2]))
	secType := strings.ToUpper(strings.TrimSpace(fields[4]))
	strikeRaw := strings.TrimSpace(fields[6])
	strike := 0.0
	var strikeErr error
	if strikeRaw != "" {
		strike, strikeErr = strconv.ParseFloat(strikeRaw, 64)
	}
	multiplierRaw := strings.TrimSpace(fields[8])
	multiplier := 1
	var multiplierErr error
	if multiplierRaw != "" {
		multiplier, multiplierErr = strconv.Atoi(multiplierRaw)
	}
	// IB omits multiplier — or encodes it as zero — on every frame whose
	// contract has no real multiplier: stocks, cash, bonds, bills, funds and
	// CFDs. One unit is the correct normalization for all of them, and the
	// downstream renderers already treat the field that way. Derivatives do
	// carry a real multiplier, so normalizing theirs to 1 would understate
	// the row; they still require an explicit positive value, and options
	// need an explicit strike.
	derivativeIdentity := secType == "OPT" || secType == "FOP" || secType == "WAR"
	requiresDerivativeTerms := secType == "OPT" || secType == "FOP"
	right := strings.ToUpper(strings.TrimSpace(fields[7]))
	requiresMultiplier := portfolioMultiplierIsContractual(secType)
	if !requiresMultiplier && multiplierErr == nil && multiplier == 0 {
		multiplier = 1
	}
	position, positionErr := strconv.ParseFloat(strings.TrimSpace(fields[13]), 64)
	marketPrice, marketPriceErr := strconv.ParseFloat(strings.TrimSpace(fields[14]), 64)
	marketValue, marketValueErr := strconv.ParseFloat(strings.TrimSpace(fields[15]), 64)
	averageCost, averageCostErr := strconv.ParseFloat(strings.TrimSpace(fields[16]), 64)
	unrealizedPNL, unrealizedPNLErr := strconv.ParseFloat(strings.TrimSpace(fields[17]), 64)
	realizedPNL, realizedPNLErr := strconv.ParseFloat(strings.TrimSpace(fields[18]), 64)
	if conIDErr != nil || conID <= 0 || strings.TrimSpace(fields[3]) == "" || secType == "" ||
		strikeErr != nil || !finiteFloat(strike) || (derivativeIdentity && strikeRaw == "") ||
		(requiresDerivativeTerms && (strings.TrimSpace(fields[5]) == "" || (right != "C" && right != "P"))) ||
		multiplierErr != nil || multiplier <= 0 || (requiresMultiplier && multiplierRaw == "") ||
		positionErr != nil || !finiteFloat(position) || marketPriceErr != nil || !finiteFloat(marketPrice) ||
		marketValueErr != nil || !finiteFloat(marketValue) || averageCostErr != nil || !finiteFloat(averageCost) ||
		unrealizedPNLErr != nil || !finiteFloat(unrealizedPNL) || realizedPNLErr != nil || !finiteFloat(realizedPNL) {
		c.invalidatePortfolioGeneration(time.Now().UTC())
		portfolioLogger.Warnf("Portfolio generation contained a malformed required field; generation discarded")
		return
	}

	contract := Contract{
		ConID:        conID,
		Symbol:       fields[3],
		SecType:      fields[4],
		Expiry:       fields[5],
		Strike:       strike,
		Right:        fields[7],
		Multiplier:   multiplier,
		PrimaryExch:  fields[9],
		Currency:     fields[10],
		LocalSymbol:  fields[11],
		TradingClass: fields[12],
	}

	key := cachedPositionKey(fields[19], contract)
	target := c.positions
	if c.portfolioStagingActive {
		target = c.portfolioStaging
	}
	if position == 0 {
		if c.portfolioStagingActive {
			delete(target, key)
		} else {
			c.positionsMu.Lock()
			_, changed := target[key]
			delete(target, key)
			if changed {
				c.advancePortfolioProjectionGeneration()
			}
			c.positionsMu.Unlock()
		}
		portfolioLogger.Debugf("Position closed: %s", key)
		return
	}

	next := &RawPosition{
		Account:       fields[19],
		Contract:      contract,
		Position:      position,
		MarketPrice:   marketPrice,
		MarketValue:   marketValue,
		AverageCost:   averageCost,
		UnrealizedPNL: unrealizedPNL,
		RealizedPNL:   realizedPNL,
	}
	if c.portfolioStagingActive {
		if existing := target[key]; existing != nil && existing.Contract.ConID == contract.ConID {
			next.Contract = mergeCachedPositionContract(existing.Contract, next.Contract)
		}
		target[key] = next
	} else {
		c.positionsMu.Lock()
		existing := target[key]
		if existing != nil && existing.Contract.ConID == contract.ConID {
			next.Contract = mergeCachedPositionContract(existing.Contract, next.Contract)
		}
		structuralChange := !samePortfolioStructure(existing, next)
		target[key] = next
		if structuralChange {
			c.advancePortfolioProjectionGeneration()
		}
		c.positionsMu.Unlock()
	}

	// Seed the option contract cache from portfolio data so SubscribeOption
	// exact Position stream when it names the same ConID. We never fabricate
	// and applyContractDetailLite only overwrites Exchange if the cache
	if contract.SecType == "OPT" && conID != 0 {
		cacheKey := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
		detail := ContractDetailsLite{
			Symbol:       contract.Symbol,
			SecType:      contract.SecType,
			Expiry:       contract.Expiry,
			Strike:       contract.Strike,
			Right:        contract.Right,
			Exchange:     "",
			PrimaryExch:  contract.PrimaryExch,
			ConID:        conID,
			LocalSymbol:  contract.LocalSymbol,
			TradingClass: contract.TradingClass,
		}
		c.optionContractMu.Lock()
		c.optionContractCache[cacheKey] = detail
		c.optionContractMu.Unlock()
	}

	portfolioLogger.Debugf("Updated: %s %.2f @ %.2f, PnL: %.2f",
		key, position, marketPrice, unrealizedPNL)
}

// portfolioMultiplierIsContractual reports whether a portfolio frame's
// secType names a contract that actually carries a multiplier. Only these
// may fail the generation for a missing or zero one: every other secType
// IB never intended to carry the field.
func portfolioMultiplierIsContractual(secType string) bool {
	switch secType {
	case "OPT", "FOP", "WAR", "FUT":
		return true
	}
	return false
}

// managedAccountMember reports whether account is one of the codes the
// gateway listed in msgManagedAccts. One TWS login can hold several
// unlinked accounts, and account updates can stream all of them.
func (c *Connection) managedAccountMember(account string) bool {
	account = strings.TrimSpace(account)
	if !accountCodeConcrete(account) {
		return false
	}
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()
	return slices.ContainsFunc(c.managedAccounts, func(managed string) bool {
		return strings.EqualFold(managed, account)
	})
}

// portfolioStreamAccount returns the concrete account the account-updates
// stream is bound to, or "" when no account-scoped subscribe has landed.
func (c *Connection) portfolioStreamAccount() string {
	c.portfolioHealthMu.RLock()
	defer c.portfolioHealthMu.RUnlock()
	bound := strings.TrimSpace(c.portfolioHealth.Account)
	if !accountCodeConcrete(bound) {
		return ""
	}
	return bound
}

func (c *Connection) acceptPortfolioAccountFrame(account string, observedAt time.Time) bool {
	account = strings.TrimSpace(account)
	// Snapshot the managed list before taking portfolioHealthMu: no path in
	// this file nests the two locks, and this keeps it that way.
	sibling := c.managedAccountMember(account)
	c.portfolioHealthMu.Lock()
	defer c.portfolioHealthMu.Unlock()
	if !c.portfolioHealth.ScopeConflictAt.IsZero() || !c.portfolioHealth.InvalidPayloadAt.IsZero() {
		return false
	}
	if !accountCodeConcrete(account) {
		c.latchPortfolioScopeConflictLocked(observedAt)
		return false
	}
	bound := strings.TrimSpace(c.portfolioHealth.Account)
	if accountCodeConcrete(bound) {
		if strings.EqualFold(account, bound) {
			return true
		}
		if sibling {
			// Steady state for an unlinked multi-account login: drop the row
			// and leave the stream healthy for the bound account.
			portfolioLogger.Debugf("Dropping portfolio frame for sibling managed account")
			return false
		}
		c.latchPortfolioScopeConflictLocked(observedAt)
		return false
	}
	c.portfolioHealth.Account = account
	return true
}

func (c *Connection) completePortfolioDownload(account string, completedAt time.Time) bool {
	unlockEvidence := c.lockEvidenceChange()
	defer unlockEvidence()
	c.portfolioProjectionMu.Lock()
	defer c.portfolioProjectionMu.Unlock()
	account = strings.TrimSpace(account)
	sibling := c.managedAccountMember(account)
	c.portfolioHealthMu.Lock()
	defer c.portfolioHealthMu.Unlock()
	if !c.portfolioHealth.ScopeConflictAt.IsZero() || !c.portfolioHealth.InvalidPayloadAt.IsZero() {
		c.portfolioStaging = nil
		c.portfolioStagingActive = false
		return false
	}
	if !accountCodeConcrete(account) {
		c.latchPortfolioScopeConflictLocked(completedAt)
		c.portfolioStaging = nil
		c.portfolioStagingActive = false
		return false
	}
	bound := strings.TrimSpace(c.portfolioHealth.Account)
	if accountCodeConcrete(bound) && !strings.EqualFold(account, bound) {
		if sibling {
			// A sibling account's end marker says nothing about the bound
			// account's download, which may still be staging.
			return false
		}
		c.latchPortfolioScopeConflictLocked(completedAt)
		c.portfolioStaging = nil
		c.portfolioStagingActive = false
		return false
	}
	if !accountCodeConcrete(bound) && accountCodeConcrete(account) {
		c.portfolioHealth.Account = account
	}
	if !c.portfolioStagingActive {
		return false
	}
	next := make(map[string]*RawPosition, len(c.portfolioStaging))
	maps.Copy(next, c.portfolioStaging)
	c.positionsMu.Lock()
	c.positions = next
	c.positionsMu.Unlock()
	c.portfolioStaging = nil
	c.portfolioStagingActive = false
	c.portfolioHealth.InitialCompletedAt = completedAt.UTC()
	c.advancePortfolioProjectionGenerationLocked()
	return true
}

// latchPortfolioScopeConflictLocked is called with portfolioHealthMu held.
// receiving rows at the account-update cadence until a resubscribe lands.
func (c *Connection) latchPortfolioScopeConflictLocked(observedAt time.Time) {
	changed := c.portfolioHealth.ScopeConflictAt.IsZero()
	if changed {
		portfolioLogger.Warnf("Portfolio stream named an account outside this login; stream health is unavailable until resubscribe")
	}
	c.portfolioHealth.RequestedAt = time.Time{}
	c.portfolioHealth.InitialCompletedAt = time.Time{}
	c.portfolioHealth.LastUpdateAt = time.Time{}
	c.portfolioHealth.ScopeConflictAt = observedAt.UTC()
	if changed {
		c.advancePortfolioProjectionGenerationLocked()
	}
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// invalidatePortfolioGeneration is called with portfolioProjectionMu held.
// It retains the last published rows only as context while ensuring neither a
// later end marker nor heartbeat can make the malformed generation current.
func (c *Connection) invalidatePortfolioGeneration(observedAt time.Time) {
	c.portfolioHealthMu.Lock()
	changed := c.portfolioHealth.InvalidPayloadAt.IsZero()
	c.portfolioHealth.RequestedAt = time.Time{}
	c.portfolioHealth.InitialCompletedAt = time.Time{}
	c.portfolioHealth.LastUpdateAt = time.Time{}
	c.portfolioHealth.InvalidPayloadAt = observedAt.UTC()
	if changed {
		c.advancePortfolioProjectionGenerationLocked()
	}
	c.portfolioHealthMu.Unlock()
	c.portfolioStaging = nil
	c.portfolioStagingActive = false
}

func (c *Connection) resetPortfolioStreamHealth(account string, requestedAt time.Time) {
	unlockEvidence := c.lockEvidenceChange()
	defer unlockEvidence()
	c.portfolioProjectionMu.Lock()
	defer c.portfolioProjectionMu.Unlock()
	c.portfolioHealthMu.Lock()
	generation := c.portfolioHealth.ProjectionGeneration
	c.portfolioHealth = PortfolioStreamHealth{Account: strings.TrimSpace(account), RequestedAt: requestedAt.UTC(), ProjectionGeneration: generation}
	c.advancePortfolioProjectionGenerationLocked()
	c.portfolioHealthMu.Unlock()
	c.portfolioStaging = make(map[string]*RawPosition)
	c.portfolioStagingActive = true
}

func (c *Connection) advancePortfolioProjectionGeneration() {
	c.portfolioHealthMu.Lock()
	c.advancePortfolioProjectionGenerationLocked()
	c.portfolioHealthMu.Unlock()
}

func (c *Connection) advancePortfolioProjectionGenerationLocked() {
	c.portfolioHealth.ProjectionGeneration++
}

// handleAccountValue handles account value updates (from reqAccountUpdates)
func (c *Connection) handleAccountValue(fields []string) {
	// Expected fields for msgAcctValue (6):
	// 5: accountName

	if len(fields) < 6 {
		ibkrLogger.Warnf("[WARNING] Invalid account value message: expected at least 6 fields, got %d", len(fields))
		return
	}

	key := fields[2]
	value := fields[3]
	currency := fields[4]
	account := strings.TrimSpace(fields[5])

	// Streaming account-value rows carry the account they belong to.
	// TWS shares the account-updates service across API clients and streams
	// every account of a multi-account login over the one subscription, so a
	// blindly clobbers the bound account's values in the shared map
	// (issue #12). Drop rows naming a different concrete account; rows with
	// an empty or aggregate account name pass through because single-account
	// The subscription's bound account is authoritative. c.account holds the
	// raw managedAccounts value, which is a comma-separated list for a
	// multi-account login and therefore never concrete — comparing against it
	// alone disabled this guard for exactly the logins that need it most
	// (issue #14: a sibling's zeroed batch and its JOINT AccountType
	// overwrote the pinned account's summary).
	bound := c.portfolioStreamAccount()
	c.accountMu.Lock()
	if !accountCodeConcrete(bound) {
		bound = strings.TrimSpace(c.account)
	}
	if accountCodeConcrete(account) && accountCodeConcrete(bound) && !strings.EqualFold(account, bound) {
		c.accountMu.Unlock()
		portfolioLogger.Debugf("Dropping %s update for foreign account %s (bound %s)", key, account, bound)
		return
	}
	mapKey := key
	if currency != "" && currency != "BASE" {
		mapKey = fmt.Sprintf("%s_%s", key, currency)
	}
	c.accountSummary[mapKey] = value
	c.accountMu.Unlock()

	// Log important values
	switch key {
	case "NetLiquidation", "BuyingPower", "TotalCashValue", "UnrealizedPnL", "RealizedPnL":
		portfolioLogger.Debugf("%s: %s %s", key, value, currency)
	}
}

func (c *Connection) dispatchStaleSystemNotice(fields []string, epoch uint64) {
	if len(fields) < 2 {
		return
	}
	note, err := parseSystemNotificationPayload([]byte(fields[1]))
	if err != nil {
		return
	}
	// Alias and recovery state belong to the current epoch. The exact-origin
	// Connector handler may use the numeric ID only to publish a stale order
	// receipt and latch uncertainty; it must reject all other mutations.
	c.dispatchSystemNotice(note, reqAliasEntry{}, epoch)
}

func (c *Connection) handleSystemNotificationAtEpoch(fields []string, epoch uint64) func() {
	if len(fields) < 2 {
		ibkrLogger.Warnf("[IBKR cid=%d] System notice: missing payload", c.config.ClientID)
		return nil
	}

	note, err := parseSystemNotificationPayload([]byte(fields[1]))
	if err != nil {
		ibkrLogger.Warnf("[IBKR cid=%d] System notice decode error: %v", c.config.ClientID, err)
		return nil
	}

	scope := "global"
	symbolAlias := ""
	aliasEntry := reqAliasEntry{}
	if note.tickerID >= 0 {
		if entry, ok := c.lookupReqAlias(int(note.tickerID)); ok {
			aliasEntry = entry
			symbolAlias = entry.symbol
			if symbolAlias != "" {
				label := symbolAlias
				if entry.secType != "" {
					label += " " + entry.secType
				}
				scope = fmt.Sprintf("reqID=%d (%s)", note.tickerID, label)
			} else {
				scope = fmt.Sprintf("reqID=%d", note.tickerID)
			}
		} else {
			scope = fmt.Sprintf("reqID=%d", note.tickerID)
		}
	}

	codeLabel := fmt.Sprintf("code=%d", note.code)
	if desc := getErrorDescription(note.code); desc != "" && !strings.Contains(note.message, desc) {
		codeLabel = fmt.Sprintf("code=%d (%s)", note.code, desc)
	}

	// Treat documented market-data error codes (300-399) as warnings so they
	shouldWarn := note.code == 200 || (note.code >= 300 && note.code < 400 && note.code != 366)
	if !shouldWarn {
		msgLower := strings.ToLower(note.message)
		// For non-cataloged codes, fall back to a substring check; this keeps
		shouldWarn = strings.Contains(msgLower, "error")
	}
	// Definition-missing on derivative/probe requests is routine, not a
	// directions when only one is listed (the fx cache absorbs the miss).
	// The requester classifies the rejection either way, so the wire echo
	// stale/zero-value warnings, so the wire echo is debug-grade.
	indicativeDisclaimer := note.code == 2129
	definitionProbe := note.code == 200 && (aliasEntry.secType == "OPT" || aliasEntry.secType == "CASH")
	upperMsg := strings.ToUpper(note.message)
	parserMisalign := strings.Contains(upperMsg, "MART") || strings.Contains(upperMsg, "'BOE") || strings.Contains(upperMsg, "\"BOE") || strings.Contains(upperMsg, " BOE")
	context := ""
	if parserMisalign {
		context = c.parserContext(symbolAlias)
	}

	msgText := note.message
	if context != "" {
		msgText = fmt.Sprintf("%s | frame=%s", msgText, context)
	}
	if note.code == 326 {
		c.setClientIDInUseErrorAtEpoch(epoch, note.message)
	}

	if note.timestamp.IsZero() {
		format := "[IBKR cid=%d] System notice %s %s: %s"
		args := []any{c.config.ClientID, scope, codeLabel, msgText}
		switch {
		case parserMisalign:
			ibkrLogger.Errorf(format, args...)
		case definitionProbe, indicativeDisclaimer:
			ibkrLogger.Debugf(format, args...)
		case shouldWarn:
			ibkrLogger.Warnf(format, args...)
		default:
			ibkrLogger.Infof(format, args...)
		}
		return c.dispatchSystemNotice(note, aliasEntry, epoch)
	}

	format := "[IBKR cid=%d] System notice %s %s @ %s: %s"
	args := []any{c.config.ClientID, scope, codeLabel, note.timestamp.UTC().Format(time.RFC3339), msgText}
	switch {
	case parserMisalign:
		ibkrLogger.Errorf(format, args...)
	case definitionProbe:
		ibkrLogger.Debugf(format, args...)
	case shouldWarn:
		ibkrLogger.Warnf(format, args...)
	default:
		ibkrLogger.Infof(format, args...)
	}
	return c.dispatchSystemNotice(note, aliasEntry, epoch)
}

func (c *Connection) setClientIDInUseErrorAtEpoch(epoch uint64, message string) {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	if c.brokerSessionEpoch.Load() != epoch {
		return
	}
	c.statusMu.Lock()
	c.lastError = clientIDInUseError(c.config.ClientID, message)
	c.statusMu.Unlock()
}

// Message encoding/decoding methods

// sendMessage sends a length-prefixed message with rate limiting
func (c *Connection) sendMessage(msg []byte) error {
	// Check connection status before queueing - reject if disconnecting
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if status != StatusConnected {
		return fmt.Errorf("cannot send message: connection status is %v", status)
	}

	// Use rate limiter for all messages
	return c.rateLimiter.Submit(RequestTypeGeneral, func() error {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		if c.writeInProgress.Load() {
			wireLogger.Errorf("CONCURRENT WRITE DETECTED: previous send still in progress")
		}
		c.writeInProgress.Store(true)
		defer c.writeInProgress.Store(false)

		lengthBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

		if err := c.waitForHandshakeReady(); err != nil {
			return err
		}
		return c.withTransport(false, func() error {
			if c.writer == nil {
				return fmt.Errorf("ibkr: send before writer initialised (connection state inconsistent)")
			}
			fields := c.decodeOutboundMessage(msg)
			msgID := determineMessageID(c.serverVersion, msg)

			c.logSuspiciousOutbound(msgID, fields)
			if c.wireTap != nil {
				c.wireTap.RecordOutbound(msgID, msg, fields)
			}

			// Debug: hex dump outgoing message
			c.logOutgoingMessageHex(msg)

			if _, err := c.writer.Write(lengthBytes); err != nil {
				return err
			}

			if _, err := c.writer.Write(msg); err != nil {
				return err
			}

			c.logPacketOutbound(msg)

			if err := c.writer.Flush(); err != nil {
				return err
			}
			if c.writer.Buffered() > 0 {
				wireLogger.Errorf("flush incomplete: %d bytes still buffered after Flush()", c.writer.Buffered())
			}

			return nil
		})
	})
}

// sendMessageWithType sends a message with specific request type for rate limiting
func (c *Connection) sendMessageWithType(msg []byte, reqType RequestType) error {
	return c.sendMessageWithTypeContext(context.Background(), msg, reqType)
}

func brokerSendError(err error, disposition SendDisposition, typed bool) error {
	if err == nil || !typed {
		return err
	}
	return WithSendDisposition(err, disposition)
}

func brokerSendMayHaveBeenWritten(err error) bool {
	switch SendDispositionOf(err) {
	case SendDispositionMayHaveWritten, SendDispositionUnknown:
		return true
	default:
		return false
	}
}

// sendMessageWithTypeContext sends a message with caller-owned cancellation
func (c *Connection) sendMessageWithTypeContext(ctx context.Context, msg []byte, reqType RequestType) error {
	return c.sendMessageWithTypeContextForEpoch(ctx, msg, reqType, 0, false)
}

func (c *Connection) sendMessageWithTypeContextForEpoch(ctx context.Context, msg []byte, reqType RequestType, epoch uint64, requireEpoch bool) error {
	return c.sendMessageWithTypeContextForEpochGuarded(ctx, msg, reqType, epoch, requireEpoch, nil)
}

// sendMessageWithTypeContextForEpochGuarded is the exact broker-instruction
// frame byte can be written. A guard failure is a definite pre-wire refusal
// and is never retried.
func (c *Connection) sendMessageWithTypeContextForEpochGuarded(ctx context.Context, msg []byte, reqType RequestType, epoch uint64, requireEpoch bool, guard func() error) error {
	// Broker instructions and epoch-bound authority reads must be exactly-once
	protectedSend := requireEpoch || reqType == RequestTypeOrder
	outboundState := c.outboundSessionState.Load()
	// Check connection status before queueing - reject if disconnecting
	c.statusMu.RLock()
	status := c.status
	c.statusMu.RUnlock()
	if status != StatusConnected {
		return brokerSendError(fmt.Errorf("cannot send message: connection status is %v", status), SendDispositionDefinitelyUnsent, protectedSend)
	}

	sendCtx := ctx
	cancel := func() {}
	if protectedSend {
		sendCtx, cancel = context.WithCancel(ctx)
	}
	var dispatchEntered atomic.Bool
	maxRetries := 3
	if protectedSend {
		maxRetries = 0
	}
	err := c.rateLimiter.SubmitWithRetriesContextFunc(sendCtx, reqType, func(dispatchCtx context.Context) error {
		dispatchEntered.Store(true)
		if err := dispatchCtx.Err(); err != nil {
			return brokerSendError(err, SendDispositionDefinitelyUnsent, protectedSend)
		}
		if err := c.waitForHandshakeReadyContext(dispatchCtx); err != nil {
			return brokerSendError(err, SendDispositionDefinitelyUnsent, protectedSend)
		}
		if err := dispatchCtx.Err(); err != nil {
			return brokerSendError(err, SendDispositionDefinitelyUnsent, protectedSend)
		}
		return c.withTransport(false, func() error {
			if protectedSend && c.publicationBarrier != nil {
				// Daemon connector publication takes the exclusive side before it
				// after pacing and transport admission: a queued sender must never
				c.publicationBarrier.RLock()
				defer c.publicationBarrier.RUnlock()
			}
			if protectedSend && c.evidenceBarrier != nil {
				// Structural portfolio/session writers take the read side. Holding
				// validated projection generation the first-byte authority.
				c.evidenceBarrier.Lock()
				defer c.evidenceBarrier.Unlock()
			}
			// Recheck cancellation while holding transportMu. If the limiter or
			// caller returned while this dispatch was queued, no later socket
			// write can escape after cancel closes sendCtx.
			if err := dispatchCtx.Err(); err != nil {
				return brokerSendError(err, SendDispositionDefinitelyUnsent, protectedSend)
			}
			if requireEpoch {
				// Epoch and namespace readiness are one allocator state. Reading
				c.reqIDMu.Lock()
				currentEpoch := c.brokerSessionEpoch.Load()
				ready := c.haveNextValidID
				c.reqIDMu.Unlock()
				if currentEpoch != epoch {
					return brokerSendError(fmt.Errorf("broker socket generation changed before request send"), SendDispositionDefinitelyUnsent, true)
				}
				if !ready {
					return brokerSendError(fmt.Errorf("broker id namespace not ready for request send"), SendDispositionDefinitelyUnsent, true)
				}
			}
			if c.writer == nil {
				return brokerSendError(fmt.Errorf("ibkr: send before writer initialised (connection state inconsistent)"), SendDispositionDefinitelyUnsent, protectedSend)
			}
			if protectedSend && (outboundState&1 != 0 || c.outboundSessionState.Load() != outboundState) {
				return brokerSendError(fmt.Errorf("broker outbound session changed before request send"), SendDispositionDefinitelyUnsent, true)
			}
			fields := c.decodeOutboundMessage(msg)
			msgID := determineMessageID(c.serverVersion, msg)

			c.logSuspiciousOutbound(msgID, fields)

			lengthBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

			// Debug: hex dump outgoing message
			c.logOutgoingMessageHex(msg)

			if guard != nil {
				if err := guard(); err != nil {
					return brokerSendError(fmt.Errorf("broker wire guard: %w", err), SendDispositionDefinitelyUnsent, protectedSend)
				}
			}
			// The outbound generation and invalidation flag are protected by
			// transportMu, which is still held here. This is the last lifecycle
			// read before the first byte and therefore the write linearization
			// point relative to Disconnect/connection loss.
			if err := dispatchCtx.Err(); err != nil {
				return brokerSendError(err, SendDispositionDefinitelyUnsent, protectedSend)
			}
			if protectedSend && (outboundState&1 != 0 || c.outboundSessionState.Load() != outboundState) {
				return brokerSendError(fmt.Errorf("broker outbound session changed before request send"), SendDispositionDefinitelyUnsent, true)
			}
			bufferedBefore := c.writer.Buffered()
			accepted := 0
			n, err := c.writer.Write(lengthBytes)
			accepted += n
			if err != nil {
				return brokerSendError(err, bufferedWriteDisposition(c.writer, bufferedBefore, accepted), protectedSend)
			}

			n, err = c.writer.Write(msg)
			accepted += n
			if err != nil {
				return brokerSendError(err, bufferedWriteDisposition(c.writer, bufferedBefore, accepted), protectedSend)
			}
			if c.wireTap != nil {
				c.wireTap.RecordOutbound(msgID, msg, fields)
			}

			c.logPacketOutbound(msg)

			if err := c.writer.Flush(); err != nil {
				return brokerSendError(err, bufferedWriteDisposition(c.writer, bufferedBefore, accepted), protectedSend)
			}
			return nil
		})
	}, maxRetries)
	// SubmitWithRetriesContext has its own completion deadline. Cancel its
	// child before returning so a queued dispatch cannot later reach the wire.
	cancel()
	if !protectedSend || err == nil {
		return err
	}
	if _, ok := errors.AsType[*SendDispositionError](err); ok {
		return err
	}
	// A limiter-level return after dispatch began is conservatively uncertain:
	// the transport callback may still be finishing. The caller must poison or
	// reconcile. A request that never entered is proven pre-wire because the
	// canceled child is rechecked under transportMu.
	if !dispatchEntered.Load() {
		return brokerSendError(err, SendDispositionDefinitelyUnsent, true)
	}
	return brokerSendError(err, SendDispositionUnknown, true)
}

// bufferedWriteDisposition distinguishes a writer refusal that accepted no
// failed flush; newly accepted bytes still buffered prove zero wire exposure.
func bufferedWriteDisposition(writer *bufio.Writer, bufferedBefore, accepted int) SendDisposition {
	if writer == nil || bufferedBefore != 0 {
		return SendDispositionMayHaveWritten
	}
	if accepted >= 0 && writer.Buffered() >= accepted {
		return SendDispositionDefinitelyUnsent
	}
	return SendDispositionMayHaveWritten
}

func (c *Connection) logOutgoingMessageHex(msg []byte) {
	if !c.logWireHex || len(msg) < 4 {
		return
	}

	var msgType int32
	var hasNullAfterType bool

	if c.serverVersion >= 100 && len(msg) >= 4 {
		msgType = int32(binary.BigEndian.Uint32(msg[:4]))
		if len(msg) > 4 && msg[4] == 0x00 {
			hasNullAfterType = true
		}
	}

	// Show first 80 bytes or entire message if shorter
	dumpLen := min(len(msg), 80)

	var hexStr strings.Builder
	for i := range dumpLen {
		hexStr.WriteString(fmt.Sprintf("%02x ", msg[i]))
		if (i+1)%16 == 0 {
			hexStr.WriteString("\n                ")
		}
	}
	if dumpLen < len(msg) {
		hexStr.WriteString(fmt.Sprintf("... (%d more bytes)", len(msg)-dumpLen))
	}

	log.Printf("[WIRE OUT] msgType=%d len=%d nullAfterType=%v\n                %s",
		msgType, len(msg), hasNullAfterType, hexStr.String())
}

// logSuspiciousOutbound inspects encoded payloads to highlight frames that
// frequently trigger IBKR MART/320 parser faults (e.g., reqID/conID set to 0).
type protocolWarning struct {
	Summary string
	Key     string
	Symbol  string
}

func (c *Connection) logSuspiciousOutbound(msgID int, fields []string) {
	if len(fields) == 0 {
		return
	}
	var warning protocolWarning
	var ok bool
	var category string

	switch msgID {
	case reqMktData:
		warning, ok = summarizeReqMktDataFields(fields)
		category = "reqMktData"
	case reqContractData:
		warning, ok = summarizeReqContractFields(fields)
		category = "reqContractData"
	case reqHistoricalData:
		warning, ok = summarizeReqHistoricalFields(fields)
		category = "reqHistoricalData"
	default:
		return
	}

	if !ok {
		return
	}

	if warning.Symbol != "" {
		c.recordSuspiciousSummary(warning.Symbol, warning.Summary)
	}

	if c.shouldLogSuspicious(warning.Key) {
		if warning.Symbol != "" {
			ibkrLogger.Warnf("[WARNING] Protocol misalignment for %s via %s: %s", warning.Symbol, category, warning.Summary)
		} else {
			ibkrLogger.Warnf("[WARNING] Protocol misalignment (%s): %s", category, warning.Summary)
		}
	}
}

func (c *Connection) recordSuspiciousSummary(symbol, summary string) {
	if symbol == "" || summary == "" {
		return
	}
	c.suspectMu.Lock()
	c.suspectSummaries[symbol] = summary
	c.suspectMu.Unlock()
}

func (c *Connection) latestSuspiciousSummary(symbol string) string {
	if symbol == "" {
		return ""
	}
	c.suspectMu.Lock()
	summary := c.suspectSummaries[symbol]
	c.suspectMu.Unlock()
	return summary
}

func (c *Connection) allSuspiciousSummaries() []string {
	c.suspectMu.Lock()
	defer c.suspectMu.Unlock()
	if len(c.suspectSummaries) == 0 {
		return nil
	}
	keys := make([]string, 0, len(c.suspectSummaries))
	for sym := range c.suspectSummaries {
		keys = append(keys, sym)
	}
	slices.Sort(keys)
	result := make([]string, 0, len(keys))
	for _, sym := range keys {
		result = append(result, fmt.Sprintf("%s: %s", sym, c.suspectSummaries[sym]))
	}
	return result
}

func (c *Connection) parserContext(symbol string) string {
	if symbol != "" {
		if summary := c.latestSuspiciousSummary(symbol); summary != "" {
			return summary
		}
	}
	if summaries := c.allSuspiciousSummaries(); len(summaries) > 0 {
		return strings.Join(summaries, "; ")
	}
	return ""
}

func (c *Connection) observeContractTiming(symbol string, elapsed time.Duration, resolved bool) {
	if symbol == "" || elapsed <= 0 {
		return
	}

	c.contractTimingMu.Lock()
	if prev, ok := c.contractTimings[symbol]; !ok || elapsed > prev {
		c.contractTimings[symbol] = elapsed
	}
	c.contractTimingMu.Unlock()

	if elapsed >= 500*time.Millisecond || !resolved {
		status := "resolved"
		if !resolved {
			status = "pending"
		}
		ibkrLogger.Infof("Contract detail latency %s: %s (%s)", symbol, elapsed, status)
	}
}

func (c *Connection) shouldLogSuspicious(key string) bool {
	if key == "" {
		return false
	}
	c.suspectMu.Lock()
	defer c.suspectMu.Unlock()
	if _, exists := c.suspectFlags[key]; exists {
		return false
	}
	c.suspectFlags[key] = struct{}{}
	return true
}

func summarizeReqMktDataFields(fields []string) (protocolWarning, bool) {
	if len(fields) < 8 {
		return protocolWarning{}, false
	}
	reqID := fieldValue(fields, 2)
	conID := fieldValue(fields, 3)
	symbol := fieldValue(fields, 4)
	exchange := fieldValue(fields, 10)
	primary := fieldValue(fields, 11)
	generic := fieldValue(fields, 18)
	snapshot := fieldValue(fields, 19)
	regSnap := fieldValue(fields, 20)
	if reqID != "0" && reqID != "" && conID != "0" {
		return protocolWarning{}, false
	}
	summary := fmt.Sprintf("reqID=%s conID=%s symbol=%s exch=%s primary=%s ticks=%s snap=%s regSnap=%s",
		reqID, conID, symbol, exchange, primary, generic, snapshot, regSnap)
	if conID == "0" {
		summary += " (contract details pending)"
	}
	key := fmt.Sprintf("mkt:%s:%s", symbol, conID)
	return protocolWarning{Summary: summary, Key: key, Symbol: symbol}, true
}

func summarizeReqContractFields(fields []string) (protocolWarning, bool) {
	// Contract detail REQUESTS are supposed to have conID=0 - that's how you ASK for the conID!
	// Only market data and historical requests with conID=0 are problematic.
	return protocolWarning{}, false
}

func summarizeReqHistoricalFields(fields []string) (protocolWarning, bool) {
	if len(fields) < 6 {
		return protocolWarning{}, false
	}
	reqID := fieldValue(fields, 1)
	if fieldValue(fields, 2) == "" {
		return protocolWarning{}, false
	}
	conID := fieldValue(fields, 2)
	symbol := fieldValue(fields, 3)
	whatToShow := fieldValue(fields, 19)
	if reqID != "0" && conID != "0" {
		return protocolWarning{}, false
	}
	summary := fmt.Sprintf("reqID=%s conID=%s symbol=%s what=%s", reqID, conID, symbol, whatToShow)
	if conID == "0" {
		summary += " (contract details pending)"
	}
	key := fmt.Sprintf("hist:%s:%s", symbol, reqID)
	return protocolWarning{Summary: summary, Key: key, Symbol: symbol}, true
}

func fieldValue(fields []string, idx int) string {
	if idx < 0 || idx >= len(fields) {
		return ""
	}
	return fields[idx]
}

func (c *Connection) logPacketOutbound(payload []byte) {
	c.packetLoggerMu.RLock()
	logger := c.packetLogger
	c.packetLoggerMu.RUnlock()
	if logger == nil || len(payload) == 0 {
		return
	}
	msgID := determineMessageID(c.serverVersion, payload)
	label := fmt.Sprintf("out msgID=%d", msgID)
	clone := make([]byte, len(payload))
	copy(clone, payload)
	logger.Outbound(label, clone)
}

func determineMessageID(serverVersion int, payload []byte) int {
	if len(payload) == 0 {
		return 0
	}
	if serverVersion >= 100 && len(payload) >= 4 {
		return int(binary.BigEndian.Uint32(payload[:4]))
	}
	idx := bytes.IndexByte(payload, '\x00')
	if idx == -1 {
		idx = len(payload)
	}
	id, err := strconv.Atoi(string(payload[:idx]))
	if err != nil {
		return -1
	}
	return id
}

func managedAccountsField(fields []string) string {
	if len(fields) <= 1 {
		return ""
	}
	if len(fields) > 2 {
		if _, err := strconv.Atoi(strings.TrimSpace(fields[1])); err == nil {
			return strings.TrimSpace(fields[2])
		}
	}
	return strings.TrimSpace(fields[1])
}

// accountCodeConcrete reports whether account names one concrete account
// code usable in account-scoped requests (reqAcctData, reqPnL). The
// aggregate "All" and comma-separated managedAccounts lists are session
// aggregates, not account codes — TWS rejects them with error 321.
func accountCodeConcrete(account string) bool {
	account = strings.TrimSpace(account)
	if account == "" || strings.EqualFold(account, "All") {
		return false
	}
	return !strings.ContainsAny(account, ", \t")
}

// parseManagedAccounts splits a raw msgManagedAccts value into its concrete
// account codes. Aggregates and empty entries are dropped.
func parseManagedAccounts(managed string) []string {
	var out []string
	for entry := range strings.SplitSeq(managed, ",") {
		entry = strings.TrimSpace(entry)
		if accountCodeConcrete(entry) {
			out = append(out, entry)
		}
	}
	return out
}

// firstConcreteAccountCode extracts a usable account code from a
// managedAccounts-style value: a concrete code passes through, a
// empty) yield "" — TWS resolves an empty acctCode to the session's
// account for single-account logins.
func firstConcreteAccountCode(account string) string {
	account = strings.TrimSpace(account)
	if strings.EqualFold(account, "All") {
		return ""
	}
	first, _, _ := strings.Cut(account, ",")
	first = strings.TrimSpace(first)
	if accountCodeConcrete(first) {
		return first
	}
	return ""
}

// readMessage reads a length-prefixed message
func (c *Connection) readMessage() ([]byte, error) {
	// Read message length (4 bytes)
	lengthBytes := make([]byte, 4)
	// Debug: Reading message length
	if _, err := io.ReadFull(c.reader, lengthBytes); err != nil {
		// Only log non-timeout errors (timeouts are expected when no messages)
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			connectLogger.Warnf("Client %d: Failed to read length: %v", c.config.ClientID, err)
		}
		return nil, err
	}

	msgLength := binary.BigEndian.Uint32(lengthBytes)
	// Debug: Message length = %d bytes

	if msgLength == 0 {
		return []byte{}, nil
	}

	// Sanity cap: 16 MB. Some IBKR responses can exceed the old 1 MB cap;
	// gateway that's gone rogue (or a wire that's been hijacked).
	if msgLength > 16*1024*1024 {
		return nil, fmt.Errorf("message too large: %d bytes", msgLength)
	}

	// Read message body
	msgBytes := make([]byte, msgLength)
	// Debug: Reading message body
	if _, err := io.ReadFull(c.reader, msgBytes); err != nil {
		connectLogger.Warnf("Client %d: Failed to read body: %v", c.config.ClientID, err)
		return nil, err
	}

	// Debug: Successfully read message
	return msgBytes, nil
}

func (c *Connection) resetHandshakeReady() {
	c.handshakeMu.Lock()
	c.handshakeReady = make(chan struct{})
	c.handshakeMu.Unlock()
}

func (c *Connection) signalHandshakeReady() {
	c.handshakeMu.RLock()
	ch := c.handshakeReady
	c.handshakeMu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (c *Connection) waitForHandshakeReady() error {
	return c.waitForHandshakeReadyContext(context.Background())
}

func (c *Connection) waitForHandshakeReadyContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.handshakeMu.RLock()
	ch := c.handshakeReady
	c.handshakeMu.RUnlock()
	if ch == nil {
		return fmt.Errorf("handshake readiness channel not initialized")
	}

	select {
	case <-ch:
		return nil
	default:
	}

	timeout := c.config.ConnectTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return fmt.Errorf("connection context closed before handshake ready: %w", c.ctx.Err())
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for handshake readiness")
	}
}

// encodeMsg encodes fields into IBKR message format.
// The IBKR protocol uses null-terminated fields within a length-prefixed frame.
// We strictly maintain field order per the TWS API reference (e.g., reqMktData v11)
func (c *Connection) encodeMsg(fields ...any) []byte {
	var buf bytes.Buffer

	for i, field := range fields {
		if i == 0 && c.serverVersion >= 100 {
			// For v100+: encode msgID as 4-byte binary, NO null terminator
			switch v := field.(type) {
			case int:
				binary.Write(&buf, binary.BigEndian, int32(v))
			case int32:
				binary.Write(&buf, binary.BigEndian, v)
			case int64:
				binary.Write(&buf, binary.BigEndian, int32(v))
			default:
				ibkrLogger.Warnf("Non-integer message type %T: %v", field, field)
				buf.WriteString(fmt.Sprintf("%v", v))
				buf.WriteByte('\x00')
			}
			continue
		}

		switch v := field.(type) {
		case int:
			buf.WriteString(strconv.Itoa(v))
		case int64:
			buf.WriteString(strconv.FormatInt(v, 10))
		case float64:
			buf.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		case string:
			buf.WriteString(v)
		case bool:
			if v {
				buf.WriteString("1")
			} else {
				buf.WriteString("0")
			}
		default:
			buf.WriteString(fmt.Sprintf("%v", v))
		}
		buf.WriteByte('\x00')
	}

	return append([]byte(nil), buf.Bytes()...)
}

func ensureASCII(label, value string) error {
	if value == "" {
		return nil
	}
	if !isASCIIPrintable(value) {
		return fmt.Errorf("%s contains non-ASCII characters", label)
	}
	return nil
}

func isASCIIPrintable(s string) bool {
	for _, r := range s {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

// decodeMessage decodes an IBKR message payload into trimmed string fields.
// Empty fields are dropped; exact-position tests should call decodeFields.
func (c *Connection) decodeMessage(msgBytes []byte) []string {
	if len(msgBytes) == 0 {
		return []string{}
	}

	if c.serverVersion >= 100 && len(msgBytes) >= 4 {
		msgType := binary.BigEndian.Uint32(msgBytes[:4])

		result := []string{strconv.Itoa(int(msgType))}
		remaining := msgBytes[4:]
		if msgType == uint32(msgSystemNotification) {
			result = append(result, string(remaining))
			return result
		}
		if c.serverVersion >= minServerVerProtoBufPlaceOrder {
			if fields, ok := summarizeInboundOrderProtoCallback(int(msgType), remaining); ok {
				return fields
			}
		}
		raw := bytes.SplitSeq(remaining, []byte{'\x00'})
		for field := range raw {
			result = append(result, string(field))
		}
		return result
	}

	var result []string
	raw := bytes.SplitSeq(msgBytes, []byte{'\x00'})
	for field := range raw {
		result = append(result, string(field))
	}
	return result
}

type systemNotification struct {
	tickerID                int64
	timestamp               time.Time
	code                    int
	message                 string
	advancedOrderRejectJSON string
}

func parseSystemNotificationPayload(payload []byte) (*systemNotification, error) {
	var note systemNotification
	buf := payload

	for len(buf) > 0 {
		tag, n := binary.Uvarint(buf)
		if n <= 0 {
			return nil, fmt.Errorf("invalid protobuf tag for system notification")
		}
		buf = buf[n:]
		fieldNum := int(tag >> 3)
		wireType := int(tag & 0x7)

		switch wireType {
		case 0: // varint
			val, m := binary.Uvarint(buf)
			if m <= 0 {
				return nil, fmt.Errorf("invalid varint for system notification field %d", fieldNum)
			}
			buf = buf[m:]
			switch fieldNum {
			case 1:
				if val == math.MaxUint64 {
					note.tickerID = -1
				} else {
					note.tickerID = int64(val)
				}
			case 2:
				note.timestamp = time.Unix(0, int64(val)*int64(time.Millisecond))
			case 3:
				note.code = int(val)
			}
		case 2: // length-delimited (message string)
			length, m := binary.Uvarint(buf)
			if m <= 0 {
				return nil, fmt.Errorf("invalid length for system notification field %d", fieldNum)
			}
			buf = buf[m:]
			if length > uint64(len(buf)) {
				return nil, fmt.Errorf("system notification field %d length overflow", fieldNum)
			}
			val := buf[:length]
			buf = buf[length:]
			switch fieldNum {
			case 4:
				note.message = string(val)
			case 5:
				note.advancedOrderRejectJSON = string(val)
			}
		default:
			return nil, fmt.Errorf("unsupported wire type %d in system notification", wireType)
		}
	}

	if note.message == "" {
		return nil, fmt.Errorf("system notification missing message text")
	}

	return &note, nil
}

// snapshotHandlers returns a copy of the handler list for a message ID.
// without racing handler removal from callbacks running under other locks.
func (c *Connection) snapshotHandlers(msgID int) []func([]string) {
	c.handlersMu.RLock()
	defer c.handlersMu.RUnlock()
	entries := c.msgHandlers[msgID]
	if len(entries) == 0 {
		return nil
	}
	fns := make([]func([]string), 0, len(entries))
	for _, entry := range entries {
		if entry.fn != nil {
			fns = append(fns, entry.fn)
		}
	}
	return fns
}

// dispatchHandlers invokes a stable copy of both legacy and epoch-aware
func (c *Connection) dispatchHandlers(msgID int, fields []string, epoch uint64) bool {
	c.handlersMu.RLock()
	entries := append([]handlerEntry(nil), c.msgHandlers[msgID]...)
	c.handlersMu.RUnlock()
	for _, entry := range entries {
		if entry.fnAtEpoch != nil {
			entry.fnAtEpoch(fields, epoch)
		} else if entry.fn != nil {
			entry.fn(fields)
		}
	}
	return len(entries) > 0
}

func (c *Connection) dispatchEpochHandlers(msgID int, fields []string, epoch uint64) bool {
	c.handlersMu.RLock()
	entries := append([]handlerEntry(nil), c.msgHandlers[msgID]...)
	c.handlersMu.RUnlock()
	called := false
	for _, entry := range entries {
		if entry.fnAtEpoch != nil {
			entry.fnAtEpoch(fields, epoch)
			called = true
		}
	}
	return called
}

// UnregisterHandler removes a previously registered handler for a message type.
func (c *Connection) UnregisterHandler(msgID int, handlerID uint64) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	entries := c.msgHandlers[msgID]
	if len(entries) == 0 {
		return
	}
	for i, entry := range entries {
		if entry.id == handlerID {
			entries = append(entries[:i], entries[i+1:]...)
			break
		}
	}
	if len(entries) == 0 {
		delete(c.msgHandlers, msgID)
	} else {
		c.msgHandlers[msgID] = entries
	}
}

// RequestContractDetails sends a request to retrieve contract details for a contract.
func (c *Connection) RequestContractDetails(contract Contract) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}

	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, err
	}
	if err := c.sendContractDetailsRequest(contract, reqID); err != nil {
		return 0, err
	}
	return reqID, nil
}

func (c *Connection) sendContractDetailsRequest(contract Contract, reqID int) error {
	return c.sendMessage(c.contractDetailsRequestMessage(contract, reqID))
}

// sendContractDetailsRequestContext is sendContractDetailsRequest with
// caller-owned cancellation and pacing priority while queued in the rate
// limiter. The chain prewarm and per-leg option resolution use it so a
// background-tagged fan-out rides the limiter's background lane instead of
// pre-booking the message bucket ahead of interactive reads.
func (c *Connection) sendContractDetailsRequestContext(ctx context.Context, contract Contract, reqID int) error {
	return c.sendMessageWithTypeContext(ctx, c.contractDetailsRequestMessage(contract, reqID), RequestTypeGeneral)
}

func (c *Connection) sendContractDetailsRequestForEpoch(ctx context.Context, contract Contract, reqID int, epoch uint64) error {
	return c.sendMessageWithTypeContextForEpoch(ctx, c.contractDetailsRequestMessage(contract, reqID), RequestTypeGeneral, epoch, true)
}

func (c *Connection) contractDetailsRequestMessage(contract Contract, reqID int) []byte {
	c.registerReqAlias(reqID, contract)

	// Handle strike field: IB API expects empty string (not "0") for non-option contracts
	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}
	multiplierField := ""
	if strings.EqualFold(contract.SecType, "OPT") && contract.Multiplier != 0 {
		multiplierField = strconv.Itoa(contract.Multiplier)
	}

	// Equity primary exchange must be empty during stock discovery (conID=0).
	// SPY/ETF options need SMART+ARCA to match TWS' SPY ARCA chain source.
	primaryField := ""
	if contract.PrimaryExch != "" && (contract.ConID != 0 || strings.EqualFold(contract.SecType, "OPT")) {
		primaryField = contract.PrimaryExch
	}

	// LocalSymbol and TradingClass can be empty during discovery.
	localSymbol := contract.LocalSymbol
	tradingClass := contract.TradingClass

	// Discovery requests need SMART defaults. An exact positive-ConID lookup
	exchangeField := contract.Exchange
	if contract.ConID == 0 {
		exchangeField = ifEmpty(exchangeField, "SMART")
	}

	fields := []any{
		reqContractData,
		8,     // version
		reqID, // request id
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField, // use empty string for stocks, actual value for options
		contract.Right,
		multiplierField,
		exchangeField,
		primaryField,
		ifEmpty(contract.Currency, "USD"),
		localSymbol,
		tradingClass,
		0, // includeExpired = false
		contract.SecIDType,
		contract.SecID,
		"", // issuerId (required for server >= 147, added for server 203)
	}

	msg := c.encodeMsg(fields...)

	// By-fields STK discovery legitimately sends primary='' with conID=0
	// (see primaryField above); trace the wire shape only under debug.
	if contract.SecType == "STK" && contract.PrimaryExch == "" && logging.LevelEnabled(logging.LevelDebug) {
		wireLogger.Debugf("[cid=%d] reqContractData by-fields STK discovery symbol=%s reqID=%d local=%q class=%q fields=%v",
			c.config.ClientID, contract.Symbol, reqID, localSymbol, tradingClass, c.decodeMessage(msg))
	}

	return msg
}

func ifEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Public API methods

// SetMarketDataType sets the market data type (live, delayed, etc.)
func (c *Connection) SetMarketDataType(dataType int) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Market data types:
	msg := c.encodeMsg(reqMarketDataType, 1, dataType)

	marketLogger.Infof("[cid=%d] Setting market data type to %d (1=Live, 3=Delayed)", c.config.ClientID, dataType)
	return c.sendMessage(msg)
}

// restoreFrozenMarketDataTypeUnlessCompeting restores frozen mode when safe.
func (c *Connection) restoreFrozenMarketDataTypeUnlessCompeting() error {
	c.competingMu.RLock()
	defer c.competingMu.RUnlock()
	if c.competingLiveSession {
		return nil
	}
	return c.SetMarketDataType(2)
}

func (c *Connection) setMarketDataTypeAtEpoch(dataType int, epoch uint64) error {
	msg := c.encodeMsg(reqMarketDataType, 1, dataType)
	return c.sendMessageWithTypeContextForEpochGuarded(context.Background(), msg, RequestTypeGeneral, epoch, true, nil)
}

// MarketDataType returns the current market data type for a reqID.
func (c *Connection) MarketDataType(reqID int) int {
	c.mktDataTypeMu.RLock()
	defer c.mktDataTypeMu.RUnlock()
	if v, ok := c.mktDataType[reqID]; ok {
		return v
	}
	return 0
}

func (c *Connection) recordHeartbeatAtEpoch(epoch uint64, heartbeatNano int64) bool {
	if c == nil || heartbeatNano <= 0 {
		return false
	}
	c.inboundEpochMu.RLock()
	defer c.inboundEpochMu.RUnlock()
	if epoch != c.BrokerSessionEpoch() {
		return false
	}
	c.lastHeartbeatNano.Store(heartbeatNano)
	return true
}

func (c *Connection) processMarketDataTypeAtEpoch(fields []string, epoch uint64) bool {
	if len(fields) < 4 {
		return false
	}
	rid, err := strconv.Atoi(fields[2])
	if err != nil {
		return false
	}
	dt, err := strconv.Atoi(fields[3])
	if err != nil {
		return false
	}
	c.inboundEpochMu.RLock()
	defer c.inboundEpochMu.RUnlock()
	if epoch != c.BrokerSessionEpoch() {
		return false
	}
	c.mktDataTypeMu.Lock()
	c.mktDataType[rid] = dt
	c.mktDataTypeMu.Unlock()
	ibkrLogger.Debugf("[cid=%d] MarketDataType notice: reqID=%d, type=%d", c.config.ClientID, rid, dt)
	return true
}

// PlaceOrder sends a placeOrder request using the v45+ wire format. The default
// but does not grant application submit authority. A nil error means the frame
// was written, not that IBKR accepted or finalized the order.
func (c *Connection) PlaceOrder(order *IBKROrder) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	return c.placeOrder(context.Background(), order, nil, nil)
}

// PlacePaperOrder validates gate against the connection and sends a paper
// not application submit authority. A nil error means the frame was written,
// not that IBKR accepted or finalized the order.
func (c *Connection) PlacePaperOrder(gate PaperOrderGate, order *IBKROrder) error {
	if err := gate.validateConnection(c); err != nil {
		return definitelyUnsent(err)
	}
	return c.placeOrder(context.Background(), order, nil, nil)
}

func (c *Connection) placePaperOrderForEpochGuarded(ctx context.Context, gate PaperOrderGate, order *IBKROrder, epoch uint64, guard func() error) error {
	if err := gate.validateConnection(c); err != nil {
		return definitelyUnsent(err)
	}
	return c.placeOrder(ctx, order, &epoch, guard)
}

func (c *Connection) placeOrderForEpochGuarded(ctx context.Context, order *IBKROrder, epoch uint64, guard func() error) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	return c.placeOrder(ctx, order, &epoch, guard)
}

func (c *Connection) placeOrder(ctx context.Context, order *IBKROrder, expectedEpoch *uint64, guard func() error) error {
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("broker order context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if order == nil {
		return definitelyUnsent(fmt.Errorf("order is nil"))
	}
	if !c.IsConnected() {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}
	if c.serverVersion > 0 && c.serverVersion < minServerVerProtoBufPlaceOrder {
		return definitelyUnsent(fmt.Errorf("server version %d is too old for placeOrder v45+ encoding; upgrade TWS/IB Gateway", c.serverVersion))
	}

	epoch, err := preparePlaceOrder(order, c, expectedEpoch)
	if err != nil {
		return definitelyUnsent(err)
	}
	if !order.Transmit {
		order.Transmit = true
	}
	order.WhatIf = false
	c.clearWhatIfOrderID(order.OrderID)

	if err := c.sendPlaceOrderFrameGuarded(ctx, order, epoch, guard); err != nil {
		return err
	}

	now := time.Now()
	c.ordersMu.Lock()
	order.Status = "Submitted"
	order.SubmittedTime = now
	if order.CreatedTime.IsZero() {
		order.CreatedTime = now
	}
	if order.Remaining == 0 {
		order.Remaining = order.TotalQty
	}
	c.openOrders[order.OrderID] = order
	c.ordersMu.Unlock()

	return nil
}

// CancelOrder sends a cancelOrder request for an existing order ID. The
// raw write but does not grant application cancel authority. A nil error means
// the frame was written, not that IBKR confirmed cancellation.
func (c *Connection) CancelOrder(orderID int) error {
	if !tradingEnabled {
		return definitelyUnsent(ErrTradingDisabled)
	}
	epoch, err := c.captureBrokerInstructionEpoch()
	if err != nil {
		return definitelyUnsent(err)
	}
	return c.cancelOrderForEpoch(orderID, epoch)
}

// CancelPaperOrder validates gate against the connection and sends a paper
// not application cancel authority. A nil error means the frame was written,
// not that IBKR confirmed cancellation.
func (c *Connection) CancelPaperOrder(gate PaperOrderGate, orderID int) error {
	if err := gate.validateConnection(c); err != nil {
		return definitelyUnsent(err)
	}
	epoch, err := c.captureBrokerInstructionEpoch()
	if err != nil {
		return definitelyUnsent(err)
	}
	return c.cancelOrderForEpoch(orderID, epoch)
}

func (c *Connection) cancelPaperOrderForEpochGuarded(ctx context.Context, gate PaperOrderGate, orderID int, epoch uint64, guard func() error) error {
	if err := gate.validateConnection(c); err != nil {
		return definitelyUnsent(err)
	}
	return c.cancelOrderForEpochGuarded(ctx, orderID, epoch, guard)
}

func (c *Connection) cancelOrderForEpoch(orderID int, epoch uint64) error {
	return c.cancelOrderForEpochGuarded(context.Background(), orderID, epoch, nil)
}

func (c *Connection) cancelOrderForEpochGuarded(ctx context.Context, orderID int, epoch uint64, guard func() error) error {
	if ctx == nil {
		return definitelyUnsent(fmt.Errorf("broker cancel context is nil"))
	}
	if err := ctx.Err(); err != nil {
		return definitelyUnsent(err)
	}
	if !c.IsConnected() {
		return definitelyUnsent(fmt.Errorf("not connected to IBKR"))
	}

	msg, err := c.encodeCancelOrderMessage(orderID)
	if err != nil {
		return definitelyUnsent(err)
	}
	if err := c.sendMessageWithTypeContextForEpochGuarded(ctx, msg, RequestTypeOrder, epoch, true, guard); err != nil {
		return err
	}

	return nil
}

func clonePlaceOrderFields() []string {
	fields := make([]string, len(placeOrderBaseFields))
	copy(fields, placeOrderBaseFields)
	return fields
}

func assignPlaceOrderFields(fields []string, order *IBKROrder) {
	setIntField(fields, 1, order.OrderID)
	setIntField(fields, 2, order.ConID)
	setStringField(fields, 3, order.Symbol)
	setStringField(fields, 4, order.SecType)
	setStringField(fields, 5, order.Expiry)
	if order.Strike != 0 {
		setFloatField(fields, 6, order.Strike)
	}
	setStringField(fields, 7, order.Right)
	setStringField(fields, 8, order.Multiplier)
	if order.Exchange != "" {
		setStringField(fields, 9, order.Exchange)
	}
	setStringField(fields, 10, order.PrimaryExch)
	if order.Currency != "" {
		setStringField(fields, 11, order.Currency)
	}
	setStringField(fields, 12, order.LocalSymbol)
	setStringField(fields, 13, order.TradingClass)
	setStringField(fields, 14, order.SecIDType)
	setStringField(fields, 15, order.SecID)
	setStringField(fields, 16, strings.ToUpper(order.Action))
	setIntField(fields, 17, order.TotalQty)
	setStringField(fields, 18, strings.ToUpper(order.OrderType))
	if order.OrderType != "MKT" && order.LmtPrice != 0 {
		setFloatField(fields, 19, order.LmtPrice)
	}
	if order.AuxPrice != 0 {
		setFloatField(fields, 20, order.AuxPrice)
	}
	setStringField(fields, 21, strings.ToUpper(order.TIF))
	setStringField(fields, 22, order.OcaGroup)
	setStringField(fields, 23, order.Account)
	if order.OpenClose != "" {
		setStringField(fields, 24, order.OpenClose)
	}
	setIntFieldWithZero(fields, 25, order.Origin)
	setStringField(fields, 26, order.OrderRef)
	setBoolField(fields, 27, order.Transmit)
	setIntFieldWithZero(fields, 28, order.ParentID)
	setBoolField(fields, 29, order.BlockOrder)
	setBoolField(fields, 30, order.SweepToFill)
	setIntField(fields, 31, order.DisplaySize)
	setIntFieldWithZero(fields, 32, order.TriggerMethod)
	setBoolField(fields, 33, order.OutsideRth)
	setBoolField(fields, 34, order.Hidden)
	if order.DiscretionaryAmt != 0 {
		setFloatField(fields, 36, order.DiscretionaryAmt)
	}
	setStringField(fields, 37, order.GoodAfterTime)
	setStringField(fields, 38, order.GoodTillDate)
	setStringField(fields, 39, order.FaGroup)
	setStringField(fields, 40, order.FaMethod)
	setStringField(fields, 41, order.FaPercentage)
	setStringField(fields, 42, order.FaProfile)
	setStringField(fields, 43, order.ModelCode)
	setIntFieldWithZero(fields, 44, order.ShortSaleSlot)
	setStringField(fields, 45, order.DesignatedLocation)
	if order.ExemptCode != 0 {
		setIntFieldWithZero(fields, 46, order.ExemptCode)
	}
	setIntFieldWithZero(fields, 47, order.OcaType)
	setStringField(fields, 48, order.Rule80A)
	setStringField(fields, 49, order.SettlingFirm)
	setBoolField(fields, 50, order.AllOrNone)
	setIntField(fields, 51, order.MinQty)
	if order.PercentOffset != 0 {
		setFloatField(fields, 52, order.PercentOffset)
	}
	setBoolField(fields, 53, order.ETradeOnly)
	setBoolField(fields, 54, order.FirmQuoteOnly)
	if order.NbboPriceCap != 0 {
		setFloatField(fields, 55, order.NbboPriceCap)
	}
	setIntField(fields, 56, order.AuctionStrategy)
	if order.StartingPrice != 0 {
		setFloatField(fields, 57, order.StartingPrice)
	}
	if order.StockRefPrice != 0 {
		setFloatField(fields, 58, order.StockRefPrice)
	}
	if order.Delta != 0 {
		setFloatField(fields, 59, order.Delta)
	}
	if order.StockRangeLower != 0 {
		setFloatField(fields, 60, order.StockRangeLower)
	}
	if order.StockRangeUpper != 0 {
		setFloatField(fields, 61, order.StockRangeUpper)
	}
	setBoolField(fields, 62, order.OverridePercentageConstraints)
	if order.Volatility != 0 {
		setFloatField(fields, 63, order.Volatility)
	}
	setIntField(fields, 64, order.VolatilityType)
	setStringField(fields, 65, order.DeltaNeutralOrderType)
	if order.DeltaNeutralAuxPrice != 0 {
		setFloatField(fields, 66, order.DeltaNeutralAuxPrice)
	}
	setIntField(fields, 67, order.DeltaNeutralConID)
	setStringField(fields, 68, order.DeltaNeutralSettlingFirm)
	setStringField(fields, 69, order.DeltaNeutralClearingAccount)
	setStringField(fields, 70, order.DeltaNeutralClearingIntent)
	setStringField(fields, 71, order.DeltaNeutralOpenClose)
	setBoolField(fields, 72, order.DeltaNeutralShortSale)
	setIntField(fields, 73, order.DeltaNeutralShortSaleSlot)
	setStringField(fields, 74, order.DeltaNeutralDesignatedLocation)
	setIntField(fields, 75, order.ContinuousUpdate)
	setIntField(fields, 76, order.ReferencePriceType)
	if order.TrailStopPrice != 0 {
		setFloatField(fields, 77, order.TrailStopPrice)
	}
	if order.TrailingPercent != 0 {
		setFloatField(fields, 78, order.TrailingPercent)
	}
	if order.BasisPoints != 0 {
		setFloatField(fields, 79, order.BasisPoints)
	}
	setIntField(fields, 80, order.BasisPointsType)
	setIntField(fields, 81, order.ScaleInitLevelSize)
	setIntField(fields, 82, order.ScaleSubsLevelSize)
	if order.ScalePriceIncrement != 0 {
		setFloatField(fields, 83, order.ScalePriceIncrement)
	}
	if order.ScalePriceAdjustValue != 0 {
		setFloatField(fields, 84, order.ScalePriceAdjustValue)
	}
	setIntField(fields, 85, order.ScalePriceAdjustInterval)
	if order.ScaleProfitOffset != 0 {
		setFloatField(fields, 86, order.ScaleProfitOffset)
	}
	setBoolField(fields, 87, order.ScaleAutoReset)
	setIntField(fields, 88, order.ScaleInitPosition)
	setIntField(fields, 89, order.ScaleInitFillQty)
	setBoolField(fields, 90, order.ScaleRandomPercent)
	setStringField(fields, 91, order.HedgeType)
	setStringField(fields, 92, order.HedgeParam)
	setBoolField(fields, 93, order.OptOutSmartRouting)
	setStringField(fields, 94, order.ClearingAccount)
	setStringField(fields, 95, order.ClearingIntent)
	setBoolField(fields, 96, order.NotHeld)
	setBoolField(fields, placeOrderFieldWhatIf, order.WhatIf)
}

func setStringField(fields []string, idx int, value string) {
	if idx >= len(fields) || value == "" {
		return
	}
	fields[idx] = value
}

func setIntField(fields []string, idx int, value int) {
	if idx >= len(fields) || value == 0 {
		return
	}
	fields[idx] = strconv.Itoa(value)
}

func setIntFieldWithZero(fields []string, idx int, value int) {
	if idx >= len(fields) {
		return
	}
	fields[idx] = strconv.Itoa(value)
}

func setFloatField(fields []string, idx int, value float64) {
	if idx >= len(fields) {
		return
	}
	fields[idx] = strconv.FormatFloat(value, 'f', -1, 64)
}

func setBoolField(fields []string, idx int, value bool) {
	if idx >= len(fields) {
		return
	}
	if value {
		fields[idx] = "1"
	} else {
		fields[idx] = "0"
	}
}

// GetNextOrderID reserves the next broker order ID after TWS has supplied a
// 32-bit broker namespace is exhausted; callers must not send an order then.
func (c *Connection) GetNextOrderID() int {
	id, _, err := c.reserveNextOrderID()
	if err != nil {
		return 0
	}
	return id
}

// reserveNextOrderID returns both the reserved ID and the exact socket epoch
func (c *Connection) reserveNextOrderID() (int, uint64, error) {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.reserveNextOrderIDLocked()
}

func (c *Connection) reserveNextOrderIDForEpoch(expectedEpoch uint64) (int, uint64, error) {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	if epoch := c.brokerSessionEpoch.Load(); epoch != expectedEpoch {
		return 0, epoch, fmt.Errorf("%w: broker socket generation changed before order ID reservation", ErrBrokerIDNamespaceConflict)
	}
	return c.reserveNextOrderIDLocked()
}

func (c *Connection) reserveNextOrderIDLocked() (int, uint64, error) {
	epoch := c.brokerSessionEpoch.Load()
	if !c.haveNextValidID || c.brokerIDExhausted {
		return 0, epoch, fmt.Errorf("broker order ID unavailable before nextValidId or after namespace exhaustion")
	}
	id := max(c.nextOrderID, c.reqIDSeq)
	if id <= 0 {
		id = 1
	}
	if id > maxProtoInt32 {
		c.brokerIDExhausted = true
		return 0, epoch, fmt.Errorf("broker order ID unavailable before nextValidId or after namespace exhaustion")
	}
	if id == maxProtoInt32 {
		c.brokerIDExhausted = true
	} else {
		c.nextOrderID = id + 1
	}
	c.reservedOrderIDs[id] = struct{}{}
	return id, epoch, nil
}

func (c *Connection) observeNextValidOrderID(id int) {
	if id <= 0 || id > maxProtoInt32 {
		return
	}
	c.reqIDMu.Lock()
	c.observeNextValidOrderIDLocked(id)
	c.reqIDMu.Unlock()
}

func (c *Connection) observeNextValidOrderIDAtEpoch(id int, epoch uint64) {
	if id <= 0 || id > maxProtoInt32 {
		return
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	if c.brokerSessionEpoch.Load() != epoch {
		return
	}
	c.observeNextValidOrderIDLocked(id)
}

func (c *Connection) observeNextValidOrderIDLocked(id int) {
	c.haveNextValidID = true
	if id > c.nextOrderID {
		c.nextOrderID = id
	}
	if c.reqIDSeq > c.nextOrderID {
		c.nextOrderID = c.reqIDSeq
	}
}

func (c *Connection) resetOrderIDReadiness() {
	c.inboundEpochMu.Lock()
	defer c.inboundEpochMu.Unlock()
	// The canonical order is inbound generation then evidence. msgErr handling
	// holds the inbound read side while its side effects may take evidence read
	// locks; reversing these here creates a writer-preference deadlock when an
	// evidence writer is already queued.
	unlockEvidence := c.lockEvidenceChange()
	defer unlockEvidence()
	c.reqIDMu.Lock()
	// Epoch, readiness, and reservation provenance are one state transition.
	// reset epoch, never a mixed combination.
	c.brokerSessionEpoch.Add(1)
	c.haveNextValidID = false
	clear(c.reservedOrderIDs)
	clear(c.reservedRequestIDs)
	c.reqIDMu.Unlock()
	c.invalidateUnstampedObservationAuthority()
	c.mktDataTypeMu.Lock()
	clear(c.mktDataType)
	c.mktDataTypeMu.Unlock()
	c.lastHeartbeatNano.Store(0)
}

// invalidateUnstampedObservationAuthority clears every connection-local
// account, portfolio, quote-routing, and subscription-budget observation that
// lacks an exact socket epoch in its public contract. A reused AutoReconnect
// briefly publishing the prior account/portfolio as current.
func (c *Connection) invalidateUnstampedObservationAuthority() {
	c.portfolioProjectionMu.Lock()
	c.positionsMu.Lock()
	clear(c.positions)
	clear(c.positionsSnapshot)
	clear(c.positionsSnapshotResult)
	c.positionsSnapshotActive = false
	c.positionsMu.Unlock()
	c.portfolioStaging = nil
	c.portfolioStagingActive = false
	c.portfolioHealthMu.Lock()
	generation := c.portfolioHealth.ProjectionGeneration + 1
	c.portfolioHealth = PortfolioStreamHealth{ProjectionGeneration: generation}
	c.portfolioHealthMu.Unlock()
	c.portfolioProjectionMu.Unlock()

	c.accountMu.Lock()
	c.account = ""
	c.managedAccounts = nil
	clear(c.accountSummary)
	clear(c.summarySnapshots)
	c.accountMu.Unlock()

	c.aliasMu.Lock()
	clear(c.reqAlias)
	c.aliasMu.Unlock()
	c.optionContractMu.Lock()
	clear(c.optionContractCache)
	c.optionContractMu.Unlock()
	c.competingMu.Lock()
	c.competingLiveSession = false
	c.competingMu.Unlock()

	c.marketDataSlotsMu.Lock()
	heldSlots := len(c.marketDataSlots)
	clear(c.marketDataSlots)
	c.marketDataSlotsMu.Unlock()
	if c.rateLimiter != nil {
		for range heldSlots {
			c.rateLimiter.ReleaseMarketDataSlot()
		}
	}
}

// captureBrokerInstructionEpoch binds an instruction without an allocator
// claim (currently cancelOrder) to the exact ready socket generation at the
// authority handoff. The returned epoch must be carried to the wire check.
func (c *Connection) captureBrokerInstructionEpoch() (uint64, error) {
	if c == nil {
		return 0, fmt.Errorf("no active connection")
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	epoch := c.brokerSessionEpoch.Load()
	if !c.haveNextValidID {
		return epoch, fmt.Errorf("broker id namespace not ready for instruction send")
	}
	return epoch, nil
}

// BrokerSessionEpoch identifies the current monotonically advancing socket generation.
func (c *Connection) BrokerSessionEpoch() uint64 {
	if c == nil {
		return 0
	}
	return c.brokerSessionEpoch.Load()
}

// BrokerIDNamespaceReady reports whether nextValidId established the shared
func (c *Connection) BrokerIDNamespaceReady() bool {
	if c == nil {
		return false
	}
	c.reqIDMu.Lock()
	ready := c.haveNextValidID && !c.brokerIDExhausted
	c.reqIDMu.Unlock()
	return ready
}

func (c *Connection) setOpenOrderSnapshotObserver(observer func(msgID int, fields []string, epoch uint64)) {
	c.openOrderObserverMu.Lock()
	c.openOrderObserver = observer
	c.openOrderObserverMu.Unlock()
}

func (c *Connection) observeInboundOrderIDAtEpoch(id int, expectedEpoch uint64) {
	if id <= 0 || id > maxProtoInt32 {
		return
	}
	c.reqIDMu.Lock()
	if c.brokerSessionEpoch.Load() != expectedEpoch {
		c.reqIDMu.Unlock()
		return
	}
	c.advanceBrokerIDPastLocked(id)
	c.reqIDMu.Unlock()
}

func (c *Connection) advanceBrokerIDPastLocked(id int) {
	if id <= 0 {
		return
	}
	delete(c.reservedOrderIDs, id)
	delete(c.reservedRequestIDs, id)
	if id >= maxProtoInt32 {
		c.brokerIDExhausted = true
		return
	}
	next := id + 1
	if c.reqIDSeq < next {
		c.reqIDSeq = next
	}
	if c.nextOrderID < next {
		c.nextOrderID = next
	}
}

func inboundOrderID(msgID int, fields []string) (int, bool) {
	if msgID != msgOpenOrder && msgID != msgOrderStatus {
		return 0, false
	}
	var raw string
	if len(fields) > 1 && fields[1] == "protobuf" {
		raw = summaryFieldValue(fields, "orderId=")
	} else {
		start := 1
		if len(fields) > 2 && isNumeric(fields[1]) && isNumeric(fields[2]) {
			start = 2
		}
		if len(fields) <= start {
			return 0, false
		}
		raw = fields[start]
	}
	id, err := strconv.Atoi(strings.TrimSpace(raw))
	return id, err == nil && id > 0 && id <= maxProtoInt32
}

func parseNextValidOrderID(fields []string) (int, bool) {
	for _, idx := range []int{2, 1} {
		if idx >= len(fields) {
			continue
		}
		raw := strings.TrimSpace(fields[idx])
		if raw == "" {
			continue
		}
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			continue
		}
		return id, true
	}
	return 0, false
}

func (c *Connection) markWhatIfOrderID(orderID int) {
	if orderID <= 0 {
		return
	}
	c.whatIfOrdersMu.Lock()
	defer c.whatIfOrdersMu.Unlock()
	if c.whatIfOrderIDs == nil {
		c.whatIfOrderIDs = make(map[int]struct{})
	}
	c.whatIfOrderIDs[orderID] = struct{}{}
}

func (c *Connection) clearWhatIfOrderID(orderID int) {
	if orderID <= 0 {
		return
	}
	c.whatIfOrdersMu.Lock()
	defer c.whatIfOrdersMu.Unlock()
	delete(c.whatIfOrderIDs, orderID)
}

// IsWhatIfOrderID reports whether orderID is currently reserved for broker
// WhatIf evaluation callbacks rather than a working broker order.
func (c *Connection) IsWhatIfOrderID(orderID int) bool {
	if c == nil || orderID <= 0 {
		return false
	}
	c.whatIfOrdersMu.Lock()
	defer c.whatIfOrdersMu.Unlock()
	_, ok := c.whatIfOrderIDs[orderID]
	return ok
}

// RequestMarketData subscribes to market data for a symbol within ctx.
func (c *Connection) RequestMarketData(ctx context.Context, symbol string) (int, error) {
	secType, exchange, currency, primaryExchange := classifySymbol(symbol)
	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	// Dual-class shares (BRK.B, BF.B) translate to IBKR's space-form
	// before going on the wire — see dualClassWireSymbol.
	wireSymbol := dualClassWireSymbol(symbol)
	if base, _, ok := FxPair(symbol); ok {
		wireSymbol = base
	}

	contract := Contract{
		Symbol:       wireSymbol,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	// For equities IBKR expects primary exchange blank unless explicitly requested.
	if contract.SecType == "STK" {
		contract.PrimaryExch = ""
	}

	return c.RequestMarketDataWithContract(ctx, contract, "100,101,104,106,165,221,233,236", false, false)
}

// RequestMarketDataWithContract issues reqMktData for contract within ctx.
func (c *Connection) RequestMarketDataWithContract(ctx context.Context, contract Contract, genericTicks string, snapshot bool, regulatorySnap bool) (int, error) {
	return c.requestMarketDataWithContract(ctx, contract, genericTicks, snapshot, regulatorySnap, nil)
}

func (c *Connection) requestMarketDataWithContract(ctx context.Context, contract Contract, genericTicks string, snapshot bool, regulatorySnap bool, beforeSend func(reqID int) func()) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestMarketData"); err != nil {
		return 0, err
	}
	if contract.Symbol == "" {
		return 0, fmt.Errorf("contract symbol is required for market data")
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if err := ensureASCII("symbol", contract.Symbol); err != nil {
		return 0, err
	}

	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, err
	}

	// Copy the contract to avoid caller mutations affecting queued send.
	contractCopy := contract
	c.registerReqAlias(reqID, contractCopy)

	fields := c.buildReqMktDataFields(contractCopy, reqID, genericTicks, snapshot, regulatorySnap)
	msg := c.encodeMsg(fields...)

	if err := c.acquireMarketDataSlot(ctx, reqID); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}
	var cleanup func()
	if beforeSend != nil {
		cleanup = beforeSend(reqID)
	}

	marketLogger.Debugf("Requesting market data for %s (ReqID: %d, SecType: %s, Exchange: %s, Primary: %s, ConID: %d)",
		contractCopy.Symbol, reqID, contractCopy.SecType, contractCopy.Exchange, contractCopy.PrimaryExch, contractCopy.ConID)

	if err := c.sendMessageWithTypeContext(ctx, msg, RequestTypeMarketData); err != nil {
		if cleanup != nil {
			cleanup()
		}
		c.releaseMarketDataSlot(reqID)
		return 0, fmt.Errorf("failed to request market data: %w", err)
	}

	return reqID, nil
}

// requestMarketDataWithContractForEpoch is the exact-socket form used by
// broker-write previews. It binds both request-ID allocation and the final
// transport check to expectedEpoch, preventing reconnect-crossing requests.
func (c *Connection) requestMarketDataWithContractForEpoch(ctx context.Context, contract Contract, genericTicks string, snapshot bool, regulatorySnap bool, expectedEpoch uint64, beforeSend func(reqID int) func()) (int, error) {
	return c.requestMarketDataWithContractForEpochMode(ctx, contract, genericTicks, snapshot, regulatorySnap, expectedEpoch, true, beforeSend)
}

// requestSharedMarketDataWithContractForEpoch is the reconnect-safe form used
// when a shared read subscription must be re-issued on its originating socket.
// Unlike broker-write evidence, a shared subscription may be replayed after reconnect.
func (c *Connection) requestSharedMarketDataWithContractForEpoch(ctx context.Context, contract Contract, genericTicks string, expectedEpoch uint64, beforeSend func(reqID int) func()) (int, error) {
	return c.requestMarketDataWithContractForEpochMode(ctx, contract, genericTicks, false, false, expectedEpoch, false, beforeSend)
}

func (c *Connection) requestMarketDataWithContractForEpochMode(ctx context.Context, contract Contract, genericTicks string, snapshot bool, regulatorySnap bool, expectedEpoch uint64, requireExactContract bool, beforeSend func(reqID int) func()) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestMarketData"); err != nil {
		return 0, err
	}
	if requireExactContract && contract.ConID <= 0 && !isExplicitSessionFXContract(contract) {
		return 0, fmt.Errorf("exact market-data contract requires positive ConID or explicit CASH/IDEALPRO pair")
	}
	if contract.Symbol == "" {
		return 0, fmt.Errorf("contract symbol is required for market data")
	}
	if contract.Currency == "" {
		contract.Currency = "USD"
	}
	if err := ensureASCII("symbol", contract.Symbol); err != nil {
		return 0, err
	}
	reqID, epoch, err := c.reserveNextRequestIDForEpoch(expectedEpoch)
	if err != nil {
		return 0, err
	}
	if _, err := c.claimRequestIDForEpoch(reqID, epoch); err != nil {
		return 0, err
	}
	contractCopy := contract
	c.registerReqAlias(reqID, contractCopy)
	msg := c.encodeMsg(c.buildReqMktDataFields(contractCopy, reqID, genericTicks, snapshot, regulatorySnap)...)
	if err := c.acquireMarketDataSlot(ctx, reqID); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}
	var cleanup func()
	if beforeSend != nil {
		cleanup = beforeSend(reqID)
	}
	if err := c.sendMessageWithTypeContextForEpoch(ctx, msg, RequestTypeMarketData, expectedEpoch, true); err != nil {
		if cleanup != nil {
			cleanup()
		}
		c.releaseMarketDataSlotAtEpoch(reqID, expectedEpoch)
		return 0, fmt.Errorf("failed to request exact market data: %w", err)
	}
	return reqID, nil
}

func (c *Connection) requireServerVersion(method string) error {
	if c.serverVersion == 0 {
		return fmt.Errorf("%s: server version not negotiated", method)
	}
	if c.serverVersion < minServerVersionRequired {
		return fmt.Errorf("%s: server version %d is too old (minimum: %d)", method, c.serverVersion, minServerVersionRequired)
	}
	return nil
}

func (c *Connection) buildReqMktDataFields(contract Contract, reqID int, genericTicks string, snapshot bool, regulatorySnap bool) []any {
	// All fields required for serverVersion >= 124
	// Per official IBKR API reqMktData message version 11

	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}

	multiplierField := ""
	if contract.Multiplier != 0 {
		multiplierField = strconv.Itoa(contract.Multiplier)
	}

	fields := []any{
		reqMktData,
		11, // message version
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField,
		contract.Right,
		multiplierField,
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass,
	}

	if contract.SecType == "BAG" {
		fields = append(fields, 0) // combo legs count
	}

	fields = append(fields,
		false, // deltaNeutral
		genericTicks,
		snapshot,
		regulatorySnap,
		"", // mktDataOptions (chart options)
	)

	return fields
}

// optionContractKey is the canonical OPRA-style identifier for an OPT
// only ever populated by the v2-read migration.
func optionContractKey(symbol, tradingClass, expiry string, strike float64, right string) string {
	return strings.ToUpper(strings.TrimSpace(symbol)) + "|" +
		strings.ToUpper(strings.TrimSpace(tradingClass)) + "|" +
		strings.TrimSpace(expiry) + "|" +
		strconv.FormatFloat(strike, 'f', 6, 64) + "|" +
		strings.ToUpper(strings.TrimSpace(right))
}

func applyContractDetailLite(detail ContractDetailsLite, contract *Contract) {
	if contract == nil {
		return
	}
	optionPrimaryHint := ""
	if strings.EqualFold(contract.SecType, "OPT") {
		optionPrimaryHint = optionUnderlyingPrimaryExchangeHint(contract.Symbol)
	}
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
	}
	if detail.TradingClass != "" {
		contract.TradingClass = detail.TradingClass
	}
	if optionPrimaryHint != "" {
		// Cached OPT details can contain the option listing venue as
		// PrimaryExch; during contract resolution SPY-style stock
		// options still want the underlying chain source hint. The
		// market-data request normalizer clears this field again once
		// a concrete option ConID is known.
		contract.PrimaryExch = optionPrimaryHint
	}
}

func normalizeResolvedOptionMarketDataContract(contract *Contract) {
	if contract == nil || !strings.EqualFold(contract.SecType, "OPT") || contract.ConID == 0 {
		return
	}
	// PrimaryExch is useful while resolving stock-option contracts
	contract.PrimaryExch = ""
}

// RequestHistoricalData submits an HMDS request for historical data and
// list past whatToShow mirrors the reqHistoricalData wire message
func (c *Connection) RequestHistoricalData(ctx context.Context, contract Contract, endDateTime, duration, barSize, whatToShow string, useRTH bool, includeExpired bool, formatDate int, keepUpToDate bool, beforeSend func(int)) (int, error) {
	return c.requestHistoricalDataWithIDGuard(ctx, contract, endDateTime, duration, barSize, whatToShow, useRTH, includeExpired, formatDate, keepUpToDate, nil, beforeSend)
}

// requestHistoricalDataWithIDGuard is the narrow internal form used when a
// monotonic broker namespace. The guard is bounded; it never guesses from
// broker prose or silently sends an id that the caller rejected.
func (c *Connection) requestHistoricalDataWithIDGuard(ctx context.Context, contract Contract, endDateTime, duration, barSize, whatToShow string, useRTH bool, includeExpired bool, formatDate int, keepUpToDate bool, requestIDAllowed func(int) bool, beforeSend func(int)) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestHistoricalData"); err != nil {
		return 0, err
	}

	// Defensive assertion: Prevent byte-shift MART/BOE errors by blocking conID=0 requests
	if contract.ConID == 0 {
		ibkrLogger.Errorf("[cid=%d] PROTOCOL VIOLATION: attempted historical request with conID=0 for symbol=%s exchange=%s (would cause MART byte-shift error at IBKR gateway)",
			c.config.ClientID, contract.Symbol, contract.Exchange)
		return 0, fmt.Errorf("PROTOCOL VIOLATION: attempted historical request with conID=0 for symbol=%s exchange=%s (would cause MART byte-shift error at IBKR gateway)",
			contract.Symbol, contract.Exchange)
	}

	duration = normalizeHistoricalDuration(duration)

	reqID, err := c.reserveRequestID(requestIDAllowed)
	if err != nil {
		return 0, err
	}

	multiplier := ""
	if contract.Multiplier != 0 {
		multiplier = strconv.Itoa(contract.Multiplier)
	}

	// Handle strike field: IB API expects empty string (not "0") for non-option contracts
	strikeField := ""
	if contract.Strike != 0 {
		strikeField = strconv.FormatFloat(contract.Strike, 'f', -1, 64)
	}

	fields := make([]any, 0, 34)
	fields = append(fields,
		reqHistoricalData,
		reqID,
		contract.ConID,
		contract.Symbol,
		contract.SecType,
		contract.Expiry,
		strikeField, // use empty string for stocks/indices, actual value for options
		contract.Right,
		multiplier,
		contract.Exchange,
		contract.PrimaryExch,
		contract.Currency,
		contract.LocalSymbol,
		contract.TradingClass, // Always sent (MIN_SERVER_VER_TRADING_CLASS=68 < 124)
	)

	fields = append(fields, includeExpired)
	if contract.SecIDType != "" || contract.SecID != "" {
		fields = append(fields, contract.SecIDType, contract.SecID)
	}

	fields = append(fields,
		endDateTime,
		barSize,    // IBKR API encodes barSizeSetting before durationStr (see twsapi v10)
		duration,   // durationStr follows barSizeSetting
		useRTH,     // useRTH flag is encoded before whatToShow
		whatToShow, // whatToShow string must follow useRTH
		formatDate,
	)

	if contract.SecType == "BAG" {
		fields = append(fields, 0) // combo legs count (unsupported)
	}

	// Always sent for serverVersion >= 124
	fields = append(fields, keepUpToDate)

	// Always sent (MIN_SERVER_VER_LINKING=70 < 124)
	fields = append(fields, "") // chart options (unused)

	msg := c.encodeMsg(fields...)

	// Enhanced diagnostics: Log contract details when wire hex logging is enabled
	if c.logWireHex {
		wireLogger.Debugf("[cid=%d] Historical reqID=%d conID=%d symbol=%s exchange=%s primary=%s fields=%d msgLen=%d",
			c.config.ClientID, reqID, contract.ConID, contract.Symbol, contract.Exchange, contract.PrimaryExch, len(fields), len(msg))
	}

	if beforeSend != nil {
		beforeSend(reqID)
	}

	if err := c.sendMessageWithTypeContext(ctx, msg, RequestTypeHistorical); err != nil {
		return 0, fmt.Errorf("failed to request historical data: %w", err)
	}

	return reqID, nil
}

// normalizeHistoricalDuration coerces legacy day-based durations into IBKR-compliant
// year tokens above the 365-day limit, preventing IBKR error 321.
func normalizeHistoricalDuration(duration string) string {
	parts := strings.Fields(strings.TrimSpace(duration))
	if len(parts) != 2 {
		return duration
	}

	value, err := strconv.Atoi(parts[0])
	if err != nil || value <= 0 {
		return duration
	}

	unit := strings.ToUpper(parts[1])
	switch unit {
	case "D", "DAY", "DAYS":
		if value > 365 {
			return formatHistoricalDuration(value)
		}
		return fmt.Sprintf("%d D", value)
	default:
		return duration
	}
}

// CancelHistoricalData cancels an active historical request and honors ctx
func (c *Connection) CancelHistoricalData(ctx context.Context, reqID int) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("historical request ID must be a positive signed 32-bit integer")
	}
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelHistoricalData, 1, reqID)
	return c.sendMessageWithTypeContext(ctx, msg, RequestTypeHistorical)
}

// RequestSecDefOptParams issues msg 78 (reqSecDefOptParams) to enumerate the
// option chain (expirations + strikes) for an underlying. The IBKR wire format
// wire so callers can register their per-request handler atomically.
func (c *Connection) RequestSecDefOptParams(underlyingSymbol, futFopExchange, underlyingSecType string, underlyingConId int, beforeSend func(int)) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if underlyingConId == 0 {
		return 0, fmt.Errorf("reqSecDefOptParams: underlying conID required")
	}

	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, err
	}

	msg := c.encodeMsg(
		reqSecDefOptParams,
		reqID,
		underlyingSymbol,
		futFopExchange,
		underlyingSecType,
		underlyingConId,
	)

	if beforeSend != nil {
		beforeSend(reqID)
	}

	if err := c.sendMessage(msg); err != nil {
		return 0, fmt.Errorf("failed to request sec def opt params: %w", err)
	}
	return reqID, nil
}

// RequestMarketDataWithPrimary subscribes to market data with an explicit
// primary-exchange hint, with ctx bounding contract resolution.
func (c *Connection) RequestMarketDataWithPrimary(ctx context.Context, symbol string, primaryExchange string) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestMarketDataWithPrimary"); err != nil {
		return 0, err
	}
	if err := ensureASCII("symbol", symbol); err != nil {
		return 0, err
	}
	if err := ensureASCII("primary exchange", primaryExchange); err != nil {
		return 0, err
	}

	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, err
	}

	// Determine security type and base exchange based on symbol
	secType, exchange, currency, primaryHint := classifySymbol(symbol)
	if primaryExchange == "" {
		primaryExchange = primaryHint
	}

	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	// Dual-class shares (BRK.B, BF.B) translate to IBKR's space-form
	// before going on the wire — see dualClassWireSymbol.
	wireSymbol := dualClassWireSymbol(symbol)
	if base, _, ok := FxPair(symbol); ok {
		wireSymbol = base
	}

	contract := Contract{
		Symbol:       wireSymbol,
		SecType:      secType,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	msg := c.encodeMsg(c.buildReqMktDataFields(contract, reqID, "100,101,104,106,165,221,233,236", false, false)...)

	if err := c.acquireMarketDataSlot(ctx, reqID); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}
	marketLogger.Debugf("Requesting market data for %s (ReqID: %d, SecType: %s, Exch: %s, Primary: %s)",
		symbol, reqID, secType, exchange, contract.PrimaryExch)

	if err := c.sendMessageWithType(msg, RequestTypeMarketData); err != nil {
		c.releaseMarketDataSlot(reqID)
		return 0, fmt.Errorf("failed to request market data: %w", err)
	}
	return reqID, nil
}

// RequestOptionsMarketData subscribes to market data for an option contract.
func (c *Connection) RequestOptionsMarketData(ctx context.Context, symbol string, expiry string, strike float64, right string) (int, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("not connected to IBKR")
	}
	if err := c.requireServerVersion("RequestOptionsMarketData"); err != nil {
		return 0, err
	}

	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, err
	}

	secType := "OPT"
	exchange := "SMART"
	primaryExchange := "CBOE"
	if hint := optionUnderlyingPrimaryExchangeHint(symbol); hint != "" {
		primaryExchange = hint
	}
	currency := "USD"

	expiryFormatted := expiry
	if len(expiry) == 10 && strings.Contains(expiry, "-") {
		expiryFormatted = strings.ReplaceAll(expiry, "-", "")
	}

	localSymbol, tradingClassHint := contractDisplayHints(symbol, secType)

	contract := Contract{
		Symbol:       symbol,
		SecType:      secType,
		Expiry:       expiryFormatted,
		Strike:       strike,
		Right:        strings.ToUpper(right),
		Multiplier:   100,
		Exchange:     exchange,
		PrimaryExch:  primaryExchange,
		Currency:     currency,
		LocalSymbol:  localSymbol,
		TradingClass: tradingClassHint,
	}

	if err := c.resolveOptionContract(ctx, &contract, 5*time.Second); err != nil {
		return 0, fmt.Errorf("resolve option contract failed: %w", err)
	}
	normalizeResolvedOptionMarketDataContract(&contract)

	msg := c.encodeMsg(c.buildReqMktDataFields(contract, reqID, "100,101,104,106,221,236", false, false)...)

	if err := c.acquireMarketDataSlot(ctx, reqID); err != nil {
		return 0, fmt.Errorf("market data subscription limit reached: %w", err)
	}

	marketLogger.Infof("Requesting options market data for %s %s %.2f %s (ReqID: %d)",
		symbol, expiryFormatted, strike, right, reqID)

	if err := c.sendMessageWithType(msg, RequestTypeMarketData); err != nil {
		c.releaseMarketDataSlot(reqID)
		return 0, fmt.Errorf("failed to request options market data: %w", err)
	}

	return reqID, nil
}

func (c *Connection) resolveOptionContract(ctx context.Context, contract *Contract, timeout time.Duration) error {
	if contract == nil {
		return fmt.Errorf("option contract is nil")
	}
	if contract.ConID != 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	if c.applyCachedOptionContract(contract) {
		return nil
	}

	var lastErr error
	for _, att := range optionContractResolutionAttempts(*contract) {
		if err := ctx.Err(); err != nil {
			return err
		}
		detail, err := c.fetchOptionContractDetail(ctx, att.Contract, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}
		if detail == nil || detail.ConID == 0 {
			lastErr = fmt.Errorf("contract details unavailable for option %s %s %.2f%s (exchange=%s)", contract.Symbol, contract.Expiry, contract.Strike, contract.Right, att.Label)
			continue
		}

		key := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
		applyContractDetailLite(*detail, contract)

		c.optionContractMu.Lock()
		c.optionContractCache[key] = *detail
		c.optionContractMu.Unlock()
		return nil
	}

	if lastErr != nil {
		return lastErr
	}

	return fmt.Errorf("contract details unavailable for option %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
}

func (c *Connection) applyCachedOptionContract(contract *Contract) bool {
	if c == nil || contract == nil {
		return false
	}
	key := optionContractKey(contract.Symbol, contract.TradingClass, contract.Expiry, contract.Strike, contract.Right)
	c.optionContractMu.RLock()
	cached, ok := c.optionContractCache[key]
	c.optionContractMu.RUnlock()
	if !ok || cached.ConID == 0 {
		return false
	}
	applyContractDetailLite(cached, contract)
	return true
}

type optionContractRouteAttempt struct {
	Contract Contract
	Label    string
}

func optionContractResolutionAttempts(contract Contract) []optionContractRouteAttempt {
	attempts := []optionContractRouteAttempt{{Contract: contract, Label: optionContractRouteLabel(contract)}}

	if contract.PrimaryExch != "" && !strings.EqualFold(contract.Exchange, contract.PrimaryExch) {
		alt := contract
		alt.Exchange = contract.PrimaryExch
		alt.PrimaryExch = ""
		attempts = append(attempts, optionContractRouteAttempt{Contract: alt, Label: optionContractRouteLabel(alt)})
	}

	if !strings.EqualFold(contract.Exchange, "CBOE") {
		alt := contract
		alt.Exchange = "CBOE"
		alt.PrimaryExch = ""
		attempts = append(attempts, optionContractRouteAttempt{Contract: alt, Label: optionContractRouteLabel(alt)})
	}

	if !strings.EqualFold(contract.Exchange, "SMART") {
		alt := contract
		alt.Exchange = "SMART"
		alt.PrimaryExch = ""
		attempts = append(attempts, optionContractRouteAttempt{Contract: alt, Label: optionContractRouteLabel(alt)})
	}

	seen := make(map[string]struct{})
	dedup := make([]optionContractRouteAttempt, 0, len(attempts))
	for _, att := range attempts {
		key := strings.ToUpper(att.Contract.Exchange) + "|" + strings.ToUpper(att.Contract.PrimaryExch)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		dedup = append(dedup, att)
	}
	return dedup
}

func optionContractRouteLabel(contract Contract) string {
	exchange := strings.ToUpper(strings.TrimSpace(contract.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(contract.PrimaryExch))
	if primary == "" {
		return exchange
	}
	return exchange + "+" + primary
}

// fetchContractDetailFirst returns the first contractData frame the gateway
func (c *Connection) fetchContractDetailFirst(ctx context.Context, contract Contract, timeout time.Duration) (*ContractDetailsLite, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	detailsCh := make(chan ContractDetailsLite, 1)
	serverVersion := c.serverVersion
	reqID, err := c.nextRequestID()
	if err != nil {
		return nil, err
	}
	dataHandlerID := c.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *lite:
			default:
			}
		}
	})
	defer c.UnregisterHandler(msgContractData, dataHandlerID)

	if err := c.sendContractDetailsRequest(contract, reqID); err != nil {
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case detail := <-detailsCh:
		return &detail, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("contract details timeout for %s", contract.Symbol)
	}
}

func (c *Connection) fetchOptionContractDetail(ctx context.Context, contract Contract, timeout time.Duration) (*ContractDetailsLite, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	detailsCh := make(chan ContractDetailsLite, 8)
	doneCh := make(chan struct{})

	serverVersion := c.serverVersion
	reqID, err := c.nextRequestID()
	if err != nil {
		return nil, err
	}

	dataHandlerID := c.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *lite:
			default:
			}
		}
	})

	endHandlerID := c.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		if id, err := strconv.Atoi(fields[2]); err == nil && id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})

	err = c.sendContractDetailsRequestContext(ctx, contract, reqID)
	if err != nil {
		c.UnregisterHandler(msgContractData, dataHandlerID)
		c.UnregisterHandler(msgContractDataEnd, endHandlerID)
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	defer c.UnregisterHandler(msgContractData, dataHandlerID)
	defer c.UnregisterHandler(msgContractDataEnd, endHandlerID)

	var selected *ContractDetailsLite
	prefer := func(candidate ContractDetailsLite) bool {
		if !optionDetailMatchesRequest(candidate, contract) {
			return false
		}
		if selected == nil {
			return true
		}
		// Prefer details that match the requested exchange or primary.
		if !strings.EqualFold(selected.Exchange, contract.Exchange) && strings.EqualFold(candidate.Exchange, contract.Exchange) {
			return true
		}
		if !strings.EqualFold(selected.PrimaryExch, contract.PrimaryExch) && strings.EqualFold(candidate.PrimaryExch, contract.PrimaryExch) {
			return true
		}
		return false
	}

	for {
		select {
		case detail := <-detailsCh:
			if prefer(detail) {
				// copy
				d := detail
				selected = &d
			}
		case <-doneCh:
			if selected != nil {
				return selected, nil
			}
			return nil, fmt.Errorf("contract details unavailable for option %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
		case <-timer.C:
			return nil, fmt.Errorf("timeout waiting for option contract details for %s %s %.2f%s", contract.Symbol, contract.Expiry, contract.Strike, contract.Right)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func optionDetailMatchesRequest(candidate ContractDetailsLite, contract Contract) bool {
	if candidate.ConID == 0 {
		return false
	}
	requestedClass := strings.TrimSpace(contract.TradingClass)
	if requestedClass != "" && !strings.EqualFold(candidate.TradingClass, requestedClass) {
		return false
	}
	return true
}

// PrewarmOptionChainResult reports per-expiry outcome of a bulk prewarm:
type PrewarmOptionChainResult struct {
	Expiry  string
	Cached  int
	Dropped int
	Elapsed time.Duration
	Err     error
}

// PrewarmOptionChain bulk-resolves an option chain by issuing one partial-
// is the technique TWS uses internally to populate a chain instantly:
// IBKR's reqContractDetails returns every listed strike × C/P for a given
// and sidesteps the IBKR per-account reqContractDetails throttle that
// semaphore (4) to avoid bursting the gateway. Failures are localised —
// one timed-out expiry does not fail the others; tradingClass disambiguates classes.
func (c *Connection) PrewarmOptionChain(
	ctx context.Context,
	symbol string,
	expiries []string,
	tradingClass string,
	timeout time.Duration,
) []PrewarmOptionChainResult {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	results := make([]PrewarmOptionChainResult, len(expiries))
	if len(expiries) == 0 {
		return results
	}

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, exp := range expiries {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()

			start := time.Now()
			cached, dropped, err := c.prewarmOneExpiry(ctx, symbol, exp, tradingClass, timeout)
			results[i] = PrewarmOptionChainResult{
				Expiry:  exp,
				Cached:  cached,
				Dropped: dropped,
				Elapsed: time.Since(start),
				Err:     err,
			}
		})
	}
	wg.Wait()
	return results
}

// prewarmOneExpiry issues one partial-Contract reqContractDetails for
// Wire shape leaves strike and right empty so IBKR returns the whole expiry.
func (c *Connection) prewarmOneExpiry(
	ctx context.Context,
	symbol, expiry, tradingClass string,
	timeout time.Duration,
) (int, int, error) {
	contract := Contract{
		Symbol:       symbol,
		SecType:      "OPT",
		Expiry:       expiry,
		Exchange:     "SMART",
		PrimaryExch:  optionUnderlyingPrimaryExchangeHint(symbol),
		Currency:     "USD",
		Multiplier:   100,
		TradingClass: tradingClass,
	}

	attempts := optionContractResolutionAttempts(contract)
	labels := make([]string, 0, len(attempts))
	var lastErr error
	for _, att := range attempts {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		labels = append(labels, att.Label)
		cached, dropped, err := c.prewarmOneExpiryAttempt(ctx, att.Contract, timeout)
		if cached > 0 || dropped > 0 {
			return cached, dropped, err
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return 0, 0, fmt.Errorf("prewarm %s %s class=%s route attempts %s: %w",
			symbol, expiry, tradingClass, strings.Join(labels, ","), lastErr)
	}
	return 0, 0, fmt.Errorf("prewarm %s %s class=%s returned zero contract details across route attempts %s",
		symbol, expiry, tradingClass, strings.Join(labels, ","))
}

func (c *Connection) prewarmOneExpiryAttempt(ctx context.Context, contract Contract, timeout time.Duration) (int, int, error) {
	detailsCh := make(chan ContractDetailsLite, 16_384)
	doneCh := make(chan struct{})
	var dropped atomic.Int32
	serverVersion := c.serverVersion
	reqID, err := c.nextRequestID()
	if err != nil {
		return 0, 0, err
	}

	dataHandlerID := c.RegisterHandler(msgContractData, func(fields []string) {
		if lite, ok := parseContractDetailsLite(fields, reqID, serverVersion); ok {
			select {
			case detailsCh <- *lite:
			default:
				dropped.Add(1)
			}
		}
	})
	endHandlerID := c.RegisterHandler(msgContractDataEnd, func(fields []string) {
		if len(fields) < 3 {
			return
		}
		if id, err := strconv.Atoi(fields[2]); err == nil && id == reqID {
			select {
			case doneCh <- struct{}{}:
			default:
			}
		}
	})
	defer c.UnregisterHandler(msgContractData, dataHandlerID)
	defer c.UnregisterHandler(msgContractDataEnd, endHandlerID)

	if err := c.sendContractDetailsRequestContext(ctx, contract, reqID); err != nil {
		return 0, int(dropped.Load()), fmt.Errorf("send reqContractDetails: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	cached := 0
	flush := func(d ContractDetailsLite) {
		// Only OPT frames with a real ConID and a usable (strike, right)
		if d.ConID == 0 || d.Strike <= 0 || d.Right == "" {
			return
		}
		if d.SecType != "" && d.SecType != "OPT" {
			return
		}
		// Preserve the gateway-returned listing venue: option ConIDs are
		// venue-specific, and a later reqMktData must pair the cached ConID
		key := optionContractKey(contract.Symbol, d.TradingClass, contract.Expiry, d.Strike, d.Right)
		c.optionContractMu.Lock()
		if existing, ok := c.optionContractCache[key]; ok && existing.ConID != 0 {
			// Don't overwrite a previously-resolved entry — keeps any
			// exchange-routing already determined.
			c.optionContractMu.Unlock()
			return
		}
		c.optionContractCache[key] = d
		c.optionContractMu.Unlock()
		cached++
	}

	for {
		select {
		case d := <-detailsCh:
			flush(d)
		case <-doneCh:
			// Drain any late frames that arrived just before contractDataEnd
			for {
				select {
				case d := <-detailsCh:
					flush(d)
				default:
					if n := dropped.Load(); n > 0 {
						return cached, int(n), fmt.Errorf("prewarm truncated after dropping %d contractData frames (cached %d)", n, cached)
					}
					return cached, 0, nil
				}
			}
		case <-timer.C:
			return cached, int(dropped.Load()), fmt.Errorf("prewarm timeout after %s (cached %d so far)", timeout, cached)
		case <-ctx.Done():
			return cached, int(dropped.Load()), ctx.Err()
		}
	}
}

// CancelMarketData cancels the market-data subscription identified by reqID.
func (c *Connection) CancelMarketData(reqID int) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("market-data request ID must be a positive signed 32-bit integer")
	}
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelMktData, 1, reqID)
	err := c.sendMessageWithType(msg, RequestTypeMarketData)

	// Release market data slot when canceling subscription. Idempotent —
	c.releaseMarketDataSlot(reqID)

	return err
}

func (c *Connection) cancelMarketDataForEpoch(ctx context.Context, reqID int, epoch uint64) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("market-data request ID must be a positive signed 32-bit integer")
	}
	msg := c.encodeMsg(cancelMktData, 1, reqID)
	err := c.sendMessageWithTypeContextForEpoch(ctx, msg, RequestTypeMarketData, epoch, true)
	c.releaseMarketDataSlotAtEpoch(reqID, epoch)
	return err
}

// RequestPositions requests current positions via the one-shot reqPositions
// wire path. Library-callable; the daemon prefers the streaming portfolio
// path through Connector.CachedPositions backed by RequestAccountUpdates.
func (c *Connection) RequestPositions() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// Begin an isolated generation. The streaming projection and its current
	c.portfolioProjectionMu.Lock()
	c.positionsSnapshot = make(map[string]*RawPosition)
	c.positionsSnapshotResult = nil
	c.positionsSnapshotActive = true
	c.portfolioProjectionMu.Unlock()

	// Clear the end channel to ensure we wait for new data
	select {
	case <-c.positionsEndChan:
	default:
	}

	msg := c.encodeMsg(reqPositions, "1")
	return c.sendMessage(msg)
}

// WaitForPositionsEnd waits for the matching msgPositionEnd frame after a
// RequestPositions call. Library-callable companion to RequestPositions
// (daemon uses the streaming path; see RequestPositions for details).
func (c *Connection) WaitForPositionsEnd(timeout time.Duration) error {
	select {
	case <-c.positionsEndChan:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for positions end")
	}
}

func (c *Connection) completePositionsSnapshot() {
	c.portfolioProjectionMu.Lock()
	defer c.portfolioProjectionMu.Unlock()
	if !c.positionsSnapshotActive {
		return
	}
	c.positionsSnapshotResult = make(map[string]*RawPosition, len(c.positionsSnapshot))
	maps.Copy(c.positionsSnapshotResult, c.positionsSnapshot)
	c.positionsSnapshot = nil
	c.positionsSnapshotActive = false
}

// summarySnapshot accumulates the account-summary rows for one
// reqAccountSummary request, keyed like the shared accountSummary map
// accountSummaryEnd for the request's reqID.
type summarySnapshot struct {
	values          map[string]string
	done            chan struct{}
	expectedAccount string
	observedRows    int
	scopeConflict   bool
}

type summarySnapshotResult struct {
	values        map[string]string
	scopeConflict bool
}

// registerSummarySnapshot opens a per-request accumulation for reqID.
// Must run before the request hits the wire so no row can be missed.
func (c *Connection) registerSummarySnapshot(reqID int, expectedAccount string) {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if c.summarySnapshots == nil {
		c.summarySnapshots = make(map[int]*summarySnapshot)
	}
	c.summarySnapshots[reqID] = &summarySnapshot{
		values:          make(map[string]string),
		done:            make(chan struct{}),
		expectedAccount: strings.TrimSpace(expectedAccount),
	}
}

// dropSummarySnapshot removes the per-request accumulation for reqID and
// rows only touch the shared map.
func (c *Connection) dropSummarySnapshot(reqID int) summarySnapshotResult {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	snap := c.summarySnapshots[reqID]
	delete(c.summarySnapshots, reqID)
	if snap == nil {
		return summarySnapshotResult{}
	}
	return summarySnapshotResult{values: snap.values, scopeConflict: snap.scopeConflict}
}

// signalSummaryEnd closes the per-request done channel for the reqID
// carried by an accountSummaryEnd message ([msgID, version, reqID]).
func (c *Connection) signalSummaryEnd(fields []string) {
	if len(fields) < 3 {
		return
	}
	reqID, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil {
		return
	}
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	snap := c.summarySnapshots[reqID]
	if snap == nil {
		return
	}
	select {
	case <-snap.done:
	default:
		close(snap.done)
	}
}

// RequestAccountSummary starts an account-summary request for reqID. An empty
// tags string requests the package's default set of account values.
func (c *Connection) RequestAccountSummary(reqID int, tags string) error {
	return c.RequestAccountSummaryForAccount(reqID, tags, c.GetAccountCode())
}

// RequestAccountSummaryForAccount starts one account-bound summary read. TWS
// still receives group "All" because account codes are not account-group
// names; every row must match expectedAccount before publication.
func (c *Connection) RequestAccountSummaryForAccount(reqID int, tags, expectedAccount string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}
	expectedAccount = strings.TrimSpace(expectedAccount)
	if !accountCodeConcrete(expectedAccount) {
		return ErrAccountSummaryScopeConflict
	}

	// If no tags specified, request all important ones
	if tags == "" {
		tags = "NetLiquidation,BuyingPower,TotalCashValue,GrossPositionValue,UnrealizedPnL,RealizedPnL"
	}
	if err := c.claimRequestID(reqID); err != nil {
		return err
	}

	c.registerSummarySnapshot(reqID, expectedAccount)

	// Clear the legacy end channel to ensure we wait for new data
	select {
	case <-c.acctSummaryEndChan:
	default:
	}

	// reqAccountSummary message:
	// 3: group ("All" to get all accounts)
	msg := c.encodeMsg(reqAccountSummary, "1", reqID, "All", tags)
	if err := c.sendMessage(msg); err != nil {
		c.dropSummarySnapshot(reqID)
		return err
	}
	return nil
}

// WaitForAccountSummaryEnd waits until an account-summary request completes or
// timeout elapses.
func (c *Connection) WaitForAccountSummaryEnd(timeout time.Duration) error {
	select {
	case <-c.acctSummaryEndChan:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timeout waiting for account summary end")
	}
}

// awaitAccountSummarySnapshot blocks until the gateway emits
// accountSummaryEnd for reqID (or timeout elapses) and returns only the
// accountSummary map is also fed by the streaming reqAccountUpdates
// subscription, so unrelated updates cannot overwrite this request snapshot.
func (c *Connection) awaitAccountSummarySnapshot(reqID int, timeout time.Duration) (map[string]string, error) {
	c.accountMu.RLock()
	snap := c.summarySnapshots[reqID]
	c.accountMu.RUnlock()
	if snap == nil {
		return nil, fmt.Errorf("no account summary request registered for reqID %d", reqID)
	}
	select {
	case <-snap.done:
		result := c.dropSummarySnapshot(reqID)
		if result.scopeConflict {
			return nil, ErrAccountSummaryScopeConflict
		}
		return result.values, nil
	case <-time.After(timeout):
		c.dropSummarySnapshot(reqID)
		return nil, fmt.Errorf("timeout waiting for account summary end")
	}
}

// CancelAccountSummary cancels the account-summary request identified by reqID.
func (c *Connection) CancelAccountSummary(reqID int) error {
	if reqID <= 0 || reqID > maxProtoInt32 {
		return fmt.Errorf("account-summary request ID must be a positive signed 32-bit integer")
	}
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(cancelAccountSummary, "1", reqID)
	return c.sendMessage(msg)
}

// GetPositions returns a detached map containing the current position cache.
func (c *Connection) GetPositions() map[string]*RawPosition {
	c.positionsMu.RLock()
	defer c.positionsMu.RUnlock()

	// Return a copy to prevent external modification
	result := make(map[string]*RawPosition)
	maps.Copy(result, c.positions)
	return result
}

// GetPositionsSnapshot returns the most recent complete reqPositions result.
// It is isolated from reqAccountUpdates; nil means no completed snapshot.
func (c *Connection) GetPositionsSnapshot() map[string]*RawPosition {
	c.portfolioProjectionMu.RLock()
	defer c.portfolioProjectionMu.RUnlock()
	if c.positionsSnapshotResult == nil {
		return nil
	}
	result := make(map[string]*RawPosition, len(c.positionsSnapshotResult))
	maps.Copy(result, c.positionsSnapshotResult)
	return result
}

// GetPositionsWithPortfolioHealth captures the cached portfolio rows and the
// stream receipts under one lock order and returns detached values.
func (c *Connection) GetPositionsWithPortfolioHealth() (map[string]*RawPosition, PortfolioStreamHealth) {
	c.portfolioProjectionMu.RLock()
	defer c.portfolioProjectionMu.RUnlock()
	c.positionsMu.RLock()
	c.portfolioHealthMu.RLock()
	result := make(map[string]*RawPosition, len(c.positions))
	maps.Copy(result, c.positions)
	health := c.portfolioHealth
	c.portfolioHealthMu.RUnlock()
	c.positionsMu.RUnlock()
	return result, health
}

// PortfolioProjectionGeneration returns the current structural portfolio
func (c *Connection) PortfolioProjectionGeneration() uint64 {
	if c == nil {
		return 0
	}
	c.portfolioProjectionMu.RLock()
	c.portfolioHealthMu.RLock()
	generation := c.portfolioHealth.ProjectionGeneration
	c.portfolioHealthMu.RUnlock()
	c.portfolioProjectionMu.RUnlock()
	return generation
}

// GetPosition returns the cached position for key, if present.
func (c *Connection) GetPosition(key string) (*RawPosition, bool) {
	c.positionsMu.RLock()
	defer c.positionsMu.RUnlock()

	pos, exists := c.positions[key]
	return pos, exists
}

// GetAccountCode returns the last known managed account code.
func (c *Connection) GetAccountCode() string {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()
	return c.account
}

// GetAccountSummary returns a detached copy of the current account-summary
func (c *Connection) GetAccountSummary() map[string]string {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()

	// Return a copy to prevent external modification
	result := make(map[string]string)
	maps.Copy(result, c.accountSummary)
	return result
}

// GetAccountValue returns the cached account value for key, if present.
func (c *Connection) GetAccountValue(key string) (string, bool) {
	c.accountMu.RLock()
	defer c.accountMu.RUnlock()

	value, exists := c.accountSummary[key]
	return value, exists
}

// RequestAccountUpdates subscribes to streaming account and portfolio updates
// for account.
func (c *Connection) RequestAccountUpdates(account string) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	// A multi-account login's managedAccounts value is a list, not a code.
	// PortfolioStreamHealth.Account and leave every concrete-account check
	bound := strings.TrimSpace(account)
	if !accountCodeConcrete(bound) {
		bound = strings.TrimSpace(c.GetAccountCode())
	}
	if !accountCodeConcrete(bound) {
		bound = ""
	}
	c.resetPortfolioStreamHealth(bound, time.Now().UTC())

	msg := c.encodeMsg(reqAcctData, "2", "1", account)
	return c.sendMessage(msg)
}

// RequestCurrentTime asks the gateway for its current time as a heartbeat.
func (c *Connection) RequestCurrentTime() error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected to IBKR")
	}

	msg := c.encodeMsg(reqCurrentTime, "1")
	return c.rateLimiter.SubmitWithRetries(RequestTypeHeartbeat, func() error {
		return c.withTransport(false, func() error {
			lengthBytes := make([]byte, 4)
			binary.BigEndian.PutUint32(lengthBytes, uint32(len(msg)))

			if _, err := c.writer.Write(lengthBytes); err != nil {
				return err
			}

			if _, err := c.writer.Write(msg); err != nil {
				return err
			}

			return c.writer.Flush()
		})
	}, 0) // No retries: heartbeat failures should surface immediately
}

// pauseTransport prevents non-handshake writers from accessing the socket.
func (c *Connection) pauseTransport() {
	if c.transportCond == nil {
		return
	}
	c.transportMu.Lock()
	c.transportPaused = true
	c.transportMu.Unlock()
}

// beginOutboundSession invalidates the prior outbound socket generation and
// generation may be published only by activateOutboundSession.
func (c *Connection) beginOutboundSession() uint64 {
	state := c.publishRevokedOutboundSession()
	if c.transportCond == nil {
		return state
	}
	c.transportMu.Lock()
	c.transportPaused = true
	c.transportMu.Unlock()
	return state
}

// activateOutboundSession publishes a successfully handshaken generation.
func (c *Connection) activateOutboundSession(state uint64) bool {
	if c.transportCond == nil {
		return state&1 != 0 && c.outboundSessionState.CompareAndSwap(state, state&^uint64(1))
	}
	c.transportMu.Lock()
	defer c.transportMu.Unlock()
	if state&1 == 0 || !c.outboundSessionState.CompareAndSwap(state, state&^uint64(1)) {
		return false
	}
	c.transportPaused = false
	c.transportCond.Broadcast()
	return true
}

// invalidateOutboundSession revokes protected-write authority under the same
func (c *Connection) invalidateOutboundSession(pause bool) {
	// Publish revocation before waiting for a writer that already owns
	c.publishRevokedOutboundSession()
	if c.transportCond == nil {
		return
	}
	c.transportMu.Lock()
	c.transportPaused = pause
	if !pause {
		c.transportCond.Broadcast()
	}
	c.transportMu.Unlock()
}

func (c *Connection) publishRevokedOutboundSession() uint64 {
	for {
		old := c.outboundSessionState.Load()
		nextGeneration := (old >> 1) + 1
		next := (nextGeneration << 1) | 1
		if c.outboundSessionState.CompareAndSwap(old, next) {
			return next
		}
	}
}

// resumeTransport unblocks any goroutines waiting to send IBKR messages.
func (c *Connection) resumeTransport() {
	if c.transportCond == nil {
		return
	}
	c.transportMu.Lock()
	if !c.transportPaused {
		c.transportMu.Unlock()
		return
	}
	c.transportPaused = false
	c.transportCond.Broadcast()
	c.transportMu.Unlock()
}

// withTransport provides exclusive, sequential access to the underlying writer.
func (c *Connection) withTransport(allowDuringPause bool, fn func() error) error {
	if c.transportCond == nil {
		return fn()
	}
	c.transportMu.Lock()
	for c.transportPaused && !allowDuringPause {
		c.transportCond.Wait()
	}
	defer c.transportMu.Unlock()
	return fn()
}

// RegisterHandler adds a handler for msgID and returns the identifier accepted
func (c *Connection) RegisterHandler(msgID int, handler func([]string)) uint64 {
	if handler == nil {
		return 0
	}
	c.handlersMu.Lock()
	c.handlerSeq++
	entry := handlerEntry{id: c.handlerSeq, fn: handler}
	c.msgHandlers[msgID] = append(c.msgHandlers[msgID], entry)
	c.handlersMu.Unlock()
	return entry.id
}

// RegisterHandlerAtEpoch adds a handler that receives the socket epoch that
// the Connection reader observed; register before the corresponding request.
func (c *Connection) RegisterHandlerAtEpoch(msgID int, handler func([]string, uint64)) uint64 {
	if handler == nil {
		return 0
	}
	c.handlersMu.Lock()
	c.handlerSeq++
	entry := handlerEntry{id: c.handlerSeq, fnAtEpoch: handler}
	c.msgHandlers[msgID] = append(c.msgHandlers[msgID], entry)
	c.handlersMu.Unlock()
	return entry.id
}

// GetNextRequestID reserves and returns the next connection-local request ID.
// broker error can never be reinterpreted as a later order event. Zero means
// the signed 32-bit broker namespace is exhausted.
func (c *Connection) GetNextRequestID() int {
	reqID, _, err := c.reserveNextRequestID()
	if err != nil {
		return 0
	}
	return reqID
}

func (c *Connection) reserveNextRequestID() (int, uint64, error) {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.reserveNextRequestIDLocked()
}

func (c *Connection) reserveNextRequestIDForEpoch(expectedEpoch uint64) (int, uint64, error) {
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	if epoch := c.brokerSessionEpoch.Load(); epoch != expectedEpoch {
		return 0, epoch, fmt.Errorf("%w: broker socket generation changed before request ID reservation", ErrBrokerIDNamespaceConflict)
	}
	return c.reserveNextRequestIDLocked()
}

func (c *Connection) reserveNextRequestIDLocked() (int, uint64, error) {
	epoch := c.brokerSessionEpoch.Load()
	if !c.haveNextValidID || c.brokerIDExhausted {
		return 0, epoch, fmt.Errorf("broker request/order id namespace unavailable")
	}
	reqID := max(c.reqIDSeq, c.nextOrderID)
	if reqID <= 0 {
		reqID = 1
	}
	if reqID > maxProtoInt32 {
		c.brokerIDExhausted = true
		return 0, epoch, fmt.Errorf("broker request/order id namespace unavailable")
	}
	if reqID == maxProtoInt32 {
		c.brokerIDExhausted = true
	} else {
		c.reqIDSeq = reqID + 1
	}
	c.reservedRequestIDs[reqID] = struct{}{}
	return reqID, epoch, nil
}

func (c *Connection) nextRequestID() (int, error) {
	id, epoch, err := c.reserveNextRequestID()
	if err != nil {
		return 0, err
	}
	if _, err := c.claimRequestIDForEpoch(id, epoch); err != nil {
		return 0, err
	}
	return id, nil
}

func (c *Connection) nextRequestIDForForwarding() (int, error) {
	id, _, err := c.nextRequestIDForForwardingWithEpoch()
	return id, err
}

func (c *Connection) nextRequestIDForForwardingWithEpoch() (int, uint64, error) {
	return c.reserveNextRequestID()
}

// discardRequestIDReservation drops retry provenance without moving the
// frontier backward. It is used when an internally generated request ID never
// reaches the Connection claim/send boundary.
func (c *Connection) discardRequestIDReservation(id int) {
	if c == nil || id <= 0 {
		return
	}
	c.reqIDMu.Lock()
	delete(c.reservedRequestIDs, id)
	c.reqIDMu.Unlock()
}

func (c *Connection) discardOrderIDReservation(id int) {
	if c == nil || id <= 0 {
		return
	}
	c.reqIDMu.Lock()
	delete(c.reservedOrderIDs, id)
	c.reqIDMu.Unlock()
}

func (c *Connection) claimRequestID(id int) error {
	_, err := c.claimRequestIDCurrentEpoch(id)
	return err
}

func (c *Connection) claimRequestIDCurrentEpoch(id int) (uint64, error) {
	if id <= 0 || id > maxProtoInt32 {
		return 0, fmt.Errorf("%w: request ID is outside the signed 32-bit broker namespace", ErrBrokerIDNamespaceConflict)
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.claimRequestIDLocked(id, nil)
}

func (c *Connection) claimRequestIDForEpoch(id int, expectedEpoch uint64) (uint64, error) {
	if id <= 0 || id > maxProtoInt32 {
		return 0, fmt.Errorf("%w: request ID is outside the signed 32-bit broker namespace", ErrBrokerIDNamespaceConflict)
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.claimRequestIDLocked(id, &expectedEpoch)
}

func (c *Connection) claimRequestIDLocked(id int, expectedEpoch *uint64) (uint64, error) {
	epoch := c.brokerSessionEpoch.Load()
	if expectedEpoch != nil && epoch != *expectedEpoch {
		return epoch, fmt.Errorf("%w: broker socket generation changed before request ID claim", ErrBrokerIDNamespaceConflict)
	}
	if !c.haveNextValidID {
		return epoch, fmt.Errorf("%w: nextValidId has not established the current socket request namespace", ErrBrokerIDNamespaceConflict)
	}
	_, reserved := c.reservedRequestIDs[id]
	if !reserved {
		frontier := max(c.reqIDSeq, c.nextOrderID)
		if c.brokerIDExhausted || id < frontier {
			return epoch, fmt.Errorf("%w: explicit request ID is behind the consumed request/order frontier", ErrBrokerIDNamespaceConflict)
		}
	}
	delete(c.reservedRequestIDs, id)
	if id >= maxProtoInt32 {
		c.brokerIDExhausted = true
		return epoch, nil
	}
	next := id + 1
	if c.reqIDSeq < next {
		c.reqIDSeq = next
	}
	if c.nextOrderID < next {
		c.nextOrderID = next
	}
	return epoch, nil
}

func (c *Connection) orderIDOwned(id int) bool {
	if id <= 0 {
		return false
	}
	c.ordersMu.RLock()
	_, open := c.openOrders[id]
	c.ordersMu.RUnlock()
	return open || c.IsWhatIfOrderID(id)
}

func (c *Connection) claimOrderIDCurrentEpoch(id int, alreadyOwned bool) (uint64, error) {
	if id <= 0 || id > maxProtoInt32 {
		return 0, fmt.Errorf("%w: order ID is outside the signed 32-bit broker namespace", ErrBrokerIDNamespaceConflict)
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.claimOrderIDLocked(id, alreadyOwned, nil)
}

func (c *Connection) claimOrderIDForEpoch(id int, alreadyOwned bool, expectedEpoch uint64) (uint64, error) {
	if id <= 0 || id > maxProtoInt32 {
		return 0, fmt.Errorf("%w: order ID is outside the signed 32-bit broker namespace", ErrBrokerIDNamespaceConflict)
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	return c.claimOrderIDLocked(id, alreadyOwned, &expectedEpoch)
}

func (c *Connection) claimOrderIDLocked(id int, alreadyOwned bool, expectedEpoch *uint64) (uint64, error) {
	epoch := c.brokerSessionEpoch.Load()
	if expectedEpoch != nil && epoch != *expectedEpoch {
		return epoch, fmt.Errorf("%w: broker socket generation changed before order ID claim", ErrBrokerIDNamespaceConflict)
	}
	if !c.haveNextValidID {
		return epoch, fmt.Errorf("%w: nextValidId has not established the current socket order namespace", ErrBrokerIDNamespaceConflict)
	}
	if _, requestReserved := c.reservedRequestIDs[id]; requestReserved {
		return epoch, fmt.Errorf("%w: order ID is owned by a reserved read-only request", ErrBrokerIDNamespaceConflict)
	}
	_, reserved := c.reservedOrderIDs[id]
	if !reserved && !alreadyOwned {
		frontier := max(c.reqIDSeq, c.nextOrderID)
		if c.brokerIDExhausted || id < frontier {
			return epoch, fmt.Errorf("%w: explicit order ID is behind the consumed request/order frontier", ErrBrokerIDNamespaceConflict)
		}
	}
	delete(c.reservedOrderIDs, id)
	if id >= maxProtoInt32 {
		c.brokerIDExhausted = true
		return epoch, nil
	}
	next := id + 1
	if c.reqIDSeq < next {
		c.reqIDSeq = next
	}
	if c.nextOrderID < next {
		c.nextOrderID = next
	}
	return epoch, nil
}

// claimOrderIDForForwarding validates a Connector-level explicit ID and leaves
// one exact reservation for the subsequent Connection place/WhatIf encoder.
func (c *Connection) claimOrderIDForForwarding(id int, alreadyOwned bool) (uint64, error) {
	return c.claimOrderIDForForwardingAtEpoch(id, alreadyOwned, nil)
}

func (c *Connection) claimOrderIDForForwardingAtEpoch(id int, alreadyOwned bool, expectedEpoch *uint64) (uint64, error) {
	if id <= 0 || id > maxProtoInt32 {
		return 0, fmt.Errorf("%w: order ID is outside the signed 32-bit broker namespace", ErrBrokerIDNamespaceConflict)
	}
	c.reqIDMu.Lock()
	defer c.reqIDMu.Unlock()
	epoch, err := c.claimOrderIDLocked(id, alreadyOwned, expectedEpoch)
	if err != nil {
		return 0, err
	}
	c.reservedOrderIDs[id] = struct{}{}
	return epoch, nil
}

func (c *Connection) reserveRequestID(allowed func(int) bool) (int, error) {
	id, _, err := c.reserveRequestIDWithEpoch(allowed)
	return id, err
}

func (c *Connection) reserveRequestIDWithEpoch(allowed func(int) bool) (int, uint64, error) {
	const maxRequestIDReservations = 1024
	for range maxRequestIDReservations {
		candidate, epoch, err := c.reserveNextRequestID()
		if err != nil {
			return 0, epoch, err
		}
		if allowed != nil && !allowed(candidate) {
			c.reqIDMu.Lock()
			delete(c.reservedRequestIDs, candidate)
			c.reqIDMu.Unlock()
			continue
		}
		if _, err := c.claimRequestIDForEpoch(candidate, epoch); err != nil {
			return 0, epoch, err
		}
		return candidate, epoch, nil
	}
	return 0, 0, fmt.Errorf("broker request id namespace unavailable")
}

// scanMessages is a split function for the scanner to handle IBKR messages
func (c *Connection) scanMessages(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	// Need at least 4 bytes for the length
	if len(data) < 4 {
		return 0, nil, nil
	}

	// Read message length
	msgLength := int(binary.BigEndian.Uint32(data[:4]))
	totalLength := 4 + msgLength

	// Check if we have the complete message
	if len(data) < totalLength {
		return 0, nil, nil
	}

	// Return the message (without length prefix)
	return totalLength, data[4:totalLength], nil
}
