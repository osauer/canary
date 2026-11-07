package futu

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/osauer/canary/v2/pkg/futu/internal/pb/common"
	"github.com/osauer/canary/v2/pkg/futu/internal/pb/notify"
	trdcommon "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_common"
	trdgetacclist "github.com/osauer/canary/v2/pkg/futu/internal/pb/trd_getacclist"
)

const (
	defaultRedialDelay      = 5 * time.Second
	defaultHandshakeTimeout = 10 * time.Second
	defaultRequestTimeout   = 10 * time.Second
	// heartbeatIntervals is how many keep-alive intervals may pass without a
	// received frame before the session is dropped.
	heartbeatIntervals = 2
)

// ErrNotConnected is a request with no open session.
var ErrNotConnected = errors.New("opend session not open")

// ErrNoScope is a trade-class request before the account list reported the
// pinned account, or after it stopped: the connector will not guess which
// environment to stamp.
var ErrNoScope = errors.New("opend account scope unknown: the account list has not reported the pinned account")

// Config builds a [Connector]. The account is the pin: every trade-class
// request carries it, and no method takes another.
type Config struct {
	// Addr is OpenD's host:port.
	Addr string
	// Account is the pinned trading account. Zero detects it: the login's one real
	// account, which then binds like a pin. A simulated one is never picked.
	Account uint64
	// ClientID is the label sent to OpenD; OpenD assigns the connection's
	// real identity itself.
	ClientID string
	// ClientVersion is the client's own version as major*100+minor.
	ClientVersion int32
	// Now is the clock for the wire's timestamps and the pacing windows; nil
	// is the wall clock. The heartbeat always runs on real elapsed time.
	Now func() time.Time
	// RedialDelay is the wait between sessions; zero is five seconds.
	RedialDelay time.Duration
	// HandshakeTimeout bounds InitConnect; zero is ten seconds.
	HandshakeTimeout time.Duration
	// OnOrder and OnFill receive the pinned account's pushed updates, on the read
	// loop: they record and return, and never block or send.
	OnOrder func(Order)
	OnFill  func(Fill)
}

// SessionBinding names one session of one connector. The zero value is never
// current.
type SessionBinding struct {
	Epoch uint64
}

// Phase is how far the connection to OpenD has come, in the order the status
// surface reports it.
type Phase string

const (
	// PhaseStopped is a connector not started, or stopped.
	PhaseStopped Phase = "stopped"
	// PhaseDisconnected is a session that ended; the connector redials and
	// LastError says why.
	PhaseDisconnected Phase = "disconnected"
	// PhasePortDown is nothing listening on OpenD's port.
	PhasePortDown Phase = "port_down"
	// PhasePortRejecting is a socket closed before InitConnect answered.
	PhasePortRejecting Phase = "port_rejecting"
	// PhaseHandshake is an InitConnect not answered, or refused.
	PhaseHandshake Phase = "handshake"
	// PhaseProgramStatus is OpenD's program status other than Ready.
	PhaseProgramStatus Phase = "program_status"
	// PhaseNotLoggedIn is the quote or trade server not logged in.
	PhaseNotLoggedIn Phase = "not_logged_in"
	// PhaseUnscoped is an account list that does not report the pin.
	PhaseUnscoped Phase = "unscoped"
	// PhaseReady is a session that can serve every read.
	PhaseReady Phase = "ready"
)

// Environment is an account's trading environment, OpenD's trdEnv.
type Environment string

const (
	// EnvironmentUnknown is before the account list has reported the pin.
	EnvironmentUnknown Environment = ""
	// EnvironmentSimulate is OpenD's paper environment.
	EnvironmentSimulate Environment = "simulate"
	// EnvironmentReal is a live account.
	EnvironmentReal Environment = "real"
)

// Account is one row of OpenD's account list, in words.
type Account struct {
	ID          uint64
	Environment Environment
	Markets     []string
	Type        string
	Firm        string
}

