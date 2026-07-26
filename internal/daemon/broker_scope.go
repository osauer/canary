package daemon

import (
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

type brokerStateScope struct {
	Account string
	Mode    string
}

func (s *Server) currentBrokerStateScope() brokerStateScope {
	if s == nil {
		return brokerStateScope{}
	}
	s.mu.Lock()
	ep := s.endpoint
	c := s.connector
	s.mu.Unlock()

	configuredAccount := ""
	if s.cfg != nil {
		configuredAccount = s.cfg.Gateway.Account
	}
	connectedAccount := ""
	if c != nil {
		connectedAccount = c.AccountID()
	}
	port := ep.Port
	if port == 0 && s.cfg != nil && s.cfg.Gateway.Port != nil {
		port = *s.cfg.Gateway.Port
	}
	return brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, connectedAccount)
}

func brokerStateScopeFromSnapshot(configuredAccount, endpointAccount string, port int, connectedAccount string) brokerStateScope {
	account := strings.TrimSpace(configuredAccount)
	if account == "" {
		account = strings.TrimSpace(endpointAccount)
	}
	if !brokerScopeAccountConcrete(account) {
		if connected := strings.TrimSpace(connectedAccount); brokerScopeAccountConcrete(connected) {
			account = connected
		}
	}
	return brokerStateScope{
		Account: account,
		Mode:    accountModeForStatus(port, account),
	}
}

func brokerScopeAccountConcrete(account string) bool {
	account = strings.TrimSpace(account)
	if account == "" || strings.EqualFold(account, "All") {
		return false
	}
	// A managedAccounts frame can carry several accounts (comma-separated)
	// for multi-account logins. That is a session aggregate, not a single
	// account identity — anything that does not trim to one token is
	// non-concrete and scoped state fails closed.
	return !strings.ContainsAny(account, ", \t")
}

// brokerScopeConcrete reports whether the scope names one concrete account
// with a known paper/live mode — the only identity scoped trading state may
// bind to.
func brokerScopeConcrete(scope brokerStateScope) bool {
	if !brokerScopeAccountConcrete(scope.Account) {
		return false
	}
	switch scope.Mode {
	case rpc.AccountModePaper, rpc.AccountModeLive:
		return true
	default:
		return false
	}
}

func sameBrokerScope(a, b brokerStateScope) bool {
	return strings.EqualFold(strings.TrimSpace(a.Account), strings.TrimSpace(b.Account)) &&
		strings.EqualFold(strings.TrimSpace(a.Mode), strings.TrimSpace(b.Mode))
}

func brokerScopedModeMatches(rowMode, scopeMode string) bool {
	scopeMode = strings.TrimSpace(scopeMode)
	if scopeMode == "" || scopeMode == rpc.AccountModeUnknown {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(rowMode), scopeMode)
}

func brokerScopeIsUnfiltered(scope brokerStateScope) bool {
	return strings.TrimSpace(scope.Account) == "" && strings.TrimSpace(scope.Mode) == ""
}

func brokerScopedAccountMatches(rowAccount string, scope brokerStateScope) bool {
	if brokerScopeIsUnfiltered(scope) {
		return true
	}
	if !brokerScopeAccountConcrete(scope.Account) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(rowAccount), scope.Account)
}

func orderViewMatchesBrokerScope(view rpc.OrderView, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(view.Account, scope) &&
		brokerScopedModeMatches(view.Mode, scope.Mode)
}

func purgeLedgerRowMatchesBrokerScope(row purgeLedgerRow, scope brokerStateScope) bool {
	return brokerScopedAccountMatches(row.Account, scope) &&
		brokerScopedModeMatches(row.Mode, scope.Mode)
}
