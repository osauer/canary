package daemon

import (
	"fmt"
	"strings"
	"sync"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// brokerWriteTransactionBinding is process-local authority for one daemon
// broker-write transaction. It binds the request-time trading gate to the
// exact daemon Connector publication, broker socket generation, and concrete
// account/mode scope that passed authorization. It is never durable order
// authority and must be revalidated immediately before broker I/O.
type brokerWriteTransactionBinding struct {
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	session        ibkrlib.ConnectorSessionBinding
	scope          brokerStateScope
	endpoint       string
	clientID       int
	origin         string
	//lint:ignore U1000 Used by the trading-tagged order submission path.
	orderID int
	//lint:ignore U1000 Used by the trading-tagged order submission path.
	orderEventSeq              int64
	tradingControlGeneration   uint64
	riskBound                  bool
	riskDraft                  rpc.OrderDraft
	riskPosition               rpc.OrderPositionImpact
	riskPortfolioGeneration    uint64
	riskPortfolioAccount       string
	riskBaseCurrency           string
	riskBaseCurrencyProvenance ibkrlib.AccountBaseCurrencyProvenance
	riskNotional               orderNotionalAuthority
	testOnly                   bool
}

func (s *Server) currentTradingStatus() rpc.TradingStatus {
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	return s.tradingStatus(ep)
}

func (s *Server) currentBrokerWriteAuthorization() brokerWriteAuthorization {
	if s == nil {
		auth := brokerWriteAuthorization{}
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, rpc.TradingBlocker{
			Code:    "trading_disabled",
			Message: "trading daemon is unavailable",
			Action:  "Start the canary daemon before broker writes.",
		})
		return auth
	}
	return s.brokerWriteAuthorization(s.currentTradingStatus())
}

// brokerWriteAuthorizationForRequest is the request-time write gate. It starts
// from the origin-agnostic authorization that also feeds trading-status
// CanWrite; origin remains available for policy hooks and audit journaling.
//
//lint:ignore U1000 Used by trading-tagged proposal and order submission.
func (s *Server) brokerWriteAuthorizationForRequest(origin string) brokerWriteAuthorization {
	auth := s.currentBrokerWriteAuthorization()
	if s == nil {
		return auth
	}
	for _, blocker := range s.brokerWriteOriginBlockers(auth.Status, origin) {
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, blocker)
		auth.Allowed = false
	}
	return auth
}

func brokerWriteTransactionDriftError() error {
	return fmt.Errorf("%w: broker connection authority changed before transmit; refresh and retry", ErrTradingDisabled)
}

func (s *Server) brokerWriteOriginBlockers(status rpc.TradingStatus, origin string) []rpc.TradingBlocker {
	if s != nil && s.orderWriteOriginBlockersForTest != nil {
		return s.orderWriteOriginBlockersForTest(status, origin)
	}
	return liveOriginBlockers(status, origin)
}