// Health is the session as the status surface reads it.
type Health struct {
	Phase     Phase
	Connected bool
	Ready     bool
	Account   uint64
	// AccountDetected says the account came from the list, not from a pin.
	AccountDetected bool
	// AccountChoices are the real accounts when detection could not choose one.
	AccountChoices []AccountChoice
	Environment    Environment
	Markets        []string
	AccountType    string
	Firm           string
	Session        SessionInfo
	State          GlobalState
	StateKnown     bool
	QuoteRights    map[string]string
	// SubscriptionQuota and HistoryQuota are OpenD's pushed quotas; the Used
	// fields its pushed spend. Zero until OpenD says.
	SubscriptionQuota int
	HistoryQuota      int
	SubscriptionsUsed int
	HistoryUsed       int
	// SpendPushed is true once OpenD has pushed the Used figures.
	SpendPushed bool
	// LastError is why the last session ended or failed to start.
	LastError error
	// LastGatewayEvent is OpenD's last event that left the session open,
	// quoted for the surface.
	LastGatewayEvent string
}

// AccountChoice is a real account detection could not settle on. It carries the
// number the owner must pin, so it goes to the status surface and never to a log.
type AccountChoice struct {
	ID      uint64
	Type    string
	Markets []string
}

// scope is what the account list reported for the pin.
type scope struct {
	known   bool
	env     trdcommon.TrdEnv
	account Account
	markets []int32
}

// Connector owns the session to one OpenD for one pinned account: it dials,
// shakes hands, keeps the heartbeat by received frames, scopes the account
// from the list, redials when the session ends and serves requests.
type Connector struct {
	cfg   Config
	now   func() time.Time
	pacer *Pacer

	refresh chan struct{}
	// pinned is the account every trade request carries: the configured one, or
	// the one detection bound for this connector's life.
	pinned       atomic.Uint64
	refreshing   atomic.Bool
	refreshAgain atomic.Bool

	mu      sync.RWMutex
	cancel  context.CancelFunc
	stopped chan struct{}
	conn    *connection
	epoch   uint64
	health  Health
	scope   scope
	// accountPushed is true once this session asked for the order and fill pushes.
	accountPushed bool

	// quotesMu guards the streaming lines, their cache and the quota readings.
	// It nests inside no other lock.
	quotesMu      sync.Mutex
	lines         map[string]time.Time
	quotes        map[string]Quote
	load          Load
	lastRefusalAt time.Time
	lastRefusal   string
}

// NewConnector builds a connector; [Connector.Start] runs it.
func NewConnector(cfg Config) *Connector {
	if cfg.RedialDelay <= 0 {
		cfg.RedialDelay = defaultRedialDelay
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "canary"
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	c := &Connector{
		cfg:     cfg,
		now:     now,
		pacer:   NewPacer(now),
		refresh: make(chan struct{}, 1),
		health:  Health{Phase: PhaseStopped, Account: cfg.Account},
		lines:   make(map[string]time.Time),
		quotes:  make(map[string]Quote),
	}
	c.pinned.Store(cfg.Account)
	return c
}

// Start runs the session loop until [Connector.Stop] or ctx ends. A second
// Start does nothing.
func (c *Connector) Start(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped != nil {
		return
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.stopped = make(chan struct{})
	go c.run(ctx, c.stopped)
}

// Stop ends the loop and the session and waits for both.
func (c *Connector) Stop() {
	c.mu.Lock()
	cancel, stopped := c.cancel, c.stopped
	c.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-stopped
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Phase = PhaseStopped
	c.cancel, c.stopped = nil, nil
}

func (c *Connector) run(ctx context.Context, stopped chan struct{}) {
	defer close(stopped)
	for {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		c.health.LastError = err
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.RedialDelay):
		}
	}
}

// setPhase records how far the session attempt came.
func (c *Connector) setPhase(phase Phase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.health.Phase = phase
}

