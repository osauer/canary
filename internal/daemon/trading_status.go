package daemon

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleTradingStatus() *rpc.TradingStatus {
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	status := s.tradingStatus(ep)
	return &status
}

func (s *Server) tradingStatus(ep discover.Endpoint) rpc.TradingStatus {
	return s.tradingStatusWithWriteProjection(ep, true)
}

// tradingStatusForCancel omits the runtime control projection. Cancellation
// must remain available while a human freeze/limit commit is in progress; the
// cancel authorization path applies every non-control blocker separately.
func (s *Server) tradingStatusForCancel(ep discover.Endpoint) rpc.TradingStatus {
	return s.tradingStatusWithWriteProjection(ep, false)
}

func (s *Server) tradingStatusWithWriteProjection(ep discover.Endpoint, includeWriteProjection bool) rpc.TradingStatus {
	if s != nil {
		// These hooks are exercised only by the trading-tag write tests, but
		// Server is compiled in the default read-only build too.
		_ = s.orderReserveBrokerID
		_ = s.orderPlaceBroker
		_ = s.orderCancelBroker
		_ = s.orderWriteBindingForTest
		_ = s.orderWriteBeforeBrokerSend
		_ = &s.paperSmokeMu
		_ = s.paperSmokeCancelBudgetOverride
	}
	var cfg config.Resolved
	if s != nil && s.cfg != nil {
		cfg = *s.cfg
	}
	tr := cfg.Trading.WithDefaults()

	host := ep.Host
	if host == "" {
		host = cfg.Gateway.HostOrDefault()
	}
	port := ep.Port
	if port == 0 && cfg.Gateway.Port != nil {
		port = *cfg.Gateway.Port
	}
	account := cfg.Gateway.Account
	if account == "" {
		account = ep.Account
	}
	clientID := ep.ClientID
	if clientID == 0 {
		clientID = cfg.Gateway.ClientIDOrDefault()
	}

	status := rpc.TradingStatus{
		Mode:           tr.Mode,
		GatewayHost:    host,
		GatewayPort:    port,
		Endpoint:       endpointString(host, port),
		PortOrigin:     string(ep.PortOrigin),
		Account:        account,
		AccountOrigin:  originPinnedOrAuto(cfg.Gateway.Account != ""),
		ClientID:       clientID,
		ClientIDOrigin: originPinnedOrDefault(cfg.Gateway.ClientID != nil),
		MCPTrading:     rpc.TradingMCPDisabled,
		LiveOverride:   rpc.TradingLiveOverrideBlocked,
	}
	if status.PortOrigin == "" {
		if cfg.Gateway.Port != nil {
			status.PortOrigin = string(discover.OriginPinned)
		} else {
			status.PortOrigin = string(discover.OriginDiscovered)
		}
	}
	if tr.Mode == config.TradingModeDisabled {
		return status
	}

	add := func(code, message, action string) {
		status.Blockers = append(status.Blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  action,
		})
	}
	if blocker, blocked := s.authorityTradingBlocker(); blocked {
		status.Blockers = appendTradingBlockerOnce(status.Blockers, blocker)
	}
	summary, journalErr := s.orderJournalSummary()
	if journalErr != nil {
		add("order_journal_unavailable", "order writes require a writable local order journal", "Fix the XDG state directory permissions before enabling trading.")
	} else {
		status.OpenOrders = summary.OpenOrders
		status.LastOrderEvent = summary.LastEvent
	}
	if !s.tradingGatewayReady() {
		add("gateway_unavailable", "order preview and broker writes require a connected IBKR Gateway/TWS session", "Run `canary status` and wait for the gateway handshake to complete.")
	}

	switch tr.Mode {
	case config.TradingModePaper, config.TradingModeLive:
	default:
		add("invalid_mode", fmt.Sprintf("trading mode %q is invalid", tr.Mode), "Set [trading].mode to disabled, paper, or live.")
	}
	if cfg.Gateway.Port == nil {
		add("gateway_port_unpinned", "order submission requires a pinned gateway port", "Set [gateway].port.")
	}
	if cfg.Gateway.Account == "" {
		add("gateway_account_unpinned", "order submission requires a pinned account", "Set [gateway].account.")
	} else if strings.EqualFold(strings.TrimSpace(cfg.Gateway.Account), "All") {
		add("gateway_account_not_concrete", "order preview requires a concrete IBKR account, not the aggregate account \"All\"", "Pin the paper/live account code shown by TWS, such as a DU paper account.")
	} else if connectedAccount := s.connectedGatewayAccount(); connectedAccount != "" && accountMismatchesConnected(cfg.Gateway.Account, connectedAccount) {
		add("gateway_account_mismatch", fmt.Sprintf("configured account %q does not match connected account %q", cfg.Gateway.Account, connectedAccount), "Pin [gateway].account to the account advertised by the connected TWS/Gateway session.")
	}
	if cfg.Gateway.ClientID == nil {
		add("gateway_client_id_unpinned", "order submission requires a pinned client ID", "Set [gateway].client_id.")
	} else if ep.ClientID != 0 && ep.ClientID != *cfg.Gateway.ClientID {
		add("gateway_client_id_mismatch", "connected client ID differs from the pinned client ID", "Stop the conflicting API client or choose a free [gateway].client_id.")
	}

	switch tr.Mode {
	case config.TradingModePaper:
		if cfg.Gateway.Port != nil && cfg.Gateway.Account != "" && !looksPaper(port, account) {
			add("paper_endpoint_unconfirmed", "paper trading requires a paper-looking endpoint or account", "Use port 4002/7497 or a DU paper account.")
		}
	case config.TradingModeLive:
		if cfg.Gateway.Port != nil && cfg.Gateway.Account != "" && looksPaper(port, account) {
			add("live_endpoint_unconfirmed", "live trading requires a live-looking endpoint and account", "Use port 4001/7496 and pin the live account in [gateway].account.")
		}
		check := s.checkPaperSmoke(account, status.Endpoint, clientID, tr.PaperSmokeMaxAgeDuration())
		status.PaperSmoke = check.Status
		status.PaperSmokeMaxAge = tr.PaperSmokeMaxAgeDuration().String()
		if check.Evidence != nil {
			at := check.Evidence.At
			status.PaperSmokeAt = &at
			status.PaperSmokeAccount = check.Evidence.Account
			status.PaperSmokeEndpoint = check.Evidence.Endpoint
			status.PaperSmokeClientID = check.Evidence.ClientID
			status.PaperSmokeVersion = check.Evidence.Version
		}
		// Paper-smoke evidence is informational only (surfaced in the
		// status fields above), not a live blocker. Since 2026-06-10 the
		// smoke is enforced in the release pipeline — `make release` runs
		// it at version bump and aborts on failure — so runtime live
		// enablement rests on the TWS API toggle, the trading-capable
		// binary, and the config pins checked above.
	}

	status.Blocked = len(status.Blockers) > 0
	status.CanPreview = tr.OrderEntryEnabled() && !status.Blocked
	if includeWriteProjection {
		writeAuth := s.brokerWriteAuthorization(status)
		status.CanWrite = writeAuth.Allowed
		if !writeAuth.Allowed && !status.Blocked {
			status.WriteBlockers = writeAuth.Blockers
		}
	}
	if tr.Mode == config.TradingModeLive && !status.Blocked {
		status.LiveOverride = rpc.TradingLiveOverrideReady
	}
	return status
}