// authorizeBrokerWriteTransaction evaluates the trading gate against one
// daemon connector snapshot and captures its exact socket session before the
// gate runs. A later connector/session/scope change invalidates the binding;
// callers must never fall through to a freshly looked-up connector.
//
//lint:ignore U1000 Used by trading-tag broker writes.
func (s *Server) authorizeBrokerWriteTransaction(origin string, cancel bool) (brokerWriteAuthorization, brokerWriteTransactionBinding, error) {
	if s == nil {
		auth := s.currentBrokerWriteAuthorization()
		return auth, brokerWriteTransactionBinding{}, nil
	}

	s.mu.Lock()
	ep, connector, connectorEpoch := s.endpoint, s.connector, s.connectorEpoch
	s.mu.Unlock()

	var session ibkrlib.ConnectorSessionBinding
	sessionCaptured := false
	if connector != nil {
		session, sessionCaptured = connector.CaptureSession()
	}

	controlGenerationBefore := uint64(0)
	var status rpc.TradingStatus
	var auth brokerWriteAuthorization
	if cancel {
		status = s.tradingStatusForCancel(ep)
		auth = s.brokerCancelAuthorization(status)
	} else {
		_, controlGenerationBefore = s.effectiveTradingControlSnapshot()
		status = s.tradingStatus(ep)
		auth = s.brokerWriteAuthorization(status)
	}
	for _, blocker := range s.brokerWriteOriginBlockers(status, origin) {
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, blocker)
		auth.Allowed = false
	}
	controlGenerationAfter := uint64(0)
	if !cancel {
		_, controlGenerationAfter = s.effectiveTradingControlSnapshot()
	}
	if !cancel && controlGenerationBefore != controlGenerationAfter {
		auth.Allowed = false
		auth.Blockers = appendTradingBlockerOnce(auth.Blockers, rpc.TradingBlocker{
			Code: "trading_controls_changed", Message: "trading controls changed during broker-write admission",
			Action: "Refresh the operation from current trading controls before retrying.",
		})
	}
	if !auth.Allowed {
		return auth, brokerWriteTransactionBinding{}, nil
	}

	configuredAccount := ""
	port := ep.Port
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
	}
	connectedAccount := ""
	if connector != nil {
		connectedAccount = connector.AccountID()
	}
	binding := brokerWriteTransactionBinding{
		connector:                connector,
		connectorEpoch:           connectorEpoch,
		session:                  session,
		scope:                    brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount),
		endpoint:                 status.Endpoint,
		clientID:                 status.ClientID,
		origin:                   origin,
		tradingControlGeneration: controlGenerationAfter,
	}

	// Tests use explicit broker-call hooks instead of a live socket. The
	// optional binding seam still exercises exact daemon pointer/epoch/scope
	// refusal at the real final boundary.
	if s.orderWriteBindingForTest != nil {
		binding.connector, binding.connectorEpoch, binding.session, binding.scope = s.orderWriteBindingForTest(status)
		binding.testOnly = true
	} else if s.orderReserveBrokerID != nil || s.orderPlaceBroker != nil || s.orderCancelBroker != nil || s.optionExerciseBroker != nil {
		binding.testOnly = true
	} else if connector == nil || !sessionCaptured {
		return auth, brokerWriteTransactionBinding{}, brokerWriteTransactionDriftError()
	}

	if !brokerScopeConcrete(binding.scope) ||
		!strings.EqualFold(strings.TrimSpace(status.Account), strings.TrimSpace(binding.scope.Account)) ||
		!strings.EqualFold(strings.TrimSpace(status.Mode), strings.TrimSpace(binding.scope.Mode)) {
		return auth, brokerWriteTransactionBinding{}, brokerWriteTransactionDriftError()
	}
	if !s.brokerWriteTransactionCurrent(binding) {
		return auth, brokerWriteTransactionBinding{}, brokerWriteTransactionDriftError()
	}
	return auth, binding, nil
}

func (s *Server) brokerWriteTransactionCurrent(binding brokerWriteTransactionBinding) bool {
	if s == nil || !brokerScopeConcrete(binding.scope) {
		return false
	}
	s.mu.Lock()
	connector, connectorEpoch, ep := s.connector, s.connectorEpoch, s.endpoint
	s.mu.Unlock()
	if connector != binding.connector || connectorEpoch != binding.connectorEpoch {
		return false
	}

	configuredAccount := ""
	host := ep.Host
	port := ep.Port
	clientID := ep.ClientID
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
		if host == "" {
			host = s.cfg.Gateway.HostOrDefault()
		}
		if port == 0 && s.cfg.Gateway.Port != nil {
			port = *s.cfg.Gateway.Port
		}
		if clientID == 0 {
			clientID = s.cfg.Gateway.ClientIDOrDefault()
		}
	}
	connectedAccount := ""
	if connector != nil {
		connectedAccount = connector.AccountID()
	}
	currentScope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount)
	if !sameBrokerScope(binding.scope, currentScope) || binding.endpoint != endpointString(host, port) || binding.clientID != clientID {
		return false
	}

	// Close the AccountID/endpoint-read gap before accepting the snapshot.
	s.mu.Lock()
	currentEndpoint := s.endpoint
	stillPublished := s.connector == binding.connector && s.connectorEpoch == binding.connectorEpoch &&
		currentEndpoint.Host == ep.Host && currentEndpoint.Port == ep.Port &&
		currentEndpoint.ClientID == ep.ClientID && currentEndpoint.Account == ep.Account
	s.mu.Unlock()
	if !stillPublished {
		return false
	}
	if binding.testOnly {
		return true
	}
	return connector != nil && connector.SessionCurrent(binding.session)
}

func (s *Server) requireBrokerWriteTransactionCurrent(binding brokerWriteTransactionBinding) error {
	if !s.brokerWriteTransactionCurrent(binding) {
		return brokerWriteTransactionDriftError()
	}
	return nil
}