// runSession is one session from dial to its end; it returns why it ended.
func (c *Connector) runSession(ctx context.Context) error {
	var dialer net.Dialer
	nc, err := dialer.DialContext(ctx, "tcp", c.cfg.Addr)
	if err != nil {
		c.setPhase(PhasePortDown)
		return err
	}
	conn := newConnection(nc, c.pacer, nil)
	conn.onPush = func(frame Frame) { c.handlePush(conn, frame) }
	go conn.serve()
	defer conn.close(&SessionEndError{Reason: EndSocket})

	c.setPhase(PhaseHandshake)
	handshakeCtx, cancel := context.WithTimeout(ctx, c.cfg.HandshakeTimeout)
	info, err := handshake(handshakeCtx, conn, c.cfg)
	cancel()
	if err != nil {
		if conn.failure() != nil && !errors.Is(err, ErrHandshakeRefused) {
			c.setPhase(PhasePortRejecting)
		}
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.epoch++
	c.health.Connected = true
	c.health.Session = info
	c.health.LastError = nil
	c.scope = scope{}
	c.accountPushed = false
	c.mu.Unlock()
	c.quotesMu.Lock()
	clear(c.lines)
	clear(c.quotes)
	c.quotesMu.Unlock()
	defer c.endSession(conn)

	c.startRefresh(ctx, conn)
	return c.maintain(ctx, conn, info.KeepAliveInterval)
}

// endSession takes the session down; nothing it bound is current after this.
func (c *Connector) endSession(conn *connection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
	}
	c.scope = scope{}
	c.health.Connected = false
	c.health.Ready = false
	c.health.Environment = EnvironmentUnknown
	c.health.Markets = nil
	c.health.Phase = PhaseDisconnected
}

// maintain keeps the session: a keep-alive every interval, the session
// dropped when no frame arrived for two intervals, and the state and scope
// read again while the session is not ready or when OpenD pushed a change.
func (c *Connector) maintain(ctx context.Context, conn *connection, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	check := time.NewTicker(max(interval/8, 10*time.Millisecond))
	defer check.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-conn.done:
			return classifyEnd(conn.failure())
		case <-check.C:
			if conn.sinceReceived() >= heartbeatIntervals*interval {
				err := &SessionEndError{Reason: EndHeartbeat}
				conn.close(err)
				return err
			}
		case <-tick.C:
			if err := conn.send(ProtoKeepAlive, keepAliveRequest(c.now())); err != nil {
				return classifyEnd(err)
			}
			if !c.IsReady() {
				c.startRefresh(ctx, conn)
			}
		case <-c.refresh:
			c.startRefresh(ctx, conn)
		}
	}
}

// startRefresh runs one refresh beside the heartbeat, so a silent OpenD cannot
// hold the loop past its two intervals. A request during a refresh runs once more
// after it: what OpenD pushed may postdate what the running one already read.
func (c *Connector) startRefresh(ctx context.Context, conn *connection) {
	c.refreshAgain.Store(true)
	if !c.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		for {
			for c.refreshAgain.Swap(false) {
				c.refreshSession(ctx, conn)
			}
			c.refreshing.Store(false)
			if !c.refreshAgain.Load() || !c.refreshing.CompareAndSwap(false, true) {
				return
			}
		}
	}()
}