func (s *Server) orderBrokerWritesEnabled() bool {
	if s != nil && s.orderWritesEnabled != nil {
		return s.orderWritesEnabled()
	}
	return orderWritesAvailable
}

func (s *Server) tradingGatewayReady() bool {
	if s != nil && s.gatewayReadyForTrading != nil {
		return s.gatewayReadyForTrading()
	}
	return s != nil && s.gatewayConnector() != nil
}

type brokerWriteAuthorization struct {
	Status   rpc.TradingStatus
	Route    string
	Allowed  bool
	Blockers []rpc.TradingBlocker
}

// tradingFrozenBlockerCode marks the runtime trading-freeze blocker so the
// cancel path can recognise and strip it (see forCancel).
const tradingFrozenBlockerCode = "trading_frozen"

// forCancel strips the runtime trading-freeze blocker from a write
// authorization: a freeze stops new and modified orders but must never
// strand an open order that needs cancelling.
func (auth brokerWriteAuthorization) forCancel() brokerWriteAuthorization {
	kept := make([]rpc.TradingBlocker, 0, len(auth.Blockers))
	for _, blocker := range auth.Blockers {
		if blocker.Code != tradingFrozenBlockerCode {
			kept = append(kept, blocker)
		}
	}
	auth.Blockers = kept
	auth.Allowed = len(kept) == 0
	return auth
}