// brokerWireGuard is the last daemon authority read at the physical transport
// boundary. Connector invokes it under the exact Connection transport lock
// after pacing, handshake, and socket-generation checks and immediately before
// the first frame byte. The captured status supplies immutable route/account
// pins; mutable storage health and freeze state are sampled afresh here.
//
//lint:ignore U1000 Used by the trading-tagged physical write path.
func (s *Server) brokerWireGuard(binding brokerWriteTransactionBinding, status rpc.TradingStatus, cancel bool) (func() error, func()) {
	var leaseMu sync.Mutex
	var releaseLease func()
	release := func() {
		leaseMu.Lock()
		if releaseLease != nil {
			releaseLease()
			releaseLease = nil
		}
		leaseMu.Unlock()
	}
	guard := func() error {
		if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
			return err
		}
		var auth brokerWriteAuthorization
		if cancel {
			auth = s.brokerCancelAuthorization(status)
		} else {
			auth = s.brokerWriteAuthorization(status)
		}
		for _, blocker := range s.brokerWriteOriginBlockers(status, binding.origin) {
			auth.Blockers = appendTradingBlockerOnce(auth.Blockers, blocker)
			auth.Allowed = false
		}
		if !auth.Allowed {
			return fmt.Errorf("%w: %s", ErrTradingDisabled, firstTradingBlockerMessage(auth.Blockers))
		}
		if cancel {
			return nil
		}
		cfg, currentControlGeneration, frozen, unlock := s.lockEffectiveTradingControlSnapshot()
		leaseMu.Lock()
		if releaseLease != nil {
			leaseMu.Unlock()
			unlock()
			return fmt.Errorf("%w: broker wire guard was invoked more than once", ErrTradingDisabled)
		}
		releaseLease = unlock
		leaseMu.Unlock()
		if frozen {
			return fmt.Errorf("%w: trading writes are frozen by runtime platform settings", ErrTradingDisabled)
		}
		if currentControlGeneration != binding.tradingControlGeneration {
			return fmt.Errorf("%w: trading controls changed after admission; refresh and retry", ErrTradingDisabled)
		}
		if binding.riskBound {
			current, err := s.captureWireOrderPositionAuthority(binding, status, binding.riskDraft)
			if err != nil {
				return err
			}
			if current.Generation != binding.riskPortfolioGeneration ||
				!strings.EqualFold(strings.TrimSpace(current.Health.Account), strings.TrimSpace(binding.riskPortfolioAccount)) ||
				!sameOrderPositionImpact(current.Impact, binding.riskPosition) ||
				!strings.EqualFold(strings.TrimSpace(current.BaseCurrency), strings.TrimSpace(binding.riskBaseCurrency)) ||
				current.BaseCurrencyProvenance != binding.riskBaseCurrencyProvenance {
				return fmt.Errorf("%w: portfolio risk authority changed after admission; preview again", ErrTradingDisabled)
			}
			if err := validateOrderRiskAuthority(cfg, binding.riskDraft, current.Impact, binding.riskNotional, current.BaseCurrency); err != nil {
				return fmt.Errorf("%w: current trading controls reject the order: %v", ErrTradingDisabled, err)
			}
		}
		if binding.orderID > 0 && binding.orderEventSeq > 0 {
			latest, err := s.orderJournal.LatestOrderEventSeq(orderJournalEvent{
				Endpoint: binding.endpoint, ClientID: binding.clientID,
				Account: binding.scope.Account, Mode: binding.scope.Mode,
				ReservedOrderID: binding.orderID,
			})
			if err != nil {
				return fmt.Errorf("%w: read current order authority: %v", ErrTradingDisabled, err)
			}
			if latest != binding.orderEventSeq {
				return fmt.Errorf("%w: order changed after modify staging; refresh and retry", ErrTradingDisabled)
			}
		}
		return nil
	}
	return guard, release
}

// withBoundBrokerWriteTransaction performs the daemon publication/scope check
// immediately before an exact-session Connector operation. The Connector API
// carries binding.session through allocator claim and the epoch-checked frame
// write. No publication lock is held while pacing or transport can block; the
// protected transport takes its short publication lease at the final write.
//
//lint:ignore U1000 Used by trading-tag broker writes.
func (s *Server) withBoundBrokerWriteTransaction(binding brokerWriteTransactionBinding, operation func() error) error {
	if operation == nil {
		return brokerWriteTransactionDriftError()
	}
	if binding.testOnly {
		if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
			return err
		}
		return operation()
	}
	if binding.connector == nil {
		return brokerWriteTransactionDriftError()
	}
	ran, err := binding.connector.WithBoundBrokerSession(binding.session, func() error {
		if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
			return err
		}
		return operation()
	})
	if !ran {
		return brokerWriteTransactionDriftError()
	}
	return err
}