// refreshSession reads the state and the account list. A failure leaves the
// session as it was; the next tick tries again. Only a list that contradicts
// the scope the session took ends it.
func (c *Connector) refreshSession(ctx context.Context, conn *connection) {
	ctx, cancel := context.WithTimeout(ctx, defaultRequestTimeout)
	defer cancel()
	state, err := fetchGlobalState(ctx, conn)
	if err == nil {
		c.mu.Lock()
		if c.conn == conn {
			c.health.State = state
			c.health.StateKnown = true
			c.recomputePhaseLocked()
		}
		c.mu.Unlock()
		if state.ProgramStatus == programStatusName(common.ProgramStatusType_ProgramStatusType_ForceUpdate) {
			conn.close(&SessionEndError{Reason: EndForceUpdate, Detail: state.ProgramDetail})
			return
		}
	}
	if err := c.readAccounts(ctx, conn); err != nil {
		if end, ok := errors.AsType[*SessionEndError](err); ok {
			conn.close(end)
		}
		return
	}
	c.mu.Lock()
	subscribe := c.conn == conn && c.scope.known && !c.accountPushed
	readRights := c.conn == conn && c.health.QuoteRights == nil
	c.mu.Unlock()
	if readRights {
		// A failure leaves the rights unread; the next refresh tries again.
		if rights, err := fetchQuoteRights(ctx, conn); err == nil {
			c.mu.Lock()
			if c.conn == conn && c.health.QuoteRights == nil {
				c.health.QuoteRights = rights
			}
			c.mu.Unlock()
		}
	}
	if subscribe && subscribeAccountPush(ctx, conn, c.pinned.Load()) == nil {
		c.mu.Lock()
		if c.conn == conn {
			c.accountPushed = true
		}
		c.mu.Unlock()
	}
}

// recomputePhaseLocked derives the phase from what the session knows.
func (c *Connector) recomputePhaseLocked() {
	state := c.health.State
	switch {
	case c.conn == nil:
		c.health.Ready = false
		return
	case !c.health.StateKnown || !state.ProgramReady:
		c.health.Phase = PhaseProgramStatus
	case !state.QuoteLoggedIn || !state.TradeLoggedIn:
		c.health.Phase = PhaseNotLoggedIn
	case !c.scope.known:
		c.health.Phase = PhaseUnscoped
	default:
		c.health.Phase = PhaseReady
	}
	c.health.Ready = c.health.Phase == PhaseReady
}

// RefreshSession re-reads OpenD's state and the account list inside the
// pacing table; the daemon's status poll calls it. A list that no longer
// reports the pinned account with the same environment drops the session, and
// the next connect scopes afresh.
func (c *Connector) RefreshSession(ctx context.Context) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return ErrNotConnected
	}
	c.refreshSession(ctx, conn)
	return conn.failure()
}