func (s *Server) brokerWriteAuthorization(status rpc.TradingStatus) brokerWriteAuthorization {
	return s.brokerWriteAuthorizationWithControls(status, true)
}

func (s *Server) brokerCancelAuthorization(status rpc.TradingStatus) brokerWriteAuthorization {
	return s.brokerWriteAuthorizationWithControls(status, false)
}

func (s *Server) brokerWriteAuthorizationWithControls(status rpc.TradingStatus, includeControls bool) brokerWriteAuthorization {
	auth := brokerWriteAuthorization{Status: status, Route: status.Mode}
	var blockers []rpc.TradingBlocker
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{
			Code:    code,
			Message: message,
			Action:  action,
		})
	}
	if status.Mode == config.TradingModeDisabled {
		add("trading_disabled", "trading is disabled", `Set [trading].mode to "paper" or "live" before broker writes.`)
	}
	for _, blocker := range status.Blockers {
		blockers = appendTradingBlockerOnce(blockers, blocker)
	}
	// Status is a request-time snapshot. Authoritative SQLite health can latch
	// fail-closed after a durable pre-transmit stage, so every later/final gate
	// must sample it again instead of trusting the copied blocker list.
	if blocker, blocked := s.authorityTradingBlocker(); blocked {
		blockers = appendTradingBlockerOnce(blockers, blocker)
	}
	if status.Blocked && len(status.Blockers) == 0 {
		add("trading_blocked", "trading status is blocked", "Refresh trading status and resolve the active blocker before broker writes.")
	}
	switch status.Mode {
	case config.TradingModeDisabled, config.TradingModePaper, config.TradingModeLive:
	default:
		add("invalid_mode", fmt.Sprintf("trading mode %q is invalid", status.Mode), "Set [trading].mode to disabled, paper, or live.")
	}
	if !s.orderBrokerWritesEnabled() {
		add("order_writes_unavailable", "order writes are unavailable in this build", "Rebuild the daemon with the trading write capability.")
	}
	if s == nil || s.orderJournal == nil {
		add("order_journal_unavailable", "order writes require a writable local order journal", "Fix the daemon state directory before enabling trading.")
	}
	if includeControls && s.tradingFrozen() {
		add(tradingFrozenBlockerCode, "trading writes are frozen by runtime platform settings", "Run `canary settings set trading.freeze=false` to resume broker writes; new orders, modifies, and reduce sweeps are all blocked while frozen — only cancels remain allowed.")
	}
	auth.Blockers = blockers
	auth.Allowed = len(blockers) == 0
	return auth
}

// normalizedWriteOrigin maps any request origin onto the journaled audit
// value: the known human origins pass through, everything else is "agent".
// Origin remains audit/policy metadata; live broker writes are authorized by
// the same trading gates, preview tokens, freeze state, and broker checks for
// every origin.
func normalizedWriteOrigin(origin string) string {
	if originIsHuman(origin) {
		return origin
	}
	return rpc.OrderOriginAgent
}

// originIsHuman reports whether a request origin represents a human in the
// loop. Unknown or empty origins classify as agent so new adapters must opt
// in to a human origin.
func originIsHuman(origin string) bool {
	switch origin {
	case rpc.OrderOriginHumanTTY, rpc.OrderOriginPairedDevice:
		return true
	}
	return false
}

// liveOriginBlockers is retained as the shared hook point for origin-specific
// policy. Live and paper broker writes currently use the same daemon gates for
// all origins; normalizedWriteOrigin still journals non-human callers as agent.
func liveOriginBlockers(_ rpc.TradingStatus, _ string) []rpc.TradingBlocker {
	return nil
}

