package daemon

import ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

// daemonBrokerEvidenceBinding joins daemon connector publication authority to
// the Connector's exact socket/order/portfolio frontiers. It is process-local
// evidence for one final commit, never a durable identity or broker-write
// authorization.
type daemonBrokerEvidenceBinding struct {
	scope          brokerStateScope
	connector      *ibkrlib.Connector
	connectorEpoch uint64
	broker         ibkrlib.BrokerEvidenceBinding
}

// withStableBrokerEvidence linearizes a derived-state commit against socket
// replacement, order callbacks, and structural portfolio changes. Connector
// publication participates in the same barrier, so s.mu is needed only for a
// short identity check and is never held across SQLite/composer work. False
// with nil error means the captured evidence drifted and commit was not called.
func (s *Server) withStableBrokerEvidence(binding daemonBrokerEvidenceBinding, commit func() error) (bool, error) {
	if s != nil && s.stableBrokerEvidenceForTest != nil {
		return s.stableBrokerEvidenceForTest(binding, commit)
	}
	if s == nil || binding.connector == nil || commit == nil || !brokerScopeConcrete(binding.scope) {
		return false, nil
	}
	var commitErr error
	committed := binding.connector.WithStableBrokerEvidence(binding.broker, func() bool {
		s.mu.Lock()
		if s.connector != binding.connector || s.connectorEpoch != binding.connectorEpoch {
			s.mu.Unlock()
			return false
		}
		ep := s.endpoint
		configuredAccount := ""
		port := ep.Port
		if s.cfg != nil {
			configuredAccount = s.cfg.Gateway.Account
			if port == 0 && s.cfg.Gateway.Port != nil {
				port = *s.cfg.Gateway.Port
			}
		}
		currentScope := brokerStateScopeFromSnapshot(configuredAccount, ep.Account, port, binding.connector.AccountID())
		if !sameBrokerScope(binding.scope, currentScope) {
			s.mu.Unlock()
			return false
		}
		s.mu.Unlock()
		commitErr = commit()
		return commitErr == nil
	})
	return committed, commitErr
}

// withStableBrokerAndOrderEvidence extends the broker barrier through the
// exact local order-event head used to derive Protection or Order Integrity.
// Lock order is binding Connector -> order journal -> SQLite composer. False
// with nil error means either frontier drifted and commit was not called.
func (s *Server) withStableBrokerAndOrderEvidence(binding daemonBrokerEvidenceBinding, orderJournal *orderJournalStore, expectedOrderHead int64, commit func() error) (bool, error) {
	if s == nil || orderJournal == nil || orderJournal != s.orderJournal || expectedOrderHead < 0 || commit == nil {
		return false, nil
	}
	journalCommitted := false
	brokerCommitted, err := s.withStableBrokerEvidence(binding, func() error {
		var journalErr error
		journalCommitted, journalErr = orderJournal.WithStableAuthorityHead(expectedOrderHead, commit)
		return journalErr
	})
	return brokerCommitted && journalCommitted, err
}

// withConnectorEvidencePublication changes the daemon's connector identity
// while both the outgoing and incoming Connector barriers participate. The
// caller supplies the exact expected current pointer; false means another
// lifecycle transition won the race and no mutation ran. mutateLocked runs
// under s.mu and must not call Connector methods.
func (s *Server) withConnectorEvidencePublication(expected, next *ibkrlib.Connector, mutateLocked func()) bool {
	if s == nil || mutateLocked == nil {
		return false
	}
	applied := false
	wrapped := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.connector == expected {
			mutateLocked()
			applied = true
		}
	}
	if expected != nil {
		expected.WithBrokerEvidenceMutation(func() {
			if next != nil && next != expected {
				next.WithBrokerEvidenceMutation(wrapped)
				return
			}
			wrapped()
		})
	} else if next != nil {
		next.WithBrokerEvidenceMutation(wrapped)
	} else {
		wrapped()
	}
	return applied
}