// readAccounts reads the list, takes the pin's environment on first sight
// and refuses to be re-scoped afterwards.
func (c *Connector) readAccounts(ctx context.Context, conn *connection) error {
	var resp trdgetacclist.Response
	req := accountListRequest()
	if err := conn.request(ctx, ProtoTrdGetAccList, req, &resp); err != nil {
		return err
	}
	rows := resp.GetS2C().GetAccList()
	pin := c.pinned.Load()
	var found *trdcommon.TrdAcc
	var choices []AccountChoice
	if pin != 0 {
		for _, row := range rows {
			if row.GetAccID() == pin {
				found = row
				break
			}
		}
	} else {
		found, choices = c.detectAccount(rows)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != conn {
		return nil
	}
	if c.scope.known {
		if found == nil || trdcommon.TrdEnv(found.GetTrdEnv()) != c.scope.env {
			return &SessionEndError{Reason: EndScopeLost, Detail: fmt.Sprintf("account %d", pin)}
		}
	}
	c.health.AccountChoices = choices
	if found == nil {
		c.recomputePhaseLocked()
		return nil
	}
	if pin == 0 {
		// Bound for this connector's life: a later session reads it as a pin, so
		// nothing moves to another account if this one leaves the list.
		c.pinned.Store(found.GetAccID())
		c.health.Account = found.GetAccID()
		c.health.AccountDetected = true
	}
	account := accountFromRow(found)
	c.scope = scope{known: true, env: trdcommon.TrdEnv(found.GetTrdEnv()), account: account, markets: slices.Clone(found.GetTrdMarketAuthList())}
	c.health.Environment = account.Environment
	c.health.Markets = account.Markets
	c.health.AccountType = account.Type
	c.health.Firm = account.Firm
	c.recomputePhaseLocked()
	return nil
}

// accountListRequest asks for universal accounts too: OpenD leaves a moomoo
// universal account, a real account, out of the list otherwise.
func accountListRequest() *trdgetacclist.Request {
	return &trdgetacclist.Request{C2S: &trdgetacclist.C2S{UserID: new(uint64(0)), NeedGeneralSecAccount: new(true)}}
}

// detectAccount settles on the one real account the list offers, or says which
// ones it could not choose between.
func (c *Connector) detectAccount(rows []*trdcommon.TrdAcc) (*trdcommon.TrdAcc, []AccountChoice) {
	var real []*trdcommon.TrdAcc
	for _, row := range rows {
		if trdcommon.TrdEnv(row.GetTrdEnv()) == trdcommon.TrdEnv_TrdEnv_Real {
			real = append(real, row)
		}
	}
	if len(real) == 1 {
		return real[0], nil
	}
	choices := make([]AccountChoice, 0, len(real))
	for _, row := range real {
		account := accountFromRow(row)
		choices = append(choices, AccountChoice{ID: row.GetAccID(), Type: account.Type, Markets: account.Markets})
	}
	return nil, choices
}

func accountFromRow(row *trdcommon.TrdAcc) Account {
	env := EnvironmentSimulate
	if trdcommon.TrdEnv(row.GetTrdEnv()) == trdcommon.TrdEnv_TrdEnv_Real {
		env = EnvironmentReal
	}
	markets := make([]string, 0, len(row.GetTrdMarketAuthList()))
	for _, market := range row.GetTrdMarketAuthList() {
		markets = append(markets, strings.TrimPrefix(trdcommon.TrdMarket(market).String(), "TrdMarket_"))
	}
	return Account{
		ID:          row.GetAccID(),
		Environment: env,
		Markets:     markets,
		Type:        strings.TrimPrefix(trdcommon.TrdAccType(row.GetAccType()).String(), "TrdAccType_"),
		Firm:        strings.TrimPrefix(trdcommon.SecurityFirm(row.GetSecurityFirm()).String(), "SecurityFirm_"),
	}
}

// Accounts reads OpenD's account list as it stands, whichever accounts it
// reports. It carries no trade header, so it works before the pin is scoped.
func (c *Connector) Accounts(ctx context.Context) ([]Account, error) {
	conn, err := c.session()
	if err != nil {
		return nil, err
	}
	var resp trdgetacclist.Response
	req := accountListRequest()
	if err := conn.request(ctx, ProtoTrdGetAccList, req, &resp); err != nil {
		return nil, err
	}
	rows := resp.GetS2C().GetAccList()
	accounts := make([]Account, 0, len(rows))
	for _, row := range rows {
		accounts = append(accounts, accountFromRow(row))
	}
	return accounts, nil
}

// session is the open connection, or [ErrNotConnected].
func (c *Connector) session() (*connection, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil {
		return nil, ErrNotConnected
	}
	return c.conn, nil
}

// tradeHeaders builds the header for each market the pin is authorised for,
// always from the pin and the environment read for it. Before the account
// list reported the pin there are none and the request fails typed.
func (c *Connector) tradeHeaders() (*connection, []*trdcommon.TrdHeader, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil {
		return nil, nil, ErrNotConnected
	}
	if !c.scope.known {
		return nil, nil, ErrNoScope
	}
	headers := make([]*trdcommon.TrdHeader, 0, len(c.scope.markets))
	for _, market := range c.scope.markets {
		headers = append(headers, &trdcommon.TrdHeader{
			TrdEnv:    new(int32(c.scope.env)),
			AccID:     new(c.pinned.Load()),
			TrdMarket: new(market),
		})
	}
	return c.conn, headers, nil
}

// CaptureSession returns the binding of the current ready session.
func (c *Connector) CaptureSession() (SessionBinding, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.conn == nil || !c.health.Ready {
		return SessionBinding{}, false
	}
	return SessionBinding{Epoch: c.epoch}, true
}

// Current reports whether the binding still names the ready session.
func (c *Connector) Current(b SessionBinding) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return b.Epoch != 0 && c.conn != nil && c.health.Ready && b.Epoch == c.epoch
}