func appendTradingBlockerOnce(blockers []rpc.TradingBlocker, next rpc.TradingBlocker) []rpc.TradingBlocker {
	for _, blocker := range blockers {
		if blocker.Code == next.Code {
			return blockers
		}
	}
	return append(blockers, next)
}

func (s *Server) connectedGatewayAccount() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	c := s.connector
	s.mu.Unlock()
	if c == nil || !c.IsReady() {
		return ""
	}
	return strings.TrimSpace(c.AccountID())
}

func accountMismatchesConnected(configured, connected string) bool {
	configured = strings.TrimSpace(configured)
	connected = strings.TrimSpace(connected)
	if configured == "" || connected == "" {
		return false
	}
	if strings.EqualFold(connected, "All") {
		return false
	}
	if !brokerScopeAccountConcrete(configured) {
		return true
	}
	if !strings.Contains(connected, ",") {
		return !strings.EqualFold(configured, connected)
	}

	matched := false
	for account := range strings.SplitSeq(connected, ",") {
		account = strings.TrimSpace(account)
		// managedAccounts is broker authority only while every member is one
		// concrete account. A matching member cannot rehabilitate a malformed
		// aggregate because that would turn ambiguous session state into write
		// readiness.
		if !brokerScopeAccountConcrete(account) {
			return true
		}
		if strings.EqualFold(configured, account) {
			matched = true
		}
	}
	return !matched
}

func endpointString(host string, port int) string {
	if host == "" {
		return ""
	}
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func originPinnedOrAuto(pinned bool) string {
	if pinned {
		return string(discover.OriginPinned)
	}
	return "auto"
}

func originPinnedOrDefault(pinned bool) string {
	if pinned {
		return string(discover.OriginPinned)
	}
	return string(discover.OriginDefault)
}

func looksPaper(port int, account string) bool {
	if port == 4002 || port == 7497 {
		return true
	}
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(account)), "DU")
}

func accountModeForStatus(port int, account string) string {
	if looksPaper(port, account) {
		return rpc.AccountModePaper
	}
	account = strings.TrimSpace(account)
	if account != "" && !strings.EqualFold(account, "All") {
		return rpc.AccountModeLive
	}
	switch port {
	case 4001, 7496:
		return rpc.AccountModeLive
	default:
		return rpc.AccountModeUnknown
	}
}

func (s *Server) checkPaperSmoke(account, endpoint string, clientID int, maxAge time.Duration) tradingPaperSmokeCheck {
	if s == nil {
		return tradingPaperSmokeCheck{
			Status:  tradingPaperSmokeStatusMissing,
			Message: "live trading requires recent paper-smoke evidence in daemon-owned state",
			Action:  "Run `canary trading paper-smoke` against the pinned paper account first.",
		}
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	return s.tradingReadiness.CheckPaperSmoke(account, endpoint, clientID, s.version, maxAge, now)
}

func (s *Server) orderJournalSummary() (orderJournalSummary, error) {
	if s == nil || s.orderJournal == nil {
		return orderJournalSummary{}, fmt.Errorf("order journal is not configured")
	}
	events, err := s.orderJournal.LoadEvents(0)
	if err != nil {
		return orderJournalSummary{}, err
	}
	var last orderJournalEvent
	for _, ev := range events {
		last = ev
	}
	scope := s.currentBrokerStateScope()
	var summary orderJournalSummary
	views := buildOrderViews(events)
	// Same inference pass as the orders view: a calendar-expired DAY order
	// must not be counted open here while the list shows it closed.
	inferDayOrderExpiry(views, buildOrderEventsByKey(events), s.orderNow())
	for _, view := range views {
		if view.Open && orderViewMatchesBrokerScope(view, scope) {
			summary.OpenOrders++
		}
	}
	if !last.At.IsZero() {
		summary.LastEvent = fmt.Sprintf("%s %s at %s", last.Type, orderJournalEventLabel(last), last.At.Format(time.RFC3339))
	}
	return summary, nil
}