// IsReady reports whether the session can serve every read.
func (c *Connector) IsReady() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn != nil && c.health.Ready
}

// Scope is the pinned account and the environment the list reported for it;
// the environment is unknown until the list has.
func (c *Connector) Scope() (uint64, Environment) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.scope.known {
		return c.pinned.Load(), EnvironmentUnknown
	}
	return c.pinned.Load(), c.scope.account.Environment
}

// Health describes the session for the status surface.
func (c *Connector) Health() Health {
	c.mu.RLock()
	defer c.mu.RUnlock()
	health := c.health
	health.Markets = slices.Clone(c.health.Markets)
	health.AccountChoices = slices.Clone(c.health.AccountChoices)
	if c.health.QuoteRights != nil {
		health.QuoteRights = maps.Clone(c.health.QuoteRights)
	}
	return health
}

// signalRefresh asks the session loop to read the state again; a pending
// signal already covers this one.
func (c *Connector) signalRefresh() {
	select {
	case c.refresh <- struct{}{}:
	default:
	}
}

// handlePush takes one frame that answered no request. It runs on the read
// loop, so it records and signals and never sends.
func (c *Connector) handlePush(conn *connection, frame Frame) {
	switch frame.ProtoID {
	case ProtoQotUpdateBasicQot:
		c.handleBasicQuotePush(conn, frame)
	case ProtoTrdUpdateOrder, ProtoTrdUpdateOrderFill:
		c.handleTradePush(conn, frame)
	case ProtoNotify:
		var resp notify.Response
		if err := proto.Unmarshal(frame.Body, &resp); err != nil {
			conn.close(fmt.Errorf("%w: Notify body: %v", ErrFrame, err))
			return
		}
		c.handleNotify(conn, resp.GetS2C())
	}
}

// handleNotify applies one Notify: a gateway event may end the session, and
// the rest update what the status surface reads.
func (c *Connector) handleNotify(conn *connection, s2c *notify.S2C) {
	if s2c == nil {
		return
	}
	switch notify.NotifyType(s2c.GetType()) {
	case notify.NotifyType_NotifyType_GtwEvent:
		if end := gatewayEventEnd(s2c.GetEvent()); end != nil {
			conn.close(end)
			return
		}
		c.mu.Lock()
		c.health.LastGatewayEvent = s2c.GetEvent().GetDesc()
		c.mu.Unlock()
		c.signalRefresh()
	case notify.NotifyType_NotifyType_ProgramStatus, notify.NotifyType_NotifyType_ConnStatus:
		c.signalRefresh()
	case notify.NotifyType_NotifyType_QotRight:
		c.mu.Lock()
		// A push may name fewer markets than the read; it updates those it names.
		rights := maps.Clone(c.health.QuoteRights)
		if rights == nil {
			rights = map[string]string{}
		}
		maps.Copy(rights, quoteRights(s2c.GetQotRight()))
		c.health.QuoteRights = rights
		c.mu.Unlock()
	case notify.NotifyType_NotifyType_APIQuota:
		c.mu.Lock()
		c.health.SubscriptionQuota = int(s2c.GetApiQuota().GetSubQuota())
		c.health.HistoryQuota = int(s2c.GetApiQuota().GetHistoryKLQuota())
		c.mu.Unlock()
	case notify.NotifyType_NotifyType_UsedQuota:
		c.mu.Lock()
		c.health.SubscriptionsUsed = int(s2c.GetUsedQuota().GetUsedSubQuota())
		c.health.HistoryUsed = int(s2c.GetUsedQuota().GetUsedKLineQuota())
		c.health.SpendPushed = true
		c.mu.Unlock()
	}
}
